package engine_test

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// driveStepperUntilCrash drives a stepper run (Step, then Settle per offered do from the
// script) until the injected crash sentinel fires at some boundary, then returns. It
// fails the test if the run seals with no crash (a misconfigured boundary cell).
func driveStepperUntilCrash(t *testing.T, store *graphstore.Store, doc *ir.IR, streamID string, script stepScript, errCrash error) {
	t.Helper()
	ctx := context.Background()
	res, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if errors.Is(err, errCrash) {
		return
	}
	if err != nil {
		t.Fatalf("step before crash: %v", err)
	}
	guard := 0
	for !res.Done {
		guard++
		if guard > 100 {
			t.Fatalf("no crash fired after 100 turns")
		}
		oc := script[res.NodeID]
		next, serr := engine.Settle(ctx, store, doc, streamID, nil, res.NodeID, oc[0], oc[1], engine.Options{})
		if errors.Is(serr, errCrash) {
			return
		}
		if serr != nil {
			t.Fatalf("settle %q before crash: %v", res.NodeID, serr)
		}
		res = next
	}
	t.Fatalf("run sealed with no crash injected")
}

// resumeStepperToSeal resumes a crashed stepper run over its surviving journal (a fresh
// Step + Settle loop, the ordinary re-claim path) and drives it to run.closed. It returns
// the first Step's result (so a caller can assert whether the interrupted do was
// re-offered or fail-cascaded) plus the sealed final result.
func resumeStepperToSeal(t *testing.T, store *graphstore.Store, doc *ir.IR, streamID string, script stepScript) (firstStep, final engine.StepResult) {
	t.Helper()
	ctx := context.Background()
	res, err := engine.Step(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("resume step: %v", err)
	}
	firstStep = res
	guard := 0
	for !res.Done {
		guard++
		if guard > 100 {
			t.Fatalf("resume did not seal after 100 turns")
		}
		oc, ok := script[res.NodeID]
		if !ok {
			t.Fatalf("resume offered unscripted do %q", res.NodeID)
		}
		res, err = engine.Settle(ctx, store, doc, streamID, nil, res.NodeID, oc[0], oc[1], engine.Options{})
		if err != nil {
			t.Fatalf("resume settle %q: %v", res.NodeID, err)
		}
	}
	return firstStep, res
}

// TestStepperCrashInSettleFailsAtMostOnce is the at-most-once crash pin: a crash injected
// DURING Settle (after effect.scheduled, before outcome.settled — the crashBeforeAct /
// crashAfterAct window) settles the interrupted do FAILED on re-claim WITHOUT re-offering
// it to the agent (no re-act), and the fail cascades to the dependent. This is the
// engine's existing effect at-most-once contract, inherited unchanged: the surviving
// journal verifies and folds byte-identically to a drop+refold.
func TestStepperCrashInSettleFailsAtMostOnce(t *testing.T) {
	for _, boundary := range []string{engine.CrashBeforeAct, engine.CrashAfterAct} {
		boundary := boundary
		t.Run(boundary, func(t *testing.T) {
			ctx := context.Background()
			doc := linear3DoDoc(t)
			store := newStore(t)
			streamID := enqueueV1(t, store, doc)
			script := stepScript{
				"a": {engine.OutcomePass, "out-a"},
				"b": {engine.OutcomePass, "out-b"},
				"c": {engine.OutcomePass, "out-c"},
			}

			errCrash := errors.New("crash at " + boundary)
			fired := false
			restore := engine.SetCrashHookForTest(func(b, _, act string) error {
				if b == boundary && act == "b:0" && !fired {
					fired = true
					return errCrash
				}
				return nil
			})
			driveStepperUntilCrash(t, store, doc, streamID, script, errCrash)
			restore()
			if !fired {
				t.Fatalf("crash boundary %s never fired", boundary)
			}

			firstStep, final := resumeStepperToSeal(t, store, doc, streamID, script)
			// No re-act: the interrupted do b is NOT re-offered — the resume goes straight to
			// the fail-cascade seal (b failed → c skipped → run failed).
			if !firstStep.Done {
				t.Fatalf("resume re-offered do %q — an at-most-once interrupted do must NOT re-act", firstStep.NodeID)
			}
			if final.Outcome != engine.OutcomeFailed {
				t.Fatalf("run outcome = %q, want failed (b interrupted → failed → cascade)", final.Outcome)
			}
			assertStepperSettled(t, store, streamID, map[string]string{"a": "pass", "b": "failed", "c": "skipped"})
			if err := store.Verify(ctx, streamID); err != nil {
				t.Fatalf("Verify surviving journal: %v", err)
			}
			assertProjectionEqualsRefold(t, store, streamID)
		})
	}
}

