package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// doNode renders one agent `do` node whose prompt template is prompt (with
// {{var}} interpolation against scope), after the given deps.
func doNode(id, prompt string, after []string) string {
	afterJSON, _ := json.Marshal(after)
	promptJSON, _ := json.Marshal(prompt)
	return `{
      "kind": "do", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "source": {"kind": "prompt"},
      "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(promptJSON) + `, "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}
    }`
}

// effectPrompts returns, in seq order, the rendered prompt from each
// effect.scheduled event.
func effectPrompts(t *testing.T, events []graphstore.StoredEvent) []string {
	t.Helper()
	var out []string
	for _, e := range events {
		if e.Type != engine.EventEffectScheduled {
			continue
		}
		var p struct {
			Spec struct {
				Prompt string `json:"prompt"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode effect.scheduled: %v", err)
		}
		out = append(out, p.Spec.Prompt)
	}
	return out
}

// effectSettledResults returns, in seq order, the result field of each
// effect.settled event (ok/failed/interrupted).
func effectSettledResults(t *testing.T, events []graphstore.StoredEvent) []string {
	t.Helper()
	var out []string
	for _, e := range events {
		if e.Type != engine.EventEffectSettled {
			continue
		}
		var p struct {
			Result string `json:"result"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode effect.settled: %v", err)
		}
		out = append(out, p.Result)
	}
	return out
}

// effectSettledDetail returns the detail of the first effect.settled event.
func effectSettledDetail(t *testing.T, events []graphstore.StoredEvent) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventEffectSettled {
			continue
		}
		var p struct {
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode effect.settled: %v", err)
		}
		return p.Detail
	}
	t.Fatalf("no effect.settled event found")
	return ""
}

// TestDoStepStubbedPassFoldsThroughExecutor proves a do step run through a
// stubbed host folds a pass outcome, emits the effect.scheduled/settled pair
// before node.settled, and the run outcome reflects it.
func TestDoStepStubbedPassFoldsThroughExecutor(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun", doNode("summarize", "Summarize the repo.", nil)))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"summarize": {Outcome: enginehost.OutcomePass, Output: "three bullets", SessionRef: "sess-1"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["summarize"]; got != "three bullets" {
		t.Errorf("NodeOutputs[summarize] = %q, want the host output", got)
	}

	wantOrder := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,
		engine.EventEffectScheduled,
		engine.EventEffectSettled,
		engine.EventOutcomeSettled,
		engine.EventRunClosed,
	}
	if got := eventTypes(res.Events); !equalStrings(got, wantOrder) {
		t.Errorf("event order = %v, want %v", got, wantOrder)
	}

	settled := settledIDs(t, res.Events)
	if len(settled) != 1 || settled[0] != [2]string{"summarize", "pass"} {
		t.Errorf("settled = %v, want [{summarize pass}]", settled)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestDoStepStubbedFailFoldsFailed proves a stubbed failed do folds the node
// and the run as failed and still emits a settled effect record.
func TestDoStepStubbedFailFoldsFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun", doNode("summarize", "Summarize.", nil)))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"summarize": {Outcome: enginehost.OutcomeFailed, Detail: "model refused"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("outcome = %q, want failed", res.Outcome)
	}
	if got := closedOutcome(t, res.Events); got != engine.OutcomeFailed {
		t.Errorf("run.closed outcome = %q, want failed", got)
	}
	settled := settledIDs(t, res.Events)
	if len(settled) != 1 || settled[0] != [2]string{"summarize", "failed"} {
		t.Errorf("settled = %v, want [{summarize failed}]", settled)
	}
	// The scheduled effect always gets a settled record.
	if types := eventTypes(res.Events); !contains(types, engine.EventEffectSettled) {
		t.Errorf("event types = %v, want an effect.settled record", types)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestDoStepOutputFlowsToDownstreamPrompt proves a do step's output enters scope
// so a later step's {{ref}} interpolation sees it. This exercises the CHAINING
// pipeline, and it depends on the host actually returning an output tail: the
// StubHost (and the fake provider) carry an explicit Output, whereas the default
// subprocess provider's Peek returns "", so under it {{ref}} would interpolate ""
// in production. See DoResult.Output for that provider-dependent capture limit.
func TestDoStepOutputFlowsToDownstreamPrompt(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		doNode("summarize", "Summarize.", nil),
		doNode("publish", "Publish after {{summarize}}.", []string{"summarize"}),
	))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"summarize": {Outcome: enginehost.OutcomePass, Output: "the summary"},
		"publish":   {Outcome: enginehost.OutcomePass, Output: "done"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want pass", res.Outcome)
	}

	// The host saw the rendered prompts; the second must carry the first output.
	prompts := stub.Calls()
	if len(prompts) != 2 {
		t.Fatalf("host calls = %d, want 2", len(prompts))
	}
	if prompts[0].Prompt != "Summarize." {
		t.Errorf("first prompt = %q, want %q", prompts[0].Prompt, "Summarize.")
	}
	if prompts[1].Prompt != "Publish after the summary." {
		t.Errorf("second prompt = %q, want the interpolated summary", prompts[1].Prompt)
	}
	// And the journal records the same rendered prompts.
	if got := effectPrompts(t, res.Events); len(got) != 2 || got[1] != "Publish after the summary." {
		t.Errorf("journal prompts = %v, want the second interpolated", got)
	}
}

// TestDoStepHostInternalErrorInterruptsAndSkipsDependent covers the
// StubHost.Errs arm together with the P4.2 skip-cascade: a host internal error
// (a result the host could not produce) settles the effect interrupted and the
// node failed; RunWithOptions does NOT return an error; and the downstream do
// step, which depends on the failed one, is SKIPPED — not run — so it never
// schedules an effect. This is the correctness fix over P1, which ran every
// leaf even after an upstream failure.
func TestDoStepHostInternalErrorInterruptsAndSkipsDependent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun",
		doNode("broken", "Do a thing.", nil),
		doNode("after", "Skipped, upstream failed.", []string{"broken"}),
	))
	stub := &enginehost.StubHost{
		Errs:    map[string]error{"broken": errors.New("host exploded")},
		Results: map[string]enginehost.DoResult{"after": {Outcome: enginehost.OutcomePass, Output: "ok"}},
	}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run must not error on a host internal error: %v", err)
	}
	// Only the broken step schedules/settles an effect; the skipped dependent
	// never acts, so there is exactly one (interrupted) effect settlement.
	if got := effectSettledResults(t, res.Events); len(got) != 1 || got[0] != engine.EffectResultInterrupted {
		t.Errorf("effect settled results = %v, want [interrupted]", got)
	}
	if h := len(stub.Calls()); h != 1 {
		t.Errorf("host called %d times, want 1 (the dependent is skipped, not run)", h)
	}
	settled := settledIDs(t, res.Events)
	if len(settled) != 2 || settled[0] != [2]string{"broken", "failed"} || settled[1] != [2]string{"after", "skipped"} {
		t.Errorf("settled = %v, want [{broken failed} {after skipped}]", settled)
	}
	if nodeStatus(t, store, "broken") != "failed" {
		t.Errorf("broken node status = %q, want failed", nodeStatus(t, store, "broken"))
	}
	if nodeStatus(t, store, "after") != "skipped" {
		t.Errorf("after node status = %q, want skipped", nodeStatus(t, store, "after"))
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed (failed dominates)", res.Outcome)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestDoStepDegradedFoldsDoneNodeButOKEffect covers the degraded arm: a degraded
// host outcome projects a degraded (done) node while the effect settles ok — a
// partial success, not a failure — and dominates the run outcome as non-failed.
func TestDoStepDegradedFoldsDoneNodeButOKEffect(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun", doNode("summarize", "Summarize.", nil)))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"summarize": {Outcome: enginehost.OutcomeDegraded, Output: "partial", Detail: "truncated"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := effectSettledResults(t, res.Events); len(got) != 1 || got[0] != engine.EffectResultOK {
		t.Errorf("effect settled results = %v, want [ok]", got)
	}
	settled := settledIDs(t, res.Events)
	if len(settled) != 1 || settled[0] != [2]string{"summarize", "degraded"} {
		t.Errorf("settled = %v, want [{summarize degraded}]", settled)
	}
	if nodeStatus(t, store, "summarize") != "done" {
		t.Errorf("degraded node status = %q, want done", nodeStatus(t, store, "summarize"))
	}
	if res.Outcome != engine.OutcomeDegraded {
		t.Errorf("run outcome = %q, want degraded", res.Outcome)
	}
}

// TestDoStepUnknownOutcomeDefaultsToFailed covers the default arm of the outcome
// fold: an unrecognized host outcome settles the node and effect failed with a
// detail naming the offending value, never silently passing.
func TestDoStepUnknownOutcomeDefaultsToFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun", doNode("summarize", "Summarize.", nil)))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"summarize": {Outcome: "weird", Output: "x"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := effectSettledResults(t, res.Events); len(got) != 1 || got[0] != engine.EffectResultFailed {
		t.Errorf("effect settled results = %v, want [failed]", got)
	}
	if detail := effectSettledDetail(t, res.Events); !strings.Contains(detail, "unknown outcome") {
		t.Errorf("effect settled detail = %q, want it to name the unknown outcome", detail)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
}

// TestDoNodeWithoutHostIsRefusedByteIdentical proves nil-host byte-identity: a
// do node with no host is refused with ErrUnsupportedNode exactly as before
// P4.1, and no journal is written.
func TestDoNodeWithoutHostIsRefusedByteIdentical(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentrun", doNode("summarize", "Summarize.", nil)))

	// engine.Run is the nil-host path. The refusal happens at flatten, before
	// the writer lease or any append — exactly as the pre-P4.1 skeleton.
	_, err := engine.Run(ctx, store, doc, nil)
	if !errors.Is(err, engine.ErrUnsupportedNode) {
		t.Fatalf("err = %v, want ErrUnsupportedNode", err)
	}

	// Byte-identity: the refusal wrote NOTHING — no journal event, no projected
	// node. Assert the store is empty rather than trusting the claim.
	var journalRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows); err != nil {
		t.Fatalf("count journal rows: %v", err)
	}
	if journalRows != 0 {
		t.Errorf("journal rows = %d, want 0 (a refused do writes no journal)", journalRows)
	}
	var nodeRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes`).Scan(&nodeRows); err != nil {
		t.Fatalf("count node rows: %v", err)
	}
	if nodeRows != 0 {
		t.Errorf("node rows = %d, want 0 (a refused do projects no node)", nodeRows)
	}
}

// TestExecOnlyRunUnchangedWithHostPresent proves the effect events are additive:
// an exec-only formula never emits them, even when a host is configured — its
// journal vocabulary is byte-identical to the pre-P4.1 skeleton.
func TestExecOnlyRunUnchangedWithHostPresent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("execonly", execNode("greet", `echo hi`, nil)))
	stub := &enginehost.StubHost{} // present but unused: no do node

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// The exec-only event set is run.started, node.activated, outcome.settled,
	// run.closed — with neither effect event present.
	want := []string{engine.EventRunStarted, engine.EventNodeActivated, engine.EventOutcomeSettled, engine.EventRunClosed}
	got := eventTypes(res.Events)
	if !equalStrings(got, want) {
		t.Errorf("event types = %v, want %v (no effect events for exec-only)", got, want)
	}
	if contains(got, engine.EventEffectScheduled) || contains(got, engine.EventEffectSettled) {
		t.Errorf("exec-only run emitted effect events: %v", got)
	}
	if len(stub.Calls()) != 0 {
		t.Errorf("host was called %d times for an exec-only formula, want 0", len(stub.Calls()))
	}
}

func contains(s []string, want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}
