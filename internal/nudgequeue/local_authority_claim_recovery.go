package nudgequeue

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
)

type localAuthorityClaimAuditPhase string

const (
	localAuthorityClaimAuditIdle         localAuthorityClaimAuditPhase = "idle"
	localAuthorityClaimAuditPreparations localAuthorityClaimAuditPhase = "preparations"
	localAuthorityClaimAuditReceipts     localAuthorityClaimAuditPhase = "receipts"
	localAuthorityClaimAuditActive       localAuthorityClaimAuditPhase = "active"
	localAuthorityClaimAuditDone         localAuthorityClaimAuditPhase = "done"
)

var (
	errLocalAuthorityClaimAuditMoved             = errors.New("local claim audit binding moved")
	errLocalAuthorityClaimAuditCheckpointInvalid = errors.New("local claim audit checkpoint is invalid")
)

type localAuthorityClaimAuditCursor struct {
	generation         uint64
	repositoryRevision uint64
	sequenceHighWater  uint64
	phase              localAuthorityClaimAuditPhase
	afterCommandID     string
	afterSequence      uint64
	identity           [sha256.Size]byte
	preparationCount   uint64
	receiptCount       uint64
}

func (c localAuthorityClaimAuditCursor) token() localAuthorityClaimRecoveryToken {
	return localAuthorityClaimRecoveryToken{
		generation: c.generation, identity: c.identity,
		preparations: c.preparationCount, receipts: c.receiptCount,
	}
}

// repairCommandClaimTransitions resolves claim write-ahead residue and audits
// every authority-admitted active command against independent claim evidence.
// Its cursor and rolling identity are durable, so bounded recovery invocations
// resume rather than restarting at the first row forever. Resume relies on the
// command repository contract that every row mutation advances its revision;
// an out-of-band rewrite that preserves the same revision requires an external
// authenticated snapshot/root digest and is outside this local store contract.
func (a *LocalNudgeAuthority) repairCommandClaimTransitions(
	ctx context.Context,
	repository *CommandRepository,
	state CommandRepositoryState,
) (localAuthorityClaimRecoveryToken, bool, error) {
	if repository == nil {
		return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("%w: command repository is required", ErrLocalNudgeAuthorityConflict)
	}
	ctx, budget := withCommandAuthorityRecoveryBudget(ctx)
	cursor, err := a.ensureClaimAuditCursor(ctx, state)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, false, err
	}
	for {
		if cursor.phase == localAuthorityClaimAuditDone {
			return cursor.token(), true, nil
		}
		remaining := budget.remainingWork()
		if remaining == 0 {
			return localAuthorityClaimRecoveryToken{}, false, budget.takeWork("auditing local claim transitions")
		}
		limit := min(localAuthorityRecoveryPageSize, remaining)
		var next localAuthorityClaimAuditCursor
		var work int
		var changed, deferred bool
		switch cursor.phase {
		case localAuthorityClaimAuditPreparations:
			next, work, changed, deferred, err = a.auditClaimPreparationPage(ctx, repository, state, cursor, limit)
		case localAuthorityClaimAuditReceipts:
			next, work, deferred, err = a.auditClaimReceiptPage(ctx, repository, state, cursor, limit)
		case localAuthorityClaimAuditActive:
			next, work, deferred, err = a.auditActiveCommandClaimPage(ctx, repository, state, cursor, limit)
		default:
			return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("%w: unsupported durable claim audit phase %q", ErrLocalNudgeAuthorityConflict, cursor.phase)
		}
		if budgetErr := budget.takeWorkUnits(work, "auditing local claim transitions"); budgetErr != nil {
			return localAuthorityClaimRecoveryToken{}, false, budgetErr
		}
		if err != nil {
			moved, movementErr := a.claimAuditBindingMoved(ctx, repository, cursor)
			if movementErr != nil {
				return localAuthorityClaimRecoveryToken{}, false, errors.Join(err, movementErr)
			}
			if moved {
				return localAuthorityClaimRecoveryToken{}, false, nil
			}
			return localAuthorityClaimRecoveryToken{}, false, err
		}
		if next != cursor {
			if err := a.persistClaimAuditCursor(ctx, cursor, next); err != nil {
				if errors.Is(err, errLocalAuthorityClaimAuditMoved) {
					return localAuthorityClaimRecoveryToken{}, false, nil
				}
				return localAuthorityClaimRecoveryToken{}, false, err
			}
			cursor = next
		}
		if changed || deferred {
			return localAuthorityClaimRecoveryToken{}, false, nil
		}
	}
}

