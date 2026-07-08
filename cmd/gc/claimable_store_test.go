package main

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// erroringReadyStore is a beads.Store whose Ready/List error, for the fail-loud
// test. It embeds a nil Store; the composite only invokes Ready and List.
type erroringReadyStore struct{ beads.Store }

func (erroringReadyStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return nil, errors.New("infra store unavailable")
}

func (erroringReadyStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("infra store unavailable")
}

func mustCreateBead(t *testing.T, s beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := s.Create(b)
	if err != nil {
		t.Fatalf("create %q: %v", b.ID, err)
	}
	return created
}

func beadIDSet(items []beads.Bead) map[string]bool {
	set := make(map[string]bool, len(items))
	for _, b := range items {
		set[b.ID] = true
	}
	return set
}

func TestClaimableStoreReadyMergesAndDedupes(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	infra := beads.NewMemStoreHonoringIDs()
	mustCreateBead(t, work, beads.Bead{ID: "ga-1", Title: "w1", Type: "task"})
	mustCreateBead(t, work, beads.Bead{ID: "ga-2", Title: "w2", Type: "task"})
	mustCreateBead(t, infra, beads.Bead{ID: "gcg-1", Title: "step", Type: "task"})
	// Defensive dedupe: the same id in both legs must collapse to one row.
	mustCreateBead(t, work, beads.Bead{ID: "dup-1", Title: "d", Type: "task"})
	mustCreateBead(t, infra, beads.Bead{ID: "dup-1", Title: "d", Type: "task"})

	cs := &claimableStore{work: work, infra: infra}
	got, err := cs.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	ids := beadIDSet(got)
	for _, want := range []string{"ga-1", "ga-2", "gcg-1", "dup-1"} {
		if !ids[want] {
			t.Errorf("merged ready set missing %q; got %v", want, ids)
		}
	}
	if len(got) != 4 {
		t.Fatalf("merged ready = %d beads, want 4 (deduped); got %v", len(got), ids)
	}
	if !ids["gcg-1"] {
		t.Fatal("infra step gcg-1 not surfaced by the composite ready read (the whole point of the composite)")
	}
}

func TestClaimableStoreRoutesByPrefix(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	infra := beads.NewMemStoreHonoringIDs()

	split := &claimableStore{work: work, infra: infra}
	if split.storeForID("gcg-1") != beads.Store(infra) {
		t.Error("gcg- id must route to the infra store on a split city")
	}
	if split.storeForID("ga-1") != beads.Store(work) {
		t.Error("non-reserved id must route to the work store")
	}

	// Single-store: everything routes to work (gcg- included).
	single := &claimableStore{work: work, infra: nil}
	if single.storeForID("gcg-1") != beads.Store(work) {
		t.Error("gcg- id must route to the work store when there is no infra store")
	}
}

func TestClaimableStoreReadyFailsLoudOnLegError(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	mustCreateBead(t, work, beads.Bead{ID: "ga-1", Type: "task"})

	// Infra leg errors → the whole read must error, never silently return
	// work-only rows (that fail-open would hide infra-resident graph work).
	cs := &claimableStore{work: work, infra: erroringReadyStore{}}
	if _, err := cs.Ready(beads.ReadyQuery{}); err == nil {
		t.Fatal("Ready must fail loud when the infra leg errors, got nil error")
	}
	// Work leg errors → also fatal.
	cs2 := &claimableStore{work: erroringReadyStore{}, infra: beads.NewMemStoreHonoringIDs()}
	if _, err := cs2.Ready(beads.ReadyQuery{}); err == nil {
		t.Fatal("Ready must fail loud when the work leg errors, got nil error")
	}
}

func TestClaimableStoreCollapsesSingleStore(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	mustCreateBead(t, work, beads.Bead{ID: "ga-1", Type: "task"})
	cs := &claimableStore{work: work, infra: nil}
	got, err := cs.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(got) != 1 || got[0].ID != "ga-1" {
		t.Fatalf("single-store Ready = %v, want [ga-1]", beadIDSet(got))
	}
}

func TestSortReadyBeadsCanonical(t *testing.T) {
	p0, p2 := 0, 2
	items := []beads.Bead{
		{ID: "b", Priority: nil}, // nil priority sorts as 2
		{ID: "a", Priority: &p0}, // priority 0 → first
		{ID: "c", Priority: &p2}, // priority 2
	}
	sortReadyBeadsCanonical(items)
	got := []string{items[0].ID, items[1].ID, items[2].ID}
	// priority 0 (a) first; b (nil→2) and c (2) tie on priority and created_at
	// (both zero) so break by id ascending: b before c.
	want := []string{"a", "b", "c"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("canonical ready order = %v, want %v", got, want)
		}
	}
}

func TestClaimableReady_AttachParentBlockedWhileInfraRootOpen(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	infra := beads.NewMemStoreHonoringIDs()
	mustCreateBead(t, work, beads.Bead{
		ID: "ga-parent", Title: "attach parent", Type: "task",
		Metadata: beads.StringMap{beadmeta.AttachedWorkflowRootMetadataKey: "gcg-root"},
	})
	mustCreateBead(t, infra, beads.Bead{ID: "gcg-root", Title: "workflow root", Type: "task"})
	cs := &claimableStore{work: work, infra: infra}

	// Root open -> parent is blocked (cross-store attach dep), excluded from Ready.
	got, err := cs.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if beadIDSet(got)["ga-parent"] {
		t.Fatal("attach parent ga-parent is READY while its attached workflow root gcg-root is open in infra (cross-store attach fail-open, landmine #4)")
	}

	// Root closed (DAG done) -> parent unblocks.
	if err := infra.Close("gcg-root"); err != nil {
		t.Fatalf("close root: %v", err)
	}
	got, err = cs.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("Ready after root close: %v", err)
	}
	if !beadIDSet(got)["ga-parent"] {
		t.Fatal("attach parent should be claimable once its workflow root is closed")
	}
}

func TestClaimableReady_DanglingAttachRootFailsLoud(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	infra := beads.NewMemStoreHonoringIDs()
	mustCreateBead(t, work, beads.Bead{
		ID: "ga-parent", Title: "p", Type: "task",
		Metadata: beads.StringMap{beadmeta.AttachedWorkflowRootMetadataKey: "gcg-missing"},
	})
	cs := &claimableStore{work: work, infra: infra}
	if _, err := cs.Ready(beads.ReadyQuery{}); err == nil {
		t.Fatal("a dangling gc.attached_workflow_root must fail loud, not fall open")
	}
}
