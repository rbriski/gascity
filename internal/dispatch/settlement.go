package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// SettlementEmittingKinds is the authoritative set of control-bead kinds whose
// ProcessControl handler emits a coarse settlement provenance event when it
// reaches a terminal settle. It is the SINGLE source of truth for which kinds
// need a SettlementEmitter constructed: the control dispatcher wires an emitter
// iff EmitsSettlement reports true (cmd_convoy_dispatch.go), and
// TestSettlementEmittingKindsTrackHandlers drives a real settling fixture
// through ProcessControl for every member to prove the set tracks handler
// reality. That closes the HIGH-1 class both ways — a kind wired for an emitter
// whose handler never emits (a wasted emitter), and an anchor added to a handler
// without wiring its kind (a nil emitter on a settling path) both fail the
// lockstep test.
//
// Membership rationale (see the handlers for the exact anchor sites):
//   - check / retry-eval — the separate-eval topology: the eval/check control
//     bead settles the logical retry/ralph step (ralph.go, retry.go).
//   - retry / ralph — the self-evaluating topology: the control bead settles
//     itself inline (control.go).
//   - workflow-finalize — settles the workflow root and the finalizer
//     (runtime.go), including the missing_root arm.
//
// fanout, drain, and scope-check never reach a root/attempt/workflow
// settlement, so they are deliberately absent.
var SettlementEmittingKinds = []string{
	beadmeta.KindCheck,
	beadmeta.KindRetryEval,
	beadmeta.KindRetry,
	beadmeta.KindRalph,
	beadmeta.KindWorkflowFinalize,
}

// EmitsSettlement reports whether a control bead of the given kind emits a coarse
// settlement provenance event, i.e. whether the control dispatcher must build a
// SettlementEmitter for it. Membership in SettlementEmittingKinds.
func EmitsSettlement(kind string) bool {
	return slices.Contains(SettlementEmittingKinds, kind)
}

// SettlementEmitter is the provenance seam the dispatcher calls AFTER a v2 root,
// attempt, or workflow reaches its terminal COLUMN state (the projection of
// record). It is PROVENANCE ONLY: an emit never alters bead state and an emit
// failure never fails the control action — the finalize/settle column write has
// already committed by the time the dispatcher reaches an Emit* call. A nil
// emitter (the default) is completely inert: a city without a graph journal
// behaves byte-identically to pre-P5.3, taking neither a journal open nor an
// append.
//
// Coarse means coarse (the cache-reconcile wisp-flood lesson): exactly one Emit*
// call per root/attempt/workflow SETTLEMENT — never per tick, never per
// bead.updated.
//
// The methods return an error only so a direct caller (the emitter's own tests)
// can observe the append; the dispatcher's emit helpers swallow-and-log it, so a
// journal failure can never propagate into the finalize/settle path.
type SettlementEmitter interface {
	// EmitRootSettled records that a workflow/molecule root reached its terminal
	// outcome.
	EmitRootSettled(ctx context.Context, engine string, s Settlement) error
	// EmitAttemptSettled records that a logical retry/ralph step settled after its
	// attempt(s).
	EmitAttemptSettled(ctx context.Context, engine string, s Settlement) error
	// EmitWorkflowFinalized records that the workflow-finalize control bead
	// completed (including the missing_root arm).
	EmitWorkflowFinalized(ctx context.Context, engine string, s Settlement) error
}

// Settlement carries the coarse facts of one settlement. Root is the molecule
// root (the per-root stream key); Bead is the settled bead (== Root for a root
// settlement); Outcome is the gc.outcome vocabulary value verbatim (pass | fail
// | missing_root | …); Attempt is set only for attempt settles. Kind and StoreRef
// are optional provenance the richer P5.4 anchors populate.
type Settlement struct {
	Root     string
	Bead     string
	Kind     string
	Outcome  string
	Attempt  int
	StoreRef string
}

// settlementAppendMaxAttempts bounds the emitter's optimistic-append retry on a
// contended per-root stream (two dispatcher goroutines settling different beads
// of one root can race the stream head). Mirrors controlFenceMaxAttempts.
const settlementAppendMaxAttempts = 8

