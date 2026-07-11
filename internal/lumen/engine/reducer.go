package engine

import (
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// lumenReducer is the pure, total fold from the Lumen event stream to the
// graph projection (nodes / edges / frontier) — reducer v3 (blueprint §2). It
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

// Reducer returns the Lumen fold reducer (v3). It is the reducer a store uses to
// rebuild or resume a lumen stream, and the one tests fold goldens through.
func Reducer() fold.Reducer { return lumenReducer{} }

// Engine reports the engine tag.
func (lumenReducer) Engine() string { return Engine }

// ReducerVersion reports the stamped reducer version (v3).
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

	// owned.admitted carries the real-bead do path's dispatch fact
	// (kind=work_bead); every other owned kind folds as deferred no-op bookkeeping.
	case EventOwnedAdmitted:
		return applyOwnedAdmitted(next, e)

	// Bookkeeping and not-yet-emitting arms: total transitions with no Tier-A
	// delta. They guard the run has started (corruption detection) and fold to a
	// no-op projection, so the fold is total (R-TOTAL) over the frozen vocabulary
	// and an emitting executor arm can be added without touching the reducer's
	// version gate for these. owned.settled is here: it settles no real-bead do (a
	// do settles through outcome.settled); it is registered for the deferred
	// async/detached-run await boundary and folds as a no-op until that lands.
	case EventNodeDecision, EventEffectScheduled, EventEffectSettled,
		EventAttemptMinted, EventChannelOpened, EventChannelEmit,
		EventChannelCursorPlanted, EventChannelCursorAdvanced, EventChannelSealed,
		EventCancelRequested, EventCancelSwept, EventSnapshotAnchored, EventOwnedSettled:
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
		DispatchMode:     p.DispatchMode,
		Route:            p.Route,
		Prompt:           p.Prompt,
	}
	next.Nodes[p.Activation] = n

	delta := fold.Delta{
		NodeUpserts: []fold.NodeRow{nodeRowFor(next, p.Activation, n, e.StreamID)},
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
	if next.ready(n) && frontierEligible(n) {
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
	n.Detail = p.Detail
	n.Retryable = p.Retryable
	n.InFrontier = false

	delta := fold.Delta{
		NodeUpserts:    []fold.NodeRow{nodeRowFor(next, p.Activation, n, e.StreamID)},
		FrontierDelete: []string{activationNodeID(p.Activation)},
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
		if next.ready(d) && frontierEligible(d) {
			d.InFrontier = true
			delta.FrontierInsert = append(delta.FrontierInsert, frontierRowFor(next, depKey))
		}
	}
	next.Outcome = next.runOutcome()
	return next, delta, nil
}

// applyOwnedAdmitted folds an owned.admitted. The only live kind is
// OwnedKindWorkBead (the real-bead do path's dispatch fact — see
// applyWorkBeadDispatched); every other owned kind (async / detached_run) is
// deferred no-op bookkeeping.
//
// The fold is TOTAL (R-TOTAL): every legal appended event folds to a DEFINED
// state, so it can NEVER return an error that breaks RebuildTierA/Resume. Only
// genuinely structural corruption (an event before run.started, or an unparseable
// payload) is a typed error, consistent with the rest of the reducer.
func applyOwnedAdmitted(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.admitted at seq %d before run.started", e.Seq)
	}
	var p ownedAdmittedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.admitted payload at seq %d: %w", e.Seq, err)
	}
	if p.Kind == OwnedKindWorkBead {
		return applyWorkBeadDispatched(next, e, p)
	}
	// A non-work-bead owned handle (async / detached_run) is deferred no-op
	// bookkeeping.
	return next, fold.Delta{}, nil
}

