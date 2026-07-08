//go:build integration

package graphstore

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// tripwireStep is one rogue-write step in the cross-arm parity script. When
// abortSubstr is "" the write must succeed on both arms; otherwise it must fail
// on both arms with an error containing abortSubstr.
//
// verify, when non-nil, runs after a successful step and asserts the resulting
// row state. It exists because a plpgsql BEFORE-ROW trigger that returns NULL
// SILENTLY skips the row op with no error: without a post-state check, a future
// edit that regressed an allow-path (RETURN OLD / RETURN NEW) to a fall-through
// would still report "no error" while quietly dropping the write. Asserting the
// AFTER state (row gone / row changed) turns that silent skip into a failure.
type tripwireStep struct {
	name        string
	query       string
	args        []any
	abortSubstr string
	verify      func(ctx context.Context, st *Store) error
}

// pgParityRowCount returns COUNT(*) for query against st's write handle.
func pgParityRowCount(ctx context.Context, st *Store, query string, args ...any) (int, error) {
	var n int
	if err := st.DB().QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// wantGone asserts no row matches countQuery — the allowed DELETE actually
// removed the row. A silently-skipped BEFORE-DELETE trigger (RETURN NULL) would
// leave the row present with no error, which this catches.
func wantGone(countQuery string, args ...any) func(context.Context, *Store) error {
	return func(ctx context.Context, st *Store) error {
		n, err := pgParityRowCount(ctx, st, countQuery, args...)
		if err != nil {
			return err
		}
		if n != 0 {
			return fmt.Errorf("row still present after an allowed DELETE (silent trigger skip?): count=%d", n)
		}
		return nil
	}
}

// wantOne asserts exactly one row matches countQuery — the allowed UPDATE landed
// its new value on the row. A silently-skipped BEFORE-UPDATE trigger (RETURN
// NULL) would leave the old value in place, which this catches.
func wantOne(countQuery string, args ...any) func(context.Context, *Store) error {
	return func(ctx context.Context, st *Store) error {
		n, err := pgParityRowCount(ctx, st, countQuery, args...)
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("expected exactly one row after an allowed UPDATE (silent trigger skip?): count=%d", n)
		}
		return nil
	}
}

