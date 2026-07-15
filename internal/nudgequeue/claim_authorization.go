package nudgequeue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// ErrCommandClaimInvalid reports malformed claim input or authority evidence.
// It never implies that a provider was entered.
var ErrCommandClaimInvalid = errors.New("invalid durable nudge command claim")

// CommandClaimDisposition is the total effect-admission result for an existing
// known-version command.
type CommandClaimDisposition string

const (
	// CommandClaimAllowed returns the authoritative in-flight command whose
	// exact durable Claim+Retry is permission to proceed to the separate
	// provider-entry gate, not proof that provider entry occurred.
	CommandClaimAllowed CommandClaimDisposition = "allowed"
	// CommandClaimDenied returns a durable terminal command with proof that no
	// provider was entered by this claim.
	CommandClaimDenied CommandClaimDisposition = "denied"
	// CommandClaimAuthorizationUnknown returns the unchanged authoritative
	// command parked for an authorization-health or policy-version wake.
	CommandClaimAuthorizationUnknown CommandClaimDisposition = "authorization_unknown"
	// CommandClaimBusy returns the unchanged authoritative command when it is
	// not eligible or another claim/terminal outcome already owns it. An
	// in-flight claim stays busy after lease expiry unless durable evidence
	// proves that its provider attempt definitely did not enter.
	CommandClaimBusy CommandClaimDisposition = "busy"
)

// CommandClaimRequest contains only ownership and lease data. Payload, target,
// store, requester, and city-scope values come from durable authority or the
// opaque partition capability and cannot be substituted by the caller.
type CommandClaimRequest struct {
	CommandID           string
	ClaimID             string
	OwnerID             string
	AttemptID           string
	BoundLaunchIdentity string
	Partition           TrustedCityPartition
	ClaimedAt           time.Time
	LeaseUntil          time.Time
}

// NudgeClaimAuthorizationRequest is the transaction-decoded command and exact
// proposed ownership shown to current policy immediately before claim commit.
type NudgeClaimAuthorizationRequest struct {
	Command             Command
	Partition           TrustedCityPartition
	ClaimID             string
	OwnerID             string
	AttemptID           string
	BoundLaunchIdentity string
	ClaimedAt           time.Time
	LeaseUntil          time.Time
}

// NudgeClaimAuthorization is current policy evidence. Reference must equal the
// independently resolved ingress record; a copied command field is not enough.
type NudgeClaimAuthorization struct {
	Disposition            NudgeAuthorizationDisposition
	PrincipalSchemaVersion uint32
	DecisionID             string
	PolicyVersion          string
	Reference              TrustedIngressReference
}

// NudgeClaimAuthorizer revalidates immutable ingress and current mechanical
// policy. Implementations must authenticate from their own authority; command
// requester fields are lookup input only.
type NudgeClaimAuthorizer interface {
	AuthorizeNudgeClaim(context.Context, NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error)
}

// CommandClaimResult always carries the exact transaction-decoded durable
// command for a total disposition. An allowed result is still pre-provider;
// the effect runner must next pass the separate provider-entry CAS.
type CommandClaimResult struct {
	Disposition               CommandClaimDisposition
	Command                   Command
	terminalTransitionWitness commandTerminalTransitionWitness
}

type commandTerminalTransitionWitness struct {
	kind   uint8
	digest [sha256.Size]byte
}

const (
	commandTerminalTransitionUnwitnessed uint8 = iota
	commandTerminalTransitionCommitted
	commandTerminalTransitionRecovered
)

// HasTerminalTransitionWitness reports whether this repository call committed
// or exactly recovered the returned terminal transition. The causal witness is
// private so callers cannot mint publication authority from public result
// fields or from a terminal value read out of the command store.
func (r CommandClaimResult) HasTerminalTransitionWitness() bool {
	return r.Disposition == CommandClaimDenied && r.Command.Terminal != nil && r.terminalTransitionWitness.provesTransition(r.Command)
}

