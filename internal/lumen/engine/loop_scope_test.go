package engine

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// numType / boolType / strType are the atomic type meta-nodes a sub-formula's declared
// input fields carry (the LIS typed sub-input layer keys retyping on these).
var (
	numType  = ir.Type{Kind: ir.TypeAtomic, Name: "number"}
	boolType = ir.Type{Kind: ir.TypeAtomic, Name: "boolean"}
	strType  = ir.Type{Kind: ir.TypeAtomic, Name: "string"}
)

// lisLoopSpec builds a minimal repeat loopSpec for the namespace-scope pins: the bare body
// id "round" (⚑B2) under the "stage/" namespace and the "iteration" binding.
func lisLoopSpec() *loopSpec {
	return &loopSpec{irKind: ir.NodeRepeat, bodyBareID: "round", bodyNodeID: "stage/round", iterationName: "iteration"}
}

// TestLoopScopeNSTypedInputNumericNotLexicographic is THE lexicographic mutant killer
// (§2.1, two-digit budget): inside a sub-formula the loop cond's `iteration >=
// max_review_rounds` compares NUMBER to NUMBER — a typed sub input — so with budget 12 it
// does NOT exit at iteration 2 (which a render-string lexicographic compare, "2" >= "12",
// would). The loop-flavored scope re-types the env-bound number field.
func TestLoopScopeNSTypedInputNumericNotLexicographic(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		env:         []runEnvField{litExprBinding("max_review_rounds", "12")},
		inputFields: []ir.Field{{Name: "max_review_rounds", Type: numType}},
	}})
	spec := lisLoopSpec()
	cond := op(">=", refV("iteration"), refV("max_review_rounds"))

	// iteration 2: numeric 2 >= 12 is FALSE (keep looping). Lexicographic "2" >= "12"
	// would be TRUE (exit at round 2) — the defeated freeze premise.
	cs2, err := d.loopScopeNS(spec, 2, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS(iter 2): %v", err)
	}
	if evalBool(t, cond, cs2) {
		t.Errorf("iteration >= max_review_rounds was TRUE at iteration 2 (LEXICOGRAPHIC bug); want FALSE (numeric 2 < 12)")
	}
	// iteration 12: numeric 12 >= 12 is TRUE (exit at the cap).
	cs12, err := d.loopScopeNS(spec, 12, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS(iter 12): %v", err)
	}
	if !evalBool(t, cond, cs12) {
		t.Errorf("iteration >= max_review_rounds was FALSE at iteration 12; want TRUE (numeric 12 >= 12)")
	}
}

// TestLoopScopeNSInputFirstPrecedence pins the §2.4 precedence BOTH WAYS: the loop scope
// is INPUT-FIRST (root parity) — an input `x` and a same-named node `x` resolves the
// INPUT, frozen across ticks — deliberately diverging from the guard condScope's child-wins
// order (pinned green in cond_scope_test.go TestCondScopeCollisionPrecedence).
func TestLoopScopeNSInputFirstPrecedence(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"stage/x:0": {NodeID: "stage/x", Settled: true, Outcome: OutcomePass, Output: "node-val"}},
		nil,
		map[string]*runSpec{"stage/": {
			env:         []runEnvField{litExprBinding("x", "input-val")},
			inputFields: []ir.Field{{Name: "x", Type: strType}},
		}},
	)
	scope := map[string]string{"stage/x": "node-val"}
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", scope, scope)
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	// The INPUT value wins over the same-named node (opposite of the guard child-wins).
	if !evalBool(t, op("==", refV("x"), lit("input-val")), cs) {
		t.Errorf("x did not resolve the INPUT value (loop scope must be input-first, root parity)")
	}
	if evalBool(t, op("==", refV("x"), lit("node-val")), cs) {
		t.Errorf("x resolved the NODE value — a same-named node shadowed the frozen input (freeze defeated)")
	}
}

