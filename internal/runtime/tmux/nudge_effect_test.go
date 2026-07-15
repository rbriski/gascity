package tmux

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderNudgeEffectRefusesUnsafeInteractionBeforeNativeEntry(t *testing.T) {
	tests := []struct {
		name        string
		observation string
		wantErr     error
	}{
		{name: "launch changed", observation: "GC_NUDGE_REFUSED|0|0|replacement-token", wantErr: runtime.ErrNudgeTargetChanged},
		{name: "human attached", observation: "GC_NUDGE_REFUSED|1|0|expected-token", wantErr: runtime.ErrNudgeHumanAttached},
		{name: "copy mode", observation: "GC_NUDGE_REFUSED|0|1|expected-token", wantErr: runtime.ErrNudgeCopyMode},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider, exec := nudgeEffectTestProvider(
				"%1\tclaude\t123",
				test.observation,
			)
			result, err := provider.NudgeEffect(t.Context(), "session-1", validTmuxNudgeEffectRequest())
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("NudgeEffect error = %v, want %v", err, test.wantErr)
			}
			if result.Stage != runtime.NudgeEffectStageNotEntered || result.Completion != runtime.NudgeEffectCompletionNotCompleted || result.ConsumptionConfirmed {
				t.Fatalf("NudgeEffect result = %#v, want definite not-entered", result)
			}
			if got := countTopLevelTmuxCommand(exec.calls, "send-keys"); got != 0 {
				t.Fatalf("top-level native send-keys calls = %d, want 0", got)
			}
		})
	}
}

func TestProviderNudgeEffectAcceptedTransportIsClassifiedUnconfirmed(t *testing.T) {
	provider, exec := nudgeEffectTestProvider(
		"%1\tclaude\t123",
		"GC_NUDGE_REFUSED|0|0|expected-token",
		"1700000000",
		"GC_NUDGE_ENTERED",
		"GC_NUDGE_SUBMITTED",
	)
	result, err := provider.NudgeEffect(t.Context(), "session-1", validTmuxNudgeEffectRequest())
	if err != nil {
		t.Fatalf("NudgeEffect: %v", err)
	}
	if result.Stage != runtime.NudgeEffectStageAccepted || result.Completion != runtime.NudgeEffectCompletionCompleted || result.ConsumptionConfirmed {
		t.Fatalf("NudgeEffect result = %#v, want accepted/completed/unconfirmed", result)
	}
	if len(exec.calls) != 5 || topLevelTmuxCommand(exec.calls[3]) != "if-shell" || topLevelTmuxCommand(exec.calls[4]) != "if-shell" {
		t.Fatalf("tmux calls = %#v, want observation plus two conditional native-entry commands", exec.calls)
	}
	if joined := strings.Join(exec.calls[3], "\x00"); !strings.Contains(joined, "expected-token") || !strings.Contains(joined, "send-keys") {
		t.Fatalf("conditional text entry = %q, want launch fence and send-keys", joined)
	}
}

func TestProviderNudgeEffectTreatsEnterNamedMessageAsLiteralText(t *testing.T) {
	provider, exec := nudgeEffectTestProvider(
		"%1\tclaude\t123",
		"GC_NUDGE_REFUSED|0|0|expected-token",
		"1700000000",
		"GC_NUDGE_ENTERED",
		"GC_NUDGE_SUBMITTED",
	)
	request := validTmuxNudgeEffectRequest()
	request.Content = runtime.TextContent("Enter")
	if _, err := provider.NudgeEffect(t.Context(), "session-1", request); err != nil {
		t.Fatalf("NudgeEffect: %v", err)
	}
	if command := strings.Join(exec.calls[3], "\x00"); !strings.Contains(command, "send-keys -t session-1 -l Enter") {
		t.Fatalf("text-entry command = %q, want literal -l Enter", command)
	}
}

func TestSeamBackedProviderCarriesClassifiedNudgeCapability(t *testing.T) {
	provider := NewSeamBackedWithConfig(DefaultConfig())
	if _, ok := provider.(runtime.NudgeEffectProvider); !ok {
		t.Fatalf("NewSeamBackedWithConfig returned %T without NudgeEffectProvider", provider)
	}
}

func TestProviderNudgeEffectErrorAtAtomicEntryIsNeverReportedPreEntry(t *testing.T) {
	provider, _ := nudgeEffectTestProvider("%1\tclaude\t123", "GC_NUDGE_REFUSED|0|0|expected-token", "1700000000")
	provider.tm.exec = &fakeExecutor{
		outs: []string{"%1\tclaude\t123", "GC_NUDGE_REFUSED|0|0|expected-token", "1700000000"},
		errs: []error{nil, nil, nil, errors.New("lost tmux response")},
	}
	result, err := provider.NudgeEffect(t.Context(), "session-1", validTmuxNudgeEffectRequest())
	if !errors.Is(err, runtime.ErrNudgeDeliveryUnknown) {
		t.Fatalf("NudgeEffect error = %v, want delivery-unknown", err)
	}
	if result.Stage != runtime.NudgeEffectStageMayHaveEntered || result.Completion != runtime.NudgeEffectCompletionUnknown {
		t.Fatalf("NudgeEffect result = %#v, want may-have-entered/unknown", result)
	}
}

func TestProviderNudgeEffectCancellationAndInvalidContractNeverTouchTmux(t *testing.T) {
	for name, setup := range map[string]func() (context.Context, runtime.NudgeEffectRequest){
		"canceled": func() (context.Context, runtime.NudgeEffectRequest) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			return ctx, validTmuxNudgeEffectRequest()
		},
		"invalid": func() (context.Context, runtime.NudgeEffectRequest) {
			request := validTmuxNudgeEffectRequest()
			request.Contract.OperationID = ""
			return context.Background(), request
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider, exec := nudgeEffectTestProvider()
			ctx, request := setup()
			result, err := provider.NudgeEffect(ctx, "session-1", request)
			if err == nil || result.Stage != runtime.NudgeEffectStageNotEntered || result.Completion != runtime.NudgeEffectCompletionNotCompleted {
				t.Fatalf("NudgeEffect = %#v, %v; want definite pre-entry refusal", result, err)
			}
			if len(exec.calls) != 0 {
				t.Fatalf("tmux calls = %#v, want none", exec.calls)
			}
		})
	}
}

func nudgeEffectTestProvider(outputs ...string) (*Provider, *fakeExecutor) {
	cfg := DefaultConfig()
	cfg.DebounceMs = 0
	exec := &fakeExecutor{outs: outputs}
	provider := NewProviderWithConfig(cfg)
	provider.tm.exec = exec
	return provider, exec
}

func validTmuxNudgeEffectRequest() runtime.NudgeEffectRequest {
	return runtime.NudgeEffectRequest{
		Contract: runtime.NudgeEffectContract{
			OperationID:            "operation-1",
			ExpectedLaunchIdentity: "expected-token",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
		Content: runtime.TextContent("do the next thing"),
	}
}

func topLevelTmuxCommand(call []string) string {
	for position := 0; position < len(call); {
		switch call[position] {
		case "-u":
			position++
		case "-L":
			position += 2
		default:
			return call[position]
		}
	}
	return ""
}

func countTopLevelTmuxCommand(calls [][]string, command string) int {
	count := 0
	for _, call := range calls {
		if topLevelTmuxCommand(call) == command {
			count++
		}
	}
	return count
}

var _ runtime.NudgeEffectProvider = (*Provider)(nil)
