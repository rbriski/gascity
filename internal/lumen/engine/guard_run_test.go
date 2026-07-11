package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- guard-in-sub-formula (GIS) black-box behavioral fixtures ----------------

// defaultReuseField renders the OPTIONAL `reuse: string = ""` sub-input field —
// the corpus shape a guard cond decides against when unbound.
func defaultReuseField() string {
	return `{"name":"reuse","type":{"kind":"atomic","name":"string"},"required":false,"default":"","body":false}`
}

// condEqualRaw builds a closed cond `<ref> == <litJSON>` where litJSON is the raw
// JSON of the literal (`""`, `"seeded"`, `true`), so both string and bool RHS shapes
// are expressible (the corpus is all ==/!=).
func condEqualRaw(ref, litJSON string) string {
	return `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"` + ref + `"},{"kind":"literal","value":` + litJSON + `}]}`
}

// condOutcomePass builds `<ref>.outcome == "pass"` — a guard cond reading a
// sibling's settled outcome.
func condOutcomePass(ref string) string {
	return `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"` + ref + `","field":"outcome"},{"kind":"literal","value":"pass"}]}`
}

// guardExecAfter renders a guard node (explicit id + after) with a closed cond and an
// exec `then` (runs inline, no host needed).
func guardExecAfter(id string, after []string, cond, thenID, thenScript string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"guard","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"cond":` + cond + `,"then":` + execNode(thenID, thenScript, nil) + `}`
}

// guardDoID renders an ungated guard node (explicit id) whose then is a pool-materializable do.
func guardDoID(id, cond, thenID, thenPrompt string) string {
	return `{"kind":"guard","id":"` + id + `","name":"` + id + `","after":[]` +
		`,"cond":` + cond + `,"then":` + doNode(thenID, thenPrompt, nil) + `}`
}

// TestGuardInSubFormulaDefaultCondTrueRunsThen (§2.1) proves the corpus shape: a run
// of a sub-formula whose body is `draft: guard (reuse == "") -> do` with reuse
// UNBOUND -> defaulted "" -> cond TRUE -> the then runs, rendered against the sub-scope
// (note <- who), and the guard settles transparently from it.
func TestGuardInSubFormulaDefaultCondTrueRunsThen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardExecAfter("draft", nil, condEqualRaw("reuse", `""`), "draft.then", `echo "drafted {{ note }}"`))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/draft.then"]; got != "drafted world" {
		t.Errorf("then output stage/draft.then = %q, want %q (sub-scope render)", got, "drafted world")
	}
	if got := res.NodeOutputs["stage/draft"]; got != "drafted world" {
		t.Errorf("guard output stage/draft = %q, want %q (transparent from then)", got, "drafted world")
	}
}

// TestGuardInSubFormulaEnvBoundCondFalseNoOp (§2.2) proves an env-bound FALSE branch:
// `given { reuse: "seeded" }` -> cond FALSE -> the guard settles PASS "no branch
// taken", its then never runs, and a downstream node still runs (no skip-cascade).
func TestGuardInSubFormulaEnvBoundCondFalseNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	envFields := `[` +
		`{"name":"note","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"reuse","value":{"kind":"expr","expr":{"kind":"literal","value":"seeded"}}}]`
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardExecAfter("draft", nil, condEqualRaw("reuse", `""`), "draft.then", `echo "drafted {{ note }}"`))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "greeter", envFields)+","+
			execNode("after", `echo done`, []string{"stage"}),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass (a false guard passes)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/draft"] != engine.OutcomePass {
		t.Errorf("guard stage/draft settled %q, want pass", settled["stage/draft"])
	}
	if got := res.NodeOutputs["stage/draft.then"]; got != "" {
		t.Errorf("then output = %q, want empty (false cond runs no then)", got)
	}
	if settled["after"] != engine.OutcomePass {
		t.Errorf("downstream after = %q, want pass (guard no-op does not skip-cascade)", settled["after"])
	}
}

