package coordrouter

import (
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
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

// prefixBackendForID returns the distinct backend that owns id's prefix (via the
// optional IDPrefix() accessor), or nil when no backend claims the prefix or the
// Router has a single backend. By-id reads use it to skip backends that provably
// cannot hold the bead — eliminating the wasted miss on the non-owning store,
// which for the bd-fork Dolt backend costs a ~1s `bd` exec per call. Bead ids are
// prefix-disjoint across backends, so the owning backend is the sole residence.
//
// It is intentionally distinct from the mutation path's backendForID (which
// resolves ownership by probing each backend's Get): this one routes purely on
// the static id prefix, so a read never forks the non-owning store at all.
func (r *Router) prefixBackendForID(id string) beads.Store {
	if _, ok := r.soleBackend(); ok {
		return nil
	}
	for _, b := range r.Backends() {
		if p, ok := b.(interface{ IDPrefix() string }); ok {
			if pfx := p.IDPrefix(); pfx != "" && strings.HasPrefix(id, pfx+"-") {
				return b
			}
		}
	}
	return nil
}

// Get federates a point read: a bead lives in exactly one backend, so it queries
// each in turn and returns the first hit, ErrNotFound when none has it.
func (r *Router) Get(id string) (beads.Bead, error) {
	if b, ok := r.soleBackend(); ok {
		return b.Get(id)
	}
	if owner := r.prefixBackendForID(id); owner != nil {
		if got, err := owner.Get(id); err == nil {
			return got, nil
		}
		// Owner miss (unknown prefix / partial migration): fall through to the
		// full federation below so correctness is never reduced.
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

// ReadyGraphOnly returns the ready set from the ClassGraph backend ALONE, never
// the ClassWork primary. It is the worker/dispatcher execution-readiness surface
// under graph_store=sqlite: a worker only ever executes graph nodes (molecule
// steps/wisps), so the Dolt work-leg is kept out of the readiness hot loop. The
// full federated Ready still serves the human/diagnostic backlog view. In the
// identity phase — no distinct ClassGraph backend — it falls back to the full
// Ready so a default Dolt-only city stays byte-identical.
func (r *Router) ReadyGraphOnly(query ...beads.ReadyQuery) ([]beads.Bead, error) {
	graph := r.Backend(coordclass.ClassGraph)
	if graph == nil || graph == r.Backend(coordclass.ClassWork) {
		return r.Ready(query...)
	}
	return graph.Ready(query...)
}

// ListGraphOnly returns List from the ClassGraph backend ALONE, never the
// ClassWork primary (mirrors ReadyGraphOnly). It is the dispatcher's root-scoped
// scope-check surface for a graph-resident molecule: every member carries a
// graph (gcg-) root, so the Dolt work-leg is kept out of the List and no `bd`
// fork happens. In the identity phase — no distinct ClassGraph backend — it falls
// back to the full federated List so a default Dolt-only city stays byte-identical.
func (r *Router) ListGraphOnly(query beads.ListQuery) ([]beads.Bead, error) {
	graph := r.Backend(coordclass.ClassGraph)
	if graph == nil || graph == r.Backend(coordclass.ClassWork) {
		return r.List(query)
	}
	return beads.HandlesFor(graph).Live.List(query)
}

// GraphIDPrefix reports the ClassGraph backend's id prefix, or "" when there is no
// distinct graph backend, so callers gate graph-only List to graph-rooted queries.
func (r *Router) GraphIDPrefix() string {
	graph := r.Backend(coordclass.ClassGraph)
	if graph == nil || graph == r.Backend(coordclass.ClassWork) {
		return ""
	}
	if p, ok := graph.(interface{ IDPrefix() string }); ok {
		return p.IDPrefix()
	}
	return ""
}

// DepList federates dependency reads: an edge touching id may be recorded in any
// backend (a cross-class blocks edge lives in the Work store), so it unions and
// dedups across backends.
func (r *Router) DepList(id, direction string) ([]beads.Dep, error) {
	if b, ok := r.soleBackend(); ok {
		return b.DepList(id, direction)
	}
	if owner := r.prefixBackendForID(id); owner != nil {
		if deps, err := owner.DepList(id, direction); err == nil && len(deps) > 0 {
			return deps, nil
		}
		// Empty or error from the owner: fall through to full federation so a
		// cross-store edge (a work bead blocked-by a graph root, recorded in the
		// Work store) is still found. Graph molecules are self-contained in
		// practice, so this fast-path returns on the first hit without the fork.
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
