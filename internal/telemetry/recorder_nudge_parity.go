package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/nudgeparity"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	nudgeParityComparisonTotalMetric        = "gc.reconcile.nudge_shadow.comparison.total"
	nudgeParityEnqueueToPlanMetric          = "gc.reconcile.nudge_shadow.enqueue_to_plan_ms"
	nudgeParityEnqueueToNativeStartMetric   = "gc.reconcile.nudge_shadow.enqueue_to_native_start_ms"
	nudgeParityShadowEventTotalMetric       = "gc.reconcile.nudge_shadow.event.total"
	nudgeParityNativeStartEvidenceAttribute = "t8_native_entry"
)

type nudgeParityInstruments struct {
	comparisonTotal      metric.Int64Counter
	enqueueToPlan        metric.Float64Histogram
	enqueueToNativeStart metric.Float64Histogram
	shadowEventTotal     metric.Int64Counter
}

type nudgeParityInstrumentSnapshot struct {
	instruments nudgeParityInstruments
	err         error
}

var nudgeParityInstrumentState struct {
	mu      sync.Mutex
	current atomic.Pointer[nudgeParityInstrumentSnapshot]
}

// RecordNudgeParityResult records one identity-free comparison and only the
// latencies whose observation evidence exactly proves the reported duration.
// It implements nudgeparity.ResultSink.
func RecordNudgeParityResult(ctx context.Context, result nudgeparity.Result) error {
	if ctx == nil {
		return errors.New("recording nudge parity result: context is nil")
	}
	classification, ok := nudgeParityClassificationLabel(result.Classification)
	if !ok {
		return fmt.Errorf("recording nudge parity result: invalid classification %d", result.Classification)
	}
	reason, ok := nudgeParityReasonLabel(result.Reason)
	if !ok {
		return fmt.Errorf("recording nudge parity result: invalid reason %d", result.Reason)
	}
	if err := validateNudgeParityLatency("expected", result.HasExpected, result.Expected, result.ExpectedLatency); err != nil {
		return err
	}
	if err := validateNudgeParityLatency("actual", result.HasActual, result.Actual, result.ActualLatency); err != nil {
		return err
	}

	snapshot := loadNudgeParityInstruments()
	if snapshot.err != nil {
		return snapshot.err
	}
	comparisonOptions := metric.WithAttributes(
		attribute.String("classification", classification),
		attribute.String("reason", reason),
	)
	snapshot.instruments.comparisonTotal.Add(ctx, 1, comparisonOptions)
	recordNudgeParityLatency(ctx, snapshot.instruments, classification, "expected", result.HasExpected, result.ExpectedLatency)
	recordNudgeParityLatency(ctx, snapshot.instruments, classification, "actual", result.HasActual, result.ActualLatency)
	return nil
}

func validateNudgeParityLatency(side string, hasObservation bool, observation nudgeparity.Observation, latency nudgeparity.Latency) error {
	if !hasObservation {
		if latency.HasEnqueueToPlan || latency.HasEnqueueToNativeStart || latency.EnqueueToPlan != 0 || latency.EnqueueToNativeStart != 0 {
			return fmt.Errorf("recording nudge parity result: %s latency has no observation", side)
		}
		return nil
	}
	wantSide := nudgeparity.SideExpected
	if side == "actual" {
		wantSide = nudgeparity.SideActual
	}
	if observation.Side != wantSide {
		return fmt.Errorf("recording nudge parity result: %s observation has side %s", side, observation.Side)
	}
	if latency.HasEnqueueToPlan {
		if observation.Timing.EnqueuedAt.IsZero() ||
			observation.Timing.PlannedAt.IsZero() ||
			observation.Timing.PlannedAt.Before(observation.Timing.EnqueuedAt) ||
			latency.EnqueueToPlan != observation.Timing.PlannedAt.Sub(observation.Timing.EnqueuedAt) {
			return fmt.Errorf("recording nudge parity result: %s planning latency lacks exact evidence", side)
		}
	} else if latency.EnqueueToPlan != 0 {
		return fmt.Errorf("recording nudge parity result: %s planning latency has no presence marker", side)
	}
	if latency.HasEnqueueToNativeStart {
		if observation.Timing.NativeStartProof != nudgeparity.NativeStartProofT8 ||
			observation.Timing.EnqueuedAt.IsZero() ||
			observation.Timing.NativeStartedAt.IsZero() ||
			observation.Timing.NativeStartedAt.Before(observation.Timing.EnqueuedAt) ||
			latency.EnqueueToNativeStart != observation.Timing.NativeStartedAt.Sub(observation.Timing.EnqueuedAt) {
			return fmt.Errorf("recording nudge parity result: %s native-start latency lacks T8 evidence", side)
		}
	} else if latency.EnqueueToNativeStart != 0 {
		return fmt.Errorf("recording nudge parity result: %s native-start latency has no presence marker", side)
	}
	return nil
}

