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

	// Tier-B claim-as-append (P4.5): a claim is a CAS owned.admitted, a close an
	// owned.settled — both with kind=tier_b. The projection (assignee/status) is a
	// pure fold of these events. A non-Tier-B owned handle (async / detached_run)
	// stays deferred no-op bookkeeping inside these arms.
	case EventOwnedAdmitted:
		return applyOwnedAdmitted(next, e)
	case EventOwnedSettled:
		return applyOwnedSettled(next, e)

	// Bookkeeping and not-yet-emitting arms: total transitions with no Tier-A
	// delta. They guard the run has started (corruption detection) and fold to a
	// no-op projection, so the fold is total (R-TOTAL) over the frozen vocabulary
	// and an emitting executor arm can be added without touching the reducer's
	// version gate for these.
	case EventNodeDecision, EventEffectScheduled, EventEffectSettled,
		EventAttemptMinted, EventChannelOpened, EventChannelEmit,
		EventChannelCursorPlanted, EventChannelCursorAdvanced, EventChannelSealed,
		EventCancelRequested, EventCancelSwept, EventSnapshotAnchored:
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
		if next.ready(d) {
			d.InFrontier = true
			delta.FrontierInsert = append(delta.FrontierInsert, frontierRowFor(next, depKey))
		}
	}
	next.Outcome = next.runOutcome()
	return next, delta, nil
}

// applyOwnedAdmitted folds a Tier-B claim (P4.5): a worker admitted an owned
// work handle (kind=tier_b) — the claim-as-append the JournalStore's beads.Store
// claim write translates into. The projection sets the node's assignee and moves
// it to StatusClaimed (in_progress) and out of the frontier — a PURE FOLD of the
// event, never a raw column write. A non-Tier-B handle (async / detached_run)
// remains deferred no-op bookkeeping. Loud-CAS one-winner selection lives in the
// append (tier_b_claim.go): the reducer only reflects the committed fact.
//
// The fold is TOTAL (R-TOTAL): every legal appended event folds to a DEFINED
// state, so it can NEVER return an error that breaks RebuildTierA/Resume. A claim
// that cannot take effect — an unactivated / non-pool / already-settled handle, or
// an empty assignee — folds to a no-op ("late claim loses"): the prior fact stands
// and the ineffective claim leaves the projection unchanged. Only genuinely
// structural corruption (an event before run.started, or an unparseable payload)
// is a typed error, consistent with the rest of the reducer.
func applyOwnedAdmitted(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.admitted at seq %d before run.started", e.Seq)
	}
	var p ownedAdmittedPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.admitted payload at seq %d: %w", e.Seq, err)
	}
	if p.Kind != OwnedKindTierB {
		// A non-Tier-B owned handle (async / detached_run) is deferred no-op
		// bookkeeping. kind is the Tier-B discriminant, symmetric with the settle
		// arm (MED-3), so an async admit whose handle collides with a pool
		// activation can never fold as a claim.
		return next, fold.Delta{}, nil
	}
	if p.Handle == "" || p.Assignee == "" {
		// A handle-less or assignee-less claim holds the bead for nobody: it cannot
		// take effect, so it folds to a no-op — the node stays claimable, never
		// orphaned (LOW-2).
		return next, fold.Delta{}, nil
	}
	n := next.Nodes[p.Handle]
	if n == nil || n.DispatchMode != DispatchModePool || n.Settled {
		// Late claim loses (HIGH-1 totality): an unactivated / non-pool / terminal
		// handle cannot be claimed. The prior fact (an unclaimed settle, an
		// engine-driven node) stands and the claim is recorded as ineffective — a
		// DEFINED no-op, never an error that would poison the stream forever. The
		// losing worker still learns it lost: the API-level guard/CAS in
		// ClaimTierBWork rejects it loudly.
		return next, fold.Delta{}, nil
	}
	n.Assignee = p.Assignee
	n.InFrontier = false
	return next, fold.Delta{
		NodeUpserts:    []fold.NodeRow{nodeRowFor(next, p.Handle, n, e.StreamID)},
		FrontierDelete: []string{activationNodeID(p.Handle)},
	}, nil
}

