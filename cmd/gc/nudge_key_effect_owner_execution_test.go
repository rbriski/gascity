package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestNudgeKeyEffectOwnerAuthorizedImmediateExecutesAfterDurableClaim(t *testing.T) {
	fixture := newNudgeEffectOwnerExecutionFixture(t)

	outcome := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{
		Causes:          nudgeCauseCommandCommit,
		FirstEnqueuedAt: fixture.now,
	})
	if err := outcome.validate(); err != nil {
		t.Fatalf("reconcile outcome is invalid: %v", err)
	}
	if outcome.disposition != nudgeReconcileOutcomeForget {
		t.Fatalf("reconcile disposition = %d, want forget", outcome.disposition)
	}

	if got := fixture.source.claimCallCount(); got != 1 {
		t.Fatalf("authorized claim calls = %d, want 1", got)
	}
	if got := fixture.handle.nudgeCallCount(); got != 1 {
		t.Fatalf("worker.Handle.Nudge calls = %d, want 1", got)
	}
	if got := fixture.source.completionCallCount(); got != 1 {
		t.Fatalf("durable completion calls = %d, want 1", got)
	}

	reads := fixture.targets.snapshotReads()
	if len(reads) != 2 {
		t.Fatalf("target reads = %#v, want pre-claim and final post-claim reads", reads)
	}
	if reads[0].commandState != nudgequeue.CommandStatePending {
		t.Fatalf("first target read saw command state %q, want pending", reads[0].commandState)
	}
	if reads[1].commandState != nudgequeue.CommandStateInFlight {
		t.Fatalf("final target read saw command state %q, want durable in-flight claim", reads[1].commandState)
	}

	call := fixture.handle.singleNudgeCall(t)
	if call.commandState != nudgequeue.CommandStateInFlight || call.claim == nil {
		t.Fatalf("worker entry observed command = %#v, want durable in-flight claim", call)
	}
	if call.request.Text != fixture.command.Message || call.request.Delivery != worker.NudgeDeliveryImmediate || call.request.Wake != worker.NudgeWakeLiveOnly {
		t.Fatalf("worker request = %#v, want exact immediate live-only delivery", call.request)
	}
	if call.request.Effect == nil {
		t.Fatal("worker request is missing classified effect contract")
	}
	if got, want := call.request.Effect.OperationID, fixture.command.ID; got != want {
		t.Fatalf("effect operation id = %q, want %q", got, want)
	}
	if got, want := call.request.Effect.ExpectedLaunchIdentity, fixture.command.Target.LaunchIdentity; got != want {
		t.Fatalf("effect launch identity = %q, want %q", got, want)
	}
	if got := call.request.Effect.InteractionPolicy; got != runtime.NudgeInteractionRequireUnattachedNormal {
		t.Fatalf("effect interaction policy = %q, want require-unattached-normal", got)
	}

	completed := fixture.source.currentCommand()
	if completed.State != nudgequeue.CommandStateDelivered || completed.Claim != nil || completed.Terminal == nil {
		t.Fatalf("durable command after reconcile = %#v, want delivered terminal", completed)
	}
	if completed.Terminal.ActionResult != nudgequeue.CommandActionResultDelivered ||
		completed.Terminal.ProviderStage != nudgequeue.ProviderStageAccepted ||
		completed.Terminal.Completion != nudgequeue.CompletionStateCompleted {
		t.Fatalf("durable terminal = %#v, want accepted completed delivery", completed.Terminal)
	}
	if completed.Retry == nil || completed.Terminal.ClaimID != completed.Retry.ClaimID ||
		completed.Terminal.AttemptID != completed.Retry.AttemptID {
		t.Fatalf("terminal correlation = %#v retry=%#v, want exact claimed attempt", completed.Terminal, completed.Retry)
	}
}

