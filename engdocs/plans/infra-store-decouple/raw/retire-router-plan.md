I have everything verified. `closeFailedCreateBead` does NOT release work (no `releaseWorkFromClosedSessionBead` call) — it's safe to route wholesale to sessionStore. Only `closeBead` is the dual-store landmine. The cutover mechanism is `registerSessionStoreBackend` at api_state.go:255/311, gated on `sessionRelocated(cfg)`. All anchors confirmed against live code. I'll now produce the plan.

# EXECUTION PLAN — Retire coordrouter.Router for SESSIONS (Track S) + Track-G Appendix

**Branch:** `plan/decouple-infra-beads` @ `2ea60d7a9` · **Target:** make controller callers class-aware so the Router can be deleted. Every anchor below was re-verified against live source.

**The threading contract (verified at `class_store.go:213`):** `resolveSessionStore(workStore, cfg, cityPath, rec)` returns the work store byte-identically at the default backend. Derive `sessionStore` at the `city_runtime.go` entry points exactly as `resolveNudgesStore` is already derived (verified live at `city_runtime.go:1517, 2197, 2251, 2760`), thread it **additively** alongside the existing `store`/`rigStores`, and repoint **only** session/wait ops. Work-assignment ops keep `store`/`rigStores`. Because default `sessionStore == workStore`, every phase is byte-identical at the default bd backend; correctness at the relocated backend is proven only at the END (guard tests + the unregister-ClassSessions cutover).

---

## 1. THE MASS-CLOSURE LANDMINE REGISTER (most important output)

A "landmine" is a **work read/write that shares one `store` handle with a session op**. Misrouting any of these to the (empty, when relocated) session store either **drops a live work assignment** or **closes a session that still holds live work** — a production mass-closure outage. The benign opposite (a session op left on the work store) is just a leak. **The rule for every site below: the work leg stays on `store`/`rigStores`; only the session leg moves to `sessionStore`.**

### 1a. The single dangerous mixed function: `closeBead` (verified `session_beads.go:2282-2324`)

`closeBead(store, id, reason, now, stderr)` takes ONE handle and does BOTH:
- **session leg** → `store.Get(id)` (`:2300`, idempotence + snapshot), `closeFailedCreateBead(store,...)` (`:2305`), `setMetaBatch(store, id, ClosePatch)` (`:2307`), `store.Close(id)` (`:2310`), `cancelStateAssignedToRetiredSessionBead(store,...)` (`:2319`, cancels gc:wait + extmsg).
- **work leg** → `releaseWorkFromClosedSessionBead(store, snapshot, stderr)` (`:2321`), which does `store.List(Assignee=..., Status=in_progress|open)` (`:2361`) and `store.Update(item.ID, {Assignee:"", Status:open})` (`:2387`) on every bead where `!session.IsSessionBeadOrRepairable(item)` (verified `:2367`). **Primary store only — no rigStores.**

**Resolution:** `closeBead` must become two-store-aware. Signature change:
```
closeBead(sessionStore beads.Store, workStore beads.Store, id, reason string, now time.Time, stderr io.Writer) bool
```
Inside: the Get/ClosePatch/Close/cancelWaits legs use `sessionStore`; `releaseWorkFromClosedSessionBead(workStore, snapshot, stderr)` uses `workStore`. **Verified `closeFailedCreateBead` (`session_beads.go:1785-1802`) does NOT call `releaseWorkFromClosedSessionBead`** — it is pure session-class and routes wholesale to `sessionStore` (its only ops are `setMetaBatch` + `store.Close` + `cancelStateAssignedToRetiredSessionBead`).

**All 7 production `closeBead` call sites** (verified by grep) — each must pass `(sessionStore, workStore)`:

| # | file:line | enclosing | risk |
|---|---|---|---|
| 1 | `session_work_guard.go:47` | `closeSessionBeadIfUnassigned` | after work-guard at `:36` |
| 2 | `session_work_guard.go:78` | `closeSessionBeadIfReachableStoreUnassigned` | after work-guard at `:67` |
| 3 | `session_beads.go:1916` | `reapStaleSessionBeads` | state=creating only (lower risk) |
| 4 | `session_beads.go:2012` | `cleanupDeadRuntimeSessionCorpses` | **HIGH**: comment `:2002-2006` says session MAY hold in_progress work |
| 5 | `session_beads.go:2218` | `closeSessionBeadIfRuntimeStoppedAndUnassigned` | after work-guard at `:2196/2207` |
| 6 | `session_reconciler.go:2572` | pool-free gate | gated by work read at `:2539` |
| 7 | `session_lifecycle_parallel.go:2065` | `rollbackPendingCreate` | failed-create rollback cascade |

### 1b. The work-guard reads that gate a session close (`session_work_guard.go` — verified live)

