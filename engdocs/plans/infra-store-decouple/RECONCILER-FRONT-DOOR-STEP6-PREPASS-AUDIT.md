# Step 6d — blanket pre-pass deletion: the complete fold audit

Authoritative enumeration for deleting the blanket pre-pass
(`for i := range ordered { refreshSessionInfo(ordered[i].ID) }` @~2919, right
before the awake scan). Produced by a 4-region fable audit + opus synthesis
(workflow `wf_df3cae94`, grounded at HEAD `61e646e18` = 6d Commit 4 DONE).

**Why this is big:** the pre-pass re-projects EVERY session from the raw bead
before `buildAwakeInputFromReconciler` (the only typed reader of most late-loop
Info). To delete it, every forward-pass mutation to an **Info-projected** key must
be reflected on `infoByID` by iteration end, on every path (incl. `continue`s,
since the pre-pass runs after the whole loop). **12 mechanisms / 22 call sites.**
While the pre-pass is present, EVERY fold is MASKED (the blanket re-project heals
any missed fold before the scan) → byte-identical, no behavior change, and NO
isolated teeth test. Each masked fold is verified by review; the teeth tests
become possible only at the deletion.

## The complete deduped dependency set (12 groups)

Every helper takes `session *beads.Bead` and mirrors its persisted batch onto
`session.Metadata` after a successful `ApplyPatch` — all genuine pre-pass deps.
Line numbers are HEAD `61e646e18` (re-grep — they shift as folds land).

| # | Group | Sites | Fold | Notes |
|---|---|---|---|---|
| 1 | `checkRateLimitStability` | 1570, 1609, 2018, 2355 | **sig change** (return batch) → `ApplyPatch(batch)` on hit | mirrors via `markProviderTerminalError` OR `recordRateLimitQuarantine`; err path mutation-free |
| 2 | `attemptRollbackPendingCreate` | 1575, 2002, 2022 | `ApplyPatch(rollbackBatch)` **+ separate `MarkClosed`** | `last_woke_at=""`, `session_name=""` (iff explicit); @1575 ClearingClaim adds `state=failed-create`+claims. **Status close is STORE-ONLY** (never set raw Status) → `Closed` half is the separate MarkClosed reconstruction |
| 3 | `markDrainAckStopPending` | 1801, 2137 | `ApplyPatch(DrainAckStopPendingPatch(clk.Now().UTC()))` on `true` | `state=draining, state_reason=drain-ack-stop-pending, pending_create_*=""`; cross-session `isDrainAckStopPendingInfo` reader |
| 4 | `healStateWithRollback` heal#2 | 2364 | `ApplyPatch(healBatch)` (batch in hand) | **LINCHPIN** — identical to heal#1@1713; makes the fall-through base coherent for Groups 5-12 |
| 5 | stability/churn/clears | 2384, 2392, 2398, 2402 | capture+`ApplyPatch(batch)` | `checkStability`/`checkChurn`/`clearWakeFailures`/`clearChurn`; `wake_attempts`/`churn_count`/`quarantined_until`/session_key/continuation_reset_pending |
| 6 | idleSleep + detachedAt | 2377, 2380 | `ApplyPatch(SleepPatch/detached_at)` | `recoverPendingIdleSleep` (SleepPatch), `reconcileDetachedAt` (detached_at) |
| 7 | `recoverRunningPendingCreate` | 2414 | `ApplyPatch(CommitStartedPatch)` | started_*_hash, core_hash_breakdown, continuation_reset_pending, cond state/claims |
| 8 | `silentRebaselineSessionHashes` | 2451, 2712 | `ApplyPatch(rebaseline patch)` | started_*_hash + core_hash_breakdown; 2712 is post-@2692 |
| 9 | `relaunchAgentForLaunchDrift` | 2535, 2595 | `ApplyPatch(rebaseline patch)` on `true` | started_config/provision/launch_hash + core_hash (NOT live_hash) |
| 10 | `resetConfiguredNamedSessionForConfigDrift` asleep | 2726 | `ApplyPatch(ConfigDriftResetPatch + deferral clears)` | **#2574 HAZARD — clears restart_requested**; post-@2692 |
| 11 | max-age SleepPatch kill | 2801 | `ApplyPatch(batch)` (batch in hand) | state=asleep etc.; falls through to wakeTargets@2889 (load-bearing re-wake) |
| 12 | idle SleepPatch kill | 2875 | `ApplyPatch(batch)` (batch in hand) | same as 11 |

