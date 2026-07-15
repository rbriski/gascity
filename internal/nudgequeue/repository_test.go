package nudgequeue

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/google/uuid"
)

func TestCommandRepositoryInitializesAndReusesCryptographicStoreBinding(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	first, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State first: %v", err)
	}
	second, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State second: %v", err)
	}
	if first != second {
		t.Fatalf("State changed without a write: first=%#v second=%#v", first, second)
	}
	if _, err := uuid.Parse(first.Store.StoreUUID); err != nil {
		t.Fatalf("store UUID %q is not a UUID: %v", first.Store.StoreUUID, err)
	}
	if first.Store.RestoreEpoch != 1 || first.SchemaVersion != CommandRepositorySchemaVersion ||
		first.WriterVersion != CommandRepositoryWriterVersion || first.Revision != 0 || first.SequenceHighWater != 0 {
		t.Fatalf("fresh repository state = %#v", first)
	}

	other, err := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore()).State(t.Context())
	if err != nil {
		t.Fatalf("other State: %v", err)
	}
	if other.Store.StoreUUID == first.Store.StoreUUID {
		t.Fatalf("two fresh stores reused UUID %q", first.Store.StoreUUID)
	}
}

func TestCommandRepositoryReadsNeverInitializeOrMutateLineage(t *testing.T) {
	operations := map[string]func(context.Context, *CommandRepository) error{
		"state": func(ctx context.Context, repo *CommandRepository) error {
			_, err := repo.State(ctx)
			return err
		},
		"get": func(ctx context.Context, repo *CommandRepository) error {
			_, err := repo.Get(ctx, "missing-command")
			return err
		},
		"snapshot": func(ctx context.Context, repo *CommandRepository) error {
			_, err := repo.Snapshot(ctx, 1)
			return err
		},
	}

	for name, operation := range operations {
		t.Run(name+" with equal durable lineage", func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			verifier := &repositoryLineageTestVerifier{}
			repo, err := NewCommandRepository(store, verifier)
			if err != nil {
				t.Fatalf("NewCommandRepository: %v", err)
			}
			if _, err := repo.Provision(t.Context()); err != nil {
				t.Fatalf("Provision: %v", err)
			}
			beforeCreate, beforeMetadata := store.durableMutationCallCounts()
			beforeProvision := verifier.provisionCallCount()
			beforeAdvance := verifier.advanceCallCount()

			if err := operation(t.Context(), repo); err != nil {
				t.Fatalf("%s equal-lineage read: %v", name, err)
			}
			afterCreate, afterMetadata := store.durableMutationCallCounts()
			if afterCreate != beforeCreate || afterMetadata != beforeMetadata {
				t.Fatalf("%s successful read called durable mutations: Create=%d->%d SetMetadata=%d->%d", name, beforeCreate, afterCreate, beforeMetadata, afterMetadata)
			}
			if got := verifier.provisionCallCount(); got != beforeProvision {
				t.Fatalf("%s successful read provision calls = %d->%d", name, beforeProvision, got)
			}
			if got := verifier.advanceCallCount(); got != beforeAdvance {
				t.Fatalf("%s successful read advance calls = %d->%d", name, beforeAdvance, got)
			}
		})

		t.Run(name+" with all repository metadata absent", func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			verifier := &repositoryLineageTestVerifier{}
			repo, err := NewCommandRepository(store, verifier)
			if err != nil {
				t.Fatalf("NewCommandRepository: %v", err)
			}

			if err := operation(t.Context(), repo); !errors.Is(err, ErrCommandRepositoryLineage) {
				t.Fatalf("%s error = %v, want fail-closed lineage error", name, err)
			}
			createCalls, metadataWriteCalls := store.durableMutationCallCounts()
			if createCalls != 0 || metadataWriteCalls != 0 {
				t.Fatalf("%s called durable mutations: Create=%d SetMetadata=%d", name, createCalls, metadataWriteCalls)
			}
			if verifier.provisionCallCount() != 0 {
				t.Fatalf("%s provision calls = %d, want zero", name, verifier.provisionCallCount())
			}
			if verifier.advanceCallCount() != 0 {
				t.Fatalf("%s advance calls = %d, want zero", name, verifier.advanceCallCount())
			}
			if len(store.metadataSnapshot()) != 0 || store.hasAnyRows() {
				t.Fatalf("%s changed absent repository: metadata=%#v rows=%v", name, store.metadataSnapshot(), store.hasAnyRows())
			}
		})

		t.Run(name+" with database ahead of lineage", func(t *testing.T) {
			anchor := CommandRepositoryState{
				Store:             CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 1},
				SchemaVersion:     CommandRepositorySchemaVersion,
				WriterVersion:     CommandRepositoryWriterVersion,
				Revision:          0,
				SequenceHighWater: 0,
			}
			database := anchor
			database.Revision = 1
			store := newRepositoryAtomicTestStore()
			store.seedMetadata(database)
			anchored := anchor
			verifier := &repositoryLineageTestVerifier{anchor: &anchored}
			repo, err := NewCommandRepository(store, verifier)
			if err != nil {
				t.Fatalf("NewCommandRepository: %v", err)
			}

			if err := operation(t.Context(), repo); !errors.Is(err, ErrCommandRepositoryLineage) {
				t.Fatalf("%s error = %v, want fail-closed lineage error", name, err)
			}
			createCalls, metadataWriteCalls := store.durableMutationCallCounts()
			if createCalls != 0 || metadataWriteCalls != 0 {
				t.Fatalf("%s called durable mutations: Create=%d SetMetadata=%d", name, createCalls, metadataWriteCalls)
			}
			if got := verifier.anchorSnapshot(); got == nil || *got != anchor {
				t.Fatalf("%s advanced lineage during read: got %#v, want %#v", name, got, anchor)
			}
			if verifier.advanceCallCount() != 0 {
				t.Fatalf("%s advance calls = %d, want zero", name, verifier.advanceCallCount())
			}
		})
	}
}

