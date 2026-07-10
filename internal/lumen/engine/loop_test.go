package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- loop-IR construction helpers (mirror compile-lumen.mjs 0.2.5 shapes) ----

// repeatNode builds a repeat loop node: the label names the BODY (bodyJSON keeps
// its own id), the loop node takes the synthetic id loopID and the `after` gates,
// and cond is the `until` closed expression. iterationName defaults to
// "iteration" (the compiler's binding).
func repeatNode(bodyJSON, cond string) string {
	return `{
      "kind": "repeat", "id": "repeat_1", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "body": ` + bodyJSON + `,
      "cond": ` + cond + `,
      "iterationName": "iteration"
    }`
}

// retryNode builds a retry loop node: id/name = loopID, `attempts` is the count
// expression, `body` is the single leaf (bodyJSON keeps its own id).
func retryNode(attempts, bodyJSON string) string {
	return `{
      "kind": "retry", "id": "attempt", "name": "attempt", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "attempts": ` + attempts + `,
      "body": ` + bodyJSON + `
    }`
}

// execNodeExit renders an exec node whose exitMap declares the given pass and
// retryable exit-code sets (a retryable set drives the retry arm's classification).
func execNodeExit(id, script string, pass, retryable []int) string {
	scriptJSON, _ := json.Marshal(script)
	passJSON, _ := json.Marshal(pass)
	retryJSON, _ := json.Marshal(retryable)
	return `{
      "kind": "exec", "id": "` + id + `", "name": "` + id + `", "after": [],
      "origin": {"uri": "t", "line": 1, "col": 0},
      "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(scriptJSON) + `, "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "exitMap": {"pass": ` + string(passJSON) + `, "retryable": ` + string(retryJSON) + `}
    }`
}

// condOutcomePassOrIter builds the canonical dogfood cond
// `draft.outcome == pass || iteration >= 5` (a cap high enough that the outcome
// branch is what exits the loop in these tests).
func condOutcomePassOrIter() string {
	return `{"kind":"operator","op":"||","operands":[
      {"kind":"operator","op":"==","operands":[
        {"kind":"ref","name":"draft","field":"outcome"},
        {"kind":"literal","value":"pass"}]},
      {"kind":"operator","op":">=","operands":[
        {"kind":"ref","name":"iteration"},
        {"kind":"literal","value":5}]}]}`
}

// TestUnsupportedExprRefusedAtLowering (T-E2) proves a repeat cond outside the
// closed subset (op `in`, kind `call`, ref field `error`) is refused at LOWERING —
// buildUnits returns ErrUnsupportedNode before any effect, so ZERO journal rows
// are written (the pre-effect refusal discipline, M2/B1).
func TestUnsupportedExprRefusedAtLowering(t *testing.T) {
	ctx := context.Background()
	body := execNodeExit("draft", "echo hi", []int{0}, nil)
	for _, tc := range []struct {
		name string
		cond string
	}{
		{"op-in", `{"kind":"operator","op":"in","operands":[{"kind":"literal","value":1},{"kind":"array","elements":[]}]}`},
		{"kind-call", `{"kind":"call","name":"len","args":[]}`},
		{"field-error", `{"kind":"ref","name":"draft","field":"error"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			doc := decodeIR(t, blockDoc("badcond", repeatNode(body, tc.cond)))
			_, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: &enginehost.StubHost{}})
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("err = %v, want ErrUnsupportedNode", err)
			}
			var journalRows int
			if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows); err != nil {
				t.Fatalf("count journal: %v", err)
			}
			if journalRows != 0 {
				t.Errorf("a refused cond wrote %d journal rows, want 0", journalRows)
			}
		})
	}
}

// TestRetryBadBodyRefusedAtLowering proves a non-exec/do loop body (here a block)
// and a bodyless/attemptsless loop are refused at lowering with zero journal.
func TestRetryBadBodyRefusedAtLowering(t *testing.T) {
	ctx := context.Background()
	blockBody := `{"kind":"block","id":"b","after":[],"origin":{"uri":"t","line":1,"col":0},"members":[]}`
	for _, tc := range []struct {
		name string
		node string
	}{
		{"block-body", retryNode(`{"kind":"literal","value":3}`, blockBody)},
		{"missing-attempts", `{"kind":"retry","id":"attempt","after":[],"origin":{"uri":"t","line":1,"col":0},"body":` + execNodeExit("f", "echo x", []int{0}, nil) + `}`},
		{"missing-cond", `{"kind":"repeat","id":"repeat_1","after":[],"origin":{"uri":"t","line":1,"col":0},"body":` + execNodeExit("d", "echo x", []int{0}, nil) + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			doc := decodeIR(t, blockDoc("badloop", tc.node))
			_, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: &enginehost.StubHost{}})
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("err = %v, want ErrUnsupportedNode", err)
			}
			var journalRows int
			_ = store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows)
			if journalRows != 0 {
				t.Errorf("a refused loop wrote %d journal rows, want 0", journalRows)
			}
		})
	}
}

