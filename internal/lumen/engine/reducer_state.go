package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// lumenState is the reducer v4 carried-forward state (blueprint §2.1): the run
// identity (all timestamps sourced from payloads, keeping the fold clock-free)
// plus the DAG of activations. Nodes is keyed by activation; every map walk is
// in canonical (sorted) key order so the fold is deterministic (R-PURE).
type lumenState struct {
	RootID    string                `json:"root_id"`
	Name      string                `json:"name"`
	CreatedAt string                `json:"created_at"`
	IRHash    string                `json:"ir_hash,omitempty"`
	InputHash string                `json:"input_hash,omitempty"`
	Closed    bool                  `json:"closed"`
	Outcome   string                `json:"outcome,omitempty"`
	Nodes     map[string]*nodeState `json:"nodes,omitempty"`
}

// nodeState is one activation's fold state: its identity, its dependency edges
// (activation keys — the DAG carried by node.activated events), and its
// settlement. InFrontier mirrors the Tier-A frontier so the fold can emit the
// minimal insert/delete deltas without re-querying.
type nodeState struct {
	NodeID           string   `json:"node_id"`
	Kind             string   `json:"kind,omitempty"`
	ParentActivation string   `json:"parent,omitempty"`
	MemberIndex      *int     `json:"member_index,omitempty"`
	After            []string `json:"after,omitempty"`
	Members          []string `json:"members,omitempty"`
	Settled          bool     `json:"settled"`
	Outcome          string   `json:"outcome,omitempty"`
	Output           string   `json:"output,omitempty"`
	// Detail is the settle's failure/skip detail (a settle's authored reason, a skip
	// message, invalid_input, …), folded from outcome.settled so a recover catch can
	// bind {{ error.reason }} from a failed guarded. omitempty; NOT projected to Tier-A
	// (only carried in the fold state) — but because it is non-empty on pre-existing
	// settles it is the reason reducerVersion bumped to 4.
	Detail     string `json:"detail,omitempty"`
	InFrontier bool   `json:"in_frontier,omitempty"`
	// Retryable is the L5 attempt-loop classification folded from a settle: an
	// engine-inline exec settle carrying exit ∈ exitMap.retryable, or a pool settle
	// the firewall marked a retryable infrastructure strand. The retry arm reads it
	// from the fold (not driver memory) to decide a re-attempt, so a re-Advance /
	// resume re-derives the same decision. omitempty + a bool ⇒ clone() copies it by
	// value and a non-retryable node serializes exactly as pre-L5 (DET-T-17).
	Retryable bool `json:"retryable,omitempty"`
	// DispatchMode is the pool-dispatch marker (DispatchModePool for a node whose
	// work is dispatched as an ordinary bead in the city work store). omitempty, so
	// an engine-driven node serializes exactly as it did pre-pool.
	DispatchMode string `json:"dispatch_mode,omitempty"`
	// Route and Prompt are the pool-dispatch fields (dispatch_mode=pool only): the
	// dispatched work bead's gc.routed_to and Description. Carried in state so a
	// drop+refold re-dispatches byte-identically (DET-T-17). Both omitempty, so an
	// engine-driven node re-marshals exactly as pre-pool.
	Route  string `json:"route,omitempty"`
	Prompt string `json:"prompt,omitempty"`
	// BeadID is the pool-dispatched do's store-minted work-bead id (REDESIGN §1.3),
	// folded ONCE from the owned.admitted{kind:work_bead} dispatch fact. It is the
	// two-way join to the real bead in the city work store — the actionable work
	// lives there, not in this fold row — and the observer settles the fold from that
	// bead's terminal close. omitempty, so an engine-driven node serializes as pre-pool.
	BeadID string `json:"bead_id,omitempty"`
}

// clone deep-copies the state so Apply never mutates its input (R-PURE).
func (s *lumenState) clone() *lumenState {
	c := *s
	if s.Nodes != nil {
		c.Nodes = make(map[string]*nodeState, len(s.Nodes))
		for k, v := range s.Nodes {
			nv := *v
			if v.After != nil {
				nv.After = append([]string(nil), v.After...)
			}
			if v.Members != nil {
				nv.Members = append([]string(nil), v.Members...)
			}
			if v.MemberIndex != nil {
				mi := *v.MemberIndex
				nv.MemberIndex = &mi
			}
			c.Nodes[k] = &nv
		}
	}
	return &c
}

// MarshalSnapshot returns the R-CANON serialization of the state.
func (s *lumenState) MarshalSnapshot() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("lumen: marshal state: %w", err)
	}
	return canon.Canonicalize(raw)
}

