package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// TestOrderDispatchSeamRoutesTrackingAndWispToDistinctStores is the load-bearing
// proof that the orders.OrderStore seam is real and not cosmetic. The production
// wiring resolves the same store for both roles (byte-identical bd phase); here
// we inject a DISTINCT order-tracking store and assert the dispatch path keeps the
// two ownership roles separate:
//
//   - the order-tracking bead is created and outcome-stamped ONLY in the order
//     store (the orders.OrderStore seam), never in the work store, and
//   - the wisp molecule is instantiated and labeled ONLY in the work store (the
//     graph beads.Store), never in the order store.
//
// This pins the routing the orders SQLite cutover relies on: tracking beads
// relocate onto the embedded SQLite order store while wisps stay on the work
// backend. If a future edit re-points a tracking write at the work store (or a
// wisp write at the order store) this test fails.
func TestOrderDispatchSeamRoutesTrackingAndWispToDistinctStores(t *testing.T) {
	dir := t.TempDir()
	// A var-free graph.v2 formula cooks and instantiates cleanly (unlike the
	// convoy fixture, which fails runtime-var validation before instantiation).
	formulaBody := `
formula = "order-seam-wisp"
version = 1
contract = "graph.v2"
type = "workflow"

[[steps]]
id = "step"
title = "Do work"
description = "Static work, no runtime vars"
`
	if err := os.WriteFile(filepath.Join(dir, "order-seam-wisp.formula.toml"), []byte(strings.TrimSpace(formulaBody)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	workStore := beads.NewMemStore()
	orderStore := beads.NewMemStore()

	ad := buildOrderDispatcherFromListExec([]orders.Order{{
		Name:         "seam-order",
		Trigger:      "cooldown",
		Interval:     "15m",
		Formula:      "order-seam-wisp",
		FormulaLayer: dir,
	}}, workStore, nil, nil, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	mad := ad.(*memoryOrderDispatcher)
	// Diverge the order-tracking store from the work store — the exact split the
	// SQLite cutover introduces.
	mad.orderStoreFn = func(beads.Store) orders.OrderStore { return orderStore }

	mad.dispatch(context.Background(), t.TempDir(), time.Now())
	mad.drain(context.Background())

	// Tracking bead: lives in the order store, absent from the work store.
	orderTracking := trackingBeads(t, orderStore, labelOrderTracking)
	if len(orderTracking) != 1 {
		t.Fatalf("order store %q beads = %d, want exactly the one tracking bead", labelOrderTracking, len(orderTracking))
	}
	if leaked := trackingBeads(t, workStore, labelOrderTracking); len(leaked) != 0 {
		t.Fatalf("work store %q beads = %d, want 0 — tracking beads must not leak to the work store", labelOrderTracking, len(leaked))
	}
	// The tracking bead carries the wisp outcome label, written through the order
	// store seam by dispatchWisp.
	if !slicesContain(orderTracking[0].Labels, "wisp") {
		t.Fatalf("tracking bead labels = %v, want the wisp outcome label", orderTracking[0].Labels)
	}

	// Wisp root: lives in the work store, absent from the order store. The root is
	// the order-run bead whose title is not the "order:" tracking title. drain()
	// above already guarantees dispatchOne has returned — which is strictly after
	// dispatchWisp wrote and labeled the wisp root — so a direct (non-polling)
	// lookup is correct here and makes a routing regression fail fast instead of
	// after a 2s poll deadline.
	var workRoot beads.Bead
	var foundRoot bool
	for _, b := range trackingBeads(t, workStore, "order-run:seam-order") {
		if !strings.HasPrefix(b.Title, "order:") {
			workRoot, foundRoot = b, true
			break
		}
	}
	if !foundRoot {
		t.Fatal("work store has no wisp root under order-run:seam-order — the wisp did not route to the work store")
	}
	if !isOrderWispRootCandidate(workRoot) {
		t.Fatalf("work store order-run bead %s (type=%q metadata=%v) is not a wisp root", workRoot.ID, workRoot.Type, workRoot.Metadata)
	}
	for _, b := range trackingBeads(t, orderStore, "order-run:seam-order") {
		if !strings.HasPrefix(b.Title, "order:") {
			t.Fatalf("order store contains a non-tracking order-run bead %s (title=%q) — the wisp leaked into the order store", b.ID, b.Title)
		}
	}
}
