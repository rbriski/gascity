package main

import (
	"context"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// This file is E2.2 of the domain/infra store split: the boundary-invariant
// test — the TDD forcing function that proves (and drives) the split.
//
// The invariant: on a REAL two-store city (a domain/work store plus a separate
// infra store), after a representative run of bead CREATORS, EVERY bead in each
// store must sit on the correct side of the coordination boundary. A domain
// store must hold no infrastructure-class bead; the infra store must hold no
// domain-class bead. The single source of truth for "which class" is
// coordclass.Classify — never a hand-rolled type list — so the boundary can
// never drift from the router.
//
// The graph-split audit's core lesson (its 73 gaps hid because tests used the
// wrong store shape) drives the design: the fast tier constructs the exact
// production wrapper stack — wrapStoreWithBeadPolicies over the store, threaded
// through the real controllerState typed accessors (resolveClassStore) — minus
// only the Dolt transport. It routes every creator through the production seam,
// so the DESTINATION of each bead is decided by production code, not by the
// test. A creator that is not yet routed through the typed accessors surfaces
// as a bead on the wrong side — that failing list IS the E2.3 worklist.
//
// SLING (the E2.3 target, now ROUTED): `gc sling` builds SlingDeps with
// GraphStore := slingSplitGraphStore(store, cfg, cityPath) — the graph
// coordination-class store, which is the infra store on a split city and nil on
// a legacy single-store city (so graphStore() collapses onto Store exactly as
// before, byte-identical). The molecule explosion therefore lands in the infra
// store on a split city. This is exercised as a PASS arm of runRoutedCreators
// (creator "sling-graph"), so a regression that unroutes the sling GraphStore
// seam fails the two boundary tests below.

// splitCity is the real two-store harness: a domain/work rig store plus a
// separate infra store, both wrapped in the SAME production policy stack
// (wrapStoreWithBeadPolicies) the controller uses, and threaded through a
// controllerState so every typed accessor (resolveClassStore) routes exactly as
// production does. Only the Dolt transport is swapped for MemStore.
type splitCity struct {
	cfg        *config.City
	workStore  beads.Store // HQ/city domain store (work class)
	rigStore   beads.Store // rig domain store (work class); sling's source store
	infraStore beads.Store // infra store (sessions, graph, messaging, orders, nudges)
	cs         *controllerState
	rigName    string
}

// newSplitCity builds the two-store harness. Both stores are policy-wrapped the
// way production wraps them, so the optional-capability assertions the create
// paths rely on (GraphApplyFor / HandlesFor / StorageCreateStore) stay intact —
// this is the production store SHAPE, not a bare MemStore.
func newSplitCity(t *testing.T) *splitCity {
	t.Helper()
	cfg := &config.City{}
	work := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	rig := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	// The infra store is wrapped the way production wraps it
	// (wrapInfraStoreWithBeadPolicies), so it mints reserved-prefix ids on every
	// explicit-ID-less create — matching openCityInfraStoreResultAt. Its backing
	// store honors explicit ids (NewMemStoreHonoringIDs) because a real bd store
	// honors the explicit --id the mint pre-fills, whereas the default MemStore
	// clobbers every id to gc-N; the id-honoring backing makes the fast tier
	// observe the minted reserved prefix exactly as a real Dolt store would.
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	const rigName = "rig-one"
	cityPath := t.TempDir()
	cs := &controllerState{
		cfg:            cfg,
		cityName:       "test-city",
		cityPath:       cityPath,
		cityBeadStore:  work,
		cityInfraStore: infra,
		beadStores:     map[string]beads.Store{rigName: rig},
	}

	// The CLI seam (cliGraphStore / cliOrderStore / cliNudgesStore, used by the
	// gc sling and gc order roots) sources the infra store from
	// cachedCityInfraStore(cityPath) — the filesystem-marker path — not the
	// controllerState.cityInfraStore field the controller accessors read. Point
	// that opener at the SAME policy-wrapped infra store this harness uses, so
	// the sling PASS arm exercises the real production GraphStore selection
	// (slingSplitGraphStore) against the two-store shape rather than a bare
	// injection. Reset the memo so the swap takes effect for this cityPath.
	clearInfraStoreCacheKey(cityPath)
	restore := swapCachedInfraStoreOpen(func(string) (beads.Store, bool, error) {
		return infra, true, nil
	})
	t.Cleanup(func() {
		restore()
		clearInfraStoreCacheKey(cityPath)
	})

	return &splitCity{
		cfg:        cfg,
		workStore:  work,
		rigStore:   rig,
		infraStore: infra,
		cs:         cs,
		rigName:    rigName,
	}
}

// domainStores returns every domain/work store of the split city: the HQ city
// store plus every rig store. These must hold NO infrastructure-class bead.
func (sc *splitCity) domainStores() map[string]beads.Store {
	return sc.cs.BeadStores()
}

// assertStoreClassBoundary lists every bead in store (open + closed, both tiers,
// scan allowed) and fails for any bead on the wrong side of the coordination
// boundary. wantInfra=false asserts a domain store (every bead must be
// ClassWork); wantInfra=true asserts the infra store (every bead must be an
// infrastructure class). It is exported inside the package so future E-phases
// (Postgres backend swap, E5) rerun the identical invariant against their store
// shapes: it reads only coordclass.Classify(bead) and which store returned the
// bead — the boundary, never the backend.
func assertStoreClassBoundary(t *testing.T, label string, store beads.Store, wantInfra bool) {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("%s: List: %v", label, err)
	}
	for _, b := range list {
		class := coordclass.Classify(b)
		gotInfra := class.IsInfrastructure()
		if gotInfra != wantInfra {
			side := "domain"
			if wantInfra {
				side = "infra"
			}
			t.Errorf("%s (%s store) holds wrong-side bead: id=%q type=%q labels=%v class=%s (want %s-class beads only)",
				label, side, b.ID, b.Type, b.Labels, class, side)
		}
	}
}

