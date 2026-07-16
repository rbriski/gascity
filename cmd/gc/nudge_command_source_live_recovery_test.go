package main

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

var (
	errLiveNudgeAdmissionPublicationLost = errors.New("simulated live admission publication loss")
	errLiveNudgeAnchorAdvanceLost        = errors.New("simulated post-commit anchor advance loss")
)

func TestProductionNudgeCommandSourceRecoversLiveAdmissionPublicationLoss(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	fixture.authority.dropNextAdmissionPublication()

	const requestID = "live-admission-publication-loss"
	_, err := fixture.ingress.Admit(fixture.requestContext(t), liveNudgeCommandRequest(requestID))
	if !errors.Is(err, errLiveNudgeAdmissionPublicationLost) {
		t.Fatalf("Admit publication loss = %v, want simulated lost publication", err)
	}
	commandID := nudgequeue.CommandIDForRequest(fixture.storeBinding, requestID)
	durable, err := fixture.repository.Get(t.Context(), commandID)
	if err != nil || !durable.Found || durable.Entry.Command == nil {
		t.Fatalf("durable command after lost publication = %#v, err=%v", durable, err)
	}

	resolved, err := fixture.source.Get(t.Context(), commandID)
	if err != nil {
		t.Fatalf("Get with online authority recovery: %v", err)
	}
	if !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.ID != commandID {
		t.Fatalf("Get after online authority recovery = %#v, want exact durable command", resolved)
	}
	if got := fixture.authority.recoveryCallCount(); got != 2 {
		t.Fatalf("authority recovery calls = %d, want startup plus one live repair", got)
	}
}

