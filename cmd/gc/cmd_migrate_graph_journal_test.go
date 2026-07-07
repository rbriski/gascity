package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// migrateTestEnv wires a real JournalStore journal leg (which exposes the
// residence-migration capability) against an in-memory legacy leg, plus the
// migrateGraphJournalDeps the state machine drives.
type migrateTestEnv struct {
	journal *beads.JournalStore
	legacy  *beads.MemStore
	deps    migrateGraphJournalDeps
}

func newMigrateTestEnv(t *testing.T) *migrateTestEnv {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "mig-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	j := beads.NewJournalStore(gs)
	mem := beads.NewMemStore()
	return &migrateTestEnv{
		journal: j,
		legacy:  mem,
		deps:    migrateGraphJournalDeps{legacy: mem, journal: j, res: j, now: time.Now},
	}
}

// migrateTestCand is the assignee every seeded molecule step carries.
const migrateTestCand = "cand"

// seedMolecule plants a legacy control molecule: a root plus stepA (ready) and
// stepB (blocked-by-stepA), both control-dispatcher steps (gc.kind=check) routed
// to migrateTestCand. They are control-class, not worker-class, so migration
// moves them; a genuine worker step (no control kind) is what the
// refuse-open-claimable guard trips on. Returns their ids.
func (e *migrateTestEnv) seedMolecule(t *testing.T) (root, stepA, stepB string) {
	t.Helper()
	r, err := e.legacy.Create(beads.Bead{Title: "root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	controlKind := map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck}
	a, err := e.legacy.Create(beads.Bead{Title: "step-a", Type: "task", Assignee: migrateTestCand, ParentID: r.ID, Metadata: controlKind})
	if err != nil {
		t.Fatalf("create step-a: %v", err)
	}
	b, err := e.legacy.Create(beads.Bead{Title: "step-b", Type: "task", Assignee: migrateTestCand, ParentID: r.ID, Metadata: controlKind})
	if err != nil {
		t.Fatalf("create step-b: %v", err)
	}
	if err := e.legacy.DepAdd(a.ID, r.ID, "parent-child"); err != nil {
		t.Fatalf("dep a->root: %v", err)
	}
	if err := e.legacy.DepAdd(b.ID, r.ID, "parent-child"); err != nil {
		t.Fatalf("dep b->root: %v", err)
	}
	if err := e.legacy.DepAdd(b.ID, a.ID, "blocks"); err != nil {
		t.Fatalf("dep b blocks a: %v", err)
	}
	return r.ID, a.ID, b.ID
}

func (e *migrateTestEnv) residence(t *testing.T, root string) (string, bool) {
	t.Helper()
	state, _, present, err := e.journal.ResidenceOf(context.Background(), root)
	if err != nil {
		t.Fatalf("ResidenceOf: %v", err)
	}
	return state, present
}

// --- init arm --------------------------------------------------------------

// TestMigrateGraphJournalInitIdempotent pins that init opts a city in and is
// safe to re-run.
func TestMigrateGraphJournalInitIdempotent(t *testing.T) {
	city := t.TempDir()
	if cityHasGraphScope(city) {
		t.Fatalf("fresh city already opted in")
	}
	if err := migrateGraphJournalInit(city); err != nil {
		t.Fatalf("init: %v", err)
	}
	if !cityHasGraphScope(city) {
		t.Fatalf("city not opted in after init")
	}
	if _, err := os.Stat(filepath.Join(graphScopeRoot(city), "journal.db")); err != nil {
		t.Fatalf("journal.db not created: %v", err)
	}
	// Re-run: no error, still opted.
	if err := migrateGraphJournalInit(city); err != nil {
		t.Fatalf("init re-run: %v", err)
	}
	if !cityHasGraphScope(city) {
		t.Fatalf("city lost opt-in on re-run")
	}
}

// --- happy path ------------------------------------------------------------

// TestMigrateRootHappyPathTransitionsAndPreservesIDs pins the residence
// transitions ∅→journal, id preservation, and legacy tombstoning.
func TestMigrateRootHappyPathTransitionsAndPreservesIDs(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, stepB := e.seedMolecule(t)

	if _, present := e.residence(t, root); present {
		t.Fatalf("root has a residence record before migration; want ∅")
	}

	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err != nil {
		t.Fatalf("migrateRoot: %v", err)
	}
	if res.Outcome != migrateOutcomeCutover || !res.Tombstoned {
		t.Fatalf("outcome = %+v; want cutover+tombstoned", res)
	}
	if state, present := e.residence(t, root); !present || state != beads.ResidenceStateJournal {
		t.Fatalf("residence = %q present=%t; want journal", state, present)
	}

	// Journal leg now holds the migrated beads with PRESERVED ids.
	for _, id := range []string{root, stepA, stepB} {
		got, err := e.journal.Get(id)
		if err != nil {
			t.Fatalf("journal Get(%s): %v", id, err)
		}
		if got.ID != id {
			t.Fatalf("id not preserved: got %q want %q", got.ID, id)
		}
	}

	// Legacy copy tombstoned (closed + gc.migrated=1), never deleted.
	legRoot, err := e.legacy.Get(root)
	if err != nil {
		t.Fatalf("legacy root gone (deleted, not tombstoned): %v", err)
	}
	if legRoot.Status != "closed" {
		t.Fatalf("legacy root status = %q; want closed", legRoot.Status)
	}
	if legRoot.Metadata["gc.migrated"] != "1" {
		t.Fatalf("legacy root missing gc.migrated=1: %v", legRoot.Metadata)
	}
}

// TestMigrateRootRefusesOpenClaimableWorker pins the refuse-open-claimable guard
// using the REAL claim signal: an OPEN, worker-class bead that is assigned. The
// dead claimed/assigned statuses are gone, and a control step assigned to the
// dispatcher does NOT trip the guard — only a genuine worker step does.
func TestMigrateRootRefusesOpenClaimableWorker(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)
	// A genuine worker step (no control kind): open + assigned = a held claim.
	w, err := e.legacy.Create(beads.Bead{Title: "worker-step", Type: "task", Assignee: "coder", ParentID: root})
	if err != nil {
		t.Fatalf("create worker step: %v", err)
	}
	if err := e.legacy.DepAdd(w.ID, root, "parent-child"); err != nil {
		t.Fatalf("dep worker->root: %v", err)
	}

	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err == nil {
		t.Fatalf("migrateRoot succeeded on a root with an open+assigned worker step; want refusal")
	}
	if res.Outcome != migrateOutcomeRefused {
		t.Fatalf("outcome = %q; want refused", res.Outcome)
	}
	if _, present := e.residence(t, root); present {
		t.Fatalf("refused root left a residence record; want ∅")
	}
	if staged, _ := e.journal.StagedRootBeads(ctx, root); len(staged) != 0 {
		t.Fatalf("refused root left %d staged beads; want 0", len(staged))
	}
}

// TestMigrateRootRefusesOpenRoutedWorker pins that the routed arm of the real
// claim signal (open + gc.routed_to, no assignee) also refuses.
func TestMigrateRootRefusesOpenRoutedWorker(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)
	w, err := e.legacy.Create(beads.Bead{
		Title:    "routed-worker",
		Type:     "task",
		ParentID: root,
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "pool-x"},
	})
	if err != nil {
		t.Fatalf("create routed worker: %v", err)
	}
	if err := e.legacy.DepAdd(w.ID, root, "parent-child"); err != nil {
		t.Fatalf("dep worker->root: %v", err)
	}
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err == nil || res.Outcome != migrateOutcomeRefused {
		t.Fatalf("outcome = %q err=%v; want refused for an open routed worker", res.Outcome, err)
	}
}

