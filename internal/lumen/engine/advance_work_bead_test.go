package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// fakeWorkStore is an in-memory stand-in for the composition root's DispatchWork /
// ObserveWork seams (REDESIGN §1.4). DispatchWork is lookup-then-create keyed on the
// activation (the idempotency the real seam gets from ListQuery.Metadata), so a
// re-Advance re-finds the SAME bead id. ObserveWork returns whatever terminal state
// the test scripts for a bead id.
type fakeWorkStore struct {
	mu         sync.Mutex
	seq        int
	byAct      map[string]string                 // activation -> bead id (idempotency)
	dispatches []engine.WorkDispatch             // every DispatchWork call, in order
	terminal   map[string]engine.WorkObservation // bead id -> terminal observation
	obsErr     map[string]error                  // bead id -> observer error
}

func newFakeWorkStore() *fakeWorkStore {
	return &fakeWorkStore{
		byAct:    map[string]string{},
		terminal: map[string]engine.WorkObservation{},
		obsErr:   map[string]error{},
	}
}

func (f *fakeWorkStore) dispatch(_ context.Context, w engine.WorkDispatch) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dispatches = append(f.dispatches, w)
	if id, ok := f.byAct[w.Activation]; ok {
		return id, nil // idempotent: the metadata lookup found the prior bead
	}
	f.seq++
	id := fmt.Sprintf("wb-%d", f.seq)
	f.byAct[w.Activation] = id
	return id, nil
}

func (f *fakeWorkStore) observe(_ context.Context, beadID string) (engine.WorkObservation, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.obsErr[beadID]; err != nil {
		return engine.WorkObservation{}, err
	}
	return f.terminal[beadID], nil // zero value = not terminal
}

// settle scripts a bead id's terminal observation. Retryable mirrors the cmd seam's
// contract (LumenFailRetryableForGCOutcome): a genuine worker fail-close is retryable,
// so the retry arm re-attempts it; pass/degraded are not. A bare/unknown-close case is
// scripted explicitly via settleObservation.
func (f *fakeWorkStore) settle(beadID, outcome, output string) {
	f.settleObservation(beadID, engine.WorkObservation{
		Terminal:  true,
		Outcome:   outcome,
		Output:    output,
		Retryable: outcome == engine.OutcomeFailed,
	})
}

// settleObservation scripts a bead id's exact terminal observation (for the
// non-retryable bare-close case that decouples Outcome from Retryable).
func (f *fakeWorkStore) settleObservation(beadID string, obs engine.WorkObservation) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.terminal[beadID] = obs
}

// settleAct scripts the terminal observation of the work bead dispatched for an
// activation. The settle lands on the next Advance (the observe arm), matching the
// controller loop; a t.Fatalf fires if the activation has not been dispatched yet.
func (f *fakeWorkStore) settleAct(t *testing.T, activation, outcome, output string) {
	t.Helper()
	f.mu.Lock()
	id, ok := f.byAct[activation]
	f.mu.Unlock()
	if !ok {
		t.Fatalf("settleAct: no dispatched work bead for activation %q", activation)
	}
	f.settle(id, outcome, output)
}

// settleAllDispatchedPass marks every dispatched-but-unsettled bead terminal(pass)
// — a "worker" closing whatever work has been dispatched so far. It is the settle
// racer for the concurrency tests.
func (f *fakeWorkStore) settleAllDispatchedPass() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, id := range f.byAct {
		if _, done := f.terminal[id]; !done {
			f.terminal[id] = engine.WorkObservation{Terminal: true, Outcome: engine.OutcomePass, Output: "done"}
		}
	}
}

func (f *fakeWorkStore) opts() engine.Options {
	return f.optsWith(advRouter)
}

// optsWith builds the engine.Options with a caller-supplied PoolRouter and this
// fake's dispatch/observe seams — for tests that route with a non-default router.
func (f *fakeWorkStore) optsWith(router func(string) (string, bool)) engine.Options {
	return engine.Options{PoolRouter: router, DispatchWork: f.dispatch, ObserveWork: f.observe}
}