// applyWorkBeadDispatched folds the real-bead do path's dispatch fact (REDESIGN
// §1.3): the driver created an ordinary fold_owned=0 work bead in the city work
// store for a ready pool-mode do and recorded its store-minted id. The fold records
// n.BeadID, drops the node out of the claimable frontier, and re-projects it as a
// PLAIN step (nodeRowFor keys the task→step flip on BeadID) so the fold row stops
// being a bd-ready doppelganger of the real bead. NO assignee, NO fold-side claim —
// the worker claims the ordinary bead through the native pool path, invisible to the
// fold.
//
// The fold is TOTAL (R-TOTAL): a dispatch naming an unactivated / non-pool /
// already-settled handle, or an empty bead id, folds to a DEFINED no-op — never an
// error that would poison RebuildTierA/Resume. Recording is write-once: a handle
// that already carries a BeadID folds idempotently (the dispatch idem token is
// write-once at the append, and a refold replays the single recorded fact).
func applyWorkBeadDispatched(next *lumenState, e fold.Event, p ownedAdmittedPayload) (fold.State, fold.Delta, error) {
	if p.Handle == "" || p.BeadID == "" {
		return next, fold.Delta{}, nil
	}
	n := next.Nodes[p.Handle]
	if n == nil || n.DispatchMode != DispatchModePool || n.Settled || n.BeadID != "" {
		return next, fold.Delta{}, nil
	}
	n.BeadID = p.BeadID
	n.InFrontier = false
	return next, fold.Delta{
		NodeUpserts:    []fold.NodeRow{nodeRowFor(next, p.Handle, n, e.StreamID)},
		FrontierDelete: []string{activationNodeID(p.Handle)},
	}, nil
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
	// Clear the whole frontier for the stream: the root plus every activation's
	// projected frontier node_id (the BARE node id — frontier rows key by nodes.id,
	// not the activation key). Deleting an absent node_id is a no-op, so an upcast
	// P1 journal (no leaf frontier rows) clears exactly the root — identical to v1.
	frontierDelete := []string{next.RootID}
	for _, act := range next.activationKeys() {
		frontierDelete = append(frontierDelete, activationNodeID(act))
	}
	delta.FrontierDelete = frontierDelete
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

	// A retry/repeat loop re-attempts one bare node id across activations b:0…b:N,
	// so several activations share a projected node id. Only the HIGHEST-numbered
	// attempt owns the bare-id row: attempts are sequential, so the incremental
	// fold's last temporal upsert of the id is the max attempt, and this full-state
	// projection must agree. Selecting by the NUMERIC attempt (not the lexical
	// activationKeys order, where "b:10" < "b:2") keeps ProjectDelta byte-identical
	// to the incremental deltas (DET-T-17) even past ten attempts. Single-attempt
	// nodes have max 0 and are unaffected.
	maxAttempt := map[string]int{}
	for act, n := range s.Nodes {
		if a := activationAttempt(act); a > maxAttempt[n.NodeID] {
			maxAttempt[n.NodeID] = a
		}
	}
	for _, act := range s.activationKeys() {
		n := s.Nodes[act]
		// nodeRowFor is the single source of truth for a step node's projected row
		// (status/assignee/metadata across activated → claimed → settled), so this
		// full-state projection matches the incremental fold byte-for-byte (H1). Only
		// the max-attempt activation projects the shared bare-id row.
		if activationAttempt(act) == maxAttempt[n.NodeID] {
			delta.NodeUpserts = append(delta.NodeUpserts, nodeRowFor(s, act, n, streamID))
		}
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

// L4-BLOCKER (run-namespaced projected ids required before multi-run pools):
// nodes.id and frontier.node_id are GLOBAL primary keys, but executor node ids are
// IR-local (not run-namespaced), and FrontierDelete keys on the bare node_id
// WITHOUT a root_id scope. So two concurrent runs of the SAME IR (identical node
// ids) would collide in Tier-A — one run's settle deleting the other run's frontier
// row, one run's node upsert clobbering the other's. Single-run L3 is unaffected
// (one run owns its node ids). Before multi-run pools land, the projected node id
// must be run-namespaced (e.g. streamID-scoped) and FrontierDelete must be
// root-scoped (the write side: this row + the FrontierDelete sites in
// applyOutcomeSettled / applyRunClosed). Nothing claims off the Tier-A projection
// anymore (the real do work is an ordinary city-store bead — REDESIGN), so the
// residual blast radius is a cosmetic observability clobber, not claim corruption.
//
// frontierRowFor builds the Tier-A frontier row for an activation. Its node_id is
// the BARE node id (activationNodeID), NOT the activation key, so a downstream
// hydrate (`nodes WHERE id IN (...)`) resolves. The frontier is now an
// observer-only projection of open, ready, engine-driven work and the run root: a
// pool-dispatched node never enters it (frontierEligible is false — its work bead
// in the city work store is the only claim surface), so the Route column here is
// always "" for the rows that do project. (One activation per node in P4.2, so the
// bare id is unique per run; a per-attempt frontier key is a retry-slice concern,
// blueprint correction #3.)
func frontierRowFor(s *lumenState, activation string) fold.FrontierRow {
	route := ""
	if n := s.Nodes[activation]; n != nil {
		route = n.Route
	}
	nodeID := activationNodeID(activation)
	return fold.FrontierRow{
		NodeID:        nodeID,
		RootID:        s.RootID,
		Route:         route,
		ReadyPriority: 2,
		CreatedAt:     s.CreatedAt,
		ID:            nodeID,
	}
}