func TestCommandRepositoryProvisionAndRepairAreExplicitWriterOperations(t *testing.T) {
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
	if state.Store.RestoreEpoch != 1 || state.Revision != 0 || state.SequenceHighWater != 0 {
		t.Fatalf("fresh provisioned state = %#v", state)
	}
	createCalls, metadataWriteCalls := store.durableMutationCallCounts()
	if createCalls != 0 || metadataWriteCalls != 6 {
		t.Fatalf("Provision mutation calls: Create=%d SetMetadata=%d, want 0/6", createCalls, metadataWriteCalls)
	}
	if verifier.provisionCallCount() != 1 || verifier.advanceCallCount() != 0 {
		t.Fatalf("Provision lineage calls: provision=%d advance=%d, want 1/0", verifier.provisionCallCount(), verifier.advanceCallCount())
	}

	if got, err := repo.Provision(t.Context()); err != nil || got != state {
		t.Fatalf("idempotent Provision = (%#v, %v), want original state", got, err)
	}
	createCalls, metadataWriteCalls = store.durableMutationCallCounts()
	if createCalls != 0 || metadataWriteCalls != 6 || verifier.provisionCallCount() != 1 || verifier.advanceCallCount() != 0 {
		t.Fatalf("idempotent Provision wrote again: Create=%d SetMetadata=%d provision=%d advance=%d", createCalls, metadataWriteCalls, verifier.provisionCallCount(), verifier.advanceCallCount())
	}

	databaseAhead := state
	databaseAhead.Revision = 1
	store.seedMetadata(databaseAhead)
	if _, err := repo.State(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("State database-ahead error = %v, want lineage refusal", err)
	}
	if got, err := repo.RepairLineage(t.Context()); err != nil || got != databaseAhead {
		t.Fatalf("RepairLineage = (%#v, %v), want %#v", got, err, databaseAhead)
	}
	if verifier.advanceCallCount() != 1 {
		t.Fatalf("RepairLineage advance calls = %d, want 1", verifier.advanceCallCount())
	}
	if got, err := repo.State(t.Context()); err != nil || got != databaseAhead {
		t.Fatalf("State after RepairLineage = (%#v, %v), want %#v", got, err, databaseAhead)
	}
	createCalls, metadataWriteCalls = store.durableMutationCallCounts()
	if createCalls != 0 || metadataWriteCalls != 6 {
		t.Fatalf("lineage repair mutated repository: Create=%d SetMetadata=%d", createCalls, metadataWriteCalls)
	}
}

func TestCommandRepositoryCreateNeverImplicitlyProvisions(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	verifier := &repositoryLineageTestVerifier{}
	repo, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	requestID := "request-before-provision"
	command := repositoryCommandForRequest(t, CommandStoreBinding{}, requestID, "before provision")

	if _, _, err := repo.Create(t.Context(), requestID, command); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("Create before Provision error = %v, want lineage refusal", err)
	}
	createCalls, metadataWriteCalls := store.durableMutationCallCounts()
	if createCalls != 0 || metadataWriteCalls != 0 || verifier.provisionCallCount() != 0 || verifier.advanceCallCount() != 0 {
		t.Fatalf("Create implicitly provisioned: Create=%d SetMetadata=%d provision=%d advance=%d", createCalls, metadataWriteCalls, verifier.provisionCallCount(), verifier.advanceCallCount())
	}
}

func TestCommandRepositoryCreateIsIdempotentAndReadsExactDurableAuthority(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	requestID := "request-1"
	command := repositoryCommandForRequest(t, state.Store, requestID, "first")

	createdEntry, created, err := repo.Create(t.Context(), requestID, command)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created || createdEntry.Command == nil {
		t.Fatalf("Create = (%#v, %v), want created known command", createdEntry, created)
	}
	if got := createdEntry.Command.Order; got != (CommandOrder{Sequence: 1, Revision: 1}) {
		t.Fatalf("created order = %#v, want sequence/revision 1", got)
	}

	retriedEntry, retriedCreated, err := repo.Create(t.Context(), requestID, command)
	if err != nil {
		t.Fatalf("idempotent Create: %v", err)
	}
	if retriedCreated || retriedEntry.Command == nil || !reflect.DeepEqual(*retriedEntry.Command, *createdEntry.Command) {
		t.Fatalf("idempotent Create = (%#v, %v), want original command", retriedEntry, retriedCreated)
	}

	resolution, err := repo.Get(t.Context(), command.ID)
	if err != nil {
		t.Fatalf("Get exact: %v", err)
	}
	if !resolution.Found || resolution.Entry.Command == nil || resolution.Entry.Command.ID != command.ID || resolution.Revision != 1 {
		t.Fatalf("Get exact = %#v", resolution)
	}
	missing, err := repo.Get(t.Context(), "missing-command")
	if err != nil {
		t.Fatalf("Get missing: %v", err)
	}
	if missing.Found || missing.Revision != 1 || missing.Store != resolution.Store {
		t.Fatalf("Get missing = %#v, want not found at current authority", missing)
	}

	snapshot, err := repo.Snapshot(t.Context(), MaxCommandRepositorySnapshotCommands)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Store != resolution.Store || snapshot.Revision != 1 || snapshot.SequenceHighWater != 1 || len(snapshot.Entries) != 1 {
		t.Fatalf("Snapshot = %#v", snapshot)
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex(repository snapshot): %v", err)
	}
}

func TestCommandRepositoryRejectsConflictingRetryWithoutAllocating(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	requestID := "request-conflict"
	command := repositoryCommandForRequest(t, state.Store, requestID, "first")
	if _, _, err := repo.Create(t.Context(), requestID, command); err != nil {
		t.Fatalf("Create first: %v", err)
	}
	conflict := repositoryCommandForRequest(t, state.Store, requestID, "changed")
	if _, _, err := repo.Create(t.Context(), requestID, conflict); !errors.Is(err, ErrCommandRepositoryIdempotencyConflict) {
		t.Fatalf("conflicting Create error = %v, want ErrCommandRepositoryIdempotencyConflict", err)
	}
	state, err = repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state.Revision != 1 || state.SequenceHighWater != 1 {
		t.Fatalf("state after conflict = %#v, want no allocation", state)
	}
}

