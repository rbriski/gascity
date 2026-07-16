package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
)

// ErrCommandClaimTransition reports missing, conflicting, or unavailable
// authority evidence for the write-ahead claim transition and its receipt.
var ErrCommandClaimTransition = errors.New("durable nudge command claim transition is unverified")

// CommandClaimTransitionIntent is the independent write-ahead record for one
// exact pending-to-in-flight command transition. Both digests are SHA-256 over
// the canonical command wire. The complete claim is copied from AfterCommandDigest's
// command so identity, authorization, and lease evidence cannot be substituted.
type CommandClaimTransitionIntent struct {
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
}

// CommandClaimTransitionReceipt is exact post-commit evidence used to consume
// a matching preparation. EffectRepositoryRevision and
// EffectSequenceHighWater are the transaction-consistent store snapshot whose
// monotonic effect fence is advanced atomically with the retained receipt.
type CommandClaimTransitionReceipt struct {
	Store                    CommandStoreBinding
	RepositoryRevision       uint64
	CommandID                string
	Sequence                 uint64
	Partition                TrustedCityPartition
	AfterCommandDigest       [sha256.Size]byte
	Claim                    CommandClaim
	EffectRepositoryRevision uint64
	EffectSequenceHighWater  uint64
}

// CommandClaimReceiptDisposition reports whether this call created the exact
// receipt or found that a previous call had already created it.
type CommandClaimReceiptDisposition string

const (
	// CommandClaimReceiptFinalized means this call consumed the preparation and
	// durably created the receipt. The caller may return provider-entry authority.
	CommandClaimReceiptFinalized CommandClaimReceiptDisposition = "finalized"
	// CommandClaimReceiptAlreadyFinalized means an earlier call created the
	// receipt. The current caller must not infer whether provider entry occurred.
	CommandClaimReceiptAlreadyFinalized CommandClaimReceiptDisposition = "already_finalized"
)

// TrustedCommandClaimTransitionAuthority owns claim write-ahead preparation
// independently of the command store. Prepare is called while the serialized
// store transaction still contains the exact pending before-state. It may
// replace a stale preparation only for that same before-state. Finalize
// atomically retains the exact receipt and advances the independent effect
// high-water. Abort removes only an exact preparation after definite rollback.
type TrustedCommandClaimTransitionAuthority interface {
	PrepareCommandClaimTransition(context.Context, CommandClaimTransitionIntent) error
	ReleaseCommandClaimTransitionWriter(context.Context, CommandClaimTransitionIntent) error
	AbortCommandClaimTransition(context.Context, CommandClaimTransitionIntent) error
	FinalizeCommandClaimTransition(context.Context, CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error)
}

func commandClaimTransitionIntentFor(state CommandRepositoryState, before, after Command, partition TrustedCityPartition) (CommandClaimTransitionIntent, error) {
	if validateCommandRepositoryBinding(state.Store) != nil || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision ||
		state.Revision == ^uint64(0) || !partition.valid() {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: repository before-state is invalid", ErrCommandClaimTransition)
	}
	if before.Store != state.Store || after.Store != state.Store || before.ID != after.ID ||
		before.Order.Sequence == 0 || before.Order.Sequence != after.Order.Sequence ||
		before.Order.Sequence > state.SequenceHighWater || before.Order.Revision == 0 || before.Order.Revision > state.Revision ||
		after.Order.Revision != state.Revision+1 || before.State != CommandStatePending || before.Claim != nil || before.Terminal != nil ||
		after.State != CommandStateInFlight || after.Claim == nil || after.Retry == nil || after.Terminal != nil {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: pending-to-in-flight states are inconsistent", ErrCommandClaimTransition)
	}
	beforeWire, err := EncodeCommandV1(before)
	if err != nil {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: encoding before-state: %w", ErrCommandClaimTransition, err)
	}
	afterWire, err := EncodeCommandV1(after)
	if err != nil {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: encoding after-state: %w", ErrCommandClaimTransition, err)
	}
	if err := validateCommandIndexUpdate(before, after); err != nil {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: pending-to-in-flight transition is invalid: %w", ErrCommandClaimTransition, err)
	}
	return CommandClaimTransitionIntent{
		Store: state.Store, RepositoryBeforeRevision: state.Revision, RepositoryRevision: after.Order.Revision,
		RepositorySequenceHighWater: state.SequenceHighWater, CommandID: after.ID, Sequence: after.Order.Sequence,
		Partition: partition, BeforeCommandDigest: sha256.Sum256(beforeWire), AfterCommandDigest: sha256.Sum256(afterWire),
		Claim: *after.Claim,
	}, nil
}

