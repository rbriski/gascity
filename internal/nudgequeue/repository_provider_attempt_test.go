package nudgequeue

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCommandRepositoryCompleteProviderAttemptCommitsMarkerLast(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	before, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before completion: %v", err)
	}
	request := providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed)

	result, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if result.Disposition != CommandCompletionRecorded {
		t.Fatalf("completion disposition = %q, want recorded", result.Disposition)
	}
	if !result.HasTerminalTransitionWitness() {
		t.Fatal("recorded completion is missing its terminal-transition witness")
	}
	tampered := result
	tampered.Command = cloneCommandValue(result.Command)
	tampered.Command.Terminal.Detail = "tampered terminal detail"
	if tampered.HasTerminalTransitionWitness() {
		t.Fatal("terminal-transition witness remained valid after returned command mutation")
	}
	assertInjectedUnconfirmedCompletion(t, result.Command, request)

	row, err := store.Get(command.ID)
	if err != nil {
		t.Fatalf("Get completed row: %v", err)
	}
	if row.Status != "closed" {
		t.Fatalf("completed row status = %q, want closed", row.Status)
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil {
		t.Fatalf("Get completed command = %#v, err=%v", resolved, err)
	}
	assertInjectedUnconfirmedCompletion(t, *resolved.Entry.Command, request)
	after, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State after completion: %v", err)
	}
	if after.Revision != before.Revision+1 || after.SequenceHighWater != before.SequenceHighWater {
		t.Fatalf("completion watermarks = revision %d sequence %d, want %d/%d", after.Revision, after.SequenceHighWater, before.Revision+1, before.SequenceHighWater)
	}

	repeated, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt repeated: %v", err)
	}
	if repeated.Disposition != CommandCompletionAlreadyRecorded || repeated.Command.Order.Revision != after.Revision {
		t.Fatalf("repeated completion = %#v, want same durable terminal", repeated)
	}
	if !repeated.HasTerminalTransitionWitness() {
		t.Fatal("prepared already-recorded completion is missing its terminal-transition witness")
	}
	stable, err := repository.State(t.Context())
	if err != nil || stable != after {
		t.Fatalf("state after repeated completion = %#v, err=%v; want %#v", stable, err, after)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRecordsDeliveryUnknown(t *testing.T) {
	repository, _, command, partition, authority := seedRepositoryProviderAttempt(t)
	request := providerAttemptCompletion(command, CommandActionResultDeliveryUnknown)

	result, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if result.Disposition != CommandCompletionRecorded || result.Command.State != CommandStateDeliveryUnknown || result.Command.Claim != nil {
		t.Fatalf("delivery-unknown completion = %#v", result)
	}
	terminal := result.Command.Terminal
	if terminal == nil || terminal.ProviderStage != ProviderStageMayHaveEntered || terminal.Completion != CompletionStateUnknown || terminal.ErrorClass != CommandErrorClassProviderAmbiguous {
		t.Fatalf("delivery-unknown terminal = %#v", terminal)
	}
}

func TestCommandRepositoryCompleteProviderAttemptIsAtomicOnWriteFailure(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	before, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before completion: %v", err)
	}
	store.failNextCommit(errors.New("injected atomic failure"))

	if _, err := repository.CompleteProviderAttempt(t.Context(), providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed), partition, authority); err == nil {
		t.Fatal("CompleteProviderAttempt error = nil, want injected failure")
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil {
		t.Fatalf("Get after failed completion = %#v, err=%v", resolved, err)
	}
	if resolved.Entry.Command.State != CommandStateInFlight || resolved.Entry.Command.Claim == nil || resolved.Entry.Command.Terminal != nil {
		t.Fatalf("failed completion changed command = %#v", resolved.Entry.Command)
	}
	row, err := store.Get(command.ID)
	if err != nil || row.Status != "open" {
		t.Fatalf("failed completion row = %#v, err=%v; want open", row, err)
	}
	after, err := repository.State(t.Context())
	if err != nil || after != before {
		t.Fatalf("state after failed completion = %#v, err=%v; want %#v", after, err, before)
	}
}

