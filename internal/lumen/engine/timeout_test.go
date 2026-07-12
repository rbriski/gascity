package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// --- timeout (TNK) driver fixtures ------------------------------------------

// tnkLitDur renders a closed-expr literal duration value.
func tnkLitDur(v string) string {
	b, _ := json.Marshal(v)
	return `{"kind":"literal","value":` + string(b) + `}`
}

// tnkTimeoutExec renders a timeout node (id "check") with a duration and an EXEC body (id "v")
// that runs engine-side — exec never pools, so the body settles inline one pass on both drivers.
func tnkTimeoutExec(after []string, durVal, bodyScript string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"timeout","id":"check","name":"check","after":` + string(a) +
		`,"duration":` + tnkLitDur(durVal) + `,"body":` + execNode("v", bodyScript, nil) + `}`
}

// tnkTimeoutExecExit renders a timeout with an exec body carrying explicit pass/retryable exit
// sets (for the ⚑B1 retryable-drop pin — a failed retryable body vs the non-retryable wrapper).
func tnkTimeoutExecExit(id string, after []string, durVal, bodyID, bodyScript string, pass, retryable []int) string {
	a, _ := json.Marshal(after)
	return `{"kind":"timeout","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"duration":` + tnkLitDur(durVal) + `,"body":` + execNodeExit(bodyID, bodyScript, pass, retryable) + `}`
}

// tnkTimeoutDo renders a timeout with a DO body (pool-materializable, like a guard-then do).
func tnkTimeoutDo(id string, after []string, durVal, bodyID, bodyPrompt string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"timeout","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"duration":` + tnkLitDur(durVal) + `,"body":` + doNode(bodyID, bodyPrompt, nil) + `}`
}

// tnkActivatedDuration returns the duration carried by an activation's node.activated (and
// whether the field was present).
func tnkActivatedDuration(t *testing.T, events []graphstore.StoredEvent, activation string) (string, bool) {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var head struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &head); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if head.Activation != activation {
			continue
		}
		keys := map[string]json.RawMessage{}
		if err := json.Unmarshal(e.Payload, &keys); err != nil {
			t.Fatalf("decode node.activated map: %v", err)
		}
		raw, ok := keys["duration"]
		if !ok {
			return "", false
		}
		var dur string
		_ = json.Unmarshal(raw, &dur)
		return dur, true
	}
	t.Fatalf("no node.activated for activation %q", activation)
	return "", false
}

// tnkSettleRetryable reports whether an activation's outcome.settled carried a `retryable`
// field and its value.
func tnkSettleRetryable(t *testing.T, events []graphstore.StoredEvent, activation string) (present, value bool) {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var head struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &head); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if head.Activation != activation {
			continue
		}
		keys := map[string]json.RawMessage{}
		if err := json.Unmarshal(e.Payload, &keys); err != nil {
			t.Fatalf("decode outcome.settled map: %v", err)
		}
		raw, ok := keys["retryable"]
		if !ok {
			return false, false
		}
		var b bool
		_ = json.Unmarshal(raw, &b)
		return true, b
	}
	t.Fatalf("no outcome.settled for activation %q", activation)
	return false, false
}

// --- §2.1 marquee at depth (both drivers) -----------------------------------

// TestTimeoutMarqueeAtDepthInline pins §2.1 for the inline driver: a run sub-formula whose
// steps end `check: timeout 5m { v: exec … }` with a downstream sibling `after:["check"]`,
// driven through a repeat-body-run mint. The body exec runs engine-side at stage/0/v, the
// wrapper settles the body's outcome transparently at stage/0/check, the downstream report
// renders {{check}} (the body output), and the repeat cond reads stage.outcome normally.
func TestTimeoutMarqueeAtDepthInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil, runNodeJSON("stage", nil, "greeter", "name", "who"), runCondPassOrIter()),
		subDoc("greeter", strField("name"),
			tnkTimeoutExec(nil, "5m", `echo "checked {{ name }}"`)+","+
				execNode("report", `echo "report {{ check }}"`, []string{"check"})),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/0/v"]; got != "checked world" {
		t.Errorf("body stage/0/v = %q, want %q (env-seeded exec ran engine-side)", got, "checked world")
	}
	if got := res.NodeOutputs["stage/0/check"]; got != "checked world" {
		t.Errorf("wrapper stage/0/check = %q, want %q (transparent from the body)", got, "checked world")
	}
	if got := res.NodeOutputs["stage/0/report"]; got != "report checked world" {
		t.Errorf("downstream stage/0/report = %q, want %q ({{check}} rendered the body output)", got, "report checked world")
	}
	// The wrapper's node.activated carried the advisory duration VERBATIM.
	if dur, ok := tnkActivatedDuration(t, res.Events, "stage/0/check:0"); !ok || dur != "5m" {
		t.Errorf("stage/0/check duration = (%q,%v), want (5m,true)", dur, ok)
	}
}

