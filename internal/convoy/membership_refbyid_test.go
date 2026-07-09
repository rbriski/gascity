package convoy

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// TestMembership_LegacyTracksDepWithoutMetadata proves backward compatibility: data
// created before the ref-by-id change (a `tracks` dep, no gc.tracking_convoy_id) still
// resolves in both directions via the legacy dep fallback — no migration required.
func TestMembership_LegacyTracksDepWithoutMetadata(t *testing.T) {
	s := beads.NewMemStore()
	convoy, _ := s.Create(beads.Bead{Type: "convoy", Title: "legacy convoy"})
	item, _ := s.Create(beads.Bead{Type: "task", Title: "legacy item"})
	// Legacy shape: the tracks dep exists, no metadata stamp.
	if err := s.DepAdd(convoy.ID, item.ID, TrackingDepType); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	members, err := Members(s, convoy.ID, false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != item.ID {
		t.Fatalf("legacy Members = %v, want [%s] via dep fallback", members, item.ID)
	}
	convoys, err := TrackingConvoysForItem(s, item.ID)
	if err != nil {
		t.Fatalf("TrackingConvoysForItem: %v", err)
	}
	if len(convoys) != 1 || convoys[0].ID != convoy.ID {
		t.Fatalf("legacy TrackingConvoysForItem = %v, want [%s] via dep fallback", convoys, convoy.ID)
	}
}

// TestTrackItem_SameStoreStampsMetadataAndKeepsDep: same-store TrackItem does both —
// stamps the metadata ref AND keeps the legacy dep (dual-write), so dep-based readers
// and views are unaffected while the metadata path also works.
func TestTrackItem_SameStoreStampsMetadataAndKeepsDep(t *testing.T) {
	s := beads.NewMemStore()
	convoy, _ := s.Create(beads.Bead{Type: "convoy"})
	item, _ := s.Create(beads.Bead{Type: "task"})
	if err := TrackItem(s, convoy.ID, item.ID); err != nil {
		t.Fatalf("TrackItem: %v", err)
	}
	if ok, _ := HasTrack(s, convoy.ID, item.ID); !ok {
		t.Fatal("same-store TrackItem must keep the tracks dep")
	}
	got, _ := s.Get(item.ID)
	if got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != convoy.ID {
		t.Fatalf("same-store TrackItem must also stamp gc.tracking_convoy_id=%s; got %v", convoy.ID, got.Metadata)
	}
}

// TestUntrackItem_ClearsRefByIdSameStore: after untrack, the metadata ref is cleared
// AND the dep is removed, so Members no longer returns the item.
func TestUntrackItem_ClearsRefByIdSameStore(t *testing.T) {
	s := beads.NewMemStore()
	convoy, _ := s.Create(beads.Bead{Type: "convoy"})
	item, _ := s.Create(beads.Bead{Type: "task"})
	if err := TrackItem(s, convoy.ID, item.ID); err != nil {
		t.Fatalf("TrackItem: %v", err)
	}
	if err := UntrackItem(s, convoy.ID, item.ID); err != nil {
		t.Fatalf("UntrackItem: %v", err)
	}
	if got, _ := s.Get(item.ID); got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != "" {
		t.Fatalf("untrack must clear gc.tracking_convoy_id; got %q", got.Metadata[beadmeta.TrackingConvoyIDMetadataKey])
	}
	members, err := Members(s, convoy.ID, false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("Members after untrack = %v, want none", members)
	}
}

// TestUntrackItem_ClearsRefByIdCrossStore: untrack reaches a work-class member in a
// different store via the variadic tail and clears its ref (no dep exists cross-store).
func TestUntrackItem_ClearsRefByIdCrossStore(t *testing.T) {
	convoyStore := beads.NewMemStore()
	memberStore := beads.NewMemStore()
	convoy, _ := convoyStore.Create(beads.Bead{Type: "convoy"})
	member, _ := memberStore.Create(beads.Bead{Type: "task"})
	if err := TrackItem(convoyStore, convoy.ID, member.ID, memberStore); err != nil {
		t.Fatalf("TrackItem cross-store: %v", err)
	}
	if err := UntrackItem(convoyStore, convoy.ID, member.ID, memberStore); err != nil {
		t.Fatalf("UntrackItem cross-store: %v", err)
	}
	if got, _ := memberStore.Get(member.ID); got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != "" {
		t.Fatalf("cross-store untrack must clear gc.tracking_convoy_id; got %q", got.Metadata[beadmeta.TrackingConvoyIDMetadataKey])
	}
	members, err := Members(convoyStore, convoy.ID, false, memberStore)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("Members after cross-store untrack = %v, want none", members)
	}
}

