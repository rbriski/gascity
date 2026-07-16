package telemetry

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/nudgeparity"
)

func TestNudgeParityShadowSmokeLegacyActsOnceAndShadowEmitsComparison(t *testing.T) {
	reader := installNudgeKeyBacklogMetricReader(t)
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	plannerObserved := make(chan nudgeparity.PlanningInput, 1)
	comparisonObserved := make(chan nudgeparity.Result, 1)
	shadow, err := nudgeparity.NewShadow(nudgeparity.ShadowConfig{
		Comparator: nudgeparity.Config{
			Retention:  time.Second,
			MaxPending: 4,
			MaxRecent:  4,
			Now:        func() time.Time { return now },
		},
		QueueCapacity: 2,
		Planner: nudgeparity.PlannerFunc(func(_ context.Context, input nudgeparity.PlanningInput) (nudgeparity.Planned, error) {
			plannerObserved <- input
			return nudgeparity.Planned{
				Plan:      nudgeparity.Plan{Decision: nudgeparity.DecisionExecute, Action: nudgeparity.ActionNudge},
				PlannedAt: now,
			}, nil
		}),
		Sink: func(ctx context.Context, result nudgeparity.Result) error {
			if err := RecordNudgeParityResult(ctx, result); err != nil {
				return err
			}
			comparisonObserved <- result
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewShadow(): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- shadow.Run(ctx) }()
	<-shadow.Ready()

	const operationID = "operation-secret-not-a-metric-label"
	sample := nudgeparity.Sample{
		OperationID: operationID,
		Input: nudgeparity.Input{
			CommandDigest:    "digest-secret-not-a-metric-label",
			TargetSession:    "session-secret-not-a-metric-label",
			TargetGeneration: 3,
			TargetLaunch:     "launch-secret-not-a-metric-label",
		},
		Watermarks: nudgeparity.Watermarks{
			StoreLineage:    "store-secret-not-a-metric-label",
			DurableRevision: 5,
			ConfigRevision:  "config-secret-not-a-metric-label",
			RuntimeRevision: 7,
			OwnerEpoch:      11,
		},
		ExpectedPlan: nudgeparity.Plan{Decision: nudgeparity.DecisionExecute, Action: nudgeparity.ActionNudge},
		CapturedAt:   now,
		ExpectedTiming: nudgeparity.TimingEvidence{
			EnqueuedAt: now,
			PlannedAt:  now,
		},
	}
	disposition, err := shadow.Submit(sample)
	if err != nil || disposition != nudgeparity.SubmissionAccepted {
		t.Fatalf("Submit() = %s, %v; want accepted", disposition, err)
	}
	var legacyEffects atomic.Int64
	legacyEffects.Add(1)

	plannerInput := <-plannerObserved
	if plannerInput.OperationID != sample.OperationID || plannerInput.Input != sample.Input || plannerInput.Watermarks != sample.Watermarks {
		t.Fatalf("keyed planner did not receive exact captured input: %#v", plannerInput)
	}
	comparison := <-comparisonObserved
	if comparison.Classification != nudgeparity.ClassificationSame || comparison.Reason != nudgeparity.ReasonEquivalent {
		t.Fatalf("comparison = %s/%s, want same/equivalent", comparison.Classification, comparison.Reason)
	}
	if legacyEffects.Load() != 1 {
		t.Fatalf("legacy effects = %d, want exactly one", legacyEffects.Load())
	}

	snapshotRecorder := NewNudgeParitySnapshotRecorder()
	if err := snapshotRecorder.Record(ctx, shadow.Snapshot()); err != nil {
		t.Fatalf("Record(snapshot): %v", err)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() shutdown: %v", err)
	}
	metrics := collectNudgeKeyBacklogMetrics(t, reader)
	comparisonMetric := requireInt64SumMetric(t, metrics, nudgeParityComparisonTotalMetric)
	if len(comparisonMetric.DataPoints) != 1 || comparisonMetric.DataPoints[0].Value != 1 {
		t.Fatalf("comparison metric = %#v, want exactly one", comparisonMetric)
	}
	assertMetricAttributes(t, comparisonMetric.DataPoints[0].Attributes, map[string]string{
		"classification": "same",
		"reason":         "equivalent",
	})
	events := int64SumsByAttribute(t, requireInt64SumMetric(t, metrics, nudgeParityShadowEventTotalMetric), "event")
	if events["accepted"] != 1 {
		t.Fatalf("accepted telemetry = %#v, want one", events)
	}
}