// creatorResult records where a creator's bead landed, so the conformance table
// can report PASS (correctly routed) or LEAK per creator.
type creatorResult struct {
	name        string
	wantClass   coordclass.Class // the class the bead should be
	landedInfra bool             // did production route it to the infra store?
	beadCount   int
}

// runRoutedCreators drives every creator that is (or should be) routed through
// the typed accessors, returning per-creator placement. Each creator obtains
// its destination store from the PRODUCTION seam (the controllerState typed
// accessors → resolveClassStore), so placement is decided by production code.
func (sc *splitCity) runRoutedCreators(t *testing.T) []creatorResult {
	t.Helper()
	var results []creatorResult

	countInfraDelta := func(fn func()) int {
		before := storeBeadCount(t, sc.infraStore)
		fn()
		return storeBeadCount(t, sc.infraStore) - before
	}

	// 1. SESSION bead — routed via cs.SessionsBeadStore() (session class).
	{
		delta := countInfraDelta(func() {
			ss := sc.cs.SessionsBeadStore()
			_, err := session.NewStore(ss).CreateSession(session.CreateSpec{
				Title:     "worker-1",
				AgentName: "worker-1",
				Metadata:  map[string]string{"provider": "tmux", "template": "claude"},
			})
			if err != nil {
				t.Fatalf("session create: %v", err)
			}
		})
		results = append(results, creatorResult{"session", coordclass.ClassSessions, delta > 0, delta})
	}

	// 2. MAIL (beadmail) — routed via newCityMailProvider (messaging class).
	{
		delta := countInfraDelta(func() {
			mp := newCityMailProvider(sc.cs.cityBeadStore, sc.cs.cityInfraStore, sc.cfg, sc.cs.cityPath, sc.cs.eventProv)
			if _, err := mp.Send("human", "worker-1", "hello", "body text"); err != nil {
				t.Fatalf("mail send: %v", err)
			}
		})
		results = append(results, creatorResult{"mail", coordclass.ClassMessaging, delta > 0, delta})
	}

	// 3. NUDGE enqueue — routed via cs.NudgesBeadStore() (nudges class).
	{
		delta := countInfraDelta(func() {
			ns := sc.cs.NudgesBeadStore()
			_, created, err := ensureQueuedNudgeBead(ns, newQueuedNudge("worker-1", "please continue", time.Now().UTC()))
			if err != nil {
				t.Fatalf("nudge enqueue: %v", err)
			}
			if !created {
				t.Fatal("nudge enqueue: expected a bead to be created")
			}
		})
		results = append(results, creatorResult{"nudge", coordclass.ClassNudges, delta > 0, delta})
	}

	// 4. WAIT bead — durable session-wait; session class, routed via
	//    cs.SessionsBeadStore(). Created through the session store the way the
	//    wait path does (Type=gate + gc:wait), so classification is honest.
	{
		delta := countInfraDelta(func() {
			ss := sc.cs.SessionsBeadStore()
			_, err := ss.Create(beads.Bead{
				Title:  "wait:deps",
				Type:   session.WaitBeadType,
				Status: "open",
				Labels: []string{session.WaitBeadLabel, "session:sess-1"},
				Metadata: map[string]string{
					"session_id": "sess-1",
					"kind":       "deps",
					"state":      "pending",
				},
			})
			if err != nil {
				t.Fatalf("wait create: %v", err)
			}
		})
		results = append(results, creatorResult{"wait", coordclass.ClassSessions, delta > 0, delta})
	}

	// 5. ORDER-TRACKING bead — routed via cs.ordersBeadStore("") (orders class).
	{
		delta := countInfraDelta(func() {
			os := sc.cs.ordersBeadStore("")
			if _, err := orders.NewStore(os).CreateRun("gate-alpha", orders.RunOpts{}); err != nil {
				t.Fatalf("order run create: %v", err)
			}
		})
		results = append(results, creatorResult{"order-tracking", coordclass.ClassOrders, delta > 0, delta})
	}

	// 6. GRAPH molecule — routed via cs.GraphBeadStore() (graph class). This is
	//    the CORRECTLY-ROUTED graph path (accessor-driven). The sling path,
	//    which does NOT use this accessor, is exercised separately below and is
	//    the known E2.3 leak.
	{
		delta := countInfraDelta(func() {
			gs := sc.cs.GraphBeadStore()
			if _, err := molecule.Instantiate(context.Background(), gs.Store, graphRecipe(), molecule.Options{}); err != nil {
				t.Fatalf("molecule instantiate (accessor-routed): %v", err)
			}
		})
		results = append(results, creatorResult{"graph-molecule (accessor)", coordclass.ClassGraph, delta > 0, delta})
	}

	// 7. PLAIN TASK — work class; stays in the rig domain store.
	{
		delta := countInfraDelta(func() {
			if _, err := sc.rigStore.Create(beads.Bead{Title: "real backlog item", Type: "task"}); err != nil {
				t.Fatalf("plain task create: %v", err)
			}
		})
		// A work bead must NOT land in infra: landedInfra=true here is itself a leak.
		results = append(results, creatorResult{"plain-task", coordclass.ClassWork, delta > 0, delta})
	}

	// 8. USER CONVOY — work class (a non-synthetic convoy); stays in rig domain.
	{
		delta := countInfraDelta(func() {
			a, err := sc.rigStore.Create(beads.Bead{Title: "convoy item A", Type: "task"})
			if err != nil {
				t.Fatalf("convoy item A: %v", err)
			}
			b, err := sc.rigStore.Create(beads.Bead{Title: "convoy item B", Type: "task"})
			if err != nil {
				t.Fatalf("convoy item B: %v", err)
			}
			if _, err := sc.rigStore.Create(beads.Bead{
				Title:  "user convoy",
				Type:   "convoy",
				Labels: []string{"tracks:" + a.ID, "tracks:" + b.ID},
			}); err != nil {
				t.Fatalf("user convoy: %v", err)
			}
		})
		results = append(results, creatorResult{"user-convoy", coordclass.ClassWork, delta > 0, delta})
	}

	// 9. SLING GRAPH molecule — routed via the PRODUCTION SlingDeps.GraphStore
	//    selection (slingSplitGraphStore → cliGraphStore → the graph class),
	//    NOT the cs accessor. This is the E2.3 fix: on a split city the sling
	//    graph store is the infra store, so the workflow/wisp explosion lands
	//    there. We build the exact SlingDeps.GraphStore production code builds
	//    and materialize onto SlingDeps.graphStore(), so a regression that
	//    unroutes the seam deposits graph beads in the rig store and fails the
	//    boundary tests.
	{
		delta := countInfraDelta(func() {
			deps := sling.SlingDeps{
				Store:      sc.rigStore,
				GraphStore: slingSplitGraphStore(sc.rigStore, sc.cfg, sc.cs.cityPath),
			}
			if _, err := molecule.Instantiate(context.Background(), slingDepsGraphStore(deps), graphRecipe(), molecule.Options{}); err != nil {
				t.Fatalf("molecule instantiate (sling seam): %v", err)
			}
		})
		results = append(results, creatorResult{"sling-graph", coordclass.ClassGraph, delta > 0, delta})
	}

	return results
}

