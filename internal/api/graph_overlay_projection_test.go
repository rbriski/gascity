package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// fakeOverlayGraphStore models the P1.5 residence router as seen by the API
// layer: it composes a legacy leg (the city store, embedded so every Store
// method promotes to it) and a journal leg, and its global reads return
// legacy ∪ journal. It advertises the overlay via beads.StoreOverlay so
// workflowStores can drop the redundant city entry.
type fakeOverlayGraphStore struct {
	beads.Store // legacy leg (== the city store)
	journal     beads.Store
}

func (o *fakeOverlayGraphStore) OverlaysStore(other beads.Store) bool {
	return other != nil && o.Store == other
}

func (o *fakeOverlayGraphStore) Get(id string) (beads.Bead, error) {
	if b, err := o.journal.Get(id); err == nil {
		return b, nil
	}
	return o.Store.Get(id)
}

func (o *fakeOverlayGraphStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	legacy, err := o.Store.List(query)
	if err != nil {
		return nil, err
	}
	journal, err := o.journal.List(query)
	if err != nil {
		return nil, err
	}
	return unionBeadsByID(legacy, journal), nil
}

var _ beads.StoreOverlay = (*fakeOverlayGraphStore)(nil)

func unionBeadsByID(legacy, journal []beads.Bead) []beads.Bead {
	if len(journal) == 0 {
		return legacy
	}
	seen := make(map[string]struct{}, len(legacy))
	for _, b := range legacy {
		seen[b.ID] = struct{}{}
	}
	out := append([]beads.Bead(nil), legacy...)
	for _, b := range journal {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		out = append(out, b)
	}
	return out
}

// closeVisitCountingStore records CloseAll and Delete invocations on the underlying
// store so a test can prove a workflow root is closed/deleted exactly once
// even when it is reachable through both an overlay and the city entry.
type closeVisitCountingStore struct {
	beads.Store
	closeAllCalls int
	deleteCalls   int
}

func (c *closeVisitCountingStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	c.closeAllCalls++
	return c.Store.CloseAll(ids, metadata)
}

func (c *closeVisitCountingStore) Delete(id string) error {
	c.deleteCalls++
	return c.Store.Delete(id)
}

func mustCreateWorkflowRoot(t *testing.T, store beads.Store, title string) beads.Bead {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title: title,
		Type:  "workflow",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root %q: %v", title, err)
	}
	return root
}

// TestWorkflowStoresDropsCityEntryWhenGraphOverlaysIt is the core structural
// assertion: an opted city (overlay router whose legacy leg is the city store)
// produces a single graph entry, not graph+city, so downstream projection and
// delete arms visit each legacy-resident root once.
func TestWorkflowStoresDropsCityEntryWhenGraphOverlaysIt(t *testing.T) {
	state := newFakeState(t)
	city := beads.NewMemStore()
	state.cityBeadStore = city
	state.graphBeadStore = &fakeOverlayGraphStore{Store: city, journal: beads.NewMemStore()}
	state.stores = map[string]beads.Store{}

	stores := workflowStores(state)
	if len(stores) != 1 {
		var refs []string
		for _, s := range stores {
			refs = append(refs, s.ref)
		}
		t.Fatalf("workflowStores refs = %v, want a single graph entry", refs)
	}
	if stores[0].ref != "graph:test-city" {
		t.Fatalf("ref = %q, want graph:test-city", stores[0].ref)
	}
}

func TestBuildWorkflowRunProjectionsNoDuplicateForOverlayCity(t *testing.T) {
	state := newFakeState(t)
	city := beads.NewMemStore()
	state.cityBeadStore = city
	state.graphBeadStore = &fakeOverlayGraphStore{Store: city, journal: beads.NewMemStore()}
	state.stores = map[string]beads.Store{}

	root := mustCreateWorkflowRoot(t, city, "Deploy")

	got, err := buildWorkflowRunProjections(state, "city", "test-city", "")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 1 {
		t.Fatalf("items = %d, want 1 (overlay must not double-project legacy roots)", len(got.Items))
	}
	if got.Items[0].RootBeadID != root.ID {
		t.Fatalf("root bead id = %q, want %q", got.Items[0].RootBeadID, root.ID)
	}
	if got.Items[0].RootStoreRef != "graph:test-city" {
		t.Fatalf("root store ref = %q, want stable graph:test-city", got.Items[0].RootStoreRef)
	}
}

// TestBuildWorkflowRunProjectionsOverlayIncludesJournalRoot proves the overlay
// path still surfaces journal-resident roots (fan-out), each exactly once.
func TestBuildWorkflowRunProjectionsOverlayIncludesJournalRoot(t *testing.T) {
	state := newFakeState(t)
	city := beads.NewMemStore()
	// The journal leg mints structurally-disjoint ids in production (gcg-j… vs
	// bd's shape); offset the test leg's sequence so the two never collide, the
	// same discipline the residence-router tests use.
	journal := beads.NewMemStoreFrom(1000, nil, nil)
	state.cityBeadStore = city
	state.graphBeadStore = &fakeOverlayGraphStore{Store: city, journal: journal}
	state.stores = map[string]beads.Store{}

	legacyRoot := mustCreateWorkflowRoot(t, city, "LegacyRun")
	journalRoot := mustCreateWorkflowRoot(t, journal, "JournalRun")

	got, err := buildWorkflowRunProjections(state, "city", "test-city", "")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2 (one legacy, one journal, no duplicates)", len(got.Items))
	}
	seen := map[string]int{}
	for _, item := range got.Items {
		seen[item.RootBeadID]++
		if item.RootStoreRef != "graph:test-city" {
			t.Fatalf("root %s store ref = %q, want graph:test-city", item.RootBeadID, item.RootStoreRef)
		}
	}
	if seen[legacyRoot.ID] != 1 || seen[journalRoot.ID] != 1 {
		t.Fatalf("root visit counts = %v, want each once", seen)
	}
}

