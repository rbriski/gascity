package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// Residence states, re-exported from graphstore so beads-layer and CLI callers
// need not import the storage package to compare a ResidenceOf result. The
// absence of a record is the third, default state (∅ = legacy-resident).
const (
	ResidenceStateMigrating = graphstore.ResidenceMigrating
	ResidenceStateJournal   = graphstore.ResidenceJournal
)

// ErrRootMigrating reports that a controller-path write (any router-routed
// mutation, INCLUDING a child Create under the root) targeted a bead whose root
// is being copied into the journal leg (residence state `migrating`). The copy is
// in flight and not yet authoritative, so the write is BLOCKED — loud and
// retryable after the migration flips or reverts — rather than landing on the old
// leg where it would be silently absent from the journal copy (09a §A-2 step 1a,
// the quarantine half of the fence).
//
// Scope (honest narrowing): this fence covers ONLY router-routed writers.
// External `bd` writers bypass the router and so bypass this guard entirely —
// they are NOT fenced. The migration DETECTS them, it does not prevent them: the
// re-verify re-read (step 6) catches a write that lands up to the flip, and the
// pre-tombstone delta guard (step 8) catches one that lands in the residual
// re-verify→flip window and converts it into a LOUD alarm (no tombstone, non-zero
// exit) instead of silent loss. Closing the window entirely — fencing external
// writers — is the P4 quiesce/settlement path, not this slice.
var ErrRootMigrating = errors.New("root is migrating to the journal leg; write blocked (retry after migration)")

// ResidenceStore is the minimal residence capability the residence-routing router
// consults on the hot write path: it needs only the set of roots currently
// migrating to decide whether a write needs the ErrRootMigrating quarantine
// check. An empty result (the common case) lets the router skip per-write root
// resolution entirely, so an opted city with no active migration keeps its P1.5
// write cost.
type ResidenceStore interface {
	// MigratingRoots returns the root ids currently in the `migrating` state.
	MigratingRoots(ctx context.Context) ([]string, error)
}

// ResidenceMigrationStore is the full residence + copy surface the
// `gc migrate graph-journal` state machine drives. Every method is a durable,
// serialized step of the strand migration (park → copy → verify → flip →
// tombstone), so the record and the staged copy together form a crash-safe
// checkpoint: a re-run inspects ResidenceOf and resumes or reverts.
type ResidenceMigrationStore interface {
	ResidenceStore

	// ResidenceOf returns rootID's residence state; present is false for the
	// default ∅ (legacy-resident). A hard error is unknowable residence, never
	// "legacy".
	ResidenceOf(ctx context.Context, rootID string) (state string, fenceEpoch int64, present bool, err error)

	// BeginResidenceMigration is the CAS ∅ → migrating(fenceEpoch) park step. It
	// returns false when a row already exists (a competing migrator, or an
	// already-journal root), never clobbering.
	BeginResidenceMigration(ctx context.Context, rootID string, fenceEpoch int64) (bool, error)

	// FlipResidenceToJournal is the CAS migrating(fenceEpoch) → journal cutover.
	// It returns false unless the row is still migrating at the same epoch.
	FlipResidenceToJournal(ctx context.Context, rootID string, fenceEpoch int64) (bool, error)

	// RevertResidence drops a migrating row back to ∅ (abort/recover) IFF it is
	// still migrating at fenceEpoch. The epoch guard fences a concurrent migrator:
	// a revert only clears the migration THIS invocation minted (or the exact stale
	// epoch it read under --force-recover), never a DIFFERENT migrator's row. A
	// journal row and a foreign-epoch row are both no-ops (false).
	RevertResidence(ctx context.Context, rootID string, fenceEpoch int64) (bool, error)

	// ImportSubtree copies subtree into the journal leg PRESERVING each bead's id
	// (09a §A-2 step 2: no rebless, no id mapping — the residence record is what
	// moves). edgeMeta carries each dependency edge's raw metadata blob (keyed by
	// EdgeKey) so waits-for gate metadata survives the copy; an absent key imports
	// the edge with empty metadata. Imported rows are tagged with stream_id=rootID
	// so they stay hidden from façade reads until the flip and can be discarded
	// wholesale on abort.
	ImportSubtree(ctx context.Context, subtree []Bead, edgeMeta map[EdgeKey]string, rootID string) error

	// StagedRootBeads reads back the imported copy for rootID, BYPASSING the
	// residence-visibility gate, so fold-verify can hash the staged journal copy
	// while it is still hidden from every façade reader.
	StagedRootBeads(ctx context.Context, rootID string) ([]Bead, error)

	// DiscardRoot deletes every imported row tagged stream_id=rootID IFF the
	// residence row is still migrating at fenceEpoch (the abort/recover cleanup).
	// The epoch guard is the BLOCKER-2 fence: an invocation may only discard the
	// staged copy of its OWN migration epoch — a foreign or already-flipped epoch is
	// a no-op, so a losing/late migrator can never delete another migrator's rows
	// (including a sibling's flipped-authoritative journal copy). The check and the
	// delete run in one transaction on the single write connection.
	DiscardRoot(ctx context.Context, rootID string, fenceEpoch int64) error
}

