package main

import (
	"bytes"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
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

// TestGcReadyReadsGraphStoreAloneAndRoundTrips proves the gc ready contract end to
// end under graph_store=sqlite: readyStoreSet reads the dedicated graph store ALONE
// (the worker-readiness slice — the Dolt work leg is kept off the hot loop) and the
// result renders to JSON that the worker's own work_query parser
// (decodeHookClaimBeads) reads back. So a worker using `gc ready` as its work_query
// discovers ready graph beads in SQLite, and the work backlog never leaks into the
// readiness probe (the post-coordrouter class-aware behavior, stronger than the old
// federated Router ready set).
func TestGcReadyReadsGraphStoreAloneAndRoundTrips(t *testing.T) {
	cityPath := t.TempDir()
	// Offset the work MemStore so it occupies a distinct id namespace from the
	// SQLite graph store (both otherwise mint gc-N — see ga-y5pwx3).
	work := beads.NewMemStoreFrom(1000, nil, nil)
	graph, ok := openGraphSQLiteStore(cityPath) // dedicated .gc/beads.sqlite (gcg-)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	wb, err := work.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	gb, err := graph.Create(beads.Bead{Title: "graph item", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}

	// readyStoreSet routes to the dedicated graph store when graph is relocated.
	ready, err := readyStoreSet(work, graphClassSQLiteCfg(), cityPath, beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("readyStoreSet: %v", err)
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
	if !ids[gb.ID] {
		t.Errorf("gc ready output missing the SQLite graph bead %s (have %v)", gb.ID, ids)
	}
	if ids[wb.ID] {
		t.Errorf("gc ready leaked the Dolt work bead %s into the worker readiness slice (have %v)", wb.ID, ids)
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
