package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- pending-loop IR + script helpers ---------------------------------------

// execNodeExitPending renders an exec node whose exitMap declares pass AND pending
// exit-code sets (empty retryable) — the engine-inline entry path for OutcomePending.
func execNodeExitPending(id, script string, pass, pending []int) string {
	scriptJSON, _ := json.Marshal(script)
	passJSON, _ := json.Marshal(pass)
	pendingJSON, _ := json.Marshal(pending)
	return `{
      "kind": "exec", "id": "` + id + `", "name": "` + id + `", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(scriptJSON) + `, "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "exitMap": {"pass": ` + string(passJSON) + `, "retryable": [], "pending": ` + string(pendingJSON) + `}
    }`
}

// phasedExecScript returns a bash body that echoes "iter-{{iteration}}" and exits with
// exits[min(run-1, len-1)] on its Nth run, using an on-disk counter so the phase
// persists across attempts (each attempt is a fresh unit). It is the exec analog of the
// pending-then-pass / fail-fail-pass pool worker.
func phasedExecScript(t *testing.T, exits []int) string {
	t.Helper()
	return phasedExecScriptEcho(t, "iter-{{iteration}}", exits)
}

// phasedExecScriptEcho is phasedExecScript with a caller-chosen echo line, so a body can
// reference its OWN recorded output (e.g. echo "R{{draft}}") to exercise scope pollution.
func phasedExecScriptEcho(t *testing.T, echo string, exits []int) string {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "phaseflag")
	var arms strings.Builder
	for i, code := range exits {
		if i == len(exits)-1 {
			fmt.Fprintf(&arms, `*) exit %d;; `, code)
		} else {
			fmt.Fprintf(&arms, `%d) exit %d;; `, i+1, code)
		}
	}
	return `c=0; [ -f "` + flag + `" ] && c=$(cat "` + flag + `"); c=$((c+1)); printf '%s' "$c" > "` + flag +
		`"; echo "` + echo + `"; case "$c" in ` + arms.String() + `esac`
}

// guardPendingThen builds a guard node whose `then` is a caller-supplied node — used to
// place a pending exec on the guard-then decode site (external package has no guardNode).
func guardPendingThen(cond, then string) string {
	return `{"kind":"guard","id":"g","name":"g","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"cond":` + cond + `,"then":` + then + `}`
}

// condLaneOutcomePassOrIter builds `lane.outcome == "pass" || iteration >= n` over the
// fixed pool body id "lane".
func condLaneOutcomePassOrIter(n int) string {
	nJSON, _ := json.Marshal(n)
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"lane","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":` + string(nJSON) + `}]}]}`
}

// condLaneOutcomePass builds `lane.outcome == "pass"` — no iteration escape, so a body
// that never passes drives to a cap (the spin / physical-cap fixtures).
func condLaneOutcomePass() string {
	return `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"lane","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]}`
}

