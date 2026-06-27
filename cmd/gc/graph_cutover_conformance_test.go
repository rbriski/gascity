package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/storeref"
)

// graph_cutover_conformance_test.go is the GF gate: the relocated-graph residence
// conformance suite that byte-identity-at-graph=bd CANNOT provide. At graph=bd the
// graph store IS the work store, so a misclassification/misroute is invisible; only
// a REAL two-store setup (Mem work + dedicated SQLite graph at the LEGACY
// <cityPath>/.gc/beads.sqlite) can assert PHYSICAL store residence. Every test runs
// through the REAL post-GF store wiring — routedPolicyStore => policy(work) plus the
// create-chokepoint resolving the dedicated graph store — with NO coordrouter.Router
// anywhere. Each contract is ALSO re-run at graph=bd and asserted identical-to-today
// (the byte-identical default-city invariant).

// cutoverEnv is the post-GF two-store wiring under test: the policy-wrapped work
// store (the create-chokepoint) plus the dedicated graph store the chokepoint routes
// graph-class creates to. At graph=bd graphStore == work (resolveGraphStore returns
// the work store), so the same assertions collapse to the single-store identity.
type cutoverEnv struct {
	cityPath  string
	work      beads.Store // bare work backend (Mem)
	store     beads.Store // policy(work): the post-GF city store, NO Router
	graph     beads.Store // resolveGraphStore: dedicated .gc/beads.sqlite at graph=sqlite, == work at graph=bd
	relocated bool
}

// graphCfgFor returns the city config for a backend mode: graph=sqlite when
// relocated, the bd default otherwise.
func graphCfgFor(relocated bool) *config.City {
	if relocated {
		return graphClassSQLiteCfg()
	}
	return &config.City{}
}

// newCutoverEnv builds the post-GF store wiring for a backend mode. relocated=true
// opts the graph class onto SQLite at the LEGACY location; relocated=false is the
// byte-identical graph=bd default.
func newCutoverEnv(t *testing.T, relocated bool) cutoverEnv {
	t.Helper()
	cityPath := t.TempDir()
	work := beads.NewMemStoreFrom(1000, nil, nil) // offset so work gc-N can overlap graph gcg-N numerics
	cfg := graphCfgFor(relocated)
	store := routedPolicyStore(work, cfg, cityPath)
	t.Cleanup(func() { _ = closeBeadStoreHandle(store) })

	// The post-GF city store must NEVER be a routing object — it is policy(work).
	if base, _, ok := unwrapBeadPolicyStore(store); !ok || base != beads.Store(work) {
		t.Fatalf("post-GF store must be policy(work) with the bare work store under it; base=%T ok=%v", base, ok)
	}

	graph := resolveGraphStore(work, cfg, cityPath, nil)
	if relocated && graph == beads.Store(work) {
		t.Fatal("relocated: resolveGraphStore must return a DISTINCT dedicated store")
	}
	if !relocated && graph != beads.Store(work) {
		t.Fatal("graph=bd: resolveGraphStore must return the work store (byte-identical)")
	}
	return cutoverEnv{cityPath: cityPath, work: work, store: store, graph: graph, relocated: relocated}
}

// graphMoleculeRecipe builds a minimal two-step graph.v2 workflow recipe (root +
// one step). The root carries gc.kind=workflow + gc.formula_contract=graph.v2 so
// coordclass.Classify maps it (and its gc.root_bead_id children) to ClassGraph.
func graphMoleculeRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "mol-graph-conformance",
		Steps: []formula.RecipeStep{
			{
				ID:     "mol-graph-conformance",
				Title:  "graph workflow root",
				Type:   "molecule",
				IsRoot: true,
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
					beadmeta.FormulaContractMetadataKey: "graph.v2",
				},
			},
			{
				ID:    "mol-graph-conformance.step",
				Title: "graph workflow step",
				Type:  "task",
				Metadata: map[string]string{
					beadmeta.FormulaContractMetadataKey: "graph.v2",
				},
			},
		},
		Deps: []formula.RecipeDep{
			{StepID: "mol-graph-conformance.step", DependsOnID: "mol-graph-conformance", Type: "parent-child"},
		},
	}
}

// assertResidesOnGraph asserts a bead id is physically present on the graph store
// and (when relocated) absent from the work store — the orphan landmine guard.
func (e cutoverEnv) assertResidesOnGraph(t *testing.T, id, label string) {
	t.Helper()
	if _, err := e.graph.Get(id); err != nil {
		t.Fatalf("%s: bead %s not physically on the graph store: %v", label, id, err)
	}
	if e.relocated {
		if _, err := e.work.Get(id); err == nil {
			t.Fatalf("%s: graph bead %s LEAKED onto the work store (orphan landmine)", label, id)
		}
	}
}

