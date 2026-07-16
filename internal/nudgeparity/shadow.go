package nudgeparity

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"
)

// Sample is one immutable capture from the authoritative legacy planner. The
// shadow planner receives the same input and watermarks, but never ExpectedPlan.
type Sample struct {
	OperationID    string
	Input          Input
	Watermarks     Watermarks
	ExpectedPlan   Plan
	CapturedAt     time.Time
	ExpectedTiming TimingEvidence
}

func (s Sample) expectedObservation() Observation {
	return Observation{
		OperationID: s.OperationID,
		Side:        SideExpected,
		Input:       s.Input,
		Watermarks:  s.Watermarks,
		Plan:        s.ExpectedPlan,
		CapturedAt:  s.CapturedAt,
		Timing:      s.ExpectedTiming,
	}
}

// PlanningInput is the exact captured view passed to the independent keyed
// planner. It deliberately cannot carry the legacy planner's expected output.
type PlanningInput struct {
	OperationID string
	Input       Input
	Watermarks  Watermarks
	CapturedAt  time.Time
	EnqueuedAt  time.Time
}

// Planned is the independently computed keyed output. A planning-only shadow
// cannot claim provider-native T8 entry, so it can report only plan evidence.
type Planned struct {
	Plan      Plan
	PlannedAt time.Time
}

// Planner computes one keyed plan without executing it. Implementations must
// be pure with respect to worker, runtime-provider, durable-command, and tmux
// effects, and must honor context cancellation.
type Planner interface {
	Plan(context.Context, PlanningInput) (Planned, error)
}

// PlannerFunc adapts a pure function to Planner.
type PlannerFunc func(context.Context, PlanningInput) (Planned, error)

// Plan invokes f or returns an error when the function is nil.
func (f PlannerFunc) Plan(ctx context.Context, input PlanningInput) (Planned, error) {
	if f == nil {
		return Planned{}, errors.New("planning keyed nudge shadow: planner function is nil")
	}
	return f(ctx, input)
}

// ResultSink receives comparison results on the shadow worker, never on the
// authoritative producer. A slow sink may shed future samples by filling the
// bounded queue, but can never delay the legacy effect path.
type ResultSink func(context.Context, Result) error

// Submission is the bounded outcome of one nonblocking shadow admission.
type Submission uint8

const (
	// SubmissionUnknown is the invalid zero value.
	SubmissionUnknown Submission = iota
	// SubmissionAccepted means the immutable sample entered the bounded queue.
	SubmissionAccepted
	// SubmissionNotRunning means shadow admission was closed.
	SubmissionNotRunning
	// SubmissionFull means the bounded queue had no immediate capacity.
	SubmissionFull
	// SubmissionInvalid means the sample was not safe to retain.
	SubmissionInvalid
)

// String returns the bounded telemetry spelling for a submission.
func (s Submission) String() string {
	switch s {
	case SubmissionAccepted:
		return "accepted"
	case SubmissionNotRunning:
		return "not_running"
	case SubmissionFull:
		return "full"
	case SubmissionInvalid:
		return "invalid"
	default:
		return "unknown"
	}
}

// ShadowState is the process-local lifecycle of one shadow worker.
type ShadowState uint8

const (
	// ShadowStateNew has not opened producer admission.
	ShadowStateNew ShadowState = iota
	// ShadowStateRunning accepts bounded samples.
	ShadowStateRunning
	// ShadowStateStopping has closed admission and is draining accepted samples.
	ShadowStateStopping
	// ShadowStateStopped is terminal; a Shadow is single-start.
	ShadowStateStopped
)

// String returns the bounded telemetry spelling for a shadow state.
func (s ShadowState) String() string {
	switch s {
	case ShadowStateNew:
		return "new"
	case ShadowStateRunning:
		return "running"
	case ShadowStateStopping:
		return "stopping"
	case ShadowStateStopped:
		return "stopped"
	default:
		return "unknown"
	}
}

// ShadowConfig configures a bounded single-worker same-input comparison lane.
type ShadowConfig struct {
	Comparator    Config
	QueueCapacity int
	Planner       Planner
	Sink          ResultSink
	// Sweep is an optional externally clocked signal. A nil channel disables
	// idle sweeping; arrival and shutdown still reconcile comparator expiry.
	Sweep <-chan struct{}
}

// ShadowSnapshot contains fixed-cardinality worker counters and bounded state.
type ShadowSnapshot struct {
	State              ShadowState
	QueueDepth         int
	Accepted           uint64
	NotRunning         uint64
	Full               uint64
	Invalid            uint64
	PlannerFailures    uint64
	ComparatorFailures uint64
	SinkFailures       uint64
	Emitted            uint64
	ShutdownDrained    uint64
	Unreported         uint64
	Comparator         Snapshot
}

