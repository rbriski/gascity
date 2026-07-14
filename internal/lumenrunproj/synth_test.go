package lumenrunproj

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/runproj"
)

// twoNodeView is a chain A→B: A settled pass (closed), B still running (a live
// do bead in_progress). It exercises the settled/live status overlay, the
// dependency edge, and the per-step session link in one fixture.
func twoNodeView() (engine.RunView, map[string]beads.Bead) {
	view := engine.RunView{
		RootID:     "gcg-run-synth",
		Name:       "My Lumen Run",
		FormulaRef: "my.lumen",
		CreatedAt:  "2026-07-14T00:00:00Z",
		Closed:     false,
		Activations: []engine.RunActivationView{
			{Activation: "A:0", NodeID: "A", Attempt: 0, Settled: true, Outcome: engine.OutcomePass},
			{Activation: "B:0", NodeID: "B", Attempt: 0, Settled: false, After: []string{"A:0"}},
		},
	}
	created := time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC)
	do := map[string]beads.Bead{
		"A:0": {
			ID: "wb-A", Status: "closed", Assignee: "agent-a",
			CreatedAt: created, UpdatedAt: created.Add(time.Minute),
			Metadata: beads.StringMap{
				beadmeta.LumenRunMetadataKey:        "gcg-run-synth",
				beadmeta.LumenActivationMetadataKey: "A:0",
				beadmeta.SessionIDMetadataKey:       "sess-a",
				beadmeta.SessionNameMetadataKey:     "pool-a",
			},
		},
		"B:0": {
			ID: "wb-B", Status: "in_progress", Assignee: "agent-b",
			CreatedAt: created.Add(time.Minute), UpdatedAt: created.Add(2 * time.Minute),
			Metadata: beads.StringMap{
				beadmeta.LumenRunMetadataKey:        "gcg-run-synth",
				beadmeta.LumenActivationMetadataKey: "B:0",
				beadmeta.SessionIDMetadataKey:       "sess-b",
				beadmeta.SessionNameMetadataKey:     "pool-b",
			},
		},
	}
	return view, do
}

// TestSyntheticBeadsSummaryLane proves the synthetic bead graph folds through
// runproj.BuildRunSummary into exactly one lane carrying the run identity,
// formula, and NON-empty status counts (the counts-wall check: a hand-built DTO
// could not populate these).
func TestSyntheticBeadsSummaryLane(t *testing.T) {
	view, do := twoNodeView()
	beadList := syntheticBeads(view, do, "mycity")

	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(beadList))
	lane, ok := findLane(summary, "gcg-run-synth")
	if !ok {
		t.Fatalf("no lane for the run; lanes=%d historical=%d blocked=%d",
			len(summary.Lanes), len(summary.HistoricalLanes), len(summary.BlockedLanes))
	}
	if lane.Title != "My Lumen Run" {
		t.Fatalf("lane.Title = %q, want %q", lane.Title, "My Lumen Run")
	}
	if lane.Formula.Status != "known" || lane.Formula.Name != "my.lumen" {
		t.Fatalf("lane.Formula = %+v, want known/my.lumen", lane.Formula)
	}
	if lane.Scope.Status != "available" || lane.Scope.Kind != "city" {
		t.Fatalf("lane.Scope = %+v, want available/city", lane.Scope)
	}
	// Non-empty status counts prove the counts wall is cleared (BuildRunSummary
	// populated the unexported StatusCounts a foreign package cannot).
	raw, err := lane.StatusCounts.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal statusCounts: %v", err)
	}
	if string(raw) == "{}" {
		t.Fatalf("lane.StatusCounts empty — expected a populated count (A closed, B in_progress)")
	}
}

// TestSyntheticBeadsDetailIdentityGraph proves BuildRunDetailForRun reconstructs
// the run detail as identity: N activations → N display nodes, the A→B edge as a
// dependency, and the running node's session link resolved.
func TestSyntheticBeadsDetailIdentityGraph(t *testing.T) {
	view, do := twoNodeView()
	beadList := syntheticBeads(view, do, "mycity")

	detail, _, _, _, _, _, err := runproj.BuildRunDetailForRun(
		beadList, "gcg-run-synth", 1, 0, nil, nil, runproj.FormulaDetailUpstreamError)
	if err != nil {
		t.Fatalf("BuildRunDetailForRun: %v", err)
	}
	if detail.RunID != "gcg-run-synth" {
		t.Fatalf("detail.RunID = %q, want gcg-run-synth", detail.RunID)
	}
	// Identity: the 2 activations reconstruct as 2 step display nodes (no
	// collapse/rename), plus runproj's own run-root node → 3 total.
	if len(detail.Nodes) != 3 {
		t.Fatalf("detail.Nodes = %d (%+v), want 3 (run-root + A + B identity)", len(detail.Nodes), nodeIDs(detail.Nodes))
	}

	nodeByStep := map[string]runproj.RunDisplayNode{}
	for _, n := range detail.Nodes {
		nodeByStep[n.SemanticNodeID] = n
	}
	if _, ok := nodeByStep["A"]; !ok {
		t.Fatalf("no display node A; got %+v", nodeIDs(detail.Nodes))
	}
	nodeB, ok := nodeByStep["B"]
	if !ok {
		t.Fatalf("no display node B; got %+v", nodeIDs(detail.Nodes))
	}

	// The A→B dependency edge survived as a "dependency" kind, correct direction.
	foundDep := false
	for _, e := range detail.Edges {
		if e.Kind == "dependency" && e.From == "A" && e.To == "B" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Fatalf("no A→B dependency edge; edges=%+v", detail.Edges)
	}

	// B is running with a live do bead → its execution instance resolves a
	// session link.
	if len(nodeB.ExecutionInstances) == 0 {
		t.Fatalf("node B has no execution instances")
	}
	if nodeB.ExecutionInstances[0].Session.Kind != "attached" {
		t.Fatalf("node B session = %+v, want attached (live do bead carries gc.session_id)",
			nodeB.ExecutionInstances[0].Session)
	}
}

