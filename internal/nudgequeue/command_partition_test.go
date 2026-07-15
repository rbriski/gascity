package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestCommandPartitionReaderIsolatesTwoCitiesSharingOneRepository(t *testing.T) {
	repo := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	cityA := trustedCityPartitionForTest("authority/city-a")
	cityB := trustedCityPartitionForTest("authority/city-b")
	resolver := newTestTrustedCityPartitionResolver()

	commandA1 := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityA, "request-a-1", "session-a", "caller-city-a")
	// The caller-authored scope deliberately claims city A while independent
	// authority resolves this reference to city B.
	commandB := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityB, "request-b-1", "session-b", "caller-city-a")
	commandA2 := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityA, "request-a-2", "session-a", "caller-city-a")

	readerA, err := NewCommandPartitionReader(repo, cityA, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city A): %v", err)
	}
	readerB, err := NewCommandPartitionReader(repo, cityB, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city B): %v", err)
	}

	snapshotA, err := readerA.Snapshot(t.Context(), 3)
	if err != nil {
		t.Fatalf("city A Snapshot: %v", err)
	}
	assertPartitionSnapshot(t, snapshotA, []string{commandA1.ID, commandA2.ID}, []uint64{commandB.Order.Sequence})
	snapshotB, err := readerB.Snapshot(t.Context(), 3)
	if err != nil {
		t.Fatalf("city B Snapshot: %v", err)
	}
	assertPartitionSnapshot(t, snapshotB, []string{commandB.ID}, []uint64{commandA1.Order.Sequence, commandA2.Order.Sequence})

	if index, err := BuildCommandIndex(snapshotA); err != nil {
		t.Fatalf("BuildCommandIndex(city A): %v", err)
	} else if foreign, err := index.Resolve(commandB.ID); err != nil || foreign.Found {
		t.Fatalf("city A Resolve(foreign) = %#v, err=%v", foreign, err)
	}
	if index, err := BuildCommandIndex(snapshotB); err != nil {
		t.Fatalf("BuildCommandIndex(city B): %v", err)
	} else if foreign, err := index.Resolve(commandA1.ID); err != nil || foreign.Found {
		t.Fatalf("city B Resolve(foreign) = %#v, err=%v", foreign, err)
	}

	foreignForA, err := readerA.Get(t.Context(), commandB.ID)
	if err != nil || foreignForA.Found || foreignForA.Entry != (CommandIndexEntry{}) {
		t.Fatalf("city A Get(city B command) = %#v, err=%v", foreignForA, err)
	}
	ownedForA, err := readerA.Get(t.Context(), commandA1.ID)
	if err != nil || !ownedForA.Found || ownedForA.Entry.Command == nil || ownedForA.Entry.Command.ID != commandA1.ID {
		t.Fatalf("city A Get(owned) = %#v, err=%v", ownedForA, err)
	}
}

func TestCommandPartitionReaderPreservesCompactedCoverageAcrossCities(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	cityA := trustedCityPartitionForTest("authority/city-a")
	cityB := trustedCityPartitionForTest("authority/city-b")
	resolver := newTestTrustedCityPartitionResolver()

	terminal := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityA, "request-terminal", "session-a", "caller-city-a")
	terminalRow := repositoryCheckpointCommandRowForTest(t, CommandRepositoryState{Store: state.Store}, "request-terminal", CommandStateDelivered, terminal.Order.Sequence, terminal.CreatedAt)
	state, err = repo.State(t.Context())
	if err != nil {
		t.Fatalf("State after terminal create: %v", err)
	}
	state.Revision++
	store.mu.Lock()
	store.rows[terminal.ID] = terminalRow
	store.metadata = repositoryMetadataForTest(state)
	store.mu.Unlock()
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after terminal write: %v", err)
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

	commandA := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityA, "request-a", "session-a", "caller-city-a")
	commandB := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityB, "request-b", "session-b", "caller-city-b")
	readerA, err := NewCommandPartitionReader(repo, cityA, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city A): %v", err)
	}
	snapshot, err := readerA.Snapshot(t.Context(), 2)
	if err != nil {
		t.Fatalf("city A Snapshot: %v", err)
	}
	assertPartitionSnapshot(t, snapshot, []string{commandA.ID}, []uint64{commandB.Order.Sequence})
	if snapshot.Coverage == nil || snapshot.Coverage.TerminalCount != 1 || len(snapshot.Coverage.Ranges) != 1 || snapshot.Coverage.Ranges[0] != (CommandIndexSequenceRange{FirstSequence: terminal.Order.Sequence, LastSequence: terminal.Order.Sequence}) {
		t.Fatalf("city A compacted coverage = %#v, want terminal sequence %d", snapshot.Coverage, terminal.Order.Sequence)
	}
}