// tnkMarqueeDoc builds the §2.1 marquee: repeat { run stage -> greeter{ check: timeout 5m {
// v: exec } ; report: exec {{check}} after check } } until stage.outcome == pass. The sub is
// exec-only, so it seals in one pass on both drivers.
func tnkMarqueeDoc() string {
	return bundleDoc(
		strField("who"),
		repeatRunLoop(nil, runNodeJSON("stage", nil, "greeter", "name", "who"), runCondPassOrIter()),
		subDoc("greeter", strField("name"),
			tnkTimeoutExec(nil, "5m", `echo "checked {{ name }}"`)+","+
				execNode("report", `echo "report {{ check }}"`, []string{"check"})))
}

// TestTimeoutMarqueeAtDepthPool pins §2.1 for the POOL driver: the same repeat-body-run marquee
// driven via Advance under a PoolRouter seals in one pass (the exec body settles engine-side),
// the wrapper settles transparently at stage/0/check, and it still stamps duration 5m.
func TestTimeoutMarqueeAtDepthPool(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, tnkMarqueeDoc())
	r, err := engine.Advance(ctx, store, doc, "gcg-tnk-marqueepool", map[string]any{"who": "world"}, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("advance = %+v err %v, want Sealed in one pass (exec-only run body)", r, err)
	}
	if r.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", r.Run.Outcome)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("dispatch count = %d, want 0 (an exec body never dispatches)", fake.dispatchCount())
	}
	settled := settledOutcomeByID(t, r.Run.Events)
	if settled["stage/0/check"] != engine.OutcomePass {
		t.Errorf("wrapper stage/0/check = %q, want pass (transparent from the exec body)", settled["stage/0/check"])
	}
	if dur, ok := tnkActivatedDuration(t, r.Run.Events, "stage/0/check:0"); !ok || dur != "5m" {
		t.Errorf("stage/0/check duration = (%q,%v), want (5m,true)", dur, ok)
	}
}

// --- §2.2 root twin + pool/inline byte-parity -------------------------------

// tnkRootDoc is the root twin IR JSON (blockDoc-wrapped).
func tnkRootDoc() string {
	return blockDoc("root",
		tnkTimeoutExec(nil, "5m", `echo checked`),
		execNode("done", `echo "done {{ check }}"`, []string{"check"}))
}

// TestTimeoutRootTwinInline pins §2.2 (inline): a top-level exec-body timeout settles from its
// body, the downstream gate fires and renders {{check}}, the wrapper carries duration 5m.
func TestTimeoutRootTwinInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, tnkRootDoc())
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["check"]; got != "checked" {
		t.Errorf("wrapper check = %q, want %q (transparent from v)", got, "checked")
	}
	if got := res.NodeOutputs["done"]; got != "done checked" {
		t.Errorf("downstream done = %q, want %q", got, "done checked")
	}
	if dur, ok := tnkActivatedDuration(t, res.Events, "check:0"); !ok || dur != "5m" {
		t.Errorf("check duration = (%q,%v), want (5m,true)", dur, ok)
	}
	// The body activation carries NO duration (only the wrapper does).
	if dur, ok := tnkActivatedDuration(t, res.Events, "v:0"); ok {
		t.Errorf("body v duration = %q present, want absent (only the wrapper stamps duration)", dur)
	}
}

// TestTimeoutRootTwinPoolInlineByteParity pins §2.2 byte-parity: the exec-body root twin
// journals BYTE-IDENTICALLY inline (Run) vs pool (Advance) — both emit exactly act(wrapper,
// +duration) → act(body) → settle(body) → settle(wrapper), zero dispatches, no effect rows.
func TestTimeoutRootTwinPoolInlineByteParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, tnkRootDoc())

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, nil)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-tnk-parity", nil, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec body runs inline on both)", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an exec body never dispatches)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// --- blocked wrapper / after-gate cluster (red-team P1-1) --------------------