type nudgeEffectOwnerExecutionFixture struct {
	now     time.Time
	command nudgequeue.Command
	source  *mutexNudgeEffectSource
	targets *scriptedNudgeEffectTargetReader
	handle  *mutexNudgeEffectHandle
	owner   *nudgeKeyEffectOwner
	key     reconcilekey.Session
}

func newNudgeEffectOwnerExecutionFixture(t *testing.T) *nudgeEffectOwnerExecutionFixture {
	t.Helper()
	now := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	command := immediateNudgeEffectCommand(now)
	source := newMutexNudgeEffectSource(command)
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 8, nil)
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	target := nudgeEffectTarget{
		sessionID:        command.Target.SessionID,
		sessionName:      "city--worker",
		intentGeneration: command.Target.IntentGeneration,
		launchIdentity:   command.Target.LaunchIdentity,
	}
	targets := &scriptedNudgeEffectTargetReader{
		source:  source,
		targets: []nudgeEffectTarget{target, target},
	}
	handle := &mutexNudgeEffectHandle{
		source: source,
		result: worker.NudgeResult{
			Delivered: true,
			Effect: &runtime.NudgeEffectResult{
				Stage:                runtime.NudgeEffectStageAccepted,
				Completion:           runtime.NudgeEffectCompletionCompleted,
				ConsumptionConfirmed: true,
			},
		},
	}
	ids := &nudgeEffectTestIDs{}
	owner, err := newNudgeKeyEffectOwner(nudgeKeyEffectOwnerConfig{
		reader:            reader,
		source:            source,
		authorizer:        allowingNudgeEffectAuthorizer{},
		targets:           targets,
		handles:           &staticNudgeEffectHandleFactory{handle: handle},
		ownerID:           "effect-owner-1",
		now:               func() time.Time { return now },
		newID:             ids.newID,
		claimLease:        time.Minute,
		completionTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyEffectOwner: %v", err)
	}
	key, err := reader.key(command.Target.SessionID)
	if err != nil {
		t.Fatalf("reader.key: %v", err)
	}
	return &nudgeEffectOwnerExecutionFixture{
		now:     now,
		command: command,
		source:  source,
		targets: targets,
		handle:  handle,
		owner:   owner,
		key:     key,
	}
}

