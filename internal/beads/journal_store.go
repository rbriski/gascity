package beads

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	beadslib "github.com/steveyegge/beads"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// journalIDPrefix is the bead ID prefix this store mints and owns. GraphClass
// beads reside under "gcg" (08-blocker-resolutions §B1 — supersedes the earlier
// "gcj" proposal in 01/06): the single graph store keeps one prefix rather than
// registering a second.
const journalIDPrefix = "gcg"

// journalIDMarker is the fixed structural marker journal-minted ids carry
// immediately after the prefix, within the shared "gcg" namespace.
//
// ID shape: gcg-j<seq>  (e.g. "gcg-j1", "gcg-j42"), where <seq> is a positive
// decimal from the store's monotonic counter. The leading "gcg-" keeps the B1/08
// single-prefix decision (storeref still parses the prefix as "gcg" — everything
// before the first "-"), while the "j" makes a journal-store id structurally
// distinguishable from a legacy graph-store gcg id by inspection of the id alone.
// P1.5's residence router and audits may rely on this shape: an id whose suffix
// begins with "j" is journal-resident; any other gcg suffix is legacy-store.
// Legacy graph-store ids must never mint a suffix beginning with "j".
const journalIDMarker = "j"

// journalIDSeqKey is the graph_meta key holding the monotonic gcg ID counter.
const journalIDSeqKey = "gcg_id_seq"

// ErrFoldOwnedWriteClosed reports that a mutation targeted a fold-owned Tier-A
// row. The journal-primary fold path owns fold_owned=1 rows and rewrites them
// only through the write-gated applier (I-14); the beads.Store façade may only
// mutate its own fold_owned=0 rows, so a mutation against a fold-owned bead
// fails loudly instead of silently corrupting the projection.
var ErrFoldOwnedWriteClosed = errors.New("bead is fold-owned (Tier-A write-closed)")

// journalReadyBlockingDepClause is the SQL IN-list of dependency types that block
// a bead from Ready until the target closes (mirrors readyBlockingDependencyTypes).
const journalReadyBlockingDepClause = `('blocks','waits-for','conditional-blocks')`

// journalNodeColumns is the ordered `nodes` column set every read hydrates from.
const journalNodeColumns = `id, title, status, bead_type, priority, description, ` +
	`assignee, from_actor, parent_id, ref, created_at, updated_at, defer_until, storage_tier`

// JournalStore surfaces the graph substrate's Tier-A projection tables as a
// beads.Store. It is mutation-primary: it writes fold_owned=0 rows directly to
// nodes/node_labels/node_metadata/edges in one SQLite transaction per operation,
// distinct from the journal-primary fold path (the executor's) that owns
// fold_owned=1 rows. Reads project the same tables; the hot Ready path computes
// dependency-blocking readiness live over nodes+edges (a dangling dependency
// BLOCKS, D-4), so no separate frontier materialization is maintained for the
// façade's own rows.
type JournalStore struct {
	gs  *graphstore.Store
	db  *sql.DB // single-connection write pool: writes, mint, Tx callbacks.
	rdb *sql.DB // pooled WAL read handle: all façade reads (H1 — never the write pool).
}

var (
	_ Store                         = (*JournalStore)(nil)
	_ GraphApplyStore               = (*JournalStore)(nil)
	_ StorageGraphApplyStore        = (*JournalStore)(nil)
	_ EphemeralGraphApplyStore      = (*JournalStore)(nil)
	_ StorageCreateStore            = (*JournalStore)(nil)
	_ ConditionalAssignmentReleaser = (*JournalStore)(nil)
	_ AtomicTxStore                 = (*JournalStore)(nil)
	_ Counter                       = (*JournalStore)(nil)
)

// NewJournalStore wraps an open graphstore.Store as a beads.Store. The façade
// writes through the store's single serialized write connection, so its
// transactions serialize with the journal engine's own writes; it reads through
// the store's pooled WAL read handle so a read never contends for that single
// write connection (a read on the write pool inside a write Tx would deadlock —
// H1).
func NewJournalStore(gs *graphstore.Store) *JournalStore {
	return &JournalStore{gs: gs, db: gs.DB(), rdb: gs.ReadDB()}
}

// IDPrefix returns the bead ID prefix this store owns, without the trailing "-".
func (s *JournalStore) IDPrefix() string { return journalIDPrefix }

// CloseStore releases the underlying graphstore handles (both SQLite pools).
// It implements the close interface closeBeadStoreHandle expects, so a handle
// that loses the one-shot open race is actually closed instead of leaking its
// SQLite connections for the life of the process.
func (s *JournalStore) CloseStore() error {
	return s.gs.Close()
}

// AtomicTx reports that Tx rolls the whole callback back on error: it runs in one
// SQLite transaction.
func (s *JournalStore) AtomicTx() bool { return true }

// SupportsEphemeralGraphApply reports that ApplyGraphPlan can materialize a whole
// graph directly into ephemeral storage.
func (s *JournalStore) SupportsEphemeralGraphApply() bool { return true }

// journalQueryer is the read/write surface shared by *sql.DB and *sql.Tx so the
// standalone and in-transaction code paths run identical SQL.
type journalQueryer interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func journalNow() time.Time { return time.Now().UTC() }

// journalNullableInt returns a *int as a nullable SQL argument (NULL when nil).
func journalNullableInt(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}

func journalFormatTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func journalParseTime(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("parsing stored timestamp %q: %w", s, err)
	}
	return t.UTC(), nil
}

func journalDeferArg(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t.UTC().Format(time.RFC3339Nano)
}

