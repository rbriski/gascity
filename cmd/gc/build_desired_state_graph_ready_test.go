package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// graphDemandReadyReader is a CachedReader/LiveReader whose Ready returns a
// fixed slice. In these tests it stands in for the FULL FEDERATED ready set
// under graph_store=sqlite: the per-tick Limit has already evicted a
// genuinely-ready graph wisp because the Dolt work leg filled the window.
type graphDemandReadyReader struct {
	ready []beads.Bead
}

func (r graphDemandReadyReader) Get(string) (beads.Bead, error)              { return beads.Bead{}, nil }
func (r graphDemandReadyReader) List(beads.ListQuery) ([]beads.Bead, error)  { return nil, nil }
func (r graphDemandReadyReader) DepList(string, string) ([]beads.Dep, error) { return nil, nil }
func (r graphDemandReadyReader) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), r.ready...), nil
}

// graphDemandStore models the work-leg store under graph_store=sqlite: its
// federated Cached/Live Ready returns a work-leg backlog that has truncated a
// genuinely-ready graph wisp out of the per-tick limit window. The dedicated
// graph store (graphDemandGraphStore) holds the wisp. The controller-demand
// readiness probe must read the graph store directly (mirroring readyStoreSet)
// so an assigned, deps-satisfied wisp is recognized as ready.
type graphDemandStore struct {
	beads.Store
	federated []beads.Bead
}

func (s *graphDemandStore) Handles() beads.StoreHandles {
	r := graphDemandReadyReader{ready: s.federated}
	return beads.StoreHandles{Cached: r, Live: r}
}

// graphDemandGraphStore is the dedicated graph store: its Ready returns the
// ClassGraph ready slice alone, where the assigned wisp survives because the
// Dolt work leg is excluded.
type graphDemandGraphStore struct {
	beads.Store
	ready []beads.Bead
}

func (s *graphDemandGraphStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), s.ready...), nil
}

func graphDemandContains(rows []beads.Bead, id string) bool {
	for _, b := range rows {
		if b.ID == id {
			return true
		}
	}
	return false
}

// The assigned cleanup wisp, ready (its sole blocks-dep closed), but evicted
// from the federated per-tick limit window by older work-leg beads.
func graphDemandWisp() beads.Bead {
	return beads.Bead{ID: "gcg-1590", Status: "open", Assignee: "mc-test"}
}

func TestLiveReadyForControllerDemandPrefersGraphOnly(t *testing.T) {
	store := &graphDemandStore{
		federated: []beads.Bead{{ID: "work-1", Status: "open"}, {ID: "work-2", Status: "open"}},
	}
	graph := &graphDemandGraphStore{ready: []beads.Bead{graphDemandWisp()}}
	got, err := liveReadyForControllerDemandQuery(store, graph, beads.ReadyQuery{Assignee: "mc-test", Limit: 5})
	if err != nil {
		t.Fatalf("liveReadyForControllerDemandQuery: %v", err)
	}
	if !graphDemandContains(got, "gcg-1590") {
		t.Fatalf("expected graph store ready slice to contain the assigned wisp gcg-1590, got %v", got)
	}
}

// routedWorkLegReader models a routed (graph_store=sqlite) work store's read
// handles: List returns nothing (the in-progress/open passes contribute no
// demand here so the test isolates the Ready/deps leg), and Live.Ready returns
// the FEDERATED work∪graph union — exactly what coordrouter.Router.Ready returns
// when a rig leg is probed with Live.Ready instead of the graph-only handle. The
// pre-rewire code read Router.ReadyGraphOnly (graph alone) on every leg; the bug
// is reading this federated set on rig legs because their graphStore was left nil.
type routedWorkLegReader struct {
	federated []beads.Bead
}

func (r routedWorkLegReader) Get(string) (beads.Bead, error)              { return beads.Bead{}, nil }
func (r routedWorkLegReader) List(beads.ListQuery) ([]beads.Bead, error)  { return nil, nil }
func (r routedWorkLegReader) DepList(string, string) ([]beads.Dep, error) { return nil, nil }
func (r routedWorkLegReader) Ready(...beads.ReadyQuery) ([]beads.Bead, error) {
	return append([]beads.Bead(nil), r.federated...), nil
}

// routedWorkLegStore is a work store (city or rig) under graph_store=sqlite: its
// Live/Cached handles return the federated work∪graph Ready, so a leg that reads
// it directly (graphStore==nil) leaks the WORK beads. The dedicated graph store
// (graphDemandGraphStore) holds the graph-only ready set.
type routedWorkLegStore struct {
	beads.Store
	federated []beads.Bead
}

