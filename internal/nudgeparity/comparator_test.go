package nudgeparity

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestComparatorPairsEquivalentObservationsInEitherOrder(t *testing.T) {
	for _, first := range []Side{SideExpected, SideActual} {
		t.Run(first.String()+" first", func(t *testing.T) {
			now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
			comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
			expected := testObservation("operation-1", SideExpected, now)
			actual := testObservation("operation-1", SideActual, now)

			firstObservation, secondObservation := expected, actual
			if first == SideActual {
				firstObservation, secondObservation = actual, expected
			}
			if results, err := comparator.Observe(firstObservation); err != nil || len(results) != 0 {
				t.Fatalf("first Observe() = %#v, %v; want no result", results, err)
			}
			results, err := comparator.Observe(secondObservation)
			if err != nil {
				t.Fatalf("second Observe(): %v", err)
			}
			assertSingleResult(t, results, ClassificationSame, ReasonEquivalent)
			if !results[0].HasExpected || !results[0].HasActual {
				t.Fatalf("paired result sides = expected:%t actual:%t, want both", results[0].HasExpected, results[0].HasActual)
			}
			assertBounds(t, comparator.Snapshot(), 0, 1, 8, 8)
		})
	}
}

func TestComparatorClassifiesDivergentPlan(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
	expected := testObservation("operation-1", SideExpected, now)
	actual := testObservation("operation-1", SideActual, now)
	actual.Plan = Plan{Decision: DecisionPark, Action: ActionNone}

	if _, err := comparator.Observe(expected); err != nil {
		t.Fatal(err)
	}
	results, err := comparator.Observe(actual)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleResult(t, results, ClassificationDivergent, ReasonPlanMismatch)
}

func TestComparatorRequiresEquivalentCompleteInputsAndWatermarks(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	tests := []struct {
		name   string
		mutate func(*Observation)
		reason Reason
	}{
		{name: "input incomplete", mutate: func(o *Observation) { o.Input.CommandDigest = "" }, reason: ReasonInputIncomplete},
		{name: "input mismatch", mutate: func(o *Observation) { o.Input.TargetLaunch = "launch-2" }, reason: ReasonInputMismatch},
		{name: "watermark incomplete", mutate: func(o *Observation) { o.Watermarks.OwnerEpoch = 0 }, reason: ReasonWatermarkIncomplete},
		{name: "store lineage mismatch", mutate: func(o *Observation) { o.Watermarks.StoreLineage = "store-2" }, reason: ReasonWatermarkMismatch},
		{name: "durable revision mismatch", mutate: func(o *Observation) { o.Watermarks.DurableRevision++ }, reason: ReasonWatermarkMismatch},
		{name: "config revision mismatch", mutate: func(o *Observation) { o.Watermarks.ConfigRevision = "config-2" }, reason: ReasonWatermarkMismatch},
		{name: "runtime revision mismatch", mutate: func(o *Observation) { o.Watermarks.RuntimeRevision++ }, reason: ReasonWatermarkMismatch},
		{name: "owner epoch mismatch", mutate: func(o *Observation) { o.Watermarks.OwnerEpoch++ }, reason: ReasonWatermarkMismatch},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
			expected := testObservation("operation-1", SideExpected, now)
			actual := testObservation("operation-1", SideActual, now)
			test.mutate(&actual)

			if _, err := comparator.Observe(expected); err != nil {
				t.Fatal(err)
			}
			results, err := comparator.Observe(actual)
			if err != nil {
				t.Fatal(err)
			}
			assertSingleResult(t, results, ClassificationIncomparable, test.reason)
		})
	}
}

func TestComparatorExpiresOneSidedObservationsAndClassifiesLateCounterparts(t *testing.T) {
	started := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name            string
		first           Side
		missing         Classification
		lateCounterpart Side
	}{
		{name: "missing actual", first: SideExpected, missing: ClassificationMissingActual, lateCounterpart: SideActual},
		{name: "missing expected", first: SideActual, missing: ClassificationMissingExpected, lateCounterpart: SideExpected},
	} {
		t.Run(test.name, func(t *testing.T) {
			now := started
			comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
			if _, err := comparator.Observe(testObservation("operation-1", test.first, now)); err != nil {
				t.Fatal(err)
			}

			now = now.Add(testRetention)
			results, err := comparator.Sweep()
			if err != nil {
				t.Fatal(err)
			}
			assertSingleResult(t, results, test.missing, ReasonExpired)

			late := testObservation("operation-1", test.lateCounterpart, now)
			results, err = comparator.Observe(late)
			if err != nil {
				t.Fatal(err)
			}
			assertSingleResult(t, results, ClassificationLate, ReasonCounterpartAfterTerminal)
		})
	}
}

