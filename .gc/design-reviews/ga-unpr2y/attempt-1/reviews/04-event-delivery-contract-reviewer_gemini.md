# Amara Osei - DeepSeek V4 Flash

**Verdict:** block

Lane: factual session events, idempotent subscribers, crash recovery, work-release guarantee. Scope reviewed: the Event And Recovery Contract (`DESIGN.md:608-658` in `.gc/design-reviews/ga-unpr2y/attempt-17/design-before.md`), the session-related event emissions in `cmd/gc/`, and the live event definitions in `internal/events/` and `internal/api/`. Findings are validated against the live checkout of the branch; inline code-level references are provided below.

---

### Top strengths:
- **Clean Invariant Enforcement:** The design is highly disciplined in defining events as post-commit facts rather than commands (`DESIGN.md:612-613`), explicitly restricting subscribers from using human-oriented `Message` fields (`DESIGN.md:655-657`). This cleanly isolates observability from execution.
- **Durable Scan Primacy:** The design correctly notes that critical operations like work release must not depend solely on transient in-process events (`DESIGN.md:632-635`). This aligns with the Bitter Lesson, ensuring that process crashes or dropped events do not lead to permanently stranded work.
- **Rigorous Scenario Coverage:** The required test matrix for event-driven slices (`DESIGN.md:629-631`) is comprehensive, demanding failure-injection tests for crash-after-commit, skipped events, and duplicate deliveries.

---

### Critical risks:

#### 1. [Blocker] Critical Recovery Context Trapped in Human-Oriented Message Strings
- **Evidence:** `cmd/gc/session_reconciler.go` lines 2467-2473:
  ```go
  rec.Record(events.Event{
      Type:    events.SessionStranded,
      Ts:      now,
      Actor:   "gc",
      Subject: session.ID,
      Message: formatStrandedMessage(template, session.Metadata["session_name"], ids),
  })
  ```
- **Why it matters:** The design establishes a strict invariant: *"Event Message text is operator-only; subscribers may consume only typed payload fields and envelope fields"* (`DESIGN.md:655-657`). However, in the case of `session.stranded`, the critical list of stranded work bead IDs (`ids`) resides **only** within the human-formatted `Message` string.
- **DeepSeek V4 Flash Challenge:** Other reviewers accepted that having `session.stranded` in the inventory was sufficient. But because this event is registered as `events.NoPayload{}` (`internal/api/event_payloads.go:455`), a subscriber attempting to automate stranded-work recovery cannot access the affected bead IDs in a structured manner. The subscriber is forced to either perform an expensive database-wide query (violating hot-loop budgets, `DESIGN.md:697-700`) or parse/regex-match the human `Message` string. This creates immediate runtime fragility, and directly violates the project's Core Design Principles.

#### 2. [Blocker] Envelope Subject Pollution and Absence of Stable Session Identifiers
- **Evidence:** `cmd/gc/session_lifecycle_parallel.go` line 1597 and `cmd/gc/cmd_runtime_drain.go` line 220:
  ```go
  // In session_lifecycle_parallel.go:
  rec.Record(events.Event{
      Type:    events.SessionWoke,
      Actor:   "gc",
      Subject: tp.DisplayName(), // Polluted with display name / template name
  })

  // In cmd_runtime_drain.go:
  rec.Record(events.Event{
      Type:    events.SessionDraining,
      Actor:   eventActor(),
      Subject: targetName,        // Polluted with user-supplied target token
  })
  ```
- **Why it matters:** Because `SessionWoke` and `SessionDraining` are registered as `NoPayload{}` (`internal/api/event_payloads.go:444-447`), subscribers are forced to rely on the `Subject` field of the event envelope for identity correlation. However, the actual implementation populates `Subject` with unstable, non-unique display names, templates, or raw user-supplied target tokens (such as `deacon` or `polecat`), rather than the stable, canonical session bead ID (`session.ID`).
- **DeepSeek V4 Flash Challenge:** If multiple parallel sessions of the same template or alias transition concurrently, a subscriber consuming `session.woke` or `session.draining` has **no reliable way** to map the event back to the correct physical session bead. This will cause target-resolution conflicts, false positives, or failure to reconcile state.

#### 3. [Major] Severe Split-Brain and Race Vulnerabilities in `NoPayload` Events
- **Evidence:** `internal/api/event_payloads.go` lines 444-456. Out of the 13 specified session events in `DESIGN.md:636-641`, 9 are registered as `events.NoPayload{}` (including `SessionWoke`, `SessionDraining`, `SessionUndrained`, `SessionQuarantined`, `SessionIdleKilled`, `SessionMaxAgeKilled`, `SessionSuspended`, `SessionUpdated`, and `SessionStranded`).
- **Why it matters:** In a dynamic, asynchronous orchestrator, session identities (pool slots or configured named sessions) are frequently reused across generations (`SESSION-ID-007`). Because these 9 events carry no typed payload, they lack a monotonic `generation/instance token` (or session-bead revision/created_at timestamp).
- **DeepSeek V4 Flash Challenge:** A subscriber receiving delayed, out-of-order, or duplicated event deliveries over the network (SSE) cannot distinguish whether a `session.stopped` or `session.draining` event belongs to a previous dead generation or the currently active, newly-spawned generation. This invites devastating split-brain bugs where a delayed historical event triggers the premature teardown or reassignment of an active, healthy session.

