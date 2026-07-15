package main

import (
	"errors"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

var errNudgeEffectTargetChanged = errors.New("durable nudge target changed")

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

// nudgeEffectTarget is one exact persisted session/runtime identity reread. It
// carries only the fields needed to reject stale command generations before a
// provider can be entered.
type nudgeEffectTarget struct {
	sessionID            string
	sessionName          string
	intentGeneration     uint64
	continuationIdentity string
	launchIdentity       string
	closed               bool
}

func selectNudgeEffectLaunch(command nudgequeue.Command, target nudgeEffectTarget) (string, error) {
	if target.closed || target.sessionID == "" || target.sessionName == "" ||
		target.launchIdentity == "" || target.sessionID != command.Target.SessionID ||
		target.intentGeneration != command.Target.IntentGeneration {
		return "", errNudgeEffectTargetChanged
	}

	switch command.Target.Policy {
	case nudgequeue.TargetPolicyContinuation:
		if target.continuationIdentity == "" || target.continuationIdentity != command.Target.ContinuationIdentity {
			return "", errNudgeEffectTargetChanged
		}
	case nudgequeue.TargetPolicyExactLaunch:
		if target.launchIdentity != command.Target.LaunchIdentity {
			return "", errNudgeEffectTargetChanged
		}
	default:
		return "", errNudgeEffectTargetChanged
	}
	if command.Binding != nil && command.Binding.LaunchIdentity != target.launchIdentity {
		return "", errNudgeEffectTargetChanged
	}
	return target.launchIdentity, nil
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
