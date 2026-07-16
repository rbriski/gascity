package nudgequeue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestLocalNudgeAuthorityClaimRecoveryRetriesPreparationAbortedAfterPageRead(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, intent, _ := preparePendingClaimTransitionForRecoveryTest(t, fixture)
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter: %v", err)
	}

	hookStore := installClaimAuditReadHook(fixture.repository, fixture.store)
	hookStore.hook = func() error {
		return fixture.authority.AbortCommandClaimTransition(t.Context(), intent)
	}
	budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(budgetCtx, fixture.repository, state); err != nil || stable {
		t.Fatalf("repairCommandClaimTransitions across concurrent abort = stable:%t err:%v, want retry", stable, err)
	}
	if budget.work != 1 {
		t.Fatalf("claim recovery work after one aborting exact read = %d, want 1", budget.work)
	}
	if err := hookStore.result(); err != nil {
		t.Fatalf("concurrent claim abort: %v", err)
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || !stable {
		t.Fatalf("repairCommandClaimTransitions after concurrent abort = stable:%t err:%v, want stable", stable, err)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryRetriesPreparationFinalizedAfterPageRead(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	before, intent, after := preparePendingClaimTransitionForRecoveryTest(t, fixture)
	state := persistPreparedClaimAfterState(t, fixture, before, after)
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter: %v", err)
	}
	receipt, err := commandClaimTransitionReceiptFor(state, after, fixture.partition)
	if err != nil {
		t.Fatalf("commandClaimTransitionReceiptFor: %v", err)
	}

	hookStore := installClaimAuditReadHook(fixture.repository, fixture.store)
	hookStore.hook = func() error {
		disposition, err := fixture.authority.FinalizeCommandClaimTransition(t.Context(), receipt)
		if err == nil && disposition != CommandClaimReceiptFinalized {
			return errors.New("concurrent claim finalization did not create the receipt")
		}
		return err
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || stable {
		t.Fatalf("repairCommandClaimTransitions across concurrent finalization = stable:%t err:%v, want retry", stable, err)
	}
	if err := hookStore.result(); err != nil {
		t.Fatalf("concurrent claim finalization: %v", err)
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || !stable {
		t.Fatalf("repairCommandClaimTransitions after concurrent finalization = stable:%t err:%v, want stable", stable, err)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryRestartsWhenPreparationAppearsBetweenAuditPhases(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	before, intent, after := preparePendingClaimTransitionForRecoveryTest(t, fixture)
	state := persistPreparedClaimAfterState(t, fixture, before, after)
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter before first audit: %v", err)
	}

	budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
	budget.work = commandAuthorityRecoveryMaxWork - 1
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(budgetCtx, fixture.repository, state); !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable {
		t.Fatalf("first bounded claim audit = stable:%t err:%v, want yielded after preparation phase", stable, err)
	}
	cursor, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read cursor after bounded preparation audit: %v", err)
	}
	if cursor.phase != localAuthorityClaimAuditReceipts || cursor.preparationCount != 1 {
		t.Fatalf("cursor after bounded preparation audit = %#v, want receipts phase after one preparation", cursor)
	}

	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition between audit phases: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition between audit phases: %v", err)
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || !stable {
		t.Fatalf("claim audit across preparation ABA = stable:%t err:%v, want a complete restarted audit", stable, err)
	}
	restarted, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read cursor after preparation ABA: %v", err)
	}
	if restarted.phase != localAuthorityClaimAuditDone || restarted.generation <= cursor.generation || restarted.preparationCount != 1 {
		t.Fatalf("cursor after preparation ABA = %#v, before %#v; want a new complete generation containing the preparation", restarted, cursor)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter after generation retry: %v", err)
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || !stable {
		t.Fatalf("claim audit after preparation writer releases = stable:%t err:%v, want stable", stable, err)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryRetriesReceiptConsumedAfterPageRead(t *testing.T) {
	fixture := newLocalAuthorityProviderAttemptFixture(t)
	completion := providerAttemptCompletion(fixture.command, CommandActionResultInjectedUnconfirmed)
	terminal, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !terminal.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", terminal, err)
	}
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	hookStore := installClaimAuditReadHook(fixture.repository, fixture.store)
	hookStore.hook = func() error {
		return recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, terminal.Command)
	}
	budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(budgetCtx, fixture.repository, state); err != nil || stable {
		t.Fatalf("repairCommandClaimTransitions across terminal publication = stable:%t err:%v, want retry", stable, err)
	}
	if budget.work != 1 {
		t.Fatalf("claim recovery work after one terminalized receipt read = %d, want 1", budget.work)
	}
	if err := hookStore.result(); err != nil {
		t.Fatalf("concurrent terminal publication: %v", err)
	}
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || !stable {
		t.Fatalf("repairCommandClaimTransitions after terminal publication = stable:%t err:%v, want stable", stable, err)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryChargesLatePageReadFailure(t *testing.T) {
	const successfulPrefix = localAuthorityRecoveryPageSize - 9
	fixture := newClaimCursorCrashFixture(t, successfulPrefix+1)
	injected := errors.New("injected late exact-read failure")
	failing := &failNthClaimAuditReadStore{
		claimCursorRecordingStore: fixture.store,
		failAt:                    successfulPrefix + 1,
		err:                       injected,
	}
	fixture.repository.reader.store = failing
	fixture.repository.reader.snapshots = failing

	budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(budgetCtx, fixture.repository, fixture.state); !errors.Is(err, injected) || stable {
		t.Fatalf("repairCommandClaimTransitions late page failure = stable:%t err:%v, want injected failure", stable, err)
	}
	if budget.work != successfulPrefix+1 {
		t.Fatalf("claim recovery work after %d successful reads and one failed attempt = %d, want %d",
			successfulPrefix, budget.work, successfulPrefix+1)
	}
}

type failNthClaimAuditReadStore struct {
	*claimCursorRecordingStore

	failAt int
	calls  int
	err    error
}

func (s *failNthClaimAuditReadStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	if commitMessage == "gc: read durable nudge command" {
		s.calls++
		if s.calls == s.failAt {
			return s.err
		}
	}
	return s.claimCursorRecordingStore.AtomicReadWrite(ctx, commitMessage, fn)
}

func preparePendingClaimTransitionForRecoveryTest(t *testing.T, fixture *localAuthorityPendingFixture) (CommandRepositoryState, CommandClaimTransitionIntent, Command) {
	t.Helper()
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-recovery-race", OwnerID: "owner-recovery-race",
		AttemptID: "attempt-recovery-race", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
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
	return state, intent, after
}

func persistPreparedClaimAfterState(
	t *testing.T,
	fixture *localAuthorityPendingFixture,
	before CommandRepositoryState,
	after Command,
) CommandRepositoryState {
	t.Helper()
	wire, err := EncodeCommandV1(after)
	if err != nil {
		t.Fatalf("EncodeCommandV1 after-state: %v", err)
	}
	state := before
	state.Revision = after.Order.Revision
	fixture.store.mu.Lock()
	row := fixture.store.rows[fixture.command.ID]
	row.Metadata[commandRecordWireMetadataKey] = string(wire)
	fixture.store.rows[fixture.command.ID] = row
	fixture.store.metadata = repositoryMetadataForTest(state)
	fixture.store.mu.Unlock()
	if err := fixture.repository.writer.AdvanceCommandRepositoryLineage(t.Context(), state); err != nil {
		t.Fatalf("AdvanceCommandRepositoryLineage: %v", err)
	}
	return state
}

type claimAuditReadHookStore struct {
	*repositoryAtomicTestStore

	once    sync.Once
	hook    func() error
	hookErr error
}

func installClaimAuditReadHook(repository *CommandRepository, store *repositoryAtomicTestStore) *claimAuditReadHookStore {
	hooked := &claimAuditReadHookStore{repositoryAtomicTestStore: store}
	repository.reader.store = hooked
	repository.reader.snapshots = hooked
	return hooked
}

func (s *claimAuditReadHookStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	err := s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, fn)
	if err == nil && commitMessage == "gc: read durable nudge command" {
		s.once.Do(func() {
			if s.hook != nil {
				s.hookErr = s.hook()
			}
		})
	}
	return err
}

func (s *claimAuditReadHookStore) result() error {
	return s.hookErr
}
