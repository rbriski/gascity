package main

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestControllerLockLeaseCreatesOnlyMinimalLockScaffold(t *testing.T) {
	cityPath := t.TempDir()

	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lease.Close() })

	if lease.path != controllerLockPath(cityPath) {
		t.Fatalf("lease path = %q, want %q", lease.path, controllerLockPath(cityPath))
	}
	rootEntries, err := os.ReadDir(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(rootEntries) != 1 || rootEntries[0].Name() != ".gc" || !rootEntries[0].IsDir() {
		t.Fatalf("city entries after lock acquire = %v, want only .gc/", entryNames(rootEntries))
	}
	gcEntries, err := os.ReadDir(filepath.Join(cityPath, ".gc"))
	if err != nil {
		t.Fatal(err)
	}
	if len(gcEntries) != 1 || gcEntries[0].Name() != "controller.lock" || gcEntries[0].IsDir() {
		t.Fatalf(".gc entries after lock acquire = %v, want only controller.lock", entryNames(gcEntries))
	}
	info, err := os.Stat(controllerLockPath(cityPath))
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("controller lock mode = %o, want 600", got)
	}
}

func TestControllerLockLeaseDoesNotCreateMissingCityAncestors(t *testing.T) {
	parent := t.TempDir()
	cityPath := filepath.Join(parent, "missing-city")

	lease, err := acquireControllerLock(cityPath)

	if lease != nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("acquire missing city = (%v, %v), want os.ErrNotExist", lease, err)
	}
	if _, err := os.Stat(cityPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing city was materialized, stat error = %v", err)
	}
}

func TestControllerLockLeaseCloseIsIdempotentAndReleases(t *testing.T) {
	cityPath := t.TempDir()
	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("contending acquire error = %v, want errControllerAlreadyRunning", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := lease.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after Close: %v", err)
	}
	_ = reacquired.Close()
}