// TestNoInfraBeadInDomainStore drives the representative correctly-routed
// creators over the real two-store shape, then asserts every domain store (HQ +
// rigs) holds NO infrastructure-class bead. This is one half of the boundary
// invariant. It is the forcing function: an infra creator that is not routed to
// the infra store (E2.3 target) will deposit an infra-class bead in a domain
// store and fail here by ID + class.
func TestNoInfraBeadInDomainStore(t *testing.T) {
	sc := newSplitCity(t)
	sc.runRoutedCreators(t)
	for name, store := range sc.domainStores() {
		assertStoreClassBoundary(t, "domain:"+name, store, false)
	}
}

// TestNoDomainBeadInInfraStore is the other half: after the same run, the infra
// store must hold NO domain (work) class bead — the boundary must not leak work
// into the coordination store either.
func TestNoDomainBeadInInfraStore(t *testing.T) {
	sc := newSplitCity(t)
	sc.runRoutedCreators(t)
	assertStoreClassBoundary(t, "infra", sc.infraStore, true)
}

// TestBoundaryAssertionIsNotVacuous is the negative control: it proves the
// boundary assertion actually FIRES when a leak is present, so a PASS of the two
// invariant tests above is meaningful and not a false green (the graph-split
// audit's failure mode: the wrong store shape passes silently). We deliberately
// deposit the sling-leak molecule into the rig DOMAIN store — exactly the E2.3
// bug — then confirm assertStoreClassBoundary reports it via a sub-test recorder.
func TestBoundaryAssertionIsNotVacuous(t *testing.T) {
	sc := newSplitCity(t)
	// Reproduce the sling leak: materialize graph beads into the rig domain store.
	if _, err := molecule.Instantiate(context.Background(), sc.rigStore, graphRecipe(), molecule.Options{}); err != nil {
		t.Fatalf("seed leak: %v", err)
	}

	// Run the assertion against a throwaway *testing.T so we can observe that it
	// records a failure without failing the real test.
	probe := &testing.T{}
	assertStoreClassBoundary(probe, "domain:probe", sc.rigStore, false)
	if !probe.Failed() {
		t.Fatal("assertStoreClassBoundary did NOT flag a known graph-class leak in a domain store — " +
			"the invariant is vacuous (wrong store shape / classifier not consulted)")
	}
}

