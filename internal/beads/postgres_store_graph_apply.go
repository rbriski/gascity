package beads

import (
	"context"
	"fmt"
	"maps"
	"strings"
)

// Compile-time proof that the Postgres store provides the graph-apply capability
// — i.e. it satisfies the GraphStore seam (coordrouter.GraphStore is
// GraphApplyStore) and the optional tier-aware extension. This is the parity that
// lets a Postgres-backed ClassGraph pour a formula-v2 topology, unblocking
// graph=postgres (the SQLite analog lives in sqlite_store_graph_apply.go).
var (
	_ GraphApplyStore        = (*PostgresStore)(nil)
	_ StorageGraphApplyStore = (*PostgresStore)(nil)
)

// ApplyGraphPlan atomically instantiates an entire bead graph (nodes + edges) in
// one transaction, returning the symbolic-key -> concrete-ID map. It is the
// Postgres store's graph-apply capability — the operation ClassGraph consumers use
// (via beads.GraphApplyFor) to pour a formula-v2 topology — the single-transaction
// analog of the per-bead Create + DepAdd the classic path performs.
func (s *PostgresStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage is ApplyGraphPlan with an explicit physical storage
// tier for every node in the plan (history / no_history / ephemeral), mirroring
// the tier-selection the policy chokepoint applies on the classic path.
//
// Unlike the SQLite analog, Postgres mints ids from a native per-schema SEQUENCE
// (nextval — concurrency-safe across processes), so ids are minted inside the
// transaction and can never collide with a concurrent writer. That removes the
// SQLite path's mint-outside-then-re-mint-on-collision dance: a single pass mints
// final ids, so edges, parents, and metadata refs resolve directly.
func (s *PostgresStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("postgres graph apply: plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("postgres graph apply: %w", err)
	}

	result := &GraphApplyResult{IDs: make(map[string]string, len(plan.Nodes))}
	if len(plan.Nodes) == 0 {
		return result, nil
	}

	// Validate node keys (non-empty, unique) before opening a tx or burning any
	// sequence values on a malformed plan.
	seen := make(map[string]struct{}, len(plan.Nodes))
	for i, node := range plan.Nodes {
		key := strings.TrimSpace(node.Key)
		if key == "" {
			return nil, fmt.Errorf("postgres graph apply: node %d has empty key", i)
		}
		if _, dup := seen[key]; dup {
			return nil, fmt.Errorf("postgres graph apply: duplicate node key %q", key)
		}
		seen[key] = struct{}{}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("postgres graph apply: begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	// Pass 1: mint a concrete id for every node (in-tx nextval, collision-free) so
	// symbolic keys in edges and MetadataRefs can resolve. Mirrors the SQLite
	// node-field mapping exactly.
	staged := make([]Bead, len(plan.Nodes))
	for i, node := range plan.Nodes {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT nextval('bead_seq')`).Scan(&n); err != nil {
			return nil, fmt.Errorf("postgres graph apply: nextval: %w", err)
		}
		b := s.normalizeCreate(Bead{
			ID:          fmt.Sprintf("%s-%d", s.prefix, n),
			Title:       node.Title,
			Description: node.Description,
			Type:        node.Type,
			Priority:    node.Priority,
			Assignee:    node.Assignee,
			From:        node.From,
			ParentID:    node.ParentID,
			Labels:      append([]string(nil), node.Labels...),
			Metadata:    maps.Clone(node.Metadata),
			Ephemeral:   ephemeral,
			NoHistory:   noHistory,
		})
		result.IDs[strings.TrimSpace(node.Key)] = b.ID
		staged[i] = b
	}

	// Pass 2: resolve symbolic parent keys and metadata refs now that every key has
	// a concrete ID.
	for i, node := range plan.Nodes {
		if pk := strings.TrimSpace(node.ParentKey); pk != "" {
			staged[i].ParentID = result.IDs[pk]
		}
		for metaKey, refKey := range node.MetadataRefs {
			if staged[i].Metadata == nil {
				staged[i].Metadata = make(map[string]string, len(node.MetadataRefs))
			}
			staged[i].Metadata[metaKey] = result.IDs[refKey]
		}
	}

	// Pass 3: insert every node (ON CONFLICT DO NOTHING; fresh nextval ids cannot
	// collide, so a non-insert is a hard error), then wire every edge. The parent
	// relationship rides the bead's parent_id column (matching Create and the
	// Children query); plan.Edges become deps rows (matching DepAdd).
	for _, b := range staged {
		inserted, err := s.insertBeadTx(ctx, tx, b)
		if err != nil {
			return nil, err
		}
		if !inserted {
			return nil, fmt.Errorf("postgres graph apply: duplicate id %q", b.ID)
		}
	}
	for i, edge := range plan.Edges {
		from := graphApplyResolveRef(edge.FromKey, edge.FromID, result.IDs)
		to := graphApplyResolveRef(edge.ToKey, edge.ToID, result.IDs)
		if from == "" || to == "" {
			return nil, fmt.Errorf("postgres graph apply: edge %d has an unresolved endpoint (from=%q to=%q)", i, from, to)
		}
		if err := s.depAddTx(ctx, tx, from, to, edge.Type); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("postgres graph apply: commit: %w", err)
	}
	return result, nil
}