func (f *fakeWorkStore) dispatchCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dispatches)
}

// TestAdvanceWorkBeadDispatchProjectsPlainStep is the real-bead dispatch spine
// (REDESIGN §1.4/§1.3): a ready pool-mode do dispatches an ORDINARY work bead (via
// the seam) and journals owned.admitted{work_bead}; the fold row becomes a PLAIN
// step with NO claimable frontier row and NO dispatch_mode/routed_to marker — so
// nothing double-claims it off Tier-A (TestPoolNodeNeverEntersFrontier folded in).
func TestAdvanceWorkBeadDispatchProjectsPlainStep(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	res, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Parked {
		t.Fatalf("advance = %+v, want Parked", res)
	}

	// The seam was called once with the do's coordinates.
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork called %d times, want 1", fake.dispatchCount())
	}
	w := fake.dispatches[0]
	if w.Activation != "hello:0" || w.NodeID != "hello" || w.Route != advPool || w.Prompt != "Say hello." || w.Attempt != 0 {
		t.Fatalf("WorkDispatch = %+v, want {hello:0, hello, %s, Say hello., attempt 0}", w, advPool)
	}

	// InFlight carries the dispatched bead id.
	if len(res.InFlight) != 1 || res.InFlight[0].BeadID != "wb-1" {
		t.Fatalf("InFlight = %+v, want one entry with BeadID wb-1", res.InFlight)
	}

	// Journal: run.started, node.activated, owned.admitted(work_bead) — no owned.settled.
	got := eventTypes(streamStored(t, store, streamID))
	want := []string{engine.EventRunStarted, engine.EventNodeActivated, engine.EventOwnedAdmitted}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %v, want %v", got, want)
	}

	// Projection is a PLAIN step, prompt NOT copied, and the claimable markers dropped.
	var beadType, description string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT bead_type, description FROM nodes WHERE id = 'hello' AND fold_owned = 1`,
	).Scan(&beadType, &description); err != nil {
		t.Fatalf("read projected node: %v", err)
	}
	if beadType != "step" {
		t.Errorf("bead_type = %q, want step (real bead lives in the work store)", beadType)
	}
	if description != "" {
		t.Errorf("description = %q, want empty (the real bead carries the prompt)", description)
	}
	if got := nodeMeta(t, store, "hello", "dispatch_mode"); got != "" {
		t.Errorf("dispatch_mode marker = %q, want dropped", got)
	}
	if got := nodeMeta(t, store, "hello", "bead_id"); got != "wb-1" {
		t.Errorf("bead_id meta = %q, want wb-1", got)
	}

	// No claimable frontier row — the real bead is the ONLY claim surface.
	if inFrontier(t, store, streamID, "hello:0") {
		t.Error("pool do left a claimable frontier row; the real bead should be the only claim surface")
	}
	var frontierCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frontier WHERE root_id = ? AND node_id = 'hello'`, streamID,
	).Scan(&frontierCount); err != nil {
		t.Fatalf("count frontier rows: %v", err)
	}
	if frontierCount != 0 {
		t.Errorf("frontier rows for 'hello' = %d, want 0", frontierCount)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestAdvanceDispatchIdempotent proves two Advances with no settlement dispatch the
// work bead exactly ONCE: the second pass no-ops on the recorded BeadID (HIGH-1).
func TestAdvanceDispatchIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	for i := 0; i < 3; i++ {
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork called %d times across 3 Advances, want 1", fake.dispatchCount())
	}
	admits := 0
	for _, tp := range eventTypes(streamStored(t, store, streamID)) {
		if tp == engine.EventOwnedAdmitted {
			admits++
		}
	}
	if admits != 1 {
		t.Fatalf("owned.admitted count = %d, want 1", admits)
	}
}

