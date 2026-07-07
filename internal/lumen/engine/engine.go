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
	// Events is the full committed journal for the run, in seq order.
	Events []graphstore.StoredEvent
}

// RegisterVocabulary registers the executor's frozen event vocabulary against
// the store so Append accepts its events. Registration is idempotent.
func RegisterVocabulary(store *graphstore.Store) {
	for _, t := range EventTypes {
		store.RegisterEventType(Engine, t)
	}
}

// Options tune a run. The zero value (nil Host) refuses a do node with
// ErrUnsupportedNode — no agent host, no do steps.
type Options struct {
	// Host runs agent `do` steps. Nil refuses do nodes.
	Host enginehost.AgentHost
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

	units, err := buildUnits(doc.Nodes, opts.Host != nil)
	if err != nil {
		return RunResult{}, err
	}

	streamID := streamIDForRun(doc.Name, opts.Host != nil)
	RegisterVocabulary(store)

	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: acquire writer lease %q: %w", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	reducer := lumenReducer{}
	d := &driver{
		ctx:      ctx,
		store:    store,
		streamID: streamID,
		irVer:    doc.Contract.Version,
		epoch:    lease.Epoch,
		reducer:  reducer,
		state:    reducer.Zero(streamID),
		host:     opts.Host,
	}

	createdAt := time.Now().UTC().Format(time.RFC3339Nano)
	if err := d.append(EventRunStarted, streamID+":run:started", runStartedPayload{
		RootID:    streamID,
		Name:      doc.Name,
		IRHash:    irHash(doc),
		CreatedAt: createdAt,
	}); err != nil {
		return RunResult{}, err
	}

	nodeOutputs := make(map[string]string)
	scope := baseScope(input)

	for i := range units {
		if err := d.runUnit(units[i], scope, nodeOutputs); err != nil {
			return RunResult{}, err
		}
	}

	runOutcome := d.st().runOutcome()
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
		return nil
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
		return nil
	}

	switch u.kind {
	case unitLeaf:
		return d.runLeaf(u, scope, nodeOutputs)
	case unitScatterAgg:
		return d.runScatter(u, nodeOutputs)
	case unitGather:
		return d.runGather(u, scope, nodeOutputs)
	default:
		return fmt.Errorf("%w: unit %q", ErrUnsupportedNode, u.nodeID)
	}
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
		stdout, _, exitCode, runErr := exechost.Run(d.ctx, u.leaf.program, script, u.leaf.cwd, u.leaf.env)
		if runErr != nil {
			return fmt.Errorf("lumen: exec %q: %w", u.nodeID, runErr)
		}
		output := strings.TrimRight(stdout, "\n")
		if err := d.appendSettled(u.activation, outcomeForExit(exitCode, u.leaf.passCodes), output, ""); err != nil {
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
	// NOTE (N2): the attempt suffix is pinned to 1 — one activation per node in
	// P4.2 (retry/repeat re-activation is deferred). When retry lands (P4.3), the
	// idem token must key on the live attempt number so at_least_once re-acts under
	// the same token and at_most_once mints a fresh one.
	effectIdem := d.streamID + ":" + u.nodeID + ":do:1"
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

	result, runErr := d.host.RunDo(d.ctx, enginehost.DoRequest{
		RunID:      d.streamID,
		NodeID:     u.nodeID,
		Activation: u.activation,
		IdemToken:  effectIdem,
		Prompt:     prompt,
		AgentRef:   u.leaf.agentRef,
	})
	nodeOutcome, effResult, detail, out, session := foldDoResult(result, runErr)

	if err := d.append(EventEffectSettled, effectIdem+":done", effectSettledPayload{
		Activation: u.activation,
		IdemToken:  effectIdem,
		Result:     effResult,
		Output:     out,
		Session:    session,
		Detail:     detail,
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

// appendSettled emits an outcome.settled event for an activation.
func (d *driver) appendSettled(activation, outcome, output, detail string) error {
	return d.append(EventOutcomeSettled, d.streamID+":"+activation+":settled", outcomeSettledPayload{
		Activation: activation,
		Outcome:    outcome,
		Output:     output,
		Detail:     detail,
	})
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
	if err := graphstore.ApplyDelta(d.ctx, tx, delta); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("lumen: apply delta for %s: %w", eventType, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lumen: commit projection for %s: %w", eventType, err)
	}
	d.head = seq
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
