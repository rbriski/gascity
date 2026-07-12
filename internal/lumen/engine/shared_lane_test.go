package engine_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- shared-lane (SLX) drive helpers ---------------------------------------

// slxHasNodeActivated reports whether events carry a node.activated for activation.
func slxHasNodeActivated(t *testing.T, events []graphstore.StoredEvent, activation string) bool {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if p.Activation == activation {
			return true
		}
	}
	return false
}

// settledRetryableFor returns the retryable flag of the outcome.settled for activation.
func settledRetryableFor(t *testing.T, events []graphstore.StoredEvent, activation string) bool {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Retryable  bool   `json:"retryable"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return p.Retryable
		}
	}
	t.Fatalf("no outcome.settled for activation %q", activation)
	return false
}

// slxDoIndex builds a do node whose prompt template is a text prefix followed by an index
// interpolation {{ <indexSrc> }} (the compiler's verbatim {kind:"literal"} part, §0.3) — the
// shape the *-shared corpus renders engine-defined.
func slxDoIndex(id, pre, indexSrc string) string {
	preJSON, _ := json.Marshal(pre)
	srcJSON, _ := json.Marshal(indexSrc)
	rawJSON, _ := json.Marshal(pre + "{{ " + indexSrc + " }}")
	return `{"kind":"do","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":` + string(rawJSON) + `,"template":{"parts":[` +
		`{"kind":"text","value":` + string(preJSON) + `},` +
		`{"kind":"interp","expr":{"kind":"literal","value":` + string(srcJSON) + `}}]},` +
		`"source":{"kind":"inline"},"templated":true,"language":"markdown","syntax":"bare","origin":{"uri":"t","line":1,"col":0}}}`
}

// slxMarqueeCond builds `lane.outcome == failed || iteration >= length(items)` — the §2.1
// exit over the do body ref, the iteration counter, and the closed-expr length call over the
// env-bound array sub input.
func slxMarqueeCond() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"lane","field":"outcome"},{"kind":"literal","value":"failed"}]},` +
		`{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},` +
		`{"kind":"call","name":"length","args":[{"kind":"ref","name":"items"}]}]}]}`
}

// slxLengthOnlyCond builds `iteration >= length(items)` (no failed escape) — the exec-body
// twin's byte-parity cond.
func slxLengthOnlyCond() string {
	return `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},` +
		`{"kind":"call","name":"length","args":[{"kind":"ref","name":"items"}]}]}`
}

// failDoStub returns a StubHost scripting each named do node to FAIL — for the failing-lane
// early-exit shape (a lane whose work fails).
func failDoStub(nodes ...string) *enginehost.StubHost {
	res := map[string]enginehost.DoResult{}
	for _, n := range nodes {
		res[n] = enginehost.DoResult{Outcome: enginehost.OutcomeFailed, Output: "no", Detail: "lane failed"}
	}
	return &enginehost.StubHost{Results: res}
}

// slxMarqueeIR builds the ns marquee IR JSON string.
func slxMarqueeIR(body, cond string) string {
	env := `[` + envField("items", "work_items") + `]`
	return bundleDoc(
		arrField("work_items"),
		runNodeRawEnv("stage", nil, "wrapper", env),
		subDoc("wrapper", arrField("items"), repeatNode(body, cond)))
}

// TestSharedLaneMarqueeInline pins §2.1 for the inline driver: the run sub-formula's repeat
// renders {{ items[iteration - 1] }} at depth over a 2-element env-bound array — attempt 0
// renders element 0 (alpha), attempt 1 renders element 1 (beta), then `iteration >=
// length(items)` (2 >= 2) exits. EXACTLY 2 attempts; the per-element render is the
// silent-misrender mutant killer.
func TestSharedLaneMarqueeInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"work_items": []any{"alpha", "beta"}}, engine.Options{Host: passDoStub("stage/lane")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	prompts := effectPrompts(t, res.Events)
	want := []string{"work on alpha", "work on beta"}
	if len(prompts) != len(want) || prompts[0] != want[0] || prompts[1] != want[1] {
		t.Fatalf("attempt prompts = %v, want %v (per-element indexed render at depth)", prompts, want)
	}
	acts := settledActivations(t, res.Events)
	if !settledActHas(acts, "stage/lane:0") || !settledActHas(acts, "stage/lane:1") {
		t.Fatalf("attempts = %v, want stage/lane:0 (alpha) and stage/lane:1 (beta)", acts)
	}
	if settledActHas(acts, "stage/lane:2") {
		t.Errorf("stage/lane:2 settled — the loop over-ran (length cond should exit at attempt 1)")
	}
}