// TestAdvanceObserveSettlesAndSeals proves the observe arm: a still-open bead parks;
// a terminal close appends the EXISTING outcome.settled and seals the run.
func TestAdvanceObserveSettlesAndSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	// Pass 1: dispatch + park.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}

	// Pass 2: bead still open → still parked, no settle.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 (open bead) = %+v err %v, want Parked", r2, err)
	}
	if types := eventTypes(streamStored(t, store, streamID)); contains(types, engine.EventOutcomeSettled) {
		t.Fatalf("a still-open bead was settled; journal = %v", types)
	}

	// Close the bead; pass 3 observes → settles → seals.
	fake.settle("wb-1", engine.OutcomePass, "hi there")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	got := eventTypes(streamStored(t, store, streamID))
	want := []string{engine.EventRunStarted, engine.EventNodeActivated, engine.EventOwnedAdmitted, engine.EventOutcomeSettled, engine.EventRunClosed}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("journal = %v, want %v (owned.settled must NOT appear — the settle is outcome.settled)", got, want)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestAdvanceObserveErrorLeavesParked proves an observer error is surfaced (so the
// controller loop can log it) and does NOT settle the node — the run stays parked to
// retry next tick (§9.7).
func TestAdvanceObserveErrorLeavesParked(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.mu.Lock()
	fake.obsErr["wb-1"] = fmt.Errorf("store outage")
	fake.mu.Unlock()

	_, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err == nil {
		t.Fatal("observer error was swallowed; want it surfaced to the loop")
	}
	if types := eventTypes(streamStored(t, store, streamID)); contains(types, engine.EventOutcomeSettled) {
		t.Fatalf("an observer error auto-settled the node; journal = %v", types)
	}
}

// TestDoFailSettleIsRetryable proves the controller do-settle stamps
// outcome.settled Retryable = (outcome == failed) (REDESIGN §5), so the formula's
// retry arm re-attempts a genuine worker failure.
func TestDoFailSettleIsRetryable(t *testing.T) {
	for _, tc := range []struct {
		outcome       string
		wantRetryable bool
	}{
		{engine.OutcomeFailed, true},
		{engine.OutcomePass, false},
		{engine.OutcomeDegraded, false},
	} {
		t.Run(tc.outcome, func(t *testing.T) {
			ctx := context.Background()
			store := newStore(t)
			docJSON, streamID := doOnlyDoc()
			doc := decodeIR(t, docJSON)
			fake := newFakeWorkStore()
			if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
				t.Fatalf("advance 1: %v", err)
			}
			fake.settle("wb-1", tc.outcome, "")
			if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
				t.Fatalf("advance 2: %v", err)
			}
			if got := settledRetryable(t, streamStored(t, store, streamID), "hello:0"); got != tc.wantRetryable {
				t.Fatalf("outcome.settled retryable = %v, want %v", got, tc.wantRetryable)
			}
		})
	}
}

// TestAdvanceObserveContinuesSamePass proves a settle drives dependents in the SAME
// pass (REDESIGN §1.4): A→B (both pool dos); once A's bead closes, the observing pass
// settles A and dispatches B without waiting for a second tick.
func TestAdvanceObserveContinuesSamePass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-ab"
	doc := decodeIR(t, blockDoc("chain",
		doNode("A", "do A", nil),
		doNode("B", "do B", []string{"A"}),
	))
	fake := newFakeWorkStore()

	// Pass 1: A dispatched, B deferred.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	if fake.dispatchCount() != 1 || fake.dispatches[0].NodeID != "A" {
		t.Fatalf("dispatches after pass 1 = %+v, want only A", fake.dispatches)
	}

	// Close A; the next pass settles A AND dispatches B in one go.
	fake.settle("wb-1", engine.OutcomePass, "a-out")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 = %+v err %v, want Parked (now on B)", r2, err)
	}
	if fake.dispatchCount() != 2 || fake.dispatches[1].NodeID != "B" {
		t.Fatalf("dispatches after pass 2 = %+v, want A then B (same-pass continuation)", fake.dispatches)
	}
	if got := settledOutcomeByID(t, streamStored(t, store, streamID))["A"]; got != engine.OutcomePass {
		t.Fatalf("A settled %q, want pass", got)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "B" {
		t.Fatalf("InFlight after pass 2 = %+v, want B", r2.InFlight)
	}
}

