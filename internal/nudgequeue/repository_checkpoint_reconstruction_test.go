package nudgequeue

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCommandRepositoryReconstructsFourActiveFrom100003LifetimeRows(t *testing.T) {
	const (
		terminalLifetime = 99_999
		activeCount      = 4
		sequenceHigh     = terminalLifetime + activeCount
	)
	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	initial, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	baseTime := time.Date(2026, 7, 15, 14, 0, 0, 0, time.UTC)
	store.mu.Lock()
	for sequence := 1; sequence <= terminalLifetime; sequence++ {
		id := fmt.Sprintf("%shistory-%06d", commandIDPrefix, sequence)
		clock := baseTime.Add(time.Duration(sequence) * time.Nanosecond)
		store.rows[id] = beads.Bead{ID: id, Status: "closed", CreatedAt: clock, UpdatedAt: clock}
	}
	state := initial
	state.Revision = sequenceHigh + 1
	state.SequenceHighWater = sequenceHigh
	for offset := 1; offset < activeCount; offset++ {
		sequence := uint64(terminalLifetime + offset)
		requestID := fmt.Sprintf("lifetime-active-%d", offset)
		row := repositoryCheckpointCommandRowForTest(t, state, requestID, CommandStatePending, sequence, baseTime.Add(time.Duration(sequence)*time.Nanosecond))
		store.rows[row.ID] = row
	}
	opaqueSequence := uint64(sequenceHigh)
	opaqueRequestID := "lifetime-active-opaque"
	opaqueID := CommandIDForRequest(state.Store, opaqueRequestID)
	opaqueRaw := []byte(fmt.Sprintf("  {\n\"version\":2,\"id\":%q,\"target\":{\"session_id\":\"session-future\",\"intent_generation\":9},\"store\":{\"store_uuid\":%q,\"restore_epoch\":%d},\"order\":{\"sequence\":%d,\"revision\":%d},\"future\":{\"marker\":\"preserve whitespace\"}}  \n", opaqueID, state.Store.StoreUUID, state.Store.RestoreEpoch, opaqueSequence, opaqueSequence))
	opaqueClock := baseTime.Add(time.Duration(opaqueSequence) * time.Nanosecond)
	store.rows[opaqueID] = beads.Bead{
		ID: opaqueID, Title: commandRecordTitle, Status: "open", Type: commandRecordBeadType,
		CreatedAt: opaqueClock, UpdatedAt: opaqueClock,
		Metadata: map[string]string{
			commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
			commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
			commandRecordRequestIDMetadataKey:   opaqueRequestID,
			commandRecordWireMetadataKey:        string(opaqueRaw),
		},
	}
	checkpoint := sealRepositoryCheckpointForTest(t, commandRepositoryCheckpoint{
		Version:           commandRepositoryCheckpointVersion,
		Store:             state.Store,
		SourceRevision:    sequenceHigh,
		PublishedRevision: sequenceHigh + 1,
		SequenceHighWater: sequenceHigh,
		TerminalCursor:    &commandRepositoryCheckpointCursor{UpdatedAt: baseTime.Add(terminalLifetime * time.Nanosecond), ID: fmt.Sprintf("%shistory-%06d", commandIDPrefix, terminalLifetime)},
		Ranges:            []CommandIndexSequenceRange{{FirstSequence: 1, LastSequence: terminalLifetime}},
		TerminalCount:     terminalLifetime,
	})
	checkpointRow, err := commandRepositoryCheckpointRecord(checkpoint)
	if err != nil {
		store.mu.Unlock()
		t.Fatalf("commandRepositoryCheckpointRecord: %v", err)
	}
	checkpointRow.CreatedAt = opaqueClock.Add(time.Nanosecond)
	checkpointRow.UpdatedAt = checkpointRow.CreatedAt
	store.rows[checkpointRow.ID] = checkpointRow
	store.metadata = repositoryMetadataForTest(state)
	store.mu.Unlock()
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after lifetime seed: %v", err)
	}

	snapshot, err := repo.Snapshot(t.Context(), activeCount)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if len(snapshot.Entries) != activeCount || len(snapshot.Tombstones) != 0 || snapshot.Revision != state.Revision || snapshot.SequenceHighWater != sequenceHigh {
		t.Fatalf("lifetime snapshot = entries:%d tombstones:%d revision:%d sequence:%d", len(snapshot.Entries), len(snapshot.Tombstones), snapshot.Revision, snapshot.SequenceHighWater)
	}
	if snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != terminalLifetime || !reflect.DeepEqual(snapshot.Coverage.Ranges, checkpoint.Ranges) {
		t.Fatalf("lifetime compacted coverage = %#v", snapshot.Coverage)
	}
	foundOpaque := false
	for _, entry := range snapshot.Entries {
		if entry.Opaque != nil {
			foundOpaque = bytes.Equal(entry.Opaque.Raw, opaqueRaw)
		}
	}
	if !foundOpaque {
		t.Fatal("lifetime reconstruction did not preserve opaque active bytes")
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex(lifetime snapshot): %v", err)
	}
	queries := store.snapshotPageQueriesForTest()
	if len(queries) != 2 || queries[0].Status != "closed" || queries[0].Order != beads.AtomicReadSnapshotOrderUpdatedAtID || queries[0].Limit != 1 ||
		queries[1].Status != "open" || queries[1].Order != beads.AtomicReadSnapshotOrderID || queries[1].Limit > beads.MaxAtomicReadSnapshotPageSize {
		t.Fatalf("lifetime reconstruction queries = %#v", queries)
	}
}

