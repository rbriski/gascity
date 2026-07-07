package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// AcquireWriterLease acquires (or steals, if expired) the writer lease for
// streamID on behalf of holder, bumping the monotonic epoch by one. It performs
// the CAS inside a BEGIN IMMEDIATE transaction so the read-decide-write is
// race-safe and expiry is compared in Go (RFC3339Nano text is not
// lexicographically time-ordered across a missing fractional part). Returns
// ErrLeaseHeld when a different, unexpired holder owns the lease. Epochs never
// reset, so a re-acquisition always advances the fencing token.
func (s *Store) AcquireWriterLease(ctx context.Context, streamID, holder string, ttl time.Duration) (WriterLease, error) {
	if streamID == "" || holder == "" {
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease: empty stream id or holder")
	}
	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease: begin: %w", mapSQLiteBusy(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	now := time.Now().UTC()
	expiresAt := now.Add(ttl)
	expiresStr := expiresAt.Format(time.RFC3339Nano)

	var (
		curHolder  string
		curEpoch   uint64
		curExpires string
	)
	err = tx.QueryRowContext(ctx,
		`SELECT holder, epoch, expires_at FROM writer_lease WHERE stream_id = ?`,
		streamID,
	).Scan(&curHolder, &curEpoch, &curExpires)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO writer_lease(stream_id, holder, epoch, expires_at)
			 VALUES (?, ?, 1, ?)`,
			streamID, holder, expiresStr,
		); err != nil {
			return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q: insert: %w", streamID, err)
		}
		if err := tx.Commit(); err != nil {
			return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q: commit: %w", streamID, err)
		}
		return WriterLease{StreamID: streamID, Holder: holder, Epoch: 1, ExpiresAt: expiresAt}, nil
	case err != nil:
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q: read: %w", streamID, err)
	}

	if curHolder != holder && !expired(curExpires, now) {
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q held by %q until %s: %w", streamID, curHolder, curExpires, ErrLeaseHeld)
	}
	newEpoch := curEpoch + 1
	if _, err := tx.ExecContext(ctx,
		`UPDATE writer_lease SET holder = ?, epoch = ?, expires_at = ? WHERE stream_id = ?`,
		holder, newEpoch, expiresStr, streamID,
	); err != nil {
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q: update: %w", streamID, err)
	}
	if err := tx.Commit(); err != nil {
		return WriterLease{}, fmt.Errorf("graphstore: acquire lease %q: commit: %w", streamID, err)
	}
	return WriterLease{StreamID: streamID, Holder: holder, Epoch: newEpoch, ExpiresAt: expiresAt}, nil
}

// RenewWriterLease extends the lease expiry without changing the epoch. It
// succeeds only if the caller still holds the lease at the same epoch; otherwise
// the caller has been fenced and ErrLeaseFenced is returned.
func (s *Store) RenewWriterLease(ctx context.Context, lease WriterLease, ttl time.Duration) (WriterLease, error) {
	expiresAt := time.Now().UTC().Add(ttl)
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE writer_lease SET expires_at = ?
		  WHERE stream_id = ? AND holder = ? AND epoch = ?`,
		expiresAt.Format(time.RFC3339Nano), lease.StreamID, lease.Holder, lease.Epoch,
	)
	if err != nil {
		return WriterLease{}, fmt.Errorf("graphstore: renew lease %q: %w", lease.StreamID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return WriterLease{}, fmt.Errorf("graphstore: renew lease %q: rows: %w", lease.StreamID, err)
	}
	if n == 0 {
		return WriterLease{}, fmt.Errorf("graphstore: renew lease %q at epoch %d: %w", lease.StreamID, lease.Epoch, ErrLeaseFenced)
	}
	lease.ExpiresAt = expiresAt
	return lease, nil
}

// ReleaseWriterLease releases the lease if the caller still holds it at its
// epoch by expiring it in place — the row (and its epoch counter) is preserved so
// the next acquisition still advances the fencing epoch (never reset, 01 §2.3).
// Releasing a lease already stolen by another holder is a no-op and returns nil.
func (s *Store) ReleaseWriterLease(ctx context.Context, lease WriterLease) error {
	if _, err := s.writeDB.ExecContext(ctx,
		`UPDATE writer_lease SET expires_at = ?
		  WHERE stream_id = ? AND holder = ? AND epoch = ?`,
		time.Unix(0, 0).UTC().Format(time.RFC3339Nano), lease.StreamID, lease.Holder, lease.Epoch,
	); err != nil {
		return fmt.Errorf("graphstore: release lease %q: %w", lease.StreamID, err)
	}
	return nil
}

// expired reports whether an RFC3339Nano expiry string is at or before now.
// A malformed timestamp is treated as expired (fail-open on liveness only;
// safety never depends on the clock).
func expired(expiresStr string, now time.Time) bool {
	t, err := time.Parse(time.RFC3339Nano, expiresStr)
	if err != nil {
		return true
	}
	return !t.After(now)
}
