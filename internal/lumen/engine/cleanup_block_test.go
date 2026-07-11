package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// cleanupBlockNode renders a cleanup(try/finally) node whose `guarded` is a BLOCK of
// the given leaf member strings (auto-chained by the caller via each member's `after`),
// and whose `body` (finally) is the single leaf string. The block's own id is inert —
// only its leaf members become units, inlined at their bare ids under a synthetic
// transparent drain aggregate `<cleanupID>/__guarded`.
func cleanupBlockNode(id string, after []string, finally string, members ...string) string {
	afterJSON := "[]"
	if len(after) > 0 {
		afterJSON = `["` + strings.Join(after, `","`) + `"]`
	}
	block := `{"kind":"block","id":"` + id + `_blk","after":[],"origin":{"uri":"t","line":1,"col":0},` +
		`"members":[` + strings.Join(members, ",") + `]}`
	return `{"kind":"cleanup","id":"` + id + `","name":"` + id + `","after":` + afterJSON + `,` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"guarded":` + block + `,"body":` + finally + `}`
}

// --- Plan lowering -----------------------------------------------------------

// TestCleanupBlockLoweringShape pins the CUB lowering: the block's leaf children are
// inlined as ordinary BARE-ID units parented to a synthetic transparent drain aggregate
// (<cleanupID>/__guarded, kind=block, members=the leaves), and the cleanup drains that
// aggregate via memberDeps.
func TestCleanupBlockLoweringShape(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
			execNode("stepC", `echo C`, []string{"stepB"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The guarded aggregate drains the three block leaves, in source order.
	if agg := activatedMembers(t, res.Events, "clean/__guarded:0"); !equalStrings(agg, []string{"stepA:0", "stepB:0", "stepC:0"}) {
		t.Fatalf("guarded agg members = %v, want [stepA:0 stepB:0 stepC:0]", agg)
	}
	// The members are inlined at BARE ids, parented under the aggregate (NOT namespaced).
	for _, m := range []string{"stepA:0", "stepB:0", "stepC:0"} {
		if p := activatedParent(t, res.Events, m); p != "clean/__guarded:0" {
			t.Errorf("member %q parent = %q, want clean/__guarded:0", m, p)
		}
	}
	// The aggregate is parented under the cleanup.
	if p := activatedParent(t, res.Events, "clean/__guarded:0"); p != "clean:0" {
		t.Errorf("agg parent = %q, want clean:0", p)
	}
	// The cleanup drains the aggregate (memberDeps = [agg]) — the always-run edge is a
	// DRAIN, not a blocking gate, so a failed block never skip-cascades the finally.
	if cm := activatedMembers(t, res.Events, "clean:0"); !equalStrings(cm, []string{"clean/__guarded:0"}) {
		t.Fatalf("cleanup members = %v, want [clean/__guarded:0]", cm)
	}
}

// --- Run semantics -----------------------------------------------------------

// TestCleanupBlockAllPass proves an all-pass block guarded settles pass with the
// LAST-ran member's output, a downstream {{cleanupID}} plumbs that output, and the
// finally can read a member's output at its BARE id (the BLOCKER-1 regression: members
// are inlined in the parent flat scope, visible to the finally and downstream).
func TestCleanupBlockAllPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo "saw={{ stepB }}"`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
			execNode("stepC", `echo C`, []string{"stepB"}),
		),
		execNode("after", `echo "c={{ clean }}"`, []string{"clean"}),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["clean"]; got != "C" {
		t.Errorf("cleanup output = %q, want C (last block step that ran)", got)
	}
	if got := res.NodeOutputs["after"]; got != "c=C" {
		t.Errorf("downstream = %q, want c=C (guarded-block last output plumbed through cleanup)", got)
	}
	if got := res.NodeOutputs["teardown"]; got != "saw=B" {
		t.Errorf("finally output = %q, want saw=B (finally reads a member at its bare id)", got)
	}
}

