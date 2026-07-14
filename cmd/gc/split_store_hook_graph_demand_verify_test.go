package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestSplitCityHookDiscoversGraphRoutedStep is the hook-side load-bearing
// verification for the graph-demand federation fix. A fresh worker's hook work
// query must discover a graph-class routed step that lives in the city store,
// or the step strands when its molecule loses its in-session operator.
//
// The mechanism upstream is store-list federation (cmd_hook.go): a rig-scoped
// agent's hook chain is [own rig store (primary), own env, city store
// (appendCityHookStore, appended LAST)], and firstStoreWithWork surfaces the
// first store that reports ready work. This test pins that end-to-end through
// the real firstStoreWithWork plumbing: the runner returns the graph-routed
// step ONLY for the city entry (modeling a step resident in the city store)
// and empty for the rig store.
func TestSplitCityHookDiscoversGraphRoutedStep(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()
	cfg := &config.City{Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}}}
	agent := &config.Agent{Name: "implementation-worker", Dir: "rig-A"}

	// The wiring half: a rig-scoped identity resolves to its rig, and the city
	// store is appended to its chain — without that entry no store in a
	// rig-bound agent's chain reaches the city-resident step.
	if rig := rigScopedHookRig(cfg, "rig-A/implementation-worker"); rig != "rig-A" {
		t.Fatalf("rigScopedHookRig = %q, want %q", rig, "rig-A")
	}
	rigEntry := hookStore{dir: rigPath}
	stores := appendCityHookStore([]hookStore{rigEntry}, cityPath, cfg, agent, nil)
	if len(stores) != 2 {
		t.Fatalf("appendCityHookStore produced %d entries, want 2 (rig primary + city)", len(stores))
	}
	if stores[0].dir != rigPath {
		t.Fatalf("primary hook entry dir = %q, want the rig store %q (city is best-effort, appended last)", stores[0].dir, rigPath)
	}
	if stores[1].dir != cityPath {
		t.Fatalf("city hook entry dir = %q, want %q", stores[1].dir, cityPath)
	}

	// The discovery half: only the city entry can see the graph-class step.
	const graphStep = `[{"id":"gcg-wisp-0042","issue_type":"task","status":"open"}]`
	run := func(_, dir string, _ []string) (string, error) {
		if dir == cityPath {
			return graphStep, nil
		}
		return `[]`, nil
	}

	out, gotStore, err := firstStoreWithWork("bd ready --json", stores, rigEntry, run)
	if err != nil {
		t.Fatalf("firstStoreWithWork: %v", err)
	}
	if out != graphStep {
		t.Fatalf("hook did not surface the graph-routed step: out=%q, want %q", out, graphStep)
	}
	if gotStore.dir != cityPath {
		t.Fatalf("hook selected store %q, want the city store %q", gotStore.dir, cityPath)
	}
}

// TestSplitCityHookRigStoreStaysPrimary is the byte-identity guard for the
// federation: the rig store stays the PRIMARY entry, so when the agent's OWN
// store has ready work the hook takes it from there — the appended city entry
// is best-effort discovery for city-resident steps, not a takeover of the
// rig-scoped hook's historical own-store-first behavior (and the primary
// keeps firstStoreWithWork's emit-on-timeout contract).
func TestSplitCityHookRigStoreStaysPrimary(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := t.TempDir()

	rigEntry := hookStore{dir: rigPath}
	stores := []hookStore{rigEntry, {dir: cityPath}}

	const rigWork = `[{"id":"ra-101","issue_type":"task","status":"open"}]`
	const cityWork = `[{"id":"gcg-wisp-0042","issue_type":"task","status":"open"}]`
	run := func(_, dir string, _ []string) (string, error) {
		if dir == rigPath {
			return rigWork, nil
		}
		return cityWork, nil
	}

	out, gotStore, err := firstStoreWithWork("bd ready --json", stores, rigEntry, run)
	if err != nil {
		t.Fatalf("firstStoreWithWork: %v", err)
	}
	if out != rigWork {
		t.Fatalf("hook output = %q, want the rig store's own work %q", out, rigWork)
	}
	if gotStore.dir != rigPath {
		t.Fatalf("hook selected store %q, want the primary rig store %q", gotStore.dir, rigPath)
	}
}
