package engine

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// maxPromptBytes caps a pool-mode do's rendered prompt. It mirrors the frozen
// 16 KiB journal payload budget (blueprint §3.1.3): the driver REFUSES an
// oversize prompt with a typed error at materialization rather than minting an
// event the store would reject downstream. A spec-ref indirection (prompt blob
// in the IR CAS) is the deferred escape hatch if dogfood hits the cap.
const maxPromptBytes = 16 * 1024

var (
	// ErrNoPoolRoute reports that a pool-mode do node's agent binding resolved to
	// no pool route (PoolRouter returned ok=false). It is a loud config error, not
	// a silent inline fallback — a do the caller declared pool-mode has nowhere to
	// run.
	ErrNoPoolRoute = errors.New("lumen: no pool route resolved for pool-mode do node")

	// ErrPromptTooLarge reports that a rendered do prompt exceeds the 16 KiB
	// payload cap (blueprint §3.1.3). It is raised at materialization, before any
	// append.
	ErrPromptTooLarge = errors.New("lumen: rendered do prompt exceeds the 16 KiB payload cap")

	// ErrAdvanceStalled reports that a pass left pending units with no pool-mode
	// work in flight — a run that cannot progress and cannot seal. Pool
	// settlements are the only async progress source, so a park with nothing in
	// flight is a bug (a dangling dep, a lowering error), surfaced loudly rather
	// than as a silent forever-park.
	//
	// UNREACHABLE by construction for the P4.2 node kinds, and kept as a defensive
	// guard: the pass walks units in topo order, and every ready unit that is not a
	// pool do settles synchronously in the same pass (engine-inline runs, silent
	// computes, skip-cascade settles). A unit therefore stays pending ONLY because
	// a dependency has not settled, and the sole dependency an in-pass walk cannot
	// settle is an in-flight pool node — so a pending unit always has a pool node in
	// flight (inFlight non-empty). If a future async node kind (async/await/detached
	// run) can leave a non-pool dependency unsettled across a pass, THIS is the
	// guard that must be widened to include it rather than silently forever-parking.
	ErrAdvanceStalled = errors.New("lumen: advance stalled with pending units and no pool work in flight")
)

// PoolWork is one pool-mode do activation the driver dispatched as an ordinary
// work bead in the city work store and is now awaiting a terminal close. The
// controller loop uses it to observe each dispatched bead (ObserveWork) and settle
// the fold from its ordinary close (REDESIGN §2.5).
type PoolWork struct {
	// Activation is the activation key (the dispatch handle).
	Activation string
	// NodeID is the do-node id.
	NodeID string
	// Route is the pool the work is routed to (the dispatched bead's gc.routed_to).
	Route string
	// Prompt is the rendered agent prompt (the dispatched bead's Description).
	Prompt string
	// BeadID is the dispatched work bead's store-minted id. The controller carries
	// it back into ObserveWork to settle the fold from the bead's ordinary close.
	BeadID string
}

// AdvanceResult reports one Advance pass. Exactly one of Sealed / Parked is true
// on a nil-error return.
type AdvanceResult struct {
	// Sealed is set when the run reached run.closed this pass (or was already
	// sealed on entry). Run is valid only then.
	Sealed bool
	// Parked is set when pending pool-mode work remains: the lease was released and
	// the run advances on the next Advance after a settlement lands.
	Parked bool
	// Run is the completed run's result — valid only when Sealed.
	Run RunResult
	// InFlight lists the materialized pool activations awaiting owned.settled, in
	// canonical activation order — valid when Parked.
	InFlight []PoolWork
	// Head is the journal head observed at return: the level-trigger cursor the
	// controller compares against on the next tick to decide whether to re-Advance.
	Head uint64
}

