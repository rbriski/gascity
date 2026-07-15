package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestMapNudgeProviderCompletionIsTotalAndConservative(t *testing.T) {
	providerErr := errors.New("provider detail must not cross the durable boundary")
	tests := []struct {
		name   string
		result worker.NudgeResult
		err    error
		want   nudgeProviderCompletion
	}{
		{
			name: "accepted and consumption confirmed is delivered",
			result: worker.NudgeResult{Delivered: true, Effect: &runtime.NudgeEffectResult{
				Stage:                runtime.NudgeEffectStageAccepted,
				Completion:           runtime.NudgeEffectCompletionCompleted,
				ConsumptionConfirmed: true,
			}},
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultDelivered,
				providerStage: nudgequeue.ProviderStageAccepted,
				completion:    nudgequeue.CompletionStateCompleted,
			},
		},
		{
			name: "accepted without consumption proof is injected unconfirmed",
			result: worker.NudgeResult{Delivered: true, Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageAccepted,
				Completion: runtime.NudgeEffectCompletionCompleted,
			}},
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultInjectedUnconfirmed,
				providerStage: nudgequeue.ProviderStageAccepted,
				completion:    nudgequeue.CompletionStateCompleted,
			},
		},
		{
			name: "may have entered is terminal delivery unknown",
			result: worker.NudgeResult{Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageMayHaveEntered,
				Completion: runtime.NudgeEffectCompletionUnknown,
			}},
			err: providerErr,
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultDeliveryUnknown,
				errorClass:    nudgequeue.CommandErrorClassProviderAmbiguous,
				detail:        nudgeProviderAmbiguousDetail,
				providerStage: nudgequeue.ProviderStageMayHaveEntered,
				completion:    nudgequeue.CompletionStateUnknown,
			},
		},
		{
			name: "definite rejection is terminal rejected",
			result: worker.NudgeResult{Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageRejected,
				Completion: runtime.NudgeEffectCompletionNotCompleted,
			}},
			err: providerErr,
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultRejected,
				errorClass:    nudgequeue.CommandErrorClassProviderRejected,
				detail:        nudgeProviderRejectedDetail,
				providerStage: nudgequeue.ProviderStageRejected,
				completion:    nudgequeue.CompletionStateNotCompleted,
			},
		},
		{
			name: "definite pre-entry result stays parked",
			result: worker.NudgeResult{Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageNotEntered,
				Completion: runtime.NudgeEffectCompletionNotCompleted,
			}},
			err: providerErr,
			want: nudgeProviderCompletion{
				providerStage: nudgequeue.ProviderStageNotEntered,
				completion:    nudgequeue.CompletionStateNotCompleted,
			},
		},
		{
			name: "invalid evidence fails terminally ambiguous",
			result: worker.NudgeResult{Delivered: true, Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageAccepted,
				Completion: runtime.NudgeEffectCompletionCompleted,
			}},
			err: providerErr,
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultDeliveryUnknown,
				errorClass:    nudgequeue.CommandErrorClassProviderAmbiguous,
				detail:        nudgeProviderInvalidEvidenceDetail,
				providerStage: nudgequeue.ProviderStageMayHaveEntered,
				completion:    nudgequeue.CompletionStateUnknown,
			},
		},
		{
			name: "missing evidence fails terminally ambiguous",
			want: nudgeProviderCompletion{
				terminal:      true,
				actionResult:  nudgequeue.CommandActionResultDeliveryUnknown,
				errorClass:    nudgequeue.CommandErrorClassProviderAmbiguous,
				detail:        nudgeProviderInvalidEvidenceDetail,
				providerStage: nudgequeue.ProviderStageMayHaveEntered,
				completion:    nudgequeue.CompletionStateUnknown,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := mapNudgeProviderCompletion(test.result, test.err)
			if got != test.want {
				t.Fatalf("completion = %#v, want %#v", got, test.want)
			}
			if strings.Contains(got.detail, providerErr.Error()) {
				t.Fatalf("durable detail leaked provider error: %q", got.detail)
			}
		})
	}
}

func TestSelectNudgeEffectLaunchRequiresExactCurrentTarget(t *testing.T) {
	continuation := nudgequeue.Command{Target: nudgequeue.CommandTarget{
		SessionID:            "session-1",
		IntentGeneration:     7,
		ContinuationIdentity: "conversation-1",
		Policy:               nudgequeue.TargetPolicyContinuation,
	}}
	exact := nudgequeue.Command{Target: nudgequeue.CommandTarget{
		SessionID:        "session-1",
		IntentGeneration: 7,
		LaunchIdentity:   "launch-1",
		Policy:           nudgequeue.TargetPolicyExactLaunch,
	}}
	current := nudgeEffectTarget{
		sessionID:            "session-1",
		sessionName:          "city--worker",
		intentGeneration:     7,
		continuationIdentity: "conversation-1",
		launchIdentity:       "launch-1",
	}

	for name, command := range map[string]nudgequeue.Command{
		"continuation": continuation,
		"exact launch": exact,
	} {
		t.Run(name, func(t *testing.T) {
			launch, err := selectNudgeEffectLaunch(command, current)
			if err != nil || launch != current.launchIdentity {
				t.Fatalf("selectNudgeEffectLaunch = %q, %v; want %q", launch, err, current.launchIdentity)
			}
		})
	}

	bound := continuation
	bound.Binding = &nudgequeue.CommandBinding{LaunchIdentity: current.launchIdentity, BoundAt: testNudgeEffectTime()}
	if launch, err := selectNudgeEffectLaunch(bound, current); err != nil || launch != current.launchIdentity {
		t.Fatalf("bound continuation = %q, %v; want current launch", launch, err)
	}

	tests := map[string]func(*nudgequeue.Command, *nudgeEffectTarget){
		"closed session":                func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.closed = true },
		"foreign session":               func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.sessionID = "session-2" },
		"missing runtime name":          func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.sessionName = "" },
		"stale intent generation":       func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.intentGeneration++ },
		"changed continuation identity": func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.continuationIdentity = "conversation-2" },
		"missing launch identity":       func(_ *nudgequeue.Command, target *nudgeEffectTarget) { target.launchIdentity = "" },
		"changed bound launch": func(command *nudgequeue.Command, _ *nudgeEffectTarget) {
			command.Binding = &nudgequeue.CommandBinding{LaunchIdentity: "launch-2", BoundAt: testNudgeEffectTime()}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			command := continuation
			target := current
			mutate(&command, &target)
			if launch, err := selectNudgeEffectLaunch(command, target); !errors.Is(err, errNudgeEffectTargetChanged) || launch != "" {
				t.Fatalf("selectNudgeEffectLaunch = %q, %v; want target-changed refusal", launch, err)
			}
		})
	}

	changedExact := current
	changedExact.launchIdentity = "launch-2"
	if launch, err := selectNudgeEffectLaunch(exact, changedExact); !errors.Is(err, errNudgeEffectTargetChanged) || launch != "" {
		t.Fatalf("changed exact launch = %q, %v; want target-changed refusal", launch, err)
	}
}

func testNudgeEffectTime() time.Time {
	return time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
}
