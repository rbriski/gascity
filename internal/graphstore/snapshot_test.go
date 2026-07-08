package graphstore

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

const anchorType = "lumen.snapshot.anchored"

// appendN appends n fresh testType events to stream and returns the head after.
func appendN(t *testing.T, s *Store, stream string, n int) uint64 {
	t.Helper()
	ctx := context.Background()
	var head uint64
	for i := 0; i < n; i++ {
		res, err := s.Append(ctx, stream, testEngine, head, 0, []JournalEvent{{
			Type: testType, IdemToken: fmt.Sprintf("%s:e%d", stream, i), Payload: canonPayload(t, fmt.Sprintf(`{"i":%d}`, i)),
		}})
		if err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		head = res.FirstSeq
	}
	return head
}

// makeSnap builds a self-consistent fold.Snapshot (StateHash == canon.Hash of
// State) covering `covered`, plus its anchor event.
func makeSnap(t *testing.T, stream string, covered uint64, stateJSON string) (fold.Snapshot, JournalEvent) {
	t.Helper()
	blob := canonPayload(t, stateJSON)
	h := canon.Hash(blob)
	snap := fold.Snapshot{
		StreamID: stream, CoveredSeq: covered, Engine: "lumen",
		ReducerVersion: 2, SnapshotFormatVersion: 2, StateHash: h, State: blob,
	}
	anchor := JournalEvent{
		Type: anchorType, IRContractVersion: "0.2.5",
		IdemToken: fmt.Sprintf("%s:snap:%d", stream, covered),
		Payload:   canonPayload(t, fmt.Sprintf(`{"covered_seq":%d,"state_hash":%q}`, covered, hex.EncodeToString(h[:]))),
	}
	return snap, anchor
}

// TestWriteSnapshotAnchorsAndVerifies proves WriteSnapshot writes the snapshots
// row AND appends the anchor event at head+1 in one transaction, LatestSnapshot
// reads the blob back, and the chain still Verifies with the anchor in it.
func TestWriteSnapshotAnchorsAndVerifies(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-snap"

	head := appendN(t, s, stream, 3) // seq 1..3
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-snap","closed":false}`)

	anchorSeq, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor)
	if err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	if anchorSeq != head+1 {
		t.Fatalf("anchor seq = %d, want %d (head+1)", anchorSeq, head+1)
	}

	got, ok, err := s.LatestSnapshot(ctx, stream)
	if err != nil || !ok {
		t.Fatalf("latest snapshot: ok=%v err=%v", ok, err)
	}
	if got.CoveredSeq != head || got.Engine != "lumen" || got.ReducerVersion != 2 {
		t.Errorf("snapshot = %+v, want covered=%d engine=lumen rv=2", got, head)
	}
	if canon.Hash(got.State) != got.StateHash {
		t.Errorf("round-tripped state hash does not match its blob")
	}
	// The anchor is a real journal event and the chain still verifies with it.
	if err := s.Verify(ctx, stream); err != nil {
		t.Errorf("Verify after snapshot = %v, want nil", err)
	}
	if h, _ := s.Head(ctx, stream); h != head+1 {
		t.Errorf("head = %d after snapshot, want %d", h, head+1)
	}
}

