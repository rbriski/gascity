# Amara Osei - DeepSeek V4 Flash

**Verdict:** block

Lane: factual session events, idempotent subscribers, crash recovery, work-release guarantee. This reviews the current `DESIGN.md` (the attempt-19 revision, located at `internal/session/DESIGN.md`), alongside `REQUIREMENTS.md` and the existing codebase. Findings are validated against the live checkout of the branch; inline citations and code-level references are provided below.

---

### Top strengths:
- **Closed-World Event Inventory and Freshness Gating:** Establishing `SESSION_EVENT_INVENTORY.yaml` as a named Slice 0 artifact (`DESIGN.md:216`) with an automated freshness validator (`DESIGN.md:800-802`) over `events.KnownEventTypes` is an excellent guard. Forcing the build to fail on any unmapped session or request/result event prevents undocumented event drifts and enforces schema discipline.
- **Events as Pure Post-Commit Facts:** Explicitly defining session events as post-commit facts rather than commands (`DESIGN.md:775-776`) prevents the anti-pattern of using event notifications as imperative triggers for downstream state mutations. Enforcing that transitions must converge from durable facts even when event emission is skipped (`DESIGN.md:795-797`) is a robust distributed-systems practice.
- **Deficient Current-State Exception Layering:** Rather than ignoring the severe legacy event deficits, the design mandates explicit current-state exception rows in Slice 0 for known gaps (`DESIGN.md:813-819`). This includes `NoPayload` lifecycle events, work IDs trapped in human-formatted `Message` text, pre-commit `session.woke` emissions, and unstable display name `Subject` fields.
- **Strict Structural Consumption Gate:** The design establishes a critical guardrail: *"No NoPayload session event may back a critical or reconciliation subscriber until it carries a typed payload with the canonical session ID, the relevant bead/work IDs, and a generation or instance token"* (`DESIGN.md:819-822`). This prevents future behavior-moving slices from consuming fragile legacy events.
- **Cost-Bounded Subscriber Constraints:** Enforcing fan-out caps, backpressure/drop policies (`DESIGN.md:827-830`), and cost rules (`DESIGN.md:894-924`) ensures that reconciler-driven event delivery and durable scans do not degrade to expensive, unindexed database-wide sweeps.

---

### Critical risks:

#### 1. [Blocker] Absence of Event Schema Refactoring in the Backlog
- **Evidence:** `DESIGN.md` Backlog lines 994-1021.
- **Why it matters:** The design relies on a structural gate forbidding reactive consumption of `NoPayload` events before they are migrated to carry typed payloads (`DESIGN.md:819-825`). However, the `Backlog` (lines 994-1021) does not reserve any explicit milestones or tasks to actually perform the code-level refactoring of these deficient event producers. Slices 3 (Explicit Wake) and 4 (Close) expect to bind event validators and utilize these notifications. Without an explicit backlog task to migrate the legacy publishers in the codebase, developers will proceed to implement the behavior-moving slices without upgrading the event schemas, leading to either bypasses or building behavioral logic on top of unstable envelope-only scraping.

#### 2. [Blocker] Pre-Commit `session.woke` Emission Lacks Migration Schedule
- **Evidence:** `cmd/gc/session_lifecycle_parallel.go:1594-1618` and `DESIGN.md:817-819`.
- **Why it matters:** In the current codebase (`cmd/gc/session_lifecycle_parallel.go`), `session.woke` is emitted via `rec.Record` at lines 1594-1598, which is *before* the durable wake metadata commit via `CommitStartedPatch` at lines 1610-1618. If a subscriber receives this event and queries the database, it will read stale state, presenting a severe risk of split-brain or recovery failure. Although this is listed as a current-state exception in Slice 0, the design does not explicitly require or schedule changing the producer site of `session.woke` to be strictly post-commit when Slice 3 (Explicit Wake) lands.

