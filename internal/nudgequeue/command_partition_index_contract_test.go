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

type partitionIndexContractTx struct {
	beads.AtomicReadSnapshotTx
	store *partitionIndexContractStore
}

func (tx *partitionIndexContractTx) ListHistoryPage(query beads.AtomicReadSnapshotPageQuery) (beads.AtomicReadSnapshotPage, error) {
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

func (r *countingPartitionResolverForContractTest) ResolveCommandPartition(ctx context.Context, reference TrustedIngressReference) (TrustedCityPartition, error) {
	r.mu.Lock()
	r.calls++
	r.mu.Unlock()
	return r.delegate.ResolveCommandPartition(ctx, reference)
}

func (r *countingPartitionResolverForContractTest) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

var _ beads.AtomicReadSnapshotStore = (*partitionIndexContractStore)(nil)
var _ beads.AtomicReadSnapshotTx = (*partitionIndexContractTx)(nil)
var _ TrustedCityPartitionResolver = (*countingPartitionResolverForContractTest)(nil)
