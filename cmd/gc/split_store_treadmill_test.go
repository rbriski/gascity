package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session"
)

// This file covers the split-city spawn/drain treadmill (mc-wisp-orphan RCA):
// routed orchestration wisps live in the INFRA store on a split city, but the
// pool-demand / assigned-work / drain-probe / release legs scanned only
// city+rig stores. The reconciler's leading store IS the sessions/infra store
// on a split city (CityRuntime.buildDesiredState passes sessionsBeadStore()),
// so these tests model the infra store as the leading store of
// buildDesiredStateWithSessionBeads, exactly as production wires it.
//
// Site 1 (driver): the cross-store demand probe was gated on isCold
// (runningSessions == 0), so the first WARM tick after a cold spawn computed
// demand=0, dropped every just-spawned session from desiredState, and drained
// them ~45s post-spawn — before the ~2min agent boot could claim. pool_desired
// cycled 5,0,0 for hours on the live trace. The probe must be unconditional.
//
// Sites 2-4 (post-claim survival): a CLAIMED infra wisp must remain reachable
// from the routed rig pool's agent (assigned-work store-ref ""), from the
// rig-bound session's drain probes (reachableStoresForSession), and from the
// orphan-release last-resort liveness check (session beads live in the infra
// store, not the work store).

// newWarmPoolSessionBead returns a live, pool-managed session bead for the
// given qualified template, shaped the way the runningSessions counter and the
// pool-reuse selection recognize it (type=session, gc:session label, active
// state, session_name + pool_slot identity).
func newWarmPoolSessionBead(qualified, sessionName, poolSlot string) beads.Bead {
	return beads.Bead{
		Title:  sessionName,
		Type:   "session",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": sessionName,
			"pool_managed": "true",
			"pool_slot":    poolSlot,
			"state":        "active",
		},
	}
}

