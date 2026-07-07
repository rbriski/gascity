//go:build integration

// This is the P3.4 half-cutover sweep — the P3 EXIT gate. It stands up ONE live
// city holding THREE graph roots whose residences coexist, and then sweeps the
// full command/read surface asserting each behaves correctly across the mix:
//
//   1. Legacy root       — minted the ordinary way, residence ∅, lives on the
//      legacy leg (the city work store). No residence record.
//   2. Migrated root      — started legacy, then went through the real
//      `gc migrate graph-journal` strand state machine (migrateRoot): its subtree
//      was copied into the journal leg preserving ids, residence flipped to
//      journal, and the legacy copy was tombstoned (closed + gc.migrated=1).
//   3. Born-journal root  — minted AFTER `gc migrate graph-journal cutover`
//      armed the city (the .gc/graph/cutover marker present), so newRootLeg mints
//      it on the journal leg with a fresh gcg-j<seq> id and no residence record —
//      journal membership IS its residence.
//
// The three legs are wired exactly as production wires them
// (newResidenceRoutingGraphStoreForCity over a real city-resolved journal.db plus
// a legacy leg), and the journal frontier / control-store probes resolve the SAME
// on-disk .gc/graph/journal.db through the production cityPath-keyed accessors, so
// the sweep exercises the real routing, the real ControlFrontier, and the real
// cutover-marker mode resolution — not a mock.
//
// Legacy-leg note (honest stubbed-vs-real): the legacy leg is a call-recording
// in-memory beads.Store rather than a bd/dolt store. The migration state machine
// needs its legacy leg to be an in-process beads.Store (it Gets/Closes/hydrates
// the subtree), and unifying one store as BOTH a migration-capable beads.Store and
// a bd-CLI store is out of scope for this slice. The legacy serve frontier is
// therefore derived from the legacy leg's REAL ready set (MemStore.Ready, which
// genuinely excludes the migrated root's tombstoned control beads because they are
// closed) rather than the `bd ready | jq` shell. The compose/merge/no-double-serve
// logic under test — composeWorkflowServeQueue over the two frontiers — is
// identical regardless of how the legacy queue was produced, and the tombstone
// exclusion it depends on is exercised against real store state. Everything else
// (residence routing, journal ControlFrontier, overlay dedupe, cross-leg edge
// rejection, cutover-marker mode) is real.

package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// sweepControlRoute is the control-dispatcher route every seeded control step
// carries (gc.routed_to), matching the routed control tier the journal
// ControlFrontier and the legacy serve tick both key on.
const sweepControlRoute = config.ControlDispatcherAgentName

// sweepRoot bundles a seeded control molecule's ids: the workflow root, its ready
// control step, and a second control step blocked behind the ready one.
type sweepRoot struct {
	root    string
	ready   string // gc.kind=check, routed control-dispatcher, unblocked → served
	blocked string // gc.kind=check, routed control-dispatcher, blocked by ready → NOT served
}

// seedControlMolecule plants a workflow root (gc.kind=workflow) with two control
// steps into store: `ready` (unblocked) and `blocked` (blocks-depends on ready).
// Both steps are control-class (gc.kind=check) routed to the control dispatcher,
// so they are the units the control frontier serves — never worker-claimable. The
// root and steps carry parent-child edges so collectSubtree can walk the molecule
// during migration. store may be a raw leg (legacy seeding) or the residence
// router (born-journal seeding); every op routes correctly either way.
func seedControlMolecule(t *testing.T, store beads.Store, title string) sweepRoot {
	t.Helper()
	mk := func(bt string, meta map[string]string, parent string) string {
		b, err := store.Create(beads.Bead{Title: title + "/" + bt, Type: "task", ParentID: parent, Metadata: meta})
		if err != nil {
			t.Fatalf("seed %s/%s: %v", title, bt, err)
		}
		return b.ID
	}
	control := map[string]string{
		beadmeta.KindMetadataKey:     beadmeta.KindCheck,
		beadmeta.RoutedToMetadataKey: sweepControlRoute,
	}
	root := mk("root", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, "")
	ready := mk("ready", control, root)
	blocked := mk("blocked", control, root)
	for _, dep := range []struct{ from, to, typ string }{
		{ready, root, "parent-child"},
		{blocked, root, "parent-child"},
		{blocked, ready, "blocks"},
	} {
		if err := store.DepAdd(dep.from, dep.to, dep.typ); err != nil {
			t.Fatalf("seed dep %s->%s (%s): %v", dep.from, dep.to, dep.typ, err)
		}
	}
	return sweepRoot{root: root, ready: ready, blocked: blocked}
}