func TestCommandPartitionReaderSameTrustedCityReopenIsStable(t *testing.T) {
	repo := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	firstPartition := trustedCityPartitionForTest("authority/city-a")
	firstResolver := newTestTrustedCityPartitionResolver()
	command := createPartitionedCommandForTest(t, repo, state.Store, firstResolver, firstPartition, "request-a", "session-a", "caller-city-a")
	firstReader, err := NewCommandPartitionReader(repo, firstPartition, firstResolver)
	if err != nil {
		t.Fatalf("first NewCommandPartitionReader: %v", err)
	}
	first, err := firstReader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("first Snapshot: %v", err)
	}

	reopenedPartition := trustedCityPartitionForTest("authority/city-a")
	reopenedResolver := newTestTrustedCityPartitionResolver()
	reopenedResolver.authorize(command.TrustedIngress, reopenedPartition)
	reopenedReader, err := NewCommandPartitionReader(repo, reopenedPartition, reopenedResolver)
	if err != nil {
		t.Fatalf("reopened NewCommandPartitionReader: %v", err)
	}
	reopened, err := reopenedReader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("reopened Snapshot: %v", err)
	}
	if !reflect.DeepEqual(reopened, first) {
		t.Fatalf("reopened snapshot = %#v, want %#v", reopened, first)
	}
}

func TestCommandPartitionReaderRejectsForgedCallerCityScope(t *testing.T) {
	repo := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	cityA := trustedCityPartitionForTest("authority/city-a")
	cityB := trustedCityPartitionForTest("authority/city-b")
	resolver := newTestTrustedCityPartitionResolver()

	command := repositoryCommandForRequest(t, state.Store, "request-forged", "forged")
	command.Target.SessionID = "session-a"
	command.TrustedIngress.TargetSessionID = command.Target.SessionID
	command.TrustedIngress.CityScope = "caller-city-a"
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	trustedReference := command.TrustedIngress
	resolver.authorize(trustedReference, cityA)

	command.TrustedIngress.CityScope = "caller-city-b"
	entry, created, err := repo.Create(t.Context(), "request-forged", command)
	if err != nil || !created || entry.Command == nil {
		t.Fatalf("Create caller-forged command = %#v, created=%t err=%v", entry, created, err)
	}
	readerB, err := NewCommandPartitionReader(repo, cityB, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city B): %v", err)
	}
	if _, err := readerB.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("city B forged-scope Snapshot error = %v, want partition refusal", err)
	}
	if _, err := readerB.Get(t.Context(), entry.Command.ID); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("city B forged-scope Get error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderRequiresVerifiedOpaqueAuthority(t *testing.T) {
	repo := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	partition := trustedCityPartitionForTest("authority/city-a")
	var typedNil *testTrustedCityPartitionResolver
	for name, candidate := range map[string]struct {
		partition TrustedCityPartition
		resolver  TrustedCityPartitionResolver
	}{
		"missing capability": {resolver: newTestTrustedCityPartitionResolver()},
		"missing resolver":   {partition: partition},
		"typed nil resolver": {partition: partition, resolver: typedNil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := NewCommandPartitionReader(repo, candidate.partition, candidate.resolver); !errors.Is(err, ErrCommandRepositoryPartition) {
				t.Fatalf("NewCommandPartitionReader error = %v, want partition refusal", err)
			}
		})
	}

	resolver := newTestTrustedCityPartitionResolver()
	resolver.defaultPartition = TrustedCityPartition{}
	reader, err := NewCommandPartitionReader(repo, partition, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	command := repositoryCommandForRequest(t, state.Store, "request-unknown", "unknown")
	if _, _, err := repo.Create(t.Context(), "request-unknown", command); err != nil {
		t.Fatalf("Create unknown-authority command: %v", err)
	}
	if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("Snapshot unknown authority error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderRejectsNewerOpaqueAuthoritySchema(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repo := newVerifiedCommandRepository(t, store)
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	commandID := CommandIDForRequest(state.Store, "future-request")
	raw := []byte(fmt.Sprintf(`{"version":2,"id":%q,"target":{"session_id":"session-future","intent_generation":1},"store":{"store_uuid":%q,"restore_epoch":%d},"order":{"sequence":1,"revision":1},"trusted_ingress":{"city_scope":"caller-city-a"}}`, commandID, state.Store.StoreUUID, state.Store.RestoreEpoch))
	store.seedRawCommand(t, state, "future-request", commandID, raw, 1, 1)
	if _, err := repo.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage: %v", err)
	}
	reader, err := NewCommandPartitionReader(repo, trustedCityPartitionForTest("authority/city-a"), newTestTrustedCityPartitionResolver())
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("Snapshot newer authority schema error = %v, want partition refusal", err)
	}
	if _, err := reader.Get(t.Context(), commandID); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("Get newer authority schema error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderRestoreRewindFailsBeforePartitionResolution(t *testing.T) {
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
	partition := trustedCityPartitionForTest("authority/city-a")
	resolver := newTestTrustedCityPartitionResolver()
	createPartitionedCommandForTest(t, repo, state.Store, resolver, partition, "request-before-rewind", "session-a", "caller-city-a")

	store.mu.Lock()
	retainedRows := cloneRepositoryRows(store.rows)
	retainedMetadata := cloneRepositoryMetadata(store.metadata)
	store.mu.Unlock()
	createPartitionedCommandForTest(t, repo, state.Store, resolver, partition, "request-after-rewind", "session-a", "caller-city-a")
	reader, err := NewCommandPartitionReader(repo, partition, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	resolver.resetCallCount()

	store.mu.Lock()
	store.rows = retainedRows
	store.metadata = retainedMetadata
	store.mu.Unlock()
	if _, err := reader.Snapshot(t.Context(), 2); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("Snapshot after rewind error = %v, want lineage refusal", err)
	}
	if got := resolver.callCount(); got != 0 {
		t.Fatalf("partition resolver calls after lineage rewind = %d, want zero", got)
	}
}

