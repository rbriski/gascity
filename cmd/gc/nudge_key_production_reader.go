package main

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

const (
	// nudgeCommandStartupSnapshotLimit matches the durable repository's hard
	// complete-snapshot ceiling. The repository queries one extra row and
	// returns an explicit overflow error; this caller never accepts truncation.
	nudgeCommandStartupSnapshotLimit = 4095
	// nudgeKeyReadMaxPagesPerCallback bounds stack-local continuation work.
	nudgeKeyReadMaxPagesPerCallback = 4
	// nudgeKeyReadMaxSlice bounds CPU residency without scheduling a follow-up
	// for unchanged read-only work.
	nudgeKeyReadMaxSlice = 25 * time.Millisecond
)

var errNudgeCommandSourceUnverified = errors.New("verified durable nudge command source unavailable")

// nudgeCommandSource is the complete read-only durable command surface used by
// cmd/gc. Snapshot must be transaction-consistent and complete-or-error. Get
// resolves one exact durable command ID and returns the repository watermark
// that produced it.
type nudgeCommandSource interface {
	Snapshot(context.Context, int) (nudgequeue.CommandIndexSnapshot, error)
	Get(context.Context, string) (nudgequeue.CommandIndexResolution, error)
}

// nudgeCommandSourceOpener receives cityPath only so the production adapter can
// locate independent restore-anchor evidence. The path is never command-store
// identity; the opened repository snapshot supplies the sole binding.
type nudgeCommandSourceOpener func(context.Context, string, beads.Store) (nudgeCommandSource, error)

type nudgeCommandSourceErrorClass uint8

const (
	nudgeCommandSourceErrorInvariant nudgeCommandSourceErrorClass = iota + 1
	nudgeCommandSourceErrorTransient
)

// nudgeCommandSourceErrorClassifier is implemented by the concrete repository
// adapter. Unknown errors fail closed as invariants; only errors the adapter
// positively identifies as retryable enter controller backoff.
type nudgeCommandSourceErrorClassifier interface {
	ClassifyNudgeCommandSourceError(error) nudgeCommandSourceErrorClass
}

// openProductionNudgeCommandSource is the certified repository adapter. Tests
// may replace the CityRuntime-local opener; production never derives store
// identity from city, project, path, or config identity.
var openProductionNudgeCommandSource nudgeCommandSourceOpener = openVerifiedProductionNudgeCommandSource

// nudgeKeyReadShadow owns only a reconstructable read projection. It has no
// store writer, runtime provider, worker handle, or effect executor; the legacy
// dispatcher remains the sole delivery owner.
type nudgeKeyReadShadow struct {
	source     nudgeCommandSource
	index      *nudgequeue.CommandIndex
	reconciler *nudgeKeyReconciler
	store      nudgequeue.CommandStoreBinding
	warnings   *nudgeKeyObservationWarnings
	now        func() time.Time
	maxPages   int
	sliceLimit time.Duration

	auditRequired atomic.Bool
}

func newNudgeKeyReadShadow(ctx context.Context, source nudgeCommandSource, pageLimit int, warnings *nudgeKeyObservationWarnings) (*nudgeKeyReadShadow, error) {
	if ctx == nil {
		return nil, errors.New("creating keyed nudge read shadow: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, errors.New("creating keyed nudge read shadow: durable source is nil")
	}
	snapshot, err := source.Snapshot(ctx, nudgeCommandStartupSnapshotLimit)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, ctxErr
	}
	if err != nil {
		return nil, fmt.Errorf("reading keyed nudge startup snapshot: %w", err)
	}
	index, err := nudgequeue.BuildCommandIndex(snapshot)
	if err != nil {
		return nil, fmt.Errorf("building keyed nudge startup index: %w", err)
	}
	reconciler, err := newNudgeKeyReconciler(index, snapshot.Store, pageLimit)
	if err != nil {
		return nil, err
	}
	return &nudgeKeyReadShadow{
		source:     source,
		index:      index,
		reconciler: reconciler,
		store:      snapshot.Store,
		warnings:   warnings,
		now:        time.Now,
		maxPages:   nudgeKeyReadMaxPagesPerCallback,
		sliceLimit: nudgeKeyReadMaxSlice,
	}, nil
}

// reconcile observes scheduling first, repairs an explicitly requested audit
// from a fresh complete snapshot, and follows stack-local page continuations
// only within a fixed slice. Remaining unchanged read-only work is forgotten:
// starting again after 10ms would be a treadmill rather than convergence.
func (r *nudgeKeyReadShadow) reconcile(ctx context.Context, key reconcilekey.Session, batch nudgeReconcileBatch) nudgeReconcileOutcome {
	if r == nil {
		return nudgeReconcileInvariant(errors.New("keyed nudge read shadow is nil"))
	}
	if ctx == nil {
		return nudgeReconcileInvariant(errors.New("keyed nudge read shadow context is nil"))
	}
	now := time.Now()
	if r.now != nil {
		now = r.now()
	}
	observeNudgeKeyScheduling(ctx, batch, now, r.warnings)
	if ctx.Err() != nil {
		return nudgeReconcileSuccess()
	}

	if batch.Causes&nudgeCauseAudit != 0 {
		completed, err := r.completeAudit(ctx)
		if ctx.Err() != nil {
			return nudgeReconcileSuccess()
		}
		if err != nil {
			var sourceFailure nudgeCommandSourceFailure
			if errors.As(err, &sourceFailure) {
				return r.sourceFailureOutcome(sourceFailure.err)
			}
			return nudgeReconcileInvariant(err)
		}
		if !completed {
			// A concurrent exact read advanced the projection. That is real
			// progress, so one follow-up audit is not an unchanged-page loop.
			return nudgeReconcileAudit()
		}
		r.auditRequired.Store(false)
	} else if r.auditRequired.Load() {
		return nudgeReconcileAudit()
	}

	started := now
	afterSequence := uint64(0)
	for pageNumber := 0; pageNumber < r.maxPages; pageNumber++ {
		result, err := r.reconciler.ReconcilePage(ctx, key, afterSequence)
		if ctx.Err() != nil {
			return nudgeReconcileSuccess()
		}
		if err != nil {
			return nudgeReconcileInvariant(err)
		}
		switch result.Disposition {
		case nudgeKeyPageAuditNeeded:
			return nudgeReconcileAudit()
		case nudgeKeyPageEvaluated:
			if !result.Continuation.Required {
				return nudgeReconcileSuccess()
			}
		default:
			return nudgeReconcileInvariant(fmt.Errorf("keyed nudge read shadow returned unknown page disposition %d", result.Disposition))
		}
		afterSequence = result.Continuation.AfterSequence
		current := time.Now()
		if r.now != nil {
			current = r.now()
		}
		if pageNumber+1 >= r.maxPages || current.Sub(started) >= r.sliceLimit {
			return nudgeReconcileSuccess()
		}
	}
	return nudgeReconcileSuccess()
}

