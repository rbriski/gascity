package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

// TruncateBelowAnchor drops every journal row at seq <= anchorSeq for streamID,
// once a durable snapshot covers that seq (13-p4 §4). anchorSeq MUST be the
// covered_seq of an existing snapshot: that snapshot becomes the new chain anchor
// after the cut, so Resume can rebuild the truncated prefix from it and Verify can
// walk the surviving tail from the recorded cut_chain_hash. Without such a
// snapshot the truncation is refused (ErrNoCoveringSnapshot) — the journal is
// never truncated past its latest durable anchor.
//
// It runs in one transaction: record cut_chain_hash (the chain_hash of the last
// deleted row, seq == anchorSeq) on the covering snapshot row through the
// snapshot write gate, open the retention_gate for seq <= anchorSeq, DELETE, then
// close the gate (drop the retention_gate row) so no standing deletion permission
// remains. The journal's append-only trigger admits the DELETE only while the
// retention_gate covers the seq, so an ungated deleter still aborts loudly.
//
// It returns the number of rows deleted. Re-truncating an already-truncated
// prefix is an idempotent no-op (0 deleted).
func (s *Store) TruncateBelowAnchor(ctx context.Context, streamID string, anchorSeq uint64) (int64, error) {
	if streamID == "" {
		return 0, fmt.Errorf("graphstore: truncate: empty stream id")
	}
	if anchorSeq == 0 {
		return 0, fmt.Errorf("graphstore: truncate %q: anchor seq 0", streamID)
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("graphstore: truncate: begin: %w", s.dialect.mapError(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// Serialize writers of this stream as the FIRST statement of the txn. No-op on
	// SQLite; a per-stream pg_advisory_xact_lock on Postgres so the covering-snapshot
	// read, the cut-anchor record, and the retention-gated DELETE serialize against a
	// concurrent Append on the same stream (it shares the snapshot-row surface with
	// WriteSnapshot). A lock_timeout maps to ErrBusy.
	if err := s.dialect.lockStream(ctx, tx, streamID); err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: lock stream: %w", streamID, s.dialect.mapError(err))
	}

	// A snapshot must cover exactly anchorSeq. This is also the "never past the
	// latest durable snapshot" guard: anchorSeq is required to BE a covered_seq,
	// so it can never exceed the newest one.
	//
	// The blob is read alongside so it can be verified BEFORE the prefix is
	// destroyed (H2). Once the prefix is gone, this snapshot is the ONLY source a
	// resume can rebuild from; if its blob has rotted (state_hash no longer hashes
	// its bytes), deleting the prefix would leave the stream unrecoverable. So a
	// mismatch ABORTS the truncation — the prefix is preserved and resume can still
	// rebuild from the journal.
	var (
		snapState []byte
		snapHash  []byte
	)
	err = tx.QueryRowContext(ctx,
		`SELECT state, state_hash FROM snapshots WHERE stream_id = ? AND covered_seq = ?`, streamID, anchorSeq,
	).Scan(&snapState, &snapHash)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("graphstore: truncate %q at %d: %w", streamID, anchorSeq, ErrNoCoveringSnapshot)
	}
	if err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: reading covering snapshot: %w", streamID, s.dialect.mapError(err))
	}
	var have [32]byte
	copy(have[:], snapHash)
	if canon.Hash(snapState) != have {
		return 0, fmt.Errorf("graphstore: truncate %q at %d: covering snapshot: %w", streamID, anchorSeq, ErrSnapshotHashMismatch)
	}

	// cut_chain_hash is the chain_hash of the last deleted row (seq == anchorSeq):
	// the prev the surviving head (anchorSeq+1) was chained from, so Verify can
	// walk across the cut.
	var cut []byte
	err = tx.QueryRowContext(ctx,
		`SELECT chain_hash FROM journal WHERE stream_id = ? AND seq = ?`, streamID, anchorSeq,
	).Scan(&cut)
	if errors.Is(err, sql.ErrNoRows) {
		// Already truncated at/above anchorSeq: nothing to delete. Idempotent.
		if err := tx.Commit(); err != nil {
			return 0, fmt.Errorf("graphstore: truncate %q: commit (no-op): %w", streamID, s.dialect.mapError(err))
		}
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: reading cut anchor at %d: %w", streamID, anchorSeq, s.dialect.mapError(err))
	}

	// Record the cut anchor on the covering snapshot row (write-gated).
	if err := openSnapshotGate(ctx, tx, s.dialect); err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE snapshots SET cut_chain_hash = ? WHERE stream_id = ? AND covered_seq = ?`,
		cut, streamID, anchorSeq,
	); err != nil {
		return 0, errors.Join(
			fmt.Errorf("graphstore: truncate %q: recording cut anchor: %w", streamID, s.dialect.mapError(err)),
			closeSnapshotGate(ctx, tx, s.dialect))
	}
	if err := closeSnapshotGate(ctx, tx, s.dialect); err != nil {
		return 0, err
	}

	// Open the retention gate for seq <= anchorSeq, delete, then close it.
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO retention_gate(stream_id, max_seq) VALUES (?, ?)
		 ON CONFLICT(stream_id) DO UPDATE SET max_seq = excluded.max_seq`,
		streamID, anchorSeq,
	); err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: opening retention gate: %w", streamID, s.dialect.mapError(err))
	}
	res, err := tx.ExecContext(ctx,
		`DELETE FROM journal WHERE stream_id = ? AND seq <= ?`, streamID, anchorSeq,
	)
	if err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: deleting seq <= %d: %w", streamID, anchorSeq, s.dialect.mapError(err))
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM retention_gate WHERE stream_id = ?`, streamID,
	); err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: closing retention gate: %w", streamID, s.dialect.mapError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: rows affected: %w", streamID, err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("graphstore: truncate %q: commit: %w", streamID, s.dialect.mapError(err))
	}
	return n, nil
}

// cutChainHashAt returns the cut_chain_hash recorded on the snapshot covering
// coveredSeq, and false when the snapshot is absent or was never used as a
// truncation anchor. Verify consults it to resume the chain walk across a cut.
func (s *Store) cutChainHashAt(ctx context.Context, streamID string, coveredSeq uint64) ([32]byte, bool, error) {
	var cut []byte
	err := s.readDB.QueryRowContext(ctx,
		`SELECT cut_chain_hash FROM snapshots WHERE stream_id = ? AND covered_seq = ?`,
		streamID, coveredSeq,
	).Scan(&cut)
	if errors.Is(err, sql.ErrNoRows) || cut == nil {
		return [32]byte{}, false, nil
	}
	if err != nil {
		return [32]byte{}, false, fmt.Errorf("graphstore: cut anchor %q@%d: %w", streamID, coveredSeq, err)
	}
	var out [32]byte
	copy(out[:], cut)
	return out, true, nil
}
