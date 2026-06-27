All `bp.sessionStore` write sites confirmed session-class:
- `normalizeNonExpandingPoolSessionBead` operates on a `sessionBead` (guarded by `isManualSessionBeadForAgent`/`isNamedSessionBead`), writing alias/agent_name/labels via `session.UpdatedAliasMetadata` — a session bead, not work.
- `createPoolSessionBeadWithGuardedAlias` calls `createPoolSessionBeadWithAlias` which materializes a SESSION bead. (The `bp.sessionStore == nil` branch at :3338 passes nil — identical to the prior `bp.beadStore` nil-branch, pre-existing and benign.)

The adjudication is complete. Every claim in both reviews is verified against the source. Here is my verdict.

---

# P5 ADJUDICATION — Retire coordrouter.Router via class-aware controller (build_desired_state.go)

**VERDICT: SAFE-TO-PROCEED**

The critical failure mode — a WORK-demand read/write reaching the session store, or a work read's store arg switched to `sessionStore` — **is not present**. Independently verified by reading every cited line plus an inverse-direction grep and a clean build.

## MUST-FIX BLOCKERS

**None.** A blocker requires a concrete work op reaching a session store. No such op exists. The decisive evidence:

1. **`bp.beadStore` has ZERO reads** in build_desired_state.go (grep empty). It is set-only at `agent_build_params.go:123` (`beadStore: store`), retained as the work handle. Every former `bp.beadStore` session op moved to `bp.sessionStore`. There is no work op on either field.
2. **Every work-demand reader/writer receives a WORK store, never `sessionStore`** (targeted grep over all work-op call sites returned empty):
   - `collectAssignedWorkBeadsWithStores(cfg, store, rigStores, ...)` — `build_desired_state.go:670`. Body (`:1080`) builds its store list from the `cityStore` param (= `store`) + `rigStores`; all reads (`listBothTiersForControllerDemand`/`liveReadyForControllerDemandQuery`) go through `source.store`. **This is the landmine read — correctly on work.**
   - `collectOpenUnassignedRoutedWork(cfg, store, rigStores, ...)` — `:702`; body (`:3700`) reads `listBothTiersForControllerDemand(source.store)` from `store`+`rigStores`.
   - `stampRunSessionIdentity(... assignedWorkStores ...)` `:689` → writes `store := workStores[i]` (`:3474`), `store.SetMetadataBatch`; `stampRunRootFromStep` (`:3518`) takes the work `store`.
   - `canonicalizeLegacyBoundAssignedWork(... assignedWorkStores ...)` `:694` and `canonicalizeLegacyBoundUnassignedRoutedWork(... unassignedRoutedStores ...)` `:703` → write `store := workStores[i]`, `store.Update` (`:3626`, `:3687`).
   - `defaultScaleCheckTargetForAgent(..., store, rigStores)` `:575/:610` → targets built with `cityStore=store`/`rigStore`; `defaultScaleCheckCounts` reads `group.store` (`:1480`).
   - `listBothTiersForControllerDemand`/`readyForControllerDemandQuery`/`liveReadyForControllerDemandQuery` (`:1715/:1728/:1775`) take a generic `store` param, always invoked with `source.store`/`group.store` from work-store collections.
3. **`sessionStore` appears at exactly two session-class read sites**, both Q3-correct:
   - `collectAllOpenSessionBeads(cfg, sessionStore, rigStores, ...)` `:477` — body (`:939`) consumes arg 2 as the **city leg** (`session.ListAllSessionBeads`) and `rigStores` as separate rig legs. Sessions are city-only; rig legs stay on work. Session enumeration, not work demand.
   - `loadSessionBeadSnapshot(bp.sessionStore)` `:1990` (in `discoverSessionBeadsWithRoots`, guarded `bp.sessionStore != nil`) — pure session enumeration.
4. **`bp.sessionStore` writes are all session-bead ops**: `normalizeNonExpandingPoolSessionBead` (`:2605/:2614`, guarded to a session bead), `recordDeferredNonExpandingPoolAliasConflict` (`:3281`), `createPoolSessionBeadWithGuardedAlias` (`:3338/:3348/:3361/:3371`, materialize SESSION beads), `ensureDependencyOnlyTemplate` nil-guard (`:2201`). None touch work beads.
5. **Build passes**: `GOCACHE=$(mktemp -d) go build ./cmd/gc/` → `BUILD_EXIT=0`. No build-breaking miss.
6. **Byte-identity holds at default bd**: all 6 production `buildDesiredStateWithSessionBeads` call sites pass the same handle to both store slots with real rigStores in the work slot — `cmd_supervisor.go:2498` `(store, store, rigStores)`, `cmd_start.go:53/893/909/949` `(store/oneShotStore, same, rigStores)`, `city_runtime.go:2787` `(store, store, cr.rigBeadStores())`. `newAgentBuildParams` at `session_reconciler.go:2690` passes `(store, store)`. The 4 `refreshDesiredStateWithSessionBeads` calls (`city_runtime.go:569/580/1171/1205`) pass `cr.cityBeadStore()` to the single session param — refresh drives only the session-class overlay (`:1027` doc + `:1054` `(sessionStore, sessionStore)`), so the work slot is moot there. Since `sessionStore == workStore` at default bd, every work read still hits the work store.

## WARNINGS (non-blocking)

- **W1 (session leak — benign, the inverse of the failure mode).** Three SESSION-class lookups still read `bp.beadStore` instead of `bp.sessionStore`, outside the P5-touched functions: `findSessionNameByTemplate(p.beadStore, ...)` at `session_name_lookup.go:343`, `session.ExactMetadataSessionCandidates(p.beadStore, ...)` and the template-resolve `p.beadStore` passes at `template_resolve.go:250,366`. These are session-bead *reads* left on the work store — a note, not a blocker (no work op reaches a session store). Byte-identical at default bd (`beadStore == sessionStore`). **Must be migrated to `bp.sessionStore` in P6 before the session class is relocated**, or these session lookups will read the work store after the split. (Confirmed via the second reviewer's claim; the field exists and is still populated, so they resolve correctly today.)
- **W2 (pre-existing, cosmetic).** `createPoolSessionBeadWithGuardedAlias:3338` passes `bp.sessionStore` inside the `bp.sessionStore == nil` branch (i.e. passes nil). Identical to the prior `bp.beadStore` nil-branch — benign and pre-existing; the legacy no-bead path tolerates a nil store.

## Scope note
This P5 phase is store-routing only (signatures + the `bp.sessionStore` field + routing); no behavioral logic changed. `bp.beadStore` is now dead-for-reads inside build_desired_state.go but is the live work handle elsewhere and the bead-backed-mode gate — leave it. The Router-retirement risk owner should track W1 as a required P6 follow-up.

Relevant file: `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/build_desired_state.go` (and `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/agent_build_params.go`).
