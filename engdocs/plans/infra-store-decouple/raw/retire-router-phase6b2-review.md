The entire stop chain is conclusively pure-session:
- `stopTargetsForNames` reads via `loadSessionBeads(sessionStore)` (the gc:session list — session class). The rest is in-memory config resolution.
- `hydrateStopTargets` only re-calls `stopTargetsForNames`.
- `stopTargetThroughWorkerBoundary` calls `cityStopSessionMarked`/`markCityStopSessionAsAsleep` (single session-bead Get/SetMetadataBatch) and `workerKill/StopSessionTargetWithConfig` (session runtime-handle teardown — `targetID` is a session ID/name, not a work bead).
- `cityStopSessionMarked` / `markCityStopSessionAsAsleep` are single session-bead ops.

No work-assignment read and no wait-dependency read reaches a session store anywhere in the stop chain. The mass-closure failure mode (#2) is avoided.

I now have complete verification of both failure modes across all cited code. Let me record my final adjudication findings.

## Adjudication: P6b-2 (CLI session/wait store routing)

I read the cited code directly in cmd_wait.go, class_store.go, cmd_nudge.go, cmd_session_wake.go, cmd_stop.go, controller.go, city_runtime.go, session_lifecycle_parallel.go, and cmd_sling.go, and grep-verified the dep-read and stamp inventories across all non-test cmd/gc files.

### MUST-FIX blockers
**NONE.**

Both critical failure modes are provably avoided:

- **Failure mode #1 (wait-failure landmine — a wait-dependency read reaching a session store): NOT PRESENT.** `depsWaitReadyDetailedForCity` (cmd_wait.go:898) and `loadWaitDependencyBead` (cmd_wait.go:945) are signature-unchanged and still take the work store. A grep over all non-test cmd/gc files returns exactly four dep-read call sites, every one on the work store: cmd_wait.go:239 (`store`), :276 (`store`), :915 (internal `store` param), and :1126 (`workStore`). The `ErrNotFound → setWaitTerminalState(failed)` terminal branches (cmd_wait.go:277-287 and :1127-1141) are therefore fed by a work-store dep read and cannot be tripped by an empty session store. The reconciler mixer `prepareWaitWakeStateForCityWithSnapshot` (cmd_wait.go:1029) has the claimed signature `(cityPath, sessionStore, workStore, nudgeStore, now, sessionBeads)` — session first — and the controller call at city_runtime.go:2204 passes `(cr.cityPath, sessionStore, store, resolveNudgesStore(...), ...)`, i.e. the work `store` into the workStore slot.

- **Failure mode #2 (mass-closure — a work-assignment read reaching a session store): NOT PRESENT.** Every session-store consumer reads only SESSION/WAIT-class beads: gc:session lists (`loadSessionBeads`), gc:wait label lists/`Get`s (IsWaitBead-guarded), session-bead `Get`/`SetMetadataBatch`/`Close`, and session runtime-handle teardown keyed by session ID/name. The full `gracefulStopAll` chain (controller.go:975-1100) and its helpers in session_lifecycle_parallel.go (stopTargetsForNames, hydrateStopTargets, stopTargetThroughWorkerBoundary, cityStopSessionMarked, markCityStopSessionAsAsleep, interruptTargetsBoundedWithForceSignal) are genuinely pure-session; the running-probe `workerSessionTargetRunningWithConfig("", nil, sp, nil, name)` calls at controller.go:1033/1062 pass a nil store. The nudge MIXED functions split correctly: `tryDeliverQueuedNudgesByPoller` (cmd_nudge.go:1230) puts the worker handle on `workStore` and the wait-validator (`splitQueuedNudgesForDelivery`→`blockedQueuedNudgeReason`, which reads only `item.Reference.ID`, the durable wait bead) + last-nudge stamp on `sessionStore`; the live caller at city_runtime.go:2768 passes `(cr.sessionBeadStore(), store, nudgeStore)`.

- **No build-breaking miss.** `go build ./cmd/gc/` and `go vet ./cmd/gc/` both exit 0 — any stale-arity caller of a renamed/split function would have failed compilation. Default-backend byte-identity is structural: `resolveClassStore` (class_store.go:158-175) returns `workStore` verbatim when `cfg==nil` or `NormalizedClassBackend(class)==BeadsBackendBD`, so `resolveSessionStore`/`sessionBeadStore()` == the work store at default bd.

### Warnings (benign leaks — session-class op left on the work store; invisible to byte-identity tests; not blockers)

1. **cmd_nudge.go:525** (`cmdNudgeDrainWithFormat`, non-inject path) — `stampLastNudgeDeliveredAt(workStore, target.sessionID, time.Now())`. `stampLastNudgeDeliveredAt` is pure-session (writes `MetadataLastNudgeDeliveredAt` to a session ID, cmd_nudge.go:1320) and `sessionStore` is already in scope (derived at :457). Its inject-path twin at :518 correctly uses `sessionStore`. At a relocated sessions backend this best-effort stamp lands on the wrong store; harmless (no failure, no mass-closure, no wait-termination) but inconsistent. Fix: change `workStore` → `sessionStore` at :525 to match :518.

2. **cmd_sling.go:1485** (`deliverSlingNudge`) — `stampLastNudgeDeliveredAt(store, target.sessionID, time.Now())`, the same pure-session stamp left on the work store. Note: cmd_sling.go is NOT in the P6b-2 file list (work-slinging is a work-assignment seam, out of scope for this phase), so this is a pre-existing session-class-op-on-work-store rather than a P6b-2 regression. Flagging for the eventual sessions-backend cutover; it should be routed to a `sessionStore` when the sling seam is converted. Not a blocker (benign leak, and out of this phase's scope).

Both warnings are the exact "benign leak, note not blocker" category the rubric calls out: a session-class write on the work store is invisible to byte-identity tests (sessionStore==workStore at default bd) and causes no work read or wait-dep read to reach a session store.

### VERDICT: SAFE-TO-PROCEED

No work read or write, and no wait-dependency read, reaches a session store anywhere in the P6b-2 surface. Both failure modes are structurally avoided, build and vet are clean, and default-backend byte-identity holds at the resolver. The only findings are two benign last-nudge-stamp leaks (cmd_nudge.go:525 in-scope; cmd_sling.go:1485 out-of-scope/pre-existing), neither of which gates this phase. Recommend landing the cmd_nudge.go:525 one-line fix opportunistically but it does not block.
