package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
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

// TestNewControllerStatePostgresOpenFailureIsFatal pins the HIGH wiring fix: when a
// backend=postgres city's journal cannot be opened at controller construction,
// newControllerState records a graphJournalStartupErr (which the controller entry
// points abort on) instead of leaving the handle nil and silently routing graph-class
// writes to the legacy work store — the cross-backend split-brain. A confirmed-SQLite
// city that opens fine records no error, and a non-opted city records none either.
func TestNewControllerStatePostgresOpenFailureIsFatal(t *testing.T) {
	prevOpen := newControllerStateOpenCityStore
	t.Cleanup(func() { newControllerStateOpenCityStore = prevOpen })
	newControllerStateOpenCityStore = func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{Store: beads.NewMemStore()}, nil
	}

	t.Run("postgres unresolvable is fatal", func(t *testing.T) {
		city := t.TempDir()
		writeGraphBackendMarker(t, city,
			"backend: postgres\npostgres:\n  dsn_env: GC_GRAPH_CTRL_UNRESOLVABLE\n")
		t.Setenv("GC_GRAPH_CTRL_UNRESOLVABLE", "") // named but empty ⇒ unresolvable

		cs := newControllerState(context.Background(), &config.City{}, runtime.NewFake(), events.NewFake(), "pg-city", city)
		if cs.graphJournalStartupErr == nil {
			t.Fatal("a backend=postgres open failure must set graphJournalStartupErr, not warn-and-degrade")
		}
		if cs.cityGraphJournal != nil {
			t.Fatal("cityGraphJournal must stay nil on a fatal postgres open failure")
		}
	})

	t.Run("sqlite opens with no startup error", func(t *testing.T) {
		city := t.TempDir()
		writeGraphScopeMarker(t, city) // provider: journal ⇒ SQLite default

		cs := newControllerState(context.Background(), &config.City{}, runtime.NewFake(), events.NewFake(), "sqlite-city", city)
		if cs.graphJournalStartupErr != nil {
			t.Fatalf("a healthy SQLite journal must not set graphJournalStartupErr: %v", cs.graphJournalStartupErr)
		}
		if cs.cityGraphJournal == nil {
			t.Fatal("a healthy SQLite journal should be opened and attached")
		}
	})

	t.Run("non-opted city has no startup error", func(t *testing.T) {
		cs := newControllerState(context.Background(), &config.City{}, runtime.NewFake(), events.NewFake(), "plain-city", t.TempDir())
		if cs.graphJournalStartupErr != nil {
			t.Fatalf("a non-opted city must not set graphJournalStartupErr: %v", cs.graphJournalStartupErr)
		}
		if cs.cityGraphJournal != nil {
			t.Fatal("a non-opted city must leave cityGraphJournal nil")
		}
	})
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
