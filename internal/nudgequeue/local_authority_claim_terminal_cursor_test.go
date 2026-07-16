package nudgequeue

import (
	"context"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityFullRecoveryConsumesTerminalClaimResidueAndPersistsCompletedCursor(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	initialState, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("initial State: %v", err)
	}
	cityPath := t.TempDir()
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, initialState, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	now := time.Date(2026, 7, 16, 0, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(
		WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()),
		validNudgeIngressRequest(now),
	)
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	pending := *admitted.Entry.Command
	claimAt := pending.DeliverAfter.Add(time.Second)
	claimed, err := repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: pending.ID, ClaimID: "claim-terminal-cursor", OwnerID: "owner-terminal-cursor",
		AttemptID: "attempt-terminal-cursor", BoundLaunchIdentity: "launch-123", Partition: admitted.Partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, authority, authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	completed, err := repository.CompleteProviderAttempt(
		t.Context(), providerAttemptCompletion(claimed.Command, CommandActionResultInjectedUnconfirmed),
		admitted.Partition, authority,
	)
	if err != nil || !completed.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", completed, err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := recordLocalAuthorityTerminal(canceled, authority, admitted.Partition, completed.Command); err == nil {
		t.Fatal("RecordCommandPartitionTerminal error = nil, want canceled publication")
	}
	beforeGeneration, beforePreparations, beforeReceipts, err := localAuthorityClaimMutationState(t.Context(), authority.db)
	if err != nil {
		t.Fatalf("claim mutation state before recovery: %v", err)
	}
	assertLocalAuthorityTerminalClaimResidue(t, authority, 1, 0, 1)
	if beforePreparations != 0 || beforeReceipts != 1 {
		t.Fatalf("claim metadata before recovery = preparations:%d receipts:%d, want 0/1", beforePreparations, beforeReceipts)
	}

	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority: %v", err)
	}
	finalState, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	afterGeneration, afterPreparations, afterReceipts, err := localAuthorityClaimMutationState(t.Context(), authority.db)
	if err != nil {
		t.Fatalf("claim mutation state after recovery: %v", err)
	}
	if afterGeneration <= beforeGeneration {
		t.Fatalf("claim mutation generation after recovery = %d, want greater than %d", afterGeneration, beforeGeneration)
	}
	if afterPreparations != 0 || afterReceipts != 0 {
		t.Fatalf("claim metadata after recovery = preparations:%d receipts:%d, want 0/0", afterPreparations, afterReceipts)
	}
	assertLocalAuthorityTerminalClaimResidue(t, authority, 0, 0, 0)
	cursor := assertCompletedClaimAuditCursor(t, authority, finalState, afterGeneration)

	if err := authority.Close(); err != nil {
		t.Fatalf("Close authority before reopen: %v", err)
	}
	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, finalState, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority after recovery: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	assertLocalAuthorityTerminalClaimResidue(t, reopened, 0, 0, 0)
	reopenedCursor, err := reopened.readLocalAuthorityClaimAuditCursor(t.Context(), reopened.db)
	if err != nil {
		t.Fatalf("read restarted claim audit cursor after reopen: %v", err)
	}
	if reopenedCursor.phase != localAuthorityClaimAuditPreparations || reopenedCursor.repositoryRevision != finalState.Revision ||
		reopenedCursor.sequenceHighWater != finalState.SequenceHighWater || reopenedCursor.generation != afterGeneration ||
		reopenedCursor.afterCommandID != "" || reopenedCursor.afterSequence != 0 || reopenedCursor.preparationCount != 0 ||
		reopenedCursor.receiptCount != 0 || reopenedCursor.identity != initialLocalAuthorityClaimAuditIdentity() {
		t.Fatalf("claim audit cursor after reopen = %#v, want fresh startup audit bound to repository %d/%d generation %d",
			reopenedCursor, finalState.Revision, finalState.SequenceHighWater, afterGeneration)
	}
	resolution, err := terminalResolutionForCommand(completed.Command, admitted.Partition)
	if err != nil {
		t.Fatalf("terminalResolutionForCommand: %v", err)
	}
	if err := reopened.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal after reopen: %v", err)
	}
	if err := reopened.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority after reopen: %v", err)
	}
	if stableCursor := assertCompletedClaimAuditCursor(t, reopened, finalState, afterGeneration); stableCursor != cursor {
		t.Fatalf("completed claim audit cursor after reopened recovery = %#v, want %#v", stableCursor, cursor)
	}
}

func assertLocalAuthorityTerminalClaimResidue(
	t *testing.T,
	authority *LocalNudgeAuthority,
	wantTerminalPreparations int,
	wantClaimPreparations int,
	wantClaimReceipts int,
) {
	t.Helper()
	var terminalPreparations, claimPreparations, claimReceipts int
	if err := authority.db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM terminal_preparations),
		(SELECT COUNT(*) FROM claim_preparations),
		(SELECT COUNT(*) FROM claim_receipts)`).Scan(
		&terminalPreparations, &claimPreparations, &claimReceipts,
	); err != nil {
		t.Fatalf("read terminal and claim residue: %v", err)
	}
	if terminalPreparations != wantTerminalPreparations || claimPreparations != wantClaimPreparations || claimReceipts != wantClaimReceipts {
		t.Fatalf("terminal/claim residue = %d/%d/%d, want %d/%d/%d",
			terminalPreparations, claimPreparations, claimReceipts,
			wantTerminalPreparations, wantClaimPreparations, wantClaimReceipts)
	}
}

func assertCompletedClaimAuditCursor(
	t *testing.T,
	authority *LocalNudgeAuthority,
	state CommandRepositoryState,
	wantGeneration uint64,
) localAuthorityClaimAuditCursor {
	t.Helper()
	cursor, err := authority.readLocalAuthorityClaimAuditCursor(t.Context(), authority.db)
	if err != nil {
		t.Fatalf("read completed claim audit cursor: %v", err)
	}
	if cursor.phase != localAuthorityClaimAuditDone || cursor.repositoryRevision != state.Revision ||
		cursor.sequenceHighWater != state.SequenceHighWater || cursor.generation != wantGeneration ||
		cursor.preparationCount != 0 || cursor.receiptCount != 0 {
		t.Fatalf("completed claim audit cursor = %#v, want done at repository %d/%d generation %d with zero claim evidence",
			cursor, state.Revision, state.SequenceHighWater, wantGeneration)
	}
	token, done, err := authority.completedClaimAuditToken(t.Context(), state)
	if err != nil {
		t.Fatalf("completedClaimAuditToken: %v", err)
	}
	if !done || token != cursor.token() {
		t.Fatalf("completed claim audit token = %#v done:%t, want %#v done:true", token, done, cursor.token())
	}
	return cursor
}
