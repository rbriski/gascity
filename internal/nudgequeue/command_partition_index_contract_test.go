package nudgequeue

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	commandPartitionRouteSchemaForContractTest      = "1"
	commandPartitionRouteMetadataKeyForContractTest = "gc.control.command_partition_key"
	commandPartitionRouteVersionKeyForContractTest  = "gc.control.command_partition_schema_version"
	commandPartitionRepositoryFenceForContractTest  = "gc.control.repository.command_partition_schema_version"
)

func TestTrustedNudgeIngressPersistsAuthorityDerivedPartitionRoute(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for _, principalSchema := range []uint32{NudgePrincipalSchemaVersion, NudgePrincipalSchemaVersion - 1} {
		t.Run(fmt.Sprintf("principal-schema-%d", principalSchema), func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repository := newVerifiedCommandRepository(t, store)
			authority := newTestNudgeAuthority()
			authority.schema = principalSchema
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}

			admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
			if err != nil || admitted.Entry.Command == nil {
				t.Fatalf("Admit = %#v, err=%v", admitted, err)
			}
			row, err := store.Get(admitted.Entry.Command.ID)
			if err != nil {
				t.Fatalf("Get admitted row: %v", err)
			}
			wantRoute := commandPartitionRouteForContractTest(admitted.Partition)
			if row.Assignee != wantRoute {
				t.Fatalf("command Assignee projection = %q, want authority-derived route %q", row.Assignee, wantRoute)
			}
			if got := row.Metadata[commandPartitionRouteMetadataKeyForContractTest]; got != wantRoute {
				t.Fatalf("command partition metadata = %q, want %q", got, wantRoute)
			}
			if got := row.Metadata[commandPartitionRouteVersionKeyForContractTest]; got != commandPartitionRouteSchemaForContractTest {
				t.Fatalf("command partition schema = %q, want %q", got, commandPartitionRouteSchemaForContractTest)
			}
			if row.Assignee == admitted.Entry.Command.TrustedIngress.CityScope {
				t.Fatal("caller-visible city scope was persisted as the indexed partition authority")
			}
		})
	}
}

func TestCommandPartitionReaderPushesRouteBelowGlobalActiveBound(t *testing.T) {
	const logicalForeignActive = 100_001
	if logicalForeignActive <= MaxCommandRepositorySnapshotCommands {
		t.Fatalf("test fixture foreign count %d must exceed global snapshot bound %d", logicalForeignActive, MaxCommandRepositorySnapshotCommands)
	}

	store := &partitionIndexContractStore{
		repositoryAtomicTestStore: newRepositoryAtomicTestStore(),
		logicalForeignActive:      logicalForeignActive,
	}
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	stampCommandPartitionRouteForContractTest(store.repositoryAtomicTestStore, admitted.Entry.Command.ID, admitted.Partition)

	resolver := &countingPartitionResolverForContractTest{delegate: ingress}
	reader, err := NewCommandPartitionReader(repository, admitted.Partition, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	snapshot, err := reader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot with %d foreign active commands: %v", logicalForeignActive, err)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.ID != admitted.Entry.Command.ID {
		t.Fatalf("owned snapshot = %#v, want exact admitted command", snapshot)
	}

	unscoped, scoped, materialized := store.partitionReadStats()
	if unscoped != 0 {
		t.Fatalf("unscoped active page queries = %d, want zero with %d foreign rows", unscoped, logicalForeignActive)
	}
	if scoped == 0 || scoped > 2 {
		t.Fatalf("scoped active page queries = %d, want one bounded page plus at most one exhaustion probe", scoped)
	}
	if materialized != 1 {
		t.Fatalf("active command rows materialized = %d, want one owned row", materialized)
	}
	if calls := resolver.callCount(); calls != 1 {
		t.Fatalf("partition authority resolutions = %d, want one owned command and no foreign commands", calls)
	}
}