// TestCleanupBlockMembersReadParentScope is the BLOCKER-1 silent-corruption regression:
// a block member's script renders a PARENT-scope input {{ who }}. If the members were
// borrowed into a run-style namespace, {{ who }} would render "" (silent prompt
// corruption); inlined at bare ids in the parent scope, it renders the real value.
func TestCleanupBlockMembersReadParentScope(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo done`, nil),
			execNode("stepA", `echo "hi {{ who }}"`, nil),
		),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stepA"]; got != "hi world" {
		t.Errorf("member output = %q, want %q (member renders parent-scope input)", got, "hi world")
	}
}

// TestCleanupBlockMidFailFinallyRuns is DET seed #1: a block step fails mid-chain, its
// block-internal successors skip-cascade, the aggregate settles failed (transparent
// worst-of; skips ignored), the finally STILL runs (always-run), and the cleanup settles
// failed (guarded's failure propagates through a passing finally).
func TestCleanupBlockMidFailFinallyRuns(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo teardown`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `exit 1`, []string{"stepA"}),
			execNode("stepC", `echo C`, []string{"stepB"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "stepA", engine.OutcomePass)
	assertSettled(t, settled, "stepB", engine.OutcomeFailed)
	assertSettled(t, settled, "stepC", engine.OutcomeSkipped) // block-internal skip-cascade
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeFailed)
	assertSettled(t, settled, "teardown", engine.OutcomePass) // finally ran despite the block failure
	assertSettled(t, settled, "clean", engine.OutcomeFailed)
	if got := res.NodeOutputs["teardown"]; got != "teardown" {
		t.Errorf("finally output = %q, want teardown (always-run)", got)
	}
}

// TestCleanupBlockFinallyFailureWins proves a failing finally overrides a passing block
// guarded (the finally's error supersedes).
func TestCleanupBlockFinallyFailureWins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, settleNode("teardown", "failed"),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomePass) // block passed
	assertSettled(t, settled, "clean", engine.OutcomeFailed)         // finally failure wins
}

// TestCleanupBlockGateFailedSkipCascade is DET seed #4: the cleanup's OWN `after` gate
// failed, so the whole block skip-cascades (H1), the aggregate settles skipped via
// blocked(), the cleanup settles skipped, and the finally never runs.
func TestCleanupBlockGateFailedSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		execNode("gate", `exit 1`, nil),
		cleanupBlockNode("clean", []string{"gate"}, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeSkipped)
	assertSettled(t, settled, "clean", engine.OutcomeSkipped)
	for _, id := range []string{"stepA", "stepB", "teardown"} {
		if got := res.NodeOutputs[id]; got != "" {
			t.Errorf("%q output = %q, want empty (gated-off cleanup runs nothing)", id, got)
		}
	}
	// The own-gate path skips everything through blocked() — the "upstream dependency"
	// detail on the members (H1 gate propagation), the aggregate (⚑SHOULD-FIX-4 gate on
	// the agg), AND the cleanup itself. NOT the aggregateAllSkipped string: blocked()
	// intercepts before the all-skipped check.
	for _, act := range []string{"stepA:0", "stepB:0", "clean/__guarded:0", "clean:0"} {
		if got := settledDetailFor(t, res.Events, act); got != blockedDetail {
			t.Errorf("%s detail = %q, want %q (blocked(), not aggregateAllSkipped)", act, got, blockedDetail)
		}
	}
}

