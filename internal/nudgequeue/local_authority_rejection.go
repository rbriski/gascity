package nudgequeue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type localAuthorityRejectionPreparation struct {
	intent CommandProvenanceRejectionIntent
}

type localAuthorityRejectionDecision struct {
	commandID          string
	sequence           uint64
	allocationRevision uint64
	decisionRevision   uint64
	originDigest       [sha256.Size]byte
	identityDigest     [sha256.Size]byte
	terminalDigest     [sha256.Size]byte
	reason             string
}

// PrepareCommandProvenanceRejection records stable exact before-state evidence
// without predicting a future repository revision. The single SQLite writer
// serializes admission and rejection preparations; whichever durable intent
// exists first blocks the competing family.
func (a *LocalNudgeAuthority) PrepareCommandProvenanceRejection(ctx context.Context, intent CommandProvenanceRejectionIntent) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateProvenanceRejectionIntent(intent); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("preparing local provenance rejection: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if decision, found, err := localAuthorityRejectionDecisionByCommand(ctx, tx, intent.CommandID); err != nil {
		return err
	} else if found {
		if rejectionDecisionMatchesIntent(decision, intent) {
			return nil
		}
		return fmt.Errorf("%w: command already has a different finalized rejection", ErrLocalNudgeAuthorityConflict)
	}
	if found, err := localAuthorityAnyDecisionExists(ctx, tx, intent.CommandID, intent.Sequence); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: command or sequence already has an authority decision", ErrLocalNudgeAuthorityConflict)
	}
	if prepared, err := localAuthorityAdmissionPreparationExists(ctx, tx, intent.CommandID); err != nil {
		return err
	} else if prepared {
		return fmt.Errorf("%w: command has a legitimate admission preparation", ErrLocalNudgeAuthorityConflict)
	}
	existing, found, err := localAuthorityRejectionPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if found {
		if existing.intent == intent {
			return nil
		}
		return fmt.Errorf("%w: command has a competing provenance rejection", ErrLocalNudgeAuthorityConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO rejection_preparations
		(sequence, command_id, allocation_revision, before_command_revision, identity_digest, before_digest, rejected_at, reason)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, encodeLocalAuthorityUint64(intent.Sequence), intent.CommandID,
		encodeLocalAuthorityUint64(intent.AllocationRevision), encodeLocalAuthorityUint64(intent.BeforeCommandRevision),
		intent.IdentityDigest[:], intent.BeforeCommandDigest[:], intent.RejectedAt.Format(time.RFC3339Nano), intent.Reason); err != nil {
		return fmt.Errorf("%w: inserting provenance rejection preparation: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("preparing local provenance rejection: commit: %w", err)
	}
	return nil
}

// VerifyCommandProvenanceRejectionPreparation proves the exact stable intent
// remains durable immediately before the command-store terminal transition.
func (a *LocalNudgeAuthority) VerifyCommandProvenanceRejectionPreparation(ctx context.Context, intent CommandProvenanceRejectionIntent) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateProvenanceRejectionIntent(intent); err != nil {
		return err
	}
	prepared, found, err := localAuthorityRejectionPreparationByCommand(ctx, a.db, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if !found || prepared.intent != intent {
		return fmt.Errorf("%w: exact provenance rejection preparation is missing", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

// RecordCommandProvenanceRejection consumes an exact preparation and publishes
// the partitionless terminal decision returned by the repository transaction.
func (a *LocalNudgeAuthority) RecordCommandProvenanceRejection(ctx context.Context, resolution CommandProvenanceRejectionResolution) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateProvenanceRejectionResolution(resolution); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording local provenance rejection: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if decision, found, err := localAuthorityRejectionDecisionByCommand(ctx, tx, resolution.Intent.CommandID); err != nil {
		return err
	} else if found {
		if rejectionDecisionMatchesResolution(decision, resolution) {
			return nil
		}
		return fmt.Errorf("%w: finalized provenance rejection differs", ErrLocalNudgeAuthorityConflict)
	}
	prepared, found, err := localAuthorityRejectionPreparationByCommand(ctx, tx, a.store, resolution.Intent.CommandID)
	if err != nil {
		return err
	}
	if !found || prepared.intent != resolution.Intent {
		return fmt.Errorf("%w: provenance rejection has no exact preparation", ErrLocalNudgeAuthorityConflict)
	}
	if found, err := localAuthorityAnyDecisionExists(ctx, tx, resolution.Intent.CommandID, resolution.Intent.Sequence); err != nil {
		return err
	} else if found {
		return fmt.Errorf("%w: command or sequence acquired a competing decision", ErrLocalNudgeAuthorityConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO admission_decisions
		(sequence, command_id, decision_kind, allocation_revision, decision_revision,
		 origin_digest, identity_digest, terminal_revision, terminal_digest, rejection_reason)
		VALUES (?, ?, 'rejected', ?, ?, ?, ?, ?, ?, ?)`, encodeLocalAuthorityUint64(resolution.Intent.Sequence),
		resolution.Intent.CommandID, encodeLocalAuthorityUint64(resolution.Intent.AllocationRevision),
		encodeLocalAuthorityUint64(resolution.RepositoryRevision), resolution.Intent.BeforeCommandDigest[:], resolution.Intent.IdentityDigest[:],
		encodeLocalAuthorityUint64(resolution.RepositoryRevision), resolution.CommandDigest[:], resolution.Intent.Reason); err != nil {
		return fmt.Errorf("%w: inserting finalized provenance rejection: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM rejection_preparations WHERE command_id = ?`, resolution.Intent.CommandID)
	if err != nil {
		return fmt.Errorf("recording local provenance rejection: %w", err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("%w: rejection preparation consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityDensePrefix(ctx, tx); err != nil {
		return err
	}
	if err := advanceLocalAuthorityObservedRepositoryState(ctx, tx, resolution.Intent.Sequence, resolution.RepositoryRevision); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording local provenance rejection: commit: %w", err)
	}
	return nil
}

// RepairCommandProvenanceRejections completes prepared rejections, then audits
// each undecided sequence through the repository's exact generated index. It
// returns only after the authority decision prefix equals the repository
// sequence high-water and no rejection preparation remains.
func (a *LocalNudgeAuthority) RepairCommandProvenanceRejections(ctx context.Context, repository *CommandRepository) error {
	if repository == nil {
		return fmt.Errorf("%w: command repository is required", ErrLocalNudgeAuthorityConflict)
	}
	observed, err := a.snapshotObservedRepositoryHighWater(ctx)
	if err != nil {
		return err
	}
	state, err := repository.State(ctx)
	if err != nil {
		state, err = repository.RepairLineage(ctx)
		if err != nil {
			return fmt.Errorf("repairing local provenance rejections: repairing repository lineage: %w", err)
		}
	}
	if err := a.validateRecoveryState(state, observed); err != nil {
		return err
	}
	ctx, budget := withCommandAuthorityRecoveryBudget(ctx)
	if err := a.repairProvenanceRejectionPreparations(ctx, repository); err != nil {
		return err
	}
	for {
		observed, err = a.snapshotObservedRepositoryHighWater(ctx)
		if err != nil {
			return err
		}
		state, err = repository.State(ctx)
		if err != nil {
			state, err = repository.RepairLineage(ctx)
			if err != nil {
				return fmt.Errorf("repairing local provenance rejections: refreshing repository lineage: %w", err)
			}
		}
		if err := a.validateRecoveryState(state, observed); err != nil {
			return err
		}
		dense, err := a.localAuthorityDenseDecisionHighWater(ctx)
		if err != nil {
			return err
		}
		if dense > state.SequenceHighWater {
			return fmt.Errorf("%w: authority decision prefix %d exceeds repository sequence %d", ErrLocalNudgeAuthorityConflict, dense, state.SequenceHighWater)
		}
		if dense == state.SequenceHighWater {
			if pending, err := a.localAuthorityRejectionPreparationCount(ctx); err != nil {
				return err
			} else if pending != 0 {
				return fmt.Errorf("%w: %d rejection preparations remain behind a complete decision prefix", ErrLocalNudgeAuthorityConflict, pending)
			}
			return nil
		}
		remaining := budget.remainingWork()
		if remaining == 0 {
			return budget.takeWork("advancing local provenance decision prefix")
		}
		limit := min(localAuthorityRecoveryPageSize, remaining)
		advanced, err := a.advanceDenseDecisionPrefixPage(ctx, limit)
		if err != nil {
			return err
		}
		if advanced > 0 {
			if err := budget.takeWorkUnits(advanced, "advancing local provenance decision prefix"); err != nil {
				return err
			}
			continue
		}
		next := dense + 1
		if err := budget.takeWork(fmt.Sprintf("repairing local provenance sequence %d", next)); err != nil {
			return err
		}
		candidate, err := repository.ResolveSequence(ctx, next)
		if err != nil {
			return fmt.Errorf("repairing local provenance sequence %d: %w", next, err)
		}
		if !candidate.Found || candidate.Entry.Command == nil {
			return fmt.Errorf("%w: repository sequence %d has no projectable known-version command", ErrLocalNudgeAuthorityConflict, next)
		}
		command := *candidate.Entry.Command
		exactGrant, err := a.prepareExactGrantAdmission(ctx, command)
		if err != nil {
			return fmt.Errorf("repairing local provenance sequence %d: checking exact grant: %w", next, err)
		}
		if exactGrant {
			if err := a.RepairCommandPartitionAdmissions(ctx, repository); err != nil {
				return fmt.Errorf("repairing local provenance sequence %d: publishing exact grant: %w", next, err)
			}
			continue
		}
		rejectedAt := a.now().UTC().Round(0)
		if rejectedAt.Before(command.CreatedAt) {
			rejectedAt = command.CreatedAt
		}
		if _, err := repository.RejectCommandProvenance(ctx, command.ID, next, rejectedAt, a); err != nil {
			return fmt.Errorf("repairing local provenance sequence %d: %w", next, err)
		}
	}
}

func (a *LocalNudgeAuthority) prepareExactGrantAdmission(ctx context.Context, command Command) (bool, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return false, err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("preparing exact-grant admission recovery: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	grant, found, err := localNudgeGrantByCommandID(ctx, tx, command.ID)
	if err != nil || !found {
		return false, err
	}
	if err := a.validatePersistedGrant(grant); err != nil {
		return false, err
	}
	if !localAuthorityCommandMatchesGrant(command, a.store, grant) {
		return false, nil
	}
	if decided, err := localAuthorityAnyDecisionExists(ctx, tx, command.ID, command.Order.Sequence); err != nil {
		return false, err
	} else if decided {
		return false, fmt.Errorf("%w: exact-grant command or sequence already has an authority decision", ErrLocalNudgeAuthorityConflict)
	}
	if rejecting, err := localAuthorityRejectionPreparationExists(ctx, tx, command.ID); err != nil {
		return false, err
	} else if rejecting {
		return false, fmt.Errorf("%w: exact-grant command has a provenance rejection preparation", ErrLocalNudgeAuthorityConflict)
	}
	prepared, err := localAuthorityAdmissionPreparationExists(ctx, tx, command.ID)
	if err != nil {
		return false, err
	}
	if !prepared {
		if _, err := tx.ExecContext(ctx, `INSERT INTO admission_preparations (command_id) VALUES (?)`, command.ID); err != nil {
			return false, fmt.Errorf("preparing exact-grant admission recovery: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("preparing exact-grant admission recovery: commit: %w", err)
	}
	return true, nil
}

func (a *LocalNudgeAuthority) repairProvenanceRejectionPreparations(ctx context.Context, repository *CommandRepository) error {
	ctx, budget := withCommandAuthorityRecoveryBudget(ctx)
	for {
		commandIDs, more, err := a.localAuthorityPreparationPage(ctx, `SELECT command_id FROM rejection_preparations ORDER BY command_id LIMIT ?`)
		if err != nil {
			return fmt.Errorf("repairing local provenance preparations: %w", err)
		}
		if len(commandIDs) == 0 {
			return nil
		}
		for _, commandID := range commandIDs {
			if err := budget.takeWork(fmt.Sprintf("repairing local provenance preparation %q", commandID)); err != nil {
				return err
			}
			release, err := a.begin(ctx)
			if err != nil {
				return err
			}
			prepared, found, prepErr := localAuthorityRejectionPreparationByCommand(ctx, a.db, a.store, commandID)
			release()
			if prepErr != nil || !found {
				return fmt.Errorf("%w: rejection preparation %q disappeared: %w", ErrLocalNudgeAuthorityConflict, commandID, prepErr)
			}
			resolution, err := repository.Get(ctx, commandID)
			if err != nil || !resolution.Found || resolution.Entry.Command == nil {
				return fmt.Errorf("%w: prepared rejection command %q is unavailable or opaque: %w", ErrLocalNudgeAuthorityConflict, commandID, err)
			}
			command := *resolution.Entry.Command
			identity := commandProvenanceIdentityDigest(command.Store, command.ID, command.Order.Sequence, prepared.intent.AllocationRevision)
			if identity != prepared.intent.IdentityDigest || command.Store != prepared.intent.Store || command.ID != prepared.intent.CommandID ||
				command.Order.Sequence != prepared.intent.Sequence {
				return fmt.Errorf("%w: prepared rejection identity differs", ErrLocalNudgeAuthorityConflict)
			}
			wire, err := EncodeCommandV1(command)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(wire)
			if command.Terminal != nil && command.State == CommandStateDeadLettered &&
				command.Terminal.ActionResult == CommandActionResultUnauthorizedProvenance &&
				command.Terminal.ErrorClass == CommandErrorClassUnauthorizedProvenance &&
				command.Terminal.ProviderStage == ProviderStageNotEntered && command.Terminal.Completion == CompletionStateNotCompleted &&
				command.Terminal.At.Equal(prepared.intent.RejectedAt) {
				if err := a.RecordCommandProvenanceRejection(ctx, CommandProvenanceRejectionResolution{
					Intent: prepared.intent, RepositoryRevision: command.Order.Revision, CommandDigest: digest,
				}); err != nil {
					return err
				}
				continue
			}
			if command.Terminal == nil && !commandIsTerminalState(command.State) && command.Order.Revision == prepared.intent.BeforeCommandRevision && digest == prepared.intent.BeforeCommandDigest {
				if _, err := repository.RejectCommandProvenance(ctx, command.ID, command.Order.Sequence, prepared.intent.RejectedAt, a); err != nil {
					return err
				}
				continue
			}
			return fmt.Errorf("%w: prepared rejection command differs from before and terminal states", ErrLocalNudgeAuthorityConflict)
		}
		if !more {
			return nil
		}
	}
}

func (a *LocalNudgeAuthority) validateProvenanceRejectionIntent(intent CommandProvenanceRejectionIntent) error {
	if intent.Store != a.store || validateCommandIdentity("rejected command id", intent.CommandID) != nil || intent.Sequence == 0 ||
		intent.AllocationRevision == 0 || intent.BeforeCommandRevision < intent.AllocationRevision ||
		intent.IdentityDigest == ([sha256.Size]byte{}) || intent.BeforeCommandDigest == ([sha256.Size]byte{}) ||
		intent.Reason != CommandProvenanceRejectionReasonUnauthorized {
		return fmt.Errorf("%w: invalid provenance rejection intent", ErrLocalNudgeAuthorityConflict)
	}
	if err := validateCommandTime("provenance rejected_at", intent.RejectedAt); err != nil {
		return fmt.Errorf("%w: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if commandProvenanceIdentityDigest(intent.Store, intent.CommandID, intent.Sequence, intent.AllocationRevision) != intent.IdentityDigest {
		return fmt.Errorf("%w: rejection identity digest differs", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func (a *LocalNudgeAuthority) validateProvenanceRejectionResolution(resolution CommandProvenanceRejectionResolution) error {
	if err := a.validateProvenanceRejectionIntent(resolution.Intent); err != nil {
		return err
	}
	if resolution.RepositoryRevision <= resolution.Intent.BeforeCommandRevision || resolution.CommandDigest == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: invalid provenance rejection resolution", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func localAuthorityRejectionPreparationByCommand(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (localAuthorityRejectionPreparation, bool, error) {
	var sequenceWire, allocationWire, beforeRevisionWire, identityDigest, beforeDigest []byte
	var rejectedAt, reason string
	err := queryer.QueryRowContext(ctx, `SELECT sequence, allocation_revision, before_command_revision, identity_digest, before_digest, rejected_at, reason
		FROM rejection_preparations WHERE command_id = ?`, commandID).Scan(&sequenceWire, &allocationWire, &beforeRevisionWire, &identityDigest, &beforeDigest, &rejectedAt, &reason)
	if errors.Is(err, sql.ErrNoRows) {
		return localAuthorityRejectionPreparation{}, false, nil
	}
	if err != nil {
		return localAuthorityRejectionPreparation{}, false, fmt.Errorf("reading local provenance rejection preparation: %w", err)
	}
	sequence, err := decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return localAuthorityRejectionPreparation{}, false, err
	}
	allocation, err := decodeLocalAuthorityUint64(allocationWire)
	if err != nil {
		return localAuthorityRejectionPreparation{}, false, err
	}
	beforeRevision, err := decodeLocalAuthorityUint64(beforeRevisionWire)
	if err != nil || len(identityDigest) != sha256.Size || len(beforeDigest) != sha256.Size {
		return localAuthorityRejectionPreparation{}, false, fmt.Errorf("%w: malformed provenance rejection preparation", ErrLocalNudgeAuthorityConflict)
	}
	at, err := time.Parse(time.RFC3339Nano, rejectedAt)
	if err != nil {
		return localAuthorityRejectionPreparation{}, false, fmt.Errorf("%w: malformed provenance rejection timestamp", ErrLocalNudgeAuthorityConflict)
	}
	intent := CommandProvenanceRejectionIntent{
		Store: store, CommandID: commandID, Sequence: sequence, AllocationRevision: allocation, BeforeCommandRevision: beforeRevision,
		Reason: reason, RejectedAt: at.UTC(),
	}
	copy(intent.IdentityDigest[:], identityDigest)
	copy(intent.BeforeCommandDigest[:], beforeDigest)
	return localAuthorityRejectionPreparation{intent: intent}, true, nil
}

func localAuthorityRejectionDecisionByCommand(ctx context.Context, queryer localAuthorityQueryer, commandID string) (localAuthorityRejectionDecision, bool, error) {
	var sequenceWire, allocationWire, decisionWire, originDigest, identityDigest, terminalDigest []byte
	var result localAuthorityRejectionDecision
	err := queryer.QueryRowContext(ctx, `SELECT sequence, command_id, allocation_revision, decision_revision, origin_digest,
		identity_digest, terminal_digest, rejection_reason FROM admission_decisions
		WHERE command_id = ? AND decision_kind = 'rejected'`, commandID).Scan(&sequenceWire, &result.commandID, &allocationWire,
		&decisionWire, &originDigest, &identityDigest, &terminalDigest, &result.reason)
	if errors.Is(err, sql.ErrNoRows) {
		return localAuthorityRejectionDecision{}, false, nil
	}
	if err != nil {
		return localAuthorityRejectionDecision{}, false, fmt.Errorf("reading local provenance rejection decision: %w", err)
	}
	result.sequence, err = decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return localAuthorityRejectionDecision{}, false, err
	}
	result.allocationRevision, err = decodeLocalAuthorityUint64(allocationWire)
	if err != nil {
		return localAuthorityRejectionDecision{}, false, err
	}
	result.decisionRevision, err = decodeLocalAuthorityUint64(decisionWire)
	if err != nil || len(originDigest) != sha256.Size || len(identityDigest) != sha256.Size || len(terminalDigest) != sha256.Size {
		return localAuthorityRejectionDecision{}, false, fmt.Errorf("%w: malformed provenance rejection decision", ErrLocalNudgeAuthorityConflict)
	}
	copy(result.originDigest[:], originDigest)
	copy(result.identityDigest[:], identityDigest)
	copy(result.terminalDigest[:], terminalDigest)
	return result, true, nil
}

func localAuthorityAnyDecisionExists(ctx context.Context, queryer localAuthorityQueryer, commandID string, sequence uint64) (bool, error) {
	var count int
	if err := queryer.QueryRowContext(ctx, `SELECT COUNT(*) FROM admission_decisions WHERE command_id = ? OR sequence = ?`,
		commandID, encodeLocalAuthorityUint64(sequence)).Scan(&count); err != nil {
		return false, fmt.Errorf("reading local admission decision collision: %w", err)
	}
	return count != 0, nil
}

func localAuthorityRejectionPreparationExists(ctx context.Context, queryer localAuthorityQueryer, commandID string) (bool, error) {
	var found int
	err := queryer.QueryRowContext(ctx, `SELECT 1 FROM rejection_preparations WHERE command_id = ?`, commandID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reading local provenance rejection preparation: %w", err)
	}
	return found == 1, nil
}

func (a *LocalNudgeAuthority) localAuthorityDenseDecisionHighWater(ctx context.Context) (uint64, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	var wire []byte
	if err := a.db.QueryRowContext(ctx, `SELECT dense_decision_high_water FROM authority_meta WHERE singleton = 1`).Scan(&wire); err != nil {
		return 0, fmt.Errorf("reading local decision prefix: %w", err)
	}
	return decodeLocalAuthorityUint64(wire)
}

func (a *LocalNudgeAuthority) localAuthorityRejectionPreparationCount(ctx context.Context) (int, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return 0, err
	}
	defer release()
	var count int
	if err := a.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM rejection_preparations`).Scan(&count); err != nil {
		return 0, fmt.Errorf("counting local provenance rejection preparations: %w", err)
	}
	return count, nil
}

func rejectionDecisionMatchesIntent(decision localAuthorityRejectionDecision, intent CommandProvenanceRejectionIntent) bool {
	return decision.commandID == intent.CommandID && decision.sequence == intent.Sequence &&
		decision.allocationRevision == intent.AllocationRevision && decision.originDigest == intent.BeforeCommandDigest &&
		decision.identityDigest == intent.IdentityDigest && decision.reason == intent.Reason
}

func rejectionDecisionMatchesResolution(decision localAuthorityRejectionDecision, resolution CommandProvenanceRejectionResolution) bool {
	return rejectionDecisionMatchesIntent(decision, resolution.Intent) && decision.decisionRevision == resolution.RepositoryRevision &&
		decision.terminalDigest == resolution.CommandDigest
}

var _ TrustedCommandProvenanceRejectionAuthority = (*LocalNudgeAuthority)(nil)
