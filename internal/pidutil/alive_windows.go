//go:build windows

package pidutil

import (
	"errors"

	"golang.org/x/sys/windows"
)

// Alive reports whether pid names a process that has not exited. Access
// denied is conservative evidence of a live process, not proof of absence.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
	if errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return true
	}
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle) //nolint:errcheck

	state, err := windows.WaitForSingleObject(handle, 0)
	if err != nil {
		return true
	}
	return state != windows.WAIT_OBJECT_0
}
