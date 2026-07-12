package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// errInjectedFBRCrash is the sentinel the FBR after-dispatch crash test injects.
var errInjectedFBRCrash = errors.New("injected FBR crash after dispatch")

// --- for-each-body=run (FBR) behavioral fixtures ----------------------------
//
// The corpus marquee: a `fan: scatter reviewer in reviewers { lane: run reviewLane
// given {reviewer: <binder>, target: <input>} }` — each element mints the reviewLane
// sub-formula's WHOLE sub-graph under `<fanID>/<index>/`, the sub-do rendering the
// per-element binder AND the env-bound target. Member aggregates are transparent at
// `<fanID>/<index>:0` (the drain memberAct); the fan drains them like a scatter.

// NOTE: these behavioral tests build the fan via the shared forEachNode helper, whose
// node id is "fan" — so the runtime member namespaces here are fan/<i>/. The PLAN-test
// helpers (fbrFanNode) and the bundled e2e fixture use "fanout" to match the corpus.
//
// reviewLaneEnv is the corpus member run's environment: reviewer <- the binder
// (excluded from the ⚑B2 gate, supplied by withBinder), target <- a parent input.
func reviewLaneEnv() string {
	return `[` + envField("reviewer", "reviewer") + `,` + envField("target", "target") + `]`
}

// reviewLaneExecSub is the corpus sub-formula (exec body — inline, no host): accepts
// reviewer + target, one node rendering both.
func reviewLaneExecSub() string {
	return subDoc("reviewLane", strField("reviewer")+","+strField("target"),
		execNode("review", `echo "item={{ reviewer }} target={{ target }}"`, nil))
}

// reviewLaneDoSub is the corpus sub-formula (do body — pool): accepts reviewer +
// target, one do rendering both.
func reviewLaneDoSub() string {
	return subDoc("reviewLane", strField("reviewer")+","+strField("target"),
		doNode("review", "review {{ reviewer }} for {{ target }}", nil))
}