func TestCommandPartitionReaderDoesNotReadGlobalTerminalHistory(t *testing.T) {
	store := &partitionIndexContractStore{
		repositoryAtomicTestStore: newRepositoryAtomicTestStore(),
	}
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 45, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}

	reader, err := NewCommandPartitionReader(repository, admitted.Partition, ingress)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	snapshot, err := reader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if checkpointReads, terminalPages := store.globalTerminalHistoryReadStats(); checkpointReads != 0 || terminalPages != 0 {
		t.Fatalf("city-local snapshot global terminal reads = checkpoint:%d pages:%d, want zero", checkpointReads, terminalPages)
	}
	if snapshot.Coverage != nil || len(snapshot.PartitionGaps) != 0 {
		t.Fatalf("city-local snapshot copied global dense coverage: coverage=%#v gaps=%#v", snapshot.Coverage, snapshot.PartitionGaps)
	}
	if _, err := BuildCommandIndex(snapshot); err != nil {
		t.Fatalf("BuildCommandIndex(trusted sparse partition snapshot): %v", err)
	}
	tampered := snapshot
	tampered.SequenceHighWater++
	if _, err := BuildCommandIndex(tampered); err == nil {
		t.Fatal("BuildCommandIndex accepted a sparse partition snapshot changed after authority verification")
	}
}

func TestCommandPartitionReaderRejectsStoreForgedPartitionProjection(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}

	forgedPartition := trustedCityPartitionForTest("store-credential-forged-city")
	if forgedPartition == admitted.Partition {
		t.Fatal("forged partition unexpectedly equals authority partition")
	}
	stampCommandPartitionRouteForContractTest(store, admitted.Entry.Command.ID, forgedPartition)
	reader, err := NewCommandPartitionReader(repository, forgedPartition, ingress)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(forged): %v", err)
	}
	if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("Snapshot through store-forged projection error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderRejectsStoreCredentialHidingOwnedCommand(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	reader, err := NewCommandPartitionReader(repository, admitted.Partition, ingress)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader(original): %v", err)
	}

	forgedPartition := trustedCityPartitionForTest("store-credential-hidden-city")
	if forgedPartition == admitted.Partition {
		t.Fatal("forged partition unexpectedly equals authority partition")
	}
	stampCommandPartitionRouteForContractTest(store, admitted.Entry.Command.ID, forgedPartition)

	if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("original partition Snapshot after route hiding error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderRejectsUnavailableOrStaleTrustedCoverage(t *testing.T) {
	for name, testCase := range map[string]struct {
		coverageErr error
		mutate      func(*CommandPartitionCoverage)
	}{
		"unavailable": {coverageErr: errors.New("authority unavailable")},
		"stale revision": {mutate: func(coverage *CommandPartitionCoverage) {
			coverage.RepositoryRevision--
		}},
		"incomplete admission history": {mutate: func(coverage *CommandPartitionCoverage) {
			coverage.DecidedCount--
		}},
		"extra command": {mutate: func(coverage *CommandPartitionCoverage) {
			coverage.ActiveEntries = append(coverage.ActiveEntries, CommandPartitionCoverageEntry{
				CommandID: "gc-nudge-authority-extra", Sequence: coverage.ActiveEntries[0].Sequence,
			})
		}},
	} {
		t.Run(name, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repository := newVerifiedCommandRepository(t, store)
			authority := newTestNudgeAuthority()
			now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
			if err != nil || admitted.Entry.Command == nil {
				t.Fatalf("Admit = %#v, err=%v", admitted, err)
			}
			resolver := &partitionCoverageOverrideResolver{delegate: ingress, coverageErr: testCase.coverageErr, mutate: testCase.mutate}
			reader, err := NewCommandPartitionReader(repository, admitted.Partition, resolver)
			if err != nil {
				t.Fatalf("NewCommandPartitionReader: %v", err)
			}
			if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositoryPartition) {
				t.Fatalf("Snapshot error = %v, want partition refusal", err)
			}
		})
	}
}

