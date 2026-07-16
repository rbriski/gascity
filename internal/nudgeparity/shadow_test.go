package nudgeparity

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestShadowUsesOneCapturedInputAndLegacyRemainsSoleEffectOwner(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	plannerInputs := make(chan PlanningInput, 1)
	results := make(chan Result, 1)
	shadow := newTestShadow(t, now, 4, PlannerFunc(func(_ context.Context, input PlanningInput) (Planned, error) {
		plannerInputs <- input
		return Planned{Plan: Plan{Decision: DecisionExecute, Action: ActionNudge}}, nil
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	})
	ctx, cancel, runErr := runTestShadow(t, shadow)
	defer cancel()

	sample := testSample("operation-1", now)
	disposition, err := shadow.Submit(sample)
	if err != nil || disposition != SubmissionAccepted {
		t.Fatalf("Submit() = %s, %v; want accepted", disposition, err)
	}
	var legacyEffects atomic.Int64
	legacyEffects.Add(1) // The authoritative path acts after the tap returns.

	plannerInput := <-plannerInputs
	if plannerInput.OperationID != sample.OperationID || plannerInput.Input != sample.Input || plannerInput.Watermarks != sample.Watermarks {
		t.Fatalf("planner input = %#v, want exact captured identity and watermarks", plannerInput)
	}
	result := <-results
	if result.Classification != ClassificationSame || result.Reason != ReasonEquivalent {
		t.Fatalf("comparison = %s/%s, want same/equivalent", result.Classification, result.Reason)
	}
	if result.Expected.Input != result.Actual.Input || result.Expected.Watermarks != result.Actual.Watermarks {
		t.Fatalf("comparison did not retain one captured view: %#v", result)
	}
	if legacyEffects.Load() != 1 {
		t.Fatalf("legacy effects = %d, want exactly one", legacyEffects.Load())
	}

	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() shutdown: %v", err)
	}
	if ctx.Err() == nil {
		t.Fatal("test context was not canceled")
	}
}

