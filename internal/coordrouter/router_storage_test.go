package coordrouter

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// storageRecordingStore is a MemStore that records the storage tier passed to
// CreateWithStorage, so a test can assert the Router forwards the policy-selected
// tier to the routed backend.
type storageRecordingStore struct {
	*beads.MemStore
	storages []beads.StorageClass
}

func newStorageRecordingStore() *storageRecordingStore {
	return &storageRecordingStore{MemStore: beads.NewMemStore()}
}

func (s *storageRecordingStore) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	s.storages = append(s.storages, storage)
	return s.Create(b)
}

func TestRouterCreateWithStorageRoutesTierByClass(t *testing.T) {
	work := newStorageRecordingStore()
	graph := newStorageRecordingStore()
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	// A graph-class bead with an ephemeral tier routes to the graph backend,
	// carrying the tier.
	if _, err := r.CreateWithStorage(beads.Bead{Title: "g", Labels: []string{"gc:wisp"}}, beads.StorageEphemeral); err != nil {
		t.Fatalf("CreateWithStorage(graph): %v", err)
	}
	if len(graph.storages) != 1 || graph.storages[0] != beads.StorageEphemeral {
		t.Fatalf("graph backend tiers = %v, want [ephemeral]", graph.storages)
	}
	if len(work.storages) != 0 {
		t.Fatalf("work backend should not have received the graph create: %v", work.storages)
	}

	// A work-class bead routes to the work backend, carrying its tier.
	if _, err := r.CreateWithStorage(beads.Bead{Title: "w"}, beads.StorageNoHistory); err != nil {
		t.Fatalf("CreateWithStorage(work): %v", err)
	}
	if len(work.storages) != 1 || work.storages[0] != beads.StorageNoHistory {
		t.Fatalf("work backend tiers = %v, want [no_history]", work.storages)
	}
}

func TestRouterApplyGraphPlanWithStorageForwardsTierByClass(t *testing.T) {
	work := newRecordingStore()
	graph := newStorageFakeGraphStore()
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	applier, ok := beads.GraphApplyFor(r)
	if !ok {
		t.Fatal("router should expose graph apply when the graph backend supports it")
	}
	storageApplier, ok := applier.(beads.StorageGraphApplyStore)
	if !ok {
		t.Fatal("routed applier should satisfy StorageGraphApplyStore")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "root", Labels: []string{"gc:wisp"}},
	}}
	if _, err := storageApplier.ApplyGraphPlanWithStorage(context.Background(), plan, beads.StorageEphemeral); err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	if len(graph.storages) != 1 || graph.storages[0] != beads.StorageEphemeral {
		t.Fatalf("graph backend tiers = %v, want [ephemeral]", graph.storages)
	}
}
