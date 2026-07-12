package engine_test

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- repeat-run-body (RBL) driver fixtures ----------------------------------

// repeatRunLoop builds a repeat loop node (fixed id "loop") whose body is a run
// (the RBL shape).
func repeatRunLoop(after []string, runBody, cond string) string {
	afterJSON := "[]"
	if len(after) > 0 {
		afterJSON = `["` + strings.Join(after, `","`) + `"]`
	}
	return `{"kind":"repeat","id":"loop","name":"loop","after":` + afterJSON +
		`,"body":` + runBody + `,"cond":` + cond + `,"iterationName":"iteration"}`
}

// runCondPassOrIter is the canonical RBL exit `stage.outcome == "pass" ||
// iteration >= 5` over the fixed "stage" body id (5 comfortably exceeds every
// fixture's attempt count without approaching the 32 loop cap).
func runCondPassOrIter() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"stage","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":5}]}]}`
}

// runCondOutcomeEq builds `stage.outcome == "<want>"` — no iteration escape, so a
// never-matching want drives the loop to its cap (the loop_cap / spin fixtures).
func runCondOutcomeEq(want string) string {
	return `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"stage","field":"outcome"},` +
		`{"kind":"literal","value":"` + want + `"}]}`
}

// TestRunRepeatRunBodyExecOnlyInlineSeals (⚑B2 inline) proves a repeat whose body runs
// an exec-only sub-formula seals in ONE inline Run pass: the exec settles in-arm, the
// attempt aggregate settles, and the cond exits — no stall.
func TestRunRepeatRunBodyExecOnlyInlineSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"),
			execNode("hello", `echo "hi {{ name }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/0/hello"]; got != "hi world" {
		t.Errorf("sub output stage/0/hello = %q, want %q (env-seeded run body)", got, "hi world")
	}
	if _, _, _, out := loopSettle(t, res.Events, "loop:0"); out != "hi world" {
		t.Errorf("loop settle output = %q, want the satisfying attempt's transparent output", out)
	}
}

// TestAdvanceRepeatRunBodyExecOnlyPoolSeals (⚑B2 pool — the stall regression) proves a
// repeat whose body runs an EXEC-only sub-formula seals in one Advance pass even under a
// PoolRouter: the run body must route to advanceLoop's run-body arm (loopPoolMode keys
// bodyIRKind==NodeDo, so without the routing fix this fell through to runLoop→runDo's
// nil-host error). No pool bead dispatches; no ErrAdvanceStalled.
func TestAdvanceRepeatRunBodyExecOnlyPoolSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"),
			execNode("hello", `echo "hi {{ name }}"`, nil)),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-rbl-execpool", map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance stalled/errored (the exec-only run-body pool regression): %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance = %+v, want Sealed (exec-only run body seals in one pass)", res)
	}
	if res.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Run.Outcome)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("DispatchWork calls = %d, want 0 (exec body dispatches no pool work)", fake.dispatchCount())
	}
}

