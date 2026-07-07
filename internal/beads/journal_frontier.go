package beads

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"
)

// ControlFrontier is the P2.1 indexed-SELECT replacement for the control
// dispatcher's per-tick `bd | jq` frontier pipeline
// (workflowServeControlReadyQueryForBeads in cmd/gc/dispatch_runtime.go). It
// reproduces that pipeline's exact
// tier/readiness/filter/sort/dedupe semantics as one parameterized read over
// the journal projection tables, with every runtime value bound as a `?`
// argument — no shell, no jq, no string-assembled SQL. Only P2.1 defines and
// implements the capability; wiring it into the serve tick is P2.3.
//
// The safety model is shadow-compare: for the same store state, ControlFrontier
// must return exactly what `bd ready | jq` returns. bd-ready is NOT
// JournalStore.Ready — the two diverge on the mechanisms the control dispatcher
// depends on (waits-for gates, parent-child cascade, dangling deps, the exclude
// set, the assignee tie-break). So ControlFrontier deliberately does NOT reuse
// journalBlockedPredicate / IsReadyCandidateForTier / sortBeadsReadyOrder: those
// implement JournalStore.Ready's own conformance-pinned contract (including the
// D-4 "dangling dep blocks" rule). ControlFrontier is a DISTINCT predicate that
// mirrors bd's is_blocked + `bd ready` filters as ported below.
//
// SEC-1/SEC-2: like the other journal capabilities, ControlFrontierStore is an
// optional, probe-gated surface (ControlFrontierStoreFor), never part of the
// base Store interface, so it stays controller-only and off every generic
// CLI/API projection.
//
// Out-of-contract assumptions (verified, not modeled):
//   - Directory-label auto-scoping (cmd/bd/ready.go:120-124 via
//     config.GetDirectoryLabels) applies a `--label-any` scope when the caller's
//     cwd matches a configured `[directory].labels` pattern. gc cities never set
//     `directory.labels`, so the dispatcher's `bd ready` invocation carries no
//     directory label. ControlFrontier therefore applies no label scope.
//   - Pinned. bd's ready excludes pinned rows (`pinned = 0 OR pinned IS NULL`,
//     sqlbuild/ready.go:95) and never marks a pinned row is_blocked
//     (blocked_state.go:146). The journal `nodes` table has no pinned column or
//     `pinned` status — the concept does not exist here — so there is nothing to
//     exclude. The block computation still treats a `pinned` target/child status
//     as bd does (defensive), but no journal write path can produce one.

// ControlFrontierParams carries the typed inputs the control-dispatcher serve
// tick needs, one field per lever the `bd | jq` frontier expressed. It is a
// closed, typed struct: callers never hand this layer raw SQL, a
// map[string]any, or a jq program — the vocabulary keys (route/instantiating
// metadata keys) are passed explicitly so this substrate layer stays free of
// dispatcher role/metadata constants.
type ControlFrontierParams struct {
	// AssigneeCandidates are the assignee-tier identities in tier order (the
	// assignee `for id in ...` loop in workflowServeControlReadyQueryForBeads:
	// control session name, alias, target, session id, plus any derived legacy
	// aliases). Each distinct, non-blank candidate yields one assignee tier; blanks
	// and repeats are skipped, exactly as the shell's `$seen` dedupe file does.
	// Deriving this ordered list (including the `*control-dispatcher` ->
	// `*workflow-control` aliasing, and the id/legacy interleave the shell walks) is
	// the caller's job (the P2.2 controlFrontierInputs helper), not this layer's.
	AssigneeCandidates []string

	// Routes are the routed-tier routes in tier order (the routed_ready calls in
	// workflowServeControlReadyQueryForBeads: GC_CONTROL_TARGET, the legacy
	// workflow-control alias, the bare route). Each route yields one tier per
	// RouteMetadataKey, in that key order.
	Routes []string

	// RouteMetadataKeys are the metadata keys a routed tier matches a route
	// against, in tier-precedence order. The serve tick passes
	// {gc.run_target, gc.routed_to} (the per-route emit order in
	// workflowServeControlReadyQueryForBeads): the run_target tier is emitted before
	// the routed_to tier so first-wins dedupe prefers a run_target match, mirroring
	// the shell's call order.
	RouteMetadataKeys []string

	// InstantiatingMetadataKey names the metadata key whose non-empty value marks
	// a half-materialized molecule bead to drop from the frontier
	// (beadmeta.InstantiatingMetadataKey; the jq instantiating-filter, the reduce
	// filter in workflowServeControlReadyQueryForBeads). Empty disables the drop.
	InstantiatingMetadataKey string

	// IncludeEphemeral widens every tier to ephemeral/no-history rows, mirroring
	// `--include-ephemeral` under UsesBD105ReadySemantics (the includeEphemeral
	// toggle in workflowServeControlReadyQueryForBeads). It maps to TierBoth; the
	// zero value maps to TierIssues (ephemeral hidden).
	IncludeEphemeral bool

	// LimitPerTier caps each individual tier SELECT, mirroring bd's
	// `--limit=workflowServeScanLimit` (the workflowServeScanLimit constant). This
	// is a PER-TIER cap, not a global one — the shell limits each `bd ready` call
	// independently, then jq merges. A value <= 0 disables the per-tier cap.
	LimitPerTier int
}

