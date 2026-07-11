package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/storeref"
)

// This file is the P3 conformance suite of the split-store harness: the same
// ownership invariants, run over BOTH store topologies via the splitEnv
// fixture (split_topology_env_test.go). Every invariant below guards a bug
// class that has already fired at least once — 18 audited landmines
// (engdocs/contributors/cross-store-split-landmines.md), the wisp-id routing
// seam (landmine #19), and the isCold spawn/drain treadmill (mc-wisp-orphan
// RCA) — and every one of those bugs was the SAME root cause: a call site
// answering "which store owns this class of bead?" differently from the
// canonical dispatch (resolveClassStore / config.IsReservedClassBeadID).
//
// Each invariant is a named subtest with a doc comment naming the incident it
// pins. Each runs in both topologies: the split subtest catches a path that
// hard-codes one store; the single-store subtest is the byte-identity check
// that a split-city fix did not change legacy behavior. Where the two
// topologies legitimately diverge (the wake filter, `gc bd` scope routing),
// the invariant asserts the exact single-store collapse instead.

// TestSplitTopologyConformance drives every conformance invariant over both
// store topologies. Run one invariant with e.g.
//
//	go test ./cmd/gc/ -run 'TestSplitTopologyConformance/I5'
func TestSplitTopologyConformance(t *testing.T) {
	t.Run("I1-ready-federation", func(t *testing.T) { forEachTopology(t, conformanceReadyFederation) })
	t.Run("I2-assigned-work-capture", func(t *testing.T) { forEachTopologyWithRig(t, conformanceAssignedWorkCapture) })
	t.Run("I3-by-id-write-residence", func(t *testing.T) { forEachTopology(t, conformanceByIDWriteResidence) })
	t.Run("I4-materialization-residence", func(t *testing.T) { forEachTopology(t, conformanceMaterializationResidence) })
	t.Run("I5-claim-routing", func(t *testing.T) { forEachTopology(t, conformanceClaimRouting) })
	t.Run("I6-strict-cross-store-deps", func(t *testing.T) { forEachTopology(t, conformanceStrictCrossStoreDeps) })
	t.Run("I7-by-id-read-federation", func(t *testing.T) { forEachTopology(t, conformanceByIDReadFederation) })
	t.Run("I8-residence-sweep", func(t *testing.T) { forEachTopology(t, conformanceResidenceSweep) })
	t.Run("I9-warm-tick-demand", func(t *testing.T) { forEachTopologyWithRig(t, conformanceWarmTickDemand) })
	t.Run("I10-wake-ownership-fast-path", func(t *testing.T) { forEachTopologyWithRig(t, conformanceWakeOwnershipFastPath) })
	t.Run("I11-read-path-consistency", func(t *testing.T) { forEachTopology(t, conformanceReadPathConsistency) })
}

// conformanceReadyFederation (I1) guards landmine #2 (`gc hook --claim` never
// federated the infra store — workers spawned, saw "no work", and exited) and
// its P2 sibling #13 (HTTP bead ready/list). A routed OPEN infra bead — the
// durable control-bead shape AND the ephemeral wisp shape production molecules
// actually take — must be surfaced by the composite claimableStore.Ready that
// backs `gc ready`, the split-city work_query. The single-store subtest is the
// collapse: one store serves both beads through the same composite.
func conformanceReadyFederation(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "routed ready control bead", "city/worker")
	wisp := e.mintWispWith(t, wispOpts{title: "routed ready wisp", routedTo: "city/worker"})

	cs := &claimableStore{work: e.work, infra: e.infra, cityPath: e.cityPath}
	ready, err := cs.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("composite ready: %v", err)
	}
	for _, tt := range []struct{ name, id string }{
		{"durable", durable.ID},
		{"wisp", wisp.ID},
	} {
		if !beadListHasID(ready, tt.id) {
			t.Errorf("%s routed open infra bead %s missing from the composite ready set — the exact \"no work\" fail-open of landmine #2", tt.name, tt.id)
		}
	}
}

