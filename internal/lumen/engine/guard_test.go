package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// guardExec renders a top-level guard "g" with cond `mode == "go"` and an exec then
// "gthen" running thenScript. The go/stop decision is driven by the run input `mode`.
func guardExec(thenScript string) string {
	cond := `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"mode"},{"kind":"literal","value":"go"}]}`
	return `{"kind":"guard","id":"g","name":"g","after":[],"cond":` + cond + `,"then":` + execNode("gthen", thenScript, nil) + `}`
}

// guardDo renders a guard whose then is a do (pool-materializable).
func guardDo(after []string, condRef, condLit, thenID, thenPrompt string) string {
	a, _ := json.Marshal(after)
	cond := `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"` + condRef + `"},` +
		`{"kind":"literal","value":"` + condLit + `"}]}`
	return `{"kind":"guard","id":"g","name":"g","after":` + string(a) +
		`,"cond":` + cond + `,"then":` + doNode(thenID, thenPrompt, nil) + `}`
}

// TestAdvanceGuardCondFalseSeals proves a guard whose cond is false settles pass and
// the run seals in one Advance pass (no then materialized).
func TestAdvanceGuardCondFalseSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("agf", guardDo(nil, "mode", "go", "gthen", "do the thing")))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-guardfalse", map[string]any{"mode": "stop"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed || res.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance = %+v, want Sealed pass (false guard is a no-op)", res)
	}
}

// TestAdvanceGuardDoThenParks proves a guard whose cond is true and whose then is a
// do materializes the then as pool work and PARKS on it (the do is dispatched, not
// run inline).
func TestAdvanceGuardDoThenParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("agt", guardDo(nil, "mode", "go", "gthen", "do the thing")))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-guardtrue", map[string]any{"mode": "go"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (the do then is dispatched, awaited)", res)
	}
	if len(res.InFlight) != 1 || res.InFlight[0].NodeID != "gthen" {
		t.Fatalf("InFlight = %+v, want the gthen do materialized as pool work", res.InFlight)
	}
}

// TestGuardCondTrueRunsThen proves a guard whose cond is truthy runs its then and
// settles from it (pass), plumbing the then's output to a downstream {{ref}}.
func TestGuardCondTrueRunsThen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("gt",
		guardExec(`echo "ran it"`),
		execNode("done", `echo "after: {{ g }}"`, []string{"g"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"mode": "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["gthen"]; got != "ran it" {
		t.Errorf("then output = %q, want %q (then ran)", got, "ran it")
	}
	if got := res.NodeOutputs["g"]; got != "ran it" {
		t.Errorf("guard output = %q, want %q (transparent from then)", got, "ran it")
	}
	if got := res.NodeOutputs["done"]; got != "after: ran it" {
		t.Errorf("downstream = %q, want %q (guard output plumbed)", got, "after: ran it")
	}
}

// TestGuardCondFalseIsPassNoOp proves a guard whose cond is falsy settles PASS
// WITHOUT running its then and does NOT skip-cascade its dependents (a conditional
// step that legitimately did not run — downstream proceeds).
func TestGuardCondFalseIsPassNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("gf",
		guardExec(`echo "ran it"`),
		execNode("done", `echo "after: {{ g }}"`, []string{"g"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"mode": "stop"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (a false guard passes)", res.Outcome)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "g", engine.OutcomePass)
	// The then never ran (no output) and never settled.
	if got := res.NodeOutputs["gthen"]; got != "" {
		t.Errorf("then output = %q, want empty (then must not run when cond is false)", got)
	}
	// Downstream ran (NOT skip-cascaded); the guard output is empty.
	assertSettled(t, settled, "done", engine.OutcomePass)
	if got := res.NodeOutputs["done"]; got != "after: " {
		t.Errorf("downstream = %q, want %q (guard output empty, downstream still ran)", got, "after: ")
	}
}

// TestGuardDropRefoldByteIdentity pins DET for a guard: the live projection (guard +
// its then node) equals a from-scratch drop+refold — reducer folds no hidden state.
func TestGuardDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"go", "stop"} { // cond true (then runs) + cond false (no-op)
		store := newStore(t)
		doc := decodeIR(t, blockDoc("gd",
			guardExec(`echo hi`),
			execNode("done", `echo "d {{ g }}"`, []string{"g"}),
		))
		res, err := engine.Run(ctx, store, doc, map[string]any{"mode": mode})
		if err != nil {
			t.Fatalf("run(mode=%s): %v", mode, err)
		}
		assertProjectionEqualsRefold(t, store, res.StreamID)
	}
}

// TestGuardThenFailSkipCascades proves a guard whose then FAILS settles failed and
// skip-cascades its dependents.
func TestGuardThenFailSkipCascades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("gx",
		guardExec(`exit 2`),
		execNode("done", `echo after`, []string{"g"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"mode": "go"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Fatalf("run outcome = %q, want failed", res.Outcome)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "g", engine.OutcomeFailed)
	assertSettled(t, settled, "done", engine.OutcomeSkipped)
}
