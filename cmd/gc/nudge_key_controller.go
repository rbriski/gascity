package main

import (
	"context"
	"fmt"
	"io"
	"runtime/debug"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/reconcilekey"
	"k8s.io/client-go/util/workqueue"
)

// nudgeReconcileCause is a bounded bit set of source classes that can change
// whether one target session has actionable nudge work. The queue carries only
// the stable key; workers always reconcile the union of causes recorded here.
type nudgeReconcileCause uint8

const (
	nudgeCauseCommandCommit nudgeReconcileCause = 1 << iota
	nudgeCauseTargetGeneration
	nudgeCauseRuntimeReadiness
	nudgeCauseQuiescence
	nudgeCauseQuiescenceDeadline
	nudgeCauseProviderResult
	nudgeCauseAudit
)

const allNudgeReconcileCauses = nudgeCauseCommandCommit |
	nudgeCauseTargetGeneration |
	nudgeCauseRuntimeReadiness |
	nudgeCauseQuiescence |
	nudgeCauseQuiescenceDeadline |
	nudgeCauseProviderResult |
	nudgeCauseAudit

type nudgeReconcileBatch struct {
	Causes          nudgeReconcileCause
	FirstEnqueuedAt time.Time
	WorkqueueReplay bool
}

type nudgeKeyBacklogAgeState uint8

const (
	nudgeKeyBacklogAgeEmpty nudgeKeyBacklogAgeState = iota + 1
	nudgeKeyBacklogAgeObserved
	nudgeKeyBacklogAgeUnavailable
	nudgeKeyBacklogAgeClockRegressed
)

// nudgeKeyBacklogSnapshot is an identity-free instantaneous view of dirty keys
// waiting or deferred in the scheduler. A key executing in a worker is not
// backlog; a duplicate admission remains one key with its original age.
type nudgeKeyBacklogSnapshot struct {
	Depth     int64
	OldestAge time.Duration
	AgeState  nudgeKeyBacklogAgeState
}

// nudgeReconcileDisposition is the closed scheduling vocabulary returned by a
// keyed callback. A callback cannot smuggle queue policy through arbitrary
// durations or booleans: the controller owns every follow-up admission.
type nudgeReconcileDisposition uint8

const (
	nudgeReconcileOutcomeForget nudgeReconcileDisposition = iota + 1
	nudgeReconcileOutcomeContinue
	nudgeReconcileOutcomeAudit
	nudgeReconcileOutcomeTransient
	nudgeReconcileOutcomeInvariant
)

type nudgeReconcileOutcome struct {
	disposition nudgeReconcileDisposition
	err         error
}

func nudgeReconcileSuccess() nudgeReconcileOutcome {
	return nudgeReconcileOutcome{disposition: nudgeReconcileOutcomeForget}
}

func nudgeReconcileContinue() nudgeReconcileOutcome {
	return nudgeReconcileOutcome{disposition: nudgeReconcileOutcomeContinue}
}

func nudgeReconcileAudit() nudgeReconcileOutcome {
	return nudgeReconcileOutcome{disposition: nudgeReconcileOutcomeAudit}
}

func nudgeReconcileTransient(err error) nudgeReconcileOutcome {
	return nudgeReconcileOutcome{disposition: nudgeReconcileOutcomeTransient, err: err}
}

func nudgeReconcileInvariant(err error) nudgeReconcileOutcome {
	return nudgeReconcileOutcome{disposition: nudgeReconcileOutcomeInvariant, err: err}
}

func (o nudgeReconcileOutcome) validate() error {
	switch o.disposition {
	case nudgeReconcileOutcomeForget, nudgeReconcileOutcomeContinue, nudgeReconcileOutcomeAudit:
		if o.err != nil {
			return fmt.Errorf("disposition %d unexpectedly carries an error", o.disposition)
		}
	case nudgeReconcileOutcomeTransient, nudgeReconcileOutcomeInvariant:
		if o.err == nil {
			return fmt.Errorf("disposition %d requires an error", o.disposition)
		}
	default:
		return fmt.Errorf("unknown disposition %d", o.disposition)
	}
	return nil
}