// TestMigrateRootAllowsControlStepAssigned pins the other side of HIGH-2: a
// control step that is open+assigned to the dispatcher is NOT worker-class, so it
// does not block migration (the seed molecule migrates cleanly).
func TestMigrateRootAllowsControlStepAssigned(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t) // steps are open + assigned + gc.kind=check
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err != nil {
		t.Fatalf("migrate control molecule: %v", err)
	}
	if res.Outcome != migrateOutcomeCutover {
		t.Fatalf("outcome = %q; want cutover (control steps are not worker-claimable)", res.Outcome)
	}
}

// TestMigrateRootReRunNoOp pins convergence: a second migrate is an idempotent
// no-op that resumes the tombstone.
func TestMigrateRootReRunNoOp(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)

	if _, err := migrateRoot(ctx, e.deps, root, false, false); err != nil {
		t.Fatalf("first migrate: %v", err)
	}
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if res.Outcome != migrateOutcomeAlready {
		t.Fatalf("re-run outcome = %q; want already", res.Outcome)
	}
	if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
		t.Fatalf("residence drifted on re-run: %q", state)
	}
}

// TestMigrateRootDryRunZeroWrites pins that --dry-run writes nothing.
func TestMigrateRootDryRunZeroWrites(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)

	res, err := migrateRoot(ctx, e.deps, root, true, false)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if res.Outcome != migrateOutcomeDryRun || res.FoldHash == "" {
		t.Fatalf("dry-run result = %+v; want dry-run with a fold hash", res)
	}
	// No residence record, no staged copy, legacy root still open.
	if _, present := e.residence(t, root); present {
		t.Fatalf("dry-run wrote a residence record")
	}
	if staged, _ := e.journal.StagedRootBeads(ctx, root); len(staged) != 0 {
		t.Fatalf("dry-run wrote %d staged beads; want 0", len(staged))
	}
	legRoot, _ := e.legacy.Get(root)
	if legRoot.Status == "closed" {
		t.Fatalf("dry-run tombstoned the legacy root")
	}
}