func (a *LocalNudgeAuthority) auditClaimPreparationPage(
	ctx context.Context,
	repository *CommandRepository,
	state CommandRepositoryState,
	cursor localAuthorityClaimAuditCursor,
	limit int,
) (localAuthorityClaimAuditCursor, int, bool, bool, error) {
	next := cursor
	commandIDs, more, err := a.localAuthorityClaimEvidencePage(ctx, "claim_preparations", cursor.afterCommandID, limit)
	if err != nil {
		return cursor, 0, false, false, fmt.Errorf("auditing local claim preparations: %w", err)
	}
	if len(commandIDs) == 0 {
		next.phase = localAuthorityClaimAuditReceipts
		next.afterCommandID = ""
		return next, 0, false, false, nil
	}
	for index, commandID := range commandIDs {
		intent, err := a.readClaimPreparation(ctx, commandID)
		if err != nil {
			return cursor, index, false, false, err
		}
		ownedDuringRepositoryRead, err := a.claimWriterOwnsIntent(ctx, intent)
		if err != nil {
			return cursor, index, false, false, err
		}
		resolution, command, digest, err := readClaimRecoveryCommand(ctx, repository, state, intent.CommandID)
		if err != nil {
			return cursor, index + 1, false, false, err
		}
		if resolution.Revision < intent.RepositoryBeforeRevision || resolution.SequenceHighWater < intent.RepositorySequenceHighWater ||
			command.Store != intent.Store || command.ID != intent.CommandID || command.Order.Sequence != intent.Sequence {
			return cursor, index + 1, false, false, fmt.Errorf("%w: prepared claim command %q has inconsistent repository identity", ErrLocalNudgeAuthorityConflict, commandID)
		}
		switch {
		case digest == intent.BeforeCommandDigest && command.State == CommandStatePending && command.Claim == nil && command.Terminal == nil:
			if err := a.verifyLocalAuthorityPendingRetry(ctx, a.db, state, command, digest, intent.Partition); err != nil {
				return cursor, index + 1, false, false, err
			}
			if ownedDuringRepositoryRead {
				return next, index + 1, false, true, nil
			}
			aborted, err := a.abortRecoveredCommandClaimTransition(ctx, intent)
			if err != nil {
				return cursor, index + 1, false, false, err
			}
			return cursor, index + 1, aborted, !aborted, nil
		case digest == intent.AfterCommandDigest && command.State == CommandStateInFlight && command.Claim != nil && command.Retry != nil && command.Terminal == nil &&
			commandClaimsEqual(*command.Claim, intent.Claim) && resolution.Revision >= intent.RepositoryRevision:
			next.identity = advanceLocalAuthorityClaimAuditIdentity(next.identity, "preparation", localAuthorityClaimRecordIdentity(localAuthorityClaimTransitionRecord{intent: intent}, false))
			next.preparationCount++
			next.afterCommandID = commandID
		case commandIsTerminalState(command.State) && command.Terminal != nil:
			valid, err := a.claimEvidenceHasExactPendingTerminal(ctx, intent, command, digest)
			if err != nil {
				return cursor, index + 1, false, false, err
			}
			if !valid {
				return cursor, index + 1, false, false, fmt.Errorf("%w: prepared claim command %q is neither exact before nor after state", ErrLocalNudgeAuthorityConflict, commandID)
			}
			return next, index + 1, false, true, nil
		default:
			return cursor, index + 1, false, false, fmt.Errorf("%w: prepared claim command %q is neither exact before nor after state", ErrLocalNudgeAuthorityConflict, commandID)
		}
	}
	if !more {
		next.phase = localAuthorityClaimAuditReceipts
		next.afterCommandID = ""
	}
	return next, len(commandIDs), false, false, nil
}

func (a *LocalNudgeAuthority) auditClaimReceiptPage(
	ctx context.Context,
	repository *CommandRepository,
	state CommandRepositoryState,
	cursor localAuthorityClaimAuditCursor,
	limit int,
) (localAuthorityClaimAuditCursor, int, bool, error) {
	next := cursor
	commandIDs, more, err := a.localAuthorityClaimEvidencePage(ctx, "claim_receipts", cursor.afterCommandID, limit)
	if err != nil {
		return cursor, 0, false, fmt.Errorf("auditing local claim receipts: %w", err)
	}
	if len(commandIDs) == 0 {
		next.phase = localAuthorityClaimAuditActive
		next.afterCommandID = ""
		next.afterSequence = 0
		return next, 0, false, nil
	}
	for index, commandID := range commandIDs {
		record, err := a.readClaimReceipt(ctx, commandID)
		if err != nil {
			return cursor, index, false, err
		}
		resolution, command, digest, err := readClaimRecoveryCommand(ctx, repository, state, commandID)
		if err != nil {
			return cursor, index + 1, false, err
		}
		if resolution.Revision < record.effectRevision || resolution.SequenceHighWater < record.effectSequenceHighWater ||
			command.Store != record.intent.Store || command.ID != record.intent.CommandID || command.Order.Sequence != record.intent.Sequence {
			return cursor, index + 1, false, fmt.Errorf("%w: finalized claim command %q has inconsistent repository identity", ErrLocalNudgeAuthorityConflict, commandID)
		}
		switch {
		case digest == record.intent.AfterCommandDigest && command.State == CommandStateInFlight && command.Claim != nil && command.Retry != nil && command.Terminal == nil &&
			commandClaimsEqual(*command.Claim, record.intent.Claim):
			next.identity = advanceLocalAuthorityClaimAuditIdentity(next.identity, "receipt", localAuthorityClaimRecordIdentity(record, true))
			next.receiptCount++
			next.afterCommandID = commandID
		case commandIsTerminalState(command.State) && command.Terminal != nil:
			valid, err := a.claimEvidenceHasExactPendingTerminal(ctx, record.intent, command, digest)
			if err != nil {
				return cursor, index + 1, false, err
			}
			if !valid {
				return cursor, index + 1, false, fmt.Errorf("%w: finalized claim command %q differs from its receipt", ErrLocalNudgeAuthorityConflict, commandID)
			}
			return next, index + 1, true, nil
		default:
			return cursor, index + 1, false, fmt.Errorf("%w: finalized claim command %q differs from its receipt", ErrLocalNudgeAuthorityConflict, commandID)
		}
	}
	if !more {
		next.phase = localAuthorityClaimAuditActive
		next.afterCommandID = ""
		next.afterSequence = 0
	}
	return next, len(commandIDs), false, nil
}

