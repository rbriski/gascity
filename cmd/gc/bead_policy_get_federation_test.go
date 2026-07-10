package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestBeadPolicyGetFederatesGraphReads is the follow-up to the ref-by-id cross-store
// tracks fix: beadPolicyStore.Create routes a ClassGraph bead to the dedicated graph
// store, so Get must read it back from there too. Before the Get override, a gcg- bead
// was write-only through the policy wrapper — TrackingConvoysForItem / hasLiveTrackingConvoy
// resolving a convoy by id through the store handle alone silently missed it.
func TestBeadPolicyGetFederatesGraphReads(t *testing.T) {
	e := newCutoverEnv(t, true) // relocated: distinct work (Mem) + graph (SQLite gcg)

	// A graph-class bead (wisp label) routes to the graph store via the create chokepoint.
	gb, err := e.store.Create(beads.Bead{Title: "graph bead", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	// It must physically live on the graph store, not the work store.
	if _, err := e.graph.Get(gb.ID); err != nil {
		t.Fatalf("graph bead %s not on the graph store: %v", gb.ID, err)
	}
	if _, err := e.work.Get(gb.ID); err == nil {
		t.Fatalf("graph bead %s must NOT be on the work store", gb.ID)
	}
	// The fix: reading it back through the policy store handle now federates to graph.
	got, err := e.store.Get(gb.ID)
	if err != nil {
		t.Fatalf("policy-store Get(%s) must federate to the graph store, got: %v", gb.ID, err)
	}
	if got.ID != gb.ID {
		t.Fatalf("Get returned %s, want %s", got.ID, gb.ID)
	}
	// A work-class bead still resolves through the same handle (byte-identical path).
	wb, err := e.store.Create(beads.Bead{Title: "work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if got, err := e.store.Get(wb.ID); err != nil || got.ID != wb.ID {
		t.Fatalf("policy-store Get(work %s) = (%v, %v), want it found", wb.ID, got.ID, err)
	}
}

// TestBeadPolicyWritesFederateToGraph is the write-side symmetry of
// TestBeadPolicyGetFederatesGraphReads: a graph-class bead created through the policy
// store must be MUTABLE through it too. Before the write federation, Update/SetMetadata/
// Close/DepAdd promoted to the work store and silently missed the graph-resident bead
// (stamping outcomes, closing steps, wiring deps on graph workflow beads all failed on a
// split city). Each by-id write must owner-route to the graph store.
func TestBeadPolicyWritesFederateToGraph(t *testing.T) {
	e := newCutoverEnv(t, true) // relocated: distinct work (Mem) + graph (SQLite gcg)

	gb, err := e.store.Create(beads.Bead{Title: "graph wf bead", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := e.graph.Get(gb.ID); err != nil {
		t.Fatalf("precondition: graph bead not on the graph store: %v", err)
	}

	// SetMetadata must land on the graph store, readable back there.
	if err := e.store.SetMetadata(gb.ID, beadmeta.OutcomeMetadataKey, "pass"); err != nil {
		t.Fatalf("SetMetadata must owner-route to graph: %v", err)
	}
	if got, _ := e.graph.Get(gb.ID); got.Metadata[beadmeta.OutcomeMetadataKey] != "pass" {
		t.Fatalf("SetMetadata did not land on the graph store: %v", got.Metadata)
	}
	// SetMetadataBatch + Update likewise.
	if err := e.store.SetMetadataBatch(gb.ID, map[string]string{"k": "v"}); err != nil {
		t.Fatalf("SetMetadataBatch must owner-route to graph: %v", err)
	}
	if err := e.store.Update(gb.ID, beads.UpdateOpts{}); err != nil {
		t.Fatalf("Update must owner-route to graph: %v", err)
	}
	// A same-store dep on the graph store, wired through the policy handle.
	gb2, _ := e.store.Create(beads.Bead{Title: "graph blocker", Type: "task", Labels: []string{"gc:wisp"}})
	if err := e.store.DepAdd(gb.ID, gb2.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd (both graph-resident) must owner-route to graph: %v", err)
	}
	if deps, err := e.store.DepList(gb.ID, "down"); err != nil || len(deps) != 1 {
		t.Fatalf("DepList must owner-route to graph and see the edge: deps=%v err=%v", deps, err)
	}
	// Close must land on the graph store.
	if err := e.store.Close(gb.ID); err != nil {
		t.Fatalf("Close must owner-route to graph: %v", err)
	}
	if got, _ := e.graph.Get(gb.ID); got.Status != "closed" {
		t.Fatalf("Close did not land on the graph store; status=%q", got.Status)
	}

	// A work-class bead still routes to the work store (byte-identical path).
	wb, _ := e.store.Create(beads.Bead{Title: "work bead", Type: "task"})
	if err := e.store.SetMetadata(wb.ID, "wk", "1"); err != nil {
		t.Fatalf("work SetMetadata: %v", err)
	}
	if got, _ := e.work.Get(wb.ID); got.Metadata["wk"] != "1" {
		t.Fatalf("work write must land on the work store: %v", got.Metadata)
	}
}

// TestBeadPolicyScansFederateGraph proves ListByLabel/ListByMetadata union the graph
// store: a graph-resident root (singleton/dedup lookups) is found through the policy
// handle, not silently missed on a work-only scan.
func TestBeadPolicyScansFederateGraph(t *testing.T) {
	e := newCutoverEnv(t, true)
	gb, err := e.store.Create(beads.Bead{Title: "graph root", Type: "task", Labels: []string{"gc:wisp", "scan-me"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := e.graph.Get(gb.ID); err != nil {
		t.Fatalf("precondition: graph bead not on graph store: %v", err)
	}
	byLabel, err := e.store.ListByLabel("scan-me", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	found := false
	for _, b := range byLabel {
		if b.ID == gb.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ListByLabel must union the graph store; graph root %s missing from %v", gb.ID, byLabel)
	}
}
