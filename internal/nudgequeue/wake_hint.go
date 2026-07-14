package nudgequeue

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"
)

const sessionWakeHintV1Prefix = "GCNW/1/"

const (
	// SessionWakeHintVersion1 is the first exact-target nudge wake protocol.
	SessionWakeHintVersion1 uint8 = 1
	// MaxSessionWakeHintIDBytes bounds each decoded identifier carried by a
	// best-effort wake. Durable queue state, not this frame, remains authority.
	MaxSessionWakeHintIDBytes = 256
	// MaxSessionWakeHintWireBytes bounds one connection-framed wake payload.
	MaxSessionWakeHintWireBytes = 1024
)

// SessionWakeHint identifies the durable command and session whose committed
// queue state changed. It deliberately excludes aliases, paths, store scope,
// requester identity, and message content. The city listener supplies trusted
// store scope locally; the hint itself grants no command authority.
type SessionWakeHint struct {
	Version   uint8
	CommandID string
	SessionID string
}

// EncodeSessionWakeHint returns the canonical bounded wire representation of
// a wake hint. Ineligible hints must fall back to the legacy global wake rather
// than making a successfully committed command fail.
func EncodeSessionWakeHint(hint SessionWakeHint) ([]byte, error) {
	if hint.Version != SessionWakeHintVersion1 {
		return nil, fmt.Errorf("encoding nudge wake hint: unsupported version %d", hint.Version)
	}
	if err := validateSessionWakeHintID("command", hint.CommandID); err != nil {
		return nil, err
	}
	if err := validateSessionWakeHintID("session", hint.SessionID); err != nil {
		return nil, err
	}
	command := base64.RawURLEncoding.EncodeToString([]byte(hint.CommandID))
	session := base64.RawURLEncoding.EncodeToString([]byte(hint.SessionID))
	wire := []byte(sessionWakeHintV1Prefix + command + "/" + session)
	if len(wire) > MaxSessionWakeHintWireBytes {
		return nil, fmt.Errorf("encoding nudge wake hint: wire payload exceeds %d bytes", MaxSessionWakeHintWireBytes)
	}
	return wire, nil
}

// DecodeSessionWakeHint parses one canonical connection-framed wake. False is
// returned for legacy, malformed, future-version, or oversized input so the
// listener can retain the global wake without log-spamming untrusted bytes.
func DecodeSessionWakeHint(wire []byte) (SessionWakeHint, bool) {
	if len(wire) == 0 || len(wire) > MaxSessionWakeHintWireBytes {
		return SessionWakeHint{}, false
	}
	remainder, ok := strings.CutPrefix(string(wire), sessionWakeHintV1Prefix)
	if !ok {
		return SessionWakeHint{}, false
	}
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return SessionWakeHint{}, false
	}
	commandID, ok := decodeSessionWakeHintID(parts[0])
	if !ok {
		return SessionWakeHint{}, false
	}
	sessionID, ok := decodeSessionWakeHintID(parts[1])
	if !ok {
		return SessionWakeHint{}, false
	}
	hint := SessionWakeHint{
		Version:   SessionWakeHintVersion1,
		CommandID: commandID,
		SessionID: sessionID,
	}
	canonical, err := EncodeSessionWakeHint(hint)
	if err != nil || string(canonical) != string(wire) {
		return SessionWakeHint{}, false
	}
	return hint, true
}

func validateSessionWakeHintID(kind, id string) error {
	if id == "" {
		return fmt.Errorf("encoding nudge wake hint: %s identity is empty", kind)
	}
	if len(id) > MaxSessionWakeHintIDBytes {
		return fmt.Errorf("encoding nudge wake hint: %s identity exceeds %d bytes", kind, MaxSessionWakeHintIDBytes)
	}
	if !utf8.ValidString(id) {
		return fmt.Errorf("encoding nudge wake hint: %s identity is not valid UTF-8", kind)
	}
	if strings.TrimSpace(id) != id {
		return fmt.Errorf("encoding nudge wake hint: %s identity is not canonical", kind)
	}
	return nil
}

func decodeSessionWakeHintID(encoded string) (string, bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil || base64.RawURLEncoding.EncodeToString(decoded) != encoded {
		return "", false
	}
	id := string(decoded)
	if validateSessionWakeHintID("decoded", id) != nil {
		return "", false
	}
	return id, true
}