`closeSessionBeadIfUnassigned` (`:24`) and `closeSessionBeadIfReachableStoreUnassigned` (`:54`) are the canonical mixed shape: **work read → then session close**.
- `sessionHasOpenAssignedWorkForConfig(store, rigStores, session, cfg)` — `session_work_guard.go:36` → keep on `store`/`rigStores`.
- `sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, store, rigStores, session)` — `session_work_guard.go:67` (also derives `GraphOnlyListFor(store)` — must stay on the work/graph handle) → keep on `store`/`rigStores`.
- the `closeBead` and `closeFailedCreateBead` that follow → `sessionStore` (session leg) + `workStore` (work-release leg via the new closeBead signature).

If either guard read hits the empty session store it returns `hasAssignedWork=false` → the session is closed while holding live work. **This is the textbook mass-closure.**

### 1c. The work-read chokepoints (`session_reconciler.go` — verified inventory). Keep ALL on `store`/`rigStores`:

- `sessionHasOpenAssignedWorkForReachableStore` (`:2738`) + `sessionHasOpenAssignedWorkInStoreByIdentifiers` (`:2746`) — **THE central work-read chokepoint; pin the work store here.**
- `sessionHasAwakeAssignedWorkForReachableStore` (`:2768`) + per-store (`:2776`) — awake-keeping chokepoint.
- `reachableStoresForSession` (`:2882`) — **the in-caller federation that replaces the Router fan-out**; the `store` passed in MUST be the work store or every reachable read goes empty.
- graph-only legs: `graphOnlyHasAssignedWork` → `gol.ListGraphOnly` (`:2804`), `graphOnlyHasAwakeAssignedWork` → `gor.ReadyGraphOnly` (`:2853`) — work/graph backend.
- `sessionHasOpenAssignedWorkForConfig` (`:2711`), `sessionHasInProgressAssignedWorkForConfig` (`:2718`), and the tier leaves `:3157/:3161/:3196/:3224/:3246/:3256/:3264`.
- callers feeding these (all landmine work reads): `:247, :317, :354, :396` (drain-ack), `:1324, :1383, :1507, :1665, :2002, :2172, :2539` (the ~8 work reads inside the big loop).
- `resolveTaskWorkDir` (`:3906`) + `resolveTaskOptionOverrides` (`:3962`) — in_progress work reads on the **start path** (`work_dir`/`opt_*`). Misroute drops worktree/option overrides on a live city.
- `firstOpenAssignedWorkBeadInStoreByIdentifiers` (`:2951`), `collectSessionAssignedWork` (`:3118`) — event-payload / stranded-list work reads.
- `filterDetachedStrandedDiagnosticWork` → `clearDetachedProbeMetadata(item.store, ...)` (`:3062`) — **work-bead mutation on the bead's OWN store**; never a session op.

### 1d. The work reads inside `build_desired_state.go` (verified). Keep on `store`/`rigStores`/`workStores[i]`:

- `collectAssignedWorkBeadsWithStores` (`:660`) and its passes: List(in_progress) `:1113`, List(open) `:1131`, Ready `:1191/:1197` — **THE primary work read; misroute makes every live session look orphaned → mass drain/closure.**
- demand reads: `defaultScaleCheckCounts`→Ready `:1466`, `defaultNamedSessionDemand`→Ready `:1535`, `collectOpenUnassignedRoutedWork`→List `:3707`.
- shared helpers (class fixed by caller, all work here): `listBothTiersForControllerDemand` `:1703/:1707`, `readyForControllerDemandQuery` `:1717`, `liveReadyForControllerDemandQuery` `:1763`.
- work WRITERS in the mixed stampers: `stampRunSessionIdentity`→`store.SetMetadataBatch(wb.ID)` `:3482`; `stampRunRootFromStep`→`store.Get(rootID)` `:3508` + `SetMetadataBatch` `:3525`; `canonicalizeLegacyBoundAssignedWork`→`store.Update` `:3608`; `canonicalizeLegacyBoundUnassignedRoutedWork`→`store.Update` `:3669`. These read the session snapshot for guards (in-memory) but **write work beads** via the aligned work store.

### 1e. The `city_runtime.go` work legs (verified). Keep on `store`/`rigStores`:

- `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, ...)` — `city_runtime.go:2115` → **THE landmine in the tick**; pure pool-work release, skips `IsSessionBeadOrRepairable`.
- `reapClosedBeadWorktrees` `:1115` / `cleanupClosedBeadAgentHomeWorktrees` `:1118` — operate on `rigBeadStores()` (work).
- `filterAssignedWorkBeadsForPoolDemand` `:2808` — in-memory over the work snapshot.

### 1f. The completeness-sweep work landmines with session-sounding names (verified). Keep on work store:

- `pool_session_name.go:364` `releaseOrphanedPoolAssignment` → `store.Update(id, {Assignee:"", Status:open, ...})` on a **WORK** bead.
- `pool_session_name.go:260` `clearDetachedProbeMetadata` → WORK-bead metadata contract (`detached_probe.go`).
- `session_affinity_metadata.go:28` `clearSessionAffinityMetadataOnBead` → called on a convoy **WORK** source bead (`cmd_convoy_dispatch.go:1373`).

### The `mixedFuncs` (the threading priorities, in danger order)

1. **`closeBead`** (`session_beads.go:2282`) — the one true dual-store function. Fix its signature FIRST (Phase 1); everything downstream depends on it.
2. **`reconcileSessionBeadsTracedWithNamedDemand`** (`session_reconciler.go:938`) — ~30 session mutations + ~8 work reads through one `store`. Densest surface.
3. **`buildDesiredStateWithSessionBeads`** (`build_desired_state.go:441`) — session snapshot + every demand work read through one `store`+`bp.beadStore`.
4. **`syncSessionBeadsWithSnapshotAndRigStores`** (`session_beads.go:826`) — session create/update/close + the close-family work guards/release.
5. **`closeSessionBeadIfUnassigned` / `closeSessionBeadIfReachableStoreUnassigned`** (`session_work_guard.go:24/54`) — work-guard + session-close.
6. **`finalizeDrainAckStoppedSession`** (`session_reconciler.go:270`), **`buildPreparedStartWithWorkDirResolver`** (`session_lifecycle_parallel.go:807`), **`rollbackPendingCreate`/`...ClearingClaim`** (`session_lifecycle_parallel.go:2052/2068`), **`emitSessionStrandedDiagnostic`** (`session_reconciler.go:2991`).

---

## 2. AMBIGUOUS-FEDERATED SITES — class-aware resolution + risk

These read through the Router's federation today and need an explicit class decision. **Resolution legend:** S = route session leg to sessionStore, W = keep work leg on work store, B = the function is dual-class and must take BOTH.

| Site | file:line | Resolution | Risk if wrong |
|---|---|---|---|
| `closeSessionBeadIfUnassigned` close-family | `session_beads.go:916, 1028` | **B** — work-guard (`:36`) on work store; `closeBead` session leg on sessionStore, work-release leg on work store | **Mass-closure**: empty session-store guard closes a session holding live work |
| `...RuntimeStoppedAndUnassigned` (duplicate/reconfigured/suspended/orphaned) | `session_beads.go:961, 1573, 1587` | **B** — same split; `:1587` is the classic orphan mass-closure surface | **Mass-closure** (orphan path) |
| `reapStaleSessionBeads` → `closeBead` | `session_beads.go:1916` | **B** — but state=creating only, no claims expected (lower risk) | Work-release runs on empty store → leak (low) |
| `cleanupDeadRuntimeSessionCorpses` → `closeBead` | `session_beads.go:2012` | **B** — **HIGH**: comment says dead session MAY hold in_progress work | **Drops the dead session's work** instead of reopening it |
| `sweepProcessTableOrphans` → `store.Get(live.SessionID)` | `session_beads.go:2162` | **S** — but **caution**: `ErrNotFound` is treated as "confirmed absent → terminate runtime". A misroute to empty session store returns `ErrNotFound` → could **SIGTERM a live runtime** | **Live-runtime kill** (mark caution; verify Get semantics on the relocated store before flipping) |
| `finalizeDrainAckStoppedSession` | `session_reconciler.go:270` (work `:317/:354/:396`, session `:323/:341/:377`) | **B** — work reads on work store; `Get`/`SetMetadataBatch`/`closeSessionBeadIfReachableStoreUnassigned` session leg on sessionStore | **Mass-closure** (drain path); reached from 3 entry points incl. finalize phase `:1127` |
| `reconcileSessionBeadsTracedWithNamedDemand` | `session_reconciler.go:2221` (caller) | **B** — thread sessionStore as a NEW param; ~8 work reads keep `store`/`rigStores` | **Mass-closure** (densest) |
| `reconcileSessionBeadsAtPathWithNamedDemand` | `city_runtime.go:2820` | **B** — control-dispatcher variant (nil assigned/ready snapshots, lighter) | Mass-closure (parallel to `:2221`) |
| `prepareWaitWakeStateForCityWithSnapshot` | `city_runtime.go:2197` / `cmd_wait.go:997` | **B** — session+wait reads/writes on sessionStore; `depsWaitReadyDetailedForCity` (`cmd_wait.go:1091`) dep reads stay on work store | **Mass wait-failure**: empty-store dep reads → `ErrNotFound` → `setWaitTerminalState(failed)` (`:1094`) terminates live waits |
| `dispatchReadyWaitNudgesWithSnapshot` | `city_runtime.go:2251` / `cmd_wait.go:1142` | **B** — wait/session reads+writes on sessionStore; nudge ops already on nudgeStore | Lost wait→nudge dispatch (medium) |
| `buildDesiredStateWithSessionBeads` | `build_desired_state.go:441` / `city_runtime.go:2780` | **B** — session snapshot + creates on sessionStore (via new `bp.sessionStore`); all demand work reads on `store`/`rigStores` | **Mass-closure**: empty session store → zero assigned work → all sessions look orphaned |
| `refreshDesiredStateWithSessionBeads` | `city_runtime.go:569, 580, 1164, 1198` | **B** — session reads on sessionStore, assigned-work cross-ref on work store | Mass-closure (mirror) |
| `runAdoptionBarrier` | `city_runtime.go:480` | **S** (verify) — creates/reads SESSION beads; audit that it does NOT read work before flipping. `adoption_barrier.go:233` Create + `:91/:276` ListAll are session-class | If it reads work → would need B; conservatively audit first |
| `finalizeDrainAckStopPendingSessions` | `city_runtime.go:1127` | **B** — session observe/kill on sessionStore; work read via `finalizeDrainAckStoppedSession` on work store | Mass-closure (drain) |
| `tryDeliverQueuedNudgesByPoller` / `cmdNudgeDrainWithFormat` | `cmd_nudge.go:672, 459` | **B** — split the one work handle: wait-bead Get (`:1437`/`:459`) + session stamp (`:1273`/`:514`) → sessionStore; worker handle + nudge leaf → work/nudge | wait-Get on empty store → nudge dead-lettered; session stamp lost |
| `bead_policy_store.go:278` `policyNameForBead` | `bead_policy_store.go:278` | **classifier seam** — must map BOTH `session.LabelSession`/`BeadType` AND `WaitBeadLabel` → ClassSessions. Not a relocation site; the seam the refactor depends on | Misclassification routes a whole class wrong |
| `api_state.go:311` `registerSessionStoreBackend` | `api_state.go:255, 311` | **the federation seam** — flipping `sessionRelocated(cfg)` is what makes the session store non-empty. Not a call site; the cutover lever | — |

