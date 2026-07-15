package main

import (
	"bytes"
	"container/list"
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"
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
	// for unchanged read-only work. A cursor retained below makes that follow-up
	// continue from the last completely evaluated page.
	nudgeKeyReadMaxSlice = 25 * time.Millisecond
	// nudgeKeyContinuationCapacity covers every distinct active key representable
	// by one complete repository snapshot while placing a hard ceiling on
	// advisory cursor memory. Eviction is safe: the next callback restarts that
	// key from sequence zero.
	nudgeKeyContinuationCapacity = nudgeCommandStartupSnapshotLimit
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
// run its explicit Provision/restore-anchor admission path before returning a
// read-only source. The path is never command-store identity; the opened
// repository snapshot supplies the sole binding. An adapter must wrap only
// positively retryable Provision/open failures with
// retryableNudgeCommandSourceFailure; unknown errors fail closed.
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
	source        nudgeCommandSource
	index         *nudgequeue.CommandIndex
	reconciler    *nudgeKeyReconciler
	store         nudgequeue.CommandStoreBinding
	startup       []string
	continuations *nudgeKeyContinuationCache
	warnings      *nudgeKeyObservationWarnings
	now           func() time.Time
	maxPages      int
	sliceLimit    time.Duration

	auditRequired      atomic.Bool
	auditRetryRequired atomic.Bool
}

type nudgeKeyContinuationEntry struct {
	key           reconcilekey.Session
	token         uint64
	afterSequence uint64
	element       *list.Element
}

// nudgeKeyContinuationCache is bounded, reconstructable advisory state. A
// token prevents an in-flight page walk from restoring a cursor after an exact
// update or audit reset made that walk stale.
type nudgeKeyContinuationCache struct {
	mu        sync.Mutex
	capacity  int
	nextToken uint64
	entries   map[reconcilekey.Session]*nudgeKeyContinuationEntry
	order     list.List
}

func newNudgeKeyContinuationCache(capacity int) *nudgeKeyContinuationCache {
	return &nudgeKeyContinuationCache{
		capacity: capacity,
		entries:  make(map[reconcilekey.Session]*nudgeKeyContinuationEntry, capacity),
	}
}

func (c *nudgeKeyContinuationCache) begin(key reconcilekey.Session) (uint64, uint64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry := c.entries[key]; entry != nil {
		c.order.MoveToBack(entry.element)
		return entry.token, entry.afterSequence
	}
	if len(c.entries) >= c.capacity {
		oldest := c.order.Front()
		if oldest != nil {
			entry := oldest.Value.(*nudgeKeyContinuationEntry)
			delete(c.entries, entry.key)
			c.order.Remove(oldest)
		}
	}
	c.nextToken++
	if c.nextToken == 0 {
		// Token wrap is practically unreachable, but clearing first preserves the
		// stale-writer fence without relying on that assumption.
		clear(c.entries)
		c.order.Init()
		c.nextToken = 1
	}
	entry := &nudgeKeyContinuationEntry{key: key, token: c.nextToken}
	entry.element = c.order.PushBack(entry)
	c.entries[key] = entry
	return entry.token, 0
}

func (c *nudgeKeyContinuationCache) advance(key reconcilekey.Session, token, afterSequence uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil || entry.token != token {
		return false
	}
	entry.afterSequence = afterSequence
	c.order.MoveToBack(entry.element)
	return true
}

func (c *nudgeKeyContinuationCache) finish(key reconcilekey.Session, token uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil || entry.token != token {
		return false
	}
	delete(c.entries, key)
	c.order.Remove(entry.element)
	return true
}

func (c *nudgeKeyContinuationCache) reset(key reconcilekey.Session) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry := c.entries[key]
	if entry == nil {
		return
	}
	delete(c.entries, key)
	c.order.Remove(entry.element)
}

func (c *nudgeKeyContinuationCache) resetAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	clear(c.entries)
	c.order.Init()
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
		return nil, newNudgeCommandSourceFailure(source, fmt.Errorf("reading keyed nudge startup snapshot: %w", err))
	}
	index, err := nudgequeue.BuildCommandIndex(snapshot)
	if err != nil {
		return nil, fmt.Errorf("building keyed nudge startup index: %w", err)
	}
	reconciler, err := newNudgeKeyReconciler(index, snapshot.Store, pageLimit)
	if err != nil {
		return nil, err
	}
	startup, err := activeNudgeCommandSessions(snapshot)
	if err != nil {
		return nil, fmt.Errorf("enumerating keyed nudge startup sessions: %w", err)
	}
	return &nudgeKeyReadShadow{
		source:        source,
		index:         index,
		reconciler:    reconciler,
		store:         snapshot.Store,
		startup:       startup,
		continuations: newNudgeKeyContinuationCache(nudgeKeyContinuationCapacity),
		warnings:      warnings,
		now:           time.Now,
		maxPages:      nudgeKeyReadMaxPagesPerCallback,
		sliceLimit:    nudgeKeyReadMaxSlice,
	}, nil
}