func TestCommandRepositorySnapshotRequiresCheckpointCatchupForTerminalTail(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStateDelivered, CommandStatePending})
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	if _, err := repo.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryCheckpointRequired) {
		t.Fatalf("Snapshot before checkpoint error = %v, want checkpoint-required", err)
	}
	for {
		_, caughtUp, err := repo.PublishCheckpoint(t.Context(), 1)
		if err != nil {
			t.Fatalf("PublishCheckpoint: %v", err)
		}
		if caughtUp {
			break
		}
	}
	snapshot, err := repo.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot after checkpoint: %v", err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.State != CommandStatePending || snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != 1 {
		t.Fatalf("reconstructed checkpoint snapshot = %#v", snapshot)
	}
}

func TestCommandRepositorySnapshotRejectsTamperedCheckpointBeforeActiveDecode(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStateDelivered})
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	if _, _, err := repo.PublishCheckpoint(t.Context(), 8); err != nil {
		t.Fatalf("PublishCheckpoint: %v", err)
	}
	store.mu.Lock()
	row := store.rows[commandRepositoryCheckpointID]
	wire := row.Metadata[commandRepositoryCheckpointWireMetadataKey]
	row.Metadata = cloneRepositoryMetadata(row.Metadata)
	row.Metadata[commandRepositoryCheckpointWireMetadataKey] = string(bytes.Replace([]byte(wire), []byte(`"terminal_count":1`), []byte(`"terminal_count":2`), 1))
	store.rows[commandRepositoryCheckpointID] = row
	store.mu.Unlock()
	if _, err := repo.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryRecord) {
		t.Fatalf("Snapshot tampered checkpoint error = %v, want record refusal", err)
	}
}

func TestCommandRepositorySnapshotPagesActiveSetDeterministically(t *testing.T) {
	t.Parallel()

	const active = 600
	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	store.seedCommands(t, state, active)
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	first, err := repo.Snapshot(t.Context(), active)
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}
	queries := store.snapshotPageQueriesForTest()
	wantLimits := []int{1, 256, 256, 89}
	gotLimits := make([]int, len(queries))
	for i, query := range queries {
		gotLimits[i] = query.Limit
		if query.Limit > beads.MaxAtomicReadSnapshotPageSize {
			t.Fatalf("query %d limit = %d, max %d", i, query.Limit, beads.MaxAtomicReadSnapshotPageSize)
		}
	}
	if !reflect.DeepEqual(gotLimits, wantLimits) {
		t.Fatalf("active page limits = %v, want %v", gotLimits, wantLimits)
	}
	store.mu.Lock()
	store.snapshotPageQueries = nil
	store.snapshotPageLimits = nil
	store.mu.Unlock()
	second, err := repo.Snapshot(t.Context(), active)
	if err != nil {
		t.Fatalf("second Snapshot: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("repeated active reconstruction was nondeterministic")
	}
}

func TestCommandRepositorySnapshotRejectsTerminalWireLeftOpen(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStateDelivered})
	store.mu.Lock()
	for id, row := range store.rows {
		if id != commandRepositoryCheckpointID {
			row.Status = "open"
			store.rows[id] = row
		}
	}
	store.mu.Unlock()
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	if _, err := repo.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryRecord) {
		t.Fatalf("Snapshot terminal/open skew error = %v, want record refusal", err)
	}
}

func repositoryCheckpointCommandRowForTest(t *testing.T, state CommandRepositoryState, requestID string, commandState CommandState, sequence uint64, updatedAt time.Time) beads.Bead {
	t.Helper()
	command := validCommandV1(commandState)
	command.ID = CommandIDForRequest(state.Store, requestID)
	command.Store = state.Store
	command.Order = CommandOrder{Sequence: sequence, Revision: sequence}
	if command.Claim != nil {
		command.Claim.OperationID = command.ID
	}
	if command.Retry != nil {
		command.Retry.OperationID = command.ID
	}
	if command.Terminal != nil && command.Terminal.OperationID != "" {
		command.Terminal.OperationID = command.ID
	}
	command.TrustedIngress.ReferenceID = requestID
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1(%d): %v", sequence, err)
	}
	status := "open"
	if commandIsTerminalState(commandState) {
		status = "closed"
	}
	return beads.Bead{
		ID: command.ID, Title: commandRecordTitle, Status: status, Type: commandRecordBeadType,
		CreatedAt: updatedAt, UpdatedAt: updatedAt,
		Metadata: map[string]string{
			commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
			commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
			commandRecordRequestIDMetadataKey:   requestID,
			commandRecordWireMetadataKey:        string(wire),
		},
	}
}
