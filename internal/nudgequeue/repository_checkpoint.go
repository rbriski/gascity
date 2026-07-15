package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	commandRepositoryCheckpointVersion uint32 = 1

	commandRepositoryCheckpointID                = "gc-control-command-checkpoint"
	commandRepositoryCheckpointTitle             = "durable control command checkpoint"
	commandRepositoryCheckpointKindMetadataValue = "checkpoint"
	commandRepositoryCheckpointWireMetadataKey   = "gc.control.checkpoint_wire"
	commandRepositoryCheckpointFingerprintDomain = "gascity.command-repository.checkpoint.v1"
	maxCommandRepositoryCheckpointWireBytes      = 1 << 20
)

type commandRepositoryCheckpointCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	ID        string    `json:"id"`
}

type commandRepositoryCheckpoint struct {
	Version           uint32                             `json:"version"`
	Store             CommandStoreBinding                `json:"store"`
	SourceRevision    uint64                             `json:"source_revision"`
	PublishedRevision uint64                             `json:"published_revision"`
	SequenceHighWater uint64                             `json:"sequence_high_water"`
	TerminalCursor    *commandRepositoryCheckpointCursor `json:"terminal_cursor"`
	Ranges            []CommandIndexSequenceRange        `json:"ranges"`
	TerminalCount     uint64                             `json:"terminal_count"`
	TombstoneCount    uint64                             `json:"tombstone_count"`
	FingerprintSHA256 string                             `json:"fingerprint_sha256"`
}

func sealCommandRepositoryCheckpoint(checkpoint commandRepositoryCheckpoint) (commandRepositoryCheckpoint, error) {
	checkpoint = cloneCommandRepositoryCheckpoint(checkpoint)
	checkpoint.FingerprintSHA256 = ""
	if err := validateCommandRepositoryCheckpointSemantics(checkpoint); err != nil {
		return commandRepositoryCheckpoint{}, err
	}
	checkpoint.FingerprintSHA256 = commandRepositoryCheckpointFingerprint(checkpoint)
	if err := validateCommandRepositoryCheckpoint(checkpoint); err != nil {
		return commandRepositoryCheckpoint{}, err
	}
	return checkpoint, nil
}

func cloneCommandRepositoryCheckpoint(checkpoint commandRepositoryCheckpoint) commandRepositoryCheckpoint {
	checkpoint.Ranges = append([]CommandIndexSequenceRange(nil), checkpoint.Ranges...)
	if checkpoint.TerminalCursor != nil {
		cursor := *checkpoint.TerminalCursor
		checkpoint.TerminalCursor = &cursor
	}
	return checkpoint
}

func commandRepositoryCheckpointFingerprint(checkpoint commandRepositoryCheckpoint) string {
	checkpoint = cloneCommandRepositoryCheckpoint(checkpoint)
	checkpoint.FingerprintSHA256 = ""
	wire, err := json.Marshal(checkpoint)
	if err != nil {
		return ""
	}
	digest := sha256.New()
	_, _ = io.WriteString(digest, commandRepositoryCheckpointFingerprintDomain)
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(wire)
	return hex.EncodeToString(digest.Sum(nil))
}

