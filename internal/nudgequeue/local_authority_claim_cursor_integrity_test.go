package nudgequeue

import (
	"bytes"
	"database/sql"
	"errors"
	"math"
	"net/url"
	"testing"
	"time"
)

func TestOpenLocalNudgeAuthorityRestartsTamperedCompletedClaimAuditCursor(t *testing.T) {
	cityPath := t.TempDir()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		_ = authority.Close()
		t.Fatalf("RecoverCommandAuthority: %v", err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	dsn := (&url.URL{Scheme: "file", Path: LocalNudgeAuthorityPath(cityPath)}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		t.Fatalf("open authority for tamper: %v", err)
	}
	if _, err := db.ExecContext(t.Context(), `UPDATE authority_meta SET claim_audit_identity = ? WHERE singleton = 1`, bytes.Repeat([]byte{0x5a}, 32)); err != nil {
		_ = db.Close()
		t.Fatalf("tamper completed claim audit identity: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close tampered authority: %v", err)
	}

	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority after derived cursor tamper: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	cursor, err := reopened.readLocalAuthorityClaimAuditCursor(t.Context(), reopened.db)
	if err != nil {
		t.Fatalf("read reset claim audit cursor: %v", err)
	}
	if cursor.phase != localAuthorityClaimAuditPreparations || cursor.afterCommandID != "" || cursor.afterSequence != 0 ||
		cursor.preparationCount != 0 || cursor.receiptCount != 0 || cursor.identity != initialLocalAuthorityClaimAuditIdentity() {
		t.Fatalf("claim audit cursor after tamper reset = %#v, want fresh preparation audit", cursor)
	}
	if err := reopened.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority after cursor tamper reset: %v", err)
	}
}

func TestLocalNudgeAuthorityClaimGenerationWrapRollsBackPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-generation-wrap", OwnerID: "owner-generation-wrap",
		AttemptID: "attempt-generation-wrap", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
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
	if _, err := fixture.authority.db.ExecContext(t.Context(), `UPDATE authority_meta SET claim_transition_generation = ? WHERE singleton = 1`, encodeLocalAuthorityUint64(math.MaxUint64)); err != nil {
		t.Fatalf("seed exhausted claim generation: %v", err)
	}

	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("PrepareCommandClaimTransition at generation wrap error = %v, want authority conflict", err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 0 || receipts != 0 {
		t.Fatalf("claim evidence after generation wrap = preparations:%d receipts:%d, want rolled back 0/0", preparations, receipts)
	}
}

func TestLocalNudgeAuthorityClaimAuditPagesEnforceFixedCeiling(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	tooLarge := localAuthorityRecoveryPageSize + 1
	if _, _, err := fixture.authority.localAuthorityClaimEvidencePage(t.Context(), "claim_preparations", "", tooLarge); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("localAuthorityClaimEvidencePage oversized limit error = %v, want authority conflict", err)
	}
	if _, _, err := fixture.authority.localAuthorityActiveAdmissionPage(t.Context(), 0, tooLarge); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("localAuthorityActiveAdmissionPage oversized limit error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityClaimAuditRestartsPartialCursorAfterRepositoryAdvance(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-cursor-before-rewind", OwnerID: "owner-cursor-before-rewind",
		AttemptID: "attempt-cursor-before-rewind", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before rewind = %#v, err=%v", claimed, err)
	}
	claimState, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after claim: %v", err)
	}

	partialCtx, partialBudget := withCommandAuthorityRecoveryBudget(t.Context())
	partialBudget.work = commandAuthorityRecoveryMaxWork - 1
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(partialCtx, fixture.repository, claimState); !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable {
		t.Fatalf("partial claim audit = stable:%t err:%v, want bounded yield", stable, err)
	}
	partial, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read partial claim audit cursor: %v", err)
	}
	if partial.phase != localAuthorityClaimAuditActive || partial.receiptCount != 1 {
		t.Fatalf("partial claim audit cursor = %#v, want one receipt and active phase", partial)
	}

	const requestID = "claim-audit-repository-advance"
	unrelated := repositoryCommandForRequest(t, claimState.Store, requestID, requestID)
	if entry, created, err := fixture.repository.createForTest(t.Context(), requestID, unrelated); err != nil || !created || entry.Command == nil {
		t.Fatalf("advance repository with unrelated command = %#v, created=%t err=%v", entry, created, err)
	}
	advanced, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after repository advance: %v", err)
	}
	if advanced.Revision <= partial.repositoryRevision || advanced.SequenceHighWater <= partial.sequenceHighWater {
		t.Fatalf("advanced repository state = %#v, partial cursor = %#v", advanced, partial)
	}

	exhaustedCtx, exhaustedBudget := withCommandAuthorityRecoveryBudget(t.Context())
	exhaustedBudget.work = commandAuthorityRecoveryMaxWork
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(exhaustedCtx, fixture.repository, advanced); !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable {
		t.Fatalf("claim audit reset at exhausted budget = stable:%t err:%v, want bounded yield", stable, err)
	}
	reset, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read reset claim audit cursor: %v", err)
	}
	if reset.phase != localAuthorityClaimAuditPreparations || reset.repositoryRevision != advanced.Revision ||
		reset.sequenceHighWater != advanced.SequenceHighWater || reset.afterCommandID != "" || reset.afterSequence != 0 ||
		reset.preparationCount != 0 || reset.receiptCount != 0 || reset.identity != initialLocalAuthorityClaimAuditIdentity() {
		t.Fatalf("claim audit cursor after repository advance = %#v, want fresh cursor bound to %#v", reset, advanced)
	}

	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, advanced); err != nil || !stable {
		t.Fatalf("resumed claim audit after repository advance = stable:%t err:%v, want stable", stable, err)
	}
	done, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read completed claim audit cursor: %v", err)
	}
	if done.phase != localAuthorityClaimAuditDone || done.repositoryRevision != advanced.Revision ||
		done.sequenceHighWater != advanced.SequenceHighWater || done.receiptCount != 1 {
		t.Fatalf("completed claim audit cursor = %#v, want advanced repository binding", done)
	}
}