// Advance drives a run one re-entrant, level-triggered, parking pass — the
// asynchronous generalization of Resume (blueprint §2.1). It:
//
//  1. acquires the writer lease (LIVE epoch — every driver append it makes, and
//     every worker claim/settle threaded through CurrentLeaseEpoch, carries a
//     non-zero fencing token, never the permanently-fenced 0);
//  2. seeds run.started if the stream is fresh, else rebuilds run state from the
//     journal (the shared rebuildDriver core Resume uses), absorbing any
//     owned.settled a worker appended since the last Advance;
//  3. walks the units in topo order, where — unlike Run/Resume — a unit whose
//     dependencies have not all settled is DEFERRED (left for a later Advance)
//     instead of blocking, and a ready pool-mode do is DISPATCHED as an ordinary
//     work bead in the city work store and NOT waited on;
//  4. seals the run (run.closed) when every unit has settled, or PARKS (releasing
//     the lease) when pending pool-mode work remains.
//
// It is a pure function of (journal, IR, input): calling it repeatedly converges,
// and a crash mid-Advance + re-Advance re-derives only the missing facts. Two
// Advances never double-emit a pool-do node.activated — the append is idem-token
// write-once and the re-rendered payload is byte-identical over the stable
// settled scope, so a re-offer dedupes to a no-op. Run (P4.1) is unchanged and
// coexists: Advance is a peer driver over the SAME fold, journal, and vocabulary.
func Advance(ctx context.Context, store *graphstore.Store, doc *ir.IR, streamID string, input map[string]any, opts Options) (AdvanceResult, error) {
	if store == nil {
		return AdvanceResult{}, fmt.Errorf("lumen: advance: nil store")
	}
	if doc == nil {
		return AdvanceResult{}, fmt.Errorf("lumen: advance: nil IR document")
	}
	if streamID == "" {
		return AdvanceResult{}, fmt.Errorf("lumen: advance: empty stream id")
	}
	// The stream id IS the run root node id, and ':' is the activation-key
	// delimiter (activationNodeID strips the trailing ':index' segment). A
	// colon-bearing stream id would make the root frontier row's projected node_id
	// (activationNodeID(RootID)) disagree with the id applyRunStarted inserts,
	// diverging the root frontier row on a drop+refold. Refuse it loudly at entry
	// (LOW-2). Run derives its stream id from a hash and never contains ':'.
	if strings.ContainsRune(streamID, ':') {
		return AdvanceResult{}, fmt.Errorf("lumen: advance: stream id %q must not contain ':' (it is the run root node id; ':' is the activation-key delimiter)", streamID)
	}

	units, err := buildUnits(doc.Nodes, opts.Host != nil || opts.PoolRouter != nil, opts.Host != nil)
	if err != nil {
		return AdvanceResult{}, err
	}
	RegisterVocabulary(store)

	// The writer lease carries the LIVE fencing epoch for every append this
	// Advance makes, and fences a concurrent driver loudly. It is released on
	// return (park OR seal): between Advances the stream is unheld, so a pool
	// worker's claim/settle appends cooperatively at the released-but-preserved
	// epoch (correction #1).
	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return AdvanceResult{}, fmt.Errorf("lumen: advance: acquire writer lease %q: %w", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return AdvanceResult{}, err
	}

	var (
		d           *driver
		scope       map[string]string
		nodeOutputs map[string]string
	)
	if head == 0 {
		// Fresh run: seed run.started exactly as Run does (stamping ir/input hashes
		// so a later Advance's rebuild guard refuses a foreign doc/input). A crash
		// after this point re-enters through the rebuild path below.
		reducer := lumenReducer{}
		d = &driver{
			ctx:           ctx,
			store:         store,
			streamID:      streamID,
			irVer:         doc.Contract.Version,
			epoch:         lease.Epoch,
			reducer:       reducer,
			state:         reducer.Zero(streamID),
			host:          opts.Host,
			input:         input,
			snapshotEvery: opts.SnapshotEvery,
		}
		createdAt := time.Now().UTC().Format(time.RFC3339Nano)
		if err := d.append(EventRunStarted, streamID+":run:started", runStartedPayload{
			RootID:    streamID,
			Name:      doc.Name,
			IRHash:    irHash(doc),
			InputHash: inputHash(input),
			CreatedAt: createdAt,
		}); err != nil {
			return AdvanceResult{}, err
		}
		if err := d.crashAt(crashAfterRunStarted, streamID); err != nil {
			return AdvanceResult{}, err
		}
		scope = baseScope(input)
		nodeOutputs = map[string]string{}
	} else {
		// Re-entrant: rebuild state from the journal (the same restore core as
		// Resume), which folds in any owned.settled a worker appended since the last
		// Advance and reconciles the projection.
		d, scope, nodeOutputs, err = rebuildDriver(ctx, store, doc, streamID, input, lease.Epoch, opts)
		if err != nil {
			return AdvanceResult{}, err
		}
	}

	// Already sealed (an Advance of a finished run): idempotent no-op read. The
	// projection was reconciled inside rebuildDriver (H1), so this returns cleanly.
	if d.st().Closed {
		full, err := store.ReadStream(ctx, streamID, 1, 0)
		if err != nil {
			return AdvanceResult{}, fmt.Errorf("lumen: advance %q: read stream: %w", streamID, err)
		}
		return AdvanceResult{
			Sealed: true,
			Run:    RunResult{StreamID: streamID, Outcome: d.st().Outcome, NodeOutputs: nodeOutputs, Events: full},
			Head:   d.head,
		}, nil
	}

	// One parking pass over the topo-ordered units.
	for i := range units {
		if err := d.advanceUnit(units[i], scope, nodeOutputs, opts); err != nil {
			return AdvanceResult{}, err
		}
	}

	if d.allUnitsSettled(units) {
		// Seal: every unit that can settle has. run.closed freezes the run outcome
		// and clears the frontier (identical seal to Run/Resume).
		runOutcome := d.st().runOutcome()
		if err := d.maybeSnapshot(true); err != nil {
			return AdvanceResult{}, err
		}
		if err := d.crashAt(crashBeforeRunClosed, streamID); err != nil {
			return AdvanceResult{}, err
		}
		if err := d.append(EventRunClosed, streamID+":run:closed", runClosedPayload{Outcome: runOutcome}); err != nil {
			return AdvanceResult{}, err
		}
		events, err := store.ReadStream(ctx, streamID, 1, 0)
		if err != nil {
			return AdvanceResult{}, fmt.Errorf("lumen: advance %q: read stream: %w", streamID, err)
		}
		return AdvanceResult{
			Sealed: true,
			Run:    RunResult{StreamID: streamID, Outcome: runOutcome, NodeOutputs: nodeOutputs, Events: events},
			Head:   d.head,
		}, nil
	}

	// Park: pending units remain. They must be waiting on pool-mode work (the only
	// async settlement source); a stall with no in-flight pool work is a loud bug,
	// never a silent forever-park.
	inFlight := d.inFlightPoolWork()
	if len(inFlight) == 0 {
		return AdvanceResult{}, fmt.Errorf("%w: stream %q", ErrAdvanceStalled, streamID)
	}
	parkedHead, err := store.Head(ctx, streamID)
	if err != nil {
		return AdvanceResult{}, err
	}
	return AdvanceResult{Parked: true, InFlight: inFlight, Head: parkedHead}, nil
}

