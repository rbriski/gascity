package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// This file pins the WARM rig-pool half of the graph-demand federation fix,
// in the incident's exact shape (maintainer-city, 2026-07: template
// gascity/gc.implementation-worker, step gcg-wisp-xqsan6 invisible for 40+
// minutes while one session, mc-rtg7n, held resume-tier work). The city-pool
// half lives in split_store_city_pool_demand_verify_test.go.
//
// The invariant under test: a rig pool's default scale-check demand = own rig
// store + city (leading) store, dedup'd — UNCONDITIONALLY, not only when the
// pool is cold. Routed graph-class molecule steps are delivered at city scope
// (vp-kvp cross-store delivery), which the rig pool's own-store probe cannot
// see. When the city-store arm was gated on isCold (runningSessions == 0 &&
// min == 0), a pool kept warm by even ONE resume-tier session never probed
// the city store: fresh molecule steps produced zero new-demand every warm
// tick and sat unassigned for 20min-3h until the pool happened to go cold.
// The gate was dropped in the treadmill fix (build_desired_state.go, both the
// generic-pool and named-backing arms); these tests hold that line with the
// pool warm the way production pools are warm — holding claimed work.

// mintGraphRoutedStep stages one graph-shaped routed molecule step (the wisp
// shape production materialization mints: type task, gc.kind=wisp) in store.
// A non-empty assignee stages the claimed, in-flight state via post-create
// mutation, exactly as production claims are.
func mintGraphRoutedStep(t *testing.T, store beads.Store, routedTo, assignee string) {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title: "routed graph step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     beadmeta.KindWisp,
			beadmeta.RoutedToMetadataKey: routedTo,
		},
	})
	if err != nil {
		t.Fatalf("minting routed step: %v", err)
	}
	if assignee == "" {
		return
	}
	status := "in_progress"
	if err := store.Update(created.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
		t.Fatalf("staging claimed step %s: %v", created.ID, err)
	}
}

// TestSplitCityDemand_WarmRigPoolSeesGraphRoutedStep stages the live incident:
// a rig-scoped default-probe pool whose one live session HOLDS a claimed
// routed step (the resume-tier accept that kept the pool warm), plus a fresh
// OPEN, UNASSIGNED graph-class step routed to the same pool, resident in the
// leading city store. The warm tick's demand count MUST include the fresh
// step — exactly once: the claimed step is resume work, not new demand, and
// the own-store + city probes must not double-count across store groups.
func TestSplitCityDemand_WarmRigPoolSeesGraphRoutedStep(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	sess, err := store.Create(newWarmPoolSessionBead(qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create warm pool session bead: %v", err)
	}
	// The warmth source: claimed, in-flight routed work held by the live
	// session. The pool is warm BECAUSE of resume work, and that resume work
	// must neither hide the fresh step nor leak into the new-demand count.
	mintGraphRoutedStep(t, store, qualified, sess.ID)
	// The fresh unassigned routed step (gcg-wisp-xqsan6 shape).
	mintGraphRoutedStep(t, store, qualified, "")

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Fatalf("warm rig-pool demand for %s = %d, want exactly 1 (the fresh unassigned routed step must be counted once on a WARM tick — 0 is the isCold-gate blindness that stranded steps for 20min-3h live; 2+ is a double-count across the rig/city store groups)", qualified, got)
	}
}

// TestSplitCityDemand_WarmAliasedRigStoreNoDoubleCount is the identity case
// for the now-unconditional probe: a rig whose store aliases the city store
// (an unbound rig falling back to the city scope — rigStores maps the rig
// name to the SAME handle the reconciler leads with). The ownTarget.store !=
// store guard must skip the extra city arm, so one routed step counts exactly
// once. The cold flavor of the aliased-store defense predates this fix; this
// is the warm flavor, which only exists now that the probe is no longer
// cold-gated — defaultScaleCheckCounts dedups per storeKey group, not across
// groups, so a second "city" group over the same physical store would count
// the same bead twice on every warm tick.
func TestSplitCityDemand_WarmAliasedRigStoreNoDoubleCount(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	// Slot 2 (not 1): a pool whose first slot drained earlier is a valid warm
	// state, and the demand invariant must not key on slot numbering.
	sess, err := store.Create(newWarmPoolSessionBead(qualified, "executor-2", "2"))
	if err != nil {
		t.Fatalf("create warm pool session bead: %v", err)
	}
	// Alias the rig store to the leading store the reconciler is handed.
	rigStores["rig-A"] = store
	mintGraphRoutedStep(t, store, qualified, "")

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Fatalf("warm aliased-rig demand for %s = %d, want exactly 1 (rig store == city store must yield one demand unit per step, not one per store group)", qualified, got)
	}
}
