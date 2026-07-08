package graphstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"github.com/lib/pq"
)

// dialect abstracts the backend-specific SQL and error handling so the journal
// engine and the beads façade can run byte-for-byte the same query strings
// against either SQLite (the default, embedded) or PostgreSQL (hosted). The
// SQLite dialect is the default and reproduces the pre-P6 behavior exactly; the
// Postgres dialect is reached through OpenPostgres, which P6.2 made write-ready by
// wiring this seam into every read-decide-write engine path.
//
// The seam is deliberately narrow. Everything backend-varying lives behind these
// methods: the DDL ladder (migrations), the schema-version probe, the
// driver-error → sentinel mapping, and the per-stream write-serialization hook.
// The chain hash, canon, fold, and every SQL string in the read/write paths are
// dialect-free.
type dialect interface {
	// name identifies the backend: "sqlite" | "postgres".
	name() string
	// migrations returns the forward-only DDL ladder for this backend. Index i
	// moves the database from schema_version i to i+1; the version numbering is
	// shared across backends (a SQLite store and a Postgres store at version N
	// have equivalent schemas).
	migrations() []string
	// schemaProbe returns a query yielding a single COUNT: 1 if the graph_meta
	// table exists in the current schema, 0 otherwise. It gates the initial
	// migration on a fresh database.
	schemaProbe() string
	// mapError maps a driver error to a store sentinel: the retryable ErrBusy for
	// lock-timeout/contention, a typed loud sentinel for the unique-violation
	// backstops (Postgres), and the error unchanged otherwise.
	mapError(err error) error
	// lockStream serializes writers of one stream inside a transaction. It is the
	// isomorphism of SQLite's BEGIN IMMEDIATE write lock, narrowed per-stream:
	//   sqlite:   no-op — the single write connection + BEGIN IMMEDIATE already
	//             serializes every writer against every other.
	//   postgres: pg_advisory_xact_lock over a hash of the stream id, held until
	//             the transaction commits/rolls back.
	// It is called as the FIRST statement of every read-decide-write transaction
	// (Append, AcquireWriterLease, WriteSnapshot, TruncateBelowAnchor,
	// RebuildTierA), before any read, so the post-lock head/gate/lease read
	// observes the previous holder's committed state and the CAS decides loudly.
	lockStream(ctx context.Context, tx *sql.Tx, streamID string) error
}

// sqliteDialect is the default backend. Every method reproduces the exact
// pre-P6 behavior, so a store opened with Open is byte-for-byte unchanged.
type sqliteDialect struct{}

var _ dialect = sqliteDialect{}

func (sqliteDialect) name() string { return "sqlite" }

// migrations returns the frozen SQLite DDL ladder (schemaV1..V4).
func (sqliteDialect) migrations() []string { return migrations }

// schemaProbe is the historical sqlite_master existence probe, unchanged.
func (sqliteDialect) schemaProbe() string {
	return `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='graph_meta'`
}

// mapError is the historical SQLITE_BUSY/LOCKED → ErrBusy mapping, unchanged.
func (sqliteDialect) mapError(err error) error { return mapSQLiteBusy(err) }

// lockStream is a no-op: BEGIN IMMEDIATE on the single write connection already
// serializes writers.
func (sqliteDialect) lockStream(context.Context, *sql.Tx, string) error { return nil }

// postgresDialect is the hosted backend. Its DDL is the plpgsql/COLLATE-"C" port
// of the SQLite schema (ddl_postgres.go); its error mapping recognizes lib/pq
// SQLSTATEs; its lockStream takes a per-stream advisory transaction lock.
type postgresDialect struct{}

var _ dialect = postgresDialect{}

func (postgresDialect) name() string { return "postgres" }

// migrations returns the Postgres DDL ladder, one rung per shared version.
func (postgresDialect) migrations() []string { return pgMigrations }

// schemaProbe checks graph_meta existence in the connection's current schema.
func (postgresDialect) schemaProbe() string {
	return `SELECT COUNT(*) FROM information_schema.tables
	         WHERE table_schema = current_schema() AND table_name = 'graph_meta'`
}

