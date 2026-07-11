package api

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// Landmine #15 Half B: a drain-unit convoy lives in the infra/graph store but
// tracks work-store members. collectBeadGraph must probe the member-store
// complement or those members render as synthetic "unknown" placeholders in the
// graph view.
func TestCollectBeadGraph_CrossStoreConvoyMembersResolved(t *testing.T) {
	graph := beads.NewMemStoreHonoringIDs()
	work := beads.NewMemStoreHonoringIDs()

	convoy, err := graph.Create(beads.Bead{
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
	if err := convoycore.TrackItem(graph, convoy.ID, member.ID, work); err != nil {
		t.Fatalf("TrackItem: %v", err)
	}

	// Without the complement, the member is an unresolved placeholder.
	beadsNoComplement, _, err := collectBeadGraph(graph, convoy)
	if err != nil {
		t.Fatalf("collectBeadGraph (no complement): %v", err)
	}
	if m, ok := findBeadByID(beadsNoComplement, member.ID); !ok || !convoycore.IsUnresolvedTrackedItem(m) {
		t.Fatalf("without the member-store complement, member %q should be an unresolved placeholder; got ok=%v bead=%+v", member.ID, ok, m)
	}

	// With the work store as the complement, the member resolves fully.
	beadsWithComplement, _, err := collectBeadGraph(graph, convoy, work)
	if err != nil {
		t.Fatalf("collectBeadGraph (with complement): %v", err)
	}
	m, ok := findBeadByID(beadsWithComplement, member.ID)
	if !ok {
		t.Fatalf("member %q missing from graph beads", member.ID)
	}
	if convoycore.IsUnresolvedTrackedItem(m) {
		t.Fatalf("member %q still a placeholder with the complement probed; got %+v", member.ID, m)
	}
	if m.Status != "open" || m.Title != "real work" {
		t.Fatalf("member resolved as %+v, want status=open title=%q", m, "real work")
	}
}

// TestMemberStoreComplement_SingleStoreIsNil pins byte-identity on a legacy city:
// with GraphBeadStore().Store == CityBeadStore(), the complement is empty so the
// view is unchanged.
func TestMemberStoreComplement_SingleStoreIsNil(t *testing.T) {
	fs := newFakeState(t) // graphBeadStore nil ⇒ GraphBeadStore().Store == CityBeadStore() (both nil here)
	srv := &Server{state: fs}
	if got := srv.memberStoreComplement(fs.stores["myrig"]); got != nil {
		t.Fatalf("memberStoreComplement on a single-store city = %v, want nil", got)
	}
}

func findBeadByID(items []beads.Bead, id string) (beads.Bead, bool) {
	for _, b := range items {
		if b.ID == id {
			return b, true
		}
	}
	return beads.Bead{}, false
}
