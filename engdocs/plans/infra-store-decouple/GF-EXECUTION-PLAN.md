---
title: "Phase GF — coordrouter deletion: gate review + execution plan"
date: 2026-06-27
branch: plan/decouple-infra-beads
status: BLOCKERS-FIRST — convert 3 residual Router read-deps, then the mechanical deletion
gate_review: agent ac0bff7176955470b (pre-GF gate)
---

## Verdict: BLOCKERS-FIRST
Tree builds/vets green. Only **5 production `coordrouter` lines remain — all in `cmd/gc/api_state.go`**
(routedPolicyStore, wrapWithCachingStore assertion, registerGraphStoreBackend/SQLite,
closeBeadStoreHandle). Consumer conversion (GA–GE + GD-b) is done EXCEPT three residual graph-READ
deps that still rely on the Router federating work∪graph and would see work-only post-cutover.

## The 3 residual Router read-deps (convert BEFORE deletion) — phase "GD-c"
1. **Convergence (BLOCKER):** `cmd/gc/convergence_store.go` (`convergenceStoreAdapter`), wired at
   `convergence_tick.go:80,87` via `cr.cityBeadStore()`; `internal/convergence/handler.go:953,976`.
   Convergence roots are `type=convergence` → ClassGraph (created on the graph store by GD-a) but
   the adapter's List/Get/SetMetadata/Close/index/`FindByIdempotencyKey` read the work store →
   invisible post-cutover (loop never ticks, dup loops). **Fix:** feed `cr.graphBeadStore()` (the
   SINGLE city-scope graph handle — same for every rig scope, keyed on cityPath) into the adapter.
2. **`collectInputConvoyWorkflowRoots` (BLOCKER):** `cmd/gc/wisp_autoclose.go:156-190` —
   `convoycore.TrackingConvoysForItem(store,…)` + `store.ListByMetadata({gc.input_convoy_id})` on the
   work store; the synthetic tracking convoys + graph.v2 roots are graph-resident → empty list →
   root-only wisps never reaped → review-pool churn. **Fix:** thread the graph store (federate
   `[store, graph]`) into the discovery; `doWispAutocloseWith` already computes `graph`/`storeSet`.
3. **Dispatch find-path by-id (BLOCKER):** `findBeadAcrossStores` + `runControlDispatcherInStore`'s
   `store.Get(beadID)` (cmd_convoy_dispatch.go ~159,536,553) resolve a graph control bead via the
   Router today. **Fix:** resolve via `storeref.Resolve(id, [graph, work])` (like `beadStoresForID`).

All three are byte-identical at graph=bd (resolveGraphStore→work) and with the Router present at
graph=sqlite (same `.gc/beads.sqlite` handle). TierBoth lesson applies to any new graph List/Ready.

## GF mechanical edit list (after GD-c) — confirmed against current code
- `api_state.go`: `routedPolicyStore` → `wrapStoreWithBeadPolicies(workBackend, cfg, cityPath)` always
  (drop coordrouter.New + registerGraphStoreBackend); remove the `*coordrouter.Router` assertion in
  `wrapWithCachingStore` (existingRouter swap → `workBackend=baseStore`, finish→routedPolicyStore(cs,…));
  remove the `*coordrouter.Router` peel in `closeBeadStoreHandle`; delete
  `registerGraphStoreBackend`/`registerGraphStoreSQLite` (KEEP `openGraphSQLiteStore`/`resolveGraphStore`/
  `graphStoreHandleCache`/`noCloseSQLiteStore`/`graphStoreIDPrefix`); drop the `coordrouter` import.
- `cmd_convoy_dispatch.go`: `controlStoreWithGraphRouting` → `policy(work)` (GE already gives the
  dispatcher its graph primary; the find-paths are fixed in GD-c #3).
- **KEEP `coordclass.ClassifyGraphPlan`** — used by `bead_policy_store.go` (GD-a). The old plan doc's
  "delete ClassifyGraphPlan" is **STALE**. All of `coordclass` survives.
- **RELOCATE `internal/coordrouter/coordtest/`** out of the doomed package (it is the SQLite/PG graph
  conformance gate; replace `coordrouter.GraphStore`/`WorkStore` aliases with
  `beads.GraphApplyStore`/`beads.Store`). Then `rm` the Router files (router*.go, stores.go,
  bdgraphstore.go + their tests).
- `internal/storeref/storeref_test.go`: delete `TestResolve_MatchesRouterGet`; keep the rest.
- Retarget ~12 test files that build `coordrouter.New(work)` fixtures → `wrapStoreWithBeadPolicies(work,
  graphClassSQLiteCfg(), cityPath)` + a real `openGraphSQLiteStore` handle. INVERT the premise in
  `api_state_router_test.go` (22 refs) + `session_router_test.go` ("city store is a Router" → "policy(work)
  + distinct graph store"). `internal/beads/{sqlite,postgres}_store_*conformance_test.go` retarget the
  `coordtest` import + the GraphStore alias.

## Conformance gaps to close as the GF gate
1. Relocate `coordtest` (biggest structural prereq).
2. New GF-gating residence test through the REAL post-GF `policy(work)`+graph store: create→by-id→
   drain→cleanup all land physically on `.gc/beads.sqlite`; byte-identical at graph=bd.
3. Convergence visibility conformance (pins BLOCKER-1).

## Cosmetic followups (do NOT gate GF)
doctor/status/store-health under-count; two-store wait/extmsg test; PG read-after-write; cmd_sling.go:1485 stamp.
