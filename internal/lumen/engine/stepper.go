package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// This file is the v1 AGENT-DRIVEN STEPPER (open decision 8): the buildable form of
// "v1 = a formula whose steps run sequentially in the agent's own session." The agent
// IS the driver; `gc` is a stateless stepper the agent calls between its own turns
// through two thin verbs — Step and Settle — each a short-lived PURE FOLD-RESUME over
// the SAME rebuild/fold/at-most-once/seal core Resume and Advance enter through. There
// is NO new AgentHost and NO new fold arm: the stepper writes the SAME event TYPES, in
// the SAME canonical order, that a synchronous engine.Run writes for the same formula
// (node.activated(engine) → effect.scheduled → effect.settled → outcome.settled → … →
// run.closed). The raw journals differ only in fold-INVISIBLE payload fields (run.started
// carries the enqueue-time Driver/FormulaRef stamps; effect.settled omits the host's
// session ref) and in run identity (stream nonce, created_at). Because the reducer's Apply
// is a pure function of (state, event) that never reads who appended — and effect.* fold
// to no-ops — a stepper run and an engine.Run scripting the same do outcomes fold to a
// byte-identical NORMALIZED fold state (reducerVersion 4). That is the determinism oracle.
//
// SPLIT DISCIPLINE (the load-bearing crash/idempotency choice). Step writes ONLY the
// do's node.activated; Settle writes the do's effect.scheduled + effect.settled +
// outcome.settled. This split — not the design's literal "step writes effect.scheduled
// too" — is what makes BOTH required contracts hold at once over one journal state:
//
//   - Re-Step before Settle re-offers the SAME do and appends nothing (node.activated
//     dedups on its write-once idem token; no effect record exists yet). This is the
//     engine's crashAfterActivate arm — an activated-but-unsettled do with no scheduled
//     effect is re-run, not failed — so idempotent re-step and a crash between Step and
//     Settle are the identical, consistent RE-OFFER.
//   - A crash DURING Settle (after effect.scheduled, before outcome.settled) settles the
//     do FAILED on re-claim WITHOUT re-invoking the agent — the engine's existing
//     crashBeforeAct/crashAfterAct at-most-once contract (interruptedEffects →
//     settleInterruptedEffect), inherited unchanged.
//
// The TOTAL event-type sequence is identical to engine.Run either way, and the fold is
// blind to which call appended an event, so the split leaves the normalized fold state
// byte-identical (the determinism oracle is unaffected); it only relocates the at-most-once
// window onto the Settle call, where it belongs. The
// step→Settle gap is inherently AT-LEAST-once for the agent's OFF-journal work (an agent
// that dies after doing the work but before Settle re-does it on re-claim) — the honest
// stepper limit, strictly better than the WorkerHost phase-guess since no sub-session's
// clean-exit-after-bad-work can read pass.

// StepResult reports one stepper turn (Step or Settle). It is a discriminated result:
//
//   - Done == true: the run has sealed (run.closed). Outcome carries the run's
//     fold-aggregated outcome; NodeID/Activation/Prompt are empty.
//   - Done == false: NodeID/Activation name the next ready do and Prompt is its rendered
//     body. The agent performs Prompt in its own session, then hands the result back
//     through Settle(streamID, NodeID, <outcome>, <output>).
type StepResult struct {
	Done       bool
	NodeID     string
	Activation string
	Prompt     string
	Outcome    string
}