func TestCommandPartitionReaderForeignStoreOrEpochFailsBeforePartitionResolution(t *testing.T) {
	for name, mutate := range map[string]func(map[string]string){
		"foreign store UUID": func(metadata map[string]string) {
			metadata[commandRepositoryStoreUUIDMetadataKey] = uuid.NewString()
		},
		"foreign restore epoch": func(metadata map[string]string) {
			metadata[commandRepositoryRestoreEpochMetadataKey] = "2"
		},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repo := newVerifiedCommandRepository(t, store)
			state, err := repo.State(t.Context())
			if err != nil {
				t.Fatalf("State: %v", err)
			}
			partition := trustedCityPartitionForTest("authority/city-a")
			resolver := newTestTrustedCityPartitionResolver()
			createPartitionedCommandForTest(t, repo, state.Store, resolver, partition, "request-a", "session-a", "caller-city-a")
			reader, err := NewCommandPartitionReader(repo, partition, resolver)
			if err != nil {
				t.Fatalf("NewCommandPartitionReader: %v", err)
			}
			resolver.resetCallCount()

			store.mu.Lock()
			mutate(store.metadata)
			store.mu.Unlock()
			if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryLineage) {
				t.Fatalf("Snapshot under foreign lineage error = %v, want lineage refusal", err)
			}
			if got := resolver.callCount(); got != 0 {
				t.Fatalf("partition resolver calls under foreign lineage = %d, want zero", got)
			}
		})
	}
}

func TestCommandPartitionReaderConcurrentSharedStoreReadsNeverCross(t *testing.T) {
	repo := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repo.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	cityA := trustedCityPartitionForTest("authority/city-a")
	cityB := trustedCityPartitionForTest("authority/city-b")
	resolver := newTestTrustedCityPartitionResolver()
	commandA := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityA, "request-a", "session-a", "caller-city-a")
	commandB := createPartitionedCommandForTest(t, repo, state.Store, resolver, cityB, "request-b", "session-b", "caller-city-b")
	readerA, err := NewCommandPartitionReader(repo, cityA, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city A): %v", err)
	}
	readerB, err := NewCommandPartitionReader(repo, cityB, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(city B): %v", err)
	}

	var wg sync.WaitGroup
	errs := make(chan error, 32)
	for i := 0; i < 16; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			snapshot, err := readerA.Snapshot(t.Context(), 2)
			if err != nil || len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.ID != commandA.ID {
				errs <- fmt.Errorf("city A snapshot=%#v err=%w", snapshot, err)
			}
		}()
		go func() {
			defer wg.Done()
			snapshot, err := readerB.Snapshot(t.Context(), 2)
			if err != nil || len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.ID != commandB.ID {
				errs <- fmt.Errorf("city B snapshot=%#v err=%w", snapshot, err)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestTrustedCityPartitionCapabilityHasNoCallerPopulatableFields(t *testing.T) {
	typ := reflect.TypeOf(TrustedCityPartition{})
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).IsExported() {
			t.Fatalf("TrustedCityPartition field %q is exported", typ.Field(i).Name)
		}
	}
	if typ.ConvertibleTo(reflect.TypeOf("")) || reflect.TypeOf(TrustedIngressReference{}).ConvertibleTo(typ) {
		t.Fatal("caller-authored command data can convert directly to TrustedCityPartition")
	}
	readerType := reflect.TypeOf(CommandPartitionReader{})
	for i := 0; i < readerType.NumField(); i++ {
		if readerType.Field(i).Type == reflect.TypeOf((*CommandRepository)(nil)) {
			t.Fatal("CommandPartitionReader retains a writable CommandRepository capability")
		}
	}
}