// TestForEachRunBodyCorpusInlineFans (§2.1 + §2.4 ⚑S1 root + §3 both-direction render
// pins) proves the marquee at ROOT: a fan over a 2-element input array whose member is a
// `run reviewLane` mints two member sub-graphs at fan/0/ and fan/1/, each sub-exec
// rendering its DISTINCT element (member 0 = alpha NOT beta; member 1 = beta NOT alpha)
// plus the env-bound target; the member aggregates settle transparent at fan/<i>:0, the
// fan drains pass. The empty-string parentNS override (⚑S1) is load-bearing here — without
// it every binding renders "" at root.
func TestForEachRunBodyCorpusInlineFans(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"alpha", "beta"}, "target": "release-7"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["fan/0/review"]; got != "item=alpha target=release-7" {
		t.Errorf("member 0 sub-do = %q, want %q (binder element + env-bound target)", got, "item=alpha target=release-7")
	}
	if got := res.NodeOutputs["fan/1/review"]; got != "item=beta target=release-7" {
		t.Errorf("member 1 sub-do = %q, want %q", got, "item=beta target=release-7")
	}
	// Both-direction render pins: no cross-element leak.
	if strings.Contains(res.NodeOutputs["fan/0/review"], "beta") {
		t.Errorf("member 0 = %q leaked element beta (per-member binding not isolated)", res.NodeOutputs["fan/0/review"])
	}
	if strings.Contains(res.NodeOutputs["fan/1/review"], "alpha") {
		t.Errorf("member 1 = %q leaked element alpha", res.NodeOutputs["fan/1/review"])
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan/0"] != engine.OutcomePass || settled["fan/1"] != engine.OutcomePass {
		t.Errorf("member aggregates fan/{0,1} = {%q,%q}, want {pass,pass} (transparent)", settled["fan/0"], settled["fan/1"])
	}
	if settled["fan"] != engine.OutcomePass {
		t.Errorf("fan aggregate fan = %q, want pass", settled["fan"])
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachRunBodyDownstreamReads (§2.1 downstream-reads clause, ROOT) pins the three
// downstream consumers of a run-bodied fan: (a) a TEMPLATE-form `{{fan}}` renders "" (the
// fan aggregate is nodeOutputs-only, never in the render scope — FIS/scatter parity) and
// the downstream render carries NO member output; (b) a sibling guard reading fan.outcome
// == "pass" runs its then; (c) post-fan binder restoration — a root input named like the
// binder renders its PRE-EXISTING value after the fan completes (the new withBinder call
// sites' save/restore).
func TestForEachRunBodyDownstreamReads(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// A silent interp whose TEMPLATE parts embed `{{fan}}`: text "v=" + ref fan.
	interpV := `{"kind":"interp","id":"v","name":"v","after":["fan"],` +
		`"parts":[{"kind":"text","value":"v="},{"kind":"interp","expr":{"kind":"ref","name":"fan"}}]}`
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target")+","+strField("reviewer"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv()))+","+
			interpV+","+
			execNode("probe", `echo "res={{ v }}"`, []string{"v"})+","+
			guardExecAfter("g", []string{"fan"}, condOutcomePass("fan"), "gthen", `echo "guard ran"`)+","+
			execNode("restore", `echo "r={{ reviewer }}"`, []string{"fan"}),
		reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{
		"reviewers": []any{"a", "b"}, "target": "t", "reviewer": "PREVAL",
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// (a) template-form {{fan}} renders "" — the probe echoes exactly "res=v=", and the
	// downstream render leaks NO member output (member outputs are item=a/item=b shapes).
	if got := res.NodeOutputs["probe"]; got != "res=v=" {
		t.Errorf("downstream probe = %q, want %q ({{fan}} renders empty)", got, "res=v=")
	}
	if strings.Contains(res.NodeOutputs["probe"], "item=") {
		t.Errorf("downstream probe = %q leaked a member output", res.NodeOutputs["probe"])
	}
	// (b) a sibling guard reads fan.outcome == "pass" and runs its then.
	if got := res.NodeOutputs["gthen"]; got != "guard ran" {
		t.Errorf("guard then = %q, want %q (fan.outcome==pass downstream read)", got, "guard ran")
	}
	// (c) the binder-named input is RESTORED after the fan (withBinder save/restore).
	if got := res.NodeOutputs["restore"]; got != "r=PREVAL" {
		t.Errorf("post-fan render = %q, want %q (binder restoration)", got, "r=PREVAL")
	}
}

// TestForEachRunBodyAggregateVisibilityInNs (§2.1 downstream reads INSIDE a namespace —
// the FIS §7 aggregate-visibility mirror over a RUN-bodied fan) pins ns visibility: a
// sub-sibling gated after:[fan] runs; a sibling guard reading fan.outcome == "pass" (the
// ⚑B1 condScope overlay) runs its then; and a TEMPLATE-form `{{fan}}` renders "" with NO
// member output leaking into the sibling render — the member aggregates (stage/fan/<i>)
// are '/'-bearing and INVISIBLE at the fan's ns level (directChildKey rejects them), so a
// regression exposing fan/0 to siblings must go red here.
func TestForEachRunBodyAggregateVisibilityInNs(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	interpV := `{"kind":"interp","id":"v","name":"v","after":["fan"],` +
		`"parts":[{"kind":"text","value":"v="},{"kind":"interp","expr":{"kind":"ref","name":"fan"}}]}`
	innerEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
	outer := subDoc("outerSub", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv))+","+
			guardExecAfter("g", []string{"fan"}, condOutcomePass("fan"), "gthen", `echo "guard ran"`)+","+
			execNode("after", `echo done`, []string{"fan"})+","+
			interpV+","+
			execNode("probe", `echo "res={{ v }}"`, []string{"v"}))
	stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("label"),
		runNodeRawEnv("stage", nil, "outerSub", stageEnv),
		outer+","+reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a"}, "label": "L"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/after"] != engine.OutcomePass {
		t.Errorf("sub-sibling stage/after = %q, want pass (after:[fan] gate settled)", settled["stage/after"])
	}
	if got := res.NodeOutputs["stage/gthen"]; got != "guard ran" {
		t.Errorf("guard then stage/gthen = %q, want %q (fan.outcome==pass via the ns overlay)", got, "guard ran")
	}
	// Template-form {{fan}} renders "" inside ns; member aggs/outputs stay invisible.
	if got := res.NodeOutputs["stage/probe"]; got != "res=v=" {
		t.Errorf("ns render probe = %q, want %q ({{fan}} renders empty inside ns)", got, "res=v=")
	}
	if strings.Contains(res.NodeOutputs["stage/probe"], "item=") {
		t.Errorf("ns probe = %q leaked a member output (member aggs must be ns-invisible)", res.NodeOutputs["stage/probe"])
	}
}