// TestGuardInSubFormulaCoercion (§2.3) proves the ==/coercion rows inside ns: a guard
// cond `push == true` over an env-bound STRING "true" is truthy (String-coerce) and
// over "false" is falsy — the then runs iff the bound string coerces equal to true.
func TestGuardInSubFormulaCoercion(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		push    string
		thenRan bool
	}{
		{"true", true},
		{"false", false},
	} {
		t.Run(tc.push, func(t *testing.T) {
			store := newStore(t)
			envFields := `[` +
				`{"name":"note","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
				`{"name":"push","value":{"kind":"expr","expr":{"kind":"literal","value":"` + tc.push + `"}}}]`
			sub := subDoc("greeter", strField("note")+","+strField("push"),
				guardExecAfter("draft", nil, condEqualRaw("push", `true`), "draft.then", `echo "pushed {{ note }}"`))
			doc := decodeIR(t, bundleDoc(
				strField("who"),
				runNodeRawEnv("stage", nil, "greeter", envFields),
				sub))
			res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			ran := res.NodeOutputs["stage/draft.then"] != ""
			if ran != tc.thenRan {
				t.Errorf("push=%q then ran=%v, want %v (coercion: \"true\"==true equal, \"false\"==true not)", tc.push, ran, tc.thenRan)
			}
		})
	}
}

// TestGuardInSubFormulaAggregateOutcomeRef (§2.4 ⚑B1, behavioral) proves a guard cond
// reading a SUB-SCATTER sibling's `.outcome == "pass"` resolves TRUE inside ns and runs
// the then. The scatter aggregate output is nodeOutputs-only (never in scope), so
// without the ⚑B1 flat-nodeOutputs overlay + fold-walk the ref would resolve null and
// the branch would silently NOT run.
func TestGuardInSubFormulaAggregateOutcomeRef(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	subScatter := scatterNode("sc", nil, "continue",
		execNode("a", `echo a`, nil),
		execNode("b", `echo b`, nil))
	// Two guards over the sub-scatter: draft reads its .outcome, bare reads its BARE
	// value (the empty aggregate output) — both ⚑B1 rows, behaviorally.
	sub := subDoc("greeter", strField("note"),
		subScatter+","+
			guardExecAfter("draft", []string{"sc"}, condOutcomePass("sc"),
				"draft.then", `echo "drafted {{ note }}"`)+","+
			guardExecAfter("bare", []string{"sc"}, condEqualRaw("sc", `""`),
				"bare.then", `echo "bare ran"`))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/draft.then"]; got != "drafted world" {
		t.Errorf("then output = %q, want %q (⚑B1: sub-scatter .outcome==pass visible inside ns)", got, "drafted world")
	}
	// Root/ns parity for the aggregate's BARE ref: the drained scatter's transparent
	// output is "" (nodeOutputs-only), so `sc == ""` is TRUE inside ns too.
	if got := res.NodeOutputs["stage/bare.then"]; got != "bare ran" {
		t.Errorf("bare-value then output = %q, want %q (⚑B1: aggregate bare value == \"\" inside ns)", got, "bare ran")
	}
}

// TestGuardInSubFormulaDropRefoldByteIdentity pins DET for a guard-in-sub-formula: the
// live projection equals a from-scratch drop+refold across both branches (cond true and
// false), so the namespace-aware decision folds no hidden reducer state (reducerVersion
// stays 4).
func TestGuardInSubFormulaDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	for _, reuse := range []string{"", "seeded"} { // "" -> then runs; "seeded" -> no-op
		store := newStore(t)
		envFields := `[` +
			`{"name":"note","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
			`{"name":"reuse","value":{"kind":"expr","expr":{"kind":"literal","value":"` + reuse + `"}}}]`
		sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
			guardExecAfter("draft", nil, condEqualRaw("reuse", `""`), "draft.then", `echo "drafted {{ note }}"`))
		doc := decodeIR(t, bundleDoc(
			strField("who"),
			runNodeRawEnv("stage", nil, "greeter", envFields),
			sub))
		res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
		if err != nil {
			t.Fatalf("run(reuse=%q): %v", reuse, err)
		}
		assertProjectionEqualsRefold(t, store, res.StreamID)
	}
}

// TestAdvanceGuardInSubFormulaFalseSeals (§2.2 advance) proves the Advance write-once
// arm settles a false guard-in-sub-formula PASS and seals with no then materialized.
func TestAdvanceGuardInSubFormulaFalseSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	envFields := `[` +
		`{"name":"note","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"reuse","value":{"kind":"expr","expr":{"kind":"literal","value":"seeded"}}}]`
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "greeter", envFields),
		sub))
	res, err := engine.Advance(ctx, store, doc, "gcg-gis-false", map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance = %+v, want Sealed pass (false guard is a no-op)", res)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("DispatchWork calls = %d, want 0 (false guard dispatches no then)", fake.dispatchCount())
	}
}

