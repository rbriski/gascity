package processgroup

import (
	"os/exec"
	"syscall"
	"testing"
)

func TestSignalCommandNilSafe(t *testing.T) {
	if err := SignalCommand(nil, syscall.SIGKILL); err != nil {
		t.Fatalf("SignalCommand(nil) error = %v, want nil", err)
	}
	if err := SignalCommand(&exec.Cmd{}, syscall.SIGKILL); err != nil {
		t.Fatalf("SignalCommand(command without process) error = %v, want nil", err)
	}
}

func TestProcessGroupsUnsupportedSentinelExists(t *testing.T) {
	if ErrProcessGroupsUnsupported == nil {
		t.Fatal("ErrProcessGroupsUnsupported = nil")
	}
}