// repeatStr returns a slice of n copies of s (small phase-list builder).
func repeatStr(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// --- ENGINE-INLINE exec pending path ----------------------------------------

// TestRepeatExecPendingThenPassInline proves the exec entry path: a repeat over an exec
// whose exitMap.pending classifies exit 75 as pending settles draft:0 PENDING (a poll)
// then draft:1 PASS, and the loop exits pass in exactly 2 physical attempts — the pending
// poll did NOT burn the budget. The rendered iteration is the CONSUMING count (the poll
// is iteration 1, the pass is also iteration 1 since the poll did not consume).
func TestRepeatExecPendingThenPassInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// exit 75 (pending) on run 1, exit 0 (pass) on run 2+.
	body := execNodeExitPending("draft", phasedExecScript(t, []int{75, 0}), []int{0}, []int{75})
	loop := repeatNode(body, condLaneDraftOutcomePassOrIter())
	doc := decodeIR(t, blockDoc("pend-exec", loop))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	got := settledActivations(t, res.Events)
	want := [][2]string{
		{"draft:0", engine.OutcomePending},
		{"draft:1", engine.OutcomePass},
		{"repeat_1:0", engine.OutcomePass},
	}
	if len(got) != len(want) {
		t.Fatalf("settled activations = %v, want %v (pending poll then pass, no third attempt)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("settled[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	// The poll (draft:0) and the pass (draft:1) both rendered iteration 1 — the pending
	// did not advance the consuming count.
	outputs := settledOutputByActivation(t, res.Events)
	if outputs["draft:0"] != "iter-1" {
		t.Errorf("poll attempt draft:0 output = %q, want iter-1 (poll is iteration 1)", outputs["draft:0"])
	}
	if outputs["draft:1"] != "iter-1" {
		t.Errorf("pass attempt draft:1 output = %q, want iter-1 (the poll did NOT consume, so the pass is still iteration 1)", outputs["draft:1"])
	}
}

// TestRepeatExecPendingRefusedOffRepeatBody proves the REPEAT-LEAF-BODY scoping wall:
// exitMap.pending is refused at lowering (ErrUnsupportedNode, zero journal) on a retry
// body, a top-level exec (outside any loop), and an exec buried in a repeat's RUN body
// (leaf-body-only). Only a repeat leaf body may declare it.
func TestRepeatExecPendingRefusedOffRepeatBody(t *testing.T) {
	ctx := context.Background()
	pendBody := func(id string) string {
		return execNodeExitPending(id, "exit 75", []int{0}, []int{75})
	}
	cases := []struct {
		name string
		doc  string
	}{
		{"top-level", blockDoc("pf-top", pendBody("draft"))},
		{"retry-body", blockDoc("pf-retry", retryNode(`{"kind":"literal","value":3}`, pendBody("draft")))},
		{
			"run-body-inner-exec",
			bundleDoc(
				strField("who"),
				repeatRunLoop(nil, runNodeJSON("stage", nil, "greeter", "name", "who"), runCondPassOrIter()),
				subDoc("greeter", strField("name"), pendBody("inner")),
			),
		},
		// Representative allowPending=false decode sites beyond top-level/retry/run-body: a
		// scatter member, a guard `then`, and a for-each body all refuse a pending exec at
		// lowering (each routes through a distinct decodeExec call site).
		{"scatter-member", blockDoc("pf-scatter", scatterNode("s", nil, "continue", pendBody("m")))},
		{"guard-then", blockDoc("pf-guard", guardPendingThen(condEqualRaw("who", `"world"`), pendBody("gthen")))},
		{"foreach-body", bundleDoc(arrField("items"), forEachNode(nil, "item", "continue", refOver("items"), pendBody("fm")), "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			doc := decodeIR(t, tc.doc)
			input := map[string]any{"who": "world", "items": []any{"a"}}
			_, err := engine.RunWithOptions(ctx, store, doc, input, engine.Options{Host: &enginehost.StubHost{}})
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("err = %v, want ErrUnsupportedNode (pending refused off a repeat leaf body)", err)
			}
			if !strings.Contains(err.Error(), "exitMap.pending") {
				t.Errorf("error = %v, want it to name exitMap.pending", err)
			}
			var journalRows int
			_ = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows)
			if journalRows != 0 {
				t.Errorf("a refused pending body wrote %d journal rows, want 0 (pre-effect refusal)", journalRows)
			}
		})
	}
}

// TestRepeatExecPendingAllowedOnRepeatBody is the positive control: the SAME exitMap.pending
// that is refused off a repeat body lowers cleanly ON a repeat leaf body (no refusal).
func TestRepeatExecPendingAllowedOnRepeatBody(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	body := execNodeExitPending("draft", "exit 0", []int{0}, []int{75})
	doc := decodeIR(t, blockDoc("pf-ok", repeatNode(body, condLaneDraftOutcomePassOrIter())))
	if _, err := engine.Run(ctx, store, doc, nil); err != nil {
		t.Fatalf("a repeat leaf body with exitMap.pending was refused: %v", err)
	}
}

// TestRepeatExecInlinePendingNoScopePollution is the exec analog of the pool pollution pin
// (P1-CONFIRMED-BUG): the exec genesis record() must be gated on ranOutcome, so a pending
// poll never seeds scope. The body echoes its OWN recorded output ("R{{draft}}"), so an
// ungated record makes each poll ACCUMULATE ("R", "RR", …) into the next attempt's render.
// With the gate every attempt renders identically ("R{{draft}}" verbatim — the poll output
// was never recorded). Mutation: drop the `if ranOutcome(outcome)` gate → this reds.
func TestRepeatExecInlinePendingNoScopePollution(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// exit 75 (pending) on runs 1-2, exit 0 (pass) on run 3+; body references its own id.
	body := execNodeExitPending("draft", phasedExecScriptEcho(t, "R{{draft}}", []int{75, 75, 0}), []int{0}, []int{75})
	doc := decodeIR(t, blockDoc("pend-pollute", repeatNode(body, condLaneDraftOutcomePassOrIter())))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	outputs := settledOutputByActivation(t, res.Events)
	// No accumulation: the pass attempt renders the SAME body the first poll did — the
	// pending polls never seeded scope[draft]. An ungated record yields "RR…{{draft}}".
	if outputs["draft:2"] != outputs["draft:0"] {
		t.Fatalf("draft:2 output %q != draft:0 output %q — a pending poll polluted scope[draft] (ungated exec record)", outputs["draft:2"], outputs["draft:0"])
	}
	for _, act := range []string{"draft:0", "draft:1", "draft:2"} {
		if strings.Contains(outputs[act], "RR") {
			t.Fatalf("%s output = %q contains accumulated poll output (RR…) — the exec record must skip pending", act, outputs[act])
		}
	}
	// The pending journal drop+refolds byte-identically and folds deterministically.
	assertProjectionEqualsRefold(t, store, res.StreamID)
	assertStreamSplitFoldDeterministic(t, store, res.StreamID)
}

// TestRepeatExecInlinePendingGenesisResumeParity is the genesis==resume proof (P1): an
// uninterrupted run and a crash-after-the-pending-attempt resume of the SAME exec-inline
// pending loop settle to byte-identical attempt outputs. The body self-references, so an
// ungated genesis record makes the uninterrupted run render the pass attempt with the poll
// output while resume (reconstructOutputs correctly skips pending) renders without it —
// divergent journals. With the gate both paths agree. Mutation: drop the gate → reds.
func TestRepeatExecInlinePendingGenesisResumeParity(t *testing.T) {
	ctx := context.Background()
	mkDoc := func() *ir.IR {
		body := execNodeExitPending("draft", phasedExecScriptEcho(t, "R{{draft}}", []int{75, 0}), []int{0}, []int{75})
		return decodeIR(t, blockDoc("pend-parity", repeatNode(body, condLaneDraftOutcomePassOrIter())))
	}

	// Uninterrupted reference (its own counter-file doc).
	refStore := newStore(t)
	ref, err := engine.Run(ctx, refStore, mkDoc(), nil)
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}
	if ref.Outcome != engine.OutcomePass {
		t.Fatalf("reference outcome = %q, want pass", ref.Outcome)
	}
	refOut := settledOutputByActivation(t, ref.Events)

	// Crash right after the pending attempt draft:0 settles, then resume (a distinct doc /
	// counter file). draft:0 is settled so it does not re-run; draft:1 runs once on resume.
	resumed, store, stream := injectCrashThenResume(t, mkDoc(), &enginehost.StubHost{}, engine.CrashAfterSettle, "draft:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	resOut := settledOutputByActivation(t, resumed.Events)

	for _, act := range []string{"draft:0", "draft:1", "repeat_1:0"} {
		if refOut[act] != resOut[act] {
			t.Fatalf("genesis vs resume diverged at %s: genesis %q, resume %q (ungated exec pending record)", act, refOut[act], resOut[act])
		}
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// --- POOL do pending path ---------------------------------------------------

// TestRepeatPoolPendingThenPassNonConsuming proves the pool entry path (the primary
// gascity case): a repeat over a pool `do` whose worker closes gc.outcome=pending twice
// (CI still running) then pass exits pass in exactly THREE physical attempts, and the
// consuming iteration never exceeds 1 before the pass. The live projection drop+refolds
// byte-identically (v4 unchanged). Determinism pin included.
func TestRepeatPoolPendingThenPassNonConsuming(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-pool-pending-pass"
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("pool-pend", repeatNode(doNode("lane", "poll CI", nil), condLaneOutcomePassOrIter(2))))

	// Attempt 0: dispatch, park; settle pending (poll).
	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "lane:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked lane:0", r1, err)
	}
	fake.settleAct(t, "lane:0", engine.OutcomePending, "poll0")

	// Attempt 1: observe pending → mint lane:1 (the poll did NOT consume), park; settle pending.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "lane:1" {
		t.Fatalf("advance 2 = %+v err %v, want Parked lane:1 (pending re-poll, non-consuming)", r2, err)
	}
	fake.settleAct(t, "lane:1", engine.OutcomePending, "poll1")

	// Attempt 2: observe pending → mint lane:2, park; settle pass.
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r3.Parked || len(r3.InFlight) != 1 || r3.InFlight[0].Activation != "lane:2" {
		t.Fatalf("advance 3 = %+v err %v, want Parked lane:2", r3, err)
	}
	fake.settleAct(t, "lane:2", engine.OutcomePass, "done")

	// Attempt 2 passes → the loop exits pass → seal.
	r4, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r4.Sealed {
		t.Fatalf("advance 4 = %+v err %v, want Sealed", r4, err)
	}
	if r4.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r4.Run.Outcome)
	}
	// Exactly three physical attempts, two of them pending polls, the third the pass.
	settled := settledActivationsMap(t, streamStored(t, store, streamID))
	for act, want := range map[string]string{"lane:0": engine.OutcomePending, "lane:1": engine.OutcomePending, "lane:2": engine.OutcomePass} {
		if settled[act] != want {
			t.Errorf("settle %s = %q, want %q", act, settled[act], want)
		}
	}
	if _, ok := settled["lane:3"]; ok {
		t.Errorf("a fourth attempt lane:3 settled — the loop should have exited on the pass at lane:2")
	}
	if outcome, _, _, out := loopSettle(t, streamStored(t, store, streamID), "repeat_1:0"); outcome != engine.OutcomePass || out != "done" {
		t.Errorf("loop settle = {%q, %q}, want {pass, done} (the passing attempt's output, not a poll's)", outcome, out)
	}
	// The passing attempt's output is what nodeOutputs carries — poll outputs never entered it.
	if got := r4.Run.NodeOutputs["lane"]; got != "done" {
		t.Errorf("NodeOutputs[lane] = %q, want done (poll outputs excluded — ranOutcome guard)", got)
	}
	assertProjectionEqualsRefold(t, store, streamID)

	// Determinism: a fresh fold of the pending stream matches the live StateHash, and a
	// mid-split snapshot + tail reproduces genesis (pending folds deterministically, v4).
	assertStreamSplitFoldDeterministic(t, store, streamID)
}