func TestCommandRepositoryConcurrentCreatesAllocateDenseGlobalOrder(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("initialize State: %v", err)
	}

	const count = 64
	entries := make(chan CommandIndexEntry, count)
	errs := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		requestID := fmt.Sprintf("request-concurrent-%03d", i)
		command := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, created, err := repo.Create(context.Background(), requestID, command)
			if err == nil && !created {
				err = errors.New("first create reported idempotent retry")
			}
			if err != nil {
				errs <- err
				return
			}
			entries <- entry
		}()
	}
	wg.Wait()
	close(errs)
	close(entries)
	for err := range errs {
		t.Errorf("concurrent Create: %v", err)
	}
	if t.Failed() {
		return
	}

	sequences := make([]int, 0, count)
	revisions := make([]int, 0, count)
	for entry := range entries {
		sequences = append(sequences, int(entry.Command.Order.Sequence))
		revisions = append(revisions, int(entry.Command.Order.Revision))
	}
	sort.Ints(sequences)
	sort.Ints(revisions)
	for i := 0; i < count; i++ {
		if sequences[i] != i+1 || revisions[i] != i+1 {
			t.Fatalf("dense order mismatch at %d: sequences=%v revisions=%v", i, sequences, revisions)
		}
	}
	snapshot, err := repo.Snapshot(t.Context(), count)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Revision != count || snapshot.SequenceHighWater != count || len(snapshot.Entries) != count {
		t.Fatalf("snapshot watermarks = revision:%d sequence:%d entries:%d, want %d", snapshot.Revision, snapshot.SequenceHighWater, len(snapshot.Entries), count)
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex(concurrent snapshot): %v", err)
	}
}

func TestCommandRepositoryCreateRollsBackRowAndDenseAllocationsTogether(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("initialize State: %v", err)
	}
	wantErr := errors.New("commit failed after callback")
	store.failNextCommit(wantErr)
	requestID := "request-rollback"
	command := repositoryCommandForRequest(t, state.Store, requestID, "rollback")
	if _, _, err := repo.Create(t.Context(), requestID, command); !errors.Is(err, wantErr) {
		t.Fatalf("Create error = %v, want %v", err, wantErr)
	}
	state, err = repo.State(t.Context())
	if err != nil {
		t.Fatalf("State after rollback: %v", err)
	}
	if state.Revision != 0 || state.SequenceHighWater != 0 || store.hasRow(command.ID) {
		t.Fatalf("rollback left state=%#v row=%v", state, store.hasRow(command.ID))
	}
	entry, created, err := repo.Create(t.Context(), requestID, command)
	if err != nil || !created || entry.Command.Order != (CommandOrder{Sequence: 1, Revision: 1}) {
		t.Fatalf("Create after rollback = (%#v, %v, %v), want dense first allocation", entry, created, err)
	}
}

func TestCommandRepositorySnapshotIsCompleteOrBoundedError(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	for _, limit := range []int{0, -1, MaxCommandRepositorySnapshotCommands + 1} {
		if _, err := repo.Snapshot(t.Context(), limit); !errors.Is(err, ErrCommandRepositorySnapshotLimit) {
			t.Fatalf("Snapshot(limit=%d) error = %v, want ErrCommandRepositorySnapshotLimit", limit, err)
		}
	}
	store.seedCommands(t, state, MaxCommandRepositorySnapshotCommands+1)
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seeding overflow: %v", err)
	}
	if _, err := repo.Snapshot(t.Context(), MaxCommandRepositorySnapshotCommands); !errors.Is(err, ErrCommandRepositorySnapshotLimit) {
		t.Fatalf("overflow Snapshot error = %v, want ErrCommandRepositorySnapshotLimit", err)
	}
}

func TestCommandRepositoryPreservesOpaqueNewerCommandByteForByte(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	commandID := CommandIDForRequest(state.Store, "future-request")
	raw := []byte(fmt.Sprintf("  {\n\"version\":2,\"id\":%q,\"target\":{\"session_id\":\"session-future\",\"intent_generation\":9},\"store\":{\"store_uuid\":%q,\"restore_epoch\":%d},\"order\":{\"sequence\":1,\"revision\":1},\"future\":{\"marker\":\"preserve whitespace\"}}  \n", commandID, state.Store.StoreUUID, state.Store.RestoreEpoch))
	store.seedRawCommand(t, state, "future-request", commandID, raw, 1, 1)
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seeding opaque command: %v", err)
	}

	resolution, err := repo.Get(t.Context(), commandID)
	if err != nil {
		t.Fatalf("Get opaque: %v", err)
	}
	if !resolution.Found || resolution.Entry.Opaque == nil || !bytes.Equal(resolution.Entry.Opaque.Raw, raw) {
		t.Fatalf("Get opaque = %#v, raw preserved=%v", resolution, resolution.Entry.Opaque != nil && bytes.Equal(resolution.Entry.Opaque.Raw, raw))
	}
	snapshot, err := repo.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot opaque: %v", err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Opaque == nil || !bytes.Equal(snapshot.Entries[0].Opaque.Raw, raw) {
		t.Fatalf("Snapshot did not byte-preserve opaque command: %#v", snapshot.Entries)
	}
}

