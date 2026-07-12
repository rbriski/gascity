package engine

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// optInputSpec builds a stage/ runSpec declaring `x` as an OPTIONAL, unbound, undefaulted
// string input — the declared-null shape this slice (ga-wvqsay) resolves to present-null.
func optInputSpec() *runSpec {
	return &runSpec{inputFields: []ir.Field{{Name: "x", Type: strType}}}
}

// TestResolveDeclaredInput pins the pure shared resolver: every DECLARED field lands in a
// FRESH layer — a raw value verbatim (even an explicit null), else the default, else
// present-NULL — while UNDECLARED names are omitted (they miss / fall to children downstream).
// An OPTIONAL unbound-undefaulted field resolves present-null with NO error (ga-wvqsay). The
// advisory error arm (⚑B2, ga-ospbql) is now LIVE: a REQUIRED field with neither a value nor a
// default returns ErrRequiredInputUnbound naming it while STILL returning the tolerant full
// layer (the genesis surfaces refuse on the error; typedSubInput/rebuildDriver discard it).
func TestResolveDeclaredInput(t *testing.T) {
	fields := []ir.Field{
		{Name: "bound", Type: strType},
		{Name: "defaulted", Type: numType, Default: float64(7)},
		{Name: "optional", Type: strType}, // unbound, undefaulted, OPTIONAL → present-null, no error
	}
	raw := map[string]any{"bound": "v", "undeclared": "ignored"}
	got, err := resolveDeclaredInput(fields, raw)
	if err != nil {
		t.Fatalf("resolveDeclaredInput errored on an optional-unbound field (only required-unbound refuses): %v", err)
	}
	want := map[string]any{"bound": "v", "defaulted": float64(7), "optional": nil}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("resolveDeclaredInput = %#v, want %#v", got, want)
	}
	// An UNDECLARED name never enters the layer.
	if _, ok := got["undeclared"]; ok {
		t.Errorf("undeclared name leaked into the declared-input layer: %#v", got)
	}
	// FRESH map: raw is not mutated (⚑B1).
	if len(raw) != 2 || raw["bound"] != "v" {
		t.Errorf("raw was mutated: %#v", raw)
	}
	// An EXPLICIT null in raw stays null (bound-to-null), NOT the field default (default: null
	// ≡ absent, but an explicit provided null is a real bound value).
	got2, err := resolveDeclaredInput([]ir.Field{{Name: "x", Default: "d"}}, map[string]any{"x": nil})
	if err != nil {
		t.Fatalf("explicit-null field errored: %v", err)
	}
	if v, ok := got2["x"]; !ok || v != nil {
		t.Errorf("explicit raw null = (%v, present=%v), want a present nil (not the default)", v, ok)
	}

	// ⚑B2 (ga-ospbql) advisory arm: a REQUIRED field with no value and no default returns
	// ErrRequiredInputUnbound naming it, AND the tolerant full layer (the field present-null).
	req := []ir.Field{{Name: "provided", Type: strType, Required: true}, {Name: "token", Type: strType, Required: true}}
	got3, err := resolveDeclaredInput(req, map[string]any{"provided": "v"})
	if !errors.Is(err, ErrRequiredInputUnbound) {
		t.Fatalf("required-unbound err = %v, want ErrRequiredInputUnbound", err)
	}
	if !strings.Contains(err.Error(), `"token"`) {
		t.Errorf("err = %v, want it to name the unbound field \"token\"", err)
	}
	if v, ok := got3["token"]; !ok || v != nil {
		t.Errorf("token in tolerant layer = (%v, present=%v), want present-null even under refusal", v, ok)
	}
	if v, ok := got3["provided"]; !ok || v != "v" {
		t.Errorf("provided in tolerant layer = (%v, present=%v), want the bound value", v, ok)
	}
	// `default: null` ≡ absent: a required field defaulted to null still refuses.
	if _, err := resolveDeclaredInput([]ir.Field{{Name: "y", Required: true, Default: nil}}, nil); !errors.Is(err, ErrRequiredInputUnbound) {
		t.Errorf("required field with default:null (≡ absent) err = %v, want ErrRequiredInputUnbound", err)
	}
}

// TestLoopScopeNSOptionalUnboundClosesChildShadow is the ns child-shadow mutant killer
// (ga-wvqsay): a loop cond names an OPTIONAL, unbound, undefaulted declared input `x` while a
// same-named node `x` has settled a truthy value. Input-first resolution now finds the
// present-null in the typed input layer, so the child NEVER shadows it — pinned STABLE across
// two decide ticks (iteration 1 and 2). Pre-fix (typedSubInput omitted the key) `x` fell
// through to the child view and `x == "node-val"` was TRUE between ticks.
func TestLoopScopeNSOptionalUnboundClosesChildShadow(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"stage/x:0": {NodeID: "stage/x", Settled: true, Outcome: OutcomePass, Output: "node-val"}},
		nil,
		map[string]*runSpec{"stage/": optInputSpec()},
	)
	scope := map[string]string{"stage/x": "node-val"}
	for _, iter := range []int{1, 2} {
		cs, err := d.loopScopeNS(lisLoopSpec(), iter, nil, "stage/", scope, scope)
		if err != nil {
			t.Fatalf("loopScopeNS(iter %d): %v", iter, err)
		}
		// The child value must NOT shadow the frozen declared-null.
		if evalBool(t, op("==", refV("x"), lit("node-val")), cs) {
			t.Errorf("iter %d: optional-unbound x resolved the NODE value node-val — the child shadowed the declared-null (freeze defeated)", iter)
		}
		// x is present-null: falsy (bare), and NOT equal to the empty string (null compare ⇒ NaN).
		if !evalBool(t, opNot(refV("x")), cs) {
			t.Errorf("iter %d: !x was FALSE — an optional-unbound declared input must resolve present-null (falsy)", iter)
		}
		if evalBool(t, op("==", refV("x"), lit("")), cs) {
			t.Errorf("iter %d: x == \"\" was TRUE — present-null must not equal the empty string", iter)
		}
	}
}