func (s *routedWorkLegStore) Handles() beads.StoreHandles {
	r := routedWorkLegReader{federated: s.federated}
	return beads.StoreHandles{Cached: r, Live: r}
}

// TestCollectAssignedWorkReadyGraphOnlyExcludesRigWorkLegUnderSQLite is the
// realistic regression guard for the controller-demand wake gate under
// graph_store=sqlite on a multi-rig city. Both the city and rig work stores are
// routed (their Live.Ready returns work∪graph), and the graph class is relocated
// to a dedicated shared store. The deps-ready demand pass must read the shared
// graph store ALONE on EVERY leg (city and rig), reproducing the pre-rewire
// Router.ReadyGraphOnly: readyAssignedIDs / result must contain only the assigned
// graph wisp and never an assigned rig-WORK bead. Leaving the rig leg's graphStore
// nil would read Router.Ready and leak the rig-work bead into the wake gate.
func TestCollectAssignedWorkReadyGraphOnlyExcludesRigWorkLegUnderSQLite(t *testing.T) {
	graphWisp := beads.Bead{ID: "gcg-1590", Status: "open", Assignee: "mc-worker"}
	cityWorkLeak := beads.Bead{ID: "gc-100", Status: "open", Assignee: "mc-worker"}
	rigWorkLeak := beads.Bead{ID: "gc-200", Status: "open", Assignee: "rig-worker"}

	cityStore := &routedWorkLegStore{federated: []beads.Bead{cityWorkLeak, graphWisp}}
	rigStore := &routedWorkLegStore{federated: []beads.Bead{rigWorkLeak, graphWisp}}
	graphStore := &graphDemandGraphStore{ready: []beads.Bead{graphWisp}}

	cfg := &config.City{
		Rigs: []config.Rig{{Name: "rig1", Path: "/tmp/rig1"}},
	}
	rigStores := map[string]beads.Store{"rig1": rigStore}

	result, _, _, readyAssignedIDs, partial :=
		collectAssignedWorkBeadsWithStores(cfg, cityStore, graphStore, rigStores, nil, nil)
	if partial {
		t.Fatalf("unexpected partial result")
	}

	if !readyAssignedIDs[graphWisp.ID] {
		t.Fatalf("readyAssignedIDs missing the assigned graph wisp %s: %v", graphWisp.ID, readyAssignedIDs)
	}
	if readyAssignedIDs[cityWorkLeak.ID] {
		t.Fatalf("city WORK bead %s leaked into readyAssignedIDs (read Router.Ready instead of the graph store)", cityWorkLeak.ID)
	}
	if readyAssignedIDs[rigWorkLeak.ID] {
		t.Fatalf("rig WORK bead %s leaked into readyAssignedIDs (rig leg read Router.Ready instead of the shared graph store)", rigWorkLeak.ID)
	}
	if graphDemandContains(result, cityWorkLeak.ID) {
		t.Fatalf("city WORK bead %s leaked into the assigned-work result", cityWorkLeak.ID)
	}
	if graphDemandContains(result, rigWorkLeak.ID) {
		t.Fatalf("rig WORK bead %s leaked into the assigned-work result", rigWorkLeak.ID)
	}
	if !graphDemandContains(result, graphWisp.ID) {
		t.Fatalf("assigned graph wisp %s missing from the result", graphWisp.ID)
	}
}

func TestControllerDemandReadyFallsBackWithoutGraphCapability(t *testing.T) {
	// Identity phase: the graph class is not relocated, so resolveGraphStore
	// returns the work store and the caller passes graphStore == store (or nil).
	// The federated Live.Ready set is returned byte-identically, so default
	// (Dolt-only) cities are unaffected by the fix.
	store := &graphDemandStore{
		federated: []beads.Bead{{ID: "work-1", Status: "open"}},
	}
	got, err := liveReadyForControllerDemandQuery(store, store, beads.ReadyQuery{Limit: 5})
	if err != nil {
		t.Fatalf("liveReadyForControllerDemandQuery: %v", err)
	}
	if len(got) != 1 || got[0].ID != "work-1" {
		t.Fatalf("expected federated fallback [work-1], got %v", got)
	}
	if graphDemandContains(got, "gcg-1590") {
		t.Fatalf("graph wisp must NOT appear when the graph class is not relocated")
	}
}
