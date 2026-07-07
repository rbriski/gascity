//go:build integration

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

// This is the E3 integration tier (§6): the in-place domain/infra store
// migration exercised end-to-end against a live, gc-MANAGED Dolt sql-server. It
// stands up a COMINGLED single-store city (no infra scope, no GC_INFRA_STORE_SPLIT
// at seed time), seeds a mixed population of infra-class and work-class beads in
// the WORK store, runs doMigrateInfraStore, and asserts the boundary holds, ids
// are stable, cross-store dependencies are handled, counts reconcile, a re-run is
// a no-op, a crash-resume converges, and --dry-run makes zero writes.
//
// It rides the same managed-Dolt harness the passing cmd/gc process tests use
// (setupManagedBdWaitTestCity), so it is gated behind GC_FAST_UNIT=0 and skips —
// never falsely fails — on a machine without a working bd/dolt toolchain.

func TestInfraStoreMigrateIntegration(t *testing.T) {
	// Stand up a comingled single-store managed-Dolt city (city hq scope + fe rig
	// scope). Deliberately DO NOT set GC_INFRA_STORE_SPLIT and DO NOT seed the
	// infra scope: this is the pre-split state the migration upgrades.
	cityPath, rigPath := setupManagedBdWaitTestCity(t)
	_ = rigPath

	if cityHasInfraStore(cityPath) {
		t.Fatal("comingled city unexpectedly already has an infra scope")
	}
	if !cityNeedsInfraStoreMigration(cityPath) {
		t.Fatal("cityNeedsInfraStoreMigration = false on a comingled single-store city; it must report needs")
	}

	// ── Seed a mixed population in the WORK store (comingled state) ──
	workStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	seeded := seedComingledMigrationPopulation(t, workStore)
	if err := closeBeadStoreHandle(workStore); err != nil {
		t.Fatalf("close work store after seed: %v", err)
	}

	// ── Dry run first: it must make ZERO writes and still report the plan ──
	dry, err := doMigrateInfraStore(cityPath, true, testWriter{t})
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if !dry.DryRun {
		t.Error("dry-run ledger.DryRun = false")
	}
	if dry.Moved != len(seeded.infraIDs) {
		t.Errorf("dry-run planned moved = %d, want %d", dry.Moved, len(seeded.infraIDs))
	}
	if dry.Deleted != 0 {
		t.Errorf("dry-run deleted = %d, want 0", dry.Deleted)
	}
	// The dry run must not have created the infra scope (zero writes).
	if cityHasInfraStore(cityPath) {
		t.Fatal("dry-run created the infra scope; it must make no writes")
	}

	// ── The real migration ──
	ledger, err := doMigrateInfraStore(cityPath, false, testWriter{t})
	if err != nil {
		t.Fatalf("migration: %v", err)
	}
	if !cityHasInfraStore(cityPath) {
		t.Fatal("cityHasInfraStore is false after migration")
	}
	if ledger.Moved != len(seeded.infraIDs) {
		t.Errorf("moved = %d, want %d", ledger.Moved, len(seeded.infraIDs))
	}
	if ledger.Deleted != len(seeded.infraIDs) {
		t.Errorf("deleted = %d, want %d", ledger.Deleted, len(seeded.infraIDs))
	}

	// Re-open both stores through the production path for assertions.
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("re-open work store: %v", err)
	}
	defer func() { _ = closeBeadStoreHandle(work) }()
	infra, present, err := openCityInfraStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open infra store: %v", err)
	}
	if !present || infra == nil {
		t.Fatal("infra store not present after migration")
	}
	defer func() { _ = closeBeadStoreHandle(infra) }()

	// ── Boundary invariant (the authoritative gate), reusing the fast-tier helper ──
	assertStoreClassBoundary(t, "domain:hq", work, false)
	assertStoreClassBoundary(t, "infra", infra, true)

	// ── Id stability: every moved id Gets from the infra store with its exact id ──
	for _, id := range seeded.infraIDs {
		got, err := infra.Get(id)
		if err != nil {
			t.Errorf("moved bead %q not found in infra store: %v", id, err)
			continue
		}
		if got.ID != id {
			t.Errorf("moved bead re-minted: %q → %q", id, got.ID)
		}
	}
	// Work beads stayed.
	for _, id := range seeded.workIDs {
		if _, err := work.Get(id); err != nil {
			t.Errorf("work bead %q missing after migration: %v", id, err)
		}
	}

	// ── Cross-store dependency: the moved graph child's edge to the moved root
	// lives on the infra store now (intra-set edge). ──
	if seeded.graphChildID != "" && seeded.graphRootID != "" {
		assertHasDownEdge(t, infra, seeded.graphChildID, seeded.graphRootID, "graph child→root on infra store")
	}

	// ── Orphan-preserving delete proof (the batch path is NOT the single-id
	// mutation bomb). Two observable facts on real bd:
	//
	//   1. The migration COMPLETED even though a staying work bead depends on a
	//      moved bead. bd's batch delete ORPHANS that external dependent instead of
	//      refusing; a plain non-force single-id delete of a bead with an external
	//      dependent hard-fails. Reaching this point is that proof.
	//   2. The staying neighbor's free-text fields are NOT rewritten to
	//      "[deleted:<id>]". The single-id `bd delete` path text-rewrites every
	//      connected bead's description/notes (delete.go:174-195) — the real
	//      mutation bomb; the batch path performs no text rewrite. We seeded the
	//      staying task's description with the moved root's id and assert it is
	//      preserved verbatim.
	//
	// NOTE (design deviation, see E3-MIGRATION-DESIGN §3/§7): the inbound dependency
	// ROW does NOT survive on real bd. Migration 0043 added
	// `fk_dep_issue_target FOREIGN KEY (depends_on_issue_id) REFERENCES issues(id)
	// ON DELETE CASCADE`, so deleting the moved bead cascade-drops the inbound edge
	// from the staying bead. That is the E2-native cross-boundary shape (design risk
	// #2: the edge stops blocking) and is bd-schema behavior, not something the batch
	// delete can avoid. The MemStore fast tier still pins the row-preserving contract
	// for backends without that cascade.
	if seeded.stayingTaskID != "" && seeded.graphRootID != "" {
		if _, err := work.Get(seeded.graphRootID); err == nil {
			t.Errorf("moved root %q still present in work store; it should have been deleted", seeded.graphRootID)
		}
		got, err := work.Get(seeded.stayingTaskID)
		if err != nil {
			t.Fatalf("get staying task: %v", err)
		}
		if strings.Contains(got.Description, "[deleted:") {
			t.Errorf("orphan-preserving delete failed: staying bead %q description was text-rewritten by the "+
				"single-id mutation bomb: %q", seeded.stayingTaskID, got.Description)
		}
		if !strings.Contains(got.Description, seeded.graphRootID) {
			t.Errorf("staying bead %q description no longer references the moved root id %q (want it preserved verbatim): %q",
				seeded.stayingTaskID, seeded.graphRootID, got.Description)
		}
	}

	// ── Count reconciliation ──
	if ledger.InfraAfter != ledger.InfraBefore+ledger.Moved {
		t.Errorf("infra_after = %d, want %d", ledger.InfraAfter, ledger.InfraBefore+ledger.Moved)
	}
	if got, want := ledger.WorkAfter["city"], ledger.WorkBefore["city"]-countMovedInStore(seeded, "city"); got != want {
		t.Errorf("work_after[city] = %d, want %d", got, want)
	}

	// ── Re-run is a convergent no-op ──
	rerun, err := doMigrateInfraStore(cityPath, false, testWriter{t})
	if err != nil {
		t.Fatalf("re-run migration: %v", err)
	}
	if rerun.Moved != 0 || rerun.Deleted != 0 {
		t.Errorf("re-run not a no-op: moved=%d deleted=%d", rerun.Moved, rerun.Deleted)
	}
	if cityNeedsInfraStoreMigration(cityPath) {
		t.Error("cityNeedsInfraStoreMigration still true after a completed migration")
	}
}