// TestLoopScopeNSOptionalUnboundNullSemantics pins the ns present-null value semantics for an
// optional-unbound-undefaulted declared input with NO same-named node: outcome is PASS (a
// value-only impl that left the ref a miss would yield outcome ""), length() is 0 and STABLE
// across ticks (closes the SLX §1.6 length-of-optional-unbound catalog row and that follow-up
// bead's scope), the bare ref is falsy, and it is NOT equal to the empty string.
func TestLoopScopeNSOptionalUnboundNullSemantics(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": optInputSpec()})
	for _, iter := range []int{1, 2} {
		cs, err := d.loopScopeNS(lisLoopSpec(), iter, nil, "stage/", map[string]string{}, map[string]string{})
		if err != nil {
			t.Fatalf("loopScopeNS(iter %d): %v", iter, err)
		}
		// present-null ⇒ outcome pass (NOT a miss — a value-only seeding impl fails this pin).
		if !evalBool(t, op("==", refO("x"), lit("pass")), cs) {
			t.Errorf("iter %d: x.outcome != pass — present-null must settle outcome pass", iter)
		}
		// length(null) == 0, stable across ticks (SLX §1.6).
		lv, err := evalClosedExpr(json.RawMessage(lengthCallExpr("x")), cs)
		if err != nil {
			t.Fatalf("iter %d: length(x): %v", iter, err)
		}
		if lv != float64(0) {
			t.Errorf("iter %d: length(x) = %v, want 0 (length of an optional-unbound declared input)", iter, lv)
		}
		if !evalBool(t, opNot(refV("x")), cs) {
			t.Errorf("iter %d: !x was FALSE — present-null is falsy", iter)
		}
		if evalBool(t, op("==", refV("x"), lit("")), cs) {
			t.Errorf("iter %d: x == \"\" TRUE — present-null is not the empty string", iter)
		}
	}
}

// TestLoopScopeNSUndeclaredNameStillMisses pins that ONLY declared fields become present-null:
// an UNDECLARED name is left out of the input layer, so it still MISSES and falls through to
// the child view (resolving the same-named node), while the DECLARED optional-unbound `x`
// resolves its present-null. Declared-null must not swallow undeclared names.
func TestLoopScopeNSUndeclaredNameStillMisses(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"stage/ghost:0": {NodeID: "stage/ghost", Settled: true, Outcome: OutcomePass, Output: "child-out"}},
		nil,
		map[string]*runSpec{"stage/": optInputSpec()}, // declares x only; `ghost` is undeclared
	)
	scope := map[string]string{"stage/ghost": "child-out"}
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", scope, scope)
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	// The UNDECLARED name falls through to the child (a miss, not a declared-null).
	if !evalBool(t, op("==", refV("ghost"), lit("child-out")), cs) {
		t.Errorf("undeclared `ghost` did not fall through to the child — declared-null must not swallow undeclared names")
	}
	// The DECLARED optional-unbound x is present-null (falsy), not a miss to any child.
	if !evalBool(t, opNot(refV("x")), cs) {
		t.Errorf("declared optional-unbound x was not present-null")
	}
}

// TestLowerRunBodyLoopOptionalUnboundInputNoFoldEdge is the freeze-allowlist regression pin
// (no-new-fold-edges mutation style): a run-body loop inside a sub-formula whose cond reads an
// OPTIONAL, unbound, undefaulted declared input `x` (that is ALSO a same-named sibling node)
// LOWERS — the freeze allowlist admits an input NAME regardless of bound-ness — and gains ZERO
// fold edges to the sibling, because a run-body loop gates only on its body run's envRefs
// (⚑S6), never its cond refs. This slice changed decide-time resolution only; the freeze
// allowlist stays a name-set and lowering is unchanged.
func TestLowerRunBodyLoopOptionalUnboundInputNoFoldEdge(t *testing.T) {
	inner := lisSubFormula("inner", lisStrField("name"), execNode("hello", nil, "echo hi"))
	optX := `{"name":"x","type":{"kind":"atomic","name":"string"},"required":false,"body":false}`
	cond := `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"round","field":"outcome"},{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"x"},{"kind":"literal","value":"done"}]}]}`
	wrapper := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField()+","+optX,
		execNode("x", nil, "echo done")+","+lisRepeatRunBody("loop", cond))
	doc := decodeBundle(t, runMainDoc(lisWrapperRun(), wrapper+","+inner))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a run-body loop whose cond reads an OPTIONAL-unbound declared input (the freeze allowlist must admit an input name, bound or not): %v", err)
	}
	loop := unitByNode(units, "wrap/loop")
	if loop == nil || loop.kind != unitLoop || loop.loop.bodyRun == nil {
		t.Fatalf("wrap/loop = %+v, want a run-body unitLoop", loop)
	}
	for _, dep := range loop.afterDeps {
		if dep == "wrap/x" || dep == "x" {
			t.Errorf("loop gained a fold edge to the sibling x (afterDeps=%v); a cond input ref must add ZERO gates", loop.afterDeps)
		}
	}
}

