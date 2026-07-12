package engine

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf16"
)

// This file is the CLOSED-EXPRESSION evaluator the attempt-loop arm (retry /
// repeat) uses to decide whether to re-run — a total, side-effect-free interpreter
// over the subset of the Lumen expression grammar the dogfood loop conditions need
// (blueprint 17 §5-L5). It is the single place any retry/repeat JUDGMENT lives: the
// driver evaluates the authored `until` condition (repeat) and `attempts`
// expression (retry) through THIS evaluator over journaled attempt outcomes, so no
// Go line branches on an outcome VALUE to decide a re-run (keep-judgment-out-of-Go,
// T-J1). It mirrors the reference runner (formula-language
// packages/core/src/index.ts evaluateLumenExpr / compareValues / isTruthy) exactly
// for the supported subset; anything outside the subset is refused at LOWERING
// (validateClosedExpr, before any effect), never a runtime surprise.
//
// The closed subset (blueprint §1.3):
//   - kinds:    literal, ref (field "" = bare value, or "outcome"), operator,
//               call (length only — the one length-shaped cond the *-shared corpus needs)
//   - operators: == != >= <= > < && || !
// Every other kind (array, object, member, handleConstruct, channel-facet), call name
// (json/string/join stay render helpers, outside the cond subset), operator (in, ?:,
// + - * / %), and ref field (error, kind, result, reason) is refused with
// ErrUnsupportedNode. Arrays and maps are first-class VALUES here (a ref may resolve to
// one), but only `length` consumes them in a cond.

// closedRefFields is the closed set of ref fields the loop conditions may read.
// A bare ref ("") reads the binding's value (folded output / iteration number /
// input); ".outcome" reads the settled outcome string. Every other field
// (error / kind / result / reason and the rest) is refused at lowering.
func closedRefFieldOK(field string) bool { return field == "" || field == "outcome" }

// closedOpOK reports whether op is a supported comparison / logical operator.
func closedOpOK(op string) bool {
	switch op {
	case "==", "!=", ">=", "<=", ">", "<", "&&", "||", "!":
		return true
	default:
		return false
	}
}

// loopScope resolves a closed-expression ref against a repeat/retry loop's
// evaluation context. Resolution precedence mirrors the reference runner:
// iterationName → body binding → run input → settled node outputs. Every source
// is journal- or input-derived, so an evaluation is a deterministic function of
// (fold state, IR, input, iteration) — DROP+refold and crash-re-Advance converge.
type loopScope struct {
	// iterationName is the repeat loop's 1-based counter binding (retry sets "").
	iterationName string
	iteration     int
	// bodyName / bodyOutcome / bodyOutput are the just-settled attempt's binding:
	// bare ref → bodyOutput (folded output string), ".outcome" → bodyOutcome.
	bodyName    string
	bodyOutcome string
	bodyOutput  string
	// input is the run input (typed values: float64 for JSON numbers, string, …).
	input map[string]any
	// nodeOutputs / nodeOutcomes carry the run's other settled nodes (the same
	// scope Run threads): bare ref → output string, ".outcome" → outcome string.
	nodeOutputs  map[string]string
	nodeOutcomes map[string]string
}

// resolve returns a ref name's (value, outcome) and whether it resolved. An
// unresolved ref folds to null (the reference: context.get miss → null).
func (s loopScope) resolve(name string) (value any, outcome string, found bool) {
	switch name {
	case "":
		return nil, "", false
	case s.iterationName:
		// The iteration counter is a step result {value: N, outcome: "pass"}.
		return float64(s.iteration), OutcomePass, true
	case s.bodyName:
		return s.bodyOutput, s.bodyOutcome, true
	}
	if v, ok := s.input[name]; ok {
		// Run input is a pre-settled binding; its outcome is pass.
		return normalizeExprValue(v), OutcomePass, true
	}
	if out, ok := s.nodeOutputs[name]; ok {
		return out, s.nodeOutcomes[name], true
	}
	return nil, "", false
}

