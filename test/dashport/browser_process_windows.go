//go:build integration && windows

package dashport_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
)

func configureBrowserProcess(cmd *exec.Cmd) {
	cmd.Cancel = func() error {
		return terminateBrowserProcess(cmd)
	}
}

func terminateBrowserProcess(cmd *exec.Cmd) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), browserTerminateGrace)
	defer cancel()
	treeKill := exec.CommandContext(ctx, "taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F")
	treeKill.WaitDelay = browserProcessWaitDelay
	if err := treeKill.Run(); err == nil {
		return nil
	}
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}
