package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// This file covers the wake-filter follow-up from the split-city spawn/drain
// treadmill red-team (split_store_treadmill_test.go, sites 2-4 family). On a
// split city the reconciler's leading store IS the sessions/infra store, and
// its assigned-work arm is captured under store-ref "". Two consumers still
// answered "which store can a rig-bound holder's claim live in" without that
// leg:
//
//   - filterAssignedWorkBeadsForSessionWake dropped the "" leg for rig-bound
//     holders, so ComputeAwakeSet could not anchor a CLAIMED infra wisp to its
//     session. The owning session lost its assigned-work wake reason and the
//     reconciler put it through a begin-drain/GC_DRAIN_ACK/cancel loop every
//     tick (pure churn — the site-3 live probe cancels the drain), and the
//     idle-sleep exemption was blind to the claim.
//   - openSessionReachableStoreRefs (via makeOpenSessionStoreRefIndex) had the
//     same gap, so releaseOrphanedPoolAssignments' ownership check missed
//     rig-bound holders of infra-arm work and fell to the per-wisp last-resort
//     live probe every tick — correct but slow, and a single fail-open leg.
//
// Both fixes are the same cityHasInfraStore-gated "" leg the treadmill fix
// gave assignedWorkIndexReachableFromAgent; a legacy single-store city stays
// byte-identical (the marker is absent, the gate stays closed).

// newRigBoundWakeHolders builds the rig-bound holder shapes of
// TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources —
// a rig-bound agent, its named-session identity, and an open pool session
// bead — against a fresh cityPath so each test decides whether to seed the
// split-city infra marker.
func newRigBoundWakeHolders(t *testing.T) (cityPath string, cfg *config.City, sessions []beads.Bead) {
	t.Helper()
	cityPath = t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg = &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions = []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	return cityPath, cfg, sessions
}

