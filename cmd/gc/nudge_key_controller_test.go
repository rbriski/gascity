package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/testutil"
)

func testSessionReconcileKey(t *testing.T, id string) reconcilekey.Session {
	t.Helper()
	key, err := reconcilekey.NewSession("test-store", id)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return key
}

func TestNudgeKeyControllerCoalescesDuplicateCausesBeforeStart(t *testing.T) {
	key := testSessionReconcileKey(t, "session-1")
	got := make(chan nudgeReconcileBatch, 1)
	controller, err := newNudgeKeyController(1, func(_ context.Context, gotKey reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		if gotKey != key {
			t.Errorf("key = %v, want %v", gotKey, key)
		}
		got <- batch
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	wantFirstEnqueuedAt := time.Unix(123, 456)
	controller.now = func() time.Time { return wantFirstEnqueuedAt }

	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue store commit: %v", err)
	}
	if err := controller.Enqueue(key, nudgeCauseTargetGeneration); err != nil {
		t.Fatalf("enqueue config: %v", err)
	}
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue duplicate store commit: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	batch := receiveBeforeDeadline(t, got)
	if want := nudgeCauseCommandCommit | nudgeCauseTargetGeneration; batch.Causes != want {
		t.Fatalf("causes = %v, want %v", batch.Causes, want)
	}
	if batch.FirstEnqueuedAt.IsZero() {
		t.Fatal("FirstEnqueuedAt is zero")
	}
	if !batch.FirstEnqueuedAt.Equal(wantFirstEnqueuedAt) {
		t.Fatalf("FirstEnqueuedAt = %s, want admission time %s", batch.FirstEnqueuedAt, wantFirstEnqueuedAt)
	}
	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyBacklogSnapshotPreservesOldestAdmissionAcrossDuplicates(t *testing.T) {
	first := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	second := first.Add(40 * time.Millisecond)
	observedAt := first.Add(125 * time.Millisecond)
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	firstKey := mustNudgeReconcileKey(t, "scope", "first")
	secondKey := mustNudgeReconcileKey(t, "scope", "second")
	controller.now = func() time.Time { return first }
	if err := controller.Enqueue(firstKey, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("first Enqueue: %v", err)
	}
	controller.now = func() time.Time { return second }
	if err := controller.Enqueue(firstKey, nudgeCauseProviderResult); err != nil {
		t.Fatalf("duplicate Enqueue: %v", err)
	}
	if err := controller.Enqueue(secondKey, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("second-key Enqueue: %v", err)
	}
	controller.now = func() time.Time { return observedAt }

	got := controller.backlogSnapshot()
	if got.Depth != 2 || got.OldestAge != observedAt.Sub(first) || got.AgeState != nudgeKeyBacklogAgeObserved {
		t.Fatalf("backlog snapshot = %+v, want depth 2 oldest age %v observed", got, observedAt.Sub(first))
	}
}

func TestNudgeKeyBacklogSnapshotClassifiesEmptyReplayAndClockRegression(t *testing.T) {
	now := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	if got := controller.backlogSnapshot(); got.Depth != 0 || got.OldestAge != 0 || got.AgeState != nudgeKeyBacklogAgeEmpty {
		t.Fatalf("empty backlog snapshot = %+v, want empty zero", got)
	}

	key := mustNudgeReconcileKey(t, "scope", "replay")
	controller.restoreBatch(key, nudgeReconcileBatch{WorkqueueReplay: true})
	if got := controller.backlogSnapshot(); got.Depth != 1 || got.OldestAge != 0 || got.AgeState != nudgeKeyBacklogAgeUnavailable {
		t.Fatalf("cause-free replay snapshot = %+v, want depth 1 unavailable", got)
	}

	controller.mu.Lock()
	controller.setPendingLocked(key, nudgeReconcileBatch{Causes: nudgeCauseAudit, FirstEnqueuedAt: now.Add(time.Millisecond)})
	controller.mu.Unlock()
	if got := controller.backlogSnapshot(); got.Depth != 1 || got.OldestAge != 0 || got.AgeState != nudgeKeyBacklogAgeClockRegressed {
		t.Fatalf("regressed backlog snapshot = %+v, want depth 1 clock-regressed", got)
	}
}

func TestNudgeKeyBacklogIndexTracksArbitraryRemovalRestoreAndUnavailableReplay(t *testing.T) {
	base := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	now := base
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	first := mustNudgeReconcileKey(t, "scope", "first")
	second := mustNudgeReconcileKey(t, "scope", "second")
	third := mustNudgeReconcileKey(t, "scope", "third")
	for i, key := range []reconcilekey.Session{first, second, third} {
		now = base.Add(time.Duration(i) * time.Millisecond)
		if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
			t.Fatalf("Enqueue(%s): %v", key, err)
		}
	}

	firstBatch, taken, ready := controller.takeBatch(first)
	if !taken || !ready {
		t.Fatalf("take first = taken %v ready %v, want true/true", taken, ready)
	}
	now = base.Add(10 * time.Millisecond)
	if got := controller.backlogSnapshot(); got.Depth != 2 || got.OldestAge != 9*time.Millisecond || got.AgeState != nudgeKeyBacklogAgeObserved {
		t.Fatalf("snapshot after oldest removal = %+v, want second key aged 9ms", got)
	}

	controller.restoreBatch(first, firstBatch)
	if got := controller.backlogSnapshot(); got.Depth != 3 || got.OldestAge != 10*time.Millisecond || got.AgeState != nudgeKeyBacklogAgeObserved {
		t.Fatalf("snapshot after oldest restore = %+v, want restored first key aged 10ms", got)
	}
	if _, taken, ready := controller.takeBatch(first); !taken || !ready {
		t.Fatalf("retake first = taken %v ready %v, want true/true", taken, ready)
	}

	replay := mustNudgeReconcileKey(t, "scope", "unavailable-replay")
	controller.restoreBatch(replay, nudgeReconcileBatch{WorkqueueReplay: true})
	if got := controller.backlogSnapshot(); got.Depth != 3 || got.OldestAge != 0 || got.AgeState != nudgeKeyBacklogAgeUnavailable {
		t.Fatalf("snapshot with unavailable replay = %+v, want unavailable depth 3", got)
	}
	if _, taken, ready := controller.takeBatch(replay); !taken || !ready {
		t.Fatalf("take replay = taken %v ready %v, want true/true", taken, ready)
	}
	if got := controller.backlogSnapshot(); got.Depth != 2 || got.OldestAge != 9*time.Millisecond || got.AgeState != nudgeKeyBacklogAgeObserved {
		t.Fatalf("snapshot after unavailable replay removal = %+v, want second key aged 9ms", got)
	}
}

func TestNudgeKeyBacklogIndexMatchesReferenceAcrossMutationSequence(t *testing.T) {
	base := time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC)
	now := base
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	keys := make([]reconcilekey.Session, 64)
	for i := range keys {
		keys[i] = mustNudgeReconcileKey(t, "scope", fmt.Sprintf("session-%03d", i))
		now = base.Add(time.Duration(i) * time.Millisecond)
		if err := controller.Enqueue(keys[i], nudgeCauseCommandCommit); err != nil {
			t.Fatalf("Enqueue(%d): %v", i, err)
		}
		if i%3 == 0 {
			now = now.Add(time.Millisecond)
			if err := controller.Enqueue(keys[i], nudgeCauseProviderResult); err != nil {
				t.Fatalf("duplicate Enqueue(%d): %v", i, err)
			}
		}
		assertNudgeKeyBacklogIndexMatchesReference(t, controller, now)
	}

	taken := make(map[int]nudgeReconcileBatch)
	for i := 0; i < len(keys); i += 2 {
		batch, ok, ready := controller.takeBatch(keys[i])
		if !ok || !ready {
			t.Fatalf("takeBatch(%d) = %v/%v, want true/true", i, ok, ready)
		}
		taken[i] = batch
		now = now.Add(time.Millisecond)
		assertNudgeKeyBacklogIndexMatchesReference(t, controller, now)
	}
	for i := 0; i < len(keys); i += 4 {
		controller.restoreBatch(keys[i], taken[i])
		now = now.Add(time.Millisecond)
		assertNudgeKeyBacklogIndexMatchesReference(t, controller, now)
	}

	replay := mustNudgeReconcileKey(t, "scope", "replay-without-admission-time")
	controller.restoreBatch(replay, nudgeReconcileBatch{WorkqueueReplay: true})
	assertNudgeKeyBacklogIndexMatchesReference(t, controller, now)
	if _, ok, ready := controller.takeBatch(replay); !ok || !ready {
		t.Fatalf("takeBatch(replay) = %v/%v, want true/true", ok, ready)
	}
	assertNudgeKeyBacklogIndexMatchesReference(t, controller, now)
}

