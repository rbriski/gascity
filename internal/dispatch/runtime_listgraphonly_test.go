package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
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

// twoBackendDispatchRouter builds a Router with a "mc" work backend and a "gcg"
// graph backend, mirroring the live control-dispatcher topology.
func twoBackendDispatchRouter(work, graph *listCountingStore) *coordrouter.Router {
	r := coordrouter.New(work)
	r.Register(coordclass.ClassGraph, graph)
	return r
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
	r := twoBackendDispatchRouter(work, graph)

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
	r := twoBackendDispatchRouter(work, graph)

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
		r := twoBackendDispatchRouter(work, graph)

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
