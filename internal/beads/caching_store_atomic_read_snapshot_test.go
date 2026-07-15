package beads

import (
	"errors"
	"testing"
)

func TestCachingStoreAtomicReadSnapshotHandleForwardsBackingCapability(t *testing.T) {
	t.Parallel()

	native := newNativeDoltStoreForTest(newAtomicNativeDoltStorageForTest())
	cache := NewCachingStoreForTest(native, nil)
	capability, ok := AtomicReadSnapshotFor(cache)
	if !ok {
		t.Fatal("AtomicReadSnapshotFor(CachingStore over NativeDolt) = false, want true")
	}
	called := false
	err := capability.AtomicReadSnapshot(t.Context(), func(AtomicReadSnapshotTx) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrAtomicReadSnapshotUnsupported) {
		t.Fatalf("AtomicReadSnapshot error = %v, want backing ErrAtomicReadSnapshotUnsupported", err)
	}
	if called {
		t.Fatal("forwarded snapshot called callback after backing capability failed closed")
	}
}

func TestCachingStoreAtomicReadSnapshotHandleDoesNotFabricateCapability(t *testing.T) {
	t.Parallel()

	cache := NewCachingStoreForTest(NewMemStore(), nil)
	if capability, ok := AtomicReadSnapshotFor(cache); ok || capability != nil {
		t.Fatalf("AtomicReadSnapshotFor(cache over MemStore) = (%T, %v), want typed absence", capability, ok)
	}
}
