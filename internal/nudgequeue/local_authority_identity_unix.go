//go:build !windows

package nudgequeue

import (
	"errors"
	"fmt"
	"os"

	"github.com/gastownhall/gascity/internal/filelock"
)

func acquireLocalNudgeAuthorityIdentity(path string, expected os.FileInfo) (*os.File, error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, fmt.Errorf("open database identity guard: %w", err)
	}
	opened, err := file.Stat()
	if err != nil || expected == nil || !os.SameFile(expected, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("%w: database identity guard differs from the authority path", ErrRestoreAnchorUnsafePath)
	}
	acquired, err := filelock.TryLock(file, filelock.Exclusive)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("lock database identity guard: %w", err)
	}
	if !acquired {
		_ = file.Close()
		return nil, ErrRestoreAnchorBusy
	}
	return file, nil
}

func releaseLocalNudgeAuthorityIdentity(file *os.File) error {
	if file == nil {
		return nil
	}
	return errorsJoinUnlockAndClose(file)
}

func errorsJoinUnlockAndClose(file *os.File) error {
	unlockErr := filelock.Unlock(file)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