// TestFilterAssignedWorkForSessionWake_KeepsClaimedInfraWispOnSplitCity is the
// T1 wake-filter regression: claimed infra-arm work (store-ref "") held by
// rig-bound assignees — a named identity and a concrete session — must survive
// the session-wake filter on a split city, refs aligned, so ComputeAwakeSet
// can anchor the claim to its session instead of draining the live holder.
func TestFilterAssignedWorkForSessionWake_KeepsClaimedInfraWispOnSplitCity(t *testing.T) {
	cityPath, cfg, sessions := newRigBoundWakeHolders(t)
	seedSplitCityInfraMarker(t, cityPath)

	work := []beads.Bead{
		{ID: "gcg-wisp-named", Status: "in_progress", Assignee: "riga/worker"},
		{ID: "gcg-wisp-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "", "riga"}

	got, gotRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessionInfosFromBeads(sessions), work, storeRefs)

	if len(got) != 3 {
		ids := make([]string, len(got))
		for i, wb := range got {
			ids[i] = wb.ID
		}
		t.Fatalf("filtered work IDs = %v, want all 3 kept (claimed infra-arm work dropped from the wake filter on a split city)", ids)
	}
	if len(gotRefs) != 3 || gotRefs[0] != "" || gotRefs[1] != "" || gotRefs[2] != "riga" {
		t.Fatalf("filtered store refs = %#v, want [\"\" \"\" riga] aligned with beads", gotRefs)
	}
}

// TestFilterAssignedWorkForSessionWake_DropsCityLegOnLegacyCity proves the
// wake filter's single-store byte-identity: without the infra marker the ""
// (city) legs stay dropped for rig-bound holders exactly as today.
func TestFilterAssignedWorkForSessionWake_DropsCityLegOnLegacyCity(t *testing.T) {
	cityPath, cfg, sessions := newRigBoundWakeHolders(t) // no infra marker → legacy city

	work := []beads.Bead{
		{ID: "gcg-wisp-named", Status: "in_progress", Assignee: "riga/worker"},
		{ID: "gcg-wisp-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "", "riga"}

	got, gotRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessionInfosFromBeads(sessions), work, storeRefs)

	if len(got) != 1 || got[0].ID != "rig-session" {
		t.Fatalf("filtered work = %#v, want only rig-session on a legacy city (byte-identity violated)", got)
	}
	if len(gotRefs) != 1 || gotRefs[0] != "riga" {
		t.Fatalf("filtered store refs = %#v, want [riga]", gotRefs)
	}
}

// TestOpenSessionStoreRefIndex_InfraLegOnSplitCity is the T2 ownership-index
// regression: on a split city a rig-bound open session must own infra-arm
// (store-ref "") work through makeOpenSessionStoreRefIndex/openSessionOwnsWork
// alone — no fall-through to the per-wisp live probe — while other rigs' refs
// stay unowned.
func TestOpenSessionStoreRefIndex_InfraLegOnSplitCity(t *testing.T) {
	cityPath, cfg, sessions := newRigBoundWakeHolders(t)
	seedSplitCityInfraMarker(t, cityPath)

	index := makeOpenSessionStoreRefIndex(cityPath, cfg, sessions, true)
	if !openSessionOwnsWork(nil, index, "session-1", "", true) {
		t.Errorf("rig-bound holder does not own its infra-arm (store-ref \"\") claim on a split city — orphan release falls to the per-wisp live probe")
	}
	if !openSessionOwnsWork(nil, index, "session-1", "riga", true) {
		t.Errorf("own rig store-ref must stay owned")
	}
	if openSessionOwnsWork(nil, index, "session-1", "rigb", true) {
		t.Errorf("another rig's store-ref must stay unowned")
	}
}

// TestOpenSessionStoreRefIndex_LegacyCityByteIdentical proves the ownership
// index's single-store byte-identity: without the infra marker the "" (city)
// leg stays unowned by rig-bound holders exactly as today.
func TestOpenSessionStoreRefIndex_LegacyCityByteIdentical(t *testing.T) {
	cityPath, cfg, sessions := newRigBoundWakeHolders(t) // no infra marker → legacy city

	index := makeOpenSessionStoreRefIndex(cityPath, cfg, sessions, true)
	if openSessionOwnsWork(nil, index, "session-1", "", true) {
		t.Errorf("city leg (store-ref \"\") became owned by a rig-bound holder on a LEGACY city (byte-identity violated)")
	}
	if !openSessionOwnsWork(nil, index, "session-1", "riga", true) {
		t.Errorf("own rig store-ref must stay owned on a legacy city")
	}
}

// TestReleaseOrphanedPoolAssignments_OwnsClaimedInfraWispWithoutLiveProbe
// drives T2 through its production consumer: on a split city the ownership
// index alone must keep a rig-bound holder's claimed infra wisp, even when the
// last-resort sessions-store live probe cannot see the holder (the probe is a
// fail-open per-wisp query; ownership must not depend on it every tick).
func TestReleaseOrphanedPoolAssignments_OwnsClaimedInfraWispWithoutLiveProbe(t *testing.T) {
	cfg, workStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	infraStore := beads.NewMemStoreHonoringIDs()
	wisp, err := infraStore.Create(beads.Bead{
		ID:       "gcg-wisp-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "s-w1",
		Metadata: map[string]string{"gc.routed_to": qualified},
	})
	if err != nil {
		t.Fatalf("create claimed infra wisp: %v", err)
	}
	if err := infraStore.Update(wisp.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set wisp status: %v", err)
	}
	wisp, err = infraStore.Get(wisp.ID)
	if err != nil {
		t.Fatalf("reload wisp: %v", err)
	}

	sess := beads.Bead{
		ID:     "s-w1",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}

	// The sessions-store option is an EMPTY store: the live probe cannot save
	// the claim, so surviving proves the store-ref index owns the infra leg.
	released := releaseOrphanedPoolAssignments(
		workStore, cfg, cityPath,
		[]beads.Bead{sess},
		[]beads.Bead{wisp},
		[]beads.Store{infraStore},
		[]string{""},
		rigStores,
		beads.NewMemStore(),
	)
	if len(released) != 0 {
		t.Fatalf("released = %v, want none (rig-bound holder's claimed infra wisp released without consulting the ownership index)", released)
	}
	got, err := infraStore.Get(wisp.ID)
	if err != nil {
		t.Fatalf("get wisp: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee != "s-w1" {
		t.Fatalf("wisp = status %q assignee %q, want in_progress/s-w1 (claim wrongfully released)", got.Status, got.Assignee)
	}
}

// TestReleaseOrphanedPoolAssignments_ReleasesCityLegOnLegacyCity proves the
// consumer-level byte-identity of the ownership index: on a legacy city the
// "" (city) leg stays outside a rig-bound holder's ownership, so the stale
// claim is released exactly as today.
func TestReleaseOrphanedPoolAssignments_ReleasesCityLegOnLegacyCity(t *testing.T) {
	cfg, workStore, rigStores, qualified := newNoScaleCheckRigPoolCity(t)
	cityPath := t.TempDir() // no infra marker → legacy city

	ownerStore := beads.NewMemStoreHonoringIDs()
	wisp, err := ownerStore.Create(beads.Bead{
		ID:       "gcg-wisp-1",
		Type:     "task",
		Status:   "in_progress",
		Assignee: "s-w1",
		Metadata: map[string]string{"gc.routed_to": qualified},
	})
	if err != nil {
		t.Fatalf("create claimed wisp: %v", err)
	}
	if err := ownerStore.Update(wisp.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
		t.Fatalf("set wisp status: %v", err)
	}
	wisp, err = ownerStore.Get(wisp.ID)
	if err != nil {
		t.Fatalf("reload wisp: %v", err)
	}

	sess := beads.Bead{
		ID:     "s-w1",
		Type:   "session",
		Status: "open",
		Metadata: map[string]string{
			"template":     qualified,
			"session_name": "executor-1",
		},
	}

	released := releaseOrphanedPoolAssignments(
		workStore, cfg, cityPath,
		[]beads.Bead{sess},
		[]beads.Bead{wisp},
		[]beads.Store{ownerStore},
		[]string{""},
		rigStores,
		beads.NewMemStore(),
	)
	if len(released) != 1 {
		t.Fatalf("released = %v, want the city-leg claim released on a legacy city (byte-identity violated)", released)
	}
}

// TestReconcileSessionBeads_ClaimedInfraWispKeepsSessionOutOfDrainLoop is the
// T4 churn regression, driven through the production reconciler: an ALIVE
// rig-bound pool session holding a claimed infra wisp (store-ref "", surviving
// the production wake filter) must keep its assigned-work wake reason on a
// split city — it must NOT enter the begin-drain/GC_DRAIN_ACK/cancel loop the
// dropped leg produced every tick.
func TestReconcileSessionBeads_ClaimedInfraWispKeepsSessionOutOfDrainLoop(t *testing.T) {
	env := newReconcilerTestEnv()
	cfg, _, _, qualified := newNoScaleCheckRigPoolCity(t)
	env.cfg = cfg
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)

	env.addDesired("executor-1", qualified, true)
	session := env.createSessionBead("executor-1", qualified)
	env.markSessionActive(&session)

	wisp := beads.Bead{
		ID:       "gcg-wisp-1",
		Status:   "in_progress",
		Assignee: session.ID,
		Metadata: map[string]string{"gc.routed_to": qualified},
	}
	// Production wiring (city_runtime.go bead_reconcile): the wake filter runs
	// first, its survivors feed the reconciler's awake scan.
	awakeBeads, _ := filterAssignedWorkBeadsForSessionWake(
		cfg, cityPath, sessionInfosFromBeads([]beads.Bead{session}), []beads.Bead{wisp}, []string{""},
	)

	cfgNames := configuredSessionNames(env.cfg, "", env.store)
	reconcileSessionBeadsAtPath(
		context.Background(), cityPath, []beads.Bead{session}, env.desiredState, cfgNames, env.cfg, env.sp,
		env.store, nil, awakeBeads, nil, nil, env.dt, map[string]int{}, false, nil, "",
		nil, env.clk, env.rec, 0, 0, &env.stdout, &env.stderr,
	)

	if ds := env.dt.get(session.ID); ds != nil {
		t.Fatalf("claimed session entered begin-drain (reason %q) — its claimed infra wisp was invisible to the wake scan (drain/cancel churn loop)", ds.reason)
	}
}
