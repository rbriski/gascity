package graphstore

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// pgTestDSN returns the Postgres DSN for the graph-substrate conformance arm, or
// skips cleanly when none is configured. GRAPHSTORE_PG_DSN is the primary gate
// (matching the P6.1 verify convention); GC_GRAPH_TEST_PG_DSN is accepted as the
// blueprint alias.
func pgTestDSN(t *testing.T) string {
	t.Helper()
	for _, env := range []string{"GRAPHSTORE_PG_DSN", "GC_GRAPH_TEST_PG_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(env)); dsn != "" {
			return dsn
		}
	}
	t.Skip("GRAPHSTORE_PG_DSN not set; skipping Postgres graphstore tests")
	return ""
}

// randSchema returns a fresh, identifier-safe schema name for per-test isolation.
func randSchema(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return "gs_test_" + hex.EncodeToString(b[:])
}

// newPGStore opens a graphstore.Store against a private, freshly created schema
// so tests are parallel-safe against a shared dev Postgres and leave nothing
// behind. It returns the store; the store's own connections have search_path
// pinned to the private schema, so store.DB()/ReadDB() operate there.
func newPGStore(t *testing.T, opts Options) *Store {
	t.Helper()
	dsn := pgTestDSN(t)
	ctx := context.Background()

	schema := randSchema(t)
	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}

	st, err := openPostgres(ctx, dsn, opts, []string{"SET search_path TO " + schema})
	if err != nil {
		_, _ = boot.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = boot.Close()
		t.Fatalf("openPostgres: %v", err)
	}

	t.Cleanup(func() {
		_ = st.Close()
		if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
		_ = boot.Close()
	})
	return st
}

// TestPostgresOpenMigratesAndSeeds proves openPostgres runs the ladder 0→4,
// re-opens idempotently at the same version, and seeds/guards city_id.
func TestPostgresOpenMigratesAndSeeds(t *testing.T) {
	st := newPGStore(t, Options{CityID: "city-pg-1"})
	ctx := context.Background()

	// schema_version is at the latest rung.
	got, err := currentSchemaVersion(ctx, st.DB(), postgresDialect{})
	if err != nil {
		t.Fatalf("currentSchemaVersion: %v", err)
	}
	if want := len(pgMigrations); got != want {
		t.Fatalf("schema_version = %d, want %d", got, want)
	}
	if st.CityID() != "city-pg-1" {
		t.Fatalf("CityID = %q, want city-pg-1", st.CityID())
	}

	// Re-running migrate is a no-op (idempotent) — still at the latest rung.
	if err := migrate(ctx, st.DB(), postgresDialect{}); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	got2, err := currentSchemaVersion(ctx, st.DB(), postgresDialect{})
	if err != nil {
		t.Fatalf("currentSchemaVersion (2): %v", err)
	}
	if got2 != got {
		t.Fatalf("re-migrate changed schema_version %d -> %d", got, got2)
	}

	// All expected tables exist in the private schema.
	for _, tbl := range []string{
		"journal", "retention_gate", "snapshots", "writer_lease", "graph_meta",
		"nodes", "node_labels", "node_metadata", "edges", "frontier",
		"defer_wakeups", "channel_cursors", "tier_a_write_gate",
		"graph_residence", "snapshot_write_gate",
	} {
		var n int
		if err := st.DB().QueryRowContext(ctx,
			`SELECT COUNT(*) FROM information_schema.tables
			  WHERE table_schema = current_schema() AND table_name = ?`, tbl,
		).Scan(&n); err != nil {
			t.Fatalf("probe %s: %v", tbl, err)
		}
		if n != 1 {
			t.Errorf("table %s: present=%d, want 1", tbl, n)
		}
	}
}

// TestPostgresCityMismatch proves city_id is immutable: reopening the same schema
// with a different CityID fails with ErrCityMismatch, and an empty want adopts
// the stored value.
func TestPostgresCityMismatch(t *testing.T) {
	dsn := pgTestDSN(t)
	ctx := context.Background()
	schema := randSchema(t)
	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	defer func() { _ = boot.Close() }()
	if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") //nolint:errcheck

	extra := []string{"SET search_path TO " + schema}
	st1, err := openPostgres(ctx, dsn, Options{CityID: "city-A"}, extra)
	if err != nil {
		t.Fatalf("open city-A: %v", err)
	}
	_ = st1.Close()

	// Wrong city → loud mismatch.
	if _, err := openPostgres(ctx, dsn, Options{CityID: "city-B"}, extra); !errors.Is(err, ErrCityMismatch) {
		t.Fatalf("reopen with city-B: err = %v, want ErrCityMismatch", err)
	}

	// Empty want adopts the stored city.
	st3, err := openPostgres(ctx, dsn, Options{CityID: ""}, extra)
	if err != nil {
		t.Fatalf("reopen with empty city: %v", err)
	}
	defer func() { _ = st3.Close() }()
	if st3.CityID() != "city-A" {
		t.Fatalf("adopted CityID = %q, want city-A", st3.CityID())
	}
}

