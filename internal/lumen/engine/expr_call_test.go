package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// callScope is a fixed loop scope for the closed-expr `length` table: an array input
// `items` ([]any of 3), an empty array `empty`, a string `word`, a map `obj`, a numeric
// `n`, and a bool `flag`. Arrays/maps arrive first-class (the value-model extension,
// §1.1.3).
func callScope() loopScope {
	return loopScope{
		input: map[string]any{
			"items": []any{"a", "b", "c"},
			"empty": []any{},
			"word":  "café",
			"obj":   map[string]any{"x": float64(1), "y": float64(2)},
			"n":     float64(7),
			"flag":  true,
		},
	}
}

func lengthCallExpr(argName string) string {
	return `{"kind":"call","name":"length","args":[{"kind":"ref","name":"` + argName + `"}]}`
}

// TestClosedExprLengthSemantics pins §1.1.2 / §2.3: length over array (n), empty (0),
// string (UTF-16 code units — café→4, surrogate pair→2), object (key count), missing
// ref (0 via null), number (0).
func TestClosedExprLengthSemantics(t *testing.T) {
	scope := callScope()
	for _, tc := range []struct {
		name string
		expr string
		want float64
	}{
		{"array", lengthCallExpr("items"), 3},
		{"empty-array", lengthCallExpr("empty"), 0},
		{"string-utf16-cafe", lengthCallExpr("word"), 4},
		{"object-key-count", lengthCallExpr("obj"), 2},
		{"missing-ref-null", lengthCallExpr("nope"), 0},
		{"number", lengthCallExpr("n"), 0},
		{"bool", lengthCallExpr("flag"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalClosedExpr(json.RawMessage(tc.expr), scope)
			if err != nil {
				t.Fatalf("evalClosedExpr(%s): %v", tc.expr, err)
			}
			f, ok := got.(float64)
			if !ok || f != tc.want {
				t.Errorf("length = %v (%T), want %v", got, got, tc.want)
			}
		})
	}
}

// TestClosedExprLengthStringUTF16SurrogatePair proves length counts UTF-16 code units,
// not runes: a single astral-plane rune (😀, one rune) is TWO UTF-16 units (§1.1.2).
func TestClosedExprLengthStringUTF16SurrogatePair(t *testing.T) {
	scope := loopScope{input: map[string]any{"emoji": "😀"}}
	got, err := evalClosedExpr(json.RawMessage(lengthCallExpr("emoji")), scope)
	if err != nil {
		t.Fatalf("evalClosedExpr: %v", err)
	}
	if f, ok := got.(float64); !ok || f != 2 {
		t.Errorf("length(\"😀\") = %v, want 2 (UTF-16 code units, not rune count 1)", got)
	}
}

