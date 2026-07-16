package nudgequeue

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestCommandClaimRequestCannotSubstituteAuthoritativePayloadOrTarget(t *testing.T) {
	typ := reflect.TypeOf(CommandClaimRequest{})
	forbidden := map[string]struct{}{
		"Command": {}, "Message": {}, "Target": {}, "SessionID": {},
		"PayloadDigest": {}, "TrustedIngress": {}, "Store": {}, "CityScope": {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, found := forbidden[typ.Field(i).Name]; found {
			t.Fatalf("CommandClaimRequest exposes substitutable authoritative field %q", typ.Field(i).Name)
		}
	}
}

func TestClaimAuthorizedReturnsExactAuthoritativeClaimedCommand(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	request := fixture.claimRequest("claim-1", "owner-1", "attempt-1", fixture.now.Add(time.Second))

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil {
		t.Fatalf("ClaimAuthorized: %v", err)
	}
	if result.Disposition != CommandClaimAllowed {
		t.Fatalf("disposition = %q, want allowed", result.Disposition)
	}
	if result.HasTerminalTransitionWitness() {
		t.Fatal("allowed nonterminal claim unexpectedly carries a terminal-transition witness")
	}
	claimed := result.Command
	if claimed.State != CommandStateInFlight || claimed.Claim == nil || claimed.Retry == nil || claimed.Binding == nil {
		t.Fatalf("claimed command = %#v, want complete in-flight evidence", claimed)
	}
	if claimed.Message != fixture.command.Message || claimed.Target != fixture.command.Target || claimed.Store != fixture.command.Store || claimed.TrustedIngress != fixture.command.TrustedIngress {
		t.Fatalf("authoritative command payload/binding changed: got %#v want %#v", claimed, fixture.command)
	}
	if claimed.Binding.LaunchIdentity != request.BoundLaunchIdentity || claimed.Claim.ID != request.ClaimID ||
		claimed.Claim.OperationID != fixture.command.ID || claimed.Claim.AttemptID != request.AttemptID ||
		claimed.Claim.AuthorizationDecisionID != testClaimDecisionID || claimed.Claim.AuthorizationPolicyVersion != testClaimPolicyVersion {
		t.Fatalf("claim evidence = %#v, want exact request and current policy", claimed.Claim)
	}
	resolution, err := fixture.repository.Get(t.Context(), fixture.command.ID)
	if err != nil || !resolution.Found || resolution.Entry.Command == nil || !reflect.DeepEqual(*resolution.Entry.Command, claimed) {
		t.Fatalf("durable command = %#v, err=%v; want returned authoritative command", resolution, err)
	}
	seen := fixture.authority.lastClaimRequest()
	if seen.Command.ID != fixture.command.ID || seen.Command.Message != fixture.command.Message ||
		seen.Command.TrustedIngress.PayloadDigest != fixture.command.TrustedIngress.PayloadDigest || seen.Partition != fixture.partition {
		t.Fatalf("claim authorizer saw %#v, want exact durable command and city capability", seen)
	}
}

func TestClaimAuthorizedParksWithoutExactActiveMembership(t *testing.T) {
	for _, scenario := range []string{"missing", "wrong sequence", "authority finalized"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizedClaimFixture(t)
			fixture.authority.coverage.mu.Lock()
			record := fixture.authority.coverage.records[fixture.command.ID]
			switch scenario {
			case "missing":
				delete(fixture.authority.coverage.records, fixture.command.ID)
			case "wrong sequence":
				record.sequence++
				fixture.authority.coverage.records[fixture.command.ID] = record
			case "authority finalized":
				record.terminalRevision = fixture.command.Order.Revision
				fixture.authority.coverage.records[fixture.command.ID] = record
			}
			fixture.authority.coverage.mu.Unlock()
			before, err := fixture.repository.State(t.Context())
			if err != nil {
				t.Fatalf("State before claim: %v", err)
			}

			result, err := fixture.repository.ClaimAuthorized(t.Context(), fixture.claimRequest("claim-membership", "owner-membership", "attempt-membership", fixture.now.Add(time.Second)), fixture.authority, fixture.authority)
			if !errors.Is(err, ErrNudgeAuthorizationUnknown) || !errors.Is(err, ErrCommandRepositoryPartition) {
				t.Fatalf("ClaimAuthorized membership error = %v, want authorization-unknown partition invariant", err)
			}
			if result.Disposition != CommandClaimAuthorizationUnknown || !reflect.DeepEqual(result.Command, fixture.command) {
				t.Fatalf("ClaimAuthorized membership result = %#v, want parked %#v", result, fixture.command)
			}
			after, stateErr := fixture.repository.State(t.Context())
			if stateErr != nil || after != before {
				t.Fatalf("repository state after membership failure = %#v, err=%v; want %#v", after, stateErr, before)
			}
		})
	}
}

