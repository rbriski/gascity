package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convergence"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/events"
)

// Phase GD-c converts the three residual coordrouter.Router graph-READ deps to
// the class-aware graph store so the branch is GF-ready (the Router federated
// work∪graph; post-cutover policy(work) returns work-only). These tests prove
// each dep now reaches the dedicated graph store under graph=sqlite, and that
// every conversion is byte-identical (a no-op) at the default graph=bd backend.

// graphRelocatedConvergenceRuntime builds a CityRuntime whose graph class is
// relocated onto the legacy .gc/beads.sqlite store, with a single city/HQ
// convergence scope wired through newConvergenceScope (mirroring
// initConvergenceHandler). It returns the runtime, the work store, and the
// resolved graph store.
func graphRelocatedConvergenceRuntime(t *testing.T) (*CityRuntime, beads.Store, beads.Store) {
	t.Helper()
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	cfg := graphClassSQLiteCfg()
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 cfg,
		rec:                 events.Discard,
		standaloneCityStore: work,
		logPrefix:           "gc test",
		stdout:              &bytes.Buffer{},
		stderr:              &bytes.Buffer{},
	}
	graph := cr.graphBeadStore()
	if graph == nil || graph == beads.Store(work) {
		t.Fatalf("expected a distinct relocated graph store, got %v (work=%v)", graph, work)
	}
	cr.convScopes = map[string]*convergenceScope{
		"": cr.newConvergenceScope("", work, cityPath, nil),
	}
	return cr, work, graph
}

// TestGDC_ConvergenceAdapterReadsReachGraphStore proves DEP 1: with graph
// relocated, the convergence adapter's reads (populateIndex / activeIndex, Get,
// FindByIdempotencyKey) reach the dedicated graph store — a convergence root
// created on the graph store is discovered by the adapter and is invisible on
// the work store.
func TestGDC_ConvergenceAdapterReadsReachGraphStore(t *testing.T) {
	cr, work, graph := graphRelocatedConvergenceRuntime(t)
	scope := cr.convScopes[""]
	if scope.store != graph {
		t.Fatalf("convergence scope must bind the relocated graph store, got %v want %v", scope.store, graph)
	}
	if scope.adapter.store != graph {
		t.Fatalf("convergence adapter must bind the relocated graph store, got %v want %v", scope.adapter.store, graph)
	}

	// A convergence root lives on the graph store (the create-chokepoint routes
	// type=convergence => ClassGraph there). populateIndex must see it.
	root, err := graph.Create(beads.Bead{Title: "loop", Type: "convergence", Status: "in_progress"})
	if err != nil {
		t.Fatalf("create convergence root on graph store: %v", err)
	}
	if err := graph.SetMetadata(root.ID, convergence.FieldState, convergence.StateActive); err != nil {
		t.Fatalf("set state: %v", err)
	}
	if err := graph.SetMetadata(root.ID, convergence.FieldTarget, "agent-x"); err != nil {
		t.Fatalf("set target: %v", err)
	}

	if err := scope.adapter.populateIndex(); err != nil {
		t.Fatalf("populateIndex: %v", err)
	}
	if _, ok := scope.adapter.activeIndex[root.ID]; !ok {
		t.Fatalf("active index did not include graph-resident convergence root %s; reads missed the graph store", root.ID)
	}

	// GetBead and CountActiveConvergenceLoops also resolve through the graph store.
	if info, err := scope.adapter.GetBead(root.ID); err != nil || info.ID != root.ID {
		t.Fatalf("GetBead(%s) via adapter: info=%+v err=%v", root.ID, info, err)
	}
	if n, err := scope.adapter.CountActiveConvergenceLoops("agent-x"); err != nil || n != 1 {
		t.Fatalf("CountActiveConvergenceLoops=agent-x got (%d,%v) want (1,nil)", n, err)
	}

	// Idempotency lookup by metadata reaches the graph store, not the work store.
	// A non-"converge:"-prefixed key drives the findByKeyScan fallback, which does
	// a List(Metadata: idempotency_key) — the scan must hit the graph store.
	if err := graph.SetMetadata(root.ID, "idempotency_key", "gdc-key-1"); err != nil {
		t.Fatalf("stamp idempotency key: %v", err)
	}
	id, found, err := scope.adapter.FindByIdempotencyKey("gdc-key-1")
	if err != nil || !found || id != root.ID {
		t.Fatalf("FindByIdempotencyKey via graph store got (%q,%v,%v) want (%s,true,nil)", id, found, err, root.ID)
	}

	// The work store never saw the convergence root.
	if all, _ := work.List(beads.ListQuery{Type: "convergence"}); len(all) != 0 {
		t.Fatalf("work store has %d convergence beads, want 0 (they belong to the graph store)", len(all))
	}
}

// TestGDC_ConvergenceAdapterByteIdenticalAtGraphBD proves DEP 1 is a no-op at the
// default backend: with graph not relocated, the convergence scope keeps the
// per-scope work store it was handed (graphBeadStore is not consulted), so a rig
// or city convergence loop stays physically on that store.
func TestGDC_ConvergenceAdapterByteIdenticalAtGraphBD(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	cr := &CityRuntime{
		cityPath:            cityPath,
		cityName:            "test",
		cfg:                 &config.City{},
		rec:                 events.Discard,
		standaloneCityStore: work,
		logPrefix:           "gc test",
		stdout:              &bytes.Buffer{},
		stderr:              &bytes.Buffer{},
	}
	// A rig scope handed a DISTINCT store must keep it at graph=bd (no diversion
	// to the city store).
	rigStore := beads.NewMemStore()
	scope := cr.newConvergenceScope("rig-a", rigStore, cityPath, nil)
	if scope.store != beads.Store(rigStore) {
		t.Fatalf("graph=bd: rig convergence scope must keep its work store, got %v want %v", scope.store, rigStore)
	}
	if scope.adapter.store != beads.Store(rigStore) {
		t.Fatalf("graph=bd: rig convergence adapter must keep its work store, got %v want %v", scope.adapter.store, rigStore)
	}
}