## STATUS: DONE. All 22 sites folded (batches 1-5); the blanket pre-pass, both
aggregating refreshes (infoAsleepDrift, wakeTargets), and `refreshSessionInfo` are
deleted. Verified by the comprehensive reconciler suite (211-212s green with every
refresh gone) + a 4-lens capstone fable review (wf_e8507262: 0 confirmed defects).

**One KNOWN-INERT residue (capstone review):** `buildPreparedStart` inside
`recoverRunningPendingCreate` (Group 7) mirrors a few extra keys onto the raw bead
(stale-resume clear of session_key/started_config_hash/continuation_reset_pending +
a session_key/instance_token mint) that are NOT threaded into the folded batch, so on
its failure path the snapshot keeps the pre-call values. Decision-inert: the block is
gated on pending_create_claim="true" (drives WakeCausePendingCreate → identical awake
decision to the pre-pass); the residue keys have no same-tick Info reader and self-heal
next tick. Deferred to the Get-cutover. Documented at the fold site.

## NOT-a-dependency (verified, so the set is exhaustive)

- **`resetConfiguredNamedSessionForConfigDrift` ALIVE lane @2538** — falls through
  to the aggregating refresh @2692, which re-projects it. Covered.
- **Live-drift `started_live_hash` re-apply @2633/@2659** — persisted WITHOUT a raw
  mirror, so the pre-pass never saw it either → out of scope for deletion; it is a
  Get-cutover exposure-set item (carry forward, don't fold).
- Non-Info keys: `env.*`, `session_circuit_*`, `provider_terminal_error_at`,
  `drain_at`, `slept_at`, `awake_started_at`, `sleep_policy_fingerprint`,
  `close_reason`/`closed_at`/`synced_at`, `startup_dialog_verified`, `live_hash`.
- tracker-only / by-value: `clearDrainTrackerForStopPending`, `queueDrainAckAsyncStop`,
  `dt.*`, `recordResetStallIfDue`, `beginSessionDrain`.

## The aggregating refresh @2692 (infoAsleepDrift)

Still a raw re-project; captures everything before it for sessions that REACH it.
It is a self-refresh, so folds BEFORE it only matter for the `continue` paths that
skip it; folds AFTER it (Groups 8@2712, 10@2726, 11, 12) are pure pre-pass deps.
The 2692 refresh (and the wakeTargets refresh @2944) must ALSO be retired at the
end (once every prior writer folds, they are redundant) — but the config-drift
decision reads `infoAsleepDrift`@2694, so 2692 becomes fold-based, not deleted
outright, unless that read moves onto the composed snapshot.

## Recommended commit sequence (each masked, review-verified; tests at deletion)

1. **heal#2 fold** (Group 4) — linchpin. [LANDED — see handoff]
2. batch-in-hand SleepPatch kills + fall-through drift (Groups 6, 7, 11, 12).
3. stability/churn/clears + detachedAt (Group 5).
4. rebaseline + relaunch (Groups 8, 9; gate 9 on `true`).
5. resetConfigDrift asleep (Group 10) — isolate for the #2574 clear.
6. drain-ack stop-pending (Group 3).
7. checkRateLimitStability sig change (Group 1) + rollback (Group 2, incl. the
   store-only `MarkClosed`). Largest surface — split if needed.
8. **DELETE the pre-pass** @2919-2921 + retire the 2692/2944 aggregating refreshes
   + add the comprehensive read-after-write tests (now finally possible) + the 6e
   guard forbidding raw `session.Metadata[` writes on the decision path.

**Coherence rule (SPEC §2):** each fold's base `infoByID[id]` must equal
`InfoFromPersistedBead(*session)` before its `ApplyPatch`. On a single forward path
only the current session is mutated; the landed folds keep the base coherent up to
each site — BUT heal#2 must land before its downstream readers (Groups 5-7 sit
after it), else their base is stale on the fall-through.

**Tests unlocked at deletion:** same-tick re-wake off a mid-tick sleep (11/12);
cross-session min-floor / drain-ack visibility (2/3); phantom-restart suppression
(10, #2574). All vacuous while the pre-pass masks them.
