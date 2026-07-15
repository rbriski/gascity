package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
)

// nudgeCommandPager is the read-only projection seam used by the keyed nudge
// reconciler. *nudgequeue.CommandIndex is the production implementation.
type nudgeCommandPager interface {
	Page(sessionID string, afterSequence uint64, limit int) (nudgequeue.CommandIndexPage, error)
}

type nudgeKeyPageDisposition uint8

const (
	nudgeKeyPageEvaluated nudgeKeyPageDisposition = iota + 1
	nudgeKeyPageAuditNeeded
)

type nudgeKeyPageContinuation struct {
	Required      bool
	AfterSequence uint64
}

// nudgeKeyReconcilePageResult contains no raw user-controlled identity or
// content. Its revision and sequence cursors are trace/scheduling facts and
// must not become metric labels.
type nudgeKeyReconcilePageResult struct {
	Disposition            nudgeKeyPageDisposition
	Revision               uint64
	CompletedAuditRevision uint64
	Evaluated              int
	Pending                int
	InFlight               int
	KnownUpgradeRequired   int
	OpaqueUpgradeRequired  int
	FirstSequence          uint64
	LastSequence           uint64
	Continuation           nudgeKeyPageContinuation
}

// nudgeKeyReconciler evaluates the latest bounded command page for one exact
// stable session key. It owns no store writer, runtime, worker, or provider.
type nudgeKeyReconciler struct {
	pager     nudgeCommandPager
	keyScope  string
	store     nudgequeue.CommandStoreBinding
	pageLimit int
}

func newNudgeKeyReconciler(pager nudgeCommandPager, store nudgequeue.CommandStoreBinding, pageLimit int) (*nudgeKeyReconciler, error) {
	if pager == nil {
		return nil, errors.New("creating keyed nudge reconciler: command pager is nil")
	}
	if err := nudgequeue.ValidateCommandStoreBinding(store); err != nil {
		return nil, errors.New("creating keyed nudge reconciler: command store lineage is invalid")
	}
	if pageLimit < 1 || pageLimit > nudgequeue.MaxCommandIndexPageSize {
		return nil, fmt.Errorf("creating keyed nudge reconciler: page limit %d is outside [1,%d]", pageLimit, nudgequeue.MaxCommandIndexPageSize)
	}
	return &nudgeKeyReconciler{
		pager:     pager,
		keyScope:  nudgeCommandReconcileScope(store),
		store:     store,
		pageLimit: pageLimit,
	}, nil
}

func nudgeCommandReconcileScope(store nudgequeue.CommandStoreBinding) string {
	return fmt.Sprintf("command-store/v1/%d:%s/%d", len(store.StoreUUID), store.StoreUUID, store.RestoreEpoch)
}

// Reconcile is the queue callback entry: it always starts from the first
// active command in the latest projection. A worker may follow the returned
// continuation only inside its bounded slice. After durable progress it
// requeues identity alone and starts from zero again, so no cursor becomes
// queued truth.
func (r *nudgeKeyReconciler) Reconcile(ctx context.Context, key reconcilekey.Session) (nudgeKeyReconcilePageResult, error) {
	return r.ReconcilePage(ctx, key, 0)
}