// TestRepeatPoolPendingNoNodeOutputsPollution is the ranOutcome-exclusion pin: a pending
// poll's output must NOT enter the interpolation scope, so the NEXT attempt's rendered
// prompt (which references the body's own recorded output {{lane}}) must not carry the
// poll output. Mutation (iii) — making ranOutcome true for pending — records the poll
// output and turns this RED.
func TestRepeatPoolPendingNoNodeOutputsPollution(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-pool-pending-pollute"
	fake := newFakeWorkStore()
	// The body prompt references the body's own prior recorded output.
	doc := decodeIR(t, blockDoc("pool-pollute", repeatNode(doNode("lane", "prev={{lane}}", nil), condLaneOutcomePass())))

	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	// The poll's distinctive output must never leak into the next render.
	fake.settleAct(t, "lane:0", engine.OutcomePending, "POLLOUT")

	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "lane:1" {
		t.Fatalf("advance 2 = %+v err %v, want Parked lane:1", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "lane:1"); strings.Contains(got, "POLLOUT") {
		t.Fatalf("attempt-1 prompt = %q, must NOT contain the poll output POLLOUT (a pending poll polluted nodeOutputs — ranOutcome guard broken)", got)
	}
	// Finish the run cleanly (pass) so it seals.
	fake.settleAct(t, "lane:1", engine.OutcomePass, "done")
	if r3, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()); err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass", r3, err)
	}
}

