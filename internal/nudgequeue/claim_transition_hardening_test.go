package nudgequeue

import (
	"errors"
	"testing"
	"time"
)

func TestCommandClaimTransitionRejectsPreexistingBindingSubstitution(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	before := cloneCommandValue(fixture.command)
	before.Binding = &CommandBinding{
		LaunchIdentity: "launch-before-claim",
		BoundAt:        fixture.now,
	}
	request := fixture.claimRequest(
		"claim-binding-substitution",
		"owner-binding-substitution",
		"attempt-binding-substitution",
		fixture.now.Add(time.Second),
	)
	authorization, err := fixture.authority.AuthorizeNudgeClaim(
		t.Context(),
		claimAuthorizationRequest(before, request),
	)
	if err != nil {
		t.Fatalf("AuthorizeNudgeClaim: %v", err)
	}

	// Construct an otherwise valid after-state whose claim evidence is
	// self-consistent but which silently replaces an already durable binding.
	unbound := cloneCommandValue(before)
	unbound.Binding = nil
	after, err := buildAuthorizedClaim(unbound, request, authorization)
	if err != nil {
		t.Fatalf("buildAuthorizedClaim: %v", err)
	}
	after.Order.Revision = state.Revision + 1
	if _, err := EncodeCommandV1(before); err != nil {
		t.Fatalf("EncodeCommandV1 before: %v", err)
	}
	if _, err := EncodeCommandV1(after); err != nil {
		t.Fatalf("EncodeCommandV1 after: %v", err)
	}

	if _, err := commandClaimTransitionIntentFor(state, before, after, fixture.partition); err == nil {
		t.Fatal("commandClaimTransitionIntentFor accepted a substituted pre-existing binding")
	} else if !errors.Is(err, ErrCommandClaimTransition) {
		t.Fatalf("commandClaimTransitionIntentFor error = %v, want ErrCommandClaimTransition", err)
	}
}

func TestLocalNudgeAuthorityAbortRetainsPreparationForAnotherExactWriter(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-shared-preparation", OwnerID: "owner-shared-preparation",
		AttemptID: "attempt-shared-preparation", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	authorization, err := fixture.authority.AuthorizeNudgeClaim(
		t.Context(),
		claimAuthorizationRequest(fixture.command, request),
	)
	if err != nil {
		t.Fatalf("AuthorizeNudgeClaim: %v", err)
	}
	after, err := buildAuthorizedClaim(cloneCommandValue(fixture.command), request, authorization)
	if err != nil {
		t.Fatalf("buildAuthorizedClaim: %v", err)
	}
	after.Order.Revision = state.Revision + 1
	intent, err := commandClaimTransitionIntentFor(state, fixture.command, after, fixture.partition)
	if err != nil {
		t.Fatalf("commandClaimTransitionIntentFor: %v", err)
	}

	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition first writer: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition second writer: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after exact writers = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}

	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition first writer: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after one exact writer aborts = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter first writer: %v", err)
	}

	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition last writer: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 0 || receipts != 0 {
		t.Fatalf("authority after last exact writer aborts = preparations:%d receipts:%d, want 0/0", preparations, receipts)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter last writer: %v", err)
	}
}