func TestCommandRepositoryCompleteProviderAttemptAbortsIntentOnDefiniteCallbackRollback(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	store.failNextCommandUpdate(errors.New("injected update failure after terminal prepare"))

	if _, err := repository.CompleteProviderAttempt(t.Context(), providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed), partition, authority); err == nil {
		t.Fatal("CompleteProviderAttempt error = nil, want update failure")
	}
	if count := authority.terminalIntentCount(); count != 0 {
		t.Fatalf("terminal preparations after definite rollback = %d, want zero", count)
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.State != CommandStateInFlight {
		t.Fatalf("command after definite rollback = %#v, err=%v", resolved, err)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRetainsIntentOnAmbiguousCommit(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	store.failNextCommit(errors.New("commit outcome unavailable"))
	request := providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed)

	if _, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority); err == nil {
		t.Fatal("CompleteProviderAttempt error = nil, want ambiguous commit")
	}
	if count := authority.terminalIntentCount(); count != 1 {
		t.Fatalf("terminal preparations after ambiguous commit = %d, want one retained", count)
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.State != CommandStateInFlight {
		t.Fatalf("command after ambiguous rolled-back test commit = %#v, err=%v", resolved, err)
	}

	repaired, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil || repaired.Disposition != CommandCompletionRecorded || !repaired.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt exact retry = %#v, err=%v", repaired, err)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRepairsAfterLineageAdvanceFailure(t *testing.T) {
	repository, _, command, partition, authority := seedRepositoryProviderAttempt(t)
	verifier, ok := repository.writer.(*repositoryLineageTestVerifier)
	if !ok {
		t.Fatalf("repository writer = %T, want test verifier", repository.writer)
	}
	verifier.failNextAdvance(errors.New("lineage authority temporarily unavailable"))
	request := providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed)

	first, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err == nil || first != (CommandCompletionResult{}) {
		t.Fatalf("CompleteProviderAttempt first = %#v, err=%v; want post-commit lineage failure", first, err)
	}
	if count := authority.terminalIntentCount(); count != 1 {
		t.Fatalf("terminal preparations after lineage failure = %d, want one retained", count)
	}

	repaired, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil || repaired.Disposition != CommandCompletionAlreadyRecorded || !repaired.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt lineage repair = %#v, err=%v", repaired, err)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRejectsCompetingPreparedTerminal(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	store.failNextCommit(errors.New("first commit outcome unavailable"))
	if _, err := repository.CompleteProviderAttempt(t.Context(), providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed), partition, authority); err == nil {
		t.Fatal("first CompleteProviderAttempt error = nil, want ambiguous commit")
	}

	competing := providerAttemptCompletion(command, CommandActionResultDeliveryUnknown)
	if _, err := repository.CompleteProviderAttempt(t.Context(), competing, partition, authority); !errors.Is(err, ErrCommandPartitionTerminalIntent) {
		t.Fatalf("competing CompleteProviderAttempt error = %v, want terminal-intent conflict", err)
	}
	if count := authority.terminalIntentCount(); count != 1 {
		t.Fatalf("terminal preparations after competing outcome = %d, want one", count)
	}
}

func TestCommandRepositoryCompleteProviderAttemptResolvesLostCommitResponse(t *testing.T) {
	repository, store, command, partition, authority := seedRepositoryProviderAttempt(t)
	store.mu.Lock()
	store.failAfterCommitNext = errors.New("lost completion response")
	store.mu.Unlock()

	result, err := repository.CompleteProviderAttempt(t.Context(), providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed), partition, authority)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt after lost response: %v", err)
	}
	if result.Disposition != CommandCompletionAlreadyRecorded {
		t.Fatalf("lost-response disposition = %q, want already recorded", result.Disposition)
	}
	if result.Command.State != CommandStateInjectedUnconfirmed || result.Command.Terminal == nil {
		t.Fatalf("lost-response command = %#v", result.Command)
	}
	if !result.HasTerminalTransitionWitness() {
		t.Fatal("recovered completion is missing its terminal-transition witness")
	}
}

