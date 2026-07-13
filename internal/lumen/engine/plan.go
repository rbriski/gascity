package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/lumen/exechost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// reservedDoMetadataKeys are the engine-owned routing keys lumenDispatchWork stamps
// LAST onto every minted work bead. A translated pack may not carry them as static
// do metadata: refusing them at decode is a belt-and-suspenders guard beside the
// dispatch seam's stamp-last ordering, so a clobber attempt of the authoritative
// routing keys fails LOUD at enqueue rather than being silently overwritten.
var reservedDoMetadataKeys = map[string]bool{
	beadmeta.RoutedToMetadataKey:        true,
	beadmeta.LumenRunMetadataKey:        true,
	beadmeta.LumenActivationMetadataKey: true,
	beadmeta.LumenAttemptMetadataKey:    true,
}

// lumenDurationRE is the reference compiler's compile-time duration grammar
// (isParseableLumenDuration, formula-language index.ts): a non-negative integer with no
// leading zero (or a bare 0) followed by one of ms/s/m/h. A timeout's raw duration literal is
// validated against this SHAPE at load and then rides the planUnit VERBATIM — the engine reads
// no clock and never parses it as a time.Duration, so a load-time refusal is stricter than the
// reference RUNTIME (which never re-checks), parity with its compile-time diagnostic.
var lumenDurationRE = regexp.MustCompile(`^(?:0|[1-9]\d*)(?:ms|s|m|h)$`)

// ErrUnsupportedNode is returned when the formula body contains a node kind the
// P4.2 executor does not implement. It is a load-time-style refusal surfaced
// before any effect runs — the executor never silently skips a node it cannot
// honor. The P4.2 core implements block/exec/settle/lit/interp/do plus the DAG
// arms scatter(members) and gather(authored). The remaining kinds are deferred
// behind this refusal (see buildUnits).
var ErrUnsupportedNode = errors.New("lumen: unsupported node kind")

// step is one executable leaf's decoded payload.
type step struct {
	kind ir.NodeKind
	id   string

	// exec fields.
	program        string
	script         string
	passCodes      []int
	retryableCodes []int // exec exitMap.retryable: exit codes a retry re-attempts
	// pendingCodes is exec exitMap.pending: exit codes that settle OutcomePending (the
	// check is not done yet — re-poll WITHOUT consuming the repeat budget). It is
	// decoded ONLY on a repeat leaf body (decodeExec's allowPending arm); on any other
	// exec it is refused at lowering, so a non-repeat exec never carries it and settles
	// byte-identically to before.
	pendingCodes []int
	cwd          string
	env          []string

	// settle fields.
	outcome string

	// do fields.
	agentRef string
	// metadata is a do node's optional STATIC routing/affinity metadata (chiefly
	// gc.continuation_group) that rides VERBATIM onto the minted work bead — the
	// TNK Duration payload-only precedent for a map[string]string. It is decoded
	// once in decodeDo (the single decode point) behind two decode-time walls
	// (static-literal + reserved-key), so it is a pure function of the IR and folds
	// byte-identically on every Advance pass with no scope dependency.
	metadata map[string]string

	// settle/lit/interp/do value evaluation.
	raw map[string]json.RawMessage
}

// unitKind classifies a plan unit's execution shape.
type unitKind int

const (
	unitLeaf           unitKind = iota // exec / settle / lit / interp / do
	unitScatterAgg                     // scatter aggregate: settles after its members
	unitGather                         // gather: head-of-line drain + authored combine
	unitLoop                           // retry / repeat: the attempt-loop over a leaf body
	unitRun                            // run: transparent sub-formula call over an inlined sub-graph
	unitGuard                          // guard: a conditional single-step arm (cond ? then : pass)
	unitDispatch                       // dispatch: a multi-way branch (subject -> matching arm's body)
	unitForEach                        // for-each: a dynamic scatter fanning a single-leaf or run-sub-formula body over a runtime array
	unitCleanup                        // cleanup: try/finally — a guarded leaf, then an always-run body leaf
	unitRecover                        // recover: try/catch — a guarded leaf, then a catch body run only on failure
	unitCleanupGuarded                 // cleanup(block guarded): a synthetic transparent drain aggregate over the block's inlined leaves
	unitTimeout                        // timeout: an advisory check-with-budget wrapper over a single-leaf body (the guard path MINUS the cond — the body always runs)
)

// planUnit is one node of the lowered execution plan. Units are emitted in
// dependency (topo) order; each carries its resolved activation-key deps so the
// executor drives the DAG and the reducer folds it.
type planUnit struct {
	kind        unitKind
	activation  string
	nodeID      string
	irKind      ir.NodeKind
	parent      string // parent activation ("" = top-level, folds under the root)
	memberIndex *int
	silent      bool   // pure lit/interp: compute scope, emit no journal events
	ns          string // the namespace this unit renders in ("" = root, "greeting/" = a run sub-graph)

	rawAfter []string // IR `after` node ids

	// Dependencies are split by kind so the drain exception is scoped correctly
	// (H1): afterDeps are blocking gates (a failed/skipped one skip-cascades this
	// unit), memberDeps drain (a scatter aggregate / gather waits for them to
	// settle but any outcome is fine — a member failure does not skip the
	// aggregate).
	afterDeps  []string // resolved blocking `after` gates
	memberDeps []string // resolved drain dependencies (scatter members / gather over-scatter)

	leaf step // unitLeaf payload

	members     []string // unitScatterAgg: direct member activation keys
	onFail      string   // scatter on_fail ("continue" | "stop")
	overScatter string   // unitGather: the drained scatter's aggregate activation key

	gatherMembers []string   // unitGather: drained member activations, member order
	combine       []planUnit // unitGather: authored combine leaf units

	loop *loopSpec // unitLoop: the attempt-loop spec (retry / repeat)

	run *runSpec // unitRun: the transparent sub-formula-call spec

	guard *guardSpec // unitGuard: the conditional single-step spec

	dispatch *dispatchSpec // unitDispatch: the multi-way branch spec

	forEach *forEachSpec // unitForEach: the dynamic-scatter fan-over-array spec

	cleanup *cleanupSpec // unitCleanup: the try/finally guarded+body spec

	recover *recoverSpec // unitRecover: the try/catch guarded+body spec

	timeout *timeoutSpec // unitTimeout: the advisory check-with-budget wrapper spec
}

// timeoutSpec carries a timeout node's decoded shape: the RAW literal duration string
// (VERBATIM — never parsed; advisory journal metadata only, stamped on the wrapper's own
// node.activated by appendActivated) and the single leaf body (exec/do) that ALWAYS runs. The
// wrapper is TRANSPARENT: it settles with the body's outcome/output (settleDecisionFromBody
// records both at the wrapper id), reads no clock, and never fails the body for time —
// enforcement is gc-side, later, off the JOURNAL wrapper activation (never the bead). It is
// structurally a guardSpec MINUS the cond: the body is not conditional, so the driver is the
// guard path with the cond removed (no skipped arm). The body id shares the guard-then
// synthesis machinery (decisionBodyUnit / addSynth), so a body id colliding with a sibling
// node or another decision body refuses loudly at lowering.
type timeoutSpec struct {
	duration   string      // the raw literal duration string, VERBATIM (e.g. "5m")
	bodyNodeID string      // the body's node id, QUALIFIED (mint prefix / addSynth / decisionBodyUnit)
	bodyIRKind ir.NodeKind // NodeExec | NodeDo
	body       step        // the decoded single leaf body
}

// recoverSpec carries a recover(try/catch) node's decoded shape: a `guarded` leaf that
// runs, and a `body` (catch) leaf that runs ONLY when the guarded FAILED — with the
// guarded's error bound into the body scope under `errorBinding` (`error.reason` =
// the guarded's failure detail, `error.step` = its node id). A guarded that passes or
// degrades settles the recover transparently from itself (the catch never runs); a
// CANCELED guarded is likewise transparent (cancellation is not a recoverable failure).
// Both are single leaves synthesized as sub-units parented under the recover (out of the
// run's top-level aggregation). Structurally the twin of cleanupSpec with an inverted,
// conditional body.
type recoverSpec struct {
	guardedNodeID string
	guardedIRKind ir.NodeKind
	guarded       step
	bodyNodeID    string
	bodyIRKind    ir.NodeKind
	body          step
	errorBinding  string
}

// cleanupSpec carries a cleanup(try/finally) node's decoded shape: a `guarded` leaf
// that runs, and a `body` (finally) leaf that runs ALWAYS afterward — even when the
// guarded step failed. The cleanup settles from the guarded's outcome UNLESS the body
// itself fails, in which case the body's failure wins (finally never swallows a
// success but its own failure supersedes). Both are single leaves (exec/do/settle);
// they are synthesized as sub-units parented under the cleanup (so their outcomes stay
// out of the run's top-level aggregation — only the cleanup settles into it). The
// always-run edge is driver-side (the body has no blocking dep on the guarded), so a
// guarded failure never skip-cascades the teardown.
//
// When `guarded` is a BLOCK (guardedAgg != ""), the leaf form fields are empty: the
// block's leaf children are lowered as ordinary BARE-ID units parented to a synthetic
// transparent drain aggregate (unitCleanupGuarded, activation guardedAgg), and the
// cleanup drains that aggregate via memberDeps rather than driving a single guarded leaf.
// The finally (body) stays a single imperatively-driven leaf in either form.
type cleanupSpec struct {
	guardedAgg    string // block form: the drain-aggregate activation ("" = single-leaf guarded form)
	guardedNodeID string
	guardedIRKind ir.NodeKind
	guarded       step
	bodyNodeID    string
	bodyIRKind    ir.NodeKind
	body          step
}

// forEachSpec carries a scatter(form:each) node's decoded shape: the `over` array
// expression, the node/input refs it reads (for the DET gate), the per-element
// binding name, the single body fanned once per element, and the on_fail policy. The
// member COUNT is a runtime property of the evaluated array (unknown at lowering), so
// — unlike scatter(members) — no member units are lowered; the driver materializes one
// member per element at run time (mirroring a loop's per-attempt minting) under a
// `<forEachID>/<index>` node id, with `binder` bound to the element.
//
// The body is EITHER a single leaf (bodyIRKind exec/do, bodyRun nil — the FIS shape,
// fanned via forEachMemberUnit) OR a `run <formula> given {…}` sub-formula call
// (bodyRun != nil, bodyIRKind NodeRun — the FBR shape): then each element mints the
// target's whole sub-graph FRESH under `<forEachID>/<index>/` (mintRunBody, the shared
// RBL helper) and the member aggregate settles transparently at
// activationFor(<forEachID>/<index>). The embedded runBodyStash carries the re-lowering
// context (nil/zero for a leaf body). The aggregate drains those dynamic members with
// the same outcome rule as scatter(members). The decision (the array) is frozen by the
// over-ref gate (a member-`over` reads the immutable input LAYER and contributes no
// gate; a bare-ref `over` gates on the node it reads), so genesis, re-Advance, and
// drop+refold fan the same members.
type forEachSpec struct {
	overRaw    json.RawMessage // the `over` array expression
	overRefs   []string        // node names `over` reads (head-derived; for the DET gate)
	binder     string          // the per-element binding name (e.g. "item")
	bodyIRKind ir.NodeKind     // NodeExec | NodeDo | NodeRun
	body       step            // the decoded single leaf body (fanned per element; empty when bodyRun != nil)
	onFail     string          // "continue" | "stop"

	// Run-body fields (fan whose member is `run <formula> given {…}`, the FBR slice):
	// non-nil bodyRun switches the fan into the run-body member arm, and the embedded
	// runBodyStash carries the lowering context mintRunBody re-invokes per member.
	bodyRun *runSpec
	runBodyStash
}

// dispatchSpec carries a dispatch node's decoded shape: the subject value expression
// to branch on, the node/input refs it reads (for the DET gate — the branch must be
// decided over stable state), and the ordered arms. At run time the subject is
// evaluated to a string and the FIRST arm whose match value equals it runs its body
// (the dispatch settles transparently from it); no matching arm settles the dispatch
// PASS with an empty result (a no-op that does not skip-cascade). Like guard, the
// decision is a pure function of the fold.
type dispatchSpec struct {
	subject     json.RawMessage
	subjectRefs []string
	arms        []dispatchArm
}

// dispatchArm is one discriminated-variant arm: a match value and the body to run
// when the subject equals it. The body is EITHER a single leaf (bodyIRKind exec/do,
// bodyRun nil — the pre-DAR shape) OR a `run <formula> given {…}` sub-formula call
// (bodyRun != nil, bodyIRKind NodeRun — the DAR shape): then the MATCHED arm mints
// the target's whole sub-graph FRESH under `<bodyNodeID>/` (mintRunBody, the shared
// RBL/FBR helper) and the arm aggregate settles transparently at
// activationFor(bodyNodeID) = bodyID:0. The embedded runBodyStash carries the
// re-lowering context (nil/zero for a leaf body) and is PER-ARM (Q-B: each arm targets
// a different sub-formula, so the stash cannot be shared). unchosen arms mint NOTHING.
type dispatchArm struct {
	matchValue string
	bodyNodeID string
	bodyIRKind ir.NodeKind
	body       step // the decoded single leaf body (empty when bodyRun != nil)

	// Run-body fields (a `run <formula> given {…}` arm, the DAR slice): non-nil bodyRun
	// switches the arm into the run-body arm (it also selects the mint route at run
	// time), and the embedded runBodyStash carries the lowering context mintRunBody
	// re-invokes when the arm is matched. All are nil/zero for a leaf body.
	bodyRun *runSpec
	runBodyStash
}

// guardSpec carries a guard node's decoded shape: the closed condition and the
// single leaf `then` step to run when the condition is truthy. It is the decision
// arm — cond true runs `then` and the guard settles with `then`'s outcome
// (transparent); cond false settles the guard `pass` WITHOUT running `then` (a
// conditional step that legitimately did not run, so it does NOT skip-cascade its
// dependents). The condition is a closed expression over the run's settled node
// outcomes/outputs + input — the same subset a repeat `until` evaluates — so the
// decision is a pure function of the fold and re-evaluates identically on resume.
type guardSpec struct {
	cond       json.RawMessage
	condRefs   []string // node/input names the cond reads (for the DET gate — see resolveDeps)
	thenNodeID string
	thenIRKind ir.NodeKind
	then       step
}

// runSpec carries a run node's decoded shape: the target formula name, the
// ordered environment bindings (evaluated against the parent scope view to seed
// the sub-formula's input scope), the sub-input schema (for default seeding),
// and the parent-scope ref names the bindings read (so resolveDeps can gate the
// sub-graph on those parent nodes — keeping the sub-scope stable before any
// sub-unit renders, DET seed #3). The sub-formula's nodes are inlined as ordinary
// units under a `<runID>/` namespace; this spec drives only the transparent
// settle and the boundary scope translation.
type runSpec struct {
	target      string
	env         []runEnvField // ordered env bindings (sub-input field name <- value expr)
	envRefs     []string      // parent-scope ref names read by the bindings (for DET gating)
	inputFields []ir.Field    // the sub-formula's declared input schema
}

// runEnvField is one environment binding: a sub-input field name and the value
// expression evaluated against the parent scope view at render time.
type runEnvField struct {
	name  string
	value json.RawMessage
}

// runBodyStash bundles the lowering context a run BODY's sub-formula is re-lowered
// against on every materialization — captured at lower time (from the enclosing
// lowerer) so the runtime mint is byte-identical to the lower-time dry run (single
// source, no drift). It is embedded in THREE specs — loopSpec (the repeat run-body
// arm, RBL), forEachSpec (the for-each run-body member arm, FBR), and dispatchArm (the
// dispatch run-body arm, DAR) — and mintRunBody is a FREE FUNCTION over it (Q-B: a
// struct, not an interface); the DAR stash is PER-ARM (arms target distinct
// sub-formulas). The switching `bodyRun *runSpec`
// stays a sibling field on each spec (it also selects the arm), and mintRunBody takes
// it explicitly. Field names keep the `body` prefix so the promoted accessors match
// the pre-FBR loopSpec field names (run_loop white-box tests read them unchanged).
type runBodyStash struct {
	bodyFormula        *ir.IR            // the target sub-formula (re-lowered per mint)
	bodyFormulas       map[string]*ir.IR // the bundle (nested-run resolution)
	bodyTargetStack    []string          // lowering-time targetStack (cycle guard for nested runs)
	bodyAllowDo        bool              // whether a `do` may lower in the sub-graph
	bodyAllowCombineDo bool              // whether a combine `do` may lower in the sub-graph
}

// loopSpec carries a retry/repeat loop node's decoded shape: the body it
// re-attempts, and the closed exit expression that decides re-runs (retry:
// attempts count; repeat: the `until` condition + iteration binding). The body is
// usually a SINGLE leaf (exec / do); block / nested-loop / scatter bodies are
// refused at lowering (§3.2). A repeat body MAY be a `run` (a transparent
// sub-formula call — the RBL slice): then bodyRun (+ the re-lowering fields below)
// is set, bodyIRKind == NodeRun, and each attempt mints the sub-graph fresh under
// `<bodyNodeID>/<N>/` (mintRunBodyAttempt), the attempt aggregate settling at
// activationForAttempt(bodyNodeID, N). retry with a run body is refused (⚑S2 — a
// retry whose body settles a transparent aggregate can never re-attempt). No IR is
// read at run time — the driver evaluates these expressions over folded attempt
// outcomes (D-P4-1).
type loopSpec struct {
	irKind        ir.NodeKind     // NodeRetry | NodeRepeat
	bodyNodeID    string          // the body's node id, QUALIFIED (mint prefix / addSynth / attemptUnit / liveAttempt)
	bodyBareID    string          // the body's AUTHORED bare id (⚑B2): the bare-name compares — freeze allowlist, ⚑S5 env self-ref ban, loopScope.bodyName. Root: bare == qualified.
	bodyIRKind    ir.NodeKind     // NodeExec | NodeDo | NodeRun
	body          step            // the decoded leaf body (empty when bodyRun != nil)
	attemptsExpr  json.RawMessage // retry: the attempts count expression (closed subset)
	attemptsRefs  []string        // ref names the attempts expr reads (charset + synth-ban sweeps, NEVER a gate)
	condExpr      json.RawMessage // repeat: the `until` exit condition (closed subset)
	condRefs      []string        // ref names the cond reads (charset + synth-ban sweeps, NEVER a gate)
	iterationName string          // repeat: the 1-based iteration binding name

	// Run-body fields (repeat { run <formula> given {…} }, the RBL slice): non-nil
	// bodyRun switches the loop into the run-body arm, and the embedded runBodyStash
	// carries exactly the lowering context mintRunBody re-invokes per attempt. All are
	// nil/zero for a leaf body.
	bodyRun *runSpec // the body run's decoded spec (env + target)
	runBodyStash
}

