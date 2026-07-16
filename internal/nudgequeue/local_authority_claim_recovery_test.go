package nudgequeue

import (
	"database/sql"
	"errors"
	"net/url"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityRecoveryRejectsOrphanInFlightCommand(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	authority := &failOnceLocalClaimTransitionAuthority{
		LocalNudgeAuthority: fixture.authority,
		nextErr:             errors.New("injected claim receipt finalization failure"),
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-orphan-in-flight", OwnerID: "owner-orphan-in-flight",
		AttemptID: "attempt-orphan-in-flight", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, authority)
	if err == nil || first.Disposition != CommandClaimAllowed || first.Command.State != CommandStateInFlight {
		t.Fatalf("ClaimAuthorized before orphaning = %#v, err=%v", first, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `DELETE FROM claim_preparations WHERE command_id = ?`, fixture.command.ID); err != nil {
		t.Fatalf("delete claim preparation: %v", err)
	}

	if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority orphan in-flight error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryPreservesLiveWriterPreparation(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-live-recovery", OwnerID: "owner-live-recovery",
		AttemptID: "attempt-live-recovery", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
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

	if _, stable, err := fixture.authority.repairCommandClaimTransitions(t.Context(), fixture.repository, state); err != nil || stable {
		t.Fatalf("repairCommandClaimTransitions across live writer = stable:%t err:%v, want deferred", stable, err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 1 || receipts != 0 {
		t.Fatalf("claim evidence after live-writer recovery = preparations:%d receipts:%d, want 1/0", preparations, receipts)
	}
	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition cleanup: %v", err)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter cleanup: %v", err)
	}
}

func TestLocalNudgeAuthorityExactPeerAbortCannotEraseCommittingClaim(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-peer-commit", OwnerID: "owner-peer-commit",
		AttemptID: "attempt-peer-commit", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
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
		t.Fatalf("PrepareCommandClaimTransition first peer: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition committing peer: %v", err)
	}
	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition failed peer: %v", err)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter failed peer: %v", err)
	}

	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed || claimed.Command.State != CommandStateInFlight {
		t.Fatalf("ClaimAuthorized committing peer = %#v, err=%v", claimed, err)
	}
	if preparations, receipts := localClaimTransitionCounts(t, fixture.authority); preparations != 0 || receipts != 1 {
		t.Fatalf("claim evidence after peer commit = preparations:%d receipts:%d, want 0/1", preparations, receipts)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter finalized peer: %v", err)
	}
}

func TestLocalNudgeAuthorityRecoveryRejectsSurgicallyRewoundClaimRow(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	fixture.store.mu.Lock()
	pendingRow := cloneRepositoryRow(fixture.store.rows[fixture.command.ID])
	fixture.store.mu.Unlock()

	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	result, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-surgical-rewind", OwnerID: "owner-surgical-rewind",
		AttemptID: "attempt-surgical-rewind", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || result.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized before surgical rewind = %#v, err=%v", result, err)
	}
	fixture.store.mu.Lock()
	fixture.store.rows[fixture.command.ID] = pendingRow
	fixture.store.mu.Unlock()

	if err := fixture.authority.RecoverCommandAuthority(t.Context(), fixture.repository); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("RecoverCommandAuthority after surgical row rewind error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityTerminalPublicationConsumesClaimReceiptAtomically(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-terminal-consumption", OwnerID: "owner-terminal-consumption",
		AttemptID: "attempt-terminal-consumption", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	beforeToken, err := fixture.authority.snapshotClaimRecoveryToken(t.Context())
	if err != nil || beforeToken.receipts != 1 {
		t.Fatalf("claim recovery token before terminal = %#v, err=%v; want one receipt", beforeToken, err)
	}
	completion := providerAttemptCompletion(claimed.Command, CommandActionResultInjectedUnconfirmed)
	terminal, err := fixture.repository.CompleteProviderAttempt(t.Context(), completion, fixture.partition, fixture.authority)
	if err != nil || !terminal.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", terminal, err)
	}
	if err := recordLocalAuthorityTerminal(t.Context(), fixture.authority, fixture.partition, terminal.Command); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal: %v", err)
	}
	afterToken, err := fixture.authority.snapshotClaimRecoveryToken(t.Context())
	if err != nil {
		t.Fatalf("snapshotClaimRecoveryToken after terminal: %v", err)
	}
	if afterToken.preparations != 0 || afterToken.receipts != 0 || afterToken.generation <= beforeToken.generation {
		t.Fatalf("claim recovery token after terminal = %#v, before %#v; want empty evidence and advanced generation", afterToken, beforeToken)
	}
}

