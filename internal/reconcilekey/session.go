// Package reconcilekey defines stable, versioned controller keys without
// depending on queues, stores, or runtime providers.
package reconcilekey

import (
	"encoding/base64"
	"fmt"
	"strings"
)

const sessionKeyPrefix = "v1/session/"

// Session identifies one canonical session within one immutable store scope.
// It is comparable so it can be used directly as a typed workqueue key.
type Session struct {
	storeID   string
	sessionID string
}

// NewSession constructs a stable controller key. Inputs must already be in
// canonical form; the constructor rejects empty or whitespace-padded values
// instead of silently changing identity.
func NewSession(storeID, sessionID string) (Session, error) {
	if err := validateIdentityPart("store", storeID); err != nil {
		return Session{}, err
	}
	if err := validateIdentityPart("session", sessionID); err != nil {
		return Session{}, err
	}
	return Session{storeID: storeID, sessionID: sessionID}, nil
}

// ParseSession decodes a versioned Session string.
func ParseSession(encoded string) (Session, error) {
	remainder, ok := strings.CutPrefix(encoded, sessionKeyPrefix)
	if !ok {
		return Session{}, fmt.Errorf("parsing session reconcile key %q: unsupported kind or version", encoded)
	}
	parts := strings.Split(remainder, "/")
	if len(parts) != 2 {
		return Session{}, fmt.Errorf("parsing session reconcile key %q: want store and session components", encoded)
	}
	storeID, err := decodeIdentityPart("store", parts[0])
	if err != nil {
		return Session{}, fmt.Errorf("parsing session reconcile key %q: %w", encoded, err)
	}
	sessionID, err := decodeIdentityPart("session", parts[1])
	if err != nil {
		return Session{}, fmt.Errorf("parsing session reconcile key %q: %w", encoded, err)
	}
	return NewSession(storeID, sessionID)
}

// StoreID returns the immutable store scope that owns the session.
func (k Session) StoreID() string { return k.storeID }

// SessionID returns the canonical durable session identifier.
func (k Session) SessionID() string { return k.sessionID }

// IsZero reports whether the key has not been constructed.
func (k Session) IsZero() bool { return k.storeID == "" || k.sessionID == "" }

// String returns the canonical versioned key encoding.
func (k Session) String() string {
	if k.IsZero() {
		return ""
	}
	return sessionKeyPrefix + encodeIdentityPart(k.storeID) + "/" + encodeIdentityPart(k.sessionID)
}

// MarshalText implements encoding.TextMarshaler.
func (k Session) MarshalText() ([]byte, error) {
	if k.IsZero() {
		return nil, fmt.Errorf("marshaling session reconcile key: zero key")
	}
	return []byte(k.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler.
func (k *Session) UnmarshalText(text []byte) error {
	if k == nil {
		return fmt.Errorf("unmarshaling session reconcile key: nil destination")
	}
	parsed, err := ParseSession(string(text))
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}

func validateIdentityPart(kind, value string) error {
	if value == "" {
		return fmt.Errorf("constructing session reconcile key: %s identity is empty", kind)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("constructing session reconcile key: %s identity is not canonical", kind)
	}
	return nil
}

func encodeIdentityPart(value string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(value))
}

func decodeIdentityPart(kind, encoded string) (string, error) {
	if encoded == "" {
		return "", fmt.Errorf("%s identity is empty", kind)
	}
	decoded, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("decoding %s identity: %w", kind, err)
	}
	if encodeIdentityPart(string(decoded)) != encoded {
		return "", fmt.Errorf("decoding %s identity: non-canonical base64", kind)
	}
	return string(decoded), nil
}