func TestCommandPartitionReaderExactReadRejectsUnpublishedTerminalTransition(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	reader, err := NewCommandPartitionReader(fixture.repository, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	fixture.authority.setClaimDisposition(NudgeAuthorizationDenied)
	result, err := fixture.repository.ClaimAuthorized(
		t.Context(),
		fixture.claimRequest("claim-unpublished-terminal", "owner-unpublished-terminal", "attempt-unpublished-terminal", fixture.now.Add(time.Second)),
		fixture.authority,
		fixture.authority,
	)
	if err != nil || result.Disposition != CommandClaimDenied || result.Command.Terminal == nil {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
	}

	if _, err := reader.Get(t.Context(), fixture.command.ID); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("Get after unpublished terminal error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderUsesHistoricalCoverageForExactSnapshotRevision(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	first, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || first.Entry.Command == nil {
		t.Fatalf("first Admit = %#v, err=%v", first, err)
	}

	var once sync.Once
	var concurrentErr error
	resolver := &partitionCoverageOverrideResolver{
		delegate: ingress,
		before: func() {
			once.Do(func() {
				request := validNudgeIngressRequest(now)
				request.RequestID = "request-concurrent-after-snapshot"
				_, concurrentErr = ingress.Admit(t.Context(), request)
			})
		},
	}
	reader, err := NewCommandPartitionReader(repository, first.Partition, resolver)
	if err != nil {
		t.Fatalf("NewCommandPartitionReader: %v", err)
	}
	snapshot, err := reader.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot at historical revision: %v", err)
	}
	if concurrentErr != nil {
		t.Fatalf("concurrent Admit: %v", concurrentErr)
	}
	if len(snapshot.Entries) != 1 || snapshot.Entries[0].Command == nil || snapshot.Entries[0].Command.ID != first.Entry.Command.ID || snapshot.Revision != 1 {
		t.Fatalf("historical snapshot = %#v, want only revision-1 command", snapshot)
	}
	state, err := repository.State(t.Context())
	if err != nil || state.SequenceHighWater != 2 {
		t.Fatalf("repository after concurrent admission = %#v, err=%v", state, err)
	}
}

func TestTrustedNudgeIngressIdempotentRetryRejectsPartitionProjectionTamper(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)
	admitted, err := ingress.Admit(t.Context(), request)
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	stampCommandPartitionRouteForContractTest(store, admitted.Entry.Command.ID, trustedCityPartitionForTest("tampered-route"))

	if _, err := ingress.Admit(t.Context(), request); !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("idempotent Admit after projection tamper error = %v, want partition refusal", err)
	}
}

func TestCommandPartitionReaderFailsClosedOnMissingOrFuturePartitionSchema(t *testing.T) {
	for name, partitionSchema := range map[string]string{
		"missing": "",
		"future":  "2",
	} {
		t.Run(name, func(t *testing.T) {
			store := newRepositoryAtomicTestStore()
			repository := newVerifiedCommandRepository(t, store)
			authority := newTestNudgeAuthority()
			now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
			if err != nil || admitted.Entry.Command == nil {
				t.Fatalf("Admit = %#v, err=%v", admitted, err)
			}
			stampCommandPartitionRouteForContractTest(store, admitted.Entry.Command.ID, admitted.Partition)
			store.mu.Lock()
			if partitionSchema == "" {
				delete(store.metadata, commandPartitionRepositoryFenceForContractTest)
			} else {
				store.metadata[commandPartitionRepositoryFenceForContractTest] = partitionSchema
			}
			store.mu.Unlock()

			reader, err := NewCommandPartitionReader(repository, admitted.Partition, ingress)
			if err != nil {
				t.Fatalf("NewCommandPartitionReader: %v", err)
			}
			if _, err := reader.Snapshot(t.Context(), 1); !errors.Is(err, ErrCommandRepositorySchemaSkew) {
				t.Fatalf("Snapshot with %s partition schema error = %v, want repository schema refusal", name, err)
			}
			if queries := store.snapshotPageQueriesForTest(); len(queries) != 0 {
				t.Fatalf("Snapshot with %s partition schema issued row queries: %#v", name, queries)
			}
		})
	}
}

