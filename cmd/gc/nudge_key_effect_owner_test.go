package main

import (
	"errors"
	"strings"
	"testing"

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