// journalTierFromBead maps a bead's ephemeral/no-history hints to the
// storage_tier column value.
func journalTierFromBead(b Bead) string {
	switch {
	case b.Ephemeral:
		return "ephemeral"
	case b.NoHistory:
		return "no_history"
	default:
		return "history"
	}
}

// resolveStorage applies an explicit StorageClass over a bead's storage hints.
// StorageDefault preserves the caller's Ephemeral/NoHistory fields; every other
// class overrides them (policy middleware has already classified the bead).
func resolveJournalStorage(b Bead, sc StorageClass) (Bead, error) {
	if sc == StorageDefault {
		return b, nil
	}
	ephemeral, noHistory, err := graphStorageFlags(sc)
	if err != nil {
		return Bead{}, err
	}
	b.Ephemeral = ephemeral
	b.NoHistory = noHistory
	return b, nil
}

// withTx runs fn inside one SQLite transaction, rolling back on any error so
// every façade mutation is atomic.
func (s *JournalStore) withTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("journal store: begin: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // no-op after a successful Commit
	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("journal store: commit: %w", err)
	}
	return nil
}

// guardMutable reports whether id names a bead this façade may mutate. It returns
// ErrNotFound when the bead is absent and ErrFoldOwnedWriteClosed when the bead
// is a fold-owned Tier-A row (I-14).
func journalGuardMutable(ctx context.Context, q journalQueryer, id string) error {
	var foldOwned int
	err := q.QueryRowContext(ctx, `SELECT fold_owned FROM nodes WHERE id = ?`, id).Scan(&foldOwned)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	if err != nil {
		return fmt.Errorf("checking bead %q: %w", id, err)
	}
	if foldOwned == 1 {
		return fmt.Errorf("bead %q: %w", id, ErrFoldOwnedWriteClosed)
	}
	return nil
}