// allDeps returns the union of a unit's blocking and drain dependencies, for
// topological ordering (both kinds constrain execution order).
func (u *planUnit) allDeps() []string {
	if len(u.memberDeps) == 0 {
		return u.afterDeps
	}
	deps := make([]string, 0, len(u.afterDeps)+len(u.memberDeps))
	deps = append(deps, u.afterDeps...)
	deps = append(deps, u.memberDeps...)
	return deps
}

// activationFor returns the attempt-0 activation key for a node id — the form
// every non-loop node uses (exactly one attempt). It is byte-identical to the
// historical single-attempt key, so all pre-L5 lowering sites stay unchanged.
func activationFor(nodeID string) string { return activationForAttempt(nodeID, 0) }

// activationForAttempt returns the activation key for a node id's Nth attempt
// (0-based): nodeID + ":" + N. Attempt 0 is the historical single-attempt key,
// so activationFor(n) == activationForAttempt(n, 0). Only a retry/repeat loop
// body mints attempt > 0; every other node has exactly one attempt (0).
func activationForAttempt(nodeID string, attempt int) string {
	return nodeID + ":" + strconv.Itoa(attempt)
}

// buildUnits lowers a formula body to a topologically ordered plan. block nodes
// are transparent; exec/settle/lit/interp/do become leaf units; scatter(members)
// lowers to member leaves plus an aggregate; gather(authored) lowers to a gather
// unit with an inline combine.
//
// allowDo gates whether a `do` node lowers at all: it is set when the run can
// place a do somewhere — a configured Host (Run/Resume run it inline) OR a
// PoolRouter (Advance materializes a TOP-LEVEL do as pool work). allowCombineDo
// is the stricter gate for a do nested INSIDE a gather combine: a combine runs
// inline inside runGather (runUnit -> runDo), so it needs a Host — pool
// materialization only happens on the top-level walk. A pool-mode Advance with no
// Host therefore sets allowDo=true but allowCombineDo=false, and a combine `do` is
// refused HERE at lowering (before any append) rather than hard-failing late in
// runGather after the scatter members already ran (M2).
func buildUnits(doc *ir.IR, allowDo, allowCombineDo bool) ([]planUnit, error) {
	l := &lowerer{
		allowDo:        allowDo,
		allowCombineDo: allowCombineDo,
		formulas:       doc.Formulas,
		inputNames:     fieldNameSet(doc.Input.Fields),
		scatterMembers: map[string][]string{},
	}
	if err := l.lowerNodes(doc.Nodes, ""); err != nil {
		return nil, err
	}
	if err := l.resolveDeps(); err != nil {
		return nil, err
	}
	return topoSortUnits(l.units)
}

type lowerer struct {
	allowDo        bool
	allowCombineDo bool
	units          []planUnit
	scatterMembers map[string][]string // qualified scatter node id -> member activation keys (in order)

	// formulas is the sub-formula bundle a `run` target resolves against; prefix is
	// the current namespace ("" at the top level, "greeting/" inside a run); targetStack
	// is the chain of formula names being inlined, so a recursive cycle is refused loudly.
	// inAggregate is true while lowering a scatter member / gather combine. It (not the
	// bare parent activation, which is also non-empty for an inlined sub-formula's own
	// statements) fences the aggregate-unsupported kinds — for-each, cleanup, recover —
	// from an aggregate placement. A `run` no longer consults it (⚑SF-2: run-in-combine is
	// fenced SOLELY by lowerCombine's leaf-only sweep), and a `loop` fences on prefix, not
	// inAggregate; a scatter-member run resets it to false around its inlined sub-graph so
	// the sub-formula's own top-level run/loop stay legal.
	formulas    map[string]*ir.IR
	prefix      string
	targetStack []string
	inAggregate bool

	// inputNames is the MAIN document's declared input field names — the immutable,
	// tick-stable ref set a run-body repeat cond may read besides its own body and
	// iteration (the re-decide freeze in lowerLoop). Run-body loops are entry-top-level
	// only (prefix fence + inAggregate fence), so the main input is always the right
	// namespace for the check.
	inputNames map[string]bool
}

// fieldNameSet returns the set of declared input field names, for the run-body
// cond ref freeze.
func fieldNameSet(fields []ir.Field) map[string]bool {
	names := make(map[string]bool, len(fields))
	for _, f := range fields {
		names[f.Name] = true
	}
	return names
}

// qid qualifies an authored node id with the current namespace prefix. Authored
// ids are '/'- and ':'-free (enforced at lowerNode entry), so the only '/' in a
// qualified id comes from a run prefix and the only ':' comes from the activation
// key — keeping activationNodeID/activationAttempt parsing unambiguous and the
// namespaced scope keys unreachable from any `{{ref}}` (scope isolation, §C).
func (l *lowerer) qid(id string) string { return l.prefix + id }

// qAfter qualifies a node's `after` ids into the current namespace. A run's sub-
// formula only references its own siblings, so each ref resolves within the
// namespace; a dangling ref stays a loud resolveDeps error.
func (l *lowerer) qAfter(after []string) []string {
	if len(after) == 0 {
		return nil
	}
	out := make([]string, len(after))
	for i, a := range after {
		out[i] = l.qid(a)
	}
	return out
}

func (l *lowerer) lowerNodes(nodes []ir.Node, parent string) error {
	for i := range nodes {
		if err := l.lowerNode(nodes[i], parent, nil); err != nil {
			return err
		}
	}
	return nil
}

func (l *lowerer) lowerNode(n ir.Node, parent string, memberIndex *int) error {
	// Authored node ids must be '/'- and ':'-free: '/' is the run-namespace
	// delimiter and ':' is the activation-attempt delimiter. Refusing them here
	// (before any unit is emitted) keeps qualified ids unambiguous and stops a
	// hand-authored id from forging a cross-namespace scope key (§B/§C).
	if strings.ContainsAny(n.ID, "/:") {
		return fmt.Errorf("lumen: node id %q must not contain '/' or ':' (reserved delimiters)", n.ID)
	}
	switch n.Kind {
	case ir.NodeBlock:
		members, err := childNodes(n.Raw["members"])
		if err != nil {
			return fmt.Errorf("lumen: block %q: %w", n.ID, err)
		}
		return l.lowerNodes(members, parent)

	case ir.NodeExec:
		s, err := decodeExec(n, false)
		if err != nil {
			return err
		}
		l.addLeaf(n, parent, memberIndex, s, false)
		return nil

	case ir.NodeSettle:
		l.addLeaf(n, parent, memberIndex, decodeSettle(n), false)
		return nil

	case ir.NodeLit, ir.NodeInterp:
		// Loud wall (§1.2.5): a silent lit/interp leaf renders through evalValue and CANNOT
		// index, so ANY index interpolation is refused here (corpus-absent; this avoids
		// defining silent-render failure semantics this slice). Gather-combine interp members
		// ride this arm.
		if err := sweepIndexParts(n.Raw, false); err != nil {
			return err
		}
		// A call-looking literal part (`ident(...)`) can never render either — refuse it too.
		if err := sweepCallParts(n.Raw); err != nil {
			return err
		}
		l.addLeaf(n, parent, memberIndex, step{kind: n.Kind, id: n.ID, raw: n.Raw}, true)
		return nil

	case ir.NodeDo:
		if !l.allowDo {
			return fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, n.Kind, n.ID)
		}
		s, err := decodeDo(n)
		if err != nil {
			return err
		}
		l.addLeaf(n, parent, memberIndex, s, false)
		return nil

	case ir.NodeScatter:
		return l.lowerScatter(n, parent)

	case ir.NodeGather:
		return l.lowerGather(n, parent)

	case ir.NodeRetry, ir.NodeRepeat:
		return l.lowerLoop(n, parent)

	case ir.NodeRun:
		return l.lowerRun(n, parent)

	case ir.NodeGuard:
		return l.lowerGuard(n, parent)

	case ir.NodeDispatch:
		return l.lowerDispatch(n, parent)

	case ir.NodeCleanup:
		return l.lowerCleanup(n, parent)

	case ir.NodeRecover:
		return l.lowerRecover(n, parent)

	case ir.NodeTimeout:
		return l.lowerTimeout(n, parent)

	default:
		// P4.2-deferred: async, await, cancel, channel, close, fail-channel, map,
		// quote, raise. Vocabulary + reducer transitions exist (total fold); executor
		// arms land in later slices. Filed follow-up: blueprint §7 P4.2 corpus TODO.
		// (retry/repeat land as the L5 attempt-loop arm above; run as the R lowerRun
		// arm; guard/dispatch/scatter/gather/for-each/cleanup/recover/timeout land as
		// their own arms above.)
		return fmt.Errorf("%w: %q (node %q)", ErrUnsupportedNode, n.Kind, n.ID)
	}
}

// lowerScatter lowers a scatter node. Only form "members" is implemented;
// form "each" (for-each over a runtime array) is deferred.
func (l *lowerer) lowerScatter(n ir.Node, parent string) error {
	var form string
	if raw, ok := n.Raw["form"]; ok {
		_ = json.Unmarshal(raw, &form)
	}
	if form == "each" {
		return l.lowerForEach(n, parent)
	}
	if form != "members" {
		return fmt.Errorf("%w: %q form %q (node %q)", ErrUnsupportedNode, n.Kind, form, n.ID)
	}
	members, err := childNodes(n.Raw["members"])
	if err != nil {
		return fmt.Errorf("lumen: scatter %q members: %w", n.ID, err)
	}
	scatterAct := activationFor(l.qid(n.ID))
	firstUnit := len(l.units)
	var memberActs []string
	savedAgg := l.inAggregate
	l.inAggregate = true // members are under an aggregate: fences a for-each/cleanup/recover member (a run/loop member is allowed)
	for i := range members {
		idx := i
		if err := l.lowerNode(members[i], scatterAct, &idx); err != nil {
			l.inAggregate = savedAgg
			return err
		}
	}
	l.inAggregate = savedAgg
	// Direct members only (L1): a member unit is one lowered directly under this
	// scatter (parent == scatterAct). A nested scatter/block contributes its own
	// aggregate/children as inner units parented elsewhere — those must NOT inflate
	// the outer member set. Silent (lit/interp) members never settle, so they are
	// excluded from the drained/aggregated set too.
	for i := firstUnit; i < len(l.units); i++ {
		u := &l.units[i]
		if u.parent == scatterAct && !u.silent {
			memberActs = append(memberActs, u.activation)
		}
	}
	l.scatterMembers[l.qid(n.ID)] = memberActs

	// Propagate the scatter's own `after` gate onto every unit lowered beneath it
	// (H1): a scatter gated on a failed dependency must not run any member. The
	// aggregate itself is gated separately below (its afterDeps). Descendants
	// inherit the gate so a nested scatter's leaves skip-cascade too.
	if len(n.After) > 0 {
		qAfter := l.qAfter(n.After)
		for i := firstUnit; i < len(l.units); i++ {
			l.units[i].rawAfter = append(l.units[i].rawAfter, qAfter...)
		}
	}

	onFail := "continue"
	if raw, ok := n.Raw["on_fail"]; ok {
		_ = json.Unmarshal(raw, &onFail)
	}
	l.units = append(l.units, planUnit{
		kind:       unitScatterAgg,
		activation: scatterAct,
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeScatter,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		members:    memberActs,
		onFail:     onFail,
	})
	return nil
}

// lowerForEach lowers a scatter(form:each) node — a dynamic fan over a runtime
// array — to a SINGLE unitForEach aggregate, at the root OR inside a run sub-formula's
// namespace (qid/ns qualified). The member count is the evaluated `over` array length
// (unknown here), so NO member units are lowered; the driver materializes one member per
// element at run time under a `<forEachID>/<index>` node id. That runtime id bypasses the
// authored-id `/`-ban (lowerNode); it can never collide with a static node's activation
// because the qualified for-each id occupies the `<ns>fanout` name in its namespace, which
// precludes a run `fanout` that would create the `<ns>fanout/` sub-namespace a colliding
// `<ns>fanout/<index>` authored id needs (the '/'-delimiter argument replaces the former
// top-level-only rationale). The single body member is either a leaf (exec/do) OR a `run`
// sub-formula call (the FBR arm, lowerForEachRunBody — each element mints the target's whole
// sub-graph under `<forEachID>/<index>/`). There is NO fan-size cap (leaf parity — the
// element count is the runtime array length). A multi-member or other non-leaf body, a
// missing binder/over, an unsupported `over` shape, or a delimiter-bearing binder/over ref
// refuses at LOAD.
func (l *lowerer) lowerForEach(n ir.Node, parent string) error {
	if l.inAggregate {
		// A for-each nested as a scatter member / gather combine is deferred this slice
		// (like lowerRun): the aggregate-member drive path for a dynamic fan is unbuilt.
		// (A run sub-formula placement IS supported — the qualified-id + '/'-delimiter
		// argument above keeps a dynamic member id collision-free there; only an aggregate
		// MEMBER stays fenced.)
		return fmt.Errorf("%w: for-each %q nested in an aggregate", ErrUnsupportedNode, n.ID)
	}
	binder := ""
	if raw, ok := n.Raw["binder"]; ok {
		_ = json.Unmarshal(raw, &binder)
	}
	if binder == "" {
		return fmt.Errorf("%w: for-each %q missing binder", ErrUnsupportedNode, n.ID)
	}
	if strings.ContainsAny(binder, "/:") {
		// The binder is bound into the render scope at the ns-qualified key u.ns+binder,
		// which the member's scopeFor(ns) view re-keys to the bare binder name via
		// directChildKey. A '/' or ':' would let that key collide with a member node-id
		// scope key (forEachID/<index>), an activation key, or a deeper-namespace child —
		// forging a cross-namespace binding.
		return fmt.Errorf("%w: for-each %q binder %q must not contain '/' or ':'", ErrUnsupportedNode, n.ID, binder)
	}
	over, ok := n.Raw["over"]
	if !ok || len(over) == 0 {
		return fmt.Errorf("%w: for-each %q missing over", ErrUnsupportedNode, n.ID)
	}
	if reason := forEachOverReason(over); reason != "" {
		return fmt.Errorf("%w: for-each %q over: %s", ErrUnsupportedNode, n.ID, reason)
	}
	// ⚑S1: derive the DET-gate over-refs from the over expression HEAD only (a bare ref
	// contributes its own name; an input.<field> member reads the immutable input layer,
	// contributing NO node ref). collectRefs over a member form would return the base ref
	// "input" — a spurious gate / silent-over / synth-ban footprint on a node literally
	// named "input" that the member arm never reads. Deriving from the head aligns the
	// gate, the silent-over sweep, the charset ban, and the synth-body ban with the arm
	// evalForEachArray actually reads.
	overRefs := forEachOverRefs(over)
	for _, ref := range overRefs {
		if strings.ContainsAny(ref, "/:") {
			return fmt.Errorf("%w: for-each %q over ref %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, ref)
		}
	}
	body, err := forEachBodyLeaf(n)
	if err != nil {
		return err
	}
	onFail := "continue"
	if raw, ok := n.Raw["on_fail"]; ok {
		_ = json.Unmarshal(raw, &onFail)
	}
	spec := &forEachSpec{
		overRaw:    over,
		overRefs:   overRefs,
		binder:     binder,
		bodyIRKind: body.Kind,
		onFail:     onFail,
	}
	switch body.Kind {
	case ir.NodeExec:
		s, err := decodeExec(body, false)
		if err != nil {
			return err
		}
		spec.body = s
	case ir.NodeDo:
		if !l.allowDo {
			return fmt.Errorf("%w: %q (for-each %q body)", ErrUnsupportedNode, body.Kind, n.ID)
		}
		s, err := decodeDo(body)
		if err != nil {
			return err
		}
		spec.body = s
	case ir.NodeRun:
		// A run body (fan { run <formula> given {…} }, the FBR slice): each element mints
		// the target sub-formula fresh under `<fanID>/<index>/` (mintRunBody, the shared RBL
		// helper), and the member aggregate settles transparently at
		// activationFor(<fanID>/<index>). decodeRunNode + the ⚑S4 dry-run mint validate the
		// body HERE at lowering, before any effect. It is fanned like a leaf member (no
		// aggregate placement — the l.inAggregate fence above still refuses a fan-in-scatter).
		if err := l.lowerForEachRunBody(n, body, spec); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: for-each %q body member kind %q (only a single exec/do leaf or a run body)", ErrUnsupportedNode, n.ID, body.Kind)
	}
	l.units = append(l.units, planUnit{
		kind:       unitForEach,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeScatter, // the IR node kind is scatter (form:each) — an aggregate
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		onFail:     onFail,
		forEach:    spec,
	})
	return nil
}

// lowerForEachRunBody decodes a for-each's run-body member (fan { run <formula> given {…} },
// the FBR slice) into spec: it validates the run node via decodeRunNode (inheriting the
// with-agent / runInput / non-transparent / missing-target / recursive-cycle / env-charset
// refusals — the same single path lowerRun and the repeat run-body arm use), stashes the
// re-lowering context, and DRY-RUN mints member 0's sub-graph (⚑S4) so an un-lowerable body
// — a nested loop refused at the member prefix, an unsupported sub-node — refuses at
// buildUnits before EnqueueRun seeds run.started. The refusal is wrapped `for-each %q run
// body does not lower: %w`, which composes UNDER a repeat run-body wrap at depth (a fan
// inside a repeat run body). Validation is member-invariant (the index enters only via the
// prefix string), so validating member 0 validates every element.
func (l *lowerer) lowerForEachRunBody(n, body ir.Node, spec *forEachSpec) error {
	runSpec, sub, err := l.decodeRunNode(body)
	if err != nil {
		return err
	}
	spec.bodyRun = runSpec
	spec.bodyFormula = sub
	spec.bodyFormulas = l.formulas
	spec.bodyTargetStack = append([]string(nil), l.targetStack...)
	spec.bodyAllowDo = l.allowDo
	spec.bodyAllowCombineDo = l.allowCombineDo
	member0 := forEachMemberNodeID(l.qid(n.ID), 0)
	zero := 0
	if _, _, err := mintRunBody(spec.runBodyStash, spec.bodyRun, member0, member0+"/", activationFor(member0),
		activationFor(l.qid(n.ID)), l.prefix, nil, nil, &zero); err != nil {
		return fmt.Errorf("lumen: for-each %q run body does not lower: %w", n.ID, err)
	}
	return nil
}

// forEachOverRefs returns the DET-gate ref set for a for-each `over` expression, derived
// from the HEAD: a bare `ref` contributes its own name (the node/input it reads); an
// `input.<field>` member reads the immutable input layer, so it contributes NO ref. This
// mirrors exactly what evalForEachArray's ref/member arms read — unlike collectRefs, which
// descends into a member form and returns the literal base ref "input" (⚑S1). A malformed
// head contributes nothing (forEachOverReason has already refused it).
func forEachOverRefs(raw json.RawMessage) []string {
	var head struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return nil
	}
	if head.Kind == "ref" && head.Name != "" {
		return []string{head.Name}
	}
	return nil
}