func TestClaimAuthorizedRevalidatesMembershipBeforeExactInFlightReplay(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	request := fixture.claimRequest("claim-membership-replay", "owner-membership", "attempt-membership", fixture.now.Add(time.Second))
	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("first ClaimAuthorized = %#v, err=%v", first, err)
	}
	fixture.authority.coverage.mu.Lock()
	delete(fixture.authority.coverage.records, fixture.command.ID)
	fixture.authority.coverage.mu.Unlock()

	replayed, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrNudgeAuthorizationUnknown) || !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("replayed ClaimAuthorized membership error = %v, want authorization-unknown partition invariant", err)
	}
	if replayed.Disposition != CommandClaimAuthorizationUnknown || !reflect.DeepEqual(replayed.Command, first.Command) {
		t.Fatalf("replayed ClaimAuthorized = %#v, want parked in-flight %#v", replayed, first.Command)
	}
}

func TestClaimAuthorizedRevocationDeniesDurablyBeforeProviderEntry(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	fixture.authority.setClaimDisposition(NudgeAuthorizationDenied)
	request := fixture.claimRequest("claim-denied", "owner-1", "attempt-denied", fixture.now.Add(time.Second))
	beforeRow, err := fixture.store.Get(fixture.command.ID)
	if err != nil {
		t.Fatalf("Get command row before denial: %v", err)
	}

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil {
		t.Fatalf("ClaimAuthorized: %v", err)
	}
	assertAuthorizationDeniedCommand(t, result)
	if !result.HasTerminalTransitionWitness() {
		t.Fatal("committed authorization denial is missing its terminal-transition witness")
	}
	tampered := result
	tampered.Command = cloneCommandValue(result.Command)
	tampered.Command.Terminal.Detail += " tampered"
	if tampered.HasTerminalTransitionWitness() {
		t.Fatal("terminal-transition witness remained valid after returned command mutation")
	}
	afterRow, err := fixture.store.Get(fixture.command.ID)
	if err != nil {
		t.Fatalf("Get command row after denial: %v", err)
	}
	if afterRow.Status != "closed" || !afterRow.UpdatedAt.After(beforeRow.UpdatedAt) {
		t.Fatalf("denied command row = status %q updated_at %s, want closed and after %s", afterRow.Status, afterRow.UpdatedAt, beforeRow.UpdatedAt)
	}
	if calls := fixture.authority.claimCalls(); calls != 1 {
		t.Fatalf("claim authorization calls = %d, want one", calls)
	}

	retry, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil {
		t.Fatalf("ClaimAuthorized denied retry: %v", err)
	}
	assertAuthorizationDeniedCommand(t, retry)
	if !retry.HasTerminalTransitionWitness() {
		t.Fatal("prepared authorization denial retry is missing its repair witness")
	}
	if calls := fixture.authority.claimCalls(); calls != 1 {
		t.Fatalf("denied retry re-entered policy: calls=%d", calls)
	}
}

