package nudgequeue

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestLocalNudgeAuthorityRecoverCommandAuthorityOrdersRepairsAndClosesFinalFence(t *testing.T) {
	store := &commandAuthorityRecoveryHookStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
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

	now := time.Date(2026, 7, 15, 18, 0, 0, 0, time.UTC)
	legitimate, legitimatePartition := prepareCommandAuthorityRecoveryGrant(t, authority, state.Store, "recovery-legitimate", now)
	legitimateEntry, created, err := repository.create(t.Context(), "recovery-legitimate", legitimate, legitimatePartition)
	if err != nil || !created || legitimateEntry.Command == nil || legitimateEntry.Command.Order.Sequence != 1 {
		t.Fatalf("create legitimate command = %#v, created=%t, err=%v", legitimateEntry, created, err)
	}

	forged := repositoryCommandForRequest(t, state.Store, "recovery-forged", "untrusted direct writer")
	forgedEntry, created, err := repository.createForTest(t.Context(), "recovery-forged", forged)
	if err != nil || !created || forgedEntry.Command == nil || forgedEntry.Command.Order.Sequence != 2 {
		t.Fatalf("create forged command = %#v, created=%t, err=%v", forgedEntry, created, err)
	}

	hookRan := false
	store.hook = func() {
		hookRan = true
		prepareCommandAuthorityRecoveryGrant(t, authority, state.Store, "recovery-late-absent", now.Add(time.Minute))
	}
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority: %v", err)
	}
	if !hookRan {
		t.Fatal("late admission hook did not run during provenance audit")
	}

	resolvedLegitimate, err := repository.Get(t.Context(), legitimateEntry.Command.ID)
	if err != nil || !resolvedLegitimate.Found || resolvedLegitimate.Entry.Command == nil ||
		resolvedLegitimate.Entry.Command.State != CommandStatePending || resolvedLegitimate.Entry.Command.Terminal != nil {
		t.Fatalf("legitimate command after recovery = %#v, err=%v; want active pending command", resolvedLegitimate, err)
	}
	resolvedForged, err := repository.Get(t.Context(), forgedEntry.Command.ID)
	if err != nil || !resolvedForged.Found || resolvedForged.Entry.Command == nil ||
		resolvedForged.Entry.Command.Terminal == nil ||
		resolvedForged.Entry.Command.Terminal.ActionResult != CommandActionResultUnauthorizedProvenance {
		t.Fatalf("forged command after recovery = %#v, err=%v; want unauthorized-provenance terminal", resolvedForged, err)
	}

	var admissionPreparations, terminalPreparations, rejectionPreparations int
	if err := authority.db.QueryRowContext(t.Context(), `SELECT
		(SELECT COUNT(*) FROM admission_preparations),
		(SELECT COUNT(*) FROM terminal_preparations),
		(SELECT COUNT(*) FROM rejection_preparations)`).Scan(
		&admissionPreparations, &terminalPreparations, &rejectionPreparations,
	); err != nil {
		t.Fatalf("read final preparation fence: %v", err)
	}
	dense, err := authority.localAuthorityDenseDecisionHighWater(t.Context())
	if err != nil {
		t.Fatalf("localAuthorityDenseDecisionHighWater: %v", err)
	}
	finalState, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("final State: %v", err)
	}
	if admissionPreparations != 0 || terminalPreparations != 0 || rejectionPreparations != 0 || dense != finalState.SequenceHighWater {
		t.Fatalf("final authority fence = admissions:%d terminals:%d rejections:%d dense:%d repository:%d; want no preparations and equal high-water",
			admissionPreparations, terminalPreparations, rejectionPreparations, dense, finalState.SequenceHighWater)
	}
	coverage, err := authority.ResolveCommandPartitionCoverage(t.Context(), CommandPartitionCoverageRequest{
		Store: state.Store, RepositoryRevision: finalState.Revision, SequenceHighWater: finalState.SequenceHighWater,
		MaxCommands: 2, Partition: legitimatePartition,
	})
	if err != nil || len(coverage.ActiveEntries) != 1 || coverage.ActiveEntries[0].CommandID != legitimateEntry.Command.ID {
		t.Fatalf("recovered legitimate coverage = %#v, err=%v; want only exact granted command", coverage, err)
	}
}

