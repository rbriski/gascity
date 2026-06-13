package coordrouter

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// BdGraphStore is the bd-delegating first implementation of [GraphStore]: it
// resolves the graph-apply capability of a backing [beads.Store] via
// [beads.GraphApplyFor] and delegates the atomic formula-v2 pour to it. It is the
// graph-class analog of "the bd store itself is the first impl" — graph needs a
// real adapter because ApplyGraphPlan is a capability, not a beads.Store method.
//
// It additionally satisfies the optional [beads.StorageGraphApplyStore]: when the
// backend supports tier-selected graph creates it forwards the storage tier,
// mirroring beadPolicyGraphStore (cmd/gc/bead_policy_store.go:207-219); when the
// backend does not, it falls back to the untiered pour for StorageDefault and
// refuses a non-default tier rather than silently dropping it.
//
// Capability resolution is lazy (per call) so a backend whose graph-apply
// capability depends on wrapped runtime state — e.g. a CachingStore over a graph
// backend — is honored exactly as beads.GraphApplyFor honors it.
type BdGraphStore struct {
	backend beads.Store
}

// NewBdGraphStore returns a GraphStore that delegates graph applies to backend's
// graph-apply capability. backend must be non-nil.
func NewBdGraphStore(backend beads.Store) *BdGraphStore {
	return &BdGraphStore{backend: backend}
}

// ApplyGraphPlan delegates the pour to the backend's graph-apply capability,
// returning an error when the backend has none.
func (s *BdGraphStore) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	applier, ok := beads.GraphApplyFor(s.backend)
	if !ok {
		return nil, fmt.Errorf("coordrouter: graph backend does not support graph apply")
	}
	return applier.ApplyGraphPlan(ctx, plan)
}

// ApplyGraphPlanWithStorage delegates a tier-selected pour to the backend when it
// supports beads.StorageGraphApplyStore. When it does not, a StorageDefault
// request falls back to the untiered pour, and any other tier is refused so the
// requested placement is never silently lost.
func (s *BdGraphStore) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	applier, ok := beads.GraphApplyFor(s.backend)
	if !ok {
		return nil, fmt.Errorf("coordrouter: graph backend does not support graph apply")
	}
	if storageApplier, ok := applier.(beads.StorageGraphApplyStore); ok {
		return storageApplier.ApplyGraphPlanWithStorage(ctx, plan, storage)
	}
	if storage == beads.StorageDefault {
		return applier.ApplyGraphPlan(ctx, plan)
	}
	return nil, fmt.Errorf("coordrouter: graph backend does not support tier-selected graph apply (storage=%q)", storage)
}

var (
	_ GraphStore                   = (*BdGraphStore)(nil)
	_ beads.StorageGraphApplyStore = (*BdGraphStore)(nil)
)