// TestCleanupBlockOutsideAfterResolves is DET seed #8: an intra-block member's `after`
// naming an OUTSIDE (top-level) node resolves in the flat namespace (same-scope
// semantics, consistent with plain block hoisting) — the member runs after the outside
// node and reads its output.
func TestCleanupBlockOutsideAfterResolves(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		execNode("prep", `echo P`, nil),
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo "saw {{ prep }}"`, []string{"prep"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stepA"]; got != "saw P" {
		t.Errorf("member output = %q, want %q (intra-block after to an outside node resolved)", got, "saw P")
	}
}

// TestCleanupBlockTwoCoexist pins SHOULD-FIX-6: two block-form cleanups in one document
// have EMPTY synthesized guarded ids, so without the addSynth guardedNodeID!="" guard
// they would collide on synthBodies[""]. Both must lower and run.
func TestCleanupBlockTwoCoexist(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("c1", nil, execNode("t1", `echo T1`, nil), execNode("a1", `echo A1`, nil)),
		cleanupBlockNode("c2", nil, execNode("t2", `echo T2`, nil), execNode("a2", `echo A2`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v (two block-form cleanups must not collide on the empty synth guarded id)", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "c1", engine.OutcomePass)
	assertSettled(t, settled, "c2", engine.OutcomePass)
}

// TestCleanupBlockMemberNamedGuardedIsDistinct pins the ⚑NICE-7 corner: a block member
// literally named `__guarded` activates at its bare id (`__guarded:0`), which is DISTINCT
// from the synthetic drain aggregate's activation (`clean/__guarded:0`) — the '/' in the
// synthetic id keeps them apart, so the member runs cleanly with no collision.
func TestCleanupBlockMemberNamedGuardedIsDistinct(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("__guarded", `echo G`, nil),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v (a member named __guarded is distinct from the synthetic agg id)", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["__guarded"]; got != "G" {
		t.Errorf("member __guarded output = %q, want G (activation __guarded:0, distinct from clean/__guarded:0)", got)
	}
}

// TestCleanupBlockUnsupportedRefusals pins the deferred/invalid shapes refused at load
// with ErrUnsupportedNode: an empty block, a non-leaf block member (message names the
// kind, ⚑NICE-9), a guarded block carrying its OWN `after` gate (⚑SHOULD-FIX-3), and a
// block finally (deferred; decodeLeafSub's kind refusal covers it).
func TestCleanupBlockUnsupportedRefusals(t *testing.T) {
	ctx := context.Background()
	blockBody := `{"kind":"block","id":"bblk","after":[],"origin":{"uri":"t","line":1,"col":0},"members":[` +
		execNode("t1", `echo 1`, nil) + `]}`
	guardedWithAfter := `{"kind":"cleanup","id":"clean","name":"clean","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"guarded":{"kind":"block","id":"blk","after":["gate"],"origin":{"uri":"t","line":1,"col":0},"members":[` +
		execNode("stepA", `echo A`, nil) + `]},"body":` + execNode("teardown", `echo T`, nil) + `}`
	cases := []struct {
		name  string
		nodes []string
	}{
		{
			name:  "empty guarded block",
			nodes: []string{cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil))},
		},
		{
			name: "non-leaf block member",
			nodes: []string{cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
				execNode("stepA", `echo A`, nil),
				scatterNode("s", nil, "continue", execNode("x", `echo 1`, nil)))},
		},
		{
			name:  "guarded block carries own after",
			nodes: []string{execNode("gate", `echo G`, nil), guardedWithAfter},
		},
		{
			name: "block finally",
			nodes: []string{cleanupBlockNode("clean", nil, blockBody,
				execNode("stepA", `echo A`, nil))},
		},
		{
			// FIX-2: an empty member id would slip past lowerNode's '/'-and-':' ban and
			// lower to the anonymous activation ":0"; refuse it in the member pre-check.
			name: "empty member id",
			nodes: []string{cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
				execNode("", `echo A`, nil))},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("cubrf", tc.nodes...))
			_, err := engine.Run(ctx, newStore(t), doc, nil)
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("run err = %v, want ErrUnsupportedNode", err)
			}
		})
	}
}

// TestCleanupBlockCollisionRefusals pins the id-collision refusals caught by
// topoSortUnits' dup-activation guard (duplicate members / a member aliasing a top-level
// node — both REAL units) and the addSynth guard (a member id aliasing the finally id).
func TestCleanupBlockCollisionRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		nodes []string
	}{
		{
			name: "duplicate member ids",
			nodes: []string{cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
				execNode("dup", `echo 1`, nil),
				execNode("dup", `echo 2`, nil))},
		},
		{
			name: "member aliases a top-level node",
			nodes: []string{
				execNode("sib", `echo sib`, nil),
				cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
					execNode("sib", `echo 2`, nil)),
			},
		},
		{
			name: "member aliases the finally id",
			nodes: []string{cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
				execNode("teardown", `echo dup`, nil))},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("cubcol", tc.nodes...))
			_, err := engine.Run(ctx, newStore(t), doc, nil)
			if err == nil {
				t.Fatalf("run err = nil, want an id-collision refusal")
			}
		})
	}
}

