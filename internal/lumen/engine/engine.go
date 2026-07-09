// Package engine is the Lumen executor: a single-writer driver that runs a
// compiled Lumen formula end-to-end on the graphstore journal substrate. It
// repeats a decide -> persist -> act -> persist cycle: it appends a typed
// journal event, folds it through the pure lumenReducer (v2), and applies the
// resulting Tier-A delta so the node/edge/frontier projection advances in
// lockstep with the log.
//
// P4.2 folds a real DAG. The plan (internal/lumen/engine/plan.go) lowers a
// formula to activations carrying dependency edges; the reducer builds the
// frontier as deps-settled readiness with skip-cascade (a node whose upstream
// failed is settled `skipped`, not run). The DAG arms scatter(members) and
// gather(authored) are implemented; the remaining node kinds are refused with
// ErrUnsupportedNode before any effect runs (blueprint §7 pressure valve).
package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/exechost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// leaseHolder identifies the executor as the writer-lease holder.
const leaseHolder = "lumen-engine"

// lumenRepeatLoopCap is the mandatory hard bound on a repeat loop's iterations
// (mirroring the reference runner). A cond that never turns truthy terminates
// after this many attempts, settling the loop failed{reason:"loop_cap"} — no
// unbounded loop is expressible.
const lumenRepeatLoopCap = 32

// leaseTTL bounds how long a run holds the writer lease without renewal.
const leaseTTL = 30 * time.Second

// RunResult is the outcome of a completed run.
type RunResult struct {
	// StreamID is the journal stream (and root node id) the run wrote to.
	StreamID string
	// Outcome is the run's aggregated outcome.
	Outcome string
	// NodeOutputs maps each executed node id to its captured output value.
	NodeOutputs map[string]string
	// Events is the committed journal for the run in seq order: the full journal
	// for an untruncated stream, or the surviving tail after the latest retention
	// cut for a truncated one (a Resume of a retention-truncated stream returns the
	// surviving events, from seq 1, not just the post-snapshot tail).
	Events []graphstore.StoredEvent
}

// RegisterVocabulary registers the executor's frozen event vocabulary against
// the store so Append accepts its events. Registration is idempotent.
func RegisterVocabulary(store *graphstore.Store) {
	for _, t := range EventTypes {
		store.RegisterEventType(Engine, t)
	}
}

// DefaultSnapshotEvery is the recommended fold-snapshot cadence for a
// long-running or resumable stream: anchor a snapshot roughly every this many
// committed events. It is a documented default, NOT the library default —
// Options.SnapshotEvery is 0 (disabled) unless a caller opts in.
const DefaultSnapshotEvery = 256

// Options tune a run. The zero value (nil Host, SnapshotEvery 0) refuses a do
// node with ErrUnsupportedNode and writes no snapshots.
type Options struct {
	// Host runs agent `do` steps. Nil refuses do nodes.
	Host enginehost.AgentHost
	// SnapshotEvery anchors a fold snapshot at the next unit boundary once this
	// many events have accumulated since the last snapshot, and once more at the
	// run seal (before run.closed). 0 disables snapshotting entirely (opt-in): the
	// disabled path is behaviorally INERT and fold-compatible — it writes no
	// snapshot.anchored events, no snapshots rows, and triggers no truncation, so
	// the event-TYPE sequence matches a P4.2 run. It is NOT chain-byte-identical to
	// a P4.2 binary for input-bearing or do runs, whose run.started (input_hash) and
	// effect.settled (node_outcome) payloads carry P4.3 fields; an input-less
	// exec-only run is byte-identical. Snapshots are additive — a snapshot.anchored
	// event folds to a no-op — so enabling them never changes the Tier-A projection,
	// only bounds the journal and enables Resume.
	SnapshotEvery int
	// PoolRouter is the L0 pool seam consulted ONLY by Advance. When non-nil, every
	// `do` node lowers pool-mode: Advance materializes it as a claimable Tier-B work
	// bead a session POOL claims and settles asynchronously, and PARKS instead of
	// blocking on a Host. agentRef selects the pool ("" = the run's default route);
	// ok=false is a loud config error (a pool-mode do with no resolvable route),
	// never a silent inline fallback. Run/RunWithOptions/Resume ignore this field
	// and run every do inline through Host. ZERO role names: a route is a
	// config-resolved pool target, exactly like gc.routed_to.
	PoolRouter func(agentRef string) (route string, ok bool)
}

// Run executes doc with no agent host — the exec-only path.
func Run(ctx context.Context, store *graphstore.Store, doc *ir.IR, input map[string]any) (RunResult, error) {
	return RunWithOptions(ctx, store, doc, input, Options{})
}

