//go:build windows

package filelock

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

const lockRangeLow uint32 = 1

func lockFile(file *os.File, mode Mode) error {
	// os.Open and os.OpenFile create synchronous Windows handles. LockFileEx
	// therefore waits in this call until the range is acquired, and the
	// stack-owned OVERLAPPED remains valid for the entire operation. The shared
	// conformance test exercises this blocking path on Windows runners.
	var overlapped windows.Overlapped
	return windows.LockFileEx(
		windows.Handle(file.Fd()),
		windowsFlags(mode),
		0,
		lockRangeLow,
		0,
		&overlapped,
	)
}

func tryLockFile(file *os.File, mode Mode) (bool, error) {
	var overlapped windows.Overlapped
	err := windows.LockFileEx(
		windows.Handle(file.Fd()),
		windowsFlags(mode)|windows.LOCKFILE_FAIL_IMMEDIATELY,
		0,
		lockRangeLow,
		0,
		&overlapped,
	)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, windows.ERROR_LOCK_VIOLATION), errors.Is(err, windows.ERROR_IO_PENDING):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(file *os.File) error {
	var overlapped windows.Overlapped
	return windows.UnlockFileEx(
		windows.Handle(file.Fd()),
		0,
		lockRangeLow,
		0,
		&overlapped,
	)
}

func windowsFlags(mode Mode) uint32 {
	if mode == Exclusive {
		return windows.LOCKFILE_EXCLUSIVE_LOCK
	}
	return 0
}