// assertResidesOnWork asserts a bead id is physically present on the work store.
func (e cutoverEnv) assertResidesOnWork(t *testing.T, id, label string) {
	t.Helper()
	if _, err := e.work.Get(id); err != nil {
		t.Fatalf("%s: bead %s not physically on the work store: %v", label, id, err)
	}
}

// runCutoverConformance runs the full residence conformance contract against one
// backend mode. It is invoked for BOTH graph=sqlite (relocated) and graph=bd
// (identity) so every contract is proven against the real two-store wiring AND
// re-asserted identical-to-today at the default backend.
func runCutoverConformance(t *testing.T, relocated bool) {
	t.Run("1_create_routing_chokepoint", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		// Graph-class create through the policy create-chokepoint lands on the graph
		// store; a work bead lands on the work store.
		gb, err := e.store.Create(beads.Bead{Title: "wisp", Type: "task", Labels: []string{"gc:wisp"}})
		if err != nil {
			t.Fatalf("create graph wisp: %v", err)
		}
		e.assertResidesOnGraph(t, gb.ID, "graph wisp create")
		wb, err := e.store.Create(beads.Bead{Title: "backlog", Type: "task"})
		if err != nil {
			t.Fatalf("create work bead: %v", err)
		}
		e.assertResidesOnWork(t, wb.ID, "work create")
		if relocated && gb.ID == wb.ID {
			t.Fatalf("graph and work ids collided (%q): disjoint prefixes did not separate the namespaces", gb.ID)
		}
		if relocated && !strings.HasPrefix(gb.ID, graphStoreIDPrefix+"-") {
			t.Fatalf("graph bead id %q lacks the disjoint %q- prefix", gb.ID, graphStoreIDPrefix)
		}
	})

	t.Run("1b_molecule_sequential_fallback_no_orphan", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		// Force the sequential-fallback Create loop (molecule.go store.Create path)
		// by deferring assignees, which skips the atomic graph-apply branch — the
		// orphan landmine: every gcg-/workflow bead must still land on the graph
		// store, zero on work. molecule.Instantiate is the caller that, in
		// production, hands the dedicated graph store in for a graph workflow; here we
		// pass e.store (policy(work)) so the create-chokepoint does the routing.
		res, err := molecule.Instantiate(context.Background(), e.store, graphMoleculeRecipe(), molecule.Options{DeferAssignees: true})
		if err != nil {
			t.Fatalf("molecule.Instantiate (sequential fallback): %v", err)
		}
		if res.Created == 0 || len(res.IDMapping) == 0 {
			t.Fatalf("molecule.Instantiate created no beads (created=%d mapping=%d)", res.Created, len(res.IDMapping))
		}
		for stepID, beadID := range res.IDMapping {
			e.assertResidesOnGraph(t, beadID, "molecule sequential-fallback bead "+stepID)
		}
	})

	t.Run("2_by_id_close_claim_release_lands_on_graph", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		gb, err := e.store.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
		if err != nil {
			t.Fatalf("create graph step: %v", err)
		}
		// By-id resolution mirrors beadStoresForID / storeref over [graph, work].
		byID := []beads.Store{e.graph, e.store}
		owner, err := storeref.Resolve(gb.ID, byID)
		if err != nil || owner.ID != gb.ID {
			t.Fatalf("storeref.Resolve(%s) = (%+v, %v), want the graph bead", gb.ID, owner, err)
		}
		// A by-id close lands on the graph store (PrefixOwner routes gcg- there).
		target := storeref.PrefixOwner(gb.ID, byID)
		if target == nil {
			target = e.store
		}
		if err := target.Close(gb.ID); err != nil {
			t.Fatalf("close graph bead: %v", err)
		}
		got, err := e.graph.Get(gb.ID)
		if err != nil || got.Status != "closed" {
			t.Fatalf("graph bead after close = (status %q, %v), want closed on the graph store", got.Status, err)
		}
		if relocated {
			// Claim + ReleaseIfCurrent of the gcg- id resolve to the graph store.
			gb2, err := e.store.Create(beads.Bead{Title: "claimable step", Type: "task", Labels: []string{"gc:wisp"}})
			if err != nil {
				t.Fatalf("create claimable step: %v", err)
			}
			ct := storeref.PrefixOwner(gb2.ID, byID)
			if ct == nil {
				t.Fatal("PrefixOwner did not route the gcg- claim to the graph store")
			}
			if claimer, ok := ct.(beads.Claimer); ok {
				if _, claimed, err := claimer.Claim(gb2.ID, "worker"); err != nil || !claimed {
					t.Fatalf("claim graph bead on the graph store: claimed=%v err=%v", claimed, err)
				}
			} else {
				t.Fatalf("graph store %T is not a beads.Claimer", ct)
			}
			if got, _ := e.graph.Get(gb2.ID); got.Assignee != "worker" {
				t.Fatalf("graph store assignee = %q, want worker (claim did not land on the graph store)", got.Assignee)
			}
			if rel, ok := ct.(beads.ConditionalAssignmentReleaser); ok {
				if _, err := rel.ReleaseIfCurrent(gb2.ID, "worker"); err != nil {
					t.Fatalf("release-if-current on the graph store: %v", err)
				}
			}
		}
	})

	t.Run("3_drain_convoy_cross_class_residence", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		// A synthetic drain-unit convoy is ClassGraph (gc.synthetic) and lands on the
		// graph store; the work backlog members it tracks live on the work store. The
		// convoy seam resolves the cross-class member via memberStores ([work]).
		unitConvoy, err := e.store.Create(beads.Bead{
			Title: "drain unit convoy",
			Type:  "convoy",
			Metadata: map[string]string{
				beadmeta.SyntheticMetadataKey: "true",
			},
		})
		if err != nil {
			t.Fatalf("create synthetic drain convoy: %v", err)
		}
		e.assertResidesOnGraph(t, unitConvoy.ID, "synthetic unit-convoy")

		// A work-class backlog member on the work store.
		member, err := e.work.Create(beads.Bead{Title: "backlog member", Type: "task"})
		if err != nil {
			t.Fatalf("create work member: %v", err)
		}
		e.assertResidesOnWork(t, member.ID, "drain member")

		// The convoy bead's home for the tracks edge is the graph store; the member
		// resolves cross-store via the memberStores tail (the work store).
		convoyStore := e.graph
		memberStores := []beads.Store{e.work}
		if !relocated {
			convoyStore = e.work // identity: everything on one store
			memberStores = nil
		}
		if err := convoy.TrackItem(convoyStore, unitConvoy.ID, member.ID, memberStores...); err != nil {
			t.Fatalf("TrackItem (graph convoy -> work member): %v", err)
		}
		members, err := convoy.Members(convoyStore, unitConvoy.ID, true, memberStores...)
		if err != nil {
			t.Fatalf("convoy.Members: %v", err)
		}
		found := false
		for _, m := range members {
			if m.ID == member.ID {
				found = true
			}
		}
		if !found {
			t.Fatalf("convoy.Members did not return the cross-store work member %s (got %d members)", member.ID, len(members))
		}
		// Reserve/read the member on the WORK store (drain reserves on opts.WorkStore).
		status := "in_progress"
		assignee := "drain-worker"
		if err := e.work.Update(member.ID, beads.UpdateOpts{Status: &status, Assignee: &assignee}); err != nil {
			t.Fatalf("reserve member on the work store: %v", err)
		}
		if got, _ := e.work.Get(member.ID); got.Assignee != "drain-worker" {
			t.Fatalf("member reservation = %q, want drain-worker on the work store", got.Assignee)
		}
	})

	t.Run("4_resolveGraphStore_ready_excludes_work_includes_wisp", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		// A ready work backlog item on the work store.
		workReady, err := e.work.Create(beads.Bead{Title: "ready work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatalf("create ready work bead: %v", err)
		}
		// An ephemeral-wisp ready step on the graph store (TierBoth must include it).
		wisp, err := e.store.Create(beads.Bead{Title: "ready wisp", Type: "task", Labels: []string{"gc:wisp"}, Status: "open", Ephemeral: true})
		if err != nil {
			t.Fatalf("create ready wisp: %v", err)
		}

		ready, err := e.graph.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
		if err != nil {
			t.Fatalf("graph Ready(TierBoth): %v", err)
		}
		ids := make(map[string]bool, len(ready))
		for _, b := range ready {
			ids[b.ID] = true
		}
		if !ids[wisp.ID] {
			t.Fatalf("graph Ready(TierBoth) missing the ephemeral-wisp ready step %s — TierBoth dropped the wisp tier", wisp.ID)
		}
		if e.relocated && ids[workReady.ID] {
			t.Fatalf("graph Ready leaked the Dolt work bead %s into the graph readiness slice", workReady.ID)
		}
		if !e.relocated && !ids[workReady.ID] {
			t.Fatalf("graph=bd: graph Ready (== work Ready) must include the work bead %s", workReady.ID)
		}
	})

	t.Run("5_graphonly_list_forwarder_alive_post_cutover", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		// G2c forwarder guard (the "dead-again" regression): the post-GF dispatcher
		// primary is policy(graphStore); beads.GraphOnlyListFor MUST report ok==true
		// over it so dispatch.liveListForRoot keeps the graph-only fast path (no bd
		// fork into Dolt). The policy store advertises ListGraphOnlyHandle sourced
		// from the dedicated graph store. At graph=bd there is no distinct graph
		// backend, so ok==false and the dispatcher federates — byte-identical default.
		graphPrimary := wrapStoreWithBeadPolicies(e.graph, graphCfgFor(relocated), e.cityPath)
		gol, ok := beads.GraphOnlyListFor(graphPrimary)
		if relocated {
			if !ok {
				t.Fatal("GraphOnlyListFor(policy(graphStore)) = false — the G2c graph-only-list forwarder went dead again after the Router was deleted")
			}
			if gol.GraphIDPrefix() != graphStoreIDPrefix {
				t.Fatalf("GraphIDPrefix() = %q, want %q", gol.GraphIDPrefix(), graphStoreIDPrefix)
			}
			// The forwarder lists the graph store ALONE: a graph wisp is returned, a
			// work bead (on the separate work store) is not.
			gb, err := e.store.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}, Status: "open"})
			if err != nil {
				t.Fatalf("create graph step: %v", err)
			}
			out, err := gol.ListGraphOnly(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
			if err != nil {
				t.Fatalf("ListGraphOnly: %v", err)
			}
			seen := false
			for _, b := range out {
				if b.ID == gb.ID {
					seen = true
				}
			}
			if !seen {
				t.Fatalf("ListGraphOnly did not return the graph step %s", gb.ID)
			}
		} else if ok {
			t.Fatal("graph=bd: GraphOnlyListFor must report ok=false (no distinct graph backend) so the dispatcher federates byte-identically")
		}
	})

	t.Run("6_landmine_graph_store_at_legacy_location", func(t *testing.T) {
		e := newCutoverEnv(t, relocated)
		if !relocated {
			// At graph=bd there is no separate SQLite file; nothing to assert.
			return
		}
		// THE data-orphan landmine: the graph SQLite file MUST be at
		// <cityPath>/.gc/beads.sqlite (citylayout.RuntimeRoot), NOT .gc/graph/.
		if _, err := e.store.Create(beads.Bead{Title: "wisp", Type: "task", Labels: []string{"gc:wisp"}}); err != nil {
			t.Fatalf("create graph bead: %v", err)
		}
		legacy := filepath.Join(e.cityPath, citylayout.RuntimeRoot, "beads.sqlite")
		if _, err := os.Stat(legacy); err != nil {
			t.Fatalf("graph DB not at the LEGACY .gc/beads.sqlite (%s): %v — openGraphSQLiteStore must use citylayout.RuntimeRoot", legacy, err)
		}
		classDir := filepath.Join(classSQLiteDir(e.cityPath, config.BeadClassGraph), "beads.sqlite")
		if _, err := os.Stat(classDir); !os.IsNotExist(err) {
			t.Fatalf("graph DB must NOT exist at the .gc/graph/ class location %s (err=%v) — data-orphan landmine", classDir, err)
		}
	})
}

// TestGraphCutoverConformance_Relocated runs the full residence conformance contract
// against the REAL post-GF two-store wiring at graph=sqlite (Mem work + dedicated
// SQLite graph at the legacy location). This is the gate byte-identity cannot give.
func TestGraphCutoverConformance_Relocated(t *testing.T) {
	runCutoverConformance(t, true)
}

// TestGraphCutoverConformance_DefaultBackendIdentity re-runs every contract at
// graph=bd and asserts identical-to-today: resolveGraphStore returns the work store,
// so every "lands on graph" assertion collapses to the single store and nothing
// regresses for a default city (the byte-identical default-city invariant, point 5).
func TestGraphCutoverConformance_DefaultBackendIdentity(t *testing.T) {
	runCutoverConformance(t, false)
}
