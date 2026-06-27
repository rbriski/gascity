# Track-G: Retire `coordrouter.Router` for the GRAPH class — implementation design

Branch `plan/decouple-infra-beads`. Verified against current code. All file:line anchors are absolute under `/data/projects/gascity/.claude/worktrees/infra-store-plan`.

## 0. The load-bearing facts that shape the design (verified, some correct against the recon, some sharpening it)

1. **The Router is the ONLY production importer of `coordrouter`** (`cmd/gc/api_state.go:25`). `internal/beads/codectest/codectest.go` is a test helper. Confirmed: `grep -rln internal/coordrouter --include='*.go' | grep -v _test` → `api_state.go` + `codectest`.

2. **The Router does FAR more than by-id routing.** `router_federation.go` federates `List/Ready/Children/ListByLabel/ListByAssignee/ListByMetadata/DepList` across *both* backends (union+dedup+sort), and exposes `ReadyGraphOnly`/`ListGraphOnly`/`GraphIDPrefix` (`router_federation.go:147-200`). `router_mutation.go` routes `Update/Close/Reopen/Delete/SetMetadata/SetMetadataBatch/DepAdd/DepRemove/CloseAll/ReleaseIfCurrent/Claim/Count` by `backendForID` (prefix-owner then Get-probe). `router.go:112-179` routes `Create`/`CreateWithStorage`/`ApplyGraphPlan`/`ApplyGraphPlanWithStorage` by `coordclass.Classify`/`ClassifyGraphPlan`.

