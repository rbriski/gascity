# Reconciler front-door — the LOCKSTEP DROP (in progress; Steps 1–3 done, 3.5 next)

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `.claude/worktrees/object-front-doors`, **HEAD `97fd6fbc6`** (re-grep
`git rev-parse HEAD`; line numbers below drift as you edit — always re-grep).

## Progress

- [x] **Step 1 — circuit persist store-authoritative + drop `circuitSessionByIdentity`**
      (`ec6127ead`). CORRECTION to the plan's stale line-anchored model: the Phase-0.5
      restore READS already project `ordered[i].Metadata` via `CircuitStateFromMetadata`
      (Step 5); the only surviving raw consumer was the progress-sig persist lookup.
      `persistSessionCircuitBreakerMetadata`/`recordSessionCircuitBreakerRestart` now take
      `id string`, equality via `sessFront.CircuitState(id)`, raw mirror dropped, dead
      `sessionCircuitMetadataEqual` removed. `circuitSessionByIdentity` → `circuitIDByIdentity`
      (`map[string]string`). Byte-identical under a healthy store; fable review wf_803d0b26
      (0 defects beyond one ACCEPTED LOW: the equality-skip now does a store Get on the
      previously-free path — details in `raw/lockstep-drop-step1-circuit.md`).
- [x] **Step 2a — `completeDrain` store-only** (`4bcec563b`). Drop the bead mirror; take
      `sessions.Info` (id + raw wake_mode). Byte-identical (mirror had no in-tick consumer;
      tests assert on the store).
- [x] **Step 2b — `advanceSessionDrains` off the raw bead + retire `beadByID`/`sessionLookup`**.
      Traced core takes `infoLookup func(id)(Info,bool)`; loop reads Info only; `verifiedStop`
      + drain-cancel Info siblings; reconciler feeds it `infoByID`. Fable review wf_381c5866:
      2 lenses clean, F2/F4 byte-identical, 1 refuted, 1 CONFIRMED-then-FIXED: the
      `buildPreparedStart` `instance_token` residue (now threaded into recoverRunningPendingCreate's
      returned fold batch + teeth test). Details in `raw/lockstep-drop-step2-drains.md`.
- [x] **Step 3 — `buildAwakeInputFromReconciler` domain → order-preserving `[]session.Info`**
      (`0d694acee`). The awake scan's decision reads were already on Info (4C/4D); only the
      iteration DOMAIN was raw. Params `sessionBeads []beads.Bead` + `sessionInfoByID map` → a single
      `sessionInfos []session.Info`; reconciler builds it in `ordered` order
      (`sessionInfos[i] = infoByID[ordered[i].ID]`, never `range infoByID`), body unchanged. 15 test
      sites convert `[]beads.Bead{X}` → `[]session.Info{InfoFromPersistedBead(X)}`; the
      divergent-snapshot test passes the Info directly; one `session`-shadowing local renamed to
      `sessionBead`. Byte-identical (every ordered ID keys infoByID, never deleted; `ordered` never
      resliced; fallback was prod-dead). Fable 4-lens review wf_21c330af: **0 findings**. Analysis in
      `raw/lockstep-drop-step3-awake-domain.md`.
