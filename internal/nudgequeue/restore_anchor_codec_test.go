package nudgequeue

import (
	"bytes"
	"strings"
	"testing"
)

func TestRestoreAnchorCodecRoundTripAndGolden(t *testing.T) {
	anchor := RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-01HZY8Q9", RestoreEpoch: 7},
		HighestAcceptedRevision: 42,
		HighestAcceptedSequence: 40,
	}
	wire, err := EncodeRestoreAnchor(anchor)
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor: %v", err)
	}
	const want = "{\"version\":1,\"store\":{\"store_uuid\":\"store-01HZY8Q9\",\"restore_epoch\":7},\"highest_accepted_revision\":42,\"highest_accepted_sequence\":40,\"checksum_sha256\":\"d8caba70f2e2d8b2971001a71b6a70c198f0392b71202ca2dee4e8d355cfd093\"}\n"
	if string(wire) != want {
		t.Fatalf("wire = %q, want golden %q", wire, want)
	}
	decoded, err := DecodeRestoreAnchor(wire)
	if err != nil {
		t.Fatalf("DecodeRestoreAnchor: %v", err)
	}
	if decoded != anchor {
		t.Fatalf("decoded = %#v, want %#v", decoded, anchor)
	}
}

func TestDecodeRestoreAnchorRejectsUntrustedWireStrictly(t *testing.T) {
	valid, err := EncodeRestoreAnchor(RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 2},
		HighestAcceptedRevision: 9,
		HighestAcceptedSequence: 5,
	})
	if err != nil {
		t.Fatalf("EncodeRestoreAnchor fixture: %v", err)
	}
	validObject := strings.TrimSuffix(string(valid), "\n")
	checksum := restoreAnchorChecksumForTest(t, valid)
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "empty", wire: nil},
		{name: "malformed", wire: []byte("{")},
		{name: "trailing value", wire: append(append([]byte(nil), valid...), []byte("{}")...)},
		{name: "unknown field", wire: []byte(strings.TrimSuffix(validObject, "}") + `,"extra":true}`)},
		{name: "case variant field", wire: []byte(strings.Replace(validObject, `"version"`, `"Version"`, 1))},
		{name: "duplicate field", wire: []byte(strings.Replace(validObject, `"version":1`, `"version":1,"version":1`, 1))},
		{name: "duplicate nested field", wire: []byte(strings.Replace(validObject, `"restore_epoch":2`, `"restore_epoch":2,"restore_epoch":2`, 1))},
		{name: "unsupported version", wire: []byte(strings.Replace(validObject, `"version":1`, `"version":2`, 1))},
		{name: "missing store uuid", wire: []byte(strings.Replace(validObject, `"store_uuid":"store-a"`, `"store_uuid":""`, 1))},
		{name: "zero restore epoch", wire: []byte(strings.Replace(validObject, `"restore_epoch":2`, `"restore_epoch":0`, 1))},
		{name: "checksum changed", wire: []byte(strings.Replace(validObject, `"highest_accepted_revision":9`, `"highest_accepted_revision":8`, 1))},
		{name: "sequence exceeds revision", wire: []byte(strings.Replace(validObject, `"highest_accepted_sequence":5`, `"highest_accepted_sequence":10`, 1))},
		{name: "checksum uppercase", wire: []byte(strings.Replace(validObject, checksum, strings.ToUpper(checksum), 1))},
		{name: "checksum short", wire: []byte(strings.Replace(validObject, checksum, "00", 1))},
		{name: "oversized", wire: bytes.Repeat([]byte("x"), MaxRestoreAnchorBytes+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if decoded, err := DecodeRestoreAnchor(tc.wire); err == nil {
				t.Fatalf("DecodeRestoreAnchor accepted %q as %#v", tc.wire, decoded)
			}
		})
	}
}

func TestEncodeRestoreAnchorRejectsInvalidKnownVersionRecords(t *testing.T) {
	tests := []RestoreAnchor{
		{},
		{Version: 2, Store: CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1}},
		{Version: RestoreAnchorVersion1, Store: CommandStoreBinding{RestoreEpoch: 1}},
		{Version: RestoreAnchorVersion1, Store: CommandStoreBinding{StoreUUID: "store-a"}},
		{Version: RestoreAnchorVersion1, Store: CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1}, HighestAcceptedRevision: 1, HighestAcceptedSequence: 2},
	}
	for i, anchor := range tests {
		if wire, err := EncodeRestoreAnchor(anchor); err == nil {
			t.Errorf("case %d EncodeRestoreAnchor(%#v) = %q, nil error", i, anchor, wire)
		}
	}
}

func FuzzDecodeRestoreAnchor(f *testing.F) {
	valid, err := EncodeRestoreAnchor(RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   CommandStoreBinding{StoreUUID: "store-a", RestoreEpoch: 1},
		HighestAcceptedRevision: 3,
		HighestAcceptedSequence: 2,
	})
	if err != nil {
		f.Fatalf("EncodeRestoreAnchor seed: %v", err)
	}
	f.Add(valid)
	f.Add([]byte("{}"))
	f.Add(bytes.Repeat([]byte("x"), MaxRestoreAnchorBytes+1))
	f.Add([]byte(`{"version":1,"version":1}`))
	f.Fuzz(func(t *testing.T, wire []byte) {
		anchor, err := DecodeRestoreAnchor(wire)
		if err != nil {
			return
		}
		reencoded, err := EncodeRestoreAnchor(anchor)
		if err != nil {
			t.Fatalf("accepted anchor cannot be encoded: %#v: %v", anchor, err)
		}
		decodedAgain, err := DecodeRestoreAnchor(reencoded)
		if err != nil || decodedAgain != anchor {
			t.Fatalf("canonical round trip = (%#v, %v), want %#v", decodedAgain, err, anchor)
		}
	})
}

func restoreAnchorChecksumForTest(t *testing.T, wire []byte) string {
	t.Helper()
	const marker = `"checksum_sha256":"`
	start := bytes.Index(wire, []byte(marker))
	if start < 0 {
		t.Fatalf("fixture has no checksum: %q", wire)
	}
	start += len(marker)
	end := bytes.IndexByte(wire[start:], '"')
	if end < 0 {
		t.Fatalf("fixture has unterminated checksum: %q", wire)
	}
	return string(wire[start : start+end])
}
