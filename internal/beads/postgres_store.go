package beads

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"

	_ "github.com/lib/pq" // database/sql driver, registers "postgres"
)

// PostgresStore is a Postgres-backed Store — the shared, server-DB backend the
// infra/beads split is moving toward (a class selects it via
// [beads.classes.<class>].backend = "postgres").
//
// SCAFFOLD STATUS: the lifecycle (Open/Ping/CloseStore), the per-class options,
// the id-prefix accessor, and the schema are implemented; the CRUD/query/dep
// methods are stubbed and return errPostgresNotImplemented. Fill them in by
// porting the SQL from internal/beads/sqlite_store.go method-by-method (the
// schema below mirrors the SQLite tables, so the queries translate closely), then
// run the conformance suite (postgres_store_conformance_test.go) against a real
// Postgres via GC_TEST_POSTGRES_DSN to prove parity with the other backends.
//
// Why Postgres needs no controller-mediated single-writer path (unlike SQLite):
// it is a client-server DB with native concurrent multi-process writes and MVCC,
// so the relocated infra classes whose writers span processes (nudges, sessions)
// flip directly — no controller write API. A standard database/sql pool with
// per-connection transactions is the whole concurrency model.
type PostgresStore struct {
	db        *sql.DB
	prefix    string
	emit      RowChangeEmitter
	closeOnce sync.Once
}

// postgresStoreOptions configures OpenPostgresStore. It mirrors
// SQLiteStoreOptions so the two backends stay option-compatible at the call site.
type postgresStoreOptions struct {
	prefix string
	emit   RowChangeEmitter
}

// PostgresStoreOption customizes OpenPostgresStore.
type PostgresStoreOption func(*postgresStoreOptions)

// WithPostgresStoreIDPrefix sets the generated bead ID prefix (e.g. "gcn" for the
// nudges class). Distinct prefixes keep cross-store ids unambiguous.
func WithPostgresStoreIDPrefix(prefix string) PostgresStoreOption {
	return func(o *postgresStoreOptions) {
		if strings.TrimSpace(prefix) != "" {
			o.prefix = normalizeIDPrefix(prefix)
		}
	}
}

// WithPostgresStoreRecorder registers an emitter invoked after every committed
// mutation with a low-level RowChange — the same store-edge event source the
// SQLite store exposes (see WithSQLiteStoreRecorder), so the controller's event
// translation is backend-agnostic.
func WithPostgresStoreRecorder(emit RowChangeEmitter) PostgresStoreOption {
	return func(o *postgresStoreOptions) {
		o.emit = emit
	}
}

// errPostgresNotImplemented marks a PostgresStore method that is scaffolded but
// not yet ported from the SQLite implementation.
var errPostgresNotImplemented = errors.New("beads: PostgresStore method not yet implemented (scaffold)")

// postgresDefaultPrefix mirrors sqliteDefaultPrefix.
const postgresDefaultPrefix = "gc"

// OpenPostgresStore opens a Postgres-backed bead store at dsn (a lib/pq DSN or
// connection URI, e.g. "postgres://user:pass@host:5432/db?sslmode=disable"),
// verifies connectivity, and ensures the schema exists. The caller closes it via
// CloseStore.
func OpenPostgresStore(dsn string, opts ...PostgresStoreOption) (Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("beads: OpenPostgresStore: empty dsn")
	}
	cfg := postgresStoreOptions{prefix: postgresDefaultPrefix}
	for _, opt := range opts {
		opt(&cfg)
	}

	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("beads: OpenPostgresStore: open: %w", err)
	}
	if err := db.Ping(); err != nil {
		_ = db.Close() //nolint:errcheck // best-effort on the failed-open path
		return nil, fmt.Errorf("beads: OpenPostgresStore: ping: %w", err)
	}
	s := &PostgresStore{db: db, prefix: cfg.prefix, emit: cfg.emit}
	if err := s.initSchema(); err != nil {
		_ = db.Close() //nolint:errcheck // best-effort on the failed-open path
		return nil, fmt.Errorf("beads: OpenPostgresStore: init schema: %w", err)
	}
	return s, nil
}

// postgresSchema mirrors the SQLite store's tables (sqlite_store.go) in Postgres
// dialect: the full bead is stored as JSON in bead_json with the predicate columns
// promoted for indexed queries. Timestamps are unix nanoseconds in BIGINT to match
// the SQLite store's integer time storage exactly (so a dolt/sqlite->postgres
// migration is a value-preserving copy).
var postgresSchema = []string{
	`CREATE TABLE IF NOT EXISTS kv (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS beads (
		id TEXT PRIMARY KEY,
		tier TEXT NOT NULL CHECK (tier IN ('main','wisp')),
		title TEXT NOT NULL,
		status TEXT NOT NULL,
		issue_type TEXT NOT NULL,
		priority BIGINT,
		created_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		assignee TEXT NOT NULL DEFAULT '',
		from_agent TEXT NOT NULL DEFAULT '',
		parent_id TEXT NOT NULL DEFAULT '',
		ref TEXT NOT NULL DEFAULT '',
		description TEXT NOT NULL DEFAULT '',
		bead_json TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS labels (
		bead_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
		label TEXT NOT NULL,
		PRIMARY KEY(bead_id, label)
	)`,
	`CREATE TABLE IF NOT EXISTS metadata (
		bead_id TEXT NOT NULL REFERENCES beads(id) ON DELETE CASCADE,
		meta_key TEXT NOT NULL,
		meta_value TEXT NOT NULL,
		PRIMARY KEY(bead_id, meta_key)
	)`,
	`CREATE TABLE IF NOT EXISTS deps (
		issue_id TEXT NOT NULL,
		depends_on_id TEXT NOT NULL,
		dep_type TEXT NOT NULL,
		PRIMARY KEY(issue_id, depends_on_id, dep_type)
	)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_tier_status ON beads(tier, status)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_type ON beads(issue_type)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_assignee ON beads(assignee)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_parent ON beads(parent_id)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_created ON beads(created_at)`,
	`CREATE INDEX IF NOT EXISTS idx_beads_updated ON beads(updated_at)`,
	`CREATE INDEX IF NOT EXISTS idx_labels_label ON labels(label)`,
	`CREATE INDEX IF NOT EXISTS idx_metadata_key_value ON metadata(meta_key, meta_value)`,
	`CREATE INDEX IF NOT EXISTS idx_deps_issue ON deps(issue_id)`,
	`CREATE INDEX IF NOT EXISTS idx_deps_depends ON deps(depends_on_id)`,
}