// errSettlementContended reports the emitter exhausted its append-retry budget
// under perpetual head contention. Like any emit error it is swallowed-and-logged
// at the emission site — provenance never fails the work path.
var errSettlementContended = errors.New("settlement provenance: exhausted append retry budget under contention")

// settlementAfterHead is a test-only seam invoked between the emitter's
// StreamHead read and its CAS append. A test uses it to inject a competing append
// that steals the head, forcing the writer's CAS to miss and exercise the retry
// path deterministically. nil in production.
var settlementAfterHead func()

// --- dispatch-side emission (nil-safe, swallow-and-log) ---------------------

func (opts ProcessOptions) settleContext() context.Context {
	if opts.Context != nil {
		return opts.Context
	}
	return context.Background()
}

// emitRootSettled emits a coarse settlement.root provenance event after a v2
// workflow root's gc.outcome column write committed. Best-effort: a nil emitter
// or an empty root id is a no-op; an emit error is loud-logged and swallowed,
// never returned (provenance must not break finalize). The engine tag is v2 by
// construction — ProcessControl processes only graph.v2 control beads
// (runtime.go), so dispatch is definitionally the v2 provenance engine (no
// judgment, a fixed data fact); v1 emissions land in P5.4.
func (opts ProcessOptions) emitRootSettled(rootID, outcome string) {
	if opts.Settlements == nil || rootID == "" {
		return
	}
	s := Settlement{Root: rootID, Bead: rootID, Outcome: outcome}
	opts.reportSettlement("root", s, opts.Settlements.EmitRootSettled(opts.settleContext(), beads.SettlementEngineV2, s))
}

// emitAttemptSettled emits a coarse settlement.attempt event after a v2 logical
// retry/ralph bead settled. Same best-effort discipline as emitRootSettled.
func (opts ProcessOptions) emitAttemptSettled(rootID, logicalID, outcome string, attempt int) {
	if opts.Settlements == nil || rootID == "" {
		return
	}
	s := Settlement{Root: rootID, Bead: logicalID, Outcome: outcome, Attempt: attempt}
	opts.reportSettlement("attempt", s, opts.Settlements.EmitAttemptSettled(opts.settleContext(), beads.SettlementEngineV2, s))
}

// emitWorkflowFinalized emits a coarse settlement.workflow.finalized event after
// the workflow-finalize control bead completed. Same best-effort discipline.
func (opts ProcessOptions) emitWorkflowFinalized(rootID, finalizeBeadID, outcome string) {
	if opts.Settlements == nil || rootID == "" {
		return
	}
	s := Settlement{Root: rootID, Bead: finalizeBeadID, Outcome: outcome}
	opts.reportSettlement("workflow", s, opts.Settlements.EmitWorkflowFinalized(opts.settleContext(), beads.SettlementEngineV2, s))
}

// reportSettlement is the loud-but-non-fatal sink for an emit error: it is traced
// (opts.tracef) and slog-warned, then dropped. The projection-of-record column
// write already committed and is untouched; a missed provenance event is
// recoverable later by an idempotent re-emit (the outcome-scoped idem tokens make
// backfill safe). This is the documented narrow exception to don't-swallow-
// errors: the error IS surfaced (trace + warn), never silently eaten, and never
// propagated into the control path.
func (opts ProcessOptions) reportSettlement(kind string, s Settlement, err error) {
	if err == nil {
		return
	}
	opts.tracef("settlement-emit kind=%s root=%s bead=%s outcome=%s attempt=%d err=%v",
		kind, s.Root, s.Bead, s.Outcome, s.Attempt, err)
	slog.Warn("settlement provenance emit failed (dropped; projection of record unaffected)",
		"kind", kind, "root", s.Root, "bead", s.Bead, "outcome", s.Outcome, "attempt", s.Attempt, "err", err)
}

// --- concrete journal-backed emitter ----------------------------------------

// journalSettlementEmitter appends coarse settlement events to a city's shared
// graph journal via the AppendLog / ConditionalVersion CAS capabilities.
type journalSettlementEmitter struct {
	appendLog beads.AppendLogStore
	head      beads.ConditionalVersionStore
}

