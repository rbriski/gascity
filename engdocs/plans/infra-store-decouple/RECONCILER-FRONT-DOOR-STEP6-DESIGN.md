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

**Persist-error caveat (opus review, HIGH-adjacent):** the byte-identical guarantee is
**happy-path only**. Several sites run the raw-mirror loop **unconditionally even when
`ApplyPatch` errors** — notably `healStateWithRollback` (session_reconcile.go:1063-1068:
error only logged @1064, mirror @1066-1068 runs regardless), writing `state` /
`pending_create_claim`, both read off `Info` via `infoPostHeal` (~1596). On a persist
FAILURE a store `Get` would diverge from the raw projection. The **write-returns-`Info`**
cutover (§2) sidesteps this entirely (it never `Get`s — it reflects the patch that was
returned), so this is a doc-scoping caveat, not a cutover blocker. The recording-fake
write oracle (healthy persists) proves the happy path.

**`Status`/`Closed` is a THIRD divergence class, NOT a metadata key (opus review, HIGH).**
`Info.Closed` derives from `b.Status == "closed"` (info_store.go:28), never from an
`ApplyPatch` batch — so no returned patch and no metadata overlay can carry it. In-memory
status closes (`session.Status = "closed"` after
`closeSessionBeadIfReachableStoreUnassigned`, ~1553-1554) and, worse,
`rollbackPendingCreate` (session_lifecycle_parallel.go, closes in the **store only**,
never sets the raw bead's Status — so today's raw-bead pre-pass does not even reflect it)
mean any refresh whose job is `Info.Closed` needs a **store re-read**, not a returned
patch. `!Info.Closed` is read cross-session by `openPoolSessionCountForTemplate`
(session_progress.go:21) mid-forward-pass (~2058). The cutover model (§2, §3 6d) must
treat status-close refreshes as a read case.

---

## 2. The intra-tick model + why the Get-cutover is DEFERRED (evidence-based correction)

The intra-tick divergences a store-authoritative refresh must reproduce are exactly
two (§1): `reset_committed_at` (freeze to its tick-start value) and `restart_requested`
(in-memory overlay). That model is correct. **But the naive "flip `refreshSessionInfo`
to `sessFront.Get`" implementation was tried and REVERTED** — it is the wrong shape for
the cutover:

- **Zero benefit during coexistence (DECISIVE).** While the lockstep is still mirroring,
  the raw bead is authoritative-equivalent, so refreshing the snapshot *from the raw bead*
  is correct AND free AND perturbs no store I/O. The store-authoritative refresh is only
  *needed* once the lockstep is removed — so there is nothing to gain by flipping now.
- **Consumes test-injected Get-errors (REAL fail-safe hazard).** The refresh at
  session_reconciler.go:1854 (feeds `infoPostZombie`) is **unconditional per forward-pass
  iteration**; `sessFront` and the raw attachment-check store are the same injected
  wrapper, so a `Get` there consumes a `store.Get(sessionID)` error meant for a downstream
  fail-safe check *before* it fires — changing production read semantics, not just a test.
  This is verified end-to-end: the reverted attempt failed
  `TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions/attachment_check_error_fails_safe`.
  **Scale (corrected, opus review):** exactly **3 injection cases in 2 files**
  (`session_reconciler_test.go:7661,7833`; `session_reconciler_progress_test.go:202`) —
  the mechanism is real, the blast radius is not suite-wide.
- **Suspected per-tick Get storm (benchmark pending, NOT load-bearing).** The 1854 refresh
  + the blanket pre-pass @2743 would add ≥1 store read per session per tick. Spec §4.3/§8
  pre-accepts "refresh-on-write, fix if hot" — gated on a benchmark **never run** — so this
  is "suspected," not a proven regression. The revert rests on the two reasons above.

**Conclusion:** the cutover belongs bundled with the lockstep drop (§3 6d), done
primarily via **write-returns-`Info`** (spec §4.3 escalation) — the ~24 `ApplyPatch`
sites know the patch they wrote, so `refreshSessionInfo` reconstructs the post-write
`Info` from the returned patch instead of a blanket `Get`. **BUT write-returns-`Info` does
NOT cover every refresh (opus review, load-bearing):** the ~11 refresh sites split into
- **adjacent-single-write** (e.g. ~1596 post-heal) — write-returns-`Info` works;
- **status-close** (1558/1703/1802/2013 + the rollback half of the 2743 pre-pass) — need a
  **targeted store re-read** (`Info.Closed` from `Status`, §1);
- **aggregating** (2521 asleep-drift, 2767 wakeTargets) — reflect cumulative prior-block
  mutations with no single returnable write; need a **targeted re-read** too.

So 6d is write-returns-`Info` **for adjacent writes + a targeted `Get` for status-close /
aggregating refreshes** — not a pure returned-patch model. The two overlays
(`reset_committed_at` freeze at snapshot build, `restart_requested` at 2098) still apply.

