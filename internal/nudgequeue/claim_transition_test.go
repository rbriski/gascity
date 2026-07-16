package nudgequeue

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestClaimAuthorizedSuccessfulFinalizeDoesNotPerformFallibleWriterRelease(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	authority := &failClaimTransitionReleaseAuthority{
		testNudgeAuthority: fixture.authority,
		releaseErr:         errors.New("injected post-finalize claim writer release failure"),
	}
	request := fixture.claimRequest("claim-finalize-no-release", "owner-finalize-no-release", "attempt-finalize-no-release", fixture.now.Add(time.Second))

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err != nil || result.Disposition != CommandClaimAllowed || result.Command.State != CommandStateInFlight {
		t.Fatalf("ClaimAuthorized = %#v, err=%v; want allowed claim without post-finalize release", result, err)
	}
	if authority.releaseCalls != 0 {
		t.Fatalf("post-finalize claim writer release calls = %d, want 0", authority.releaseCalls)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("authority after finalized claim = preparations:%d receipts:%d, want 0/1", preparations, receipts)
	}
}

func TestClaimAuthorizedFinalizeFailureReturnsExactCommandAndRetryFinalizesOnce(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	authority := &failOnceClaimTransitionAuthority{
		testNudgeAuthority: fixture.authority,
		nextErr:            errors.New("injected claim receipt finalization failure"),
	}
	request := fixture.claimRequest("claim-finalize-retry", "owner-finalize-retry", "attempt-finalize-retry", fixture.now.Add(time.Second))

	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err == nil || first.Disposition != CommandClaimAllowed || first.Command.State != CommandStateInFlight || first.Command.Claim == nil {
		t.Fatalf("first ClaimAuthorized = %#v, err=%v; want exact allowed command plus finalization error", first, err)
	}
	resolution, getErr := fixture.repository.Get(t.Context(), fixture.command.ID)
	if getErr != nil || !resolution.Found || resolution.Entry.Command == nil || !reflect.DeepEqual(*resolution.Entry.Command, first.Command) {
		t.Fatalf("durable command after failed finalization = %#v, err=%v; want %#v", resolution, getErr, first.Command)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after failed finalization = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}

	competing := fixture.claimRequest("claim-competing", "owner-competing", "attempt-competing", fixture.now.Add(2*time.Second))
	busy, err := fixture.repository.ClaimAuthorized(t.Context(), competing, fixture.authority, authority)
	if err != nil || busy.Disposition != CommandClaimBusy || !reflect.DeepEqual(busy.Command, first.Command) {
		t.Fatalf("competing ClaimAuthorized = %#v, err=%v; want busy exact command", busy, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after competing retry = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}

	recovered, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err != nil || recovered.Disposition != CommandClaimAllowed || !reflect.DeepEqual(recovered.Command, first.Command) {
		t.Fatalf("recovered ClaimAuthorized = %#v, err=%v; want allowed %#v", recovered, err, first)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("authority after recovery = preparations:%d receipts:%d, want 0/1", preparations, receipts)
	}

	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err != nil || replayed.Disposition != CommandClaimEntryUnknown || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("finalized ClaimAuthorized retry = %#v, err=%v; want entry-unknown exact command", replayed, err)
	}
}