func TestCommandRepositoryRefusesLegacyWriterAfterPartitionFence(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	state := CommandRepositoryState{
		Store:             CommandStoreBinding{StoreUUID: "4d0d56a4-11cc-4b8f-9b8d-f8ec88ae9845", RestoreEpoch: 1},
		SchemaVersion:     CommandRepositorySchemaVersion,
		WriterVersion:     CommandRepositoryWriterVersion - 1,
		Revision:          0,
		SequenceHighWater: 0,
	}
	store.seedMetadata(state)
	verifier := &repositoryLineageTestVerifier{anchor: &state}
	repository, err := NewCommandRepository(store, verifier)
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	requestID := "legacy-writer-refusal"
	command := repositoryCommandForRequest(t, state.Store, requestID, requestID)
	partition := trustedCityPartitionFromAuthority(command.TrustedIngress)
	if _, _, err := repository.create(t.Context(), requestID, command, partition); !errors.Is(err, ErrCommandRepositorySchemaSkew) {
		t.Fatalf("create against legacy writer metadata error = %v, want schema skew", err)
	}
	if createCalls, metadataCalls := store.durableMutationCallCounts(); createCalls != 0 || metadataCalls != 0 {
		t.Fatalf("legacy writer refusal mutated repository: Create=%d SetMetadata=%d", createCalls, metadataCalls)
	}
}

func commandPartitionRouteForContractTest(partition TrustedCityPartition) string {
	return "gc:control-partition:v1:" + hex.EncodeToString(partition.identity[:])
}

func stampCommandPartitionRouteForContractTest(store *repositoryAtomicTestStore, commandID string, partition TrustedCityPartition) {
	route := commandPartitionRouteForContractTest(partition)
	store.mu.Lock()
	defer store.mu.Unlock()
	row := store.rows[commandID]
	row.Metadata = cloneRepositoryMetadata(row.Metadata)
	row.Assignee = route
	row.Metadata[commandPartitionRouteMetadataKeyForContractTest] = route
	row.Metadata[commandPartitionRouteVersionKeyForContractTest] = commandPartitionRouteSchemaForContractTest
	store.rows[commandID] = row
	store.metadata[commandPartitionRepositoryFenceForContractTest] = commandPartitionRouteSchemaForContractTest
}

var errPartitionIndexContractGlobalScan = errors.New("partition index contract attempted an unscoped active scan")

type partitionIndexContractStore struct {
	*repositoryAtomicTestStore

	statsMu              sync.Mutex
	logicalForeignActive int
	unscopedActivePages  int
	scopedActivePages    int
	materializedRows     int
	checkpointReads      int
	terminalPages        int
}

func (s *partitionIndexContractStore) AtomicReadSnapshot(ctx context.Context, fn func(beads.AtomicReadSnapshotTx) error) error {
	return s.repositoryAtomicTestStore.AtomicReadSnapshot(ctx, func(tx beads.AtomicReadSnapshotTx) error {
		return fn(&partitionIndexContractTx{AtomicReadSnapshotTx: tx, store: s})
	})
}

func (s *partitionIndexContractStore) partitionReadStats() (unscoped, scoped, materialized int) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.unscopedActivePages, s.scopedActivePages, s.materializedRows
}

func (s *partitionIndexContractStore) globalTerminalHistoryReadStats() (checkpointReads, terminalPages int) {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	return s.checkpointReads, s.terminalPages
}

type partitionIndexContractTx struct {
	beads.AtomicReadSnapshotTx
	store *partitionIndexContractStore
}

func (tx *partitionIndexContractTx) GetIssue(id string) (beads.Bead, error) {
	if id == commandRepositoryCheckpointID {
		tx.store.statsMu.Lock()
		tx.store.checkpointReads++
		tx.store.statsMu.Unlock()
	}
	return tx.AtomicReadSnapshotTx.GetIssue(id)
}

func (tx *partitionIndexContractTx) ListHistoryPage(query beads.AtomicReadSnapshotPageQuery) (beads.AtomicReadSnapshotPage, error) {
	if query.Status == "closed" {
		tx.store.statsMu.Lock()
		tx.store.terminalPages++
		tx.store.statsMu.Unlock()
	}
	if query.Status != "open" {
		return tx.AtomicReadSnapshotTx.ListHistoryPage(query)
	}
	assignee, scoped := atomicSnapshotAssigneeForContractTest(query)
	if !scoped || assignee == "" {
		tx.store.statsMu.Lock()
		tx.store.unscopedActivePages++
		foreign := tx.store.logicalForeignActive
		tx.store.statsMu.Unlock()
		return beads.AtomicReadSnapshotPage{}, fmt.Errorf("%w: %d foreign active commands", errPartitionIndexContractGlobalScan, foreign)
	}

	tx.store.statsMu.Lock()
	tx.store.scopedActivePages++
	tx.store.statsMu.Unlock()
	page, err := tx.AtomicReadSnapshotTx.ListHistoryPage(query)
	if err != nil {
		return beads.AtomicReadSnapshotPage{}, err
	}
	tx.store.statsMu.Lock()
	tx.store.materializedRows += len(page.Rows)
	tx.store.statsMu.Unlock()
	return page, nil
}

