// Package execprovider models the legacy read/write Exec seam and its typed
// outer replacement for effect-boundary discovery tests.
package execprovider

import "context"

// ExecRequest is the fixture's typed outer-place request.
type ExecRequest struct {
	Argv []string
}

// ExecResult is the fixture's typed outer-place result.
type ExecResult struct {
	Output []byte
	Code   int
}

// Place is the typed outer mutation seam.
type Place interface {
	Exec(context.Context, ExecRequest) (ExecResult, error)
}

// LegacyExecProvider is the read/write-conflated inner connection seam.
type LegacyExecProvider interface {
	Exec(context.Context, string, []string) ([]byte, int, error)
}

// MutateThroughPlace runs an arbitrary command through the typed outer seam.
func MutateThroughPlace(ctx context.Context, place Place) error {
	_, err := place.Exec(ctx, ExecRequest{Argv: []string{"tmux", "send-keys"}})
	return err
}

// ProcessAlive uses the legacy connection for an observational command.
func ProcessAlive(ctx context.Context, conn LegacyExecProvider) bool {
	_, code, err := conn.Exec(ctx, "session", []string{"pgrep", "agent"})
	return err == nil && code == 0
}
