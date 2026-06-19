package coordrouter

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file routes by-id mutations and capabilities to the backend that owns the
// bead, and preserves the optional capabilities (ConditionalAssignmentReleaser,
// Counter) that the policy wrapper delegates by type-assertion — which the
// embedded beads.Store field does NOT promote.
//
// Every method short-circuits to the sole backend in the identity phase, so the
// Router is byte-identical until a class is relocated.

// backendForID returns the backend that physically owns id (the one whose Get
// succeeds), which by construction is id's class backend. It short-circuits to
// the sole backend in the identity phase and falls back to the primary when no
// backend has the bead, so the operation surfaces the natural not-found error.
func (r *Router) backendForID(id string) beads.Store {
	if b, ok := r.soleBackend(); ok {
		return b
	}
	// Static prefix routing first: if a backend owns id's prefix and actually
	// holds the bead, route there without probing the other backends' Get. This
	// skips the wasted Get probe on the non-owning store — for the bd-fork Dolt
	// backend a ~1s `bd` exec on every Update/Close/SetMetadata of a graph-class
	// (gcg-) bead. The owner Get is on its own store (SQLite for graph), so it is
	// cheap; on miss (stray id / partial migration) we fall back to the full
	// federated probe below, preserving the original ownership semantics.
	if owner := r.prefixBackendForID(id); owner != nil {
		if _, err := owner.Get(id); err == nil {
			return owner
		}
	}
	for _, backend := range r.Backends() {
		if _, err := backend.Get(id); err == nil {
			return backend
		}
	}
	return r.Store
}

// Update routes a field update to the bead's owning backend.
func (r *Router) Update(id string, opts beads.UpdateOpts) error {
	return r.backendForID(id).Update(id, opts)
}

// Close routes a close to the bead's owning backend.
func (r *Router) Close(id string) error { return r.backendForID(id).Close(id) }

// Reopen routes a reopen to the bead's owning backend.
func (r *Router) Reopen(id string) error { return r.backendForID(id).Reopen(id) }

// Delete routes a delete to the bead's owning backend.
func (r *Router) Delete(id string) error { return r.backendForID(id).Delete(id) }

// SetMetadata routes a single-key metadata write to the bead's owning backend.
func (r *Router) SetMetadata(id, key, value string) error {
	return r.backendForID(id).SetMetadata(id, key, value)
}

// SetMetadataBatch routes a multi-key metadata write to the bead's owning backend.
func (r *Router) SetMetadataBatch(id string, kvs map[string]string) error {
	return r.backendForID(id).SetMetadataBatch(id, kvs)
}

// DepAdd routes a dependency edge to the backend owning the dependent (issue)
// bead, so a cross-class blocks edge (e.g. a work bead blocked by a graph root)
// is recorded in the Work store — the pinned home for ready-blocking deps.
func (r *Router) DepAdd(issueID, dependsOnID, depType string) error {
	return r.backendForID(issueID).DepAdd(issueID, dependsOnID, depType)
}

// DepRemove routes a dependency removal to the backend owning the issue bead.
func (r *Router) DepRemove(issueID, dependsOnID string) error {
	return r.backendForID(issueID).DepRemove(issueID, dependsOnID)
}

// CloseAll groups ids by owning backend and batch-closes within each, summing the
// counts. The metadata is applied to every closed bead in every backend.
func (r *Router) CloseAll(ids []string, metadata map[string]string) (int, error) {
	if b, ok := r.soleBackend(); ok {
		return b.CloseAll(ids, metadata)
	}
	byBackend := make(map[beads.Store][]string)
	for _, id := range ids {
		backend := r.backendForID(id)
		byBackend[backend] = append(byBackend[backend], id)
	}
	total := 0
	var firstErr error
	for backend, batch := range byBackend {
		n, err := backend.CloseAll(batch, metadata)
		total += n
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return total, firstErr
}

// ReleaseIfCurrent routes the conditional-assignment release to the bead's owning
// backend, satisfying beads.ConditionalAssignmentReleaser. Without this the policy
// wrapper's type-assertion would miss the capability through the Router and the
// controller's orphan/affinity releases would silently no-op.
func (r *Router) ReleaseIfCurrent(id, expectedAssignee string) (bool, error) {
	releaser, ok := r.backendForID(id).(beads.ConditionalAssignmentReleaser)
	if !ok {
		return false, beads.ErrConditionalReleaseUnsupported
	}
	return releaser.ReleaseIfCurrent(id, expectedAssignee)
}

// Count delegates to the sole backend's Counter in the identity phase. A
// cross-backend count cannot dedup ids cheaply, so the split case reports
// unsupported and callers fall back to List (which federates correctly).
func (r *Router) Count(ctx context.Context, query beads.ListQuery, excludeTypes ...string) (int, error) {
	if b, ok := r.soleBackend(); ok {
		if counter, ok := b.(beads.Counter); ok {
			return counter.Count(ctx, query, excludeTypes...)
		}
	}
	return 0, beads.ErrCountUnsupported
}

// Claim routes an atomic claim to the bead's owning backend, bridging the two
// claim shapes the split spans: a backend that takes an explicit assignee
// (beads.Claimer, e.g. the SQLite graph store) is claimed with assignee, while a
// backend that claims for its own configured actor (beads.EnvActorClaimer, e.g. a
// BdStore with BEADS_ACTOR baked into its runner) is claimed without it. For the
// latter the caller must have constructed the backend with the matching actor, so
// assignee is advisory there. Returns ErrClaimUnsupported when the owning backend
// can do neither. Declaring Claim(id, assignee) here also shadows any embedded
// single-arg Claim promoted from the primary backend, so the Router presents one
// claim surface.
func (r *Router) Claim(id, assignee string) (beads.Bead, bool, error) {
	backend := r.backendForID(id)
	if c, ok := backend.(beads.Claimer); ok {
		return c.Claim(id, assignee)
	}
	if c, ok := backend.(beads.EnvActorClaimer); ok {
		return c.Claim(id)
	}
	return beads.Bead{}, false, beads.ErrClaimUnsupported
}

var (
	_ beads.ConditionalAssignmentReleaser = (*Router)(nil)
	_ beads.Counter                       = (*Router)(nil)
	_ beads.Claimer                       = (*Router)(nil)
)
