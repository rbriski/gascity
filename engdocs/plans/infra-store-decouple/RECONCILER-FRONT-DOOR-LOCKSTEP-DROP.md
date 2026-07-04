# Reconciler front-door — the LOCKSTEP DROP (next phase)

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `.claude/worktrees/object-front-doors`, **HEAD `33de818df`** (re-grep
`git rev-parse HEAD`; line numbers below drift as you edit — always re-grep).

## Where things stand

The reconciler's decision reads are all on the typed `session.Info` snapshot
(`infoByID`), and every snapshot refresh is write-returns-`Info` — **no code
re-derives `Info` from the raw working bead anywhere on the decision or refresh
path.** The blanket pre-pass, both aggregating refreshes, and `refreshSessionInfo`
are deleted (see `RECONCILER-FRONT-DOOR-STEP6-PREPASS-AUDIT.md`). Verified by the
comprehensive reconciler suite (211-212s green) + a 4-lens capstone fable review
(0 defects).

**What's still physically present but now READ-DEAD for decisions:** the raw
`ordered []beads.Bead` working set, `beadByID`, `circuitSessionByIdentity`,
`sessionLookup`, and **13 `session.Metadata[k]=v` lockstep mirror writes**. The
lockstep drop removes them.

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

1. **`advanceSessionDrainsWithSessionsTraced`** (called ~3338) — takes `sessionLookup`
   (= `beadByID[id]`) + `ordered` + `wakeEvals`. It mutates drains off the raw bead
   (`completeDrain`, `cancelSessionDrainFor*`). This is the LAST real consumer of
   `sessionLookup`/`beadByID`. STEP6-DESIGN §7 6c-audit note: its `ordered`/`sessionBeads`
   param is DEAD in the production call (`wakeEvals` always non-nil there → the
   `computeWakeEvaluations` fallback never fires), but non-prod callers pass
   `wakeEvals==nil`, so it can't be dropped without handling those. Convert its raw-bead
   mutations to the typed store (`sessFront`) + retire `sessionLookup`.
2. **The Phase-0.5 circuit-breaker block** (~1347-1388) — builds `circuitSessionByIdentity`
   from `ordered`, reads `circuitSessionByIdentity[identity]` (a raw `*beads.Bead`) for
   CB restore. CircuitState already has a typed accessor (`session.Store.CircuitState(id)`,
   Step 5). Route the CB restore through it; drop `circuitSessionByIdentity`.
3. **`buildAwakeInputFromReconciler`** (~3040s) — takes `ordered` for the SessionBeads
   slice order (see the invariant above). Replace the `ordered` domain with an
   order-preserving `[]Info` (or `[]string` of IDs + `infoByID`) built once at tick start.
4. **The post-loop sleep-policy loop** (~3198) — `target.session.Metadata["sleep_intent"] = ""`
   is a RAW mutation on `target.session` after the awake scan. Convert to a typed write
   (fold onto infoByID if any later read consumes it, else route through the store).
5. **`newSessionBeadSnapshot` / `resolvePreservedConfiguredNamedSessionTemplate`** (bucket-D,
   STEP6-PREPASS-AUDIT / §7) — the whole-bead template subsystem still reads raw beads;
   feed it from a store source. HARDEST — may need a store `List`.
6. **The 13 raw `session.Metadata[k]=v` mirrors** (re-grep `session\.Metadata\[.*\]=` in
   session_reconciler.go): these are the lockstep. Each has a fold beside it now. Delete
   them ONLY after 1-5, in the same commit as removing the raw working set (nothing reads
   the raw bead by then).

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
- **`buildPreparedStart` residue** (`recoverRunningPendingCreate` failure path): a few
  keys not threaded into the fold — decision-inert (documented at the fold site).
  Thread it out here if you want full byte-identity, else leave (inert).

## 6e — the CI guard (last)

Extend `snapshotInfoOnlyFiles` (`frontdoor_di_guard_test.go:83`) to ALSO forbid raw
`session.Metadata[` reads/writes on the reconciler decision path (today it only forbids
the four raw snapshot accessors), then add the reconciler files once raw-free. Keep the
documented raw-by-design exceptions (witness full-resync, work-bead reads).

## Suggested commit sequence

1. **CB block → `Store.CircuitState`** (drop `circuitSessionByIdentity`). Small, self-contained.
2. **`advanceSessionDrains` off the raw bead** (retire `sessionLookup`; handle the dead
   non-prod param). Medium.
3. **`buildAwakeInputFromReconciler` domain → order-preserving `[]Info`/`[]string`**
   (NOT map iteration) + the post-loop `sleep_intent` write. The awake-scan invariant lives here.
4. **`newSessionBeadSnapshot` off a store source** (bucket-D, hardest — may need `List`).
5. **Drop the 13 lockstep mirrors + `beadByID` + `ordered []beads.Bead` + cut the tick-start
   build to the store**. Nothing reads the raw bead by now.
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