func recordNudgeParityLatency(
	ctx context.Context,
	instruments nudgeParityInstruments,
	classification string,
	side string,
	hasObservation bool,
	latency nudgeparity.Latency,
) {
	if !hasObservation {
		return
	}
	baseOptions := metric.WithAttributes(
		attribute.String("classification", classification),
		attribute.String("side", side),
	)
	if latency.HasEnqueueToPlan {
		instruments.enqueueToPlan.Record(ctx, durationMilliseconds(latency.EnqueueToPlan), baseOptions)
	}
	if latency.HasEnqueueToNativeStart {
		instruments.enqueueToNativeStart.Record(
			ctx,
			durationMilliseconds(latency.EnqueueToNativeStart),
			metric.WithAttributes(
				attribute.String("classification", classification),
				attribute.String("evidence", nudgeParityNativeStartEvidenceAttribute),
				attribute.String("side", side),
			),
		)
	}
}

func durationMilliseconds(duration time.Duration) float64 {
	return float64(duration) / float64(time.Millisecond)
}

func nudgeParityClassificationLabel(classification nudgeparity.Classification) (string, bool) {
	switch classification {
	case nudgeparity.ClassificationSame,
		nudgeparity.ClassificationDivergent,
		nudgeparity.ClassificationIncomparable,
		nudgeparity.ClassificationMissingExpected,
		nudgeparity.ClassificationMissingActual,
		nudgeparity.ClassificationDuplicate,
		nudgeparity.ClassificationLate:
		return classification.String(), true
	default:
		return "", false
	}
}

func nudgeParityReasonLabel(reason nudgeparity.Reason) (string, bool) {
	switch reason {
	case nudgeparity.ReasonEquivalent,
		nudgeparity.ReasonPlanMismatch,
		nudgeparity.ReasonInputIncomplete,
		nudgeparity.ReasonInputMismatch,
		nudgeparity.ReasonWatermarkIncomplete,
		nudgeparity.ReasonWatermarkMismatch,
		nudgeparity.ReasonExpired,
		nudgeparity.ReasonCapacity,
		nudgeparity.ReasonFlush,
		nudgeparity.ReasonDuplicateIdentical,
		nudgeparity.ReasonDuplicateConflicting,
		nudgeparity.ReasonCounterpartAfterTerminal:
		return reason.String(), true
	default:
		return "", false
	}
}

// NudgeParitySnapshotRecorder converts monotonic, identity-free worker
// snapshots into bounded counter deltas. Create one recorder per Shadow.
type NudgeParitySnapshotRecorder struct {
	mu       sync.Mutex
	previous nudgeparity.ShadowSnapshot
}

// NewNudgeParitySnapshotRecorder creates a recorder whose first Record call
// emits the complete current monotonic counters from a single Shadow.
func NewNudgeParitySnapshotRecorder() *NudgeParitySnapshotRecorder {
	return &NudgeParitySnapshotRecorder{}
}

type nudgeParityEventCounter struct {
	label    string
	current  uint64
	previous uint64
}