// TestCleanupBlockDropRefoldByteIdentity pins DET for a block cleanup (mid-fail + pass
// finally): the live projection equals a drop+refold, so the incremental fold and a
// full-state refold agree over the inlined members, the block aggregate, and the cleanup.
func TestCleanupBlockDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo teardown`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `exit 1`, []string{"stepA"}),
			execNode("stepC", `echo C`, []string{"stepB"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// --- Advance (pool) ----------------------------------------------------------

// TestAdvanceCleanupBlockDrainsThenFinallyParks proves a pool-do block cleanup drives
// its block members (each parking, the chain draining one at a time), then — after the
// aggregate settles inline — its finally do (parking), then seals: three member
// dispatches + one finally dispatch = four owned.admitted, and the cleanup settles pass.
func TestAdvanceCleanupBlockDrainsThenFinallyParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acub",
		cleanupBlockNode("clean", nil, doNode("teardown", "tear down", nil),
			doNode("stepA", "do A", nil),
			doNode("stepB", "do B", []string{"stepA"}),
			doNode("stepC", "do C", []string{"stepB"}),
		),
	))
	opts := fake.opts()
	const streamID = "gcg-run-acub"

	// Pass 1: only stepA is ready (stepB/stepC chain on it) — materialized, parked.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed || len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "stepA" {
		t.Fatalf("advance 1 = %+v, want parked on stepA", r1)
	}

	fake.settleAct(t, "stepA:0", engine.OutcomePass, "A")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if r2.Sealed || len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "stepB" {
		t.Fatalf("advance 2 = %+v, want parked on stepB", r2)
	}

	fake.settleAct(t, "stepB:0", engine.OutcomePass, "B")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if r3.Sealed || len(r3.InFlight) != 1 || r3.InFlight[0].NodeID != "stepC" {
		t.Fatalf("advance 3 = %+v, want parked on stepC", r3)
	}

	// stepC closes; pass 4 settles the aggregate inline and dispatches the finally.
	fake.settleAct(t, "stepC:0", engine.OutcomePass, "C")
	r4, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 4: %v", err)
	}
	if r4.Sealed || len(r4.InFlight) != 1 || r4.InFlight[0].NodeID != "teardown" {
		t.Fatalf("advance 4 = %+v, want parked on teardown (finally)", r4)
	}

	// teardown closes; pass 5 seals.
	fake.settleAct(t, "teardown:0", engine.OutcomePass, "torn")
	r5, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 5: %v", err)
	}
	if !r5.Sealed {
		t.Fatalf("advance 5 = %+v, want Sealed", r5)
	}
	settled := settledIDs(t, r5.Run.Events)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomePass)
	assertSettled(t, settled, "clean", engine.OutcomePass)
	if n := countActivatedWorkBeads(t, r5.Run.Events); n != 4 {
		t.Fatalf("owned.admitted (work bead) count = %d, want 4 (3 block members + 1 finally)", n)
	}
}

// countActivatedWorkBeads counts owned.admitted{work_bead} dispatch facts.
func countActivatedWorkBeads(t *testing.T, events []graphstore.StoredEvent) int {
	t.Helper()
	n := 0
	for _, e := range events {
		if e.Type == engine.EventOwnedAdmitted {
			n++
		}
	}
	return n
}

// TestAdvanceCleanupBlockWriteOnceAndRefold pins DET seed #5: after the block drains and
// the finally parks, a redundant Advance re-dispatches NOTHING (write-once) and the run
// stays parked on the finally; after seal, the live projection equals a drop+refold.
func TestAdvanceCleanupBlockWriteOnceAndRefold(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acub",
		cleanupBlockNode("clean", nil, doNode("teardown", "tear down", nil),
			doNode("stepA", "do A", nil),
			doNode("stepB", "do B", []string{"stepA"}),
		),
	))
	opts := fake.opts()
	const streamID = "gcg-run-acub2"

	// Drive the block to drain and the finally to park.
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settleAct(t, "stepA:0", engine.OutcomePass, "A")
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	fake.settleAct(t, "stepB:0", engine.OutcomePass, "B")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if len(r3.InFlight) != 1 || r3.InFlight[0].NodeID != "teardown" {
		t.Fatalf("advance 3 = %+v, want parked on teardown", r3)
	}

	// Write-once: a redundant Advance with no new settlement re-dispatches nothing.
	dispatchesBefore := fake.dispatchCount()
	r3b, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 3b (redundant): %v", err)
	}
	if len(r3b.InFlight) != 1 || r3b.InFlight[0].NodeID != "teardown" {
		t.Fatalf("advance 3b = %+v, want still parked on teardown", r3b)
	}
	if got := fake.dispatchCount(); got != dispatchesBefore {
		t.Fatalf("dispatch count grew %d -> %d on a redundant Advance (write-once broken)", dispatchesBefore, got)
	}

	// Seal, then DET drop+refold byte-identity.
	fake.settleAct(t, "teardown:0", engine.OutcomePass, "torn")
	r4, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 4: %v", err)
	}
	if !r4.Sealed {
		t.Fatalf("advance 4 = %+v, want Sealed", r4)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestAdvanceCleanupBlockBodyRunsAfterBlockFail proves the always-run edge under the pool
// path: a block member closes FAILED, the aggregate settles failed, yet the finally do is
// still dispatched and the cleanup seals failed.
func TestAdvanceCleanupBlockBodyRunsAfterBlockFail(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acubf",
		cleanupBlockNode("clean", nil, doNode("teardown", "tear down", nil),
			doNode("stepA", "do A", nil),
		),
	))
	opts := fake.opts()
	const streamID = "gcg-run-acubf"

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settleAct(t, "stepA:0", engine.OutcomeFailed, "boom")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "teardown" {
		t.Fatalf("advance 2 = %+v, want the finally dispatched despite the block failure", r2)
	}
	fake.settleAct(t, "teardown:0", engine.OutcomePass, "torn")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if !r3.Sealed {
		t.Fatalf("advance 3 = %+v, want Sealed", r3)
	}
	settled := settledIDs(t, r3.Run.Events)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeFailed)
	assertSettled(t, settled, "clean", engine.OutcomeFailed)
}

// --- All-skipped block: the finally is suppressed (nothing ran) ---------------

// blockedDetail / allSkippedDetail are the two distinct skip detail strings the driver
// emits: blocked() for a failed `after` gate, aggregateAllSkipped() for a drain whose
// every member did-not-run. Pinning them apart is load-bearing — they identify WHICH
// suppression path fired.
const (
	blockedDetail    = "skipped: upstream dependency did not pass"
	allSkippedDetail = "skipped: every drain member skipped (nothing ran)"
)

// settledDetailFor returns the outcome.settled Detail for an activation (fatal if the
// activation never settled).
func settledDetailFor(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Detail     string `json:"detail"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return p.Detail
		}
	}
	t.Fatalf("no outcome.settled for activation %q", activation)
	return ""
}

