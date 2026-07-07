package graphstore

import (
	"context"
	"path/filepath"
	"testing"
)

func newResidenceTestStore(t *testing.T) *Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	s, err := Open(context.Background(), path, Options{CityID: "residence-city"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestResidenceDefaultIsAbsent pins that a root with no record is the default ∅
// (legacy-resident) and that MigratingRoots is empty on a fresh store.
func TestResidenceDefaultIsAbsent(t *testing.T) {
	ctx := context.Background()
	s := newResidenceTestStore(t)

	_, _, present, err := s.ResidenceOf(ctx, "gcg-1")
	if err != nil {
		t.Fatalf("ResidenceOf: %v", err)
	}
	if present {
		t.Fatalf("fresh store reports gcg-1 present; want absent (∅ = legacy)")
	}
	roots, err := s.MigratingRoots(ctx)
	if err != nil {
		t.Fatalf("MigratingRoots: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("MigratingRoots = %v, want empty", roots)
	}
}

// TestResidenceCASTransitions pins the full CAS lifecycle: ∅→migrating→journal,
// with the losing transitions (double-park, stale-epoch flip, revert-of-journal)
// all rejected rather than clobbering.
func TestResidenceCASTransitions(t *testing.T) {
	ctx := context.Background()
	s := newResidenceTestStore(t)
	root := "gcg-7"

	won, err := s.BeginResidenceMigration(ctx, root, 100, "t0")
	if err != nil || !won {
		t.Fatalf("BeginResidenceMigration won=%t err=%v; want true,nil", won, err)
	}
	// Double-park loses.
	won2, err := s.BeginResidenceMigration(ctx, root, 200, "t1")
	if err != nil {
		t.Fatalf("second park err: %v", err)
	}
	if won2 {
		t.Fatalf("second BeginResidenceMigration won; want lost CAS")
	}
	state, epoch, present, err := s.ResidenceOf(ctx, root)
	if err != nil || !present {
		t.Fatalf("ResidenceOf present=%t err=%v", present, err)
	}
	if state != ResidenceMigrating || epoch != 100 {
		t.Fatalf("state=%q epoch=%d; want migrating,100 (first park wins, not clobbered)", state, epoch)
	}

	// A flip at the wrong epoch loses.
	flipped, err := s.FlipResidenceToJournal(ctx, root, 999, "t2")
	if err != nil {
		t.Fatalf("stale flip err: %v", err)
	}
	if flipped {
		t.Fatalf("flip at wrong epoch won; want lost CAS")
	}

	// MigratingRoots sees it.
	roots, err := s.MigratingRoots(ctx)
	if err != nil || len(roots) != 1 || roots[0] != root {
		t.Fatalf("MigratingRoots = %v err=%v; want [%s]", roots, err, root)
	}

	// The correct-epoch flip wins.
	flipped, err = s.FlipResidenceToJournal(ctx, root, 100, "t3")
	if err != nil || !flipped {
		t.Fatalf("flip won=%t err=%v; want true,nil", flipped, err)
	}
	state, _, _, err = s.ResidenceOf(ctx, root)
	if err != nil {
		t.Fatalf("ResidenceOf after flip: %v", err)
	}
	if state != ResidenceJournal {
		t.Fatalf("state=%q; want journal", state)
	}

	// A journal row is permanent: revert is a no-op.
	reverted, err := s.RevertResidence(ctx, root, 100)
	if err != nil {
		t.Fatalf("revert err: %v", err)
	}
	if reverted {
		t.Fatalf("RevertResidence demoted a journal row; want no-op")
	}
	state, _, _, _ = s.ResidenceOf(ctx, root)
	if state != ResidenceJournal {
		t.Fatalf("journal row not permanent after revert attempt; state=%q", state)
	}
}

// TestResidenceRevertMigrating pins that a migrating row reverts to ∅.
func TestResidenceRevertMigrating(t *testing.T) {
	ctx := context.Background()
	s := newResidenceTestStore(t)
	root := "gcg-9"

	if _, err := s.BeginResidenceMigration(ctx, root, 1, "t0"); err != nil {
		t.Fatalf("park: %v", err)
	}
	// A revert at a FOREIGN epoch is a no-op: it must never drop a sibling
	// migrator's row (BLOCKER-2 fence).
	reverted, err := s.RevertResidence(ctx, root, 999)
	if err != nil {
		t.Fatalf("foreign-epoch revert err: %v", err)
	}
	if reverted {
		t.Fatalf("RevertResidence dropped a row at a foreign epoch; want no-op")
	}
	if _, _, present, _ := s.ResidenceOf(ctx, root); !present {
		t.Fatalf("foreign-epoch revert removed the migrating row; want it intact")
	}
	// The own-epoch revert succeeds.
	reverted, err = s.RevertResidence(ctx, root, 1)
	if err != nil || !reverted {
		t.Fatalf("RevertResidence reverted=%t err=%v; want true,nil", reverted, err)
	}
	_, _, present, _ := s.ResidenceOf(ctx, root)
	if present {
		t.Fatalf("row still present after revert; want ∅")
	}
	// After revert, a fresh park succeeds (converges).
	won, err := s.BeginResidenceMigration(ctx, root, 2, "t1")
	if err != nil || !won {
		t.Fatalf("re-park after revert won=%t err=%v; want true,nil", won, err)
	}
}

// TestFrontierWriteClosedTriggers pins that a direct frontier write with the
// Tier-A gate closed is loudly aborted, completing the P1.2 write-closure.
func TestFrontierWriteClosedTriggers(t *testing.T) {
	ctx := context.Background()
	s := newResidenceTestStore(t)

	_, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO frontier (node_id, root_id, route, ready_priority, created_at, id)
		 VALUES ('n1','gcg-1','',2,'t','n1')`)
	if err == nil {
		t.Fatalf("direct frontier insert with gate closed succeeded; want write-closed ABORT")
	}
}
