package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// The driver harness: a do-only (or multi-do) formula, a fixed pool route, and a
// stream id. Advance dispatches each ready pool-mode do as an ordinary work bead
// (via the fakeWorkStore seams) and PARKS; scripting the bead's terminal close
// (fake.settleAct) plus a re-Advance drives the DAG to run.closed — with NO real
// pool and NO controller loop.

const advPool = "pool-reviewers"

// advRouter routes every do to the test pool. A non-nil PoolRouter is what makes
// Advance treat do nodes as pool-mode.
func advRouter(string) (string, bool) { return advPool, true }

// doOnlyDoc is a single pool-mode do node, no dependencies.
func doOnlyDoc() (doc, streamID string) {
	return blockDoc("greet", doNode("hello", "Say hello.", nil)), "gcg-run-adv-doonly"
}

// TestAdvanceParksWithLiveLeaseAndInFlight proves the park mechanics: a do-only
// Advance emits the pool task, releases the lease, and reports the in-flight pool
// work — and that the driver's node.activated append carries the LIVE lease epoch
// (>= 1), never the permanently-fenced 0.
func TestAdvanceParksWithLiveLeaseAndInFlight(t *testing.T) {
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
		t.Fatalf("want Parked, got %+v", res)
	}
	if len(res.InFlight) != 1 {
		t.Fatalf("InFlight = %+v, want exactly one pool work", res.InFlight)
	}
	pw := res.InFlight[0]
	if pw.Activation != "hello:0" || pw.NodeID != "hello" || pw.Route != advPool || pw.Prompt != "Say hello." {
		t.Fatalf("InFlight[0] = %+v, want {hello:0, hello, %s, Say hello.}", pw, advPool)
	}
	if res.Head == 0 {
		t.Fatal("parked Head = 0, want the journal head as the level-trigger cursor")
	}

	// Live lease epoch on the driver's own append (the pool node.activated), not 0.
	epoch := leaseEpochOfType(t, store, streamID, engine.EventNodeActivated)
	if epoch < 1 {
		t.Fatalf("node.activated lease_epoch = %d, want >= 1 (live epoch, never 0)", epoch)
	}
	if cur, err := store.CurrentLeaseEpoch(ctx, streamID); err != nil || cur < 1 {
		t.Fatalf("CurrentLeaseEpoch = %d, err %v; want >= 1 (epoch preserved across park release)", cur, err)
	}

	// The lease was RELEASED at park: a different holder can acquire it (a held,
	// unexpired lease would return ErrLeaseHeld).
	if _, err := store.AcquireWriterLease(ctx, streamID, "some-other-holder", 30*time.Second); err != nil {
		t.Fatalf("acquire after park: %v — the driver did not release the lease at park", err)
	}
}

// TestAdvanceSettleThenAdvanceCloses is the L0 core loop: Advance a do-only
// formula (park), then a SCRIPTED owned.settled (as a pool worker's close would
// emit) plus a re-Advance seals the run pass — WITHOUT a real pool. It also proves
// the settle append carries the live lease epoch (correction #1): with the old
// hardcoded epoch 0, the settle would be permanently fenced on the driver-leased
// stream.
func TestAdvanceSettleThenAdvanceCloses(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()
	opts := fake.opts()

	first, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !first.Parked {
		t.Fatalf("first advance = %+v, err %v; want Parked", first, err)
	}

	// The pool worker's close: script the dispatched bead terminal. The next Advance
	// observes it and appends the outcome.settled — which carries the LIVE lease
	// epoch (the parked driver's preserved epoch); a hardcoded 0 would be fenced.
	fake.settleAct(t, "hello:0", engine.OutcomePass, "hi there")

	second, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("second advance: %v", err)
	}
	if !second.Sealed || second.Parked {
		t.Fatalf("second advance = %+v, want Sealed", second)
	}
	if epoch := leaseEpochOfType(t, store, streamID, engine.EventOutcomeSettled); epoch < 1 {
		t.Fatalf("outcome.settled lease_epoch = %d, want >= 1 (live epoch, not 0)", epoch)
	}
	if second.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", second.Run.Outcome)
	}
	// run.closed is the last journal fact; the projected root is done.
	if last := lastEventType(second.Run.Events); last != engine.EventRunClosed {
		t.Fatalf("last event = %q, want run.closed", last)
	}
	if st := nodeStatus(t, store, streamID); st != "done" {
		t.Fatalf("root status = %q, want done", st)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	// A third Advance on a sealed stream is an idempotent no-op read.
	third, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !third.Sealed || third.Run.Outcome != engine.OutcomePass {
		t.Fatalf("third advance = %+v, err %v; want idempotent Sealed pass", third, err)
	}
}

