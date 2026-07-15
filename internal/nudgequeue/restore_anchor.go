package nudgequeue

import (
	"errors"
	"fmt"
	"math"
)

const (
	// RestoreAnchorVersion1 is the first independent nudge-command restore
	// anchor format.
	RestoreAnchorVersion1 uint32 = 1
)

// ErrRestoreAnchorAdmission reports that independent restore evidence does
// not currently authorize effects. Inspect RestoreAnchorDecisionError for
// the closed disposition and retained recovery evidence.
var ErrRestoreAnchorAdmission = errors.New("nudge command restore anchor denies effect admission")

// RestoreAnchor is independent durable evidence of the greatest command-store
// revision and dense command sequence accepted in one store lineage. It must
// live outside the database it fences. Store identity is provisioned authority
// and is never derived from a project identifier or filesystem path.
type RestoreAnchor struct {
	Version                 uint32              `json:"version"`
	Store                   CommandStoreBinding `json:"store"`
	HighestAcceptedRevision uint64              `json:"highest_accepted_revision"`
	HighestAcceptedSequence uint64              `json:"highest_accepted_sequence"`
}

// RestoreAnchorDisposition is the closed result of comparing independent
// anchor evidence with one transaction-consistent command repository state.
type RestoreAnchorDisposition string

const (
	// RestoreAnchorFirstInitialization means no anchor exists and explicit
	// provisioning must durably install Candidate before effects are admitted.
	RestoreAnchorFirstInitialization RestoreAnchorDisposition = "first_initialization"
	// RestoreAnchorEqual means the database exactly matches the durable anchor
	// high-water and effects may be admitted.
	RestoreAnchorEqual RestoreAnchorDisposition = "equal"
	// RestoreAnchorAdvanceRequired means the database advanced normally, but
	// Candidate must be durably anchored before effects at that revision.
	RestoreAnchorAdvanceRequired RestoreAnchorDisposition = "advance_required"
	// RestoreAnchorDatabaseRewind means the database revision or restore epoch
	// regressed. Effects remain frozen until explicit recovery advances the
	// epoch and quarantines recovered effectful work.
	RestoreAnchorDatabaseRewind RestoreAnchorDisposition = "database_rewind"
	// RestoreAnchorForeignStore means the provisioned database identity differs
	// from the independent anchor identity.
	RestoreAnchorForeignStore RestoreAnchorDisposition = "foreign_store"
	// RestoreAnchorEpochAdvance means the database presents a newer restore
	// epoch. It is evidence of a recovery transition, not authorization to skip
	// quarantine and re-anchoring.
	RestoreAnchorEpochAdvance RestoreAnchorDisposition = "epoch_advance"
	// RestoreAnchorInvalid means anchor evidence is corrupt, unavailable,
	// internally invalid, or regressed below evidence already accepted by this
	// process. It always fails closed.
	RestoreAnchorInvalid RestoreAnchorDisposition = "anchor_invalid"
)

// RestoreAnchorCheck is the complete pure input to [DecideRestoreAnchor].
// AnchorReadFailed must be set when loading the persisted anchor returned any
// error; a read error is never equivalent to a missing first-time anchor.
// PreviouslyAccepted detects deletion, substitution, or rewind while a
// controller remains alive. Detecting a valid historical replay across a full
// host restore requires an external monotonic anchor and is outside this local
// file's guarantee.
type RestoreAnchorCheck struct {
	Persisted                 *RestoreAnchor
	PreviouslyAccepted        *RestoreAnchor
	AnchorReadFailed          bool
	DatabaseStore             CommandStoreBinding
	DatabaseRevision          uint64
	DatabaseSequenceHighWater uint64
}

// RestoreAnchorDecision contains the admission result and the exact retained
// recovery evidence. Candidate is a fresh value and never aliases caller
// input. MinimumRecoveryEpoch is non-zero only when a safe recovery epoch can
// be represented.
type RestoreAnchorDecision struct {
	Disposition             RestoreAnchorDisposition
	EffectsAllowed          bool
	RetainedHighestRevision uint64
	RetainedHighestSequence uint64
	MinimumRecoveryEpoch    uint64
	Candidate               *RestoreAnchor
}

// RestoreAnchorDecisionError is the typed fail-closed result returned by
// [RestoreAnchorDecision.AdmissionError].
type RestoreAnchorDecisionError struct {
	Decision RestoreAnchorDecision
}

// Error implements error.
func (e *RestoreAnchorDecisionError) Error() string {
	return fmt.Sprintf("%v: %s", ErrRestoreAnchorAdmission, e.Decision.Disposition)
}

// Unwrap exposes [ErrRestoreAnchorAdmission].
func (e *RestoreAnchorDecisionError) Unwrap() error {
	return ErrRestoreAnchorAdmission
}

// AdmissionError returns nil only when the decision authorizes effects.
func (d RestoreAnchorDecision) AdmissionError() error {
	if d.EffectsAllowed {
		return nil
	}
	return &RestoreAnchorDecisionError{Decision: d}
}

// ValidateRestoreAnchor checks the complete known-version anchor contract.
func ValidateRestoreAnchor(anchor RestoreAnchor) error {
	if anchor.Version != RestoreAnchorVersion1 {
		return fmt.Errorf("restore anchor version %d is unsupported", anchor.Version)
	}
	if err := ValidateCommandStoreBinding(anchor.Store); err != nil {
		return fmt.Errorf("restore anchor store binding: %w", err)
	}
	if anchor.HighestAcceptedSequence > anchor.HighestAcceptedRevision {
		return fmt.Errorf("restore anchor sequence high-water %d exceeds revision %d", anchor.HighestAcceptedSequence, anchor.HighestAcceptedRevision)
	}
	return nil
}

