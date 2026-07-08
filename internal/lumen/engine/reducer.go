package engine

import (
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// lumenReducer is the pure, total fold from the Lumen event stream to the
// graph projection (nodes / edges / frontier) — reducer v2 (blueprint §2). It
// performs no I/O and reads no clock: every timestamp it projects comes from an
// event payload (run.started carries created_at, threaded onto every node row).
//
// The DAG is carried IN the journal: node.activated events name their
// dependency edges (as activation keys), so the fold builds readiness purely
// from the log with no external IR lookup (D-P4-1). The frontier is deps-settled
// readiness WITH skip-cascade: a node whose dependency settled with a blocking
// outcome (failed / canceled / skipped) never becomes ready, and the executor
// settles it `skipped` rather than running it.
type lumenReducer struct{}

var _ fold.Reducer = lumenReducer{}

// Reducer returns the Lumen fold reducer (v2). It is the reducer a store uses to
// rebuild or resume a lumen stream, and the one tests fold goldens through.
func Reducer() fold.Reducer { return lumenReducer{} }

// Engine reports the engine tag.
func (lumenReducer) Engine() string { return Engine }

// ReducerVersion reports the stamped reducer version (v2).
func (lumenReducer) ReducerVersion() int { return reducerVersion }

// Zero returns the empty fold state. The stream id is read from each event
// rather than carried in state, so resume stays clean at covered_seq == 0.
func (lumenReducer) Zero(string) fold.State { return &lumenState{} }

// UnmarshalSnapshot deserializes a lumenState blob.
func (lumenReducer) UnmarshalSnapshot(formatVersion int, b []byte) (fold.State, error) {
	if formatVersion != snapshotFormatVersion {
		return nil, fmt.Errorf("lumen: snapshot format %d, want %d", formatVersion, snapshotFormatVersion)
	}
	var s lumenState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("lumen: unmarshal snapshot: %w", err)
	}
	return &s, nil
}

// Apply is the pure transition. It never mutates the input state: it clones,
// mutates the clone, and returns it with the projection delta. Structurally
// impossible sequences (a settlement before the run started) return a typed
// error — journal corruption, never ordinary control flow.
func (lumenReducer) Apply(s fold.State, e fold.Event) (fold.State, fold.Delta, error) {
	prev, ok := s.(*lumenState)
	if !ok {
		return nil, fold.Delta{}, fmt.Errorf("lumen: state is %T, want *lumenState", s)
	}
	next := prev.clone()

	switch e.Type {
	case EventRunStarted:
		return applyRunStarted(next, e)
	case EventNodeActivated:
		return applyNodeActivated(next, e)
	case EventOutcomeSettled:
		return applyOutcomeSettled(next, e)
	case EventRunClosed:
		return applyRunClosed(next, e)

	// Bookkeeping and not-yet-emitting arms: total transitions with no Tier-A
	// delta. They guard the run has started (corruption detection) and fold to a
	// no-op projection, so the fold is total (R-TOTAL) over the frozen vocabulary
	// and an emitting executor arm can be added without touching the reducer's
	// version gate for these.
	case EventNodeDecision, EventEffectScheduled, EventEffectSettled,
		EventAttemptMinted, EventChannelOpened, EventChannelEmit,
		EventChannelCursorPlanted, EventChannelCursorAdvanced, EventChannelSealed,
		EventCancelRequested, EventCancelSwept, EventOwnedAdmitted,
		EventOwnedSettled, EventSnapshotAnchored:
		if next.RootID == "" {
			return nil, fold.Delta{}, fmt.Errorf("lumen: %s at seq %d before run.started", e.Type, e.Seq)
		}
		return next, fold.Delta{}, nil

	default:
		return nil, fold.Delta{}, fmt.Errorf("lumen: unknown event type %q at seq %d", e.Type, e.Seq)
	}
}

func applyRunStarted(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	var p runStartedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: run.started payload at seq %d: %w", e.Seq, err)
	}
	if p.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: run.started at seq %d missing root_id", e.Seq)
	}
	next.RootID = p.RootID
	next.Name = p.Name
	next.CreatedAt = p.CreatedAt
	next.IRHash = p.IRHash
	next.InputHash = p.InputHash
	delta := fold.Delta{
		NodeUpserts: []fold.NodeRow{{
			ID:          p.RootID,
			Title:       p.Name,
			Status:      "open",
			BeadType:    "run",
			CreatedAt:   p.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
		}},
		FrontierInsert: []fold.FrontierRow{{
			NodeID:        p.RootID,
			RootID:        e.StreamID,
			ReadyPriority: 2,
			CreatedAt:     p.CreatedAt,
			ID:            p.RootID,
		}},
	}
	return next, delta, nil
}

