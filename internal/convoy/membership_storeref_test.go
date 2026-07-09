package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestMembershipResolvesCrossStoreMemberViaTail proves the class-aware convoy seam:
// a synthetic (graph-class) convoy whose tracks edge points at a member that
// physically lives in a DIFFERENT store resolves that member via the memberStores
// variadic tail (storeref) — the standalone by-id member read that replaced the
// retired per-class Router. The convoy lives in a SQLite graph store (gcg- ids) and
// the member in a work store (gc- ids), mirroring graph_store=sqlite.
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

	// Cross-store TrackItem is ref-by-id: it stamps gc.tracking_convoy_id on the member
	// (in the member's own store) and does NOT add a cross-store `tracks` dep — the
	// dep-add would have to resolve both endpoints in one store's dep table, which is
	// exactly the split-store failure this replaces. So HasTrack is false cross-store.
	if err := TrackItem(convoyStore, convoy.ID, member.ID, memberStore); err != nil {
		t.Fatalf("TrackItem cross-store: %v", err)
	}
	if ok, err := HasTrack(convoyStore, convoy.ID, member.ID); err != nil || ok {
		t.Fatalf("HasTrack = (%v, %v), want (false, nil) — cross-store uses a metadata ref, not a dep", ok, err)
	}
	if got, err := memberStore.Get(member.ID); err != nil || got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != convoy.ID {
		t.Fatalf("member %s missing gc.tracking_convoy_id=%s (err=%v, meta=%v)", member.ID, convoy.ID, err, got.Metadata)
	}

	// Members resolves the cross-store member via the metadata ref + the tail.
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

	// Reverse: the member points up at its convoy via the metadata ref.
	convoys, err := TrackingConvoysForItem(memberStore, member.ID, convoyStore)
	if err != nil {
		t.Fatalf("TrackingConvoysForItem cross-store: %v", err)
	}
	if len(convoys) != 1 || convoys[0].ID != convoy.ID {
		t.Fatalf("TrackingConvoysForItem = %v, want [%s]", convoys, convoy.ID)
	}

	// Without the tail, a cross-store member is invisible: its gc.tracking_convoy_id
	// ref lives with the item in the other store, so the convoy store alone has no
	// record of it (never a wrong bead, never a hard error). Real readers (drain)
	// always pass the member stores.
	soloMembers, err := Members(convoyStore, convoy.ID, true)
	if err != nil {
		t.Fatalf("Members single-store: %v", err)
	}
	if len(soloMembers) != 0 {
		t.Fatalf("single-store Members = %v, want none (the member's ref lives in the other store)", soloMembers)
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
