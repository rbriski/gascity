//go:build windows

package main

import (
	"os/exec"
	"syscall"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func prepareProviderOpCommand(cmd *exec.Cmd) {
	// Windows provider cleanup remains direct-process-only until provider ops use job objects.
	cmd.Cancel = func() error {
		return processgroup.SignalCommand(cmd, syscall.SIGKILL)
	}
}
