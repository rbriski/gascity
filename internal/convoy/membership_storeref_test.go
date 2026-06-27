package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestMembershipResolvesCrossStoreMemberViaTail proves the class-aware convoy seam:
// a synthetic (graph-class) convoy whose tracks edge points at a member that
// physically lives in a DIFFERENT store resolves that member via the memberStores
// variadic tail (storeref) — exactly as coordrouter.Router's federated member read
// did, with no Router. The convoy lives in a SQLite graph store (gcg- ids) and the
// member in a work store (gc- ids), mirroring graph_store=sqlite.
func TestMembershipResolvesCrossStoreMemberViaTail(t *testing.T) {
	sqlite, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	convoyStore := sqlite // graph analog: the convoy bead's home
	t.Cleanup(func() {
		if c, ok := sqlite.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	memberStore := beads.NewMemStore() // work analog: the member's home

	convoy, err := convoyStore.Create(beads.Bead{Type: "convoy", Title: "drain unit"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	member, err := memberStore.Create(beads.Bead{Type: "task", Title: "backlog item"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	// TrackItem resolves the cross-store member via the tail (existence pre-check) and
	// records the tracks edge on the convoy's store. Without the tail it would error.
	if err := TrackItem(convoyStore, convoy.ID, member.ID, memberStore); err != nil {
		t.Fatalf("TrackItem cross-store: %v", err)
	}
	if ok, err := HasTrack(convoyStore, convoy.ID, member.ID); err != nil || !ok {
		t.Fatalf("HasTrack = (%v, %v), want (true, nil)", ok, err)
	}

	members, err := Members(convoyStore, convoy.ID, false, memberStore)
	if err != nil {
		t.Fatalf("Members cross-store: %v", err)
	}
	if len(members) != 1 || members[0].ID != member.ID {
		t.Fatalf("Members = %v, want [%s]", members, member.ID)
	}
	if IsUnresolvedTrackedItem(members[0]) {
		t.Fatalf("member %s came back unresolved — the memberStores tail did not resolve it", member.ID)
	}

	// Without the tail, the member is invisible to the convoy store alone and comes
	// back as the unresolved placeholder (never a wrong bead, never a hard error).
	soloMembers, err := Members(convoyStore, convoy.ID, true)
	if err != nil {
		t.Fatalf("Members single-store: %v", err)
	}
	if len(soloMembers) != 1 || !IsUnresolvedTrackedItem(soloMembers[0]) {
		t.Fatalf("single-store Members = %v, want one unresolved placeholder (member lives in the other store)", soloMembers)
	}
}

// TestMembershipSingleStoreIsByteIdentical proves the variadic tail is a no-op for
// same-class callers: with no memberStores a co-resident convoy+member behave exactly
// as before (member found in the convoy's own store).
func TestMembershipSingleStoreIsByteIdentical(t *testing.T) {
	store := beads.NewMemStore()
	convoy, err := store.Create(beads.Bead{Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	member, err := store.Create(beads.Bead{Type: "task"})
	if err != nil {
		t.Fatalf("create member: %v", err)
	}
	if err := TrackItem(store, convoy.ID, member.ID); err != nil {
		t.Fatalf("TrackItem single-store: %v", err)
	}
	members, err := Members(store, convoy.ID, false)
	if err != nil {
		t.Fatalf("Members single-store: %v", err)
	}
	if len(members) != 1 || members[0].ID != member.ID {
		t.Fatalf("Members = %v, want [%s]", members, member.ID)
	}
}

// TestTrackItemRejectsMemberAbsentFromAllStores: the existence pre-check fails when
// the item is in neither the convoy store nor any memberStore.
func TestTrackItemRejectsMemberAbsentFromAllStores(t *testing.T) {
	convoyStore := beads.NewMemStore()
	memberStore := beads.NewMemStore()
	convoy, err := convoyStore.Create(beads.Bead{Type: "convoy"})
	if err != nil {
		t.Fatalf("create convoy: %v", err)
	}
	if err := TrackItem(convoyStore, convoy.ID, "gc-missing", memberStore); err == nil {
		t.Fatal("TrackItem must error when the member exists in no store")
	}
}
