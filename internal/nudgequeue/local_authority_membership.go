package nudgequeue

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
)

const (
	localAuthorityRecoveryPageSize    = 256
	localAuthorityActiveCoverageQuery = `SELECT command_id, sequence FROM memberships
		WHERE partition_id = ? AND admission_revision <= ? AND sequence <= ? AND terminal_revision IS NULL
		ORDER BY sequence LIMIT ?`
	localAuthorityHistoricalCoverageQuery = `SELECT command_id, sequence FROM memberships
		WHERE partition_id = ? AND terminal_revision > ? AND admission_revision <= ? AND sequence <= ?
		ORDER BY terminal_revision, sequence LIMIT ?`
)

// RecordCommandPartitionAdmission durably publishes one exact command
// membership and advances the amortized dense admission prefix.
func (a *LocalNudgeAuthority) RecordCommandPartitionAdmission(ctx context.Context, admission CommandPartitionAdmission) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateAdmission(admission); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording local command admission: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	grant, found, err := localNudgeGrantByCommandID(ctx, tx, admission.CommandID)
	if err != nil {
		return err
	}
	if !found || trustedCityPartitionFromAuthority(grant.reference) != admission.Partition {
		return fmt.Errorf("%w: admission has no exact authority grant", ErrLocalNudgeAuthorityConflict)
	}
	if err := a.validatePersistedGrant(grant); err != nil {
		return err
	}
	existing, found, err := localAuthorityMembershipByCommand(ctx, tx, admission.CommandID)
	if err != nil {
		return err
	}
	prepared, err := localAuthorityAdmissionPreparationExists(ctx, tx, admission.CommandID)
	if err != nil {
		return err
	}
	if found {
		if existing.sequence != admission.Sequence || existing.admissionRevision != admission.RepositoryRevision || existing.partition != admission.Partition {
			return fmt.Errorf("%w: conflicting admission membership", ErrLocalNudgeAuthorityConflict)
		}
		if prepared {
			return fmt.Errorf("%w: admitted command retains an admission preparation", ErrLocalNudgeAuthorityConflict)
		}
		return nil
	}
	if !prepared {
		return fmt.Errorf("%w: admission has no write-ahead preparation", ErrLocalNudgeAuthorityConflict)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (command_id, sequence, admission_revision, partition_id) VALUES (?, ?, ?, ?)`,
		admission.CommandID, encodeLocalAuthorityUint64(admission.Sequence), encodeLocalAuthorityUint64(admission.RepositoryRevision), admission.Partition.identity[:]); err != nil {
		return fmt.Errorf("%w: inserting command admission: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM admission_preparations WHERE command_id = ?`, admission.CommandID)
	if err != nil {
		return fmt.Errorf("%w: consuming command admission preparation: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("%w: command admission preparation consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := advanceLocalAuthorityDensePrefix(ctx, tx); err != nil {
		return err
	}
	if err := advanceLocalAuthorityObservedRepositoryState(ctx, tx, admission.Sequence, admission.RepositoryRevision); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording local command admission: commit: %w", err)
	}
	return nil
}

