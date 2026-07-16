package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"time"
)

// ErrCommandRetryTransition reports missing, conflicting, or unavailable
// authority evidence for a definite-non-entry retry transition.
var ErrCommandRetryTransition = errors.New("durable nudge command retry transition is unverified")

// CommandRetryTransitionIntent is the independent write-ahead record for one
// exact in-flight-to-pending transition. The previous claim receipt remains
// executable authority until Finalize atomically consumes it after the command
// store commit and retains the retry receipt.
type CommandRetryTransitionIntent struct {
	Store                       CommandStoreBinding
	RepositoryBeforeRevision    uint64
	RepositoryRevision          uint64
	RepositorySequenceHighWater uint64
	CommandID                   string
	Sequence                    uint64
	Partition                   TrustedCityPartition
	BeforeCommandDigest         [sha256.Size]byte
	AfterCommandDigest          [sha256.Size]byte
	Claim                       CommandClaim
	Retry                       CommandRetry
	ObservedAt                  time.Time
	ProviderStage               ProviderStage
	Completion                  CompletionState
}

// CommandRetryTransitionCommit is independently observable command-store
// evidence for finalizing a prepared retry. It deliberately omits the old
// claim: the authority journal must load that exact one-shot receipt instead
// of trusting a reconstruction from the v1 command wire.
type CommandRetryTransitionCommit struct {
	Store                    CommandStoreBinding
	RepositoryRevision       uint64
	CommandID                string
	Sequence                 uint64
	Partition                TrustedCityPartition
	AfterCommandDigest       [sha256.Size]byte
	AttemptID                string
	ObservedAt               time.Time
	ProviderStage            ProviderStage
	Completion               CompletionState
	EffectRepositoryRevision uint64
	EffectSequenceHighWater  uint64
}

// CommandRetryTransitionReceipt is immutable historical evidence that the
// exact prior attempt definitely did not enter the provider and its one-shot
// claim receipt was consumed. Receipts are keyed by command and attempt.
type CommandRetryTransitionReceipt struct {
	Store                    CommandStoreBinding
	RepositoryRevision       uint64
	CommandID                string
	Sequence                 uint64
	Partition                TrustedCityPartition
	AfterCommandDigest       [sha256.Size]byte
	Claim                    CommandClaim
	Retry                    CommandRetry
	ObservedAt               time.Time
	ProviderStage            ProviderStage
	Completion               CompletionState
	EffectRepositoryRevision uint64
	EffectSequenceHighWater  uint64
}

// CommandRetryReceiptDisposition reports whether finalization consumed the
// exact claim receipt now or recovered a previously finalized retry.
type CommandRetryReceiptDisposition string

const (
	// CommandRetryReceiptFinalized created the exact durable retry receipt.
	CommandRetryReceiptFinalized CommandRetryReceiptDisposition = "finalized"
	// CommandRetryReceiptAlreadyFinalized found the same retained receipt.
	CommandRetryReceiptAlreadyFinalized CommandRetryReceiptDisposition = "already_finalized"
)

// TrustedCommandRetryTransitionAuthority owns retry preparation independently
// of the command store. Finalize must atomically consume the exact prior claim
// receipt and retain the exact historical retry receipt; neither step may
// infer safety from lease expiry or command-store state alone.
type TrustedCommandRetryTransitionAuthority interface {
	PrepareCommandRetryTransition(context.Context, CommandRetryTransitionIntent) error
	ReleaseCommandRetryTransitionWriter(context.Context, CommandRetryTransitionIntent) error
	AbortCommandRetryTransition(context.Context, CommandRetryTransitionIntent) error
	FinalizeCommandRetryTransition(context.Context, CommandRetryTransitionCommit) (CommandRetryReceiptDisposition, error)
}

