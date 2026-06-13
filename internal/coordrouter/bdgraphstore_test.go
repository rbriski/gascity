package coordrouter

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// storageFakeGraphStore extends fakeGraphStore with the optional
// beads.StorageGraphApplyStore capability, recording the tier it was asked for.
type storageFakeGraphStore struct {
	*fakeGraphStore
	storages []beads.StorageClass
}

func newStorageFakeGraphStore() *storageFakeGraphStore {
	return &storageFakeGraphStore{fakeGraphStore: newFakeGraphStore()}
}

func (s *storageFakeGraphStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	s.storages = append(s.storages, storage)
	return s.ApplyGraphPlan(ctx, plan)
}

func twoNodePlan() *beads.GraphApplyPlan {
	return &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "root", Title: "root"}, {Key: "step", Title: "step", ParentKey: "root"}}}
}

func TestBdGraphStoreDelegatesApplyGraphPlan(t *testing.T) {
	backend := newFakeGraphStore()
	gs := NewBdGraphStore(backend)

	res, err := gs.ApplyGraphPlan(context.Background(), twoNodePlan())
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if len(backend.applied) != 1 {
		t.Fatalf("backend received %d plans, want 1", len(backend.applied))
	}
	if err := beads.ValidateGraphApplyResult(twoNodePlan(), res); err != nil {
		t.Fatalf("result did not resolve every node key: %v", err)
	}
}

func TestBdGraphStoreApplyGraphPlanUnsupported(t *testing.T) {
	// A plain MemStore has no graph-apply capability.
	gs := NewBdGraphStore(beads.NewMemStore())

	if _, err := gs.ApplyGraphPlan(context.Background(), twoNodePlan()); err == nil {
		t.Fatal("ApplyGraphPlan on a non-graph backend = nil error, want failure")
	}
}

func TestBdGraphStoreWithStorageDelegatesWhenSupported(t *testing.T) {
	backend := newStorageFakeGraphStore()
	gs := NewBdGraphStore(backend)

	if _, err := gs.ApplyGraphPlanWithStorage(context.Background(), twoNodePlan(), beads.StorageEphemeral); err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	if len(backend.storages) != 1 || backend.storages[0] != beads.StorageEphemeral {
		t.Fatalf("backend tiers = %v, want [%q]", backend.storages, beads.StorageEphemeral)
	}
}

func TestBdGraphStoreWithStorageFallsBackForDefault(t *testing.T) {
	// fakeGraphStore implements ApplyGraphPlan but NOT StorageGraphApplyStore.
	backend := newFakeGraphStore()
	gs := NewBdGraphStore(backend)

	if _, err := gs.ApplyGraphPlanWithStorage(context.Background(), twoNodePlan(), beads.StorageDefault); err != nil {
		t.Fatalf("StorageDefault should fall back to the untiered pour, got: %v", err)
	}
	if len(backend.applied) != 1 {
		t.Fatalf("backend received %d plans, want 1 (fallback)", len(backend.applied))
	}
}

func TestBdGraphStoreWithStorageRefusesNonDefaultWhenUnsupported(t *testing.T) {
	backend := newFakeGraphStore() // no StorageGraphApplyStore
	gs := NewBdGraphStore(backend)

	_, err := gs.ApplyGraphPlanWithStorage(context.Background(), twoNodePlan(), beads.StorageEphemeral)
	if err == nil {
		t.Fatal("non-default tier on a tier-unaware backend = nil error, want failure (no silent downgrade)")
	}
	if len(backend.applied) != 0 {
		t.Fatalf("backend should not have applied the plan, got %d", len(backend.applied))
	}
}
