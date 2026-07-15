package telemetry

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

const (
	nudgeKeySchedulingTotalMetric = "gc.reconcile.nudge_shadow.scheduling.total"
	nudgeKeyQueueDelayMetric      = "gc.reconcile.nudge_shadow.queue_delay_ms"
)

// NudgeKeyQueueDelayState classifies whether a scheduling delay is measurable.
// Its numeric representation bounds the caller-controlled value space.
type NudgeKeyQueueDelayState uint8

const (
	// NudgeKeyQueueDelayObserved means the source admission timestamp produced a nonnegative delay.
	NudgeKeyQueueDelayObserved NudgeKeyQueueDelayState = iota + 1
	// NudgeKeyQueueDelayUnavailable means no source admission timestamp was available.
	NudgeKeyQueueDelayUnavailable
	// NudgeKeyQueueDelayClockRegressed means the observed wall clock preceded source admission.
	NudgeKeyQueueDelayClockRegressed
)

// NudgeKeySchedulingRecord is the bounded, identity-free metric record emitted
// by the scheduling-only keyed nudge shadow.
type NudgeKeySchedulingRecord struct {
	CauseBits       uint8
	WorkqueueReplay bool
	QueueDelay      time.Duration
	QueueDelayState NudgeKeyQueueDelayState
}

type nudgeKeySchedulingInstruments struct {
	total      metric.Int64Counter
	queueDelay metric.Float64Histogram
}

type nudgeKeySchedulingInstrumentSnapshot struct {
	instruments nudgeKeySchedulingInstruments
	err         error
}

var nudgeKeySchedulingInstrumentState struct {
	mu      sync.Mutex
	current atomic.Pointer[nudgeKeySchedulingInstrumentSnapshot]
}

// RecordNudgeKeyScheduling records one bounded scheduling-only keyed nudge
// shadow callback. The record type cannot carry session, command, alias, or
// message identity.
func RecordNudgeKeyScheduling(ctx context.Context, record NudgeKeySchedulingRecord) error {
	snapshot := loadNudgeKeySchedulingInstruments()
	if snapshot.err != nil {
		return snapshot.err
	}
	attrs := []attribute.KeyValue{
		attribute.Int64("cause_bits", int64(record.CauseBits)),
		attribute.Bool("workqueue_replay", record.WorkqueueReplay),
		attribute.String("queue_delay_state", nudgeKeyQueueDelayStateLabel(record.QueueDelayState)),
		attribute.String("scope_certification", "provisional"),
		attribute.String("authorization", "not_evaluated"),
		attribute.Bool("effects_admissible", false),
	}
	options := metric.WithAttributes(attrs...)
	snapshot.instruments.total.Add(ctx, 1, options)
	if record.QueueDelayState == NudgeKeyQueueDelayObserved {
		snapshot.instruments.queueDelay.Record(ctx, float64(record.QueueDelay)/float64(time.Millisecond), options)
	}
	return nil
}

func nudgeKeyQueueDelayStateLabel(state NudgeKeyQueueDelayState) string {
	switch state {
	case NudgeKeyQueueDelayObserved:
		return "observed"
	case NudgeKeyQueueDelayUnavailable:
		return "unavailable"
	case NudgeKeyQueueDelayClockRegressed:
		return "clock_regressed"
	default:
		return "invalid"
	}
}

func loadNudgeKeySchedulingInstruments() *nudgeKeySchedulingInstrumentSnapshot {
	if current := nudgeKeySchedulingInstrumentState.current.Load(); current != nil {
		return current
	}
	nudgeKeySchedulingInstrumentState.mu.Lock()
	defer nudgeKeySchedulingInstrumentState.mu.Unlock()
	if current := nudgeKeySchedulingInstrumentState.current.Load(); current != nil {
		return current
	}

	meter := otel.GetMeterProvider().Meter(meterRecorderName)
	total, totalErr := meter.Int64Counter(
		nudgeKeySchedulingTotalMetric,
		metric.WithDescription("Total scheduling-only keyed nudge shadow callbacks"),
	)
	queueDelay, queueDelayErr := meter.Float64Histogram(
		nudgeKeyQueueDelayMetric,
		metric.WithDescription("Delay from first keyed nudge admission to its scheduling-only shadow callback"),
		metric.WithUnit("ms"),
	)
	snapshot := &nudgeKeySchedulingInstrumentSnapshot{
		instruments: nudgeKeySchedulingInstruments{
			total:      total,
			queueDelay: queueDelay,
		},
		err: errors.Join(
			wrapNudgeKeySchedulingInstrumentError("counter", totalErr),
			wrapNudgeKeySchedulingInstrumentError("queue-delay histogram", queueDelayErr),
		),
	}
	nudgeKeySchedulingInstrumentState.current.Store(snapshot)
	return snapshot
}

func wrapNudgeKeySchedulingInstrumentError(instrument string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("creating keyed nudge scheduling %s: %w", instrument, err)
}

func resetNudgeKeySchedulingInstruments() {
	nudgeKeySchedulingInstrumentState.mu.Lock()
	nudgeKeySchedulingInstrumentState.current.Store(nil)
	nudgeKeySchedulingInstrumentState.mu.Unlock()
}