func TestCommandRepositoryRejectsUnsupportedSchemaSkewAndLineage(t *testing.T) {
	t.Parallel()

	verifier := &repositoryLineageTestVerifier{}
	if _, err := NewCommandRepository(beads.NewMemStore(), verifier); !errors.Is(err, ErrCommandRepositoryUnsupported) {
		t.Fatalf("NewCommandRepository(MemStore) error = %v, want unsupported", err)
	} else {
		var typed *CommandRepositoryUnsupportedError
		if !errors.As(err, &typed) {
			t.Fatalf("unsupported error type = %T", err)
		}
	}
	var nilVerifier *repositoryLineageTestVerifier
	if _, err := NewCommandRepository(newRepositoryAtomicTestStore(), nilVerifier); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("NewCommandRepository(typed nil verifier) error = %v, want lineage refusal", err)
	}

	partial := newRepositoryAtomicTestStore()
	partial.metadata[commandRepositoryStoreUUIDMetadataKey] = uuid.NewString()
	partialRepo, err := NewCommandRepository(partial, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(partial): %v", err)
	}
	if _, err := partialRepo.State(t.Context()); !errors.Is(err, ErrCommandRepositorySchemaSkew) {
		t.Fatalf("partial metadata State error = %v, want schema skew", err)
	} else {
		var typed *CommandRepositorySchemaSkewError
		if !errors.As(err, &typed) {
			t.Fatalf("schema skew error type = %T", err)
		}
	}

	skewed := newRepositoryAtomicTestStore()
	skewed.seedMetadata(CommandRepositoryState{
		Store:             CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 1},
		SchemaVersion:     CommandRepositorySchemaVersion + 1,
		WriterVersion:     CommandRepositoryWriterVersion,
		Revision:          0,
		SequenceHighWater: 0,
	})
	skewedRepo, err := NewCommandRepository(skewed, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(skewed): %v", err)
	}
	if _, err := skewedRepo.State(t.Context()); !errors.Is(err, ErrCommandRepositorySchemaSkew) {
		t.Fatalf("newer schema State error = %v, want schema skew", err)
	}

	canonicalState := CommandRepositoryState{
		Store:         CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 1},
		SchemaVersion: CommandRepositorySchemaVersion,
		WriterVersion: CommandRepositoryWriterVersion,
	}
	canonicalMutations := map[string]func(map[string]string){
		"leading-zero schema": func(metadata map[string]string) {
			metadata[commandRepositorySchemaVersionMetadataKey] = "01"
		},
		"spaced epoch": func(metadata map[string]string) {
			metadata[commandRepositoryRestoreEpochMetadataKey] = " 1 "
		},
		"spaced uuid": func(metadata map[string]string) {
			metadata[commandRepositoryStoreUUIDMetadataKey] = " " + metadata[commandRepositoryStoreUUIDMetadataKey] + " "
		},
		"newer writer": func(metadata map[string]string) {
			metadata[commandRepositoryWriterVersionMetadataKey] = fmt.Sprint(CommandRepositoryWriterVersion + 1)
		},
	}
	for name, mutate := range canonicalMutations {
		t.Run(name, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			store.seedMetadata(canonicalState)
			store.mu.Lock()
			mutate(store.metadata)
			store.mu.Unlock()
			repo, err := NewCommandRepository(store, verifier)
			if err != nil {
				t.Fatalf("NewCommandRepository: %v", err)
			}
			if _, err := repo.State(t.Context()); !errors.Is(err, ErrCommandRepositorySchemaSkew) {
				t.Fatalf("State error = %v, want canonical schema skew", err)
			}
		})
	}

	lineageCause := errors.New("independent anchor is ahead")
	lineageStore := newRepositoryAtomicTestStore()
	lineageState := CommandRepositoryState{
		Store:         CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 1},
		SchemaVersion: CommandRepositorySchemaVersion,
		WriterVersion: CommandRepositoryWriterVersion,
	}
	lineageStore.seedMetadata(lineageState)
	lineageAnchor := lineageState
	lineageRepo, err := NewCommandRepository(lineageStore, &repositoryLineageTestVerifier{
		anchor:           &lineageAnchor,
		verifyErr:        lineageCause,
		advanceAlwaysErr: lineageCause,
	})
	if err != nil {
		t.Fatalf("NewCommandRepository(lineage): %v", err)
	}
	if _, err := lineageRepo.State(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) || !errors.Is(err, lineageCause) {
		t.Fatalf("lineage State error = %v, want typed lineage cause", err)
	} else {
		var typed *CommandRepositoryLineageError
		if !errors.As(err, &typed) {
			t.Fatalf("lineage error type = %T", err)
		}
	}
	requestID := "request-lineage-denied"
	if _, _, err := lineageRepo.Create(t.Context(), requestID, repositoryCommandForRequest(t, CommandStoreBinding{}, requestID, "denied")); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("Create under lineage refusal error = %v", err)
	}
	if lineageStore.hasAnyRows() {
		t.Fatal("lineage refusal admitted a command")
	}
}

func TestCommandRepositoryCancellationDoesNotInitializeOrAdmit(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo, err := NewCommandRepository(store, &repositoryLineageTestVerifier{})
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := repo.State(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("State canceled error = %v", err)
	}
	requestID := "request-canceled"
	if _, _, err := repo.Create(ctx, requestID, repositoryCommandForRequest(t, CommandStoreBinding{}, requestID, "canceled")); !errors.Is(err, context.Canceled) {
		t.Fatalf("Create canceled error = %v", err)
	}
	if len(store.metadataSnapshot()) != 0 || store.hasAnyRows() {
		t.Fatalf("canceled operation mutated metadata=%#v rows=%v", store.metadataSnapshot(), store.hasAnyRows())
	}
}

func TestCommandRepositorySnapshotHonorsCancellationDuringDecode(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	store.seedCommands(t, state, 2)
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seeding cancellation fixture: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	store.mu.Lock()
	store.afterList = cancel
	store.mu.Unlock()
	if _, err := repo.Snapshot(ctx, 2); !errors.Is(err, context.Canceled) {
		t.Fatalf("Snapshot cancellation error = %v, want context.Canceled", err)
	}
}

func TestCommandRepositoryCachingStoreForwardsAtomicWritesAndLiveSnapshotsCoherently(t *testing.T) {
	t.Parallel()

	backing := newRepositoryAtomicTestStore()
	cache := beads.NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	repo := newVerifiedCommandRepository(t, cache)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State through cache: %v", err)
	}
	requestID := "request-cache"
	command := repositoryCommandForRequest(t, state.Store, requestID, "cache")
	entry, created, err := repo.Create(t.Context(), requestID, command)
	if err != nil || !created {
		t.Fatalf("Create through cache = (%#v, %v, %v)", entry, created, err)
	}
	cached, err := cache.Get(command.ID)
	if err != nil {
		t.Fatalf("cache.Get committed command: %v", err)
	}
	if cached.Metadata[commandRecordWireMetadataKey] == "" {
		t.Fatalf("cached committed command metadata = %#v", cached.Metadata)
	}
	snapshot, err := repo.Snapshot(t.Context(), 1)
	if err != nil || len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil {
		t.Fatalf("Snapshot through cache = (%#v, %v)", snapshot, err)
	}
}