// TestSharedLaneMarqueePool pins §2.1 for the pool driver: the SAME marquee DISPATCHES each
// attempt with the indexed render resolved in the prompt — attempt 0 "work on alpha",
// attempt 1 "work on beta" — then exits via the length cond.
func TestSharedLaneMarqueePool(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-slx-marquee"
	fake := newFakeWorkStore()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}

	r1, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r1.Parked || len(r1.InFlight) != 1 || r1.InFlight[0].Activation != "stage/lane:0" {
		t.Fatalf("advance 1 = %+v err %v, want Parked with stage/lane:0", r1, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/lane:0"); got != "work on alpha" {
		t.Fatalf("attempt-0 dispatched prompt = %q, want %q (indexed render on the POOL path)", got, "work on alpha")
	}
	fake.settle("wb-1", engine.OutcomePass, "ok0")

	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "stage/lane:1" {
		t.Fatalf("advance 2 = %+v err %v, want Parked with stage/lane:1 (re-attempt)", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/lane:1"); got != "work on beta" {
		t.Fatalf("attempt-1 dispatched prompt = %q, want %q", got, "work on beta")
	}
	fake.settle("wb-2", engine.OutcomePass, "ok1")

	r3, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass", r3, err)
	}
	if fake.dispatchCount() != 2 {
		t.Errorf("DispatchWork calls = %d, want 2 (one per attempt; the length cond exits at 2)", fake.dispatchCount())
	}
}

// TestSharedLaneFailingLaneEarlyExit pins §2.1 (the failed clause): a lane whose do FAILS on
// attempt 0 exits the loop early via `lane.outcome == failed` — attempt 1 never mints.
func TestSharedLaneFailingLaneEarlyExit(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"work_items": []any{"alpha", "beta"}}, engine.Options{Host: failDoStub("stage/lane")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	acts := settledActivations(t, res.Events)
	if !settledActFailed(acts, "stage/lane:0") {
		t.Fatalf("attempt 0 = %v, want stage/lane:0 failed", acts)
	}
	if settledActHas(acts, "stage/lane:1") {
		t.Errorf("stage/lane:1 minted — the failed clause should exit after attempt 0")
	}
}

// TestSharedLaneMarqueeRoot pins §2.2's root twin: the same repeat at the ROOT (loop over a
// root array input, iteration seeded bare) renders element 0 then element 1 and exits via
// length — proving the render + length mechanics are identical at root and ns.
func TestSharedLaneMarqueeRoot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatNode(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()),
		""))
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"items": []any{"alpha", "beta"}}, engine.Options{Host: passDoStub("lane")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	prompts := effectPrompts(t, res.Events)
	if len(prompts) != 2 || prompts[0] != "work on alpha" || prompts[1] != "work on beta" {
		t.Fatalf("root attempt prompts = %v, want [work on alpha, work on beta]", prompts)
	}
}