---

## 3. THE PHASED PLAN

**Invariants for every phase:** ≤5 files; ends green (`go build ./cmd/gc/`, `go vet ./...`, the cmd/gc shards per `TESTING.md`); sessions stay Router-routed (relocation still functions) until the FINAL phase. Order is **leaf-most / lowest-blast-radius first → entry points last → cutover**. At the default backend every intermediate phase is byte-identical (`sessionStore == workStore`), so each phase is independently shippable.

> Use `GOCACHE=$(mktemp -d) go build ./cmd/gc/` for cold builds (never `go clean -cache`). Run shards via `make test-cmd-gc-process-parallel` and `make test-fast-parallel`.

### Phase 1 — Make `closeBead` two-store-aware (the load-bearing leaf)
**Why first:** every close path funnels through it; until its signature carries both stores, no caller can be safely split. Byte-identical because callers pass `(store, store)`.

- **Files (4):** `cmd/gc/session_beads.go`, `cmd/gc/session_work_guard.go`, `cmd/gc/session_reconciler.go`, `cmd/gc/session_lifecycle_parallel.go`.
- **Entry point where sessionStore derived:** none yet — this phase only changes signatures. Each of the 7 callers passes the SAME `store` for both params (`closeBead(store, store, ...)`).
- **Functions to thread:** `closeBead` → add `workStore beads.Store` param; session legs (`:2300/:2305/:2307/:2310/:2319`) use the first (session) param, `releaseWorkFromClosedSessionBead` (`:2321`) uses `workStore`. `closeFailedCreateBead` unchanged (pure session, verified). Update all 7 call sites (`session_work_guard.go:47/78`, `session_beads.go:1916/2012/2218`, `session_reconciler.go:2572`, `session_lifecycle_parallel.go:2065`) to `(store, store, ...)`.
- **Leaf session ops repointed:** none functionally yet — this is a pure signature widening that threads `store` twice.
- **Verification:** build + vet + cmd/gc shards green. Add a unit assertion that `closeBead` calls `releaseWorkFromClosedSessionBead` with its `workStore` arg (table test with two distinct fakes) — this is the seed of the Phase-7 guard test.

### Phase 2 — Split the close-family work-guards (`session_work_guard.go` + its two-store callees)
**Why second:** these are the densest, highest-risk mixed leaves and they now have a two-store `closeBead` to call into.

