package engine_test

import (
	"context"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- loop-in-sub-formula drive helpers (LIS slice) ---------------------------

// lisdNumField renders a required number-typed input field (the marquee budget shape).
func lisdNumField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"atomic","name":"number"},"required":true,"body":false}`
}

// lisdMarqueeCond builds `round.outcome == "pass" || iteration >= max_review_rounds` —
// the §2.1 marquee exit over the fixed "round" body ref and the TYPED number sub input.
func lisdMarqueeCond() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"round","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"ref","name":"max_review_rounds"}]}]}`
}

// lisdIterGECond builds `iteration >= <n>` with a literal bound (no outcome escape).
func lisdIterGECond(n int) string {
	return `{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":` + strconv.Itoa(n) + `}]}`
}

// settledActContains reports whether the settled-activation list carries (activation, outcome).
func settledActContains(pairs [][2]string, activation, outcome string) bool {
	for _, p := range pairs {
		if p[0] == activation && p[1] == outcome {
			return true
		}
	}
	return false
}

// settledActHas reports whether the settled-activation list carries the activation (any outcome).
func settledActHas(pairs [][2]string, activation string) bool {
	for _, p := range pairs {
		if p[0] == activation {
			return true
		}
	}
	return false
}

// TestRunLeafRepeatInSubRendersIteration pins §2.2 for the inline driver: a repeat LEAF
// loop inside a run sub-formula renders {{iteration}} in the attempt prompt at depth, and
// the numeric iteration cond drives the right attempt count (root parity). cond `iteration
// >= 2` runs attempts wrap/body:0 (iteration 1) and wrap/body:1 (iteration 2), then exits.
func TestRunLeafRepeatInSubRendersIteration(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	loop := repeatNode(execNode("body", `echo "round {{iteration}}"`, nil), lisdIterGECond(2))
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", loop),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	acts := settledActivations(t, res.Events)
	if !settledActHas(acts, "wrap/body:0") || !settledActHas(acts, "wrap/body:1") {
		t.Fatalf("attempts = %v, want wrap/body:0 (iteration 1) and wrap/body:1 (iteration 2)", acts)
	}
	if settledActHas(acts, "wrap/body:2") {
		t.Errorf("wrap/body:2 settled — the loop over-ran (cond `iteration >= 2` should exit at attempt 1)")
	}
	// {{iteration}} rendered at depth: the highest attempt's output is "round 2".
	if got := res.NodeOutputs["wrap/body"]; got != "round 2" {
		t.Errorf("wrap/body output = %q, want %q ({{iteration}} rendered in the sub namespace)", got, "round 2")
	}
	if _, _, _, out := loopSettle(t, res.Events, "wrap/repeat_1:0"); out != "round 2" {
		t.Errorf("loop settle output = %q, want the satisfying attempt's output round 2", out)
	}
}

