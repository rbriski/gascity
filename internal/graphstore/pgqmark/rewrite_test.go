package pgqmark

import "testing"

// TestRewritePlaceholders exercises the qmark→$N rewriter across every
// literal/comment context it must skip. This shim is the P6 #2 risk: a rewrite
// bug binds the wrong parameter, so the cases below pin placeholder numbering,
// literal-awareness (strings, E-strings, identifiers, dollar-quotes), comment
// skipping, and pass-through of existing $N.
func TestRewritePlaceholders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"none", "SELECT 1", "SELECT 1"},
		{"single", "SELECT * FROM t WHERE a = ?", "SELECT * FROM t WHERE a = $1"},
		{
			"multiple",
			"INSERT INTO t(a,b,c) VALUES(?,?,?)",
			"INSERT INTO t(a,b,c) VALUES($1,$2,$3)",
		},
		{
			"insert_on_conflict",
			"INSERT INTO graph_meta(key, value) VALUES('city_id', ?) ON CONFLICT(key) DO NOTHING",
			"INSERT INTO graph_meta(key, value) VALUES('city_id', $1) ON CONFLICT(key) DO NOTHING",
		},
		{
			// A ? inside a single-quoted literal must NOT be rewritten; the real
			// binds after it must number from $1.
			"qmark_in_string",
			"SELECT '? literal ?' , ? , ?",
			"SELECT '? literal ?' , $1 , $2",
		},
		{
			"escaped_quote_in_string",
			"SELECT 'it''s a ? mark' , ?",
			"SELECT 'it''s a ? mark' , $1",
		},
		{
			"qmark_in_identifier",
			`SELECT "weird?col" FROM t WHERE a = ?`,
			`SELECT "weird?col" FROM t WHERE a = $1`,
		},
		{
			"escaped_quote_in_identifier",
			`SELECT "a""?b" , ?`,
			`SELECT "a""?b" , $1`,
		},
		{
			"line_comment",
			"SELECT ? -- trailing ? comment\n, ?",
			"SELECT $1 -- trailing ? comment\n, $2",
		},
		{
			"block_comment",
			"SELECT ? /* a ? in a comment */ , ?",
			"SELECT $1 /* a ? in a comment */ , $2",
		},
		{
			"nested_block_comment",
			"SELECT ? /* outer /* inner ? */ still ? */ , ?",
			"SELECT $1 /* outer /* inner ? */ still ? */ , $2",
		},
		{
			"dollar_quote_empty_tag",
			"SELECT $$ body with ? inside $$ , ?",
			"SELECT $$ body with ? inside $$ , $1",
		},
		{
			"dollar_quote_named_tag",
			"SELECT $tag$ ? and $$ inner $$ ? $tag$ , ?",
			"SELECT $tag$ ? and $$ inner $$ ? $tag$ , $1",
		},
		{
			// A plpgsql function body (dollar-quoted, contains RAISE + a ? that
			// is not a placeholder) must survive untouched.
			"plpgsql_body",
			"CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'no ? here'; END; $$ LANGUAGE plpgsql",
			"CREATE FUNCTION f() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'no ? here'; END; $$ LANGUAGE plpgsql",
		},
		{
			// The lockStream statement is authored with `?` and becomes $1; a
			// statement that already uses $N (and no `?`) is left untouched. The
			// shim's contract is "rewrite ? from $1"; the substrate never mixes
			// `?` and `$N` in one statement, so no renumbering of existing $N is
			// attempted.
			"lockstream_qmark_becomes_dollar1",
			"SELECT pg_advisory_xact_lock(hashtextextended(?, 0))",
			"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		},
		{
			"e_string_backslash_escape",
			`SELECT E'line\'? still string' , ?`,
			`SELECT E'line\'? still string' , $1`,
		},
		{
			// Lowercase e-string.
			"e_string_lowercase",
			`SELECT e'a ? b' , ?`,
			`SELECT e'a ? b' , $1`,
		},
		{
			// A trailing `e` that is part of an identifier is NOT an E-string
			// prefix; the following quote opens a normal string.
			"identifier_ending_e_then_string",
			`SELECT type WHERE type = 'lumen' AND x = ?`,
			`SELECT type WHERE type = 'lumen' AND x = $1`,
		},
		{
			"ten_placeholders_numbering",
			"VALUES(?,?,?,?,?,?,?,?,?,?)",
			"VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)",
		},
		{
			"unclosed_string_is_copied",
			"SELECT 'never closed ? ",
			"SELECT 'never closed ? ",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rewrite(tc.in); got != tc.want {
				t.Errorf("rewrite(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRewriteIdempotentOnNonQmark confirms strings without `?` are returned
// unchanged (the fast path), including ones that contain $-syntax.
func TestRewriteIdempotentOnNonQmark(t *testing.T) {
	for _, s := range []string{
		"SELECT 1",
		"SELECT pg_advisory_xact_lock(hashtextextended($1, 0))",
		"CREATE TABLE t (x TEXT COLLATE \"C\")",
		"SELECT $$body$$",
	} {
		if got := rewrite(s); got != s {
			t.Errorf("rewrite(%q) = %q, want unchanged", s, got)
		}
	}
}

// TestDollarTag pins the positional-vs-dollar-quote discrimination.
func TestDollarTag(t *testing.T) {
	cases := []struct {
		in      string
		wantTag string
		wantOK  bool
	}{
		{"$$rest", "$$", true},
		{"$tag$rest", "$tag$", true},
		{"$_x1$rest", "$_x1$", true},
		{"$1", "", false},    // positional param
		{"$2, x", "", false}, // positional param
		{"$ notag", "", false},
		{"$", "", false},
	}
	for _, tc := range cases {
		gotTag, gotOK := dollarTag(tc.in, 0)
		if gotOK != tc.wantOK || gotTag != tc.wantTag {
			t.Errorf("dollarTag(%q) = (%q,%v), want (%q,%v)", tc.in, gotTag, gotOK, tc.wantTag, tc.wantOK)
		}
	}
}
