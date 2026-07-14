//go:build !windows

package runtime

import (
	"os/exec"
	"syscall"
)

func signalProcessGroup(cmd *exec.Cmd, sig syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, sig); err == nil {
		return nil
	}
	return cmd.Process.Signal(sig)
}