func TestShadowSubmissionIsNonblockingAndStrictlyBounded(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	plannerEntered := make(chan struct{}, 1)
	releasePlanner := make(chan struct{})
	results := make(chan Result, 2)
	shadow := newTestShadow(t, now, 1, PlannerFunc(func(_ context.Context, _ PlanningInput) (Planned, error) {
		plannerEntered <- struct{}{}
		<-releasePlanner
		return Planned{Plan: Plan{Decision: DecisionExecute, Action: ActionNudge}}, nil
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	})
	_, cancel, runErr := runTestShadow(t, shadow)
	defer cancel()

	if disposition, err := shadow.Submit(testSample("operation-1", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("first Submit() = %s, %v", disposition, err)
	}
	<-plannerEntered
	if disposition, err := shadow.Submit(testSample("operation-2", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("queued Submit() = %s, %v", disposition, err)
	}
	if disposition, err := shadow.Submit(testSample("operation-3", now)); err != nil || disposition != SubmissionFull {
		t.Fatalf("full Submit() = %s, %v; want full without error", disposition, err)
	}
	snapshot := shadow.Snapshot()
	if snapshot.Accepted != 2 || snapshot.Full != 1 || snapshot.QueueDepth != 1 {
		t.Fatalf("bounded snapshot = %#v, want accepted=2 full=1 depth=1", snapshot)
	}

	close(releasePlanner)
	for index := 0; index < 2; index++ {
		if result := <-results; result.Classification != ClassificationSame {
			t.Fatalf("result %d = %s, want same", index, result.Classification)
		}
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() shutdown: %v", err)
	}
}

func TestShadowShutdownReportsEveryAcceptedButUnplannedSample(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	plannerEntered := make(chan struct{})
	results := make(chan Result, 2)
	shadow := newTestShadow(t, now, 1, PlannerFunc(func(ctx context.Context, _ PlanningInput) (Planned, error) {
		close(plannerEntered)
		<-ctx.Done()
		return Planned{}, ctx.Err()
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	})
	_, cancel, runErr := runTestShadow(t, shadow)

	if disposition, err := shadow.Submit(testSample("operation-1", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("first Submit() = %s, %v", disposition, err)
	}
	<-plannerEntered
	if disposition, err := shadow.Submit(testSample("operation-2", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("second Submit() = %s, %v", disposition, err)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() canceled shutdown: %v", err)
	}

	for _, operationID := range []string{"operation-1", "operation-2"} {
		result := <-results
		if result.OperationID != operationID || result.Classification != ClassificationMissingActual || result.Reason != ReasonFlush {
			t.Fatalf("shutdown result = %#v, want %s missing_actual/flush", result, operationID)
		}
	}
	snapshot := shadow.Snapshot()
	if snapshot.Accepted != 2 || snapshot.ShutdownDrained != 1 || snapshot.Comparator.Pending != 0 {
		t.Fatalf("shutdown snapshot = %#v", snapshot)
	}
}

func TestShadowPlannerFailureIsSurfacedAndPendingExpectedIsFlushed(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	plannerFailure := errors.New("planner unavailable")
	results := make(chan Result, 1)
	shadow := newTestShadow(t, now, 1, PlannerFunc(func(context.Context, PlanningInput) (Planned, error) {
		return Planned{}, plannerFailure
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	})
	_, cancel, runErr := runTestShadow(t, shadow)
	defer cancel()
	if disposition, err := shadow.Submit(testSample("operation-1", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("Submit() = %s, %v", disposition, err)
	}

	err := <-runErr
	if !errors.Is(err, plannerFailure) {
		t.Fatalf("Run() error = %v, want planner failure", err)
	}
	result := <-results
	if result.Classification != ClassificationMissingActual || result.Reason != ReasonFlush {
		t.Fatalf("planner-failure flush = %s/%s", result.Classification, result.Reason)
	}
	if snapshot := shadow.Snapshot(); snapshot.PlannerFailures != 1 {
		t.Fatalf("planner failures = %d, want 1", snapshot.PlannerFailures)
	}
}

func TestShadowSweepUsesComparatorRetentionWithoutPolling(t *testing.T) {
	started := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	now := started
	sweep := make(chan struct{})
	plannerRelease := make(chan struct{})
	plannerEntered := make(chan struct{})
	results := make(chan Result, 1)
	shadow := newTestShadowWithClock(t, func() time.Time { return now }, 1, PlannerFunc(func(ctx context.Context, _ PlanningInput) (Planned, error) {
		close(plannerEntered)
		select {
		case <-plannerRelease:
			return Planned{}, errors.New("unexpected release")
		case <-ctx.Done():
			return Planned{}, ctx.Err()
		}
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	}, sweep)
	_, cancel, runErr := runTestShadow(t, shadow)
	if disposition, err := shadow.Submit(testSample("operation-1", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("Submit() = %s, %v", disposition, err)
	}
	<-plannerEntered

	// The blocked planner prevents the run loop from sweeping; cancellation is
	// the deterministic barrier that reports the accepted sample instead.
	now = started.Add(testRetention)
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() canceled shutdown: %v", err)
	}
	result := <-results
	if result.Classification != ClassificationMissingActual || result.Reason != ReasonExpired {
		t.Fatalf("retention result = %s/%s, want missing_actual/expired", result.Classification, result.Reason)
	}
}

func TestShadowConcurrentSubmissionNeverExceedsQueueBound(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	const capacity = 8
	plannerEntered := make(chan struct{})
	releasePlanner := make(chan struct{})
	results := make(chan Result, capacity+1)
	shadow := newTestShadow(t, now, capacity, PlannerFunc(func(_ context.Context, _ PlanningInput) (Planned, error) {
		select {
		case <-plannerEntered:
		default:
			close(plannerEntered)
		}
		<-releasePlanner
		return Planned{Plan: Plan{Decision: DecisionExecute, Action: ActionNudge}}, nil
	}), func(_ context.Context, result Result) error {
		results <- result
		return nil
	})
	_, cancel, runErr := runTestShadow(t, shadow)
	defer cancel()
	if disposition, err := shadow.Submit(testSample("operation-anchor", now)); err != nil || disposition != SubmissionAccepted {
		t.Fatalf("anchor Submit() = %s, %v", disposition, err)
	}
	<-plannerEntered

	const attempts = 128
	var accepted atomic.Int64
	var full atomic.Int64
	var submitErrors atomic.Int64
	var workers sync.WaitGroup
	for index := 0; index < attempts; index++ {
		workers.Add(1)
		go func(index int) {
			defer workers.Done()
			disposition, err := shadow.Submit(testSample(fmt.Sprintf("operation-%03d", index), now))
			if err != nil {
				submitErrors.Add(1)
				return
			}
			switch disposition {
			case SubmissionAccepted:
				accepted.Add(1)
			case SubmissionFull:
				full.Add(1)
			default:
				submitErrors.Add(1)
			}
		}(index)
	}
	workers.Wait()
	if submitErrors.Load() != 0 || accepted.Load() != capacity || full.Load() != attempts-capacity {
		t.Fatalf("submissions = accepted:%d full:%d errors:%d", accepted.Load(), full.Load(), submitErrors.Load())
	}
	if snapshot := shadow.Snapshot(); snapshot.QueueDepth != capacity || snapshot.QueueDepth > capacity {
		t.Fatalf("queue snapshot = %#v, want depth %d", snapshot, capacity)
	}

	close(releasePlanner)
	for index := 0; index < capacity+1; index++ {
		<-results
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() shutdown: %v", err)
	}
}

func TestShadowRejectsInvalidOrInactiveSubmissionWithoutRetention(t *testing.T) {
	now := time.Date(2026, 7, 16, 3, 0, 0, 0, time.UTC)
	shadow := newTestShadow(t, now, 1, PlannerFunc(func(context.Context, PlanningInput) (Planned, error) {
		return Planned{}, nil
	}), nil)
	if disposition, err := shadow.Submit(testSample("operation-before-run", now)); err != nil || disposition != SubmissionNotRunning {
		t.Fatalf("pre-run Submit() = %s, %v", disposition, err)
	}
	_, cancel, runErr := runTestShadow(t, shadow)
	invalid := testSample("", now)
	if disposition, err := shadow.Submit(invalid); !errors.Is(err, ErrInvalidObservation) || disposition != SubmissionInvalid {
		t.Fatalf("invalid Submit() = %s, %v", disposition, err)
	}
	if snapshot := shadow.Snapshot(); snapshot.NotRunning != 1 || snapshot.Invalid != 1 || snapshot.QueueDepth != 0 {
		t.Fatalf("rejection snapshot = %#v", snapshot)
	}
	cancel()
	if err := <-runErr; err != nil {
		t.Fatalf("Run() shutdown: %v", err)
	}
	if disposition, err := shadow.Submit(testSample("operation-after-stop", now)); err != nil || disposition != SubmissionNotRunning {
		t.Fatalf("post-stop Submit() = %s, %v", disposition, err)
	}
}

func newTestShadow(t *testing.T, now time.Time, capacity int, planner Planner, sink ResultSink) *Shadow {
	t.Helper()
	return newTestShadowWithClock(t, func() time.Time { return now }, capacity, planner, sink, nil)
}

func newTestShadowWithClock(t *testing.T, now func() time.Time, capacity int, planner Planner, sink ResultSink, sweep <-chan struct{}) *Shadow {
	t.Helper()
	shadow, err := NewShadow(ShadowConfig{
		Comparator: Config{
			Retention:  testRetention,
			MaxPending: capacity + 2,
			MaxRecent:  capacity + 2,
			Now:        now,
		},
		QueueCapacity: capacity,
		Planner:       planner,
		Sink:          sink,
		Sweep:         sweep,
	})
	if err != nil {
		t.Fatalf("NewShadow(): %v", err)
	}
	return shadow
}

func runTestShadow(t *testing.T, shadow *Shadow) (context.Context, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	runErr := make(chan error, 1)
	go func() { runErr <- shadow.Run(ctx) }()
	<-shadow.Ready()
	return ctx, cancel, runErr
}

func testSample(operationID string, capturedAt time.Time) Sample {
	observation := testObservation(operationID, SideExpected, capturedAt)
	return Sample{
		OperationID:    observation.OperationID,
		Input:          observation.Input,
		Watermarks:     observation.Watermarks,
		ExpectedPlan:   observation.Plan,
		CapturedAt:     observation.CapturedAt,
		ExpectedTiming: observation.Timing,
	}
}
