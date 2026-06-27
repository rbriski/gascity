Confirmed: production wires `newControllerState` (controller.go:1333, cmd_supervisor.go:1956) → the API `s.state` is `*controllerState` whose `BeadStores()` injects `cityName`. The blocker fires in the live controller's `/v0/beads/ready`.

I have exhaustively verified both blockers against the cited code, confirmed live exposure (graph=sqlite + 6 rigs on maintainer-city), confirmed the build is clean, and identified that the huma blocker is actually broader than either review stated. I also corrected both reviews' errors. Let me compose the final adjudication.

---

# G1 ADJUDICATION — Retiring coordrouter.Router for GRAPH (Ready callers)

**VERDICT: FIX-FIRST.** Two MUST-FIX blockers. The rewire is NOT byte-equivalent to `GraphOnlyReadyFor(...).ReadyGraphOnly()` at `graph_store=sqlite` (LIVE on maintainer-city, 6 rigs). graph=bd byte-identity and `gc ready` are clean. I read every cited path; the disagreement between the two reviews resolves in favor of review 2's verdict (has-blockers), but **both reviews are partly wrong** and the huma blocker is broader than either stated.

## Root cause (shared by both blockers)
Under `graph_store=sqlite`, **both the city store AND every rig store** are `policy(Router(workCache + sharedGraphSQLite))` — rig stores get routed via `buildStores`→`wrapWithCachingStore`→`finish()`→`routedPolicyStore` because `graphRelocated(cfg)==true` (api_state.go:251-258, 223). `coordrouter.Router` has **no `Handles()` method**, so `HandlesFor(Router).Live.Ready()` dispatches through `logicalLiveStoreReader` to **`Router.Ready` = the FEDERATED work∪graph union** (router_federation.go:147-158, 23-29: two backends ⇒ no `soleBackend` fast path). The OLD code never called `Live.Ready` on a routed store; it called `GraphOnlyReadyFor(store).ReadyGraphOnly()` → `Router.ReadyGraphOnly` → `graph.Ready()` **ALONE** (router_federation.go:167-173). The rewire dropped that probe and substituted `Live.Ready`, silently flipping graph-only → federated on every routed leg whose `graphStore` arg is nil.

## MUST-FIX BLOCKERS

### Blocker 1 — `/v0/beads/ready` leaks WORK ready beads on the city leg AND every rig leg
**`internal/api/huma_handlers_beads.go:372-397`** (with `cmd/gc/api_state.go:1158-1163`, `internal/api/handler_beads.go:317-337`)

- `controllerState.BeadStores()` **injects `m[cs.cityName]=cs.cityBeadStore`** (api_state.go:1161-1163) — verified. `sortedRigNames` dedups by *store identity*, and `cityBeadStore` is a distinct object, so `cityName` **survives in `rigNames`**. The new in-line comment at line 380 ("The city store is NOT among the per-rig BeadStores()") is **factually wrong for `controllerState`**.
- The rig loop runs `federate("rig "+rigName, stores[rigName])` for `rigName==cityName` AND for each real rig. NEW `federate` (line 372-379) calls `beads.HandlesFor(store).Live.Ready()`:
  - **cityName leg**: `HandlesFor(cityBeadStore).Live.Ready()` → `beadPolicyLiveReader` → `HandlesFor(Router).Live.Ready()` → `Router.Ready` = work(gc-N)∪graph(gcg-N). gcg-N dedup against the explicit graph leg via `seen`; **gc-N WORK beads are net-new and leak**.
  - **each real rig leg** (graph=sqlite ⇒ rig store is also `policy(Router(...))`): OLD `federate` probed `GraphOnlyReadyFor(rigStore)=true` → graph-only (deduped to ~0); NEW reads `Router.Ready` → **rig-WORK ready beads leak too.** (This is broader than review 2's "only cityName"; and it directly refutes review 1's claim that rig stores are "plain bd, never Routers.")
- **Net**: OLD `/v0/beads/ready` = graph ready only (the documented worker-readiness contract). NEW = graph ∪ city-work ∪ every-rig-work. The worker/dispatcher readiness hot loop changes live.
- **Why the guard test misses it**: `TestBeadReadyGraphOnlyExcludesWorkLegUnderSQLite` (bead_http_graph_store_test.go:255-293) sets `state.stores=nil` and `fakeState.BeadStores()` returns `f.stores` **raw** (fake_state_test.go:92) with no `cityName` injection and no rig store — it models neither leak.

**Fix**: When `graphRelocated`, do not re-federate the city store via `rigNames`, and read the rig legs the way the Router did — i.e. read the shared graph store once and skip per-rig `Live.Ready` work reads (or, if rigs must now contribute work-ready, justify it explicitly). Concretely: skip any `stores[rigName]` whose identity `== cityStore`, and for relocated-graph make rig legs class-aware (graph-only) instead of `Live.Ready`. Add a federation test using a real-shaped `BeadStores()` (cityName entry + ≥1 rig work store) asserting no `gc-N`/rig-work bead appears.

