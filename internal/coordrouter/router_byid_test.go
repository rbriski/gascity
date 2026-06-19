package coordrouter

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// prefixSpyStore wraps a MemStore, advertises a fixed IDPrefix, and counts the
// read calls (Get/DepList/List) it receives. It lets a test prove that a read is
// routed to the owning backend and never forked into the non-owning one.
type prefixSpyStore struct {
	*beads.MemStore
	prefix       string
	getCalls     int
	depListCalls int
	listCalls    int
}

func newPrefixSpyStore(prefix string, beadsIn []beads.Bead, deps []beads.Dep) *prefixSpyStore {
	return &prefixSpyStore{
		MemStore: beads.NewMemStoreFrom(0, beadsIn, deps),
		prefix:   prefix,
	}
}

// IDPrefix reports the prefix this spy owns, satisfying the optional accessor
// that backendForID looks for.
func (s *prefixSpyStore) IDPrefix() string { return s.prefix }

func (s *prefixSpyStore) Get(id string) (beads.Bead, error) {
	s.getCalls++
	return s.MemStore.Get(id)
}

func (s *prefixSpyStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return s.MemStore.DepList(id, direction)
}

func (s *prefixSpyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.MemStore.List(query)
}

// twoBackendRouter builds a Router with a "mc"-prefixed work store and a
// "gcg"-prefixed graph store, mirroring the live control-dispatcher topology
// (Dolt work registered first, SQLite graph second).
func twoBackendRouter(work, graph *prefixSpyStore) *Router {
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)
	return r
}

func TestGetRoutesGraphIDToGraphBackendOnly(t *testing.T) {
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", []beads.Bead{{ID: "gcg-1", Title: "g"}}, nil)
	r := twoBackendRouter(work, graph)

	got, err := r.Get("gcg-1")
	if err != nil {
		t.Fatalf("Get(gcg-1) error = %v, want nil", err)
	}
	if got.ID != "gcg-1" {
		t.Errorf("Get(gcg-1).ID = %q, want gcg-1", got.ID)
	}
	if work.getCalls != 0 {
		t.Errorf("work backend Get called %d times, want 0 (must not fork the non-owning store)", work.getCalls)
	}
	if graph.getCalls != 1 {
		t.Errorf("graph backend Get called %d times, want 1", graph.getCalls)
	}
}

func TestGetRoutesWorkIDToWorkBackendOnly(t *testing.T) {
	work := newPrefixSpyStore("mc", []beads.Bead{{ID: "mc-1", Title: "w"}}, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	got, err := r.Get("mc-1")
	if err != nil {
		t.Fatalf("Get(mc-1) error = %v, want nil", err)
	}
	if got.ID != "mc-1" {
		t.Errorf("Get(mc-1).ID = %q, want mc-1", got.ID)
	}
	if graph.getCalls != 0 {
		t.Errorf("graph backend Get called %d times, want 0 (must not fork the non-owning store)", graph.getCalls)
	}
	if work.getCalls != 1 {
		t.Errorf("work backend Get called %d times, want 1", work.getCalls)
	}
}

func TestGetUnknownPrefixFallsBackToFederation(t *testing.T) {
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	_, err := r.Get("zz-1")
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(zz-1) error = %v, want ErrNotFound", err)
	}
	// No backend owns "zz", so backendForID returns nil and full federation runs:
	// every backend must be queried.
	if work.getCalls != 1 {
		t.Errorf("work backend Get called %d times, want 1 (federation fallback)", work.getCalls)
	}
	if graph.getCalls != 1 {
		t.Errorf("graph backend Get called %d times, want 1 (federation fallback)", graph.getCalls)
	}
}

