package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ErrCommandProviderAttemptInvalid reports malformed completion evidence or
// an action result that cannot terminalize an exact provider attempt.
var ErrCommandProviderAttemptInvalid = errors.New("invalid durable nudge provider attempt")

// CommandCompletionDisposition is the total result of one exact terminal
// completion attempt.
type CommandCompletionDisposition string

const (
	// CommandCompletionRecorded means this call atomically committed the
	// terminal command, closed row, and repository revision.
	CommandCompletionRecorded CommandCompletionDisposition = "recorded"
	// CommandCompletionAlreadyRecorded means the same durable attempt was
	// already terminal. Command contains the authoritative existing outcome.
	CommandCompletionAlreadyRecorded CommandCompletionDisposition = "already_recorded"
	// CommandCompletionStale means the command is still nonterminal but the
	// requested claim or attempt no longer owns it.
	CommandCompletionStale CommandCompletionDisposition = "stale"
)

// CommandCompletionRequest carries only controller-observed provider outcome
// evidence. Immutable target, binding, authorization, and attempt evidence are
// copied from the authoritative in-flight command inside the transaction.
type CommandCompletionRequest struct {
	CommandID     string
	ClaimID       string
	OperationID   string
	AttemptID     string
	CompletedAt   time.Time
	ActionResult  CommandActionResult
	ErrorClass    CommandErrorClass
	Detail        string
	ProviderStage ProviderStage
	Completion    CompletionState
}

// CommandCompletionResult returns the authoritative command observed or
// committed by [CommandRepository.CompleteProviderAttempt].
type CommandCompletionResult struct {
	Disposition               CommandCompletionDisposition
	Command                   Command
	terminalTransitionWitness commandTerminalTransitionWitness
}

// HasTerminalTransitionWitness reports whether this repository call committed
// or exactly recovered the returned provider-attempt terminal. The private
// witness prevents callers from turning public result fields into publication
// authority.
func (r CommandCompletionResult) HasTerminalTransitionWitness() bool {
	return (r.Disposition == CommandCompletionRecorded || r.Disposition == CommandCompletionAlreadyRecorded) &&
		r.Command.Terminal != nil && r.terminalTransitionWitness.provesTransition(r.Command)
}