func newCommandTerminalTransitionWitness(kind uint8, command Command) commandTerminalTransitionWitness {
	if kind != commandTerminalTransitionCommitted && kind != commandTerminalTransitionRecovered {
		return commandTerminalTransitionWitness{}
	}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return commandTerminalTransitionWitness{}
	}
	return commandTerminalTransitionWitness{kind: kind, digest: sha256.Sum256(wire)}
}

func (w commandTerminalTransitionWitness) provesTransition(command Command) bool {
	if w.kind != commandTerminalTransitionCommitted && w.kind != commandTerminalTransitionRecovered {
		return false
	}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return false
	}
	digest := sha256.Sum256(wire)
	return subtle.ConstantTimeCompare(w.digest[:], digest[:]) == 1
}

// ClaimAuthorized atomically re-reads one durable command, revalidates current
// policy, and records either its exact bounded claim or a typed denial. Policy
// unknown leaves the command byte-for-byte unchanged. The method owns no
// provider/runtime capability and an allowed claim is not provider entry. Its
// committed Claim+Retry is durable may-enter permission: a crash can occur
// after provider entry but before later evidence is recorded, so lease expiry
// alone never releases the in-flight command for an automatic competing claim.
func (r *CommandRepository) ClaimAuthorized(ctx context.Context, request CommandClaimRequest, authorizer NudgeClaimAuthorizer, terminalAuthority TrustedCommandPartitionTerminalIntentAuthority) (CommandClaimResult, error) {
	if r == nil || isNilRepositoryDependency(r.reader.store) || isNilRepositoryDependency(r.reader.verifier) {
		return CommandClaimResult{}, fmt.Errorf("%w: command repository is not fully bound", ErrCommandClaimInvalid)
	}
	if isNilRepositoryDependency(authorizer) {
		return CommandClaimResult{}, fmt.Errorf("%w: claim authorizer is required", ErrCommandClaimInvalid)
	}
	if isNilRepositoryDependency(terminalAuthority) {
		return CommandClaimResult{}, fmt.Errorf("%w: terminal intent authority is required", ErrCommandClaimInvalid)
	}
	if err := validateCommandClaimRequest(request); err != nil {
		return CommandClaimResult{}, err
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return CommandClaimResult{}, err
	}
	before, err := r.State(ctx)
	if err != nil {
		before, err = r.repairLineage(ctx, "pre-claim lineage repair")
		if err != nil {
			return CommandClaimResult{}, err
		}
	}

	var (
		result       CommandClaimResult
		state        CommandRepositoryState
		mutated      bool
		authorityErr error
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: claim authorized durable nudge command", func(tx beads.AtomicReadWriteTx) error {
		var err error
		state, err = readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(before, state); err != nil {
			return err
		}
		row, err := tx.GetIssue(request.CommandID)
		if errors.Is(err, beads.ErrNotFound) {
			return fmt.Errorf("%w: command %q: %w", ErrCommandClaimInvalid, request.CommandID, beads.ErrNotFound)
		}
		if err != nil {
			return fmt.Errorf("reading command %q for claim: %w", request.CommandID, err)
		}
		entry, _, err := decodeCommandRecord(row, state)
		if err != nil {
			return err
		}
		if entry.Command == nil {
			return fmt.Errorf("%w: command %q requires a newer owner", ErrCommandClaimInvalid, request.CommandID)
		}
		command := cloneCommandValue(*entry.Command)
		result.Command = cloneCommandValue(command)

		if disposition, done := existingCommandClaimDisposition(command, request); done {
			result.Disposition = disposition
			if disposition == CommandClaimDenied && command.Terminal != nil {
				if err := verifyCommandPartitionTerminal(ctx, terminalAuthority, command, request.Partition); err != nil {
					return err
				}
				result.terminalTransitionWitness = newCommandTerminalTransitionWitness(commandTerminalTransitionRecovered, command)
			}
			return nil
		}
		if command.State != CommandStatePending {
			result.Disposition = CommandClaimBusy
			return nil
		}
		if request.ClaimedAt.Before(command.DeliverAfter) ||
			(command.Retry != nil && command.Retry.NextEligibleAt != nil && request.ClaimedAt.Before(*command.Retry.NextEligibleAt)) {
			result.Disposition = CommandClaimBusy
			return nil
		}
		if !request.ClaimedAt.Before(command.ExpiresAt) {
			terminalized, err := terminalizeExpiredCommand(command, request.ClaimedAt)
			if err != nil {
				return err
			}
			if state.Revision == math.MaxUint64 {
				return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
			}
			terminalized.Order.Revision = state.Revision + 1
			intent, err := prepareCommandPartitionTerminal(ctx, terminalAuthority, state.Revision, command, terminalized, request.Partition)
			if err != nil {
				return err
			}
			updated, err := writeCommandTransition(tx, state, row, terminalized)
			if err != nil {
				abortErr := abortCommandPartitionTerminal(context.WithoutCancel(ctx), terminalAuthority, intent)
				return errors.Join(err, abortErr)
			}
			result = CommandClaimResult{Disposition: CommandClaimDenied, Command: updated}
			mutated = true
			return nil
		}

		authorization, err := authorizer.AuthorizeNudgeClaim(ctx, NudgeClaimAuthorizationRequest{
			Command:             cloneCommandValue(command),
			Partition:           request.Partition,
			ClaimID:             request.ClaimID,
			OwnerID:             request.OwnerID,
			AttemptID:           request.AttemptID,
			BoundLaunchIdentity: request.BoundLaunchIdentity,
			ClaimedAt:           request.ClaimedAt,
			LeaseUntil:          request.LeaseUntil,
		})
		if err != nil {
			result = CommandClaimResult{Disposition: CommandClaimAuthorizationUnknown, Command: cloneCommandValue(command)}
			authorityErr = fmt.Errorf("%w: claim policy failed: %w", ErrNudgeAuthorizationUnknown, err)
			return nil
		}
		if authorization.Disposition == NudgeAuthorizationUnknown {
			result = CommandClaimResult{Disposition: CommandClaimAuthorizationUnknown, Command: cloneCommandValue(command)}
			return nil
		}
		if err := validateNudgeClaimAuthorization(command, authorization); err != nil {
			return err
		}

		denialDetail := "current authorization policy denied the command"
		denied := authorization.Disposition == NudgeAuthorizationDenied
		if request.Partition != trustedCityPartitionFromAuthority(authorization.Reference) {
			denied = true
			denialDetail = "trusted city authority does not own the command"
		}
		if !request.ClaimedAt.Before(command.TrustedIngress.ExpiresAt) {
			denied = true
			denialDetail = "trusted ingress authorization expired before claim"
		}
		if command.Binding != nil && request.BoundLaunchIdentity != command.Binding.LaunchIdentity {
			denied = true
			denialDetail = "claim launch does not match the durable target binding"
		}
		if denied {
			terminalized, err := terminalizeAuthorizationDeniedCommand(command, request.ClaimedAt, authorization, denialDetail)
			if err != nil {
				return err
			}
			if state.Revision == math.MaxUint64 {
				return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
			}
			terminalized.Order.Revision = state.Revision + 1
			intent, err := prepareCommandPartitionTerminal(ctx, terminalAuthority, state.Revision, command, terminalized, request.Partition)
			if err != nil {
				return err
			}
			updated, err := writeCommandTransition(tx, state, row, terminalized)
			if err != nil {
				abortErr := abortCommandPartitionTerminal(context.WithoutCancel(ctx), terminalAuthority, intent)
				return errors.Join(err, abortErr)
			}
			result = CommandClaimResult{Disposition: CommandClaimDenied, Command: updated}
			mutated = true
			return nil
		}

		claimed, err := buildAuthorizedClaim(command, request, authorization)
		if err != nil {
			return err
		}
		updated, err := writeCommandTransition(tx, state, row, claimed)
		if err != nil {
			return err
		}
		result = CommandClaimResult{Disposition: CommandClaimAllowed, Command: updated}
		mutated = true
		return nil
	})
	if err != nil {
		if mutated {
			if recovered, ok := r.resolveAmbiguousClaim(ctx, result); ok {
				return recovered, nil
			}
		}
		return CommandClaimResult{}, err
	}
	if mutated {
		if _, err := r.repairLineage(ctx, "post-claim lineage advance"); err != nil {
			return CommandClaimResult{}, err
		}
		if result.Command.Terminal != nil {
			result.terminalTransitionWitness = newCommandTerminalTransitionWitness(commandTerminalTransitionCommitted, result.Command)
		}
	}
	if authorityErr != nil {
		return result, authorityErr
	}
	return result, nil
}