// TestAdvanceIsIdempotentWhenParked proves a re-Advance with NO new settlement
// makes no progress: it re-offers the SAME pool task (deduped on the journal — no
// second node.activated), the head does not move, and it parks again. Advance
// after a settlement makes progress; without one it is a pure no-op.
func TestAdvanceIsIdempotentWhenParked(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()
	opts := fake.opts()

	first, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !first.Parked {
		t.Fatalf("first advance = %+v, err %v; want Parked", first, err)
	}
	headAfterFirst := first.Head
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 1 {
		t.Fatalf("node.activated count after first advance = %d, want 1", n)
	}

	second, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("idempotent re-advance: %v", err)
	}
	if !second.Parked {
		t.Fatalf("re-advance without a settlement = %+v, want Parked (no progress)", second)
	}
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 1 {
		t.Fatalf("node.activated count after re-advance = %d, want 1 (no duplicate emit)", n)
	}
	if second.Head != headAfterFirst {
		t.Fatalf("head moved on a no-progress re-advance: %d -> %d", headAfterFirst, second.Head)
	}
	if !reflect.DeepEqual(second.InFlight, first.InFlight) {
		t.Fatalf("in-flight set changed across an idempotent re-advance: %+v -> %+v", first.InFlight, second.InFlight)
	}
}

// TestAdvanceMultiDoDAGConverges walks a two-do DAG (A -> B, B after A) step by
// step with scripted settlements and proves the FOLD drives the DAG: B is not
// materialized until A settles, B's prompt interpolates A's output across the pool
// boundary, and the run converges to run.closed.
func TestAdvanceMultiDoDAGConverges(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-multi"
	doc := decodeIR(t, blockDoc("chain",
		doNode("A", "Produce a value.", nil),
		doNode("B", "Refine {{A}}.", []string{"A"}),
	))
	fake := newFakeWorkStore()
	opts := fake.opts()

	// Pass 1: A materialized, B deferred.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v, err %v; want Parked on A", r1, err)
	}
	if len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "A" {
		t.Fatalf("advance 1 in-flight = %+v, want only A", r1.InFlight)
	}
	if countJournalType(t, store, streamID, engine.EventNodeActivated) != 1 {
		t.Fatal("B was materialized before A settled (fold did not gate the DAG)")
	}

	// A settles; B becomes ready and materializes with A's output interpolated.
	if err := engine.SettleWorkForTest(ctx, store, streamID, "A:0", engine.OutcomePass, "raw-value"); err != nil {
		t.Fatalf("settle A: %v", err)
	}

	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 = %+v, err %v; want Parked on B", r2, err)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "B" {
		t.Fatalf("advance 2 in-flight = %+v, want only B", r2.InFlight)
	}
	if r2.InFlight[0].Prompt != "Refine raw-value." {
		t.Fatalf("B prompt = %q, want A's output interpolated across the pool boundary", r2.InFlight[0].Prompt)
	}

	// B settles; the run seals.
	if err := engine.SettleWorkForTest(ctx, store, streamID, "B:0", engine.OutcomePass, "refined"); err != nil {
		t.Fatalf("settle B: %v", err)
	}
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v, err %v; want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAdvanceFailedPoolSettleSkipCascades proves an owned.settled{failed} drives
// the skip-cascade through the fold exactly like an engine outcome: a failed pool
// A never lets its dependent B become ready — B is settled skipped and the run
// fails.
func TestAdvanceFailedPoolSettleSkipCascades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-skip"
	doc := decodeIR(t, blockDoc("chain",
		doNode("A", "Do A.", nil),
		doNode("B", "Do B after {{A}}.", []string{"A"}),
	))
	fake := newFakeWorkStore()
	opts := fake.opts()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if err := engine.SettleWorkForTest(ctx, store, streamID, "A:0", engine.OutcomeFailed, "boom"); err != nil {
		t.Fatalf("settle A failed: %v", err)
	}
	res, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance after failed dependency = %+v, want Sealed (B skip-cascaded, no pool work)", res)
	}
	if res.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("run outcome = %q, want failed", res.Run.Outcome)
	}
	// B skip-cascaded: it settled skipped and was NEVER offered as claimable pool
	// work (no gc.routed_to, no pool bead_type — a doomed activation is not claimable).
	var bType, bStatus string
	if err := store.DB().QueryRowContext(ctx,
		`SELECT bead_type, status FROM nodes WHERE id = 'B' AND fold_owned = 1`).Scan(&bType, &bStatus); err != nil {
		t.Fatalf("read B: %v", err)
	}
	if bStatus != "skipped" {
		t.Fatalf("B status = %q, want skipped", bStatus)
	}
	if bType == "task" {
		t.Fatalf("B bead_type = task — a skip-cascaded pool do must NOT be offered as claimable work")
	}
	if got := nodeMeta(t, store, "B", beadmeta.RoutedToMetadataKey); got != "" {
		t.Fatalf("B gc.routed_to = %q, want empty (never offered to a pool)", got)
	}
	// No pool work was ever left in flight.
	if len(res.InFlight) != 0 {
		t.Fatalf("InFlight = %+v, want empty (B skip-cascaded)", res.InFlight)
	}
}