// RunWithOptions executes doc against store, threading input into {{var}}
// interpolation, and returns the run's outcome, per-node outputs, and the
// committed journal. It is the single writer for the run's stream: it acquires
// the writer lease, appends run.started, drives each plan unit (emitting
// node.activated, then either running it or settling it skipped, then
// outcome.settled), and appends run.closed with the fold-aggregated outcome.
func RunWithOptions(ctx context.Context, store *graphstore.Store, doc *ir.IR, input map[string]any, opts Options) (RunResult, error) {
	if store == nil {
		return RunResult{}, fmt.Errorf("lumen: nil store")
	}
	if doc == nil {
		return RunResult{}, fmt.Errorf("lumen: nil IR document")
	}

	units, err := buildUnits(doc.Nodes, opts.Host != nil, opts.Host != nil)
	if err != nil {
		return RunResult{}, err
	}

	streamID := streamIDForRun(doc.Name, opts.Host != nil)
	RegisterVocabulary(store)

	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: acquire writer lease %q: %w", streamID, err)
	}
	// NOTE (crash-harness fidelity): an injected crash (crashHook error) unwinds
	// through this defer and RELEASES the lease, whereas a real SIGKILL leaves it
	// held until leaseTTL (~30s). That difference is behaviorally invisible to
	// resume: AcquireWriterLease steals a same-holder row regardless of expiry
	// (lease.go), so resume re-acquires as leaseHolder either way.
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	reducer := lumenReducer{}
	d := &driver{
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
		return RunResult{}, err
	}

	// Crash boundary: after run.started, before the first unit (test-only, inert).
	if err := d.crashAt(crashAfterRunStarted, streamID); err != nil {
		return RunResult{}, err
	}

	nodeOutputs := make(map[string]string)
	scope := baseScope(input)

	for i := range units {
		if err := d.runUnit(units[i], scope, nodeOutputs); err != nil {
			return RunResult{}, err
		}
		if err := d.maybeSnapshot(false); err != nil {
			return RunResult{}, err
		}
	}

	runOutcome := d.st().runOutcome()
	// Seal snapshot: anchor the final state before run.closed so a resume of a
	// sealed stream loads the whole run from one snapshot plus the closed marker.
	if err := d.maybeSnapshot(true); err != nil {
		return RunResult{}, err
	}
	// Crash boundary: work done and sealed-snapshot anchored, before run.closed.
	if err := d.crashAt(crashBeforeRunClosed, streamID); err != nil {
		return RunResult{}, err
	}
	if err := d.append(EventRunClosed, streamID+":run:closed", runClosedPayload{Outcome: runOutcome}); err != nil {
		return RunResult{}, err
	}

	events, err := store.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: read stream %q: %w", streamID, err)
	}
	return RunResult{StreamID: streamID, Outcome: runOutcome, NodeOutputs: nodeOutputs, Events: events}, nil
}

// driver holds the single-writer append/fold/project loop state for one run.
type driver struct {
	ctx      context.Context
	store    *graphstore.Store
	streamID string
	irVer    string
	epoch    uint64
	reducer  lumenReducer
	state    fold.State
	head     uint64
	host     enginehost.AgentHost
	// input is the run input (typed), threaded into the closed-expression scope a
	// retry/repeat loop evaluates its attempts/cond against (typed comparisons —
	// `iteration >= max_check_attempts`). It is the same map baseScope seeds the
	// string interpolation scope from.
	input map[string]any

	// snapshotEvery is the fold-snapshot cadence (0 = disabled); sinceSnapshot
	// counts committed events since the last anchored snapshot.
	snapshotEvery int
	sinceSnapshot int

	// Resume-only memoization, keyed by activation. On a fresh run both are nil
	// and the memoization is skipped. crashInterrupted holds effects that were
	// scheduled but never settled (a crash mid-effect): they settle FAILED under
	// at-most-once without re-acting. settledEffects holds effects that settled
	// but whose outcome.settled never committed (a crash in the
	// effect.settled -> outcome.settled window): the node settles from the
	// recorded effect result, again without re-invoking the host (B1). Consulting
	// them inside runUnit makes reload/settle idempotent at ANY nesting level, so
	// a settled combine member nested in a gather is never re-executed (B2).
	crashInterrupted map[string]string               // activation -> idem token
	settledEffects   map[string]effectSettledPayload // activation -> recorded settlement
}

// st returns the driver's live fold state as the concrete lumenState so the
// decide phase can read dependency outcomes and aggregate results.
func (d *driver) st() *lumenState { return d.state.(*lumenState) }

