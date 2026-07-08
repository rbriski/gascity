// Package graphstore implements the journal-core of the graph substrate: an
// append-only, hash-chained event log backed by a single SQLite-WAL database
// file, with a writer lease, forward-only migrations, and a closed event
// vocabulary. It is the durable persistence layer beneath the graph engines and
// knows nothing about beads; the adapter that surfaces it as a beads.Store
// capability is a separate slice.
package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"modernc.org/sqlite" // pure-Go SQLite driver, CGO_ENABLED=0 safe
)

// defaultBusyTimeoutMS is the spec-mandated SQLite busy_timeout: a writer that
// finds the write lock held waits up to this long before SQLITE_BUSY.
const defaultBusyTimeoutMS = 5000

// Options configures Open.
type Options struct {
	// CityID seeds graph_meta.city_id on a fresh store. It is the chain-genesis
	// input (D-SEC-1) and is immutable once written: opening an existing store
	// keeps the stored value and ignores this field. May be "" (genesis then
	// derives from stream_id alone).
	CityID string
}

// Store is the journal engine: an append-only event log with a writer lease,
// backed by a single SQLite-WAL database file. It knows nothing about beads; the
// adapter that surfaces it as a beads.Store capability is a separate slice.
//
// Concurrency model — single serialized writer per process. writeDB is capped
// at one open connection (SetMaxOpenConns(1)), so Append, the writer-lease CAS,
// and migrations serialize on the pool and every BEGIN IMMEDIATE takes the
// write lock without contending with a sibling connection. readDB is a separate
// pooled handle for ReadStream/Head/Verify that serves WAL snapshot reads
// concurrently with an in-flight write. Cross-process (or cross-handle)
// contention on the single SQLite write lock surfaces as the retryable ErrBusy
// rather than a raw driver error; callers may retry. This process assumes it is
// the only writer to the file — safety across processes still rests on
// expectedVersion and the writer lease, not on this pool shape.
type Store struct {
	writeDB *sql.DB
	readDB  *sql.DB
	// path is the SQLite file path or, for a Postgres store, the connection DSN.
	// It may carry a credential, so every error/log use routes through redactDSN.
	path string
	// dialect is the backend seam. Open defaults it to sqliteDialect{}, so every
	// existing SQLite call site and test is byte-for-byte unchanged; openPostgres
	// sets postgresDialect{}.
	dialect dialect
	cityID  string

	mu    sync.RWMutex
	vocab map[vocabKey]struct{}

	// rebuildAfterRead, when non-nil, is invoked inside RebuildTierA immediately
	// after the from-genesis stream read and before the write transaction opens.
	// It is a test-only seam for driving the read/write TOCTOU window
	// deterministically; production leaves it nil.
	rebuildAfterRead func()
}

type vocabKey struct {
	engine string
	typ    string
}

// Open opens (creating if necessary) the journal store at path, applies the
// connection PRAGMAs required by the spec (WAL, synchronous=FULL,
// foreign_keys=ON, busy_timeout=5000) on every pooled connection, runs the
// forward-only migration ladder, and seeds city_id when absent. Every
// transaction begins IMMEDIATE so Append and the lease CAS take the write lock up
// front. See the Store doc comment for the single-writer-per-process model.
func Open(ctx context.Context, path string, opts Options) (*Store, error) {
	return openStore(ctx, path, opts, defaultBusyTimeoutMS)
}

// openStore is Open with a configurable busy_timeout so tests can force
// SQLITE_BUSY without a multi-second wait. Production always uses
// defaultBusyTimeoutMS.
func openStore(ctx context.Context, path string, opts Options, busyTimeoutMS int) (*Store, error) {
	if path == "" {
		return nil, fmt.Errorf("graphstore: open: empty path")
	}
	dsn := buildDSN(path, busyTimeoutMS)
	writeDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("graphstore: opening %q: %w", path, err)
	}
	// Single serialized writer: one connection means BEGIN IMMEDIATE never
	// races a sibling write connection inside this process.
	writeDB.SetMaxOpenConns(1)
	readDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		_ = writeDB.Close()
		return nil, fmt.Errorf("graphstore: opening %q (read handle): %w", path, err)
	}
	if err := writeDB.PingContext(ctx); err != nil {
		_ = writeDB.Close()
		_ = readDB.Close()
		return nil, fmt.Errorf("graphstore: connecting %q: %w", path, err)
	}
	if err := migrate(ctx, writeDB, sqliteDialect{}); err != nil {
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
		path:    path,
		dialect: sqliteDialect{},
		cityID:  cityID,
		vocab:   make(map[vocabKey]struct{}),
	}, nil
}

