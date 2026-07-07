package engine

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
)

// lumenState is the reducer v2 carried-forward state (blueprint §2.1): the run
// identity (all timestamps sourced from payloads, keeping the fold clock-free)
// plus the DAG of activations. Nodes is keyed by activation; every map walk is
// in canonical (sorted) key order so the fold is deterministic (R-PURE).
type lumenState struct {
	RootID    string                `json:"root_id"`
	Name      string                `json:"name"`
	CreatedAt string                `json:"created_at"`
	IRHash    string                `json:"ir_hash,omitempty"`
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
	InFrontier       bool     `json:"in_frontier,omitempty"`
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

// ActivationNodeID is the exported bare-node-id derivation for projection
// consumers (e.g. the `gc run` summary) that read outcome.settled activations.
func ActivationNodeID(activation string) string { return activationNodeID(activation) }