- **Files (3):** `cmd/gc/session_work_guard.go`, `cmd/gc/session_beads.go` (the `closeSessionBeadIf*RuntimeStoppedAndUnassigned` family + `syncSessionBeadsWithSnapshotAndRigStores` close-family delegates only), `cmd/gc/session_reconciler.go` (`closeSessionBeadIfReachableStoreUnassigned` is here per inventory? — it is in `session_work_guard.go:54`; the reconciler holds its CALLERS).
- **Functions to thread:** add `sessionStore beads.Store` param to `closeSessionBeadIfUnassigned` (`:24`) and `closeSessionBeadIfReachableStoreUnassigned` (`:54`). Work-guard reads (`:36 sessionHasOpenAssignedWorkForConfig`, `:67 sessionHasOpenAssignedWorkForReachableStore`) keep `store`/`rigStores`. The `closeBead`/`closeFailedCreateBead` they call now take `(sessionStore, store)`. Thread `sessionStore` through the `session_beads.go` close-family that delegates: `closeSessionBeadIfRuntimeStoppedAndUnassigned` (`:2182`, work guards `:2196/:2207` stay work, `closeFailedCreateBead :2216`/`closeBead :2218` get sessionStore), `retireDuplicate`/`retireRemoved` delegates (`:968/:1549`).
- **Leaf session ops repointed:** the session-close leg of all close-family functions now targets `sessionStore` (still `==store` at default).
- **Verification:** build/vet/shards. Extend the guard test: assert each close-family function's work-guard read and the `releaseWorkFromClosedSessionBead` leg both receive the WORK fake, never the session fake.

### Phase 3 — `session_beads.go` reconciler body (the big two-store function)
**Why:** `syncSessionBeadsWithSnapshotAndRigStores` + the reaper/retire functions are the bulk of session writes; they now have two-store close-family helpers.

- **Files (1):** `cmd/gc/session_beads.go` (large but single-file; ≤5 satisfied).
- **Functions to thread:** add `sessionStore` param to `syncSessionBeadsWithSnapshotAndRigStores` (`:826`), `reapStaleSessionBeads` (`:1828`), `cleanupDeadRuntimeSessionCorpses` (`:1960`), `reapRuntimesBoundToClosedBeads`, `sweepProcessTableOrphans`, `retireDuplicateConfiguredNamedSessionBeads` (`:403`), `retireRemovedConfiguredNamedSessionBead` (`:510`), and the `loadSessionBeads`/snapshot helpers.
- **Leaf session ops → sessionStore:** all `klass=session` sites — `loadSessionBeads`/`ListAllSessionBeads` (`:41`), Create (`:1137`), alias/name guards (`:338/342/1173/1184/1487/1489/1491`), `setMeta`/`setMetaBatch` (`:1767/1778` + every session-context caller), session `store.Update` (`:347/459/529/861`), `store.Get` (`:2096/2162`), wait ops `ReassignWaits`/`CancelWaits`/`ListSessionWaitBeads` (`:746/764/767`), `cancelStateAssignedToRetiredSessionBead` (`:1800/2319`).
- **Work legs UNCHANGED on `store`/`rigStores`:** `unclaimWorkAssignedToRetiredSessionBead` (`:653/689`), `reassignWorkAssignedToRetiredSessionBead` (`:716/730`), `releaseWorkFromClosedSessionBead` (`:2361/2387`), `sessionHasOpenAssignedWorkForConfig` guards. extmsg ops (`:749/752/770`) are mail-class — leave as-is (covered by mail relocation).
- **Verification:** build/vet/shards + the existing Phase-C tier and Ready-leak guards (already landed at `3a6396ee7`).

### Phase 4 — `session_reconciler.go` + `session_wake.go` + `session_lifecycle_parallel.go` (session-write surfaces)
**Why:** the largest session-mutation files; `session_wake.go` is pure-session (zero work reads — verified inventory) so it converts cleanly; the other two are the dense mixed loops with their work chokepoints clearly enumerated in §1c.

- **Files (5):** `cmd/gc/session_reconciler.go`, `cmd/gc/session_wake.go`, `cmd/gc/session_lifecycle_parallel.go`, `cmd/gc/session_reconcile.go`, `cmd/gc/session_sleep.go` (the last two are the sweep-discovered ~17 SetMetadata session writers — `last_woke_at`, `churn_count`, `wake_attempts`, quarantine, `sleep_intent`, `detached_at`, `held_until`).
- **Functions to thread:** add `sessionStore` param down the `reconcileSessionBeadsTracedWithNamedDemand` wrapper chain (`796/835/868/904 → 938`); `finalizeDrainAckStoppedSession` (`:270`), `reconcileDrainAckStopPending` (`:400`), `finalizeDrainAckStopPendingSessions` (`:433`); the start path `executePlannedStartsTraced`/`buildPreparedStartWithWorkDirResolver`/`commitStartResultTraced`/`commitStartFailure`/`rollbackPendingCreate`; all of `session_wake.go` (`preWakeCommit`, `completeDrain`, `verifiedStop`, `verifiedInterrupt`, the two `workerSessionTargetRunningWithConfig` probes — every `store` param becomes `sessionStore`); the session writers in `session_reconcile.go`/`session_sleep.go`.
- **Leaf session ops → sessionStore:** every `klass=session` site (the ~30 `SetMetadataBatch`/heal/circuit-breaker/`Get`/close sites incl. the easy-to-miss `sessionAttachedForConfigDrift → ResolveSessionBeadByExactID` at `session_reconciler.go:3491`).
- **Work legs UNCHANGED:** all §1c chokepoints (`:2738/2746/2768/2776/2804/2853/2882`, `resolveTaskWorkDir :3906`, `resolveTaskOptionOverrides :3962`, the 8 in-loop work reads, `collectSessionAssignedWork`, `firstOpen...`, `clearDetachedProbeMetadata :3062`).
- **Verification:** build/vet/shards. Confirm `reachableStoresForSession` (`:2882`) still receives the work store in tests.