// runUnit drives one plan unit through the decide -> persist -> act -> persist
// cycle. A silent (pure lit/interp) unit only computes its scope value. Every
// other unit emits node.activated, then — if a leaf's dependency settled with a
// blocking outcome — settles `skipped` (the skip-cascade), otherwise runs and
// settles its real outcome.
func (d *driver) runUnit(u planUnit, scope, nodeOutputs map[string]string) error {
	if u.silent {
		val, err := evalSilent(u.leaf, scope)
		if err != nil {
			return err
		}
		scope[u.nodeID] = val
		nodeOutputs[u.nodeID] = val
		return nil
	}

	// Resume memoization (B1/B2): before touching the journal, reload an
	// already-settled unit, settle a do node from its recorded effect, or settle a
	// crash-interrupted effect — all from the journal, never by re-acting. Because
	// this lives in runUnit (not only the top-level resume loop), it applies at any
	// nesting level, so a settled combine member inside a gather is reloaded, not
	// re-run. On a fresh run the maps are nil and this is a no-op.
	if handled, err := d.resumeMemoized(u, scope, nodeOutputs); err != nil || handled {
		return err
	}

	// Crash boundary (a): after the decide phase picked this unit, before its
	// first append (node.activated). Test-only; inert in production.
	if err := d.crashAt(crashBeforeActivate, u.activation); err != nil {
		return err
	}

	if err := d.appendActivated(u); err != nil {
		return err
	}

	// Skip-cascade applies to every kind, gated on blocking `after` deps only
	// (H1): a scatter/gather whose non-member `after` gate failed is SKIPPED — its
	// members never run and its aggregate settles skipped. A failed drain MEMBER
	// does not trigger this (members drain into the aggregate).
	if d.blocked(u) {
		if err := d.appendSettled(u.activation, OutcomeSkipped, "", "skipped: upstream dependency did not pass"); err != nil {
			return err
		}
		return d.crashAt(crashAfterSettle, u.activation) // boundary (d)
	}

	// Drain-aggregate skip-cascade (N-1): a scatter aggregate or gather whose
	// every drain member settled skipped/canceled did no work at all. It must
	// itself SKIP — its combine / authored body must NOT run (no side effects) —
	// and the skip cascades to its dependents, exactly like a failed `after` gate.
	// A single member that RAN (pass/degraded/failed) makes it drain instead. This
	// runs BEFORE runScatter/runGather so an all-skip aggregate never executes.
	if d.aggregateAllSkipped(u) {
		if err := d.appendSettled(u.activation, OutcomeSkipped, "", "skipped: every drain member skipped (nothing ran)"); err != nil {
			return err
		}
		return d.crashAt(crashAfterSettle, u.activation) // boundary (d)
	}

	var runErr error
	switch u.kind {
	case unitLeaf:
		runErr = d.runLeaf(u, scope, nodeOutputs)
	case unitScatterAgg:
		runErr = d.runScatter(u, nodeOutputs)
	case unitGather:
		runErr = d.runGather(u, scope, nodeOutputs)
	case unitLoop:
		runErr = d.runLoop(u, scope, nodeOutputs)
	default:
		runErr = fmt.Errorf("%w: unit %q", ErrUnsupportedNode, u.nodeID)
	}
	if runErr != nil {
		return runErr
	}
	// Crash boundary (d): after the unit's outcome.settled committed, before the
	// next unit. Test-only; inert in production.
	return d.crashAt(crashAfterSettle, u.activation)
}

// blocked reports whether any of a unit's blocking `after` deps settled with a
// blocking outcome (failed / canceled / skipped) — the trigger for the
// skip-cascade. Drain member deps are deliberately excluded (a failed member
// drains into its aggregate, it does not skip it).
func (d *driver) blocked(u planUnit) bool {
	st := d.st()
	for _, dep := range u.afterDeps {
		if outcome, settled := st.outcomeOf(dep); settled && isBlocking(outcome) {
			return true
		}
	}
	return false
}

// aggregateAllSkipped reports whether u is a drain aggregate (scatter aggregate
// or gather) whose every drain member settled skipped/canceled — nothing ran, so
// the aggregate itself must SKIP (N-1) rather than drain-and-run its combine. A
// unit with no drain members is never "all skipped" (there is nothing that could
// have skipped). It reads memberDeps — the same edge set the reducer's ready()
// gates on (nodeState.Members) — so the executor and the fold stay in agreement.
func (d *driver) aggregateAllSkipped(u planUnit) bool {
	if len(u.memberDeps) == 0 {
		return false
	}
	st := d.st()
	for _, m := range u.memberDeps {
		o, settled := st.outcomeOf(m)
		if !settled || !didNotRun(o) {
			return false
		}
	}
	return true
}

// runLeaf executes an exec / settle / do leaf and settles its outcome.
func (d *driver) runLeaf(u planUnit, scope, nodeOutputs map[string]string) error {
	switch u.leaf.kind {
	case ir.NodeExec:
		script := interpolate(u.leaf.script, scope)
		// Crash boundary (b): after node.activated, before the shell runs.
		if err := d.crashAt(crashBeforeAct, u.activation); err != nil {
			return err
		}
		stdout, _, exitCode, runErr := exechost.Run(d.ctx, u.leaf.program, script, u.leaf.cwd, u.leaf.env)
		if runErr != nil {
			return fmt.Errorf("lumen: exec %q: %w", u.nodeID, runErr)
		}
		// Crash boundary (c): after the shell ran, before outcome.settled. On
		// resume the exec re-runs (at-least-once) — it carries no effect record.
		if err := d.crashAt(crashAfterAct, u.activation); err != nil {
			return err
		}
		output := strings.TrimRight(stdout, "\n")
		outcome := outcomeForExit(exitCode, u.leaf.passCodes)
		// Stamp the retry classification DATA: a failed exec whose exit code is in
		// exitMap.retryable is infrastructure-retryable (the retry arm reads this from
		// the fold, never from a driver-side inference). A non-loop exec (empty
		// retryable set) never sets it, so the settle is byte-identical to pre-L5.
		retryable := outcome == OutcomeFailed && intInSlice(exitCode, u.leaf.retryableCodes)
		if err := d.appendSettledRetryable(u.activation, outcome, output, "", retryable); err != nil {
			return err
		}
		d.record(u.nodeID, output, scope, nodeOutputs)
		return nil

	case ir.NodeSettle:
		value := ""
		if raw, ok := u.leaf.raw["value"]; ok {
			v, err := evalValue(raw, scope)
			if err != nil {
				return fmt.Errorf("lumen: settle %q value: %w", u.nodeID, err)
			}
			value = v
		}
		outcome := u.leaf.outcome
		if outcome == "" {
			outcome = OutcomePass
		}
		if err := d.appendSettled(u.activation, outcome, value, ""); err != nil {
			return err
		}
		d.record(u.nodeID, value, scope, nodeOutputs)
		return nil

	case ir.NodeDo:
		return d.runDo(u, scope, nodeOutputs)

	default:
		return fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, u.leaf.kind, u.nodeID)
	}
}

