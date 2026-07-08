package beads

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestJournalStoreSetMetadataIfFoldOwnedWriteClosed pins that a conditional
// metadata compare-and-set against a fold-owned Tier-A row is refused with
// ErrFoldOwnedWriteClosed (I-14): the mutation-primary façade never rewrites a
// row the fold applier owns, even through the CAS surface.
func TestJournalStoreSetMetadataIfFoldOwnedWriteClosed(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	db := s.gs.DB()
	// Insert a fold-owned Tier-A row by opening the write gate directly, mimicking
	// the fold applier. The façade must refuse to CAS it.
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO nodes (id, title, status, created_at, fold_owned, stream_id)
		VALUES ('gcg-fold-cas', 'fold owned', 'open', ?, 1, 'gcg-root')`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert fold-owned node: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}

	swapped, err := s.SetMetadataIf(ctx, "gcg-fold-cas", "gc.control_epoch", "1", "2")
	if !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("SetMetadataIf fold-owned = (%v, %v), want ErrFoldOwnedWriteClosed", swapped, err)
	}
	if swapped {
		t.Fatal("swapped = true on a fold-owned row, want false")
	}
}
