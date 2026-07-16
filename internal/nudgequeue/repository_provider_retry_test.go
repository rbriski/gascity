package nudgequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestCommandRepositoryRetryProviderAttemptCommitsDefiniteNonEntryAndConsumesClaimReceipt(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	claimRequest := fixture.claimRequest(
		"claim-retry-1",
		"owner-retry-1",
		"attempt-retry-1",
		fixture.now.Add(time.Second),
	)
	claimed, err := fixture.repository.ClaimAuthorized(
		t.Context(),
		claimRequest,
		fixture.authority,
		fixture.authority,
	)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v; want allowed", claimed, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("claim authority before retry = preparations %d receipts %d, want 0/1", preparations, receipts)
	}

	nextEligibleAt := claimRequest.ClaimedAt.Add(250 * time.Millisecond)
	retried, err := fixture.repository.RetryProviderAttempt(t.Context(), CommandRetryRequest{
		CommandID:      fixture.command.ID,
		ClaimID:        claimRequest.ClaimID,
		OperationID:    fixture.command.ID,
		AttemptID:      claimRequest.AttemptID,
		ObservedAt:     claimRequest.ClaimedAt.Add(100 * time.Millisecond),
		NextEligibleAt: nextEligibleAt,
		ErrorClass:     CommandErrorClassProviderBusy,
		Detail:         "provider proved that native nudge entry did not occur",
		ProviderStage:  ProviderStageNotEntered,
		Completion:     CompletionStateNotCompleted,
	}, fixture.partition, fixture.authority)
	if err != nil {
		t.Fatalf("RetryProviderAttempt: %v", err)
	}
	if retried.Disposition != CommandRetryRecorded || !retried.HasRetryTransitionWitness() {
		t.Fatalf("retry result = %#v, want recorded with transition witness", retried)
	}
	command := retried.Command
	if command.State != CommandStatePending || command.Claim != nil || command.Terminal != nil || command.Retry == nil {
		t.Fatalf("retried command = %#v, want pending without claim/terminal", command)
	}
	if command.Retry.AttemptCount != 1 || command.Retry.AttemptID != claimRequest.AttemptID ||
		command.Retry.NextEligibleAt == nil || !command.Retry.NextEligibleAt.Equal(nextEligibleAt) ||
		command.Retry.ErrorClass != CommandErrorClassProviderBusy {
		t.Fatalf("retry evidence = %#v, want exact definite-non-entry attempt", command.Retry)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 0 {
		t.Fatalf("claim authority after retry = preparations %d receipts %d, want exact receipt consumed", preparations, receipts)
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("retry authority after retry = preparations %d receipts %d, want 0/1", preparations, receipts)
	}
	fixture.authority.mu.Lock()
	receipt := fixture.authority.retryReceipts[testRetryReceiptKey(fixture.command.ID, claimRequest.AttemptID)]
	fixture.authority.mu.Unlock()
	if receipt.ProviderStage != ProviderStageNotEntered || receipt.Completion != CompletionStateNotCompleted ||
		receipt.Claim.OwnerID != claimRequest.OwnerID || !receipt.Claim.LeaseUntil.Equal(claimRequest.LeaseUntil) {
		t.Fatalf("independent retry receipt = %#v, want exact full claim and definite non-entry evidence", receipt)
	}

	repeated, err := fixture.repository.RetryProviderAttempt(t.Context(), CommandRetryRequest{
		CommandID:      fixture.command.ID,
		ClaimID:        claimRequest.ClaimID,
		OperationID:    fixture.command.ID,
		AttemptID:      claimRequest.AttemptID,
		ObservedAt:     claimRequest.ClaimedAt.Add(100 * time.Millisecond),
		NextEligibleAt: nextEligibleAt,
		ErrorClass:     CommandErrorClassProviderBusy,
		Detail:         "provider proved that native nudge entry did not occur",
		ProviderStage:  ProviderStageNotEntered,
		Completion:     CompletionStateNotCompleted,
	}, fixture.partition, fixture.authority)
	if err != nil || repeated.Disposition != CommandRetryAlreadyRecorded || !repeated.HasRetryTransitionWitness() {
		t.Fatalf("repeated RetryProviderAttempt = %#v, err=%v; want already-recorded witness", repeated, err)
	}
}

