package main

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/contract"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/telemetry"
)

const (
	testNudgeKeySchedulingTotalMetric = "gc.reconcile.nudge_shadow.scheduling.total"
	testNudgeKeyQueueDelayMetric      = "gc.reconcile.nudge_shadow.queue_delay_ms"
)

func TestNewNudgeKeySchedulingObservationPreservesBoundedSchedulingFacts(t *testing.T) {
	firstReady := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	now := firstReady.Add(37 * time.Millisecond)
	batch := nudgeReconcileBatch{
		Causes:          nudgeCauseCommandCommit | nudgeCauseRuntimeReadiness,
		FirstEnqueuedAt: firstReady,
		WorkqueueReplay: true,
	}

	got := newNudgeKeySchedulingObservation(batch, now)
	if got.CauseBits != uint8(batch.Causes) {
		t.Fatalf("CauseBits = 0x%x, want 0x%x", got.CauseBits, uint8(batch.Causes))
	}
	if !got.WorkqueueReplay {
		t.Fatal("WorkqueueReplay = false, want true")
	}
	if got.QueueDelayState != nudgeKeyQueueDelayObserved || got.QueueDelay != 37*time.Millisecond {
		t.Fatalf("queue delay = %v/%v, want %v/%v", got.QueueDelayState, got.QueueDelay, nudgeKeyQueueDelayObserved, 37*time.Millisecond)
	}
	if got.ScopeCertification != nudgeKeyScopeProvisional {
		t.Fatalf("ScopeCertification = %v, want %v", got.ScopeCertification, nudgeKeyScopeProvisional)
	}
	if got.Authorization != nudgeKeyAuthorizationNotEvaluated {
		t.Fatalf("Authorization = %v, want %v", got.Authorization, nudgeKeyAuthorizationNotEvaluated)
	}
	if got.EffectsAdmissible {
		t.Fatal("EffectsAdmissible = true, want false in scheduling-only shadow")
	}
}

func TestNewNudgeKeySchedulingObservationClassifiesUnavailableAndRegressedDelay(t *testing.T) {
	now := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		first     time.Time
		wantState nudgeKeyQueueDelayState
	}{
		{name: "workqueue replay without a source timestamp", wantState: nudgeKeyQueueDelayUnavailable},
		{name: "clock regression", first: now.Add(time.Millisecond), wantState: nudgeKeyQueueDelayClockRegressed},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := newNudgeKeySchedulingObservation(nudgeReconcileBatch{
				FirstEnqueuedAt: tc.first,
				WorkqueueReplay: true,
			}, now)
			if got.QueueDelayState != tc.wantState {
				t.Fatalf("QueueDelayState = %v, want %v", got.QueueDelayState, tc.wantState)
			}
			if got.QueueDelay != 0 {
				t.Fatalf("QueueDelay = %v, want zero when delay is not observable", got.QueueDelay)
			}
		})
	}
}

func TestNudgeKeySchedulingObservationTypeCannotCarryIdentityOrContent(t *testing.T) {
	typ := reflect.TypeOf(nudgeKeySchedulingObservation{})
	wantFields := map[string]bool{
		"CauseBits":          true,
		"WorkqueueReplay":    true,
		"QueueDelay":         true,
		"QueueDelayState":    true,
		"ScopeCertification": true,
		"Authorization":      true,
		"EffectsAdmissible":  true,
	}
	if typ.NumField() != len(wantFields) {
		t.Fatalf("observation fields = %d, want exactly %d scheduling-only fields", typ.NumField(), len(wantFields))
	}
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		if !wantFields[field.Name] {
			t.Errorf("observation carries unapproved field %q", field.Name)
		}
		if field.Type.Kind() == reflect.String && field.Name != "QueueDelayState" && field.Name != "ScopeCertification" && field.Name != "Authorization" {
			t.Errorf("observation carries unapproved free-form string field %q", field.Name)
		}
	}
}