type testTrustedCityPartitionResolver struct {
	mu               sync.Mutex
	authorized       map[string]testTrustedCityPartitionAuthorization
	defaultPartition TrustedCityPartition
	calls            int
}

type testTrustedCityPartitionAuthorization struct {
	reference TrustedIngressReference
	partition TrustedCityPartition
}

func newTestTrustedCityPartitionResolver() *testTrustedCityPartitionResolver {
	return &testTrustedCityPartitionResolver{authorized: make(map[string]testTrustedCityPartitionAuthorization)}
}

func (r *testTrustedCityPartitionResolver) ResolveCommandPartition(ctx context.Context, reference TrustedIngressReference) (TrustedCityPartition, error) {
	if err := ctx.Err(); err != nil {
		return TrustedCityPartition{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls++
	authorization, ok := r.authorized[reference.ReferenceID]
	if !ok {
		return r.defaultPartition, nil
	}
	if authorization.reference != reference {
		return TrustedCityPartition{}, errors.New("trusted ingress reference does not match authority")
	}
	return authorization.partition, nil
}

func (r *testTrustedCityPartitionResolver) authorize(reference TrustedIngressReference, partition TrustedCityPartition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.authorized[reference.ReferenceID] = testTrustedCityPartitionAuthorization{reference: reference, partition: partition}
}

func (r *testTrustedCityPartitionResolver) resetCallCount() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = 0
}

func (r *testTrustedCityPartitionResolver) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

func trustedCityPartitionForTest(identity string) TrustedCityPartition {
	return TrustedCityPartition{identity: sha256.Sum256([]byte(identity))}
}

func createPartitionedCommandForTest(t *testing.T, repo *CommandRepository, binding CommandStoreBinding, resolver *testTrustedCityPartitionResolver, partition TrustedCityPartition, requestID, sessionID, callerCityScope string) Command {
	t.Helper()
	command := repositoryCommandForRequest(t, binding, requestID, requestID)
	command.Target.SessionID = sessionID
	command.TrustedIngress.TargetSessionID = sessionID
	command.TrustedIngress.CityScope = callerCityScope
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	resolver.authorize(command.TrustedIngress, partition)
	entry, created, err := repo.Create(t.Context(), requestID, command)
	if err != nil || !created || entry.Command == nil {
		t.Fatalf("Create(%s) = %#v, created=%t err=%v", requestID, entry, created, err)
	}
	return *entry.Command
}

func assertPartitionSnapshot(t *testing.T, snapshot CommandIndexSnapshot, wantIDs []string, wantGaps []uint64) {
	t.Helper()
	gotIDs := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		if entry.Command == nil {
			t.Fatalf("partition snapshot exposed non-v1 entry: %#v", entry)
		}
		gotIDs = append(gotIDs, entry.Command.ID)
	}
	gotGaps := make([]uint64, 0, len(snapshot.PartitionGaps))
	for _, gap := range snapshot.PartitionGaps {
		gotGaps = append(gotGaps, gap.Sequence)
	}
	if !reflect.DeepEqual(gotIDs, wantIDs) || !reflect.DeepEqual(gotGaps, wantGaps) {
		t.Fatalf("partition snapshot IDs/gaps = %v/%v, want %v/%v", gotIDs, gotGaps, wantIDs, wantGaps)
	}
}

var _ TrustedCityPartitionResolver = (*testTrustedCityPartitionResolver)(nil)