func validateCommandClaimRequest(request CommandClaimRequest) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "command id", value: request.CommandID},
		{name: "claim id", value: request.ClaimID},
		{name: "claim owner id", value: request.OwnerID},
		{name: "claim attempt id", value: request.AttemptID},
		{name: "claim bound launch identity", value: request.BoundLaunchIdentity},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return fmt.Errorf("%w: %w", ErrCommandClaimInvalid, err)
		}
	}
	if !request.Partition.valid() {
		return fmt.Errorf("%w: trusted city partition capability is required", ErrCommandClaimInvalid)
	}
	if err := validateCommandTime("claim requested_at", request.ClaimedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandClaimInvalid, err)
	}
	if err := validateCommandTime("claim lease_until", request.LeaseUntil); err != nil {
		return fmt.Errorf("%w: %w", ErrCommandClaimInvalid, err)
	}
	if !request.LeaseUntil.After(request.ClaimedAt) {
		return fmt.Errorf("%w: lease_until must be after claimed_at", ErrCommandClaimInvalid)
	}
	return nil
}

func validateNudgeClaimAuthorization(command Command, authorization NudgeClaimAuthorization) error {
	if authorization.Disposition != NudgeAuthorizationAllowed && authorization.Disposition != NudgeAuthorizationDenied {
		return fmt.Errorf("%w: claim policy returned disposition %q", ErrNudgeAuthorizationInvalid, authorization.Disposition)
	}
	if authorization.PrincipalSchemaVersion != NudgePrincipalSchemaVersion &&
		authorization.PrincipalSchemaVersion != NudgePrincipalSchemaVersion-1 {
		return fmt.Errorf("%w: claim principal schema %d is unsupported", ErrNudgeAuthorizationInvalid, authorization.PrincipalSchemaVersion)
	}
	if err := validateCommandIdentity("claim authorization decision id", authorization.DecisionID); err != nil {
		return fmt.Errorf("%w: %w", ErrNudgeAuthorizationInvalid, err)
	}
	if err := validateCommandIdentity("claim authorization policy version", authorization.PolicyVersion); err != nil {
		return fmt.Errorf("%w: %w", ErrNudgeAuthorizationInvalid, err)
	}
	if authorization.Reference != command.TrustedIngress {
		return fmt.Errorf("%w: claim policy did not resolve the exact immutable ingress reference", ErrNudgeAuthorizationInvalid)
	}
	return nil
}