3. **CRITICAL CORRECTION to the recon's G1 framing.** Only the *Router* implements `ReadyGraphOnly`/`ListGraphOnly`/`GraphIDPrefix`. A bare `*beads.SQLiteStore` does **not** — it implements plain `Ready`/`List`. The Router's graph-only methods are literally `r.Backend(ClassGraph).Ready(...)` / `.Live.List(...)`. So `resolveGraphStore(...).Ready()` (graph store's plain `Ready`) is **byte-identical** to `Router.ReadyGraphOnly()` when graph is relocated, and at `graph=bd` `resolveGraphStore` returns the work store so `.Ready()` is the full work `Ready` — exactly the Router's identity-phase fallback (`router_federation.go:167-173`). This is why G1's "rewire to `resolveGraphStore().Ready()/.List()`" is correct and equivalence-provable.

4. **The graph CREATE/APPLY + by-id surface is 100% store-mediated.** ZERO callers type-assert `*coordrouter.Router`. Every graph create/apply/by-id op flows through a `store beads.Store` that is `policy(Router(work+graph))` today, built at exactly **two wrapper functions**:
   - `routedPolicyStore` (`api_state.go:252`) — feeds `openStoreResultAtForCity` (`main.go:1207,1242`) and the caching-store builder (`api_state.go:224`).
   - `controlStoreWithGraphRouting` (`cmd_convoy_dispatch.go:457`) — feeds `openControlStoreAtForCity` → `dispatch.ProcessControl`.

5. **`dispatch.ProcessControl(store beads.Store, ...)` and `molecule.Instantiate(ctx, store, ...)` take ONE `beads.Store`** and route the whole graph class *internally* (by-id Get/Update/SetMetadata + `liveListForRoot`→`GraphOnlyListFor` + `GraphApplyFor(store)`). They cannot be handed an explicit `[]beads.Store` without invasive signature churn across `internal/dispatch`, `internal/molecule`, `internal/sling`. **Therefore the faithful, minimal, ≤5-files-per-phase replacement keeps the single-`beads.Store` shape and collapses the Router into a focused class-aware wrapper** — a `graphRoutedStore` that holds `[work, graph]` and routes graph-class ops via `internal/storeref` + `coordclass.Classify`, mirroring the Router op-for-op. This is the lowest-churn target the by-id recon itself names ("the lowest-churn drop-in is to keep handing it ONE store whose by-id methods federate over [work, graph] internally — a thin storeref-backed beads.Store replacing the Router").

6. **The data-orphan landmine is real.** Live graph SQLite is at `<cityPath>/.gc/beads.sqlite` (`registerGraphStoreSQLite`, `api_state.go:358-383`, via `graphStoreHandleCache` `:320`, prefix `gcg`, retention `(0,0)`). `resolveClassStore`/`openClassSQLiteStore` use `.gc/<class>/` = `.gc/graph/` (`class_store.go:95-96`, `cmd_beads_migrate.go:26`). **`resolveGraphStore` MUST replicate `registerGraphStoreSQLite`'s legacy `.gc/` path and reuse `graphStoreHandleCache` — never `openClassSQLiteStore`.** Postgres uses `openClassPostgresStore(cfg, cityPath, BeadClassGraph, nil)` (`api_state.go:348`), which IS the canonical class path and is safe to share.

7. **`storeref` is ready and dark** (`internal/storeref/storeref.go`): `PrefixOwner` mirrors `prefixBackendForID`, `Resolve` mirrors `Router.Get`, conformance-pinned by `storeref_test.go:TestResolve_MatchesRouterGet`. Zero production callers.

8. **`coordclass` survives** via `internal/storemigrate` (`Classify`/`Classes`/`ClassWork`). Only `ClassifyGraphPlan` is Router-exclusive (`router.go:151,167`) and dies with the Router.

---

## The end-state shape (what replaces the Router)

The Router is replaced by **one new fork-owned wrapper type** `graphRoutedStore` plus the `resolveGraphStore` resolver and two thin accessors. `graphRoutedStore` is the Router minus the multi-class generality — it knows exactly two backends (work + graph) and routes by `coordclass.Classify` (create/apply) and `internal/storeref.PrefixOwner` (by-id), satisfying the same capability set the policy wrapper and callers probe for (`GraphApplyHandleProvider`, `GraphOnlyReadyStore`, `GraphOnlyListStore`, `ConditionalAssignmentReleaser`, `Claimer`, `Counter`, `StorageCreateStore`). It is the single object that lets `molecule.Instantiate` / `dispatch.ProcessControl` / `sling` / the API handlers stay on one `beads.Store` while graph-class ops reach the legacy SQLite store.

```
BEFORE:  policy( Router{ work: cache(dolt), graph: sqlite(.gc/beads.sqlite) } )
AFTER:   policy( graphRoutedStore{ work: cache(dolt), graph: resolveGraphStore(...) } )
                 ^ same beads.Store surface, graph ops routed by classify/storeref, no coordrouter import
```

Two consumer shapes, exactly as today:
- **By-id federation (API handlers)** target end-state per the recon = `beadStoresForID` returns `[work, graph]` and the existing handler loops federate via `storeref`. This is independent of `graphRoutedStore` and is done in **G2** for the explicit-slice sites.
- **Single-store callers (dispatch/molecule/sling)** keep one store = `graphRoutedStore`, built inside the two wrappers.

---

## Phase 0 — `resolveGraphStore` + accessors (1 file: `cmd/gc/class_store.go`; +1 trivial: `api_state.go` doc)

**Why first / leaf:** every later phase depends on this resolver. It writes no call-site, changes no routing — byte-identical everywhere because nothing calls it yet.

Add to `cmd/gc/class_store.go` (it already houses `resolveSessionStore`/`resolveNudgesStore` and imports `beads`/`config`/`events`):

```go
// resolveGraphStore returns the beads.Store backing the GRAPH coordination class.
// It is the dedicated, class-aware successor to registerGraphStoreBackend +
// coordrouter.Router's ClassGraph leg. It MUST NOT route through resolveClassStore /
// openClassSQLiteStore: the SQLite graph store lives at the LEGACY <cityPath>/.gc/
// (citylayout.RuntimeRoot, file beads.sqlite), NOT the .gc/<class>/ class-store
// convention, so the live graph_store=sqlite city is never pointed at an empty
// .gc/graph/ and its graph data is never orphaned.
//
//   graph=bd       -> the work store (graphRelocated false; byte-identical default)
//   graph=sqlite   -> the cached embedded SQLite store at .gc/beads.sqlite (gcg, retention 0,0)
//   graph=postgres -> openClassPostgresStore(cfg, cityPath, BeadClassGraph, nil) (gcg schema)
//
// rec is accepted for signature parity with the other resolve*Store helpers but is
// intentionally IGNORED: the graph store stays event-silent (matching the prior
// registerGraphStoreSQLite/registerGraphStoreBackend opens, which passed no recorder)
// because the formula-v2 topology is high-churn and is not mirrored to the bus.
func resolveGraphStore(workStore beads.Store, cfg *config.City, cityPath string, _ events.Recorder) beads.Store {
	if !graphRelocated(cfg) {
		return workStore
	}
	switch cfg.Beads.NormalizedClassBackend(config.BeadClassGraph) {
	case config.BeadsBackendSQLite:
		if s, ok := openGraphSQLiteStore(cityPath); ok {
			return s
		}
		return workStore
	case config.BeadsBackendPostgres:
		if s, ok := openClassPostgresStore(cfg, cityPath, config.BeadClassGraph, nil); ok {
			return s
		}
		return workStore
	default:
		return workStore
	}
}
```

Move the legacy-location opener out of `registerGraphStoreSQLite` into a Router-free helper (also in `class_store.go`, beside `openClassSQLiteStore`), reusing `graphStoreHandleCache`/`noCloseSQLiteStore`/`graphStoreIDPrefix` verbatim — this is a *cut-and-rename* of `api_state.go:358-383`'s body, NOT a reimplementation:

```go
// openGraphSQLiteStore opens (or returns the cached) embedded SQLite graph store at
// the LEGACY <cityPath>/.gc/beads.sqlite location — distinct from the .gc/<class>/
// class-store convention (openClassSQLiteStore) — so it stays byte-identical for
// cities already on graph_store="sqlite". Mirrors the prior registerGraphStoreSQLite.
func openGraphSQLiteStore(cityPath string) (beads.Store, bool) {
	dir := filepath.Join(cityPath, citylayout.RuntimeRoot)
	if cached, ok := graphStoreHandleCache.Load(dir); ok {
		return cached.(beads.Store), true
	}
	store, err := beads.OpenSQLiteStore(dir,
		beads.WithSQLiteStoreRetention(0, 0),
		beads.WithSQLiteStoreIDPrefix(graphStoreIDPrefix))
	if err != nil {
		log.Printf("beads: graph_store=sqlite requested but opening the SQLite graph store at %s failed: %v; graph beads stay on the work backend", dir, err)
		return nil, false
	}
	shared := store
	if sq, ok := store.(*beads.SQLiteStore); ok {
		shared = noCloseSQLiteStore{sq}
	}
	if actual, loaded := graphStoreHandleCache.LoadOrStore(dir, shared); loaded {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore() //nolint:errcheck
		}
		shared = actual.(beads.Store)
	}
	return shared, true
}
```

`class_store.go` gains imports `citylayout`, `filepath`, `log`. **`registerGraphStoreSQLite` (`api_state.go:358`) becomes a one-line shim** `r.Register(coordclass.ClassGraph, mustGraphStore)` calling `openGraphSQLiteStore` — keep it functioning so api_state.go stays green this phase; it is deleted in G3.

Add the two accessors mirroring `SessionsBeadStore` exactly:
- Controller — `cmd/gc/city_runtime.go` beside `sessionBeadStore()` (`city_runtime.go:2948`):
  ```go
  func (cr *CityRuntime) graphBeadStore() beads.Store {
  	return resolveGraphStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)
  }
  ```
  And the `controllerState`/`api.State` mirror in `cmd/gc/api_state.go:1309` neighborhood:
  ```go
  func (cs *controllerState) GraphBeadStore() beads.Store {
  	cs.mu.RLock(); defer cs.mu.RUnlock()
  	return resolveGraphStore(cs.cityBeadStore, cs.cfg, cs.cityPath, cs.eventProv)
  }
  ```
- Add `GraphBeadStore() beads.Store` to the `api.State` interface (`internal/api/state.go:119` neighborhood) with a doc comment mirroring `SessionsBeadStore`'s. **Update every `api.State` test double / fake** (grep `SessionsBeadStore()` in `internal/api/*_test.go` and `cmd/gc/*_test.go` fakes → add the parallel `GraphBeadStore()` returning the same store) — this is a CI-surfaced compile gate, so the grep is exhaustive by construction.

**Verification (P0):** `go build ./...` + `go vet ./...`. No behavior change (`resolveGraphStore`/accessors uncalled on hot paths; `registerGraphStoreSQLite` shim still drives live routing). `graph=bd`: `resolveGraphStore` returns work store — byte-identical. **Landmine guard:** `openGraphSQLiteStore` uses `citylayout.RuntimeRoot` + `graphStoreHandleCache`; assert in a unit test that its dir == `registerGraphStoreSQLite`'s dir and the handle is the SAME cached object (so live `.gc/beads.sqlite` is reused, never `.gc/graph/`).

---

## Phase G1 — rewire the ~7 graph-only READ callers to the dedicated graph store (≤5 files)

These currently get the policy-wrapped Router and probe `GraphOnlyReadyFor`/`GraphOnlyListFor`. Re-home each to the dedicated graph store via the new accessor (controller/API) or `resolveGraphStore` (CLI), calling plain `.Ready()`/`.List()`.

**Equivalence proof (per site):** when relocated, `Router.ReadyGraphOnly()` ≡ `graph.Ready()` and `Router.ListGraphOnly(q)` ≡ `graph.Live.List(q)` (`router_federation.go:167-187`); `resolveGraphStore` returns that same graph store. At `graph=bd`, `resolveGraphStore` returns the work store and the Router's graph-only methods fall back to full `Ready`/`List` on the work store (`:169`,`:183`) — identical.

Sites (from recon read-path JSON; the bd_policy forwarding wrapper is intentionally KEPT):
1. **`internal/api/huma_handlers_beads.go:330-368`** (`/v0/beads/ready` fast-path). The federate loop runs per-store over `BeadStores()`. Change: stop probing `GraphOnlyReadyFor(store)` per rig store; instead, when `graphRelocated`, read the single `s.state.GraphBeadStore().Ready()` ONCE for the graph leg and read each work/rig store's `Live.Ready()` for the work leg (deduping by id via the existing `seen` map). Because the live graph store is a single city-scope store shared by all rigs, iterating `GraphOnlyReadyFor` per rig today returns the SAME graph set N times (deduped) — reading it once is equivalent and strictly fewer SQLite scans. File 1.
2. **`internal/dispatch/runtime.go:435-447`** (`liveListForRoot`). This is generic dispatch code threading one `store`. It stays as-is — but its `store` is now `graphRoutedStore`, which (G3) implements `GraphOnlyListFor` (delegating to the graph leg). **No change in G1**; covered by G3's wrapper satisfying the capability. (Documented here so the site isn't "missed".)
3. **`cmd/gc/build_desired_state.go:1762-1766`** (controller-demand readiness). Replace `GraphOnlyReadyFor(store)` probe with `cr.graphBeadStore().Ready()` for the graph leg + work `Live.Ready()`. File 2.
4. **`cmd/gc/cmd_ready.go:181-183`** (CLI `gc ready`). Replace probe with `resolveGraphStore(store, cfg, cityPath, nil).Ready()`. File 3.
5. **`cmd/gc/session_reconciler.go:2804,2853`** (graph-only awake probes). Replace with `cr.graphBeadStore()` `.List()`/`.Ready()`. File 4.
6. **`cmd/gc/pool_session_name.go:199-201`** (orphan-release strand heal, uses `GraphIDPrefix()`). Replace with `storeref.PrefixOwner` over `[work, cr.graphBeadStore()]` (or read the prefix const `graphStoreIDPrefix` directly when relocated). File 5.
7. **`cmd/gc/bead_policy_store.go:108-121`** (`ReadyGraphOnlyHandle` forwarding) — **KEEP unchanged** (recon item 8: store-agnostic, survives). It forwards through whatever inner store offers the capability; after G3 the inner is `graphRoutedStore`.

