package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRuntimeHandleClassifiedNudgeUsesProviderEffectResult(t *testing.T) {
	provider := runtime.NewFake()
	startFakeNudgeEffectTarget(t, provider)
	provider.NudgeEffectResults["session-a"] = runtime.NudgeEffectResult{
		Stage:                runtime.NudgeEffectStageAccepted,
		Completion:           runtime.NudgeEffectCompletionCompleted,
		ConsumptionConfirmed: true,
	}
	handle := newRuntimeNudgeEffectHandle(t, provider)

	result, err := handle.Nudge(t.Context(), NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !result.Delivered || result.Effect == nil {
		t.Fatalf("classified result = %#v, want delivered effect evidence", result)
	}
	if *result.Effect != provider.NudgeEffectResults["session-a"] {
		t.Fatalf("effect result = %#v, want %#v", *result.Effect, provider.NudgeEffectResults["session-a"])
	}
	if got := provider.CountCalls("NudgeEffect", "session-a"); got != 1 {
		t.Fatalf("classified provider entries = %d, want 1", got)
	}
	if got := provider.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("legacy provider entries = %d, want 0", got)
	}
}

func TestRuntimeHandleClassifiedNudgeFailsClosedBeforeNativeEntry(t *testing.T) {
	tests := []struct {
		name           string
		prepare        func(*runtime.Fake)
		expectedLaunch string
		wantErr        error
	}{
		{
			name:           "launch replaced",
			expectedLaunch: "stale-launch",
			wantErr:        runtime.ErrNudgeTargetChanged,
		},
		{
			name:           "human attached",
			prepare:        func(provider *runtime.Fake) { provider.SetAttached("session-a", true) },
			expectedLaunch: "launch-a",
			wantErr:        runtime.ErrNudgeHumanAttached,
		},
		{
			name:           "copy mode",
			prepare:        func(provider *runtime.Fake) { provider.SetCopyMode("session-a", true) },
			expectedLaunch: "launch-a",
			wantErr:        runtime.ErrNudgeCopyMode,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			provider := runtime.NewFake()
			startFakeNudgeEffectTarget(t, provider)
			if tc.prepare != nil {
				tc.prepare(provider)
			}
			handle := newRuntimeNudgeEffectHandle(t, provider)

			result, err := handle.Nudge(t.Context(), NudgeRequest{
				Text: "inspect the failed build",
				Effect: &runtime.NudgeEffectContract{
					OperationID:            "command-a",
					ExpectedLaunchIdentity: tc.expectedLaunch,
					InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
				},
			})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("Nudge error = %v, want %v", err, tc.wantErr)
			}
			if result.Delivered || result.Effect == nil || result.Effect.Stage != runtime.NudgeEffectStageNotEntered || result.Effect.Completion != runtime.NudgeEffectCompletionNotCompleted {
				t.Fatalf("refused result = %#v, want definite pre-entry evidence", result)
			}
			if got := provider.CountCalls("NudgeEffect", "session-a"); got != 0 {
				t.Fatalf("native provider entries = %d, want 0", got)
			}
		})
	}
}