// TestRunRunBodyLoopInSubCapsAtTypedBudget is the DRIVE-level lexicographic mutant killer
// (§2.1, two-digit budget): the marquee run-body loop inside a wrapper sub-formula whose
// inner do ALWAYS fails runs exactly 12 attempts (budget max_review_rounds=12, a TYPED
// number sub input) before `iteration >= max_review_rounds` exits — NOT 2, which a
// render-string lexicographic compare ("2" >= "12") would produce. Inline Run driver.
func TestRunRunBodyLoopInSubCapsAtTypedBudget(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// The wrapper run binds max_review_rounds to a literal 12 (two-digit); its number type
	// drives the typed re-type so the loop cond compares numerically.
	wrapEnv := `[{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}]`
	loop := repeatNode(
		runNodeJSON("round", nil, "inner", "name", "who"),
		lisdMarqueeCond())
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", strField("who")+","+lisdNumField("max_review_rounds"), loop)+","+
			subDoc("inner", strField("name"), execNodeExit("hello", "exit 3", []int{0}, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	acts := settledActivations(t, res.Events)
	// The 12th attempt (iteration 12, attempt 11) settled; a 13th never minted.
	if !settledActHas(acts, "wrap/round:11") {
		t.Fatalf("wrap/round:11 (the 12th attempt) did not settle; the loop exited EARLY — lexicographic bug? acts=%v", acts)
	}
	if settledActHas(acts, "wrap/round:12") {
		t.Errorf("wrap/round:12 settled — the loop over-ran past the typed budget 12")
	}
	if n := countAttemptMinted(res.Events); n != 12 {
		t.Errorf("attempt.minted count = %d, want 12 (numeric budget); a lexicographic compare would exit at 2", n)
	}
	// The loop settles failed (the last attempt's outcome), reason "" (the cond exited).
	if out, reason, _, _ := loopSettle(t, res.Events, "wrap/repeat_1:0"); out != engine.OutcomeFailed || reason != "" {
		t.Errorf("loop settle = (%q, reason %q), want (failed, \"\") — cond exit at the typed cap", out, reason)
	}
}

// TestAdvanceRunBodyLoopInSubReMintsAtDepth is the §3 e2e shape driven inline via the pool
// fake: the marquee run-body loop inside a wrapper sub-formula re-mints attempt 1 at the
// DEPTH prefix wrap/round/1/ after attempt 0's sub-do fails, the re-minted sub-do's prompt
// still renders its env-bound value (the ⚑B1 seam at depth), attempt 1 passes, and the run
// seals pass. Both attempt aggregates settle; zero control beads (a pure Lumen run).
func TestAdvanceRunBodyLoopInSubReMintsAtDepth(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-remint"
	fake := newFakeWorkStore()
	wrapEnv := `[{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}]`
	loop := repeatNode(
		runNodeJSON("round", nil, "inner", "name", "who"),
		lisdMarqueeCond())
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", strField("who")+","+lisdNumField("max_review_rounds"), loop)+","+
			subDoc("inner", strField("name"), doNode("hello", "greet {{ name }}", nil)),
	))

	// Pass 1: attempt 0's sub-do dispatches at the DEPTH prefix wrap/round/0/hello:0, park.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "wrap/round/0/hello:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with wrap/round/0/hello:0 (depth mint)", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "wrap/round/0/hello:0"); got != "greet world" {
		t.Fatalf("attempt-0 depth prompt = %q, want %q (env chained through the wrapper)", got, "greet world")
	}

	// Attempt 0 fails → re-mint attempt 1 at the FRESH depth prefix wrap/round/1/.
	fake.settle("wb-1", engine.OutcomeFailed, "no")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "wrap/round/1/hello:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with wrap/round/1/hello:0 (re-mint at depth)", r2, err)
	}
	// THE ⚑B1 depth pin: the re-minted attempt-1 prompt still renders the env-bound value.
	if got := fake.dispatchPromptFor(t, "wrap/round/1/hello:0"); got != "greet world" {
		t.Fatalf("attempt-1 depth prompt = %q, want %q (⚑B1 seam one namespace deeper)", got, "greet world")
	}

	// Attempt 1 passes → the aggregate passes, the loop settles pass, the run seals.
	fake.settle("wb-2", engine.OutcomePass, "hi world")
	r3, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed", r3, err)
	}
	if r3.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r3.Run.Outcome)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["wrap/round/0/hello"] != engine.OutcomeFailed {
		t.Errorf("attempt-0 sub-do = %q, want failed", settled["wrap/round/0/hello"])
	}
	if settled["wrap/round/1/hello"] != engine.OutcomePass {
		t.Errorf("attempt-1 sub-do = %q, want pass", settled["wrap/round/1/hello"])
	}
	if settled["wrap/round"] != engine.OutcomePass {
		t.Errorf("round final settle = %q, want pass (highest attempt)", settled["wrap/round"])
	}
	// Two dispatches, one fresh bead per attempt.
	if fake.dispatchCount() != 2 {
		t.Errorf("DispatchWork calls = %d, want 2 (one sub-do per attempt)", fake.dispatchCount())
	}
	// The live projection drop+refolds byte-identically (no hidden reducer state, v4).
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestRunLeafLoopInScatterInSubDrives pins §2.6 (Q-A): a LEAF loop that is a scatter member
// INSIDE a run sub-formula both lowers AND drives — the loop attempts run under the doubly
// nested namespace, and the sub-scatter aggregates the settled loop. Inline Run driver.
func TestRunLeafLoopInScatterInSubDrives(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	member := repeatNode(execNode("body", `echo "r {{iteration}}"`, nil), lisdIterGECond(2))
	sc := scatterNode("sc", nil, "continue", member)
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", sc),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	acts := settledActivations(t, res.Events)
	if !settledActHas(acts, "wrap/body:0") || !settledActHas(acts, "wrap/body:1") {
		t.Fatalf("loop-in-scatter-in-sub did not drive its attempts; acts=%v", acts)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["wrap/sc"] != engine.OutcomePass {
		t.Errorf("sub-scatter wrap/sc = %q, want pass (it aggregates the settled loop)", settled["wrap/sc"])
	}
}

// TestRunLoopDownstreamRefRendersAtDepth pins §2.9: a ns sibling gated on the loop node
// renders {{loopID}} — settleLoop records the loop output at its QUALIFIED id, which
// scopeFor exposes bare inside the namespace, so a same-namespace `{{repeat_1}}` resolves
// the satisfying attempt's output.
func TestRunLoopDownstreamRefRendersAtDepth(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	loop := repeatNode(execNode("body", `echo "r {{iteration}}"`, nil), lisdIterGECond(1))
	after := execNode("tail", `echo "loop said {{repeat_1}}"`, []string{"repeat_1"})
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", loop, after),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["wrap/tail"]; got != "loop said r 1" {
		t.Errorf("wrap/tail = %q, want %q (downstream {{loopID}} render at depth)", got, "loop said r 1")
	}
}