func TestClaimAuthorizedContextCancellationDuringFinalizeReturnsExactCommand(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	authority := &cancelOnceClaimTransitionAuthority{testNudgeAuthority: fixture.authority, cancel: cancel}
	request := fixture.claimRequest("claim-canceled-finalize", "owner-canceled-finalize", "attempt-canceled-finalize", fixture.now.Add(time.Second))

	first, err := fixture.repository.ClaimAuthorized(ctx, request, fixture.authority, authority)
	if !errors.Is(err, context.Canceled) || first.Disposition != CommandClaimAllowed || first.Command.State != CommandStateInFlight {
		t.Fatalf("ClaimAuthorized canceled finalization = %#v, err=%v; want exact allowed command plus context cancellation", first, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after canceled finalization = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}

	recovered, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || recovered.Disposition != CommandClaimAllowed || !reflect.DeepEqual(recovered.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after canceled finalization = %#v, err=%v; want allowed exact command", recovered, err)
	}
}

func TestClaimAuthorizedLineageFailureReturnsExactPreparedCommand(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	verifier, ok := fixture.repository.writer.(*repositoryLineageTestVerifier)
	if !ok {
		t.Fatalf("repository writer = %T, want test verifier", fixture.repository.writer)
	}
	verifier.failNextAdvance(errors.New("injected post-claim lineage failure"))
	request := fixture.claimRequest("claim-lineage-failure", "owner-lineage-failure", "attempt-lineage-failure", fixture.now.Add(time.Second))

	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err == nil || first.Disposition != CommandClaimAllowed || first.Command.State != CommandStateInFlight || first.Command.Claim == nil {
		t.Fatalf("ClaimAuthorized lineage failure = %#v, err=%v; want exact allowed command plus error", first, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after lineage failure = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}
	recovered, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || recovered.Disposition != CommandClaimAllowed || !reflect.DeepEqual(recovered.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after lineage repair = %#v, err=%v; want allowed exact command", recovered, err)
	}
}

func TestClaimAuthorizedReplacesStalePendingPreparationInsideStoreTransaction(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	staleRequest := fixture.claimRequest("claim-stale-preparation", "owner-stale-preparation", "attempt-stale-preparation", fixture.now.Add(time.Second))
	staleAuthorization, err := fixture.authority.AuthorizeNudgeClaim(t.Context(), claimAuthorizationRequest(fixture.command, staleRequest))
	if err != nil {
		t.Fatalf("AuthorizeNudgeClaim stale: %v", err)
	}
	staleAfter, err := buildAuthorizedClaim(cloneCommandValue(fixture.command), staleRequest, staleAuthorization)
	if err != nil {
		t.Fatalf("buildAuthorizedClaim stale: %v", err)
	}
	staleAfter.Order.Revision = state.Revision + 1
	staleIntent, err := commandClaimTransitionIntentFor(state, fixture.command, staleAfter, fixture.partition)
	if err != nil {
		t.Fatalf("commandClaimTransitionIntentFor stale: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), staleIntent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition stale: %v", err)
	}

	request := fixture.claimRequest("claim-replacement", "owner-replacement", "attempt-replacement", fixture.now.Add(2*time.Second))
	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || result.Disposition != CommandClaimAllowed || result.Command.Claim == nil || result.Command.Claim.ID != request.ClaimID {
		t.Fatalf("replacement ClaimAuthorized = %#v, err=%v", result, err)
	}
	if preparations, receipts := fixture.authority.claimTransitionCounts(); preparations != 0 || receipts != 1 {
		t.Fatalf("authority after stale replacement = preparations:%d receipts:%d, want 0/1", preparations, receipts)
	}
}

func TestCommandClaimTransitionIntentRejectsPreexistingBindingSubstitution(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	before := cloneCommandValue(fixture.command)
	before.Binding = &CommandBinding{LaunchIdentity: "launch-original", BoundAt: before.DeliverAfter}
	request := fixture.claimRequest("claim-binding-substitution", "owner-binding-substitution", "attempt-binding-substitution", fixture.now.Add(time.Second))
	request.BoundLaunchIdentity = before.Binding.LaunchIdentity
	authorization, err := fixture.authority.AuthorizeNudgeClaim(t.Context(), claimAuthorizationRequest(before, request))
	if err != nil {
		t.Fatalf("AuthorizeNudgeClaim: %v", err)
	}
	after, err := buildAuthorizedClaim(before, request, authorization)
	if err != nil {
		t.Fatalf("buildAuthorizedClaim: %v", err)
	}
	after.Order.Revision = state.Revision + 1
	after.Binding = &CommandBinding{LaunchIdentity: "launch-substituted", BoundAt: before.DeliverAfter}
	after.Claim.BoundLaunchIdentity = after.Binding.LaunchIdentity
	after.Retry.BoundLaunchIdentity = after.Binding.LaunchIdentity

	if _, err := commandClaimTransitionIntentFor(state, before, after, fixture.partition); !errors.Is(err, ErrCommandClaimTransition) {
		t.Fatalf("commandClaimTransitionIntentFor binding substitution error = %v, want claim-transition refusal", err)
	}
}

func TestLocalNudgeAuthorityClaimPreparationTamperFailsClosed(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	authority := &failOnceLocalClaimTransitionAuthority{
		LocalNudgeAuthority: fixture.authority,
		nextErr:             errors.New("injected claim receipt finalization failure"),
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-preparation-tamper", OwnerID: "owner-preparation-tamper",
		AttemptID: "attempt-preparation-tamper", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err == nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before preparation tamper = %#v, err=%v", first, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `UPDATE claim_preparations SET authorization_policy_version = 'tampered-policy' WHERE command_id = ?`, fixture.command.ID); err != nil {
		t.Fatalf("tamper claim preparation: %v", err)
	}

	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) || replayed.Disposition != CommandClaimAllowed || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after preparation tamper = %#v, err=%v; want exact result plus authority conflict", replayed, err)
	}
}