// TestSharedLaneExecBodyTwinJournalParity pins §2.2(a): a length-cond repeat with an EXEC
// body (no indexed render, so byte-parity is achievable) journals BYTE-IDENTICALLY inline
// vs pool — the length-in-cond byte-parity pin.
func TestSharedLaneExecBodyTwinJournalParity(t *testing.T) {
	ctx := context.Background()
	body := execNode("lane", `echo "round {{iteration}}"`, nil)
	doc := decodeIR(t, slxMarqueeIR(body, slxLengthOnlyCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-slx-execparity", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec loop runs inline on both)", r, err)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestSharedLaneDoBodyMarqueeRenderParity pins §2.2(b): the do-body marquee (whose journals
// can NOT be byte-identical) agrees per-attempt on the RENDERED prompt (inline effect
// prompts == pool dispatched prompts) and on the attempt count across both drivers.
func TestSharedLaneDoBodyMarqueeRenderParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}

	inStore := newStore(t)
	inRes, err := engine.RunWithOptions(ctx, inStore, doc, input, engine.Options{Host: passDoStub("stage/lane")})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	inlinePrompts := effectPrompts(t, inRes.Events)

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	streamID := "gcg-slx-renderparity"
	var poolPrompts []string
	for i := 0; i < 3; i++ {
		r, err := engine.Advance(ctx, poolStore, doc, streamID, input, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		for _, w := range r.InFlight {
			poolPrompts = append(poolPrompts, fake.dispatchPromptFor(t, w.Activation))
			fake.settleAct(t, w.Activation, engine.OutcomePass, "ok")
		}
		if r.Sealed {
			break
		}
	}
	if len(inlinePrompts) != len(poolPrompts) {
		t.Fatalf("attempt counts diverge: inline %v vs pool %v", inlinePrompts, poolPrompts)
	}
	for i := range inlinePrompts {
		if inlinePrompts[i] != poolPrompts[i] {
			t.Errorf("attempt %d prompt: inline %q vs pool %q", i, inlinePrompts[i], poolPrompts[i])
		}
	}
}

// TestSharedLaneMarqueeDropRefold pins §2.2(c): the do-body marquee's live projection
// drop+refolds byte-identically (no hidden reducer state — reducerVersion 4).
func TestSharedLaneMarqueeDropRefold(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-slx-refold"
	fake := newFakeWorkStore()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}
	for i := 0; i < 3; i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		fake.settleAllDispatchedPass()
		if r.Sealed {
			break
		}
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestSharedLaneIndexRenderSettlesFailed pins §1.2.4 / §2.7 (⚑B2): each index-render
// failure row SETTLES the do failed{detail} on BOTH drivers with an IDENTICAL outcome and
// detail, and NEVER dispatches a work bead. base-absent vs present-empty carry DISTINCT
// details (miss != present-empty).
func TestSharedLaneIndexRenderSettlesFailed(t *testing.T) {
	ctx := context.Background()
	rows := []struct {
		name, indexSrc, inputFields, wantDetail string
		input                                   map[string]any
	}{
		{"base-absent", "missing[0]", arrField("items"), `index base "missing" is not in scope`, map[string]any{"items": []any{"a"}}},
		{"base-not-array", "items[0]", strField("items"), `index base "items" is not a JSON array`, map[string]any{"items": "scalar"}},
		{"present-empty-out-of-range", "items[0]", arrField("items"), `index 0 out of range for "items" (length 0)`, map[string]any{"items": []any{}}},
		{"out-of-range", "items[9]", arrField("items"), `index 9 out of range for "items" (length 1)`, map[string]any{"items": []any{"a"}}},
		{"index-ident-absent", "items[nope]", arrField("items"), `index "nope" is not in scope`, map[string]any{"items": []any{"a"}}},
		{"index-non-integral", "items[frac]", arrField("items") + "," + strField("frac"), `index "frac" = "1.5" is not an integer`, map[string]any{"items": []any{"a", "b"}, "frac": "1.5"}},
		{"negative-index", "items[i - 2]", arrField("items") + "," + strField("i"), `index -1 out of range for "items" (length 2)`, map[string]any{"items": []any{"a", "b"}, "i": "1"}},
	}
	for _, r := range rows {
		t.Run(r.name, func(t *testing.T) {
			doc := decodeIR(t, bundleDoc(r.inputFields, slxDoIndex("d", "x=", r.indexSrc), ""))

			inStore := newStore(t)
			inRes, err := engine.RunWithOptions(ctx, inStore, doc, r.input, engine.Options{Host: passDoStub("d")})
			if err != nil {
				t.Fatalf("inline: %v", err)
			}
			if got := settledOutcomeByID(t, inRes.Events)["d"]; got != engine.OutcomeFailed {
				t.Fatalf("inline d outcome = %q, want failed", got)
			}
			inDetail := settledDetailFor(t, inRes.Events, "d:0")

			poolStore := newStore(t)
			fake := newFakeWorkStore()
			streamID := "gcg-slx-fail-" + r.name
			pr, err := engine.Advance(ctx, poolStore, doc, streamID, r.input, fake.opts())
			if err != nil || !pr.Sealed {
				t.Fatalf("pool advance = %+v err %v, want Sealed", pr, err)
			}
			poolEvents := streamStored(t, poolStore, streamID)
			if got := settledOutcomeByID(t, poolEvents)["d"]; got != engine.OutcomeFailed {
				t.Fatalf("pool d outcome = %q, want failed", got)
			}
			poolDetail := settledDetailFor(t, poolEvents, "d:0")

			if inDetail != poolDetail {
				t.Errorf("detail diverges by driver: inline %q vs pool %q", inDetail, poolDetail)
			}
			if inDetail != r.wantDetail {
				t.Errorf("detail = %q, want %q", inDetail, r.wantDetail)
			}
			if fake.dispatchCount() != 0 {
				t.Errorf("pool dispatched %d beads, want 0 (index render fails before dispatch)", fake.dispatchCount())
			}
			// The settle is NON-retryable on both drivers (⚑B2) — a retryable index failure
			// would put an unfixable render on a retry treadmill.
			if settledRetryableFor(t, inRes.Events, "d:0") {
				t.Errorf("inline settle marked retryable, want non-retryable")
			}
			if settledRetryableFor(t, poolEvents, "d:0") {
				t.Errorf("pool settle marked retryable, want non-retryable")
			}
		})
	}
}

// TestSharedLaneRetryIndexFailStopsEarly pins the fail-fast the non-retryable ⚑B2 clause
// exists for: a retry-wrapped index-failing do runs EXACTLY ONE attempt — the non-retryable
// failure stops early with the unused budget stamped (retries_remaining 1 of attempts 2),
// never re-minting an attempt whose render can only fail again.
func TestSharedLaneRetryIndexFailStopsEarly(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-slx-retryidx"
	fake := newFakeWorkStore()
	retry := retryNode(`{"kind":"literal","value":2}`, slxDoIndex("body", "x=", "missing[0]"))
	doc := decodeIR(t, bundleDoc(arrField("items"), retry, ""))
	input := map[string]any{"items": []any{"a"}}

	r, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("advance = %+v err %v, want Sealed (non-retryable settle decides in-pass)", r, err)
	}
	events := streamStored(t, store, streamID)
	acts := settledActivations(t, events)
	if !settledActFailed(acts, "body:0") {
		t.Fatalf("attempt 0 = %v, want body:0 failed", acts)
	}
	if settledActHas(acts, "body:1") {
		t.Errorf("body:1 minted — a non-retryable index failure must NOT re-attempt")
	}
	outcome, reason, rem, _ := loopSettle(t, events, "attempt:0")
	if outcome != engine.OutcomeFailed || reason == "exhausted" {
		t.Errorf("retry settle = (%q, %q), want (failed, not-exhausted) — the early stop, not a consumed budget", outcome, reason)
	}
	if rem == nil {
		t.Errorf("retries_remaining = nil, want 1 (the unused budget stamped by the early stop)")
	} else if *rem != 1 {
		t.Errorf("retries_remaining = %d, want 1 (the unused budget stamped by the early stop)", *rem)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("dispatched %d beads, want 0", fake.dispatchCount())
	}
}

// TestSharedLaneMissDistinctFromPresentEmpty pins §2.7 directly: an ABSENT base and a
// PRESENT-empty base both settle failed but carry DISTINCT details (the scope key-existence
// check comes BEFORE the decode).
func TestSharedLaneMissDistinctFromPresentEmpty(t *testing.T) {
	ctx := context.Background()
	absDoc := decodeIR(t, bundleDoc(arrField("items"), slxDoIndex("d", "", "missing[0]"), ""))
	emptyDoc := decodeIR(t, bundleDoc(arrField("items"), slxDoIndex("d", "", "items[0]"), ""))
	absRes, err := engine.RunWithOptions(ctx, newStore(t), absDoc, map[string]any{"items": []any{"a"}}, engine.Options{Host: passDoStub("d")})
	if err != nil {
		t.Fatalf("absent run: %v", err)
	}
	emptyRes, err := engine.RunWithOptions(ctx, newStore(t), emptyDoc, map[string]any{"items": []any{}}, engine.Options{Host: passDoStub("d")})
	if err != nil {
		t.Fatalf("empty run: %v", err)
	}
	if a, e := settledDetailFor(t, absRes.Events, "d:0"), settledDetailFor(t, emptyRes.Events, "d:0"); a == e {
		t.Errorf("absent and present-empty share detail %q — must differ", a)
	}
}

// TestSharedLaneMixedTemplateBoundary pins §1.2.4 / §2.7 (the mixed template): a do template
// carrying BOTH a missing PLAIN ref (renders silent "") and a missing INDEXED base (settles
// failed) settles failed — the plain miss stays silent, the indexed miss drives the failure.
func TestSharedLaneMixedTemplateBoundary(t *testing.T) {
	ctx := context.Background()
	mixed := `{"kind":"do","id":"d","name":"d","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":"pre {{ gone }} mid {{ missing[0] }} post","template":{"parts":[` +
		`{"kind":"text","value":"pre "},` +
		`{"kind":"interp","expr":{"kind":"ref","name":"gone"}},` +
		`{"kind":"text","value":" mid "},` +
		`{"kind":"interp","expr":{"kind":"literal","value":"missing[0]"}},` +
		`{"kind":"text","value":" post"}]},` +
		`"source":{"kind":"inline"},"templated":true,"language":"markdown","syntax":"bare","origin":{"uri":"t","line":1,"col":0}}}`
	doc := decodeIR(t, bundleDoc(arrField("items"), mixed, ""))
	res, err := engine.RunWithOptions(ctx, newStore(t), doc, map[string]any{"items": []any{"a"}}, engine.Options{Host: passDoStub("d")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := settledOutcomeByID(t, res.Events)["d"]; got != engine.OutcomeFailed {
		t.Fatalf("d outcome = %q, want failed (the indexed miss drives failure)", got)
	}
	if got := settledDetailFor(t, res.Events, "d:0"); got != `index base "missing" is not in scope` {
		t.Errorf("detail = %q, want the index-miss detail (the plain {{ gone }} miss must stay silent)", got)
	}
}

// TestSharedLaneEmptyArrayAttemptOneDivergence pins §2.7 (the PINNED divergence): a marquee
// over an EMPTY array renders attempt 1's items[iteration - 1] = items[0] BEFORE the cond,
// which is out of range → the lane SETTLES FAILED (the reference would render "" and exit).
// The loop then exits via the failed clause. Authoring guard: `if length(items) > 0`.
func TestSharedLaneEmptyArrayAttemptOneDivergence(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	res, err := engine.RunWithOptions(ctx, newStore(t), doc, map[string]any{"work_items": []any{}}, engine.Options{Host: passDoStub("stage/lane")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	acts := settledActivations(t, res.Events)
	if !settledActFailed(acts, "stage/lane:0") {
		t.Fatalf("attempt 0 = %v, want stage/lane:0 FAILED (empty-array out-of-range divergence)", acts)
	}
	if settledActHas(acts, "stage/lane:1") {
		t.Errorf("stage/lane:1 minted — the failed clause should exit after the out-of-range attempt 0")
	}
	if got := settledDetailFor(t, res.Events, "stage/lane:0"); got != `index 0 out of range for "items" (length 0)` {
		t.Errorf("detail = %q, want the empty-array out-of-range detail", got)
	}
}

// TestSharedLaneRenderForms pins §2.8: the accepted index forms render correctly (a literal
// int, an ident, an ident-minus-int, a kebab base), and a non-pre-grammar literal survives
// VERBATIM (`see [docs]`).
func TestSharedLaneRenderForms(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name, indexSrc, field, wantPrompt string
		input                             map[string]any
	}{
		{"literal-int", "items[0]", arrField("items"), "x=alpha", map[string]any{"items": []any{"alpha", "beta"}}},
		{"ident", "items[i]", arrField("items") + "," + strField("i"), "x=beta", map[string]any{"items": []any{"alpha", "beta"}, "i": "1"}},
		{"ident-minus-int", "items[i - 1]", arrField("items") + "," + strField("i"), "x=alpha", map[string]any{"items": []any{"alpha", "beta"}, "i": "1"}},
		{"kebab-base", "work-items[0]", `{"name":"work-items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}`, "x=alpha", map[string]any{"work-items": []any{"alpha"}}},
		{"verbatim-survival", "see [docs]", arrField("items"), "x=see [docs]", map[string]any{"items": []any{"a"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, bundleDoc(tc.field, slxDoIndex("d", "x=", tc.indexSrc), ""))
			res, err := engine.RunWithOptions(ctx, newStore(t), doc, tc.input, engine.Options{Host: passDoStub("d")})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			prompts := effectPrompts(t, res.Events)
			if len(prompts) != 1 || prompts[0] != tc.wantPrompt {
				t.Errorf("prompt = %v, want [%q]", prompts, tc.wantPrompt)
			}
		})
	}
}

// TestSharedLaneDefaultedArraySubInputLengthAtDepth pins §2.5's defaulted-UNBOUND arm (the
// depth half of the ⚑ROOT-DELTA pin) through a REAL driver: a sub-formula's array input
// defaulted to ["a","b"] and left unbound arrives in the loop scope as decoded []any
// (typedSubInput's fld.Default arm), so `iteration >= length(items)` exits at EXACTLY 2
// attempts — a string-typed default (`length("[\"a\",\"b\"]")` = 9 UTF-16 units) would run 9.
func TestSharedLaneDefaultedArraySubInputLengthAtDepth(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	defaultedItems := `{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":false,"default":["a","b"],"body":false}`
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", defaultedItems, repeatNode(execNode("body", `echo "r {{iteration}}"`, nil), slxLengthOnlyCond()))))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	acts := settledActivations(t, res.Events)
	if !settledActHas(acts, "wrap/body:0") || !settledActHas(acts, "wrap/body:1") {
		t.Fatalf("attempts = %v, want wrap/body:0 and wrap/body:1 (length over the typed default)", acts)
	}
	if settledActHas(acts, "wrap/body:2") {
		t.Errorf("wrap/body:2 settled — length counted the default's RENDER STRING, not the []any (⚑ROOT-DELTA depth half)")
	}
}

// TestSharedLaneRootGuardLengthDrives pins the amended §1.1.7's ROOT direction through the
// REAL lowering + driver (not a hand-built loopScope): a root guard `length(items) > 0`
// evaluates over the TYPED d.input — the then runs for a non-empty array and is skipped
// ("no branch taken") for an empty one.
func TestSharedLaneRootGuardLengthDrives(t *testing.T) {
	ctx := context.Background()
	cond := `{"kind":"operator","op":">","operands":[` +
		`{"kind":"call","name":"length","args":[{"kind":"ref","name":"items"}]},{"kind":"literal","value":0}]}`
	guard := `{"kind":"guard","id":"g","name":"g","after":[],"cond":` + cond + `,` +
		`"then":{"kind":"exec","id":"gthen","name":"gthen","after":[],` +
		`"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo ran"}}}`
	doc := decodeIR(t, bundleDoc(arrField("items"), guard, ""))

	res, err := engine.Run(ctx, newStore(t), doc, map[string]any{"items": []any{"a"}})
	if err != nil {
		t.Fatalf("non-empty run: %v", err)
	}
	if got := res.NodeOutputs["gthen"]; got != "ran" {
		t.Errorf("non-empty array: then output = %q, want %q (length > 0 true)", got, "ran")
	}

	res, err = engine.Run(ctx, newStore(t), doc, map[string]any{"items": []any{}})
	if err != nil {
		t.Fatalf("empty run: %v", err)
	}
	if got := res.NodeOutputs["gthen"]; got != "" {
		t.Errorf("empty array: then output = %q, want empty (length > 0 false — [] must NOT be truthy)", got)
	}
}

// TestReducerVersionStaysFour pins §2.11: the SLX slice adds no folded field, so the
// reducer version is unchanged at 4 (the failed settle reuses the existing Detail fold).
func TestReducerVersionStaysFour(t *testing.T) {
	if v := engine.Reducer().ReducerVersion(); v != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4 (SLX adds no fold state)", v)
	}
}

// TestSharedLaneCrashBeforeActivateReRenders pins §2.9 (crashBeforeActivate seam): a crash on
// the marquee attempt-0 BEFORE node.activated leaves nothing dispatched; the re-Advance
// RE-RENDERS the indexed prompt byte-identically (the iteration is re-seeded
// deterministically) and dispatches "work on alpha".
func TestSharedLaneCrashBeforeActivateReRenders(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-slx-crashbefore"
	fake := newFakeWorkStore()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashBeforeActivate && act == "stage/lane:0" {
			return fmt.Errorf("injected crash before activate")
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("dispatched %d before activate, want 0", fake.dispatchCount())
	}

	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("re-advance = %+v err %v, want Parked", r2, err)
	}
	if got := fake.dispatchPromptFor(t, "stage/lane:0"); got != "work on alpha" {
		t.Fatalf("re-rendered attempt-0 prompt = %q, want %q (deterministic re-seed)", got, "work on alpha")
	}
}

// TestSharedLaneCrashAfterDispatchReuses pins §2.9 (post-activate/CrashAfterDispatch seam):
// a crash AFTER the marquee attempt-0 bead is created re-adopts the SAME bead on resume with
// the FOLDED prompt (NO re-render — the HIGH-1 idem-token discipline), minting no second bead.
func TestSharedLaneCrashAfterDispatchReuses(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-slx-crashafter"
	fake := newFakeWorkStore()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{"alpha", "beta"}}

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == "stage/lane:0" {
			return fmt.Errorf("injected crash after dispatch")
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1 (bead created)", fake.dispatchCount())
	}

	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("re-advance = %+v err %v, want Parked (re-adopt the findable bead)", r2, err)
	}
	fake.mu.Lock()
	beadCount := fake.seq
	fake.mu.Unlock()
	if beadCount != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 (folded reuse, no re-render/re-mint)", beadCount)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].Activation != "stage/lane:0" {
		t.Fatalf("InFlight = %+v, want the re-adopted stage/lane:0", r2.InFlight)
	}
}