// evalClosedExpr evaluates a closed expression against scope, returning one of
// nil | bool | float64 | string. It is total over the validated subset; a kind /
// op / field outside the subset returns ErrUnsupportedNode (defensive — lowering
// already refused it). It performs no I/O and reads no clock.
func evalClosedExpr(raw json.RawMessage, scope loopScope) (any, error) {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil, fmt.Errorf("lumen: closed expr: %w", err)
	}
	switch head.Kind {
	case "literal":
		var lit struct {
			Value json.RawMessage `json:"value"`
		}
		if err := json.Unmarshal(raw, &lit); err != nil {
			return nil, fmt.Errorf("lumen: closed expr literal: %w", err)
		}
		return decodeLiteralValue(lit.Value), nil

	case "ref":
		var r struct {
			Name  string `json:"name"`
			Field string `json:"field"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return nil, fmt.Errorf("lumen: closed expr ref: %w", err)
		}
		if !closedRefFieldOK(r.Field) {
			return nil, fmt.Errorf("%w: closed expr ref field %q", ErrUnsupportedNode, r.Field)
		}
		value, outcome, found := scope.resolve(r.Name)
		if !found {
			return nil, nil
		}
		if r.Field == "outcome" {
			return outcome, nil
		}
		return value, nil

	case "operator":
		return evalClosedOperator(raw, scope)

	case "call":
		return evalClosedCall(raw, scope)

	default:
		return nil, fmt.Errorf("%w: closed expr kind %q", ErrUnsupportedNode, head.Kind)
	}
}

// evalClosedCall evaluates the one supported call — `length(x)` — over the single
// evaluated argument. It is the defensive twin of validateClosedExpr's call arm (lowering
// already refused a non-length name or a wrong arity).
func evalClosedCall(raw json.RawMessage, scope loopScope) (any, error) {
	var c struct {
		Name string            `json:"name"`
		Args []json.RawMessage `json:"args"`
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("lumen: closed expr call: %w", err)
	}
	if c.Name != "length" {
		return nil, fmt.Errorf("%w: closed expr call %q", ErrUnsupportedNode, c.Name)
	}
	if len(c.Args) != 1 {
		return nil, fmt.Errorf("%w: closed expr call \"length\" wants 1 arg, got %d", ErrUnsupportedNode, len(c.Args))
	}
	arg, err := evalClosedExpr(c.Args[0], scope)
	if err != nil {
		return nil, err
	}
	return lengthOf(arg), nil
}

// lengthOf mirrors the reference callExprFunction("length", …): an array or a map returns
// its element/key count; a string returns its UTF-16 code-unit count (JS `.length`, NOT
// bytes or rune count); null/number/bool return 0.
func lengthOf(v any) float64 {
	switch x := v.(type) {
	case []any:
		return float64(len(x))
	case map[string]any:
		return float64(len(x))
	case string:
		return float64(len(utf16.Encode([]rune(x))))
	default:
		return 0
	}
}

// evalClosedOperator evaluates a comparison / logical operator. Like the
// reference, it evaluates ALL operands (the operands are pure, so there is no
// short-circuit to observe), then applies the operator.
func evalClosedOperator(raw json.RawMessage, scope loopScope) (any, error) {
	var o struct {
		Op       string            `json:"op"`
		Operands []json.RawMessage `json:"operands"`
	}
	if err := json.Unmarshal(raw, &o); err != nil {
		return nil, fmt.Errorf("lumen: closed expr operator: %w", err)
	}
	if !closedOpOK(o.Op) {
		return nil, fmt.Errorf("%w: closed expr op %q", ErrUnsupportedNode, o.Op)
	}
	if o.Op == "!" {
		if len(o.Operands) != 1 {
			return nil, fmt.Errorf("%w: closed expr op %q wants 1 operand, got %d", ErrUnsupportedNode, o.Op, len(o.Operands))
		}
		v, err := evalClosedExpr(o.Operands[0], scope)
		if err != nil {
			return nil, err
		}
		return !isExprTruthy(v), nil
	}
	if len(o.Operands) != 2 {
		return nil, fmt.Errorf("%w: closed expr op %q wants 2 operands, got %d", ErrUnsupportedNode, o.Op, len(o.Operands))
	}
	left, err := evalClosedExpr(o.Operands[0], scope)
	if err != nil {
		return nil, err
	}
	right, err := evalClosedExpr(o.Operands[1], scope)
	if err != nil {
		return nil, err
	}
	switch o.Op {
	case "&&":
		return isExprTruthy(left) && isExprTruthy(right), nil
	case "||":
		return isExprTruthy(left) || isExprTruthy(right), nil
	}
	cmp, nan := compareExprValues(left, right)
	switch o.Op {
	case "==":
		return !nan && cmp == 0, nil
	case "!=":
		return nan || cmp != 0, nil
	case ">=":
		return !nan && cmp >= 0, nil
	case "<=":
		return !nan && cmp <= 0, nil
	case ">":
		return !nan && cmp > 0, nil
	case "<":
		return !nan && cmp < 0, nil
	}
	return nil, fmt.Errorf("%w: closed expr op %q", ErrUnsupportedNode, o.Op)
}

// validateClosedExpr walks a closed expression tree and refuses any kind / op /
// field outside the subset with ErrUnsupportedNode, so a bad formula refuses at
// LOAD (buildUnits, before any append), never at attempt N. It is the structural
// twin of evalClosedExpr.
func validateClosedExpr(raw json.RawMessage) error {
	var head struct {
		Kind string `json:"kind"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return fmt.Errorf("lumen: closed expr: %w", err)
	}
	switch head.Kind {
	case "literal":
		return nil
	case "ref":
		var r struct {
			Field string `json:"field"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("lumen: closed expr ref: %w", err)
		}
		if !closedRefFieldOK(r.Field) {
			return fmt.Errorf("%w: closed expr ref field %q", ErrUnsupportedNode, r.Field)
		}
		return nil
	case "operator":
		var o struct {
			Op       string            `json:"op"`
			Operands []json.RawMessage `json:"operands"`
		}
		if err := json.Unmarshal(raw, &o); err != nil {
			return fmt.Errorf("lumen: closed expr operator: %w", err)
		}
		if !closedOpOK(o.Op) {
			return fmt.Errorf("%w: closed expr op %q", ErrUnsupportedNode, o.Op)
		}
		arity := 2
		if o.Op == "!" {
			arity = 1
		}
		if len(o.Operands) != arity {
			return fmt.Errorf("%w: closed expr op %q wants %d operand(s), got %d", ErrUnsupportedNode, o.Op, arity, len(o.Operands))
		}
		for _, operand := range o.Operands {
			if err := validateClosedExpr(operand); err != nil {
				return err
			}
		}
		return nil
	case "call":
		var c struct {
			Name string            `json:"name"`
			Args []json.RawMessage `json:"args"`
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return fmt.Errorf("lumen: closed expr call: %w", err)
		}
		if c.Name != "length" {
			return fmt.Errorf("%w: closed expr call %q", ErrUnsupportedNode, c.Name)
		}
		if len(c.Args) != 1 {
			return fmt.Errorf("%w: closed expr call \"length\" wants 1 arg, got %d", ErrUnsupportedNode, len(c.Args))
		}
		return validateClosedExpr(c.Args[0])
	default:
		return fmt.Errorf("%w: closed expr kind %q", ErrUnsupportedNode, head.Kind)
	}
}

// decodeLiteralValue decodes a JSON literal into one of nil | bool | float64 |
// string. A non-scalar literal (array/object — never present in a closed cond)
// falls back to its trimmed raw JSON so comparison stays panic-free.
func decodeLiteralValue(raw json.RawMessage) any {
	if len(raw) == 0 {
		return nil
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return strings.TrimSpace(string(raw))
	}
	switch v.(type) {
	case nil, bool, float64, string:
		return v
	default:
		return strings.TrimSpace(string(raw))
	}
}

// normalizeExprValue coerces an input value (decoded from JSON into map[string]any)
// to a comparable scalar. JSON numbers are already float64; arrays and maps pass THROUGH
// as first-class values (§1.1.3 — length reads them, ordered/equality compare coerces them
// via jsString, truthiness reads array length); anything else non-scalar falls back to its
// Go string form.
func normalizeExprValue(v any) any {
	switch x := v.(type) {
	case nil, bool, float64, string:
		return x
	case []any, map[string]any:
		return x
	case int:
		return float64(x)
	case int64:
		return float64(x)
	case json.Number:
		if f, err := x.Float64(); err == nil {
			return f
		}
		return x.String()
	default:
		return fmt.Sprintf("%v", x)
	}
}

// isExprTruthy mirrors the reference isTruthy: null/false falsy; a number falsy
// iff 0 or NaN; a string falsy iff empty; an array falsy iff empty; everything else
// (a map/object, …) truthy.
func isExprTruthy(v any) bool {
	switch x := v.(type) {
	case nil:
		return false
	case bool:
		return x
	case float64:
		return x != 0 && x == x // x == x is false for NaN
	case string:
		return len(x) > 0
	case []any:
		return len(x) > 0
	default:
		return true
	}
}

// compareExprValues mirrors the reference compareValues: it returns (cmp, nan)
// where cmp is -1/0/1 and nan reports the null-vs-non-null case (JS NaN), which
// makes every comparison operator false. Numbers compare numerically; booleans
// by value; everything else by String()-coercion (so "3" and 3 compare equal).
func compareExprValues(left, right any) (cmp int, nan bool) {
	// ⚑B1: Go's == PANICS on two non-comparable dynamic types ([]any / map[string]any).
	// Take the value-equality fast path ONLY when both operands are comparable scalars
	// (nil/bool/float64/string); array/map operands route straight to the jsString-coercion
	// arms below (reference parity: an identity-miss falls to String()).
	if exprScalar(left) && exprScalar(right) && left == right {
		return 0, false
	}
	if left == nil || right == nil {
		return 0, true // null vs non-null ⇒ NaN
	}
	lf, lNum := left.(float64)
	rf, rNum := right.(float64)
	if lNum && rNum {
		if lf < rf {
			return -1, false
		}
		return 1, false // not equal (handled above) ⇒ greater
	}
	lb, lBool := left.(bool)
	rb, rBool := right.(bool)
	if lBool && rBool {
		if lb == rb {
			return 0, false
		}
		if lb {
			return 1, false
		}
		return -1, false
	}
	ls, rs := jsString(left), jsString(right)
	switch {
	case ls == rs:
		return 0, false
	case ls < rs:
		return -1, false
	default:
		return 1, false
	}
}

// jsString mirrors JS String() for the values the evaluator handles: a number renders
// without a trailing ".0", a bool as "true"/"false", a string verbatim, a top-level null as
// "null". An array joins its elements with "," — a nil/undefined element renders EMPTY (NOT
// "null"; `String([null,"a"]) === ",a"`), a nested array recurses with the same join rule,
// and a map element renders "[object Object]" (⚑B3). A top-level map renders "[object
// Object]".
func jsString(v any) string {
	switch x := v.(type) {
	case nil:
		return "null"
	case bool:
		if x {
			return "true"
		}
		return "false"
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	case string:
		return x
	case []any:
		parts := make([]string, len(x))
		for i, e := range x {
			if e == nil {
				parts[i] = "" // JS String() renders a null/undefined array element as ""
				continue
			}
			parts[i] = jsString(e)
		}
		return strings.Join(parts, ",")
	case map[string]any:
		return "[object Object]"
	default:
		return fmt.Sprintf("%v", x)
	}
}

// exprScalar reports whether v is one of the comparable scalar dynamic types (nil, bool,
// float64, string). It guards compareExprValues' == fast path: Go's == panics on two
// []any/map[string]any operands, so those route to the String()-coercion arms instead.
func exprScalar(v any) bool {
	switch v.(type) {
	case nil, bool, float64, string:
		return true
	default:
		return false
	}
}
