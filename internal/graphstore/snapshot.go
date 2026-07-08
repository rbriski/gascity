package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// WriteSnapshot durably anchors a fold-state snapshot for snap.StreamID and
// appends its `snapshot.anchored` marker to the journal in ONE transaction
// (13-p4 §4). The snapshots row and the anchor event are indivisible: a resume
// that finds the anchor in the log always finds the covered blob in the table,
// and vice versa. anchor is the engine-typed marker event (its type must be a
// registered vocabulary entry); the store stamps its seq at head+1 and chains it
// exactly like an ordinary append.
//
// The R-SNAP-WRITE gate: the stored state_hash must be the canonical hash of the
// state blob (a corrupted snapshot never becomes durable), the snapshot must
// cover exactly the committed head (covered_seq == head, so the anchor lands at
// head+1 with no gap), and the lease epoch must not be fenced. The snapshots row
// is written through the snapshot_write_gate so a non-WriteSnapshot writer hits a
// loud ABORT (DET-T-18). The crashpoint the blueprint names
// (BetweenSnapshotWriteAndAnchor) sits BEFORE this transaction: a crash there
// loses nothing but an unanchored in-memory attempt.
//
// It returns the seq assigned to the anchor event so the caller can fold it
// forward (snapshot.anchored folds to a no-op delta) and advance its head.
func (s *Store) WriteSnapshot(ctx context.Context, engine string, leaseEpoch uint64, snap fold.Snapshot, anchor JournalEvent) (uint64, error) {
	if snap.StreamID == "" {
		return 0, fmt.Errorf("graphstore: write snapshot: empty stream id")
	}
	if snap.CoveredSeq == 0 {
		return 0, fmt.Errorf("graphstore: write snapshot %q: covered_seq 0 (nothing to anchor)", snap.StreamID)
	}
	if len(snap.State) == 0 {
		return 0, fmt.Errorf("graphstore: write snapshot %q: empty state blob", snap.StreamID)
	}
	if canon.Hash(snap.State) != snap.StateHash {
		return 0, fmt.Errorf("graphstore: write snapshot %q@%d: %w", snap.StreamID, snap.CoveredSeq, ErrSnapshotHashMismatch)
	}
	if anchor.Payload == nil {
		return 0, fmt.Errorf("graphstore: write snapshot %q: nil anchor payload", snap.StreamID)
	}
	if !s.isRegistered(engine, anchor.Type) {
		return 0, fmt.Errorf("graphstore: write snapshot %q: anchor event (%s, %s): %w", snap.StreamID, engine, anchor.Type, ErrUnknownEventType)
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("graphstore: write snapshot: begin: %w", s.dialect.mapError(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Serialize writers of this stream as the FIRST statement of the txn. No-op on
	// SQLite; a per-stream pg_advisory_xact_lock on Postgres so the covered_seq ==
	// head gate and the head+1 anchor append can't rot between the check and the
	// commit — a concurrent Append blocks until this snapshot commits (or this
	// blocks until it does), then the gate decision is authoritative. A lock_timeout
	// maps to ErrBusy.
	if err := s.dialect.lockStream(ctx, tx, snap.StreamID); err != nil {
		return 0, fmt.Errorf("graphstore: write snapshot %q: lock stream: %w", snap.StreamID, s.dialect.mapError(err))
	}

	head, prevChain, err := headAndChain(ctx, tx, snap.StreamID)
	if err != nil {
		return 0, err
	}
	if head != snap.CoveredSeq {
		return 0, fmt.Errorf("graphstore: write snapshot %q: covered_seq %d != head %d: %w", snap.StreamID, snap.CoveredSeq, head, ErrWrongExpectedVersion)
	}
	if err := checkLeaseEpoch(ctx, tx, snap.StreamID, leaseEpoch); err != nil {
		return 0, err
	}

	// Insert the snapshots row through the write gate.
	if err := openSnapshotGate(ctx, tx, s.dialect); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO snapshots
		   (stream_id, covered_seq, engine, reducer_version, snapshot_format_version,
		    state_hash, state, cut_chain_hash, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, NULL, ?)`,
		snap.StreamID, snap.CoveredSeq, snap.Engine, snap.ReducerVersion,
		snap.SnapshotFormatVersion, snap.StateHash[:], snap.State,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, errors.Join(
			fmt.Errorf("graphstore: write snapshot %q@%d: insert: %w", snap.StreamID, snap.CoveredSeq, s.dialect.mapError(err)),
			closeSnapshotGate(ctx, tx, s.dialect))
	}
	if err := closeSnapshotGate(ctx, tx, s.dialect); err != nil {
		return 0, err
	}

	// Append the anchor event at head+1, chained from the head row, in the SAME
	// transaction so the anchor and the blob are indivisible.
	seq := head + 1
	payloadHash := canon.Hash(anchor.Payload)
	chain := chainHash(prevChain, snap.StreamID, seq, engine, anchor.Type, anchor.Substream, anchor.IRContractVersion, payloadHash)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO journal
		   (stream_id, seq, substream, engine, type, ir_contract_version,
		    idem_token, payload, payload_hash, chain_hash, lease_epoch, appended_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snap.StreamID, seq, anchor.Substream, engine, anchor.Type, anchor.IRContractVersion,
		nullableToken(anchor.IdemToken), anchor.Payload, payloadHash[:], chain[:], leaseEpoch,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		return 0, fmt.Errorf("graphstore: write snapshot %q: anchor append seq %d: %w", snap.StreamID, seq, s.dialect.mapError(err))
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("graphstore: write snapshot %q: commit: %w", snap.StreamID, s.dialect.mapError(err))
	}
	return seq, nil
}

// LatestSnapshot returns the highest-covered durable snapshot for streamID, and
// false when the stream has none. It is the resume entry point: Resume loads this
// and folds the surviving tail after CoveredSeq (R-RESUME).
func (s *Store) LatestSnapshot(ctx context.Context, streamID string) (fold.Snapshot, bool, error) {
	var (
		snap      fold.Snapshot
		stateHash []byte
	)
	snap.StreamID = streamID
	err := s.readDB.QueryRowContext(ctx,
		`SELECT covered_seq, engine, reducer_version, snapshot_format_version, state_hash, state
		   FROM snapshots WHERE stream_id = ? ORDER BY covered_seq DESC LIMIT 1`,
		streamID,
	).Scan(&snap.CoveredSeq, &snap.Engine, &snap.ReducerVersion, &snap.SnapshotFormatVersion, &stateHash, &snap.State)
	if errors.Is(err, sql.ErrNoRows) {
		return fold.Snapshot{}, false, nil
	}
	if err != nil {
		return fold.Snapshot{}, false, fmt.Errorf("graphstore: latest snapshot %q: %w", streamID, err)
	}
	copy(snap.StateHash[:], stateHash)
	return snap, true, nil
}

// openSnapshotGate and closeSnapshotGate toggle the snapshot write-closure gate.
// The gate is a SINGLETON row (WHERE singleton = 0) shared across every stream, so
// on Postgres two different streams' snapshot/truncate transactions contend on this
// one row lock even though they hold different per-stream advisory locks — a
// cross-stream blocking path that partially serializes snapshot/truncate/projection.
// A lock_timeout on that contention is routed through d.mapError so it surfaces as
// the retryable ErrBusy, not a raw 55P03 the transient classifier would treat as
// hard. SQLite is unaffected: its mapError is the identical SQLITE_BUSY mapping.
func openSnapshotGate(ctx context.Context, tx *sql.Tx, d dialect) error {
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		return fmt.Errorf("graphstore: opening snapshot write gate: %w", d.mapError(err))
	}
	return nil
}

func closeSnapshotGate(ctx context.Context, tx *sql.Tx, d dialect) error {
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		return fmt.Errorf("graphstore: closing snapshot write gate: %w", d.mapError(err))
	}
	return nil
}