---

## 3. Ordered sub-phase backlog (re-sequenced: safe conversions first, cutover last)

- [ ] **6a — codec fidelity gaps** (the new first step; additive, byte-identical, NO
  store-I/O change). Add the `Info` mirrors the remaining raw decision reads need,
  each with a `TestSessionClassifierInfoEquivalence` case. The three genuine codec gaps
  (verified absent from `Info`): `session_id_flag` (freshRestartSessionKey ~2139),
  `template_overrides` (ParseTemplateOverrides ~3918), and the `wake_attempts` fidelity
  gap (raw `!="" && !="0"` ~857 vs the int-parsed `Info.WakeAttempts`, which needs a
  raw-string mirror to stay identical). NOTE (opus review): `isDrainAckStopPending`
  (~54) is NOT a codec gap — `Info.MetadataState`/`StateReason` already exist, so its
  `*Info` sibling belongs in 6b.
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
  remove `ordered []beads.Bead`/`beadByID`.** Primarily **write-returns-`Info`** (§2)
  for adjacent-single-write refreshes, PLUS a **targeted store re-read for the
  status-close and aggregating refreshes** (§1/§2 — write results cannot reconstruct
  `Info.Closed`, which derives from `Status`). Build the two intra-tick overlays
  (`reset_committed_at` freeze at snapshot build; `restart_requested` overlay at 2098).
  **Before deleting the blanket pre-pass @2743, regenerate the COMPLETE set of
  forward-pass sites that write an `Info`-read key and lack a per-site refresh** — the
  opus review found the set under-specified: at minimum `SleepPatch`@2631 (max-age kill)
  + `SleepPatch`@2705 (idle kill) write `state=asleep`/`sleep_reason` (read via
  `LifecycleInputFromInfo`→awake scan), and `RestartRequestPatch`@2144 (restart handoff,
  `continue`@2173) writes `continuation_reset_pending`/clears `started_config_hash` — all
  seen TODAY only via the blanket pre-pass. Each must gain a write-returns-`Info` (or
  re-read) refresh. Drop each `session.Metadata[k]=v` lockstep + its dependent same-tick
  reads as ONE commit with a read-after-write test (incl. an idle/max-age-kill test that
  asserts the awake scan sees `state=asleep`). LANDMINE.
- [ ] **6e — join the guard.** Extend `snapshotInfoOnlyFiles`
  (frontdoor_di_guard_test.go:83) to ALSO forbid raw `.Metadata[` on session beads
  (Audit C blocker: today it only forbids the 4 raw snapshot accessors), and add
  the reconciler files once raw-free.

**Realistic scope per session:** 6a is one small, safe, verifiable commit set (additive
mirrors + oracle). 6b is several small oracle-backed commits. 6c and 6d are the large
landmines and likely span multiple sessions each. Checkpoint after each.

**Reverted-attempt note:** the naive Get-cutover (`refreshSessionInfo → sessFront.Get`
+ the two overlays) was implemented and reverted this session (uncommitted experiment —
no git artifact; traced by inspection: the unconditional refresh at
session_reconciler.go:**1854** feeds `infoPostZombie`, and the injection wrapper
`sessionObservationGetErrorStore{remaining:1}` is shared by `sessFront` and the
attachment check). It built and passed the reset-pending/awake/classifier suites but
failed
`TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions/attachment_check_error_fails_safe`
because the refresh-`Get` consumed the injected attachment-check error. That is the
evidence behind moving the cutover to 6d via write-returns-`Info`.

**Line-number caveat:** this doc's anchors were captured against base `48ab9e107`; the
live tree has drifted (e.g. the unconditional refresh is `1854` not `1876`; the blanket
pre-pass is `2743` not `2742`). Re-grep before editing. **Design status: GO-WITH-CHANGES**
per the opus red-team (`step6-design-redteam`) — all findings above folded in; the
store-centric front door, two-overlay intra-tick model, lockstep-last ordering, and
write-returns-`Info`-for-adjacent-writes are validated; the required changes were doc
tightenings (status-close/aggregating read case, persist-error scoping, complete 6d
refresh set), not architecture rejections.

---

## 4. Guard note (6e)

`snapshotInfoOnlyFiles` (frontdoor_di_guard_test.go:83-91) currently lists
`{template_resolve.go, session_name_lookup.go, cmd_citystatus.go,
session_reconciler_trace_cycle.go, providers.go, nudge_dispatcher.go,
named_sessions.go}` and its guard only forbids the four raw snapshot accessors
(`.Open()/.FindByID(/.FindSessionBeadByTemplate(/.FindSessionBeadByNamedIdentity(`).
To make the reconciler files meaningfully raw-free, 6e must extend the guard to also
forbid raw session-bead `.Metadata[` reads/writes (minus the documented raw-by-design
exceptions: the witness full-resync @361-363 and work-bead reads @3391/4213).