// TestSharedLaneIndexSettleCrashWindowPlainDo pins the AMENDED ⚑B2 crash convergence
// (plain-do route): a death BETWEEN settleIndexRenderFailed's two appends leaves an
// ENGINE-mode activated-unsettled node with no bead; the re-Advance must detect the
// half-settled window, re-render (deterministically re-erroring), and land the SAME
// failed{detail} settle as the no-crash run — NEVER dispatch a garbage empty-prompt bead.
func TestSharedLaneIndexSettleCrashWindowPlainDo(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, bundleDoc(arrField("items"), slxDoIndex("d", "x=", "missing[0]"), ""))
	input := map[string]any{"items": []any{"a"}}

	// No-crash baseline: the settle detail the crashed run must converge to.
	baseStore := newStore(t)
	baseFake := newFakeWorkStore()
	rb, err := engine.Advance(ctx, baseStore, doc, "gcg-slx-cwdo-base", input, baseFake.opts())
	if err != nil || !rb.Sealed {
		t.Fatalf("baseline advance = %+v err %v, want Sealed", rb, err)
	}
	baseDetail := settledDetailFor(t, streamStored(t, baseStore, "gcg-slx-cwdo-base"), "d:0")

	// Crashed run: die between the activate and the settle.
	store := newStore(t)
	fake := newFakeWorkStore()
	streamID := "gcg-slx-cwdo"
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterActivate && act == "d:0" {
			return fmt.Errorf("injected crash between the settle pair")
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash (the ⚑B2 between-appends seam is missing)")
	}
	// The half-settled window on disk: activated, NOT settled, no bead dispatched.
	crashed := streamStored(t, store, streamID)
	if !slxHasNodeActivated(t, crashed, "d:0") {
		t.Fatal("no node.activated for d:0 — the crash fired before the first append")
	}
	if settledActHas(settledActivations(t, crashed), "d:0") {
		t.Fatal("d:0 settled before the crash boundary — the seam is not between the two appends")
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("dispatched %d beads before the crash, want 0", fake.dispatchCount())
	}

	// Re-Advance converges: same failed{detail} settle, still zero beads.
	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("re-advance = %+v err %v, want Sealed (convergent resume)", r2, err)
	}
	events := streamStored(t, store, streamID)
	if got := settledOutcomeByID(t, events)["d"]; got != engine.OutcomeFailed {
		t.Fatalf("resumed d outcome = %q, want failed", got)
	}
	if got := settledDetailFor(t, events, "d:0"); got != baseDetail {
		t.Errorf("resumed detail = %q, want the no-crash detail %q", got, baseDetail)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("crash-resume dispatched %d beads, want 0 (the garbage empty-prompt bead bug)", fake.dispatchCount())
	}
}