// TestAdvanceRepeatRunBodyFailThenPassReMints (§6 seed #1, the acceptance shape) proves
// the fresh-namespace re-mint: attempt 0's sub-do fails → the attempt aggregate settles
// failed → the cond re-mints attempt 1 under stage/1/ with a FRESH bead → it passes →
// the aggregate settles pass → the cond exits and the loop settles pass → seal. Both
// attempts' sub-dos are distinct activations (stage/0/hello:0 vs stage/1/hello:0), and
// the live projection drop+refolds byte-identically (no hidden reducer state, v4).
func TestAdvanceRepeatRunBodyFailThenPassReMints(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-failthenpass"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"),
			doNode("hello", "greet {{ name }}", nil)),
	))

	// Pass 1: dispatch attempt 0's sub-do stage/0/hello:0, park.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "stage/0/hello:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/0/hello:0", r1, err)
	}

	// Attempt 0 fails; pass 2 settles it, the aggregate settles failed, and the cond
	// re-mints attempt 1 (stage/1/hello:0, a FRESH bead), then parks on it.
	fake.settle("wb-1", engine.OutcomeFailed, "nope")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "stage/1/hello:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with stage/1/hello:0 (re-mint)", r2, err)
	}

	// Attempt 1 passes; pass 3 settles it, the aggregate settles pass, and the loop settles pass → seal.
	fake.settle("wb-2", engine.OutcomePass, "hello world")
	r3, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r3.Run.Outcome)
	}

	// Two dispatches, distinct per-attempt activations + fresh beads.
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (one fresh sub-do per attempt)", fake.dispatchCount())
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	// Both attempts' aggregates settled (stage), highest-attempt (pass) wins the bare id.
	if settled["stage"] != engine.OutcomePass {
		t.Errorf("stage final settle = %q, want pass (highest attempt)", settled["stage"])
	}
	if settled["stage/0/hello"] != engine.OutcomeFailed {
		t.Errorf("attempt-0 sub-do = %q, want failed", settled["stage/0/hello"])
	}
	if settled["stage/1/hello"] != engine.OutcomePass {
		t.Errorf("attempt-1 sub-do = %q, want pass", settled["stage/1/hello"])
	}
	if _, _, _, out := loopSettle(t, r3.Run.Events, "loop:0"); out != "hello world" {
		t.Errorf("loop settle output = %q, want the passing attempt's transparent output", out)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestAdvanceRepeatRunBodyEnvRenderPin (§6 seed #2, ⚑B1 — the load-bearing catch) pins
// that a run-body env binding renders correctly INSIDE attempt ≥ 1. The sub-do prompt
// binds an INPUT (name<-who) and a parent NODE (from<-prep); without the mint-time env
// registration + parent-namespace override, scopeFor("stage/1/") sees a phantom parent
// and every binding renders "" silently. The pin asserts the RENDERED prompt of the
// re-minted attempt-1 sub-do, not just its outcome.
func TestAdvanceRepeatRunBodyEnvRenderPin(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-envpin"
	fake := newFakeWorkStore()
	envFields := `[` +
		`{"name":"name","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"from","value":{"kind":"expr","expr":{"kind":"ref","name":"prep"}}}]`
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		execNode("prep", `echo prepped`, nil)+","+
			repeatRunLoop(nil,
				runNodeRawEnv("stage", nil, "greeter", envFields),
				runCondPassOrIter()),
		subDoc("greeter", strField("name")+","+strField("from"),
			doNode("hello", "hi {{ name }} from {{ from }}", nil)),
	))

	// Pass 1: prep runs inline; attempt 0's sub-do dispatches.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || r1.InFlight[0].Activation != "stage/0/hello:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/0/hello:0", r1, err)
	}
	// Attempt-0 prompt already resolves the env (baseline).
	if got := fake.dispatchPromptFor(t, "stage/0/hello:0"); got != "hi world from prepped" {
		t.Fatalf("attempt-0 prompt = %q, want %q", got, "hi world from prepped")
	}

	// Attempt 0 fails → re-mint attempt 1 under the FRESH namespace stage/1/.
	fake.settle("wb-1", engine.OutcomeFailed, "no")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || r2.InFlight[0].Activation != "stage/1/hello:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with stage/1/hello:0", r2, err)
	}
	// THE PIN: the re-minted attempt-1 sub-do prompt must still render the env — the B1
	// seam (registered per attempt) is what keeps scopeFor("stage/1/") from collapsing.
	if got := fake.dispatchPromptFor(t, "stage/1/hello:0"); got != "hi world from prepped" {
		t.Fatalf("attempt-1 rendered prompt = %q, want %q (B1 env seam — a phantom parent renders \"\")", got, "hi world from prepped")
	}
}

// TestAdvanceRepeatRunBodyScatterConcurrency (§6 seed #5) proves a scatter INSIDE the run
// body fans both members as claimable pool work in ONE sweep (intra-attempt concurrency,
// not one-at-a-time), then parks on both; a later Advance drains them and seals.
func TestAdvanceRepeatRunBodyScatterConcurrency(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-scatter"
	fake := newFakeWorkStore()
	subScatter := scatterNode("lanes", nil, "continue",
		doNode("a", "do a for {{ name }}", nil),
		doNode("b", "do b for {{ name }}", nil))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"), subScatter),
	))

	// Pass 1: both scatter members dispatch CONCURRENTLY in one sweep, then park.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	if len(r1.InFlight) != 2 || fake.dispatchCount() != 2 {
		t.Fatalf("in-flight = %d, dispatched = %d, want 2 concurrent sub-do beads in one sweep", len(r1.InFlight), fake.dispatchCount())
	}

	// Both pass → the scatter drains pass, the attempt aggregate passes, the cond exits.
	// Scatter members inside the attempt are namespaced under the ATTEMPT prefix
	// (stage/0/a), not under the scatter node — a scatter creates no sub-namespace.
	fake.settleAct(t, "stage/0/a:0", engine.OutcomePass, "ra")
	fake.settleAct(t, "stage/0/b:0", engine.OutcomePass, "rb")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v err %v, want Sealed", r2, err)
	}
	if r2.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r2.Run.Outcome)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestAdvanceRepeatRunBodyWriteOnceUnderRedundantAdvance proves a redundant Advance with
