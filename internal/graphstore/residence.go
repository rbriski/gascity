package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Residence states. The absence of a graph_residence row is the third,
// default state (∅ = legacy-resident); only the two non-default states carry a
// row.
const (
	// ResidenceMigrating marks a root whose subgraph is being copied into the
	// journal leg. Reads route legacy, the half-copied journal rows stay hidden
	// (JournalStore residence-visibility gate), and conflicting controller writes
	// are blocked. Carries the migration's fence epoch.
	ResidenceMigrating = "migrating"
	// ResidenceJournal marks a root the journal leg now owns authoritatively. Set
	// only by the post-re-verify CAS flip, and permanent thereafter (a migrated or
	// journal-born root never reverts to legacy).
	ResidenceJournal = "journal"
)

// ResidenceRecord is one graph_residence row.
type ResidenceRecord struct {
	RootID     string
	State      string
	FenceEpoch int64
	UpdatedAt  string
}

// ResidenceOf returns the residence state of rootID. present is false when no row
// exists (the default ∅ = legacy-resident); callers MUST treat a hard error as
// unknowable residence and never flatten it into "legacy". The read is served off
// the pooled WAL read handle, so it observes the last committed CAS transition.
func (s *Store) ResidenceOf(ctx context.Context, rootID string) (state string, fenceEpoch int64, present bool, err error) {
	row := s.readDB.QueryRowContext(ctx,
		`SELECT state, fence_epoch FROM graph_residence WHERE root_id = ?`, rootID)
	if err := row.Scan(&state, &fenceEpoch); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, nil
		}
		return "", 0, false, fmt.Errorf("graphstore: reading residence of %q: %w", rootID, s.dialect.mapError(err))
	}
	return state, fenceEpoch, true, nil
}

// BeginResidenceMigration is the CAS ∅ → migrating(fenceEpoch) transition (09a
// §A-2 step 1a). It succeeds (returns true) only when no row exists for rootID;
// a row in ANY state (migrating or journal) loses the CAS and returns false, so a
// second migrator, or a re-run over an already-journal root, is a loud no-win
// rather than a silent clobber. The write serializes on the single write
// connection.
func (s *Store) BeginResidenceMigration(ctx context.Context, rootID string, fenceEpoch int64, now string) (bool, error) {
	res, err := s.writeDB.ExecContext(ctx,
		`INSERT INTO graph_residence(root_id, state, fence_epoch, updated_at)
		 VALUES(?, 'migrating', ?, ?)
		 ON CONFLICT(root_id) DO NOTHING`,
		rootID, fenceEpoch, now)
	if err != nil {
		return false, fmt.Errorf("graphstore: begin residence migration %q: %w", rootID, s.dialect.mapError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("graphstore: begin residence migration %q rows: %w", rootID, err)
	}
	return n > 0, nil
}

// FlipResidenceToJournal is the CAS migrating(fenceEpoch) → journal transition
// (09a §A-2 step 5, the atomic cutover point). It succeeds only when the row is
// still migrating at the SAME fence epoch, so a stale flip (the epoch moved on)
// or a flip of an already-reverted root loses. The write serializes on the single
// write connection.
func (s *Store) FlipResidenceToJournal(ctx context.Context, rootID string, fenceEpoch int64, now string) (bool, error) {
	res, err := s.writeDB.ExecContext(ctx,
		`UPDATE graph_residence SET state = 'journal', updated_at = ?
		 WHERE root_id = ? AND state = 'migrating' AND fence_epoch = ?`,
		now, rootID, fenceEpoch)
	if err != nil {
		return false, fmt.Errorf("graphstore: flip residence %q: %w", rootID, s.dialect.mapError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("graphstore: flip residence %q rows: %w", rootID, err)
	}
	return n > 0, nil
}

// RevertResidence deletes a migrating row IFF it is still migrating at the given
// fence epoch, returning the root to ∅ (legacy). The epoch guard is the fence
// against a concurrent migrator: an invocation may only revert the migration it
// itself minted (or the exact stale epoch it read under --force-recover), never a
// DIFFERENT migrator's in-flight row. A journal row is permanent, and a row at a
// foreign epoch is not this invocation's to drop, so both are a no-op (return
// false) that never demotes an authoritative copy or stomps a live sibling. This
// is the abort/recover path (09a §A-2 step 4): the journal copy for this epoch was
// never authoritative, so dropping the record is safe and loses nothing.
func (s *Store) RevertResidence(ctx context.Context, rootID string, fenceEpoch int64) (bool, error) {
	res, err := s.writeDB.ExecContext(ctx,
		`DELETE FROM graph_residence WHERE root_id = ? AND state = 'migrating' AND fence_epoch = ?`,
		rootID, fenceEpoch)
	if err != nil {
		return false, fmt.Errorf("graphstore: revert residence %q: %w", rootID, s.dialect.mapError(err))
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("graphstore: revert residence %q rows: %w", rootID, err)
	}
	return n > 0, nil
}

// MigratingRoots returns the set of root ids currently in the migrating state
// (usually empty). The router consults it to decide whether any controller write
// needs the ErrRootMigrating quarantine check; an empty result (the common case)
// lets the router skip the per-write root resolution entirely.
func (s *Store) MigratingRoots(ctx context.Context) ([]string, error) {
	rows, err := s.readDB.QueryContext(ctx,
		`SELECT root_id FROM graph_residence WHERE state = 'migrating' ORDER BY root_id`)
	if err != nil {
		return nil, fmt.Errorf("graphstore: listing migrating roots: %w", s.dialect.mapError(err))
	}
	defer func() { _ = rows.Close() }()
	var roots []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("graphstore: scanning migrating root: %w", err)
		}
		roots = append(roots, id)
	}
	return roots, rows.Err()
}
