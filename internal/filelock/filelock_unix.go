//go:build !windows

package filelock

import (
	"errors"
	"os"
	"syscall"
)

func lockFile(file *os.File, mode Mode) error {
	return syscall.Flock(int(file.Fd()), unixOperation(mode))
}

func tryLockFile(file *os.File, mode Mode) (bool, error) {
	err := syscall.Flock(int(file.Fd()), unixOperation(mode)|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN):
		return false, nil
	default:
		return false, err
	}
}

func unlockFile(file *os.File) error {
	return syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
}

func unixOperation(mode Mode) int {
	if mode == Shared {
		return syscall.LOCK_SH
	}
	return syscall.LOCK_EX
}
