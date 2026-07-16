package nudgequeue

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestLocalNudgeAuthorityClaimAuditCancellationBeforePublicationPreservesRawCursor(t *testing.T) {
	fixture := newClaimCursorCrashFixture(t, 2)
	expected, err := fixture.authority.ensureClaimAuditCursor(t.Context(), fixture.state)
	if err != nil {
		t.Fatalf("ensureClaimAuditCursor: %v", err)
	}
	before := readRawClaimAuditCursor(t, fixture.authority.db)
	next, work, changed, deferred, err := fixture.authority.auditClaimPreparationPage(
		t.Context(), fixture.repository, fixture.state, expected, 1,
	)
	if err != nil || work != 1 || changed || deferred || next == expected {
		t.Fatalf("auditClaimPreparationPage = next:%#v work:%d changed:%t deferred:%t err:%v", next, work, changed, deferred, err)
	}

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if err := fixture.authority.persistClaimAuditCursor(canceled, expected, next); !errors.Is(err, context.Canceled) {
		t.Fatalf("persistClaimAuditCursor with canceled context error = %v, want context.Canceled", err)
	}
	afterCancellation := readRawClaimAuditCursor(t, fixture.authority.db)
	if !reflect.DeepEqual(afterCancellation, before) {
		t.Fatalf("raw claim audit cursor changed before publication:\n before: %#v\n  after: %#v", before, afterCancellation)
	}

	if err := fixture.authority.persistClaimAuditCursor(t.Context(), expected, next); err != nil {
		t.Fatalf("persistClaimAuditCursor retry: %v", err)
	}
	persisted, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("readLocalAuthorityClaimAuditCursor after retry: %v", err)
	}
	if persisted != next {
		t.Fatalf("persisted claim audit cursor = %#v, want %#v", persisted, next)
	}
}

func TestLocalNudgeAuthorityClaimAuditCloseReopenHonorsPublicationBoundary(t *testing.T) {
	fixture := newClaimCursorCrashFixture(t, 2)
	expected, err := fixture.authority.ensureClaimAuditCursor(t.Context(), fixture.state)
	if err != nil {
		t.Fatalf("ensureClaimAuditCursor: %v", err)
	}
	rawExpected := readRawClaimAuditCursor(t, fixture.authority.db)

	fixture.store.resetExactReads()
	nextBeforeCrash, work, changed, deferred, err := fixture.authority.auditClaimPreparationPage(
		t.Context(), fixture.repository, fixture.state, expected, 1,
	)
	if err != nil || work != 1 || changed || deferred {
		t.Fatalf("auditClaimPreparationPage before publication = next:%#v work:%d changed:%t deferred:%t err:%v", nextBeforeCrash, work, changed, deferred, err)
	}
	if nextBeforeCrash.phase != localAuthorityClaimAuditPreparations || nextBeforeCrash.afterCommandID != fixture.commandIDs[0] {
		t.Fatalf("first computed cursor = %#v, want preparation key %q", nextBeforeCrash, fixture.commandIDs[0])
	}
	assertExactClaimAuditReads(t, fixture.store.exactReadsSnapshot(), fixture.commandIDs[0])

	fixture.closeAndReopenAuthority(t)
	if rawReopened := readRawClaimAuditCursor(t, fixture.authority.db); !reflect.DeepEqual(rawReopened, rawExpected) {
		t.Fatalf("unpublished claim audit cursor survived close+reopen:\n expected: %#v\n      got: %#v", rawExpected, rawReopened)
	}
	reopenedExpected, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("readLocalAuthorityClaimAuditCursor before reread: %v", err)
	}
	if reopenedExpected != expected {
		t.Fatalf("cursor after close before publication = %#v, want %#v", reopenedExpected, expected)
	}

	fixture.store.resetExactReads()
	nextAfterReread, work, changed, deferred, err := fixture.authority.auditClaimPreparationPage(
		t.Context(), fixture.repository, fixture.state, reopenedExpected, 1,
	)
	if err != nil || work != 1 || changed || deferred || nextAfterReread != nextBeforeCrash {
		t.Fatalf("reread unpublished page = next:%#v work:%d changed:%t deferred:%t err:%v; want next %#v", nextAfterReread, work, changed, deferred, err, nextBeforeCrash)
	}
	assertExactClaimAuditReads(t, fixture.store.exactReadsSnapshot(), fixture.commandIDs[0])
	if err := fixture.authority.persistClaimAuditCursor(t.Context(), reopenedExpected, nextAfterReread); err != nil {
		t.Fatalf("persistClaimAuditCursor before published close: %v", err)
	}
	rawPublished := readRawClaimAuditCursor(t, fixture.authority.db)

	fixture.closeAndReopenAuthority(t)
	if rawReopened := readRawClaimAuditCursor(t, fixture.authority.db); !reflect.DeepEqual(rawReopened, rawPublished) {
		t.Fatalf("published claim audit cursor changed across close+reopen:\n expected: %#v\n      got: %#v", rawPublished, rawReopened)
	}
	published, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("readLocalAuthorityClaimAuditCursor after published reopen: %v", err)
	}
	if published != nextAfterReread {
		t.Fatalf("cursor after published reopen = %#v, want %#v", published, nextAfterReread)
	}

	fixture.store.resetExactReads()
	_, work, changed, deferred, err = fixture.authority.auditClaimPreparationPage(
		t.Context(), fixture.repository, fixture.state, published, 1,
	)
	if err != nil || work != 1 || changed || deferred {
		t.Fatalf("auditClaimPreparationPage after published reopen = work:%d changed:%t deferred:%t err:%v", work, changed, deferred, err)
	}
	reads := fixture.store.exactReadsSnapshot()
	assertExactClaimAuditReads(t, reads, fixture.commandIDs[1])
	if reads[0] <= published.afterCommandID {
		t.Fatalf("resumed command key = %q, want strictly after published key %q", reads[0], published.afterCommandID)
	}
}

