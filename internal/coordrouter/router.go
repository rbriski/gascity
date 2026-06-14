// Package coordrouter routes bead persistence operations to a per-class
// backend, using the work-vs-infrastructure boundary defined by
// internal/coordclass.
//
// The Router is the seam that will replace cmd/gc/bead_policy_store.go's
// wrapStoreWithBeadPolicies: it classifies each created bead (and each graph
// plan) and routes it to the backend registered for that [coordclass.Class].
// Its first deployment registers every class to the same cached bd store, which
// makes it a provable identity transform; later phases register a faster
// backend for one class at a time behind the same Router.
//
// Scope of this skeleton (deliberately minimal — see
// engdocs/design/beads-work-infra-split.md):
//
//   - Create routes by coordclass.Classify(bead).
//   - ApplyGraphPlan routes by coordclass.ClassifyGraphPlan(plan), exposed
//     through the beads.GraphApplyHandleProvider extension point.
//   - Every other beads.Store operation delegates to the primary (work)
//     backend. Per-class read fan-out and by-ID mutation routing are a later
//     phase (P2); when all classes share one backend they are no-ops anyway.
//   - Storage-class selection and read-tier expansion (today done by
//     beadPolicyStore) are intentionally NOT yet here; the wiring commit that
//     re-points wrapStoreWithBeadPolicies preserves them. This package has no
//     production importers yet.
package coordrouter

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

// Router routes creation operations to a per-class backend and delegates all
// other beads.Store operations to its primary (work) backend. It implements
// beads.Store and, when its graph backend supports graph-apply,
// beads.GraphApplyHandleProvider.
type Router struct {
	// Store is the primary backend: the ClassWork store and the delegate for
	// every operation the Router does not route. Embedding satisfies the rest of
	// the beads.Store surface for free.
	beads.Store

	backends map[coordclass.Class]beads.Store
}

// Compile-time assertions: a Router is a beads.Store and advertises graph-apply
// through the handle-provider extension point (never by directly implementing
// GraphApplyStore, so beads.GraphApplyFor consults the routed handle).
var (
	_ beads.Store                    = (*Router)(nil)
	_ beads.GraphApplyHandleProvider = (*Router)(nil)
)

// New returns a Router whose primary (ClassWork) backend is primary. primary
// must be non-nil; it is the delegate for every unrouted operation and the
// fallback for any class without its own registered backend. Register adds or
// overrides the backend for a class.
func New(primary beads.Store) *Router {
	r := &Router{
		Store:    primary,
		backends: make(map[coordclass.Class]beads.Store, len(coordclass.Classes())),
	}
	r.backends[coordclass.ClassWork] = primary
	return r
}

// Register sets the backend for a class. Registering ClassWork also replaces the
// primary delegate. A nil store is ignored.
func (r *Router) Register(c coordclass.Class, store beads.Store) {
	if store == nil {
		return
	}
	r.backends[c] = store
	if c == coordclass.ClassWork {
		r.Store = store
	}
}

// Backend returns the backend registered for c, falling back to the primary
// (work) backend when c has no dedicated registration.
func (r *Router) Backend(c coordclass.Class) beads.Store {
	if store, ok := r.backends[c]; ok && store != nil {
		return store
	}
	return r.Store
}

// Backends returns each DISTINCT registered backend (the primary plus any
// per-class overrides), deduplicated by identity. It exists for lifecycle
// operations — notably close — so a caller can reach every underlying store the
// Router fans out to. In the identity phase (all classes → one backend) it
// returns a single store; after a class relocates it returns each distinct one.
func (r *Router) Backends() []beads.Store {
	seen := make(map[beads.Store]bool, len(r.backends)+1)
	var out []beads.Store
	add := func(s beads.Store) {
		if s != nil && !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	add(r.Store)
	for _, c := range coordclass.Classes() {
		add(r.backends[c])
	}
	return out
}

// Create classifies the bead and routes it to the owning class's backend.
func (r *Router) Create(b beads.Bead) (beads.Bead, error) {
	return r.Backend(coordclass.Classify(b)).Create(b)
}

// CreateWithStorage routes a tier-selected create to the owning class's backend,
// satisfying beads.StorageCreateStore. The policy wrapper above the Router
// computes the tier (from config) and calls this; the Router only forwards the
// already-selected tier to the right backend, so the tier survives the split.
// A backend without tier support gets a plain Create (it has no tiers).
func (r *Router) CreateWithStorage(b beads.Bead, storage beads.StorageClass) (beads.Bead, error) {
	backend := r.Backend(coordclass.Classify(b))
	if sc, ok := backend.(beads.StorageCreateStore); ok {
		return sc.CreateWithStorage(b, storage)
	}
	return backend.Create(b)
}

// GraphApplyHandle exposes a routed graph-apply capability when the graph-class
// backend supports one, satisfying beads.GraphApplyHandleProvider so that
// beads.GraphApplyFor(router) resolves to the routed applier. It reports false
// when the graph backend cannot apply graphs, exactly mirroring the conditional
// capability of the store it wraps.
func (r *Router) GraphApplyHandle() (beads.GraphApplyStore, bool) {
	if _, ok := beads.GraphApplyFor(r.Backend(coordclass.ClassGraph)); !ok {
		return nil, false
	}
	return routedGraphApplier{r: r}, true
}

// routedGraphApplier routes each ApplyGraphPlan call to the backend that owns
// the plan's class. Classification is per-call because a plan's class is a
// property of its nodes, not of the Router.
type routedGraphApplier struct {
	r *Router
}

// ApplyGraphPlan classifies the plan and applies it on the owning class's
// graph-apply backend.
func (g routedGraphApplier) ApplyGraphPlan(ctx context.Context, plan *beads.GraphApplyPlan) (*beads.GraphApplyResult, error) {
	class := coordclass.ClassifyGraphPlan(plan)
	applier, ok := beads.GraphApplyFor(g.r.Backend(class))
	if !ok {
		return nil, fmt.Errorf("coordrouter: %s backend does not support graph apply", class)
	}
	return applier.ApplyGraphPlan(ctx, plan)
}

// ApplyGraphPlanWithStorage classifies the plan and applies it on the owning
// class's backend in the caller-selected storage tier, satisfying
// beads.StorageGraphApplyStore. The policy wrapper above the Router computes the
// tier; the Router forwards it to the owning backend so a graph.v2 pour keeps its
// ephemeral/no-history placement after the split. When the backend cannot honor a
// tier it falls back to the untiered pour for StorageDefault and refuses any other
// tier rather than silently dropping it.
func (g routedGraphApplier) ApplyGraphPlanWithStorage(ctx context.Context, plan *beads.GraphApplyPlan, storage beads.StorageClass) (*beads.GraphApplyResult, error) {
	class := coordclass.ClassifyGraphPlan(plan)
	applier, ok := beads.GraphApplyFor(g.r.Backend(class))
	if !ok {
		return nil, fmt.Errorf("coordrouter: %s backend does not support graph apply", class)
	}
	if storageApplier, ok := applier.(beads.StorageGraphApplyStore); ok {
		return storageApplier.ApplyGraphPlanWithStorage(ctx, plan, storage)
	}
	if storage == beads.StorageDefault {
		return applier.ApplyGraphPlan(ctx, plan)
	}
	return nil, fmt.Errorf("coordrouter: %s backend does not support tier-selected graph apply (storage=%q)", class, storage)
}

var (
	_ beads.StorageCreateStore     = (*Router)(nil)
	_ beads.StorageGraphApplyStore = routedGraphApplier{}
)