func TestCommandRepositoryCommandIDForRequestIsDeterministicAndDomainSeparated(t *testing.T) {
	t.Parallel()

	binding := CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 3}
	first := CommandIDForRequest(binding, "request-stable")
	second := CommandIDForRequest(binding, "request-stable")
	other := CommandIDForRequest(binding, "request-other")
	otherBinding := CommandIDForRequest(CommandStoreBinding{StoreUUID: uuid.NewString(), RestoreEpoch: 3}, "request-stable")
	if first == "" || first != second || first == other || first == otherBinding || !strings.HasPrefix(first, "gc-nudge-") {
		t.Fatalf("CommandIDForRequest results = %q %q %q %q", first, second, other, otherBinding)
	}
	if got := CommandIDForRequest(binding, ""); got != "" {
		t.Fatalf("CommandIDForRequest(empty) = %q, want empty", got)
	}
}

func TestCommandRepositoryProvisioningEvidenceCannotReplayAfterRestartOrRestore(t *testing.T) {
	t.Parallel()

	provisionFailure := errors.New("anchor fsync failed")
	firstVerifier := &repositoryLineageTestVerifier{provisionErr: provisionFailure}
	store := newRepositoryAtomicTestStore()
	first, err := NewCommandRepository(store, firstVerifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(first): %v", err)
	}
	if _, err := first.Provision(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) || !errors.Is(err, provisionFailure) {
		t.Fatalf("first Provision error = %v, want typed provision failure", err)
	}
	if firstVerifier.provisionCallCount() != 1 {
		t.Fatalf("first provision calls = %d, want 1", firstVerifier.provisionCallCount())
	}
	// Model a restored database carrying a plausible replay token. The
	// repository does not own or read this key; only the exact initializing
	// process held provisioning evidence in memory.
	store.mu.Lock()
	store.metadata["gc.control.repository.provisioning_token"] = strings.Repeat("ab", 32)
	store.mu.Unlock()

	restartedVerifier := &repositoryLineageTestVerifier{}
	restarted, err := NewCommandRepository(store, restartedVerifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(restarted): %v", err)
	}
	if _, err := restarted.State(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("restarted State error = %v, want missing-anchor lineage refusal", err)
	}
	if _, err := restarted.Provision(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("restarted Provision error = %v, want missing-anchor lineage refusal", err)
	}
	if restartedVerifier.provisionCallCount() != 0 {
		t.Fatalf("restarted provision calls = %d, want 0", restartedVerifier.provisionCallCount())
	}

	// A retained higher anchor must refuse the replayed lower database state,
	// even when the restored metadata carries the same fake token.
	databaseState := commandRepositoryStateFromMetadata(t, store.metadataSnapshot())
	higher := databaseState
	higher.Revision = 9
	higher.SequenceHighWater = 7
	higherVerifier := &repositoryLineageTestVerifier{anchor: &higher}
	higherRepo, err := NewCommandRepository(store, higherVerifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(higher): %v", err)
	}
	if _, err := higherRepo.State(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("State below retained anchor error = %v, want lineage refusal", err)
	}
	if higherVerifier.provisionCallCount() != 0 {
		t.Fatalf("higher-anchor provision calls = %d, want 0", higherVerifier.provisionCallCount())
	}
}

func TestCommandRepositoryPostCommitInitializationErrorLeavesFailClosedMissingAnchor(t *testing.T) {
	t.Parallel()

	releaseErr := errors.New("named lock release result was uncertain")
	store := newRepositoryAtomicTestStore()
	store.failAfterCommitNext = errors.Join(beads.ErrAtomicReadWriteSerialization, releaseErr)
	firstVerifier := &repositoryLineageTestVerifier{}
	first, err := NewCommandRepository(store, firstVerifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(first): %v", err)
	}
	if _, err := first.Provision(t.Context()); !errors.Is(err, beads.ErrAtomicReadWriteSerialization) || !errors.Is(err, releaseErr) {
		t.Fatalf("first Provision error = %v, want ambiguous post-commit serialization error", err)
	}
	if firstVerifier.provisionCallCount() != 0 {
		t.Fatalf("first provision calls = %d, want 0 after ambiguous initialization commit", firstVerifier.provisionCallCount())
	}
	if state := commandRepositoryStateFromMetadata(t, store.metadataSnapshot()); state.Store.StoreUUID == "" {
		t.Fatal("ambiguous initialization error did not leave committed database state")
	}

	restartedVerifier := &repositoryLineageTestVerifier{}
	restarted, err := NewCommandRepository(store, restartedVerifier)
	if err != nil {
		t.Fatalf("NewCommandRepository(restarted): %v", err)
	}
	if _, err := restarted.State(t.Context()); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("restarted State error = %v, want missing-anchor lineage refusal", err)
	}
	if restartedVerifier.provisionCallCount() != 0 {
		t.Fatalf("restarted provision calls = %d, want 0 without one-shot evidence", restartedVerifier.provisionCallCount())
	}
}

func TestCommandRepositoryPostCommitAnchorFailureIsRepairedByIdempotentRetry(t *testing.T) {
	t.Parallel()

	verifier := &repositoryLineageTestVerifier{}
	store := newRepositoryAtomicTestStore()
	repo, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	state, err := repo.Provision(t.Context())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	anchorErr := errors.New("anchor write interrupted")
	verifier.failNextAdvance(anchorErr)
	requestID := "request-anchor-window"
	command := repositoryCommandForRequest(t, state.Store, requestID, "anchor window")
	if _, _, err := repo.Create(t.Context(), requestID, command); !errors.Is(err, ErrCommandRepositoryLineage) || !errors.Is(err, anchorErr) {
		t.Fatalf("Create post-commit error = %v, want typed anchor failure", err)
	}
	if !store.hasRow(command.ID) {
		t.Fatal("post-commit verifier failure rolled back the already durable command")
	}

	entry, created, err := repo.Create(t.Context(), requestID, command)
	if err != nil {
		t.Fatalf("idempotent repair Create: %v", err)
	}
	if created || entry.Command == nil || entry.Command.Order != (CommandOrder{Sequence: 1, Revision: 1}) {
		t.Fatalf("idempotent repair = (%#v, %v), want original first allocation", entry, created)
	}
	anchored := verifier.anchorSnapshot()
	if anchored == nil || anchored.Revision != 1 || anchored.SequenceHighWater != 1 {
		t.Fatalf("anchor after retry = %#v, want revision/sequence 1", anchored)
	}
}