func TestClaimAuthorizedAbortsTerminalIntentOnDefiniteCallbackRollback(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	fixture.authority.setClaimDisposition(NudgeAuthorizationDenied)
	fixture.store.failNextCommandUpdate(errors.New("injected denial update failure"))
	request := fixture.claimRequest("claim-denied-rollback", "owner-rollback", "attempt-rollback", fixture.now.Add(time.Second))

	if _, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority); err == nil {
		t.Fatal("ClaimAuthorized error = nil, want update failure")
	}
	if count := fixture.authority.terminalIntentCount(); count != 0 {
		t.Fatalf("terminal preparations after definite claim rollback = %d, want zero", count)
	}
	resolved, err := fixture.repository.Get(t.Context(), fixture.command.ID)
	if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.State != CommandStatePending {
		t.Fatalf("command after definite claim rollback = %#v, err=%v", resolved, err)
	}
}

func TestClaimAuthorizedRepairsPreparedTerminalAfterLineageAdvanceFailure(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	fixture.authority.setClaimDisposition(NudgeAuthorizationDenied)
	verifier, ok := fixture.repository.writer.(*repositoryLineageTestVerifier)
	if !ok {
		t.Fatalf("repository writer = %T, want test verifier", fixture.repository.writer)
	}
	verifier.failNextAdvance(errors.New("lineage authority temporarily unavailable"))
	request := fixture.claimRequest("claim-denied-lineage", "owner-lineage", "attempt-lineage", fixture.now.Add(time.Second))

	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err == nil || first != (CommandClaimResult{}) {
		t.Fatalf("ClaimAuthorized first = %#v, err=%v; want post-commit lineage failure", first, err)
	}
	if count := fixture.authority.terminalIntentCount(); count != 1 {
		t.Fatalf("terminal preparations after lineage failure = %d, want one retained", count)
	}

	repaired, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || repaired.Disposition != CommandClaimDenied || !repaired.HasTerminalTransitionWitness() {
		t.Fatalf("ClaimAuthorized lineage repair = %#v, err=%v", repaired, err)
	}
}

func TestClaimAuthorizedUnknownParksWithoutMutation(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	fixture.authority.setClaimDisposition(NudgeAuthorizationUnknown)
	request := fixture.claimRequest("claim-unknown", "owner-1", "attempt-unknown", fixture.now.Add(time.Second))
	before, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before: %v", err)
	}

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil {
		t.Fatalf("ClaimAuthorized: %v", err)
	}
	if result.Disposition != CommandClaimAuthorizationUnknown || !reflect.DeepEqual(result.Command, fixture.command) {
		t.Fatalf("unknown result = %#v, want unchanged pending command", result)
	}
	after, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after: %v", err)
	}
	if after != before {
		t.Fatalf("repository changed across authorization unknown: before=%#v after=%#v", before, after)
	}
}

func TestClaimAuthorizedAuthorityOutageReturnsParkedCommandAndTypedError(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	fixture.authority.setClaimError(errors.New("policy unavailable"))
	request := fixture.claimRequest("claim-outage", "owner-1", "attempt-outage", fixture.now.Add(time.Second))

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrNudgeAuthorizationUnknown) {
		t.Fatalf("ClaimAuthorized error = %v, want authorization unknown", err)
	}
	if result.Disposition != CommandClaimAuthorizationUnknown || !reflect.DeepEqual(result.Command, fixture.command) {
		t.Fatalf("outage result = %#v, want parked authoritative command", result)
	}
	resolution, getErr := fixture.repository.Get(t.Context(), fixture.command.ID)
	if getErr != nil || resolution.Entry.Command == nil || !reflect.DeepEqual(*resolution.Entry.Command, fixture.command) {
		t.Fatalf("durable command changed during outage: %#v err=%v", resolution, getErr)
	}
}