func TestCommandRepositoryCompleteProviderAttemptSerializesConcurrentOutcomes(t *testing.T) {
	repository, _, command, partition, authority := seedRepositoryProviderAttempt(t)
	requests := []CommandCompletionRequest{
		providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed),
		providerAttemptCompletion(command, CommandActionResultDeliveryUnknown),
	}
	results := make([]CommandCompletionResult, len(requests))
	errs := make([]error, len(requests))
	var wg sync.WaitGroup
	for i := range requests {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			results[index], errs[index] = repository.CompleteProviderAttempt(t.Context(), requests[index], partition, authority)
		}(i)
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("completion %d: %v", i, err)
		}
	}
	recorded := 0
	for _, result := range results {
		if result.Disposition == CommandCompletionRecorded {
			recorded++
		}
		if result.Command.Terminal == nil {
			t.Fatalf("concurrent completion returned nonterminal command: %#v", result)
		}
	}
	if recorded != 1 {
		t.Fatalf("recorded completions = %d, want exactly 1; results=%#v", recorded, results)
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || resolved.Entry.Command == nil || resolved.Entry.Command.Terminal == nil {
		t.Fatalf("Get after concurrent completion = %#v, err=%v", resolved, err)
	}
	if resolved.Entry.Command.State != CommandStateInjectedUnconfirmed && resolved.Entry.Command.State != CommandStateDeliveryUnknown {
		t.Fatalf("concurrent terminal state = %q", resolved.Entry.Command.State)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRejectsStaleAttemptWithoutMutation(t *testing.T) {
	repository, _, command, partition, authority := seedRepositoryProviderAttempt(t)
	request := providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed)
	request.AttemptID = "different-attempt"
	before, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before stale completion: %v", err)
	}

	result, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt stale: %v", err)
	}
	if result.Disposition != CommandCompletionStale || result.Command.State != CommandStateInFlight {
		t.Fatalf("stale completion = %#v", result)
	}
	if result.HasTerminalTransitionWitness() {
		t.Fatal("stale completion unexpectedly carries a terminal-transition witness")
	}
	after, err := repository.State(t.Context())
	if err != nil || after != before {
		t.Fatalf("state after stale completion = %#v, err=%v; want %#v", after, err, before)
	}
}

func TestCommandRepositoryCompleteProviderAttemptRejectsContradictoryOutcomeWithoutMutation(t *testing.T) {
	repository, _, command, partition, authority := seedRepositoryProviderAttempt(t)
	request := providerAttemptCompletion(command, CommandActionResultInjectedUnconfirmed)
	request.Completion = CompletionStateUnknown
	before, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State before contradictory completion: %v", err)
	}

	if _, err := repository.CompleteProviderAttempt(t.Context(), request, partition, authority); !errors.Is(err, ErrCommandProviderAttemptInvalid) {
		t.Fatalf("CompleteProviderAttempt error = %v, want invalid provider attempt", err)
	}
	after, err := repository.State(t.Context())
	if err != nil || after != before {
		t.Fatalf("state after contradictory completion = %#v, err=%v; want %#v", after, err, before)
	}
}