// TestInfraStoreMigrateCrashResumeIntegration seeds a comingled city, hand-copies
// one infra bead into the infra store (the crash-between-copy-and-delete state),
// then runs the migration and asserts it converges with no duplicate.
func TestInfraStoreMigrateCrashResumeIntegration(t *testing.T) {
	cityPath, _ := setupManagedBdWaitTestCity(t)

	workStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	seeded := seedComingledMigrationPopulation(t, workStore)
	preID := seeded.infraIDs[0]
	src, err := workStore.Get(preID)
	if err != nil {
		t.Fatalf("get pre-copy source: %v", err)
	}
	if err := closeBeadStoreHandle(workStore); err != nil {
		t.Fatalf("close work store: %v", err)
	}

	// Create the infra scope + database WITHOUT running the migration, then
	// hand-copy the one bead so it exists in BOTH stores.
	cfg, err := loadCityConfig(cityPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	resolveRigPaths(cityPath, cfg.Rigs)
	if err := initDirIfReadyEnsureBeadsProvider(cityPath); err != nil {
		t.Fatalf("ensure provider: %v", err)
	}
	if err := initDirIfReadyWaitForManagedDolt(cityPath, managedDoltInitReadyTimeout); err != nil {
		t.Fatalf("wait managed dolt: %v", err)
	}
	if err := ensureInfraScopeForMigration(cityPath, cfg); err != nil {
		t.Fatalf("ensure infra scope: %v", err)
	}
	infra, present, err := openCityInfraStoreAt(cityPath)
	if err != nil || !present {
		t.Fatalf("open infra store: err=%v present=%v", err, present)
	}
	if err := copyBeadPreservingID(infra, src); err != nil {
		t.Fatalf("hand pre-copy: %v", err)
	}
	_ = closeBeadStoreHandle(infra)

	// Now run the migration: it must skip the pre-copied bead's copy but still
	// delete it, converging with no duplicate.
	ledger, err := doMigrateInfraStore(cityPath, false, testWriter{t})
	if err != nil {
		t.Fatalf("resume migration: %v", err)
	}
	if ledger.AlreadyPresent < 1 {
		t.Errorf("already_present = %d, want >= 1", ledger.AlreadyPresent)
	}

	infra2, _, err := openCityInfraStoreAt(cityPath)
	if err != nil {
		t.Fatalf("re-open infra store: %v", err)
	}
	defer func() { _ = closeBeadStoreHandle(infra2) }()
	if n := countIDInStore(t, infra2, preID); n != 1 {
		t.Errorf("pre-copied bead appears %d times in infra store, want 1", n)
	}
	assertStoreClassBoundary(t, "infra", infra2, true)

	work2, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("re-open work store: %v", err)
	}
	defer func() { _ = closeBeadStoreHandle(work2) }()
	assertStoreClassBoundary(t, "domain:hq", work2, false)
}