// --- crash injection -------------------------------------------------------

// TestMigrateRootCrashResume injects a failure at each step boundary and asserts
// the next run converges to exactly one authoritative journal copy with the
// legacy leg tombstoned — no data loss at any crash point.
func TestMigrateRootCrashResume(t *testing.T) {
	ctx := context.Background()
	crashErr := errors.New("injected crash")

	points := []struct {
		name  string
		hooks func() *migrateRootHooks
	}{
		{"afterPark", func() *migrateRootHooks { return &migrateRootHooks{afterPark: func() error { return crashErr }} }},
		{"afterCopy", func() *migrateRootHooks { return &migrateRootHooks{afterCopy: func() error { return crashErr }} }},
		{"afterFoldVerify", func() *migrateRootHooks { return &migrateRootHooks{afterFoldVerify: func() error { return crashErr }} }},
		{"afterReVerify", func() *migrateRootHooks { return &migrateRootHooks{afterReVerify: func() error { return crashErr }} }},
		{"afterFlip", func() *migrateRootHooks { return &migrateRootHooks{afterFlip: func() error { return crashErr }} }},
	}

	for _, pt := range points {
		t.Run(pt.name, func(t *testing.T) {
			e := newMigrateTestEnv(t)
			root, stepA, stepB := e.seedMolecule(t)

			// First run crashes at the injected boundary.
			e.deps.hooks = pt.hooks()
			if _, err := migrateRoot(ctx, e.deps, root, false, false); !errors.Is(err, crashErr) {
				t.Fatalf("%s: expected injected crash, got %v", pt.name, err)
			}

			// Recovery run (no hooks) converges. A crash that left the root
			// `migrating` requires --force-recover to reclaim the stale epoch (the
			// engine cannot tell a crashed owner from a live one); a post-flip crash
			// resumes at the tombstone without it. force-recover is harmless in the
			// journal-state case, so the recovery run passes it uniformly.
			e.deps.hooks = nil
			res, err := migrateRoot(ctx, e.deps, root, false, true)
			if err != nil {
				t.Fatalf("%s: recovery run: %v", pt.name, err)
			}
			if res.Outcome != migrateOutcomeCutover && res.Outcome != migrateOutcomeAlready {
				t.Fatalf("%s: recovery outcome = %q; want cutover/already", pt.name, res.Outcome)
			}
			if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
				t.Fatalf("%s: residence = %q; want journal", pt.name, state)
			}
			// Exactly one authoritative copy: all ids present journal-side.
			for _, id := range []string{root, stepA, stepB} {
				if _, err := e.journal.Get(id); err != nil {
					t.Fatalf("%s: journal missing %s after recovery: %v", pt.name, id, err)
				}
			}
			// Legacy tombstoned.
			legRoot, err := e.legacy.Get(root)
			if err != nil || legRoot.Status != "closed" || legRoot.Metadata["gc.migrated"] != "1" {
				t.Fatalf("%s: legacy root not tombstoned: %+v err=%v", pt.name, legRoot, err)
			}
		})
	}
}

// --- concurrent old-leg write ----------------------------------------------

