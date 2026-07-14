package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newCityPoolCity wires a CITY-scoped (no Dir) default-probe pool agent named
// implementation-worker — the exact shape the graph-demand federation bug
// cites: a molecule's routed worker step lands on a city-scoped pool, not a
// rig pool. It is the city-scoped sibling of newNoScaleCheckRigPoolCity; it
// takes no rig so the pool's own demand target is the leading (city) store.
func newCityPoolCity() (*config.City, beads.Store, string) {
	maxSess, minSess := 5, 0
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "implementation-worker",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
			Provider:          "mock",
		}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	return cfg, beads.NewMemStore(), cfg.Agents[0].QualifiedName()
}

// TestSplitCityDemand_CityPoolSeesGraphRoutedStep is the load-bearing
// verification for the graph-demand federation fix. It stages the exact bug
// shape from the incident: an OPEN, UNASSIGNED, graph-class (type=task) worker
// step routed to a CITY-scoped pool (implementation-worker), resident in the
// leading city store. A fresh reconciler tick must count it as pool demand so
// a worker spawns to run it — otherwise the step strands when its in-session
// operator is lost.
func TestSplitCityDemand_CityPoolSeesGraphRoutedStep(t *testing.T) {
	cfg, store, qualified := newCityPoolCity()
	mintGraphRoutedStep(t, store, qualified, "")

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, nil, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got < 1 {
		t.Fatalf("pool demand for city-scoped %s = %d, want >= 1 (open unassigned graph-class step routed to the city pool must be discoverable as demand)", qualified, got)
	}
}

// TestSplitCityDemand_CityPoolGraphStepNoDoubleCount pins that counting
// city-pool demand does NOT double-count a routed step: the city pool's own
// demand target IS the leading city store, so the cross-store city arm must
// not add a second demand source over the same store (the ownTarget.storeKey
// != "city" half of the guard). Exactly one routed step must yield exactly
// one unit of demand.
func TestSplitCityDemand_CityPoolGraphStepNoDoubleCount(t *testing.T) {
	cfg, store, qualified := newCityPoolCity()
	mintGraphRoutedStep(t, store, qualified, "")

	result := buildDesiredStateWithSessionBeads(
		"test-city", t.TempDir(), time.Now(), cfg, &localMockProvider{},
		store, nil, &sessionBeadSnapshot{}, nil, os.Stderr,
	)

	if got := result.ScaleCheckCounts[qualified]; got != 1 {
		t.Fatalf("pool demand for city-scoped %s = %d, want exactly 1 (a single routed graph step must not be double-counted)", qualified, got)
	}
}
