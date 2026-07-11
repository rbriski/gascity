package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestGCReadyOutputWireShape(t *testing.T) {
	pri := 1
	items := []beads.Bead{{
		ID: "gcg-1", Title: "step", Type: "task", Status: "open",
		ParentID: "gcg-0", Priority: &pri,
		Metadata: beads.StringMap{"gc.routed_to": "worker"},
	}}
	var buf bytes.Buffer
	if err := renderReadyBeads(&buf, items); err != nil {
		t.Fatalf("renderReadyBeads: %v", err)
	}
	out := buf.String()
	// bd wire tags, not the bd-store-bridge's type/parent_id shape.
	if !strings.Contains(out, `"issue_type"`) {
		t.Errorf("output missing bd wire tag issue_type: %s", out)
	}
	if !strings.Contains(out, `"parent"`) {
		t.Errorf("output missing bd wire tag parent: %s", out)
	}
	if strings.Contains(out, `"parent_id"`) || strings.Contains(out, `"type":`) {
		t.Errorf("output uses bridge tags (type/parent_id) instead of bd wire tags: %s", out)
	}
	// Round-trip through the hook's work-query decode: Type/ParentID must survive.
	decoded, err := decodeHookClaimBeads(out)
	if err != nil {
		t.Fatalf("decodeHookClaimBeads: %v", err)
	}
	if len(decoded) != 1 || decoded[0].ID != "gcg-1" || decoded[0].Type != "task" || decoded[0].ParentID != "gcg-0" {
		t.Fatalf("round-trip lost fields: %+v", decoded)
	}
}

func TestGCReadyEmptyRendersArrayNotNull(t *testing.T) {
	var buf bytes.Buffer
	if err := renderReadyBeads(&buf, nil); err != nil {
		t.Fatalf("renderReadyBeads: %v", err)
	}
	if got := strings.TrimSpace(buf.String()); got != "[]" {
		t.Fatalf("empty ready output = %q, want [] (bd ready emits [], not null)", got)
	}
}

func TestFilterReadyBeads(t *testing.T) {
	items := []beads.Bead{
		{ID: "a", Type: "task", Assignee: "", Metadata: beads.StringMap{"gc.routed_to": "worker"}},
		{ID: "b", Type: "task", Assignee: "someone", Metadata: beads.StringMap{"gc.routed_to": "worker"}},
		{ID: "c", Type: "epic", Assignee: "", Metadata: beads.StringMap{"gc.routed_to": "worker"}},
		{ID: "d", Type: "task", Assignee: "", Metadata: beads.StringMap{"gc.routed_to": "other"}},
	}
	got := filterReadyBeads(items, readyOpts{
		unassigned:     true,
		excludeTypes:   []string{"epic"},
		metadataFields: []string{"gc.routed_to=worker"},
	})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("filter = %v, want [a] (b assigned, c epic, d wrong route)", beadIDSet(got))
	}
}

func TestSortReadyOutput(t *testing.T) {
	base := func() []beads.Bead {
		return []beads.Bead{{ID: "z"}, {ID: "a"}, {ID: "m"}}
	}
	// created_at is zero for all, so oldest/newest tie-break by id.
	asc := base()
	sortReadyOutput(asc, "oldest")
	if asc[0].ID != "a" {
		t.Errorf("--sort oldest: first = %q, want a", asc[0].ID)
	}
	desc := base()
	sortReadyOutput(desc, "newest")
	if desc[0].ID != "z" {
		t.Errorf("--sort newest: first = %q, want z", desc[0].ID)
	}
}
