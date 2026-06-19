package coordrouter

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// graphMemberQuery is a representative root-scoped List query: members carry the
// graph root id in their RootBeadID metadata, exactly as the dispatcher's
// scope-check List does.
func graphMemberQuery(rootID string) beads.ListQuery {
	return beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": rootID},
		IncludeClosed: true,
	}
}

func TestListGraphOnlyQueriesGraphBackendOnly(t *testing.T) {
	members := []beads.Bead{
		{ID: "gcg-2", Title: "m2", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
		{ID: "gcg-3", Title: "m3", Metadata: map[string]string{"gc.root_bead_id": "gcg-1"}},
	}
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", members, nil)
	r := twoBackendRouter(work, graph)

	got, err := r.ListGraphOnly(graphMemberQuery("gcg-1"))
	if err != nil {
		t.Fatalf("ListGraphOnly error = %v, want nil", err)
	}
	if len(got) != 2 {
		t.Fatalf("ListGraphOnly returned %d beads, want 2 (the graph members)", len(got))
	}
	if work.listCalls != 0 {
		t.Errorf("work backend List called %d times, want 0 (Dolt leg must be skipped — no bd fork)", work.listCalls)
	}
	if graph.listCalls != 1 {
		t.Errorf("graph backend List called %d times, want 1", graph.listCalls)
	}
}

func TestListGraphOnlyIdentityPhaseFallsBackToFullList(t *testing.T) {
	members := []beads.Bead{
		{ID: "mc-2", Title: "m2", Metadata: map[string]string{"gc.root_bead_id": "mc-1"}},
	}
	work := newPrefixSpyStore("mc", members, nil)
	r := New(work) // single backend → identity phase, no distinct ClassGraph backend

	got, err := r.ListGraphOnly(graphMemberQuery("mc-1"))
	if err != nil {
		t.Fatalf("ListGraphOnly error = %v, want nil", err)
	}
	ref, err := r.List(graphMemberQuery("mc-1"))
	if err != nil {
		t.Fatalf("List error = %v, want nil", err)
	}
	if len(got) != len(ref) {
		t.Fatalf("identity-phase ListGraphOnly len=%d, full List len=%d — must be byte-identical", len(got), len(ref))
	}
	for i := range got {
		if got[i].ID != ref[i].ID {
			t.Errorf("identity-phase ListGraphOnly[%d].ID=%q, full List[%d].ID=%q — fallback not byte-identical", i, got[i].ID, i, ref[i].ID)
		}
	}
}

func TestGraphIDPrefixDistinctVsIdentity(t *testing.T) {
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)
	if pfx := r.GraphIDPrefix(); pfx != "gcg" {
		t.Errorf("GraphIDPrefix() with distinct graph backend = %q, want %q", pfx, "gcg")
	}

	identity := New(newPrefixSpyStore("mc", nil, nil))
	if pfx := identity.GraphIDPrefix(); pfx != "" {
		t.Errorf("GraphIDPrefix() in identity phase = %q, want \"\"", pfx)
	}
}

func TestGraphOnlyListForCapabilityPresence(t *testing.T) {
	work := newPrefixSpyStore("mc", nil, nil)
	graph := newPrefixSpyStore("gcg", nil, nil)
	r := twoBackendRouter(work, graph)

	if gol, ok := beads.GraphOnlyListFor(r); !ok || gol == nil {
		t.Errorf("GraphOnlyListFor(router) ok=%v, want true (Router satisfies GraphOnlyListStore)", ok)
	}

	plain := beads.NewMemStoreFrom(0, nil, nil)
	if _, ok := beads.GraphOnlyListFor(plain); ok {
		t.Errorf("GraphOnlyListFor(plainMemStore) ok=true, want false (no graph-only-list capability)")
	}
}
