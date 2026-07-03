# Step 6 design — retire the raw lockstep + raw working set

Status: DESIGN (fable 4-lens audit → opus synthesis). Branch
`upstream/object-front-doors-cleanup`, PR #3839, base HEAD `48ab9e107`.

Step 6 is the finale of the reconciler front-door migration: make the typed
`infoByID` snapshot the tick's single source of truth, drop the raw
`session.Metadata[k]=v` lockstep mirrors, remove the raw `ordered []beads.Bead` /
`beadByID` / `circuitSessionByIdentity` working set, and add the reconciler files
to the `snapshotInfoOnlyFiles` CI guard. It is a LANDMINE — a wrong Get-cutover
reintroduces the #2345 force-wake and #2574 phantom-restart production bugs.

This design was grounded by a fable 4-lens audit of the raw/lockstep surface
(`raw/step6-*` — audits A/B/C/D). Every claim below carries a verified file:line.

---

## 1. The Get-cutover exposure set (VERIFIED COMPLETE)

`refreshSessionInfo(id)` (session_reconciler.go:1353) today re-projects the snapshot
from the **raw in-memory bead** (`InfoFromPersistedBead(*beadByID[id])`). Flipping it
to the store-authoritative `sessFront.Get(id)` exposes exactly the keys that the raw
projection deliberately hid this tick. Audit A classified all 24 `ApplyPatch` sites +
7 `SetMarker` sites; the complete exposure set read off the `Info` snapshot is:

| key | why the raw bead hid it | who reads it off the snapshot | handling |
| --- | --- | --- | --- |
| `reset_committed_at` | persisted by `RestartRequestPatch` (lifecycle_transition.go:387) at **two** sites — session_reconciler.go:2144 (mirror skips it @2156) AND session_bead_cycle.go:100 (mirror skips it @114); the durable marker is for the NEXT tick (#2345 force-wake prevention) | `compute_awake_bridge.go:140` (`info.ResetCommittedAt`) + `resetPendingCommittedAtInfo`@126 (via `infoByID`@1411) | **freeze to tick-start value** (see §2) |
| `restart_requested` | set **in-memory only** at session_reconciler.go:2098 (progress-stall), never persisted | `compute_awake_bridge.go:138` (`info.RestartRequested`) | **in-memory intra-tick overlay** (see §2) |

**Everything else persisted-without-mirror is UNREAD off `Info`, so needs NO
suppression** (Audit A/D confirmed by grep):
- `started_live_hash` — `Info.StartedLiveHash` exists but has **zero** readers; the
  only reader is the raw `session.Metadata["started_live_hash"]`@2455. `live_hash` is
  not an `Info` field. (persist-without-mirror @2462-2465, 2488-2491)
- `config_drift_deferred_*` / `attached_config_drift_deferred_*` — zero `Info`
  readers (raw reads @3703-3713 are a §4 6c concern, not a Get-flip exposure).
- the `session_circuit_*` cluster cleared by `clearPersistedSessionCircuitBreakerMetadata`
  — not in the `Info` codec (Step 5 owns it via `CircuitState`).

**`reset_committed_at` is tick-start-invariant** (VERIFIED): the *only* writer is
`RestartRequestPatch`, both call sites skip the mirror, and no locksteped batch ever
writes it → the raw bead's value never changes during the tick. So a value captured
once at snapshot build equals the raw projection at every refresh point.

**`restart_requested` in-memory set is the only one** (Audit B): 2098 is the sole
in-memory-only decision write read off `Info`. `stranded_event_emitted_at`@3374 is
in-memory-first but read-off-info=NO. The store-persisted writers (drain-ack clear
@395 lockstep, cmd_handoff @378) are seen by a Get.

**One residual divergence class (deferred, unobservable for 6a):** the "unconditional
mirror on persist error" sites (e.g. `recordWakeFailure` session_reconcile.go:834-866)
mirror onto the raw bead even when `ApplyPatch` errored, so a Get would be
store-authoritative (older) on a *persist failure*. None of those keys (`wake_attempts`,
etc.) are read off the `Info` snapshot, so 6a stays byte-identical for snapshot
consumers in both the healthy and persist-error cases; the recording-fake write oracle
(healthy persists) proves the happy path. Store-authoritative-on-error is the design
intent (the write didn't happen).

---

## 2. The intra-tick model + why the Get-cutover is DEFERRED (evidence-based correction)

The intra-tick divergences a store-authoritative refresh must reproduce are exactly
two (§1): `reset_committed_at` (freeze to its tick-start value) and `restart_requested`
(in-memory overlay). That model is correct. **But the naive "flip `refreshSessionInfo`
to `sessFront.Get`" implementation was tried and REVERTED** — it is the wrong shape for
the cutover:

- **Per-tick Get storm.** `refreshSessionInfo(session.ID)` at session_reconciler.go:1876
  is **unconditional per forward-pass iteration** (it feeds `infoPostZombie`), and the
  blanket pre-pass @2742 refreshes every session. Flipping to `Get` therefore adds ≥1
  store read per session *every tick* — a real Dolt I/O regression on the hot path.
- **Consumes test-injected Get-errors.** ~13 reconciler test files inject
  `store.Get(sessionID)` errors to exercise fail-safe paths (e.g.
  `sessionObservationGetErrorStore{remaining:1}` for "attachment-check error → don't
  recycle"). The unconditional 1876 refresh-`Get` consumes that injected error *before*
  the intended check sees it, defeating the scenario — coupling every such test to the
  internal refresh count.
- **Zero benefit during coexistence.** While the lockstep is still mirroring, the raw
  bead is authoritative-equivalent, so refreshing the snapshot *from the raw bead* is
  correct AND free AND perturbs no store I/O. The store-authoritative refresh is only
  *needed* once the lockstep is removed.

**Conclusion:** the cutover belongs bundled with the lockstep drop (§3 6d), and should
be done via **write-returns-`Info`** (spec §4.3 escalation) — the ~24 `ApplyPatch`
sites already know the patch they wrote, so `refreshSessionInfo` can reconstruct the
post-write `Info` from the write result (or a single targeted read) instead of a blanket
`Get` per session. That adds no per-tick Get storm and consumes no injected errors. The
two overlays (`reset_committed_at` freeze, `restart_requested` intra-tick) still apply,
built once at snapshot construction and at 2098 respectively.

---

## 3. Ordered sub-phase backlog (re-sequenced: safe conversions first, cutover last)

- [ ] **6a — codec fidelity gaps** (the new first step; additive, byte-identical, NO
  store-I/O change). Add the `Info` mirrors the remaining raw decision reads need,
  each with a `TestSessionClassifierInfoEquivalence` case. Audit D blockers:
  `session_id_flag` (freshRestartSessionKey @2139), `template_overrides`
  (ParseTemplateOverrides @3918), a `state`/`state_reason` sibling for
  `isDrainAckStopPending` @54 (`Info.MetadataState`/`StateReason` already exist — just
  the sibling), and the `wake_attempts` fidelity gap (raw `!="" && !="0"` @857 vs the
  int-parsed `Info.WakeAttempts`, which needs a raw-string mirror to stay identical).
- [ ] **6b — convert the residual raw decision reads to `Info`** (Audit D `note`
  cluster: `lifecycleTimerBlocker`, `evaluateWakeReasons`, `healExpiredTimers`,
  `sessionExitFacts`, `recordWakeFailure`, `healStatePatchWithRollback`, the
  pendingCreate*/config-drift/hash reads). Flip callers to the existing `*Info`
  siblings, delete dead raw siblings. Each a small, oracle-backed commit. Trace
  payloads that need the raw verbatim string (`pending_create_claim` bool gap) keep
  a named raw accessor (spec §4.1).
- [ ] **6c — retire the raw working set** (Audit C, hardest). Convert the three
  aliased consumers: the Phase-1 forward-pass loop (`for i := range ordered`,
  `&ordered[i]`), the wakeTargets loop (@2809), and startCandidates →
  executePlannedStarts. Route `advanceSessionDrains` + `clearMissingIdleProbes` +
  `computeNamedSessionProgressSignatures` + `openPoolSessionCountForTemplate` +
  `circuitSessionByIdentity` off `infoByID`/ID-lists. LANDMINE — multi-commit.
- [ ] **6d — the cutover: `refreshSessionInfo` off the raw bead + drop the lockstep +
  remove `ordered []beads.Bead`/`beadByID`.** Do it via **write-returns-`Info`** (§2),
  NOT a blanket `Get` per session (which adds a per-tick Get storm and consumes the
  test-injected Get-errors, proven by the reverted 6a attempt). Build the two intra-tick
  overlays (`reset_committed_at` freeze at snapshot build; `restart_requested` overlay
  at 2098). Drop each `session.Metadata[k]=v` lockstep + its dependent same-tick reads
  as ONE commit with a read-after-write test; the blanket pre-pass @2742 collapses once
  restart_requested is the overlay and rollbacks refresh from their write. LANDMINE.
- [ ] **6e — join the guard.** Extend `snapshotInfoOnlyFiles`
  (frontdoor_di_guard_test.go:83) to ALSO forbid raw `.Metadata[` on session beads
  (Audit C blocker: today it only forbids the 4 raw snapshot accessors), and add
  the reconciler files once raw-free.

**Realistic scope per session:** 6a is one small, safe, verifiable commit set (additive
mirrors + oracle). 6b is several small oracle-backed commits. 6c and 6d are the large
landmines and likely span multiple sessions each. Checkpoint after each.

**Reverted-attempt note:** the naive Get-cutover (`refreshSessionInfo → sessFront.Get`
+ the two overlays) was implemented and reverted this session — it built and passed the
reset-pending/awake/classifier suites but failed
`TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions/attachment_check_error_fails_safe`
because the unconditional 1876 refresh-`Get` consumed the injected attachment-check
error. That is the evidence behind moving the cutover to 6d via write-returns-`Info`.

---

## 4. Guard note (6f)

`snapshotInfoOnlyFiles` (frontdoor_di_guard_test.go:83-91) currently lists
`{template_resolve.go, session_name_lookup.go, cmd_citystatus.go,
session_reconciler_trace_cycle.go, providers.go, nudge_dispatcher.go,
named_sessions.go}` and its guard only forbids the four raw snapshot accessors
(`.Open()/.FindByID(/.FindSessionBeadByTemplate(/.FindSessionBeadByNamedIdentity(`).
To make the reconciler files meaningfully raw-free, 6f must extend the guard to also
forbid raw session-bead `.Metadata[` reads/writes (minus the documented raw-by-design
exceptions: the witness full-resync @361-363 and work-bead reads @3391/4213).
