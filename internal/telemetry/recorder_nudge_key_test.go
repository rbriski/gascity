package telemetry

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

const (
	testNudgeKeyBacklogDepthMetric = "gc.reconcile.nudge_shadow.backlog.keys"
	testNudgeKeyBacklogAgeMetric   = "gc.reconcile.nudge_shadow.backlog.oldest_age_ms"
)

func TestNudgeKeyBacklogRecordCannotCarryIdentityOrContent(t *testing.T) {
	typ := reflect.TypeOf(NudgeKeyBacklogRecord{})
	want := map[string]reflect.Type{
		"Depth":     reflect.TypeOf(int64(0)),
		"OldestAge": reflect.TypeOf(time.Duration(0)),
		"AgeState":  reflect.TypeOf(NudgeKeyBacklogAgeState(0)),
	}
	if typ.NumField() != len(want) {
		t.Fatalf("backlog record fields = %d, want exactly %d", typ.NumField(), len(want))
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if want[field.Name] != field.Type {
			t.Errorf("backlog field %q type = %v, want %v", field.Name, field.Type, want[field.Name])
		}
	}
}

func TestNudgeKeyBacklogObserversAggregateAndUnregisterWithoutIdentityLabels(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	unregisterFirst, err := RegisterNudgeKeyBacklogObserver(func() NudgeKeyBacklogRecord {
		return NudgeKeyBacklogRecord{Depth: 2, OldestAge: 125 * time.Millisecond, AgeState: NudgeKeyBacklogAgeObserved}
	})
	if err != nil {
		t.Fatalf("RegisterNudgeKeyBacklogObserver(first): %v", err)
	}
	unregisterSecond, err := RegisterNudgeKeyBacklogObserver(func() NudgeKeyBacklogRecord {
		return NudgeKeyBacklogRecord{Depth: 3, OldestAge: 75 * time.Millisecond, AgeState: NudgeKeyBacklogAgeObserved}
	})
	if err != nil {
		t.Fatalf("RegisterNudgeKeyBacklogObserver(second): %v", err)
	}
	t.Cleanup(func() {
		_ = unregisterFirst()
		_ = unregisterSecond()
	})

	assertNudgeKeyBacklogMetrics(t, collectNudgeKeyBacklogMetrics(t, reader), 5, 125, "observed")
	if err := unregisterFirst(); err != nil {
		t.Fatalf("unregister first: %v", err)
	}
	assertNudgeKeyBacklogMetrics(t, collectNudgeKeyBacklogMetrics(t, reader), 3, 75, "observed")
	if err := unregisterSecond(); err != nil {
		t.Fatalf("unregister second: %v", err)
	}
	assertNudgeKeyBacklogMetrics(t, collectNudgeKeyBacklogMetrics(t, reader), 0, 0, "empty")
}

func TestNudgeKeyBacklogAggregationFailsSafeWhenAnyActiveAgeIsUnknown(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	unregisterKnown, err := RegisterNudgeKeyBacklogObserver(func() NudgeKeyBacklogRecord {
		return NudgeKeyBacklogRecord{Depth: 2, OldestAge: 125 * time.Millisecond, AgeState: NudgeKeyBacklogAgeObserved}
	})
	if err != nil {
		t.Fatalf("register known observer: %v", err)
	}
	unregisterUnknown, err := RegisterNudgeKeyBacklogObserver(func() NudgeKeyBacklogRecord {
		return NudgeKeyBacklogRecord{Depth: 1, AgeState: NudgeKeyBacklogAgeUnavailable}
	})
	if err != nil {
		t.Fatalf("register unknown observer: %v", err)
	}
	t.Cleanup(func() {
		_ = unregisterKnown()
		_ = unregisterUnknown()
	})
	assertNudgeKeyBacklogMetrics(t, collectNudgeKeyBacklogMetrics(t, reader), 3, 0, "unavailable")
}

func TestNudgeKeyBacklogObserverPanicIsContainedAndReportedByCollection(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	unregister, err := RegisterNudgeKeyBacklogObserver(func() NudgeKeyBacklogRecord {
		panic("session-secret command-secret")
	})
	if err != nil {
		t.Fatalf("RegisterNudgeKeyBacklogObserver: %v", err)
	}
	t.Cleanup(func() { _ = unregister() })
	var metrics metricdata.ResourceMetrics
	err = reader.Collect(context.Background(), &metrics)
	if err == nil || !strings.Contains(err.Error(), "keyed nudge backlog observer failed") {
		t.Fatalf("Collect error = %v, want static observer failure", err)
	}
	for _, secret := range []string{"session-secret", "command-secret"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("Collect error leaked panic content %q: %v", secret, err)
		}
	}
}

func TestRegisterNudgeKeyBacklogObserverRejectsNil(t *testing.T) {
	if unregister, err := RegisterNudgeKeyBacklogObserver(nil); err == nil || unregister != nil {
		t.Fatalf("RegisterNudgeKeyBacklogObserver(nil) = (%v, %v), want nil error result", unregister, err)
	}
}

func installNudgeKeyBacklogMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(provider)
	ResetInstrumentsForTest()
	t.Cleanup(func() {
		if err := provider.Shutdown(context.Background()); err != nil && !errors.Is(err, sdkmetric.ErrReaderShutdown) {
			t.Errorf("metric provider shutdown: %v", err)
		}
		otel.SetMeterProvider(previous)
		ResetInstrumentsForTest()
	})
	return reader
}

func collectNudgeKeyBacklogMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return out
}

func assertNudgeKeyBacklogMetrics(t *testing.T, metrics metricdata.ResourceMetrics, wantDepth int64, wantAge float64, wantState string) {
	t.Helper()
	var depthFound, ageFound bool
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			switch current.Name {
			case testNudgeKeyBacklogDepthMetric:
				gauge, ok := current.Data.(metricdata.Gauge[int64])
				if !ok || len(gauge.DataPoints) != 1 {
					t.Fatalf("depth metric = %T %+v, want one int64 gauge point", current.Data, current.Data)
				}
				point := gauge.DataPoints[0]
				if point.Value != wantDepth || point.Attributes.Len() != 0 {
					t.Fatalf("depth point = %+v, want value %d and no labels", point, wantDepth)
				}
				depthFound = true
			case testNudgeKeyBacklogAgeMetric:
				gauge, ok := current.Data.(metricdata.Gauge[float64])
				if !ok || len(gauge.DataPoints) != 1 {
					t.Fatalf("age metric = %T %+v, want one float64 gauge point", current.Data, current.Data)
				}
				point := gauge.DataPoints[0]
				state, ok := point.Attributes.Value("state")
				if point.Value != wantAge || point.Attributes.Len() != 1 || !ok || state.AsString() != wantState {
					t.Fatalf("age point = %+v, want value %v and state=%q only", point, wantAge, wantState)
				}
				ageFound = true
			}
		}
	}
	if !depthFound || !ageFound {
		t.Fatalf("backlog metrics found depth=%v age=%v", depthFound, ageFound)
	}
}