// assertNeverSettled asserts NO outcome.settled exists for a bare node id — the proof a
// suppressed finally never ran (not even as a skip: it was never activated).
func assertNeverSettled(t *testing.T, events []graphstore.StoredEvent, nodeID string) {
	t.Helper()
	for _, s := range settledIDs(t, events) {
		if s[0] == nodeID {
			t.Errorf("%q settled %q, want NO settle at all (suppressed — never activated)", nodeID, s[1])
			return
		}
	}
}

// countSettlesFor counts outcome.settled events for one activation (write-once proof).
func countSettlesFor(t *testing.T, events []graphstore.StoredEvent, activation string) int {
	t.Helper()
	n := 0
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			n++
		}
	}
	return n
}

// typePayloadPairs flattens a journal to (type, payload-bytes) pairs, SKIPPING the
// run.started head (its root_id/created_at legitimately differ across drivers). It is the
// cross-driver byte-identity view: two drivers agree on this state iff every subsequent
// event's type AND canonical payload bytes match in order.
func typePayloadPairs(events []graphstore.StoredEvent) [][2]string {
	var out [][2]string
	for _, e := range events {
		if e.Type == engine.EventRunStarted {
			continue
		}
		out = append(out, [2]string{e.Type, string(e.Payload)})
	}
	return out
}

// pairTypes projects the pair list to types only, for divergence diagnostics.
func pairTypes(pairs [][2]string) []string {
	out := make([]string, len(pairs))
	for i := range pairs {
		out[i] = pairs[i][0]
	}
	return out
}

