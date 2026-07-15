package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/testutil"
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
	if got := fixture.handle.nativeEntryCount(); got != 1 {
		t.Fatalf("native provider entries = %d, want 1", got)
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

func TestNudgeKeyEffectOwnerClaimRefusalsNeverReachWorker(t *testing.T) {
	tests := []struct {
		name        string
		disposition nudgequeue.CommandClaimDisposition
		claimErr    error
		wantState   nudgequeue.CommandState
	}{
		{
			name:        "authorization denied",
			disposition: nudgequeue.CommandClaimDenied,
			wantState:   nudgequeue.CommandStateDeadLettered,
		},
		{
			name:        "authorization unknown disposition",
			disposition: nudgequeue.CommandClaimAuthorizationUnknown,
			wantState:   nudgequeue.CommandStatePending,
		},
		{
			name:        "authorization authority unavailable",
			disposition: nudgequeue.CommandClaimAuthorizationUnknown,
			claimErr:    fmt.Errorf("%w: policy authority unavailable", nudgequeue.ErrNudgeAuthorizationUnknown),
			wantState:   nudgequeue.CommandStatePending,
		},
		{
			name:        "concurrent owner busy",
			disposition: nudgequeue.CommandClaimBusy,
			wantState:   nudgequeue.CommandStatePending,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNudgeEffectOwnerExecutionFixture(t)
			fixture.source.setClaimResult(test.disposition, test.claimErr)

			outcome := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
			assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

			if got := fixture.source.claimCallCount(); got != 1 {
				t.Fatalf("claim calls = %d, want 1", got)
			}
			if got := fixture.handle.nudgeCallCount(); got != 0 {
				t.Fatalf("worker.Handle.Nudge calls = %d, want 0", got)
			}
			if got := fixture.handle.nativeEntryCount(); got != 0 {
				t.Fatalf("native provider entries = %d, want 0", got)
			}
			if got := fixture.source.completionCallCount(); got != 0 {
				t.Fatalf("provider completion calls = %d, want 0", got)
			}
			if got := fixture.source.currentCommand().State; got != test.wantState {
				t.Fatalf("durable state = %q, want %q", got, test.wantState)
			}
		})
	}
}

func TestNudgeKeyEffectOwnerStaleGenerationBeforeClaimNeverEntersProvider(t *testing.T) {
	fixture := newNudgeEffectOwnerExecutionFixture(t)
	stale := fixture.targets.firstTarget()
	stale.intentGeneration++
	fixture.targets.setTargets(stale)

	outcome := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseTargetGeneration})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

	if got := fixture.source.claimCallCount(); got != 0 {
		t.Fatalf("claim calls = %d, want 0 for stale pre-claim generation", got)
	}
	if got := fixture.handle.nudgeCallCount(); got != 0 {
		t.Fatalf("worker.Handle.Nudge calls = %d, want 0", got)
	}
	if got := fixture.handle.nativeEntryCount(); got != 0 {
		t.Fatalf("native provider entries = %d, want 0", got)
	}
	if got := fixture.source.currentCommand().State; got != nudgequeue.CommandStatePending {
		t.Fatalf("durable state = %q, want safely parked pending", got)
	}
}

func TestNudgeKeyEffectOwnerFinalRereadTerminalizesChangedTargetWithoutProviderEntry(t *testing.T) {
	fixture := newNudgeEffectOwnerExecutionFixture(t)
	current := fixture.targets.firstTarget()
	changed := current
	changed.launchIdentity = "launch-replaced-after-claim"
	fixture.targets.setTargets(current, changed)

	outcome := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseTargetGeneration})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

	reads := fixture.targets.snapshotReads()
	if len(reads) != 2 || reads[1].commandState != nudgequeue.CommandStateInFlight {
		t.Fatalf("target reads = %#v, want final reread after durable claim", reads)
	}
	if got := fixture.handle.nudgeCallCount(); got != 0 {
		t.Fatalf("worker.Handle.Nudge calls = %d, want 0", got)
	}
	if got := fixture.handle.nativeEntryCount(); got != 0 {
		t.Fatalf("native provider entries = %d, want 0", got)
	}
	if got := fixture.source.completionCallCount(); got != 1 {
		t.Fatalf("superseded completion calls = %d, want 1", got)
	}
	completed := fixture.source.currentCommand()
	if completed.State != nudgequeue.CommandStateSuperseded || completed.Terminal == nil {
		t.Fatalf("durable command = %#v, want superseded terminal", completed)
	}
	if completed.Terminal.ActionResult != nudgequeue.CommandActionResultSuperseded ||
		completed.Terminal.ProviderStage != nudgequeue.ProviderStageNotEntered ||
		completed.Terminal.Completion != nudgequeue.CompletionStateNotCompleted {
		t.Fatalf("superseded terminal = %#v, want definite not-entered evidence", completed.Terminal)
	}
}