func TestProductionNudgeCommandSourceRejectsLiveUnauthorizedCommand(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	forgedAuthority := newProductionNudgeTestAuthority()
	forgedIngress, err := nudgequeue.NewTrustedNudgeIngress(fixture.repository, forgedAuthority)
	if err != nil {
		t.Fatalf("NewTrustedNudgeIngress(forged): %v", err)
	}

	const requestID = "live-unauthorized-direct-command"
	forged, err := forgedIngress.Admit(t.Context(), liveNudgeCommandRequest(requestID))
	if err != nil || forged.Entry.Command == nil {
		t.Fatalf("forged Admit = %#v, err=%v", forged, err)
	}

	snapshot, err := fixture.source.Snapshot(t.Context(), 10)
	if err != nil {
		t.Fatalf("Snapshot with online provenance recovery: %v", err)
	}
	for _, entry := range snapshot.Entries {
		if entry.Command != nil && entry.Command.ID == forged.Entry.Command.ID {
			t.Fatalf("unauthorized command %q escaped into live partition snapshot", entry.Command.ID)
		}
	}
	resolved, err := fixture.repository.Get(t.Context(), forged.Entry.Command.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.Terminal == nil {
		t.Fatalf("unauthorized command after online recovery = %#v, err=%v", resolved, err)
	}
	if got := resolved.Entry.Command.Terminal.ActionResult; got != nudgequeue.CommandActionResultUnauthorizedProvenance {
		t.Fatalf("unauthorized command action result = %q, want %q", got, nudgequeue.CommandActionResultUnauthorizedProvenance)
	}
	if got := fixture.authority.recoveryCallCount(); got != 2 {
		t.Fatalf("authority recovery calls = %d, want startup plus one live audit", got)
	}
}

func TestProductionNudgeCommandSourceSuccessfulReadsSkipLiveRecovery(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	baseline := fixture.authority.recoveryCallCount()

	if _, err := fixture.source.Snapshot(t.Context(), 10); err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if _, err := fixture.source.Get(t.Context(), fixture.seed.ID); err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := fixture.authority.recoveryCallCount(); got != baseline {
		t.Fatalf("authority recovery calls after successful reads = %d, want unchanged %d", got, baseline)
	}
}

func TestProductionNudgeCommandSourceRecoversLiveDatabaseAheadOfAnchor(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	lineage := &liveNudgeInterruptedLineageController{
		RestoreAnchorRepositoryVerifier: nudgequeue.NewRestoreAnchorRepositoryVerifier(fixture.cityPath),
		failAdvance:                     errLiveNudgeAnchorAdvanceLost,
	}
	writerRepository, err := nudgequeue.NewCommandRepository(fixture.store, lineage)
	if err != nil {
		t.Fatalf("NewCommandRepository(interrupted writer): %v", err)
	}
	writerIngress, err := nudgequeue.NewTrustedNudgeIngress(writerRepository, fixture.authority)
	if err != nil {
		t.Fatalf("NewTrustedNudgeIngress(interrupted writer): %v", err)
	}

	const requestID = "live-database-ahead-of-anchor"
	_, err = writerIngress.Admit(fixture.requestContext(t), liveNudgeCommandRequest(requestID))
	if !errors.Is(err, nudgequeue.ErrCommandRepositoryLineage) || !errors.Is(err, errLiveNudgeAnchorAdvanceLost) {
		t.Fatalf("Admit after interrupted anchor advance = %v, want typed post-commit lineage failure", err)
	}
	commandID := nudgequeue.CommandIDForRequest(fixture.storeBinding, requestID)
	if _, err := fixture.source.reader.Get(t.Context(), commandID); !errors.Is(err, nudgequeue.ErrCommandRepositoryLineage) || !errors.Is(err, nudgequeue.ErrRestoreAnchorAdmission) {
		t.Fatalf("raw database-ahead read error = %v, want typed lineage/admission refusal", err)
	}

	resolved, err := fixture.source.Get(t.Context(), commandID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.ID != commandID {
		t.Fatalf("Get after online lineage repair = %#v, err=%v", resolved, err)
	}
	if got := fixture.authority.recoveryCallCount(); got != 2 {
		t.Fatalf("authority recovery calls = %d, want startup plus one live lineage repair", got)
	}
}

func TestProductionNudgeCommandSourcePreservesCanceledReadWithoutRecovery(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err := fixture.source.Get(ctx, fixture.seed.ID)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Get error = %v, want context.Canceled", err)
	}
	if got := fixture.authority.recoveryCallCount(); got != 1 {
		t.Fatalf("authority recovery calls after canceled read = %d, want startup only", got)
	}
}

func TestProductionNudgeCommandSourceFailsClosedOnUnsafeLiveLineage(t *testing.T) {
	tests := map[string]func(nudgequeue.RestoreAnchor) nudgequeue.RestoreAnchor{
		"anchor rewind": func(anchor nudgequeue.RestoreAnchor) nudgequeue.RestoreAnchor {
			anchor.HighestAcceptedRevision = 0
			anchor.HighestAcceptedSequence = 0
			return anchor
		},
		"foreign anchor": func(anchor nudgequeue.RestoreAnchor) nudgequeue.RestoreAnchor {
			anchor.Store.StoreUUID = "00000000-0000-4000-8000-000000000001"
			return anchor
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			fixture := newLiveNudgeCommandRecoveryFixture(t)
			anchorPath := nudgequeue.RestoreAnchorPath(fixture.cityPath)
			anchor, exists, err := nudgequeue.LoadRestoreAnchor(t.Context(), anchorPath)
			if err != nil || !exists {
				t.Fatalf("LoadRestoreAnchor = %#v, exists=%t, err=%v", anchor, exists, err)
			}
			wire, err := nudgequeue.EncodeRestoreAnchor(mutate(anchor))
			if err != nil {
				t.Fatalf("EncodeRestoreAnchor: %v", err)
			}
			if err := os.WriteFile(anchorPath, wire, 0o600); err != nil {
				t.Fatalf("write unsafe live anchor: %v", err)
			}

			resolved, err := fixture.source.Get(t.Context(), fixture.seed.ID)
			if err == nil || resolved.Found {
				t.Fatalf("unsafe-lineage Get = %#v, err=%v; want fail-closed refusal", resolved, err)
			}
			if !errors.Is(err, nudgequeue.ErrCommandRepositoryLineage) || !errors.Is(err, nudgequeue.ErrRestoreAnchorAdmission) {
				t.Fatalf("unsafe-lineage error = %v, want typed lineage/admission refusal", err)
			}
			if got := fixture.authority.recoveryCallCount(); got != 1 {
				t.Fatalf("authority recovery calls = %d, want unsafe binding refused before authority delegation", got)
			}
		})
	}
}