func commandClaimTransitionReceiptFor(state CommandRepositoryState, command Command, partition TrustedCityPartition) (CommandClaimTransitionReceipt, error) {
	if validateCommandRepositoryBinding(state.Store) != nil || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision || !partition.valid() ||
		command.Store != state.Store || command.State != CommandStateInFlight || command.Claim == nil || command.Retry == nil || command.Terminal != nil ||
		command.ID == "" || command.Order.Sequence == 0 || command.Order.Sequence > state.SequenceHighWater ||
		command.Order.Revision == 0 || command.Order.Revision > state.Revision {
		return CommandClaimTransitionReceipt{}, fmt.Errorf("%w: in-flight receipt state is inconsistent", ErrCommandClaimTransition)
	}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return CommandClaimTransitionReceipt{}, fmt.Errorf("%w: encoding receipt after-state: %w", ErrCommandClaimTransition, err)
	}
	return CommandClaimTransitionReceipt{
		Store: state.Store, RepositoryRevision: command.Order.Revision, CommandID: command.ID,
		Sequence: command.Order.Sequence, Partition: partition, AfterCommandDigest: sha256.Sum256(wire), Claim: *command.Claim,
		EffectRepositoryRevision: state.Revision, EffectSequenceHighWater: state.SequenceHighWater,
	}, nil
}

func validateCommandClaimTransitionIntent(intent CommandClaimTransitionIntent) error {
	if validateCommandRepositoryBinding(intent.Store) != nil || !intent.Partition.valid() ||
		intent.RepositoryBeforeRevision == 0 || intent.RepositoryBeforeRevision == ^uint64(0) ||
		intent.RepositoryRevision != intent.RepositoryBeforeRevision+1 ||
		intent.Sequence == 0 || intent.Sequence > intent.RepositorySequenceHighWater ||
		intent.RepositorySequenceHighWater > intent.RepositoryBeforeRevision ||
		intent.BeforeCommandDigest == ([sha256.Size]byte{}) || intent.AfterCommandDigest == ([sha256.Size]byte{}) ||
		validateCommandIdentity("claim transition command id", intent.CommandID) != nil ||
		validatePersistedCommandClaim(intent.CommandID, intent.Claim) != nil {
		return fmt.Errorf("%w: claim transition intent is invalid", ErrCommandClaimTransition)
	}
	return nil
}

func validateCommandClaimTransitionReceipt(receipt CommandClaimTransitionReceipt) error {
	if validateCommandRepositoryBinding(receipt.Store) != nil || !receipt.Partition.valid() ||
		receipt.RepositoryRevision == 0 || receipt.Sequence == 0 || receipt.AfterCommandDigest == ([sha256.Size]byte{}) ||
		receipt.EffectRepositoryRevision < receipt.RepositoryRevision || receipt.EffectSequenceHighWater < receipt.Sequence ||
		receipt.EffectSequenceHighWater > receipt.EffectRepositoryRevision ||
		validateCommandIdentity("claim receipt command id", receipt.CommandID) != nil ||
		validatePersistedCommandClaim(receipt.CommandID, receipt.Claim) != nil {
		return fmt.Errorf("%w: claim transition receipt is invalid", ErrCommandClaimTransition)
	}
	return nil
}

func validatePersistedCommandClaim(commandID string, claim CommandClaim) error {
	for _, field := range []struct{ name, value string }{
		{"claim id", claim.ID},
		{"claim owner id", claim.OwnerID},
		{"claim operation id", claim.OperationID},
		{"claim attempt id", claim.AttemptID},
		{"claim bound launch identity", claim.BoundLaunchIdentity},
		{"claim authorization decision id", claim.AuthorizationDecisionID},
		{"claim authorization policy version", claim.AuthorizationPolicyVersion},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return err
		}
	}
	if claim.OperationID != commandID {
		return errors.New("claim operation id does not match command id")
	}
	if err := validateCommandTime("claim claimed_at", claim.ClaimedAt); err != nil {
		return err
	}
	if err := validateCommandTime("claim lease_until", claim.LeaseUntil); err != nil {
		return err
	}
	if !claim.LeaseUntil.After(claim.ClaimedAt) {
		return errors.New("claim lease_until is not after claimed_at")
	}
	return nil
}