// reconcile observes scheduling first, repairs an explicitly requested audit
// from a fresh complete snapshot, and follows page continuations within a fixed
// slice. Remaining work retains only a bounded advisory cursor and returns the
// controller-owned Continue disposition.
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
				return r.sourceFailureOutcome(sourceFailure)
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
	token, afterSequence := r.continuations.begin(key)
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
			r.continuations.reset(key)
			return nudgeReconcileAudit()
		case nudgeKeyPageEvaluated:
			if !result.Continuation.Required {
				if r.continuations.finish(key, token) {
					return nudgeReconcileSuccess()
				}
				// An exact update or audit invalidated this walk while it was in
				// flight. Revisit from zero rather than acknowledging stale work.
				return nudgeReconcileContinue()
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
			if !r.continuations.advance(key, token, afterSequence) {
				// The cursor was reset by newer evidence. Continue is still needed,
				// but the next callback safely begins at zero.
				return nudgeReconcileContinue()
			}
			return nudgeReconcileContinue()
		}
	}
	return nudgeReconcileContinue()
}

func (r *nudgeKeyReadShadow) sourceFailureOutcome(failure nudgeCommandSourceFailure) nudgeReconcileOutcome {
	if failure.class == nudgeCommandSourceErrorTransient {
		return nudgeReconcileTransient(failure.err)
	}
	return nudgeReconcileInvariant(failure.err)
}

type nudgeCommandSourceFailure struct {
	err   error
	class nudgeCommandSourceErrorClass
}

func (e nudgeCommandSourceFailure) Error() string {
	return e.err.Error()
}

func (e nudgeCommandSourceFailure) Unwrap() error {
	return e.err
}

func newNudgeCommandSourceFailure(source nudgeCommandSource, err error) nudgeCommandSourceFailure {
	class := nudgeCommandSourceErrorInvariant
	if classifier, ok := source.(nudgeCommandSourceErrorClassifier); ok &&
		classifier.ClassifyNudgeCommandSourceError(err) == nudgeCommandSourceErrorTransient {
		class = nudgeCommandSourceErrorTransient
	}
	if errors.Is(err, nudgequeue.ErrRestoreAnchorBusy) ||
		errors.Is(err, nudgequeue.ErrRestoreAnchorConflict) ||
		errors.Is(err, nudgequeue.ErrRestoreAnchorDurabilityUncertain) {
		class = nudgeCommandSourceErrorTransient
	}
	return nudgeCommandSourceFailure{err: err, class: class}
}

func retryableNudgeCommandSourceFailure(err error) error {
	if err == nil {
		return errors.New("classifying retryable nudge command source failure: error is nil")
	}
	return nudgeCommandSourceFailure{err: err, class: nudgeCommandSourceErrorTransient}
}

func (r *nudgeKeyReadShadow) completeAudit(ctx context.Context) (bool, error) {
	_, completed, err := r.auditSnapshot(ctx)
	return completed, err
}

