package nudgequeue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"
)

func (a *LocalNudgeAuthority) validateRetryMetadata(ctx context.Context, state CommandRepositoryState) error {
	var generationWire, preparationCountWire, receiptCountWire []byte
	var auditGenerationWire, auditRevisionWire, auditSequenceWire []byte
	var phase, afterCommandID, afterAttemptID string
	var identityWire, auditPreparationCountWire, auditReceiptCountWire, checkpointDigest []byte
	if err := a.db.QueryRowContext(ctx, `SELECT
		transition_generation, preparation_count, receipt_count,
		audit_generation, audit_repository_revision, audit_sequence_high_water, audit_phase,
		audit_after_command_id, audit_after_attempt_id, audit_identity,
		audit_preparation_count, audit_receipt_count, audit_checkpoint_digest
		FROM retry_meta WHERE singleton = 1`).Scan(
		&generationWire, &preparationCountWire, &receiptCountWire,
		&auditGenerationWire, &auditRevisionWire, &auditSequenceWire, &phase,
		&afterCommandID, &afterAttemptID, &identityWire,
		&auditPreparationCountWire, &auditReceiptCountWire, &checkpointDigest,
	); err != nil {
		return fmt.Errorf("%w: reading retry authority metadata: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	values := make([]uint64, 0, 8)
	for _, wire := range [][]byte{
		generationWire, preparationCountWire, receiptCountWire,
		auditGenerationWire, auditRevisionWire, auditSequenceWire,
		auditPreparationCountWire, auditReceiptCountWire,
	} {
		value, err := decodeLocalAuthorityUint64(wire)
		if err != nil {
			return err
		}
		values = append(values, value)
	}
	generation, preparationCount, receiptCount := values[0], values[1], values[2]
	auditGeneration, auditRevision, auditSequence := values[3], values[4], values[5]
	auditPreparationCount, auditReceiptCount := values[6], values[7]
	if preparationCount > generation || receiptCount > generation || auditGeneration > generation ||
		auditRevision > state.Revision || auditSequence > state.SequenceHighWater || auditSequence > auditRevision ||
		auditPreparationCount > preparationCount || auditReceiptCount > receiptCount {
		return fmt.Errorf("%w: retry authority metadata high-waters are inconsistent", ErrLocalNudgeAuthorityConflict)
	}
	if phase != "idle" || auditGeneration != 0 || auditRevision != 0 || auditSequence != 0 ||
		afterCommandID != "" || afterAttemptID != "" || auditPreparationCount != 0 || auditReceiptCount != 0 {
		return fmt.Errorf("%w: unsupported non-idle retry audit checkpoint", ErrLocalNudgeAuthorityConflict)
	}
	initialIdentity := sha256.Sum256([]byte("gascity.local-retry-audit.v1"))
	wantDigest := localAuthorityRetryAuditCheckpointDigest(a.store, a.opts.AuthorityID, initialIdentity)
	if !bytes.Equal(identityWire, initialIdentity[:]) || !bytes.Equal(checkpointDigest, wantDigest[:]) {
		return fmt.Errorf("%w: retry audit checkpoint identity or digest differs", ErrLocalNudgeAuthorityConflict)
	}
	var actualPreparations, actualReceipts uint64
	if err := a.db.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM retry_preparations),
		(SELECT COUNT(*) FROM retry_receipts)`).Scan(&actualPreparations, &actualReceipts); err != nil {
		return fmt.Errorf("reading retry authority evidence counts: %w", err)
	}
	if actualPreparations != preparationCount || actualReceipts != receiptCount {
		return fmt.Errorf("%w: retry evidence counts %d/%d differ from metadata %d/%d",
			ErrLocalNudgeAuthorityConflict, actualPreparations, actualReceipts, preparationCount, receiptCount)
	}
	return nil
}

type localAuthorityRetryTransitionRecord struct {
	intent                  CommandRetryTransitionIntent
	effectRevision          uint64
	effectSequenceHighWater uint64
}

type localAuthorityRetryOwner struct {
	intent  CommandRetryTransitionIntent
	writers uint64
}

// PrepareCommandRetryTransition persists one exact definite-non-entry retry
// before the command-store transition may commit. The prior claim receipt is
// validated but deliberately retained until finalization.
func (a *LocalNudgeAuthority) PrepareCommandRetryTransition(ctx context.Context, intent CommandRetryTransitionIntent) error {
	if err := validateCommandRetryTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if intent.Store != a.store {
		return fmt.Errorf("%w: retry preparation store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.retryOwnershipMu.Lock()
	defer a.retryOwnershipMu.Unlock()
	owner, owned := a.retryOwners[intent.CommandID]
	if owned && !sameCommandRetryTransitionIntent(owner.intent, intent) {
		return fmt.Errorf("%w: live writer owns a different retry intent", ErrLocalNudgeAuthorityConflict)
	}
	if owned && owner.writers == math.MaxUint64 {
		return fmt.Errorf("%w: retry writer ownership overflow", ErrLocalNudgeAuthorityConflict)
	}

	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("preparing local retry transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, intent.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != intent.Sequence || membership.partition != intent.Partition ||
		membership.terminalRevision != nil || membership.admissionRevision > intent.RepositoryBeforeRevision {
		return fmt.Errorf("%w: retry transition has no matching active admission", ErrLocalNudgeAuthorityConflict)
	}
	highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(ctx, tx)
	if err != nil {
		return err
	}
	if intent.RepositoryBeforeRevision < highestRevision || intent.RepositorySequenceHighWater < highestSequence {
		return fmt.Errorf("%w: retry repository state %d/%d is behind authority effect fence %d/%d",
			ErrLocalNudgeAuthorityConflict, intent.RepositoryBeforeRevision, intent.RepositorySequenceHighWater, highestRevision, highestSequence)
	}
	if terminal, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, intent.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry transition conflicts with terminal preparation at revision %d", ErrLocalNudgeAuthorityConflict, terminal.RepositoryRevision)
	}
	if _, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, intent.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry transition conflicts with unresolved claim preparation", ErrLocalNudgeAuthorityConflict)
	}
	claimReceipt, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if !found || !claimReceiptMatchesRetryIntent(claimReceipt, intent) {
		return fmt.Errorf("%w: retry transition lacks the exact active claim receipt", ErrLocalNudgeAuthorityConflict)
	}
	if receipt, found, err := localAuthorityRetryReceiptByAttempt(ctx, tx, a.store, intent.CommandID, intent.Claim.AttemptID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry transition for attempt %q is already finalized at revision %d", ErrLocalNudgeAuthorityConflict, intent.Claim.AttemptID, receipt.RepositoryRevision)
	}
	existing, found, err := localAuthorityRetryPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if found {
		if !sameCommandRetryTransitionIntent(existing, intent) {
			return fmt.Errorf("%w: competing retry transition for command %q", ErrLocalNudgeAuthorityConflict, intent.CommandID)
		}
		a.retainRetryWriterOwnership(intent, owner)
		return nil
	}
	if err := insertLocalAuthorityRetryPreparation(ctx, tx, intent); err != nil {
		return err
	}
	if err := advanceLocalAuthorityRetryTransitionGeneration(ctx, tx, 1, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("preparing local retry transition: commit: %w", err)
	}
	a.retainRetryWriterOwnership(intent, owner)
	return nil
}

func (a *LocalNudgeAuthority) retainRetryWriterOwnership(intent CommandRetryTransitionIntent, owner localAuthorityRetryOwner) {
	if a.retryOwners == nil {
		a.retryOwners = make(map[string]localAuthorityRetryOwner)
	}
	owner.intent = intent
	owner.writers++
	a.retryOwners[intent.CommandID] = owner
}

// ReleaseCommandRetryTransitionWriter releases one in-process store writer
// without changing durable preparation evidence.
func (a *LocalNudgeAuthority) ReleaseCommandRetryTransitionWriter(ctx context.Context, intent CommandRetryTransitionIntent) error {
	if err := validateCommandRetryTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if intent.Store != a.store {
		return fmt.Errorf("%w: retry writer store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.retryOwnershipMu.Lock()
	defer a.retryOwnershipMu.Unlock()
	owner, owned := a.retryOwners[intent.CommandID]
	if !owned {
		receipt, found, err := localAuthorityRetryReceiptByAttempt(ctx, a.db, a.store, intent.CommandID, intent.Claim.AttemptID)
		if err != nil {
			return err
		}
		if found && retryReceiptMatchesIntent(receipt, intent) {
			return nil
		}
		return fmt.Errorf("%w: retry writer ownership is missing", ErrLocalNudgeAuthorityConflict)
	}
	if owner.writers == 0 || !sameCommandRetryTransitionIntent(owner.intent, intent) {
		return fmt.Errorf("%w: retry writer ownership is missing or different", ErrLocalNudgeAuthorityConflict)
	}
	if owner.writers == 1 {
		delete(a.retryOwners, intent.CommandID)
		return nil
	}
	owner.writers--
	a.retryOwners[intent.CommandID] = owner
	return nil
}

// AbortCommandRetryTransition removes only the exact preparation after the
// caller proves that the command-store transaction rolled back.
func (a *LocalNudgeAuthority) AbortCommandRetryTransition(ctx context.Context, intent CommandRetryTransitionIntent) error {
	if err := validateCommandRetryTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if intent.Store != a.store {
		return fmt.Errorf("%w: retry abort store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.retryOwnershipMu.Lock()
	defer a.retryOwnershipMu.Unlock()
	owner, owned := a.retryOwners[intent.CommandID]
	if owned {
		if owner.writers == 0 || !sameCommandRetryTransitionIntent(owner.intent, intent) {
			return fmt.Errorf("%w: retry writer ownership differs from abort intent", ErrLocalNudgeAuthorityConflict)
		}
		if owner.writers > 1 {
			return nil
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aborting local retry transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existing, found, err := localAuthorityRetryPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if found {
		if !sameCommandRetryTransitionIntent(existing, intent) {
			return fmt.Errorf("%w: refusing to abort a different retry transition", ErrLocalNudgeAuthorityConflict)
		}
		deleted, err := tx.ExecContext(ctx, `DELETE FROM retry_preparations WHERE command_id = ? AND attempt_id = ?`, intent.CommandID, intent.Claim.AttemptID)
		if err != nil {
			return fmt.Errorf("aborting local retry transition: %w", err)
		}
		if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
			return fmt.Errorf("%w: retry preparation abort affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
		}
		if err := advanceLocalAuthorityRetryTransitionGeneration(ctx, tx, -1, 0); err != nil {
			return err
		}
	} else if receipt, found, err := localAuthorityRetryReceiptByAttempt(ctx, tx, a.store, intent.CommandID, intent.Claim.AttemptID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: finalized retry transition cannot be aborted at revision %d", ErrLocalNudgeAuthorityConflict, receipt.RepositoryRevision)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aborting local retry transition: commit: %w", err)
	}
	return nil
}

// FinalizeCommandRetryTransition atomically retains immutable definite-
// non-entry evidence and consumes the exact prior one-shot claim receipt.
func (a *LocalNudgeAuthority) FinalizeCommandRetryTransition(ctx context.Context, commit CommandRetryTransitionCommit) (CommandRetryReceiptDisposition, error) {
	if err := validateCommandRetryTransitionCommit(commit); err != nil {
		return "", err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if commit.Store != a.store {
		return "", fmt.Errorf("%w: retry commit store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.retryOwnershipMu.Lock()
	defer a.retryOwnershipMu.Unlock()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("finalizing local retry transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if existing, found, err := localAuthorityRetryReceiptByAttempt(ctx, tx, a.store, commit.CommandID, commit.AttemptID); err != nil {
		return "", err
	} else if found {
		if !retryReceiptMatchesCommit(existing, commit) {
			return "", fmt.Errorf("%w: finalized retry receipt differs", ErrLocalNudgeAuthorityConflict)
		}
		if preparation, prepared, err := localAuthorityRetryPreparationByCommand(ctx, tx, a.store, commit.CommandID); err != nil {
			return "", err
		} else if prepared && preparation.Claim.AttemptID == commit.AttemptID {
			return "", fmt.Errorf("%w: finalized retry retained its preparation", ErrLocalNudgeAuthorityConflict)
		}
		clearOwner := false
		if owner, owned := a.retryOwners[commit.CommandID]; owned && owner.intent.Claim.AttemptID == commit.AttemptID {
			if !retryIntentMatchesCommit(owner.intent, commit) {
				return "", fmt.Errorf("%w: finalized retry receipt has a different live writer", ErrLocalNudgeAuthorityConflict)
			}
			clearOwner = true
		}
		if err := advanceLocalAuthorityObservedRepositoryState(ctx, tx, commit.EffectSequenceHighWater, commit.EffectRepositoryRevision); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("finalizing existing local retry transition: commit: %w", err)
		}
		if clearOwner {
			delete(a.retryOwners, commit.CommandID)
		}
		return CommandRetryReceiptAlreadyFinalized, nil
	}
	intent, found, err := localAuthorityRetryPreparationByCommand(ctx, tx, a.store, commit.CommandID)
	if err != nil {
		return "", err
	}
	if !found || !retryIntentMatchesCommit(intent, commit) {
		return "", fmt.Errorf("%w: retry commit has no exact write-ahead preparation", ErrLocalNudgeAuthorityConflict)
	}
	if owner, owned := a.retryOwners[commit.CommandID]; owned && (owner.writers == 0 || !sameCommandRetryTransitionIntent(owner.intent, intent)) {
		return "", fmt.Errorf("%w: retry writer ownership differs from commit", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, commit.CommandID); err != nil {
		return "", err
	} else if found {
		return "", fmt.Errorf("%w: retry finalization conflicts with terminal preparation", ErrLocalNudgeAuthorityConflict)
	}
	claimReceipt, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, commit.CommandID)
	if err != nil {
		return "", err
	}
	if !found || !claimReceiptMatchesRetryIntent(claimReceipt, intent) {
		return "", fmt.Errorf("%w: retry finalization cannot consume the exact claim receipt", ErrLocalNudgeAuthorityConflict)
	}
	receipt, err := commandRetryTransitionReceiptFor(intent, commit)
	if err != nil {
		return "", err
	}
	if err := insertLocalAuthorityRetryReceipt(ctx, tx, intent, receipt); err != nil {
		return "", err
	}
	deletedRetry, err := tx.ExecContext(ctx, `DELETE FROM retry_preparations WHERE command_id = ? AND attempt_id = ?`, commit.CommandID, commit.AttemptID)
	if err != nil {
		return "", fmt.Errorf("finalizing local retry transition: consuming preparation: %w", err)
	}
	if affected, err := deletedRetry.RowsAffected(); err != nil || affected != 1 {
		return "", fmt.Errorf("%w: retry preparation consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	deletedClaim, err := tx.ExecContext(ctx, `DELETE FROM claim_receipts WHERE command_id = ? AND attempt_id = ?`, commit.CommandID, commit.AttemptID)
	if err != nil {
		return "", fmt.Errorf("finalizing local retry transition: consuming claim receipt: %w", err)
	}
	if affected, err := deletedClaim.RowsAffected(); err != nil || affected != 1 {
		return "", fmt.Errorf("%w: retry claim receipt consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(ctx, tx, 0, -1); err != nil {
		return "", err
	}
	if err := advanceLocalAuthorityRetryTransitionGeneration(ctx, tx, -1, 1); err != nil {
		return "", err
	}
	if err := advanceLocalAuthorityObservedRepositoryState(ctx, tx, commit.EffectSequenceHighWater, commit.EffectRepositoryRevision); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("finalizing local retry transition: commit: %w", err)
	}
	delete(a.retryOwners, commit.CommandID)
	return CommandRetryReceiptFinalized, nil
}

// VerifyCommandRetryClaim proves that the pending command is exactly the
// latest finalized definite-non-entry retry and that the next one-shot claim
// uses identifiers absent from retained retry history.
func (a *LocalNudgeAuthority) VerifyCommandRetryClaim(ctx context.Context, verification CommandRetryClaimVerification) error {
	if err := validateCommandRetryClaimVerification(verification); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if verification.Store != a.store {
		return fmt.Errorf("%w: retry claim store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("verifying local retry claim: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, verification.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != verification.Sequence || membership.partition != verification.Partition ||
		membership.terminalRevision != nil || membership.admissionRevision > verification.CommandRevision {
		return fmt.Errorf("%w: retry claim has no matching active admission", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityRetryPreparationByCommand(ctx, tx, a.store, verification.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry claim still has an unresolved retry preparation", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, verification.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry claim conflicts with terminal preparation", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, verification.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry claim conflicts with claim preparation", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, verification.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: retry claim retained an active prior claim receipt", ErrLocalNudgeAuthorityConflict)
	}
	latest, found, err := localAuthorityLatestRetryReceipt(ctx, tx, a.store, verification.CommandID)
	if err != nil {
		return err
	}
	if !found || !retryReceiptMatchesClaimVerification(latest, verification) {
		return fmt.Errorf("%w: pending retry differs from its latest immutable receipt", ErrLocalNudgeAuthorityConflict)
	}
	var reused int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM retry_receipts
		WHERE command_id = ? AND (claim_id = ? OR attempt_id = ?)`,
		verification.CommandID, verification.NextClaimID, verification.NextAttemptID).Scan(&reused); err != nil {
		return fmt.Errorf("verifying local retry claim identifier freshness: %w", err)
	}
	if reused != 0 {
		return fmt.Errorf("%w: retry claim or attempt identifier was already used", ErrLocalNudgeAuthorityConflict)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("verifying local retry claim: commit: %w", err)
	}
	return nil
}

