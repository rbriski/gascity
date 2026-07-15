package main

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// This deliberately short deadline is the subject of these tests. The handle
// waits for it to expire before returning terminal provider evidence.
const nudgeEffectDeadlineSplitTestTimeout = 25 * time.Millisecond

func TestNudgeKeyEffectOwnerTerminalEvidenceGetsFreshPersistenceDeadline(t *testing.T) {
	tests := []struct {
		name      string
		result    worker.NudgeResult
		effectErr error
		wantState nudgequeue.CommandState
	}{
		{
			name: "accepted and consumed",
			result: worker.NudgeResult{Delivered: true, Effect: &runtime.NudgeEffectResult{
				Stage:                runtime.NudgeEffectStageAccepted,
				Completion:           runtime.NudgeEffectCompletionCompleted,
				ConsumptionConfirmed: true,
			}},
			wantState: nudgequeue.CommandStateDelivered,
		},
		{
			name: "accepted but unconfirmed",
			result: worker.NudgeResult{Delivered: true, Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageAccepted,
				Completion: runtime.NudgeEffectCompletionCompleted,
			}},
			wantState: nudgequeue.CommandStateInjectedUnconfirmed,
		},
		{
			name: "definitively rejected",
			result: worker.NudgeResult{Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageRejected,
				Completion: runtime.NudgeEffectCompletionNotCompleted,
			}},
			effectErr: errors.New("classified provider refusal"),
			wantState: nudgequeue.CommandStateDeadLettered,
		},
		{
			name: "entry ambiguous",
			result: worker.NudgeResult{Effect: &runtime.NudgeEffectResult{
				Stage:      runtime.NudgeEffectStageMayHaveEntered,
				Completion: runtime.NudgeEffectCompletionUnknown,
			}},
			effectErr: errors.New("classified provider ambiguity"),
			wantState: nudgequeue.CommandStateDeliveryUnknown,
		},
		{
			name:      "invalid evidence",
			result:    worker.NudgeResult{},
			wantState: nudgequeue.CommandStateDeliveryUnknown,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newNudgeEffectDeadlineSplitFixture(t)
			fixture.handle.setResult(test.result, test.effectErr)

			outcome := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
			assertNudgeEffectDeadlineSplitCompleted(t, fixture, test.wantState)
			assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

			duplicate := fixture.owner.reconcile(t.Context(), fixture.key, nudgeReconcileBatch{
				Causes:          nudgeCauseProviderResult,
				WorkqueueReplay: true,
			})
			assertNudgeEffectOutcomeDoesNotViolateInvariant(t, duplicate)
			if got := fixture.handle.callCount(); got != 1 {
				t.Fatalf("provider entries after duplicate callback = %d, want exactly 1", got)
			}
		})
	}
}

func TestNudgeKeyEffectOwnerParentCancellationAfterClaimCannotConsumeCompletionBudget(t *testing.T) {
	fixture := newNudgeEffectDeadlineSplitFixture(t)
	parent, cancelParent := context.WithCancel(context.Background())
	t.Cleanup(cancelParent)
	done := make(chan nudgeReconcileOutcome, 1)
	go func() {
		done <- fixture.owner.reconcile(parent, fixture.key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	}()

	awaitNudgeEffectSignal(t, fixture.handle.started)
	cancelParent()
	outcome := awaitNudgeEffectValue(t, done)

	provider := fixture.handle.singleContextObservation(t)
	if !errors.Is(provider.errAtReturn, context.DeadlineExceeded) {
		t.Fatalf("provider context error after parent cancellation = %v, want its own deadline exceeded", provider.errAtReturn)
	}
	assertNudgeEffectDeadlineSplitCompleted(t, fixture, nudgequeue.CommandStateDelivered)
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, outcome)

	duplicate := fixture.owner.reconcile(context.Background(), fixture.key, nudgeReconcileBatch{
		Causes:          nudgeCauseProviderResult,
		WorkqueueReplay: true,
	})
	assertNudgeEffectOutcomeDoesNotViolateInvariant(t, duplicate)
	if got := fixture.handle.callCount(); got != 1 {
		t.Fatalf("provider entries after canceled-parent replay = %d, want exactly 1", got)
	}
}

func assertNudgeEffectDeadlineSplitCompleted(t *testing.T, fixture *nudgeEffectDeadlineSplitFixture, wantState nudgequeue.CommandState) {
	t.Helper()
	provider := fixture.handle.singleContextObservation(t)
	if !provider.hasDeadline {
		t.Fatal("provider context has no deadline, want an independently bounded provider phase")
	}
	if !errors.Is(provider.errAtReturn, context.DeadlineExceeded) {
		t.Fatalf("provider context error at terminal evidence = %v, want deadline exceeded", provider.errAtReturn)
	}

	persistence := fixture.source.singleCompletionContextObservation(t)
	if persistence.errAtEntry != nil {
		t.Fatalf("completion context entered persistence already expired: %v (provider deadline %v, completion deadline %v)", persistence.errAtEntry, provider.deadline, persistence.deadline)
	}
	if !persistence.hasDeadline {
		t.Fatal("completion context has no deadline, want a fresh bounded persistence phase")
	}
	if !persistence.deadline.After(provider.deadline) {
		t.Fatalf("completion deadline = %v, want later than consumed provider deadline %v", persistence.deadline, provider.deadline)
	}
	if remaining := persistence.deadline.Sub(persistence.observedAt); remaining <= 0 {
		t.Fatalf("completion persistence budget at entry = %v, want positive budget", remaining)
	}
	if got := fixture.source.completionCallCount(); got != 1 {
		t.Fatalf("durable marker-last completion calls = %d, want 1", got)
	}
	command := fixture.source.currentCommand()
	if command.State != wantState || command.Claim != nil || command.Terminal == nil {
		t.Fatalf("durable command after terminal provider evidence = %#v, want marker-last terminal state %q", command, wantState)
	}
}