If files 3-6 push past 5, split CLI sites (cmd_ready/session_reconciler/pool_session_name) into a G1b batch — each is independent and byte-identical at `graph=bd`.

**Verification (G1):** unit/integration on `/v0/beads/ready` and `gc ready` under `graph=sqlite` (set membership identical to pre-change) and `graph=bd` (byte-identical). `go vet`. The Router still exists (G1 only moves READS off the *capability probe* to the *direct accessor*; the Router is still built and still serves create/apply/by-id).

---

## Phase G2 — the by-id explicit-slice sites: `beadStoresForID` returns `[work, graph]` (2 files)

Per the by-id recon: the API handler loop bodies (`huma_handlers_beads.go:512-940`, 11 sites) are **already storeref-shaped** (probe `Get`, `continue` on `ErrNotFound`, mutate). The ONLY change is what `beadStoresForID` returns for a `gcg-` id.

**File 1 — `internal/api/handler_beads.go:165-186` (`beadStoresForID`).** Add a class-prefix arm BEFORE the legacy candidate scan: when the id carries `graphStoreIDPrefix` (`"gcg"`), return `storeref`-ordered `[workOwner, graphStore]`. Concretely:
```go
func (s *Server) beadStoresForID(id string) []beads.Store {
	id = strings.TrimSpace(id)
	if store := s.resolveStoreByConfiguredIDPrefix(id); store != nil {
		return []beads.Store{store}
	}
	if prefix := beadPrefix(id); prefix != "" {
		if store := s.resolveStoreByPrefix(prefix); store != nil {
			return []beads.Store{store}
		}
	}
	// Class-prefix arm: a graph-class id (gcg-) is owned by the dedicated graph
	// store, which is NOT a rig/HQ-prefixed store and would otherwise fall through
	// to the candidate scan and miss. Return [work, graph] so the existing per-store
	// Get-then-mutate loop federates exactly as coordrouter.Router did.
	if graph := s.state.GraphBeadStore(); graph != nil {
		if p, ok := graph.(interface{ IDPrefix() string }); ok && p.IDPrefix() != "" && strings.HasPrefix(id, p.IDPrefix()+"-") {
			work := s.state.CityBeadStore()
			return []beads.Store{work, graph} // graph owns gcg-; work first preserves prior probe order for the work leg
		}
	}
	// ... existing candidate scan unchanged ...
}
```
At `graph=bd`, `GraphBeadStore()` == `CityBeadStore()` so the arm's prefix check (`gcg`) never fires for a default city (work ids are `gc`/`ga`/rig-prefix), and even if it did, `[work, work]` dedups in the loop → byte-identical. **Add a guard:** if `graph == s.state.CityBeadStore()` (i.e. not relocated), skip the arm entirely so default cities take the identical legacy path.