// --- loop-execution assertion helpers --------------------------------------

// countAttemptMinted returns how many attempt.minted events the stream carries.
func countAttemptMinted(events []graphstore.StoredEvent) int {
	n := 0
	for _, e := range events {
		if e.Type == engine.EventAttemptMinted {
			n++
		}
	}
	return n
}

// settledActivations returns (full activation key, outcome) for every
// outcome.settled event, in seq order — the attempt-resolved view (draft:0,
// draft:1, …) the bare-id settledIDs helper collapses.
func settledActivations(t *testing.T, events []graphstore.StoredEvent) [][2]string {
	t.Helper()
	var out [][2]string
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
		out = append(out, [2]string{p.Activation, p.Outcome})
	}
	return out
}

// loopSettle returns the (outcome, reason, retriesRemaining) of a loop node's
// own outcome.settled (activation loopID:0).
func loopSettle(t *testing.T, events []graphstore.StoredEvent, loopAct string) (outcome, reason string, retriesRemaining *int, output string) {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation       string `json:"activation"`
			Outcome          string `json:"outcome"`
			Output           string `json:"output"`
			Reason           string `json:"reason"`
			RetriesRemaining *int   `json:"retries_remaining"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == loopAct {
			return p.Outcome, p.Reason, p.RetriesRemaining, p.Output
		}
	}
	t.Fatalf("no outcome.settled for loop activation %q", loopAct)
	return "", "", nil, ""
}

// flakyExec returns an exec script that FAILS on its first run and PASSES on
// every run after, using a flag file to persist state across attempts. It prints
// a distinguishable line per phase so the loop's satisfying-attempt output is
// checkable.
func flakyExec(t *testing.T) string {
	t.Helper()
	flag := filepath.Join(t.TempDir(), "flakyflag")
	return `if [ -f "` + flag + `" ]; then echo attempt-pass; exit 0; else : > "` + flag + `"; echo attempt-fail; exit 1; fi`
}

// TestRepeatEngineInlineFailThenPass (T-L1) is the smallest loop proof: a repeat
// over an exec body that fails once then passes runs attempts draft:0 (failed) →
// draft:1 (pass); the loop settles pass carrying draft:1's output; attempt.minted
// fires twice; and a dependent gated on the loop runs exactly once.
func TestRepeatEngineInlineFailThenPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	body := execNodeExit("draft", flakyExec(t), []int{0}, nil)
	// until draft.outcome == pass || iteration >= 5 (the outcome branch exits at :1).
	loop := repeatNode(body, condOutcomePassOrIter())
	publish := execNode("publish", "echo published", []string{"repeat_1"})
	doc := decodeIR(t, blockDoc("l1", loop, publish))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if n := countAttemptMinted(res.Events); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2 (draft:0 then draft:1)", n)
	}
	got := settledActivations(t, res.Events)
	want := [][2]string{
		{"draft:0", "failed"},
		{"draft:1", "pass"},
		{"repeat_1:0", "pass"},
		{"publish:0", "pass"},
	}
	if len(got) != len(want) {
		t.Fatalf("settled activations = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("settled[%d] = %v, want %v", i, got[i], want[i])
		}
	}
	if outcome, _, _, output := loopSettle(t, res.Events, "repeat_1:0"); outcome != "pass" || output != "attempt-pass" {
		t.Fatalf("loop settle = {%q, %q}, want {pass, attempt-pass} (the satisfying attempt's output)", outcome, output)
	}
	// The dependent ran exactly once (gated on the loop node, not a body attempt).
	if n := 0; func() bool {
		for _, s := range got {
			if s[0] == "publish:0" {
				n++
			}
		}
		return n != 1
	}() {
		t.Fatalf("publish settled %d times, want 1", 0)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestRepeatLoopCapSettlesFailed (T-L2) proves the mandatory infinite-loop guard:
// a repeat whose cond never turns truthy runs EXACTLY 32 attempts, then the loop
// settles failed{reason:"loop_cap"}, and a dependent skip-cascades.
func TestRepeatLoopCapSettlesFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	body := execNodeExit("draft", "echo tick", []int{0}, nil)
	// until false — never truthy (a literal-false operand pair that is always false).
	never := `{"kind":"operator","op":"==","operands":[{"kind":"literal","value":0},{"kind":"literal","value":1}]}`
	loop := repeatNode(body, never)
	publish := execNode("publish", "echo published", []string{"repeat_1"})
	doc := decodeIR(t, blockDoc("l2", loop, publish))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countAttemptMinted(res.Events); n != 32 {
		t.Fatalf("attempt.minted count = %d, want 32 (the loop cap)", n)
	}
	// Count body settles (draft:*) — exactly 32.
	bodySettles := 0
	for _, s := range settledActivations(t, res.Events) {
		if engine.ActivationNodeID(s[0]) == "draft" {
			bodySettles++
		}
	}
	if bodySettles != 32 {
		t.Fatalf("draft settled %d times, want exactly 32", bodySettles)
	}
	outcome, reason, _, _ := loopSettle(t, res.Events, "repeat_1:0")
	if outcome != "failed" || reason != "loop_cap" {
		t.Fatalf("loop settle = {%q, reason %q}, want {failed, loop_cap}", outcome, reason)
	}
	// The dependent skip-cascades off the failed loop.
	if st := nodeStatus(t, store, "publish"); st != "skipped" {
		t.Fatalf("publish status = %q, want skipped (skip-cascade off the failed loop)", st)
	}
}

// TestRetryNonRetryableReturnsImmediately (T-L3) proves the retry FAILURE arm: a
// plain non-retryable exec failure (exit 1, empty retryable set) returns after ONE
// attempt with retries_remaining == attempts − 1.
func TestRetryNonRetryableReturnsImmediately(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// exec exits 1; exitMap.retryable is empty ⇒ NOT retryable ⇒ return immediately.
	body := execNodeExit("flaky", "exit 1", []int{0}, []int{})
	loop := retryNode(`{"kind":"literal","value":3}`, body)
	doc := decodeIR(t, blockDoc("l3", loop))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countAttemptMinted(res.Events); n != 1 {
		t.Fatalf("attempt.minted count = %d, want 1 (non-retryable ⇒ one attempt)", n)
	}
	outcome, _, rem, _ := loopSettle(t, res.Events, "attempt:0")
	if outcome != "failed" {
		t.Fatalf("loop outcome = %q, want failed", outcome)
	}
	if rem == nil || *rem != 2 {
		t.Fatalf("retries_remaining = %v, want 2 (attempts 3 − attempt 1)", rem)
	}
}

// TestRetryExhaustionRetriesRemainingZero (T-L4) proves the retry budget: an
// always-retryable exec failure (exit 1 ∈ exitMap.retryable) re-attempts up to the
// budget (3), then settles failed with retries_remaining == 0 and reason
// "exhausted".
func TestRetryExhaustionRetriesRemainingZero(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// exit 1 IS in the retryable set ⇒ each failure retries until the budget ends.
	body := execNodeExit("flaky", "exit 1", []int{0}, []int{1})
	loop := retryNode(`{"kind":"literal","value":3}`, body)
	doc := decodeIR(t, blockDoc("l4", loop))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if n := countAttemptMinted(res.Events); n != 3 {
		t.Fatalf("attempt.minted count = %d, want 3 (the full budget)", n)
	}
	outcome, reason, rem, _ := loopSettle(t, res.Events, "attempt:0")
	if outcome != "failed" || reason != "exhausted" {
		t.Fatalf("loop settle = {%q, reason %q}, want {failed, exhausted}", outcome, reason)
	}
	if rem == nil || *rem != 0 {
		t.Fatalf("retries_remaining = %v, want 0 (exhausted)", rem)
	}
}

// TestRetryInvalidAttemptsSettlesFailed (T-L5) proves an invalid attempts value
// (non-integer or < 1) settles the loop failed{reason:"invalid_input"} with ZERO
// attempts run.
func TestRetryInvalidAttemptsSettlesFailed(t *testing.T) {
	ctx := context.Background()
	for _, attempts := range []string{`{"kind":"literal","value":"x"}`, `{"kind":"literal","value":0}`} {
		store := newStore(t)
		body := execNodeExit("flaky", "echo hi", []int{0}, nil)
		loop := retryNode(attempts, body)
		doc := decodeIR(t, blockDoc("l5", loop))

		res, err := engine.Run(ctx, store, doc, nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if n := countAttemptMinted(res.Events); n != 0 {
			t.Fatalf("attempt.minted count = %d, want 0 (invalid attempts ⇒ no attempt)", n)
		}
		outcome, reason, _, _ := loopSettle(t, res.Events, "attempt:0")
		if outcome != "failed" || reason != "invalid_input" {
			t.Fatalf("loop settle = {%q, reason %q}, want {failed, invalid_input}", outcome, reason)
		}
	}
}

// TestRepeatIterationBindingInPromptAndCond (T-L6) proves the per-attempt
// iteration binding: a do body whose prompt interpolates {{iteration}} renders "1"
// then "2" (distinct per-attempt effect specs), and the cond `until iteration >= 2`
// exits after attempt 2.
func TestRepeatIterationBindingInPromptAndCond(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	body := doNode("draft", "iteration {{iteration}}", nil)
	// until iteration >= 2 (outcome-agnostic; exits after the 2nd attempt).
	cond := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":2}]}`
	loop := repeatNode(body, cond)
	doc := decodeIR(t, blockDoc("l6", loop))

	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"draft": {Outcome: enginehost.OutcomePass, Output: "ok"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	prompts := effectPrompts(t, res.Events)
	if len(prompts) != 2 || prompts[0] != "iteration 1" || prompts[1] != "iteration 2" {
		t.Fatalf("effect prompts = %v, want [iteration 1, iteration 2] (per-attempt iteration binding)", prompts)
	}
	// Two distinct effect tokens (:do:1, :do:2), so each attempt is its own effect.
	tokens := map[string]bool{}
	for _, e := range res.Events {
		if e.Type == engine.EventEffectScheduled {
			var p struct {
				IdemToken string `json:"idem_token"`
			}
			_ = json.Unmarshal(e.Payload, &p)
			tokens[p.IdemToken] = true
		}
	}
	if !tokens[res.StreamID+":draft:do:1"] || !tokens[res.StreamID+":draft:do:2"] {
		t.Fatalf("effect tokens = %v, want :do:1 and :do:2", tokens)
	}
	if outcome, _, _, _ := loopSettle(t, res.Events, "repeat_1:0"); outcome != "pass" {
		t.Fatalf("loop outcome = %q, want pass", outcome)
	}
}

// assertReducerLevelIdentity proves the incremental fold deltas and the full-state
// ProjectDelta agree on the node projection (DET-T-17 at the reducer level) — the
// check that catches a bare-id collision across attempts (the >9 lexical trap the
// ProjectDelta max-attempt selection fixes).
func assertReducerLevelIdentity(t *testing.T, store *graphstore.Store, streamID string) {
	t.Helper()
	events := readFoldEvents(t, store, streamID)
	state, deltas, err := fold.Fold(engine.Reducer(), nil, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	projector, ok := state.(fold.SnapshotProjector)
	if !ok {
		t.Fatal("lumen state is not a SnapshotProjector")
	}
	full := projector.ProjectDelta(streamID)
	inc := collapseNodeUpserts(deltas)
	fullNodes := collapseNodeUpserts([]fold.Delta{full})
	if !reflect.DeepEqual(inc, fullNodes) {
		t.Fatalf("incremental node projection != full ProjectDelta (multi-attempt bare-id collision):\nincremental=%+v\nfull=%+v", inc, fullNodes)
	}
}

// TestDETT17_MultiAttemptRebuildByteIdentity (T-D1) is DET-T-17 over multi-attempt
// streams: (A) a pool repeat fail-then-pass (2 attempts) reprojects byte-identically
// on drop+refold, and (B) an engine repeat with MORE THAN TEN attempts and distinct
// per-attempt outputs keeps the incremental fold and the full-state ProjectDelta in
// agreement — the case where a lexical activationKeys walk ("draft:10" < "draft:9")
// would otherwise pick the wrong attempt's bare-id row.
func TestDETT17_MultiAttemptRebuildByteIdentity(t *testing.T) {
	ctx := context.Background()

	// Part A: pool repeat, 2 attempts, drop+refold identity.
	{
		store := newStore(t)
		streamID := "gcg-run-det-pool"
		loop := repeatNode(doNode("draft", "Do it.", nil), condOutcomePassOrIter())
		doc := decodeIR(t, blockDoc("det-pool", loop))
		fake := newFakeWorkStore()
		opts := fake.opts()
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
			t.Fatalf("advance 1: %v", err)
		}
		fake.settleAct(t, "draft:0", engine.OutcomeFailed, "no")
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
			t.Fatalf("advance 2: %v", err)
		}
		fake.settleAct(t, "draft:1", engine.OutcomePass, "ok")
		if r, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil || !r.Sealed {
			t.Fatalf("advance 3 = %+v, err %v; want Sealed", r, err)
		}
		assertProjectionEqualsRefold(t, store, streamID)
		assertReducerLevelIdentity(t, store, streamID)
	}

	// Part B: engine repeat, 11 attempts, distinct per-attempt outputs (the >9 trap).
	{
		store := newStore(t)
		body := execNodeExit("draft", "echo iter-{{iteration}}", []int{0}, nil)
		cond := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":11}]}`
		loop := repeatNode(body, cond)
		doc := decodeIR(t, blockDoc("det-big", loop))
		res, err := engine.Run(ctx, store, doc, nil)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if n := countAttemptMinted(res.Events); n != 11 {
			t.Fatalf("attempt.minted count = %d, want 11", n)
		}
		// The projected bare-id row is the MAX attempt (draft:10 ⇒ iter-11), NOT the
		// lexical-max draft:9 (iter-10). This is the ProjectDelta max-attempt fix.
		if out := nodeMeta(t, store, "draft", "output"); out != "iter-11" {
			t.Fatalf("projected draft output = %q, want iter-11 (max attempt, not lexical-max draft:9)", out)
		}
		assertReducerLevelIdentity(t, store, res.StreamID)
	}
}