// auditSnapshot performs exactly one complete repository read, conditionally
// installs it, and returns the active keys discovered by that same read. It is
// shared by explicit repair and periodic anti-entropy so an interval never
// fans out into one snapshot per key.
func (r *nudgeKeyReadShadow) auditSnapshot(ctx context.Context) ([]string, bool, error) {
	status := r.index.Status()
	snapshot, err := r.source.Snapshot(ctx, nudgeCommandStartupSnapshotLimit)
	if err != nil {
		return nil, false, newNudgeCommandSourceFailure(r.source, fmt.Errorf("reading keyed nudge audit snapshot: %w", err))
	}
	if err := ctx.Err(); err != nil {
		return nil, false, newNudgeCommandSourceFailure(r.source, err)
	}
	if snapshot.Store != r.store {
		return nil, false, fmt.Errorf("keyed nudge audit snapshot lineage %#v does not match installed lineage %#v", snapshot.Store, r.store)
	}
	active, err := activeNudgeCommandSessions(snapshot)
	if err != nil {
		return nil, false, fmt.Errorf("enumerating keyed nudge audit sessions: %w", err)
	}
	completed, err := r.index.CompleteAudit(status.Revision, snapshot)
	if err != nil {
		return nil, false, fmt.Errorf("installing keyed nudge audit snapshot: %w", err)
	}
	if completed {
		r.continuations.resetAll()
		r.auditRequired.Store(false)
		r.auditRetryRequired.Store(false)
	} else {
		// A concurrent exact update won the compare-and-install edge. The
		// projection may be healthy, but this independent reconstruction was not
		// certified, so the global lifecycle retains a bounded retry explicitly.
		r.auditRetryRequired.Store(true)
	}
	return active, completed, nil
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
		return "", false, newNudgeCommandSourceFailure(r.source, fmt.Errorf("rereading exact durable nudge command: %w", err))
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
	current, currentErr := r.index.Resolve(commandID)
	if currentErr != nil && !errors.Is(currentErr, nudgequeue.ErrCommandIndexUnsynced) {
		return "", false, fmt.Errorf("resolving indexed exact durable nudge command: %w", currentErr)
	}
	if currentErr == nil && current.Found {
		currentFacts, err := inspectNudgeKeyPageEntry(current.Entry)
		if err != nil {
			return "", false, fmt.Errorf("validating indexed exact durable nudge command: %w", err)
		}
		if facts.revision < currentFacts.revision {
			return "", false, fmt.Errorf("exact durable nudge command revision %d rewinds indexed record revision %d", facts.revision, currentFacts.revision)
		}
		if facts.revision == currentFacts.revision {
			equal, err := equalNudgeCommandIndexEntry(current.Entry, entry)
			if err != nil {
				return "", false, fmt.Errorf("comparing exact durable nudge command replay: %w", err)
			}
			if !equal {
				return "", false, fmt.Errorf("exact durable nudge command conflicts with indexed content at revision %d", facts.revision)
			}
			if sourceUnsynced {
				r.auditRequired.Store(true)
				r.continuations.resetAll()
			}
			return facts.sessionID, true, nil
		}
	}
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
		r.continuations.resetAll()
	} else {
		key, keyErr := r.key(facts.sessionID)
		if keyErr != nil {
			return "", false, fmt.Errorf("resetting exact durable nudge continuation: %w", keyErr)
		}
		r.continuations.reset(key)
	}
	if sourceUnsynced {
		r.auditRequired.Store(true)
		r.continuations.resetAll()
	}
	return facts.sessionID, true, nil
}

func (r *nudgeKeyReadShadow) key(sessionID string) (reconcilekey.Session, error) {
	key, err := reconcilekey.NewSession(r.reconciler.keyScope, sessionID)
	if err != nil {
		return reconcilekey.Session{}, err
	}
	return key, nil
}

func equalNudgeCommandIndexEntry(left, right nudgequeue.CommandIndexEntry) (bool, error) {
	switch {
	case left.Command != nil && right.Command != nil:
		leftWire, err := nudgequeue.EncodeCommandV1(*left.Command)
		if err != nil {
			return false, err
		}
		rightWire, err := nudgequeue.EncodeCommandV1(*right.Command)
		if err != nil {
			return false, err
		}
		return bytes.Equal(leftWire, rightWire), nil
	case left.Opaque != nil && right.Opaque != nil:
		return left.Opaque.Version == right.Opaque.Version &&
			left.Opaque.Routing == right.Opaque.Routing &&
			bytes.Equal(left.Opaque.Raw, right.Opaque.Raw), nil
	default:
		return false, nil
	}
}

func activeNudgeCommandSessions(snapshot nudgequeue.CommandIndexSnapshot) ([]string, error) {
	// This read-only shadow is deliberately repository-wide. TrustedIngress
	// CityScope is authorization evidence, not a filter this projection may
	// interpret. Effect-owner cutover is forbidden until the production source
	// provides an authoritative city partition and a shared-store two-city test
	// proves that foreign session IDs cannot cross it.
	active := make(map[string]struct{})
	for position, entry := range snapshot.Entries {
		facts, err := inspectNudgeKeyPageEntry(entry)
		if err != nil {
			return nil, fmt.Errorf("snapshot entry %d: %w", position, err)
		}
		switch facts.state {
		case nudgequeue.CommandStatePending, nudgequeue.CommandStateInFlight, nudgequeue.CommandStateUpgradeRequired:
			active[facts.sessionID] = struct{}{}
		}
	}
	sessions := make([]string, 0, len(active))
	for sessionID := range active {
		sessions = append(sessions, sessionID)
	}
	sort.Strings(sessions)
	return sessions, nil
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
