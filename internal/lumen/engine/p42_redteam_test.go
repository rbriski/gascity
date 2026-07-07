package engine_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- node builders for the DAG arms ----------------------------------------

// scatterNode renders a scatter(form:members) node.
func scatterNode(id string, after []string, onFail string, members ...string) string {
	afterJSON, _ := json.Marshal(after)
	return `{
      "kind": "scatter", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "form": "members",
      "members": [` + strings.Join(members, ",") + `],
      "on_fail": "` + onFail + `"
    }`
}

// gatherNode renders a gather(authored) node over the scatter named `over`, with
// an authored combine block containing the given member nodes.
func gatherNode(id, over string, after []string, combineMembers ...string) string {
	afterJSON, _ := json.Marshal(after)
	return `{
      "kind": "gather", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "over": {"kind": "ref", "name": "` + over + `", "origin": {"uri": "t", "line": 1, "col": 0}},
      "combine": {"kind": "authored", "block": {
        "kind": "block", "id": "` + id + `.body", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
        "members": [` + strings.Join(combineMembers, ",") + `]
      }}
    }`
}

// settleNode renders a top-level (no `after`) settle node with a fixed outcome.
func settleNode(id, outcome string) string {
	return `{
      "kind": "settle", "id": "` + id + `", "name": "` + id + `", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0}, "outcome": "` + outcome + `"
    }`
}

