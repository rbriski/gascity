// Package exechost is the Lumen exec-host: it runs an `exec` node's script
// under a POSIX shell and reports its captured output and exit status. It is
// the single side-effecting leaf the linear executor drives; everything above
// it (the reducer, the journal fold) stays pure.
//
// The exec-host contract is that the script is handed to the shell as ONE
// argument to `-c`: the host runs the script text as-is and never splices it
// into a command line of its own. This says nothing about safety of the script
// itself — values the caller interpolated into the script (see engine
// interpolate) are spliced verbatim and are NOT quoted here, so an authored
// script that embeds untrusted input remains injection-prone. The host quotes
// nothing; whatever text it receives is the program the shell parses.
package exechost

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
)

// Program selects the interpreter. The Lumen IR carries the interpreter program
// kind ("exec" for a plain POSIX shell, "bash" for bash); any value other than
// "bash" runs under /bin/sh.
const (
	ProgramExec = "exec"
	ProgramBash = "bash"
)

// Run executes script under a POSIX shell and returns its captured stdout and
// stderr, the process exit code, and an error only for failures that are NOT a
// non-zero exit (a non-zero exit is a normal outcome the caller maps to a
// failed settlement, so it returns err == nil with the real exitCode).
//
// program selects the shell: "bash" runs /bin/bash, anything else /bin/sh. The
// script is passed as a single argument to the shell's -c flag; the host does
// NOT quote or escape values a caller interpolated into the script (that is the
// caller's responsibility — the host runs whatever text it is given). cwd sets
// the working directory when non-empty; env, when non-nil, replaces the child
// environment (nil inherits the parent's).
func Run(ctx context.Context, program, script, cwd string, env []string) (stdout, stderr string, exitCode int, err error) {
	shell := "/bin/sh"
	if program == ProgramBash {
		shell = "/bin/bash"
	}

	cmd := exec.CommandContext(ctx, shell, "-c", script)
	if cwd != "" {
		cmd.Dir = cwd
	}
	if env != nil {
		cmd.Env = env
	}

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	runErr := cmd.Run()
	stdout = outBuf.String()
	stderr = errBuf.String()

	if runErr == nil {
		return stdout, stderr, 0, nil
	}
	// A ctx cancellation/timeout that kills the process surfaces as an ExitError
	// (code -1), so this MUST be checked before the ExitError branch below —
	// otherwise a killed run would masquerade as a normal non-zero exit and the
	// caller would settle a cancellation as a plain failed exec. Return the ctx
	// error as a real host failure.
	if ctxErr := ctx.Err(); ctxErr != nil {
		return stdout, stderr, -1, ctxErr
	}
	// A non-zero exit is a normal outcome, not a host failure: surface the code
	// and no error so the caller settles it as a failed exec.
	var exitErr *exec.ExitError
	if errors.As(runErr, &exitErr) {
		return stdout, stderr, exitErr.ExitCode(), nil
	}
	// Anything else — shell missing, context canceled, cwd unusable — is a real
	// host failure the caller cannot interpret as an exit code.
	return stdout, stderr, -1, runErr
}
