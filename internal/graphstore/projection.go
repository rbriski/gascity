package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// ErrProjectionIDCollision reports that a fold delta tried to upsert a node whose
// id already names a façade-minted (fold_owned=0) row. The fold path owns
// fold_owned=1 rows only; adopting a façade row would silently rewrite it
// fold_owned=1 and write-close the beads.Store's own bead, so the applier refuses
// loudly instead (I-14).
var ErrProjectionIDCollision = errors.New("fold node id collides with a façade-owned (fold_owned=0) row")

// ApplyDelta writes one fold Delta to the Tier-A projection tables inside tx,
// stamping every node row fold_owned=1 (I-14). It runs in the caller's write
// transaction so the projection lands atomically with the journal append
// (I-13): projection lag is identically zero.
//
// The Tier-A tables are write-closed. ApplyDelta opens the tier_a_write_gate at
// the start of its work and closes it before returning, so the fold-owned
// triggers admit these writes; any writer that does NOT open the gate (i.e. is
// not the fold applier) hits a loud ABORT (DET-T-18).
//
// Gate contract: ApplyDelta always closes the gate before it returns, on both
// the success and the error path. The caller owns tx and decides whether to
// commit or roll back; ApplyDelta never leaves the gate committed-open, so a
// caller that commits after an ApplyDelta error still lands with the closure
// re-armed rather than permanently disabled. On the happy path a caller rollback
// also restores the closed gate, since the whole toggle is part of tx.
func ApplyDelta(ctx context.Context, tx *sql.Tx, d fold.Delta) error {
	if err := openTierAGate(ctx, tx); err != nil {
		return err
	}
	if err := applyDeltaLocked(ctx, tx, d); err != nil {
		// Re-close the gate even though the delta failed: an exported caller may
		// still commit tx, and a committed-open gate would permanently disable the
		// write-closure. Join so neither error is swallowed.
		return errors.Join(err, closeTierAGate(ctx, tx))
	}
	return closeTierAGate(ctx, tx)
}