// TestGDC_CollectInputConvoyWorkflowRootsViaGraphStore proves DEP 2: a root-only
// graph.v2 wisp discovered via its synthetic input-convoy tracking edge is found
// when discovery runs against the graph store, where the synthetic convoy, its
// tracks dep, and the graph.v2 root all live once the graph class is relocated.
func TestGDC_CollectInputConvoyWorkflowRootsViaGraphStore(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	graph, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	// The just-closed work issue lives on the work store.
	parent, err := work.Create(beads.Bead{Title: "issue", Type: "task", Status: "closed"})
	if err != nil {
		t.Fatalf("create parent issue: %v", err)
	}

	// The synthetic tracking convoy + its tracks edge are graph-resident.
	convoy, err := graph.Create(beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	if err != nil {
		t.Fatalf("create synthetic convoy: %v", err)
	}
	if err := graph.DepAdd(convoy.ID, parent.ID, convoycore.TrackingDepType); err != nil {
		t.Fatalf("add tracks dep: %v", err)
	}

	// The root-only graph.v2 wisp linking back solely via gc.input_convoy_id.
	root, err := graph.Create(beads.Bead{
		Title:  "mol-focus-review root",
		Type:   "molecule",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.InputConvoyIDMetadataKey:  convoy.ID,
			beadmeta.FormulaContractMetadataKey: "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("create graph.v2 root: %v", err)
	}

	// Discovery against the graph store finds the root.
	got := collectInputConvoyWorkflowRoots(graph, work, parent, map[string]bool{})
	if len(got) != 1 || got[0].ID != root.ID {
		t.Fatalf("graph-store discovery returned %v, want [%s]", rootIDsOf(got), root.ID)
	}

	// Byte-identity floor: at graph=bd graph == work; the work store holds nothing
	// graph-resident, so discovery against the work store finds nothing (exactly
	// the pre-GD-c single-store behavior for a relocated city's leftover work store).
	if got := collectInputConvoyWorkflowRoots(work, work, parent, map[string]bool{}); len(got) != 0 {
		t.Fatalf("work-store discovery found %d roots, want 0", len(got))
	}
}

func rootIDsOf(beadsList []beads.Bead) []string {
	ids := make([]string, len(beadsList))
	for i, b := range beadsList {
		ids[i] = b.ID
	}
	return ids
}

// TestGDC_GetControlBeadByIDResolvesGraphResident proves DEP 3: a graph-resident
// control bead (gcg-* prefix) is resolved by getControlBeadByID via the
// [graph, work] federation when the graph class is relocated, even though the
// scope's (work-backed) control store cannot see it.
func TestGDC_GetControlBeadByIDResolvesGraphResident(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	cfg := graphClassSQLiteCfg()
	graph := resolveGraphStore(work, cfg, cityPath, nil)
	if graph == beads.Store(work) {
		t.Fatal("expected a distinct relocated graph store")
	}

	// A control bead lives on the graph store (gcg-* prefix).
	ctrl, err := graph.Create(beads.Bead{
		Title:    "fanout control",
		Type:     "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: "fanout", beadmeta.RootBeadIDMetadataKey: "gcg-100"},
	})
	if err != nil {
		t.Fatalf("create control bead on graph store: %v", err)
	}

	// The work-backed scope store cannot see it.
	if _, err := work.Get(ctrl.ID); err == nil {
		t.Fatalf("control bead %s unexpectedly visible on the work store", ctrl.ID)
	}

	// getControlBeadByID federates [graph, work] and finds it. cityPath here is a
	// bare tempdir (no city.toml / rigs), so the scope resolves to the city scope
	// and the graph store is the legacy .gc/beads.sqlite handle.
	got, err := getControlBeadByID(work, ctrl.ID, cityPath, cityPath, cfg)
	if err != nil {
		t.Fatalf("getControlBeadByID(%s) via [graph, work]: %v", ctrl.ID, err)
	}
	if got.ID != ctrl.ID {
		t.Fatalf("getControlBeadByID returned %s, want %s", got.ID, ctrl.ID)
	}
}

// TestGDC_GetControlBeadByIDByteIdenticalAtGraphBD proves DEP 3 is a no-op at the
// default backend: with graph not relocated, getControlBeadByID is exactly
// store.Get(beadID) — a bead on the work store resolves, and a missing id returns
// the store's own ErrNotFound.
func TestGDC_GetControlBeadByIDByteIdenticalAtGraphBD(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	cfg := &config.City{}

	wb, err := work.Create(beads.Bead{Title: "control", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	got, err := getControlBeadByID(work, wb.ID, cityPath, cityPath, cfg)
	if err != nil || got.ID != wb.ID {
		t.Fatalf("graph=bd getControlBeadByID got (%s,%v) want (%s,nil)", got.ID, err, wb.ID)
	}
	if _, err := getControlBeadByID(work, "gc-missing", cityPath, cityPath, cfg); err == nil {
		t.Fatal("graph=bd getControlBeadByID for a missing id should error")
	}
}
