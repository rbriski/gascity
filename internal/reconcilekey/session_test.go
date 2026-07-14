package reconcilekey

import (
	"encoding"
	"testing"
)

func TestSessionKeyGoldenRoundTrip(t *testing.T) {
	key, err := NewSession("city:alpha", "gc-123")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	const encoded = "v1/session/Y2l0eTphbHBoYQ/Z2MtMTIz"
	if got := key.String(); got != encoded {
		t.Fatalf("String() = %q, want %q", got, encoded)
	}

	decoded, err := ParseSession(encoded)
	if err != nil {
		t.Fatalf("ParseSession: %v", err)
	}
	if decoded != key {
		t.Fatalf("ParseSession() = %#v, want %#v", decoded, key)
	}
	if got := decoded.StoreID(); got != "city:alpha" {
		t.Fatalf("StoreID() = %q, want city:alpha", got)
	}
	if got := decoded.SessionID(); got != "gc-123" {
		t.Fatalf("SessionID() = %q, want gc-123", got)
	}
}

func TestSessionKeyEncodingDoesNotCollideAcrossStoreOrSessionBoundaries(t *testing.T) {
	tests := []struct {
		storeID   string
		sessionID string
	}{
		{storeID: "ab", sessionID: "c"},
		{storeID: "a", sessionID: "bc"},
		{storeID: "city/a", sessionID: "session:b"},
		{storeID: "city", sessionID: "a/session:b"},
	}

	seen := make(map[string]struct{}, len(tests))
	for _, tc := range tests {
		key, err := NewSession(tc.storeID, tc.sessionID)
		if err != nil {
			t.Fatalf("NewSession(%q, %q): %v", tc.storeID, tc.sessionID, err)
		}
		encoded := key.String()
		if _, ok := seen[encoded]; ok {
			t.Fatalf("duplicate encoding %q for store=%q session=%q", encoded, tc.storeID, tc.sessionID)
		}
		seen[encoded] = struct{}{}
	}
}

func TestSessionKeyChangesAcrossStoreAndSessionReplacement(t *testing.T) {
	original, err := NewSession("store-installation-1", "session-1")
	if err != nil {
		t.Fatalf("NewSession original: %v", err)
	}
	sameSessionOtherStore, err := NewSession("store-installation-2", "session-1")
	if err != nil {
		t.Fatalf("NewSession other store: %v", err)
	}
	replacementSession, err := NewSession("store-installation-1", "session-2")
	if err != nil {
		t.Fatalf("NewSession replacement: %v", err)
	}
	if original == sameSessionOtherStore {
		t.Fatal("same session ID in a different store produced the same key")
	}
	if original == replacementSession {
		t.Fatal("replacement session ID in the same store produced the same key")
	}
}

func TestSessionKeyRejectsNonCanonicalOrIncompleteIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		storeID   string
		sessionID string
	}{
		{name: "missing store", sessionID: "gc-1"},
		{name: "missing session", storeID: "city"},
		{name: "store whitespace", storeID: " city", sessionID: "gc-1"},
		{name: "session whitespace", storeID: "city", sessionID: "gc-1\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := NewSession(tc.storeID, tc.sessionID); err == nil {
				t.Fatal("NewSession() error = nil, want validation error")
			}
		})
	}

	for _, encoded := range []string{
		"",
		"v2/session/Y2l0eQ/Z2MtMQ",
		"v1/pool/Y2l0eQ/Z2MtMQ",
		"v1/session/Y2l0eQ",
		"v1/session/@@@/Z2MtMQ",
		"v1/session/Y2l0eQ/@@@",
		"v1/session/Y2l0eQ/Z2MtMQ/extra",
	} {
		if _, err := ParseSession(encoded); err == nil {
			t.Fatalf("ParseSession(%q) error = nil, want validation error", encoded)
		}
	}
}

func TestSessionKeyImplementsTextEncoding(t *testing.T) {
	var _ encoding.TextMarshaler = Session{}
	var _ encoding.TextUnmarshaler = (*Session)(nil)

	want, err := NewSession("city", "session")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	text, err := want.MarshalText()
	if err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
	var got Session
	if err := got.UnmarshalText(text); err != nil {
		t.Fatalf("UnmarshalText: %v", err)
	}
	if got != want {
		t.Fatalf("text round trip = %#v, want %#v", got, want)
	}
}