// advanceUnit drives one unit through Advance's parking cycle. It reloads an
// already-settled unit, evaluates a ready silent unit, DEFERS a unit whose deps
// have not settled, materializes a ready pool-mode do (without waiting), or runs
// a ready engine-inline unit through the SAME path a fresh Run does.
func (d *driver) advanceUnit(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	// Reload an already-settled unit (a pool node a worker closed, or an engine
	// node settled in a prior pass) plus the at-most-once effect memoization,
	// exactly as Resume does. On a fresh pass these maps are empty and this is a
	// no-op, so a not-yet-run unit falls through.
	handled, err := d.resumeMemoized(u, scope, nodeOutputs)
	if err != nil || handled {
		return err
	}

	if u.silent {
		// A pure lit/interp: compute its scope value once its deps have settled;
		// otherwise DEFER, so its {{ref}} interpolation sees the settled outputs on a
		// later pass. (Silent units emit no journal events; scope is re-derived each
		// Advance, matching Run/Resume.)
		if !d.depsSettled(u) {
			return nil
		}
		val, err := evalSilent(u.leaf, scope)
		if err != nil {
			return err
		}
		scope[u.nodeID] = val
		nodeOutputs[u.nodeID] = val
		return nil
	}

	// DEFER a unit whose dependencies have not all settled — it is neither ready
	// nor doomed this pass. A later Advance (after the pool settlement lands) sees
	// them settled and drives it. This is the sole new behavior over Run/Resume,
	// whose synchronous topo walk guarantees every dep settled before a unit.
	if !d.depsSettled(u) {
		return nil
	}

	// A pool-mode retry/repeat loop (a do body under a PoolRouter): drive its
	// park-aware attempt arm, which materializes ONE attempt at a time as claimable
	// pool work and parks between attempts. A blocked loop skip-cascades through
	// runUnit below (its attempts never materialize). An exec-bodied loop (no pool
	// materialization) falls through to runUnit → runLoop, which runs its attempts
	// inline in-pass exactly as Run does (Advance/Run parity).
	if u.kind == unitLoop && loopPoolMode(u, opts) && !d.blocked(u) {
		return d.advanceLoop(u, scope, nodeOutputs, opts)
	}

	// Materialize a READY pool-mode do as claimable work. A pool node whose
	// blocking dep failed is NOT offered to the pool — it skip-cascades through
	// runUnit below (blocked() settles it skipped), exactly like an engine node,
	// so a doomed activation never becomes claimable.
	if poolMode(u, opts) && !d.blocked(u) {
		// Real-bead path (REDESIGN §1.4): an already-dispatched, unsettled pool node
		// is OBSERVED — a terminal close settles it in THIS pass so dependents later
		// in the topo walk go ready immediately; a still-open bead parks; an observer
		// error is transient (the run stays parked, the loop retries next tick).
		if opts.ObserveWork != nil {
			if n := d.st().Nodes[u.activation]; n != nil && !n.Settled && n.BeadID != "" {
				return d.observePoolWork(u.activation, u.nodeID, n.BeadID, opts)
			}
		}
		return d.materializePoolWork(u, scope, opts)
	}

	// Engine-inline ready unit — OR a skip-cascading unit of any kind: run it
	// through the SAME path a fresh Run does (appendActivated, skip-cascade /
	// aggregate-skip, then run and settle) with the per-unit snapshot cadence. A
	// blocked pool do reaches here and settles skipped without touching the host.
	if err := d.runUnit(u, scope, nodeOutputs); err != nil {
		return err
	}
	return d.maybeSnapshot(false)
}