func TestComparatorClassifiesIdenticalAndConflictingDuplicates(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	for _, side := range []Side{SideExpected, SideActual} {
		t.Run(side.String(), func(t *testing.T) {
			comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
			original := testObservation("operation-1", side, now)
			if _, err := comparator.Observe(original); err != nil {
				t.Fatal(err)
			}

			results, err := comparator.Observe(original)
			if err != nil {
				t.Fatal(err)
			}
			assertSingleResult(t, results, ClassificationDuplicate, ReasonDuplicateIdentical)

			conflict := original
			conflict.Plan = Plan{Decision: DecisionReject, Action: ActionNone}
			results, err = comparator.Observe(conflict)
			if err != nil {
				t.Fatal(err)
			}
			assertSingleResult(t, results, ClassificationDuplicate, ReasonDuplicateConflicting)
		})
	}
}

func TestComparatorEvictsOldestPendingAndBoundsPendingAndRecentState(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	comparator := newTestComparator(t, func() time.Time { return now }, 2, 2)

	for index := 1; index <= 5; index++ {
		operationID := fmt.Sprintf("operation-%d", index)
		results, err := comparator.Observe(testObservation(operationID, SideExpected, now))
		if err != nil {
			t.Fatalf("Observe(%s): %v", operationID, err)
		}
		if index <= 2 {
			if len(results) != 0 {
				t.Fatalf("Observe(%s) = %#v, want no eviction", operationID, results)
			}
		} else {
			assertSingleResult(t, results, ClassificationMissingActual, ReasonCapacity)
			wantEvicted := fmt.Sprintf("operation-%d", index-2)
			if results[0].OperationID != wantEvicted {
				t.Fatalf("evicted operation = %q, want %q", results[0].OperationID, wantEvicted)
			}
		}
		assertBounds(t, comparator.Snapshot(), min(index, 2), min(max(index-2, 0), 2), 2, 2)
	}
}

func TestComparatorRejectsInvalidObservationsAndClockRegressionWithoutRetainingThem(t *testing.T) {
	started := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	now := started
	comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)

	oversized := testObservation(strings.Repeat("x", MaxTextBytes+1), SideExpected, now)
	if _, err := comparator.Observe(oversized); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("oversized Observe() error = %v, want ErrInvalidObservation", err)
	}
	invalidUTF8 := testObservation("operation-\xff", SideExpected, now)
	if _, err := comparator.Observe(invalidUTF8); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("invalid UTF-8 Observe() error = %v, want ErrInvalidObservation", err)
	}
	future := testObservation("operation-future", SideExpected, now.Add(time.Second))
	if _, err := comparator.Observe(future); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("future Observe() error = %v, want ErrInvalidObservation", err)
	}
	assertBounds(t, comparator.Snapshot(), 0, 0, 8, 8)

	valid := testObservation("operation-valid", SideExpected, now)
	if _, err := comparator.Observe(valid); err != nil {
		t.Fatal(err)
	}
	now = now.Add(-time.Second)
	if _, err := comparator.Sweep(); !errors.Is(err, ErrClockRegression) {
		t.Fatalf("regressing Sweep() error = %v, want ErrClockRegression", err)
	}
	snapshot := comparator.Snapshot()
	if snapshot.ClockRegressions != 1 || snapshot.InvalidObservations != 3 {
		t.Fatalf("error counters = clock:%d invalid:%d, want 1/3", snapshot.ClockRegressions, snapshot.InvalidObservations)
	}
	assertBounds(t, snapshot, 1, 0, 8, 8)
}

func TestComparatorFlushReportsPendingLossInInsertionOrderAndResetsTombstones(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
	for _, observation := range []Observation{
		testObservation("operation-expected", SideExpected, now),
		testObservation("operation-actual", SideActual, now),
	} {
		if _, err := comparator.Observe(observation); err != nil {
			t.Fatal(err)
		}
	}

	results := comparator.Flush()
	if len(results) != 2 {
		t.Fatalf("Flush() returned %d results, want 2", len(results))
	}
	if results[0].OperationID != "operation-expected" || results[0].Classification != ClassificationMissingActual || results[0].Reason != ReasonFlush {
		t.Fatalf("first Flush result = %#v", results[0])
	}
	if results[1].OperationID != "operation-actual" || results[1].Classification != ClassificationMissingExpected || results[1].Reason != ReasonFlush {
		t.Fatalf("second Flush result = %#v", results[1])
	}
	assertBounds(t, comparator.Snapshot(), 0, 0, 8, 8)
}