// TestStrandMigrationConcurrentOldLegWriteBlockedOrDetected is 09a §A-2's
// three-armed fence test.
func TestStrandMigrationConcurrentOldLegWriteBlockedOrDetected(t *testing.T) {
	ctx := context.Background()

	// Arm (a): a controller-path write through the router during `migrating`
	// returns ErrRootMigrating.
	t.Run("controller-write-blocked", func(t *testing.T) {
		e := newMigrateTestEnv(t)
		root, _, _ := e.seedMolecule(t)
		router := newResidenceRoutingGraphStore(e.journal, e.legacy)

		if _, err := e.journal.BeginResidenceMigration(ctx, root, 1); err != nil {
			t.Fatalf("park: %v", err)
		}
		err := router.Update(root, beads.UpdateOpts{Title: strptr("racing controller write")})
		if !errors.Is(err, beads.ErrRootMigrating) {
			t.Fatalf("router.Update during migration = %v; want ErrRootMigrating", err)
		}
		// A write to an UNRELATED, non-migrating bead is not blocked.
		other, _ := e.legacy.Create(beads.Bead{Title: "unrelated"})
		if err := router.Update(other.ID, beads.UpdateOpts{Title: strptr("ok")}); err != nil {
			t.Fatalf("router.Update on a non-migrating bead = %v; want nil", err)
		}
	})

	// Arm (b): an external old-leg write between copy and re-verify aborts the
	// migration (no tombstone); a re-run converges with the late write included.
	t.Run("external-write-detected-and-reverted", func(t *testing.T) {
		e := newMigrateTestEnv(t)
		root, stepA, _ := e.seedMolecule(t)

		// Inject an external legacy write right after the copy, then let the flow
		// continue so re-verify sees the delta.
		e.deps.hooks = &migrateRootHooks{
			afterCopy: func() error {
				return e.legacy.SetMetadata(stepA, "external.write", "raced")
			},
		}
		res, err := migrateRoot(ctx, e.deps, root, false, false)
		if err != nil {
			t.Fatalf("migrate with raced write: %v", err)
		}
		if res.Outcome != migrateOutcomeRevertedRace || !res.RacedWrite {
			t.Fatalf("outcome = %+v; want reverted-race", res)
		}
		// No tombstone, residence reverted to ∅, staged copy discarded.
		if _, present := e.residence(t, root); present {
			t.Fatalf("raced migration left a residence record; want ∅")
		}
		if staged, _ := e.journal.StagedRootBeads(ctx, root); len(staged) != 0 {
			t.Fatalf("raced migration left %d staged beads; want 0", len(staged))
		}
		legRoot, _ := e.legacy.Get(root)
		if legRoot.Status == "closed" {
			t.Fatalf("raced migration tombstoned the legacy root; must not")
		}

		// Re-run (no hook) converges WITH the late write included.
		e.deps.hooks = nil
		res, err = migrateRoot(ctx, e.deps, root, false, false)
		if err != nil {
			t.Fatalf("re-run after race: %v", err)
		}
		if res.Outcome != migrateOutcomeCutover {
			t.Fatalf("re-run outcome = %q; want cutover", res.Outcome)
		}
		got, err := e.journal.Get(stepA)
		if err != nil {
			t.Fatalf("journal Get(stepA): %v", err)
		}
		if got.Metadata["external.write"] != "raced" {
			t.Fatalf("late external write not included in re-run copy: %v", got.Metadata)
		}
	})

	// Arm (c): a crash between re-verify and flip converges on re-run to exactly
	// one authoritative copy.
	t.Run("crash-between-reverify-and-flip", func(t *testing.T) {
		e := newMigrateTestEnv(t)
		root, stepA, stepB := e.seedMolecule(t)
		boom := errors.New("crash before flip")
		e.deps.hooks = &migrateRootHooks{afterReVerify: func() error { return boom }}
		if _, err := migrateRoot(ctx, e.deps, root, false, false); !errors.Is(err, boom) {
			t.Fatalf("expected crash, got %v", err)
		}
		// The crash left the root migrating; reclaim with --force-recover.
		e.deps.hooks = nil
		if _, err := migrateRoot(ctx, e.deps, root, false, true); err != nil {
			t.Fatalf("recovery: %v", err)
		}
		if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
			t.Fatalf("residence = %q; want journal", state)
		}
		for _, id := range []string{root, stepA, stepB} {
			if _, err := e.journal.Get(id); err != nil {
				t.Fatalf("journal missing %s: %v", id, err)
			}
		}
	})
}

// --- migrated-root frontier parity -----------------------------------------

// TestMigratedRootControlFrontierParity pins that the journal ControlFrontier over
// a migrated root yields the correct ready control set (stepA ready, stepB blocked
// by stepA) — the unit-tier analog of the P3.1 real-bd parity gate.
func TestMigratedRootControlFrontierParity(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, stepB := e.seedMolecule(t)

	if _, err := migrateRoot(ctx, e.deps, root, false, false); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	got, err := e.journal.ControlFrontier(ctx, beads.ControlFrontierParams{
		AssigneeCandidates:       []string{"cand"},
		InstantiatingMetadataKey: "gc.instantiating",
	})
	if err != nil {
		t.Fatalf("ControlFrontier: %v", err)
	}
	var ids []string
	for _, b := range got {
		ids = append(ids, b.ID)
	}
	if len(ids) != 1 || ids[0] != stepA {
		t.Fatalf("ControlFrontier over migrated root = %v; want [%s] (stepB is blocked by stepA)", ids, stepA)
	}
	_ = stepB
}

// --- residence-aware dedupe crash window -----------------------------------