// materializePoolWork emits the pool-mode node.activated for a ready pool-mode do
// and dispatches its work bead, then does NOT wait: the session pool claims and
// closes the ordinary bead asynchronously; a later Advance observes the close and
// continues.
//
// A node already materialized in the fold state and not yet settled is a TRUE
// NO-OP for this pass — it is already dispatched (its bead id is recorded), and it
// must NOT be re-rendered or re-appended (HIGH-1). The write-once
// activation idem token assumes a byte-identical re-render, but a prompt {{ref}}
// to a node that is NOT a declared `after` dep renders DIFFERENTLY once that node
// settles; re-offering the same token with a divergent payload trips
// ErrIdemTokenReuse and wedges the driver permanently. Skipping the re-render
// makes re-Advance a true no-op for any in-flight pool node regardless of prompt
// determinism: the first-rendered prompt stands (carried in the folded n.Route /
// n.Prompt and reported via inFlightPoolWork), and no non-transient error class
// survives — every Advance re-run error is retryable. (An already-SETTLED node is
// intercepted earlier by resumeMemoized, so it never reaches here.)
func (d *driver) materializePoolWork(u planUnit, scope map[string]string, opts Options) error {
	if n := d.st().Nodes[u.activation]; n != nil && !n.Settled {
		// A node already carrying its dispatched bead id is a TRUE no-op (HIGH-1); a
		// node activated but not yet dispatched (a crash between the two appends,
		// §9.1) falls through to dispatch using the FOLD-recorded route/prompt
		// (byte-stable, so no divergent re-render).
		if n.BeadID != "" {
			return nil
		}
		return d.dispatchPoolWork(u.activation, u.nodeID, n.Route, n.Prompt, opts)
	}
	route, ok := opts.PoolRouter(u.leaf.agentRef)
	if !ok {
		return fmt.Errorf("%w: node %q (agent %q)", ErrNoPoolRoute, u.nodeID, u.leaf.agentRef)
	}
	prompt, err := renderPrompt(u.leaf.raw, scope)
	if err != nil {
		return fmt.Errorf("lumen: advance: do %q prompt: %w", u.nodeID, err)
	}
	if len(prompt) > maxPromptBytes {
		return fmt.Errorf("%w: node %q (%d bytes)", ErrPromptTooLarge, u.nodeID, len(prompt))
	}
	// Crash boundary (a): after the decide phase picked this pool do, before its
	// node.activated append. Test-only; inert in production (crashHook is nil).
	if err := d.crashAt(crashBeforeActivate, u.activation); err != nil {
		return err
	}
	if err := d.appendPoolActivated(u, route, prompt); err != nil {
		return err
	}
	return d.dispatchPoolWork(u.activation, u.nodeID, route, prompt, opts)
}