func commandRetryTransitionIntentFor(state CommandRepositoryState, before, after Command, observedAt time.Time, partition TrustedCityPartition) (CommandRetryTransitionIntent, error) {
	if validateCommandRepositoryBinding(state.Store) != nil || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision ||
		state.Revision == ^uint64(0) || !partition.valid() {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: repository before-state is invalid", ErrCommandRetryTransition)
	}
	if before.Store != state.Store || after.Store != state.Store || before.ID != after.ID ||
		before.Order.Sequence == 0 || before.Order.Sequence != after.Order.Sequence ||
		before.Order.Sequence > state.SequenceHighWater || before.Order.Revision == 0 || before.Order.Revision > state.Revision ||
		after.Order.Revision != state.Revision+1 || before.State != CommandStateInFlight || before.Claim == nil || before.Retry == nil || before.Terminal != nil ||
		after.State != CommandStatePending || after.Claim != nil || after.Retry == nil || after.Terminal != nil {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: in-flight-to-pending states are inconsistent", ErrCommandRetryTransition)
	}
	if err := validateCommandTime("retry observation time", observedAt); err != nil ||
		observedAt.Before(before.Claim.ClaimedAt) || !observedAt.Before(before.ExpiresAt) {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: observation time is outside the exact attempt", ErrCommandRetryTransition)
	}
	if after.Retry.NextEligibleAt == nil || !after.Retry.NextEligibleAt.After(observedAt) || !after.Retry.NextEligibleAt.Before(after.ExpiresAt) ||
		(after.Retry.ErrorClass != CommandErrorClassProviderBusy && after.Retry.ErrorClass != CommandErrorClassProviderUnavailable) {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: after-state lacks retryable definite-non-entry evidence", ErrCommandRetryTransition)
	}
	if !sameCommandRetryAttemptIdentity(*before.Retry, *after.Retry) || before.Retry.AttemptCount != after.Retry.AttemptCount ||
		!retryMatchesClaim(*before.Retry, *before.Claim) {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: exact attempt evidence changed", ErrCommandRetryTransition)
	}
	if err := validateCommandIndexUpdate(before, after); err != nil {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: retry transition is invalid: %w", ErrCommandRetryTransition, err)
	}
	beforeWire, err := EncodeCommandV1(before)
	if err != nil {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: encoding before-state: %w", ErrCommandRetryTransition, err)
	}
	afterWire, err := EncodeCommandV1(after)
	if err != nil {
		return CommandRetryTransitionIntent{}, fmt.Errorf("%w: encoding after-state: %w", ErrCommandRetryTransition, err)
	}
	intent := CommandRetryTransitionIntent{
		Store: state.Store, RepositoryBeforeRevision: state.Revision, RepositoryRevision: after.Order.Revision,
		RepositorySequenceHighWater: state.SequenceHighWater, CommandID: after.ID, Sequence: after.Order.Sequence,
		Partition: partition, BeforeCommandDigest: sha256.Sum256(beforeWire), AfterCommandDigest: sha256.Sum256(afterWire),
		Claim: *before.Claim, Retry: cloneCommandRetry(*after.Retry), ObservedAt: observedAt,
		ProviderStage: ProviderStageNotEntered, Completion: CompletionStateNotCompleted,
	}
	if err := validateCommandRetryTransitionIntent(intent); err != nil {
		return CommandRetryTransitionIntent{}, err
	}
	return intent, nil
}