func TestCommandRepositoryConcurrentSameRequestConsumesOneAllocation(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	requestID := "request-same-concurrent"
	command := repositoryCommandForRequest(t, state.Store, requestID, "same")

	const callers = 32
	var wg sync.WaitGroup
	results := make(chan bool, callers)
	errs := make(chan error, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			entry, created, err := repo.Create(context.Background(), requestID, command)
			if err != nil {
				errs <- err
				return
			}
			if entry.Command == nil || entry.Command.ID != command.ID {
				errs <- errors.New("same-request create returned wrong command")
				return
			}
			results <- created
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		t.Errorf("same-request Create: %v", err)
	}
	createdCount := 0
	for created := range results {
		if created {
			createdCount++
		}
	}
	if createdCount != 1 {
		t.Fatalf("created results = %d, want exactly 1", createdCount)
	}
	final, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	if final.Revision != 1 || final.SequenceHighWater != 1 {
		t.Fatalf("same-request state = %#v, want one dense allocation", final)
	}
}

func TestCommandRepositoryRejectsCallerAuthorityAndDigestBeforeAllocation(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	requestID := "request-authority"
	valid := repositoryCommandForRequest(t, state.Store, requestID, "authority")
	tests := map[string]struct {
		request string
		mutate  func(*Command)
	}{
		"wrong deterministic id": {request: requestID, mutate: func(c *Command) {
			c.ID = CommandIDForRequest(state.Store, "different-request")
			c.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(*c)
		}},
		"caller store": {request: requestID, mutate: func(c *Command) {
			c.Store = state.Store
		}},
		"caller order": {request: requestID, mutate: func(c *Command) {
			c.Order = CommandOrder{Sequence: 99, Revision: 99}
		}},
		"stale payload digest": {request: requestID, mutate: func(c *Command) {
			c.Message = "changed without restamping trusted ingress"
		}},
		"noncanonical request id": {request: " request-authority ", mutate: func(*Command) {}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			command := valid
			tc.mutate(&command)
			if _, _, err := repo.Create(t.Context(), tc.request, command); !errors.Is(err, ErrCommandRepositoryInvalidRequest) {
				t.Fatalf("Create error = %v, want ErrCommandRepositoryInvalidRequest", err)
			}
		})
	}
	final, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	if final.Revision != 0 || final.SequenceHighWater != 0 || store.hasAnyRows() {
		t.Fatalf("invalid requests allocated authority: state=%#v rows=%v", final, store.hasAnyRows())
	}
}

func TestCommandRepositoryExactReadRejectsSameIDWrongContractRow(t *testing.T) {
	t.Parallel()

	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	requestID := "request-poison"
	command := repositoryCommandForRequest(t, state.Store, requestID, "poison")
	command.Store = state.Store
	command.Order = CommandOrder{Sequence: 1, Revision: 1}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	store.mu.Lock()
	store.rows[command.ID] = beads.Bead{
		ID: command.ID, Title: "not a command", Status: "open", Type: "task",
		Metadata: map[string]string{
			commandRecordKindMetadataKey:        "other",
			commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
			commandRecordRequestIDMetadataKey:   requestID,
			commandRecordWireMetadataKey:        string(wire),
		},
	}
	state.Revision = 1
	state.SequenceHighWater = 1
	store.metadata = repositoryMetadataForTest(state)
	store.mu.Unlock()
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after seeding poison row: %v", err)
	}

	if _, err := repo.Get(t.Context(), command.ID); !errors.Is(err, ErrCommandRepositoryRecord) {
		t.Fatalf("Get poison error = %v, want ErrCommandRepositoryRecord", err)
	}
	if _, err := repo.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryRecord) {
		t.Fatalf("Snapshot poison error = %v, want ErrCommandRepositoryRecord", err)
	}
}

func newVerifiedCommandRepository(t *testing.T, store beads.Store) *CommandRepository {
	t.Helper()
	repo, err := NewCommandRepository(store, &repositoryLineageTestVerifier{})
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repo.Provision(t.Context()); err != nil {
		t.Fatalf("Provision command repository: %v", err)
	}
	return repo
}

func repositoryCommandForRequest(t *testing.T, binding CommandStoreBinding, requestID, marker string) Command {
	t.Helper()
	command := validCommandV1(CommandStatePending)
	command.ID = CommandIDForRequest(binding, requestID)
	command.Store = CommandStoreBinding{}
	command.Order = CommandOrder{}
	command.Message = "wake up: " + marker
	command.TrustedIngress.ReferenceID = requestID
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	return command
}

type repositoryLineageTestVerifier struct {
	mu sync.Mutex

	anchor           *CommandRepositoryState
	provisionErr     error
	advanceErr       error
	advanceAlwaysErr error
	verifyErr        error
	provisionCalls   int
	advanceCalls     int
}

func (v *repositoryLineageTestVerifier) ProvisionCommandRepositoryLineage(_ context.Context, state CommandRepositoryState, evidence CommandRepositoryProvisioningEvidence) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.provisionCalls++
	if !evidence.validFor(state) {
		return errors.New("invalid one-shot provisioning evidence")
	}
	if v.provisionErr != nil {
		return v.provisionErr
	}
	if v.anchor == nil {
		anchored := state
		v.anchor = &anchored
		return nil
	}
	if *v.anchor != state {
		return errors.New("anchor already belongs to different repository state")
	}
	return nil
}