### Phase 5 — `build_desired_state.go` + agentBuildParams (desired-state dual-class)
**Why:** the single largest mixed session/work surface; needs the additive `bp.sessionStore` field.

- **Files (3):** `cmd/gc/build_desired_state.go`, `cmd/gc/agent_build_params.go` (add `sessionStore` field next to `beadStore`; set in `newAgentBuildParams`), `cmd/gc/pool_session_name.go` (the session-existence probes `:405` move to sessionStore; the work landmines `:260/:364` stay on work store — verify both in one file).
- **Functions to thread:** `buildDesiredStateWithSessionBeads` (`:441`) takes `sessionStore`; thread into `collectAllOpenSessionBeads` (`:467`), `discoverSessionBeadsWithRoots` (`:1963` → `bp.sessionStore`), `ensureDependencyOnlyTemplate`/`selectOrCreateDependencyPoolSessionBead`/`createPoolSessionBeadWithGuardedAlias`/`normalizeNonExpandingPoolSessionBead`.
- **Leaf session ops → sessionStore:** `loadSessionBeadSnapshot` (`:430/1975`), `collectAllOpenSessionBeads`/`ListAllSessionBeads` (`:467/961`), all pool/dependency session Create/Update/alias sites (`:2588/2597/3264/3320/3330/3338/3343/3353/3742/3751`).
- **Work legs UNCHANGED:** §1d in full (`:660/1113/1131/1191/1197/1466/1535/3707`, helpers `:1703/1717/1763`, work writers `:3482/3508/3525/3608/3669`).
- **Verification:** build/vet/shards + `make dashboard-check` is NOT required (no `internal/api/` change here).

### Phase 6 — Derive `sessionStore` at the `city_runtime.go` entry points + remaining CLI/sweep writers
**Why last before cutover:** with every callee threaded, the entry points finally derive the real `sessionStore` and pass it down. THIS is where the byte-identical guarantee becomes a real two-store split at the relocated backend.

- **Files (5):** `cmd/gc/city_runtime.go`, `cmd/gc/cmd_wait.go`, `cmd/gc/cmd_nudge.go`, `cmd/gc/cmd_session_wake.go`, `cmd/gc/cmd_stop.go` (+ the small CLI session writers `cmd_session.go`, `cmd_session_pin.go`, `adoption_barrier.go`, `session_name_lookup.go` may need a follow-up micro-phase 6b if 5-file cap is hit — split CLI from controller).
- **Entry points where sessionStore derived (mirror `resolveNudgesStore`):**
  - `beadReconcileTick` `:2091` — `sessionStore := resolveSessionStore(store, cr.cfg, cr.cityPath, cr.rec)`; thread to `:2104/2174/2197/2221/2248/2251` session legs; keep `:2115/2221`-work-args on `store`/`rigStores`.
  - the run()/tick() reaper sequences `:537-580` and `:1075-1216` (incl. `finalizeDrainAckStopPendingSessions :1127`).
  - `controlDispatcherTick` `:2767`; `nudgeDispatchTick` `:2752`; `shutdown` `:3279` (`markCityStopSessionSleepReason` + `gracefulStopAll*`); `reloadConfigTraced :1888`.
  - **Highest-leverage single edit:** change `loadSessionBeadSnapshotWithPartial` (`:2946-2951`) to read from the derived `sessionStore` — verified this is the funnel for nearly every session snapshot read. Add a `cr.sessionBeadStore()` accessor wrapping `resolveSessionStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)` to mirror `cityBeadStore()`/`rigBeadStores()`.
  - CLI entry points derive locally: `cmd_session_wake.go:46`, `cmd_wait.go` (`cmdSessionWait :193`, `cmdWaitSetStateResult :642`), `cmd_nudge.go` (the per-entry openers `:452/623/695/1014/1079/1205`), `cmd_stop.go:351 markCityStopSessionSleepReason`.