// TestRepeatPoolPendingDoesNotExhaustBudget proves a pending poll never burns the repair
// budget: `pending×5, fail, fail` under `until lane.outcome == "pass" || iteration >= 2`
// exhausts the budget on the SECOND fail (the 7th physical attempt), NOT during the polls.
// With the buggy attempt+1 iteration, the budget would trip after the second PENDING.
func TestRepeatPoolPendingDoesNotExhaustBudget(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-pool-pending-budget"
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("pool-budget", repeatNode(doNode("lane", "check", nil), condLaneOutcomePassOrIter(2))))

	// Five pending polls then two fails. Drive attempt N, settle it, repeat.
	phase := append(append([]string{}, repeatStr("pending", 5)...), "failed", "failed")
	var last engine.AdvanceResult
	for i := 0; i < len(phase); i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		if r.Sealed {
			t.Fatalf("advance %d sealed early after %d settles — a pending poll exhausted the budget (mutation i?)", i, i)
		}
		act := fmt.Sprintf("lane:%d", i)
		if len(r.InFlight) != 1 || r.InFlight[0].Activation != act {
			t.Fatalf("advance %d InFlight = %+v, want %s", i, r.InFlight, act)
		}
		fake.settleAct(t, act, phase[i], "out"+phase[i])
		last = r
	}
	_ = last
	// The final Advance observes the 2nd fail (lane:6) and exhausts the budget → seal failed.
	rF, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !rF.Sealed {
		t.Fatalf("final advance = %+v err %v, want Sealed", rF, err)
	}
	if rF.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("run outcome = %q, want failed (budget exhausted on the 2nd fail)", rF.Run.Outcome)
	}
	settled := settledActivationsMap(t, streamStored(t, store, streamID))
	// Exactly 7 physical attempts (lane:0..lane:6): 5 pending, 2 failed — the 8th never minted.
	if _, ok := settled["lane:7"]; ok {
		t.Error("an 8th attempt lane:7 settled — the loop should have exhausted on lane:6 (the 2nd fail)")
	}
	for i := 0; i < 5; i++ {
		if settled[fmt.Sprintf("lane:%d", i)] != engine.OutcomePending {
			t.Errorf("lane:%d = %q, want pending (a poll)", i, settled[fmt.Sprintf("lane:%d", i)])
		}
	}
	if settled["lane:5"] != engine.OutcomeFailed || settled["lane:6"] != engine.OutcomeFailed {
		t.Errorf("fail attempts = {5:%q 6:%q}, want {failed failed}", settled["lane:5"], settled["lane:6"])
	}
	if outcome, _, _, _ := loopSettle(t, streamStored(t, store, streamID), "repeat_1:0"); outcome != engine.OutcomeFailed {
		t.Errorf("loop outcome = %q, want failed (iteration reached the budget on the 2nd consuming fail)", outcome)
	}
}