// ResidenceMigrationHandleProvider lets a wrapper (policy/caching) expose the
// backing store's residence-migration capability without claiming the interface
// globally, mirroring AppendLogHandleProvider.
type ResidenceMigrationHandleProvider interface {
	ResidenceMigrationHandle() (ResidenceMigrationStore, bool)
}

// ResidenceMigrationStoreFor returns the full residence-migration capability for
// store when available, reaching through wrapper handle providers. A store
// without it returns (nil, false) — the honest "absent" signal.
func ResidenceMigrationStoreFor(store Store) (ResidenceMigrationStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(ResidenceMigrationStore); ok {
		return s, true
	}
	if p, ok := store.(ResidenceMigrationHandleProvider); ok {
		return p.ResidenceMigrationHandle()
	}
	return nil, false
}

// ResidenceStoreFor returns the minimal residence capability for store. Every
// ResidenceMigrationStore is a ResidenceStore, so this reaches the same backing
// store the router's write-gate needs.
func ResidenceStoreFor(store Store) (ResidenceStore, bool) {
	if store == nil {
		return nil, false
	}
	if m, ok := ResidenceMigrationStoreFor(store); ok {
		return m, true
	}
	if s, ok := store.(ResidenceStore); ok {
		return s, true
	}
	return nil, false
}

// --- JournalStore forwarding impl ------------------------------------------

var (
	_ ResidenceStore          = (*JournalStore)(nil)
	_ ResidenceMigrationStore = (*JournalStore)(nil)
	_ EdgeMetadataReader      = (*JournalStore)(nil)
)

// MigratingRoots forwards to the underlying journal engine's residence table.
func (s *JournalStore) MigratingRoots(ctx context.Context) ([]string, error) {
	return s.gs.MigratingRoots(ctx)
}

// ResidenceOf forwards to the underlying journal engine's residence table.
func (s *JournalStore) ResidenceOf(ctx context.Context, rootID string) (string, int64, bool, error) {
	return s.gs.ResidenceOf(ctx, rootID)
}

// BeginResidenceMigration forwards the CAS ∅ → migrating park step.
func (s *JournalStore) BeginResidenceMigration(ctx context.Context, rootID string, fenceEpoch int64) (bool, error) {
	return s.gs.BeginResidenceMigration(ctx, rootID, fenceEpoch, journalFormatTime(journalNow()))
}

// FlipResidenceToJournal forwards the CAS migrating → journal cutover.
func (s *JournalStore) FlipResidenceToJournal(ctx context.Context, rootID string, fenceEpoch int64) (bool, error) {
	return s.gs.FlipResidenceToJournal(ctx, rootID, fenceEpoch, journalFormatTime(journalNow()))
}

// RevertResidence forwards the epoch-guarded migrating → ∅ revert.
func (s *JournalStore) RevertResidence(ctx context.Context, rootID string, fenceEpoch int64) (bool, error) {
	return s.gs.RevertResidence(ctx, rootID, fenceEpoch)
}