**Semantic guards preserved (no handler-body edits needed, but verify each):**
- `humaHandleBeadAssign` / `humaHandleBeadUpdate` (`:734-842`): "Get-hit pins the store; subsequent Update-ErrNotFound = 409 concurrent-delete, NOT try-next" (`:748-755`). With `[work, graph]` a `gcg-` bead is found only in `graph` (prefix-disjoint), so the pin lands on `graph` deterministically — preserved. **Do not** convert these to a re-probing `storeref.Resolve` on the mutation.
- `humaHandleBeadReleaseIfCurrent` (`:845-881`) / `humaHandleBeadClaim` (`:883-916`): capability asserted on the RESOLVED leaf (`SQLiteStore` implements both `ConditionalAssignmentReleaser` and `Claimer`). **Update the two comments** that say "through the Router" → "on the resolved graph store". The Router-only `EnvActorClaimer` bridge (`router_mutation.go:143`) is moot — irrelevant once the graph leaf resolves directly.
- `humaHandleBeadDeps` (`:584-612`) / `humaHandleBeadGraph` (`:512-540`): parent `Get` and child `List`/subtree must stay on the SAME resolved store. With `[work, graph]` the loop resolves the parent's store and runs the List against it — preserved (children of a `gcg-` root are graph-resident).