// TestSharedLaneIndexSettleCrashWindowLoopAttempt pins the AMENDED ⚑B2 crash convergence
// (loop-attempt route): the empty-array marquee's attempt-0 render index-fails; a death
// between the settle pair leaves an engine-mode activated-unsettled ATTEMPT node, which the
// unamended advanceLoop live-attempt arm parks on forever. The re-Advance must detect the
// window, re-settle failed{detail} identically to the no-crash run, decide (the failed
// clause exits), and seal — with zero beads ever dispatched.
func TestSharedLaneIndexSettleCrashWindowLoopAttempt(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, slxMarqueeIR(slxDoIndex("lane", "work on ", "items[iteration - 1]"), slxMarqueeCond()))
	input := map[string]any{"work_items": []any{}}

	// No-crash pool baseline: attempt 0 settles failed{out-of-range}, the failed clause
	// exits, the run seals failed — all without dispatching.
	baseStore := newStore(t)
	baseFake := newFakeWorkStore()
	rb, err := engine.Advance(ctx, baseStore, doc, "gcg-slx-cwloop-base", input, baseFake.opts())
	if err != nil || !rb.Sealed {
		t.Fatalf("baseline advance = %+v err %v, want Sealed in one pass (settle-in-pass decide)", rb, err)
	}
	if rb.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("baseline run outcome = %q, want failed (the lane failed)", rb.Run.Outcome)
	}
	baseDetail := settledDetailFor(t, streamStored(t, baseStore, "gcg-slx-cwloop-base"), "stage/lane:0")
	if baseFake.dispatchCount() != 0 {
		t.Fatalf("baseline dispatched %d beads, want 0", baseFake.dispatchCount())
	}

	// Crashed run: die between the attempt's activate and settle.
	store := newStore(t)
	fake := newFakeWorkStore()
	streamID := "gcg-slx-cwloop"
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterActivate && act == "stage/lane:0" {
			return fmt.Errorf("injected crash between the settle pair")
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash (the ⚑B2 between-appends seam is missing)")
	}
	crashed := streamStored(t, store, streamID)
	if !slxHasNodeActivated(t, crashed, "stage/lane:0") {
		t.Fatal("no node.activated for stage/lane:0 — the crash fired before the first append")
	}
	if settledActHas(settledActivations(t, crashed), "stage/lane:0") {
		t.Fatal("stage/lane:0 settled before the crash boundary — the seam is not between the two appends")
	}

	// Re-Advance converges: the live-attempt arm must NOT park forever on the half-settled
	// attempt — it re-settles, the failed clause decides, the run seals failed.
	r2, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("re-advance = %+v err %v, want Sealed (the unamended arm parks forever here)", r2, err)
	}
	if r2.Run.Outcome != engine.OutcomeFailed {
		t.Fatalf("resumed run outcome = %q, want failed", r2.Run.Outcome)
	}
	events := streamStored(t, store, streamID)
	if got := settledDetailFor(t, events, "stage/lane:0"); got != baseDetail {
		t.Errorf("resumed detail = %q, want the no-crash detail %q", got, baseDetail)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("crash-resume dispatched %d beads, want 0", fake.dispatchCount())
	}
}