// idSet returns the ids present exactly once, failing on any duplicate.
func idSet(t *testing.T, rows []string) map[string]bool {
	t.Helper()
	set := make(map[string]bool, len(rows))
	for _, id := range rows {
		if set[id] {
			t.Fatalf("id %q appears more than once in %v", id, rows)
		}
		set[id] = true
	}
	return set
}

// TestHalfCutoverThreeResidenceSweep is the P3 exit gate: it proves a live city
// with legacy-resident, migrated-to-journal, and born-journal roots coexisting
// behaves correctly across the full command/read surface.
func TestHalfCutoverThreeResidenceSweep(t *testing.T) {
	ctx := context.Background()
	// Serve mode for the whole test: makeJournalFrontierFn and controlStoreForBead
	// resolve the journal leg under serve semantics. (The cutover marker armed
	// below independently forces serve too; the explicit env pins determinism
	// regardless of ambient harness scrubbing.)
	t.Setenv(graphFrontierModeEnvVar, "serve")

	cityPath := t.TempDir()

	// Opt the city into the graph-journal scope (the real `gc migrate
	// graph-journal init`): creates .gc/graph/.beads/config.yaml + journal.db.
	if err := migrateGraphJournalInit(cityPath); err != nil {
		t.Fatalf("graph-journal init: %v", err)
	}
	// Resolve the city's journal leg through the SAME production cityPath-keyed
	// accessor the frontier func and control-store probe use, so every path shares
	// one authoritative .gc/graph/journal.db handle.
	journalStore, opted, err := cachedCityGraphJournalResult(cityPath)
	if err != nil || !opted || journalStore == nil {
		t.Fatalf("resolve city journal leg: store=%v opted=%t err=%v", journalStore, opted, err)
	}
	res, ok := beads.ResidenceMigrationStoreFor(journalStore)
	if !ok {
		t.Fatal("journal leg does not expose the residence-migration capability")
	}

	// The legacy leg: a call-recording in-memory store (the city work store stand-in).
	// It is BOTH the migration's legacy leg and the residence router's legacy leg,
	// so a migration tombstone genuinely lands where the router later reads it.
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStoreForCity(journalStore, legacy, cityPath)

	// --- residence 1: legacy root (minted legacy, no residence record) ---------
	legacyRoot := seedControlMolecule(t, legacy, "legacy")

	// --- residence 2: migrated root (legacy → journal via the real state machine)
	migrated := seedControlMolecule(t, legacy, "migrated")
	deps := migrateGraphJournalDeps{legacy: legacy, journal: journalStore, res: res, now: time.Now}
	migRes, err := migrateRoot(ctx, deps, migrated.root, false, false)
	if err != nil {
		t.Fatalf("migrateRoot(%s): %v", migrated.root, err)
	}
	if migRes.Outcome != migrateOutcomeCutover || !migRes.Tombstoned {
		t.Fatalf("migration outcome = %+v; want cutover + tombstoned", migRes)
	}
	if state, _, present, _ := res.ResidenceOf(ctx, migrated.root); !present || state != beads.ResidenceStateJournal {
		t.Fatalf("migrated root residence = %q present=%t; want journal", state, present)
	}

	// --- residence 3: born-journal root (minted AFTER cutover arms) ------------
	if err := migrateGraphJournalArmCutover(cityPath); err != nil {
		t.Fatalf("arm cutover: %v", err)
	}
	if !cityGraphCutoverArmed(cityPath) {
		t.Fatal("cutover marker not armed after arm")
	}
	born := seedControlMolecule(t, router, "born") // router mints the root on the journal leg
	// A born-journal root carries the journal mint shape (gcg-j<seq>); a migrated
	// root preserved its legacy id (gc-<seq>). This is the visible generational
	// distinction the whole slice rests on.
	if len(born.root) < 5 || born.root[:5] != "gcg-j" {
		t.Fatalf("born root id = %q; want a journal-minted gcg-j<seq> id", born.root)
	}

	all := map[string]sweepRoot{"legacy": legacyRoot, "migrated": migrated, "born": born}

	// ===================================================================== ROUTING
	t.Run("residence-routing", func(t *testing.T) {
		type want struct {
			journal bool
			leg     beads.Store
		}
		cases := map[string]want{
			legacyRoot.root:  {journal: false, leg: legacy},
			legacyRoot.ready: {journal: false, leg: legacy},
			migrated.root:    {journal: true, leg: journalStore},
			migrated.ready:   {journal: true, leg: journalStore},
			migrated.blocked: {journal: true, leg: journalStore},
			born.root:        {journal: true, leg: journalStore},
			born.ready:       {journal: true, leg: journalStore},
		}
		for id, w := range cases {
			leg, isJournal, err := router.resolveLeg(id)
			if err != nil {
				t.Fatalf("resolveLeg(%s): %v", id, err)
			}
			if isJournal != w.journal || leg != w.leg {
				t.Fatalf("resolveLeg(%s) = journal=%t; want journal=%t", id, isJournal, w.journal)
			}
		}

		// The migrated root exists on BOTH legs (journal-authoritative open copy +
		// legacy tombstone). router.Get MUST serve the journal (open) copy, never the
		// closed legacy tombstone — the core "no bead served from the wrong store".
		got, err := router.Get(migrated.root)
		if err != nil {
			t.Fatalf("Get(migrated root): %v", err)
		}
		if got.Status != "open" {
			t.Fatalf("router.Get(migrated root).Status = %q; want open (journal copy, not the closed legacy tombstone)", got.Status)
		}
		legTomb, err := legacy.Get(migrated.root)
		if err != nil || legTomb.Status != "closed" || legTomb.Metadata["gc.migrated"] != "1" {
			t.Fatalf("legacy migrated copy not a tombstone: %+v err=%v", legTomb, err)
		}

		// Legacy root and born root each resolve to exactly one leg's copy.
		if lr, err := router.Get(legacyRoot.root); err != nil || lr.Status != "open" {
			t.Fatalf("router.Get(legacy root) = %+v err=%v; want open legacy copy", lr, err)
		}
		if br, err := router.Get(born.root); err != nil || br.ID != born.root {
			t.Fatalf("router.Get(born root) = %+v err=%v; want the journal-born copy", br, err)
		}
	})

	// ============================================================ NO CROSS-LEG EDGE
	t.Run("no-cross-leg-dependency-edge", func(t *testing.T) {
		legacy.calls = nil
		// A dependency spanning legacy → journal is rejected (typed refs, not edges,
		// carry cross-class waits in P4/P5). Neither leg is written.
		if err := router.DepAdd(legacyRoot.ready, born.ready, "blocks"); !isCrossResidence(err) {
			t.Fatalf("DepAdd(legacy → journal) err = %v; want errCrossResidenceDependency", err)
		}
		if err := router.DepAdd(migrated.ready, legacyRoot.ready, "blocks"); !isCrossResidence(err) {
			t.Fatalf("DepAdd(journal → legacy) err = %v; want errCrossResidenceDependency", err)
		}
		if legacy.has("DepAdd") {
			t.Fatalf("a cross-leg DepAdd wrote the legacy leg: %v", legacy.calls)
		}

		// Every existing edge in the mix stays within one residence: walk each
		// root's subtree deps and require both ends to co-reside.
		for name, r := range all {
			for _, id := range []string{r.root, r.ready, r.blocked} {
				deps, err := router.DepList(id, "down")
				if err != nil {
					t.Fatalf("%s DepList(%s): %v", name, id, err)
				}
				_, fromJournal, err := router.resolveLeg(id)
				if err != nil {
					t.Fatalf("%s resolveLeg(%s): %v", name, id, err)
				}
				for _, d := range deps {
					if d.DependsOnID == "" {
						continue
					}
					_, toJournal, err := router.resolveLeg(d.DependsOnID)
					if err != nil {
						t.Fatalf("%s resolveLeg(dep %s): %v", name, d.DependsOnID, err)
					}
					if fromJournal != toJournal {
						t.Fatalf("%s: cross-leg edge %s->%s (fromJournal=%t toJournal=%t)", name, id, d.DependsOnID, fromJournal, toJournal)
					}
				}
			}
		}
	})

	// ================================================================ SERVE / FRONTIER
	t.Run("serve-no-double-serve", func(t *testing.T) {
		// Legacy frontier: the legacy leg's REAL ready control set (routed to the
		// dispatcher). The migrated root's tombstoned steps are closed, so Ready
		// genuinely excludes them — the exclusion the merge relies on.
		legacyReady, err := legacy.Ready()
		if err != nil {
			t.Fatalf("legacy.Ready: %v", err)
		}
		var legacyControl []beads.Bead
		for _, b := range legacyReady {
			if b.Metadata[beadmeta.RoutedToMetadataKey] == sweepControlRoute {
				legacyControl = append(legacyControl, b)
			}
		}
		legacyQueue := hookBeadsFromBeads(legacyControl)

		// Journal frontier: the REAL city-resolved ControlFrontier over the journal
		// leg, serving the migrated + born-journal roots' ready control steps.
		ctlAgent := config.Agent{Name: config.ControlDispatcherAgentName}
		journalFrontier := makeJournalFrontierFn(cityPath, ctlAgent, config.BeadsConfig{}, "", nil)

		// Prove the two frontiers are id-disjoint (residence guarantees it): no
		// double-serve is even possible from an overlap.
		jRows, jOpted, jErr := journalFrontier()
		if jErr != nil || !jOpted {
			t.Fatalf("journal frontier: opted=%t err=%v", jOpted, jErr)
		}
		if frontiersShareAnyID(legacyQueue, jRows) {
			t.Fatalf("legacy and journal frontiers share an id (double-serve risk): legacy=%v journal=%v", ids(legacyQueue), ids(jRows))
		}

		merged, err := composeWorkflowServeQueue(ctlAgent, cityPath, legacyQueue, journalFrontier, io.Discard)
		if err != nil {
			t.Fatalf("composeWorkflowServeQueue: %v", err)
		}
		set := idSet(t, ids(merged)) // fails on any duplicate

		// Each root's READY control step is served exactly once; no root is missing.
		for _, id := range []string{legacyRoot.ready, migrated.ready, born.ready} {
			if !set[id] {
				t.Fatalf("merged serve queue missing ready control bead %s: %v", id, ids(merged))
			}
		}
		// Blocked steps are served by neither leg.
		for _, id := range []string{legacyRoot.blocked, migrated.blocked, born.blocked} {
			if set[id] {
				t.Fatalf("merged serve queue served a blocked control bead %s: %v", id, ids(merged))
			}
		}
		if len(merged) != 3 {
			t.Fatalf("merged serve queue = %v; want exactly the 3 ready control beads", ids(merged))
		}
	})

	// ========================================================== NO DOUBLE PROJECTION
	t.Run("no-double-projection-overlay", func(t *testing.T) {
		// The overlay contract workflowStores/buildWorkflowRunProjections rely on:
		// the router reports it overlays its legacy (city) leg, so the API projection
		// and delete loops DROP the redundant city-store entry (graphOverlaysCity).
		// (buildWorkflowRunProjections itself is package `api`; its overlay-drop is
		// unit-covered by internal/api/graph_overlay_projection_test.go. Here we pin
		// the exact StoreOverlaps predicate + the router's own fan-out dedupe under
		// the REAL three-residence mix that feeds it.)
		if !beads.StoreOverlaps(router, legacy) {
			t.Fatal("StoreOverlaps(router, legacy) = false; the API would keep a duplicate city entry")
		}
		if beads.StoreOverlaps(router, journalStore) {
			t.Fatal("StoreOverlaps(router, journal) = true; the journal leg is not a separate overlaid entry")
		}

		// With the city entry dropped, the router is the SINGLE graph store the
		// projection iterates. Its fan-out over both legs must surface each of the
		// three workflow roots exactly once — even though the migrated root exists on
		// BOTH legs (journal open + legacy tombstone), the residence-aware dedupe
		// collapses it to one, so no run appears twice.
		rows, err := router.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
		if err != nil {
			t.Fatalf("router.List: %v", err)
		}
		var roots []string
		for _, b := range rows {
			if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
				roots = append(roots, b.ID)
			}
		}
		set := idSet(t, roots) // fails on any duplicate (a double-projected root)
		for name, r := range all {
			if !set[r.root] {
				t.Fatalf("%s root %s missing from the projection scan: %v", name, r.root, roots)
			}
		}
		if len(roots) != 3 {
			t.Fatalf("projection scan surfaced %d workflow roots; want exactly 3 (one per residence): %v", len(roots), roots)
		}
	})

	// =============================================================== CITYSTATUS COUNT
	t.Run("citystatus-open-count", func(t *testing.T) {
		// A gc citystatus-style aggregate (open beads across the residence mix) must
		// count each root once: the migrated root's closed legacy tombstone must not
		// inflate the count, and the journal-open copy must not be missed.
		rows, err := router.List(beads.ListQuery{AllowScan: true}) // open only
		if err != nil {
			t.Fatalf("router.List(open): %v", err)
		}
		openRoots := map[string]int{}
		for _, b := range rows {
			if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWorkflow {
				openRoots[b.ID]++
			}
		}
		for name, r := range all {
			if openRoots[r.root] != 1 {
				t.Fatalf("%s root %s counted %d times in the open aggregate; want exactly 1", name, r.root, openRoots[r.root])
			}
		}
		if len(openRoots) != 3 {
			t.Fatalf("open aggregate counted %d roots; want 3", len(openRoots))
		}
	})

	// ============================================================= HOOK/CLAIM CONTROL
	t.Run("control-claim-routes-journal", func(t *testing.T) {
		// A control bead on a journal-resident root is claimable via the control path:
		// controlStoreForBead, under serve mode on an opted city, routes it to the
		// journal leg that actually holds it (a Get-hit returns the journal store).
		// WORKER-bead claim over a journal root is P4 (journal work beads are
		// worker-invisible until then), so this asserts only the provable-now control
		// routing.
		for _, id := range []string{migrated.ready, born.ready} {
			store, err := controlStoreForBead(cityPath, cityPath, &config.City{}, id)
			if err != nil {
				t.Fatalf("controlStoreForBead(%s): %v", id, err)
			}
			got, err := store.Get(id)
			if err != nil || got.ID != id {
				t.Fatalf("control store for %s did not resolve the journal-resident bead: %+v err=%v", id, got, err)
			}
		}
	})

	// ============================================================ DELETE SINGLE-VISIT
	// MUTATES state (closes everything) — must run last.
	t.Run("delete-single-visit", func(t *testing.T) {
		// The API workflow-delete loop iterates workflowStores(state); because the
		// router overlays the city store, that list is just [router] (city dropped),
		// so each root is visited once. router.CloseAll then partitions ids by
		// residence — each id closed on exactly its owning leg, never both.
		var ids []string
		for _, r := range all {
			ids = append(ids, r.root, r.ready, r.blocked)
		}
		// Count how many are still open before the close (migrated legacy copies are
		// already tombstoned; their ids route to the journal-open copies).
		wantClosed := 0
		for _, id := range ids {
			b, err := router.Get(id)
			if err != nil {
				t.Fatalf("pre-delete Get(%s): %v", id, err)
			}
			if b.Status != "closed" {
				wantClosed++
			}
		}
		legacy.calls = nil
		total, err := router.CloseAll(ids, map[string]string{"gc.close_reason": "p3.4-sweep-delete"})
		if err != nil {
			t.Fatalf("router.CloseAll: %v", err)
		}
		if total != wantClosed {
			t.Fatalf("CloseAll closed %d beads; want %d (each open bead closed exactly once)", total, wantClosed)
		}
		// Single-visit: the legacy leg's CloseAll was invoked exactly once by the
		// single overlay store (not a second time from a separate city entry).
		legacyCloseAlls := 0
		for _, c := range legacy.calls {
			if c == "CloseAll" {
				legacyCloseAlls++
			}
		}
		if legacyCloseAlls != 1 {
			t.Fatalf("legacy leg CloseAll called %d times; want exactly 1 (single visit across the overlay)", legacyCloseAlls)
		}
		// Every root ends closed exactly once, resolved through the router.
		for name, r := range all {
			b, err := router.Get(r.root)
			if err != nil || b.Status != "closed" {
				t.Fatalf("%s root %s not closed after delete: %+v err=%v", name, r.root, b, err)
			}
		}
	})
}

// isCrossResidence reports whether err is the router's cross-residence rejection.
func isCrossResidence(err error) bool {
	return errors.Is(err, errCrossResidenceDependency)
}
