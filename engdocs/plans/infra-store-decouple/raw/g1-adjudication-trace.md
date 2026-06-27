# G1 adjudication trace (verified against working-tree diff @ HEAD 4d7641378)

Diff under review = uncommitted working tree (not yet committed). graph=sqlite LIVE on maintainer-city + 6 rigs.

## Store wiring (graph=sqlite)
- cityBeadStore = policy(Router(workCache + sharedGraphSQLite))  [api_state.go:148 wrapWithCachingStore -> finish() -> routedPolicyStore, graphRelocated=true]
- rigStores[name] = policy(Router(rigWorkCache + sharedGraphSQLite))  [buildStores:431 wrapWithCachingStore -> finish:223 routedPolicyStore; graph shared via cityPath]
- GraphBeadStore() == resolveGraphStore(city) == registerGraphStoreSQLite's openGraphSQLiteStore(cityPath) shared handle. SAME object.
- Router has NO Handles() method -> HandlesFor(Router).Live = logicalLiveStoreReader -> .Ready() calls Router.Ready (FEDERATED work∪graph).
- beadPolicyStore HAS Handles() (bead_policy_store.go:94) -> beadPolicyLiveReader -> Router.Ready(expandPolicyReadyQuery).
- Router.Ready (2 backends) = federateRead union work+graph. Router.ReadyGraphOnly = graph.Ready ALONE.

## BLOCKER 1 — huma /v0/beads/ready cityName leg
- controllerState.BeadStores() injects m[cityName]=cityBeadStore (api_state.go:1158-1163). CONFIRMED.
- sortedRigNames dedups by STORE IDENTITY; cityBeadStore distinct from rig stores -> cityName SURVIVES in rigNames.
- rig loop: federate("rig "+cityName, cityBeadStore).
  - OLD federate body: GraphOnlyReadyFor(cityBeadStore)=true -> g.ReadyGraphOnly() -> Router.ReadyGraphOnly -> graph.Ready (gcg-N). Already in `seen` from explicit city leg -> 0 net.
  - NEW federate body: HandlesFor(cityBeadStore).Live.Ready() -> Router.Ready -> work∪graph. gcg-N deduped; gc-N WORK beads NET-NEW -> leak into worker readiness set.
- Guard test TestBeadReadyGraphOnlyExcludesWorkLegUnderSQLite sets state.stores=nil + fakeState.BeadStores returns raw f.stores (no cityName inject) -> never models the leak.
- FIX: when graphRelocated, skip the cityName entry in the rig loop (skip stores whose identity == cityStore).

## BLOCKER 2 — build_desired_state rig legs (collectAssignedWorkBeadsWithStores)
- graphStore set only on city leg (:1096); rig legs graphStore=nil (:1102).
- rig stores = policy(Router(rigWork+sharedGraph)) under graph=sqlite.
  - OLD liveReadyForControllerDemandQuery(rigStore, query): GraphOnlyReadyFor(rigStore)=true -> Router.ReadyGraphOnly -> graph.Ready (gcg-N ONLY).
  - NEW liveReadyForControllerDemandQuery(rigStore, nil, query): graphStore==nil -> HandlesFor(rigStore).Live.Ready -> Router.Ready -> rigWork∪graph.
- Delta = assigned deps-ready rig-WORK beads (gc-/rig-prefix) now in readyAssignedIDs (appendAssignedUnique filters Assignee!="" ; per-leg seen so net-new).
- in_progress/open passes (listBothTiersForControllerDemand) already federate in BOTH (List on policy store -> Router.List) so delta limited to deps-ready not-yet-open/in_progress assigned work.
- readyAssignedIDs gates open-assigned named/on-demand wake (:831) + reachability (assignedWorkStoreRefs :841). rig-scoped agents affected.
- FIX: set graphStore on EVERY workStore entry (rig legs too) so rig legs read graph-only like the old Router.ReadyGraphOnly.

## Verified EQUIVALENT (no blocker)
- graph=bd: resolveGraphStore returns workStore; all 3 rewired branches skipped; byte-identical.
- cmd_ready.go: single store, TierIssues->TierBoth inline == expandPolicyReadyQuery; Assignee/Limit preserved; no rig federation.
- huma genuine rig legs (non-cityName): rig stores route List/Ready via Router in BOTH; for Live.Ready -> Router.Ready in NEW vs OLD GraphOnlyReadyFor(rig)... WAIT rig IS routedPolicyStore under sqlite so OLD rig leg in huma ALSO read graph-only. See note.

## huma genuine rig legs re-check (graph=sqlite)
- rig store = policy(Router(rigWork+sharedGraph)). OLD huma federate(rig): GraphOnlyReadyFor(rigStore)=true -> graph.Ready (gcg-N, deduped vs city graph leg -> ~0 net). NEW huma federate(rig): Router.Ready -> rigWork∪graph -> rigWork NET-NEW.
- => huma BLOCKER is BROADER than just cityName: EVERY rig leg also leaks rig-work ready in NEW (same root cause: NEW federate dropped the GraphOnlyReadyFor probe). Both city AND rig legs regressed in huma.

Build: go build ./cmd/gc ./internal/api ./internal/coordrouter => EXIT 0.