type nudgeReconcileFunc func(context.Context, reconcilekey.Session, nudgeReconcileBatch) nudgeReconcileOutcome

const (
	defaultNudgeContinuationDelay = 10 * time.Millisecond
	defaultNudgeRetryBaseDelay    = 100 * time.Millisecond
	defaultNudgeRetryMaxDelay     = 30 * time.Second
)

type nudgeKeyControllerOptions struct {
	continuationDelay time.Duration
	retryBaseDelay    time.Duration
	retryMaxDelay     time.Duration
}

type nudgeDeferredAdmission struct {
	disposition nudgeReconcileDisposition
	notBefore   time.Time
}

// nudgeKeyController is the first domain-local keyed scheduler. It deliberately
// owns no provider or store dependencies: the injected callback is read-only
// until a later ownership gate installs an effectful nudge reconciler.
type nudgeKeyController struct {
	queue     workqueue.TypedRateLimitingInterface[reconcilekey.Session]
	limiter   workqueue.TypedRateLimiter[reconcilekey.Session]
	workers   int
	reconcile nudgeReconcileFunc
	stderr    io.Writer
	now       func() time.Time
	addAfter  func(reconcilekey.Session, time.Duration)
	yield     time.Duration
	ready     chan struct{}
	failureCh chan error

	mu       sync.Mutex
	stderrMu sync.Mutex

	accepting bool
	started   bool
	stopped   bool
	pending   map[reconcilekey.Session]nudgeReconcileBatch
	deferred  map[reconcilekey.Session]nudgeDeferredAdmission

	// Deterministic barriers for the Get/takeBatch race contract. Production
	// leaves both nil; tests install them before Run starts.
	afterGet          func(reconcilekey.Session)
	onEmptyReplay     func(reconcilekey.Session)
	onAdmissionClosed func()
	onDeferred        func(reconcilekey.Session, nudgeReconcileDisposition, time.Duration)
	onForget          func(reconcilekey.Session)
}

func newNudgeKeyController(workers int, reconcile nudgeReconcileFunc, stderr io.Writer, supplied ...nudgeKeyControllerOptions) (*nudgeKeyController, error) {
	if workers < 1 {
		return nil, fmt.Errorf("creating nudge keyed reconciler: workers must be positive")
	}
	if reconcile == nil {
		return nil, fmt.Errorf("creating nudge keyed reconciler: reconcile callback is nil")
	}
	if stderr == nil {
		return nil, fmt.Errorf("creating nudge keyed reconciler: stderr is nil")
	}
	options, err := normalizeNudgeKeyControllerOptions(supplied)
	if err != nil {
		return nil, err
	}
	limiter := workqueue.NewTypedItemExponentialFailureRateLimiter[reconcilekey.Session](options.retryBaseDelay, options.retryMaxDelay)
	queue := workqueue.NewTypedRateLimitingQueue[reconcilekey.Session](limiter)
	controller := &nudgeKeyController{
		queue:     queue,
		limiter:   limiter,
		workers:   workers,
		reconcile: reconcile,
		stderr:    stderr,
		now:       time.Now,
		yield:     options.continuationDelay,
		ready:     make(chan struct{}),
		failureCh: make(chan error, 1),
		accepting: true,
		pending:   make(map[reconcilekey.Session]nudgeReconcileBatch),
		deferred:  make(map[reconcilekey.Session]nudgeDeferredAdmission),
	}
	controller.addAfter = queue.AddAfter
	return controller, nil
}