func applyNodeActivated(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: node.activated at seq %d before run.started", e.Seq)
	}
	var p nodeActivatedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: node.activated payload at seq %d: %w", e.Seq, err)
	}
	if p.Activation == "" || p.NodeID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: node.activated at seq %d missing activation/node_id", e.Seq)
	}
	if next.Nodes == nil {
		next.Nodes = map[string]*nodeState{}
	}
	n := &nodeState{
		NodeID:           p.NodeID,
		Kind:             p.Kind,
		ParentActivation: p.ParentActivation,
		MemberIndex:      p.MemberIndex,
		After:            append([]string(nil), p.After...),
		Members:          append([]string(nil), p.Members...),
	}
	next.Nodes[p.Activation] = n

	parentID := next.RootID
	if p.ParentActivation != "" {
		parentID = activationNodeID(p.ParentActivation)
	}
	delta := fold.Delta{
		NodeUpserts: []fold.NodeRow{{
			ID:          p.NodeID,
			Title:       p.NodeID,
			Status:      "open",
			BeadType:    "step",
			ParentID:    parentID,
			CreatedAt:   next.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
			Metadata:    map[string]string{"kind": p.Kind, "activation": p.Activation},
		}},
	}
	// Edges: one per dependency, keyed on bare node ids. In topo order the
	// dependency's node row already exists, so the edge FK resolves. Both blocking
	// `after` gates and drain member edges are projected so the DAG in Tier-A is
	// complete (a scatter aggregate carries edges from its members).
	for _, dep := range n.After {
		delta.EdgeUpserts = append(delta.EdgeUpserts, fold.EdgeRow{
			FromID:  activationNodeID(dep),
			ToID:    p.NodeID,
			DepType: "after",
		})
	}
	for _, m := range n.Members {
		delta.EdgeUpserts = append(delta.EdgeUpserts, fold.EdgeRow{
			FromID:  activationNodeID(m),
			ToID:    p.NodeID,
			DepType: "member",
		})
	}
	if next.ready(n) {
		n.InFrontier = true
		delta.FrontierInsert = []fold.FrontierRow{frontierRowFor(next, p.Activation)}
	}
	return next, delta, nil
}

func applyOutcomeSettled(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: outcome.settled at seq %d before run.started", e.Seq)
	}
	var p outcomeSettledPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: outcome.settled payload at seq %d: %w", e.Seq, err)
	}
	if p.Activation == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: outcome.settled at seq %d missing activation", e.Seq)
	}
	if next.Nodes == nil {
		next.Nodes = map[string]*nodeState{}
	}
	n := next.Nodes[p.Activation]
	if n == nil {
		// Lazy node: an upcast P1 stream (or any stream) that settles an
		// activation without a preceding node.activated. Parent defaults to the
		// root, matching the P1 projection so P1 journals fold identically.
		n = &nodeState{NodeID: activationNodeID(p.Activation)}
		next.Nodes[p.Activation] = n
	}
	n.Settled = true
	n.Outcome = p.Outcome
	n.Output = p.Output
	n.InFrontier = false

	parentID := next.RootID
	if n.ParentActivation != "" {
		parentID = activationNodeID(n.ParentActivation)
	}
	delta := fold.Delta{
		NodeUpserts: []fold.NodeRow{{
			ID:          n.NodeID,
			Title:       n.NodeID,
			Status:      statusForOutcome(p.Outcome),
			BeadType:    "step",
			ParentID:    parentID,
			CreatedAt:   next.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
			Metadata:    map[string]string{"outcome": p.Outcome, "output": p.Output},
		}},
		FrontierDelete: []string{p.Activation},
	}
	// Readiness propagation: a dependent that just became ready enters the
	// frontier; a dependent still blocked (this settle was blocking) stays out —
	// the executor settles it skipped (the skip-cascade).
	//
	// NOTE (N1): this frontier insert feeds the Tier-A projection for external
	// observers only. The single-writer executor drives its own decide loop in
	// topo order and never SELECTs this frontier, so as an execution-control path
	// it is currently dead — it goes live when a claim/serve surface reads the
	// frontier (P4.5).
	for _, depKey := range next.dependentsOf(p.Activation) {
		d := next.Nodes[depKey]
		if d.InFrontier || d.Settled {
			continue
		}
		if next.ready(d) {
			d.InFrontier = true
			delta.FrontierInsert = append(delta.FrontierInsert, frontierRowFor(next, depKey))
		}
	}
	next.Outcome = next.runOutcome()
	return next, delta, nil
}