// TestCleanupBlockAllSkippedSuppressesFinally (inline) is the FIX-1 semantics pin: the
// cleanup's OWN gate passes (absent), but every block member is gated off by a
// member-level outside `after` naming a failing node. Nothing in the block ran, so there
// is nothing to tear down: the aggregate settles skipped via aggregateAllSkipped, the
// cleanup settles skipped through the SAME intercept (its detail string — NOT blocked()'s
// — proves which path fired), and the finally NEVER runs.
func TestCleanupBlockAllSkippedSuppressesFinally(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cubskip",
		execNode("gate", `exit 1`, nil),
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, []string{"gate"}),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "stepA", engine.OutcomeSkipped)
	assertSettled(t, settled, "stepB", engine.OutcomeSkipped)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeSkipped)
	assertSettled(t, settled, "clean", engine.OutcomeSkipped)
	assertNeverSettled(t, res.Events, "teardown")
	if got := res.NodeOutputs["teardown"]; got != "" {
		t.Errorf("finally output = %q, want empty (suppressed — nothing to tear down)", got)
	}
	// The agg and cleanup skipped via the aggregate-skip intercept (their own gates
	// passed), while the members skipped via blocked() on the failed outside gate.
	if got := settledDetailFor(t, res.Events, "stepA:0"); got != blockedDetail {
		t.Errorf("member detail = %q, want %q", got, blockedDetail)
	}
	if got := settledDetailFor(t, res.Events, "clean/__guarded:0"); got != allSkippedDetail {
		t.Errorf("agg detail = %q, want %q", got, allSkippedDetail)
	}
	if got := settledDetailFor(t, res.Events, "clean:0"); got != allSkippedDetail {
		t.Errorf("cleanup detail = %q, want %q", got, allSkippedDetail)
	}
}

// TestCleanupBlockAllSkippedPoolInlineJournalParity is the FIX-1 cross-driver proof: the
// SAME all-skipped-block IR (do members + do finally, gated off by a failing outside
// node) driven by the inline Run driver (Host, never invoked) and the pool Advance driver
// (PoolRouter, never dispatching) must produce BYTE-IDENTICAL journals after run.started
// — same event types, same canonical payload bytes, in the same order. Before the fix the
// pool driver dispatched the finally to a real worker (an owned.admitted + pool-mode
// activation the inline journal does not have) and settled a cleanup the reducer's
// ready() never considered frontier-ready (an all-didNotRun member set).
func TestCleanupBlockAllSkippedPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("cubpar",
		execNode("gate", `exit 1`, nil),
		cleanupBlockNode("clean", nil, doNode("teardown", "tear down", nil),
			doNode("stepA", "do A", []string{"gate"}),
			doNode("stepB", "do B", []string{"stepA"}),
		),
	))

	// Inline driver: the host must never be invoked (every do is gated off / suppressed).
	inStore := newStore(t)
	inRes, err := engine.RunWithOptions(ctx, inStore, doc, nil, engine.Options{Host: passDoStub()})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if n := countJournalType(t, inStore, inRes.StreamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("inline effect.scheduled count = %d, want 0 (host must never be invoked)", n)
	}

	// Pool driver: one Advance pass must SEAL (gate fails inline, members skip, agg skips,
	// cleanup skips — all in-pass) — never park on a dispatched finally.
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-run-cubpar", nil, fake.opts())
	if err != nil {
		t.Fatalf("pool advance: %v", err)
	}
	if !r.Sealed {
		t.Fatalf("pool advance = %+v, want Sealed in one pass (the finally must NOT be dispatched)", r)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (nothing ran — nothing to tear down)", n)
	}
	if n := countActivatedWorkBeads(t, r.Run.Events); n != 0 {
		t.Fatalf("pool owned.admitted count = %d, want 0", n)
	}

	// Byte-identity: every post-run.started event agrees across the two drivers.
	inPairs, poolPairs := typePayloadPairs(inRes.Events), typePayloadPairs(r.Run.Events)
	if len(inPairs) != len(poolPairs) {
		t.Fatalf("journal lengths diverge: inline %d events vs pool %d (after run.started)\ninline: %v\npool:   %v",
			len(inPairs), len(poolPairs), pairTypes(inPairs), pairTypes(poolPairs))
	}
	for i := range inPairs {
		if inPairs[i] != poolPairs[i] {
			t.Fatalf("journals diverge at post-run.started event %d:\ninline: %s %s\npool:   %s %s",
				i, inPairs[i][0], inPairs[i][1], poolPairs[i][0], poolPairs[i][1])
		}
	}
}