func prepareCommandClaimTransition(ctx context.Context, authority TrustedCommandClaimTransitionAuthority, intent CommandClaimTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: claim transition authority is required", ErrCommandClaimTransition)
	}
	if err := authority.PrepareCommandClaimTransition(ctx, intent); err != nil {
		return fmt.Errorf("%w: preparing exact claim transition: %w", ErrCommandClaimTransition, err)
	}
	return nil
}

func abortCommandClaimTransition(ctx context.Context, authority TrustedCommandClaimTransitionAuthority, intent CommandClaimTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: claim transition authority is required", ErrCommandClaimTransition)
	}
	if err := authority.AbortCommandClaimTransition(ctx, intent); err != nil {
		return fmt.Errorf("%w: aborting exact claim transition: %w", ErrCommandClaimTransition, err)
	}
	return nil
}

func releaseCommandClaimTransitionWriter(ctx context.Context, authority TrustedCommandClaimTransitionAuthority, intent CommandClaimTransitionIntent) error {
	if isNilRepositoryDependency(authority) {
		return fmt.Errorf("%w: claim transition authority is required", ErrCommandClaimTransition)
	}
	if err := authority.ReleaseCommandClaimTransitionWriter(ctx, intent); err != nil {
		return fmt.Errorf("%w: releasing claim transition writer: %w", ErrCommandClaimTransition, err)
	}
	return nil
}

func finalizeCommandClaimTransition(ctx context.Context, authority TrustedCommandClaimTransitionAuthority, state CommandRepositoryState, command Command, partition TrustedCityPartition) (CommandClaimReceiptDisposition, error) {
	if isNilRepositoryDependency(authority) {
		return "", fmt.Errorf("%w: claim transition authority is required", ErrCommandClaimTransition)
	}
	receipt, err := commandClaimTransitionReceiptFor(state, command, partition)
	if err != nil {
		return "", err
	}
	disposition, err := authority.FinalizeCommandClaimTransition(ctx, receipt)
	if err != nil {
		return "", fmt.Errorf("%w: finalizing exact claim transition: %w", ErrCommandClaimTransition, err)
	}
	if disposition != CommandClaimReceiptFinalized && disposition != CommandClaimReceiptAlreadyFinalized {
		return "", fmt.Errorf("%w: authority returned receipt disposition %q", ErrCommandClaimTransition, disposition)
	}
	return disposition, nil
}

func commandClaimsEqual(left, right CommandClaim) bool {
	return left.ID == right.ID && left.OwnerID == right.OwnerID && left.OperationID == right.OperationID &&
		left.AttemptID == right.AttemptID && left.BoundLaunchIdentity == right.BoundLaunchIdentity &&
		left.AuthorizationDecisionID == right.AuthorizationDecisionID &&
		left.AuthorizationPolicyVersion == right.AuthorizationPolicyVersion &&
		left.ClaimedAt.Equal(right.ClaimedAt) && left.LeaseUntil.Equal(right.LeaseUntil)
}

func claimIntentMatchesReceipt(intent CommandClaimTransitionIntent, receipt CommandClaimTransitionReceipt) bool {
	return intent.Store == receipt.Store && intent.RepositoryRevision == receipt.RepositoryRevision &&
		intent.CommandID == receipt.CommandID && intent.Sequence == receipt.Sequence && intent.Partition == receipt.Partition &&
		intent.AfterCommandDigest == receipt.AfterCommandDigest && commandClaimsEqual(intent.Claim, receipt.Claim) &&
		receipt.EffectRepositoryRevision >= intent.RepositoryRevision &&
		receipt.EffectSequenceHighWater >= intent.RepositorySequenceHighWater
}

func sameCommandClaimTransitionReceipt(left, right CommandClaimTransitionReceipt) bool {
	return left.Store == right.Store && left.RepositoryRevision == right.RepositoryRevision &&
		left.CommandID == right.CommandID && left.Sequence == right.Sequence && left.Partition == right.Partition &&
		left.AfterCommandDigest == right.AfterCommandDigest && commandClaimsEqual(left.Claim, right.Claim)
}