// TestAdvanceForEachRunBodyConcurrentDispatch (§2.3 ⚑B1 — the serialization-mutant killer)
// proves a 2-member pool run-bodied fan dispatches BOTH members' sub-dos in ONE Advance
// pass (dispatchCount == 2 before any close), each prompt rendered with its distinct
// element. An RBL-shaped early-return (park on the first in-flight member) would dispatch
// only member 0 → dispatchCount == 1.
func TestAdvanceForEachRunBodyConcurrentDispatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneDoSub()))

	r1, err := engine.Advance(ctx, store, doc, "gcg-fbr-concurrent", map[string]any{"reviewers": []any{"alpha", "beta"}, "target": "rel"}, fake.opts())
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed || !r1.Parked {
		t.Fatalf("advance 1 = %+v, want Parked (both members' sub-dos dispatched)", r1)
	}
	if fake.dispatchCount() != 2 || len(r1.InFlight) != 2 {
		t.Fatalf("dispatch=%d inFlight=%d, want 2 concurrent sub-dos in ONE pass (serialization mutant → 1)", fake.dispatchCount(), len(r1.InFlight))
	}
	if got := fake.dispatchPromptFor(t, "fan/0/review:0"); got != "review alpha for rel" {
		t.Errorf("member 0 sub-do prompt = %q, want %q", got, "review alpha for rel")
	}
	if got := fake.dispatchPromptFor(t, "fan/1/review:0"); got != "review beta for rel" {
		t.Errorf("member 1 sub-do prompt = %q, want %q", got, "review beta for rel")
	}

	// Both close → the member aggs + fan + run seal pass.
	fake.settleAct(t, "fan/0/review:0", engine.OutcomePass, "ok0")
	fake.settleAct(t, "fan/1/review:0", engine.OutcomePass, "ok1")
	r2, err := engine.Advance(ctx, store, doc, "gcg-fbr-concurrent", map[string]any{"reviewers": []any{"alpha", "beta"}, "target": "rel"}, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v err %v, want Sealed", r2, err)
	}
	if r2.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r2.Run.Outcome)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-fbr-concurrent"))
	if settled["fan/0"] != engine.OutcomePass || settled["fan/1"] != engine.OutcomePass || settled["fan"] != engine.OutcomePass {
		t.Errorf("settles = {m0:%q m1:%q fan:%q}, want all pass", settled["fan/0"], settled["fan/1"], settled["fan"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-fbr-concurrent")
}

// TestForEachRunBodyFailureDrainContinue (§2.6) proves a member run's sub-do FAILS → the
// member aggregate settles transparent failed → on_fail:continue drains the fan DEGRADED,
// and a FAILED member does NOT suppress the sibling's minting (both members ran).
func TestForEachRunBodyFailureDrainContinue(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ reviewer }}" = "bad" ]; then exit 1; fi; echo "ok {{ reviewer }}"`
	sub := subDoc("reviewLane", strField("reviewer")+","+strField("target"),
		execNode("review", fail, nil))
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"ok", "bad"}, "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan/0"] != engine.OutcomePass {
		t.Errorf("member 0 agg = %q, want pass", settled["fan/0"])
	}
	if settled["fan/1"] != engine.OutcomeFailed {
		t.Errorf("member 1 agg = %q, want failed (transparent from the failed sub-do)", settled["fan/1"])
	}
	if settled["fan"] != engine.OutcomeDegraded {
		t.Errorf("fan = %q, want degraded (on_fail:continue, mixed pass/fail)", settled["fan"])
	}
	// The failed member did not suppress the sibling: both sub-dos ran.
	if _, ok := settled["fan/0/review"]; !ok {
		t.Errorf("member 0 sub-do never ran; a failed sibling must not suppress minting")
	}
}

// TestForEachRunBodyFailureDrainStop (§2.6) proves on_fail:stop fails the fan when a member
// run's sub-do fails, and BOTH members still mint (scatter drains everything; on_fail
// affects only the settle outcome, never mint suppression).
func TestForEachRunBodyFailureDrainStop(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ reviewer }}" = "bad" ]; then exit 1; fi; echo ok`
	sub := subDoc("reviewLane", strField("reviewer")+","+strField("target"),
		execNode("review", fail, nil))
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "stop", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"bad", "ok"}, "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan"] != engine.OutcomeFailed {
		t.Errorf("fan = %q, want failed (on_fail:stop, a member sub failed)", settled["fan"])
	}
	// Both members minted despite element 0 failing.
	if _, ok := settled["fan/0/review"]; !ok {
		t.Errorf("member 0 sub-do missing")
	}
	if _, ok := settled["fan/1/review"]; !ok {
		t.Errorf("member 1 sub-do missing (a failed member 0 must not abort the fan mint)")
	}
}

