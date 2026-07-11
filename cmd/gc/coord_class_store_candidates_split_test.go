package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
)

// This file proves the E2.3 "read-side fan-out collapse (deferred)" note for
// coordClassStoreCandidates is safe WITHOUT a code change: the reconciler's
// session read already reaches the infra store on a split city because the
// LEADING store the reconciler passes into buildDesiredStateWithSessionBeads is
// the session-class store (sessionsBeadStore().Store / cliSessionStore), which
// resolveSessionStore routes to the infra store on a split city. That leading
// store is the "city" candidate (index 0) of coordClassStoreCandidates, so the
// collectAllOpenSessionBeads session arm scans the infra store, not the work
// store. The rig arms hold no session beads on a split city (session/wait beads
// classify as ClassSessions and live only in the infra store after E3
// migration), so they contribute nothing — no session bead is missed.

// seedInfraSessionBead creates a session-class bead in the given store and
// asserts it classifies as infrastructure, keeping the fixture honest against
// coordclass.Classify (the sole class authority).
func seedInfraSessionBead(t *testing.T, store beads.Store, title, sessionID string) beads.Bead {
	t.Helper()
	created, err := store.Create(beads.Bead{
		Title:    title,
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession, "agent:worker"},
		Metadata: map[string]string{"session_id": sessionID},
	})
	if err != nil {
		t.Fatalf("seed session bead %q: %v", title, err)
	}
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("seed session bead %q did not classify as infrastructure (type=%q labels=%v)",
			title, created.Type, created.Labels)
	}
	return created
}

// TestCollectAllOpenSessionBeadsReadsInfraStoreOnSplitCity proves the
// reconciler's session arm sees an infra-resident session bead on a split city.
// The reconciler passes the SESSION store as the leading (index-0 "city")
// candidate; here that leading store is the infra store, exactly as
// sessionsBeadStore().Store / cliSessionStore resolve on a split city. The rig
// candidate is a WORK store holding no session beads. The infra-resident session
// bead MUST be found — if the reconciler had instead fanned the work store as
// the leading arm, this bead would be invisible and the session unreconciled.
func TestCollectAllOpenSessionBeadsReadsInfraStoreOnSplitCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: "rigs/alpha"}},
	}

	// Two-store harness in the production wrapper shape: the infra store honors
	// explicit ids (reserved-prefix minting) like the real infra store, and the
	// work store is the ordinary policy-wrapped store.
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	rigWork := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)

	// The session bead lives ONLY in the infra store — the split-city reality
	// after E3 migration routes all session-class beads to the infra store.
	sessionBead := seedInfraSessionBead(t, infra, "worker-1", "sess-1")

	// The rig work store holds only a work bead (no session bead), proving the
	// rig arm contributes nothing to the session read on a split city.
	if _, err := rigWork.Create(beads.Bead{Title: "real backlog item", Type: "task"}); err != nil {
		t.Fatalf("seed rig work bead: %v", err)
	}

	// The reconciler's session arm: leading candidate = the SESSION store (infra
	// on a split city), rig candidates = the WORK stores. This mirrors the daemon
	// (CityRuntime.buildDesiredState passes sessionsBeadStore().Store as the
	// leading store; collectAllOpenSessionBeads takes it as cityStore) and the
	// standalone start path (cmd_start.go passes cliSessionStore(...)).
	rigStores := map[string]beads.Store{"alpha": rigWork}
	got, err := collectAllOpenSessionBeads(cfg, infra, rigStores, nil)
	if err != nil {
		t.Fatalf("collectAllOpenSessionBeads returned err = %v", err)
	}

	if !containsBeadID(got, sessionBead.ID) {
		t.Fatalf("collectAllOpenSessionBeads did not find the infra-resident session bead %q; got %d bead(s): %v",
			sessionBead.ID, len(got), beadIDs(got))
	}

	// Discrimination control: had the reconciler fanned only the WORK stores as
	// the E2.3 note feared (leading store = work store, no infra store anywhere),
	// the infra-resident session bead would be invisible. Prove that failure mode
	// concretely, so "already covered" rests on the infra store being the leading
	// candidate, not on the bead happening to be scanned some other way.
	missed, err := collectAllOpenSessionBeads(cfg, rigWork, rigStores, nil)
	if err != nil {
		t.Fatalf("collectAllOpenSessionBeads (work-leading control) returned err = %v", err)
	}
	if containsBeadID(missed, sessionBead.ID) {
		t.Fatalf("work-leading control unexpectedly found the infra-resident session bead %q; "+
			"the discrimination control is not exercising the miss path", sessionBead.ID)
	}
}

// TestCoordClassStoreCandidatesLeadingCandidateIsInfraStoreOnSplitCity pins the
// mechanical invariant the coverage argument rests on: the index-0 candidate of
// coordClassStoreCandidates is exactly the store the caller passes as cityStore.
// The reconciler passes the session store there on a split city, so the session
// arm's leading candidate IS the infra store — never the work store.
func TestCoordClassStoreCandidatesLeadingCandidateIsInfraStoreOnSplitCity(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "alpha", Path: "rigs/alpha"}},
	}
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	rigWork := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	rigStores := map[string]beads.Store{"alpha": rigWork}

	candidates := coordClassStoreCandidates(cfg, infra, rigStores, nil, "city")
	if len(candidates) != 2 {
		t.Fatalf("coordClassStoreCandidates len = %d, want 2 (city + 1 rig)", len(candidates))
	}
	if !sameStorePtr(candidates[0].store, infra) {
		t.Fatalf("leading candidate = %p, want the infra store %p (the reconciler passes the session store here on a split city)",
			candidates[0].store, infra)
	}
	if candidates[0].ref != "city" {
		t.Fatalf("leading candidate ref = %q, want %q", candidates[0].ref, "city")
	}
	if !sameStorePtr(candidates[1].store, rigWork) {
		t.Fatalf("rig candidate = %p, want the rig work store %p", candidates[1].store, rigWork)
	}
}

func containsBeadID(list []beads.Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}

func beadIDs(list []beads.Bead) []string {
	ids := make([]string, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	return ids
}