// TestRepeatPoolInfinitePendingPhysicalCap proves the physical spin bound: a forever-
// pending check (no iteration escape) settles failed{poll_cap} at lumenLoopPhysicalCap
// physical attempts, and the consuming cap (loop_cap) NEVER fires — the consuming count
// stays flat at 0. The cap is shrunk to 4 so the test does not mint 512 attempts.
// Mutation (ii) — dropping the physical cap — spins forever, so the bounded drive loop
// exits without sealing and the test REDS.
func TestRepeatPoolInfinitePendingPhysicalCap(t *testing.T) {
	ctx := context.Background()
	restore := engine.SetLoopPhysicalCapForTest(4)
	defer restore()

	store := newStore(t)
	streamID := "gcg-pool-pending-spincap"
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("pool-spin", repeatNode(doNode("lane", "poll", nil), condLaneOutcomePass())))

	var sealed engine.AdvanceResult
	for i := 0; i < 12; i++ { // bounded: cap is 4, so seal well within 12 passes
		r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		if r.Sealed {
			sealed = r
			break
		}
		if len(r.InFlight) != 1 {
			t.Fatalf("advance %d parked with in-flight %+v, want exactly one", i, r.InFlight)
		}
		fake.settleAct(t, r.InFlight[0].Activation, engine.OutcomePending, "polling")
	}
	if !sealed.Sealed {
		t.Fatal("the forever-pending loop never sealed within the bound — the physical cap did not fire (mutation ii: cap dropped)")
	}
	if sealed.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("run outcome = %q, want failed (poll cap)", sealed.Run.Outcome)
	}
	outcome, reason, _, _ := loopSettle(t, streamStored(t, store, streamID), "repeat_1:0")
	if outcome != engine.OutcomeFailed || reason != "poll_cap" {
		t.Fatalf("loop settle = {%q, %q}, want {failed, poll_cap} (NOT loop_cap — the consuming cap never fires on pending)", outcome, reason)
	}
	settled := settledActivationsMap(t, streamStored(t, store, streamID))
	// Exactly 4 physical attempts (lane:0..lane:3), all pending; no 5th.
	if _, ok := settled["lane:4"]; ok {
		t.Error("a 5th attempt lane:4 minted past the physical cap of 4")
	}
	for i := 0; i < 4; i++ {
		if settled[fmt.Sprintf("lane:%d", i)] != engine.OutcomePending {
			t.Errorf("lane:%d = %q, want pending", i, settled[fmt.Sprintf("lane:%d", i)])
		}
	}
}

