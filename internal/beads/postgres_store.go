package beads

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"strings"
	"sync"
	"time"

	"github.com/lib/pq" // database/sql driver ("postgres") + NewConnector/QuoteIdentifier
)

// PostgresStore is a Postgres-backed Store — the shared, server-DB backend the
// infra/beads split is moving toward (a class selects it via
// [beads.classes.<class>].backend = "postgres").
//
// Storage model mirrors SQLiteStore (internal/beads/sqlite_store.go): the full
// bead is persisted as JSON in beads.bead_json with the predicate columns promoted
// for indexed queries, and labels/metadata/deps live in side tables. Reads decode
// bead_json and the shared finishers (ApplyListQuery/Matches, sortBeadsForQuery,
// IsReadyCandidateForTier) apply the rest, so behavior matches the other backends
// — proven by the shared conformance suite (postgres_store_conformance_test.go).
//
// Why Postgres needs no controller-mediated single-writer path (unlike SQLite):
// it is a client-server DB with native concurrent multi-process writes and MVCC,
// so the relocated infra classes whose writers span processes (nudges, sessions)
// flip directly. Auto-ids are minted from a native per-schema SEQUENCE (nextval),
// which is concurrency-safe across processes — unlike an in-memory counter, two
// writers can never mint the same id. Per-class isolation is a Postgres SCHEMA
// (one DB, schema-per-class); the store sets search_path on every connection.
type PostgresStore struct {
	db        *sql.DB
	prefix    string
	schema    string // per-class schema (search_path); "" or "public" for the default
	emit      RowChangeEmitter
	closeOnce sync.Once
}

