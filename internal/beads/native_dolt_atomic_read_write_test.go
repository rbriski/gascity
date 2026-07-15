package beads

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltAtomicReadWritePreservesExplicitCommandIDOutsideProjectPrefix(t *testing.T) {
	t.Parallel()

	storage := newAtomicNativeDoltStorageForTest()
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "atomic-test", "project-prefix")
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}
	wantID := "gc-nudge-0123456789abcdef"
	if err := capability.AtomicReadWrite(t.Context(), "create explicit command id", func(tx AtomicReadWriteTx) error {
		created, err := tx.Create(Bead{
			ID: wantID, Title: "durable control command", Type: "task",
			Metadata: map[string]string{"gc.control.record_kind": "command"},
		})
		if err != nil {
			return err
		}
		if created.ID != wantID {
			t.Fatalf("created ID = %q, want explicit %q", created.ID, wantID)
		}
		listed, err := tx.ListHistory(AtomicReadWriteList{IDPrefix: "gc-nudge-", Limit: 2})
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0].ID != wantID {
			t.Fatalf("ListHistory = %#v, want explicit command", listed)
		}
		return nil
	}); err != nil {
		t.Fatalf("AtomicReadWrite: %v", err)
	}
}

func TestNativeDoltAtomicReadWriteConcurrentDifferentCommandsAllocateDenseMetadata(t *testing.T) {
	t.Parallel()

	storage := newAtomicNativeDoltStorageForTest()
	store := newNativeDoltStoreForTest(storage)
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}
	const count = 32
	errCh := make(chan error, count)
	var wg sync.WaitGroup
	for i := 0; i < count; i++ {
		id := fmt.Sprintf("gc-nudge-concurrent-%02d", i)
		wg.Add(1)
		go func() {
			defer wg.Done()
			errCh <- capability.AtomicReadWrite(context.Background(), "allocate command order", func(tx AtomicReadWriteTx) error {
				value, err := tx.GetMetadata("command_revision")
				if err != nil {
					return err
				}
				var revision uint64
				if value != "" {
					revision, err = strconv.ParseUint(value, 10, 64)
					if err != nil {
						return err
					}
				}
				revision++
				if _, err := tx.Create(Bead{
					ID: id, Title: "durable command", Type: "task",
					Metadata: map[string]string{"revision": strconv.FormatUint(revision, 10)},
				}); err != nil {
					return err
				}
				return tx.SetMetadata("command_revision", strconv.FormatUint(revision, 10))
			})
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Errorf("concurrent AtomicReadWrite: %v", err)
		}
	}
	if t.Failed() {
		return
	}
	if err := capability.AtomicReadWrite(t.Context(), "verify dense allocation", func(tx AtomicReadWriteTx) error {
		value, err := tx.GetMetadata("command_revision")
		if err != nil {
			return err
		}
		if value != strconv.Itoa(count) {
			t.Fatalf("command_revision = %q, want %d", value, count)
		}
		rows, err := tx.ListHistory(AtomicReadWriteList{IDPrefix: "gc-nudge-concurrent-", Limit: count})
		if err != nil {
			return err
		}
		revisions := make([]int, 0, len(rows))
		for _, row := range rows {
			revision, err := strconv.Atoi(row.Metadata["revision"])
			if err != nil {
				return err
			}
			revisions = append(revisions, revision)
		}
		sort.Ints(revisions)
		for i := 0; i < count; i++ {
			if revisions[i] != i+1 {
				t.Fatalf("dense revisions = %v", revisions)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("verifying dense allocation: %v", err)
	}
}

func TestNativeDoltAtomicReadWriteListHistoryUsesBoundedFilteredTransactionSearch(t *testing.T) {
	t.Parallel()

	commandType := beadslib.IssueType("task")
	wantFilter := beadslib.IssueFilter{
		IssueType:      &commandType,
		MetadataFields: map[string]string{"gc.control.record_kind": "command"},
		Limit:          7,
		SkipWisps:      true,
		SortBy:         "id",
	}
	persistent := false
	wantFilter.Ephemeral = &persistent
	wantIssue, err := nativeIssueFromBead(Bead{
		ID:       "gc-command-1",
		Title:    "command",
		Status:   "open",
		Type:     "task",
		Metadata: map[string]string{"gc.control.record_kind": "command"},
	})
	if err != nil {
		t.Fatalf("nativeIssueFromBead: %v", err)
	}
	storage := &nativeDoltStorageSpy{}
	storage.runInTransaction = func(ctx context.Context, _ string, fn func(beadslib.Transaction) error) error {
		return fn(atomicSearchTransactionForTest{
			search: func(gotCtx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
				if gotCtx != ctx {
					t.Fatal("SearchIssues received a different transaction context")
				}
				if query != "" {
					t.Fatalf("SearchIssues query = %q, want empty structured query", query)
				}
				if !reflect.DeepEqual(filter, wantFilter) {
					t.Fatalf("SearchIssues filter = %#v, want %#v", filter, wantFilter)
				}
				return []*beadslib.Issue{wantIssue}, nil
			},
		})
	}
	store := newNativeDoltStoreForTest(storage)
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}

	err = capability.AtomicReadWrite(t.Context(), "read command snapshot", func(tx AtomicReadWriteTx) error {
		got, err := tx.ListHistory(AtomicReadWriteList{
			IssueType: "task",
			Metadata:  map[string]string{"gc.control.record_kind": "command"},
			Limit:     7,
		})
		if err != nil {
			return err
		}
		if len(got) != 1 || got[0].ID != "gc-command-1" {
			t.Fatalf("ListHistory = %#v, want gc-command-1", got)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("AtomicReadWrite: %v", err)
	}
}

func TestNativeDoltAtomicReadWriteListHistoryRejectsScansAndUnboundedLimits(t *testing.T) {
	t.Parallel()

	store := newNativeDoltStoreForTest(newAtomicNativeDoltStorageForTest())
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}
	tests := map[string]AtomicReadWriteList{
		"empty scan":           {Limit: 1},
		"type-only scan":       {IssueType: "task", Limit: 1},
		"metadata-only scan":   {Metadata: map[string]string{"kind": "command"}, Limit: 1},
		"zero limit":           {IssueType: "task", Metadata: map[string]string{"kind": "command"}},
		"negative limit":       {IDs: []string{"gc-command-1"}, Limit: -1},
		"limit above maximum":  {IDs: []string{"gc-command-1"}, Limit: MaxAtomicReadWriteListLimit + 1},
		"empty id prefix":      {IDPrefix: " ", Limit: 1},
		"empty metadata key":   {IssueType: "task", Metadata: map[string]string{"": "command"}, Limit: 1},
		"empty metadata value": {IssueType: "task", Metadata: map[string]string{"kind": ""}, Limit: 1},
	}
	for name, query := range tests {
		query := query
		t.Run(name, func(t *testing.T) {
			err := capability.AtomicReadWrite(t.Context(), "reject unsafe history query", func(tx AtomicReadWriteTx) error {
				_, err := tx.ListHistory(query)
				return err
			})
			if !errors.Is(err, ErrAtomicReadWriteQuery) {
				t.Fatalf("ListHistory error = %v, want ErrAtomicReadWriteQuery", err)
			}
		})
	}
}

func TestNativeDoltAtomicReadWriteListHistoryRejectsNonHistoryAndMismatchedRows(t *testing.T) {
	t.Parallel()

	tests := map[string]Bead{
		"ephemeral": {
			ID: "gc-command-ephemeral", Title: "command", Status: "open", Type: "task",
			Metadata: map[string]string{"kind": "command"}, Ephemeral: true,
		},
		"no history": {
			ID: "gc-command-no-history", Title: "command", Status: "open", Type: "task",
			Metadata: map[string]string{"kind": "command"}, NoHistory: true,
		},
		"wrong metadata": {
			ID: "gc-command-wrong-kind", Title: "command", Status: "open", Type: "task",
			Metadata: map[string]string{"kind": "other"},
		},
	}
	for name, bead := range tests {
		bead := bead
		t.Run(name, func(t *testing.T) {
			issue, err := nativeIssueFromBead(bead)
			if err != nil {
				t.Fatalf("nativeIssueFromBead: %v", err)
			}
			storage := &nativeDoltStorageSpy{}
			storage.runInTransaction = func(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
				return fn(atomicSearchTransactionForTest{search: func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error) {
					return []*beadslib.Issue{issue}, nil
				}})
			}
			store := newNativeDoltStoreForTest(storage)
			capability, ok := AtomicReadWriteFor(store)
			if !ok {
				t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
			}
			err = capability.AtomicReadWrite(t.Context(), "validate history rows", func(tx AtomicReadWriteTx) error {
				_, err := tx.ListHistory(AtomicReadWriteList{
					IssueType: "task", Metadata: map[string]string{"kind": "command"}, Limit: 1,
				})
				return err
			})
			if !errors.Is(err, ErrAtomicReadWriteQuery) {
				t.Fatalf("ListHistory error = %v, want ErrAtomicReadWriteQuery", err)
			}
		})
	}
}

type atomicSearchTransactionForTest struct {
	beadslib.Transaction
	search func(context.Context, string, beadslib.IssueFilter) ([]*beadslib.Issue, error)
}

func (tx atomicSearchTransactionForTest) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	return tx.search(ctx, query, filter)
}

