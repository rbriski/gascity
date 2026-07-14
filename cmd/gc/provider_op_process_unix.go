//go:build !windows

package main

import (
	"os/exec"
	"syscall"

	"github.com/gastownhall/gascity/internal/processgroup"
)

func prepareProviderOpCommand(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		return processgroup.SignalCommand(cmd, syscall.SIGKILL)
	}
}