func immediateNudgeEffectCommand(now time.Time) nudgequeue.Command {
	store := nudgequeue.CommandStoreBinding{StoreUUID: "effect-store", RestoreEpoch: 1}
	command := nudgequeue.Command{
		Version: nudgequeue.CommandVersion1,
		ID:      "command-immediate-1",
		State:   nudgequeue.CommandStatePending,
		Mode:    nudgequeue.DeliveryModeImmediate,
		Target: nudgequeue.CommandTarget{
			SessionID:        "session-1",
			IntentGeneration: 7,
			LaunchIdentity:   "launch-1",
			Policy:           nudgequeue.TargetPolicyExactLaunch,
		},
		Store: store,
		Order: nudgequeue.CommandOrder{Sequence: 1, Revision: 1},
		TrustedIngress: nudgequeue.TrustedIngressReference{
			Issuer:           "local-ingress",
			ReferenceID:      "ingress-1",
			PrincipalID:      "principal-1",
			TenantScope:      "tenant-1",
			CityScope:        "city-1",
			CredentialClass:  "controller-ingress",
			PolicyVersion:    "policy-v1",
			PolicyDecisionID: "decision-1",
			Action:           nudgequeue.NudgeCommandAction,
			TargetSessionID:  "session-1",
			IssuedAt:         now.Add(-time.Minute),
			ExpiresAt:        now.Add(time.Hour),
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      "inspect the failed build",
		CreatedAt:    now,
		DeliverAfter: now,
		ExpiresAt:    now.Add(30 * time.Minute),
		Binding: &nudgequeue.CommandBinding{
			LaunchIdentity: "launch-1",
			BoundAt:        now,
		},
	}
	command.TrustedIngress.PayloadDigest = nudgequeue.ComputeCommandPayloadDigest(command)
	return command
}

type mutexNudgeEffectSource struct {
	mu              sync.Mutex
	command         nudgequeue.Command
	claimCalls      []nudgeEffectClaimRequest
	completionCalls []nudgequeue.CommandCompletionRequest
}

func newMutexNudgeEffectSource(command nudgequeue.Command) *mutexNudgeEffectSource {
	return &mutexNudgeEffectSource{command: cloneNudgeEffectTestCommand(command)}
}

func (s *mutexNudgeEffectSource) Snapshot(ctx context.Context, limit int) (nudgequeue.CommandIndexSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandIndexSnapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if limit < 1 {
		return nudgequeue.CommandIndexSnapshot{}, fmt.Errorf("snapshot limit %d is not positive", limit)
	}
	command := cloneNudgeEffectTestCommand(s.command)
	return nudgequeue.CommandIndexSnapshot{
		Store:             command.Store,
		Entries:           []nudgequeue.CommandIndexEntry{{Command: &command}},
		Revision:          command.Order.Revision,
		SequenceHighWater: command.Order.Sequence,
	}, nil
}

func (s *mutexNudgeEffectSource) Get(ctx context.Context, commandID string) (nudgequeue.CommandIndexResolution, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandIndexResolution{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	resolution := nudgequeue.CommandIndexResolution{
		Store:                  s.command.Store,
		Revision:               s.command.Order.Revision,
		CompletedAuditRevision: 1,
	}
	if commandID != s.command.ID {
		return resolution, nil
	}
	command := cloneNudgeEffectTestCommand(s.command)
	resolution.Entry = nudgequeue.CommandIndexEntry{Command: &command}
	resolution.Found = true
	return resolution, nil
}

func (s *mutexNudgeEffectSource) ClaimAuthorized(ctx context.Context, request nudgeEffectClaimRequest, _ nudgequeue.NudgeClaimAuthorizer) (nudgequeue.CommandClaimResult, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandClaimResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimCalls = append(s.claimCalls, request)
	if s.command.State != nudgequeue.CommandStatePending {
		return nudgequeue.CommandClaimResult{
			Disposition: nudgequeue.CommandClaimBusy,
			Command:     cloneNudgeEffectTestCommand(s.command),
		}, nil
	}
	if request.commandID != s.command.ID || request.boundLaunchIdentity != s.command.Target.LaunchIdentity {
		return nudgequeue.CommandClaimResult{}, fmt.Errorf("claim does not address the exact command launch")
	}
	claim := &nudgequeue.CommandClaim{
		ID:                         request.claimID,
		OwnerID:                    request.ownerID,
		OperationID:                request.commandID,
		AttemptID:                  request.attemptID,
		BoundLaunchIdentity:        request.boundLaunchIdentity,
		AuthorizationDecisionID:    "claim-decision-1",
		AuthorizationPolicyVersion: "policy-v2",
		ClaimedAt:                  request.claimedAt,
		LeaseUntil:                 request.leaseUntil,
	}
	s.command.State = nudgequeue.CommandStateInFlight
	s.command.Order.Revision++
	s.command.Claim = claim
	s.command.Retry = &nudgequeue.CommandRetry{
		AttemptCount:               1,
		LastAttemptAt:              request.claimedAt,
		ClaimID:                    claim.ID,
		OperationID:                claim.OperationID,
		AttemptID:                  claim.AttemptID,
		BoundLaunchIdentity:        claim.BoundLaunchIdentity,
		AuthorizationDecisionID:    claim.AuthorizationDecisionID,
		AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
	}
	return nudgequeue.CommandClaimResult{
		Disposition: nudgequeue.CommandClaimAllowed,
		Command:     cloneNudgeEffectTestCommand(s.command),
	}, nil
}

func (s *mutexNudgeEffectSource) CompleteProviderAttempt(ctx context.Context, request nudgequeue.CommandCompletionRequest) (nudgequeue.CommandCompletionResult, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandCompletionResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionCalls = append(s.completionCalls, request)
	if s.command.State != nudgequeue.CommandStateInFlight || s.command.Claim == nil ||
		s.command.Claim.ID != request.ClaimID || s.command.Claim.AttemptID != request.AttemptID {
		return nudgequeue.CommandCompletionResult{
			Disposition: nudgequeue.CommandCompletionStale,
			Command:     cloneNudgeEffectTestCommand(s.command),
		}, nil
	}
	s.command.Order.Revision++
	s.command.State = nudgeEffectTerminalState(request.ActionResult)
	s.command.Terminal = &nudgequeue.CommandTerminal{
		At:                         request.CompletedAt,
		ActionResult:               request.ActionResult,
		ErrorClass:                 request.ErrorClass,
		Detail:                     request.Detail,
		ClaimID:                    s.command.Claim.ID,
		OperationID:                s.command.Claim.OperationID,
		AttemptID:                  s.command.Claim.AttemptID,
		BoundLaunchIdentity:        s.command.Claim.BoundLaunchIdentity,
		AuthorizationDecisionID:    s.command.Claim.AuthorizationDecisionID,
		AuthorizationPolicyVersion: s.command.Claim.AuthorizationPolicyVersion,
		ProviderStage:              request.ProviderStage,
		Completion:                 request.Completion,
	}
	s.command.Claim = nil
	return nudgequeue.CommandCompletionResult{
		Disposition: nudgequeue.CommandCompletionRecorded,
		Command:     cloneNudgeEffectTestCommand(s.command),
	}, nil
}

func (s *mutexNudgeEffectSource) currentCommand() nudgequeue.Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneNudgeEffectTestCommand(s.command)
}

func (s *mutexNudgeEffectSource) claimCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.claimCalls)
}

