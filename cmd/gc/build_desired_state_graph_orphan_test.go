package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestCollectAssignedWorkBeads_CapturesGraphResidentInProgressOrphan is the
// assigned-work-scan analog of the Seam C demand fix (collectOpenUnassignedRoutedWork).
// Under graph-class relocation (graph_store=sqlite/postgres) a molecule's step
// beads live in a dedicated graph store, physically separate from the work/rig
// stores that the in_progress and open-routed passes scan (they read source.store,
// which is work-only). A step bead stranded in_progress by a dead session must
// still be captured for orphan release, with the graph store recorded as its owner
// store, or releaseOrphanedPoolAssignments never sees it and the dead session's
// drain stays wedged.
func TestCollectAssignedWorkBeads_CapturesGraphResidentInProgressOrphan(t *testing.T) {
	cityStore := beads.NewMemStore()
	graphStore := beads.NewMemStore() // a separate handle => relocatedGraph == true

	// A routed step bead stranded in_progress by a dead session, living ONLY in
	// the graph store.
	created, err := graphStore.Create(beads.Bead{
		Title:    "graph-resident step orphaned by a dead session",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	inProgress, deadAssignee := "in_progress", "pool__worker-gc-session-deadbeef"
	if err := graphStore.Update(created.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &deadAssignee}); err != nil {
		t.Fatalf("strand graph bead in_progress: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	found, foundStores, _, readyIDs, partial := collectAssignedWorkBeadsWithStores(cfg, cityStore, graphStore, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}

	idx := -1
	for i, b := range found {
		if b.ID == created.ID {
			idx = i
			break
		}
	}
	if idx == -1 {
		t.Fatalf("graph-resident in_progress orphan %s was not captured; got %#v", created.ID, found)
	}
	if foundStores[idx] != graphStore {
		t.Fatalf("captured orphan owner store index-aligned to the wrong store: got %p, want the graph store %p", foundStores[idx], graphStore)
	}
	// Constraint: a bead captured solely for orphan-release must NOT enter the
	// wake-demand readiness set by status alone — a stranded gcg- must not hold a
	// dead session awake.
	if readyIDs[created.ID] {
		t.Fatalf("graph orphan %s must not be marked ready-assigned", created.ID)
	}
}

// TestCollectAssignedWorkBeads_GraphOrphanPassNoOpAtBDDefault pins the bd-default
// invariant: when graphStore == cityStore (relocatedGraph == false) the dedicated
// graph orphan-release pass must not run, so the in_progress orphan is captured
// exactly once (by the ordinary city work leg) — no duplicate, and its readiness
// flag stays whatever the ordinary in_progress pass records (true).
func TestCollectAssignedWorkBeads_GraphOrphanPassNoOpAtBDDefault(t *testing.T) {
	cityStore := beads.NewMemStore()

	created, err := cityStore.Create(beads.Bead{
		Title:    "work-resident step orphaned by a dead session",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress, deadAssignee := "in_progress", "pool__worker-gc-session-deadbeef"
	if err := cityStore.Update(created.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &deadAssignee}); err != nil {
		t.Fatalf("strand work bead in_progress: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	// graphStore == cityStore: the bd default. resolveGraphStore returns the work
	// store, so relocatedGraph is false and no dedicated graph pass runs.
	found, _, _, _, partial := collectAssignedWorkBeadsWithStores(cfg, cityStore, cityStore, nil, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	count := 0
	for _, b := range found {
		if b.ID == created.ID {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("bd-default (graphStore==cityStore) captured the orphan %d times, want exactly 1 (no duplicate graph pass)", count)
	}
}
