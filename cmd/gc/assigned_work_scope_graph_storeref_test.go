package main

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// remapGraphMemStore is a MemStore posing as a graph_store=sqlite city Router:
// its federated List returns the seeded gcg- beads (they physically live in the
// city ClassGraph leg) and ListGraphOnlyHandle advertises the capability with the
// gcg id prefix. hasGraph=false models a default Dolt city (capability absent).
type remapGraphMemStore struct {
	*beads.MemStore
	hasGraph bool
}

func (s *remapGraphMemStore) ListGraphOnlyHandle() (beads.GraphOnlyListStore, bool) {
	if !s.hasGraph {
		return nil, false
	}
	return remapGraphLister{s.MemStore}, true
}

type remapGraphLister struct{ *beads.MemStore }

func (l remapGraphLister) ListGraphOnly(q beads.ListQuery) ([]beads.Bead, error) { return l.List(q) }
func (l remapGraphLister) GraphIDPrefix() string                                 { return "gcg" }

func remapTestIndexOf(work []beads.Bead, id string) int {
	for i := range work {
		if work[i].ID == id {
			return i
		}
	}
	return -1
}

func remapTestIDs(work []beads.Bead) []string {
	ids := make([]string, len(work))
	for i := range work {
		ids[i] = work[i].ID
	}
	return ids
}

// newGraphResidentRemapFixture builds a two-scope city (one rig "gascity" with a
// rig-scoped "worker" agent) whose city store holds one in-progress graph step
// gcg-1590 routed to that rig worker — the state that deadlocks today: the city
// collection pass tags it storeRef "" so no rig-scoped gate reaches it.
func newGraphResidentRemapFixture(t *testing.T, hasGraph bool) (*config.City, string, beads.Store, map[string]beads.Store) {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity")
	cfg := &config.City{
		Rigs:   []config.Rig{{Name: "gascity", Path: rigPath}},
		Agents: []config.Agent{{Name: "worker", Dir: "gascity"}},
	}
	gcg := beads.Bead{
		ID: "gcg-1590", Type: "task", Status: "in_progress", Assignee: "session-1",
		Metadata: map[string]string{
			"gc.routed_to":      "gascity/worker",
			"gc.root_store_ref": "rig:gascity",
		},
	}
	cityStore := &remapGraphMemStore{MemStore: beads.NewMemStoreFrom(1, []beads.Bead{gcg}, nil), hasGraph: hasGraph}
	rigStores := map[string]beads.Store{"gascity": beads.NewMemStore()}
	return cfg, cityPath, cityStore, rigStores
}

// TestGraphResidentAssignedWorkStoreRefRemap: the raw collection tags a
// city-resident graph step storeRef "" even though its routing binds it to a rig
// worker; the remap retags it to the rig so every rig-scoped gate can reach it.
func TestGraphResidentAssignedWorkStoreRefRemap(t *testing.T) {
	cfg, cityPath, cityStore, rigStores := newGraphResidentRemapFixture(t, true)
	work, _, storeRefs, _, partial := collectAssignedWorkBeadsWithStores(cfg, cityStore, rigStores, nil, nil)
	if partial {
		t.Fatalf("unexpected partial collection")
	}
	idx := remapTestIndexOf(work, "gcg-1590")
	if idx < 0 {
		t.Fatalf("gcg-1590 not collected; got %v", remapTestIDs(work))
	}
	if storeRefs[idx] != "" {
		t.Fatalf("precondition: raw collection should tag the city graph bead storeRef \"\", got %q", storeRefs[idx])
	}
	remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
	if storeRefs[idx] != "gascity" {
		t.Fatalf("remap should retag the graph-resident bead to its routed rig %q, got %q", "gascity", storeRefs[idx])
	}
}

// TestGraphResidentAssignedWorkWakesAsleepRigWorker drives the real wake gate
// end-to-end: after the remap, the asleep rig worker's session must be woken for
// its in-progress graph work (the a7f7b2bcd assigned-work sibling deadlock).
func TestGraphResidentAssignedWorkWakesAsleepRigWorker(t *testing.T) {
	cfg, cityPath, cityStore, rigStores := newGraphResidentRemapFixture(t, true)
	work, _, storeRefs, _, _ := collectAssignedWorkBeadsWithStores(cfg, cityStore, rigStores, nil, nil)

	sessions := []beads.Bead{{
		ID: "session-1", Status: "open", Type: sessionBeadType,
		Metadata: map[string]string{"template": "gascity/worker", "session_name": "worker-1"},
	}}
	// Red half: without the remap the city-tagged ("") graph bead is unreachable
	// from the rig-scoped session and the wake filter drops it — the deadlock.
	if pre := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessions, work, storeRefs); remapTestIndexOf(pre, "gcg-1590") >= 0 {
		t.Fatalf("precondition: pre-remap wake filter should EXCLUDE the city-tagged graph bead, got %v", remapTestIDs(pre))
	}

	remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessions, work, storeRefs)
	if remapTestIndexOf(got, "gcg-1590") < 0 {
		t.Fatalf("asleep rig worker must wake for its in-progress graph work; wake filter returned %v", remapTestIDs(got))
	}
}