// ReconcilePage evaluates one explicit continuation page without performing
// effects. Every call rereads the current immutable projection.
func (r *nudgeKeyReconciler) ReconcilePage(ctx context.Context, key reconcilekey.Session, afterSequence uint64) (nudgeKeyReconcilePageResult, error) {
	if r == nil {
		return nudgeKeyReconcilePageResult{}, errors.New("reconciling keyed nudge page: reconciler is nil")
	}
	if ctx == nil {
		return nudgeKeyReconcilePageResult{}, errors.New("reconciling keyed nudge page: context is nil")
	}
	if err := ctx.Err(); err != nil {
		return nudgeKeyReconcilePageResult{}, err
	}
	if key.IsZero() {
		return nudgeKeyReconcilePageResult{}, errors.New("reconciling keyed nudge page: key is zero")
	}
	if key.StoreID() != r.keyScope {
		return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: key scope %q does not match installed scope", key.StoreID())
	}

	page, err := r.pager.Page(key.SessionID(), afterSequence, r.pageLimit)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nudgeKeyReconcilePageResult{}, ctxErr
	}
	if err != nil {
		if errors.Is(err, nudgequeue.ErrCommandIndexUnsynced) {
			return nudgeKeyReconcilePageResult{
				Disposition: nudgeKeyPageAuditNeeded,
			}, nil
		}
		return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: %w", err)
	}
	if page.Store != r.store {
		return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: projection lineage %#v does not match installed lineage %#v", page.Store, r.store)
	}
	if page.CompletedAuditRevision > page.Revision {
		return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: completed audit revision %d exceeds projection revision %d", page.CompletedAuditRevision, page.Revision)
	}
	if len(page.Entries) > r.pageLimit {
		return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: projection returned %d entries above limit %d", len(page.Entries), r.pageLimit)
	}

	result := nudgeKeyReconcilePageResult{
		Disposition:            nudgeKeyPageEvaluated,
		Revision:               page.Revision,
		CompletedAuditRevision: page.CompletedAuditRevision,
	}
	previousSequence := afterSequence
	barrierSeen := false
	for position, entry := range page.Entries {
		if err := ctx.Err(); err != nil {
			return nudgeKeyReconcilePageResult{}, err
		}
		if barrierSeen {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d follows an upgrade barrier", position)
		}
		facts, err := inspectNudgeKeyPageEntry(entry)
		if err != nil {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d: %w", position, err)
		}
		if facts.store != r.store {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d has foreign store lineage", position)
		}
		if facts.sessionID != key.SessionID() {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d has foreign target", position)
		}
		if facts.revision == 0 || facts.revision > page.Revision {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d revision %d is outside page revision %d", position, facts.revision, page.Revision)
		}
		if facts.sequence <= previousSequence {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d sequence %d does not advance %d", position, facts.sequence, previousSequence)
		}

		switch facts.state {
		case nudgequeue.CommandStatePending:
			result.Pending++
		case nudgequeue.CommandStateInFlight:
			result.InFlight++
		case nudgequeue.CommandStateUpgradeRequired:
			if facts.opaque {
				result.OpaqueUpgradeRequired++
			} else {
				result.KnownUpgradeRequired++
			}
			barrierSeen = true
		default:
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: entry at position %d has non-active state %q", position, facts.state)
		}

		if result.Evaluated == 0 {
			result.FirstSequence = facts.sequence
		}
		result.Evaluated++
		result.LastSequence = facts.sequence
		previousSequence = facts.sequence
	}

	if page.NextAfterSequence != 0 {
		if barrierSeen {
			return nudgeKeyReconcilePageResult{}, errors.New("reconciling keyed nudge page: continuation crosses an upgrade barrier")
		}
		if result.Evaluated == 0 {
			return nudgeKeyReconcilePageResult{}, errors.New("reconciling keyed nudge page: continuation exists without a command")
		}
		if result.Evaluated != r.pageLimit {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: continuation exists on short page of %d below limit %d", result.Evaluated, r.pageLimit)
		}
		if page.NextAfterSequence != result.LastSequence {
			return nudgeKeyReconcilePageResult{}, fmt.Errorf("reconciling keyed nudge page: continuation %d does not match last sequence %d", page.NextAfterSequence, result.LastSequence)
		}
		result.Continuation = nudgeKeyPageContinuation{
			Required:      true,
			AfterSequence: page.NextAfterSequence,
		}
	}
	return result, nil
}

type nudgeKeyPageEntryFacts struct {
	store     nudgequeue.CommandStoreBinding
	sessionID string
	sequence  uint64
	revision  uint64
	state     nudgequeue.CommandState
	opaque    bool
}

func inspectNudgeKeyPageEntry(entry nudgequeue.CommandIndexEntry) (nudgeKeyPageEntryFacts, error) {
	hasCommand := entry.Command != nil
	hasOpaque := entry.Opaque != nil
	if hasCommand == hasOpaque {
		return nudgeKeyPageEntryFacts{}, errors.New("expected exactly one known or opaque command")
	}
	if hasCommand {
		return nudgeKeyPageEntryFacts{
			store:     entry.Command.Store,
			sessionID: entry.Command.Target.SessionID,
			sequence:  entry.Command.Order.Sequence,
			revision:  entry.Command.Order.Revision,
			state:     entry.Command.State,
		}, nil
	}
	if entry.Opaque.Version <= nudgequeue.CommandVersion1 {
		return nudgeKeyPageEntryFacts{}, fmt.Errorf("opaque command version %d is not newer than %d", entry.Opaque.Version, nudgequeue.CommandVersion1)
	}

	return nudgeKeyPageEntryFacts{
		store:     entry.Opaque.Routing.Store,
		sessionID: entry.Opaque.Routing.TargetSessionID,
		sequence:  entry.Opaque.Routing.Sequence,
		revision:  entry.Opaque.Routing.Revision,
		state:     nudgequeue.CommandStateUpgradeRequired,
		opaque:    true,
	}, nil
}