func TestNativeDoltAtomicReadWriteReadsAndWritesHistoryStateInOneTransaction(t *testing.T) {
	t.Parallel()

	storage := newAtomicNativeDoltStorageForTest()
	store := newNativeDoltStoreForTest(storage)
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}

	commitsBefore := storage.commits
	var committedID string
	if err := capability.AtomicReadWrite(t.Context(), "create durable command", func(tx AtomicReadWriteTx) error {
		if got, err := tx.GetMetadata("command_revision"); err != nil || got != "" {
			t.Fatalf("initial GetMetadata = (%q, %v), want empty, nil", got, err)
		}
		if err := tx.SetMetadata("command_revision", "1"); err != nil {
			return err
		}
		created, err := tx.Create(Bead{
			ID:       "gc-command-1",
			Title:    "durable command",
			Metadata: map[string]string{"state": "pending", "revision": "1"},
		})
		if err != nil {
			return err
		}
		if created.Ephemeral || created.NoHistory {
			t.Fatalf("created command storage = ephemeral:%v no_history:%v, want history", created.Ephemeral, created.NoHistory)
		}
		committedID = created.ID
		readAfterCreate, err := tx.GetIssue(created.ID)
		if err != nil {
			return err
		}
		if readAfterCreate.Metadata["state"] != "pending" {
			t.Fatalf("read-after-create state = %q, want pending", readAfterCreate.Metadata["state"])
		}
		if err := tx.Update(created.ID, UpdateOpts{Metadata: map[string]string{"state": "claimed"}}); err != nil {
			return err
		}
		readAfterUpdate, err := tx.GetIssue(created.ID)
		if err != nil {
			return err
		}
		if readAfterUpdate.Metadata["state"] != "claimed" || readAfterUpdate.Metadata["revision"] != "1" {
			t.Fatalf("read-after-update metadata = %#v, want merged claimed state", readAfterUpdate.Metadata)
		}
		if got, err := tx.GetMetadata("command_revision"); err != nil || got != "1" {
			t.Fatalf("read-after-write metadata = (%q, %v), want 1, nil", got, err)
		}
		if _, exposesLocalMetadata := any(tx).(interface {
			SetLocalMetadata(string, string) error
		}); exposesLocalMetadata {
			t.Fatal("AtomicReadWriteTx exposes ignored LocalMetadata")
		}
		return nil
	}); err != nil {
		t.Fatalf("AtomicReadWrite: %v", err)
	}

	if got := storage.commits - commitsBefore; got != 1 {
		t.Fatalf("RunInTransaction calls = %d, want exactly 1", got)
	}
	got, err := store.Get(committedID)
	if err != nil {
		t.Fatalf("Get committed command: %v", err)
	}
	if got.Metadata["state"] != "claimed" || got.Metadata["revision"] != "1" {
		t.Fatalf("committed metadata = %#v, want claimed revision 1", got.Metadata)
	}
}