func validateCommandRepositoryCheckpoint(checkpoint commandRepositoryCheckpoint) error {
	if err := validateCommandRepositoryCheckpointSemantics(checkpoint); err != nil {
		return err
	}
	digest, err := hex.DecodeString(checkpoint.FingerprintSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != checkpoint.FingerprintSHA256 {
		return errors.New("command repository checkpoint fingerprint is not a canonical SHA-256 digest")
	}
	want, err := hex.DecodeString(commandRepositoryCheckpointFingerprint(checkpoint))
	if err != nil || subtle.ConstantTimeCompare(digest, want) != 1 {
		return errors.New("command repository checkpoint fingerprint does not match its canonical payload")
	}
	return nil
}

func validateCommandRepositoryCheckpointSemantics(checkpoint commandRepositoryCheckpoint) error {
	if checkpoint.Version != commandRepositoryCheckpointVersion {
		return fmt.Errorf("command repository checkpoint version %d, want %d", checkpoint.Version, commandRepositoryCheckpointVersion)
	}
	if err := validateCommandRepositoryBinding(checkpoint.Store); err != nil {
		return fmt.Errorf("command repository checkpoint store binding: %w", err)
	}
	if checkpoint.SourceRevision == 0 || checkpoint.SourceRevision == math.MaxUint64 || checkpoint.PublishedRevision != checkpoint.SourceRevision+1 {
		return fmt.Errorf("command repository checkpoint publication revision %d is not source revision %d plus one", checkpoint.PublishedRevision, checkpoint.SourceRevision)
	}
	if checkpoint.SequenceHighWater == 0 || checkpoint.SequenceHighWater > checkpoint.SourceRevision {
		return fmt.Errorf("command repository checkpoint sequence high-water %d is outside source revision %d", checkpoint.SequenceHighWater, checkpoint.SourceRevision)
	}
	if checkpoint.TerminalCursor == nil {
		return errors.New("command repository checkpoint terminal cursor is absent")
	}
	if err := validateCommandTime("command repository checkpoint terminal cursor updated_at", checkpoint.TerminalCursor.UpdatedAt); err != nil {
		return err
	}
	if err := validateCommandIdentity("command repository checkpoint terminal cursor id", checkpoint.TerminalCursor.ID); err != nil {
		return err
	}
	if len(checkpoint.TerminalCursor.ID) < len(commandIDPrefix) || checkpoint.TerminalCursor.ID[:len(commandIDPrefix)] != commandIDPrefix {
		return fmt.Errorf("command repository checkpoint terminal cursor id %q is outside command prefix", checkpoint.TerminalCursor.ID)
	}
	if len(checkpoint.Ranges) == 0 {
		return errors.New("command repository checkpoint has no compacted ranges")
	}
	var (
		covered   uint64
		priorLast uint64
	)
	for i, sequenceRange := range checkpoint.Ranges {
		if sequenceRange.FirstSequence == 0 || sequenceRange.LastSequence < sequenceRange.FirstSequence || sequenceRange.LastSequence > checkpoint.SequenceHighWater {
			return fmt.Errorf("command repository checkpoint range %d [%d,%d] is outside sequence high-water %d", i, sequenceRange.FirstSequence, sequenceRange.LastSequence, checkpoint.SequenceHighWater)
		}
		if i > 0 {
			if sequenceRange.FirstSequence <= priorLast {
				return fmt.Errorf("command repository checkpoint range %d overlaps or is out of order", i)
			}
			if priorLast != math.MaxUint64 && sequenceRange.FirstSequence == priorLast+1 {
				return fmt.Errorf("command repository checkpoint range %d is adjacent instead of canonically merged", i)
			}
		}
		length := sequenceRange.LastSequence - sequenceRange.FirstSequence + 1
		var overflow bool
		covered, overflow = addCommandIndexUint64(covered, length)
		if overflow {
			return errors.New("command repository checkpoint covered sequence count overflows uint64")
		}
		priorLast = sequenceRange.LastSequence
	}
	conserved, overflow := addCommandIndexUint64(checkpoint.TerminalCount, checkpoint.TombstoneCount)
	if overflow || conserved != covered {
		return fmt.Errorf("command repository checkpoint conserves %d terminal plus %d tombstone records, want %d covered sequences", checkpoint.TerminalCount, checkpoint.TombstoneCount, covered)
	}
	return nil
}

func encodeCommandRepositoryCheckpoint(checkpoint commandRepositoryCheckpoint) ([]byte, error) {
	if err := validateCommandRepositoryCheckpoint(checkpoint); err != nil {
		return nil, err
	}
	wire, err := json.Marshal(checkpoint)
	if err != nil {
		return nil, fmt.Errorf("encoding command repository checkpoint: %w", err)
	}
	if len(wire) > maxCommandRepositoryCheckpointWireBytes {
		return nil, fmt.Errorf("command repository checkpoint wire is %d bytes, limit %d", len(wire), maxCommandRepositoryCheckpointWireBytes)
	}
	return wire, nil
}

func decodeCommandRepositoryCheckpoint(wire []byte) (commandRepositoryCheckpoint, error) {
	if len(wire) == 0 || len(wire) > maxCommandRepositoryCheckpointWireBytes {
		return commandRepositoryCheckpoint{}, fmt.Errorf("command repository checkpoint wire size %d is outside 1..%d", len(wire), maxCommandRepositoryCheckpointWireBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.DisallowUnknownFields()
	var checkpoint commandRepositoryCheckpoint
	if err := decoder.Decode(&checkpoint); err != nil {
		return commandRepositoryCheckpoint{}, fmt.Errorf("decoding command repository checkpoint: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return commandRepositoryCheckpoint{}, fmt.Errorf("decoding command repository checkpoint: %w", err)
	}
	if err := validateCommandRepositoryCheckpoint(checkpoint); err != nil {
		return commandRepositoryCheckpoint{}, err
	}
	canonical, err := json.Marshal(checkpoint)
	if err != nil {
		return commandRepositoryCheckpoint{}, fmt.Errorf("re-encoding command repository checkpoint: %w", err)
	}
	if !bytes.Equal(wire, canonical) {
		return commandRepositoryCheckpoint{}, errors.New("command repository checkpoint wire is not canonical JSON")
	}
	return cloneCommandRepositoryCheckpoint(checkpoint), nil
}

func commandRepositoryCheckpointRecord(checkpoint commandRepositoryCheckpoint) (beads.Bead, error) {
	wire, err := encodeCommandRepositoryCheckpoint(checkpoint)
	if err != nil {
		return beads.Bead{}, err
	}
	return beads.Bead{
		ID:     commandRepositoryCheckpointID,
		Title:  commandRepositoryCheckpointTitle,
		Status: "open",
		Type:   commandRecordBeadType,
		Metadata: map[string]string{
			commandRecordKindMetadataKey:               commandRepositoryCheckpointKindMetadataValue,
			commandRepositoryCheckpointWireMetadataKey: string(wire),
		},
	}, nil
}

func decodeCommandRepositoryCheckpointRecord(record beads.Bead) (commandRepositoryCheckpoint, error) {
	if record.ID != commandRepositoryCheckpointID || record.Title != commandRepositoryCheckpointTitle || record.Status != "open" || record.Type != commandRecordBeadType || record.Ephemeral || record.NoHistory {
		return commandRepositoryCheckpoint{}, &CommandRepositoryRecordError{CommandID: record.ID, Err: errors.New("checkpoint storage identity is non-canonical")}
	}
	if record.Priority != nil || record.Assignee != "" || record.From != "" || record.ParentID != "" || record.Ref != "" ||
		len(record.Needs) != 0 || record.Description != "" || len(record.Labels) != 0 || len(record.Dependencies) != 0 ||
		record.DeferUntil != nil || record.IsBlocked != nil {
		return commandRepositoryCheckpoint{}, &CommandRepositoryRecordError{CommandID: record.ID, Err: errors.New("checkpoint has unrelated bead fields")}
	}
	if len(record.Metadata) != 2 || record.Metadata[commandRecordKindMetadataKey] != commandRepositoryCheckpointKindMetadataValue {
		return commandRepositoryCheckpoint{}, &CommandRepositoryRecordError{CommandID: record.ID, Err: errors.New("checkpoint metadata contract is non-canonical")}
	}
	wire, ok := record.Metadata[commandRepositoryCheckpointWireMetadataKey]
	if !ok || wire == "" {
		return commandRepositoryCheckpoint{}, &CommandRepositoryRecordError{CommandID: record.ID, Err: errors.New("checkpoint canonical wire is absent")}
	}
	checkpoint, err := decodeCommandRepositoryCheckpoint([]byte(wire))
	if err != nil {
		return commandRepositoryCheckpoint{}, &CommandRepositoryRecordError{CommandID: record.ID, Err: err}
	}
	return checkpoint, nil
}
