package nudgequeue

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	commandProvenanceIdentityDomainV1 = "gascity.nudge-command.provenance-identity.v1"
	// CommandProvenanceRejectionReasonUnauthorized is the sole first-version
	// reason for a partitionless authority decision.
	CommandProvenanceRejectionReasonUnauthorized = "unauthorized_provenance"
)

// ErrCommandProvenanceRejection reports malformed, conflicting, or
// unavailable evidence while quarantining an unadmitted command row.
var ErrCommandProvenanceRejection = errors.New("durable nudge command provenance rejection failed")

// CommandProvenanceRejectionIntent is the stable write-ahead authority record
// for one unadmitted known-version command. It deliberately excludes the next
// repository revision: unrelated repository writes may advance that value
// before a crash-recovered retry binds its exact terminal resolution.
type CommandProvenanceRejectionIntent struct {
	Store                 CommandStoreBinding
	CommandID             string
	Sequence              uint64
	AllocationRevision    uint64
	BeforeCommandRevision uint64
	IdentityDigest        [sha256.Size]byte
	BeforeCommandDigest   [sha256.Size]byte
	Reason                string
	RejectedAt            time.Time
}

// CommandProvenanceRejectionResolution reports the exact terminal revision and
// canonical command digest actually committed by the command repository.
type CommandProvenanceRejectionResolution struct {
	Intent             CommandProvenanceRejectionIntent
	RepositoryRevision uint64
	CommandDigest      [sha256.Size]byte
}

// TrustedCommandProvenanceRejectionAuthority owns partitionless write-ahead
// rejection evidence. Prepare records stable before-state intent. Verify proves
// that exact preparation remains durable immediately before the store write.
// Record finalizes the exact resolution returned by the committed repository
// transaction and advances dense authority decisions.
type TrustedCommandProvenanceRejectionAuthority interface {
	PrepareCommandProvenanceRejection(context.Context, CommandProvenanceRejectionIntent) error
	VerifyCommandProvenanceRejectionPreparation(context.Context, CommandProvenanceRejectionIntent) error
	RecordCommandProvenanceRejection(context.Context, CommandProvenanceRejectionResolution) error
}

