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

	units, err := buildUnits(doc, opts.Host != nil || opts.PoolRouter != nil, opts.Host != nil)
	if err != nil {
		return AdvanceResult{}, err
	}
	RegisterVocabulary(store)

	// The writer lease carries the LIVE fencing epoch for every append this
	// Advance makes, and fences a concurrent driver loudly. It is released on
	// return (park OR seal): between Advances the stream is unheld, so a pool
	// worker's claim/settle appends cooperatively at the released-but-preserved
	// epoch (correction #1).
	lease, err := store.AcquireWriterLease(ctx, streamID, resolveLeaseHolder(opts), leaseTTL)
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
	// Index the run sub-formula env specs for scopeFor (identical on fresh + rebuild,
	// since units is a pure function of the doc — keeps the sub-scope deterministic).
	d.runEnvs = runEnvIndex(units)

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
		view, err := d.scopeFor(u.ns, scope)
		if err != nil {
			return err
		}
		val, err := evalSilent(u.leaf, view)
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
	//
	// A repeat RUN body (bodyRun != nil) ALSO routes here — in BOTH pool and inline
	// modes (⚑B2): loopPoolMode keys bodyIRKind==NodeDo, so a run body would otherwise
	// fall through to runUnit → runLoop → attemptUnit's nil-leaf host error. advanceLoop
	// dispatches to its run-body arm, which drives each attempt's inlined sub-graph
	// through advanceUnit (parking on any in-flight sub-do, settling exec/settle subs
	// in-pass).
	if u.kind == unitLoop && !d.blocked(u) && (loopPoolMode(u, opts) || (u.loop != nil && u.loop.bodyRun != nil)) {
		return d.advanceLoop(u, scope, nodeOutputs, opts)
	}

	// A guard decides in-pass; a false cond or an exec then settles inline, a pool-do
	// then materializes and parks. A blocked guard skip-cascades through runUnit below.
	if u.kind == unitGuard && !d.blocked(u) {
		return d.advanceGuard(u, scope, nodeOutputs, opts)
	}

	// A dispatch selects one arm and drives its body (the multi-way analog of guard).
	if u.kind == unitDispatch && !d.blocked(u) {
		return d.advanceDispatch(u, scope, nodeOutputs, opts)
	}

	// A timeout is a transparent wrapper: it activates once (stamping the advisory duration),
	// then ALWAYS runs its single body (the guard path MINUS the cond) — an exec settles inline
	// in this pass, a pool-do materializes and parks. A blocked timeout skip-cascades through
	// runUnit below.
	if u.kind == unitTimeout && !d.blocked(u) {
		return d.advanceTimeout(u, scope, nodeOutputs, opts)
	}

	// A pool for-each fans its body over the runtime array and parks on the members:
	// a pool-do leaf body dispatches each member as claimable pool work, and a RUN
	// body (bodyRun != nil) ALWAYS routes here under a PoolRouter regardless of the
	// sub-graph's kinds (advanceForEachRunBody drives each member's minted sub-graph
	// as a nested mini-pass — parking sub-dos, settling exec/settle subs in-pass). An
	// exec-bodied leaf fan (or any fan with no PoolRouter) falls through to runUnit ->
	// runForEach, running its members inline in-pass. A blocked for-each skip-cascades
	// through runUnit below.
	if u.kind == unitForEach && forEachPoolMode(u, opts) && !d.blocked(u) {
		return d.advanceForEach(u, scope, nodeOutputs, opts)
	}

	// A pool cleanup drives its guarded then its finally body sequentially, parking on
	// whichever sub-do is in flight. An all-inline cleanup (or no PoolRouter) falls
	// through to runUnit -> runCleanup. A blocked cleanup skip-cascades through runUnit.
	// An all-didNotRun guarded block (every drain member skipped/canceled — nothing ran,
	// so there is nothing to tear down) must NOT dispatch the finally: it falls through
	// to runUnit's aggregate-skip intercept and settles skipped with the SAME detail
	// string the inline driver emits, keeping the two drivers' journals byte-identical —
	// and the reducer's ready() never fronts a cleanup whose every drain member
	// did-not-run, so dispatching here would settle a node the fold refused to make
	// ready. Any FAILED member means the block RAN (didNotRun(failed)=false) and the
	// finally always runs; a leaf-form cleanup has no memberDeps, so the check is
	// vacuously false and its routing is unchanged.
	if u.kind == unitCleanup && cleanupPoolMode(u, opts) && !d.blocked(u) && !d.aggregateAllSkipped(u) {
		return d.advanceCleanup(u, scope, nodeOutputs, opts)
	}

	// A pool recover drives its guarded, then — ONLY if the guarded failed — its catch
	// body (with the error bound), parking on whichever sub-do is in flight. An
	// all-inline recover (or no PoolRouter) falls through to runUnit -> runRecover.
	if u.kind == unitRecover && recoverPoolMode(u, opts) && !d.blocked(u) {
		return d.advanceRecover(u, scope, nodeOutputs, opts)
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
				return d.observePoolWork(u.activation, u.nodeID, n.BeadID, scope, nodeOutputs, opts)
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
		// Amended ⚑B2 crash window: an ENGINE-mode activation (no pool dispatch marker)
		// with no bead is the half-settled index-failure pair — settleIndexRenderFailed's
		// activate committed, its settle did not. Re-derive the settle (the re-render
		// deterministically re-errors); falling through instead would dispatch a REAL bead
		// with the fold's empty route/prompt — a garbage dispatch that loses the detail.
		if n.DispatchMode != DispatchModePool {
			return d.resettleIndexWindow(u, scope)
		}
		return d.dispatchPoolWork(u.activation, u.nodeID, n.Route, n.Prompt, opts)
	}
	route, ok := opts.PoolRouter(u.leaf.agentRef)
	if !ok {
		return fmt.Errorf("%w: node %q (agent %q)", ErrNoPoolRoute, u.nodeID, u.leaf.agentRef)
	}
	// A pool-do inside a run sub-formula renders its prompt against the namespace
	// view (env bindings + settled sub outputs), not the flat root scope.
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return err
	}
	prompt, err := renderPrompt(u.leaf.raw, view)
	if err != nil {
		// ⚑B2: an index-render failure SETTLES the node failed{detail} (non-retryable)
		// in-pass — activate-if-needed + outcome.settled, the evalForEachArray→
		// failed{invalid_input} precedent — rather than erroring the Advance; the loop then
		// exits via its own `lane.outcome == failed` clause. Any other render error
		// (ErrPromptTooLarge etc.) keeps today's error path.
		var ire *indexRenderError
		if errors.As(err, &ire) {
			return d.settleIndexRenderFailed(u, ire.detail)
		}
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

// resettleIndexWindow re-derives the ⚑B2 settle for a HALF-SETTLED index-failure window
// (amended contract): the fold holds an ENGINE-mode activated-unsettled node with no bead —
// a death between settleIndexRenderFailed's two appends. The prompt render is a pure
// function of (IR, scope), so re-rendering deterministically re-errors with the SAME
// sentinel detail; settleIndexRenderFailed then no-ops the activate under its idem token
// and lands the missing settle. A non-sentinel re-render outcome here is a structural
// inconsistency (nothing else writes an engine-mode activation for a pool-routed do) and
// is refused loudly rather than guessed around.
func (d *driver) resettleIndexWindow(u planUnit, scope map[string]string) error {
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return err
	}
	_, err = renderPrompt(u.leaf.raw, view)
	var ire *indexRenderError
	if errors.As(err, &ire) {
		return d.settleIndexRenderFailed(u, ire.detail)
	}
	if err != nil {
		return fmt.Errorf("lumen: advance: do %q prompt: %w", u.nodeID, err)
	}
	return fmt.Errorf("lumen: advance: do %q is activated engine-mode with no bead yet its prompt renders (inconsistent half-settled window)", u.nodeID)
}

