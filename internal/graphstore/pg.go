package graphstore

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// pgLockTimeoutMS is the PostgreSQL lock_timeout, the analog of SQLite's
// busy_timeout: a writer that finds a lock held waits up to this long before the
// server aborts the statement (SQLSTATE 55P03), which the Postgres dialect maps
// to the retryable ErrBusy.
const pgLockTimeoutMS = defaultBusyTimeoutMS

// openPostgres opens the journal store against a PostgreSQL DSN, mirroring Open
// but for the hosted backend. It:
//
//   - connects through the pgqmark shim driver over lib/pq, so every `?`
//     placeholder in the shared SQL is rewritten to `$N` and no query string
//     changes between backends;
//   - opens a single-connection write pool (SetMaxOpenConns(1)) and a pooled read
//     pool — the same single-serialized-writer model as SQLite; PG's multi-conn
//     pools are ready for the P6.2 per-stream advisory-lock serialization;
//   - sets lock_timeout on every connection (the busy_timeout analog);
//   - runs the Postgres migration ladder 0→schemaV4 and seeds/guards city_id
//     (ErrCityMismatch), exactly as Open;
//   - requires READ COMMITTED (see requireReadCommitted); and
//   - never lets the DSN's password reach an error or log — every path routes the
//     DSN through redactDSN.
//
// dsn is either a postgres:// URL or a keyword=value string (lib/pq parses both).
// The returned *Store is the same type Open returns, so the beads façade, the
// router, and the engines are untouched.
//
// It is deliberately UNEXPORTED in P6.1: the dialect seam it installs
// (Store.dialect = postgresDialect{}) has no readers yet. The engine paths —
// Append, the writer-lease CAS, retention truncation, snapshot writes, and the
// Tier-A projection — still call mapSQLiteBusy directly and never call
// lockStream, so a store opened here would surface a 55P03 lock_timeout as an
// untyped error instead of the retryable ErrBusy and would take no per-stream
// advisory lock, i.e. no cross-process write serialization. P6.2 wires the
// dialect into those paths (typed busy mapping via dialect.mapError + per-stream
// advisory-lock serialization via dialect.lockStream) and only then is a
// write-ready Postgres opener re-exported. Until then this is reachable only
// from package tests.
//
// extraOnConnect carries additional per-connection setup statements: production
// passes nil; tests pass a `SET search_path …` to pin a private schema for
// isolation. The setup runs on every freshly established connection (lib/pq's
// ResetSession does not clear session state, so a SET persists for the pooled
// connection's life).
func openPostgres(ctx context.Context, dsn string, opts Options, extraOnConnect []string) (*Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, fmt.Errorf("graphstore: open postgres: empty dsn")
	}

	setup := make([]string, 0, 1+len(extraOnConnect))
	setup = append(setup, fmt.Sprintf("SET lock_timeout = %d", pgLockTimeoutMS))
	setup = append(setup, extraOnConnect...)
	onConnect := func(ctx context.Context, c driver.Conn) error {
		ec, ok := c.(driver.ExecerContext)
		if !ok {
			return fmt.Errorf("graphstore: open postgres: driver connection is not an ExecerContext")
		}
		for _, stmt := range setup {
			if _, err := ec.ExecContext(ctx, stmt, nil); err != nil {
				return fmt.Errorf("graphstore: open postgres: connect setup %q: %w", stmt, err)
			}
		}
		return nil
	}

	writeDB, err := openPGPool(dsn, onConnect)
	if err != nil {
		return nil, fmt.Errorf("graphstore: opening %q: %w", redactDSN(dsn), err)
	}
	// Single serialized writer, identical to the SQLite model: one write
	// connection so the P6.2 per-stream advisory lock never contends a sibling
	// write connection inside this process.
	writeDB.SetMaxOpenConns(1)

	readDB, err := openPGPool(dsn, onConnect)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("graphstore: opening %q (read handle): %w", redactDSN(dsn), err)
	}

	if err := writeDB.PingContext(ctx); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, fmt.Errorf("graphstore: connecting %q: %w", redactDSN(dsn), err)
	}
	if err := requireReadCommitted(ctx, writeDB); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, err
	}
	if err := migrate(ctx, writeDB, postgresDialect{}); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, err
	}
	cityID, err := seedCityID(ctx, writeDB, opts.CityID)
	if err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, err
	}
	return &Store{
		writeDB: writeDB,
		readDB:  readDB,
		path:    dsn,
		dialect: postgresDialect{},
		cityID:  cityID,
		vocab:   make(map[vocabKey]struct{}),
	}, nil
}

// openPGPool builds a *sql.DB on the qmark shim connector for dsn, wiring the
// per-connection setup hook.
func openPGPool(dsn string, onConnect func(context.Context, driver.Conn) error) (*sql.DB, error) {
	connector, err := pgqmark.NewConnector(dsn, onConnect)
	if err != nil {
		// The connector-construction error can echo DSN fragments; do not embed
		// the DSN and scrub any password from the underlying message.
		return nil, fmt.Errorf("graphstore: building postgres connector: %s", scrubSecrets(err.Error()))
	}
	return sql.OpenDB(connector), nil
}

// requireReadCommitted verifies the connection's default transaction isolation
// is READ COMMITTED and refuses to open otherwise. READ COMMITTED is
// load-bearing for the per-stream CAS, not a default we happen to inherit: the
// read-decide-write entry points take a per-stream advisory lock as the first
// statement of the transaction, then read the stream head to decide the CAS.
// Under REPEATABLE READ or SERIALIZABLE the transaction snapshot is established
// by that first statement — BEFORE the previous lock holder's commit is visible
// — so the post-lock head read would be stale and the CAS would mis-fire (two
// writers computing the same seq, falling through to the PK backstop). Under
// READ COMMITTED each statement takes a fresh snapshot, so the head read after
// the lock observes the previous holder's committed state, exactly like SQLite's
// BEGIN IMMEDIATE.
//
// The store never SETs a session isolation level and always calls BeginTx with
// nil options (inheriting this default); it guards the requirement instead, so a
// server misconfigured to a stricter default fails loudly at Open rather than
// silently corrupting the CAS.
func requireReadCommitted(ctx context.Context, db *sql.DB) error {
	var iso string
	if err := db.QueryRowContext(ctx, `SHOW default_transaction_isolation`).Scan(&iso); err != nil {
		return fmt.Errorf("graphstore: open postgres: reading default_transaction_isolation: %w", err)
	}
	if !strings.EqualFold(strings.TrimSpace(iso), "read committed") {
		return fmt.Errorf("graphstore: open postgres: default_transaction_isolation is %q, but READ COMMITTED is required — REPEATABLE READ/SERIALIZABLE fixes the transaction snapshot at the advisory-lock statement and breaks the per-stream CAS", iso)
	}
	return nil
}