// TestAdvanceCrashBetweenCreateAndDispatchFact pins the §9.1 window: a crash after
// the work bead is created but before the owned.admitted{work_bead} fact commits. A
// re-Advance re-looks-up (the seam returns the SAME id) and lands the dispatch fact —
// exactly ONE bead, exactly ONE owned.admitted.
func TestAdvanceCrashBetweenCreateAndDispatchFact(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	// Crash exactly at the after-dispatch boundary for hello:0.
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == "hello:0" {
			return fmt.Errorf("injected crash after dispatch")
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	// The bead was created, but the dispatch fact never committed.
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1", fake.dispatchCount())
	}
	if types := eventTypes(streamStored(t, store, streamID)); contains(types, engine.EventOwnedAdmitted) {
		t.Fatalf("owned.admitted committed before the crash boundary; journal = %v", types)
	}

	// Re-Advance: adopt the findable bead and land the dispatch fact.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("re-advance = %+v err %v, want Parked", r2, err)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork total calls = %d, want 2 (re-looked-up)", fake.dispatchCount())
	}
	// Exactly one bead minted (idempotent lookup returned the same id).
	fake.mu.Lock()
	beadCount := fake.seq
	fake.mu.Unlock()
	if beadCount != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 (lookup-before-create idempotency)", beadCount)
	}
	admits := 0
	for _, tp := range eventTypes(streamStored(t, store, streamID)) {
		if tp == engine.EventOwnedAdmitted {
			admits++
		}
	}
	if admits != 1 {
		t.Fatalf("owned.admitted count = %d, want exactly 1", admits)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].BeadID != "wb-1" {
		t.Fatalf("InFlight = %+v, want one entry BeadID wb-1", r2.InFlight)
	}
}

// TestRetryDoFreshBeadPerAttempt proves fresh-bead-per-attempt (REDESIGN §5): a
// failed do attempt re-attempts on a NEW activation with a DISTINCT work bead, and a
// passing later attempt seals the run pass.
func TestRetryDoFreshBeadPerAttempt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-retry-wb"
	body := doNode("draft", "Do the work.", nil)
	doc := decodeIR(t, blockDoc("adv-retry", retryNode(`{"kind":"literal","value":3}`, body)))
	fake := newFakeWorkStore()

	// Pass 1: dispatch attempt draft:0 (wb-1).
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "draft:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with draft:0", r1, err)
	}

	// Attempt 0 fails; pass 2 settles it AND mints a FRESH attempt draft:1 (wb-2).
	fake.settle("wb-1", engine.OutcomeFailed, "nope")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "draft:1" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with draft:1 (re-attempt)", r2, err)
	}

	// Attempt 1 passes; pass 3 settles the loop pass and seals.
	fake.settle("wb-2", engine.OutcomePass, "done")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r3.Run.Outcome)
	}

	// Two dispatches, two DISTINCT beads, per-attempt activations + attempt indices.
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (one per attempt)", fake.dispatchCount())
	}
	d0, d1 := fake.dispatches[0], fake.dispatches[1]
	if d0.Activation != "draft:0" || d0.Attempt != 0 {
		t.Errorf("attempt 0 dispatch = %+v, want draft:0 attempt 0", d0)
	}
	if d1.Activation != "draft:1" || d1.Attempt != 1 {
		t.Errorf("attempt 1 dispatch = %+v, want draft:1 attempt 1", d1)
	}
	fake.mu.Lock()
	id0, id1 := fake.byAct["draft:0"], fake.byAct["draft:1"]
	fake.mu.Unlock()
	if id0 == id1 || id0 == "" || id1 == "" {
		t.Fatalf("attempt bead ids not distinct: draft:0=%q draft:1=%q", id0, id1)
	}
	// Both attempt beads settled: draft:0 failed, draft:1 pass.
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["draft"] != engine.OutcomePass {
		// settledOutcomeByID keys on bare node id; the highest attempt (pass) wins.
		t.Fatalf("draft final settle = %q, want pass (highest attempt)", settled["draft"])
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestRetryDoBudgetExhaustionSealsFailed proves a retry whose every attempt fails
// exhausts its budget and seals failed (fresh bead each attempt).
func TestRetryDoBudgetExhaustionSealsFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-retry-exhaust"
	body := doNode("draft", "Do the work.", nil)
	doc := decodeIR(t, blockDoc("adv-retry-x", retryNode(`{"kind":"literal","value":2}`, body)))
	fake := newFakeWorkStore()

	// Drive: dispatch draft:0, fail; dispatch draft:1, fail; seal failed.
	steps := []struct {
		beadID  string
		wantAct string
	}{{"wb-1", "draft:0"}, {"wb-2", "draft:1"}}
	for i, s := range steps {
		r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil || !r.Parked || len(r.InFlight) != 1 || r.InFlight[0].Activation != s.wantAct {
			t.Fatalf("advance %d = %+v err %v, want Parked with %s", i, r, err, s.wantAct)
		}
		fake.settle(s.beadID, engine.OutcomeFailed, "nope")
	}
	rFinal, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !rFinal.Sealed {
		t.Fatalf("final advance = %+v err %v, want Sealed", rFinal, err)
	}
	if rFinal.Run.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed (budget exhausted)", rFinal.Run.Outcome)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (budget of 2 attempts)", fake.dispatchCount())
	}
	if _, reason, _, _ := loopSettle(t, rFinal.Run.Events, "attempt:0"); reason != "exhausted" {
		t.Errorf("loop settle reason = %q, want exhausted", reason)
	}
}

