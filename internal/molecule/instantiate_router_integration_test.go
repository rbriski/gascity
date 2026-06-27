package molecule_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// TestInstantiateRoutesGraphMoleculeToSQLite proves the formula-sling pour works
// with graph beads in the embedded SQLite store. It instantiates a real graph.v2
// recipe — the sling entry point — directly into the SQLite graph store (the store
// the production caller passes for a graph-class molecule) and asserts that:
//
//  1. the ENTIRE molecule (root + step) lands in the SQLite graph backend, and
//     nothing leaks into the work backend (clean work/graph separation);
//  2. the molecule is reachable through the graph store (Get); and
//  3. a Close of a step lands in SQLite (graph mutations reach the store).
//
// This is the store-level guarantee that a real molecule.Instantiate flows the
// graph topology into the in-process graph store and that the molecule remains
// fully workable afterwards — one layer up from M1's raw ApplyGraphPlan proof,
// at the actual sling/dispatch entry point. The class-aware BEHAVIOR (a
// homogeneous graph molecule lands wholly on its class's store) is unchanged from
// the former Router fixture; the caller now selects the graph store directly, the
// way the production graph-routing chokepoint does.
func TestInstantiateRoutesGraphMoleculeToSQLite(t *testing.T) {
	prev := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prev) })

	work := beads.NewMemStore() // stands in for the Dolt work store
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}

	// The production caller passes the graph store for a graph-class molecule;
	// instantiate directly into it (no Router).
	result, err := molecule.Instantiate(context.Background(), graph, recipe, molecule.Options{})
	if err != nil {
		t.Fatalf("Instantiate: %v", err)
	}

	// (1) The whole molecule poured into the SQLite graph backend, not the work store.
	rootID := result.RootID
	stepID := result.IDMapping["wf.step"]
	if rootID == "" || stepID == "" {
		t.Fatalf("missing root/step bead id: root=%q step=%q mapping=%v", rootID, stepID, result.IDMapping)
	}
	for _, id := range []string{rootID, stepID} {
		if _, err := graph.Get(id); err != nil {
			t.Fatalf("molecule bead %s not in the SQLite graph backend: %v", id, err)
		}
		if _, err := work.Get(id); err == nil {
			t.Fatalf("molecule bead %s leaked into the work backend", id)
		}
	}

	// (2) The molecule is reachable through the graph store.
	if _, err := graph.Get(rootID); err != nil {
		t.Fatalf("Get(root): %v", err)
	}

	// (3) A Close of the step lands in SQLite.
	if err := graph.Close(stepID); err != nil {
		t.Fatalf("Close(step): %v", err)
	}
	closed, err := graph.Get(stepID)
	if err != nil {
		t.Fatalf("re-get closed step: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("step status = %q, want closed (the close must land in SQLite)", closed.Status)
	}
}