func (v *repositoryLineageTestVerifier) VerifyCommandRepositoryLineage(_ context.Context, state CommandRepositoryState) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.verifyErr != nil {
		return v.verifyErr
	}
	if v.anchor == nil {
		return errors.New("restore anchor is missing")
	}
	if v.anchor.Store != state.Store || v.anchor.SchemaVersion != state.SchemaVersion || v.anchor.WriterVersion != state.WriterVersion {
		return errors.New("restore anchor binding or version differs")
	}
	if state.Revision < v.anchor.Revision || state.SequenceHighWater < v.anchor.SequenceHighWater {
		return errors.New("database high water regressed below restore anchor")
	}
	if state.Revision > v.anchor.Revision || state.SequenceHighWater > v.anchor.SequenceHighWater {
		return errors.New("database high water is ahead of restore anchor")
	}
	return nil
}

func (v *repositoryLineageTestVerifier) AdvanceCommandRepositoryLineage(_ context.Context, state CommandRepositoryState) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.advanceCalls++
	if v.advanceAlwaysErr != nil {
		return v.advanceAlwaysErr
	}
	if v.anchor == nil {
		return errors.New("restore anchor is missing")
	}
	if v.anchor.Store != state.Store || v.anchor.SchemaVersion != state.SchemaVersion || v.anchor.WriterVersion != state.WriterVersion {
		return errors.New("restore anchor binding or version differs")
	}
	if state.Revision < v.anchor.Revision || state.SequenceHighWater < v.anchor.SequenceHighWater {
		return errors.New("database high water regressed below restore anchor")
	}
	if state.Revision > v.anchor.Revision || state.SequenceHighWater > v.anchor.SequenceHighWater {
		if v.advanceErr != nil {
			err := v.advanceErr
			v.advanceErr = nil
			return err
		}
		advanced := state
		v.anchor = &advanced
	}
	return nil
}

func (v *repositoryLineageTestVerifier) failNextAdvance(err error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.advanceErr = err
}

func (v *repositoryLineageTestVerifier) provisionCallCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.provisionCalls
}

func (v *repositoryLineageTestVerifier) advanceCallCount() int {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.advanceCalls
}

func (v *repositoryLineageTestVerifier) anchorSnapshot() *CommandRepositoryState {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.anchor == nil {
		return nil
	}
	snapshot := *v.anchor
	return &snapshot
}

func commandRepositoryStateFromMetadata(t *testing.T, metadata map[string]string) CommandRepositoryState {
	t.Helper()
	parse := func(key string) uint64 {
		t.Helper()
		value, err := strconv.ParseUint(metadata[key], 10, 64)
		if err != nil {
			t.Fatalf("parse metadata %s=%q: %v", key, metadata[key], err)
		}
		return value
	}
	return CommandRepositoryState{
		Store: CommandStoreBinding{
			StoreUUID:    metadata[commandRepositoryStoreUUIDMetadataKey],
			RestoreEpoch: parse(commandRepositoryRestoreEpochMetadataKey),
		},
		SchemaVersion:     uint32(parse(commandRepositorySchemaVersionMetadataKey)),
		WriterVersion:     uint32(parse(commandRepositoryWriterVersionMetadataKey)),
		Revision:          parse(commandRepositoryRevisionMetadataKey),
		SequenceHighWater: parse(commandRepositorySequenceHighWaterMetadataKey),
	}
}

type repositoryAtomicTestStore struct {
	beads.Store

	mu                  sync.Mutex
	rows                map[string]beads.Bead
	metadata            map[string]string
	failNext            error
	failAfterCommitNext error
	nextClock           time.Time
	afterList           func()
	createCalls         int
	setMetadataCalls    int
}

func newRepositoryAtomicTestStore() *repositoryAtomicTestStore {
	return &repositoryAtomicTestStore{
		Store:     beads.NewMemStore(),
		rows:      make(map[string]beads.Bead),
		metadata:  make(map[string]string),
		nextClock: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
	}
}

func (s *repositoryAtomicTestStore) AtomicReadWrite(ctx context.Context, _ string, fn func(beads.AtomicReadWriteTx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx := &repositoryAtomicTestTx{
		rows:      cloneRepositoryRows(s.rows),
		metadata:  cloneRepositoryMetadata(s.metadata),
		now:       s.nextClock,
		afterList: s.afterList,
		onCreate: func() {
			s.createCalls++
		},
		onSetMetadata: func() {
			s.setMetadataCalls++
		},
	}
	if err := fn(tx); err != nil {
		return err
	}
	changed := !reflect.DeepEqual(s.rows, tx.rows) || !reflect.DeepEqual(s.metadata, tx.metadata)
	if s.failNext != nil && changed {
		err := s.failNext
		s.failNext = nil
		return err
	}
	s.rows = tx.rows
	s.metadata = tx.metadata
	s.nextClock = tx.now.Add(time.Nanosecond)
	if s.failAfterCommitNext != nil && changed {
		err := s.failAfterCommitNext
		s.failAfterCommitNext = nil
		return err
	}
	return nil
}

func (s *repositoryAtomicTestStore) Get(id string) (beads.Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	row, ok := s.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneRepositoryRow(row), nil
}

func (s *repositoryAtomicTestStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows := make([]beads.Bead, 0, len(s.rows))
	for _, row := range s.rows {
		if query.Matches(row) {
			rows = append(rows, cloneRepositoryRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if query.Limit > 0 && len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (s *repositoryAtomicTestStore) failNextCommit(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failNext = err
}

func (s *repositoryAtomicTestStore) hasRow(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.rows[id]
	return ok
}

func (s *repositoryAtomicTestStore) hasAnyRows() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows) != 0
}

func (s *repositoryAtomicTestStore) metadataSnapshot() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneRepositoryMetadata(s.metadata)
}

func (s *repositoryAtomicTestStore) durableMutationCallCounts() (create, setMetadata int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.createCalls, s.setMetadataCalls
}

func (s *repositoryAtomicTestStore) seedMetadata(state CommandRepositoryState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.metadata = repositoryMetadataForTest(state)
}

func (s *repositoryAtomicTestStore) seedRawCommand(t *testing.T, state CommandRepositoryState, requestID, commandID string, wire []byte, sequence, revision uint64) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows[commandID] = beads.Bead{
		ID:        commandID,
		Title:     commandRecordTitle,
		Status:    "open",
		Type:      commandRecordBeadType,
		CreatedAt: s.nextClock,
		Metadata: map[string]string{
			commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
			commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
			commandRecordRequestIDMetadataKey:   requestID,
			commandRecordWireMetadataKey:        string(wire),
		},
	}
	state.SequenceHighWater = sequence
	state.Revision = revision
	s.metadata = repositoryMetadataForTest(state)
}

func (s *repositoryAtomicTestStore) seedCommands(t *testing.T, state CommandRepositoryState, count int) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := 1; i <= count; i++ {
		requestID := fmt.Sprintf("seed-request-%05d", i)
		command := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		command.Store = state.Store
		command.Order = CommandOrder{Sequence: uint64(i), Revision: uint64(i)}
		wire, err := EncodeCommandV1(command)
		if err != nil {
			t.Fatalf("EncodeCommandV1(seed %d): %v", i, err)
		}
		s.rows[command.ID] = beads.Bead{
			ID: command.ID, Title: commandRecordTitle, Status: "open", Type: commandRecordBeadType,
			CreatedAt: s.nextClock.Add(time.Duration(i) * time.Nanosecond),
			Metadata: map[string]string{
				commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
				commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
				commandRecordRequestIDMetadataKey:   requestID,
				commandRecordWireMetadataKey:        string(wire),
			},
		}
	}
	state.SequenceHighWater = uint64(count)
	state.Revision = uint64(count)
	s.metadata = repositoryMetadataForTest(state)
}

