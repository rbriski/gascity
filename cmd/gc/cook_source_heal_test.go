package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// errOnGetStore wraps a store and returns a synthetic non-not-found error for
// Get on one id — the degraded-graph-leg case the heal must fail-safe on.
type errOnGetStore struct {
	beads.Store
	failID string
}

func (s errOnGetStore) Get(id string) (beads.Bead, error) {
	if id == s.failID {
		return beads.Bead{}, errors.New("graph leg down")
	}
	return s.Store.Get(id)
}

func mkCookRoot(t *testing.T, st *beads.MemStore, status, outcome string) beads.Bead {
	t.Helper()
	md := map[string]string{}
	if outcome != "" {
		md[beadmeta.OutcomeMetadataKey] = outcome
	}
	b, err := st.Create(beads.Bead{Title: "cook root", Metadata: md})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	if status != "" && status != "open" {
		if err := st.Update(b.ID, beads.UpdateOpts{Status: &status}); err != nil {
			t.Fatalf("set root status: %v", err)
		}
	}
	got, err := st.Get(b.ID)
	if err != nil {
		t.Fatalf("reget root: %v", err)
	}
	return got
}

func mkCookSource(t *testing.T, st *beads.MemStore, markerRoot string) beads.Bead {
	t.Helper()
	b, err := st.Create(beads.Bead{Title: "cook source", Metadata: map[string]string{
		beadmeta.CookAttachLaunchMetadataKey: markerRoot,
	}})
	if err != nil {
		t.Fatalf("create source: %v", err)
	}
	ip := "in_progress"
	if err := st.Update(b.ID, beads.UpdateOpts{Status: &ip}); err != nil {
		t.Fatalf("set source in_progress: %v", err)
	}
	got, err := st.Get(b.ID)
	if err != nil {
		t.Fatalf("reget source: %v", err)
	}
	return got
}

func healOne(cityStore beads.Store, src beads.Bead, srcStore beads.Store) []string {
	return healStrandedCookAttachSources(cityStore, []beads.Bead{src}, []beads.Store{srcStore})
}

func statusOf(t *testing.T, st beads.Store, id string) beads.Bead {
	t.Helper()
	b, err := st.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func TestHealCookSource_FailedRootReopens(t *testing.T) {
	st := beads.NewMemStore()
	root := mkCookRoot(t, st, "closed", "fail")
	src := mkCookSource(t, st, root.ID)

	healed := healOne(st, src, st)

	if len(healed) != 1 || healed[0] != src.ID {
		t.Fatalf("want healed=[%s], got %v", src.ID, healed)
	}
	got := statusOf(t, st, src.ID)
	if got.Status != "open" {
		t.Errorf("status = %q, want open", got.Status)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != "fail" {
		t.Errorf("outcome = %q, want fail", got.Metadata[beadmeta.OutcomeMetadataKey])
	}
	if got.Metadata[beadmeta.CookAttachLaunchMetadataKey] != "" {
		t.Errorf("marker not cleared: %q", got.Metadata[beadmeta.CookAttachLaunchMetadataKey])
	}

	// Idempotent: re-running against the same (stale) candidate is a no-op
	// because the live source's marker is now cleared.
	if again := healOne(st, src, st); len(again) != 0 {
		t.Errorf("second heal not idempotent: %v", again)
	}
}

func TestHealCookSource_PassedRootCloses(t *testing.T) {
	st := beads.NewMemStore()
	root := mkCookRoot(t, st, "closed", "pass")
	src := mkCookSource(t, st, root.ID)

	healed := healOne(st, src, st)

	if len(healed) != 1 {
		t.Fatalf("want 1 heal, got %v", healed)
	}
	got := statusOf(t, st, src.ID)
	if got.Status != "closed" {
		t.Errorf("status = %q, want closed (pass must close, not reopen)", got.Status)
	}
	if got.Metadata[beadmeta.OutcomeMetadataKey] != "pass" {
		t.Errorf("outcome = %q, want pass", got.Metadata[beadmeta.OutcomeMetadataKey])
	}
}

func TestHealCookSource_PurgedRootReopens(t *testing.T) {
	st := beads.NewMemStore()
	src := mkCookSource(t, st, "gc-999999") // marker points at a nonexistent root

	healed := healOne(st, src, st)

	if len(healed) != 1 {
		t.Fatalf("want 1 heal (purged root reopens), got %v", healed)
	}
	if got := statusOf(t, st, src.ID); got.Status != "open" {
		t.Errorf("status = %q, want open", got.Status)
	}
}

func TestHealCookSource_LiveRootUntouched(t *testing.T) {
	st := beads.NewMemStore()
	root := mkCookRoot(t, st, "open", "") // molecule still live
	src := mkCookSource(t, st, root.ID)

	healed := healOne(st, src, st)

	if len(healed) != 0 {
		t.Fatalf("live root must not heal, got %v", healed)
	}
	if got := statusOf(t, st, src.ID); got.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress (untouched)", got.Status)
	}
}

func TestHealCookSource_ReadErrorIsFailSafe(t *testing.T) {
	st := beads.NewMemStore()
	root := mkCookRoot(t, st, "closed", "fail")
	src := mkCookSource(t, st, root.ID)

	// The city Router errors (non-not-found) reading the marker root.
	cityStore := errOnGetStore{Store: st, failID: root.ID}
	healed := healStrandedCookAttachSources(cityStore, []beads.Bead{src}, []beads.Store{st})

	if len(healed) != 0 {
		t.Fatalf("read error must fail-safe (no heal), got %v", healed)
	}
	if got := statusOf(t, st, src.ID); got.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress (untouched)", got.Status)
	}
}