func TestObserveNudgeKeySchedulingRecordsBoundedIdentityFreeMetrics(t *testing.T) {
	reader := installNudgeKeyObservationMetricReader(t)
	firstReady := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	var stderr bytes.Buffer
	warnings := newNudgeKeyObservationWarnings(&stderr)
	observeNudgeKeyScheduling(context.Background(), nudgeReconcileBatch{
		Causes:          nudgeCauseCommandCommit | nudgeCauseProviderResult,
		FirstEnqueuedAt: firstReady,
	}, firstReady.Add(25*time.Millisecond), warnings)
	if stderr.Len() != 0 {
		t.Fatalf("stderr = %q, want no observation warning", stderr.String())
	}

	metrics := collectNudgeKeyObservationMetrics(t, reader)
	counter := findNudgeKeyObservationCounter(t, metrics, testNudgeKeySchedulingTotalMetric)
	if len(counter.DataPoints) != 1 || counter.DataPoints[0].Value != 1 {
		t.Fatalf("scheduling counter datapoints = %+v, want one value=1", counter.DataPoints)
	}
	assertNudgeKeyObservationAttributes(t, counter.DataPoints[0].Attributes, uint8(nudgeCauseCommandCommit|nudgeCauseProviderResult), false, nudgeKeyQueueDelayObserved)

	histogram := findNudgeKeyObservationHistogram(t, metrics, testNudgeKeyQueueDelayMetric)
	if len(histogram.DataPoints) != 1 {
		t.Fatalf("queue-delay histogram datapoints = %+v, want one", histogram.DataPoints)
	}
	point := histogram.DataPoints[0]
	if point.Count != 1 || point.Sum != 25 {
		t.Fatalf("queue-delay histogram count/sum = %d/%v, want 1/25ms", point.Count, point.Sum)
	}
	assertNudgeKeyObservationAttributes(t, point.Attributes, uint8(nudgeCauseCommandCommit|nudgeCauseProviderResult), false, nudgeKeyQueueDelayObserved)
}

func TestExactWakeAdmissionRecordsProductionKeySchedulingWithoutPatrol(t *testing.T) {
	reader := installNudgeKeyObservationMetricReader(t)
	dir := t.TempDir()
	if err := contract.WriteProjectIdentity(fsys.OSFS{}, dir, "metric-scope-must-not-be-a-label"); err != nil {
		t.Fatalf("WriteProjectIdentity: %v", err)
	}
	cr := &CityRuntime{
		cityPath: dir,
		cfg:      supervisorCfg(),
		stderr:   &bytes.Buffer{},
	}
	if err := cr.installNudgeKeyShadow(); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	if cr.nudgeKeyController == nil {
		t.Fatal("installNudgeKeyShadow did not install the production observer")
	}

	productionCallback := cr.nudgeKeyController.reconcile
	callbackDone := make(chan struct{}, 1)
	cr.nudgeKeyController.reconcile = func(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		outcome := productionCallback(ctx, key, batch)
		callbackDone <- struct{}{}
		return outcome
	}

	ctx, cancel := context.WithCancel(context.Background())
	stopController := cr.startNudgeKeyController(ctx)
	wakeCh := make(chan struct{}, 1)
	lis, err := startNudgeWakeListenerWithHints(ctx, dir, wakeCh, func(hint nudgeWakeHint) {
		cr.acceptNudgeKeyShadowHint(ctx, hint)
	}, cr.stderr, "test")
	if err != nil {
		cancel()
		stopController()
		t.Fatalf("startNudgeWakeListenerWithHints: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		stopController()
		_ = lis.Close()
	})

	item := newQueuedNudge("display-alias-must-not-be-a-label", "message-must-not-be-a-label", time.Now())
	item.ID = "command-must-not-be-a-label"
	item.SessionID = "session-must-not-be-a-label"
	if err := enqueueQueuedNudgeWithStore(dir, beads.NudgesStore{Store: beads.NewMemStore()}, item); err != nil {
		t.Fatalf("enqueueQueuedNudgeWithStore: %v", err)
	}
	receiveBeforeDeadline(t, callbackDone)
	receiveBeforeDeadline(t, wakeCh)

	metrics := collectNudgeKeyObservationMetrics(t, reader)
	counter := findNudgeKeyObservationCounter(t, metrics, testNudgeKeySchedulingTotalMetric)
	if len(counter.DataPoints) != 1 || counter.DataPoints[0].Value != 1 {
		t.Fatalf("scheduling counter datapoints = %+v, want one admitted-key callback", counter.DataPoints)
	}
	assertNudgeKeyObservationAttributes(t, counter.DataPoints[0].Attributes, uint8(nudgeCauseCommandCommit), false, nudgeKeyQueueDelayObserved)
	for _, attr := range counter.DataPoints[0].Attributes.ToSlice() {
		value := attr.Value.Emit()
		for _, forbidden := range []string{"metric-scope", "command-must", "session-must", "display-alias", "message-must"} {
			if strings.Contains(value, forbidden) {
				t.Fatalf("metric attribute %q leaked identity/content %q", attr.Key, value)
			}
		}
	}
}