// observePoolWork consults the ObserveWork seam for a dispatched-but-unsettled pool
// node and, when its real bead has reached a terminal close, copies the outcome into
// the journal as the EXISTING outcome.settled (REDESIGN §1.4) — zero new settle arm.
// Retryable comes from the observation (the seam sets it true ONLY for an explicit
// gc.outcome=fail — a bare/unknown close is failed but NOT retryable, MEDIUM-2), so
// the formula's retry arm re-attempts a genuine worker failure with a FRESH bead
// (§5) but never re-runs work a bare close left already-complete.
//
// It ALSO seeds the do's output into scope/nodeOutputs (HIGH-2/3), exactly as genesis
// runDo's record() and the crash-restart reconstructOutputs do, so a same-pass
// downstream {{ref}} renders the resolved value and byte-identically to a
// crash-restart — closing the determinism hole where the settle updated only the
// fold state and left the driver's live interpolation scope stale. A still-open bead
// parks (nil, no append); an observer error is returned so the controller loop logs
// it and leaves the run parked to retry next tick (§9.7).
func (d *driver) observePoolWork(activation, nodeID, beadID string, scope, nodeOutputs map[string]string, opts Options) error {
	obs, err := opts.ObserveWork(d.ctx, beadID)
	if err != nil {
		return fmt.Errorf("lumen: advance: observe do %q bead %q: %w", nodeID, beadID, err)
	}
	if !obs.Terminal {
		return nil
	}
	if err := d.appendSettledRetryable(activation, obs.Outcome, obs.Output, "", obs.Retryable); err != nil {
		return err
	}
	// Gated on ranOutcome to mirror reconstructOutputs' genesis rule (a skip/cancel
	// records nothing); an observed terminal outcome is always a ran outcome.
	if ranOutcome(obs.Outcome) {
		d.record(nodeID, obs.Output, scope, nodeOutputs)
	}
	return nil
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

// forEachPoolMode reports whether Advance drives u (a for-each) through the park-aware
// advanceForEach fan arm rather than inline runForEach. True when a PoolRouter is set AND
// the body is either a pool-mode do OR a RUN sub-formula call (bodyRun != nil): a
// run-bodied fan ALWAYS routes here under a PoolRouter regardless of the sub-graph's kinds
// (the RBL dual-arm precedent, advanceLoop) — its members are minted at runtime, so we
// cannot know whether they contain pool dos, and driving them via advanceUnit handles both
// a parking sub-do AND an in-pass exec/settle sub. An exec-bodied leaf fan (or any fan with
// no PoolRouter) runs its members inline through runForEach instead (Advance/Run parity);
// an exec-only run-bodied fan still routes here and settles in one pass without stalling.
func forEachPoolMode(u planUnit, opts Options) bool {
	return opts.PoolRouter != nil && u.kind == unitForEach && u.forEach != nil &&
		(u.forEach.bodyIRKind == ir.NodeDo || u.forEach.bodyRun != nil)
}

// cleanupPoolMode reports whether a cleanup has a pool-do guarded OR body — the case
// Advance drives through the park-aware advanceCleanup arm. An all-exec/settle cleanup
// (or one with no PoolRouter) runs both subs inline through runCleanup instead. A
// BLOCK-form cleanup is routed by the existing bodyIRKind==NodeDo disjunct: its
// guardedIRKind is zero (the guarded disjunct can never fire — the block's do MEMBERS
// dispatch as ordinary pool units driven by the top loop, not through this arm), so only
// a do FINALLY routes it here; a block cleanup with an exec/settle finally stays on
// inline runCleanup (its aggregate is already settled by topo order when it is reached).
func cleanupPoolMode(u planUnit, opts Options) bool {
	return opts.PoolRouter != nil && u.kind == unitCleanup && u.cleanup != nil &&
		(u.cleanup.guardedIRKind == ir.NodeDo || u.cleanup.bodyIRKind == ir.NodeDo)
}

// recoverPoolMode reports whether a recover has a pool-do guarded OR catch body — the
// case Advance drives through advanceRecover. An all-exec/settle recover (or one with no
// PoolRouter) runs both subs inline through runRecover instead.
func recoverPoolMode(u planUnit, opts Options) bool {
	return opts.PoolRouter != nil && u.kind == unitRecover && u.recover != nil &&
		(u.recover.guardedIRKind == ir.NodeDo || u.recover.bodyIRKind == ir.NodeDo)
}

// advanceLoop is Advance's park-aware attempt-loop arm (§4.1) for a pool-mode
// retry/repeat: it materializes ONE body attempt at a time as claimable pool work
// and PARKS on it, minting the next attempt only after the current one settles.
// Each pass either settles the loop, mints exactly one attempt, or leaves a live
// attempt in flight, so it is re-entrant and idempotent — a re-Advance with no new
// settlement re-derives the same decision and the write-once appends dedupe.
func (d *driver) advanceLoop(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.loop

	// A repeat run body (⚑B2) drives an inlined sub-graph per attempt, not a single
	// pool do — a distinct park/mint shape (the aggregate activates last, so liveAttempt
	// is blind to a live attempt; the arm re-mints and re-drives idempotently instead).
	if spec.bodyRun != nil {
		return d.advanceRunBodyLoop(u, scope, nodeOutputs, opts)
	}

	// Ensure the loop node.activated (write-once): dependents gate on the loop node,
	// and the seal condition keys on its settle.
	if err := d.ensureLoopActivated(u); err != nil {
		return err
	}

	// retry: evaluate the attempts budget ONCE. An invalid budget settles the loop
	// failed{invalid_input} with zero attempts (reference parity).
	maxAttempts := 0
	if spec.irKind == ir.NodeRetry {
		as, err := d.loopEvalScope(u, 0, nil, scope, nodeOutputs)
		if err != nil {
			return fmt.Errorf("lumen: loop %q attempts: %w", u.nodeID, err)
		}
		n, ok := evalAttempts(spec.attemptsExpr, as)
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
		switch {
		case bn != nil && bn.BeadID == "" && bn.DispatchMode != DispatchModePool:
			// Amended ⚑B2 crash window: an ENGINE-mode activation with no bead is the
			// half-settled index-failure pair (settleIndexRenderFailed's activate committed,
			// its settle did not). Without this arm the empty BeadID parks the loop FOREVER
			// (a silent wedge — nothing will ever settle the attempt). Re-derive the settle,
			// then fall through to the decide logic below. The PRE-EXISTING pool-mode
			// activated-undispatched wedge (DispatchMode=pool, no bead) is deliberately NOT
			// routed here — it keeps its documented park behavior.
			if err := d.resettleLoopAttemptIndexWindow(u, liveAtt, scope); err != nil {
				return err
			}
			// Re-settled this pass: fall through to the decide logic below.
		case opts.ObserveWork == nil || bn == nil || bn.BeadID == "":
			return nil
		default:
			if err := d.observePoolWork(bodyAct, spec.bodyNodeID, bn.BeadID, scope, nodeOutputs, opts); err != nil {
				return err
			}
			if bn := d.st().Nodes[bodyAct]; bn == nil || !bn.Settled {
				return nil // still in flight — park
			}
			// Settled this pass: fall through to the decide logic below.
		}
	}

	// Decide over the highest settled attempt (if any), then mint. An attempt can settle
	// IN-PASS — the ⚑B2 index-fail settle is the only pool-path writer — in which case
	// parking would strand the run (no in-flight work → a false ErrAdvanceStalled): decide
	// immediately instead, the observe arm's settled-this-pass discipline. The chain is
	// bounded: loopDecide's loop_cap / retry budget settles the loop before the mint loop
	// can spin unboundedly.
	next := 0
	if settledAttempt, hasSettled := d.lastSettledAttempt(spec.bodyNodeID); hasSettled {
		bn := d.st().Nodes[activationForAttempt(spec.bodyNodeID, settledAttempt)]
		cont, err := d.loopDecide(u, settledAttempt, maxAttempts, bn, scope, nodeOutputs)
		if err != nil {
			return err
		}
		if !cont {
			return nil // loop settled inside loopDecide
		}
		next = settledAttempt + 1
	}
	for {
		if err := d.materializeLoopAttempt(u, next, maxAttempts, scope, opts); err != nil {
			return err
		}
		bn := d.st().Nodes[activationForAttempt(spec.bodyNodeID, next)]
		if bn == nil || !bn.Settled {
			return nil // dispatched — park on the in-flight attempt
		}
		cont, err := d.loopDecide(u, next, maxAttempts, bn, scope, nodeOutputs)
		if err != nil {
			return err
		}
		if !cont {
			return nil // loop settled inside loopDecide
		}
		next++
	}
}

// resettleLoopAttemptIndexWindow re-derives the ⚑B2 settle for a half-settled loop ATTEMPT
// (the amended crash window): it rebuilds attempt N's unit and render window exactly as
// materializeLoopAttempt does — the 1-based iteration binding seeded at the namespace-
// qualified key around the render, then restored — and routes through resettleIndexWindow,
// so the re-derived detail is byte-identical to the no-crash settle.
func (d *driver) resettleLoopAttemptIndexWindow(u planUnit, attempt int, scope map[string]string) error {
	spec := u.loop
	au := d.attemptUnit(u, attempt)
	iterKey := u.ns + spec.iterationName
	restore, had := "", false
	if spec.irKind == ir.NodeRepeat {
		restore, had = scope[iterKey]
		scope[iterKey] = strconv.Itoa(attempt + 1)
	}
	err := d.resettleIndexWindow(au, scope)
	if spec.irKind == ir.NodeRepeat {
		if had {
			scope[iterKey] = restore
		} else {
			delete(scope, iterKey)
		}
	}
	return err
}

// advanceRunBodyLoop is Advance's park-aware arm for a repeat whose body is a run
// (⚑B2, the advanceLoop-owned nested mini-pass). Unlike a do-body loop it cannot key
// on liveAttempt: the attempt aggregate settles at activationForAttempt(bodyNodeID, N)
// but activates LAST (⚑S1), so while an attempt is in flight there is NO
// activated-unsettled node at that node id. Instead it re-mints and re-drives the
// current attempt idempotently each pass: settle the loop or mint the next attempt
// from the last SETTLED aggregate (loopDecide), drive the current attempt's inlined
// sub-graph through advanceUnit (a nested mini-pass), and PARK when a minted sub-do is
// in flight (inFlightPoolWork reports it from the fold with zero changes). An
// exec/settle-only attempt settles in-pass, so the loop can mint and settle several
// attempts in one pass (Advance/Run parity), bounded by lumenRepeatLoopCap.
//
// DECIDE-EVERY-TICK is only sound because the cond's ref set is FROZEN at lowering
// to {the body's bare id, the iteration counter, input fields} — all attempt-local /
// immutable, so re-running loopDecide over the last settled attempt derives the SAME
// answer on every tick. Without the freeze, an external node ref settling between
// ticks could flip an already-acted-on continue into a stale settleLoop while the
// next attempt's bead is live (the seal path never re-consults inFlightPoolWork) —
// an orphaned live bead plus permanently activated-unsettled minted nodes. Lifting
// the freeze needs a durable per-attempt decision record (the guard write-once
// precedent) — a follow-up design, not this slice.
//
// WALL-TIME NOTE: this is the first Advance arm that can spend unbounded time in one
// pass — up to lumenRepeatLoopCap (32) sequential exec-bodied attempts under the
// non-renewed 30s writer lease, so a slow exec body can outlive the lease mid-pass
// (the same class as inline Run's attempt loops). A lease renew or a per-tick attempt
// budget is a deliberate follow-up, not this slice.
func (d *driver) advanceRunBodyLoop(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.loop
	// Ensure the loop node.activated (write-once): dependents gate on the loop node,
	// and the seal condition keys on its settle.
	if err := d.ensureLoopActivated(u); err != nil {
		return err
	}
	for {
		attempt := 0
		if settledAttempt, hasSettled := d.lastSettledAttempt(spec.bodyNodeID); hasSettled {
			// The highest attempt's aggregate settled: decide (settle the loop OR
			// re-attempt). loopDecide reads only bodyName/outcome + iteration, so a run
			// aggregate is indistinguishable from a leaf attempt to it (ZERO changes there).
			bn := d.st().Nodes[activationForAttempt(spec.bodyNodeID, settledAttempt)]
			cont, err := d.loopDecide(u, settledAttempt, 0, bn, scope, nodeOutputs)
			if err != nil {
				return err
			}
			if !cont {
				return nil // loop settled inside loopDecide
			}
			attempt = settledAttempt + 1
		}
		settled, err := d.driveRunBodyAttempt(u, attempt, scope, nodeOutputs, opts)
		if err != nil {
			return err
		}
		if !settled {
			return nil // park — a minted sub-do is in flight (inFlightPoolWork reports it)
		}
		// The attempt aggregate settled this pass: loop back to decide (mint N+1 or settle).
	}
}

// driveRunBodyAttempt mints attempt N's inlined sub-graph and drives it one nested
// mini-pass, reporting whether the attempt aggregate has settled. It re-mints
// deterministically every pass (mintRunBodyAttempt), registers the attempt env seam
// (⚑B1), emits the idem-keyed attempt.minted, then drives each minted sub-unit and
// finally the aggregate through advanceUnit — which inherits defer-on-deps, pool
// materialize/observe/park, inline exec/settle, and resume memoization. The aggregate
// is driven LAST (⚑S1 Tier-A FK ordering: its Members edges reference the sub-node
// rows, which its members-gate defers behind); it stays unsettled — so this returns
// false and the loop PARKS — while any sub-do is in flight.
func (d *driver) driveRunBodyAttempt(u planUnit, attempt int, scope, nodeOutputs map[string]string, opts Options) (bool, error) {
	spec := u.loop
	subUnits, agg, err := spec.mintRunBodyAttempt(attempt, u.activation, u.ns, u.afterDeps, u.rawAfter)
	if err != nil {
		return false, err
	}
	d.registerRunBodyEnv(spec, attempt, u.ns, subUnits)
	if err := d.appendAttemptMinted(u.activation, attempt, repeatRemaining(attempt)); err != nil {
		return false, err
	}
	for i := range subUnits {
		if err := d.advanceUnit(subUnits[i], scope, nodeOutputs, opts); err != nil {
			return false, err
		}
	}
	if err := d.advanceUnit(agg, scope, nodeOutputs, opts); err != nil {
		return false, err
	}
	n := d.st().Nodes[agg.activation]
	return n != nil && n.Settled, nil
}

// advanceGuard drives a guard's decision arm in the parking model: it activates the
// guard once, evaluates the closed condition over the fold, and either settles the
// guard PASS (false cond — a no-op that does not skip-cascade) or runs the `then`. An
// exec then runs inline in this pass; a pool-do then materializes as ordinary work and
// PARKS, is observed on a later pass when its bead closes, and then the guard settles
// transparently from it. The decision is a pure function of the fold (re-evaluated
// identically across passes), so re-Advance converges.
func (d *driver) advanceGuard(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.guard
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	tu := d.guardThenUnit(u)
	// Write-once decision (red-team): the then's node.activated IS the durable record
	// that the TRUE branch was taken. Only evaluate the cond when the then has NOT been
	// activated yet — otherwise a re-Advance over a grown fold could re-decide FALSE
	// while the then's bead is already dispatched. The cond-ref gate (resolveDeps)
	// makes the FIRST evaluation stable; this makes it permanent across passes.
	if d.st().Nodes[tu.activation] == nil {
		cs, err := d.condScope(u.ns, scope, nodeOutputs)
		if err != nil {
			return fmt.Errorf("lumen: guard %q cond: %w", u.nodeID, err)
		}
		truthy, err := evalCondTruthy(spec.cond, cs)
		if err != nil {
			return fmt.Errorf("lumen: guard %q cond: %w", u.nodeID, err)
		}
		if !truthy {
			return d.settleDecisionSkipped(u, scope, nodeOutputs)
		}
	}
	if opts.PoolRouter != nil && spec.thenIRKind == ir.NodeDo {
		tn := d.st().Nodes[tu.activation]
		switch {
		case tn == nil || (!tn.Settled && tn.BeadID == ""):
			// Not yet materialized (or activated but not dispatched): dispatch it, park.
			return d.materializePoolWork(tu, scope, opts)
		case !tn.Settled:
			// In flight: observe its bead; if still open, park.
			if opts.ObserveWork == nil {
				return nil
			}
			if err := d.observePoolWork(tu.activation, tu.nodeID, tn.BeadID, scope, nodeOutputs, opts); err != nil {
				return err
			}
			if n := d.st().Nodes[tu.activation]; n == nil || !n.Settled {
				return nil
			}
		}
		// tn settled: fall through to settle the guard.
	} else if tn := d.st().Nodes[tu.activation]; tn == nil || !tn.Settled {
		// An exec then (or a do then with a Host) runs inline in this pass.
		if err := d.runUnit(tu, scope, nodeOutputs); err != nil {
			return err
		}
	}
	return d.settleDecisionFromBody(u, tu, scope, nodeOutputs)
}

// advanceTimeout is Advance's park-aware arm for a timeout wrapper — the guard path MINUS the
// cond (the body ALWAYS runs, no write-once decision, no skipped arm). It activates the wrapper
// once (stamping the advisory duration via appendActivated), then drives the single body: an
// exec settles inline in this pass, a pool-do materializes/observes/parks. It settles the
// wrapper transparently from the body (settleDecisionFromBody).
//
// The DAR lesson (§1.1.5): it calls crashAt(crashAfterActivate, u.activation) IMMEDIATELY after
// ensureDecisionActivated — mirroring runUnit's inline placement — so the activated-unsettled
// wrapper window (the ONLY point between the wrapper's two appends) is injectable on the POOL
// driver too. Resume re-drives the body and settles, converging. The re-emitted wrapper
// activation carries the raw duration byte-for-byte (idem token), so no ErrIdemTokenReuse.
func (d *driver) advanceTimeout(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.timeout
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	if err := d.crashAt(crashAfterActivate, u.activation); err != nil {
		return err
	}
	bu := d.timeoutBodyUnit(u)
	if opts.PoolRouter != nil && spec.bodyIRKind == ir.NodeDo {
		bn := d.st().Nodes[bu.activation]
		switch {
		case bn == nil || (!bn.Settled && bn.BeadID == ""):
			// Not yet materialized (or activated but not dispatched): dispatch it, park.
			return d.materializePoolWork(bu, scope, opts)
		case !bn.Settled:
			// In flight: observe its bead; if still open, park.
			if opts.ObserveWork == nil {
				return nil
			}
			if err := d.observePoolWork(bu.activation, bu.nodeID, bn.BeadID, scope, nodeOutputs, opts); err != nil {
				return err
			}
			if n := d.st().Nodes[bu.activation]; n == nil || !n.Settled {
				return nil
			}
		}
		// bn settled: fall through to settle the wrapper.
	} else if bn := d.st().Nodes[bu.activation]; bn == nil || !bn.Settled {
		// An exec body (or a do body with a Host) runs inline in this pass. The `!bn.Settled`
		// disjunct is load-bearing for the BODY-side crash window: a kill between act(body) and
		// settle(body) leaves an activated-unsettled body node, and resume MUST re-drive it here
		// (at-least-once) — falling through would hand settleDecisionFromBody its silent PASS/""
		// default and seal a FALSE-PASS wrapper over a never-settled body (the DAR ⚑B2 class).
		if err := d.runUnit(bu, scope, nodeOutputs); err != nil {
			return err
		}
	}
	return d.settleDecisionFromBody(u, bu, scope, nodeOutputs)
}

// advanceDispatch drives a dispatch's multi-way branch in the parking model: it
// activates the dispatch once, then selects an arm. If a prior pass already activated
// an arm body (the durable, write-once decision), it drives THAT arm without
// re-evaluating the subject; otherwise it evaluates the subject and selects the
// matching arm (no match settles the dispatch pass). The chosen arm's exec body runs
// inline; a pool-do body materializes/observes/parks; a RUN arm mints+drives its whole
// sub-graph (advanceDispatchRunArm). The subject-ref gate keeps the first selection stable.
//
// ⚑B2 kind-route FIRST: the branch on the matched/chosen arm's bodyRun happens BEFORE any
// leaf/pool-do handling, on BOTH the chosen-arm and matched-arm paths. chosenArm (which fires
// on node existence) may only SKIP re-matchingArm; a returned RUN arm STILL takes the
// re-mint/drive route (after an agg-activated-unsettled crash the fast-path returns the run
// arm and handing it to the leaf machinery is an empty step / loud crash-loop). unitDispatch
// routes here unconditionally (no dispatchPoolMode-analog: a unit-level gate cannot know the
// chosen arm and would misroute a MIXED dispatch) — the fork lives INSIDE, per matched arm.
func (d *driver) advanceDispatch(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	arm, chosen := d.chosenArm(u)
	if !chosen {
		var ok bool
		var err error
		arm, ok, err = d.matchingArm(u, scope)
		if err != nil {
			return fmt.Errorf("lumen: dispatch %q subject: %w", u.nodeID, err)
		}
		if !ok {
			return d.settleDecisionSkipped(u, scope, nodeOutputs)
		}
	}
	// ⚑B2: kind-route on the MATCHED/CHOSEN arm's bodyRun BEFORE any leaf/pool-do handling.
	if arm.bodyRun != nil {
		return d.advanceDispatchRunArm(u, arm, scope, nodeOutputs, opts)
	}
	au := d.dispatchArmUnit(u, arm)
	if opts.PoolRouter != nil && arm.bodyIRKind == ir.NodeDo {
		bn := d.st().Nodes[au.activation]
		switch {
		case bn == nil || (!bn.Settled && bn.BeadID == ""):
			return d.materializePoolWork(au, scope, opts)
		case !bn.Settled:
			if opts.ObserveWork == nil {
				return nil
			}
			if err := d.observePoolWork(au.activation, au.nodeID, bn.BeadID, scope, nodeOutputs, opts); err != nil {
				return err
			}
			if n := d.st().Nodes[au.activation]; n == nil || !n.Settled {
				return nil
			}
		}
	} else if bn := d.st().Nodes[au.activation]; bn == nil || !bn.Settled {
		if err := d.runUnit(au, scope, nodeOutputs); err != nil {
			return err
		}
	}
	return d.settleDecisionFromBody(u, au, scope, nodeOutputs)
}

// advanceDispatchRunArm is Advance's park-aware arm for a MATCHED run arm (DAR): it re-mints
// the arm sub-graph STATELESSLY every pass (no fold-state early-out — the arm aggregate
// `<armBodyID>:0` activates LAST, so a live sub-do leaves no activated-unsettled node at the
// aggregate id; an early-out would misroute the agg-activated-unsettled crash window AND skip
// re-registration), registers its env seam, and drives every minted sub-unit + the arm
// aggregate LAST (⚑S1) through advanceUnit — which inherits defer-on-deps, pool
// materialize/observe/park, inline exec/settle, and resume memoization. It settles the
// dispatch ONLY when the aggregate has SETTLED (the ⚑B2 precondition — settleDecisionFromBody
// silently defaults PASS/"" on an unsettled agg); while any sub-do is in flight the aggregate
// stays unsettled and the dispatch stays UNSETTLED (park). inFlightPoolWork reports a parked
// arm's minted sub-dos from the fold, so a re-Advance with no new settlement is a no-op.
func (d *driver) advanceDispatchRunArm(u planUnit, arm *dispatchArm, scope, nodeOutputs map[string]string, opts Options) error {
	subUnits, agg, err := d.dispatchArmRunBody(u, arm)
	if err != nil {
		return err
	}
	d.registerDispatchArmRunEnv(u, arm, subUnits)
	for i := range subUnits {
		if err := d.advanceUnit(subUnits[i], scope, nodeOutputs, opts); err != nil {
			return err
		}
	}
	if err := d.advanceUnit(agg, scope, nodeOutputs, opts); err != nil {
		return err
	}
	if n := d.st().Nodes[agg.activation]; n == nil || !n.Settled {
		return nil // park — a sub-do is in flight; the dispatch stays UNSETTLED
	}
	return d.settleDecisionFromBody(u, agg, scope, nodeOutputs)
}

// advanceForEach is Advance's park-aware fan arm for a pool for-each: it activates the
// aggregate once, evaluates the `over` array (stable — the over-ref gate froze it), and
// drives every member in this pass (a concurrent fan-out, unlike a loop's one-at-a-time
// minting). For a LEAF-do fan it materializes each not-yet-minted member as claimable pool
// work; for a RUN-bodied fan (bodyRun != nil) it delegates to advanceForEachRunBody, which
// mints+drives each element's sub-graph mini-pass. Either way it PARKS until all members
// settle, then settles the aggregate with the shared scatter drain rule. An empty array
// settles PASS in one pass. It is re-entrant: re-mint is write-once (a member already in the
// fold is skipped) and the array is DET-stable, so a re-Advance with no new settlement
// converges.
func (d *driver) advanceForEach(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	spec := u.forEach
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	elems, ok, err := d.evalForEachArray(u.ns, spec, scope)
	if err != nil {
		// The wrap contract: evalForEachArray's messages carry no "lumen:"/id prefix;
		// this (and runForEach, driver parity) names the failing fan exactly once.
		return fmt.Errorf("lumen: for-each %q over: %w", u.nodeID, err)
	}
	if !ok {
		return d.settleForEachInvalid(u, nodeOutputs)
	}
	if len(elems) == 0 {
		return d.settleForEach(u, nil, nodeOutputs)
	}
	// A run-bodied fan (FBR) mints a whole sub-graph per element rather than a single
	// pool do — the FIS-shaped concurrent member loop drives EVERY member's mini-pass
	// before parking (⚑B1), so it stays a concurrent fan, never serialized.
	if u.forEach.bodyRun != nil {
		return d.advanceForEachRunBody(u, elems, scope, nodeOutputs, opts)
	}
	memberActs := make([]string, len(elems))
	allSettled := true
	for i, elem := range elems {
		mu := d.forEachMemberUnit(u, i)
		memberActs[i] = mu.activation
		mn := d.st().Nodes[mu.activation]
		switch {
		case mn == nil || (!mn.Settled && mn.BeadID == ""):
			// Not yet materialized (or activated but not dispatched): dispatch it, park.
			member := mu
			if err := d.withBinder(scope, u.ns+spec.binder, elem, func() error {
				return d.materializePoolWork(member, scope, opts)
			}); err != nil {
				return err
			}
			allSettled = false
		case !mn.Settled:
			// In flight: observe its bead; if still open, it stays unsettled (park).
			if opts.ObserveWork != nil {
				if err := d.observePoolWork(mu.activation, mu.nodeID, mn.BeadID, scope, nodeOutputs, opts); err != nil {
					return err
				}
			}
			if n := d.st().Nodes[mu.activation]; n == nil || !n.Settled {
				allSettled = false
			}
		}
	}
	if !allSettled {
		return nil // park — the members are in-flight pool work (inFlightPoolWork reports them)
	}
	return d.settleForEach(u, memberActs, nodeOutputs)
}

// settleForEach settles the for-each aggregate from its (settled) member activations
// via the shared scatter drain rule (an empty member set is a vacuous PASS).
func (d *driver) settleForEach(u planUnit, memberActs []string, nodeOutputs map[string]string) error {
	outcome := scatterDrainOutcome(d.st(), memberActs, u.forEach.onFail)
	if err := d.appendSettled(u.activation, outcome, "", ""); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// settleForEachInvalid settles the aggregate failed{invalid_input} for a non-array over.
func (d *driver) settleForEachInvalid(u planUnit, nodeOutputs map[string]string) error {
	if err := d.appendSettled(u.activation, OutcomeFailed, "", "invalid_input"); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// advanceForEachRunBody is Advance's park-aware arm for a RUN-bodied for-each (FBR) — the
// ⚑B1 concurrent fan. Unlike a repeat run body (one attempt at a time), it drives the FULL
// member loop EVERY pass: for each element it mints the sub-graph, registers the env seam,
// and drives every minted sub-unit + the member aggregate through advanceUnit (a nested
// mini-pass), accumulating whether all members' aggregates have settled. It PARKS only
// AFTER the loop (never on the first in-flight member — the RBL early-return shape would
// SERIALIZE the fan, one member per settle-round-trip, diverging from leaf-fan semantics
// and the corpus marquee), so a 2-member fan dispatches BOTH members' sub-dos in ONE pass.
// A member whose aggregate settled FAILED does not suppress minting/driving the others
// (scatter drains everything; on_fail affects only the settle outcome). When every member's
// aggregate has settled it settles the fan with the shared scatter drain rule.
//
// It re-mints STATELESSLY every pass (no fold-state early-out): the member aggregate
// activates LAST, so a live sub-do leaves no activated-unsettled node at the aggregate id
// (an `mn != nil` early-out would misroute the agg-activated-unsettled crash window AND skip
// re-registration). inFlightPoolWork reports a parked member's minted sub-dos from the fold
// with zero changes, so a re-Advance with no new settlement is a no-op.
func (d *driver) advanceForEachRunBody(u planUnit, elems []string, scope, nodeOutputs map[string]string, opts Options) error {
	memberActs := make([]string, len(elems))
	allSettled := true
	for i, elem := range elems {
		memberActs[i] = activationFor(forEachMemberNodeID(u.nodeID, i))
		settled, err := d.driveForEachRunMember(u, i, elem, scope, nodeOutputs, opts)
		if err != nil {
			return err
		}
		if !settled {
			allSettled = false
		}
	}
	if !allSettled {
		return nil // park — minted sub-dos are in flight (inFlightPoolWork reports them)
	}
	return d.settleForEach(u, memberActs, nodeOutputs)
}

// driveForEachRunMember mints for-each member `index`'s run sub-graph and drives it one
// nested mini-pass INSIDE the per-member withBinder window (⚑S2 — the binder must be live
// during every sub-do's render, on EVERY pass, so a sub-do first materialized on a later
// pass renders elems[index], not a same-named node that settled in between; and it must not
// leak into member j's mini-pass, so it is saved/restored per member). It mints+registers
// deterministically every pass, drives each minted sub-unit then the aggregate LAST (⚑S1)
// through advanceUnit — which inherits defer-on-deps, pool materialize/observe/park, inline
// exec/settle, and resume memoization — and reports whether the aggregate has settled (it
// stays unsettled while any sub-do is in flight, so the caller parks).
func (d *driver) driveForEachRunMember(u planUnit, index int, elem string, scope, nodeOutputs map[string]string, opts Options) (bool, error) {
	settled := false
	err := d.withBinder(scope, u.ns+u.forEach.binder, elem, func() error {
		subUnits, agg, mintErr := d.forEachRunMember(u, index)
		if mintErr != nil {
			return mintErr
		}
		d.registerForEachRunMemberEnv(u, agg.nodeID, subUnits)
		for j := range subUnits {
			if e := d.advanceUnit(subUnits[j], scope, nodeOutputs, opts); e != nil {
				return e
			}
		}
		if e := d.advanceUnit(agg, scope, nodeOutputs, opts); e != nil {
			return e
		}
		n := d.st().Nodes[agg.activation]
		settled = n != nil && n.Settled
		return nil
	})
	return settled, err
}

// advanceCleanup is Advance's park-aware arm for a pool cleanup (try/finally): it drives
// the guarded to settlement, then — ALWAYS, regardless of the guarded's outcome — the
// finally body to settlement, parking on whichever sub-do is in flight, then settles the
// cleanup (finally-failure-wins). It FALLS THROUGH within a pass on any in-pass
// settlement (an exec/settle sub, or an observe that closes a sub-do) so a mixed
// exec+do cleanup never returns with a pending sub and no pool work in flight (which
// would trip ErrAdvanceStalled). Re-entrant: a settled sub is a no-op reporting settled.
func (d *driver) advanceCleanup(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	var gu planUnit
	if u.cleanup.guardedAgg == "" {
		// Single-leaf form: drive the guarded sub-step to settlement first.
		gu = d.cleanupGuardedUnit(u)
		settled, err := d.driveSubStep(gu, scope, nodeOutputs, opts)
		if err != nil {
			return err
		}
		if !settled {
			return nil // park on the in-flight guarded do
		}
	}
	// Block form: PHASE-1 is a no-op — the guarded block's do members and drain aggregate
	// are ordinary units driven by the top loop, and the cleanup's memberDep on the
	// aggregate means advanceUnit only reaches here once the aggregate has settled.
	bu := d.cleanupBodyUnit(u)
	settled, err := d.driveSubStep(bu, scope, nodeOutputs, opts)
	if err != nil {
		return err
	}
	if !settled {
		return nil // park on the in-flight finally do
	}
	return d.settleCleanup(u, gu, bu, scope, nodeOutputs)
}

// driveSubStep drives one cleanup sub-unit one step toward settlement and reports
// whether it is now settled. A pool-mode do is materialized (if not yet dispatched) or
// observed (if in flight) — reporting settled only once its bead closes; an exec/settle
// (or a do with a Host and no pool) sub runs inline in-pass. An already-settled sub is a
// no-op that reports settled, so a re-Advance is idempotent (the write-once appends
// dedupe, and a closed sub never re-runs).
func (d *driver) driveSubStep(su planUnit, scope, nodeOutputs map[string]string, opts Options) (bool, error) {
	if n := d.st().Nodes[su.activation]; n != nil && n.Settled {
		return true, nil
	}
	if poolMode(su, opts) {
		n := d.st().Nodes[su.activation]
		switch {
		case n == nil || (!n.Settled && n.BeadID == ""):
			return false, d.materializePoolWork(su, scope, opts)
		case !n.Settled:
			if opts.ObserveWork == nil {
				return false, nil
			}
			if err := d.observePoolWork(su.activation, su.nodeID, n.BeadID, scope, nodeOutputs, opts); err != nil {
				return false, err
			}
			m := d.st().Nodes[su.activation]
			return m != nil && m.Settled, nil
		}
	}
	if err := d.runUnit(su, scope, nodeOutputs); err != nil {
		return false, err
	}
	return true, nil
}

// advanceRecover is Advance's park-aware arm for a pool recover (try/catch): it drives
// the guarded to settlement; if the guarded did NOT fail it settles the recover
// transparently (the catch is never dispatched); if it FAILED it binds the error and
// drives the catch body to settlement, parking on whichever sub-do is in flight, then
// settles from the catch. It falls through on any in-pass settlement (driveSubStep), so
// a mixed exec/settle+do recover never stalls. Re-entrant like advanceCleanup.
func (d *driver) advanceRecover(u planUnit, scope, nodeOutputs map[string]string, opts Options) error {
	if err := d.ensureDecisionActivated(u); err != nil {
		return err
	}
	gu := d.recoverGuardedUnit(u)
	settled, err := d.driveSubStep(gu, scope, nodeOutputs, opts)
	if err != nil {
		return err
	}
	if !settled {
		return nil // park on the guarded
	}
	if !recoverCaught(d.settledOutcome(gu.activation)) {
		return d.settleRecoverFrom(u, gu, scope, nodeOutputs) // transparent; catch never dispatched
	}
	bu := d.recoverBodyUnit(u)
	var bodySettled bool
	if err := d.withErrorBindings(scope, d.errorBindings(u), func() error {
		var e error
		bodySettled, e = d.driveSubStep(bu, scope, nodeOutputs, opts)
		return e
	}); err != nil {
		return err
	}
	if !bodySettled {
		return nil // park on the catch
	}
	return d.settleRecoverFrom(u, bu, scope, nodeOutputs)
}

// ensureDecisionActivated emits a guard node's node.activated once (write-once via the
// idem token; a fold-state check avoids a redundant append attempt).
func (d *driver) ensureDecisionActivated(u planUnit) error {
	if n := d.st().Nodes[u.activation]; n != nil {
		return nil
	}
	if err := d.crashAt(crashBeforeActivate, u.activation); err != nil {
		return err
	}
	return d.appendActivated(u)
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
	// The 1-based iteration binding is NAMESPACE-QUALIFIED (u.ns + name), so an attempt
	// rendering in the loop's namespace resolves {{iteration}} via scopeFor; at the root
	// u.ns == "" so the key is the bare name (byte-identical to before).
	iterKey := u.ns + spec.iterationName
	restore, had := "", false
	if spec.irKind == ir.NodeRepeat {
		restore, had = scope[iterKey]
		scope[iterKey] = strconv.Itoa(attempt + 1)
	}
	err := d.materializePoolWork(au, scope, opts)
	if spec.irKind == ir.NodeRepeat {
		if had {
			scope[iterKey] = restore
		} else {
			delete(scope, iterKey)
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