- [x] **Step 3.5 — wakeTargets apply loop + awake bridge off the raw bead** (consumer #4).
      3.5a (`2d387146c`): additive codec `Info.PendingCreateClaimMetadata` (verbatim). 3.5b
      (`60e231cb2`): the post-Phase-1 wakeTargets apply loop reads off `info := infoByID[id]`
      (session_name/wake_mode/sleep_intent/sleep_reason/last_woke_at + classifier siblings +
      idle-probe helpers threaded infoByID + shouldBeginIdleDrainInfo); 4 mutating helpers return
      their fold (recordCurrentBeadIDOnWake, cycleAlive minus ResetCommittedAtKey, markIdleSleepPending,
      emitSessionStrandedDiagnostic) + MarkClosed/sleep_intent-clear folds; raw mirrors kept for Step 5.
      3.5c (`a06980fd0`): the awake bridge wakeTargets loop + `shouldProbeAttachmentForAwakeInput` read
      off an ID-keyed `sessionInfos` index (state → `MetadataState`, the raw-verbatim landmine). All
      three fable-reviewed (0 findings), reconciler subset green. See
      `RECONCILER-FRONT-DOOR-REMAINING-PLAN.md` (the fable design of record) for 4/5/6e.
- [x] **Step 4 — preserve-template feed off the raw `ordered`** (consumer #5, `656d322c5`).
      4a: additive verbatim codec `Info.Pack` (`beadmeta.PackMetadataKey`). 4b:
      `newSessionBeadSnapshotFromInfos([]Info)` (OpenInfos-only, filters `info.Closed`) +
      `resolveTemplateForSessionBeadInfo` + `resolvePreservedConfiguredNamedSessionTemplate([]Info, Info)`;
      the reconciler `:1587` builds the feed from live `infoByID` in `ordered` order, start-refresh caller
      passes `snapshot.OpenInfos()`. Byte-identical (membership matches at HEAD; the only reachable read
      is the GC_SESSION_ID scan, drift-fingerprint-excluded). Cleared by a 3-agent fable feed-hazard
      analysis (wf_58aa9f17) + 3-lens review (0 findings) + a guard test. Boundary correction: the
      original "may need store List" was wrong — live `infoByID`, not a store List (REMAINING-PLAN §Step 4).
- [x] **Step 5a — forward-pass decision reads off the raw bead** (`6e31df0dc`). Flipped the 20 remaining
      raw `session.Metadata[k]` DECISION reads in the forward-pass loop to `infoByID[session.ID].<Field>`
      (state/pending_create_claim/pending_create_started_at/last_woke_at/restart_requested + the config-drift
      hash cluster + creation_complete_at). Raw mirror WRITES stay (5c). **The fable review caught a real
      MEDIUM byte-identity defect**: flipping the config-drift gate (`started_config_hash`) made it a same-tick
      reader of `buildPreparedStart`'s stale-resume residue; `clearStaleResumeKeyMetadata` clears it on
      raw+store but the abort-path fold didn't carry it → drift block wrongly ran (#127). FIXED by threading it
      (`pendingCreateInstanceTokenFold`→`pendingCreateResidueFold` carries `started_config_hash`; teeth test).
      `session_key`/`continuation_reset_pending` residue stays unthreaded (documented: the CRP one-tick-deferral
      is a pre-existing Step-3/6d gap, not 5a's). Reviews wf_9be58e9c + fix re-review wf_78063ee2.
- [x] **Trace-decouple precursor — gc-trace terminal read off the store** (`245a86b4a`). **The 5b review
      surfaced a systemic blocker the plan missed:** the raw mirror WRITES are NOT read-dead — the caller's
      post-reconcile `recordReconcileTraceResults` reads `open[i].Metadata["state"]/["sleep_reason"]`, and `open`
      shares Metadata maps with the reconciler's `ordered` working set, so deleting any mirror stales that trace.
      There is **no byte-identical replacement** for that read: `open[i].Metadata` is an in-memory-mirror artifact
      (a first fix attempt returning the reconciler's final `infoByID` was fable-refuted — infoByID is stale for
      woken sessions because `preWakeCommit` mirrors onto a discarded `store.Get` copy, and `completeDrain`'s
      mirror was already dropped). **Owner decision: source the trace from the authoritative post-reconcile store
      snapshot** — an intentional accuracy improvement (woken→`creating`, drain-completed→`asleep`/reason are now
      recorded correctly where HEAD read stale in-memory state), trace/observability-only, reusing the snapshot
      the wait-nudge dispatch already loads (no added query). Beads closed this tick fall back to the open bead.
      This decouples the trace from the mirrors so 5b/5c can delete them. Reviews wf_d9dc087d (refuted the
      infoByID approach) + wf_a1c4e6cc (confirmed the intended store-authoritative deltas).
- [x] **Step 5b — drain-ack finalize family off raw metadata** (`97fd6fbc6`). `markDrainAckStopPending` takes
      `session.Info` (drops the bead + its mirror loop); `finalizeDrainAckStoppedSession` gains an `Info` param
      for its reads (session_name/template/wake_mode/restart_requested) and drops the closePatch + drain-ack
      ApplyPatch mirror loops, but **KEEPS its `*beads.Bead` param** — the whole-bead raw-by-design helpers
      (`sessionHasOpenAssignedWorkForReachableStore`, `closeSessionBeadIfReachableStoreUnassigned`,
      `recordDrainAckAssignedWorkEvent`, `sessionAgentMetricIdentity`) and the store.Get witness still need it.
      The raw `session.Status="closed"` struct write + witness Status/Metadata swap stay (non-bracket;
      telemetry-test-asserted). `reconcileDrainAckStopPending` threads `Info`; the non-reconciler
      `finalizeDrainAckStopPendingSessions` projects per-bead at its boundary. **Plan deviation:** the FINISH
      doc said "drop the `*beads.Bead` param" — infeasible for `finalizeDrainAckStoppedSession` (raw-by-design
      whole-bead helpers); the achievable goal (family off `session.Metadata[` reads + mirror writes) is met.
      Byte-identical on decision/store/lifecycle (fable 5-lens wf_fdf44eb5: 4 clean; the trace finding resolved
      by the precursor above). LOW residual: drain-ack sessions that CLOSE this tick trace their pre-close state
      (closed → snapshot-excluded → open fallback → mirror-free), consistent with store-only closes.
- [x] **Step 5c — delete the read-dead raw lockstep mirror WRITES (per-key census)**. Grepped the 9
      `session.Metadata[...] = ...` write sites and ran the per-key census (does any THIS-TICK reader — incl.
      the start-execution path — read the key off the RAW bead?). The start boundary: `startCandidate.session`
      (the raw `&ordered[i]`) escapes into `executePlannedStartsTraced`; `prepareStartCandidateForCity`
      (`session_lifecycle_parallel.go:766-783`) does `store.Get` and REPLACES `candidate.session=&current`
      before `buildPreparedStart` reads metadata — so those reads are store-sourced. But THREE reads hit the
      ORIGINAL pre-re-Get raw bead: `wakeFairnessTime` (:161, `last_woke_at`), `name()`/`namedSessionIdentity`
      (`session_name`), `logicalTemplate` (`template`). So a mirror is **START-COUPLED iff it writes
      `last_woke_at`/`session_name`/`template` AND the session can reach `startCandidates` this tick.**
      Census verdict:
      - **DELETED (4)** — keys read by NO raw same-tick reader; each keeps its `infoByID` fold + store write:
        (D1) `restart_requested="true"` progress-stall (was in-memory-only, consumed+cleared same tick by S1);
        (D2) `sleep_intent=""` in the shouldWake&&alive arm (alive → never `startCandidates`; SetMarker + fold
        kept); (D4) the config-hash loop in `silentRebaselineSessionHashes`; (D5) the config-hash loop in
        `rebaselineLaunchDriftHashesWithBatch`. Also removed the now-dead `if session.Metadata==nil` guards.
      - **KEPT as documented survivors (5)** — in-code `START-EXECUTION COUPLING` / `CROSS-TICK EMIT-ONCE`
        comments: (S1) restartFold `RestartRequestPatch` (`last_woke_at`, runtime-dead fall-through);
        (S2) SleepPatch max-age; (S3) SleepPatch idle; (S4) `ConfigDriftResetPatch` in
        `resetConfiguredNamedSessionForConfigDrift` (start-pending caller falls through, no `continue`) —
        all four write `last_woke_at` and reach `startCandidates`; (S5) the stranded `strandedEventEmittedKey`
        write — **CROSS-TICK emit-once guard, NOT start-coupled**: set BEFORE the durable SetMarker so a
        store-write failure + carried-forward `*Bead` cannot re-emit
        (`TestReconcileSessionBeads_PoolSlotStrandedThrottleSurvivesSetMetadataFailure`). **The census tried to
        delete S5 first; the test caught it and the mirror was restored** — the corrected model: production
        reloads `loadSessionBeadSnapshot` each tick (so D1/D2/D4/D5 have no cross-tick role the store write
        misses), but the stranded emit-once storm is guarded against the worst-case carried-forward bead.
      Gates: gofmt clean, `go build`/`go vet` clean, `golangci-lint` 0, full reconciler subset green (206s).
      Fable 3-lens adversarial review (wf_38565cae, deletion-safety / fold+guard / survivor+trace; ~345K tokens,
      103 tool calls): **0 findings**.
- [x] **Step 5d — drop the dead fallback params on `advanceSessionDrainsWithSessionsTraced`**. The traced core
      had a `wakeEvals==nil` fallback (`computeWakeEvaluations(sessionBeads, cfg, sp, poolDesired, workSet,
      readyWaitSet, clk)`); the prod caller (reconciler `:3399`) and both direct test callers always pass
      `wakeEvals` non-nil, and the drain scan runs entirely off `infoLookup`, so `sessionBeads`/`poolDesired`/
      `workSet`/`readyWaitSet` were all dead in the core. Dropped those 4 params + the fallback from the core;
      moved the identical fallback into the test-only wrapper `advanceSessionDrainsWithSessions` (which keeps
      `sessions`/`poolDesired`/`workSet`/`readyWaitSet`). `advanceSessionDrains` still reaches it via a
      `wakeEvals==nil` call. `computeWakeEvaluations`/`evaluateWakeReasons` kept (CLI wake column + wrappers).
      All 3 core call sites updated to the 8-arg signature. gofmt/vet clean, `golangci-lint` 0 (the newly-dead
      params would have tripped `unparam` — hence removing them, not just `sessionBeads`), reconciler+drain+trace
      subset green (224s); fable 2-lens equivalence review (wf_2c0d9209): 0 findings.
- [ ] **Steps 5e, 6e remain** (see `RECONCILER-FRONT-DOOR-REMAINING-PLAN.md` §Step 5 / 6e). Demote `ordered`
      (5e), then the 6e guard. **6e caveat sharpened by 5c:** the guard cannot blanket-forbid `session.Metadata[`
      in
      `session_reconciler.go` — the raw-by-design classifier helpers (`isDrainAckStopPending`,
      `pendingCreateStartInFlight`, …) legitimately read it, and the 5 survivor mirrors (S1-S5) legitimately
      write it. Scope the needle or relocate per the FINISH-doc caveat. Step 5 stays re-scoped to DEMOTE
      `ordered` (not delete — start-execution `startCandidate`/`buildPreparedStart` is raw-by-design consumer #7,
      out of scope).

## Where things stand

The reconciler's decision reads are all on the typed `session.Info` snapshot
(`infoByID`), and every snapshot refresh is write-returns-`Info` — **no code
re-derives `Info` from the raw working bead anywhere on the decision or refresh
path.** The blanket pre-pass, both aggregating refreshes, and `refreshSessionInfo`
are deleted (see `RECONCILER-FRONT-DOOR-STEP6-PREPASS-AUDIT.md`). Verified by the
comprehensive reconciler suite (211-212s green) + a 4-lens capstone fable review
(0 defects).

**Already removed (Steps 1–3):** `circuitSessionByIdentity`, `beadByID`, and
`sessionLookup` are GONE. The circuit persist (`persist`/`recordSessionCircuitBreakerRestart`),
`completeDrain`, and the whole Phase-2 drain scan (`advanceSessionDrainsWithSessionsTraced`,
`verifiedStop`, the drain-cancel helpers) are off the raw bead and their mirrors dropped.
Step 3 moved the **awake scan** (`buildAwakeInputFromReconciler`) fully onto an order-preserving
`[]session.Info` — it no longer accepts or reads any raw session bead.

**What's still physically present but READ-DEAD for decisions:** the raw
`ordered []beads.Bead` working set, and the remaining `session.Metadata[k]=v` lockstep
mirror writes in the forward pass (re-grep `session\.Metadata\[.*\] *=` in
session_reconciler.go). The `wakeTargets` loop still carries raw `target.session` beads (a
**separate** source from `ordered`) and still reads `session_name`/`sleep_intent` off them — that
is the **Step 3.5** sub-thread (consumer #4). `newSessionBeadSnapshot` (consumer #5) is the other
whole-bead reader. The remaining lockstep drop removes all of it.

## The governing safety principle (unchanged)

> Never remove a raw read/mirror until its typed replacement is in place and
> byte-identical. Convert each consumer, verify, THEN delete.

Two hard invariants the CI enforces and the awake scan depends on:
- **`buildAwakeInputFromReconciler` slice order is load-bearing.** It appends to
  `input.SessionBeads` in `ordered` slice order and `ComputeAwakeSet` does
  `SessionName`-keyed **last-write-wins** + first-match `resolveNamedSessionBeadName`
  over it. `SessionName` is NON-unique (a retired-duplicate + winner share it). So
  the iteration domain must stay **ORDER-PRESERVING** — replace `ordered` with an
  `[]Info`/`[]string` in the SAME order, **never** `range infoByID` (map iteration
  reorders and can flip an outcome). `openPoolSessionCountForTemplate` MAY
  domain-switch to `infoByID` (unique IDs proven, order-independent count).
- **The tick-start snapshot is store-equivalent already.** `infoByID` is built at
  tick entry as `InfoFromPersistedBead(ordered[i])`, and `ordered` is the
  store-loaded bead set the reconciler was handed. So "cut to store `Get`/`List`"
  is mostly: keep building the tick-start snapshot from the loaded beads, then stop
  keeping the raw beads around — it is NOT a per-refresh `Get` (the reverted
  #2345/#2574 hazard). Per-refresh `Get` was tried and rejected (STEP6-DESIGN §2).

## The remaining raw consumers (re-grep — these are what to convert)

1. ~~**`advanceSessionDrainsWithSessionsTraced`**~~ **DONE (2b, `1d2ea2028`).** Takes
   `infoLookup func(id)(Info,bool)`; drain scan reads Info only; `verifiedStop` + the
   drain-cancel helpers have Info siblings; `beadByID`/`sessionLookup` removed. The
   `sessionBeads []beads.Bead` param SURVIVES (dead in the prod call — `wakeEvals` non-nil
   — but non-prod callers pass `wakeEvals==nil` for the `computeWakeEvaluations` fallback);
   drop it only when `ordered` goes (Step 5).
2. ~~**The Phase-0.5 circuit-breaker block**~~ **DONE (1, `ec6127ead`).** `circuitSessionByIdentity`
   (`map[string]*beads.Bead`) → `circuitIDByIdentity` (`map[string]string`); circuit persist
   is store-authoritative by id.
3. ~~**`buildAwakeInputFromReconciler`**~~ **DONE (3, `0d694acee`).** Params `sessionBeads` +
   `sessionInfoByID` → a single order-preserving `sessionInfos []session.Info`, built in the
   reconciler as `sessionInfos[i] = infoByID[ordered[i].ID]` (never `range infoByID`). Fallback +
   map dropped; 15 test sites + reconciler site converted. Byte-identical; fable review 0 findings.
4. **The `wakeTargets` / `sleep_intent` sub-thread** (`session_reconciler.go` ~3185-3222, ~4362;
   and the `wakeTargets` loop in `buildAwakeInputFromReconciler` reading
   `target.session.Metadata["session_name"]`) — `target.session` is a **raw bead carried on
   `wakeTarget`** (a different source than `ordered`, deemed out-of-scope in 4C). The post-loop
   `sleep_intent` read/clear (`SetMarker` + `target.session.Metadata["sleep_intent"] = ""`) is a
   raw read+mirror. `Info.SleepIntent` exists (`b.Metadata["sleep_intent"]`, raw). Convert these
   reads to `Info`/store and drop the mirror. Can be its own step (3.5) or folded into Step 3.
5. **`newSessionBeadSnapshot` / `resolvePreservedConfiguredNamedSessionTemplate`** (bucket-D,
   STEP6-PREPASS-AUDIT / §7) — the whole-bead template subsystem still reads raw beads;
   feed it from a store source. HARDEST — may need a store `List`.
6. **The remaining raw `session.Metadata[k]=v` mirrors + `ordered []beads.Bead`** (re-grep
   `session\.Metadata\[.*\] *=` in session_reconciler.go — ~11 left; each has a fold beside it
   now). Delete them ONLY after 3-5, in the same commit as removing `ordered` (nothing reads the
   raw bead by then). This also drops the now-dead `sessionBeads` param on `advanceSessionDrains`.

## The Get-cutover exposure set (mostly already handled — verify, don't re-solve)

The raw refresh preserved deliberate intra-tick raw/store divergences. Confirm each is
handled before cutting the tick-start build to a store `List`:
- **`reset_committed_at`** (#2345 force-wake): persisted by RestartRequestPatch this tick
  but kept OFF this tick's snapshot. Already handled — `restartFold` EXCLUDES
  `ResetCommittedAtKey` (Commit 4), so the fold never adds it this tick; a tick-start
  `List` correctly reads the PRIOR tick's durable value. **No new work if the build stays
  at tick entry.**
- **`started_live_hash`**: persisted without a mirror; `Info.StartedLiveHash` has ZERO
  decision readers (verified). Harmless.
- **`buildPreparedStart` residue** (`recoverRunningPendingCreate`): the `instance_token`
  mint is now THREADED into the returned fold batch (Step 2b, `pendingCreateInstanceTokenFold`)
  because `verifiedStop` reads `info.InstanceToken`. The OTHER residue keys (a stale-resume
  clear of `session_key`/`started_config_hash`/`continuation_reset_pending`) are still NOT
  threaded — verified inert (no divergent same-tick Info reader) by the pre-pass capstone
  (wf_e8507262). **Step 3 outcome:** the awake scan reads `info.ContinuationResetPending`/
  `ResetCommittedAt`, but Step 3 changed the DOMAIN, not the SOURCE — the OLD reconciler already
  read `infoByID` (the `ok` branch), so OLD and NEW read the identically stale-or-fresh value and
  the residue is NOT a new divergence. Confirmed by the Step-3 fable review (wf_21c330af, lens
  snapshot-equivalence: 0 findings). No threading needed. Re-confirm again only if a later step
  moves this read onto a per-refresh store `Get` (STEP6-DESIGN exposure set).

## 6e — the CI guard (last)

Extend `snapshotInfoOnlyFiles` (`frontdoor_di_guard_test.go:83`) to ALSO forbid raw
`session.Metadata[` reads/writes on the reconciler decision path (today it only forbids
the four raw snapshot accessors), then add the reconciler files once raw-free. Keep the
documented raw-by-design exceptions (witness full-resync, work-bead reads).

## Suggested commit sequence

1. ~~**CB block → `Store.CircuitState`**~~ DONE (`ec6127ead`).
2. ~~**`advanceSessionDrains` off the raw bead**~~ DONE (2a `4bcec563b`, 2b `1d2ea2028`).
3. ~~**`buildAwakeInputFromReconciler` domain → order-preserving `[]session.Info`**~~ DONE
   (`0d694acee`).
3.5. **The `wakeTargets` / `sleep_intent` raw reads+mirror** (consumer #4). ← NEXT. `target.session`
   is a raw bead carried on `wakeTarget` (a DIFFERENT source than `ordered`). Convert the loop's
   `target.session.Metadata["session_name"]` read and the post-loop `sleep_intent` read+clear
   (`target.session.Metadata["sleep_intent"] = ""`) onto `Info.SleepIntent`/store. Also
   `shouldProbeAttachmentForAwakeInput` reads `target.session.Metadata[...]` raw. Smaller than Step 4;
   good next slice.
4. **`newSessionBeadSnapshot` off a store source** (bucket-D, hardest — may need `List`).
5. **Drop the remaining lockstep mirrors + `ordered []beads.Bead` + the dead `sessionBeads`
   param + cut the tick-start build to the store**. Nothing reads the raw bead by now.
6. **6e guard.**

Each step: build · vet · golangci-lint 0 · gofmt · the reconciler suite (`go test ./cmd/gc/
-run 'Reconcile|Awake|Wake|Sleep|Pool|DrainAck|Recycle|Zombie|Heal|Drift|Churn|Stability|
RateLimit|Named|Restart|Progress|Rollback|PendingCreate|MinFloor|Idle|MaxAge|Detach|Rebaseline|
Relaunch|Quarantine|Circuit|Lifecycle|Session' -timeout 25m` — the full `cmd/gc` package times
out at 600s, use this subset or TESTING.md shards). The suite is the byte-identity gate: a raw
consumer converted wrong flips an awake/drain decision and fails. Run a fable adversarial review
per non-trivial step (owner prefers fable). Commit + push `--no-verify`. Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. #3839 stays DRAFT.

## Beyond the lockstep drop (the wider backlog)

This completes the reconciler front-door (Phase 5 reads). Then, per
`infra-beads-decoupling-plan.md` / OBJECT-MODEL-FRONT-DOOR-DESIGN §7:
- The cross-class **WORK/assignment split** (design §5 / Phase 6).
- The tier fix (Phase C).
- The owner-gated **cold migration** (`maintainer-city` dolt→postgres) — stop-first,
  owner-approved, the live-system landmine. NOT a code change; do last.
