package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"
)

const (
	// MaxRestoreAnchorBytes bounds all bytes accepted by the independent anchor
	// decoder, including insignificant JSON whitespace.
	MaxRestoreAnchorBytes = 4 << 10
	// RestoreAnchorChecksumDomainV1 separates the anchor checksum from every
	// other Gas City SHA-256 protocol.
	RestoreAnchorChecksumDomainV1 = "gascity.nudge-command.restore-anchor.v1"
)

type restoreAnchorWire struct {
	Version                 uint32              `json:"version"`
	Store                   CommandStoreBinding `json:"store"`
	HighestAcceptedRevision uint64              `json:"highest_accepted_revision"`
	HighestAcceptedSequence uint64              `json:"highest_accepted_sequence"`
	ChecksumSHA256          string              `json:"checksum_sha256"`
}

type restoreAnchorDecodeWire struct {
	Version                 *uint32              `json:"version"`
	Store                   *CommandStoreBinding `json:"store"`
	HighestAcceptedRevision *uint64              `json:"highest_accepted_revision"`
	HighestAcceptedSequence *uint64              `json:"highest_accepted_sequence"`
	ChecksumSHA256          *string              `json:"checksum_sha256"`
}

type restoreAnchorChecksumPayload struct {
	Domain                  string              `json:"domain"`
	Version                 uint32              `json:"version"`
	Store                   CommandStoreBinding `json:"store"`
	HighestAcceptedRevision uint64              `json:"highest_accepted_revision"`
	HighestAcceptedSequence uint64              `json:"highest_accepted_sequence"`
}

// EncodeRestoreAnchor returns one canonical, checksummed, newline-terminated
// v1 anchor record. The checksum protects corruption; it is not a signature or
// an authorization credential.
func EncodeRestoreAnchor(anchor RestoreAnchor) ([]byte, error) {
	if err := ValidateRestoreAnchor(anchor); err != nil {
		return nil, fmt.Errorf("encoding nudge command restore anchor: %w", err)
	}
	checksum, err := computeRestoreAnchorChecksum(anchor)
	if err != nil {
		return nil, fmt.Errorf("encoding nudge command restore anchor: %w", err)
	}
	wire, err := json.Marshal(restoreAnchorWire{
		Version:                 anchor.Version,
		Store:                   anchor.Store,
		HighestAcceptedRevision: anchor.HighestAcceptedRevision,
		HighestAcceptedSequence: anchor.HighestAcceptedSequence,
		ChecksumSHA256:          checksum,
	})
	if err != nil {
		return nil, fmt.Errorf("encoding nudge command restore anchor: %w", err)
	}
	wire = append(wire, '\n')
	if len(wire) > MaxRestoreAnchorBytes {
		return nil, fmt.Errorf("encoding nudge command restore anchor: record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	return wire, nil
}

// DecodeRestoreAnchor totally validates one bounded v1 anchor record. It
// rejects unknown or duplicate fields, invalid Unicode, trailing JSON values,
// non-canonical checksums, and every invalid known-version value.
func DecodeRestoreAnchor(wire []byte) (RestoreAnchor, error) {
	if len(wire) == 0 {
		return RestoreAnchor{}, errors.New("decoding nudge command restore anchor: record is empty")
	}
	if len(wire) > MaxRestoreAnchorBytes {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: record exceeds %d bytes", MaxRestoreAnchorBytes)
	}
	if !utf8.Valid(wire) {
		return RestoreAnchor{}, errors.New("decoding nudge command restore anchor: record is not valid UTF-8")
	}
	trimmed := bytes.TrimSpace(wire)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return RestoreAnchor{}, errors.New("decoding nudge command restore anchor: record is not a JSON object")
	}
	if err := validateCommandJSONUnicodeEscapes(trimmed); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: invalid JSON string: %w", err)
	}
	if err := validateCommandJSONStructure(trimmed); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: invalid JSON structure: %w", err)
	}
	if err := validateRestoreAnchorJSONFields(trimmed); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var decoded restoreAnchorDecodeWire
	if err := decoder.Decode(&decoded); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}
	if decoded.Version == nil || decoded.Store == nil || decoded.HighestAcceptedRevision == nil || decoded.HighestAcceptedSequence == nil || decoded.ChecksumSHA256 == nil {
		return RestoreAnchor{}, errors.New("decoding nudge command restore anchor: required field is missing")
	}
	anchor := RestoreAnchor{
		Version:                 *decoded.Version,
		Store:                   *decoded.Store,
		HighestAcceptedRevision: *decoded.HighestAcceptedRevision,
		HighestAcceptedSequence: *decoded.HighestAcceptedSequence,
	}
	if err := ValidateRestoreAnchor(anchor); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}
	want, err := computeRestoreAnchorChecksum(anchor)
	if err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}
	if err := validateRestoreAnchorChecksum(*decoded.ChecksumSHA256, want); err != nil {
		return RestoreAnchor{}, fmt.Errorf("decoding nudge command restore anchor: %w", err)
	}
	return anchor, nil
}

func validateRestoreAnchorJSONFields(wire []byte) error {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(wire, &top); err != nil || top == nil {
		return errors.New("record is not a JSON object")
	}
	for _, field := range []string{"version", "store", "highest_accepted_revision", "highest_accepted_sequence", "checksum_sha256"} {
		if _, ok := top[field]; !ok {
			return fmt.Errorf("required field %q is missing or has non-canonical case", field)
		}
	}
	if len(top) != 5 {
		return errors.New("record contains an unknown field")
	}
	var store map[string]json.RawMessage
	if err := json.Unmarshal(top["store"], &store); err != nil || store == nil {
		return errors.New("store is not a JSON object")
	}
	for _, field := range []string{"store_uuid", "restore_epoch"} {
		if _, ok := store[field]; !ok {
			return fmt.Errorf("required store field %q is missing or has non-canonical case", field)
		}
	}
	if len(store) != 2 {
		return errors.New("store contains an unknown field")
	}
	return nil
}

func computeRestoreAnchorChecksum(anchor RestoreAnchor) (string, error) {
	payload, err := json.Marshal(restoreAnchorChecksumPayload{
		Domain:                  RestoreAnchorChecksumDomainV1,
		Version:                 anchor.Version,
		Store:                   anchor.Store,
		HighestAcceptedRevision: anchor.HighestAcceptedRevision,
		HighestAcceptedSequence: anchor.HighestAcceptedSequence,
	})
	if err != nil {
		return "", fmt.Errorf("computing checksum payload: %w", err)
	}
	digest := sha256.Sum256(payload)
	return hex.EncodeToString(digest[:]), nil
}

func validateRestoreAnchorChecksum(got, want string) error {
	if len(got) != sha256.Size*2 {
		return errors.New("checksum is not a canonical SHA-256 digest")
	}
	decoded, err := hex.DecodeString(got)
	if err != nil || hex.EncodeToString(decoded) != got {
		return errors.New("checksum is not a canonical SHA-256 digest")
	}
	wantBytes, err := hex.DecodeString(want)
	if err != nil {
		return errors.New("computed checksum is invalid")
	}
	if subtle.ConstantTimeCompare(decoded, wantBytes) != 1 {
		return errors.New("checksum does not match anchor fields")
	}
	return nil
}
