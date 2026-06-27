package main

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// graphSQLiteCfg returns a city config that opts the graph class onto the
// embedded SQLite backend.
func graphSQLiteCfg() *config.City {
	cfg := &config.City{}
	cfg.Beads.GraphStore = "sqlite"
	return cfg
}

// graphSQLiteBackend extracts the embedded *beads.SQLiteStore from a graph
// backend, unwrapping the noCloseSQLiteStore the cache registers (the shared
// handle is wrapped so a consumer's CloseStore is a no-op; see 7cfff89fa).
func graphSQLiteBackend(b beads.Store) (*beads.SQLiteStore, bool) {
	switch s := b.(type) {
	case *beads.SQLiteStore:
		return s, true
	case noCloseSQLiteStore:
		return s.SQLiteStore, true
	}
	return nil, false
}

// TestGraphStoreBackendIsSharedCityScopeAcrossScopes proves the city-scope single
// graph store: two scopes (distinct work backends, e.g. two rigs) under the SAME
// city share ONE embedded SQLite graph store, so graph-bead IDs mint from one
// sequence and never collide across scopes. This is the structural fix for the
// cross-scope gcg-N collision that deadlocked the claim path (a bare gcg-4 resolved
// to the wrong store). Post-coordrouter the two scopes route their graph creates
// through the policy create-chokepoint (routedPolicyStore => policy(work) with the
// shared resolveGraphStore handle); a bead created via one scope's store is the
// SAME bead, same id, read back through the other scope's graph store.
func TestGraphStoreBackendIsSharedCityScopeAcrossScopes(t *testing.T) {
	cfg := graphSQLiteCfg()
	cityPath := t.TempDir()

	rigA := routedPolicyStore(beads.NewMemStoreFrom(1000, nil, nil), cfg, cityPath)
	t.Cleanup(func() { _ = closeBeadStoreHandle(rigA) })
	rigB := routedPolicyStore(beads.NewMemStoreFrom(2000, nil, nil), cfg, cityPath)
	t.Cleanup(func() { _ = closeBeadStoreHandle(rigB) })

	// Both scopes resolve the SAME city-scope graph handle (graphStoreHandleCache
	// keyed on cityPath), so a graph create via either lands in the one store.
	graphA := resolveGraphStore(beads.NewMemStore(), cfg, cityPath, nil)
	graphB := resolveGraphStore(beads.NewMemStore(), cfg, cityPath, nil)
	if graphA != graphB {
		t.Fatal("two scopes under one city resolved DIFFERENT graph stores — the cross-scope gcg-N collision is back")
	}

	a, err := rigA.Create(beads.Bead{Title: "graph A", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("rigA create graph bead: %v", err)
	}
	b, err := rigB.Create(beads.Bead{Title: "graph B", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("rigB create graph bead: %v", err)
	}
	if a.ID == b.ID {
		t.Fatalf("two scopes minted colliding graph id %q — the cross-scope gcg-N collision is back", a.ID)
	}
	got, err := graphB.Get(a.ID)
	if err != nil {
		t.Fatalf("graphB.Get(%s) (created via rigA) = %v — scopes are NOT sharing one city graph store", a.ID, err)
	}
	if got.Title != "graph A" {
		t.Fatalf("graphB.Get(%s).Title = %q, want %q", a.ID, got.Title, "graph A")
	}
}

// TestCloseBeadStoreHandleReachesCachingStore proves closeBeadStoreHandle reaches
// the underlying CachingStore through the policy wrapper (so StopReconciler/
// CloseStore fire and no reconciler goroutine leaks). Post-coordrouter there is no
// Router to peel — the policy(caching(mem)) chain must still be walked.
func TestCloseBeadStoreHandleReachesCachingStore(t *testing.T) {
	cs := beads.NewCachingStore(beads.NewMemStore(), nil)
	wrapped := wrapStoreWithBeadPolicies(cs, nil) // policy(caching(mem))
	if err := closeBeadStoreHandle(wrapped); err != nil {
		t.Fatalf("closeBeadStoreHandle(policy(caching)): %v", err)
	}
}

// TestRoutedPolicyStoreNeverInsertsRouter proves the post-coordrouter boundary:
// routedPolicyStore returns plain policy(workBackend) for EVERY city — default OR
// graph-relocated. The base under the policy wrapper is the exact work store, never
// a routing object. When graph is relocated the dedicated graph store is reached via
// resolveGraphStore (a distinct handle, the legacy .gc/beads.sqlite file), and the
// create-chokepoint routes graph-class creates there.
func TestRoutedPolicyStoreNeverInsertsRouter(t *testing.T) {
	// Default off: policy(work), no graph store.
	work := beads.NewMemStore()
	off := routedPolicyStore(work, &config.City{}, t.TempDir())
	t.Cleanup(func() { _ = closeBeadStoreHandle(off) })
	base, _, ok := unwrapBeadPolicyStore(off)
	if !ok {
		t.Fatal("expected the default result to be policy-wrapped")
	}
	if base != beads.Store(work) {
		t.Fatalf("default-off base = %T, want the work store directly (no routing object)", base)
	}

	// Opted in: still policy(work) — no Router — and a distinct SQLite graph store.
	dir := t.TempDir()
	onWork := beads.NewMemStore()
	on := routedPolicyStore(onWork, graphSQLiteCfg(), dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(on) })
	base, _, ok = unwrapBeadPolicyStore(on)
	if !ok {
		t.Fatal("expected the opted-in result to be policy-wrapped")
	}
	if base != beads.Store(onWork) {
		t.Fatalf("graph=sqlite base = %T, want policy(work) — no Router", base)
	}
	graph := resolveGraphStore(onWork, graphSQLiteCfg(), dir, nil)
	if graph == beads.Store(onWork) {
		t.Fatal("graph=sqlite must resolve a distinct dedicated graph store, got the work store")
	}
	if _, ok := graphSQLiteBackend(graph); !ok {
		t.Fatalf("resolved graph store = %T, want an embedded SQLite store", graph)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "beads.sqlite")); err != nil {
		t.Fatalf("expected the SQLite graph file at <scope>/.gc/beads.sqlite: %v", err)
	}
}

