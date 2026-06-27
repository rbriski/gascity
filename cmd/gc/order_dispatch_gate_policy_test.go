package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/orders"
)

// gateTimeoutStore makes the strict open-work gate scan (the
// `order-run:`-labeled, !IncludeClosed, Limit==0 List that hasOpenWorkStrict
// issues) block past the per-order gate timeout, reproducing the #2893 hang
// where storeHasOpenDescendants exceeds its budget under Dolt contention. Only
// that exact query shape is delayed; every other read stays fast.
type gateTimeoutStore struct {
	beads.Store
	delay time.Duration
}

func (s *gateTimeoutStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "order-run:") && !query.IncludeClosed && query.Limit == 0 {
		time.Sleep(s.delay)
	}
	return s.Store.List(query)
}

// TestOrderDispatchIdempotentFailsOpenOnGateTimeout is the #2893 #2'
// regression test: when the open-work gate exceeds its bound, an order marked
// idempotent must dispatch anyway (fail open) while a non-idempotent order
// must still be skipped (fail closed). Before the fix BOTH orders were skipped
// on gate timeout, starving the feeders fleet-wide.
func TestOrderDispatchIdempotentFailsOpenOnGateTimeout(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	store := &gateTimeoutStore{Store: beads.NewMemStore(), delay: 300 * time.Millisecond}
	now := time.Date(2026, 6, 4, 12, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		{Name: "unrouted-feeder", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: true},
		{Name: "merge-loop-sweep", Trigger: "cooldown", Interval: "1m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}
	ad.dispatch(context.Background(), t.TempDir(), now)
	ad.drain(context.Background())

	if got := trackingBeads(t, store, "order-run:unrouted-feeder"); len(got) == 0 {
		t.Error("idempotent order should fail OPEN on gate timeout and dispatch, but no tracking bead was created (order was skipped — the starvation regression)")
	}
	if got := trackingBeads(t, store, "order-run:merge-loop-sweep"); len(got) != 0 {
		t.Errorf("non-idempotent order should fail CLOSED on gate timeout and skip; got %d tracking beads", len(got))
	}
}

// TestGateFailClosed covers the gate-error decision logic directly: a per-order
// gate timeout fails open only for idempotent orders, but a done dispatch
// context (shutdown / tick deadline) always blocks, even for idempotent orders.
func TestGateFailClosed(t *testing.T) {
	m := &memoryOrderDispatcher{stderr: lockedStderr(&bytes.Buffer{})}
	gateErr := fmt.Errorf("open-work gate for x timed out: %w", errGateTimeout)

	if m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "feeder", gateErr) {
		t.Error("idempotent order on a live-context gate timeout should fail OPEN (not blocked)")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: false}, "sweep", gateErr) {
		t.Error("non-idempotent order on gate timeout should fail CLOSED (blocked)")
	}
	if !m.gateFailClosed(context.Background(), orders.Order{Idempotent: true}, "feeder", errors.New("dolt: read failed")) {
		t.Error("idempotent order must fail CLOSED on a non-timeout gate error (only the bounded-gate timeout fails open)")
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if !m.gateFailClosed(canceledCtx, orders.Order{Idempotent: true}, "feeder", gateErr) {
		t.Error("a canceled dispatch context must block even idempotent orders (no dispatch into a dead context)")
	}
}

// TestStoreHasOpenDescendantsSkipsTransientNotifications covers #2893 #3: a
// lingering open nudge/mail descendant must not keep the gate "open", but a real
// open work descendant still counts, and the nil-skip (sweeper) path keeps the
// original semantics where any open child counts.
func TestStoreHasOpenDescendantsSkipsTransientNotifications(t *testing.T) {
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{Title: "wisp root", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(beads.Bead{
		Title:    "nudge:abc",
		Type:     nudgeBeadType,
		Status:   "open",
		ParentID: root.ID,
		Labels:   []string{nudgeBeadLabel},
	}); err != nil {
		t.Fatal(err)
	}

	// Gate semantics (skip notifications): a lone open nudge does not block.
	if has, err := storeHasOpenDescendants(store, root.ID, isTransientNotificationBead); err != nil {
		t.Fatal(err)
	} else if has {
		t.Error("a lone open nudge descendant must NOT count as open work (#2893 #3)")
	}

	// Sweeper semantics (nil skip): the open nudge still counts.
	if has, err := storeHasOpenDescendants(store, root.ID, nil); err != nil {
		t.Fatal(err)
	} else if !has {
		t.Error("nil skip must preserve original semantics: any open child counts")
	}

	// A real open work descendant still blocks even with the skip predicate.
	if _, err := store.Create(beads.Bead{Title: "real work", Type: "task", Status: "open", ParentID: root.ID}); err != nil {
		t.Fatal(err)
	}
	if has, err := storeHasOpenDescendants(store, root.ID, isTransientNotificationBead); err != nil {
		t.Fatal(err)
	} else if !has {
		t.Error("a real open work descendant must still count as open work")
	}
}

// countingGateTimeoutStore wraps a Store and counts how many times the
// hasOpenWorkStrict query shape (order-run: prefix, not IncludeClosed, no
// Limit) is issued, while also introducing a configurable delay to force gate
// timeouts in tests.
type countingGateTimeoutStore struct {
	beads.Store
	delay     time.Duration
	gateCount atomic.Int32
}

func (s *countingGateTimeoutStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if strings.HasPrefix(query.Label, "order-run:") && !query.IncludeClosed && query.Limit == 0 {
		s.gateCount.Add(1)
		time.Sleep(s.delay)
	}
	return s.Store.List(query)
}

// TestOrderDispatchNonIdempotentBackoffOnGateTimeout is the #3688 regression
// test: when a non-idempotent order's open-work gate times out, the dispatcher
// must apply a temporary backoff (via rememberLastRun) so the same gate is not
// retried on the very next tick. Without the fix the order is still "due" on
// tick 2 and the 8-second gate runs again, thrashing dolt sql-server CPU.
func TestOrderDispatchNonIdempotentBackoffOnGateTimeout(t *testing.T) {
	prev := orderGateTimeout
	orderGateTimeout = 20 * time.Millisecond
	defer func() { orderGateTimeout = prev }()

	// Gate goroutine sleeps 50ms > 20ms timeout, so gateOpenWorkBounded times
	// out and leaves the goroutine behind. The goroutine finishes ~50ms later.
	store := &countingGateTimeoutStore{Store: beads.NewMemStore(), delay: 50 * time.Millisecond}
	now := time.Date(2026, 6, 23, 17, 0, 0, 0, time.UTC)

	aa := []orders.Order{
		// 5-minute cooldown, non-idempotent — the reported order shape from #3688.
		{Name: "cascade-nudge-on-blocker-close", Trigger: "cooldown", Interval: "5m", Exec: "true", Idempotent: false},
	}
	ad := buildOrderDispatcherFromListExec(aa, store, nil, successfulExec, nil)
	if ad == nil {
		t.Fatal("expected non-nil dispatcher")
	}

	// cityPath must be the same for both ticks: the rememberLastRun cache key
	// includes storeKey which is derived from cityPath, so different temp dirs
	// would produce different keys and the backoff wouldn't transfer.
	cityPath := t.TempDir()

	// Tick 1: gate times out → order skipped (fail-closed); backoff must be applied.
	ad.dispatch(context.Background(), cityPath, now)
	ad.drain(context.Background())
	if got := trackingBeads(t, store.Store, "order-run:cascade-nudge-on-blocker-close"); len(got) != 0 {
		t.Fatalf("tick 1: non-idempotent order must be skipped on gate timeout; got %d tracking beads", len(got))
	}

	// Wait for the orphaned gate goroutine from tick 1 to finish (it sleeps
	// 50ms; we wait 3× to be safe), then snapshot the gate-call count.
	time.Sleep(150 * time.Millisecond)
	countAfterTick1 := store.gateCount.Load()

	// Tick 2: dispatched immediately after (well within the 5m cooldown).
	// Without the fix the order is still "due" and the gate fires again.
	// With the fix the backoff keeps the order out of the due-check.
	ad.dispatch(context.Background(), cityPath, now.Add(time.Millisecond))
	ad.drain(context.Background())
	time.Sleep(150 * time.Millisecond) // let any tick-2 gate goroutine finish

	if got := store.gateCount.Load(); got != countAfterTick1 {
		t.Errorf("gate called again on tick 2 (backoff not applied): after tick 1 count=%d, after tick 2 count=%d; want no change",
			countAfterTick1, got)
	}
	if got := trackingBeads(t, store.Store, "order-run:cascade-nudge-on-blocker-close"); len(got) != 0 {
		t.Fatalf("tick 2: no tracking bead expected when order is backed off; got %d", len(got))
	}
}