// RebuildTierA is the Tier-A repair story (I-14): it DROPs streamID's Tier-A rows
// and re-derives them by folding the stream's journal from genesis, re-applying
// every Delta. A drop+refold reproduces the tables byte-identically (DET-T-17);
// repair is always re-fold, never hand-edit. Snapshot-anchored rebuild (fold from
// a snapshot tail rather than genesis) plugs in here when snapshot.go lands; this
// slice folds from genesis, which is byte-equivalent by the R-RESUME law.
//
// The whole operation — drop, fold-apply — commits in one transaction so a rebuild
// is never partially observable.
func (s *Store) RebuildTierA(ctx context.Context, r fold.Reducer, streamID string) error {
	if streamID == "" {
		return fmt.Errorf("graphstore: rebuild tier A: empty stream id")
	}
	stored, err := s.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return err
	}
	var foldedHead uint64
	if n := len(stored); n > 0 {
		foldedHead = stored[n-1].Seq
	}
	events := make([]fold.Event, len(stored))
	for i, e := range stored {
		events[i] = toFoldEvent(e)
	}
	_, deltas, err := fold.Fold(r, nil, events)
	if err != nil {
		return fmt.Errorf("graphstore: rebuild tier A %q: fold: %w", streamID, err)
	}

	if s.rebuildAfterRead != nil {
		s.rebuildAfterRead()
	}

	tx, err := s.writeDB.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("graphstore: rebuild tier A %q: begin: %w", streamID, mapSQLiteBusy(err))
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit

	// TOCTOU guard: the stream was read before this write transaction opened, so
	// an Append could have committed in that window and made the folded prefix
	// stale. BEGIN IMMEDIATE now holds the write lock, so re-reading the head
	// inside tx is authoritative — any drift means we folded an old prefix and
	// must abort rather than project a torn view (deltas cover seq <= foldedHead
	// only). seq is dense and monotonic, so a changed MAX(seq) catches any append.
	var head uint64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(seq), 0) FROM journal WHERE stream_id = ?`, streamID,
	).Scan(&head); err != nil {
		return fmt.Errorf("graphstore: rebuild tier A %q: rechecking head: %w", streamID, mapSQLiteBusy(err))
	}
	if head != foldedHead {
		return fmt.Errorf("graphstore: rebuild tier A %q: folded head %d but journal head %d: %w", streamID, foldedHead, head, ErrRebuildRaced)
	}

	if err := openTierAGate(ctx, tx); err != nil {
		return err
	}
	if err := dropStreamTierA(ctx, tx, streamID); err != nil {
		return fmt.Errorf("graphstore: rebuild tier A %q: drop: %w", streamID, err)
	}
	for i := range deltas {
		if err := applyDeltaLocked(ctx, tx, deltas[i]); err != nil {
			return fmt.Errorf("graphstore: rebuild tier A %q: apply delta %d: %w", streamID, i, err)
		}
	}
	if err := closeTierAGate(ctx, tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("graphstore: rebuild tier A %q: commit: %w", streamID, mapSQLiteBusy(err))
	}
	return nil
}

// toFoldEvent projects a committed journal row onto the I/O-free fold.Event view.
// The hash and lease-epoch columns are intentionally dropped: no pure fold may
// depend on them.
func toFoldEvent(e StoredEvent) fold.Event {
	return fold.Event{
		StreamID:          e.StreamID,
		Seq:               e.Seq,
		Engine:            e.Engine,
		Substream:         e.Substream,
		Type:              e.Type,
		IRContractVersion: e.IRContractVersion,
		IdemToken:         e.IdemToken,
		Payload:           e.Payload,
	}
}

func openTierAGate(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		return fmt.Errorf("graphstore: opening tier-A write gate: %w", err)
	}
	return nil
}

func closeTierAGate(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		return fmt.Errorf("graphstore: closing tier-A write gate: %w", err)
	}
	return nil
}

// dropStreamTierA removes every Tier-A row owned by streamID. Node deletion
// cascades node_labels, node_metadata, and edges(from_id) via ON DELETE CASCADE;
// frontier, defer_wakeups, and channel_cursors carry no FK to nodes, so they are
// cleared explicitly (frontier by root_id, defer_wakeups by node membership,
// cursors by stream). The caller must hold the tier-A write gate open.
func dropStreamTierA(ctx context.Context, tx *sql.Tx, streamID string) error {
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM defer_wakeups WHERE node_id IN (SELECT id FROM nodes WHERE stream_id = ?)`,
		streamID,
	); err != nil {
		return fmt.Errorf("clearing defer_wakeups: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM frontier WHERE root_id = ?`, streamID); err != nil {
		return fmt.Errorf("clearing frontier: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_cursors WHERE stream_id = ?`, streamID); err != nil {
		return fmt.Errorf("clearing channel_cursors: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM nodes WHERE stream_id = ? AND fold_owned = 1`, streamID); err != nil {
		return fmt.Errorf("clearing nodes: %w", err)
	}
	return nil
}

// applyDeltaLocked performs the Tier-A writes assuming the caller has already
// opened the write gate. Order is chosen so foreign keys always resolve: nodes
// (and their label/metadata child sets) before edges; frontier deletes before
// inserts; wakeup deletes before upserts.
func applyDeltaLocked(ctx context.Context, tx *sql.Tx, d fold.Delta) error {
	for _, n := range d.NodeUpserts {
		if err := upsertNode(ctx, tx, n); err != nil {
			return fmt.Errorf("upserting node %q: %w", n.ID, err)
		}
	}
	for _, e := range d.EdgeUpserts {
		depType := e.DepType
		if depType == "" {
			depType = "blocks"
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO edges (from_id, to_id, dep_type, metadata) VALUES (?, ?, ?, ?)
			 ON CONFLICT(from_id, to_id, dep_type) DO UPDATE SET metadata = excluded.metadata`,
			e.FromID, e.ToID, depType, e.Metadata,
		); err != nil {
			return fmt.Errorf("upserting edge %s->%s: %w", e.FromID, e.ToID, err)
		}
	}
	for _, id := range d.FrontierDelete {
		if _, err := tx.ExecContext(ctx, `DELETE FROM frontier WHERE node_id = ?`, id); err != nil {
			return fmt.Errorf("deleting frontier %q: %w", id, err)
		}
	}
	for _, f := range d.FrontierInsert {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO frontier (node_id, root_id, route, ready_priority, created_at, id, defer_until)
			 VALUES (?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(node_id) DO UPDATE SET
			   root_id = excluded.root_id, route = excluded.route,
			   ready_priority = excluded.ready_priority, created_at = excluded.created_at,
			   id = excluded.id, defer_until = excluded.defer_until`,
			f.NodeID, f.RootID, f.Route, f.ReadyPriority, f.CreatedAt, f.ID, nullableString(f.DeferUntil),
		); err != nil {
			return fmt.Errorf("inserting frontier %q: %w", f.NodeID, err)
		}
	}
	for _, c := range d.CursorUpserts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_cursors (stream_id, substream, reader_key, position, planted_seq, advanced_seq)
			 VALUES (?, ?, ?, ?, ?, ?)
			 ON CONFLICT(stream_id, substream, reader_key) DO UPDATE SET
			   position = excluded.position, planted_seq = excluded.planted_seq,
			   advanced_seq = excluded.advanced_seq`,
			c.StreamID, c.Substream, c.ReaderKey, c.Position, c.PlantedSeq, c.AdvancedSeq,
		); err != nil {
			return fmt.Errorf("upserting cursor %s/%s/%s: %w", c.StreamID, c.Substream, c.ReaderKey, err)
		}
	}
	for _, id := range d.WakeupDeletes {
		if _, err := tx.ExecContext(ctx, `DELETE FROM defer_wakeups WHERE node_id = ?`, id); err != nil {
			return fmt.Errorf("deleting wakeup %q: %w", id, err)
		}
	}
	for _, w := range d.WakeupUpserts {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO defer_wakeups (node_id, wake_at) VALUES (?, ?)
			 ON CONFLICT(node_id) DO UPDATE SET wake_at = excluded.wake_at`,
			w.NodeID, w.WakeAt,
		); err != nil {
			return fmt.Errorf("upserting wakeup %q: %w", w.NodeID, err)
		}
	}
	return nil
}

