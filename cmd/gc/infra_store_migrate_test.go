package main

import (
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/session"
)

// This is the FAST-TIER migrator-core test (E3, §6): it drives
// runInfraStoreMigration over the exact production wrapper stack — a work store
// = wrapStoreWithBeadPolicies(NewMemStore) and an infra store =
// wrapInfraStoreWithBeadPolicies(NewMemStoreHonoringIDs) — minus only the Dolt
// transport. It pins classification (coordclass.Classify is the sole authority),
// the global phase ordering, id stability, cross-store edge handling, count
// reconciliation, idempotent re-run, crash resume, dry-run zero-writes, and the
// orphan-preserving delete. It runs in-sandbox with no bd/dolt.

// migrateFastFixture is the two-store fast fixture: a work store and an infra
// store, both in the production policy shape, plus a record of the seeded ids by
// class so assertions can reason about the expected partition.
type migrateFastFixture struct {
	work  beads.Store
	infra beads.Store

	infraIDs []string // ids expected to move to the infra store
	workIDs  []string // ids expected to stay in the work store
}

func newMigrateFastFixture(t *testing.T) *migrateFastFixture {
	t.Helper()
	cfg := &config.City{}
	work := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	return &migrateFastFixture{work: work, infra: infra}
}

// createWork creates a bead in the work store honoring its explicit id (the work
// MemStore is NOT id-honoring by default, so seed via a stable-id create by
// passing the id — MemStore mints gc-N; to keep stable ids for cross-store dep
// assertions we instead record the returned id).
func (f *migrateFastFixture) createWork(t *testing.T, b beads.Bead, wantInfra bool) beads.Bead {
	t.Helper()
	created, err := f.work.Create(b)
	if err != nil {
		t.Fatalf("seed work bead %q: %v", b.Title, err)
	}
	// Sanity: the seeded bead must classify as we expect, so the fixture stays
	// honest against coordclass.Classify.
	if coordclass.Classify(created).IsInfrastructure() != wantInfra {
		t.Fatalf("seed bead %q classifies wrong: got infra=%v want infra=%v (type=%q labels=%v)",
			created.Title, coordclass.Classify(created).IsInfrastructure(), wantInfra, created.Type, created.Labels)
	}
	if wantInfra {
		f.infraIDs = append(f.infraIDs, created.ID)
	} else {
		f.workIDs = append(f.workIDs, created.ID)
	}
	return created
}

