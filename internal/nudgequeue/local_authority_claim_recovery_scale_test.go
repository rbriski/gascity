//go:build integration

package nudgequeue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	claimAuditScalePending       = 40_000
	claimAuditScalePreparation   = 30_176
	claimAuditScaleReceipt       = 30_176
	claimAuditScaleTotal         = claimAuditScalePending + claimAuditScalePreparation + claimAuditScaleReceipt
	claimAuditScaleGeneration    = claimAuditScalePreparation + 2*claimAuditScaleReceipt
	claimAuditScaleFinalRevision = claimAuditScaleTotal + claimAuditScalePreparation + claimAuditScaleReceipt
	claimAuditScaleFinalIdentity = "f4f5447ce0228dd14cab52512d94a8edb21015305fc5953cc30bba0d2fe54d59"
)

type generatedClaimAuditKind uint8

const (
	generatedClaimAuditPending generatedClaimAuditKind = iota + 1
	generatedClaimAuditPreparation
	generatedClaimAuditReceipt
)

type generatedClaimAuditSeed struct {
	sequence uint64
	kind     generatedClaimAuditKind
}

func TestLocalNudgeAuthorityClaimAuditResumesAcrossMoreThan100KCommands(t *testing.T) {
	if claimAuditScaleTotal <= 100_000 {
		t.Fatalf("claim audit scale population = %d, want greater than 100,000", claimAuditScaleTotal)
	}
	binding := CommandStoreBinding{StoreUUID: "11111111-1111-4111-8111-111111111111", RestoreEpoch: 1}
	emptyState := CommandRepositoryState{
		Store: binding, SchemaVersion: CommandRepositorySchemaVersion, WriterVersion: CommandRepositoryWriterVersion,
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), emptyState, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	state := emptyState
	state.SequenceHighWater = claimAuditScaleTotal
	state.Revision = claimAuditScaleFinalRevision
	store := &generatedClaimAuditStore{
		Store: beads.NewMemStore(), state: state,
		metadata: repositoryMetadataForTest(state), seeds: make(map[string]generatedClaimAuditSeed, claimAuditScaleTotal),
	}
	repository := &CommandRepository{reader: commandRepositoryReader{
		store: store,
		verifier: CommandRepositoryLineageVerifierFunc(func(_ context.Context, observed CommandRepositoryState) error {
			if observed != state {
				return fmt.Errorf("generated repository state = %#v, want %#v", observed, state)
			}
			return nil
		}),
	}}
	seedGeneratedClaimAuditAuthority(t, authority, store)

	previous, err := authority.readLocalAuthorityClaimAuditCursor(t.Context(), authority.db)
	if err != nil {
		t.Fatalf("read initial claim audit cursor: %v", err)
	}
	maxInvocations := (claimAuditScaleTotal+claimAuditScalePreparation+claimAuditScaleReceipt)/commandAuthorityRecoveryMaxWork + 8
	var finalToken localAuthorityClaimRecoveryToken
	for invocation := 1; invocation <= maxInvocations; invocation++ {
		beforeGets, beforeAtomic := store.getCalls, store.atomicCalls
		token, stable, recoveryErr := authority.repairCommandClaimTransitions(t.Context(), repository, state)
		getDelta := store.getCalls - beforeGets
		atomicDelta := store.atomicCalls - beforeAtomic
		if getDelta > commandAuthorityRecoveryMaxWork {
			t.Fatalf("claim audit invocation %d generated %d commands, want <= %d", invocation, getDelta, commandAuthorityRecoveryMaxWork)
		}
		if atomicDelta > 2*getDelta {
			t.Fatalf("claim audit invocation %d used %d repository transactions for %d exact commands", invocation, atomicDelta, getDelta)
		}
		current, err := authority.readLocalAuthorityClaimAuditCursor(t.Context(), authority.db)
		if err != nil {
			t.Fatalf("read claim audit cursor after invocation %d: %v", invocation, err)
		}
		if !localClaimAuditCursorProgressed(previous, current) {
			t.Fatalf("claim audit invocation %d did not advance durable progress: before=%#v after=%#v", invocation, previous, current)
		}
		if stable {
			if recoveryErr != nil {
				t.Fatalf("stable claim audit invocation %d error: %v", invocation, recoveryErr)
			}
			finalToken = token
			previous = current
			break
		}
		if !errors.Is(recoveryErr, ErrCommandAuthorityRecoveryYield) {
			t.Fatalf("partial claim audit invocation %d error = %v, want recovery yield", invocation, recoveryErr)
		}
		previous = current
		if invocation == maxInvocations {
			t.Fatalf("claim audit did not converge within %d bounded invocations", maxInvocations)
		}
	}
	if previous.phase != localAuthorityClaimAuditDone || previous.generation != claimAuditScaleGeneration ||
		previous.repositoryRevision != state.Revision || previous.sequenceHighWater != state.SequenceHighWater ||
		previous.preparationCount != claimAuditScalePreparation || previous.receiptCount != claimAuditScaleReceipt ||
		previous.identity == initialLocalAuthorityClaimAuditIdentity() {
		t.Fatalf("final claim audit cursor = %#v, want complete 100K mixed audit", previous)
	}
	if got := hex.EncodeToString(previous.identity[:]); got != claimAuditScaleFinalIdentity {
		t.Fatalf("final claim audit identity = %s, want %s", got, claimAuditScaleFinalIdentity)
	}
	if finalToken != previous.token() {
		t.Fatalf("stable claim recovery token = %#v, want final cursor token %#v", finalToken, previous.token())
	}
	wantGets := claimAuditScaleTotal + claimAuditScalePreparation + claimAuditScaleReceipt
	if store.getCalls != wantGets {
		t.Fatalf("lazy repository exact reads = %d, want %d", store.getCalls, wantGets)
	}
}