func TestLocalNudgeAuthorityClaimAuditDoesNotMovePartialCursorOnRepositoryRewind(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	fixture.store.mu.Lock()
	rewoundRows := cloneRepositoryRows(fixture.store.rows)
	rewoundMetadata := cloneRepositoryMetadata(fixture.store.metadata)
	fixture.store.mu.Unlock()

	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-cursor-before-rewind", OwnerID: "owner-cursor-before-rewind",
		AttemptID: "attempt-cursor-before-rewind", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before rewind = %#v, err=%v", claimed, err)
	}
	claimState, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after claim: %v", err)
	}
	partialCtx, partialBudget := withCommandAuthorityRecoveryBudget(t.Context())
	partialBudget.work = commandAuthorityRecoveryMaxWork - 1
	if _, stable, err := fixture.authority.repairCommandClaimTransitions(partialCtx, fixture.repository, claimState); !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable {
		t.Fatalf("partial claim audit = stable:%t err:%v, want bounded yield", stable, err)
	}
	partial, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read partial claim audit cursor: %v", err)
	}
	if partial.phase != localAuthorityClaimAuditActive || partial.receiptCount != 1 {
		t.Fatalf("partial claim audit cursor = %#v, want one receipt and active phase", partial)
	}

	fixture.store.mu.Lock()
	fixture.store.rows = cloneRepositoryRows(rewoundRows)
	fixture.store.metadata = cloneRepositoryMetadata(rewoundMetadata)
	fixture.store.mu.Unlock()
	rewoundState := commandRepositoryStateFromMetadata(t, rewoundMetadata)
	rewoundRepository, err := NewCommandRepository(fixture.store, &repositoryLineageTestVerifier{anchor: &rewoundState})
	if err != nil {
		t.Fatalf("NewCommandRepository after restore: %v", err)
	}
	if err := fixture.authority.RecoverCommandAuthority(t.Context(), rewoundRepository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority after repository rewind error = %v, want authority conflict", err)
	}
	unchanged, err := fixture.authority.readLocalAuthorityClaimAuditCursor(t.Context(), fixture.authority.db)
	if err != nil {
		t.Fatalf("read claim audit cursor after rejected rewind: %v", err)
	}
	if unchanged != partial {
		t.Fatalf("claim audit cursor moved across rejected repository rewind: before=%#v after=%#v", partial, unchanged)
	}
}

