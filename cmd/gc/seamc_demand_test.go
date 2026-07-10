package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCollectOpenUnassignedRoutedWork_IncludesGraphStore is the Seam C regression:
// when the graph class is relocated, routed orchestration work (gcg- graph.v2 steps)
// is graph-resident, and the demand scan must see it or the pool never scales. The
// graph store is scanned as its own leg.
func TestCollectOpenUnassignedRoutedWork_IncludesGraphStore(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStore()
	// A routed, open, unassigned bead living ONLY in the graph store.
	gb, err := graph.Create(beads.Bead{Type: "task", Status: "open", Metadata: map[string]string{"gc.routed_to": "pool"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	got, _ := collectOpenUnassignedRoutedWork(&config.City{}, work, graph, nil, nil, io.Discard)
	found := false
	for _, b := range got {
		if b.ID == gb.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("graph-resident routed work %s must be collected via the graph leg; got %v", gb.ID, got)
	}
	// Byte-identical no-op when graphStore == store (bd default): no panic, no dup.
	if out, _ := collectOpenUnassignedRoutedWork(&config.City{}, work, work, nil, nil, io.Discard); len(out) != 0 {
		t.Fatalf("bd-default (graphStore==store) must be a no-op leg; got %v", out)
	}
}

// TestGraphStoreRoutedDemand counts graph-resident routed pool work (the demand-side
// of Seam C): a ready, unassigned, routed gcg- bead in the graph store contributes to
// the routed pool template's demand, and non-matching / assigned beads do not.
func TestGraphStoreRoutedDemand(t *testing.T) {
	graph := beads.NewMemStore()
	// ready + unassigned + routed to the pool template -> counted.
	if _, err := graph.Create(beads.Bead{Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gc.run-operator"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// assigned -> NOT counted.
	if _, err := graph.Create(beads.Bead{Type: "task", Status: "open", Assignee: "x", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "gc.run-operator"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	// routed to a different template -> NOT counted for run-operator.
	if _, err := graph.Create(beads.Bead{Type: "task", Status: "open", Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "other"}}); err != nil {
		t.Fatalf("create: %v", err)
	}
	targets := []defaultScaleCheckTarget{{template: "gc.run-operator", store: beads.NewMemStore(), storeKey: "city"}}
	got := graphStoreRoutedDemand(graph, targets, io.Discard)
	if got["gc.run-operator"] != 1 {
		t.Fatalf("graph routed demand for gc.run-operator = %d, want 1; got %v", got["gc.run-operator"], got)
	}
}
