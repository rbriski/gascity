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
	"os"
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

// leaseHolderBase names the executor class of the writer-lease holder.
const leaseHolderBase = "lumen-engine"

// leaseHolder is this process's INSTANCE-UNIQUE writer-lease holder (MEDIUM-1).
// AcquireWriterLease treats a same-holder acquire as a re-acquire that STEALS the
// lease regardless of expiry (lease.go), so a constant holder let two concurrent
// controllers both steal one stream's lease and each dispatch the same do — a
// double-execution hole. A per-process token makes a second concurrent driver a
// DIFFERENT holder, so it is fenced with ErrLeaseHeld instead of stealing, while a
// same-process re-Advance keeps this token and still re-acquires its OWN lease.
// Tests inject a distinct holder via Options.LeaseHolder to model two instances.
var leaseHolder = newLeaseHolder()

// newLeaseHolder mints this process's instance-unique writer-lease holder.
func newLeaseHolder() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%s:%d", leaseHolderBase, os.Getpid())
	}
	return fmt.Sprintf("%s:%d-%s", leaseHolderBase, os.Getpid(), hex.EncodeToString(b[:]))
}

// resolveLeaseHolder returns the writer-lease holder for a driver: the caller's
// explicit Options.LeaseHolder when set (tests modeling two concurrent instances),
// else this process's instance-unique holder.
func resolveLeaseHolder(opts Options) string {
	if opts.LeaseHolder != "" {
		return opts.LeaseHolder
	}
	return leaseHolder
}

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
	// PoolRouter is the pool seam consulted ONLY by Advance. When non-nil, every
	// `do` node lowers pool-mode: Advance dispatches its work as an ordinary bead in
	// the city work store (via DispatchWork), observes that bead for closure (via
	// ObserveWork), and PARKS instead of blocking on a Host. agentRef selects the
	// pool ("" = the run's default route); ok=false is a loud config error (a
	// pool-mode do with no resolvable route), never a silent inline fallback.
	// Run/RunWithOptions/Resume ignore this field and run every do inline through
	// Host. ZERO role names: a route is a config-resolved pool target, exactly like
	// gc.routed_to.
	PoolRouter func(agentRef string) (route string, ok bool)

	// DispatchWork is the real-bead do-path seam (REDESIGN §1.4): it creates
	// (idempotently) the ordinary fold_owned=0 work bead for one ready pool-mode do
	// activation in the city work store and returns its store-minted id. It is a
	// Layer-0 side effect injected by the composition root (the engine stays free of
	// internal/beads). It is REQUIRED whenever PoolRouter is set — a pool-mode do
	// with no DispatchWork seam is a loud configuration error (nowhere to create the
	// bead), and set without ObserveWork the dispatched bead would never settle.
	DispatchWork func(ctx context.Context, w WorkDispatch) (beadID string, err error)

	// ObserveWork reports an already-dispatched bead's terminal state (REDESIGN
	// §1.4) so Advance can copy an ordinary close into the journal as the existing
	// outcome.settled and advance the fold. Consulted for every pool node that
	// carries a recorded BeadID; REQUIRED alongside DispatchWork.
	ObserveWork func(ctx context.Context, beadID string) (WorkObservation, error)

	// LeaseHolder overrides the writer-lease holder for this driver (MEDIUM-1).
	// Production leaves it "" so the driver uses the process's instance-unique
	// holder (resolveLeaseHolder); tests set distinct holders to model two
	// concurrent controller instances contending one stream.
	LeaseHolder string
}

// WorkDispatch describes one ready pool-mode do activation the DispatchWork seam
// materializes as an ordinary work bead (REDESIGN §1.4/§2.2). Attempt is the
// activation's 0-based attempt index (fresh-bead-per-attempt visibility, §5): a
// retry/repeat re-attempt is a NEW activation, so the seam stamps a distinct
// (run, activation, attempt) triple and a prior failed attempt's bead survives.
type WorkDispatch struct {
	StreamID   string
	Activation string
	NodeID     string
	Route      string
	Prompt     string
	Attempt    int
}

