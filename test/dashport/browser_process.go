//go:build integration

package dashport_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"time"
)

const (
	browserProcessWaitDelay = 2 * time.Second
	browserTerminateGrace   = 2 * time.Second
)

type browserProcessConfig struct {
	dir string
	env []string
}

func runStructuredTranscriptBrowser(ctx context.Context, cfg browserProcessConfig) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "npm", "run", "test:e2e:structured")
	cmd.Dir = cfg.dir
	cmd.Env = append(os.Environ(), cfg.env...)
	configureBrowserProcess(cmd)
	cmd.WaitDelay = browserProcessWaitDelay
	output, err := cmd.CombinedOutput()
	if ctx.Err() != nil || errors.Is(err, exec.ErrWaitDelay) {
		if cleanupErr := terminateBrowserProcess(cmd); cleanupErr != nil {
			err = errors.Join(err, cleanupErr)
		}
	}
	return output, err
}
