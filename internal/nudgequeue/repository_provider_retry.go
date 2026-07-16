package nudgequeue

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ErrCommandProviderRetryInvalid reports malformed or contradictory evidence
// for an attempt claimed to have definitely not entered its provider.
var ErrCommandProviderRetryInvalid = errors.New("invalid durable nudge provider retry")

// CommandRetryDisposition is the total result of one exact definite-non-entry
// transition.
type CommandRetryDisposition string

const (
	// CommandRetryRecorded committed the in-flight-to-pending transition and
	// consumed the exact one-shot claim receipt.
	CommandRetryRecorded CommandRetryDisposition = "recorded"
	// CommandRetryAlreadyRecorded recovered the same durable transition.
	CommandRetryAlreadyRecorded CommandRetryDisposition = "already_recorded"
	// CommandRetryStale means another attempt or terminal state owns the command.
	CommandRetryStale CommandRetryDisposition = "stale"
)

// CommandRetryRequest carries provider-observed definite-non-entry evidence.
// It cannot choose command payload, target, authorization, or prior claim data.
type CommandRetryRequest struct {
	CommandID      string
	ClaimID        string
	OperationID    string
	AttemptID      string
	ObservedAt     time.Time
	NextEligibleAt time.Time
	ErrorClass     CommandErrorClass
	Detail         string
	ProviderStage  ProviderStage
	Completion     CompletionState
}

// CommandRetryResult returns the authoritative pending command observed or
// committed by [CommandRepository.RetryProviderAttempt].
type CommandRetryResult struct {
	Disposition            CommandRetryDisposition
	Command                Command
	retryTransitionWitness commandRetryTransitionWitness
}

type commandRetryTransitionWitness struct {
	digest [sha256.Size]byte
}

// HasRetryTransitionWitness reports whether this call committed or exactly
// recovered the returned definite-non-entry transition.
func (r CommandRetryResult) HasRetryTransitionWitness() bool {
	if (r.Disposition != CommandRetryRecorded && r.Disposition != CommandRetryAlreadyRecorded) ||
		r.Command.State != CommandStatePending || r.Command.Retry == nil {
		return false
	}
	wire, err := EncodeCommandV1(r.Command)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(wire)
	return subtle.ConstantTimeCompare(r.retryTransitionWitness.digest[:], digest[:]) == 1
}

func newCommandRetryTransitionWitness(command Command) commandRetryTransitionWitness {
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return commandRetryTransitionWitness{}
	}
	return commandRetryTransitionWitness{digest: sha256.Sum256(wire)}
}

