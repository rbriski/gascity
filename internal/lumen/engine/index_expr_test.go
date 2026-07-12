package engine

import (
	"errors"
	"testing"
)

// TestMatchIndexPreGrammar pins §1.2.2: the pre-grammar matches a lumen ident IMMEDIATELY
// bracketed and spanning the rest of the string (kebab included); a space before the
// bracket, a forged '/'/':' base, or a non-bracketed literal does NOT match (verbatim).
func TestMatchIndexPreGrammar(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{"items[0]", true},
		{"items[iteration - 1]", true},
		{"work-items[iteration - 1]", true},
		{"x[i]", true},
		{"x[i + 1]", true},    // pre-grammar hit (strict rejects it later)
		{"see [docs]", false}, // space before '[' — verbatim
		{"just text", false},
		{"items/x[0]", false}, // forged '/' base — charset excludes it
		{"a:b[0]", false},     // forged ':' base
		{"nobrackets", false},
		{"items[0] tail", false}, // bracket does not span the rest
	} {
		if got := matchIndexPreGrammar(tc.in); got != tc.want {
			t.Errorf("matchIndexPreGrammar(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestParseIndexExpr pins §1.2.3: the strict grammar base '[' (int | ident | ident ws '-'
// ws int) ']' — NO '+', subtraction requires whitespace on BOTH sides (so kebab idents
// survive as plain idents).
func TestParseIndexExpr(t *testing.T) {
	for _, tc := range []struct {
		in       string
		wantOK   bool
		base     string
		hasLit   bool
		litIndex int
		idxIdent string
		subtract int
	}{
		{in: "x[0]", wantOK: true, base: "x", hasLit: true, litIndex: 0},
		{in: "x[42]", wantOK: true, base: "x", hasLit: true, litIndex: 42},
		{in: "x[i]", wantOK: true, base: "x", idxIdent: "i"},
		{in: "items[iteration - 1]", wantOK: true, base: "items", idxIdent: "iteration", subtract: 1},
		{in: "work-items[iteration - 1]", wantOK: true, base: "work-items", idxIdent: "iteration", subtract: 1},
		{in: "x[a-1]", wantOK: true, base: "x", idxIdent: "a-1"}, // no ws → kebab ident, NOT subtraction
		{in: "x[i + 1]", wantOK: false},                          // '+' unsupported
		{in: "x[i -1]", wantOK: false},                           // ws before only — stricter than reference
		{in: "x[]", wantOK: false},
		{in: "x[i][j]", wantOK: false}, // nested indexing unsupported
		{in: "see [docs]", wantOK: false},
	} {
		pi, ok := parseIndexExpr(tc.in)
		if ok != tc.wantOK {
			t.Errorf("parseIndexExpr(%q) ok = %v, want %v", tc.in, ok, tc.wantOK)
			continue
		}
		if !tc.wantOK {
			continue
		}
		if pi.base != tc.base || pi.hasLit != tc.hasLit || pi.litIndex != tc.litIndex ||
			pi.idxIdent != tc.idxIdent || pi.subtract != tc.subtract {
			t.Errorf("parseIndexExpr(%q) = %+v, want base=%q hasLit=%v lit=%d ident=%q sub=%d",
				tc.in, pi, tc.base, tc.hasLit, tc.litIndex, tc.idxIdent, tc.subtract)
		}
	}
}

// TestRenderIndexExpr pins §1.2.4 render semantics: a present JSON-array base indexes
// zero-based; the index ident resolves from scope with '-' arithmetic; every failure row
// returns *indexRenderError (base absent, base not an array, index absent/non-integral,
// out of range), and a present-EMPTY base is distinct from an ABSENT base.
//
// INS catalog annotation (ga-ospbql): the base-MISS row deliberately uses `missing`, an
// UNDECLARED name. Post-seeding a DECLARED input never misses — an optional-unbound declared
// input seeds present-null → baseScope "" → a present-EMPTY base (the out-of-range detail row),
// NOT base-absent — so only an undeclared name can exercise the absent-base arm here.
func TestRenderIndexExpr(t *testing.T) {
	scope := map[string]string{
		"items":     `["alpha","beta"]`,
		"empty":     `[]`,
		"notarr":    `"scalar"`,
		"iteration": "2",
		"i":         "0",
		"frac":      "1.5",
	}
	mustParse := func(s string) parsedIndex {
		pi, ok := parseIndexExpr(s)
		if !ok {
			t.Fatalf("parseIndexExpr(%q) failed", s)
		}
		return pi
	}
	// success rows
	for _, tc := range []struct{ expr, want string }{
		{"items[0]", "alpha"},
		{"items[1]", "beta"},
		{"items[i]", "alpha"},
		{"items[iteration - 1]", "beta"},
	} {
		got, err := renderIndexExpr(mustParse(tc.expr), scope)
		if err != nil {
			t.Errorf("renderIndexExpr(%q): %v", tc.expr, err)
		} else if got != tc.want {
			t.Errorf("renderIndexExpr(%q) = %q, want %q", tc.expr, got, tc.want)
		}
	}
	// failure rows — each must be an *indexRenderError, with miss != present-empty details
	var ire *indexRenderError
	absent, err := renderIndexExpr(mustParse("missing[0]"), scope)
	if _, _ = absent, err; !errors.As(err, &ire) {
		t.Fatalf("absent base: err = %v, want *indexRenderError", err)
	}
	absentDetail := ire.detail
	if _, err := renderIndexExpr(mustParse("empty[0]"), scope); !errors.As(err, &ire) {
		t.Fatalf("present-empty base: err = %v, want *indexRenderError", err)
	}
	if ire.detail == absentDetail {
		t.Errorf("miss vs present-empty share detail %q — must differ (§2.7)", ire.detail)
	}
	for _, tc := range []struct{ expr, why string }{
		{"notarr[0]", "base not a JSON array"},
		{"items[9]", "out of range"},
		{"items[frac]", "non-integral index ident"},
		{"items[nope]", "absent index ident"},
	} {
		if _, err := renderIndexExpr(mustParse(tc.expr), scope); !errors.As(err, &ire) {
			t.Errorf("%s: err = %v, want *indexRenderError", tc.why, err)
		}
	}
	// NEGATIVE index ('-' arithmetic below zero: i=0, i - 2 → -2): out of range, never a
	// panic on a negative slice index — the idx < 0 half of the range check.
	if _, err := renderIndexExpr(mustParse("items[i - 2]"), scope); !errors.As(err, &ire) {
		t.Fatalf("negative index: err = %v, want *indexRenderError", err)
	} else if ire.detail != `index -2 out of range for "items" (length 2)` {
		t.Errorf("negative-index detail = %q, want the negative out-of-range detail", ire.detail)
	}
}
