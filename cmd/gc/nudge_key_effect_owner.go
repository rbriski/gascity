package main

import (
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

const (
	nudgeProviderAmbiguousDetail       = "provider may have accepted the nudge without a durable result"
	nudgeProviderRejectedDetail        = "provider definitively rejected the nudge"
	nudgeProviderInvalidEvidenceDetail = "provider returned incomplete or contradictory delivery evidence"
)

// nudgeProviderCompletion is the closed durable projection of one classified
// worker nudge result. A nonterminal value is proven pre-entry evidence that
// the owner must park until an explicit recovery policy exists.
type nudgeProviderCompletion struct {
	terminal      bool
	actionResult  nudgequeue.CommandActionResult
	errorClass    nudgequeue.CommandErrorClass
	detail        string
	providerStage nudgequeue.ProviderStage
	completion    nudgequeue.CompletionState
}

func mapNudgeProviderCompletion(result worker.NudgeResult, effectErr error) nudgeProviderCompletion {
	if result.Effect == nil {
		return invalidNudgeProviderCompletion()
	}
	effect := *result.Effect
	transportAccepted := effect.Stage == runtime.NudgeEffectStageAccepted &&
		effect.Completion == runtime.NudgeEffectCompletionCompleted
	if effect.Validate(effectErr) != nil || result.Delivered != transportAccepted {
		return invalidNudgeProviderCompletion()
	}

	switch effect.Stage {
	case runtime.NudgeEffectStageNotEntered:
		return nudgeProviderCompletion{
			providerStage: nudgequeue.ProviderStageNotEntered,
			completion:    nudgequeue.CompletionStateNotCompleted,
		}
	case runtime.NudgeEffectStageRejected:
		return nudgeProviderCompletion{
			terminal:      true,
			actionResult:  nudgequeue.CommandActionResultRejected,
			errorClass:    nudgequeue.CommandErrorClassProviderRejected,
			detail:        nudgeProviderRejectedDetail,
			providerStage: nudgequeue.ProviderStageRejected,
			completion:    nudgequeue.CompletionStateNotCompleted,
		}
	case runtime.NudgeEffectStageMayHaveEntered:
		return nudgeProviderCompletion{
			terminal:      true,
			actionResult:  nudgequeue.CommandActionResultDeliveryUnknown,
			errorClass:    nudgequeue.CommandErrorClassProviderAmbiguous,
			detail:        nudgeProviderAmbiguousDetail,
			providerStage: nudgequeue.ProviderStageMayHaveEntered,
			completion:    nudgequeue.CompletionStateUnknown,
		}
	case runtime.NudgeEffectStageAccepted:
		actionResult := nudgequeue.CommandActionResultInjectedUnconfirmed
		if effect.ConsumptionConfirmed {
			actionResult = nudgequeue.CommandActionResultDelivered
		}
		return nudgeProviderCompletion{
			terminal:      true,
			actionResult:  actionResult,
			providerStage: nudgequeue.ProviderStageAccepted,
			completion:    nudgequeue.CompletionStateCompleted,
		}
	default:
		return invalidNudgeProviderCompletion()
	}
}

func invalidNudgeProviderCompletion() nudgeProviderCompletion {
	return nudgeProviderCompletion{
		terminal:      true,
		actionResult:  nudgequeue.CommandActionResultDeliveryUnknown,
		errorClass:    nudgequeue.CommandErrorClassProviderAmbiguous,
		detail:        nudgeProviderInvalidEvidenceDetail,
		providerStage: nudgequeue.ProviderStageMayHaveEntered,
		completion:    nudgequeue.CompletionStateUnknown,
	}
}