// seededMigrationPopulation records ids by class + the notable cross-store beads
// so the assertions can reason about the migration.
type seededMigrationPopulation struct {
	infraIDs      []string
	workIDs       []string
	graphRootID   string
	graphChildID  string
	stayingTaskID string
}

func countMovedInStore(s seededMigrationPopulation, _ string) int {
	// All infra beads were seeded in the city/hq work store in this fixture.
	return len(s.infraIDs)
}

// seedComingledMigrationPopulation seeds a representative mix of infra-class and
// work-class beads directly in the WORK store, plus the cross-store and inbound
// edges the migration must handle. It returns the recorded ids.
func seedComingledMigrationPopulation(t *testing.T, workStore beads.Store) seededMigrationPopulation {
	t.Helper()
	var pop seededMigrationPopulation

	recordInfra := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := workStore.Create(b)
		if err != nil {
			t.Fatalf("seed infra bead %q: %v", b.Title, err)
		}
		if !coordclass.Classify(created).IsInfrastructure() {
			t.Fatalf("seed bead %q expected infra, classifies work (type=%q labels=%v)", b.Title, created.Type, created.Labels)
		}
		pop.infraIDs = append(pop.infraIDs, created.ID)
		return created
	}
	recordWork := func(b beads.Bead) beads.Bead {
		t.Helper()
		created, err := workStore.Create(b)
		if err != nil {
			t.Fatalf("seed work bead %q: %v", b.Title, err)
		}
		if coordclass.Classify(created).IsInfrastructure() {
			t.Fatalf("seed bead %q expected work, classifies infra", b.Title)
		}
		pop.workIDs = append(pop.workIDs, created.ID)
		return created
	}

	// Work controls.
	task := recordWork(beads.Bead{Title: "real backlog item", Type: "task"})
	pop.stayingTaskID = task.ID
	convoyA := recordWork(beads.Bead{Title: "convoy item A", Type: "task"})
	recordWork(beads.Bead{Title: "user convoy", Type: "convoy", Labels: []string{"tracks:" + convoyA.ID}})

	// Session lifecycle bead via the production session store.
	if _, err := session.NewStore(beads.SessionStore{Store: workStore}).CreateSession(session.CreateSpec{
		Title:     "worker-1",
		AgentName: "worker-1",
		Metadata:  map[string]string{"provider": "tmux", "template": "claude"},
	}); err != nil {
		t.Fatalf("session create: %v", err)
	}
	// Record every session-class bead that just landed (session + any wait).
	recordExistingInfra(t, workStore, &pop)

	// Mail message via the two-store mail provider (single-store here).
	cfg, _ := loadCityConfig(t.TempDir()) // empty cfg; provider only needs the store
	mp := newCityMailProvider(workStore, nil, cfg, "", nil)
	if _, err := mp.Send("human", "worker-1", "hello", "body text"); err != nil {
		t.Fatalf("mail send: %v", err)
	}
	// Nudge shadow bead.
	if _, _, err := ensureQueuedNudgeBead(beads.NudgesStore{Store: workStore},
		newQueuedNudge("worker-1", "please continue", time.Now().UTC())); err != nil {
		t.Fatalf("nudge enqueue: %v", err)
	}
	// Order-tracking run bead.
	if _, err := orders.NewStore(beads.OrdersStore{Store: workStore}).CreateRun("gate-alpha", orders.RunOpts{}); err != nil {
		t.Fatalf("order run: %v", err)
	}
	// Re-scan for the newly minted infra beads (mail/nudge/order) and record any
	// not already tracked.
	recordExistingInfra(t, workStore, &pop)

	// Graph molecule instantiated into the WORK store (comingled explosion).
	res, err := molecule.Instantiate(context.Background(), workStore, migrationGraphRecipe(), molecule.Options{})
	if err != nil {
		t.Fatalf("molecule instantiate: %v", err)
	}
	pop.graphRootID = res.RootID
	if pop.graphRootID == "" {
		t.Fatal("molecule instantiate returned an empty root id")
	}
	recordExistingInfra(t, workStore, &pop)
	// Prefer the mapped child step id; fall back to a metadata scan.
	pop.graphChildID = res.IDMapping["wf.step"]
	if pop.graphChildID == "" {
		pop.graphChildID = findGraphChild(t, workStore, pop.graphRootID)
	}

	// Cross-store edge: staying work task depends on the moved graph root (an
	// inbound edge to a moved bead). On real bd the edge ROW is cascade-dropped when
	// the moved bead is deleted (fk_dep_issue_target ON DELETE CASCADE, migration
	// 0043) — the E2-native cross-boundary shape.
	if err := workStore.DepAdd(task.ID, pop.graphRootID, "blocks"); err != nil {
		t.Fatalf("dep task→root: %v", err)
	}
	// Also reference the moved root's id in the STAYING task's free text. bd's
	// single-id delete path text-rewrites connected beads' descriptions to
	// "[deleted:<id>]"; the batch path does not. This lets the migration assertion
	// prove the batch (non-mutation-bomb) path was taken.
	rootRef := "blocked by " + pop.graphRootID
	if err := workStore.Update(task.ID, beads.UpdateOpts{Description: &rootRef}); err != nil {
		t.Fatalf("set task description referencing root: %v", err)
	}

	// A closed infra bead (exercises the closed-status restore path).
	closed := recordInfra(beads.Bead{
		Title: "retired session", Type: session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-old", "close_reason": "retired ahead of the migration copy-status test"},
	})
	if err := workStore.Close(closed.ID); err != nil {
		t.Fatalf("close infra bead: %v", err)
	}

	return pop
}