func TestRuntimeHandleClassifiedNudgePreservesAmbiguousProviderEvidence(t *testing.T) {
	provider := runtime.NewFake()
	startFakeNudgeEffectTarget(t, provider)
	provider.NudgeEffectResults["session-a"] = runtime.NudgeEffectResult{
		Stage:      runtime.NudgeEffectStageMayHaveEntered,
		Completion: runtime.NudgeEffectCompletionUnknown,
	}
	provider.NudgeEffectErrors["session-a"] = runtime.ErrNudgeDeliveryUnknown
	handle := newRuntimeNudgeEffectHandle(t, provider)

	result, err := handle.Nudge(t.Context(), NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if !errors.Is(err, runtime.ErrNudgeDeliveryUnknown) {
		t.Fatalf("Nudge error = %v, want delivery unknown", err)
	}
	if result.Delivered || result.Effect == nil || *result.Effect != provider.NudgeEffectResults["session-a"] {
		t.Fatalf("ambiguous result = %#v, want preserved provider evidence", result)
	}
	if got := provider.CountCalls("NudgeEffect", "session-a"); got != 1 {
		t.Fatalf("native provider entries = %d, want 1", got)
	}
}

func TestRuntimeHandleClassifiedNudgeTreatsInvalidProviderEvidenceAsAmbiguous(t *testing.T) {
	provider := runtime.NewFake()
	startFakeNudgeEffectTarget(t, provider)
	provider.NudgeEffectResults["session-a"] = runtime.NudgeEffectResult{}
	handle := newRuntimeNudgeEffectHandle(t, provider)

	result, err := handle.Nudge(t.Context(), NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if !errors.Is(err, runtime.ErrNudgeDeliveryUnknown) || !errors.Is(err, runtime.ErrNudgeEffectInvalid) {
		t.Fatalf("Nudge error = %v, want invalid delivery-unknown", err)
	}
	if result.Delivered || result.Effect == nil || result.Effect.Stage != runtime.NudgeEffectStageMayHaveEntered || result.Effect.Completion != runtime.NudgeEffectCompletionUnknown {
		t.Fatalf("invalid provider result = %#v, want conservative ambiguity", result)
	}
}

func TestRuntimeHandleClassifiedNudgeHonorsCanceledContextBeforeNativeEntry(t *testing.T) {
	provider := runtime.NewFake()
	startFakeNudgeEffectTarget(t, provider)
	handle := newRuntimeNudgeEffectHandle(t, provider)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	result, err := handle.Nudge(ctx, NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Nudge error = %v, want context canceled", err)
	}
	if result.Delivered || result.Effect == nil || result.Effect.Stage != runtime.NudgeEffectStageNotEntered {
		t.Fatalf("canceled result = %#v, want definite pre-entry evidence", result)
	}
	if got := provider.CountCalls("NudgeEffect", "session-a"); got != 0 {
		t.Fatalf("native provider entries = %d, want 0", got)
	}
}

func TestRuntimeHandleClassifiedNudgeRejectsLegacyOnlyProvider(t *testing.T) {
	provider := runtime.NewFake()
	startFakeNudgeEffectTarget(t, provider)
	legacyOnly := struct{ runtime.Provider }{Provider: provider}
	handle := newRuntimeNudgeEffectHandle(t, legacyOnly)

	result, err := handle.Nudge(t.Context(), NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if !errors.Is(err, runtime.ErrNudgeEffectUnsupported) {
		t.Fatalf("Nudge error = %v, want classified-effect unsupported", err)
	}
	if result.Delivered || result.Effect == nil || result.Effect.Stage != runtime.NudgeEffectStageNotEntered {
		t.Fatalf("unsupported result = %#v, want definite pre-entry evidence", result)
	}
	if got := provider.CountCalls("Nudge", "session-a"); got != 0 {
		t.Fatalf("legacy fallback entries = %d, want 0", got)
	}
}

func TestSessionHandleClassifiedNudgeDoesNotFallBackToLegacyManagerPath(t *testing.T) {
	handle, _, provider, _ := newTestSessionHandle(t, SessionSpec{
		Profile:  ProfileClaudeTmuxCLI,
		Template: "probe",
		Title:    "Probe",
		Command:  "claude",
		WorkDir:  t.TempDir(),
		Provider: "claude",
	})

	result, err := handle.Nudge(t.Context(), NudgeRequest{
		Text: "inspect the failed build",
		Effect: &runtime.NudgeEffectContract{
			OperationID:            "command-a",
			ExpectedLaunchIdentity: "launch-a",
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if !errors.Is(err, runtime.ErrNudgeEffectUnsupported) {
		t.Fatalf("Nudge error = %v, want classified-effect unsupported", err)
	}
	if result.Delivered || result.Effect == nil || result.Effect.Stage != runtime.NudgeEffectStageNotEntered {
		t.Fatalf("unsupported result = %#v, want definite pre-entry evidence", result)
	}
	if handle.currentSessionID() != "" {
		t.Fatalf("classified refusal created session %q", handle.currentSessionID())
	}
	for _, call := range provider.SnapshotCalls() {
		if call.Method == "Nudge" || call.Method == "NudgeEffect" {
			t.Fatalf("classified refusal reached provider entry: %#v", call)
		}
	}
}

func startFakeNudgeEffectTarget(t *testing.T, provider *runtime.Fake) {
	t.Helper()
	if err := provider.Start(t.Context(), "session-a", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := provider.SetMeta("session-a", "GC_INSTANCE_TOKEN", "launch-a"); err != nil {
		t.Fatalf("SetMeta launch identity: %v", err)
	}
}

func newRuntimeNudgeEffectHandle(t *testing.T, provider runtime.Provider) *RuntimeHandle {
	t.Helper()
	handle, err := NewRuntimeHandle(RuntimeHandleConfig{
		Provider:    provider,
		SessionName: "session-a",
	})
	if err != nil {
		t.Fatalf("NewRuntimeHandle: %v", err)
	}
	return handle
}