// TestSnapshotWriteClosureGuard_DETT18 proves the snapshots table is write-closed:
// a writer that does not open the snapshot_write_gate (i.e. is not WriteSnapshot /
// TruncateBelowAnchor) hits a loud ABORT on INSERT, UPDATE, and DELETE, while the
// gated WriteSnapshot path succeeds. Mirrors the tier_a_write_gate discipline.
func TestSnapshotWriteClosureGuard_DETT18(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-snapguard"
	db := s.DB()

	// A non-gated INSERT is refused.
	_, err := db.ExecContext(ctx,
		`INSERT INTO snapshots (stream_id, covered_seq, engine, reducer_version,
		   snapshot_format_version, state_hash, state, created_at)
		 VALUES ('x', 1, 'lumen', 2, 2, zeroblob(32), x'00', '2020-01-01T00:00:00Z')`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("non-gated INSERT = %v, want write-closed abort", err)
	}

	// The gated path succeeds and plants a row.
	head := appendN(t, s, stream, 1)
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-snapguard"}`)
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil {
		t.Fatalf("gated WriteSnapshot: %v", err)
	}

	// A non-gated UPDATE and DELETE of the planted row are both refused.
	_, err = db.ExecContext(ctx, `UPDATE snapshots SET state = x'ff' WHERE stream_id = ?`, stream)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("non-gated UPDATE = %v, want write-closed abort", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM snapshots WHERE stream_id = ?`, stream)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("non-gated DELETE = %v, want write-closed abort", err)
	}

	// The row survived the rejected mutations intact.
	var covered uint64
	if err := db.QueryRowContext(ctx, `SELECT covered_seq FROM snapshots WHERE stream_id = ?`, stream).Scan(&covered); err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if covered != head {
		t.Fatalf("covered = %d, want %d — a non-gated write leaked through", covered, head)
	}

	// The gate is committed-closed after WriteSnapshot, so the closure still bites.
	var open int
	if err := db.QueryRowContext(ctx, `SELECT open FROM snapshot_write_gate WHERE singleton = 0`).Scan(&open); err != nil {
		t.Fatalf("read gate: %v", err)
	}
	if open != 0 {
		t.Fatalf("snapshot_write_gate.open = %d after WriteSnapshot, want 0", open)
	}
}

// TestWriteSnapshotRejectsHashMismatch proves the R-SNAP-WRITE gate: a snapshot
// whose stored state_hash is not the canonical hash of its state blob never
// becomes durable — it is refused with ErrSnapshotHashMismatch and nothing is
// written.
func TestWriteSnapshotRejectsHashMismatch(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-snaphash"

	head := appendN(t, s, stream, 2)
	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-snaphash"}`)
	snap.StateHash[0] ^= 0xff // corrupt the stored hash

	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); !errors.Is(err, ErrSnapshotHashMismatch) {
		t.Fatalf("WriteSnapshot with a mismatched hash = %v, want ErrSnapshotHashMismatch", err)
	}
	if _, ok, _ := s.LatestSnapshot(ctx, stream); ok {
		t.Fatalf("a hash-mismatched snapshot was persisted, want none")
	}
	if h, _ := s.Head(ctx, stream); h != head {
		t.Fatalf("head = %d after a refused snapshot, want %d (no anchor written)", h, head)
	}
}

// TestWriteSnapshotRequiresHeadCoverage proves a snapshot must cover exactly the
// committed head: a covered_seq that lags (or leads) the head is refused so the
// anchor never lands with a gap.
func TestWriteSnapshotRequiresHeadCoverage(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-snapcover"

	head := appendN(t, s, stream, 3)
	snap, anchor := makeSnap(t, stream, head-1, `{"root_id":"gcj-root-snapcover"}`) // covers 2, head is 3
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); !errors.Is(err, ErrWrongExpectedVersion) {
		t.Fatalf("WriteSnapshot with covered_seq < head = %v, want ErrWrongExpectedVersion", err)
	}
}

// TestSnapshotOptInAdditive proves snapshots are additive and opt-in: a stream
// that never snapshots has an empty snapshots table and Verifies exactly as a
// plain journal, and snapshotting adds only the anchor event — the pre-snapshot
// events are byte-identical.
func TestSnapshotOptInAdditive(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	s.RegisterEventType(testEngine, anchorType)
	const stream = "gcj-root-snapadd"

	head := appendN(t, s, stream, 3)
	before, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	var snapCount int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&snapCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapCount != 0 {
		t.Fatalf("snapshots without WriteSnapshot = %d, want 0 (opt-in)", snapCount)
	}
	if err := s.Verify(ctx, stream); err != nil {
		t.Fatalf("Verify plain journal = %v", err)
	}

	snap, anchor := makeSnap(t, stream, head, `{"root_id":"gcj-root-snapadd"}`)
	if _, err := s.WriteSnapshot(ctx, testEngine, 0, snap, anchor); err != nil {
		t.Fatalf("write snapshot: %v", err)
	}
	after, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if len(after) != len(before)+1 {
		t.Fatalf("events after snapshot = %d, want %d (one added anchor)", len(after), len(before)+1)
	}
	for i := range before {
		if string(after[i].Payload) != string(before[i].Payload) || after[i].ChainHash != before[i].ChainHash {
			t.Fatalf("event %d changed after snapshot — snapshotting is not additive", i)
		}
	}
}