// TestValidateClosedExprCall pins §1.1.1: length is accepted (arity 1, recurses into the
// arg); a non-length name and a wrong arity are refused with ErrUnsupportedNode; a bad
// arg propagates.
func TestValidateClosedExprCall(t *testing.T) {
	for _, tc := range []struct {
		name    string
		expr    string
		wantErr bool
	}{
		{"length-ok", lengthCallExpr("items"), false},
		{"length-literal-arg-ok", `{"kind":"call","name":"length","args":[{"kind":"literal","value":"hi"}]}`, false},
		{"non-length-name", `{"kind":"call","name":"join","args":[{"kind":"ref","name":"items"}]}`, true},
		{"arity-zero", `{"kind":"call","name":"length","args":[]}`, true},
		{"arity-two", `{"kind":"call","name":"length","args":[{"kind":"ref","name":"a"},{"kind":"ref","name":"b"}]}`, true},
		{"bad-arg-kind", `{"kind":"call","name":"length","args":[{"kind":"member","base":{"kind":"ref","name":"a"},"name":"b"}]}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateClosedExpr(json.RawMessage(tc.expr))
			if tc.wantErr && !errors.Is(err, ErrUnsupportedNode) {
				t.Errorf("validateClosedExpr(%s) err = %v, want ErrUnsupportedNode", tc.expr, err)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateClosedExpr(%s) err = %v, want nil", tc.expr, err)
			}
		})
	}
}

// TestEvalClosedExprCallDefensiveMirrors pins the EVAL twin's defensive arms (P3 gap 6):
// lowering already refuses these, but evalClosedCall mirrors the refusals so a bad call can
// never evaluate — a non-length name and a wrong arity return ErrUnsupportedNode with the
// validate twin's messages.
func TestEvalClosedExprCallDefensiveMirrors(t *testing.T) {
	scope := callScope()
	for _, tc := range []struct {
		name string
		expr string
		want string
	}{
		{"non-length-name", `{"kind":"call","name":"join","args":[{"kind":"ref","name":"items"}]}`, `closed expr call "join"`},
		{"arity-zero", `{"kind":"call","name":"length","args":[]}`, `closed expr call "length" wants 1 arg, got 0`},
		{"arity-two", `{"kind":"call","name":"length","args":[{"kind":"ref","name":"items"},{"kind":"ref","name":"n"}]}`, `closed expr call "length" wants 1 arg, got 2`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := evalClosedExpr(json.RawMessage(tc.expr), scope)
			if !errors.Is(err, ErrUnsupportedNode) {
				t.Fatalf("evalClosedExpr(%s) err = %v, want ErrUnsupportedNode", tc.expr, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want it to contain %q (the validate twin's message)", err, tc.want)
			}
		})
	}
}

// TestClosedExprLengthInOrderedCompare pins §2.3: length nested in an ordered comparison
// (`iteration >= length(items)`) compares numerically — 3 >= 3 true, 2 >= 3 false.
func TestClosedExprLengthInOrderedCompare(t *testing.T) {
	expr := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},` + lengthCallExpr("items") + `]}`
	for _, tc := range []struct {
		iter int
		want bool
	}{
		{3, true},
		{2, false},
	} {
		scope := callScope()
		scope.iterationName = "iteration"
		scope.iteration = tc.iter
		got, err := evalClosedExpr(json.RawMessage(expr), scope)
		if err != nil {
			t.Fatalf("evalClosedExpr: %v", err)
		}
		if got != tc.want {
			t.Errorf("iteration %d >= length(items 3) = %v, want %v", tc.iter, got, tc.want)
		}
	}
}

// TestNormalizeExprValueArrayMapPassthrough pins §1.1.3: arrays and maps pass THROUGH the
// value model as first-class values (no %v Go-fmt flattening), so length/truthiness/compare
// see the real container.
func TestNormalizeExprValueArrayMapPassthrough(t *testing.T) {
	arr := []any{"a", "b"}
	if got := normalizeExprValue(arr); got == nil {
		t.Fatal("array normalized to nil")
	} else if _, ok := got.([]any); !ok {
		t.Errorf("normalizeExprValue([]any) = %T, want []any (passthrough)", got)
	}
	m := map[string]any{"k": float64(1)}
	if got := normalizeExprValue(m); got == nil {
		t.Fatal("map normalized to nil")
	} else if _, ok := got.(map[string]any); !ok {
		t.Errorf("normalizeExprValue(map) = %T, want map[string]any (passthrough)", got)
	}
}

// TestIsExprTruthyArray pins §1.1.3 (⚑ROOT-DELTA): a bare array-ref truthiness follows the
// reference — [] FALSE, [x] TRUE — instead of a %v-string non-empty coercion.
func TestIsExprTruthyArray(t *testing.T) {
	if isExprTruthy([]any{}) {
		t.Error("isExprTruthy([]) = true, want false")
	}
	if !isExprTruthy([]any{"x"}) {
		t.Error("isExprTruthy([x]) = false, want true")
	}
}

// TestCompareExprValuesArrayNoPanic pins ⚑B1: comparing two []any operands must NOT panic
// (Go == on non-comparable dynamic types) — it routes through String() coercion. Equal
// arrays compare equal; unequal compare unequal; array == itself is true.
func TestCompareExprValuesArrayNoPanic(t *testing.T) {
	a := []any{"a", "b"}
	b := []any{"a", "b"}
	c := []any{"a", "c"}
	if cmp, nan := compareExprValues(a, b); nan || cmp != 0 {
		t.Errorf("compare([a,b],[a,b]) = (%d,%v), want (0,false)", cmp, nan)
	}
	if cmp, nan := compareExprValues(a, c); nan || cmp == 0 {
		t.Errorf("compare([a,b],[a,c]) = (%d,%v), want unequal", cmp, nan)
	}
	if cmp, nan := compareExprValues(a, a); nan || cmp != 0 {
		t.Errorf("compare(a,a) = (%d,%v), want (0,false)", cmp, nan)
	}
}

// TestCompareExprValuesArrayEqualityViaOperator pins ⚑B1 through the == operator over refs
// naming the SAME array (`items == items`) — true via String coercion, no panic.
func TestCompareExprValuesArrayEqualityViaOperator(t *testing.T) {
	expr := `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"items"},{"kind":"ref","name":"items"}]}`
	got, err := evalClosedExpr(json.RawMessage(expr), callScope())
	if err != nil {
		t.Fatalf("evalClosedExpr: %v", err)
	}
	if got != true {
		t.Errorf("items == items = %v, want true", got)
	}
}

// TestJSStringArrayJoin pins ⚑B3: jsString mirrors JS String() for arrays exactly — join
// with ",", a nil element renders EMPTY (not "null"), nested arrays recurse, a map element
// renders "[object Object]", and a top-level map renders "[object Object]".
func TestJSStringArrayJoin(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   any
		want string
	}{
		{"nil-element-empty", []any{nil, "a"}, ",a"},
		{"plain-join", []any{"a", "b"}, "a,b"},
		{"number-join", []any{float64(1), float64(2)}, "1,2"},
		{"nested-flatten", []any{[]any{"a"}, "b"}, "a,b"},
		{"map-element", []any{map[string]any{"k": float64(1)}, "b"}, "[object Object],b"},
		{"top-level-map", map[string]any{"k": float64(1)}, "[object Object]"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := jsString(tc.in); got != tc.want {
				t.Errorf("jsString(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBareArrayRefTruthiness pins §2.4 (⚑ROOT-DELTA): a bare array-ref cond truthiness
// follows the reference — [] FALSE, [x] TRUE — through resolve→normalizeExprValue→isExprTruthy,
// where the old %v-string coercion made even [] truthy (non-empty "[a b]"). The same []any
// value model backs a depth-DEFAULTED array sub-input (typedSubInput passes the default
// through), so this pins both the root and depth-defaulted shapes.
func TestBareArrayRefTruthiness(t *testing.T) {
	bareRef := json.RawMessage(`{"kind":"ref","name":"items"}`)
	if got, err := evalCondTruthy(bareRef, loopScope{input: map[string]any{"items": []any{}}}); err != nil || got {
		t.Errorf("bare-ref [] truthy = %v (err %v), want false", got, err)
	}
	if got, err := evalCondTruthy(bareRef, loopScope{input: map[string]any{"items": []any{"x"}}}); err != nil || !got {
		t.Errorf("bare-ref [x] truthy = %v (err %v), want true", got, err)
	}
}

// TestEvalAttemptsLength pins §1.1.7 / §2.3: a retry `attempts: length(items)` evaluates to
// the array length as the integer budget (typed — via the length call, not invalid_input).
func TestEvalAttemptsLength(t *testing.T) {
	n, ok := evalAttempts(json.RawMessage(lengthCallExpr("items")), loopScope{input: map[string]any{"items": []any{"a", "b", "c"}}})
	if !ok || n != 3 {
		t.Errorf("evalAttempts(length(items)) = (%d, %v), want (3, true)", n, ok)
	}
}

// TestGuardCondLength pins §1.1.7 / §2.3: `length(items) > 0` in a cond is true for a
// non-empty array and false for an empty one (the guard-shaped length pin).
func TestGuardCondLength(t *testing.T) {
	expr := json.RawMessage(`{"kind":"operator","op":">","operands":[` + lengthCallExpr("items") + `,{"kind":"literal","value":0}]}`)
	if got, _ := evalCondTruthy(expr, loopScope{input: map[string]any{"items": []any{"a", "b"}}}); !got {
		t.Error("length([a,b]) > 0 = false, want true")
	}
	if got, _ := evalCondTruthy(expr, loopScope{input: map[string]any{"items": []any{}}}); got {
		t.Error("length([]) > 0 = true, want false")
	}
}

// TestRetypeScalarArrayArm pins §1.1.4: an array-typed env binding's JSON-array render
// string re-types to []any; a garbage (non-array) string is KEPT (lenient — length then
// counts its UTF-16 units).
func TestRetypeScalarArrayArm(t *testing.T) {
	arrType := ir.Type{Kind: ir.TypeArray, Element: &ir.Type{Kind: ir.TypeAtomic, Name: "string"}}
	got := retypeScalar(`["a","b"]`, arrType)
	arr, ok := got.([]any)
	if !ok || len(arr) != 2 || arr[0] != "a" || arr[1] != "b" {
		t.Fatalf("retypeScalar array = %v (%T), want []any{a,b}", got, got)
	}
	if kept := retypeScalar("not-json", arrType); kept != "not-json" {
		t.Errorf("retypeScalar(garbage) = %v, want the string kept", kept)
	}
	// A RECORD-typed field keeps its string verbatim (§1.1.4 "Record fields: unchanged") —
	// even when the render text is valid JSON.
	recType := ir.Type{Kind: ir.TypeRecord}
	if kept := retypeScalar(`{"k":"v"}`, recType); kept != `{"k":"v"}` {
		t.Errorf("retypeScalar(record) = %v (%T), want the string kept", kept, kept)
	}
}