func TestProductionNudgeCommandSourceSerializesConcurrentLiveRecovery(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	fixture.authority.dropNextAdmissionPublication()

	const requestID = "concurrent-live-admission-publication-loss"
	_, err := fixture.ingress.Admit(fixture.requestContext(t), liveNudgeCommandRequest(requestID))
	if !errors.Is(err, errLiveNudgeAdmissionPublicationLost) {
		t.Fatalf("Admit publication loss = %v, want simulated lost publication", err)
	}
	commandID := nudgequeue.CommandIDForRequest(fixture.storeBinding, requestID)
	membershipReads := fixture.authority.observeMembershipReads()
	recoveryStarted, releaseRecovery := fixture.authority.blockLiveRecovery()
	released := false
	defer func() {
		if !released {
			close(releaseRecovery)
		}
	}()

	type readResult struct {
		resolution nudgequeue.CommandIndexResolution
		err        error
	}
	results := make(chan readResult, 2)
	read := func() {
		resolution, err := fixture.source.Get(t.Context(), commandID)
		results <- readResult{resolution: resolution, err: err}
	}
	go read()
	receiveBeforeDeadline(t, recoveryStarted)
	// The first caller performs its initial read and its under-lock recheck
	// before recovery begins.
	receiveBeforeDeadline(t, membershipReads)
	receiveBeforeDeadline(t, membershipReads)

	go read()
	// Hold the first recovery until the second caller has independently seen
	// the same missing-membership failure. Without source-level serialization,
	// both callers would enter durable recovery.
	receiveBeforeDeadline(t, membershipReads)
	close(releaseRecovery)
	released = true

	for range 2 {
		result := receiveBeforeDeadline(t, results)
		if result.err != nil || !result.resolution.Found || result.resolution.Entry.Command == nil || result.resolution.Entry.Command.ID != commandID {
			t.Fatalf("concurrent Get after online recovery = %#v, err=%v", result.resolution, result.err)
		}
	}
	if got := fixture.authority.recoveryCallCount(); got != 2 {
		t.Fatalf("authority recovery calls = %d, want startup plus one serialized live repair", got)
	}
}

func TestProductionNudgeCommandSourceCanceledReaderDoesNotWaitForActiveRecovery(t *testing.T) {
	testProductionNudgeCommandSourceWaitingReaderDoesNotWaitForActiveRecovery(
		t,
		context.WithCancel,
		context.Canceled,
	)
}

func TestProductionNudgeCommandSourceDeadlineReaderDoesNotWaitForActiveRecovery(t *testing.T) {
	testProductionNudgeCommandSourceWaitingReaderDoesNotWaitForActiveRecovery(
		t,
		func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			return liveNudgeDeadlineContext{Context: ctx}, cancel
		},
		context.DeadlineExceeded,
	)
}

