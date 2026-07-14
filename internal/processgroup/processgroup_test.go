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

func TestSignalGroupRejectsUnsafeID(t *testing.T) {
	for _, pgid := range []int{0, 1} {
		if err := SignalGroup(pgid, syscall.SIGKILL); err == nil {
			t.Errorf("SignalGroup(%d) error = nil, want refusal", pgid)
		}
	}
}

func TestProcessGroupsUnsupportedSentinelExists(t *testing.T) {
	if ErrProcessGroupsUnsupported == nil {
		t.Fatal("ErrProcessGroupsUnsupported = nil")
	}
}