- **Work legs UNCHANGED:** `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete :2115`, all rig-store reaps, and every `cmd_*` dep/wait-dep read (`cmd_wait.go:234/270/890/925/935/944/1091`, `cmd_nudge.go` worker observation).
- **Verification:** build/vet/shards. Manual: run a standalone city at the default backend, confirm reconcile tick + wait + nudge all green and byte-identical (no behavior change yet because `sessionStore == workStore`).

### Phase 7 — Cutover: flip `sessionRelocated`, add the work-store-isolation guard test, unregister ClassSessions
**Why last:** correctness at the relocated backend is validated ONLY here. Sessions stop being Router-federated.

- **Files (2-3):** a new `cmd/gc/session_store_routing_test.go` (the guard test), `cmd/gc/api_state.go` (the unregister), and the config/fixture enabling `[beads.classes.sessions].backend`.
- **The guard test (the proof):** construct a two-store harness where the **work fake panics/records on any session-class List/Create/Get** and the **session fake panics/records on any work read (`List{Assignee:...}` returning non-session beads)**. Drive: a reconcile tick over a city with (a) a live session holding in_progress work, (b) an orphaned session, (c) a ready wait, (d) a failed-create rollback. Assert: **no work read ever touches the session fake** (this is the explicit mass-closure guard the task demands), no session op touches the work fake for its session leg, and the live-work session is NOT closed while its work sits on the work store. Add focused assertions for the 7 `closeBead` sites and the `closeSessionBeadIf*` guards.
- **The cutover:** with the guard green at the relocated backend, the Router's ClassSessions registration (`registerSessionStoreBackend`, `api_state.go:255/311`, gated `sessionRelocated :295`) is now exercised by real callers. Validate live: relocate sessions to the SQLite/PG session backend, confirm no mass-closure (the guard + a live smoke). Once proven, the Router for sessions is dead weight — this unblocks the eventual `unregister` / coordrouter deletion (Track-G).
- **Verification:** guard test green at BOTH default and relocated backends; full `make test-cmd-gc-process-parallel`; live smoke on a non-production city before the owner-gated cold migration of maintainer-city.

---

## 4. OPEN QUESTIONS / DECISIONS FOR THE HUMAN OWNER

1. **`sweepProcessTableOrphans` Get-semantics (`session_beads.go:2162`) — CONFIRM BEFORE PHASE 3.** This treats `ErrNotFound` as "confirmed absent → terminate runtime". At the relocated backend, does the session store's `Get` return `ErrNotFound` (→ could SIGTERM a live runtime) or a transient error (→ fail-safe)? If the former, this site needs a fail-closed wrapper (treat session-store `ErrNotFound` as "uncertain, do not terminate") before flipping. **This is the one site where a session-leg misroute kills a live runtime, not just leaks.**
2. **`runAdoptionBarrier` (`city_runtime.go:480`) classification.** Inventory marks it ambiguous. Decision: does adoption read any WORK bead, or only create/read SESSION beads? If session-only, route S; if it reads work, it must take both stores. **Needs a 10-minute audit of `adoption_barrier.go` before Phase 6.**
3. **Rig-leg session beads (`build_desired_state.go:467/961` `collectAllOpenSessionBeads`).** Sessions are city-class today, but the rig legs historically carried per-rig session beads. Confirm sessions are city-only so the rig legs stay on work stores; otherwise the rig session reads also need sessionStore. **Decision gate for Phase 5.**
4. **`bead.*` event emission for relocated session writes.** `api_state.go:299-310` notes the relocated session backend is opened event-silent (nil recorder) and "restoring bead.* emission … is a cutover follow-up." Confirm the generic bead feed / cache observers don't depend on session-row events during the cutover window (session LIFECYCLE events are emitted by the reconciler independently). **Owner decision: ship cutover with silent session-row events, restore in a follow-up?**
5. **Phase-6 file-count overflow.** `city_runtime.go` + 4 `cmd_*` files already hits the cap, and there are ~6 more small CLI session writers (`cmd_session.go:1601`, `cmd_session_pin.go:125`, `adoption_barrier.go:233`, `session_name_lookup.go:211`, `providers.go:221`, `session_index.go:47`). Approve splitting Phase 6 into **6a (controller entry points)** and **6b (CLI/adoption writers)** to honor the ≤5-file rule.

---

## 5. TRACK-G APPENDIX — retire the Router for GRAPH

**Verified premise corrections (from the Track-G surface map):** the graph READ paths do **not** type-assert `*coordrouter.Router`; they all go through the `beads.GraphOnlyReadyFor` / `beads.GraphOnlyListFor` capability interfaces, which the Router merely satisfies. The entire production Router delete surface is **one file** (`cmd/gc/api_state.go`). `internal/storeref` (PrefixOwner + Resolve) is implemented, conformance-pinned to `Router.Get` in `storeref_test.go`, and has **zero** production callers. So Track-G is small and almost entirely already-class-aware.

