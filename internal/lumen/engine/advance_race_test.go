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

// TestAdvanceVsClaimSettleRace is the L2-exit multi-writer race suite: a re-entrant
// Advance loop over a multi-do stream racing concurrent cross-process claim/settle
// appends. The one genuinely new concurrency surface (driver ⟷ claimants) must be
// loud, never silent: every Advance error is retryable-typed (the loop simply
// re-runs), the journal never corrupts (graphstore.Verify clean), and the run
// eventually seals. Run with -race -count=100 to shake fence/CAS/head races.
func TestAdvanceVsClaimSettleRace(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-race-multi"
	// Three independent do's: all materialize on pass 1, then settle concurrently
	// while the driver re-Advances.
	doc := decodeIR(t, blockDoc("race",
		doNode("a", "Do a.", nil),
		doNode("b", "Do b.", nil),
		doNode("c", "Do c.", nil),
	))
	opts := engine.Options{PoolRouter: advRouter}

	// Pass 1: materialize all three (park).
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r1.Parked || len(r1.InFlight) != 3 {
		t.Fatalf("advance 1 = %+v, err %v; want Parked with 3 in flight", r1, err)
	}

	activations := []string{"a:0", "b:0", "c:0"}

	// Concurrently: N re-Advance passes race the three settle appends. Every append
	// and every Advance is CAS + write-once + epoch-fenced, so no interleaving may
	// silently corrupt the journal.
	var wg sync.WaitGroup

	// The settle racers (cross-process worker closes). A real worker retries a lost
	// CAS / lease fence (S17); mirror that so every do eventually settles once.
	for _, act := range activations {
		wg.Add(1)
		go func(act string) {
			defer wg.Done()
			for {
				err := engine.SettleTierBWork(ctx, store, streamID, act, engine.OutcomePass, "done")
				if err == nil {
					return
				}
				if !isRetryableRaceErr(err) {
					t.Errorf("settle %q errored non-retryably: %v", act, err)
					return
				}
				runtime.Gosched()
			}
		}(act)
	}

	// The driver racers (re-entrant Advance passes).
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
			if err != nil && !isRetryableRaceErr(err) {
				t.Errorf("advance raced non-retryably: %v", err)
			}
		}()
	}
	wg.Wait()

	// Drain to a seal: re-Advance until the run closes (a claim storm can make an
	// Advance keep losing the CAS, which is acceptable — the next pass converges).
	var sealed engine.AdvanceResult
	for attempt := 0; attempt < 50; attempt++ {
		res, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
		if err != nil {
			if isRetryableRaceErr(err) {
				continue
			}
			t.Fatalf("drain advance: %v", err)
		}
		if res.Sealed {
			sealed = res
			break
		}
	}
	if !sealed.Sealed {
		t.Fatal("run did not seal after the race")
	}
	if sealed.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", sealed.Run.Outcome)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify after the race: %v", err)
	}
	// Each do settled exactly once (write-once): no duplicate owned.settled.
	if n := countJournalType(t, store, streamID, engine.EventOwnedSettled); n != 3 {
		t.Fatalf("owned.settled count = %d, want 3 (one per do, write-once)", n)
	}
}

// TestAdvanceLoopRaceConcurrentSettles is the L5 loop analog of the multi-writer
// race suite: a re-entrant Advance loop over a repeat/do formula (three sequential
// attempts) racing a concurrent settle racer. The driver ⟷ claimant surface across
// ATTEMPTS must stay loud, never silent — every Advance error is retryable-typed,
// each attempt settles exactly once (write-once per-attempt token), the journal
// verifies, and the run seals. Run with -race -count=100.
func TestAdvanceLoopRaceConcurrentSettles(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-race-loop"
	// repeat until iteration >= 3 — three sequential do attempts.
	cond := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":3}]}`
	doc := decodeIR(t, blockDoc("race-loop",
		repeatNode(doNode("draft", "do {{iteration}}", nil), cond)))
	opts := engine.Options{PoolRouter: advRouter}

	// Prime: materialize attempt 0.
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("prime advance: %v", err)
	}

	done := make(chan struct{})
	var wg sync.WaitGroup
	var mu sync.Mutex
	var raceErr error

	// Settle racer: resolve the live draft attempt and settle it; a settle of an
	// already-settled / re-opened attempt races and is tolerated.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			ref, ok, err := engine.ResolveTierBWorkRef(ctx, store, "draft")
			if err != nil || !ok || ref.Settled || ref.Activation == "" {
				runtime.Gosched()
				continue
			}
			if err := engine.SettleTierBWork(ctx, store, streamID, ref.Activation, engine.OutcomePass, "ok"); err != nil && !isRetryableRaceErr(err) {
				runtime.Gosched()
			}
		}
	}()

	// Driver racers: concurrent re-Advance passes. A concurrent-mint transient
	// (ErrAdvanceStalled — one Advance minted the next attempt while another observed
	// the between-attempts window with nothing yet in its local view) is a benign
	// race the controller loop simply re-runs, so it is tolerated here alongside the
	// CAS/fence/conflict transients.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-done:
					return
				default:
				}
				res, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
				if err != nil {
					if isRetryableRaceErr(err) || errors.Is(err, engine.ErrAdvanceStalled) {
						runtime.Gosched()
						continue
					}
					mu.Lock()
					if raceErr == nil {
						raceErr = err
					}
					mu.Unlock()
					return
				}
				if res.Sealed {
					return
				}
				runtime.Gosched()
			}
		}()
	}

	// Drain to a seal.
	sealed := false
	for attempt := 0; attempt < 400; attempt++ {
		res, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
		if err != nil {
			if isRetryableRaceErr(err) || errors.Is(err, engine.ErrAdvanceStalled) {
				continue
			}
			t.Fatalf("drain advance: %v", err)
		}
		if res.Sealed {
			sealed = true
			break
		}
		runtime.Gosched()
	}
	close(done)
	wg.Wait()
	if raceErr != nil {
		t.Fatalf("advance raced non-retryably: %v", raceErr)
	}
	if !sealed {
		t.Fatal("loop run did not seal under the race")
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify after the loop race: %v", err)
	}
	// Three attempts, each settled exactly once (write-once per-attempt settle token).
	if n := countJournalType(t, store, streamID, engine.EventOwnedSettled); n != 3 {
		t.Fatalf("owned.settled count = %d, want 3 (one per attempt, write-once)", n)
	}
}

// isRetryableRaceErr reports whether err is a transient multi-writer race the
// controller loop retries rather than surfaces (an expected-version CAS loss, a
// lease fence, a busy store, or a Tier-B claim/settle conflict wrapping them).
func isRetryableRaceErr(err error) bool {
	return errors.Is(err, graphstore.ErrWrongExpectedVersion) ||
		errors.Is(err, graphstore.ErrLeaseFenced) ||
		errors.Is(err, graphstore.ErrBusy) ||
		errors.Is(err, graphstore.ErrRebuildRaced) ||
		errors.Is(err, engine.ErrTierBClaimConflict)
}