// TestCrashMidLoopConverges (T-D2) injects a crash at the before-activate and
// after-settle boundaries of the SECOND loop attempt (draft:1), then resumes: the
// run converges to pass, each attempt's do host is called at most once (at-most-once
// PER attempt, via distinct effect tokens :do:1 / :do:2), and the journal verifies.
func TestCrashMidLoopConverges(t *testing.T) {
	ctx := context.Background()
	// until iteration >= 2 — outcome-agnostic, exactly two attempts.
	cond := `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":2}]}`
	for _, boundary := range []string{engine.CrashBeforeActivate, engine.CrashAfterSettle} {
		t.Run(boundary, func(t *testing.T) {
			stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
				"draft": {Outcome: enginehost.OutcomePass, Output: "ok"},
			}}
			doc := decodeIR(t, blockDoc("crashloop",
				repeatNode(doNode("draft", "do {{iteration}}", nil), cond)))
			resumed, store, stream := injectCrashThenResume(t, doc, stub, boundary, "draft:1", 0)
			if resumed.Outcome != engine.OutcomePass {
				t.Fatalf("resumed outcome = %q, want pass", resumed.Outcome)
			}
			perAttempt := map[string]int{}
			for _, c := range stub.Calls() {
				perAttempt[c.Activation]++
			}
			for act, n := range perAttempt {
				if n > 1 {
					t.Fatalf("attempt %s host called %d times — at-most-once PER attempt violated", act, n)
				}
			}
			full, err := store.ReadStream(ctx, stream, 1, 0)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			toks := map[string]bool{}
			for _, e := range full {
				if e.Type == engine.EventEffectScheduled {
					var p struct {
						IdemToken string `json:"idem_token"`
					}
					_ = json.Unmarshal(e.Payload, &p)
					toks[p.IdemToken] = true
				}
			}
			if !toks[stream+":draft:do:1"] || !toks[stream+":draft:do:2"] {
				t.Fatalf("effect tokens = %v, want :do:1 and :do:2 (distinct per attempt)", toks)
			}
			if err := store.Verify(ctx, stream); err != nil {
				t.Fatalf("Verify: %v", err)
			}
		})
	}
}

