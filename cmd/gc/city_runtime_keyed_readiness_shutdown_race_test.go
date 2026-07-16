package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestCityRuntimeKeyedReadinessCancellationWinsPreserveDecisionRace(t *testing.T) {
	baseCtx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	tracingCtx := &readinessPreserveRaceContext{
		Context:  baseCtx,
		observed: make(chan struct{}),
		release:  make(chan struct{}),
	}
	fixture := newKeyedReadinessRunFixture(t, func(context.Context) (nudgeCommandSource, error) {
		tracingCtx.armed.Store(true)
		return nil, errors.Join(errNudgeCommandSourceUnverified, errors.New("authority schema invariant failed"))
	})

	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.cr.run(tracingCtx)
	}()

	select {
	case <-tracingCtx.observed:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for readiness refusal to observe the uncanceled context")
	}
	cancel()
	close(tracingCtx.release)
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("timed out waiting for city runtime shutdown")
	}

	if fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("cancellation racing the readiness refusal selected preserve-sessions shutdown")
	}
	if fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatal("preexisting session remained live after cancellation won the readiness refusal race")
	}
	if got := fixture.provider.CountCalls("Stop", fixture.sessionName); got == 0 {
		t.Fatal("preexisting session received no stop call after cancellation won the readiness refusal race")
	}
	got, err := fixture.store.Get(fixture.sessionBeadID)
	if err != nil {
		t.Fatalf("get canceled session bead: %v", err)
	}
	if reason := got.Metadata["sleep_reason"]; reason != string(session.SleepReasonCityStop) {
		t.Fatalf("canceled session sleep_reason = %q, want %q", reason, session.SleepReasonCityStop)
	}
}

func TestSessionShutdownDecisionExplicitPreserveBeforeCancellationWins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	decision := new(sessionShutdownDecision)
	decision.preserveUnlessCanceled(ctx)

	decision.preserve()
	cancel()

	if !decision.resolve() {
		t.Fatal("explicit supervisor preserve selected before cancellation was not retained")
	}
}

func TestSessionShutdownDecisionCancellationBeforeExplicitPreserveWins(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	decision := new(sessionShutdownDecision)
	decision.preserveUnlessCanceled(ctx)

	cancel()
	decision.preserve()

	if decision.resolve() {
		t.Fatal("explicit preserve selected after cancellation overrode destructive shutdown")
	}
}

// readinessPreserveRaceContext exposes the exact check-then-store window in
// the readiness-refusal path. Err captures the uncanceled result, then lets the
// test cancel before returning that result to the caller.
type readinessPreserveRaceContext struct {
	context.Context
	armed    atomic.Bool
	observed chan struct{}
	release  chan struct{}
}

func (c *readinessPreserveRaceContext) Err() error {
	err := c.Context.Err()
	if err == nil && c.armed.CompareAndSwap(true, false) {
		close(c.observed)
		<-c.release
	}
	return err
}