func (p ControlFrontierParams) tierMode() TierMode {
	if p.IncludeEphemeral {
		return TierBoth
	}
	return TierIssues
}

// ControlFrontierStore is the optional beads.Store capability exposing the
// indexed control-dispatcher frontier read. Probe it with
// ControlFrontierStoreFor; never assume the base Store implements it.
type ControlFrontierStore interface {
	// ControlFrontier returns the ready control beads for the dispatcher, in the
	// exact tier order, readiness/filter semantics, sort, and first-wins /
	// instantiating-drop dedupe the `bd | jq` serve-tick pipeline produced for
	// the same store state.
	ControlFrontier(ctx context.Context, params ControlFrontierParams) ([]Bead, error)
}

// ControlFrontierHandleProvider lets a wrapper store (e.g. CachingStore) expose
// a delegated frontier handle without claiming the interface globally, mirroring
// AppendLogHandleProvider.
type ControlFrontierHandleProvider interface {
	ControlFrontierHandle() (ControlFrontierStore, bool)
}

// ControlFrontierStoreFor returns the frontier capability for store when
// available, following the same probe idiom as AppendLogStoreFor: a direct
// implementation wins, then a wrapper's delegated handle, else (nil, false) —
// the honest "absent" signal, never a degraded stub.
func ControlFrontierStoreFor(store Store) (ControlFrontierStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(ControlFrontierStore); ok {
		return s, true
	}
	if p, ok := store.(ControlFrontierHandleProvider); ok {
		return p.ControlFrontierHandle()
	}
	return nil, false
}

// Compile-time assertion that JournalStore surfaces the frontier capability.
var _ ControlFrontierStore = (*JournalStore)(nil)

// frontierExcludedTypes is the exact set of bead types bd drops from the
// dispatcher's ready invocation. It is bd's built-in ready exclude set
// (sqlbuild/ready.go:16-38 ReadyWorkExcludeTypes: merge-request, gate, molecule,
// rig, plus domain.DefaultInfraTypes() = agent, role, message) PLUS the shell's
// explicit `--exclude-type=epic` (the `--exclude-type=epic` flags in
// workflowServeControlReadyQueryForBeads).
//
// It deliberately does NOT include step/convoy/session or any label exclusion:
// those live in IsReadyCandidateForTier for JournalStore.Ready, but bd's actual
// `ready` query excludes none of them, so mirroring bd here means NOT applying
// them (finding 5).
var frontierExcludedTypes = map[string]bool{
	"merge-request": true, // ReadyWorkExcludeTypes
	"gate":          true, // ReadyWorkExcludeTypes (types.TypeGate)
	"molecule":      true, // ReadyWorkExcludeTypes (types.TypeMolecule)
	"rig":           true, // ReadyWorkExcludeTypes
	"agent":         true, // domain.DefaultInfraTypes
	"role":          true, // domain.DefaultInfraTypes
	"message":       true, // domain.DefaultInfraTypes
	"epic":          true, // shell --exclude-type=epic
}