// runDo runs one agent `do` step through the configured host under the
// memoized-effect discipline (blueprint §3.3): it appends effect.scheduled
// BEFORE acting, calls host.RunDo (the only side-effecting line), then appends
// effect.settled, then outcome.settled. A nil host is impossible here
// (buildUnits refuses do without one) but is guarded defensively.
func (d *driver) runDo(u planUnit, scope, nodeOutputs map[string]string) error {
	if d.host == nil {
		return fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, u.leaf.kind, u.nodeID)
	}
	prompt, err := renderPrompt(u.leaf.raw, scope)
	if err != nil {
		return fmt.Errorf("lumen: do %q prompt: %w", u.nodeID, err)
	}
	// The effect suffix is the live attempt number (1-based): attempt 0 derives
	// `:do:1` — byte-identical to the single-attempt path — and a retry/repeat
	// re-attempt N mints a FRESH token `:do:<N+1>`, so each attempt gets its own
	// at-most-once effect record (L5). The attempt is read from the activation key,
	// not a driver-side counter, so a re-Advance re-derives the same token.
	effectIdem := d.streamID + ":" + u.nodeID + ":do:" + strconv.Itoa(activationAttempt(u.activation)+1)
	spec := effectSpec{Prompt: prompt, AgentRef: u.leaf.agentRef}
	specBytes, err := canonPayload(spec)
	if err != nil {
		return fmt.Errorf("lumen: do %q spec hash: %w", u.nodeID, err)
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

	// Crash boundary (b): after effect.scheduled, before the agent runs. On resume
	// the scheduled-but-unsettled effect settles FAILED without re-invoking the
	// host — the at-most-once contract (host called 0 times across this crash).
	if err := d.crashAt(crashBeforeAct, u.activation); err != nil {
		return err
	}

	result, runErr := d.host.RunDo(d.ctx, enginehost.DoRequest{
		RunID:      d.streamID,
		NodeID:     u.nodeID,
		Activation: u.activation,
		IdemToken:  effectIdem,
		Prompt:     prompt,
		AgentRef:   u.leaf.agentRef,
	})
	// Crash boundary (c): the agent ran (host called exactly once) but its
	// settlement is not yet recorded. On resume the effect settles FAILED without
	// re-invoking the host — at-most-once (host called ≤1 across this crash).
	if err := d.crashAt(crashAfterAct, u.activation); err != nil {
		return err
	}
	nodeOutcome, effResult, detail, out, session := foldDoResult(result, runErr)

	if err := d.append(EventEffectSettled, effectIdem+":done", effectSettledPayload{
		Activation:  u.activation,
		IdemToken:   effectIdem,
		Result:      effResult,
		NodeOutcome: nodeOutcome,
		Output:      out,
		Session:     session,
		Detail:      detail,
	}); err != nil {
		return err
	}
	if err := d.appendSettled(u.activation, nodeOutcome, out, detail); err != nil {
		return err
	}
	d.record(u.nodeID, out, scope, nodeOutputs)
	return nil
}