// TestRepeatPoolPendingIterationRendersConsumingCount pins the ADVANCE/RESUME-side
// {{iteration}} bind (materializeLoopAttempt): a pool do repeat whose body prompt renders
// {{iteration}}, driven pending×2 then pass, must render the CONSUMING count (1) on every
// attempt — NOT the physical attempt index (which reaches 3). The design promised "render
// == consuming count on BOTH genesis (runAttempt) AND resume (materializeLoopAttempt)";
// TestRepeatExecPendingThenPassInline pins genesis, this pins the advance-side bind.
// Mutation: revert materializeLoopAttempt's bind to attempt+1 → lane:2 renders "iter 3".
func TestRepeatPoolPendingIterationRendersConsumingCount(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-pool-pending-iterrender"
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("pool-iter", repeatNode(doNode("lane", "iter {{iteration}}", nil), condLaneOutcomePassOrIter(9))))

	// Pending × 2 then pass; each attempt dispatches with an iteration-bearing prompt.
	phase := []string{engine.OutcomePending, engine.OutcomePending, engine.OutcomePass}
	for i, out := range phase {
		r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		act := fmt.Sprintf("lane:%d", i)
		if len(r.InFlight) != 1 || r.InFlight[0].Activation != act {
			t.Fatalf("advance %d InFlight = %+v, want %s", i, r.InFlight, act)
		}
		// Every attempt — poll or pass — renders the CONSUMING iteration 1 (polls do not
		// advance it), never the physical index i+1.
		if got := fake.dispatchPromptFor(t, act); got != "iter 1" {
			t.Fatalf("%s dispatched prompt = %q, want %q (consuming count, not physical index %d)", act, got, "iter 1", i+1)
		}
		fake.settleAct(t, act, out, "out")
	}
	rF, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil || !rF.Sealed || rF.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", rF, err)
	}
}

