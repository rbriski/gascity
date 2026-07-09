package engine

import (
	"encoding/json"
	"errors"
	"testing"
)

// closedExprTestScope is a fixed loop scope for the T-E1 table: iteration 2, a
// body binding `draft` (outcome pass, output "hello"), a numeric run input
// `max`=3, and a settled node `a` (outcome failed, output "aout").
func closedExprTestScope() loopScope {
	return loopScope{
		iterationName: "iteration",
		iteration:     2,
		bodyName:      "draft",
		bodyOutcome:   OutcomePass,
		bodyOutput:    "hello",
		input:         map[string]any{"max": float64(3)},
		nodeOutputs:   map[string]string{"a": "aout"},
		nodeOutcomes:  map[string]string{"a": OutcomeFailed},
	}
}

// TestClosedExprSubsetMirrorsReference (T-E1) is the conformance table for the
// closed expression evaluator: every supported op (==,!=,>=,<=,>,<,&&,||,!),
// literal/ref/ref.outcome resolution, and the isTruthy / compareValues edge rows
// (null-compare ⇒ NaN ⇒ every comparison false; string-coerce; "" and 0 falsy),
// transcribed from the reference runner (evaluateLumenExpr / compareValues /
// isTruthy, formula-language packages/core/src/index.ts).
func TestClosedExprSubsetMirrorsReference(t *testing.T) {
	scope := closedExprTestScope()
	for _, tc := range []struct {
		name string
		expr string
		want any
	}{
		// literals
		{"lit-number", `{"kind":"literal","value":3}`, float64(3)},
		{"lit-string", `{"kind":"literal","value":"pass"}`, "pass"},
		{"lit-bool", `{"kind":"literal","value":true}`, true},
		{"lit-null", `{"kind":"literal","value":null}`, nil},
		// refs
		{"ref-iteration", `{"kind":"ref","name":"iteration"}`, float64(2)},
		{"ref-body-outcome", `{"kind":"ref","name":"draft","field":"outcome"}`, "pass"},
		{"ref-body-bare", `{"kind":"ref","name":"draft"}`, "hello"},
		{"ref-input", `{"kind":"ref","name":"max"}`, float64(3)},
		{"ref-node-outcome", `{"kind":"ref","name":"a","field":"outcome"}`, "failed"},
		{"ref-unresolved", `{"kind":"ref","name":"nope"}`, nil},
		// == / != over numbers, strings, and mixed (string-coerce)
		{"eq-num-true", op("==", lit(3), lit(3)), true},
		{"eq-num-false", op("==", lit(3), lit(4)), false},
		{"eq-str-true", op("==", lit("pass"), lit("pass")), true},
		{"eq-str-false", op("==", lit("pass"), lit("fail")), false},
		{"eq-string-coerce", op("==", lit("3"), lit(3)), true}, // String(3)==="3"
		{"neq-true", op("!=", lit(3), lit(4)), true},
		{"neq-false", op("!=", lit("pass"), lit("pass")), false},
		// null-compare ⇒ NaN ⇒ == false, != true, all inequalities false
		{"eq-null-null", op("==", lit(nil), lit(nil)), true}, // strict-equal, not NaN
		{"eq-null-num", op("==", lit(nil), lit(3)), false},
		{"neq-null-num", op("!=", lit(nil), lit(3)), true},
		{"ge-null-num", op(">=", lit(nil), lit(0)), false},
		{"le-null-num", op("<=", lit(nil), lit(0)), false},
		{"gt-null-num", op(">", lit(nil), lit(0)), false},
		{"lt-null-num", op("<", lit(nil), lit(0)), false},
		// >= <= > < over numbers
		{"ge-eq", op(">=", lit(3), lit(3)), true},
		{"ge-lt", op(">=", lit(2), lit(3)), false},
		{"ge-gt", op(">=", lit(4), lit(3)), true},
		{"le-eq", op("<=", lit(3), lit(3)), true},
		{"gt-true", op(">", lit(4), lit(3)), true},
		{"gt-false", op(">", lit(3), lit(3)), false},
		{"lt-true", op("<", lit(2), lit(3)), true},
		// >= with string-coerce ("3" >= 3 ⇒ String-coerce ⇒ "3">="3" ⇒ true)
		{"ge-string-coerce", op(">=", lit("3"), lit(3)), true},
		// && / || (both operands evaluated, no side effects)
		{"and-true", op("&&", op(">=", lit(1), lit(1)), op(">=", lit(2), lit(1))), true},
		{"and-false", op("&&", op(">=", lit(1), lit(2)), op(">=", lit(2), lit(1))), false},
		{"or-true", op("||", op(">=", lit(1), lit(2)), op(">=", lit(2), lit(1))), true},
		{"or-false", op("||", op(">=", lit(1), lit(2)), op(">=", lit(0), lit(1))), false},
		// ! (isTruthy edge rows: 0 falsy, "" falsy, non-empty truthy, false, null)
		{"not-zero", opNot(lit(0)), true},
		{"not-empty-string", opNot(lit("")), true},
		{"not-nonempty-string", opNot(lit("x")), false},
		{"not-false", opNot(lit(false)), true},
		{"not-null", opNot(lit(nil)), true},
		{"not-nonzero", opNot(lit(5)), false},
		// the canonical dogfood cond over the fixed scope: draft.outcome == pass || iteration >= 3
		{"dogfood-cond-true", op("||",
			op("==", `{"kind":"ref","name":"draft","field":"outcome"}`, lit("pass")),
			op(">=", `{"kind":"ref","name":"iteration"}`, lit(3))), true},
		{"dogfood-cond-fail-branch", op("||",
			op("==", `{"kind":"ref","name":"draft","field":"outcome"}`, lit("fail")),
			op(">=", `{"kind":"ref","name":"iteration"}`, lit(3))), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := evalClosedExpr(json.RawMessage(tc.expr), scope)
			if err != nil {
				t.Fatalf("evalClosedExpr(%s): %v", tc.expr, err)
			}
			if got != tc.want {
				t.Errorf("evalClosedExpr(%s) = %#v (%T), want %#v (%T)", tc.expr, got, got, tc.want, tc.want)
			}
		})
	}
}