// activatedMembers returns the `members` (drain deps) of an activation's
// node.activated event.
func activatedMembers(t *testing.T, events []graphstore.StoredEvent, activation string) []string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string   `json:"activation"`
			Members    []string `json:"members"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if p.Activation == activation {
			return p.Members
		}
	}
	t.Fatalf("no node.activated for %q", activation)
	return nil
}

// --- H1: drain exception must not leak to non-member `after` deps -----------

// TestH1_ScatterAfterGateSkips is probe H1's exact repro: a scatter gated on a
// failed `after` dependency (a NON-member dep) must SKIP — its members never
// run, and its aggregate settles skipped. The drain exception is for member deps
// only.
func TestH1_ScatterAfterGateSkips(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("h1scatter",
		execNode("A", `exit 1`, nil),
		scatterNode("S", []string{"A"}, "continue",
			execNode("m1", `echo ran`, nil),
			execNode("m2", `echo ran`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["A"] != engine.OutcomeFailed {
		t.Errorf("A settled %q, want failed", settled["A"])
	}
	if settled["S"] != engine.OutcomeSkipped {
		t.Errorf("scatter S settled %q, want skipped (after-gate failed)", settled["S"])
	}
	for _, m := range []string{"m1", "m2"} {
		if settled[m] != engine.OutcomeSkipped {
			t.Errorf("member %q settled %q, want skipped (never ran)", m, settled[m])
		}
		if got := res.NodeOutputs[m]; got != "" {
			t.Errorf("member %q output = %q, want empty (never echoed)", m, got)
		}
	}
	if nodeStatus(t, store, "S") != "skipped" {
		t.Errorf("S status = %q, want skipped", nodeStatus(t, store, "S"))
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestH1_GatherAfterGateSkips is the gather peer of H1: a gather with a
// non-member `after` gate that failed is SKIPPED — its combine never runs. The
// scatter it drains (`over`) is a member dep and is excluded from the gate.
func TestH1_GatherAfterGateSkips(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("h1gather",
		execNode("A", `exit 1`, nil),
		scatterNode("reviews", nil, "continue", settleNode("s1", "pass")),
		gatherNode("G", "reviews", []string{"reviews", "A"}, settleNode("c1", "pass")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["reviews"] != engine.OutcomePass {
		t.Errorf("scatter reviews settled %q, want pass (its own gate is empty)", settled["reviews"])
	}
	if settled["G"] != engine.OutcomeSkipped {
		t.Errorf("gather G settled %q, want skipped (after-gate A failed)", settled["G"])
	}
	if _, ran := settled["c1"]; ran {
		t.Errorf("combine member c1 settled %q, want it to never run (gather skipped)", settled["c1"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestH1_ScatterMemberFailStillDrains keeps the drain CORE intact: a scatter
// whose MEMBER fails (not an after-gate) still drains to the aggregate — the
// aggregate settles (degraded here), it does not skip.
func TestH1_ScatterMemberFailStillDrains(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("h1drain",
		scatterNode("S", nil, "continue",
			settleNode("good", "pass"),
			settleNode("bad", "failed")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["good"] != engine.OutcomePass || settled["bad"] != engine.OutcomeFailed {
		t.Errorf("members = %v, want good pass / bad failed", settled)
	}
	if settled["S"] != engine.OutcomeDegraded {
		t.Errorf("scatter S settled %q, want degraded (member drained, not skipped)", settled["S"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- N-1: a drain aggregate over an all-skipped member set must SKIP ---------

// TestN1_GatherOverSkippedScatterSkipsNoEffects is the N-1 repro: A fails, so a
// scatter S gated on A is skipped (its members skipped), and a gather G that
// drains S must itself SKIP — its authored combine exec must NOT run (no marker,
// no side effect) and G settles skipped, not pass. Before the fix the drain
// exception admitted the skipped scatter as a settled member and G ran its
// combine downstream of a failure.
func TestN1_GatherOverSkippedScatterSkipsNoEffects(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	marker := filepath.Join(t.TempDir(), "combine-ran")
	scriptJSON, _ := json.Marshal("echo ran > " + marker)
	combineExec := `{
      "kind": "exec", "id": "combine", "name": "combine", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(scriptJSON) + `, "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "exitMap": {"pass": [0], "retryable": []}
    }`

	doc := decodeIR(t, blockDoc("n1gather",
		execNode("A", `exit 1`, nil),
		scatterNode("S", []string{"A"}, "continue", settleNode("m1", "pass")),
		gatherNode("G", "S", []string{"S"}, combineExec),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Fatalf("combine exec ran (marker %q exists) — a gather draining an all-skipped scatter must NOT run its combine", marker)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["A"] != engine.OutcomeFailed {
		t.Errorf("A settled %q, want failed", settled["A"])
	}
	if settled["S"] != engine.OutcomeSkipped {
		t.Errorf("scatter S settled %q, want skipped (after-gate A failed)", settled["S"])
	}
	if settled["G"] != engine.OutcomeSkipped {
		t.Errorf("gather G settled %q, want skipped (drains an all-skipped scatter — nothing ran)", settled["G"])
	}
	if _, ran := settled["combine"]; ran {
		t.Errorf("combine exec settled %q, want it to never run (gather skipped)", settled["combine"])
	}
	if nodeStatus(t, store, "G") != "skipped" {
		t.Errorf("G status = %q, want skipped", nodeStatus(t, store, "G"))
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestN1_GatherOverSkippedScatterMakesNoHostCall is the do-in-combine variant of
// the N-1 repro: the gather's combine is an agent `do`, and draining an
// all-skipped scatter must make ZERO host calls (the effect never schedules).
func TestN1_GatherOverSkippedScatterMakesNoHostCall(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("n1gatherdo",
		execNode("A", `exit 1`, nil),
		scatterNode("S", []string{"A"}, "continue", settleNode("m1", "pass")),
		gatherNode("G", "S", []string{"S"}, doNode("combineDo", "Combine the reviews.", nil)),
	))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"combineDo": {Outcome: enginehost.OutcomePass, Output: "combined"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if calls := stub.Calls(); len(calls) != 0 {
		t.Errorf("host called %d times, want 0 (a gather over an all-skipped scatter must not run its combine do)", len(calls))
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["G"] != engine.OutcomeSkipped {
		t.Errorf("gather G settled %q, want skipped", settled["G"])
	}
	if _, ran := settled["combineDo"]; ran {
		t.Errorf("combine do settled %q, want it to never run", settled["combineDo"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestN1_ScatterAllMembersSkippedIsSkipped is the N-3 fix: a scatter with no
// after-gate whose every member skip-cascades on an external failure must settle
// `skipped` (nothing ran), NOT `degraded` (which falsely claims partial success).
func TestN1_ScatterAllMembersSkippedIsSkipped(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("n3allskip",
		execNode("A", `exit 1`, nil),
		scatterNode("S", nil, "continue",
			execNode("m1", `echo ran`, []string{"A"}),
			execNode("m2", `echo ran`, []string{"A"})),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	for _, m := range []string{"m1", "m2"} {
		if settled[m] != engine.OutcomeSkipped {
			t.Errorf("member %q settled %q, want skipped (gated on failed A)", m, settled[m])
		}
		if got := res.NodeOutputs[m]; got != "" {
			t.Errorf("member %q output = %q, want empty (never ran)", m, got)
		}
	}
	if settled["S"] != engine.OutcomeSkipped {
		t.Errorf("scatter S settled %q, want skipped (all members skipped — nothing ran, not degraded)", settled["S"])
	}
	if nodeStatus(t, store, "S") != "skipped" {
		t.Errorf("S status = %q, want skipped", nodeStatus(t, store, "S"))
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestN1_ScatterPartialSkipStillDrains keeps the drain CORE intact under a
// partial skip: one member runs (no gate) and one skip-cascades on an external
// failure. The scatter still DRAINS on the ran member — the ran member's effect
// fires, the skipped member's does not, and the aggregate settles degraded
// (partial success), NOT skipped.
func TestN1_ScatterPartialSkipStillDrains(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	dir := t.TempDir()
	ranMarker := filepath.Join(dir, "m1-ran")
	skipMarker := filepath.Join(dir, "m2-ran")

	doc := decodeIR(t, blockDoc("n1partial",
		execNode("A", `exit 1`, nil),
		scatterNode("S", nil, "continue",
			execNode("m1", `echo ran > `+ranMarker, nil),
			execNode("m2", `echo ran > `+skipMarker, []string{"A"})),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, statErr := os.Stat(ranMarker); statErr != nil {
		t.Fatalf("m1 effect did not fire: marker %q missing (%v)", ranMarker, statErr)
	}
	if _, statErr := os.Stat(skipMarker); statErr == nil {
		t.Fatalf("m2 effect fired (marker %q exists) — a skipped member must not run", skipMarker)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["m1"] != engine.OutcomePass {
		t.Errorf("m1 settled %q, want pass (ran, no gate)", settled["m1"])
	}
	if settled["m2"] != engine.OutcomeSkipped {
		t.Errorf("m2 settled %q, want skipped (gated on failed A)", settled["m2"])
	}
	if settled["S"] != engine.OutcomeDegraded {
		t.Errorf("scatter S settled %q, want degraded (partial: one ran, one skipped — still drains)", settled["S"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- B1: gather combine actually executes its members -----------------------

// TestB1_ExecInCombineActuallyRuns proves a combine containing an `exec exit 1`
// actually RUNS the exec (a filesystem side effect proves it) and the gather
// settles FAILED from the exec's real outcome — not a silent pass.
func TestB1_ExecInCombineActuallyRuns(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	marker := filepath.Join(t.TempDir(), "combine-ran")
	scriptJSON, _ := json.Marshal("echo ran > " + marker + "; exit 1")
	execBoom := `{
      "kind": "exec", "id": "boom", "name": "boom", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(scriptJSON) + `, "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "exitMap": {"pass": [0], "retryable": []}
    }`

	doc := decodeIR(t, blockDoc("b1exec",
		scatterNode("reviews", nil, "continue", settleNode("s1", "pass")),
		gatherNode("G", "reviews", []string{"reviews"}, execBoom),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("combine exec did not run: marker %q missing (%v)", marker, statErr)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["boom"] != engine.OutcomeFailed {
		t.Errorf("combine exec boom settled %q, want failed (exit 1)", settled["boom"])
	}
	if settled["G"] != engine.OutcomeFailed {
		t.Errorf("gather G settled %q, want failed (combine exec failed)", settled["G"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestB1_DoInCombineCallsHost proves a combine containing a `do` actually calls
// the agent host (rather than being mistaken for a settle that never runs).
func TestB1_DoInCombineCallsHost(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("b1do",
		scatterNode("reviews", nil, "continue", settleNode("s1", "pass")),
		gatherNode("G", "reviews", []string{"reviews"}, doNode("combineDo", "Combine the reviews.", nil)),
	))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"combineDo": {Outcome: enginehost.OutcomePass, Output: "combined"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	calls := stub.Calls()
	if len(calls) != 1 || calls[0].Prompt != "Combine the reviews." {
		t.Fatalf("host calls = %v, want one call with the combine prompt", calls)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["combineDo"] != engine.OutcomePass {
		t.Errorf("combine do settled %q, want pass", settled["combineDo"])
	}
	if settled["G"] != engine.OutcomePass {
		t.Errorf("gather G settled %q, want pass", settled["G"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- M1: all-members-failed scatter is failed, not degraded -----------------

// TestScatterOnFailStopFails proves the on_fail "stop" path: any failed member
// fails the scatter even when another member passed (stop = no partial credit).
func TestScatterOnFailStopFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("stopfail",
		scatterNode("S", nil, "stop",
			settleNode("good", "pass"),
			settleNode("bad", "failed")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["S"] != engine.OutcomeFailed {
		t.Errorf("scatter S settled %q, want failed (on_fail stop, a member failed)", settled["S"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestM1_ScatterAllMembersFailedIsFailed proves that a scatter whose every
// member failed settles `failed` (a total loss), not `degraded` (which means
// partial success), even under on_fail "continue".
func TestM1_ScatterAllMembersFailedIsFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("m1",
		scatterNode("S", nil, "continue",
			settleNode("a", "failed"),
			settleNode("b", "failed")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["S"] != engine.OutcomeFailed {
		t.Errorf("scatter S settled %q, want failed (all members failed)", settled["S"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- M3: a dangling `after` reference is refused loudly ---------------------

// TestM3_DanglingAfterRefused proves an `after` reference to an unknown node is
// a loud lowering error, not a silently dropped dependency.
func TestM3_DanglingAfterRefused(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("m3", execNode("X", `echo x`, []string{"ghost"})))
	_, err := engine.Run(ctx, store, doc, nil)
	if err == nil {
		t.Fatal("expected an error for a dangling `after` reference, got nil")
	}
	if !strings.Contains(err.Error(), "unknown node") {
		t.Errorf("err = %v, want it to name the unknown node", err)
	}
	// The refusal happens at lowering, before any append.
	var journalRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows); err != nil {
		t.Fatalf("count journal: %v", err)
	}
	if journalRows != 0 {
		t.Errorf("a refused dangling-after wrote %d journal rows, want 0", journalRows)
	}
}

// --- L1: nested scatter members do not inflate the outer member set ---------

// TestL1_NestedScatterMemberSetNotInflated proves that when a scatter's member
// is itself a scatter, only the DIRECT member (the inner aggregate) counts as an
// outer member — the inner scatter's own members do not leak into the outer set.
func TestL1_NestedScatterMemberSetNotInflated(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("l1",
		scatterNode("outer", nil, "continue",
			scatterNode("inner", nil, "continue",
				settleNode("x", "pass"),
				settleNode("y", "pass"))),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	outerMembers := activatedMembers(t, res.Events, "outer:0")
	if len(outerMembers) != 1 || outerMembers[0] != "inner:0" {
		t.Errorf("outer scatter members = %v, want exactly [inner:0] (inner leaves must not inflate it)", outerMembers)
	}
	innerMembers := activatedMembers(t, res.Events, "inner:0")
	if len(innerMembers) != 2 {
		t.Errorf("inner scatter members = %v, want the two leaves", innerMembers)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- H2: an authored `skipped` P1 settle adopts the v2 status ---------------

// TestP1JournalUpcastSkippedSettleAdoptsV2Status documents the intentional,
// safe divergence in the v2 upcast: a P1 node.settled with outcome "skipped"
// folds to the v2 status "skipped" (P1 mapped it to "done"). This is acceptable
// because no live P1 journal contains a skipped settle (the P1 skeleton was
// exec-only) and the reducer-version bump to 2 licenses the change.
func TestP1JournalUpcastSkippedSettleAdoptsV2Status(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	const stream = "gcg-run-p1skipped0"

	store.RegisterEventType("lumen", "lumen.run.started")
	store.RegisterEventType("lumen", "lumen.node.settled")
	store.RegisterEventType("lumen", "lumen.run.closed")

	lease, err := store.AcquireWriterLease(ctx, stream, "test", 30_000_000_000)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	appendP1 := func(head uint64, typ, idem string, payload any) {
		body, _ := canonJSON(t, payload)
		if _, err := store.Append(ctx, stream, "lumen", head, lease.Epoch, []graphstore.JournalEvent{{
			Type: typ, IRContractVersion: "0.2.5", IdemToken: idem, Payload: body,
		}}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendP1(0, "lumen.run.started", stream+":run:started", map[string]any{
		"root_id": stream, "name": "legacy", "created_at": "2020-01-01T00:00:00Z",
	})
	appendP1(1, "lumen.node.settled", stream+":n1:0", map[string]any{
		"id": "n1", "outcome": "skipped", "output": "",
	})
	appendP1(2, "lumen.run.closed", stream+":run:closed", map[string]any{"outcome": "pass"})
	_ = store.ReleaseWriterLease(ctx, lease)

	if err := store.RebuildTierA(ctx, engine.Reducer(), stream); err != nil {
		t.Fatalf("rebuild (upcast) failed: %v", err)
	}
	// The NEW v2 status is "skipped" (P1 would have projected "done"). This is the
	// documented, version-bump-licensed divergence.
	if got := nodeStatus(t, store, "n1"); got != "skipped" {
		t.Errorf("n1 status = %q, want skipped (v2 status for an authored skip)", got)
	}
}