// upsertNode writes one node row (fold_owned=1) and replaces its label and
// metadata child sets. An empty metadata value clears the key (it is simply not
// re-inserted), matching the node_metadata empty-clear contract.
func upsertNode(ctx context.Context, tx *sql.Tx, n fold.NodeRow) error {
	// A fold delta must never adopt a façade-minted (fold_owned=0) row. The
	// ON CONFLICT(id) upsert below would otherwise rewrite it fold_owned=1 and
	// write-close it, silently corrupting the beads.Store's own bead. Refuse
	// loudly; an existing fold_owned=1 row upserts normally (idempotent refold).
	var existingFoldOwned int
	switch err := tx.QueryRowContext(ctx, `SELECT fold_owned FROM nodes WHERE id = ?`, n.ID).Scan(&existingFoldOwned); {
	case errors.Is(err, sql.ErrNoRows):
		// No existing row: a fresh fold insert.
	case err != nil:
		return fmt.Errorf("checking existing node %q: %w", n.ID, err)
	case existingFoldOwned == 0:
		return fmt.Errorf("node %q: %w", n.ID, ErrProjectionIDCollision)
	}
	tier := n.StorageTier
	if tier == "" {
		tier = "history"
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO nodes
		   (id, title, status, bead_type, priority, description, assignee, from_actor,
		    parent_id, ref, created_at, updated_at, defer_until, storage_tier,
		    is_blocked, fold_owned, stream_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 1, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = excluded.title, status = excluded.status, bead_type = excluded.bead_type,
		   priority = excluded.priority, description = excluded.description,
		   assignee = excluded.assignee, from_actor = excluded.from_actor,
		   parent_id = excluded.parent_id, ref = excluded.ref,
		   created_at = excluded.created_at, updated_at = excluded.updated_at,
		   defer_until = excluded.defer_until, storage_tier = excluded.storage_tier,
		   is_blocked = excluded.is_blocked, fold_owned = 1, stream_id = excluded.stream_id`,
		n.ID, n.Title, n.Status, n.BeadType, nullableInt(n.Priority), n.Description,
		n.Assignee, n.FromActor, n.ParentID, n.Ref, n.CreatedAt, n.UpdatedAt,
		nullableString(n.DeferUntil), tier, boolToInt(n.IsBlocked), n.StreamID,
	); err != nil {
		return err
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_labels WHERE node_id = ?`, n.ID); err != nil {
		return fmt.Errorf("clearing labels: %w", err)
	}
	labels := append([]string(nil), n.Labels...)
	sort.Strings(labels)
	for _, lbl := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_labels (node_id, label) VALUES (?, ?)
			 ON CONFLICT(node_id, label) DO NOTHING`,
			n.ID, lbl,
		); err != nil {
			return fmt.Errorf("inserting label %q: %w", lbl, err)
		}
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM node_metadata WHERE node_id = ?`, n.ID); err != nil {
		return fmt.Errorf("clearing metadata: %w", err)
	}
	keys := make([]string, 0, len(n.Metadata))
	for k := range n.Metadata {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := n.Metadata[k]
		if v == "" { // empty value clears the key
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_metadata (node_id, key, value) VALUES (?, ?, ?)
			 ON CONFLICT(node_id, key) DO UPDATE SET value = excluded.value`,
			n.ID, k, v,
		); err != nil {
			return fmt.Errorf("inserting metadata %q: %w", k, err)
		}
	}
	return nil
}

func nullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func nullableString(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