// TestLoopScopeNSBodyBareIDResolves pins ⚑B2: the just-settled attempt binds under the
// BARE body id, so `round.outcome`/`round` resolve the attempt's outcome/output inside a
// sub-formula (the cond names the body by its authored bare id, not the qualified one).
func TestLoopScopeNSBodyBareIDResolves(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {}})
	bn := &nodeState{Settled: true, Outcome: OutcomePass, Output: "the-output"}
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, bn, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	if !evalBool(t, op("==", refO("round"), lit("pass")), cs) {
		t.Errorf("round.outcome did not resolve the attempt outcome (bodyBareID arm)")
	}
	if !evalBool(t, op("==", refV("round"), lit("the-output")), cs) {
		t.Errorf("round did not resolve the attempt output")
	}
}

// TestLoopScopeNSTypedBoolInput pins the boolean re-type: an env-bound boolean sub input
// `false` is a real bool (falsy) inside the loop scope, so `!flag` is TRUE — the numeric/bool
// root parity, opposite of the guard render-string view where "false" is a truthy non-empty
// string (cond_scope_test.go TestCondScopeDivergencePins).
func TestLoopScopeNSTypedBoolInput(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		env:         []runEnvField{litExprBinding("flag", "false")},
		inputFields: []ir.Field{{Name: "flag", Type: boolType}},
	}})
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	if !evalBool(t, opNot(refV("flag")), cs) {
		t.Errorf("!flag was FALSE inside the loop scope; want TRUE (typed bool false is falsy — root parity)")
	}
}

// TestLoopScopeNSRetryAttemptsTyped pins §2.2: a retry `attempts: <number sub input>`
// evaluated at depth reads the TYPED number (not the render string), so evalAttempts
// yields the integer budget rather than invalid_input (a string "3" would fail the
// float64 assertion).
func TestLoopScopeNSRetryAttemptsTyped(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		env:         []runEnvField{litExprBinding("n", "3")},
		inputFields: []ir.Field{{Name: "n", Type: numType}},
	}})
	spec := &loopSpec{irKind: ir.NodeRetry, bodyBareID: "body", bodyNodeID: "stage/body"}
	cs, err := d.loopScopeNS(spec, 0, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	budget, ok := evalAttempts(json.RawMessage(refV("n")), cs)
	if !ok || budget != 3 {
		t.Errorf("evalAttempts(typed sub input n=3) = (%d, %v), want (3, true) — a string would be invalid_input", budget, ok)
	}
}

// TestLoopScopeNSDefaultedUnboundInput pins the defaulted-unbound arm: a declared sub input
// with a TYPED default and no env binding contributes its typed default directly.
func TestLoopScopeNSDefaultedUnboundInput(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		inputFields: []ir.Field{{Name: "cap", Type: numType, Default: float64(7)}},
	}})
	spec := lisLoopSpec()
	cs, err := d.loopScopeNS(spec, 3, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	// iteration 3 >= cap 7 → FALSE (numeric); the default flows in as a typed 7.
	if evalBool(t, op(">=", refV("iteration"), refV("cap")), cs) {
		t.Errorf("iteration >= cap TRUE at iteration 3 with default cap 7; want FALSE (typed default 7)")
	}
	if !evalBool(t, op("==", refV("cap"), lit(float64(7))), cs) {
		t.Errorf("cap did not resolve the typed default 7")
	}
}

// TestLoopScopeNSUnregisteredNamespaceLoud pins ⚑S5 for the LOOP call-site: an unregistered
// namespace refuses LOUDLY and the message reports it as a LOOP cond scope (not a guard).
func TestLoopScopeNSUnregisteredNamespaceLoud(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {}})
	_, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "ghost/", map[string]string{}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "ghost/") || !strings.Contains(err.Error(), "no registered environment") {
		t.Fatalf("loopScopeNS(unregistered) err = %v, want a loud unregistered-namespace refusal naming ghost/", err)
	}
	if !strings.Contains(err.Error(), "loop") {
		t.Errorf("loopScopeNS refusal %q should report a LOOP cond scope, not a guard (⚑S5 parametrized message)", err.Error())
	}
}

