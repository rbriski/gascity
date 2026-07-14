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

type nudgeReconcileFunc func(context.Context, reconcilekey.Session, nudgeReconcileBatch)

// nudgeKeyController is the first domain-local keyed scheduler. It deliberately
// owns no provider or store dependencies: the injected callback is shadow-only
// until a later ownership gate installs a real nudge reconciler.
type nudgeKeyController struct {
	queue     workqueue.TypedInterface[reconcilekey.Session]
	workers   int
	reconcile nudgeReconcileFunc
	stderr    io.Writer
	now       func() time.Time
	ready     chan struct{}
	failureCh chan error

	mu        sync.Mutex
	accepting bool
	started   bool
	stopped   bool
	pending   map[reconcilekey.Session]nudgeReconcileBatch

	// Deterministic barriers for the Get/takeBatch race contract. Production
	// leaves both nil; tests install them before Run starts.
	afterGet          func(reconcilekey.Session)
	onEmptyReplay     func(reconcilekey.Session)
	onAdmissionClosed func()
}

func newNudgeKeyController(workers int, reconcile nudgeReconcileFunc, stderr io.Writer) (*nudgeKeyController, error) {
	if workers < 1 {
		return nil, fmt.Errorf("creating nudge keyed reconciler: workers must be positive")
	}
	if reconcile == nil {
		return nil, fmt.Errorf("creating nudge keyed reconciler: reconcile callback is nil")
	}
	if stderr == nil {
		return nil, fmt.Errorf("creating nudge keyed reconciler: stderr is nil")
	}
	return &nudgeKeyController{
		queue:     workqueue.NewTyped[reconcilekey.Session](),
		workers:   workers,
		reconcile: reconcile,
		stderr:    stderr,
		now:       time.Now,
		ready:     make(chan struct{}),
		failureCh: make(chan error, 1),
		accepting: true,
		pending:   make(map[reconcilekey.Session]nudgeReconcileBatch),
	}, nil
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
	// Add while holding mu serializes admission with stopAdmission. The
	// workqueue never runs callbacks while Add holds its own lock.
	c.queue.Add(key)
	return nil
}

// Run starts the fixed worker set and blocks until ctx is canceled. A callback
// must honor ctx for shutdown to be bounded; the initial production seam never
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

		batch, ok := c.takeBatch(key)
		if !ok {
			// Enqueue can win after Get marks the key processing but before
			// takeBatch. The cause joins the current batch while Add also marks
			// the queue dirty. Preserve client-go's promised follow-up reconcile
			// explicitly; causes are zero because that source was already folded
			// into the preceding batch, while WorkqueueReplay records why this
			// latest-state evaluation exists.
			batch = nudgeReconcileBatch{WorkqueueReplay: true}
		}
		err := c.invoke(ctx, key, batch)
		c.queue.Done(key)
		if batch.WorkqueueReplay && c.onEmptyReplay != nil {
			c.onEmptyReplay(key)
		}
		if err != nil {
			c.restoreBatch(key, batch)
			c.reportFailure(err)
			return
		}
	}
}

func (c *nudgeKeyController) takeBatch(key reconcilekey.Session) (nudgeReconcileBatch, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	batch, ok := c.pending[key]
	if !ok || batch.Causes == 0 {
		return nudgeReconcileBatch{}, false
	}
	delete(c.pending, key)
	return batch, true
}

func (c *nudgeKeyController) restoreBatch(key reconcilekey.Session, failed nudgeReconcileBatch) {
	c.mu.Lock()
	defer c.mu.Unlock()
	pending := c.pending[key]
	if !failed.FirstEnqueuedAt.IsZero() &&
		(pending.FirstEnqueuedAt.IsZero() || failed.FirstEnqueuedAt.Before(pending.FirstEnqueuedAt)) {
		pending.FirstEnqueuedAt = failed.FirstEnqueuedAt
	}
	pending.Causes |= failed.Causes
	pending.WorkqueueReplay = pending.WorkqueueReplay || failed.WorkqueueReplay
	c.pending[key] = pending
}

func (c *nudgeKeyController) reportFailure(err error) {
	select {
	case c.failureCh <- err:
	default:
		fmt.Fprintf(c.stderr, "nudge keyed reconciler additional worker failure: %v\n", err) //nolint:errcheck // primary failure is already returning through Run
	}
}

func (c *nudgeKeyController) invoke(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("nudge keyed reconciler panicked for %s: %v\n%s", key, recovered, debug.Stack())
		}
	}()
	c.reconcile(ctx, key, batch)
	return nil
}