func (a *LocalNudgeAuthority) verifyLocalAuthorityPendingRetry(
	ctx context.Context,
	queryer localAuthorityQueryer,
	state CommandRepositoryState,
	command Command,
	digest [sha256.Size]byte,
	partition TrustedCityPartition,
) error {
	if command.State != CommandStatePending || command.Claim != nil || command.Terminal != nil ||
		command.Store != state.Store || command.Order.Sequence == 0 || command.Order.Sequence > state.SequenceHighWater ||
		command.Order.Revision == 0 || command.Order.Revision > state.Revision || !partition.valid() {
		return fmt.Errorf("%w: invalid pending retry audit input", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityRetryPreparationByCommand(ctx, queryer, a.store, command.ID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: pending retry retained an unresolved retry preparation", ErrLocalNudgeAuthorityConflict)
	}
	latest, found, err := localAuthorityLatestRetryReceipt(ctx, queryer, a.store, command.ID)
	if err != nil {
		return err
	}
	if command.Retry == nil {
		if found {
			return fmt.Errorf("%w: initial pending command retained retry history", ErrLocalNudgeAuthorityConflict)
		}
		return nil
	}
	if !found || !retryReceiptMatchesPendingCommand(latest, state, command, digest, partition) {
		return fmt.Errorf("%w: pending retry differs from its latest immutable receipt", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func insertLocalAuthorityRetryPreparation(ctx context.Context, tx *sql.Tx, intent CommandRetryTransitionIntent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO retry_preparations (
		command_id, attempt_id, sequence, partition_id, repository_before_revision, retry_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		retry_attempt_count, retry_last_attempt_at, retry_next_eligible_at, retry_error_class, retry_error_detail,
		observed_at, provider_stage, completion
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.CommandID, intent.Claim.AttemptID, encodeLocalAuthorityUint64(intent.Sequence), intent.Partition.identity[:],
		encodeLocalAuthorityUint64(intent.RepositoryBeforeRevision), encodeLocalAuthorityUint64(intent.RepositoryRevision),
		encodeLocalAuthorityUint64(intent.RepositorySequenceHighWater), intent.BeforeCommandDigest[:], intent.AfterCommandDigest[:],
		intent.Claim.ID, intent.Claim.OwnerID, intent.Claim.OperationID, intent.Claim.BoundLaunchIdentity,
		intent.Claim.AuthorizationDecisionID, intent.Claim.AuthorizationPolicyVersion,
		intent.Claim.ClaimedAt.Format(time.RFC3339Nano), intent.Claim.LeaseUntil.Format(time.RFC3339Nano),
		int64(intent.Retry.AttemptCount), intent.Retry.LastAttemptAt.Format(time.RFC3339Nano),
		intent.Retry.NextEligibleAt.Format(time.RFC3339Nano), string(intent.Retry.ErrorClass), intent.Retry.ErrorDetail,
		intent.ObservedAt.Format(time.RFC3339Nano), string(intent.ProviderStage), string(intent.Completion))
	if err != nil {
		return fmt.Errorf("%w: inserting retry preparation: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	return nil
}

func insertLocalAuthorityRetryReceipt(ctx context.Context, tx *sql.Tx, intent CommandRetryTransitionIntent, receipt CommandRetryTransitionReceipt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO retry_receipts (
		command_id, attempt_id, sequence, partition_id, repository_before_revision, retry_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		retry_attempt_count, retry_last_attempt_at, retry_next_eligible_at, retry_error_class, retry_error_detail,
		observed_at, provider_stage, completion, effect_revision, effect_sequence_high_water
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.CommandID, intent.Claim.AttemptID, encodeLocalAuthorityUint64(intent.Sequence), intent.Partition.identity[:],
		encodeLocalAuthorityUint64(intent.RepositoryBeforeRevision), encodeLocalAuthorityUint64(intent.RepositoryRevision),
		encodeLocalAuthorityUint64(intent.RepositorySequenceHighWater), intent.BeforeCommandDigest[:], intent.AfterCommandDigest[:],
		intent.Claim.ID, intent.Claim.OwnerID, intent.Claim.OperationID, intent.Claim.BoundLaunchIdentity,
		intent.Claim.AuthorizationDecisionID, intent.Claim.AuthorizationPolicyVersion,
		intent.Claim.ClaimedAt.Format(time.RFC3339Nano), intent.Claim.LeaseUntil.Format(time.RFC3339Nano),
		int64(intent.Retry.AttemptCount), intent.Retry.LastAttemptAt.Format(time.RFC3339Nano),
		intent.Retry.NextEligibleAt.Format(time.RFC3339Nano), string(intent.Retry.ErrorClass), intent.Retry.ErrorDetail,
		intent.ObservedAt.Format(time.RFC3339Nano), string(intent.ProviderStage), string(intent.Completion),
		encodeLocalAuthorityUint64(receipt.EffectRepositoryRevision), encodeLocalAuthorityUint64(receipt.EffectSequenceHighWater))
	if err != nil {
		return fmt.Errorf("%w: inserting retry receipt: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	return nil
}

func localAuthorityRetryPreparationByCommand(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (CommandRetryTransitionIntent, bool, error) {
	record, found, err := scanLocalAuthorityRetryTransition(ctx, queryer, queryer.QueryRowContext(ctx, `SELECT
		attempt_id, sequence, partition_id, repository_before_revision, retry_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		retry_attempt_count, retry_last_attempt_at, retry_next_eligible_at, retry_error_class, retry_error_detail,
		observed_at, provider_stage, completion
		FROM retry_preparations WHERE command_id = ?`, commandID), store, commandID, false)
	if err != nil || !found {
		return CommandRetryTransitionIntent{}, found, err
	}
	if counterpart, err := localAuthorityRetryTransitionRowExists(ctx, queryer, "retry_receipts", commandID, record.intent.Claim.AttemptID); err != nil {
		return CommandRetryTransitionIntent{}, false, err
	} else if counterpart {
		return CommandRetryTransitionIntent{}, false, fmt.Errorf("%w: retry attempt %q has both preparation and receipt", ErrLocalNudgeAuthorityConflict, record.intent.Claim.AttemptID)
	}
	return record.intent, true, nil
}

func localAuthorityRetryReceiptByAttempt(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID, attemptID string) (CommandRetryTransitionReceipt, bool, error) {
	record, found, err := scanLocalAuthorityRetryTransition(ctx, queryer, queryer.QueryRowContext(ctx, `SELECT
		attempt_id, sequence, partition_id, repository_before_revision, retry_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		retry_attempt_count, retry_last_attempt_at, retry_next_eligible_at, retry_error_class, retry_error_detail,
		observed_at, provider_stage, completion, effect_revision, effect_sequence_high_water
		FROM retry_receipts WHERE command_id = ? AND attempt_id = ?`, commandID, attemptID), store, commandID, true)
	if err != nil || !found {
		return CommandRetryTransitionReceipt{}, found, err
	}
	if counterpart, err := localAuthorityRetryTransitionRowExists(ctx, queryer, "retry_preparations", commandID, attemptID); err != nil {
		return CommandRetryTransitionReceipt{}, false, err
	} else if counterpart {
		return CommandRetryTransitionReceipt{}, false, fmt.Errorf("%w: retry attempt %q has both preparation and receipt", ErrLocalNudgeAuthorityConflict, attemptID)
	}
	return localAuthorityRetryReceipt(record), true, nil
}

func localAuthorityLatestRetryReceipt(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (CommandRetryTransitionReceipt, bool, error) {
	record, found, err := scanLocalAuthorityRetryTransition(ctx, queryer, queryer.QueryRowContext(ctx, `SELECT
		attempt_id, sequence, partition_id, repository_before_revision, retry_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		retry_attempt_count, retry_last_attempt_at, retry_next_eligible_at, retry_error_class, retry_error_detail,
		observed_at, provider_stage, completion, effect_revision, effect_sequence_high_water
		FROM retry_receipts WHERE command_id = ? ORDER BY retry_revision DESC, attempt_id DESC LIMIT 1`, commandID), store, commandID, true)
	if err != nil || !found {
		return CommandRetryTransitionReceipt{}, found, err
	}
	if counterpart, err := localAuthorityRetryTransitionRowExists(ctx, queryer, "retry_preparations", commandID, record.intent.Claim.AttemptID); err != nil {
		return CommandRetryTransitionReceipt{}, false, err
	} else if counterpart {
		return CommandRetryTransitionReceipt{}, false, fmt.Errorf("%w: retry attempt %q has both preparation and receipt", ErrLocalNudgeAuthorityConflict, record.intent.Claim.AttemptID)
	}
	return localAuthorityRetryReceipt(record), true, nil
}

func localAuthorityRetryTransitionRowExists(ctx context.Context, queryer localAuthorityQueryer, table, commandID, attemptID string) (bool, error) {
	var query string
	switch table {
	case "retry_preparations":
		query = `SELECT 1 FROM retry_preparations WHERE command_id = ? AND attempt_id = ?`
	case "retry_receipts":
		query = `SELECT 1 FROM retry_receipts WHERE command_id = ? AND attempt_id = ?`
	default:
		return false, fmt.Errorf("%w: unknown retry transition table %q", ErrLocalNudgeAuthorityConflict, table)
	}
	var found int
	if err := queryer.QueryRowContext(ctx, query, commandID, attemptID).Scan(&found); errors.Is(err, sql.ErrNoRows) {
		return false, nil
	} else if err != nil {
		return false, fmt.Errorf("reading local retry transition counterpart: %w", err)
	}
	return found == 1, nil
}

func scanLocalAuthorityRetryTransition(ctx context.Context, queryer localAuthorityQueryer, row *sql.Row, store CommandStoreBinding, commandID string, receipt bool) (localAuthorityRetryTransitionRecord, bool, error) {
	var attemptID, claimID, ownerID, operationID, launchIdentity, decisionID, policyVersion string
	var claimedAtWire, leaseUntilWire, lastAttemptWire, nextEligibleWire, errorClass, errorDetail, observedAtWire string
	var providerStage, completion string
	var sequenceWire, partitionWire, beforeRevisionWire, retryRevisionWire, sequenceHighWaterWire []byte
	var beforeDigestWire, afterDigestWire, effectRevisionWire, effectSequenceWire []byte
	var attemptCount int64
	destinations := []any{
		&attemptID, &sequenceWire, &partitionWire, &beforeRevisionWire, &retryRevisionWire, &sequenceHighWaterWire,
		&beforeDigestWire, &afterDigestWire, &claimID, &ownerID, &operationID, &launchIdentity,
		&decisionID, &policyVersion, &claimedAtWire, &leaseUntilWire,
		&attemptCount, &lastAttemptWire, &nextEligibleWire, &errorClass, &errorDetail,
		&observedAtWire, &providerStage, &completion,
	}
	if receipt {
		destinations = append(destinations, &effectRevisionWire, &effectSequenceWire)
	}
	if err := row.Scan(destinations...); errors.Is(err, sql.ErrNoRows) {
		return localAuthorityRetryTransitionRecord{}, false, nil
	} else if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("reading local retry transition: %w", err)
	}
	sequence, err := decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, err
	}
	beforeRevision, err := decodeLocalAuthorityUint64(beforeRevisionWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, err
	}
	retryRevision, err := decodeLocalAuthorityUint64(retryRevisionWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, err
	}
	sequenceHighWater, err := decodeLocalAuthorityUint64(sequenceHighWaterWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, err
	}
	if len(partitionWire) != sha256.Size || len(beforeDigestWire) != sha256.Size || len(afterDigestWire) != sha256.Size || attemptCount < 1 || attemptCount > math.MaxUint32 {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry transition identity or attempt count", ErrLocalNudgeAuthorityConflict)
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, claimedAtWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry claimed_at", ErrLocalNudgeAuthorityConflict)
	}
	leaseUntil, err := time.Parse(time.RFC3339Nano, leaseUntilWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry lease_until", ErrLocalNudgeAuthorityConflict)
	}
	lastAttempt, err := time.Parse(time.RFC3339Nano, lastAttemptWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry last_attempt_at", ErrLocalNudgeAuthorityConflict)
	}
	nextEligible, err := time.Parse(time.RFC3339Nano, nextEligibleWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry next_eligible_at", ErrLocalNudgeAuthorityConflict)
	}
	observedAt, err := time.Parse(time.RFC3339Nano, observedAtWire)
	if err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed retry observed_at", ErrLocalNudgeAuthorityConflict)
	}
	claim := CommandClaim{
		ID: claimID, OwnerID: ownerID, OperationID: operationID, AttemptID: attemptID,
		BoundLaunchIdentity: launchIdentity, AuthorizationDecisionID: decisionID,
		AuthorizationPolicyVersion: policyVersion, ClaimedAt: claimedAt, LeaseUntil: leaseUntil,
	}
	retry := CommandRetry{
		AttemptCount: uint32(attemptCount), LastAttemptAt: lastAttempt,
		ClaimID: claimID, OperationID: operationID, AttemptID: attemptID,
		BoundLaunchIdentity: launchIdentity, AuthorizationDecisionID: decisionID,
		AuthorizationPolicyVersion: policyVersion, NextEligibleAt: &nextEligible,
		ErrorClass: CommandErrorClass(errorClass), ErrorDetail: errorDetail,
	}
	record := localAuthorityRetryTransitionRecord{intent: CommandRetryTransitionIntent{
		Store: store, RepositoryBeforeRevision: beforeRevision, RepositoryRevision: retryRevision,
		RepositorySequenceHighWater: sequenceHighWater, CommandID: commandID, Sequence: sequence,
		Claim: claim, Retry: retry, ObservedAt: observedAt,
		ProviderStage: ProviderStage(providerStage), Completion: CompletionState(completion),
	}}
	copy(record.intent.Partition.identity[:], partitionWire)
	copy(record.intent.BeforeCommandDigest[:], beforeDigestWire)
	copy(record.intent.AfterCommandDigest[:], afterDigestWire)
	if err := validateCommandRetryTransitionIntent(record.intent); err != nil {
		return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed persisted retry transition: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if receipt {
		record.effectRevision, err = decodeLocalAuthorityUint64(effectRevisionWire)
		if err != nil {
			return localAuthorityRetryTransitionRecord{}, false, err
		}
		record.effectSequenceHighWater, err = decodeLocalAuthorityUint64(effectSequenceWire)
		if err != nil {
			return localAuthorityRetryTransitionRecord{}, false, err
		}
		if err := validateCommandRetryTransitionReceipt(localAuthorityRetryReceipt(record)); err != nil {
			return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: malformed persisted retry receipt: %w", ErrLocalNudgeAuthorityConflict, err)
		}
		highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(ctx, queryer)
		if err != nil {
			return localAuthorityRetryTransitionRecord{}, false, err
		}
		if highestSequence < record.effectSequenceHighWater || highestRevision < record.effectRevision {
			return localAuthorityRetryTransitionRecord{}, false, fmt.Errorf("%w: authority high-water %d/%d does not dominate retry receipt effect %d/%d",
				ErrLocalNudgeAuthorityConflict, highestRevision, highestSequence, record.effectRevision, record.effectSequenceHighWater)
		}
	}
	return record, true, nil
}

func localAuthorityRetryReceipt(record localAuthorityRetryTransitionRecord) CommandRetryTransitionReceipt {
	return CommandRetryTransitionReceipt{
		Store: record.intent.Store, RepositoryRevision: record.intent.RepositoryRevision,
		CommandID: record.intent.CommandID, Sequence: record.intent.Sequence, Partition: record.intent.Partition,
		AfterCommandDigest: record.intent.AfterCommandDigest, Claim: record.intent.Claim, Retry: cloneCommandRetry(record.intent.Retry),
		ObservedAt: record.intent.ObservedAt, ProviderStage: record.intent.ProviderStage, Completion: record.intent.Completion,
		EffectRepositoryRevision: record.effectRevision, EffectSequenceHighWater: record.effectSequenceHighWater,
	}
}

func retryReceiptMatchesIntent(receipt CommandRetryTransitionReceipt, intent CommandRetryTransitionIntent) bool {
	return receipt.Store == intent.Store && receipt.RepositoryRevision == intent.RepositoryRevision &&
		receipt.CommandID == intent.CommandID && receipt.Sequence == intent.Sequence && receipt.Partition == intent.Partition &&
		receipt.AfterCommandDigest == intent.AfterCommandDigest && commandClaimsEqual(receipt.Claim, intent.Claim) &&
		sameCommandRetry(receipt.Retry, intent.Retry) && receipt.ObservedAt.Equal(intent.ObservedAt) &&
		receipt.ProviderStage == intent.ProviderStage && receipt.Completion == intent.Completion
}

func claimReceiptMatchesRetryIntent(receipt localAuthorityClaimTransitionRecord, intent CommandRetryTransitionIntent) bool {
	return receipt.intent.Store == intent.Store && receipt.intent.CommandID == intent.CommandID &&
		receipt.intent.Sequence == intent.Sequence && receipt.intent.Partition == intent.Partition &&
		receipt.intent.AfterCommandDigest == intent.BeforeCommandDigest &&
		commandClaimsEqual(receipt.intent.Claim, intent.Claim) &&
		receipt.effectRevision <= intent.RepositoryBeforeRevision &&
		receipt.effectSequenceHighWater <= intent.RepositorySequenceHighWater
}

func advanceLocalAuthorityRetryTransitionGeneration(ctx context.Context, tx *sql.Tx, preparationDelta, receiptDelta int) error {
	var generationWire, preparationsWire, receiptsWire []byte
	if err := tx.QueryRowContext(ctx, `SELECT transition_generation, preparation_count, receipt_count FROM retry_meta WHERE singleton = 1`).Scan(
		&generationWire, &preparationsWire, &receiptsWire,
	); err != nil {
		return fmt.Errorf("reading local retry mutation state: %w", err)
	}
	generation, err := decodeLocalAuthorityUint64(generationWire)
	if err != nil {
		return err
	}
	preparations, err := decodeLocalAuthorityUint64(preparationsWire)
	if err != nil {
		return err
	}
	receipts, err := decodeLocalAuthorityUint64(receiptsWire)
	if err != nil {
		return err
	}
	if generation == math.MaxUint64 {
		return fmt.Errorf("%w: retry transition generation is exhausted", ErrLocalNudgeAuthorityConflict)
	}
	preparations, err = applyLocalAuthorityRetryCountDelta(preparations, preparationDelta)
	if err != nil {
		return err
	}
	receipts, err = applyLocalAuthorityRetryCountDelta(receipts, receiptDelta)
	if err != nil {
		return err
	}
	result, err := tx.ExecContext(ctx, `UPDATE retry_meta SET transition_generation = ?, preparation_count = ?, receipt_count = ? WHERE singleton = 1`,
		encodeLocalAuthorityUint64(generation+1), encodeLocalAuthorityUint64(preparations), encodeLocalAuthorityUint64(receipts))
	if err != nil {
		return fmt.Errorf("advancing local retry transition generation: %w", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("%w: retry metadata update affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	return nil
}

func applyLocalAuthorityRetryCountDelta(value uint64, delta int) (uint64, error) {
	switch {
	case delta > 0:
		increment := uint64(delta)
		if value > math.MaxUint64-increment {
			return 0, fmt.Errorf("%w: retry evidence count overflow", ErrLocalNudgeAuthorityConflict)
		}
		return value + increment, nil
	case delta < 0:
		decrement := uint64(-delta)
		if value < decrement {
			return 0, fmt.Errorf("%w: retry evidence count underflow", ErrLocalNudgeAuthorityConflict)
		}
		return value - decrement, nil
	default:
		return value, nil
	}
}

var (
	_ TrustedCommandRetryTransitionAuthority = (*LocalNudgeAuthority)(nil)
	_ TrustedCommandRetryClaimVerifier       = (*LocalNudgeAuthority)(nil)
)
