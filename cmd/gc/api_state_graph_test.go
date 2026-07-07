package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// writeGraphScopeMarker creates the provider marker that opts a city into a
// journal graph scope (activation by presence).
func writeGraphScopeMarker(t *testing.T, cityPath string) {
	t.Helper()
	dir := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "config.yaml"), []byte("provider: journal\n"), 0o644); err != nil {
		t.Fatalf("write graph scope marker: %v", err)
	}
}

// TestBeadEventGraphClassOwnedButUnloaded pins the HIGH-2 event-plane fallback:
// on a graph-scoped city whose journal leg is not loaded in this process, a gcg-
// event id resolves to the LEGACY graph store (the city store) with known=true —
// NOT (nil, true). Returning a nil store would make beadEventStores gate autoclose
// on an empty set and silently skip convoy/wisp/molecule autoclose. A non-scoped
// city still resolves the same id to (_, false) so it falls through to today's
// all-stores scan.
func TestBeadEventGraphClassOwnedButUnloaded(t *testing.T) {
	scoped := t.TempDir()
	writeGraphScopeMarker(t, scoped)
	city := beads.NewMemStore()
	csScoped := &controllerState{
		cfg:              &config.City{},
		cityPath:         scoped,
		cityBeadStore:    city,
		cityGraphJournal: nil, // scope present, journal leg unloaded in this process
	}
	store, known := csScoped.beadEventConfiguredStoreLocked("gcg-j7")
	if !known {
		t.Fatalf("scoped+unloaded gcg id known = false, want true")
	}
	if !sameStorePtr(store, city) {
		t.Fatalf("scoped+unloaded gcg id store = %p, want the legacy city store %p", store, city)
	}

	unscoped := t.TempDir()
	csUnscoped := &controllerState{
		cfg:           &config.City{},
		cityPath:      unscoped,
		cityBeadStore: beads.NewMemStore(),
	}
	if _, known := csUnscoped.beadEventConfiguredStoreLocked("gcg-j7"); known {
		t.Fatalf("non-scoped gcg id known = true, want false")
	}
}

// TestBeadEventGraphClassUnloadedAutocloseNotSkipped proves the HIGH-2 fix at the
// point the stall manifests: beadEventStoresLocked must return a non-empty store
// set for a gcg BeadClosed event on a scoped-but-unloaded city, since
// applyBeadEventToStores gates runBeadCloseAutoclose on len(stores) > 0. Before
// the fix the graph arm returned (nil, true) → an empty set → autoclose silently
// skipped.
func TestBeadEventGraphClassUnloadedAutocloseNotSkipped(t *testing.T) {
	scoped := t.TempDir()
	writeGraphScopeMarker(t, scoped)
	city := beads.NewMemStore()
	cs := &controllerState{
		cfg:              &config.City{},
		cityPath:         scoped,
		cityBeadStore:    city,
		cityGraphJournal: nil,
	}

	stores := cs.beadEventStoresLocked(events.Event{Type: events.BeadClosed, Subject: "gcg-j7"})
	if len(stores) == 0 {
		t.Fatal("scoped+unloaded gcg close event yielded no stores — autoclose would be silently skipped")
	}
	if len(stores) != 1 || !sameStorePtr(stores[0], city) {
		t.Fatalf("scoped+unloaded gcg close event stores = %v, want the single legacy city store", stores)
	}
}
