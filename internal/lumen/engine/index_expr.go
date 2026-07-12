package engine

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// This file is the DO-TEMPLATE indexed-member interpolation surface (SLX §1.2): a
// fork-invented render semantic for `{{ base[index] }}` interpolations the reference
// compiler cannot lower to a structured index kind and therefore emits as a verbatim
// {kind:"literal", value:"<source>"} part (§0.3). The engine defines the semantic here —
// pre-grammar (is this literal an index expression?), strict grammar (can we render it?),
// and the render itself — plus the loud-wall lower sweep that refuses an out-of-subset
// index on the routes that CANNOT render it (exec, silent lets), and the strict-fail on
// the do route. Data positions (lit values, env bindings, dispatch subjects) are
// deliberately untouched — they are indistinguishable from genuine strings.

// indexShapeRe is the pre-grammar AND the strict grammar's base split: a lumen ident
// (kebab included; '/' and ':' excluded by construction, so a forged cross-namespace ref
// can never form an index base) IMMEDIATELY followed by a bracketed tail spanning the rest
// of the string.
var indexShapeRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\[(.*)\]$`)

// indexIntRe / indexIdentRe / indexSubRe are the strict inner-index forms: a bare int, a
// bare ident, or `ident ws '-' ws int` (subtraction with whitespace on BOTH sides — stricter
// than the reference tokenizer's before-only rule, DELIBERATELY, so a kebab ident like
// `a-1` reads as an ident, not a subtraction). NO '+' form (corpus-absent, premature).
var (
	indexIntRe   = regexp.MustCompile(`^[0-9]+$`)
	indexIdentRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_-]*$`)
	indexSubRe   = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_-]*)\s+-\s+([0-9]+)$`)
)

// parsedIndex is a strictly-parsed do-template index interpolation base[index]. When hasLit
// is set the index is a bare int literal (litIndex); otherwise it resolves the scope value
// of idxIdent and subtracts subtract (0 when there is no `- N`).
type parsedIndex struct {
	base     string
	hasLit   bool
	litIndex int
	idxIdent string
	subtract int
}

// indexRenderError is the typed sentinel a do-template index interpolation returns when the
// index cannot be resolved at render time (⚑B2, §1.2.4): the base key is absent, the base is
// not a JSON array, the index ident is absent/non-integral, or the index is out of range.
// Both do drivers catch it (errors.As) and SETTLE the step failed{detail}, non-retryable,
// rather than erroring the dispatch. Every other render error keeps today's error path — the
// conversion matches ONLY this sentinel.
type indexRenderError struct{ detail string }

// Error returns the render-failure detail carried into the settled outcome.
func (e *indexRenderError) Error() string { return e.detail }

// matchIndexPreGrammar reports whether a literal part looks like an index expression: a
// lumen ident immediately bracketed, spanning the whole string (§1.2.2). It is a SUPERSET
// of the strict grammar — a pre-grammar hit that fails the strict parse is a malformed
// index the lower sweep refuses (do) rather than misrendering verbatim.
func matchIndexPreGrammar(s string) bool {
	return indexShapeRe.MatchString(s)
}

// parseIndexExpr strictly parses base[index] with index := int | ident | ident ws '-' ws
// int (§1.2.3). It returns ok=false for any other inner shape (a '+' form, whitespace on
// only one side of '-', nested brackets, an empty index). Every accepted form is
// fork-invented semantics upstream must later match.
func parseIndexExpr(s string) (parsedIndex, bool) {
	m := indexShapeRe.FindStringSubmatch(s)
	if m == nil {
		return parsedIndex{}, false
	}
	base, inner := m[1], m[2]
	switch {
	case indexIntRe.MatchString(inner):
		n, err := strconv.Atoi(inner)
		if err != nil {
			return parsedIndex{}, false
		}
		return parsedIndex{base: base, hasLit: true, litIndex: n}, true
	case indexIdentRe.MatchString(inner):
		return parsedIndex{base: base, idxIdent: inner}, true
	default:
		if sm := indexSubRe.FindStringSubmatch(inner); sm != nil {
			n, err := strconv.Atoi(sm[2])
			if err != nil {
				return parsedIndex{}, false
			}
			return parsedIndex{base: base, idxIdent: sm[1], subtract: n}, true
		}
	}
	return parsedIndex{}, false
}

// renderIndexExpr renders a do-template index interpolation against the render scope
// (engine-defined, §1.2.4). base presence is a scope KEY-EXISTENCE check FIRST — only a
// PRESENT key decodes (decodeArrayString("") is empty-VALID, so a base-miss must not
// masquerade as out-of-range: miss != present-empty). A present base parses via the
// decodeArrayString convention; the index resolves (with '-' arithmetic), must be integral,
// is ZERO-BASED, and the element string renders verbatim. Every failure returns
// *indexRenderError so the do drivers settle the step failed rather than error the dispatch.
func renderIndexExpr(pi parsedIndex, scope map[string]string) (string, error) {
	raw, ok := scope[pi.base]
	if !ok {
		return "", &indexRenderError{detail: fmt.Sprintf("index base %q is not in scope", pi.base)}
	}
	elems, valid, _ := decodeArrayString(raw)
	if !valid {
		return "", &indexRenderError{detail: fmt.Sprintf("index base %q is not a JSON array", pi.base)}
	}
	idx, err := pi.indexValue(scope)
	if err != nil {
		return "", err
	}
	if idx < 0 || idx >= len(elems) {
		return "", &indexRenderError{detail: fmt.Sprintf("index %d out of range for %q (length %d)", idx, pi.base, len(elems))}
	}
	return elems[idx], nil
}

// indexValue computes the zero-based index for a parsedIndex against scope: a bare literal
// int directly, or the scope value of the index ident (which must parse integral — a float
// is accepted iff it is integral) minus the subtraction.
func (pi parsedIndex) indexValue(scope map[string]string) (int, error) {
	if pi.hasLit {
		return pi.litIndex, nil
	}
	raw, ok := scope[pi.idxIdent]
	if !ok {
		return 0, &indexRenderError{detail: fmt.Sprintf("index %q is not in scope", pi.idxIdent)}
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || f != float64(int64(f)) {
		return 0, &indexRenderError{detail: fmt.Sprintf("index %q = %q is not an integer", pi.idxIdent, raw)}
	}
	return int(int64(f)) - pi.subtract, nil
}

// exprLiteralString returns the string value of a {kind:"literal", value:<string>} expression
// node, unwrapping a single {kind:"interp", expr:...} template-part layer first (the shape a
// do/interp part carries). It reports false for any other kind (ref/member/nested interp) or a
// non-string literal, so only a bare literal string is a candidate for index rendering — a
// genuine ref/member interpolation renders through the existing evalValue path untouched.
func exprLiteralString(raw json.RawMessage) (string, bool) {
	var probe struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
		Expr  json.RawMessage `json:"expr"`
	}
	if json.Unmarshal(raw, &probe) != nil {
		return "", false
	}
	switch probe.Kind {
	case "interp":
		return exprLiteralString(probe.Expr)
	case "literal":
		var s string
		if json.Unmarshal(probe.Value, &s) == nil {
			return s, true
		}
	}
	return "", false
}

// collectIndexPreGrammarLiterals returns the literal-part source strings under a node/body
// raw that MATCH the index pre-grammar (candidate index interpolations, §1.2.5). It inspects
// a bare `value` (a lit leaf / value-form interp), direct `parts`, a `template`.parts, and a
// nested `body` (a do/exec body carries its parts under body.template.parts). Only a literal
// STRING matching the pre-grammar is a candidate; a text part, a ref/member interp, or a
// genuine literal contributes nothing.
func collectIndexPreGrammarLiterals(raw map[string]json.RawMessage) []string {
	var out []string
	consider := func(src string, ok bool) {
		if ok && matchIndexPreGrammar(src) {
			out = append(out, src)
		}
	}
	if v, ok := raw["value"]; ok {
		consider(exprLiteralString(v))
	}
	if p, ok := raw["parts"]; ok {
		for _, src := range partLiteralStrings(p) {
			consider(src, true)
		}
	}
	if tmpl, ok := raw["template"]; ok {
		var t struct {
			Parts json.RawMessage `json:"parts"`
		}
		if json.Unmarshal(tmpl, &t) == nil && t.Parts != nil {
			for _, src := range partLiteralStrings(t.Parts) {
				consider(src, true)
			}
		}
	}
	if body, ok := raw["body"]; ok {
		var bm map[string]json.RawMessage
		if json.Unmarshal(body, &bm) == nil {
			out = append(out, collectIndexPreGrammarLiterals(bm)...)
		}
	}
	return out
}

// partLiteralStrings decodes a template parts array and returns the literal-string value of
// each part that is (directly, or via an interp wrapper) a {kind:"literal"} string.
func partLiteralStrings(partsRaw json.RawMessage) []string {
	var parts []json.RawMessage
	if json.Unmarshal(partsRaw, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		if src, ok := exprLiteralString(p); ok {
			out = append(out, src)
		}
	}
	return out
}

// sweepIndexParts refuses an out-of-subset indexed interpolation the engine cannot render
// (§1.2.5, the loud-wall discipline). A do route passes strictAllowed=true: a pre-grammar
// hit that STRICT-parses renders at runtime, so only a strict-FAIL is refused. An exec route
// and a silent lit/interp leaf pass false — they render via interpolate(body.raw) /
// evalValue and CANNOT index, so ANY pre-grammar hit is refused regardless of strict-parse.
// The error wraps ErrUnsupportedNode so the enqueue gate triages it (and the run-body /
// dispatch-arm dry-run mints wrap it with their provenance).
func sweepIndexParts(raw map[string]json.RawMessage, strictAllowed bool) error {
	for _, src := range collectIndexPreGrammarLiterals(raw) {
		if strictAllowed {
			if _, ok := parseIndexExpr(src); ok {
				continue
			}
		}
		return fmt.Errorf("%w: interp index expr %q", ErrUnsupportedNode, src)
	}
	return nil
}