// ControlFrontier implements the indexed frontier read. It runs every tier
// inside one read-only WAL snapshot (the M2 torn-read discipline that
// hydrateSnapshot uses), computes bd's is_blocked / deferred-child sets once
// over that snapshot, assembles the tiers in the shell's concatenation order
// (assignee tiers first, then per-route run_target/routed_to/fold-projection
// tiers), and applies the jq instantiating-drop + first-wins dedupe over the
// merged slice.
//
// Injection surface: every runtime value (assignee, route, metadata key,
// per-tier limit, defer cutoff) is bound as a `?` argument below; the SQL text
// itself is composed only from compile-time constants (journalNodeColumns,
// journalTierClause, the fixed edge/nodes projection queries). No caller string
// is interpolated into SQL — this is the injection-surface win over the escaped
// jq/shell program the frontier used to be (the jq filter in
// workflowServeControlReadyQueryForBeads).
func (s *JournalStore) ControlFrontier(ctx context.Context, params ControlFrontierParams) ([]Bead, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	tier := params.tierMode()
	now := journalNow()

	tx, err := s.rdb.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("journal store: begin frontier snapshot: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck // read-only; rollback just releases the snapshot

	// Compute bd's is_blocked and deferred-parent-children sets once over the
	// snapshot (M2), so every tier filters against identical readiness state.
	bg, err := s.computeFrontierBlockGraph(ctx, tx, now)
	if err != nil {
		return nil, err
	}

	var merged []Bead

	// Assignee tiers (the assignee loop in workflowServeControlReadyQueryForBeads).
	// Candidate order preserved; blanks and repeats skipped like the shell's
	// `$seen` file.
	seenCand := make(map[string]bool)
	for _, cand := range params.AssigneeCandidates {
		cand = strings.TrimSpace(cand)
		if cand == "" || seenCand[cand] {
			continue
		}
		seenCand[cand] = true
		rows, err := s.frontierAssigneeTier(ctx, tx, cand, tier, now, bg, params.LimitPerTier)
		if err != nil {
			return nil, err
		}
		merged = append(merged, rows...)
	}

	// Routed tiers (the routed_ready calls in workflowServeControlReadyQueryForBeads).
	// Per route, one tier per metadata
	// key in precedence order (run_target before routed_to), then the fold-owned
	// frontier projection (Arm B) for that route.
	for _, route := range params.Routes {
		route = strings.TrimSpace(route)
		if route == "" {
			continue
		}
		for _, key := range params.RouteMetadataKeys {
			key = strings.TrimSpace(key)
			if key == "" {
				continue
			}
			rows, err := s.frontierRoutedTier(ctx, tx, key, route, tier, now, bg, params.LimitPerTier)
			if err != nil {
				return nil, err
			}
			merged = append(merged, rows...)
		}
		rows, err := s.frontierProjectionTier(ctx, tx, route, now, params.LimitPerTier)
		if err != nil {
			return nil, err
		}
		merged = append(merged, rows...)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("journal store: commit frontier snapshot: %w", err)
	}

	return dedupeControlFrontier(merged, params.InstantiatingMetadataKey), nil
}

// frontierBlockGraph holds the per-snapshot readiness state ControlFrontier
// filters against: the ids bd would mark is_blocked = 1, and the children of a
// future-deferred parent bd's ready query excludes.
type frontierBlockGraph struct {
	blocked       map[string]bool
	deferredChild map[string]bool
}

// computeFrontierBlockGraph ports bd's is_blocked recompute (blocked_state.go)
// and its deferred-parent-children exclusion (ready_work.go:487-553 /
// sqlbuild/ready.go:127-135) over the journal `edges`/`nodes` projection.
//
// It reads the whole edge/node graph once inside the caller's read snapshot and
// derives, in Go, the exact set bd's mark/unmark fixpoint would compute. Doing
// the fixpoint (parent-child cascade) in Go rather than a WITH RECURSIVE SQL
// keeps the port readable and directly checkable against the bd source; the
// result is identical.
//
// bd rules ported (blocked_state.go markBlockedTemplateForIssues ~:146-182 and
// waitsForGateBlockedSQL ~:22-57):
//   - blocks / conditional-blocks: blocked if a dep of that type has a target
//     that EXISTS (INNER JOIN issues t ON t.id = depends_on) and is not
//     closed/pinned. A dangling target (no row) is NOT a block — this is where
//     ControlFrontier deliberately differs from JournalStore.Ready's D-4
//     (journal_store.go:515-531), which uses a LEFT JOIN so a dangling dep DOES
//     block. bd's ready path uses the INNER-JOIN is_blocked, so the frontier
//     matches bd, not D-4 (finding 2).
//   - waits-for: gate semantics — the dep blocks iff its TARGET (the spawner)
//     has any parent-child CHILD that is not closed/pinned; the target's own
//     status is irrelevant. Released early when the edge carries
//     metadata `{"gate":"any-children"}` and at least one child is closed
//     (finding 1).
//   - parent-child: a bead is blocked if it has a parent-child dep on a parent
//     that is itself blocked — transitive, via the recompute fixpoint (finding 3).
//   - subject guard: bd never marks a closed/pinned row is_blocked
//     (`i.status <> 'closed' AND i.status <> 'pinned'`), so cascade never
//     propagates through a closed/pinned parent.
func (s *JournalStore) computeFrontierBlockGraph(ctx context.Context, q journalQueryer, now time.Time) (*frontierBlockGraph, error) {
	status := make(map[string]string)
	futureDeferred := make(map[string]bool)

	nrows, err := q.QueryContext(ctx, `SELECT id, status, defer_until FROM nodes`)
	if err != nil {
		return nil, fmt.Errorf("journal store: frontier block graph nodes: %w", err)
	}
	if err := func() error {
		defer func() { _ = nrows.Close() }()
		for nrows.Next() {
			var id, st string
			var deferUntil sql.NullString
			if err := nrows.Scan(&id, &st, &deferUntil); err != nil {
				return fmt.Errorf("journal store: scanning block-graph node: %w", err)
			}
			status[id] = st
			if deferUntil.Valid {
				d, err := journalParseTime(deferUntil.String)
				if err != nil {
					return fmt.Errorf("bead %q: %w", id, err)
				}
				if !d.IsZero() && d.After(now) {
					futureDeferred[id] = true
				}
			}
		}
		return nrows.Err()
	}(); err != nil {
		return nil, err
	}

	type waitsForDep struct {
		spawner string
		gate    string
	}
	blockTargets := make(map[string][]string)  // n -> targets of blocks/conditional-blocks
	waitsFor := make(map[string][]waitsForDep) // n -> waits-for deps
	parentsOf := make(map[string][]string)     // n -> parent ids (parent-child)
	childrenOf := make(map[string][]string)    // parent id -> child ids (parent-child)

	erows, err := q.QueryContext(ctx, `SELECT from_id, to_id, dep_type, metadata FROM edges`)
	if err != nil {
		return nil, fmt.Errorf("journal store: frontier block graph edges: %w", err)
	}
	if err := func() error {
		defer func() { _ = erows.Close() }()
		for erows.Next() {
			var from, to, depType, metadata string
			if err := erows.Scan(&from, &to, &depType, &metadata); err != nil {
				return fmt.Errorf("journal store: scanning block-graph edge: %w", err)
			}
			switch depType {
			case "blocks", "conditional-blocks":
				blockTargets[from] = append(blockTargets[from], to)
			case "waits-for":
				waitsFor[from] = append(waitsFor[from], waitsForDep{spawner: to, gate: frontierEdgeGate(metadata)})
			case "parent-child":
				parentsOf[from] = append(parentsOf[from], to)
				childrenOf[to] = append(childrenOf[to], from)
			}
		}
		return erows.Err()
	}(); err != nil {
		return nil, err
	}

	// A target/child status blocks (is "active") when the row exists and is
	// neither closed nor pinned — bd's `t.status <> 'closed' AND t.status <>
	// 'pinned'` under an INNER JOIN (dangling => not active).
	active := func(id string) bool {
		st, ok := status[id]
		return ok && st != "closed" && st != "pinned"
	}
	// A subject is eligible to be is_blocked only when it is not closed/pinned.
	eligible := func(id string) bool {
		st, ok := status[id]
		return ok && st != "closed" && st != "pinned"
	}

	gateBlocks := func(dep waitsForDep) bool {
		hasActiveChild := false
		hasClosedChild := false
		for _, child := range childrenOf[dep.spawner] {
			st := status[child]
			switch {
			case st == "closed":
				hasClosedChild = true
			case st != "pinned":
				// child rows always exist (FK on edges.from_id); a blank/unknown
				// status is treated as active, matching bd's non-closed/non-pinned.
				hasActiveChild = true
			}
		}
		if !hasActiveChild {
			return false
		}
		if dep.gate == "any-children" && hasClosedChild {
			return false // early release
		}
		return true
	}

	directBlocked := func(id string) bool {
		for _, t := range blockTargets[id] {
			if active(t) {
				return true
			}
		}
		for _, w := range waitsFor[id] {
			if gateBlocks(w) {
				return true
			}
		}
		return false
	}

	blocked := make(map[string]bool)
	for id := range status {
		if eligible(id) && directBlocked(id) {
			blocked[id] = true
		}
	}
	// Parent-child cascade: iterate the mark fixpoint until it converges
	// (blocked_state.go RecomputeIsBlockedInTx loops until 0 rows change).
	for {
		changed := false
		for id := range status {
			if blocked[id] || !eligible(id) {
				continue
			}
			for _, parent := range parentsOf[id] {
				if blocked[parent] {
					blocked[id] = true
					changed = true
					break
				}
			}
		}
		if !changed {
			break
		}
	}

	// Children of a future-deferred parent are excluded from ready even when the
	// child itself is not deferred (single hop, matching bd's join in
	// getChildrenOfDeferredParentsInTx, ready_work.go:521-528).
	deferredChild := make(map[string]bool)
	for parent, kids := range childrenOf {
		if futureDeferred[parent] {
			for _, child := range kids {
				deferredChild[child] = true
			}
		}
	}

	return &frontierBlockGraph{blocked: blocked, deferredChild: deferredChild}, nil
}

// frontierEdgeGate extracts the `gate` field from a waits-for edge's JSON
// metadata (`{"gate":"any-children"}`, minted by internal/formula/compile.go:751),
// mirroring bd's JSON_EXTRACT(d.metadata, '$.gate') (blocked_state.go:40).
// A blank or malformed metadata yields "" (no gate).
func frontierEdgeGate(metadata string) string {
	if strings.TrimSpace(metadata) == "" {
		return ""
	}
	var parsed struct {
		Gate string `json:"gate"`
	}
	if err := json.Unmarshal([]byte(metadata), &parsed); err != nil {
		return ""
	}
	return parsed.Gate
}

// frontierAssigneeTier reproduces one `bd --readonly ready --assignee=$cand
// --exclude-type=epic [--include-ephemeral] --limit=N` call (Arm A over
// fold_owned=0 nodes). The SQL narrows to open rows for the candidate in the
// requested tier; Go applies bd's is_blocked / deferred-child exclusions and the
// residual ready filters, then bd's default `--sort priority` order and the
// per-tier limit. The shell passes no `--sort` for the assignee call, so it
// takes bd's default priority sort (finding 4).
func (s *JournalStore) frontierAssigneeTier(ctx context.Context, q journalQueryer, assignee string, tier TierMode, now time.Time, bg *frontierBlockGraph, limit int) ([]Bead, error) {
	where := "n.status = 'open' AND n.assignee = ?"
	args := []any{assignee}
	if tc := journalTierClause(tier); tc != "" {
		where += " AND " + tc
	}
	rows, err := s.hydrateWhere(ctx, q, where, args)
	if err != nil {
		return nil, err
	}
	rows = filterFrontierCandidates(rows, now, tier, bg)
	sortBeadsPriorityOrder(rows) // bd's default --sort priority: priority ASC, created_at DESC, id ASC
	return capTierBeads(rows, limit), nil
}

// frontierRoutedTier reproduces one routed `bd --readonly ready
// --metadata-field "$key=$route" --unassigned --exclude-type=epic
// [--include-ephemeral] --sort oldest --limit=N` call (Arm A over fold_owned=0
// nodes). The metadata predicate is an EXISTS over node_metadata; the route and
// key are bound args. `--unassigned` maps to assignee = ”; `--sort oldest` maps
// to (created_at, id) ascending.
func (s *JournalStore) frontierRoutedTier(ctx context.Context, q journalQueryer, metaKey, route string, tier TierMode, now time.Time, bg *frontierBlockGraph, limit int) ([]Bead, error) {
	where := "n.status = 'open' AND n.assignee = '' AND " +
		"EXISTS (SELECT 1 FROM node_metadata m WHERE m.node_id = n.id AND m.key = ? AND m.value = ?)"
	args := []any{metaKey, route}
	if tc := journalTierClause(tier); tc != "" {
		where += " AND " + tc
	}
	rows, err := s.hydrateWhere(ctx, q, where, args)
	if err != nil {
		return nil, err
	}
	rows = filterFrontierCandidates(rows, now, tier, bg)
	sortBeadsOldest(rows) // --sort oldest
	return capTierBeads(rows, limit), nil
}

// frontierProjectionTier is Arm B: a covering-index walk of the frontier
// projection table for fold-owned (fold_owned=1) routed work, using the
// frontier_route_order index (route, ready_priority, created_at, id)
// (internal/graphstore/ddl.go). The façade maintains no frontier rows for its
// own fold_owned=0 writes (journal_store.go:57-60), so at P2 this returns rows
// only for fold-routed roots — dormant, but a live, tested read path from day
// one. The frontier table is already a ready projection; only the future-defer
// cutoff is applied here, then ids hydrate through the nodes read path.
func (s *JournalStore) frontierProjectionTier(ctx context.Context, q journalQueryer, route string, now time.Time, limit int) ([]Bead, error) {
	query := `SELECT f.node_id FROM frontier f
		WHERE f.route = ? AND (f.defer_until IS NULL OR f.defer_until <= ?)
		ORDER BY f.ready_priority, f.created_at, f.id`
	args := []any{route, journalFormatTime(now)}
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("journal store: frontier projection for route %q: %w", route, err)
	}
	ids, err := scanFrontierNodeIDs(rows)
	if err != nil {
		return nil, err
	}
	return s.hydrateIDsOrdered(ctx, q, ids)
}