// TestSharedLaneFixtureDrivesPool proves the bundled corpus fixture drives END-TO-END on the
// pool driver (§2.10): the dispatch same-session arm mints the doWorkShared run, whose repeat
// leaf-loop DISPATCHES two SEQUENTIAL lane beads rendering element 0 then element 1, then
// exits via the length clause and seals pass.
func TestSharedLaneFixtureDrivesPool(t *testing.T) {
	ctx := context.Background()
	data, err := os.ReadFile(filepath.Join("..", "..", "..", "examples", "lumen", "shared-lane.lumen.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc := decodeIR(t, string(data))
	store := newStore(t)
	streamID := "gcg-slx-fixture"
	fake := newFakeWorkStore()
	input := map[string]any{"mode": "same-session", "work_items": []any{"alpha", "beta"}}

	var prompts []string
	sealed := false
	for i := 0; i < 6 && !sealed; i++ {
		r, err := engine.Advance(ctx, store, doc, streamID, input, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i, err)
		}
		for _, w := range r.InFlight {
			prompts = append(prompts, fake.dispatchPromptFor(t, w.Activation))
			fake.settleAct(t, w.Activation, engine.OutcomePass, "ok")
		}
		sealed = r.Sealed
	}
	if !sealed {
		t.Fatalf("fixture did not seal within the pass budget; prompts=%v", prompts)
	}
	want := []string{"Work item: alpha", "Work item: beta"}
	if len(prompts) != len(want) || prompts[0] != want[0] || prompts[1] != want[1] {
		t.Fatalf("lane prompts = %v, want %v (two sequential indexed renders through the DAR arm)", prompts, want)
	}
}