// workOnlyReadStore mirrors cmd/gc's beadPolicyStore split topology: a graph convoy is
// physically created in a distinct graph store (gcg- prefix) but the store's own
// Get/DepAdd/SetMetadata only see the embedded work store. This is the exact shape that
// made the OLD TrackItem fail with "resolving issue ID gcg-2: no issue found" — its
// DepAdd hit the work store, which cannot resolve the graph convoy.
type workOnlyReadStore struct {
	beads.Store             // work store: Get/DepAdd/SetMetadata land here
	graph       beads.Store // where ClassGraph beads (convoys) physically live
}

func (s *workOnlyReadStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == "convoy" {
		return s.graph.Create(b) // route convoy to the graph store, like createTarget
	}
	return s.Store.Create(b)
}

// TestTrackItem_PolicyStoreTopologySkipsCrossStoreDep is the regression guard for the
// split-store escalation: a single store handle whose Create routes a convoy to a graph
// backend it cannot itself read back. TrackItem must record membership via the metadata
// ref (on the work member) and skip the cross-store dep — never hard-fail on DepAdd.
func TestTrackItem_PolicyStoreTopologySkipsCrossStoreDep(t *testing.T) {
	work := beads.NewMemStore()
	graph, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := graph.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	policy := &workOnlyReadStore{Store: work, graph: graph}

	member, _ := policy.Create(beads.Bead{Type: "task", Title: "work member"})
	convoy, _ := policy.Create(beads.Bead{Type: "convoy", Title: "input convoy"})

	if err := TrackItem(policy, convoy.ID, member.ID); err != nil {
		t.Fatalf("TrackItem through policy-store topology: %v", err)
	}
	if got, _ := work.Get(member.ID); got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != convoy.ID {
		t.Fatalf("member missing gc.tracking_convoy_id=%s; got %v", convoy.ID, got.Metadata)
	}
	if ok, _ := HasTrack(policy, convoy.ID, member.ID); ok {
		t.Fatal("cross-store dep must be skipped through the policy-store topology")
	}
	members, err := Members(policy, convoy.ID, false)
	if err != nil {
		t.Fatalf("Members: %v", err)
	}
	if len(members) != 1 || members[0].ID != member.ID {
		t.Fatalf("Members = %v, want [%s] via metadata", members, member.ID)
	}
}

// federatingReadStore models the POST-ccebee78b beadPolicyStore: Create routes a
// convoy to the graph store, and Get FEDERATES across [work, graph]. The original
// TrackItem guard probed store.Get(convoyID) to decide "same-store, safe to dep-add";
// the federating Get resolves the graph convoy through this work handle, so that guard
// passed and re-fired the cross-store dep-add ("resolving gcg-2"). The prefix guard
// must skip the dep regardless of what Get can resolve.
type federatingReadStore struct {
	beads.Store             // work store
	graph       beads.Store // graph store (gcg-)
}

func (s *federatingReadStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.Type == "convoy" {
		return s.graph.Create(b)
	}
	return s.Store.Create(b)
}

func (s *federatingReadStore) Get(id string) (beads.Bead, error) {
	if b, err := s.Store.Get(id); err == nil {
		return b, nil
	}
	return s.graph.Get(id) // federate — the ccebee78b behavior
}

func TestTrackItem_FederatingGetStillSkipsCrossStoreDep(t *testing.T) {
	work := beads.NewMemStore()
	graph, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := graph.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	store := &federatingReadStore{Store: work, graph: graph}

	member, _ := store.Create(beads.Bead{Type: "task", Title: "work member"})    // gc- in work
	convoy, _ := store.Create(beads.Bead{Type: "convoy", Title: "graph convoy"}) // gcg- in graph

	// Guard: store.Get(convoy) FEDERATES and would resolve it — the old guard's trap.
	if _, err := store.Get(convoy.ID); err != nil {
		t.Fatalf("precondition: federating Get must resolve the graph convoy: %v", err)
	}
	if err := TrackItem(store, convoy.ID, member.ID); err != nil {
		t.Fatalf("TrackItem must not hard-fail on the cross-store pair: %v", err)
	}
	// The metadata ref is stamped (authoritative), but NO cross-store dep is written.
	if got, _ := work.Get(member.ID); got.Metadata[beadmeta.TrackingConvoyIDMetadataKey] != convoy.ID {
		t.Fatalf("member missing gc.tracking_convoy_id=%s; got %v", convoy.ID, got.Metadata)
	}
	if ok, _ := HasTrack(store, convoy.ID, member.ID); ok {
		t.Fatal("cross-store dep must be skipped even when Get federates (prefix guard), else the gcg-2 regression is back")
	}
}
