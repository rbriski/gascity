// Package filelock provides cross-process file locking for caller-owned files.
package filelock

import (
	"errors"
	"fmt"
	"os"
	"runtime"
)

// Mode selects shared or exclusive lock semantics.
type Mode uint8

const (
	// Shared permits other shared holders while excluding exclusive holders.
	Shared Mode = iota + 1
	// Exclusive excludes both shared and exclusive holders.
	Exclusive
)

var errNilFile = errors.New("filelock: nil file")

// Lock blocks until mode is acquired on file. The caller owns file and must
// keep it open until calling Unlock or Close.
func Lock(file *os.File, mode Mode) error {
	if err := validate(file, mode); err != nil {
		return err
	}
	err := lockFile(file, mode)
	runtime.KeepAlive(file)
	return err
}

// TryLock attempts to acquire mode on file without blocking. It returns
// (false, nil) only when another holder currently prevents acquisition.
// The caller owns file and must keep it open after a successful acquisition
// until calling Unlock or Close.
func TryLock(file *os.File, mode Mode) (bool, error) {
	if err := validate(file, mode); err != nil {
		return false, err
	}
	acquired, err := tryLockFile(file, mode)
	runtime.KeepAlive(file)
	return acquired, err
}

// Unlock releases the lock held through file without closing file.
func Unlock(file *os.File) error {
	if file == nil {
		return errNilFile
	}
	err := unlockFile(file)
	runtime.KeepAlive(file)
	return err
}

func validate(file *os.File, mode Mode) error {
	if file == nil {
		return errNilFile
	}
	switch mode {
	case Shared, Exclusive:
		return nil
	default:
		return fmt.Errorf("filelock: invalid lock mode %d", mode)
	}
}