func TestObserveNudgeKeySchedulingDoesNotInventReplayDelay(t *testing.T) {
	reader := installNudgeKeyObservationMetricReader(t)
	var stderr bytes.Buffer
	observeNudgeKeyScheduling(context.Background(), nudgeReconcileBatch{WorkqueueReplay: true}, time.Now(), newNudgeKeyObservationWarnings(&stderr))

	metrics := collectNudgeKeyObservationMetrics(t, reader)
	counter := findNudgeKeyObservationCounter(t, metrics, testNudgeKeySchedulingTotalMetric)
	if len(counter.DataPoints) != 1 {
		t.Fatalf("scheduling counter datapoints = %+v, want one", counter.DataPoints)
	}
	assertNudgeKeyObservationAttributes(t, counter.DataPoints[0].Attributes, 0, true, nudgeKeyQueueDelayUnavailable)
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name == testNudgeKeyQueueDelayMetric {
				t.Fatalf("unobservable replay delay emitted histogram data: %+v", metric.Data)
			}
		}
	}
}

func TestEmitNudgeKeySchedulingObservationContainsTelemetryPanic(t *testing.T) {
	var stderr bytes.Buffer
	warnings := newNudgeKeyObservationWarnings(&stderr)
	emitNudgeKeySchedulingObservation(context.Background(), nudgeKeySchedulingObservation{}, warnings, func(context.Context, nudgeKeySchedulingObservation) error {
		panic("session-secret command-secret message-secret")
	})

	got := stderr.String()
	if !strings.Contains(got, "nudge keyed shadow scheduling observation failed") {
		t.Fatalf("stderr = %q, want static telemetry failure warning", got)
	}
	for _, secret := range []string{"session-secret", "command-secret", "message-secret"} {
		if strings.Contains(got, secret) {
			t.Fatalf("stderr leaked panic content %q: %q", secret, got)
		}
	}
}

func TestEmitNudgeKeySchedulingObservationWarnsOnceAcrossRepeatedFailures(t *testing.T) {
	var stderr bytes.Buffer
	warnings := newNudgeKeyObservationWarnings(&stderr)
	emit := func(context.Context, nudgeKeySchedulingObservation) error {
		return errors.New("telemetry unavailable")
	}
	for i := 0; i < 10; i++ {
		emitNudgeKeySchedulingObservation(context.Background(), nudgeKeySchedulingObservation{}, warnings, emit)
	}
	const warning = "nudge keyed shadow scheduling observation failed\n"
	if got := stderr.String(); got != warning {
		t.Fatalf("stderr = %q, want exactly one static warning %q", got, warning)
	}
}

func TestEmitNudgeKeySchedulingObservationContainsPanickingWarningWriter(_ *testing.T) {
	warnings := newNudgeKeyObservationWarnings(panickingNudgeKeyWarningWriter{})
	emit := func(context.Context, nudgeKeySchedulingObservation) error {
		return errors.New("telemetry unavailable")
	}
	for i := 0; i < 10; i++ {
		emitNudgeKeySchedulingObservation(context.Background(), nudgeKeySchedulingObservation{}, warnings, emit)
	}
}