// applyOwnedSettled folds a Tier-B close (P4.5): the worker settled a claimed
// work handle. It mirrors applyOutcomeSettled — the node settles to its outcome
// status (assignee retained for provenance), leaves the frontier, and propagates
// readiness so the settle drives the rest of the run's DAG. A handle that is not
// a claimable Tier-B node (a deferred async / detached_run handle) is no-op
// bookkeeping.
//
// The fold is TOTAL (R-TOTAL), symmetric with applyOwnedAdmitted: kind is the
// Tier-B discriminant (an async/detached settle whose handle collides with a pool
// activation folds as deferred bookkeeping, never a Tier-B settle; MED-3), and a
// settle that cannot take effect — an unactivated / non-pool / already-settled
// handle, or a missing outcome — folds to a DEFINED idempotent no-op. The first
// settle stands; a divergent re-settle is rejected loudly at the append (idem
// token), and reaching the fold with a redundant settle (a re-sliced or crafted
// stream) must never error and poison RebuildTierA/Resume.
func applyOwnedSettled(next *lumenState, e fold.Event) (fold.State, fold.Delta, error) {
	if next.RootID == "" {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.settled at seq %d before run.started", e.Seq)
	}
	var p ownedSettledPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return nil, fold.Delta{}, fmt.Errorf("lumen: owned.settled payload at seq %d: %w", e.Seq, err)
	}
	if p.Kind != OwnedKindTierB {
		return next, fold.Delta{}, nil
	}
	n := next.Nodes[p.Handle]
	if n == nil || n.DispatchMode != DispatchModePool || n.Settled || p.Outcome == "" {
		return next, fold.Delta{}, nil
	}
	n.Settled = true
	n.Outcome = p.Outcome
	n.Output = p.Output
	n.InFrontier = false
	delta := fold.Delta{
		NodeUpserts:    []fold.NodeRow{nodeRowFor(next, p.Handle, n, e.StreamID)},
		FrontierDelete: []string{activationNodeID(p.Handle)},
	}
	for _, depKey := range next.dependentsOf(p.Handle) {
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

	for _, act := range s.activationKeys() {
		n := s.Nodes[act]
		// nodeRowFor is the single source of truth for a step node's projected row
		// (status/assignee/metadata across activated → claimed → settled), so this
		// full-state projection matches the incremental fold byte-for-byte (H1).
		delta.NodeUpserts = append(delta.NodeUpserts, nodeRowFor(s, act, n, streamID))
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
// root-scoped. readTierBNode already scopes its READ by stream_id (MED-2); the
// write side (this row + the FrontierDelete sites in applyOutcomeSettled /
// applyOwnedSettled / applyRunClosed) is the remaining half.
//
// frontierRowFor builds the Tier-A frontier row for an activation. Its node_id is
// the BARE node id (activationNodeID), NOT the activation key: the frontier is a
// claim surface, and the claimable-pool-work SELECT (frontierProjectionTier,
// internal/beads/journal_frontier.go) reads `frontier.node_id` and hydrates
// `nodes WHERE id IN (...)` — so the frontier node_id must equal nodes.id or the
// row hydrates to nothing. The root's activation is the stream id (no ':' suffix),
// so it is already bare. A pool-mode node carries its route so the
// frontier_route_order index (route, ready_priority, created_at, id) IS the
// claim SELECT: rows with route=<pool> are exactly the open+ready+unassigned pool
// set. The run root and engine-driven nodes carry route "" and never match a pool
// SELECT. (One activation per node in P4.2, so the bare id is unique per run; a
// per-attempt frontier key is a retry-slice concern, blueprint correction #3.)
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