// StateHash is the SHA-256 over the canonical serialization.
func (s *lumenState) StateHash() [32]byte {
	b, err := s.MarshalSnapshot()
	if err != nil {
		panic(fmt.Sprintf("lumen: StateHash: %v", err))
	}
	return canon.Hash(b)
}

// isBlocking reports whether an outcome blocks its dependents from running (and
// triggers the skip-cascade). failed, canceled, and skipped are blocking;
// pass and degraded drain through. Making skipped blocking is what makes the
// skip-cascade transitive (A fails → B skipped → C skipped).
func isBlocking(outcome string) bool {
	switch outcome {
	case OutcomeFailed, OutcomeCanceled, OutcomeSkipped:
		return true
	default:
		return false
	}
}

// didNotRun reports whether a settled member did NO work — skipped or canceled.
// It is deliberately distinct from isBlocking, which also includes `failed` (a
// member that DID run and lost): a drain aggregate over an all-didNotRun member
// set never ran and itself SKIPS (N-1); a single member that ran
// (pass/degraded/failed) makes the aggregate DRAIN.
func didNotRun(outcome string) bool {
	return outcome == OutcomeSkipped || outcome == OutcomeCanceled
}

// ranOutcome reports whether a settled outcome means the unit actually RAN
// (pass / degraded / failed), as opposed to skip-cascading (skipped / canceled).
// It is the genesis record() predicate: runLeaf/runScatter/runGather record a
// unit's output into scope/nodeOutputs, but a unit intercepted by blocked() or
// aggregateAllSkipped() settles WITHOUT recording. Resume reproduces this rule
// exactly so a resumed run's interpolation scope matches genesis (B1).
func ranOutcome(outcome string) bool {
	switch outcome {
	case OutcomePass, OutcomeDegraded, OutcomeFailed:
		return true
	default:
		return false
	}
}

// ready reports whether an activation is frontier-ready: activated, not
// settled, every blocking `after` gate settled with a non-blocking outcome, and
// its drain members settled with AT LEAST ONE having actually run. The drain
// exception (H1) is scoped to member dependencies only — a scatter/gather's
// non-member `after` gate blocks (and skip-cascades) exactly like any other
// node's. An aggregate whose every drain member settled skipped/canceled did no
// work: it is NOT ready — it skip-cascades and settles `skipped` (N-1), it does
// not drain-and-run its combine. A single ran member makes it ready to drain.
func (s *lumenState) ready(n *nodeState) bool {
	if n.Settled {
		return false
	}
	for _, dep := range n.After {
		d := s.Nodes[dep]
		if d == nil || !d.Settled {
			return false
		}
		if isBlocking(d.Outcome) {
			return false
		}
	}
	if len(n.Members) > 0 {
		anyRan := false
		for _, m := range n.Members {
			d := s.Nodes[m]
			if d == nil || !d.Settled {
				return false
			}
			if !didNotRun(d.Outcome) {
				anyRan = true
			}
		}
		if !anyRan {
			return false
		}
	}
	return true
}

// outcomeOf returns a dependency's settled outcome, and whether it has settled.
func (s *lumenState) outcomeOf(activation string) (outcome string, settled bool) {
	if n, ok := s.Nodes[activation]; ok && n.Settled {
		return n.Outcome, true
	}
	return "", false
}

// dependentsOf returns, in canonical order, the activation keys of every node
// that lists `activation` among its dependencies — blocking `after` gates or
// drain members alike, so a settling member re-evaluates its aggregate's
// readiness.
func (s *lumenState) dependentsOf(activation string) []string {
	var deps []string
	for k, n := range s.Nodes {
		if containsStr(n.After, activation) || containsStr(n.Members, activation) {
			deps = append(deps, k)
		}
	}
	sort.Strings(deps)
	return deps
}

