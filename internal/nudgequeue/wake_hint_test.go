package nudgequeue

import (
	"bytes"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestSessionWakeHintV1GoldenRoundTrip(t *testing.T) {
	want := SessionWakeHint{
		Version:   SessionWakeHintVersion1,
		CommandID: "command/with punctuation",
		SessionID: "session:durable:123",
	}
	wire, err := EncodeSessionWakeHint(want)
	if err != nil {
		t.Fatalf("EncodeSessionWakeHint: %v", err)
	}
	const wantWire = "GCNW/1/Y29tbWFuZC93aXRoIHB1bmN0dWF0aW9u/c2Vzc2lvbjpkdXJhYmxlOjEyMw"
	if string(wire) != wantWire {
		t.Fatalf("wire = %q, want golden %q", wire, wantWire)
	}
	got, ok := DecodeSessionWakeHint(wire)
	if !ok {
		t.Fatalf("DecodeSessionWakeHint(%q) rejected golden frame", wire)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestSessionWakeHintRejectsIneligibleIDs(t *testing.T) {
	invalidUTF8 := string([]byte{0xff})
	if utf8.ValidString(invalidUTF8) {
		t.Fatal("test fixture unexpectedly valid UTF-8")
	}
	tests := []struct {
		name string
		hint SessionWakeHint
	}{
		{name: "unknown version", hint: SessionWakeHint{Version: 2, CommandID: "command", SessionID: "session"}},
		{name: "empty command", hint: SessionWakeHint{Version: 1, SessionID: "session"}},
		{name: "empty session", hint: SessionWakeHint{Version: 1, CommandID: "command"}},
		{name: "padded command", hint: SessionWakeHint{Version: 1, CommandID: " command", SessionID: "session"}},
		{name: "padded session", hint: SessionWakeHint{Version: 1, CommandID: "command", SessionID: "session\n"}},
		{name: "invalid command utf8", hint: SessionWakeHint{Version: 1, CommandID: invalidUTF8, SessionID: "session"}},
		{name: "invalid session utf8", hint: SessionWakeHint{Version: 1, CommandID: "command", SessionID: invalidUTF8}},
		{name: "oversized command", hint: SessionWakeHint{Version: 1, CommandID: strings.Repeat("c", MaxSessionWakeHintIDBytes+1), SessionID: "session"}},
		{name: "oversized session", hint: SessionWakeHint{Version: 1, CommandID: "command", SessionID: strings.Repeat("s", MaxSessionWakeHintIDBytes+1)}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if wire, err := EncodeSessionWakeHint(tc.hint); err == nil {
				t.Fatalf("EncodeSessionWakeHint(%+v) = %q, nil error", tc.hint, wire)
			}
		})
	}
}

func TestSessionWakeHintRejectsMalformedFrames(t *testing.T) {
	tests := []struct {
		name string
		wire []byte
	}{
		{name: "empty", wire: nil},
		{name: "legacy", wire: []byte{1}},
		{name: "unknown version", wire: []byte("GCNW/2/Y29tbWFuZA/c2Vzc2lvbg")},
		{name: "missing command", wire: []byte("GCNW/1//c2Vzc2lvbg")},
		{name: "missing session", wire: []byte("GCNW/1/Y29tbWFuZA/")},
		{name: "extra component", wire: []byte("GCNW/1/Y29tbWFuZA/c2Vzc2lvbg/ZXh0cmE")},
		{name: "padded base64", wire: []byte("GCNW/1/Y29tbWFuZA==/c2Vzc2lvbg")},
		{name: "invalid base64", wire: []byte("GCNW/1/%%%/c2Vzc2lvbg")},
		{name: "decoded padded identity", wire: []byte("GCNW/1/IGNvbW1hbmQ/c2Vzc2lvbg")},
		{name: "oversized wire", wire: bytes.Repeat([]byte("x"), MaxSessionWakeHintWireBytes+1)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if hint, ok := DecodeSessionWakeHint(tc.wire); ok {
				t.Fatalf("DecodeSessionWakeHint(%q) = %+v, true", tc.wire, hint)
			}
		})
	}
}

func TestSessionWakeHintWireContainsOnlyItsTwoIdentifiers(t *testing.T) {
	hint := SessionWakeHint{Version: 1, CommandID: "unique-command", SessionID: "unique-session"}
	wire, err := EncodeSessionWakeHint(hint)
	if err != nil {
		t.Fatalf("EncodeSessionWakeHint: %v", err)
	}
	for _, forbidden := range []string{
		"unique-agent-alias",
		"unique-message-secret",
		"unique-city-path",
		"unique-store-id",
	} {
		if bytes.Contains(wire, []byte(forbidden)) {
			t.Fatalf("wire %q leaked forbidden field %q", wire, forbidden)
		}
	}
}

func FuzzDecodeSessionWakeHint(f *testing.F) {
	f.Add([]byte("GCNW/1/Y29tbWFuZA/c2Vzc2lvbg"))
	f.Add([]byte{1})
	f.Add([]byte("GCNW/999/not/base64"))
	f.Fuzz(func(t *testing.T, wire []byte) {
		hint, ok := DecodeSessionWakeHint(wire)
		if !ok {
			return
		}
		reencoded, err := EncodeSessionWakeHint(hint)
		if err != nil {
			t.Fatalf("decoded hint cannot be re-encoded: %+v: %v", hint, err)
		}
		if !bytes.Equal(reencoded, wire) {
			t.Fatalf("accepted non-canonical wire %q; canonical form %q", wire, reencoded)
		}
	})
}