// RetryProviderAttempt atomically records that one exact in-flight attempt did
// not enter the provider. It makes the same logical command pending with a
// durable eligibility deadline, then consumes the prior one-shot claim receipt
// only after the store commit is reconstructible.
func (r *CommandRepository) RetryProviderAttempt(ctx context.Context, request CommandRetryRequest, partition TrustedCityPartition, authority TrustedCommandRetryTransitionAuthority) (result CommandRetryResult, retErr error) {
	if r == nil {
		return CommandRetryResult{}, fmt.Errorf("%w: repository is nil", ErrCommandProviderRetryInvalid)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandRetryResult{}, err
	}
	if err := validateCommandRetryRequest(request); err != nil {
		return CommandRetryResult{}, err
	}
	if !partition.valid() {
		return CommandRetryResult{}, fmt.Errorf("%w: trusted city partition is required", ErrCommandProviderRetryInvalid)
	}
	if isNilRepositoryDependency(authority) {
		return CommandRetryResult{}, fmt.Errorf("%w: retry transition authority is required", ErrCommandProviderRetryInvalid)
	}

	var prepared *CommandRetryTransitionIntent
	defer func() {
		if prepared == nil || (retErr == nil && result.HasRetryTransitionWitness()) {
			return
		}
		if releaseErr := releaseCommandRetryTransitionWriter(context.WithoutCancel(ctx), authority, *prepared); releaseErr != nil {
			result = CommandRetryResult{}
			retErr = errors.Join(retErr, releaseErr)
		}
	}()

	before, err := r.State(ctx)
	if err != nil {
		before, err = r.repairLineage(ctx, "pre-retry lineage repair")
		if err != nil {
			return CommandRetryResult{}, err
		}
	}
	var (
		state   CommandRepositoryState
		mutated bool
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: retry durable nudge provider attempt", func(tx beads.AtomicReadWriteTx) error {
		var err error
		state, err = readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		row, err := tx.GetIssue(request.CommandID)
		if err != nil {
			return fmt.Errorf("getting retry command %q: %w", request.CommandID, err)
		}
		entry, _, err := decodeCommandRecord(row, state)
		if err != nil {
			return err
		}
		if entry.Command == nil {
			return &CommandRepositorySchemaSkewError{Field: "retry command version", Found: "newer opaque command", Want: fmt.Sprintf("version %d", CommandVersion1)}
		}
		command := cloneCommandValue(*entry.Command)
		result.Command = cloneCommandValue(command)
		if commandPendingRetryMatches(command, request) {
			result = CommandRetryResult{Disposition: CommandRetryAlreadyRecorded, Command: command}
			return nil
		}
		if !commandInFlightRetryMatches(command, request) {
			result = CommandRetryResult{Disposition: CommandRetryStale, Command: command}
			return nil
		}
		if state.Revision == math.MaxUint64 {
			return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
		}

		retryable := command
		retryable.State = CommandStatePending
		retryable.Order.Revision = state.Revision + 1
		retryable.Claim = nil
		retry := cloneCommandRetry(*command.Retry)
		retry.NextEligibleAt = commandRetryTimePointer(request.NextEligibleAt)
		retry.ErrorClass = request.ErrorClass
		retry.ErrorDetail = request.Detail
		retryable.Retry = &retry
		intent, err := commandRetryTransitionIntentFor(state, command, retryable, request.ObservedAt, partition)
		if err != nil {
			return err
		}
		if err := prepareCommandRetryTransition(ctx, authority, intent); err != nil {
			return err
		}
		prepared = &intent
		wire, err := EncodeCommandV1(retryable)
		if err != nil {
			return fmt.Errorf("%w: encoding pending retry: %w", ErrCommandProviderRetryInvalid, err)
		}
		open := "open"
		if err := tx.Update(retryable.ID, beads.UpdateOpts{Status: &open, Metadata: map[string]string{commandRecordWireMetadataKey: string(wire)}}); err != nil {
			abortErr := abortCommandRetryTransition(context.WithoutCancel(ctx), authority, intent)
			return errors.Join(fmt.Errorf("updating pending retry command %q: %w", retryable.ID, err), abortErr)
		}
		if err := setCommandRepositoryHighWaters(tx, retryable.Order.Revision, state.SequenceHighWater); err != nil {
			abortErr := abortCommandRetryTransition(context.WithoutCancel(ctx), authority, intent)
			return errors.Join(err, abortErr)
		}
		mutated = true
		result = CommandRetryResult{Disposition: CommandRetryRecorded, Command: retryable}
		return nil
	})
	if err != nil {
		if mutated && prepared != nil {
			if recovered, recoveredState, ok := r.resolveRetriedProviderAttempt(ctx, result.Command, request); ok {
				result = CommandRetryResult{Disposition: CommandRetryAlreadyRecorded, Command: recovered}
				finalizeErr := finalizeRetriedProviderAttempt(ctx, authority, recoveredState, recovered, request, partition)
				if finalizeErr == nil {
					result.retryTransitionWitness = newCommandRetryTransitionWitness(recovered)
					prepared = nil
					return result, nil
				}
				return CommandRetryResult{}, finalizeErr
			}
		}
		return CommandRetryResult{}, err
	}

	if !mutated {
		if result.Disposition != CommandRetryAlreadyRecorded {
			return result, nil
		}
		commit, commitErr := commandRetryTransitionCommitFor(state, result.Command, request, partition)
		if commitErr != nil {
			return CommandRetryResult{}, commitErr
		}
		disposition, finalizeErr := authority.FinalizeCommandRetryTransition(ctx, commit)
		if finalizeErr != nil ||
			(disposition != CommandRetryReceiptFinalized && disposition != CommandRetryReceiptAlreadyFinalized) {
			return CommandRetryResult{}, errors.Join(fmt.Errorf("%w: verifying retained retry receipt", ErrCommandRetryTransition), finalizeErr)
		}
		result.retryTransitionWitness = newCommandRetryTransitionWitness(result.Command)
		return result, nil
	}

	state, err = r.repairLineage(ctx, "post-retry lineage advance")
	if err != nil {
		return CommandRetryResult{}, err
	}
	if prepared == nil {
		return CommandRetryResult{}, fmt.Errorf("%w: committed retry lacks preparation", ErrCommandRetryTransition)
	}
	if err := finalizeRetriedProviderAttempt(ctx, authority, state, result.Command, request, partition); err != nil {
		return CommandRetryResult{}, err
	}
	result.retryTransitionWitness = newCommandRetryTransitionWitness(result.Command)
	prepared = nil
	return result, nil
}

