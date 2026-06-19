package beads

// GraphOnlyListStore is an optional store capability: a per-class Router exposes
// List over its ClassGraph backend ALONE (the ClassWork/Dolt leg skipped), so the
// dispatcher's root-scoped scope-check List can avoid forking `bd` into Dolt for a
// molecule that is wholly graph-resident. It mirrors GraphOnlyReadyStore. The
// GraphIDPrefix accessor lets callers gate the fast path to graph-rooted queries.
// In the identity phase — no distinct ClassGraph backend — ListGraphOnly MUST fall
// back to the full federated List and GraphIDPrefix MUST return "" so a default
// Dolt-only city stays byte-identical.
type GraphOnlyListStore interface {
	ListGraphOnly(query ListQuery) ([]Bead, error)
	GraphIDPrefix() string
}

// GraphOnlyListProvider exposes a graph-only-list handle for wrappers whose
// capability depends on wrapped runtime state. A wrapper returns ok=false when its
// backing has no distinct ClassGraph backend, so capability presence gates the
// graph-only List path without a config lookup.
type GraphOnlyListProvider interface {
	ListGraphOnlyHandle() (GraphOnlyListStore, bool)
}

// GraphOnlyListFor returns the graph-only-list capability for store when one is
// available, walking wrapper delegation. It mirrors GraphOnlyReadyFor: a plain
// implementation is used directly, while a wrapper delegates through its handle
// without claiming the interface globally.
func GraphOnlyListFor(store Store) (GraphOnlyListStore, bool) {
	if store == nil {
		return nil, false
	}
	if provider, ok := store.(GraphOnlyListProvider); ok {
		return provider.ListGraphOnlyHandle()
	}
	if g, ok := store.(GraphOnlyListStore); ok {
		return g, true
	}
	return nil, false
}