**File 2 — `cmd/gc/cmd_bd_shim.go`** (by-id recon item 1): **NO CHANGE.** The shim is pure-HTTP (`ga-2gap48`, `cmd_bd_shim.go:31-35,1051-1062`); `bd close gcg-N` lands on the server handlers above. Add a one-line test/assertion that the shim has no in-process Router fallback after retirement (the contract is preserved by absence — a `grep -L coordrouter cmd/gc/cmd_bd_shim.go`).

**Verification (G2):** under `graph=sqlite`, exercise `GET/POST /v0/bead/{gcg-N}/{get,close,reopen,assign,update,deps,release-if-current,claim,delete}` end-to-end and assert each lands on the graph SQLite store; assert assign/update 409-on-concurrent-delete still fires; assert `graph=bd` byte-identical (arm skipped). The Router still exists for the single-store dispatch/molecule path until G3.

---

## Phase G3 — delete the Router; introduce `graphRoutedStore`; rework `api_state.go:193-258` and `controlStoreWithGraphRouting` (≤5 files, split into G3a/G3b)

This is the cutover. Order the sub-steps so the tree compiles at each.

### G3a — add `graphRoutedStore` (new file: `cmd/gc/graph_routed_store.go`), repoint the two wrappers, drop the Router constructor

`graphRoutedStore` is the Router's two-backend specialization, built on `internal/storeref`. It embeds the work store (so the full `beads.Store` surface delegates to work for free, exactly like `Router{ beads.Store }`) and holds the graph store; it overrides precisely the methods the Router overrode, routing graph-class ops:

```go
type graphRoutedStore struct {
	beads.Store              // work backend (primary delegate, like Router.Store)
	graph        beads.Store // dedicated graph store (legacy .gc/beads.sqlite or gcg PG)
}

func newGraphRoutedStore(work, graph beads.Store) beads.Store {
	if graph == nil || graph == work {
		return work // identity phase: byte-identical, no wrapper
	}
	return &graphRoutedStore{Store: work, graph: graph}
}

func (g *graphRoutedStore) stores() []beads.Store { return []beads.Store{g.Store, g.graph} }

// --- create/apply: route by class (mirrors router.go) ---
func (g *graphRoutedStore) Create(b beads.Bead) (beads.Bead, error) {
	if coordclass.Classify(b) == coordclass.ClassGraph { return g.graph.Create(b) }
	return g.Store.Create(b)
}
func (g *graphRoutedStore) CreateWithStorage(b beads.Bead, sc beads.StorageClass) (beads.Bead, error) { /* same split, StorageCreateStore assert on leg */ }

// --- by-id: route via storeref.PrefixOwner over [work, graph] (mirrors backendForID) ---
func (g *graphRoutedStore) ownerForID(id string) beads.Store {
	if o := storeref.PrefixOwner(id, g.stores()); o != nil {
		if _, err := o.Get(id); err == nil { return o }
	}
	for _, s := range g.stores() { if _, err := s.Get(id); err == nil { return s } }
	return g.Store
}
func (g *graphRoutedStore) Update(id string, o beads.UpdateOpts) error { return g.ownerForID(id).Update(id, o) }
func (g *graphRoutedStore) Close(id string) error  { return g.ownerForID(id).Close(id) }
// ... Reopen/Delete/SetMetadata/SetMetadataBatch/DepAdd/DepRemove identical pattern ...
func (g *graphRoutedStore) Get(id string) (beads.Bead, error) { return storeref.Resolve(id, g.stores()) }

// --- federated reads: union over [work, graph] (mirrors federateRead) ---
func (g *graphRoutedStore) List(q beads.ListQuery) ([]beads.Bead, error) { /* union+dedup+sort, q.Sort/q.Limit */ }
func (g *graphRoutedStore) Ready(q ...beads.ReadyQuery) ([]beads.Bead, error) { /* union+dedup, SortCreatedAsc */ }
// ... Children/ListByLabel/ListByAssignee/ListByMetadata/ListOpen/DepList ...

// --- graph-only capability: the graph leg ALONE (mirrors router_federation.go:167-200) ---
func (g *graphRoutedStore) ReadyGraphOnly(q ...beads.ReadyQuery) ([]beads.Bead, error) { return g.graph.Ready(q...) }
func (g *graphRoutedStore) ListGraphOnly(q beads.ListQuery) ([]beads.Bead, error) { return beads.HandlesFor(g.graph).Live.List(q) }
func (g *graphRoutedStore) GraphIDPrefix() string { if p, ok := g.graph.(interface{ IDPrefix() string }); ok { return p.IDPrefix() }; return "" }

// --- capabilities the policy wrapper / handlers probe ---
func (g *graphRoutedStore) GraphApplyHandle() (beads.GraphApplyStore, bool) { /* applier that routes by ClassifyGraphPlan==Graph ? graph : work — but with only 2 backends, simplifies to: graph plan -> graph applier */ }
func (g *graphRoutedStore) ReleaseIfCurrent(id, exp string) (bool, error) { /* assert on ownerForID(id) */ }
func (g *graphRoutedStore) Claim(id, a string) (beads.Bead, bool, error) { /* assert Claimer on ownerForID(id) */ }
func (g *graphRoutedStore) Count(...) (int, error) { return 0, beads.ErrCountUnsupported } // split: callers fall back to List, like Router
func (g *graphRoutedStore) Backends() []beads.Store { return g.stores() } // for closeBeadStoreHandle

var ( _ beads.Store = (*graphRoutedStore)(nil); _ beads.GraphApplyHandleProvider = (*graphRoutedStore)(nil)
      _ beads.GraphOnlyReadyStore = (*graphRoutedStore)(nil); _ beads.GraphOnlyListStore = (*graphRoutedStore)(nil)
      _ beads.ConditionalAssignmentReleaser = (*graphRoutedStore)(nil); _ beads.Claimer = (*graphRoutedStore)(nil) )
```