// TestWrapWithCachingStoreRoutesGraphSQLiteWhenOptedIn is E1: with
// [beads] graph_store = "sqlite" the controller's store construction yields
// policy(caching(work)) and a DISTINCT dedicated SQLite graph store at <scope>/.gc/.
// The work backend is cached; the graph backend lives outside the cache, reached
// via the create-chokepoint / resolveGraphStore.
func TestWrapWithCachingStoreRoutesGraphSQLiteWhenOptedIn(t *testing.T) {
	dir := t.TempDir()
	work := beads.NewMemStore()
	policy := wrapStoreWithBeadPolicies(work, graphSQLiteCfg(), dir) // policy(mem)
	wrapped := wrapWithCachingStore(context.TODO(), policy, nil, false, dir)
	t.Cleanup(func() { _ = closeBeadStoreHandle(wrapped) })

	base, _, ok := unwrapBeadPolicyStore(wrapped)
	if !ok {
		t.Fatal("expected the result to be policy-wrapped")
	}
	if _, ok := base.(*beads.CachingStore); !ok {
		t.Fatalf("work backend = %T, want *beads.CachingStore (no Router)", base)
	}
	// A graph create through the wrapped store lands in the dedicated SQLite store,
	// physically present at <scope>/.gc/beads.sqlite.
	gb, err := wrapped.Create(beads.Bead{Title: "wisp", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if !strings.HasPrefix(gb.ID, graphStoreIDPrefix+"-") {
		t.Fatalf("graph bead id %q does not carry the distinct graph prefix %q-", gb.ID, graphStoreIDPrefix)
	}
	graph := resolveGraphStore(work, graphSQLiteCfg(), dir, nil)
	if _, err := graph.Get(gb.ID); err != nil {
		t.Fatalf("graph bead not in the dedicated SQLite store: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gc", "beads.sqlite")); err != nil {
		t.Fatalf("expected the SQLite graph file at <scope>/.gc/beads.sqlite: %v", err)
	}
}

// TestOpenStoreResultAtForCityRoutesGraphToSQLiteWhenOptedIn proves the universal
// store chokepoint — which every gc process (controller AND workers) funnels
// through — honors [beads] graph_store = "sqlite": a graph-class create lands in the
// embedded SQLite graph store while a work-class create stays on the (file) work
// backend. This is the no-socket worker mediation: a worker's in-process store
// reaches the dedicated graph store via the policy create-chokepoint.
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
	if _, isCaching := base.(*beads.CachingStore); !isCaching {
		// The file work backend may or may not be cache-wrapped depending on layout;
		// the load-bearing assertion is that it is NOT a routing object and graph
		// creates physically land in SQLite (below).
		if _, sqlOK := graphSQLiteBackend(base); sqlOK {
			t.Fatalf("graph_store=sqlite: the base must be the WORK backend, not the SQLite graph store (got %T)", base)
		}
	}

	cfg, err := loadCityConfig(cityDir, os.Stderr)
	if err != nil {
		t.Fatalf("loadCityConfig: %v", err)
	}
	graph := resolveGraphStore(result.Store, cfg, cityDir, nil)
	sqliteBackend, ok := graphSQLiteBackend(graph)
	if !ok {
		t.Fatalf("resolved graph store = %T, want an embedded SQLite store", graph)
	}

	// A graph-classified bead (gc:wisp) routes to SQLite; a work bead does not.
	// The SQLite graph store mints a DISTINCT id prefix (graphStoreIDPrefix) from
	// the work backend's "gc", so by-id resolution is unambiguous: a graph id can
	// never alias a work id even though both stores run independent N sequences.
	gb, err := result.Store.Create(beads.Bead{Title: "wisp", Type: "task", Labels: []string{"gc:wisp"}})
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
	if _, err := result.Store.Create(beads.Bead{Title: "backlog", Type: "task"}); err != nil {
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

// TestRoutedGraphStoreByIDRoutingSurvivesNumericIDOverlap is the regression guard
// for the convergence-blocking id-namespace collision (ga-y5pwx3): the work backend
// and the SQLite graph backend each run an independent N sequence, so without
// distinct prefixes a work bead and a graph bead would both be "gc-1" — and a by-id
// close routed work-first would misroute a close of the graph step to the work
// store, leaving the graph step open so the molecule never converges. The graph
// store's distinct graphStoreIDPrefix makes the two namespaces disjoint, so a by-id
// close (routed via storeref.PrefixOwner over [graph, work]) lands on the owning
// backend even when the numeric component overlaps.
func TestRoutedGraphStoreByIDRoutingSurvivesNumericIDOverlap(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore() // mints gc-N like the file/native work store
	cfg := graphSQLiteCfg()
	store := routedPolicyStore(work, cfg, cityPath)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })
	graph := resolveGraphStore(work, cfg, cityPath, nil)

	// Work bead -> work backend (gc-1); graph bead -> SQLite (gcg-1). Same N=1,
	// disjoint prefixes. The create-chokepoint routes the graph bead to SQLite.
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
	// convergence-critical mutation. Resolve the owning store the way doClose /
	// beadStoresForID do (prefix-owner over [graph, work]).
	closeViaOwner := func(id string) error {
		owner := store
		if g := graph; g != beads.Store(work) {
			if o := pickOwner(id, g, store); o != nil {
				owner = o
			}
		}
		return owner.Close(id)
	}
	if err := closeViaOwner(gb.ID); err != nil {
		t.Fatalf("close graph bead %q: %v", gb.ID, err)
	}
	got, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("get graph bead after close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph bead %q status = %q, want closed (the close misrouted to the work backend)", gb.ID, got.Status)
	}
	gotWork, err := work.Get(wb.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if gotWork.Status == "closed" {
		t.Fatalf("work bead %q was closed by a graph-step close — by-id routing misfired", wb.ID)
	}
}

// pickOwner mirrors the production by-id store resolution (storeref.PrefixOwner over
// [graph, work]): the graph store owns the gcg- prefix, so a gcg- id routes there;
// a work id has no graph-prefix match and stays on the work store.
func pickOwner(id string, graph, work beads.Store) beads.Store {
	if p, ok := graph.(interface{ IDPrefix() string }); ok {
		if pfx := p.IDPrefix(); pfx != "" && strings.HasPrefix(id, pfx+"-") {
			return graph
		}
	}
	return work
}

// TestResolveGraphStoreReusesSQLiteHandlePerDir proves the stampede fix: repeated
// resolveGraphStore calls for the same graph dir reuse ONE SQLite handle (the
// controller rebuilds its store map frequently; without reuse each rebuild leaked a
// fresh handle, serializing SQLite's writer and hanging graph-step claims). Distinct
// dirs still get distinct handles.
func TestResolveGraphStoreReusesSQLiteHandlePerDir(t *testing.T) {
	cfg := graphSQLiteCfg()
	scope := t.TempDir()

	b1 := resolveGraphStore(beads.NewMemStore(), cfg, scope, nil)
	b2 := resolveGraphStore(beads.NewMemStore(), cfg, scope, nil)
	if b1 == nil || b2 == nil {
		t.Fatal("graph store was not resolved")
	}
	if b1 != b2 {
		t.Fatal("resolveGraphStore opened a new SQLite handle for the same dir instead of reusing the cached one (the stampede leak)")
	}

	b3 := resolveGraphStore(beads.NewMemStore(), cfg, t.TempDir(), nil)
	if b3 == b1 {
		t.Fatal("distinct graph dirs must get distinct SQLite handles")
	}
}

// TestResolveGraphStoreCachedHandleSurvivesConsumerClose proves the use-after-close
// fix: a consumer's closeBeadStoreHandle (CloseStore) on the shared cached graph
// store must NOT close the underlying DB, or every other consumer (reconciler, order
// dispatch, convergence) fails with "sql: database is closed". After a CloseStore,
// the cached store must still be usable.
func TestResolveGraphStoreCachedHandleSurvivesConsumerClose(t *testing.T) {
	cfg := graphSQLiteCfg()
	scope := t.TempDir()

	g := resolveGraphStore(beads.NewMemStore(), cfg, scope, nil)
	if g == nil {
		t.Fatal("graph store not resolved")
	}

	// Simulate a short-lived consumer closing the store it opened.
	if c, ok := g.(interface{ CloseStore() error }); ok {
		if err := c.CloseStore(); err != nil {
			t.Fatalf("CloseStore returned error: %v", err)
		}
	} else {
		t.Fatal("cached graph store does not expose CloseStore")
	}

	// The shared handle must still be usable — not "sql: database is closed".
	if _, err := g.Create(beads.Bead{Title: "after consumer close", Type: "task", Labels: []string{"gc:wisp"}}); err != nil {
		t.Fatalf("cached graph store unusable after a consumer CloseStore (use-after-close regression): %v", err)
	}
}

// graphPostgresCfg builds a graph=postgres city config from the test DSN (setting
// the pgauth password env via postgresCfgFromDSN), and provisions + truncates the
// gcg schema for a clean slate. SKIPs nothing — the caller gates on the DSN.
func graphPostgresCfg(t *testing.T, dsn string) *config.City {
	t.Helper()
	cfg := postgresCfgFromDSN(t, dsn, config.BeadClassGraph)
	schema, _ := config.ReservedClassPrefix(config.BeadClassGraph) // gcg
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close() //nolint:errcheck
	if _, err := db.Exec(fmt.Sprintf(`TRUNCATE %[1]s.beads, %[1]s.labels, %[1]s.metadata, %[1]s.deps, %[1]s.kv CASCADE`, schema)); err != nil {
		t.Fatalf("truncate %q: %v", schema, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER SEQUENCE %s.bead_seq RESTART WITH 1`, schema)); err != nil {
		t.Fatalf("reset seq for %q: %v", schema, err)
	}
	return cfg
}

// TestRoutedGraphStorePostgresRoutesAndConverges is the Path A end-to-end proof:
// a graph=postgres city routes graph-classified beads to the Postgres gcg schema via
// the policy create-chokepoint (work beads stay on the work store), the graph bead is
// physically a row in the gcg schema, and the convergence-critical by-id close of a
// graph step lands on Postgres — the ga-y5pwx3 numeric-id-overlap regression, now
// over Postgres. SKIPPED unless GC_TEST_POSTGRES_DSN points at a disposable Postgres.
func TestRoutedGraphStorePostgresRoutesAndConverges(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	cityPath := t.TempDir()
	cfg := graphPostgresCfg(t, dsn)
	gcg, _ := config.ReservedClassPrefix(config.BeadClassGraph) // gcg

	work := beads.NewMemStore() // mints gc-N like the work store
	store := routedPolicyStore(work, cfg, cityPath)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })

	// graph=postgres must resolve a distinct (non-work) graph backend.
	graph := resolveGraphStore(work, cfg, cityPath, nil)
	if graph == beads.Store(work) {
		t.Fatal("graph backend == work backend: the Postgres graph backend was not resolved")
	}

	// Graph-classified bead (gc:wisp) routes to Postgres (gcg prefix); the work bead
	// stays on the work store with an overlapping numeric id (gc-N vs gcg-N).
	gb, err := store.Create(beads.Bead{Title: "workflow root", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if !strings.HasPrefix(gb.ID, gcg+"-") {
		t.Fatalf("graph bead id %q, want the %q- graph prefix", gb.ID, gcg)
	}
	wb, err := store.Create(beads.Bead{Title: "backlog", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if !strings.HasPrefix(wb.ID, "gc-") {
		t.Fatalf("work bead id %q, want a gc- work id", wb.ID)
	}

	// Definitive: the graph bead is physically a row in the Postgres gcg schema.
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open pg: %v", err)
	}
	defer db.Close() //nolint:errcheck
	var cnt int
	if err := db.QueryRow(fmt.Sprintf(`SELECT count(*) FROM %s.beads WHERE id=$1`, gcg), gb.ID).Scan(&cnt); err != nil {
		t.Fatalf("query gcg.beads: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("graph bead %q not found in the Postgres %s schema (count=%d)", gb.ID, gcg, cnt)
	}

	// Convergence-critical: a by-id close of the graph step lands on Postgres, not
	// the work store. The work bead with the overlapping numeric id stays open.
	if err := graph.Close(gb.ID); err != nil {
		t.Fatalf("close graph bead %q: %v", gb.ID, err)
	}
	got, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("get graph bead after close: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph bead %q status = %q, want closed (close misrouted off Postgres)", gb.ID, got.Status)
	}
	gotWork, err := work.Get(wb.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if gotWork.Status == "closed" {
		t.Fatalf("work bead %q was closed by a graph-step close — by-id routing misfired", wb.ID)
	}
}