// TestKeepJudgmentOutOfGoLoopPolicyIsData (T-J1) is the load-bearing keep-judgment-
// out-of-Go proof, in two halves.
//
// Behavioral: two formulas identical except the cond / retryable DATA produce
// DIFFERENT re-run behavior over the same body outcomes, with NO change to Go — a
// repeat's iteration cap and a retry's retryable exit-code set each drive the loop.
//
// Structural: a source scan asserting the loop re-run decision flows through the
// closed-expression evaluator (repeat) and the folded Retryable FIELD (retry), and
// that loopDecide carries no hardcoded `outcome == failed ⇒ re-run` Go branch — the
// only outcome comparison is the stop-on-success check.
func TestKeepJudgmentOutOfGoLoopPolicyIsData(t *testing.T) {
	ctx := context.Background()

	attemptsOf := func(t *testing.T, doc string, opts engine.Options) int {
		t.Helper()
		store := newStore(t)
		res, err := engine.RunWithOptions(ctx, store, decodeIR(t, doc), nil, opts)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		return countAttemptMinted(res.Events)
	}

	// (a) repeat: the iteration cap in the cond DATA sets the re-run count.
	iterCond := func(iterCap int) string {
		return `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":` + itoaTest(iterCap) + `}]}`
	}
	body := execNodeExit("draft", "echo hi", []int{0}, nil)
	if got := attemptsOf(t, blockDoc("j1a1", repeatNode(body, iterCond(1))), engine.Options{}); got != 1 {
		t.Fatalf("repeat until iteration>=1: %d attempts, want 1", got)
	}
	if got := attemptsOf(t, blockDoc("j1a3", repeatNode(body, iterCond(3))), engine.Options{}); got != 3 {
		t.Fatalf("repeat until iteration>=3: %d attempts, want 3 (cond DATA drives it, no Go change)", got)
	}

	// (b) retry: the retryable exit-code set in the body DATA decides whether a
	// failure re-runs — same exec, same failure, different DATA ⇒ different behavior.
	retryDoc := func(retryable []int) string {
		return blockDoc("j1r", retryNode(`{"kind":"literal","value":3}`, execNodeExit("flaky", "exit 1", []int{0}, retryable)))
	}
	if got := attemptsOf(t, retryDoc([]int{1}), engine.Options{}); got != 3 {
		t.Fatalf("retry with exit 1 retryable: %d attempts, want 3", got)
	}
	if got := attemptsOf(t, retryDoc([]int{}), engine.Options{}); got != 1 {
		t.Fatalf("retry with exit 1 NON-retryable: %d attempts, want 1 (retryable DATA drives it, no Go change)", got)
	}

	// Structural: scan the loop-driver source.
	engineSrc := readSource(t, "engine.go")
	advanceSrc := readSource(t, "advance.go")

	decide := funcBody(t, engineSrc, "func (d *driver) loopDecide")
	if !strings.Contains(decide, "evalCondTruthy(") {
		t.Fatal("loopDecide does not evaluate the repeat cond through evalCondTruthy — a Go outcome branch may have replaced the closed expression")
	}
	if !strings.Contains(decide, "bn.Retryable") {
		t.Fatal("loopDecide does not read the folded Retryable FIELD — the retry re-run must be data-driven")
	}
	if strings.Contains(decide, "== OutcomeFailed") {
		t.Fatal("loopDecide contains an `== OutcomeFailed` branch — a hardcoded re-run judgment leaked into Go (only the stop-on-success `!= OutcomeFailed` check is allowed)")
	}
	if strings.Contains(decide, `"failed"`) {
		t.Fatal("loopDecide contains a raw \"failed\" literal — use the OutcomeFailed constant; judgment must not be a string match")
	}
	// The mint sites must not branch on an outcome value themselves.
	for _, fn := range []struct {
		src, name string
	}{
		{engineSrc, "func (d *driver) runLoop"},
		{engineSrc, "func (d *driver) runAttempt"},
		{advanceSrc, "func (d *driver) advanceLoop"},
		{advanceSrc, "func (d *driver) materializeLoopAttempt"},
	} {
		body := funcBody(t, fn.src, fn.name)
		if strings.Contains(body, "== OutcomeFailed") {
			t.Fatalf("%s contains an `== OutcomeFailed` branch — a mint decision must not be gated on a hardcoded outcome value", fn.name)
		}
	}
}

// readSource reads a package source file for the structural scan.
func readSource(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// funcBody extracts a Go function's body by brace matching from its signature.
func funcBody(t *testing.T, src, signature string) string {
	t.Helper()
	i := strings.Index(src, signature)
	if i < 0 {
		t.Fatalf("function %q not found in source", signature)
	}
	open := strings.IndexByte(src[i:], '{')
	if open < 0 {
		t.Fatalf("no opening brace after %q", signature)
	}
	depth := 0
	start := i + open
	for j := start; j < len(src); j++ {
		switch src[j] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return src[start : j+1]
			}
		}
	}
	t.Fatalf("unbalanced braces for %q", signature)
	return ""
}

func itoaTest(n int) string {
	b, _ := json.Marshal(n)
	return string(b)
}