// TestDoBeadsAloneProduceNoLane pins the no-duplicate-lane guarantee: the
// events-fold do beads for a run (which carry gc.root_bead_id = the stream id
// from P5-OBS.1) group under the stream id but have no bead with that id, so
// isDanglingRootGroup drops them — no lane. The single lane a Lumen run shows is
// the adapter's, never a duplicate from the events fold. If this ever regresses
// (e.g. a future slice emits a bead-shaped run root), the merge would double the
// lane.
func TestDoBeadsAloneProduceNoLane(t *testing.T) {
	doBeads := []beads.Bead{
		{ID: "wb-A", Type: "task", Status: "closed", Title: "A", Metadata: beads.StringMap{
			beadmeta.LumenRunMetadataKey:        "gcg-run-synth",
			beadmeta.LumenActivationMetadataKey: "A:0",
			beadmeta.RootBeadIDMetadataKey:      "gcg-run-synth", // P5-OBS.1 stamp
			beadmeta.StepIDMetadataKey:          "A",
		}},
		{ID: "wb-B", Type: "task", Status: "in_progress", Title: "B", Metadata: beads.StringMap{
			beadmeta.LumenRunMetadataKey:        "gcg-run-synth",
			beadmeta.LumenActivationMetadataKey: "B:0",
			beadmeta.RootBeadIDMetadataKey:      "gcg-run-synth",
			beadmeta.StepIDMetadataKey:          "B",
		}},
	}
	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(doBeads))
	if _, ok := findLane(summary, "gcg-run-synth"); ok {
		t.Fatal("events-fold do beads alone produced a lane — the dangling-root drop regressed; the Lumen merge would now double the lane")
	}
}

// TestSyntheticBeadsStatusMapping pins the synthetic-bead spec (FINAL-SPEC §2):
// every settled activation → status "closed" (never a raw outcome, which would
// break mapRunPhase's exact-match complete detection); outcome badges for
// fail/skip/degrade; 1-based gc.attempt (numericFieldRe rejects "0"); the closed
// run root carries the failure badge; and no forbidden disambiguation metadata
// leaks onto a step.
func TestSyntheticBeadsStatusMapping(t *testing.T) {
	view := engine.RunView{
		RootID: "gcg-s", Name: "r", FormulaRef: "f.lumen", CreatedAt: "2026-07-14T00:00:00Z",
		Closed: true, Outcome: engine.OutcomeFailed,
		Activations: []engine.RunActivationView{
			{Activation: "P:0", NodeID: "P", Attempt: 0, Settled: true, Outcome: engine.OutcomePass},
			{Activation: "F:0", NodeID: "F", Attempt: 0, Settled: true, Outcome: engine.OutcomeFailed},
			{Activation: "S:0", NodeID: "S", Attempt: 0, Settled: true, Outcome: engine.OutcomeSkipped},
			{Activation: "D:0", NodeID: "D", Attempt: 0, Settled: true, Outcome: engine.OutcomeDegraded},
			{Activation: "O:0", NodeID: "O", Attempt: 0, Settled: false},
			{Activation: "R:1", NodeID: "R", Attempt: 1, Settled: true, Outcome: engine.OutcomePass},
		},
	}
	beadList := syntheticBeads(view, nil, "c1")
	byStep := map[string]beads.Bead{}
	var root beads.Bead
	for _, b := range beadList {
		if b.ID == "gcg-s" {
			root = b
			continue
		}
		byStep[b.Metadata[beadmeta.StepIDMetadataKey]] = b
	}

	for _, step := range []string{"P", "F", "S", "D"} {
		if byStep[step].Status != "closed" {
			t.Fatalf("settled step %s status = %q, want closed (raw outcome would break mapRunPhase complete)", step, byStep[step].Status)
		}
	}
	if byStep["O"].Status != "open" {
		t.Fatalf("unsettled step O status = %q, want open", byStep["O"].Status)
	}
	badges := map[string]string{"P": "", "F": engine.OutcomeFailed, "S": engine.OutcomeSkipped, "D": engine.OutcomeDegraded}
	for step, want := range badges {
		if got := byStep[step].Metadata[beadmeta.OutcomeMetadataKey]; got != want {
			t.Fatalf("step %s outcome badge = %q, want %q", step, got, want)
		}
	}
	// 1-based attempt: attempt 0 → "1", attempt 1 → "2".
	if got := byStep["P"].Metadata[beadmeta.AttemptMetadataKey]; got != "1" {
		t.Fatalf("attempt-0 step gc.attempt = %q, want 1 (1-based)", got)
	}
	if got := byStep["R"].Metadata[beadmeta.AttemptMetadataKey]; got != "2" {
		t.Fatalf("attempt-1 step gc.attempt = %q, want 2 (1-based)", got)
	}
	if root.Status != "closed" {
		t.Fatalf("closed run root status = %q, want closed", root.Status)
	}
	if root.Metadata[beadmeta.OutcomeMetadataKey] != engine.OutcomeFailed {
		t.Fatalf("failed run root outcome badge = %q, want failed", root.Metadata[beadmeta.OutcomeMetadataKey])
	}
	for _, forbidden := range []string{
		beadmeta.IterationMetadataKey, beadmeta.LogicalBeadIDMetadataKey,
		beadmeta.ScopeRefMetadataKey, beadmeta.StepRefMetadataKey,
		beadmeta.KindMetadataKey, beadmeta.ControlForMetadataKey,
	} {
		if _, ok := byStep["P"].Metadata[forbidden]; ok {
			t.Fatalf("step bead carries forbidden disambiguation metadata %q", forbidden)
		}
	}
}

