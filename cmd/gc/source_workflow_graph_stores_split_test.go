package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// This file covers the E2.3 deferred read-side gap at openSourceWorkflowStores:
// the workflow-ROOT scan (ListLiveRoots in collectSourceWorkflowMatches / the
// workflow-finalize lister) reads graph-class beads, which a split city routes
// to the infra store. The by-id source-bead read (findUniqueBeadAcrossStoresView)
// reads WORK beads, which stay in the city/rig work stores. Wrapping the shared
// opener blindly would misroute the by-id read, so the two uses are split:
// openSourceWorkflowGraphStores includes the infra store, openSourceWorkflowStores
// does not.

// seedSplitCityInfraMarker writes the infra scope's canonical config marker so
// cityHasInfraStore(cityPath) reports true, simulating a split city without
// standing up dolt.
func seedSplitCityInfraMarker(t *testing.T, cityPath string) {
	t.Helper()
	dir := filepath.Join(infraScopeRoot(cityPath), ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir infra scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("db: infra\n"), 0o644); err != nil {
		t.Fatalf("write infra config marker: %v", err)
	}
}

// withSourceWorkflowInfraStore swaps the graph-root infra-store seam to a test
// store and restores it on cleanup.
func withSourceWorkflowInfraStore(t *testing.T, store beads.Store) {
	t.Helper()
	prev := sourceWorkflowInfraStore
	sourceWorkflowInfraStore = func(string) beads.Store { return store }
	t.Cleanup(func() { sourceWorkflowInfraStore = prev })
}

// TestOpenSourceWorkflowGraphStoresIncludesInfraStoreOnSplitCity proves the
// graph-root variant appends the infra store as a scan candidate on a split
// city, while the by-id variant does not — the load-bearing split.
func TestOpenSourceWorkflowGraphStoresIncludesInfraStoreOnSplitCity(t *testing.T) {
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	withSourceWorkflowInfraStore(t, infra)

	// Every work-store candidate opens to its own memstore; the injected infra
	// store is the graph-root extra. Using a custom openStore keeps the work
	// candidates off the filesystem.
	openStore := func(string) (beads.Store, error) { return beads.NewMemStore(), nil }

	graphViews, graphSkips, err := openSourceWorkflowStoresWith(cfg, cityPath, "", true, openStore)
	if err != nil {
		t.Fatalf("openSourceWorkflowStoresWith(includeInfra=true) err = %v", err)
	}
	if len(graphSkips) != 0 {
		t.Fatalf("graph-root scan skips = %v, want none", graphSkips)
	}
	if !sourceWorkflowViewsIncludeStore(graphViews, infra) {
		t.Fatalf("graph-root scan candidates do not include the infra store; paths=%v", sourceWorkflowViewPaths(graphViews))
	}
	if !sourceWorkflowViewsIncludePath(graphViews, infraScopeRoot(cityPath)) {
		t.Fatalf("graph-root scan candidates do not include the infra scope path %q; paths=%v",
			infraScopeRoot(cityPath), sourceWorkflowViewPaths(graphViews))
	}

	// By-id (work-class) variant: the infra store must NOT be a candidate.
	byIDViews, _, err := openSourceWorkflowStoresWith(cfg, cityPath, "", false, openStore)
	if err != nil {
		t.Fatalf("openSourceWorkflowStoresWith(includeInfra=false) err = %v", err)
	}
	if sourceWorkflowViewsIncludeStore(byIDViews, infra) {
		t.Fatalf("by-id scan candidates unexpectedly include the infra store; paths=%v", sourceWorkflowViewPaths(byIDViews))
	}
	// The graph-root variant is exactly the by-id fan-out plus the infra store.
	if len(graphViews) != len(byIDViews)+1 {
		t.Fatalf("graph-root candidates = %d, want by-id candidates (%d) + 1 infra", len(graphViews), len(byIDViews))
	}
}