// TestAdvanceForEachRunBodyFailedMemberNoSuppression (§2.6 ⚑B1(b), POOL side — the
// break-on-failed mutant killer) proves a member whose aggregate settled FAILED does not
// suppress driving the others: member 0's bead closes FAILED while member 1 is still in
// flight → the next Advance settles member 0's aggregate failed AND keeps member 1 parked
// (no error, no suppression) → member 1 closes pass → the fan drains DEGRADED (on_fail
// continue) and seals. The mutant this kills: an added break/return-on-first-failed-agg
// in advanceForEachRunBody's member loop — pass 3 would return before observing member
// 1's closed bead and park forever instead of sealing.
func TestAdvanceForEachRunBodyFailedMemberNoSuppression(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"reviewers": []any{"a", "b"}, "target": "t"}
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneDoSub()))

	// Pass 1: both members' sub-dos dispatch concurrently.
	r1, err := engine.Advance(ctx, store, doc, "gcg-fbr-failns", input, fake.opts())
	if err != nil || !r1.Parked || fake.dispatchCount() != 2 {
		t.Fatalf("advance 1 = %+v err %v dispatch=%d, want Parked with 2 dispatches", r1, err, fake.dispatchCount())
	}

	// Member 0 FAILS while member 1 is still open. Pass 2: member 0's aggregate settles
	// failed in-pass, member 1 stays in flight — parked, no error, no suppression.
	fake.settleAct(t, "fan/0/review:0", engine.OutcomeFailed, "nope")
	r2, err := engine.Advance(ctx, store, doc, "gcg-fbr-failns", input, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 = %+v err %v, want Parked on member 1 (a failed member 0 must not error/suppress)", r2, err)
	}
	mid := settledOutcomeByID(t, streamStored(t, store, "gcg-fbr-failns"))
	if mid["fan/0"] != engine.OutcomeFailed {
		t.Fatalf("member 0 aggregate = %q after pass 2, want failed (settled in-pass)", mid["fan/0"])
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "fan/1/review:0" {
		t.Fatalf("in-flight after pass 2 = %+v, want only fan/1/review:0 (member 1 still driven)", r2.InFlight)
	}

	// Member 1 passes → pass 3 observes it, drains the fan DEGRADED, and seals.
	fake.settleAct(t, "fan/1/review:0", engine.OutcomePass, "ok")
	r3, err := engine.Advance(ctx, store, doc, "gcg-fbr-failns", input, fake.opts())
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v err %v, want Sealed (the break-on-failed mutant parks forever here)", r3, err)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-fbr-failns"))
	if settled["fan/1"] != engine.OutcomePass {
		t.Errorf("member 1 aggregate = %q, want pass (driven despite the failed sibling)", settled["fan/1"])
	}
	if settled["fan"] != engine.OutcomeDegraded {
		t.Errorf("fan = %q, want degraded (on_fail continue over failed+pass)", settled["fan"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-fbr-failns")
}

// TestForEachRunBodyFailureDrainInsideNs (§2.6's "both, inside ns" clause) proves the
// failure-drain rule holds one namespace deep: a run-bodied fan inside a run sub-formula
// with one failing member drains DEGRADED under on_fail:continue and FAILED under
// on_fail:stop, the enclosing run reporting each transparently.
func TestForEachRunBodyFailureDrainInsideNs(t *testing.T) {
	fail := `if [ "{{ reviewer }}" = "bad" ]; then exit 1; fi; echo ok`
	lane := subDoc("reviewLane", strField("reviewer")+","+strField("target"),
		execNode("review", fail, nil))
	for _, tc := range []struct {
		onFail  string
		wantFan string
		wantRun string
	}{
		{"continue", engine.OutcomeDegraded, engine.OutcomeDegraded},
		{"stop", engine.OutcomeFailed, engine.OutcomeFailed},
	} {
		t.Run(tc.onFail, func(t *testing.T) {
			ctx := context.Background()
			store := newStore(t)
			innerEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
			outer := subDoc("outerSub", arrField("arr")+","+strField("tag"),
				forEachNode(nil, "r", tc.onFail, refOver("arr"),
					runNodeRawEnv("lane", nil, "reviewLane", innerEnv)))
			stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
			doc := decodeIR(t, bundleDoc(
				arrField("items")+","+strField("label"),
				runNodeRawEnv("stage", nil, "outerSub", stageEnv),
				outer+","+lane))
			res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"ok", "bad"}, "label": "L"})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			settled := settledOutcomeByID(t, res.Events)
			if settled["stage/fan/1"] != engine.OutcomeFailed {
				t.Errorf("ns member 1 agg = %q, want failed (transparent from the failed sub)", settled["stage/fan/1"])
			}
			if settled["stage/fan"] != tc.wantFan {
				t.Errorf("ns fan = %q, want %q (on_fail %s)", settled["stage/fan"], tc.wantFan, tc.onFail)
			}
			if settled["stage"] != tc.wantRun {
				t.Errorf("enclosing run stage = %q, want %q (transparent)", settled["stage"], tc.wantRun)
			}
		})
	}
}

// TestForEachRunBodyEmptyTargetSubPasses (§2.8 NICE pin) proves a run member whose target
// sub-formula lowers to ZERO non-silent units (only a silent lit) settles the member
// aggregate PASS — static-run parity (a run over an empty sub-formula passes vacuously).
func TestForEachRunBodyEmptyTargetSubPasses(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	silentOnly := `"emptyLane":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"test"},` +
		`"name":"emptyLane","input":{"name":"emptyLane.input","fields":[` + strField("reviewer") + `]},` +
		`"nodes":[{"kind":"lit","id":"note","name":"note","after":[],"value":{"kind":"literal","value":"x"}}]}`
	env := `[` + envField("reviewer", "reviewer") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "emptyLane", env)),
		silentOnly))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan/0"] != engine.OutcomePass || settled["fan/1"] != engine.OutcomePass {
		t.Errorf("member aggs = {%q,%q}, want {pass,pass} (empty sub-formula passes vacuously)", settled["fan/0"], settled["fan/1"])
	}
	if settled["fan"] != engine.OutcomePass {
		t.Errorf("fan = %q, want pass", settled["fan"])
	}
}

// TestForEachRunBodyDuplicateElements (§2.9) proves duplicate over elements fan to DISTINCT
// member namespaces by index: two "a" elements mint fan/0/ and fan/1/ independently.
func TestForEachRunBodyDuplicateElements(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"a", "a"}, "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.NodeOutputs["fan/0/review"] != "item=a target=t" || res.NodeOutputs["fan/1/review"] != "item=a target=t" {
		t.Errorf("dup members = {%q,%q}, want both item=a target=t at distinct namespaces", res.NodeOutputs["fan/0/review"], res.NodeOutputs["fan/1/review"])
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan/0"] != engine.OutcomePass || settled["fan/1"] != engine.OutcomePass {
		t.Errorf("dup member aggs = {%q,%q}, want distinct pass members", settled["fan/0"], settled["fan/1"])
	}
}