**Ordered steps (exact anchors from the map):**

- **G1 — Wire `storeref` into the by-id read path (the only real code move).** `internal/storeref/storeref.go` is ready: `PrefixOwner` (`:31`, mirrors `Router.prefixBackendForID`) and `Resolve` (`:51`, mirrors `Router.Get`'s multi-backend body, returns `beads.ErrNotFound`). The Router's by-id federation lives at `internal/coordrouter/router_federation.go:41 (prefixBackendForID) / :57,:61 (Get)` and `router_mutation.go:21 (backendForID)`. Introduce a small explicit-handles store (the post-Router replacement for `coordrouter.New`) that delegates by-id reads/mutations to `storeref.Resolve`/`PrefixOwner` over its `[]beads.Store`. **Model it on `order_dispatch.go:512-562 storesForGate`** — the verified template where the caller already assembles an explicit `[]beads.Store{store, legacyStore, orderStore}` and never touches a Router method. Distinguish from `dispatch.ProcessOptions.ResolveStoreRef` (`runtime.go:55`, set at `cmd_convoy_dispatch.go:205` → `makeStoreRefResolver :324-360`): that is a store-ref-**scheme** resolver (`city:`/`rig:` → whole store via `openStoreAtForCity`), NOT a by-id resolver — leave it unchanged.

- **G2 — Confirm the 2 graph read paths need NO change (they ride interfaces, not the Router).** Verify, then leave as-is:
  - **ready fast-path:** `internal/api/huma_handlers_beads.go:349-350` — `if g, ok := beads.GraphOnlyReadyFor(store); ok { ready, err = g.ReadyGraphOnly() }`. Already interface-mediated; a non-Router store implementing `GraphOnlyReadyStore` (or falling through to `Live.Ready`) is a drop-in.
  - **dispatch scope-check:** `internal/dispatch/runtime.go:441-446` — `if gol, ok := beads.GraphOnlyListFor(store); ok { if pfx := gol.GraphIDPrefix(); ... { return gol.ListGraphOnly(query) } }`. `storeref.PrefixOwner` gives the same prefix routing over an explicit `[]beads.Store`.
  - The other graph-only sites (`build_desired_state.go:1762`, `cmd_ready.go:181`, `session_reconciler.go:2804/2853`, `pool_session_name.go:199`, the `bead_policy_store.go:108/120` forwarding wrapper) are likewise interface drop-ins — no change. The `?type=molecule` augment (`huma_handlers_beads.go:121-134, :153`) is a plain federated `store.List` with a `gc.kind=workflow` filter — **no Router method**, no change.

- **G3 — Delete coordrouter + fold graph into `resolveClassStore`.** With G1 supplying by-id federation and G2 confirming reads are interface-based, excise the Router from the **one** production file:
  - `cmd/gc/api_state.go:25` (import), `:199` (`baseStore.(*coordrouter.Router)`), `:202` (`existingRouter.Backend(ClassWork)`), `:221` (`existingRouter.Register(ClassWork, cs)`), `:250` (`coordrouter.New(workBackend)` — the only constructor), `:311 registerSessionStoreBackend` + `:368 registerGraphStoreBackend` + `:389 registerGraphStoreSQLite` (`r.Register(ClassGraph, ...)` at `:380/392/413`), `:860` (`store.(*coordrouter.Router)`), `:865` (`router.Backends()`).
  - Replace `coordrouter.New` with the G1 explicit-handles store; replace the three `register*StoreBackend` Router-Register calls with the same `resolveClassStore` pattern already used for nudges/mail/sessions (graph folds into `resolveClassStore(workStore, cfg, cityPath, BeadClassGraph, rec)`).
  - Then delete `internal/coordrouter/router*.go`, `stores.go`, `bdgraphstore.go` (keep `coordtest/conformance.go` only if still referenced by surviving tests; retarget `storeref_test.go` off `Router.Get`).
  - **`coordclass` survives** (verified): `internal/storemigrate/migrate.go:80/86/91` (`Classify`/`Classes`/`ClassWork`) reached from `cmd_beads_migrate.go:184` and `cmd_beads_postgres.go:206`. Only `coordclass.ClassifyGraphPlan` is Router-exclusive (callers `router.go:151/167`) and dies with the Router. Do not delete the `coordclass` package.

**Track-G ordering note:** G1→G2→G3 is independent of Track S except that both end by deleting the Router. Land Track S Phase 7 (sessions off the Router) and Track-G G3 (graph off the Router) before the final `coordrouter` package deletion, since `api_state.go:311 registerSessionStoreBackend` and `:380 registerGraphStoreBackend` are both Router-Register calls that must be converted in the same excision.