// tnkHasActivation reports whether ANY node.activated exists for the given activation
// (non-fatal — the blocked-wrapper pins assert the body was NEVER activated).
func tnkHasActivation(t *testing.T, events []graphstore.StoredEvent, activation string) bool {
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

// TestTimeoutBlockedWrapperSkipCascadesBothDrivers pins the blocked-wrapper/after-gate
// cluster (red-team P1-1): a timeout gated on a FAILED upstream settles SKIPPED on BOTH
// drivers — its body is NEVER activated (no exec side effect), the downstream cascades, and
// the two journals agree BYTE-for-byte. Kills (a) dropping rawAfter at lowering (the gate
// never resolves) and (b) dropping the !d.blocked(u) check in advanceUnit's timeout clause
// (a blocked pool timeout would run its body despite the failed dep and diverge from inline).
func TestTimeoutBlockedWrapperSkipCascadesBothDrivers(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("root",
		execNode("fail", `exit 1`, nil),
		tnkTimeoutExec([]string{"fail"}, "5m", `echo never`),
		execNode("done", `echo d`, []string{"check"})))

	// Inline driver.
	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, nil)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	assertBlockedWrapper(t, inRes.Events, "inline")

	// Pool driver: one Advance pass seals (everything settles or skips), zero dispatches.
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-tnk-blocked", nil, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (a blocked wrapper offers nothing)", n)
	}
	assertBlockedWrapper(t, r.Run.Events, "pool")

	// The strongest form: the two journals agree byte-for-byte after run.started.
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// assertBlockedWrapper asserts the blocked-wrapper shape on one driver's journal: the wrapper
// skipped, the body NEVER activated, the downstream skipped.
func assertBlockedWrapper(t *testing.T, events []graphstore.StoredEvent, driver string) {
	t.Helper()
	settled := settledOutcomeByID(t, events)
	if settled["check"] != engine.OutcomeSkipped {
		t.Errorf("[%s] wrapper check = %q, want skipped (failed upstream skip-cascades)", driver, settled["check"])
	}
	if tnkHasActivation(t, events, "v:0") {
		t.Errorf("[%s] body v:0 was ACTIVATED despite the failed gate — a blocked wrapper must never run its body", driver)
	}
	if settled["done"] != engine.OutcomeSkipped {
		t.Errorf("[%s] downstream done = %q, want skipped (the cascade continues)", driver, settled["done"])
	}
}

// --- §2.4 outcome transparency + retryable drop -----------------------------

// TestTimeoutBodyFailedWrapperFailedSkipCascades pins §2.4: a FAILED body makes the wrapper
// settle failed and the downstream gate skip-cascades.
func TestTimeoutBodyFailedWrapperFailedSkipCascades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("root",
		tnkTimeoutExec(nil, "5m", `exit 3`),
		execNode("done", `echo ran`, []string{"check"})))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Fatalf("run outcome = %q, want failed", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["check"] != engine.OutcomeFailed {
		t.Errorf("wrapper check = %q, want failed (transparent from the failed body)", settled["check"])
	}
	if settled["v"] != engine.OutcomeFailed {
		t.Errorf("body v = %q, want failed", settled["v"])
	}
	if settled["done"] != engine.OutcomeSkipped {
		t.Errorf("downstream done = %q, want skipped (the wrapper failure skip-cascades)", settled["done"])
	}
}

// TestTimeoutRetryableDroppedAtWrapper pins ⚑B1 (BLOCKER): a body exec that FAILS with a
// retryable exit code carries retryable=true on ITS settle, but the wrapper's transparent
// settle DROPS it (appendSettled hardcodes retryable=false) — guard/dispatch parity. Pinned on
// BOTH drivers.
func TestTimeoutRetryableDroppedAtWrapper(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("root",
		tnkTimeoutExecExit("check", nil, "5m", "v", `exit 7`, []int{0}, []int{7})))

	// Inline.
	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, nil)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	assertRetryableDrop(t, inRes.Events, "inline")

	// Pool (exec body still settles inline in-pass under a router).
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-tnk-retryable", nil, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed", r, err)
	}
	assertRetryableDrop(t, r.Run.Events, "pool")
}

// assertRetryableDrop asserts the body v:0 settle carries retryable=true and the wrapper
// check:0 settle drops it (absent or false).
func assertRetryableDrop(t *testing.T, events []graphstore.StoredEvent, driver string) {
	t.Helper()
	if present, val := tnkSettleRetryable(t, events, "v:0"); !present || !val {
		t.Errorf("[%s] body v:0 retryable = (present=%v,val=%v), want (true,true) — the body keeps its classification", driver, present, val)
	}
	if present, val := tnkSettleRetryable(t, events, "check:0"); present && val {
		t.Errorf("[%s] wrapper check:0 retryable = true, want DROPPED (absent/false) — retryable never carries to the wrapper", driver)
	}
}