func commandRetryTransitionCommitFor(state CommandRepositoryState, command Command, request CommandRetryRequest, partition TrustedCityPartition) (CommandRetryTransitionCommit, error) {
	if validateCommandRepositoryBinding(state.Store) != nil || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision ||
		command.Store != state.Store || command.State != CommandStatePending || command.Claim != nil || command.Retry == nil || command.Terminal != nil ||
		command.Order.Revision == 0 || command.Order.Revision > state.Revision || command.Order.Sequence == 0 ||
		command.Order.Sequence > state.SequenceHighWater || !partition.valid() || !commandPendingRetryMatches(command, request) {
		return CommandRetryTransitionCommit{}, fmt.Errorf("%w: pending commit state is inconsistent", ErrCommandRetryTransition)
	}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return CommandRetryTransitionCommit{}, fmt.Errorf("%w: encoding committed retry: %w", ErrCommandRetryTransition, err)
	}
	commit := CommandRetryTransitionCommit{
		Store: state.Store, RepositoryRevision: command.Order.Revision, CommandID: command.ID, Sequence: command.Order.Sequence,
		Partition: partition, AfterCommandDigest: sha256.Sum256(wire), AttemptID: request.AttemptID,
		ObservedAt: request.ObservedAt, ProviderStage: request.ProviderStage, Completion: request.Completion,
		EffectRepositoryRevision: state.Revision, EffectSequenceHighWater: state.SequenceHighWater,
	}
	if err := validateCommandRetryTransitionCommit(commit); err != nil {
		return CommandRetryTransitionCommit{}, err
	}
	return commit, nil
}

func commandRetryTransitionReceiptFor(intent CommandRetryTransitionIntent, commit CommandRetryTransitionCommit) (CommandRetryTransitionReceipt, error) {
	if !retryIntentMatchesCommit(intent, commit) {
		return CommandRetryTransitionReceipt{}, fmt.Errorf("%w: commit does not match its exact preparation", ErrCommandRetryTransition)
	}
	receipt := CommandRetryTransitionReceipt{
		Store: commit.Store, RepositoryRevision: commit.RepositoryRevision, CommandID: commit.CommandID, Sequence: commit.Sequence,
		Partition: commit.Partition, AfterCommandDigest: commit.AfterCommandDigest, Claim: intent.Claim,
		Retry: cloneCommandRetry(intent.Retry), ObservedAt: commit.ObservedAt,
		ProviderStage: commit.ProviderStage, Completion: commit.Completion,
		EffectRepositoryRevision: commit.EffectRepositoryRevision, EffectSequenceHighWater: commit.EffectSequenceHighWater,
	}
	if err := validateCommandRetryTransitionReceipt(receipt); err != nil {
		return CommandRetryTransitionReceipt{}, err
	}
	return receipt, nil
}

func validateCommandRetryTransitionIntent(intent CommandRetryTransitionIntent) error {
	if validateCommandRepositoryBinding(intent.Store) != nil || !intent.Partition.valid() ||
		intent.RepositoryBeforeRevision == 0 || intent.RepositoryBeforeRevision == ^uint64(0) ||
		intent.RepositoryRevision != intent.RepositoryBeforeRevision+1 || intent.Sequence == 0 ||
		intent.Sequence > intent.RepositorySequenceHighWater || intent.RepositorySequenceHighWater > intent.RepositoryBeforeRevision ||
		intent.BeforeCommandDigest == ([sha256.Size]byte{}) || intent.AfterCommandDigest == ([sha256.Size]byte{}) ||
		intent.ProviderStage != ProviderStageNotEntered || intent.Completion != CompletionStateNotCompleted ||
		validateCommandIdentity("retry transition command id", intent.CommandID) != nil ||
		validatePersistedCommandClaim(intent.CommandID, intent.Claim) != nil ||
		validateCommandTime("retry transition observed_at", intent.ObservedAt) != nil ||
		intent.ObservedAt.Before(intent.Claim.ClaimedAt) || intent.Retry.NextEligibleAt == nil ||
		!intent.Retry.NextEligibleAt.After(intent.ObservedAt) ||
		(intent.Retry.ErrorClass != CommandErrorClassProviderBusy && intent.Retry.ErrorClass != CommandErrorClassProviderUnavailable) ||
		!retryMatchesClaim(intent.Retry, intent.Claim) {
		return fmt.Errorf("%w: retry transition intent is invalid", ErrCommandRetryTransition)
	}
	return nil
}

