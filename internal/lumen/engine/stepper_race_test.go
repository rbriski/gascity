package engine_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// isStepperRetryable reports whether a concurrent Settle error is a transient contention
// loss the caller should retry: an expected-version CAS loss, a lease fence, a lease held
// by the other agent instance, a busy store, or a Tier-A rebuild race.
func isStepperRetryable(err error) bool {
	return isRetryableRaceErr(err) || errors.Is(err, graphstore.ErrLeaseHeld)
}

// TestStepperConcurrentSettleNoDoubleSettle is the -race double-drive pin: two agents (two
// distinct lease holders) race to Settle the SAME offered do. The writer lease + the
// expected-version CAS fence one writer at a time, and the outcome.settled idem token makes
// a duplicate settle a no-op — so the do settles EXACTLY once, no double-settle, and the
// journal verifies. Run with -race -count=100 to shake the fence/CAS/head races.
func TestStepperConcurrentSettleNoDoubleSettle(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)

	// Offer do a (single writer, no contention on the run.started seed's env-specific
	// created_at).
	first, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if first.NodeID != "a" {
		t.Fatalf("step = %+v, want do a", first)
	}

	// Two racers, each a distinct controller/agent instance (distinct lease holder), each
	// retrying its Settle until it lands (tolerating retryable lease/CAS fences), so both
	// terminate and the idempotent dedup is exercised under contention.
	var wg sync.WaitGroup
	var mu sync.Mutex
	var fatal error
	for i, holder := range []string{"agent-A", "agent-B"} {
		i, holder := i, holder
		wg.Add(1)
		go func() {
			defer wg.Done()
			for attempt := 0; attempt < 500; attempt++ {
				_, serr := engine.Settle(ctx, store, doc, streamID, nil, "a", engine.OutcomePass, "out-a",
					engine.Options{LeaseHolder: holder})
				if serr == nil {
					return
				}
				if !isStepperRetryable(serr) {
					mu.Lock()
					if fatal == nil {
						fatal = serr
					}
					mu.Unlock()
					return
				}
				runtime.Gosched()
			}
			mu.Lock()
			if fatal == nil {
				fatal = fmt.Errorf("racer %d (%s): settle never landed in 500 attempts", i, holder)
			}
			mu.Unlock()
		}()
	}
	wg.Wait()
	if fatal != nil {
		t.Fatalf("concurrent settle raced non-retryably: %v", fatal)
	}

	// Do a settled exactly once — the write-once outcome.settled idem token, never double.
	if n := countJournalType(t, store, streamID, engine.EventOutcomeSettled); n != 1 {
		t.Fatalf("outcome.settled count for a = %d, want 1 (no double-settle)", n)
	}
	if n := countJournalType(t, store, streamID, engine.EventEffectSettled); n != 1 {
		t.Fatalf("effect.settled count for a = %d, want 1 (no double-settle)", n)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify after the race: %v", err)
	}
}