// RejectCommandProvenance terminalizes one exact known-version command whose
// sequence was not admitted by independent authority. The authority intent and
// exact resolution are persisted before the command-store transaction commits.
func (r *CommandRepository) RejectCommandProvenance(ctx context.Context, commandID string, sequence uint64, rejectedAt time.Time, authority TrustedCommandProvenanceRejectionAuthority) (Command, error) {
	if r == nil || isNilRepositoryDependency(r.reader.store) || isNilRepositoryDependency(r.reader.verifier) {
		return Command{}, fmt.Errorf("%w: command repository is not fully bound", ErrCommandProvenanceRejection)
	}
	if isNilRepositoryDependency(authority) {
		return Command{}, fmt.Errorf("%w: rejection authority is required", ErrCommandProvenanceRejection)
	}
	if err := validateCommandIdentity("rejected command id", commandID); err != nil || sequence == 0 {
		return Command{}, fmt.Errorf("%w: invalid command identity or sequence: %w", ErrCommandProvenanceRejection, err)
	}
	if err := validateCommandTime("provenance rejected_at", rejectedAt); err != nil {
		return Command{}, fmt.Errorf("%w: %w", ErrCommandProvenanceRejection, err)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return Command{}, err
	}
	beforeState, err := r.State(ctx)
	if err != nil {
		beforeState, err = r.repairLineage(ctx, "pre-provenance-rejection lineage repair")
		if err != nil {
			return Command{}, err
		}
	}

	var (
		result     Command
		resolution CommandProvenanceRejectionResolution
	)
	err = r.reader.store.AtomicReadWrite(ctx, "gc: reject unauthorized durable nudge command provenance", func(tx beads.AtomicReadWriteTx) error {
		state, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		if err := validateCommandRepositoryStateAdvance(beforeState, state); err != nil {
			return err
		}
		if state.Revision == math.MaxUint64 {
			return &CommandRepositorySchemaSkewError{Field: "repository revision", Found: fmt.Sprint(state.Revision), Want: "allocatable uint64"}
		}
		row, err := tx.GetIssue(commandID)
		if err != nil {
			return fmt.Errorf("%w: reading command %q: %w", ErrCommandProvenanceRejection, commandID, err)
		}
		entry, _, err := decodeCommandRecord(row, state)
		if err != nil {
			return err
		}
		if entry.Command == nil {
			return fmt.Errorf("%w: command %q is opaque and requires preserved-wire quarantine", ErrCommandProvenanceRejection, commandID)
		}
		before := cloneCommandValue(*entry.Command)
		if before.ID != commandID || before.Order.Sequence != sequence || before.Terminal != nil || commandIsTerminalState(before.State) {
			return fmt.Errorf("%w: command does not match an active undecided sequence", ErrCommandProvenanceRejection)
		}
		beforeWire, err := EncodeCommandV1(before)
		if err != nil {
			return err
		}
		intent := CommandProvenanceRejectionIntent{
			Store: before.Store, CommandID: before.ID, Sequence: before.Order.Sequence,
			AllocationRevision: before.Order.Revision, BeforeCommandRevision: before.Order.Revision,
			IdentityDigest:      commandProvenanceIdentityDigest(before.Store, before.ID, before.Order.Sequence, before.Order.Revision),
			BeforeCommandDigest: sha256.Sum256(beforeWire),
			Reason:              CommandProvenanceRejectionReasonUnauthorized, RejectedAt: rejectedAt,
		}
		if err := authority.PrepareCommandProvenanceRejection(ctx, intent); err != nil {
			return fmt.Errorf("%w: preparing rejection: %w", ErrCommandProvenanceRejection, err)
		}
		after, err := terminalizeUnauthorizedProvenance(before, rejectedAt)
		if err != nil {
			return err
		}
		after.Order.Revision = state.Revision + 1
		afterWire, err := EncodeCommandV1(after)
		if err != nil {
			return err
		}
		resolution = CommandProvenanceRejectionResolution{
			Intent: intent, RepositoryRevision: after.Order.Revision, CommandDigest: sha256.Sum256(afterWire),
		}
		if err := authority.VerifyCommandProvenanceRejectionPreparation(ctx, intent); err != nil {
			return fmt.Errorf("%w: verifying rejection preparation: %w", ErrCommandProvenanceRejection, err)
		}
		result, err = writeCommandTransition(tx, state, row, after)
		return err
	})
	if err != nil {
		return Command{}, err
	}
	if _, err := r.repairLineage(ctx, "post-provenance-rejection lineage advance"); err != nil {
		return Command{}, err
	}
	if err := authority.RecordCommandProvenanceRejection(ctx, resolution); err != nil {
		return result, fmt.Errorf("%w: finalizing rejection: %w", ErrCommandProvenanceRejection, err)
	}
	return result, nil
}

func terminalizeUnauthorizedProvenance(command Command, rejectedAt time.Time) (Command, error) {
	if command.Terminal != nil || commandIsTerminalState(command.State) {
		return Command{}, fmt.Errorf("%w: command is already terminal", ErrCommandProvenanceRejection)
	}
	if rejectedAt.Before(command.CreatedAt) {
		rejectedAt = command.CreatedAt
	}
	command.State = CommandStateDeadLettered
	command.Claim = nil
	command.Retry = nil
	if command.Target.Policy == TargetPolicyContinuation {
		command.Binding = nil
	}
	command.Terminal = &CommandTerminal{
		At: rejectedAt, ActionResult: CommandActionResultUnauthorizedProvenance,
		ErrorClass:    CommandErrorClassUnauthorizedProvenance,
		Detail:        "command has no trusted ingress admission",
		ProviderStage: ProviderStageNotEntered, Completion: CompletionStateNotCompleted,
	}
	if _, err := EncodeCommandV1(command); err != nil {
		return Command{}, fmt.Errorf("%w: building terminal command: %w", ErrCommandProvenanceRejection, err)
	}
	return command, nil
}

func commandProvenanceIdentityDigest(store CommandStoreBinding, commandID string, sequence, allocationRevision uint64) [sha256.Size]byte {
	digest := sha256.New()
	_, _ = io.WriteString(digest, commandProvenanceIdentityDomainV1)
	for _, value := range []string{store.StoreUUID, commandID} {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(value)))
		_, _ = digest.Write(length[:])
		_, _ = io.WriteString(digest, value)
	}
	for _, value := range []uint64{store.RestoreEpoch, sequence, allocationRevision} {
		var wire [8]byte
		binary.BigEndian.PutUint64(wire[:], value)
		_, _ = digest.Write(wire[:])
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result
}