func TestLocalNudgeAuthorityRecoverCommandAuthorityRetriesMovingFinalFence(t *testing.T) {
	store := &commandAuthorityRecoveryHookStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
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

	var forgedID string
	store.afterSecondLineageRead = func() {
		const requestID = "moving-final-fence-forged-command"
		forged := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		entry, created, err := repository.createForTest(t.Context(), requestID, forged)
		if err != nil || !created || entry.Command == nil {
			t.Fatalf("create command across final fence = %#v, created=%t err=%v", entry, created, err)
		}
		forgedID = entry.Command.ID
	}
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority moving fence: %v", err)
	}
	if forgedID == "" {
		t.Fatal("moving-fence hook did not create a command")
	}
	resolved, err := repository.Get(t.Context(), forgedID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.Terminal == nil ||
		resolved.Entry.Command.Terminal.ActionResult != CommandActionResultUnauthorizedProvenance {
		t.Fatalf("moving-fence command after retry = %#v, err=%v", resolved, err)
	}
}

func TestLocalNudgeAuthorityRecoverCommandAuthorityRetriesAdvanceBeforeFinalFenceRead(t *testing.T) {
	store := &commandAuthorityRecoveryHookStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
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

	var forgedID string
	store.beforeSecondLineageRead = func() {
		const requestID = "advance-before-final-fence-read"
		forged := repositoryCommandForRequest(t, state.Store, requestID, requestID)
		entry, created, err := repository.createForTest(t.Context(), requestID, forged)
		if err != nil || !created || entry.Command == nil {
			t.Fatalf("create command before final fence read = %#v, created=%t err=%v", entry, created, err)
		}
		forgedID = entry.Command.ID
	}
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority advancing fence: %v", err)
	}
	resolved, err := repository.Get(t.Context(), forgedID)
	if forgedID == "" || err != nil || !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.Terminal == nil ||
		resolved.Entry.Command.Terminal.ActionResult != CommandActionResultUnauthorizedProvenance {
		t.Fatalf("advanced-fence command after retry = id:%q resolution:%#v err:%v", forgedID, resolved, err)
	}
}

func TestLocalNudgeAuthorityRecoverCommandAuthorityRepairsDatabaseAheadOfAnchorDuringIntermediateStage(t *testing.T) {
	store := &commandAuthorityRecoveryHookStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	verifier, ok := repository.writer.(*repositoryLineageTestVerifier)
	if !ok {
		t.Fatalf("repository writer = %T, want *repositoryLineageTestVerifier", repository.writer)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	const requestID = "database-ahead-during-intermediate-repair"
	forgedID := CommandIDForRequest(state.Store, requestID)
	store.armBeforeNextStateRead(func() {
		commitCommandAheadOfRecoveryAnchor(t, repository, verifier, state.Store, requestID)
	})

	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority with intermediate database-ahead gap: %v", err)
	}
	assertRecoveredUnauthorizedCommand(t, repository, forgedID)
}

func TestLocalNudgeAuthorityRecoverCommandAuthorityRepairsDatabaseAheadOfAnchorDuringFinalConfirmation(t *testing.T) {
	store := &commandAuthorityRecoveryHookStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	verifier, ok := repository.writer.(*repositoryLineageTestVerifier)
	if !ok {
		t.Fatalf("repository writer = %T, want *repositoryLineageTestVerifier", repository.writer)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	const requestID = "database-ahead-during-final-confirmation"
	forgedID := CommandIDForRequest(state.Store, requestID)
	store.beforeSecondLineageRead = func() {
		store.armBeforeNextStateRead(func() {
			commitCommandAheadOfRecoveryAnchor(t, repository, verifier, state.Store, requestID)
		})
	}

	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority with final-confirmation database-ahead gap: %v", err)
	}
	assertRecoveredUnauthorizedCommand(t, repository, forgedID)
}

func commitCommandAheadOfRecoveryAnchor(
	t *testing.T,
	repository *CommandRepository,
	verifier *repositoryLineageTestVerifier,
	store CommandStoreBinding,
	requestID string,
) {
	t.Helper()
	verifier.failNextAdvance(errors.New("injected post-commit lineage interruption"))
	command := repositoryCommandForRequest(t, store, requestID, requestID)
	if _, _, err := repository.createForTest(t.Context(), requestID, command); !errors.Is(err, ErrCommandRepositoryLineage) {
		t.Fatalf("createForTest post-commit interruption error = %v, want ErrCommandRepositoryLineage", err)
	}
}