// Shadow is a bounded, nonblocking producer tap and single background planner.
// It has no API for executing provider effects or acknowledging commands.
type Shadow struct {
	comparator *Comparator
	planner    Planner
	sink       ResultSink
	now        func() time.Time
	sweep      <-chan struct{}
	queue      chan Sample
	ready      chan struct{}

	state         atomic.Uint32
	inFlight      atomic.Int64
	admissionIdle chan struct{}

	accepted           atomic.Uint64
	notRunning         atomic.Uint64
	full               atomic.Uint64
	invalid            atomic.Uint64
	plannerFailures    atomic.Uint64
	comparatorFailures atomic.Uint64
	sinkFailures       atomic.Uint64
	emitted            atomic.Uint64
	shutdownDrained    atomic.Uint64
	unreported         atomic.Uint64
}

// NewShadow constructs a stopped same-input shadow with explicit queue and
// comparator bounds. Run must be called exactly once to open admission.
func NewShadow(config ShadowConfig) (*Shadow, error) {
	if config.QueueCapacity <= 0 {
		return nil, errors.New("creating nudge parity shadow: queue capacity must be positive")
	}
	if config.Planner == nil {
		return nil, errors.New("creating nudge parity shadow: planner is nil")
	}
	now := config.Comparator.Now
	if now == nil {
		now = time.Now
		config.Comparator.Now = now
	}
	comparator, err := New(config.Comparator)
	if err != nil {
		return nil, fmt.Errorf("creating nudge parity shadow: %w", err)
	}
	return &Shadow{
		comparator:    comparator,
		planner:       config.Planner,
		sink:          config.Sink,
		now:           now,
		sweep:         config.Sweep,
		queue:         make(chan Sample, config.QueueCapacity),
		ready:         make(chan struct{}),
		admissionIdle: make(chan struct{}, 1),
	}, nil
}

// Ready closes after Run has atomically opened producer admission.
func (s *Shadow) Ready() <-chan struct{} {
	if s == nil {
		return nil
	}
	return s.ready
}

// Submit validates and attempts to enqueue a sample without waiting for queue
// capacity, planning, comparison, telemetry, or any provider effect.
func (s *Shadow) Submit(sample Sample) (Submission, error) {
	if s == nil {
		return SubmissionInvalid, errors.New("submitting nudge parity shadow: shadow is nil")
	}
	if ShadowState(s.state.Load()) != ShadowStateRunning {
		s.notRunning.Add(1)
		return SubmissionNotRunning, nil
	}

	s.inFlight.Add(1)
	defer s.finishAdmission()
	if ShadowState(s.state.Load()) != ShadowStateRunning {
		s.notRunning.Add(1)
		return SubmissionNotRunning, nil
	}
	now := s.now()
	if now.IsZero() {
		s.invalid.Add(1)
		return SubmissionInvalid, ErrInvalidClock
	}
	if err := sample.expectedObservation().validate(now); err != nil {
		s.invalid.Add(1)
		return SubmissionInvalid, err
	}
	select {
	case s.queue <- sample:
		s.accepted.Add(1)
		return SubmissionAccepted, nil
	default:
		s.full.Add(1)
		return SubmissionFull, nil
	}
}

func (s *Shadow) finishAdmission() {
	if s.inFlight.Add(-1) != 0 {
		return
	}
	select {
	case s.admissionIdle <- struct{}{}:
	default:
	}
}

// Run opens admission, plans queued samples serially, and drains accepted work
// into explicit loss results on cancellation or failure. A Shadow is single-start.
func (s *Shadow) Run(ctx context.Context) error {
	if s == nil {
		return errors.New("running nudge parity shadow: shadow is nil")
	}
	if ctx == nil {
		return errors.New("running nudge parity shadow: context is nil")
	}
	if !s.state.CompareAndSwap(uint32(ShadowStateNew), uint32(ShadowStateRunning)) {
		return errors.New("running nudge parity shadow: shadow is single-start")
	}
	close(s.ready)

	var runErr error
runLoop:
	for {
		select {
		case <-ctx.Done():
			break runLoop
		case <-s.sweep:
			results, err := s.comparator.Sweep()
			if err != nil {
				s.comparatorFailures.Add(1)
				runErr = fmt.Errorf("sweeping nudge parity shadow: %w", err)
				break runLoop
			}
			if err := s.emitResults(ctx, results); err != nil {
				runErr = err
				break runLoop
			}
		case sample := <-s.queue:
			if err := s.process(ctx, sample); err != nil {
				if ctx.Err() != nil && errors.Is(err, ctx.Err()) {
					break runLoop
				}
				runErr = err
				break runLoop
			}
		}
	}

	shutdownErr := s.shutdown(context.WithoutCancel(ctx))
	return errors.Join(runErr, shutdownErr)
}