func applyRunClosed(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: run.closed at seq %d before run.started", e.Seq)
	}
	var p runClosedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: run.closed payload at seq %d: %w", e.Seq, err)
	}
	next.Closed = true
	next.Outcome = p.Outcome
	delta := fold.Delta{
		NodeUpserts: []fold.NodeRow{{
			ID:          next.RootID,
			Title:       next.Name,
			Status:      statusForOutcome(p.Outcome),
			BeadType:    "run",
			CreatedAt:   next.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
		}},
	}
	// Clear the whole frontier for the stream: the root plus any activation that
	// was left ready. Deleting an absent node_id is a no-op, so an upcast P1
	// journal (no leaf frontier rows) clears exactly the root — identical to v1.
	delta.FrontierDelete = append([]string{next.RootID}, next.activationKeys()...)
	return next, delta, nil
}

// ProjectDelta renders the FULL Tier-A projection of the carried state as a
// single fold delta: the root node, every activation's node/edge rows, and the
// live frontier. It is the fold.SnapshotProjector capability RebuildTierA uses to
// reconstruct a retention-truncated stream's covered prefix from its snapshot
// state — the journal prefix is gone, but the snapshot captures its cumulative
// projection. It mirrors, over the carried state, exactly what applyRunStarted /
// applyNodeActivated / applyOutcomeSettled emit incrementally (and what
// applyRunClosed clears), so a projected-from-state prefix plus the folded
// surviving tail reproduces the pre-truncation projection byte-for-byte (H1). A
// snapshot is always anchored at a unit boundary before run.closed, so the state
// it renders is never mid-effect.
func (s *lumenState) ProjectDelta(streamID string) fold.Delta {
	var delta fold.Delta

	// Root (applyRunStarted, upgraded to closed by applyRunClosed). It carries no
	// metadata and no parent; it sits in the frontier only while the run is open.
	rootStatus := "open"
	if s.Closed {
		rootStatus = statusForOutcome(s.Outcome)
	}
	delta.NodeUpserts = append(delta.NodeUpserts, fold.NodeRow{
		ID:          s.RootID,
		Title:       s.Name,
		Status:      rootStatus,
		BeadType:    "run",
		CreatedAt:   s.CreatedAt,
		StorageTier: "history",
		StreamID:    streamID,
	})
	if !s.Closed {
		delta.FrontierInsert = append(delta.FrontierInsert, frontierRowFor(s, s.RootID))
	}

	for _, act := range s.activationKeys() {
		n := s.Nodes[act]
		parentID := s.RootID
		if n.ParentActivation != "" {
			parentID = activationNodeID(n.ParentActivation)
		}
		// A settled node carries its outcome/output metadata (the applyOutcomeSettled
		// upsert replaces the activated {kind, activation} set); an activated-only
		// node carries {kind, activation}. Empty metadata values clear their key at
		// the applier, matching the incremental fold exactly.
		status := "open"
		var meta map[string]string
		if n.Settled {
			status = statusForOutcome(n.Outcome)
			meta = map[string]string{"outcome": n.Outcome, "output": n.Output}
		} else {
			meta = map[string]string{"kind": n.Kind, "activation": act}
		}
		delta.NodeUpserts = append(delta.NodeUpserts, fold.NodeRow{
			ID:          n.NodeID,
			Title:       n.NodeID,
			Status:      status,
			BeadType:    "step",
			ParentID:    parentID,
			CreatedAt:   s.CreatedAt,
			StorageTier: "history",
			StreamID:    streamID,
			Metadata:    meta,
		})
		for _, dep := range n.After {
			delta.EdgeUpserts = append(delta.EdgeUpserts, fold.EdgeRow{
				FromID: activationNodeID(dep), ToID: n.NodeID, DepType: "after",
			})
		}
		for _, m := range n.Members {
			delta.EdgeUpserts = append(delta.EdgeUpserts, fold.EdgeRow{
				FromID: activationNodeID(m), ToID: n.NodeID, DepType: "member",
			})
		}
		// A closed run clears the whole frontier (applyRunClosed), so no frontier row
		// survives regardless of a node's stale in-state InFrontier flag.
		if n.InFrontier && !s.Closed {
			delta.FrontierInsert = append(delta.FrontierInsert, frontierRowFor(s, act))
		}
	}
	return delta
}

// frontierRowFor builds the Tier-A frontier row for an activation. The row id
// is the activation key (unique within a run), which the frontier index orders
// deterministically after (route, ready_priority, created_at).
func frontierRowFor(s *lumenState, activation string) fold.FrontierRow {
	return fold.FrontierRow{
		NodeID:        activation,
		RootID:        s.RootID,
		ReadyPriority: 2,
		CreatedAt:     s.CreatedAt,
		ID:            activation,
	}
}