func existingCommandClaimDisposition(command Command, request CommandClaimRequest) (CommandClaimDisposition, bool) {
	if command.State == CommandStateInFlight && command.Claim != nil {
		if command.Claim.ID == request.ClaimID && command.Claim.OwnerID == request.OwnerID &&
			command.Claim.OperationID == request.CommandID && command.Claim.AttemptID == request.AttemptID &&
			command.Claim.BoundLaunchIdentity == request.BoundLaunchIdentity &&
			command.Claim.ClaimedAt.Equal(request.ClaimedAt) && command.Claim.LeaseUntil.Equal(request.LeaseUntil) {
			return CommandClaimAllowed, true
		}
		return CommandClaimBusy, true
	}
	if command.Terminal != nil {
		if command.Terminal.ActionResult == CommandActionResultAuthorizationDenied || command.Terminal.ActionResult == CommandActionResultExpired {
			return CommandClaimDenied, true
		}
		return CommandClaimBusy, true
	}
	return "", false
}

func buildAuthorizedClaim(command Command, request CommandClaimRequest, authorization NudgeClaimAuthorization) (Command, error) {
	if request.ClaimedAt.Before(command.CreatedAt) || request.ClaimedAt.Before(command.DeliverAfter) || !request.ClaimedAt.Before(command.ExpiresAt) {
		return Command{}, fmt.Errorf("%w: claim time is outside the command delivery window", ErrCommandClaimInvalid)
	}
	if request.LeaseUntil.After(command.ExpiresAt) {
		return Command{}, fmt.Errorf("%w: claim lease extends beyond command expiry", ErrCommandClaimInvalid)
	}
	if command.Retry != nil {
		return Command{}, fmt.Errorf("%w: retry reauthorization requires the next command schema", ErrCommandClaimInvalid)
	}
	if command.Binding == nil {
		command.Binding = &CommandBinding{LaunchIdentity: request.BoundLaunchIdentity, BoundAt: request.ClaimedAt}
	}
	claim := CommandClaim{
		ID:                         request.ClaimID,
		OwnerID:                    request.OwnerID,
		OperationID:                command.ID,
		AttemptID:                  request.AttemptID,
		BoundLaunchIdentity:        request.BoundLaunchIdentity,
		AuthorizationDecisionID:    authorization.DecisionID,
		AuthorizationPolicyVersion: authorization.PolicyVersion,
		ClaimedAt:                  request.ClaimedAt,
		LeaseUntil:                 request.LeaseUntil,
	}
	command.State = CommandStateInFlight
	command.Claim = &claim
	command.Retry = &CommandRetry{
		AttemptCount:               1,
		LastAttemptAt:              request.ClaimedAt,
		ClaimID:                    claim.ID,
		OperationID:                claim.OperationID,
		AttemptID:                  claim.AttemptID,
		BoundLaunchIdentity:        claim.BoundLaunchIdentity,
		AuthorizationDecisionID:    claim.AuthorizationDecisionID,
		AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
	}
	command.Terminal = nil
	if _, err := EncodeCommandV1(command); err != nil {
		return Command{}, fmt.Errorf("%w: building authorized claim: %w", ErrCommandClaimInvalid, err)
	}
	return command, nil
}