func (s *Shadow) process(ctx context.Context, sample Sample) error {
	expected := sample.expectedObservation()
	results, err := s.comparator.Observe(expected)
	if err != nil {
		s.comparatorFailures.Add(1)
		return fmt.Errorf("recording expected nudge parity observation: %w", err)
	}
	if err := s.emitResults(ctx, results); err != nil {
		return err
	}

	planned, err := s.planner.Plan(ctx, PlanningInput{
		OperationID: sample.OperationID,
		Input:       sample.Input,
		Watermarks:  sample.Watermarks,
		CapturedAt:  sample.CapturedAt,
		EnqueuedAt:  sample.ExpectedTiming.EnqueuedAt,
	})
	if err != nil {
		if ctx.Err() == nil || !errors.Is(err, ctx.Err()) {
			s.plannerFailures.Add(1)
		}
		return fmt.Errorf("planning keyed nudge shadow: %w", err)
	}

	now := s.now()
	actual := Observation{
		OperationID: sample.OperationID,
		Side:        SideActual,
		Input:       sample.Input,
		Watermarks:  sample.Watermarks,
		Plan:        planned.Plan,
		CapturedAt:  now,
		Timing: TimingEvidence{
			EnqueuedAt: sample.ExpectedTiming.EnqueuedAt,
			PlannedAt:  planned.PlannedAt,
		},
	}
	results, err = s.comparator.Observe(actual)
	if err != nil {
		s.comparatorFailures.Add(1)
		return fmt.Errorf("recording actual nudge parity observation: %w", err)
	}
	return s.emitResults(ctx, results)
}

func (s *Shadow) shutdown(ctx context.Context) error {
	s.state.Store(uint32(ShadowStateStopping))
	s.waitForAdmissions()
	var failures []error

	results, err := s.comparator.Sweep()
	if err != nil {
		s.comparatorFailures.Add(1)
		failures = append(failures, fmt.Errorf("sweeping nudge parity shadow during shutdown: %w", err))
	} else if err := s.emitResults(ctx, results); err != nil {
		failures = append(failures, err)
	}

	for {
		select {
		case sample := <-s.queue:
			s.shutdownDrained.Add(1)
			results, err := s.comparator.Observe(sample.expectedObservation())
			if err != nil {
				s.comparatorFailures.Add(1)
				failures = append(failures, fmt.Errorf("draining nudge parity shadow: %w", err))
				continue
			}
			if err := s.emitResults(ctx, results); err != nil {
				failures = append(failures, err)
			}
		default:
			results := s.comparator.Flush()
			if err := s.emitResults(ctx, results); err != nil {
				failures = append(failures, err)
			}
			s.state.Store(uint32(ShadowStateStopped))
			return errors.Join(failures...)
		}
	}
}

func (s *Shadow) waitForAdmissions() {
	for s.inFlight.Load() != 0 {
		<-s.admissionIdle
	}
}

func (s *Shadow) emitResults(ctx context.Context, results []Result) error {
	for index, result := range results {
		if s.sink != nil {
			if err := invokeResultSink(ctx, s.sink, result); err != nil {
				s.sinkFailures.Add(1)
				s.unreported.Add(uint64(len(results) - index))
				return err
			}
		}
		s.emitted.Add(1)
	}
	return nil
}

func invokeResultSink(ctx context.Context, sink ResultSink, result Result) (err error) {
	defer func() {
		if recover() != nil {
			err = errors.New("recording nudge parity result: sink panicked")
		}
	}()
	if err := sink(ctx, result); err != nil {
		return fmt.Errorf("recording nudge parity result: %w", err)
	}
	return nil
}

// Snapshot returns current worker counters, queue depth, and comparator state.
func (s *Shadow) Snapshot() ShadowSnapshot {
	if s == nil {
		return ShadowSnapshot{}
	}
	return ShadowSnapshot{
		State:              ShadowState(s.state.Load()),
		QueueDepth:         len(s.queue),
		Accepted:           s.accepted.Load(),
		NotRunning:         s.notRunning.Load(),
		Full:               s.full.Load(),
		Invalid:            s.invalid.Load(),
		PlannerFailures:    s.plannerFailures.Load(),
		ComparatorFailures: s.comparatorFailures.Load(),
		SinkFailures:       s.sinkFailures.Load(),
		Emitted:            s.emitted.Load(),
		ShutdownDrained:    s.shutdownDrained.Load(),
		Unreported:         s.unreported.Load(),
		Comparator:         s.comparator.Snapshot(),
	}
}