// NO new settlement is a no-op: attempt 0 stays in flight, no re-dispatch, no double
// append (the head is unchanged), and the re-mint + attempt.minted dedupe cleanly.
func TestAdvanceRepeatRunBodyWriteOnceUnderRedundantAdvance(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-writeonce"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"), doNode("hello", "greet {{ name }}", nil)),
	))

	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	headAfter1 := r1.Head
	// Two redundant Advances with no settlement: each must be a pure no-op.
	for i := 0; i < 2; i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
		if err != nil || !r.Parked {
			t.Fatalf("redundant advance %d = %+v err %v, want Parked", i, r, err)
		}
		if r.Head != headAfter1 {
			t.Fatalf("redundant advance %d moved the head %d -> %d (a double append)", i, headAfter1, r.Head)
		}
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls = %d across redundant Advances, want 1 (write-once)", fake.dispatchCount())
	}
}

// TestAdvanceRepeatRunBodyLoopCap (§6 seed #6) proves a cond that never matches drives to
// the loop cap: each attempt's sub-formula RUNS (settle pass), the cond checks an
// impossible outcome, and after lumenRepeatLoopCap attempts the loop settles
// failed{loop_cap} — each attempt in a fresh namespace, bounded.
func TestAdvanceRepeatRunBodyLoopCap(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-loopcap"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondOutcomeEq("never")), // impossible; no iteration escape
		subDoc("greeter", strField("name"), settleNode("s", "pass")),
	))
	res, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("advance = %+v, want Sealed failed (loop cap)", res)
	}
	out, reason, _, _ := loopSettle(t, res.Run.Events, "loop:0")
	if reason != "loop_cap" {
		t.Errorf("loop settle reason = %q (out %q), want loop_cap", reason, out)
	}
	// Each attempt got a fresh namespace: attempt 0 and the last attempt both settled.
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["stage/0/s"] != engine.OutcomePass || settled["stage/31/s"] != engine.OutcomePass {
		t.Errorf("want fresh namespaces stage/0/s and stage/31/s both settled pass; got 0=%q 31=%q", settled["stage/0/s"], settled["stage/31/s"])
	}
}

// TestAdvanceRepeatRunBodyAllDidNotRunSpins (§6 seed #7) pins the chosen behavior for an
// all-didNotRun attempt: the sub-formula's sole member settle-CANCELS, so the attempt
// aggregate settles SKIPPED; the cond (outcome == "pass") never matches a skipped
// aggregate, so the loop re-mints identical skipped attempts to the cap and settles
// failed{loop_cap} — deterministic and bounded, not an infinite spin.
func TestAdvanceRepeatRunBodyAllDidNotRunSpins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-rbl-spin"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondOutcomeEq("pass")), // a skipped aggregate never matches
		subDoc("greeter", strField("name"), settleNode("s", "canceled")),
	))
	res, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("advance = %+v, want Sealed failed (bounded spin -> loop cap)", res)
	}
	if _, reason, _, _ := loopSettle(t, res.Run.Events, "loop:0"); reason != "loop_cap" {
		t.Errorf("loop settle reason = %q, want loop_cap (bounded)", reason)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["stage"] != engine.OutcomeSkipped {
		t.Errorf("attempt aggregate stage = %q, want skipped (all-didNotRun)", settled["stage"])
	}
}

