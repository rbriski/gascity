// Package canon implements R-CANON: the single canonical JSON encoding used for
// every journal payload, snapshot blob, and golden comparison in the graph
// substrate (02-determinism §5.3). It is the ONLY encoder of payloads — the
// journal stores canonical bytes verbatim and never re-encodes them (I-11).
//
// The canonical form is: UTF-8, object keys sorted lexicographically, no
// insignificant whitespace, deterministic number formatting, NaN/Inf forbidden,
// and an explicit null-vs-absent distinction (nulls are preserved, absent keys
// stay absent). Producing bytes through this package and then hashing them with
// Hash is what makes payload_hash and chain_hash reproducible across processes
// and platforms.
package canon

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Canonicalize parses raw JSON and re-emits it in R-CANON form. It is
// idempotent: Canonicalize(Canonicalize(x)) == Canonicalize(x), and it is
// insensitive to input key order, so two payloads that differ only in object
// key ordering produce byte-identical output. Non-finite numbers (NaN, +Inf,
// -Inf), duplicate object keys, and trailing garbage after the top-level value
// are rejected — a hospital-grade canonical form must not silently last-win on
// a repeated key.
func Canonicalize(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	v, err := decodeValue(dec)
	if err != nil {
		return nil, fmt.Errorf("canon: decoding payload: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("canon: unexpected trailing data after top-level JSON value")
	}
	var buf bytes.Buffer
	if err := writeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// decodeValue reads one JSON value from dec via the token stream so that
// duplicate object keys can be detected (encoding/json's Decode into any would
// silently keep the last). Numbers arrive as json.Number (dec.UseNumber).
func decodeValue(dec *json.Decoder) (any, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		// Scalar: string, bool, nil, or json.Number.
		return tok, nil
	}
	switch delim {
	case '{':
		m := make(map[string]any)
		for dec.More() {
			keyTok, err := dec.Token()
			if err != nil {
				return nil, err
			}
			key, ok := keyTok.(string)
			if !ok {
				return nil, fmt.Errorf("canon: non-string object key %v", keyTok)
			}
			if _, dup := m[key]; dup {
				return nil, fmt.Errorf("canon: duplicate object key %q", key)
			}
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			m[key] = val
		}
		if _, err := dec.Token(); err != nil { // consume '}'
			return nil, err
		}
		return m, nil
	case '[':
		arr := []any{}
		for dec.More() {
			val, err := decodeValue(dec)
			if err != nil {
				return nil, err
			}
			arr = append(arr, val)
		}
		if _, err := dec.Token(); err != nil { // consume ']'
			return nil, err
		}
		return arr, nil
	default:
		return nil, fmt.Errorf("canon: unexpected delimiter %q", delim)
	}
}

// Hash returns the SHA-256 of payload. Callers pass bytes already produced by
// Canonicalize; Hash never re-encodes. This is the payload_hash primitive
// (D-SEC-1) and the state_hash primitive for snapshots.
func Hash(payload []byte) [32]byte {
	return sha256.Sum256(payload)
}

func writeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		if t {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
	case json.Number:
		s, err := canonicalNumber(t)
		if err != nil {
			return err
		}
		buf.WriteString(s)
	case string:
		writeString(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeString(buf, k)
			buf.WriteByte(':')
			if err := writeValue(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("canon: unsupported JSON value type %T", v)
	}
	return nil
}

// canonicalNumber normalizes a JSON number literal. Integer literals are kept
// verbatim (lossless, no precision risk) with -0 folded to 0; fractional or
// exponential literals are reformatted to the shortest round-trippable float64
// representation, folding every zero form (-0.0, -0e5, 0e0) to "0" so the
// encoder stays idempotent (FormatFloat(-0.0) would emit "-0"). Non-finite
// values are rejected.
//
// Number format = Go strconv 'g' (shortest round-trippable float64), NOT
// RFC-8785; this differs from strict JCS and is pending DDL-freeze confirmation
// (01-architecture §7 S7). Do not change the format here without that decision.
func canonicalNumber(n json.Number) (string, error) {
	s := n.String()
	if !strings.ContainsAny(s, ".eE") {
		if s == "-0" {
			return "0", nil
		}
		return s, nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return "", fmt.Errorf("canon: parsing number %q: %w", s, err)
	}
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return "", fmt.Errorf("canon: non-finite number %q forbidden", s)
	}
	if f == 0 { // folds -0.0, -0e5, 0e0, etc. — FormatFloat(-0.0) emits "-0"
		return "0", nil
	}
	return strconv.FormatFloat(f, 'g', -1, 64), nil
}

// writeString emits a JSON string with minimal, deterministic escaping: only
// the characters JSON requires (control characters, quote, backslash) are
// escaped, and control characters use the lowercase \u00XX form. HTML-significant
// bytes are NOT escaped (R-CANON does not HTML-escape, unlike encoding/json).
func writeString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); {
		c := s[i]
		if c < 0x80 {
			switch c {
			case '"':
				buf.WriteString(`\"`)
			case '\\':
				buf.WriteString(`\\`)
			case '\n':
				buf.WriteString(`\n`)
			case '\r':
				buf.WriteString(`\r`)
			case '\t':
				buf.WriteString(`\t`)
			case '\b':
				buf.WriteString(`\b`)
			case '\f':
				buf.WriteString(`\f`)
			default:
				if c < 0x20 {
					fmt.Fprintf(buf, `\u%04x`, c)
				} else {
					buf.WriteByte(c)
				}
			}
			i++
			continue
		}
		// Multi-byte UTF-8: emit verbatim when valid, replacement when not.
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			buf.WriteString("�")
			i++
			continue
		}
		buf.WriteString(s[i : i+size])
		i += size
	}
	buf.WriteByte('"')
}
