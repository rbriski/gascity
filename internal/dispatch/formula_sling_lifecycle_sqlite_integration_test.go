package dispatch

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/formulatest"
	"github.com/gastownhall/gascity/internal/molecule"
)

// TestFormulaSlingLifecycleKeepsGraphMutationsInSQLite is the end-to-end proof
// for the work/graph store split: a real graph.v2 formula sling runs its WHOLE
// lifecycle — instantiate (pour) → discover (Ready) → worker complete
// (mutate + close) → controller converge (ProcessControl) → terminal — on the
// dedicated SQLite graph store, and EVERY graph create and mutation lands in that
// SQLite store, never the (separate) work backend.
//
// It fuses the two prior link-level proofs into one chained run with a real
// molecule.Instantiate pour:
//   - the pour half (TestInstantiateRoutesGraphMoleculeToSQLite), and
//   - the convergence half (TestProcessControlConvergesGraphMoleculeOnSQLiteGraphStore),
//
// plus the worker's own discover/complete steps. This is the
// "a simple formula sling runs through the entire process with graph metadata in
// the in-process store" guarantee, end to end rather than link by link.
func TestFormulaSlingLifecycleKeepsGraphMutationsInSQLite(t *testing.T) {
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

	// The graph store is the dispatcher primary; the work store is a separate
	// backend never touched by graph-class ops (post-Router GE shape).
	store := graph

	// (1) SLING POUR. Instantiate a graph.v2 molecule — root + one actionable
	// work step + the workflow-finalize control bead the compiler emits — onto
	// the graph store. With graph-apply enabled the whole molecule pours atomically
	// to the SQLite graph backend (the recipe mirrors formula.Compile's output:
	// root --blocks--> workflow-finalize --blocks--> work).
	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
			}},
			{ID: "wf.work", Title: "Work", Type: "task"},
			{ID: "wf.workflow-finalize", Title: "Finalize workflow", Type: "task", Metadata: map[string]string{
				"gc.kind": "workflow-finalize",
			}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf", DependsOnID: "wf.workflow-finalize", Type: "blocks"},
			{StepID: "wf.workflow-finalize", DependsOnID: "wf.work", Type: "blocks"},
		},
	}
	result, err := molecule.Instantiate(context.Background(), store, recipe, molecule.Options{})
	if err != nil {
		t.Fatalf("Instantiate (sling pour): %v", err)
	}
	rootID := result.RootID
	stepID := result.IDMapping["wf.work"]
	finalizeID := result.IDMapping["wf.workflow-finalize"]
	if rootID == "" || stepID == "" || finalizeID == "" {
		t.Fatalf("missing bead ids: root=%q work=%q finalize=%q mapping=%v", rootID, stepID, finalizeID, result.IDMapping)
	}

	// (2) Every poured graph bead lives in SQLite; nothing leaked to the work store.
	assertBeadsResidentInGraphStore(t, graph, work, "poured", rootID, stepID, finalizeID)

	// (3) DISCOVER. The worker's federated Ready() surfaces the actionable work
	// step, while the root and the finalize control bead are held back by their
	// blocks deps.
	if !mustReadyContains(t, store, stepID) {
		t.Fatalf("Ready() did not surface the actionable work step %s", stepID)
	}
	if mustReadyContains(t, store, finalizeID) {
		t.Fatalf("finalize control bead %s surfaced in Ready() before the work step closed", finalizeID)
	}
	if mustReadyContains(t, store, rootID) {
		t.Fatalf("root bead %s surfaced in Ready() (it must close via the convergence engine)", rootID)
	}

	// (4) WORKER COMPLETE. Stamp the outcome and close the step on the graph
	// store; the mutation lands directly on the owning (SQLite) backend.
	if err := store.SetMetadata(stepID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("worker SetMetadata(gc.outcome): %v", err)
	}
	if err := store.Close(stepID); err != nil {
		t.Fatalf("worker Close(work step): %v", err)
	}
	stepAfter, err := graph.Get(stepID)
	if err != nil {
		t.Fatalf("re-get work step from SQLite: %v", err)
	}
	if stepAfter.Status != "closed" || stepAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("work step in SQLite = status %q outcome %q, want closed/pass", stepAfter.Status, stepAfter.Metadata["gc.outcome"])
	}

	// (5) CONVERGE. The controller's real engine drives the workflow-finalize
	// control bead (now unblocked): it closes the root with gc.outcome=pass then
	// closes itself — every closure landing directly on the SQLite graph store.
	finalize, err := store.Get(finalizeID)
	if err != nil {
		t.Fatalf("get finalize control bead: %v", err)
	}
	res, err := ProcessControl(store, finalize, ProcessOptions{WorkStore: work})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !res.Processed || res.Action != "workflow-pass" {
		t.Fatalf("convergence result = %+v, want processed workflow-pass", res)
	}

	// (6) TERMINAL. Read the closures straight from the SQLite backend: the root
	// and the finalizer are closed with gc.outcome=pass — the molecule converged
	// entirely in the graph store.
	rootAfter, err := graph.Get(rootID)
	if err != nil {
		t.Fatalf("re-get root from SQLite: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root in SQLite = status %q outcome %q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	finalizeAfter, err := graph.Get(finalizeID)
	if err != nil {
		t.Fatalf("re-get finalize from SQLite: %v", err)
	}
	if finalizeAfter.Status != "closed" {
		t.Fatalf("finalize in SQLite = status %q, want closed", finalizeAfter.Status)
	}

	// (7) FINAL INVARIANT. The entire molecule — every create and every mutation
	// across the whole sling lifecycle — is resident in SQLite, and the work
	// backend never saw a single graph bead.
	assertBeadsResidentInGraphStore(t, graph, work, "terminal", rootID, stepID, finalizeID)
}

// TestCookedFormulaSlingConvergesInSQLite is the gold-standard variant: it drives
// the REAL formula compiler (molecule.Cook → formula.Compile → applyGraphControls)
// so the workflow-finalize control bead is emitted by the compiler itself, not
// hand-declared. A minimal graph.v2 formula is cooked onto the dedicated SQLite
// graph store; the worker completes the discovered step and the controller
// converges the molecule to terminal — every graph bead resident in SQLite
// throughout. This proves the compiler's actual sling output (not just a
// hand-mirrored recipe) lives entirely in the new store.
func TestCookedFormulaSlingConvergesInSQLite(t *testing.T) {
	formulatest.EnableV2ForTest(t)
	prev := molecule.IsGraphApplyEnabled()
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() { molecule.SetGraphApplyEnabled(prev) })

	dir := t.TempDir()
	const toml = `
formula = "slingdemo"
version = 2
contract = "graph.v2"

[[steps]]
id = "work"
title = "Work"
`
	if err := os.WriteFile(filepath.Join(dir, "slingdemo.toml"), []byte(toml), 0o644); err != nil {
		t.Fatalf("writing formula: %v", err)
	}

	work := beads.NewMemStore()
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	// The graph store is the dispatcher primary; the work store stays separate.
	store := graph

	// SLING via the real compiler: Cook = Compile (adds the workflow-finalize
	// control bead) + Instantiate (pours directly into the SQLite graph store).
	result, err := molecule.Cook(context.Background(), store, "slingdemo", []string{dir}, molecule.Options{})
	if err != nil {
		t.Fatalf("Cook (real-compiler sling): %v", err)
	}
	if !result.GraphWorkflow {
		t.Fatal("result.GraphWorkflow = false, want true (graph.v2 root)")
	}
	rootID := result.RootID
	stepID := result.IDMapping["slingdemo.work"]
	finalizeID := result.IDMapping["slingdemo.workflow-finalize"]
	if rootID == "" || stepID == "" || finalizeID == "" {
		t.Fatalf("missing bead ids: root=%q work=%q finalize=%q mapping=%v", rootID, stepID, finalizeID, result.IDMapping)
	}

	// The whole compiler-generated molecule poured into SQLite, nothing to work.
	assertBeadsResidentInGraphStore(t, graph, work, "cooked", rootID, stepID, finalizeID)

	// DISCOVER + COMPLETE the actionable step (worker), then CONVERGE (controller).
	if !mustReadyContains(t, store, stepID) {
		t.Fatalf("Ready() did not surface the cooked work step %s", stepID)
	}
	if err := store.SetMetadata(stepID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("worker SetMetadata(gc.outcome): %v", err)
	}
	if err := store.Close(stepID); err != nil {
		t.Fatalf("worker Close(work step): %v", err)
	}

	finalize, err := store.Get(finalizeID)
	if err != nil {
		t.Fatalf("get finalize control bead: %v", err)
	}
	res, err := ProcessControl(store, finalize, ProcessOptions{WorkStore: work})
	if err != nil {
		t.Fatalf("ProcessControl(workflow-finalize): %v", err)
	}
	if !res.Processed || res.Action != "workflow-pass" {
		t.Fatalf("convergence result = %+v, want processed workflow-pass", res)
	}

	// TERMINAL in SQLite.
	rootAfter, err := graph.Get(rootID)
	if err != nil {
		t.Fatalf("re-get root from SQLite: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root in SQLite = status %q outcome %q, want closed/pass", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	assertBeadsResidentInGraphStore(t, graph, work, "terminal", rootID, stepID, finalizeID)
}

// assertBeadsResidentInGraphStore asserts every id is present in the SQLite graph
// store and absent from the work backend — the work/graph locality invariant the
// split must preserve across the whole lifecycle.
func assertBeadsResidentInGraphStore(t *testing.T, graph *beads.SQLiteStore, work beads.Store, label string, ids ...string) {
	t.Helper()
	for _, id := range ids {
		if _, err := graph.Get(id); err != nil {
			t.Fatalf("%s bead %s is not in the SQLite graph store: %v", label, id, err)
		}
		if _, err := work.Get(id); err == nil {
			t.Fatalf("%s bead %s leaked into the work backend", label, id)
		}
	}
}
