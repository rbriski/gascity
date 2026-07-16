package nudgequeue

import (
	"errors"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityTerminalPublicationRejectsRetainedClaimPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-terminal-preparation", OwnerID: "owner-terminal-preparation",
		AttemptID: "attempt-terminal-preparation", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	authorization, err := fixture.authority.AuthorizeNudgeClaim(t.Context(), claimAuthorizationRequest(fixture.command, request))
	if err != nil {
		t.Fatalf("AuthorizeNudgeClaim: %v", err)
	}
	claimed, err := buildAuthorizedClaim(cloneCommandValue(fixture.command), request, authorization)
	if err != nil {
		t.Fatalf("buildAuthorizedClaim: %v", err)
	}
	claimed.Order.Revision = state.Revision + 1
	claimIntent, err := commandClaimTransitionIntentFor(state, fixture.command, claimed, fixture.partition)
	if err != nil {
		t.Fatalf("commandClaimTransitionIntentFor: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), claimIntent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition: %v", err)
	}

	terminalIntent := CommandPartitionTerminalIntent{
		Store: fixture.command.Store, RepositoryBeforeRevision: claimIntent.RepositoryRevision,
		RepositoryRevision: claimIntent.RepositoryRevision + 1, CommandID: fixture.command.ID,
		Sequence: fixture.command.Order.Sequence, Partition: fixture.partition,
		BeforeCommandDigest: claimIntent.AfterCommandDigest, CommandDigest: [32]byte{0x7a},
	}
	if err := fixture.authority.PrepareCommandPartitionTerminal(t.Context(), terminalIntent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
	}
	err = fixture.authority.RecordCommandPartitionTerminal(t.Context(), CommandPartitionTerminal{
		Store: terminalIntent.Store, RepositoryRevision: terminalIntent.RepositoryRevision,
		CommandID: terminalIntent.CommandID, Sequence: terminalIntent.Sequence, Partition: terminalIntent.Partition,
	})
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecordCommandPartitionTerminal with retained claim preparation error = %v, want authority conflict", err)
	}

	membership, found, err := localAuthorityMembershipByCommand(t.Context(), fixture.authority.db, fixture.command.ID)
	if err != nil || !found {
		t.Fatalf("localAuthorityMembershipByCommand = found:%t err:%v", found, err)
	}
	if membership.terminalRevision != nil {
		t.Fatalf("terminal revision = %d, want active membership after rejected publication", *membership.terminalRevision)
	}
	if _, found, err := localAuthorityClaimPreparationByCommand(t.Context(), fixture.authority.db, fixture.command.Store, fixture.command.ID); err != nil || !found {
		t.Fatalf("claim preparation after rejected terminal = found:%t err:%v, want retained", found, err)
	}
}

func TestLocalNudgeAuthorityFinalizedTerminalReplayRejectsInjectedClaimPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-finalized-terminal-tamper", OwnerID: "owner-finalized-terminal-tamper",
		AttemptID: "attempt-finalized-terminal-tamper", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	claimIntent, err := commandClaimTransitionIntentFor(state, fixture.command, claimed.Command, fixture.partition)
	if err != nil {
		t.Fatalf("commandClaimTransitionIntentFor: %v", err)
	}
	completion := providerAttemptCompletion(claimed.Command, CommandActionResultInjectedUnconfirmed)
	terminal, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !terminal.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", terminal, err)
	}
	terminalIntent, found, err := localAuthorityPreparationByCommand(t.Context(), fixture.authority.db, fixture.command.Store, fixture.command.ID)
	if err != nil || !found {
		t.Fatalf("localAuthorityPreparationByCommand before terminal publication = found:%t err:%v", found, err)
	}
	if err := recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, terminal.Command); err != nil {
		t.Fatalf("recordLocalAuthorityTerminal: %v", err)
	}

	tx, err := fixture.authority.db.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin injected claim preparation: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := insertLocalAuthorityClaimPreparation(t.Context(), tx, claimIntent); err != nil {
		t.Fatalf("insertLocalAuthorityClaimPreparation: %v", err)
	}
	if err := advanceLocalAuthorityClaimTransitionGeneration(t.Context(), tx, 1, 0); err != nil {
		t.Fatalf("advanceLocalAuthorityClaimTransitionGeneration: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit injected claim preparation: %v", err)
	}

	err = fixture.authority.VerifyCommandPartitionTerminal(t.Context(), CommandPartitionTerminalResolution{
		Store: terminalIntent.Store, RepositoryRevision: terminalIntent.RepositoryRevision,
		CommandID: terminalIntent.CommandID, Sequence: terminalIntent.Sequence,
		Partition: terminalIntent.Partition, CommandDigest: terminalIntent.CommandDigest,
	})
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("VerifyCommandPartitionTerminal with retained claim preparation error = %v, want authority conflict", err)
	}
	if err := fixture.authority.ReleaseCommandPartitionTerminalWriter(t.Context(), terminalIntent); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("ReleaseCommandPartitionTerminalWriter with retained claim preparation error = %v, want authority conflict", err)
	}
	err = recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, terminal.Command)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("replayed finalized terminal with retained claim preparation error = %v, want authority conflict", err)
	}
}