func validateCommandRetryRequest(request CommandRetryRequest) error {
	for _, field := range []struct{ name, value string }{
		{"retry command id", request.CommandID},
		{"retry claim id", request.ClaimID},
		{"retry operation id", request.OperationID},
		{"retry attempt id", request.AttemptID},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return fmt.Errorf("%w: %w", ErrCommandProviderRetryInvalid, err)
		}
	}
	if request.OperationID != request.CommandID {
		return fmt.Errorf("%w: operation id does not match command id", ErrCommandProviderRetryInvalid)
	}
	if err := validateCommandTime("retry observed_at", request.ObservedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandProviderRetryInvalid, err)
	}
	if err := validateCommandTime("retry next_eligible_at", request.NextEligibleAt); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandProviderRetryInvalid, err)
	}
	if !request.NextEligibleAt.After(request.ObservedAt) {
		return fmt.Errorf("%w: next eligibility must follow observation", ErrCommandProviderRetryInvalid)
	}
	if request.ProviderStage != ProviderStageNotEntered || request.Completion != CompletionStateNotCompleted {
		return fmt.Errorf("%w: retry requires definite non-entry evidence", ErrCommandProviderRetryInvalid)
	}
	if request.ErrorClass != CommandErrorClassProviderBusy && request.ErrorClass != CommandErrorClassProviderUnavailable {
		return fmt.Errorf("%w: error class %q is not retryable", ErrCommandProviderRetryInvalid, request.ErrorClass)
	}
	if request.Detail == "" {
		return fmt.Errorf("%w: retry detail is required", ErrCommandProviderRetryInvalid)
	}
	if err := validateCommandDetail("retry detail", request.Detail, MaxCommandRetryErrorDetailBytes); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandProviderRetryInvalid, err)
	}
	return nil
}

func commandInFlightRetryMatches(command Command, request CommandRetryRequest) bool {
	return command.State == CommandStateInFlight && command.Claim != nil && command.Retry != nil &&
		command.ID == request.CommandID && command.Claim.ID == request.ClaimID && command.Claim.OperationID == request.OperationID &&
		command.Claim.AttemptID == request.AttemptID && retryMatchesClaim(*command.Retry, *command.Claim) &&
		!request.ObservedAt.Before(command.Claim.ClaimedAt) && request.ObservedAt.Before(command.ExpiresAt) &&
		request.NextEligibleAt.Before(command.ExpiresAt)
}

func commandPendingRetryMatches(command Command, request CommandRetryRequest) bool {
	return command.State == CommandStatePending && command.Claim == nil && command.Terminal == nil && command.Retry != nil &&
		command.ID == request.CommandID && command.Retry.ClaimID == request.ClaimID && command.Retry.OperationID == request.OperationID &&
		command.Retry.AttemptID == request.AttemptID && command.Retry.NextEligibleAt != nil &&
		command.Retry.NextEligibleAt.Equal(request.NextEligibleAt) && command.Retry.ErrorClass == request.ErrorClass &&
		command.Retry.ErrorDetail == request.Detail
}

func commandRetryTimePointer(value time.Time) *time.Time {
	cloned := value
	return &cloned
}

func finalizeRetriedProviderAttempt(ctx context.Context, authority TrustedCommandRetryTransitionAuthority, state CommandRepositoryState, command Command, request CommandRetryRequest, partition TrustedCityPartition) error {
	commit, err := commandRetryTransitionCommitFor(state, command, request, partition)
	if err != nil {
		return err
	}
	disposition, err := authority.FinalizeCommandRetryTransition(ctx, commit)
	if err != nil {
		return fmt.Errorf("%w: finalizing exact retry transition: %w", ErrCommandRetryTransition, err)
	}
	if disposition != CommandRetryReceiptFinalized && disposition != CommandRetryReceiptAlreadyFinalized {
		return fmt.Errorf("%w: authority returned retry receipt disposition %q", ErrCommandRetryTransition, disposition)
	}
	return nil
}

func (r *CommandRepository) resolveRetriedProviderAttempt(ctx context.Context, expected Command, request CommandRetryRequest) (Command, CommandRepositoryState, bool) {
	state, err := r.repairLineage(ctx, "ambiguous retry lineage repair")
	if err != nil {
		return Command{}, CommandRepositoryState{}, false
	}
	resolved, err := r.Get(ctx, expected.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil || !sameCommandTransition(*resolved.Entry.Command, expected) ||
		!commandPendingRetryMatches(*resolved.Entry.Command, request) {
		return Command{}, CommandRepositoryState{}, false
	}
	return *resolved.Entry.Command, state, true
}