// TestInfraCreatorConformanceTable is the creator conformance table: adding a
// new infra-bead creator without routing it through the typed accessors fails
// here in seconds. Each routed infra creator must land in the infra store; each
// work creator must land in a domain store. This is the fast local guard that
// keeps the split honest as creators are added.
func TestInfraCreatorConformanceTable(t *testing.T) {
	sc := newSplitCity(t)
	results := sc.runRoutedCreators(t)

	// Deterministic report order.
	sort.Slice(results, func(i, j int) bool { return results[i].name < results[j].name })

	for _, r := range results {
		wantInfra := r.wantClass.IsInfrastructure()
		if r.beadCount == 0 && wantInfra {
			t.Errorf("creator %q produced no bead in the infra store (expected %s-class routing)", r.name, r.wantClass)
			continue
		}
		if r.landedInfra != wantInfra {
			where := "domain store"
			if r.landedInfra {
				where = "infra store"
			}
			t.Errorf("creator %q (class %s) LEAK: routed to the %s (want %s)",
				r.name, r.wantClass, where, sideName(wantInfra))
		}
	}
}

// TestSlingGraphRoutesToInfraStore is the direct E2.3 assertion (the former
// TestSlingGraphMaterializationLeaksIntoDomainStore, flipped): on a split city
// the production SlingDeps.GraphStore selection (slingSplitGraphStore) resolves
// to the infra store, so the sling molecule explosion lands there and NOT in
// the rig/domain store. This pins the fix directly, in addition to the
// sling-graph arm of the two boundary tests.
func TestSlingGraphRoutesToInfraStore(t *testing.T) {
	sc := newSplitCity(t)

	// Build the exact SlingDeps.GraphStore production builds for a rig-scoped
	// sling, then materialize where production materializes it: onto
	// SlingDeps.graphStore().
	deps := sling.SlingDeps{
		Store:      sc.rigStore,
		GraphStore: slingSplitGraphStore(sc.rigStore, sc.cfg, sc.cs.cityPath),
	}
	if deps.GraphStore == nil {
		t.Fatal("split city: SlingDeps.GraphStore must be the infra store, got nil (leak preserved)")
	}

	beforeRig := storeBeadCount(t, sc.rigStore)
	beforeInfra := storeBeadCount(t, sc.infraStore)
	if _, err := molecule.Instantiate(context.Background(), slingDepsGraphStore(deps), graphRecipe(), molecule.Options{}); err != nil {
		t.Fatalf("molecule instantiate (sling seam): %v", err)
	}

	graphBeadsInRig := countGraphClassBeads(t, sc.rigStore)
	if graphBeadsInRig != 0 {
		t.Fatalf("sling graph LEAK: %d graph-class bead(s) landed in the rig/domain store %q "+
			"(rig delta %d); the GraphStore seam is unrouted", graphBeadsInRig, sc.rigName,
			storeBeadCount(t, sc.rigStore)-beforeRig)
	}
	graphBeadsInInfra := countGraphClassBeads(t, sc.infraStore)
	if graphBeadsInInfra == 0 {
		t.Fatalf("sling graph produced no graph-class bead in the infra store (infra delta %d)",
			storeBeadCount(t, sc.infraStore)-beforeInfra)
	}
}

