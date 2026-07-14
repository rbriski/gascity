//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

// makeManagedDoltParentPipeNonInheritable normalizes a received Windows
// handle. The watchdog remains compile-only on Windows because os/exec does
// not support ExtraFiles there.
func makeManagedDoltParentPipeNonInheritable(file *os.File) error {
	if file == nil {
		return errors.New("parent pipe is nil")
	}
	if err := windows.SetHandleInformation(windows.Handle(file.Fd()), windows.HANDLE_FLAG_INHERIT, 0); err != nil {
		return fmt.Errorf("clear parent pipe HANDLE_FLAG_INHERIT: %w", err)
	}
	return nil
}
