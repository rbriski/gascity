package engine

import (
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// TestPendingFlowsThroughReducerDefaults is the reducerVersion-STAYS-4 pin for the
// pending outcome: `pending` is a new VALUE in the already-folded nodeState.Outcome
// string, NOT a new folded field, so every reducer predicate must hit its EXISTING
// default arm for "pending" — identical to what an old v4 reducer would do folding a
// pending journal. Mutation (iii) — making ranOutcome true for pending — turns the
// pool nodeOutputs-pollution behavioral pin RED; this test locks the predicate defaults
// that make the determinism argument hold.
func TestPendingFlowsThroughReducerDefaults(t *testing.T) {
	if isBlocking(OutcomePending) {
		t.Error("isBlocking(pending) = true, want false (a pending poll is non-blocking)")
	}
	if ranOutcome(OutcomePending) {
		t.Error("ranOutcome(pending) = true, want false (a poll did not RAN — it must be excluded from loopScope + record, so nodeOutputs is never polluted)")
	}
	if didNotRun(OutcomePending) {
		t.Error("didNotRun(pending) = true, want false (pending is neither skipped nor canceled)")
	}
	if got := statusForOutcome(OutcomePending); got != "done" {
		t.Errorf("statusForOutcome(pending) = %q, want done (default projection arm, StateHash-transparent)", got)
	}
	if got := Reducer().ReducerVersion(); got != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4 (pending is a new outcome VALUE, not a new folded field)", got)
	}
}

// TestApplyOutcomeSettledStoresPendingVerbatim proves applyOutcomeSettled folds a
// `pending` outcome into nodeState.Outcome verbatim with no branch on the value, drops
// the settling activation out of the frontier, and does NOT flip the run outcome to a
// failure (runOutcome ignores a parented attempt). This is the fold-side half of the
// "reducer does not change" argument.
func TestApplyOutcomeSettledStoresPendingVerbatim(t *testing.T) {
	base := &lumenState{
		RootID:    "gcg-pending",
		Name:      "pend",
		CreatedAt: "2026-07-13T00:00:00Z",
		Nodes: map[string]*nodeState{
			"draft:0": {NodeID: "draft", ParentActivation: "repeat_1:0"},
		},
	}
	body, err := canonPayload(outcomeSettledPayload{Activation: "draft:0", Outcome: OutcomePending, Output: "still running"})
	if err != nil {
		t.Fatalf("canon payload: %v", err)
	}
	next, delta, err := lumenReducer{}.Apply(base, fold.Event{
		StreamID: base.RootID, Seq: 3, Engine: Engine, Type: EventOutcomeSettled, Payload: body,
	})
	if err != nil {
		t.Fatalf("Apply outcome.settled(pending): %v", err)
	}
	st := next.(*lumenState)
	n := st.Nodes["draft:0"]
	if !n.Settled || n.Outcome != OutcomePending {
		t.Fatalf("draft:0 = {settled:%v outcome:%q}, want {true pending}", n.Settled, n.Outcome)
	}
	if n.InFrontier {
		t.Error("a settled pending attempt stayed in the frontier")
	}
	// A parented attempt never reaches run aggregation, so the run outcome is the empty
	// (no top-level settled) pass — a pending attempt is NOT a run failure.
	if st.Outcome == OutcomeFailed {
		t.Error("a pending attempt flipped the run outcome to failed (runOutcome must ignore a parented attempt)")
	}
	// The settle projects a frontier delete for the bare id, like any other settle.
	if len(delta.FrontierDelete) == 0 || delta.FrontierDelete[0] != "draft" {
		t.Errorf("FrontierDelete = %v, want [draft ...]", delta.FrontierDelete)
	}
}

// TestConsumingCountBeforeSemantics is the direct unit test of the consuming-vs-physical
// decoupling. It pins: (1) a NON-pending settled run has consumingCountBefore == k for
// every k (so iteration == attempt+1 byte-identically — the load-bearing regression
// equivalence); (2) pending settles are SKIPPED while the physical index still advances;
// (3) unsettled and other-node attempts are ignored. Mutation (i) — reverting the loop
// iteration to attempt+1 — is what these numbers diverge from once a pending appears.
func TestConsumingCountBeforeSemantics(t *testing.T) {
	// (1) Non-pending: draft:0 pass, draft:1 failed, draft:2 pass — consuming == k.
	nonPending := map[string]NodeStateForTest{
		"draft:0": {Settled: true, Outcome: OutcomePass},
		"draft:1": {Settled: true, Outcome: OutcomeFailed},
		"draft:2": {Settled: true, Outcome: OutcomePass},
	}
	for k := 0; k <= 3; k++ {
		if got := ConsumingCountBeforeForTest(nonPending, "draft", k); got != k {
			t.Errorf("non-pending consumingCountBefore(draft, %d) = %d, want %d (iteration == attempt+1 equivalence)", k, got, k)
		}
	}

	// (2) Pending polls skipped, physical index still advances: draft:0..2 pending, draft:3 failed.
	withPending := map[string]NodeStateForTest{
		"draft:0": {Settled: true, Outcome: OutcomePending},
		"draft:1": {Settled: true, Outcome: OutcomePending},
		"draft:2": {Settled: true, Outcome: OutcomePending},
		"draft:3": {Settled: true, Outcome: OutcomeFailed},
	}
	for k := 0; k <= 3; k++ {
		if got := ConsumingCountBeforeForTest(withPending, "draft", k); got != 0 {
			t.Errorf("pending-only consumingCountBefore(draft, %d) = %d, want 0 (polls never consume the budget)", k, got)
		}
	}
	if got := ConsumingCountBeforeForTest(withPending, "draft", 4); got != 1 {
		t.Errorf("consumingCountBefore(draft, 4) = %d, want 1 (only the failed attempt consumed)", got)
	}

	// (3) Unsettled attempts and a different node id are ignored.
	mixed := map[string]NodeStateForTest{
		"draft:0": {Settled: true, Outcome: OutcomePass},
		"draft:1": {Settled: false, Outcome: ""}, // in flight — ignored
		"other:0": {Settled: true, Outcome: OutcomePass},
	}
	if got := ConsumingCountBeforeForTest(mixed, "draft", 5); got != 1 {
		t.Errorf("consumingCountBefore(draft, 5) over mixed = %d, want 1 (unsettled + other-node ignored)", got)
	}
}

// TestPhysicalCapDefaultSizing pins the production physical-cap sizing at the design's
// 512 recommendation (owner-review flag). A change to the default is a policy decision,
// caught here.
func TestPhysicalCapDefaultSizing(t *testing.T) {
	if got := LoopPhysicalCap(); got != 512 {
		t.Fatalf("lumenLoopPhysicalCap = %d, want 512 (owner-sized poll bound; change is a policy decision)", got)
	}
}
