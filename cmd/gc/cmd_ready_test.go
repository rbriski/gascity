package main

import (
	"bytes"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// TestGcReadyFederatesAndRoundTrips proves the gc ready contract end to end: ready
// work federated across a Router{work, graph: SQLite} renders to JSON that the
// worker's own work_query parser (decodeHookClaimBeads) reads back — so a worker
// using `gc ready` as its work_query discovers ready graph beads in SQLite, not
// just Dolt work beads.
func TestGcReadyFederatesAndRoundTrips(t *testing.T) {
	// Offset the work MemStore so it occupies a distinct id namespace from the
	// SQLite graph store (both otherwise mint gc-N — see ga-y5pwx3).
	work := beads.NewMemStoreFrom(1000, nil, nil)
	sqlite, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	r := coordrouter.New(work)
	r.Register(coordclass.ClassGraph, graph)

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph item", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}

	ready, err := r.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}

	var buf bytes.Buffer
	if code := writeReadyJSON(ready, &buf, io.Discard); code != 0 {
		t.Fatalf("writeReadyJSON returned exit code %d", code)
	}

	parsed, err := decodeHookClaimBeads(buf.String())
	if err != nil {
		t.Fatalf("a work_query consumer could not parse gc ready output: %v", err)
	}
	ids := make(map[string]bool, len(parsed))
	for _, b := range parsed {
		ids[b.ID] = true
	}
	if !ids[wb.ID] {
		t.Errorf("gc ready output missing the work bead %s (have %v)", wb.ID, ids)
	}
	if !ids[gb.ID] {
		t.Errorf("gc ready output missing the SQLite graph bead %s (have %v)", gb.ID, ids)
	}
}

// TestWriteReadyJSONEmitsArrayNeverNull guards the work_query contract: an empty
// ready set must serialize as [] (not null), which a []beads.Bead consumer parses.
func TestWriteReadyJSONEmitsArrayNeverNull(t *testing.T) {
	var buf bytes.Buffer
	if code := writeReadyJSON(nil, &buf, io.Discard); code != 0 {
		t.Fatalf("writeReadyJSON(nil) exit code %d", code)
	}
	parsed, err := decodeHookClaimBeads(buf.String())
	if err != nil {
		t.Fatalf("empty ready output not parseable: %v", err)
	}
	if len(parsed) != 0 {
		t.Fatalf("empty ready output parsed to %d beads, want 0", len(parsed))
	}
}