func TestNudgeKeySchedulingRecorderReturnsAndRetainsConstructorErrors(t *testing.T) {
	counterErr := errors.New("counter constructor failed")
	histogramErr := errors.New("histogram constructor failed")
	meter := &failingNudgeKeyMeter{counterErr: counterErr, histogramErr: histogramErr}
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(failingNudgeKeyMeterProvider{meter: meter})
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		telemetry.ResetInstrumentsForTest()
	})

	record := telemetry.NudgeKeySchedulingRecord{QueueDelayState: telemetry.NudgeKeyQueueDelayObserved}
	for attempt := 0; attempt < 2; attempt++ {
		err := telemetry.RecordNudgeKeyScheduling(context.Background(), record)
		if !errors.Is(err, counterErr) || !errors.Is(err, histogramErr) {
			t.Fatalf("RecordNudgeKeyScheduling error = %v, want retained counter and histogram errors", err)
		}
	}
	if meter.counterCalls != 1 || meter.histogramCalls != 1 {
		t.Fatalf("constructor calls = counter:%d histogram:%d, want one retained initialization", meter.counterCalls, meter.histogramCalls)
	}

	meter.counterErr = nil
	meter.histogramErr = nil
	telemetry.ResetInstrumentsForTest()
	if err := telemetry.RecordNudgeKeyScheduling(context.Background(), record); err != nil {
		t.Fatalf("RecordNudgeKeyScheduling after reset: %v", err)
	}
	if meter.counterCalls != 2 || meter.histogramCalls != 2 {
		t.Fatalf("constructor calls after reset = counter:%d histogram:%d, want two", meter.counterCalls, meter.histogramCalls)
	}
}

func TestNudgeKeySchedulingInstrumentResetIsRaceSafe(t *testing.T) {
	previous := otel.GetMeterProvider()
	otel.SetMeterProvider(noop.NewMeterProvider())
	telemetry.ResetInstrumentsForTest()
	t.Cleanup(func() {
		otel.SetMeterProvider(previous)
		telemetry.ResetInstrumentsForTest()
	})

	record := telemetry.NudgeKeySchedulingRecord{QueueDelayState: telemetry.NudgeKeyQueueDelayUnavailable}
	var workers sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for attempt := 0; attempt < 100; attempt++ {
				if err := telemetry.RecordNudgeKeyScheduling(context.Background(), record); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		for attempt := 0; attempt < 100; attempt++ {
			telemetry.ResetInstrumentsForTest()
		}
	}()
	workers.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent record/reset: %v", err)
	}
}

func TestNudgeKeyDuplicateAdmissionPreservesFirstEnqueuedDelay(t *testing.T) {
	firstAdmission := time.Date(2026, 7, 15, 8, 0, 0, 0, time.UTC)
	duplicateAdmission := firstAdmission.Add(80 * time.Millisecond)
	callbackAt := firstAdmission.Add(125 * time.Millisecond)
	observed := make(chan nudgeKeySchedulingObservation, 1)
	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		observed <- newNudgeKeySchedulingObservation(batch, callbackAt)
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return firstAdmission }
	key, err := reconcilekey.NewSession("scope", "session")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	controller.now = func() time.Time { return duplicateAdmission }
	if err := controller.Enqueue(key, nudgeCauseProviderResult); err != nil {
		t.Fatalf("duplicate Enqueue: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	got := receiveBeforeDeadline(t, observed)
	if got.QueueDelayState != nudgeKeyQueueDelayObserved || got.QueueDelay != callbackAt.Sub(firstAdmission) {
		t.Fatalf("admitted-key delay = %v/%v, want %v from first admission", got.QueueDelayState, got.QueueDelay, callbackAt.Sub(firstAdmission))
	}
	if want := uint8(nudgeCauseCommandCommit | nudgeCauseProviderResult); got.CauseBits != want {
		t.Fatalf("CauseBits = 0x%x, want coalesced 0x%x", got.CauseBits, want)
	}
	cancel()
	if err := receiveBeforeDeadline(t, done); err != nil {
		t.Fatalf("controller.Run: %v", err)
	}
}

func installNudgeKeyObservationMetricReader(t *testing.T) *sdkmetric.ManualReader {
	t.Helper()
	return installManualMetricReader(t)
}

func collectNudgeKeyObservationMetrics(t *testing.T, reader *sdkmetric.ManualReader) metricdata.ResourceMetrics {
	t.Helper()
	var out metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &out); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return out
}

