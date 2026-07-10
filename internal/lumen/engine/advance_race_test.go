package engine_test

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// raceDriveToSeal runs a bounded concurrent race phase — a settle racer closing
// each dispatched bead while `drivers` re-entrant Advance goroutines hammer the SAME
// stream — then a DETERMINISTIC serial drain that seals the run without contention.
// The concurrent phase shakes the driver ⟷ driver writer-lease/CAS races (every
// Advance error must be retryable-typed); the serial finish guarantees termination
// (a sequential run under continuous multi-driver contention can otherwise livelock,
// every pass fenced before it completes). Returns once the run has sealed.
func raceDriveToSeal(t *testing.T, store *graphstore.Store, doc, streamID string, drivers int, retryable func(error) bool) {
	t.Helper()
	ctx := context.Background()
	d := decodeIR(t, doc)
	fake := newFakeWorkStore()

	// Prime the stream from ONE driver so the racers never contend the from-genesis
	// run.started seed (whose created_at timestamp is env-specific).
	if _, err := engine.Advance(ctx, store, d, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("prime advance: %v", err)
	}

	stop := make(chan struct{})
	var driversWg, settleWg sync.WaitGroup
	var mu sync.Mutex
	var raceErr error
	recordErr := func(err error) {
		mu.Lock()
		if raceErr == nil {
			raceErr = err
		}
		mu.Unlock()
	}

	// Settle racer (its own waitgroup): close every dispatched-but-open bead as it
	// appears, until the driver bursts finish and stop is closed.
	settleWg.Add(1)
	go func() {
		defer settleWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			fake.settleAllDispatchedPass()
			runtime.Gosched()
		}
	}()

	// Bounded driver racers: each does a fixed burst of re-entrant Advance passes,
	// then exits (so the concurrent phase always terminates).
	for i := 0; i < drivers; i++ {
		driversWg.Add(1)
		go func() {
			defer driversWg.Done()
			for pass := 0; pass < 200; pass++ {
				res, err := engine.Advance(ctx, store, d, streamID, nil, fake.opts())
				if err != nil {
					if !retryable(err) {
						recordErr(err)
						return
					}
				} else if res.Sealed {
					return
				}
				runtime.Gosched()
			}
		}()
	}
	driversWg.Wait() // the bounded bursts end

	mu.Lock()
	err := raceErr
	mu.Unlock()
	if err != nil {
		close(stop)
		settleWg.Wait()
		t.Fatalf("advance raced non-retryably: %v", err)
	}

	// Serial finish: a SINGLE driver, so no writer-lease contention and a full pass
	// always completes — but the settle racer stays LIVE, so the driver still observes
	// concurrently-closed beads across the run's remaining steps. This deterministically
	// seals a run the bounded bursts may have left mid-flight.
	sealed := false
	for attempt := 0; attempt < 400 && !sealed; attempt++ {
		fake.settleAllDispatchedPass()
		res, aerr := engine.Advance(ctx, store, d, streamID, nil, fake.opts())
		switch {
		case aerr != nil && retryable(aerr):
			runtime.Gosched()
		case aerr != nil:
			close(stop)
			settleWg.Wait()
			t.Fatalf("serial drain advance: %v", aerr)
		case res.Sealed:
			sealed = true
		default:
			runtime.Gosched()
		}
	}
	close(stop)
	settleWg.Wait()

	if !sealed {
		t.Fatal("run did not seal after the race")
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify after the race: %v", err)
	}
}

// TestAdvanceConcurrentDriversConverge is the multi-writer race suite for the
// real-bead path: several re-entrant Advance goroutines race the SAME multi-do
// stream while a "worker" goroutine closes each dispatched work bead. The only
// journal writer is the leased driver, so the surface under test is driver ⟷ driver
// (the writer-lease epoch fence), never a worker append. Every Advance error must be
// retryable-typed, the journal never corrupts (Verify clean), the run seals, and each
// do settles exactly once (write-once outcome.settled idem token). Run with
// -race -count=100 to shake fence/CAS/head races.
func TestAdvanceConcurrentDriversConverge(t *testing.T) {
	store := newStore(t)
	streamID := "gcg-run-race-multi"
	doc := blockDoc("race",
		doNode("a", "Do a.", nil),
		doNode("b", "Do b.", nil),
		doNode("c", "Do c.", nil),
	)
	raceDriveToSeal(t, store, doc, streamID, 4, isRetryableRaceErr)
	// Each do settled exactly once (write-once outcome.settled token): no duplicates.
	if n := countJournalType(t, store, streamID, engine.EventOutcomeSettled); n != 3 {
		t.Fatalf("outcome.settled count = %d, want 3 (one per do, write-once)", n)
	}
}

// TestAdvanceLoopRaceConcurrentSettles is the loop analog: a repeat/do formula
// (three sequential attempts) advanced by a single driver while a CONCURRENT settle
// racer closes each attempt's work bead. It pins the observe-vs-close race ACROSS a
// loop's sequential attempts — the driver must copy each concurrently-closed bead
// into outcome.settled exactly once (write-once per-attempt token), never double, the
// journal verifies, and the run seals. Concurrent DRIVERS are covered by the multi-do
// suite; a single driver here keeps the sequential attempt loop deterministic. Run
// with -race -count=100.
func TestAdvanceLoopRaceConcurrentSettles(t *testing.T) {
	store := newStore(t)
	streamID := "gcg-run-race-loop"
	// repeat until iteration >= 3 — three sequential do attempts.
	cond := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":3}]}`
	doc := blockDoc("race-loop", repeatNode(doNode("draft", "do {{iteration}}", nil), cond))
	raceDriveToSeal(t, store, doc, streamID, 0, isRetryableRaceErr)
	// Three attempt settles + the loop node's own settle, each write-once: no duplicates.
	if n := countJournalType(t, store, streamID, engine.EventOutcomeSettled); n != 4 {
		t.Fatalf("outcome.settled count = %d, want 4 (three attempts + the loop settle, write-once)", n)
	}
}

// isRetryableRaceErr reports whether err is a transient multi-writer race the
// controller loop retries rather than surfaces (an expected-version CAS loss, a
// lease fence, a busy store, or a Tier-A rebuild that raced a concurrent append).
func isRetryableRaceErr(err error) bool {
	return errors.Is(err, graphstore.ErrWrongExpectedVersion) ||
		errors.Is(err, graphstore.ErrLeaseFenced) ||
		errors.Is(err, graphstore.ErrBusy) ||
		errors.Is(err, graphstore.ErrRebuildRaced)
}
