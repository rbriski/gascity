package beads

import (
	"context"
	"fmt"
	"maps"
	"strings"
)

// Compile-time proof that the recovered SQLite store provides the graph-apply
// capability — i.e. it satisfies the GraphStore seam (coordrouter.GraphStore is
// GraphApplyStore) and the optional tier-aware extension.
var (
	_ GraphApplyStore        = (*SQLiteStore)(nil)
	_ StorageGraphApplyStore = (*SQLiteStore)(nil)
)

// ApplyGraphPlan atomically instantiates an entire bead graph (nodes + edges) in
// one transaction, returning the symbolic-key -> concrete-ID map. It is the
// SQLite store's graph-apply capability — the operation ClassGraph consumers use
// (via beads.GraphApplyFor) to pour a formula-v2 topology. It is the in-process,
// single-transaction analog of the per-bead Create + DepAdd the classic path
// performs, with no fork/exec or per-bead commit.
func (s *SQLiteStore) ApplyGraphPlan(ctx context.Context, plan *GraphApplyPlan) (*GraphApplyResult, error) {
	return s.ApplyGraphPlanWithStorage(ctx, plan, StorageDefault)
}

// ApplyGraphPlanWithStorage is ApplyGraphPlan with an explicit physical storage
// tier for every node in the plan (history / no_history / ephemeral), mirroring
// the tier-selection the policy chokepoint applies on the classic path.
func (s *SQLiteStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *GraphApplyPlan, storage StorageClass) (*GraphApplyResult, error) {
	if plan == nil {
		return nil, fmt.Errorf("sqlite graph apply: plan is nil")
	}
	ephemeral, noHistory, err := graphStorageFlags(storage)
	if err != nil {
		return nil, fmt.Errorf("sqlite graph apply: %w", err)
	}

	result := &GraphApplyResult{IDs: make(map[string]string, len(plan.Nodes))}
	if len(plan.Nodes) == 0 {
		return result, nil
	}

	// Pass 1: materialize each node into a Bead with a concrete ID (so symbolic
	// keys in edges and MetadataRefs can resolve) but do not touch the DB yet.
	staged := make([]Bead, len(plan.Nodes))
	for i, node := range plan.Nodes {
		key := strings.TrimSpace(node.Key)
		if key == "" {
			return nil, fmt.Errorf("sqlite graph apply: node %d has empty key", i)
		}
		if _, dup := result.IDs[key]; dup {
			return nil, fmt.Errorf("sqlite graph apply: duplicate node key %q", key)
		}
		b := s.normalizeCreate(Bead{
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
		result.IDs[key] = b.ID
		staged[i] = b
	}

	// Pass 2: resolve symbolic parent keys and metadata refs now that every key
	// has a concrete ID.
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

	// Pass 3: one transaction — create every node, then wire every edge. The
	// parent relationship rides the bead's parent_id column (matching Create and
	// the Children query); plan.Edges become deps rows (matching DepAdd).
	err = retryOnBusy(func() error {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("sqlite graph apply: begin tx: %w", err)
		}
		defer tx.Rollback() //nolint:errcheck
		for _, b := range staged {
			if err := s.ensureCreateDoesNotExist(ctx, tx, b.ID); err != nil {
				return err
			}
			if err := s.upsertBeadTx(ctx, tx, b); err != nil {
				return err
			}
		}
		for i, edge := range plan.Edges {
			from := graphApplyResolveRef(edge.FromKey, edge.FromID, result.IDs)
			to := graphApplyResolveRef(edge.ToKey, edge.ToID, result.IDs)
			if from == "" || to == "" {
				return fmt.Errorf("sqlite graph apply: edge %d has an unresolved endpoint (from=%q to=%q)", i, from, to)
			}
			if err := s.depAddTx(ctx, tx, from, to, edge.Type); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// graphApplyResolveRef resolves an edge endpoint: a symbolic key resolves through
// keyToID (this-plan node), otherwise the literal id is used (an existing bead).
func graphApplyResolveRef(key, id string, keyToID map[string]string) string {
	if k := strings.TrimSpace(key); k != "" {
		return keyToID[k]
	}
	return strings.TrimSpace(id)
}