// TestSlingSplitGraphStoreIsNilOnLegacyCity pins the byte-identity gate: with no
// infra store present (cachedCityInfraStore nil), slingSplitGraphStore returns
// nil so SlingDeps.graphStore() collapses onto Store, exactly as before the
// seam. A legacy single-store city therefore keeps the historical rig-store
// graph destination.
func TestSlingSplitGraphStoreIsNilOnLegacyCity(t *testing.T) {
	cityPath := t.TempDir() // no infra marker seeded
	clearInfraStoreCacheKey(cityPath)
	rig := wrapStoreWithBeadPolicies(beads.NewMemStore(), &config.City{})
	if got := slingSplitGraphStore(rig, nil, cityPath); got != nil {
		t.Fatalf("legacy city: slingSplitGraphStore must return nil (graphStore() collapses onto Store), got %T", got)
	}
}

// slingDepsGraphStore returns the store SlingDeps.graphStore() resolves to. The
// method is unexported; this mirrors its one-line fallback (GraphStore ?? Store)
// so the tests use the exact production selection logic.
func slingDepsGraphStore(deps sling.SlingDeps) beads.Store {
	if deps.GraphStore != nil {
		return deps.GraphStore
	}
	return deps.Store
}

// countGraphClassBeads lists every bead in a store and counts those the
// classifier routes to the graph class.
func countGraphClassBeads(t *testing.T, store beads.Store) int {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, b := range list {
		if coordclass.Classify(b) == coordclass.ClassGraph {
			n++
		}
	}
	return n
}

// graphRecipe is a minimal formula recipe that materializes a graph molecule:
// a workflow root (gc.kind=workflow → ClassGraph) plus a child step (inherits
// gc.root_bead_id → ClassGraph). Copied from the proven shape in
// internal/molecule/molecule_test.go.
func graphRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}
}

// storeBeadCount lists every bead in a store (both tiers, closed included) and
// returns the count. Used to attribute a creator's writes to a store.
func storeBeadCount(t *testing.T, store beads.Store) int {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("store count: List: %v", err)
	}
	return len(list)
}

func sideName(infra bool) string {
	if infra {
		return "infra store"
	}
	return "domain store"
}

// TestInfraStoreIDPrefixBoundary is the ID-prefix half of the invariant, landed
// with E2.4. The infra store is opened through wrapInfraStoreWithBeadPolicies,
// which mints reserved-prefix IDs (config.MintInfraBeadID → the "gcg" infra scope
// prefix) on every explicit-ID-less create, and MemStore now honors a pre-set
// ID, so the fast harness observes the mint. The invariant: every infra-store
// bead id's prefix segment satisfies config.IsReservedClassPrefix, and no
// domain-store bead id does — the ID-space boundary a stranded by-id read relies
// on to resolve into the right store post-split.
func TestInfraStoreIDPrefixBoundary(t *testing.T) {
	sc := newSplitCity(t)
	sc.runRoutedCreators(t)

	infra, err := sc.infraStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("infra List: %v", err)
	}
	if len(infra) == 0 {
		t.Fatal("infra store holds no beads after the representative run; the mint boundary is vacuous")
	}
	for _, b := range infra {
		if !config.IsReservedClassPrefix(idPrefixSegment(b.ID)) {
			t.Errorf("infra-store bead %q (type=%q labels=%v) lacks a reserved class prefix", b.ID, b.Type, b.Labels)
		}
	}

	// The other half: no domain-store bead may carry a reserved class prefix, so
	// the by-id read arms never misroute a work bead into the infra store.
	for name, store := range sc.domainStores() {
		domain, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			t.Fatalf("domain %q List: %v", name, err)
		}
		for _, b := range domain {
			if config.IsReservedClassPrefix(idPrefixSegment(b.ID)) {
				t.Errorf("domain-store %q bead %q (type=%q) carries a reserved class prefix", name, b.ID, b.Type)
			}
		}
	}
}

// idPrefixSegment returns the prefix segment of a bead id (everything before the
// last "-"), matching how reserved-prefix validation splits ids.
func idPrefixSegment(id string) string {
	if i := strings.LastIndex(id, "-"); i > 0 {
		return id[:i]
	}
	return id
}
