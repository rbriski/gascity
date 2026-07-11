package main

import (
	"io"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// TestSlingSourceWorkflowStoresIncludesInfraOnSplitCity pins landmine #8: the
// CLI sling's SourceWorkflowStores list feeds the source-workflow singleton scan
// (ListLiveRoots), and on a split city workflow roots live in the infra store.
// The list must therefore include the infra store, or a duplicate workflow can
// launch (the scan misses the live infra root).
func TestSlingSourceWorkflowStoresIncludesInfraOnSplitCity(t *testing.T) {
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	infra := beads.NewMemStoreHonoringIDs()
	withSourceWorkflowInfraStore(t, infra)

	// Work-store candidates open to their own memstores so we never touch dolt.
	openStore := func(string) (beads.Store, error) { return beads.NewMemStore(), nil }

	got, err := slingSourceWorkflowStoresWith(cfg, cityPath, "test-city", io.Discard, openStore)
	if err != nil {
		t.Fatalf("slingSourceWorkflowStoresWith: %v", err)
	}
	found := false
	for _, s := range got {
		if s.Store == beads.Store(infra) {
			found = true
		}
	}
	if !found {
		t.Fatalf("sling source-workflow stores do not include the infra store (singleton scan would miss infra-resident roots); got %d stores", len(got))
	}
}

// TestSlingSourceWorkflowStoresFailsLoudOnBrokenInfra: a broken infra store on a
// split city means the singleton invariant is silently open, so the list build
// must fail loud rather than warn-and-continue with a work-only scan.
func TestSlingSourceWorkflowStoresFailsLoudOnBrokenInfra(t *testing.T) {
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath)
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	withSourceWorkflowInfraStore(t, nil) // infra seam returns nil ⇒ infra skip
	openStore := func(string) (beads.Store, error) { return beads.NewMemStore(), nil }

	_, err := slingSourceWorkflowStoresWith(cfg, cityPath, "test-city", io.Discard, openStore)
	if err == nil {
		t.Fatal("slingSourceWorkflowStoresWith must fail loud when the infra store is broken on a split city")
	}
	if !strings.Contains(err.Error(), infraScopeRoot(cityPath)) {
		t.Fatalf("error = %v, want it to name the infra scope root %q", err, infraScopeRoot(cityPath))
	}
}

// TestSlingSourceWorkflowStoresLegacyIsWorkOnly pins byte-identity on a legacy
// single-store city: no infra marker ⇒ no infra store consulted, the list is the
// plain work-store fan-out.
func TestSlingSourceWorkflowStoresLegacyIsWorkOnly(t *testing.T) {
	cityPath := t.TempDir() // no infra marker seeded ⇒ cityHasInfraStore == false
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}

	withSourceWorkflowInfraStore(t, nil)
	sourceWorkflowInfraStore = func(string) beads.Store {
		t.Fatal("sourceWorkflowInfraStore consulted on a legacy single-store city")
		return nil
	}

	work := beads.NewMemStore()
	openStore := func(string) (beads.Store, error) { return work, nil }

	got, err := slingSourceWorkflowStoresWith(cfg, cityPath, "test-city", io.Discard, openStore)
	if err != nil {
		t.Fatalf("slingSourceWorkflowStoresWith: %v", err)
	}
	if len(got) != 1 || got[0].Store != beads.Store(work) {
		t.Fatalf("legacy city source-workflow stores = %d entries, want exactly the single work store", len(got))
	}
}
