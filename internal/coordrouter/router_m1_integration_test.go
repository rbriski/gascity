package coordrouter

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// TestM1StoreLevelFederationProof is the M1 milestone: a Router over a real split
// — work beads in one backend, graph beads in the embedded SQLite store — proves
// end to end that (1) a graph pour lands in SQLite and a work create does not,
// (2) federated reads find a bead in whichever backend owns it, and (3) by-id
// mutations route to the owning backend. No production wiring; the earliest proof
// that the work/graph store split actually works.
func TestM1StoreLevelFederationProof(t *testing.T) {
	work := beads.NewMemStore() // stands in for the Dolt work store (distinct id namespace: bd-*)
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore) // ids: gc-*
	t.Cleanup(func() { _ = graph.CloseStore() })

	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	// (1a) A work-class bead routes to the work backend, not the graph store.
	wb, err := r.Create(beads.Bead{Title: "backlog task", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if _, err := work.Get(wb.ID); err != nil {
		t.Fatalf("work bead not in the work backend: %v", err)
	}
	if _, err := graph.Get(wb.ID); err == nil {
		t.Fatal("work bead leaked into the SQLite graph store")
	}

	// (1b) A graph pour routes to the SQLite graph store via the routed applier.
	applier, ok := beads.GraphApplyFor(r)
	if !ok {
		t.Fatal("router should expose graph apply when the graph backend supports it")
	}
	plan := &beads.GraphApplyPlan{Nodes: []beads.GraphApplyNode{
		{Key: "root", Title: "workflow root", Labels: []string{"gc:wisp"}},
		{Key: "step", Title: "actionable step", Type: "task", Labels: []string{"gc:wisp"}, ParentKey: "root"},
	}}
	res, err := applier.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	stepID := res.IDs["step"]
	if _, err := graph.Get(stepID); err != nil {
		t.Fatalf("graph step not in the SQLite graph store: %v", err)
	}
	if _, err := work.Get(stepID); err == nil {
		t.Fatal("graph step leaked into the work backend")
	}

	// (2) Federated reads find a bead in whichever backend owns it.
	if _, err := r.Get(wb.ID); err != nil {
		t.Fatalf("federated Get(work bead): %v", err)
	}
	if _, err := r.Get(stepID); err != nil {
		t.Fatalf("federated Get(graph step): %v", err)
	}

	// (3a) A by-id update routes to the owning (SQLite) backend.
	assignee := "worker-1"
	if err := r.Update(stepID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("routed Update(graph step): %v", err)
	}
	got, err := graph.Get(stepID)
	if err != nil {
		t.Fatalf("re-get step: %v", err)
	}
	if got.Assignee != assignee {
		t.Fatalf("update did not land in SQLite: assignee=%q want %q", got.Assignee, assignee)
	}

	// (3b) A by-id close routes to the owning (SQLite) backend.
	if err := r.Close(stepID); err != nil {
		t.Fatalf("routed Close(graph step): %v", err)
	}
	closed, err := graph.Get(stepID)
	if err != nil {
		t.Fatalf("re-get closed step: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("close did not land in SQLite: status=%q want closed", closed.Status)
	}
}