func (a *LocalNudgeAuthority) validateAdmission(admission CommandPartitionAdmission) error {
	if admission.Store != a.store || !admission.Partition.valid() || admission.RepositoryRevision == 0 || admission.Sequence == 0 ||
		validateCommandIdentity("admission command id", admission.CommandID) != nil {
		return fmt.Errorf("%w: invalid command admission", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

func advanceLocalAuthorityDensePrefix(ctx context.Context, tx *sql.Tx) error {
	var wire []byte
	if err := tx.QueryRowContext(ctx, `SELECT dense_admission_high_water FROM authority_meta WHERE singleton = 1`).Scan(&wire); err != nil {
		return fmt.Errorf("advancing local admission prefix: %w", err)
	}
	dense, err := decodeLocalAuthorityUint64(wire)
	if err != nil {
		return err
	}
	if dense != math.MaxUint64 {
		rows, err := tx.QueryContext(ctx, `SELECT sequence FROM memberships WHERE sequence > ? ORDER BY sequence`, encodeLocalAuthorityUint64(dense))
		if err != nil {
			return fmt.Errorf("advancing local admission prefix: %w", err)
		}
		for rows.Next() {
			var sequenceWire []byte
			if err := rows.Scan(&sequenceWire); err != nil {
				_ = rows.Close()
				return fmt.Errorf("advancing local admission prefix: %w", err)
			}
			sequence, err := decodeLocalAuthorityUint64(sequenceWire)
			if err != nil {
				_ = rows.Close()
				return err
			}
			if sequence != dense+1 {
				break
			}
			dense = sequence
			if dense == math.MaxUint64 {
				break
			}
		}
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr != nil || closeErr != nil {
			return fmt.Errorf("advancing local admission prefix: %w", errors.Join(rowsErr, closeErr))
		}
	}
	if _, err := tx.ExecContext(ctx, `UPDATE authority_meta SET dense_admission_high_water = ? WHERE singleton = 1`, encodeLocalAuthorityUint64(dense)); err != nil {
		return fmt.Errorf("advancing local admission prefix: %w", err)
	}
	return nil
}

func advanceLocalAuthorityObservedRepositoryState(ctx context.Context, tx *sql.Tx, sequence, revision uint64) error {
	var sequenceWire, revisionWire []byte
	if err := tx.QueryRowContext(ctx, `SELECT highest_observed_sequence, highest_observed_revision FROM authority_meta WHERE singleton = 1`).
		Scan(&sequenceWire, &revisionWire); err != nil {
		return fmt.Errorf("advancing observed repository authority: %w", err)
	}
	highestSequence, err := decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return err
	}
	highestRevision, err := decodeLocalAuthorityUint64(revisionWire)
	if err != nil {
		return err
	}
	if sequence > highestSequence {
		highestSequence = sequence
	}
	if revision > highestRevision {
		highestRevision = revision
	}
	if _, err := tx.ExecContext(ctx, `UPDATE authority_meta SET highest_observed_sequence = ?, highest_observed_revision = ? WHERE singleton = 1`,
		encodeLocalAuthorityUint64(highestSequence), encodeLocalAuthorityUint64(highestRevision)); err != nil {
		return fmt.Errorf("advancing observed repository authority: %w", err)
	}
	return nil
}

// ResolveCommandPartitionCoverage returns the complete historical active set
// for one exact repository snapshot after proving admissions are dense through
// its sequence high-water.
func (a *LocalNudgeAuthority) ResolveCommandPartitionCoverage(ctx context.Context, request CommandPartitionCoverageRequest) (CommandPartitionCoverage, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return CommandPartitionCoverage{}, err
	}
	defer release()
	if request.MaxCommands <= 0 || request.MaxCommands > MaxCommandRepositorySnapshotCommands {
		return CommandPartitionCoverage{}, fmt.Errorf("coverage command bound %d is outside 1..%d: %w", request.MaxCommands, MaxCommandRepositorySnapshotCommands, ErrCommandRepositorySnapshotLimit)
	}
	if request.Store != a.store || !request.Partition.valid() {
		return CommandPartitionCoverage{}, fmt.Errorf("%w: invalid coverage request", ErrLocalNudgeAuthorityConflict)
	}
	tx, err := a.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CommandPartitionCoverage{}, fmt.Errorf("resolving local partition coverage: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var denseWire []byte
	if err := tx.QueryRowContext(ctx, `SELECT dense_admission_high_water FROM authority_meta WHERE singleton = 1`).Scan(&denseWire); err != nil {
		return CommandPartitionCoverage{}, fmt.Errorf("resolving local partition coverage: %w", err)
	}
	dense, err := decodeLocalAuthorityUint64(denseWire)
	if err != nil {
		return CommandPartitionCoverage{}, err
	}
	if dense < request.SequenceHighWater {
		return CommandPartitionCoverage{}, fmt.Errorf("%w: authority admission prefix %d is behind repository sequence %d", ErrLocalNudgeAuthorityConflict, dense, request.SequenceHighWater)
	}
	overflowBound := request.MaxCommands + 1
	active, err := queryLocalAuthorityCoverageEntries(ctx, tx, localAuthorityActiveCoverageQuery, request.Partition.identity[:], encodeLocalAuthorityUint64(request.RepositoryRevision),
		encodeLocalAuthorityUint64(request.SequenceHighWater), overflowBound)
	if err != nil {
		return CommandPartitionCoverage{}, err
	}
	if len(active) > request.MaxCommands {
		return CommandPartitionCoverage{}, fmt.Errorf("trusted partition contains more than %d active commands: %w", request.MaxCommands, ErrCommandRepositorySnapshotLimit)
	}
	historical, err := queryLocalAuthorityCoverageEntries(ctx, tx, localAuthorityHistoricalCoverageQuery, request.Partition.identity[:], encodeLocalAuthorityUint64(request.RepositoryRevision),
		encodeLocalAuthorityUint64(request.RepositoryRevision), encodeLocalAuthorityUint64(request.SequenceHighWater), overflowBound-len(active))
	if err != nil {
		return CommandPartitionCoverage{}, err
	}
	active = append(active, historical...)
	if len(active) > request.MaxCommands {
		return CommandPartitionCoverage{}, fmt.Errorf("trusted partition contains more than %d active commands: %w", request.MaxCommands, ErrCommandRepositorySnapshotLimit)
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Sequence < active[j].Sequence })
	if err := tx.Commit(); err != nil {
		return CommandPartitionCoverage{}, fmt.Errorf("resolving local partition coverage: commit read snapshot: %w", err)
	}
	return CommandPartitionCoverage{
		Store: request.Store, RepositoryRevision: request.RepositoryRevision, SequenceHighWater: request.SequenceHighWater,
		AdmittedCount: request.SequenceHighWater, Partition: request.Partition, ActiveEntries: active,
	}, nil
}

func queryLocalAuthorityCoverageEntries(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]CommandPartitionCoverageEntry, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("resolving local partition coverage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var entries []CommandPartitionCoverageEntry
	for rows.Next() {
		var commandID string
		var sequenceWire []byte
		if err := rows.Scan(&commandID, &sequenceWire); err != nil {
			return nil, fmt.Errorf("resolving local partition coverage: %w", err)
		}
		sequence, err := decodeLocalAuthorityUint64(sequenceWire)
		if err != nil {
			return nil, err
		}
		entries = append(entries, CommandPartitionCoverageEntry{CommandID: commandID, Sequence: sequence})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("resolving local partition coverage: %w", err)
	}
	if err := rows.Close(); err != nil {
		return nil, fmt.Errorf("resolving local partition coverage: close rows: %w", err)
	}
	return entries, nil
}

// ResolveCommandPartitionMembership returns exact historical membership for a
// command at one repository revision.
func (a *LocalNudgeAuthority) ResolveCommandPartitionMembership(ctx context.Context, request CommandPartitionMembershipRequest) (CommandPartitionMembership, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return CommandPartitionMembership{}, err
	}
	defer release()
	result := CommandPartitionMembership{Store: request.Store, RepositoryRevision: request.RepositoryRevision, CommandID: request.CommandID, Partition: request.Partition}
	if request.Store != a.store || !request.Partition.valid() || validateCommandIdentity("membership command id", request.CommandID) != nil {
		return CommandPartitionMembership{}, fmt.Errorf("%w: invalid membership request", ErrLocalNudgeAuthorityConflict)
	}
	membership, found, err := localAuthorityMembershipByCommand(ctx, a.db, request.CommandID)
	if err != nil {
		return CommandPartitionMembership{}, err
	}
	if !found || membership.partition != request.Partition || membership.admissionRevision > request.RepositoryRevision {
		return result, nil
	}
	result.Found = true
	result.Sequence = membership.sequence
	result.Active = membership.terminalRevision == nil || *membership.terminalRevision > request.RepositoryRevision
	return result, nil
}

