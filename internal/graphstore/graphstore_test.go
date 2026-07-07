package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

// TestAppendIdemTokenReuseWithDifferentPayloadIsLoud pins B1: reusing an idem
// token with a DIFFERENT canonical payload is not an honest replay and must fail
// loudly with ErrIdemTokenReuse, writing nothing. Reusing it with the identical
// payload still dedupes silently.
func TestAppendIdemTokenReuseWithDifferentPayloadIsLoud(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()
	const stream = "gcj-root-idem-reuse"
	const token = "act:node-1:0"

	orig := JournalEvent{Type: testType, IdemToken: token, Payload: canonPayload(t, `{"n":1}`)}
	if _, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{orig}); err != nil {
		t.Fatalf("first append: %v", err)
	}

	// Same token, DIFFERENT payload → loud sentinel, nothing written.
	diff := JournalEvent{Type: testType, IdemToken: token, Payload: canonPayload(t, `{"n":2}`)}
	_, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{diff})
	if !errors.Is(err, ErrIdemTokenReuse) {
		t.Fatalf("reuse with different payload = %v, want ErrIdemTokenReuse", err)
	}

	head, err := s.Head(ctx, stream)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 1 {
		t.Fatalf("head = %d after loud reuse, want 1 (nothing written)", head)
	}
	events, err := s.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(events) != 1 || string(events[0].Payload) != string(orig.Payload) {
		t.Fatalf("stored events = %+v, want just the original payload", events)
	}

	// Same token, IDENTICAL payload → still deduped, no error, head unchanged.
	second, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{orig})
	if err != nil {
		t.Fatalf("honest replay: %v", err)
	}
	if got, ok := second.Duplicates[0]; !ok || got != 1 {
		t.Fatalf("honest replay duplicates = %+v, want {0:1}", second.Duplicates)
	}
	if head, _ := s.Head(ctx, stream); head != 1 {
		t.Fatalf("head = %d after honest replay, want 1", head)
	}
}

// TestOpenAppliesPragmas pins S1: the durability PRAGMAs the spec mandates are
// live on the connection. A silent DSN typo dropping WAL or FULL would forfeit
// the durability guarantee, so assert them against a real connection.
func TestOpenAppliesPragmas(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "pragmas.db")
	s, err := Open(ctx, path, Options{CityID: "city-pragma"})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	// Pin to one connection so per-connection PRAGMAs are read where they were set.
	conn, err := s.DB().Conn(ctx)
	if err != nil {
		t.Fatalf("conn: %v", err)
	}
	defer func() { _ = conn.Close() }()

	var journalMode string
	if err := conn.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatalf("pragma journal_mode: %v", err)
	}
	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	var synchronous int
	if err := conn.QueryRowContext(ctx, `PRAGMA synchronous`).Scan(&synchronous); err != nil {
		t.Fatalf("pragma synchronous: %v", err)
	}
	if synchronous != 2 { // 2 == FULL
		t.Fatalf("synchronous = %d, want 2 (FULL)", synchronous)
	}

	var foreignKeys int
	if err := conn.QueryRowContext(ctx, `PRAGMA foreign_keys`).Scan(&foreignKeys); err != nil {
		t.Fatalf("pragma foreign_keys: %v", err)
	}
	if foreignKeys != 1 {
		t.Fatalf("foreign_keys = %d, want 1", foreignKeys)
	}

	var busyTimeout int
	if err := conn.QueryRowContext(ctx, `PRAGMA busy_timeout`).Scan(&busyTimeout); err != nil {
		t.Fatalf("pragma busy_timeout: %v", err)
	}
	if busyTimeout != 5000 {
		t.Fatalf("busy_timeout = %d, want 5000", busyTimeout)
	}
}