func TestLocalNudgeAuthorityClaimReceiptTamperFailsClosed(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-receipt-tamper", OwnerID: "owner-receipt-tamper",
		AttemptID: "attempt-receipt-tamper", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before receipt tamper = %#v, err=%v", first, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `UPDATE claim_receipts SET authorization_decision_id = 'tampered-decision' WHERE command_id = ?`, fixture.command.ID); err != nil {
		t.Fatalf("tamper claim receipt: %v", err)
	}

	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) || replayed.Disposition != CommandClaimAllowed || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after receipt tamper = %#v, err=%v; want exact result plus authority conflict", replayed, err)
	}
}

func TestLocalNudgeAuthorityClaimReceiptEffectCannotTrailPreparedSequenceHighWater(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-receipt-effect-tamper", OwnerID: "owner-receipt-effect-tamper",
		AttemptID: "attempt-receipt-effect-tamper", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	if result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority); err != nil || result.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before receipt effect tamper = %#v, err=%v", result, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `UPDATE claim_receipts SET
		repository_before_revision = ?, claim_revision = ?, sequence_high_water = ?,
		effect_revision = ?, effect_sequence_high_water = ? WHERE command_id = ?`,
		encodeLocalAuthorityUint64(3), encodeLocalAuthorityUint64(4), encodeLocalAuthorityUint64(3),
		encodeLocalAuthorityUint64(4), encodeLocalAuthorityUint64(2), fixture.command.ID); err != nil {
		t.Fatalf("tamper claim receipt effect: %v", err)
	}
	if _, _, err := localAuthorityClaimReceiptByCommand(t.Context(), fixture.authority.db, fixture.command.Store, fixture.command.ID); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("read claim receipt with effect behind preparation error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityClaimPreparationAndReceiptCoexistenceFailsClosed(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-dual-row-tamper", OwnerID: "owner-dual-row-tamper",
		AttemptID: "attempt-dual-row-tamper", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before dual-row tamper = %#v, err=%v", first, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `INSERT INTO claim_preparations (
		command_id, sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until)
		SELECT command_id, sequence, partition_id, repository_before_revision, claim_revision, sequence_high_water,
		before_digest, after_digest, claim_id, owner_id, operation_id, attempt_id, bound_launch_identity,
		authorization_decision_id, authorization_policy_version, claimed_at, lease_until
		FROM claim_receipts WHERE command_id = ?`, fixture.command.ID); err != nil {
		t.Fatalf("insert dual claim transition rows: %v", err)
	}

	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) || replayed.Disposition != CommandClaimAllowed || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("ClaimAuthorized with preparation+receipt = %#v, err=%v; want exact result plus authority conflict", replayed, err)
	}
}

func TestLocalNudgeAuthorityRecoveryAbortsPendingClaimPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-pending-recovery", OwnerID: "owner-pending-recovery",
		AttemptID: "attempt-pending-recovery", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	authorization, err := fixture.authority.AuthorizeNudgeClaim(t.Context(), claimAuthorizationRequest(fixture.command, request))
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
		t.Fatalf("PrepareCommandClaimTransition: %v", err)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter: %v", err)
	}

	if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); err != nil {
		t.Fatalf("RecoverCommandAuthority: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 0 || receipts != 0 {
		t.Fatalf("authority after pending recovery = preparations:%d receipts:%d, want 0/0", preparations, receipts)
	}
}

func TestLocalNudgeAuthorityRecoveryRetainsExactInFlightClaimPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	authority := &failOnceLocalClaimTransitionAuthority{
		LocalNudgeAuthority: fixture.authority,
		nextErr:             errors.New("injected claim receipt finalization failure"),
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-inflight-recovery", OwnerID: "owner-inflight-recovery",
		AttemptID: "attempt-inflight-recovery", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err == nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before recovery = %#v, err=%v", first, err)
	}

	if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); err != nil {
		t.Fatalf("RecoverCommandAuthority: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 1 || receipts != 0 {
		t.Fatalf("authority after in-flight recovery = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}
	recovered, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || recovered.Disposition != CommandClaimAllowed || !reflect.DeepEqual(recovered.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after recovery = %#v, err=%v; want allowed exact command", recovered, err)
	}
	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || replayed.Disposition != CommandClaimEntryUnknown || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("ClaimAuthorized after finalized recovery = %#v, err=%v; want entry unknown", replayed, err)
	}
}

func claimAuthorizationRequest(command Command, request CommandClaimRequest) NudgeClaimAuthorizationRequest {
	return NudgeClaimAuthorizationRequest{
		Command: cloneCommandValue(command), Partition: request.Partition, ClaimID: request.ClaimID,
		OwnerID: request.OwnerID, AttemptID: request.AttemptID, BoundLaunchIdentity: request.BoundLaunchIdentity,
		ClaimedAt: request.ClaimedAt, LeaseUntil: request.LeaseUntil,
	}
}

type failOnceClaimTransitionAuthority struct {
	*testNudgeAuthority
	mu      sync.Mutex
	nextErr error
}

type failClaimTransitionReleaseAuthority struct {
	*testNudgeAuthority
	releaseErr   error
	releaseCalls int
}

func (a *failClaimTransitionReleaseAuthority) ReleaseCommandClaimTransitionWriter(context.Context, CommandClaimTransitionIntent) error {
	a.releaseCalls++
	return a.releaseErr
}

func (a *failOnceClaimTransitionAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	a.mu.Lock()
	err := a.nextErr
	a.nextErr = nil
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	return a.testNudgeAuthority.FinalizeCommandClaimTransition(ctx, receipt)
}

type cancelOnceClaimTransitionAuthority struct {
	*testNudgeAuthority
	mu     sync.Mutex
	cancel context.CancelFunc
}

func (a *cancelOnceClaimTransitionAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	a.mu.Lock()
	cancel := a.cancel
	a.cancel = nil
	a.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	return a.testNudgeAuthority.FinalizeCommandClaimTransition(ctx, receipt)
}

type failOnceLocalClaimTransitionAuthority struct {
	*LocalNudgeAuthority
	mu      sync.Mutex
	nextErr error
}

func (a *failOnceLocalClaimTransitionAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	a.mu.Lock()
	err := a.nextErr
	a.nextErr = nil
	a.mu.Unlock()
	if err != nil {
		return "", err
	}
	return a.LocalNudgeAuthority.FinalizeCommandClaimTransition(ctx, receipt)
}

func localClaimTransitionCounts(t *testing.T, authority *LocalNudgeAuthority) (int, int) {
	t.Helper()
	var preparations, receipts int
	if err := authority.db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM claim_preparations),
		(SELECT COUNT(*) FROM claim_receipts)`).Scan(&preparations, &receipts); err != nil {
		t.Fatalf("read claim transition counts: %v", err)
	}
	return preparations, receipts
}