// Step advances a v1 run by one turn WITHOUT running any agent work: it rebuilds the
// driver from the journal, drives every ready non-do unit inline (exec/settle/silent/
// skip-cascade/aggregate — through the SAME runUnit path engine.Run uses), and stops at
// the first ready engine-mode do, which it ACTIVATES (write-once node.activated) and returns with its
// rendered prompt. A re-Step before Settle re-offers the SAME do and appends nothing. If
// the run is already closed, or every unit settled, it seals and returns Done.
//
// It reuses rebuildDriver (the shared restore core), so an already-settled unit is
// reloaded, a recorded-but-unsettled effect is applied, and a crash-interrupted effect
// is settled FAILED — all from the journal, never by re-acting.
func Step(ctx context.Context, store *graphstore.Store, doc *ir.IR, streamID string, input map[string]any, opts Options) (StepResult, error) {
	d, units, scope, nodeOutputs, release, err := stepDriverFor(ctx, store, doc, streamID, input, opts)
	if err != nil {
		return StepResult{}, err
	}
	defer release()

	if d.st().Closed {
		return StepResult{Done: true, Outcome: d.st().Outcome}, nil
	}
	return d.stepDrive(units, scope, nodeOutputs)
}

// Settle records the agent's self-reported outcome for a do the agent just performed,
// then FUSES the next Step: it appends the do's effect.scheduled + effect.settled +
// outcome.settled (the at-most-once effect pair, identical to runDo's minus the host
// call), advances the fold, and returns the next ready do (or Done at the seal). node
// is the do's bare node id (as Step printed it); outcome is the agent's self-report
// (pass/fail/degraded/pending, fail-closed on anything else); output is the do's result
// the downstream {{ref}} interpolation consumes.
//
// A duplicate Settle of an already-settled do is a NO-OP: every append dedups on its
// write-once idem token, so the journal is unchanged and the next ready do is returned.
// Settling an already-sealed run is an idempotent Done.
func Settle(ctx context.Context, store *graphstore.Store, doc *ir.IR, streamID string, input map[string]any, node, outcome, output string, opts Options) (StepResult, error) {
	d, units, scope, nodeOutputs, release, err := stepDriverFor(ctx, store, doc, streamID, input, opts)
	if err != nil {
		return StepResult{}, err
	}
	defer release()

	if d.st().Closed {
		return StepResult{Done: true, Outcome: d.st().Outcome}, nil
	}

	u, ok := findStepperDoUnit(units, node)
	if !ok {
		return StepResult{}, fmt.Errorf("lumen: settle: formula %q has no do node %q", doc.Name, node)
	}
	// Readiness guard (agent-trust robustness): refuse to settle a do that is not a
	// currently-offered ready engine-mode do. A misbehaving agent naming an out-of-order
	// do (its dependencies unsettled, so its {{ref}} scope is not yet resolved) or a
	// skip-cascaded do gets a LOUD error with NO journal write — no node.activated, no
	// effect record — rather than an out-of-oracle-order journal. This is the same trust
	// class as the self-reported --outcome, closed cheaply. An already-settled do passes
	// (its deps are settled by construction), so a duplicate Settle stays a no-op via the
	// idem-token dedup below.
	if n := d.st().Nodes[u.activation]; n == nil || !n.Settled {
		if !d.depsSettled(u) {
			return StepResult{}, fmt.Errorf("lumen: settle: do %q is not ready — its dependencies have not settled; settle it only when `gc lumen step` offers it", node)
		}
		if d.blocked(u) {
			return StepResult{}, fmt.Errorf("lumen: settle: do %q is skip-cascaded (an upstream dependency did not pass) and cannot be settled by the agent", node)
		}
	}
	// Record the do's effect + outcome. On a duplicate Settle the do is already settled;
	// settleDoTurn's appends all dedup on their write-once idem tokens, so this is a
	// no-op — the dedup, not a status short-circuit, is what makes a double Settle safe
	// (mutation pin (iii): dropping the idem-token discipline double-appends the settle).
	if err := d.settleDoTurn(u, scope, nodeOutputs, outcome, output); err != nil {
		return StepResult{}, err
	}
	return d.stepDrive(units, scope, nodeOutputs)
}