// dispatchPromptFor returns the prompt the seam was last dispatched with for an
// activation — the resolved bead Description the worker would receive.
func (f *fakeWorkStore) dispatchPromptFor(t *testing.T, activation string) string {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.dispatches) - 1; i >= 0; i-- {
		if f.dispatches[i].Activation == activation {
			return f.dispatches[i].Prompt
		}
	}
	t.Fatalf("no dispatch for activation %q", activation)
	return ""
}

// TestAdvanceObserveSeedsScopeForDownstream is the HIGH-2/3 value-plumbing proof:
// do A (worker outputs "aval") → do B (after A, prompt "use {{A}}"). When A's bead
// closes, the SAME observing pass must seed A's output into the interpolation scope
// so B's dispatched prompt renders "use aval" (NOT "use {{A}}"). It also pins
// DETERMINISM: the same-pass render EQUALS the crash-restart render — a crash after A
// settles but before B dispatches, then a fresh-driver re-Advance (whose scope is
// rebuilt from the fold via reconstructOutputs), must dispatch B with the identical
// resolved prompt. Before the fix the settle updated only the fold state and left the
// live scope stale, so the same-pass render diverged from the crash-restart render.
func TestAdvanceObserveSeedsScopeForDownstream(t *testing.T) {
	chainDoc := func() string {
		return blockDoc("chain",
			doNode("A", "produce", nil),
			doNode("B", "use {{A}}", []string{"A"}),
		)
	}

	// --- same-pass path: A closes, one Advance observes A AND dispatches B ---
	samePass := func() string {
		ctx := context.Background()
		store := newStore(t)
		doc := decodeIR(t, chainDoc())
		streamID := "gcg-run-vp-same"
		fake := newFakeWorkStore()
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("same-pass advance 1: %v", err)
		}
		fake.settle("wb-1", engine.OutcomePass, "aval") // A closes with output "aval"
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("same-pass advance 2: %v", err)
		}
		return fake.dispatchPromptFor(t, "B:0")
	}()

	// --- crash-restart path: crash after A settles, before B dispatches; fresh driver ---
	crashRestart := func() string {
		ctx := context.Background()
		store := newStore(t)
		doc := decodeIR(t, chainDoc())
		streamID := "gcg-run-vp-crash"
		fake := newFakeWorkStore()
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("crash advance 1: %v", err)
		}
		fake.settle("wb-1", engine.OutcomePass, "aval")
		// Crash right before B's node.activated: A observes+settles this pass, then B's
		// materialize hits crashBeforeActivate, so A is journaled but B is not dispatched.
		restore := engine.SetCrashHookForTest(func(b, _, act string) error {
			if b == engine.CrashBeforeActivate && act == "B:0" {
				return fmt.Errorf("injected crash before B activate")
			}
			return nil
		})
		_, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		restore()
		if err == nil {
			t.Fatal("crash advance 2 did not surface the injected crash")
		}
		// Fresh-driver re-Advance: scope is rebuilt from the fold (reconstructOutputs
		// seeds A's "aval"), then B is dispatched.
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("crash advance 3 (restart): %v", err)
		}
		return fake.dispatchPromptFor(t, "B:0")
	}()

	if samePass != "use aval" {
		t.Errorf("same-pass B prompt = %q, want \"use aval\" (A's output must seed scope in the observing pass)", samePass)
	}
	if samePass != crashRestart {
		t.Errorf("determinism hole: same-pass B prompt %q != crash-restart B prompt %q", samePass, crashRestart)
	}
}