// mintID advances and returns the next gcg-N identifier inside tx.
func (s *JournalStore) mintID(ctx context.Context, tx *sql.Tx) (string, error) {
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO graph_meta(key, value) VALUES(?, '0') ON CONFLICT(key) DO NOTHING`,
		journalIDSeqKey,
	); err != nil {
		return "", fmt.Errorf("journal store: seeding id sequence: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE graph_meta SET value = CAST(value AS INTEGER) + 1 WHERE key = ?`,
		journalIDSeqKey,
	); err != nil {
		return "", fmt.Errorf("journal store: advancing id sequence: %w", err)
	}
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT CAST(value AS INTEGER) FROM graph_meta WHERE key = ?`, journalIDSeqKey,
	).Scan(&n); err != nil {
		return "", fmt.Errorf("journal store: reading id sequence: %w", err)
	}
	return fmt.Sprintf("%s-%s%d", journalIDPrefix, journalIDMarker, n), nil
}

// --- Reads -----------------------------------------------------------------

// hydrateSnapshot runs the read + N+1 child hydration inside one read-only,
// DEFERRED transaction on the pooled WAL read handle. Two invariants ride on
// this: every façade read is served off the read pool, so it never contends for
// the single write connection (H1); and the node SELECT and its per-bead
// label/metadata/dependency/blocked follow-ups all observe one WAL snapshot, so a
// concurrent commit can never tear a hydrated bead (M2).
func (s *JournalStore) hydrateSnapshot(ctx context.Context, extraWhere string, args []any) ([]Bead, error) {
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("journal store: begin read snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot
	beads, err := s.hydrateWhere(ctx, tx, extraWhere, args)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("journal store: commit read snapshot: %w", err)
	}
	return beads, nil
}

// Get retrieves a bead by ID, returning a wrapped ErrNotFound when absent.
func (s *JournalStore) Get(id string) (Bead, error) {
	ctx := context.Background()
	beads, err := s.hydrateSnapshot(ctx, "n.id = ?", []any{id})
	if err != nil {
		return Bead{}, err
	}
	if len(beads) == 0 {
		return Bead{}, fmt.Errorf("bead %q: %w", id, ErrNotFound)
	}
	return beads[0], nil
}

// List returns beads matching the query. A filterless query without AllowScan is
// rejected with ErrQueryRequiresScan.
func (s *JournalStore) List(query ListQuery) ([]Bead, error) {
	return s.listCtx(context.Background(), query)
}

func (s *JournalStore) listCtx(ctx context.Context, query ListQuery) ([]Bead, error) {
	if err := query.Validate(); err != nil {
		return nil, err
	}
	if !query.HasFilter() && !query.AllowScan {
		return nil, fmt.Errorf("listing beads: %w", ErrQueryRequiresScan)
	}
	where, args := journalListWhere(query)
	beads, err := s.hydrateSnapshot(ctx, where, args)
	if err != nil {
		return nil, err
	}
	return ApplyListQuery(beads, query), nil
}

// journalListWhere pushes the cheap, high-selectivity ListQuery filters into SQL.
// The remaining conjunctive filters (labels, metadata, time bounds, batched
// parent/assignee sets) are applied by ApplyListQuery over the hydrated beads.
func journalListWhere(query ListQuery) (string, []any) {
	var clauses []string
	var args []any
	if query.Status != "" {
		clauses = append(clauses, "n.status = ?")
		args = append(args, query.Status)
	} else if !query.IncludeClosed {
		clauses = append(clauses, "n.status <> 'closed'")
	}
	if query.Type != "" {
		clauses = append(clauses, "n.bead_type = ?")
		args = append(args, query.Type)
	}
	if query.Assignee != "" {
		clauses = append(clauses, "n.assignee = ?")
		args = append(args, query.Assignee)
	}
	if query.ParentID != "" {
		clauses = append(clauses, "n.parent_id = ?")
		args = append(args, query.ParentID)
	}
	if tier := journalTierClause(query.TierMode); tier != "" {
		clauses = append(clauses, tier)
	}
	return strings.Join(clauses, " AND "), args
}

// journalTierClause returns the storage_tier SQL predicate for a tier mode, or ""
// for TierBoth (no filter). Mirrors IsReadyCandidateForTier / ListQuery.Matches.
func journalTierClause(mode TierMode) string {
	switch mode {
	case TierWisps:
		return "n.storage_tier IN ('ephemeral','no_history')"
	case TierBoth:
		return ""
	default: // TierIssues: durable rows only; no-history stays visible, ephemeral hidden.
		return "n.storage_tier <> 'ephemeral'"
	}
}

// hydrateWhere selects fold_owned=0 nodes matching extraWhere and hydrates each
// into a full Bead (labels, metadata, dependencies, live is_blocked).
func (s *JournalStore) hydrateWhere(ctx context.Context, q journalQueryer, extraWhere string, args []any) ([]Bead, error) {
	sqlText := "SELECT " + journalNodeColumns + " FROM nodes n WHERE n.fold_owned = 0"
	if extraWhere != "" {
		sqlText += " AND " + extraWhere
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("journal store: querying nodes: %w", err)
	}
	beads, err := scanBeadRows(rows)
	if err != nil {
		return nil, err
	}
	for i := range beads {
		if err := s.hydrateChildren(ctx, q, &beads[i]); err != nil {
			return nil, err
		}
	}
	return beads, nil
}

func scanBeadRows(rows *sql.Rows) ([]Bead, error) {
	defer func() { _ = rows.Close() }()
	var beads []Bead
	for rows.Next() {
		var (
			b          Bead
			priority   sql.NullInt64
			deferUntil sql.NullString
			createdAt  string
			updatedAt  string
			tier       string
		)
		if err := rows.Scan(
			&b.ID, &b.Title, &b.Status, &b.Type, &priority, &b.Description,
			&b.Assignee, &b.From, &b.ParentID, &b.Ref, &createdAt, &updatedAt,
			&deferUntil, &tier,
		); err != nil {
			return nil, fmt.Errorf("journal store: scanning node: %w", err)
		}
		if priority.Valid {
			b.Priority = journalReadPriority(int(priority.Int64))
		}
		created, err := journalParseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("bead %q: %w", b.ID, err)
		}
		b.CreatedAt = created
		updated, err := journalParseTime(updatedAt)
		if err != nil {
			return nil, fmt.Errorf("bead %q: %w", b.ID, err)
		}
		b.UpdatedAt = updated
		if deferUntil.Valid {
			d, err := journalParseTime(deferUntil.String)
			if err != nil {
				return nil, fmt.Errorf("bead %q: %w", b.ID, err)
			}
			if !d.IsZero() {
				b.DeferUntil = &d
			}
		}
		switch tier {
		case "ephemeral":
			b.Ephemeral = true
		case "no_history":
			b.NoHistory = true
		}
		beads = append(beads, b)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal store: iterating nodes: %w", err)
	}
	return beads, nil
}

// journalReadPriority mirrors nativePriorityFromIssue: an explicit P2 reads back
// as nil, matching the sparse Priority round-trip the other stores expose.
func journalReadPriority(p int) *int {
	if p == 2 {
		return nil
	}
	v := p
	return &v
}

func (s *JournalStore) hydrateChildren(ctx context.Context, q journalQueryer, b *Bead) error {
	labels, err := journalLoadLabels(ctx, q, b.ID)
	if err != nil {
		return err
	}
	b.Labels = labels
	metadata, err := journalLoadMetadata(ctx, q, b.ID)
	if err != nil {
		return err
	}
	b.Metadata = metadata
	deps, err := journalLoadDeps(ctx, q, b.ID, "down")
	if err != nil {
		return err
	}
	b.Dependencies = deps
	blocked, err := journalIsBlocked(ctx, q, b.ID)
	if err != nil {
		return err
	}
	b.IsBlocked = cloneBoolPtr(&blocked)
	return nil
}

func journalLoadLabels(ctx context.Context, q journalQueryer, id string) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT label FROM node_labels WHERE node_id = ? ORDER BY label`, id)
	if err != nil {
		return nil, fmt.Errorf("journal store: loading labels for %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var labels []string
	for rows.Next() {
		var label string
		if err := rows.Scan(&label); err != nil {
			return nil, fmt.Errorf("journal store: scanning label for %q: %w", id, err)
		}
		labels = append(labels, label)
	}
	return labels, rows.Err()
}

func journalLoadMetadata(ctx context.Context, q journalQueryer, id string) (StringMap, error) {
	rows, err := q.QueryContext(ctx, `SELECT key, value FROM node_metadata WHERE node_id = ?`, id)
	if err != nil {
		return nil, fmt.Errorf("journal store: loading metadata for %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var metadata StringMap
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, fmt.Errorf("journal store: scanning metadata for %q: %w", id, err)
		}
		if metadata == nil {
			metadata = make(StringMap)
		}
		metadata[key] = value
	}
	return metadata, rows.Err()
}

func journalLoadDeps(ctx context.Context, q journalQueryer, id, direction string) ([]Dep, error) {
	var (
		sqlText string
		build   func(other, depType string) Dep
	)
	if direction == "up" {
		sqlText = `SELECT from_id, dep_type FROM edges WHERE to_id = ? ORDER BY from_id, dep_type`
		build = func(other, depType string) Dep { return Dep{IssueID: other, DependsOnID: id, Type: depType} }
	} else {
		sqlText = `SELECT to_id, dep_type FROM edges WHERE from_id = ? ORDER BY to_id, dep_type`
		build = func(other, depType string) Dep { return Dep{IssueID: id, DependsOnID: other, Type: depType} }
	}
	rows, err := q.QueryContext(ctx, sqlText, id)
	if err != nil {
		return nil, fmt.Errorf("journal store: loading deps for %q: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	var deps []Dep
	for rows.Next() {
		var other, depType string
		if err := rows.Scan(&other, &depType); err != nil {
			return nil, fmt.Errorf("journal store: scanning dep for %q: %w", id, err)
		}
		deps = append(deps, build(other, depType))
	}
	return deps, rows.Err()
}

// journalIsBlocked reports whether id has any blocking dependency whose target is
// unclosed OR absent (dangling dependency BLOCKS, D-4).
func journalIsBlocked(ctx context.Context, q journalQueryer, id string) (bool, error) {
	var blocked bool
	err := q.QueryRowContext(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM edges e
			LEFT JOIN nodes t ON t.id = e.to_id
			WHERE e.from_id = ?
			  AND e.dep_type IN `+journalReadyBlockingDepClause+`
			  AND (t.id IS NULL OR t.status <> 'closed')
		)`, id).Scan(&blocked)
	if err != nil {
		return false, fmt.Errorf("journal store: computing blocked for %q: %w", id, err)
	}
	return blocked, nil
}

// Ready returns open, unblocked, actionable beads. The SQL narrows to open,
// non-blocked fold_owned=0 rows in the requested tier; Go applies the remaining
// candidate filters (excluded types/labels, defer_until), canonical ready
// ordering, assignee, and limit.
func (s *JournalStore) Ready(queries ...ReadyQuery) ([]Bead, error) {
	q := readyQueryFromArgs(queries)
	ctx := context.Background()
	where := "n.status = 'open' AND NOT " + journalBlockedPredicate("n.id")
	if tier := journalTierClause(q.TierMode); tier != "" {
		where += " AND " + tier
	}
	candidates, err := s.hydrateSnapshot(ctx, where, nil)
	if err != nil {
		return nil, err
	}
	now := journalNow()
	var result []Bead
	for _, b := range candidates {
		if !IsReadyCandidateForTier(b, now, q.TierMode) {
			continue
		}
		if q.Assignee != "" && b.Assignee != q.Assignee {
			continue
		}
		result = append(result, b)
	}
	sortBeadsReadyOrder(result)
	if q.Limit > 0 && len(result) > q.Limit {
		result = result[:q.Limit]
	}
	return result, nil
}

func journalBlockedPredicate(fromExpr string) string {
	return `EXISTS (
		SELECT 1 FROM edges e
		LEFT JOIN nodes t ON t.id = e.to_id
		WHERE e.from_id = ` + fromExpr + `
		  AND e.dep_type IN ` + journalReadyBlockingDepClause + `
		  AND (t.id IS NULL OR t.status <> 'closed')
	)`
}

// ListOpen returns non-closed beads by default, or beads with the given status.
func (s *JournalStore) ListOpen(status ...string) ([]Bead, error) {
	query := ListQuery{AllowScan: true}
	if len(status) > 0 {
		query.Status = status[0]
		if status[0] == "closed" {
			query.IncludeClosed = true
		}
	}
	return s.List(query)
}

// Children returns beads whose parent_id matches parentID.
func (s *JournalStore) Children(parentID string, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		ParentID:      parentID,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByLabel returns beads carrying an exact label.
func (s *JournalStore) ListByLabel(label string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Label:         label,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// ListByAssignee returns beads assigned to assignee with the requested status.
func (s *JournalStore) ListByAssignee(assignee, status string, limit int) ([]Bead, error) {
	return s.List(ListQuery{Assignee: assignee, Status: status, Limit: limit, AllowScan: true})
}

// ListByMetadata returns beads whose metadata contains every filter pair.
func (s *JournalStore) ListByMetadata(filters map[string]string, limit int, opts ...QueryOpt) ([]Bead, error) {
	return s.List(ListQuery{
		Metadata:      filters,
		Limit:         limit,
		IncludeClosed: HasOpt(opts, IncludeClosed),
		AllowScan:     true,
		TierMode:      TierModeFromOpts(opts),
	})
}

// Count returns the number of beads List would return for query, minus beads
// whose Type is in excludeTypes.
//
// P1.4: Count fully hydrates every matching bead and counts in Go rather than
// pushing a COUNT(*) down to SQL. The conformance suite pins whether Count must
// avoid the hydration cost.
func (s *JournalStore) Count(ctx context.Context, query ListQuery, excludeTypes ...string) (int, error) {
	beads, err := s.listCtx(ctx, query)
	if err != nil {
		return 0, err
	}
	excluded := make(map[string]bool, len(excludeTypes))
	for _, t := range excludeTypes {
		excluded[t] = true
	}
	count := 0
	for _, b := range beads {
		if excluded[b.Type] {
			continue
		}
		count++
	}
	return count, nil
}

// Ping verifies the underlying database is reachable. Like every façade read it
// probes the read handle, never the single write connection (H1).
func (s *JournalStore) Ping() error {
	return s.rdb.PingContext(context.Background())
}

// DepList returns dependencies for a bead. "up" returns what depends on the bead;
// "down" (default) returns what the bead depends on. It is a single-statement
// read, so the read handle's implicit per-statement snapshot already suffices.
func (s *JournalStore) DepList(id, direction string) ([]Dep, error) {
	return journalLoadDeps(context.Background(), s.rdb, id, direction)
}

// --- Writes ----------------------------------------------------------------

// Create persists a new bead, minting a gcg- ID and defaulting Status/Type/times.
func (s *JournalStore) Create(b Bead) (Bead, error) {
	return s.CreateWithStorage(b, StorageDefault)
}

// CreateWithStorage persists a new bead in the storage tier selected by policy.
func (s *JournalStore) CreateWithStorage(b Bead, storage StorageClass) (Bead, error) {
	resolved, err := resolveJournalStorage(b, storage)
	if err != nil {
		return Bead{}, err
	}
	var out Bead
	err = s.withTx(context.Background(), func(tx *sql.Tx) error {
		created, err := s.applyCreateInTx(context.Background(), tx, resolved)
		if err != nil {
			return err
		}
		out = created
		return nil
	})
	return out, err
}

func (s *JournalStore) applyCreateInTx(ctx context.Context, tx *sql.Tx, b Bead) (Bead, error) {
	if err := journalRejectCallerID(b.ID); err != nil {
		return Bead{}, err
	}
	id, err := s.mintID(ctx, tx)
	if err != nil {
		return Bead{}, err
	}
	status := b.Status
	if status == "" {
		status = "open"
	}
	beadType := b.Type
	if beadType == "" {
		beadType = "task"
	}
	now := journalNow()
	// Preserve a caller-supplied CreatedAt — P1.5 rehoming/backfill replays the
	// original timestamp, and Ready ordering plus CreatedBefore purges depend on
	// it — stamping now only when it is absent (L1).
	createdAt := b.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	// Project a parent-child relationship onto the parent_id column so Children()
	// and Get().ParentID agree with native's projection. The explicit ParentID
	// field wins; otherwise the first parent-child dependency/need supplies it. The
	// relationship is also written as an edge below, mirroring native (M4).
	parentID := b.ParentID
	if parentID == "" {
		parentID = journalParentFromDeps(b)
	}
	tier := journalTierFromBead(b)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes
		  (id, title, status, bead_type, priority, description, assignee, from_actor,
		   parent_id, ref, created_at, updated_at, defer_until, storage_tier, fold_owned, stream_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, '')`,
		id, b.Title, status, beadType, journalNullableInt(b.Priority), b.Description, b.Assignee, b.From,
		parentID, b.Ref, journalFormatTime(createdAt), journalFormatTime(now), journalDeferArg(b.DeferUntil), tier,
	); err != nil {
		return Bead{}, fmt.Errorf("journal store: inserting bead %q: %w", id, err)
	}
	if err := journalReplaceLabels(ctx, tx, id, b.Labels); err != nil {
		return Bead{}, err
	}
	if err := journalMergeMetadata(ctx, tx, id, b.Metadata); err != nil {
		return Bead{}, err
	}
	for _, dep := range b.Dependencies {
		if err := journalInsertEdge(ctx, tx, id, dep.DependsOnID, dep.Type, ""); err != nil {
			return Bead{}, err
		}
	}
	for _, need := range b.Needs {
		depType, dependsOnID := parseJournalNeed(need)
		if err := journalInsertEdge(ctx, tx, id, dependsOnID, depType, ""); err != nil {
			return Bead{}, err
		}
	}
	// Read the row back through the same write tx so the caller sees the fully
	// hydrated bead (labels/metadata/deps/live is_blocked) it just wrote.
	hydrated, err := s.hydrateWhere(ctx, tx, "n.id = ?", []any{id})
	if err != nil {
		return Bead{}, err
	}
	if len(hydrated) == 0 {
		// P1.4: this guard is effectively dead — the row was just inserted in this
		// same transaction, so the readback cannot miss it. Keep it as a cheap
		// invariant until the conformance suite pins the create/readback contract.
		return Bead{}, fmt.Errorf("journal store: created bead %q not readable", id)
	}
	return hydrated[0], nil
}

func parseJournalNeed(need string) (depType, dependsOnID string) {
	depType, dependsOnID = "blocks", need
	if before, after, ok := strings.Cut(need, ":"); ok && before != "" && after != "" {
		depType, dependsOnID = before, after
	}
	return depType, dependsOnID
}

// journalRejectCallerID rejects any caller-supplied bead id. The journal store
// mints its own gcg-j<seq> ids from a monotonic counter; honoring a caller id
// would collide with a future minted id and wedge the sequence forever (a
// gcg-j<seq> id most directly), and a foreign-shaped id would violate the
// gcg-j<seq> residence marker P1.5 routing relies on. A blank id mints normally.
func journalRejectCallerID(id string) error {
	if id == "" {
		return nil
	}
	return fmt.Errorf("journal store: caller-supplied bead id %q rejected: the store mints its own %s-%s<seq> ids", id, journalIDPrefix, journalIDMarker)
}

// journalParentFromDeps returns the target of the first parent-child dependency
// (or need) on b, or "" when none is present. It lets applyCreateInTx project a
// parent-child relationship expressed as a dependency onto the parent_id column
// (M4), mirroring how native derives ParentID from a parent-child dependency.
func journalParentFromDeps(b Bead) string {
	for _, dep := range b.Dependencies {
		if dep.Type == string(beadslib.DepParentChild) {
			return dep.DependsOnID
		}
	}
	for _, need := range b.Needs {
		if depType, target := parseJournalNeed(need); depType == string(beadslib.DepParentChild) {
			return target
		}
	}
	return ""
}

func journalInsertEdge(ctx context.Context, tx *sql.Tx, fromID, toID, depType, metadata string) error {
	if strings.TrimSpace(toID) == "" {
		return fmt.Errorf("journal store: edge from %q has empty target", fromID)
	}
	if depType == "" {
		depType = "blocks"
	}
	// Only overwrite existing edge metadata when the incoming metadata is
	// non-empty: DepAdd re-adds edges with metadata="" and must not wipe metadata
	// a plan (ApplyGraphPlan) wrote earlier (M3).
	// P1.4: edge (from,to) identity is keyed by dep_type, so re-adding the same
	// pair with a different dep_type inserts a second edge rather than downgrading
	// the existing one (L2), and a plan that lists a duplicate edge upserts
	// silently rather than surfacing the collision (L5). The conformance suite
	// pins both.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO edges (from_id, to_id, dep_type, metadata) VALUES (?, ?, ?, ?)
		ON CONFLICT(from_id, to_id, dep_type) DO UPDATE SET metadata = excluded.metadata
		  WHERE excluded.metadata <> ''`,
		fromID, toID, depType, metadata,
	); err != nil {
		return fmt.Errorf("journal store: inserting edge %s->%s: %w", fromID, toID, err)
	}
	return nil
}

// journalReplaceLabels adds each label to id, skipping duplicates. Despite the
// "Replace" in its name it is additive: it never deletes labels absent from the
// slice — Update removes labels explicitly through UpdateOpts.RemoveLabels. The
// name is kept for call-site stability across Create/Update/graph-apply.
func journalReplaceLabels(ctx context.Context, tx *sql.Tx, id string, labels []string) error {
	for _, label := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_labels (node_id, label) VALUES (?, ?) ON CONFLICT(node_id, label) DO NOTHING`,
			id, label,
		); err != nil {
			return fmt.Errorf("journal store: inserting label %q on %q: %w", label, id, err)
		}
	}
	return nil
}

