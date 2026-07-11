package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestScatterMixedDoAndRunSeals is the run-in-scatter runtime crux: a scatter whose
// direct member is a pool `do` and whose other member is a `run` (sub-formula call with
// an inline exec body) seals — the run member runs its sub inline in the SAME pass while
// the direct do parks on the pool ("inline Run + pool Advance"). One re-Advance after the
// direct do settles drains the scatter and seals pass. Exactly one work bead dispatches
// (the direct do; the run's exec sub is inline).
func TestScatterMixedDoAndRunSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-scatter-run-mixed"
	doc := decodeIR(t, bundleDoc(
		"",
		scatterNode("lanes", nil, "continue",
			doNode("direct", "Do the direct lane.", nil),
			runNodeJSON("extra", nil, "sub", "", "")),
		subDoc("sub", "", execNode("inner", "echo subval", nil)),
	))
	fake := newFakeWorkStore()
	opts := fake.opts()

	// Pass 1: the direct do dispatches as pool work; the run member runs its exec sub
	// inline and settles transparently; the scatter parks on the still-open direct do.
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if !r1.Parked || r1.Sealed {
		t.Fatalf("advance 1 = %+v, want Parked (direct do awaits the pool)", r1)
	}
	if len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "direct" {
		t.Fatalf("advance 1 in-flight = %+v, want only the direct do", r1.InFlight)
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls = %d, want 1 (only the direct do; the run's exec sub is inline)", fake.dispatchCount())
	}
	// The run member ran inline this pass: its sub and the transparent aggregate settled.
	if o := settledOutcomeOf(t, store, streamID, "extra/inner:0"); o != engine.OutcomePass {
		t.Fatalf("extra/inner:0 = %q, want pass (the run's exec sub ran inline)", o)
	}
	if o := settledOutcomeOf(t, store, streamID, "extra:0"); o != engine.OutcomePass {
		t.Fatalf("extra:0 = %q, want pass (transparent run member settled inline)", o)
	}
	if o := settledOutcomeOf(t, store, streamID, "lanes:0"); o != "" {
		t.Fatalf("lanes:0 = %q, want unsettled (parked on the direct do)", o)
	}

	// The direct do closes; one re-Advance drains the scatter and seals.
	fake.settleAct(t, "direct:0", engine.OutcomePass, "direct-done")
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if !r2.Sealed {
		t.Fatalf("advance 2 = %+v, want Sealed after the direct do settled", r2)
	}
	if r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r2.Run.Outcome)
	}
	if o := settledOutcomeOf(t, store, streamID, "lanes:0"); o != engine.OutcomePass {
		t.Fatalf("lanes:0 = %q, want pass (drained do + run members)", o)
	}
	// Pool + inline drop+refold byte-identity over a mixed scatter-run stream.
	assertProjectionEqualsRefold(t, store, streamID)
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestScatterRunMemberFailedOnFailStopFails pins the on_fail=stop path over a run
// member: a run whose sub-formula FAILS settles the run failed (transparent), and under
// on_fail=stop that fails the whole scatter even though a sibling passed.
func TestScatterRunMemberFailedOnFailStopFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		"",
		scatterNode("lanes", nil, "stop",
			settleNode("good", "pass"),
			runNodeJSON("extra", nil, "sub", "", "")),
		subDoc("sub", "", settleNode("boom", "failed")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["extra"] != engine.OutcomeFailed {
		t.Errorf("run member extra settled %q, want failed (transparent from the sub)", settled["extra"])
	}
	if settled["lanes"] != engine.OutcomeFailed {
		t.Errorf("scatter lanes settled %q, want failed (on_fail=stop, a run member failed)", settled["lanes"])
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestScatterRunMemberSkippedOnFailStopDegrades pins the ⚑ nuance: a run member that
// SKIPS (its environment gates on a failed node OUTSIDE the scatter) is didNotRun, not
// blocking — so under on_fail=stop the scatter DEGRADES rather than fails (a skipped
// member ≠ a failed member). The env ref is to an outside NODE (an input ref would create
// no gate), and that node is not a scatter member (so ⚑SF-1 does not refuse it).
func TestScatterRunMemberSkippedOnFailStopDegrades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		"",
		execNode("gate", "exit 1", nil)+","+
			scatterNode("lanes", nil, "stop",
				settleNode("good", "pass"),
				runNodeJSON("extra", nil, "sub", "topic", "gate")),
		subDoc("sub", strField("topic"), settleNode("done", "pass")),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["gate"] != engine.OutcomeFailed {
		t.Fatalf("gate settled %q, want failed", settled["gate"])
	}
	if settled["extra"] != engine.OutcomeSkipped {
		t.Errorf("run member extra settled %q, want skipped (env gate on the failed outside node)", settled["extra"])
	}
	// THE pin: a skipped run member does NOT trip on_fail=stop's blocking path.
	if settled["lanes"] != engine.OutcomeDegraded {
		t.Errorf("scatter lanes settled %q, want degraded (skipped run member ≠ blocking under on_fail=stop)", settled["lanes"])
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestGatherOverScatterWithRunMemberReadsTransparentOutput proves a gather draining a
// scatter that has a run member treats the run aggregate as opaque and the combine reads
// the run's transparent output `{{ extra }}` — the sub-formula's last-ran result plumbed
// out of the run boundary into the parent combine scope. Inline (engine.Run) drop+refold
// identity is asserted too.
func TestGatherOverScatterWithRunMemberReadsTransparentOutput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		"",
		scatterNode("lanes", nil, "continue",
			runNodeJSON("extra", nil, "sub", "", ""))+","+
			gatherNode("G", "lanes", []string{"lanes"},
				execNode("c", "echo got-{{extra}}", nil)),
		subDoc("sub", "", execNode("inner", "echo subval", nil)),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["extra"] != engine.OutcomePass {
		t.Errorf("run member extra settled %q, want pass", settled["extra"])
	}
	if settled["G"] != engine.OutcomePass {
		t.Errorf("gather G settled %q, want pass", settled["G"])
	}
	// THE plumbing pin: the combine read the run's transparent output.
	if got := res.NodeOutputs["c"]; got != "got-subval" {
		t.Errorf("combine c output = %q, want %q (combine read {{ extra }} = the run's transparent output)", got, "got-subval")
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestResumeMidScatterRunSubFormulaSealsIdentically pins the resume crash window inside a
// scatter-member run's sub-formula: a crash right after the run's sub-node settles
// resumes to the SAME sealed outcome, with a projection that drop+refolds identically —
// resume threads the bundle + sub-scope through the run boundary even one namespace deep
// under a scatter member.
func TestResumeMidScatterRunSubFormulaSealsIdentically(t *testing.T) {
	docJSON := bundleDoc(
		"",
		scatterNode("lanes", nil, "continue",
			settleNode("direct", "pass"),
			runNodeJSON("extra", nil, "sub", "", "")),
		subDoc("sub", "", execNode("inner", "echo subval", nil)),
	)

	// Uninterrupted baseline.
	base := newStore(t)
	want, err := engine.Run(context.Background(), base, decodeIR(t, docJSON), nil)
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}

	// Crash right after the run's sub-node extra/inner settles, then resume.
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), nil,
		engine.CrashAfterSettle, "extra/inner:0", 0)

	if resumed.Outcome != want.Outcome {
		t.Errorf("resumed outcome = %q, want %q", resumed.Outcome, want.Outcome)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	assertProjectionEqualsRefold(t, store, stream)
}