// TestCleanupBlockCanceledMemberVsLeafAsymmetry pins the block/leaf asymmetry for a
// settle-canceled guarded. BLOCK form: the block is a DRAIN aggregate — a sole canceled
// member means nothing RAN (didNotRun(canceled)=true), so the aggregate and cleanup
// settle skipped and the finally is suppressed. LEAF form: the guarded is TRANSPARENT —
// the finally still runs and the cleanup settles canceled from the guarded. The asymmetry
// is deliberate: a drain aggregate reports whether any work happened; a leaf guarded IS
// the work, and its teardown pairs with the attempt, not the outcome.
func TestCleanupBlockCanceledMemberVsLeafAsymmetry(t *testing.T) {
	ctx := context.Background()

	// Block-of-one canceled: suppressed.
	bStore := newStore(t)
	bDoc := decodeIR(t, blockDoc("cubcan",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			settleNode("only", "canceled"),
		),
	))
	bRes, err := engine.Run(ctx, bStore, bDoc, nil)
	if err != nil {
		t.Fatalf("block run: %v", err)
	}
	bSettled := settledIDs(t, bRes.Events)
	assertSettled(t, bSettled, "only", engine.OutcomeCanceled)
	assertSettled(t, bSettled, "clean/__guarded", engine.OutcomeSkipped)
	assertSettled(t, bSettled, "clean", engine.OutcomeSkipped)
	assertNeverSettled(t, bRes.Events, "teardown")

	// Leaf contrast: the finally runs, the cleanup settles canceled (pre-existing
	// transparent semantics, unchanged by the block slice).
	lStore := newStore(t)
	lDoc := decodeIR(t, blockDoc("culcan",
		cleanupNode(nil, settleNode("g", "canceled"), execNode("b", `echo T`, nil)),
	))
	lRes, err := engine.Run(ctx, lStore, lDoc, nil)
	if err != nil {
		t.Fatalf("leaf run: %v", err)
	}
	lSettled := settledIDs(t, lRes.Events)
	assertSettled(t, lSettled, "g", engine.OutcomeCanceled)
	assertSettled(t, lSettled, "b", engine.OutcomePass) // leaf form: the finally STILL runs
	assertSettled(t, lSettled, "clean", engine.OutcomeCanceled)
}

// TestAdvanceCleanupBlockFailedSkipMixStillDispatchesFinally guards the FIX-1 intercept
// against over-suppression on the pool path: a FAILED member means the block RAN
// (didNotRun(failed)=false), so even with its successor skipped the aggregate settles
// failed — NOT all-skipped — and the finally do IS dispatched.
func TestAdvanceCleanupBlockFailedSkipMixStillDispatchesFinally(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acubmix",
		cleanupBlockNode("clean", nil, doNode("teardown", "tear down", nil),
			doNode("stepA", "do A", nil),
			doNode("stepB", "do B", []string{"stepA"}),
		),
	))
	opts := fake.opts()
	const streamID = "gcg-run-acubmix"

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settleAct(t, "stepA:0", engine.OutcomeFailed, "boom")
	// Pass 2: stepB skip-cascades off the failed stepA, the agg settles failed (a ran
	// member), and the finally dispatches — the mix must not be mistaken for all-skipped.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "teardown" {
		t.Fatalf("advance 2 = %+v, want the finally dispatched (failed+skipped mix is NOT all-skipped)", r2)
	}
	fake.settleAct(t, "teardown:0", engine.OutcomePass, "torn")
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if !r3.Sealed {
		t.Fatalf("advance 3 = %+v, want Sealed", r3)
	}
	settled := settledIDs(t, r3.Run.Events)
	assertSettled(t, settled, "stepB", engine.OutcomeSkipped)
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeFailed)
	assertSettled(t, settled, "clean", engine.OutcomeFailed)
}

// TestCleanupBlockDegradedMemberWorstOf covers the worst-of degraded arm: a degraded
// member (non-blocking — its successor still runs) degrades the aggregate, and the
// cleanup settles degraded through a passing finally, with the last-RAN member's output.
func TestCleanupBlockDegradedMemberWorstOf(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cubdeg",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			settleNode("stepA", "degraded"),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "stepA", engine.OutcomeDegraded)
	assertSettled(t, settled, "stepB", engine.OutcomePass) // degraded is non-blocking
	assertSettled(t, settled, "clean/__guarded", engine.OutcomeDegraded)
	assertSettled(t, settled, "teardown", engine.OutcomePass)
	assertSettled(t, settled, "clean", engine.OutcomeDegraded)
	if got := res.NodeOutputs["clean"]; got != "B" {
		t.Errorf("cleanup output = %q, want B (last block step that ran)", got)
	}
}

