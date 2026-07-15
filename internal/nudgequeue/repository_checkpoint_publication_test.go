package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCommandRepositoryPublishesCheckpointInBoundedIncrementalBatches(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{
		CommandStateDelivered,
		CommandStatePending,
		CommandStateExpired,
	})
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}

	first, caughtUp, err := repo.PublishCheckpoint(t.Context(), 1)
	if err != nil {
		t.Fatalf("first PublishCheckpoint: %v", err)
	}
	if caughtUp || first.Revision != 4 || first.SequenceHighWater != 3 {
		t.Fatalf("first publication = (%#v, caughtUp=%v), want revision 4 sequence 3 and more tail", first, caughtUp)
	}
	firstCheckpoint := repositoryCheckpointFromStoreForTest(t, store)
	if firstCheckpoint.SourceRevision != 3 || firstCheckpoint.PublishedRevision != 4 || firstCheckpoint.TerminalCount != 1 || firstCheckpoint.TombstoneCount != 0 || len(firstCheckpoint.Ranges) != 1 {
		t.Fatalf("first checkpoint = %#v", firstCheckpoint)
	}

	second, caughtUp, err := repo.PublishCheckpoint(t.Context(), 1)
	if err != nil {
		t.Fatalf("second PublishCheckpoint: %v", err)
	}
	if caughtUp || second.Revision != 5 || second.SequenceHighWater != 3 {
		t.Fatalf("second publication = (%#v, caughtUp=%v), want revision 5 sequence 3 and exact-full-page uncertainty", second, caughtUp)
	}
	secondCheckpoint := repositoryCheckpointFromStoreForTest(t, store)
	if secondCheckpoint.SourceRevision != 4 || secondCheckpoint.PublishedRevision != 5 || secondCheckpoint.TerminalCount != 2 || secondCheckpoint.TombstoneCount != 0 {
		t.Fatalf("second checkpoint = %#v", secondCheckpoint)
	}
	wantRanges := []CommandIndexSequenceRange{{FirstSequence: 1, LastSequence: 1}, {FirstSequence: 3, LastSequence: 3}}
	if !reflect.DeepEqual(secondCheckpoint.Ranges, wantRanges) {
		t.Fatalf("second ranges = %#v, want %#v", secondCheckpoint.Ranges, wantRanges)
	}

	final, caughtUp, err := repo.PublishCheckpoint(t.Context(), 1)
	if err != nil {
		t.Fatalf("final PublishCheckpoint: %v", err)
	}
	if !caughtUp || final != second {
		t.Fatalf("final publication = (%#v, caughtUp=%v), want no-op %#v", final, caughtUp, second)
	}
	if got := repositoryCheckpointFromStoreForTest(t, store); !reflect.DeepEqual(got, secondCheckpoint) {
		t.Fatalf("no-op publication changed checkpoint: got %#v, want %#v", got, secondCheckpoint)
	}
	if limits := store.snapshotPageLimitsForTest(); !reflect.DeepEqual(limits, []int{1, 1, 1}) {
		t.Fatalf("snapshot page limits = %v, want one bounded page per call", limits)
	}
}

func TestCommandRepositoryCheckpointPublicationIsAtomicAcrossCommitFailures(t *testing.T) {
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

	beforeErr := errors.New("checkpoint commit rejected")
	store.failNextCommit(beforeErr)
	if _, _, err := repo.PublishCheckpoint(t.Context(), 8); !errors.Is(err, beforeErr) {
		t.Fatalf("PublishCheckpoint before-commit error = %v, want %v", err, beforeErr)
	}
	if store.hasRow(commandRepositoryCheckpointID) {
		t.Fatal("failed pre-commit publication left a checkpoint")
	}
	if got := commandRepositoryStateFromMetadata(t, store.metadataSnapshot()); got.Revision != 1 || got.SequenceHighWater != 1 {
		t.Fatalf("failed pre-commit publication advanced state: %#v", got)
	}

	afterErr := errors.New("checkpoint response lost after commit")
	store.mu.Lock()
	store.failAfterCommitNext = afterErr
	store.mu.Unlock()
	got, caughtUp, err := repo.PublishCheckpoint(t.Context(), 8)
	if err != nil {
		t.Fatalf("ambiguous committed PublishCheckpoint: %v", err)
	}
	if !caughtUp || got.Revision != 2 || got.SequenceHighWater != 1 {
		t.Fatalf("ambiguous committed publication = (%#v, caughtUp=%v)", got, caughtUp)
	}
	checkpoint := repositoryCheckpointFromStoreForTest(t, store)
	if checkpoint.PublishedRevision != 2 || checkpoint.TerminalCount != 1 {
		t.Fatalf("ambiguous committed checkpoint = %#v", checkpoint)
	}
}