func findNudgeKeyObservationCounter(t *testing.T, metrics metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			counter, ok := metric.Data.(metricdata.Sum[int64])
			if !ok {
				t.Fatalf("metric %q data = %T, want Sum[int64]", name, metric.Data)
			}
			return counter
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Sum[int64]{}
}

func findNudgeKeyObservationHistogram(t *testing.T, metrics metricdata.ResourceMetrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, metric := range scope.Metrics {
			if metric.Name != name {
				continue
			}
			histogram, ok := metric.Data.(metricdata.Histogram[float64])
			if !ok {
				t.Fatalf("metric %q data = %T, want Histogram[float64]", name, metric.Data)
			}
			return histogram
		}
	}
	t.Fatalf("metric %q not found", name)
	return metricdata.Histogram[float64]{}
}

func assertNudgeKeyObservationAttributes(t *testing.T, attrs attribute.Set, causeBits uint8, replay bool, delayState nudgeKeyQueueDelayState) {
	t.Helper()
	want := map[attribute.Key]attribute.Value{
		"cause_bits":          attribute.Int64Value(int64(causeBits)),
		"workqueue_replay":    attribute.BoolValue(replay),
		"queue_delay_state":   attribute.StringValue(testNudgeKeyQueueDelayStateLabel(delayState)),
		"scope_certification": attribute.StringValue("provisional"),
		"authorization":       attribute.StringValue("not_evaluated"),
		"effects_admissible":  attribute.BoolValue(false),
	}
	got := attrs.ToSlice()
	if len(got) != len(want) {
		t.Fatalf("metric attributes = %+v, want exactly %d bounded fields", got, len(want))
	}
	for key, wantValue := range want {
		value, ok := attrs.Value(key)
		if !ok || value != wantValue {
			t.Errorf("attribute %q = %v (present %v), want %v", key, value, ok, wantValue)
		}
	}
}

func testNudgeKeyQueueDelayStateLabel(state nudgeKeyQueueDelayState) string {
	switch state {
	case nudgeKeyQueueDelayObserved:
		return "observed"
	case nudgeKeyQueueDelayUnavailable:
		return "unavailable"
	case nudgeKeyQueueDelayClockRegressed:
		return "clock_regressed"
	default:
		return "invalid"
	}
}

type panickingNudgeKeyWarningWriter struct{}

func (panickingNudgeKeyWarningWriter) Write([]byte) (int, error) {
	panic("warning writer failed")
}

type failingNudgeKeyMeterProvider struct {
	noop.MeterProvider
	meter *failingNudgeKeyMeter
}

func (provider failingNudgeKeyMeterProvider) Meter(string, ...metric.MeterOption) metric.Meter {
	return provider.meter
}

type failingNudgeKeyMeter struct {
	noop.Meter
	counterErr     error
	histogramErr   error
	counterCalls   int
	histogramCalls int
}

func (meter *failingNudgeKeyMeter) Int64Counter(string, ...metric.Int64CounterOption) (metric.Int64Counter, error) {
	meter.counterCalls++
	return noop.Int64Counter{}, meter.counterErr
}

func (meter *failingNudgeKeyMeter) Float64Histogram(string, ...metric.Float64HistogramOption) (metric.Float64Histogram, error) {
	meter.histogramCalls++
	return noop.Float64Histogram{}, meter.histogramErr
}