func journalMergeMetadata(ctx context.Context, tx *sql.Tx, id string, metadata map[string]string) error {
	for key, value := range metadata {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO node_metadata (node_id, key, value) VALUES (?, ?, ?)
			 ON CONFLICT(node_id, key) DO UPDATE SET value = excluded.value`,
			id, key, value,
		); err != nil {
			return fmt.Errorf("journal store: setting metadata %q on %q: %w", key, id, err)
		}
	}
	return nil
}

// Update modifies non-nil fields of a bead. It never touches defer_until.
func (s *JournalStore) Update(id string, opts UpdateOpts) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		return s.applyUpdateInTx(context.Background(), tx, id, opts)
	})
}

func (s *JournalStore) applyUpdateInTx(ctx context.Context, tx *sql.Tx, id string, opts UpdateOpts) error {
	if err := journalGuardMutable(ctx, tx, id); err != nil {
		return err
	}
	// P1.4: Update takes each non-nil field verbatim without validating it
	// (e.g. an unknown status or bead_type is written as-is). The conformance
	// suite pins the field-validation contract.
	sets := []string{"updated_at = ?"}
	args := []any{journalFormatTime(journalNow())}
	if opts.Title != nil {
		sets = append(sets, "title = ?")
		args = append(args, *opts.Title)
	}
	if opts.Status != nil {
		sets = append(sets, "status = ?")
		args = append(args, *opts.Status)
	}
	if opts.Type != nil {
		sets = append(sets, "bead_type = ?")
		args = append(args, *opts.Type)
	}
	if opts.Priority != nil {
		sets = append(sets, "priority = ?")
		args = append(args, *opts.Priority)
	}
	if opts.Description != nil {
		sets = append(sets, "description = ?")
		args = append(args, *opts.Description)
	}
	if opts.Assignee != nil {
		sets = append(sets, "assignee = ?")
		args = append(args, *opts.Assignee)
	}
	if opts.ParentID != nil {
		sets = append(sets, "parent_id = ?")
		args = append(args, *opts.ParentID)
	}
	args = append(args, id)
	if _, err := tx.ExecContext(ctx,
		"UPDATE nodes SET "+strings.Join(sets, ", ")+" WHERE id = ? AND fold_owned = 0", args...,
	); err != nil {
		return fmt.Errorf("journal store: updating bead %q: %w", id, err)
	}
	if err := journalReplaceLabels(ctx, tx, id, opts.Labels); err != nil {
		return err
	}
	for _, label := range opts.RemoveLabels {
		if _, err := tx.ExecContext(ctx, `DELETE FROM node_labels WHERE node_id = ? AND label = ?`, id, label); err != nil {
			return fmt.Errorf("journal store: removing label %q from %q: %w", label, id, err)
		}
	}
	return journalMergeMetadata(ctx, tx, id, opts.Metadata)
}

// SetMetadata sets a single metadata key on a bead.
func (s *JournalStore) SetMetadata(id, key, value string) error {
	return s.SetMetadataBatch(id, map[string]string{key: value})
}

// SetMetadataBatch atomically sets multiple metadata keys on a bead.
func (s *JournalStore) SetMetadataBatch(id string, kvs map[string]string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		return s.applySetMetadataBatchInTx(context.Background(), tx, id, kvs)
	})
}

func (s *JournalStore) applySetMetadataBatchInTx(ctx context.Context, tx *sql.Tx, id string, kvs map[string]string) error {
	if err := journalGuardMutable(ctx, tx, id); err != nil {
		return err
	}
	// P1.4: an empty batch is a guarded no-op (it still verifies the bead exists
	// and is mutable, but does not touch updated_at). The conformance suite pins
	// whether an empty SetMetadataBatch should be a pure no-op.
	if len(kvs) == 0 {
		return nil
	}
	if err := journalMergeMetadata(ctx, tx, id, kvs); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET updated_at = ? WHERE id = ? AND fold_owned = 0`,
		journalFormatTime(journalNow()), id); err != nil {
		return fmt.Errorf("journal store: touching bead %q: %w", id, err)
	}
	return nil
}