func TestOpenLocalNudgeAuthorityRejectsIdentityReplacementAndPreservesPartialClaimCursor(t *testing.T) {
	cityPath := t.TempDir()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	now := time.Date(2026, 7, 15, 23, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		_ = authority.Close()
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		_ = authority.Close()
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	command := *admitted.Entry.Command
	claimAt := command.DeliverAfter.Add(time.Second)
	claimed, err := repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: command.ID, ClaimID: "claim-before-authority-replacement", OwnerID: "owner-before-authority-replacement",
		AttemptID: "attempt-before-authority-replacement", BoundLaunchIdentity: "launch-123", Partition: admitted.Partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, authority, authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		_ = authority.Close()
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	claimState, err := repository.State(t.Context())
	if err != nil {
		_ = authority.Close()
		t.Fatalf("State after claim: %v", err)
	}
	partialCtx, partialBudget := withCommandAuthorityRecoveryBudget(t.Context())
	partialBudget.work = commandAuthorityRecoveryMaxWork - 1
	if _, stable, err := authority.repairCommandClaimTransitions(partialCtx, repository, claimState); !errors.Is(err, ErrCommandAuthorityRecoveryYield) || stable {
		_ = authority.Close()
		t.Fatalf("partial claim audit = stable:%t err:%v, want bounded yield", stable, err)
	}
	partial, err := authority.readLocalAuthorityClaimAuditCursor(t.Context(), authority.db)
	if err != nil {
		_ = authority.Close()
		t.Fatalf("read partial claim audit cursor: %v", err)
	}
	if partial.phase != localAuthorityClaimAuditActive || partial.receiptCount != 1 {
		_ = authority.Close()
		t.Fatalf("partial claim audit cursor = %#v, want one receipt and active phase", partial)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close original authority: %v", err)
	}

	replacementOptions := localAuthorityOptions()
	replacementOptions.AuthorityID = "authority-local-replacement"
	if replacement, err := OpenLocalNudgeAuthority(t.Context(), cityPath, claimState, replacementOptions); replacement != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		if replacement != nil {
			_ = replacement.Close()
		}
		t.Fatalf("OpenLocalNudgeAuthority with replacement identity = %v, err=%v; want conflict", replacement, err)
	}
	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, claimState, localAuthorityOptions())
	if err != nil {
		t.Fatalf("reopen original authority identity: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	resumed, err := reopened.readLocalAuthorityClaimAuditCursor(t.Context(), reopened.db)
	if err != nil {
		t.Fatalf("read resumed claim audit cursor: %v", err)
	}
	if resumed != partial {
		t.Fatalf("original authority cursor changed across rejected identity replacement: before=%#v after=%#v", partial, resumed)
	}
	if err := reopened.RecoverCommandAuthority(t.Context(), repository); err != nil {
		t.Fatalf("RecoverCommandAuthority after rejected identity replacement: %v", err)
	}
}

func TestLocalNudgeAuthorityReauditsCompletedCursorAfterReopen(t *testing.T) {
	cityPath := t.TempDir()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	now := time.Date(2026, 7, 16, 1, 0, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		_ = authority.Close()
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		_ = authority.Close()
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	pending := cloneCommandValue(*admitted.Entry.Command)
	store.mu.Lock()
	pendingRow := cloneRepositoryRow(store.rows[pending.ID])
	store.mu.Unlock()
	claimAt := pending.DeliverAfter.Add(time.Second)
	claimed, err := repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: pending.ID, ClaimID: "claim-before-completed-reaudit", OwnerID: "owner-before-completed-reaudit",
		AttemptID: "attempt-before-completed-reaudit", BoundLaunchIdentity: "launch-123", Partition: admitted.Partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, authority, authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		_ = authority.Close()
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	if err := authority.RecoverCommandAuthority(t.Context(), repository); err != nil {
		_ = authority.Close()
		t.Fatalf("initial RecoverCommandAuthority: %v", err)
	}
	finalState, err := repository.State(t.Context())
	if err != nil {
		_ = authority.Close()
		t.Fatalf("final State: %v", err)
	}
	done, err := authority.readLocalAuthorityClaimAuditCursor(t.Context(), authority.db)
	if err != nil || done.phase != localAuthorityClaimAuditDone {
		_ = authority.Close()
		t.Fatalf("completed claim audit cursor = %#v, err=%v", done, err)
	}
	if err := authority.Close(); err != nil {
		t.Fatalf("Close completed authority: %v", err)
	}

	// Model accidental/out-of-band row corruption that evades the repository
	// high-water metadata. A completed cursor from a prior process must not turn
	// that metadata into a permanent exemption from startup anti-entropy.
	store.mu.Lock()
	store.rows[pending.ID] = pendingRow
	store.mu.Unlock()
	reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, finalState, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority after same-revision row rewind: %v", err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	persisted, err := reopened.readLocalAuthorityClaimAuditCursor(t.Context(), reopened.db)
	if err != nil {
		t.Fatalf("read restarted claim audit cursor: %v", err)
	}
	if persisted.phase != localAuthorityClaimAuditPreparations || persisted.repositoryRevision != finalState.Revision ||
		persisted.sequenceHighWater != finalState.SequenceHighWater || persisted.generation != done.generation ||
		persisted.afterCommandID != "" || persisted.afterSequence != 0 || persisted.preparationCount != 0 ||
		persisted.receiptCount != 0 || persisted.identity != initialLocalAuthorityClaimAuditIdentity() {
		t.Fatalf("claim audit cursor after reopen = %#v, want fresh startup audit replacing completed cursor %#v", persisted, done)
	}
	if err := reopened.RecoverCommandAuthority(t.Context(), repository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority after same-revision row rewind error = %v, want authority conflict", err)
	}
}
