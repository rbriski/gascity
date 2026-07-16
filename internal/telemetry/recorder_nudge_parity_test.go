package telemetry

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgeparity"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestRecordNudgeParityResultUsesOnlyBoundedLabelsAndEvidenceBackedLatency(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	ctx := context.Background()
	started := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	secret := "secret-operation-session-command"
	expected := nudgeparity.Observation{
		OperationID: secret,
		Side:        nudgeparity.SideExpected,
		Input:       nudgeparity.Input{TargetSession: secret},
		Timing: nudgeparity.TimingEvidence{
			EnqueuedAt:       started,
			PlannedAt:        started.Add(100 * time.Millisecond),
			NativeStartedAt:  started.Add(150 * time.Millisecond),
			NativeStartProof: nudgeparity.NativeStartProofT8,
		},
	}
	actual := nudgeparity.Observation{
		OperationID: secret,
		Side:        nudgeparity.SideActual,
		Input:       nudgeparity.Input{TargetSession: secret},
		Timing: nudgeparity.TimingEvidence{
			EnqueuedAt: started,
			PlannedAt:  started.Add(200 * time.Millisecond),
		},
	}
	result := nudgeparity.Result{
		OperationID:    secret,
		Classification: nudgeparity.ClassificationSame,
		Reason:         nudgeparity.ReasonEquivalent,
		Expected:       expected,
		HasExpected:    true,
		Actual:         actual,
		HasActual:      true,
		ExpectedLatency: nudgeparity.Latency{
			EnqueueToPlan:           100 * time.Millisecond,
			HasEnqueueToPlan:        true,
			EnqueueToNativeStart:    150 * time.Millisecond,
			HasEnqueueToNativeStart: true,
		},
		ActualLatency: nudgeparity.Latency{
			EnqueueToPlan:    200 * time.Millisecond,
			HasEnqueueToPlan: true,
		},
	}

	if err := RecordNudgeParityResult(ctx, result); err != nil {
		t.Fatalf("RecordNudgeParityResult(): %v", err)
	}
	metrics := collectNudgeKeyBacklogMetrics(t, reader)
	comparison := requireInt64SumMetric(t, metrics, nudgeParityComparisonTotalMetric)
	if len(comparison.DataPoints) != 1 || comparison.DataPoints[0].Value != 1 {
		t.Fatalf("comparison metric = %#v, want one count", comparison)
	}
	assertMetricAttributes(t, comparison.DataPoints[0].Attributes, map[string]string{
		"classification": "same",
		"reason":         "equivalent",
	})

	planning := requireFloat64HistogramMetric(t, metrics, nudgeParityEnqueueToPlanMetric)
	if len(planning.DataPoints) != 2 {
		t.Fatalf("planning histogram points = %d, want 2", len(planning.DataPoints))
	}
	planBySide := histogramSumsBySide(t, planning)
	if planBySide["expected"] != 100 || planBySide["actual"] != 200 {
		t.Fatalf("planning histogram sums = %#v, want expected=100 actual=200", planBySide)
	}

	native := requireFloat64HistogramMetric(t, metrics, nudgeParityEnqueueToNativeStartMetric)
	if len(native.DataPoints) != 1 || native.DataPoints[0].Count != 1 || native.DataPoints[0].Sum != 150 {
		t.Fatalf("native histogram = %#v, want one 150ms T8 point", native)
	}
	assertMetricAttributes(t, native.DataPoints[0].Attributes, map[string]string{
		"classification": "same",
		"evidence":       "t8_native_entry",
		"side":           "expected",
	})
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			for _, attrs := range metricAttributeSets(current.Data) {
				for _, keyValue := range attrs.ToSlice() {
					if strings.Contains(keyValue.Value.Emit(), secret) {
						t.Fatalf("metric %s leaked identity in %s=%s", current.Name, keyValue.Key, keyValue.Value.Emit())
					}
				}
			}
		}
	}
}