// TestRunRetryLeafLoopInSubAttemptsTyped pins §2.2 for retry at depth: a retry loop inside
// a sub-formula whose `attempts` reads a TYPED number sub input evaluates to the integer
// budget (NOT invalid_input, which a render-string would force) — so the body actually runs
// its budget of attempts. attempts=3 over a retryable failure runs wrap/body:0,1,2.
func TestRunRetryLeafLoopInSubAttemptsTyped(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// retry { attempts: n } where n is a number sub input bound to 3; body fails RETRYABLY.
	retry := `{"kind":"retry","id":"attempt","name":"attempt","after":[],` +
		`"attempts":{"kind":"ref","name":"n"},` +
		`"body":` + execNodeExit("body", "exit 3", []int{0}, []int{3}) + `}`
	wrapEnv := `[{"name":"n","value":{"kind":"expr","expr":{"kind":"literal","value":3}}}]`
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", lisdNumField("n"), retry),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	acts := settledActivations(t, res.Events)
	for _, want := range []string{"wrap/body:0", "wrap/body:1", "wrap/body:2"} {
		if !settledActContains(acts, want, engine.OutcomeFailed) {
			t.Fatalf("attempt %s did not run failed; the typed budget 3 was not honored (invalid_input?) acts=%v", want, acts)
		}
	}
	// The loop settled failed{exhausted} — the budget was consumed, not invalid_input.
	if out, reason, _, _ := loopSettle(t, res.Events, "wrap/attempt:0"); out != engine.OutcomeFailed || reason != "exhausted" {
		t.Errorf("retry settle = (%q, %q), want (failed, exhausted) — proves attempts read the typed number, not invalid_input", out, reason)
	}
}