func terminalizeAuthorizationDeniedCommand(command Command, at time.Time, authorization NudgeClaimAuthorization, detail string) (Command, error) {
	if command.Retry != nil || (command.Target.Policy == TargetPolicyContinuation && command.Binding != nil) {
		return Command{}, fmt.Errorf("%w: current command schema cannot retain a post-attempt authorization denial", ErrCommandClaimInvalid)
	}
	command.State = CommandStateDeadLettered
	command.Claim = nil
	command.Terminal = &CommandTerminal{
		At:                         at,
		ActionResult:               CommandActionResultAuthorizationDenied,
		ErrorClass:                 CommandErrorClassAuthorizationDenied,
		Detail:                     detail,
		AuthorizationDecisionID:    authorization.DecisionID,
		AuthorizationPolicyVersion: authorization.PolicyVersion,
		ProviderStage:              ProviderStageNotEntered,
		Completion:                 CompletionStateNotCompleted,
	}
	if _, err := EncodeCommandV1(command); err != nil {
		return Command{}, fmt.Errorf("%w: building authorization denial: %w", ErrCommandClaimInvalid, err)
	}
	return command, nil
}

func terminalizeExpiredCommand(command Command, at time.Time) (Command, error) {
	command.State = CommandStateExpired
	command.Claim = nil
	terminal := &CommandTerminal{
		At:            at,
		ActionResult:  CommandActionResultExpired,
		ErrorClass:    CommandErrorClassExpired,
		Detail:        "command delivery window expired before provider entry",
		ProviderStage: ProviderStageNotEntered,
		Completion:    CompletionStateNotCompleted,
	}
	if command.Retry != nil {
		terminal.ClaimID = command.Retry.ClaimID
		terminal.OperationID = command.Retry.OperationID
		terminal.AttemptID = command.Retry.AttemptID
		terminal.BoundLaunchIdentity = command.Retry.BoundLaunchIdentity
		terminal.AuthorizationDecisionID = command.Retry.AuthorizationDecisionID
		terminal.AuthorizationPolicyVersion = command.Retry.AuthorizationPolicyVersion
	}
	command.Terminal = terminal
	if _, err := EncodeCommandV1(command); err != nil {
		return Command{}, fmt.Errorf("%w: building command expiry: %w", ErrCommandClaimInvalid, err)
	}
	return command, nil
}