func TestRecordNudgeParityResultRejectsUnboundedEnumsAndUnprovenNativeLatency(t *testing.T) {
	started := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	base := nudgeparity.Result{
		Classification: nudgeparity.ClassificationSame,
		Reason:         nudgeparity.ReasonEquivalent,
		Expected: nudgeparity.Observation{
			Side: nudgeparity.SideExpected,
			Timing: nudgeparity.TimingEvidence{
				EnqueuedAt:       started,
				NativeStartedAt:  started.Add(time.Millisecond),
				NativeStartProof: nudgeparity.NativeStartProofT8,
			},
		},
		HasExpected: true,
		ExpectedLatency: nudgeparity.Latency{
			EnqueueToNativeStart:    time.Millisecond,
			HasEnqueueToNativeStart: true,
		},
	}
	for _, test := range []struct {
		name   string
		mutate func(*nudgeparity.Result)
	}{
		{name: "classification", mutate: func(result *nudgeparity.Result) { result.Classification = 255 }},
		{name: "reason", mutate: func(result *nudgeparity.Result) { result.Reason = 255 }},
		{name: "side", mutate: func(result *nudgeparity.Result) { result.Expected.Side = nudgeparity.SideActual }},
		{name: "native proof", mutate: func(result *nudgeparity.Result) {
			result.Expected.Timing.NativeStartProof = nudgeparity.NativeStartProofNone
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			result := base
			test.mutate(&result)
			if err := RecordNudgeParityResult(context.Background(), result); err == nil {
				t.Fatal("RecordNudgeParityResult() succeeded, want validation error")
			}
		})
	}
}

