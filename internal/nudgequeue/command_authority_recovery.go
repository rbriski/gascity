package nudgequeue

import (
	"context"
	"errors"
	"fmt"
)

// TrustedCommandAuthorityRecovery is the sole startup recovery contract for
// durable command authority. Success proves repository lineage is current,
// every allocated sequence through the repository high-water has an admission
// or rejection decision, and every retained claim preparation is an exact
// recoverable in-flight pre-entry intent. Other write-ahead families are fully
// resolved before success.
type TrustedCommandAuthorityRecovery interface {
	RecoverCommandAuthority(context.Context, *CommandRepository) error
}

// RecoverCommandAuthority repairs the complete local authority journal in the
// only safe startup order. It retries monotonic repository movement until one
// final fence is stable, and fails closed on opaque commands, schema or store
// skew, conflicting evidence, cancellation, or an impossible decision fence.
func (a *LocalNudgeAuthority) RecoverCommandAuthority(ctx context.Context, repository *CommandRepository) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	release()
	if repository == nil {
		return fmt.Errorf("%w: command repository is required", ErrLocalNudgeAuthorityConflict)
	}
	ctx, budget := withCommandAuthorityRecoveryBudget(ctx)
	for {
		if err := budget.takePass("recovering local command authority"); err != nil {
			return err
		}
		stable, err := a.recoverCommandAuthorityPass(ctx, repository)
		if err != nil {
			return err
		}
		if stable {
			return nil
		}
	}
}

func (a *LocalNudgeAuthority) recoverCommandAuthorityPass(ctx context.Context, repository *CommandRepository) (bool, error) {
	passState, err := repository.RepairLineage(ctx)
	if err != nil {
		if commandAuthorityRepositoryMovedAfterLineageError(ctx, repository, err) {
			return false, nil
		}
		return false, fmt.Errorf("recovering local command authority: repairing repository lineage: %w", err)
	}
	if err := a.RepairCommandPartitionAdmissions(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "repairing admissions", err)
	}
	if err := a.RepairCommandPartitionTerminals(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "repairing admitted terminals", err)
	}
	if err := a.RepairCommandProvenanceRejections(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "auditing provenance decisions", err)
	}

	// Audit can overlap a new write-ahead preparation. Re-run every preparation
	// repair before taking the final fence; the fence itself never guesses how
	// to resolve residual state.
	if err := a.RepairCommandPartitionAdmissions(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "rechecking admissions", err)
	}
	if err := a.RepairCommandPartitionTerminals(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "rechecking admitted terminals", err)
	}
	if err := a.repairProvenanceRejectionPreparations(ctx, repository); err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, passState, "rechecking provenance rejections", err)
	}
	claimState, err := repository.RepairLineage(ctx)
	if err != nil {
		if commandAuthorityRepositoryMovedAfterLineageError(ctx, repository, err) {
			return false, nil
		}
		return false, fmt.Errorf("recovering local command authority: fencing claim transition audit: %w", err)
	}
	claimToken, stable, err := a.repairCommandClaimTransitions(ctx, repository, claimState)
	if err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, claimState, "auditing claim transitions", err)
	}
	if !stable {
		return false, nil
	}

	observed, err := a.snapshotObservedRepositoryHighWater(ctx)
	if err != nil {
		return false, err
	}
	state, err := repository.RepairLineage(ctx)
	if err != nil {
		if commandAuthorityRepositoryMovedAfterLineageError(ctx, repository, err) {
			return false, nil
		}
		return false, fmt.Errorf("recovering local command authority: fencing repository lineage: %w", err)
	}
	stable, err = a.verifyCommandAuthorityRecoveryFence(ctx, state, observed, claimToken)
	if err != nil {
		return false, err
	}
	if !stable {
		return false, nil
	}
	confirmed, err := repository.State(ctx)
	if err != nil {
		return retryCommandAuthorityRecoveryAfterStageError(ctx, repository, state, "confirming repository fence", err)
	}
	if confirmed != state {
		return false, nil
	}
	confirmedClaimToken, claimAuditDone, err := a.completedClaimAuditToken(ctx, confirmed)
	if err != nil {
		return false, err
	}
	if !claimAuditDone || confirmedClaimToken != claimToken {
		return false, nil
	}
	return true, nil
}

