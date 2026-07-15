//go:build integration

package tmux

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestNudgeEffectRealTmuxConditionallyEntersExactIsolatedPane(t *testing.T) {
	if !hasTmux() {
		t.Skip("tmux not installed")
	}
	tm := testTmux()
	sessionName := fmt.Sprintf("gctest-nudge-effect-%d", time.Now().UnixNano())
	if _, err := tm.run("new-session", "-d", "-s", sessionName, "cat"); err != nil {
		t.Fatalf("new isolated tmux session: %v", err)
	}
	t.Cleanup(func() { _, _ = tm.run("kill-session", "-t", sessionName) })
	if err := tm.SetEnvironment(sessionName, "GC_INSTANCE_TOKEN", "expected-token"); err != nil {
		t.Fatalf("SetEnvironment: %v", err)
	}

	cfg := DefaultConfig()
	cfg.SocketName = testSocketName
	cfg.DebounceMs = 0
	provider := NewProviderWithConfig(cfg)
	recorded := &recordingNudgeEffectExecutor{delegate: provider.tm.exec}
	provider.tm.exec = recorded
	result, err := provider.NudgeEffect(t.Context(), sessionName, validTmuxNudgeEffectRequest())
	if err != nil {
		t.Fatalf("NudgeEffect real tmux: %v", err)
	}
	if result.Stage != runtime.NudgeEffectStageAccepted || result.Completion != runtime.NudgeEffectCompletionCompleted {
		t.Fatalf("NudgeEffect real tmux = %#v, want accepted/completed", result)
	}

	if _, err := tm.run("copy-mode", "-t", sessionName); err != nil {
		t.Fatalf("enter copy mode: %v", err)
	}
	request := validTmuxNudgeEffectRequest()
	request.Contract.OperationID = "operation-copy-mode"
	refused, err := provider.NudgeEffect(t.Context(), sessionName, request)
	if !errors.Is(err, runtime.ErrNudgeCopyMode) {
		t.Fatalf("NudgeEffect copy-mode error = %v, want copy-mode; calls=%#v", err, recorded.calls)
	}
	if refused.Stage != runtime.NudgeEffectStageNotEntered || refused.Completion != runtime.NudgeEffectCompletionNotCompleted {
		t.Fatalf("NudgeEffect copy-mode = %#v, want not-entered", refused)
	}
}

type recordingNudgeEffectExecutor struct {
	delegate executor
	calls    [][]string
}

func (e *recordingNudgeEffectExecutor) execute(args []string) (string, error) {
	e.calls = append(e.calls, append([]string(nil), args...))
	return e.delegate.execute(args)
}

func (e *recordingNudgeEffectExecutor) executeCtx(ctx context.Context, args []string) (string, error) {
	e.calls = append(e.calls, append([]string(nil), args...))
	return e.delegate.executeCtx(ctx, args)
}
