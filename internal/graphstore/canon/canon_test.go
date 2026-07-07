package canon

import (
	"bytes"
	"testing"
)

func TestCanonicalizeSortsKeysAndStrips(t *testing.T) {
	a, err := Canonicalize([]byte(`{ "b":1, "a":2,  "c": [3, 4 ] }`))
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	b, err := Canonicalize([]byte(`{"c":[3,4],"a":2,"b":1}`))
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	if !bytes.Equal(a, b) {
		t.Fatalf("key order changed output: %q vs %q", a, b)
	}
	if want := `{"a":2,"b":1,"c":[3,4]}`; string(a) != want {
		t.Fatalf("canonical form = %q, want %q", a, want)
	}
}

func TestCanonicalizeIdempotent(t *testing.T) {
	once, err := Canonicalize([]byte(`{"z":{"y":true,"x":null},"a":"hi"}`))
	if err != nil {
		t.Fatalf("first: %v", err)
	}
	twice, err := Canonicalize(once)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if !bytes.Equal(once, twice) {
		t.Fatalf("not idempotent: %q vs %q", once, twice)
	}
}

func TestCanonicalizeRejectsGarbageAndNonFinite(t *testing.T) {
	if _, err := Canonicalize([]byte(`{"a":1} trailing`)); err == nil {
		t.Fatal("expected error on trailing data")
	}
	if _, err := Canonicalize([]byte(`{"a":1e400}`)); err == nil {
		t.Fatal("expected error on non-finite number")
	}
}

func TestCanonicalizeNumberNormalization(t *testing.T) {
	got, err := Canonicalize([]byte(`{"i":-0,"f":1.50,"e":1e2}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if want := `{"e":100,"f":1.5,"i":0}`; string(got) != want {
		t.Fatalf("number normalization = %q, want %q", got, want)
	}
}

// TestCanonicalizeNegativeZeroFolds pins B2: every zero form (-0, -0.0, -0e5,
// 0e0) canonicalizes to "0", so payload_hash is stable and the encoder is
// idempotent. FormatFloat(-0.0) would otherwise emit "-0", breaking replay.
func TestCanonicalizeNegativeZeroFolds(t *testing.T) {
	zero, err := Canonicalize([]byte(`{"a":0}`))
	if err != nil {
		t.Fatalf("canonicalize {a:0}: %v", err)
	}
	for _, in := range []string{`{"a":-0.0}`, `{"a":-0}`, `{"a":-0e5}`, `{"a":0e0}`, `{"a":0.0}`} {
		got, err := Canonicalize([]byte(in))
		if err != nil {
			t.Fatalf("canonicalize %s: %v", in, err)
		}
		if !bytes.Equal(got, zero) {
			t.Fatalf("%s canonicalized to %q, want %q", in, got, zero)
		}
	}
	if want := `{"a":0}`; string(zero) != want {
		t.Fatalf("zero canonical form = %q, want %q", zero, want)
	}
}

// TestCanonicalizeIdempotentNumbers proves Canonicalize(Canonicalize(x)) ==
// Canonicalize(x) across the zero and exponent forms that previously round-tripped
// to a different literal.
func TestCanonicalizeIdempotentNumbers(t *testing.T) {
	for _, in := range []string{`{"a":-0.0}`, `{"a":-0}`, `{"a":0e0}`, `{"n":1.50}`, `{"n":1e2}`} {
		once, err := Canonicalize([]byte(in))
		if err != nil {
			t.Fatalf("first %s: %v", in, err)
		}
		twice, err := Canonicalize(once)
		if err != nil {
			t.Fatalf("second %s: %v", in, err)
		}
		if !bytes.Equal(once, twice) {
			t.Fatalf("%s not idempotent: %q vs %q", in, once, twice)
		}
	}
}

// TestCanonicalizeRejectsDuplicateKeys pins S5: a canonical form must not
// silently last-win on a repeated object key.
func TestCanonicalizeRejectsDuplicateKeys(t *testing.T) {
	if _, err := Canonicalize([]byte(`{"a":1,"a":2}`)); err == nil {
		t.Fatal("expected error on duplicate object key")
	}
	// A duplicate nested inside an object must also be caught.
	if _, err := Canonicalize([]byte(`{"o":{"b":1,"b":2}}`)); err == nil {
		t.Fatal("expected error on nested duplicate object key")
	}
	// Distinct keys and repeated keys across sibling objects stay valid.
	if _, err := Canonicalize([]byte(`{"a":{"x":1},"b":{"x":2}}`)); err != nil {
		t.Fatalf("distinct keys rejected: %v", err)
	}
}

// TestHashOfCanonicalIsOrderAndWhitespaceInvariant proves the payload_hash
// primitive is stable across the insignificant syntactic differences R-CANON is
// meant to fold away: two payloads that differ only in object key order and
// insignificant whitespace hash to the same value once canonicalized. This is
// the property that makes payload_hash reproducible across producers.
func TestHashOfCanonicalIsOrderAndWhitespaceInvariant(t *testing.T) {
	a, err := Canonicalize([]byte(`{ "b" : 1,  "a": 2, "c":[3, 4] }`))
	if err != nil {
		t.Fatalf("canonicalize a: %v", err)
	}
	b, err := Canonicalize([]byte(`{"c":[3,4],"a":2,"b":1}`))
	if err != nil {
		t.Fatalf("canonicalize b: %v", err)
	}
	if Hash(a) != Hash(b) {
		t.Fatalf("hash differs across key order / whitespace: %x vs %x", Hash(a), Hash(b))
	}
}

// TestHashOfCanonicalIsIdempotent proves canonical hashing reaches a fixed
// point: re-canonicalizing already-canonical bytes does not change the hash, so
// an honest replay of a stored payload always matches (I-11 / R-IDEM).
func TestHashOfCanonicalIsIdempotent(t *testing.T) {
	once, err := Canonicalize([]byte(`{"z":{"y":true,"x":null},"a":"hi","n":1.50}`))
	if err != nil {
		t.Fatalf("first canonicalize: %v", err)
	}
	twice, err := Canonicalize(once)
	if err != nil {
		t.Fatalf("second canonicalize: %v", err)
	}
	if Hash(once) != Hash(twice) {
		t.Fatalf("hash not idempotent under re-canonicalization: %x vs %x", Hash(once), Hash(twice))
	}
}