// TestResidenceAwareDedupePrefersJournalInCrashWindow pins Risk 1: in the
// flip→tombstone crash window a root exists in BOTH legs; the router's fan-out
// dedupe must prefer the journal (authoritative) row over the stale legacy one.
func TestResidenceAwareDedupePrefersJournalInCrashWindow(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)

	// Drive the migration to a flip-crash: residence=journal, legacy NOT yet
	// tombstoned (both legs hold the root).
	e.deps.hooks = &migrateRootHooks{afterFlip: func() error { return errors.New("crash after flip") }}
	if _, err := migrateRoot(ctx, e.deps, root, false, false); err == nil {
		t.Fatalf("expected flip crash")
	}
	if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
		t.Fatalf("precondition: residence = %q; want journal", state)
	}
	legRoot, _ := e.legacy.Get(root)
	if legRoot.Status == "closed" {
		t.Fatalf("precondition: legacy root already tombstoned; want the both-legs window")
	}

	// Distinguish the two copies: bump the journal (authoritative) title.
	if err := e.journal.SetMetadata(root, "leg", "journal"); err != nil {
		t.Fatalf("mark journal copy: %v", err)
	}
	if err := e.legacy.SetMetadata(root, "leg", "legacy"); err != nil {
		t.Fatalf("mark legacy copy: %v", err)
	}

	router := newResidenceRoutingGraphStore(e.journal, e.legacy)
	beadsOut, err := router.List(beads.ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("router.List: %v", err)
	}
	var rootCount int
	var served string
	for _, b := range beadsOut {
		if b.ID == root {
			rootCount++
			served = b.Metadata["leg"]
		}
	}
	if rootCount != 1 {
		t.Fatalf("root appears %d times in fan-out; want 1 (deduped)", rootCount)
	}
	if served != "journal" {
		t.Fatalf("dedupe served the %q copy; want journal (authoritative)", served)
	}
}

// --- inert ------------------------------------------------------------------