// postgresStoreOptions configures OpenPostgresStore. It mirrors
// SQLiteStoreOptions so the two backends stay option-compatible at the call site.
type postgresStoreOptions struct {
	prefix string
	schema string
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

// WithPostgresStoreSchema scopes the store to a Postgres schema (search_path),
// giving each coordination class its own namespace in one shared database. Empty
// or "public" uses the default schema. The schema must already be provisioned
// (ProvisionPostgres / `gc beads postgres init`) — Open verifies, it does not create.
func WithPostgresStoreSchema(schema string) PostgresStoreOption {
	return func(o *postgresStoreOptions) {
		o.schema = strings.TrimSpace(schema)
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

// postgresDefaultPrefix mirrors sqliteDefaultPrefix.
const postgresDefaultPrefix = "gc"

// Connection-pool defaults for the shared server. Bounded so many gc processes do
// not exhaust Postgres's connection slots.
const (
	postgresMaxOpenConns    = 8
	postgresMaxIdleConns    = 4
	postgresConnMaxLifetime = 30 * time.Minute
)

var _ ConditionalAssignmentReleaser = (*PostgresStore)(nil)

// OpenPostgresStore opens a Postgres-backed bead store at dsn (a lib/pq DSN or
// connection URI, e.g. "postgres://user:pass@host:5432/db?sslmode=disable"). It
// configures a bounded connection pool, sets search_path to the configured schema
// on every connection, verifies connectivity, and verifies the schema is already
// PROVISIONED — it does NOT run DDL. Provisioning (ProvisionPostgres /
// `gc beads postgres init`) is a separate, advisory-lock-guarded step, so opening a
// store never races schema creation across the many processes that share the DB.
func OpenPostgresStore(dsn string, opts ...PostgresStoreOption) (Store, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("beads: OpenPostgresStore: empty dsn")
	}
	cfg := postgresStoreOptions{prefix: postgresDefaultPrefix}
	for _, opt := range opts {
		opt(&cfg)
	}

	base, err := pq.NewConnector(dsn)
	if err != nil {
		return nil, fmt.Errorf("beads: OpenPostgresStore: connector: %w", err)
	}
	db := sql.OpenDB(searchPathConnector{base: base, schema: cfg.schema})
	db.SetMaxOpenConns(postgresMaxOpenConns)
	db.SetMaxIdleConns(postgresMaxIdleConns)
	db.SetConnMaxLifetime(postgresConnMaxLifetime)
	if err := db.Ping(); err != nil {
		_ = db.Close() //nolint:errcheck // best-effort on the failed-open path
		return nil, fmt.Errorf("beads: OpenPostgresStore: ping: %w", err)
	}
	s := &PostgresStore{db: db, prefix: cfg.prefix, schema: cfg.schema, emit: cfg.emit}
	if err := s.verifyProvisioned(); err != nil {
		_ = db.Close() //nolint:errcheck // best-effort on the failed-open path
		return nil, err
	}
	return s, nil
}

// searchPathConnector wraps a lib/pq connector and runs `SET search_path` on every
// new connection, so unqualified table/sequence names resolve to the class's schema
// regardless of which pooled connection serves a query. Schema-isolation without
// schema-qualifying every statement (and DSN-agnostic — no fragile DSN munging).
type searchPathConnector struct {
	base   driver.Connector
	schema string
}

func (c searchPathConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.base.Connect(ctx)
	if err != nil {
		return nil, err
	}
	if c.schema != "" && c.schema != "public" {
		execer, ok := conn.(driver.ExecerContext)
		if !ok {
			_ = conn.Close() //nolint:errcheck
			return nil, fmt.Errorf("beads: postgres connection does not support setting search_path")
		}
		if _, err := execer.ExecContext(ctx, `SET search_path = `+pq.QuoteIdentifier(c.schema), nil); err != nil {
			_ = conn.Close() //nolint:errcheck
			return nil, fmt.Errorf("beads: setting search_path=%q: %w", c.schema, err)
		}
	}
	return conn, nil
}

func (c searchPathConnector) Driver() driver.Driver { return c.base.Driver() }

// verifyProvisioned errors when the configured schema's bead tables/sequence are
// absent, pointing the operator at the provisioning step rather than silently
// running DDL on a hot path.
func (s *PostgresStore) verifyProvisioned() error {
	schema := s.schema
	if schema == "" {
		schema = "public"
	}
	var beadsTbl, beadSeq sql.NullString
	if err := s.db.QueryRowContext(context.Background(),
		`SELECT to_regclass($1), to_regclass($2)`,
		schema+".beads", schema+".bead_seq",
	).Scan(&beadsTbl, &beadSeq); err != nil {
		return fmt.Errorf("beads: OpenPostgresStore: checking schema %q: %w", schema, err)
	}
	if !beadsTbl.Valid || !beadSeq.Valid {
		return fmt.Errorf("beads: OpenPostgresStore: schema %q is not provisioned (run `gc beads postgres init`)", schema)
	}
	return nil
}

// provisionStatements returns the schema-qualified DDL that creates the bead store
// in a Postgres schema: the native id SEQUENCE plus the tables (mirroring
// sqlite_store.go — full bead JSON + promoted predicate columns + label/metadata/dep
// side tables, BIGINT unix-nano timestamps for value-preserving migration) and
// indexes. All IF NOT EXISTS, so it is idempotent.
func provisionStatements(schema string) []string {
	q := pq.QuoteIdentifier(schema)
	return []string{
		`CREATE SCHEMA IF NOT EXISTS ` + q,
		`CREATE SEQUENCE IF NOT EXISTS ` + q + `.bead_seq`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.kv (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.beads (
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
		`CREATE TABLE IF NOT EXISTS ` + q + `.labels (
			bead_id TEXT NOT NULL REFERENCES ` + q + `.beads(id) ON DELETE CASCADE,
			label TEXT NOT NULL,
			PRIMARY KEY(bead_id, label)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.metadata (
			bead_id TEXT NOT NULL REFERENCES ` + q + `.beads(id) ON DELETE CASCADE,
			meta_key TEXT NOT NULL,
			meta_value TEXT NOT NULL,
			PRIMARY KEY(bead_id, meta_key)
		)`,
		`CREATE TABLE IF NOT EXISTS ` + q + `.deps (
			issue_id TEXT NOT NULL,
			depends_on_id TEXT NOT NULL,
			dep_type TEXT NOT NULL,
			PRIMARY KEY(issue_id, depends_on_id, dep_type)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_tier_status ON ` + q + `.beads(tier, status)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_type ON ` + q + `.beads(issue_type)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_assignee ON ` + q + `.beads(assignee)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_parent ON ` + q + `.beads(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_created ON ` + q + `.beads(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_beads_updated ON ` + q + `.beads(updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_labels_label ON ` + q + `.labels(label)`,
		`CREATE INDEX IF NOT EXISTS idx_metadata_key_value ON ` + q + `.metadata(meta_key, meta_value)`,
		`CREATE INDEX IF NOT EXISTS idx_deps_issue ON ` + q + `.deps(issue_id)`,
		`CREATE INDEX IF NOT EXISTS idx_deps_depends ON ` + q + `.deps(depends_on_id)`,
	}
}

// ProvisionPostgres creates (idempotently) the bead-store schema, sequence, tables,
// and indexes for one coordination class in its Postgres schema. It is the
// separated, privileged provisioning step Open assumes has run. A session advisory
// lock keyed on the schema serializes concurrent inits so Postgres's non-atomic
// CREATE ... IF NOT EXISTS can never race across the processes sharing the DB. Safe
// to re-run. Empty schema provisions the default "public" schema.
func ProvisionPostgres(dsn, schema string) error {
	if strings.TrimSpace(dsn) == "" {
		return errors.New("beads: ProvisionPostgres: empty dsn")
	}
	if strings.TrimSpace(schema) == "" {
		schema = "public"
	}
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return fmt.Errorf("beads: ProvisionPostgres: open: %w", err)
	}
	defer db.Close() //nolint:errcheck
	ctx := context.Background()
	conn, err := db.Conn(ctx) // the advisory lock + DDL must share one connection
	if err != nil {
		return fmt.Errorf("beads: ProvisionPostgres: conn: %w", err)
	}
	defer conn.Close() //nolint:errcheck
	lockArg := "gascity-beads-provision:" + schema
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_lock(hashtext($1))`, lockArg); err != nil {
		return fmt.Errorf("beads: ProvisionPostgres: advisory lock: %w", err)
	}
	defer conn.ExecContext(ctx, `SELECT pg_advisory_unlock(hashtext($1))`, lockArg) //nolint:errcheck
	for _, stmt := range provisionStatements(schema) {
		if _, err := conn.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("beads: ProvisionPostgres: exec %q: %w", firstLine(stmt), err)
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

func (s *PostgresStore) emitRowChange(rc RowChange) {
	if s.emit != nil {
		s.emit(rc)
	}
}

// IDPrefix returns the bead ID prefix this store mints, so the cross-store prefix
// resolver (internal/storeref) routes ids to it.
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

func (s *PostgresStore) normalizeCreate(b Bead) Bead {
	b = cloneBead(b)
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}
	return b
}

// Create persists a new bead. Auto-ids come from the native per-schema SEQUENCE
// (nextval — concurrency-safe across processes), so two writers can never mint the
// same id. A caller-pinned id is inserted atomically (ON CONFLICT DO NOTHING); an
// already-present pinned id is a hard duplicate-id error, and the sequence floor is
// lifted past the pinned suffix so a later nextval never re-mints it. Unqualified
// names resolve to the store's schema via the connection search_path.
func (s *PostgresStore) Create(b Bead) (Bead, error) {
	autoID := b.ID == ""
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Bead{}, fmt.Errorf("postgres create: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	stored := s.normalizeCreate(b)
	if autoID {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT nextval('bead_seq')`).Scan(&n); err != nil {
			return Bead{}, fmt.Errorf("postgres create: nextval: %w", err)
		}
		stored.ID = fmt.Sprintf("%s-%d", s.prefix, n)
	}
	inserted, err := s.insertBeadTx(ctx, tx, stored)
	if err != nil {
		return Bead{}, err
	}
	if !inserted {
		return Bead{}, fmt.Errorf("creating bead %q: duplicate id", stored.ID)
	}
	if !autoID {
		if suffix := int64(numericIDSuffix(stored.ID)); suffix > 0 {
			if _, err := tx.ExecContext(ctx,
				`SELECT setval('bead_seq', GREATEST((SELECT last_value FROM bead_seq), $1))`, suffix); err != nil {
				return Bead{}, fmt.Errorf("postgres create: setval: %w", err)
			}
		}
	}
	for _, dep := range depsFromBeadFields(stored) {
		if err := s.depAddTx(ctx, tx, dep.IssueID, dep.DependsOnID, dep.Type); err != nil {
			return Bead{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Bead{}, fmt.Errorf("postgres create: commit: %w", err)
	}
	s.emitRowChange(RowChange{ID: stored.ID, Type: stored.Type, Op: RowCreated})
	return cloneBead(stored), nil
}

// insertBeadTx atomically inserts a NEW bead row (ON CONFLICT DO NOTHING) plus its
// labels and metadata. Returns false (no error) when the id already exists, so
// Create can report a duplicate-id error without a separate racy existence probe.
// Distinct from upsertBeadTx (ON CONFLICT DO UPDATE), which Update uses.
func (s *PostgresStore) insertBeadTx(ctx context.Context, tx *sql.Tx, b Bead) (bool, error) {
	payload, err := json.Marshal(b)
	if err != nil {
		return false, fmt.Errorf("postgres marshal bead %q: %w", b.ID, err)
	}
	tier := "main"
	if b.Ephemeral {
		tier = "wisp"
	}
	var priority any
	if b.Priority != nil {
		priority = *b.Priority
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO beads(id,tier,title,status,issue_type,priority,created_at,updated_at,assignee,from_agent,parent_id,ref,description,bead_json)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT(id) DO NOTHING`,
		b.ID, tier, b.Title, b.Status, b.Type, priority, b.CreatedAt.UnixNano(), sqliteUnixNanoOrZero(b.UpdatedAt),
		b.Assignee, b.From, b.ParentID, b.Ref, b.Description, string(payload))
	if err != nil {
		return false, fmt.Errorf("postgres insert bead %q: %w", b.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, nil
	}
	for _, label := range b.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(bead_id,label) VALUES($1,$2) ON CONFLICT DO NOTHING`, b.ID, label); err != nil {
			return false, fmt.Errorf("postgres insert label for %q: %w", b.ID, err)
		}
	}
	for k, v := range b.Metadata {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(bead_id,meta_key,meta_value) VALUES($1,$2,$3)`, b.ID, k, v); err != nil {
			return false, fmt.Errorf("postgres insert metadata for %q: %w", b.ID, err)
		}
	}
	return true, nil
}

func (s *PostgresStore) upsertBeadTx(ctx context.Context, tx *sql.Tx, b Bead) error {
	payload, err := json.Marshal(b)
	if err != nil {
		return fmt.Errorf("postgres marshal bead %q: %w", b.ID, err)
	}
	tier := "main"
	if b.Ephemeral {
		tier = "wisp"
	}
	var priority any
	if b.Priority != nil {
		priority = *b.Priority
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO beads(id,tier,title,status,issue_type,priority,created_at,updated_at,assignee,from_agent,parent_id,ref,description,bead_json)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT(id) DO UPDATE SET
			tier=excluded.tier,
			title=excluded.title,
			status=excluded.status,
			issue_type=excluded.issue_type,
			priority=excluded.priority,
			created_at=excluded.created_at,
			updated_at=excluded.updated_at,
			assignee=excluded.assignee,
			from_agent=excluded.from_agent,
			parent_id=excluded.parent_id,
			ref=excluded.ref,
			description=excluded.description,
			bead_json=excluded.bead_json`,
		b.ID, tier, b.Title, b.Status, b.Type, priority, b.CreatedAt.UnixNano(), sqliteUnixNanoOrZero(b.UpdatedAt),
		b.Assignee, b.From, b.ParentID, b.Ref, b.Description, string(payload))
	if err != nil {
		return fmt.Errorf("postgres upsert bead %q: %w", b.ID, err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM labels WHERE bead_id=$1`, b.ID); err != nil {
		return fmt.Errorf("postgres replace labels for %q: %w", b.ID, err)
	}
	for _, label := range b.Labels {
		if _, err := tx.ExecContext(ctx, `INSERT INTO labels(bead_id,label) VALUES($1,$2) ON CONFLICT DO NOTHING`, b.ID, label); err != nil {
			return fmt.Errorf("postgres insert label for %q: %w", b.ID, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM metadata WHERE bead_id=$1`, b.ID); err != nil {
		return fmt.Errorf("postgres replace metadata for %q: %w", b.ID, err)
	}
	for k, v := range b.Metadata {
		if _, err := tx.ExecContext(ctx, `INSERT INTO metadata(bead_id,meta_key,meta_value) VALUES($1,$2,$3)`, b.ID, k, v); err != nil {
			return fmt.Errorf("postgres insert metadata for %q: %w", b.ID, err)
		}
	}
	return nil
}

// Get retrieves a bead by ID.
func (s *PostgresStore) Get(id string) (Bead, error) {
	row := s.db.QueryRowContext(context.Background(), `SELECT bead_json FROM beads WHERE id=$1`, id)
	b, err := scanSQLiteBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, err)
	}
	return b, nil
}

func (s *PostgresStore) getTx(ctx context.Context, tx *sql.Tx, id string) (Bead, error) {
	row := tx.QueryRowContext(ctx, `SELECT bead_json FROM beads WHERE id=$1`, id)
	b, err := scanSQLiteBead(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return b, err
}

// Update modifies fields of an existing bead. Emits RowClosed on a true
// open->closed transition, RowUpdated otherwise; a metadata-only no-op emits
// nothing (matching the SQLite store).
func (s *PostgresStore) Update(id string, opts UpdateOpts) error {
	op := RowUpdated
	noop := false
	var changedType string
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres update: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	b, err := s.getTx(ctx, tx, id)
	if err != nil {
		return err
	}
	if isMetadataOnlyNoop(b, opts) {
		return nil
	}
	prevStatus := b.Status
	if opts.Title != nil {
		b.Title = *opts.Title
	}
	if opts.Status != nil {
		b.Status = *opts.Status
	}
	if opts.Type != nil {
		b.Type = *opts.Type
	}
	if opts.Priority != nil {
		b.Priority = cloneIntPtr(opts.Priority)
	}
	if opts.Description != nil {
		b.Description = *opts.Description
	}
	if opts.ParentID != nil {
		b.ParentID = *opts.ParentID
	}
	if opts.Assignee != nil {
		b.Assignee = *opts.Assignee
	}
	if len(opts.Metadata) > 0 {
		if b.Metadata == nil {
			b.Metadata = make(map[string]string, len(opts.Metadata))
		}
		for k, v := range opts.Metadata {
			b.Metadata[k] = v
		}
	}
	if len(opts.Labels) > 0 {
		b.Labels = append(b.Labels, opts.Labels...)
	}
	if len(opts.RemoveLabels) > 0 {
		remove := make(map[string]bool, len(opts.RemoveLabels))
		for _, label := range opts.RemoveLabels {
			remove[label] = true
		}
		filtered := b.Labels[:0]
		for _, label := range b.Labels {
			if !remove[label] {
				filtered = append(filtered, label)
			}
		}
		b.Labels = filtered
	}
	b.UpdatedAt = time.Now()
	changedType = b.Type
	if b.Status == "closed" && prevStatus != "closed" {
		op = RowClosed
	}
	if err := s.upsertBeadTx(ctx, tx, b); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres update: commit: %w", err)
	}
	if !noop {
		s.emitRowChange(RowChange{ID: id, Type: changedType, Op: op})
	}
	return nil
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still has
// the expected assignee.
func (s *PostgresStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("postgres release-if-current: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	b, err := s.getTx(ctx, tx, id)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if b.Status != "in_progress" || b.Assignee != expectedAssignee {
		return false, nil
	}
	b.Status = "open"
	b.Assignee = ""
	b.UpdatedAt = time.Now()
	if err := s.upsertBeadTx(ctx, tx, b); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

// Close sets a bead's status to closed.
func (s *PostgresStore) Close(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("closing bead %q: %w", id, err)
	}
	if b.Status == "closed" {
		return nil
	}
	return s.Update(id, UpdateOpts{Status: ptrTo("closed")})
}

// Reopen sets a bead's status to open.
func (s *PostgresStore) Reopen(id string) error {
	b, err := s.Get(id)
	if err != nil {
		return fmt.Errorf("reopening bead %q: %w", id, err)
	}
	if b.Status == "open" {
		return nil
	}
	return s.Update(id, UpdateOpts{Status: ptrTo("open")})
}

// CloseAll closes multiple beads and applies metadata to each closed bead.
func (s *PostgresStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		b, err := s.Get(id)
		if err != nil {
			if errors.Is(err, ErrNotFound) {
				continue
			}
			return closed, err
		}
		if b.Status == "closed" {
			continue
		}
		if err := s.Update(id, UpdateOpts{Status: ptrTo("closed"), Metadata: maps.Clone(metadata)}); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// List returns beads matching the query.
func (s *PostgresStore) List(query ListQuery) ([]Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	sqlText, args := postgresListSQL(query)
	rows, err := s.db.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing postgres beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	for rows.Next() {
		b, err := scanSQLiteBead(rows)
		if err != nil {
			return nil, fmt.Errorf("listing postgres beads: %w", err)
		}
		if !query.Matches(b) {
			continue
		}
		result = append(result, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("listing postgres beads: %w", err)
	}
	sortBeadsForQuery(result, query.Sort)
	if query.Limit > 0 && len(result) > query.Limit {
		result = result[:query.Limit]
	}
	return result, nil
}

func postgresListSQL(q ListQuery) (string, []any) {
	var args []any
	ph := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where := []string{}
	switch q.TierMode {
	case TierWisps, TierBoth:
		// NoHistory rows live in the main tier but are part of the logical wisp
		// tier, so final tier filtering happens after decode.
	default:
		where = append(where, "tier='main'")
	}
	if q.Status != "" {
		where = append(where, "status="+ph(q.Status))
	} else if !q.IncludeClosed {
		where = append(where, "status <> 'closed'")
	}
	if q.Type != "" {
		where = append(where, "issue_type="+ph(q.Type))
	}
	if q.Assignee != "" {
		where = append(where, "assignee="+ph(q.Assignee))
	}
	if q.ParentID != "" {
		where = append(where, "parent_id="+ph(q.ParentID))
	}
	if len(q.ParentIDs) > 0 {
		placeholders := make([]string, len(q.ParentIDs))
		for i, pid := range q.ParentIDs {
			placeholders[i] = ph(pid)
		}
		where = append(where, "parent_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if !q.CreatedBefore.IsZero() {
		where = append(where, "created_at < "+ph(q.CreatedBefore.UnixNano()))
	}
	if !q.UpdatedBefore.IsZero() {
		where = append(where, "COALESCE(NULLIF(updated_at, 0), created_at) < "+ph(q.UpdatedBefore.UnixNano()))
	}
	if q.Label != "" {
		where = append(where, "EXISTS (SELECT 1 FROM labels l WHERE l.bead_id=beads.id AND l.label="+ph(q.Label)+")")
	}
	for k, v := range q.Metadata {
		where = append(where, "beads.id IN (SELECT m.bead_id FROM metadata m WHERE m.meta_key="+ph(k)+" AND m.meta_value="+ph(v)+")")
	}
	sqlText := "SELECT bead_json FROM beads"
	if len(where) > 0 {
		sqlText += " WHERE " + strings.Join(where, " AND ")
	}
	switch q.Sort {
	case SortCreatedAsc:
		sqlText += " ORDER BY created_at ASC, id ASC"
	case SortCreatedDesc:
		sqlText += " ORDER BY created_at DESC, id DESC"
	}
	if q.Limit > 0 && q.TierMode != TierWisps {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	return sqlText, args
}

// ListOpen returns non-closed beads in creation order by default.
func (s *PostgresStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true, Sort: SortCreatedAsc}
	if len(status) > 0 {
		query.Status = status[0]
	}
	return s.List(query)
}

// Ready returns open, unblocked actionable beads from the requested tier.
func (s *PostgresStore) Ready(query ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(query)
	var args []any
	ph := func(v any) string { args = append(args, v); return fmt.Sprintf("$%d", len(args)) }
	where := []string{
		"b.status='open'",
		`b.issue_type NOT IN ('merge-request','gate','molecule','step','message','session','agent','role','rig')`,
		`NOT EXISTS (
			SELECT 1 FROM deps d
			LEFT JOIN beads blocker ON blocker.id=d.depends_on_id
			WHERE d.issue_id=b.id
			  AND d.dep_type IN ('blocks','waits-for','conditional-blocks')
			  AND COALESCE(blocker.status, '') <> 'closed'
		  )`,
	}
	switch q.TierMode {
	case TierWisps, TierBoth:
		// Filter after decode so NoHistory rows in the main tier stay visible to
		// logical wisp-tier reads.
	default:
		where = append(where, "b.tier='main'")
	}
	sqlText := `SELECT b.bead_json FROM beads b WHERE ` + strings.Join(where, " AND ")
	if q.Assignee != "" {
		sqlText += " AND b.assignee=" + ph(q.Assignee)
	}
	sqlText += " ORDER BY b.created_at ASC, b.id ASC"
	if q.Limit > 0 && q.TierMode != TierWisps {
		sqlText += fmt.Sprintf(" LIMIT %d", q.Limit)
	}
	rows, err := s.db.QueryContext(context.Background(), sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("listing postgres ready beads: %w", err)
	}
	defer rows.Close() //nolint:errcheck
	var result []Bead
	now := time.Now().UTC()
	for rows.Next() {
		b, err := scanSQLiteBead(rows)
		if err != nil {
			return nil, err
		}
		if !IsReadyCandidateForTier(b, now, q.TierMode) {
			continue
		}
		result = append(result, b)
		if q.Limit > 0 && len(result) >= q.Limit {
			break
		}
	}
	return result, rows.Err()
}

// Children returns beads whose ParentID matches the given ID.
func (s *PostgresStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedAsc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByLabel returns beads matching an exact label string.
func (s *PostgresStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to the given agent with the given status.
func (s *PostgresStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{
		Assignee: assignee,
		Status:   status,
		Limit:    limit,
		Sort:     SortCreatedDesc,
	})
}

// ListByMetadata returns beads whose metadata contains all key-value pairs.
func (s *PostgresStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		Sort:          SortCreatedDesc,
		TierMode:      TierModeFromOpts(opts),
	})
}

// SetMetadata sets a key-value metadata pair on a bead.
func (s *PostgresStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch atomically sets multiple metadata keys on a bead.
func (s *PostgresStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if len(kvs) == 0 {
		return nil
	}
	return s.Update(id, UpdateOpts{Metadata: maps.Clone(kvs)})
}

// Tx executes fn sequentially against the store.
func (s *PostgresStore) Tx(_ string, fn func(tx Tx) error) error {
	return runSequentialTx(s, fn)
}

// Delete permanently removes a bead and its indexed rows.
func (s *PostgresStore) Delete(id string) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres delete: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	// Capture the type before removing the row so the deleted RowChange can be
	// translated without re-reading the (now gone) bead.
	var deletedType string
	_ = tx.QueryRowContext(ctx, `SELECT issue_type FROM beads WHERE id=$1`, id).Scan(&deletedType)
	res, err := tx.ExecContext(ctx, `DELETE FROM beads WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("deleting bead %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("deleting bead %q: %w", id, ErrNotFound)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM deps WHERE issue_id=$1 OR depends_on_id=$2`, id, id); err != nil {
		return fmt.Errorf("deleting bead %q deps: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("postgres delete: commit: %w", err)
	}
	s.emitRowChange(RowChange{ID: id, Type: deletedType, Op: RowDeleted})
	return nil
}

// DepAdd records a dependency edge.
func (s *PostgresStore) DepAdd(issueID, dependsOnID, depType string) error {
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("postgres dep add: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck
	if err := s.depAddTx(ctx, tx, issueID, dependsOnID, depType); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *PostgresStore) depAddTx(ctx context.Context, tx *sql.Tx, issueID, dependsOnID, depType string) error {
	if depType == "" {
		depType = "blocks"
	}
	_, err := tx.ExecContext(ctx, `
		INSERT INTO deps(issue_id, depends_on_id, dep_type) VALUES($1,$2,$3)
		ON CONFLICT(issue_id, depends_on_id, dep_type) DO NOTHING`,
		issueID, dependsOnID, depType)
	if err != nil {
		return fmt.Errorf("adding dependency %s -> %s: %w", issueID, dependsOnID, err)
	}
	return nil
}

// DepRemove removes a dependency edge.
func (s *PostgresStore) DepRemove(issueID, dependsOnID string) error {
	_, err := s.db.ExecContext(context.Background(), `DELETE FROM deps WHERE issue_id=$1 AND depends_on_id=$2`, issueID, dependsOnID)
	return err
}

// DepList returns dependency edges for a bead in the given direction.
func (s *PostgresStore) DepList(id, direction string) ([]Dep, error) {
	col := "issue_id"
	if direction == "up" {
		col = "depends_on_id"
	}
	rows, err := s.db.QueryContext(context.Background(),
		`SELECT issue_id, depends_on_id, dep_type FROM deps WHERE `+col+`=$1`, id)
	if err != nil {
		return nil, fmt.Errorf("listing dependencies for %q: %w", id, err)
	}
	defer rows.Close() //nolint:errcheck
	var out []Dep
	for rows.Next() {
		var d Dep
		if err := rows.Scan(&d.IssueID, &d.DependsOnID, &d.Type); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// Compile-time proof the store satisfies the full Store contract.
var _ Store = (*PostgresStore)(nil)