func TestComparatorReportsNativeStartLatencyOnlyWithExplicitT8Evidence(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 2, 0, time.UTC)
	comparator := newTestComparator(t, func() time.Time { return now }, 8, 8)
	expected := testObservation("operation-1", SideExpected, now)
	expected.Timing = TimingEvidence{
		EnqueuedAt: now.Add(-2 * time.Second),
		PlannedAt:  now.Add(-time.Second),
	}
	actual := testObservation("operation-1", SideActual, now)
	actual.Timing = TimingEvidence{
		EnqueuedAt:       now.Add(-2 * time.Second),
		PlannedAt:        now.Add(-500 * time.Millisecond),
		NativeStartedAt:  now.Add(-250 * time.Millisecond),
		NativeStartProof: NativeStartProofT8,
	}

	if _, err := comparator.Observe(expected); err != nil {
		t.Fatal(err)
	}
	results, err := comparator.Observe(actual)
	if err != nil {
		t.Fatal(err)
	}
	assertSingleResult(t, results, ClassificationSame, ReasonEquivalent)
	if !results[0].ExpectedLatency.HasEnqueueToPlan || results[0].ExpectedLatency.EnqueueToPlan != time.Second {
		t.Fatalf("expected planning latency = %#v", results[0].ExpectedLatency)
	}
	if results[0].ExpectedLatency.HasEnqueueToNativeStart {
		t.Fatalf("expected native latency must be absent without T8 evidence: %#v", results[0].ExpectedLatency)
	}
	if !results[0].ActualLatency.HasEnqueueToNativeStart || results[0].ActualLatency.EnqueueToNativeStart != 1750*time.Millisecond {
		t.Fatalf("actual native latency = %#v", results[0].ActualLatency)
	}

	invalid := testObservation("operation-2", SideActual, now)
	invalid.Timing.NativeStartedAt = now
	if _, err := comparator.Observe(invalid); !errors.Is(err, ErrInvalidObservation) {
		t.Fatalf("unproven native timestamp error = %v, want ErrInvalidObservation", err)
	}
}

func TestComparatorConcurrentObservationKeepsStateBounded(t *testing.T) {
	now := time.Date(2026, 7, 16, 2, 0, 0, 0, time.UTC)
	const operations = 128
	comparator := newTestComparator(t, func() time.Time { return now }, operations, operations)
	results := make(chan Result, operations)
	errorsCh := make(chan error, operations*2)
	var workers sync.WaitGroup
	for _, side := range []Side{SideExpected, SideActual} {
		workers.Add(1)
		go func(side Side) {
			defer workers.Done()
			for index := 0; index < operations; index++ {
				produced, err := comparator.Observe(testObservation(fmt.Sprintf("operation-%03d", index), side, now))
				if err != nil {
					errorsCh <- err
					continue
				}
				for _, result := range produced {
					results <- result
				}
			}
		}(side)
	}
	workers.Wait()
	close(errorsCh)
	close(results)
	for err := range errorsCh {
		t.Errorf("Observe(): %v", err)
	}
	count := 0
	for result := range results {
		count++
		if result.Classification != ClassificationSame {
			t.Errorf("result %q classification = %s, want same", result.OperationID, result.Classification)
		}
	}
	if count != operations {
		t.Fatalf("paired results = %d, want %d", count, operations)
	}
	snapshot := comparator.Snapshot()
	if snapshot.Count(ClassificationSame) != operations {
		t.Fatalf("same counter = %d, want %d", snapshot.Count(ClassificationSame), operations)
	}
	assertBounds(t, snapshot, 0, operations, operations, operations)
}

const testRetention = 10 * time.Second

func newTestComparator(t *testing.T, now func() time.Time, maxPending, maxRecent int) *Comparator {
	t.Helper()
	comparator, err := New(Config{
		Retention:  testRetention,
		MaxPending: maxPending,
		MaxRecent:  maxRecent,
		Now:        now,
	})
	if err != nil {
		t.Fatalf("New(): %v", err)
	}
	return comparator
}

func testObservation(operationID string, side Side, capturedAt time.Time) Observation {
	return Observation{
		OperationID: operationID,
		Side:        side,
		Input: Input{
			CommandDigest:    "command-digest-1",
			TargetSession:    "session-1",
			TargetGeneration: 3,
			TargetLaunch:     "launch-1",
		},
		Watermarks: Watermarks{
			StoreLineage:    "store-1",
			DurableRevision: 7,
			ConfigRevision:  "config-1",
			RuntimeRevision: 11,
			OwnerEpoch:      13,
		},
		Plan:       Plan{Decision: DecisionExecute, Action: ActionNudge},
		CapturedAt: capturedAt,
	}
}

func assertSingleResult(t *testing.T, results []Result, classification Classification, reason Reason) {
	t.Helper()
	if len(results) != 1 {
		t.Fatalf("results count = %d, want 1: %#v", len(results), results)
	}
	if results[0].Classification != classification || results[0].Reason != reason {
		t.Fatalf("result = %s/%s, want %s/%s", results[0].Classification, results[0].Reason, classification, reason)
	}
}

func assertBounds(t *testing.T, snapshot Snapshot, wantPending, wantRecent, maxPending, maxRecent int) {
	t.Helper()
	if snapshot.Pending != wantPending || snapshot.Recent != wantRecent {
		t.Fatalf("state = pending:%d recent:%d, want %d/%d", snapshot.Pending, snapshot.Recent, wantPending, wantRecent)
	}
	if snapshot.Pending > maxPending || snapshot.Recent > maxRecent {
		t.Fatalf("state exceeds bounds: pending:%d/%d recent:%d/%d", snapshot.Pending, maxPending, snapshot.Recent, maxRecent)
	}
}