// TestAdvanceGuardInSubFormulaTrueDoParks (§2.1 advance/pool) proves a TRUE guard-in-
// sub-formula whose then is a do materializes the then as pool work in the plain
// (non-attempt) run namespace at stage/draft.then:0 and parks (rendered against the
// sub-scope).
func TestAdvanceGuardInSubFormulaTrueDoParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	res, err := engine.Advance(ctx, store, doc, "gcg-gis-true", map[string]any{"who": "world"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (the true guard's do then is dispatched)", res)
	}
	if len(res.InFlight) != 1 || res.InFlight[0].Activation != "stage/draft.then:0" {
		t.Fatalf("InFlight = %+v, want the namespaced then stage/draft.then:0", res.InFlight)
	}
	if got := fake.dispatchPromptFor(t, "stage/draft.then:0"); got != "greet world" {
		t.Errorf("then prompt = %q, want %q (rendered against the sub-scope)", got, "greet world")
	}
}

// TestGuardInScatterSiblingCondRefSkipCascadesRoot (§2.9b ⚑S6, root) proves a guard
// scatter member whose cond reads a SIBLING member installs a gate edge (a sequenced
// decision the code blesses), and a FAILED sibling skip-cascades the guard exactly like
// any blocking `after` dep — the guard settles skipped, its then never runs.
func TestGuardInScatterSiblingCondRefSkipCascadesRoot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("gsc",
		scatterNode("lanes", nil, "continue",
			execNode("bad", `exit 2`, nil),
			guardExecAfter("g", nil, condOutcomePass("bad"), "gthen", `echo t`))))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["bad"] != engine.OutcomeFailed {
		t.Errorf("sibling bad = %q, want failed", settled["bad"])
	}
	if settled["g"] != engine.OutcomeSkipped {
		t.Errorf("guard g = %q, want skipped (failed sibling skip-cascades the gated guard)", settled["g"])
	}
	if got := res.NodeOutputs["gthen"]; got != "" {
		t.Errorf("guard then output = %q, want empty (skip-cascaded guard runs no then)", got)
	}
	// The drain: a failed member dominates → the scatter aggregate settles failed
	// (the skipped guard contributes nothing) and the run reports it.
	if settled["lanes"] != engine.OutcomeFailed {
		t.Errorf("scatter aggregate lanes = %q, want failed (failed member dominates the drain)", settled["lanes"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
}

// TestGuardInScatterSiblingCondRefSkipCascadesNS (§2.9b ⚑S6, ns) is the same sibling-ref
// skip-cascade INSIDE a run sub-formula: a failed sub-scatter member skip-cascades the
// guarded member at its qualified id (stage/g), and the run seals with the honest
// transparent outcome of the members that ran.
func TestGuardInScatterSiblingCondRefSkipCascadesNS(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("greeter", strField("note"),
		scatterNode("lanes", nil, "continue",
			execNode("bad", `exit 2`, nil),
			guardExecAfter("g", nil, condOutcomePass("bad"), "gthen", `echo t`)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/bad"] != engine.OutcomeFailed {
		t.Errorf("sub-sibling stage/bad = %q, want failed", settled["stage/bad"])
	}
	if settled["stage/g"] != engine.OutcomeSkipped {
		t.Errorf("sub-guard stage/g = %q, want skipped (failed sub-sibling skip-cascades at ns)", settled["stage/g"])
	}
	if got := res.NodeOutputs["stage/gthen"]; got != "" {
		t.Errorf("sub-guard then output = %q, want empty", got)
	}
	// The drain + the transparent chain: failed member → sub-scatter failed → the run
	// aggregate reports it transparently → run.closed failed.
	if settled["stage/lanes"] != engine.OutcomeFailed {
		t.Errorf("sub-scatter stage/lanes = %q, want failed (failed member dominates the drain)", settled["stage/lanes"])
	}
	if settled["stage"] != engine.OutcomeFailed {
		t.Errorf("run aggregate stage = %q, want failed (transparent from the failed drain)", settled["stage"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
}

// TestRunGuardInSubFormulaCrashResumeConverges (§2.10b, inline Resume) proves the inline
// driver re-evaluates a guard-in-sub-formula cond on resume and provably converges: a
// crash right after the guard's then settles resumes, re-decides the (unchanged) cond,
// reloads the then via resumeMemoized exactly once (no duplicate activation), settles the
// guard transparently, plumbs its output to a downstream sub-sibling, and drop+refolds
// byte-identically. It also exercises the resume.go scope-seed masking (a reloaded guard's
// scope value comes from reconstructOutputs, not the resumeMemoized list).
func TestRunGuardInSubFormulaCrashResumeConverges(t *testing.T) {
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}")+","+
			doNode("echo", "post {{ draft }}", []string{"draft"}))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/draft.then": {Outcome: enginehost.OutcomePass, Output: "greeted"},
		"stage/echo":       {Outcome: enginehost.OutcomePass, Output: "posted"},
	}}

	// Baseline (uninterrupted).
	base := newStore(t)
	want, err := engine.RunWithOptions(context.Background(), base, doc, map[string]any{"who": "world"}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	if want.Outcome != engine.OutcomePass {
		t.Fatalf("baseline outcome = %q, want pass", want.Outcome)
	}

	// Crash right after the guard's then settles, then resume.
	resumed, store, stream := injectCrashThenResumeInput(t, doc, host, "stage/draft.then:0")
	if resumed.Outcome != want.Outcome || resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want %q (pass — guard re-decides and converges)", resumed.Outcome, want.Outcome)
	}
	// The guard settled transparently from its then; the downstream sub-sibling read it.
	if resumed.NodeOutputs["stage/draft"] != "greeted" {
		t.Errorf("resumed guard output stage/draft = %q, want greeted (transparent from then)", resumed.NodeOutputs["stage/draft"])
	}
	if resumed.NodeOutputs["stage/echo"] != "posted" {
		t.Errorf("resumed downstream stage/echo = %q, want posted (guard output plumbed on resume)", resumed.NodeOutputs["stage/echo"])
	}
	// "Reloaded exactly ONCE": the settled then must NOT be re-run on resume. The host
	// saw exactly 4 invocations — baseline 2 (draft.then, echo) + crashed run 1
	// (draft.then, crash fires after its settle) + resume 1 (echo only; draft.then is
	// reloaded via resumeMemoized, never re-invoked).
	if calls := host.Calls(); len(calls) != 4 {
		ids := make([]string, len(calls))
		for i, c := range calls {
			ids[i] = c.NodeID
		}
		t.Errorf("host calls = %d (%v), want 4 (settled then reloaded, not re-run)", len(calls), ids)
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestAdvanceGuardWriteOnceNoReEvalOnGrownFold (§2.10a, the write-once pin) proves the
// Advance arm does NOT re-evaluate a guard cond once the then is activated. The only
// public-API vector that can change a decided cond's value between passes is a GROWN
// FOLD (input is hash-checked on re-Advance; node cond-refs are gate-frozen; synth-body
// refs are refused at lowering), so the test grows it adversely: after the guard decides
// TRUE on `probe.outcome == "pass"` and dispatches its then, a higher-attempt settle for
// probe (probe:1, failed — the observer-appended shape SettleWorkForTest mirrors) flips
// what a RE-evaluation would read (max-attempt-wins). The decision must hold: the next
// Advance stays parked on the then (never settleDecisionSkipped over a live bead), the
// dispatch count stays put, and the guard finally settles TRANSPARENTLY from the then.
// Deleting the advance write-once arm re-evaluates the cond FALSE and fails this test.
func TestAdvanceGuardWriteOnceNoReEvalOnGrownFold(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-gis-writeonce"
	fake := newFakeWorkStore()
	sub := subDoc("greeter", strField("note"),
		doNode("probe", "probe {{ note }}", nil)+","+
			guardDoID("g", condOutcomePass("probe"), "gthen", "gated {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))

	// Pass 1: probe dispatches; the guard is gate-frozen behind it.
	r1, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "stage/probe:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/probe:0", r1, err)
	}
	// probe passes; pass 2 decides TRUE and dispatches the then (the write-once record).
	fake.settleAct(t, "stage/probe:0", engine.OutcomePass, "ok")
	r2, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "stage/gthen:0" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with the then stage/gthen:0", r2, err)
	}

	// Grow the fold adversely: probe:1 settles FAILED (a cooperative observer append),
	// so a cond RE-evaluation would now read probe.outcome == "failed" → FALSE.
	if err := engine.SettleWorkForTest(ctx, store, streamID, "stage/probe:1", engine.OutcomeFailed, "flip"); err != nil {
		t.Fatalf("settle probe:1: %v", err)
	}

	// Pass 3: the decision must HOLD — still parked on the then, no re-dispatch, and
	// above all no settleDecisionSkipped over the live then bead.
	r3, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r3.Parked || len(r3.InFlight) != 1 || r3.InFlight[0].Activation != "stage/gthen:0" {
		t.Fatalf("advance 3 = %+v err %v, want still Parked on stage/gthen:0 (write-once: cond NOT re-evaluated)", r3, err)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork calls = %d, want 2 (no re-dispatch on the held decision)", fake.dispatchCount())
	}
	if got, settledNow := settledOutcomeByID(t, streamStored(t, store, streamID))["stage/g"]; settledNow {
		t.Fatalf("guard stage/g settled %q after the grown fold, want unsettled (a re-eval would have skipped it)", got)
	}

	// The then closes pass; the guard settles TRANSPARENTLY from it (output plumbed),
	// never "no branch taken".
	fake.settleAct(t, "stage/gthen:0", engine.OutcomePass, "gated done")
	r4, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r4.Sealed {
		t.Fatalf("advance 4 = %+v err %v, want Sealed", r4, err)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, streamID))
	if settled["stage/g"] != engine.OutcomePass {
		t.Errorf("guard stage/g = %q, want pass (transparent from the then)", settled["stage/g"])
	}
	if got := r4.Run.NodeOutputs["stage/g"]; got != "gated done" {
		t.Errorf("guard output = %q, want %q (transparent settle, NOT the skipped empty)", got, "gated done")
	}
	// The forged probe:1 row is a root-parented lazy node, so the RUN outcome honestly
	// reports failed — the pin is the guard's held decision, not the run outcome.
	if r4.Run.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed (the forged probe:1 counts at the root)", r4.Run.Outcome)
	}
}

// TestGuardInSubFormulaFalsePoolInlineJournalParity (§2.10 parity, FALSE branch) proves
// the SAME ns-guard-FALSE IR driven inline (Run + Host, never invoked) and pooled
// (Advance + PoolRouter, never dispatching) journals BYTE-IDENTICALLY after run.started
// — same types, same canonical payload bytes, same order.
func TestGuardInSubFormulaFalsePoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	envFields := `[` +
		`{"name":"note","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"reuse","value":{"kind":"expr","expr":{"kind":"literal","value":"seeded"}}}]`
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "greeter", envFields),
		sub))

	// Inline driver: the host must never be invoked (the false guard runs no then).
	inStore := newStore(t)
	inRes, err := engine.RunWithOptions(ctx, inStore, doc, map[string]any{"who": "world"}, engine.Options{Host: passDoStub()})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if n := countJournalType(t, inStore, inRes.StreamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("inline effect.scheduled count = %d, want 0 (host must never be invoked)", n)
	}

	// Pool driver: one Advance pass must SEAL without dispatching anything.
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-gis-parfalse", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (a false guard dispatches no then)", n)
	}

	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestGuardInSubFormulaExecThenPoolInlineJournalParity (§2.10 parity, TRUE branch) is