func seedGeneratedClaimAuditAuthority(t *testing.T, authority *LocalNudgeAuthority, store *generatedClaimAuditStore) {
	t.Helper()
	tx, err := authority.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin claim audit scale seed: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	grant, err := tx.PrepareContext(t.Context(), `INSERT INTO ingress_grants (
		reference_id, request_id, request_fingerprint, command_id, principal_schema, issuer, principal_id,
		tenant_scope, city_scope, credential_class, policy_version, policy_decision_id, action, target_session_id,
		payload_digest, command_created_at, issued_at, expires_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare scale ingress insert: %v", err)
	}
	defer func() { _ = grant.Close() }()
	admission, err := tx.PrepareContext(t.Context(), `INSERT INTO admission_decisions (
		sequence, command_id, decision_kind, allocation_revision, decision_revision,
		grant_command_id, grant_reference_id, partition_id
	) VALUES (?, ?, 'admitted', ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare scale admission insert: %v", err)
	}
	defer func() { _ = admission.Close() }()
	partition := trustedCityPartitionFromAuthority(generatedClaimAuditCommand(store.state, generatedClaimAuditSeed{sequence: 1, kind: generatedClaimAuditPending}).TrustedIngress)
	for sequence := uint64(1); sequence <= claimAuditScaleTotal; sequence++ {
		seed := generatedClaimAuditSeedForSequence(sequence)
		kind := seed.kind
		requestID := generatedClaimAuditRequestID(sequence)
		commandID := CommandIDForRequest(store.state.Store, requestID)
		store.seeds[commandID] = seed
		before := generatedClaimAuditPendingCommand(store.state, seed)
		fingerprint := generatedClaimAuditAuthorizationFingerprint(before, requestID)
		if _, err := grant.ExecContext(t.Context(),
			before.TrustedIngress.ReferenceID, requestID, fingerprint[:], commandID, NudgePrincipalSchemaVersion,
			before.TrustedIngress.Issuer, before.TrustedIngress.PrincipalID, before.TrustedIngress.TenantScope,
			before.TrustedIngress.CityScope, before.TrustedIngress.CredentialClass, before.TrustedIngress.PolicyVersion,
			before.TrustedIngress.PolicyDecisionID, before.TrustedIngress.Action, before.TrustedIngress.TargetSessionID,
			before.TrustedIngress.PayloadDigest, before.CreatedAt.Format(time.RFC3339Nano),
			before.TrustedIngress.IssuedAt.Format(time.RFC3339Nano), before.TrustedIngress.ExpiresAt.Format(time.RFC3339Nano),
		); err != nil {
			t.Fatalf("insert scale ingress %d: %v", sequence, err)
		}
		sequenceWire := encodeLocalAuthorityUint64(sequence)
		if _, err := admission.ExecContext(t.Context(), sequenceWire, commandID, sequenceWire, sequenceWire, commandID, before.TrustedIngress.ReferenceID, partition.identity[:]); err != nil {
			t.Fatalf("insert scale admission %d: %v", sequence, err)
		}
		if kind == generatedClaimAuditPending {
			continue
		}
		after := generatedClaimAuditCommand(store.state, seed)
		beforeWire, err := EncodeCommandV1(before)
		if err != nil {
			t.Fatalf("encode scale before command %d: %v", sequence, err)
		}
		afterWire, err := EncodeCommandV1(after)
		if err != nil {
			t.Fatalf("encode scale after command %d: %v", sequence, err)
		}
		intent := CommandClaimTransitionIntent{
			Store: store.state.Store, RepositoryBeforeRevision: after.Order.Revision - 1,
			RepositoryRevision: after.Order.Revision, RepositorySequenceHighWater: store.state.SequenceHighWater,
			CommandID: commandID, Sequence: sequence, Partition: partition,
			BeforeCommandDigest: sha256.Sum256(beforeWire), AfterCommandDigest: sha256.Sum256(afterWire), Claim: *after.Claim,
		}
		if err := validateCommandClaimTransitionIntent(intent); err != nil {
			t.Fatalf("validate scale claim intent %d: %v", sequence, err)
		}
		if kind == generatedClaimAuditPreparation {
			if err := insertLocalAuthorityClaimPreparation(t.Context(), tx, intent); err != nil {
				t.Fatalf("insert scale claim preparation %d: %v", sequence, err)
			}
			continue
		}
		receipt := CommandClaimTransitionReceipt{
			Store: store.state.Store, RepositoryRevision: after.Order.Revision, CommandID: commandID,
			Sequence: sequence, Partition: partition, AfterCommandDigest: intent.AfterCommandDigest, Claim: intent.Claim,
			EffectRepositoryRevision: store.state.Revision, EffectSequenceHighWater: store.state.SequenceHighWater,
		}
		if err := insertLocalAuthorityClaimReceipt(t.Context(), tx, intent, receipt); err != nil {
			t.Fatalf("insert scale claim receipt %d: %v", sequence, err)
		}
	}
	generation := uint64(claimAuditScaleGeneration)
	if _, err := tx.ExecContext(t.Context(), `UPDATE authority_meta SET
		dense_decision_high_water = ?, highest_observed_sequence = ?, highest_observed_revision = ?,
		claim_transition_generation = ?, claim_preparation_count = ?, claim_receipt_count = ?
		WHERE singleton = 1`,
		encodeLocalAuthorityUint64(store.state.SequenceHighWater), encodeLocalAuthorityUint64(store.state.SequenceHighWater),
		encodeLocalAuthorityUint64(store.state.Revision), encodeLocalAuthorityUint64(generation),
		encodeLocalAuthorityUint64(claimAuditScalePreparation), encodeLocalAuthorityUint64(claimAuditScaleReceipt),
	); err != nil {
		t.Fatalf("update claim audit scale metadata: %v", err)
	}
	cursor := localAuthorityClaimAuditCursor{
		generation: generation, repositoryRevision: store.state.Revision, sequenceHighWater: store.state.SequenceHighWater,
		phase: localAuthorityClaimAuditPreparations, identity: initialLocalAuthorityClaimAuditIdentity(),
	}
	if err := authority.updateLocalAuthorityClaimAuditCursor(t.Context(), tx, cursor); err != nil {
		t.Fatalf("initialize claim audit scale cursor: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit claim audit scale seed: %v", err)
	}
	assertGeneratedClaimAuditGrantSamples(t, authority, store.state)
}

func localClaimAuditCursorProgressed(before, after localAuthorityClaimAuditCursor) bool {
	rank := func(phase localAuthorityClaimAuditPhase) int {
		switch phase {
		case localAuthorityClaimAuditPreparations:
			return 1
		case localAuthorityClaimAuditReceipts:
			return 2
		case localAuthorityClaimAuditActive:
			return 3
		case localAuthorityClaimAuditDone:
			return 4
		default:
			return 0
		}
	}
	if rank(after.phase) != rank(before.phase) {
		return rank(after.phase) > rank(before.phase)
	}
	switch after.phase {
	case localAuthorityClaimAuditPreparations, localAuthorityClaimAuditReceipts:
		return after.afterCommandID > before.afterCommandID
	case localAuthorityClaimAuditActive:
		return after.afterSequence > before.afterSequence
	default:
		return false
	}
}

func generatedClaimAuditRequestID(sequence uint64) string {
	return fmt.Sprintf("claim-audit-scale-%06d", sequence)
}

func generatedClaimAuditPendingCommand(state CommandRepositoryState, seed generatedClaimAuditSeed) Command {
	command := validCommandV1(CommandStatePending)
	requestID := generatedClaimAuditRequestID(seed.sequence)
	command.ID = CommandIDForRequest(state.Store, requestID)
	command.Store = state.Store
	command.Order = CommandOrder{Sequence: seed.sequence, Revision: seed.sequence}
	command.Message = requestID
	command.TrustedIngress = generatedClaimAuditTrustedIngress(command, requestID)
	return command
}

func generatedClaimAuditRevision(seed generatedClaimAuditSeed) uint64 {
	switch seed.kind {
	case generatedClaimAuditPending:
		return seed.sequence
	case generatedClaimAuditPreparation:
		return claimAuditScaleTotal + (seed.sequence - claimAuditScalePending)
	case generatedClaimAuditReceipt:
		return claimAuditScaleTotal + claimAuditScalePreparation +
			(seed.sequence - claimAuditScalePending - claimAuditScalePreparation)
	default:
		return 0
	}
}

func generatedClaimAuditRequester() AuthenticatedNudgeRequester {
	return AuthenticatedNudgeRequester{
		PrincipalID: "claim-audit-scale-principal", TenantScope: localAuthorityOptions().TenantScope,
		CityScope: localAuthorityOptions().CityScope, CredentialClass: localAuthorityOptions().CredentialClass,
		EvidenceID: "claim-audit-scale-evidence",
	}
}

func generatedClaimAuditAuthorizationFingerprint(command Command, requestID string) [sha256.Size]byte {
	request := NudgeIngressAuthorizationRequest{
		RequestID: requestID, Action: NudgeCommandAction, Mode: command.Mode, Target: command.Target,
		IntentDigest: computeNudgeIngressIntentDigest(command), PayloadDigest: ComputeCommandPayloadDigest(command),
		DeliverAfter: command.DeliverAfter, ExpiresAt: command.ExpiresAt, RequestedAt: command.CreatedAt,
	}
	return localNudgeAuthorizationFingerprint(request, generatedClaimAuditRequester())
}

func generatedClaimAuditTrustedIngress(command Command, requestID string) TrustedIngressReference {
	fingerprint := generatedClaimAuditAuthorizationFingerprint(command, requestID)
	referenceMaterial := append([]byte("gascity.local-nudge-authority.reference.v1\x00"), []byte(command.ID)...)
	referenceMaterial = append(referenceMaterial, fingerprint[:]...)
	referenceDigest := sha256.Sum256(referenceMaterial)
	requester := generatedClaimAuditRequester()
	return TrustedIngressReference{
		Issuer: localAuthorityOptions().Issuer, ReferenceID: "local-ref-" + hex.EncodeToString(referenceDigest[:]),
		PrincipalID: requester.PrincipalID, TenantScope: requester.TenantScope, CityScope: requester.CityScope,
		CredentialClass: requester.CredentialClass, PolicyVersion: localAuthorityOptions().PolicyVersion,
		PolicyDecisionID: "local-decision-" + hex.EncodeToString(fingerprint[:]), Action: NudgeCommandAction,
		TargetSessionID: command.Target.SessionID, PayloadDigest: ComputeCommandPayloadDigest(command),
		IssuedAt: command.CreatedAt, ExpiresAt: time.Date(9999, 12, 31, 23, 59, 59, 0, time.UTC),
	}
}

func assertGeneratedClaimAuditGrantSamples(t *testing.T, authority *LocalNudgeAuthority, state CommandRepositoryState) {
	t.Helper()
	for _, sequence := range []uint64{1, claimAuditScaleTotal / 2, claimAuditScaleTotal} {
		seed := generatedClaimAuditSeedForSequence(sequence)
		command := generatedClaimAuditPendingCommand(state, seed)
		requestID := generatedClaimAuditRequestID(sequence)
		grant, found, err := authority.grantByRequestID(t.Context(), requestID)
		if err != nil || !found {
			t.Fatalf("read generated ingress grant sample %d = found:%t err:%v", sequence, found, err)
		}
		if err := authority.validatePersistedGrant(grant); err != nil {
			t.Fatalf("validate generated ingress grant sample %d: %v", sequence, err)
		}
		if grant.commandID != command.ID || grant.fingerprint != generatedClaimAuditAuthorizationFingerprint(command, requestID) ||
			grant.reference != command.TrustedIngress {
			t.Fatalf("generated ingress grant sample %d differs from canonical command provenance", sequence)
		}
	}
}

func generatedClaimAuditSeedForSequence(sequence uint64) generatedClaimAuditSeed {
	kind := generatedClaimAuditPending
	switch {
	case sequence > claimAuditScalePending+claimAuditScalePreparation:
		kind = generatedClaimAuditReceipt
	case sequence > claimAuditScalePending:
		kind = generatedClaimAuditPreparation
	}
	return generatedClaimAuditSeed{sequence: sequence, kind: kind}
}

func generatedClaimAuditCommand(state CommandRepositoryState, seed generatedClaimAuditSeed) Command {
	command := generatedClaimAuditPendingCommand(state, seed)
	if seed.kind == generatedClaimAuditPending {
		return command
	}
	claimedAt := command.CreatedAt.Add(2 * time.Second)
	claim := &CommandClaim{
		ID: fmt.Sprintf("claim-scale-%06d", seed.sequence), OwnerID: "claim-audit-scale-owner",
		OperationID: command.ID, AttemptID: fmt.Sprintf("attempt-scale-%06d", seed.sequence),
		BoundLaunchIdentity: "claim-audit-scale-launch", AuthorizationDecisionID: "claim-audit-scale-authorization",
		AuthorizationPolicyVersion: localAuthorityOptions().PolicyVersion,
		ClaimedAt:                  claimedAt, LeaseUntil: claimedAt.Add(time.Minute),
	}
	command.State = CommandStateInFlight
	command.Order.Revision = generatedClaimAuditRevision(seed)
	command.Claim = claim
	command.Binding = &CommandBinding{LaunchIdentity: claim.BoundLaunchIdentity, BoundAt: claim.ClaimedAt}
	command.Retry = &CommandRetry{
		AttemptCount: 1, LastAttemptAt: claim.ClaimedAt, ClaimID: claim.ID, OperationID: claim.OperationID,
		AttemptID: claim.AttemptID, BoundLaunchIdentity: claim.BoundLaunchIdentity,
		AuthorizationDecisionID: claim.AuthorizationDecisionID, AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
	}
	return command
}

type generatedClaimAuditStore struct {
	beads.Store
	state       CommandRepositoryState
	metadata    map[string]string
	seeds       map[string]generatedClaimAuditSeed
	getCalls    int
	atomicCalls int
}

func (s *generatedClaimAuditStore) AtomicReadWrite(ctx context.Context, _ string, fn func(beads.AtomicReadWriteTx) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	s.atomicCalls++
	return fn(generatedClaimAuditTx{store: s})
}

type generatedClaimAuditTx struct {
	store *generatedClaimAuditStore
}

func (tx generatedClaimAuditTx) GetIssue(id string) (beads.Bead, error) {
	seed, found := tx.store.seeds[id]
	if !found {
		return beads.Bead{}, beads.ErrNotFound
	}
	command := generatedClaimAuditCommand(tx.store.state, seed)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		return beads.Bead{}, err
	}
	tx.store.getCalls++
	return beads.Bead{
		ID: id, Title: commandRecordTitle, Type: commandRecordBeadType, Status: "open",
		Metadata: map[string]string{
			commandRecordKindMetadataKey:        commandRecordKindMetadataValue,
			commandRecordCommandKindMetadataKey: commandRecordCommandKindMetadataValue,
			commandRecordRequestIDMetadataKey:   generatedClaimAuditRequestID(seed.sequence),
			commandRecordWireMetadataKey:        string(wire),
		},
	}, nil
}

func (tx generatedClaimAuditTx) ListHistory(beads.AtomicReadWriteList) ([]beads.Bead, error) {
	return nil, errors.New("generated claim audit store does not support history scans")
}

func (tx generatedClaimAuditTx) Create(beads.Bead) (beads.Bead, error) {
	return beads.Bead{}, errors.New("generated claim audit store is read-only")
}

func (tx generatedClaimAuditTx) Update(string, beads.UpdateOpts) error {
	return errors.New("generated claim audit store is read-only")
}

func (tx generatedClaimAuditTx) GetMetadata(key string) (string, error) {
	return tx.store.metadata[key], nil
}

func (tx generatedClaimAuditTx) SetMetadata(string, string) error {
	return errors.New("generated claim audit store is read-only")
}