func seedRepositoryProviderAttempt(t *testing.T) (*CommandRepository, *repositoryAtomicTestStore, Command, TrustedCityPartition, *testNudgeAuthority) {
	t.Helper()
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	const requestID = "provider-attempt-request"
	created, wasCreated, err := repository.createForTest(t.Context(), requestID, repositoryCommandForRequest(t, state.Store, requestID, "provider-attempt"))
	if err != nil || !wasCreated || created.Command == nil {
		t.Fatalf("Create = %#v, created=%v, err=%v", created, wasCreated, err)
	}
	command := *created.Command
	claimAt := command.DeliverAfter.Add(time.Second)
	command.State = CommandStateInFlight
	command.Binding = &CommandBinding{LaunchIdentity: "launch-provider-attempt", BoundAt: claimAt}
	command.Claim = &CommandClaim{
		ID:                         "claim-provider-attempt",
		OwnerID:                    "controller-provider-attempt",
		OperationID:                command.ID,
		AttemptID:                  "attempt-provider-attempt",
		BoundLaunchIdentity:        command.Binding.LaunchIdentity,
		AuthorizationDecisionID:    "decision-provider-attempt",
		AuthorizationPolicyVersion: "policy-provider-attempt",
		ClaimedAt:                  claimAt,
		LeaseUntil:                 claimAt.Add(time.Minute),
	}
	command.Retry = &CommandRetry{
		AttemptCount:               1,
		LastAttemptAt:              claimAt,
		ClaimID:                    command.Claim.ID,
		OperationID:                command.Claim.OperationID,
		AttemptID:                  command.Claim.AttemptID,
		BoundLaunchIdentity:        command.Claim.BoundLaunchIdentity,
		AuthorizationDecisionID:    command.Claim.AuthorizationDecisionID,
		AuthorizationPolicyVersion: command.Claim.AuthorizationPolicyVersion,
	}

	if err := store.AtomicReadWrite(t.Context(), "test: seed in-flight provider attempt", func(tx beads.AtomicReadWriteTx) error {
		repositoryState, err := readCommandRepositoryState(tx)
		if err != nil {
			return err
		}
		command.Order.Revision = repositoryState.Revision + 1
		wire, err := EncodeCommandV1(command)
		if err != nil {
			return err
		}
		if err := tx.Update(command.ID, beads.UpdateOpts{Metadata: map[string]string{commandRecordWireMetadataKey: string(wire)}}); err != nil {
			return err
		}
		return setCommandRepositoryHighWaters(tx, command.Order.Revision, repositoryState.SequenceHighWater)
	}); err != nil {
		t.Fatalf("seed in-flight command: %v", err)
	}
	if _, err := repository.RepairLineage(t.Context()); err != nil {
		t.Fatalf("RepairLineage after in-flight seed: %v", err)
	}
	return repository, store, command, trustedCityPartitionFromAuthority(command.TrustedIngress), newTestNudgeAuthority()
}

func providerAttemptCompletion(command Command, actionResult CommandActionResult) CommandCompletionRequest {
	request := CommandCompletionRequest{
		CommandID:    command.ID,
		ClaimID:      command.Claim.ID,
		OperationID:  command.Claim.OperationID,
		AttemptID:    command.Claim.AttemptID,
		CompletedAt:  command.Claim.ClaimedAt.Add(time.Second),
		ActionResult: actionResult,
	}
	switch actionResult {
	case CommandActionResultInjectedUnconfirmed:
		request.ProviderStage = ProviderStageAccepted
		request.Completion = CompletionStateCompleted
	case CommandActionResultDeliveryUnknown:
		request.ProviderStage = ProviderStageMayHaveEntered
		request.Completion = CompletionStateUnknown
		request.ErrorClass = CommandErrorClassProviderAmbiguous
		request.Detail = "provider completion could not be proven"
	}
	return request
}

func assertInjectedUnconfirmedCompletion(t *testing.T, command Command, request CommandCompletionRequest) {
	t.Helper()
	if command.State != CommandStateInjectedUnconfirmed || command.Claim != nil || command.Terminal == nil {
		t.Fatalf("completed command = %#v", command)
	}
	terminal := command.Terminal
	if terminal.ActionResult != request.ActionResult || terminal.ProviderStage != ProviderStageAccepted || terminal.Completion != CompletionStateCompleted || terminal.ClaimID != request.ClaimID || terminal.AttemptID != request.AttemptID {
		t.Fatalf("completed terminal = %#v, request=%#v", terminal, request)
	}
}