// TestAdvanceDropRefoldIdentityPoolRows is DET-T-17 over driver-materialized
// pool rows: the live projection equals a from-scratch drop+refold, and the sum
// of the incremental deltas equals the full-state ProjectDelta — proving the new
// route/prompt/task-type fields carry no state a refold misses. Checked both while
// parked (open claimable row) and after seal (settled row).
func TestAdvanceDropRefoldIdentityPoolRows(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()
	opts := fake.opts()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance: %v", err)
	}
	assertProjectionEqualsRefold(t, store, streamID)
	assertIncrementalEqualsProjectDelta(t, store, streamID)

	if err := engine.SettleWorkForTest(ctx, store, streamID, "hello:0", engine.OutcomePass, "done"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance to seal: %v", err)
	}
	assertProjectionEqualsRefold(t, store, streamID)
	assertIncrementalEqualsProjectDelta(t, store, streamID)
}

// TestAdvanceCrashMidAdvanceConverges is the crash-harness diagonal over the park
// boundary: a crash before the pool node.activated append (nothing materialized),
// re-Advance materializes + parks; then a crash before run.closed (work done, not
// sealed), re-Advance seals. Advance re-derives only the missing facts.
func TestAdvanceCrashMidAdvanceConverges(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()
	opts := fake.opts()

	// Crash 1: before materializing the pool do.
	sentinel := errors.New("crash before materialize")
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashBeforeActivate && act == "hello:0" {
			return sentinel
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	restore()
	if !errors.Is(err, sentinel) {
		t.Fatalf("advance under crash-before-activate = %v, want the sentinel", err)
	}
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 0 {
		t.Fatalf("node.activated committed despite crash before activate: %d", n)
	}

	// Re-Advance (no hook): materialize + park.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r1.Parked {
		t.Fatalf("re-advance = %+v, err %v; want Parked", r1, err)
	}

	if err := engine.SettleWorkForTest(ctx, store, streamID, "hello:0", engine.OutcomePass, "ok"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Crash 2: after the work is done, before run.closed.
	restore = engine.SetCrashHookForTest(func(b, _, _ string) error {
		if b == engine.CrashBeforeRunClosed {
			return sentinel
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, streamID, nil, opts)
	restore()
	if !errors.Is(err, sentinel) {
		t.Fatalf("advance under crash-before-run-closed = %v, want the sentinel", err)
	}
	if n := countJournalType(t, store, streamID, engine.EventRunClosed); n != 0 {
		t.Fatalf("run.closed committed despite crash before it: %d", n)
	}

	// Re-Advance: seal. The run converged across two crashes.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v, err %v; want Sealed pass", r2, err)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAdvanceEqualsRunForEngineFormula is the coexistence pin: an engine-only
// (exec) formula driven to completion by a single Advance produces the SAME
// journal event-type sequence, the same per-node settled outcomes, and the same
// terminal outcome as the synchronous Run — and Run itself is unchanged (it still
// runs every do inline and ignores PoolRouter). No pool router; every unit is
// engine-inline and settles in one parking pass, so Advance never parks.
func TestAdvanceEqualsRunForEngineFormula(t *testing.T) {
	ctx := context.Background()
	docJSON := blockDoc("pipe",
		execNode("A", "echo a", nil),
		execNode("B", "echo b", []string{"A"}),
	)

	runStore := newStore(t)
	runRes, err := engine.Run(ctx, runStore, decodeIR(t, docJSON), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	advStore := newStore(t)
	advRes, err := engine.Advance(ctx, advStore, decodeIR(t, docJSON), "gcg-run-adv-engine", nil, engine.Options{})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !advRes.Sealed || advRes.Parked {
		t.Fatalf("engine-only advance = %+v, want Sealed in one pass (no pool)", advRes)
	}

	if a, b := eventTypes(runRes.Events), eventTypes(advRes.Run.Events); !reflect.DeepEqual(a, b) {
		t.Fatalf("event-type sequence differs:\n run     = %v\n advance = %v", a, b)
	}
	if a, b := settledIDs(t, runRes.Events), settledIDs(t, advRes.Run.Events); !reflect.DeepEqual(a, b) {
		t.Fatalf("settled (node, outcome) differs:\n run     = %v\n advance = %v", a, b)
	}
	if runRes.Outcome != advRes.Run.Outcome {
		t.Fatalf("outcome: run %q != advance %q", runRes.Outcome, advRes.Run.Outcome)
	}
}

// TestAdvancePoolRepeatFailThenPass (T-A1) is the pool loop spine: Advance
// dispatches attempt draft:0 (open, fresh bead) and parks; a scripted failed close
// plus a re-Advance mints draft:1 (attempt.minted, node.activated, a fresh work
// bead); a scripted pass close plus a re-Advance settles the loop pass and seals.
func TestAdvancePoolRepeatFailThenPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-repeat"
	body := doNode("draft", "Do the work.", nil)
	loop := repeatNode(body, condOutcomePassOrIter())
	doc := decodeIR(t, blockDoc("adv-repeat", loop))
	fake := newFakeWorkStore()
	opts := fake.opts()

	// Pass 1: dispatch attempt draft:0, park.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "draft:0" {
		t.Fatalf("advance 1 = %+v, err %v; want Parked with draft:0 in flight", r1, err)
	}
	if r1.InFlight[0].Route != advPool {
		t.Fatalf("draft:0 route = %q, want %q", r1.InFlight[0].Route, advPool)
	}
	if st := nodeStatus(t, store, "draft"); st != "open" {
		t.Fatalf("draft status after mint = %q, want open", st)
	}

	// Attempt 0 fails; a re-Advance re-attempts.
	fake.settleAct(t, "draft:0", engine.OutcomeFailed, "nope")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "draft:1" {
		t.Fatalf("advance 2 = %+v, err %v; want Parked with draft:1 in flight (re-attempt)", r2, err)
	}
	events := streamStored(t, store, streamID)
	if n := countAttemptMinted(events); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2 (draft:0 then draft:1)", n)
	}
	if st := nodeStatus(t, store, "draft"); st != "open" {
		t.Fatalf("draft status after re-attempt = %q, want open (re-opened)", st)
	}
	// Per-attempt activations: draft:0 and draft:1 have distinct node.activated idem
	// tokens (a fresh work bead is dispatched per attempt).
	tokens := journalIdemTokensAdv(t, store, streamID)
	for _, want := range []string{streamID + ":draft:0:act", streamID + ":draft:1:act"} {
		if !advContains(tokens, want) {
			t.Fatalf("journal tokens %v missing %q (per-attempt activation token)", tokens, want)
		}
	}
	// A distinct work bead was dispatched per attempt.
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (one bead per attempt)", fake.dispatchCount())
	}

	// Attempt 1 passes; a re-Advance settles the loop pass and seals.
	fake.settleAct(t, "draft:1", engine.OutcomePass, "done")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v, err %v; want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	if outcome, _, _, _ := loopSettle(t, r3.Run.Events, "repeat_1:0"); outcome != "pass" {
		t.Fatalf("loop settle = %q, want pass", outcome)
	}
	// Both attempts settled under DISTINCT per-attempt outcome.settled tokens.
	final := journalIdemTokensAdv(t, store, streamID)
	for _, want := range []string{streamID + ":draft:0:settled", streamID + ":draft:1:settled"} {
		if !advContains(final, want) {
			t.Fatalf("journal tokens %v missing %q (per-attempt settle token)", final, want)
		}
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAdvanceIdempotentMidLoopNoDoubleMint (T-A2) proves re-entrancy: after draft:0
// settles failed, three Advances with NO new settlement mint EXACTLY ONE draft:1
// (one node.activated, one attempt.minted) and leave the head stable.
func TestAdvanceIdempotentMidLoopNoDoubleMint(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-idem"
	body := doNode("draft", "Do it.", nil)
	loop := repeatNode(body, condOutcomePassOrIter())
	doc := decodeIR(t, blockDoc("adv-idem", loop))
	fake := newFakeWorkStore()
	opts := fake.opts()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if err := engine.SettleWorkForTest(ctx, store, streamID, "draft:0", engine.OutcomeFailed, "nope"); err != nil {
		t.Fatalf("settle draft:0: %v", err)
	}
	var lastHead uint64
	for i := 0; i < 3; i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
		if err != nil {
			t.Fatalf("advance %d: %v", i+2, err)
		}
		if i > 0 && r.Head != lastHead {
			t.Fatalf("head moved on a no-settlement re-Advance: %d -> %d", lastHead, r.Head)
		}
		lastHead = r.Head
	}
	events := streamStored(t, store, streamID)
	if n := countActivationActivated(t, events, "draft:1"); n != 1 {
		t.Fatalf("draft:1 node.activated count = %d, want exactly 1 (no double-mint)", n)
	}
	if n := countAttemptMinted(events); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2 (draft:0, draft:1 — no extra)", n)
	}
}

// TestAdvanceEqualsRunForEngineLoopFormula (T-A3) is the loop oracle-parity pin: an
// EXEC-bodied repeat driven through Run and through repeated Advance yields the same
// journal type sequence and settled (node, outcome) — Advance runs an exec-bodied
// loop inline in one pass, exactly like Run.
func TestAdvanceEqualsRunForEngineLoopFormula(t *testing.T) {
	ctx := context.Background()
	mkDoc := func(t *testing.T) string {
		return blockDoc("loop-parity",
			repeatNode(execNodeExit("draft", flakyExec(t), []int{0}, nil), condOutcomePassOrIter()),
		)
	}
	runStore := newStore(t)
	runRes, err := engine.Run(ctx, runStore, decodeIR(t, mkDoc(t)), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	advStore := newStore(t)
	advRes, err := engine.Advance(ctx, advStore, decodeIR(t, mkDoc(t)), "gcg-run-adv-loopparity", nil, engine.Options{})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !advRes.Sealed || advRes.Parked {
		t.Fatalf("engine-only loop advance = %+v, want Sealed in one pass", advRes)
	}
	if a, b := eventTypes(runRes.Events), eventTypes(advRes.Run.Events); !reflect.DeepEqual(a, b) {
		t.Fatalf("event-type sequence differs:\n run     = %v\n advance = %v", a, b)
	}
	if a, b := settledIDs(t, runRes.Events), settledIDs(t, advRes.Run.Events); !reflect.DeepEqual(a, b) {
		t.Fatalf("settled (node, outcome) differs:\n run     = %v\n advance = %v", a, b)
	}
	if runRes.Outcome != advRes.Run.Outcome {
		t.Fatalf("outcome: run %q != advance %q", runRes.Outcome, advRes.Run.Outcome)
	}
}

// countActivationActivated counts node.activated events for a specific activation.
func countActivationActivated(t *testing.T, events []graphstore.StoredEvent, activation string) int {
	t.Helper()
	n := 0
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if p.Activation == activation {
			n++
		}
	}
	return n
}

// journalIdemTokensAdv reads a stream's idem tokens in seq order.
func journalIdemTokensAdv(t *testing.T, store *graphstore.Store, streamID string) []string {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT idem_token FROM journal WHERE stream_id = ? ORDER BY seq`, streamID)
	if err != nil {
		t.Fatalf("query idem tokens: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var tok string
		if err := rows.Scan(&tok); err != nil {
			t.Fatalf("scan idem token: %v", err)
		}
		out = append(out, tok)
	}
	return out
}

func advContains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// streamStored reads a stream's committed events (StoredEvent view) from seq 1.
func streamStored(t *testing.T, store *graphstore.Store, streamID string) []graphstore.StoredEvent {
	t.Helper()
	events, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream %s: %v", streamID, err)
	}
	return events
}

// TestAdvanceEmitsOnlyLumenVocabZeroControlBeads is the §1.2 pin: an Advanced
// pool run's journal is ENTIRELY Lumen vocabulary, and no projected node carries a
// gc.kind control-bead marker — the v2 control dispatcher cannot see any of it.
func TestAdvanceEmitsOnlyLumenVocabZeroControlBeads(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	fake := newFakeWorkStore()
	opts := fake.opts()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := engine.SettleWorkForTest(ctx, store, streamID, "hello:0", engine.OutcomePass, "ok"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	res, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !res.Sealed {
		t.Fatalf("advance to seal = %+v, err %v", res, err)
	}

	known := map[string]bool{}
	for _, tp := range engine.EventTypes {
		known[tp] = true
	}
	for _, e := range res.Run.Events {
		if !known[e.Type] {
			t.Fatalf("journal carries non-Lumen event type %q", e.Type)
		}
	}
	// No gc.kind control metadata anywhere in the projection.
	var gcKind int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM node_metadata WHERE key = 'gc.kind'`).Scan(&gcKind); err != nil {
		t.Fatalf("count gc.kind: %v", err)
	}
	if gcKind != 0 {
		t.Fatalf("found %d gc.kind control-bead markers, want 0 (zero control beads)", gcKind)
	}
}