func (a *LocalNudgeAuthority) auditActiveCommandClaimPage(
	ctx context.Context,
	repository *CommandRepository,
	state CommandRepositoryState,
	cursor localAuthorityClaimAuditCursor,
	limit int,
) (localAuthorityClaimAuditCursor, int, bool, error) {
	next := cursor
	admissions, more, err := a.localAuthorityActiveAdmissionPage(ctx, cursor.afterSequence, limit)
	if err != nil {
		return cursor, 0, false, fmt.Errorf("auditing active local command claims: %w", err)
	}
	if len(admissions) == 0 {
		next.phase = localAuthorityClaimAuditDone
		next.afterSequence = 0
		return next, 0, false, nil
	}
	for index, admission := range admissions {
		_, command, digest, err := readClaimRecoveryCommand(ctx, repository, state, admission.commandID)
		if err != nil {
			return cursor, index + 1, false, err
		}
		if command.Store != a.store || command.ID != admission.commandID || command.Order.Sequence != admission.sequence ||
			trustedCityPartitionFromAuthority(command.TrustedIngress) != admission.partition || command.Order.Revision < admission.decisionRevision {
			return cursor, index + 1, false, fmt.Errorf("%w: active admitted command %q differs from authority membership", ErrLocalNudgeAuthorityConflict, admission.commandID)
		}
		preparation, hasPreparation, receipt, hasReceipt, err := a.readClaimEvidence(ctx, admission.commandID)
		if err != nil {
			return cursor, index + 1, false, err
		}
		switch command.State {
		case CommandStatePending:
			if command.Claim != nil || command.Terminal != nil || hasPreparation || hasReceipt {
				return cursor, index + 1, false, fmt.Errorf("%w: pending command %q retained claim evidence", ErrLocalNudgeAuthorityConflict, admission.commandID)
			}
			if err := a.verifyLocalAuthorityPendingRetry(ctx, a.db, state, command, digest, admission.partition); err != nil {
				return cursor, index + 1, false, err
			}
		case CommandStateInFlight:
			if command.Claim == nil || command.Retry == nil || command.Terminal != nil || hasPreparation == hasReceipt {
				return cursor, index + 1, false, fmt.Errorf("%w: in-flight command %q lacks exactly one claim preparation or receipt", ErrLocalNudgeAuthorityConflict, admission.commandID)
			}
			record := preparation
			if hasReceipt {
				record = receipt
			}
			if record.intent.AfterCommandDigest != digest || !commandClaimsEqual(record.intent.Claim, *command.Claim) {
				return cursor, index + 1, false, fmt.Errorf("%w: in-flight command %q differs from retained claim evidence", ErrLocalNudgeAuthorityConflict, admission.commandID)
			}
		default:
			if !commandIsTerminalState(command.State) || command.Terminal == nil {
				return cursor, index + 1, false, fmt.Errorf("%w: active admitted command %q has unsupported state %q", ErrLocalNudgeAuthorityConflict, admission.commandID, command.State)
			}
			var intent CommandClaimTransitionIntent
			if hasPreparation {
				intent = preparation.intent
			} else if hasReceipt {
				intent = receipt.intent
			}
			valid, err := a.claimEvidenceHasExactPendingTerminal(ctx, intent, command, digest)
			if err != nil {
				return cursor, index + 1, false, err
			}
			if !valid {
				return cursor, index + 1, false, fmt.Errorf("%w: active terminal command %q lacks exact terminal transition evidence", ErrLocalNudgeAuthorityConflict, admission.commandID)
			}
			return next, index + 1, true, nil
		}
		next.identity = advanceLocalAuthorityClaimAuditIdentity(next.identity, "active", localAuthorityActiveClaimIdentity(admission, command, digest, preparation, hasPreparation, receipt, hasReceipt))
		next.afterSequence = admission.sequence
	}
	if !more {
		next.phase = localAuthorityClaimAuditDone
		next.afterSequence = 0
	}
	return next, len(admissions), false, nil
}

func advanceLocalAuthorityClaimAuditIdentity(current [sha256.Size]byte, kind string, item [sha256.Size]byte) [sha256.Size]byte {
	var canonical bytes.Buffer
	_, _ = canonical.Write(current[:])
	writeLocalClaimRecoveryString(&canonical, kind)
	_, _ = canonical.Write(item[:])
	return sha256.Sum256(canonical.Bytes())
}

func localAuthorityActiveClaimIdentity(
	admission localAuthorityActiveAdmission,
	command Command,
	commandDigest [sha256.Size]byte,
	preparation localAuthorityClaimTransitionRecord,
	hasPreparation bool,
	receipt localAuthorityClaimTransitionRecord,
	hasReceipt bool,
) [sha256.Size]byte {
	var canonical bytes.Buffer
	writeLocalClaimRecoveryString(&canonical, admission.commandID)
	writeLocalClaimRecoveryUint64(&canonical, admission.sequence)
	_, _ = canonical.Write(admission.partition.identity[:])
	writeLocalClaimRecoveryUint64(&canonical, admission.decisionRevision)
	writeLocalClaimRecoveryUint64(&canonical, command.Order.Revision)
	_, _ = canonical.Write(commandDigest[:])
	switch {
	case hasPreparation:
		writeLocalClaimRecoveryString(&canonical, "preparation")
		identity := localAuthorityClaimRecordIdentity(preparation, false)
		_, _ = canonical.Write(identity[:])
	case hasReceipt:
		writeLocalClaimRecoveryString(&canonical, "receipt")
		identity := localAuthorityClaimRecordIdentity(receipt, true)
		_, _ = canonical.Write(identity[:])
	default:
		writeLocalClaimRecoveryString(&canonical, "none")
	}
	return sha256.Sum256(canonical.Bytes())
}

