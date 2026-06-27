package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
)

// graphSQLiteCfg (the graph=sqlite city config) is defined in
// api_state_router_test.go and shared across the package's graph-store tests.

// graphClassSQLiteCfg opts the graph class onto SQLite via the canonical
// [beads.classes.graph] knob (the other valid spelling alongside the legacy
// graph_store="sqlite" that graphSQLiteCfg uses). Both normalize to graph=sqlite, so
// resolveGraphStore must route identically for either.
func graphClassSQLiteCfg() *config.City {
	return &config.City{Beads: config.BeadsConfig{
		Classes: map[string]config.BeadClassConfig{
			config.BeadClassGraph: {Backend: config.BeadsBackendSQLite},
		},
	}}
}

// TestOpenGraphSQLiteStore_UsesLegacyLocation pins THE data-orphan landmine: the
// embedded SQLite graph store MUST open at the legacy <cityPath>/.gc/ (the
// citylayout.RuntimeRoot directory itself, file beads.sqlite), NOT at the
// .gc/graph/ class-store convention (classSQLiteDir(cityPath, "graph")). Routing
// graph through .gc/graph/ would point a live graph_store="sqlite" city at an empty
// store and orphan its graph data.
func TestOpenGraphSQLiteStore_UsesLegacyLocation(t *testing.T) {
	cityPath := t.TempDir()
	got, ok := openGraphSQLiteStore(cityPath)
	if !ok || got == nil {
		t.Fatalf("openGraphSQLiteStore returned ok=%v store=%v, want a store", ok, got)
	}

	legacyDir := filepath.Join(cityPath, citylayout.RuntimeRoot)
	classDir := classSQLiteDir(cityPath, config.BeadClassGraph)
	if legacyDir == classDir {
		t.Fatalf("test premise broken: legacy dir %q must differ from class dir %q", legacyDir, classDir)
	}

	// Writing a graph bead must materialize the DB file in the LEGACY dir, never the
	// class dir.
	if _, err := got.Create(beads.Bead{Title: "root", Type: "molecule"}); err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := os.Stat(filepath.Join(legacyDir, "beads.sqlite")); err != nil {
		t.Fatalf("graph DB not at legacy .gc/beads.sqlite (%s): %v", legacyDir, err)
	}
	if _, err := os.Stat(filepath.Join(classDir, "beads.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("graph DB must NOT exist at the .gc/graph/ class location %s (err=%v) — data-orphan landmine", classDir, err)
	}
}

// TestOpenGraphSQLiteStore_SharesCachedHandleWithResolve proves the opener and the
// class-aware resolveGraphStore return the SAME cached handle (graphStoreHandleCache):
// a bead written via the opener is visible through the store resolveGraphStore
// returns, and a second openGraphSQLiteStore call is pointer-identical (cache reuse).
// This is the post-coordrouter wiring — resolveGraphStore replaces the Router's
// ClassGraph leg and must reuse the one cached handle so no second SQLite handle is
// opened against the same file.
func TestOpenGraphSQLiteStore_SharesCachedHandleWithResolve(t *testing.T) {
	cityPath := t.TempDir()

	opened, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}
	// Cache reuse: a second open returns the identical handle, never a fresh one.
	again, ok := openGraphSQLiteStore(cityPath)
	if !ok || again != opened {
		t.Fatalf("openGraphSQLiteStore not cached: again=%v first=%v ok=%v", again, opened, ok)
	}

	bead, err := opened.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create via opener: %v", err)
	}

	// resolveGraphStore must return the SAME cached handle, so the bead written via
	// the opener is visible through the resolved graph store.
	resolved := resolveGraphStore(beads.NewMemStore(), graphSQLiteCfg(), cityPath, nil)
	if resolved != opened {
		t.Fatalf("resolveGraphStore returned a different handle than openGraphSQLiteStore (resolved=%v opened=%v)", resolved, opened)
	}
	if _, err := resolved.Get(bead.ID); err != nil {
		t.Fatalf("bead written via opener not visible through the resolved graph store: %v", err)
	}
}

// TestResolveGraphStore_DefaultReturnsWorkStore pins the byte-identical floor: at the
// default (bd) backend resolveGraphStore IS the work store (same pointer), so nothing
// changes for a default city.
func TestResolveGraphStore_DefaultReturnsWorkStore(t *testing.T) {
	work := beads.NewMemStore()
	got := resolveGraphStore(work, &config.City{}, t.TempDir(), nil)
	if any(got) != any(work) {
		t.Fatal("default backend should return the work store as the graph seam (byte-identical)")
	}
	// nil cfg also falls back to the work store (graphRelocated false).
	if got := resolveGraphStore(work, nil, t.TempDir(), nil); any(got) != any(work) {
		t.Fatal("nil cfg should return the work store")
	}
}

// TestResolveGraphStore_RoutesToLegacySQLiteWhenConfigured proves graph=sqlite routes
// to a distinct store that is the LEGACY .gc/beads.sqlite location (the same cached
// handle openGraphSQLiteStore returns), never the .gc/graph/ class location.
func TestResolveGraphStore_RoutesToLegacySQLiteWhenConfigured(t *testing.T) {
	work := beads.NewMemStore()
	cityPath := t.TempDir()

	got := resolveGraphStore(work, graphClassSQLiteCfg(), cityPath, nil)
	if got == nil {
		t.Fatal("expected a SQLite graph store, got nil")
	}
	if any(got) == any(work) {
		t.Fatal("expected a distinct SQLite store for graph=sqlite, got the work store")
	}

	// It must be the legacy-location handle, identical to openGraphSQLiteStore's.
	legacy, ok := openGraphSQLiteStore(cityPath)
	if !ok || got != legacy {
		t.Fatalf("resolveGraphStore did not return the legacy graph handle (got=%v legacy=%v ok=%v)", got, legacy, ok)
	}

	gb, err := got.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := got.Get(gb.ID); err != nil {
		t.Fatalf("graph bead not in the relocated store: %v", err)
	}
	if all, _ := work.List(beads.ListQuery{AllowScan: true}); len(all) != 0 {
		t.Fatalf("work store has %d beads, want 0 (graph should land in SQLite)", len(all))
	}

	// Landmine: the DB file lives at the legacy dir, not the class dir.
	if _, err := os.Stat(filepath.Join(cityPath, citylayout.RuntimeRoot, "beads.sqlite")); err != nil {
		t.Fatalf("graph DB not at legacy .gc/beads.sqlite: %v", err)
	}
	if _, err := os.Stat(filepath.Join(classSQLiteDir(cityPath, config.BeadClassGraph), "beads.sqlite")); !os.IsNotExist(err) {
		t.Fatalf("graph DB must NOT exist at the .gc/graph/ class location (err=%v) — data-orphan landmine", err)
	}
}