// PrepareCommandPartitionTerminal persists the one exact before/after intent
// before the command-store transaction may terminalize a row.
func (a *LocalNudgeAuthority) PrepareCommandPartitionTerminal(ctx context.Context, intent CommandPartitionTerminalIntent) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateTerminalIntent(intent); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("preparing local terminal intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, intent.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != intent.Sequence || membership.partition != intent.Partition || membership.terminalRevision != nil ||
		membership.admissionRevision > intent.RepositoryBeforeRevision {
		return fmt.Errorf("%w: terminal intent has no matching active admission", ErrLocalNudgeAuthorityConflict)
	}
	existing, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if found {
		if existing != intent {
			return fmt.Errorf("%w: competing terminal intent for command %q", ErrLocalNudgeAuthorityConflict, intent.CommandID)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO terminal_preparations
		(command_id, repository_before_revision, before_digest, terminal_revision, terminal_digest) VALUES (?, ?, ?, ?, ?)`,
		intent.CommandID, encodeLocalAuthorityUint64(intent.RepositoryBeforeRevision), intent.BeforeCommandDigest[:],
		encodeLocalAuthorityUint64(intent.RepositoryRevision), intent.CommandDigest[:]); err != nil {
		return fmt.Errorf("%w: inserting terminal intent: %w", ErrLocalNudgeAuthorityConflict, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("preparing local terminal intent: commit: %w", err)
	}
	return nil
}

func (a *LocalNudgeAuthority) validateTerminalIntent(intent CommandPartitionTerminalIntent) error {
	if intent.Store != a.store || !intent.Partition.valid() || intent.CommandID == "" || intent.Sequence == 0 ||
		intent.RepositoryBeforeRevision == 0 || intent.RepositoryBeforeRevision == math.MaxUint64 ||
		intent.RepositoryRevision != intent.RepositoryBeforeRevision+1 ||
		intent.BeforeCommandDigest == ([sha256.Size]byte{}) || intent.CommandDigest == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: invalid terminal intent", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

// AbortCommandPartitionTerminal removes only an exact unresolved preparation.
// An already-finalized terminal cannot be aborted.
func (a *LocalNudgeAuthority) AbortCommandPartitionTerminal(ctx context.Context, intent CommandPartitionTerminalIntent) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if err := a.validateTerminalIntent(intent); err != nil {
		return err
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("aborting local terminal intent: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	preparation, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, intent.CommandID)
	if err != nil {
		return err
	}
	if found {
		if preparation != intent {
			return fmt.Errorf("%w: refusing to abort a different terminal intent", ErrLocalNudgeAuthorityConflict)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM terminal_preparations WHERE command_id = ?`, intent.CommandID); err != nil {
			return fmt.Errorf("aborting local terminal intent: %w", err)
		}
	} else {
		membership, exists, err := localAuthorityMembershipByCommand(ctx, tx, intent.CommandID)
		if err != nil {
			return err
		}
		if !exists || membership.terminalRevision != nil {
			return fmt.Errorf("%w: terminal intent is missing or already finalized", ErrLocalNudgeAuthorityConflict)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("aborting local terminal intent: commit: %w", err)
	}
	return nil
}

// VerifyCommandPartitionTerminal accepts only an exact unresolved preparation
// or the exact revision+digest retained by finalized membership.
func (a *LocalNudgeAuthority) VerifyCommandPartitionTerminal(ctx context.Context, resolution CommandPartitionTerminalResolution) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if resolution.Store != a.store || !resolution.Partition.valid() || resolution.CommandID == "" || resolution.Sequence == 0 ||
		resolution.RepositoryRevision == 0 || resolution.CommandDigest == ([sha256.Size]byte{}) {
		return fmt.Errorf("%w: invalid terminal resolution", ErrLocalNudgeAuthorityConflict)
	}
	membership, found, err := localAuthorityMembershipByCommand(ctx, a.db, resolution.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != resolution.Sequence || membership.partition != resolution.Partition {
		return fmt.Errorf("%w: terminal membership is missing", ErrLocalNudgeAuthorityConflict)
	}
	if membership.terminalRevision != nil {
		if *membership.terminalRevision == resolution.RepositoryRevision && membership.terminalDigest == resolution.CommandDigest {
			return nil
		}
		return fmt.Errorf("%w: finalized terminal digest differs", ErrLocalNudgeAuthorityConflict)
	}
	preparation, found, err := localAuthorityPreparationByCommand(ctx, a.db, a.store, resolution.CommandID)
	if err != nil {
		return err
	}
	if !found || preparation.RepositoryRevision != resolution.RepositoryRevision || preparation.CommandDigest != resolution.CommandDigest ||
		preparation.Sequence != resolution.Sequence || preparation.Partition != resolution.Partition || preparation.Store != resolution.Store {
		return fmt.Errorf("%w: exact terminal preparation is missing", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

// RecordCommandPartitionTerminal atomically consumes an exact preparation and
// retains its terminal revision+digest in historical membership.
func (a *LocalNudgeAuthority) RecordCommandPartitionTerminal(ctx context.Context, terminal CommandPartitionTerminal) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	if terminal.Store != a.store || !terminal.Partition.valid() || terminal.CommandID == "" || terminal.Sequence == 0 || terminal.RepositoryRevision == 0 {
		return fmt.Errorf("%w: invalid terminal membership", ErrLocalNudgeAuthorityConflict)
	}
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("recording local terminal membership: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	membership, found, err := localAuthorityMembershipByCommand(ctx, tx, terminal.CommandID)
	if err != nil {
		return err
	}
	if !found || membership.sequence != terminal.Sequence || membership.partition != terminal.Partition {
		return fmt.Errorf("%w: terminal membership has no matching admission", ErrLocalNudgeAuthorityConflict)
	}
	if membership.terminalRevision != nil {
		if *membership.terminalRevision != terminal.RepositoryRevision {
			return fmt.Errorf("%w: conflicting finalized terminal revision", ErrLocalNudgeAuthorityConflict)
		}
		return nil
	}
	preparation, found, err := localAuthorityPreparationByCommand(ctx, tx, a.store, terminal.CommandID)
	if err != nil {
		return err
	}
	if !found || preparation.Store != terminal.Store || preparation.RepositoryRevision != terminal.RepositoryRevision ||
		preparation.Sequence != terminal.Sequence || preparation.Partition != terminal.Partition {
		return fmt.Errorf("%w: terminal has no exact write-ahead preparation", ErrLocalNudgeAuthorityConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE memberships SET terminal_revision = ?, terminal_digest = ? WHERE command_id = ?`,
		encodeLocalAuthorityUint64(terminal.RepositoryRevision), preparation.CommandDigest[:], terminal.CommandID); err != nil {
		return fmt.Errorf("recording local terminal membership: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM terminal_preparations WHERE command_id = ?`, terminal.CommandID); err != nil {
		return fmt.Errorf("recording local terminal membership: %w", err)
	}
	if err := advanceLocalAuthorityObservedRepositoryState(ctx, tx, terminal.Sequence, terminal.RepositoryRevision); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("recording local terminal membership: commit: %w", err)
	}
	return nil
}

type localAuthorityMembership struct {
	commandID         string
	sequence          uint64
	admissionRevision uint64
	partition         TrustedCityPartition
	terminalRevision  *uint64
	terminalDigest    [sha256.Size]byte
}

type localAuthorityQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func localAuthorityMembershipByCommand(ctx context.Context, queryer localAuthorityQueryer, commandID string) (localAuthorityMembership, bool, error) {
	var sequenceWire, admissionWire, partitionWire []byte
	var terminalWire, terminalDigest []byte
	result := localAuthorityMembership{commandID: commandID}
	err := queryer.QueryRowContext(ctx, `SELECT sequence, admission_revision, partition_id, terminal_revision, terminal_digest FROM memberships WHERE command_id = ?`, commandID).
		Scan(&sequenceWire, &admissionWire, &partitionWire, &terminalWire, &terminalDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return localAuthorityMembership{}, false, nil
	}
	if err != nil {
		return localAuthorityMembership{}, false, fmt.Errorf("reading local command membership: %w", err)
	}
	sequence, err := decodeLocalAuthorityUint64(sequenceWire)
	if err != nil {
		return localAuthorityMembership{}, false, err
	}
	admission, err := decodeLocalAuthorityUint64(admissionWire)
	if err != nil {
		return localAuthorityMembership{}, false, err
	}
	if len(partitionWire) != sha256.Size {
		return localAuthorityMembership{}, false, fmt.Errorf("%w: invalid membership partition length", ErrLocalNudgeAuthorityConflict)
	}
	result.sequence = sequence
	result.admissionRevision = admission
	copy(result.partition.identity[:], partitionWire)
	if terminalWire != nil {
		terminal, err := decodeLocalAuthorityUint64(terminalWire)
		if err != nil || len(terminalDigest) != sha256.Size {
			return localAuthorityMembership{}, false, fmt.Errorf("%w: malformed finalized terminal membership", ErrLocalNudgeAuthorityConflict)
		}
		result.terminalRevision = &terminal
		copy(result.terminalDigest[:], terminalDigest)
	} else if terminalDigest != nil {
		return localAuthorityMembership{}, false, fmt.Errorf("%w: partial terminal membership", ErrLocalNudgeAuthorityConflict)
	}
	return result, true, nil
}

func localNudgeGrantByCommandID(ctx context.Context, queryer localAuthorityQueryer, commandID string) (localNudgeGrant, bool, error) {
	return scanLocalNudgeGrant(queryer.QueryRowContext(ctx, `SELECT reference_id, request_fingerprint, command_id, principal_schema, issuer,
		principal_id, tenant_scope, city_scope, credential_class, policy_version, policy_decision_id,
		action, target_session_id, payload_digest, command_created_at, issued_at, expires_at FROM ingress_grants WHERE command_id = ?`, commandID))
}

func localAuthorityPreparationByCommand(ctx context.Context, queryer localAuthorityQueryer, store CommandStoreBinding, commandID string) (CommandPartitionTerminalIntent, bool, error) {
	var beforeRevisionWire, beforeDigest, terminalRevisionWire, terminalDigest []byte
	err := queryer.QueryRowContext(ctx, `SELECT repository_before_revision, before_digest, terminal_revision, terminal_digest FROM terminal_preparations WHERE command_id = ?`, commandID).
		Scan(&beforeRevisionWire, &beforeDigest, &terminalRevisionWire, &terminalDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return CommandPartitionTerminalIntent{}, false, nil
	}
	if err != nil {
		return CommandPartitionTerminalIntent{}, false, fmt.Errorf("reading local terminal preparation: %w", err)
	}
	membership, found, err := localAuthorityMembershipByCommand(ctx, queryer, commandID)
	if err != nil || !found {
		return CommandPartitionTerminalIntent{}, false, fmt.Errorf("%w: preparation has no membership", ErrLocalNudgeAuthorityConflict)
	}
	beforeRevision, err := decodeLocalAuthorityUint64(beforeRevisionWire)
	if err != nil {
		return CommandPartitionTerminalIntent{}, false, err
	}
	terminalRevision, err := decodeLocalAuthorityUint64(terminalRevisionWire)
	if err != nil || len(beforeDigest) != sha256.Size || len(terminalDigest) != sha256.Size {
		return CommandPartitionTerminalIntent{}, false, fmt.Errorf("%w: malformed terminal preparation", ErrLocalNudgeAuthorityConflict)
	}
	intent := CommandPartitionTerminalIntent{
		Store: store, RepositoryBeforeRevision: beforeRevision, RepositoryRevision: terminalRevision,
		CommandID: commandID, Sequence: membership.sequence, Partition: membership.partition,
	}
	copy(intent.BeforeCommandDigest[:], beforeDigest)
	copy(intent.CommandDigest[:], terminalDigest)
	return intent, true, nil
}

// RepairCommandPartitionAdmissions finalizes creates that committed after a
// grant but before admission membership publication.
func (a *LocalNudgeAuthority) RepairCommandPartitionAdmissions(ctx context.Context, reader CommandPartitionRecoveryReader) error {
	if isNilRepositoryDependency(reader) {
		return fmt.Errorf("%w: command repository recovery reader is required", ErrLocalNudgeAuthorityConflict)
	}
	checkRelease, err := a.begin(ctx)
	if err != nil {
		return err
	}
	checkRelease()
	state, err := reader.State(ctx)
	if err != nil {
		return fmt.Errorf("repairing local command admissions: reading repository state: %w", err)
	}
	if err := a.validateRecoveryState(state); err != nil {
		return err
	}
	for {
		commandIDs, more, err := a.localAuthorityPreparationPage(ctx, `SELECT command_id FROM admission_preparations ORDER BY command_id LIMIT ?`)
		if err != nil {
			return fmt.Errorf("repairing local command admissions: %w", err)
		}
		if len(commandIDs) == 0 {
			return nil
		}
		for _, commandID := range commandIDs {
			resolution, err := reader.Get(ctx, commandID)
			if err != nil {
				return fmt.Errorf("repairing local admission %q: %w", commandID, err)
			}
			if resolution.Store != a.store || resolution.Revision < state.Revision {
				return fmt.Errorf("%w: admission command %q was read from inconsistent repository authority", ErrLocalNudgeAuthorityConflict, commandID)
			}
			if !resolution.Found {
				if err := a.consumeAbsentLocalNudgeAdmissionPreparation(ctx, commandID); err != nil {
					return err
				}
				continue
			}
			if resolution.Entry.Command == nil {
				return fmt.Errorf("%w: admission command %q is opaque", ErrLocalNudgeAuthorityConflict, commandID)
			}
			command := *resolution.Entry.Command
			release, err := a.begin(ctx)
			if err != nil {
				return err
			}
			grant, found, grantErr := localNudgeGrantByCommandID(ctx, a.db, commandID)
			release()
			if grantErr != nil || !found {
				return fmt.Errorf("%w: admission grant %q disappeared", ErrLocalNudgeAuthorityConflict, commandID)
			}
			if err := a.validatePersistedGrant(grant); err != nil {
				return err
			}
			if !commandIsPristinePending(command) || command.Store != a.store || command.ID != commandID || command.Order.Revision > resolution.Revision ||
				command.TrustedIngress != grant.reference ||
				command.TrustedIngress.PayloadDigest != ComputeCommandPayloadDigest(command) {
				return fmt.Errorf("%w: admission command %q differs from prepared grant", ErrLocalNudgeAuthorityConflict, commandID)
			}
			if err := a.RecordCommandPartitionAdmission(ctx, CommandPartitionAdmission{
				Store: command.Store, RepositoryRevision: command.Order.Revision, CommandID: command.ID,
				Sequence: command.Order.Sequence, Partition: trustedCityPartitionFromAuthority(command.TrustedIngress),
			}); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
	}
}

func (a *LocalNudgeAuthority) consumeAbsentLocalNudgeAdmissionPreparation(ctx context.Context, commandID string) error {
	release, err := a.begin(ctx)
	if err != nil {
		return err
	}
	defer release()
	tx, err := a.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("consuming absent local admission preparation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	grant, found, err := localNudgeGrantByCommandID(ctx, tx, commandID)
	if err != nil || !found {
		return fmt.Errorf("%w: absent admission grant %q disappeared", ErrLocalNudgeAuthorityConflict, commandID)
	}
	if err := a.validatePersistedGrant(grant); err != nil {
		return err
	}
	if _, found, err := localAuthorityMembershipByCommand(ctx, tx, commandID); err != nil || found {
		return fmt.Errorf("%w: absent admission %q acquired membership during recovery", ErrLocalNudgeAuthorityConflict, commandID)
	}
	deleted, err := tx.ExecContext(ctx, `DELETE FROM admission_preparations WHERE command_id = ?`, commandID)
	if err != nil {
		return fmt.Errorf("consuming absent local admission preparation: %w", err)
	}
	if affected, err := deleted.RowsAffected(); err != nil || affected != 1 {
		return fmt.Errorf("%w: absent admission preparation consumption affected %d rows: %w", ErrLocalNudgeAuthorityConflict, affected, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("consuming absent local admission preparation: commit: %w", err)
	}
	return nil
}

func (a *LocalNudgeAuthority) localAuthorityPreparationPage(ctx context.Context, query string) ([]string, bool, error) {
	release, err := a.begin(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	rows, err := a.db.QueryContext(ctx, query, localAuthorityRecoveryPageSize+1)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	commandIDs := make([]string, 0, localAuthorityRecoveryPageSize+1)
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
	more := len(commandIDs) > localAuthorityRecoveryPageSize
	if more {
		commandIDs = commandIDs[:localAuthorityRecoveryPageSize]
	}
	return commandIDs, more, nil
}

// RepairCommandPartitionTerminals resolves every outstanding preparation
// before partition readers or writers become reachable.
func (a *LocalNudgeAuthority) RepairCommandPartitionTerminals(ctx context.Context, reader CommandPartitionRecoveryReader) error {
	if isNilRepositoryDependency(reader) {
		return fmt.Errorf("%w: command repository recovery reader is required", ErrLocalNudgeAuthorityConflict)
	}
	checkRelease, err := a.begin(ctx)
	if err != nil {
		return err
	}
	checkRelease()
	state, err := reader.State(ctx)
	if err != nil {
		return fmt.Errorf("repairing local terminal preparations: reading repository state: %w", err)
	}
	if err := a.validateRecoveryState(state); err != nil {
		return err
	}
	for {
		commandIDs, more, err := a.localAuthorityPreparationPage(ctx, `SELECT command_id FROM terminal_preparations ORDER BY command_id LIMIT ?`)
		if err != nil {
			return fmt.Errorf("repairing local terminal preparations: %w", err)
		}
		if len(commandIDs) == 0 {
			return nil
		}
		for _, commandID := range commandIDs {
			release, err := a.begin(ctx)
			if err != nil {
				return err
			}
			intent, found, intentErr := localAuthorityPreparationByCommand(ctx, a.db, a.store, commandID)
			release()
			if intentErr != nil || !found {
				return fmt.Errorf("%w: terminal preparation %q disappeared", ErrLocalNudgeAuthorityConflict, commandID)
			}
			resolution, err := reader.Get(ctx, commandID)
			if err != nil || !resolution.Found || resolution.Entry.Command == nil {
				return fmt.Errorf("%w: prepared terminal command %q is unavailable: %w", ErrLocalNudgeAuthorityConflict, commandID, err)
			}
			if resolution.Store != a.store || resolution.Revision < state.Revision {
				return fmt.Errorf("%w: prepared terminal command %q was read from inconsistent repository authority", ErrLocalNudgeAuthorityConflict, commandID)
			}
			command := *resolution.Entry.Command
			wire, err := EncodeCommandV1(command)
			if err != nil {
				return err
			}
			digest := sha256.Sum256(wire)
			if command.Terminal != nil && commandIsTerminalState(command.State) {
				if command.Store != intent.Store || command.ID != intent.CommandID || command.Order.Sequence != intent.Sequence ||
					command.Order.Revision != intent.RepositoryRevision || digest != intent.CommandDigest || resolution.Revision < intent.RepositoryRevision {
					return fmt.Errorf("%w: prepared terminal after-state differs", ErrLocalNudgeAuthorityConflict)
				}
				if err := a.RecordCommandPartitionTerminal(ctx, CommandPartitionTerminal{
					Store: intent.Store, RepositoryRevision: intent.RepositoryRevision, CommandID: intent.CommandID,
					Sequence: intent.Sequence, Partition: intent.Partition,
				}); err != nil {
					return err
				}
				continue
			}
			if command.Store != intent.Store || command.ID != intent.CommandID || command.Order.Sequence != intent.Sequence ||
				digest != intent.BeforeCommandDigest || resolution.Revision != intent.RepositoryBeforeRevision {
				return fmt.Errorf("%w: prepared terminal before-state is not safely abortable", ErrLocalNudgeAuthorityConflict)
			}
			if err := a.AbortCommandPartitionTerminal(ctx, intent); err != nil {
				return err
			}
		}
		if !more {
			return nil
		}
	}
}

func (a *LocalNudgeAuthority) validateRecoveryState(state CommandRepositoryState) error {
	if state.Store != a.store || state.SchemaVersion != CommandRepositorySchemaVersion ||
		state.WriterVersion != CommandRepositoryWriterVersion || state.SequenceHighWater > state.Revision {
		return fmt.Errorf("%w: recovery repository authority differs from the local authority binding", ErrLocalNudgeAuthorityConflict)
	}
	return nil
}

// Local authority implements the complete trusted authority surface used by
// ingress, partition reads, claim authorization, and crash recovery.
var _ TrustedNudgeAuthority = (*LocalNudgeAuthority)(nil)