// --- Resume (crash points) ---------------------------------------------------

// TestResumeCleanupBlockCrashAfterLastMemberBeforeAgg pins the crash window between the
// last member's settle and the aggregate's settle: resume reloads the members and settles
// the aggregate exactly ONCE, then the finally and the cleanup, converging to genesis.
func TestResumeCleanupBlockCrashAfterLastMemberBeforeAgg(t *testing.T) {
	doc := decodeIR(t, blockDoc("rcubagg",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	resumed, store, streamID := injectCrashThenResume(t, doc, nil, engine.CrashAfterSettle, "stepB:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	if n := countSettlesFor(t, resumed.Events, "clean/__guarded:0"); n != 1 {
		t.Fatalf("agg outcome.settled count = %d, want exactly 1 (settled once on resume)", n)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	for id, want := range map[string]string{
		"stepA": engine.OutcomePass, "stepB": engine.OutcomePass,
		"clean/__guarded": engine.OutcomePass, "teardown": engine.OutcomePass, "clean": engine.OutcomePass,
	} {
		if settled[id] != want {
			t.Errorf("resumed %q = %q, want %q", id, settled[id], want)
		}
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestResumeCleanupBlockCrashAfterDrainBeforeFinally is DET seed #6: a crash after the
// guarded block drains (the aggregate settled) but before the finally runs. Resume runs
// the finally ONCE and settles the cleanup, converging to the genesis terminal.
func TestResumeCleanupBlockCrashAfterDrainBeforeFinally(t *testing.T) {
	doc := decodeIR(t, blockDoc("rcub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	resumed, store, streamID := injectCrashThenResume(t, doc, nil, engine.CrashAfterSettle, "clean/__guarded:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	for id, want := range map[string]string{
		"stepA": engine.OutcomePass, "stepB": engine.OutcomePass,
		"clean/__guarded": engine.OutcomePass, "teardown": engine.OutcomePass, "clean": engine.OutcomePass,
	} {
		if settled[id] != want {
			t.Errorf("resumed %q = %q, want %q", id, settled[id], want)
		}
	}
	if got := resumed.NodeOutputs["teardown"]; got != "T" {
		t.Errorf("finally output = %q, want T (finally ran once on resume)", got)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestResumeCleanupBlockCrashAfterFinally is the second §4.6 crash point: a crash after
// the finally settled but before the cleanup settled. Resume reloads the finally (does
// NOT re-run it) and settles the cleanup once.
func TestResumeCleanupBlockCrashAfterFinally(t *testing.T) {
	doc := decodeIR(t, blockDoc("rcub",
		cleanupBlockNode("clean", nil, execNode("teardown", `echo T`, nil),
			execNode("stepA", `echo A`, nil),
			execNode("stepB", `echo B`, []string{"stepA"}),
		),
	))
	resumed, store, streamID := injectCrashThenResume(t, doc, nil, engine.CrashAfterSettle, "teardown:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	assertSettled(t, [][2]string{{"clean", settled["clean"]}}, "clean", engine.OutcomePass)
	if settled["teardown"] != engine.OutcomePass {
		t.Errorf("resumed teardown = %q, want pass", settled["teardown"])
	}
	// Scope parity: a clean genesis run seeds the SAME node outputs the resumed run does.
	gStore := newStore(t)
	gRes, err := engine.Run(context.Background(), gStore, doc, nil)
	if err != nil {
		t.Fatalf("genesis run: %v", err)
	}
	for _, id := range []string{"stepA", "stepB", "clean/__guarded", "teardown", "clean"} {
		if resumed.NodeOutputs[id] != gRes.NodeOutputs[id] {
			t.Errorf("scope parity %q: resumed=%q genesis=%q", id, resumed.NodeOutputs[id], gRes.NodeOutputs[id])
		}
	}
	assertProjectionEqualsRefold(t, store, streamID)
}