// TestResidenceRoutingInertWithoutMigrations pins that the P3.2 additions are
// inert when no root is migrating: router writes are unblocked and the empty
// residence table costs nothing observable.
func TestResidenceRoutingInertWithoutMigrations(t *testing.T) {
	e := newMigrateTestEnv(t)
	router := newResidenceRoutingGraphStore(e.journal, e.legacy)

	b, err := e.legacy.Create(beads.Bead{Title: "ordinary"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// No migrations in flight: a legacy write routes and succeeds, unblocked.
	if err := router.Update(b.ID, beads.UpdateOpts{Title: strptr("edited")}); err != nil {
		t.Fatalf("router.Update with empty residence table = %v; want nil (inert)", err)
	}
	got, err := router.Get(b.ID)
	if err != nil || got.Title != "edited" {
		t.Fatalf("router.Get = %+v err=%v; want the edited bead", got, err)
	}
}

// strptr returns a pointer to s for UpdateOpts fields.
func strptr(s string) *string { return &s }

// --- BLOCKER-2: concurrent invocation must not destroy the authoritative copy --

// TestMigrateConcurrentSecondInvocationRefusesAndPreserves pins the fence: while
// one migrator holds a root (migrating@epochA, staged rows present), a SECOND
// invocation REFUSES (pointing at --force-recover) and deletes none of the first
// migrator's rows. A subsequent --force-recover then converges to exactly one
// authoritative journal copy. This is the data-destruction guard.
func TestMigrateConcurrentSecondInvocationRefusesAndPreserves(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, stepB := e.seedMolecule(t)

	// Migrator A parks + copies, then stops before flip (proxy for a live A).
	e.deps.hooks = &migrateRootHooks{afterCopy: func() error { return errors.New("A still in flight") }}
	if _, err := migrateRoot(ctx, e.deps, root, false, false); err == nil {
		t.Fatalf("expected A to stop after copy")
	}
	stateA, epochA, present, err := e.journal.ResidenceOf(ctx, root)
	if err != nil || !present || stateA != beads.ResidenceStateMigrating {
		t.Fatalf("precondition: residence=%q present=%t err=%v; want migrating", stateA, present, err)
	}
	stagedA, _ := e.journal.StagedRootBeads(ctx, root)
	if len(stagedA) == 0 {
		t.Fatalf("precondition: migrator A left no staged rows")
	}

	// Migrator B (no force) must REFUSE and stomp nothing.
	e.deps.hooks = nil
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err == nil {
		t.Fatalf("second invocation succeeded over a foreign migrating root; want refusal")
	}
	if res.Outcome != migrateOutcomeMigratingLock {
		t.Fatalf("outcome = %q; want migrating-lock", res.Outcome)
	}
	if !strings.Contains(err.Error(), "--force-recover") {
		t.Fatalf("refusal error missing the --force-recover hint: %v", err)
	}
	stagedAfter, _ := e.journal.StagedRootBeads(ctx, root)
	if len(stagedAfter) != len(stagedA) {
		t.Fatalf("second invocation deleted migrator A's rows: %d -> %d", len(stagedA), len(stagedAfter))
	}
	stateB, epochB, _, _ := e.journal.ResidenceOf(ctx, root)
	if stateB != beads.ResidenceStateMigrating || epochB != epochA {
		t.Fatalf("residence changed under the refusal: state=%q epoch=%d; want migrating@%d", stateB, epochB, epochA)
	}

	// --force-recover reclaims the stale epoch and converges to ONE copy.
	res, err = migrateRoot(ctx, e.deps, root, false, true)
	if err != nil {
		t.Fatalf("force-recover run: %v", err)
	}
	if res.Outcome != migrateOutcomeCutover || !res.Tombstoned {
		t.Fatalf("force-recover outcome = %+v; want cutover+tombstoned", res)
	}
	// Exactly one authoritative copy per id: each resolves journal-side, and the
	// re-import could only have succeeded because force-recover first discarded the
	// stale epoch's rows (a leftover would collide on the id primary key).
	for _, id := range []string{root, stepA, stepB} {
		got, err := e.journal.Get(id)
		if err != nil {
			t.Fatalf("journal missing %s after recovery: %v", id, err)
		}
		if got.ID != id {
			t.Fatalf("journal id drift for %s: got %q", id, got.ID)
		}
	}
	legRoot, _ := e.legacy.Get(root)
	if legRoot.Status != "closed" || legRoot.Metadata["gc.migrated"] != "1" {
		t.Fatalf("legacy not tombstoned after recovery: %+v", legRoot)
	}
}

// --- BLOCKER-1: re-verify→flip external-write window is a loud alarm ----------

// TestMigratePostFlipExternalWriteAlarmsNoTombstone pins that an external write
// landing in the residual re-verify→flip window is DETECTED after the flip and
// converted into a loud alarm (typed error, RacedWrite, no tombstone) rather than
// silently lost or, worse, closing the window-created state.
func TestMigratePostFlipExternalWriteAlarmsNoTombstone(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, _ := e.seedMolecule(t)

	// The write lands after re-verify passed but before the flip completes.
	e.deps.hooks = &migrateRootHooks{
		afterReVerify: func() error { return e.legacy.SetMetadata(stepA, "external.write", "window") },
	}
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if !errors.Is(err, errPostFlipExternalWrite) {
		t.Fatalf("err = %v; want errPostFlipExternalWrite", err)
	}
	if res.Outcome != migrateOutcomePostFlipDelta || !res.RacedWrite {
		t.Fatalf("outcome = %+v; want post-flip-delta + raced", res)
	}
	if res.Tombstoned {
		t.Fatalf("tombstoned despite the post-flip delta alarm")
	}
	// The flip happened (journal authoritative), but legacy is intact for
	// reconciliation: stepA NOT closed, root NOT marked migrated.
	if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
		t.Fatalf("residence = %q; want journal (flip must have completed)", state)
	}
	legStepA, _ := e.legacy.Get(stepA)
	if legStepA.Status == "closed" {
		t.Fatalf("alarm path closed a window bead; must leave legacy intact")
	}
	legRoot, _ := e.legacy.Get(root)
	if legRoot.Metadata["gc.migrated"] == "1" {
		t.Fatalf("alarm path marked legacy migrated; must not tombstone")
	}
	// The failure drives a non-zero exit via the summary.
	summary := migrateGraphJournalSummary{Roots: []migrateRootResult{res}}
	if !summary.hasFailure() {
		t.Fatalf("post-flip alarm did not mark the summary as a failure (would exit 0)")
	}
}

// TestMigrateChildCreateUnderMigratingRootBlocked pins BLOCKER-1(a): a router
// child Create under a migrating root is fenced with ErrRootMigrating, closing the
// gap that let a new step slip onto the old leg mid-copy.
func TestMigrateChildCreateUnderMigratingRootBlocked(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)
	router := newResidenceRoutingGraphStore(e.journal, e.legacy)

	if _, err := e.journal.BeginResidenceMigration(ctx, root, 1); err != nil {
		t.Fatalf("park: %v", err)
	}
	_, err := router.Create(beads.Bead{Title: "late child", ParentID: root})
	if !errors.Is(err, beads.ErrRootMigrating) {
		t.Fatalf("child Create under a migrating root = %v; want ErrRootMigrating", err)
	}
	_, err = router.CreateWithStorage(beads.Bead{Title: "late child 2", ParentID: root}, beads.StorageDefault)
	if !errors.Is(err, beads.ErrRootMigrating) {
		t.Fatalf("child CreateWithStorage under a migrating root = %v; want ErrRootMigrating", err)
	}
}

// --- MEDIUM: cross-root blocking dependents refuse migration ------------------

// TestMigrateRefusesCrossRootBlockingDependent pins that an INBOUND cross-root
// blocking dependency refuses the migration: tombstoning the subtree would
// prematurely unblock an external bead while the journal copy is still open.
func TestMigrateRefusesCrossRootBlockingDependent(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, _ := e.seedMolecule(t)

	ext, err := e.legacy.Create(beads.Bead{Title: "external-dependent", Type: "task"})
	if err != nil {
		t.Fatalf("create external: %v", err)
	}
	// ext (outside the root) blocks-depends on stepA (inside the subtree).
	if err := e.legacy.DepAdd(ext.ID, stepA, "blocks"); err != nil {
		t.Fatalf("dep ext blocks stepA: %v", err)
	}

	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err == nil {
		t.Fatalf("migrate succeeded with a cross-root blocking dependent; want refusal")
	}
	if res.Outcome != migrateOutcomeRefused {
		t.Fatalf("outcome = %q; want refused", res.Outcome)
	}
	if _, present := e.residence(t, root); present {
		t.Fatalf("refused root left a residence record; want ∅")
	}
}

