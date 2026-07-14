package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestFoldRunViewProjectsSealedRun folds a two-node chain (A→B) that has run to
// seal and asserts the RunView carries the run identity, both settled
// activations, the A→B dependency, and the terminal outcome. This is the
// per-run, collision-free topology source P5-OBS.4 projects into the dashboard —
// the Tier-A nodes/edges tables cannot supply it (their ids are IR-local and
// last-writer-wins across runs, reducer.go's L4-BLOCKER).
func TestFoldRunViewProjectsSealedRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-run-view-chain"
	doc := decodeIR(t, blockDoc("chain",
		doNode("A", "Produce a value.", nil),
		doNode("B", "Refine {{A}}.", []string{"A"}),
	))
	opts := newFakeWorkStore().opts()

	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if err := engine.SettleWorkForTest(ctx, store, streamID, "A:0", engine.OutcomePass, "raw"); err != nil {
		t.Fatalf("settle A: %v", err)
	}
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if err := engine.SettleWorkForTest(ctx, store, streamID, "B:0", engine.OutcomePass, "refined"); err != nil {
		t.Fatalf("settle B: %v", err)
	}
	r3, err := engine.Advance(ctx, store, doc, streamID, nil, opts)
	if err != nil || !r3.Sealed {
		t.Fatalf("advance 3 = %+v, err %v; want Sealed", r3, err)
	}

	view, err := engine.FoldRunView(ctx, store, streamID)
	if err != nil {
		t.Fatalf("FoldRunView: %v", err)
	}
	if view.RootID != streamID {
		t.Fatalf("RootID = %q, want %q", view.RootID, streamID)
	}
	if !view.Closed {
		t.Fatalf("Closed = false, want true (run sealed)")
	}
	if view.Outcome != engine.OutcomePass {
		t.Fatalf("Outcome = %q, want %q", view.Outcome, engine.OutcomePass)
	}
	if len(view.Activations) != 2 {
		t.Fatalf("Activations = %d (%+v), want 2 (A, B)", len(view.Activations), view.Activations)
	}

	byNode := map[string]engine.RunActivationView{}
	for _, a := range view.Activations {
		byNode[a.NodeID] = a
	}
	a, okA := byNode["A"]
	b, okB := byNode["B"]
	if !okA || !okB {
		t.Fatalf("want activations for A and B, got %+v", view.Activations)
	}
	if !a.Settled || a.Outcome != engine.OutcomePass {
		t.Fatalf("A settled=%v outcome=%q, want settled pass", a.Settled, a.Outcome)
	}
	if a.Attempt != 0 {
		t.Fatalf("A attempt = %d, want 0", a.Attempt)
	}
	// B depends on A: A's activation key appears in B.After.
	foundDep := false
	for _, dep := range b.After {
		if engine.ActivationNodeID(dep) == "A" {
			foundDep = true
		}
	}
	if !foundDep {
		t.Fatalf("B.After = %+v, want a dependency on A", b.After)
	}
}

// TestFoldRunViewIsPerRunIndependent is the regression the Tier-A read path
// fails: two runs of the SAME formula (identical IR-local node ids) in distinct
// streams project INDEPENDENT views. Tier-A's global-PK nodes rows would have
// run 2 clobber run 1's rows (stream_id last-writer-wins, projection.go:399); a
// journal-stream fold is per-run and immutable, so each view stands alone.
func TestFoldRunViewIsPerRunIndependent(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("chain", doNode("A", "Do A.", nil)))
	opts := newFakeWorkStore().opts()

	run := func(streamID, output string) engine.RunView {
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
			t.Fatalf("advance %s: %v", streamID, err)
		}
		if err := engine.SettleWorkForTest(ctx, store, streamID, "A:0", engine.OutcomePass, output); err != nil {
			t.Fatalf("settle %s: %v", streamID, err)
		}
		if _, err := engine.Advance(ctx, store, doc, streamID, nil, opts); err != nil {
			t.Fatalf("seal %s: %v", streamID, err)
		}
		v, err := engine.FoldRunView(ctx, store, streamID)
		if err != nil {
			t.Fatalf("FoldRunView %s: %v", streamID, err)
		}
		return v
	}

	run("gcg-run-view-r1", "first")
	run("gcg-run-view-r2", "second")

	// THE REGRESSION: re-fold run 1 AFTER run 2 has run. Both runs share the
	// same IR-local node ids, so a Tier-A `nodes WHERE stream_id=?` read would
	// now return only run 1's ROOT (run 2 stole the step rows via the
	// last-writer-wins stream_id upsert). The journal-stream fold is immune — run
	// 1's stream still carries its own settled activation.
	v1, err := engine.FoldRunView(ctx, store, "gcg-run-view-r1")
	if err != nil {
		t.Fatalf("re-fold run 1 after run 2: %v", err)
	}
	if v1.RootID != "gcg-run-view-r1" {
		t.Fatalf("re-folded run 1 root = %q, want gcg-run-view-r1", v1.RootID)
	}
	if len(v1.Activations) != 1 || v1.Activations[0].NodeID != "A" || !v1.Activations[0].Settled {
		t.Fatalf("re-folded run 1 lost its settled activation A (Tier-A clobber would): %+v", v1.Activations)
	}
	if !v1.Closed {
		t.Fatalf("re-folded run 1 Closed = false, want true")
	}
}

// TestFoldRunViewRejectsEmptyStream proves the loud-fail contract: no journal
// stream (or one without run.started) is an error, never a silent empty view.
func TestFoldRunViewRejectsEmptyStream(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	if _, err := engine.FoldRunView(ctx, store, "gcg-run-view-absent"); err == nil {
		t.Fatal("FoldRunView on an absent stream = nil error, want a loud failure")
	}
}