// TestBaseScopeNilRendersEmpty pins the Q-C null-render mechanism (ga-ospbql): baseScope renders a
// nil value — a declared-null seeded input, or an explicit caller-passed null — as the EMPTY
// string, never json.Marshal(nil)'s 4-byte "null" and never a skipped key (which would leave
// {{name}} verbatim on interpolate's miss). A string passes verbatim; a non-string non-nil value
// marshals. Reverting the nil arm regresses to "null" (marshal) or a dropped key (skip) — both are
// caught here, so this is the load-bearing mutant killer for the null-render fix.
func TestBaseScopeNilRendersEmpty(t *testing.T) {
	got := baseScope(map[string]any{
		"n":   nil,        // declared-null / explicit null → present ""
		"s":   "hi",       // string verbatim
		"num": float64(3), // non-string → json.Marshal
		"arr": []any{"a"}, // non-string → json.Marshal
	})
	if v, ok := got["n"]; !ok || v != "" {
		t.Errorf("baseScope[n] = (%q, present=%v), want a present empty string (nil → \"\", never \"null\", never skipped)", v, ok)
	}
	if got["s"] != "hi" {
		t.Errorf("baseScope[s] = %q, want hi (string verbatim)", got["s"])
	}
	if got["num"] != "3" {
		t.Errorf("baseScope[num] = %q, want 3 (json.Marshal of a number)", got["num"])
	}
	if got["arr"] != `["a"]` {
		t.Errorf("baseScope[arr] = %q, want the marshaled array", got["arr"])
	}
}

// TestEmptyRawInputStaysUnpinned pins ⚑B1: an empty raw input hashes "" (unpinned) regardless of a
// declared/defaulted schema — seeding feeds baseScope + d.input, NEVER the hash — so a genesis-era
// journal that took no input imposes no resume input constraint (old journals resume green). A
// non-empty raw input still pins; had the hash seen the seeded default bytes, this would differ.
func TestEmptyRawInputStaysUnpinned(t *testing.T) {
	if h := inputHash(nil); h != "" {
		t.Errorf("inputHash(nil) = %q, want \"\" (empty raw is unpinned)", h)
	}
	if h := inputHash(map[string]any{}); h != "" {
		t.Errorf("inputHash(empty map) = %q, want \"\"", h)
	}
	if h := inputHash(map[string]any{"x": "v"}); h == "" {
		t.Errorf("inputHash(non-empty) = \"\", want a pinned hash")
	}
}

// TestLoopScopeNSEnvBoundNullCrossesAsEmpty pins the §4 null-hop divergence row (ga-ospbql): a null
// does NOT survive a hop. An env binding of a declared-null parent input renders "" and arrives at
// the child as a BOUND, PRESENT empty string (retypeScalar keeps ""), so `y == ""` is TRUE at the
// child — where a propagated present-null would be NaN/FALSE (the divergence from the reference) —
// and length(y) == 0 (parity with the present-null length row). Cataloged so nobody "fixes" this
// into null propagation across hops.
func TestLoopScopeNSEnvBoundNullCrossesAsEmpty(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {
		inputFields: []ir.Field{{Name: "y", Type: strType}},
		env:         []runEnvField{litExprBinding("y", "")}, // the parent's declared-null, rendered "" then bound
	}})
	cs, err := d.loopScopeNS(lisLoopSpec(), 1, nil, "stage/", map[string]string{}, map[string]string{})
	if err != nil {
		t.Fatalf("loopScopeNS: %v", err)
	}
	// Present-"" at the child: == "" TRUE (a present-null would be NaN/FALSE — the divergence).
	if !evalBool(t, op("==", refV("y"), lit("")), cs) {
		t.Errorf("y == \"\" was FALSE, want TRUE (a bound null crosses as present-empty, NOT null)")
	}
	// bound "" is falsy, and length("") == 0 (parity with the present-null length row).
	if !evalBool(t, opNot(refV("y")), cs) {
		t.Errorf("!y was FALSE, want TRUE (empty string is falsy)")
	}
	lv, err := evalClosedExpr(json.RawMessage(lengthCallExpr("y")), cs)
	if err != nil {
		t.Fatalf("length(y): %v", err)
	}
	if lv != float64(0) {
		t.Errorf("length(y) = %v, want 0 (bound-empty length)", lv)
	}
}