func TestNudgeKeyEffectOwnerRuntimeInteractionRefusalsNeverEnterNativeProvider(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*runtime.Fake, string)
	}{
		{
			name: "human attached",
			prepare: func(provider *runtime.Fake, sessionName string) {
				provider.SetAttached(sessionName, true)
			},
		},
		{
			name: "copy mode",
			prepare: func(provider *runtime.Fake, sessionName string) {
				provider.SetCopyMode(sessionName, true)
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNudgeEffectOwnerExecutionFixture(t)
			target := fixture.targets.firstTarget()
			provider := runtime.NewFake()
			if err := provider.Start(t.Context(), target.sessionName, runtime.Config{}); err != nil {
				t.Fatalf("start fake runtime target: %v", err)
			}
			if err := provider.SetMeta(target.sessionName, "GC_INSTANCE_TOKEN", target.launchIdentity); err != nil {
				t.Fatalf("set fake runtime launch identity: %v", err)
			}
			test.prepare(provider, target.sessionName)
			handle, err := worker.NewRuntimeHandle(worker.RuntimeHandleConfig{
				Provider:    provider,
				SessionName: target.sessionName,
			})
			if err != nil {
				t.Fatalf("worker.NewRuntimeHandle: %v", err)
			}
			owner, key := newNudgeEffectOwnerForSource(
				t,
				fixture.source,
				fixture.targets,
				&staticNudgeEffectHandleFactory{handle: handle},
				fixture.now,
				"runtime-refusal-owner",
			)

			outcome := owner.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseRuntimeReadiness})
			assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

			if got := provider.CountCalls("NudgeEffect", target.sessionName); got != 0 {
				t.Fatalf("native NudgeEffect entries = %d, want 0", got)
			}
			if got := provider.CountCalls("Nudge", target.sessionName); got != 0 {
				t.Fatalf("legacy Nudge entries = %d, want 0", got)
			}
			if got := fixture.source.completionCallCount(); got != 0 {
				t.Fatalf("terminal completion calls = %d, want 0 for parked pre-entry refusal", got)
			}
			command := fixture.source.currentCommand()
			if command.State != nudgequeue.CommandStateInFlight || command.Claim == nil || command.Terminal != nil {
				t.Fatalf("durable command after refusal = %#v, want claimed parked command", command)
			}

			owner.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseRuntimeReadiness})
			if got := provider.CountCalls("NudgeEffect", target.sessionName); got != 0 {
				t.Fatalf("duplicate callback native entries = %d, want no blind replay", got)
			}
		})
	}
}

func TestNudgeKeyEffectOwnerDuplicateCallbackAfterCompletionDoesNotReplay(t *testing.T) {
	fixture := newNudgeEffectOwnerExecutionFixture(t)
	first := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, first)
	second := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseProviderResult, WorkqueueReplay: true})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, second)

	if got := fixture.handle.nudgeCallCount(); got != 1 {
		t.Fatalf("worker.Handle.Nudge calls after duplicate callback = %d, want 1", got)
	}
	if got := fixture.handle.nativeEntryCount(); got != 1 {
		t.Fatalf("native provider entries after duplicate callback = %d, want 1", got)
	}
	if got := fixture.source.completionCallCount(); got != 1 {
		t.Fatalf("completion calls after duplicate callback = %d, want 1", got)
	}
}

func TestNudgeKeyEffectOwnerConcurrentClaimAllowsOneProviderEntry(t *testing.T) {
	first := newNudgeEffectOwnerExecutionFixture(t)
	started := make(chan struct{})
	release := make(chan struct{})
	first.handle.blockUntil(started, release)

	secondTargets := &scriptedNudgeEffectTargetReader{
		source: first.source,
		targets: []nudgeEffectTarget{
			first.targets.firstTarget(),
			first.targets.firstTarget(),
		},
	}
	secondOwner, secondKey := newNudgeEffectOwnerForSource(
		t,
		first.source,
		secondTargets,
		&staticNudgeEffectHandleFactory{handle: first.handle},
		first.now,
		"effect-owner-2",
	)

	firstDone := make(chan nudgeReconcileOutcome, 1)
	go func() {
		firstDone <- first.owner.reconcile(t.Context(), first.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	}()
	awaitNudgeEffectSignal(t, started)

	secondOutcome := secondOwner.reconcile(t.Context(), secondKey, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, secondOutcome)
	if got := first.source.claimCallCount(); got != 2 {
		t.Fatalf("competing claim calls = %d, want 2", got)
	}
	if got := first.handle.nudgeCallCount(); got != 1 {
		t.Fatalf("worker calls while first attempt blocked = %d, want 1", got)
	}
	close(release)
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, awaitNudgeEffectValue(t, firstDone))

	if got := first.handle.nativeEntryCount(); got != 1 {
		t.Fatalf("native provider entries = %d, want exactly 1", got)
	}
	if got := first.source.completionCallCount(); got != 1 {
		t.Fatalf("durable completion calls = %d, want exactly 1", got)
	}
}