// TestOpenCityIDMismatchIsLoud pins S8: reopening an existing store under a
// different CityID is a cross-city open and must fail loudly; reopening with an
// unspecified CityID adopts the stored value.
func TestOpenCityIDMismatchIsLoud(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "city.db")

	s1, err := Open(ctx, path, Options{CityID: "city-A"})
	if err != nil {
		t.Fatalf("open city A: %v", err)
	}
	_ = s1.Close()

	if _, err := Open(ctx, path, Options{CityID: "city-B"}); !errors.Is(err, ErrCityMismatch) {
		t.Fatalf("reopen with city B = %v, want ErrCityMismatch", err)
	}

	s3, err := Open(ctx, path, Options{CityID: ""})
	if err != nil {
		t.Fatalf("reopen with unspecified city: %v", err)
	}
	t.Cleanup(func() { _ = s3.Close() })
	if s3.CityID() != "city-A" {
		t.Fatalf("adopted city id = %q, want city-A", s3.CityID())
	}
}

// TestAppendBusyIsRetryableSentinel pins S3: when another connection holds the
// single SQLite write lock, a writer maps SQLITE_BUSY to the retryable ErrBusy
// (not a raw driver error), while WAL snapshot reads keep succeeding
// concurrently. The store uses a short busy_timeout so the contention resolves
// fast.
func TestAppendBusyIsRetryableSentinel(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "busy.db")
	s, err := openStore(ctx, path, Options{CityID: "city-busy"}, 50)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.RegisterEventType(testEngine, testType)
	const stream = "gcj-root-busy"

	// Seed one committed event so there is data to read under the held lock.
	if _, err := s.Append(ctx, stream, testEngine, 0, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"seed":1}`),
	}}); err != nil {
		t.Fatalf("seed append: %v", err)
	}

	// Hold the single write lock on an independent connection.
	lockDB, err := sql.Open("sqlite", buildDSN(path, 5000))
	if err != nil {
		t.Fatalf("open lock handle: %v", err)
	}
	defer func() { _ = lockDB.Close() }()
	lockConn, err := lockDB.Conn(ctx)
	if err != nil {
		t.Fatalf("lock conn: %v", err)
	}
	if _, err := lockConn.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatalf("begin immediate on lock conn: %v", err)
	}

	// A write now contends for the held lock and must surface ErrBusy.
	_, appendErr := s.Append(ctx, stream, testEngine, 1, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"blocked":1}`),
	}})
	if !errors.Is(appendErr, ErrBusy) {
		t.Fatalf("append under held write lock = %v, want ErrBusy", appendErr)
	}

	// Reads keep working under WAL while the write lock is held.
	head, err := s.Head(ctx, stream)
	if err != nil {
		t.Fatalf("concurrent head under held lock: %v", err)
	}
	if head != 1 {
		t.Fatalf("head = %d during contention, want 1", head)
	}
	if _, err := s.ReadStream(ctx, stream, 1, 0); err != nil {
		t.Fatalf("concurrent read under held lock: %v", err)
	}

	// Release the lock; the retry now succeeds.
	if _, err := lockConn.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatalf("rollback lock conn: %v", err)
	}
	_ = lockConn.Close()
	if _, err := s.Append(ctx, stream, testEngine, 1, 0, []JournalEvent{{
		Type: testType, Payload: canonPayload(t, `{"unblocked":1}`),
	}}); err != nil {
		t.Fatalf("append after lock released: %v", err)
	}
}

// TestMapSQLiteBusy is the fast unit guard on the busy classifier: textual
// "database is locked" maps to ErrBusy, an unrelated error passes through, and
// nil stays nil.
func TestMapSQLiteBusy(t *testing.T) {
	if got := mapSQLiteBusy(nil); got != nil {
		t.Fatalf("mapSQLiteBusy(nil) = %v, want nil", got)
	}
	locked := errors.New("database is locked")
	if got := mapSQLiteBusy(locked); !errors.Is(got, ErrBusy) {
		t.Fatalf("mapSQLiteBusy(database is locked) = %v, want ErrBusy", got)
	}
	other := errors.New("some other failure")
	if got := mapSQLiteBusy(other); errors.Is(got, ErrBusy) {
		t.Fatalf("mapSQLiteBusy(other) = %v, must not be ErrBusy", got)
	}
}