func commandAuthorityRepositoryMovedAfterLineageError(ctx context.Context, repository *CommandRepository, lineageErr error) bool {
	var typed *CommandRepositoryLineageError
	if !errors.As(lineageErr, &typed) {
		return false
	}
	current, err := repository.State(ctx)
	if err != nil || current.Store != typed.State.Store || current.SchemaVersion != typed.State.SchemaVersion ||
		current.WriterVersion != typed.State.WriterVersion || current.Revision < typed.State.Revision ||
		current.SequenceHighWater < typed.State.SequenceHighWater {
		return false
	}
	return current.Revision != typed.State.Revision || current.SequenceHighWater != typed.State.SequenceHighWater
}

func retryCommandAuthorityRecoveryAfterStageError(
	ctx context.Context,
	repository *CommandRepository,
	before CommandRepositoryState,
	operation string,
	stageErr error,
) (bool, error) {
	moved, movementErr := commandAuthorityRepositoryAdvancedAfterRecoveryError(ctx, repository, before, stageErr)
	if movementErr != nil {
		return false, fmt.Errorf("recovering local command authority: %s: %w", operation, movementErr)
	}
	if moved {
		return false, nil
	}
	return false, fmt.Errorf("recovering local command authority: %s: %w", operation, stageErr)
}

func commandAuthorityRepositoryAdvancedAfterRecoveryError(
	ctx context.Context,
	repository *CommandRepository,
	before CommandRepositoryState,
	recoveryErr error,
) (bool, error) {
	var typed *CommandRepositoryLineageError
	if !errors.As(recoveryErr, &typed) {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	current, err := repository.RepairLineage(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		return false, errors.Join(recoveryErr, fmt.Errorf("repairing repository lineage after concurrent movement: %w", err))
	}
	if current.Store != before.Store || current.SchemaVersion != before.SchemaVersion || current.WriterVersion != before.WriterVersion ||
		current.Revision < before.Revision || current.SequenceHighWater < before.SequenceHighWater {
		return false, nil
	}
	return current.Revision != before.Revision || current.SequenceHighWater != before.SequenceHighWater, nil
}

func (a *LocalNudgeAuthority) verifyCommandAuthorityRecoveryFence(
	ctx context.Context,
	state CommandRepositoryState,
	observed localAuthorityRepositoryHighWater,
	claimToken localAuthorityClaimRecoveryToken,
) (bool, error) {
	if err := a.validateRecoveryState(state, observed); err != nil {
		return false, err
	}
	currentClaimToken, claimAuditDone, err := a.completedClaimAuditToken(ctx, state)
	if err != nil {
		return false, err
	}
	if !claimAuditDone || currentClaimToken != claimToken {
		return false, nil
	}
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	var denseWire, highestSequenceWire, highestRevisionWire []byte
	var admissionPreparations, terminalPreparations, rejectionPreparations int
	if err := a.db.QueryRowContext(ctx, `SELECT dense_decision_high_water, highest_observed_sequence, highest_observed_revision,
		(SELECT COUNT(*) FROM admission_preparations),
		(SELECT COUNT(*) FROM terminal_preparations),
		(SELECT COUNT(*) FROM rejection_preparations)
		FROM authority_meta WHERE singleton = 1`).Scan(
		&denseWire, &highestSequenceWire, &highestRevisionWire,
		&admissionPreparations, &terminalPreparations, &rejectionPreparations,
	); err != nil {
		return false, fmt.Errorf("recovering local command authority: reading final fence: %w", err)
	}
	dense, err := decodeLocalAuthorityUint64(denseWire)
	if err != nil {
		return false, err
	}
	highestSequence, err := decodeLocalAuthorityUint64(highestSequenceWire)
	if err != nil {
		return false, err
	}
	highestRevision, err := decodeLocalAuthorityUint64(highestRevisionWire)
	if err != nil {
		return false, err
	}
	if highestSequence < observed.sequence || highestRevision < observed.revision {
		return false, fmt.Errorf("%w: final authority high-water %d/%d rewound below ordered snapshot %d/%d",
			ErrLocalNudgeAuthorityConflict, highestRevision, highestSequence, observed.revision, observed.sequence)
	}
	if highestSequence != observed.sequence || highestRevision != observed.revision {
		return false, nil
	}
	if admissionPreparations != 0 || terminalPreparations != 0 || rejectionPreparations != 0 {
		return false, nil
	}
	if dense > state.SequenceHighWater {
		return false, fmt.Errorf("%w: final command authority decision prefix %d exceeds repository sequence %d",
			ErrLocalNudgeAuthorityConflict, dense, state.SequenceHighWater)
	}
	return dense == state.SequenceHighWater, nil
}

var _ TrustedCommandAuthorityRecovery = (*LocalNudgeAuthority)(nil)
