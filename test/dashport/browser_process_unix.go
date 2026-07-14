//go:build integration && !windows

package dashport_test

import (
	"os/exec"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func configureBrowserProcess(cmd *exec.Cmd) {
	processgroup.StartCommandInNewGroup(cmd)
	cmd.Cancel = func() error {
		return terminateBrowserProcess(cmd)
	}
}

func terminateBrowserProcess(cmd *exec.Cmd) error {
	knownGroupID := 0
	if cmd != nil && cmd.Process != nil {
		// StartCommandInNewGroup makes the npm PID the process-group ID. Keep
		// that ID even if npm exits before a Chromium descendant closes an
		// inherited output pipe.
		knownGroupID = cmd.Process.Pid
	}
	return processgroup.TerminateCommand(
		cmd,
		knownGroupID,
		browserTerminateGrace,
		processgroup.Options{},
	)
}