func TestLocalNudgeAuthorityClaimAuditResumesAcrossPageBoundary(t *testing.T) {
	fixture := newClaimCursorCrashFixture(t, localAuthorityRecoveryPageSize+1)
	wantWork := []int{commandAuthorityRecoveryMaxWork, commandAuthorityRecoveryMaxWork, 2}
	for invocation, want := range wantWork {
		fixture.store.resetExactReads()
		budgetCtx, budget := withCommandAuthorityRecoveryBudget(t.Context())
		token, stable, err := fixture.authority.repairCommandClaimTransitions(budgetCtx, fixture.repository, fixture.state)
		reads := fixture.store.exactReadsSnapshot()
		if budget.work != want || len(reads) != want {
			t.Fatalf("claim audit invocation %d work/reads = %d/%d, want %d/%d", invocation+1, budget.work, len(reads), want, want)
		}
		cursor, cursorErr := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
		if cursorErr != nil {
			t.Fatalf("read claim audit cursor after invocation %d: %v", invocation+1, cursorErr)
		}
		switch invocation {
		case 0:
			if !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable || cursor.phase != localAuthorityClaimAuditPreparations ||
				cursor.preparationCount != localAuthorityRecoveryPageSize {
				t.Fatalf("first bounded claim audit = cursor:%#v stable:%t err:%v", cursor, stable, err)
			}
		case 1:
			if !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable || cursor.phase != localAuthorityClaimAuditActive ||
				cursor.preparationCount != localAuthorityRecoveryPageSize+1 || cursor.afterSequence == 0 {
				t.Fatalf("second bounded claim audit = cursor:%#v stable:%t err:%v", cursor, stable, err)
			}
		case 2:
			if err != nil || !stable || cursor.phase != localAuthorityClaimAuditDone ||
				cursor.preparationCount != localAuthorityRecoveryPageSize+1 || token != cursor.token() {
				t.Fatalf("final bounded claim audit = cursor:%#v token:%#v stable:%t err:%v", cursor, token, stable, err)
			}
		}
	}
}

type rawClaimAuditCursor struct {
	generation         []byte
	repositoryRevision []byte
	sequenceHighWater  []byte
	phase              string
	afterCommandID     string
	afterSequence      []byte
	identity           []byte
	preparationCount   []byte
	receiptCount       []byte
	checkpointDigest   []byte
}

func readRawClaimAuditCursor(t *testing.T, queryer localAuthorityQueryer) rawClaimAuditCursor {
	t.Helper()
	var cursor rawClaimAuditCursor
	if err := queryer.QueryRowContext(t.Context(), `SELECT claim_audit_generation, claim_audit_repository_revision,
		claim_audit_sequence_high_water, claim_audit_phase, claim_audit_after_command_id,
		claim_audit_after_sequence, claim_audit_identity, claim_audit_preparation_count, claim_audit_receipt_count,
		claim_audit_checkpoint_digest
		FROM authority_meta WHERE singleton = 1`).Scan(
		&cursor.generation, &cursor.repositoryRevision, &cursor.sequenceHighWater, &cursor.phase, &cursor.afterCommandID,
		&cursor.afterSequence, &cursor.identity, &cursor.preparationCount, &cursor.receiptCount, &cursor.checkpointDigest,
	); err != nil {
		t.Fatalf("read raw claim audit cursor: %v", err)
	}
	return cursor
}

