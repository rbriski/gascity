package graphstore

import (
	"context"
	"errors"
	"testing"
)

// TestTruncateBelowAnchorThenVerify proves the SEC-T-6 story: after truncating a
// stream below a durable snapshot, the surviving tail still Verifies — the chain
// walk resumes from the cut_chain_hash the covering snapshot recorded, with the
// snapshot as the new anchor. The truncated prefix is gone; the head is intact.
func TestTruncateBelowAnchorThenVerify(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-trunc"

	head := appendN(t, s, stream, 3) // seq 1..3
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-trunc"}`)
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil { // anchor at seq 4
		t.Fatalf("write snapshot: %v", err)
	}
	appendMore(t, s, stream, 4, 2) // seq 5..6, appended after head=4

	// Before truncation the whole stream verifies.
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("Verify before truncate: %v", err)
	}

	n, err := s.TruncateBelowAnchor(ctx, stream, 3)
	if err != nil {
		t.Fatalf("truncate: %v", err)
	}
	if n != 3 {
		t.Fatalf("deleted %d rows, want 3 (seq 1..3)", n)
	}

	surviving, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read surviving: %v", err)
	}
	if len(surviving) != 3 || surviving[0].Seq != 4 {
		t.Fatalf("surviving = %d events starting seq %d, want 3 starting seq 4", len(surviving), surviving[0].Seq)
	}
	// The chain still holds across the cut.
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("Verify after truncate = %v, want nil (chain walks from the cut anchor)", err)
	}

	// Re-truncating at the same anchor is an idempotent no-op.
	if n, err := s.TruncateBelowAnchor(ctx, stream, 3); err != nil || n != 0 {
		t.Fatalf("re-truncate = (%d, %v), want (0, nil)", n, err)
	}
}

// TestTruncateRefusesWithoutCoveringSnapshot proves truncation is never allowed
// past the latest durable snapshot: an anchor seq that is not a snapshot's
// covered_seq is refused with ErrNoCoveringSnapshot and nothing is deleted.
func TestTruncateRefusesWithoutCoveringSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-truncrefuse"

	head := appendN(t, s, stream, 5)
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-truncrefuse"}`) // covers 5
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}

	// No snapshot covers seq 3, so truncating there is refused.
	if _, err := s.TruncateBelowAnchor(ctx, stream, 3); !errors.Is(err, ErrNoCoveringSnapshot) {
		t.Fatalf("truncate without a covering snapshot = %v, want ErrNoCoveringSnapshot", err)
	}
	// And nothing was deleted.
	if h, _ := s.Head(ctx, stream); h != head+1 {
		t.Fatalf("head = %d after a refused truncate, want %d", h, head+1)
	}
	if all, _ := s.ReadStream(ctx, stream, 1, 0); len(all) != 6 {
		t.Fatalf("events = %d after a refused truncate, want 6", len(all))
	}
}

// TestRetentionGateWriteClosure proves the journal delete path stays write-closed:
// an ungated DELETE of a journal row aborts with the append-only trigger, and
// TruncateBelowAnchor (which opens the retention_gate) is the only path that can
// delete — after which the gate is closed again (no standing delete permission).
func TestRetentionGateWriteClosure(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-retgate"

	appendN(t, s, stream, 2)

	// An ungated DELETE aborts: no retention_gate row covers the seq.
	_, err := s.DB().ExecContext(ctx, `DELETE FROM journal WHERE stream_id = ? AND seq = 1`, stream)
	if err == nil || indexOf(err.Error(), "append-only") < 0 {
		t.Fatalf("ungated journal DELETE = %v, want append-only abort", err)
	}

	// After a real truncation the gate is closed again: a fresh ungated DELETE of
	// the surviving head still aborts.
	snap, anchor := makeSnap(t, stream, 2, `{"root_id":"gcj-root-retgate"}`)
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil { // anchor at 3
		t.Fatalf("write snapshot: %v", err)
	}
	if _, err := s.TruncateBelowAnchor(ctx, stream, 2); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	var gateRows int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM retention_gate WHERE stream_id = ?`, stream).Scan(&gateRows); err != nil {
		t.Fatalf("count retention_gate: %v", err)
	}
	if gateRows != 0 {
		t.Fatalf("retention_gate rows = %d after truncate, want 0 (no standing delete permission)", gateRows)
	}
	_, err = s.DB().ExecContext(ctx, `DELETE FROM journal WHERE stream_id = ? AND seq = 3`, stream)
	if err == nil || indexOf(err.Error(), "append-only") < 0 {
		t.Fatalf("ungated DELETE after truncate = %v, want append-only abort", err)
	}
}

// TestTruncateRefusesRottedCoveringSnapshot proves H2 at the store layer:
// TruncateBelowAnchor verifies the covering snapshot's blob hashes to its
// state_hash BEFORE deleting the prefix. A rotted blob (the only rebuild source
// once the prefix is gone) aborts the truncation with ErrSnapshotHashMismatch,
// and nothing is deleted.
func TestTruncateRefusesRottedCoveringSnapshot(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-truncrot"

	head := appendN(t, s, stream, 3) // seq 1..3
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-truncrot"}`)
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil { // anchor at 4
		t.Fatalf("write snapshot: %v", err)
	}

	// Rot the covering snapshot's blob through the write gate.
	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET state = x'deadbeef' WHERE stream_id = ? AND covered_seq = ?`, stream, head); err != nil {
		t.Fatalf("rot blob: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit rot: %v", err)
	}

	if _, err := s.TruncateBelowAnchor(ctx, stream, head); !errors.Is(err, ErrSnapshotHashMismatch) {
		t.Fatalf("truncate over a rotted covering snapshot = %v, want ErrSnapshotHashMismatch", err)
	}
	// Nothing was deleted: the prefix (seq 1..3) survives intact.
	surviving, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read surviving: %v", err)
	}
	if len(surviving) != 4 || surviving[0].Seq != 1 {
		t.Fatalf("surviving = %d events from seq %d, want 4 from seq 1 (prefix preserved)", len(surviving), surviving[0].Seq)
	}
}

// appendMore appends n testType events to stream starting at expectedVersion.
func appendMore(t *testing.T, s *Store, stream string, from uint64, n int) {
	t.Helper()
	ctx := context.Background()
	head := from
	for i := 0; i < n; i++ {
		res, err := s.Append(ctx, stream, testEngine, head, 0, []JournalEvent{{
			Type: testType, IdemToken: stream + ":more:" + itoa(int(head)), Payload: canonPayload(t, `{"more":1}`),
		}})
		if err != nil {
			t.Fatalf("append more: %v", err)
		}
		head = res.FirstSeq
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