func assertNudgeKeyBacklogIndexMatchesReference(t *testing.T, controller *nudgeKeyController, now time.Time) {
	t.Helper()
	controller.mu.Lock()
	depth := len(controller.pending)
	want := nudgeKeyBacklogSnapshot{Depth: int64(depth), AgeState: nudgeKeyBacklogAgeEmpty}
	var oldest time.Time
	for _, batch := range controller.pending {
		if batch.FirstEnqueuedAt.IsZero() {
			want.OldestAge = 0
			want.AgeState = nudgeKeyBacklogAgeUnavailable
			break
		}
		if oldest.IsZero() || batch.FirstEnqueuedAt.Before(oldest) {
			oldest = batch.FirstEnqueuedAt
		}
	}
	if depth > 0 && want.AgeState != nudgeKeyBacklogAgeUnavailable {
		age := now.Sub(oldest)
		if age < 0 {
			want.AgeState = nudgeKeyBacklogAgeClockRegressed
		} else {
			want.OldestAge = age
			want.AgeState = nudgeKeyBacklogAgeObserved
		}
	}
	controller.mu.Unlock()

	if got := controller.backlogSnapshot(); got != want {
		t.Fatalf("indexed backlog snapshot = %+v, reference = %+v", got, want)
	}
}

func TestNudgeKeyBacklogSnapshotAgesWhileWorkerIsStarvedAndClearsOnShutdown(t *testing.T) {
	first := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	now := first
	callbackStarted := make(chan struct{})
	releaseCallback := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		close(callbackStarted)
		<-releaseCallback
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	if err := controller.Enqueue(mustNudgeReconcileKey(t, "scope", "blocking"), nudgeCauseCommandCommit); err != nil {
		t.Fatalf("blocking Enqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	receiveBeforeDeadline(t, callbackStarted)

	now = first.Add(20 * time.Millisecond)
	if err := controller.Enqueue(mustNudgeReconcileKey(t, "scope", "starved"), nudgeCauseCommandCommit); err != nil {
		t.Fatalf("starved Enqueue: %v", err)
	}
	now = first.Add(220 * time.Millisecond)
	if got := controller.backlogSnapshot(); got.Depth != 1 || got.OldestAge != 200*time.Millisecond || got.AgeState != nudgeKeyBacklogAgeObserved {
		t.Fatalf("starved backlog snapshot = %+v, want one key aged 200ms", got)
	}

	cancel()
	close(releaseCallback)
	if err := receiveBeforeDeadline(t, done); err != nil {
		t.Fatalf("controller.Run: %v", err)
	}
	if got := controller.backlogSnapshot(); got.Depth != 0 || got.AgeState != nudgeKeyBacklogAgeEmpty {
		t.Fatalf("shutdown backlog snapshot = %+v, want empty", got)
	}
}

func TestNudgeKeyControllerSerializesOneKeyAndRunsIndependentKeysConcurrently(t *testing.T) {
	keyA := testSessionReconcileKey(t, "session-a")
	keyB := testSessionReconcileKey(t, "session-b")
	keyC := testSessionReconcileKey(t, "session-c")

	type startedCall struct {
		key   reconcilekey.Session
		batch nudgeReconcileBatch
		call  int
	}
	started := make(chan startedCall, 8)
	releaseA := make(chan struct{})
	releaseB := make(chan struct{})

	var mu sync.Mutex
	active := make(map[reconcilekey.Session]int)
	calls := make(map[reconcilekey.Session]int)
	concurrent := 0
	maxConcurrent := 0
	overlappedSameKey := false

	controller, err := newNudgeKeyController(2, func(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		mu.Lock()
		active[key]++
		if active[key] > 1 {
			overlappedSameKey = true
		}
		calls[key]++
		call := calls[key]
		concurrent++
		if concurrent > maxConcurrent {
			maxConcurrent = concurrent
		}
		mu.Unlock()

		started <- startedCall{key: key, batch: batch, call: call}
		switch {
		case key == keyA && call == 1:
			select {
			case <-releaseA:
			case <-ctx.Done():
			}
		case key == keyB:
			select {
			case <-releaseB:
			case <-ctx.Done():
			}
		}

		mu.Lock()
		active[key]--
		concurrent--
		mu.Unlock()
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	for _, key := range []reconcilekey.Session{keyA, keyB} {
		if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
			t.Fatalf("enqueue %v: %v", key, err)
		}
	}
	first := receiveBeforeDeadline(t, started)
	second := receiveBeforeDeadline(t, started)
	if first.key == second.key {
		t.Fatalf("first two concurrent starts used the same key %v", first.key)
	}

	if err := controller.Enqueue(keyA, nudgeCauseTargetGeneration); err != nil {
		t.Fatalf("enqueue key A config replay: %v", err)
	}
	if err := controller.Enqueue(keyA, nudgeCauseRuntimeReadiness); err != nil {
		t.Fatalf("enqueue key A runtime replay: %v", err)
	}
	if err := controller.Enqueue(keyA, nudgeCauseTargetGeneration); err != nil {
		t.Fatalf("enqueue key A duplicate replay: %v", err)
	}
	close(releaseB)
	if err := controller.Enqueue(keyC, nudgeCauseQuiescenceDeadline); err != nil {
		t.Fatalf("enqueue key C: %v", err)
	}
	third := receiveBeforeDeadline(t, started)
	if third.key != keyC {
		t.Fatalf("third start key = %v, want independent key C before key A is released", third.key)
	}

	close(releaseA)
	replay := receiveBeforeDeadline(t, started)
	if replay.key != keyA || replay.call != 2 {
		t.Fatalf("replay = key %v call %d, want key A call 2", replay.key, replay.call)
	}
	if want := nudgeCauseTargetGeneration | nudgeCauseRuntimeReadiness; replay.batch.Causes != want {
		t.Fatalf("replay causes = %v, want %v", replay.batch.Causes, want)
	}

	mu.Lock()
	gotMaxConcurrent := maxConcurrent
	gotOverlap := overlappedSameKey
	mu.Unlock()
	if gotMaxConcurrent < 2 {
		t.Fatalf("max concurrent reconciles = %d, want at least 2", gotMaxConcurrent)
	}
	if gotOverlap {
		t.Fatal("same key reconciled concurrently")
	}

	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyControllerEnqueueStartsWithoutPatrolOrFullScan(t *testing.T) {
	key := testSessionReconcileKey(t, "session-immediate")
	started := make(chan nudgeReconcileBatch, 1)
	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		started <- batch
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cr := &CityRuntime{nudgeKeyController: controller}
	stop := cr.startNudgeKeyController(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})

	// No patrol signal or fleet scan is supplied. Enqueue itself must make the
	// blocked worker runnable.
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	batch := receiveBeforeDeadline(t, started)
	if batch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("causes = %v, want store commit", batch.Causes)
	}
	cancel()
}

func TestCityRuntimeNudgeKeyControllerDefaultsInert(t *testing.T) {
	cr := &CityRuntime{}
	if cr.nudgeKeyController != nil {
		t.Fatal("zero CityRuntime unexpectedly has a keyed nudge controller")
	}
	ctx, cancel := context.WithCancel(context.Background())
	stop := cr.startNudgeKeyController(ctx)
	cancel()
	stop()
	stop() // inert stop is idempotent
}

func TestInstallNudgeKeyShadowNeverFallsBackToCityPathOrDisplayName(t *testing.T) {
	cr := &CityRuntime{
		cityPath: t.TempDir(),
		cityName: "tempting-display-name",
		cfg:      supervisorCfg(),
		stderr:   &bytes.Buffer{},
	}
	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("installNudgeKeyShadow without project identity: %v", err)
	}
	if cr.nudgeKeyController != nil || cr.nudgeKeyShadowScope != "" {
		t.Fatalf("missing project identity installed controller=%v scope=%q; path/name fallback is forbidden", cr.nudgeKeyController != nil, cr.nudgeKeyShadowScope)
	}
}

func TestNudgeKeyControllerRejectsMalformedAdmission(t *testing.T) {
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	valid := testSessionReconcileKey(t, "session-valid")
	for _, tc := range []struct {
		name  string
		key   reconcilekey.Session
		cause nudgeReconcileCause
	}{
		{name: "zero key", cause: nudgeCauseCommandCommit},
		{name: "zero cause", key: valid},
		{name: "unknown cause", key: valid, cause: nudgeReconcileCause(1 << 7)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := controller.Enqueue(tc.key, tc.cause); err == nil {
				t.Fatal("Enqueue() error = nil, want admission validation error")
			}
		})
	}
}