// stepDriverFor is the shared Step/Settle preamble: it validates the stream id, lowers
// the SAME units engine.Run lowers with a host present (allowDo/allowCombineDo both
// true, so the do set and activation keys match the determinism oracle exactly),
// acquires the writer lease, and rebuilds the driver from the journal. The returned
// driver has NO host and NO pool router (the stepper is engine-mode and never runs a do
// itself), so only SnapshotEvery + LeaseHolder are threaded through. The caller MUST
// defer release() to drop the lease.
func stepDriverFor(ctx context.Context, store *graphstore.Store, doc *ir.IR, streamID string, input map[string]any, opts Options) (d *driver, units []planUnit, scope, nodeOutputs map[string]string, release func(), err error) {
	if store == nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("lumen: stepper: nil store")
	}
	if doc == nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("lumen: stepper: nil IR document")
	}
	if streamID == "" {
		return nil, nil, nil, nil, nil, fmt.Errorf("lumen: stepper: empty stream id")
	}

	units, err = buildUnits(doc, true, true)
	if err != nil {
		return nil, nil, nil, nil, nil, err
	}
	RegisterVocabulary(store)

	// Engine-mode only: the stepper never dispatches to a pool or a host — it is the
	// agent's own session. Threading a clean Options prevents a caller accidentally
	// engaging pool mode and turning a v1 do into a dispatched bead.
	stepOpts := Options{SnapshotEvery: opts.SnapshotEvery, LeaseHolder: opts.LeaseHolder}

	lease, err := store.AcquireWriterLease(ctx, streamID, resolveLeaseHolder(stepOpts), leaseTTL)
	if err != nil {
		return nil, nil, nil, nil, nil, fmt.Errorf("lumen: stepper: acquire writer lease %q: %w", streamID, err)
	}
	release = func() { _ = store.ReleaseWriterLease(ctx, lease) }

	d, scope, nodeOutputs, err = rebuildDriver(ctx, store, doc, streamID, input, lease.Epoch, stepOpts)
	if err != nil {
		release()
		return nil, nil, nil, nil, nil, err
	}
	d.runEnvs = runEnvIndex(units)
	return d, units, scope, nodeOutputs, release, nil
}

// stepDrive walks the topo-ordered units, settling every ready non-do unit inline and
// stopping at the first ready engine-mode do (which it activates and returns). It is the
// shared body of Step (fresh pass) and Settle (post-settle continuation): reaching the
// end of the walk with no ready do means every unit settled, so it seals.
func (d *driver) stepDrive(units []planUnit, scope, nodeOutputs map[string]string) (StepResult, error) {
	for i := range units {
		u := units[i]
		// Reload an already-settled unit, settle a do from a recorded effect, or settle a
		// crash-interrupted effect FAILED — the shared at-most-once memoization. On a fresh
		// pass the maps are empty and this is a no-op.
		handled, err := d.resumeMemoized(u, scope, nodeOutputs)
		if err != nil {
			return StepResult{}, err
		}
		if handled {
			continue
		}
		// Defer a unit whose deps have not all settled: a linear stepper never hits this
		// before the frontier do, but a fan/gather leaves later units for a future turn.
		if !d.depsSettled(u) {
			continue
		}
		// The first ready engine-mode do is the step target: ACTIVATE it (write-once) and
		// hand back its rendered prompt. It is NOT run here — the agent performs it in its
		// own session and reports back through Settle. A blocked do falls through to
		// runUnit below and skip-cascades, exactly like engine.Run.
		if u.kind == unitLeaf && u.leaf.kind == ir.NodeDo && !d.blocked(u) {
			offered, nodeID, prompt, err := d.offerDo(u, scope, nodeOutputs)
			if err != nil {
				return StepResult{}, err
			}
			if offered {
				return StepResult{NodeID: nodeID, Activation: u.activation, Prompt: prompt}, nil
			}
			// offerDo settled the do inline (an index-render failure); keep walking.
			continue
		}
		// Every other ready unit settles synchronously through the SAME path engine.Run
		// drives (exec, settle, silent, skip-cascade, aggregate), so the event types before
		// and after each do match a synchronous run's (fold-invisible run identity aside).
		if err := d.runUnit(u, scope, nodeOutputs); err != nil {
			return StepResult{}, err
		}
		if err := d.maybeSnapshot(false); err != nil {
			return StepResult{}, err
		}
	}
	return d.stepSeal(units)
}

