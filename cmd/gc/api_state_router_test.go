package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// graphSQLiteCfg returns a city config that opts the graph class onto the
// embedded SQLite backend.
func graphSQLiteCfg() *config.City {
	cfg := &config.City{}
	cfg.Beads.GraphStore = "sqlite"
	return cfg
}

func countSQLiteBackends(r *coordrouter.Router) int {
	n := 0
	for _, b := range r.Backends() {
		if _, ok := b.(*beads.SQLiteStore); ok {
			n++
		}
	}
	return n
}

// TestCloseBeadStoreHandlePeelsRouter proves closeBeadStoreHandle reaches the
// underlying CachingStore through the Router (so StopReconciler/CloseStore fire
// and no reconciler goroutine leaks).
func TestCloseBeadStoreHandlePeelsRouter(t *testing.T) {
	cs := beads.NewCachingStore(beads.NewMemStore(), nil)
	wrapped := wrapStoreWithBeadPolicies(coordrouter.New(cs), nil) // policy(Router(caching(mem)))
	if err := closeBeadStoreHandle(wrapped); err != nil {
		t.Fatalf("closeBeadStoreHandle(policy(Router(caching))): %v", err)
	}
}

// TestRoutedPolicyStoreBuildsRouterOnlyWhenOptedIn proves the opt-in boundary:
// without graph_store the result is plain policy(workBackend) — no Router, zero
// overhead, byte-identical to before the split; with graph_store=sqlite it inserts
// the per-class Router with a registered SQLite graph backend.
func TestRoutedPolicyStoreBuildsRouterOnlyWhenOptedIn(t *testing.T) {
	// Default off: no Router.
	off := routedPolicyStore(beads.NewMemStore(), &config.City{}, t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(off) })
	base, _, ok := unwrapBeadPolicyStore(off)
	if !ok {
		t.Fatal("expected the default result to be policy-wrapped")
	}
	if _, isRouter := base.(*coordrouter.Router); isRouter {
		t.Fatal("default-off must not insert a Router")
	}

	// Opted in: Router with a SQLite graph backend.
	dir := t.TempDir()
	on := routedPolicyStore(beads.NewMemStore(), graphSQLiteCfg(), dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(on) })
	base, _, ok = unwrapBeadPolicyStore(on)
	if !ok {
		t.Fatal("expected the opted-in result to be policy-wrapped")
	}
	router, isRouter := base.(*coordrouter.Router)
	if !isRouter {
		t.Fatalf("graph_store=sqlite must insert a *coordrouter.Router, got %T", base)
	}
	if n := countSQLiteBackends(router); n != 1 {
		t.Fatalf("opted-in Router has %d SQLite backends, want 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "beads.sqlite")); err != nil {
		t.Fatalf("expected the SQLite graph file at <scope>/.gc/beads.sqlite: %v", err)
	}
}

// TestWrapWithCachingStoreRegistersGraphSQLiteWhenOptedIn is E1: with
// [beads] graph_store = "sqlite" the controller's store construction yields
// policy(Router(caching(work) + sqlite-graph)) and the store file is created under
// <scope>/.gc/. The work backend is cached; the graph backend is a distinct
// SQLite store outside the cache.
func TestWrapWithCachingStoreRegistersGraphSQLiteWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	policy := wrapStoreWithBeadPolicies(beads.NewMemStore(), graphSQLiteCfg()) // policy(mem)
	wrapped := wrapWithCachingStore(context.TODO(), policy, nil, false, dir)   // policy(Router(caching(mem)) + sqlite)
	t.Cleanup(func() { _ = closeBeadStoreHandle(wrapped) })

	base, _, ok := unwrapBeadPolicyStore(wrapped)
	if !ok {
		t.Fatal("expected the result to be policy-wrapped")
	}
	router, ok := base.(*coordrouter.Router)
	if !ok {
		t.Fatalf("expected a *coordrouter.Router inside the policy wrapper, got %T", base)
	}
	if _, ok := router.Backend(coordclass.ClassWork).(*beads.CachingStore); !ok {
		t.Fatalf("work backend = %T, want *beads.CachingStore", router.Backend(coordclass.ClassWork))
	}
	if n := countSQLiteBackends(router); n != 1 {
		t.Fatalf("Router has %d SQLite backends, want exactly 1", n)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "beads.sqlite")); err != nil {
		t.Fatalf("expected the SQLite graph file at <scope>/.gc/beads.sqlite: %v", err)
	}
}