func TestNudgeKeyControllerCancellationBoundsCooperativeShutdown(t *testing.T) {
	key := testSessionReconcileKey(t, "session-cancel")
	started := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(ctx context.Context, _ reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		close(started)
		<-ctx.Done()
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	receiveBeforeDeadline(t, started)
	cancel()
	waitControllerStopped(t, done)
	if err := controller.Enqueue(key, nudgeCauseAudit); err == nil {
		t.Fatal("enqueue after shutdown error = nil, want admission refusal")
	}
}

func TestNudgeKeyControllerPanicFailsClosedWithoutConsumingAdmission(t *testing.T) {
	keyA := testSessionReconcileKey(t, "session-panic")
	keyB := testSessionReconcileKey(t, "session-after-panic")
	var stderr bytes.Buffer
	controller, err := newNudgeKeyController(1, func(_ context.Context, key reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		if key == keyA {
			panic("boom")
		}
		return nudgeReconcileSuccess()
	}, &stderr)
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}

	if err := controller.Enqueue(keyA, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue panic key: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runNudgeKeyController(ctx, t, controller)
	runErr := receiveBeforeDeadline(t, done)
	if runErr == nil || !bytes.Contains([]byte(runErr.Error()), []byte("nudge keyed reconciler panicked")) {
		t.Fatalf("Run() error = %v, want surfaced panic", runErr)
	}
	if !bytes.Contains([]byte(runErr.Error()), []byte(keyA.String())) {
		t.Fatalf("Run() error = %v, want preserved key %s", runErr, keyA)
	}
	controller.mu.Lock()
	failedBatch := controller.pending[keyA]
	controller.mu.Unlock()
	if failedBatch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("preserved failed causes = %v, want command commit", failedBatch.Causes)
	}
	if err := controller.Enqueue(keyB, nudgeCauseCommandCommit); err == nil {
		t.Fatal("enqueue after panic error = nil, want failed-closed admission")
	}
}

