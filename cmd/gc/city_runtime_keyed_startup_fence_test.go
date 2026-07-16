package main

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestKeyedProductionNudgeRecoveryFailurePublishesNothing(t *testing.T) {
	tests := []struct {
		name          string
		recoveryErr   error
		wantTransient bool
	}{
		{
			name:        "schema skew is permanent",
			recoveryErr: errors.Join(nudgequeue.ErrLocalNudgeAuthorityConflict, errors.New("authority schema manifest differs")),
		},
		{
			name:          "deadline is retryable",
			recoveryErr:   context.DeadlineExceeded,
			wantTransient: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProductionNudgeCommandFixture(t)
			authority := &startupFenceNudgeAuthority{ingress: fixture.ingress, recoveryErr: test.recoveryErr}
			cr := &CityRuntime{
				cityPath:                   fixture.cityPath,
				cityName:                   "startup-fence-city",
				cfg:                        supervisorCfg(),
				sp:                         runtime.NewFake(),
				rec:                        events.Discard,
				standaloneCityStore:        fixture.store,
				stderr:                     io.Discard,
				nudgeCityPartition:         fixture.partition,
				nudgeCityPartitionResolver: authority,
				nudgeEffectOwnership:       nudgeEffectOwnershipKeyed,
				nudgeClaimAuthorizer:       fixture.authority,
			}

			err := cr.installNudgeKeyShadow(t.Context())
			if !errors.Is(err, test.recoveryErr) {
				t.Fatalf("keyed installation error = %v, want recovery cause %v", err, test.recoveryErr)
			}
			controller, reader, scope, retry := cr.nudgeKeyShadowState()
			if controller != nil || reader != nil || scope != "" {
				t.Fatalf("failed recovery published controller=%v reader=%v scope=%q", controller != nil, reader != nil, scope)
			}
			if retry != test.wantTransient {
				t.Fatalf("failed recovery retry publication = %t, want %t", retry, test.wantTransient)
			}
			if authority.recoveryCalls != 1 || authority.readCalls != 0 {
				t.Fatalf("failed recovery calls recovery/read = %d/%d, want 1/0", authority.recoveryCalls, authority.readCalls)
			}
		})
	}
}

func TestStartNudgeKeyBeforeReadinessRefusesKeyedRecoveryFailure(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	schemaSkew := errors.Join(nudgequeue.ErrLocalNudgeAuthorityConflict, errors.New("authority schema manifest differs"))
	authority := &startupFenceNudgeAuthority{ingress: fixture.ingress, recoveryErr: schemaSkew}
	cr := &CityRuntime{
		cityPath:                   fixture.cityPath,
		cityName:                   "startup-fence-city",
		cfg:                        supervisorCfg(),
		sp:                         runtime.NewFake(),
		rec:                        events.Discard,
		standaloneCityStore:        fixture.store,
		stderr:                     io.Discard,
		nudgeCityPartition:         fixture.partition,
		nudgeCityPartitionResolver: authority,
		nudgeEffectOwnership:       nudgeEffectOwnershipKeyed,
		nudgeClaimAuthorizer:       fixture.authority,
	}

	installErr := cr.installNudgeKeyShadow(t.Context())
	stop, ready := cr.startNudgeKeyBeforeReadiness(t.Context(), installErr)
	defer stop()
	if ready {
		t.Fatal("keyed readiness admitted after schema-skewed authority recovery")
	}
	controller, reader, scope, retry := cr.nudgeKeyShadowState()
	if controller != nil || reader != nil || scope != "" || retry {
		t.Fatalf("schema skew published controller=%v reader=%v scope=%q retry=%t", controller != nil, reader != nil, scope, retry)
	}
	if authority.recoveryCalls != 1 || authority.readCalls != 0 {
		t.Fatalf("schema-skew recovery/read calls = %d/%d, want 1/0", authority.recoveryCalls, authority.readCalls)
	}
}

func TestStartNudgeKeyBeforeReadinessRetriesTransientWhileUnpublished(t *testing.T) {
	command := immediateNudgeEffectCommand(time.Now().UTC())
	source := newMutexNudgeEffectSource(command)
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipKeyed,
		source,
		newNudgeOwnershipReadCountingStore(nudgeOwnershipSessionStore(command)),
		runtime.NewFake(),
		allowingNudgeEffectAuthorizer{},
	)

	openAttempts := 0
	cr.nudgeCommandSourceOpener = func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
		openAttempts++
		if openAttempts == 1 {
			return nil, retryableNudgeCommandSourceFailure(context.DeadlineExceeded)
		}
		return source, nil
	}
	periodicTicks := make(chan time.Time)
	retryTicks := make(chan time.Time, 1)
	retryArmed := make(chan struct{}, 1)
	cr.nudgeKeyTickerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		return nudgeKeyPeriodicTicker{ticks: periodicTicks, stop: func() {}}
	}
	cr.nudgeKeyRetryTimerFactory = func(time.Duration) nudgeKeyPeriodicTicker {
		retryArmed <- struct{}{}
		return nudgeKeyPeriodicTicker{ticks: retryTicks, stop: func() {}}
	}
	type readinessResult struct {
		stop  func()
		ready bool
	}
	installErr := cr.installNudgeKeyShadow(t.Context())
	if !nudgeCommandSourceFailureIsTransient(installErr) {
		t.Fatalf("initial install error = %v, want transient", installErr)
	}
	result := make(chan readinessResult, 1)
	go func() {
		stop, ready := cr.startNudgeKeyBeforeReadiness(t.Context(), installErr)
		result <- readinessResult{stop: stop, ready: ready}
	}()

	receiveBeforeDeadline(t, retryArmed)
	select {
	case early := <-result:
		early.stop()
		t.Fatalf("transient recovery published readiness before retry: ready=%t", early.ready)
	default:
	}
	controller, reader, scope, retry := cr.nudgeKeyShadowState()
	if controller != nil || reader != nil || scope != "" || !retry {
		t.Fatalf("transient fence before retry = controller:%v reader:%v scope:%q retry:%t", controller != nil, reader != nil, scope, retry)
	}

	retryTicks <- time.Now()
	completed := receiveBeforeDeadline(t, result)
	defer completed.stop()
	if !completed.ready {
		t.Fatal("successful bounded retry did not release keyed readiness")
	}
	controller, reader, scope, retry = cr.nudgeKeyShadowState()
	if controller == nil || reader == nil || scope == "" || retry {
		t.Fatalf("transient fence after retry = controller:%v reader:%v scope:%q retry:%t", controller != nil, reader != nil, scope, retry)
	}
	if openAttempts != 2 {
		t.Fatalf("source open attempts = %d, want initial failure plus one successful retry", openAttempts)
	}
}