func TestHealCookSource_AssignedSourceUntouched(t *testing.T) {
	st := beads.NewMemStore()
	root := mkCookRoot(t, st, "closed", "fail")
	src := mkCookSource(t, st, root.ID)
	// Race: a worker claimed the source after the scan captured it.
	assignee := "worker-x"
	if err := st.Update(src.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatal(err)
	}

	healed := healOne(st, src, st)

	if len(healed) != 0 {
		t.Fatalf("assigned source must not heal, got %v", healed)
	}
	if got := statusOf(t, st, src.ID); got.Status != "in_progress" {
		t.Errorf("status = %q, want in_progress", got.Status)
	}
}

func TestHealCookSource_EmptyMarkerSkipped(t *testing.T) {
	st := beads.NewMemStore()
	b, _ := st.Create(beads.Bead{Title: "no marker"})
	ip := "in_progress"
	_ = st.Update(b.ID, beads.UpdateOpts{Status: &ip})
	src := statusOf(t, st, b.ID)

	if healed := healOne(st, src, st); len(healed) != 0 {
		t.Fatalf("empty-marker source must be skipped, got %v", healed)
	}
}

func TestCollectStrandedCookSources_CapturesOnlyStrandedMarked(t *testing.T) {
	city := beads.NewMemStore()
	rig := beads.NewMemStore()

	// city: one genuine strand (captured)
	strand := mkCookSource(t, city, "gc-root-1")
	// city: marked but assigned -> not a strand
	assigned := mkCookSource(t, city, "gc-root-2")
	who := "worker"
	_ = city.Update(assigned.ID, beads.UpdateOpts{Assignee: &who})
	// city: in_progress but no marker -> human/other work, untouched
	noMarker, _ := city.Create(beads.Bead{Title: "human"})
	ip := "in_progress"
	_ = city.Update(noMarker.ID, beads.UpdateOpts{Status: &ip})
	// city: marked but still open (not in_progress) -> not captured
	openMarked, _ := city.Create(beads.Bead{Title: "open marked", Metadata: map[string]string{beadmeta.CookAttachLaunchMetadataKey: "gc-root-3"}})
	_ = openMarked
	// rig: a strand in a rig store (captured, with its own store)
	rigStrand := mkCookSource(t, rig, "gc-root-4")

	// Note: independent MemStores restart their id sequence, so the city and rig
	// strands can share an id — assert by count + owning-store identity rather
	// than an id-keyed map. (assigned/noMarker/openMarked have distinct city
	// ids gc-2/gc-3/gc-4, so a city-store capture count of 1 proves they were
	// excluded.)
	sources, stores := collectStrandedCookSources(city, map[string]beads.Store{"rig-a": rig})

	if len(sources) != len(stores) {
		t.Fatalf("index misalignment: %d beads vs %d stores", len(sources), len(stores))
	}
	if len(sources) != 2 {
		t.Fatalf("want 2 strands captured, got %d", len(sources))
	}
	cityCount, rigCount := 0, 0
	for i, s := range sources {
		if s.Assignee != "" {
			t.Errorf("captured an assigned bead %s", s.ID)
		}
		if s.Metadata[beadmeta.CookAttachLaunchMetadataKey] == "" {
			t.Errorf("captured an unmarked bead %s", s.ID)
		}
		if s.Status != "in_progress" {
			t.Errorf("captured a non-in_progress bead %s (%s)", s.ID, s.Status)
		}
		switch stores[i] {
		case beads.Store(city):
			cityCount++
		case beads.Store(rig):
			rigCount++
		default:
			t.Errorf("captured with an unexpected store")
		}
	}
	if cityCount != 1 {
		t.Errorf("want exactly 1 city strand (assigned/unmarked/open excluded), got %d", cityCount)
	}
	if rigCount != 1 {
		t.Errorf("want exactly 1 rig strand, got %d", rigCount)
	}
	_ = strand
	_ = rigStrand
}