// forEachOverReason returns "" if the `over` expression is in the supported subset
// (a bare `ref` — a node output / flattened input holding a JSON array — or an
// `input.<field>` member, the conformance-golden form), else a human reason the
// caller wraps in ErrUnsupportedNode. Every other shape (operator, call, member on a
// non-input base, …) is an unsupported-node load error. It returns a reason string
// (not an error) so the caller wraps only the sentinel.
func forEachOverReason(raw json.RawMessage) string {
	var head struct {
		Kind string          `json:"kind"`
		Base json.RawMessage `json:"base"`
	}
	if err := json.Unmarshal(raw, &head); err != nil {
		return "malformed over expression"
	}
	switch head.Kind {
	case "ref":
		return ""
	case "member":
		var base struct {
			Kind string `json:"kind"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(head.Base, &base); err != nil {
			return "malformed member base"
		}
		if base.Kind != "ref" || base.Name != "input" {
			return fmt.Sprintf("member base must be the input record (got kind %q name %q)", base.Kind, base.Name)
		}
		return ""
	default:
		return fmt.Sprintf("kind %q (only a bare ref or an input.<field> member)", head.Kind)
	}
}

// forEachBodyLeaf unwraps a for-each `body` block and returns its SINGLE member (a leaf
// exec/do OR a run node — the caller's switch classifies the kind). A body that is not a
// block, or a block that does not hold exactly one member, is a load error — the
// multi-statement / nested-aggregate body is deferred this slice.
func forEachBodyLeaf(n ir.Node) (ir.Node, error) {
	raw, ok := n.Raw["body"]
	if !ok {
		return ir.Node{}, fmt.Errorf("%w: for-each %q missing body", ErrUnsupportedNode, n.ID)
	}
	var body ir.Node
	if err := json.Unmarshal(raw, &body); err != nil {
		return ir.Node{}, fmt.Errorf("lumen: for-each %q body: %w", n.ID, err)
	}
	if body.Kind != ir.NodeBlock {
		return ir.Node{}, fmt.Errorf("%w: for-each %q body must be a block", ErrUnsupportedNode, n.ID)
	}
	members, err := childNodes(body.Raw["members"])
	if err != nil {
		return ir.Node{}, fmt.Errorf("lumen: for-each %q body members: %w", n.ID, err)
	}
	if len(members) != 1 {
		return ir.Node{}, fmt.Errorf("%w: for-each %q body has %d members (only a single exec/do leaf or run body)", ErrUnsupportedNode, n.ID, len(members))
	}
	return members[0], nil
}

// lowerCleanup lowers a cleanup(try/finally) node: a `guarded` leaf that runs, then a
// `body` (finally) leaf that runs ALWAYS afterward. Both are single leaves; the driver
// sequences them and the cleanup settles from the guarded UNLESS the body itself fails.
// Only a top-level cleanup with single exec/do/settle guarded+body is supported this
// slice; a sub-formula/aggregate placement, a non-leaf guarded/body, a missing/gated
// sub, or a delimiter-bearing sub id refuses at LOAD. (A guarded/body id colliding with
// each other, the cleanup, or another node is refused in resolveDeps' synth-id guard.)
func (l *lowerer) lowerCleanup(n ir.Node, parent string) error {
	if l.prefix != "" {
		return fmt.Errorf("%w: cleanup %q in a sub-formula", ErrUnsupportedNode, n.ID)
	}
	if l.inAggregate {
		return fmt.Errorf("%w: cleanup %q nested in an aggregate", ErrUnsupportedNode, n.ID)
	}
	guardedRaw, ok := n.Raw["guarded"]
	if !ok {
		return fmt.Errorf("%w: cleanup %q missing guarded", ErrUnsupportedNode, n.ID)
	}
	bodyRaw, ok := n.Raw["body"]
	if !ok {
		return fmt.Errorf("%w: cleanup %q missing body", ErrUnsupportedNode, n.ID)
	}
	var guarded, body ir.Node
	if err := json.Unmarshal(guardedRaw, &guarded); err != nil {
		return fmt.Errorf("lumen: cleanup %q guarded: %w", n.ID, err)
	}
	if err := json.Unmarshal(bodyRaw, &body); err != nil {
		return fmt.Errorf("lumen: cleanup %q body: %w", n.ID, err)
	}
	// A block guarded (a multi-step chain) lowers its leaves as ordinary BARE-ID units
	// under a synthetic transparent drain aggregate — branch BEFORE decodeLeafSub, which
	// only decodes a single leaf. The single-leaf guarded form below is unchanged.
	if guarded.Kind == ir.NodeBlock {
		return l.lowerCleanupBlock(n, parent, guarded, body)
	}
	gStep, gKind, err := decodeLeafSub(guarded, l.allowDo, "cleanup "+n.ID+" guarded")
	if err != nil {
		return err
	}
	bStep, bKind, err := decodeLeafSub(body, l.allowDo, "cleanup "+n.ID+" body")
	if err != nil {
		return err
	}
	l.units = append(l.units, planUnit{
		kind:       unitCleanup,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeCleanup,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		cleanup: &cleanupSpec{
			guardedNodeID: l.qid(guarded.ID),
			guardedIRKind: gKind,
			guarded:       gStep,
			bodyNodeID:    l.qid(body.ID),
			bodyIRKind:    bKind,
			body:          bStep,
		},
	})
	return nil
}

// lowerCleanupBlock lowers a cleanup whose `guarded` is a BLOCK: the block's leaf
// children are inlined as ordinary BARE-ID plan units (ns "", parent = the synthetic
// aggregate) so they render in the parent's flat scope — a guarded block is authored IN
// the parent formula's scope, so namespacing the members would blind the finally and
// downstream to their outputs and render their {{ref}}s "" (⚑BLOCKER-1). A transparent
// drain aggregate (unitCleanupGuarded, activation <cleanupID>/__guarded, irKind block)
// collects the leaves with the run's worst-of/last-ran rule, and the cleanup drains it
// via memberDeps — a DRAIN edge, not a blocking gate, so a FAILED block never
// skip-cascades the always-run finally. The finally (body) stays a single imperatively
// driven leaf, decoded via decodeLeafSub. Refusals: an empty block, a non-leaf member
// (message names the kind, ⚑NICE-9), a lit/interp member, and a guarded block carrying
// its own `after` gate (⚑SHOULD-FIX-3 — silently discarding it would be worse than the
// leaf path's loud refusal). Id collisions (dup members, member↔top-level-node,
// member↔finally) are caught downstream by topoSortUnits' dup-activation guard and the
// addSynth guard.
func (l *lowerer) lowerCleanupBlock(n ir.Node, parent string, guarded, body ir.Node) error {
	// ⚑SHOULD-FIX-3: the leaf path refuses a sub `after`; a block gate would be silently
	// dropped (the driver sequences the block by its members' own edges), so refuse loudly.
	if len(guarded.After) > 0 {
		return fmt.Errorf("%w: cleanup %q guarded block %q must not carry an `after` gate", ErrUnsupportedNode, n.ID, guarded.ID)
	}
	members, err := childNodes(guarded.Raw["members"])
	if err != nil {
		return fmt.Errorf("lumen: cleanup %q guarded block: %w", n.ID, err)
	}
	if len(members) == 0 {
		return fmt.Errorf("%w: cleanup %q guarded block has no members", ErrUnsupportedNode, n.ID)
	}
	// Leaf-kind + id check FIRST (before lowerNodes) so a non-leaf member refuses with the
	// cleanup-specific message naming the kind (⚑NICE-9), not lowerNode's generic one. A
	// lit/interp member is refused too — narrower than a scatter's silent members, since
	// this slice's block guarded is exec/do/settle-only. An EMPTY member id is refused
	// here as well: it would slip past lowerNode's '/'-and-':' ban (ContainsAny on "" is
	// false) and lower to the anonymous activation ":0".
	for i := range members {
		if members[i].ID == "" {
			return fmt.Errorf("%w: cleanup %q guarded block member %d missing id", ErrUnsupportedNode, n.ID, i)
		}
		switch members[i].Kind {
		case ir.NodeExec, ir.NodeSettle, ir.NodeDo:
		default:
			return fmt.Errorf("%w: cleanup %q guarded block member %q kind %q (only exec/do/settle leaf)", ErrUnsupportedNode, n.ID, members[i].ID, members[i].Kind)
		}
	}

	cleanupAct := activationFor(l.qid(n.ID))
	guardedAggID := l.qid(n.ID) + "/__guarded"
	guardedAggAct := activationFor(guardedAggID)

	// Inline the members at BARE ids: NO prefix push (l.prefix stays ""), so intra-block
	// `after` refs — including refs to an OUTSIDE top-level node — resolve in the flat
	// namespace, matching plain block hoisting. lowerNode applies the '/'-and-':' authored
	// id ban and (for do) the allowDo gate; a member leaf-kind escaping the pre-check
	// above cannot occur.
	firstUnit := len(l.units)
	if err := l.lowerNodes(members, guardedAggAct); err != nil {
		return err
	}

	// H1 gate propagation: append the CLEANUP's own `after` gate onto every inlined
	// member's rawAfter, so a gated-off cleanup skip-cascades the whole block. Then collect
	// the direct non-silent members (all exec/do/settle leaves) in source order.
	gate := l.qAfter(n.After)
	var memberActs []string
	for i := firstUnit; i < len(l.units); i++ {
		u := &l.units[i]
		if len(gate) > 0 {
			u.rawAfter = append(u.rawAfter, gate...)
		}
		if u.parent == guardedAggAct && !u.silent {
			memberActs = append(memberActs, u.activation)
		}
	}

	// The finally stays a single imperatively driven leaf (block finally deferred —
	// decodeLeafSub's kind refusal covers it).
	bStep, bKind, err := decodeLeafSub(body, l.allowDo, "cleanup "+n.ID+" body")
	if err != nil {
		return err
	}

	// The transparent drain aggregate. ⚑SHOULD-FIX-4: it also carries the cleanup's gate
	// (like lowerRun), so a gate-failed path settles it via blocked() with the standard
	// "upstream dependency did not pass" detail and the Tier-A DAG keeps the gate edge.
	l.units = append(l.units, planUnit{
		kind:       unitCleanupGuarded,
		activation: guardedAggAct,
		nodeID:     guardedAggID,
		irKind:     ir.NodeBlock,
		parent:     cleanupAct,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		members:    memberActs,
	})
	l.units = append(l.units, planUnit{
		kind:       unitCleanup,
		activation: cleanupAct,
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeCleanup,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		cleanup: &cleanupSpec{
			guardedAgg: guardedAggAct,
			bodyNodeID: l.qid(body.ID),
			bodyIRKind: bKind,
			body:       bStep,
		},
	})
	return nil
}

// decodeLeafSub decodes a single-leaf sub-node (a cleanup guarded/body) into a step +
// its IR kind. Only exec/do/settle leaves are supported; a block/aggregate/loop/run/
// nested-decision sub refuses. The sub id must be non-empty and delimiter-free (it
// becomes the sub-unit node id / activation), and a non-empty inner `after` refuses
// loudly — the sub runs in the cleanup's fixed sequence, not on an authored gate that
// would be silently discarded.
func decodeLeafSub(n ir.Node, allowDo bool, owner string) (step, ir.NodeKind, error) {
	if n.ID == "" {
		return step{}, "", fmt.Errorf("%w: %s missing id", ErrUnsupportedNode, owner)
	}
	if strings.ContainsAny(n.ID, "/:") {
		return step{}, "", fmt.Errorf("%w: %s id %q must not contain '/' or ':'", ErrUnsupportedNode, owner, n.ID)
	}
	if len(n.After) > 0 {
		return step{}, "", fmt.Errorf("%w: %s %q must not carry an `after` gate", ErrUnsupportedNode, owner, n.ID)
	}
	switch n.Kind {
	case ir.NodeExec:
		s, err := decodeExec(n, false)
		return s, n.Kind, err
	case ir.NodeSettle:
		return decodeSettle(n), n.Kind, nil
	case ir.NodeDo:
		if !allowDo {
			return step{}, "", fmt.Errorf("%w: %s do %q needs a host/pool", ErrUnsupportedNode, owner, n.ID)
		}
		s, err := decodeDo(n)
		return s, n.Kind, err
	default:
		return step{}, "", fmt.Errorf("%w: %s %q kind %q (only exec/do/settle leaf)", ErrUnsupportedNode, owner, n.ID, n.Kind)
	}
}

// lowerRecover lowers a recover(try/catch) node: a `guarded` leaf that runs, and a
// `body` (catch) leaf that runs ONLY when the guarded FAILED, with the error bound under
// `errorBinding`. Structurally the twin of lowerCleanup (single exec/do/settle subs, same
// refusals + collision guard); adds the errorBinding (default "error"), refused if it
// carries a `.`/`/`/`:` (it flat-keys `<errorBinding>.reason`).
func (l *lowerer) lowerRecover(n ir.Node, parent string) error {
	if l.prefix != "" {
		return fmt.Errorf("%w: recover %q in a sub-formula", ErrUnsupportedNode, n.ID)
	}
	if l.inAggregate {
		return fmt.Errorf("%w: recover %q nested in an aggregate", ErrUnsupportedNode, n.ID)
	}
	guardedRaw, ok := n.Raw["guarded"]
	if !ok {
		return fmt.Errorf("%w: recover %q missing guarded", ErrUnsupportedNode, n.ID)
	}
	bodyRaw, ok := n.Raw["body"]
	if !ok {
		return fmt.Errorf("%w: recover %q missing body", ErrUnsupportedNode, n.ID)
	}
	var guarded, body ir.Node
	if err := json.Unmarshal(guardedRaw, &guarded); err != nil {
		return fmt.Errorf("lumen: recover %q guarded: %w", n.ID, err)
	}
	if err := json.Unmarshal(bodyRaw, &body); err != nil {
		return fmt.Errorf("lumen: recover %q body: %w", n.ID, err)
	}
	gStep, gKind, err := decodeLeafSub(guarded, l.allowDo, "recover "+n.ID+" guarded")
	if err != nil {
		return err
	}
	bStep, bKind, err := decodeLeafSub(body, l.allowDo, "recover "+n.ID+" body")
	if err != nil {
		return err
	}
	errorBinding := "error"
	if raw, ok := n.Raw["errorBinding"]; ok {
		var eb string
		if err := json.Unmarshal(raw, &eb); err != nil {
			return fmt.Errorf("lumen: recover %q errorBinding: %w", n.ID, err)
		}
		if eb != "" {
			errorBinding = eb
		}
	}
	if strings.ContainsAny(errorBinding, "./:") {
		return fmt.Errorf("%w: recover %q errorBinding %q must not contain '.', '/', or ':'", ErrUnsupportedNode, n.ID, errorBinding)
	}
	l.units = append(l.units, planUnit{
		kind:       unitRecover,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeRecover,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		recover: &recoverSpec{
			guardedNodeID: l.qid(guarded.ID),
			guardedIRKind: gKind,
			guarded:       gStep,
			bodyNodeID:    l.qid(body.ID),
			bodyIRKind:    bKind,
			body:          bStep,
			errorBinding:  errorBinding,
		},
	})
	return nil
}

// lowerGather lowers a gather(authored) node: the head-of-line drain over the
// referenced scatter's members plus the authored combine block.
func (l *lowerer) lowerGather(n ir.Node, parent string) error {
	overName, err := gatherOverName(n)
	if err != nil {
		return err
	}
	// The `over` ref is a sibling scatter's node id in the SAME namespace; qualify
	// it to look up the (qualified) scatter members, so a bare-name `over` never
	// resolves across a run boundary.
	qOver := l.qid(overName)
	memberActs, ok := l.scatterMembers[qOver]
	if !ok {
		return fmt.Errorf("lumen: gather %q references unknown scatter %q", n.ID, overName)
	}
	combine, err := l.lowerCombine(n)
	if err != nil {
		return err
	}
	l.units = append(l.units, planUnit{
		kind:          unitGather,
		activation:    activationFor(l.qid(n.ID)),
		nodeID:        l.qid(n.ID),
		irKind:        ir.NodeGather,
		parent:        parent,
		ns:            l.prefix,
		rawAfter:      l.qAfter(n.After),
		overScatter:   activationFor(qOver),
		gatherMembers: memberActs,
		combine:       combine,
	})
	return nil
}

// lowerLoop lowers a retry/repeat node to a unitLoop planUnit (§3.2). It is the
// ONLY place a retry/repeat's shape is validated: the body must be a leaf exec/do
// — or, for a REPEAT only, a `run` (the RBL arm: the spec is stashed and the
// sub-graph minted per attempt; retry+run is refused, ⚑S2) — while block / nested
// loop / scatter bodies are refused HERE, before any append; the closed exit
// expression (retry: `attempts`; repeat: `cond`) is validated by walking its tree,
// so a bad expression refuses at LOAD, never at attempt N.
//
// The label names the BODY (compiled evidence: repeat/retry give the loop node a
// synthetic id and the body keeps its own id), and dependents gate on the LOOP
// node id (the compiler rewrites body-binding `after` refs onto the loop node), so
// resolveDeps registers only the loop unit. A raw `after: [bodyID]` in hand-crafted
// IR is a dangling ref, refused loudly (the compiler never emits it).
func (l *lowerer) lowerLoop(n ir.Node, parent string) error {
	// A loop may be a SCATTER MEMBER at top level (RN slice) OR inlined inside a run
	// sub-formula (LIS slice — its decision scope is now namespace-aware, loopScopeNS).
	// A LEAF-body loop as a scatter member inside a sub-formula falls out generically
	// (Q-A). Still refused below: a run-body loop as a scatter member (l.inAggregate —
	// the re-mint arm is an entry-top-level surface), a loop as a gather-COMBINE member
	// (lowerCombine's leaf-only check), and a loop whose BODY is not a leaf exec/do or
	// (repeat-only) a run — the body switch refuses block / nested-loop / scatter bodies,
	// and the retry arm refuses retry+run (⚑S2).
	spec := &loopSpec{irKind: n.Kind}

	bodyRaw, ok := n.Raw["body"]
	if !ok {
		return fmt.Errorf("%w: %q %q missing body", ErrUnsupportedNode, n.Kind, n.ID)
	}
	var body ir.Node
	if err := json.Unmarshal(bodyRaw, &body); err != nil {
		return fmt.Errorf("lumen: %s %q body: %w", n.Kind, n.ID, err)
	}
	switch body.Kind {
	case ir.NodeExec:
		// allowPending only on a REPEAT leaf body: a retry body settles a pending inert
		// (the retry arm branches on != OutcomeFailed), so exitMap.pending is refused there.
		s, err := decodeExec(body, n.Kind == ir.NodeRepeat)
		if err != nil {
			return err
		}
		spec.body = s
	case ir.NodeDo:
		if !l.allowDo {
			// A do body obeys the same gate as a top-level do: it needs a Host
			// (Run/Resume) or a PoolRouter (Advance). Refuse it before any append.
			return fmt.Errorf("%w: %q body %q (node %q)", ErrUnsupportedNode, n.Kind, body.Kind, body.ID)
		}
		s, err := decodeDo(body)
		if err != nil {
			return err
		}
		spec.body = s
	case ir.NodeRun:
		// A run body (repeat { run <formula> given {…} }, the RBL slice): each attempt
		// mints the sub-formula fresh under `<bodyNodeID>/<N>/`. Refuse a run-body loop
		// as a scatter member (l.inAggregate) — the driver's re-mint arm is an
		// entry-top-level surface this slice (the corpus loops are entry-top-level), and
		// an aggregate placement is untested. decodeRunNode does the same validation
		// lowerRun does (transparent, target present, cycle, env), so a bad run body
		// refuses HERE at lowering, before any unit is emitted.
		if l.inAggregate {
			return fmt.Errorf("%w: %q %q with a run body is not a legal scatter member this slice (loops with run bodies are entry-top-level)", ErrUnsupportedNode, n.Kind, n.ID)
		}
		runSpec, sub, err := l.decodeRunNode(body)
		if err != nil {
			return err
		}
		spec.bodyRun = runSpec
		spec.bodyFormula = sub
		spec.bodyFormulas = l.formulas
		spec.bodyTargetStack = append([]string(nil), l.targetStack...)
		spec.bodyAllowDo = l.allowDo
		spec.bodyAllowCombineDo = l.allowCombineDo
	default:
		return fmt.Errorf("%w: %q %q body kind %q (only exec/do leaf or run bodies)", ErrUnsupportedNode, n.Kind, n.ID, body.Kind)
	}
	if body.ID == "" {
		return fmt.Errorf("%w: %q %q body missing id", ErrUnsupportedNode, n.Kind, n.ID)
	}
	spec.bodyNodeID = l.qid(body.ID)
	// ⚑B2: the AUTHORED bare id is what the cond/attempts refs and env refs name (all
	// bare — collectRefs strips no namespace), so every bare-name comparison keys on it.
	// At the root the prefix is "" so bodyBareID == bodyNodeID (byte-identical).
	spec.bodyBareID = body.ID
	spec.bodyIRKind = body.Kind

	switch n.Kind {
	case ir.NodeRetry:
		// ⚑S2: a retry whose body is a run can NEVER re-attempt — settleTransparentAgg
		// always settles the aggregate non-retryable, so loopDecide's retry arm stops
		// on attempt 0. Lifting it needs an aggregate-retryability rule (real design
		// work); the corpus is all repeat. Refuse it loudly rather than silently emit a
		// one-shot retry.
		if spec.bodyRun != nil {
			return fmt.Errorf("%w: retry %q with a run body cannot re-attempt (a transparent aggregate is never retryable); use repeat", ErrUnsupportedNode, n.ID)
		}
		att, ok := n.Raw["attempts"]
		if !ok {
			return fmt.Errorf("%w: retry %q missing attempts", ErrUnsupportedNode, n.ID)
		}
		if err := validateClosedExpr(att); err != nil {
			return err
		}
		spec.attemptsExpr = att
		spec.attemptsRefs = collectRefs(att)
	case ir.NodeRepeat:
		cond, ok := n.Raw["cond"]
		if !ok {
			return fmt.Errorf("%w: repeat %q missing cond", ErrUnsupportedNode, n.ID)
		}
		if err := validateClosedExpr(cond); err != nil {
			return err
		}
		spec.condExpr = cond
		spec.condRefs = collectRefs(cond)
		iterName := "iteration"
		if raw, ok := n.Raw["iterationName"]; ok {
			_ = json.Unmarshal(raw, &iterName)
		}
		if iterName == "" {
			iterName = "iteration"
		}
		// The for-each binder / recover errorBinding charset parity: a '/'- or
		// ':'-bearing iterationName forges a namespaced/activation scope key — the
		// qualified seed (u.ns+iterationName) would land in the wrong namespace,
		// silently rendering "" at depth while appearing to work at root.
		// Compiler-emitted IR never does this; refuse hand-crafted IR loudly.
		if strings.ContainsAny(iterName, "/:") {
			return fmt.Errorf("%w: repeat %q iterationName %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, iterName)
		}
		spec.iterationName = iterName
	}

	// Charset sweep (lowerGuard parity): a cond/attempts ref carrying '/' or ':' is a
	// forged cross-namespace flat key (idents carry neither delimiter) — it would gate
	// via byNodeID AND resolve the flat nodeOutputs qualified key, so it must refuse at
	// ALL levels (root and inside a sub-formula). The ErrUnsupportedNode wrap is
	// load-bearing (enqueue-gate triage).
	for _, r := range append(append([]string(nil), spec.condRefs...), spec.attemptsRefs...) {
		if strings.ContainsAny(r, "/:") {
			return fmt.Errorf("%w: %q %q cond/attempts ref %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.Kind, n.ID, r)
		}
	}

	if spec.bodyRun != nil {
		// ⚑S5: an env binding that reads the repeat's iteration is refused. The
		// iteration binding lives in the PARENT (loop) scope only inside the drive
		// window; a memoized re-render of an in-flight sub-do sees the FOLDED prompt,
		// not the live scope, so a `given { x: iteration }` binding would render its
		// value on first materialization and "" on any re-render — a silent,
		// path-dependent corruption. Refuse it at load. The body's OWN id is refused
		// in the same sweep: byNodeID misses the spec-only body id (no gate installs),
		// so `given { x: <bodyID> }` would render "" on attempt 0 but attempt N-1's
		// transparent output on attempt N ≥ 1 (the bare scope[bodyID] last-wins key) —
		// a silently attempt-VARYING mint that voids the byte-identical-mint premise.
		for _, ref := range spec.bodyRun.envRefs {
			if ref == spec.iterationName {
				return fmt.Errorf("%w: repeat %q run body binds the iteration counter %q into the sub-formula environment (path-dependent render); reference it in the sub-formula prompt instead", ErrUnsupportedNode, n.ID, spec.iterationName)
			}
			// ⚑B2: compare against the BARE body id — envRefs are bare, so a qualified
			// bodyNodeID would never match and the ban would silently STOP firing at depth.
			if ref == spec.bodyBareID {
				return fmt.Errorf("%w: repeat %q run body binds its own body id %q into the sub-formula environment (attempt-varying render: \"\" on attempt 0, the prior attempt's output after)", ErrUnsupportedNode, n.ID, spec.bodyBareID)
			}
		}
		// Re-decide freeze: a run-body repeat cond may read ONLY the body's bare id,
		// the iteration counter, and input fields (all tick-stable). advanceRunBodyLoop
		// cannot park on liveAttempt (the attempt aggregate activates LAST, ⚑S1), so it
		// re-runs loopDecide over the LAST SETTLED attempt on EVERY tick — including
		// ticks where attempt N+1 is already minted with a live dispatched bead. An
		// external node ref settling BETWEEN ticks would flip an already-acted-on
		// continue into a stale settleLoop over the orphaned live attempt (the seal path
		// never re-consults inFlightPoolWork). Freezing the ref set makes decide(N) a
		// pure function of (bn(N), N) — write-once per attempt by construction. Lifting
		// this needs a durable decision record (the guard write-once precedent) — a
		// follow-up design, not this slice.
		// ⚑B2: the allowlist keys on the BARE body id (condRefs are bare); l.inputNames
		// carries the CURRENT formula's inputs (the wrapper's, threaded by lowerRun's
		// save/restore — Q-D), so a run-body loop inside a sub-formula freezes against the
		// SUB-formula's declared inputs, not the main document's.
		for _, ref := range spec.condRefs {
			if ref == spec.bodyBareID || ref == spec.iterationName || l.inputNames[ref] {
				continue
			}
			return fmt.Errorf("%w: repeat %q run-body cond reads %q — a run-body repeat cond may read only the body outcome, the iteration counter, and inputs this slice (an external node ref makes the per-tick re-decide non-write-once)", ErrUnsupportedNode, n.ID, ref)
		}
		// ⚑S4 DRY-RUN mint: lower attempt 0's sub-graph now (the same helper the runtime
		// mint uses) so an un-lowerable run body — an unsupported sub-node, a nested loop
		// (refused at the attempt prefix), a cycle — refuses HERE at buildUnits, before
		// EnqueueRun ever seeds run.started. Attempt-invariant (N enters only via the
		// prefix string), so validating attempt 0 validates every attempt.
		if _, _, err := spec.mintRunBodyAttempt(0, activationFor(l.qid(n.ID)), l.prefix, nil, l.qAfter(n.After)); err != nil {
			return fmt.Errorf("lumen: repeat %q run body does not lower: %w", n.ID, err)
		}
	}

	l.units = append(l.units, planUnit{
		kind:       unitLoop,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     n.Kind,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		loop:       spec,
	})
	return nil
}

// decodeRunNode validates and decodes a run node's transparent, by-name shape into
// a runSpec plus the resolved target sub-formula. It is the SINGLE run-node
// validation path (⚑RBL §5), with FOUR callers so there is no drift between an inlined
// run and a re-materialized one: lowerRun (which then inlines the sub-graph),
// lowerLoop's repeat-run-body arm (stashes the spec for per-attempt minting),
// lowerForEachRunBody (per-element member minting), and lowerDispatchRunArm (per-arm
// minting when the arm is matched). It
// refuses the deferred with-agent / runInput / non-transparent forms, a target
// missing from the bundle, a recursive cycle (vs targetStack), and — via
// decodeRunEnv — an env binding for an undeclared field, an unbound required input,
// or a delimiter-bearing env ref. It reads l.formulas/l.targetStack (the current
// namespace context), so a cycle check is correct at any nesting depth.
func (l *lowerer) decodeRunNode(n ir.Node) (*runSpec, *ir.IR, error) {
	// Closed-payload guards: the agent override and detached-run forms are deferred;
	// only the transparent, by-name form lowers this slice.
	if _, ok := n.Raw["with"]; ok {
		return nil, nil, fmt.Errorf("%w: run %q with-agent override (node %q)", ErrUnsupportedNode, n.ID, n.ID)
	}
	if _, ok := n.Raw["runInput"]; ok {
		return nil, nil, fmt.Errorf("%w: run %q runInput form (node %q)", ErrUnsupportedNode, n.ID, n.ID)
	}
	var outcome string
	if raw, ok := n.Raw["outcome"]; ok {
		_ = json.Unmarshal(raw, &outcome)
	}
	if outcome != "transparent" {
		return nil, nil, fmt.Errorf("%w: run %q outcome %q (only transparent)", ErrUnsupportedNode, n.ID, outcome)
	}
	target, err := runTargetName(n)
	if err != nil {
		return nil, nil, err
	}
	sub, ok := l.formulas[target]
	if !ok {
		return nil, nil, fmt.Errorf("lumen: run %q targets formula %q not present in the document's formulas bundle", n.ID, target)
	}
	if sub == nil {
		return nil, nil, fmt.Errorf("lumen: run %q target formula %q is null in the bundle", n.ID, target)
	}
	for _, t := range l.targetStack {
		if t == target {
			return nil, nil, fmt.Errorf("lumen: run %q: recursive formula cycle %s", n.ID, strings.Join(append(append([]string(nil), l.targetStack...), target), " -> "))
		}
	}
	env, envRefs, err := decodeRunEnv(n, sub.Input.Fields)
	if err != nil {
		return nil, nil, err
	}
	return &runSpec{target: target, env: env, envRefs: envRefs, inputFields: sub.Input.Fields}, sub, nil
}

// lowerRun lowers a run node — a transparent call to another formula — by
// inlining the target formula's nodes as ordinary units under a `<runID>/`
// namespace and emitting a unitRun aggregate that settles with the sub-formula's
// transparent outcome. A run may be TOP-LEVEL, a scatter member (its aggregate
// parent = the scatterAct), or a sub-formula's own statement, at any run-nesting
// depth. It does NOT consult inAggregate: a "scoped" placement rule is not
// implementable here — inside lowerRun no signal distinguishes a scatter member
// (legal) from a gather-combine member (illegal), both arriving inAggregate=true,
// parent != "" (⚑SF-2). Run-in-combine stays fenced SOLELY by lowerCombine's
// leaf-only unit sweep, which rejects the unitRun agg after this inlines it. A run
// as a REPEAT body never reaches here — lowerLoop's run-body arm stashes the spec
// and mints the sub-graph per attempt (mintRunBodyAttempt); a retry run body is
// refused there (⚑S2). The target body comes from the document's flat `formulas`
// bundle; a missing target, a recursive cycle, an unknown env field, or an unbound
// required input all refuse in decodeRunNode, before any append.
func (l *lowerer) lowerRun(n ir.Node, parent string) error {
	spec, sub, err := l.decodeRunNode(n)
	if err != nil {
		return err
	}
	target := spec.target

	qRunID := l.qid(n.ID)
	qAfter := l.qAfter(n.After)
	runAct := activationFor(qRunID)
	nodeNS := l.prefix // the namespace the run node itself lives in (parent scope)

	// Inline the sub-graph one namespace deeper. The sub-formula's internal `after`
	// refs qualify under the new prefix, so they resolve strictly within it.
	savedPrefix, savedStack, savedInAgg, savedInputNames := l.prefix, l.targetStack, l.inAggregate, l.inputNames
	l.prefix = qRunID + "/"
	// Fresh backing array per push: appending to savedStack directly could alias the
	// same array across sibling runs (spare capacity), corrupting a later sibling's
	// cycle stack. Copy so each nesting level owns its slice.
	l.targetStack = append(append([]string(nil), savedStack...), target)
	// A scatter-member run enters with inAggregate=true, but the sub-formula's OWN
	// top-level statements are not aggregate members — reset it so a nested run/loop
	// (a legal top-of-sub statement) is not wrongly fenced by a bled-in true. Restored
	// on EVERY exit path, mirroring the prefix/targetStack save-restore (§1).
	l.inAggregate = false
	// Q-D: the inlined sub-graph's inputNames are the SUB-formula's declared inputs, so a
	// run-body loop lowered inside this namespace freezes its cond against the right input
	// set. Restored on every exit path alongside prefix/targetStack (lowerCombine already
	// threads l.inputNames into its sub-lowerer; a static run needs the same window).
	l.inputNames = fieldNameSet(sub.Input.Fields)
	firstUnit := len(l.units)
	if err := l.lowerNodes(sub.Nodes, runAct); err != nil {
		l.prefix, l.targetStack, l.inAggregate, l.inputNames = savedPrefix, savedStack, savedInAgg, savedInputNames
		return err
	}
	l.prefix, l.targetStack, l.inAggregate, l.inputNames = savedPrefix, savedStack, savedInAgg, savedInputNames

	// Direct members: units lowered directly under this run (parent == runAct),
	// non-silent — the same collection rule as lowerScatter, in source order.
	var members []string
	for i := firstUnit; i < len(l.units); i++ {
		u := &l.units[i]
		if u.parent == runAct && !u.silent {
			members = append(members, u.activation)
		}
	}

	// Propagate the run's own gate onto every sub-unit (lowerScatter H1 rule): a run
	// gated on a failed dep runs no sub-effect (each sub-unit skip-cascades; the
	// aggregate settles skipped via the blocked() intercept).
	if len(qAfter) > 0 {
		for i := firstUnit; i < len(l.units); i++ {
			l.units[i].rawAfter = append(l.units[i].rawAfter, qAfter...)
		}
	}

	l.units = append(l.units, planUnit{
		kind:       unitRun,
		activation: runAct,
		nodeID:     qRunID,
		irKind:     ir.NodeRun,
		parent:     parent,
		ns:         nodeNS,
		rawAfter:   qAfter,
		members:    members,
		run:        spec,
	})
	return nil
}

// lowerGuard lowers a guard node — a conditional single-step arm — to a unitGuard.
// The closed condition is validated at LOAD (a bad expr refuses here, never at run
// time). The `then` is a SINGLE leaf (exec/do); a non-leaf then (block/scatter/loop)
// is refused this slice. The then is NOT a separate unit — it is synthesized and run
// (or skipped) by the driver, so a false condition runs no side effect. A guard is
// top-level, a scatter member, or inlined in a run sub-formula (like a leaf): its cond
// and then are qualified by the lowerer prefix, so it renders/decides against the
// unit's namespace view (condScope keys `l.prefix`). A cond ref carrying a reserved
// delimiter is refused below — a '/'-ref both gates via byNodeID and resolves the flat
// nodeOutputs qualified key, forging a cross-namespace read.
func (l *lowerer) lowerGuard(n ir.Node, parent string) error {
	cond, ok := n.Raw["cond"]
	if !ok {
		return fmt.Errorf("%w: guard %q missing cond", ErrUnsupportedNode, n.ID)
	}
	if err := validateClosedExpr(cond); err != nil {
		return err
	}
	// Amended SLX §1.1.7: a call expr (length) in a guard cond is ROOT-only. Inside a run
	// sub-formula the cond evaluates through condScope's DELIBERATELY string-typed
	// child-wins view (GIS-pinned — retyping it would flip settled semantics), so
	// `length(<array binding>)` would count the UTF-16 units of the JSON render TEXT and
	// an empty bound array would go truthy. Refuse loudly at load; loop conds keep depth
	// call support (loopScopeNS is typed).
	if l.prefix != "" && exprContainsCall(cond) {
		return fmt.Errorf("%w: guard %q cond: call expressions unsupported in a sub-formula (string-typed decision scope)", ErrUnsupportedNode, n.ID)
	}
	thenRaw, ok := n.Raw["then"]
	if !ok {
		return fmt.Errorf("%w: guard %q missing then", ErrUnsupportedNode, n.ID)
	}
	var then ir.Node
	if err := json.Unmarshal(thenRaw, &then); err != nil {
		return fmt.Errorf("lumen: guard %q then: %w", n.ID, err)
	}
	if then.ID == "" {
		return fmt.Errorf("%w: guard %q then missing id", ErrUnsupportedNode, n.ID)
	}
	// An authored then bypasses lowerNode's id check (decoded inline here), so this is the
	// ban site (decodeLeafSub / timeout-body parity): a '/' or ':' in the then id would forge
	// a cross-namespace activation key (a forged `sib/x` aliases a sibling's minted sub-unit
	// activation). A non-empty then `after` is refused LOUDLY — the then is a single
	// synthesized leaf (activationFor(thenID), gates inherited from the wrapper), so a gate
	// slot on it would silently drop (decisionBodyUnit substitutes the wrapper's afterDeps).
	if strings.ContainsAny(then.ID, "/:") {
		return fmt.Errorf("%w: guard %q then id %q must not contain '/' or ':'", ErrUnsupportedNode, n.ID, then.ID)
	}
	if len(then.After) > 0 {
		return fmt.Errorf("%w: guard %q then %q must not carry an 'after' gate", ErrUnsupportedNode, n.ID, then.ID)
	}
	condRefs := collectRefs(cond)
	// Charset sweep FIRST, over ALL refs, then the self-ref sweep — a multi-defect
	// cond reports the malformed-ref class before the semantic one. A cond ref
	// carrying '/' or ':' is a forged cross-namespace key (idents carry neither
	// delimiter): it would gate via byNodeID AND resolve the flat nodeOutputs
	// qualified key, so it must refuse at ALL levels — the decodeRunEnv charset
	// parity. The ErrUnsupportedNode wrap is load-bearing (enqueue-gate triage).
	for _, r := range condRefs {
		if strings.ContainsAny(r, "/:") {
			return fmt.Errorf("%w: guard %q cond ref %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, r)
		}
	}
	// A cond that references the guard's own id or its `then` is self-referential —
	// the guard/then have not settled when the decision is made, so the ref folds to
	// null on genesis but could flip on a resume that reloaded the then. Refuse it at load.
	for _, r := range condRefs {
		if r == n.ID || r == then.ID {
			return fmt.Errorf("%w: guard %q cond references its own %s (self-referential decision)", ErrUnsupportedNode, n.ID, r)
		}
	}
	spec := &guardSpec{cond: cond, condRefs: condRefs, thenNodeID: l.qid(then.ID), thenIRKind: then.Kind}
	switch then.Kind {
	case ir.NodeExec:
		s, err := decodeExec(then, false)
		if err != nil {
			return err
		}
		spec.then = s
	case ir.NodeDo:
		if !l.allowDo {
			return fmt.Errorf("%w: guard %q then %q (node %q)", ErrUnsupportedNode, n.ID, then.Kind, then.ID)
		}
		s, err := decodeDo(then)
		if err != nil {
			return err
		}
		spec.then = s
	default:
		return fmt.Errorf("%w: guard %q then kind %q (only exec/do leaf then)", ErrUnsupportedNode, n.ID, then.Kind)
	}

	l.units = append(l.units, planUnit{
		kind:       unitGuard,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeGuard,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		guard:      spec,
	})
	return nil
}

// lowerTimeout lowers a timeout node — an advisory check-with-budget wrapper — to a
// unitTimeout. Semantics are option (a), owner-framed: a TRANSPARENT wrapper that lowers its
// single-leaf body (exec/do) and records the raw duration literal as ADVISORY journal metadata
// on the wrapper's node.activated (via appendActivated). The engine reads NO clock and NEVER
// fails the body for time; enforcement is gc-side, later, off the JOURNAL (never the bead). The
// body ALWAYS runs (unlike a guard's conditional then), so the driver is the guard path MINUS
// the cond. It admits root top-level, block member, sub-formula top-level (any prefix), and
// scatter member (root and ns) — the guard positioning table, no inAggregate fence. A
// gather-combine member, dispatch arm body, retry/repeat body, guard then, and for-each body
// stay refused by those kinds' own leaf-only sweeps / decision switches.
//
// The duration DECODE refuses an absent duration, a non-literal expr (no clock — the raw
// literal must ride verbatim), and a literal failing the pure regex (§lumenDurationRE). The
// BODY decode follows the decodeLeafSub precedent — body id non-empty, '/'+':' charset ban, a
// LOUD refusal of a non-empty body `after` gate — admitting only a single exec/do leaf; a
// body id colliding with a sibling refuses via the addSynth registry. The raw duration string
// rides the planUnit VERBATIM (never parsed or normalized) so resume re-emits it byte-for-byte
// under the :act idem token.
func (l *lowerer) lowerTimeout(n ir.Node, parent string) error {
	durRaw, ok := n.Raw["duration"]
	if !ok {
		return fmt.Errorf("%w: timeout %q missing duration", ErrUnsupportedNode, n.ID)
	}
	var durHead struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
	}
	if err := json.Unmarshal(durRaw, &durHead); err != nil {
		return fmt.Errorf("lumen: timeout %q duration: %w", n.ID, err)
	}
	if durHead.Kind != "literal" {
		return fmt.Errorf("%w: timeout %q duration must be a literal", ErrUnsupportedNode, n.ID)
	}
	var durStr string
	if err := json.Unmarshal(durHead.Value, &durStr); err != nil || !lumenDurationRE.MatchString(durStr) {
		return fmt.Errorf("%w: timeout %q duration %q", ErrUnsupportedNode, n.ID, canonScalar(durHead.Value))
	}

	bodyRaw, ok := n.Raw["body"]
	if !ok {
		return fmt.Errorf("%w: timeout %q missing body", ErrUnsupportedNode, n.ID)
	}
	var body ir.Node
	if err := json.Unmarshal(bodyRaw, &body); err != nil {
		return fmt.Errorf("lumen: timeout %q body: %w", n.ID, err)
	}
	if body.ID == "" {
		return fmt.Errorf("%w: timeout %q body missing id", ErrUnsupportedNode, n.ID)
	}
	// An authored body bypasses lowerNode's id check (decoded inline here), so this is the
	// ban site: a '/' or ':' in the body id would forge a cross-namespace activation key
	// (decodeLeafSub parity). A non-empty body `after` is refused LOUDLY — the body is a
	// single synthesized leaf (activationFor(bodyID), gates inherited from the wrapper), so a
	// gate slot on it would silently drop.
	if strings.ContainsAny(body.ID, "/:") {
		return fmt.Errorf("%w: timeout %q body id %q must not contain '/' or ':'", ErrUnsupportedNode, n.ID, body.ID)
	}
	if len(body.After) > 0 {
		return fmt.Errorf("%w: timeout %q body %q must not carry an 'after' gate", ErrUnsupportedNode, n.ID, body.ID)
	}
	spec := &timeoutSpec{duration: durStr, bodyNodeID: l.qid(body.ID), bodyIRKind: body.Kind}
	switch body.Kind {
	case ir.NodeExec:
		s, err := decodeExec(body, false)
		if err != nil {
			return err
		}
		spec.body = s
	case ir.NodeDo:
		if !l.allowDo {
			return fmt.Errorf("%w: timeout %q body %q (node %q)", ErrUnsupportedNode, n.ID, body.Kind, body.ID)
		}
		s, err := decodeDo(body)
		if err != nil {
			return err
		}
		spec.body = s
	default:
		return fmt.Errorf("%w: timeout %q body kind %q", ErrUnsupportedNode, n.ID, body.Kind)
	}

	l.units = append(l.units, planUnit{
		kind:       unitTimeout,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeTimeout,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		timeout:    spec,
	})
	return nil
}

// lowerDispatch lowers a dispatch node — a multi-way branch — to a unitDispatch. The
// subject is a value expression; each arm has a match literal and a body that is EITHER a
// single leaf (exec/do) OR a `run <formula> given {…}` sub-formula call (the DAR arm: the
// MATCHED arm mints the target's whole sub-graph under `<armBodyID>/`, lowerDispatchRunArm,
// the shared RBL/FBR helper). A block/scatter arm body is refused. A dispatch may live at ANY
// depth (the DAD slice deleted the prefix fence): matchingArm evaluates its subject against the
// unit-ns view (scopeFor(u.ns, scope)), exactly as guard's condScope and for-each's
// evalForEachArray do, so the arm mechanism — already qualified-key-general — mints under the
// deep-qualified `<armBodyID>/`. A subject that reads the dispatch's own id or an arm body id is
// refused (self-referential).
//
// ⚑B1: subject REFS and arm BODY IDS are held to the '/'+':' charset ban (guard-cond /
// authored-id parity), at ALL times. It is load-bearing for the STATELESS DAR design: an
// ungated '/'-forged subject ref would evaluate "" pre-mint then resolve the chosen arm's
// minted sub-key on a later pass → an arm FLIP mid-mint (dual live beads); a forged arm body
// id `armA/x` aliases arm A's minted sub-unit activation → chosenArm returns arm B mid-mint.
func (l *lowerer) lowerDispatch(n ir.Node, parent string) error {
	subject, ok := n.Raw["subject"]
	if !ok {
		return fmt.Errorf("%w: dispatch %q missing subject", ErrUnsupportedNode, n.ID)
	}
	var rawArms []struct {
		Match json.RawMessage `json:"match"`
		Body  json.RawMessage `json:"body"`
	}
	if raw, ok := n.Raw["arms"]; ok {
		if err := json.Unmarshal(raw, &rawArms); err != nil {
			return fmt.Errorf("lumen: dispatch %q arms: %w", n.ID, err)
		}
	}
	if len(rawArms) == 0 {
		return fmt.Errorf("%w: dispatch %q has no arms", ErrUnsupportedNode, n.ID)
	}

	spec := &dispatchSpec{subject: subject, subjectRefs: collectRefs(subject)}
	// ⚑B1: charset sweep over ALL subject refs (guard-cond parity). A subject ref carrying
	// '/' or ':' is a forged cross-namespace flat key — idents carry neither delimiter — that
	// gates via byNodeID AND resolves the flat nodeOutputs qualified key; ungated it would
	// flip the chosen arm mid-mint (the stateless re-select depends on this purity). Refuse
	// at ALL levels. The ErrUnsupportedNode wrap is load-bearing (enqueue-gate triage).
	for _, r := range spec.subjectRefs {
		if strings.ContainsAny(r, "/:") {
			return fmt.Errorf("%w: dispatch %q subject ref %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, r)
		}
	}
	seenMatch := map[string]bool{}
	seenBody := map[string]bool{}
	for i := range rawArms {
		var match struct {
			Value json.RawMessage `json:"value"`
		}
		_ = json.Unmarshal(rawArms[i].Match, &match)
		matchVal := canonScalar(match.Value)
		if seenMatch[matchVal] {
			return fmt.Errorf("%w: dispatch %q has duplicate arm match %q", ErrUnsupportedNode, n.ID, matchVal)
		}
		seenMatch[matchVal] = true

		var body ir.Node
		if err := json.Unmarshal(rawArms[i].Body, &body); err != nil {
			return fmt.Errorf("lumen: dispatch %q arm %d body: %w", n.ID, i, err)
		}
		if body.ID == "" {
			return fmt.Errorf("%w: dispatch %q arm %q body missing id", ErrUnsupportedNode, n.ID, matchVal)
		}
		// ⚑B1: an arm body id carrying '/' or ':' would let a forged id `armA/x` alias arm
		// A's minted sub-unit activation (the DAR sub-graph lives under `<armBodyID>/`), so
		// chosenArm returns the wrong arm mid-mint. Refuse it (decodeLeafSub parity). Authored
		// arm bodies bypass lowerNode's id check (decoded inline), so this is the ban site.
		if strings.ContainsAny(body.ID, "/:") {
			return fmt.Errorf("%w: dispatch %q arm %q body id %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, matchVal, body.ID)
		}
		// An arm body is a single synthesized decision leaf (activationFor(bodyID), gates
		// inherited from the dispatch), so a non-empty `after` on it silently drops today.
		// Refuse it LOUDLY (the timeout-body precedent). Before the kind switch, so it covers
		// exec/do leaf AND run arm bodies uniformly (decodeRunNode ignores a run node's after).
		if len(body.After) > 0 {
			return fmt.Errorf("%w: dispatch %q arm %q body %q must not carry an 'after' gate", ErrUnsupportedNode, n.ID, matchVal, body.ID)
		}
		// Arm body ids must be distinct: their activations (bodyID:0) are the durable
		// write-once decision records + Tier-A node ids, so a shared id would collide.
		if seenBody[body.ID] {
			return fmt.Errorf("%w: dispatch %q has duplicate arm body id %q", ErrUnsupportedNode, n.ID, body.ID)
		}
		seenBody[body.ID] = true
		for _, r := range spec.subjectRefs {
			if r == n.ID || r == body.ID {
				return fmt.Errorf("%w: dispatch %q subject references its own %s (self-referential decision)", ErrUnsupportedNode, n.ID, r)
			}
		}
		arm := dispatchArm{matchValue: matchVal, bodyNodeID: l.qid(body.ID), bodyIRKind: body.Kind}
		switch body.Kind {
		case ir.NodeExec:
			s, err := decodeExec(body, false)
			if err != nil {
				return err
			}
			arm.body = s
		case ir.NodeDo:
			if !l.allowDo {
				return fmt.Errorf("%w: dispatch %q arm %q do body (node %q)", ErrUnsupportedNode, n.ID, matchVal, body.ID)
			}
			s, err := decodeDo(body)
			if err != nil {
				return err
			}
			arm.body = s
		case ir.NodeRun:
			// A run arm (the DAR slice): the matched arm mints the target sub-formula fresh
			// under `<armBodyID>/` (mintRunBody, the shared RBL/FBR helper), and the arm
			// aggregate settles transparently at activationFor(<armBodyID>). decodeRunNode +
			// the DRY-RUN mint validate the body HERE at lowering, before any effect.
			if err := l.lowerDispatchRunArm(n, body, matchVal, &arm); err != nil {
				return err
			}
		default:
			return fmt.Errorf("%w: dispatch %q arm %q body kind %q (only exec/do leaf or run arm bodies)", ErrUnsupportedNode, n.ID, matchVal, body.Kind)
		}
		spec.arms = append(spec.arms, arm)
	}

	// ⚑S5 (RBL parity): REFUSE a run arm's env ref that names ANY of the SAME dispatch's arm
	// body ids. An arm body id is a spec-only synth activation, gate-exempt (never in
	// byNodeID), so `given { x: <someArmBody> }` would render a stable "" — but the render is
	// silently meaningless (the arm sub-graph is not a referenceable node from a sibling arm's
	// env). Refuse it loudly over the stable-"" oddity. Deferred to AFTER the arm loop so a
	// forward ref (arm i naming arm j > i) is caught. Body ids and refs are qualified to the
	// dispatch's own ns (l.qid) — bare at root, deep-qualified inside a run sub-formula (DAD).
	for i := range spec.arms {
		if spec.arms[i].bodyRun == nil {
			continue
		}
		for _, ref := range spec.arms[i].bodyRun.envRefs {
			if seenBody[ref] {
				return fmt.Errorf("%w: dispatch %q run arm %q binds arm body id %q into the sub-formula environment (a sibling arm's sub-graph is not a referenceable node)", ErrUnsupportedNode, n.ID, spec.arms[i].matchValue, ref)
			}
		}
	}

	l.units = append(l.units, planUnit{
		kind:       unitDispatch,
		activation: activationFor(l.qid(n.ID)),
		nodeID:     l.qid(n.ID),
		irKind:     ir.NodeDispatch,
		parent:     parent,
		ns:         l.prefix,
		rawAfter:   l.qAfter(n.After),
		dispatch:   spec,
	})
	return nil
}

// lowerDispatchRunArm decodes a dispatch arm's run body (`run <formula> given {…}`, the DAR
// slice) into arm: it validates the run node via decodeRunNode (inheriting the with-agent /
// runInput / non-transparent / missing-target / recursive-cycle / env-charset refusals — the
// same single path lowerRun and the repeat/for-each run-body arms use), stashes the
// PER-ARM re-lowering context, and DRY-RUN mints the arm's sub-graph (⚑S4) so an un-lowerable
// body — an unsupported sub-node, a '/'-forged cond ref, a nested loop refused at the arm prefix
// — refuses at buildUnits before any effect. (A dispatch inside the arm target now lowers, at the
// arm-qualified prefix, since DAD deleted the prefix fence.)
// The refusal is wrapped `dispatch %q arm %q run body does not lower: %w`. Every run arm is
// dry-run minted (Q-B: arms target DIFFERENT formulas, so validating one validates nothing
// about the others). It reads no index — an arm mints exactly once — so genesis, re-Advance,
// and resume mint byte-identically.
func (l *lowerer) lowerDispatchRunArm(n, body ir.Node, matchVal string, arm *dispatchArm) error {
	runSpec, sub, err := l.decodeRunNode(body)
	if err != nil {
		return err
	}
	arm.bodyRun = runSpec
	arm.bodyFormula = sub
	arm.bodyFormulas = l.formulas
	arm.bodyTargetStack = append([]string(nil), l.targetStack...)
	arm.bodyAllowDo = l.allowDo
	arm.bodyAllowCombineDo = l.allowCombineDo
	if _, _, err := mintRunBody(arm.runBodyStash, arm.bodyRun, arm.bodyNodeID, arm.bodyNodeID+"/",
		activationFor(arm.bodyNodeID), activationFor(l.qid(n.ID)), l.prefix, nil, nil, nil); err != nil {
		return fmt.Errorf("lumen: dispatch %q arm %q run body does not lower: %w", n.ID, matchVal, err)
	}
	return nil
}

// runTargetName decodes a run node's target ref, refusing anything but the
// by-name form this slice supports.
func runTargetName(n ir.Node) (string, error) {
	raw, ok := n.Raw["target"]
	if !ok {
		return "", fmt.Errorf("lumen: run %q missing target", n.ID)
	}
	var target struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &target); err != nil {
		return "", fmt.Errorf("lumen: run %q target: %w", n.ID, err)
	}
	if target.Kind != "by-name" || target.Name == "" {
		return "", fmt.Errorf("%w: run %q target kind %q (only by-name)", ErrUnsupportedNode, n.ID, target.Kind)
	}
	return target.Name, nil
}

// decodeRunEnv decodes a run node's environment bindings, validates them against
// the target formula's declared input schema (a binding for an undeclared field,
// or a required input left unbound with no default, is a loud lowering error that
// protects hand-authored bundles — the compiler itself enforces the same at
// compile time), and returns the ordered bindings plus the parent-scope ref names
// they read (for the DET sub-graph gating in resolveDeps). Ref NAMES are held to
// the same '/'-and-':' ban as authored node ids: a ref can only legally name a
// same-namespace node or an input (idents carry neither delimiter), and a
// delimiter-bearing ref is a forged cross-namespace key — "sibling/hello" would
// resolve in resolveDeps' byNodeID to another run's SUB-node, bypassing the
// sibling-member refusal (⚑SF-1) and the scope-isolation invariant (§C).
func decodeRunEnv(n ir.Node, inputFields []ir.Field) ([]runEnvField, []string, error) {
	var env struct {
		Fields []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"fields"`
	}
	if raw, ok := n.Raw["environment"]; ok {
		if err := json.Unmarshal(raw, &env); err != nil {
			return nil, nil, fmt.Errorf("lumen: run %q environment: %w", n.ID, err)
		}
	}
	declared := make(map[string]bool, len(inputFields))
	for _, f := range inputFields {
		declared[f.Name] = true
	}
	bound := map[string]bool{}
	var out []runEnvField
	var refs []string
	for _, f := range env.Fields {
		if !declared[f.Name] {
			return nil, nil, fmt.Errorf("lumen: run %q binds unknown environment field %q (not declared by target's accepts)", n.ID, f.Name)
		}
		bound[f.Name] = true
		out = append(out, runEnvField{name: f.Name, value: f.Value})
		for _, r := range collectRefs(f.Value) {
			if strings.ContainsAny(r, "/:") {
				return nil, nil, fmt.Errorf("%w: run %q environment ref %q must not contain '/' or ':' (reserved delimiters)", ErrUnsupportedNode, n.ID, r)
			}
			refs = append(refs, r)
		}
	}
	for _, fld := range inputFields {
		if fld.Required && !bound[fld.Name] && fld.Default == nil {
			return nil, nil, fmt.Errorf("lumen: run %q leaves required target input %q unbound (no default)", n.ID, fld.Name)
		}
	}
	return out, refs, nil
}

// collectRefs returns every ref name ({"kind":"ref","name":X}) reachable inside a
// value expression, at any nesting depth. It is used to derive the parent-scope
// dependencies a run's environment reads.
func collectRefs(raw json.RawMessage) []string {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var refs []string
	var walk func(x any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if t["kind"] == "ref" {
				if name, ok := t["name"].(string); ok && name != "" {
					refs = append(refs, name)
				}
			}
			for _, mv := range t {
				walk(mv)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return refs
}

// exprContainsCall reports whether an expression tree contains a {kind:"call"} node at any
// depth — the same generic deep walk as collectRefs, keyed on kind. lowerGuard uses it to
// fence call exprs out of NAMESPACE guard conds (amended SLX §1.1.7).
func exprContainsCall(raw json.RawMessage) bool {
	var v any
	if json.Unmarshal(raw, &v) != nil {
		return false
	}
	found := false
	var walk func(x any)
	walk = func(x any) {
		if found {
			return
		}
		switch t := x.(type) {
		case map[string]any:
			if t["kind"] == "call" {
				found = true
				return
			}
			for _, mv := range t {
				walk(mv)
			}
		case []any:
			for _, e := range t {
				walk(e)
			}
		}
	}
	walk(v)
	return found
}

// lowerCombine lowers a gather's authored combine block into leaf units parented
// to the gather, resolving their inter-member deps. Only the authored form is
// implemented, and only leaf members (exec/settle/do/lit/interp) are executable
// in the combine — an aggregate or a deferred kind is refused with
// ErrUnsupportedNode before any effect runs (B1).
func (l *lowerer) lowerCombine(n ir.Node) ([]planUnit, error) {
	raw, ok := n.Raw["combine"]
	if !ok {
		return nil, fmt.Errorf("lumen: gather %q missing combine", n.ID)
	}
	var combine struct {
		Kind  string          `json:"kind"`
		Block json.RawMessage `json:"block"`
	}
	if err := json.Unmarshal(raw, &combine); err != nil {
		return nil, fmt.Errorf("lumen: gather %q combine: %w", n.ID, err)
	}
	if combine.Kind != "authored" {
		// P4.2-deferred: builtin/reduce combine forms.
		return nil, fmt.Errorf("%w: gather combine kind %q (node %q)", ErrUnsupportedNode, combine.Kind, n.ID)
	}
	var block ir.Node
	if err := json.Unmarshal(combine.Block, &block); err != nil {
		return nil, fmt.Errorf("lumen: gather %q combine block: %w", n.ID, err)
	}
	members, err := childNodes(block.Raw["members"])
	if err != nil {
		return nil, fmt.Errorf("lumen: gather %q combine members: %w", n.ID, err)
	}
	gatherAct := activationFor(l.qid(n.ID))
	// A combine runs inline inside runGather, so a `do` combine member needs a Host
	// (allowCombineDo), NOT merely a PoolRouter: the top-level pool-materialization
	// walk never reaches a combine member. Gating the sub-lowerer's do arm on
	// allowCombineDo refuses a combine `do` (at any nesting depth) HERE at lowering
	// with ErrUnsupportedNode, before the lease is taken or any event is appended —
	// never as a late hard fail after the drained members ran (M2). The sub-lowerer
	// inherits this combine's namespace + bundle so a combine INSIDE a run sub-formula
	// qualifies its ids identically, and a `run` combine member is refused by the
	// leaf-only sweep below (⚑SF-2: lowerRun no longer checks placement — the run inlines
	// fully, then its non-leaf unitRun agg is rejected here).
	sub := &lowerer{
		allowDo:        l.allowCombineDo,
		allowCombineDo: l.allowCombineDo,
		formulas:       l.formulas,
		prefix:         l.prefix,
		targetStack:    l.targetStack,
		inAggregate:    true, // fences for-each/cleanup/recover members; a non-leaf run/loop member is refused by the leaf-only sweep below
		inputNames:     l.inputNames,
		scatterMembers: map[string][]string{},
	}
	if err := sub.lowerNodes(members, gatherAct); err != nil {
		return nil, err
	}
	// Only executable leaf members can run in the combine loop. A scatter/gather
	// aggregate (or any non-leaf) is refused here, before any append, preserving
	// the pre-lease refusal discipline.
	for i := range sub.units {
		if sub.units[i].kind != unitLeaf {
			return nil, fmt.Errorf("%w: gather %q combine member %q (kind %q) is not executable in a combine block",
				ErrUnsupportedNode, n.ID, sub.units[i].nodeID, sub.units[i].irKind)
		}
	}
	if err := sub.resolveDeps(); err != nil {
		return nil, err
	}
	return topoSortUnits(sub.units)
}

// resolveDeps maps each unit's IR `after` node ids to activation keys, splitting
// blocking `after` gates from drain member dependencies (H1). A dangling `after`
// reference to an unknown node is a real lowering/IR bug and is refused loudly
// (M3) rather than silently dropped.
//
// A silent (lit/interp) dep emits no journal event and never settles, so it cannot
// itself gate a dependent. But the REAL nodes a silent dep interpolates its value
// from must: a dependent that consumes a silent dep transitively depends on those
// nodes. So when a silent dep is elided, its TRANSITIVE non-silent dep closure is
// substituted in the dependent's afterDeps (H2). This is what makes Advance DEFER
// such a dependent until the silent chain's real inputs settle — so its {{ref}}
// interpolation resolves to the same value the synchronous Run produces, instead
// of running early against an unresolved ref (HIGH-2). It also gives the dependent
// a genuine topo edge to those inputs. Bare-elision was safe only for Run's
// synchronous walk, which happens to visit deps before dependents by source order;
// Advance's parking walk defers on afterDeps, so the closure must be explicit.
func (l *lowerer) resolveDeps() error {
	byNodeID := make(map[string]string, len(l.units))
	silent := make(map[string]bool, len(l.units))
	for _, u := range l.units {
		byNodeID[u.nodeID] = u.activation
		silent[u.activation] = u.silent
	}
	// Resolve every unit's raw `after` node ids to activation keys once, so the
	// silent-closure walk can follow a silent dep's own dependencies. A dangling
	// reference is refused here, before any dependent is resolved (M3).
	rawDeps := make(map[string][]string, len(l.units))
	for _, u := range l.units {
		deps := make([]string, 0, len(u.rawAfter))
		for _, dep := range u.rawAfter {
			act, ok := byNodeID[dep]
			if !ok {
				return fmt.Errorf("lumen: node %q has an `after` reference to unknown node %q", u.nodeID, dep)
			}
			deps = append(deps, act)
		}
		rawDeps[u.activation] = deps
	}
	for i := range l.units {
		u := &l.units[i]
		seen := map[string]bool{}
		var afterDeps []string
		for _, dep := range rawDeps[u.activation] {
			// The scatter a gather drains is a drain dependency (memberDeps below),
			// not a blocking gate — the whole point of a gather is to collect the
			// scatter regardless of its outcome.
			if u.kind == unitGather && dep == u.overScatter {
				continue
			}
			// Substitute a silent (lit/interp) dep's transitive non-silent closure so
			// the gate is always settleable (H2 — nonSilentClosure).
			for _, g := range nonSilentClosure(dep, silent, rawDeps) {
				if !seen[g] {
					seen[g] = true
					afterDeps = append(afterDeps, g)
				}
			}
		}
		u.afterDeps = afterDeps

		switch u.kind {
		case unitScatterAgg, unitRun, unitCleanupGuarded:
			u.memberDeps = append([]string(nil), u.members...)
		case unitGather:
			if u.overScatter != "" {
				u.memberDeps = []string{u.overScatter}
			}
		case unitCleanup:
			// A block-form cleanup DRAINS its guarded aggregate (memberDep, not an `after`
			// gate) so a FAILED block never skip-cascades the always-run finally. The
			// single-leaf form has no aggregate and no member drain.
			if u.cleanup != nil && u.cleanup.guardedAgg != "" {
				u.memberDeps = []string{u.cleanup.guardedAgg}
			}
		}
	}

	// Synthesized/spec-only body ids (a guard `then`, dispatch arm bodies, cleanup
	// guarded/body, AND a retry/repeat loop's body) are NOT plan units, so
	// topoSortUnits' duplicate-activation guard cannot see them. Two of them sharing an
	// id collide on activationFor(bodyID) / activationForAttempt(bodyID,0) — the SAME
	// fold node — so whichever settles first is silently adopted by the other (a cleanup
	// sub reloads a loop attempt's outcome and its teardown never runs; a loop adopts a
	// sub's settle as attempt 0). A body id colliding with a real node has the same
	// hazard. Refuse all of it loudly. (A loop's body id is registered here too so a
	// decision/cleanup sub can never alias a loop attempt activation.) The registry is
	// built BEFORE the expr-ref gate pass below, which consults it for guard cond refs.
	synthBodies := map[string]string{}
	addSynth := func(bodyID, owner string) error {
		if prev, ok := synthBodies[bodyID]; ok {
			return fmt.Errorf("lumen: decision body id %q used by both %q and %q", bodyID, prev, owner)
		}
		if _, ok := byNodeID[bodyID]; ok {
			return fmt.Errorf("lumen: decision %q body id %q collides with node %q", owner, bodyID, bodyID)
		}
		synthBodies[bodyID] = owner
		return nil
	}
	for i := range l.units {
		u := &l.units[i]
		switch u.kind {
		case unitGuard:
			if u.guard != nil {
				if err := addSynth(u.guard.thenNodeID, u.nodeID); err != nil {
					return err
				}
			}
		case unitDispatch:
			if u.dispatch != nil {
				for _, arm := range u.dispatch.arms {
					if err := addSynth(arm.bodyNodeID, u.nodeID); err != nil {
						return err
					}
				}
			}
		case unitCleanup:
			if u.cleanup != nil {
				// The guarded and body synthesize sub-units on activationFor(subID). If
				// they share an id (or collide with the cleanup, a loop body, or another
				// node), the second resumeMemoizes the first's settled outcome and never
				// runs — silently defeating the always-run finally. Refuse both here.
				// ⚑SHOULD-FIX-6: a block-form cleanup has NO synthesized guarded leaf
				// (guardedNodeID == ""; its block members are REAL units caught by the
				// dup-activation guard). Guard the registration so two block-form cleanups
				// in one doc do not collide on synthBodies[""]. The finally is always synth.
				if u.cleanup.guardedNodeID != "" {
					if err := addSynth(u.cleanup.guardedNodeID, u.nodeID); err != nil {
						return err
					}
				}
				if err := addSynth(u.cleanup.bodyNodeID, u.nodeID); err != nil {
					return err
				}
			}
		case unitRecover:
			if u.recover != nil {
				// Same collision hazard as cleanup: a guarded/body sub sharing an id (with
				// each other, the recover, a loop body, or a node) would alias one fold
				// activation and silently skip a sub.
				if err := addSynth(u.recover.guardedNodeID, u.nodeID); err != nil {
					return err
				}
				if err := addSynth(u.recover.bodyNodeID, u.nodeID); err != nil {
					return err
				}
			}
		case unitLoop:
			if u.loop != nil {
				// A retry/repeat body id is spec-only (the loop lowers ONE unit), but its
				// attempts activate on activationForAttempt(bodyID, N) — attempt 0 is
				// activationFor(bodyID), byte-identical to a decision/cleanup sub's
				// activation. Register it so a sub can never alias a loop attempt node.
				if err := addSynth(u.loop.bodyNodeID, u.nodeID); err != nil {
					return err
				}
			}
		case unitTimeout:
			if u.timeout != nil {
				// A timeout's body activates on activationFor(bodyID) exactly like a guard
				// then. Register it so a decision/cleanup/loop sub can never alias the
				// wrapper's body activation — and so a body id colliding with a sibling node
				// (or another decision body) refuses LOUDLY here (the SILENT sibling-id
				// aliasing miss, proved by the sibling-collision pin).
				if err := addSynth(u.timeout.bodyNodeID, u.nodeID); err != nil {
					return err
				}
			}
		}
	}

	// BAN-ONLY sweep (site 9, ⚑B2 §1.1.3): a loop cond/attempts ref that names a
	// SYNTHESIZED decision body (a sibling guard's then, a dispatch arm body, ANOTHER
	// loop's body id) is the same ungated inline/pool decision-divergence hole guard cond
	// refs get — refuse it at every level. This is deliberately SEPARATE from the gate
	// loop below: gating a leaf loop's cond refs would be a ROOT behavior change (deferred
	// loops, changed attempt counts), so it appends ZERO fold edges and the loop's gate
	// slot stays ⚑S6 bodyRun.envRefs only. The loop's OWN bare body id and iteration
	// counter are EXEMPT — both resolve via loopScope's iterationName/bodyName arms (not
	// the children view), so a sibling synth named like either must not false-refuse (the
	// body id is in synthBodies as the loop's own attempt activation).
	//
	// Catalog row (accepted, recorded): a declared INPUT named like a same-ns synth
	// body is loud-refused here even though the input arm would resolve it — the
	// improbable-trigger/errs-loud class the dispatch subjectRefs note accepts.
	// (The freeze allowlist admitting an input NAME regardless of bound-ness is now
	// honest at every level: resolveDeclaredInput lands every declared field —
	// present-null when optional-unbound — in both the ns typed layer (ga-wvqsay)
	// and the genesis-seeded root d.input (ga-ospbql), so a cond input ref can never
	// fall through to the child view; l.inputNames stays a name-set.)
	for i := range l.units {
		u := &l.units[i]
		if u.kind != unitLoop || u.loop == nil {
			continue
		}
		for _, ref := range append(append([]string(nil), u.loop.condRefs...), u.loop.attemptsRefs...) {
			if ref == u.loop.bodyBareID || ref == u.loop.iterationName {
				continue
			}
			if _, isSynth := synthBodies[u.ns+ref]; isSynth {
				return fmt.Errorf("%w: %q %q cond/attempts ref %q names a synthesized decision body (not a referenceable node)", ErrUnsupportedNode, u.irKind, u.nodeID, ref)
			}
		}
	}

	// DET hardening (seed #3): an env binding that reads a parent NODE output must
	// gate the run's sub-graph, so the sub-scope a sub-unit renders against is
	// stable before it renders — making genesis byte-identical to a crash-resume
	// re-render for ANY bundle, not only compiler-emitted IR (whose statements
	// already chain sequentially). An env ref to an input (not a node) is not a
	// gate. Runs are top-level this slice, so an env ref resolves in the run's own
	// namespace (u.ns). The gate is applied to the run aggregate AND every sub-unit
	// so the whole sub-graph defers until the referenced parent node settles.
	// byNodeID (built above) already maps every unit's node id to its activation.
	// A run gates its whole sub-graph on the parent nodes its `environment` reads; a
	// guard gates itself on the parent nodes its `cond` reads. Both keep the deciding
	// expression's inputs frozen before the unit becomes ready, so the decision is a
	// pure, stable function of the fold (never flipping across Advance passes).
	for i := range l.units {
		u := &l.units[i]
		var exprRefs []string
		switch u.kind {
		case unitRun:
			if u.run != nil {
				exprRefs = u.run.envRefs
			}
		case unitGuard:
			if u.guard != nil {
				exprRefs = u.guard.condRefs
			}
		case unitDispatch:
			if u.dispatch != nil {
				// subjectRefs stay DEEP-collected (a subject is a general value expr, so
				// collectRefs is the honest read-set for the gate) — which means a
				// member-form subject contributes its base ref "input", and a synth body
				// literally named "input" would false-positive the synth-body ban below.
				// DAD is the dispatch-in-sub-formula slice that was to revisit this: we
				// RE-ACCEPT the false-positive at every depth (improbable trigger; the ban
				// errs LOUD, never silent — the member-subject loud-error contract). Guard
				// condRefs can never carry a member base (validateClosedExpr refuses member
				// exprs) and for-each overRefs are head-derived (⚑S1), so only dispatch has this.
				exprRefs = u.dispatch.subjectRefs
			}
		case unitForEach:
			if u.forEach != nil {
				// Gate the fan-out on the nodes `over` reads, so the array is frozen
				// before any member materializes. overRefs is head-derived (⚑S1): a
				// bare-ref `over` contributes its node; an input.<field> member head-derives
				// to no ref, so it reads the immutable input layer and gates on nothing.
				exprRefs = u.forEach.overRefs
			}
		case unitLoop:
			// ⚑S6: a repeat whose body is a run gates the LOOP on the parent nodes the
			// body run's environment reads, so the parent scope is frozen before the
			// first attempt mints — every attempt then re-evaluates the same env against
			// the same stable scope (byte-identical mints). The sub-graph is minted at
			// runtime (not a top-level unit), so only the loop unit takes the gate.
			if u.loop != nil && u.loop.bodyRun != nil {
				exprRefs = u.loop.bodyRun.envRefs
			}
		default:
			continue
		}
		var gates []string
		for _, refName := range exprRefs {
			act, ok := byNodeID[u.ns+refName]
			if !ok {
				// A DECISION ref naming a SYNTHESIZED body (a sibling guard's then, a
				// dispatch arm body, a retry/repeat body id) is never a lowered unit, so it
				// takes no gate edge here — yet once the body runs, record() exposes its
				// qualified id in the flat nodeOutputs and the deciding scope resolves it.
				// Ungated, the decision becomes DRIVER-dependent: the inline walk settles
				// the sibling's body synchronously before the decision evaluates, while the
				// pool walk evaluates the same pass and freezes a null miss write-once — the
				// same doc takes opposite branches by driver. It bites a guard cond, a
				// for-each `over` (head-derived ⚑S1, so a member form contributes no
				// candidate), AND a dispatch subject (the live hole at ANY level now DAD lowers
				// deep dispatches: subjectRefs traverse this miss arm ungated while
				// advanceDispatch freezes the chosen arm write-once). Refuse all three loudly at
				// every level (the ⚑SF-1
				// refuse-now-lift-later precedent); loop/run exprRefs are untouched (a loop
				// legitimately reads its own body id via loopScope's bodyName arm).
				if _, isSynth := synthBodies[u.ns+refName]; isSynth {
					switch u.kind {
					case unitGuard:
						return fmt.Errorf("%w: guard %q cond ref %q names a synthesized decision body (not a referenceable node)", ErrUnsupportedNode, u.nodeID, refName)
					case unitForEach:
						return fmt.Errorf("%w: for-each %q over ref %q names a synthesized decision body (not a referenceable node)", ErrUnsupportedNode, u.nodeID, refName)
					case unitDispatch:
						return fmt.Errorf("%w: dispatch %q subject ref %q names a synthesized decision body (not a referenceable node)", ErrUnsupportedNode, u.nodeID, refName)
					}
				}
				continue // a ref to an input (not a node) is not a gate
			}
			// A silent (lit/interp) ref node never settles, so gating on it directly
			// would defer forever on the Advance path. Substitute its transitive
			// non-silent closure — the real nodes the silent value derives from — exactly
			// like the H2 after-dep rule. A pure constant (empty closure) adds no gate.
			closure := nonSilentClosure(act, silent, rawDeps)
			// ⚑SF-1: a run scatter member whose environment reads a SIBLING member is
			// refused loudly this slice. Silently accepting it creates an intra-scatter
			// ordering edge (the members stop being concurrent; a failed sibling
			// skip-cascades the run member, so on_fail=stop reports DEGRADED — a "stop"
			// scatter that doesn't fail). The semantics are unspecified — refuse now, lift
			// later (backward-compatible). It fires ONLY for a run parented directly by a
			// scatter (activationNodeID(u.parent) keys scatterMembers) reading ANOTHER member
			// (candidate != self); an env ref to an OUTSIDE node is not in the member set and
			// gates normally, and a top-level run's parent is the root (missing the map). The
			// check covers the raw ref AND its closure results: a SILENT sibling (excluded
			// from the drained member set) whose closure lands on a non-silent sibling would
			// otherwise install the same edge through the substitution hop.
			if u.kind == unitRun {
				if members, isMember := l.scatterMembers[activationNodeID(u.parent)]; isMember {
					for _, candidate := range append([]string{act}, closure...) {
						if candidate == u.activation {
							continue
						}
						for _, m := range members {
							if m == candidate {
								return fmt.Errorf("lumen: run %q (a scatter member) reads sibling member %q in its environment; intra-scatter member refs are unsupported this slice (members must stay concurrent)", u.nodeID, activationNodeID(candidate))
							}
						}
					}
				}
			}
			for _, g := range closure {
				if g != u.activation {
					gates = append(gates, g)
				}
			}
		}
		// ⚑B2: a run-bodied for-each ALSO gates on the parent nodes its BODY RUN's
		// environment reads (minus the binder) — so the parent scope is frozen before the
		// first member mints, exactly like a repeat run body gates its loop (⚑S6). These
		// env refs are contributed as ADDITIONAL gate-only refs, NEVER unioned into overRefs:
		// they are synth-ban-EXEMPT (the loop/run precedent — a run body legitimately names a
		// synth sibling via its env, static-run parity) while the over ref keeps the synth-ban
		// above. The BINDER is EXCLUDED (Q-D): withBinder supplies the element regardless of a
		// same-named settled node, so a gate would freeze a value the eval never reads. A
		// silent env-ref node is closure-substituted (nonSilentClosure), not refused.
		if u.kind == unitForEach && u.forEach != nil && u.forEach.bodyRun != nil {
			for _, refName := range u.forEach.bodyRun.envRefs {
				if refName == u.forEach.binder {
					continue
				}
				act, ok := byNodeID[u.ns+refName]
				if !ok {
					continue // an input ref (or a synth-body ref — exempt) is not a gate
				}
				for _, g := range nonSilentClosure(act, silent, rawDeps) {
					if g != u.activation {
						gates = append(gates, g)
					}
				}
			}
		}
		// ⚑B2 (DAR): a dispatch with RUN arms ALSO gates on the parent nodes EACH arm's body
		// run's environment reads — so the parent scope is frozen before ANY arm mints (the
		// LIS separate-contribution mechanism), UNIONED with the subject-ref gate above. These
		// env refs are gate-only, synth-ban-EXEMPT (the loop/run/for-each precedent — an arm
		// run body legitimately names a synth sibling via its env, static-run parity) while the
		// SUBJECT ref keeps the synth-ban. The union is STATIC across ALL arms (a dispatch has
		// no binder to exclude): an UNCHOSEN arm's env dep still gates, so an unchosen arm's
		// failed env dep skip-cascades the WHOLE dispatch (fold-edge honesty — sharply
		// different from a no-match PASS). A silent env-ref node is closure-substituted, not
		// refused. An arm-env ref to another arm's body id already refused at lowering (⚑S5).
		if u.kind == unitDispatch && u.dispatch != nil {
			for ai := range u.dispatch.arms {
				arm := &u.dispatch.arms[ai]
				if arm.bodyRun == nil {
					continue
				}
				for _, refName := range arm.bodyRun.envRefs {
					act, ok := byNodeID[u.ns+refName]
					if !ok {
						continue // an input ref (or a synth-body ref — exempt) is not a gate
					}
					for _, g := range nonSilentClosure(act, silent, rawDeps) {
						if g != u.activation {
							gates = append(gates, g)
						}
					}
				}
			}
		}
		if len(gates) == 0 {
			continue
		}
		// Apply to the unit itself; a run ALSO applies to its whole inlined sub-graph
		// (so no sub-unit renders before an env-ref parent settles).
		subPrefix := u.nodeID + "/"
		for j := range l.units {
			s := &l.units[j]
			if s.activation == u.activation || (u.kind == unitRun && strings.HasPrefix(s.nodeID, subPrefix)) {
				s.afterDeps = appendMissing(s.afterDeps, gates)
			}
		}
	}

	// A for-each `over` that reads a SILENT (lit/interp) node has no settleable gate to
	// order the fan after the value is computed: a pure-constant silent node contributes
	// an empty non-silent closure (no gate edge), so topo order would fall back to source
	// order and the fan could evaluate `over` against an uncomputed (empty) scope. Refuse
	// it at load — the array must come from an input or a real (do/exec) node output this
	// slice. (A member `input.<field>` over head-derives to NO overRef ⚑S1, so it never
	// enters this sweep — it reads the immutable input layer, not a node.)
	for i := range l.units {
		u := &l.units[i]
		if u.kind != unitForEach || u.forEach == nil {
			continue
		}
		for _, ref := range u.forEach.overRefs {
			if act, ok := byNodeID[u.ns+ref]; ok && silent[act] {
				return fmt.Errorf("%w: for-each %q over reads silent node %q (the array must come from an input or a do/exec output)", ErrUnsupportedNode, u.nodeID, ref)
			}
		}
	}

	return nil
}

// nonSilentClosure returns the settleable activations a dependency contributes as a
// gate: the activation itself if it is non-silent; otherwise the transitive
// non-silent closure over its own dependencies. A silent (lit/interp) unit emits no
// journal event and never settles, so a dependent must gate on the REAL nodes the
// silent value interpolates from (H2) — gating on the silent activation directly
// would defer the dependent forever on the Advance path. A silent cycle or a pure
// constant contributes nothing. It is the single source of truth for the
// silent-substitution both resolveDeps' after-dep resolution and its env-ref gate
// pass rely on.
func nonSilentClosure(act string, silent map[string]bool, rawDeps map[string][]string) []string {
	var out []string
	seen := map[string]bool{}
	var walk func(a string, guard map[string]bool)
	walk = func(a string, guard map[string]bool) {
		if a == "" {
			return
		}
		if silent[a] {
			if guard[a] {
				return
			}
			guard[a] = true
			for _, d := range rawDeps[a] {
				walk(d, guard)
			}
			return
		}
		if seen[a] {
			return
		}
		seen[a] = true
		out = append(out, a)
	}
	walk(act, map[string]bool{})
	return out
}

// appendMissing appends each element of add to dst that is not already present,
// preserving order and skipping duplicates.
func appendMissing(dst, add []string) []string {
	seen := make(map[string]bool, len(dst))
	for _, d := range dst {
		seen[d] = true
	}
	for _, a := range add {
		if !seen[a] {
			dst = append(dst, a)
			seen[a] = true
		}
	}
	return dst
}

// topoSortUnits returns units in a stable topological order over their resolved
// deps. Ties break by source order for determinism; a cycle is a loud error.
func topoSortUnits(units []planUnit) ([]planUnit, error) {
	n := len(units)
	idx := make(map[string]int, n)
	for i, u := range units {
		if _, dup := idx[u.activation]; dup {
			return nil, fmt.Errorf("lumen: duplicate activation %q", u.activation)
		}
		idx[u.activation] = i
	}
	indeg := make([]int, n)
	adj := make([][]int, n)
	for i := range units {
		for _, dep := range units[i].allDeps() {
			j, ok := idx[dep]
			if !ok {
				continue
			}
			adj[j] = append(adj[j], i)
			indeg[i]++
		}
	}
	order := make([]planUnit, 0, n)
	done := make([]bool, n)
	for len(order) < n {
		picked := -1
		for i := 0; i < n; i++ {
			if !done[i] && indeg[i] == 0 {
				picked = i
				break
			}
		}
		if picked == -1 {
			return nil, fmt.Errorf("lumen: dependency cycle among units")
		}
		done[picked] = true
		order = append(order, units[picked])
		for _, m := range adj[picked] {
			indeg[m]--
		}
	}
	return order, nil
}

// mintRunBody lowers a run BODY's sub-formula into a fresh sub-graph rooted at a
// transparent aggregate — the SINGLE lowering path shared by the repeat run-body
// attempt (RBL: mintRunBodyAttempt) and the for-each run-body member (FBR:
// forEachRunMember), so there is no drift between a validated materialization and a
// driven one. It builds a fresh lowerer at `prefix`, inlines the sub-formula's nodes
// under `aggActivation`, resolves deps, and topo-sorts (cycle detection lives in
// topoSort); it then synthesizes the transparent aggregate (kind unitRun, at
// aggActivation/aggNodeID, parented under parentActivation so the minted units stay
// OUT of the enclosing runOutcome) and propagates the caller's gate (afterDeps) onto
// every minted unit + the aggregate (⚑S6 / lowerRun H1: fold-edge honesty even though
// a gated-off materialization never mints).
//
// The differing coordinates are explicit params so each leg stays BYTE-IDENTICAL:
// loop = bodyNodeID:N / bodyNodeID / `<bodyNodeID>/<N>/` / loopActivation / loopNS,
// memberIndex nil; fan = `<fan>/<i>:0` / `<fan>/<i>` / `<fan>/<i>/` / the fan aggregate
// activation / u.ns, memberIndex &i (the leaf-member projection parity, Q-C); dispatch arm
// (DAR) = `<armBodyID>:0` / `<armBodyID>` / `<armBodyID>/` / the DISPATCH activation / u.ns,
// memberIndex nil (an arm is not a fan member; it settles the dispatch transparently). The
// aggregate is NOT topo-linked into the returned unit list — the caller drives the
// sub-units first, then the aggregate LAST (⚑S1: its Members edges' from_id are the
// sub-node ids, whose node rows must already exist under the Tier-A edges.from_id FK;
// a pre-activated aggregate is a committed poison event). It reads N/the index only via
// the prefix string, so genesis, re-Advance, and resume mint byte-identically.
func mintRunBody(stash runBodyStash, run *runSpec, aggNodeID, prefix, aggActivation, parentActivation, ns string, afterDeps, rawAfter []string, memberIndex *int) ([]planUnit, planUnit, error) {
	l := &lowerer{
		allowDo:        stash.bodyAllowDo,
		allowCombineDo: stash.bodyAllowCombineDo,
		formulas:       stash.bodyFormulas,
		prefix:         prefix,
		// The sub-graph's nested runs check cycles against the body run's target on top
		// of the lower-time chain — the exact stack lowerRun pushes for an inlined run.
		targetStack: append(append([]string(nil), stash.bodyTargetStack...), run.target),
		// Q-D: a run-body loop nested INSIDE this minted sub-graph freezes its cond against
		// THIS sub-formula's declared inputs — bodyFormula is already stashed (no new field).
		inputNames:     fieldNameSet(stash.bodyFormula.Input.Fields),
		scatterMembers: map[string][]string{},
	}
	if err := l.lowerNodes(stash.bodyFormula.Nodes, aggActivation); err != nil {
		return nil, planUnit{}, err
	}
	if err := l.resolveDeps(); err != nil {
		return nil, planUnit{}, err
	}
	ordered, err := topoSortUnits(l.units)
	if err != nil {
		return nil, planUnit{}, err
	}
	// Direct members: units parented directly by the aggregate, non-silent, collected
	// from the PRE-topo unit list — SOURCE order, exactly lowerRun's rule (a nested
	// aggregate contributes its own children, which must not inflate this member set;
	// silent lit/interp never settle). Source order is load-bearing: the aggregate's
	// Members payload and settleTransparentAgg's "returns lastResult" selection (the
	// LAST source-order member that ran) must be byte-identical to an inlined static
	// run of the same sub-formula — a forward `after` ref makes source ≠ topo.
	var members []string
	for i := range l.units {
		if l.units[i].parent == aggActivation && !l.units[i].silent {
			members = append(members, l.units[i].activation)
		}
	}
	// Propagate the caller's gate onto every minted unit (⚑S6 / lowerRun H1): a gated-off
	// materialization never mints, but the fold edges stay honest across a drop+refold.
	for i := range ordered {
		ordered[i].afterDeps = appendMissing(ordered[i].afterDeps, afterDeps)
	}
	agg := planUnit{
		kind:        unitRun,
		activation:  aggActivation,
		nodeID:      aggNodeID,
		irKind:      ir.NodeRun,
		parent:      parentActivation,
		memberIndex: memberIndex,
		ns:          ns,
		afterDeps:   append([]string(nil), afterDeps...),
		rawAfter:    append([]string(nil), rawAfter...),
		members:     members,
		memberDeps:  members,
		run:         run,
	}
	return ordered, agg, nil
}

// mintRunBodyAttempt mints a repeat run body's sub-formula for attempt N via mintRunBody
// (the RBL leg): the attempt aggregate settles at activationForAttempt(bodyNodeID, N)
// under the loop activation, and the sub-graph lives at `<bodyNodeID>/<N>/`. It carries
// no member index (a loop attempt is not a fan member). See mintRunBody for the ⚑S1/⚑S6
// invariants and the dry-run / per-attempt sharing.
func (spec *loopSpec) mintRunBodyAttempt(attempt int, loopActivation, loopNS string, loopAfterDeps, loopRawAfter []string) ([]planUnit, planUnit, error) {
	aggAct := activationForAttempt(spec.bodyNodeID, attempt)
	prefix := spec.bodyNodeID + "/" + strconv.Itoa(attempt) + "/"
	return mintRunBody(spec.runBodyStash, spec.bodyRun, spec.bodyNodeID, prefix, aggAct, loopActivation, loopNS, loopAfterDeps, loopRawAfter, nil)
}

func (l *lowerer) addLeaf(n ir.Node, parent string, memberIndex *int, s step, silent bool) {
	l.units = append(l.units, planUnit{
		kind:        unitLeaf,
		activation:  activationFor(l.qid(n.ID)),
		nodeID:      l.qid(n.ID),
		irKind:      n.Kind,
		parent:      parent,
		memberIndex: memberIndex,
		silent:      silent,
		ns:          l.prefix,
		rawAfter:    l.qAfter(n.After),
		leaf:        s,
	})
}

// gatherOverName extracts the scatter node id a gather drains from its `over`
// ref expression.
func gatherOverName(n ir.Node) (string, error) {
	raw, ok := n.Raw["over"]
	if !ok {
		return "", fmt.Errorf("lumen: gather %q missing over", n.ID)
	}
	var over struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &over); err != nil {
		return "", fmt.Errorf("lumen: gather %q over: %w", n.ID, err)
	}
	if over.Kind != "ref" || over.Name == "" {
		return "", fmt.Errorf("%w: gather %q over kind %q (node %q)", ErrUnsupportedNode, n.ID, over.Kind, n.ID)
	}
	return over.Name, nil
}

// decodeDo lifts a do (agent) node into a step, preserving the raw node so the
// prompt renders against the live scope at run time, and extracting the optional
// interpreter.agent binding name.
func decodeDo(n ir.Node) (step, error) {
	// Loud wall (§1.2.5): a do template may carry an index interpolation `{{ base[i] }}`,
	// but only a STRICT-parsing one renders at runtime — a pre-grammar hit that fails the
	// strict grammar would sweep clean then misrender verbatim, so refuse it here at
	// buildUnits (with the run-body / dispatch-arm dry-run provenance when re-entered).
	if err := sweepIndexParts(n.Raw, true); err != nil {
		return step{}, err
	}
	// A call-looking literal part (`ident(...)`) has NO renderable subset — even on the do
	// route (which renders index interpolations) it is refused, since a template part is never
	// evaluated as a call.
	if err := sweepCallParts(n.Raw); err != nil {
		return step{}, err
	}
	s := step{kind: ir.NodeDo, id: n.ID, raw: n.Raw}
	if raw, ok := n.Raw["interpreter"]; ok {
		var interp struct {
			Agent struct {
				Name string `json:"name"`
			} `json:"agent"`
		}
		if err := json.Unmarshal(raw, &interp); err != nil {
			return step{}, fmt.Errorf("lumen: do %q interpreter: %w", n.ID, err)
		}
		s.agentRef = interp.Agent.Name
	}
	meta, err := decodeDoMetadata(n)
	if err != nil {
		return step{}, err
	}
	s.metadata = meta
	return s, nil
}

// decodeDoMetadata decodes a do node's optional static routing/affinity metadata
// (n.Raw["metadata"]) into a string map that rides VERBATIM onto the minted work
// bead. Two decode-time walls run here — inside buildUnits, which the enqueue gate
// pre-executes before any journal event, so both surface LOUD at enqueue:
//
//  1. Every value must be a STATIC LITERAL: a value carrying a `{{…}}` template
//     (plain, indexed, or call form) is refused. This is the load-bearing
//     determinism wall — static metadata is a pure function of the IR, so it is
//     byte-identical on every Advance pass with no scope dependency, sidestepping
//     the folded-prompt re-render hazard entirely.
//  2. An engine-reserved routing key (reservedDoMetadataKeys) is refused so a pack
//     cannot clobber the authoritative keys the dispatch seam stamps.
//
// An absent metadata object is a nil map (no field on the wire). Refusals wrap
// ErrUnsupportedNode so the enqueue gate triages them in house style.
func decodeDoMetadata(n ir.Node) (map[string]string, error) {
	raw, ok := n.Raw["metadata"]
	if !ok {
		return nil, nil
	}
	var meta map[string]string
	if err := json.Unmarshal(raw, &meta); err != nil {
		return nil, fmt.Errorf("%w: do %q metadata must be a static string map: %w", ErrUnsupportedNode, n.ID, err)
	}
	if len(meta) == 0 {
		return nil, nil
	}
	keys := make([]string, 0, len(meta))
	for k := range meta {
		keys = append(keys, k)
	}
	sort.Strings(keys) // deterministic refusal order
	for _, k := range keys {
		if reservedDoMetadataKeys[k] {
			return nil, fmt.Errorf("%w: do %q metadata key %q is engine-reserved (routing keys are stamped by the dispatch seam)", ErrUnsupportedNode, n.ID, k)
		}
		if strings.Contains(meta[k], "{{") {
			return nil, fmt.Errorf("%w: do %q metadata value for %q is interpolated (%q); metadata must be a static literal", ErrUnsupportedNode, n.ID, k, meta[k])
		}
	}
	return meta, nil
}

// renderPrompt renders a do node's body to the prompt string against scope. It
// prefers the structured template.parts form and falls back to {{var}}
// interpolation of body.raw. An absent body is an empty prompt.
func renderPrompt(raw map[string]json.RawMessage, scope map[string]string) (string, error) {
	bodyRaw, ok := raw["body"]
	if !ok {
		return "", nil
	}
	var body struct {
		Raw      string          `json:"raw"`
		Template json.RawMessage `json:"template"`
	}
	if err := json.Unmarshal(bodyRaw, &body); err != nil {
		return "", fmt.Errorf("lumen: do body: %w", err)
	}
	if len(body.Template) > 0 {
		return evalInterp(map[string]json.RawMessage{"template": body.Template}, scope)
	}
	return interpolate(body.Raw, scope), nil
}

func childNodes(raw json.RawMessage) ([]ir.Node, error) {
	if raw == nil {
		return nil, nil
	}
	var nodes []ir.Node
	if err := json.Unmarshal(raw, &nodes); err != nil {
		return nil, fmt.Errorf("decoding members: %w", err)
	}
	return nodes, nil
}

// decodeExec lifts an exec node's interpreter/body/exitMap into a step. allowPending
// gates the exitMap.pending set: it is true ONLY when the exec is a repeat loop LEAF
// body (lowerLoop's NodeRepeat arm). Every other decode site passes false, so an
// exitMap.pending on a retry body, a run body's inner exec, a scatter member, a guard/
// dispatch/timeout/cleanup body, or a top-level exec (outside any loop) is refused
// LOUDLY at lowering (ErrUnsupportedNode) — pending is repeat-scoped and would otherwise
// settle inert. The refusal runs inside buildUnits, which the enqueue gate pre-executes
// before any journal event, so it surfaces at enqueue.
func decodeExec(n ir.Node, allowPending bool) (step, error) {
	// Loud wall (§1.2.5): an exec renders via interpolate(body.raw) ONLY and CANNOT index,
	// so ANY index interpolation in its template parts is refused here (no strict carve-out)
	// — a strict-passing part would sweep clean then misrender verbatim on the exec path.
	if err := sweepIndexParts(n.Raw, false); err != nil {
		return step{}, err
	}
	// A call-looking literal part (`ident(...)`) can never render on the exec path either —
	// refuse it too.
	if err := sweepCallParts(n.Raw); err != nil {
		return step{}, err
	}
	s := step{kind: ir.NodeExec, id: n.ID, program: exechost.ProgramExec}

	if raw, ok := n.Raw["interpreter"]; ok {
		var interp struct {
			Program struct {
				Kind string `json:"kind"`
			} `json:"program"`
		}
		if err := json.Unmarshal(raw, &interp); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q interpreter: %w", n.ID, err)
		}
		if interp.Program.Kind != "" {
			s.program = interp.Program.Kind
		}
	}

	if raw, ok := n.Raw["body"]; ok {
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal(raw, &body); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q body: %w", n.ID, err)
		}
		s.script = body.Raw
	}

	if raw, ok := n.Raw["exitMap"]; ok {
		var em struct {
			Pass      []int `json:"pass"`
			Retryable []int `json:"retryable"`
			Pending   []int `json:"pending"`
		}
		if err := json.Unmarshal(raw, &em); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q exitMap: %w", n.ID, err)
		}
		s.passCodes = em.Pass
		// The retryable exit-code set is the retry arm's classification DATA (a
		// timeout wraps to retryable elsewhere): an exit in this set marks a failed
		// attempt infrastructure-retryable, carried per-settle so the fold — not a
		// driver-side memory — is the source on re-Advance (§3.3).
		s.retryableCodes = em.Retryable
		// The pending exit-code set settles OutcomePending (a non-consuming re-poll). It
		// is REPEAT-scoped: only a repeat leaf body may declare it. Refuse it anywhere
		// else at lowering (before any effect) rather than letting a pending settle inert
		// on a retry body / non-loop node.
		if len(em.Pending) > 0 {
			if !allowPending {
				return step{}, fmt.Errorf("%w: exec %q declares exitMap.pending but is not a repeat loop leaf body (pending is a repeat check-poll concept)", ErrUnsupportedNode, n.ID)
			}
			s.pendingCodes = em.Pending
		}
	}

	if raw, ok := n.Raw["cwd"]; ok {
		if err := json.Unmarshal(raw, &s.cwd); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q cwd: %w", n.ID, err)
		}
	}

	if raw, ok := n.Raw["env"]; ok {
		var envMap map[string]string
		if err := json.Unmarshal(raw, &envMap); err != nil {
			return step{}, fmt.Errorf("lumen: exec %q env: %w", n.ID, err)
		}
		keys := make([]string, 0, len(envMap))
		for k := range envMap {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			s.env = append(s.env, k+"="+envMap[k])
		}
	}

	return s, nil
}