func writeCommandTransition(tx beads.AtomicReadWriteTx, state CommandRepositoryState, row beads.Bead, command Command) (Command, error) {
	if state.Revision == math.MaxUint64 {
		return Command{}, &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
	}
	command.Order.Revision = state.Revision + 1
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return Command{}, err
	}
	status := "open"
	if commandIsTerminalState(command.State) {
		status = "closed"
	}
	if err := tx.Update(row.ID, beads.UpdateOpts{
		Status:   &status,
		Metadata: map[string]string{commandRecordWireMetadataKey: string(wire)},
	}); err != nil {
		return Command{}, fmt.Errorf("updating durable nudge command %q: %w", row.ID, err)
	}
	if err := setCommandRepositoryHighWaters(tx, command.Order.Revision, state.SequenceHighWater); err != nil {
		return Command{}, err
	}
	updatedRow, err := tx.GetIssue(row.ID)
	if err != nil {
		return Command{}, fmt.Errorf("reading back transitioned command %q: %w", row.ID, err)
	}
	updatedState := state
	updatedState.Revision = command.Order.Revision
	entry, _, err := decodeCommandRecord(updatedRow, updatedState)
	if err != nil {
		return Command{}, err
	}
	if entry.Command == nil {
		return Command{}, fmt.Errorf("%w: transitioned command became opaque", ErrCommandClaimInvalid)
	}
	return cloneCommandValue(*entry.Command), nil
}

func (r *CommandRepository) resolveAmbiguousClaim(ctx context.Context, expected CommandClaimResult) (CommandClaimResult, bool) {
	if _, err := r.repairLineage(ctx, "ambiguous claim lineage repair"); err != nil {
		return CommandClaimResult{}, false
	}
	resolution, err := r.Get(ctx, expected.Command.ID)
	if err != nil || !resolution.Found || resolution.Entry.Command == nil {
		return CommandClaimResult{}, false
	}
	command := cloneCommandValue(*resolution.Entry.Command)
	if !sameCommandTransition(command, expected.Command) {
		return CommandClaimResult{}, false
	}
	expected.Command = command
	if command.Terminal != nil {
		expected.terminalTransitionWitness = newCommandTerminalTransitionWitness(commandTerminalTransitionRecovered, command)
	}
	return expected, true
}

func sameCommandTransition(left, right Command) bool {
	leftWire, err := EncodeCommandV1(left)
	if err != nil {
		return false
	}
	rightWire, err := EncodeCommandV1(right)
	if err != nil {
		return false
	}
	return bytes.Equal(leftWire, rightWire)
}

func cloneCommandValue(command Command) Command {
	command.Reference = cloneCommandReference(command.Reference)
	if command.Binding != nil {
		binding := *command.Binding
		command.Binding = &binding
	}
	if command.Retry != nil {
		retry := *command.Retry
		if retry.NextEligibleAt != nil {
			next := *retry.NextEligibleAt
			retry.NextEligibleAt = &next
		}
		command.Retry = &retry
	}
	if command.Claim != nil {
		claim := *command.Claim
		command.Claim = &claim
	}
	if command.Terminal != nil {
		terminal := *command.Terminal
		command.Terminal = &terminal
	}
	return command
}
