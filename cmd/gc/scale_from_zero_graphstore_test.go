package main

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newGraphStoreRigPoolCity builds a city whose store is the live graph_store=sqlite
// shape — policy(Router(caching(work) + sqlite-graph)) — with a single rig pool
// agent that has min=0 and NO custom scale_check (the default-probe path that
// gascity/gc.implementation-worker and gascity/gc.run-operator use).
func newGraphStoreRigPoolCity(t *testing.T) (cfg *config.City, cityStore beads.Store, rigStores map[string]beads.Store, qualified, dir string) {
	t.Helper()
	dir = t.TempDir()
	cityStore = wrapWithCachingStore(
		context.TODO(),
		wrapStoreWithBeadPolicies(beads.NewMemStore(), graphSQLiteCfg()),
		nil, false, dir,
	)
	t.Cleanup(func() { _ = closeBeadStoreHandle(cityStore) })

	maxSess := 5
	minSess := 0
	cfg = &config.City{
		Agents: []config.Agent{{
			Name:              "executor",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
			Dir:               "rig-A",
			Provider:          "mock",
		}},
		Rigs:      []config.Rig{{Name: "rig-A", Path: dir + "/rigs/rig-A"}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	cfg.Beads.GraphStore = "sqlite"
	rigStores = map[string]beads.Store{"rig-A": beads.NewMemStore()}
	return cfg, cityStore, rigStores, "rig-A/executor", dir
}

// createCityGraphRoutedWork mints an open, empty-assignee, graph-classified task
// routed to the cold pool. gc.root_bead_id forces ClassGraph so the bead lands in
// the city's sqlite graph leg (not the rig work store) — exactly where formula-v2
// nodes routed to a rig pool live under graph_store=sqlite. NoHistory mirrors the
// live starving bead (gcg-23484) shape.
func createCityGraphRoutedWork(t *testing.T, cityStore beads.Store, qualified, id string) {
	t.Helper()
	if _, err := cityStore.Create(beads.Bead{
		Status:    "open",
		Type:      "task",
		NoHistory: true,
		Metadata: map[string]string{
			"gc.routed_to":    qualified,
			"gc.root_bead_id": "root-" + id,
		},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestBuildDesiredState_ScaleFromZero_GraphStore_CityGraphWorkWakesColdPool guards
// the baseline graph_store=sqlite path: a cold (zero-session) rig pool with no
// custom scale_check must wake from city-resident graph work routed to it, even
// though that work lives in the city's sqlite graph leg rather than the rig store.
func TestBuildDesiredState_ScaleFromZero_GraphStore_CityGraphWorkWakesColdPool(t *testing.T) {
	cfg, cityStore, rigStores, qualified, dir := newGraphStoreRigPoolCity(t)
	createCityGraphRoutedWork(t, cityStore, qualified, "1")

	result := buildDesiredStateWithSessionBeads(
		"test-city", dir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("cold-pool demand = %d, want 1 (city graph work must wake the pool)", got)
	}
	if len(result.State) != 1 {
		t.Errorf("desired sessions = %d, want 1", len(result.State))
	}
}

// TestBuildDesiredState_ScaleFromZero_GraphStore_PhantomSessionStillWakes is the
// regression guard for the scale-from-zero starve under graph_store=sqlite. A rig
// pool with a single lingering session bead in a non-serving state (asleep on
// no-work) made runningSessions>=1, so isCold flipped to false and the cross-store
// city probe — the ONLY probe that sees city-resident graph work routed to a
// rig-scoped pool — was skipped. The own-rig (work-leg) probe cannot see the city
// graph bead, so demand fell to 0 and no fresh worker spawned: the routed work
// starved while a phantom asleep session sat idle. The fix removes the isCold gate
// from the city cross-probe so the authoritative routed-demand count is always
// computed; downstream computePoolDesiredStates offsets it against running
// sessions, so an always-on probe over-counts nothing.
func TestBuildDesiredState_ScaleFromZero_GraphStore_PhantomSessionStillWakes(t *testing.T) {
	cfg, cityStore, rigStores, qualified, dir := newGraphStoreRigPoolCity(t)
	createCityGraphRoutedWork(t, cityStore, qualified, "1")

	// One asleep pool session bead — not claiming the city work, but enough to
	// flip isCold to false under the old gate.
	asleep := beads.Bead{
		Title:  qualified + "-1",
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel, "agent:" + qualified + "-1", "template:" + qualified},
		Metadata: map[string]string{
			"template":             qualified,
			"agent_name":           qualified + "-1",
			"alias":                qualified + "-1",
			"state":                "asleep",
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
		},
	}
	if _, err := rigStores["rig-A"].Create(asleep); err != nil {
		t.Fatal(err)
	}
	snap := newSessionBeadSnapshot([]beads.Bead{asleep})

	result := buildDesiredStateWithSessionBeads(
		"test-city", dir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, snap, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Errorf("phantom-session demand = %d, want 1 (city graph work still needs a worker)", got)
	}
}