func (s *PostgresStore) initSchema() error {
	for _, stmt := range postgresSchema {
		if _, err := s.db.Exec(stmt); err != nil {
			return fmt.Errorf("exec %q: %w", firstLine(stmt), err)
		}
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// IDPrefix returns the bead ID prefix this store mints, so the cross-store
// prefix resolver (internal/storeref) routes ids to it.
func (s *PostgresStore) IDPrefix() string {
	if s == nil {
		return ""
	}
	return s.prefix
}

// Ping verifies the database connection is operational.
func (s *PostgresStore) Ping() error {
	if s == nil || s.db == nil {
		return ErrStoreClosed
	}
	return s.db.Ping()
}

// CloseStore closes the underlying connection pool. Safe to call multiple times.
func (s *PostgresStore) CloseStore() error {
	var err error
	s.closeOnce.Do(func() {
		if s.db != nil {
			err = s.db.Close()
		}
	})
	return err
}

// --- Store CRUD/query surface (SCAFFOLD: port from sqlite_store.go) ---

// Create persists a new bead. Port: sqlite_store.go Create (insert into beads +
// labels + metadata, mint id from prefix+seq, emit RowCreated after commit).
func (s *PostgresStore) Create(b Bead) (Bead, error) { return Bead{}, errPostgresNotImplemented }

// Get retrieves a bead by ID. Port: select bead_json + labels + metadata, return
// ErrNotFound when absent.
func (s *PostgresStore) Get(id string) (Bead, error) { return Bead{}, errPostgresNotImplemented }

// Update modifies fields of an existing bead (emit RowUpdated, or RowClosed on a
// true open->closed transition — see RowChange).
func (s *PostgresStore) Update(id string, opts UpdateOpts) error { return errPostgresNotImplemented }

// Close sets a bead's status to "closed" (RowClosed only on a real transition).
func (s *PostgresStore) Close(id string) error { return errPostgresNotImplemented }

// Reopen sets a closed bead's status back to "open".
func (s *PostgresStore) Reopen(id string) error { return errPostgresNotImplemented }

// CloseAll closes multiple beads and stamps metadata on each.
func (s *PostgresStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	return 0, errPostgresNotImplemented
}

// List returns beads matching the query. Port: build a WHERE from ListQuery, then
// hydrate via ApplyListQuery (the shared in-memory finisher the other stores reuse).
func (s *PostgresStore) List(query ListQuery) ([]Bead, error) { return nil, errPostgresNotImplemented }

// ListOpen returns non-closed beads (or beads of the given status).
func (s *PostgresStore) ListOpen(status ...string) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// Ready returns open, unblocked, actionable beads — apply IsReadyCandidateForTier
// + the dependency/assignee filters, mirroring the SQLite store's Ready.
func (s *PostgresStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// Children returns beads whose ParentID matches.
func (s *PostgresStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// ListByLabel returns beads carrying an exact label.
func (s *PostgresStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// ListByAssignee returns beads assigned to agent with the given status.
func (s *PostgresStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// ListByMetadata returns beads whose metadata contains all key/value pairs.
func (s *PostgresStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return nil, errPostgresNotImplemented
}

// SetMetadata sets one metadata pair.
func (s *PostgresStore) SetMetadata(id, key, value string) error { return errPostgresNotImplemented }

// SetMetadataBatch sets multiple metadata pairs atomically (Postgres has real
// transactions, so unlike the external stores this can be a single tx).
func (s *PostgresStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return errPostgresNotImplemented
}

// Tx runs fn inside a single Postgres transaction. Port: BEGIN, build a tx-bound
// Tx implementation (Update/SetMetadataBatch/Close), COMMIT/ROLLBACK.
func (s *PostgresStore) Tx(commitMsg string, fn func(tx Tx) error) error {
	return errPostgresNotImplemented
}

// Delete permanently removes a bead (labels/metadata cascade via FK).
func (s *PostgresStore) Delete(id string) error { return errPostgresNotImplemented }

// DepAdd records issueID depends-on dependsOnID.
func (s *PostgresStore) DepAdd(issueID, dependsOnID, depType string) error {
	return errPostgresNotImplemented
}

// DepRemove removes a dependency edge.
func (s *PostgresStore) DepRemove(issueID, dependsOnID string) error {
	return errPostgresNotImplemented
}

// DepList returns dependency edges for a bead in the given direction.
func (s *PostgresStore) DepList(id, direction string) ([]Dep, error) {
	return nil, errPostgresNotImplemented
}

// Compile-time proof the scaffold satisfies the full Store contract — every
// method is present, so filling in the bodies is the only remaining work.
var _ Store = (*PostgresStore)(nil)