func testProductionNudgeCommandSourceWaitingReaderDoesNotWaitForActiveRecovery(
	t *testing.T,
	newWaitingContext func(context.Context) (context.Context, context.CancelFunc),
	wantErr error,
) {
	t.Helper()
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	fixture.authority.dropNextAdmissionPublication()

	const requestID = "cancel-waiting-live-recovery"
	_, err := fixture.ingress.Admit(fixture.requestContext(t), liveNudgeCommandRequest(requestID))
	if !errors.Is(err, errLiveNudgeAdmissionPublicationLost) {
		t.Fatalf("Admit publication loss = %v, want simulated lost publication", err)
	}
	commandID := nudgequeue.CommandIDForRequest(fixture.storeBinding, requestID)
	membershipReads := fixture.authority.observeMembershipReads()
	recoveryStarted, releaseRecovery := fixture.authority.blockLiveRecovery()
	released := false
	defer func() {
		if !released {
			close(releaseRecovery)
		}
	}()

	type readResult struct {
		resolution nudgequeue.CommandIndexResolution
		err        error
	}
	firstResult := make(chan readResult, 1)
	go func() {
		resolution, err := fixture.source.Get(t.Context(), commandID)
		firstResult <- readResult{resolution: resolution, err: err}
	}()
	receiveBeforeDeadline(t, recoveryStarted)
	// The first caller has completed both its initial read and its serialized
	// recheck before entering the deliberately blocked recovery.
	receiveBeforeDeadline(t, membershipReads)
	receiveBeforeDeadline(t, membershipReads)

	waitingCtx, finishWaiting := newWaitingContext(t.Context())
	secondResult := make(chan readResult, 1)
	go func() {
		resolution, err := fixture.source.Get(waitingCtx, commandID)
		secondResult <- readResult{resolution: resolution, err: err}
	}()
	// Prove the second caller independently reached the recovery-serialization
	// boundary before cancellation. No elapsed-time sleep is needed.
	receiveBeforeDeadline(t, membershipReads)
	finishWaiting()

	canceled := receiveBeforeDeadline(t, secondResult)
	if !errors.Is(canceled.err, wantErr) {
		t.Fatalf("waiting Get error = %v, want %v", canceled.err, wantErr)
	}
	if canceled.resolution.Found {
		t.Fatalf("waiting canceled Get resolution = %#v, want no published command", canceled.resolution)
	}
	select {
	case early := <-firstResult:
		t.Fatalf("active recovery returned before release: resolution=%#v err=%v", early.resolution, early.err)
	default:
	}

	close(releaseRecovery)
	released = true
	first := receiveBeforeDeadline(t, firstResult)
	if first.err != nil || !first.resolution.Found || first.resolution.Entry.Command == nil || first.resolution.Entry.Command.ID != commandID {
		t.Fatalf("active Get after release = %#v, err=%v", first.resolution, first.err)
	}
	if got := fixture.authority.recoveryCallCount(); got != 2 {
		t.Fatalf("authority recovery calls = %d, want startup plus active live repair only", got)
	}
}

type liveNudgeDeadlineContext struct {
	context.Context
}

func (c liveNudgeDeadlineContext) Err() error {
	if c.Context.Err() != nil {
		return context.DeadlineExceeded
	}
	return nil
}

func TestProductionNudgeCommandSourceDoesNotRecoverDefinitiveProvenanceRejection(t *testing.T) {
	fixture := newLiveNudgeCommandRecoveryFixture(t)
	fixture.authority.rejectMembership(fixture.seed.ID, fixture.seed.Order.Sequence)
	baseline := fixture.authority.recoveryCallCount()

	for attempt := 1; attempt <= 2; attempt++ {
		resolution, err := fixture.source.Get(t.Context(), fixture.seed.ID)
		if !errors.Is(err, nudgequeue.ErrCommandProvenanceRejected) {
			t.Fatalf("rejected Get attempt %d error = %v, want definitive provenance rejection", attempt, err)
		}
		if resolution.Found {
			t.Fatalf("rejected Get attempt %d resolution = %#v, want no published command", attempt, resolution)
		}
	}
	if got := fixture.authority.recoveryCallCount(); got != baseline {
		t.Fatalf("authority recovery calls after stable rejection = %d, want unchanged %d", got, baseline)
	}
}

func TestProductionNudgeCommandSourceClassifiesRecoveryYieldAsTransient(t *testing.T) {
	source := &productionNudgeCommandSource{}
	err := errors.Join(errors.New("live authority recovery paused safely"), nudgequeue.ErrCommandAuthorityRecoveryYield)

	if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorTransient {
		t.Fatalf("ClassifyNudgeCommandSourceError(%v) = %d, want transient", err, got)
	}
}

type liveNudgeCommandRecoveryFixture struct {
	cityPath     string
	store        *nudgeCommandSourceAtomicStore
	repository   *nudgequeue.CommandRepository
	authority    *liveNudgeCommandRecoveryAuthority
	ingress      *nudgequeue.TrustedNudgeIngress
	source       *productionNudgeCommandSource
	storeBinding nudgequeue.CommandStoreBinding
	seed         nudgequeue.Command
}