// NewJournalSettlementEmitter builds the concrete emitter over a city's graph
// journal store (typically cachedCityGraphJournal(cityPath)). It returns a nil
// SettlementEmitter — inert, byte-identical to pre-P5.3 — when journal is nil (the
// city has no .gc/graph scope). A non-nil journal that does not expose the
// append/CAS capabilities is a wiring bug, but provenance is best-effort: it logs
// once and stays inert rather than risking the work path.
func NewJournalSettlementEmitter(journal beads.Store) SettlementEmitter {
	if journal == nil {
		return nil
	}
	appendLog, okAppend := beads.AppendLogStoreFor(journal)
	head, okCAS := beads.ConditionalVersionStoreFor(journal)
	if !okAppend || !okCAS {
		slog.Warn("settlement provenance disabled: journal store lacks append/CAS capabilities",
			"append", okAppend, "cas", okCAS)
		return nil
	}
	return &journalSettlementEmitter{appendLog: appendLog, head: head}
}

func (e *journalSettlementEmitter) EmitRootSettled(ctx context.Context, engine string, s Settlement) error {
	return e.append(ctx, engine, beads.SettlementRootType, s)
}

func (e *journalSettlementEmitter) EmitAttemptSettled(ctx context.Context, engine string, s Settlement) error {
	return e.append(ctx, engine, beads.SettlementAttemptType, s)
}

func (e *journalSettlementEmitter) EmitWorkflowFinalized(ctx context.Context, engine string, s Settlement) error {
	return e.append(ctx, engine, beads.SettlementWorkflowFinalizedType, s)
}

// append commits one coarse settlement event to the per-root stream with a small
// bounded optimistic-CAS retry. Outcomes:
//   - fresh append succeeds → nil;
//   - a clean idempotent redo (byte-identical payload under the same idem token)
//     returns nil with nothing appended (R-IDEM dedupe = success/no-op);
//   - a contended head (a sibling settlement on the same root committed first)
//     re-reads the head and retries behind it (ErrWrongExpectedVersion is never
//     an error to the caller);
//   - a divergent idem-token reuse is treated as a no-op (never a hard failure)
//     but warned, since deterministic clock-free payloads should make it
//     impossible;
//   - any other store error, or budget exhaustion, propagates (and is swallowed-
//     and-logged at the emission site).
//
// Provenance-only: graphstore.Append does not fold, and no reducer is registered
// for the settlement.* types in P5.3, so a settlement append produces ZERO Tier-A
// (nodes/edges/frontier) deltas — the v1/v2 projection of record stays the
// mutation-primary columns.
//
// NOTE: the append is SYNCHRONOUS on the dispatch settle path, so a settlement
// can add up to the journal's busy_timeout of latency when the shared journal is
// under write contention. That bound is accepted as-is: the column write of
// record already committed, and any emit error (including a busy_timeout) is
// swallowed-and-logged at the emission site, so this never blocks or fails the
// control action beyond that bounded wait.
func (e *journalSettlementEmitter) append(ctx context.Context, engine, typ string, s Settlement) error {
	streamID := beads.SettlementStreamID(s.Root)
	ev, err := beads.SettlementEvent(typ, beads.SettlementPayload{
		Root:     s.Root,
		Bead:     s.Bead,
		Kind:     s.Kind,
		Outcome:  s.Outcome,
		Attempt:  s.Attempt,
		StoreRef: s.StoreRef,
	})
	if err != nil {
		return fmt.Errorf("building %s event: %w", typ, err)
	}
	for attempt := 0; attempt < settlementAppendMaxAttempts; attempt++ {
		head, err := e.head.StreamHead(ctx, streamID)
		if err != nil {
			return fmt.Errorf("reading settlement stream head %s: %w", streamID, err)
		}
		if settlementAfterHead != nil {
			settlementAfterHead()
		}
		_, err = e.appendLog.AppendEvent(ctx, streamID, engine, head, 0, []graphstore.JournalEvent{ev})
		if err == nil {
			return nil
		}
		if errors.Is(err, graphstore.ErrWrongExpectedVersion) {
			continue
		}
		if errors.Is(err, graphstore.ErrIdemTokenReuse) {
			slog.Warn("settlement provenance idem-token reuse treated as no-op",
				"stream", streamID, "type", typ, "err", err)
			return nil
		}
		return fmt.Errorf("appending %s to %s: %w", typ, streamID, err)
	}
	return fmt.Errorf("appending %s to %s: %w", typ, streamID, errSettlementContended)
}
