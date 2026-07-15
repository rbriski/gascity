package telemetry

import (
	"context"
	"errors"
	"fmt"
	"slices"
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
	nudgeKeyBacklogDepthMetric    = "gc.reconcile.nudge_shadow.backlog.keys"
	nudgeKeyBacklogAgeMetric      = "gc.reconcile.nudge_shadow.backlog.oldest_age_ms"
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

// NudgeKeyBacklogAgeState classifies whether the oldest dirty-key age is exact.
type NudgeKeyBacklogAgeState uint8

const (
	// NudgeKeyBacklogAgeEmpty means no dirty keys are waiting or deferred.
	NudgeKeyBacklogAgeEmpty NudgeKeyBacklogAgeState = iota + 1
	// NudgeKeyBacklogAgeObserved means OldestAge is an exact nonnegative age.
	NudgeKeyBacklogAgeObserved
	// NudgeKeyBacklogAgeUnavailable means at least one dirty replay lacks an
	// authoritative admission timestamp, so no oldest age may be invented.
	NudgeKeyBacklogAgeUnavailable
	// NudgeKeyBacklogAgeClockRegressed means the observation clock preceded an
	// admitted key timestamp.
	NudgeKeyBacklogAgeClockRegressed
)

// NudgeKeyBacklogRecord is one identity-free scheduler-owned snapshot.
type NudgeKeyBacklogRecord struct {
	Depth     int64
	OldestAge time.Duration
	AgeState  NudgeKeyBacklogAgeState
}

// NudgeKeyBacklogObserver returns one instantaneous scheduler snapshot. It is
// invoked only during metric collection and must be safe for concurrent use.
type NudgeKeyBacklogObserver func() NudgeKeyBacklogRecord

// NudgeKeyBacklogUnregister removes one observer. It is idempotent and safe for
// concurrent use.
type NudgeKeyBacklogUnregister func() error

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

type nudgeKeyBacklogInstrumentSnapshot struct {
	depth        metric.Int64ObservableGauge
	oldestAge    metric.Float64ObservableGauge
	registration metric.Registration
	err          error
}

var nudgeKeyBacklogInstrumentState struct {
	mu        sync.Mutex
	current   *nudgeKeyBacklogInstrumentSnapshot
	nextID    uint64
	observers map[uint64]NudgeKeyBacklogObserver
}

// RegisterNudgeKeyBacklogObserver adds one scheduler-owned source to the
// process aggregate. Collection sums depth and reports the maximum exact age;
// no city, store, command, or session identity becomes a metric label.
func RegisterNudgeKeyBacklogObserver(observer NudgeKeyBacklogObserver) (NudgeKeyBacklogUnregister, error) {
	if observer == nil {
		return nil, errors.New("registering keyed nudge backlog observer: observer is nil")
	}
	nudgeKeyBacklogInstrumentState.mu.Lock()
	snapshot := loadNudgeKeyBacklogInstrumentsLocked()
	if snapshot.err != nil {
		nudgeKeyBacklogInstrumentState.mu.Unlock()
		return nil, snapshot.err
	}
	if nudgeKeyBacklogInstrumentState.nextID == ^uint64(0) {
		nudgeKeyBacklogInstrumentState.mu.Unlock()
		return nil, errors.New("registering keyed nudge backlog observer: observer id space exhausted")
	}
	nudgeKeyBacklogInstrumentState.nextID++
	id := nudgeKeyBacklogInstrumentState.nextID
	if nudgeKeyBacklogInstrumentState.observers == nil {
		nudgeKeyBacklogInstrumentState.observers = make(map[uint64]NudgeKeyBacklogObserver)
	}
	nudgeKeyBacklogInstrumentState.observers[id] = observer
	nudgeKeyBacklogInstrumentState.mu.Unlock()

	var once sync.Once
	return func() error {
		once.Do(func() {
			nudgeKeyBacklogInstrumentState.mu.Lock()
			delete(nudgeKeyBacklogInstrumentState.observers, id)
			nudgeKeyBacklogInstrumentState.mu.Unlock()
		})
		return nil
	}, nil
}

func loadNudgeKeyBacklogInstrumentsLocked() *nudgeKeyBacklogInstrumentSnapshot {
	if nudgeKeyBacklogInstrumentState.current != nil {
		return nudgeKeyBacklogInstrumentState.current
	}
	meter := otel.GetMeterProvider().Meter(meterRecorderName)
	depth, depthErr := meter.Int64ObservableGauge(
		nudgeKeyBacklogDepthMetric,
		metric.WithDescription("Dirty keyed nudge sessions waiting or deferred across active city controllers"),
	)
	oldestAge, ageErr := meter.Float64ObservableGauge(
		nudgeKeyBacklogAgeMetric,
		metric.WithDescription("Oldest scheduler-owned keyed nudge admission age across active city controllers"),
		metric.WithUnit("ms"),
	)
	snapshot := &nudgeKeyBacklogInstrumentSnapshot{
		depth:     depth,
		oldestAge: oldestAge,
		err: errors.Join(
			wrapNudgeKeySchedulingInstrumentError("backlog-depth gauge", depthErr),
			wrapNudgeKeySchedulingInstrumentError("backlog-age gauge", ageErr),
		),
	}
	if snapshot.err == nil {
		snapshot.registration, snapshot.err = meter.RegisterCallback(
			observeNudgeKeyBacklog,
			depth,
			oldestAge,
		)
		if snapshot.err != nil {
			snapshot.err = fmt.Errorf("registering keyed nudge backlog metric callback: %w", snapshot.err)
		}
	}
	nudgeKeyBacklogInstrumentState.current = snapshot
	return snapshot
}

func observeNudgeKeyBacklog(_ context.Context, observer metric.Observer) error {
	record, err := aggregateNudgeKeyBacklog()
	nudgeKeyBacklogInstrumentState.mu.Lock()
	snapshot := nudgeKeyBacklogInstrumentState.current
	nudgeKeyBacklogInstrumentState.mu.Unlock()
	if snapshot == nil || snapshot.err != nil {
		return errors.Join(err, errors.New("observing keyed nudge backlog: instruments are unavailable"))
	}
	observer.ObserveInt64(snapshot.depth, record.Depth)
	observer.ObserveFloat64(
		snapshot.oldestAge,
		float64(record.OldestAge)/float64(time.Millisecond),
		metric.WithAttributes(attribute.String("state", nudgeKeyBacklogAgeStateLabel(record.AgeState))),
	)
	return err
}

func aggregateNudgeKeyBacklog() (NudgeKeyBacklogRecord, error) {
	nudgeKeyBacklogInstrumentState.mu.Lock()
	ids := make([]uint64, 0, len(nudgeKeyBacklogInstrumentState.observers))
	for id := range nudgeKeyBacklogInstrumentState.observers {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	observers := make([]NudgeKeyBacklogObserver, 0, len(ids))
	for _, id := range ids {
		observers = append(observers, nudgeKeyBacklogInstrumentState.observers[id])
	}
	nudgeKeyBacklogInstrumentState.mu.Unlock()

	aggregate := NudgeKeyBacklogRecord{AgeState: NudgeKeyBacklogAgeEmpty}
	var failures []error
	hasUnavailable := false
	hasClockRegression := false
	for _, observer := range observers {
		record, err := invokeNudgeKeyBacklogObserver(observer)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if err := validateNudgeKeyBacklogRecord(record); err != nil {
			failures = append(failures, err)
			continue
		}
		if record.Depth > int64(^uint64(0)>>1)-aggregate.Depth {
			failures = append(failures, errors.New("aggregating keyed nudge backlog: depth overflow"))
			continue
		}
		aggregate.Depth += record.Depth
		if record.Depth == 0 {
			continue
		}
		switch record.AgeState {
		case NudgeKeyBacklogAgeObserved:
			if record.OldestAge > aggregate.OldestAge {
				aggregate.OldestAge = record.OldestAge
			}
		case NudgeKeyBacklogAgeUnavailable:
			hasUnavailable = true
		case NudgeKeyBacklogAgeClockRegressed:
			hasClockRegression = true
		}
	}
	switch {
	case aggregate.Depth == 0:
		aggregate.OldestAge = 0
		aggregate.AgeState = NudgeKeyBacklogAgeEmpty
	case hasUnavailable:
		aggregate.OldestAge = 0
		aggregate.AgeState = NudgeKeyBacklogAgeUnavailable
	case hasClockRegression:
		aggregate.OldestAge = 0
		aggregate.AgeState = NudgeKeyBacklogAgeClockRegressed
	default:
		aggregate.AgeState = NudgeKeyBacklogAgeObserved
	}
	return aggregate, errors.Join(failures...)
}

func invokeNudgeKeyBacklogObserver(observer NudgeKeyBacklogObserver) (record NudgeKeyBacklogRecord, err error) {
	defer func() {
		if recover() != nil {
			record = NudgeKeyBacklogRecord{}
			err = errors.New("keyed nudge backlog observer failed")
		}
	}()
	return observer(), nil
}

func validateNudgeKeyBacklogRecord(record NudgeKeyBacklogRecord) error {
	if record.Depth < 0 {
		return errors.New("keyed nudge backlog observer returned negative depth")
	}
	if record.Depth == 0 {
		if record.OldestAge != 0 || record.AgeState != NudgeKeyBacklogAgeEmpty {
			return errors.New("keyed nudge backlog observer returned inconsistent empty state")
		}
		return nil
	}
	switch record.AgeState {
	case NudgeKeyBacklogAgeObserved:
		if record.OldestAge < 0 {
			return errors.New("keyed nudge backlog observer returned negative age")
		}
	case NudgeKeyBacklogAgeUnavailable, NudgeKeyBacklogAgeClockRegressed:
		if record.OldestAge != 0 {
			return errors.New("keyed nudge backlog observer returned unprovable age")
		}
	default:
		return errors.New("keyed nudge backlog observer returned invalid age state")
	}
	return nil
}

func nudgeKeyBacklogAgeStateLabel(state NudgeKeyBacklogAgeState) string {
	switch state {
	case NudgeKeyBacklogAgeEmpty:
		return "empty"
	case NudgeKeyBacklogAgeObserved:
		return "observed"
	case NudgeKeyBacklogAgeUnavailable:
		return "unavailable"
	case NudgeKeyBacklogAgeClockRegressed:
		return "clock_regressed"
	default:
		return "invalid"
	}
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

func resetNudgeKeyBacklogInstruments() {
	nudgeKeyBacklogInstrumentState.mu.Lock()
	snapshot := nudgeKeyBacklogInstrumentState.current
	nudgeKeyBacklogInstrumentState.current = nil
	nudgeKeyBacklogInstrumentState.nextID = 0
	nudgeKeyBacklogInstrumentState.observers = nil
	nudgeKeyBacklogInstrumentState.mu.Unlock()
	if snapshot != nil && snapshot.registration != nil {
		_ = snapshot.registration.Unregister()
	}
}