type nudgeEffectDeadlineSplitFixture struct {
	source *deadlineObservingNudgeEffectSource
	handle *deadlineConsumingNudgeEffectHandle
	owner  *nudgeKeyEffectOwner
	key    reconcilekey.Session
}

func newNudgeEffectDeadlineSplitFixture(t *testing.T) *nudgeEffectDeadlineSplitFixture {
	t.Helper()
	now := time.Date(2026, 7, 15, 15, 0, 0, 0, time.UTC)
	command := immediateNudgeEffectCommand(now)
	baseSource := newMutexNudgeEffectSource(command)
	source := &deadlineObservingNudgeEffectSource{mutexNudgeEffectSource: baseSource}
	reader, err := newNudgeKeyReadShadow(t.Context(), source, 8, nil)
	if err != nil {
		t.Fatalf("newNudgeKeyReadShadow: %v", err)
	}
	target := nudgeEffectTarget{
		sessionID:        command.Target.SessionID,
		sessionName:      "city--deadline-worker",
		intentGeneration: command.Target.IntentGeneration,
		launchIdentity:   command.Target.LaunchIdentity,
	}
	targets := &scriptedNudgeEffectTargetReader{
		source:  baseSource,
		targets: []nudgeEffectTarget{target, target},
	}
	handle := &deadlineConsumingNudgeEffectHandle{
		started: make(chan struct{}),
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
		ownerID:           "deadline-split-owner",
		now:               func() time.Time { return now },
		newID:             ids.newID,
		claimLease:        time.Minute,
		completionTimeout: nudgeEffectDeadlineSplitTestTimeout,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyEffectOwner: %v", err)
	}
	key, err := reader.key(command.Target.SessionID)
	if err != nil {
		t.Fatalf("reader.key: %v", err)
	}
	return &nudgeEffectDeadlineSplitFixture{source: source, handle: handle, owner: owner, key: key}
}

type nudgeEffectContextObservation struct {
	deadline    time.Time
	hasDeadline bool
	observedAt  time.Time
	errAtEntry  error
	errAtReturn error
}

type deadlineObservingNudgeEffectSource struct {
	*mutexNudgeEffectSource

	mu          sync.Mutex
	completions []nudgeEffectContextObservation
}

func (s *deadlineObservingNudgeEffectSource) CompleteProviderAttempt(ctx context.Context, request nudgequeue.CommandCompletionRequest) (nudgequeue.CommandCompletionResult, error) {
	deadline, hasDeadline := ctx.Deadline()
	observation := nudgeEffectContextObservation{
		deadline:    deadline,
		hasDeadline: hasDeadline,
		observedAt:  time.Now(),
		errAtEntry:  ctx.Err(),
	}
	s.mu.Lock()
	s.completions = append(s.completions, observation)
	s.mu.Unlock()
	return s.mutexNudgeEffectSource.CompleteProviderAttempt(ctx, request)
}

func (s *deadlineObservingNudgeEffectSource) singleCompletionContextObservation(t *testing.T) nudgeEffectContextObservation {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.completions) != 1 {
		t.Fatalf("completion persistence entries = %d, want 1", len(s.completions))
	}
	return s.completions[0]
}

type deadlineConsumingNudgeEffectHandle struct {
	worker.Handle

	mu           sync.Mutex
	started      chan struct{}
	startedOnce  sync.Once
	observations []nudgeEffectContextObservation
	result       worker.NudgeResult
	effectErr    error
}

func (h *deadlineConsumingNudgeEffectHandle) Nudge(ctx context.Context, _ worker.NudgeRequest) (worker.NudgeResult, error) {
	deadline, hasDeadline := ctx.Deadline()
	observation := nudgeEffectContextObservation{
		deadline:    deadline,
		hasDeadline: hasDeadline,
		observedAt:  time.Now(),
		errAtEntry:  ctx.Err(),
	}
	h.startedOnce.Do(func() { close(h.started) })
	if !hasDeadline {
		return worker.NudgeResult{}, fmt.Errorf("provider context is unbounded")
	}
	<-ctx.Done()
	observation.errAtReturn = ctx.Err()
	h.mu.Lock()
	h.observations = append(h.observations, observation)
	result, effectErr := h.result, h.effectErr
	h.mu.Unlock()
	return result, effectErr
}

func (h *deadlineConsumingNudgeEffectHandle) setResult(result worker.NudgeResult, effectErr error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.result = result
	h.effectErr = effectErr
}

func (h *deadlineConsumingNudgeEffectHandle) callCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.observations)
}

func (h *deadlineConsumingNudgeEffectHandle) singleContextObservation(t *testing.T) nudgeEffectContextObservation {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.observations) != 1 {
		t.Fatalf("provider entries = %d, want 1", len(h.observations))
	}
	return h.observations[0]
}

var (
	_ nudgeCommandEffectSource = (*deadlineObservingNudgeEffectSource)(nil)
	_ worker.Handle            = (*deadlineConsumingNudgeEffectHandle)(nil)
)