// TestAdvanceParallelUndeclaredRefNoWedge is the HIGH-1 repro: two parallel
// pool do's where B's prompt {{A}} refs A WITHOUT declaring `after A`. Both
// materialize on pass 1 and B's prompt renders with A unresolved. After A settles,
// a re-Advance MUST NOT re-render B's prompt: a re-render would now interpolate A's
// output, and re-offering the SAME write-once activation idem token with that
// divergent payload trips ErrIdemTokenReuse and permanently wedges the driver.
// The first-rendered prompt stands, the re-Advance no-ops for the in-flight B, and
// the run converges when both settle — and re-Advance errors are all retryable.
func TestAdvanceParallelUndeclaredRefNoWedge(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-undeclared"
	doc := decodeIR(t, blockDoc("parallel",
		doNode("A", "Produce a value.", nil),
		doNode("B", "Use {{A}}.", nil), // NO declared `after A` — an undeclared ref
	))
	fake := newFakeWorkStore()
	opts := fake.opts()

	// Pass 1: both materialize (both have empty afterDeps); B renders A unresolved.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v, err %v; want Parked", r1, err)
	}
	if len(r1.InFlight) != 2 {
		t.Fatalf("advance 1 in-flight = %+v, want A and B in flight", r1.InFlight)
	}
	if got := inFlightPrompt(r1.InFlight, "B"); got != "Use {{A}}." {
		t.Fatalf("B first-render prompt = %q, want the render with A unresolved", got)
	}

	// A settles with an output that WOULD change B's prompt if re-rendered.
	if err := engine.SettleWorkForTest(ctx, store, streamID, "A:0", engine.OutcomePass, "APPLE"); err != nil {
		t.Fatalf("settle A: %v", err)
	}

	// Re-Advance: MUST NOT wedge. B is already materialized+unsettled → no-op.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("re-advance after A settled WEDGED the driver (ErrIdemTokenReuse regression): %v", err)
	}
	if !r2.Parked {
		t.Fatalf("advance 2 = %+v, want Parked on B", r2)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "B" {
		t.Fatalf("advance 2 in-flight = %+v, want only B", r2.InFlight)
	}
	if r2.InFlight[0].Prompt != "Use {{A}}." {
		t.Fatalf("B prompt after re-advance = %q, want the UNCHANGED first render", r2.InFlight[0].Prompt)
	}
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 2 {
		t.Fatalf("node.activated count = %d, want 2 (A and B, no re-emit)", n)
	}

	// B settles; the run seals.
	if err := engine.SettleWorkForTest(ctx, store, streamID, "B:0", engine.OutcomePass, "used"); err != nil {
		t.Fatalf("settle B: %v", err)
	}
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v, err %v; want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAdvanceScatterPoolMembersFanOutAndAggregate (T-U2, L4 scenario 3) pins the
// scatter-of-pool-do's fan-out no test drove before: a 3-do-member scatter
// materializes ALL members as claimable pool work in ONE Advance pass (each a
// task-typed row carrying the route + a frontier row), while the aggregate parks on
// its member drain deps and is NOT activated. Once the members settle, one re-Advance
// settles the aggregate from their outcomes and seals the run. The degraded subtest
// pins the mixed-outcome aggregate under on_fail continue, and the drop+refold
// identity proves the fan-out projection carries no state a refold misses.
func TestAdvanceScatterPoolMembersFanOutAndAggregate(t *testing.T) {
	scatterOfDos := func(onFail string) string {
		return blockDoc("fan",
			scatterNode("lanes", nil, onFail,
				doNode("one", "Do lane one.", nil),
				doNode("two", "Do lane two.", nil),
				doNode("three", "Do lane three.", nil),
			),
		)
	}
	t.Run("all_members_pass_aggregate_passes", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		streamID := "gcg-run-adv-scatter-fanout"
		doc := decodeIR(t, scatterOfDos("continue"))
		fake := newFakeWorkStore()

		// Pass 1: all three do members dispatch as ordinary work beads; the aggregate
		// parks on its member drain deps and is NOT activated.
		r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance 1: %v", err)
		}
		if !r1.Parked || r1.Sealed {
			t.Fatalf("advance 1 = %+v, want Parked (members await the pool)", r1)
		}
		if got := inFlightNodeIDs(r1.InFlight); !equalStringSet(got, []string{"one", "two", "three"}) {
			t.Fatalf("in-flight members = %v, want {one,two,three} (all three fan out in one pass)", got)
		}
		if inFlightHas(r1.InFlight, "lanes") {
			t.Fatalf("scatter aggregate 'lanes' was offered as pool work; want it parked on member drain")
		}
		if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 3 {
			t.Fatalf("node.activated count after pass 1 = %d, want 3 (members only; aggregate not yet activated)", n)
		}
		// Each member dispatched exactly one ordinary work bead and projects a plain
		// step (the real work bead is the claim surface, not this fold row).
		if fake.dispatchCount() != 3 {
			t.Fatalf("DispatchWork calls = %d, want 3 (one per member)", fake.dispatchCount())
		}
		for _, m := range []string{"one", "two", "three"} {
			if got := nodeMeta(t, store, m, beadmeta.RoutedToMetadataKey); got != "" {
				t.Fatalf("member %q gc.routed_to = %q, want empty (routing lives on the real bead)", m, got)
			}
			if bt := nodeBeadType(t, store, m); bt != "step" {
				t.Fatalf("member %q bead_type = %q, want step (real work bead lives in the work store)", m, bt)
			}
			if inFrontier(t, store, streamID, m+":0") {
				t.Fatalf("member %q left a claimable frontier row; the real bead is the only claim surface", m)
			}
		}

		// Members settle pass; one re-Advance settles the aggregate and seals.
		for _, m := range []string{"one", "two", "three"} {
			fake.settleAct(t, m+":0", engine.OutcomePass, "done "+m)
		}
		r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance 2: %v", err)
		}
		if !r2.Sealed {
			t.Fatalf("advance 2 = %+v, want Sealed after members settled", r2)
		}
		if r2.Run.Outcome != engine.OutcomePass {
			t.Fatalf("run outcome = %q, want pass (all members passed)", r2.Run.Outcome)
		}
		if o := settledOutcomeOf(t, store, streamID, "lanes:0"); o != engine.OutcomePass {
			t.Fatalf("aggregate lanes:0 settled %q, want pass", o)
		}
		if err := store.Verify(ctx, streamID); err != nil {
			t.Fatalf("Verify: %v", err)
		}
		// Drop+refold byte-identity over the fanned-out member rows.
		assertProjectionEqualsRefold(t, store, streamID)
		assertIncrementalEqualsProjectDelta(t, store, streamID)
	})

	t.Run("one_member_fails_aggregate_degrades", func(t *testing.T) {
		ctx := context.Background()
		store := newStore(t)
		streamID := "gcg-run-adv-scatter-degrade"
		doc := decodeIR(t, scatterOfDos("continue"))
		fake := newFakeWorkStore()

		if _, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil {
			t.Fatalf("advance 1: %v", err)
		}
		// One member fails; the other two pass. on_fail=continue ⇒ the aggregate degrades
		// (a mix of pass and non-pass), NOT a total failure.
		settles := map[string]string{"one": engine.OutcomeFailed, "two": engine.OutcomePass, "three": engine.OutcomePass}
		for m, o := range settles {
			fake.settleAct(t, m+":0", o, "settle "+m)
		}
		r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance 2: %v", err)
		}
		if !r2.Sealed {
			t.Fatalf("advance 2 = %+v, want Sealed", r2)
		}
		if o := settledOutcomeOf(t, store, streamID, "lanes:0"); o != engine.OutcomeDegraded {
			t.Fatalf("aggregate lanes:0 settled %q, want degraded (mixed member outcomes under on_fail continue)", o)
		}
		if r2.Run.Outcome != engine.OutcomeDegraded {
			t.Fatalf("run outcome = %q, want degraded", r2.Run.Outcome)
		}
		if err := store.Verify(ctx, streamID); err != nil {
			t.Fatalf("Verify: %v", err)
		}
	})
}

