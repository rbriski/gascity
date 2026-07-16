package nudgequeue

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityTerminalRecoveryPreservesLiveWriterPreparationUntilCommit(t *testing.T) {
	authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-live-writer-race")
	terminal := localAuthorityDeadLetteredCommand(t, pending)
	intent, err := terminalIntentForTransition(state.Revision, pending, terminal, partition)
	if err != nil {
		t.Fatalf("terminalIntentForTransition: %v", err)
	}
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
	}

	// Recovery observes the last committed repository generation while the
	// writer's terminal transaction is still in flight. That observation must
	// not destroy the writer-owned preparation: the transaction may commit
	// immediately after this recovery pass.
	if err := authority.RepairCommandPartitionTerminals(
		t.Context(),
		localAuthorityRecoveryReaderFor(state, pending),
	); err != nil {
		t.Fatalf("RepairCommandPartitionTerminals(before commit): %v", err)
	}

	// Model the repository commit winning after recovery read the before-state.
	// The store call has now returned, so its in-memory writer ownership ends
	// while the durable preparation remains for recovery.
	if err := authority.ReleaseCommandPartitionTerminalWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandPartitionTerminalWriter: %v", err)
	}
	// A subsequent recovery pass must still have enough independent authority
	// evidence to finalize terminal membership and consume the preparation.
	if err := authority.RepairCommandPartitionTerminals(
		t.Context(),
		localAuthorityRecoveryReaderFor(state, terminal),
	); err != nil {
		t.Fatalf("RepairCommandPartitionTerminals(after commit): %v", err)
	}
	resolution, err := terminalResolutionForCommand(terminal, partition)
	if err != nil {
		t.Fatalf("terminalResolutionForCommand: %v", err)
	}
	if err := authority.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal: %v", err)
	}

	var preparations int
	if err := authority.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM terminal_preparations`).Scan(&preparations); err != nil {
		t.Fatalf("count terminal preparations: %v", err)
	}
	if preparations != 0 {
		t.Fatalf("terminal preparations after converged recovery = %d, want 0", preparations)
	}
}

func TestLocalNudgeAuthorityTerminalPublicationCanFinalizeWhileRecoveryReadsRepository(t *testing.T) {
	authority, state, pending, partition := localAuthorityPendingCommand(t, "request-terminal-publication-race")
	terminal := localAuthorityDeadLetteredCommand(t, pending)
	intent, err := terminalIntentForTransition(state.Revision, pending, terminal, partition)
	if err != nil {
		t.Fatalf("terminalIntentForTransition: %v", err)
	}
	if err := authority.PrepareCommandPartitionTerminal(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandPartitionTerminal: %v", err)
	}

	reader := &blockingTerminalRecoveryReader{
		localAuthorityRecoveryReader: localAuthorityRecoveryReaderFor(state, pending),
		observed:                     make(chan struct{}),
		resume:                       make(chan struct{}),
	}
	recoveryDone := make(chan error, 1)
	go func() {
		recoveryDone <- authority.RepairCommandPartitionTerminals(t.Context(), reader)
	}()
	<-reader.observed

	// The authority must not hold its lifetime or ownership locks while the
	// recovery reader is outside the authority journal. Publication therefore
	// remains able to finalize the repository commit without deadlocking.
	if err := authority.RecordCommandPartitionTerminal(t.Context(), CommandPartitionTerminal{
		Store: intent.Store, RepositoryRevision: intent.RepositoryRevision,
		CommandID: intent.CommandID, Sequence: intent.Sequence, Partition: intent.Partition,
	}); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal while recovery read is blocked: %v", err)
	}
	if err := authority.ReleaseCommandPartitionTerminalWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandPartitionTerminalWriter: %v", err)
	}
	close(reader.resume)
	if err := <-recoveryDone; err != nil {
		t.Fatalf("RepairCommandPartitionTerminals after concurrent publication: %v", err)
	}

	resolution, err := terminalResolutionForCommand(terminal, partition)
	if err != nil {
		t.Fatalf("terminalResolutionForCommand: %v", err)
	}
	if err := authority.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
		t.Fatalf("VerifyCommandPartitionTerminal: %v", err)
	}
}

type blockingTerminalRecoveryReader struct {
	localAuthorityRecoveryReader
	observed chan struct{}
	resume   chan struct{}
}

func TestLocalNudgeAuthorityProviderCompletionKeepsWriterOwnedUntilTerminalPublication(t *testing.T) {
	fixture := newLocalAuthorityProviderAttemptFixture(t)
	completion := providerAttemptCompletion(fixture.command, CommandActionResultInjectedUnconfirmed)
	result, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !result.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", result, err)
	}
	intent, err := terminalIntentForTransition(fixture.command.Order.Revision, fixture.command, result.Command, fixture.partition)
	if err != nil {
		t.Fatalf("terminalIntentForTransition: %v", err)
	}
	owned, err := fixture.authority.terminalWriterOwnsIntent(t.Context(), intent)
	if err != nil || !owned {
		t.Fatalf("terminal writer ownership after repository return = %t, err=%v; want retained through publication", owned, err)
	}

	if err := fixture.authority.RecordCommandPartitionTerminal(t.Context(), CommandPartitionTerminal{
		Store: intent.Store, RepositoryRevision: intent.RepositoryRevision,
		CommandID: intent.CommandID, Sequence: intent.Sequence, Partition: intent.Partition,
	}); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal: %v", err)
	}
	owned, err = fixture.authority.terminalWriterOwnsIntent(t.Context(), intent)
	if err != nil || owned {
		t.Fatalf("terminal writer ownership after publication = %t, err=%v; want released", owned, err)
	}
}

func TestLocalNudgeAuthorityClaimTerminalPathsKeepWriterOwnedUntilPublication(t *testing.T) {
	for _, test := range []struct {
		name       string
		claimAt    func(Command) time.Time
		authorizer func(*LocalNudgeAuthority) NudgeClaimAuthorizer
	}{
		{
			name:    "expired",
			claimAt: func(command Command) time.Time { return command.ExpiresAt },
			authorizer: func(authority *LocalNudgeAuthority) NudgeClaimAuthorizer {
				return authority
			},
		},
		{
			name:    "policy-denied",
			claimAt: func(command Command) time.Time { return command.DeliverAfter.Add(time.Second) },
			authorizer: func(authority *LocalNudgeAuthority) NudgeClaimAuthorizer {
				return denyingLocalNudgeClaimAuthorizer{delegate: authority}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newLocalAuthorityPendingFixture(t)
			claimAt := test.claimAt(fixture.command)
			result, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
				CommandID: fixture.command.ID, ClaimID: "claim-" + test.name, OwnerID: "owner-" + test.name,
				AttemptID: "attempt-" + test.name, BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
				ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
			}, test.authorizer(fixture.authority), fixture.authority)
			if err != nil || result.Disposition != CommandClaimDenied || !result.HasTerminalTransitionWitness() {
				t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
			}
			intent, err := terminalIntentForTransition(fixture.command.Order.Revision, fixture.command, result.Command, fixture.partition)
			if err != nil {
				t.Fatalf("terminalIntentForTransition: %v", err)
			}
			owned, err := fixture.authority.terminalWriterOwnsIntent(t.Context(), intent)
			if err != nil || !owned {
				t.Fatalf("terminal writer ownership after claim return = %t, err=%v; want retained", owned, err)
			}
			if err := recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, result.Command); err != nil {
				t.Fatalf("RecordCommandPartitionTerminal: %v", err)
			}
			owned, err = fixture.authority.terminalWriterOwnsIntent(t.Context(), intent)
			if err != nil || owned {
				t.Fatalf("terminal writer ownership after claim publication = %t, err=%v; want released", owned, err)
			}
		})
	}
}

func TestLocalNudgeAuthorityProviderCompletionReleasesAmbiguousRollbackForRecovery(t *testing.T) {
	fixture := newLocalAuthorityProviderAttemptFixture(t)
	fixture.store.failNextCommit(errors.New("ambiguous completion commit outcome"))
	completion := providerAttemptCompletion(fixture.command, CommandActionResultInjectedUnconfirmed)

	if _, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority); err == nil {
		t.Fatal("CompleteProviderAttempt error = nil, want ambiguous commit error")
	}
	if owners := fixture.terminalOwnerCount(); owners != 0 {
		t.Fatalf("terminal writer owners after ambiguous rollback = %d, want 0", owners)
	}
	if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); err != nil {
		t.Fatalf("RecoverCommandAuthority after ambiguous rollback: %v", err)
	}
	if preparations := fixture.terminalPreparationCount(); preparations != 0 {
		t.Fatalf("terminal preparations after ambiguous rollback recovery = %d, want 0", preparations)
	}

	retried, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !retried.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt retry = %#v, err=%v", retried, err)
	}
	if err := fixture.recordTerminal(t.Context(), retried.Command); err != nil {
		t.Fatalf("record retried terminal: %v", err)
	}
}

func TestLocalNudgeAuthorityProviderCompletionRetainsAmbiguousCommitUntilPublication(t *testing.T) {
	fixture := newLocalAuthorityProviderAttemptFixture(t)
	fixture.store.mu.Lock()
	fixture.store.failAfterCommitNext = errors.New("lost completion commit response")
	fixture.store.mu.Unlock()
	completion := providerAttemptCompletion(fixture.command, CommandActionResultInjectedUnconfirmed)

	result, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || result.Disposition != CommandCompletionAlreadyRecorded || !result.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt lost response = %#v, err=%v", result, err)
	}
	if owners := fixture.terminalOwnerCount(); owners != 1 {
		t.Fatalf("terminal writer owners after recovered commit = %d, want 1 through publication", owners)
	}
	if err := fixture.recordTerminal(t.Context(), result.Command); err != nil {
		t.Fatalf("record recovered terminal: %v", err)
	}
	if owners := fixture.terminalOwnerCount(); owners != 0 {
		t.Fatalf("terminal writer owners after recovered publication = %d, want 0", owners)
	}
}

func TestLocalNudgeAuthorityTerminalPublicationFailureReleasesWriterForRecovery(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*testing.T) terminalPublicationFailureFixture
	}{
		{
			name: "claim-denial",
			setup: func(t *testing.T) terminalPublicationFailureFixture {
				fixture := newLocalAuthorityPendingFixture(t)
				claimAt := fixture.command.DeliverAfter.Add(time.Second)
				result, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
					CommandID: fixture.command.ID, ClaimID: "claim-publication-failure", OwnerID: "owner-publication-failure",
					AttemptID: "attempt-publication-failure", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
					ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
				}, denyingLocalNudgeClaimAuthorizer{delegate: fixture.authority}, fixture.authority)
				if err != nil || !result.HasTerminalTransitionWitness() {
					t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
				}
				return terminalPublicationFailureFixture{
					repository: fixture.repository, authority: fixture.authority,
					terminal: result.Command, partition: fixture.partition,
				}
			},
		},
		{
			name: "provider-completion",
			setup: func(t *testing.T) terminalPublicationFailureFixture {
				fixture := newLocalAuthorityProviderAttemptFixture(t)
				result, err := fixture.repository.CompleteProviderAttempt(
					t.Context(), providerAttemptCompletion(fixture.command, CommandActionResultInjectedUnconfirmed),
					fixture.partition, fixture.authority,
				)
				if err != nil || !result.HasTerminalTransitionWitness() {
					t.Fatalf("CompleteProviderAttempt = %#v, err=%v", result, err)
				}
				return terminalPublicationFailureFixture{
					repository: fixture.repository, authority: fixture.authority,
					terminal: result.Command, partition: fixture.partition,
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := test.setup(t)
			if owners := localAuthorityTerminalOwnerCount(fixture.authority); owners != 1 {
				t.Fatalf("terminal owners before publication = %d, want 1", owners)
			}
			canceled, cancel := context.WithCancel(t.Context())
			cancel()
			if err := recordLocalAuthorityTerminal(canceled, fixture.authority, fixture.partition, fixture.terminal); err == nil {
				t.Fatal("RecordCommandPartitionTerminal error = nil, want canceled publication")
			}
			if owners := localAuthorityTerminalOwnerCount(fixture.authority); owners != 0 {
				t.Fatalf("terminal owners after failed publication = %d, want 0", owners)
			}
			if preparations := localAuthorityTerminalPreparationCount(fixture.authority); preparations != 1 {
				t.Fatalf("terminal preparations after failed publication = %d, want retained 1", preparations)
			}
			if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); err != nil {
				t.Fatalf("RecoverCommandAuthority after failed publication: %v", err)
			}
			resolution, err := terminalResolutionForCommand(fixture.terminal, fixture.partition)
			if err != nil {
				t.Fatalf("terminalResolutionForCommand: %v", err)
			}
			if err := fixture.authority.VerifyCommandPartitionTerminal(t.Context(), resolution); err != nil {
				t.Fatalf("VerifyCommandPartitionTerminal after recovery: %v", err)
			}
		})
	}
}

type terminalPublicationFailureFixture struct {
	repository *CommandRepository
	authority  *LocalNudgeAuthority
	terminal   Command
	partition  TrustedCityPartition
}

type localAuthorityProviderAttemptFixture struct {
	repository *CommandRepository
	store      *repositoryAtomicTestStore
	authority  *LocalNudgeAuthority
	command    Command
	partition  TrustedCityPartition
}

type localAuthorityPendingFixture struct {
	repository *CommandRepository
	store      *repositoryAtomicTestStore
	authority  *LocalNudgeAuthority
	command    Command
	partition  TrustedCityPartition
}

type denyingLocalNudgeClaimAuthorizer struct {
	delegate *LocalNudgeAuthority
}

func (a denyingLocalNudgeClaimAuthorizer) AuthorizeNudgeClaim(ctx context.Context, request NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error) {
	authorization, err := a.delegate.AuthorizeNudgeClaim(ctx, request)
	if err == nil {
		authorization.Disposition = NudgeAuthorizationDenied
	}
	return authorization, err
}

func newLocalAuthorityPendingFixture(t *testing.T) *localAuthorityPendingFixture {
	t.Helper()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })
	now := time.Date(2026, 7, 15, 20, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	ctx := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	admitted, err := ingress.Admit(ctx, validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	return &localAuthorityPendingFixture{
		repository: repository, store: store, authority: authority,
		command: *admitted.Entry.Command, partition: admitted.Partition,
	}
}

func newLocalAuthorityProviderAttemptFixture(t *testing.T) *localAuthorityProviderAttemptFixture {
	t.Helper()
	pending := newLocalAuthorityPendingFixture(t)
	claimAt := pending.command.DeliverAfter.Add(time.Second)
	claimed, err := pending.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: pending.command.ID, ClaimID: "claim-local-provider", OwnerID: "owner-local-provider",
		AttemptID: "attempt-local-provider", BoundLaunchIdentity: "launch-123", Partition: pending.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, pending.authority, pending.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed || claimed.Command.State != CommandStateInFlight {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	return &localAuthorityProviderAttemptFixture{
		repository: pending.repository, store: pending.store, authority: pending.authority,
		command: claimed.Command, partition: pending.partition,
	}
}

func (f *localAuthorityProviderAttemptFixture) terminalOwnerCount() int {
	return localAuthorityTerminalOwnerCount(f.authority)
}

func (f *localAuthorityProviderAttemptFixture) terminalPreparationCount() int {
	return localAuthorityTerminalPreparationCount(f.authority)
}

func localAuthorityTerminalOwnerCount(authority *LocalNudgeAuthority) int {
	authority.terminalOwnershipMu.Lock()
	defer authority.terminalOwnershipMu.Unlock()
	return len(authority.terminalOwners)
}

func localAuthorityTerminalPreparationCount(authority *LocalNudgeAuthority) int {
	var count int
	if err := authority.db.QueryRow(`SELECT COUNT(*) FROM terminal_preparations`).Scan(&count); err != nil {
		return -1
	}
	return count
}

func (f *localAuthorityProviderAttemptFixture) recordTerminal(ctx context.Context, command Command) error {
	return recordLocalAuthorityTerminal(ctx, f.authority, f.partition, command)
}

func recordLocalAuthorityTerminal(ctx context.Context, authority *LocalNudgeAuthority, partition TrustedCityPartition, command Command) error {
	return authority.RecordCommandPartitionTerminal(ctx, CommandPartitionTerminal{
		Store: command.Store, RepositoryRevision: command.Order.Revision,
		CommandID: command.ID, Sequence: command.Order.Sequence, Partition: partition,
	})
}

func (r *blockingTerminalRecoveryReader) Get(ctx context.Context, commandID string) (CommandIndexResolution, error) {
	resolution, err := r.localAuthorityRecoveryReader.Get(ctx, commandID)
	close(r.observed)
	select {
	case <-r.resume:
		return resolution, err
	case <-ctx.Done():
		return CommandIndexResolution{}, ctx.Err()
	}
}