#### 4. [Major] No Backlog Allocation for Migrating Deficient Event Payloads
- **Evidence:** `DESIGN.md` Backlog (lines 786-813).
- **Why it matters:** The backlog schedules behavior-moving extractions (Slice 3 Wake, Slice 4 Close, etc.) that will emit and consume these events. However, no backlog milestone is allocated to migrating the legacy `NoPayload` session events to typed payloads containing stable identifiers.
- **DeepSeek V4 Flash Challenge:** Without an explicit, dedicated milestone to unexport and upgrade `NoPayload` events, developers will inevitably construct the behavior-moving slices on top of this fragile foundation, cementing the unstable `Subject` scraping and race-prone patterns into production code.

---

### Missing evidence:
- **No Mapping of Subscriber Dependency Risk:** There is no evidence or inventory in `BOUNDARY_INVENTORY.md` or `DESIGN.md` showing which external systems, dashboards, or internal subscribers currently read `session.woke`, `session.draining`, `session.undrained`, or `session.suspended`.
- **No Multi-Region/Multi-Node Event Out-of-Order Proof:** The design assumes that "durable scans" solve all out-of-order event issues, but it provides no proof or test design for how a subscriber handling SSE events filters out old generations during rapid restart cycles.

---

### Required changes:
1. **Remediation of `session.stranded` Payload:** Upgrade `session.stranded` from `NoPayload` to a typed payload (e.g., `SessionStrandedEventPayload`) that explicitly carries a machine-readable list of affected work bead IDs (`bead_ids []string`), rather than burying them in the `Message` string.
2. **Standardization of Envelope `Subject`:** Mandate that for **all** session-related events, the envelope `Subject` field MUST exclusively carry the canonical, stable session bead ID (`session.ID`). User-supplied tokens, template names, or display names are forbidden in the envelope `Subject` and must be relegated to typed payload fields.
3. **Deprecate `NoPayload` for Critical Reactions:** Insert a strict structural gate: *no `session.*` event may be consumed by any reactive subscriber unless it has been migrated from `NoPayload` to a typed payload carrying both the canonical `session_id` and a monotonic `generation/instance token`*.
4. **Update the Backlog:** Add an explicit step in Slice 0 or Slice 4 to migrate the deficient `NoPayload` session events (`session.woke`, `session.draining`, etc.) to unified, typed lifecycle payloads before any behavior-moving slices consume them.

---

### Questions:
- Is there any current production subscriber that relies on parsing the `Message` of `session.stranded` or scraping the `Subject` of `session.woke`?
- For configured named sessions and pool slots, what is the authoritative source of the monotonic `generation/instance token` (e.g., `created_at` timestamp or a specific metadata counter), and is it reliably stamped on all session beads today?

---

## Answers to Persona Questions

### 1. Are SessionEvent payloads facts with stable identifiers rather than commands to work, mail, trace, or extmsg?
**Answer:** While the event types themselves are framed as post-commit facts rather than commands, the actual identifiers they carry are highly unstable. Because 9 out of 13 session events are registered as `NoPayload`, they rely on the envelope's `Subject` field, which is polluted with raw user-supplied target tokens (`cmd_runtime_drain.go:220`) or display names (`session_lifecycle_parallel.go:1597`) instead of stable session bead IDs.

### 2. If the process crashes after a durable session mutation but before in-process event delivery, how does a safety-critical subscriber such as work release converge?
**Answer:** The current design and implementation rely on reconciler scans in `cmd/gc/session_reconciler.go` (such as the zombie process and stranded work collectors) to achieve eventual convergence. This satisfies the durability constraint. However, if any subscriber is built to react to events for faster or real-time convergence, the complete absence of stable identifiers and generation tokens in `NoPayload` events makes idempotent replay impossible, presenting a severe risk of split-brain or stale-state clobbering upon crash recovery.

### 3. Which reactions are critical versus best-effort, and is the recovery path documented for each?
**Answer:** The recovery path for the critical work-release and zombie-reclaim loops is scan-backed and lives in the reconciler. However, because there is no closed-world inventory of all registered `session.*` events mapped to their respective criticality and recovery paths, the boundaries between best-effort operator feedback and critical infrastructure reactions remain dangerously blurred.