// TestAdvanceSilentDepDefersUntilRealInputSettles is the HIGH-2 repro:
// do P (pool) -> interp S (after P, = {{P}}) -> exec U (after S, echo {{S}}).
// A silent (interp) dep never settles, but the REAL node it derives its value from
// (P) must gate U. Advance MUST DEFER U until P settles — otherwise U's shell runs
// with {{S}} unresolved and settles early (a real side effect with the wrong
// input), diverging from Run. With the transitive-non-silent-closure fix, U defers,
// then runs with {{S}} == P's output, matching Run byte-for-byte.
func TestAdvanceSilentDepDefersUntilRealInputSettles(t *testing.T) {
	ctx := context.Background()
	docJSON := blockDoc("silentchain",
		doNode("P", "Produce.", nil),
		interpRefNode("S", "P", []string{"P"}),
		execNode("U", "echo {{S}}", []string{"S"}),
	)

	// Run: P inline via a host returning "pval"; U echoes S == P's output.
	runStore := newStore(t)
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"P": {Outcome: enginehost.OutcomePass, Output: "pval"},
	}}
	runRes, err := engine.RunWithOptions(ctx, runStore, decodeIR(t, docJSON), nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if runRes.NodeOutputs["U"] != "pval" {
		t.Fatalf("Run NodeOutputs[U] = %q, want pval (U echoes S == P's output)", runRes.NodeOutputs["U"])
	}

	// Advance: P pool, scripted settle "pval".
	advStore := newStore(t)
	streamID := "gcg-run-adv-silentchain"
	fake := newFakeWorkStore()
	opts := fake.opts()

	r1, err := engine.Advance(ctx, advStore, decodeIR(t, docJSON), streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "P" {
		t.Fatalf("advance 1 = %+v, want Parked with only P in flight", r1)
	}
	// U MUST NOT have run early: no outcome.settled for U while P is unsettled.
	if o := settledOutcomeOf(t, advStore, streamID, "U:0"); o != "" {
		t.Fatalf("U settled %q before P settled — the silent-dep defer failed (premature side effect)", o)
	}

	// P settles; re-Advance computes S then runs U with {{S}} resolved, and seals.
	if err := engine.SettleWorkForTest(ctx, advStore, streamID, "P:0", engine.OutcomePass, "pval"); err != nil {
		t.Fatalf("settle P: %v", err)
	}
	r2, err := engine.Advance(ctx, advStore, decodeIR(t, docJSON), streamID, nil, opts)
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v, err %v; want Sealed", r2, err)
	}
	if r2.Run.NodeOutputs["U"] != "pval" {
		t.Fatalf("Advance NodeOutputs[U] = %q, want pval (U resolved {{S}} == P's output)", r2.Run.NodeOutputs["U"])
	}
	if r2.Run.NodeOutputs["U"] != runRes.NodeOutputs["U"] {
		t.Fatalf("Advance U output %q != Run U output %q (Advance must match Run)", r2.Run.NodeOutputs["U"], runRes.NodeOutputs["U"])
	}
	if err := advStore.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestAdvanceRefusesDoInsideCombineAtLowering is MEDIUM-2: a `do` inside a gather
// combine has nowhere to run under a pool-mode Advance with no Host (pool
// materialization is top-level only; runGather runs the combine inline). It MUST be
// refused at lowering, before the lease is taken or any event is appended — never a
// late hard fail in runGather after the drained members already ran (which would
// re-fail on every re-Advance and never seal).
func TestAdvanceRefusesDoInsideCombineAtLowering(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-adv-combine-do"
	doc := decodeIR(t, blockDoc("fan",
		scatterNode("sc", nil, "continue",
			execNode("m1", "echo one", nil),
			execNode("m2", "echo two", nil),
		),
		gatherNode("gr", "sc", nil,
			doNode("combineDo", "Combine the results.", nil),
		),
	))

	_, err := engine.Advance(ctx, store, doc, streamID, nil, engine.Options{PoolRouter: advRouter})
	if !errors.Is(err, engine.ErrUnsupportedNode) {
		t.Fatalf("advance = %v, want ErrUnsupportedNode (do refused inside combine at lowering)", err)
	}
	// Refused before ANY append: no run.started committed.
	if n := countJournalType(t, store, streamID, engine.EventRunStarted); n != 0 {
		t.Fatalf("run.started committed despite a lowering refusal: %d (late hard fail, not a lowering refusal)", n)
	}
}

// TestAdvanceRefusesColonInStreamID is LOW-2: the stream id is the run root node
// id, and ':' is the activation-key delimiter. A colon-bearing stream id is refused
// loudly at Advance entry (it would diverge the root frontier row on a refold).
func TestAdvanceRefusesColonInStreamID(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, _ := doOnlyDoc()
	doc := decodeIR(t, docJSON)

	_, err := engine.Advance(ctx, store, doc, "gcg:run:bad", nil, engine.Options{PoolRouter: advRouter})
	if err == nil {
		t.Fatal("advance accepted a colon-bearing stream id; want a loud refusal")
	}
	if !strings.Contains(err.Error(), "':'") {
		t.Fatalf("advance error = %v, want a clear colon refusal", err)
	}
}

// TestAdvanceNoPoolRouteIsLoudError exercises the ErrNoPoolRoute branch: a
// pool-mode do whose agent binding resolves to no route is a loud config error, not
// a silent inline fallback.
func TestAdvanceNoPoolRouteIsLoudError(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, streamID := doOnlyDoc()
	doc := decodeIR(t, docJSON)

	noRoute := func(string) (string, bool) { return "", false }
	_, err := engine.Advance(ctx, store, doc, streamID, nil, engine.Options{PoolRouter: noRoute})
	if !errors.Is(err, engine.ErrNoPoolRoute) {
		t.Fatalf("advance with an unresolvable pool route = %v, want ErrNoPoolRoute", err)
	}
}

// --- helpers ---------------------------------------------------------------

// interpRefNode renders a silent interp node whose value is a ref to refName,
// after the given deps. It emits no journal event and never settles; its value is
// computed into scope from the referenced node's settled output.
func interpRefNode(id, refName string, after []string) string {
	afterJSON, _ := json.Marshal(after)
	return `{
      "kind": "interp", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "value": {"kind": "ref", "name": "` + refName + `"}
    }`
}

// inFlightPrompt returns the rendered prompt of the in-flight pool work for nodeID
// (or "" if absent).
func inFlightPrompt(inFlight []engine.PoolWork, nodeID string) string {
	for _, pw := range inFlight {
		if pw.NodeID == nodeID {
			return pw.Prompt
		}
	}
	return ""
}

// inFlightNodeIDs returns the node ids of the in-flight pool work, order-free.
func inFlightNodeIDs(inFlight []engine.PoolWork) []string {
	out := make([]string, len(inFlight))
	for i, pw := range inFlight {
		out[i] = pw.NodeID
	}
	return out
}

// inFlightHas reports whether nodeID is among the in-flight pool work.
func inFlightHas(inFlight []engine.PoolWork, nodeID string) bool {
	for _, pw := range inFlight {
		if pw.NodeID == nodeID {
			return true
		}
	}
	return false
}

// equalStringSet compares two string slices as sets (order- and duplicate-free).
func equalStringSet(got, want []string) bool {
	set := map[string]int{}
	for _, s := range got {
		set[s]++
	}
	for _, s := range want {
		if set[s] == 0 {
			return false
		}
		set[s]--
	}
	for _, n := range set {
		if n != 0 {
			return false
		}
	}
	return true
}

// nodeBeadType reads a projected node's bead_type from the Tier-A projection.
func nodeBeadType(t *testing.T, store *graphstore.Store, nodeID string) string {
	t.Helper()
	var bt string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT bead_type FROM nodes WHERE id = ? AND fold_owned = 1`, nodeID).Scan(&bt); err != nil {
		t.Fatalf("read bead_type of %q: %v", nodeID, err)
	}
	return bt
}

// settledOutcomeOf returns the outcome of the first outcome.settled event for
// activation in streamID, or "" if the activation has not settled.
func settledOutcomeOf(t *testing.T, store *graphstore.Store, streamID, activation string) string {
	t.Helper()
	events, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return p.Outcome
		}
	}
	return ""
}

// leaseEpochOfType returns the lease_epoch stamped on the first journal row of
// the given type — the fencing token the append carried.
func leaseEpochOfType(t *testing.T, store *graphstore.Store, streamID, typ string) uint64 {
	t.Helper()
	events, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, e := range events {
		if e.Type == typ {
			return e.LeaseEpoch
		}
	}
	t.Fatalf("no %q event in stream %q", typ, streamID)
	return 0
}

// lastEventType returns the type of the last committed event.
func lastEventType(events []graphstore.StoredEvent) string {
	if len(events) == 0 {
		return ""
	}
	return events[len(events)-1].Type
}

// assertIncrementalEqualsProjectDelta proves the incremental fold's net node
// projection equals the full-state ProjectDelta (DET-T-17), so the pool
// route/prompt/task-type fields carry no state ProjectDelta misses.
func assertIncrementalEqualsProjectDelta(t *testing.T, store *graphstore.Store, streamID string) {
	t.Helper()
	events := readFoldEvents(t, store, streamID)
	state, deltas, err := fold.Fold(engine.Reducer(), nil, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	projector, ok := state.(fold.SnapshotProjector)
	if !ok {
		t.Fatal("lumen state is not a SnapshotProjector")
	}
	inc := collapseNodeUpserts(deltas)
	full := collapseNodeUpserts([]fold.Delta{projector.ProjectDelta(streamID)})
	if !reflect.DeepEqual(inc, full) {
		t.Fatalf("incremental node projection != full ProjectDelta:\nincremental=%+v\nfull=%+v", inc, full)
	}
}