// Close sets a bead's status to closed. Closing an already-closed bead is a no-op.
func (s *JournalStore) Close(id string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		return s.applyCloseInTx(context.Background(), tx, id)
	})
}

func (s *JournalStore) applyCloseInTx(ctx context.Context, tx *sql.Tx, id string) error {
	if err := journalGuardMutable(ctx, tx, id); err != nil {
		return err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM nodes WHERE id = ?`, id).Scan(&status); err != nil {
		return fmt.Errorf("journal store: reading status of %q: %w", id, err)
	}
	if status == "closed" {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE nodes SET status = 'closed', updated_at = ? WHERE id = ? AND fold_owned = 0`,
		journalFormatTime(journalNow()), id); err != nil {
		return fmt.Errorf("journal store: closing bead %q: %w", id, err)
	}
	return nil
}

// Reopen sets a closed bead's status back to open.
func (s *JournalStore) Reopen(id string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if err := journalGuardMutable(context.Background(), tx, id); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRowContext(context.Background(), `SELECT status FROM nodes WHERE id = ?`, id).Scan(&status); err != nil {
			return fmt.Errorf("journal store: reading status of %q: %w", id, err)
		}
		if status == "open" {
			return nil
		}
		if _, err := tx.ExecContext(context.Background(), `UPDATE nodes SET status = 'open', updated_at = ? WHERE id = ? AND fold_owned = 0`,
			journalFormatTime(journalNow()), id); err != nil {
			return fmt.Errorf("journal store: reopening bead %q: %w", id, err)
		}
		return nil
	})
}

