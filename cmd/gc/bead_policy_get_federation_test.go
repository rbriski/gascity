package main

import (
	"testing"

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