### Blocker 2 — controller-demand wake gate reads each RIG WORK store instead of the city GRAPH store
**`cmd/gc/build_desired_state.go:1096,1102,1208,1214` + `liveReadyForControllerDemandQuery` at `:1781-1788`**

- `graphStore` is set **only on the city leg** (`stores := []workStore{{store: cityStore, graphStore: graphStore}}`, :1096); rig legs append with `graphStore` nil (:1102).
- In the Ready pass, `liveReadyForControllerDemandQuery(source.store, source.graphStore=nil, query)` takes the `graphStore==nil` branch → `HandlesFor(rigStore).Live.Ready(query)` → `Router.Ready` = **rigWork∪graph**. OLD `liveReadyForControllerDemandQuery(rigStore, query)` ran `GraphOnlyReadyFor(rigStore)` (true, rig store is routed under sqlite) → `Router.ReadyGraphOnly` → **graph.Ready ALONE (gcg-N)**.
- **Delta**: assigned, deps-ready **rig-WORK** beads now enter `readyAssignedIDs` (`appendAssignedUnique` keeps `Assignee!=""`; per-leg `seen`, results concatenated at :1168-1171 ⇒ net-new). The `in_progress`/`open` passes already federate in both versions (`listBothTiersForControllerDemand`→`Router.List`), so the delta is narrowed to assigned deps-ready work not yet open/in_progress — **but it is still a live change**: `readyAssignedIDs` gates the open-assigned named/on-demand wake at `:831` and reachability via `assignedWorkStoreRefs` at `:841` (rig-scoped agents — polecat/refinery/witness). An `open` assigned rig-work bead can flip from "not woken" (OLD) to "woken" (NEW).
- No test covers `collectAssignedWorkBeadsWithStores` with a rig store under graph=sqlite (`build_desired_state_graph_ready_test.go` unit-tests `liveReadyForControllerDemandQuery` in isolation).

**Fix**: Set `graphStore` on **every** `workStore` entry (rig legs too), not just the city leg — the graph store is a single city-scope shared handle, so a relocated-graph city then reads graph-only on rig legs and the per-tick result equals the prior `Router.ReadyGraphOnly` union. Add a `collectAssignedWorkBeadsWithStores` test with ≥1 rig store + relocated graph asserting `readyAssignedIDs`/result match pre-rewire.

## WARNINGS (not blockers)
- **Both reviews contain errors.** Review 1 ("clean") is **wrong**: it asserts huma rig legs read `Live.Ready` "in BOTH" and that rig stores are "plain bd, never Routers" — false under graph=sqlite (rig stores are `routedPolicyStore`), and it entirely missed `cityName` being inside `rigNames`. Review 2 ("has-blockers") reaches the right verdict but understates Blocker 1 (it's not only the `cityName` leg — every rig leg in huma also leaks) and its stated rig-store wrap-chain ("openRigStore->wrapWithCachingStore->routedPolicyStore") is imprecise: the routing actually comes from `wrapWithCachingStore`'s `finish()` calling `routedPolicyStore`, not `openRigStore` (which uses plain `wrapStoreWithBeadPolicies`).
- The in-line comment at `huma_handlers_beads.go:380` must be corrected once the fix lands (it is currently false for the controller).

## CLEAN / VERIFIED-EQUIVALENT
- **graph=bd byte-identity** at all 3 sites: `resolveGraphStore` returns `workStore` when `!graphRelocated` (class_store.go:283-284); rewired branches skipped; `graphStore==store` everywhere. ✓
- **`gc ready`** (`cmd/gc/cmd_ready.go:187-196`): single store, no rig federation; inline `if TierMode==TierIssues {=TierBoth}` is byte-identical to `expandPolicyReadyQuery` (bead_policy_store.go:419-421); `Assignee`/`Limit` preserved; `resolveGraphStore` returns the same cached `openGraphSQLiteStore` handle. Equivalent. ✓
- **Graph-store identity**: `resolveGraphStore`/`GraphBeadStore()` and the Router's `registerGraphStoreSQLite` both resolve the dir-cached `openGraphSQLiteStore(cityPath)` shared handle (class_store.go:231-256, api_state.go:360-363). ✓
- **TierMode**: where the rewire does fire, the TierMode handling matches (`TierBoth` forced/expanded as the policy forwarder did) — so the blockers are **store-routing** regressions, not TierMode mismatches. The TierMode hazard called out in the task is clean.
- **The 3 List sites** (session_reconciler / pool_session_name / dispatch.runtime) are correctly **untouched**; their graph-only branch is dead today (`beadPolicyStore` forwards `ReadyGraphOnlyHandle` but no `ListGraphOnly` handle).
- `go build ./cmd/gc ./internal/api ./internal/coordrouter` ⇒ exit 0.

**Live exposure confirmed**: `/data/projects/maintainer-city/city.toml` has `graph_store = "sqlite"` (line 968) and 6 rigs (gascity, beads, gastown, gasworks-gui, gascity-dashboard, registry). Both blockers fire on the live controller, whose `s.state` is `*controllerState` (controller.go:1333). Full trace: `/data/projects/gascity/.claude/worktrees/infra-store-plan/engdocs/plans/infra-store-decouple/raw/g1-adjudication-trace.md`.
