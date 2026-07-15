package hybrid

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderNudgeEffectDelegatesOnlyToRoutedBackend(t *testing.T) {
	tests := []struct {
		name       string
		session    string
		wantResult runtime.NudgeEffectResult
		backendErr error
	}{
		{
			name:    "local success",
			session: "local-agent",
			wantResult: runtime.NudgeEffectResult{
				Stage:                runtime.NudgeEffectStageAccepted,
				Completion:           runtime.NudgeEffectCompletionCompleted,
				ConsumptionConfirmed: true,
			},
		},
		{
			name:       "remote ambiguous error",
			session:    "remote-agent-1",
			backendErr: runtime.ErrNudgeDeliveryUnknown,
			wantResult: runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageMayHaveEntered,
				Completion: runtime.NudgeEffectCompletionUnknown,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local := runtime.NewFake()
			remote := runtime.NewFake()
			provider := New(local, remote, isRemote)
			startNudgeEffectTargets(t, test.session, local, remote)

			selected, other := local, remote
			if isRemote(test.session) {
				selected, other = remote, local
			}
			selected.NudgeEffectResults[test.session] = test.wantResult
			selected.NudgeEffectErrors[test.session] = test.backendErr

			result, err := provider.NudgeEffect(t.Context(), test.session, validNudgeEffectRequest())
			if !errors.Is(err, test.backendErr) {
				t.Fatalf("NudgeEffect error = %v, want %v", err, test.backendErr)
			}
			if result != test.wantResult {
				t.Fatalf("NudgeEffect result = %#v, want %#v", result, test.wantResult)
			}
			if got := selected.CountCalls("NudgeEffect", test.session); got != 1 {
				t.Fatalf("selected backend NudgeEffect calls = %d, want 1", got)
			}
			if got := other.CountCalls("NudgeEffect", test.session); got != 0 {
				t.Fatalf("other backend NudgeEffect calls = %d, want 0", got)
			}
			if got := selected.CountCalls("Nudge", test.session); got != 0 {
				t.Fatalf("selected backend legacy Nudge calls = %d, want 0", got)
			}
		})
	}
}

func TestProviderNudgeEffectFailsClosedWhenRoutedBackendLacksCapability(t *testing.T) {
	tests := []struct {
		name    string
		session string
	}{
		{name: "local", session: "local-agent"},
		{name: "remote", session: "remote-agent-1"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			localFake := runtime.NewFake()
			remoteFake := runtime.NewFake()
			startNudgeEffectTargets(t, test.session, localFake, remoteFake)
			var local runtime.Provider = localFake
			var remote runtime.Provider = remoteFake
			if isRemote(test.session) {
				remote = &nudgeEffectUnsupportedProvider{Provider: remoteFake}
			} else {
				local = &nudgeEffectUnsupportedProvider{Provider: localFake}
			}
			provider := New(local, remote, isRemote)

			result, err := provider.NudgeEffect(t.Context(), test.session, validNudgeEffectRequest())
			if !errors.Is(err, runtime.ErrNudgeEffectUnsupported) {
				t.Fatalf("NudgeEffect error = %v, want ErrNudgeEffectUnsupported", err)
			}
			if result.Stage != runtime.NudgeEffectStageNotEntered || result.Completion != runtime.NudgeEffectCompletionNotCompleted || result.ConsumptionConfirmed {
				t.Fatalf("NudgeEffect result = %#v, want definite pre-entry refusal", result)
			}
			backends := []struct {
				label    string
				provider *runtime.Fake
			}{
				{label: "local", provider: localFake},
				{label: "remote", provider: remoteFake},
			}
			for _, backend := range backends {
				if got := backend.provider.CountCalls("NudgeEffect", test.session); got != 0 {
					t.Fatalf("%s backend NudgeEffect calls = %d, want 0", backend.label, got)
				}
				if got := backend.provider.CountCalls("Nudge", test.session); got != 0 {
					t.Fatalf("%s backend legacy Nudge calls = %d, want 0", backend.label, got)
				}
			}
		})
	}
}

type nudgeEffectUnsupportedProvider struct {
	runtime.Provider
}

func startNudgeEffectTargets(t *testing.T, session string, providers ...*runtime.Fake) {
	t.Helper()
	for _, provider := range providers {
		if err := provider.Start(t.Context(), session, runtime.Config{}); err != nil {
			t.Fatalf("Start(%s): %v", session, err)
		}
		if err := provider.SetMeta(session, "GC_INSTANCE_TOKEN", "launch-1"); err != nil {
			t.Fatalf("SetMeta(%s): %v", session, err)
		}
	}
}

func validNudgeEffectRequest() runtime.NudgeEffectRequest {
	return runtime.NudgeEffectRequest{
		Contract: runtime.NudgeEffectContract{
			OperationID:            "operation-1",
			ExpectedLaunchIdentity: "launch-1",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
		Content: runtime.TextContent("continue"),
	}
}

var _ runtime.NudgeEffectProvider = (*Provider)(nil)