func TestCommandRepositoryRetryProviderAttemptRejectsUnsafeOutcomesBeforeMutation(t *testing.T) {
	for _, test := range []struct {
		name       string
		stage      ProviderStage
		completion CompletionState
		errorClass CommandErrorClass
	}{
		{name: "may have entered", stage: ProviderStageMayHaveEntered, completion: CompletionStateUnknown, errorClass: CommandErrorClassProviderAmbiguous},
		{name: "accepted", stage: ProviderStageAccepted, completion: CompletionStateCompleted, errorClass: CommandErrorClassProviderBusy},
		{name: "rejected", stage: ProviderStageNotEntered, completion: CompletionStateNotCompleted, errorClass: CommandErrorClassProviderRejected},
		{name: "target changed", stage: ProviderStageNotEntered, completion: CompletionStateNotCompleted, errorClass: CommandErrorClassTargetMissing},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, claimRequest := authorizedRetryFixture(t)
			before, err := fixture.repository.State(t.Context())
			if err != nil {
				t.Fatalf("State before retry: %v", err)
			}
			request := retryRequestForClaim(fixture, claimRequest)
			request.ProviderStage = test.stage
			request.Completion = test.completion
			request.ErrorClass = test.errorClass

			if _, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, fixture.authority); !errors.Is(err, ErrCommandProviderRetryInvalid) {
				t.Fatalf("RetryProviderAttempt error = %v, want ErrCommandProviderRetryInvalid", err)
			}
			after, err := fixture.repository.State(t.Context())
			if err != nil || after != before {
				t.Fatalf("State after rejected retry = %#v, err=%v; want %#v", after, err, before)
			}
			resolved, err := fixture.repository.Get(t.Context(), fixture.command.ID)
			if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.State != CommandStateInFlight {
				t.Fatalf("command after rejected retry = %#v, err=%v", resolved, err)
			}
			if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 0 || receipts != 0 {
				t.Fatalf("retry authority after rejected outcome = %d/%d, want 0/0", preparations, receipts)
			}
			if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
				t.Fatalf("claim authority after rejected outcome = %d/%d, want 0/1", preparations, receipts)
			}
		})
	}
}

func TestCommandRepositoryRetryProviderAttemptLeavesStaleAttemptUnchanged(t *testing.T) {
	fixture, claimRequest := authorizedRetryFixture(t)
	request := retryRequestForClaim(fixture, claimRequest)
	request.AttemptID = "different-attempt"

	result, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, fixture.authority)
	if err != nil || result.Disposition != CommandRetryStale || result.Command.State != CommandStateInFlight {
		t.Fatalf("RetryProviderAttempt stale = %#v, err=%v", result, err)
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 0 || receipts != 0 {
		t.Fatalf("retry authority after stale attempt = %d/%d, want 0/0", preparations, receipts)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("claim authority after stale attempt = %d/%d, want 0/1", preparations, receipts)
	}
}

func TestCommandRepositoryRetryProviderAttemptAbortsPreparationOnDefiniteRollback(t *testing.T) {
	fixture, claimRequest := authorizedRetryFixture(t)
	fixture.store.failNextCommandUpdate(errors.New("injected retry update failure"))

	if _, err := fixture.repository.RetryProviderAttempt(t.Context(), retryRequestForClaim(fixture, claimRequest), fixture.partition, fixture.authority); err == nil {
		t.Fatal("RetryProviderAttempt error = nil, want update failure")
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 0 || receipts != 0 {
		t.Fatalf("retry authority after rollback = %d/%d, want 0/0", preparations, receipts)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("claim authority after rollback = %d/%d, want 0/1", preparations, receipts)
	}
}

func TestCommandRepositoryRetryProviderAttemptRetainsPreparationAcrossAmbiguousRollback(t *testing.T) {
	fixture, claimRequest := authorizedRetryFixture(t)
	request := retryRequestForClaim(fixture, claimRequest)
	fixture.store.failNextCommit(errors.New("retry commit outcome unavailable"))

	if _, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, fixture.authority); err == nil {
		t.Fatal("RetryProviderAttempt error = nil, want ambiguous commit")
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("retry authority after ambiguous rollback = %d/%d, want 1/0", preparations, receipts)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("claim authority after ambiguous rollback = %d/%d, want 0/1", preparations, receipts)
	}

	recovered, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, fixture.authority)
	if err != nil || recovered.Disposition != CommandRetryRecorded || !recovered.HasRetryTransitionWitness() {
		t.Fatalf("RetryProviderAttempt recovery = %#v, err=%v", recovered, err)
	}
}

