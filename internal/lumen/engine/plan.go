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
	silent      bool // pure lit/interp: compute scope, emit no journal events

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
func buildUnits(nodes []ir.Node, allowDo, allowCombineDo bool) ([]planUnit, error) {
	l := &lowerer{allowDo: allowDo, allowCombineDo: allowCombineDo, scatterMembers: map[string][]string{}}
	if err := l.lowerNodes(nodes, ""); err != nil {
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
	scatterMembers map[string][]string // scatter node id -> member activation keys (in order)
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

	default:
		// P4.2-deferred: async, await, cancel, channel, cleanup, close, dispatch,
		// fail-channel, for-each(scatter form:each), guard, map, quote, raise,
		// recover, run, timeout. Vocabulary + reducer transitions exist (total
		// fold); executor arms land in later slices. Filed follow-up: blueprint §7
		// P4.2 corpus TODO. (retry/repeat land as the L5 attempt-loop arm above.)
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
	scatterAct := activationFor(n.ID)
	firstUnit := len(l.units)
	var memberActs []string
	for i := range members {
		idx := i
		if err := l.lowerNode(members[i], scatterAct, &idx); err != nil {
			return err
		}
	}
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
	l.scatterMembers[n.ID] = memberActs

	// Propagate the scatter's own `after` gate onto every unit lowered beneath it
	// (H1): a scatter gated on a failed dependency must not run any member. The
	// aggregate itself is gated separately below (its afterDeps). Descendants
	// inherit the gate so a nested scatter's leaves skip-cascade too.
	if len(n.After) > 0 {
		for i := firstUnit; i < len(l.units); i++ {
			l.units[i].rawAfter = append(l.units[i].rawAfter, n.After...)
		}
	}

	onFail := "continue"
	if raw, ok := n.Raw["on_fail"]; ok {
		_ = json.Unmarshal(raw, &onFail)
	}
	l.units = append(l.units, planUnit{
		kind:       unitScatterAgg,
		activation: scatterAct,
		nodeID:     n.ID,
		irKind:     ir.NodeScatter,
		parent:     parent,
		rawAfter:   n.After,
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
	memberActs, ok := l.scatterMembers[overName]
	if !ok {
		return fmt.Errorf("lumen: gather %q references unknown scatter %q", n.ID, overName)
	}
	combine, err := l.lowerCombine(n)
	if err != nil {
		return err
	}
	l.units = append(l.units, planUnit{
		kind:          unitGather,
		activation:    activationFor(n.ID),
		nodeID:        n.ID,
		irKind:        ir.NodeGather,
		parent:        parent,
		rawAfter:      n.After,
		overScatter:   activationFor(overName),
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
	if parent != "" {
		// A loop nested under a scatter/gather aggregate is a non-leaf member; refuse
		// it this slice (scatter/combine-nested loops are a follow-up, §7.7). Top-level
		// loops (possibly inside transparent blocks) carry parent "".
		return fmt.Errorf("%w: %q %q nested under an aggregate; loops are top-level only in this slice", ErrUnsupportedNode, n.Kind, n.ID)
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
	spec.bodyNodeID = body.ID
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
		activation: activationFor(n.ID),
		nodeID:     n.ID,
		irKind:     n.Kind,
		parent:     parent,
		rawAfter:   n.After,
		loop:       spec,
	})
	return nil
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
	gatherAct := activationFor(n.ID)
	// A combine runs inline inside runGather, so a `do` combine member needs a Host
	// (allowCombineDo), NOT merely a PoolRouter: the top-level pool-materialization
	// walk never reaches a combine member. Gating the sub-lowerer's do arm on
	// allowCombineDo refuses a combine `do` (at any nesting depth) HERE at lowering
	// with ErrUnsupportedNode, before the lease is taken or any event is appended —
	// never as a late hard fail after the drained members ran (M2).
	sub := &lowerer{allowDo: l.allowCombineDo, allowCombineDo: l.allowCombineDo, scatterMembers: map[string][]string{}}
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
		// addClosure adds a non-silent dependency directly; for a silent dep it
		// recurses into that dep's own dependencies, substituting its transitive
		// non-silent closure. guard breaks any (degenerate) silent cycle so the walk
		// terminates; a silent cycle has no settleable input, so it contributes none.
		var addClosure func(act string, guard map[string]bool)
		addClosure = func(act string, guard map[string]bool) {
			if act == "" {
				return
			}
			if silent[act] {
				if guard[act] {
					return
				}
				guard[act] = true
				for _, d := range rawDeps[act] {
					addClosure(d, guard)
				}
				return
			}
			if seen[act] {
				return
			}
			seen[act] = true
			afterDeps = append(afterDeps, act)
		}
		for _, dep := range rawDeps[u.activation] {
			// The scatter a gather drains is a drain dependency (memberDeps below),
			// not a blocking gate — the whole point of a gather is to collect the
			// scatter regardless of its outcome.
			if u.kind == unitGather && dep == u.overScatter {
				continue
			}
			addClosure(dep, map[string]bool{})
		}
		u.afterDeps = afterDeps

		switch u.kind {
		case unitScatterAgg:
			u.memberDeps = append([]string(nil), u.members...)
		case unitGather:
			if u.overScatter != "" {
				u.memberDeps = []string{u.overScatter}
			}
		}
	}
	return nil
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
		activation:  activationFor(n.ID),
		nodeID:      n.ID,
		irKind:      n.Kind,
		parent:      parent,
		memberIndex: memberIndex,
		silent:      silent,
		rawAfter:    n.After,
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