// containsStr reports whether xs contains x.
func containsStr(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// activationKeys returns every activation key in canonical order.
func (s *lumenState) activationKeys() []string {
	keys := make([]string, 0, len(s.Nodes))
	for k := range s.Nodes {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// runOutcome aggregates the run's outcome over TOP-LEVEL activations (parent ==
// root, i.e. ParentActivation == ""): failed dominates, then degraded, then
// pass; skipped nodes do not count, and members/combine children inside a
// scatter or gather drain into their aggregate rather than the run. An empty
// run passes.
func (s *lumenState) runOutcome() string {
	var anyFailed, anyDegraded, anySettled bool
	for _, k := range s.activationKeys() {
		n := s.Nodes[k]
		if n.ParentActivation != "" || !n.Settled {
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

// statusForOutcome maps a settled outcome onto a projected node status.
func statusForOutcome(outcome string) string {
	switch outcome {
	case OutcomeFailed, OutcomeCanceled:
		return "failed"
	case OutcomeSkipped:
		return "skipped"
	default:
		return "done"
	}
}

// nodeProjectedStatus is the Tier-A `nodes.status` a fold projects for n. It is
// shared by the incremental appliers and ProjectDelta so an incremental fold and
// a drop+refold project byte-identical rows (DET-T-17). A settled node takes its
// outcome status; everything else is open. There is no fold-side "claimed" status
// anymore — a pool node's real work bead carries its own claim lifecycle in the
// city work store, invisible to the fold (REDESIGN).
func nodeProjectedStatus(n *nodeState) string {
	if n.Settled {
		return statusForOutcome(n.Outcome)
	}
	return "open"
}

// frontierEligible reports whether an activated, ready node projects a Tier-A
// frontier row. A pool-dispatched node never does (REDESIGN): its work is an
// ordinary bead in the city work store — the only claim surface — so a fold
// frontier row would be a bd-ready doppelganger. Engine-driven nodes and the run
// root populate the frontier exactly as before, so a non-pool node's projection is
// byte-identical.
func frontierEligible(n *nodeState) bool {
	return n.DispatchMode != DispatchModePool
}

// nodeProjectedMeta is the Tier-A `node_metadata` a fold projects for the
// activation act in state n, shared by the incremental appliers and ProjectDelta.
// A settled node carries {outcome, output}; an unsettled node carries
// {kind, activation}. A pool-dispatched node additionally carries {bead_id} while
// unsettled — the two-way join to its real work bead in the city work store
// (REDESIGN observability only); the bead itself owns the claim-routing metadata
// (gc.routed_to) and claim lifecycle, not this fold row. An empty value clears its
// key at the applier, matching the incremental fold.
func nodeProjectedMeta(act string, n *nodeState) map[string]string {
	if n.Settled {
		return map[string]string{"outcome": n.Outcome, "output": n.Output}
	}
	meta := map[string]string{"kind": n.Kind, "activation": act}
	if n.BeadID != "" {
		meta["bead_id"] = n.BeadID
	}
	return meta
}

// nodeRowFor builds the Tier-A node upsert for the activation act in state s,
// the single source of truth for a step node's projected row. The incremental
// appliers (activated / claimed / settled) and ProjectDelta all route through it,
// so the incremental fold and a drop+refold are byte-identical (DET-T-17).
func nodeRowFor(s *lumenState, act string, n *nodeState, streamID string) fold.NodeRow {
	parentID := s.RootID
	if n.ParentActivation != "" {
		parentID = activationNodeID(n.ParentActivation)
	}
	// Every step node — engine-driven OR pool-dispatched — projects a plain `step`
	// row with no description (REDESIGN). The actionable pool work is the ordinary
	// bead in the city work store, which carries the prompt in its Description; a
	// task-typed fold row here would be a bd-ready doppelganger of it.
	return fold.NodeRow{
		ID:          n.NodeID,
		Title:       n.NodeID,
		Status:      nodeProjectedStatus(n),
		BeadType:    "step",
		ParentID:    parentID,
		CreatedAt:   s.CreatedAt,
		StorageTier: "history",
		StreamID:    streamID,
		Metadata:    nodeProjectedMeta(act, n),
	}
}

// activationNodeID derives the bare node id from an activation key
// (nodeID + ":" + index). The node id never contains ':', so the trailing
// index segment is stripped. It is what the Tier-A `nodes` projection keys on,
// so a v2-written stream and an upcast P1 stream project the SAME node ids.
//
// NOTE (N3): this assumes a node id contains no ':' (LastIndex strips only the
// trailing index segment). The IR compiler does not currently emit colon-bearing
// ids; if that ever changes, activation keying needs a delimiter that cannot
// appear in a node id.
func activationNodeID(activation string) string {
	if i := strings.LastIndex(activation, ":"); i >= 0 {
		return activation[:i]
	}
	return activation
}

// activationAttempt parses the trailing attempt index from an activation key
// (nodeID + ":" + attempt). It is the numeric-ordering companion to
// activationNodeID: the retry/repeat arm and reconstructOutputs use it to pick
// the highest-numbered attempt of a node id, where a lexicographic sort would
// wrongly rank "b:10" below "b:2". An absent (a bare stream-id run root) or
// non-numeric trailing segment is attempt 0 — the single-attempt and legacy
// shapes.
func activationAttempt(activation string) int {
	i := strings.LastIndex(activation, ":")
	if i < 0 {
		return 0
	}
	n, err := strconv.Atoi(activation[i+1:])
	if err != nil {
		return 0
	}
	return n
}

// ActivationNodeID is the exported bare-node-id derivation for projection
// consumers (e.g. the `gc run` summary) that read outcome.settled activations.
func ActivationNodeID(activation string) string { return activationNodeID(activation) }