func (s *mutexNudgeEffectSource) completionCallCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.completionCalls)
}

func nudgeEffectTerminalState(result nudgequeue.CommandActionResult) nudgequeue.CommandState {
	switch result {
	case nudgequeue.CommandActionResultDelivered, nudgequeue.CommandActionResultDuplicate:
		return nudgequeue.CommandStateDelivered
	case nudgequeue.CommandActionResultInjectedUnconfirmed:
		return nudgequeue.CommandStateInjectedUnconfirmed
	case nudgequeue.CommandActionResultDeliveryUnknown:
		return nudgequeue.CommandStateDeliveryUnknown
	case nudgequeue.CommandActionResultExpired:
		return nudgequeue.CommandStateExpired
	case nudgequeue.CommandActionResultSuperseded, nudgequeue.CommandActionResultTargetMissing:
		return nudgequeue.CommandStateSuperseded
	default:
		return nudgequeue.CommandStateDeadLettered
	}
}

func cloneNudgeEffectTestCommand(command nudgequeue.Command) nudgequeue.Command {
	clone := command
	if command.Binding != nil {
		binding := *command.Binding
		clone.Binding = &binding
	}
	if command.Retry != nil {
		retry := *command.Retry
		if retry.NextEligibleAt != nil {
			next := *retry.NextEligibleAt
			retry.NextEligibleAt = &next
		}
		clone.Retry = &retry
	}
	if command.Claim != nil {
		claim := *command.Claim
		clone.Claim = &claim
	}
	if command.Terminal != nil {
		terminal := *command.Terminal
		clone.Terminal = &terminal
	}
	if command.Reference != nil {
		reference := *command.Reference
		clone.Reference = &reference
	}
	return clone
}

type nudgeEffectTargetRead struct {
	target       nudgeEffectTarget
	commandState nudgequeue.CommandState
}

type scriptedNudgeEffectTargetReader struct {
	mu      sync.Mutex
	source  *mutexNudgeEffectSource
	targets []nudgeEffectTarget
	reads   []nudgeEffectTargetRead
}