func TestNudgeKeyEffectOwnerRestartAfterCompletionFailureNeverReplaysProvider(t *testing.T) {
	fixture := newNudgeEffectOwnerExecutionFixture(t)
	fixture.source.setCompletionError(errors.New("durable completion unavailable"))

	first := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if err := first.validate(); err != nil {
		t.Fatalf("first reconcile outcome is invalid: %v", err)
	}
	if got := fixture.handle.nativeEntryCount(); got != 1 {
		t.Fatalf("first native provider entries = %d, want 1", got)
	}
	parked := fixture.source.currentCommand()
	if parked.State != nudgequeue.CommandStateInFlight || parked.Claim == nil || parked.Terminal != nil {
		t.Fatalf("command after completion failure = %#v, want in-flight ambiguity fence", parked)
	}

	fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseProviderResult, WorkqueueReplay: true})
	fixture.source.setCompletionError(nil)
	restartTargets := &scriptedNudgeEffectTargetReader{
		source:  fixture.source,
		targets: []nudgeEffectTarget{fixture.targets.firstTarget()},
	}
	restarted, restartKey := newNudgeEffectOwnerForSource(
		t,
		fixture.source,
		restartTargets,
		&staticNudgeEffectHandleFactory{handle: fixture.handle},
		fixture.now.Add(time.Minute),
		"effect-owner-after-restart",
	)
	restartOutcome := restarted.reconcile(t.Context(), restartKey, nudgeReconcileBatch{Causes: nudgeCauseAudit, WorkqueueReplay: true})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, restartOutcome)

	if got := fixture.handle.nudgeCallCount(); got != 1 {
		t.Fatalf("worker.Handle.Nudge calls across duplicate and restart = %d, want 1", got)
	}
	if got := fixture.handle.nativeEntryCount(); got != 1 {
		t.Fatalf("native provider entries across duplicate and restart = %d, want 1", got)
	}
	if got := fixture.source.completionCallCount(); got != 1 {
		t.Fatalf("completion calls across duplicate and restart = %d, want failed original only", got)
	}
}