func TestNudgeParitySnapshotRecorderEmitsMonotonicIdentityFreeDeltas(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	ctx := context.Background()
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	comparator, err := nudgeparity.New(nudgeparity.Config{
		Retention:  time.Second,
		MaxPending: 2,
		MaxRecent:  2,
		Now:        func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	observation := nudgeparity.Observation{
		OperationID: "operation-1",
		Side:        nudgeparity.SideExpected,
		Input: nudgeparity.Input{
			CommandDigest:    "digest",
			TargetSession:    "session",
			TargetGeneration: 1,
			TargetLaunch:     "launch",
		},
		Watermarks: nudgeparity.Watermarks{
			StoreLineage:    "store",
			DurableRevision: 1,
			ConfigRevision:  "config",
			RuntimeRevision: 1,
			OwnerEpoch:      1,
		},
		Plan:       nudgeparity.Plan{Decision: nudgeparity.DecisionExecute, Action: nudgeparity.ActionNudge},
		CapturedAt: now,
	}
	if _, err := comparator.Observe(observation); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if _, err := comparator.Sweep(); err != nil {
		t.Fatal(err)
	}

	recorder := NewNudgeParitySnapshotRecorder()
	first := nudgeparity.ShadowSnapshot{
		Accepted:           5,
		NotRunning:         1,
		Full:               2,
		Invalid:            1,
		PlannerFailures:    1,
		ComparatorFailures: 1,
		SinkFailures:       1,
		ShutdownDrained:    1,
		Unreported:         1,
		Comparator:         comparator.Snapshot(),
	}
	if err := recorder.Record(ctx, first); err != nil {
		t.Fatalf("first Record(): %v", err)
	}
	second := first
	second.Accepted = 7
	second.Full = 3
	if err := recorder.Record(ctx, second); err != nil {
		t.Fatalf("second Record(): %v", err)
	}
	metrics := collectNudgeKeyBacklogMetrics(t, reader)
	events := requireInt64SumMetric(t, metrics, nudgeParityShadowEventTotalMetric)
	got := int64SumsByAttribute(t, events, "event")
	want := map[string]int64{
		"accepted":           7,
		"not_running":        1,
		"full":               3,
		"invalid":            1,
		"planner_failure":    1,
		"comparator_failure": 1,
		"sink_failure":       1,
		"shutdown_drained":   1,
		"unreported":         1,
		"expired":            1,
	}
	if len(got) != len(want) {
		t.Fatalf("event series = %#v, want %#v", got, want)
	}
	for event, wantValue := range want {
		if got[event] != wantValue {
			t.Errorf("event %q = %d, want %d", event, got[event], wantValue)
		}
	}

	regressed := second
	regressed.Accepted--
	if err := recorder.Record(ctx, regressed); err == nil {
		t.Fatal("regressed snapshot succeeded, want error")
	}
}

func TestNudgeParitySnapshotRecorderRejectsCounterDeltaOverflow(t *testing.T) {
	recorder := NewNudgeParitySnapshotRecorder()
	err := recorder.Record(context.Background(), nudgeparity.ShadowSnapshot{Accepted: ^uint64(0)})
	if err == nil || !strings.Contains(err.Error(), "accepted counter delta overflows int64") {
		t.Fatalf("Record() error = %v, want accepted overflow", err)
	}
}

func requireInt64SumMetric(t *testing.T, metrics metricdata.ResourceMetrics, name string) metricdata.Sum[int64] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name == name {
				sum, ok := current.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("metric %s data = %T, want int64 sum", name, current.Data)
				}
				return sum
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Sum[int64]{}
}

func requireFloat64HistogramMetric(t *testing.T, metrics metricdata.ResourceMetrics, name string) metricdata.Histogram[float64] {
	t.Helper()
	for _, scope := range metrics.ScopeMetrics {
		for _, current := range scope.Metrics {
			if current.Name == name {
				histogram, ok := current.Data.(metricdata.Histogram[float64])
				if !ok {
					t.Fatalf("metric %s data = %T, want float64 histogram", name, current.Data)
				}
				return histogram
			}
		}
	}
	t.Fatalf("metric %s not found", name)
	return metricdata.Histogram[float64]{}
}

func assertMetricAttributes(t *testing.T, attributes attribute.Set, want map[string]string) {
	t.Helper()
	if attributes.Len() != len(want) {
		t.Fatalf("attributes = %v, want exactly %#v", attributes, want)
	}
	for key, wantValue := range want {
		value, ok := attributes.Value(attribute.Key(key))
		if !ok || value.AsString() != wantValue {
			t.Errorf("attribute %q = %q/%t, want %q", key, value.AsString(), ok, wantValue)
		}
	}
}

func histogramSumsBySide(t *testing.T, histogram metricdata.Histogram[float64]) map[string]float64 {
	t.Helper()
	result := make(map[string]float64, len(histogram.DataPoints))
	for _, point := range histogram.DataPoints {
		value, ok := point.Attributes.Value("side")
		if !ok || point.Count != 1 {
			t.Fatalf("histogram point = %#v, want one side-tagged sample", point)
		}
		result[value.AsString()] = point.Sum
	}
	return result
}

func int64SumsByAttribute(t *testing.T, sum metricdata.Sum[int64], key string) map[string]int64 {
	t.Helper()
	result := make(map[string]int64, len(sum.DataPoints))
	for _, point := range sum.DataPoints {
		value, ok := point.Attributes.Value(attribute.Key(key))
		if !ok || point.Attributes.Len() != 1 {
			t.Fatalf("sum point = %#v, want only %q", point, key)
		}
		result[value.AsString()] += point.Value
	}
	return result
}

func metricAttributeSets(data metricdata.Aggregation) []attribute.Set {
	switch typed := data.(type) {
	case metricdata.Sum[int64]:
		result := make([]attribute.Set, 0, len(typed.DataPoints))
		for _, point := range typed.DataPoints {
			result = append(result, point.Attributes)
		}
		return result
	case metricdata.Histogram[float64]:
		result := make([]attribute.Set, 0, len(typed.DataPoints))
		for _, point := range typed.DataPoints {
			result = append(result, point.Attributes)
		}
		return result
	default:
		return nil
	}
}

var _ nudgeparity.ResultSink = RecordNudgeParityResult
