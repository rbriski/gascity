package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file CHARACTERIZES the split-city reconciler frame as it is actually
// wired on feat/split-store-conformance, to ground the "3 GAPS" differential in
// reproducible facts rather than a source read.
//
// The load-bearing wiring fact: on a split city the daemon reconciler passes the
// SESSIONS-class store as the leading (`store`) argument to
// buildDesiredStateWithSessionBeads (city_runtime.go:3215-3217 and :2985-2996,
// cmd_supervisor.go:2624-2625, cmd_start.go:922-932). resolveSessionStore routes
// the sessions class to the INFRA store on a split city (class_store.go:275 →
// resolveClassStore:244), and controllerState.cityInfraStore is populated on a
// split city (api_state.go:169-172). So the leading `store` — i.e. the index-0
// "city" candidate of coordClassStoreCandidates (session_beads.go:699) — IS the
// infra store on a split-city daemon frame, NOT the work store.
//
// Consequence, proven below: the infra store is ALREADY scanned by the work-arm
// reconciler collectors (as leg 0), and an infra-resident routed `gcg-` bead is
// captured with the INFRA store as its index-aligned owner. The gap the
// differential names as "infra never scanned" does not reproduce on this branch;
// the store actually dropped from the split-city frame is the city WORK store
// (documented in TestSplitReconcilerFrame_CityWorkStoreIsDroppedNotInfra).

func mkInProgressRouted(t *testing.T, s beads.Store, id, assignee, routedTo string) beads.Bead {
	t.Helper()
	b, err := s.Create(beads.Bead{
		ID:       id,
		Title:    id,
		Type:     "step",
		Assignee: assignee,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: routedTo},
	})
	if err != nil {
		t.Fatalf("create %s: %v", id, err)
	}
	inProgress := "in_progress"
	if err := s.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("update %s to in_progress: %v", id, err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return got
}

// TestSplitReconcilerFrame_InfraIsLeg0AndOwnsItsBeads proves that when the leading
// store IS the infra store (the split-city daemon frame), collectAssignedWork-
// BeadsWithStores captures an infra-resident routed gcg- orphan with the INFRA
// store as its index-aligned owner — so releaseOrphanedPoolAssignments would
// write the release back to the infra store. This is the coverage the "orphan-
// release GAP" claims is missing; it is present via the leg-0 = infra wiring.
func TestSplitReconcilerFrame_InfraIsLeg0AndOwnsItsBeads(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)

	orphan := mkInProgressRouted(t, infra, "gcg-step-orphan", "dead-session-xyz", "worker")

	// Daemon frame on a split city: leading store = sessions store = infra.
	got, stores, refs, _, partial := collectAssignedWorkBeadsWithStores(cfg, infra, nil, nil, nil)
	if partial {
		t.Fatalf("unexpected partial result")
	}

	idx := -1
	for i, b := range got {
		if b.ID == orphan.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("infra-resident routed gcg- orphan %q not captured; got %d beads: %v",
			orphan.ID, len(got), beadIDsList(got))
	}
	if !sameStorePtr(stores[idx], infra) {
		t.Fatalf("captured %q with owner store %p, want the INFRA store %p (index-aligned owner drives orphan release write-back)",
			orphan.ID, stores[idx], infra)
	}
	// Leg-0 records the city store under the empty ref (assigned-work arm).
	if refs[idx] != "" {
		t.Fatalf("captured %q with store ref %q, want the leg-0 empty city ref", orphan.ID, refs[idx])
	}
}

// TestSplitReconcilerFrame_OpenUnassignedInfraSeenAtLeg0 proves collectOpen-
// UnassignedRoutedWork sees an infra-resident open, unassigned, routed bead with
// the infra store as its owner on the split-city daemon frame — the coverage the
// "spawn-demand GAP" claims is missing.
func TestSplitReconcilerFrame_OpenUnassignedInfraSeenAtLeg0(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)

	open, err := infra.Create(beads.Bead{
		ID:       "gcg-step-open",
		Title:    "open routed",
		Type:     "step",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
	})
	if err != nil {
		t.Fatalf("create open routed: %v", err)
	}

	got, stores := collectOpenUnassignedRoutedWork(cfg, infra, nil, nil, nil)
	idx := -1
	for i, b := range got {
		if b.ID == open.ID {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatalf("infra-resident open routed bead %q not seen; got %d: %v", open.ID, len(got), beadIDsList(got))
	}
	if !sameStorePtr(stores[idx], infra) {
		t.Fatalf("open routed %q owner store %p, want INFRA %p", open.ID, stores[idx], infra)
	}
}

// TestSplitReconcilerFrame_CityWorkStoreIsDroppedNotInfra documents the ACTUAL
// gap on this branch (the inverse of the differential's premise): because the
// leading `store` is the infra store, the city WORK store (cityBeadStore, which
// on a split city is a DISTINCT store from the infra store) is not part of the
// reconciler frame — it is neither the leading store nor a member of rigStores
// (rigBeadStores() deletes the cityName key, city_runtime.go:3149-3156). A
// city-scope routed orphan is therefore invisible to the work-arm collectors.
//
// This is deliberately a characterization of current behavior. It is asserted as
// the present reality so the eventual fix (thread the city WORK store into the
// split-city work-arm frame) has a red anchor to flip.
func TestSplitReconcilerFrame_CityWorkStoreIsDroppedNotInfra(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	cityWork := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)

	cwOrphan := mkInProgressRouted(t, cityWork, "ga-citywork-orphan", "dead-session-abc", "worker")

	// The split-city daemon frame never receives the city WORK store: leading
	// store = infra, rigStores excludes the city. The city-scope orphan is missed.
	got, _, _, _, _ := collectAssignedWorkBeadsWithStores(cfg, infra, nil, nil, nil)
	for _, b := range got {
		if b.ID == cwOrphan.ID {
			t.Fatalf("city WORK store orphan %q was captured — the split-city frame now includes the city work store; "+
				"update this characterization test and confirm the work-arm frame fix landed", cwOrphan.ID)
		}
	}
}

func beadIDsList(list []beads.Bead) []string {
	ids := make([]string, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	return ids
}
