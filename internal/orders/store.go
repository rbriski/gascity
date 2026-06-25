package orders

import "github.com/gastownhall/gascity/internal/beads"

// OrderStore is the persistence seam for order-dispatch tracking beads (the
// order-tracking / order-run:<scoped> records, NoHistory, that gate repeat order
// firing). It is the swap point for relocating order tracking off bd: the
// bd-delegating first implementation is any [beads.Store] (a faithful subset,
// proven below), and a future SQLite-backed store satisfies the same interface.
//
// NOTE the single-flight gate (hasOpenWorkStrict) and the canonical-list helpers
// are deliberately NOT expressed through this seam: they use
// beads.HandlesFor(store).Live and union multiple stores (tracking beads here +
// wisp roots in the graph store), so they stay on beads.Store.
//
// P1 surface: the tracking-bead lifecycle the dispatch path owns — Create (the
// tracking-bead create leg), recency-read (Get), outcome-stamp (Update), and
// close/close-batch (Close/CloseAll). The remaining design surface
// (List/ListByLabel recency + gate scans, DepList/DepRemove/Delete retention
// prune) folds in as the sweep paths are narrowed behind this seam at the orders
// SQLite cutover, never ahead of a consumer.
type OrderStore interface {
	Create(b beads.Bead) (beads.Bead, error)
	Get(id string) (beads.Bead, error)
	Update(id string, opts beads.UpdateOpts) error
	Close(id string) error
	CloseAll(ids []string, metadata map[string]string) (int, error)
}

// Compile-time proof that the bd-delegating first implementation of OrderStore is
// any beads.Store — introducing the seam is a no-op type narrowing, no wrapper.
var _ OrderStore = beads.Store(nil)