func TestControllerLockLeaseTransferMovesExactHeldDescriptorOnce(t *testing.T) {
	cityPath := t.TempDir()
	source, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	originalFile := source.file

	target, err := source.Transfer()
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	if source.file != nil {
		t.Fatal("source retained file after transfer")
	}
	if target.file != originalFile {
		t.Fatal("transfer did not move the exact open file descriptor")
	}
	if err := source.Close(); err != nil {
		t.Fatalf("source Close after transfer: %v", err)
	}
	if _, err := source.Transfer(); !errors.Is(err, errControllerLockAlreadyTransferred) {
		t.Fatalf("second source Transfer error = %v, want errControllerLockAlreadyTransferred", err)
	}
	if _, err := target.Transfer(); !errors.Is(err, errControllerLockAlreadyTransferred) {
		t.Fatalf("target Transfer error = %v, want single-use transfer rejection", err)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("contending acquire after source Close = %v, want lock still held", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("target Close: %v", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after target Close: %v", err)
	}
	_ = reacquired.Close()
}

func TestControllerLockLeaseTransferRejectsClosedLease(t *testing.T) {
	lease, err := acquireControllerLock(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := lease.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := lease.Transfer(); !errors.Is(err, errControllerLockLeaseClosed) {
		t.Fatalf("Transfer after Close error = %v, want errControllerLockLeaseClosed", err)
	}
}

func TestAcquireControllerLockPreservesUnexpectedFlockError(t *testing.T) {
	cityPath := t.TempDir()
	wantErr := syscall.EPERM
	ops := defaultControllerLockOps()
	ops.flock = func(fd, operation int) error {
		if err := syscall.Flock(fd, operation); err != nil {
			t.Fatalf("seeding injected held lock: %v", err)
		}
		return wantErr
	}

	lease, err := acquireControllerLockWithOps(cityPath, ops)

	if lease != nil {
		t.Fatal("lease returned after flock failure")
	}
	if !errors.Is(err, wantErr) {
		t.Fatalf("acquire error = %v, want preserved EPERM", err)
	}
	if errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("unexpected flock error collapsed to contention: %v", err)
	}
	// The failed acquisition must close its descriptor.
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after injected flock failure: %v", err)
	}
	_ = reacquired.Close()
}

func TestControllerLocksRemainScopedToCityPath(t *testing.T) {
	sharedStore := t.TempDir()
	cityA := filepath.Join(t.TempDir(), "city-a")
	cityB := filepath.Join(t.TempDir(), "city-b")
	for _, cityPath := range []string{cityA, cityB} {
		if err := os.MkdirAll(cityPath, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(sharedStore, filepath.Join(cityPath, ".beads")); err != nil {
			t.Fatal(err)
		}
	}

	leaseA, err := acquireControllerLock(cityA)
	if err != nil {
		t.Fatal(err)
	}
	defer leaseA.Close() //nolint:errcheck
	leaseB, err := acquireControllerLock(cityB)
	if err != nil {
		t.Fatalf("second city path sharing one store must retain independent legacy scope: %v", err)
	}
	defer leaseB.Close() //nolint:errcheck
}

func TestAcquireControllerLockForStopRequiresFreeValidatedOwnership(t *testing.T) {
	cityPath := t.TempDir()
	result := controllerStopResult{
		outcome:    controllerStopDefinitePreEntryUnavailable,
		socketPath: controllerSocketPath(cityPath),
	}

	lease, err := acquireControllerLockForStop(cityPath, result)
	if err != nil {
		t.Fatalf("free direct acquire: %v", err)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("direct lease not retained: %v", err)
	}
	_ = lease.Close()

	held, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close() //nolint:errcheck
	if lease, err := acquireControllerLockForStop(cityPath, result); lease != nil || !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("held direct acquire = (%v, %v), want nil + errControllerAlreadyRunning", lease, err)
	}
}

func TestAcquireControllerLockForStopRejectsSocketAppearingAfterDefinitePreEntry(t *testing.T) {
	cityPath := t.TempDir()
	result := controllerStopResult{
		outcome:    controllerStopDefinitePreEntryUnavailable,
		socketPath: controllerSocketPath(cityPath),
	}
	if err := os.MkdirAll(filepath.Dir(result.socketPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(result.socketPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	lease, err := acquireControllerLockForStop(cityPath, result)

	if lease != nil || !errors.Is(err, errControllerSocketReplaced) {
		t.Fatalf("direct acquire = (%v, %v), want replacement rejection", lease, err)
	}
	probe, probeErr := acquireControllerLock(cityPath)
	if probeErr != nil {
		t.Fatalf("failed ownership check leaked lock: %v", probeErr)
	}
	_ = probe.Close()
}

func TestControllerLockStopAPIsRejectWrongOutcomeBeforeAcquire(t *testing.T) {
	cityPath := t.TempDir()
	acquires := 0
	ops := defaultControllerLockWaitOps()
	ops.acquire = func(string) (*controllerLockLease, error) {
		acquires++
		return nil, errors.New("must not acquire")
	}
	tests := []struct {
		name string
		run  func() (*controllerLockLease, error)
	}{
		{
			name: "immediate acquire rejects acknowledged",
			run: func() (*controllerLockLease, error) {
				return acquireControllerLockForStopWithOps(cityPath, controllerStopResult{
					outcome:    controllerStopAcknowledged,
					socketPath: controllerSocketPath(cityPath),
					socketInfo: statFixtureInfo(t, "ack-witness"),
				}, ops)
			},
		},
		{
			name: "wait rejects definite pre-entry",
			run: func() (*controllerLockLease, error) {
				return waitForControllerExitAndAcquireWithOps(cityPath, controllerStopResult{
					outcome:    controllerStopDefinitePreEntryUnavailable,
					socketPath: controllerSocketPath(cityPath),
				}, time.Second, ops)
			},
		},
		{
			name: "wait rejects acknowledgement carrying error",
			run: func() (*controllerLockLease, error) {
				return waitForControllerExitAndAcquireWithOps(cityPath, controllerStopResult{
					outcome:    controllerStopAcknowledged,
					err:        errors.New("contradictory acknowledgement"),
					socketPath: controllerSocketPath(cityPath),
					socketInfo: statFixtureInfo(t, "bad-ack-witness"),
				}, time.Second, ops)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lease, err := tt.run()
			if lease != nil || !errors.Is(err, errControllerStopOwnershipUnproven) {
				t.Fatalf("result = (%v, %v), want ownership-unproven rejection", lease, err)
			}
		})
	}
	if acquires != 0 {
		t.Fatalf("acquire calls for invalid API outcomes = %d, want 0", acquires)
	}
}

func TestControllerLockStopAPIsRejectInvalidWitnessBeforeAcquire(t *testing.T) {
	cityPath := t.TempDir()
	acquires := 0
	ops := defaultControllerLockWaitOps()
	ops.acquire = func(string) (*controllerLockLease, error) {
		acquires++
		return nil, errors.New("must not acquire")
	}
	tests := []struct {
		name string
		run  func() (*controllerLockLease, error)
	}{
		{
			name: "direct path mismatch",
			run: func() (*controllerLockLease, error) {
				return acquireControllerLockForStopWithOps(cityPath, controllerStopResult{
					outcome:    controllerStopDefinitePreEntryUnavailable,
					socketPath: filepath.Join(cityPath, ".gc", "wrong.sock"),
				}, ops)
			},
		},
		{
			name: "acknowledgement missing identity",
			run: func() (*controllerLockLease, error) {
				return waitForControllerExitAndAcquireWithOps(cityPath, controllerStopResult{
					outcome:    controllerStopAcknowledged,
					socketPath: controllerSocketPath(cityPath),
				}, time.Second, ops)
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lease, err := tt.run()
			if lease != nil || !errors.Is(err, errControllerStopOwnershipUnproven) {
				t.Fatalf("result = (%v, %v), want invalid-witness rejection", lease, err)
			}
		})
	}
	if acquires != 0 {
		t.Fatalf("acquire calls for invalid witnesses = %d, want 0", acquires)
	}
}

func TestWaitForControllerExitAndAcquireReturnsContinuouslyHeldLease(t *testing.T) {
	cityPath := t.TempDir()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: socketInfo,
	}
	retries := 0
	ops := defaultControllerLockWaitOps()
	ops.retry = func(time.Duration) {
		retries++
		if err := os.Remove(sockPath); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}

	lease, err := waitForControllerExitAndAcquireWithOps(cityPath, result, time.Second, ops)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if retries != 1 {
		t.Fatalf("retry barriers = %d, want 1", retries)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("wait returned without retaining lease: %v", err)
	}
	_ = lease.Close()
}

func TestWaitForControllerExitAndAcquireRejectsReplacementBeforeAcquire(t *testing.T) {
	cityPath := t.TempDir()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHandle, err := os.Open(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer originalHandle.Close() //nolint:errcheck
	original, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sockPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: original,
	}

	lease, err := waitForControllerExitAndAcquireWithOps(cityPath, result, time.Second, defaultControllerLockWaitOps())

	if lease != nil || !errors.Is(err, errControllerSocketReplaced) {
		t.Fatalf("wait = (%v, %v), want replacement rejection", lease, err)
	}
}

func TestWaitForControllerExitAndAcquireRejectsStarterWinningDuringWait(t *testing.T) {
	cityPath := t.TempDir()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHandle, err := os.Open(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	defer originalHandle.Close() //nolint:errcheck
	original, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: original,
	}
	ops := defaultControllerLockWaitOps()
	acquireCalls := 0
	ops.acquire = func(string) (*controllerLockLease, error) {
		acquireCalls++
		return nil, errControllerAlreadyRunning
	}
	ops.retry = func(time.Duration) {
		if err := os.Remove(sockPath); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(sockPath, []byte("replacement"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	lease, waitErr := waitForControllerExitAndAcquireWithOps(cityPath, result, time.Second, ops)

	if lease != nil || !errors.Is(waitErr, errControllerSocketReplaced) {
		t.Fatalf("wait = (%v, %v), want replacement rejection", lease, waitErr)
	}
	if acquireCalls != 1 {
		t.Fatalf("acquire calls = %d, want only the original contended attempt", acquireCalls)
	}
}

func TestWaitForControllerExitAndAcquireTimesOutWithoutReleasingOwner(t *testing.T) {
	cityPath := t.TempDir()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: socketInfo,
	}
	now := time.Unix(100, 0)
	ops := defaultControllerLockWaitOps()
	ops.now = func() time.Time { return now }
	ops.retry = func(time.Duration) { now = now.Add(time.Second) }

	lease, err := waitForControllerExitAndAcquireWithOps(cityPath, result, time.Second, ops)

	if lease != nil || !errors.Is(err, errControllerLockWaitTimeout) {
		t.Fatalf("wait = (%v, %v), want timeout", lease, err)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("timeout released original owner: %v", err)
	}
}

func TestWaitForControllerExitAndAcquirePreservesUnexpectedAcquireErrorWithoutRetry(t *testing.T) {
	cityPath := t.TempDir()
	witness := statFixtureInfo(t, "wait-acquire-witness")
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: controllerSocketPath(cityPath),
		socketInfo: witness,
	}
	wantErr := syscall.EPERM
	retries := 0
	ops := defaultControllerLockWaitOps()
	ops.acquire = func(string) (*controllerLockLease, error) { return nil, wantErr }
	ops.retry = func(time.Duration) { retries++ }

	lease, err := waitForControllerExitAndAcquireWithOps(cityPath, result, time.Second, ops)

	if lease != nil || !errors.Is(err, wantErr) {
		t.Fatalf("wait = (%v, %v), want preserved EPERM", lease, err)
	}
	if retries != 0 {
		t.Fatalf("retries after unexpected acquire error = %d, want 0", retries)
	}
}

func TestWaitForControllerExitAndAcquireRemovesSameStaleLongSocket(t *testing.T) {
	cityPath := t.TempDir()
	for len(filepath.Join(normalizePathForCompare(cityPath), ".gc", "controller.sock")) <= controllerSocketPathLimit {
		cityPath = filepath.Join(cityPath, "long-controller-path-segment")
	}
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o700); err != nil {
		t.Fatal(err)
	}
	sockPath := controllerSocketPath(cityPath)
	legacyPath := filepath.Join(cityPath, ".gc", "controller.sock")
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy-sentinel"), 0o600); err != nil {
		t.Fatal(err)
	}
	socketInfo, err := os.Stat(sockPath)
	if err != nil {
		t.Fatal(err)
	}
	result := controllerStopResult{
		outcome:    controllerStopAcknowledged,
		socketPath: sockPath,
		socketInfo: socketInfo,
	}

	lease, err := waitForControllerExitAndAcquire(cityPath, result, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Close() //nolint:errcheck
	if _, err := os.Stat(sockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale witnessed socket still exists: %v", err)
	}
	data, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatalf("legacy sentinel was removed: %v", err)
	}
	if string(data) != "legacy-sentinel" {
		t.Fatalf("legacy sentinel contents = %q", data)
	}
}

func TestWaitForSupervisorControllerOwnershipWaitsThenReturnsHeldLease(t *testing.T) {
	cityPath := t.TempDir()
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	sockPath := controllerSocketPath(cityPath)
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	retries := 0
	statCalls := 0
	ops := defaultControllerLockWaitOps()
	ops.stat = func(path string) (os.FileInfo, error) {
		statCalls++
		return os.Stat(path)
	}
	ops.retry = func(time.Duration) {
		retries++
		if err := os.Remove(sockPath); err != nil {
			t.Fatal(err)
		}
		if err := owner.Close(); err != nil {
			t.Fatal(err)
		}
	}

	lease, err := waitForSupervisorControllerOwnershipWithOps(cityPath, time.Second, ops)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if retries != 1 {
		t.Fatalf("retry barriers = %d, want 1", retries)
	}
	if statCalls != 1 {
		t.Fatalf("socket stat calls = %d, want exactly one after ownership", statCalls)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("wait returned without retaining lease: %v", err)
	}
	_ = lease.Close()
}

func TestWaitForSupervisorControllerOwnershipRejectsUnwitnessedSocket(t *testing.T) {
	cityPath := t.TempDir()
	sockPath := controllerSocketPath(cityPath)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sockPath, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	lease, err := waitForSupervisorControllerOwnership(cityPath, time.Second)

	if lease != nil || !errors.Is(err, errControllerSocketReplaced) {
		t.Fatalf("wait = (%v, %v), want unwitnessed replacement rejection", lease, err)
	}
	data, readErr := os.ReadFile(sockPath)
	if readErr != nil {
		t.Fatalf("replacement was removed: %v", readErr)
	}
	if string(data) != "replacement" {
		t.Fatalf("replacement contents = %q", data)
	}
	probe, probeErr := acquireControllerLock(cityPath)
	if probeErr != nil {
		t.Fatalf("reacquire after replacement rejection: %v", probeErr)
	}
	_ = probe.Close()
}

func TestWaitForSupervisorControllerOwnershipTimesOutWithoutInspectingCurrentOwner(t *testing.T) {
	cityPath := t.TempDir()
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close() //nolint:errcheck
	sockPath := controllerSocketPath(cityPath)
	if err := os.WriteFile(sockPath, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Unix(100, 0)
	statCalls := 0
	ops := defaultControllerLockWaitOps()
	ops.now = func() time.Time { return now }
	ops.retry = func(time.Duration) { now = now.Add(time.Second) }
	ops.stat = func(path string) (os.FileInfo, error) {
		statCalls++
		return os.Stat(path)
	}

	lease, err := waitForSupervisorControllerOwnershipWithOps(cityPath, time.Second, ops)

	if lease != nil || !errors.Is(err, errControllerLockWaitTimeout) {
		t.Fatalf("wait = (%v, %v), want timeout", lease, err)
	}
	if statCalls != 0 {
		t.Fatalf("socket inspected %d times without ownership, want 0", statCalls)
	}
	if _, err := acquireControllerLock(cityPath); !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("timeout released current owner: %v", err)
	}
}

func TestWaitForSupervisorControllerOwnershipReleasesLeaseAfterSocketStatError(t *testing.T) {
	cityPath := t.TempDir()
	wantErr := syscall.EIO
	ops := defaultControllerLockWaitOps()
	ops.stat = func(string) (os.FileInfo, error) { return nil, wantErr }

	lease, err := waitForSupervisorControllerOwnershipWithOps(cityPath, time.Second, ops)

	if lease != nil || !errors.Is(err, wantErr) {
		t.Fatalf("wait = (%v, %v), want preserved EIO", lease, err)
	}
	probe, probeErr := acquireControllerLock(cityPath)
	if probeErr != nil {
		t.Fatalf("socket stat failure leaked lock: %v", probeErr)
	}
	_ = probe.Close()
}

func TestRemoveControllerSocketIfSamePreservesReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "controller.sock")
	if err := os.WriteFile(path, []byte("original"), 0o600); err != nil {
		t.Fatal(err)
	}
	originalHandle, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer originalHandle.Close() //nolint:errcheck
	original, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := removeControllerSocketIfSame(path, original); !errors.Is(err, errControllerSocketReplaced) {
		t.Fatalf("remove replacement error = %v, want errControllerSocketReplaced", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("replacement was removed: %v", err)
	}
	if string(data) != "replacement" {
		t.Fatalf("replacement contents = %q", data)
	}
}

func entryNames(entries []os.DirEntry) []string {
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return names
}
