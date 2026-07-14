//go:build !windows

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

func makeManagedDoltParentPipeNonInheritable(file *os.File) error {
	if file == nil {
		return errors.New("parent pipe is nil")
	}
	flags, err := unix.FcntlInt(file.Fd(), unix.F_GETFD, 0)
	if err != nil {
		return fmt.Errorf("read parent pipe descriptor flags: %w", err)
	}
	if _, err := unix.FcntlInt(file.Fd(), unix.F_SETFD, flags|unix.FD_CLOEXEC); err != nil {
		return fmt.Errorf("write parent pipe descriptor flags: %w", err)
	}
	return nil
}