func newLiveNudgeCommandRecoveryFixture(t *testing.T) liveNudgeCommandRecoveryFixture {
	t.Helper()
	cityPath := t.TempDir()
	store := newNudgeCommandSourceAtomicStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(cityPath))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	state, err := repository.Provision(t.Context())
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	local, err := nudgequeue.OpenLocalNudgeAuthority(t.Context(), cityPath, state, liveNudgeAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Errorf("Close local nudge authority: %v", err)
		}
	})
	authority := &liveNudgeCommandRecoveryAuthority{LocalNudgeAuthority: local}
	ingress, err := nudgequeue.NewTrustedNudgeIngress(repository, authority)
	if err != nil {
		t.Fatalf("NewTrustedNudgeIngress: %v", err)
	}
	seed, err := ingress.Admit(
		nudgequeue.WithAuthenticatedNudgeRequester(t.Context(), liveNudgeRequester()),
		liveNudgeCommandRequest("live-recovery-seed"),
	)
	if err != nil || seed.Entry.Command == nil {
		t.Fatalf("seed Admit = %#v, err=%v", seed, err)
	}
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, seed.Partition, ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source, ok := opened.(*productionNudgeCommandSource)
	if !ok {
		t.Fatalf("production source = %T, want *productionNudgeCommandSource", opened)
	}
	if got := authority.recoveryCallCount(); got != 1 {
		t.Fatalf("startup authority recovery calls = %d, want 1", got)
	}
	return liveNudgeCommandRecoveryFixture{
		cityPath: cityPath, store: store, repository: repository, authority: authority,
		ingress: ingress, source: source, storeBinding: state.Store, seed: *seed.Entry.Command,
	}
}

type liveNudgeInterruptedLineageController struct {
	*nudgequeue.RestoreAnchorRepositoryVerifier

	mu          sync.Mutex
	failAdvance error
}

func (c *liveNudgeInterruptedLineageController) AdvanceCommandRepositoryLineage(ctx context.Context, state nudgequeue.CommandRepositoryState) error {
	c.mu.Lock()
	failure := c.failAdvance
	c.failAdvance = nil
	c.mu.Unlock()
	if failure != nil {
		return failure
	}
	return c.RestoreAnchorRepositoryVerifier.AdvanceCommandRepositoryLineage(ctx, state)
}

func (f liveNudgeCommandRecoveryFixture) requestContext(t *testing.T) context.Context {
	t.Helper()
	return nudgequeue.WithAuthenticatedNudgeRequester(t.Context(), liveNudgeRequester())
}

type liveNudgeCommandRecoveryAuthority struct {
	*nudgequeue.LocalNudgeAuthority

	mu                    sync.Mutex
	recoveryCalls         int
	dropAdmissionNextCall bool
	membershipReads       chan<- struct{}
	blockRecovery         bool
	recoveryStarted       chan<- struct{}
	releaseRecovery       <-chan struct{}
	rejectedCommandID     string
	rejectedSequence      uint64
}