func assertNudgeEffectOutcomeDoesNotViolateInvariant(t *testing.T, outcome nudgeReconcileOutcome) {
	t.Helper()
	if err := outcome.validate(); err != nil {
		t.Fatalf("reconcile outcome is invalid: %v", err)
	}
	if outcome.disposition == nudgeReconcileOutcomeInvariant {
		t.Fatalf("reconcile reported invariant failure: %v", outcome.err)
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
	owner, key := newNudgeEffectOwnerForSource(
		t,
		source,
		targets,
		&staticNudgeEffectHandleFactory{handle: handle},
		now,
		"effect-owner-1",
	)
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

func newNudgeEffectOwnerForSource(
	t *testing.T,
	source *mutexNudgeEffectSource,
	targets nudgeEffectTargetReader,
	handles nudgeEffectHandleFactory,
	now time.Time,
	ownerID string,
) (*nudgeKeyEffectOwner, reconcilekey.Session) {
	t.Helper()
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 8, nil)
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	ids := &nudgeEffectTestIDs{}
	owner, err := newNudgeKeyEffectOwner(nudgeKeyEffectOwnerConfig{
		reader:            reader,
		source:            source,
		authorizer:        allowingNudgeEffectAuthorizer{},
		targets:           targets,
		handles:           handles,
		ownerID:           ownerID,
		now:               func() time.Time { return now },
		newID:             ids.newID,
		claimLease:        time.Minute,
		completionTimeout: time.Minute,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyEffectOwner: %v", err)
	}
	key, err := reader.key(source.currentCommand().Target.SessionID)
	if err != nil {
		t.Fatalf("reader.key: %v", err)
	}
	return owner, key
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
	mu               sync.Mutex
	command          nudgequeue.Command
	claimDisposition nudgequeue.CommandClaimDisposition
	claimErr         error
	completionErr    error
	claimCalls       []nudgeEffectClaimRequest
	completionCalls  []nudgequeue.CommandCompletionRequest
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
	switch s.claimDisposition {
	case nudgequeue.CommandClaimDenied:
		s.terminalizeAuthorizationDenied(request.claimedAt)
		return nudgequeue.CommandClaimResult{
			Disposition: nudgequeue.CommandClaimDenied,
			Command:     cloneNudgeEffectTestCommand(s.command),
		}, s.claimErr
	case nudgequeue.CommandClaimAuthorizationUnknown:
		return nudgequeue.CommandClaimResult{
			Disposition: nudgequeue.CommandClaimAuthorizationUnknown,
			Command:     cloneNudgeEffectTestCommand(s.command),
		}, s.claimErr
	case nudgequeue.CommandClaimBusy:
		return nudgequeue.CommandClaimResult{
			Disposition: nudgequeue.CommandClaimBusy,
			Command:     cloneNudgeEffectTestCommand(s.command),
		}, s.claimErr
	case "", nudgequeue.CommandClaimAllowed:
		// Continue through the normal atomic-claim behavior below.
	default:
		return nudgequeue.CommandClaimResult{}, fmt.Errorf("unsupported fake claim disposition %q", s.claimDisposition)
	}
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
	}, s.claimErr
}

func (s *mutexNudgeEffectSource) CompleteProviderAttempt(ctx context.Context, request nudgequeue.CommandCompletionRequest) (nudgequeue.CommandCompletionResult, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandCompletionResult{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionCalls = append(s.completionCalls, request)
	if s.completionErr != nil {
		return nudgequeue.CommandCompletionResult{}, s.completionErr
	}
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

func (s *mutexNudgeEffectSource) terminalizeAuthorizationDenied(at time.Time) {
	if s.command.State != nudgequeue.CommandStatePending {
		return
	}
	s.command.Order.Revision++
	s.command.State = nudgequeue.CommandStateDeadLettered
	s.command.Claim = nil
	s.command.Retry = nil
	s.command.Terminal = &nudgequeue.CommandTerminal{
		At:                         at,
		ActionResult:               nudgequeue.CommandActionResultAuthorizationDenied,
		ErrorClass:                 nudgequeue.CommandErrorClassAuthorizationDenied,
		Detail:                     "current authorization policy denied the command",
		AuthorizationDecisionID:    "claim-decision-1",
		AuthorizationPolicyVersion: "policy-v2",
		ProviderStage:              nudgequeue.ProviderStageNotEntered,
		Completion:                 nudgequeue.CompletionStateNotCompleted,
	}
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

func (s *mutexNudgeEffectSource) setClaimResult(disposition nudgequeue.CommandClaimDisposition, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.claimDisposition = disposition
	s.claimErr = err
}

func (s *mutexNudgeEffectSource) setCompletionError(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.completionErr = err
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

func (r *scriptedNudgeEffectTargetReader) firstTarget() nudgeEffectTarget {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.targets) == 0 {
		return nudgeEffectTarget{}
	}
	return r.targets[0]
}

func (r *scriptedNudgeEffectTargetReader) setTargets(targets ...nudgeEffectTarget) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.targets = append([]nudgeEffectTarget(nil), targets...)
	r.reads = nil
}

type nudgeEffectNudgeCall struct {
	request      worker.NudgeRequest
	commandState nudgequeue.CommandState
	claim        *nudgequeue.CommandClaim
}

type mutexNudgeEffectHandle struct {
	worker.Handle

	mu            sync.Mutex
	source        *mutexNudgeEffectSource
	result        worker.NudgeResult
	calls         []nudgeEffectNudgeCall
	nativeEntries int
	started       chan struct{}
	release       <-chan struct{}
	startedOnce   sync.Once
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
	if h.result.Effect != nil && h.result.Effect.Stage != runtime.NudgeEffectStageNotEntered {
		h.nativeEntries++
	}
	result := h.result
	started, release := h.started, h.release
	h.mu.Unlock()
	if started != nil {
		h.startedOnce.Do(func() { close(started) })
	}
	if release != nil {
		select {
		case <-release:
		case <-ctx.Done():
			return worker.NudgeResult{}, ctx.Err()
		}
	}
	return result, nil
}

func (h *mutexNudgeEffectHandle) nudgeCallCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.calls)
}

func (h *mutexNudgeEffectHandle) nativeEntryCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.nativeEntries
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

func (h *mutexNudgeEffectHandle) blockUntil(started chan struct{}, release <-chan struct{}) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.started = started
	h.release = release
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

func awaitNudgeEffectSignal(t *testing.T, signal <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cancel()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal("timed out waiting for nudge effect signal")
	}
}

func awaitNudgeEffectValue[T any](t *testing.T, values <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cancel()
	select {
	case value := <-values:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for nudge effect result")
		var zero T
		return zero
	}
}

var (
	_ nudgeCommandEffectSource        = (*mutexNudgeEffectSource)(nil)
	_ nudgeEffectTargetReader         = (*scriptedNudgeEffectTargetReader)(nil)
	_ worker.Handle                   = (*mutexNudgeEffectHandle)(nil)
	_ nudgeEffectHandleFactory        = (*staticNudgeEffectHandleFactory)(nil)
	_ nudgequeue.NudgeClaimAuthorizer = allowingNudgeEffectAuthorizer{}
)