// --- HIGH-1: waits-for gate edge metadata survives migration ------------------

// seedGateMolecule plants a spawner with one active and one closed child, plus a
// waiter that waits-for the spawner behind an any-children gate carried on the
// edge metadata. Returns the ids.
func (e *migrateTestEnv) seedGateMolecule(t *testing.T) (root, spawner, waiter string) {
	t.Helper()
	control := func(title, parent, assignee string) string {
		b, err := e.legacy.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			ParentID: parent,
			Assignee: assignee,
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindCheck},
		})
		if err != nil {
			t.Fatalf("create %s: %v", title, err)
		}
		if parent != "" {
			if err := e.legacy.DepAdd(b.ID, parent, "parent-child"); err != nil {
				t.Fatalf("dep %s->parent: %v", title, err)
			}
		}
		return b.ID
	}
	r, err := e.legacy.Create(beads.Bead{Title: "gate-root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create root: %v", err)
	}
	sp := control("spawner", r.ID, "")
	active := control("child-active", sp, "")
	closed := control("child-closed", sp, "")
	if err := e.legacy.Close(closed); err != nil {
		t.Fatalf("close child: %v", err)
	}
	_ = active
	w := control("waiter", r.ID, migrateTestCand)
	if err := e.legacy.DepAdd(w, sp, "waits-for"); err != nil {
		t.Fatalf("dep waiter waits-for spawner: %v", err)
	}
	if err := e.legacy.SetEdgeMetadata(w, sp, "waits-for", `{"gate":"any-children"}`); err != nil {
		t.Fatalf("set gate metadata: %v", err)
	}
	return r.ID, sp, w
}

// TestMigratePreservesWaitsForGateMetadata pins HIGH-1: the waits-for gate blob is
// copied onto the journal edge AND still honored (any-children early release) by
// the journal ControlFrontier post-migration. If the metadata were dropped the
// waiter would be blocked (an active child, no early release) and absent from the
// frontier.
func TestMigratePreservesWaitsForGateMetadata(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, spawner, waiter := e.seedGateMolecule(t)

	if _, err := migrateRoot(ctx, e.deps, root, false, false); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// The journal edge preserves the gate blob.
	got, err := e.journal.EdgeMetadata(waiter, spawner, "waits-for")
	if err != nil {
		t.Fatalf("journal EdgeMetadata: %v", err)
	}
	if got != `{"gate":"any-children"}` {
		t.Fatalf("journal edge metadata = %q; want the preserved gate", got)
	}
	// The gate is honored: the waiter is early-released, so it is ready.
	frontier, err := e.journal.ControlFrontier(ctx, beads.ControlFrontierParams{
		AssigneeCandidates:       []string{migrateTestCand},
		InstantiatingMetadataKey: "gc.instantiating",
	})
	if err != nil {
		t.Fatalf("ControlFrontier: %v", err)
	}
	var found bool
	for _, b := range frontier {
		if b.ID == waiter {
			found = true
		}
	}
	if !found {
		t.Fatalf("waiter %q absent from frontier; any-children early release was lost with the gate metadata", waiter)
	}
}

// TestMigrateEdgeMetadataChangeCaughtByReVerify pins that a change to edge
// metadata during the copy window is caught by re-verify (the hash includes edge
// metadata), aborting cleanly with no tombstone.
func TestMigrateEdgeMetadataChangeCaughtByReVerify(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, spawner, waiter := e.seedGateMolecule(t)

	e.deps.hooks = &migrateRootHooks{
		afterCopy: func() error {
			return e.legacy.SetEdgeMetadata(waiter, spawner, "waits-for", `{"gate":"all-children"}`)
		},
	}
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if res.Outcome != migrateOutcomeRevertedRace || !res.RacedWrite {
		t.Fatalf("outcome = %+v; want reverted-race (edge-metadata change must be caught)", res)
	}
	if _, present := e.residence(t, root); present {
		t.Fatalf("edge-metadata race left a residence record; want ∅")
	}
}

// --- non-zero exit on refusal ------------------------------------------------