// CloseAll closes multiple beads and stamps metadata on each newly closed bead.
func (s *JournalStore) CloseAll(ids []string, metadata map[string]string) (int, error) {
	closed := 0
	for _, id := range ids {
		current, err := s.Get(id)
		if err != nil {
			return closed, err
		}
		if current.Status == "closed" {
			continue
		}
		if len(metadata) > 0 {
			if err := s.SetMetadataBatch(id, metadata); err != nil {
				return closed, err
			}
		}
		if err := s.Close(id); err != nil {
			return closed, err
		}
		closed++
	}
	return closed, nil
}

// Delete permanently removes a bead. node_labels, node_metadata, and outbound
// edges cascade; inbound edges are left dangling (and correctly block their
// dependents, D-4).
func (s *JournalStore) Delete(id string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if err := journalGuardMutable(context.Background(), tx, id); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(), `DELETE FROM nodes WHERE id = ? AND fold_owned = 0`, id); err != nil {
			return fmt.Errorf("journal store: deleting bead %q: %w", id, err)
		}
		return nil
	})
}

// DepAdd records that issueID depends on dependsOnID. A parent-child dependency
// additionally projects onto issueID's parent_id column so Children() and
// Get().ParentID agree with native's projection (M4).
func (s *JournalStore) DepAdd(issueID, dependsOnID, depType string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		ctx := context.Background()
		if err := journalGuardMutable(ctx, tx, issueID); err != nil {
			return err
		}
		if err := journalInsertEdge(ctx, tx, issueID, dependsOnID, depType, ""); err != nil {
			return err
		}
		if depType == string(beadslib.DepParentChild) {
			if _, err := tx.ExecContext(ctx,
				`UPDATE nodes SET parent_id = ?, updated_at = ? WHERE id = ? AND fold_owned = 0`,
				dependsOnID, journalFormatTime(journalNow()), issueID,
			); err != nil {
				return fmt.Errorf("journal store: projecting parent-child of %q: %w", issueID, err)
			}
		}
		return nil
	})
}