// TestOpenStoreResultAtForCityRoutesGraphToSQLiteWhenOptedIn proves the universal
// store chokepoint — which every gc process (controller AND workers) funnels
// through — honors [beads] graph_store = "sqlite": the opened store is a Router
// whose graph-class creates land in the embedded SQLite backend while work-class
// creates stay on the (file) work backend. This is the no-socket worker mediation:
// a worker's in-process store reaches the graph store directly.
func TestOpenStoreResultAtForCityRoutesGraphToSQLiteWhenOptedIn(t *testing.T) {
	cityDir := t.TempDir()
	cityTOML := "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\ngraph_store = \"sqlite\"\n"
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte(cityTOML), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_BEADS", "file")
	if err := ensureScopedFileStoreLayout(cityDir); err != nil {
		t.Fatal(err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityDir); err != nil {
		t.Fatal(err)
	}

	result, err := openStoreResultAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreResultAtForCity: %v", err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(result.Store) })

	base, _, ok := unwrapBeadPolicyStore(result.Store)
	if !ok {
		t.Fatal("expected the opened store to be policy-wrapped")
	}
	router, ok := base.(*coordrouter.Router)
	if !ok {
		t.Fatalf("graph_store=sqlite: expected a *coordrouter.Router from the chokepoint, got %T", base)
	}
	sqliteBackend := func() *beads.SQLiteStore {
		for _, b := range router.Backends() {
			if s, ok := b.(*beads.SQLiteStore); ok {
				return s
			}
		}
		return nil
	}()
	if sqliteBackend == nil {
		t.Fatal("router has no SQLite graph backend")
	}

	// A graph-classified bead (gc:wisp) routes to SQLite; a work bead does not.
	// The SQLite graph store mints a DISTINCT id prefix (graphStoreIDPrefix) from
	// the work backend's "gc", so by-id resolution (Router.backendForID) is
	// unambiguous: a graph id can never alias a work id even though both stores
	// run independent N sequences. Assert the prefix is disjoint AND that a by-id
	// lookup lands on the graph backend.
	gb, err := router.Create(beads.Bead{Title: "wisp", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if !strings.HasPrefix(gb.ID, graphStoreIDPrefix+"-") {
		t.Fatalf("graph bead id %q does not carry the distinct graph prefix %q-", gb.ID, graphStoreIDPrefix)
	}
	gotGraph, err := sqliteBackend.Get(gb.ID)
	if err != nil || gotGraph.Title != "wisp" {
		t.Fatalf("graph bead not in the SQLite backend: got %q, err %v", gotGraph.Title, err)
	}
	if _, err := router.Create(beads.Bead{Title: "backlog", Type: "task"}); err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	// The SQLite graph backend must hold ONLY the one graph bead — the work bead
	// stayed on the file work backend.
	graphList, err := sqliteBackend.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("sqlite List: %v", err)
	}
	if len(graphList) != 1 || graphList[0].Title != "wisp" {
		t.Fatalf("SQLite graph backend holds %d bead(s); want only the graph bead %q", len(graphList), "wisp")
	}
}

// TestRoutedGraphStoreByIDRoutingSurvivesNumericIDOverlap is the regression
// guard for the convergence-blocking id-namespace collision (ga-y5pwx3): the
// work backend and the SQLite graph backend each run an independent N sequence,
// so without distinct prefixes a work bead and a graph bead would both be "gc-1"
// — and Router.backendForID (first backend whose Get succeeds, work first) would
// misroute a by-id close of the graph step to the work store, leaving the graph
// step open so the molecule never converges. The graph store's distinct
// graphStoreIDPrefix makes the two namespaces disjoint, so a by-id close lands on
// the owning backend even when the numeric component overlaps.
func TestRoutedGraphStoreByIDRoutingSurvivesNumericIDOverlap(t *testing.T) {
	work := beads.NewMemStore() // mints gc-N like the file/native work store
	store := routedPolicyStore(work, graphSQLiteCfg(), t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })

	// Work bead -> work backend (gc-1); graph bead -> SQLite (gcg-1). Same N=1,
	// disjoint prefixes.
	wb, err := store.Create(beads.Bead{Title: "backlog", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	gb, err := store.Create(beads.Bead{Title: "workflow root", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if !strings.HasPrefix(wb.ID, "gc-") {
		t.Fatalf("work bead id %q, want a gc- work id", wb.ID)
	}
	if !strings.HasPrefix(gb.ID, graphStoreIDPrefix+"-") {
		t.Fatalf("graph bead id %q, want the distinct %q- graph prefix", gb.ID, graphStoreIDPrefix)
	}
	if wb.ID == gb.ID {
		t.Fatalf("work and graph ids collide (%q): distinct prefixes did not separate the namespaces", wb.ID)
	}

	// A by-id close of the graph bead must land on the SQLite backend — the
	// convergence-critical mutation. The work bead with the overlapping N stays open.
	if err := store.Close(gb.ID); err != nil {
		t.Fatalf("close graph bead %q: %v", gb.ID, err)
	}
	got, err := store.Get(gb.ID)
	if err != nil {
		t.Fatalf("get graph bead after close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph bead %q status = %q, want closed (the close misrouted to the work backend)", gb.ID, got.Status)
	}
	gotWork, err := store.Get(wb.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if gotWork.Status == "closed" {
		t.Fatalf("work bead %q was closed by a graph-step close — by-id routing misfired", wb.ID)
	}
}

// TestWrapWithCachingStorePeelsAndCachesIncomingRouter proves the controller path
// for a worker-chokepoint store: openStoreResultAtForCity already built
// policy(Router(work) + sqlite), so wrapWithCachingStore must cache the work
// backend IN PLACE — keeping the single already-open SQLite graph backend — rather
// than double-wrapping or re-opening the graph file.
func TestWrapWithCachingStorePeelsAndCachesIncomingRouter(t *testing.T) {
	dir := t.TempDir()
	// Simulate the chokepoint output: policy(Router(mem) + sqlite).
	incoming := routedPolicyStore(beads.NewMemStore(), graphSQLiteCfg(), dir)
	base, _, _ := unwrapBeadPolicyStore(incoming)
	incomingRouter := base.(*coordrouter.Router)
	graphBefore := incomingRouter.Backend(coordclass.ClassGraph)

	wrapped := wrapWithCachingStore(context.TODO(), incoming, nil, false, dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(wrapped) })

	base, _, ok := unwrapBeadPolicyStore(wrapped)
	if !ok {
		t.Fatal("expected the result to be policy-wrapped")
	}
	router, ok := base.(*coordrouter.Router)
	if !ok {
		t.Fatalf("expected a *coordrouter.Router, got %T", base)
	}
	// Work backend is now cached.
	if _, ok := router.Backend(coordclass.ClassWork).(*beads.CachingStore); !ok {
		t.Fatalf("work backend = %T, want *beads.CachingStore (cached in place)", router.Backend(coordclass.ClassWork))
	}
	// Exactly one SQLite backend, and it is the SAME handle (not re-opened).
	if n := countSQLiteBackends(router); n != 1 {
		t.Fatalf("Router has %d SQLite backends after caching, want exactly 1 (no re-open)", n)
	}
	if router.Backend(coordclass.ClassGraph) != graphBefore {
		t.Fatal("graph backend was replaced; expected the single already-open SQLite handle to be reused")
	}
}