// offerDo activates a ready do (write-once node.activated) and returns its rendered
// prompt for the agent to perform. It writes NO effect record — Settle owns the effect
// pair — so a re-Step before Settle re-offers the SAME do (the activation dedups) rather
// than settling it. An index-render failure settles the do failed{detail} inline
// (offered=false), mirroring runDo's ⚑B2 arm; any other render error is loud.
func (d *driver) offerDo(u planUnit, scope, nodeOutputs map[string]string) (offered bool, nodeID, prompt string, err error) {
	if err := d.crashAt(crashBeforeActivate, u.activation); err != nil {
		return false, "", "", err
	}
	if err := d.appendActivated(u); err != nil {
		return false, "", "", err
	}
	if err := d.crashAt(crashAfterActivate, u.activation); err != nil {
		return false, "", "", err
	}
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return false, "", "", err
	}
	prompt, err = renderPrompt(u.leaf.raw, view)
	if err != nil {
		var ire *indexRenderError
		if errors.As(err, &ire) {
			if serr := d.settleIndexRenderFailed(u, ire.detail); serr != nil {
				return false, "", "", serr
			}
			d.record(u.nodeID, "", scope, nodeOutputs)
			return false, "", "", nil
		}
		return false, "", "", fmt.Errorf("lumen: step: do %q prompt: %w", u.nodeID, err)
	}
	return true, u.nodeID, prompt, nil
}

// settleDoTurn records the effect + outcome for a do the agent performed, with the
// agent's self-reported outcome substituted for host.RunDo. It writes the same
// effect.scheduled → effect.settled → outcome.settled sequence runDo writes (the
// effect.settled session ref aside, which folds to a no-op), so the stepper journal folds
// to the same normalized state as engine.Run. The effect.scheduled is written
// HERE (not in Step) so the at-most-once window is confined to Settle: a crash after it
// but before outcome.settled settles the do FAILED on re-claim (interruptedEffects).
func (d *driver) settleDoTurn(u planUnit, scope, nodeOutputs map[string]string, agentOutcome, agentOutput string) error {
	// Activate-if-needed (idempotent — Step already did this; robust when Settle is
	// called without a prior Step), mirroring runUnit's appendActivated-before-runDo.
	if err := d.appendActivated(u); err != nil {
		return err
	}
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return err
	}
	prompt, err := renderPrompt(u.leaf.raw, view)
	if err != nil {
		var ire *indexRenderError
		if errors.As(err, &ire) {
			if serr := d.settleIndexRenderFailed(u, ire.detail); serr != nil {
				return serr
			}
			d.record(u.nodeID, "", scope, nodeOutputs)
			return nil
		}
		return fmt.Errorf("lumen: settle: do %q prompt: %w", u.nodeID, err)
	}

	effectIdem := d.streamID + ":" + u.nodeID + ":do:" + strconv.Itoa(activationAttempt(u.activation)+1)
	spec := effectSpec{Prompt: prompt, AgentRef: u.leaf.agentRef}
	specBytes, err := canonPayload(spec)
	if err != nil {
		return fmt.Errorf("lumen: settle: do %q spec hash: %w", u.nodeID, err)
	}
	specHash := sha256.Sum256(specBytes)
	if err := d.append(EventEffectScheduled, effectIdem+":sched", effectScheduledPayload{
		Activation: u.activation,
		Effect:     "do",
		IdemToken:  effectIdem,
		Policy:     PolicyAtMostOnce,
		SpecHash:   hex.EncodeToString(specHash[:]),
		Spec:       spec,
	}); err != nil {
		return err
	}
	// The agent's off-journal work already ran (before Settle). These boundaries map the
	// interrupted-effect window onto the Settle call: a crash here settles the do FAILED
	// on re-claim (at-most-once, no re-act — settleInterruptedEffect).
	if err := d.crashAt(crashBeforeAct, u.activation); err != nil {
		return err
	}
	if err := d.crashAt(crashAfterAct, u.activation); err != nil {
		return err
	}

	nodeOutcome, effResult, detail, out := foldAgentOutcome(agentOutcome, agentOutput)
	if err := d.append(EventEffectSettled, effectIdem+":done", effectSettledPayload{
		Activation:  u.activation,
		IdemToken:   effectIdem,
		Result:      effResult,
		NodeOutcome: nodeOutcome,
		Output:      out,
		Detail:      detail,
	}); err != nil {
		return err
	}
	if err := d.appendSettled(u.activation, nodeOutcome, out, detail); err != nil {
		return err
	}
	d.record(u.nodeID, out, scope, nodeOutputs)
	return d.crashAt(crashAfterSettle, u.activation)
}