// the exec-then twin: the guard decides TRUE (default "" == "") and its exec then runs
// INLINE on both drivers, so the journals must agree byte-for-byte after run.started.
func TestGuardInSubFormulaExecThenPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardExecAfter("draft", nil, condEqualRaw("reuse", `""`), "draft.then", `echo "drafted {{ note }}"`))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if inRes.Outcome != engine.OutcomePass {
		t.Fatalf("inline outcome = %q, want pass", inRes.Outcome)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-gis-partrue", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec then runs inline)", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an exec then never dispatches)", n)
	}

	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// assertJournalPairsEqual byte-compares two journals after run.started (the
// cross-driver parity view — typePayloadPairs).
func assertJournalPairsEqual(t *testing.T, inline, pool []graphstore.StoredEvent) {
	t.Helper()
	inPairs, poolPairs := typePayloadPairs(inline), typePayloadPairs(pool)
	if len(inPairs) != len(poolPairs) {
		t.Fatalf("journal lengths diverge: inline %d events vs pool %d (after run.started)\ninline: %v\npool:   %v",
			len(inPairs), len(poolPairs), pairTypes(inPairs), pairTypes(poolPairs))
	}
	for i := range inPairs {
		if inPairs[i] != poolPairs[i] {
			t.Fatalf("journals diverge at post-run.started event %d:\ninline: %s %s\npool:   %s %s",
				i, inPairs[i][0], inPairs[i][1], poolPairs[i][0], poolPairs[i][1])
		}
	}
}

