package exechost_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/lumen/exechost"
)

func TestRunCapturesStdoutAndZeroExit(t *testing.T) {
	stdout, stderr, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `echo hello`, "", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if strings.TrimSpace(stdout) != "hello" {
		t.Errorf("stdout = %q, want hello", stdout)
	}
	if stderr != "" {
		t.Errorf("stderr = %q, want empty", stderr)
	}
}

func TestRunNonZeroExitIsNotAnError(t *testing.T) {
	_, _, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `exit 3`, "", nil)
	if err != nil {
		t.Fatalf("a non-zero exit must not be a host error: %v", err)
	}
	if code != 3 {
		t.Errorf("exit = %d, want 3", code)
	}
}

func TestRunCapturesStderr(t *testing.T) {
	_, stderr, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `echo oops 1>&2; exit 1`, "", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 1 {
		t.Errorf("exit = %d, want 1", code)
	}
	if strings.TrimSpace(stderr) != "oops" {
		t.Errorf("stderr = %q, want oops", stderr)
	}
}

// TestRunScriptIsSingleArgNoInjection proves the script is handed to the shell
// as one -c argument: shell metacharacters are interpreted by the shell (as
// intended for an exec body), and the host does not splice anything around it.
func TestRunScriptIsSingleArgNoInjection(t *testing.T) {
	stdout, _, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `printf '%s' "$0 a b"`, "", nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	// $0 under `sh -c "<script>"` is the shell name (no extra args spliced in).
	if !strings.HasSuffix(stdout, " a b") {
		t.Errorf("stdout = %q, want it to end with ' a b'", stdout)
	}
}

func TestRunCwd(t *testing.T) {
	dir := t.TempDir()
	stdout, _, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `pwd`, dir, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if !strings.Contains(stdout, strings.TrimPrefix(dir, "/private")) {
		t.Errorf("pwd = %q, want it under %q", strings.TrimSpace(stdout), dir)
	}
}

// TestRunContextTimeoutIsHostError proves a ctx-timeout that kills the process
// is surfaced as a real host error, not swallowed as a normal non-zero exit. A
// killed process returns *exec.ExitError (code -1); without the ctx check the
// host would return (_, -1, nil) and the caller would mis-settle a cancellation
// as a plain failed exec.
func TestRunContextTimeoutIsHostError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, code, err := exechost.Run(ctx, exechost.ProgramExec, `sleep 1`, "", nil)
	if err == nil {
		t.Fatalf("ctx-timeout-killed run must be a host error, got err=nil (code=%d)", code)
	}
	if ctx.Err() == nil {
		t.Fatalf("expected ctx to have expired")
	}
}

func TestRunEnvReplacesEnvironment(t *testing.T) {
	stdout, _, code, err := exechost.Run(context.Background(), exechost.ProgramExec, `printf '%s' "$LUMEN_TEST"`, "", []string{"LUMEN_TEST=xyz"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if code != 0 {
		t.Errorf("exit = %d, want 0", code)
	}
	if stdout != "xyz" {
		t.Errorf("stdout = %q, want xyz", stdout)
	}
}