// mapError maps lib/pq SQLSTATEs to the store's sentinels (blueprint §3.4).
// Lock-timeout / deadlock / serialization failures become the retryable ErrBusy
// — the busy_timeout analog. The 23505 unique-violation cases are BACKSTOPS:
// under the P6.2 advisory-lock discipline they are unreachable, but if a
// lock-discipline bug ever let two writers compute the same seq or reuse an idem
// token, the PK / journal_idem constraint fails the loser's whole transaction
// LOUDLY (typed, never a silent lost update) rather than silently. Anything else
// is returned unchanged so sentinel classification stays errors.Is-based.
func (postgresDialect) mapError(err error) error {
	if err == nil {
		return nil
	}
	var pe *pq.Error
	if !errors.As(err, &pe) {
		return err
	}
	switch pe.Code {
	case "55P03", // lock_not_available (lock_timeout fired) — the busy_timeout analog
		"40P01", // deadlock_detected
		"40001": // serialization_failure (defensive; not expected at READ COMMITTED)
		return fmt.Errorf("%w: %w", ErrBusy, err)
	case "23505": // unique_violation — expected-unreachable backstops
		switch pe.Constraint {
		case "journal_pkey":
			return fmt.Errorf("%w: journal (stream_id, seq) unique-violation backstop tripped — advisory-lock discipline bug: %w", ErrWrongExpectedVersion, err)
		case "journal_idem":
			return fmt.Errorf("%w: journal_idem unique-violation backstop tripped — advisory-lock discipline bug: %w", ErrIdemTokenReuse, err)
		}
	}
	return err
}

// lockStream takes a per-stream advisory lock held to end of transaction. It is
// written with `?` and rewritten to `$1` by the pgqmark shim. Key collisions
// (hashtextextended is 64-bit) merely over-serialize two streams — never a
// correctness problem.
func (postgresDialect) lockStream(ctx context.Context, tx *sql.Tx, streamID string) error {
	_, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtextextended(?, 0))`, streamID)
	return err
}

// pgKeywordPasswordRE matches a password=… token in a keyword-form DSN, either
// quoted ('…' with ” escapes) or bare (up to whitespace).
var pgKeywordPasswordRE = regexp.MustCompile(`(?i)password\s*=\s*('(?:[^']|'')*'|\S+)`)

// pgURLUserinfoRE matches the userinfo password in a postgres:// URL wherever it
// appears — including embedded in a larger error string.
var pgURLUserinfoRE = regexp.MustCompile(`(postgres(?:ql)?://[^:/@\s]+):[^@/\s]+@`)

// redactDSN removes a credential from a DSN (or any string that may embed one)
// before it appears in an error or a log. For a well-formed postgres:// URL it
// returns the canonical redacted URL; otherwise it scrubs userinfo and
// keyword-form passwords in place. It is the identity on anything without a
// credential, so a SQLite file path passes through byte-for-byte — the
// byte-identity guarantee for the SQLite Close/error paths.
func redactDSN(dsn string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			changed := false
			if u.User != nil {
				if _, hasPW := u.User.Password(); hasPW {
					u.User = url.UserPassword(u.User.Username(), "xxxxx")
					changed = true
				}
			}
			if q := u.Query(); q.Get("password") != "" {
				q.Set("password", "xxxxx")
				u.RawQuery = q.Encode()
				changed = true
			}
			if changed {
				return u.String()
			}
			return dsn
		}
		// Parse failed: fall through to in-place scrubbing rather than risk
		// leaking the raw string.
	}
	return scrubSecrets(dsn)
}

// scrubSecrets replaces any postgres URL userinfo password and any keyword-form
// password=… token in s. It operates anywhere in the string, so it is safe on
// driver error messages that embed a DSN fragment.
func scrubSecrets(s string) string {
	s = pgURLUserinfoRE.ReplaceAllString(s, `$1:xxxxx@`)
	s = pgKeywordPasswordRE.ReplaceAllString(s, "password=xxxxx")
	return s
}