// Record validates monotonicity, emits positive deltas, and advances the
// recorder baseline only after instrument creation succeeds.
func (r *NudgeParitySnapshotRecorder) Record(ctx context.Context, current nudgeparity.ShadowSnapshot) error {
	if r == nil {
		return errors.New("recording nudge parity snapshot: recorder is nil")
	}
	if ctx == nil {
		return errors.New("recording nudge parity snapshot: context is nil")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	events := nudgeParityEventCounters(current, r.previous)
	for _, event := range events {
		if event.current < event.previous {
			return fmt.Errorf("recording nudge parity snapshot: %s counter regressed", event.label)
		}
		if event.current-event.previous > ^uint64(0)>>1 {
			return fmt.Errorf("recording nudge parity snapshot: %s counter delta overflows int64", event.label)
		}
	}
	snapshot := loadNudgeParityInstruments()
	if snapshot.err != nil {
		return snapshot.err
	}
	for _, event := range events {
		delta := event.current - event.previous
		if delta == 0 {
			continue
		}
		snapshot.instruments.shadowEventTotal.Add(
			ctx,
			int64(delta),
			metric.WithAttributes(attribute.String("event", event.label)),
		)
	}
	r.previous = current
	return nil
}

func nudgeParityEventCounters(current, previous nudgeparity.ShadowSnapshot) []nudgeParityEventCounter {
	return []nudgeParityEventCounter{
		{label: "accepted", current: current.Accepted, previous: previous.Accepted},
		{label: "not_running", current: current.NotRunning, previous: previous.NotRunning},
		{label: "full", current: current.Full, previous: previous.Full},
		{label: "invalid", current: current.Invalid, previous: previous.Invalid},
		{label: "planner_failure", current: current.PlannerFailures, previous: previous.PlannerFailures},
		{label: "comparator_failure", current: current.ComparatorFailures, previous: previous.ComparatorFailures},
		{label: "sink_failure", current: current.SinkFailures, previous: previous.SinkFailures},
		{label: "emitted", current: current.Emitted, previous: previous.Emitted},
		{label: "shutdown_drained", current: current.ShutdownDrained, previous: previous.ShutdownDrained},
		{label: "unreported", current: current.Unreported, previous: previous.Unreported},
		{
			label:    "expired",
			current:  current.Comparator.ReasonCount(nudgeparity.ReasonExpired),
			previous: previous.Comparator.ReasonCount(nudgeparity.ReasonExpired),
		},
		{
			label:    "capacity_evicted",
			current:  current.Comparator.ReasonCount(nudgeparity.ReasonCapacity),
			previous: previous.Comparator.ReasonCount(nudgeparity.ReasonCapacity),
		},
	}
}

func loadNudgeParityInstruments() *nudgeParityInstrumentSnapshot {
	if current := nudgeParityInstrumentState.current.Load(); current != nil {
		return current
	}
	nudgeParityInstrumentState.mu.Lock()
	defer nudgeParityInstrumentState.mu.Unlock()
	if current := nudgeParityInstrumentState.current.Load(); current != nil {
		return current
	}
	meter := otel.GetMeterProvider().Meter(meterRecorderName)
	comparisonTotal, comparisonErr := meter.Int64Counter(
		nudgeParityComparisonTotalMetric,
		metric.WithDescription("Legacy/keyed nudge plan comparisons by bounded classification and reason"),
	)
	enqueueToPlan, planErr := meter.Float64Histogram(
		nudgeParityEnqueueToPlanMetric,
		metric.WithDescription("Nudge admission to independently evidenced plan completion"),
		metric.WithUnit("ms"),
	)
	enqueueToNativeStart, nativeErr := meter.Float64Histogram(
		nudgeParityEnqueueToNativeStartMetric,
		metric.WithDescription("Nudge admission to explicitly evidenced provider-native T8 entry"),
		metric.WithUnit("ms"),
	)
	shadowEventTotal, eventErr := meter.Int64Counter(
		nudgeParityShadowEventTotalMetric,
		metric.WithDescription("Bounded nudge parity shadow admission, loss, and failure events"),
	)
	snapshot := &nudgeParityInstrumentSnapshot{
		instruments: nudgeParityInstruments{
			comparisonTotal:      comparisonTotal,
			enqueueToPlan:        enqueueToPlan,
			enqueueToNativeStart: enqueueToNativeStart,
			shadowEventTotal:     shadowEventTotal,
		},
		err: errors.Join(
			wrapNudgeParityInstrumentError("comparison counter", comparisonErr),
			wrapNudgeParityInstrumentError("enqueue-to-plan histogram", planErr),
			wrapNudgeParityInstrumentError("enqueue-to-native-start histogram", nativeErr),
			wrapNudgeParityInstrumentError("shadow-event counter", eventErr),
		),
	}
	nudgeParityInstrumentState.current.Store(snapshot)
	return snapshot
}

func wrapNudgeParityInstrumentError(instrument string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("creating nudge parity %s: %w", instrument, err)
}

func resetNudgeParityInstruments() {
	nudgeParityInstrumentState.mu.Lock()
	nudgeParityInstrumentState.current.Store(nil)
	nudgeParityInstrumentState.mu.Unlock()
}

var _ nudgeparity.ResultSink = RecordNudgeParityResult