// CompleteProviderAttempt atomically makes an exact in-flight provider result
// terminal. The command wire, closed-row status, and repository revision share
// one backing transaction; a response-loss retry resolves the existing result
// instead of writing a second terminal outcome.
func (r *CommandRepository) CompleteProviderAttempt(ctx context.Context, request CommandCompletionRequest, partition TrustedCityPartition, terminalAuthority TrustedCommandPartitionTerminalIntentAuthority) (CommandCompletionResult, error) {
	if r == nil {
		return CommandCompletionResult{}, fmt.Errorf("%w: repository is nil", ErrCommandProviderAttemptInvalid)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandCompletionResult{}, err
	}
	if err := validateCommandCompletionRequest(request); err != nil {
		return CommandCompletionResult{}, err
	}
	if !partition.valid() {
		return CommandCompletionResult{}, fmt.Errorf("%w: trusted city partition is required", ErrCommandProviderAttemptInvalid)
	}
	if isNilRepositoryDependency(terminalAuthority) {
		return CommandCompletionResult{}, fmt.Errorf("%w: terminal intent authority is required", ErrCommandProviderAttemptInvalid)
	}
	before, err := r.State(ctx)
	if err != nil {
		before, err = r.repairLineage(ctx, "pre-completion lineage repair")
		if err != nil {
			return CommandCompletionResult{}, err
		}
	}

	var (
		result  CommandCompletionResult
		mutated bool
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: complete durable nudge provider attempt", func(tx beads.AtomicReadWriteTx) error {
		state, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		row, err := tx.GetIssue(request.CommandID)
		if err != nil {
			return fmt.Errorf("getting provider-attempt command %q: %w", request.CommandID, err)
		}
		entry, _, err := decodeCommandRecord(row, state)
		if err != nil {
			return err
		}
		if entry.Command == nil {
			return &CommandRepositorySchemaSkewError{
				Field: "provider-attempt command version",
				Found: "newer opaque command",
				Want:  fmt.Sprintf("version %d", CommandVersion1),
			}
		}
		command := *entry.Command
		if commandTerminalAttemptMatches(command, request) {
			result = CommandCompletionResult{Disposition: CommandCompletionAlreadyRecorded, Command: command}
			if err := verifyCommandPartitionTerminal(ctx, terminalAuthority, command, partition); err != nil {
				return err
			}
			result.terminalTransitionWitness = newCommandTerminalTransitionWitness(commandTerminalTransitionRecovered, command)
			return nil
		}
		if !commandInFlightAttemptMatches(command, request) {
			result = CommandCompletionResult{Disposition: CommandCompletionStale, Command: command}
			return nil
		}
		if state.Revision == math.MaxUint64 {
			return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
		}

		terminal := CommandTerminal{
			At:                         request.CompletedAt.UTC(),
			ActionResult:               request.ActionResult,
			ErrorClass:                 request.ErrorClass,
			Detail:                     request.Detail,
			ClaimID:                    command.Claim.ID,
			OperationID:                command.Claim.OperationID,
			AttemptID:                  command.Claim.AttemptID,
			BoundLaunchIdentity:        command.Claim.BoundLaunchIdentity,
			AuthorizationDecisionID:    command.Claim.AuthorizationDecisionID,
			AuthorizationPolicyVersion: command.Claim.AuthorizationPolicyVersion,
			ProviderStage:              request.ProviderStage,
			Completion:                 request.Completion,
		}
		rule, ok := commandTerminalRuleFor(request.ActionResult)
		if !ok || (rule.evidence != commandTerminalEvidenceAttempt && rule.evidence != commandTerminalEvidenceOptionalAttempt) {
			return fmt.Errorf("%w: action result %q is not a provider-attempt terminal", ErrCommandProviderAttemptInvalid, request.ActionResult)
		}
		completed := command
		completed.State = rule.state
		completed.Order.Revision = state.Revision + 1
		completed.Claim = nil
		completed.Terminal = &terminal
		wire, err := EncodeCommandV1(completed)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrCommandProviderAttemptInvalid, err)
		}
		intent, err := prepareCommandPartitionTerminal(ctx, terminalAuthority, state.Revision, command, completed, partition)
		if err != nil {
			return err
		}
		closed := "closed"
		if err := tx.Update(completed.ID, beads.UpdateOpts{
			Status: &closed,
			Metadata: map[string]string{
				commandRecordWireMetadataKey: string(wire),
			},
		}); err != nil {
			abortErr := abortCommandPartitionTerminal(context.WithoutCancel(ctx), terminalAuthority, intent)
			return errors.Join(fmt.Errorf("updating terminal provider-attempt command %q: %w", completed.ID, err), abortErr)
		}
		if err := setCommandRepositoryHighWaters(tx, completed.Order.Revision, state.SequenceHighWater); err != nil {
			abortErr := abortCommandPartitionTerminal(context.WithoutCancel(ctx), terminalAuthority, intent)
			return errors.Join(err, abortErr)
		}
		mutated = true
		result = CommandCompletionResult{
			Disposition:               CommandCompletionRecorded,
			Command:                   completed,
			terminalTransitionWitness: newCommandTerminalTransitionWitness(commandTerminalTransitionCommitted, completed),
		}
		return nil
	})
	if err != nil {
		if mutated {
			if recovered, ok := r.resolveCompletedProviderAttempt(ctx, result); ok {
				return recovered, nil
			}
		}
		return CommandCompletionResult{}, err
	}
	if !mutated {
		return result, nil
	}
	if _, err := r.repairLineage(ctx, "post-completion lineage advance"); err != nil {
		return CommandCompletionResult{}, err
	}
	return result, nil
}

