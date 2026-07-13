package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// linear3DoDoc is the canonical v1 fixture: three agent `do` steps chained a → b → c,
// the degenerate linear DAG the stepper drives one turn at a time in one session.
func linear3DoDoc(t *testing.T) *ir.IR {
	t.Helper()
	return decodeIR(t, blockDoc("linear3",
		doNode("a", "Do a.", nil),
		doNode("b", "Do b after {{a}}.", []string{"a"}),
		doNode("c", "Do c after {{b}}.", []string{"b"}),
	))
}

// enqueueV1 seeds a self-driven run.started for doc and returns its stream id — the
// state the agent's first Step rebuilds from.
func enqueueV1(t *testing.T, store *graphstore.Store, doc *ir.IR) string {
	t.Helper()
	streamID, err := engine.EnqueueRunWithDriver(context.Background(), store, doc, nil, "ref", "", "self")
	if err != nil {
		t.Fatalf("enqueue v1: %v", err)
	}
	return streamID
}

// stepScript maps a do node id to the {outcome, output} the scripted agent self-reports.
type stepScript map[string][2]string

// driveStepperToSeal drives a stepper run to run.closed: an initial Step, then a Settle
// per offered do (with the scripted outcome/output), until Done. It asserts the run
// sealed and returns the final result.
func driveStepperToSeal(t *testing.T, store *graphstore.Store, doc *ir.IR, streamID string, script stepScript) engine.StepResult {
	t.Helper()
	ctx := context.Background()
	res, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("initial step: %v", err)
	}
	guard := 0
	for !res.Done {
		guard++
		if guard > 100 {
			t.Fatalf("stepper did not seal after 100 turns (last do %q)", res.NodeID)
		}
		oc, ok := script[res.NodeID]
		if !ok {
			t.Fatalf("stepper offered unscripted do %q", res.NodeID)
		}
		res, err = engine.Settle(ctx, store, doc, streamID, nil, res.NodeID, oc[0], oc[1], engine.Options{})
		if err != nil {
			t.Fatalf("settle %q: %v", res.NodeID, err)
		}
	}
	return res
}

// scriptedStubHost returns a StubHost scripting each do to the same {outcome, output} the
// stepper script self-reports — the determinism oracle's synchronous twin.
func scriptedStubHost(script stepScript) *enginehost.StubHost {
	res := map[string]enginehost.DoResult{}
	for node, oc := range script {
		var out string
		switch oc[0] {
		case engine.OutcomePass:
			out = oc[1]
			res[node] = enginehost.DoResult{Outcome: enginehost.OutcomePass, Output: out}
		case engine.OutcomeDegraded:
			res[node] = enginehost.DoResult{Outcome: enginehost.OutcomeDegraded, Output: oc[1]}
		default:
			res[node] = enginehost.DoResult{Outcome: enginehost.OutcomeFailed, Output: oc[1]}
		}
	}
	return &enginehost.StubHost{Results: res}
}

// TestStepperDeterminismOracle is THE load-bearing pin: a v1 stepper run driven
// turn-by-turn through Step/Settle emits the SAME journal event TYPE sequence, in the
// same canonical order, and folds to the SAME normalized state hash as a synchronous
// engine.Run scripting the same do outcomes with a StubHost. The stepper substitutes the
// agent's turns for the host's RunDo but writes the identical journal — the strongest
// possible determinism statement (v1 self-drive IS engine.Run with the agent as host).
func TestStepperDeterminismOracle(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	script := stepScript{
		"a": {engine.OutcomePass, "out-a"},
		"b": {engine.OutcomePass, "out-b"},
		"c": {engine.OutcomePass, "out-c"},
	}

	// Oracle: synchronous engine.Run + StubHost.
	oracleStore := newStore(t)
	oracleRes, err := engine.RunWithOptions(ctx, oracleStore, doc, nil, engine.Options{Host: scriptedStubHost(script)})
	if err != nil {
		t.Fatalf("oracle run: %v", err)
	}
	if oracleRes.Outcome != engine.OutcomePass {
		t.Fatalf("oracle outcome = %q, want pass", oracleRes.Outcome)
	}

	// Stepper: enqueue + drive turn-by-turn.
	stepStore := newStore(t)
	streamID := enqueueV1(t, stepStore, doc)
	stepRes := driveStepperToSeal(t, stepStore, doc, streamID, script)
	if stepRes.Outcome != engine.OutcomePass {
		t.Fatalf("stepper outcome = %q, want pass", stepRes.Outcome)
	}

	oracleEvents, err := oracleStore.ReadStream(ctx, oracleRes.StreamID, 1, 0)
	if err != nil {
		t.Fatalf("read oracle stream: %v", err)
	}
	stepEvents, err := stepStore.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stepper stream: %v", err)
	}

	// (1) Byte-identical event TYPE sequence in canonical order.
	got := eventTypes(stepEvents)
	want := eventTypes(oracleEvents)
	if len(got) != len(want) {
		t.Fatalf("event count: stepper=%d oracle=%d\nstepper=%v\noracle=%v", len(got), len(want), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("event[%d] type: stepper=%q oracle=%q\nstepper=%v\noracle=%v", i, got[i], want[i], got, want)
		}
	}

	// (2) The determinism-oracle SHA: normalized folds are byte-identical.
	if engine.NormalizedFoldHashForTest(t, stepEvents) != engine.NormalizedFoldHashForTest(t, oracleEvents) {
		t.Fatalf("stepper fold hash != engine.Run oracle fold hash — the stepper diverged from the determinism oracle")
	}

	// Sanity: the surviving stepper journal verifies and the settled do outcomes match.
	if err := stepStore.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify stepper journal: %v", err)
	}
	settled := settledIDs(t, stepEvents)
	wantSettled := [][2]string{{"a", "pass"}, {"b", "pass"}, {"c", "pass"}}
	if len(settled) != len(wantSettled) {
		t.Fatalf("settled = %v, want %v", settled, wantSettled)
	}
	for i := range wantSettled {
		if settled[i] != wantSettled[i] {
			t.Fatalf("settled[%d] = %v, want %v", i, settled[i], wantSettled[i])
		}
	}
}