**`GraphApplyHandle` simplification (kills `ClassifyGraphPlan`):** the recon notes `ClassifyGraphPlan`'s only callers are the Router. With exactly two backends, a graph-apply plan that reaches the apply path is by construction a graph plan (the policy wrapper only exposes `ApplyGraphPlan` when `GraphApplyFor(inner)` succeeds, and the create/apply recon confirms every pour is graph-classed). So `graphRoutedStore.GraphApplyHandle` returns an applier that calls `GraphApplyFor(g.graph)` — no `ClassifyGraphPlan`. (A defensive `coordclass.Classify` on the plan's root node, NOT `ClassifyGraphPlan`, can guard the work-fallback; either way `ClassifyGraphPlan` is unreferenced and deleted with the Router.)

Repoint the two wrappers to build `graphRoutedStore` instead of the Router:

- **`routedPolicyStore` (`api_state.go:252-259`)** becomes:
  ```go
  func routedPolicyStore(workBackend beads.Store, cfg *config.City, cityPath string) beads.Store {
  	if !graphRelocated(cfg) {
  		return wrapStoreWithBeadPolicies(workBackend, cfg)
  	}
  	graph := resolveGraphStore(workBackend, cfg, cityPath, nil) // legacy .gc/beads.sqlite or gcg PG
  	return wrapStoreWithBeadPolicies(newGraphRoutedStore(workBackend, graph), cfg)
  }
  ```
  Delete `registerGraphStoreBackend` (`api_state.go:337-352`) and `registerGraphStoreSQLite` (`:358-383`) — their logic now lives in `resolveGraphStore`/`openGraphSQLiteStore`. `coordrouter.New` (`:256`) is gone.

- **`controlStoreWithGraphRouting` (`cmd_convoy_dispatch.go:457-462`)** is unchanged in body (`return routedPolicyStore(store, cfg, cityPath)`) — it now produces `policy(graphRoutedStore)`. **But correct the comment** (`:447-456`): drop "inserts the per-class Router", say "wraps the bd control store in a graph-routed store". The `controlBdStoreForCity`/`-ForRig` input is the work leg; `resolveGraphStore` provides the graph leg at the legacy location — landmine guarded.

### G3b — the `api_state.go:193-258` caching-store rework (the subtle one)

Today (`api_state.go:192-235`): `wrapWithCachingStore` type-asserts `existingRouter, _ := baseStore.(*coordrouter.Router)` (`:199`); if present, caches only `existingRouter.Backend(ClassWork)` (`:202`) and on `finish()` does `existingRouter.Register(ClassWork, cs)` (`:221`) to swap the work backend to the cache in place, keeping the graph backend OUTSIDE the cache.

**Why the graph backend must stay outside the cache:** `CachingStore` reconciles its single backing via a `bd` subprocess; the graph SQLite store has no `bd` path, and many short-lived processes share `.gc/beads.sqlite`, so caching it would serve stale graph reads cross-process. The recon confirms `registerGraphStoreSQLite` is never wrapped by `CachingStore`.