// dispatchPoolWork is the real-bead path's create+journal step (REDESIGN §1.4/§2.3):
// it calls the DispatchWork seam (lookup-then-create — a durable, metadata-findable
// side effect FIRST, the CAS-blob-before-append discipline), then appends the
// write-once owned.admitted{kind:work_bead, bead_id} dispatch fact. It passes the
// FOLD-recorded route/prompt so a re-Advance re-dispatches byte-identically. A crash
// between the create and the fact (crashAfterDispatch) leaves a findable bead the
// next Advance re-adopts (§9.1) — never an orphan, never a duplicate.
func (d *driver) dispatchPoolWork(activation, nodeID, route, prompt string, opts Options) error {
	if opts.DispatchWork == nil {
		// A pool-mode do (PoolRouter set) with no DispatchWork seam is a composition
		// error — there is nowhere to create the work bead. Loud, never a silent park.
		return fmt.Errorf("lumen: advance: pool-mode do %q has no DispatchWork seam (configuration error)", nodeID)
	}
	beadID, err := opts.DispatchWork(d.ctx, WorkDispatch{
		StreamID:   d.streamID,
		Activation: activation,
		NodeID:     nodeID,
		Route:      route,
		Prompt:     prompt,
		Attempt:    activationAttempt(activation),
	})
	if err != nil {
		return fmt.Errorf("lumen: advance: dispatch do %q work bead: %w", nodeID, err)
	}
	if beadID == "" {
		return fmt.Errorf("lumen: advance: dispatch do %q returned an empty bead id", nodeID)
	}
	if err := d.crashAt(crashAfterDispatch, activation); err != nil {
		return err
	}
	return d.append(EventOwnedAdmitted, "bead-dispatch:"+activation, ownedAdmittedPayload{
		Handle:     activation,
		Activation: activation,
		Kind:       OwnedKindWorkBead,
		BeadID:     beadID,
	})
}

// observePoolWork consults the ObserveWork seam for a dispatched-but-unsettled pool
// node and, when its real bead has reached a terminal close, copies the outcome into
// the journal as the EXISTING outcome.settled (REDESIGN §1.4) — zero new settle arm.
// Retryable is stamped true iff the outcome is failed, so the formula's retry arm
// re-attempts a genuine worker failure with a FRESH bead (§5). A still-open bead
// parks (nil, no append); an observer error is returned so the controller loop logs
// it and leaves the run parked to retry next tick (§9.7).
func (d *driver) observePoolWork(activation, nodeID, beadID string, opts Options) error {
	obs, err := opts.ObserveWork(d.ctx, beadID)
	if err != nil {
		return fmt.Errorf("lumen: advance: observe do %q bead %q: %w", nodeID, beadID, err)
	}
	if !obs.Terminal {
		return nil
	}
	return d.appendSettledRetryable(activation, obs.Outcome, obs.Output, "", obs.Outcome == OutcomeFailed)
}

// appendPoolActivated emits a pool-mode node.activated: the plain engine
// activation payload (node id, DAG edges, kind) plus the pool-dispatch fields
// (dispatch_mode=pool, route, prompt). Its idem token matches the
// engine-mode appendActivated (streamID:activation:act), and a given activation
// is either pool OR engine — never both — so there is no collision. This is
// reached only for the FIRST materialization of an activation: an already
// in-flight pool node is short-circuited to a no-op in materializePoolWork before
// any re-render, so a divergent re-render can never reach a duplicate append.
func (d *driver) appendPoolActivated(u planUnit, route, prompt string) error {
	return d.append(EventNodeActivated, d.streamID+":"+u.activation+":act", nodeActivatedPayload{
		NodeID:           u.nodeID,
		Activation:       u.activation,
		ParentActivation: u.parent,
		MemberIndex:      u.memberIndex,
		After:            u.afterDeps,
		Members:          u.memberDeps,
		Kind:             string(u.irKind),
		DispatchMode:     DispatchModePool,
		Route:            route,
		Prompt:           prompt,
	})
}