func TestCommandRepositoryRetryProviderAttemptResolvesLostStoreResponse(t *testing.T) {
	fixture, claimRequest := authorizedRetryFixture(t)
	fixture.store.mu.Lock()
	fixture.store.failAfterCommitNext = errors.New("lost retry store response")
	fixture.store.mu.Unlock()

	result, err := fixture.repository.RetryProviderAttempt(t.Context(), retryRequestForClaim(fixture, claimRequest), fixture.partition, fixture.authority)
	if err != nil || result.Disposition != CommandRetryAlreadyRecorded || !result.HasRetryTransitionWitness() {
		t.Fatalf("RetryProviderAttempt lost response = %#v, err=%v", result, err)
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("retry authority after lost response = %d/%d, want 0/1", preparations, receipts)
	}
}

func TestCommandRepositoryRetryProviderAttemptRecoversFinalizationFailureWithoutClaimReconstruction(t *testing.T) {
	fixture, claimRequest := authorizedRetryFixture(t)
	wantErr := errors.New("authority temporarily unavailable")
	authority := &failOnceRetryFinalizeAuthority{testNudgeAuthority: fixture.authority, err: wantErr}
	request := retryRequestForClaim(fixture, claimRequest)

	if _, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, authority); !errors.Is(err, wantErr) {
		t.Fatalf("RetryProviderAttempt first error = %v, want authority failure", err)
	}
	resolved, err := fixture.repository.Get(t.Context(), fixture.command.ID)
	if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.State != CommandStatePending {
		t.Fatalf("command after finalization failure = %#v, err=%v", resolved, err)
	}
	if preparations, receipts := fixture.authority.retryTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("retry authority after finalization failure = %d/%d, want 1/0", preparations, receipts)
	}

	recovered, err := fixture.repository.RetryProviderAttempt(t.Context(), request, fixture.partition, authority)
	if err != nil || recovered.Disposition != CommandRetryAlreadyRecorded || !recovered.HasRetryTransitionWitness() {
		t.Fatalf("RetryProviderAttempt finalized recovery = %#v, err=%v", recovered, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 0 {
		t.Fatalf("claim authority after finalized recovery = %d/%d, want 0/0", preparations, receipts)
	}
}

type failOnceRetryFinalizeAuthority struct {
	*testNudgeAuthority
	err error
}

func (a *failOnceRetryFinalizeAuthority) FinalizeCommandRetryTransition(ctx context.Context, commit CommandRetryTransitionCommit) (CommandRetryReceiptDisposition, error) {
	if a.err != nil {
		err := a.err
		a.err = nil
		return "", err
	}
	return a.testNudgeAuthority.FinalizeCommandRetryTransition(ctx, commit)
}

func authorizedRetryFixture(t *testing.T) (*authorizedClaimFixture, CommandClaimRequest) {
	t.Helper()
	fixture := newAuthorizedClaimFixture(t)
	claimRequest := fixture.claimRequest("claim-retry", "owner-retry", "attempt-retry", fixture.now.Add(time.Second))
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), claimRequest, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v; want allowed", claimed, err)
	}
	return fixture, claimRequest
}

func retryRequestForClaim(fixture *authorizedClaimFixture, claimRequest CommandClaimRequest) CommandRetryRequest {
	observedAt := claimRequest.ClaimedAt.Add(100 * time.Millisecond)
	return CommandRetryRequest{
		CommandID: fixture.command.ID, ClaimID: claimRequest.ClaimID, OperationID: fixture.command.ID,
		AttemptID: claimRequest.AttemptID, ObservedAt: observedAt, NextEligibleAt: observedAt.Add(250 * time.Millisecond),
		ErrorClass: CommandErrorClassProviderBusy, Detail: "provider proved that native nudge entry did not occur",
		ProviderStage: ProviderStageNotEntered, Completion: CompletionStateNotCompleted,
	}
}