func initialLocalAuthorityClaimAuditIdentity() [sha256.Size]byte {
	return sha256.Sum256([]byte("gascity.local-claim-audit.v1"))
}

func (a *LocalNudgeAuthority) ensureClaimAuditCursor(ctx context.Context, state CommandRepositoryState) (localAuthorityClaimAuditCursor, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("binding local claim audit cursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cursor, err := a.readLocalAuthorityClaimAuditCursor(ctx, tx)
	checkpointInvalid := errors.Is(err, errLocalAuthorityClaimAuditCheckpointInvalid)
	if err != nil && !checkpointInvalid {
		return localAuthorityClaimAuditCursor{}, err
	}
	generation, preparations, receipts, err := localAuthorityClaimMutationState(ctx, tx)
	if err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if !checkpointInvalid && cursor.generation > generation {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("%w: claim audit generation %d exceeds mutation generation %d", ErrLocalNudgeAuthorityConflict, cursor.generation, generation)
	}
	if checkpointInvalid || cursor.phase == localAuthorityClaimAuditIdle || cursor.generation != generation ||
		cursor.repositoryRevision != state.Revision || cursor.sequenceHighWater != state.SequenceHighWater ||
		(cursor.phase == localAuthorityClaimAuditDone && (cursor.preparationCount != preparations || cursor.receiptCount != receipts)) {
		cursor = localAuthorityClaimAuditCursor{
			generation: generation, repositoryRevision: state.Revision, sequenceHighWater: state.SequenceHighWater,
			phase: localAuthorityClaimAuditPreparations, identity: initialLocalAuthorityClaimAuditIdentity(),
		}
		if err := a.updateLocalAuthorityClaimAuditCursor(ctx, tx, cursor); err != nil {
			return localAuthorityClaimAuditCursor{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("binding local claim audit cursor: commit: %w", err)
	}
	return cursor, nil
}

func (a *LocalNudgeAuthority) persistClaimAuditCursor(ctx context.Context, expected, next localAuthorityClaimAuditCursor) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("persisting local claim audit cursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	current, err := a.readLocalAuthorityClaimAuditCursor(ctx, tx)
	if errors.Is(err, errLocalAuthorityClaimAuditCheckpointInvalid) {
		generation, _, _, mutationErr := localAuthorityClaimMutationState(ctx, tx)
		if mutationErr != nil {
			return mutationErr
		}
		reset := localAuthorityClaimAuditCursor{
			generation: generation, repositoryRevision: expected.repositoryRevision, sequenceHighWater: expected.sequenceHighWater,
			phase: localAuthorityClaimAuditPreparations, identity: initialLocalAuthorityClaimAuditIdentity(),
		}
		if updateErr := a.updateLocalAuthorityClaimAuditCursor(ctx, tx, reset); updateErr != nil {
			return updateErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return fmt.Errorf("resetting invalid local claim audit cursor: commit: %w", commitErr)
		}
		return errLocalAuthorityClaimAuditMoved
	}
	if err != nil {
		return err
	}
	generation, preparations, receipts, err := localAuthorityClaimMutationState(ctx, tx)
	if err != nil {
		return err
	}
	if current != expected || generation != expected.generation {
		return errLocalAuthorityClaimAuditMoved
	}
	if next.phase == localAuthorityClaimAuditDone && (next.preparationCount != preparations || next.receiptCount != receipts) {
		return fmt.Errorf("%w: audited claim evidence counts %d/%d differ from authority metadata %d/%d",
			ErrLocalNudgeAuthorityConflict, next.preparationCount, next.receiptCount, preparations, receipts)
	}
	if err := a.updateLocalAuthorityClaimAuditCursor(ctx, tx, next); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("persisting local claim audit cursor: commit: %w", err)
	}
	return nil
}

func (a *LocalNudgeAuthority) completedClaimAuditToken(
	ctx context.Context,
	state CommandRepositoryState,
) (localAuthorityClaimRecoveryToken, bool, error) {
	if state.Store != a.store {
		return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("%w: claim audit repository store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	release, err := a.begin(ctx)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, false, err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("reading completed local claim audit: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	cursor, err := a.readLocalAuthorityClaimAuditCursor(ctx, tx)
	if errors.Is(err, errLocalAuthorityClaimAuditCheckpointInvalid) {
		generation, _, _, mutationErr := localAuthorityClaimMutationState(ctx, tx)
		if mutationErr != nil {
			return localAuthorityClaimRecoveryToken{}, false, mutationErr
		}
		reset := localAuthorityClaimAuditCursor{
			generation: generation, repositoryRevision: state.Revision, sequenceHighWater: state.SequenceHighWater,
			phase: localAuthorityClaimAuditPreparations, identity: initialLocalAuthorityClaimAuditIdentity(),
		}
		if updateErr := a.updateLocalAuthorityClaimAuditCursor(ctx, tx, reset); updateErr != nil {
			return localAuthorityClaimRecoveryToken{}, false, updateErr
		}
		if commitErr := tx.Commit(); commitErr != nil {
			return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("resetting invalid completed local claim audit: commit: %w", commitErr)
		}
		return localAuthorityClaimRecoveryToken{}, false, nil
	}
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, false, err
	}
	generation, preparations, receipts, err := localAuthorityClaimMutationState(ctx, tx)
	if err != nil {
		return localAuthorityClaimRecoveryToken{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf("reading completed local claim audit: commit: %w", err)
	}
	if cursor.phase != localAuthorityClaimAuditDone || cursor.repositoryRevision != state.Revision ||
		cursor.sequenceHighWater != state.SequenceHighWater || cursor.generation != generation {
		return localAuthorityClaimRecoveryToken{}, false, nil
	}
	if cursor.preparationCount != preparations || cursor.receiptCount != receipts {
		return localAuthorityClaimRecoveryToken{}, false, fmt.Errorf(
			"%w: completed claim audit counts %d/%d differ from authority metadata %d/%d",
			ErrLocalNudgeAuthorityConflict, cursor.preparationCount, cursor.receiptCount, preparations, receipts,
		)
	}
	return cursor.token(), true, nil
}

func (a *LocalNudgeAuthority) claimAuditBindingMoved(ctx context.Context, repository *CommandRepository, cursor localAuthorityClaimAuditCursor) (bool, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	generation, err := localAuthorityClaimTransitionGeneration(ctx, a.db)
	release()
	if err != nil {
		return false, err
	}
	if generation != cursor.generation {
		return true, nil
	}
	state, err := repository.State(ctx)
	if err != nil {
		return false, nil
	}
	return state.Revision != cursor.repositoryRevision || state.SequenceHighWater != cursor.sequenceHighWater, nil
}

func localAuthorityClaimMutationState(ctx context.Context, queryer localAuthorityQueryer) (generation, preparations, receipts uint64, err error) {
	var generationWire, preparationsWire, receiptsWire []byte
	if err := queryer.QueryRowContext(ctx, `SELECT claim_transition_generation, claim_preparation_count, claim_receipt_count
		FROM authority_meta WHERE singleton = 1`).Scan(&generationWire, &preparationsWire, &receiptsWire); err != nil {
		return 0, 0, 0, fmt.Errorf("reading local claim mutation state: %w", err)
	}
	generation, err = decodeLocalAuthorityUint64(generationWire)
	if err != nil {
		return 0, 0, 0, err
	}
	preparations, err = decodeLocalAuthorityUint64(preparationsWire)
	if err != nil {
		return 0, 0, 0, err
	}
	receipts, err = decodeLocalAuthorityUint64(receiptsWire)
	if err != nil {
		return 0, 0, 0, err
	}
	return generation, preparations, receipts, nil
}

func (a *LocalNudgeAuthority) readLocalAuthorityClaimAuditCursor(ctx context.Context, queryer localAuthorityQueryer) (localAuthorityClaimAuditCursor, error) {
	var generationWire, revisionWire, sequenceWire, afterSequenceWire, identityWire, preparationsWire, receiptsWire, checkpointDigest []byte
	var phase, afterCommandID string
	if err := queryer.QueryRowContext(ctx, `SELECT claim_audit_generation, claim_audit_repository_revision,
		claim_audit_sequence_high_water, claim_audit_phase, claim_audit_after_command_id,
		claim_audit_after_sequence, claim_audit_identity, claim_audit_preparation_count, claim_audit_receipt_count,
		claim_audit_checkpoint_digest
		FROM authority_meta WHERE singleton = 1`).Scan(
		&generationWire, &revisionWire, &sequenceWire, &phase, &afterCommandID,
		&afterSequenceWire, &identityWire, &preparationsWire, &receiptsWire, &checkpointDigest,
	); err != nil {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("reading local claim audit cursor: %w", err)
	}
	cursor := localAuthorityClaimAuditCursor{phase: localAuthorityClaimAuditPhase(phase), afterCommandID: afterCommandID}
	var err error
	if cursor.generation, err = decodeLocalAuthorityUint64(generationWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if cursor.repositoryRevision, err = decodeLocalAuthorityUint64(revisionWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if cursor.sequenceHighWater, err = decodeLocalAuthorityUint64(sequenceWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if cursor.afterSequence, err = decodeLocalAuthorityUint64(afterSequenceWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if cursor.preparationCount, err = decodeLocalAuthorityUint64(preparationsWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if cursor.receiptCount, err = decodeLocalAuthorityUint64(receiptsWire); err != nil {
		return localAuthorityClaimAuditCursor{}, err
	}
	if len(identityWire) != sha256.Size {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("%w: malformed local claim audit identity", ErrLocalNudgeAuthorityConflict)
	}
	copy(cursor.identity[:], identityWire)
	if err := validateLocalAuthorityClaimAuditCursor(cursor); err != nil {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("%w: %w", errLocalAuthorityClaimAuditCheckpointInvalid, err)
	}
	if len(checkpointDigest) != sha256.Size {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("%w: %w: malformed local claim audit checkpoint digest", errLocalAuthorityClaimAuditCheckpointInvalid, ErrLocalNudgeAuthorityConflict)
	}
	wantDigest := localAuthorityClaimAuditCursorDigest(cursor, a.store, a.opts.AuthorityID)
	if !bytes.Equal(checkpointDigest, wantDigest[:]) {
		return localAuthorityClaimAuditCursor{}, fmt.Errorf("%w: %w: local claim audit checkpoint digest differs", errLocalAuthorityClaimAuditCheckpointInvalid, ErrLocalNudgeAuthorityConflict)
	}
	return cursor, nil
}

func (a *LocalNudgeAuthority) updateLocalAuthorityClaimAuditCursor(ctx context.Context, tx *sql.Tx, cursor localAuthorityClaimAuditCursor) error {
	if err := validateLocalAuthorityClaimAuditCursor(cursor); err != nil {
		return err
	}
	checkpointDigest := localAuthorityClaimAuditCursorDigest(cursor, a.store, a.opts.AuthorityID)
	if _, err := tx.ExecContext(ctx, `UPDATE authority_meta SET
		claim_audit_generation = ?, claim_audit_repository_revision = ?, claim_audit_sequence_high_water = ?,
		claim_audit_phase = ?, claim_audit_after_command_id = ?, claim_audit_after_sequence = ?,
		claim_audit_identity = ?, claim_audit_preparation_count = ?, claim_audit_receipt_count = ?,
		claim_audit_checkpoint_digest = ?
		WHERE singleton = 1`, encodeLocalAuthorityUint64(cursor.generation), encodeLocalAuthorityUint64(cursor.repositoryRevision),
		encodeLocalAuthorityUint64(cursor.sequenceHighWater), string(cursor.phase), cursor.afterCommandID,
		encodeLocalAuthorityUint64(cursor.afterSequence), cursor.identity[:], encodeLocalAuthorityUint64(cursor.preparationCount),
		encodeLocalAuthorityUint64(cursor.receiptCount), checkpointDigest[:]); err != nil {
		return fmt.Errorf("updating local claim audit cursor: %w", err)
	}
	return nil
}

func localAuthorityClaimAuditCursorDigest(cursor localAuthorityClaimAuditCursor, store CommandStoreBinding, authorityID string) [sha256.Size]byte {
	// The journal and this unkeyed digest share one local trust boundary. The
	// digest detects torn writes, accidental corruption, and a checkpoint copied
	// across repository or authority identities; it cannot authenticate the
	// journal against an actor that can rewrite every SQLite field coherently.
	var canonical bytes.Buffer
	writeLocalClaimRecoveryString(&canonical, "gascity.local-claim-audit-checkpoint.v1")
	writeLocalClaimRecoveryString(&canonical, store.StoreUUID)
	writeLocalClaimRecoveryUint64(&canonical, store.RestoreEpoch)
	writeLocalClaimRecoveryString(&canonical, authorityID)
	writeLocalClaimRecoveryUint64(&canonical, cursor.generation)
	writeLocalClaimRecoveryUint64(&canonical, cursor.repositoryRevision)
	writeLocalClaimRecoveryUint64(&canonical, cursor.sequenceHighWater)
	writeLocalClaimRecoveryString(&canonical, string(cursor.phase))
	writeLocalClaimRecoveryString(&canonical, cursor.afterCommandID)
	writeLocalClaimRecoveryUint64(&canonical, cursor.afterSequence)
	_, _ = canonical.Write(cursor.identity[:])
	writeLocalClaimRecoveryUint64(&canonical, cursor.preparationCount)
	writeLocalClaimRecoveryUint64(&canonical, cursor.receiptCount)
	return sha256.Sum256(canonical.Bytes())
}

func validateLocalAuthorityClaimAuditCursor(cursor localAuthorityClaimAuditCursor) error {
	if cursor.sequenceHighWater > cursor.repositoryRevision || cursor.preparationCount > cursor.generation || cursor.receiptCount > cursor.generation {
		return fmt.Errorf("%w: inconsistent local claim audit checkpoint high-waters", ErrLocalNudgeAuthorityConflict)
	}
	if cursor.afterCommandID != "" {
		if err := validateCommandIdentity("claim audit command id", cursor.afterCommandID); err != nil {
			return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
		}
	}
	switch cursor.phase {
	case localAuthorityClaimAuditIdle:
		if cursor != (localAuthorityClaimAuditCursor{phase: localAuthorityClaimAuditIdle}) {
			return fmt.Errorf("%w: nonempty idle local claim audit checkpoint", ErrLocalNudgeAuthorityConflict)
		}
		return nil
	case localAuthorityClaimAuditPreparations:
		if cursor.afterSequence != 0 || cursor.receiptCount != 0 || (cursor.afterCommandID == "") != (cursor.preparationCount == 0) {
			return fmt.Errorf("%w: inconsistent preparation claim audit cursor", ErrLocalNudgeAuthorityConflict)
		}
	case localAuthorityClaimAuditReceipts:
		if cursor.afterSequence != 0 || (cursor.afterCommandID == "") != (cursor.receiptCount == 0) {
			return fmt.Errorf("%w: inconsistent receipt claim audit cursor", ErrLocalNudgeAuthorityConflict)
		}
	case localAuthorityClaimAuditActive:
		if cursor.afterCommandID != "" || cursor.afterSequence > cursor.sequenceHighWater {
			return fmt.Errorf("%w: inconsistent active claim audit cursor", ErrLocalNudgeAuthorityConflict)
		}
	case localAuthorityClaimAuditDone:
		if cursor.afterCommandID != "" || cursor.afterSequence != 0 {
			return fmt.Errorf("%w: inconsistent completed claim audit cursor", ErrLocalNudgeAuthorityConflict)
		}
	default:
		return fmt.Errorf("%w: invalid local claim audit phase %q", ErrLocalNudgeAuthorityConflict, cursor.phase)
	}
	if cursor.identity == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: empty local claim audit rolling identity", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func (a *LocalNudgeAuthority) validateClaimAuditMetadata(ctx context.Context, state CommandRepositoryState, allowReset bool) error {
	cursor, err := a.readLocalAuthorityClaimAuditCursor(ctx, a.db)
	if errors.Is(err, errLocalAuthorityClaimAuditCheckpointInvalid) {
		if !allowReset {
			return fmt.Errorf("%w: v3 claim audit checkpoint is invalid before migration", ErrLocalNudgeAuthorityConflict)
		}
		return a.resetClaimAuditCursor(ctx, state)
	}
	if err != nil {
		return err
	}
	generation, preparations, receipts, err := localAuthorityClaimMutationState(ctx, a.db)
	if err != nil {
		return err
	}
	if cursor.generation > generation || cursor.repositoryRevision > state.Revision || cursor.sequenceHighWater > state.SequenceHighWater {
		return fmt.Errorf("%w: local claim audit checkpoint exceeds current authority lineage", ErrLocalNudgeAuthorityConflict)
	}
	if cursor.phase == localAuthorityClaimAuditIdle || cursor.generation != generation {
		return nil
	}
	if cursor.preparationCount > preparations || cursor.receiptCount > receipts {
		return fmt.Errorf("%w: local claim audit checkpoint counts exceed authority metadata", ErrLocalNudgeAuthorityConflict)
	}
	if cursor.phase != localAuthorityClaimAuditPreparations && cursor.preparationCount != preparations {
		return fmt.Errorf("%w: local claim audit checkpoint skipped preparation evidence", ErrLocalNudgeAuthorityConflict)
	}
	if (cursor.phase == localAuthorityClaimAuditActive || cursor.phase == localAuthorityClaimAuditDone) && cursor.receiptCount != receipts {
		return fmt.Errorf("%w: local claim audit checkpoint skipped receipt evidence", ErrLocalNudgeAuthorityConflict)
	}
	// A completed cursor is proof for one authority-process lifetime, not a
	// permanent exemption from anti-entropy. Restart it on reopen so mutations
	// after a completed pass are audited by the next process, while preserving
	// partial cursors that make large crash recovery resumable.
	if cursor.phase == localAuthorityClaimAuditDone {
		if !allowReset {
			return nil
		}
		return a.resetClaimAuditCursor(ctx, state)
	}
	return nil
}

func (a *LocalNudgeAuthority) resetClaimAuditCursor(ctx context.Context, state CommandRepositoryState) error {
	if state.Store != a.store {
		return fmt.Errorf("%w: claim audit reset repository store differs from authority", ErrLocalNudgeAuthorityConflict)
	}
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("resetting local claim audit cursor: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	generation, _, _, err := localAuthorityClaimMutationState(ctx, tx)
	if err != nil {
		return err
	}
	reset := localAuthorityClaimAuditCursor{
		generation: generation, repositoryRevision: state.Revision, sequenceHighWater: state.SequenceHighWater,
		phase: localAuthorityClaimAuditPreparations, identity: initialLocalAuthorityClaimAuditIdentity(),
	}
	if err := a.updateLocalAuthorityClaimAuditCursor(ctx, tx, reset); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("resetting local claim audit cursor: commit: %w", err)
	}
	return nil
}

func readClaimRecoveryCommand(
	ctx context.Context,
	repository *CommandRepository,
	state CommandRepositoryState,
	commandID string,
) (CommandIndexResolution, Command, [sha256.Size]byte, error) {
	resolution, err := repository.Get(ctx, commandID)
	if err != nil {
		return CommandIndexResolution{}, Command{}, [sha256.Size]byte{}, fmt.Errorf("auditing claim command %q: %w", commandID, err)
	}
	if resolution.Store != state.Store || resolution.Revision < state.Revision || resolution.SequenceHighWater < state.SequenceHighWater ||
		!resolution.Found || resolution.Entry.Command == nil {
		return CommandIndexResolution{}, Command{}, [sha256.Size]byte{}, fmt.Errorf("%w: claim command %q is unavailable, opaque, or from inconsistent repository authority", ErrLocalNudgeAuthorityConflict, commandID)
	}
	command := cloneCommandValue(*resolution.Entry.Command)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return CommandIndexResolution{}, Command{}, [sha256.Size]byte{}, fmt.Errorf("auditing claim command %q: %w", commandID, err)
	}
	return resolution, command, sha256.Sum256(wire), nil
}

func (a *LocalNudgeAuthority) readClaimPreparation(ctx context.Context, commandID string) (CommandClaimTransitionIntent, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return CommandClaimTransitionIntent{}, err
	}
	defer release()
	record, found, err := localAuthorityClaimPreparationByCommand(ctx, a.db, a.store, commandID)
	if err != nil {
		return CommandClaimTransitionIntent{}, err
	}
	if !found {
		return CommandClaimTransitionIntent{}, fmt.Errorf("%w: claim preparation %q disappeared", ErrLocalNudgeAuthorityConflict, commandID)
	}
	return record.intent, nil
}

func (a *LocalNudgeAuthority) readClaimReceipt(ctx context.Context, commandID string) (localAuthorityClaimTransitionRecord, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, err
	}
	defer release()
	record, found, err := localAuthorityClaimReceiptByCommand(ctx, a.db, a.store, commandID)
	if err != nil {
		return localAuthorityClaimTransitionRecord{}, err
	}
	if !found {
		return localAuthorityClaimTransitionRecord{}, fmt.Errorf("%w: claim receipt %q disappeared", ErrLocalNudgeAuthorityConflict, commandID)
	}
	return record, nil
}

func (a *LocalNudgeAuthority) readClaimEvidence(ctx context.Context, commandID string) (
	preparation localAuthorityClaimTransitionRecord,
	hasPreparation bool,
	receipt localAuthorityClaimTransitionRecord,
	hasReceipt bool,
	err error,
) {
	release, err := a.begin(ctx)
	if err != nil {
		return preparation, false, receipt, false, err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return preparation, false, receipt, false, fmt.Errorf("reading local claim evidence: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	preparation, hasPreparation, err = localAuthorityClaimPreparationByCommand(ctx, tx, a.store, commandID)
	if err != nil {
		return preparation, false, receipt, false, err
	}
	receipt, hasReceipt, err = localAuthorityClaimReceiptByCommand(ctx, tx, a.store, commandID)
	if err != nil {
		return preparation, false, receipt, false, err
	}
	if hasPreparation && hasReceipt {
		return preparation, false, receipt, false, fmt.Errorf("%w: command %q has both claim preparation and receipt", ErrLocalNudgeAuthorityConflict, commandID)
	}
	if err := tx.Commit(); err != nil {
		return preparation, false, receipt, false, fmt.Errorf("reading local claim evidence: commit: %w", err)
	}
	return preparation, hasPreparation, receipt, hasReceipt, nil
}

func (a *LocalNudgeAuthority) claimEvidenceHasExactPendingTerminal(
	ctx context.Context,
	claimIntent CommandClaimTransitionIntent,
	command Command,
	commandDigest [sha256.Size]byte,
) (bool, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	terminalIntent, found, err := localAuthorityPreparationByCommand(ctx, a.db, a.store, command.ID)
	if err != nil || !found {
		return false, err
	}
	if terminalIntent.Store != command.Store || terminalIntent.CommandID != command.ID || terminalIntent.Sequence != command.Order.Sequence ||
		terminalIntent.RepositoryRevision != command.Order.Revision || terminalIntent.CommandDigest != commandDigest {
		return false, nil
	}
	if claimIntent.CommandID != "" && (terminalIntent.Partition != claimIntent.Partition ||
		terminalIntent.BeforeCommandDigest != claimIntent.AfterCommandDigest || terminalIntent.RepositoryBeforeRevision < claimIntent.RepositoryRevision) {
		return false, nil
	}
	return true, nil
}

func (a *LocalNudgeAuthority) localAuthorityClaimEvidencePage(ctx context.Context, table, afterCommandID string, limit int) ([]string, bool, error) {
	if limit <= 0 || limit > localAuthorityRecoveryPageSize {
		return nil, false, fmt.Errorf("%w: claim evidence page limit %d is outside 1..%d", ErrLocalNudgeAuthorityConflict, limit, localAuthorityRecoveryPageSize)
	}
	query := ""
	switch table {
	case "claim_preparations":
		query = `SELECT command_id FROM claim_preparations WHERE command_id > ? ORDER BY command_id LIMIT ?`
	case "claim_receipts":
		query = `SELECT command_id FROM claim_receipts WHERE command_id > ? ORDER BY command_id LIMIT ?`
	default:
		return nil, false, fmt.Errorf("%w: unknown claim evidence table %q", ErrLocalNudgeAuthorityConflict, table)
	}
	release, err := a.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rows, err := a.db.QueryContext(ctx, query, afterCommandID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	commandIDs := make([]string, 0, limit+1)
	for rows.Next() {
		var commandID string
		if err := rows.Scan(&commandID); err != nil {
			return nil, false, err
		}
		commandIDs = append(commandIDs, commandID)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	more := len(commandIDs) > limit
	if more {
		commandIDs = commandIDs[:limit]
	}
	return commandIDs, more, nil
}

type localAuthorityActiveAdmission struct {
	commandID        string
	sequence         uint64
	partition        TrustedCityPartition
	decisionRevision uint64
}

func (a *LocalNudgeAuthority) localAuthorityActiveAdmissionPage(ctx context.Context, afterSequence uint64, limit int) ([]localAuthorityActiveAdmission, bool, error) {
	if limit <= 0 || limit > localAuthorityRecoveryPageSize {
		return nil, false, fmt.Errorf("%w: active admission page limit %d is outside 1..%d", ErrLocalNudgeAuthorityConflict, limit, localAuthorityRecoveryPageSize)
	}
	release, err := a.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rows, err := a.db.QueryContext(ctx, `SELECT command_id, sequence, partition_id, decision_revision
		FROM admission_decisions
		WHERE decision_kind = 'admitted' AND terminal_revision IS NULL AND sequence > ?
		ORDER BY sequence LIMIT ?`, encodeLocalAuthorityUint64(afterSequence), limit+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	admissions := make([]localAuthorityActiveAdmission, 0, limit+1)
	for rows.Next() {
		var admission localAuthorityActiveAdmission
		var sequenceWire, partitionWire, revisionWire []byte
		if err := rows.Scan(&admission.commandID, &sequenceWire, &partitionWire, &revisionWire); err != nil {
			return nil, false, err
		}
		admission.sequence, err = decodeLocalAuthorityUint64(sequenceWire)
		if err != nil {
			return nil, false, err
		}
		admission.decisionRevision, err = decodeLocalAuthorityUint64(revisionWire)
		if err != nil {
			return nil, false, err
		}
		if len(partitionWire) != sha256.Size {
			return nil, false, fmt.Errorf("%w: active admission %q has malformed partition", ErrLocalNudgeAuthorityConflict, admission.commandID)
		}
		copy(admission.partition.identity[:], partitionWire)
		admissions = append(admissions, admission)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	more := len(admissions) > limit
	if more {
		admissions = admissions[:limit]
	}
	return admissions, more, nil
}