// TestGuardTrueInScatterInSubFormulaDrives (§2.9a TRUE branch) proves a scatter-member
// guard inside a sub-formula whose cond is TRUE via an env binding DRIVES: the then runs
// as part of the drained member, the guard settles pass transparently, the drain passes,
// and the run passes.
func TestGuardTrueInScatterInSubFormulaDrives(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("greeter", strField("note"),
		scatterNode("lanes", nil, "continue",
			execNode("direct", `echo d`, nil),
			guardExecAfter("g", nil, condEqualRaw("note", `"world"`), "gthen", `echo "gated {{ note }}"`)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/g"] != engine.OutcomePass {
		t.Errorf("member guard stage/g = %q, want pass (transparent from the ran then)", settled["stage/g"])
	}
	if settled["stage/lanes"] != engine.OutcomePass {
		t.Errorf("drain stage/lanes = %q, want pass (both members ran)", settled["stage/lanes"])
	}
	if got := res.NodeOutputs["stage/gthen"]; got != "gated world" {
		t.Errorf("member-guard then output = %q, want %q (env binding drove the branch)", got, "gated world")
	}
}

// TestAdvanceGuardInSubFormulaSilentLetBranches (§2.5 behavioral, Advance) proves a
// guard cond branching on a REAL silent let inside ns on the pool path: the let's value
// is visible to the cond (TRUE branch dispatches the then, rendered against the
// sub-scope), and its pure-lit closure installs no gate.
func TestAdvanceGuardInSubFormulaSilentLetBranches(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("greeter", strField("note"),
		`{"kind":"lit","id":"mylet","name":"mylet","after":[],"value":{"kind":"literal","value":"yes"}}`+","+
			guardDoID("g", condEqualRaw("mylet", `"yes"`), "gthen", "gated {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("stage", nil, "greeter", "note", "who"),
		sub))
	r1, err := engine.Advance(ctx, store, doc, "gcg-gis-silentlet", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "stage/gthen:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/gthen:0 (silent-let cond TRUE)", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/gthen:0"); got != "gated world" {
		t.Errorf("then prompt = %q, want %q", got, "gated world")
	}
	fake.settleAct(t, "stage/gthen:0", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, "gcg-gis-silentlet", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
	}
}

// TestRunRepeatRunBodyGuardPerAttemptRedecisionInline (P2.7, the inline attempt-ns twin
// of the marquee) proves the INLINE driver drives a guard inside an ATTEMPT namespace
// (stage/N/): condScope("stage/0/") consults the parentNS override registered at mint
// time, attempt 0's then FAILS → the guard and the attempt aggregate settle failed → the
// cond re-mints attempt 1, whose guard RE-decides fresh at stage/1/ and its then PASSES
// → the loop settles pass with the satisfying attempt's output.
func TestRunRepeatRunBodyGuardPerAttemptRedecisionInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "note", "who"),
			runCondPassOrIter()),
		sub))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/0/draft.then": {Outcome: enginehost.OutcomeFailed, Output: "no"},
		"stage/1/draft.then": {Outcome: enginehost.OutcomePass, Output: "done"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"who": "world"}, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass (per-attempt re-decision, inline)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/draft"] != engine.OutcomeFailed || settled["stage/1/draft"] != engine.OutcomePass {
		t.Errorf("per-attempt guards = {0:%q 1:%q}, want {failed pass}", settled["stage/0/draft"], settled["stage/1/draft"])
	}
	if _, _, _, out := loopSettle(t, res.Events, "loop:0"); out != "done" {
		t.Errorf("loop settle output = %q, want the passing attempt's output", out)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestRunRepeatRunBodyGuardCrashResumeInAttemptNS (P2.7 crash) proves a crash right
// after an ATTEMPT-namespace guard's then settles resumes convergently: the resume
// re-mints attempt 0 (re-registering the env seam + parentNS override), re-decides the
// guard at stage/0/, reloads the settled then WITHOUT re-invoking the host, and the
// loop exits pass — byte-identical drop+refold.
func TestRunRepeatRunBodyGuardCrashResumeInAttemptNS(t *testing.T) {
	sub := subDoc("greeter", strField("note")+","+defaultReuseField(),
		guardDoID("draft", condEqualRaw("reuse", `""`), "draft.then", "greet {{ note }}"))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeJSON("stage", nil, "greeter", "note", "who"),
			runCondPassOrIter()),
		sub))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/0/draft.then": {Outcome: enginehost.OutcomePass, Output: "done0"},
	}}

	resumed, store, stream := injectCrashThenResumeInput(t, doc, host, "stage/0/draft.then:0")
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	if resumed.NodeOutputs["stage/0/draft"] != "done0" {
		t.Errorf("resumed attempt-0 guard output = %q, want done0 (transparent from the reloaded then)", resumed.NodeOutputs["stage/0/draft"])
	}
	// The settled then was reloaded, never re-invoked: exactly ONE host call total
	// (the pre-crash run of stage/0/draft.then).
	if calls := host.Calls(); len(calls) != 1 {
		ids := make([]string, len(calls))
		for i, c := range calls {
			ids[i] = c.NodeID
		}
		t.Errorf("host calls = %d (%v), want 1 (attempt-ns then reloaded on resume)", len(calls), ids)
	}
	assertProjectionEqualsRefold(t, store, stream)
}