func normalizeNudgeKeyControllerOptions(supplied []nudgeKeyControllerOptions) (nudgeKeyControllerOptions, error) {
	if len(supplied) > 1 {
		return nudgeKeyControllerOptions{}, fmt.Errorf("creating nudge keyed reconciler: received %d option sets, want at most one", len(supplied))
	}
	options := nudgeKeyControllerOptions{
		continuationDelay: defaultNudgeContinuationDelay,
		retryBaseDelay:    defaultNudgeRetryBaseDelay,
		retryMaxDelay:     defaultNudgeRetryMaxDelay,
	}
	if len(supplied) == 1 {
		options = supplied[0]
	}
	if options.continuationDelay <= 0 {
		return nudgeKeyControllerOptions{}, fmt.Errorf("creating nudge keyed reconciler: continuation delay must be positive")
	}
	if options.retryBaseDelay <= 0 {
		return nudgeKeyControllerOptions{}, fmt.Errorf("creating nudge keyed reconciler: retry base delay must be positive")
	}
	if options.retryMaxDelay < options.retryBaseDelay {
		return nudgeKeyControllerOptions{}, fmt.Errorf("creating nudge keyed reconciler: retry max delay %s is below base delay %s", options.retryMaxDelay, options.retryBaseDelay)
	}
	return options, nil
}

