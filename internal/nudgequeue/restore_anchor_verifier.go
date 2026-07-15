package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

const maxRestoreAnchorVerifierAttempts = 4

// RestoreAnchorRepositoryVerifier binds the durable command repository to its
// independent local restore anchor. It serializes local decisions and retains
// the last durably accepted high-water so deletion or rewind during this
// process cannot look like first provisioning.
type RestoreAnchorRepositoryVerifier struct {
	path string
	ops  restoreAnchorFileOps

	mu                          sync.Mutex
	previouslyAccepted          *RestoreAnchor
	needsDurabilityConfirmation bool
}

var (
	_ CommandRepositoryLineageVerifier   = (*RestoreAnchorRepositoryVerifier)(nil)
	_ CommandRepositoryLineageWriter     = (*RestoreAnchorRepositoryVerifier)(nil)
	_ CommandRepositoryLineageController = (*RestoreAnchorRepositoryVerifier)(nil)
)

// NewRestoreAnchorRepositoryVerifier constructs the production local verifier
// for cityPath. cityPath selects the independent file location only; it never
// supplies or derives store identity.
func NewRestoreAnchorRepositoryVerifier(cityPath string) *RestoreAnchorRepositoryVerifier {
	return newRestoreAnchorRepositoryVerifier(RestoreAnchorPath(cityPath), osRestoreAnchorFileOps)
}

func newRestoreAnchorRepositoryVerifier(path string, ops restoreAnchorFileOps) *RestoreAnchorRepositoryVerifier {
	return &RestoreAnchorRepositoryVerifier{path: path, ops: ops}
}