func TestClaimAuthorizedRejectsDirectStoreStampAndCrossCityReplay(t *testing.T) {
	for _, scenario := range []string{"unrecognized ingress", "cross city"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizedClaimFixture(t)
			if scenario == "unrecognized ingress" {
				fixture.authority.forgetReference(fixture.command.TrustedIngress.ReferenceID)
			}
			request := fixture.claimRequest("claim-forged", "owner-1", "attempt-forged", fixture.now.Add(time.Second))
			if scenario == "cross city" {
				request.Partition = trustedCityPartitionForTest("foreign-city")
			}
			result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
			if scenario == "cross city" {
				if !errors.Is(err, ErrNudgeAuthorizationUnknown) || !errors.Is(err, ErrCommandRepositoryPartition) {
					t.Fatalf("ClaimAuthorized cross-city error = %v, want parked partition invariant", err)
				}
				if result.Disposition != CommandClaimAuthorizationUnknown || !reflect.DeepEqual(result.Command, fixture.command) {
					t.Fatalf("ClaimAuthorized cross-city = %#v, want unchanged pending command", result)
				}
				return
			}
			if err != nil {
				t.Fatalf("ClaimAuthorized: %v", err)
			}
			assertAuthorizationDeniedCommand(t, result)
		})
	}
}

func TestClaimAuthorizedExpiredIngressDeniesAndExpiredCommandTerminalizes(t *testing.T) {
	for _, scenario := range []string{"ingress expired", "command expired"} {
		t.Run(scenario, func(t *testing.T) {
			fixture := newAuthorizedClaimFixture(t)
			beforeRow, err := fixture.store.Get(fixture.command.ID)
			if err != nil {
				t.Fatalf("Get command row before expiry: %v", err)
			}
			claimAt := fixture.now.Add(2 * time.Second)
			if scenario == "ingress expired" {
				fixture.authority.expireReference(fixture.command.TrustedIngress.ReferenceID, fixture.now.Add(time.Second))
				fixture.rewriteIngressExpiry(t, fixture.now.Add(time.Second))
			} else {
				claimAt = fixture.command.ExpiresAt
			}
			request := fixture.claimRequest("claim-expired", "owner-1", "attempt-expired", claimAt)
			result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
			if err != nil {
				t.Fatalf("ClaimAuthorized: %v", err)
			}
			if result.Disposition != CommandClaimDenied || result.Command.Terminal == nil || result.Command.Terminal.ProviderStage != ProviderStageNotEntered {
				t.Fatalf("expired result = %#v, want durable pre-provider denial", result)
			}
			if scenario == "ingress expired" && result.Command.Terminal.ActionResult != CommandActionResultAuthorizationDenied {
				t.Fatalf("ingress expiry result = %q, want authorization_denied", result.Command.Terminal.ActionResult)
			}
			if scenario == "command expired" && result.Command.Terminal.ActionResult != CommandActionResultExpired {
				t.Fatalf("command expiry result = %q, want expired", result.Command.Terminal.ActionResult)
			}
			afterRow, err := fixture.store.Get(fixture.command.ID)
			if err != nil {
				t.Fatalf("Get command row after expiry: %v", err)
			}
			if afterRow.Status != "closed" || !afterRow.UpdatedAt.After(beforeRow.UpdatedAt) {
				t.Fatalf("expired command row = status %q updated_at %s, want closed and after %s", afterRow.Status, afterRow.UpdatedAt, beforeRow.UpdatedAt)
			}
		})
	}
}

func TestClaimAuthorizedCommitResponseLossAndDuplicateRetryConverge(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	request := fixture.claimRequest("claim-lost-response", "owner-1", "attempt-1", fixture.now.Add(time.Second))
	fixture.store.mu.Lock()
	fixture.store.failAfterCommitNext = errors.New("lost claim commit response")
	fixture.store.mu.Unlock()

	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized lost response = %#v, err=%v", first, err)
	}
	second, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || second.Disposition != CommandClaimEntryUnknown || !reflect.DeepEqual(second.Command, first.Command) {
		t.Fatalf("ClaimAuthorized duplicate = %#v, err=%v; want entry unknown with %#v", second, err, first.Command)
	}
	if second.Command.Retry == nil || second.Command.Retry.AttemptCount != 1 {
		t.Fatalf("duplicate attempt evidence = %#v, want one attempt", second.Command.Retry)
	}
}

