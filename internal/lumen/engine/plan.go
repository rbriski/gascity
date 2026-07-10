package engine

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/lumen/exechost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

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
	cwd            string
	env            []string

	// settle fields.
	outcome string

	// do fields.
	agentRef string

	// settle/lit/interp/do value evaluation.
	raw map[string]json.RawMessage
}

// unitKind classifies a plan unit's execution shape.
type unitKind int

const (
	unitLeaf       unitKind = iota // exec / settle / lit / interp / do
	unitScatterAgg                 // scatter aggregate: settles after its members
	unitGather                     // gather: head-of-line drain + authored combine
	unitLoop                       // retry / repeat: the attempt-loop over a leaf body
	unitRun                        // run: transparent sub-formula call over an inlined sub-graph
	unitGuard                      // guard: a conditional single-step arm (cond ? then : pass)
	unitDispatch                   // dispatch: a multi-way branch (subject -> matching arm's body)
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

// dispatchArm is one discriminated-variant arm: a match value and the single leaf
// body to run when the subject equals it.
type dispatchArm struct {
	matchValue string
	bodyNodeID string
	bodyIRKind ir.NodeKind
	body       step
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

// loopSpec carries a retry/repeat loop node's decoded shape: the leaf body it
// re-attempts, and the closed exit expression that decides re-runs (retry:
// attempts count; repeat: the `until` condition + iteration binding). The body is
// a SINGLE leaf (exec / do); block / run / nested-loop / scatter bodies are
// refused at lowering (§3.2). No IR is read at run time — the driver evaluates
// these expressions over folded attempt outcomes (D-P4-1).
type loopSpec struct {
	irKind        ir.NodeKind     // NodeRetry | NodeRepeat
	bodyNodeID    string          // the leaf body's node id (the re-attempted bare id)
	bodyIRKind    ir.NodeKind     // NodeExec | NodeDo
	body          step            // the decoded leaf body
	attemptsExpr  json.RawMessage // retry: the attempts count expression (closed subset)
	condExpr      json.RawMessage // repeat: the `until` exit condition (closed subset)
	iterationName string          // repeat: the 1-based iteration binding name
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
	// inAggregate is true while lowering a scatter member / gather combine — it (not the
	// bare parent activation, which is also non-empty for an inlined sub-formula's own
	// statements) is what refuses a `run`/`loop` placed under an aggregate, so a nested
	// run (a run that is a sub-formula's own statement) stays allowed.
	formulas    map[string]*ir.IR
	prefix      string
	targetStack []string
	inAggregate bool
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
		s, err := decodeExec(n)
		if err != nil {
			return err
		}
		l.addLeaf(n, parent, memberIndex, s, false)
		return nil

	case ir.NodeSettle:
		l.addLeaf(n, parent, memberIndex, decodeSettle(n), false)
		return nil

	case ir.NodeLit, ir.NodeInterp:
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

	default:
		// P4.2-deferred: async, await, cancel, channel, cleanup, close, dispatch,
		// fail-channel, for-each(scatter form:each), guard, map, quote, raise,
		// recover, timeout. Vocabulary + reducer transitions exist (total fold);
		// executor arms land in later slices. Filed follow-up: blueprint §7 P4.2
		// corpus TODO. (retry/repeat land as the L5 attempt-loop arm above; run as
		// the R lowerRun arm.)
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
	if form != "members" {
		// P4.2-deferred: scatter form:each iterates `over` a runtime array; it
		// needs member materialization from a value, out of scope for this slice.
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
	l.inAggregate = true // members are under an aggregate: a run/loop member is refused
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

// lowerLoop lowers a retry/repeat node to a unitLoop planUnit over a single leaf
// body (§3.2). It is the ONLY place a retry/repeat's shape is validated: the body
// must be exec/do (block / run / nested loop / scatter bodies are refused HERE,
// before any append); the closed exit expression (retry: `attempts`; repeat:
// `cond`) is validated by walking its tree, so a bad expression refuses at LOAD,
// never at attempt N. A loop nested under an aggregate (scatter member / gather
// combine) is refused this slice — it is a non-top-level, non-leaf placement.
//
// The label names the BODY (compiled evidence: repeat/retry give the loop node a
// synthetic id and the body keeps its own id), and dependents gate on the LOOP
// node id (the compiler rewrites body-binding `after` refs onto the loop node), so
// resolveDeps registers only the loop unit. A raw `after: [bodyID]` in hand-crafted
// IR is a dangling ref, refused loudly (the compiler never emits it).
func (l *lowerer) lowerLoop(n ir.Node, parent string) error {
	// A loop may be a SCATTER MEMBER at top level (RN slice — the mol-review-quorum lane
	// shape). Still refused: a loop INSIDE a run sub-formula (prefix != "") — its decision
	// scope (loopScope) is namespace-unaware, so cond/attempts refs would resolve wrong;
	// that needs a namespace-aware loopScope (a follow-on). Also refused: a loop as a
	// gather-COMBINE member (lowerCombine's leaf-only check), and a loop whose BODY is not
	// a leaf exec/do (the body switch below refuses run/block/nested-loop bodies).
	if l.prefix != "" {
		return fmt.Errorf("%w: %q %q inside a run sub-formula; loops are top-level (or a top-level scatter member) only this slice", ErrUnsupportedNode, n.Kind, n.ID)
	}
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
		s, err := decodeExec(body)
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
	default:
		return fmt.Errorf("%w: %q %q body kind %q (only exec/do leaf bodies)", ErrUnsupportedNode, n.Kind, n.ID, body.Kind)
	}
	if body.ID == "" {
		return fmt.Errorf("%w: %q %q body missing id", ErrUnsupportedNode, n.Kind, n.ID)
	}
	spec.bodyNodeID = l.qid(body.ID)
	spec.bodyIRKind = body.Kind

	switch n.Kind {
	case ir.NodeRetry:
		att, ok := n.Raw["attempts"]
		if !ok {
			return fmt.Errorf("%w: retry %q missing attempts", ErrUnsupportedNode, n.ID)
		}
		if err := validateClosedExpr(att); err != nil {
			return err
		}
		spec.attemptsExpr = att
	case ir.NodeRepeat:
		cond, ok := n.Raw["cond"]
		if !ok {
			return fmt.Errorf("%w: repeat %q missing cond", ErrUnsupportedNode, n.ID)
		}
		if err := validateClosedExpr(cond); err != nil {
			return err
		}
		spec.condExpr = cond
		iterName := "iteration"
		if raw, ok := n.Raw["iterationName"]; ok {
			_ = json.Unmarshal(raw, &iterName)
		}
		if iterName == "" {
			iterName = "iteration"
		}
		spec.iterationName = iterName
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

// lowerRun lowers a run node — a transparent call to another formula — by
// inlining the target formula's nodes as ordinary units under a `<runID>/`
// namespace and emitting a unitRun aggregate that settles with the sub-formula's
// transparent outcome. R1 supports TOP-LEVEL run only: a run nested under an
// aggregate (scatter/gather/combine, parent != "") or as a loop body (refused by
// lowerLoop's body switch) is refused loudly this slice; later slices lift those.
// The target body comes from the document's flat `formulas` bundle; a missing
// target, a recursive cycle, an unknown env field, or an unbound required input
// all refuse HERE, before any append.
func (l *lowerer) lowerRun(n ir.Node, parent string) error {
	if l.inAggregate {
		// A run under a scatter/gather aggregate (or a gather combine) is refused this
		// slice — only a top-level run, at any run-nesting depth, lowers. (A run that is
		// a sub-formula's own statement has a non-empty parent but inAggregate=false, so
		// nested runs stay allowed.)
		return fmt.Errorf("%w: %q %q nested under an aggregate; run is top-level only in this slice", ErrUnsupportedNode, n.Kind, n.ID)
	}
	// Closed-payload guards: the agent override and detached-run forms are deferred;
	// only the transparent, by-name form lowers this slice.
	if _, ok := n.Raw["with"]; ok {
		return fmt.Errorf("%w: run %q with-agent override (node %q)", ErrUnsupportedNode, n.ID, n.ID)
	}
	if _, ok := n.Raw["runInput"]; ok {
		return fmt.Errorf("%w: run %q runInput form (node %q)", ErrUnsupportedNode, n.ID, n.ID)
	}
	var outcome string
	if raw, ok := n.Raw["outcome"]; ok {
		_ = json.Unmarshal(raw, &outcome)
	}
	if outcome != "transparent" {
		return fmt.Errorf("%w: run %q outcome %q (only transparent)", ErrUnsupportedNode, n.ID, outcome)
	}
	target, err := runTargetName(n)
	if err != nil {
		return err
	}
	sub, ok := l.formulas[target]
	if !ok {
		return fmt.Errorf("lumen: run %q targets formula %q not present in the document's formulas bundle", n.ID, target)
	}
	if sub == nil {
		return fmt.Errorf("lumen: run %q target formula %q is null in the bundle", n.ID, target)
	}
	for _, t := range l.targetStack {
		if t == target {
			return fmt.Errorf("lumen: run %q: recursive formula cycle %s", n.ID, strings.Join(append(append([]string(nil), l.targetStack...), target), " -> "))
		}
	}

	env, envRefs, err := decodeRunEnv(n, sub.Input.Fields)
	if err != nil {
		return err
	}
	spec := &runSpec{target: target, env: env, envRefs: envRefs, inputFields: sub.Input.Fields}

	qRunID := l.qid(n.ID)
	qAfter := l.qAfter(n.After)
	runAct := activationFor(qRunID)
	nodeNS := l.prefix // the namespace the run node itself lives in (parent scope)

	// Inline the sub-graph one namespace deeper. The sub-formula's internal `after`
	// refs qualify under the new prefix, so they resolve strictly within it.
	savedPrefix, savedStack := l.prefix, l.targetStack
	l.prefix = qRunID + "/"
	// Fresh backing array per push: appending to savedStack directly could alias the
	// same array across sibling runs (spare capacity), corrupting a later sibling's
	// cycle stack. Copy so each nesting level owns its slice.
	l.targetStack = append(append([]string(nil), savedStack...), target)
	firstUnit := len(l.units)
	if err := l.lowerNodes(sub.Nodes, runAct); err != nil {
		l.prefix, l.targetStack = savedPrefix, savedStack
		return err
	}
	l.prefix, l.targetStack = savedPrefix, savedStack

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
// top-level or a scatter member (like a leaf); a guard inside a run sub-formula
// renders/decides against the root scope this slice — refuse it when prefix != "".
func (l *lowerer) lowerGuard(n ir.Node, parent string) error {
	if l.prefix != "" {
		return fmt.Errorf("%w: %q %q inside a run sub-formula (decision scope is namespace-unaware this slice)", ErrUnsupportedNode, n.Kind, n.ID)
	}
	cond, ok := n.Raw["cond"]
	if !ok {
		return fmt.Errorf("%w: guard %q missing cond", ErrUnsupportedNode, n.ID)
	}
	if err := validateClosedExpr(cond); err != nil {
		return err
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
	condRefs := collectRefs(cond)
	// A cond that references the guard's own id or its `then` is self-referential — the
	// guard/then have not settled when the decision is made, so the ref folds to null on
	// genesis but could flip on a resume that reloaded the then. Refuse it at load.
	for _, r := range condRefs {
		if r == n.ID || r == then.ID {
			return fmt.Errorf("%w: guard %q cond references its own %s (self-referential decision)", ErrUnsupportedNode, n.ID, r)
		}
	}
	spec := &guardSpec{cond: cond, condRefs: condRefs, thenNodeID: l.qid(then.ID), thenIRKind: then.Kind}
	switch then.Kind {
	case ir.NodeExec:
		s, err := decodeExec(then)
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

// lowerDispatch lowers a dispatch node — a multi-way branch — to a unitDispatch. The
// subject is a value expression; each arm has a match literal and a SINGLE leaf body
// (exec/do). A non-leaf arm body (block/run/scatter) is refused this slice — a
// dispatch-to-run composition is a follow-on. Like guard, a dispatch inside a run
// sub-formula (prefix != "") is refused (namespace-unaware decision), and a subject
// that reads the dispatch's own id or an arm body id is refused (self-referential).
func (l *lowerer) lowerDispatch(n ir.Node, parent string) error {
	if l.prefix != "" {
		return fmt.Errorf("%w: %q %q inside a run sub-formula (decision scope is namespace-unaware this slice)", ErrUnsupportedNode, n.Kind, n.ID)
	}
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
			s, err := decodeExec(body)
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
		default:
			return fmt.Errorf("%w: dispatch %q arm %q body kind %q (only exec/do leaf arm bodies)", ErrUnsupportedNode, n.ID, matchVal, body.Kind)
		}
		spec.arms = append(spec.arms, arm)
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
// they read (for the DET sub-graph gating in resolveDeps).
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
		refs = append(refs, collectRefs(f.Value)...)
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
	// qualifies its ids identically, and a `run` combine member is refused by lowerRun's
	// top-level check (parent != "").
	sub := &lowerer{
		allowDo:        l.allowCombineDo,
		allowCombineDo: l.allowCombineDo,
		formulas:       l.formulas,
		prefix:         l.prefix,
		targetStack:    l.targetStack,
		inAggregate:    true, // combine members run under an aggregate: run/loop refused
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
		case unitScatterAgg, unitRun:
			u.memberDeps = append([]string(nil), u.members...)
		case unitGather:
			if u.overScatter != "" {
				u.memberDeps = []string{u.overScatter}
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
				exprRefs = u.dispatch.subjectRefs
			}
		default:
			continue
		}
		var gates []string
		for _, refName := range exprRefs {
			act, ok := byNodeID[u.ns+refName]
			if !ok {
				continue // a ref to an input (not a node) is not a gate
			}
			// A silent (lit/interp) ref node never settles, so gating on it directly
			// would defer forever on the Advance path. Substitute its transitive
			// non-silent closure — the real nodes the silent value derives from — exactly
			// like the H2 after-dep rule. A pure constant (empty closure) adds no gate.
			for _, g := range nonSilentClosure(act, silent, rawDeps) {
				if g != u.activation {
					gates = append(gates, g)
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

	// Synthesized decision-arm bodies (a guard `then`, dispatch arm bodies) are NOT
	// plan units, so topoSortUnits' duplicate-activation guard cannot see them. A body
	// id that collides with a real unit's node id or with another synthesized body id
	// would collide on activationFor(bodyID) — corrupting the write-once decision
	// record (dispatch chosenArm) and the Tier-A projection. Refuse it loudly.
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
	return s, nil
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

// decodeExec lifts an exec node's interpreter/body/exitMap into a step.
func decodeExec(n ir.Node) (step, error) {
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