#### 3. [Blocker] The Work-Release Idempotency Key is Under-pinned and Ambiguous
- **Evidence:** `DESIGN.md` lines 821-822 and lines 840-843.
- **Why it matters:** The design states that "the work-release idempotency key is bead ID plus generation" (`DESIGN.md:821-822`) but does not clarify which database fields represent "bead ID" (is it the work bead ID, the session bead ID, or the pair?) and what "generation" refers to. The codebase has three distinct durably-stored concepts: integer `generation` metadata (`internal/session/lifecycle_transition.go:116`, `internal/session/manager.go:402`), `continuation_epoch`, and the crypto-random `instance_token` minted by `NewInstanceToken` (`internal/session/lifecycle.go:19-37`). This ambiguity could cause divergent implementations of the recovery scan or the eventual work-release slice, leading to silent recovery failures and stale work assignments.

#### 4. [Major] The Hard Adequacy Gate Allows Deficient Typed Payloads to Escape
- **Evidence:** `DESIGN.md` lines 819-822 and `internal/api/event_payloads.go:245-249`.
- **Why it matters:** The structural gate is scoped strictly to `NoPayload` events ("No `NoPayload` session event may back a critical or reconciliation subscriber until..."). However, several existing events registered with `SessionLifecyclePayload` (`session.stopped`, `session.crashed`, and `session.work_query_failed`) carry only `session_id`, `template`, and `reason` (`internal/api/event_payloads.go:245-249`)—they lack work/bead IDs and generation/instance tokens. These events pass the letter of the gate while failing its intent, allowing critical recovery subscribers to bind to events that cannot reliably identify lifecycle generation or prevent stale/duplicate triggers.

#### 5. [Major] Vague Durable Scan Recovery Triggering and Scheduling
- **Evidence:** `DESIGN.md` lines 830-833 and lines 923-924.
- **Why it matters:** The design depends heavily on periodic "durable scans" as the absolute convergence backstop when events are dropped or processes crash. However, the design does not specify *who* runs the scans (e.g. the reconciler vs the controller) or provide concrete guidelines on the scan frequency, trigger conditions, and partial-success state reconciliation, especially for non-pool (adhoc or named) sessions. Without these details, the recovery path is too vague to verify or safely implement.

#### 6. [Minor] Consumer-Side Closed-World and Public Invitation Gaps
- **Evidence:** `DESIGN.md` lines 799-811 and `internal/events/events.go:44-59`.
- **Why it matters:** The inventory is strictly producer-focused. It does not require a census of active event consumers (e.g., SSE projections, order gates, doctor/trace, pack-level subscribers). Concretely, `session.stranded` and `session.drain_acked_with_assigned_work` were shipped explicitly inviting pack-level subscribers to own the recovery policy (`internal/events/events.go:50-51`). However, `session.stranded` is emitted with `NoPayload` and its work IDs are trapped in human-formatted `Message` text, while using an in-memory throttle marker (`strandedEventEmittedKey`, `cmd/gc/session_reconciler.go:2454-2456`). A critical recovery subscriber cannot consume these events reliably. The design records the payload gap as an exception row but states no interim disposition for the already-public recovery invitation.

---

### Missing evidence:
- **No Backlog Item for Code-Level Publisher Migrations:** The backlog has no explicit task to refactor legacy `Record` sites (e.g., in `cmd/gc/session_lifecycle_parallel.go` or `cmd/gc/session_reconciler.go`) to use typed payloads.
- **No Detail on Monotonic Generation Source:** While the design references a "generation or instance token" (`DESIGN.md:783, 821`), it does not specify the exact database field or metadata source of this token or confirm if it is already reliably populated on all session beads.
- **Producer-Side Emission-Failure Contract is Unstated:** Recording serializes behind a cross-process flock with a 250ms bounded wait, and the `Recorder` interface is documented as best-effort. The design does not state what a failed or timed-out `Record` produces on session lifecycle paths (e.g., silent drop, logged drop, or an automatic fallback to durable-scan-only).