// TestClosedExprValidationRefusesUnsupported (T-E2 unit half) proves validation
// refuses any kind/op/field outside the closed subset with ErrUnsupportedNode.
// The lowering-level refusal (buildUnits, zero journal) is proven end-to-end in
// loop_test.go's TestUnsupportedExprRefusedAtLowering.
func TestClosedExprValidationRefusesUnsupported(t *testing.T) {
	for _, tc := range []struct {
		name string
		expr string
	}{
		{"op-in", op("in", lit(1), `{"kind":"array","elements":[]}`)},
		{"kind-call", `{"kind":"call","name":"len","args":[]}`},
		{"kind-array", `{"kind":"array","elements":[]}`},
		{"kind-member", `{"kind":"member","base":{"kind":"ref","name":"x"},"name":"id"}`},
		{"ref-field-error", `{"kind":"ref","name":"draft","field":"error"}`},
		{"ref-field-reason", `{"kind":"ref","name":"draft","field":"reason"}`},
		{"op-arith-plus", op("+", lit(1), lit(2))},
		{"op-ternary", `{"kind":"operator","op":"?:","operands":[{"kind":"literal","value":true},{"kind":"literal","value":1},{"kind":"literal","value":2}]}`},
		{"op-bad-arity", `{"kind":"operator","op":"==","operands":[{"kind":"literal","value":1}]}`},
		{"kind-unknown", `{"kind":"handleConstruct","typeName":"Bead","id":{"kind":"literal","value":"x"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateClosedExpr(json.RawMessage(tc.expr)); err == nil {
				t.Fatalf("validateClosedExpr(%s) = nil, want ErrUnsupportedNode", tc.expr)
			} else if !errors.Is(err, ErrUnsupportedNode) {
				t.Fatalf("validateClosedExpr(%s) = %v, want ErrUnsupportedNode", tc.expr, err)
			}
			// A supported expression validates cleanly.
		})
	}
	if err := validateClosedExpr(json.RawMessage(op("||",
		op("==", `{"kind":"ref","name":"draft","field":"outcome"}`, lit("pass")),
		op(">=", `{"kind":"ref","name":"iteration"}`, lit(3))))); err != nil {
		t.Fatalf("validateClosedExpr of the dogfood cond errored: %v", err)
	}
}

// --- expr construction helpers ---------------------------------------------

func lit(v any) string {
	b, _ := json.Marshal(v)
	return `{"kind":"literal","value":` + string(b) + `}`
}

func op(o string, a, b string) string {
	return `{"kind":"operator","op":"` + o + `","operands":[` + a + `,` + b + `]}`
}

func opNot(a string) string {
	return `{"kind":"operator","op":"!","operands":[` + a + `]}`
}