// TestRepeatNonPendingByteIdentityGolden is the load-bearing regression: a fail,fail,pass
// repeat over an exec (the consuming == physical path) must be BYTE-IDENTICAL to the
// pre-change engine. The proof is that the rendered {{iteration}} equals attempt+1 at
// every attempt (the sole input the decoupling changed), asserted directly; plus the
// exact settle sequence, drop+refold identity, and split-fold StateHash determinism.
func TestRepeatNonPendingByteIdentityGolden(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// exit 1 (fail) on runs 1-2, exit 0 (pass) on run 3+.
	body := execNodeExit("draft", phasedExecScript(t, []int{1, 1, 0}), []int{0}, nil)
	doc := decodeIR(t, blockDoc("byteid", repeatNode(body, condLaneDraftOutcomePassOrIter())))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	got := settledActivations(t, res.Events)
	want := [][2]string{
		{"draft:0", engine.OutcomeFailed},
		{"draft:1", engine.OutcomeFailed},
		{"draft:2", engine.OutcomePass},
		{"repeat_1:0", engine.OutcomePass},
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("settled = %v, want %v (non-pending fail,fail,pass sequence unchanged)", got, want)
	}
	// The load-bearing equivalence: {{iteration}} == attempt+1 at every attempt. A
	// non-pending loop has consumingCountBefore(bodyID, k) == k (every prior settled
	// attempt consumes), so the iteration render — the SOLE input the decoupling changed —
	// is byte-identical to the pre-change engine, hence so is the whole journal above and
	// the StateHash it folds to. (A cross-run StateHash constant is not usable: run.started
	// stamps a wall-clock created_at, so the hash varies per run. The determinism of the
	// SAME journal — the thing a resume/rebuild must reproduce — is pinned below.)
	outputs := settledOutputByActivation(t, res.Events)
	for i, act := range []string{"draft:0", "draft:1", "draft:2"} {
		if want := fmt.Sprintf("iter-%d", i+1); outputs[act] != want {
			t.Errorf("%s output = %q, want %q ({{iteration}} must equal attempt+1 for a non-pending loop)", act, outputs[act], want)
		}
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
	assertReducerLevelIdentity(t, store, res.StreamID)
	// StateHash determinism over THIS journal: every prefix-snapshot + tail refolds to the
	// genesis StateHash (a resume/rebuild reproduces the folded state byte-for-byte).
	assertStreamSplitFoldDeterministic(t, store, res.StreamID)
}

// --- helpers ----------------------------------------------------------------

// condLaneDraftOutcomePassOrIter builds `draft.outcome == "pass" || iteration >= 5` over
// the fixed leaf body id "draft" (cap 5 comfortably exceeds every fixture's attempt count).
func condLaneDraftOutcomePassOrIter() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"draft","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":5}]}]}`
}

// settledOutputByActivation returns activation -> settled output for every outcome.settled.
func settledOutputByActivation(t *testing.T, events []graphstore.StoredEvent) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Output     string `json:"output"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		out[p.Activation] = p.Output
	}
	return out
}

// settledActivationsMap returns activation -> outcome for every outcome.settled.
func settledActivationsMap(t *testing.T, events []graphstore.StoredEvent) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		out[p.Activation] = p.Outcome
	}
	return out
}

// assertStreamSplitFoldDeterministic proves a stream folds deterministically: a
// mid-split snapshot plus tail reproduces the genesis StateHash for every split point.
func assertStreamSplitFoldDeterministic(t *testing.T, store *graphstore.Store, streamID string) {
	t.Helper()
	all := readFoldEvents(t, store, streamID)
	r := engine.Reducer()
	genesis, _, err := fold.Fold(r, nil, all)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}
	genesisHash := genesis.StateHash()
	for k := 0; k <= len(all); k++ {
		prefix, _, err := fold.Fold(r, nil, all[:k])
		if err != nil {
			t.Fatalf("k=%d prefix fold: %v", k, err)
		}
		var snap *fold.Snapshot
		if k > 0 {
			blob, err := prefix.MarshalSnapshot()
			if err != nil {
				t.Fatalf("k=%d marshal: %v", k, err)
			}
			snap = &fold.Snapshot{
				StreamID: streamID, CoveredSeq: all[k-1].Seq, Engine: "lumen",
				ReducerVersion: r.ReducerVersion(), SnapshotFormatVersion: 4,
				StateHash: prefix.StateHash(), State: blob,
			}
		}
		tail, _, err := fold.Fold(r, snap, all[k:])
		if err != nil {
			t.Fatalf("k=%d tail fold: %v", k, err)
		}
		if tail.StateHash() != genesisHash {
			t.Fatalf("k=%d: pending stream split fold diverges from genesis StateHash", k)
		}
	}
}