// TestStepperIdempotentReStep pins re-Step idempotency (mutation pin (iv)): calling Step
// twice before any Settle returns the SAME ready do BOTH times and appends NOTHING on the
// second call — the node.activated dedups on its write-once idem token, and no effect
// record exists yet (the split discipline: Step writes only node.activated). Breaking the
// activation's write-once token (re-appending on re-step) grows the journal and reds this.
func TestStepperIdempotentReStep(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)

	first, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step 1: %v", err)
	}
	if first.Done || first.NodeID != "a" {
		t.Fatalf("step 1 = %+v, want the ready do a", first)
	}
	headAfterFirst, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	second, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step 2 (re-step): %v", err)
	}
	if second.NodeID != first.NodeID || second.Prompt != first.Prompt {
		t.Fatalf("re-step returned a different do: first=%+v second=%+v", first, second)
	}
	headAfterSecond, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if headAfterSecond != headAfterFirst {
		t.Fatalf("re-step appended events: head %d -> %d (must be idempotent)", headAfterFirst, headAfterSecond)
	}
	// Exactly one node.activated for a, zero effect records (Step writes neither).
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 1 {
		t.Fatalf("node.activated count = %d, want 1 (re-step must not re-activate)", n)
	}
	if n := countJournalType(t, store, streamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("effect.scheduled count = %d, want 0 (Step writes no effect record)", n)
	}
}

// TestStepperDuplicateSettleNoOp pins duplicate-Settle idempotency (mutation pin (iii)):
// settling the same do twice with the same outcome appends the effect/outcome events ONCE
// — every append in settleDoTurn dedups on its write-once idem token — so the journal head
// is unchanged and exactly one outcome.settled exists for the do. Dropping the idem-token
// discipline double-appends the settle and reds this.
func TestStepperDuplicateSettleNoOp(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)

	first, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	if first.NodeID != "a" {
		t.Fatalf("step = %+v, want do a", first)
	}

	next1, err := engine.Settle(ctx, store, doc, streamID, nil, "a", engine.OutcomePass, "out-a", engine.Options{})
	if err != nil {
		t.Fatalf("settle a (1): %v", err)
	}
	if next1.Done || next1.NodeID != "b" {
		t.Fatalf("settle a (1) = %+v, want next do b", next1)
	}
	headAfterFirst, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	// Duplicate Settle of a (already settled) with the SAME outcome: a no-op that still
	// fuses the next ready do b.
	next2, err := engine.Settle(ctx, store, doc, streamID, nil, "a", engine.OutcomePass, "out-a", engine.Options{})
	if err != nil {
		t.Fatalf("settle a (2, duplicate): %v", err)
	}
	if next2.Done || next2.NodeID != "b" {
		t.Fatalf("duplicate settle a = %+v, want the same next do b", next2)
	}
	headAfterSecond, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if headAfterSecond != headAfterFirst {
		t.Fatalf("duplicate settle appended events: head %d -> %d (must dedup)", headAfterFirst, headAfterSecond)
	}
	if n := countJournalType(t, store, streamID, engine.EventOutcomeSettled); n != 1 {
		t.Fatalf("outcome.settled count = %d, want 1 (a settled once, write-once)", n)
	}
}

