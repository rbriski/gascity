package auto

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestProviderNudgeEffectDelegatesOnlyToRoutedBackend(t *testing.T) {
	tests := []struct {
		name       string
		session    string
		routeACP   bool
		wantResult runtime.NudgeEffectResult
		backendErr error
	}{
		{
			name:    "default success",
			session: "default-session",
			wantResult: runtime.NudgeEffectResult{
				Stage:                runtime.NudgeEffectStageAccepted,
				Completion:           runtime.NudgeEffectCompletionCompleted,
				ConsumptionConfirmed: true,
			},
		},
		{
			name:       "acp ambiguous error",
			session:    "acp-session",
			routeACP:   true,
			backendErr: runtime.ErrNudgeDeliveryUnknown,
			wantResult: runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageMayHaveEntered,
				Completion: runtime.NudgeEffectCompletionUnknown,
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defaultProvider := runtime.NewFake()
			acpProvider := runtime.NewFake()
			provider := New(defaultProvider, acpProvider)
			if test.routeACP {
				provider.RouteACP(test.session)
			}
			startNudgeEffectTargets(t, test.session, defaultProvider, acpProvider)

			selected, other := defaultProvider, acpProvider
			if test.routeACP {
				selected, other = acpProvider, defaultProvider
			}
			selected.NudgeEffectResults[test.session] = test.wantResult
			selected.NudgeEffectErrors[test.session] = test.backendErr

			request := validNudgeEffectRequest()
			result, err := provider.NudgeEffect(t.Context(), test.session, request)
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
		name     string
		session  string
		routeACP bool
	}{
		{name: "default", session: "default-session"},
		{name: "acp", session: "acp-session", routeACP: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			defaultFake := runtime.NewFake()
			acpFake := runtime.NewFake()
			startNudgeEffectTargets(t, test.session, defaultFake, acpFake)
			var defaultProvider runtime.Provider = defaultFake
			var acpProvider runtime.Provider = acpFake
			if test.routeACP {
				acpProvider = &nudgeEffectUnsupportedProvider{Provider: acpFake}
			} else {
				defaultProvider = &nudgeEffectUnsupportedProvider{Provider: defaultFake}
			}
			provider := New(defaultProvider, acpProvider)
			if test.routeACP {
				provider.RouteACP(test.session)
			}

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
				{label: "default", provider: defaultFake},
				{label: "acp", provider: acpFake},
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