// recordExistingInfra scans the work store and appends any infra-class bead id not
// already recorded to pop.infraIDs. Used after each production creator so beads
// minted internally (waits, mail, order wisps) are all captured.
func recordExistingInfra(t *testing.T, store beads.Store, pop *seededMigrationPopulation) {
	t.Helper()
	known := make(map[string]struct{}, len(pop.infraIDs)+len(pop.workIDs))
	for _, id := range pop.infraIDs {
		known[id] = struct{}{}
	}
	for _, id := range pop.workIDs {
		known[id] = struct{}{}
	}
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("scan for infra beads: %v", err)
	}
	for _, b := range list {
		if _, seen := known[b.ID]; seen {
			continue
		}
		if coordclass.Classify(b).IsInfrastructure() {
			pop.infraIDs = append(pop.infraIDs, b.ID)
		} else {
			pop.workIDs = append(pop.workIDs, b.ID)
		}
	}
}

func findGraphChild(t *testing.T, store beads.Store, rootID string) string {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range list {
		if b.ID == rootID {
			continue
		}
		if b.Metadata[beadmeta.RootBeadIDMetadataKey] == rootID {
			return b.ID
		}
	}
	return ""
}

func assertHasDownEdge(t *testing.T, store beads.Store, issueID, dependsOnID, what string) {
	t.Helper()
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		t.Fatalf("DepList(%q) for %s: %v", issueID, what, err)
	}
	for _, d := range deps {
		if d.DependsOnID == dependsOnID {
			return
		}
	}
	t.Errorf("%s: missing edge %s→%s (got %v)", what, issueID, dependsOnID, deps)
}

func countIDInStore(t *testing.T, store beads.Store, id string) int {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	n := 0
	for _, b := range list {
		if b.ID == id {
			n++
		}
	}
	return n
}

// migrationGraphRecipe is a minimal formula recipe that materializes a graph
// molecule: a workflow root (gc.kind=workflow → ClassGraph) plus a child step
// inheriting gc.root_bead_id → ClassGraph.
func migrationGraphRecipe() *formula.Recipe {
	return &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{ID: "wf", Title: "Workflow", Type: "task", IsRoot: true, Metadata: map[string]string{"gc.kind": "workflow"}},
			{ID: "wf.step", Title: "Work", Type: "task"},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.step", DependsOnID: "wf", Type: "parent-child"},
		},
	}
}

// testWriter adapts *testing.T to an io.Writer so migration stderr flows into the
// test log.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("migrate: %s", p)
	return len(p), nil
}