func (r *scriptedNudgeEffectTargetReader) Read(ctx context.Context, sessionID string) (nudgeEffectTarget, error) {
	if err := ctx.Err(); err != nil {
		return nudgeEffectTarget{}, err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.targets) == 0 {
		return nudgeEffectTarget{}, fmt.Errorf("target script is exhausted")
	}
	index := len(r.reads)
	if index >= len(r.targets) {
		index = len(r.targets) - 1
	}
	target := r.targets[index]
	if target.sessionID != sessionID {
		return nudgeEffectTarget{}, fmt.Errorf("target read session %q does not match %q", sessionID, target.sessionID)
	}
	command := r.source.currentCommand()
	r.reads = append(r.reads, nudgeEffectTargetRead{target: target, commandState: command.State})
	return target, nil
}

func (r *scriptedNudgeEffectTargetReader) snapshotReads() []nudgeEffectTargetRead {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]nudgeEffectTargetRead(nil), r.reads...)
}

type nudgeEffectNudgeCall struct {
	request      worker.NudgeRequest
	commandState nudgequeue.CommandState
	claim        *nudgequeue.CommandClaim
}

type mutexNudgeEffectHandle struct {
	worker.Handle

	mu     sync.Mutex
	source *mutexNudgeEffectSource
	result worker.NudgeResult
	err    error
	calls  []nudgeEffectNudgeCall
}

func (h *mutexNudgeEffectHandle) Nudge(ctx context.Context, request worker.NudgeRequest) (worker.NudgeResult, error) {
	if err := ctx.Err(); err != nil {
		return worker.NudgeResult{}, err
	}
	command := h.source.currentCommand()
	var claim *nudgequeue.CommandClaim
	if command.Claim != nil {
		cloned := *command.Claim
		claim = &cloned
	}
	h.mu.Lock()
	h.calls = append(h.calls, nudgeEffectNudgeCall{request: request, commandState: command.State, claim: claim})
	result, err := h.result, h.err
	h.mu.Unlock()
	return result, err
}

func (h *mutexNudgeEffectHandle) nudgeCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *mutexNudgeEffectHandle) singleNudgeCall(t *testing.T) nudgeEffectNudgeCall {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.calls) != 1 {
		t.Fatalf("worker nudge calls = %d, want 1", len(h.calls))
	}
	return h.calls[0]
}

type staticNudgeEffectHandleFactory struct {
	handle worker.Handle
}

func (f *staticNudgeEffectHandleFactory) Handle(nudgeEffectTarget) (worker.Handle, error) {
	if f == nil || f.handle == nil {
		return nil, fmt.Errorf("worker handle is unavailable")
	}
	return f.handle, nil
}

type allowingNudgeEffectAuthorizer struct{}

func (allowingNudgeEffectAuthorizer) AuthorizeNudgeClaim(_ context.Context, request nudgequeue.NudgeClaimAuthorizationRequest) (nudgequeue.NudgeClaimAuthorization, error) {
	return nudgequeue.NudgeClaimAuthorization{
		Disposition:            nudgequeue.NudgeAuthorizationAllowed,
		PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		DecisionID:             "claim-decision-1",
		PolicyVersion:          "policy-v2",
		Reference:              request.Command.TrustedIngress,
	}, nil
}

type nudgeEffectTestIDs struct {
	sequence atomic.Uint64
}

func (g *nudgeEffectTestIDs) newID(kind string) (string, error) {
	return fmt.Sprintf("%s-%d", kind, g.sequence.Add(1)), nil
}

var (
	_ nudgeCommandEffectSource        = (*mutexNudgeEffectSource)(nil)
	_ nudgeEffectTargetReader         = (*scriptedNudgeEffectTargetReader)(nil)
	_ worker.Handle                   = (*mutexNudgeEffectHandle)(nil)
	_ nudgeEffectHandleFactory        = (*staticNudgeEffectHandleFactory)(nil)
	_ nudgequeue.NudgeClaimAuthorizer = allowingNudgeEffectAuthorizer{}
)
