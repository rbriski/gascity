package coordrouter

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestRouterFederatesReadsAcrossBackends(t *testing.T) {
	work := beads.NewMemStore()
	// Offset the graph store's id sequence so the two MemStores occupy distinct id
	// namespaces (as real bd vs sqlite backends do); otherwise both mint "bd-1"
	// and a by-id read collides across backends.
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph node", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}
	if wb.ID == gb.ID {
		t.Fatalf("test setup: id namespaces collided (%s)", wb.ID)
	}

	// Each bead physically lands only in its owning backend.
	if _, err := work.Get(wb.ID); err != nil {
		t.Fatalf("work bead not in the work backend: %v", err)
	}
	if _, err := graph.Get(gb.ID); err != nil {
		t.Fatalf("graph bead not in the graph backend: %v", err)
	}
	if _, err := graph.Get(wb.ID); err == nil {
		t.Fatal("work bead leaked into the graph backend")
	}

	// Federated Get finds a bead in whichever backend owns it.
	if _, err := r.Get(wb.ID); err != nil {
		t.Fatalf("federated Get(work): %v", err)
	}
	if _, err := r.Get(gb.ID); err != nil {
		t.Fatalf("federated Get(graph): %v", err)
	}
	if _, err := r.Get("does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("federated Get(missing) = %v, want ErrNotFound", err)
	}

	// Federated List and Ready union both backends.
	assertUnions := func(name string, beadsOut []beads.Bead) {
		t.Helper()
		ids := make(map[string]bool, len(beadsOut))
		for _, b := range beadsOut {
			ids[b.ID] = true
		}
		if !ids[wb.ID] || !ids[gb.ID] {
			t.Fatalf("%s did not union both backends: have %v, want %s + %s", name, ids, wb.ID, gb.ID)
		}
	}
	listed, err := r.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertUnions("List", listed)

	ready, err := r.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	assertUnions("Ready", ready)
}

// TestRouterSingleBackendReadsDelegate confirms the identity-phase fast path: with
// one backend, federated reads delegate directly (byte-identical to that backend).
func TestRouterSingleBackendReadsDelegate(t *testing.T) {
	mem := beads.NewMemStore()
	r := New(mem)
	created, err := r.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := r.Get(created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("single-backend Get = (%q, %v), want %q", got.ID, err, created.ID)
	}
	if _, ok := r.soleBackend(); !ok {
		t.Fatal("expected a sole backend in the identity phase")
	}
}

// TestRouterReadyGraphOnlyExcludesWorkBackend proves the worker/dispatcher
// execution-readiness surface: under graph_store=sqlite (a distinct ClassGraph
// backend), ReadyGraphOnly returns ONLY the graph backend's ready set and never
// the Dolt ClassWork primary, while the full federated Ready still unions both
// for the human/diagnostic backlog view.
func TestRouterReadyGraphOnlyExcludesWorkBackend(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph node", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}

	graphOnly, err := r.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	ids := make(map[string]bool, len(graphOnly))
	for _, b := range graphOnly {
		ids[b.ID] = true
	}
	if !ids[gb.ID] {
		t.Fatalf("ReadyGraphOnly missing graph bead %s: have %v", gb.ID, ids)
	}
	if ids[wb.ID] {
		t.Fatalf("ReadyGraphOnly leaked the Dolt work bead %s into the worker readiness hot loop", wb.ID)
	}

	// The full federation contract is unchanged: Ready still unions both backends.
	full, err := r.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	fids := make(map[string]bool, len(full))
	for _, b := range full {
		fids[b.ID] = true
	}
	if !fids[wb.ID] || !fids[gb.ID] {
		t.Fatalf("Ready must still union both backends: have %v", fids)
	}
}

// TestRouterReadyGraphOnlyIdentityPhaseFallsBack proves the default (non-sqlite)
// city stays byte-identical: with no distinct ClassGraph backend, ReadyGraphOnly
// falls back to the sole backend's ready set (the work store), so a Dolt-only
// city's readiness is unchanged.
func TestRouterReadyGraphOnlyIdentityPhaseFallsBack(t *testing.T) {
	mem := beads.NewMemStore()
	r := New(mem)
	created, err := r.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ready, err := r.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	found := false
	for _, b := range ready {
		if b.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("identity-phase ReadyGraphOnly must return the sole backend's bead %s: have %v", created.ID, ready)
	}
}
