package beads_test

import (
	"context"
	"os"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// openPostgresGraphStore returns a fresh, truncated Postgres store in its own
// schema, or (nil,false) when GC_TEST_POSTGRES_DSN is unset. These white-box
// tests exercise the graph-apply paths the thin shared conformance suite
// (coordtest.RunGraphStoreTests) does not reach — edge wiring, parent linkage,
// transactional atomicity, and storage-tier selection — proving the Postgres
// ApplyGraphPlan matches the SQLite analog (sqlite_store_graph_apply_test.go) on
// every substantive behavior, not just key resolution.
func openPostgresGraphStore(t *testing.T) (*beads.PostgresStore, bool) {
	t.Helper()
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		return nil, false
	}
	s := openPostgresSchema(t, dsn, "gcg_graphapply", "gcg")
	return s.(*beads.PostgresStore), true
}

func TestPostgresApplyGraphPlanResolvesPersistsAndWiresEdges(t *testing.T) {
	s, ok := openPostgresGraphStore(t)
	if !ok {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	plan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{
			{Key: "root", Title: "root", Type: "task"},
			{Key: "step", Title: "step", Type: "task", ParentKey: "root"},
		},
		Edges: []beads.GraphApplyEdge{
			{FromKey: "step", ToKey: "root", Type: "blocks"},
		},
	}
	res, err := s.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if err := beads.ValidateGraphApplyResult(plan, res); err != nil {
		t.Fatalf("result must resolve every node key: %v", err)
	}
	rootID, stepID := res.IDs["root"], res.IDs["step"]

	if _, err := s.Get(rootID); err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	step, err := s.Get(stepID)
	if err != nil {
		t.Fatalf("Get(step): %v", err)
	}
	// Parent relationship rides the parent_id column (matching Create + Children).
	if step.ParentID != rootID {
		t.Fatalf("step.ParentID = %q, want %q", step.ParentID, rootID)
	}
	// Edge materialized as a dep row resolvable by DepList, identically to SQLite.
	deps, err := s.DepList(stepID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != rootID || deps[0].Type != "blocks" {
		t.Fatalf("DepList(step) = %+v, want one blocks->root", deps)
	}
}

func TestPostgresApplyGraphPlanIsAtomicOnBadEdge(t *testing.T) {
	s, ok := openPostgresGraphStore(t)
	if !ok {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	plan := &beads.GraphApplyPlan{
		Nodes: []beads.GraphApplyNode{{Key: "a", Title: "a", Type: "task"}},
		Edges: []beads.GraphApplyEdge{{FromKey: "a", ToKey: "missing", Type: "blocks"}},
	}
	if _, err := s.ApplyGraphPlan(context.Background(), plan); err == nil {
		t.Fatal("ApplyGraphPlan with an unresolved edge = nil error, want failure")
	}
	// Atomicity: the node must NOT have been persisted (the whole tx rolled back).
	all, err := s.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("expected zero beads after rollback, got %d", len(all))
	}
}

func TestPostgresApplyGraphPlanWithStorageEphemeral(t *testing.T) {
	s, ok := openPostgresGraphStore(t)
	if !ok {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{{Key: "w", Title: "wisp", Type: "task"}}}
	res, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, beads.StorageEphemeral)
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