type claimCursorCrashFixture struct {
	cityPath   string
	store      *claimCursorRecordingStore
	repository *CommandRepository
	authority  *LocalNudgeAuthority
	state      CommandRepositoryState
	commandIDs []string
}

func newClaimCursorCrashFixture(t *testing.T, commandCount int) *claimCursorCrashFixture {
	t.Helper()
	store := &claimCursorRecordingStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before authority open: %v", err)
	}
	fixture := &claimCursorCrashFixture{cityPath: t.TempDir(), store: store, repository: repository}
	fixture.authority, err = OpenLocalNudgeAuthority(t.Context(), fixture.cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() {
		if fixture.authority != nil {
			_ = fixture.authority.Close()
		}
	})

	now := time.Date(2026, 7, 15, 22, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, fixture.authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	requesterContext := WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester())
	commands := make([]Command, 0, commandCount)
	partitions := make([]TrustedCityPartition, 0, commandCount)
	for index := 0; index < commandCount; index++ {
		request := validNudgeIngressRequest(now)
		request.RequestID = fmt.Sprintf("claim-cursor-crash-%d", index)
		request.Message = fmt.Sprintf("claim cursor crash command %d", index)
		admitted, err := ingress.Admit(requesterContext, request)
		if err != nil || !admitted.Created || admitted.Entry.Command == nil {
			t.Fatalf("Admit command %d = %#v, err=%v", index, admitted, err)
		}
		commands = append(commands, cloneCommandValue(*admitted.Entry.Command))
		partitions = append(partitions, admitted.Partition)
	}
	for index, command := range commands {
		claimAt := command.DeliverAfter.Add(time.Second)
		transitionAuthority := &failOnceLocalClaimTransitionAuthority{
			LocalNudgeAuthority: fixture.authority,
			nextErr:             errors.New("injected receipt publication failure"),
		}
		result, err := repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
			CommandID: command.ID, ClaimID: fmt.Sprintf("claim-cursor-crash-%d", index),
			OwnerID: "claim-cursor-owner", AttemptID: fmt.Sprintf("claim-cursor-attempt-%d", index),
			BoundLaunchIdentity: "claim-cursor-launch", Partition: partitions[index],
			ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
		}, fixture.authority, transitionAuthority)
		if err == nil || result.Disposition != CommandClaimAllowed || result.Command.State != CommandStateInFlight {
			t.Fatalf("ClaimAuthorized command %d = %#v, err=%v; want committed in-flight preparation and injected error", index, result, err)
		}
		fixture.commandIDs = append(fixture.commandIDs, command.ID)
	}
	sort.Strings(fixture.commandIDs)
	fixture.state, err = repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after claims: %v", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != commandCount || receipts != 0 {
		t.Fatalf("claim evidence = preparations:%d receipts:%d, want %d/0", preparations, receipts, commandCount)
	}
	fixture.store.resetExactReads()
	return fixture
}

func (f *claimCursorCrashFixture) closeAndReopenAuthority(t *testing.T) {
	t.Helper()
	if err := f.authority.Close(); err != nil {
		t.Fatalf("Close LocalNudgeAuthority: %v", err)
	}
	f.authority = nil
	reopened, err := OpenLocalNudgeAuthority(t.Context(), f.cityPath, f.state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen LocalNudgeAuthority: %v", err)
	}
	f.authority = reopened
}

type claimCursorRecordingStore struct {
	*repositoryAtomicTestStore

	readsMu    sync.Mutex
	exactReads []string
}

func (s *claimCursorRecordingStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	return s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, func(tx beads.AtomicReadWriteTx) error {
		if commitMessage != "gc: read durable nudge command" {
			return fn(tx)
		}
		return fn(claimCursorRecordingTx{AtomicReadWriteTx: tx, record: s.recordExactRead})
	})
}

func (s *claimCursorRecordingStore) recordExactRead(commandID string) {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	s.exactReads = append(s.exactReads, commandID)
}

func (s *claimCursorRecordingStore) resetExactReads() {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	s.exactReads = nil
}

func (s *claimCursorRecordingStore) exactReadsSnapshot() []string {
	s.readsMu.Lock()
	defer s.readsMu.Unlock()
	return append([]string(nil), s.exactReads...)
}

type claimCursorRecordingTx struct {
	beads.AtomicReadWriteTx
	record func(string)
}

func (tx claimCursorRecordingTx) GetIssue(commandID string) (beads.Bead, error) {
	tx.record(commandID)
	return tx.AtomicReadWriteTx.GetIssue(commandID)
}

func assertExactClaimAuditReads(t *testing.T, got []string, want ...string) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exact claim-audit repository reads = %v, want %v", got, want)
	}
}