// WorkObservation is a dispatched work bead's terminal state as ObserveWork reports
// it (REDESIGN §1.4/§2.4). Outcome is a pre-mapped Lumen outcome
// (pass/failed/degraded — the seam applies LumenOutcomeForGCOutcome, fail-closed),
// so the driver copies it verbatim into outcome.settled with no further judgment.
// Output is the do's result the downstream {{ref}} interpolation consumes (the seam
// reads it from the closed bead's gc.output_json, the dispatcher's step-output
// convention). Retryable is the fold retry arm's re-attempt gate for a failed
// outcome (MEDIUM-2): the seam sets it true ONLY for an explicit gc.outcome=fail —
// a bare/unknown close maps to failed too (fail-closed) but is NOT retryable, since
// a missing outcome is a definitive contract violation, not a transient strand, and
// re-running would re-execute possibly-complete work.
type WorkObservation struct {
	Terminal  bool
	Outcome   string
	Output    string
	Retryable bool
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

	units, err := buildUnits(doc, opts.Host != nil, opts.Host != nil)
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
		runEnvs:       runEnvIndex(units),
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

	// runEnvs indexes a run sub-formula's environment spec by its sub-namespace
	// (`<runID>/`), so scopeFor can build the per-namespace render view (env
	// bindings evaluated against the parent view + declared defaults). It is
	// derived purely from the lowered units (runEnvIndex), so it is identical on a
	// fresh run and a rebuild — keeping the sub-scope deterministic across resume.
	// The runtime-minted run-body namespaces are registered into it at drive time,
	// re-derived identically every pass: a repeat run body's per-attempt namespaces
	// (`<bodyNodeID>/<N>/`, registerRunBodyEnv), a run-bodied for-each's per-member
	// namespaces (`<fanID>/<index>/`, registerForEachRunMemberEnv), and a matched
	// dispatch run arm's namespace (`<armBodyID>/`, registerDispatchArmRunEnv).
	runEnvs map[string]*runSpec

	// parentNS overrides the scopeFor parent-namespace derivation for a namespace
	// whose STRING-derived parent is a phantom (⚑B1): a repeat run attempt namespace
	// `<bodyNodeID>/<N>/` has string-parent `<bodyNodeID>/`, and a for-each run
	// member namespace `<fanID>/<index>/` has string-parent `<fanID>/` — namespaces
	// with no env spec, whose view is {} — so every env binding would render ""
	// silently. The override points scopeFor at the loop's / the fan's real namespace
	// instead. registerRunBodyEnv, registerForEachRunMemberEnv, and
	// registerDispatchArmRunEnv populate it at drive time, UNCONDITIONALLY including the
	// empty string (⚑S1 — the phantom exists at EVERY depth, and "" is the load-bearing
	// override for a ROOT loop/fan; a dispatch is root-only, so its arm override is "");
	// a plain run's namespace is absent and derives its parent structurally exactly as
	// before (nil-map read is a miss).
	parentNS map[string]string
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

	// Crash boundary: after the unit's node.activated committed, before it acts or
	// settles — the activated-UNSETTLED window a real kill between the two journal
	// commits leaves behind (the only injectable point between a transparent
	// aggregate's two appends). Test-only; inert in production.
	if err := d.crashAt(crashAfterActivate, u.activation); err != nil {
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
	case unitRun:
		runErr = d.runRun(u, scope, nodeOutputs)
	case unitGuard:
		runErr = d.runGuard(u, scope, nodeOutputs)
	case unitDispatch:
		runErr = d.runDispatch(u, scope, nodeOutputs)
	case unitForEach:
		runErr = d.runForEach(u, scope, nodeOutputs)
	case unitCleanup:
		runErr = d.runCleanup(u, scope, nodeOutputs)
	case unitCleanupGuarded:
		// A cleanup's guarded-block drain aggregate settles transparently from its inlined
		// leaf members (which ran as ordinary units before it in topo order). It has no
		// advanceUnit arm — like unitScatterAgg/unitRun it defers via depsSettled and falls
		// through to this runUnit call.
		runErr = d.runCleanupGuarded(u, nodeOutputs)
	case unitRecover:
		runErr = d.runRecover(u, scope, nodeOutputs)
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
	// A unit inside a run sub-formula renders against its namespace view (env
	// bindings + sub outputs), not the flat root scope; a root unit's view IS the
	// flat scope, so run-free behavior is byte-identical.
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return err
	}
	switch u.leaf.kind {
	case ir.NodeExec:
		script := interpolate(u.leaf.script, view)
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
			v, err := evalValue(raw, view)
			if err != nil {
				return fmt.Errorf("lumen: settle %q value: %w", u.nodeID, err)
			}
			value = v
		}
		outcome := u.leaf.outcome
		if outcome == "" {
			outcome = OutcomePass
		}
		// A settle's authored `reason` is carried as the settle's detail so a recover
		// catch can bind {{ error.reason }} from a failed guarded (folded to
		// nodeState.Detail, v4). A reason-less settle keeps detail "" (byte-identical).
		// A malformed (non-string) reason is a loud load/render error, never a silent "".
		reason := ""
		if raw, ok := u.leaf.raw["reason"]; ok {
			if err := json.Unmarshal(raw, &reason); err != nil {
				return fmt.Errorf("lumen: settle %q reason: %w", u.nodeID, err)
			}
		}
		if err := d.appendSettled(u.activation, outcome, value, reason); err != nil {
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
	view, err := d.scopeFor(u.ns, scope)
	if err != nil {
		return err
	}
	prompt, err := renderPrompt(u.leaf.raw, view)
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
	outcome := scatterDrainOutcome(d.st(), u.members, u.onFail)
	if err := d.appendSettled(u.activation, outcome, "", ""); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// scatterDrainOutcome computes a drain aggregate's outcome from its settled member
// activations (M1): with on_fail "stop", any failed/canceled member fails the
// aggregate; otherwise the outcome reflects the degree of success — no pass with at
// least one blocking failure is `failed` (a total loss), a mix of pass and non-pass
// is `degraded`, all-pass is `pass`. It is the single source of the scatter(members)
// and for-each fan outcome rule, so the static and dynamic fans agree.
func scatterDrainOutcome(st *lumenState, memberActs []string, onFail string) string {
	anyPass, anyNonPass, anyBlocking := false, false, false
	for _, m := range memberActs {
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
	switch {
	case onFail == "stop" && anyBlocking:
		return OutcomeFailed
	case anyBlocking && !anyPass:
		return OutcomeFailed
	case anyNonPass:
		return OutcomeDegraded
	}
	return OutcomePass
}

// runForEach drives a for-each (scatter form:each) inline: it evaluates the `over`
// array and materializes one member per element with the binder bound into the render
// scope. A LEAF-bodied fan runs each synthesized leaf through runUnit (so every member
// gets its own journal fact node.activated/outcome.settled and resume memoization — NOT
// runLeaf, which would skip both); a RUN-bodied fan (spec.bodyRun != nil, the FBR arm)
// mints and drives each element's sub-formula sub-graph (runForEachRunMember). Either
// way the aggregate settles with the shared scatter drain rule over the member
// aggregates. An empty/absent array settles PASS (a vacuous fan — never skipped); a
// non-array `over` settles failed{invalid_input}. It is reached only when the for-each
// is not gated off — runUnit handles blocked()/skip-cascade before this switch arm.
func (d *driver) runForEach(u planUnit, scope, nodeOutputs map[string]string) error {
	spec := u.forEach
	elems, ok, err := d.evalForEachArray(u.ns, spec, scope)
	if err != nil {
		// The wrap contract: evalForEachArray's messages carry no "lumen:"/id prefix;
		// this (and advanceForEach, driver parity) names the failing fan exactly once.
		return fmt.Errorf("lumen: for-each %q over: %w", u.nodeID, err)
	}
	if !ok {
		if err := d.appendSettled(u.activation, OutcomeFailed, "", "invalid_input"); err != nil {
			return err
		}
		nodeOutputs[u.nodeID] = ""
		return nil
	}
	memberActs := make([]string, 0, len(elems))
	for i, elem := range elems {
		binderKey := u.ns + spec.binder
		// A run-bodied member mints the target sub-formula's whole sub-graph per element
		// (FBR); a leaf member fans a single synthesized leaf (FIS). Both bind the element
		// into the render scope for the duration of the member drive (withBinder, ⚑S2).
		if spec.bodyRun != nil {
			memberActs = append(memberActs, activationFor(forEachMemberNodeID(u.nodeID, i)))
			idx := i
			if err := d.withBinder(scope, binderKey, elem, func() error {
				return d.runForEachRunMember(u, idx, scope, nodeOutputs)
			}); err != nil {
				return err
			}
			continue
		}
		mu := d.forEachMemberUnit(u, i)
		memberActs = append(memberActs, mu.activation)
		if err := d.withBinder(scope, binderKey, elem, func() error {
			return d.runUnit(mu, scope, nodeOutputs)
		}); err != nil {
			return err
		}
	}
	outcome := scatterDrainOutcome(d.st(), memberActs, spec.onFail)
	if err := d.appendSettled(u.activation, outcome, "", ""); err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = ""
	return nil
}

// withBinder binds a for-each element to the (namespace-qualified) binder key in scope
// for the duration of fn, then restores the prior value (save/restore, like a repeat
// loop's iteration binding). The caller passes u.ns+spec.binder, so inside a run
// sub-formula the element lands at the direct-child key the member's scopeFor(ns) view
// re-keys to the bare binder name (shadowing a same-named sub-input/sub-node during the
// member render, restored afterward). At the root the key is the bare binder.
func (d *driver) withBinder(scope map[string]string, binder, elem string, fn func() error) error {
	restore, had := scope[binder]
	scope[binder] = elem
	err := fn()
	if had {
		scope[binder] = restore
	} else {
		delete(scope, binder)
	}
	return err
}

// forEachMemberUnit synthesizes the per-index member leaf for a for-each: a fresh
// activation under a `<forEachID>/<index>` node id, parented under the aggregate so a
// failed member drains into the aggregate and stays OUT of the run's top-level
// outcome (the same parenting rule as a loop attemptUnit). The binder is bound around
// the run/materialize call by the caller.
func (d *driver) forEachMemberUnit(u planUnit, index int) planUnit {
	spec := u.forEach
	memberID := forEachMemberNodeID(u.nodeID, index)
	idx := index
	return planUnit{
		kind:        unitLeaf,
		activation:  activationFor(memberID),
		nodeID:      memberID,
		irKind:      spec.bodyIRKind,
		parent:      u.activation,
		memberIndex: &idx,
		ns:          u.ns,
		afterDeps:   u.afterDeps,
		rawAfter:    u.rawAfter,
		leaf:        spec.body,
	}
}

// forEachMemberNodeID is the node id of a for-each's Nth member: forEachID/N. The '/'
// makes it distinct from any authored id (which the lowerer bans '/' in) and from a
// loop attempt (bodyID:N), and activation parsing splits on the last ':' so the '/'
// is preserved in the node id.
func forEachMemberNodeID(forEachID string, index int) string {
	return forEachID + "/" + strconv.Itoa(index)
}

// forEachRunMember mints for-each member `index`'s run sub-graph via the shared
// mintRunBody helper (the RBL leg's twin): prefix `<fanID>/<index>/`, aggregate
// `<fanID>/<index>:0` parented under the FAN aggregate, MemberIndex stamped `index`
// (leaf-member projection parity, Q-C), gates = the fan unit's afterDeps (the ⚑B2
// env-ref gate + any `after`). It is a pure function of (spec stash, index, fan
// coordinates), so genesis, re-Advance, and resume mint byte-identically.
func (d *driver) forEachRunMember(u planUnit, index int) ([]planUnit, planUnit, error) {
	memberID := forEachMemberNodeID(u.nodeID, index)
	idx := index
	return mintRunBody(u.forEach.runBodyStash, u.forEach.bodyRun, memberID, memberID+"/", activationFor(memberID),
		u.activation, u.ns, u.afterDeps, u.rawAfter, &idx)
}

// registerForEachRunMemberEnv wires for-each run-member `index`'s env seam into scopeFor
// (⚑B1), analogous to registerRunBodyEnv: the member namespace `<fanID>/<index>/` → the
// body run's env spec, PLUS an explicit parent-namespace override = the FAN's namespace
// (u.ns), registered UNCONDITIONALLY including u.ns == "" (⚑S1 — the structural parent
// `<fanID>/` is a phantom at EVERY depth, precluded from being a real run namespace; an
// `if u.ns != ""` guard would silently collapse the ROOT corpus shape to all-"" bindings).
// Nested runs inside the member sub-graph register too. It is pure — re-derived identically
// every pass and resume (genesis ≡ resume) — so re-registering is an idempotent map write.
func (d *driver) registerForEachRunMemberEnv(u planUnit, memberID string, subUnits []planUnit) {
	if d.runEnvs == nil {
		d.runEnvs = map[string]*runSpec{}
	}
	if d.parentNS == nil {
		d.parentNS = map[string]string{}
	}
	memberNS := memberID + "/"
	d.runEnvs[memberNS] = u.forEach.bodyRun
	d.parentNS[memberNS] = u.ns
	for i := range subUnits {
		if subUnits[i].kind == unitRun && subUnits[i].run != nil {
			d.runEnvs[subUnits[i].nodeID+"/"] = subUnits[i].run
		}
	}
}

// runForEachRunMember mints for-each member `index`'s run sub-graph, registers its env
// seam, and drives every minted sub-unit then the transparent member aggregate through
// runUnit — the aggregate LAST (⚑S1 Tier-A FK ordering: its Members edges reference the
// sub-node rows). It re-mints STATELESSLY every pass (no fold-state early-out — the member
// aggregate `<fanID>/<index>:0` is nil mid-mint since it activates last, so an early-out
// would misroute the agg-activated-unsettled crash window AND skip re-registration);
// resume memoization inside runUnit reloads any already-settled sub-unit. NO attempt.minted
// (Q-C STATELESS events). The binder is bound by the caller's withBinder window (⚑S2).
func (d *driver) runForEachRunMember(u planUnit, index int, scope, nodeOutputs map[string]string) error {
	subUnits, agg, err := d.forEachRunMember(u, index)
	if err != nil {
		return err
	}
	d.registerForEachRunMemberEnv(u, agg.nodeID, subUnits)
	for i := range subUnits {
		if err := d.runUnit(subUnits[i], scope, nodeOutputs); err != nil {
			return err
		}
	}
	if err := d.runUnit(agg, scope, nodeOutputs); err != nil {
		return err
	}
	// An engine-inline member always settles its aggregate in-pass; if it did not, the
	// body lowering or host is broken — surface it loudly (the runLoop invariant) rather
	// than let scatterDrainOutcome silently skip an unsettled memberAct.
	if n := d.st().Nodes[agg.activation]; n == nil || !n.Settled {
		return fmt.Errorf("lumen: for-each %q run member %d aggregate did not settle in-pass", u.nodeID, index)
	}
	return nil
}

// evalForEachArray evaluates a for-each `over` expression to its element strings for the
// unit's namespace ns. At the root (ns == "") a bare `ref X` reads the flat scope (a node
// output / flattened input holding a JSON array, DET-gated on node X by resolveDeps) and
// an `input.<field>` member reads the field from the IMMUTABLE input map — never the flat
// scope, where a node output could shadow the field between passes. Inside a run
// sub-formula (ns != "") the ref arm reads the namespace VIEW (scopeFor: a settled
// sub-sibling shadows a same-named binding, root child-wins parity) and the member arm
// reads the run INPUT LAYER alone (runInputLayer: the env binding / declared default,
// never the children — root's "never the flat scope" rationale). A miss (absent/empty)
// is (nil, true) — the vacuous PASS (root parity); ok=false marks a PRESENT non-array
// `over` (the caller settles failed{invalid_input}). An unregistered ns is a structural
// bug (register-before-drive holds on every real path), refused loudly BEFORE the arm
// switch so the ref arm never silently builds a children-only view.
func (d *driver) evalForEachArray(ns string, spec *forEachSpec, scope map[string]string) ([]string, bool, error) {
	var head struct {
		Kind string          `json:"kind"`
		Name string          `json:"name"`
		Base json.RawMessage `json:"base"`
	}
	if err := json.Unmarshal(spec.overRaw, &head); err != nil {
		return nil, false, fmt.Errorf("malformed over expression: %w", err)
	}
	// No "lumen:" / for-each-id prefix on this message — BOTH call sites (runForEach,
	// advanceForEach) wrap every error from here as `lumen: for-each %q over: %w` (the
	// GIS condScope-wrap parity), so the surfaced error names the failing fan exactly
	// once.
	if ns != "" && d.runEnvs[ns] == nil {
		return nil, false, fmt.Errorf("namespace %q has no registered environment", ns)
	}
	switch head.Kind {
	case "ref":
		if ns == "" {
			return decodeArrayString(scope[head.Name])
		}
		view, err := d.scopeFor(ns, scope)
		if err != nil {
			return nil, false, err
		}
		return decodeArrayString(view[head.Name])
	case "member":
		if ns == "" {
			return arrayFromInputValue(d.input[head.Name])
		}
		layer, err := d.runInputLayer(ns, scope)
		if err != nil {
			return nil, false, err
		}
		return decodeArrayString(layer[head.Name])
	default:
		return nil, false, fmt.Errorf("%w: over kind %q", ErrUnsupportedNode, head.Kind)
	}
}

// decodeArrayString decodes a JSON-array scope string (`["a","b"]`) to its element
// strings. Empty/whitespace → (nil, true) (an empty fan is a vacuous PASS). A value
// that is not a JSON array → (nil, false) (invalid_input). Each element is stringified
// exactly as baseScope seeds an input value (string as-is, else canonical JSON).
func decodeArrayString(s string) ([]string, bool, error) {
	if strings.TrimSpace(s) == "" {
		return nil, true, nil
	}
	var arr []json.RawMessage
	if err := json.Unmarshal([]byte(s), &arr); err != nil {
		return nil, false, nil
	}
	out := make([]string, len(arr))
	for i, e := range arr {
		out[i] = canonScalar(e)
	}
	return out, true, nil
}

// arrayFromInputValue converts a raw input value (from the immutable input map) to
// element strings: a []any array or a JSON-array string decodes (via the same
// canonicalization decodeArrayString applies); nil/absent → empty (PASS); any other
// type → invalid_input.
func arrayFromInputValue(v any) ([]string, bool, error) {
	switch t := v.(type) {
	case nil:
		return nil, true, nil
	case string:
		return decodeArrayString(t)
	case []any:
		raw, err := json.Marshal(t)
		if err != nil {
			return nil, false, nil
		}
		return decodeArrayString(string(raw))
	default:
		return nil, false, nil
	}
}

// cleanupOutcome computes a cleanup's settled outcome from its guarded and body
// (finally) outcomes: the finally NEVER swallows a result — it changes the outcome
// only if it ITSELF fails, in which case its failure supersedes. So a failed/canceled
// body wins; otherwise the outcome is transparently the guarded's (pass/degraded/failed).
func cleanupOutcome(guarded, body string) string {
	if body == OutcomeFailed || body == OutcomeCanceled {
		return body
	}
	return guarded
}

// runCleanup drives a cleanup (try/finally) inline. Single-leaf form: it runs the guarded
// leaf, then ALWAYS runs the finally leaf, then settles. Block form (guardedAgg != ""):
// PHASE-1 is a NO-OP — the guarded block's leaves and their drain aggregate already ran as
// ordinary units before this cleanup (the aggregate is a memberDep, so topo order
// guarantees it settled) — so runCleanup only runs the finally and settles from the
// aggregate. In both forms the finally's only blocking dep is the cleanup's own gate (not
// the guarded), so a guarded/block failure never skip-cascades the teardown: blocked()
// reads only afterDeps, and the guarded aggregate is a memberDep.
//
// The finally is suppressed on exactly two paths — both intercepted BEFORE this function,
// in runUnit's pre-switch (inline) and mirrored by the advanceUnit routing guard (pool),
// and both meaning nothing ran so there is nothing to tear down:
//   - the cleanup's own gate failed (blocked() — skip-cascade, "upstream dependency"
//     detail), or
//   - the whole guarded block did-not-run: every drain member settled skipped/canceled
//     (a member's outside `after` gating it off, or an authored settle-canceled member),
//     where aggregateAllSkipped() settles the cleanup skipped ("every drain member
//     skipped" detail).
//
// Any FAILED member means the block RAN (didNotRun(failed)=false), the aggregate settles
// failed — not skipped — and the finally always runs. A single-leaf cleanup has no
// memberDeps, so only the gate path applies to it (its finally runs even for a canceled
// guarded — the leaf is transparent, not a drain).
func (d *driver) runCleanup(u planUnit, scope, nodeOutputs map[string]string) error {
	var gu planUnit
	if u.cleanup.guardedAgg == "" {
		gu = d.cleanupGuardedUnit(u)
		if err := d.runUnit(gu, scope, nodeOutputs); err != nil {
			return err
		}
	}
	bu := d.cleanupBodyUnit(u)
	if err := d.runUnit(bu, scope, nodeOutputs); err != nil {
		return err
	}
	return d.settleCleanup(u, gu, bu, scope, nodeOutputs)
}

// cleanupGuardedUnit / cleanupBodyUnit synthesize the guarded and finally sub-units,
// each parented under the cleanup (so their outcomes stay out of the run's top-level
// aggregation — only the cleanup settles into it).
func (d *driver) cleanupGuardedUnit(u planUnit) planUnit {
	s := u.cleanup
	return d.decisionBodyUnit(u, s.guardedNodeID, s.guardedIRKind, s.guarded)
}

func (d *driver) cleanupBodyUnit(u planUnit) planUnit {
	s := u.cleanup
	return d.decisionBodyUnit(u, s.bodyNodeID, s.bodyIRKind, s.body)
}

// settleCleanup settles the cleanup from its (settled) guarded + body outcomes via the
// finally-failure-wins rule, with the guarded's output as the result. The scope seed is
// gated on ranOutcome so a canceled/skipped cleanup seeds nothing — matching resume's
// reconstructOutputs (which skips a non-ran node), keeping a downstream {{cleanupID}}
// identical across genesis and resume.
func (d *driver) settleCleanup(u, gu, bu planUnit, scope, nodeOutputs map[string]string) error {
	st := d.st()
	// Block form reads the guarded outcome/output from the drain AGGREGATE (its last-ran
	// block member's output); single-leaf form reads the synthesized guarded leaf (gu).
	guardedAct := gu.activation
	if u.cleanup.guardedAgg != "" {
		guardedAct = u.cleanup.guardedAgg
	}
	guardedOutcome, guardedOutput := OutcomeSkipped, ""
	if n := st.Nodes[guardedAct]; n != nil && n.Settled {
		guardedOutcome, guardedOutput = n.Outcome, n.Output
	}
	bodyOutcome := OutcomeSkipped
	if n := st.Nodes[bu.activation]; n != nil && n.Settled {
		bodyOutcome = n.Outcome
	}
	outcome := cleanupOutcome(guardedOutcome, bodyOutcome)
	if err := d.appendSettled(u.activation, outcome, guardedOutput, ""); err != nil {
		return err
	}
	if ranOutcome(outcome) {
		d.record(u.nodeID, guardedOutput, scope, nodeOutputs)
	}
	return nil
}

// recoverCaught reports whether a guarded outcome triggers the catch. A FAILED guarded
// is caught (recovered); a passing/degraded guarded is not, and a CANCELED guarded is
// NOT caught either — cancellation is not a recoverable failure, and catching it would
// let a passing catch flip the run to pass despite the cancel.
func recoverCaught(guardedOutcome string) bool {
	return guardedOutcome == OutcomeFailed
}

// runRecover drives a recover (try/catch) inline: it runs the guarded; if the guarded
// did NOT fail, the recover settles transparently from it (the catch never runs); if it
// FAILED, the recover binds the error into the catch scope, runs the catch, and settles
// from it (a handled error, or a re-failed catch). Both subs run through runUnit. A
// gated-off recover is intercepted by runUnit's pre-switch skip-cascade.
func (d *driver) runRecover(u planUnit, scope, nodeOutputs map[string]string) error {
	gu := d.recoverGuardedUnit(u)
	if err := d.runUnit(gu, scope, nodeOutputs); err != nil {
		return err
	}
	if !recoverCaught(d.settledOutcome(gu.activation)) {
		return d.settleRecoverFrom(u, gu, scope, nodeOutputs)
	}
	bu := d.recoverBodyUnit(u)
	if err := d.withErrorBindings(scope, d.errorBindings(u), func() error {
		return d.runUnit(bu, scope, nodeOutputs)
	}); err != nil {
		return err
	}
	return d.settleRecoverFrom(u, bu, scope, nodeOutputs)
}

// recoverGuardedUnit / recoverBodyUnit synthesize the guarded and catch sub-units, each
// parented under the recover (so a caught guarded failure stays out of the run outcome).
func (d *driver) recoverGuardedUnit(u planUnit) planUnit {
	s := u.recover
	return d.decisionBodyUnit(u, s.guardedNodeID, s.guardedIRKind, s.guarded)
}

func (d *driver) recoverBodyUnit(u planUnit) planUnit {
	s := u.recover
	return d.decisionBodyUnit(u, s.bodyNodeID, s.bodyIRKind, s.body)
}

// settledOutcome returns an activation's settled outcome from the fold ("" if absent/unsettled).
func (d *driver) settledOutcome(activation string) string {
	if n := d.st().Nodes[activation]; n != nil && n.Settled {
		return n.Outcome
	}
	return ""
}

// errorBindings returns the flat error scope keys for a caught guarded: <binding>.reason
// = the guarded's failure Detail, <binding>.step = the guarded node id, <binding>.message
// = the Detail. It reads the guarded's settled fold node, so it is a pure function of the
// fold (stable across passes and drop+refold).
//
// Detail is populated for a SETTLE guarded (its authored reason). It is EMPTY for a
// failed exec guarded (the exit code carries no structured reason) and for a pool-do
// guarded (the WorkObservation carries no reason field) — for those, error.reason is ""
// and error.step still names the failing node. Threading exec exit codes / a do bead's
// close detail into the fold is a follow-up (it would broaden the Detail fold to every
// failed node); this slice binds the reason for the try/catch-settle shape (the golden).
func (d *driver) errorBindings(u planUnit) map[string]string {
	s := u.recover
	detail := ""
	if n := d.st().Nodes[activationFor(s.guardedNodeID)]; n != nil {
		detail = n.Detail
	}
	return map[string]string{
		s.errorBinding + ".reason":  detail,
		s.errorBinding + ".step":    s.guardedNodeID,
		s.errorBinding + ".message": detail,
	}
}

// withErrorBindings binds the error keys into scope for the duration of fn, then restores
// each (save/restore, so a same-named pre-existing scope entry survives). The catch body's
// prompt renders {{ error.reason }} against these; the keys are order-independent.
func (d *driver) withErrorBindings(scope, bindings map[string]string, fn func() error) error {
	type saved struct {
		v   string
		had bool
	}
	prev := make(map[string]saved, len(bindings))
	for k, v := range bindings {
		old, had := scope[k]
		prev[k] = saved{old, had}
		scope[k] = v
	}
	err := fn()
	for k, s := range prev {
		if s.had {
			scope[k] = s.v
		} else {
			delete(scope, k)
		}
	}
	return err
}

// settleRecoverFrom settles the recover transparently from a sub (the guarded when not
// caught, the catch body when caught): the sub's outcome + output. The scope seed is
// gated on ranOutcome (a canceled/skipped recover seeds nothing — resume parity).
func (d *driver) settleRecoverFrom(u, su planUnit, scope, nodeOutputs map[string]string) error {
	outcome, output := OutcomeSkipped, ""
	if n := d.st().Nodes[su.activation]; n != nil && n.Settled {
		outcome, output = n.Outcome, n.Output
	}
	if err := d.appendSettled(u.activation, outcome, output, ""); err != nil {
		return err
	}
	if ranOutcome(outcome) {
		d.record(u.nodeID, output, scope, nodeOutputs)
	}
	return nil
}

// runRun settles a run's transparent aggregate from its (already-settled) members
// and records the sub-formula's result into the PARENT scope for downstream value
// plumbing. It is reached only after every member settled (the run's memberDeps
// gate the aggregate in topo order); a gated-off run was intercepted earlier by
// blocked()/aggregateAllSkipped() and settled skipped without running any member.
//
// The outcome is "transparent" — exactly what the sub-formula would seal with as a
// standalone run (transparentOutcome mirrors runOutcome: failed|canceled dominates,
// then degraded, else pass; a skipped member is IGNORED, unlike a scatter's degraded
// mapping). The output is the LAST source-order member that actually RAN (the
// sub-formula's final executed statement — the "returns lastResult" convention),
// recorded under the run's (parent-namespace) node id so a downstream `{{runRef}}`
// in the parent resolves.
func (d *driver) runRun(u planUnit, scope, nodeOutputs map[string]string) error {
	output, err := d.settleTransparentAgg(u)
	if err != nil {
		return err
	}
	d.record(u.nodeID, output, scope, nodeOutputs)
	return nil
}

// settleTransparentAgg settles a transparent drain aggregate — a run's sub-graph OR a
// cleanup guarded block — from its already-settled members, and appends the settle. The
// outcome is the worst-of transparent aggregation (transparentOutcome: failed|canceled
// dominates → failed, then degraded, else pass; a skipped member is IGNORED) and the
// output is the LAST source-order member that actually RAN (the "returns lastResult"
// convention; last-wins for a linear chain). It records NOTHING into scope/nodeOutputs —
// the two callers diverge there (⚑SHOULD-FIX-2): runRun d.records the run's downstream
// value into the parent scope (a run's transparent output is {{runRef}}-consumable),
// while the cleanup guarded aggregate seeds nodeOutputs ONLY, never the scope (the
// scatter/gather aggregate convention). Genesis, resume, and drop+refold agree because
// reconstructOutputs treats both the run and block aggregate kinds identically.
func (d *driver) settleTransparentAgg(u planUnit) (output string, err error) {
	st := d.st()
	outcome := transparentOutcome(st, u.members)
	for _, m := range u.members {
		if n := st.Nodes[m]; n != nil && n.Settled && ranOutcome(n.Outcome) {
			output = n.Output
		}
	}
	if err := d.appendSettled(u.activation, outcome, output, ""); err != nil {
		return "", err
	}
	return output, nil
}

// runCleanupGuarded settles a cleanup's guarded-block drain aggregate. It shares runRun's
// transparent outcome/last-ran-output logic (settleTransparentAgg) but seeds nodeOutputs
// ONLY — never the interpolation scope (⚑SHOULD-FIX-2, the scatter/gather aggregate
// convention). The synthetic aggregate node id (<cleanupID>/__guarded) is unreachable
// from any {{ref}} (idents cannot contain '/'); the finally reads block-member outputs at
// their BARE ids in the parent scope, not through this aggregate. reconstructOutputs
// treats ir.NodeBlock as an aggregate kind, so genesis == resume == refold here.
func (d *driver) runCleanupGuarded(u planUnit, nodeOutputs map[string]string) error {
	output, err := d.settleTransparentAgg(u)
	if err != nil {
		return err
	}
	nodeOutputs[u.nodeID] = output
	return nil
}

// runGuard drives a guard's decision arm inline (Run, and Advance for an exec then):
// it evaluates the closed condition over the folded scope; a FALSE condition settles
// the guard `pass` with no side effect (a conditional step that legitimately did not
// run — it does NOT skip-cascade dependents); a TRUE condition runs the synthesized
// `then` leaf and settles the guard transparently from it, recording its output for a
// downstream {{guardID}}. The condition is a pure function of the fold, so a resume
// re-evaluates it identically.
func (d *driver) runGuard(u planUnit, scope, nodeOutputs map[string]string) error {
	spec := u.guard
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
	tu := d.guardThenUnit(u)
	if err := d.runUnit(tu, scope, nodeOutputs); err != nil {
		return err
	}
	return d.settleDecisionFromBody(u, tu, scope, nodeOutputs)
}

// runDispatch drives a dispatch (multi-way branch) inline: it evaluates the subject
// to a string and runs the FIRST arm whose match equals it, settling the dispatch
// transparently from that arm's body — a leaf arm runs one synthesized leaf, a RUN arm
// mints its whole sub-graph (runDispatchRunArm); no matching arm settles the dispatch PASS
// with an empty result (a no-op that does not skip-cascade). The subject is a pure function
// of the fold, so a resume re-selects the same arm.
func (d *driver) runDispatch(u planUnit, scope, nodeOutputs map[string]string) error {
	arm, ok, err := d.matchingArm(u, scope)
	if err != nil {
		return fmt.Errorf("lumen: dispatch %q subject: %w", u.nodeID, err)
	}
	if !ok {
		return d.settleDecisionSkipped(u, scope, nodeOutputs)
	}
	// ⚑B2: kind-route on the MATCHED arm's bodyRun BEFORE the leaf machinery — a run arm
	// mints its whole sub-graph (runDispatchRunArm), a leaf arm runs one synthesized leaf.
	if arm.bodyRun != nil {
		return d.runDispatchRunArm(u, arm, scope, nodeOutputs)
	}
	au := d.dispatchArmUnit(u, arm)
	if err := d.runUnit(au, scope, nodeOutputs); err != nil {
		return err
	}
	return d.settleDecisionFromBody(u, au, scope, nodeOutputs)
}

// runDispatchRunArm drives a MATCHED run arm inline (Run, and Advance for an exec/settle
// sub-graph): it mints the arm's sub-graph, registers its env seam, drives every minted
// sub-unit then the arm aggregate LAST (⚑S1 Tier-A FK ordering: its Members edges reference
// the sub-node rows) through runUnit, asserts the aggregate settled in-pass (the loud
// FBR-mirror invariant), then settles the dispatch transparently from it. It re-mints
// STATELESSLY (resume memoization inside runUnit reloads any already-settled sub-unit); NO
// attempt.minted / node.decision (Q-C stateless).
func (d *driver) runDispatchRunArm(u planUnit, arm *dispatchArm, scope, nodeOutputs map[string]string) error {
	subUnits, agg, err := d.dispatchArmRunBody(u, arm)
	if err != nil {
		return err
	}
	d.registerDispatchArmRunEnv(u, arm, subUnits)
	for i := range subUnits {
		if err := d.runUnit(subUnits[i], scope, nodeOutputs); err != nil {
			return err
		}
	}
	if err := d.runUnit(agg, scope, nodeOutputs); err != nil {
		return err
	}
	// An engine-inline arm always settles its aggregate in-pass; if it did not, the body
	// lowering or host is broken — surface it loudly (the runLoop/FBR invariant) rather than
	// let settleDecisionFromBody silently default the dispatch PASS/"" off the unsettled agg.
	if n := d.st().Nodes[agg.activation]; n == nil || !n.Settled {
		return fmt.Errorf("lumen: dispatch %q run arm %q aggregate did not settle in-pass", u.nodeID, arm.matchValue)
	}
	return d.settleDecisionFromBody(u, agg, scope, nodeOutputs)
}

// dispatchArmRunBody mints a MATCHED run arm's sub-graph via the shared mintRunBody helper:
// prefix `<armBodyID>/`, aggregate `<armBodyID>:0` parented under the DISPATCH activation
// (so the sub-graph stays OUT of any enclosing runOutcome and the dispatch settles
// transparently from it), NO member index (an arm is not a fan member), gates = the
// dispatch's afterDeps (the ⚑B2 subject-ref + arm-env gate + any `after`). It is a pure
// function of (arm stash, arm coordinates), so genesis, re-Advance, and resume mint
// byte-identically.
func (d *driver) dispatchArmRunBody(u planUnit, arm *dispatchArm) ([]planUnit, planUnit, error) {
	return mintRunBody(arm.runBodyStash, arm.bodyRun, arm.bodyNodeID, arm.bodyNodeID+"/", activationFor(arm.bodyNodeID),
		u.activation, u.ns, u.afterDeps, u.rawAfter, nil)
}

// registerDispatchArmRunEnv wires a matched run arm's env seam into scopeFor (⚑B1),
// analogous to registerForEachRunMemberEnv: the arm namespace `<armBodyID>/` → the arm run's
// env spec, PLUS an explicit parent-namespace override = the dispatch's namespace (u.ns),
// registered UNCONDITIONALLY including u.ns == "" (⚑S1 parity — a dispatch is root-only, so
// u.ns is always "" and the structural parent of `<armBodyID>/` is already root, but the
// unconditional write keeps the FBR/RBL registration shape). Nested runs inside the arm
// sub-graph register too. It is pure — re-derived identically every pass and resume — so
// re-registering is an idempotent map write.
func (d *driver) registerDispatchArmRunEnv(u planUnit, arm *dispatchArm, subUnits []planUnit) {
	if d.runEnvs == nil {
		d.runEnvs = map[string]*runSpec{}
	}
	if d.parentNS == nil {
		d.parentNS = map[string]string{}
	}
	armNS := arm.bodyNodeID + "/"
	d.runEnvs[armNS] = arm.bodyRun
	d.parentNS[armNS] = u.ns
	for i := range subUnits {
		if subUnits[i].kind == unitRun && subUnits[i].run != nil {
			d.runEnvs[subUnits[i].nodeID+"/"] = subUnits[i].run
		}
	}
}

// matchingArm evaluates a dispatch's subject against the scope and returns the first
// arm whose match value equals it.
func (d *driver) matchingArm(u planUnit, scope map[string]string) (*dispatchArm, bool, error) {
	subjectVal, err := evalValue(u.dispatch.subject, scope)
	if err != nil {
		return nil, false, err
	}
	for i := range u.dispatch.arms {
		if u.dispatch.arms[i].matchValue == subjectVal {
			return &u.dispatch.arms[i], true, nil
		}
	}
	return nil, false, nil
}

// chosenArm returns the arm whose body has already been activated — the durable record of
// which branch a prior Advance pass selected. It is no longer the whole write-once truth: for
// a LEAF arm the body activation IS the leaf, so chosenArm fires as soon as the arm dispatches;
// but for a RUN arm the body is the arm AGGREGATE, which activates LAST (after its sub-graph),
// so chosenArm returns not-chosen while the arm is mid-mint (its sub-dos in flight). It may
// therefore only SKIP re-matchingArm — the caller MUST still kind-route to the re-mint/drive
// (advanceDispatchRunArm), never fast-settle a returned run arm (⚑B2).
func (d *driver) chosenArm(u planUnit) (*dispatchArm, bool) {
	for i := range u.dispatch.arms {
		if d.st().Nodes[activationFor(u.dispatch.arms[i].bodyNodeID)] != nil {
			return &u.dispatch.arms[i], true
		}
	}
	return nil, false
}

// settleDecisionSkipped settles a decision arm (guard/dispatch) that took no branch:
// PASS with an empty result (no side effect, no skip-cascade). It records the empty
// output so a downstream {{id}} resolves to "" and genesis matches a resume.
func (d *driver) settleDecisionSkipped(u planUnit, scope, nodeOutputs map[string]string) error {
	if err := d.appendSettled(u.activation, OutcomePass, "", "no branch taken"); err != nil {
		return err
	}
	d.record(u.nodeID, "", scope, nodeOutputs)
	return nil
}

// settleDecisionFromBody settles a decision arm transparently from its chosen body (a leaf,
// a guard/dispatch arm, or a run arm's aggregate) and records the body's output for a
// downstream {{id}}. ⚑B2 PRECONDITION: it silently DEFAULTS PASS/"" when the body node is
// nil-or-unsettled — so the CALLER must not call it for a run arm until the aggregate is
// Settled (runDispatchRunArm asserts the loud in-pass invariant; advanceDispatchRunArm parks
// while unsettled). A leaf/guard arm body always settles before this in the same pass.
func (d *driver) settleDecisionFromBody(u, bu planUnit, scope, nodeOutputs map[string]string) error {
	outcome, output := OutcomePass, ""
	if bn := d.st().Nodes[bu.activation]; bn != nil && bn.Settled {
		outcome, output = bn.Outcome, bn.Output
	}
	if err := d.appendSettled(u.activation, outcome, output, ""); err != nil {
		return err
	}
	d.record(u.nodeID, output, scope, nodeOutputs)
	return nil
}

// decisionBodyUnit synthesizes the leaf unit for a decision arm's body (a guard
// `then` or a dispatch arm body): activation bodyID:0, parented under the decision
// node, inheriting its `after` gates (already cleared) and namespace.
func (d *driver) decisionBodyUnit(u planUnit, bodyNodeID string, bodyIRKind ir.NodeKind, body step) planUnit {
	return planUnit{
		kind:       unitLeaf,
		activation: activationFor(bodyNodeID),
		nodeID:     bodyNodeID,
		irKind:     bodyIRKind,
		parent:     u.activation,
		ns:         u.ns,
		afterDeps:  u.afterDeps,
		rawAfter:   u.rawAfter,
		leaf:       body,
	}
}

// guardThenUnit synthesizes the guard's `then` leaf unit.
func (d *driver) guardThenUnit(u planUnit) planUnit {
	s := u.guard
	return d.decisionBodyUnit(u, s.thenNodeID, s.thenIRKind, s.then)
}

// dispatchArmUnit synthesizes a dispatch arm's body leaf unit.
func (d *driver) dispatchArmUnit(u planUnit, arm *dispatchArm) planUnit {
	return d.decisionBodyUnit(u, arm.bodyNodeID, arm.bodyIRKind, arm.body)
}

// subScopeFold is the shared namespace-local fold assembly both the guard cond scope
// (child-wins, GIS ⚑S4) and the loop cond/attempts scope (typed input-first, ⚑B1)
// build from a run sub-formula's registered env + the fold — the Q-C shared-builder
// product. The two callers layer the sub-input differently over these pieces.
type subScopeFold struct {
	spec      *runSpec          // the namespace's registered env (bindings + declared input schema)
	childView map[string]string // the fold's DIRECT-child outputs at their bare ids (⚑B1, incl. aggregates)
	outcomes  map[string]string // bare-keyed direct-child outcomes (Settled && ranOutcome, highest attempt wins)
	childKeys map[string]bool   // the direct-child bare-id shadow set (scope + nodeOutputs children)
}

// subScope assembles the shared namespace-local fold pieces for ns (§1.2): the
// registered env spec, the direct-child outputs from the FLAT nodeOutputs (which —
// unlike scope — carries scatter/gather/for-each/cleanup aggregate outputs, the
// nodeOutputs-only convention), the settled direct-child outcomes at bare ids, and the
// child-id shadow set. It reads no clock and no randomness (a fold walk only), so
// genesis, re-Advance, and crash-resume build identical pieces (DET). unitKind names
// the caller for the ⚑S5 unregistered-namespace refusal (which loops must not report as
// a guard); call sites wrap it with `lumen: <kind> %q cond: %w` / `… attempts: %w`.
// The inner noun stays "cond scope" even under the attempts wrap — accepted cosmetic:
// the unit KIND (what triage keys on) is parametrized, and cond-vs-attempts is visible
// from the call-site wrap.
func (d *driver) subScope(ns, unitKind string, scope, nodeOutputs map[string]string) (subScopeFold, error) {
	// ⚑S5: register-before-drive holds on every drive path today, so an unregistered
	// namespace here is a structural bug — the view would collapse and freeze a wrong
	// write-once decision. Refuse loudly rather than fold silently. (No "lumen:" prefix.)
	spec := d.runEnvs[ns]
	if spec == nil {
		return subScopeFold{}, fmt.Errorf("%s cond scope: namespace %q has no registered environment", unitKind, ns)
	}
	childView := map[string]string{}
	childKeys := map[string]bool{}
	// Defensive redundancy: every ns-child scope key is ALSO a nodeOutputs key today
	// (record() and the silent arm write both), so the scope pass adds nothing the
	// nodeOutputs overlay misses — it only keeps the shadow set honest if that ever bends.
	for k := range scope {
		if rest, ok := directChildKey(k, ns); ok {
			childKeys[rest] = true
		}
	}
	for k, v := range nodeOutputs {
		if rest, ok := directChildKey(k, ns); ok {
			childView[rest] = v
			childKeys[rest] = true
		}
	}
	outcomes := map[string]string{}
	best := map[string]int{}
	for act, n := range d.st().Nodes {
		if !n.Settled || !ranOutcome(n.Outcome) {
			continue
		}
		rest, ok := directChildKey(activationNodeID(act), ns)
		if !ok {
			continue
		}
		att := activationAttempt(act)
		if prev, ok := best[rest]; ok && att <= prev {
			continue
		}
		best[rest] = att
		outcomes[rest] = n.Outcome
	}
	return subScopeFold{spec: spec, childView: childView, outcomes: outcomes, childKeys: childKeys}, nil
}

// condScope builds the closed-expression scope a guard cond evaluates against for a
// unit in namespace ns. At the root (ns == "") it is byte-identical to the loop
// machinery's assembly: the run input plus every settled node's flat qualified
// output/outcome (an empty loop spec — a cond reads only inputs and node results, no
// iteration/body binding). Inside a run sub-formula (ns != "") it builds the
// GUARD-flavored namespace-local view (Q-C): the render view scopeFor renders against
// (env bindings + defaults + direct children) with the fold's direct-child outputs
// overlaid on top (child-wins, GIS ⚑S4 prompt parity), input nil, plus the ⚑B2
// spec-derived outcome backfill. So a bare cond ref resolves a sub-sibling or a
// sub-input binding and can never silently hit a same-named MAIN input.
func (d *driver) condScope(ns string, scope, nodeOutputs map[string]string) (loopScope, error) {
	if ns == "" {
		return d.loopScope(&loopSpec{}, 0, nil, nodeOutputs), nil
	}
	parts, err := d.subScope(ns, "guard", scope, nodeOutputs)
	if err != nil {
		return loopScope{}, err
	}
	// The render view (scopeFor: env bindings + declared sub-input defaults + direct scope
	// children at their bare ids), with the flat-nodeOutputs direct children overlaid on
	// top — child-wins, byte-identical to the pre-refactor overlay.
	view, err := d.scopeFor(ns, scope)
	if err != nil {
		return loopScope{}, err
	}
	for k, v := range parts.childView {
		view[k] = v
	}
	// ⚑B2: back-fill OutcomePass for the SPEC-DERIVED binding/default names ONLY — an env
	// binding, or a defaulted-unbound sub-input, is a pre-settled run input (root input
	// parity: "its outcome is pass"). A name shadowed by a direct-child key keeps that
	// node's real outcome, and a name already walked is left alone. NEVER blanket-stamp
	// view keys: a silent let is in the view but never settles, so stamping it would flip
	// `mylet.outcome == "pass"` TRUE inside ns where root yields "".
	outcomes := parts.outcomes
	bound := map[string]bool{}
	backfill := func(name string) {
		if parts.childKeys[name] {
			return
		}
		if _, ok := outcomes[name]; ok {
			return
		}
		outcomes[name] = OutcomePass
	}
	for _, f := range parts.spec.env {
		bound[f.name] = true
		backfill(f.name)
	}
	for _, fld := range parts.spec.inputFields {
		if !bound[fld.Name] && fld.Default != nil {
			backfill(fld.Name)
		}
	}
	return loopScope{input: nil, nodeOutputs: view, nodeOutcomes: outcomes}, nil
}

// loopScopeNS builds the LOOP-flavored namespace-local cond/attempts scope (⚑B1, Q-C):
// a TYPED sub-input layer as `input` (so an ORDERED cond `iteration >= max_review_rounds`
// compares number-to-number — root parity — instead of the render-string lexicographic
// compare a two-digit budget would defeat), the fold's direct children as a bare
// nodeOutputs/nodeOutcomes view (WITHOUT the binding layer — it now lives in `input`),
// and the just-settled attempt bound under the BARE body id (⚑B2). Resolution precedence
// is therefore iterationName → bodyBareID → input → children (root parity, input-FIRST),
// deliberately diverging from condScope's child-wins order: the loop's freeze premise
// requires input-first so a same-named node can never shadow a frozen cond value between
// ticks. bn is nil when evaluating a retry's attempts expression (before any attempt).
func (d *driver) loopScopeNS(spec *loopSpec, iteration int, bn *nodeState, ns string, scope, nodeOutputs map[string]string) (loopScope, error) {
	parts, err := d.subScope(ns, "loop", scope, nodeOutputs)
	if err != nil {
		return loopScope{}, err
	}
	input, err := d.typedSubInput(ns, parts.spec, scope)
	if err != nil {
		return loopScope{}, err
	}
	sc := loopScope{
		iterationName: spec.iterationName,
		iteration:     iteration,
		input:         input,
		nodeOutputs:   parts.childView,
		nodeOutcomes:  parts.outcomes,
	}
	if bn != nil {
		sc.bodyName = spec.bodyBareID
		sc.bodyOutcome = bn.Outcome
		sc.bodyOutput = bn.Output
	}
	return sc, nil
}

// loopEvalScope builds the scope a loop's cond (repeat) / attempts (retry) evaluates
// against for a unit in namespace u.ns. At the root it is the flat loopScope
// (byte-identical); inside a run sub-formula it is the namespace-local loop-flavored
// view (loopScopeNS). It is the single dispatcher both drivers' evalAttempts and
// loopDecide route through, so inline Run and pool Advance decide over an identical
// scope at any depth.
func (d *driver) loopEvalScope(u planUnit, iteration int, bn *nodeState, scope, nodeOutputs map[string]string) (loopScope, error) {
	if u.ns == "" {
		return d.loopScope(u.loop, iteration, bn, nodeOutputs), nil
	}
	return d.loopScopeNS(u.loop, iteration, bn, u.ns, scope, nodeOutputs)
}

// typedSubInput builds the loop cond/attempts INPUT layer for a run sub-formula's
// namespace (§1.2 ⚑B1): the run's declared inputs as TYPED scalars, so the loop cond
// compares against numbers/bools the way the ROOT cond compares against d.input. For
// each declared field: an env-BOUND field takes runInputLayer's render string RE-TYPED
// per the field's declared atomic type (number → ParseFloat, boolean → ParseBool, else
// the string verbatim; a parse failure keeps the string — the garbage-in root analog); a
// defaulted-UNBOUND field takes the field's TYPED default directly. It reads runInputLayer
// (env bindings against the parent view) + the IR field schema only, so genesis and
// resume build an identical layer (DET).
func (d *driver) typedSubInput(ns string, spec *runSpec, scope map[string]string) (map[string]any, error) {
	layer, err := d.runInputLayer(ns, scope)
	if err != nil {
		return nil, err
	}
	bound := make(map[string]bool, len(spec.env))
	for _, f := range spec.env {
		bound[f.name] = true
	}
	input := make(map[string]any, len(spec.inputFields))
	for _, fld := range spec.inputFields {
		switch {
		case bound[fld.Name]:
			input[fld.Name] = retypeScalar(layer[fld.Name], fld.Type)
		case fld.Default != nil:
			input[fld.Name] = fld.Default
		}
	}
	return input, nil
}

// retypeScalar re-types a run env binding's render STRING to the scalar its declared
// atomic type implies (root-cond parity): a "number" field ParseFloats, a "boolean"
// field ParseBools, and every other type (string, or a non-atomic) keeps the string
// verbatim. A parse failure keeps the string too — the garbage-in analog of the root
// path, where a malformed input value flows through as-is (loopScope.resolve then
// normalizes and compares it as a string).
func retypeScalar(s string, t ir.Type) any {
	if t.Kind == ir.TypeAtomic {
		switch t.Name {
		case "number":
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return f
			}
		case "boolean":
			if b, err := strconv.ParseBool(s); err == nil {
				return b
			}
		}
	}
	return s
}

// transparentOutcome aggregates a run's direct-member outcomes into the transparent
// run outcome, byte-for-byte mirroring runOutcome (reducer_state.go) over the member
// set: failed|canceled dominates → failed; any degraded → degraded; else pass. A
// skipped member contributes nothing (it did not run), so a sub-formula that
// skip-cascaded a tail still reports the honest outcome of the steps that ran.
func transparentOutcome(st *lumenState, members []string) string {
	var anyFailed, anyDegraded, anySettled bool
	for _, m := range members {
		n := st.Nodes[m]
		if n == nil || !n.Settled {
			continue
		}
		anySettled = true
		switch n.Outcome {
		case OutcomeFailed, OutcomeCanceled:
			anyFailed = true
		case OutcomeDegraded:
			anyDegraded = true
		}
	}
	switch {
	case anyFailed:
		return OutcomeFailed
	case anyDegraded:
		return OutcomeDegraded
	case anySettled:
		return OutcomePass
	default:
		return OutcomePass
	}
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
	// A repeat run body drives an inlined sub-graph per attempt, not a single leaf.
	if spec.bodyRun != nil {
		return d.runRunBodyLoop(u, scope, nodeOutputs)
	}

	// retry: evaluate the attempts budget ONCE. An invalid budget (non-integer or
	// < 1) settles the loop failed{invalid_input} with ZERO attempts (reference
	// parity). repeat has no budget expression (it is capped at lumenRepeatLoopCap).
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
	// parity). The scope key is NAMESPACE-QUALIFIED (u.ns + name) so an attempt unit
	// rendering in the loop's namespace resolves {{iteration}} via scopeFor's direct-child
	// overlay; at the root u.ns == "" so the key is the bare name (byte-identical). retry
	// has no iteration binding.
	iterKey := u.ns + spec.iterationName
	restore, had := "", false
	if spec.irKind == ir.NodeRepeat {
		restore, had = scope[iterKey]
		scope[iterKey] = strconv.Itoa(attempt + 1)
	}
	err := d.runUnit(au, scope, nodeOutputs)
	if spec.irKind == ir.NodeRepeat {
		if had {
			scope[iterKey] = restore
		} else {
			delete(scope, iterKey)
		}
	}
	return err
}

// runRunBodyLoop drives a repeat whose body is a run inline (Run/Resume). It mirrors
// runLoop but each attempt is a whole sub-graph (mintRunBodyAttempt) rather than a
// single leaf: it re-mints attempt N, drives every minted sub-unit + the attempt
// aggregate through runUnit (so resume memoization, per-attempt effect tokens, and
// crash boundaries apply for free at any nesting level), then decides via the shared
// loopDecide over the folded aggregate outcome. Only repeat reaches here (⚑S2 refuses
// retry+run), so maxAttempts is unused (the cap is lumenRepeatLoopCap).
func (d *driver) runRunBodyLoop(u planUnit, scope, nodeOutputs map[string]string) error {
	spec := u.loop
	for attempt := 0; ; attempt++ {
		aggAct := activationForAttempt(spec.bodyNodeID, attempt)
		bn := d.st().Nodes[aggAct]
		if bn == nil || !bn.Settled {
			if err := d.runRunBodyAttempt(u, attempt, scope, nodeOutputs); err != nil {
				return err
			}
			bn = d.st().Nodes[aggAct]
			if bn == nil || !bn.Settled {
				return fmt.Errorf("lumen: loop %q run-body attempt %d aggregate did not settle in-pass", u.nodeID, attempt)
			}
		}
		cont, err := d.loopDecide(u, attempt, 0, bn, scope, nodeOutputs)
		if err != nil {
			return err
		}
		if !cont {
			return nil
		}
	}
}

// runRunBodyAttempt mints and drives one repeat run-body attempt inline: it re-lowers
// the sub-graph (mintRunBodyAttempt), registers its env seam (⚑B1), emits the
// bookkeeping attempt.minted, then runs every minted sub-unit and finally the
// transparent aggregate through runUnit. The aggregate runs LAST (⚑S1 Tier-A FK
// ordering — its Members edges reference the sub-node rows). No iteration binding is
// threaded: the sub-graph renders against the run environment view (env-ref-to-
// iteration is refused at lowering, ⚑S5), so the iteration is only ever read by the
// loop's own cond (loopScope), never by a sub-node.
func (d *driver) runRunBodyAttempt(u planUnit, attempt int, scope, nodeOutputs map[string]string) error {
	spec := u.loop
	subUnits, agg, err := spec.mintRunBodyAttempt(attempt, u.activation, u.ns, u.afterDeps, u.rawAfter)
	if err != nil {
		return err
	}
	d.registerRunBodyEnv(spec, attempt, u.ns, subUnits)
	if err := d.appendAttemptMinted(u.activation, attempt, repeatRemaining(attempt)); err != nil {
		return err
	}
	for i := range subUnits {
		if err := d.runUnit(subUnits[i], scope, nodeOutputs); err != nil {
			return err
		}
	}
	return d.runUnit(agg, scope, nodeOutputs)
}

// repeatRemaining is the remaining-budget bookkeeping a repeat attempt stamps on
// attempt.minted: the loop cap minus the 1-based attempt number, floored at zero.
func repeatRemaining(attempt int) int {
	remaining := lumenRepeatLoopCap - (attempt + 1)
	if remaining < 0 {
		return 0
	}
	return remaining
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
		ns:         u.ns,         // render the attempt in the loop's namespace (decisionBodyUnit precedent)
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
		cs, err := d.loopEvalScope(u, iteration, bn, scope, nodeOutputs)
		if err != nil {
			return false, fmt.Errorf("lumen: loop %q cond: %w", u.nodeID, err)
		}
		truthy, err := evalCondTruthy(spec.condExpr, cs)
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
		// ⚑B2: the cond names the body by its BARE authored id (root: bare == qualified).
		sc.bodyName = spec.bodyBareID
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

// runInputLayer derives a namespace's INPUT layer (§C, layers (a)+(b)): the run
// environment bindings (evaluated in IR field order against the PARENT view, keyed by
// the sub-input field name) plus declared sub-input defaults for any unbound field. It
// is the shared source of that layer for both scopeFor (which overlays the direct-child
// outputs on top) and evalForEachArray's member arm (which reads it ALONE — never the
// children). The parent view is derived override-aware: a repeat run attempt namespace
// consults an explicit parentNS override BEFORE the structural parentNamespace, whose
// string-derived parent is a phantom (no env spec) that would collapse the parent view
// to {} and every binding to "". It reads no clock and no randomness, so genesis,
// re-Advance, and crash-resume build an identical layer (DET). ns is always non-empty
// here (both callers handle ns == "" before descending).
func (d *driver) runInputLayer(ns string, scope map[string]string) (map[string]string, error) {
	parentNS := parentNamespace(ns)
	if override, ok := d.parentNS[ns]; ok {
		parentNS = override
	}
	parent, err := d.scopeFor(parentNS, scope)
	if err != nil {
		return nil, err
	}
	view := make(map[string]string)
	if spec := d.runEnvs[ns]; spec != nil {
		bound := make(map[string]bool, len(spec.env))
		for _, f := range spec.env {
			v, err := evalValue(f.value, parent)
			if err != nil {
				return nil, fmt.Errorf("lumen: run env %q: %w", f.name, err)
			}
			view[f.name] = v
			bound[f.name] = true
		}
		for _, fld := range spec.inputFields {
			if !bound[fld.Name] && fld.Default != nil {
				view[fld.Name] = scalarDefaultToString(fld.Default)
			}
		}
	}
	return view, nil
}

// scopeFor derives the render view for a unit's namespace (§C). The root
// namespace ("") IS the flat scope, so a run-free document renders byte-identically
// to before. A run sub-formula's namespace ("greeting/") is the input LAYER
// (runInputLayer: the run's environment bindings + declared sub-input defaults) plus
// (c) the settled outputs of the namespace's DIRECT sub-nodes, which shadow same-named
// bindings exactly as record() shadows baseScope. It reads no clock and no randomness —
// everything derives from (bundle, input, fold state) — so genesis, re-Advance, and
// crash-resume build an identical view (DET). It is recomputed per render (cheap at
// these sizes), so there is no cache to invalidate across passes.
func (d *driver) scopeFor(ns string, scope map[string]string) (map[string]string, error) {
	if ns == "" {
		return scope, nil
	}
	view, err := d.runInputLayer(ns, scope)
	if err != nil {
		return nil, err
	}
	for k, v := range scope {
		if rest, ok := directChildKey(k, ns); ok {
			view[rest] = v
		}
	}
	return view, nil
}

// runEnvIndex maps each run sub-formula's sub-namespace ("<runID>/") to its env
// spec, so scopeFor can resolve bindings by namespace. It is a pure function of the
// lowered units, so a fresh run and a rebuild produce the same index.
func runEnvIndex(units []planUnit) map[string]*runSpec {
	idx := make(map[string]*runSpec)
	for i := range units {
		if units[i].kind == unitRun && units[i].run != nil {
			idx[units[i].nodeID+"/"] = units[i].run
		}
	}
	return idx
}

// registerRunBodyEnv wires attempt N's env seam into scopeFor (⚑B1), so a run body's
// sub-do prompts render against the environment bindings rather than "" (evalValue's
// ref arm has no missing-key error, so the corruption would be silent). It is pure —
// re-derived identically every pass and resume (genesis ≡ resume), so re-registering
// is an idempotent map write. Two facts are registered:
//
//	(i)  the attempt namespace `<bodyNodeID>/<N>/` → the body run's env spec, PLUS an
//	     explicit parent-namespace override = the loop's namespace (loopNS), consulted
//	     by scopeFor BEFORE the structural parentNamespace (which would resolve to the
//	     phantom `<bodyNodeID>/`);
//	(ii) every nested run inside the attempt sub-graph → its own env spec; their
//	     string-derived parent is `<bodyNodeID>/<N>/`, now registered by (i), so
//	     scopeFor's recursion resolves the whole chain.
func (d *driver) registerRunBodyEnv(spec *loopSpec, attempt int, loopNS string, subUnits []planUnit) {
	if d.runEnvs == nil {
		d.runEnvs = map[string]*runSpec{}
	}
	if d.parentNS == nil {
		d.parentNS = map[string]string{}
	}
	attemptNS := spec.bodyNodeID + "/" + strconv.Itoa(attempt) + "/"
	d.runEnvs[attemptNS] = spec.bodyRun
	d.parentNS[attemptNS] = loopNS
	for i := range subUnits {
		if subUnits[i].kind == unitRun && subUnits[i].run != nil {
			d.runEnvs[subUnits[i].nodeID+"/"] = subUnits[i].run
		}
	}
}

// parentNamespace returns the enclosing namespace of a run namespace: "greeting/"
// -> "" (root), "outer/inner/" -> "outer/". A namespace always ends in "/".
func parentNamespace(ns string) string {
	trimmed := strings.TrimSuffix(ns, "/")
	idx := strings.LastIndex(trimmed, "/")
	if idx < 0 {
		return ""
	}
	return trimmed[:idx+1]
}

// directChildKey reports whether a flat scope key is a DIRECT child of ns (exactly
// one segment deeper, no further "/") and returns the child's bare name.
// "greeting/hello" under "greeting/" -> ("hello", true); "outer/inner/leaf" under
// "outer/" -> ("", false) — that key belongs to the inner namespace's view.
func directChildKey(key, ns string) (string, bool) {
	if !strings.HasPrefix(key, ns) {
		return "", false
	}
	rest := key[len(ns):]
	if rest == "" || strings.Contains(rest, "/") {
		return "", false
	}
	return rest, true
}

// scalarDefaultToString stringifies an IR input default exactly as baseScope seeds
// a run input value (string as-is, else canonical JSON), so a defaulted sub-input
// renders identically to the same value passed explicitly.
func scalarDefaultToString(v any) string {
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return ""
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