func TestClaimAuthorizedRecoversExactTerminalTransitionWitnessAfterCommitResponseLoss(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	request := fixture.claimRequest("claim-lost-terminal-response", "owner-1", "attempt-1", fixture.command.ExpiresAt)
	fixture.store.mu.Lock()
	fixture.store.failAfterCommitNext = errors.New("lost terminal claim commit response")
	fixture.store.mu.Unlock()

	first, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimDenied || first.Command.Terminal == nil {
		t.Fatalf("ClaimAuthorized lost terminal response = %#v, err=%v", first, err)
	}
	if !first.HasTerminalTransitionWitness() {
		t.Fatal("recovered terminal transition is missing its causal witness")
	}
	second, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if err != nil || second.Disposition != CommandClaimDenied || !reflect.DeepEqual(second.Command, first.Command) {
		t.Fatalf("ClaimAuthorized terminal duplicate = %#v, err=%v; want %#v", second, err, first)
	}
	if !second.HasTerminalTransitionWitness() {
		t.Fatal("prepared terminal duplicate is missing its repair witness")
	}
}

func TestClaimAuthorizedLeaseRaceHasOneWinner(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	requests := []CommandClaimRequest{
		fixture.claimRequest("claim-a", "owner-a", "attempt-a", fixture.now.Add(time.Second)),
		fixture.claimRequest("claim-b", "owner-b", "attempt-b", fixture.now.Add(time.Second)),
	}
	results := make(chan CommandClaimResult, 2)
	errs := make(chan error, 2)
	var wg sync.WaitGroup
	for _, request := range requests {
		request := request
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
			results <- result
			errs <- err
		}()
	}
	wg.Wait()
	close(results)
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("ClaimAuthorized race: %v", err)
		}
	}
	counts := map[CommandClaimDisposition]int{}
	for result := range results {
		counts[result.Disposition]++
	}
	if counts[CommandClaimAllowed] != 1 || counts[CommandClaimBusy] != 1 {
		t.Fatalf("race dispositions = %#v, want one allowed and one busy", counts)
	}
}

func TestClaimAuthorizedExpiredInFlightLeaseRemainsBusyWithoutDefiniteNotEnteredEvidence(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	firstRequest := fixture.claimRequest("claim-first", "owner-first", "attempt-first", fixture.now.Add(time.Second))
	first, err := fixture.repository.ClaimAuthorized(t.Context(), firstRequest, fixture.authority, fixture.authority)
	if err != nil || first.Disposition != CommandClaimAllowed {
		t.Fatalf("first ClaimAuthorized = %#v, err=%v; want allowed", first, err)
	}

	reclaimAt := firstRequest.LeaseUntil.Add(time.Second)
	replacementRequest := fixture.claimRequest("claim-replacement", "owner-replacement", "attempt-replacement", reclaimAt)
	replacement, err := fixture.repository.ClaimAuthorized(t.Context(), replacementRequest, fixture.authority, fixture.authority)
	if err != nil {
		t.Fatalf("replacement ClaimAuthorized: %v", err)
	}
	if replacement.Disposition != CommandClaimBusy || !reflect.DeepEqual(replacement.Command, first.Command) {
		t.Fatalf("replacement after expired lease = %#v, want busy with unchanged in-flight command %#v", replacement, first.Command)
	}
	if calls := fixture.authority.claimCalls(); calls != 1 {
		t.Fatalf("expired lease re-entered authorization policy: calls=%d, want 1", calls)
	}
	resolution, err := fixture.repository.Get(t.Context(), fixture.command.ID)
	if err != nil || resolution.Entry.Command == nil || !reflect.DeepEqual(*resolution.Entry.Command, first.Command) {
		t.Fatalf("durable command changed after expired-lease reclaim: %#v err=%v", resolution, err)
	}
}