// TestAdvanceRepeatRunBodyNestedRunEnvChains pins ⚑B1(i): a run INSIDE the repeat's run
// body gets its own env spec registered per attempt, and its string-derived parent
// (`stage/N/`) is the attempt namespace — so scopeFor chains name<-who through two run
// boundaries and the DEEP sub-do (stage/N/inner/greet) renders "hi world", not "". A
// missing nested-run registration would collapse the deep view to {}. Pinned at BOTH
// attempt 0 AND the re-minted attempt 1 (the B1 geometry one namespace level deeper
// than the top-level render pin).
func TestAdvanceRepeatRunBodyNestedRunEnvChains(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "mid", "name", "who"),
			runCondPassOrIter()),
		subDoc("mid", strField("name"),
			runNodeJSON("inner", nil, "leaf", "name", "name"))+","+
			subDoc("leaf", strField("name"), doNode("greet", "hi {{ name }}", nil)),
	))
	r1, err := engine.Advance(ctx, store, doc, "gcg-rbl-nested", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || r1.InFlight[0].Activation != "stage/0/inner/greet:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked on the deep sub-do stage/0/inner/greet:0", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/0/inner/greet:0"); got != "hi world" {
		t.Fatalf("attempt-0 deep sub-do prompt = %q, want %q (env chained through two run boundaries)", got, "hi world")
	}

	// Attempt 0's deep do fails → the inner run agg fails → the attempt agg fails → the
	// cond re-mints attempt 1. The RE-registered nested seam (inner under stage/1/) must
	// chain identically — the deep pin inside attempt ≥ 1.
	fake.settle("wb-1", engine.OutcomeFailed, "no")
	r2, err := engine.Advance(ctx, store, doc, "gcg-rbl-nested", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || r2.InFlight[0].Activation != "stage/1/inner/greet:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked on the re-minted deep sub-do stage/1/inner/greet:0", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/1/inner/greet:0"); got != "hi world" {
		t.Fatalf("attempt-1 deep sub-do prompt = %q, want %q (B1 seam re-registered one level deeper)", got, "hi world")
	}
}

// TestAdvanceRepeatRunBodyBlockedSkipsWithZeroMints proves a run-body loop gated on a
// FAILED parent skip-cascades exactly like a leaf-body loop: the loop settles skipped
// through runUnit's blocked() intercept, ZERO attempts mint (no attempt.minted, no
// sub-graph, no dispatches), and the run seals failed in one pass.
func TestAdvanceRepeatRunBodyBlockedSkipsWithZeroMints(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		execNode("gate", `exit 1`, nil)+","+
			repeatRunLoop([]string{"gate"},
				runNodeJSON("stage", nil, "greeter", "name", "who"),
				runCondPassOrIter()),
		subDoc("greeter", strField("name"), doNode("hello", "greet {{ name }}", nil)),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-rbl-blocked", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !res.Sealed {
		t.Fatalf("advance = %+v err %v, want Sealed in one pass (nothing in flight)", res, err)
	}
	if res.Run.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed (the gate failed)", res.Run.Outcome)
	}
	settled := settledOutcomeByID(t, res.Run.Events)
	if settled["loop"] != engine.OutcomeSkipped {
		t.Errorf("loop settled %q, want skipped (blocked gate skip-cascade)", settled["loop"])
	}
	if n := countAttemptMinted(res.Run.Events); n != 0 {
		t.Errorf("attempt.minted count = %d, want 0 (a blocked loop never mints)", n)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("DispatchWork calls = %d, want 0", fake.dispatchCount())
	}
}