func scanFrontierNodeIDs(rows *sql.Rows) ([]string, error) {
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("journal store: scanning frontier node id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("journal store: iterating frontier rows: %w", err)
	}
	return ids, nil
}

// hydrateIDsOrdered hydrates the given node ids (any fold_owned tier — Arm B
// rows are fold_owned=1) into full Beads, preserving the input id order that the
// frontier_route_order walk established.
func (s *JournalStore) hydrateIDsOrdered(ctx context.Context, q journalQueryer, ids []string) ([]Bead, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	sqlText := "SELECT " + journalNodeColumns + " FROM nodes n WHERE n.id IN (" + placeholders + ")"
	rows, err := q.QueryContext(ctx, sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("journal store: hydrating frontier ids: %w", err)
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
	byID := make(map[string]Bead, len(beads))
	for _, b := range beads {
		byID[b.ID] = b
	}
	ordered := make([]Bead, 0, len(ids))
	for _, id := range ids {
		if b, ok := byID[id]; ok {
			ordered = append(ordered, b)
		}
	}
	return ordered, nil
}

// filterFrontierCandidates drops the hydrated tier rows bd's ready query would
// not surface: bd-blocked rows (is_blocked = 1), children of a future-deferred
// parent, and rows failing the residual ready filters (excluded type, future
// defer, tier). This is ControlFrontier's own predicate — NOT
// IsReadyCandidateForTier — so it matches `bd ready`, not JournalStore.Ready.
func filterFrontierCandidates(in []Bead, now time.Time, tier TierMode, bg *frontierBlockGraph) []Bead {
	out := in[:0:0]
	for _, b := range in {
		if bg.blocked[b.ID] || bg.deferredChild[b.ID] {
			continue
		}
		if !frontierReadyCandidate(b, now, tier) {
			continue
		}
		out = append(out, b)
	}
	return out
}

// frontierReadyCandidate is the store-independent half of bd's ready filter for
// ControlFrontier: open status, actionable type (frontierExcludedTypes), no
// future defer, in the requested tier. It applies NO ready-excluded-label check
// — bd's ready query applies no default label exclusion for the dispatcher's
// invocation (finding 5).
func frontierReadyCandidate(b Bead, now time.Time, tier TierMode) bool {
	switch tier {
	case TierWisps:
		if !b.Ephemeral && !b.NoHistory {
			return false
		}
	case TierBoth:
		// no tier filter
	default: // TierIssues: durable only; ephemeral hidden.
		if b.Ephemeral {
			return false
		}
	}
	if b.Status != "open" {
		return false
	}
	if frontierExcludedTypes[b.Type] {
		return false
	}
	return !IsDeferred(b, now)
}

// sortBeadsPriorityOrder sorts into bd's default `--sort priority` order:
// priority ASC, created_at DESC, id ASC (BuildReadyWorkOrder(SortPolicyPriority),
// sqlbuild/ready.go:55-56). A nil priority sorts as 2 (bd's stored default). The
// created_at tie-break is DESCending — newest first — which is what distinguishes
// the assignee tier from the routed `--sort oldest` tier (finding 4).
func sortBeadsPriorityOrder(items []Bead) {
	sort.Slice(items, func(i, j int) bool {
		pi, pj := frontierSortPriority(items[i]), frontierSortPriority(items[j])
		if pi != pj {
			return pi < pj
		}
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.After(items[j].CreatedAt) // DESC: newest first
		}
		return items[i].ID < items[j].ID
	})
}

