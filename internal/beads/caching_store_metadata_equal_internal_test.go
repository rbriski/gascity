package beads

import "testing"

// TestBeadChangedIgnoresMetadataJSONKeyOrder pins the metadata-value
// re-serialize false-positive fix: a metadata value that is a JSON object
// differing only in key order is NOT a change. The Dolt rig-store scan and the
// cache can serialize the same JSON blob with different key order, so an exact
// maps.Equal compare reported a spurious change every reconcile pass and drove a
// re-absorb flood.
func TestBeadChangedIgnoresMetadataJSONKeyOrder(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"a":1,"b":2}`}}
	fresh := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"b":2,"a":1}`}}

	if beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged reported a change for a metadata JSON value that only differs by key order")
	}
}

// TestBeadChangedDetectsLargeIntegerMetadataChange pins the >2^53 precision fix:
// two metadata JSON integers that differ only past the float53 boundary
// (9007199254740992 vs 9007199254740993) MUST be seen as a change. Canonicalizing
// via a float round-trip collapsed both to the same float and silently dropped
// the write; the json.Number path keeps them distinct.
func TestBeadChangedDetectsLargeIntegerMetadataChange(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Metadata: map[string]string{"count": `{"n":9007199254740992}`}}
	fresh := Bead{ID: "gc-1", Metadata: map[string]string{"count": `{"n":9007199254740993}`}}

	if !beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged missed a >2^53 metadata integer change (float round-trip conflated the two values)")
	}
}

// TestBeadChangedIgnoresLargeIntegerMetadataKeyOrder verifies the canonical
// compare still collapses key-order/whitespace artifacts while preserving a
// >2^53 integer exactly: same big-int value, reordered keys, is NOT a change.
func TestBeadChangedIgnoresLargeIntegerMetadataKeyOrder(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"a":9007199254740993,"b":2}`}}
	fresh := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"b":2,"a":9007199254740993}`}}

	if beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged reported a change for a big-int metadata value that only differs by key order")
	}
}

// TestBeadChangedDetectsMetadataJSONValueChange verifies a genuine value change
// inside a JSON-blob metadata value is still detected.
func TestBeadChangedDetectsMetadataJSONValueChange(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"a":1,"b":2}`}}
	fresh := Bead{ID: "gc-1", Metadata: map[string]string{"payload": `{"a":1,"b":3}`}}

	if !beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged missed a genuine metadata JSON value change")
	}
}

// TestBeadChangedNonJSONMetadata verifies non-JSON metadata values compare
// exactly: an unchanged value is not a change, a changed value is.
func TestBeadChangedNonJSONMetadata(t *testing.T) {
	t.Parallel()

	if beadChanged(
		Bead{ID: "gc-1", Metadata: map[string]string{"note": "hello"}},
		Bead{ID: "gc-1", Metadata: map[string]string{"note": "hello"}},
		false,
	) {
		t.Fatalf("beadChanged reported a change for identical non-JSON metadata")
	}

	if !beadChanged(
		Bead{ID: "gc-1", Metadata: map[string]string{"note": "hello"}},
		Bead{ID: "gc-1", Metadata: map[string]string{"note": "world"}},
		false,
	) {
		t.Fatalf("beadChanged missed a genuine non-JSON metadata change")
	}
}

// TestBeadChangedDetectsMetadataKeySetChange verifies a differing metadata key
// set is a change even when the overlapping values match.
func TestBeadChangedDetectsMetadataKeySetChange(t *testing.T) {
	t.Parallel()

	old := Bead{ID: "gc-1", Metadata: map[string]string{"a": "1"}}
	fresh := Bead{ID: "gc-1", Metadata: map[string]string{"b": "1"}}

	if !beadChanged(old, fresh, false) {
		t.Fatalf("beadChanged missed a metadata key-set change")
	}
}
