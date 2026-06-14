package beads

import (
	"context"
	"testing"
)

func openTestSQLiteStore(t *testing.T) *SQLiteStore {
	t.Helper()
	store, err := OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	s := store.(*SQLiteStore)
	t.Cleanup(func() { _ = s.CloseStore() })
	return s
}

func TestSQLiteApplyGraphPlanResolvesPersistsAndWiresEdges(t *testing.T) {
	s := openTestSQLiteStore(t)
	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root", Type: "task"},
			{Key: "step", Title: "step", Type: "task", ParentKey: "root"},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "step", ToKey: "root", Type: "blocks"},
		},
	}
	res, err := s.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if err := ValidateGraphApplyResult(plan, res); err != nil {
		t.Fatalf("result must resolve every node key: %v", err)
	}
	rootID, stepID := res.IDs["root"], res.IDs["step"]

	// Both nodes persisted and retrievable.
	if _, err := s.Get(rootID); err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	step, err := s.Get(stepID)
	if err != nil {
		t.Fatalf("Get(step): %v", err)
	}
	// Parent relationship rides the parent_id column.
	if step.ParentID != rootID {
		t.Fatalf("step.ParentID = %q, want %q", step.ParentID, rootID)
	}
	// Edge materialized as a dep row resolvable by DepList.
	deps, err := s.DepList(stepID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != rootID || deps[0].Type != "blocks" {
		t.Fatalf("DepList(step) = %+v, want one blocks->root", deps)
	}
}

func TestSQLiteApplyGraphPlanEmptyIsAccepted(t *testing.T) {
	s := openTestSQLiteStore(t)
	res, err := s.ApplyGraphPlan(context.Background(), &GraphApplyPlan{})
	if err != nil {
		t.Fatalf("ApplyGraphPlan(empty) = %v, want nil", err)
	}
	if len(res.IDs) != 0 {
		t.Fatalf("empty plan produced IDs: %v", res.IDs)
	}
}

func TestSQLiteApplyGraphPlanIsAtomicOnBadEdge(t *testing.T) {
	s := openTestSQLiteStore(t)
	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{{Key: "a", Title: "a", Type: "task"}},
		Edges: []GraphApplyEdge{{FromKey: "a", ToKey: "missing", Type: "blocks"}},
	}
	if _, err := s.ApplyGraphPlan(context.Background(), plan); err == nil {
		t.Fatal("ApplyGraphPlan with an unresolved edge = nil error, want failure")
	}
	// Atomicity: the node must NOT have been persisted (the whole tx rolled back).
	all, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected zero beads after rollback, got %d", len(all))
	}
}

func TestSQLiteApplyGraphPlanWithStorageEphemeral(t *testing.T) {
	s := openTestSQLiteStore(t)
	plan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "w", Title: "wisp", Type: "task"}}}
	res, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, StorageEphemeral)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage(ephemeral): %v", err)
	}
	got, err := s.Get(res.IDs["w"])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Ephemeral {
		t.Fatalf("ephemeral-tier pour produced a non-ephemeral bead: %+v", got)
	}
}