// DecideRestoreAnchor compares independent local evidence with database
// authority without I/O or mutation. Only an exact equal decision admits
// effects. A normal database advance first returns AdvanceRequired so callers
// must persist Candidate, then compare again. Rewind returns both the retained
// revision and the minimum strictly newer restore epoch required by recovery.
func DecideRestoreAnchor(check RestoreAnchorCheck) RestoreAnchorDecision {
	if check.AnchorReadFailed {
		return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
	}
	if err := ValidateCommandStoreBinding(check.DatabaseStore); err != nil {
		return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
	}
	if check.DatabaseSequenceHighWater > check.DatabaseRevision {
		return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
	}
	if check.PreviouslyAccepted != nil {
		if err := ValidateRestoreAnchor(*check.PreviouslyAccepted); err != nil {
			return invalidRestoreAnchorDecision(nil)
		}
	}
	if check.Persisted == nil {
		if check.PreviouslyAccepted != nil {
			return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
		}
		candidate := newRestoreAnchor(check.DatabaseStore, check.DatabaseRevision, check.DatabaseSequenceHighWater)
		return RestoreAnchorDecision{
			Disposition:          RestoreAnchorFirstInitialization,
			MinimumRecoveryEpoch: check.DatabaseStore.RestoreEpoch,
			Candidate:            &candidate,
		}
	}
	anchor := *check.Persisted
	if err := ValidateRestoreAnchor(anchor); err != nil {
		return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
	}
	if check.PreviouslyAccepted != nil && restoreAnchorRegressed(anchor, *check.PreviouslyAccepted) {
		return invalidRestoreAnchorDecision(check.PreviouslyAccepted)
	}

	base := RestoreAnchorDecision{
		RetainedHighestRevision: anchor.HighestAcceptedRevision,
		RetainedHighestSequence: anchor.HighestAcceptedSequence,
	}
	if anchor.Store.StoreUUID != check.DatabaseStore.StoreUUID {
		base.Disposition = RestoreAnchorForeignStore
		return base
	}
	minimumEpoch, ok := nextRestoreEpoch(anchor.Store.RestoreEpoch)
	if check.DatabaseStore.RestoreEpoch < anchor.Store.RestoreEpoch {
		if !ok {
			return invalidRestoreAnchorDecision(&anchor)
		}
		base.Disposition = RestoreAnchorDatabaseRewind
		base.MinimumRecoveryEpoch = minimumEpoch
		return base
	}
	if check.DatabaseStore.RestoreEpoch > anchor.Store.RestoreEpoch {
		minimumEpoch, ok = nextRestoreEpoch(check.DatabaseStore.RestoreEpoch)
		if !ok {
			return invalidRestoreAnchorDecision(&anchor)
		}
		base.Disposition = RestoreAnchorEpochAdvance
		base.MinimumRecoveryEpoch = minimumEpoch
		return base
	}
	if check.DatabaseRevision < anchor.HighestAcceptedRevision || check.DatabaseSequenceHighWater < anchor.HighestAcceptedSequence {
		if !ok {
			return invalidRestoreAnchorDecision(&anchor)
		}
		base.Disposition = RestoreAnchorDatabaseRewind
		base.MinimumRecoveryEpoch = minimumEpoch
		return base
	}
	if check.DatabaseRevision == anchor.HighestAcceptedRevision && check.DatabaseSequenceHighWater == anchor.HighestAcceptedSequence {
		base.Disposition = RestoreAnchorEqual
		base.EffectsAllowed = true
		return base
	}
	candidate := newRestoreAnchor(check.DatabaseStore, check.DatabaseRevision, check.DatabaseSequenceHighWater)
	base.Disposition = RestoreAnchorAdvanceRequired
	base.Candidate = &candidate
	return base
}

func newRestoreAnchor(store CommandStoreBinding, revision, sequence uint64) RestoreAnchor {
	return RestoreAnchor{
		Version:                 RestoreAnchorVersion1,
		Store:                   store,
		HighestAcceptedRevision: revision,
		HighestAcceptedSequence: sequence,
	}
}

func restoreAnchorRegressed(current, prior RestoreAnchor) bool {
	if current.Store.StoreUUID != prior.Store.StoreUUID {
		return true
	}
	if current.Store.RestoreEpoch != prior.Store.RestoreEpoch {
		return current.Store.RestoreEpoch < prior.Store.RestoreEpoch
	}
	return current.HighestAcceptedRevision < prior.HighestAcceptedRevision || current.HighestAcceptedSequence < prior.HighestAcceptedSequence
}

func invalidRestoreAnchorDecision(retained *RestoreAnchor) RestoreAnchorDecision {
	decision := RestoreAnchorDecision{Disposition: RestoreAnchorInvalid}
	if retained != nil && ValidateRestoreAnchor(*retained) == nil {
		decision.RetainedHighestRevision = retained.HighestAcceptedRevision
		decision.RetainedHighestSequence = retained.HighestAcceptedSequence
		if epoch, ok := nextRestoreEpoch(retained.Store.RestoreEpoch); ok {
			decision.MinimumRecoveryEpoch = epoch
		}
	}
	return decision
}

func nextRestoreEpoch(current uint64) (uint64, bool) {
	if current == math.MaxUint64 {
		return 0, false
	}
	return current + 1, true
}
