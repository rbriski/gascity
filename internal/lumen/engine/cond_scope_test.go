package engine

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// condScopeDriver builds a minimal driver carrying a hand-seeded fold + runEnvs index,
// so condScope's namespace-local assembly can be pinned directly (the resolution
// semantics §1.2 folds are otherwise only observable indirectly through a full run).
func condScopeDriver(nodes map[string]*nodeState, input map[string]any, runEnvs map[string]*runSpec) *driver {
	if nodes == nil {
		nodes = map[string]*nodeState{}
	}
	return &driver{
		reducer:  lumenReducer{},
		state:    &lumenState{Nodes: nodes},
		input:    input,
		runEnvs:  runEnvs,
		parentNS: map[string]string{},
	}
}

// litExprBinding is a run env binding whose value is a literal render-string (the
// shape scopeFor evaluates for a `given { x: "v" }` binding).
func litExprBinding(name, value string) runEnvField {
	return runEnvField{name: name, value: json.RawMessage(`{"kind":"expr","expr":{"kind":"literal","value":` + jsonStr(value) + `}}`)}
}

func refV(name string) string { return `{"kind":"ref","name":"` + name + `"}` }
func refO(name string) string { return `{"kind":"ref","name":"` + name + `","field":"outcome"}` }

// evalBool evaluates a closed expr over cs and asserts it is a bool, returning it.
func evalBool(t *testing.T, exprJSON string, cs loopScope) bool {
	t.Helper()
	v, err := evalClosedExpr(json.RawMessage(exprJSON), cs)
	if err != nil {
		t.Fatalf("evalClosedExpr(%s): %v", exprJSON, err)
	}
	b, ok := v.(bool)
	if !ok {
		t.Fatalf("evalClosedExpr(%s) = %#v (%T), want bool", exprJSON, v, v)
	}
	return b
}

// TestCondScopeRootParityByteIdentical (§1.2 ns=="") pins that condScope at the root is
// byte-identical to the loop machinery's empty-loop scope — the run-free / top-level
// decision path is unchanged.
func TestCondScopeRootParityByteIdentical(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"a:0": {NodeID: "a", Settled: true, Outcome: OutcomeFailed, Output: "aout"}},
		map[string]any{"max": float64(3)},
		map[string]*runSpec{},
	)
	nodeOutputs := map[string]string{"a": "aout"}
	got, err := d.condScope("", map[string]string{"a": "aout"}, nodeOutputs)
	if err != nil {
		t.Fatalf("condScope(root): %v", err)
	}
	want := d.loopScope(&loopSpec{}, 0, nil, nodeOutputs)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("condScope(\"\") = %+v, want byte-identical to loopScope(empty) = %+v", got, want)
	}
}

// TestCondScopeUnregisteredNamespaceLoud (§2.13 ⚑S5) pins that condScope over an
// unregistered namespace refuses loudly (a direct unit test — register-before-drive
// makes this state unreachable on every real path, so a hit is a structural bug).
func TestCondScopeUnregisteredNamespaceLoud(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {}})
	_, err := d.condScope("ghost/", map[string]string{}, map[string]string{})
	if err == nil || !strings.Contains(err.Error(), "ghost/") || !strings.Contains(err.Error(), "no registered environment") {
		t.Fatalf("condScope(unregistered) err = %v, want a loud unregistered-namespace refusal naming ghost/", err)
	}
}

// TestCondScopeIsolationAndMiss (§2.6 ⚑S2) pins namespace isolation + per-operator miss
// semantics: a ROOT node not visible in the ns view resolves null (no flat-scope leak);
// a same-named MAIN input does NOT leak; and a ref naming nothing resolves null →
// == FALSE, != TRUE, ordered FALSE, bare falsy.
func TestCondScopeIsolationAndMiss(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"other:0": {NodeID: "other", Settled: true, Outcome: OutcomePass, Output: "rootout"}},
		map[string]any{"secret": "leaked"}, // a MAIN input the ns cond must NOT see
		map[string]*runSpec{"stage/": {}},  // registered but empty (no bindings/children)
	)
	cs, err := d.condScope("stage/", map[string]string{"other": "rootout"}, map[string]string{"other": "rootout"})
	if err != nil {
		t.Fatalf("condScope(stage/): %v", err)
	}
	// A root node is invisible inside the ns (its flat key is not a direct child).
	if evalBool(t, op("==", refV("other"), lit("rootout")), cs) {
		t.Errorf("root node `other` leaked into the ns view (== rootout was TRUE)")
	}
	// A same-named MAIN input does not leak (input is nil'd for ns scopes).
	if evalBool(t, op("==", refV("secret"), lit("leaked")), cs) {
		t.Errorf("MAIN input `secret` leaked into the ns view (== leaked was TRUE)")
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

// TestCondScopeDivergencePins (§2.11 ⚑S3) pins the DOCUMENTED namespace semantics: ns
// bindings/defaults are render-strings, so an ORDERED comparison on a number-typed
// binding goes LEXICOGRAPHIC ("10" > 3 → FALSE, since "10" < "3" as strings) and the
// bare-ref truthiness of "false" is TRUTHY (a non-empty string), where root compares
// numerically / treats typed false as falsy.
func TestCondScopeDivergencePins(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		env: []runEnvField{litExprBinding("n", "10"), litExprBinding("flag", "false")},
	}})
	cs, err := d.condScope("stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("condScope(stage/): %v", err)
	}
	// "10" > 3 → String-coerce → "10" vs "3" → lexical "10" < "3" → FALSE.
	if evalBool(t, op(">", refV("n"), lit(3)), cs) {
		t.Errorf("n > 3 was TRUE inside ns, want FALSE (lexical: \"10\" < \"3\")")
	}
	// bare-ref truthiness of the render-string "false" is TRUTHY (non-empty), so
	// `!flag` is FALSE.
	if evalBool(t, opNot(refV("flag")), cs) {
		t.Errorf("!flag was TRUE inside ns, want FALSE (\"false\" is a truthy non-empty string)")
	}
}