// decodeSettle lifts a settle node's outcome into a step; its value expression
// is evaluated later against the live scope.
func decodeSettle(n ir.Node) step {
	s := step{kind: ir.NodeSettle, id: n.ID, raw: n.Raw}
	if raw, ok := n.Raw["outcome"]; ok {
		_ = json.Unmarshal(raw, &s.outcome)
	}
	return s
}

// evalValue renders an IR value expression to a string against scope.
func evalValue(raw json.RawMessage, scope map[string]string) (string, error) {
	if len(raw) == 0 {
		return "", nil
	}
	var probe struct {
		Kind  string          `json:"kind"`
		Value json.RawMessage `json:"value"`
		Name  string          `json:"name"`
		Expr  json.RawMessage `json:"expr"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return scalarToString(raw), nil
	}
	switch probe.Kind {
	case "":
		return scalarToString(raw), nil
	case "literal":
		return scalarToString(probe.Value), nil
	case "ref":
		return scope[probe.Name], nil
	case "interp":
		return evalValue(probe.Expr, scope)
	case "expr":
		// The wrapper a run node's environment binding (and other value contexts)
		// emits: {"kind":"expr","expr":{...}}. Recurse into the inner expression.
		return evalValue(probe.Expr, scope)
	case "member":
		// A member `X.Y` resolves against a FLAT-keyed scope entry "X.Y" — used by the
		// recover error binding ({{ error.reason }} etc., which the recover driver binds
		// as error.reason/step/message). Localized: resolve ONLY when the flat key
		// exists, so any OTHER member expr in a template stays a loud unsupported error
		// (never a silent empty render).
		var m struct {
			Base struct {
				Name string `json:"name"`
			} `json:"base"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &m); err != nil {
			return "", fmt.Errorf("lumen: member value: %w", err)
		}
		if v, ok := scope[m.Base.Name+"."+m.Name]; ok {
			return v, nil
		}
		return "", fmt.Errorf("lumen: unsupported value expression kind %q", probe.Kind)
	default:
		return "", fmt.Errorf("lumen: unsupported value expression kind %q", probe.Kind)
	}
}