// --- §2.3 duration stamping + fold-ignore -----------------------------------

// TestTimeoutDurationOnlyOnTimeoutActivations pins §2.3: the wrapper activation carries
// duration; a NON-timeout activation (a plain exec) carries no duration field at all (byte
// shape unchanged for every other kind).
func TestTimeoutDurationOnlyOnTimeoutActivations(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("root",
		execNode("prep", `echo p`, nil),
		tnkTimeoutExec([]string{"prep"}, "30s", `echo ok`)))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if dur, ok := tnkActivatedDuration(t, res.Events, "check:0"); !ok || dur != "30s" {
		t.Errorf("wrapper check duration = (%q,%v), want (30s,true)", dur, ok)
	}
	for _, act := range []string{"prep:0", "v:0"} {
		if dur, ok := tnkActivatedDuration(t, res.Events, act); ok {
			t.Errorf("%s carries a duration field %q, want none (non-timeout activations omit it)", act, dur)
		}
	}
}

// TestTimeoutFoldIgnoresDuration pins §2.3's fold-ignore: the advisory duration is NOT folded
// — rewriting the wrapper's duration to a DIFFERENT value yields the IDENTICAL StateHash
// (nodeState never carries it, so snapshots/StateHash are duration-transparent).
func TestTimeoutFoldIgnoresDuration(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, tnkRootDoc())
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	r := engine.Reducer()
	baseState, _, err := fold.Fold(r, nil, foldEvents(res.Events))
	if err != nil {
		t.Fatalf("base fold: %v", err)
	}
	rewritten := tnkRewriteDuration(t, res.Events, "check:0", "999h")
	altState, _, err := fold.Fold(r, nil, foldEvents(rewritten))
	if err != nil {
		t.Fatalf("rewritten fold: %v", err)
	}
	if baseState.StateHash() != altState.StateHash() {
		t.Fatalf("StateHash changed when the wrapper duration changed 5m→999h; the advisory field must NOT fold")
	}
	// reducerVersion STAYS 4 — TNK adds no folded field.
	if v := r.ReducerVersion(); v != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4 (TNK adds no fold state)", v)
	}
}

// tnkRewriteDuration returns a copy of events where the given activation's node.activated
// duration field is set to newDur (re-canonicalized), leaving every other event untouched.
func tnkRewriteDuration(t *testing.T, events []graphstore.StoredEvent, activation, newDur string) []graphstore.StoredEvent {
	t.Helper()
	out := make([]graphstore.StoredEvent, len(events))
	copy(out, events)
	for i, e := range out {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		m := map[string]json.RawMessage{}
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		var act string
		_ = json.Unmarshal(m["activation"], &act)
		if act != activation {
			continue
		}
		m["duration"], _ = json.Marshal(newDur)
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("re-marshal: %v", err)
		}
		canonical, err := canon.Canonicalize(raw)
		if err != nil {
			t.Fatalf("canonicalize: %v", err)
		}
		out[i].Payload = canonical
	}
	return out
}

// --- §2.7 do-body pool dispatch + non-promise -------------------------------

// TestTimeoutDoBodyPoolDispatch pins §2.7: a do-body timeout materializes its body as ordinary
// pool work under a router (guard-then-do parity), then settles transparently when the bead
// closes — and the dispatched bead carries NO budget/duration metadata (the §1.2 non-promise:
// enforcement reads the journal wrapper activation, never the bead).
func TestTimeoutDoBodyPoolDispatch(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("root",
		tnkTimeoutDo("check", nil, "5m", "v", "do the check")))

	// First pass: the do body materializes as pool work; the wrapper parks unsettled.
	r1, err := engine.Advance(ctx, store, doc, "gcg-tnk-dobody", nil, fake.opts())
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed {
		t.Fatalf("sealed on pass 1, want the do body parked in flight")
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("dispatch count = %d, want 1 (the do body dispatches as pool work)", fake.dispatchCount())
	}
	// The dispatched bead carries NO budget/duration metadata — the non-promise pin.
	d := fake.dispatches[0]
	blob, _ := json.Marshal(d)
	if containsDuration(blob) {
		t.Fatalf("dispatched work bead carries duration/budget metadata %s; enforcement must read the journal wrapper, NOT the bead", blob)
	}

	// Close the body bead; the next Advance observes it and settles the wrapper transparently.
	fake.settleAct(t, "v:0", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, "gcg-tnk-dobody", nil, fake.opts())
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v, want Sealed pass (wrapper settles from the closed do body)", r2)
	}
	settled := settledOutcomeByID(t, r2.Run.Events)
	if settled["check"] != engine.OutcomePass {
		t.Errorf("wrapper check = %q, want pass (transparent from the do body)", settled["check"])
	}
	// The wrapper still stamped its advisory duration on its own node.activated.
	if dur, ok := tnkActivatedDuration(t, r2.Run.Events, "check:0"); !ok || dur != "5m" {
		t.Errorf("wrapper check duration = (%q,%v), want (5m,true)", dur, ok)
	}
}

