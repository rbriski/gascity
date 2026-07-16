package nudgequeue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

func TestLocalNudgeAuthorityRefusesLiveStoreAndAnchorRewindBeforeRecoveryOrClaim(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)

	// Preserve the exact admitted-pending repository generation. Restoring this
	// generation together with its matching lineage anchor must not erase the
	// independently retained authority journal's later terminal evidence.
	fixture.store.mu.Lock()
	rewoundRows := cloneRepositoryRows(fixture.store.rows)
	rewoundMetadata := cloneRepositoryMetadata(fixture.store.metadata)
	fixture.store.mu.Unlock()

	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-before-restore", OwnerID: "owner-before-restore",
		AttemptID: "attempt-before-restore", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before restore = %#v, err=%v", claimed, err)
	}
	completion := providerAttemptCompletion(claimed.Command, CommandActionResultInjectedUnconfirmed)
	terminal, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !terminal.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt before restore = %#v, err=%v", terminal, err)
	}
	if err := recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, terminal.Command); err != nil {
		t.Fatalf("record terminal authority before restore: %v", err)
	}

	// Model an out-of-band backup restore of both the command database and its
	// matching anchor. A fresh repository verifier accepts that pair; the still-
	// open independent authority journal is the remaining monotonic fence.
	fixture.store.mu.Lock()
	fixture.store.rows = cloneRepositoryRows(rewoundRows)
	fixture.store.metadata = cloneRepositoryMetadata(rewoundMetadata)
	fixture.store.mu.Unlock()
	rewoundState := commandRepositoryStateFromMetadata(t, rewoundMetadata)
	rewoundRepository, err := NewCommandRepository(fixture.store, &repositoryLineageTestVerifier{anchor: &rewoundState})
	if err != nil {
		t.Fatalf("NewCommandRepository after restore: %v", err)
	}

	if err := fixture.authority.RecoverCommandAuthority(t.Context(), rewoundRepository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority after store+anchor rewind error = %v, want authority conflict", err)
	}

	replayed, err := rewoundRepository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-after-restore", OwnerID: "owner-after-restore",
		AttemptID: "attempt-after-restore", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt.Add(time.Minute), LeaseUntil: claimAt.Add(2 * time.Minute),
	}, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("ClaimAuthorized after store+anchor rewind = %#v, err=%v; want authority conflict", replayed, err)
	}
	if replayed.Disposition == CommandClaimAllowed {
		t.Fatalf("rewound pending command regained provider-entry permission: %#v", replayed)
	}
}

func TestLocalNudgeAuthorityRecoveryRetriesNormalEffectFenceAdvanceAfterRepositoryRead(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	var claimErr error
	reader := &recoveryReaderAfterStateHook{
		CommandPartitionRecoveryReader: fixture.repository,
		hook: func() {
			claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
				CommandID: fixture.command.ID, ClaimID: "claim-during-recovery", OwnerID: "owner-during-recovery",
				AttemptID: "attempt-during-recovery", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
				ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
			}, fixture.authority, fixture.authority)
			if err != nil {
				claimErr = err
				return
			}
			if claimed.Disposition != CommandClaimAllowed {
				claimErr = errors.New("concurrent claim did not receive provider permission")
			}
		},
	}
	if err := fixture.authority.RepairCommandPartitionAdmissions(t.Context(), reader); err != nil {
		t.Fatalf("RepairCommandPartitionAdmissions across normal claim advance: %v", err)
	}
	if claimErr != nil {
		t.Fatalf("concurrent claim: %v", claimErr)
	}
}

type recoveryReaderAfterStateHook struct {
	CommandPartitionRecoveryReader
	once sync.Once
	hook func()
}

func (r *recoveryReaderAfterStateHook) State(ctx context.Context) (CommandRepositoryState, error) {
	state, err := r.CommandPartitionRecoveryReader.State(ctx)
	if err == nil {
		r.once.Do(r.hook)
	}
	return state, err
}

func TestLocalNudgeAuthorityPersistsClaimHighWaterBeforeProviderPermission(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	fixture.store.mu.Lock()
	rewoundRows := cloneRepositoryRows(fixture.store.rows)
	rewoundMetadata := cloneRepositoryMetadata(fixture.store.metadata)
	fixture.store.mu.Unlock()

	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-watermark", OwnerID: "owner-watermark",
		AttemptID: "attempt-watermark", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read authority high-water after claim: %v", err)
	}
	if highestSequence != claimed.Command.Order.Sequence || highestRevision < claimed.Command.Order.Revision {
		t.Fatalf("authority high-water after provider permission = %d/%d, want at least %d/%d",
			highestRevision, highestSequence, claimed.Command.Order.Revision, claimed.Command.Order.Sequence)
	}

	fixture.store.mu.Lock()
	fixture.store.rows = cloneRepositoryRows(rewoundRows)
	fixture.store.metadata = cloneRepositoryMetadata(rewoundMetadata)
	fixture.store.mu.Unlock()
	rewoundState := commandRepositoryStateFromMetadata(t, rewoundMetadata)
	rewoundRepository, err := NewCommandRepository(fixture.store, &repositoryLineageTestVerifier{anchor: &rewoundState})
	if err != nil {
		t.Fatalf("NewCommandRepository after claim restore: %v", err)
	}
	if err := fixture.authority.RecoverCommandAuthority(t.Context(), rewoundRepository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority after claim restore error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityClaimAcceptsStrongerConcurrentEffectFence(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	blocking := &blockingFinalizeClaimTransitionAuthority{
		LocalNudgeAuthority: fixture.authority,
		started:             make(chan struct{}),
		resume:              make(chan struct{}),
	}
	type claimResult struct {
		result CommandClaimResult
		err    error
	}
	resultCh := make(chan claimResult, 1)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	go func() {
		result, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
			CommandID: fixture.command.ID, ClaimID: "claim-before-concurrent-admission", OwnerID: "owner-concurrent-admission",
			AttemptID: "attempt-concurrent-admission", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
			ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
		}, fixture.authority, blocking)
		resultCh <- claimResult{result: result, err: err}
	}()
	select {
	case <-blocking.started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for claim effect-fence publication")
	}

	now := claimAt.Add(time.Minute)
	ingress, err := newTrustedNudgeIngressWithClock(fixture.repository, fixture.authority, func() time.Time { return now })
	if err != nil {
		close(blocking.resume)
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)
	request.RequestID = "admission-between-claim-and-effect-fence"
	if admitted, err := ingress.Admit(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), request); err != nil || !admitted.Created {
		close(blocking.resume)
		t.Fatalf("concurrent Admit = %#v, err=%v", admitted, err)
	}
	close(blocking.resume)

	var claimed claimResult
	select {
	case claimed = <-resultCh:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for claim after concurrent effect fence")
	}
	if claimed.err != nil || claimed.result.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized across stronger concurrent fence = %#v, err=%v", claimed.result, claimed.err)
	}
	highestSequence, highestRevision, err := localAuthorityObservedRepositoryHighWaters(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read final authority high-water: %v", err)
	}
	if highestSequence < 2 || highestRevision < 3 {
		t.Fatalf("stronger concurrent authority high-water regressed to %d/%d", highestRevision, highestSequence)
	}
}

type blockingFinalizeClaimTransitionAuthority struct {
	*LocalNudgeAuthority
	started chan struct{}
	resume  chan struct{}
	once    sync.Once
}

func (a *blockingFinalizeClaimTransitionAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	a.once.Do(func() { close(a.started) })
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-a.resume:
	}
	return a.LocalNudgeAuthority.FinalizeCommandClaimTransition(ctx, receipt)
}