// TestStepperCrashAfterSettleReloads is the crashAfterSettle pin: a crash injected after
// outcome.settled committed leaves the do fully settled on disk; the resume reloads it via
// the memoization and never re-runs it. b converges to its recorded PASS (not failed), and
// the run completes normally.
func TestStepperCrashAfterSettleReloads(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)
	script := stepScript{
		"a": {engine.OutcomePass, "out-a"},
		"b": {engine.OutcomePass, "out-b"},
		"c": {engine.OutcomePass, "out-c"},
	}

	errCrash := errors.New("crash after settle")
	fired := false
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterSettle && act == "b:0" && !fired {
			fired = true
			return errCrash
		}
		return nil
	})
	driveStepperUntilCrash(t, store, doc, streamID, script, errCrash)
	restore()
	if !fired {
		t.Fatal("crashAfterSettle never fired")
	}

	_, final := resumeStepperToSeal(t, store, doc, streamID, script)
	if final.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (b settled pass before the crash, reloaded)", final.Outcome)
	}
	assertStepperSettled(t, store, streamID, map[string]string{"a": "pass", "b": "pass", "c": "pass"})
	// b settled exactly once — the crash-after-settle reload never re-appends its outcome.
	if n := countJournalType(t, store, streamID, engine.EventOutcomeSettled); n != 3 {
		t.Fatalf("outcome.settled count = %d, want 3 (each do once, b never re-run)", n)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// TestStepperCrashBetweenStepAndSettleReoffers is the honest step→Settle window: a crash
// after Step activated the do but before Settle wrote any effect (the crashAfterActivate
// arm) RE-OFFERS the same do on re-claim — at-LEAST-once for the agent's off-journal work,
// consistent with idempotent re-Step. The do then completes normally.
func TestStepperCrashBetweenStepAndSettleReoffers(t *testing.T) {
	ctx := context.Background()
	doc := linear3DoDoc(t)
	store := newStore(t)
	streamID := enqueueV1(t, store, doc)
	script := stepScript{
		"a": {engine.OutcomePass, "out-a"},
		"b": {engine.OutcomePass, "out-b"},
		"c": {engine.OutcomePass, "out-c"},
	}

	errCrash := errors.New("crash after activate")
	fired := false
	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterActivate && act == "b:0" && !fired {
			fired = true
			return errCrash
		}
		return nil
	})
	driveStepperUntilCrash(t, store, doc, streamID, script, errCrash)
	restore()
	if !fired {
		t.Fatal("crashAfterActivate never fired")
	}

	firstStep, final := resumeStepperToSeal(t, store, doc, streamID, script)
	// Re-offer: the interrupted do b IS handed back to the agent (at-least-once).
	if firstStep.Done || firstStep.NodeID != "b" {
		t.Fatalf("resume first step = %+v, want the re-offered do b (at-least-once)", firstStep)
	}
	if final.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", final.Outcome)
	}
	assertStepperSettled(t, store, streamID, map[string]string{"a": "pass", "b": "pass", "c": "pass"})
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
	assertProjectionEqualsRefold(t, store, streamID)
}

// assertStepperSettled asserts the settled outcome per bare node id in the journal.
func assertStepperSettled(t *testing.T, store *graphstore.Store, streamID string, want map[string]string) {
	t.Helper()
	events, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	got := map[string]string{}
	for _, kv := range settledIDs(t, events) {
		got[kv[0]] = kv[1]
	}
	for id, w := range want {
		if got[id] != w {
			t.Fatalf("settled[%s] = %q, want %q (all: %v)", id, got[id], w, got)
		}
	}
}