func TestLocalNudgeAuthorityClaimRecoveryTokenRejectsEmptySetABA(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	request := CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-generation-aba", OwnerID: "owner-generation-aba",
		AttemptID: "attempt-generation-aba", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
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
	beforeToken, err := fixture.authority.snapshotClaimRecoveryToken(t.Context())
	if err != nil {
		t.Fatalf("snapshotClaimRecoveryToken before ABA: %v", err)
	}
	if err := fixture.authority.PrepareCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandClaimTransition: %v", err)
	}
	if err := fixture.authority.AbortCommandClaimTransition(t.Context(), intent); err != nil {
		t.Fatalf("AbortCommandClaimTransition: %v", err)
	}
	if err := fixture.authority.ReleaseCommandClaimTransitionWriter(t.Context(), intent); err != nil {
		t.Fatalf("ReleaseCommandClaimTransitionWriter: %v", err)
	}
	afterToken, err := fixture.authority.snapshotClaimRecoveryToken(t.Context())
	if err != nil {
		t.Fatalf("snapshotClaimRecoveryToken after ABA: %v", err)
	}
	if beforeToken.preparations != 0 || beforeToken.receipts != 0 || afterToken.preparations != 0 || afterToken.receipts != 0 || beforeToken == afterToken {
		t.Fatalf("empty-set ABA tokens = before:%#v after:%#v; want identical membership but distinct generation identity", beforeToken, afterToken)
	}
}

func TestLocalNudgeAuthorityClaimReceiptMustBeDominatedByAuthorityMetadata(t *testing.T) {
	fixture := newLocalAuthorityPendingFixture(t)
	claimAt := fixture.command.DeliverAfter.Add(time.Second)
	claimed, err := fixture.repository.ClaimAuthorized(t.Context(), CommandClaimRequest{
		CommandID: fixture.command.ID, ClaimID: "claim-metadata-dominance", OwnerID: "owner-metadata-dominance",
		AttemptID: "attempt-metadata-dominance", BoundLaunchIdentity: "launch-123", Partition: fixture.partition,
		ClaimedAt: claimAt, LeaseUntil: claimAt.Add(time.Minute),
	}, fixture.authority, fixture.authority)
	if err != nil || claimed.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claimed, err)
	}
	if _, err := fixture.authority.db.ExecContext(t.Context(), `UPDATE authority_meta SET highest_observed_revision = ? WHERE singleton = 1`, encodeLocalAuthorityUint64(claimed.Command.Order.Revision-1)); err != nil {
		t.Fatalf("rewind authority metadata beneath receipt: %v", err)
	}
	if _, _, err := localAuthorityClaimReceiptByCommand(t.Context(), fixture.authority.db, fixture.command.Store, fixture.command.ID); !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("read receipt ahead of authority metadata error = %v, want authority conflict", err)
	}
}

func TestLocalNudgeAuthorityRefusesOldOrMissingClaimTransitionSchema(t *testing.T) {
	for _, scenario := range []string{"old schema version", "missing claim preparations", "missing claim receipts"} {
		t.Run(scenario, func(t *testing.T) {
			cityPath := t.TempDir()
			authority, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
			if err != nil {
				t.Fatalf("OpenLocalNudgeAuthority: %v", err)
			}
			if err := authority.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
			dsn := (&url.URL{Scheme: "file", Path: LocalNudgeAuthorityPath(cityPath)}).String()
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatalf("open schema fixture: %v", err)
			}
			switch scenario {
			case "old schema version":
				_, err = db.Exec(`UPDATE authority_meta SET schema_version = 2`)
			case "missing claim preparations":
				_, err = db.Exec(`DROP TABLE claim_preparations`)
			case "missing claim receipts":
				_, err = db.Exec(`DROP TABLE claim_receipts`)
			}
			if err != nil {
				_ = db.Close()
				t.Fatalf("mutate authority schema: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatalf("close schema fixture: %v", err)
			}
			reopened, err := OpenLocalNudgeAuthority(t.Context(), cityPath, localAuthorityRepositoryState(), localAuthorityOptions())
			if reopened != nil || !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
				if reopened != nil {
					_ = reopened.Close()
				}
				t.Fatalf("OpenLocalNudgeAuthority(%s) = %v, err=%v; want conflict", scenario, reopened, err)
			}
		})
	}
}
