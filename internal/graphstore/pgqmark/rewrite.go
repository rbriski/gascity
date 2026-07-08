package pgqmark

import (
	"strconv"
	"strings"
)

// rewrite replaces each `?` positional placeholder with a `$N` placeholder,
// numbering them left to right from $1. It is literal-aware: a `?` is only
// rewritten in "normal" SQL text. Inside any of the following it is copied
// verbatim, and the region as a whole is skipped:
//
//   - single-quoted string literals '…' (with ” as an embedded quote)
//   - E-strings E'…' / e'…' (backslash escapes the next byte)
//   - double-quoted identifiers "…" (with "" as an embedded quote)
//   - dollar-quoted strings $tag$ … $tag$ (including the empty tag $$…$$)
//   - line comments -- … to end of line
//   - block comments /* … */ (nesting)
//
// Bytes ≥ 0x80 (UTF-8 continuation bytes) never collide with the ASCII
// delimiters scanned here, so byte-wise scanning is safe for UTF-8 input. The
// substrate uses only positional `?`, so rewriting every non-literal `?`
// matches the intended binds exactly (the same rule sqlx's Rebind applies).
func rewrite(query string) string {
	// Fast path: nothing to do when there is no placeholder at all.
	if !strings.ContainsRune(query, '?') {
		return query
	}

	var b strings.Builder
	b.Grow(len(query) + 8)
	n := 0
	i := 0
	L := len(query)

	for i < L {
		c := query[i]
		switch {
		case c == '\'':
			i = copyQuoted(&b, query, i, '\'', isEStringStart(query, i))
		case c == '"':
			i = copyQuoted(&b, query, i, '"', false)
		case c == '-' && i+1 < L && query[i+1] == '-':
			i = copyLineComment(&b, query, i)
		case c == '/' && i+1 < L && query[i+1] == '*':
			i = copyBlockComment(&b, query, i)
		case c == '$':
			if tag, ok := dollarTag(query, i); ok {
				i = copyDollarQuoted(&b, query, i, tag)
			} else {
				b.WriteByte(c)
				i++
			}
		case c == '?':
			n++
			b.WriteByte('$')
			b.WriteString(strconv.Itoa(n))
			i++
		default:
			b.WriteByte(c)
			i++
		}
	}
	return b.String()
}

// copyQuoted copies a quoted region beginning at the opening quote query[start]
// (which is q) through its closing quote, returning the index just past it. A
// doubled quote (q q) is an embedded quote and does not close the region. When
// esc is true (E-strings) a backslash escapes the following byte. An unclosed
// region is copied to end of input.
func copyQuoted(b *strings.Builder, q string, start int, quote byte, esc bool) int {
	L := len(q)
	b.WriteByte(q[start]) // opening quote
	i := start + 1
	for i < L {
		ch := q[i]
		if esc && ch == '\\' && i+1 < L {
			b.WriteByte(ch)
			b.WriteByte(q[i+1])
			i += 2
			continue
		}
		if ch == quote {
			if i+1 < L && q[i+1] == quote { // doubled → embedded quote
				b.WriteByte(quote)
				b.WriteByte(quote)
				i += 2
				continue
			}
			b.WriteByte(quote)
			return i + 1
		}
		b.WriteByte(ch)
		i++
	}
	return L
}

// copyLineComment copies a `-- …` comment through (but not past) the newline.
func copyLineComment(b *strings.Builder, q string, start int) int {
	L := len(q)
	i := start
	for i < L && q[i] != '\n' {
		b.WriteByte(q[i])
		i++
	}
	return i
}

// copyBlockComment copies a `/* … */` comment, honoring PostgreSQL's nesting.
func copyBlockComment(b *strings.Builder, q string, start int) int {
	L := len(q)
	i := start
	depth := 0
	for i < L {
		if q[i] == '/' && i+1 < L && q[i+1] == '*' {
			b.WriteString("/*")
			i += 2
			depth++
			continue
		}
		if q[i] == '*' && i+1 < L && q[i+1] == '/' {
			b.WriteString("*/")
			i += 2
			depth--
			if depth == 0 {
				return i
			}
			continue
		}
		b.WriteByte(q[i])
		i++
	}
	return L
}

// dollarTag reports whether query[start] ('$') opens a dollar-quote and returns
// the full opening tag ("$…$"). A tag is `$` + an optional identifier (not
// starting with a digit) + `$`; `$$` (empty tag) qualifies. A `$` followed by a
// digit ($1, $2 — a positional parameter) or by anything that is not a valid
// tag is not a dollar-quote.
func dollarTag(q string, start int) (string, bool) {
	L := len(q)
	j := start + 1
	for j < L {
		c := q[j]
		if c == '$' {
			return q[start : j+1], true
		}
		isAlpha := c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		isDigit := c >= '0' && c <= '9'
		if isAlpha || isDigit {
			if isDigit && j == start+1 { // $1 … positional parameter
				return "", false
			}
			j++
			continue
		}
		return "", false
	}
	return "", false
}

// copyDollarQuoted copies a dollar-quoted region that opens with tag at index
// start, through the matching closing tag, returning the index just past it. An
// unterminated body is copied to end of input.
func copyDollarQuoted(b *strings.Builder, q string, start int, tag string) int {
	L := len(q)
	b.WriteString(tag)
	i := start + len(tag)
	for i < L {
		if i+len(tag) <= L && q[i:i+len(tag)] == tag {
			b.WriteString(tag)
			return i + len(tag)
		}
		b.WriteByte(q[i])
		i++
	}
	return L
}

// isEStringStart reports whether the single quote at index i opens an E-string,
// i.e. it is immediately preceded by a standalone `E`/`e` token. Our SQL uses no
// E-strings; this keeps the rewriter correct for callers that might.
func isEStringStart(q string, i int) bool {
	if i == 0 {
		return false
	}
	c := q[i-1]
	if c != 'e' && c != 'E' {
		return false
	}
	if i-1 == 0 {
		return true
	}
	p := q[i-2]
	// If the char before E is part of an identifier, the E is a suffix of that
	// identifier, not an E-string prefix.
	if p == '_' || (p >= 'a' && p <= 'z') || (p >= 'A' && p <= 'Z') || (p >= '0' && p <= '9') {
		return false
	}
	return true
}