func (a *liveNudgeCommandRecoveryAuthority) RecoverCommandAuthority(ctx context.Context, repository *nudgequeue.CommandRepository) error {
	a.mu.Lock()
	a.recoveryCalls++
	block := a.blockRecovery
	started := a.recoveryStarted
	release := a.releaseRecovery
	a.mu.Unlock()
	if block {
		select {
		case started <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return a.LocalNudgeAuthority.RecoverCommandAuthority(ctx, repository)
}

func (a *liveNudgeCommandRecoveryAuthority) ResolveCommandPartitionMembership(ctx context.Context, request nudgequeue.CommandPartitionMembershipRequest) (nudgequeue.CommandPartitionMembership, error) {
	a.mu.Lock()
	reads := a.membershipReads
	rejectedCommandID := a.rejectedCommandID
	rejectedSequence := a.rejectedSequence
	a.mu.Unlock()
	if reads != nil {
		select {
		case reads <- struct{}{}:
		default:
		}
	}
	if request.CommandID == rejectedCommandID {
		return nudgequeue.CommandPartitionMembership{
			Store:              request.Store,
			RepositoryRevision: request.RepositoryRevision,
			SequenceHighWater:  request.SequenceHighWater,
			CommandID:          request.CommandID,
			Partition:          request.Partition,
			Found:              true,
			Rejected:           true,
			Sequence:           rejectedSequence,
		}, nil
	}
	return a.LocalNudgeAuthority.ResolveCommandPartitionMembership(ctx, request)
}

func (a *liveNudgeCommandRecoveryAuthority) RecordCommandPartitionAdmission(ctx context.Context, admission nudgequeue.CommandPartitionAdmission) error {
	a.mu.Lock()
	drop := a.dropAdmissionNextCall
	a.dropAdmissionNextCall = false
	a.mu.Unlock()
	if drop {
		return errLiveNudgeAdmissionPublicationLost
	}
	return a.LocalNudgeAuthority.RecordCommandPartitionAdmission(ctx, admission)
}

func (a *liveNudgeCommandRecoveryAuthority) dropNextAdmissionPublication() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dropAdmissionNextCall = true
}

func (a *liveNudgeCommandRecoveryAuthority) recoveryCallCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.recoveryCalls
}

func (a *liveNudgeCommandRecoveryAuthority) observeMembershipReads() <-chan struct{} {
	a.mu.Lock()
	defer a.mu.Unlock()
	reads := make(chan struct{}, 8)
	a.membershipReads = reads
	return reads
}

func (a *liveNudgeCommandRecoveryAuthority) blockLiveRecovery() (<-chan struct{}, chan struct{}) {
	a.mu.Lock()
	defer a.mu.Unlock()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	a.blockRecovery = true
	a.recoveryStarted = started
	a.releaseRecovery = release
	return started, release
}

func (a *liveNudgeCommandRecoveryAuthority) rejectMembership(commandID string, sequence uint64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.rejectedCommandID = commandID
	a.rejectedSequence = sequence
}

func liveNudgeAuthorityOptions() nudgequeue.LocalNudgeAuthorityOptions {
	return nudgequeue.LocalNudgeAuthorityOptions{
		Profile:         nudgequeue.LocalNudgeAuthorityProfileStoreWriterIsController,
		AuthorityID:     "live-recovery-authority",
		Issuer:          "live-recovery-controller",
		TenantScope:     "live-recovery-tenant",
		CityScope:       "live-recovery-city",
		CredentialClass: "live-recovery-write-grant",
		PolicyVersion:   "live-recovery-policy-v1",
	}
}

func liveNudgeRequester() nudgequeue.AuthenticatedNudgeRequester {
	return nudgequeue.AuthenticatedNudgeRequester{
		PrincipalID:     "live-recovery-principal",
		TenantScope:     "live-recovery-tenant",
		CityScope:       "live-recovery-city",
		CredentialClass: "live-recovery-write-grant",
		EvidenceID:      "live-recovery-evidence",
	}
}

func liveNudgeCommandRequest(requestID string) nudgequeue.NudgeIngressRequest {
	return nudgequeue.NudgeIngressRequest{
		RequestID: requestID,
		Mode:      nudgequeue.DeliveryModeQueue,
		Target: nudgequeue.CommandTarget{
			SessionID:            "live-recovery-session",
			IntentGeneration:     1,
			ContinuationIdentity: "live-recovery-continuation",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Source:    nudgequeue.CommandSourceSession,
		Message:   "live authority recovery proof",
		ExpiresAt: time.Now().UTC().Add(time.Hour),
	}
}

func newProductionNudgeTestAuthority() *productionNudgeTestAuthority {
	return &productionNudgeTestAuthority{
		references: make(map[string]nudgequeue.NudgeAuthorization),
		admissions: make(map[string]nudgequeue.CommandPartitionAdmission),
		terminals:  make(map[string]nudgequeue.CommandPartitionTerminal),
		intents:    make(map[nudgequeue.CommandPartitionTerminalIntent]struct{}),
		finalized:  make(map[string]nudgequeue.CommandPartitionTerminalResolution),
	}
}