// TestCondScopeCollisionPrecedence (§2.12 ⚑S4) pins prompt-parity precedence: when a
// sub-formula declares an input `x` AND contains a node `x`, the cond reads the NODE
// (children shadow same-named bindings), inverting root cond's input-first order —
// pinned for BOTH spec layers: a defaulted-unbound input and an explicit env BINDING.
func TestCondScopeCollisionPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		spec *runSpec
	}{
		{"default collision", &runSpec{inputFields: []ir.Field{{Name: "x", Default: "default-val"}}}},
		{"binding collision", &runSpec{env: []runEnvField{litExprBinding("x", "binding-val")}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := condScopeDriver(
				map[string]*nodeState{"stage/x:0": {NodeID: "stage/x", Settled: true, Outcome: OutcomePass, Output: "node-val"}},
				nil,
				map[string]*runSpec{"stage/": tc.spec},
			)
			scope := map[string]string{"stage/x": "node-val"}
			cs, err := d.condScope("stage/", scope, scope)
			if err != nil {
				t.Fatalf("condScope(stage/): %v", err)
			}
			// The NODE value shadows the same-named binding/default.
			if !evalBool(t, op("==", refV("x"), lit("node-val")), cs) {
				t.Errorf("x did not resolve the NODE value node-val (the spec layer shadowed the node)")
			}
			if evalBool(t, op("==", refV("x"), lit("default-val")), cs) || evalBool(t, op("==", refV("x"), lit("binding-val")), cs) {
				t.Errorf("x resolved the spec-layer value, want the node value (node shadows binding)")
			}
			// The node's real outcome wins over the ⚑B2 backfill for a shadowed name.
			if !evalBool(t, op("==", refO("x"), lit("pass")), cs) {
				t.Errorf("x.outcome did not resolve the node's settled outcome pass")
			}
		})
	}
}

// TestCondScopeOutcomeSemantics (§2.4) pins the four .outcome rows inside ns: a leaf
// sibling (max-attempt-wins), a ⚑B1 AGGREGATE sibling (nodeOutputs-only value + fold
// outcome), a ⚑B2 silent-let (.outcome "", NOT blanket-stamped "pass"), and a spec
// binding (.outcome backfilled "pass").
func TestCondScopeOutcomeSemantics(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{
			// leaf sibling with two attempts — the highest (pass) wins the bare id.
			"stage/leaf:0": {NodeID: "stage/leaf", Settled: true, Outcome: OutcomeFailed, Output: "leaf0"},
			"stage/leaf:1": {NodeID: "stage/leaf", Settled: true, Outcome: OutcomePass, Output: "leaf1"},
			// an aggregate sibling (a sub-scatter) — settled pass, empty transparent output.
			"stage/sc:0": {NodeID: "stage/sc", Settled: true, Outcome: OutcomePass, Output: ""},
		},
		nil,
		map[string]*runSpec{"stage/": {env: []runEnvField{litExprBinding("name", "bound")}}},
	)
	// scope carries non-aggregate leaf + a silent let; nodeOutputs ALSO carries the
	// aggregate `sc` (the nodeOutputs-only convention — never in scope).
	scope := map[string]string{"stage/leaf": "leaf1", "stage/mylet": "letval"}
	nodeOutputs := map[string]string{"stage/leaf": "leaf1", "stage/sc": "", "stage/mylet": "letval"}
	cs, err := d.condScope("stage/", scope, nodeOutputs)
	if err != nil {
		t.Fatalf("condScope(stage/): %v", err)
	}
	// (1) leaf sibling: max-attempt-wins ⇒ outcome pass, value the highest attempt's.
	if !evalBool(t, op("==", refO("leaf"), lit("pass")), cs) {
		t.Errorf("leaf.outcome != pass (max-attempt-wins failed)")
	}
	if !evalBool(t, op("==", refV("leaf"), lit("leaf1")), cs) {
		t.Errorf("leaf did not resolve leaf1")
	}
	// (2) ⚑B1 aggregate sibling: outcome pass AND bare == "" (the empty transparent
	// output, visible ONLY via the flat-nodeOutputs overlay).
	if !evalBool(t, op("==", refO("sc"), lit("pass")), cs) {
		t.Errorf("sc.outcome != pass — ⚑B1 aggregate outcome unreachable inside ns")
	}
	if !evalBool(t, op("==", refV("sc"), lit("")), cs) {
		t.Errorf("sc did not resolve its empty aggregate output — ⚑B1 value overlay missing")
	}
	// (3) ⚑B2 silent let: value visible, but .outcome is "" (NOT blanket-stamped pass).
	if !evalBool(t, op("==", refV("mylet"), lit("letval")), cs) {
		t.Errorf("silent let value not visible inside ns")
	}
	if evalBool(t, op("==", refO("mylet"), lit("pass")), cs) {
		t.Errorf("mylet.outcome == pass was TRUE — a silent let must NOT be blanket-stamped (⚑B2)")
	}
	if !evalBool(t, op("==", refO("mylet"), lit("")), cs) {
		t.Errorf("mylet.outcome != \"\" — a silent let keeps root-parity empty outcome")
	}
	// (4) ⚑B2 binding backfill: a spec-derived binding name's outcome is pass.
	if !evalBool(t, op("==", refO("name"), lit("pass")), cs) {
		t.Errorf("name.outcome != pass — spec-derived binding backfill missing (⚑B2)")
	}
}
