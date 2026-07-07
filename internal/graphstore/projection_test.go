package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/graphstore/fold/foldtest"
)

// TestTierAProjectionAndRebuildByteIdentity_DETT17 proves two things:
//   - Fold → ApplyDelta populates the Tier-A projection (nodes / edges /
//     frontier) from a journal stream; and
//   - RebuildTierA (DROP the stream's rows, refold from genesis, re-apply)
//     reproduces the tables byte-identically to the incrementally-built live
//     projection (DET-T-17). The live path applies one delta per serve cycle in
//     its own transaction; the rebuild applies the whole refold in one — two
//     genuinely different code paths that must converge on identical bytes.
func TestTierAProjectionAndRebuildByteIdentity_DETT17(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := foldtest.EchoReducer{}
	const stream = "gcj-root-proj"
	s.RegisterEventType(foldtest.Engine, foldtest.EventNode)
	s.RegisterEventType(foldtest.Engine, foldtest.EventEdge)
	s.RegisterEventType(foldtest.Engine, foldtest.EventCursor)

	// The stream touches all seven Tier-A tables non-vacuously: nodes (+ their
	// node_labels / node_metadata child rows), edges, frontier, and a cursor event
	// that plants a channel_cursors row plus a defer_wakeups row.
	events := []struct{ typ, payload string }{
		{foldtest.EventNode, `{"id":"n1","title":"one"}`},
		{foldtest.EventNode, `{"id":"n2","title":"two"}`},
		{foldtest.EventEdge, `{"from":"n1","to":"n2"}`}, // blocks n2 out of frontier
		{foldtest.EventNode, `{"id":"n3","title":"three"}`},
		{foldtest.EventEdge, `{"from":"n2","to":"n3"}`}, // blocks n3 out of frontier
		{foldtest.EventCursor, `{"reader":"r1","node":"n1","position":2,"wake_at":"2020-02-02T00:00:00Z"}`},
	}

	// Live incremental path: append, fold, project — one serve cycle per event,
	// each projection delta committed in its own transaction.
	state := r.Zero(stream)
	for i, e := range events {
		p := canonPayload(t, e.payload)
		if _, err := s.Append(ctx, stream, foldtest.Engine, uint64(i), 0, []JournalEvent{{Type: e.typ, Payload: p}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
		next, delta, err := r.Apply(state, fold.Event{
			StreamID: stream, Seq: uint64(i + 1), Engine: foldtest.Engine, Type: e.typ, Payload: p,
		})
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
		state = next

		tx, err := s.DB().BeginTx(ctx, nil)
		if err != nil {
			t.Fatalf("begin %d: %v", i, err)
		}
		if err := ApplyDelta(ctx, tx, delta); err != nil {
			_ = tx.Rollback()
			t.Fatalf("apply delta %d: %v", i, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %d: %v", i, err)
		}
	}

	liveDump := dumpTierA(t, s, stream)

	// Every one of the seven Tier-A tables is genuinely populated for this stream,
	// so the DROP+refold byte-identity comparison below is non-vacuous (not
	// empty-vs-empty) for all of them — the DET-T-17 regression this guards.
	for _, tc := range []struct {
		table string
		query string
	}{
		{"nodes", `SELECT COUNT(*) FROM nodes WHERE stream_id = ?`},
		{"node_labels", `SELECT COUNT(*) FROM node_labels nl JOIN nodes n ON n.id = nl.node_id WHERE n.stream_id = ?`},
		{"node_metadata", `SELECT COUNT(*) FROM node_metadata nm JOIN nodes n ON n.id = nm.node_id WHERE n.stream_id = ?`},
		{"edges", `SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.stream_id = ?`},
		{"frontier", `SELECT COUNT(*) FROM frontier WHERE root_id = ?`},
		{"defer_wakeups", `SELECT COUNT(*) FROM defer_wakeups d JOIN nodes n ON n.id = d.node_id WHERE n.stream_id = ?`},
		{"channel_cursors", `SELECT COUNT(*) FROM channel_cursors WHERE stream_id = ?`},
	} {
		var n int
		if err := s.DB().QueryRowContext(ctx, tc.query, stream).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", tc.table, err)
		}
		if n == 0 {
			t.Fatalf("Tier-A table %q is empty for stream %q — DET-T-17 byte-identity would be vacuous", tc.table, stream)
		}
	}

	// The frontier reflects blocking: only n1 remains ready (n2 blocked by
	// n1->n2, n3 by n2->n3).
	if got := frontierNodeIDs(t, s, stream); !equalStrings(got, []string{"n1"}) {
		t.Fatalf("frontier = %v, want [n1] (blocking not projected)", got)
	}

	// Rebuild: DROP + refold from genesis.
	if err := s.RebuildTierA(ctx, r, stream); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuildDump := dumpTierA(t, s, stream)

	if liveDump != rebuildDump {
		t.Fatalf("live projection is not byte-identical to rebuild:\n--- live ---\n%s\n--- rebuild ---\n%s", liveDump, rebuildDump)
	}
}

// TestTierAWriteClosureGuard_DETT18 proves the DET-T-18 tripwire: a fold-owned
// row cannot be mutated, deleted, or forged by any writer that does not go
// through the fold applier (which opens the write gate). A legacy fold_owned=0
// row stays freely mutable — the closure is per-row, not per-table.
func TestTierAWriteClosureGuard_DETT18(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-guard"

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	delta := fold.Delta{NodeUpserts: []fold.NodeRow{{
		ID: "n1", Status: "open", BeadType: "task",
		CreatedAt: "2020-01-01T00:00:00Z", StorageTier: "history", StreamID: stream,
	}}}
	if err := ApplyDelta(ctx, tx, delta); err != nil {
		_ = tx.Rollback()
		t.Fatalf("apply delta: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	db := s.DB()

	_, err = db.ExecContext(ctx, `UPDATE nodes SET title = 'hax' WHERE id = 'n1'`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("external UPDATE of fold-owned row = %v, want write-closed abort", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM nodes WHERE id = 'n1'`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("external DELETE of fold-owned row = %v, want write-closed abort", err)
	}
	_, err = db.ExecContext(ctx,
		`INSERT INTO nodes (id, created_at, fold_owned) VALUES ('n2', '2020-01-01T00:00:00Z', 1)`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("external INSERT of a fold-owned row = %v, want write-closed abort", err)
	}

	// The protected row is untouched by the rejected mutations.
	var title string
	if err := db.QueryRowContext(ctx, `SELECT title FROM nodes WHERE id = 'n1'`).Scan(&title); err != nil {
		t.Fatalf("read node: %v", err)
	}
	if title != "" {
		t.Fatalf("title = %q, want '' — an external UPDATE leaked through the closure", title)
	}

	// A legacy (fold_owned=0) row is freely mutable.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO nodes (id, created_at, fold_owned) VALUES ('legacy-1', '2020-01-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE nodes SET title = 'ok' WHERE id = 'legacy-1'`); err != nil {
		t.Fatalf("update legacy row = %v, want success (closure is per-row)", err)
	}

	// Escalation bypass (B1): a non-fold writer must not be able to insert a
	// fold_owned=0 row and then flip it to fold_owned=1 to forge a fold-owned row
	// while the gate is closed. The UPDATE guard fires on NEW.fold_owned=1, not
	// just OLD.fold_owned=1, so the escalation aborts.
	if _, err := db.ExecContext(ctx,
		`INSERT INTO nodes (id, created_at, fold_owned) VALUES ('escalate-1', '2020-01-01T00:00:00Z', 0)`); err != nil {
		t.Fatalf("insert non-fold row: %v", err)
	}
	_, err = db.ExecContext(ctx,
		`UPDATE nodes SET fold_owned = 1, title = 'forged' WHERE id = 'escalate-1'`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("escalation UPDATE fold_owned 0->1 = %v, want write-closed abort", err)
	}
	// The escalation did not take: the row is still non-fold and unforged.
	var owned int
	var escTitle string
	if err := db.QueryRowContext(ctx,
		`SELECT fold_owned, title FROM nodes WHERE id = 'escalate-1'`).Scan(&owned, &escTitle); err != nil {
		t.Fatalf("read escalate row: %v", err)
	}
	if owned != 0 || escTitle != "" {
		t.Fatalf("escalate-1 = (fold_owned=%d, title=%q), want (0, '') — escalation leaked through", owned, escTitle)
	}
}

// TestRebuildTierARacesConcurrentAppend proves the S1 TOCTOU guard: if an Append
// commits between the from-genesis stream read and the rebuild's write
// transaction, RebuildTierA aborts with ErrRebuildRaced rather than projecting a
// stale prefix.
func TestRebuildTierARacesConcurrentAppend(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	r := foldtest.EchoReducer{}
	const stream = "gcj-root-race"
	s.RegisterEventType(foldtest.Engine, foldtest.EventNode)

	for i := 0; i < 2; i++ {
		if _, err := s.Append(ctx, stream, foldtest.Engine, uint64(i), 0, []JournalEvent{{
			Type: foldtest.EventNode, Payload: canonPayload(t, fmt.Sprintf(`{"id":"n%d"}`, i)),
		}}); err != nil {
			t.Fatalf("append %d: %v", i, err)
		}
	}

	// Simulate a concurrent append landing in the read/write window: the hook fires
	// after RebuildTierA has read+folded seq 1..2 but before its write txn opens.
	var raced bool
	s.rebuildAfterRead = func() {
		if raced {
			return // only race the first rebuild attempt
		}
		raced = true
		if _, err := s.Append(ctx, stream, foldtest.Engine, 2, 0, []JournalEvent{{
			Type: foldtest.EventNode, Payload: canonPayload(t, `{"id":"n2"}`),
		}}); err != nil {
			t.Errorf("racing append: %v", err)
		}
	}

	err := s.RebuildTierA(ctx, r, stream)
	if !errors.Is(err, ErrRebuildRaced) {
		t.Fatalf("rebuild racing a concurrent append = %v, want ErrRebuildRaced", err)
	}

	// The aborted rebuild left no partial projection, and a clean retry (no race)
	// now succeeds against the new head.
	s.rebuildAfterRead = nil
	if err := s.RebuildTierA(ctx, r, stream); err != nil {
		t.Fatalf("clean retry after race: %v", err)
	}
	var nodes int
	if err := s.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE stream_id = ?`, stream).Scan(&nodes); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodes != 3 { // n0, n1, n2
		t.Fatalf("nodes after clean rebuild = %d, want 3", nodes)
	}
}

// TestApplyDeltaErrorLeavesGateClosed proves the S2 contract: when applyDeltaLocked
// fails, ApplyDelta still closes the tier-A write gate, so a caller that commits
// its transaction afterward does NOT leave the write-closure permanently disabled.
func TestApplyDeltaErrorLeavesGateClosed(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	// Force applyDeltaLocked to fail mid-flight: an edge whose from_id references a
	// node that does not exist violates the edges FK, so the INSERT errors after
	// the gate was opened.
	badDelta := fold.Delta{EdgeUpserts: []fold.EdgeRow{{FromID: "ghost", ToID: "also-ghost"}}}

	tx, err := s.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := ApplyDelta(ctx, tx, badDelta); err == nil {
		_ = tx.Rollback()
		t.Fatal("ApplyDelta of an FK-violating delta = nil, want error")
	}
	// The caller commits despite the error (the buggy pattern S2 defends against).
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}

	// The gate must be committed-closed, not committed-open.
	var open int
	if err := s.DB().QueryRowContext(ctx, `SELECT open FROM tier_a_write_gate WHERE singleton = 0`).Scan(&open); err != nil {
		t.Fatalf("read gate: %v", err)
	}
	if open != 0 {
		t.Fatalf("tier_a_write_gate.open = %d after a failed+committed ApplyDelta, want 0 (closure disabled)", open)
	}

	// And the closure still bites: an external write of a fold-owned row aborts.
	_, err = s.DB().ExecContext(ctx,
		`INSERT INTO nodes (id, created_at, fold_owned) VALUES ('n-guard', '2020-01-01T00:00:00Z', 1)`)
	if err == nil || indexOf(err.Error(), "write-closed") < 0 {
		t.Fatalf("external fold-owned INSERT after failed ApplyDelta = %v, want write-closed abort", err)
	}
}

func frontierNodeIDs(t *testing.T, s *Store, streamID string) []string {
	t.Helper()
	rows, err := s.DB().QueryContext(context.Background(),
		`SELECT node_id FROM frontier WHERE root_id = ? ORDER BY node_id`, streamID)
	if err != nil {
		t.Fatalf("frontier query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, id)
	}
	return out
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// dumpTierA renders every Tier-A row owned by streamID in a deterministic,
// column-labeled form so two projections can be compared byte-for-byte.
func dumpTierA(t *testing.T, s *Store, streamID string) string {
	t.Helper()
	db := s.DB()
	var b strings.Builder
	dumpTable(t, &b, db, "nodes",
		`SELECT id,title,status,bead_type,priority,description,assignee,from_actor,parent_id,ref,created_at,updated_at,defer_until,storage_tier,is_blocked,fold_owned,stream_id
		   FROM nodes WHERE stream_id = ? ORDER BY id`, streamID)
	dumpTable(t, &b, db, "node_labels",
		`SELECT nl.node_id, nl.label FROM node_labels nl JOIN nodes n ON n.id = nl.node_id
		  WHERE n.stream_id = ? ORDER BY nl.node_id, nl.label`, streamID)
	dumpTable(t, &b, db, "node_metadata",
		`SELECT nm.node_id, nm.key, nm.value FROM node_metadata nm JOIN nodes n ON n.id = nm.node_id
		  WHERE n.stream_id = ? ORDER BY nm.node_id, nm.key`, streamID)
	dumpTable(t, &b, db, "edges",
		`SELECT e.from_id, e.to_id, e.dep_type, e.metadata FROM edges e JOIN nodes n ON n.id = e.from_id
		  WHERE n.stream_id = ? ORDER BY e.from_id, e.to_id, e.dep_type`, streamID)
	dumpTable(t, &b, db, "frontier",
		`SELECT node_id, root_id, route, ready_priority, created_at, id, defer_until
		   FROM frontier WHERE root_id = ? ORDER BY node_id`, streamID)
	dumpTable(t, &b, db, "defer_wakeups",
		`SELECT d.node_id, d.wake_at FROM defer_wakeups d JOIN nodes n ON n.id = d.node_id
		  WHERE n.stream_id = ? ORDER BY d.node_id`, streamID)
	dumpTable(t, &b, db, "channel_cursors",
		`SELECT stream_id, substream, reader_key, position, planted_seq, advanced_seq
		   FROM channel_cursors WHERE stream_id = ? ORDER BY substream, reader_key`, streamID)
	return b.String()
}

func dumpTable(t *testing.T, b *strings.Builder, db *sql.DB, label, query string, args ...any) {
	t.Helper()
	rows, err := db.QueryContext(context.Background(), query, args...)
	if err != nil {
		t.Fatalf("dump %s: %v", label, err)
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		t.Fatalf("dump %s columns: %v", label, err)
	}
	fmt.Fprintf(b, "%s:\n", label)
	for rows.Next() {
		cells := make([]sql.NullString, len(cols))
		ptrs := make([]any, len(cols))
		for i := range cells {
			ptrs[i] = &cells[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			t.Fatalf("dump %s scan: %v", label, err)
		}
		parts := make([]string, len(cols))
		for i, c := range cols {
			v := "NULL"
			if cells[i].Valid {
				v = cells[i].String
			}
			parts[i] = c + "=" + v
		}
		fmt.Fprintf(b, "  %s\n", strings.Join(parts, " "))
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("dump %s rows: %v", label, err)
	}
}
