package main

import (
	"io"
	"testing"

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