// TestForEachRunBodyInsideRunNs (§2.4 depth composition) proves a run-bodied fan lowers and
// drives one namespace DEEP: `run stage -> reviewer{ fan: scatter r in arr { lane: run
// reviewLane given {...} } }`. The fan's ns is stage/, its members mint at stage/fan/<i>/,
// and the sub-do renders through TWO run boundaries.
func TestForEachRunBodyInsideRunNs(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	innerEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
	reviewer := subDoc("reviewer", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv)))
	stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("label"),
		runNodeRawEnv("stage", nil, "reviewer", stageEnv),
		reviewer+","+reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}, "label": "L"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/fan/0/review"]; got != "item=a target=L" {
		t.Errorf("deep member 0 = %q, want item=a target=L (rendered through two run boundaries)", got)
	}
	if got := res.NodeOutputs["stage/fan/1/review"]; got != "item=b target=L" {
		t.Errorf("deep member 1 = %q, want item=b target=L", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass || settled["stage"] != engine.OutcomePass {
		t.Errorf("stage/fan=%q stage=%q, want both pass", settled["stage/fan"], settled["stage"])
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachRunBodyInsideRepeatAttemptNsInline (§2.4 depth, Q-A in-process) proves a
// run-bodied fan drives inside a repeat-run-body ATTEMPT namespace on the inline driver: the
// fan mints its run members at stage/0/fan/<i>/, the attempt aggregate + loop settle pass in
// ONE attempt. The attempt-ns parentNS override chains the fan's own member override.
func TestForEachRunBodyInsideRepeatAttemptNsInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	innerEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
	reviewer := subDoc("reviewer", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv)))
	stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("label"),
		repeatRunLoop(nil, runNodeRawEnv("stage", nil, "reviewer", stageEnv), runCondPassOrIter()),
		reviewer+","+reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}, "label": "L"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/0/fan/0/review"]; got != "item=a target=L" {
		t.Errorf("attempt-0 deep member 0 = %q, want item=a target=L", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/fan"] != engine.OutcomePass {
		t.Errorf("attempt-0 fan stage/0/fan = %q, want pass", settled["stage/0/fan"])
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestAdvanceForEachRunBodyInsideRepeatAttemptNs (§2.4 depth, advance twin) proves the same
// composition drives under a PoolRouter: the fan inside the repeat attempt ns dispatches its
// two run-member sub-dos CONCURRENTLY at stage/0/fan/<i>/review:0.
func TestAdvanceForEachRunBodyInsideRepeatAttemptNs(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	innerEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
	reviewer := subDoc("reviewer", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv)))
	stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("label"),
		repeatRunLoop(nil, runNodeRawEnv("stage", nil, "reviewer", stageEnv), runCondPassOrIter()),
		reviewer+","+reviewLaneDoSub()))
	r1, err := engine.Advance(ctx, store, doc, "gcg-fbr-in-attempt", map[string]any{"items": []any{"a", "b"}, "label": "L"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if r1.Sealed || !r1.Parked {
		t.Fatalf("advance = %+v, want Parked (two deep members dispatched)", r1)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count = %d, want 2 (concurrent members inside the attempt ns)", fake.dispatchCount())
	}
	if got := fake.dispatchPromptFor(t, "stage/0/fan/0/review:0"); got != "review a for L" {
		t.Errorf("deep member 0 prompt = %q, want %q", got, "review a for L")
	}
}

// TestAdvanceForEachRunBodyCrossPassShadow (§2.2a ⚑S2 cross-pass shadow) proves the
// per-member withBinder window holds ACROSS passes: a member sub-do first materialized on
// pass 2 renders elems[i], NOT a same-named node that settled between passes. The binder is
// `item`; a ROOT pool-do node ALSO named `item` settles on pass 2; the member sub-formula's
// SECOND do (chained after a first do that parks pass 1) renders `{{ item }}` on pass 2 — it
// must resolve the element, shadowing the root node's output.
func TestAdvanceForEachRunBodyCrossPassShadow(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("reviewLane", strField("item"),
		doNode("first", "prep {{ item }}", nil)+","+
			doNode("second", "use {{ item }}", []string{"first"}))
	env := `[` + envField("item", "item") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("arr"),
		// A ROOT pool-do node literally named `item` (the binder name), ungated by the fan
		// (the binder is excluded from the ⚑B2 gate), so it settles independently between passes.
		doNode("item", "root item work", nil)+","+
			forEachNode(nil, "item", "continue", refOver("arr"),
				runNodeRawEnv("lane", nil, "reviewLane", env)),
		sub))

	// Pass 1: root `item` + both members' `first` dispatch; each member's `second` defers.
	r1, err := engine.Advance(ctx, store, doc, "gcg-fbr-shadow", map[string]any{"arr": []any{"a", "b"}}, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	// Settle root item (ROOTVAL) and both firsts BETWEEN passes.
	fake.settleAct(t, "item:0", engine.OutcomePass, "ROOTVAL")
	fake.settleAct(t, "fan/0/first:0", engine.OutcomePass, "f0")
	fake.settleAct(t, "fan/1/first:0", engine.OutcomePass, "f1")

	// Pass 2: root item settles (scope[item]=ROOTVAL, topo before the fan); each member's
	// `second` materializes INSIDE the re-established withBinder window → renders the element.
	r2, err := engine.Advance(ctx, store, doc, "gcg-fbr-shadow", map[string]any{"arr": []any{"a", "b"}}, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 = %+v err %v, want Parked (on the seconds)", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "fan/0/second:0"); got != "use a" {
		t.Fatalf("member 0 second prompt = %q, want %q (binder shadows the settled root node)", got, "use a")
	}
	if got := fake.dispatchPromptFor(t, "fan/1/second:0"); got != "use b" {
		t.Fatalf("member 1 second prompt = %q, want %q", got, "use b")
	}
}

