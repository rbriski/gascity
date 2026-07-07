package beads

import (
	"context"
	"testing"
	"time"
)

// importedBead builds an open Bead with a preserved id for the migration import
// path.
func importedBead(id, title string) Bead {
	return Bead{
		ID:        id,
		Title:     title,
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC),
	}
}

// TestImportSubtreePreservesIDsAndHidesWhileMigrating pins the two load-bearing
// properties of the copy step: ids are preserved verbatim (no gcg-j<seq> mint),
// and while the root is `migrating` the imported rows are hidden from every façade
// read (the residence-visibility gate) yet readable via StagedRootBeads for
// fold-verify. The flip then makes them visible.
func TestImportSubtreePreservesIDsAndHidesWhileMigrating(t *testing.T) {
	ctx := context.Background()
	s := newJournalTestStore(t)
	root := "gcg-100"

	subtree := []Bead{
		importedBead(root, "root molecule"),
		importedBead("gcg-101", "step one"),
	}
	// Attach a dependency so edge preservation is exercised.
	subtree[1].Dependencies = []Dep{{IssueID: "gcg-101", DependsOnID: root, Type: "parent-child"}}

	// Park the root (migrating) BEFORE importing, so the visibility gate is armed.
	if _, err := s.BeginResidenceMigration(ctx, root, 1); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := s.ImportSubtree(ctx, subtree, nil, root); err != nil {
		t.Fatalf("ImportSubtree: %v", err)
	}

	// Hidden from façade Get while migrating.
	if _, err := s.Get(root); err == nil {
		t.Fatalf("Get(%s) returned a migrating import row; want hidden (ErrNotFound)", root)
	}
	// Hidden from List/Ready too.
	open, err := s.List(ListQuery{AllowScan: true, IncludeClosed: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(open) != 0 {
		t.Fatalf("List returned %d migrating rows; want 0 (hidden)", len(open))
	}

	// Visible to the staged reader (ids preserved).
	staged, err := s.StagedRootBeads(ctx, root)
	if err != nil {
		t.Fatalf("StagedRootBeads: %v", err)
	}
	if len(staged) != 2 {
		t.Fatalf("staged = %d beads; want 2", len(staged))
	}
	ids := map[string]bool{}
	for _, b := range staged {
		ids[b.ID] = true
	}
	if !ids[root] || !ids["gcg-101"] {
		t.Fatalf("staged ids = %v; want the preserved gcg-100/gcg-101", ids)
	}

	// Flip → visible, journal-authoritative.
	if _, err := s.FlipResidenceToJournal(ctx, root, 1); err != nil {
		t.Fatalf("flip: %v", err)
	}
	got, err := s.Get(root)
	if err != nil {
		t.Fatalf("Get(%s) after flip: %v", root, err)
	}
	if got.ID != root || got.Title != "root molecule" {
		t.Fatalf("post-flip Get = %+v; want the preserved root", got)
	}
	if !got.CreatedAt.Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
		t.Fatalf("CreatedAt not preserved: %v", got.CreatedAt)
	}
}

// TestDiscardRootRemovesStagedCopy pins the abort/recover cleanup: discarding a
// staged root deletes exactly its imported rows.
func TestDiscardRootRemovesStagedCopy(t *testing.T) {
	ctx := context.Background()
	s := newJournalTestStore(t)
	root := "gcg-200"

	if _, err := s.BeginResidenceMigration(ctx, root, 1); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := s.ImportSubtree(ctx, []Bead{importedBead(root, "r")}, nil, root); err != nil {
		t.Fatalf("import: %v", err)
	}
	staged, _ := s.StagedRootBeads(ctx, root)
	if len(staged) != 1 {
		t.Fatalf("staged = %d; want 1", len(staged))
	}
	// A discard at a FOREIGN epoch must delete nothing (BLOCKER-2 fence): the
	// staged copy belongs to the migrating(1) row, not to epoch 999.
	if err := s.DiscardRoot(ctx, root, 999); err != nil {
		t.Fatalf("foreign-epoch DiscardRoot err: %v", err)
	}
	if staged, _ = s.StagedRootBeads(ctx, root); len(staged) != 1 {
		t.Fatalf("foreign-epoch discard removed staged rows; want 1 intact, got %d", len(staged))
	}
	// The own-epoch discard clears exactly its imported rows.
	if err := s.DiscardRoot(ctx, root, 1); err != nil {
		t.Fatalf("DiscardRoot: %v", err)
	}
	staged, _ = s.StagedRootBeads(ctx, root)
	if len(staged) != 0 {
		t.Fatalf("staged after discard = %d; want 0", len(staged))
	}
}

// TestImportSubtreeRejectsMintShapedID pins the MEDIUM id-namespace guard: a
// source id shaped like the store's own mint (gcg-j<seq>) is refused, because
// importing one could collide with a future mint and wedge the counter.
func TestImportSubtreeRejectsMintShapedID(t *testing.T) {
	ctx := context.Background()
	s := newJournalTestStore(t)
	root := "gcg-j7" // mint-shaped: illegal as a legacy source id
	if _, err := s.BeginResidenceMigration(ctx, root, 1); err != nil {
		t.Fatalf("park: %v", err)
	}
	err := s.ImportSubtree(ctx, []Bead{importedBead(root, "r")}, nil, root)
	if err == nil {
		t.Fatalf("ImportSubtree accepted a gcg-j<seq>-shaped source id; want rejection")
	}
	// A non-mint-shaped legacy id (gcg-500) imports fine.
	ok := "gcg-500"
	if _, err := s.BeginResidenceMigration(ctx, ok, 1); err != nil {
		t.Fatalf("park ok: %v", err)
	}
	if err := s.ImportSubtree(ctx, []Bead{importedBead(ok, "r")}, nil, ok); err != nil {
		t.Fatalf("ImportSubtree(gcg-500): %v", err)
	}
}

// TestImportSubtreePreservesEdgeMetadata pins HIGH-1 at the store layer: an edge
// imported with metadata keeps it, so a waits-for gate survives the copy.
func TestImportSubtreePreservesEdgeMetadata(t *testing.T) {
	ctx := context.Background()
	s := newJournalTestStore(t)
	root := "gcg-600"
	child := "gcg-601"
	subtree := []Bead{
		importedBead(root, "root"),
		importedBead(child, "child"),
	}
	subtree[1].Dependencies = []Dep{{IssueID: child, DependsOnID: root, Type: "waits-for"}}
	edgeMeta := map[EdgeKey]string{
		{FromID: child, ToID: root, DepType: "waits-for"}: `{"gate":"any-children"}`,
	}
	if _, err := s.BeginResidenceMigration(ctx, root, 1); err != nil {
		t.Fatalf("park: %v", err)
	}
	if err := s.ImportSubtree(ctx, subtree, edgeMeta, root); err != nil {
		t.Fatalf("ImportSubtree: %v", err)
	}
	got, err := s.EdgeMetadata(child, root, "waits-for")
	if err != nil {
		t.Fatalf("EdgeMetadata: %v", err)
	}
	if got != `{"gate":"any-children"}` {
		t.Fatalf("edge metadata = %q; want the preserved gate", got)
	}
}

// TestResidenceCapabilityReachableThroughAccessors pins that the residence
// capability probes resolve to the JournalStore.
func TestResidenceCapabilityReachableThroughAccessors(t *testing.T) {
	s := newJournalTestStore(t)
	if _, ok := ResidenceStoreFor(s); !ok {
		t.Fatalf("ResidenceStoreFor(JournalStore) = false; want true")
	}
	if _, ok := ResidenceMigrationStoreFor(s); !ok {
		t.Fatalf("ResidenceMigrationStoreFor(JournalStore) = false; want true")
	}
	// A store without the capability returns the honest absent signal.
	if _, ok := ResidenceStoreFor(NewMemStore()); ok {
		t.Fatalf("ResidenceStoreFor(MemStore) = true; want false")
	}
}

// TestJournalBornBeadsStayVisibleWithResidenceTable pins the inert property at the
// storage level: a normal façade-created bead (stream_id=”) is visible even
// while an UNRELATED root is migrating, so the visibility gate never hides
// journal-born work.
func TestJournalBornBeadsStayVisibleWithResidenceTable(t *testing.T) {
	ctx := context.Background()
	s := newJournalTestStore(t)

	born, err := s.Create(Bead{Title: "journal-born"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Some other root is migrating.
	if _, err := s.BeginResidenceMigration(ctx, "gcg-999", 1); err != nil {
		t.Fatalf("park unrelated: %v", err)
	}
	got, err := s.Get(born.ID)
	if err != nil {
		t.Fatalf("Get journal-born: %v", err)
	}
	if got.ID != born.ID {
		t.Fatalf("journal-born hidden by an unrelated migration")
	}
}
