package dispatch

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestProcessControlConvergesGraphMoleculeOnSQLiteGraphStore is the capstone
// proof for the work/graph split: the controller's REAL convergence engine
// (ProcessControl) drives a graph.v2 molecule to terminal entirely on the
// dedicated SQLite graph store — no agent, no bd subprocess, no tmux. It mirrors
// the zero-agent retry-eval-pass scenario (TestProcessRetryEvalPassClosesLogical)
// but the graph molecule is resident in SQLite while the (separate) work store is
// threaded only as opts.WorkStore — the post-Router GE shape: graph primary +
// work via ProcessOptions.WorkStore. It asserts the molecule lives in SQLite
// (never the work backend) and the engine's closures land in SQLite.
//
// This is the "a simple formula sling runs through the entire process with graph
// metadata in the in-process store" guarantee at the convergence-engine level: the
// same control-bead machinery that finishes a molecule in production operates
// correctly when graph beads are relocated to the SQLite graph store.
func TestProcessControlConvergesGraphMoleculeOnSQLiteGraphStore(t *testing.T) {
	work := beads.NewMemStore()
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	// The dispatcher's PRIMARY store is the graph store (control beads, retry
	// logicals, and the molecule itself are all graph-resident); the work store
	// is threaded only via opts.WorkStore for cross-class drain members. The
	// molecule is created directly on the graph store — the create-chokepoint's
	// job in production — so nothing graph-class ever touches the work store.
	store := graph

	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.run.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "pass",
			"gc.output_json":     `{"ok":true}`,
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.step_ref":        "demo.review.eval.1",
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	// The whole molecule lives in SQLite (graph class), not the work backend.
	for _, b := range []beads.Bead{root, logical, run1, eval1} {
		if _, err := graph.Get(b.ID); err != nil {
			t.Fatalf("molecule bead %s not in the SQLite graph store: %v", b.ID, err)
		}
		if _, err := work.Get(b.ID); err == nil {
			t.Fatalf("molecule bead %s leaked into the work backend", b.ID)
		}
	}

	// Drive the real convergence engine on the graph store, with the work store
	// threaded via opts.WorkStore (the GE control-dispatcher shape).
	result, err := ProcessControl(store, eval1, ProcessOptions{WorkStore: work})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval pass): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}

	// The engine's closures landed in SQLite (read straight from the graph backend).
	evalAfter, err := graph.Get(eval1.ID)
	if err != nil {
		t.Fatalf("re-get eval from SQLite: %v", err)
	}
	if evalAfter.Status != "closed" || evalAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("eval in SQLite = status %q outcome %q, want closed/pass", evalAfter.Status, evalAfter.Metadata["gc.outcome"])
	}
	logicalAfter, err := graph.Get(logical.ID)
	if err != nil {
		t.Fatalf("re-get logical from SQLite: %v", err)
	}
	if logicalAfter.Status != "closed" || logicalAfter.Metadata["gc.final_disposition"] != "pass" {
		t.Fatalf("logical in SQLite = status %q disposition %q, want closed/pass", logicalAfter.Status, logicalAfter.Metadata["gc.final_disposition"])
	}
	if logicalAfter.Metadata["gc.output_json"] != `{"ok":true}` {
		t.Fatalf("logical gc.output_json = %q, want propagated output", logicalAfter.Metadata["gc.output_json"])
	}
}