// runScatter settles a scatter aggregate from its members' outcomes. It is
// reached only when at least one member RAN — an all-skipped/canceled member set
// is intercepted upstream (aggregateAllSkipped) and settles the aggregate
// `skipped`, not `degraded` (N-1/N-3). A member failure drains into the
// aggregate rather than skip-cascading it. With on_fail "stop", any
// failed/canceled member fails the scatter. Otherwise the outcome reflects the
// degree of success (M1): if NO member passed and at least one failed, the
// honest outcome is `failed` (a total loss, not a partial success); a mix of
// pass and non-pass is `degraded`; all-pass is `pass`.
func (d *driver) runScatter(u planUnit, nodeOutputs map[string]string) error {
	st := d.st()
	anyPass := false
	anyNonPass := false
	anyBlocking := false
	for _, m := range u.members {
		o, settled := st.outcomeOf(m)
		if !settled {
			continue
		}
		if o == OutcomePass {
			anyPass = true
		} else {
			anyNonPass = true
		}
		if o == OutcomeFailed || o == OutcomeCanceled {
			anyBlocking = true
		}
	}
	outcome := OutcomePass
	switch {
	case u.onFail == "stop" && anyBlocking:
		outcome = OutcomeFailed
	case anyBlocking && !anyPass:
		outcome = OutcomeFailed
	case anyNonPass:
		outcome = OutcomeDegraded
	}
	if err := d.appendSettled(u.activation, outcome, "", ""); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// runGather drains its scatter's members head-of-line (a node.decision fold
// checkpoint per member in member order, even if settlements arrived out of
// order), runs the authored combine block, and settles the gather with the
// combine's aggregated outcome. gatherMembers exclude silent (lit/interp)
// members (L2): those never settle, so a fold checkpoint over them is
// meaningless — the exclusion happens at lowering (scatterMembers).
func (d *driver) runGather(u planUnit, scope, nodeOutputs map[string]string) error {
	// NOTE (crash-harness fidelity): the in-process seam fires only BETWEEN these
	// appends, but a real kill can truncate the head-of-line node.decision suffix
	// mid-loop. Convergence composes per-event: resume re-emits these checkpoints in
	// order and the append dedup short-circuit (append, "Idempotent replay") skips
	// the ones that committed and lands the missing tail, so a partial suffix heals.
	for _, m := range u.gatherMembers {
		if err := d.append(EventNodeDecision, d.streamID+":"+u.activation+":ckpt:"+m, nodeDecisionPayload{
			Activation:     u.activation,
			Decision:       DecisionFoldCkpt,
			NextMember:     m,
			AccumulatorRef: u.activation,
		}); err != nil {
			return err
		}
	}

	// Run each authored-combine member through the SAME execution path as any
	// other unit (B1): an exec/do inside a combine actually runs and settles its
	// REAL outcome (feeding the fold), rather than being mistaken for a settle.
	// Skip-cascade and silent (lit/interp) handling come for free from runUnit.
	for i := range u.combine {
		if err := d.runUnit(u.combine[i], scope, nodeOutputs); err != nil {
			return err
		}
	}

	if err := d.appendSettled(u.activation, d.combineOutcome(u.combine), "", ""); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// runLoop drives a retry/repeat attempt loop inline (Run/Resume). It re-attempts
// the leaf body until the authored exit predicate says stop, then settles the loop
// node. Each attempt runs through the SAME runUnit path a fresh unit does, so
// per-attempt effect tokens (S4), crash boundaries, and resume memoization apply
// for free. Keep-judgment-out-of-Go: the re-run decision is the closed expression
// evaluated over folded attempt outcomes (loopDecide), never a Go branch on an
// outcome value.
func (d *driver) runLoop(u planUnit, scope, nodeOutputs map[string]string) error {
	spec := u.loop
	if spec == nil {
		return fmt.Errorf("lumen: loop %q missing spec", u.nodeID)
	}

	// retry: evaluate the attempts budget ONCE. An invalid budget (non-integer or
	// < 1) settles the loop failed{invalid_input} with ZERO attempts (reference
	// parity). repeat has no budget expression (it is capped at lumenRepeatLoopCap).
	maxAttempts := 0
	if spec.irKind == ir.NodeRetry {
		n, ok := evalAttempts(spec.attemptsExpr, d.loopScope(spec, 0, nil, nodeOutputs))
		if !ok {
			return d.settleLoop(u, OutcomeFailed, "", "invalid_input", nil, scope, nodeOutputs)
		}
		maxAttempts = n
	}

	for attempt := 0; ; attempt++ {
		bodyAct := activationForAttempt(spec.bodyNodeID, attempt)
		bn := d.st().Nodes[bodyAct]
		if bn == nil || !bn.Settled {
			if err := d.runAttempt(u, attempt, maxAttempts, scope, nodeOutputs); err != nil {
				return err
			}
			bn = d.st().Nodes[bodyAct]
			if bn == nil || !bn.Settled {
				// An engine-inline attempt always settles in-pass; if it did not, the
				// body lowering or host is broken — surface it loudly.
				return fmt.Errorf("lumen: loop %q attempt %d did not settle in-pass", u.nodeID, attempt)
			}
		}
		cont, err := d.loopDecide(u, attempt, maxAttempts, bn, scope, nodeOutputs)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
}

// runAttempt mints one loop attempt: it emits attempt.minted (bookkeeping), binds
// the 1-based iteration for a repeat body's prompt scope (loop-local), and drives
// the synthesized per-attempt leaf unit through runUnit. The attempt's activation
// is bodyID:N — a NEW activation ⇒ a fresh claim/settle/effect token per attempt.
func (d *driver) runAttempt(u planUnit, attempt, maxAttempts int, scope, nodeOutputs map[string]string) error {
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
	// A repeat body's prompt/interp may reference {{iteration}} (1-based). Bind it
	// for THIS attempt only, then restore — the binding is loop-local (reference
	// parity). retry has no iteration binding.
	restore, had := "", false
	if spec.irKind == ir.NodeRepeat {
		restore, had = scope[spec.iterationName]
		scope[spec.iterationName] = strconv.Itoa(attempt + 1)
	}
	err := d.runUnit(au, scope, nodeOutputs)
	if spec.irKind == ir.NodeRepeat {
		if had {
			scope[spec.iterationName] = restore
		} else {
			delete(scope, spec.iterationName)
		}
	}
	return err
}

// attemptUnit synthesizes the per-attempt leaf unit for a loop. It carries the
// SAME `after` gates as the loop (the attempt-edge rule) and NO edge to the prior
// attempt — the driver's sequential minting is the sequencer, not a fold edge (an
// edge to attempt N would skip-cascade every re-attempt off the prior failure and
// project a bare-id self-loop). It is parented under the loop activation, so it
// stays out of the run's top-level outcome aggregation.
func (d *driver) attemptUnit(u planUnit, attempt int) planUnit {
	spec := u.loop
	return planUnit{
		kind:       unitLeaf,
		activation: activationForAttempt(spec.bodyNodeID, attempt),
		nodeID:     spec.bodyNodeID,
		irKind:     spec.bodyIRKind,
		parent:     u.activation, // loopID:0
		afterDeps:  u.afterDeps,
		rawAfter:   u.rawAfter,
		leaf:       spec.body,
	}
}

// loopDecide evaluates the exit predicate after attempt N settled: it either
// settles the loop (returns continueLoop=false) or decides to re-attempt (returns
// continueLoop=true, so the next runAttempt mints attempt N+1). The whole decision
// is the closed expression over folded outcomes plus the folded retryable flag —
// no Go branch on an outcome value drives a re-run.
func (d *driver) loopDecide(u planUnit, attempt, maxAttempts int, bn *nodeState, scope, nodeOutputs map[string]string) (bool, error) {
	spec := u.loop
	switch spec.irKind {
	case ir.NodeRepeat:
		iteration := attempt + 1
		truthy, err := evalCondTruthy(spec.condExpr, d.loopScope(spec, iteration, bn, nodeOutputs))
		if err != nil {
			return false, err
		}
		if truthy {
			// Exit returning the last body result (reference: returns lastResult).
			return false, d.settleLoop(u, bn.Outcome, bn.Output, "", nil, scope, nodeOutputs)
		}
		if iteration >= lumenRepeatLoopCap {
			return false, d.settleLoop(u, OutcomeFailed, bn.Output, "loop_cap", nil, scope, nodeOutputs)
		}
		return true, nil

	case ir.NodeRetry:
		n := attempt + 1 // 1-based attempt number
		if bn.Outcome != OutcomeFailed {
			// Success (or a non-failed outcome) returns immediately.
			return false, d.settleLoop(u, bn.Outcome, bn.Output, "", nil, scope, nodeOutputs)
		}
		if !bn.Retryable {
			// A non-retryable failure stops early, stamping the unused budget.
			rem := maxAttempts - n
			return false, d.settleLoop(u, OutcomeFailed, bn.Output, "", &rem, scope, nodeOutputs)
		}
		if n >= maxAttempts {
			// The budget is exhausted by retryable failures.
			rem := 0
			return false, d.settleLoop(u, OutcomeFailed, bn.Output, "exhausted", &rem, scope, nodeOutputs)
		}
		return true, nil

	default:
		return false, fmt.Errorf("lumen: loop %q unknown kind %q", u.nodeID, spec.irKind)
	}
}

// settleLoop settles the loop node and records its output into scope + nodeOutputs
// so a downstream {{loopID}} ref and RunResult.NodeOutputs see the satisfying
// attempt's output (the body binding scope[bodyID] was set by that attempt's own
// record).
func (d *driver) settleLoop(u planUnit, outcome, output, reason string, retriesRemaining *int, scope, nodeOutputs map[string]string) error {
	if err := d.appendLoopSettled(u.activation, outcome, output, reason, retriesRemaining); err != nil {
		return err
	}
	d.record(u.nodeID, output, scope, nodeOutputs)
	return nil
}

// loopScope builds the closed-expression evaluation scope for a loop's cond /
// attempts: the iteration counter, the just-settled attempt binding (bn), the run
// input (typed), and the run's other settled node outputs/outcomes (max attempt
// per node id). bn is nil when evaluating a retry's attempts expression (before any
// attempt).
func (d *driver) loopScope(spec *loopSpec, iteration int, bn *nodeState, nodeOutputs map[string]string) loopScope {
	outcomes := map[string]string{}
	best := map[string]int{}
	for act, n := range d.st().Nodes {
		if !n.Settled || !ranOutcome(n.Outcome) {
			continue
		}
		id := activationNodeID(act)
		att := activationAttempt(act)
		if prev, ok := best[id]; ok && att <= prev {
			continue
		}
		best[id] = att
		outcomes[id] = n.Outcome
	}
	sc := loopScope{
		iterationName: spec.iterationName,
		iteration:     iteration,
		input:         d.input,
		nodeOutputs:   nodeOutputs,
		nodeOutcomes:  outcomes,
	}
	if bn != nil {
		sc.bodyName = spec.bodyNodeID
		sc.bodyOutcome = bn.Outcome
		sc.bodyOutput = bn.Output
	}
	return sc
}

// evalAttempts evaluates a retry's attempts expression and reports the integer
// budget, or ok=false for a non-integer / < 1 value (reference invalid_input).
func evalAttempts(expr json.RawMessage, scope loopScope) (int, bool) {
	v, err := evalClosedExpr(expr, scope)
	if err != nil {
		return 0, false
	}
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	if f < 1 || f != float64(int64(f)) {
		return 0, false
	}
	return int(f), true
}

// evalCondTruthy evaluates a repeat's `until` condition and reports its truthiness.
func evalCondTruthy(expr json.RawMessage, scope loopScope) (bool, error) {
	v, err := evalClosedExpr(expr, scope)
	if err != nil {
		return false, err
	}
	return isExprTruthy(v), nil
}

// combineOutcome aggregates the settled outcomes of a gather's combine members:
// a blocking outcome (failed/canceled/skipped) dominates to failed, then
// degraded, else pass. Silent members contribute nothing (they never settle).
func (d *driver) combineOutcome(combine []planUnit) string {
	st := d.st()
	var anyFailed, anyDegraded bool
	for i := range combine {
		if combine[i].silent {
			continue
		}
		o, settled := st.outcomeOf(combine[i].activation)
		if !settled {
			continue
		}
		switch {
		case isBlocking(o):
			anyFailed = true
		case o == OutcomeDegraded:
			anyDegraded = true
		}
	}
	switch {
	case anyFailed:
		return OutcomeFailed
	case anyDegraded:
		return OutcomeDegraded
	default:
		return OutcomePass
	}
}

// record threads a settled node's output into the scope and the result map.
func (d *driver) record(nodeID, output string, scope, nodeOutputs map[string]string) {
	scope[nodeID] = output
	nodeOutputs[nodeID] = output
}

// appendActivated emits a node.activated event for a unit, carrying its
// dependency edges (activation keys) so the reducer folds the DAG.
func (d *driver) appendActivated(u planUnit) error {
	return d.append(EventNodeActivated, d.streamID+":"+u.activation+":act", nodeActivatedPayload{
		NodeID:           u.nodeID,
		Activation:       u.activation,
		ParentActivation: u.parent,
		MemberIndex:      u.memberIndex,
		After:            u.afterDeps,
		Members:          u.memberDeps,
		Kind:             string(u.irKind),
	})
}

// appendSettled emits an outcome.settled event for an activation (no retry
// classification — the byte-identical pre-L5 shape used by every non-exec settle).
func (d *driver) appendSettled(activation, outcome, output, detail string) error {
	return d.appendSettledRetryable(activation, outcome, output, detail, false)
}

// appendSettledRetryable emits an outcome.settled carrying the retry
// classification (exec bodies): retryable=false omits the field, so it is
// byte-identical to appendSettled.
func (d *driver) appendSettledRetryable(activation, outcome, output, detail string, retryable bool) error {
	return d.append(EventOutcomeSettled, d.streamID+":"+activation+":settled", outcomeSettledPayload{
		Activation: activation,
		Outcome:    outcome,
		Output:     output,
		Detail:     detail,
		Retryable:  retryable,
	})
}

// appendLoopSettled emits a loop node's own outcome.settled, carrying the
// reason (loop_cap / exhausted / invalid_input) and retries_remaining the retry
// arm stamps (§3.3).
func (d *driver) appendLoopSettled(activation, outcome, output, reason string, retriesRemaining *int) error {
	return d.append(EventOutcomeSettled, d.streamID+":"+activation+":settled", outcomeSettledPayload{
		Activation:       activation,
		Outcome:          outcome,
		Output:           output,
		Reason:           reason,
		RetriesRemaining: retriesRemaining,
	})
}

// appendAttemptMinted emits the bookkeeping attempt.minted for a loop attempt
// (1-based attempt number, remaining budget). It is idem-keyed on the loop
// activation and attempt so a re-Advance/resume dedupes; the reducer folds it to
// a no-op (state is re-derivable from the attempt activation itself).
func (d *driver) appendAttemptMinted(loopAct string, attempt, remaining int) error {
	bodyAttempt := attempt // 0-based body attempt index
	return d.append(EventAttemptMinted, d.streamID+":"+loopAct+":attempt:"+strconv.Itoa(bodyAttempt), attemptMintedPayload{
		Activation: loopAct,
		Attempt:    attempt + 1, // 1-based, mirroring the reference node.attempt counter
		Remaining:  remaining,
	})
}

// intInSlice reports whether xs contains x.
func intInSlice(x int, xs []int) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// append canonicalizes payload, commits it to the journal at head+1, folds the
// committed event, and applies the resulting Tier-A delta in its own
// transaction — the decide -> persist -> act -> persist cycle for one event.
func (d *driver) append(eventType, idemToken string, payload any) error {
	body, err := canonPayload(payload)
	if err != nil {
		return fmt.Errorf("lumen: encoding %s payload: %w", eventType, err)
	}
	ev := graphstore.JournalEvent{
		Type:              eventType,
		IRContractVersion: d.irVer,
		IdemToken:         idemToken,
		Payload:           body,
	}
	res, err := d.store.Append(d.ctx, d.streamID, Engine, d.head, d.epoch, []graphstore.JournalEvent{ev})
	if err != nil {
		return fmt.Errorf("lumen: append %s: %w", eventType, err)
	}
	// Idempotent replay short-circuit (resume re-emission). A crashed run that
	// resumes re-drives units whose events already committed (e.g. a gather's
	// head-of-line node.decision checkpoints emitted before the crash). The store
	// dedups an identical idem token and returns its existing seq without writing
	// a row. That event is ALREADY folded into d.state (resume rebuilt state from
	// the journal) and sits BELOW the live head, so we must neither re-fold it
	// (double-apply) nor move d.head back to the older seq (which would break the
	// next append's expectedVersion CAS).
	//
	// ORDER-DEPENDENCE (L3): this only stays sound because resume re-emits events
	// in their original journal order. A fresh event always lands at head+1; a
	// replayed one is a byte-identical duplicate at its original seq. Re-emitting
	// out of order would either present a stale expectedVersion (ErrWrongExpected-
	// Version) or bind an idem token to a divergent payload (ErrIdemTokenReuse).
	if _, dup := res.Duplicates[0]; dup {
		return nil
	}
	seq := res.FirstSeq

	next, delta, err := d.reducer.Apply(d.state, fold.Event{
		StreamID:          d.streamID,
		Seq:               seq,
		Engine:            Engine,
		Type:              ev.Type,
		IRContractVersion: ev.IRContractVersion,
		IdemToken:         ev.IdemToken,
		Payload:           ev.Payload,
	})
	if err != nil {
		return fmt.Errorf("lumen: fold %s at seq %d: %w", eventType, seq, err)
	}
	d.state = next

	tx, err := d.store.DB().BeginTx(d.ctx, nil)
	if err != nil {
		return fmt.Errorf("lumen: begin projection tx: %w", err)
	}
	if err := d.store.ApplyDelta(d.ctx, tx, delta); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("lumen: apply delta for %s: %w", eventType, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lumen: commit projection for %s: %w", eventType, err)
	}
	d.head = seq
	d.sinceSnapshot++
	return nil
}

// evalSilent computes a pure lit/interp node's value for scope. It emits no
// journal events (pure nodes fold into scope, R-PURE).
func evalSilent(s step, scope map[string]string) (string, error) {
	switch s.kind {
	case ir.NodeLit:
		v, err := evalValue(s.raw["value"], scope)
		if err != nil {
			return "", fmt.Errorf("lumen: lit %q value: %w", s.id, err)
		}
		return v, nil
	case ir.NodeInterp:
		v, err := evalInterp(s.raw, scope)
		if err != nil {
			return "", fmt.Errorf("lumen: interp %q: %w", s.id, err)
		}
		return v, nil
	default:
		return "", fmt.Errorf("lumen: evalSilent on non-pure kind %q", s.kind)
	}
}

// foldDoResult maps a host's DoResult (and any internal error) onto the node
// outcome, the effect.settled result, and the settled fields.
func foldDoResult(result enginehost.DoResult, runErr error) (nodeOutcome, effResult, detail, output, session string) {
	if runErr != nil {
		return OutcomeFailed, EffectResultInterrupted, "effect_interrupted: " + runErr.Error(), "", ""
	}
	switch result.Outcome {
	case enginehost.OutcomeFailed:
		return OutcomeFailed, EffectResultFailed, result.Detail, result.Output, result.SessionRef
	case enginehost.OutcomeDegraded:
		return OutcomeDegraded, EffectResultOK, result.Detail, result.Output, result.SessionRef
	case enginehost.OutcomePass:
		return OutcomePass, EffectResultOK, result.Detail, result.Output, result.SessionRef
	default:
		return OutcomeFailed, EffectResultFailed,
			fmt.Sprintf("host returned unknown outcome %q", result.Outcome),
			result.Output, result.SessionRef
	}
}

// outcomeForExit maps an exit code onto a step outcome, honoring the exec node's
// exitMap.pass set. With no pass set declared, only exit 0 passes.
func outcomeForExit(exitCode int, passCodes []int) string {
	if len(passCodes) == 0 {
		if exitCode == 0 {
			return OutcomePass
		}
		return OutcomeFailed
	}
	for _, c := range passCodes {
		if c == exitCode {
			return OutcomePass
		}
	}
	return OutcomeFailed
}

var interpRe = regexp.MustCompile(`\{\{\s*([a-zA-Z_][a-zA-Z0-9_]*)\s*\}\}`)

// interpolate substitutes {{name}} tokens in s with values from scope. An
// unknown name is left verbatim.
//
// SECURITY: interpolated values are spliced VERBATIM — not shell-quoted. Under
// the exec-host's `sh -c`, an untrusted value carrying shell metacharacters
// executes. Untrusted input is unsafe here (Lumen feedback 0020); argv-based
// execution is a later phase.
func interpolate(s string, scope map[string]string) string {
	return interpRe.ReplaceAllStringFunc(s, func(m string) string {
		name := interpRe.FindStringSubmatch(m)[1]
		if v, ok := scope[name]; ok {
			return v
		}
		return m
	})
}

// baseScope seeds the interpolation scope from the run input.
func baseScope(input map[string]any) map[string]string {
	scope := make(map[string]string, len(input))
	for k, v := range input {
		if s, ok := v.(string); ok {
			scope[k] = s
			continue
		}
		if b, err := json.Marshal(v); err == nil {
			scope[k] = string(b)
		}
	}
	return scope
}

// canonPayload marshals v to R-CANON bytes so payload_hash and chain_hash are
// reproducible.
func canonPayload(v any) ([]byte, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canon.Canonicalize(raw)
}

// inputHash is the provenance pin stamped on run.started for the run input: the
// SHA-256 of the canonicalized input map. It pins interpolation scope so Resume
// can refuse a different input (M2). An empty input is left unpinned (""), so a
// run that takes no input writes a run.started byte-identical to the pre-P4.3
// executor and Resume imposes no input constraint.
func inputHash(input map[string]any) string {
	if len(input) == 0 {
		return ""
	}
	raw, err := json.Marshal(input)
	if err != nil {
		return ""
	}
	canonical, err := canon.Canonicalize(raw)
	if err != nil {
		return ""
	}
	h := canon.Hash(canonical)
	return hex.EncodeToString(h[:])
}

// irHash is the provenance pin stamped on run.started: the SHA-256 of the
// canonicalized IR document. Resume (P4.3) refuses an IR whose hash differs.
func irHash(doc *ir.IR) string {
	raw, err := json.Marshal(doc)
	if err != nil {
		return ""
	}
	canonical, err := canon.Canonicalize(raw)
	if err != nil {
		return ""
	}
	h := canon.Hash(canonical)
	return hex.EncodeToString(h[:])
}

// streamIDFor derives a deterministic stream id from the formula name.
func streamIDFor(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "gcg-run-" + hex.EncodeToString(sum[:])[:12]
}

// streamIDForRun derives the run's stream id. Exec-only runs (no host) use the
// pure hash of the formula name; agent runs (host != nil) append a per-run
// nonce so repeated runs of one do-formula do not contend on a single stream.
func streamIDForRun(name string, withNonce bool) string {
	base := streamIDFor(name)
	if !withNonce {
		return base
	}
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return base
	}
	return base + "-" + hex.EncodeToString(b[:])
}