func TestGetOwnerMissFallsBackToFederation(t *testing.T) {
	// gcg-9 matches the graph prefix but is absent everywhere: the owner fast
	// path misses, then full federation runs and still resolves correctly.
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	_, err := r.Get("gcg-9")
	if !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(gcg-9) error = %v, want ErrNotFound", err)
	}
	// Owner (graph) queried once on the fast path, then again in federation;
	// work is queried once in federation. Correctness preserved.
	if graph.getCalls < 1 {
		t.Errorf("graph backend Get called %d times, want >=1", graph.getCalls)
	}
	if work.getCalls != 1 {
		t.Errorf("work backend Get called %d times, want 1 (federation fallback after owner miss)", work.getCalls)
	}
}

func TestGetOwnerMissResolvesFromOtherBackend(t *testing.T) {
	// A gcg-prefixed id that, due to a partial migration, actually lives in the
	// work store. The owner (graph) misses, federation finds it in work.
	work := newPrefixSpyStore("mc", []beads.Bead{{ID: "gcg-7", Title: "stray"}}, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	got, err := r.Get("gcg-7")
	if err != nil {
		t.Fatalf("Get(gcg-7) error = %v, want nil (federation must find the stray bead)", err)
	}
	if got.ID != "gcg-7" {
		t.Errorf("Get(gcg-7).ID = %q, want gcg-7", got.ID)
	}
}

func TestDepListRoutesGraphIDToGraphBackendOnly(t *testing.T) {
	dep := beads.Dep{IssueID: "gcg-1", DependsOnID: "gcg-2", Type: "blocks"}
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", nil, []beads.Dep{dep})
	r := twoBackendRouter(work, graph)

	deps, err := r.DepList("gcg-1", "down")
	if err != nil {
		t.Fatalf("DepList(gcg-1) error = %v, want nil", err)
	}
	if len(deps) != 1 || deps[0] != dep {
		t.Errorf("DepList(gcg-1) = %v, want [%v]", deps, dep)
	}
	if work.depListCalls != 0 {
		t.Errorf("work backend DepList called %d times, want 0 (must not fork the non-owning store)", work.depListCalls)
	}
	if graph.depListCalls != 1 {
		t.Errorf("graph backend DepList called %d times, want 1", graph.depListCalls)
	}
}

func TestDepListEmptyOwnerFallsBackToFederation(t *testing.T) {
	// The graph owner has no dep for gcg-3, but a cross-store edge is recorded in
	// the work store. Federation fallback must surface it.
	crossEdge := beads.Dep{IssueID: "gcg-3", DependsOnID: "mc-5", Type: "blocks"}
	work := newPrefixSpyStore("mc", nil, []beads.Dep{crossEdge})
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	deps, err := r.DepList("gcg-3", "down")
	if err != nil {
		t.Fatalf("DepList(gcg-3) error = %v, want nil", err)
	}
	if len(deps) != 1 || deps[0] != crossEdge {
		t.Errorf("DepList(gcg-3) = %v, want [%v]", deps, crossEdge)
	}
	if work.depListCalls == 0 {
		t.Errorf("work backend DepList called %d times, want >=1 (federation fallback must query work)", work.depListCalls)
	}
}

func TestPrefixBackendForIDReturnsNilForSoleBackend(t *testing.T) {
	work := newPrefixSpyStore("mc", []beads.Bead{{ID: "mc-1", Title: "w"}}, nil)
	r := New(work) // single backend → identity phase

	if owner := r.prefixBackendForID("mc-1"); owner != nil {
		t.Errorf("prefixBackendForID on sole-backend router = %v, want nil", owner)
	}
	// Behavior must be byte-identical to direct delegation: Get still works.
	got, err := r.Get("mc-1")
	if err != nil {
		t.Fatalf("Get(mc-1) on sole-backend router error = %v, want nil", err)
	}
	if got.ID != "mc-1" {
		t.Errorf("Get(mc-1).ID = %q, want mc-1", got.ID)
	}
	// The sole-backend path is reached, not the by-id fast path: one Get call.
	if work.getCalls != 1 {
		t.Errorf("work backend Get called %d times, want 1", work.getCalls)
	}
}

func TestByIDFastPathEquivalentToFederation(t *testing.T) {
	// Build two identically populated 2-backend routers. Compare the fast-path
	// Router's results against a reference that always federates, proving the
	// fast path is a strict superset (identical values, fewer backend calls).
	seedBeads := []beads.Bead{{ID: "gcg-1", Title: "g1"}, {ID: "gcg-2", Title: "g2"}}
	seedDeps := []beads.Dep{{IssueID: "gcg-1", DependsOnID: "gcg-2", Type: "blocks"}}

	work := newPrefixSpyStore("mc", []beads.Bead{{ID: "mc-1", Title: "w1"}}, nil)
	graph := newPrefixSpyStore("gcg", seedBeads, seedDeps)
	fast := twoBackendRouter(work, graph)

	// Reference router with NON-prefix-advertising stores forces pure federation.
	refWork := beads.NewMemStoreFrom(0, []beads.Bead{{ID: "mc-1", Title: "w1"}}, nil)
	refGraph := beads.NewMemStoreFrom(0, seedBeads, seedDeps)
	ref := New(refWork)
	ref.Register(coordclass.ClassGraph, refGraph)

	for _, id := range []string{"gcg-1", "gcg-2", "mc-1"} {
		fastGot, fastErr := fast.Get(id)
		refGot, refErr := ref.Get(id)
		if (fastErr == nil) != (refErr == nil) {
			t.Errorf("Get(%s): fast err=%v, ref err=%v — mismatch", id, fastErr, refErr)
		}
		if fastGot.ID != refGot.ID {
			t.Errorf("Get(%s): fast ID=%q, ref ID=%q — not equivalent", id, fastGot.ID, refGot.ID)
		}
	}

	fastDeps, err := fast.DepList("gcg-1", "down")
	if err != nil {
		t.Fatal(err)
	}
	refDeps, err := ref.DepList("gcg-1", "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(fastDeps) != len(refDeps) {
		t.Fatalf("DepList(gcg-1): fast len=%d, ref len=%d", len(fastDeps), len(refDeps))
	}
	for i := range fastDeps {
		if fastDeps[i] != refDeps[i] {
			t.Errorf("DepList(gcg-1)[%d]: fast=%v, ref=%v", i, fastDeps[i], refDeps[i])
		}
	}
}

func TestMutationRoutesGraphIDToGraphWithoutProbingWork(t *testing.T) {
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", []beads.Bead{{ID: "gcg-1", Title: "g", Status: "open"}}, nil)
	r := twoBackendRouter(work, graph)

	if err := r.Close("gcg-1"); err != nil {
		t.Fatalf("Close(gcg-1) error = %v, want nil", err)
	}
	if work.getCalls != 0 {
		t.Errorf("work backend Get probed %d times during mutation routing, want 0 (no bd fork for a graph-class id)", work.getCalls)
	}
	got, err := graph.MemStore.Get("gcg-1")
	if err != nil {
		t.Fatalf("graph Get after close: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("gcg-1 status = %q after Close, want closed", got.Status)
	}
}

func TestMutationStrayGraphIDFallsBackToFederation(t *testing.T) {
	// A gcg-prefixed id that physically lives in the work store (stray / partial
	// migration): the prefix owner (graph) Get misses, so routing falls back to
	// the full federated probe and still finds + mutates it in the work store.
	work := newPrefixSpyStore("mc", []beads.Bead{{ID: "gcg-stray", Title: "s", Status: "open"}}, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	if err := r.Close("gcg-stray"); err != nil {
		t.Fatalf("Close(gcg-stray) should fall back to federation: %v", err)
	}
	got, err := work.MemStore.Get("gcg-stray")
	if err != nil {
		t.Fatalf("work Get after fallback close: %v", err)
	}
	if got.Status != "closed" {
		t.Errorf("stray gcg- in work store not closed via federation fallback (status=%q)", got.Status)
	}
}
