package main

import (
	"io"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/sling"
	"github.com/gastownhall/gascity/internal/storeref"
)

func collectAttachedBeads(parent beads.Bead, store beads.Store, childQuerier BeadChildQuerier) ([]beads.Bead, error) {
	return sling.CollectAttachedBeads(parent, store, childQuerier)
}

func attachmentLabel(b beads.Bead) string {
	return sling.AttachmentLabel(b)
}

// autocloseGraphStore resolves the dedicated graph-class store for a cwd-rooted
// autoclose CLI entry point: the legacy <cityPath>/.gc/beads.sqlite store when the
// graph class is relocated (graph=sqlite/postgres), else the work store itself.
// Wisp/molecule attachments are ClassGraph, so a graph-relocated city must close
// their subtrees on this store rather than on the just-closed parent's work store.
// Byte-identical at graph=bd: resolveGraphStore returns workStore, so callers see
// the same store they passed and every route is a no-op.
func autocloseGraphStore(workStore beads.Store, cityPath string) beads.Store {
	cfg, err := loadCityConfig(cityPath, io.Discard)
	if err != nil {
		return workStore
	}
	return resolveGraphStore(workStore, cfg, cityPath, nil)
}

// autocloseGraphStoreArg normalizes the optional graph-store variadic argument the
// *With autoclose cores accept: the first non-nil entry, defaulting to store when
// omitted or nil. Keeps the many single-store unit tests and the byte-identical
// graph=bd path unchanged (graph == store -> every route collapses to store).
func autocloseGraphStoreArg(store beads.Store, graphStore []beads.Store) beads.Store {
	if len(graphStore) > 0 && graphStore[0] != nil {
		return graphStore[0]
	}
	return store
}

// autocloseStoreSet returns the resolution set for a class-agnostic by-id lookup:
// [store] when graph is not relocated (graph == store), else [store, graph]. The
// order is deliberate — the work store first — so storeref probes the common case
// before the graph store, matching the convoy seam (Phase GC).
func autocloseStoreSet(store, graph beads.Store) []beads.Store {
	if graph == nil || graph == store {
		return []beads.Store{store}
	}
	return []beads.Store{store, graph}
}

// autocloseOwningStore returns the store in storeSet that physically owns id (by
// id-prefix, then by probe), falling back to fallback when the set has no owner.
// This is the class-agnostic routing the autoclose cores use to send a graph-class
// subtree close to the graph store and a work-class subtree close to the work
// store, without statically knowing the bead's class. When storeSet is the single
// work store (graph=bd) it always returns that store — byte-identical.
func autocloseOwningStore(id string, storeSet []beads.Store, fallback beads.Store) beads.Store {
	if len(storeSet) <= 1 {
		return fallback
	}
	if owner := storeref.PrefixOwner(id, storeSet); owner != nil {
		return owner
	}
	for _, s := range storeSet {
		if s == nil {
			continue
		}
		if _, err := s.Get(id); err == nil {
			return s
		}
	}
	return fallback
}