// tripwireScript is a backend-agnostic sequence of writes exercising every
// write-closure tripwire plus the successful (gate-open / non-fold) paths. The
// SQLite and Postgres DDL preserve identical table/column names and identical
// trigger messages, so the same `?`-placeholder SQL runs unchanged on both arms
// (Postgres via the pgqmark shim).
func tripwireScript() []tripwireStep {
	return []tripwireStep{
		// Journal append-only pair.
		{name: "seed_journal_1", query: `INSERT INTO journal(stream_id, seq, engine, type, ir_contract_version, payload, payload_hash, chain_hash, lease_epoch, appended_at) VALUES('s', 1, 'lumen', 't', '', ?, ?, ?, 0, '2026-01-01T00:00:00Z')`, args: []any{[]byte{0x01}, b32(0xAA), b32(0xBB)}},
		{name: "journal_no_update", query: `UPDATE journal SET type='x' WHERE stream_id='s' AND seq=1`, abortSubstr: "journal is append-only"},
		{name: "journal_no_delete", query: `DELETE FROM journal WHERE stream_id='s' AND seq=1`, abortSubstr: "retention gate closed"},
		// ALLOWED path: with the retention gate open over seq 1, the DELETE takes
		// the RETURN OLD branch and the row is actually removed.
		{name: "open_retention_gate", query: `INSERT INTO retention_gate(stream_id, max_seq) VALUES('s', 1)`},
		{
			name: "journal_delete_gated_ok", query: `DELETE FROM journal WHERE stream_id='s' AND seq=1`,
			verify: wantGone(`SELECT COUNT(*) FROM journal WHERE stream_id='s' AND seq=1`),
		},

		// nodes fold-owned guards.
		{name: "nodes_fold_insert_closed", query: `INSERT INTO nodes(id, created_at, fold_owned) VALUES('n1','2026-01-01',1)`, abortSubstr: "fold-owned row is write-closed"},
		{name: "nodes_plain_insert_ok", query: `INSERT INTO nodes(id, created_at, fold_owned) VALUES('n0','2026-01-01',0)`},
		{name: "open_tier_a_gate", query: `UPDATE tier_a_write_gate SET open=1 WHERE singleton=0`},
		{name: "nodes_fold_insert_open_ok", query: `INSERT INTO nodes(id, created_at, fold_owned) VALUES('n1','2026-01-01',1)`},
		// ALLOWED paths (gate open): a fold-owned node UPDATE (RETURN NEW) mutates
		// the row and a fold-owned node DELETE (RETURN OLD) removes it. n2 is a
		// throwaway so n1 survives for the closed-gate abort steps below.
		{name: "nodes_fold_insert_open_ok_n2", query: `INSERT INTO nodes(id, created_at, fold_owned) VALUES('n2','2026-01-01',1)`},
		{
			name: "nodes_fold_update_open_ok", query: `UPDATE nodes SET title='updated' WHERE id='n2'`,
			verify: wantOne(`SELECT COUNT(*) FROM nodes WHERE id='n2' AND title='updated'`),
		},
		{
			name: "nodes_fold_delete_open_ok", query: `DELETE FROM nodes WHERE id='n2'`,
			verify: wantGone(`SELECT COUNT(*) FROM nodes WHERE id='n2'`),
		},
		{name: "frontier_insert_open_ok", query: `INSERT INTO frontier(node_id, root_id, created_at, id) VALUES('f1','r','2026-01-01','id1')`},
		// ALLOWED paths (gate open): a frontier UPDATE (RETURN NEW) and DELETE
		// (RETURN OLD) on throwaway f3, leaving f1 for the closed-gate abort steps.
		{name: "frontier_insert_open_ok_f3", query: `INSERT INTO frontier(node_id, root_id, created_at, id) VALUES('f3','r','2026-01-01','id3')`},
		{
			name: "frontier_update_open_ok", query: `UPDATE frontier SET route='rr' WHERE node_id='f3'`,
			verify: wantOne(`SELECT COUNT(*) FROM frontier WHERE node_id='f3' AND route='rr'`),
		},
		{
			name: "frontier_delete_open_ok", query: `DELETE FROM frontier WHERE node_id='f3'`,
			verify: wantGone(`SELECT COUNT(*) FROM frontier WHERE node_id='f3'`),
		},
		{name: "close_tier_a_gate", query: `UPDATE tier_a_write_gate SET open=0 WHERE singleton=0`},
		{name: "nodes_fold_update_closed", query: `UPDATE nodes SET title='x' WHERE id='n1'`, abortSubstr: "fold-owned row is write-closed"},
		{name: "nodes_fold_delete_closed", query: `DELETE FROM nodes WHERE id='n1'`, abortSubstr: "fold-owned row is write-closed"},

		// frontier guards.
		{name: "frontier_insert_closed", query: `INSERT INTO frontier(node_id, root_id, created_at, id) VALUES('f2','r','2026-01-01','id2')`, abortSubstr: "frontier is write-closed"},
		{name: "frontier_update_closed", query: `UPDATE frontier SET route='x' WHERE node_id='f1'`, abortSubstr: "frontier is write-closed"},
		{name: "frontier_delete_closed", query: `DELETE FROM frontier WHERE node_id='f1'`, abortSubstr: "frontier is write-closed"},

		// snapshots guards.
		{name: "snapshot_insert_closed", query: `INSERT INTO snapshots(stream_id, covered_seq, engine, reducer_version, snapshot_format_version, state_hash, state, created_at) VALUES('s',5,'lumen',1,1,?,?,'2026-01-01')`, args: []any{b32(0xCC), []byte{0x02}}, abortSubstr: "snapshots is write-closed"},
		{name: "open_snapshot_gate", query: `UPDATE snapshot_write_gate SET open=1 WHERE singleton=0`},
		{name: "snapshot_insert_open_ok", query: `INSERT INTO snapshots(stream_id, covered_seq, engine, reducer_version, snapshot_format_version, state_hash, state, created_at) VALUES('s',5,'lumen',1,1,?,?,'2026-01-01')`, args: []any{b32(0xCC), []byte{0x02}}},
		// ALLOWED paths (gate open): a snapshot UPDATE (RETURN NEW) and DELETE
		// (RETURN OLD) on throwaway covered_seq 6, leaving seq 5 for the
		// closed-gate abort steps.
		{name: "snapshot_insert_open_ok_s6", query: `INSERT INTO snapshots(stream_id, covered_seq, engine, reducer_version, snapshot_format_version, state_hash, state, created_at) VALUES('s',6,'lumen',1,1,?,?,'2026-01-01')`, args: []any{b32(0xCC), []byte{0x02}}},
		{
			name: "snapshot_update_open_ok", query: `UPDATE snapshots SET engine='v2' WHERE stream_id='s' AND covered_seq=6`,
			verify: wantOne(`SELECT COUNT(*) FROM snapshots WHERE stream_id='s' AND covered_seq=6 AND engine='v2'`),
		},
		{
			name: "snapshot_delete_open_ok", query: `DELETE FROM snapshots WHERE stream_id='s' AND covered_seq=6`,
			verify: wantGone(`SELECT COUNT(*) FROM snapshots WHERE stream_id='s' AND covered_seq=6`),
		},
		{name: "close_snapshot_gate", query: `UPDATE snapshot_write_gate SET open=0 WHERE singleton=0`},
		{name: "snapshot_update_closed", query: `UPDATE snapshots SET engine='v2' WHERE stream_id='s' AND covered_seq=5`, abortSubstr: "snapshots is write-closed"},
		{name: "snapshot_delete_closed", query: `DELETE FROM snapshots WHERE stream_id='s' AND covered_seq=5`, abortSubstr: "snapshots is write-closed"},
	}
}