func validateCommandRetryTransitionCommit(commit CommandRetryTransitionCommit) error {
	if validateCommandRepositoryBinding(commit.Store) != nil || !commit.Partition.valid() || commit.RepositoryRevision == 0 ||
		commit.Sequence == 0 || commit.AfterCommandDigest == ([sha256.Size]byte{}) ||
		commit.EffectRepositoryRevision < commit.RepositoryRevision || commit.EffectSequenceHighWater < commit.Sequence ||
		commit.EffectSequenceHighWater > commit.EffectRepositoryRevision || commit.ProviderStage != ProviderStageNotEntered ||
		commit.Completion != CompletionStateNotCompleted || validateCommandIdentity("retry commit command id", commit.CommandID) != nil ||
		validateCommandIdentity("retry commit attempt id", commit.AttemptID) != nil ||
		validateCommandTime("retry commit observed_at", commit.ObservedAt) != nil {
		return fmt.Errorf("%w: retry transition commit is invalid", ErrCommandRetryTransition)
	}
	return nil
}

func validateCommandRetryTransitionReceipt(receipt CommandRetryTransitionReceipt) error {
	if validateCommandRepositoryBinding(receipt.Store) != nil || !receipt.Partition.valid() || receipt.RepositoryRevision == 0 ||
		receipt.Sequence == 0 || receipt.AfterCommandDigest == ([sha256.Size]byte{}) ||
		receipt.EffectRepositoryRevision < receipt.RepositoryRevision || receipt.EffectSequenceHighWater < receipt.Sequence ||
		receipt.EffectSequenceHighWater > receipt.EffectRepositoryRevision || receipt.ProviderStage != ProviderStageNotEntered ||
		receipt.Completion != CompletionStateNotCompleted || validateCommandIdentity("retry receipt command id", receipt.CommandID) != nil ||
		validatePersistedCommandClaim(receipt.CommandID, receipt.Claim) != nil ||
		validateCommandTime("retry receipt observed_at", receipt.ObservedAt) != nil || receipt.ObservedAt.Before(receipt.Claim.ClaimedAt) ||
		receipt.Retry.NextEligibleAt == nil || !receipt.Retry.NextEligibleAt.After(receipt.ObservedAt) ||
		(receipt.Retry.ErrorClass != CommandErrorClassProviderBusy && receipt.Retry.ErrorClass != CommandErrorClassProviderUnavailable) ||
		!retryMatchesClaim(receipt.Retry, receipt.Claim) {
		return fmt.Errorf("%w: retry transition receipt is invalid", ErrCommandRetryTransition)
	}
	return nil
}

func retryMatchesClaim(retry CommandRetry, claim CommandClaim) bool {
	return retry.ClaimID == claim.ID && retry.OperationID == claim.OperationID && retry.AttemptID == claim.AttemptID &&
		retry.BoundLaunchIdentity == claim.BoundLaunchIdentity && retry.AuthorizationDecisionID == claim.AuthorizationDecisionID &&
		retry.AuthorizationPolicyVersion == claim.AuthorizationPolicyVersion && retry.LastAttemptAt.Equal(claim.ClaimedAt)
}

func sameCommandRetryAttemptIdentity(left, right CommandRetry) bool {
	return left.AttemptCount == right.AttemptCount && left.LastAttemptAt.Equal(right.LastAttemptAt) &&
		left.ClaimID == right.ClaimID && left.OperationID == right.OperationID && left.AttemptID == right.AttemptID &&
		left.BoundLaunchIdentity == right.BoundLaunchIdentity && left.AuthorizationDecisionID == right.AuthorizationDecisionID &&
		left.AuthorizationPolicyVersion == right.AuthorizationPolicyVersion
}

func cloneCommandRetry(retry CommandRetry) CommandRetry {
	if retry.NextEligibleAt != nil {
		next := *retry.NextEligibleAt
		retry.NextEligibleAt = &next
	}
	return retry
}

