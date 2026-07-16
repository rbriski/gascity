package main

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestCityRuntimeKeyedChildDeathBeforeReadinessPublicationRefusesReadiness(t *testing.T) {
	fixture, _, _, _ := newKeyedChildLivenessRunFixture(t)
	started := make(chan struct{})
	statusResult := make(chan error, 1)
	fixture.cr.onStarted = func() { close(started) }
	fixture.cr.onStatus = func(status string) {
		if status != "starting_agents" {
			return
		}
		controller, _, _, _ := fixture.cr.nudgeKeyShadowState()
		if controller == nil {
			statusResult <- errors.New("keyed child is nil after effect-ready check")
			return
		}
		admissionClosed := make(chan struct{})
		controller.mu.Lock()
		controller.onAdmissionClosed = func() { close(admissionClosed) }
		controller.mu.Unlock()
		controller.reportFailure(errors.New("keyed child invariant failed before readiness publication"))
		select {
		case <-admissionClosed:
			statusResult <- nil
		case <-time.After(testutil.GoroutineRaceTimeout):
			statusResult <- errors.New("timed out waiting for keyed child admission to close")
		}
	}

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.cr.run(ctx)
	}()
	select {
	case err := <-statusResult:
		if err != nil {
			cancel()
			<-done
			t.Fatal(err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("timed out waiting for pre-publication keyed child failure")
	}

	select {
	case <-started:
		cancel()
		<-done
		t.Fatal("CityRuntime published readiness after its keyed effect owner died")
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("CityRuntime neither refused readiness nor terminated after keyed child death")
	}
	if !fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("keyed child death before readiness did not preserve sessions for re-adoption")
	}
	if !fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatal("keyed child death before readiness stopped the preexisting session")
	}
}

func TestCityRuntimeKeyedChildDeathAfterReadinessTerminatesAndPreserves(t *testing.T) {
	fixture, _, _, tickerStopped := newKeyedChildLivenessRunFixture(t)
	started := make(chan struct{})
	fixture.cr.onStarted = func() { close(started) }

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.cr.run(ctx)
	}()

	select {
	case <-started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("timed out waiting for CityRuntime readiness")
	}
	controller, _, _, _ := fixture.cr.nudgeKeyShadowState()
	if controller == nil {
		cancel()
		<-done
		t.Fatal("ready CityRuntime has no keyed effect owner")
	}
	controller.reportFailure(errors.New("keyed child invariant failed after readiness"))
	select {
	case <-tickerStopped:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("timed out waiting for keyed child lifecycle exit")
	}
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("ready CityRuntime stayed alive after losing its keyed effect owner")
	}
	if !fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("keyed child death after readiness did not preserve sessions for re-adoption")
	}
	if !fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatal("keyed child death after readiness stopped the preexisting session")
	}
}

func TestCityRuntimeKeyedChildPanicAfterReadinessTerminatesAndPreserves(t *testing.T) {
	fixture, source, ticks, _ := newKeyedChildLivenessRunFixture(t)
	started := make(chan struct{})
	fixture.cr.onStarted = func() { close(started) }

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	done := make(chan struct{})
	go func() {
		defer close(done)
		fixture.cr.run(ctx)
	}()

	select {
	case <-started:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("timed out waiting for CityRuntime readiness")
	}
	source.panicSnapshot.Store(true)
	ticks <- time.Now()
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		cancel()
		<-done
		t.Fatal("ready CityRuntime stayed alive after its keyed child panicked")
	}
	if !fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("keyed child panic after readiness did not preserve sessions for re-adoption")
	}
	if !fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatal("keyed child panic after readiness stopped the preexisting session")
	}
}

func newKeyedChildLivenessRunFixture(t *testing.T) (keyedReadinessRunFixture, *keyedChildLivenessSource, chan<- time.Time, <-chan struct{}) {
	t.Helper()
	source := &keyedChildLivenessSource{
		store: nudgequeue.CommandStoreBinding{StoreUUID: "keyed-child-liveness", RestoreEpoch: 1},
	}
	fixture := newKeyedReadinessRunFixture(t, func(context.Context) (nudgeCommandSource, error) {
		return source, nil
	})
	fixture.cr.stderr = io.Discard
	ticks := make(chan time.Time)
	tickerStopped := make(chan struct{})
	var stopOnce sync.Once
	fixture.cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{
			ticks: ticks,
			stop:  func() { stopOnce.Do(func() { close(tickerStopped) }) },
		}
	}
	return fixture, source, ticks, tickerStopped
}

type keyedChildLivenessSource struct {
	store         nudgequeue.CommandStoreBinding
	panicSnapshot atomic.Bool
}

func (s *keyedChildLivenessSource) Snapshot(ctx context.Context, _ int) (nudgequeue.CommandIndexSnapshot, error) {
	if err := ctx.Err(); err != nil {
		return nudgequeue.CommandIndexSnapshot{}, err
	}
	if s.panicSnapshot.Load() {
		panic("keyed child liveness snapshot panic")
	}
	return nudgequeue.CommandIndexSnapshot{Store: s.store}, nil
}

func (s *keyedChildLivenessSource) Get(context.Context, string) (nudgequeue.CommandIndexResolution, error) {
	return nudgequeue.CommandIndexResolution{}, errors.New("unexpected keyed child liveness Get")
}

func (s *keyedChildLivenessSource) ClaimAuthorized(context.Context, nudgeEffectClaimRequest, nudgequeue.NudgeClaimAuthorizer) (nudgequeue.CommandClaimResult, error) {
	return nudgequeue.CommandClaimResult{}, errors.New("unexpected keyed child liveness claim")
}

func (s *keyedChildLivenessSource) CompleteProviderAttempt(context.Context, nudgequeue.CommandCompletionRequest) (nudgequeue.CommandCompletionResult, error) {
	return nudgequeue.CommandCompletionResult{}, errors.New("unexpected keyed child liveness completion")
}