func (c *nudgeKeyController) backlogSnapshot() nudgeKeyBacklogSnapshot {
	if c == nil {
		return nudgeKeyBacklogSnapshot{AgeState: nudgeKeyBacklogAgeEmpty}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	depth := len(c.pending)
	if depth == 0 {
		return nudgeKeyBacklogSnapshot{AgeState: nudgeKeyBacklogAgeEmpty}
	}
	var oldest time.Time
	for _, batch := range c.pending {
		if batch.FirstEnqueuedAt.IsZero() {
			return nudgeKeyBacklogSnapshot{Depth: int64(depth), AgeState: nudgeKeyBacklogAgeUnavailable}
		}
		if oldest.IsZero() || batch.FirstEnqueuedAt.Before(oldest) {
			oldest = batch.FirstEnqueuedAt
		}
	}
	now := time.Now()
	if c.now != nil {
		now = c.now()
	}
	age := now.Sub(oldest)
	if age < 0 {
		return nudgeKeyBacklogSnapshot{Depth: int64(depth), AgeState: nudgeKeyBacklogAgeClockRegressed}
	}
	return nudgeKeyBacklogSnapshot{Depth: int64(depth), OldestAge: age, AgeState: nudgeKeyBacklogAgeObserved}
}

// Enqueue marks one stable target dirty and merges the typed cause before the
// workqueue Add. That ordering prevents a worker from observing a key without
// the source evidence that made it runnable.
func (c *nudgeKeyController) Enqueue(key reconcilekey.Session, cause nudgeReconcileCause) error {
	if key.IsZero() {
		return fmt.Errorf("enqueueing nudge reconcile: target key is zero")
	}
	if cause == 0 || cause&^allNudgeReconcileCauses != 0 {
		return fmt.Errorf("enqueueing nudge reconcile for %s: invalid cause bits 0x%x", key, uint8(cause))
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.accepting {
		return fmt.Errorf("enqueueing nudge reconcile for %s: controller is stopped", key)
	}
	batch := c.pending[key]
	if batch.Causes == 0 {
		// This is the admission linearization point. Recording time under the
		// same mutex as the cause union makes concurrent first-ready ordering
		// deterministic and keeps later duplicates from resetting age.
		batch.FirstEnqueuedAt = c.now()
	}
	batch.Causes |= cause
	c.pending[key] = batch
	if _, delayed := c.deferred[key]; delayed {
		// The source evidence remains dirty, but an irrelevant duplicate must
		// not bypass the callback-owned continuation/retry eligibility edge.
		return nil
	}
	// Add while holding mu serializes admission with stopAdmission. The
	// workqueue never runs callbacks while Add holds its own lock.
	c.queue.Add(key)
	return nil
}

// Run starts the fixed worker set and blocks until ctx is canceled. A callback
// must honor ctx for shutdown to be bounded; the production read shadow never
// installs an effectful callback.
func (c *nudgeKeyController) Run(ctx context.Context) error {
	if ctx == nil {
		return fmt.Errorf("running nudge keyed reconciler: context is nil")
	}
	c.mu.Lock()
	if c.started || c.stopped {
		c.mu.Unlock()
		return fmt.Errorf("running nudge keyed reconciler: controller is single-start")
	}
	c.started = true
	c.mu.Unlock()

	workerCtx, cancelWorkers := context.WithCancel(ctx)
	defer cancelWorkers()
	var workers sync.WaitGroup
	workers.Add(c.workers)
	for i := 0; i < c.workers; i++ {
		go func() {
			defer workers.Done()
			c.runWorker(workerCtx)
		}()
	}
	close(c.ready)

	var runErr error
	select {
	case <-ctx.Done():
	case runErr = <-c.failureCh:
	}
	// Admission closure, cancellation, and worker join are deliberately
	// separate. An in-flight callback may report a failure while cancellation
	// is already readable; joining before the final failure drain makes that
	// race deterministic instead of allowing select order to swallow a panic.
	c.closeAdmission()
	cancelWorkers()
	workers.Wait()
	select {
	case joinedErr := <-c.failureCh:
		if runErr == nil {
			runErr = joinedErr
		} else {
			fmt.Fprintf(c.stderr, "nudge keyed reconciler additional joined worker failure: %v\n", joinedErr) //nolint:errcheck // primary failure is returned
		}
	default:
	}
	c.mu.Lock()
	if runErr == nil {
		clear(c.pending)
	}
	c.stopped = true
	c.mu.Unlock()
	return runErr
}

func (c *nudgeKeyController) closeAdmission() {
	c.mu.Lock()
	if !c.accepting {
		c.mu.Unlock()
		return
	}
	c.accepting = false
	// Do not drain: durable command truth and later audit reconstruct work. A
	// shutdown must not wait behind a queued shadow backlog.
	c.queue.ShutDown()
	onClosed := c.onAdmissionClosed
	c.mu.Unlock()
	if onClosed != nil {
		onClosed()
	}
}

func (c *nudgeKeyController) runWorker(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		key, shutdown := c.queue.Get()
		if shutdown {
			return
		}
		if c.afterGet != nil {
			c.afterGet(key)
		}
		if ctx.Err() != nil {
			c.queue.Done(key)
			return
		}

		batch, ok, eligible := c.takeBatch(key)
		if !eligible {
			// A dirty Add that raced the callback result may already be in the
			// workqueue. Consume that queue replay without consuming the retained
			// batch; the single delayed admission remains responsible for wakeup.
			c.queue.Done(key)
			continue
		}
		if !ok {
			// Enqueue can win after Get marks the key processing but before
			// takeBatch. The cause joins the current batch while Add also marks
			// the queue dirty. Preserve client-go's promised follow-up reconcile
			// explicitly; causes are zero because that source was already folded
			// into the preceding batch, while WorkqueueReplay records why this
			// latest-state evaluation exists.
			batch = nudgeReconcileBatch{WorkqueueReplay: true}
		}
		outcome, err := c.invoke(ctx, key, batch)
		if err != nil {
			c.restoreBatch(key, batch)
			c.queue.Done(key)
			c.reportFailure(err)
			return
		}
		if err := outcome.validate(); err != nil {
			c.restoreBatch(key, batch)
			c.queue.Done(key)
			c.reportFailure(fmt.Errorf("nudge keyed reconciler invariant failed for %s: invalid callback outcome: %w", key, err))
			return
		}

		switch outcome.disposition {
		case nudgeReconcileOutcomeForget:
			c.queue.Forget(key)
			c.queue.Done(key)
			if c.onForget != nil {
				c.onForget(key)
			}
		case nudgeReconcileOutcomeContinue:
			c.queue.Forget(key)
			c.deferBatch(key, batch, outcome.disposition, c.yield)
			c.queue.Done(key)
		case nudgeReconcileOutcomeAudit:
			c.queue.Forget(key)
			batch.Causes |= nudgeCauseAudit
			c.deferBatch(key, batch, outcome.disposition, c.yield)
			c.queue.Done(key)
		case nudgeReconcileOutcomeTransient:
			delay := c.limiter.When(key)
			c.deferBatch(key, batch, outcome.disposition, delay)
			c.queue.Done(key)
			c.logTransient(key, outcome.err, delay)
		case nudgeReconcileOutcomeInvariant:
			c.restoreBatch(key, batch)
			c.queue.Done(key)
			c.reportFailure(fmt.Errorf("nudge keyed reconciler invariant failed for %s: %w", key, outcome.err))
			return
		}
		if batch.WorkqueueReplay && c.onEmptyReplay != nil {
			c.onEmptyReplay(key)
		}
	}
}

func (c *nudgeKeyController) takeBatch(key reconcilekey.Session) (nudgeReconcileBatch, bool, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if deferred, ok := c.deferred[key]; ok {
		now := c.now()
		if now.Before(deferred.notBefore) {
			return nudgeReconcileBatch{}, false, false
		}
		delete(c.deferred, key)
	}
	batch, ok := c.pending[key]
	if !ok || (batch.Causes == 0 && !batch.WorkqueueReplay) {
		return nudgeReconcileBatch{}, false, true
	}
	delete(c.pending, key)
	return batch, true, true
}

func (c *nudgeKeyController) restoreBatch(key reconcilekey.Session, failed nudgeReconcileBatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.restoreBatchLocked(key, failed)
}

func (c *nudgeKeyController) restoreBatchLocked(key reconcilekey.Session, failed nudgeReconcileBatch) {
	pending := c.pending[key]
	if !failed.FirstEnqueuedAt.IsZero() &&
		(pending.FirstEnqueuedAt.IsZero() || failed.FirstEnqueuedAt.Before(pending.FirstEnqueuedAt)) {
		pending.FirstEnqueuedAt = failed.FirstEnqueuedAt
	}
	pending.Causes |= failed.Causes
	pending.WorkqueueReplay = pending.WorkqueueReplay || failed.WorkqueueReplay
	c.pending[key] = pending
}

func (c *nudgeKeyController) deferBatch(key reconcilekey.Session, batch nudgeReconcileBatch, disposition nudgeReconcileDisposition, delay time.Duration) {
	c.mu.Lock()
	now := c.now()
	c.restoreBatchLocked(key, batch)
	pending := c.pending[key]
	if pending.FirstEnqueuedAt.IsZero() {
		pending.FirstEnqueuedAt = now
		c.pending[key] = pending
	}
	c.deferred[key] = nudgeDeferredAdmission{
		disposition: disposition,
		notBefore:   now.Add(delay),
	}
	c.mu.Unlock()

	c.addAfter(key, delay)
	if c.onDeferred != nil {
		c.onDeferred(key, disposition, delay)
	}
}

func (c *nudgeKeyController) logTransient(key reconcilekey.Session, err error, delay time.Duration) {
	c.stderrMu.Lock()
	defer c.stderrMu.Unlock()
	fmt.Fprintf(c.stderr, "nudge keyed reconciler transient failure for %s; retrying after %s: %v\n", key, delay, err) //nolint:errcheck // the callback error is retained by the retry state
}

func (c *nudgeKeyController) reportFailure(err error) {
	select {
	case c.failureCh <- err:
	default:
		fmt.Fprintf(c.stderr, "nudge keyed reconciler additional worker failure: %v\n", err) //nolint:errcheck // primary failure is already returning through Run
	}
}

func (c *nudgeKeyController) invoke(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) (outcome nudgeReconcileOutcome, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("nudge keyed reconciler panicked for %s: %v\n%s", key, recovered, debug.Stack())
		}
	}()
	return c.reconcile(ctx, key, batch), nil
}