// frontierSortPriority maps a nil Priority to 2, matching bd's stored default
// and COALESCE(priority, 2) in the SQL sort.
func frontierSortPriority(b Bead) int {
	if b.Priority == nil {
		return 2
	}
	return *b.Priority
}

// sortBeadsOldest sorts into (created_at, id) ascending — bd's `--sort oldest`
// (BuildReadyWorkOrder(SortPolicyOldest), sqlbuild/ready.go:53-54).
func sortBeadsOldest(items []Bead) {
	sort.Slice(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})
}

func capTierBeads(items []Bead, limit int) []Bead {
	if limit > 0 && len(items) > limit {
		return items[:limit]
	}
	return items
}

// dedupeControlFrontier reproduces the jq merge filter (the jq reduce/merge
// filter in workflowServeControlReadyQueryForBeads):
//
//	reduce add[] as $item ([];
//	  if instantiating != "" then . elif seen(.id) then . else . + [$item] end)
//
// It drops any bead whose instantiating metadata is non-empty (a
// half-materialized molecule never dispatches), then keeps the first occurrence
// of each id, preserving the tier-concatenation order.
func dedupeControlFrontier(merged []Bead, instantiatingKey string) []Bead {
	out := make([]Bead, 0, len(merged))
	seen := make(map[string]bool, len(merged))
	for _, b := range merged {
		if instantiatingKey != "" && b.Metadata[instantiatingKey] != "" {
			continue
		}
		if seen[b.ID] {
			continue
		}
		seen[b.ID] = true
		out = append(out, b)
	}
	return out
}