// evalInterp renders an interp node to a string.
func evalInterp(raw map[string]json.RawMessage, scope map[string]string) (string, error) {
	if v, ok := raw["value"]; ok {
		return evalValue(v, scope)
	}

	partsRaw := raw["parts"]
	if partsRaw == nil {
		if tmpl, ok := raw["template"]; ok {
			var t struct {
				Parts json.RawMessage `json:"parts"`
			}
			if err := json.Unmarshal(tmpl, &t); err != nil {
				return "", fmt.Errorf("lumen: interp template: %w", err)
			}
			partsRaw = t.Parts
		}
	}
	if partsRaw != nil {
		var parts []map[string]json.RawMessage
		if err := json.Unmarshal(partsRaw, &parts); err != nil {
			return "", fmt.Errorf("lumen: interp parts: %w", err)
		}
		var sb strings.Builder
		for _, p := range parts {
			var kind string
			_ = json.Unmarshal(p["kind"], &kind)
			if kind == "text" {
				sb.WriteString(scalarToString(p["value"]))
				continue
			}
			expr, err := json.Marshal(p)
			if err != nil {
				return "", err
			}
			// A literal part the compiler could not lower to a structured index kind is an
			// index interpolation `{{ base[index] }}` (§0.3): render it engine-defined
			// (SLX §1.2.4). A render failure returns *indexRenderError, which the do drivers
			// catch and settle the step failed — every non-index literal (and any strict-fail
			// on a non-do route) was refused at LOWER, so this arm is reached only for a
			// strict-passing do-template index.
			if src, ok := exprLiteralString(expr); ok {
				if pi, ok := parseIndexExpr(src); ok {
					v, err := renderIndexExpr(pi, scope)
					if err != nil {
						return "", err
					}
					sb.WriteString(v)
					continue
				}
			}
			v, err := evalValue(expr, scope)
			if err != nil {
				return "", err
			}
			sb.WriteString(v)
		}
		return sb.String(), nil
	}

	if b, ok := raw["body"]; ok {
		var body struct {
			Raw string `json:"raw"`
		}
		if err := json.Unmarshal(b, &body); err != nil {
			return "", fmt.Errorf("lumen: interp body: %w", err)
		}
		return interpolate(body.Raw, scope), nil
	}

	return "", fmt.Errorf("lumen: interp node: unrecognized shape")
}

// canonScalar stringifies a JSON scalar the SAME way baseScope seeds a run input
// value: a string as-is, else canonical Go JSON (json.Marshal of the decoded value).
// A dispatch subject that resolves through baseScope (a numeric input → float64 →
// "5") must compare equal to a match literal spelled 5.0 or 5 — so the match side
// canonicalizes here rather than keeping raw source bytes ("5.0"). Falls back to raw
// bytes only for a value that will not decode.
func canonScalar(raw json.RawMessage) string {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return scalarToString(raw)
	}
	if s, ok := v.(string); ok {
		return s
	}
	if b, err := json.Marshal(v); err == nil {
		return string(b)
	}
	return scalarToString(raw)
}

// scalarToString renders a JSON scalar as a plain Go string.
func scalarToString(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return strings.TrimSpace(string(raw))
}
