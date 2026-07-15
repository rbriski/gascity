package beads

import "testing"

func TestCachingStoreAtomicReadWriteHandleUsesOneBackingTransactionAndLiveReads(t *testing.T) {
	t.Parallel()

	storage := newAtomicNativeDoltStorageForTest()
	native := newNativeDoltStoreForTest(storage)
	seed, err := native.Create(Bead{ID: "gc-command-cache", Title: "cached old", Metadata: map[string]string{"state": "pending"}})
	if err != nil {
		t.Fatalf("Create seed: %v", err)
	}
	cache := NewCachingStoreForTest(native, nil)
	if err := cache.Prime(t.Context()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	newTitle := "backing truth"
	if err := native.Update(seed.ID, UpdateOpts{Title: &newTitle}); err != nil {
		t.Fatalf("external backing Update: %v", err)
	}
	stale, err := cache.Get(seed.ID)
	if err != nil {
		t.Fatalf("cached Get before atomic transaction: %v", err)
	}
	if stale.Title != "cached old" {
		t.Fatalf("cached title before transaction = %q, want deliberately stale cached old", stale.Title)
	}

	capability, ok := AtomicReadWriteFor(cache)
	if !ok {
		t.Fatal("AtomicReadWriteFor(CachingStore over NativeDolt) = false, want true")
	}
	commitsBefore := storage.commits
	if err := capability.AtomicReadWrite(t.Context(), "claim command", func(tx AtomicReadWriteTx) error {
		listed, err := tx.ListHistory(AtomicReadWriteList{
			IssueType: "task",
			Metadata:  map[string]string{"state": "pending"},
			Limit:     1,
		})
		if err != nil {
			return err
		}
		if len(listed) != 1 || listed[0].Title != "backing truth" {
			t.Fatalf("transactional ListHistory = %#v, want live backing truth", listed)
		}
		live, err := tx.GetIssue(seed.ID)
		if err != nil {
			return err
		}
		if live.Title != "backing truth" {
			t.Fatalf("transactional GetIssue title = %q, want live backing truth", live.Title)
		}
		return tx.Update(seed.ID, UpdateOpts{Metadata: map[string]string{"state": "claimed"}})
	}); err != nil {
		t.Fatalf("AtomicReadWrite: %v", err)
	}
	if got := storage.commits - commitsBefore; got != 1 {
		t.Fatalf("RunInTransaction calls through cache = %d, want exactly 1", got)
	}
	refreshed, err := cache.Get(seed.ID)
	if err != nil {
		t.Fatalf("cached Get after atomic transaction: %v", err)
	}
	if refreshed.Title != "backing truth" || refreshed.Metadata["state"] != "claimed" {
		t.Fatalf("refreshed cache = %+v, want live title and claimed state", refreshed)
	}
}

func TestCachingStoreAtomicReadWriteHandleDoesNotFabricateCapability(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if capability, ok := AtomicReadWriteFor(cache); ok || capability != nil {
		t.Fatalf("AtomicReadWriteFor(cache over MemStore) = (%T, %v), want typed absence", capability, ok)
	}
}