func TestClaimAuthorizedAcceptsCurrentAndPreviousPrincipalSchema(t *testing.T) {
	for _, schema := range []uint32{NudgePrincipalSchemaVersion, NudgePrincipalSchemaVersion - 1} {
		fixture := newAuthorizedClaimFixture(t)
		fixture.authority.setClaimSchema(schema)
		result, err := fixture.repository.ClaimAuthorized(t.Context(), fixture.claimRequest("claim-schema", "owner", "attempt", fixture.now.Add(time.Second)), fixture.authority, fixture.authority)
		if err != nil || result.Disposition != CommandClaimAllowed {
			t.Fatalf("schema %d result = %#v, err=%v", schema, result, err)
		}
	}
}

func assertAuthorizationDeniedCommand(t *testing.T, result CommandClaimResult) {
	t.Helper()
	if result.Disposition != CommandClaimDenied || result.Command.State != CommandStateDeadLettered || result.Command.Terminal == nil {
		t.Fatalf("denied result = %#v, want durable terminal denial", result)
	}
	terminal := result.Command.Terminal
	if terminal.ActionResult != CommandActionResultAuthorizationDenied || terminal.ErrorClass != CommandErrorClassAuthorizationDenied ||
		terminal.AuthorizationDecisionID != testClaimDecisionID || terminal.AuthorizationPolicyVersion != testClaimPolicyVersion ||
		terminal.ProviderStage != ProviderStageNotEntered || terminal.Completion != CompletionStateNotCompleted ||
		result.Command.Claim != nil || result.Command.Retry != nil {
		t.Fatalf("denial evidence = command %#v terminal %#v", result.Command, terminal)
	}
}

type authorizedClaimFixture struct {
	repository *CommandRepository
	store      *repositoryAtomicTestStore
	authority  *testNudgeAuthority
	ingress    *TrustedNudgeIngress
	command    Command
	partition  TrustedCityPartition
	now        time.Time
}

func newAuthorizedClaimFixture(t *testing.T) *authorizedClaimFixture {
	t.Helper()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)
	request.ExpiresAt = now.Add(30 * time.Minute)
	result, err := ingress.Admit(t.Context(), request)
	if err != nil || result.Entry.Command == nil {
		t.Fatalf("Admit fixture = %#v, err=%v", result, err)
	}
	return &authorizedClaimFixture{
		repository: repository,
		store:      store,
		authority:  authority,
		ingress:    ingress,
		command:    *result.Entry.Command,
		partition:  result.Partition,
		now:        now,
	}
}

func (f *authorizedClaimFixture) claimRequest(claimID, ownerID, attemptID string, claimedAt time.Time) CommandClaimRequest {
	return CommandClaimRequest{
		CommandID:           f.command.ID,
		ClaimID:             claimID,
		OwnerID:             ownerID,
		AttemptID:           attemptID,
		BoundLaunchIdentity: "launch-123",
		Partition:           f.partition,
		ClaimedAt:           claimedAt,
		LeaseUntil:          claimedAt.Add(time.Minute),
	}
}

func (f *authorizedClaimFixture) rewriteIngressExpiry(t *testing.T, expiresAt time.Time) {
	t.Helper()
	f.store.mu.Lock()
	defer f.store.mu.Unlock()
	row := f.store.rows[f.command.ID]
	decoded := DecodeCommand([]byte(row.Metadata[commandRecordWireMetadataKey]))
	if decoded.Disposition != CommandDecodeDecoded {
		t.Fatalf("DecodeCommand fixture: %#v", decoded)
	}
	command := decoded.Command
	command.TrustedIngress.ExpiresAt = expiresAt
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1 fixture: %v", err)
	}
	row.Metadata[commandRecordWireMetadataKey] = string(wire)
	f.store.rows[f.command.ID] = row
	f.command = command
}