---

### Required changes:
1. **Add Backlog Work for Code-Level Publisher Migrations:** Explicitly add an item to the Backlog (such as in Slice 0 or Slice 3) to migrate the deficient `NoPayload` session events (`session.woke`, `session.draining`, `session.stranded`, etc.) to carry typed payloads with canonical identifiers and generation/instance tokens *before* any behavior-moving slices consume them.
2. **Explicitly Mandate Post-Commit `session.woke` Emission:** Under Slice 3 (Explicit Wake) or the Event and Recovery Contract, explicitly require that the publisher of `session.woke` must be moved to be strictly post-commit.
3. **Pin the Work-Release Idempotency Key:** Explicitly define the work-release idempotency key (specifying the exact combination of work bead ID, session bead ID, and the chosen durable field: `Metadata["generation"]`, `instance_token`, or `continuation_epoch`), citing the minting/storage sites and asserting stability across create/resume/re-create.
4. **Broaden the Adequacy Gate:** Make the adequacy gate payload-shape-neutral: no session event (whether `NoPayload` or typed) may back a critical or reconciliation subscriber unless its payload carries the canonical session ID, the relevant work/bead IDs, and the pinned generation/instance token. Add `session.stopped`, `session.crashed`, and `session.work_query_failed` to the current-state exception rows.
5. **Detail Durable Scan Ownership and Scheduling:** Specify the recovery owner (e.g., reconciler or controller) and detail the scan frequency, trigger conditions, and duplicate/stale event rejection behaviors in the Event recovery scans section. Extend this to cover non-pool sessions (adhoc/named) during the close-crash-partial-release window.
6. **Add Consumer Census to the Inventory:** Require a `consumers` field in the `SESSION_EVENT_INVENTORY.yaml` row schema enumerating active consumer classes, and document an interim disposition for recovery-purposed events with inadequate payloads (e.g., state that recovery via `session.stranded` is unsupported until upgraded).

---

### Questions:
- Is there any technical justification for not scheduling the actual code-level event migrations in the Backlog, given that Slices 3-4 expect to consume/rely on them?
- For configured named sessions and pool slots, what is the planned source for the monotonic generation/instance token, and is it already reliably stamped on session beads?
- For `session.woke`'s pre-commit emission: is the end state moving emission post-commit (per the Atomic Command Contract's "post-commit event ordering"), or redefining the event as "wake attempted"? The exception row records the gap but not the intended resolution.

---

## Answers to Persona Questions

### 1. Are SessionEvent payloads facts with stable identifiers rather than commands to work, mail, trace, or extmsg?
**Answer:** While the event types themselves are framed as post-commit facts rather than commands (`DESIGN.md:775-776`), the actual current identifiers they carry in the codebase are highly unstable (display names, user-supplied target tokens). The design successfully mitigates this by requiring `SESSION_EVENT_INVENTORY.yaml` and a structural gate preventing any reactive subscriber from consuming a `NoPayload` event (`DESIGN.md:819-825`), but the actual code migration has not been scheduled.

### 2. If the process crashes after a durable session mutation but before in-process event delivery, how does a safety-critical subscriber such as work release converge?
**Answer:** It converges via periodic **durable scans** driven by the designated recovery authority (`DESIGN.md:791, 830-833`). All critical transitions must converge from durable facts even when event emission is skipped (`DESIGN.md:795-797`). However, the design currently lacks detail on scan frequency and ownership.

### 3. Which reactions are critical versus best-effort, and is the recovery path documented for each?
**Answer:** The design requires `SESSION_EVENT_INVENTORY.yaml` in Slice 0 to explicitly classify each row's criticality (diagnostic-only or recovery-relevant) and define the durable scan owner (`DESIGN.md:807-811`). This provides the necessary framework, but the actual recovery path details for specific events are deferred.
