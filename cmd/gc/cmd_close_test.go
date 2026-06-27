package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/storeref"
)

// TestGcCloseRoutesGraphBeadToSQLite proves `gc close` routes a graph bead's
// close (and its gc.outcome stamp) to the dedicated graph store, while a work bead
// closes in the work store — routed by id via storeref.PrefixOwner over [graph,
// work] (the post-coordrouter wiring doClose uses). This is the worker's
// graph-store-aware close that finishes a step found via `gc ready`.
func TestGcCloseRoutesGraphBeadToSQLite(t *testing.T) {
	cityPath := t.TempDir()
	// Offset the work MemStore so it occupies a distinct id namespace from the
	// SQLite graph store (both otherwise mint gc-N — see ga-y5pwx3).
	work := beads.NewMemStoreFrom(1000, nil, nil)
	graph, ok := openGraphSQLiteStore(cityPath) // legacy .gc/beads.sqlite (gcg- prefix)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	// route mirrors doClose's storeref.PrefixOwner([graph, work]) resolution: a
	// gcg- id resolves to the graph store, everything else stays on work.
	route := func(id string) beads.Store {
		if owner := storeref.PrefixOwner(id, []beads.Store{graph, work}); owner != nil {
			return owner
		}
		return work
	}

	// A graph bead closes (with outcome) in SQLite.
	gb, err := graph.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if err := closeBeadThroughStore(route(gb.ID), gb.ID, "pass"); err != nil {
		t.Fatalf("closeBeadThroughStore(graph): %v", err)
	}
	stored, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get graph bead from SQLite: %v", err)
	}
	if stored.Status != "closed" || stored.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("graph bead in SQLite = status %q outcome %q, want closed/pass", stored.Status, stored.Metadata["gc.outcome"])
	}

	// A work bead closes in the work backend, not SQLite.
	wb, err := work.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := closeBeadThroughStore(route(wb.ID), wb.ID, ""); err != nil {
		t.Fatalf("closeBeadThroughStore(work): %v", err)
	}
	wstored, err := work.Get(wb.ID)
	if err != nil {
		t.Fatalf("re-get work bead: %v", err)
	}
	if wstored.Status != "closed" {
		t.Fatalf("work bead status = %q, want closed", wstored.Status)
	}
}
