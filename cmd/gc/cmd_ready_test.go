package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// TestApplyReadyPredicateMatchesBdReadyWorkQuery proves gc ready renders the same
// routed/unassigned/non-epic/oldest predicate as the canonical bd ready work
// query (config.bdReadyPoolDemandShell), so swapping the work-query binary from
// `bd ready` to `gc ready` returns the same shape.
func TestApplyReadyPredicateMatchesBdReadyWorkQuery(t *testing.T) {
	at := func(sec int64) time.Time { return time.Unix(sec, 0).UTC() }
	in := []beads.Bead{
		{ID: "gcg-1", Type: "task", CreatedAt: at(300), Metadata: map[string]string{"gc.routed_to": "gascity/gc.run-operator"}},
		{ID: "gcg-2", Type: "task", CreatedAt: at(100), Metadata: map[string]string{"gc.routed_to": "gascity/gc.run-operator"}},
		{ID: "gcg-3", Type: "task", Assignee: "someone", CreatedAt: at(50), Metadata: map[string]string{"gc.routed_to": "gascity/gc.run-operator"}},
		{ID: "gcg-4", Type: "epic", CreatedAt: at(10), Metadata: map[string]string{"gc.routed_to": "gascity/gc.run-operator"}},
		{ID: "gcg-5", Type: "task", CreatedAt: at(20), Metadata: map[string]string{"gc.routed_to": "gascity/gc.other"}},
		{ID: "gcg-6", Type: "task", CreatedAt: at(5)}, // no routing
	}
	pred, err := readyPredicateFromFlags(
		[]string{"gc.routed_to=gascity/gc.run-operator"},
		[]string{"epic"},
		true,  // unassigned
		false, // includeEphemeral
		"oldest",
		1, // limit
	)
	if err != nil {
		t.Fatalf("readyPredicateFromFlags: %v", err)
	}
	got := applyReadyPredicate(in, pred)
	// Only gcg-1 and gcg-2 are unassigned, non-epic, routed-to the target; oldest
	// (gcg-2, created earlier) wins under limit=1.
	if len(got) != 1 || got[0].ID != "gcg-2" {
		ids := make([]string, len(got))
		for i, b := range got {
			ids[i] = b.ID
		}
		t.Fatalf("applyReadyPredicate = %v, want [gcg-2]", ids)
	}
}

func TestReadyPredicateFromFlagsRejectsMalformedMetadataField(t *testing.T) {
	if _, err := readyPredicateFromFlags([]string{"no-equals-sign"}, nil, false, false, "", 0); err == nil {
		t.Fatal("readyPredicateFromFlags accepted a --metadata-field without key=value, want error")
	}
	if _, err := readyPredicateFromFlags(nil, nil, false, false, "newest", 0); err == nil {
		t.Fatal("readyPredicateFromFlags accepted --sort newest, want error")
	}
}

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
