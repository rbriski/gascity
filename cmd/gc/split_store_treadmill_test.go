package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// This file covers the spawn/drain treadmill (mc-wisp-orphan RCA): routed
// orchestration work delivered at city scope (vp-kvp cross-store delivery)
// lives in the LEADING city store, but a rig pool's default demand probe
// scanned only its own rig store while the pool was warm — the city-store
// probe was appended only under isCold (runningSessions == 0).
//
// Driver: the cross-store demand probe was gated on isCold, so the first WARM
// tick after a cold spawn computed demand=0, dropped every just-spawned
// session from desiredState, and drained them ~45s post-spawn — before the
// ~2min agent boot could claim. pool_desired cycled 5,0,0 for hours on the
// live trace, and a pool kept warm by even one resume-tier session never saw
// fresh routed steps at all. The probe must be unconditional.

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
// city store. Before the fix the cross-store probe was appended only under
// isCold, so this demand read 0 on every warm tick.
func TestBuildDesiredState_WarmTick_RoutedDemandInLeadingStoreCounts(t *testing.T) {
	cfg, store, rigStores, qualified := newNoScaleCheckRigPoolCity(t)

	// One live pool session → runningSessions > 0 → NOT cold.
	sess, err := store.Create(newWarmPoolSessionBead(qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create warm session bead: %v", err)
	}

	// Routed demand ONLY in the leading (city) store.
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
// (warm) tick — with the work still open and unclaimed — the desired state
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

	// Warm tick: sessions exist and are live, the routed beads are STILL
	// open/unclaimed (the agents have not booted far enough to claim). The
	// sessions must not fall out of desiredState.
	snap, err := loadSessionBeadSnapshot(store)
	if err != nil {
		t.Fatalf("load session snapshot: %v", err)
	}
	warm := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now(), cfg, &localMockProvider{},
		store, rigStores, snap, nil, os.Stderr,
	)
	if got := warm.ScaleCheckCounts[qualified]; got != 2 {
		t.Errorf("warm tick demand = %d, want 2 (treadmill: demand collapsed while work was still unclaimed)", got)
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