// TestStepperSealsOnLastSettle pins that run.closed fires EXACTLY on the last do's Settle
// — not before, not on Step. It also proves the run-outcome aggregation over the linear
// chain (a pass, b degraded, c pass ⇒ degraded run).
func TestStepperSealsOnLastSettle(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)

	if n := countJournalType(t, store, streamID, engine.EventRunClosed); n != 0 {
		t.Fatalf("run.closed present before any step")
	}

	res, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("step: %v", err)
	}
	// Settle a, then b — no seal yet (c still pending).
	for _, step := range []struct{ node, outcome, output string }{
		{"a", engine.OutcomePass, "out-a"},
		{"b", engine.OutcomeDegraded, "out-b"},
	} {
		res, err = engine.Settle(ctx, store, doc, streamID, nil, step.node, step.outcome, step.output, engine.Options{})
		if err != nil {
			t.Fatalf("settle %q: %v", step.node, err)
		}
		if res.Done {
			t.Fatalf("run sealed early after settling %q (c still pending)", step.node)
		}
		if n := countJournalType(t, store, streamID, engine.EventRunClosed); n != 0 {
			t.Fatalf("run.closed present after settling %q (c still pending)", step.node)
		}
	}
	if res.NodeID != "c" {
		t.Fatalf("after settling a,b the next do = %q, want c", res.NodeID)
	}

	// The last Settle seals.
	final, err := engine.Settle(ctx, store, doc, streamID, nil, "c", engine.OutcomePass, "out-c", engine.Options{})
	if err != nil {
		t.Fatalf("settle c: %v", err)
	}
	if !final.Done {
		t.Fatalf("run did not seal on the last settle: %+v", final)
	}
	if final.Outcome != engine.OutcomeDegraded {
		t.Fatalf("run outcome = %q, want degraded (b degraded)", final.Outcome)
	}
	if n := countJournalType(t, store, streamID, engine.EventRunClosed); n != 1 {
		t.Fatalf("run.closed count = %d, want exactly 1 (sealed once, on the last settle)", n)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestStepperSettleRefusesUnreadyDo is the agent-trust robustness pin (P3-A): Settle
// refuses a do that is not a currently-offered ready do — one whose dependencies have not
// settled (its {{ref}} scope is unresolved), or a non-existent / non-do node — with a LOUD
// error and NO journal write (no node.activated, no effect record). Dropping the readiness
// guard lets a misbehaving agent settle an out-of-order do, growing the journal and
// reddening this pin.
func TestStepperSettleRefusesUnreadyDo(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)

	head0, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	// Settle b before a has settled: b's dependency (a) is unsettled, so b is not ready.
	if _, err := engine.Settle(ctx, store, doc, streamID, nil, "b", engine.OutcomePass, "x", engine.Options{}); err == nil {
		t.Fatalf("settle of unready do b succeeded; want a loud refusal")
	}
	// Settle c (deps b, a unsettled) likewise.
	if _, err := engine.Settle(ctx, store, doc, streamID, nil, "c", engine.OutcomePass, "x", engine.Options{}); err == nil {
		t.Fatalf("settle of unready do c succeeded; want a loud refusal")
	}
	// Settle a non-existent node.
	if _, err := engine.Settle(ctx, store, doc, streamID, nil, "nope", engine.OutcomePass, "x", engine.Options{}); err == nil {
		t.Fatalf("settle of non-existent node succeeded; want a loud refusal")
	}

	// NO journal write from any refused settle: head unchanged, zero activations/effects.
	head1, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head1 != head0 {
		t.Fatalf("refused settles wrote to the journal: head %d -> %d (must be no-op)", head0, head1)
	}
	if n := countJournalType(t, store, streamID, engine.EventNodeActivated); n != 0 {
		t.Fatalf("node.activated count = %d, want 0 (a refused settle must not activate)", n)
	}
	if n := countJournalType(t, store, streamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("effect.scheduled count = %d, want 0 (a refused settle must not schedule)", n)
	}

	// The correctly-offered first do (a, no deps) still settles — the guard is not overbroad.
	next, err := engine.Settle(ctx, store, doc, streamID, nil, "a", engine.OutcomePass, "out-a", engine.Options{})
	if err != nil {
		t.Fatalf("settle of ready do a: %v", err)
	}
	if next.Done || next.NodeID != "b" {
		t.Fatalf("settle a = %+v, want next do b (now that a settled, b is ready)", next)
	}
}