// TestMigrateTombstoneCloseLoopExternalWriteAlarms pins the narrowed BLOCKER-1
// residual: an external modification to an EXISTING legacy bead that lands DURING
// the tombstone close loop (after the pre-close guard passed, before gc.migrated=1)
// is caught by the post-close re-hash — a loud alarm, no tombstone, non-zero exit —
// never silent loss. The write survives on the legacy leg for reconciliation.
func TestMigrateTombstoneCloseLoopExternalWriteAlarms(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, stepA, _ := e.seedMolecule(t)

	// The write races the close loop: it lands after the pre-close guard and the
	// close loop but before the post-close re-hash. It modifies an EXISTING bead
	// (stepA metadata), the detect-not-prevent case.
	e.deps.hooks = &migrateRootHooks{
		duringTombstoneClose: func() error {
			return e.legacy.SetMetadata(stepA, "external.write", "close-window")
		},
	}
	res, err := migrateRoot(ctx, e.deps, root, false, false)
	if !errors.Is(err, errPostFlipExternalWrite) {
		t.Fatalf("err = %v; want errPostFlipExternalWrite", err)
	}
	if res.Outcome != migrateOutcomePostFlipDelta || !res.RacedWrite {
		t.Fatalf("outcome = %+v; want post-flip-delta + raced", res)
	}
	if res.Tombstoned {
		t.Fatalf("tombstoned despite the close-loop external-write alarm")
	}
	// The flip completed (journal authoritative) but the root is NOT marked migrated,
	// so a resume re-checks rather than short-circuiting (no mark-first double-serve).
	if state, _ := e.residence(t, root); state != beads.ResidenceStateJournal {
		t.Fatalf("residence = %q; want journal", state)
	}
	legRoot, _ := e.legacy.Get(root)
	if legRoot.Metadata["gc.migrated"] == "1" {
		t.Fatalf("alarm path marked legacy migrated; must not tombstone")
	}
	// The racing write is preserved on the legacy leg (not lost) for reconciliation.
	legStepA, _ := e.legacy.Get(stepA)
	if legStepA.Metadata["external.write"] != "close-window" {
		t.Fatalf("racing write lost from the legacy leg: %v", legStepA.Metadata)
	}
	// It drives a non-zero exit via the summary.
	summary := migrateGraphJournalSummary{Roots: []migrateRootResult{res}}
	if !summary.hasFailure() {
		t.Fatalf("close-loop alarm did not mark the summary as a failure (would exit 0)")
	}
}

// TestMigrateRefusesOpenAttemptKindWorker pins NOTE-2: an open+assigned attempt
// bead a worker could hold must trip the refuse-open-claimable guard even though
// isWorkerClassBead classes it as non-worker. v1-era attempts carry gc.kind=run /
// retry-run (StructuralGraphKinds); v2 attempts carry gc.attempt on their original
// kind. The gc.attempt case is paired with a structural kind so it exercises the
// gc.attempt arm of isWorkerHoldableAttempt (not the plain-worker fallback).
func TestMigrateRefusesOpenAttemptKindWorker(t *testing.T) {
	cases := []struct {
		name string
		meta map[string]string
	}{
		{"v1-run-kind", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRun}},
		{"v1-retry-run-kind", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindRetryRun}},
		{"v2-gc-attempt", map[string]string{
			beadmeta.KindMetadataKey:    beadmeta.KindCleanup,
			beadmeta.AttemptMetadataKey: "1",
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			e := newMigrateTestEnv(t)
			root, _, _ := e.seedMolecule(t)
			a, err := e.legacy.Create(beads.Bead{
				Title: "attempt", Type: "task", Assignee: "coder", ParentID: root, Metadata: tc.meta,
			})
			if err != nil {
				t.Fatalf("create attempt: %v", err)
			}
			if err := e.legacy.DepAdd(a.ID, root, "parent-child"); err != nil {
				t.Fatalf("dep attempt->root: %v", err)
			}
			res, err := migrateRoot(ctx, e.deps, root, false, false)
			if err == nil || res.Outcome != migrateOutcomeRefused {
				t.Fatalf("outcome = %q err=%v; want refused for an open+assigned attempt bead", res.Outcome, err)
			}
			if _, present := e.residence(t, root); present {
				t.Fatalf("refused root left a residence record; want ∅")
			}
		})
	}
}

// TestRunMigrateSummaryHasFailureOnRefusal pins that a refusal drives a non-zero
// exit (summary.hasFailure), so automation never reads a stranded-claim refusal as
// success.
func TestRunMigrateSummaryHasFailureOnRefusal(t *testing.T) {
	ctx := context.Background()
	e := newMigrateTestEnv(t)
	root, _, _ := e.seedMolecule(t)
	w, err := e.legacy.Create(beads.Bead{Title: "worker", Type: "task", Assignee: "coder", ParentID: root})
	if err != nil {
		t.Fatalf("create worker: %v", err)
	}
	if err := e.legacy.DepAdd(w.ID, root, "parent-child"); err != nil {
		t.Fatalf("dep: %v", err)
	}
	summary := runMigrateGraphJournal(ctx, e.deps, []string{root}, false, false, io.Discard)
	if !summary.hasFailure() {
		t.Fatalf("refusal did not mark the summary as a failure (would exit 0)")
	}
}
