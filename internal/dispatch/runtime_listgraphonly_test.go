package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// listCountingStore wraps a MemStore, advertises a fixed IDPrefix, and counts the
// List calls it receives, so a dispatch routing test can prove the Dolt (work)
// leg is never forked for a graph-rooted scope query.
type listCountingStore struct {
	*beads.MemStore
	prefix    string
	listCalls int
}

func newListCountingStore(prefix string, seed []beads.Bead) *listCountingStore {
	return &listCountingStore{
		MemStore: beads.NewMemStoreFrom(0, seed, nil),
		prefix:   prefix,
	}
}

func (s *listCountingStore) IDPrefix() string { return s.prefix }

func (s *listCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.MemStore.List(query)
}

// twoBackendGraphOnlyStore advertises the beads.GraphOnlyListStore capability
// (ListGraphOnly + GraphIDPrefix) over a "gcg" graph backend and a "mc" work
// backend, so liveListForRoot exercises its graph-only fast path exactly as it
// did against the retired per-class Router. It mirrors the deleted Router's three
// load-bearing methods for this test:
//   - List federates both backends (dedup by id) — the reference set, and the
//     fallback liveListForRoot takes for a non-graph-rooted (mc-) query;
//   - ListGraphOnly reads the graph backend ALONE (the work leg never forked);
//   - GraphIDPrefix reports the graph backend's id prefix so the caller gates the
//     fast path to graph-rooted (gcg-) queries.
//
// It embeds the graph store to satisfy the rest of the beads.Store surface; only
// the federation-sensitive methods are overridden.
type twoBackendGraphOnlyStore struct {
	*listCountingStore // graph backend (also the base beads.Store)
	work               *listCountingStore
}

var _ beads.GraphOnlyListStore = (*twoBackendGraphOnlyStore)(nil)

// twoBackendDispatchStore builds a graph-only-list store with a "mc" work backend
// and a "gcg" graph backend, mirroring the live control-dispatcher topology.
func twoBackendDispatchStore(work, graph *listCountingStore) *twoBackendGraphOnlyStore {
	return &twoBackendGraphOnlyStore{listCountingStore: graph, work: work}
}

// List federates the work and graph backends, deduping by id — the same set a
// single store spanning both backends would return (the Router's federated List).
func (s *twoBackendGraphOnlyStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	workRows, err := s.work.List(query)
	if err != nil {
		return nil, err
	}
	graphRows, err := s.listCountingStore.List(query)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]bool, len(workRows)+len(graphRows))
	merged := make([]beads.Bead, 0, len(workRows)+len(graphRows))
	for _, rows := range [][]beads.Bead{workRows, graphRows} {
		for _, b := range rows {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, b)
		}
	}
	return merged, nil
}

// ListGraphOnly reads the graph backend alone — the work (Dolt) leg is never
// forked for a wholly graph-resident molecule.
func (s *twoBackendGraphOnlyStore) ListGraphOnly(query beads.ListQuery) ([]beads.Bead, error) {
	return s.listCountingStore.List(query)
}

// GraphIDPrefix reports the graph backend's id prefix so liveListForRoot gates
// its graph-only fast path to graph-rooted (gcg-) queries.
func (s *twoBackendGraphOnlyStore) GraphIDPrefix() string {
	return s.listCountingStore.IDPrefix()
}

func rootScopedQuery(rootID string) beads.ListQuery {
	return beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
	}
}

func TestLiveListForRootGraphRootedSkipsWorkLeg(t *testing.T) {
	members := []beads.Bead{
		{ID: "gcg-2", Title: "m2", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
		{ID: "gcg-3", Title: "m3", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
	}
	work := newListCountingStore("mc", nil)
	graph := newListCountingStore("gcg", members)
	r := twoBackendDispatchStore(work, graph)

	got, err := liveListForRoot(r, "gcg-1", rootScopedQuery("gcg-1"))
	if err != nil {
		t.Fatalf("liveListForRoot error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("liveListForRoot returned %d beads, want 2 (graph members)", len(got))
	}
	if work.listCalls != 0 {
		t.Errorf("work (Dolt) backend List called %d times for a gcg- root, want 0 (no bd fork)", work.listCalls)
	}
	if graph.listCalls != 1 {
		t.Errorf("graph backend List called %d times, want 1", graph.listCalls)
	}
}

func TestLiveListForRootWorkRootedFederates(t *testing.T) {
	work := newListCountingStore("mc", []beads.Bead{
		{ID: "mc-2", Title: "w2", Metadata: map[string]string{"gc.root_bead_id": "mc-1"}},
	})
	graph := newListCountingStore("gcg", nil)
	r := twoBackendDispatchStore(work, graph)

	got, err := liveListForRoot(r, "mc-1", rootScopedQuery("mc-1"))
	if err != nil {
		t.Fatalf("liveListForRoot error = %v, want nil", err)
	}
	if len(got) != 1 {
		t.Fatalf("liveListForRoot returned %d beads, want 1 (work member)", len(got))
	}
	// An mc- root has no graph-prefix match, so the graph-only fast path is NOT
	// taken: the federated List queries both backends.
	if work.listCalls != 1 {
		t.Errorf("work backend List called %d times, want 1 (federation)", work.listCalls)
	}
	if graph.listCalls != 1 {
		t.Errorf("graph backend List called %d times, want 1 (federation)", graph.listCalls)
	}
}

func TestLiveListForRootEquivalentToFederatedList(t *testing.T) {
	members := []beads.Bead{
		{ID: "gcg-2", Title: "m2", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
		{ID: "gcg-3", Title: "m3", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
		{ID: "mc-2", Title: "w2", Metadata: map[string]string{"gc.root_bead_id": "mc-1"}},
	}
	graphSeed := members[:2]
	workSeed := members[2:]

	for _, root := range []string{"gcg-1", "mc-1"} {
		work := newListCountingStore("mc", workSeed)
		graph := newListCountingStore("gcg", graphSeed)
		r := twoBackendDispatchStore(work, graph)

		fast, err := liveListForRoot(r, root, rootScopedQuery(root))
		if err != nil {
			t.Fatalf("liveListForRoot(%s) error = %v", root, err)
		}
		ref, err := r.List(rootScopedQuery(root))
		if err != nil {
			t.Fatalf("federated List(%s) error = %v", root, err)
		}
		if len(fast) != len(ref) {
			t.Fatalf("root %s: liveListForRoot len=%d, federated List len=%d — semantics not preserved", root, len(fast), len(ref))
		}
		fastIDs := map[string]bool{}
		for _, b := range fast {
			fastIDs[b.ID] = true
		}
		for _, b := range ref {
			if !fastIDs[b.ID] {
				t.Errorf("root %s: federated List has %q but liveListForRoot does not — sets differ", root, b.ID)
			}
		}
	}
}