func (r *nudgeKeyReadShadow) sourceFailureOutcome(err error) nudgeReconcileOutcome {
	classifier, ok := r.source.(nudgeCommandSourceErrorClassifier)
	if ok && classifier.ClassifyNudgeCommandSourceError(err) == nudgeCommandSourceErrorTransient {
		return nudgeReconcileTransient(err)
	}
	return nudgeReconcileInvariant(err)
}

type nudgeCommandSourceFailure struct {
	err error
}

func (e nudgeCommandSourceFailure) Error() string {
	return e.err.Error()
}

func (e nudgeCommandSourceFailure) Unwrap() error {
	return e.err
}

func (r *nudgeKeyReadShadow) completeAudit(ctx context.Context) (bool, error) {
	status := r.index.Status()
	snapshot, err := r.source.Snapshot(ctx, nudgeCommandStartupSnapshotLimit)
	if err != nil {
		return false, nudgeCommandSourceFailure{err: fmt.Errorf("reading keyed nudge audit snapshot: %w", err)}
	}
	if err := ctx.Err(); err != nil {
		return false, nudgeCommandSourceFailure{err: err}
	}
	if snapshot.Store != r.store {
		return false, fmt.Errorf("keyed nudge audit snapshot lineage %#v does not match installed lineage %#v", snapshot.Store, r.store)
	}
	completed, err := r.index.CompleteAudit(status.Revision, snapshot)
	if err != nil {
		return false, fmt.Errorf("installing keyed nudge audit snapshot: %w", err)
	}
	return completed, nil
}

// acceptCommandHint rereads durable authority by command ID only. The socket's
// hinted session is intentionally absent from this signature. The returned
// session is decoded from the verified repository record after lineage,
// identity, and watermark validation.
func (r *nudgeKeyReadShadow) acceptCommandHint(ctx context.Context, commandID string) (string, bool, error) {
	if r == nil {
		return "", false, errors.New("accepting keyed nudge command hint: read shadow is nil")
	}
	if ctx == nil {
		return "", false, errors.New("accepting keyed nudge command hint: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return "", false, err
	}
	resolution, err := r.source.Get(ctx, commandID)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return "", false, ctxErr
	}
	sourceUnsynced := errors.Is(err, nudgequeue.ErrCommandIndexUnsynced)
	if err != nil && !sourceUnsynced {
		return "", false, fmt.Errorf("rereading exact durable nudge command: %w", err)
	}
	if resolution.Store != r.store {
		return "", false, fmt.Errorf("exact durable nudge resolution lineage %#v does not match installed lineage %#v", resolution.Store, r.store)
	}
	if resolution.CompletedAuditRevision > resolution.Revision {
		return "", false, fmt.Errorf("exact durable nudge resolution audit revision %d exceeds revision %d", resolution.CompletedAuditRevision, resolution.Revision)
	}
	if !resolution.Found {
		return "", false, nil
	}
	facts, err := inspectNudgeKeyPageEntry(resolution.Entry)
	if err != nil {
		return "", false, fmt.Errorf("validating exact durable nudge command: %w", err)
	}
	entryCommandID := nudgeCommandEntryID(resolution.Entry)
	if entryCommandID != commandID {
		return "", false, fmt.Errorf("exact durable nudge command identity %q does not match requested identity", entryCommandID)
	}
	if facts.store != r.store {
		return "", false, fmt.Errorf("exact durable nudge command has foreign store lineage")
	}
	if facts.revision == 0 || facts.revision > resolution.Revision {
		return "", false, fmt.Errorf("exact durable nudge command revision %d is outside repository revision %d", facts.revision, resolution.Revision)
	}
	entry := resolution.Entry
	mutation := nudgequeue.CommandIndexMutation{
		Store:    r.store,
		Revision: facts.revision,
		Entry:    &entry,
	}
	if err := r.index.Apply(mutation); err != nil {
		if !errors.Is(err, nudgequeue.ErrCommandIndexUnsynced) {
			return "", false, fmt.Errorf("applying exact durable nudge command to index: %w", err)
		}
		r.auditRequired.Store(true)
	}
	if sourceUnsynced {
		r.auditRequired.Store(true)
	}
	return facts.sessionID, true, nil
}

func nudgeCommandEntryID(entry nudgequeue.CommandIndexEntry) string {
	if entry.Command != nil {
		return entry.Command.ID
	}
	if entry.Opaque != nil {
		return entry.Opaque.Routing.CommandID
	}
	return ""
}
