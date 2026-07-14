package lumenrunproj

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// TestProjectorNoJournalIsEmpty proves a city that has run no Lumen runs (no
// graph journal on disk) yields no lanes and no detail — never an error and
// never a fabricated Lumen run.
func TestProjectorNoJournalIsEmpty(t *testing.T) {
	ctx := context.Background()
	cityRoot := t.TempDir() // no .gc/graph/journal.db
	p := New()
	defer func() { _ = p.Close() }()

	lanes, err := p.SummaryLanes(ctx, "mycity", cityRoot, nil)
	if err != nil {
		t.Fatalf("SummaryLanes on a journal-less city: %v", err)
	}
	if len(lanes) != 0 {
		t.Fatalf("SummaryLanes = %d lanes, want 0", len(lanes))
	}
	_, ok, err := p.Detail(ctx, "mycity", cityRoot, "gcg-absent", nil)
	if err != nil {
		t.Fatalf("Detail on a journal-less city: %v", err)
	}
	if ok {
		t.Fatal("Detail reported a Lumen run for a journal-less city")
	}
}

// TestProjectorEmptyJournalNoRuns proves the store-path resolution finds a real
// (but run-less) journal and reports no lanes, and that an unknown run id in a
// real journal is "not a Lumen run" (the caller keeps its 404).
func TestProjectorEmptyJournalNoRuns(t *testing.T) {
	ctx := context.Background()
	cityRoot := t.TempDir()
	// Create the journal on disk exactly where storeFor looks for it.
	journalDir := graphScopeRoot(cityRoot)
	if err := os.MkdirAll(journalDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed, err := graphstore.Open(ctx, filepath.Join(journalDir, "journal.db"), graphstore.Options{})
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	_ = seed.Close()

	p := New()
	defer func() { _ = p.Close() }()

	lanes, err := p.SummaryLanes(ctx, "mycity", cityRoot, nil)
	if err != nil {
		t.Fatalf("SummaryLanes on an empty journal: %v", err)
	}
	if len(lanes) != 0 {
		t.Fatalf("SummaryLanes = %d lanes, want 0 (no runs seeded)", len(lanes))
	}
	_, ok, err := p.Detail(ctx, "mycity", cityRoot, "gcg-no-such-run", nil)
	if err != nil {
		t.Fatalf("Detail for an unknown run: %v", err)
	}
	if ok {
		t.Fatal("Detail reported a Lumen run for an id with no journal stream")
	}
}

// TestIndexDoBeadsByRun pins the do-bead join: beads group by gc.lumen_run then
// gc.lumen_activation; beads missing either key are ignored.
func TestIndexDoBeadsByRun(t *testing.T) {
	beadList := []beads.Bead{
		{ID: "wb1", Metadata: beads.StringMap{
			beadmeta.LumenRunMetadataKey: "S1", beadmeta.LumenActivationMetadataKey: "A:0",
		}},
		{ID: "wb2", Metadata: beads.StringMap{
			beadmeta.LumenRunMetadataKey: "S1", beadmeta.LumenActivationMetadataKey: "B:0",
		}},
		{ID: "wb3", Metadata: beads.StringMap{
			beadmeta.LumenRunMetadataKey: "S2", beadmeta.LumenActivationMetadataKey: "A:0",
		}},
		{ID: "unrelated", Metadata: beads.StringMap{beadmeta.StepIDMetadataKey: "x"}},
		{ID: "no-activation", Metadata: beads.StringMap{beadmeta.LumenRunMetadataKey: "S1"}},
	}
	idx := indexDoBeadsByRun(beadList)

	if len(idx["S1"]) != 2 {
		t.Fatalf("S1 = %d beads, want 2 (A:0,B:0)", len(idx["S1"]))
	}
	if idx["S1"]["A:0"].ID != "wb1" || idx["S1"]["B:0"].ID != "wb2" {
		t.Fatalf("S1 join wrong: %+v", idx["S1"])
	}
	if idx["S2"]["A:0"].ID != "wb3" {
		t.Fatalf("S2 join wrong: %+v", idx["S2"])
	}
	if _, ok := idx[""]; ok {
		t.Fatal("a bead with no gc.lumen_run leaked into the index")
	}
}