// TestPostgresReadCommittedGuard proves openPostgres refuses a server whose
// default isolation is stricter than READ COMMITTED, and accepts the READ
// COMMITTED default.
func TestPostgresReadCommittedGuard(t *testing.T) {
	dsn := pgTestDSN(t)
	ctx := context.Background()
	schema := randSchema(t)
	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	defer func() { _ = boot.Close() }()
	if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	defer boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE") //nolint:errcheck

	// Default (read committed) opens cleanly.
	st, err := openPostgres(ctx, dsn, Options{}, []string{"SET search_path TO " + schema})
	if err != nil {
		t.Fatalf("open at default isolation: %v", err)
	}
	_ = st.Close()

	// A connection forced to REPEATABLE READ is rejected.
	_, err = openPostgres(ctx, dsn, Options{}, []string{
		"SET search_path TO " + schema,
		"SET default_transaction_isolation = 'repeatable read'",
	})
	if err == nil {
		t.Fatal("openPostgres accepted REPEATABLE READ default; want a loud refusal")
	}
	if !strings.Contains(err.Error(), "READ COMMITTED") {
		t.Fatalf("refusal error = %v, want it to mention READ COMMITTED", err)
	}
}

// b32 returns a deterministic 32-byte slice for BYTEA hash columns.
func b32(fill byte) []byte {
	b := make([]byte, 32)
	for i := range b {
		b[i] = fill
	}
	return b
}

// seedJournalRow inserts one journal row directly (INSERT is allowed; the
// append-only triggers guard UPDATE/DELETE).
func seedJournalRow(t *testing.T, st *Store, seq int) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		`INSERT INTO journal(stream_id, seq, engine, type, ir_contract_version,
		    payload, payload_hash, chain_hash, lease_epoch, appended_at)
		 VALUES('s', ?, 'lumen', 't', '', ?, ?, ?, 0, '2026-01-01T00:00:00Z')`,
		seq, []byte{0x01}, b32(0xAA), b32(0xBB),
	); err != nil {
		t.Fatalf("seed journal row: %v", err)
	}
}

func setGate(t *testing.T, st *Store, table string, open int) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(),
		"UPDATE "+table+" SET open = ? WHERE singleton = 0", open,
	); err != nil {
		t.Fatalf("set gate %s open=%d: %v", table, open, err)
	}
}

// mustAbort runs sql and asserts it fails with an error containing wantSubstr —
// i.e. a tripwire fired.
func mustAbort(t *testing.T, st *Store, wantSubstr, query string, args ...any) {
	t.Helper()
	_, err := st.DB().ExecContext(context.Background(), query, args...)
	if err == nil {
		t.Fatalf("expected tripwire abort (%q) but the write succeeded: %s", wantSubstr, query)
	}
	if !strings.Contains(err.Error(), wantSubstr) {
		t.Fatalf("tripwire message mismatch: got %v, want substring %q", err, wantSubstr)
	}
}

func mustSucceed(t *testing.T, st *Store, query string, args ...any) {
	t.Helper()
	if _, err := st.DB().ExecContext(context.Background(), query, args...); err != nil {
		t.Fatalf("expected success but got %v: %s", err, query)
	}
}

