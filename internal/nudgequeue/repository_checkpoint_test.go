package nudgequeue

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCommandRepositoryCheckpointRecordCanonicalRoundTrip(t *testing.T) {
	t.Parallel()

	want := sealRepositoryCheckpointForTest(t, validRepositoryCheckpointForTest())
	record, err := commandRepositoryCheckpointRecord(want)
	if err != nil {
		t.Fatalf("commandRepositoryCheckpointRecord: %v", err)
	}
	if record.ID != commandRepositoryCheckpointID || record.Title != commandRepositoryCheckpointTitle || record.Status != "open" || record.Type != commandRecordBeadType {
		t.Fatalf("checkpoint record identity = %#v", record)
	}
	if record.Ephemeral || record.NoHistory || record.Metadata[commandRecordKindMetadataKey] != commandRepositoryCheckpointKindMetadataValue {
		t.Fatalf("checkpoint record storage contract = %#v", record)
	}
	got, err := decodeCommandRepositoryCheckpointRecord(record)
	if err != nil {
		t.Fatalf("decodeCommandRepositoryCheckpointRecord: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decoded checkpoint = %#v, want %#v", got, want)
	}
	wire, err := encodeCommandRepositoryCheckpoint(want)
	if err != nil {
		t.Fatalf("encodeCommandRepositoryCheckpoint: %v", err)
	}
	if string(wire) != record.Metadata[commandRepositoryCheckpointWireMetadataKey] {
		t.Fatalf("record wire is not the canonical codec output")
	}
}

func TestCommandRepositoryCheckpointRejectsSemanticAndFingerprintSkew(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*commandRepositoryCheckpoint){
		"version": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.Version++
		},
		"invalid store": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.Store.StoreUUID = "not-a-canonical-uuid"
		},
		"publication is not source plus one": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.PublishedRevision++
		},
		"sequence ahead of source": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.SequenceHighWater = checkpoint.SourceRevision + 1
		},
		"adjacent ranges": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.Ranges = []CommandIndexSequenceRange{{FirstSequence: 1, LastSequence: 2}, {FirstSequence: 3, LastSequence: 7}}
		},
		"count mismatch": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.TerminalCount++
		},
		"nil ranges": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.Ranges = nil
		},
		"non UTC cursor": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.TerminalCursor.UpdatedAt = checkpoint.TerminalCursor.UpdatedAt.In(time.FixedZone("offset", 3600))
		},
		"foreign cursor id": func(checkpoint *commandRepositoryCheckpoint) {
			checkpoint.TerminalCursor.ID = "other-command"
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			checkpoint := cloneCommandRepositoryCheckpoint(sealRepositoryCheckpointForTest(t, validRepositoryCheckpointForTest()))
			mutate(&checkpoint)
			checkpoint.FingerprintSHA256 = commandRepositoryCheckpointFingerprint(checkpoint)
			if err := validateCommandRepositoryCheckpoint(checkpoint); err == nil {
				t.Fatal("validateCommandRepositoryCheckpoint accepted semantic skew")
			}
		})
	}

	checkpoint := sealRepositoryCheckpointForTest(t, validRepositoryCheckpointForTest())
	checkpoint.FingerprintSHA256 = strings.Repeat("0", 64)
	if err := validateCommandRepositoryCheckpoint(checkpoint); err == nil {
		t.Fatal("validateCommandRepositoryCheckpoint accepted a forged fingerprint")
	}
}

func TestCommandRepositoryCheckpointCodecRejectsNonCanonicalOrExtendedWire(t *testing.T) {
	t.Parallel()

	checkpoint := sealRepositoryCheckpointForTest(t, validRepositoryCheckpointForTest())
	wire, err := encodeCommandRepositoryCheckpoint(checkpoint)
	if err != nil {
		t.Fatalf("encodeCommandRepositoryCheckpoint: %v", err)
	}
	tests := map[string][]byte{
		"leading whitespace": append([]byte(" "), wire...),
		"trailing value":     append(append([]byte(nil), wire...), []byte("{}")...),
		"unknown field":      []byte(strings.Replace(string(wire), "{", `{"unknown":true,`, 1)),
	}
	for name, candidate := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeCommandRepositoryCheckpoint(candidate); err == nil {
				t.Fatal("decodeCommandRepositoryCheckpoint accepted non-canonical wire")
			}
		})
	}
}

