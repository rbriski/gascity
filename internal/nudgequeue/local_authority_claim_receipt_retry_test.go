package nudgequeue

import (
	"testing"
	"time"
)

func TestLocalNudgeAuthorityFinalizedClaimRetryAdvancesStrongerEffectFence(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-stronger-receipt-retry", OwnerID: "owner-stronger-receipt-retry",
		AttemptID: "attempt-stronger-receipt-retry", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}

	record, found, err := localAuthorityClaimReceiptByCommand(t.Context(), fixture.authority.db, fixture.command.Store, fixture.command.ID)
	if err != nil || !found {
		t.Fatalf("localAuthorityClaimReceiptByCommand = found:%t err:%v", found, err)
	}
	retry := localClaimTransitionReceipt(record)
	beforeSequence, beforeRevision, err := localAuthorityObservedRepositoryHighWaters(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("localAuthorityObservedRepositoryHighWaters before retry: %v", err)
	}
	retry.EffectSequenceHighWater = beforeSequence + 1
	retry.EffectRepositoryRevision = beforeRevision + 2

	disposition, err := fixture.authority.FinalizeCommandClaimTransition(t.Context(), retry)
	if err != nil || disposition != CommandClaimReceiptAlreadyFinalized {
		t.Fatalf("FinalizeCommandClaimTransition stronger retry = %q, err=%v; want already finalized", disposition, err)
	}
	afterSequence, afterRevision, err := localAuthorityObservedRepositoryHighWaters(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("localAuthorityObservedRepositoryHighWaters after retry: %v", err)
	}
	if afterSequence != retry.EffectSequenceHighWater || afterRevision != retry.EffectRepositoryRevision {
		t.Fatalf("authority effect fence after stronger retry = %d/%d, want %d/%d",
			afterRevision, afterSequence, retry.EffectRepositoryRevision, retry.EffectSequenceHighWater)
	}
}