// DepRemove removes a dependency between two beads.
func (s *JournalStore) DepRemove(issueID, dependsOnID string) error {
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		if err := journalGuardMutable(context.Background(), tx, issueID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(context.Background(), `DELETE FROM edges WHERE from_id = ? AND to_id = ?`, issueID, dependsOnID); err != nil {
			return fmt.Errorf("journal store: removing dep %s->%s: %w", issueID, dependsOnID, err)
		}
		return nil
	})
}

// ReleaseIfCurrent clears an in-progress assignment only when the bead still has
// the expected assignee, in one conditional UPDATE.
func (s *JournalStore) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	released := false
	err := s.withTx(context.Background(), func(tx *sql.Tx) error {
		res, err := tx.ExecContext(context.Background(), `
			UPDATE nodes SET status = 'open', assignee = '', updated_at = ?
			WHERE id = ? AND fold_owned = 0 AND status = 'in_progress' AND assignee = ?`,
			journalFormatTime(journalNow()), id, expectedAssignee)
		if err != nil {
			return fmt.Errorf("journal store: releasing bead %q: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("journal store: release rows affected for %q: %w", id, err)
		}
		released = n > 0
		return nil
	})
	return released, err
}

// Tx runs fn inside one SQLite transaction; every write coalesces into a single
// commit and rolls back atomically on error.
func (s *JournalStore) Tx(_ string, fn func(tx Tx) error) error {
	if fn == nil {
		return errors.New("beads tx: nil callback")
	}
	return s.withTx(context.Background(), func(tx *sql.Tx) error {
		return fn(&journalTx{store: s, ctx: context.Background(), tx: tx})
	})
}

// journalTx adapts the Store.Tx write surface onto an open SQLite transaction,
// routing every method through the store's applyXInTx helpers so transactional
// and standalone writes share one implementation.
type journalTx struct {
	store *JournalStore
	ctx   context.Context
	tx    *sql.Tx
}

