package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	testNudgeKeyBacklogDepthMetric = "gc.reconcile.nudge_shadow.backlog.keys"
	testNudgeKeyBacklogAgeMetric   = "gc.reconcile.nudge_shadow.backlog.oldest_age_ms"
)

func TestNewNudgeKeyBacklogRecordPreservesOnlyBoundedSchedulerFacts(t *testing.T) {
	tests := []struct {
		name      string
		snapshot  nudgeKeyBacklogSnapshot
		wantState telemetry.NudgeKeyBacklogAgeState
	}{
		{name: "empty", snapshot: nudgeKeyBacklogSnapshot{AgeState: nudgeKeyBacklogAgeEmpty}, wantState: telemetry.NudgeKeyBacklogAgeEmpty},
		{name: "observed", snapshot: nudgeKeyBacklogSnapshot{Depth: 3, OldestAge: 125 * time.Millisecond, AgeState: nudgeKeyBacklogAgeObserved}, wantState: telemetry.NudgeKeyBacklogAgeObserved},
		{name: "unavailable", snapshot: nudgeKeyBacklogSnapshot{Depth: 2, AgeState: nudgeKeyBacklogAgeUnavailable}, wantState: telemetry.NudgeKeyBacklogAgeUnavailable},
		{name: "clock regressed", snapshot: nudgeKeyBacklogSnapshot{Depth: 1, AgeState: nudgeKeyBacklogAgeClockRegressed}, wantState: telemetry.NudgeKeyBacklogAgeClockRegressed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newNudgeKeyBacklogRecord(tc.snapshot)
			if got.Depth != tc.snapshot.Depth || got.OldestAge != tc.snapshot.OldestAge || got.AgeState != tc.wantState {
				t.Fatalf("record = %+v, want depth=%d age=%v state=%v", got, tc.snapshot.Depth, tc.snapshot.OldestAge, tc.wantState)
			}
		})
	}
}

func TestNudgeKeyControllerLifecyclePublishesAndUnregistersBacklogObserver(t *testing.T) {
	reader := installNudgeKeyObservationMetricReader(t)
	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	var elapsedNanos atomic.Int64
	callbackStarted := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(ctx context.Context, _ reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		close(callbackStarted)
		<-ctx.Done()
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return base.Add(time.Duration(elapsedNanos.Load())) }
	var stderr bytes.Buffer
	cr := &CityRuntime{nudgeKeyController: controller, stderr: &stderr}
	stop := cr.startNudgeKeyController(t.Context())
	t.Cleanup(stop)

	if err := controller.Enqueue(mustNudgeReconcileKey(t, "scope", "blocking"), nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue blocking key: %v", err)
	}
	receiveBeforeDeadline(t, callbackStarted)
	elapsedNanos.Store(int64(20 * time.Millisecond))
	if err := controller.Enqueue(mustNudgeReconcileKey(t, "scope", "starved"), nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue starved key: %v", err)
	}
	elapsedNanos.Store(int64(220 * time.Millisecond))

	metrics := collectNudgeKeyObservationMetrics(t, reader)
	assertNudgeKeyBacklogGauge(t, metrics, 1, 200, "observed")
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no backlog observation warning", stderr.String())
	}

	stop()
	metrics = collectNudgeKeyObservationMetrics(t, reader)
	assertNudgeKeyBacklogGauge(t, metrics, 0, 0, "empty")
}

func TestNudgeKeyBacklogRegistrationFailureWarnsOnceWithoutStoppingController(t *testing.T) {
	callback := make(chan struct{}, 1)
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		callback <- struct{}{}
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	var stderr bytes.Buffer
	warnings := newNudgeKeyBacklogWarnings(&stderr)
	stopObservation := startNudgeKeyBacklogObservationWithRegistrar(controller, warnings, func(telemetry.NudgeKeyBacklogObserver) (telemetry.NudgeKeyBacklogUnregister, error) {
		return nil, errors.New("registration-secret")
	})
	stopObservation()
	stopObservation()

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(mustNudgeReconcileKey(t, "scope", "still-runs"), nudgeCauseCommandCommit); err != nil {
		t.Fatalf("Enqueue after telemetry failure: %v", err)
	}
	receiveBeforeDeadline(t, callback)
	cancel()
	if err := receiveBeforeDeadline(t, done); err != nil {
		t.Fatalf("controller.Run: %v", err)
	}

	const warning = "nudge keyed backlog observation unavailable\n"
	if got := stderr.String(); got != warning {
		t.Fatalf("stderr = %q, want one static warning %q", got, warning)
	}
	if strings.Contains(stderr.String(), "registration-secret") {
		t.Fatalf("warning leaked registration error content: %q", stderr.String())
	}
}

func TestNudgeKeyBacklogUnregisterPanicIsContainedAndIdempotent(t *testing.T) {
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	var calls atomic.Int64
	var stderr bytes.Buffer
	stopObservation := startNudgeKeyBacklogObservationWithRegistrar(controller, newNudgeKeyBacklogWarnings(&stderr), func(telemetry.NudgeKeyBacklogObserver) (telemetry.NudgeKeyBacklogUnregister, error) {
		return func() error {
			calls.Add(1)
			panic("unregister-secret")
		}, nil
	})
	stopObservation()
	stopObservation()
	if got := calls.Load(); got != 1 {
		t.Fatalf("unregister calls = %d, want 1", got)
	}
	if got := stderr.String(); got != "nudge keyed backlog observation unavailable\n" {
		t.Fatalf("stderr = %q, want one static unregister warning", got)
	}
	if strings.Contains(stderr.String(), "unregister-secret") {
		t.Fatalf("warning leaked unregister panic content: %q", stderr.String())
	}
}

func assertNudgeKeyBacklogGauge(t *testing.T, metrics metricdata.ResourceMetrics, wantDepth int64, wantAge float64, wantState string) {
	t.Helper()
	var depthFound, ageFound bool
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			switch current.Name {
			case testNudgeKeyBacklogDepthMetric:
				gauge, ok := current.Data.(metricdata.Gauge[int64])
				if !ok || len(gauge.DataPoints) != 1 || gauge.DataPoints[0].Value != wantDepth || gauge.DataPoints[0].Attributes.Len() != 0 {
					t.Fatalf("depth gauge = %T %+v, want one unlabeled value %d", current.Data, current.Data, wantDepth)
				}
				depthFound = true
			case testNudgeKeyBacklogAgeMetric:
				gauge, ok := current.Data.(metricdata.Gauge[float64])
				if !ok || len(gauge.DataPoints) != 1 {
					t.Fatalf("age gauge = %T %+v, want one point", current.Data, current.Data)
				}
				point := gauge.DataPoints[0]
				state, ok := point.Attributes.Value("state")
				if point.Value != wantAge || point.Attributes.Len() != 1 || !ok || state.AsString() != wantState {
					t.Fatalf("age point = %+v, want value=%v state=%q only", point, wantAge, wantState)
				}
				ageFound = true
			}
		}
	}
	if !depthFound || !ageFound {
		t.Fatalf("backlog gauges found depth=%v age=%v", depthFound, ageFound)
	}
}