func atomicSnapshotAssigneeForContractTest(query beads.AtomicReadSnapshotPageQuery) (string, bool) {
	field := reflect.ValueOf(query).FieldByName("Assignee")
	if !field.IsValid() || field.Kind() != reflect.String {
		return "", false
	}
	return field.String(), true
}

type countingPartitionResolverForContractTest struct {
	delegate TrustedCityPartitionResolver
	mu       sync.Mutex
	calls    int
}

type partitionCoverageOverrideResolver struct {
	delegate    *TrustedNudgeIngress
	before      func()
	coverageErr error
	mutate      func(*CommandPartitionCoverage)
}

func (r *partitionCoverageOverrideResolver) ResolveCommandPartition(ctx context.Context, reference TrustedIngressReference) (TrustedCityPartition, error) {
	return r.delegate.ResolveCommandPartition(ctx, reference)
}

func (r *partitionCoverageOverrideResolver) ResolveCommandPartitionCoverage(ctx context.Context, request CommandPartitionCoverageRequest) (CommandPartitionCoverage, error) {
	if r.before != nil {
		r.before()
	}
	if r.coverageErr != nil {
		return CommandPartitionCoverage{}, r.coverageErr
	}
	coverage, err := r.delegate.ResolveCommandPartitionCoverage(ctx, request)
	if err != nil {
		return CommandPartitionCoverage{}, err
	}
	if r.mutate != nil {
		r.mutate(&coverage)
	}
	return coverage, nil
}

func (r *partitionCoverageOverrideResolver) ResolveCommandPartitionMembership(ctx context.Context, request CommandPartitionMembershipRequest) (CommandPartitionMembership, error) {
	return r.delegate.ResolveCommandPartitionMembership(ctx, request)
}

func (r *countingPartitionResolverForContractTest) ResolveCommandPartition(ctx context.Context, reference TrustedIngressReference) (TrustedCityPartition, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.delegate.ResolveCommandPartition(ctx, reference)
}

func (r *countingPartitionResolverForContractTest) ResolveCommandPartitionCoverage(ctx context.Context, request CommandPartitionCoverageRequest) (CommandPartitionCoverage, error) {
	resolver, ok := r.delegate.(TrustedCommandPartitionCoverageResolver)
	if !ok {
		return CommandPartitionCoverage{}, errors.New("delegate has no trusted partition coverage")
	}
	return resolver.ResolveCommandPartitionCoverage(ctx, request)
}

func (r *countingPartitionResolverForContractTest) ResolveCommandPartitionMembership(ctx context.Context, request CommandPartitionMembershipRequest) (CommandPartitionMembership, error) {
	resolver, ok := r.delegate.(TrustedCommandPartitionCoverageResolver)
	if !ok {
		return CommandPartitionMembership{}, errors.New("delegate has no trusted partition membership")
	}
	return resolver.ResolveCommandPartitionMembership(ctx, request)
}

func (r *countingPartitionResolverForContractTest) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

var (
	_ beads.AtomicReadSnapshotStore           = (*partitionIndexContractStore)(nil)
	_ beads.AtomicReadSnapshotTx              = (*partitionIndexContractTx)(nil)
	_ TrustedCityPartitionResolver            = (*countingPartitionResolverForContractTest)(nil)
	_ TrustedCommandPartitionCoverageResolver = (*countingPartitionResolverForContractTest)(nil)
	_ TrustedCityPartitionResolver            = (*partitionCoverageOverrideResolver)(nil)
	_ TrustedCommandPartitionCoverageResolver = (*partitionCoverageOverrideResolver)(nil)
)
