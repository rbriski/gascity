package nudgequeue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"time"
)

type localAuthorityClaimTransitionRecord struct {
	intent                  CommandClaimTransitionIntent
	effectRevision          uint64
	effectSequenceHighWater uint64
}

type localAuthorityClaimOwner struct {
	intent  CommandClaimTransitionIntent
	writers uint64
}

type localAuthorityClaimRecoveryToken struct {
	generation   uint64
	identity     [sha256.Size]byte
	preparations uint64
	receipts     uint64
}

// PrepareCommandClaimTransition persists one exact pending-to-in-flight
// transition before its serialized command-store transaction may commit. A
// different preparation can replace an abandoned one only when both name the
// exact same durable pending before-state.
func (a *LocalNudgeAuthority) PrepareCommandClaimTransition(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if err := validateCommandClaimTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if intent.Store != a.store {
		return fmt.Errorf("%w: claim preparation store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	owner, owned := a.claimOwners[intent.CommandID]
	if owned && owner.intent != intent {
		return fmt.Errorf("%w: live writer owns a different claim transition", ErrLocalNudgeAuthorityConflict)
	}
	if owned && owner.writers == ^uint64(0) {
		return fmt.Errorf("%w: claim transition writer ownership overflow", ErrLocalNudgeAuthorityConflict)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("preparing local claim transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, intent.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != intent.Sequence || membership.partition != intent.Partition ||
		membership.terminalRevision != nil || membership.admissionRevision > intent.RepositoryBeforeRevision {
		return fmt.Errorf("%w: claim preparation has no matching active admission", ErrLocalNudgeAuthorityConflict)
	}
	if _, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, intent.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: claim transition for %q is already finalized", ErrLocalNudgeAuthorityConflict, intent.CommandID)
	}
	existing, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	replacing := found
	if found {
		if existing.intent == intent {
			a.retainClaimTransitionWriter(intent, owner)
			return nil
		}
		if existing.intent.CommandID != intent.CommandID || existing.intent.Sequence != intent.Sequence ||
			existing.intent.Partition != intent.Partition || existing.intent.BeforeCommandDigest != intent.BeforeCommandDigest {
			return fmt.Errorf("%w: competing claim preparation for command %q", ErrLocalNudgeAuthorityConflict, intent.CommandID)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM claim_preparations WHERE command_id = ?`, intent.CommandID); err != nil {
			return fmt.Errorf("replacing local claim preparation: %w", err)
		}
	}
	if err := insertLocalAuthorityClaimPreparation(ctx, tx, intent); err != nil {
		return err
	}
	preparationDelta := 1
	if replacing {
		preparationDelta = 0
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(ctx, tx, preparationDelta, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("preparing local claim transition: commit: %w", err)
	}
	a.retainClaimTransitionWriter(intent, owner)
	return nil
}

func (a *LocalNudgeAuthority) retainClaimTransitionWriter(intent CommandClaimTransitionIntent, owner localAuthorityClaimOwner) {
	if a.claimOwners == nil {
		a.claimOwners = make(map[string]localAuthorityClaimOwner)
	}
	owner.intent = intent
	owner.writers++
	a.claimOwners[intent.CommandID] = owner
}

// ReleaseCommandClaimTransitionWriter releases one in-process writer without
// changing durable preparation evidence. Exact finalized receipts and exact
// already-aborted preparations make a repeated release harmless.
func (a *LocalNudgeAuthority) ReleaseCommandClaimTransitionWriter(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if err := validateCommandClaimTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	owner, owned := a.claimOwners[intent.CommandID]
	if !owned {
		receipt, finalized, err := localAuthorityClaimReceiptByCommand(ctx, a.db, a.store, intent.CommandID)
		if err != nil {
			return err
		}
		if finalized {
			if !claimIntentMatchesReceipt(intent, localClaimTransitionReceipt(receipt)) {
				return fmt.Errorf("%w: finalized claim receipt differs from released writer", ErrLocalNudgeAuthorityConflict)
			}
			return nil
		}
		if preparation, found, err := localAuthorityClaimPreparationByCommand(ctx, a.db, a.store, intent.CommandID); err != nil {
			return err
		} else if !found {
			return nil
		} else if preparation.intent != intent {
			return fmt.Errorf("%w: durable claim preparation differs from released writer", ErrLocalNudgeAuthorityConflict)
		}
		return nil
	}
	if owner.intent != intent || owner.writers == 0 {
		return fmt.Errorf("%w: claim transition writer ownership is missing or different", ErrLocalNudgeAuthorityConflict)
	}
	if owner.writers == 1 {
		delete(a.claimOwners, intent.CommandID)
		return nil
	}
	owner.writers--
	a.claimOwners[intent.CommandID] = owner
	return nil
}

func insertLocalAuthorityClaimPreparation(ctx context.Context, tx *sql.Tx, intent CommandClaimTransitionIntent) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_preparations (
		command_id, sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.CommandID, encodeLocalAuthorityUint64(intent.Sequence), intent.Partition.identity[:],
		encodeLocalAuthorityUint64(intent.RepositoryBeforeRevision), encodeLocalAuthorityUint64(intent.RepositoryRevision),
		encodeLocalAuthorityUint64(intent.RepositorySequenceHighWater), intent.BeforeCommandDigest[:], intent.AfterCommandDigest[:],
		intent.Claim.ID, intent.Claim.OwnerID, intent.Claim.OperationID, intent.Claim.AttemptID,
		intent.Claim.BoundLaunchIdentity, intent.Claim.AuthorizationDecisionID, intent.Claim.AuthorizationPolicyVersion,
		intent.Claim.ClaimedAt.Format(time.RFC3339Nano), intent.Claim.LeaseUntil.Format(time.RFC3339Nano))
	if err != nil {
		return fmt.Errorf("%w: inserting claim preparation: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	return nil
}

// AbortCommandClaimTransition removes only an exact unresolved preparation.
// An absent preparation is idempotent; a finalized or substituted transition
// fails closed.
func (a *LocalNudgeAuthority) AbortCommandClaimTransition(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if err := validateCommandClaimTransitionIntent(intent); err != nil {
		return err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if intent.Store != a.store {
		return fmt.Errorf("%w: claim abort store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	if owner, owned := a.claimOwners[intent.CommandID]; owned {
		if owner.intent != intent || owner.writers == 0 {
			return fmt.Errorf("%w: claim writer ownership differs from abort intent", ErrLocalNudgeAuthorityConflict)
		}
		if owner.writers > 1 {
			return nil
		}
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aborting local claim transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, intent.CommandID); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: claim transition for %q is already finalized", ErrLocalNudgeAuthorityConflict, intent.CommandID)
	}
	existing, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if !found {
		return nil
	}
	if existing.intent != intent {
		return fmt.Errorf("%w: claim abort differs from prepared transition", ErrLocalNudgeAuthorityConflict)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM claim_preparations WHERE command_id = ?`, intent.CommandID)
	if err != nil {
		return fmt.Errorf("aborting local claim transition: %w", err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("%w: claim preparation abort affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(ctx, tx, -1, 0); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aborting local claim transition: commit: %w", err)
	}
	return nil
}

// claimWriterOwnsIntent snapshots whether recovery overlapped a live writer
// before crossing into the command store. A true snapshot remains a barrier
// for that store read even if the writer releases or finalizes meanwhile.
func (a *LocalNudgeAuthority) claimWriterOwnsIntent(ctx context.Context, intent CommandClaimTransitionIntent) (bool, error) {
	if err := validateCommandClaimTransitionIntent(intent); err != nil {
		return false, err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if intent.Store != a.store {
		return false, fmt.Errorf("%w: claim recovery store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	owner, owned := a.claimOwners[intent.CommandID]
	if !owned {
		return false, nil
	}
	if owner.intent != intent || owner.writers == 0 {
		return false, fmt.Errorf("%w: claim writer ownership differs from recovery snapshot", ErrLocalNudgeAuthorityConflict)
	}
	return true, nil
}

// abortRecoveredCommandClaimTransition removes an exact crash residue only
// when no in-process writer can still commit it. Finalization that wins after
// the repository before-state read is recognized as movement, not corruption.
func (a *LocalNudgeAuthority) abortRecoveredCommandClaimTransition(ctx context.Context, intent CommandClaimTransitionIntent) (bool, error) {
	if err := validateCommandClaimTransitionIntent(intent); err != nil {
		return false, err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	if intent.Store != a.store {
		return false, fmt.Errorf("%w: claim recovery abort store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	if owner, owned := a.claimOwners[intent.CommandID]; owned {
		if owner.intent != intent || owner.writers == 0 {
			return false, fmt.Errorf("%w: claim writer ownership differs from recovery abort", ErrLocalNudgeAuthorityConflict)
		}
		return false, nil
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("aborting recovered local claim transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if receipt, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, intent.CommandID); err != nil {
		return false, err
	} else if found {
		if !claimIntentMatchesReceipt(intent, localClaimTransitionReceipt(receipt)) {
			return false, fmt.Errorf("%w: finalized claim receipt differs from recovery intent", ErrLocalNudgeAuthorityConflict)
		}
		return false, nil
	}
	prepared, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return false, err
	}
	if !found {
		return false, nil
	}
	if prepared.intent != intent {
		return false, fmt.Errorf("%w: recovered claim preparation was substituted", ErrLocalNudgeAuthorityConflict)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM claim_preparations WHERE command_id = ?`, intent.CommandID)
	if err != nil {
		return false, fmt.Errorf("aborting recovered local claim transition: %w", err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return false, fmt.Errorf("%w: recovered claim abort affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(ctx, tx, -1, 0); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("aborting recovered local claim transition: commit: %w", err)
	}
	return true, nil
}

// FinalizeCommandClaimTransition atomically consumes the exact write-ahead
// preparation, retains its receipt, and advances the independent repository
// effect high-water. An exact retained receipt reports AlreadyFinalized so a
// later caller cannot re-enter the provider.
func (a *LocalNudgeAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	if err := validateCommandClaimTransitionReceipt(receipt); err != nil {
		return "", err
	}
	release, err := a.begin(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	if receipt.Store != a.store {
		return "", fmt.Errorf("%w: claim receipt store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	a.claimOwnershipMu.Lock()
	defer a.claimOwnershipMu.Unlock()
	if owner, owned := a.claimOwners[receipt.CommandID]; owned &&
		(owner.writers == 0 || owner.intent.Store != receipt.Store || owner.intent.RepositoryRevision != receipt.RepositoryRevision ||
			owner.intent.CommandID != receipt.CommandID || owner.intent.Sequence != receipt.Sequence || owner.intent.Partition != receipt.Partition ||
			owner.intent.AfterCommandDigest != receipt.AfterCommandDigest || !commandClaimsEqual(owner.intent.Claim, receipt.Claim)) {
		return "", fmt.Errorf("%w: claim writer ownership differs from receipt", ErrLocalNudgeAuthorityConflict)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("finalizing local claim transition: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	existingReceipt, found, err := localAuthorityClaimReceiptByCommand(ctx, tx, a.store, receipt.CommandID)
	if err != nil {
		return "", err
	}
	if found {
		if !sameCommandClaimTransitionReceipt(localClaimTransitionReceipt(existingReceipt), receipt) ||
			receipt.EffectRepositoryRevision < existingReceipt.effectRevision ||
			receipt.EffectSequenceHighWater < existingReceipt.effectSequenceHighWater {
			return "", fmt.Errorf("%w: finalized claim receipt differs", ErrLocalNudgeAuthorityConflict)
		}
		if err := advanceLocalAuthorityClaimEffectFence(ctx, tx, receipt); err != nil {
			return "", err
		}
		if err := tx.Commit(); err != nil {
			return "", fmt.Errorf("finalizing existing local claim transition: commit: %w", err)
		}
		delete(a.claimOwners, receipt.CommandID)
		return CommandClaimReceiptAlreadyFinalized, nil
	}
	preparation, found, err := localAuthorityClaimPreparationByCommand(ctx, tx, a.store, receipt.CommandID)
	if err != nil {
		return "", err
	}
	if !found || !claimIntentMatchesReceipt(preparation.intent, receipt) {
		return "", fmt.Errorf("%w: claim receipt has no exact write-ahead preparation", ErrLocalNudgeAuthorityConflict)
	}
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, receipt.CommandID)
	if err != nil {
		return "", err
	}
	if !found || membership.sequence != receipt.Sequence || membership.partition != receipt.Partition || membership.terminalRevision != nil {
		return "", fmt.Errorf("%w: claim receipt has no matching active admission", ErrLocalNudgeAuthorityConflict)
	}
	if err := insertLocalAuthorityClaimReceipt(ctx, tx, preparation.intent, receipt); err != nil {
		return "", err
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM claim_preparations WHERE command_id = ?`, receipt.CommandID)
	if err != nil {
		return "", fmt.Errorf("finalizing local claim transition: consuming preparation: %w", err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return "", fmt.Errorf("%w: claim preparation consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityClaimEffectFence(ctx, tx, receipt); err != nil {
		return "", err
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(ctx, tx, -1, 1); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("finalizing local claim transition: commit: %w", err)
	}
	delete(a.claimOwners, receipt.CommandID)
	return CommandClaimReceiptFinalized, nil
}

func advanceLocalAuthorityClaimEffectFence(ctx context.Context, tx *sql.Tx, receipt CommandClaimTransitionReceipt) error {
	highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(ctx, tx)
	if err != nil {
		return err
	}
	currentDominatesReceipt := highestSequence >= receipt.EffectSequenceHighWater && highestRevision >= receipt.EffectRepositoryRevision
	receiptDominatesCurrent := receipt.EffectSequenceHighWater >= highestSequence && receipt.EffectRepositoryRevision >= highestRevision
	if !currentDominatesReceipt && !receiptDominatesCurrent {
		return fmt.Errorf("%w: claim receipt effect state %d/%d is incomparable with authority high-water %d/%d",
			ErrLocalNudgeAuthorityConflict, receipt.EffectRepositoryRevision, receipt.EffectSequenceHighWater, highestRevision, highestSequence)
	}
	return advanceLocalAuthorityObservedRepositoryState(ctx, tx, receipt.EffectSequenceHighWater, receipt.EffectRepositoryRevision)
}

func insertLocalAuthorityClaimReceipt(ctx context.Context, tx *sql.Tx, intent CommandClaimTransitionIntent, receipt CommandClaimTransitionReceipt) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO claim_receipts (
		command_id, sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		effect_revision, effect_sequence_high_water
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		intent.CommandID, encodeLocalAuthorityUint64(intent.Sequence), intent.Partition.identity[:],
		encodeLocalAuthorityUint64(intent.RepositoryBeforeRevision), encodeLocalAuthorityUint64(intent.RepositoryRevision),
		encodeLocalAuthorityUint64(intent.RepositorySequenceHighWater), intent.BeforeCommandDigest[:], intent.AfterCommandDigest[:],
		intent.Claim.ID, intent.Claim.OwnerID, intent.Claim.OperationID, intent.Claim.AttemptID,
		intent.Claim.BoundLaunchIdentity, intent.Claim.AuthorizationDecisionID, intent.Claim.AuthorizationPolicyVersion,
		intent.Claim.ClaimedAt.Format(time.RFC3339Nano), intent.Claim.LeaseUntil.Format(time.RFC3339Nano),
		encodeLocalAuthorityUint64(receipt.EffectRepositoryRevision), encodeLocalAuthorityUint64(receipt.EffectSequenceHighWater))
	if err != nil {
		return fmt.Errorf("%w: inserting claim receipt: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	return nil
}

func localAuthorityClaimPreparationByCommand(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (localAuthorityClaimTransitionRecord, bool, error) {
	record, found, err := scanLocalAuthorityClaimTransition(queryer.QueryRowContext(ctx, `SELECT
		sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until
		FROM claim_preparations WHERE command_id = ?`, commandID), store, commandID, false)
	if err != nil || !found {
		return record, found, err
	}
	if counterpart, err := localAuthorityClaimTransitionRowExists(ctx, queryer, "claim_receipts", commandID); err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	} else if counterpart {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: command %q has both claim preparation and receipt", ErrLocalNudgeAuthorityConflict, commandID)
	}
	return record, true, nil
}

func localAuthorityClaimReceiptByCommand(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (localAuthorityClaimTransitionRecord, bool, error) {
	record, found, err := scanLocalAuthorityClaimTransition(queryer.QueryRowContext(ctx, `SELECT
		sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until,
		effect_revision, effect_sequence_high_water
		FROM claim_receipts WHERE command_id = ?`, commandID), store, commandID, true)
	if err != nil || !found {
		return record, found, err
	}
	if counterpart, err := localAuthorityClaimTransitionRowExists(ctx, queryer, "claim_preparations", commandID); err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	} else if counterpart {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: command %q has both claim preparation and receipt", ErrLocalNudgeAuthorityConflict, commandID)
	}
	highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(ctx, queryer)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	}
	if highestSequence < record.effectSequenceHighWater || highestRevision < record.effectRevision {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf(
			"%w: authority high-water %d/%d does not dominate persisted claim receipt effect %d/%d",
			ErrLocalNudgeAuthorityConflict, highestRevision, highestSequence, record.effectRevision, record.effectSequenceHighWater,
		)
	}
	return record, true, nil
}

func localAuthorityClaimTransitionRowExists(ctx context.Context, queryer localAuthorityQueryer, table, commandID string) (bool, error) {
	var found int
	query := ""
	switch table {
	case "claim_preparations":
		query = `SELECT 1 FROM claim_preparations WHERE command_id = ?`
	case "claim_receipts":
		query = `SELECT 1 FROM claim_receipts WHERE command_id = ?`
	default:
		return false, fmt.Errorf("%w: unknown claim transition table %q", ErrLocalNudgeAuthorityConflict, table)
	}
	err := queryer.QueryRowContext(ctx, query, commandID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading local claim transition counterpart: %w", err)
	}
	return found == 1, nil
}

func localAuthorityClaimTransitionGeneration(ctx context.Context, queryer localAuthorityQueryer) (uint64, error) {
	var wire []byte
	if err := queryer.QueryRowContext(ctx, `SELECT claim_transition_generation FROM authority_meta WHERE singleton = 1`).Scan(&wire); err != nil {
		return 0, fmt.Errorf("reading local claim transition generation: %w", err)
	}
	return decodeLocalAuthorityUint64(wire)
}

func advanceLocalAuthorityClaimTransitionGeneration(ctx context.Context, tx *sql.Tx, preparationDelta, receiptDelta int) error {
	var generationWire, preparationsWire, receiptsWire []byte
	if err := tx.QueryRowContext(ctx, `SELECT claim_transition_generation, claim_preparation_count, claim_receipt_count
		FROM authority_meta WHERE singleton = 1`).Scan(&generationWire, &preparationsWire, &receiptsWire); err != nil {
		return fmt.Errorf("reading local claim transition mutation state: %w", err)
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
		return fmt.Errorf("%w: claim transition generation is exhausted", ErrLocalNudgeAuthorityConflict)
	}
	preparations, err = applyLocalAuthorityClaimCountDelta(preparations, preparationDelta)
	if err != nil {
		return err
	}
	receipts, err = applyLocalAuthorityClaimCountDelta(receipts, receiptDelta)
	if err != nil {
		return err
	}
	generation++
	if _, err := tx.ExecContext(ctx, `UPDATE authority_meta SET claim_transition_generation = ?, claim_preparation_count = ?, claim_receipt_count = ? WHERE singleton = 1`,
		encodeLocalAuthorityUint64(generation), encodeLocalAuthorityUint64(preparations), encodeLocalAuthorityUint64(receipts)); err != nil {
		return fmt.Errorf("advancing local claim transition generation: %w", err)
	}
	return nil
}

func applyLocalAuthorityClaimCountDelta(value uint64, delta int) (uint64, error) {
	switch {
	case delta > 0:
		increment := uint64(delta)
		if value > math.MaxUint64-increment {
			return 0, fmt.Errorf("%w: claim transition evidence count overflow", ErrLocalNudgeAuthorityConflict)
		}
		return value + increment, nil
	case delta < 0:
		decrement := uint64(-delta)
		if value < decrement {
			return 0, fmt.Errorf("%w: claim transition evidence count underflow", ErrLocalNudgeAuthorityConflict)
		}
		return value - decrement, nil
	default:
		return value, nil
	}
}

func (a *LocalNudgeAuthority) snapshotClaimRecoveryToken(ctx context.Context) (localAuthorityClaimRecoveryToken, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, err
	}
	defer release()
	var generationWire, preparationsWire, receiptsWire []byte
	if err := a.db.QueryRowContext(ctx, `SELECT claim_transition_generation, claim_preparation_count, claim_receipt_count
		FROM authority_meta WHERE singleton = 1`).Scan(&generationWire, &preparationsWire, &receiptsWire); err != nil {
		return localAuthorityClaimRecoveryToken{}, fmt.Errorf("snapshotting local claim transition summary: %w", err)
	}
	token := localAuthorityClaimRecoveryToken{}
	token.generation, err = decodeLocalAuthorityUint64(generationWire)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, err
	}
	token.preparations, err = decodeLocalAuthorityUint64(preparationsWire)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, err
	}
	token.receipts, err = decodeLocalAuthorityUint64(receiptsWire)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, err
	}
	var canonical bytes.Buffer
	writeLocalClaimRecoveryUint64(&canonical, token.generation)
	writeLocalClaimRecoveryUint64(&canonical, token.preparations)
	writeLocalClaimRecoveryUint64(&canonical, token.receipts)
	token.identity = sha256.Sum256(canonical.Bytes())
	return token, nil
}

func localAuthorityClaimRecordIdentity(record localAuthorityClaimTransitionRecord, receipt bool) [sha256.Size]byte {
	var canonical bytes.Buffer
	writeLocalClaimRecoveryRecord(&canonical, record, receipt)
	return sha256.Sum256(canonical.Bytes())
}

func writeLocalClaimRecoveryRecord(buffer *bytes.Buffer, record localAuthorityClaimTransitionRecord, receipt bool) {
	intent := record.intent
	writeLocalClaimRecoveryString(buffer, intent.Store.StoreUUID)
	writeLocalClaimRecoveryUint64(buffer, intent.Store.RestoreEpoch)
	writeLocalClaimRecoveryUint64(buffer, intent.RepositoryBeforeRevision)
	writeLocalClaimRecoveryUint64(buffer, intent.RepositoryRevision)
	writeLocalClaimRecoveryUint64(buffer, intent.RepositorySequenceHighWater)
	writeLocalClaimRecoveryString(buffer, intent.CommandID)
	writeLocalClaimRecoveryUint64(buffer, intent.Sequence)
	_, _ = buffer.Write(intent.Partition.identity[:])
	_, _ = buffer.Write(intent.BeforeCommandDigest[:])
	_, _ = buffer.Write(intent.AfterCommandDigest[:])
	for _, value := range []string{
		intent.Claim.ID, intent.Claim.OwnerID, intent.Claim.OperationID, intent.Claim.AttemptID,
		intent.Claim.BoundLaunchIdentity, intent.Claim.AuthorizationDecisionID,
		intent.Claim.AuthorizationPolicyVersion, intent.Claim.ClaimedAt.Format(time.RFC3339Nano),
		intent.Claim.LeaseUntil.Format(time.RFC3339Nano),
	} {
		writeLocalClaimRecoveryString(buffer, value)
	}
	if receipt {
		writeLocalClaimRecoveryUint64(buffer, record.effectRevision)
		writeLocalClaimRecoveryUint64(buffer, record.effectSequenceHighWater)
	}
}

func writeLocalClaimRecoveryString(buffer *bytes.Buffer, value string) {
	writeLocalClaimRecoveryUint64(buffer, uint64(len(value)))
	_, _ = buffer.WriteString(value)
}

func writeLocalClaimRecoveryUint64(buffer *bytes.Buffer, value uint64) {
	var wire [8]byte
	binary.BigEndian.PutUint64(wire[:], value)
	_, _ = buffer.Write(wire[:])
}

func scanLocalAuthorityClaimTransition(row *sql.Row, store CommandStoreBinding, commandID string, receipt bool) (localAuthorityClaimTransitionRecord, bool, error) {
	var sequenceWire, partitionWire, beforeRevisionWire, claimRevisionWire, sequenceHighWaterWire []byte
	var beforeDigestWire, afterDigestWire []byte
	var claimID, ownerID, operationID, attemptID, launchIdentity, decisionID, policyVersion, claimedAtWire, leaseUntilWire string
	var effectRevisionWire, effectSequenceWire []byte
	destinations := []any{
		&sequenceWire, &partitionWire, &beforeRevisionWire, &claimRevisionWire, &sequenceHighWaterWire,
		&beforeDigestWire, &afterDigestWire, &claimID, &ownerID, &operationID, &attemptID, &launchIdentity,
		&decisionID, &policyVersion, &claimedAtWire, &leaseUntilWire,
	}
	if receipt {
		destinations = append(destinations, &effectRevisionWire, &effectSequenceWire)
	}
	if err := row.Scan(destinations...); errors.Is(err, sql.ErrNoRows) {
		return localAuthorityClaimTransitionRecord{}, false, nil
	} else if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("reading local claim transition: %w", err)
	}
	sequence, err := decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	}
	beforeRevision, err := decodeLocalAuthorityUint64(beforeRevisionWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	}
	claimRevision, err := decodeLocalAuthorityUint64(claimRevisionWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	}
	sequenceHighWater, err := decodeLocalAuthorityUint64(sequenceHighWaterWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, err
	}
	if len(partitionWire) != sha256.Size || len(beforeDigestWire) != sha256.Size || len(afterDigestWire) != sha256.Size {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: malformed claim transition digest or partition", ErrLocalNudgeAuthorityConflict)
	}
	claimedAt, err := time.Parse(time.RFC3339Nano, claimedAtWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: malformed claim claimed_at", ErrLocalNudgeAuthorityConflict)
	}
	leaseUntil, err := time.Parse(time.RFC3339Nano, leaseUntilWire)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: malformed claim lease_until", ErrLocalNudgeAuthorityConflict)
	}
	record := localAuthorityClaimTransitionRecord{intent: CommandClaimTransitionIntent{
		Store: store, RepositoryBeforeRevision: beforeRevision, RepositoryRevision: claimRevision,
		RepositorySequenceHighWater: sequenceHighWater, CommandID: commandID, Sequence: sequence,
		Claim: CommandClaim{
			ID: claimID, OwnerID: ownerID, OperationID: operationID, AttemptID: attemptID,
			BoundLaunchIdentity: launchIdentity, AuthorizationDecisionID: decisionID,
			AuthorizationPolicyVersion: policyVersion, ClaimedAt: claimedAt, LeaseUntil: leaseUntil,
		},
	}}
	copy(record.intent.Partition.identity[:], partitionWire)
	copy(record.intent.BeforeCommandDigest[:], beforeDigestWire)
	copy(record.intent.AfterCommandDigest[:], afterDigestWire)
	if err := validateCommandClaimTransitionIntent(record.intent); err != nil {
		return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: malformed persisted claim transition: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if receipt {
		effectRevision, err := decodeLocalAuthorityUint64(effectRevisionWire)
		if err != nil {
			return localAuthorityClaimTransitionRecord{}, false, err
		}
		effectSequence, err := decodeLocalAuthorityUint64(effectSequenceWire)
		if err != nil {
			return localAuthorityClaimTransitionRecord{}, false, err
		}
		record.effectRevision = effectRevision
		record.effectSequenceHighWater = effectSequence
		if effectRevision < record.intent.RepositoryRevision || effectSequence < record.intent.RepositorySequenceHighWater {
			return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf(
				"%w: persisted claim receipt effect %d/%d is behind its prepared repository state %d/%d",
				ErrLocalNudgeAuthorityConflict, effectRevision, effectSequence,
				record.intent.RepositoryRevision, record.intent.RepositorySequenceHighWater,
			)
		}
		if err := validateCommandClaimTransitionReceipt(localClaimTransitionReceipt(record)); err != nil {
			return localAuthorityClaimTransitionRecord{}, false, fmt.Errorf("%w: malformed persisted claim receipt: %w", ErrLocalNudgeAuthorityConflict, err)
		}
	}
	return record, true, nil
}

func localClaimTransitionReceipt(record localAuthorityClaimTransitionRecord) CommandClaimTransitionReceipt {
	return CommandClaimTransitionReceipt{
		Store: record.intent.Store, RepositoryRevision: record.intent.RepositoryRevision,
		CommandID: record.intent.CommandID, Sequence: record.intent.Sequence, Partition: record.intent.Partition,
		AfterCommandDigest: record.intent.AfterCommandDigest, Claim: record.intent.Claim,
		EffectRepositoryRevision: record.effectRevision, EffectSequenceHighWater: record.effectSequenceHighWater,
	}
}

var _ TrustedCommandClaimTransitionAuthority = (*LocalNudgeAuthority)(nil)