// containsDuration reports whether a JSON blob mentions a duration/budget key (a crude but
// sufficient guard for the non-promise pin — the WorkDispatch struct has no such field).
func containsDuration(blob []byte) bool {
	m := map[string]json.RawMessage{}
	if json.Unmarshal(blob, &m) != nil {
		return false
	}
	for _, k := range []string{"duration", "budget", "Duration", "Budget"} {
		if _, ok := m[k]; ok {
			return true
		}
	}
	return false
}

// TestTimeoutDoBodyInline pins §2.7's inline-driver leg: a do-body timeout run inline with a
// Host runs its body via runDo and settles the wrapper transparently — the do-body twin of the
// pool pin, and the wrapper still stamps its advisory duration.
func TestTimeoutDoBodyInline(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("root",
		tnkTimeoutDo("check", nil, "5m", "v", "do the check")))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: passDoStub("v")})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["check"] != engine.OutcomePass || settled["v"] != engine.OutcomePass {
		t.Errorf("check=%q v=%q, want both pass (wrapper transparent from the do body)", settled["check"], settled["v"])
	}
	if dur, ok := tnkActivatedDuration(t, res.Events, "check:0"); !ok || dur != "5m" {
		t.Errorf("wrapper check duration = (%q,%v), want (5m,true)", dur, ok)
	}
}

// --- §2.6 crash + drop/refold render parity ---------------------------------

// TestTimeoutCrashAfterActivateInline pins §2.6 (inline): a crash at the wrapper's
// crashAfterActivate window (activated-unsettled) converges on resume — the body runs
// at-least-once, the wrapper settles, and the downstream {{check}} render EQUALS genesis.
func TestTimeoutCrashAfterActivateInline(t *testing.T) {
	doc := decodeIR(t, tnkRootDoc())
	resumed, store, stream := injectCrashThenResume(t, doc, passDoStub(), engine.CrashAfterActivate, "check:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass (converges past the wrapper crash)", resumed.Outcome)
	}
	if got := resumed.NodeOutputs["done"]; got != "done checked" {
		t.Errorf("post-resume downstream done = %q, want %q (render EQUALS genesis)", got, "done checked")
	}
	if err := store.Verify(context.Background(), stream); err != nil {
		t.Fatalf("Verify after resume: %v", err)
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestTimeoutCrashAfterActivatePool pins §2.6 (pool): the DAR lesson — advanceTimeout injects
// crashAfterActivate right after ensureDecisionActivated, so the pool driver's
// activated-unsettled wrapper window is injectable too; resume converges.
func TestTimeoutCrashAfterActivatePool(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, tnkRootDoc())

	errCrash := "crash"
	fired := false
	var crashedStream string
	restore := engine.SetCrashHookForTest(func(b, streamID, act string) error {
		if b == engine.CrashAfterActivate && act == "check:0" && !fired {
			fired = true
			crashedStream = streamID
			return &tnkCrashErr{errCrash}
		}
		return nil
	})
	store := newStore(t)
	fake := newFakeWorkStore()
	_, err := engine.Advance(ctx, store, doc, "gcg-tnk-crashpool", nil, fake.opts())
	restore()
	if err == nil || !fired {
		t.Fatalf("advance err = %v fired=%v, want the injected crash at the pool wrapper window", err, fired)
	}

	// Resume over the surviving journal converges.
	r, err := engine.Advance(ctx, store, doc, crashedStream, nil, fake.opts())
	if err != nil {
		t.Fatalf("resume advance: %v", err)
	}
	if !r.Sealed || r.Run.Outcome != engine.OutcomePass {
		t.Fatalf("resume = %+v, want Sealed pass (converges past the pool wrapper crash)", r)
	}
	if err := store.Verify(ctx, crashedStream); err != nil {
		t.Fatalf("Verify after resume: %v", err)
	}
}

