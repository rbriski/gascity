package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// TestResolveGraphInput_ExpandsCrossStoreConvoyMembers pins landmine #15 Half B
// on the CLI: `gc graph gcg-<convoy>` expands a drain-unit convoy that lives in
// the infra store but tracks work-store members. Without the member-store
// complement the members render as "unknown" placeholders.
func TestResolveGraphInput_ExpandsCrossStoreConvoyMembers(t *testing.T) {
	infra := beads.NewMemStoreHonoringIDs()
	work := beads.NewMemStoreHonoringIDs()

	convoy, err := infra.Create(beads.Bead{
		ID: "gcg-convoy", Type: "convoy", Title: "drain unit convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	member, err := work.Create(beads.Bead{ID: "hq-member", Type: "task", Title: "real work"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := convoycore.TrackItem(infra, convoy.ID, member.ID, work); err != nil {
		t.Fatalf("TrackItem: %v", err)
	}

	var stderr bytes.Buffer

	// Without the complement: member is an unresolved placeholder.
	resolvedNoComplement, err := resolveGraphInput(infra, []string{convoy.ID}, &stderr)
	if err != nil {
		t.Fatalf("resolveGraphInput (no complement): %v", err)
	}
	if len(resolvedNoComplement) != 1 || !convoycore.IsUnresolvedTrackedItem(resolvedNoComplement[0]) {
		t.Fatalf("without complement, expected 1 unresolved placeholder; got %+v", resolvedNoComplement)
	}

	// With the work store as complement: member resolves fully.
	resolved, err := resolveGraphInput(infra, []string{convoy.ID}, &stderr, work)
	if err != nil {
		t.Fatalf("resolveGraphInput (with complement): %v", err)
	}
	if len(resolved) != 1 {
		t.Fatalf("resolved = %+v, want exactly the one member", resolved)
	}
	if convoycore.IsUnresolvedTrackedItem(resolved[0]) {
		t.Fatalf("member still a placeholder with complement: %+v", resolved[0])
	}
	if resolved[0].ID != member.ID || resolved[0].Title != "real work" || resolved[0].Status != "open" {
		t.Fatalf("resolved member = %+v, want id=%q title=%q status=open", resolved[0], member.ID, "real work")
	}
}