func TestNudgeKeyControllerOutcomeContinuationYieldsAndPreservesAdmission(t *testing.T) {
	key := testSessionReconcileKey(t, "session-continuation")
	start := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	now := start
	const yield = 25 * time.Millisecond
	batches := make(chan nudgeReconcileBatch, 2)
	deferred := make(chan time.Duration, 1)
	forgotten := make(chan struct{}, 1)
	var calls atomic.Int32
	controller, err := newNudgeKeyController(1, func(_ context.Context, gotKey reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		if gotKey != key {
			t.Errorf("key = %v, want %v", gotKey, key)
		}
		batches <- batch
		if calls.Add(1) == 1 {
			return nudgeReconcileContinue()
		}
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{}, nudgeKeyControllerOptions{
		continuationDelay: yield,
		retryBaseDelay:    time.Second,
		retryMaxDelay:     time.Minute,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	controller.addAfter = func(reconcilekey.Session, time.Duration) {}
	controller.onDeferred = func(_ reconcilekey.Session, disposition nudgeReconcileDisposition, delay time.Duration) {
		if disposition != nudgeReconcileOutcomeContinue {
			t.Errorf("deferred disposition = %v, want continuation", disposition)
		}
		deferred <- delay
	}
	controller.onForget = func(reconcilekey.Session) { forgotten <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first := receiveBeforeDeadline(t, batches)
	if got := receiveBeforeDeadline(t, deferred); got != yield {
		t.Fatalf("continuation delay = %v, want %v", got, yield)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls before clock advance = %d, want 1", got)
	}
	controller.mu.Lock()
	preserved := controller.pending[key]
	controller.mu.Unlock()
	if !preserved.FirstEnqueuedAt.Equal(start) || preserved.Causes != first.Causes {
		t.Fatalf("preserved batch = %#v, want admission %#v", preserved, first)
	}

	now = now.Add(yield)
	controller.queue.Add(key)
	second := receiveBeforeDeadline(t, batches)
	if !second.FirstEnqueuedAt.Equal(first.FirstEnqueuedAt) || second.Causes != first.Causes {
		t.Fatalf("continuation batch = %#v, want original admission %#v", second, first)
	}
	receiveBeforeDeadline(t, forgotten)
	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyControllerOutcomeAuditAddsCauseAfterBoundedYield(t *testing.T) {
	key := testSessionReconcileKey(t, "session-audit-needed")
	start := time.Date(2026, 7, 15, 11, 0, 0, 0, time.UTC)
	now := start
	const yield = 40 * time.Millisecond
	batches := make(chan nudgeReconcileBatch, 2)
	deferred := make(chan time.Duration, 1)
	var calls atomic.Int32
	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		batches <- batch
		if calls.Add(1) == 1 {
			return nudgeReconcileAudit()
		}
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{}, nudgeKeyControllerOptions{
		continuationDelay: yield,
		retryBaseDelay:    time.Second,
		retryMaxDelay:     time.Minute,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	controller.addAfter = func(reconcilekey.Session, time.Duration) {}
	controller.onDeferred = func(_ reconcilekey.Session, disposition nudgeReconcileDisposition, delay time.Duration) {
		if disposition != nudgeReconcileOutcomeAudit {
			t.Errorf("deferred disposition = %v, want audit", disposition)
		}
		deferred <- delay
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseProviderResult); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first := receiveBeforeDeadline(t, batches)
	if got := receiveBeforeDeadline(t, deferred); got != yield {
		t.Fatalf("audit delay = %v, want %v", got, yield)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls before audit eligibility = %d, want 1", got)
	}
	now = now.Add(yield)
	controller.queue.Add(key)
	second := receiveBeforeDeadline(t, batches)
	if want := first.Causes | nudgeCauseAudit; second.Causes != want {
		t.Fatalf("audit causes = %v, want %v", second.Causes, want)
	}
	if !second.FirstEnqueuedAt.Equal(first.FirstEnqueuedAt) {
		t.Fatalf("audit admission = %v, want %v", second.FirstEnqueuedAt, first.FirstEnqueuedAt)
	}
	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyControllerOutcomeTransientRetryIsCappedAndDuplicatesDoNotHotLoop(t *testing.T) {
	key := testSessionReconcileKey(t, "session-transient")
	start := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	now := start
	const (
		base     = 100 * time.Millisecond
		maxDelay = 200 * time.Millisecond
	)
	batches := make(chan nudgeReconcileBatch, 4)
	deferred := make(chan time.Duration, 4)
	forgotten := make(chan struct{}, 1)
	var calls atomic.Int32
	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		batches <- batch
		call := calls.Add(1)
		if call <= 3 {
			return nudgeReconcileTransient(errors.New("provider unavailable"))
		}
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{}, nudgeKeyControllerOptions{
		continuationDelay: time.Millisecond,
		retryBaseDelay:    base,
		retryMaxDelay:     maxDelay,
	})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.now = func() time.Time { return now }
	controller.addAfter = func(reconcilekey.Session, time.Duration) {}
	controller.onDeferred = func(_ reconcilekey.Session, disposition nudgeReconcileDisposition, delay time.Duration) {
		if disposition != nudgeReconcileOutcomeTransient {
			t.Errorf("deferred disposition = %v, want transient", disposition)
		}
		deferred <- delay
	}
	controller.onForget = func(reconcilekey.Session) { forgotten <- struct{}{} }

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	first := receiveBeforeDeadline(t, batches)
	if got := receiveBeforeDeadline(t, deferred); got != base {
		t.Fatalf("retry 1 delay = %v, want %v", got, base)
	}
	for attempt := 0; attempt < 10_000; attempt++ {
		if err := controller.Enqueue(key, nudgeCauseRuntimeReadiness); err != nil {
			t.Fatalf("duplicate enqueue %d: %v", attempt, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls under frozen clock after duplicates = %d, want 1", got)
	}

	now = now.Add(base)
	controller.queue.Add(key)
	second := receiveBeforeDeadline(t, batches)
	if want := nudgeCauseCommandCommit | nudgeCauseRuntimeReadiness; second.Causes != want {
		t.Fatalf("retry causes = %v, want %v", second.Causes, want)
	}
	if !second.FirstEnqueuedAt.Equal(first.FirstEnqueuedAt) {
		t.Fatalf("retry admission = %v, want %v", second.FirstEnqueuedAt, first.FirstEnqueuedAt)
	}
	if got := receiveBeforeDeadline(t, deferred); got != maxDelay {
		t.Fatalf("retry 2 delay = %v, want cap %v", got, maxDelay)
	}
	now = now.Add(maxDelay)
	controller.queue.Add(key)
	receiveBeforeDeadline(t, batches)
	if got := receiveBeforeDeadline(t, deferred); got != maxDelay {
		t.Fatalf("retry 3 delay = %v, want capped %v", got, maxDelay)
	}
	now = now.Add(maxDelay)
	controller.queue.Add(key)
	receiveBeforeDeadline(t, batches)
	receiveBeforeDeadline(t, forgotten)
	if got := controller.queue.NumRequeues(key); got != 0 {
		t.Fatalf("NumRequeues after success = %d, want reset", got)
	}
	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyControllerOutcomeInvariantFailsClosedAndPreservesKey(t *testing.T) {
	key := testSessionReconcileKey(t, "session-invariant")
	controller, err := newNudgeKeyController(1, func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome {
		return nudgeReconcileInvariant(errors.New("foreign lineage"))
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runErr := receiveBeforeDeadline(t, runNudgeKeyController(ctx, t, controller))
	if runErr == nil || !bytes.Contains([]byte(runErr.Error()), []byte("foreign lineage")) {
		t.Fatalf("Run() error = %v, want invariant cause", runErr)
	}
	if !bytes.Contains([]byte(runErr.Error()), []byte(key.String())) {
		t.Fatalf("Run() error = %v, want preserved key %s", runErr, key)
	}
	controller.mu.Lock()
	preserved := controller.pending[key]
	controller.mu.Unlock()
	if preserved.Causes != nudgeCauseCommandCommit {
		t.Fatalf("preserved invariant batch = %#v, want command cause", preserved)
	}
	if err := controller.Enqueue(key, nudgeCauseAudit); err == nil {
		t.Fatal("enqueue after invariant failure error = nil, want failed-closed admission")
	}
}

func TestNudgeKeyControllerGetTakeRacePreservesExplicitReplay(t *testing.T) {
	key := testSessionReconcileKey(t, "session-get-take-race")
	firstGet := make(chan struct{})
	releaseFirstGet := make(chan struct{})
	emptyReplayDone := make(chan struct{})
	processed := make(chan nudgeReconcileBatch, 2)
	var gets atomic.Int32
	var calls atomic.Int32

	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		calls.Add(1)
		processed <- batch
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.afterGet = func(reconcilekey.Session) {
		if gets.Add(1) == 1 {
			close(firstGet)
			<-releaseFirstGet
		}
	}
	controller.onEmptyReplay = func(reconcilekey.Session) { close(emptyReplayDone) }

	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue command: %v", err)
	}
	receiveBeforeDeadline(t, firstGet)
	// Get has marked the key processing, but takeBatch has not run. This Add is
	// included in the current cause snapshot and also marks the workqueue dirty.
	if err := controller.Enqueue(key, nudgeCauseTargetGeneration); err != nil {
		t.Fatalf("enqueue target generation: %v", err)
	}
	close(releaseFirstGet)
	batch := receiveBeforeDeadline(t, processed)
	if want := nudgeCauseCommandCommit | nudgeCauseTargetGeneration; batch.Causes != want {
		t.Fatalf("causes = %v, want %v", batch.Causes, want)
	}
	replay := receiveBeforeDeadline(t, processed)
	if !replay.WorkqueueReplay || replay.Causes != 0 || !replay.FirstEnqueuedAt.IsZero() {
		t.Fatalf("empty replay = %#v, want an explicit cause-free workqueue replay", replay)
	}
	receiveBeforeDeadline(t, emptyReplayDone)
	if got := calls.Load(); got != 2 {
		t.Fatalf("handler calls = %d, want initial evaluation plus one dirty replay", got)
	}
	cancel()
	waitControllerStopped(t, done)
}

func TestNudgeKeyControllerCancellationDoesNotSwallowCallbackPanic(t *testing.T) {
	key := testSessionReconcileKey(t, "session-cancel-panic")
	started := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(ctx context.Context, _ reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		close(started)
		<-ctx.Done()
		panic("panic-after-cancel")
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	receiveBeforeDeadline(t, started)
	cancel()
	runErr := receiveBeforeDeadline(t, done)
	if runErr == nil || !bytes.Contains([]byte(runErr.Error()), []byte("panic-after-cancel")) {
		t.Fatalf("Run() error = %v, want cancel-racing callback panic", runErr)
	}
	controller.mu.Lock()
	failedBatch := controller.pending[key]
	controller.mu.Unlock()
	if failedBatch.Causes != nudgeCauseCommandCommit {
		t.Fatalf("preserved failed causes = %v, want command commit", failedBatch.Causes)
	}
}

func TestNudgeKeyControllerPreservesPanicOnCauseFreeReplay(t *testing.T) {
	key := testSessionReconcileKey(t, "session-replay-panic")
	firstGet := make(chan struct{})
	releaseFirstGet := make(chan struct{})
	firstProcessed := make(chan struct{})
	var gets atomic.Int32
	controller, err := newNudgeKeyController(1, func(_ context.Context, _ reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
		if batch.WorkqueueReplay {
			panic("replay-panic")
		}
		close(firstProcessed)
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.afterGet = func(reconcilekey.Session) {
		if gets.Add(1) == 1 {
			close(firstGet)
			<-releaseFirstGet
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue command: %v", err)
	}
	receiveBeforeDeadline(t, firstGet)
	if err := controller.Enqueue(key, nudgeCauseTargetGeneration); err != nil {
		t.Fatalf("enqueue target generation: %v", err)
	}
	close(releaseFirstGet)
	receiveBeforeDeadline(t, firstProcessed)
	runErr := receiveBeforeDeadline(t, done)
	if runErr == nil || !bytes.Contains([]byte(runErr.Error()), []byte("replay-panic")) {
		t.Fatalf("Run() error = %v, want replay panic", runErr)
	}
	controller.mu.Lock()
	failedBatch := controller.pending[key]
	controller.mu.Unlock()
	if !failedBatch.WorkqueueReplay || failedBatch.Causes != 0 {
		t.Fatalf("preserved replay batch = %#v, want cause-free workqueue replay", failedBatch)
	}
}

func TestNudgeKeyControllerShutdownClosesAdmissionBeforeCancellation(t *testing.T) {
	key := testSessionReconcileKey(t, "session-shutdown-linearization")
	started := make(chan struct{})
	closed := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(ctx context.Context, _ reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		close(started)
		<-ctx.Done()
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	controller.onAdmissionClosed = func() { close(closed) }
	ctx, cancel := context.WithCancel(context.Background())
	cr := &CityRuntime{nudgeKeyController: controller}
	stop := cr.startNudgeKeyController(ctx)
	t.Cleanup(func() {
		cancel()
		stop()
	})
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	receiveBeforeDeadline(t, started)
	stopped := make(chan struct{})
	go func() {
		stop()
		close(stopped)
	}()
	receiveBeforeDeadline(t, closed)
	if err := controller.Enqueue(key, nudgeCauseAudit); err == nil {
		t.Fatal("enqueue after shutdown admission boundary error = nil")
	}
	receiveBeforeDeadline(t, stopped)
	cancel()
}

func TestNudgeKeyControllerRunIsSingleStart(t *testing.T) {
	key := testSessionReconcileKey(t, "session-single-start")
	started := make(chan struct{})
	controller, err := newNudgeKeyController(1, func(ctx context.Context, _ reconcilekey.Session, _ nudgeReconcileBatch) nudgeReconcileOutcome {
		close(started)
		<-ctx.Done()
		return nudgeReconcileSuccess()
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("newNudgeKeyController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := runNudgeKeyController(ctx, t, controller)
	if err := controller.Enqueue(key, nudgeCauseCommandCommit); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	receiveBeforeDeadline(t, started)

	secondRun := make(chan error, 1)
	go func() { secondRun <- controller.Run(context.Background()) }()
	if err := receiveBeforeDeadline(t, secondRun); err == nil {
		t.Fatal("second Run() error = nil, want single-start refusal")
	}
	cancel()
	waitControllerStopped(t, done)
}

func runNudgeKeyController(ctx context.Context, t *testing.T, controller *nudgeKeyController) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- controller.Run(ctx) }()
	return done
}

func waitControllerStopped(t *testing.T, done <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("controller.Run: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("controller did not stop within shutdown bound")
	}
}

func receiveBeforeDeadline[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), testutil.GoroutineRaceTimeout)
	defer cancel()
	select {
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal("timed out waiting for controller signal")
		var zero T
		return zero
	}
}