// TestTimeoutRefoldSeedsWrapperScope pins §1.3 / §2.6's drop+refold discipline (the
// isAggregateKind mutant killer): a crash AFTER the wrapper settles but before the downstream
// `done` renders. On resume, reconstructOutputs must seed scope[check] — because "timeout" is
// NOT an aggregate kind, the kind-neutral reconstructOutputs seeds BOTH scope and nodeOutputs
// at the wrapper id — so `done` re-renders {{check}} identically to genesis. If "timeout"
// joined isAggregateKind, scope[check] would be dropped on refold and `done` would render the
// unresolved "done {{ check }}" (the DET render break).
func TestTimeoutRefoldSeedsWrapperScope(t *testing.T) {
	doc := decodeIR(t, tnkRootDoc())
	resumed, store, stream := injectCrashThenResume(t, doc, passDoStub(), engine.CrashAfterSettle, "check:0", 0)
	if got := resumed.NodeOutputs["done"]; got != "done checked" {
		t.Fatalf("post-refold downstream done = %q, want %q (reconstructOutputs must seed scope[check] — timeout NOT aggregate)", got, "done checked")
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	if err := store.Verify(context.Background(), stream); err != nil {
		t.Fatalf("Verify after resume: %v", err)
	}
}

// TestTimeoutPoolBodyCrashWindowConverges pins the BODY-side pool crash window (red-team
// P1-2, the DAR ⚑B2 class): a kill between act(v:0) and settle(v:0) on the pool driver leaves
// an activated-UNSETTLED body. Resume MUST take the runUnit(bu) route (the `!bn.Settled`
// disjunct in advanceTimeout's else-if), re-run the exec (at-least-once), settle v:0, and
// settle the wrapper from the REAL body output — asserted as OUTPUT == "checked", not merely
// Sealed pass. The mutant (`bn == nil` alone) skips the re-run and falls to
// settleDecisionFromBody's silent PASS/"" default: the run SEALS FALSE-PASS with an empty
// {{check}} while the body never settles.
func TestTimeoutPoolBodyCrashWindowConverges(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, tnkRootDoc())

	fired := false
	var crashedStream string
	restore := engine.SetCrashHookForTest(func(b, streamID, act string) error {
		if b == engine.CrashAfterActivate && act == "v:0" && !fired {
			fired = true
			crashedStream = streamID
			return &tnkCrashErr{"crash between act(v:0) and settle(v:0)"}
		}
		return nil
	})
	store := newStore(t)
	fake := newFakeWorkStore()
	_, err := engine.Advance(ctx, store, doc, "gcg-tnk-bodycrash", nil, fake.opts())
	restore()
	if err == nil || !fired {
		t.Fatalf("advance err = %v fired=%v, want the injected crash at the pool BODY window", err, fired)
	}

	// Resume: the body re-runs at-least-once and the wrapper settles from its REAL output.
	r, err := engine.Advance(ctx, store, doc, crashedStream, nil, fake.opts())
	if err != nil {
		t.Fatalf("resume advance: %v", err)
	}
	if !r.Sealed || r.Run.Outcome != engine.OutcomePass {
		t.Fatalf("resume = %+v, want Sealed pass", r)
	}
	settled := settledOutcomeByID(t, r.Run.Events)
	if settled["v"] != engine.OutcomePass {
		t.Fatalf("body v = %q, want pass (resume must re-run the interrupted exec, never orphan it)", settled["v"])
	}
	if got := r.Run.NodeOutputs["check"]; got != "checked" {
		t.Fatalf("wrapper output = %q, want %q (the REAL body output — not the silent PASS/\"\" default)", got, "checked")
	}
	if got := r.Run.NodeOutputs["done"]; got != "done checked" {
		t.Errorf("downstream done = %q, want %q ({{check}} rendered the real output)", got, "done checked")
	}
	if err := store.Verify(ctx, crashedStream); err != nil {
		t.Fatalf("Verify after resume: %v", err)
	}
}

// tnkCrashErr is a sentinel crash error for the pool crash-seam pin.
type tnkCrashErr struct{ msg string }

func (e *tnkCrashErr) Error() string { return e.msg }