func assertRecoveredUnauthorizedCommand(t *testing.T, repository *CommandRepository, commandID string) {
	t.Helper()
	resolved, err := repository.Get(t.Context(), commandID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil || resolved.Entry.Command.Terminal == nil ||
		resolved.Entry.Command.Terminal.ActionResult != CommandActionResultUnauthorizedProvenance {
		t.Fatalf("recovered database-ahead command = %#v, err=%v; want unauthorized-provenance terminal", resolved, err)
	}
}

func prepareCommandAuthorityRecoveryGrant(
	t *testing.T,
	authority *LocalNudgeAuthority,
	store CommandStoreBinding,
	requestID string,
	requestedAt time.Time,
) (Command, TrustedCityPartition) {
	t.Helper()
	command := validCommandV1(CommandStatePending)
	command.ID = CommandIDForRequest(store, requestID)
	command.Store = CommandStoreBinding{}
	command.Order = CommandOrder{}
	command.Mode = DeliveryModeQueue
	command.Target = localAuthorityIngressRequest().Target
	command.Source = CommandSourceSession
	command.Message = "authorized recovery nudge"
	command.Reference = nil
	command.CreatedAt = requestedAt
	command.DeliverAfter = requestedAt
	command.ExpiresAt = requestedAt.Add(time.Hour)
	command.Binding = nil
	command.Claim = nil
	command.Retry = nil
	command.Terminal = nil
	command.TrustedIngress = TrustedIngressReference{}
	authorized, err := authority.AuthorizeNudgeIngress(
		WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()),
		NudgeIngressAuthorizationRequest{
			RequestID: requestID, Action: NudgeCommandAction, Mode: command.Mode, Target: command.Target,
			IntentDigest: computeNudgeIngressIntentDigest(command), PayloadDigest: ComputeCommandPayloadDigest(command),
			DeliverAfter: command.DeliverAfter, ExpiresAt: command.ExpiresAt, RequestedAt: requestedAt,
		},
	)
	if err != nil || authorized.Disposition != NudgeAuthorizationAllowed {
		t.Fatalf("AuthorizeNudgeIngress(%q) = %#v, err=%v", requestID, authorized, err)
	}
	command.TrustedIngress = authorized.Reference
	return command, trustedCityPartitionFromAuthority(authorized.Reference)
}

type commandAuthorityRecoveryHookStore struct {
	*repositoryAtomicTestStore
	hookOnce                sync.Once
	hook                    func()
	lineageMu               sync.Mutex
	lineageAttempts         int
	beforeSecondLineageRead func()
	afterSecondLineageRead  func()
	stateMu                 sync.Mutex
	beforeNextStateRead     func()
}

func (s *commandAuthorityRecoveryHookStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	if commitMessage == "gc: read durable nudge command repository" {
		s.stateMu.Lock()
		hook := s.beforeNextStateRead
		s.beforeNextStateRead = nil
		s.stateMu.Unlock()
		if hook != nil {
			hook()
		}
	}
	if commitMessage != "gc: read durable nudge command repository for lineage repair" {
		return s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, fn)
	}
	s.lineageMu.Lock()
	s.lineageAttempts++
	attempt := s.lineageAttempts
	beforeHook := s.beforeSecondLineageRead
	afterHook := s.afterSecondLineageRead
	if attempt == 2 {
		s.beforeSecondLineageRead = nil
		s.afterSecondLineageRead = nil
	} else {
		beforeHook = nil
		afterHook = nil
	}
	s.lineageMu.Unlock()
	if beforeHook != nil {
		beforeHook()
	}
	err := s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, fn)
	if err == nil && afterHook != nil {
		afterHook()
	}
	return err
}

func (s *commandAuthorityRecoveryHookStore) armBeforeNextStateRead(hook func()) {
	s.stateMu.Lock()
	defer s.stateMu.Unlock()
	s.beforeNextStateRead = hook
}

func (s *commandAuthorityRecoveryHookStore) AtomicReadSnapshot(ctx context.Context, fn func(beads.AtomicReadSnapshotTx) error) error {
	return s.repositoryAtomicTestStore.AtomicReadSnapshot(ctx, func(tx beads.AtomicReadSnapshotTx) error {
		return fn(&commandAuthorityRecoveryHookTx{AtomicReadSnapshotTx: tx, hookOnce: &s.hookOnce, hook: s.hook})
	})
}

type commandAuthorityRecoveryHookTx struct {
	beads.AtomicReadSnapshotTx
	hookOnce *sync.Once
	hook     func()
}

func (tx *commandAuthorityRecoveryHookTx) ListHistoryByControlSequence(query beads.AtomicReadSnapshotControlSequenceQuery) (beads.AtomicReadSnapshotControlSequencePage, error) {
	page, err := tx.AtomicReadSnapshotTx.ListHistoryByControlSequence(query)
	if err == nil && tx.hook != nil {
		tx.hookOnce.Do(tx.hook)
	}
	return page, err
}