func sameCommandRetryTransitionIntent(left, right CommandRetryTransitionIntent) bool {
	return left.Store == right.Store && left.RepositoryBeforeRevision == right.RepositoryBeforeRevision &&
		left.RepositoryRevision == right.RepositoryRevision && left.RepositorySequenceHighWater == right.RepositorySequenceHighWater &&
		left.CommandID == right.CommandID && left.Sequence == right.Sequence && left.Partition == right.Partition &&
		left.BeforeCommandDigest == right.BeforeCommandDigest && left.AfterCommandDigest == right.AfterCommandDigest &&
		commandClaimsEqual(left.Claim, right.Claim) && sameCommandRetry(left.Retry, right.Retry) &&
		left.ObservedAt.Equal(right.ObservedAt) && left.ProviderStage == right.ProviderStage && left.Completion == right.Completion
}

func sameCommandRetry(left, right CommandRetry) bool {
	if !sameCommandRetryAttemptIdentity(left, right) || left.ErrorClass != right.ErrorClass || left.ErrorDetail != right.ErrorDetail {
		return false
	}
	if left.NextEligibleAt == nil || right.NextEligibleAt == nil {
		return left.NextEligibleAt == nil && right.NextEligibleAt == nil
	}
	return left.NextEligibleAt.Equal(*right.NextEligibleAt)
}

func retryIntentMatchesCommit(intent CommandRetryTransitionIntent, commit CommandRetryTransitionCommit) bool {
	return intent.Store == commit.Store && intent.RepositoryRevision == commit.RepositoryRevision &&
		intent.CommandID == commit.CommandID && intent.Sequence == commit.Sequence && intent.Partition == commit.Partition &&
		intent.AfterCommandDigest == commit.AfterCommandDigest && intent.Claim.AttemptID == commit.AttemptID &&
		intent.ObservedAt.Equal(commit.ObservedAt) && intent.ProviderStage == commit.ProviderStage && intent.Completion == commit.Completion &&
		commit.EffectRepositoryRevision >= intent.RepositoryRevision &&
		commit.EffectSequenceHighWater >= intent.RepositorySequenceHighWater
}

func retryReceiptMatchesCommit(receipt CommandRetryTransitionReceipt, commit CommandRetryTransitionCommit) bool {
	return receipt.Store == commit.Store && receipt.RepositoryRevision == commit.RepositoryRevision &&
		receipt.CommandID == commit.CommandID && receipt.Sequence == commit.Sequence && receipt.Partition == commit.Partition &&
		receipt.AfterCommandDigest == commit.AfterCommandDigest && receipt.Claim.AttemptID == commit.AttemptID &&
		receipt.ObservedAt.Equal(commit.ObservedAt) && receipt.ProviderStage == commit.ProviderStage && receipt.Completion == commit.Completion &&
		commit.EffectRepositoryRevision >= receipt.EffectRepositoryRevision &&
		commit.EffectSequenceHighWater >= receipt.EffectSequenceHighWater
}

func prepareCommandRetryTransition(ctx context.Context, authority TrustedCommandRetryTransitionAuthority, intent CommandRetryTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: retry transition authority is required", ErrCommandRetryTransition)
	}
	if err := authority.PrepareCommandRetryTransition(ctx, intent); err != nil {
		return fmt.Errorf("%w: preparing exact retry transition: %w", ErrCommandRetryTransition, err)
	}
	return nil
}

func abortCommandRetryTransition(ctx context.Context, authority TrustedCommandRetryTransitionAuthority, intent CommandRetryTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: retry transition authority is required", ErrCommandRetryTransition)
	}
	if err := authority.AbortCommandRetryTransition(ctx, intent); err != nil {
		return fmt.Errorf("%w: aborting exact retry transition: %w", ErrCommandRetryTransition, err)
	}
	return nil
}

func releaseCommandRetryTransitionWriter(ctx context.Context, authority TrustedCommandRetryTransitionAuthority, intent CommandRetryTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: retry transition authority is required", ErrCommandRetryTransition)
	}
	if err := authority.ReleaseCommandRetryTransitionWriter(ctx, intent); err != nil {
		return fmt.Errorf("%w: releasing retry transition writer: %w", ErrCommandRetryTransition, err)
	}
	return nil
}