// TestLoopScopeNSIsolationAndMiss pins GIS §2.6 parity for the LOOP scope: a ROOT node
// and ANOTHER namespace's node are both invisible inside the loop ns view (no flat-scope
// leak); a same-named MAIN input does NOT leak (the typed input layer derives from the
// SUB spec only, never d.input); and a ref naming nothing resolves null → == FALSE,
// != TRUE, every ordered operator FALSE, bare falsy.
func TestLoopScopeNSIsolationAndMiss(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{
			"other:0":       {NodeID: "other", Settled: true, Outcome: OutcomePass, Output: "rootout"},
			"elsewhere/n:0": {NodeID: "elsewhere/n", Settled: true, Outcome: OutcomePass, Output: "nsout"},
		},
		map[string]any{"secret": "leaked"}, // a MAIN input the ns loop cond must NOT see
		map[string]*runSpec{"stage/": {}},  // registered but empty (no bindings/children)
	)
	scope := map[string]string{"other": "rootout", "elsewhere/n": "nsout"}
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", scope, scope)
	if err != nil {
		t.Fatalf("loopScopeNS(stage/): %v", err)
	}
	// A root node is invisible inside the ns (its flat key is not a direct child).
	if evalBool(t, op("==", refV("other"), lit("rootout")), cs) {
		t.Errorf("root node `other` leaked into the loop ns view (== rootout was TRUE)")
	}
	// Another namespace's node is invisible too (elsewhere/n is not a stage/ child).
	if evalBool(t, op("==", refV("n"), lit("nsout")), cs) {
		t.Errorf("other-namespace node `elsewhere/n` leaked into the loop ns view as bare n")
	}
	// A same-named MAIN input does not leak (the typed layer derives from the sub spec).
	if evalBool(t, op("==", refV("secret"), lit("leaked")), cs) {
		t.Errorf("MAIN input `secret` leaked into the loop ns view (== leaked was TRUE)")
	}
	// Per-operator miss: == FALSE, != TRUE, EVERY ordered operator FALSE, bare falsy.
	if evalBool(t, op("==", refV("missing"), lit("x")), cs) {
		t.Errorf("missing == \"x\" was TRUE, want FALSE (null compare)")
	}
	if !evalBool(t, op("!=", refV("missing"), lit("x")), cs) {
		t.Errorf("missing != \"x\" was FALSE, want TRUE (null compare NaN)")
	}
	for _, ordered := range []string{">=", "<=", ">", "<"} {
		if evalBool(t, op(ordered, refV("missing"), lit("x")), cs) {
			t.Errorf("missing %s \"x\" was TRUE, want FALSE (ordered null ⇒ NaN)", ordered)
		}
	}
	if !evalBool(t, opNot(refV("missing")), cs) {
		t.Errorf("!missing was FALSE, want TRUE (a missing bare ref is falsy)")
	}
}

// TestLoopScopeNSAggregateSiblingVisible pins §2.4 aggregate-sibling parity: a sub-scatter
// aggregate sibling's outcome/empty-output is visible to the loop cond via the ⚑B1
// flat-nodeOutputs children overlay (the same reachability the guard scope gets).
func TestLoopScopeNSAggregateSiblingVisible(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"stage/sc:0": {NodeID: "stage/sc", Settled: true, Outcome: OutcomePass, Output: ""}},
		nil,
		map[string]*runSpec{"stage/": {}},
	)
	// nodeOutputs carries the aggregate (never in scope — the nodeOutputs-only convention).
	nodeOutputs := map[string]string{"stage/sc": ""}
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", map[string]string{}, nodeOutputs)
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	if !evalBool(t, op("==", refO("sc"), lit("pass")), cs) {
		t.Errorf("sc.outcome != pass — ⚑B1 aggregate outcome unreachable inside the loop scope")
	}
	if !evalBool(t, op("==", refV("sc"), lit("")), cs) {
		t.Errorf("sc did not resolve its empty aggregate output — ⚑B1 value overlay missing")
	}
}
