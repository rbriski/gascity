package extras_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads/extras"
)

// testKnown stands in for an adapter's enumerated-but-not-column fields. The
// extras package never sees this type directly; the adapter owns the
// struct<->map conversion. These helpers model that contract in the test.
type testKnown struct {
	Alias string `json:"alias"`
	State string `json:"state"`
}

func (k testKnown) keys() []string { return []string{"alias", "state"} }

func (k testKnown) toMap() map[string]string {
	m := map[string]string{}
	if k.Alias != "" {
		m["alias"] = k.Alias
	}
	if k.State != "" {
		m["state"] = k.State
	}
	return m
}

func TestLeftoverExcludesClaimedKeysAndDoesNotMutateInput(t *testing.T) {
	meta := map[string]string{"a": "1", "b": "2", "c": "3", "d": "4"}
	got := extras.Leftover(meta, "a", "c")
	want := map[string]string{"b": "2", "d": "4"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Leftover = %v, want %v", got, want)
	}
	// Mutating the result must not touch the input.
	got["b"] = "mutated"
	if meta["b"] != "2" {
		t.Fatalf("Leftover mutated its input: meta[b]=%q", meta["b"])
	}
	if len(meta) != 4 {
		t.Fatalf("input map length changed: %d", len(meta))
	}
}

func TestLeftoverNilMetaReturnsEmptyNonNil(t *testing.T) {
	got := extras.Leftover(nil, "a")
	if got == nil {
		t.Fatal("Leftover(nil) returned nil; want empty non-nil map")
	}
	if len(got) != 0 {
		t.Fatalf("Leftover(nil) = %v, want empty", got)
	}
}

func TestUnionMergesDisjointParts(t *testing.T) {
	got, err := extras.Union(
		map[string]string{"a": "1"},
		map[string]string{"b": "2"},
		map[string]string{"c": "3"},
	)
	if err != nil {
		t.Fatalf("Union error: %v", err)
	}
	want := map[string]string{"a": "1", "b": "2", "c": "3"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Union = %v, want %v", got, want)
	}
}

func TestUnionToleratesNilAndEmptyParts(t *testing.T) {
	got, err := extras.Union(nil, map[string]string{}, map[string]string{"a": "1"})
	if err != nil {
		t.Fatalf("Union error: %v", err)
	}
	if !reflect.DeepEqual(got, map[string]string{"a": "1"}) {
		t.Fatalf("Union = %v, want {a:1}", got)
	}
}

func TestUnionErrorsOnDuplicateKeyEvenWhenValuesMatch(t *testing.T) {
	// The double-write/drift guard: a key in two parts is an error regardless of
	// value, so a field that drifts into both `known` and `unknown` is caught.
	for _, tc := range []struct {
		name string
		a, b map[string]string
	}{
		{"different values", map[string]string{"k": "1"}, map[string]string{"k": "2"}},
		{"same value", map[string]string{"k": "1"}, map[string]string{"k": "1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := extras.Union(tc.a, tc.b)
			if err == nil {
				t.Fatal("Union: expected duplicate-key error, got nil")
			}
			if !strings.Contains(err.Error(), `"k"`) {
				t.Fatalf("error should name the offending key: %v", err)
			}
		})
	}
}

func TestEncodeDefaultsVersionWhenZero(t *testing.T) {
	data, err := extras.Encode(extras.Envelope[testKnown]{
		Known:   testKnown{Alias: "x"},
		Unknown: map[string]string{"u": "1"},
	})
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	if !strings.Contains(string(data), `"v":`) {
		t.Fatalf("encoded blob missing version field: %s", data)
	}
	dec, err := extras.Decode[testKnown](data)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if dec.V != extras.Version {
		t.Fatalf("decoded V = %d, want %d", dec.V, extras.Version)
	}
}

func TestEncodeDecodeRoundTripPreservesKnownAndUnknown(t *testing.T) {
	env := extras.Envelope[testKnown]{
		V:       extras.Version,
		Known:   testKnown{Alias: "alias-1", State: "asleep"},
		Unknown: map[string]string{"continuation_epoch": "7", "weird_key": "v"},
	}
	data, err := extras.Encode(env)
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}
	got, err := extras.Decode[testKnown](data)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	if !reflect.DeepEqual(got, env) {
		t.Fatalf("round-trip mismatch:\n got  %+v\n want %+v", got, env)
	}
}

func TestDecodeRejectsMalformedJSON(t *testing.T) {
	if _, err := extras.Decode[testKnown]([]byte("{not json")); err == nil {
		t.Fatal("Decode: expected error on malformed JSON, got nil")
	}
}

// TestFullRoundTripPreservesEveryMetadataKey is the loss-detector: it models the
// entire adapter pattern — split a bead's metadata into typed `known` fields and
// an `unknown` passthrough, persist via the envelope, then reconstruct — and
// asserts the reconstructed metadata equals the original key-for-key. This is the
// single most important guarantee under conformance-only validation: no metadata
// key is ever dropped by the bd<->domain<->row translation.
func TestFullRoundTripPreservesEveryMetadataKey(t *testing.T) {
	original := map[string]string{
		// modeled as typed `known` fields:
		"alias": "sess-alias",
		"state": "active",
		// everything this binary did not enumerate — must survive verbatim:
		"continuation_epoch": "12",
		"instance_token":     "tok-abc",
		"future_unknown_key": "still here",
		"mcp_snapshot":       `{"a":1}`,
	}

	// Adapter side: pull known fields, compute the unknown passthrough.
	known := testKnown{Alias: original["alias"], State: original["state"]}
	unknown := extras.Leftover(original, known.keys()...)
	blob, err := extras.Encode(extras.Envelope[testKnown]{Known: known, Unknown: unknown})
	if err != nil {
		t.Fatalf("Encode error: %v", err)
	}

	// ... later, on read: decode and reconstruct the full metadata map.
	env, err := extras.Decode[testKnown](blob)
	if err != nil {
		t.Fatalf("Decode error: %v", err)
	}
	reconstructed, err := extras.Union(env.Known.toMap(), env.Unknown)
	if err != nil {
		t.Fatalf("Union error: %v", err)
	}

	if !reflect.DeepEqual(reconstructed, original) {
		t.Fatalf("lossy round-trip:\n got  %v\n want %v", reconstructed, original)
	}
}

// TestReconstructUnionRejectsKnownUnknownOverlap proves the drift guard fires if
// a key is modeled as `known` AND also leaks into `unknown` (a codec bug).
func TestReconstructUnionRejectsKnownUnknownOverlap(t *testing.T) {
	known := testKnown{Alias: "a", State: "s"}
	// Buggy unknown that still carries a promoted key.
	unknown := map[string]string{"alias": "a", "other": "x"}
	if _, err := extras.Union(known.toMap(), unknown); err == nil {
		t.Fatal("expected overlap error when a known key also appears in unknown")
	}
}