// depsSettled reports whether every one of u's dependencies (blocking `after`
// gates and drain members alike) has settled — the DEFER predicate. It reads the
// same afterDeps/memberDeps the topo sort and readiness fold use, so Advance's
// defer decision is consistent with Run's ordering and the reducer's ready().
func (d *driver) depsSettled(u planUnit) bool {
	st := d.st()
	for _, dep := range u.afterDeps {
		if _, settled := st.outcomeOf(dep); !settled {
			return false
		}
	}
	for _, m := range u.memberDeps {
		if _, settled := st.outcomeOf(m); !settled {
			return false
		}
	}
	return true
}

// poolMode reports whether u is a do leaf that Advance materializes as pool work
// rather than running inline — true exactly when a PoolRouter is configured.
// ZERO role names: the decision is a do-kind + config-seam test, not a role check.
func poolMode(u planUnit, opts Options) bool {
	return opts.PoolRouter != nil && u.kind == unitLeaf && u.leaf.kind == ir.NodeDo
}

// loopPoolMode reports whether u is a retry/repeat loop whose body is a pool-mode
// do — the case Advance drives through the park-aware advanceLoop arm. An
// exec-bodied loop (or a do-bodied loop with a Host and no PoolRouter) runs its
// attempts inline through runLoop instead.
func loopPoolMode(u planUnit, opts Options) bool {
	return opts.PoolRouter != nil && u.kind == unitLoop && u.loop != nil && u.loop.bodyIRKind == ir.NodeDo
}

// advanceLoop is Advance's park-aware attempt-loop arm (§4.1) for a pool-mode
// retry/repeat: it materializes ONE body attempt at a time as claimable pool work
// and PARKS on it, minting the next attempt only after the current one settles.
// Each pass either settles the loop, mints exactly one attempt, or leaves a live
// attempt in flight, so it is re-entrant and idempotent — a re-Advance with no new
// settlement re-derives the same decision and the write-once appends dedupe.
func (d *driver) advanceLoop(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.loop

	// Ensure the loop node.activated (write-once): dependents gate on the loop node,
	// and the seal condition keys on its settle.
	if err := d.ensureLoopActivated(u); err != nil {
		return err
	}

	// retry: evaluate the attempts budget ONCE. An invalid budget settles the loop
	// failed{invalid_input} with zero attempts (reference parity).
	maxAttempts := 0
	if spec.irKind == ir.NodeRetry {
		n, ok := evalAttempts(spec.attemptsExpr, d.loopScope(spec, 0, nil, nodeOutputs))
		if !ok {
			return d.settleLoop(u, OutcomeFailed, "", "invalid_input", nil, scope, nodeOutputs)
		}
		maxAttempts = n
	}

	// A live (materialized, unsettled) attempt is in flight — park on it. On the
	// real-bead path, OBSERVE it first: if its work bead has closed, settle the
	// attempt in-pass so the decide logic below runs THIS pass; otherwise park.
	if liveAtt, hasLive := d.liveAttempt(spec.bodyNodeID); hasLive {
		bodyAct := activationForAttempt(spec.bodyNodeID, liveAtt)
		bn := d.st().Nodes[bodyAct]
		if opts.ObserveWork == nil || bn == nil || bn.BeadID == "" {
			return nil
		}
		if err := d.observePoolWork(bodyAct, spec.bodyNodeID, bn.BeadID, opts); err != nil {
			return err
		}
		if bn := d.st().Nodes[bodyAct]; bn == nil || !bn.Settled {
			return nil // still in flight — park
		}
		// Settled this pass: fall through to the decide logic below.
	}

	settledAttempt, hasSettled := d.lastSettledAttempt(spec.bodyNodeID)
	if !hasSettled {
		// No attempt yet: mint attempt 0.
		return d.materializeLoopAttempt(u, 0, maxAttempts, scope, opts)
	}

	// The highest attempt has settled: decide (settle the loop OR re-attempt). The
	// decision is the shared closed-expression evaluation (loopDecide); it settles
	// the loop itself on a stop, and on a continue we mint the next pool attempt.
	bn := d.st().Nodes[activationForAttempt(spec.bodyNodeID, settledAttempt)]
	cont, err := d.loopDecide(u, settledAttempt, maxAttempts, bn, scope, nodeOutputs)
	if err != nil {
		return err
	}
	if !cont {
		return nil // loop settled inside loopDecide
	}
	return d.materializeLoopAttempt(u, settledAttempt+1, maxAttempts, scope, opts)
}