// conformanceAssignedWorkCapture (I2) guards the post-claim half of the
// spawn/drain treadmill (mc-wisp-orphan RCA sites 2+4) and the orphan-release
// TOCTOU class (PR #4151's territory — asserted in-process only). A claimed
// (in_progress) infra wisp with a DEAD assignee must be captured by
// collectAssignedWorkBeadsWithStores under the leading store's arm (owner
// store aligned, store-ref "") so orphan release can recover it; a claimed
// wisp whose holder is a LIVE open session must NOT be releasable. Both
// topologies expect the same outcome — release exactly the dead claim — but
// through different legs (split: the store-ref ownership index; legacy: the
// last-resort sessions-store live probe), which is exactly the divergence this
// suite exists to hold in place.
func conformanceAssignedWorkCapture(t *testing.T, e splitEnv) {
	sess, err := e.sessionsStore().Create(newWarmPoolSessionBead(e.qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create live pool session bead: %v", err)
	}
	live := e.mintWispWith(t, wispOpts{title: "live-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: sess.ID})
	dead := e.mintWispWith(t, wispOpts{title: "dead-held claimed wisp", routedTo: e.qualified, status: "in_progress", assignee: "s-dead99"})

	got, stores, refs, _, partial := collectAssignedWorkBeadsWithStores(e.cfg, e.sessionsStore(), e.rigStores, nil, nil)
	if partial {
		t.Fatal("collectAssignedWorkBeadsWithStores reported partial results")
	}
	for _, want := range []beads.Bead{live, dead} {
		i := beadIndexOf(got, want.ID)
		if i < 0 {
			t.Fatalf("claimed wisp %s not captured by collectAssignedWorkBeadsWithStores — post-claim work invisible to the reconciler (treadmill site 2)", want.ID)
		}
		if !sameStorePtr(stores[i], e.sessionsStore()) {
			t.Errorf("wisp %s captured with the wrong owner store — release would mutate a store that does not hold it", want.ID)
		}
		if refs[i] != "" {
			t.Errorf("wisp %s captured under store-ref %q, want \"\" (the leading-store arm)", want.ID, refs[i])
		}
	}

	released := releaseOrphanedPoolAssignments(
		e.sessionsStore(), e.cfg, e.cityPath,
		[]beads.Bead{sess},
		got, stores, refs,
		e.rigStores,
		e.sessionsStore(),
	)
	if len(released) != 1 || released[0].ID != dead.ID {
		t.Errorf("released = %v, want exactly the dead-assignee wisp %s (live holder's claim must survive; dead claim must recover)", released, dead.ID)
	}
	reloaded, err := e.graphStore().Get(live.ID)
	if err != nil {
		t.Fatalf("reload live-held wisp: %v", err)
	}
	if reloaded.Status != "in_progress" || reloaded.Assignee != sess.ID {
		t.Errorf("live holder's wisp = status %q assignee %q, want in_progress/%s (claim wrongfully released — the orphan-release TOCTOU class)", reloaded.Status, reloaded.Assignee, sess.ID)
	}
}

// conformanceByIDWriteResidence (I3) guards the by-id write-residence class:
// landmine #18 (a session-class read against the work store failed the create
// wait) and the `gc bd update gcg-…` silent write drop the cmd_bd infra arm
// fixed — any by-id mutation that resolves the wrong store either fails
// "not found" or, worse, mints a shadow row (the write-dual hazard). Update,
// SetMetadata, and Close through the graph class accessor on a reserved-id
// bead (durable AND wisp) must land in the infra store and must NOT create any
// residue in the work store.
func conformanceByIDWriteResidence(t *testing.T, e splitEnv) {
	shapes := []struct {
		name string
		bead beads.Bead
	}{
		{"durable", mintDurableGraphBead(t, e, "by-id write durable graph bead", "")},
		{"wisp", e.mintWisp(t, "by-id write wisp")},
	}
	owner, ownerName := e.work, "work"
	if e.split {
		owner, ownerName = e.infra, "infra"
	}
	for _, tt := range shapes {
		front := e.classStore(config.BeadClassGraph)
		if err := front.Update(tt.bead.ID, beads.UpdateOpts{Status: stringPtr("in_progress")}); err != nil {
			t.Fatalf("%s: Update via graph class accessor: %v", tt.name, err)
		}
		if err := front.SetMetadata(tt.bead.ID, "gc.conformance_probe", tt.name); err != nil {
			t.Fatalf("%s: SetMetadata via graph class accessor: %v", tt.name, err)
		}
		if err := front.Close(tt.bead.ID); err != nil {
			t.Fatalf("%s: Close via graph class accessor: %v", tt.name, err)
		}
		got, err := owner.Get(tt.bead.ID)
		if err != nil {
			t.Fatalf("%s: bead %s not resident in the %s store after by-id writes: %v", tt.name, tt.bead.ID, ownerName, err)
		}
		if got.Status != "closed" || got.Metadata["gc.conformance_probe"] != tt.name {
			t.Errorf("%s: %s-store bead = status %q probe %q, want closed/%q — a by-id write landed elsewhere", tt.name, ownerName, got.Status, got.Metadata["gc.conformance_probe"], tt.name)
		}
		if e.split {
			if _, err := e.work.Get(tt.bead.ID); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("%s: bead %s resolves in the WORK store after by-id writes (err=%v) — a write minted a shadow row (write-dual)", tt.name, tt.bead.ID, err)
			}
		}
	}
}

// conformanceMaterializationResidence (I4) guards landmine #17 (P1.5: sling
// stranded molecule beads in the wrong store) and the E2.2 boundary invariant:
// a molecule materialized through the graph-class policy front door — the
// durable graph.v2 workflow shape AND the root-only vapor wisp shape — must
// land EVERY bead in the infra store on a split city, zero in the work store.
// The single-store subtest pins the collapse: all beads in the one store.
func conformanceMaterializationResidence(t *testing.T, e splitEnv) {
	ctx := context.Background()
	res, err := molecule.Instantiate(ctx, e.graphStore(), graphRecipe(), molecule.Options{})
	if err != nil {
		t.Fatalf("materialize durable graph molecule: %v", err)
	}
	if res.Created != 2 {
		t.Fatalf("durable molecule created %d beads, want 2 (root + step)", res.Created)
	}
	wres, err := molecule.Instantiate(ctx, e.graphStore(), conformanceWispRecipe(), molecule.Options{})
	if err != nil {
		t.Fatalf("materialize root-only wisp molecule: %v", err)
	}
	wispRoot, err := e.graphStore().Get(wres.RootID)
	if err != nil {
		t.Fatalf("reload wisp molecule root: %v", err)
	}
	if !wispRoot.Ephemeral {
		t.Errorf("wisp molecule root %s is not ephemeral — the vapor shape lost its tier through the front door", wispRoot.ID)
	}

	ids := []string{wres.RootID}
	for _, id := range res.IDMapping {
		ids = append(ids, id)
	}
	owner, ownerName := e.work, "work"
	if e.split {
		owner, ownerName = e.infra, "infra"
	}
	for _, id := range ids {
		if _, err := owner.Get(id); err != nil {
			t.Errorf("materialized bead %s not resident in the %s store: %v", id, ownerName, err)
		}
		if e.split {
			if _, err := e.work.Get(id); !errors.Is(err, beads.ErrNotFound) {
				t.Errorf("materialized bead %s resolves in the WORK store (err=%v) — the explosion leaked across the boundary (landmine #17)", id, err)
			}
		}
	}
	if n := countGraphClassBeads(t, e.work); e.split && n != 0 {
		t.Errorf("work store holds %d graph-class beads after materialization, want 0", n)
	} else if !e.split && n != 3 {
		t.Errorf("single store holds %d graph-class beads, want all 3 (single-store collapse)", n)
	}
}

// conformanceClaimRouting (I5) guards landmine #2's claim-mutation half
// (split_city_claim: `bd update --claim` ran against the work store and failed
// "not found") and landmine #19 (the wisp-id suffix lottery: gcg-wisp-<suffix>
// ids routed by sling.BeadPrefix landed in the infra store only when the
// random suffix happened to be letter-only). storeForID and
// hookClaimTargetsInfra must route the fixture's REAL minted ids — the durable
// gcg- shape and the gcg-wisp-<digits> shape that mis-routed before the fix —
// to the infra store on a split city, and collapse everything to the work
// store on a legacy city.
func conformanceClaimRouting(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "claim-routing durable control bead", "")
	wisp := e.mintWisp(t, "claim-routing wisp")
	workBead, err := e.work.Create(beads.Bead{Title: "claim-routing work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}

	cs := &claimableStore{work: e.work, infra: e.infra, cityPath: e.cityPath}
	for _, tt := range []struct {
		name      string
		id        string
		wantInfra bool
	}{
		{"durable graph bead", durable.ID, true},
		{"wisp (the landmine #19 shape)", wisp.ID, true},
		{"work bead", workBead.ID, false},
	} {
		owner, ownerName := e.work, "work"
		if e.split && tt.wantInfra {
			owner, ownerName = e.infra, "infra"
		}
		if got := cs.storeForID(tt.id); !sameStorePtr(got, owner) {
			t.Errorf("%s: storeForID(%q) did not route to the %s store", tt.name, tt.id, ownerName)
		}
		wantClaimInfra := e.split && tt.wantInfra
		if got := hookClaimTargetsInfra(e.cityPath, tt.id); got != wantClaimInfra {
			t.Errorf("%s: hookClaimTargetsInfra(%q) = %v, want %v — the claim mutation would hit the wrong store", tt.name, tt.id, got, wantClaimInfra)
		}
	}
}

// conformanceStrictCrossStoreDeps (I6) guards landmine #4 (`cook --attach`
// wired a cross-store blocks edge that bd stored non-blocking — the parent
// went READY mid-DAG) and #7/#15 (convoy membership/tracks edges across
// stores). bd cannot express a cross-store dependency, so a blocking dep-add
// whose endpoints resolve to different stores must FAIL LOUD through the
// policy front doors — in both directions, for wisp and durable endpoints
// alike. The single-store subtest is the byte-identity half: one store
// resolves both endpoints and the same calls succeed.
func conformanceStrictCrossStoreDeps(t *testing.T, e splitEnv) {
	workBead, err := e.work.Create(beads.Bead{Title: "cross-dep work bead", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	durable := mintDurableGraphBead(t, e, "cross-dep durable graph bead", "")
	wisp := e.mintWisp(t, "cross-dep wisp")

	for _, tt := range []struct {
		name     string
		front    beads.Store
		from, to string
	}{
		{"work front door: work blocks-on wisp", e.work, workBead.ID, wisp.ID},
		{"graph front door: wisp blocks-on work", e.graphStore(), wisp.ID, workBead.ID},
		{"work front door: work blocks-on durable", e.work, workBead.ID, durable.ID},
		{"graph front door: durable blocks-on work", e.graphStore(), durable.ID, workBead.ID},
	} {
		err := tt.front.DepAdd(tt.from, tt.to, "blocks")
		if e.split {
			if err == nil || !strings.Contains(err.Error(), "no issue found") {
				t.Errorf("%s: DepAdd(%s → %s) = %v, want the bd-shaped \"no issue found\" rejection (a fail-open cross-store edge is landmine #4)", tt.name, tt.from, tt.to, err)
			}
		} else if err != nil {
			t.Errorf("%s: single-store DepAdd(%s → %s) = %v, want success (one store resolves both endpoints)", tt.name, tt.from, tt.to, err)
		}
	}
}

// conformanceByIDReadFederation (I7) guards the by-id read half of the split:
// landmine #15 Half B (graph/convoy views NotFound-ing on cross-store member
// ids) and the `gc bd` scope seam (a reserved id that matched no rig/HQ prefix
// fell through to the city work store and the exec'd bd silently read/wrote
// the wrong database). storeref.Resolve and the composite Get must find
// reserved-id beads — durable and wisp — across [work, infra], and
// resolveBdScopeTarget must target the infra scope for them on a split city
// while keeping the city-scope answer on a legacy city.
func conformanceByIDReadFederation(t *testing.T, e splitEnv) {
	origProbe := bdBeadExists
	t.Cleanup(func() { bdBeadExists = origProbe })
	// The infra arm routes by prefix WITHOUT a work-store existence probe; fail
	// every probe so a regression that reintroduces one is caught.
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }
	t.Setenv("GC_RIG", "")

	durable := mintDurableGraphBead(t, e, "federated durable graph bead", "")
	wisp := e.mintWisp(t, "federated read wisp")
	cs := &claimableStore{work: e.work, infra: e.infra, cityPath: e.cityPath}
	legs := []beads.Store{e.work, e.infra} // infra is nil on single-store; Resolve skips nil legs

	for _, tt := range []struct{ name, id string }{
		{"durable", durable.ID},
		{"wisp", wisp.ID},
	} {
		if got, err := storeref.Resolve(tt.id, legs); err != nil || got.ID != tt.id {
			t.Errorf("%s: storeref.Resolve(%q) = (%q, %v), want the bead", tt.name, tt.id, got.ID, err)
		}
		if got, err := cs.Get(tt.id); err != nil || got.ID != tt.id {
			t.Errorf("%s: composite Get(%q) = (%q, %v), want the bead", tt.name, tt.id, got.ID, err)
		}

		target, err := resolveBdScopeTarget(e.cfg, e.cityPath, "", []string{"update", tt.id, "--set-metadata", "gc.probe=x"}, false)
		if err != nil {
			t.Fatalf("%s: resolveBdScopeTarget: %v", tt.name, err)
		}
		if e.split {
			want := execStoreTarget{ScopeRoot: infraScopeRoot(e.cityPath), ScopeKind: "infra", Prefix: config.InfraScopePrefix}
			if target != want {
				t.Errorf("%s: resolveBdScopeTarget(update %s) = %#v, want the infra scope %#v — the exec'd bd would silently hit the work store", tt.name, tt.id, target, want)
			}
		} else if target.ScopeKind != "city" || target.ScopeRoot != e.cityPath {
			t.Errorf("%s: resolveBdScopeTarget(update %s) = %#v, want the city scope at %q (single-store collapse)", tt.name, tt.id, target, e.cityPath)
		}
	}
}

// conformanceResidenceSweep (I8) is the integrity backstop, generalizing the
// E2.2 boundary invariant (infra_store_boundary_invariant_test.go) onto the
// two-topology fixture: after minting a representative population — work
// beads with a dep, one durable coordination bead per class (session, mail,
// order-tracking, nudge), a wisp, and a full molecule — every bead's
// coordclass classification must match its resident store, every dependency's
// endpoints must co-reside, and the reserved id-prefix boundary must hold
// (every infra id reserved, no work id reserved). On a legacy city the
// population collapses into the one store and no id is reserved-prefixed.
func conformanceResidenceSweep(t *testing.T, e splitEnv) {
	w1, err := e.work.Create(beads.Bead{Title: "sweep work bead one", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	w2, err := e.work.Create(beads.Bead{Title: "sweep work bead two", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if err := e.work.DepAdd(w2.ID, w1.ID, "blocks"); err != nil {
		t.Fatalf("co-resident work dep: %v", err)
	}
	if _, err := e.classStore(config.BeadClassSessions).Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-1"},
	}); err != nil {
		t.Fatalf("create session bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassMessaging).Create(beads.Bead{Title: "mail: sweep", Type: "message"}); err != nil {
		t.Fatalf("create mail bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassOrders).Create(beads.Bead{
		Title:  "order tracking: sweep",
		Type:   "task",
		Labels: []string{labelOrderTracking},
	}); err != nil {
		t.Fatalf("create order-tracking bead: %v", err)
	}
	if _, err := e.classStore(config.BeadClassNudges).Create(beads.Bead{
		Title:  "nudge: sweep",
		Type:   "task",
		Labels: []string{nudgeBeadLabel},
	}); err != nil {
		t.Fatalf("create nudge bead: %v", err)
	}
	e.mintWisp(t, "sweep wisp")
	if _, err := molecule.Instantiate(context.Background(), e.graphStore(), graphRecipe(), molecule.Options{}); err != nil {
		t.Fatalf("materialize sweep molecule: %v", err)
	}

	legs := []struct {
		name      string
		store     beads.Store
		wantInfra bool
	}{{"work", e.work, false}}
	if e.split {
		legs = append(legs, struct {
			name      string
			store     beads.Store
			wantInfra bool
		}{"infra", e.infra, true})
	}
	for _, leg := range legs {
		list, err := leg.store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
		if err != nil {
			t.Fatalf("%s store list: %v", leg.name, err)
		}
		if len(list) == 0 {
			t.Fatalf("%s store is empty after the representative mint; the sweep is vacuous", leg.name)
		}
		for _, b := range list {
			if e.split {
				if gotInfra := coordclass.Classify(b).IsInfrastructure(); gotInfra != leg.wantInfra {
					t.Errorf("%s store holds %s (type=%q labels=%v metadata=%v): coordclass infra=%v — resident on the wrong side of the boundary", leg.name, b.ID, b.Type, b.Labels, b.Metadata, gotInfra)
				}
			}
			wantReserved := e.split && leg.wantInfra
			if got := config.IsReservedClassBeadID(b.ID); got != wantReserved {
				t.Errorf("%s store bead %q: reserved-class id = %v, want %v — the id-prefix boundary by-id routing rides on is broken", leg.name, b.ID, got, wantReserved)
			}
			deps, err := leg.store.DepList(b.ID, "down")
			if err != nil {
				t.Errorf("%s store DepList(%s): %v", leg.name, b.ID, err)
				continue
			}
			for _, d := range deps {
				if _, err := leg.store.Get(d.DependsOnID); err != nil {
					t.Errorf("dep %s → %s: endpoint does not co-reside in the %s store (%v) — a cross-store edge leaked past the strict guard", b.ID, d.DependsOnID, leg.name, err)
				}
			}
		}
	}
}

// conformanceWarmTickDemand (I9) guards the treadmill driver (mc-wisp-orphan
// RCA site 1): the cross-store demand probe was gated on isCold, so the first
// WARM tick after a cold spawn read routed infra demand as 0 and drained every
// just-spawned session before its agent could claim — pool_desired cycled
// 5,0,0 for hours on the live trace. Through the rig-legged fixture (leading
// store = sessions store, exactly as CityRuntime.buildDesiredState wires it):
// a cold tick spawns sessions for routed leading-store wisps, and CONSECUTIVE
// warm ticks — wisps still open/unclaimed — must keep demand AND the spawned
// sessions desired, without minting replacements. Complements
// split_store_treadmill_test.go by running the same production entry over both
// topologies via the shared fixture.
func conformanceWarmTickDemand(t *testing.T, e splitEnv) {
	e.mintWispWith(t, wispOpts{title: "routed treadmill wisp A", routedTo: e.qualified})
	e.mintWispWith(t, wispOpts{title: "routed treadmill wisp B", routedTo: e.qualified})

	cold := buildDesiredStateWithSessionBeads(
		"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
		e.sessionsStore(), e.rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
	)
	if len(cold.State) != 2 {
		t.Fatalf("cold tick desired sessions = %d, want 2", len(cold.State))
	}

	for tick := 1; tick <= 2; tick++ {
		snap, err := loadSessionBeadSnapshot(e.sessionsStore())
		if err != nil {
			t.Fatalf("load session snapshot before warm tick %d: %v", tick, err)
		}
		warm := buildDesiredStateWithSessionBeads(
			"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), e.rigStores, snap, nil, os.Stderr,
		)
		if got := warm.ScaleCheckCounts[e.qualified]; got != 2 {
			t.Errorf("warm tick %d demand = %d, want 2 (treadmill: routed leading-store demand went blind while sessions ran)", tick, got)
		}
		if len(warm.State) != 2 {
			t.Errorf("warm tick %d desired sessions = %d, want 2 (treadmill: just-spawned sessions fell out of desiredState)", tick, len(warm.State))
		}
	}

	after, err := session.ListAllSessionBeads(e.sessionsStore(), beads.ListQuery{})
	if err != nil {
		t.Fatalf("list session beads after warm ticks: %v", err)
	}
	if len(after) != 2 {
		t.Errorf("session beads after warm ticks = %d, want 2 (warm ticks must reuse the spawned sessions, not mint replacements)", len(after))
	}
}

// conformanceWakeOwnershipFastPath (I10) pins the wake-filter fix
// (split_store_wake_filter_test.go's incident): a rig-bound session holding a
// CLAIMED infra wisp (store-ref "") lost its assigned-work wake reason —
// filterAssignedWorkBeadsForSessionWake dropped the leg, so the reconciler
// churned the live holder through begin-drain/GC_DRAIN_ACK/cancel every tick —
// and the openSessionOwnsWork index missed the leg, so orphan release fell to
// the per-wisp last-resort live probe (a single fail-open leg) every tick. On
// a split city the claim must be wake-visible and ownership-matched through
// the index alone; on a legacy city the "" leg stays dropped/unowned,
// byte-identical to the historical behavior.
func conformanceWakeOwnershipFastPath(t *testing.T, e splitEnv) {
	sess, err := e.sessionsStore().Create(newWarmPoolSessionBead(e.qualified, "executor-1", "1"))
	if err != nil {
		t.Fatalf("create rig-bound pool session bead: %v", err)
	}
	wisp := e.mintWispWith(t, wispOpts{title: "claimed wake wisp", routedTo: e.qualified, status: "in_progress", assignee: sess.ID})

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(
		e.cfg, e.cityPath, sessionInfosFromBeads([]beads.Bead{sess}), []beads.Bead{wisp}, []string{""},
	)
	index := makeOpenSessionStoreRefIndex(e.cityPath, e.cfg, []beads.Bead{sess}, true)
	owns := openSessionOwnsWork(nil, index, sess.ID, "", true)

	if e.split {
		if len(kept) != 1 || kept[0].ID != wisp.ID || len(keptRefs) != 1 || keptRefs[0] != "" {
			t.Errorf("wake filter kept %d beads (refs %v), want the claimed infra wisp under ref \"\" — its session would drain-churn every tick", len(kept), keptRefs)
		}
		if !owns {
			t.Error("ownership index does not own the infra-arm claim — orphan release would fall to the per-wisp live probe every tick")
		}
	} else {
		if len(kept) != 0 {
			t.Errorf("legacy wake filter kept %d beads, want 0 (byte-identity: rig-bound holders never owned the city leg)", len(kept))
		}
		if owns {
			t.Error("legacy ownership index owns the city leg for a rig-bound holder (byte-identity violated)")
		}
	}
}

// conformanceReadPathConsistency (I11) pins the operator-confusion class from
// the live treadmill debugging: a store holding wisps looks EMPTY through
// `bd list` (default main-tier read) while `gc ready` serves work from it —
// operators concluded "no work exists" while the fleet claimed wisps. For a
// store holding one durable graph bead and one open wisp, each read path must
// answer exactly as production does: the raw-leaf default List (the `bd list`
// view) is durable-only; the policy front door's default List is tier-expanded
// (the warm-tick reader view — both beads); Ready through the front door
// includes the open wisp while the leaf default Ready does not; and the
// production ephemeral query (the wisp-GC shape: gc.kind=wisp over both tiers)
// returns exactly the ephemeral-tagged beads.
func conformanceReadPathConsistency(t *testing.T, e splitEnv) {
	durable := mintDurableGraphBead(t, e, "read-path durable graph bead", "")
	wisp := e.mintWisp(t, "read-path wisp")
	front := e.graphStore()
	leaf, _, ok := unwrapBeadPolicyStore(front)
	if !ok {
		t.Fatalf("graph front door %T is not policy-wrapped", front)
	}

	leafList, err := leaf.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("leaf default list: %v", err)
	}
	if !beadListHasID(leafList, durable.ID) || beadListHasID(leafList, wisp.ID) {
		t.Errorf("`bd list` view (leaf default List) sees durable=%v wisp=%v, want true/false — wisps are invisible to the operator's default list", beadListHasID(leafList, durable.ID), beadListHasID(leafList, wisp.ID))
	}

	frontList, err := front.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("front-door default list: %v", err)
	}
	if !beadListHasID(frontList, durable.ID) || !beadListHasID(frontList, wisp.ID) {
		t.Errorf("front-door default List sees durable=%v wisp=%v, want both — warm-tick readers on this path must not be wisp-blind", beadListHasID(frontList, durable.ID), beadListHasID(frontList, wisp.ID))
	}

	frontReady, err := front.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("front-door ready: %v", err)
	}
	if !beadListHasID(frontReady, wisp.ID) || !beadListHasID(frontReady, durable.ID) {
		t.Errorf("front-door Ready sees durable=%v wisp=%v, want both — `gc ready` must include open wisps", beadListHasID(frontReady, durable.ID), beadListHasID(frontReady, wisp.ID))
	}
	leafReady, err := leaf.Ready(beads.ReadyQuery{})
	if err != nil {
		t.Fatalf("leaf default ready: %v", err)
	}
	if beadListHasID(leafReady, wisp.ID) {
		t.Errorf("leaf default Ready surfaces wisp %s — bd's default ready is main-tier only; the tier expansion belongs to the policy front door", wisp.ID)
	}

	eph, err := front.List(beads.ListQuery{
		Metadata:  map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
		TierMode:  beads.TierBoth,
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("ephemeral (wisp-GC shape) query: %v", err)
	}
	if len(eph) != 1 || eph[0].ID != wisp.ID {
		got := make([]string, len(eph))
		for i, b := range eph {
			got[i] = b.ID
		}
		t.Errorf("ephemeral query returned %v, want exactly the wisp %s (ephemeral-tagged beads only)", got, wisp.ID)
	}
	if len(eph) == 1 && !eph[0].Ephemeral {
		t.Errorf("ephemeral query returned %s without the ephemeral flag — the bead is not genuinely on the wisp tier", eph[0].ID)
	}
}

// mintDurableGraphBead creates a DURABLE graph-class bead through the graph
// policy front door: the routed control/workflow shape (gc.kind=workflow),
// which the bd-1.0.5 storage policy keeps off the ephemeral tier. routedTo, if
// non-empty, targets it at a pool template (gc.routed_to). Fixture honesty
// mirrors mintWisp: the bead must classify as infrastructure and must NOT be
// ephemeral, or every "durable" invariant built on it is vacuous.
func mintDurableGraphBead(t *testing.T, e splitEnv, title, routedTo string) beads.Bead {
	t.Helper()
	md := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}
	if routedTo != "" {
		md[beadmeta.RoutedToMetadataKey] = routedTo
	}
	created, err := e.graphStore().Create(beads.Bead{Title: title, Type: "task", Metadata: md})
	if err != nil {
		t.Fatalf("minting durable graph bead %q: %v", title, err)
	}
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("minted durable graph bead %s classifies as work, want infrastructure (type=%q metadata=%v)", created.ID, created.Type, created.Metadata)
	}
	if created.Ephemeral {
		t.Fatalf("minted durable graph bead %s landed on the ephemeral tier, want a durable row", created.ID)
	}
	return created
}

// conformanceWispRecipe is the root-only vapor shape a wisp materializes from:
// the root bead IS the work (gc.kind=wisp), no child steps — the ephemeral
// sibling of graphRecipe.
func conformanceWispRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name:     "vapor",
		RootOnly: true,
		Steps: []formula.RecipeStep{{
			ID:       "vapor",
			Title:    "vapor wisp root",
			Type:     "task",
			IsRoot:   true,
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
		}},
	}
}

// beadIndexOf returns the index of the bead with the given id, or -1.
func beadIndexOf(list []beads.Bead, id string) int {
	for i, b := range list {
		if b.ID == id {
			return i
		}
	}
	return -1
}
