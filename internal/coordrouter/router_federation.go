package coordrouter

import (
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
)

// This file federates the Router's read surface across its per-class backends.
//
// Every method short-circuits to direct delegation when there is exactly one
// distinct backend (the identity phase: all classes → one store), so the Router
// is byte-identical to that backend until a class is relocated. Only once a class
// points at a different backend do the union/dedup/sort paths run.
//
// Mutations and by-id ops (Update/Close/SetMetadata/Dep{Add,Remove}/Tx/...) are
// left to the embedded primary here; per-id by-class routing is B3.

// soleBackend returns the single backend when the Router has exactly one distinct
// backend, enabling a byte-identical fast path.
func (r *Router) soleBackend() (beads.Store, bool) {
	bs := r.Backends()
	if len(bs) == 1 {
		return bs[0], true
	}
	return nil, false
}

// Get federates a point read: a bead lives in exactly one backend, so it queries
// each in turn and returns the first hit, ErrNotFound when none has it.
func (r *Router) Get(id string) (beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.Get(id)
	}
	var lastErr error
	for _, backend := range r.Backends() {
		got, err := backend.Get(id)
		if err == nil {
			return got, nil
		}
		lastErr = err
	}
	if lastErr != nil {
		return beads.Bead{}, lastErr
	}
	return beads.Bead{}, beads.ErrNotFound
}

// List federates a query: union each backend's matches, dedup by id, then re-sort
// and re-limit over the combined set so the result matches a single store spanning
// all backends.
func (r *Router) List(query beads.ListQuery) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.List(query)
	}
	return r.federateRead(query.Sort, query.Limit, func(s beads.Store) ([]beads.Bead, error) {
		return s.List(query)
	})
}

// ListOpen federates the legacy open-list helper (creation order).
func (r *Router) ListOpen(status ...string) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.ListOpen(status...)
	}
	return r.federateRead(beads.SortCreatedAsc, 0, func(s beads.Store) ([]beads.Bead, error) {
		return s.ListOpen(status...)
	})
}

// Children federates the parent-child listing (creation order). Children may live
// in a different backend than their parent, so the union is required.
func (r *Router) Children(parentID string, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.Children(parentID, opts...)
	}
	return r.federateRead(beads.SortCreatedAsc, 0, func(s beads.Store) ([]beads.Bead, error) {
		return s.Children(parentID, opts...)
	})
}

// ListByLabel federates a label query (newest first).
func (r *Router) ListByLabel(label string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.ListByLabel(label, limit, opts...)
	}
	return r.federateRead(beads.SortCreatedDesc, limit, func(s beads.Store) ([]beads.Bead, error) {
		return s.ListByLabel(label, limit, opts...)
	})
}

// ListByAssignee federates an assignee query (newest first).
func (r *Router) ListByAssignee(assignee, status string, limit int) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.ListByAssignee(assignee, status, limit)
	}
	return r.federateRead(beads.SortCreatedDesc, limit, func(s beads.Store) ([]beads.Bead, error) {
		return s.ListByAssignee(assignee, status, limit)
	})
}

// ListByMetadata federates a metadata query (newest first).
func (r *Router) ListByMetadata(filters map[string]string, limit int, opts ...beads.QueryOpt) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.ListByMetadata(filters, limit, opts...)
	}
	return r.federateRead(beads.SortCreatedDesc, limit, func(s beads.Store) ([]beads.Bead, error) {
		return s.ListByMetadata(filters, limit, opts...)
	})
}

// Ready federates the ready-work lookup: union each backend's ready set, dedup by
// id, sort by (created_at asc, id asc) — the canonical ready order — then limit.
func (r *Router) Ready(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.Ready(query...)
	}
	limit := 0
	if len(query) > 0 {
		limit = query[0].Limit
	}
	return r.federateRead(beads.SortCreatedAsc, limit, func(s beads.Store) ([]beads.Bead, error) {
		return s.Ready(query...)
	})
}

// DepList federates dependency reads: an edge touching id may be recorded in any
// backend (a cross-class blocks edge lives in the Work store), so it unions and
// dedups across backends.
func (r *Router) DepList(id, direction string) ([]beads.Dep, error) {
	if b, ok := r.soleBackend(); ok {
		return b.DepList(id, direction)
	}
	seen := make(map[beads.Dep]bool)
	var out []beads.Dep
	var lastErr error
	for _, backend := range r.Backends() {
		deps, err := backend.DepList(id, direction)
		if err != nil {
			lastErr = err
			continue
		}
		for _, d := range deps {
			if !seen[d] {
				seen[d] = true
				out = append(out, d)
			}
		}
	}
	if out == nil && lastErr != nil {
		return nil, lastErr
	}
	return out, nil
}

// federateRead runs read on every backend, unions the results, dedups by id, and
// re-sorts + re-limits over the combined set.
func (r *Router) federateRead(order beads.SortOrder, limit int, read func(beads.Store) ([]beads.Bead, error)) ([]beads.Bead, error) {
	seen := make(map[string]bool)
	var merged []beads.Bead
	var lastErr error
	for _, backend := range r.Backends() {
		got, err := read(backend)
		if err != nil {
			lastErr = err
			continue
		}
		for _, b := range got {
			if seen[b.ID] {
				continue
			}
			seen[b.ID] = true
			merged = append(merged, b)
		}
	}
	if merged == nil && lastErr != nil {
		return nil, lastErr
	}
	sortBeads(merged, order)
	if limit > 0 && len(merged) > limit {
		merged = merged[:limit]
	}
	return merged, nil
}

// sortBeads orders beads by creation time (with id as a stable tiebreak) in the
// direction implied by order. SortCreatedAsc is ascending; every other order
// (Default/CreatedDesc) is descending, matching the per-method conventions.
func sortBeads(beadsList []beads.Bead, order beads.SortOrder) {
	asc := order == beads.SortCreatedAsc
	sort.SliceStable(beadsList, func(i, j int) bool {
		ti, tj := beadsList[i].CreatedAt, beadsList[j].CreatedAt
		if ti.Equal(tj) {
			if asc {
				return beadsList[i].ID < beadsList[j].ID
			}
			return beadsList[i].ID > beadsList[j].ID
		}
		if asc {
			return ti.Before(tj)
		}
		return ti.After(tj)
	})
}