// TestSyntheticBeadsClosedRunIsHistorical proves a fully-settled run folds into a
// HistoricalLane with phase "complete" — the integration-level guard for the
// status-vocabulary rule (a settled step returning a raw outcome instead of
// "closed" would strand every sealed run in Active forever).
func TestSyntheticBeadsClosedRunIsHistorical(t *testing.T) {
	view := engine.RunView{
		RootID: "gcg-h", Name: "done run", FormulaRef: "f.lumen", CreatedAt: "2026-07-14T00:00:00Z",
		Closed: true, Outcome: engine.OutcomePass,
		Activations: []engine.RunActivationView{
			{Activation: "A:0", NodeID: "A", Settled: true, Outcome: engine.OutcomePass},
		},
	}
	summary := runproj.BuildRunSummary(runproj.FilterRunBeads(syntheticBeads(view, nil, "c1")))
	lane, ok := findLane(summary, "gcg-h")
	if !ok {
		t.Fatalf("sealed run produced no lane; active=%d historical=%d", len(summary.Lanes), len(summary.HistoricalLanes))
	}
	if lane.Phase != "complete" {
		t.Fatalf("sealed run lane phase = %q, want complete", lane.Phase)
	}
	found := false
	for _, l := range summary.HistoricalLanes {
		if l.ID == "gcg-h" {
			found = true
		}
	}
	if !found {
		t.Fatal("sealed run lane is not in HistoricalLanes")
	}
}

// TestSyntheticBeadsMemberEdge pins the Members → "member" edge path (a
// scatter/drain aggregate), which no other fixture exercises.
func TestSyntheticBeadsMemberEdge(t *testing.T) {
	view := engine.RunView{
		RootID: "gcg-m", Name: "r", FormulaRef: "f.lumen", CreatedAt: "2026-07-14T00:00:00Z",
		Activations: []engine.RunActivationView{
			{Activation: "m1:0", NodeID: "m1", Settled: true, Outcome: engine.OutcomePass},
			{Activation: "G:0", NodeID: "G", Settled: false, Members: []string{"m1:0"}},
		},
	}
	detail, _, _, _, _, _, err := runproj.BuildRunDetailForRun(
		syntheticBeads(view, nil, "c1"), "gcg-m", 1, 0, nil, nil, runproj.FormulaDetailUpstreamError)
	if err != nil {
		t.Fatalf("BuildRunDetailForRun: %v", err)
	}
	found := false
	for _, e := range detail.Edges {
		if e.Kind == "member" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no member edge from the drain aggregate; edges=%+v", detail.Edges)
	}
}

func findLane(s runproj.RunSummary, id string) (runproj.RunLane, bool) {
	for _, group := range [][]runproj.RunLane{s.Lanes, s.HistoricalLanes, s.BlockedLanes} {
		for _, l := range group {
			if l.ID == id {
				return l, true
			}
		}
	}
	return runproj.RunLane{}, false
}

func nodeIDs(nodes []runproj.RunDisplayNode) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.SemanticNodeID
	}
	return out
}