**Post-Router rework** — replace the `*coordrouter.Router` assertion with a `*graphRoutedStore` assertion and swap the work leg by reconstruction (no `Register`, since `graphRoutedStore` is immutable-by-construction):

```go
existingGraph, _ := baseStore.(*graphRoutedStore)
workBackend := baseStore
if existingGraph != nil {
	workBackend = existingGraph.Store // the work leg only; graph leg stays uncached
}
cs := beads.NewCachingStore(workBackend, onChange)
...
finish := func() beads.Store {
	if !policyWrapped { return cs }
	if existingGraph != nil {
		// Rebuild the graph-routed store with the CACHED work leg; the graph leg
		// (legacy .gc/beads.sqlite) is reused as-is, never cached.
		return wrapStoreWithBeadPolicies(newGraphRoutedStore(cs, existingGraph.graph), policyStore.cfg)
	}
	return routedPolicyStore(cs, policyStore.cfg, cityPath)
}
```
This is behavior-preserving: before, `Register(ClassWork, cs)` mutated the existing Router's work map entry to `cs` while keeping the same graph backend; now we construct a new `graphRoutedStore{Store: cs, graph: existingGraph.graph}` with the identical legs. The graph leg pointer is reused (same cached `noCloseSQLiteStore` from `graphStoreHandleCache`) — no second open, no orphan. **Update the comment block `:192-198`** to say "graphRoutedStore" not "Router".

`closeBeadStoreHandle` (`api_state.go:822-850`): replace the `*coordrouter.Router` arm (`:829-840`) with a `*graphRoutedStore` arm calling `.Backends()` (the method exists on the wrapper) — identical peel-to-each-backend logic so the `CachingStore` reconciler on the work leg is still `StopReconciler`'d. The graph leg is the shared no-close handle (its `CloseStore` is a no-op), so peeling it is safe.

### G3c — delete the package + retarget the test (separate commit, after G3a/G3b are green)

- Remove `import ".../coordrouter"` and `".../coordclass"` from `api_state.go:24-25` **iff** no longer referenced there (after G3a, `coordclass.ClassWork/ClassGraph/ClassSessions` in api_state.go are gone with the Register calls; `coordclass` may still be imported by `class_store.go`/`graph_routed_store.go` for `Classify` — keep those).
- `rm -rf internal/coordrouter/` (package + all `router*_test.go`, `stores.go`, `bdgraphstore.go`, `coordtest/`). Confirm no remaining importers: `grep -rln internal/coordrouter --include='*.go'` → only `internal/beads/codectest/codectest.go` (a test helper) may reference it; check and retarget/remove that reference.
- **`internal/storeref/storeref_test.go`**: `TestResolve_MatchesRouterGet` (`:87-142`) imports `coordrouter` and pins `Resolve` to `Router.Get`. Per its own doc comment (`:83-86`: "When the Router is deleted, this differential test is deleted with it; storeref.go is untouched"), **delete `TestResolve_MatchesRouterGet`** and drop the `coordrouter`/`coordclass` imports. `TestPrefixOwner` and `TestResolve_FederationFallback` stay (they pin the contract without the Router). The new G3 conformance test (below) replaces the lost coverage with a `graphRoutedStore`-vs-legacy-SQLite differential.
- `coordclass.ClassifyGraphPlan` is now unreferenced — delete it from `internal/coordclass/classify.go:88-105` and its test in `classify_test.go`. `Classify`/`Classes`/`ClassWork` survive (used by `graph_routed_store.go` + `storemigrate`).

**Verification (G3):** full `go build ./...`, `go vet ./...`, `make test-fast-parallel`, the `cmd/gc` process shard, and the dispatch/molecule/sling integration tests under both `graph=sqlite` and `graph=bd`. The existing Router behavioral coverage (`router_byid_test.go`, `router_federation_test.go`, `router_storage_test.go`, `router_claim_test.go`) is the spec — port its assertions to `graph_routed_store_test.go` against `graphRoutedStore` so nothing is lost.

---

## Phase G4 — the relocated-graph CONFORMANCE test (1 file: `cmd/gc/graph_routed_store_conformance_test.go`)

A single integration test, `graph_store="sqlite"`, on a temp city, asserting the four contract points the task requires plus the landmine:

- **(a) graph-plan apply lands on `.gc/beads.sqlite`.** Pour a graph.v2 formula via `molecule.Instantiate` through `routedPolicyStore`'s output; assert the root/step beads carry the `gcg-` prefix and are readable by a *bare* `beads.OpenSQLiteStore(<cityPath>/.gc, WithSQLiteStoreIDPrefix("gcg"))` handle, and that the Dolt/work store has zero `gcg-` beads. Also exercise the **sequential fallback** (graph-apply disabled): assert `store.Create` of a `type=molecule`/`wisp` bead also lands on `.gc/beads.sqlite` (covers the create/apply recon's "orphan when graph-apply disabled" landmine — `molecule.go:663/890`, `ralph.go:432`).
- **(b) by-id close of `gcg-N` lands on the graph store.** Through the API handler path (`humaHandleBeadClose` with `beadStoresForID` returning `[work, graph]`), close a `gcg-N` bead; assert it is closed in the SQLite graph store and untouched in work. Repeat for `claim`/`release-if-current` to pin the leaf capability asserts.
- **(c) `resolveGraphStore(...).Ready()` == the graph store's Ready.** Seed ready beads in both legs; assert `resolveGraphStore(work, cfg, cityPath, nil).Ready()` equals a bare-graph-handle `.Ready()` and excludes work-leg beads — pinning the G1 equivalence directly.
- **(d) byte-identical at `graph=bd`.** Same scenario with default config: assert `resolveGraphStore` returns the work store (pointer-identical), `beadStoresForID` skips the class arm, `newGraphRoutedStore(work, work)` returns `work` (no wrapper), and create/apply/by-id/Ready results are identical to a no-Router default city.
- **Landmine assertion:** assert `openGraphSQLiteStore(cityPath)`'s dir is `<cityPath>/.gc` (== `filepath.Join(cityPath, citylayout.RuntimeRoot)`) and is **not** `<cityPath>/.gc/graph` (`classSQLiteDir(cityPath, "graph")`); assert a graph bead written via the live path is visible at `.gc/beads.sqlite` and absent at `.gc/graph/`.

---

## Ordering, file-count, and landmine summary

| Phase | Files (≤5) | Router still live? | byte-identical at `graph=bd`? | Landmine guard |
|---|---|---|---|---|
| **P0** resolveGraphStore + accessors | `class_store.go`, `city_runtime.go`, `api_state.go`(shim+accessor), `internal/api/state.go`, test doubles | yes | yes (resolver uncalled on hot path) | `openGraphSQLiteStore` uses `.gc/` + `graphStoreHandleCache`; unit-assert dir == legacy & handle reused |
| **G1** rewire graph-only reads | `huma_handlers_beads.go`, `build_desired_state.go`, `cmd_ready.go`, `session_reconciler.go`, `pool_session_name.go` (split if >5) | yes | yes (`resolveGraphStore`→work at bd; `.Ready()`==full Ready) | reads route to dedicated graph store, never `.gc/graph/` |
| **G2** `beadStoresForID` `[work,graph]` | `handler_beads.go`, (+comment-only `huma_handlers_beads.go`) | yes | yes (class arm skipped when `graph==city`) | graph leg = `GraphBeadStore()` = `resolveGraphStore` (legacy loc) |
| **G3a** `graphRoutedStore` + repoint wrappers | `graph_routed_store.go`(new), `api_state.go`, `cmd_convoy_dispatch.go` | **retired** | yes (`newGraphRoutedStore(work,work)`→work) | wrappers source graph via `resolveGraphStore`; legacy loc preserved |
| **G3b** caching-store rework | `api_state.go` (`:192-258`, `closeBeadStoreHandle`) | — | yes | graph leg reused (cached no-close handle), never cached/reopened |
| **G3c** delete package + retarget test | rm `internal/coordrouter/`, `storeref_test.go`, `coordclass/classify.go` (`ClassifyGraphPlan`), `api_state.go` imports | — | yes | n/a |
| **G4** conformance test | `graph_routed_store_conformance_test.go`(new) | — | proves (d) | proves dir==`.gc/`, not `.gc/graph/` |

**Why this order is leaf-first and safe:** P0 adds the resolver with the legacy-location guard before anything calls it. G1 (reads) and G2 (by-id explicit slices) move *consumers* onto the dedicated accessor while the Router still serves the single-store dispatch/molecule path — so a regression in either is isolated and reversible without touching the create/apply spine. Only G3 flips the single-store spine (`routedPolicyStore`/`controlStoreWithGraphRouting`) from Router to `graphRoutedStore`, and it does so by *reconstruction with identical legs* (the graph leg pointer is the same cached `.gc/beads.sqlite` handle throughout), so the live `maintainer-city` (`graph_store=sqlite`) never sees an empty store. `ClassifyGraphPlan` dies naturally because the two-backend wrapper doesn't need per-plan classification. The conformance test (G4) pins all four contract points and the dir-location landmine permanently.

**Key correction to carry into implementation:** the recon's "G1 sites need no change, only the impl behind the interface moves" is true ONLY if you keep a store that *satisfies* `GraphOnlyReadyStore`/`GraphOnlyListStore`. Two valid routes: (i) the task's stated G1 — rewire callers to `resolveGraphStore(...).Ready()/.List()` directly (done in G1 for the controller/CLI/API sites); (ii) keep `dispatch/runtime.go:liveListForRoot` and `bead_policy_store.go`'s forwarder unchanged because `graphRoutedStore` (G3) implements the capability. The plan uses (i) for the explicit accessor sites and (ii) for the generic single-store dispatch site — both end byte-identical at `graph=bd`.
