package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

var _ Claimer = (*PostgresStore)(nil)

// Claim atomically claims a bead for assignee via a row-locked compare-and-set on
// the assignee field: it succeeds (status -> in_progress, assignee -> caller) only
// when the bead is open/in_progress and currently unassigned, and is idempotent
// when the same assignee already holds it. It returns ok=false (a conflict, not an
// error) when a different assignee already holds the bead or the bead is closed,
// and ErrNotFound when the bead does not exist.
//
// It is the acquire-dual of [PostgresStore.ReleaseIfCurrent] and the Postgres peer
// of [SQLiteStore.Claim] — the capability the SQLite-tested infra/beads split never
// exercised, so a graph-class work bead relocated onto Postgres could not be
// claimed (the API claim path fell through to ErrClaimUnsupported and drained the
// worker). Unlike SQLite (whose single write connection serializes claims), Postgres
// serves concurrent connections, so single-winner is enforced by SELECT ... FOR
// UPDATE: the row lock queues competing claims on one BeginTx, and the loser
// re-reads the now-assigned row and returns ok=false cleanly rather than
// double-claiming.
func (s *PostgresStore) Claim(id, assignee string) (Bead, bool, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Bead{}, false, fmt.Errorf("postgres claim: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	// FOR UPDATE locks the bead row so a concurrent claim on the SAME id blocks
	// until this tx commits and then observes the updated assignee — the
	// multi-connection analog of SQLite's single-writer serialization.
	row := tx.QueryRowContext(ctx, `SELECT bead_json FROM beads WHERE id=$1 FOR UPDATE`, id)
	b, err := scanSQLiteBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Bead{}, false, fmt.Errorf("claiming bead %q: %w", id, err)
	}
	cur := strings.TrimSpace(b.Assignee)
	if cur != "" && cur != assignee {
		// Held by another worker — conflict, not an error.
		if err := tx.Commit(); err != nil {
			return Bead{}, false, fmt.Errorf("postgres claim: commit: %w", err)
		}
		return Bead{}, false, nil
	}
	if b.Status == "closed" {
		// Terminal work is not claimable.
		if err := tx.Commit(); err != nil {
			return Bead{}, false, fmt.Errorf("postgres claim: commit: %w", err)
		}
		return Bead{}, false, nil
	}
	b.Assignee = assignee
	b.Status = "in_progress"
	b.UpdatedAt = time.Now()
	if err := s.upsertBeadTx(ctx, tx, b); err != nil {
		return Bead{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Bead{}, false, fmt.Errorf("postgres claim: commit: %w", err)
	}
	return cloneBead(b), true, nil
}
