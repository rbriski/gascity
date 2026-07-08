package dispatch

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// controlFenceMaxAttempts bounds the optimistic-retry loop in fenceControlWrite.
// Each iteration is one lost CAS (a competing writer acquired the serialization
// slot first); the loop re-reads the fresh head and re-appends behind them. The
// bound exists only to convert a pathological live-lock — many writers churning
// one control bead's fence stream indefinitely — into a transient, retryable
// signal instead of an unbounded spin. In-process writers are already serialized
// by fenceLocks (below), so a CAS miss only arises cross-process, where at most
// one other controller contends a given control bead; a budget of 8 is far above
// the expected worst case of 1 retry while still terminating a runaway.
const controlFenceMaxAttempts = 8

// errControlFenceContended reports that the optimistic-retry loop exhausted its
// budget: fenceControlWrite could not acquire the per-bead serialization slot
// within controlFenceMaxAttempts because concurrent writers kept winning the
// CAS. It is classified TRANSIENT (IsTransientControllerError) so the dispatcher
// re-dispatches the control bead rather than closing its workflow — a CAS loser
// must never kill the workflow.
var errControlFenceContended = errors.New("control-epoch fence: exhausted retry budget under contention")

// errControlFenceUncapped reports the MED wiring bug: the bead id is
// journal-resident (IsJournalResidentID) but the store handed to the fence does
// not expose the journal CAS capabilities, so the write cannot be serialized.
// This means a wrapper on the store path dropped the AppendLogHandle /
// ConditionalVersionHandle forward. The fence refuses to silently degrade to an
// unfenced write; it fails LOUD. Classified TRANSIENT so the workflow survives
// (a deploy that restores the forward lets the retry succeed) rather than being
// closed on an infrastructure defect.
var errControlFenceUncapped = errors.New("control-epoch fence: journal-resident bead but store lacks journal CAS capabilities (wiring bug)")

// fenceLocks serializes the fence's acquire → decide → write critical section
// per bead id WITHIN this process, on BOTH branches (journal-resident and
// legacy). It is load-bearing for correctness, not an optimization: the epoch
// record lives in separate gc.control_epoch metadata, so two in-process writers
// that both won their own serialization slot would otherwise still race their
// read-compare-write on that metadata — the very lost-update S0.4 names. The
// singleton control dispatcher makes in-process the operative concurrency, so
// this mutex is what actually serializes decideAndWrite and kills the staggered
// regression. Cross-process, the durable conflict detector makes a loser retry
// rather than clobber: on the journal branch the append CAS, and on both branches
// (P5.2) the epoch write's own store-level SetMetadataIf CAS via
// beads.SetMetadataConditionally. That store-level CAS is SQL-conditioned in every
// production store (BdStore/NativeDoltStore's guarded UPDATE, and the JournalStore's
// conditional UPDATE/INSERT — never a Go-side read-then-write), so the guarantee
// holds on the hosted Postgres backend, not just on SQLite's single-writer
// serialization. The former "cross-process metadata race remains a P2 limitation"
// is now closed — a cross-process metadata CAS miss surfaces as
// ErrMetadataCASConflict and both branches re-decide behind the winner.
var fenceLocks keyedMutex

// fenceAfterHead is a test-only seam invoked between the fence's StreamHead read
// and its CAS append, on every loop iteration. A test uses it to inject a
// competing append that steals the head, forcing the writer's CAS to miss and
// exercise the retry path deterministically. nil in production.
var fenceAfterHead func()

