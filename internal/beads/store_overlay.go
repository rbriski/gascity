package beads

// StoreOverlay is an optional capability for a Store that composes another
// Store as one of its legs and fans its reads/writes out over that leg — so
// the overlay's own results already cover the composed store's beads. A caller
// that would otherwise iterate BOTH the overlay and the store it overlays uses
// this to visit each bead exactly once, avoiding duplicate projections and
// double-visit writes.
//
// The residence-routing graph store (cmd/gc) implements this: its global reads
// return legacy ∪ journal, so a projection/delete arm that also iterated the
// legacy (city) store separately would double-count every legacy-resident bead.
type StoreOverlay interface {
	// OverlaysStore reports whether other is a leg this store fans out over,
	// so iterating both this store and other would double-count other's beads.
	OverlaysStore(other Store) bool
}

// StoreOverlaps reports whether overlay composes base as one of its legs. It
// returns false when either store is nil or overlay does not implement
// StoreOverlay — the common single-store / disjoint-store case — so callers
// stay byte-identical when no overlay is in play.
func StoreOverlaps(overlay, base Store) bool {
	if overlay == nil || base == nil {
		return false
	}
	o, ok := overlay.(StoreOverlay)
	if !ok {
		return false
	}
	return o.OverlaysStore(base)
}