// TestForEachRunBodyResumeGuardReadsBinder (§2.2b ⚑S2 resume variant) proves a guard INSIDE
// a member sub-formula whose cond reads the binder-bound sub-input is decided correctly on a
// post-crash RESUME pass: crash after member 0's aggregate settles; on resume member 1 is
// minted+driven fresh and its guard re-decides reading item="nomatch" inside the
// re-established withBinder window (condScope → runInputLayer(memberNS) → binder).
func TestForEachRunBodyResumeGuardReadsBinder(t *testing.T) {
	sub := subDoc("reviewLane", strField("item"),
		guardExecAfter("g", nil, condEqualRaw("item", `"match"`), "gthen", `echo "hit {{ item }}"`))
	env := `[` + envField("item", "item") + `]`
	// The array comes from a ROOT node so the run takes NO input (injectCrashThenResume
	// threads nil input).
	docJSON := bundleDoc(
		"",
		execNode("src", `printf '["match","nomatch"]'`, nil)+","+
			forEachNode(nil, "item", "continue", refOver("src"),
				runNodeRawEnv("lane", nil, "reviewLane", env)),
		sub)

	// Baseline (uninterrupted).
	base := newStore(t)
	want, err := engine.Run(context.Background(), base, decodeIR(t, docJSON), nil)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	if want.Outcome != engine.OutcomePass {
		t.Fatalf("baseline outcome = %q, want pass", want.Outcome)
	}

	// Crash right after member 0's aggregate settles, then resume (member 1's guard first
	// decides on the resume pass, reading item="nomatch" → FALSE, no branch).
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), nil,
		engine.CrashAfterSettle, "fan/0:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	// Member 0 (item=match): guard TRUE → gthen ran "hit match".
	if resumed.NodeOutputs["fan/0/gthen"] != "hit match" {
		t.Errorf("member 0 gthen = %q, want %q (guard TRUE reading item=match)", resumed.NodeOutputs["fan/0/gthen"], "hit match")
	}
	// Member 1 (item=nomatch): the guard decided FALSE on the RESUME pass → pass, no branch.
	if settled["fan/1/g"] != engine.OutcomePass {
		t.Errorf("member 1 guard = %q, want pass (FALSE decided on resume, no branch)", settled["fan/1/g"])
	}
	if _, ran := settled["fan/1/gthen"]; ran {
		t.Errorf("member 1 gthen ran; the guard cond (item=nomatch) must decide FALSE on resume")
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestForEachRunBodyCrashMidMemberResumes (§2.7) proves a crash mid-fan (member 0's sub-do
// settled, its aggregate not yet) resumes convergently: member 0's sub-do is reloaded by
// exact key (never re-invoked), member 1 is re-minted fresh, and the run seals identically
// with a byte-identical drop+refold.
func TestForEachRunBodyCrashMidMemberResumes(t *testing.T) {
	sub := subDoc("reviewLane", strField("reviewer"),
		doNode("review", "review {{ reviewer }}", nil))
	env := `[` + envField("reviewer", "reviewer") + `]`
	// The array comes from a ROOT node so the run takes NO input (injectCrashThenResume
	// threads nil input); the sub-dos are pool-shaped so the crash boundary has an effect.
	docJSON := bundleDoc(
		"",
		execNode("src", `printf '["a","b"]'`, nil)+","+
			forEachNode(nil, "reviewer", "continue", refOver("src"),
				runNodeRawEnv("lane", nil, "reviewLane", env)),
		sub)
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"fan/0/review": {Outcome: enginehost.OutcomePass, Output: "r0"},
		"fan/1/review": {Outcome: enginehost.OutcomePass, Output: "r1"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), host,
		engine.CrashAfterSettle, "fan/0/review:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	if resumed.NodeOutputs["fan/0/review"] != "r0" || resumed.NodeOutputs["fan/1/review"] != "r1" {
		t.Errorf("resumed members = {%q,%q}, want {r0,r1}", resumed.NodeOutputs["fan/0/review"], resumed.NodeOutputs["fan/1/review"])
	}
	// member 0 reloaded (not re-invoked), member 1 minted once: exactly 2 host calls.
	if calls := host.Calls(); len(calls) != 2 {
		t.Errorf("host calls = %d, want 2 (member 0 reloaded, member 1 minted once)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestAdvanceForEachRunBodyCrashAfterDispatchReAdopts (§2.7 crash window, pool) pins the
// §9.1 window over a fan member: member 0's work bead is created, the crash fires BEFORE
// its owned.admitted fact commits (and before member 1 dispatches at all). The re-Advance
// re-looks-up member 0's bead (the seam returns the SAME id — no duplicate bead) AND
// first-dispatches member 1, all in ONE pass; both then close and the run converges with a
// byte-identical refold.
func TestAdvanceForEachRunBodyCrashAfterDispatchReAdopts(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"reviewers": []any{"a", "b"}, "target": "t"}
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneDoSub()))

	// Crash exactly at the after-dispatch boundary for member 0's sub-do.
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == "fan/0/review:0" {
			return errInjectedFBRCrash
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, "gcg-fbr-crashdisp", input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	// Member 0's bead was created; its dispatch fact never committed; member 1 never dispatched.
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1 (member 0 only)", fake.dispatchCount())
	}

	// Re-Advance: member 0 re-looked-up (SAME bead, no dup) + member 1 first-dispatched,
	// in one pass.
	r2, err := engine.Advance(ctx, store, doc, "gcg-fbr-crashdisp", input, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 2 {
		t.Fatalf("re-advance = %+v err %v, want Parked with BOTH members in flight", r2, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 2 {
		t.Fatalf("distinct beads minted = %d, want 2 (member 0 re-adopted, not re-minted)", minted)
	}

	// Both close pass → seal, refold identity.
	fake.settleAllDispatchedPass()
	r3, err := engine.Advance(ctx, store, doc, "gcg-fbr-crashdisp", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", r3, err)
	}
	assertProjectionEqualsRefold(t, store, "gcg-fbr-crashdisp")
}

// TestForEachRunBodyCrashAfterActAtMostOnce (§2.7 crash window, inline — the crash_test
// boundary-matrix CrashAfterAct cell over a two-level dynamic id) pins the interrupted-act
// window on a member sub-do: the host RAN member 0's do but the crash fired before its
// settlement committed. Resume settles it FAILED under at-most-once (never re-invoking the
// host — exactly 2 calls total across crash+resume), member 0's aggregate drains failed
// transparently, member 1 runs fresh, and the fan drains degraded (on_fail continue).
func TestForEachRunBodyCrashAfterActAtMostOnce(t *testing.T) {
	sub := subDoc("reviewLane", strField("reviewer"),
		doNode("review", "review {{ reviewer }}", nil))
	env := `[` + envField("reviewer", "reviewer") + `]`
	docJSON := bundleDoc(
		"",
		execNode("src", `printf '["a","b"]'`, nil)+","+
			forEachNode(nil, "reviewer", "continue", refOver("src"),
				runNodeRawEnv("lane", nil, "reviewLane", env)),
		sub)
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"fan/0/review": {Outcome: enginehost.OutcomePass, Output: "r0"},
		"fan/1/review": {Outcome: enginehost.OutcomePass, Output: "r1"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), host,
		engine.CrashAfterAct, "fan/0/review:0", 0)
	settled := settledOutcomeByID(t, resumed.Events)
	// The interrupted effect settles FAILED (at-most-once) — never re-run.
	if settled["fan/0/review"] != engine.OutcomeFailed {
		t.Errorf("interrupted member sub-do = %q, want failed (at-most-once)", settled["fan/0/review"])
	}
	if settled["fan/0"] != engine.OutcomeFailed {
		t.Errorf("member 0 aggregate = %q, want failed (transparent)", settled["fan/0"])
	}
	if settled["fan/1"] != engine.OutcomePass {
		t.Errorf("member 1 aggregate = %q, want pass (driven fresh on resume)", settled["fan/1"])
	}
	if settled["fan"] != engine.OutcomeDegraded {
		t.Errorf("fan = %q, want degraded (on_fail continue over failed+pass)", settled["fan"])
	}
	// Host called exactly twice: member 0 ONCE (before the crash), member 1 once on resume.
	if calls := host.Calls(); len(calls) != 2 {
		t.Errorf("host calls = %d across crash+resume, want 2 (at-most-once for the interrupted member)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestForEachRunBodyThreeLevelCorpusChain (§2.4, the exact pilot-review chain in-process)
// proves the parentNS-override chain through THREE dynamic/static boundaries on one driver:
// a repeat-run-body attempt ns (stage/0/) containing a STATIC run (mid/) whose sub-formula
// holds a run-bodied fan — member mints at stage/0/mid/fan/<i>/, the sub-exec rendering the
// per-element binder AND an env value threaded through all three env seams. Seals pass with
// a byte-identical refold.
func TestForEachRunBodyThreeLevelCorpusChain(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	laneEnv := `[` + envField("reviewer", "r") + `,` + envField("target", "tag") + `]`
	midSub := subDoc("midSub", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", laneEnv)))
	midEnv := `[` + envField("arr", "arr") + `,` + envField("tag", "tag") + `]`
	outerSub := subDoc("outerSub", arrField("arr")+","+strField("tag"),
		runNodeRawEnv("mid", nil, "midSub", midEnv))
	stageEnv := `[` + envField("arr", "items") + `,` + envField("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("label"),
		repeatRunLoop(nil, runNodeRawEnv("stage", nil, "outerSub", stageEnv), runCondPassOrIter()),
		outerSub+","+midSub+","+reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}, "label": "L"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// The member sub-execs render through attempt ns → static-run ns → fan-member ns.
	if got := res.NodeOutputs["stage/0/mid/fan/0/review"]; got != "item=a target=L" {
		t.Errorf("3-level member 0 = %q, want item=a target=L (env chained through all three seams)", got)
	}
	if got := res.NodeOutputs["stage/0/mid/fan/1/review"]; got != "item=b target=L" {
		t.Errorf("3-level member 1 = %q, want item=b target=L", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/0/mid/fan"] != engine.OutcomePass {
		t.Errorf("3-level fan = %q, want pass", settled["stage/0/mid/fan"])
	}
	if settled["stage/0/mid"] != engine.OutcomePass || settled["stage"] != engine.OutcomePass {
		t.Errorf("mid=%q stage=%q, want both pass", settled["stage/0/mid"], settled["stage"])
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachRunBodyDropRefold (§2.7 DET) pins that the two-level '/'-bearing dynamic rows
// (member aggregates fan/<i>:0 and their sub-nodes fan/<i>/review:0) refold
// byte-identically.
func TestForEachRunBodyDropRefold(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneExecSub()))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{"a", "b", "c"}, "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachRunBodyExecPoolInlineJournalParity (§2.7 / §1.5 STATELESS-events parity) proves
// an EXEC-only run-bodied fan driven inline (Run) and pooled (Advance — it routes to the
// run-body arm and seals in one pass, never dispatching) journals BYTE-IDENTICALLY after
// run.started, and NEITHER driver emits attempt.minted or node.decision (Q-C stateless).
func TestForEachRunBodyExecPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneExecSub()))
	input := map[string]any{"reviewers": []any{"a", "b"}, "target": "t"}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-fbr-parity", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec run body seals inline)", r, err)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an exec run-body fan dispatches nothing)", fake.dispatchCount())
	}
	if n := countAttemptMinted(inRes.Events); n != 0 {
		t.Fatalf("inline attempt.minted = %d, want 0 (Q-C stateless: no per-member attempt.minted)", n)
	}
	if n := countAttemptMinted(r.Run.Events); n != 0 {
		t.Fatalf("pool attempt.minted = %d, want 0 (Q-C stateless)", n)
	}
	for _, e := range inRes.Events {
		if e.Type == engine.EventNodeDecision {
			t.Fatalf("inline emitted node.decision; a run-bodied fan decides nothing (Q-C)")
		}
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestAdvanceForEachRunBodyWriteOnceRedundantAdvance (§2.7 dedupe) proves a redundant Advance
// with NO new settlement is a no-op: the members stay in flight, no re-dispatch, no head
// movement — the stateless re-mint + write-once appends dedupe cleanly.
func TestAdvanceForEachRunBodyWriteOnceRedundantAdvance(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		arrField("reviewers")+","+strField("target"),
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", reviewLaneEnv())),
		reviewLaneDoSub()))
	r1, err := engine.Advance(ctx, store, doc, "gcg-fbr-writeonce", map[string]any{"reviewers": []any{"a", "b"}, "target": "t"}, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	head := r1.Head
	for i := 0; i < 2; i++ {
		r, err := engine.Advance(ctx, store, doc, "gcg-fbr-writeonce", map[string]any{"reviewers": []any{"a", "b"}, "target": "t"}, fake.opts())
		if err != nil || !r.Parked {
			t.Fatalf("redundant advance %d = %+v err %v, want Parked", i, r, err)
		}
		if r.Head != head {
			t.Fatalf("redundant advance %d moved head %d -> %d (a double append)", i, head, r.Head)
		}
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count across redundant advances = %d, want 2 (write-once)", fake.dispatchCount())
	}
}

// TestForEachRunBodyMemberContainsLeafForEach (§2.11 nested composition) proves a member
// run sub-formula that itself contains a leaf-bodied for-each is legal (composition by
// induction): the outer fan mints reviewLane per element, and reviewLane's own inner fan
// fans over a sub-input array.
func TestForEachRunBodyMemberContainsLeafForEach(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// reviewLane accepts an array `lines`, and its body is a LEAF for-each over it.
	sub := subDoc("reviewLane", arrField("lines"),
		forEachNode(nil, "line", "continue", refOver("lines"),
			execNode("emit", `echo "L={{ line }}"`, nil)))
	env := `[` + envField("lines", "reviewer") + `]` // bind the sub array-input from the outer binder
	doc := decodeIR(t, bundleDoc(
		// outer array of arrays: each element is itself a JSON array string.
		`{"name":"reviewers","type":{"kind":"array","element":{"kind":"array","element":{"kind":"atomic","name":"string"}}},"required":true,"body":false}`,
		forEachNode(nil, "reviewer", "continue", refOver("reviewers"),
			runNodeRawEnv("lane", nil, "reviewLane", env)),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"reviewers": []any{[]any{"x", "y"}, []any{"z"}}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// Outer member 0's inner fan fanned 2 lines; member 1's inner fan fanned 1.
	if got := res.NodeOutputs["fan/0/fan/0"]; got != "L=x" {
		t.Errorf("member 0 inner line 0 = %q, want L=x", got)
	}
	if got := res.NodeOutputs["fan/0/fan/1"]; got != "L=y" {
		t.Errorf("member 0 inner line 1 = %q, want L=y", got)
	}
	if got := res.NodeOutputs["fan/1/fan/0"]; got != "L=z" {
		t.Errorf("member 1 inner line 0 = %q, want L=z", got)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}