// fenceControlWrite serializes an epoch-fenced control write against a
// journal-resident control bead. It replaces the S0.4 check-then-act (read
// gc.control_epoch → compare → SetMetadata, where every reader saw the same
// stale epoch and the last writer silently won) with a mutually-exclusive
// decide-then-write over the bead's per-bead control-epoch fence stream:
//
//	take the per-bead in-process lock → acquire the serialization slot (CAS
//	append at the current head) → run decideAndWrite, which re-reads the epoch
//	and re-checks its precondition against post-serialization state.
//
// The decision lives inside decideAndWrite precisely so it runs AFTER the lock
// and the acquisition: each adopting site re-reads gc.control_epoch and
// re-evaluates its guard once it holds the section, so a writer that lost the
// race to a fresher epoch observes the advanced value and no-ops instead of
// regressing it. That is what kills both the silent lost update (S0.4 BLOCKER)
// and the staggered regression (P2.3 HIGH). The in-process lock (fenceLocks) is
// what makes "post-serialization state" true for the realistic singleton-
// dispatcher concurrency; the CAS append is the durable ordering token.
//
// Loser handling: a cross-process CAS conflict (graphstore.ErrWrongExpectedVersion)
// is NOT an error to the caller — the loser re-reads the head and retries behind
// the winner. Only a genuine non-conflict store error propagates, and budget
// exhaustion returns a TRANSIENT-classified error. No path here returns
// molecule.ErrEpochConflict, so a fenced write can never reach
// markControllerSpawnError's hard branch and close the workflow.
//
// The fence is TOTAL: no store class silently loses an epoch update.
//   - Journal-resident bead → the append-CAS branch below (P2.3).
//   - Legacy bead on a ConditionalMetadataStore-capable store → the store-level
//     SetMetadataIf CAS loop (fenceLegacyControlWrite, P5.2), structurally
//     identical to the journal branch.
//   - Legacy bead on a store with NEITHER CAS capability → the ONLY remaining
//     non-loud path: today's raw check-then-act (byte-identical, documented in
//     fenceLegacyControlWrite). Not expected in production.
//
// A journal-resident id whose store lacks the journal CAS capabilities is a
// wiring bug (a wrapper dropped the forward): the fence fails LOUD with
// errControlFenceUncapped rather than silently writing unfenced.
//
// The append and decideAndWrite's epoch write are two transactions. A crash
// between them leaves the fence event committed with the epoch metadata still
// stale; this is safe because decideAndWrite is idempotent-redo by construction
// (it re-reads and no-ops when the epoch already advanced). The redo appends a
// harmless second fence event. The deferred "collapse the append and the epoch
// record into one journal transaction" is SUPERSEDED by P5.2, not implemented:
// decideAndWrite now routes its epoch write through beads.SetMetadataConditionally
// (a store-level SetMetadataIf CAS), which is exactly the conditional-write
// property the one-txn fold was for — so the journal branch also retries a lost
// cross-process metadata CAS (below), closing the P2.3 cross-process metadata-
// record residual. The fence stream remains the in-process ordering token.
func fenceControlWrite(ctx context.Context, store beads.Store, beadID string, decideAndWrite func(context.Context) error) error {
	if !beads.IsJournalResidentID(beadID) {
		return fenceLegacyControlWrite(ctx, store, beadID, decideAndWrite)
	}

	appendLog, okAppend := beads.AppendLogStoreFor(store)
	casReader, okCAS := beads.ConditionalVersionStoreFor(store)
	if !okAppend || !okCAS {
		// MED: journal-resident but the CAS caps did not reach this store — a
		// wrapper dropped the AppendLogHandle / ConditionalVersionHandle
		// forward. Refuse to process unfenced; fail loud (transient).
		return fmt.Errorf("%w: %s (append=%v cas=%v)", errControlFenceUncapped, beadID, okAppend, okCAS)
	}

	// Serialize the whole acquire → decide → write section per bead within this
	// process, so decideAndWrite's read-compare-write cannot interleave with a
	// concurrent in-process writer's (see fenceLocks).
	unlock := fenceLocks.lock(beadID)
	defer unlock()

	streamID := beads.ControlEpochFenceStreamID(beadID)
	for attempt := 0; attempt < controlFenceMaxAttempts; attempt++ {
		head, err := casReader.StreamHead(ctx, streamID)
		if err != nil {
			return fmt.Errorf("control-epoch fence: reading stream head for %s: %w", beadID, err)
		}
		if fenceAfterHead != nil {
			fenceAfterHead()
		}
		_, err = appendLog.AppendEvent(ctx, streamID, beads.ControlFenceEngine, head, 0,
			[]graphstore.JournalEvent{beads.ControlEpochFenceEvent(beadID)})
		if err != nil {
			if errors.Is(err, graphstore.ErrWrongExpectedVersion) {
				// A competing (cross-process) writer acquired the slot first.
				// Re-read the head and try again behind them; never error out.
				continue
			}
			// A genuine store error (e.g. graphstore.ErrBusy) — propagate it so
			// the caller's transient classifier can decide.
			return fmt.Errorf("control-epoch fence: appending for %s: %w", beadID, err)
		}
		// We hold the serialization slot. Decide and write against
		// post-serialization state — decideAndWrite re-reads the epoch and
		// CAS-writes it. A cross-process writer that committed its epoch metadata
		// between our re-read and CAS makes the write a loud ErrMetadataCASConflict;
		// re-acquire the slot and re-decide (closing the P2.3 two-txn metadata-
		// record residual). Any other outcome — nil, a no-op, or a genuine store
		// error — returns.
		if err := decideAndWrite(ctx); err != nil {
			if errors.Is(err, beads.ErrMetadataCASConflict) {
				continue
			}
			return err
		}
		return nil
	}
	return fmt.Errorf("control-epoch fence for %s: %w", beadID, errControlFenceContended)
}

