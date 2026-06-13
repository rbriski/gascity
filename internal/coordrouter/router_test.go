package coordrouter

import (
	"context"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// recordingStore wraps a MemStore and records the beads passed to Create, so a
// test can assert which backend a routed create landed on.
type recordingStore struct {
	*beads.MemStore
	creates []beads.Bead
}

func newRecordingStore() *recordingStore {
	return &recordingStore{MemStore: beads.NewMemStore()}
}

func (s *recordingStore) Create(b beads.Bead) (beads.Bead, error) {
	s.creates = append(s.creates, b)
	return s.MemStore.Create(b)
}

// fakeGraphStore implements beads.GraphApplyStore over a MemStore and records
// the plans it receives.
type fakeGraphStore struct {
	*beads.MemStore
	applied []*beads.GraphApplyPlan
}

func newFakeGraphStore() *fakeGraphStore {
	return &fakeGraphStore{MemStore: beads.NewMemStore()}
}

func (s *fakeGraphStore) ApplyGraphPlan(_ context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	s.applied = append(s.applied, plan)
	ids := make(map[string]string, len(plan.Nodes))
	for i, n := range plan.Nodes {
		ids[n.Key] = fmt.Sprintf("g-%d", i)
	}
	return &beads.GraphApplyResult{IDs: ids}, nil
}

func TestRouterCreateRoutesByClass(t *testing.T) {
	work := newRecordingStore()
	graph := newRecordingStore()
	msg := newRecordingStore()
	sess := newRecordingStore()
	ord := newRecordingStore()
	nudge := newRecordingStore()

	r := New(work)
	r.Register(coordclass.ClassGraph, graph)
	r.Register(coordclass.ClassMessaging, msg)
	r.Register(coordclass.ClassSessions, sess)
	r.Register(coordclass.ClassOrders, ord)
	r.Register(coordclass.ClassNudges, nudge)

	cases := []struct {
		name string
		bead beads.Bead
		want *recordingStore
	}{
		{"task", beads.Bead{Type: "task", Title: "t"}, work},
		{"graph child", beads.Bead{Type: "step", Title: "g", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gc-1"}}, graph},
		{"mail", beads.Bead{Type: "message", Title: "m"}, msg},
		{"session", beads.Bead{Type: "session", Title: "s"}, sess},
		{"order-tracking", beads.Bead{Type: "task", Title: "o", Labels: []string{"order-tracking"}}, ord},
		{"nudge", beads.Bead{Type: "chore", Title: "n", Labels: []string{"gc:nudge"}}, nudge},
	}

	all := []*recordingStore{work, graph, msg, sess, ord, nudge}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := map[*recordingStore]int{}
			for _, s := range all {
				before[s] = len(s.creates)
			}
			if _, err := r.Create(tc.bead); err != nil {
				t.Fatal(err)
			}
			for _, s := range all {
				delta := len(s.creates) - before[s]
				wantDelta := 0
				if s == tc.want {
					wantDelta = 1
				}
				if delta != wantDelta {
					t.Errorf("backend create delta = %d, want %d (bead %q routed to wrong backend)", delta, wantDelta, tc.name)
				}
			}
		})
	}
}

func TestRouterBackendFallsBackToPrimary(t *testing.T) {
	work := beads.NewMemStore()
	r := New(work)
	// No backend registered for ClassOrders → falls back to the primary.
	if got := r.Backend(coordclass.ClassOrders); got != beads.Store(work) {
		t.Errorf("Backend(orders) did not fall back to primary work store")
	}
	if got := r.Backend(coordclass.ClassWork); got != beads.Store(work) {
		t.Errorf("Backend(work) = %v, want primary", got)
	}
}

func TestRouterIsIdentityTransformWhenSingleBackend(t *testing.T) {
	mem := beads.NewMemStore()
	r := New(mem) // every class falls back to the one backend

	// Create one bead of each class through the router.
	beadsToCreate := []beads.Bead{
		{Type: "task", Title: "work"},
		{Type: "molecule", Title: "graph", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{Type: "message", Title: "mail"},
		{Type: "session", Title: "sess"},
		{Type: "task", Title: "order", Labels: []string{"order-tracking"}},
		{Type: "chore", Title: "nudge", Labels: []string{"gc:nudge"}},
	}
	for _, b := range beadsToCreate {
		if _, err := r.Create(b); err != nil {
			t.Fatal(err)
		}
	}

	// All of them must be in the single backing store, and a delegated read
	// through the router must equal a direct read of the backend.
	direct, err := mem.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	viaRouter, err := r.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	if len(direct) != len(beadsToCreate) {
		t.Fatalf("backend has %d beads, want %d", len(direct), len(beadsToCreate))
	}
	if len(viaRouter) != len(direct) {
		t.Errorf("router List returned %d beads, backend List returned %d — not an identity transform", len(viaRouter), len(direct))
	}
}

func TestRouterGraphApplyRoutesToGraphBackend(t *testing.T) {
	work := beads.NewMemStore()
	graph := newFakeGraphStore()
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	applier, ok := beads.GraphApplyFor(r)
	if !ok {
		t.Fatal("GraphApplyFor(router) = false, want true when graph backend supports graph apply")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
		{Key: "s1", Type: "step", ParentKey: "root", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "root"}},
	}}
	if _, err := applier.ApplyGraphPlan(context.Background(), plan); err != nil {
		t.Fatal(err)
	}
	if len(graph.applied) != 1 {
		t.Errorf("graph backend received %d plans, want 1 (plan routed to wrong backend)", len(graph.applied))
	}
}

func TestRouterGraphApplyAbsentWhenBackendUnsupported(t *testing.T) {
	// MemStore does not implement GraphApplyStore, and ClassGraph is unregistered
	// so it falls back to the primary MemStore.
	r := New(beads.NewMemStore())
	if _, ok := beads.GraphApplyFor(r); ok {
		t.Error("GraphApplyFor(router) = true, want false when no backend supports graph apply")
	}
}
