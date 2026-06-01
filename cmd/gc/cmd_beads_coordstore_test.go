package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCopyBeadsIntoCoordstoreDryRunCountsDepsWithoutTarget(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	source := beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
		{IssueID: "ga-2", DependsOnID: "ga-missing", Type: "blocks"},
	})

	summary, err := copyBeadsIntoCoordstore(source, nil, true, coordstoreRetentionOptions{Full: true})
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore dry run: %v", err)
	}
	if summary.SourceCount != 2 || summary.Deps != 1 || summary.Imported != 0 || summary.Skipped != 0 || !summary.DryRun {
		t.Fatalf("dry-run summary = %+v, want source=2 importable deps=1 no writes", summary)
	}
}

func TestCopyBeadsIntoCoordstoreSkipsExpiredTerminalAndBatchesDeps(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-6 * time.Hour)
	recent := now.Add(-30 * time.Minute)
	source := newBatchTrackingCoordstoreStore([]beads.Bead{
		{ID: "ga-open", Title: "open", Status: "open", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-old", Title: "old closed", Status: "closed", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-old-canceled", Title: "old canceled", Status: "cancel" + "led", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-recent", Title: "recent closed", Status: "closed", Type: "task", CreatedAt: recent, UpdatedAt: recent},
	}, []beads.Dep{
		{IssueID: "ga-recent", DependsOnID: "ga-open", Type: "blocks"},
		{IssueID: "ga-recent", DependsOnID: "ga-old", Type: "blocks"},
	})

	summary, err := copyBeadsIntoCoordstore(source, nil, true, coordstoreRetentionOptions{
		Retention: 4 * time.Hour,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore: %v", err)
	}
	if summary.SourceCount != 2 || summary.Filtered != 2 || summary.Deps != 1 {
		t.Fatalf("summary = %+v, want two kept beads, two filtered, one kept dep", summary)
	}
	if source.batchCalls != 1 {
		t.Fatalf("DepListBatch calls = %d, want 1", source.batchCalls)
	}
	if source.depListCalls != 0 {
		t.Fatalf("DepList calls = %d, want 0", source.depListCalls)
	}
}

func TestCopyBeadsIntoCoordstoreFullIncludesExpiredTerminal(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-6 * time.Hour)
	source := newBatchTrackingCoordstoreStore([]beads.Bead{
		{ID: "ga-open", Title: "open", Status: "open", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-old", Title: "old closed", Status: "closed", Type: "task", CreatedAt: old, UpdatedAt: old},
	}, []beads.Dep{
		{IssueID: "ga-old", DependsOnID: "ga-open", Type: "blocks"},
	})

	summary, err := copyBeadsIntoCoordstore(source, nil, true, coordstoreRetentionOptions{Full: true, Now: now})
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore: %v", err)
	}
	if summary.SourceCount != 2 || summary.Filtered != 0 || summary.Deps != 1 {
		t.Fatalf("summary = %+v, want full snapshot with old terminal bead and dep", summary)
	}
}

func TestCoordstoreRetentionOptionsConfigureSQLiteSweeper(t *testing.T) {
	opts, err := newCoordstoreRetentionOptions(false, 8*time.Hour)
	if err != nil {
		t.Fatalf("newCoordstoreRetentionOptions: %v", err)
	}
	period, sweep := opts.sqliteRetention()
	if period != 8*time.Hour || sweep != coordstoreDefaultRetentionSweepInterval {
		t.Fatalf("sqlite retention = (%s, %s), want custom period and default sweep", period, sweep)
	}
	full, err := newCoordstoreRetentionOptions(true, 0)
	if err != nil {
		t.Fatalf("full newCoordstoreRetentionOptions: %v", err)
	}
	period, sweep = full.sqliteRetention()
	if period != 0 || sweep != 0 {
		t.Fatalf("full sqlite retention = (%s, %s), want disabled sweeper", period, sweep)
	}
	if _, err := newCoordstoreRetentionOptions(false, 0); err == nil {
		t.Fatal("newCoordstoreRetentionOptions accepted zero retention without --full")
	}
}

func TestDiffCoordstoreShadowDetectsDependencyMismatch(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	sourceBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	targetBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	source := beads.NewMemStoreFrom(2, sourceBeads, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
	})
	target := beads.NewMemStoreFrom(2, targetBeads, nil)

	summary, err := diffCoordstoreShadow(source, target, coordstoreRetentionOptions{Full: true})
	if err != nil {
		t.Fatalf("diffCoordstoreShadow: %v", err)
	}
	if summary.OK {
		t.Fatal("shadow summary OK = true, want dependency mismatch")
	}
	if !reflect.DeepEqual(summary.Corrupted, []string{"ga-2"}) {
		t.Fatalf("corrupted = %+v, want [ga-2]", summary.Corrupted)
	}
}

func TestDiffCoordstoreShadowSkipsExpiredTerminalInSourceAndTarget(t *testing.T) {
	now := time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC)
	old := now.Add(-6 * time.Hour)
	source := beads.NewMemStoreFrom(3, []beads.Bead{
		{ID: "ga-open", Title: "open", Status: "open", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-old-source", Title: "old source", Status: "closed", Type: "task", CreatedAt: old, UpdatedAt: old},
	}, nil)
	target := beads.NewMemStoreFrom(3, []beads.Bead{
		{ID: "ga-open", Title: "open", Status: "open", Type: "task", CreatedAt: old, UpdatedAt: old},
		{ID: "ga-old-target", Title: "old target", Status: "closed", Type: "task", CreatedAt: old, UpdatedAt: old},
	}, nil)

	summary, err := diffCoordstoreShadow(source, target, coordstoreRetentionOptions{
		Retention: 4 * time.Hour,
		Now:       now,
	})
	if err != nil {
		t.Fatalf("diffCoordstoreShadow: %v", err)
	}
	if !summary.OK {
		t.Fatalf("summary = %+v, want old terminal beads ignored on both sides", summary)
	}
	if summary.SourceCount != 1 || summary.TargetCount != 1 || summary.FilteredSource != 1 || summary.FilteredTarget != 1 {
		t.Fatalf("summary counts = %+v, want one kept and one filtered per side", summary)
	}
}

func TestBeadsCoordstoreShadowDeclaresJSONSupport(t *testing.T) {
	var stdout, stderr bytes.Buffer
	code := run([]string{"beads", "coordstore", "shadow", "--json-schema"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("run json-schema = %d, stderr=%q stdout=%q", code, stderr.String(), stdout.String())
	}
	var manifest struct {
		JSONSupported bool `json:"json_supported"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &manifest); err != nil {
		t.Fatalf("manifest is not JSON: %v\n%s", err, stdout.String())
	}
	if !manifest.JSONSupported {
		t.Fatalf("manifest = %s, want JSON support", stdout.String())
	}
}

type batchTrackingCoordstoreStore struct {
	*beads.MemStore
	batchCalls   int
	depListCalls int
}

func newBatchTrackingCoordstoreStore(existing []beads.Bead, deps []beads.Dep) *batchTrackingCoordstoreStore {
	return &batchTrackingCoordstoreStore{MemStore: beads.NewMemStoreFrom(len(existing), existing, deps)}
}

func (s *batchTrackingCoordstoreStore) DepList(id, direction string) ([]beads.Dep, error) {
	s.depListCalls++
	return nil, fmt.Errorf("DepList should not be called for %q direction %q", id, direction)
}

func (s *batchTrackingCoordstoreStore) DepListBatch(ids []string) (map[string][]beads.Dep, error) {
	s.batchCalls++
	return s.MemStore.DepListBatch(ids)
}