func TestCommandRepositoryCheckpointRecordRejectsStorageContractSkew(t *testing.T) {
	t.Parallel()

	checkpoint := sealRepositoryCheckpointForTest(t, validRepositoryCheckpointForTest())
	valid, err := commandRepositoryCheckpointRecord(checkpoint)
	if err != nil {
		t.Fatalf("commandRepositoryCheckpointRecord: %v", err)
	}
	tests := map[string]func(*beads.Bead){
		"id":     func(record *beads.Bead) { record.ID += "-other" },
		"title":  func(record *beads.Bead) { record.Title += " other" },
		"status": func(record *beads.Bead) { record.Status = "closed" },
		"type":   func(record *beads.Bead) { record.Type = "message" },
		"priority": func(record *beads.Bead) {
			priority := 1
			record.Priority = &priority
		},
		"assignee":    func(record *beads.Bead) { record.Assignee = "other" },
		"from":        func(record *beads.Bead) { record.From = "other" },
		"parent":      func(record *beads.Bead) { record.ParentID = "other" },
		"ref":         func(record *beads.Bead) { record.Ref = "other" },
		"needs":       func(record *beads.Bead) { record.Needs = []string{"other"} },
		"description": func(record *beads.Bead) { record.Description = "other" },
		"labels":      func(record *beads.Bead) { record.Labels = []string{"other"} },
		"dependencies": func(record *beads.Bead) {
			record.Dependencies = []beads.Dep{{IssueID: record.ID, DependsOnID: "other", Type: "blocks"}}
		},
		"ephemeral":  func(record *beads.Bead) { record.Ephemeral = true },
		"no history": func(record *beads.Bead) { record.NoHistory = true },
		"defer until": func(record *beads.Bead) {
			deferUntil := time.Date(2026, 7, 16, 0, 0, 0, 0, time.UTC)
			record.DeferUntil = &deferUntil
		},
		"blocked projection": func(record *beads.Bead) {
			blocked := true
			record.IsBlocked = &blocked
		},
		"kind": func(record *beads.Bead) {
			record.Metadata[commandRecordKindMetadataKey] = commandRecordKindMetadataValue
		},
		"missing wire": func(record *beads.Bead) {
			delete(record.Metadata, commandRepositoryCheckpointWireMetadataKey)
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			record := cloneRepositoryRowForCheckpointTest(valid)
			mutate(&record)
			if _, err := decodeCommandRepositoryCheckpointRecord(record); err == nil {
				t.Fatal("decodeCommandRepositoryCheckpointRecord accepted storage-contract skew")
			}
		})
	}
}

func validRepositoryCheckpointForTest() commandRepositoryCheckpoint {
	return commandRepositoryCheckpoint{
		Version:           commandRepositoryCheckpointVersion,
		Store:             CommandStoreBinding{StoreUUID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: 3},
		SourceRevision:    9,
		PublishedRevision: 10,
		SequenceHighWater: 7,
		TerminalCursor:    &commandRepositoryCheckpointCursor{UpdatedAt: time.Date(2026, 7, 15, 12, 30, 0, 123, time.UTC), ID: commandIDPrefix + "terminal"},
		Ranges:            []CommandIndexSequenceRange{{FirstSequence: 1, LastSequence: 3}, {FirstSequence: 5, LastSequence: 7}},
		TerminalCount:     5,
		TombstoneCount:    1,
		FingerprintSHA256: "",
	}
}

func sealRepositoryCheckpointForTest(t *testing.T, checkpoint commandRepositoryCheckpoint) commandRepositoryCheckpoint {
	t.Helper()
	sealed, err := sealCommandRepositoryCheckpoint(checkpoint)
	if err != nil {
		t.Fatalf("sealCommandRepositoryCheckpoint: %v", err)
	}
	return sealed
}

func cloneRepositoryRowForCheckpointTest(row beads.Bead) beads.Bead {
	original := row.Metadata
	row.Metadata = make(map[string]string, len(original))
	for key, value := range original {
		row.Metadata[key] = value
	}
	return row
}