// TestBuildDesiredState_WarmTick_RoutedDemandInLeadingStoreCounts is the T1
// driver regression: a rig-scoped default-probe pool with a RUNNING session
// (warm tick) must still count routed demand that lives only in the leading
// city/infra store. Before the fix the cross-store probe was appended only
// under isCold, so this demand read 0 on every warm tick.
func TestBuildDesiredState_WarmTick_RoutedDemandInLeadingStoreCounts(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	// One live pool session → runningSessions > 0 → NOT cold.
	sess, err := store.Create(newWarmPoolSessionBead(qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	// Routed demand ONLY in the leading store (the infra store on a split city).
	if _, err := store.Create(beads.Bead{
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("warm-tick cross-store demand = %d, want 1 (routed demand in the leading store must stay visible while sessions run)", got)
	}
}

// TestBuildDesiredState_WarmTick_TreadmillSessionsStayDesired is the T2
// treadmill regression, driven through the production spawn path: a cold tick
// materializes pool sessions for routed leading-store demand; on the next
// (warm) tick — with the wisps still open and unclaimed — the desired state
// must KEEP those sessions instead of dropping them to zero (which the
// reconciler then drains as "orphaned" before the agent can claim).
func TestBuildDesiredState_WarmTick_TreadmillSessionsStayDesired(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir()

	for i := 0; i < 2; i++ {
		if _, err := store.Create(beads.Bead{
			Status:    "open",
			Type:      "task",
			Ephemeral: true,
			Metadata:  map[string]string{"gc.routed_to": qualified},
		}); err != nil {
			t.Fatal(err)
		}
	}

	// Cold tick: demand=2 spawns two pool session beads through the production
	// create path.
	cold := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now(), cfg, &localMockProvider{},
		store, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)
	if len(cold.State) != 2 {
		t.Fatalf("cold tick desired sessions = %d, want 2", len(cold.State))
	}
	spawned, err := session.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("list spawned session beads: %v", err)
	}
	if len(spawned) != 2 {
		t.Fatalf("spawned session beads = %d, want 2", len(spawned))
	}

	// Warm tick: sessions exist and are live, wisps are STILL open/unclaimed
	// (the agents have not booted far enough to claim). The sessions must not
	// fall out of desiredState.
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	warm := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now(), cfg, &localMockProvider{},
		store, rigStores, snap, nil, os.Stderr,
	)
	if got := warm.ScaleCheckCounts[qualified]; got != 2 {
		t.Errorf("warm tick demand = %d, want 2 (treadmill: demand collapsed while wisps were still unclaimed)", got)
	}
	if len(warm.State) != 2 {
		t.Errorf("warm tick desired sessions = %d, want 2 (treadmill: just-spawned sessions fell out of desiredState)", len(warm.State))
	}
	// The warm tick must reuse the spawned sessions, not mint replacements.
	after, err := session.ListAllSessionBeads(store, beads.ListQuery{})
	if err != nil {
		t.Fatalf("list session beads after warm tick: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("session beads after warm tick = %d, want 2 (warm tick must reuse spawned sessions, not create new ones)", len(after))
	}
}

// TestBuildDesiredState_WarmTick_UnrelatedLeadingStoreWorkDoesNotScaleRigPool
// is the leak-safety guard for the unconditional cross-store probe: routed
// demand for OTHER templates (or unrouted work) in the leading store must not
// scale this rig pool on warm ticks. The count-form filters on
// gc.routed_to=<template>, so rig pools cannot scale on unrelated city work.
func TestBuildDesiredState_WarmTick_UnrelatedLeadingStoreWorkDoesNotScaleRigPool(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	sess, err := store.Create(newWarmPoolSessionBead(qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	// Unrelated work in the leading store: routed elsewhere, and unrouted.
	if _, err := store.Create(beads.Bead{
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": "other-rig/other-agent"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Status: "open",
		Type:   "task",
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 0 {
		t.Errorf("warm-tick demand = %d, want 0 (rig pool must not scale on unrelated leading-store work)", got)
	}
}

// TestBuildDesiredState_WarmTick_NamedBackingPoolSeesLeadingStoreDemand covers
// the named-backing sibling of the driver (build_desired_state.go's named
// branch): an on_demand named-backing pool with a running session must still
// see routed leading-store demand on a warm tick, clamped to 1 as always.
func TestBuildDesiredState_WarmTick_NamedBackingPoolSeesLeadingStoreDemand(t *testing.T) {
	cfg, store, rigStores, identity := newNoScaleCheckNamedBackingCity(t)

	sess, err := store.Create(newWarmPoolSessionBead(identity, "planner-1", "1"))
	if err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	if _, err := store.Create(beads.Bead{
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": identity},
	}); err != nil {
		t.Fatal(err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[identity]; got != 1 {
		t.Errorf("warm-tick named-backing demand = %d, want 1 (on_demand clamp, but visible)", got)
	}
}

// TestAssignedWorkIndexReachable_InfraLegOnSplitCity is the T3 site-2
// regression: the infra assigned-work leg is captured under store-ref "" (the
// leading store's arm in collectAssignedWorkBeadsWithStores). On a split city
// that leg must be reachable from a rig-bound agent — its claimed wisps LIVE
// there — while other rigs' refs stay unreachable.
func TestAssignedWorkIndexReachable_InfraLegOnSplitCity(t *testing.T) {
	cfg, _, _, _ := newNoScaleCheckRigPoolCity(t)
	agentCfg := &cfg.Agents[0]
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	refs := []string{"", "rig-A", "rig-B"}
	if !assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, refs, 0) {
		t.Errorf("infra leg (store-ref \"\") unreachable from rig-bound agent on a split city — claimed infra wisps are invisible post-claim")
	}
	if !assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, refs, 1) {
		t.Errorf("own rig store-ref must stay reachable")
	}
	if assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, refs, 2) {
		t.Errorf("another rig's store-ref must stay unreachable")
	}
}

// TestAssignedWorkIndexReachable_LegacyCityByteIdentical proves single-store
// byte-identity for site 2: without the infra marker the "" (city) leg stays
// unreachable from rig-bound agents exactly as today.
func TestAssignedWorkIndexReachable_LegacyCityByteIdentical(t *testing.T) {
	cfg, _, _, _ := newNoScaleCheckRigPoolCity(t)
	agentCfg := &cfg.Agents[0]
	cityPath := t.TempDir() // no infra marker → legacy city

	refs := []string{"", "rig-A"}
	if assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, refs, 0) {
		t.Errorf("city leg (store-ref \"\") became reachable from a rig-bound agent on a LEGACY city (byte-identity violated)")
	}
	if !assignedWorkIndexReachableFromAgent(cityPath, cfg, agentCfg, refs, 1) {
		t.Errorf("own rig store-ref must stay reachable on a legacy city")
	}
}

// TestFilterAssignedWorkForPoolDemand_KeepsClaimedInfraWispOnSplitCity drives
// site 2 through its production consumer: a claimed (in_progress) infra wisp
// routed to a rig pool must survive the pool-demand filter on a split city so
// the resume tier keeps the owning session desired.
func TestFilterAssignedWorkForPoolDemand_KeepsClaimedInfraWispOnSplitCity(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	wisp := beads.Bead{
		ID:       "gcg-wisp-1",
		Status:   "in_progress",
		Assignee: "s-abc123",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}
	filtered := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, nil, []beads.Bead{wisp}, []string{""})
	if len(filtered) != 1 {
		t.Errorf("filtered = %d beads, want 1 (claimed infra wisp dropped from pool demand on a split city)", len(filtered))
	}
}

// TestFilterAssignedWorkForPoolDemand_DropsCityLegOnLegacyCity proves the
// consumer-level byte-identity of site 2 on a legacy single-store city.
func TestFilterAssignedWorkForPoolDemand_DropsCityLegOnLegacyCity(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir() // no infra marker

	wisp := beads.Bead{
		ID:       "gcg-wisp-1",
		Status:   "in_progress",
		Assignee: "s-abc123",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}
	filtered := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, nil, []beads.Bead{wisp}, []string{""})
	if len(filtered) != 0 {
		t.Errorf("filtered = %d beads, want 0 on a legacy city (byte-identity violated)", len(filtered))
	}
}

// TestReachableStoresForSession_IncludesPrimaryStoreOnSplitCity is the T4
// site-3 regression: a rig-bound session's reachable-store set must include
// the primary (sessions/infra) store on a split city — its claimed graph
// wisps live there — in addition to its rig store.
func TestReachableStoresForSession_IncludesPrimaryStoreOnSplitCity(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	primary := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	sess := beads.Bead{
		ID:   "s-w1",
		Type: "session",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}

	stores, err := reachableStoresForSession(cityPath, cfg, primary, map[string]beads.Store{"rig-A": rigStore}, sess)
	if err != nil {
		t.Fatalf("reachableStoresForSession: %v", err)
	}
	if len(stores) != 2 {
		t.Fatalf("reachable stores = %d, want 2 (rig store + infra store) on a split city", len(stores))
	}
	if stores[0] != rigStore {
		t.Errorf("first reachable store is not the rig store (historical first-match ordering violated)")
	}
	if stores[1] != primary {
		t.Errorf("second reachable store is not the primary (infra) store")
	}
}

// TestReachableStoresForSession_LegacyCityByteIdentical proves single-store
// byte-identity for site 3: without the infra marker a rig-bound session
// probes ONLY its rig store, exactly as today.
func TestReachableStoresForSession_LegacyCityByteIdentical(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir() // no infra marker

	primary := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	sess := beads.Bead{
		ID:   "s-w1",
		Type: "session",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}

	stores, err := reachableStoresForSession(cityPath, cfg, primary, map[string]beads.Store{"rig-A": rigStore}, sess)
	if err != nil {
		t.Fatalf("reachableStoresForSession: %v", err)
	}
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("reachable stores = %d, want exactly the rig store on a legacy city (byte-identity violated)", len(stores))
	}
}