// TestPostgresTripwiresFire proves all nine write-closure tripwires abort on a
// direct rogue write against the Postgres arm, exactly as on SQLite: the journal
// append-only pair, the three nodes fold-owned guards, the three frontier
// guards, and the three snapshots guards.
func TestPostgresTripwiresFire(t *testing.T) {
	st := newPGStore(t, Options{CityID: "city-trip"})

	t.Run("journal_no_update", func(t *testing.T) {
		seedJournalRow(t, st, 1)
		mustAbort(t, st, "journal is append-only",
			`UPDATE journal SET type = 'x' WHERE stream_id='s' AND seq=1`)
	})

	t.Run("journal_no_delete_retention_gated", func(t *testing.T) {
		seedJournalRow(t, st, 2)
		// No retention_gate row covers seq 2 → delete aborts.
		mustAbort(t, st, "retention gate closed",
			`DELETE FROM journal WHERE stream_id='s' AND seq=2`)
	})

	t.Run("nodes_fold_owned_no_insert", func(t *testing.T) {
		// Gate closed by default: a fold_owned=1 insert aborts...
		mustAbort(t, st, "fold-owned row is write-closed",
			`INSERT INTO nodes(id, created_at, fold_owned) VALUES('n-ins', '2026-01-01', 1)`)
		// ...but a non-fold-owned insert succeeds (the guard is narrow).
		mustSucceed(t, st,
			`INSERT INTO nodes(id, created_at, fold_owned) VALUES('n-plain', '2026-01-01', 0)`)
	})

	t.Run("nodes_fold_owned_no_update_no_delete", func(t *testing.T) {
		// Seed a fold-owned node with the gate open, then close it.
		setGate(t, st, "tier_a_write_gate", 1)
		mustSucceed(t, st,
			`INSERT INTO nodes(id, created_at, fold_owned) VALUES('n-fo', '2026-01-01', 1)`)
		setGate(t, st, "tier_a_write_gate", 0)
		mustAbort(t, st, "fold-owned row is write-closed",
			`UPDATE nodes SET title = 'x' WHERE id='n-fo'`)
		mustAbort(t, st, "fold-owned row is write-closed",
			`DELETE FROM nodes WHERE id='n-fo'`)
	})

	t.Run("frontier_write_closed", func(t *testing.T) {
		// Insert aborts with the gate closed.
		mustAbort(t, st, "frontier is write-closed",
			`INSERT INTO frontier(node_id, root_id, created_at, id) VALUES('f1','r','2026-01-01','id1')`)
		// Seed with the gate open, close it, then update/delete abort.
		setGate(t, st, "tier_a_write_gate", 1)
		mustSucceed(t, st,
			`INSERT INTO frontier(node_id, root_id, created_at, id) VALUES('f1','r','2026-01-01','id1')`)
		setGate(t, st, "tier_a_write_gate", 0)
		mustAbort(t, st, "frontier is write-closed",
			`UPDATE frontier SET route = 'x' WHERE node_id='f1'`)
		mustAbort(t, st, "frontier is write-closed",
			`DELETE FROM frontier WHERE node_id='f1'`)
	})

	t.Run("snapshots_write_closed", func(t *testing.T) {
		insert := `INSERT INTO snapshots(stream_id, covered_seq, engine, reducer_version,
		     snapshot_format_version, state_hash, state, created_at)
		     VALUES('s', 5, 'lumen', 1, 1, ?, ?, '2026-01-01')`
		// Insert aborts with the gate closed.
		mustAbort(t, st, "snapshots is write-closed", insert, b32(0xCC), []byte{0x02})
		// Seed with the gate open, close it, then update/delete abort.
		setGate(t, st, "snapshot_write_gate", 1)
		mustSucceed(t, st, insert, b32(0xCC), []byte{0x02})
		setGate(t, st, "snapshot_write_gate", 0)
		mustAbort(t, st, "snapshots is write-closed",
			`UPDATE snapshots SET engine = 'v2' WHERE stream_id='s' AND covered_seq=5`)
		mustAbort(t, st, "snapshots is write-closed",
			`DELETE FROM snapshots WHERE stream_id='s' AND covered_seq=5`)
	})
}

// TestPostgresCollationIsC proves ordering-relevant TEXT columns sort by byte
// value (COLLATE "C"), matching SQLite's BINARY collation, even when the
// database's default collation is a locale. This is the frontier_route_order
// parity guarantee (§0.1 #18).
func TestPostgresCollationIsC(t *testing.T) {
	st := newPGStore(t, Options{})
	ctx := context.Background()

	setGate(t, st, "tier_a_write_gate", 1)
	// Routes chosen so byte order ('-'=45, ':'=58, 'A'=65, 'a'=97) differs from a
	// typical locale collation that would interleave case/punctuation.
	for i, route := range []string{"a", "A", ":", "-"} {
		id := string(rune('0' + i))
		mustSucceed(t, st,
			`INSERT INTO frontier(node_id, root_id, route, created_at, id) VALUES(?, 'r', ?, '2026-01-01', ?)`,
			"n"+id, route, id)
	}
	setGate(t, st, "tier_a_write_gate", 0)

	rows, err := st.ReadDB().QueryContext(ctx, `SELECT route FROM frontier ORDER BY route`)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var got []string
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	want := []string{"-", ":", "A", "a"} // pure byte order
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("ORDER BY route = %v, want byte order %v (COLLATE \"C\" not applied?)", got, want)
	}
}