func runTripwireScript(t *testing.T, arm string, st *Store) {
	t.Helper()
	ctx := context.Background()
	for _, step := range tripwireScript() {
		_, err := st.DB().ExecContext(ctx, step.query, step.args...)
		if step.abortSubstr == "" {
			if err != nil {
				t.Fatalf("[%s] step %q: expected success, got %v", arm, step.name, err)
			}
			if step.verify != nil {
				if verr := step.verify(ctx, st); verr != nil {
					t.Fatalf("[%s] step %q: allowed write did not take effect: %v", arm, step.name, verr)
				}
			}
			continue
		}
		if err == nil {
			t.Fatalf("[%s] step %q: expected tripwire abort %q, but write succeeded", arm, step.name, step.abortSubstr)
		}
		if !strings.Contains(err.Error(), step.abortSubstr) {
			t.Fatalf("[%s] step %q: abort message %v, want substring %q", arm, step.name, err, step.abortSubstr)
		}
	}
}

// TestPGParityTripwires runs the identical rogue-write script against a fresh
// SQLite store and a fresh Postgres store and asserts every step behaves the
// same on both arms — the nine write-closure tripwires fire identically and the
// gate-open / non-fold paths succeed identically. It is the P6.1 cross-arm
// exit gate for DDL/trigger fidelity. It skips cleanly when no Postgres DSN is
// configured (newPGStore skips), so the SQLite arm still proves the script is
// well-formed.
func TestPGParityTripwires(t *testing.T) {
	// SQLite arm.
	sqliteStore, err := Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"), Options{CityID: "parity"})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer sqliteStore.Close()
	runTripwireScript(t, "sqlite", sqliteStore)

	// Postgres arm (skips if no DSN).
	pgStore := newPGStore(t, Options{CityID: "parity"})
	runTripwireScript(t, "postgres", pgStore)
}
