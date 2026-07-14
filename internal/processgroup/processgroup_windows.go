//go:build windows

package processgroup

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// StartCommandInNewGroup is a no-op on Windows. CREATE_NEW_PROCESS_GROUP is a
// console-control facility, not process-tree containment; callers receive the
// explicit direct-process behavior implemented by SignalCommand and
// TerminateCommand instead.
func StartCommandInNewGroup(_ *exec.Cmd) {}

// SignalCommand signals only cmd's root process on Windows. Go supports
// os.Kill; other signals may return a platform error.
func SignalCommand(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}

// Terminate rejects group-only termination on Windows. A console process group
// is not a killable containment boundary, so success here would falsely claim
// descendant cleanup.
func Terminate(pgid int, _ time.Duration, _ Options) error {
	return fmt.Errorf("%w: cannot terminate process group %d", ErrProcessGroupsUnsupported, pgid)
}

// TerminateCommand force-terminates only cmd's root process on Windows.
func TerminateCommand(cmd *exec.Cmd, _ int, _ time.Duration, _ Options) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	err := cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	return err
}
