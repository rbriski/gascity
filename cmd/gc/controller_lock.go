package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/filelock"
)

var (
	errControllerLockAlreadyTransferred = errors.New("controller lock lease already transferred")
	errControllerLockLeaseClosed        = errors.New("controller lock lease closed")
	errControllerLockWaitTimeout        = errors.New("timed out waiting for controller lock")
	errControllerSocketReplaced         = errors.New("controller socket was replaced")
	errControllerStopOwnershipUnproven  = errors.New("controller stop ownership is unproven")
)

type controllerLockLease struct {
	mu               sync.Mutex
	path             string
	file             *os.File
	closed           bool
	transferred      bool
	transferDisabled bool
}

func (l *controllerLockLease) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	if l.closed || l.transferred {
		l.mu.Unlock()
		return nil
	}
	file := l.file
	l.file = nil
	l.closed = true
	l.mu.Unlock()
	if file == nil {
		return nil
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("closing controller lock lease: %w", err)
	}
	return nil
}

func (l *controllerLockLease) Transfer() (*controllerLockLease, error) {
	if l == nil {
		return nil, errControllerLockLeaseClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.transferred || l.transferDisabled {
		return nil, errControllerLockAlreadyTransferred
	}
	if l.closed || l.file == nil {
		return nil, errControllerLockLeaseClosed
	}
	transferred := &controllerLockLease{
		path:             l.path,
		file:             l.file,
		transferDisabled: true,
	}
	l.file = nil
	l.transferred = true
	return transferred, nil
}

func controllerLockPath(cityPath string) string {
	return filepath.Join(cityPath, ".gc", "controller.lock")
}

type controllerLockOps struct {
	mkdir    func(string, os.FileMode) error
	openFile func(string, int, os.FileMode) (*os.File, error)
	tryLock  func(*os.File) (bool, error)
}

func defaultControllerLockOps() controllerLockOps {
	return controllerLockOps{
		mkdir:    os.Mkdir,
		openFile: os.OpenFile,
		tryLock: func(file *os.File) (bool, error) {
			return filelock.TryLock(file, filelock.Exclusive)
		},
	}
}

// acquireControllerLock takes a non-blocking exclusive lease on the city's
// stable controller.lock inode. It may create only the minimal .gc parent and
// lock file; all city materialization belongs behind the acquired lease.
func acquireControllerLock(cityPath string) (*controllerLockLease, error) {
	return acquireControllerLockWithOps(cityPath, defaultControllerLockOps())
}

func acquireControllerLockWithOps(cityPath string, ops controllerLockOps) (*controllerLockLease, error) {
	lockPath := controllerLockPath(cityPath)
	if err := ops.mkdir(filepath.Dir(lockPath), 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("creating controller lock parent: %w", err)
	}
	file, err := ops.openFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("opening controller lock: %w", err)
	}
	acquired, err := ops.tryLock(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("locking controller lock: %w", err)
	}
	if !acquired {
		_ = file.Close()
		return nil, errControllerAlreadyRunning
	}
	return &controllerLockLease{path: lockPath, file: file}, nil
}

type controllerLockWaitOps struct {
	stat     func(string) (os.FileInfo, error)
	acquire  func(string) (*controllerLockLease, error)
	now      func() time.Time
	retry    func(time.Duration)
	interval time.Duration
}

func defaultControllerLockWaitOps() controllerLockWaitOps {
	return controllerLockWaitOps{
		stat:     os.Stat,
		acquire:  acquireControllerLock,
		now:      time.Now,
		retry:    time.Sleep,
		interval: 50 * time.Millisecond,
	}
}

func acquireControllerLockForStop(cityPath string, result controllerStopResult) (*controllerLockLease, error) {
	return acquireControllerLockForStopWithOps(cityPath, result, defaultControllerLockWaitOps())
}

func acquireControllerLockForStopWithOps(cityPath string, result controllerStopResult, ops controllerLockWaitOps) (*controllerLockLease, error) {
	return acquireControllerLockForStopUntilWithOps(cityPath, result, time.Time{}, ops)
}