// TestGraphResidentAssignedWorkDrivesRigPoolDemand: the same remap makes the
// graph step visible to the rig pool-demand gate.
func TestGraphResidentAssignedWorkDrivesRigPoolDemand(t *testing.T) {
	cfg, cityPath, cityStore, rigStores := newGraphResidentRemapFixture(t, true)
	work, _, storeRefs, _, _ := collectAssignedWorkBeadsWithStores(cfg, cityStore, rigStores, nil, nil)
	remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)

	sessions := []beads.Bead{{
		ID: "session-1", Status: "open", Type: sessionBeadType,
		Metadata: map[string]string{"template": "gascity/worker", "session_name": "worker-1"},
	}}
	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, work, storeRefs)
	if remapTestIndexOf(got, "gcg-1590") < 0 {
		t.Fatalf("graph-resident routed work must drive rig pool demand; pool filter returned %v", remapTestIDs(got))
	}
}

// TestGraphResidentRemapWiredIntoBuildDesiredState is the production-composition
// test: it drives buildDesiredStateWithSessionBeads end-to-end (not the helper in
// isolation), so deleting the single remap call site regresses it. It pins two
// things the unit tests can't: (i) the remapped ref lands in
// DesiredStateResult.AssignedWorkStoreRefs, and (ii) the release path protects the
// live rig owner's graph work via scoped ownership (openSessionOwnsWork) rather
// than the label-only fail-safe — the session bead deliberately carries no
// gc:session label, so without the remap the bead's ref stays "" and it would be
// reopened out from under the live worker.
func TestGraphResidentRemapWiredIntoBuildDesiredState(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}},
		Agents:    []config.Agent{{Name: "worker", Dir: "gascity", StartCommand: "true"}},
	}
	gcg := beads.Bead{
		ID: "gcg-1590", Type: "task", Status: "in_progress", Assignee: "worker-1",
		Metadata: map[string]string{
			"gc.routed_to":      "gascity/worker",
			"gc.root_store_ref": "rig:gascity",
		},
	}
	store := &remapGraphMemStore{MemStore: beads.NewMemStoreFrom(1, []beads.Bead{gcg}, nil), hasGraph: true}

	var stderr strings.Builder
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		store, nil, newSessionBeadSnapshot(nil), nil, &stderr,
	)
	if dsResult.snapshotQueryPartial() {
		t.Fatalf("unexpected partial snapshot; stderr:\n%s", stderr.String())
	}

	idx := remapTestIndexOf(dsResult.AssignedWorkBeads, "gcg-1590")
	if idx < 0 {
		t.Fatalf("gcg-1590 not in DesiredStateResult.AssignedWorkBeads: %v", remapTestIDs(dsResult.AssignedWorkBeads))
	}
	if dsResult.AssignedWorkStoreRefs[idx] != "gascity" {
		t.Fatalf("production remap not wired: AssignedWorkStoreRefs[%d] = %q, want \"gascity\" (deleting the :669 remap call regresses this)", idx, dsResult.AssignedWorkStoreRefs[idx])
	}

	// Release path: the live rig session owns the graph work through the remapped
	// ref, so it must NOT be reopened. No gc:session label -> the label-only
	// fail-safe cannot protect it; only the scoped ownership can.
	session := beads.Bead{
		ID: "session-live", Type: sessionBeadType, Status: "open",
		Metadata: map[string]string{"template": "gascity/worker", "session_name": "worker-1"},
	}
	released := releaseOrphanedPoolAssignmentsWhenSnapshotsComplete(store, cfg, cityPath, []beads.Bead{session}, dsResult, nil)
	for _, r := range released {
		if r.ID == "gcg-1590" {
			t.Fatalf("live rig worker's in-progress graph work was reopened out from under it: %+v", released)
		}
	}
}