// TestSessionHasOpenAssignedWork_FindsClaimedInfraWispOnSplitCity drives
// site 3 through its production consumer: the drain-probe must find a claimed
// wisp that lives ONLY in the primary (infra) store for a rig-bound session
// on a split city, so the drain does not simply move post-claim.
func TestSessionHasOpenAssignedWork_FindsClaimedInfraWispOnSplitCity(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	primary := beads.NewMemStoreHonoringIDs()
	rigStore := beads.NewMemStore()
	sess := beads.Bead{
		ID:   "s-w1",
		Type: "session",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}
	if _, err := primary.Create(beads.Bead{
		ID:       "gcg-wisp-1",
		Status:   "in_progress",
		Assignee: "s-w1",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, primary, map[string]beads.Store{"rig-A": rigStore}, sess)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Errorf("claimed infra wisp invisible to the rig-bound session's drain probe on a split city")
	}
}

// TestSessionHasOpenAssignedWork_IgnoresPrimaryStoreOnLegacyCity proves the
// consumer-level byte-identity of site 3: on a legacy city the primary-store
// bead stays invisible to a rig-bound session, exactly as today.
func TestSessionHasOpenAssignedWork_IgnoresPrimaryStoreOnLegacyCity(t *testing.T) {
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir() // no infra marker

	primary := beads.NewMemStoreHonoringIDs()
	rigStore := beads.NewMemStore()
	sess := beads.Bead{
		ID:   "s-w1",
		Type: "session",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}
	if _, err := primary.Create(beads.Bead{
		ID:       "gcg-wisp-1",
		Status:   "in_progress",
		Assignee: "s-w1",
		Type:     "task",
		Metadata: map[string]string{"gc.routed_to": qualified},
	}); err != nil {
		t.Fatal(err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, primary, map[string]beads.Store{"rig-A": rigStore}, sess)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if has {
		t.Errorf("primary-store bead became visible to a rig-bound session on a LEGACY city (byte-identity violated)")
	}
}

// TestReleaseOrphanedPoolAssignments_SessionStoreProbeFindsLiveSession is the
// T5 site-4 regression: the orphan-release last-resort liveness check must
// probe the SESSIONS store. On a split city session beads live in the infra
// store; probing the work store finds nothing and wrongfully releases a live
// session's claim.
func TestReleaseOrphanedPoolAssignments_SessionStoreProbeFindsLiveSession(t *testing.T) {
	workStore := beads.NewMemStore()
	sessionStore := beads.NewMemStore()

	sess, err := sessionStore.Create(beads.Bead{
		Title: "worker-1",
		Type:  "session",
		Metadata: map[string]string{
			"session_name": "worker-1",
			"state":        "active",
		},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := workStore.Create(beads.Bead{
		Title:    "claimed pool wisp",
		Assignee: sess.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := workStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work status: %v", err)
	}
	work, err = workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	// The open-session snapshot MISSED the live session (the exact race the
	// last-resort live probe exists for). With the sessions store threaded,
	// the probe finds the live holder and the claim survives.
	released := releaseOrphanedPoolAssignments(
		workStore, cfg, "", nil,
		[]beads.Bead{work}, nil, nil, nil,
		sessionStore,
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (live session in the sessions store must keep its claim)", released)
	}
	got, err := workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("get work bead: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != sess.ID {
		t.Fatalf("work = status %q assignee %q, want in_progress/%s (claim wrongfully released)", got.Status, got.Assignee, sess.ID)
	}
}

// TestReleaseOrphanedPoolAssignments_DefaultSessionStoreIsByteIdentical proves
// site-4 byte-identity: without the sessions-store option the probe stays on
// the work store, so a session bead living elsewhere is NOT found and the
// stale claim is released exactly as today.
func TestReleaseOrphanedPoolAssignments_DefaultSessionStoreIsByteIdentical(t *testing.T) {
	workStore := beads.NewMemStore()
	otherStore := beads.NewMemStore()

	sess, err := otherStore.Create(beads.Bead{
		Title:    "worker-1",
		Type:     "session",
		Metadata: map[string]string{"session_name": "worker-1"},
	})
	if err != nil {
		t.Fatalf("create session bead: %v", err)
	}

	work, err := workStore.Create(beads.Bead{
		Title:    "claimed pool wisp",
		Assignee: sess.ID,
		Metadata: map[string]string{"gc.routed_to": "worker"},
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := workStore.Update(work.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set work status: %v", err)
	}
	work, err = workStore.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}

	cfg := &config.City{Agents: []config.Agent{{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(2)}}}

	released := releaseOrphanedPoolAssignments(
		workStore, cfg, "", nil,
		[]beads.Bead{work}, nil, nil, nil,
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want the stale claim released (legacy single-store behavior)", released)
	}
}
