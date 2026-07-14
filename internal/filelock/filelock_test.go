package filelock

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestTryLockNormalizesContentionAndUnlockReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openTestFile(t, path)
	second := openTestFile(t, path)

	if err := Lock(first, Exclusive); err != nil {
		t.Fatalf("Lock(first): %v", err)
	}
	acquired, err := TryLock(second, Exclusive)
	if err != nil {
		t.Fatalf("TryLock(second): %v", err)
	}
	if acquired {
		t.Fatal("TryLock(second) acquired while first held the lock")
	}
	if err := Unlock(first); err != nil {
		t.Fatalf("Unlock(first): %v", err)
	}
	acquired, err = TryLock(second, Exclusive)
	if err != nil {
		t.Fatalf("TryLock(second) after unlock: %v", err)
	}
	if !acquired {
		t.Fatal("TryLock(second) did not acquire after unlock")
	}
	if err := Unlock(second); err != nil {
		t.Fatalf("Unlock(second): %v", err)
	}
}

func TestSharedAndExclusiveModesConflictCorrectly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openTestFile(t, path)
	second := openTestFile(t, path)
	third := openTestFile(t, path)

	if err := Lock(first, Shared); err != nil {
		t.Fatalf("Lock(first, Shared): %v", err)
	}
	acquired, err := TryLock(second, Shared)
	if err != nil {
		t.Fatalf("TryLock(second, Shared): %v", err)
	}
	if !acquired {
		t.Fatal("a second shared lock did not acquire")
	}
	acquired, err = TryLock(third, Exclusive)
	if err != nil {
		t.Fatalf("TryLock(third, Exclusive): %v", err)
	}
	if acquired {
		t.Fatal("exclusive lock acquired while shared locks were held")
	}

	if err := Unlock(second); err != nil {
		t.Fatalf("Unlock(second): %v", err)
	}
	if err := Unlock(first); err != nil {
		t.Fatalf("Unlock(first): %v", err)
	}
	acquired, err = TryLock(third, Exclusive)
	if err != nil {
		t.Fatalf("TryLock(third, Exclusive) after shared unlock: %v", err)
	}
	if !acquired {
		t.Fatal("exclusive lock did not acquire after shared locks released")
	}
	acquired, err = TryLock(first, Shared)
	if err != nil {
		t.Fatalf("TryLock(first, Shared) under exclusive lock: %v", err)
	}
	if acquired {
		t.Fatal("shared lock acquired while exclusive lock was held")
	}
	if err := Unlock(third); err != nil {
		t.Fatalf("Unlock(third): %v", err)
	}
}

func TestCloseReleasesLockWithoutExplicitUnlock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openTestFile(t, path)
	second := openTestFile(t, path)

	if err := Lock(first, Exclusive); err != nil {
		t.Fatalf("Lock(first): %v", err)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("Close(first): %v", err)
	}
	acquired, err := TryLock(second, Exclusive)
	if err != nil {
		t.Fatalf("TryLock(second): %v", err)
	}
	if !acquired {
		t.Fatal("closing the held file did not release the lock")
	}
	if err := Unlock(second); err != nil {
		t.Fatalf("Unlock(second): %v", err)
	}
}

func TestLockBlocksUntilHolderReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lock")
	first := openTestFile(t, path)
	second := openTestFile(t, path)

	if err := Lock(first, Exclusive); err != nil {
		t.Fatalf("Lock(first): %v", err)
	}
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- Lock(second, Exclusive)
	}()
	<-started

	if err := Unlock(first); err != nil {
		t.Fatalf("Unlock(first): %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Lock(second) after release: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("Lock(second) did not acquire after first released")
	}
	if err := Unlock(second); err != nil {
		t.Fatalf("Unlock(second): %v", err)
	}
}

func TestLockAcceptsEmptyFileAndRejectsInvalidInputs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty")
	file := openTestFile(t, path)
	if info, err := file.Stat(); err != nil {
		t.Fatalf("Stat: %v", err)
	} else if info.Size() != 0 {
		t.Fatalf("empty lock file size = %d, want 0", info.Size())
	}
	if err := Lock(file, Exclusive); err != nil {
		t.Fatalf("Lock(empty file): %v", err)
	}
	if err := Unlock(file); err != nil {
		t.Fatalf("Unlock(empty file): %v", err)
	}

	if err := Lock(nil, Exclusive); err == nil {
		t.Fatal("Lock(nil) error = nil")
	}
	if _, err := TryLock(nil, Exclusive); err == nil {
		t.Fatal("TryLock(nil) error = nil")
	}
	if err := Unlock(nil); err == nil {
		t.Fatal("Unlock(nil) error = nil")
	}
	if err := Lock(file, Mode(0)); err == nil {
		t.Fatal("Lock with zero mode error = nil")
	}
	if _, err := TryLock(file, Mode(255)); err == nil {
		t.Fatal("TryLock with invalid mode error = nil")
	}

	closed := openTestFile(t, filepath.Join(t.TempDir(), "closed"))
	if err := closed.Close(); err != nil {
		t.Fatalf("Close(closed fixture): %v", err)
	}
	if acquired, err := TryLock(closed, Exclusive); err == nil || acquired {
		t.Fatalf("TryLock(closed) = (%t, %v), want false and an error", acquired, err)
	}
}

func openTestFile(t *testing.T, path string) *os.File {
	t.Helper()
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		t.Fatalf("OpenFile(%q): %v", path, err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
