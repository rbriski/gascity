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
	nudgeWakeIngressTotalMetric   = "gc.reconcile.nudge_shadow.wake_ingress.total"
)

// NudgeWakeIngressDisposition is the closed, identity-free classification of
// one accepted exact-wake socket connection.
type NudgeWakeIngressDisposition uint8

const (
	// NudgeWakeIngressValid means a complete v1 exact hint was decoded.
	NudgeWakeIngressValid NudgeWakeIngressDisposition = iota + 1
	// NudgeWakeIngressFallback means the connection intentionally carried only
	// a legacy/global wake or exact decoding was unavailable during shutdown.
	NudgeWakeIngressFallback
	// NudgeWakeIngressMalformed means the bounded frame was invalid or oversized.
	NudgeWakeIngressMalformed
	// NudgeWakeIngressSaturated means the global wake was preserved but the
	// bounded exact-reader pool had no admission slot.
	NudgeWakeIngressSaturated
)

// NudgeWakeIngressRecord contains no command, session, city, or payload data.
type NudgeWakeIngressRecord struct {
	Disposition NudgeWakeIngressDisposition
}

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

type nudgeWakeIngressInstrumentSnapshot struct {
	total metric.Int64Counter
	err   error
}

var nudgeWakeIngressInstrumentState struct {
	mu      sync.Mutex
	current atomic.Pointer[nudgeWakeIngressInstrumentSnapshot]
}

// RecordNudgeWakeIngress records exactly one bounded classification for an
// accepted wake connection. Invalid enum values are rejected rather than
// creating an unbounded label vocabulary.
func RecordNudgeWakeIngress(ctx context.Context, record NudgeWakeIngressRecord) error {
	label, ok := nudgeWakeIngressDispositionLabel(record.Disposition)
	if !ok {
		return fmt.Errorf("recording nudge wake ingress: invalid disposition %d", record.Disposition)
	}
	snapshot := loadNudgeWakeIngressInstrument()
	if snapshot.err != nil {
		return snapshot.err
	}
	snapshot.total.Add(ctx, 1, metric.WithAttributes(attribute.String("disposition", label)))
	return nil
}

func nudgeWakeIngressDispositionLabel(disposition NudgeWakeIngressDisposition) (string, bool) {
	switch disposition {
	case NudgeWakeIngressValid:
		return "valid", true
	case NudgeWakeIngressFallback:
		return "fallback", true
	case NudgeWakeIngressMalformed:
		return "malformed", true
	case NudgeWakeIngressSaturated:
		return "saturated", true
	default:
		return "", false
	}
}

func loadNudgeWakeIngressInstrument() *nudgeWakeIngressInstrumentSnapshot {
	if current := nudgeWakeIngressInstrumentState.current.Load(); current != nil {
		return current
	}
	nudgeWakeIngressInstrumentState.mu.Lock()
	defer nudgeWakeIngressInstrumentState.mu.Unlock()
	if current := nudgeWakeIngressInstrumentState.current.Load(); current != nil {
		return current
	}
	meter := otel.GetMeterProvider().Meter(meterRecorderName)
	total, err := meter.Int64Counter(
		nudgeWakeIngressTotalMetric,
		metric.WithDescription("Accepted nudge exact-wake connections by bounded decode disposition"),
	)
	snapshot := &nudgeWakeIngressInstrumentSnapshot{
		total: total,
		err:   wrapNudgeKeySchedulingInstrumentError("wake-ingress counter", err),
	}
	nudgeWakeIngressInstrumentState.current.Store(snapshot)
	return snapshot
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

func resetNudgeWakeIngressInstruments() {
	nudgeWakeIngressInstrumentState.mu.Lock()
	nudgeWakeIngressInstrumentState.current.Store(nil)
	nudgeWakeIngressInstrumentState.mu.Unlock()
}