// TestRunDepth2RunBodyLoopInsideMintedAttemptSeals pins §2.9 depth-2 nesting AND the
// runtime leg of the mintRunBody inputNames pin: an OUTER repeat-run-body (stage -> mid)
// whose MINTED attempt namespace contains ANOTHER run-body repeat (rounds { run round ->
// leaf }) reading MID's typed sub input — the fresh-lowerer inputNames + nested override
// chain. Inline Run: outer attempt 0 mints stage/0/, the inner loop mints
// stage/0/round/0/, the leaf passes, both loops exit, the run seals pass.
func TestRunDepth2RunBodyLoopInsideMintedAttemptSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	innerLoop := `{"kind":"repeat","id":"rounds","name":"rounds","after":[],"iterationName":"iteration",` +
		`"cond":` + lisdMarqueeCond() + `,` +
		`"body":` + runNodeJSON("round", nil, "leaf", "name", "who") + `}`
	midEnv := `[{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}]`
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "mid", midEnv),
			runCondPassOrIter()),
		subDoc("mid", strField("who")+","+lisdNumField("max_review_rounds"), innerLoop)+","+
			subDoc("leaf", strField("name"), execNode("hello", `echo "hi {{ name }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run (depth-2 nesting): %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// The doubly-minted leaf ran at depth 2 with the env chained through both mints.
	if got := res.NodeOutputs["stage/0/round/0/hello"]; got != "hi world" {
		t.Errorf("stage/0/round/0/hello = %q, want %q (depth-2 env chain)", got, "hi world")
	}
	// The INNER loop settled pass inside the minted attempt namespace.
	if out, _, _, _ := loopSettle(t, res.Events, "stage/0/rounds:0"); out != engine.OutcomePass {
		t.Errorf("inner loop stage/0/rounds settle = %q, want pass", out)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestAdvanceLeafRepeatInSubRendersIterationPool is the POOL-driver twin of the inline
// iteration-render pin (the materializeLoopAttempt iterKey mutant killer): a do-body
// repeat inside a run sub-formula DISPATCHES its attempt with {{iteration}} resolved in
// the rendered prompt — the iteration seed must be namespace-qualified or the depth
// render silently degrades while root stays green.
func TestAdvanceLeafRepeatInSubRendersIterationPool(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-iterpool"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", repeatNode(doNode("body", "round {{iteration}}", nil), lisdIterGECond(1))),
	))

	// Pass 1: attempt 0 dispatches at wrap/body:0 with the iteration RENDERED.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "wrap/body:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with wrap/body:0", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "wrap/body:0"); got != "round 1" {
		t.Fatalf("dispatched attempt prompt = %q, want %q ({{iteration}} must render at depth on the POOL path)", got, "round 1")
	}

	// Attempt 0 passes -> cond (iteration >= 1) exits -> seal.
	fake.settle("wb-1", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
	}
}

// TestAdvanceRetryInSubAttemptsTypedPool is the POOL-driver twin of the typed-attempts
// pin (the advanceLoop evalAttempts scope mutant killer): a do-body retry inside a run
// sub-formula whose `attempts` reads a TYPED number sub input DISPATCHES attempt 0 —
// with the root flat scope the ref is a miss, evalAttempts yields invalid_input, and the
// loop silently seals failed with ZERO dispatches.
func TestAdvanceRetryInSubAttemptsTypedPool(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-retrypool"
	fake := newFakeWorkStore()
	retry := `{"kind":"retry","id":"attempt","name":"attempt","after":[],` +
		`"attempts":{"kind":"ref","name":"n"},` +
		`"body":` + doNode("body", "try the thing", nil) + `}`
	wrapEnv := `[{"name":"n","value":{"kind":"expr","expr":{"kind":"literal","value":3}}}]`
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", lisdNumField("n"), retry),
	))

	// Pass 1: the typed budget evaluates (3) and attempt 0 DISPATCHES — not invalid_input.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "wrap/body:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with wrap/body:0 (typed attempts at depth; invalid_input would seal with zero dispatches)", r1, err)
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls = %d, want 1", fake.dispatchCount())
	}

	// Attempt 0 passes -> retry settles pass -> seal.
	fake.settle("wb-1", engine.OutcomePass, "ok")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
	}
}

// TestAdvanceLeafLoopInScatterInSubDrives is the POOL side of Q-A (§2.6): a do-body
// repeat that is a scatter member INSIDE a run sub-formula dispatches its attempt as
// claimable pool work at depth, settles, and the sub-scatter aggregates the settled loop.
func TestAdvanceLeafLoopInScatterInSubDrives(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-scatterpool"
	fake := newFakeWorkStore()
	member := repeatNode(doNode("body", "scattered round {{iteration}}", nil), lisdIterGECond(1))
	sc := scatterNode("sc", nil, "continue", member)
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", sc),
	))

	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "wrap/body:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with wrap/body:0 (loop-in-scatter-in-sub pool drive)", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "wrap/body:0"); got != "scattered round 1" {
		t.Fatalf("scatter-member attempt prompt = %q, want %q", got, "scattered round 1")
	}
	fake.settle("wb-1", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["wrap/sc"] != engine.OutcomePass {
		t.Errorf("sub-scatter wrap/sc = %q, want pass (aggregated the settled loop)", settled["wrap/sc"])
	}
}

// TestRunRunBodyLoopInSubMidAttemptCrashResumes pins §2.8 crash-mid-attempt AT DEPTH: the
// run-body loop lives inside a wrapper sub-formula, its attempt-0 mint is a scatter with
// two members, and the crash fires right after the FIRST member settles (inside
// wrap/stage/0/). The resume reloads the settled member by exact key, drives the other,
// seals to the baseline outcome, and the projection drop+refolds byte-identically.
func TestRunRunBodyLoopInSubMidAttemptCrashResumes(t *testing.T) {
	subScatter := scatterNode("lanes", nil, "continue",
		doNode("m1", "m1 for {{ name }}", nil),
		doNode("m2", "m2 for {{ name }}", nil))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("wrap", nil, "wrapper", "who", "who"),
		subDoc("wrapper", strField("who"),
			repeatRunLoop(nil,
				runNodeJSON("stage", nil, "greeter", "name", "who"),
				runCondPassOrIter()))+","+
			subDoc("greeter", strField("name"), subScatter),
	))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"wrap/stage/0/m1": {Outcome: enginehost.OutcomePass, Output: "r1"},
		"wrap/stage/0/m2": {Outcome: enginehost.OutcomePass, Output: "r2"},
	}}

	// Baseline (uninterrupted).
	base := newStore(t)
	want, err := engine.RunWithOptions(context.Background(), base, doc, map[string]any{"who": "world"}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}

	// Crash right after the first depth scatter member settles, then resume.
	resumed, store, stream := injectCrashThenResumeInput(t, doc, host, "wrap/stage/0/m1:0")
	if resumed.Outcome != want.Outcome || resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want %q (pass)", resumed.Outcome, want.Outcome)
	}
	// m1 reloaded by exact depth key (not re-run); m2 driven fresh.
	if resumed.NodeOutputs["wrap/stage/0/m1"] != "r1" || resumed.NodeOutputs["wrap/stage/0/m2"] != "r2" {
		t.Errorf("resumed sub outputs = {m1:%q m2:%q}, want {r1 r2}",
			resumed.NodeOutputs["wrap/stage/0/m1"], resumed.NodeOutputs["wrap/stage/0/m2"])
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestLoopInSubInlinePoolJournalParityLeaf pins §2.8 journal parity AT DEPTH for the LEAF
// attempt branch: an exec-body repeat inside a run sub-formula (two deterministic
// attempts via `iteration >= 2`) seals in one pass on BOTH drivers, and the journals
// after run.started are byte-identical (type + canonical payload, in order).
func TestLoopInSubInlinePoolJournalParityLeaf(t *testing.T) {
	ctx := context.Background()
	docJSON := bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", "", repeatNode(execNode("body", `echo hi`, nil), lisdIterGECond(2))),
	)

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, decodeIR(t, docJSON), nil)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if inRes.Outcome != engine.OutcomePass {
		t.Fatalf("inline outcome = %q, want pass", inRes.Outcome)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, decodeIR(t, docJSON), "gcg-lis-parity", nil, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec attempts run inline)", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (exec attempts never dispatch)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestAdvanceRunBodyLoopInSubWriteOnceRedundant pins §2.8 write-once re-mint AT DEPTH: a
// redundant Advance with NO new settlement over an in-flight depth attempt is a pure
// no-op — the head does not move (no double append; the depth re-mint + attempt.minted
// idem keys carry the full prefix) and no second bead dispatches.
func TestAdvanceRunBodyLoopInSubWriteOnceRedundant(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-writeonce"
	fake := newFakeWorkStore()
	wrapEnv := `[{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}]`
	loop := repeatNode(
		runNodeJSON("round", nil, "inner", "name", "who"),
		lisdMarqueeCond())
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", strField("who")+","+lisdNumField("max_review_rounds"), loop)+","+
			subDoc("inner", strField("name"), doNode("hello", "greet {{ name }}", nil)),
	))

	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || r1.InFlight[0].Activation != "wrap/round/0/hello:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with wrap/round/0/hello:0", r1, err)
	}
	headAfter1 := r1.Head
	// Two redundant Advances with no settlement: each must be a pure no-op at depth.
	for i := 0; i < 2; i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
		if err != nil || !r.Parked {
			t.Fatalf("redundant advance %d = %+v err %v, want Parked", i, r, err)
		}
		if r.Head != headAfter1 {
			t.Fatalf("redundant advance %d moved the head %d -> %d (a double append at depth)", i, headAfter1, r.Head)
		}
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls = %d across redundant Advances, want 1 (write-once at depth)", fake.dispatchCount())
	}
}

// TestAdvanceRunBodyLoopInSubLoopCapAtDepth pins §2.9's hard-cap arm AT DEPTH: a run-body
// loop inside a wrapper whose cond never matches drives to lumenRepeatLoopCap — each
// attempt's sub-formula RUNS in a fresh depth namespace (wrap/stage/0/s … wrap/stage/31/s)
// and the loop settles failed{loop_cap}, bounded.
func TestAdvanceRunBodyLoopInSubLoopCapAtDepth(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-loopcap"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("wrap", nil, "wrapper", "who", "who"),
		subDoc("wrapper", strField("who"),
			repeatRunLoop(nil,
				runNodeJSON("stage", nil, "greeter", "name", "who"),
				runCondOutcomeEq("never")))+","+ // impossible; no iteration escape
			subDoc("greeter", strField("name"), settleNode("s", "pass")),
	))
	res, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("advance = %+v, want Sealed failed (loop cap at depth)", res)
	}
	out, reason, _, _ := loopSettle(t, res.Run.Events, "wrap/loop:0")
	if reason != "loop_cap" {
		t.Errorf("loop settle reason = %q (out %q), want loop_cap", reason, out)
	}
	// Each attempt got a fresh DEPTH namespace: attempt 0 and the last attempt both settled.
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["wrap/stage/0/s"] != engine.OutcomePass || settled["wrap/stage/31/s"] != engine.OutcomePass {
		t.Errorf("want fresh depth namespaces wrap/stage/0/s and wrap/stage/31/s both settled pass; got 0=%q 31=%q",
			settled["wrap/stage/0/s"], settled["wrap/stage/31/s"])
	}
}

// TestAdvanceRunBodyLoopInSubAllDidNotRunSpins pins §2.9's all-didNotRun behavior AT
// DEPTH: the attempt sub-formula's sole member settle-CANCELS, the attempt aggregate
// settles SKIPPED, the cond (outcome == "pass") never matches, and the loop re-mints
// identical skipped attempts to the cap — deterministic and bounded one namespace deeper.
func TestAdvanceRunBodyLoopInSubAllDidNotRunSpins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-lis-spin"
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("wrap", nil, "wrapper", "who", "who"),
		subDoc("wrapper", strField("who"),
			repeatRunLoop(nil,
				runNodeJSON("stage", nil, "greeter", "name", "who"),
				runCondOutcomeEq("pass")))+","+ // a skipped aggregate never matches
			subDoc("greeter", strField("name"), settleNode("s", "canceled")),
	))
	res, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("advance = %+v, want Sealed failed (bounded spin -> loop cap at depth)", res)
	}
	if _, reason, _, _ := loopSettle(t, res.Run.Events, "wrap/loop:0"); reason != "loop_cap" {
		t.Errorf("loop settle reason = %q, want loop_cap (bounded)", reason)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["wrap/stage"] != engine.OutcomeSkipped {
		t.Errorf("attempt aggregate wrap/stage = %q, want skipped (all-didNotRun at depth)", settled["wrap/stage"])
	}
}

// TestRunRunBodyLoopInSubRefBoundBudget pins the corpus REF-BOUND budget chain
// (`max_review_rounds = max_review_rounds`): the wrapper's number sub input is bound via a
// REF to the main document's same-named number input — retypeScalar re-types the PARENT
// RENDER of a real input value (baseScope stringification), not a literal — and the
// always-failing marquee runs exactly the two-digit budget of attempts.
func TestRunRunBodyLoopInSubRefBoundBudget(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	wrapEnv := `[{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"ref","name":"max_review_rounds"}}}]`
	loop := repeatNode(
		runNodeJSON("round", nil, "inner", "name", "who"),
		lisdMarqueeCond())
	doc := decodeIR(t, bundleDoc(
		strField("who")+","+lisdNumField("max_review_rounds"),
		runNodeRawEnv("wrap", nil, "wrapper", wrapEnv),
		subDoc("wrapper", strField("who")+","+lisdNumField("max_review_rounds"), loop)+","+
			subDoc("inner", strField("name"), execNodeExit("hello", "exit 3", []int{0}, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world", "max_review_rounds": float64(12)})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countAttemptMinted(res.Events); n != 12 {
		t.Errorf("attempt.minted count = %d, want 12 (ref-bound two-digit budget re-typed from the parent render)", n)
	}
	if out, reason, _, _ := loopSettle(t, res.Events, "wrap/repeat_1:0"); out != engine.OutcomeFailed || reason != "" {
		t.Errorf("loop settle = (%q, reason %q), want (failed, \"\") — the cond exited at the ref-bound cap", out, reason)
	}
}