func TestNativeDoltAtomicReadWriteRollsBackRecordAndMetadataTogether(t *testing.T) {
	t.Parallel()

	storage := newAtomicNativeDoltStorageForTest()
	store := newNativeDoltStoreForTest(storage)
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}
	wantErr := errors.New("abort command commit")

	err := capability.AtomicReadWrite(t.Context(), "rollback durable command", func(tx AtomicReadWriteTx) error {
		if err := tx.SetMetadata("command_revision", "1"); err != nil {
			return err
		}
		if _, err := tx.Create(Bead{ID: "gc-command-rollback", Title: "rolled back"}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("AtomicReadWrite error = %v, want %v", err, wantErr)
	}
	if _, err := store.Get("gc-command-rollback"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get rolled-back command error = %v, want ErrNotFound", err)
	}
	if err := capability.AtomicReadWrite(t.Context(), "verify rollback", func(tx AtomicReadWriteTx) error {
		got, err := tx.GetMetadata("command_revision")
		if err != nil {
			return err
		}
		if got != "" {
			t.Fatalf("rolled-back metadata = %q, want empty", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("verifying rollback: %v", err)
	}
}

func TestNativeDoltAtomicReadWriteRejectsCanceledContextBeforeCallback(t *testing.T) {
	t.Parallel()

	store := newNativeDoltStoreForTest(newAtomicNativeDoltStorageForTest())
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	err := capability.AtomicReadWrite(ctx, "canceled command", func(AtomicReadWriteTx) error {
		called = true
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("AtomicReadWrite error = %v, want context.Canceled", err)
	}
	if called {
		t.Fatal("AtomicReadWrite called callback after cancellation")
	}
}

func TestNativeDoltAtomicReadWriteRejectsIgnoredRecordTiers(t *testing.T) {
	t.Parallel()

	store := newNativeDoltStoreForTest(newAtomicNativeDoltStorageForTest())
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(NativeDoltStore) = false, want true")
	}

	for _, bead := range []Bead{
		{ID: "gc-command-ephemeral", Title: "ephemeral", Ephemeral: true},
		{ID: "gc-command-no-history", Title: "no history", NoHistory: true},
	} {
		bead := bead
		t.Run(bead.ID, func(t *testing.T) {
			err := capability.AtomicReadWrite(t.Context(), "reject ignored tier", func(tx AtomicReadWriteTx) error {
				_, err := tx.Create(bead)
				return err
			})
			if !errors.Is(err, ErrAtomicReadWriteStorageClass) {
				t.Fatalf("Create error = %v, want ErrAtomicReadWriteStorageClass", err)
			}
		})
	}
}

type atomicNativeDoltStorageForTest struct {
	*nativeDoltMemStorage
	mu       sync.Mutex
	metadata map[string]string
	commits  int
}

func newAtomicNativeDoltStorageForTest() *atomicNativeDoltStorageForTest {
	return &atomicNativeDoltStorageForTest{
		nativeDoltMemStorage: newNativeDoltMemStorage(),
		metadata:             make(map[string]string),
	}
}

func (s *atomicNativeDoltStorageForTest) RunInTransaction(_ context.Context, _ string, fn func(beadslib.Transaction) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commits++
	metadataSnapshot := maps.Clone(s.metadata)
	err := runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(atomicNativeDoltTransactionForTest{
			nativeDoltTransactionForTest: nativeDoltTransactionForTest{storage: s},
			storage:                      s,
		})
	})
	if err != nil {
		s.metadata = metadataSnapshot
	}
	return err
}

func (s *atomicNativeDoltStorageForTest) CreateIssue(ctx context.Context, issue *beadslib.Issue, actor string) error {
	if issue.ID == "" {
		return s.nativeDoltMemStorage.CreateIssue(ctx, issue, actor)
	}
	withoutDependencies := *issue
	withoutDependencies.Dependencies = nil
	bead, err := beadFromNativeIssue(&withoutDependencies)
	if err != nil {
		return err
	}
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	if s.store.indexOfLocked(bead.ID) >= 0 {
		return fmt.Errorf("bead %q already exists", bead.ID)
	}
	if bead.Status == "" {
		bead.Status = "open"
	}
	if bead.Type == "" {
		bead.Type = "task"
	}
	if bead.CreatedAt.IsZero() {
		bead.CreatedAt = time.Now().UTC()
	}
	bead.Revision = 1
	s.store.beads = append(s.store.beads, cloneBead(bead))
	converted, err := nativeIssueFromBead(bead)
	if err != nil {
		return err
	}
	*issue = *converted
	return nil
}

type atomicNativeDoltTransactionForTest struct {
	nativeDoltTransactionForTest
	storage *atomicNativeDoltStorageForTest
}

func (tx atomicNativeDoltTransactionForTest) GetMetadata(_ context.Context, key string) (string, error) {
	return tx.storage.metadata[key], nil
}

func (tx atomicNativeDoltTransactionForTest) SetMetadata(_ context.Context, key, value string) error {
	tx.storage.metadata[key] = value
	return nil
}

func (tx atomicNativeDoltTransactionForTest) SearchIssues(ctx context.Context, query string, filter beadslib.IssueFilter) ([]*beadslib.Issue, error) {
	return tx.storage.SearchIssues(ctx, query, filter)
}