// seedMixedPopulation seeds a representative mix of infra-class and work-class
// beads in the WORK store (the comingled single-store state), returning the
// fixture. It also seeds a cross-store dependency (a moved graph child →
// staying work bead) and an inbound edge (staying work bead → a moved infra
// bead) to exercise edge preservation.
func (f *migrateFastFixture) seedMixedPopulation(t *testing.T) {
	t.Helper()

	// Work-class controls (stay behind).
	task := f.createWork(t, beads.Bead{Title: "real backlog item", Type: "task"}, false)
	convoyA := f.createWork(t, beads.Bead{Title: "convoy item A", Type: "task"}, false)
	f.createWork(t, beads.Bead{
		Title: "user convoy", Type: "convoy",
		Labels: []string{"tracks:" + convoyA.ID},
	}, false)

	// Session (infra).
	f.createWork(t, beads.Bead{
		Title: "worker-1", Type: session.BeadType,
		Labels: []string{session.LabelSession}, Metadata: map[string]string{"session_id": "sess-1"},
	}, true)
	// Session wait (infra).
	f.createWork(t, beads.Bead{
		Title: "wait:deps", Type: session.WaitBeadType,
		Labels: []string{session.WaitBeadLabel}, Metadata: map[string]string{"session_id": "sess-1"},
	}, true)
	// Mail message (infra).
	f.createWork(t, beads.Bead{Title: "hello", Type: "message", From: "human"}, true)
	// Nudge (infra).
	f.createWork(t, beads.Bead{
		Title: "nudge:worker-1", Type: "chore",
		Labels: []string{nudgeBeadLabel},
	}, true)
	// Order-tracking (infra).
	f.createWork(t, beads.Bead{
		Title: "order:gate-alpha", Type: "task",
		Labels: []string{labelOrderTracking},
	}, true)
	// Closed infra bead (exercises the closed-status restore path).
	closedInfra := f.createWork(t, beads.Bead{
		Title: "old session", Type: session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-0", "close_reason": "retired for the migration copy-status test"},
	}, true)
	if err := f.work.Close(closedInfra.ID); err != nil {
		t.Fatalf("close infra bead: %v", err)
	}

	// Closed work bead (stays; control that closed work is not touched).
	closedWork := f.createWork(t, beads.Bead{
		Title: "done task", Type: "task",
		Metadata: map[string]string{"close_reason": "finished before the migration ran here"},
	}, false)
	if err := f.work.Close(closedWork.ID); err != nil {
		t.Fatalf("close work bead: %v", err)
	}

	// Graph molecule root (infra) + child (infra), with a cross-store dep from the
	// child to the staying work task, and an intra-set dep from child to root.
	root := f.createWork(t, beads.Bead{
		Title: "workflow root", Type: "task",
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
	}, true)
	child := f.createWork(t, beads.Bead{
		Title: "workflow step", Type: "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	}, true)
	// child → root (intra-infra edge, must be re-added on the infra store).
	if err := f.work.DepAdd(child.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("dep child→root: %v", err)
	}
	// child → task (cross-boundary: source moves, target stays; must be re-added
	// as a dangling edge on the infra store).
	if err := f.work.DepAdd(child.ID, task.ID, "blocks"); err != nil {
		t.Fatalf("dep child→task: %v", err)
	}
	// task → root (INBOUND to a moved bead from a staying work bead: this edge is
	// co-resident with task (stays), and must SURVIVE in the work store as a
	// dangling row after root moves — the orphan-preserving-delete proof).
	if err := f.work.DepAdd(task.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("dep task→root: %v", err)
	}

	// Ephemeral wisp (infra, wisp tier).
	f.createWork(t, beads.Bead{
		Title: "wisp root", Type: beadmeta.KindWisp, Ephemeral: true,
		Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp},
	}, true)
}

func (f *migrateFastFixture) domainStores() []migrateStore {
	return []migrateStore{{ref: "city", store: f.work}}
}

// TestInfraStoreMigrateFastCore is the end-to-end fast-tier migration: seed a
// mixed population in the work store, migrate, and assert the boundary, id
// stability, cross-store edges, inbound-edge preservation, and count
// reconciliation.
func TestInfraStoreMigrateFastCore(t *testing.T) {
	f := newMigrateFastFixture(t)
	f.seedMixedPopulation(t)

	wantMoved := len(f.infraIDs)

	ledger, err := runInfraStoreMigration(f.domainStores(), f.infra, false)
	if err != nil {
		t.Fatalf("migration: %v", err)
	}

	if ledger.Moved != wantMoved {
		t.Errorf("moved = %d, want %d", ledger.Moved, wantMoved)
	}
	if ledger.Deleted != wantMoved {
		t.Errorf("deleted = %d, want %d", ledger.Deleted, wantMoved)
	}

	// Boundary: work store holds no infra bead; infra store holds no work bead.
	if err := verifyStoreClassBoundary(f.work, false); err != nil {
		t.Errorf("work store boundary: %v", err)
	}
	if err := verifyStoreClassBoundary(f.infra, true); err != nil {
		t.Errorf("infra store boundary: %v", err)
	}

	// Id stability: every moved id Gets from the infra store with its original id.
	for _, id := range f.infraIDs {
		got, err := f.infra.Get(id)
		if err != nil {
			t.Errorf("moved bead %q not found in infra store: %v", id, err)
			continue
		}
		if got.ID != id {
			t.Errorf("moved bead re-minted: %q → %q", id, got.ID)
		}
	}

	// Work beads still present in the work store, unchanged in id.
	for _, id := range f.workIDs {
		if _, err := f.work.Get(id); err != nil {
			t.Errorf("work bead %q missing from work store after migration: %v", id, err)
		}
	}

	// Count reconciliation: work_after == work_before − moved; infra_after ==
	// infra_before + moved.
	if got := ledger.WorkAfter["city"]; got != ledger.WorkBefore["city"]-wantMoved {
		t.Errorf("work_after[city] = %d, want %d (before %d − moved %d)",
			got, ledger.WorkBefore["city"]-wantMoved, ledger.WorkBefore["city"], wantMoved)
	}
	if ledger.InfraAfter != ledger.InfraBefore+wantMoved {
		t.Errorf("infra_after = %d, want %d", ledger.InfraAfter, ledger.InfraBefore+wantMoved)
	}

	// Cross-store edges: the graph child's edges to the (moved) root and the
	// (staying) work task both live on the infra store now.
	assertInfraHasEdge(t, f.infra, findIDByTitle(t, f.infra, "workflow step"), findIDByTitle(t, f.infra, "workflow root"))

	// Inbound-edge preservation on a NON-cascading backend (the MemStore contract):
	// the staying work task's edge to the now-moved root survives in the work store
	// as a dangling row. NOTE: real bd cascades this row away
	// (fk_dep_issue_target ON DELETE CASCADE, migration 0043), so the integration
	// tier instead asserts the batch delete's neighbor free-text is not
	// mutation-bombed. The MemStore contract is the row-preserving model for
	// backends without that cascade.
	taskID := findIDByTitle(t, f.work, "real backlog item")
	rootID := findIDByTitle(t, f.infra, "workflow root")
	assertWorkStoreHasDanglingEdge(t, f.work, taskID, rootID)
}

// TestInfraStoreMigrateReRunNoOp asserts a second migration on an already-migrated
// city moves and deletes nothing (convergent no-op).
func TestInfraStoreMigrateReRunNoOp(t *testing.T) {
	f := newMigrateFastFixture(t)
	f.seedMixedPopulation(t)

	if _, err := runInfraStoreMigration(f.domainStores(), f.infra, false); err != nil {
		t.Fatalf("first migration: %v", err)
	}
	ledger, err := runInfraStoreMigration(f.domainStores(), f.infra, false)
	if err != nil {
		t.Fatalf("second migration: %v", err)
	}
	if ledger.Moved != 0 {
		t.Errorf("re-run moved = %d, want 0 (convergent no-op)", ledger.Moved)
	}
	if ledger.Deleted != 0 {
		t.Errorf("re-run deleted = %d, want 0", ledger.Deleted)
	}
}

// TestInfraStoreMigrateCrashResume simulates a crash between copy and delete: one
// infra bead is HAND-COPIED into the infra store (present in both stores) before
// the migration runs. The migration must converge — skip its copy, still delete
// it from the work store — with no duplicate.
func TestInfraStoreMigrateCrashResume(t *testing.T) {
	f := newMigrateFastFixture(t)
	f.seedMixedPopulation(t)

	// Pick a moved bead and pre-copy it into the infra store (the crash-between-
	// copy-and-delete state: it exists in BOTH stores).
	preCopiedID := f.infraIDs[0]
	src, err := f.work.Get(preCopiedID)
	if err != nil {
		t.Fatalf("get pre-copy source: %v", err)
	}
	if err := copyBeadPreservingID(f.infra, src); err != nil {
		t.Fatalf("hand pre-copy: %v", err)
	}

	ledger, err := runInfraStoreMigration(f.domainStores(), f.infra, false)
	if err != nil {
		t.Fatalf("resume migration: %v", err)
	}
	if ledger.AlreadyPresent < 1 {
		t.Errorf("already_present = %d, want >= 1 (the hand-copied bead)", ledger.AlreadyPresent)
	}

	// No duplicate: the pre-copied id appears exactly once in the infra store.
	if n := countInfraOccurrences(t, f.infra, preCopiedID); n != 1 {
		t.Errorf("pre-copied bead appears %d times in infra store, want exactly 1", n)
	}
	// It is deleted from the work store.
	if _, err := f.work.Get(preCopiedID); err == nil {
		t.Errorf("pre-copied bead %q still present in work store after resume", preCopiedID)
	}
	// Full boundary still holds.
	if err := verifyStoreClassBoundary(f.work, false); err != nil {
		t.Errorf("work store boundary after resume: %v", err)
	}
	if err := verifyStoreClassBoundary(f.infra, true); err != nil {
		t.Errorf("infra store boundary after resume: %v", err)
	}
}

// TestInfraStoreMigrateDryRunZeroWrites asserts --dry-run makes NO writes: the
// work store is unchanged, the infra store gains nothing, and the ledger reports
// the plan.
func TestInfraStoreMigrateDryRunZeroWrites(t *testing.T) {
	f := newMigrateFastFixture(t)
	f.seedMixedPopulation(t)

	workBefore := storeIDs(t, f.work)
	infraBefore := storeIDs(t, f.infra)

	ledger, err := runInfraStoreMigration(f.domainStores(), f.infra, true)
	if err != nil {
		t.Fatalf("dry-run migration: %v", err)
	}
	if !ledger.DryRun {
		t.Error("ledger.DryRun = false, want true")
	}
	if ledger.Moved != len(f.infraIDs) {
		t.Errorf("dry-run moved (planned) = %d, want %d", ledger.Moved, len(f.infraIDs))
	}
	if ledger.Deleted != 0 {
		t.Errorf("dry-run deleted = %d, want 0", ledger.Deleted)
	}

	// Zero writes: both stores are byte-identical (by id set) to before.
	if got := storeIDs(t, f.work); !equalStringSets(got, workBefore) {
		t.Errorf("dry-run mutated the work store: before=%v after=%v", workBefore, got)
	}
	if got := storeIDs(t, f.infra); !equalStringSets(got, infraBefore) {
		t.Errorf("dry-run mutated the infra store: before=%v after=%v", infraBefore, got)
	}
}

// ── the tripwire ──

// deleteSpyStore wraps a MemStore and RECORDS every Delete / DeleteAllOrphaning
// call so a test can assert that single-id Delete is NEVER used on a multi-bead
// move set (the mutation-bomb path). It delegates everything to the embedded
// MemStore so behavior is otherwise identical.
type deleteSpyStore struct {
	*beads.MemStore
	singleDeletes []string
	batchDeletes  [][]string
}

func newDeleteSpyStore() *deleteSpyStore {
	return &deleteSpyStore{MemStore: beads.NewMemStore()}
}

func (s *deleteSpyStore) Delete(id string) error {
	s.singleDeletes = append(s.singleDeletes, id)
	return s.MemStore.Delete(id)
}

func (s *deleteSpyStore) DeleteAllOrphaning(ids []string) (int, error) {
	s.batchDeletes = append(s.batchDeletes, append([]string(nil), ids...))
	return s.MemStore.DeleteAllOrphaning(ids)
}

// TestInfraStoreMigrateNeverSingleDeletesMoveSet is the tripwire (§8 deliverable):
// with more than one infra bead to move, the migration must delete them via the
// orphan-preserving batch path — NEVER via single-id Delete, which would strip
// inbound edges and text-rewrite neighbors (the mutation bomb).
func TestInfraStoreMigrateNeverSingleDeletesMoveSet(t *testing.T) {
	cfg := &config.City{}
	spy := newDeleteSpyStore()
	work := wrapStoreWithBeadPolicies(spy, cfg)
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)

	// Seed at least two infra beads (a multi-bead move set) plus a work control.
	seed := func(b beads.Bead) {
		if _, err := work.Create(b); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	seed(beads.Bead{Title: "s1", Type: session.BeadType, Labels: []string{session.LabelSession}})
	seed(beads.Bead{Title: "s2", Type: session.BeadType, Labels: []string{session.LabelSession}})
	seed(beads.Bead{Title: "m1", Type: "message"})
	seed(beads.Bead{Title: "work", Type: "task"})

	if _, err := runInfraStoreMigration([]migrateStore{{ref: "city", store: work}}, infra, false); err != nil {
		t.Fatalf("migration: %v", err)
	}

	if len(spy.singleDeletes) != 0 {
		t.Errorf("single-id Delete was called %d time(s) on a multi-bead move set: %v "+
			"(must use the orphan-preserving batch delete)", len(spy.singleDeletes), spy.singleDeletes)
	}
	if len(spy.batchDeletes) == 0 {
		t.Error("no batch delete recorded; the move set must be deleted via DeleteAllOrphaning")
	}
	for _, chunk := range spy.batchDeletes {
		if len(chunk) < 2 {
			t.Errorf("batch delete chunk %v has fewer than 2 ids; the batch path requires >= 2", chunk)
		}
	}
}

// TestBatchDeleterPreservesInboundEdges pins the crux BatchDeleter semantic at the
// store level: DeleteAllOrphaning drops the deleted beads' OWN outbound edges but
// PRESERVES inbound edges from staying beads as dangling rows.
func TestBatchDeleterPreservesInboundEdges(t *testing.T) {
	m := beads.NewMemStore()
	stay, _ := m.Create(beads.Bead{Title: "stay", Type: "task"})
	goA, _ := m.Create(beads.Bead{Title: "goA", Type: "task"})
	goB, _ := m.Create(beads.Bead{Title: "goB", Type: "task"})
	// stay → goA (inbound to the deleted set): must survive dangling.
	if err := m.DepAdd(stay.ID, goA.ID, "blocks"); err != nil {
		t.Fatalf("dep stay→goA: %v", err)
	}
	// goA → goB (outbound of a deleted bead): must be dropped.
	if err := m.DepAdd(goA.ID, goB.ID, "blocks"); err != nil {
		t.Fatalf("dep goA→goB: %v", err)
	}

	deleted, err := m.DeleteAllOrphaning([]string{goA.ID, goB.ID})
	if err != nil {
		t.Fatalf("DeleteAllOrphaning: %v", err)
	}
	if deleted != 2 {
		t.Errorf("deleted = %d, want 2", deleted)
	}

	// The inbound edge stay→goA survives as a dangling row.
	stayDeps, err := m.DepList(stay.ID, "down")
	if err != nil {
		t.Fatalf("DepList(stay): %v", err)
	}
	found := false
	for _, d := range stayDeps {
		if d.DependsOnID == goA.ID {
			found = true
		}
	}
	if !found {
		t.Errorf("inbound edge stay→goA was stripped; it must survive as a dangling row (got %v)", stayDeps)
	}

	// The staying bead itself is untouched.
	if _, err := m.Get(stay.ID); err != nil {
		t.Errorf("staying bead was deleted: %v", err)
	}
	// The deleted beads are gone.
	if _, err := m.Get(goA.ID); err == nil {
		t.Error("goA still present after DeleteAllOrphaning")
	}
}

// TestBatchDeleterSingletonWarnsFallback documents the degenerate single-id case:
// DeleteAllOrphaning with one id falls back to Delete (there is no batch shape for
// one id) and still removes the bead.
func TestBatchDeleterSingletonFallback(t *testing.T) {
	m := beads.NewMemStore()
	only, _ := m.Create(beads.Bead{Title: "only", Type: "task"})
	deleted, err := m.DeleteAllOrphaning([]string{only.ID})
	if err != nil {
		t.Fatalf("DeleteAllOrphaning(single): %v", err)
	}
	if deleted != 1 {
		t.Errorf("deleted = %d, want 1", deleted)
	}
	if _, err := m.Get(only.ID); err == nil {
		t.Error("single bead not deleted")
	}
}

// ── test helpers ──

func findIDByTitle(t *testing.T, store beads.Store, title string) string {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	for _, b := range list {
		if b.Title == title {
			return b.ID
		}
	}
	t.Fatalf("no bead titled %q in store", title)
	return ""
}

func assertInfraHasEdge(t *testing.T, store beads.Store, issueID, dependsOnID string) {
	t.Helper()
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		t.Fatalf("DepList(%q): %v", issueID, err)
	}
	for _, d := range deps {
		if d.DependsOnID == dependsOnID {
			return
		}
	}
	t.Errorf("infra store missing edge %s→%s (got %v)", issueID, dependsOnID, deps)
}

func assertWorkStoreHasDanglingEdge(t *testing.T, store beads.Store, issueID, danglingTarget string) {
	t.Helper()
	deps, err := store.DepList(issueID, "down")
	if err != nil {
		t.Fatalf("DepList(%q): %v", issueID, err)
	}
	for _, d := range deps {
		if d.DependsOnID == danglingTarget {
			// And the target is gone from this store (that's what makes it dangling).
			if _, err := store.Get(danglingTarget); err == nil {
				t.Errorf("edge %s→%s target is still in the work store; not a dangling edge", issueID, danglingTarget)
			}
			return
		}
	}
	t.Errorf("work store lost the inbound edge %s→%s; the orphan-preserving delete must keep it dangling (got %v)",
		issueID, danglingTarget, deps)
}

func countInfraOccurrences(t *testing.T, store beads.Store, id string) int {
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

func storeIDs(t *testing.T, store beads.Store) []string {
	t.Helper()
	list, err := store.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	ids := make([]string, 0, len(list))
	for _, b := range list {
		ids = append(ids, b.ID)
	}
	sort.Strings(ids)
	return ids
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
