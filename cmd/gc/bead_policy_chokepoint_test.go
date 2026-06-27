package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestBeadPolicyCreateChokepointRoutesGraphClassToGraphStore proves the Phase G2a
// create-chokepoint: under a graph-relocated city a graph-class bead created through the
// policy store (policy(work), no Router) lands on the dedicated graph store, while a
// work-class bead stays on the work store. This is the orphan-prevention safety net —
// a caller that is not itself class-aware still cannot strand a graph bead on the work
// (Dolt) store.
func TestBeadPolicyCreateChokepointRoutesGraphClassToGraphStore(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	graph, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	policy := wrapStoreWithBeadPolicies(work, graphClassSQLiteCfg(), cityPath)

	gb, err := policy.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := graph.Get(gb.ID); err != nil {
		t.Fatalf("graph-class bead %s not in the graph store: %v", gb.ID, err)
	}
	if _, err := work.Get(gb.ID); err == nil {
		t.Fatalf("graph-class bead %s orphaned onto the work store", gb.ID)
	}

	wb, err := policy.Create(beads.Bead{Title: "backlog item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if _, err := work.Get(wb.ID); err != nil {
		t.Fatalf("work-class bead %s not in the work store: %v", wb.ID, err)
	}
	if _, err := graph.Get(wb.ID); err == nil {
		t.Fatalf("work-class bead %s wrongly routed to the graph store", wb.ID)
	}
}

// TestBeadPolicyCreateChokepointByteIdenticalAtGraphBD proves the default backend is
// untouched: with graph not relocated, graphStore == work, so a graph-class bead is
// created on the work store exactly as before (no routing).
func TestBeadPolicyCreateChokepointByteIdenticalAtGraphBD(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	policy := wrapStoreWithBeadPolicies(work, &config.City{}, cityPath)
	gb, err := policy.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := work.Get(gb.ID); err != nil {
		t.Fatalf("graph=bd: graph-class bead %s must stay on the work store: %v", gb.ID, err)
	}
}