// ImportSubtree writes subtree into the journal leg preserving ids and tagging
// every node stream_id=rootID (so the residence-visibility gate hides the copy
// until the flip). Nodes land before edges so the edges FK (from_id → nodes)
// always resolves; an edge whose target is outside the subtree is left to dangle,
// which correctly blocks its dependent (D-4), exactly as it did on the old leg.
// The whole copy commits in one transaction.
func (s *JournalStore) ImportSubtree(ctx context.Context, subtree []Bead, edgeMeta map[EdgeKey]string, rootID string) error {
	if rootID == "" {
		return fmt.Errorf("journal store: import subtree: empty root id")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		for _, b := range subtree {
			if err := journalInsertImportedNode(ctx, tx, b, rootID); err != nil {
				return err
			}
		}
		for _, b := range subtree {
			if err := journalReplaceLabels(ctx, tx, b.ID, b.Labels); err != nil {
				return err
			}
			if err := journalMergeMetadata(ctx, tx, b.ID, b.Metadata); err != nil {
				return err
			}
			for _, dep := range b.Dependencies {
				if dep.DependsOnID == "" {
					continue
				}
				meta := edgeMeta[EdgeKey{FromID: b.ID, ToID: dep.DependsOnID, DepType: dep.Type}]
				if err := journalInsertEdge(ctx, tx, b.ID, dep.DependsOnID, dep.Type, meta); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// journalInsertImportedNode inserts one migrated bead with its id preserved,
// fold_owned=0 (façade-owned), and stream_id=rootID (the residence tag). It
// rejects an id shaped like this store's own mint (gcg-j<seq>): a legacy source
// id is always gcg-<other>, so a gcg-j* id can only be a corrupt or already-minted
// value, and importing one at or above the mint counter would permanently wedge
// every future Create (mintID would re-issue a colliding id). Rejecting loudly is
// the safe choice — the alternative, silently advancing the counter, hides a real
// source-data defect.
func journalInsertImportedNode(ctx context.Context, tx *sql.Tx, b Bead, rootID string) error {
	if b.ID == "" {
		return fmt.Errorf("journal store: import subtree: bead with empty id")
	}
	if journalIsMintShapedID(b.ID) {
		return fmt.Errorf("journal store: import subtree: source id %q has this store's mint shape %s-%s<seq>; a legacy id must not — refusing to avoid wedging the mint counter", b.ID, journalIDPrefix, journalIDMarker)
	}
	status := b.Status
	if status == "" {
		status = "open"
	}
	beadType := b.Type
	if beadType == "" {
		beadType = "task"
	}
	createdAt := b.CreatedAt
	if createdAt.IsZero() {
		createdAt = journalNow()
	}
	updatedAt := b.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = createdAt
	}
	tier := journalTierFromBead(b)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes
		  (id, title, status, bead_type, priority, description, assignee, from_actor,
		   parent_id, ref, created_at, updated_at, defer_until, storage_tier, fold_owned, stream_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?)`,
		b.ID, b.Title, status, beadType, journalNullableInt(b.Priority), b.Description, b.Assignee, b.From,
		b.ParentID, b.Ref, journalFormatTime(createdAt), journalFormatTime(updatedAt),
		journalDeferArg(b.DeferUntil), tier, rootID,
	); err != nil {
		return fmt.Errorf("journal store: importing bead %q: %w", b.ID, err)
	}
	return nil
}

// StagedRootBeads reads the imported copy for rootID directly, bypassing the
// residence-visibility gate that hides `migrating` rows from every façade read.
// Fold-verify hashes this to compare the staged journal copy against the old-leg
// snapshot before the flip.
func (s *JournalStore) StagedRootBeads(ctx context.Context, rootID string) ([]Bead, error) {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("journal store: begin staged read: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot
	rows, err := tx.QueryContext(ctx,
		"SELECT "+journalNodeColumns+" FROM nodes n WHERE n.fold_owned = 0 AND n.stream_id = ?",
		rootID)
	if err != nil {
		return nil, fmt.Errorf("journal store: querying staged root %q: %w", rootID, err)
	}
	beads, err := scanBeadRows(rows)
	if err != nil {
		return nil, err
	}
	for i := range beads {
		if err := s.hydrateChildren(ctx, tx, &beads[i]); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("journal store: commit staged read: %w", err)
	}
	return beads, nil
}

// DiscardRoot deletes every imported row tagged stream_id=rootID IFF the
// residence row is still migrating at fenceEpoch. The residence check and the
// delete run in ONE transaction on the single write connection, so the guard is
// atomic: a losing or late migrator whose epoch no longer owns the migrating row
// (it was reverted, re-parked at a new epoch, or already flipped to journal)
// deletes nothing. This is the BLOCKER-2 fence — without it a stale flip-lost
// cleanup, or a second migrator's step-0 recovery, would blow away a DIFFERENT
// migrator's rows, including a sibling's flipped-authoritative journal copy (those
// rows keep stream_id=rootID after the flip). Deleting a fold_owned=0 node
// cascades its node_labels, node_metadata, and outbound edges via ON DELETE
// CASCADE. graph_residence and nodes share this store's single database, so both
// statements observe the same committed state.
func (s *JournalStore) DiscardRoot(ctx context.Context, rootID string, fenceEpoch int64) error {
	if rootID == "" {
		return fmt.Errorf("journal store: discard root: empty root id")
	}
	return s.withTx(ctx, func(tx *sql.Tx) error {
		var one int
		err := tx.QueryRowContext(ctx,
			`SELECT 1 FROM graph_residence WHERE root_id = ? AND state = 'migrating' AND fence_epoch = ?`,
			rootID, fenceEpoch).Scan(&one)
		if errors.Is(err, sql.ErrNoRows) {
			// Not this invocation's epoch to discard: refuse to delete another
			// migrator's (or a flipped root's) rows.
			return nil
		}
		if err != nil {
			return fmt.Errorf("journal store: discard root %q: guarding epoch: %w", rootID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM nodes WHERE stream_id = ? AND fold_owned = 0`, rootID); err != nil {
			return fmt.Errorf("journal store: discarding staged root %q: %w", rootID, err)
		}
		return nil
	})
}

// EdgeMetadata returns the raw metadata blob on the dependency edge
// fromID -> toID of the given type, or "" when the edge is absent or carries no
// metadata. It reads the journal leg's own edges table, so the strand migration's
// pre-tombstone delta guard can hash the journal (authoritative) subtree with the
// same edge-metadata fidelity as the legacy side.
func (s *JournalStore) EdgeMetadata(fromID, toID, depType string) (string, error) {
	if depType == "" {
		depType = "blocks"
	}
	var meta string
	err := s.rdb.QueryRowContext(context.Background(),
		`SELECT metadata FROM edges WHERE from_id = ? AND to_id = ? AND dep_type = ?`,
		fromID, toID, depType).Scan(&meta)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("journal store: reading edge metadata %s->%s: %w", fromID, toID, err)
	}
	return meta, nil
}