// VerifyCommandRepositoryLineage verifies existing repository authority. It
// performs no file write, fsync, rename, provisioning, or anchor advancement.
// Only an exact durable-anchor match succeeds; every database-ahead state,
// rewind, foreign store, epoch change, corruption, or missing anchor fails
// closed until an explicit writer operation runs.
func (v *RestoreAnchorRepositoryVerifier) VerifyCommandRepositoryLineage(ctx context.Context, state CommandRepositoryState) error {
	if v == nil {
		return restoreAnchorVerifierInvalidError(nil, errors.New("restore anchor repository verifier is nil"))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.verifyReadLocked(ctx, state)
}

// ProvisionCommandRepositoryLineage initializes a missing anchor only for the
// in-memory one-shot evidence produced by the exact all-absent repository
// initialization winner. Evidence read from a database or reconstructed after
// restart cannot call this path.
func (v *RestoreAnchorRepositoryVerifier) ProvisionCommandRepositoryLineage(ctx context.Context, state CommandRepositoryState, evidence CommandRepositoryProvisioningEvidence) error {
	if v == nil {
		return restoreAnchorVerifierInvalidError(nil, errors.New("restore anchor repository verifier is nil"))
	}
	if !evidence.validFor(state) {
		return restoreAnchorVerifierInvalidError(nil, errors.New("one-shot repository provisioning evidence is invalid"))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.writeLocked(ctx, state, restoreAnchorRepositoryWriteProvision)
}

// AdvanceCommandRepositoryLineage explicitly repairs durability uncertainty or
// advances an existing anchor to a database-ahead high-water in the same store
// UUID and restore epoch. It never provisions a missing anchor or accepts an
// epoch transition.
func (v *RestoreAnchorRepositoryVerifier) AdvanceCommandRepositoryLineage(ctx context.Context, state CommandRepositoryState) error {
	if v == nil {
		return restoreAnchorVerifierInvalidError(nil, errors.New("restore anchor repository verifier is nil"))
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.writeLocked(ctx, state, restoreAnchorRepositoryWriteAdvance)
}

func (v *RestoreAnchorRepositoryVerifier) verifyReadLocked(ctx context.Context, state CommandRepositoryState) error {
	database, err := restoreAnchorFromRepositoryStateChecked(state)
	if err != nil {
		return restoreAnchorVerifierInvalidError(v.previouslyAccepted, err)
	}
	persisted, exists, err := LoadRestoreAnchor(ctx, v.path)
	if err != nil {
		return restoreAnchorVerifierInvalidError(v.previouslyAccepted, err)
	}
	var persistedPtr *RestoreAnchor
	if exists {
		persistedCopy := persisted
		persistedPtr = &persistedCopy
	}
	decision := DecideRestoreAnchor(RestoreAnchorCheck{
		Persisted:                 persistedPtr,
		PreviouslyAccepted:        v.previouslyAccepted,
		DatabaseStore:             database.Store,
		DatabaseRevision:          database.HighestAcceptedRevision,
		DatabaseSequenceHighWater: database.HighestAcceptedSequence,
	})
	if decision.Disposition != RestoreAnchorEqual || !decision.EffectsAllowed {
		return decision.AdmissionError()
	}
	if v.needsDurabilityConfirmation {
		return errors.Join(
			invalidRestoreAnchorDecision(v.previouslyAccepted).AdmissionError(),
			ErrRestoreAnchorDurabilityUncertain,
			errors.New("explicit lineage repair is required before accepting an ambiguously published anchor"),
		)
	}
	accepted := persisted
	v.previouslyAccepted = &accepted
	return nil
}

type restoreAnchorRepositoryWriteOperation uint8

const (
	restoreAnchorRepositoryWriteProvision restoreAnchorRepositoryWriteOperation = iota + 1
	restoreAnchorRepositoryWriteAdvance
)

func (v *RestoreAnchorRepositoryVerifier) writeLocked(ctx context.Context, state CommandRepositoryState, operation restoreAnchorRepositoryWriteOperation) error {
	database, err := restoreAnchorFromRepositoryStateChecked(state)
	if err != nil {
		return restoreAnchorVerifierInvalidError(v.previouslyAccepted, err)
	}
	var confirmed *RestoreAnchor
	var lastDecision RestoreAnchorDecision
	for attempt := 0; attempt < maxRestoreAnchorVerifierAttempts; attempt++ {
		persisted, exists, loadErr := LoadRestoreAnchor(ctx, v.path)
		if loadErr != nil {
			return restoreAnchorVerifierInvalidError(v.previouslyAccepted, loadErr)
		}
		var persistedPtr *RestoreAnchor
		if exists {
			persistedCopy := persisted
			persistedPtr = &persistedCopy
		}
		decision := DecideRestoreAnchor(RestoreAnchorCheck{
			Persisted:                 persistedPtr,
			PreviouslyAccepted:        v.previouslyAccepted,
			DatabaseStore:             database.Store,
			DatabaseRevision:          database.HighestAcceptedRevision,
			DatabaseSequenceHighWater: database.HighestAcceptedSequence,
		})
		lastDecision = decision

		switch decision.Disposition {
		case RestoreAnchorEqual:
			if !v.anchorDurabilityKnown(persisted, confirmed) {
				if err := writeRestoreAnchor(ctx, v.path, &persisted, persisted, RestoreAnchorWriteAdvance, 0, v.ops); err != nil {
					if errors.Is(err, ErrRestoreAnchorBusy) || errors.Is(err, ErrRestoreAnchorConflict) {
						continue
					}
					if errors.Is(err, ErrRestoreAnchorDurabilityUncertain) {
						v.needsDurabilityConfirmation = true
					}
					return restoreAnchorVerifierInvalidError(v.previouslyAccepted, err)
				}
				confirmedValue := persisted
				confirmed = &confirmedValue
				v.needsDurabilityConfirmation = false
				continue
			}
			accepted := persisted
			v.previouslyAccepted = &accepted
			v.needsDurabilityConfirmation = false
			return nil

		case RestoreAnchorAdvanceRequired:
			if operation != restoreAnchorRepositoryWriteAdvance {
				return errors.Join(decision.AdmissionError(), errors.New("repository provisioning cannot advance an existing restore anchor"))
			}
			if decision.Candidate == nil || persistedPtr == nil {
				return errors.Join(decision.AdmissionError(), errors.New("restore anchor advance has no exact candidate or prior"))
			}
			candidate := *decision.Candidate
			if err := writeRestoreAnchor(ctx, v.path, persistedPtr, candidate, RestoreAnchorWriteAdvance, 0, v.ops); err != nil {
				if errors.Is(err, ErrRestoreAnchorBusy) || errors.Is(err, ErrRestoreAnchorConflict) {
					continue
				}
				if errors.Is(err, ErrRestoreAnchorDurabilityUncertain) {
					v.needsDurabilityConfirmation = true
				}
				return errors.Join(decision.AdmissionError(), fmt.Errorf("advancing independent restore anchor: %w", err))
			}
			confirmed = &candidate
			v.needsDurabilityConfirmation = false
			continue

		case RestoreAnchorFirstInitialization:
			if operation != restoreAnchorRepositoryWriteProvision {
				return decision.AdmissionError()
			}
			if decision.Candidate == nil || persistedPtr != nil {
				return errors.Join(decision.AdmissionError(), errors.New("restore anchor provisioning has no exact missing-to-present transition"))
			}
			candidate := *decision.Candidate
			if err := writeRestoreAnchor(ctx, v.path, nil, candidate, RestoreAnchorWriteInitialize, 0, v.ops); err != nil {
				if errors.Is(err, ErrRestoreAnchorBusy) || errors.Is(err, ErrRestoreAnchorConflict) {
					continue
				}
				if errors.Is(err, ErrRestoreAnchorDurabilityUncertain) {
					v.needsDurabilityConfirmation = true
				}
				return errors.Join(decision.AdmissionError(), fmt.Errorf("provisioning independent restore anchor: %w", err))
			}
			confirmed = &candidate
			v.needsDurabilityConfirmation = false
			continue

		default:
			return decision.AdmissionError()
		}
	}
	if lastDecision.EffectsAllowed {
		lastDecision = invalidRestoreAnchorDecision(v.previouslyAccepted)
	}
	return errors.Join(lastDecision.AdmissionError(), ErrRestoreAnchorBusy, errors.New("restore anchor did not stabilize within the bounded CAS retry"))
}

func (v *RestoreAnchorRepositoryVerifier) anchorDurabilityKnown(persisted RestoreAnchor, confirmed *RestoreAnchor) bool {
	if v.needsDurabilityConfirmation {
		return false
	}
	if confirmed != nil && *confirmed == persisted {
		return true
	}
	return v.previouslyAccepted != nil && *v.previouslyAccepted == persisted
}

func restoreAnchorFromRepositoryState(state CommandRepositoryState) RestoreAnchor {
	return newRestoreAnchor(state.Store, state.Revision, state.SequenceHighWater)
}

func restoreAnchorFromRepositoryStateChecked(state CommandRepositoryState) (RestoreAnchor, error) {
	if state.SchemaVersion != CommandRepositorySchemaVersion {
		return RestoreAnchor{}, fmt.Errorf("command repository schema version %d does not match %d", state.SchemaVersion, CommandRepositorySchemaVersion)
	}
	if state.WriterVersion != CommandRepositoryWriterVersion {
		return RestoreAnchor{}, fmt.Errorf("command repository writer version %d does not match %d", state.WriterVersion, CommandRepositoryWriterVersion)
	}
	anchor := restoreAnchorFromRepositoryState(state)
	if err := ValidateRestoreAnchor(anchor); err != nil {
		return RestoreAnchor{}, err
	}
	return anchor, nil
}

func restoreAnchorVerifierInvalidError(previouslyAccepted *RestoreAnchor, cause error) error {
	decision := invalidRestoreAnchorDecision(previouslyAccepted)
	if cause == nil {
		return decision.AdmissionError()
	}
	return errors.Join(decision.AdmissionError(), cause)
}
