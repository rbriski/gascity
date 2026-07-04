package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func candidateIDs(bs []beads.Bead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

func TestFilterClaimCandidatesForGraphSplit(t *testing.T) {
	mixed := []beads.Bead{
		{ID: "gcg-1", Status: "open"},
		{ID: "ga-99", Status: "in_progress", Assignee: "worker-1"},
		{ID: "gcg-2", Status: "in_progress"},
		{ID: "mc-7", Status: "open"}, // a city-scope non-graph id
	}

	t.Run("disabled keeps everything (fail-open)", func(t *testing.T) {
		var stderr bytes.Buffer
		got := filterClaimCandidatesForGraphSplit(mixed, false, &stderr)
		if len(got) != len(mixed) {
			t.Fatalf("want all %d kept, got %v", len(mixed), candidateIDs(got))
		}
		if stderr.Len() != 0 {
			t.Errorf("no warning expected when disabled: %s", stderr.String())
		}
	})

	t.Run("enabled drops non-gcg candidates and logs them", func(t *testing.T) {
		var stderr bytes.Buffer
		got := candidateIDs(filterClaimCandidatesForGraphSplit(mixed, true, &stderr))
		if want := []string{"gcg-1", "gcg-2"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("kept = %v, want %v", got, want)
		}
		if !strings.Contains(stderr.String(), "ga-99") || !strings.Contains(stderr.String(), "mc-7") {
			t.Errorf("dropped candidates not logged: %s", stderr.String())
		}
	})

	t.Run("enabled all-graph unchanged, no warning", func(t *testing.T) {
		allGraph := []beads.Bead{{ID: "gcg-1"}, {ID: "gcg-2"}}
		var stderr bytes.Buffer
		got := filterClaimCandidatesForGraphSplit(allGraph, true, &stderr)
		if len(got) != 2 {
			t.Fatalf("want 2 kept, got %v", candidateIDs(got))
		}
		if stderr.Len() != 0 {
			t.Errorf("no warning expected for all-graph: %s", stderr.String())
		}
	})

	t.Run("empty input", func(t *testing.T) {
		var stderr bytes.Buffer
		if got := filterClaimCandidatesForGraphSplit(nil, true, &stderr); len(got) != 0 {
			t.Fatalf("want empty, got %v", candidateIDs(got))
		}
	})
}

func writeGraphSplitCityTOML(t *testing.T, graphStore string) string {
	t.Helper()
	dir := t.TempDir()
	body := "[workspace]\nname = \"demo\"\n\n[beads]\nprovider = \"file\"\n"
	if graphStore != "" {
		body += "graph_store = \"" + graphStore + "\"\n"
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestHookClaimGraphStoreSQLite(t *testing.T) {
	sqliteCity := writeGraphSplitCityTOML(t, "sqlite")
	doltCity := writeGraphSplitCityTOML(t, "")

	if !hookClaimGraphStoreSQLite("", []string{"GC_CITY=" + sqliteCity}) {
		t.Errorf("sqlite city should report graph_store=sqlite")
	}
	if hookClaimGraphStoreSQLite("", []string{"GC_CITY=" + doltCity}) {
		t.Errorf("default (non-sqlite) city must report false")
	}
	// Missing/unloadable config -> fail-open false (no guard).
	if hookClaimGraphStoreSQLite("/tmp/does-not-exist-city", nil) {
		t.Errorf("unresolvable city must fail-open to false")
	}
}

// TestDoHookClaimDropsNonGraphCandidateUnderSQLite proves the guard end-to-end:
// a ga- work-leg bead that WOULD be recovered as an existing assignment
// (in_progress + this worker's identity) is dropped under graph_store=sqlite, so
// the worker claims the graph-resident gcg- candidate instead and never touches
// the work-leg bead.
func TestDoHookClaimDropsNonGraphCandidateUnderSQLite(t *testing.T) {
	sqliteCity := writeGraphSplitCityTOML(t, "sqlite")

	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"ga-99","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker"}},` +
				`{"id":"gcg-2024","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: map[string]string{"gc.routed_to": "worker"}}, true, nil
		},
		ListContinuation:  func(context.Context, string, []string, string, string) ([]beads.Bead, error) { return nil, nil },
		ResolveWorkBranch: func(string) string { return "" },
		RecordSessionPointers: func(context.Context, string, []string, string, string, string, string) error {
			return nil
		},
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env:                []string{"GC_CITY=" + sqliteCity, "GC_SESSION_ID=sess-1"},
		JSON:               true,
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", sqliteCity, opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}

	var res hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal claim JSON: %v; stdout=%s", err, stdout.String())
	}
	if res.BeadID != "gcg-2024" {
		t.Fatalf("claimed bead = %q, want gcg-2024 (the ga- recovery candidate must be dropped under sqlite)", res.BeadID)
	}
	if !strings.Contains(stderr.String(), "ga-99") {
		t.Errorf("expected a drop warning for ga-99: %s", stderr.String())
	}
}