// buildDSN builds the modernc.org/sqlite DSN. Each _pragma value runs as a
// PRAGMA on every new connection, so the whole pool is configured identically;
// _txlock=immediate makes every BEGIN a BEGIN IMMEDIATE.
func buildDSN(path string, busyTimeoutMS int) string {
	q := url.Values{}
	q.Add("_pragma", "busy_timeout("+strconv.Itoa(busyTimeoutMS)+")")
	q.Add("_pragma", "foreign_keys(ON)")
	q.Add("_pragma", "journal_mode(WAL)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Set("_txlock", "immediate")
	return "file:" + path + "?" + q.Encode()
}

// seedCityID writes city_id when the store has none (immutable thereafter) and
// returns the effective value. Opening an existing store with a non-empty want
// that differs from the stored city_id is a cross-city open and fails loudly
// with ErrCityMismatch (S8); want == "" adopts whatever is stored.
func seedCityID(ctx context.Context, db *sql.DB, want string) (string, error) {
	if want != "" {
		if _, err := db.ExecContext(ctx,
			`INSERT INTO graph_meta(key, value) VALUES('city_id', ?)
			 ON CONFLICT(key) DO NOTHING`,
			want,
		); err != nil {
			return "", fmt.Errorf("graphstore: seeding city_id: %w", err)
		}
	}
	var got string
	err := db.QueryRowContext(ctx,
		`SELECT value FROM graph_meta WHERE key='city_id'`,
	).Scan(&got)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("graphstore: reading city_id: %w", err)
	}
	if want != "" && got != want {
		return "", fmt.Errorf("graphstore: opening store for city %q but it belongs to city %q: %w", want, got, ErrCityMismatch)
	}
	return got, nil
}

// mapSQLiteBusy maps a SQLite SQLITE_BUSY / SQLITE_LOCKED (or the textual
// "database is locked" / "database table is locked") into the retryable ErrBusy
// sentinel, preserving the original error in the chain. Any other error is
// returned unchanged.
func mapSQLiteBusy(err error) error {
	if err == nil {
		return nil
	}
	var se *sqlite.Error
	if errors.As(err, &se) {
		switch se.Code() & 0xff { // strip extended-result-code high bits
		case 5, 6: // SQLITE_BUSY, SQLITE_LOCKED
			return fmt.Errorf("%w: %w", ErrBusy, err)
		}
	}
	if msg := err.Error(); strings.Contains(msg, "database is locked") ||
		strings.Contains(msg, "database table is locked") {
		return fmt.Errorf("%w: %w", ErrBusy, err)
	}
	return err
}

// CityID returns the store's chain-genesis city id.
func (s *Store) CityID() string { return s.cityID }

// DB exposes the single-connection write handle for tests and sibling engine
// packages within internal/graphstore. internal/beads' JournalStore is now a
// production dependent: it writes and mints through DB() but MUST route every
// read through ReadDB(). A façade read issued on the write pool from inside a
// write Tx would try to check out a second connection from a pool capped at one,
// self-deadlocking on the city's only write connection. Not part of any public
// contract.
func (s *Store) DB() *sql.DB { return s.writeDB }

// ReadDB exposes the pooled WAL read handle. It serves snapshot-consistent reads
// concurrently with an in-flight write without contending for the single write
// connection, so JournalStore routes all façade reads (Get/List/Ready/DepList/
// Ping/Count/hydration) through it. A read here observes the last committed
// state — reads inside a live write Tx therefore see the pre-commit snapshot,
// matching the in-tree read-committed contract. Not part of any public contract.
func (s *Store) ReadDB() *sql.DB { return s.readDB }

// Close closes both underlying database handles.
func (s *Store) Close() error {
	werr := s.writeDB.Close()
	rerr := s.readDB.Close()
	if werr != nil {
		return fmt.Errorf("graphstore: closing %q (write): %w", redactDSN(s.path), werr)
	}
	if rerr != nil {
		return fmt.Errorf("graphstore: closing %q (read): %w", redactDSN(s.path), rerr)
	}
	return nil
}

// RegisterEventType adds (engine, typ) to the closed vocabulary this store will
// accept at Append (I-5). Registration is additive and idempotent.
func (s *Store) RegisterEventType(engine, typ string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.vocab[vocabKey{engine: engine, typ: typ}] = struct{}{}
}

// isRegistered reports whether (engine, typ) is in the closed vocabulary.
func (s *Store) isRegistered(engine, typ string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.vocab[vocabKey{engine: engine, typ: typ}]
	return ok
}