type repositoryAtomicTestTx struct {
	rows          map[string]beads.Bead
	metadata      map[string]string
	now           time.Time
	afterList     func()
	onCreate      func()
	onSetMetadata func()
}

func (tx *repositoryAtomicTestTx) GetIssue(id string) (beads.Bead, error) {
	row, ok := tx.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneRepositoryRow(row), nil
}

func (tx *repositoryAtomicTestTx) ListHistory(query beads.AtomicReadWriteList) ([]beads.Bead, error) {
	ids := make(map[string]struct{}, len(query.IDs))
	for _, id := range query.IDs {
		ids[id] = struct{}{}
	}
	rows := make([]beads.Bead, 0, min(len(tx.rows), query.Limit))
	for _, row := range tx.rows {
		if len(ids) > 0 {
			if _, ok := ids[row.ID]; !ok {
				continue
			}
		}
		if query.IDPrefix != "" && !strings.HasPrefix(row.ID, query.IDPrefix) {
			continue
		}
		if query.IssueType != "" && row.Type != query.IssueType {
			continue
		}
		matched := true
		for key, value := range query.Metadata {
			if row.Metadata[key] != value {
				matched = false
				break
			}
		}
		if matched && !row.Ephemeral && !row.NoHistory {
			rows = append(rows, cloneRepositoryRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	if tx.afterList != nil {
		tx.afterList()
		tx.afterList = nil
	}
	return rows, nil
}

func (tx *repositoryAtomicTestTx) Create(row beads.Bead) (beads.Bead, error) {
	if tx.onCreate != nil {
		tx.onCreate()
	}
	if _, exists := tx.rows[row.ID]; exists {
		return beads.Bead{}, fmt.Errorf("bead %q already exists", row.ID)
	}
	if row.Status == "" {
		row.Status = "open"
	}
	if row.Type == "" {
		row.Type = "task"
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = tx.now
	}
	tx.rows[row.ID] = cloneRepositoryRow(row)
	return cloneRepositoryRow(row), nil
}

func (tx *repositoryAtomicTestTx) Update(id string, opts beads.UpdateOpts) error {
	row, ok := tx.rows[id]
	if !ok {
		return beads.ErrNotFound
	}
	if opts.Title != nil {
		row.Title = *opts.Title
	}
	if opts.Status != nil {
		row.Status = *opts.Status
	}
	if opts.Type != nil {
		row.Type = *opts.Type
	}
	for key, value := range opts.Metadata {
		if row.Metadata == nil {
			row.Metadata = make(map[string]string)
		}
		row.Metadata[key] = value
	}
	tx.rows[id] = cloneRepositoryRow(row)
	return nil
}

func (tx *repositoryAtomicTestTx) GetMetadata(key string) (string, error) {
	return tx.metadata[key], nil
}

func (tx *repositoryAtomicTestTx) SetMetadata(key, value string) error {
	if tx.onSetMetadata != nil {
		tx.onSetMetadata()
	}
	tx.metadata[key] = value
	return nil
}

func cloneRepositoryRows(rows map[string]beads.Bead) map[string]beads.Bead {
	cloned := make(map[string]beads.Bead, len(rows))
	for id, row := range rows {
		cloned[id] = cloneRepositoryRow(row)
	}
	return cloned
}

func cloneRepositoryRow(row beads.Bead) beads.Bead {
	row.Metadata = cloneRepositoryMetadata(row.Metadata)
	row.Labels = append([]string(nil), row.Labels...)
	return row
}

func cloneRepositoryMetadata(metadata map[string]string) map[string]string {
	cloned := make(map[string]string, len(metadata))
	for key, value := range metadata {
		cloned[key] = value
	}
	return cloned
}

func repositoryMetadataForTest(state CommandRepositoryState) map[string]string {
	return map[string]string{
		commandRepositorySchemaVersionMetadataKey:     fmt.Sprint(state.SchemaVersion),
		commandRepositoryWriterVersionMetadataKey:     fmt.Sprint(state.WriterVersion),
		commandRepositoryStoreUUIDMetadataKey:         state.Store.StoreUUID,
		commandRepositoryRestoreEpochMetadataKey:      fmt.Sprint(state.Store.RestoreEpoch),
		commandRepositoryRevisionMetadataKey:          fmt.Sprint(state.Revision),
		commandRepositorySequenceHighWaterMetadataKey: fmt.Sprint(state.SequenceHighWater),
	}
}