// TestAdvanceConcurrentDriverFencedSingleDispatch is the MEDIUM-1 mutual-exclusion
// proof over one work store: a driver whose holder DIFFERS from the current lease
// holder is FENCED with ErrLeaseHeld and dispatches nothing, while the driver whose
// holder MATCHES re-acquires its OWN lease and dispatches exactly ONE bead — so two
// concurrent instances never both create a bead (double execution). The matching-holder
// dispatch is the assertion that fails before the fix: the old driver ignored
// Options.LeaseHolder and acquired under a constant holder, so it was FENCED by the
// prior lease and dispatched nothing (and two production controllers sharing that
// constant would instead both STEAL and double-dispatch).
func TestAdvanceConcurrentDriverFencedSingleDispatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	// Instance A holds the stream's writer lease (a driver mid-pass).
	if _, err := store.AcquireWriterLease(ctx, streamID, "inst-A", 30*time.Second); err != nil {
		t.Fatalf("acquire lease as inst-A: %v", err)
	}

	// A DISTINCT instance (inst-B) is FENCED while inst-A holds the lease: no dispatch.
	optsB := fake.opts()
	optsB.LeaseHolder = "inst-B"
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, optsB); !errors.Is(err, graphstore.ErrLeaseHeld) {
		t.Fatalf("distinct-holder driver Advance error = %v, want ErrLeaseHeld (fenced, not a steal)", err)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("fenced driver dispatched %d work beads, want 0 (it must never reach the create seam)", fake.dispatchCount())
	}

	// The SAME instance (inst-A) re-acquires its OWN lease and dispatches exactly one
	// bead — Options.LeaseHolder MUST be honored for the same-instance re-Advance path.
	optsA := fake.opts()
	optsA.LeaseHolder = "inst-A"
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, optsA); err != nil {
		t.Fatalf("matching-holder re-Advance = %v, want success (Options.LeaseHolder honored)", err)
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("matching-holder driver dispatched %d, want exactly 1 bead (single-instance mutual exclusion)", fake.dispatchCount())
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 across both drivers", minted)
	}
}

// TestAdvanceObserveBareCloseNonRetryable is the MEDIUM-2 engine-side proof: a
// non-retryable failed observation settles the do failed but WITHOUT the retryable
// flag, so a retry loop stops rather than re-running a bare-closed (possibly complete)
// do. (The seam maps a bare gc.outcome to failed+non-retryable — see the cmd seam
// test; here the fake scripts that observation directly.)
func TestAdvanceObserveBareCloseNonRetryable(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	// A bare/unknown close: failed outcome, but the seam reports it NON-retryable.
	fake.settleObservation("wb-1", engine.WorkObservation{Terminal: true, Outcome: engine.OutcomeFailed, Retryable: false})
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if got := settledRetryable(t, streamStored(t, store, streamID), "hello:0"); got {
		t.Fatalf("bare-close settle retryable = true, want false (a missing outcome is not a retryable strand)")
	}
}

// settledRetryable reads the retryable flag of the outcome.settled for an activation.
func settledRetryable(t *testing.T, events []graphstore.StoredEvent, activation string) bool {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Retryable  bool   `json:"retryable"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return p.Retryable
		}
	}
	t.Fatalf("no outcome.settled for %q", activation)
	return false
}