// ensureLoopActivated emits a loop node's node.activated once (write-once via the
// idem token; a fold-state check avoids a redundant append attempt).
func (d *driver) ensureLoopActivated(u planUnit) error {
	if n := d.st().Nodes[u.activation]; n != nil {
		return nil
	}
	if err := d.crashAt(crashBeforeActivate, u.activation); err != nil {
		return err
	}
	return d.appendActivated(u)
}

// materializeLoopAttempt mints one pool attempt: it emits attempt.minted, binds
// the 1-based iteration for a repeat body's prompt render (loop-local), and
// materializes the synthesized per-attempt do as ordinary pool work. The attempt
// is a NEW activation (bodyID:N) ⇒ a fresh work bead per attempt, so a fresh worker
// claims each attempt.
func (d *driver) materializeLoopAttempt(u planUnit, attempt, maxAttempts int, scope map[string]string, opts Options) error {
	spec := u.loop
	budget := lumenRepeatLoopCap
	if spec.irKind == ir.NodeRetry {
		budget = maxAttempts
	}
	remaining := budget - (attempt + 1)
	if remaining < 0 {
		remaining = 0
	}
	if err := d.appendAttemptMinted(u.activation, attempt, remaining); err != nil {
		return err
	}

	au := d.attemptUnit(u, attempt)
	restore, had := "", false
	if spec.irKind == ir.NodeRepeat {
		restore, had = scope[spec.iterationName]
		scope[spec.iterationName] = strconv.Itoa(attempt + 1)
	}
	err := d.materializePoolWork(au, scope, opts)
	if spec.irKind == ir.NodeRepeat {
		if had {
			scope[spec.iterationName] = restore
		} else {
			delete(scope, spec.iterationName)
		}
	}
	return err
}

// liveAttempt returns the highest-numbered activated-but-unsettled attempt of a
// loop body node id, and whether one exists — the park signal.
func (d *driver) liveAttempt(bodyNodeID string) (int, bool) {
	best, found := -1, false
	for act, n := range d.st().Nodes {
		if activationNodeID(act) != bodyNodeID || n.Settled {
			continue
		}
		if att := activationAttempt(act); att > best {
			best, found = att, true
		}
	}
	return best, found
}

// lastSettledAttempt returns the highest-numbered SETTLED attempt of a loop body
// node id (numeric, not lexical), and whether one exists — the decision anchor.
func (d *driver) lastSettledAttempt(bodyNodeID string) (int, bool) {
	best, found := -1, false
	for act, n := range d.st().Nodes {
		if activationNodeID(act) != bodyNodeID || !n.Settled {
			continue
		}
		if att := activationAttempt(act); att > best {
			best, found = att, true
		}
	}
	return best, found
}

// inFlightPoolWork returns, in canonical activation order, every pool-mode node
// that is materialized but not yet settled — the work the run is parked on.
func (d *driver) inFlightPoolWork() []PoolWork {
	st := d.st()
	var out []PoolWork
	for _, act := range st.activationKeys() {
		n := st.Nodes[act]
		if n.DispatchMode == DispatchModePool && !n.Settled {
			out = append(out, PoolWork{Activation: act, NodeID: n.NodeID, Route: n.Route, Prompt: n.Prompt, BeadID: n.BeadID})
		}
	}
	return out
}

// allUnitsSettled reports whether every non-silent top-level unit has settled —
// the seal condition. A skip-cascaded unit counts as settled (outcome skipped),
// and an engine-inline scatter/gather settles its aggregate inline in one pass,
// so its top-level activation settling implies its members/combine ran.
func (d *driver) allUnitsSettled(units []planUnit) bool {
	st := d.st()
	for i := range units {
		if units[i].silent {
			continue
		}
		n := st.Nodes[units[i].activation]
		if n == nil || !n.Settled {
			return false
		}
	}
	return true
}