func TestHumaHandleWorkflowDeleteSingleVisitForOverlayCity(t *testing.T) {
	state := newFakeState(t)
	city := &closeVisitCountingStore{Store: beads.NewMemStore()}
	state.cityBeadStore = city
	state.graphBeadStore = &fakeOverlayGraphStore{Store: city, journal: beads.NewMemStore()}
	state.stores = map[string]beads.Store{}

	root := mustCreateWorkflowRoot(t, city, "Deploy")

	srv := New(state)
	resp, err := srv.humaHandleWorkflowDelete(context.Background(), &WorkflowDeleteInput{
		WorkflowID: root.ID,
		Delete:     true,
	})
	if err != nil {
		t.Fatalf("humaHandleWorkflowDelete: %v", err)
	}
	if city.closeAllCalls != 1 {
		t.Fatalf("CloseAll invoked %d times, want 1 (root must be visited once)", city.closeAllCalls)
	}
	if resp.Body.Closed != 1 {
		t.Fatalf("closed = %d, want 1", resp.Body.Closed)
	}
	if resp.Body.Deleted != 1 {
		t.Fatalf("deleted = %d, want 1", resp.Body.Deleted)
	}
	if city.deleteCalls != 1 {
		t.Fatalf("Delete invoked %d times, want 1", city.deleteCalls)
	}
}

// TestWorkflowStoresKeepsBothForDisjointRelocatedGraph proves a graph-store
// split (relocated but NOT an overlay) is unchanged: both graph and city
// entries remain, and each disjoint root is projected once under its own store.
func TestWorkflowStoresKeepsBothForDisjointRelocatedGraph(t *testing.T) {
	state := newFakeState(t)
	city := beads.NewMemStore()
	// A relocated (split) graph store owns a disjoint id-space from the city
	// store; offset the sequence so the two roots do not collide.
	graph := beads.NewMemStoreFrom(1000, nil, nil) // plain store: does NOT implement StoreOverlay
	state.cityBeadStore = city
	state.graphBeadStore = graph
	state.stores = map[string]beads.Store{}

	graphRoot := mustCreateWorkflowRoot(t, graph, "GraphRun")
	cityRoot := mustCreateWorkflowRoot(t, city, "CityRun")

	stores := workflowStores(state)
	if len(stores) != 2 {
		t.Fatalf("workflowStores len = %d, want 2 (graph + city kept for disjoint split)", len(stores))
	}
	if stores[0].ref != "graph:test-city" || stores[1].ref != "city:test-city" {
		t.Fatalf("refs = [%q %q], want [graph:test-city city:test-city]", stores[0].ref, stores[1].ref)
	}

	got, err := buildWorkflowRunProjections(state, "city", "test-city", "")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 2 {
		t.Fatalf("items = %d, want 2", len(got.Items))
	}
	refByRoot := map[string]string{}
	for _, item := range got.Items {
		refByRoot[item.RootBeadID] = item.RootStoreRef
	}
	if refByRoot[graphRoot.ID] != "graph:test-city" {
		t.Fatalf("graph root ref = %q, want graph:test-city", refByRoot[graphRoot.ID])
	}
	if refByRoot[cityRoot.ID] != "city:test-city" {
		t.Fatalf("city root ref = %q, want city:test-city", refByRoot[cityRoot.ID])
	}
}

// TestWorkflowStoresDefaultCityByteIdentical proves a non-relocated city
// (GraphBeadStore() == CityBeadStore()) yields only the city entry — no graph
// entry, no behavior change.
func TestWorkflowStoresDefaultCityByteIdentical(t *testing.T) {
	state := newFakeState(t)
	city := beads.NewMemStore()
	state.cityBeadStore = city
	state.graphBeadStore = nil // GraphBeadStore falls back to cityBeadStore
	state.stores = map[string]beads.Store{}

	root := mustCreateWorkflowRoot(t, city, "Deploy")

	stores := workflowStores(state)
	if len(stores) != 1 || stores[0].ref != "city:test-city" {
		t.Fatalf("workflowStores = %+v, want a single city:test-city entry", stores)
	}

	got, err := buildWorkflowRunProjections(state, "city", "test-city", "")
	if err != nil {
		t.Fatalf("buildWorkflowRunProjections: %v", err)
	}
	if len(got.Items) != 1 || got.Items[0].RootStoreRef != "city:test-city" {
		t.Fatalf("items = %+v, want one item with city:test-city ref", got.Items)
	}
	_ = root
}