func TestCommandRepositoryCheckpointRejectsClosedOpaqueOrNonTerminalRows(t *testing.T) {
	t.Parallel()

	tests := map[string]func(*testing.T, *repositoryAtomicTestStore, CommandRepositoryState){
		"known non-terminal": func(t *testing.T, store *repositoryAtomicTestStore, state CommandRepositoryState) {
			seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStatePending})
			store.mu.Lock()
			for id, row := range store.rows {
				if id != commandRepositoryCheckpointID {
					row.Status = "closed"
					store.rows[id] = row
				}
			}
			store.mu.Unlock()
		},
		"opaque future": func(t *testing.T, store *repositoryAtomicTestStore, state CommandRepositoryState) {
			commandID := CommandIDForRequest(state.Store, "checkpoint-future")
			raw := []byte(fmt.Sprintf(`{"version":2,"id":%q,"target":{"session_id":"session-future","intent_generation":1},"store":{"store_uuid":%q,"restore_epoch":%d},"order":{"sequence":1,"revision":1}}`, commandID, state.Store.StoreUUID, state.Store.RestoreEpoch))
			store.seedRawCommand(t, state, "checkpoint-future", commandID, raw, 1, 1)
			store.mu.Lock()
			row := store.rows[commandID]
			row.Status = "closed"
			store.rows[commandID] = row
			store.mu.Unlock()
		},
	}
	for name, seed := range tests {
		t.Run(name, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repo := newVerifiedCommandRepository(t, store)
			state, err := repo.State(t.Context())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			seed(t, store, state)
			if _, err := repo.RepairLineage(t.Context()); err != nil {
				t.Fatalf("RepairLineage after seed: %v", err)
			}
			if _, _, err := repo.PublishCheckpoint(t.Context(), 8); !errors.Is(err, ErrCommandRepositoryRecord) {
				t.Fatalf("PublishCheckpoint error = %v, want ErrCommandRepositoryRecord", err)
			}
			if store.hasRow(commandRepositoryCheckpointID) {
				t.Fatal("poison tail was checkpointed")
			}
		})
	}
}

func TestCommandRepositoryCheckpointPublicationHonorsCancellationBeforeCAS(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStateDelivered, CommandStateExpired})
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.mu.Lock()
	store.afterSnapshotPage = cancel
	store.mu.Unlock()
	if _, _, err := repo.PublishCheckpoint(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("PublishCheckpoint cancellation error = %v, want context.Canceled", err)
	}
	if store.hasRow(commandRepositoryCheckpointID) {
		t.Fatal("canceled checkpoint read reached publication CAS")
	}
	if got := commandRepositoryStateFromMetadata(t, store.metadataSnapshot()); got.Revision != 2 || got.SequenceHighWater != 2 {
		t.Fatalf("canceled checkpoint changed state: %#v", got)
	}
}

func TestCommandRepositoryCheckpointRestoreRewindFailsClosed(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	verifier := &repositoryLineageTestVerifier{}
	repo, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	state, err := repo.Provision(t.Context())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	seedRepositoryCheckpointCommands(t, store, state, []CommandState{CommandStateDelivered})
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seed: %v", err)
	}
	published, _, err := repo.PublishCheckpoint(t.Context(), 8)
	if err != nil {
		t.Fatalf("PublishCheckpoint: %v", err)
	}
	if published.Revision != 2 {
		t.Fatalf("published state = %#v", published)
	}

	store.mu.Lock()
	delete(store.rows, commandRepositoryCheckpointID)
	rewound := published
	rewound.Revision = 1
	store.metadata = repositoryMetadataForTest(rewound)
	store.mu.Unlock()
	if _, _, err := repo.PublishCheckpoint(t.Context(), 8); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("PublishCheckpoint after restore rewind error = %v, want lineage refusal", err)
	}
	if store.hasRow(commandRepositoryCheckpointID) {
		t.Fatal("restore rewind resurrected a checkpoint")
	}
	if anchored := verifier.anchorSnapshot(); anchored == nil || *anchored != published {
		t.Fatalf("restore rewind changed independent anchor: got %#v, want %#v", anchored, published)
	}
}

func seedRepositoryCheckpointCommands(t *testing.T, store *repositoryAtomicTestStore, state CommandRepositoryState, states []CommandState) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	for offset, commandState := range states {
		sequence := uint64(offset + 1)
		requestID := fmt.Sprintf("checkpoint-seed-%06d", sequence)
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
		clock := store.nextClock.Add(time.Duration(sequence) * time.Nanosecond)
		store.rows[command.ID] = beads.Bead{
			ID: command.ID, Title: commandRecordTitle, Status: status, Type: commandRecordBeadType,
			CreatedAt: clock, UpdatedAt: clock,
			Metadata: map[string]string{
				commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
				commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
				commandRecordRequestIDMetadataKey:   requestID,
				commandRecordWireMetadataKey:        string(wire),
			},
		}
	}
	state.Revision = uint64(len(states))
	state.SequenceHighWater = uint64(len(states))
	store.metadata = repositoryMetadataForTest(state)
}

func repositoryCheckpointFromStoreForTest(t *testing.T, store *repositoryAtomicTestStore) commandRepositoryCheckpoint {
	t.Helper()
	row, err := store.Get(commandRepositoryCheckpointID)
	if err != nil {
		t.Fatalf("Get checkpoint: %v", err)
	}
	checkpoint, err := decodeCommandRepositoryCheckpointRecord(row)
	if err != nil {
		t.Fatalf("decode checkpoint record: %v", err)
	}
	return checkpoint
}