// stepSeal seals the run when every unit has settled — the identical seal Run/Resume/
// Advance emit (final snapshot, then run.closed with the fold-aggregated outcome). A
// walk that ends with an unsettled, non-deferred unit is a stall (a dangling dep, a
// lowering error, or an unsupported do-bodied shape the linear stepper cannot drive):
// surfaced loudly rather than sealed with a bogus outcome.
func (d *driver) stepSeal(units []planUnit) (StepResult, error) {
	if !d.allUnitsSettled(units) {
		return StepResult{}, fmt.Errorf("%w: stream %q (a non-do unit is unsettled with no ready do — a dangling dep or an unsupported v1 shape)", ErrAdvanceStalled, d.streamID)
	}
	runOutcome := d.st().runOutcome()
	if err := d.maybeSnapshot(true); err != nil {
		return StepResult{}, err
	}
	if err := d.crashAt(crashBeforeRunClosed, d.streamID); err != nil {
		return StepResult{}, err
	}
	if err := d.append(EventRunClosed, d.streamID+":run:closed", runClosedPayload{Outcome: runOutcome}); err != nil {
		return StepResult{}, err
	}
	return StepResult{Done: true, Outcome: runOutcome}, nil
}

// findStepperDoUnit returns the do unit for a bare node id (single-attempt: one unit per
// node id, the linear v1 shape).
func findStepperDoUnit(units []planUnit, node string) (planUnit, bool) {
	for i := range units {
		u := units[i]
		if u.kind == unitLeaf && u.leaf.kind == ir.NodeDo && u.nodeID == node {
			return u, true
		}
	}
	return planUnit{}, false
}

// foldAgentOutcome maps the agent's self-reported --outcome onto the node outcome,
// effect result, detail, and output the settle records. pass/degraded/failed reuse the
// host-path foldDoResult, so a stepper settle is byte-identical (modulo the provenance
// session ref, which folds to a no-op) to a StubHost RunDo of the same outcome — the
// determinism oracle. A missing/unknown outcome is fail-closed to failed (the v1
// self-report discipline, mirroring LumenOutcomeForGCOutcome). pending settles the node
// pending (the non-consuming re-poll outcome) with an ok effect result.
func foldAgentOutcome(agentOutcome, agentOutput string) (nodeOutcome, effResult, detail, out string) {
	switch agentOutcome {
	case OutcomePass:
		n, e, dt, o, _ := foldDoResult(enginehost.DoResult{Outcome: enginehost.OutcomePass, Output: agentOutput}, nil)
		return n, e, dt, o
	case OutcomeDegraded:
		n, e, dt, o, _ := foldDoResult(enginehost.DoResult{Outcome: enginehost.OutcomeDegraded, Output: agentOutput}, nil)
		return n, e, dt, o
	case OutcomePending:
		return OutcomePending, EffectResultOK, "", agentOutput
	default:
		// "fail", "failed", "", or any unrecognized self-report → fail-closed to failed.
		n, e, dt, o, _ := foldDoResult(enginehost.DoResult{Outcome: enginehost.OutcomeFailed, Output: agentOutput}, nil)
		return n, e, dt, o
	}
}