func acquireControllerLockForStopUntilWithOps(cityPath string, result controllerStopResult, deadline time.Time, ops controllerLockWaitOps) (*controllerLockLease, error) {
	if result.outcome != controllerStopDefinitePreEntryUnavailable {
		return nil, fmt.Errorf("%w: direct acquire requires definite pre-entry result, got %s", errControllerStopOwnershipUnproven, result.outcome)
	}
	checkDeadline := func() error {
		if !deadline.IsZero() && !ops.now().Before(deadline) {
			return errControllerLockWaitTimeout
		}
		return nil
	}
	if err := checkDeadline(); err != nil {
		return nil, err
	}
	if err := validateControllerStopSocketWitness(cityPath, result, ops.stat); err != nil {
		return nil, err
	}
	if err := checkDeadline(); err != nil {
		return nil, err
	}
	lease, err := ops.acquire(cityPath)
	if err != nil {
		return nil, err
	}
	if lease == nil {
		return nil, fmt.Errorf("%w: controller lock acquisition returned no lease", errControllerStopOwnershipUnproven)
	}
	if err := checkDeadline(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if err := validateControllerStopSocketWitness(cityPath, result, ops.stat); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if err := checkDeadline(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if err := removeControllerSocketIfSame(result.socketPath, result.socketInfo); err != nil {
		_ = lease.Close()
		return nil, err
	}
	if err := checkDeadline(); err != nil {
		_ = lease.Close()
		return nil, err
	}
	return lease, nil
}

func waitForControllerExitAndAcquire(cityPath string, result controllerStopResult, timeout time.Duration) (*controllerLockLease, error) {
	return waitForControllerExitAndAcquireWithOps(cityPath, result, timeout, defaultControllerLockWaitOps())
}

func waitForControllerExitAndAcquireWithOps(cityPath string, result controllerStopResult, timeout time.Duration, ops controllerLockWaitOps) (*controllerLockLease, error) {
	if result.outcome != controllerStopAcknowledged || result.err != nil {
		return nil, fmt.Errorf("%w: acknowledged wait requires an error-free acknowledgement", errControllerStopOwnershipUnproven)
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return waitForControllerExitAndAcquireUntilWithOps(cityPath, result, ops.now().Add(timeout), timeout, ops)
}

func waitForControllerExitAndAcquireUntilWithOps(cityPath string, result controllerStopResult, deadline time.Time, timeoutLabel time.Duration, ops controllerLockWaitOps) (*controllerLockLease, error) {
	if result.outcome != controllerStopAcknowledged || result.err != nil {
		return nil, fmt.Errorf("%w: acknowledged wait requires an error-free acknowledgement", errControllerStopOwnershipUnproven)
	}
	if deadline.IsZero() || !ops.now().Before(deadline) {
		return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
	}
	for {
		if !ops.now().Before(deadline) {
			return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
		}
		if err := validateControllerStopSocketWitness(cityPath, result, ops.stat); err != nil {
			return nil, err
		}
		if !ops.now().Before(deadline) {
			return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
		}
		lease, err := ops.acquire(cityPath)
		switch {
		case err == nil:
			if lease == nil {
				return nil, fmt.Errorf("%w: controller lock acquisition returned no lease", errControllerStopOwnershipUnproven)
			}
			if !ops.now().Before(deadline) {
				_ = lease.Close()
				return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
			}
			if err := validateControllerStopSocketWitness(cityPath, result, ops.stat); err != nil {
				_ = lease.Close()
				return nil, err
			}
			if !ops.now().Before(deadline) {
				_ = lease.Close()
				return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
			}
			if err := removeControllerSocketIfSame(result.socketPath, result.socketInfo); err != nil {
				_ = lease.Close()
				return nil, err
			}
			if !ops.now().Before(deadline) {
				_ = lease.Close()
				return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
			}
			return lease, nil
		case !errors.Is(err, errControllerAlreadyRunning):
			return nil, fmt.Errorf("acquiring controller ownership: %w", err)
		default:
			remaining := deadline.Sub(ops.now())
			if remaining <= 0 {
				return nil, fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
			}
			retry := ops.interval
			if retry <= 0 || retry > remaining {
				retry = remaining
			}
			ops.retry(retry)
		}
	}
}

func waitForSupervisorControllerOwnership(cityPath string, timeout time.Duration) (*controllerLockLease, error) {
	return waitForSupervisorControllerOwnershipWithOps(cityPath, timeout, defaultControllerLockWaitOps())
}

func waitForSupervisorControllerOwnershipWithOps(cityPath string, timeout time.Duration, ops controllerLockWaitOps) (*controllerLockLease, error) {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return waitForSupervisorControllerOwnershipUntilWithOps(cityPath, ops.now().Add(timeout), timeout, ops)
}

func waitForSupervisorControllerOwnershipUntil(cityPath string, deadline time.Time, timeoutLabel time.Duration) (*controllerLockLease, error) {
	return waitForSupervisorControllerOwnershipUntilWithOps(cityPath, deadline, timeoutLabel, defaultControllerLockWaitOps())
}

func waitForSupervisorControllerOwnershipUntilWithOps(cityPath string, deadline time.Time, timeoutLabel time.Duration, ops controllerLockWaitOps) (*controllerLockLease, error) {
	timedOut := func() error {
		return fmt.Errorf("%w after %s", errControllerLockWaitTimeout, timeoutLabel)
	}
	if deadline.IsZero() || !ops.now().Before(deadline) {
		return nil, timedOut()
	}
	for {
		if !ops.now().Before(deadline) {
			return nil, timedOut()
		}
		lease, err := ops.acquire(cityPath)
		switch {
		case err == nil:
			if lease == nil {
				return nil, fmt.Errorf("%w: lock acquisition returned no lease", errControllerStopOwnershipUnproven)
			}
			if !ops.now().Before(deadline) {
				_ = lease.Close()
				return nil, timedOut()
			}
			_, statErr := ops.stat(controllerSocketPath(cityPath))
			switch {
			case errors.Is(statErr, os.ErrNotExist):
				if !ops.now().Before(deadline) {
					_ = lease.Close()
					return nil, timedOut()
				}
				return lease, nil
			case statErr != nil:
				_ = lease.Close()
				return nil, fmt.Errorf("stating controller socket after acquiring supervisor ownership: %w", statErr)
			default:
				_ = lease.Close()
				return nil, fmt.Errorf("%w at %s", errControllerSocketReplaced, controllerSocketPath(cityPath))
			}
		case !errors.Is(err, errControllerAlreadyRunning):
			return nil, fmt.Errorf("acquiring supervisor controller ownership: %w", err)
		case !ops.now().Before(deadline):
			return nil, timedOut()
		default:
			remaining := deadline.Sub(ops.now())
			if remaining <= 0 {
				return nil, timedOut()
			}
			retry := ops.interval
			if retry <= 0 || retry > remaining {
				retry = remaining
			}
			ops.retry(retry)
		}
	}
}

func validateControllerStopSocketWitness(cityPath string, result controllerStopResult, stat func(string) (os.FileInfo, error)) error {
	switch result.outcome {
	case controllerStopAcknowledged:
		if result.socketInfo == nil {
			return fmt.Errorf("%w: acknowledged result has no socket identity", errControllerStopOwnershipUnproven)
		}
	case controllerStopDefinitePreEntryUnavailable:
		// A missing pre-dial identity is valid only while the path stays absent.
	default:
		return fmt.Errorf("%w: outcome %s", errControllerStopOwnershipUnproven, result.outcome)
	}
	wantPath := controllerSocketPath(cityPath)
	if result.socketPath == "" || result.socketPath != wantPath {
		return fmt.Errorf("%w: witness path %q does not match %q", errControllerStopOwnershipUnproven, result.socketPath, wantPath)
	}
	current, err := stat(wantPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stating controller socket while acquiring ownership: %w", err)
	}
	if result.socketInfo == nil || !os.SameFile(result.socketInfo, current) {
		return fmt.Errorf("%w at %s", errControllerSocketReplaced, wantPath)
	}
	return nil
}

func removeControllerSocketIfSame(path string, original os.FileInfo) error {
	if original == nil {
		return nil
	}
	current, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("stating controller socket for cleanup: %w", err)
	}
	if !os.SameFile(original, current) {
		return fmt.Errorf("%w at %s", errControllerSocketReplaced, path)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing controller socket: %w", err)
	}
	return nil
}