// TestGraphResidentStoreRefRemapInertness pins that the remap is a no-op wherever
// it must be: default Dolt cities, non-graph beads, unroutable graph beads, and —
// the one real regression trap — a graph bead routed to a city-dir agent (the
// per-bead owner outranks the root store-ref, so it stays city scope "").
func TestGraphResidentStoreRefRemapInertness(t *testing.T) {
	t.Run("default dolt city (capability absent)", func(t *testing.T) {
		cfg, cityPath, cityStore, rigStores := newGraphResidentRemapFixture(t, false)
		work, _, storeRefs, _, _ := collectAssignedWorkBeadsWithStores(cfg, cityStore, rigStores, nil, nil)
		idx := remapTestIndexOf(work, "gcg-1590")
		if idx < 0 {
			t.Fatalf("gcg-1590 not collected")
		}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[idx] != "" {
			t.Fatalf("default Dolt city must not remap (byte-identical), got %q", storeRefs[idx])
		}
	})

	t.Run("non-graph bead", func(t *testing.T) {
		cityPath := t.TempDir()
		cfg := &config.City{
			Rigs:   []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}},
			Agents: []config.Agent{{Name: "worker", Dir: "gascity"}},
		}
		work := []beads.Bead{{ID: "gc-7", Metadata: map[string]string{"gc.root_store_ref": "rig:gascity", "gc.routed_to": "gascity/worker"}}}
		storeRefs := []string{""}
		cityStore := &remapGraphMemStore{MemStore: beads.NewMemStore(), hasGraph: true}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[0] != "" {
			t.Fatalf("non-graph bead (no gcg- prefix) must not remap, got %q", storeRefs[0])
		}
	})

	t.Run("graph bead routed to city-dir agent stays city scope", func(t *testing.T) {
		cityPath := t.TempDir()
		cfg := &config.City{
			Rigs:   []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}},
			Agents: []config.Agent{{Name: "ops"}}, // city-dir agent (Dir == "")
		}
		// Routed to the city-dir agent, but root ref names a rig: the per-bead
		// owner (city scope) must win — a naive "root ref wins" remap would wrongly
		// retag it to the rig.
		work := []beads.Bead{{ID: "gcg-9", Metadata: map[string]string{"gc.routed_to": "ops", "gc.root_store_ref": "rig:gascity"}}}
		storeRefs := []string{""}
		cityStore := &remapGraphMemStore{MemStore: beads.NewMemStore(), hasGraph: true}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[0] != "" {
			t.Fatalf("graph bead routed to a city-dir agent must stay city scope, got %q", storeRefs[0])
		}
	})

	t.Run("graph bead with unknown rig root ref stays city scope", func(t *testing.T) {
		cityPath := t.TempDir()
		cfg := &config.City{Rigs: []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}}}
		work := []beads.Bead{{ID: "gcg-11", Metadata: map[string]string{"gc.root_store_ref": "rig:nonexistent"}}}
		storeRefs := []string{""}
		cityStore := &remapGraphMemStore{MemStore: beads.NewMemStore(), hasGraph: true}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[0] != "" {
			t.Fatalf("graph bead whose root ref names an unconfigured rig must stay city scope, got %q", storeRefs[0])
		}
	})

	t.Run("direct session bind keeps city scope despite rig root ref", func(t *testing.T) {
		cityPath := t.TempDir()
		// A step direct-bound to a session (gc.session_id set, gc.routed_to deleted)
		// whose owner is a city-dir agent: the workflow root's rig must NOT govern,
		// or the bead flips from reachable ("") to unreachable ("gascity").
		cfg := &config.City{
			Rigs:   []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}},
			Agents: []config.Agent{{Name: "ops"}}, // city-dir, gate ref ""
		}
		work := []beads.Bead{{ID: "gcg-13", Assignee: "ops-session", Metadata: map[string]string{
			"gc.session_id":     "ops-session",
			"gc.root_store_ref": "rig:gascity",
		}}}
		storeRefs := []string{""}
		cityStore := &remapGraphMemStore{MemStore: beads.NewMemStore(), hasGraph: true}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[0] != "" {
			t.Fatalf("direct-session-bound graph step must keep city scope (owner scope governs, not workflow root rig), got %q", storeRefs[0])
		}
	})

	t.Run("unresolvable route keeps city scope (no root-provenance guess)", func(t *testing.T) {
		cityPath := t.TempDir()
		cfg := &config.City{
			Rigs:   []config.Rig{{Name: "gascity", Path: filepath.Join(cityPath, "gascity")}},
			Agents: []config.Agent{{Name: "worker", Dir: "gascity"}},
		}
		// Route present but names an agent absent from cfg.Agents (config drift):
		// the per-bead owner is unknown, so we must not guess from the root ref.
		work := []beads.Bead{{ID: "gcg-15", Metadata: map[string]string{
			"gc.routed_to":      "gascity/ghost",
			"gc.root_store_ref": "rig:gascity",
		}}}
		storeRefs := []string{""}
		cityStore := &remapGraphMemStore{MemStore: beads.NewMemStore(), hasGraph: true}
		remapGraphResidentAssignedWorkStoreRefs(cfg, cityPath, cityStore, work, storeRefs)
		if storeRefs[0] != "" {
			t.Fatalf("graph bead with an unresolvable route must keep city scope, got %q", storeRefs[0])
		}
	})
}
