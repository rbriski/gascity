All session-write helpers (`markDrainAckStopPending`, `silentRebaselineSessionHashes`) receive `sessionStore` — session writes go to the session store. `resolveTaskWorkDir`/`resolveTaskOptionOverrides` (pure-work, work `store` param) have no call site inside the loop body here; they are passed as resolvers built from `assignedWorkBeads` snapshots, not from sessionStore. The work reads at :3926/:3971 operate on the work store only.

I have independently verified every load-bearing claim across all three reviews by reading the cited code. The adjudication is complete.

---

## P4c ADJUDICATION — cmd/gc/session_reconciler.go (highest-risk file)

### MUST-FIX BLOCKERS

**None.** I searched exhaustively for the catastrophic failure mode (any WORK-assignment read/write reaching `sessionStore`) and found zero instances:

- **No work-read method is ever called on `sessionStore`.** Every `sessionStore.*` call in the file is one of exactly 9 session-class ops: `sessionStore.Get(session.ID)` ×1 (:342, fetches the session bead) and `sessionStore.SetMetadata[Batch](session.ID, ...)` ×8 (:378, :1744, :2062, :2088, :2219, :2293, :2486, :3043) — all keyed on `session.ID`/`target.session.ID`, verified line-by-line. `grep -E "sessionStore\.(List|Ready|CachedList|ListGraphOnly|ReadyGraphOnly|ReadyLive|ListByAssignee|ReadyForAssignee|Update|Create)"` returns nothing.
- **Every work-assignment read passes the work `store`.** All 10 call sites of `sessionHasOpenAssignedWorkForReachableStore`/`sessionHasAwakeAssignedWorkForReachableStore`/`sessionHasOpenAssignedWorkForConfig`/`sessionHasInProgressAssignedWorkForConfig`/`firstOpenAssignedWorkBeadForReachableStore`/`collectSessionAssignedWork`/`recordDrainAckAssignedWorkEvent` (:247, :318, :355, :397, :1332, :1391, :1515, :1673, :2010, :2180, :2547, :3020) pass `store` in the work position.
- **The mass-close gate is safe.** The pool-free close at :2580 (`closeBead(sessionStore, store, ...)`) fires only when `!hasAssignedWork`, and `hasAssignedWork` at :2547 reads `sessionHasOpenAssignedWorkForReachableStore(..., store, ...)` (work store). The drain-ack close gate (`finalizeDrainAckStoppedSession` :318/:355) and `closeSessionBeadIfReachableStoreUnassigned` (session_work_guard.go:78) both drive the unassigned decision from the WORK store; closes target `sessionStore`. A relocated empty session backend therefore cannot report "no work" and mass-close a live session.
- **No direct work read in the ~1700-line loop body** (945-2635): zero `.List`/`.Ready*`/`.CachedList` calls; all funnel through the unchanged single-store helper family. No shadowing of `store` or `sessionStore` in the loop.
- **All 5 pure-work `store.List` / 7 pure-work `store.SetMetadataBatch(session.ID)` sites** live in helpers whose call sites pass the correct leg: work-list helpers (:2959, :3273, :3926, :3971) are only reached with the work `store`; session-write helpers (`markDrainAckStopPending`, `silentRebaselineSessionHashes`) are called with `sessionStore` (:1357, :1590, :1880, :2072, :2129).
- **Pure-work signatures UNCHANGED**: the entire `sessionHas*AssignedWork*` family, `reachableStoresForSession` (:2890), `graphOnlyHas*` (:2796/:2835), `firstOpenAssignedWorkBead*` (:2924/:2944), `collectSessionAssignedWork` (:3114), `recordDrainAckAssignedWorkEvent` (:232), `resolveTaskWorkDir/OptionOverrides` each still take a single `store beads.Store`. No pure-work caller was switched to `sessionStore`.
- **Build clean**: `GOCACHE=$(mktemp -d) go build ./cmd/gc/` exits 0 — every threaded arity (including the wrappers at :902/:939 passing `store, store`, and the entry points in city_runtime.go:1127/:2222 passing `cityBeadStore(), cityBeadStore()` / `store, store`) matches.

### WARNINGS

- **(nit, not blocking)** `closeBead` (session_beads.go:2398) routes the extmsg cancel via `cancelStateAssignedToRetiredSessionBead(sessionStore, workStore, ...)`; its own comment notes the wait-cancel is session-class and the extmsg cancel is work-class, and that the internal split lands in a later phase (byte-identical now since stores are identical). Not a P4c concern — flagged only so the later split isn't forgotten.
- **(nit)** New `sessionStore == nil` guards at :285 and :450 widen the existing fail-safe (`store == nil`) — defensive, no behavior change.
- **(observation, no action)** All session-write helpers correctly receive `sessionStore`. The only theoretical leak direction left is a session write landing on the work store, which does not occur here — every session write is routed to `sessionStore`. (A session op left on the work store would be benign per the contract anyway.)

### VERDICT: **SAFE-TO-PROCEED**

All three reviews are corroborated by direct code reading. The contract (session FIRST in two-store fns; pure-session helpers single param; pure-work fns unchanged; byte-identical at `sessionStore == workStore`) is satisfied with no work op reaching a session store, no pure-work signature/caller changed, and a clean build. The catastrophic mass-close/work-drop failure mode is structurally absent.

Adjudicated file: `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_reconciler.go` (supporting: `cmd/gc/session_work_guard.go`, `cmd/gc/session_beads.go`, `cmd/gc/session_lifecycle_parallel.go`, `cmd/gc/session_reconcile.go`, `cmd/gc/session_sleep.go`, `cmd/gc/session_circuit_breaker.go`, `cmd/gc/controller.go`, `cmd/gc/city_runtime.go`, `cmd/gc/session_reconciler_test.go`).
