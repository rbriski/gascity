package beads

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// The Tier-B claim-surface read capability exposes the fold-owned (fold_owned=1)
// pool-work projection to a claiming worker's composition root (cmd/gc). Every
// façade read filters fold_owned=0 (hydrateWhere), so these reads are the only
// way a claim/close/show leg sees a Lumen pool bead. Like the other journal
// capabilities it is optional and probe-gated (TierBClaimSurfaceStoreFor), never
// part of the base Store interface — it stays off every generic CLI/API
// projection.
//
// This substrate layer stays free of Lumen vocabulary: the marker key/value that
// selects pool rows (engine.DispatchModeMetaKey / DispatchModePool) is passed by
// the CALLER, exactly as ControlFrontierParams passes route/metadata keys.
//
// Read-leg caveat (fold edge direction): the fold writes dependency edges as
// FromID=predecessor → ToID=dependent, while journalLoadDeps renders `from` as
// the depender — so a hydrated fold row's Dependencies list names its DEPENDENTS,
// not its dependencies. It is harmless here (blocking-dep types are
// blocks/waits-for/conditional-blocks only; fold after/member edges never trip
// journalIsBlocked), but a consumer that interprets Dependencies semantically
// would be misled. Do not read the hydrated deps of a fold row as its upstream.

// TierBAssignedQuery selects claimed fold-owned pool rows. The marker key/value
// keep this layer vocabulary-agnostic (the caller passes
// engine.DispatchModeMetaKey / DispatchModePool); an empty MarkerKey disables the
// marker predicate. Assignees filters by claimant; nil/empty means all assignees.
type TierBAssignedQuery struct {
	Assignees   []string
	MarkerKey   string
	MarkerValue string
}

// TierBClaimSurfaceStore is the optional beads.Store capability exposing the
// fold-owned pool-work claim surface: the routed frontier a worker claims from,
// the assigned rows crash-recovery and session-preserve read, and an exact-id
// fold-owned read for the show leg.
type TierBClaimSurfaceStore interface {
	// TierBRoutedFrontier returns the ready, open, unassigned pool rows for the
	// given routes in frontier_route_order, reusing the frontier projection walk
	// (Arm B). An empty route is never queried (run roots / engine nodes carry
	// route ""); limitPerRoute <= 0 disables the per-route cap.
	TierBRoutedFrontier(ctx context.Context, routes []string, limitPerRoute int) ([]Bead, error)
	// TierBAssigned returns claimed fold-owned rows (status open or in_progress)
	// matching the query's assignee and marker filters — the crash-recovery and
	// session-preserve source.
	TierBAssigned(ctx context.Context, q TierBAssignedQuery) ([]Bead, error)
	// FoldOwnedGet reads one fold-owned row by exact id. The bool is false for an
	// absent id or a fold_owned=0 façade row — the "not a fold row" signal.
	FoldOwnedGet(ctx context.Context, id string) (Bead, bool, error)
}

// TierBClaimSurfaceHandleProvider lets a wrapper store expose a delegated Tier-B
// claim-surface handle without claiming the interface globally, mirroring
// ControlFrontierHandleProvider.
type TierBClaimSurfaceHandleProvider interface {
	TierBClaimSurfaceHandle() (TierBClaimSurfaceStore, bool)
}

// TierBClaimSurfaceStoreFor returns the Tier-B claim-surface capability for store
// when available, following the ControlFrontierStoreFor probe idiom: a direct
// implementation wins, then a wrapper's delegated handle, else (nil, false).
func TierBClaimSurfaceStoreFor(store Store) (TierBClaimSurfaceStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(TierBClaimSurfaceStore); ok {
		return s, true
	}
	if p, ok := store.(TierBClaimSurfaceHandleProvider); ok {
		return p.TierBClaimSurfaceHandle()
	}
	return nil, false
}

// Compile-time assertion that JournalStore surfaces the Tier-B claim surface.
var _ TierBClaimSurfaceStore = (*JournalStore)(nil)

// TierBRoutedFrontier walks the frontier projection for each route inside one
// read-only WAL snapshot (the M2 torn-read discipline), reusing frontierProjectionTier
// + hydrateIDsOrdered so candidates carry labels/metadata/deps/is_blocked
// identically to Arm B.
func (s *JournalStore) TierBRoutedFrontier(ctx context.Context, routes []string, limitPerRoute int) ([]Bead, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	now := journalNow()
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("journal store: begin tier-b frontier snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot
	var merged []Bead
	for _, route := range routes {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}
		rows, err := s.frontierProjectionTier(ctx, tx, route, now, limitPerRoute)
		if err != nil {
			return nil, err
		}
		merged = append(merged, rows...)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("journal store: commit tier-b frontier snapshot: %w", err)
	}
	return merged, nil
}

// TierBAssigned reads claimed fold-owned pool rows (status open or in_progress)
// inside one read-only snapshot. The marker and assignee predicates are bound as
// `?` args; no caller string is interpolated into SQL.
func (s *JournalStore) TierBAssigned(ctx context.Context, q TierBAssignedQuery) ([]Bead, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	where := "n.status IN ('open','in_progress')"
	var args []any
	if q.MarkerKey != "" {
		where += " AND EXISTS (SELECT 1 FROM node_metadata m WHERE m.node_id = n.id AND m.key = ? AND m.value = ?)"
		args = append(args, q.MarkerKey, q.MarkerValue)
	}
	if len(q.Assignees) > 0 {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(q.Assignees)), ",")
		where += " AND n.assignee IN (" + placeholders + ")"
		for _, a := range q.Assignees {
			args = append(args, a)
		}
	}
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("journal store: begin tier-b assigned snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot
	rows, err := s.hydrateFoldOwnedWhere(ctx, tx, where, args)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("journal store: commit tier-b assigned snapshot: %w", err)
	}
	return rows, nil
}

// FoldOwnedGet reads one fold-owned row by exact id.
func (s *JournalStore) FoldOwnedGet(ctx context.Context, id string) (Bead, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Bead{}, false, fmt.Errorf("journal store: begin fold-owned get snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot
	rows, err := s.hydrateFoldOwnedWhere(ctx, tx, "n.id = ?", []any{id})
	if err != nil {
		return Bead{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Bead{}, false, fmt.Errorf("journal store: commit fold-owned get snapshot: %w", err)
	}
	if len(rows) == 0 {
		return Bead{}, false, nil
	}
	return rows[0], true, nil
}

// hydrateFoldOwnedWhere is the fold-owned peer of hydrateWhere: it selects
// fold_owned=1 nodes matching extraWhere and hydrates each into a full Bead
// (labels, metadata, dependencies, live is_blocked), so a claim-surface row
// carries the same fields Arm B produces. It applies no residence-visibility gate
// (fold-owned rows are journal-resident by construction — the frontierProjectionTier
// / hydrateIDsOrdered path is likewise ungated).
func (s *JournalStore) hydrateFoldOwnedWhere(ctx context.Context, q journalQueryer, extraWhere string, args []any) ([]Bead, error) {
	sqlText := "SELECT " + journalNodeColumns + " FROM nodes n WHERE n.fold_owned = 1"
	if extraWhere != "" {
		sqlText += " AND " + extraWhere
	}
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("journal store: querying fold-owned nodes: %w", err)
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