func (t *journalTx) Create(b Bead) (Bead, error) {
	return t.store.applyCreateInTx(t.ctx, t.tx, b)
}

func (t *journalTx) Update(id string, opts UpdateOpts) error {
	return t.store.applyUpdateInTx(t.ctx, t.tx, id, opts)
}

func (t *journalTx) SetMetadataBatch(id string, kvs map[string]string) error {
	return t.store.applySetMetadataBatchInTx(t.ctx, t.tx, id, kvs)
}

func (t *journalTx) Close(id string) error {
	return t.store.applyCloseInTx(t.ctx, t.tx, id)
}

// --- Graph apply -----------------------------------------------------------

// ApplyGraphPlan atomically materializes a symbolic bead graph.
func (s *JournalStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage materializes a graph in the selected storage tier.
// Nodes, labels, metadata, edges, and post-create assignments all land in one
// SQLite transaction, mirroring NativeDoltStore's atomic semantics.
func (s *JournalStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("graph apply plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("journal graph apply: %w", err)
	}
	tier := journalTierFromBead(Bead{Ephemeral: ephemeral, NoHistory: noHistory})
	if err := validateNativeGraphApplyPlan(plan); err != nil {
		return nil, fmt.Errorf("journal graph apply: %w", err)
	}
	keyToID := make(map[string]string, len(plan.Nodes))
	err = s.withTx(ctx, func(tx *sql.Tx) error {
		for _, node := range plan.Nodes {
			id, err := s.mintID(ctx, tx)
			if err != nil {
				return err
			}
			keyToID[node.Key] = id
		}
		now := journalFormatTime(journalNow())
		for _, node := range plan.Nodes {
			if err := s.applyGraphNode(ctx, tx, node, keyToID, tier, now); err != nil {
				return err
			}
		}
		parentDepPairs := nativeGraphApplyParentDepPairs(plan.Nodes, keyToID)
		for i, edge := range plan.Edges {
			if err := journalApplyGraphEdge(ctx, tx, i, edge, keyToID, parentDepPairs); err != nil {
				return err
			}
		}
		for _, node := range plan.Nodes {
			if node.Assignee != "" && node.AssignAfterCreate {
				if _, err := tx.ExecContext(ctx, `UPDATE nodes SET assignee = ? WHERE id = ?`, node.Assignee, keyToID[node.Key]); err != nil {
					return fmt.Errorf("journal graph apply: assigning node %q: %w", node.Key, err)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	result := &GraphApplyResult{IDs: keyToID}
	if err := ValidateGraphApplyResult(plan, result); err != nil {
		return nil, fmt.Errorf("journal graph apply: %w", err)
	}
	return result, nil
}

func (s *JournalStore) applyGraphNode(ctx context.Context, tx *sql.Tx, node GraphApplyNode, keyToID map[string]string, tier, now string) error {
	id := keyToID[node.Key]
	beadType := node.Type
	if beadType == "" {
		beadType = "task"
	}
	parentID := node.ParentID
	if node.ParentKey != "" {
		parentID = keyToID[node.ParentKey]
	}
	assignee := ""
	if node.Assignee != "" && !node.AssignAfterCreate {
		assignee = node.Assignee
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO nodes
		  (id, title, status, bead_type, priority, description, assignee, from_actor,
		   parent_id, ref, created_at, updated_at, defer_until, storage_tier, fold_owned, stream_id)
		VALUES (?, ?, 'open', ?, ?, ?, ?, ?, ?, '', ?, ?, NULL, ?, 0, '')`,
		id, node.Title, beadType, journalNullableInt(node.Priority), node.Description, assignee, node.From,
		parentID, now, now, tier,
	); err != nil {
		return fmt.Errorf("journal graph apply: inserting node %q: %w", node.Key, err)
	}
	if err := journalReplaceLabels(ctx, tx, id, node.Labels); err != nil {
		return err
	}
	metadata := make(map[string]string, len(node.Metadata)+len(node.MetadataRefs))
	for k, v := range node.Metadata {
		metadata[k] = v
	}
	for metaKey, refKey := range node.MetadataRefs {
		metadata[metaKey] = keyToID[refKey]
	}
	return journalMergeMetadata(ctx, tx, id, metadata)
}

func journalApplyGraphEdge(ctx context.Context, tx *sql.Tx, i int, edge GraphApplyEdge, keyToID map[string]string, parentDepPairs map[string]bool) error {
	fromID := nativeGraphApplyResolveRef(edge.FromKey, edge.FromID, keyToID)
	toID := nativeGraphApplyResolveRef(edge.ToKey, edge.ToID, keyToID)
	depType := nativeGraphApplyDependencyType(edge.Type)
	if parentDepPairs[nativeGraphApplyDepPairKey(fromID, toID)] {
		if depType == beadslib.DepParentChild {
			// The parent relationship is recorded on the node's parent_id column;
			// a duplicate parent-child edge is silently skipped (NativeDolt parity).
			return nil
		}
		return fmt.Errorf("journal graph apply: edge %d %s->%s duplicates a parent-child relationship with dependency type %q", i, fromID, toID, depType)
	}
	if parentDepPairs[nativeGraphApplyDepPairKey(toID, fromID)] && nativeGraphApplyCycleRelevantDependencyType(depType) {
		return fmt.Errorf("journal graph apply: edge %d %s->%s creates a blocking reverse of a parent-child relationship", i, fromID, toID)
	}
	return journalInsertEdge(ctx, tx, fromID, toID, string(depType), edge.Metadata)
}
