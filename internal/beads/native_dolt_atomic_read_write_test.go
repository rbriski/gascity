package beads

import (
	"context"
	"errors"
	"maps"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

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
	s.commits++
	metadataSnapshot := maps.Clone(s.metadata)
	err := runNativeDoltMemStorageTransactionForTest(s.nativeDoltMemStorage, func() error {
		return fn(atomicNativeDoltTransactionForTest{
			nativeDoltTransactionForTest: nativeDoltTransactionForTest{storage: s.nativeDoltMemStorage},
			storage:                      s,
		})
	})
	if err != nil {
		s.metadata = metadataSnapshot
	}
	return err
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