// TestOpenSourceWorkflowGraphStoresIsWorkOnlyOnLegacyCity pins byte-identity on
// a legacy single-store city: with no infra scope, the graph-root variant and
// the by-id variant return the identical work-store fan-out.
func TestOpenSourceWorkflowGraphStoresIsWorkOnlyOnLegacyCity(t *testing.T) {
	cityPath := t.TempDir() // no infra marker seeded ⇒ cityHasInfraStore == false
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	// Fail loudly if the infra seam is consulted at all on a legacy city.
	withSourceWorkflowInfraStore(t, nil)
	sourceWorkflowInfraStore = func(string) beads.Store {
		t.Fatal("sourceWorkflowInfraStore consulted on a legacy single-store city")
		return nil
	}

	openStore := func(string) (beads.Store, error) { return beads.NewMemStore(), nil }

	graphViews, _, err := openSourceWorkflowStoresWith(cfg, cityPath, "", true, openStore)
	if err != nil {
		t.Fatalf("graph-root variant err = %v", err)
	}
	byIDViews, _, err := openSourceWorkflowStoresWith(cfg, cityPath, "", false, openStore)
	if err != nil {
		t.Fatalf("by-id variant err = %v", err)
	}
	if len(graphViews) != len(byIDViews) {
		t.Fatalf("legacy city: graph-root candidates = %d, want == by-id candidates (%d)", len(graphViews), len(byIDViews))
	}
	if len(graphViews) == 0 {
		t.Fatal("legacy city returned zero candidates; want the work-store fan-out")
	}
}

// TestGraphRootScanFindsInfraResidentWorkflowRoot closes the loop: a workflow
// root that lives ONLY in the infra store (as a split city routes it) is found
// by scanning the graph-root variant's candidates with ListLiveRoots, and is
// invisible to the by-id (work-only) fan-out — the exact miss the E2.3 note
// flagged for a split city's finalize/delete-source walk.
func TestGraphRootScanFindsInfraResidentWorkflowRoot(t *testing.T) {
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	const sourceBeadID = "gc-1234"

	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	withSourceWorkflowInfraStore(t, infra)

	// The workflow root is a graph-class bead; on a split city sling/order create
	// it in the infra store. Seed it there only.
	root, err := infra.Create(beads.Bead{
		Title: "workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey: sourceBeadID,
		},
	})
	if err != nil {
		t.Fatalf("seed infra workflow root: %v", err)
	}

	openStore := func(string) (beads.Store, error) { return beads.NewMemStore(), nil }

	// Graph-root variant: scan every candidate for the live root. It must be found
	// in the infra store.
	graphViews, _, err := openSourceWorkflowStoresWith(cfg, cityPath, sourceBeadID, true, openStore)
	if err != nil {
		t.Fatalf("graph-root variant err = %v", err)
	}
	if !scanFindsWorkflowRoot(t, graphViews, sourceBeadID, root.ID) {
		t.Fatalf("graph-root scan did not find the infra-resident workflow root %q", root.ID)
	}

	// By-id (work-only) variant: the same scan finds nothing — proving the root is
	// invisible without the infra store, i.e. the fix is load-bearing.
	byIDViews, _, err := openSourceWorkflowStoresWith(cfg, cityPath, sourceBeadID, false, openStore)
	if err != nil {
		t.Fatalf("by-id variant err = %v", err)
	}
	if scanFindsWorkflowRoot(t, byIDViews, sourceBeadID, root.ID) {
		t.Fatal("by-id (work-only) scan unexpectedly found the infra-resident workflow root; the miss control is not exercised")
	}
}

func scanFindsWorkflowRoot(t *testing.T, views []convoyStoreView, sourceBeadID, rootID string) bool {
	t.Helper()
	for _, view := range views {
		roots, err := sourceworkflow.ListLiveRoots(view.store, sourceBeadID, "", "")
		if err != nil {
			t.Fatalf("ListLiveRoots on %q: %v", view.path, err)
		}
		for _, r := range roots {
			if r.ID == rootID {
				return true
			}
		}
	}
	return false
}

func sourceWorkflowViewsIncludeStore(views []convoyStoreView, store beads.Store) bool {
	for _, view := range views {
		if sameStorePtr(view.store, store) {
			return true
		}
	}
	return false
}

func sourceWorkflowViewsIncludePath(views []convoyStoreView, path string) bool {
	for _, view := range views {
		if view.path == path {
			return true
		}
	}
	return false
}

func sourceWorkflowViewPaths(views []convoyStoreView) []string {
	paths := make([]string, 0, len(views))
	for _, view := range views {
		paths = append(paths, view.path)
	}
	return paths
}