// fenceLegacyControlWrite serializes an epoch-fenced control write against a
// NON-journal-resident (legacy v1/v2) control bead. It is the P5.2 kill for the
// S0.4 lost update on the legacy path: where the pre-P5 fence ran decideAndWrite
// once, unserialized and silent, this runs the same lock + bounded optimistic
// retry + decide-inside-the-fence shape as the journal branch, only with the
// store-level SetMetadataIf CAS (beads.SetMetadataConditionally, invoked inside
// decideAndWrite) standing in for the journal append CAS.
//
//   - Capable legacy store (both production legacy stores — BdStore and
//     NativeDoltStore — qualify): take the per-bead in-process lock (the same
//     fenceLocks the journal branch uses, so two in-process writers serialize and
//     the second re-reads the winner's epoch and no-ops), then loop: run
//     decideAndWrite, which re-reads the epoch, re-checks its precondition, and
//     CAS-writes conditioned on the value it just observed. A cross-process writer
//     that advanced the epoch first makes the CAS a loud ErrMetadataCASConflict;
//     re-run the decision (re-read → converge, never clobber, never regress).
//     Budget exhaustion returns the TRANSIENT errControlFenceContended, so a CAS
//     loser re-dispatches rather than closing its workflow — never a hard
//     controller error.
//   - Store WITHOUT the ConditionalMetadataStore capability: the ONLY remaining
//     non-loud path. There is no way to detect a cross-process lost update, so the
//     fence degrades to today's exact raw check-then-act — no lock, no loop, one
//     unconditional decideAndWrite — byte-identical to the pre-P5 legacy path. In
//     production this is the exec provider (internal/beads/exec), which has no CAS
//     verb: an exec:-provider city — a supported config, notably this fork's T3/exec
//     integration mission — takes this honest degrade for its legacy control writes.
//     It is a genuine capability gap, not a wiring bug (unlike a journal-resident
//     bead missing its journal caps, which fails loud); a test double reaches it the
//     same way. Extending an exec-contract CAS verb so exec cities also get the S0.4
//     kill for legacy control writes is a documented follow-up.
func fenceLegacyControlWrite(ctx context.Context, store beads.Store, beadID string, decideAndWrite func(context.Context) error) error {
	if _, ok := beads.ConditionalMetadataStoreFor(store); !ok {
		return decideAndWrite(ctx)
	}

	unlock := fenceLocks.lock(beadID)
	defer unlock()
	for attempt := 0; attempt < controlFenceMaxAttempts; attempt++ {
		err := decideAndWrite(ctx)
		if !errors.Is(err, beads.ErrMetadataCASConflict) {
			// nil, a no-op, or a genuine store error — the CAS held, wrote a no-op,
			// or never fired. The decision has settled.
			return err
		}
		// The store-level CAS lost to a cross-process writer that advanced the epoch
		// first. Re-decide behind them: the closure re-reads the winner's value and
		// either no-ops (regression kill) or targets a still-higher epoch.
	}
	return fmt.Errorf("control-epoch fence for %s: %w", beadID, errControlFenceContended)
}

// keyedMutex hands out a mutex per string key and reclaims it when no goroutine
// holds or waits on it, so the map stays bounded over a long-running process.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	mu  sync.Mutex
	ref int
}

// lock acquires the mutex for key and returns its release function. The returned
// func must be called exactly once (defer it).
func (k *keyedMutex) lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = make(map[string]*refMutex)
	}
	m := k.locks[key]
	if m == nil {
		m = &refMutex{}
		k.locks[key] = m
	}
	m.ref++
	k.mu.Unlock()

	m.mu.Lock()
	return func() {
		m.mu.Unlock()
		k.mu.Lock()
		m.ref--
		if m.ref == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}