// TestEnqueueRepeatRunBodyDryRunRefuses (§6 seed #8) proves the ⚑S4 dry-run guards the
// ENQUEUE gate: a repeat run body whose sub-formula contains an un-lowerable node does not
// lower, so EnqueueRun refuses LOUDLY rather than seeding a run.started that would wedge
// open forever — pre-validated with the controller-loop flags. (A plain nested loop now
// lowers after LIS; the durable un-lowerable shape is a '/'-forged cond ref.)
func TestEnqueueRepeatRunBodyDryRunRefuses(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// greeter's own body is a repeat loop whose cond forges a '/'-bearing ref — a
	// permanent reserved-delimiter refusal, so the dry-run mint refuses.
	forgedCond := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"forged/ref","field":"outcome"},{"kind":"literal","value":"pass"}]}`
	subLoop := subDoc("greeter", strField("name"),
		repeatNode(execNode("b1", "echo hi", nil), forgedCond))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subLoop,
	))
	streamID, err := engine.EnqueueRun(ctx, store, doc, nil, "packs/x@v1", "workers")
	if err == nil {
		t.Fatalf("enqueue accepted a run body that cannot lower (streamID=%q); want a loud refusal", streamID)
	}
	if !strings.Contains(err.Error(), "does not lower") || !strings.Contains(err.Error(), "reserved delimiter") {
		t.Errorf("enqueue error = %v, want it to name the un-lowerable run body (does not lower / reserved delimiter)", err)
	}
}

// TestRunRepeatRunBodyInlineFailThenPassReMints proves the INLINE (Run) run-body arm
// re-mints across attempts: attempt 0's sub-do fails → the aggregate settles failed →
// the cond re-mints attempt 1 under stage/1/ → its sub-do passes → the loop settles pass.
// The StubHost is keyed by the per-attempt qualified node id, so each attempt is a
// distinct at-most-once effect.
func TestRunRepeatRunBodyInlineFailThenPassReMints(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"), doNode("hello", "greet {{ name }}", nil)),
	))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/0/hello": {Outcome: enginehost.OutcomeFailed, Output: "no"},
		"stage/1/hello": {Outcome: enginehost.OutcomePass, Output: "done"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"who": "world"}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass (re-mint then pass)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/hello"] != engine.OutcomeFailed || settled["stage/1/hello"] != engine.OutcomePass {
		t.Errorf("attempt sub-dos = {0:%q 1:%q}, want {failed pass}", settled["stage/0/hello"], settled["stage/1/hello"])
	}
	if _, _, _, out := loopSettle(t, res.Events, "loop:0"); out != "done" {
		t.Errorf("loop settle output = %q, want the passing attempt's output", out)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestRunRepeatRunBodyMidAttemptCrashResumes (§6 seed #3) proves a crash mid-attempt (a
// scatter body with one of two members settled) resumes identically: the re-mint reloads
// the settled member by exact key and drives the other, sealing to the same outcome with
// a byte-identical drop+refold. It exercises the INLINE (Run/Resume) run-body arm.
func TestRunRepeatRunBodyMidAttemptCrashResumes(t *testing.T) {
	subScatter := scatterNode("lanes", nil, "continue",
		doNode("m1", "m1 for {{ name }}", nil),
		doNode("m2", "m2 for {{ name }}", nil))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "name", "who"),
			runCondPassOrIter()),
		subDoc("greeter", strField("name"), subScatter),
	))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/0/m1": {Outcome: enginehost.OutcomePass, Output: "r1"},
		"stage/0/m2": {Outcome: enginehost.OutcomePass, Output: "r2"},
	}}

	// Baseline (uninterrupted).
	base := newStore(t)
	want, err := engine.RunWithOptions(context.Background(), base, doc, map[string]any{"who": "world"}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}

	// Crash right after the first scatter member settles, then resume.
	resumed, store, stream := injectCrashThenResumeInput(t, doc, host, "stage/0/m1:0")
	if resumed.Outcome != want.Outcome || resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want %q (pass)", resumed.Outcome, want.Outcome)
	}
	// m1 reloaded by exact key (not re-run); m2 driven fresh.
	if resumed.NodeOutputs["stage/0/m1"] != "r1" || resumed.NodeOutputs["stage/0/m2"] != "r2" {
		t.Errorf("resumed sub outputs = {m1:%q m2:%q}, want {r1 r2}", resumed.NodeOutputs["stage/0/m1"], resumed.NodeOutputs["stage/0/m2"])
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestAdvanceRepeatRunBodyGuardPerAttemptRedecision (§2.7, the GIS marquee) proves a
// guard INSIDE a repeat-run-body re-decides fresh per attempt: attempt 0's guard decides
// TRUE (reuse defaulted "") and dispatches its then-do at stage/0/draft.then:0; that then
// FAILS → the guard settles failed → the attempt aggregate settles failed → the cond
// re-mints attempt 1 under the FRESH namespace stage/1/, whose guard RE-decides fresh and
// dispatches a DISTINCT write-once then activation stage/1/draft.then:0 with a fresh bead;
// it passes → the loop settles pass → seal. Pins distinct per-attempt then activations,
// two distinct dispatch facts, re-rendered attempt-1 prompt, and a byte-identical refold.
func TestAdvanceRepeatRunBodyGuardPerAttemptRedecision(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-gis-redecide"
	fake := newFakeWorkStore()
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "note", "who"),
			runCondPassOrIter()),
		sub))

	// Pass 1: attempt 0's guard decides TRUE and dispatches its then-do, park.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "stage/0/draft.then:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/0/draft.then:0", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/0/draft.then:0"); got != "greet world" {
		t.Fatalf("attempt-0 then prompt = %q, want %q", got, "greet world")
	}

	// Attempt 0's then FAILS → re-mint attempt 1, whose guard re-decides fresh.
	fake.settle("wb-1", engine.OutcomeFailed, "no")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "stage/1/draft.then:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with stage/1/draft.then:0 (per-attempt re-decision)", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/1/draft.then:0"); got != "greet world" {
		t.Fatalf("attempt-1 re-rendered then prompt = %q, want %q", got, "greet world")
	}

	// Attempt 1's then PASSES → loop settles pass → seal.
	fake.settle("wb-2", engine.OutcomePass, "done")
	r3, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (one fresh then-do per attempt)", fake.dispatchCount())
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["stage/0/draft"] != engine.OutcomeFailed || settled["stage/1/draft"] != engine.OutcomePass {
		t.Errorf("per-attempt guards = {0:%q 1:%q}, want {failed pass} (fresh transparent decision each)", settled["stage/0/draft"], settled["stage/1/draft"])
	}
	if settled["stage/0/draft.then"] != engine.OutcomeFailed || settled["stage/1/draft.then"] != engine.OutcomePass {
		t.Errorf("per-attempt then-dos = {0:%q 1:%q}, want {failed pass}", settled["stage/0/draft.then"], settled["stage/1/draft.then"])
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// --- for-each inside a repeat-run-body ATTEMPT namespace (FIS ⚑B2) -----------

// TestForEachInRunBodyAttemptRefFormInline (§2.8a) proves a for-each ref-over fans inside
// a repeat-run-body ATTEMPT namespace on the inline driver: the fan materializes fresh
// members at stage/0/fan/<i>, the attempt aggregate + loop settle pass in ONE attempt
// (the cond immediately passes). The ref-over resolves through scopeFor(stage/0/), which
// consults the parentNS override registered at mint time.
func TestForEachInRunBodyAttemptRefFormInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/0/fan/0"]; got != "a" {
		t.Errorf("attempt-0 member 0 = %q, want a (ref-over inside the attempt ns)", got)
	}
	if got := res.NodeOutputs["stage/0/fan/1"]; got != "b" {
		t.Errorf("attempt-0 member 1 = %q, want b", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/fan"] != engine.OutcomePass {
		t.Errorf("attempt-0 aggregate stage/0/fan = %q, want pass", settled["stage/0/fan"])
	}
	// The dynamic '/'-bearing member rows at attempt depth refold byte-identically.
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachInRunBodyAttemptMemberFormInline (§2.8b ⚑B2) is the pin that kills a
// structural-parentNamespace member arm: a member-over `input.arr` inside the ATTEMPT
// namespace must read the ATTEMPT env layer (via the parentNS override) and fan N members
// (NOT zero). A phantom structural parent (<bodyID>/) would collapse every binding to ""
// and silently fan zero.
func TestForEachInRunBodyAttemptMemberFormInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", memberOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/0/fan/0"]; got != "a" {
		t.Errorf("attempt-0 member 0 = %q, want a (member-over reads the attempt input layer, NOT a phantom parent)", got)
	}
	if got := res.NodeOutputs["stage/0/fan/1"]; got != "b" {
		t.Errorf("attempt-0 member 1 = %q, want b (structural parentNamespace would fan ZERO here)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/fan"] != engine.OutcomePass {
		t.Errorf("attempt-0 aggregate stage/0/fan = %q, want pass", settled["stage/0/fan"])
	}
	// The dynamic '/'-bearing member rows at attempt depth refold byte-identically.
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestAdvanceForEachInRunBodyAttemptMemberForm (§2.8b ⚑B2, advance) is the pool twin:
// a member-over inside the attempt ns dispatches N pool-do members (NOT zero) at
// stage/0/fan/<i>:0.
func TestAdvanceForEachInRunBodyAttemptMemberForm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", memberOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	res, err := engine.Advance(ctx, store, doc, "gcg-fis-attempt-member", map[string]any{"items": []any{"a", "b"}}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (2 attempt-ns members dispatched)", res)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count = %d, want 2 (member-over reads the attempt input layer, NOT a phantom parent)", fake.dispatchCount())
	}
	if got := fake.dispatchPromptFor(t, "stage/0/fan/0:0"); got != "review a" {
		t.Errorf("attempt member 0 prompt = %q, want %q", got, "review a")
	}
}

// TestAdvanceForEachInRunBodyAttemptRefForm (§2.8a advance — closes the §2.8 4-cell
// matrix) is the pool twin of the ref arm: a ref-over inside the attempt ns resolves
// through scopeFor under the parentNS override and dispatches N pool-do members at
// stage/0/fan/<i>:0 — the fast-suite tripwire for advance-path ref-arm-under-override
// regressions (the dolt e2e covers this cell only under the integration tag).
func TestAdvanceForEachInRunBodyAttemptRefForm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	res, err := engine.Advance(ctx, store, doc, "gcg-fis-attempt-ref", map[string]any{"items": []any{"a", "b"}}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (2 attempt-ns members dispatched)", res)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count = %d, want 2 (ref-over resolves the attempt view under the override)", fake.dispatchCount())
	}
	if got := fake.dispatchPromptFor(t, "stage/0/fan/0:0"); got != "review a" {
		t.Errorf("attempt member 0 prompt = %q, want %q", got, "review a")
	}
	if got := fake.dispatchPromptFor(t, "stage/0/fan/1:0"); got != "review b" {
		t.Errorf("attempt member 1 prompt = %q, want %q", got, "review b")
	}
}

// TestForEachInRunBodyPerAttemptRefanInline (the FIS marquee analog of the GIS
// per-attempt re-decision) proves a failing fan member in attempt 0 re-fans FRESH
// members in attempt 1 from the identically re-evaluated array: attempt 0's on_fail:stop
// fan fails → the attempt aggregate settles failed → the cond re-mints attempt 1 at
// stage/1/fan/<i> (distinct activations, same 2-element fan) → all pass → the loop exits
// pass. Byte-identical drop+refold over both attempts' dynamic member rows.
func TestForEachInRunBodyPerAttemptRefanInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "stop", refOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/0/fan/0": {Outcome: enginehost.OutcomeFailed, Output: "no"},
		"stage/0/fan/1": {Outcome: enginehost.OutcomePass, Output: "ok0"},
		"stage/1/fan/0": {Outcome: enginehost.OutcomePass, Output: "ok1a"},
		"stage/1/fan/1": {Outcome: enginehost.OutcomePass, Output: "ok1b"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"items": []any{"a", "b"}}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (per-attempt re-fan)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	// Attempt 0: member 0 failed → on_fail:stop fails the fan → the attempt fails.
	if settled["stage/0/fan/0"] != engine.OutcomeFailed || settled["stage/0/fan/1"] != engine.OutcomePass {
		t.Errorf("attempt-0 members = {0:%q 1:%q}, want {failed pass}", settled["stage/0/fan/0"], settled["stage/0/fan/1"])
	}
	if settled["stage/0/fan"] != engine.OutcomeFailed {
		t.Errorf("attempt-0 fan = %q, want failed (on_fail:stop)", settled["stage/0/fan"])
	}
	// Attempt 1: a FRESH fan at distinct activations, the SAME re-evaluated 2-element
	// array, all pass.
	if settled["stage/1/fan/0"] != engine.OutcomePass || settled["stage/1/fan/1"] != engine.OutcomePass {
		t.Errorf("attempt-1 members = {0:%q 1:%q}, want {pass pass} (fresh re-fan)", settled["stage/1/fan/0"], settled["stage/1/fan/1"])
	}
	if settled["stage/1/fan"] != engine.OutcomePass {
		t.Errorf("attempt-1 fan = %q, want pass", settled["stage/1/fan"])
	}
	// The re-fan is identical: exactly 2 members per attempt (no third, no zero).
	for _, id := range []string{"stage/0/fan/2", "stage/1/fan/2"} {
		if _, ok := settled[id]; ok {
			t.Errorf("unexpected third member %s settled (the re-fan must mint the identical 2-element array)", id)
		}
	}
	if _, _, _, out := loopSettle(t, res.Events, "loop:0"); out != "" {
		t.Errorf("loop settle output = %q, want \"\" (the passing attempt's transparent aggregate output)", out)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}