func validateCommandCompletionRequest(request CommandCompletionRequest) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "completion command id", value: request.CommandID},
		{name: "completion claim id", value: request.ClaimID},
		{name: "completion operation id", value: request.OperationID},
		{name: "completion attempt id", value: request.AttemptID},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return fmt.Errorf("%w: %w", ErrCommandProviderAttemptInvalid, err)
		}
	}
	if request.OperationID != request.CommandID {
		return fmt.Errorf("%w: operation id does not match command id", ErrCommandProviderAttemptInvalid)
	}
	if err := validateCommandTime("completion time", request.CompletedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandProviderAttemptInvalid, err)
	}
	rule, ok := commandTerminalRuleFor(request.ActionResult)
	if !ok || (rule.evidence != commandTerminalEvidenceAttempt && rule.evidence != commandTerminalEvidenceOptionalAttempt) {
		return fmt.Errorf("%w: action result %q is not a provider-attempt terminal", ErrCommandProviderAttemptInvalid, request.ActionResult)
	}
	if request.ProviderStage != rule.providerStage || request.Completion != rule.completion || !rule.acceptsErrorClass(request.ErrorClass) {
		return fmt.Errorf("%w: action result %q rejects stage/completion/error class %q/%q/%q", ErrCommandProviderAttemptInvalid, request.ActionResult, request.ProviderStage, request.Completion, request.ErrorClass)
	}
	if request.ErrorClass == CommandErrorClassNone {
		if request.Detail != "" {
			return fmt.Errorf("%w: successful result carries detail", ErrCommandProviderAttemptInvalid)
		}
	} else {
		if request.Detail == "" {
			return fmt.Errorf("%w: failed result is missing detail", ErrCommandProviderAttemptInvalid)
		}
		if err := validateCommandDetail("completion detail", request.Detail, MaxCommandTerminalDetailBytes); err != nil {
			return fmt.Errorf("%w: %w", ErrCommandProviderAttemptInvalid, err)
		}
	}
	return nil
}

func commandInFlightAttemptMatches(command Command, request CommandCompletionRequest) bool {
	return command.State == CommandStateInFlight && command.Claim != nil && command.Retry != nil &&
		command.ID == request.CommandID &&
		command.Claim.ID == request.ClaimID &&
		command.Claim.OperationID == request.OperationID &&
		command.Claim.AttemptID == request.AttemptID &&
		command.Retry.ClaimID == request.ClaimID &&
		command.Retry.OperationID == request.OperationID &&
		command.Retry.AttemptID == request.AttemptID
}

func commandTerminalAttemptMatches(command Command, request CommandCompletionRequest) bool {
	return command.Terminal != nil && command.Retry != nil && commandIsTerminalState(command.State) &&
		command.ID == request.CommandID &&
		command.Terminal.ClaimID == request.ClaimID &&
		command.Terminal.OperationID == request.OperationID &&
		command.Terminal.AttemptID == request.AttemptID &&
		command.Retry.ClaimID == request.ClaimID &&
		command.Retry.OperationID == request.OperationID &&
		command.Retry.AttemptID == request.AttemptID
}

func (r *CommandRepository) resolveCompletedProviderAttempt(ctx context.Context, expected CommandCompletionResult) (CommandCompletionResult, bool) {
	if _, err := r.repairLineage(ctx, "ambiguous completion lineage repair"); err != nil {
		return CommandCompletionResult{}, false
	}
	resolved, err := r.Get(ctx, expected.Command.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil {
		return CommandCompletionResult{}, false
	}
	command := *resolved.Entry.Command
	if !sameCommandTransition(command, expected.Command) {
		return CommandCompletionResult{}, false
	}
	return CommandCompletionResult{
		Disposition:               CommandCompletionAlreadyRecorded,
		Command:                   command,
		terminalTransitionWitness: newCommandTerminalTransitionWitness(commandTerminalTransitionRecovered, command),
	}, true
}
