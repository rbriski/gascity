package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// cleanupNode renders a cleanup(try/finally) node with id "clean": a `guarded` leaf
// runs, then the `body` (finally) leaf runs ALWAYS.
func cleanupNode(after []string, guarded, body string) string {
	afterJSON := "[]"
	if len(after) > 0 {
		afterJSON = `["` + strings.Join(after, `","`) + `"]`
	}
	return `{"kind":"cleanup","id":"clean","name":"clean","after":` + afterJSON + `,` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"guarded":` + guarded + `,"body":` + body + `}`
}

// TestCleanupGuardPassBodyPass proves a cleanup whose guarded and body both pass
// settles pass, both sub-steps ran, and the cleanup output is the guarded's.
func TestCleanupGuardPassBodyPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cu1",
		cleanupNode(nil, execNode("g", `echo G`, nil), execNode("b", `echo B`, nil)),
		execNode("after", `echo "c={{ clean }}"`, []string{"clean"}),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["g"]; got != "G" {
		t.Errorf("guarded output = %q, want G", got)
	}
	if got := res.NodeOutputs["b"]; got != "B" {
		t.Errorf("body output = %q, want B (finally ran)", got)
	}
	if got := res.NodeOutputs["clean"]; got != "G" {
		t.Errorf("cleanup output = %q, want G (transparent from guarded)", got)
	}
	if got := res.NodeOutputs["after"]; got != "c=G" {
		t.Errorf("downstream = %q, want c=G (guarded output plumbed through cleanup)", got)
	}
}

// TestCleanupBodyRunsWhenGuardedFailed is the always-run proof: the finally body runs
// EVEN when the guarded step failed, and the cleanup settles failed (guarded's error
// propagates through a passing finally).
func TestCleanupBodyRunsWhenGuardedFailed(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cu2",
		cleanupNode(nil, execNode("g", `exit 1`, nil), execNode("b", `echo teardown`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "g", engine.OutcomeFailed)
	assertSettled(t, settled, "b", engine.OutcomePass) // finally ran despite the guarded failure
	assertSettled(t, settled, "clean", engine.OutcomeFailed)
	if got := res.NodeOutputs["b"]; got != "teardown" {
		t.Errorf("body output = %q, want teardown (always-run finally)", got)
	}
}

// TestCleanupFinallyFailureWins proves a failing finally overrides a passing guarded
// (the finally's error supersedes).
func TestCleanupFinallyFailureWins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cu3",
		cleanupNode(nil, execNode("g", `echo ok`, nil), settleNode("b", "failed")),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "g", engine.OutcomePass)
	assertSettled(t, settled, "clean", engine.OutcomeFailed) // finally failure wins over a passing guarded
}

// TestCleanupSkipCascade proves a cleanup gated on a failed `after` dep settles skipped
// and runs NEITHER sub-step.
func TestCleanupSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cu4",
		execNode("gate", `exit 1`, nil),
		cleanupNode([]string{"gate"}, execNode("g", `echo G`, nil), execNode("b", `echo B`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "clean", engine.OutcomeSkipped)
	if got := res.NodeOutputs["g"]; got != "" {
		t.Errorf("guarded output = %q, want empty (skip-cascaded cleanup runs nothing)", got)
	}
	if got := res.NodeOutputs["b"]; got != "" {
		t.Errorf("body output = %q, want empty (skip-cascaded cleanup runs nothing)", got)
	}
}

// TestCleanupDropRefoldByteIdentity pins DET for a cleanup (guarded fail + body pass).
func TestCleanupDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("cu5",
		cleanupNode(nil, execNode("g", `exit 1`, nil), execNode("b", `echo teardown`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestCleanupLoweringRefusals pins the refused shapes: recover (deferred), a non-leaf
// guarded/body, a missing guarded/body, a delimiter-bearing sub id, a sub-formula /
// in-aggregate placement.
func TestCleanupLoweringRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		nodes []string
	}{
		{
			name:  "non-leaf guarded",
			nodes: []string{cleanupNode(nil, scatterNode("s", nil, "continue", execNode("x", `echo 1`, nil)), execNode("b", `echo 1`, nil))},
		},
		{
			name: "missing guarded",
			nodes: []string{`{"kind":"cleanup","id":"clean","name":"clean","after":[],` +
				`"origin":{"uri":"t","line":1,"col":0},"body":` + execNode("b", `echo 1`, nil) + `}`},
		},
		{
			name: "missing body",
			nodes: []string{`{"kind":"cleanup","id":"clean","name":"clean","after":[],` +
				`"origin":{"uri":"t","line":1,"col":0},"guarded":` + execNode("g", `echo 1`, nil) + `}`},
		},
		{
			name:  "nested in an aggregate",
			nodes: []string{scatterNode("outer", nil, "continue", cleanupNode(nil, execNode("g", `echo 1`, nil), execNode("b", `echo 1`, nil)))},
		},
		{
			name:  "sub carries an inner after gate",
			nodes: []string{cleanupNode(nil, execNode("g", `echo 1`, []string{"x"}), execNode("b", `echo 1`, nil))},
		},
		{
			name:  "sub id with reserved delimiter",
			nodes: []string{cleanupNode(nil, execNode("a/b", `echo 1`, nil), execNode("b", `echo 1`, nil))},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("curf", tc.nodes...))
			_, err := engine.Run(ctx, newStore(t), doc, nil)
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("run err = %v, want ErrUnsupportedNode", err)
			}
		})
	}
}

// TestCleanupSubIdCollisionRefused pins the arch-review blocker: a guarded and body that
// share an id (or collide with the cleanup / another node) would share one activation —
// the second would resume-memoize the first's settled outcome and never run, silently
// defeating the always-run finally. Refused loudly at load (a plain collision error,
// like the decision arms' synth-id guard).
func TestCleanupSubIdCollisionRefused(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		nodes []string
	}{
		{
			name:  "guarded id equals body id",
			nodes: []string{cleanupNode(nil, execNode("same", `echo g`, nil), execNode("same", `echo b`, nil))},
		},
		{
			name:  "guarded id equals cleanup id",
			nodes: []string{cleanupNode(nil, execNode("clean", `echo g`, nil), execNode("b", `echo b`, nil))},
		},
		{
			name: "body id equals a sibling node id",
			nodes: []string{
				execNode("sib", `echo sib`, nil),
				cleanupNode(nil, execNode("g", `echo g`, nil), execNode("sib", `echo b`, nil)),
			},
		},
		{
			// A cleanup sub id aliasing a retry loop's body id: both drive fold node
			// "loopbody:0" (attempt 0 == activationFor), so the finally would silently
			// never run (red-team CONFIRMED). Refused at load.
			name: "body id equals a loop body id",
			nodes: []string{
				retryLane("rt", "loopbody", "2", "echo loop", nil),
				cleanupNode(nil, execNode("g", `echo g`, nil), execNode("loopbody", `echo b`, nil)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("cucol", tc.nodes...))
			_, err := engine.Run(ctx, newStore(t), doc, nil)
			if err == nil {
				t.Fatalf("run err = nil, want a collision refusal")
			}
		})
	}
}

// --- Advance (pool) ---

// TestAdvanceCleanupGuardedThenBodyParks proves a pool-do cleanup drives its guarded do
// first (parking), then — after it closes — its body do (parking), then seals.
func TestAdvanceCleanupGuardedThenBodyParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acu",
		cleanupNode(nil, doNode("g", "do the work", nil), doNode("b", "tear down", nil)),
	))
	opts := fake.opts()

	// Pass 1: guarded materialized, parked.
	r1, err := engine.Advance(ctx, store, doc, "gcg-run-cu", nil, opts)
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed || len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "g" {
		t.Fatalf("advance 1 = %+v, want parked on guarded g", r1)
	}

	// Guarded closes; pass 2 dispatches the body.
	fake.settleAct(t, "g:0", engine.OutcomePass, "did work")
	r2, err := engine.Advance(ctx, store, doc, "gcg-run-cu", nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if r2.Sealed || len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "b" {
		t.Fatalf("advance 2 = %+v, want parked on body b (finally)", r2)
	}

	// Body closes; pass 3 seals.
	fake.settleAct(t, "b:0", engine.OutcomePass, "torn down")
	r3, err := engine.Advance(ctx, store, doc, "gcg-run-cu", nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if !r3.Sealed {
		t.Fatalf("advance 3 = %+v, want Sealed", r3)
	}
	settled := settledIDs(t, r3.Run.Events)
	assertSettled(t, settled, "clean", engine.OutcomePass)
}

// TestAdvanceCleanupBodyRunsAfterGuardedFail proves the finally body is dispatched even
// when the guarded do closed FAILED, and the cleanup seals failed.
func TestAdvanceCleanupBodyRunsAfterGuardedFail(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("acuf",
		cleanupNode(nil, doNode("g", "do the work", nil), doNode("b", "tear down", nil)),
	))
	opts := fake.opts()

	if _, err := engine.Advance(ctx, store, doc, "gcg-run-cuf", nil, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settleAct(t, "g:0", engine.OutcomeFailed, "work failed")
	r2, err := engine.Advance(ctx, store, doc, "gcg-run-cuf", nil, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if len(r2.InFlight) != 1 || r2.InFlight[0].NodeID != "b" {
		t.Fatalf("advance 2 = %+v, want the body dispatched despite the guarded failure", r2)
	}
	fake.settleAct(t, "b:0", engine.OutcomePass, "torn down")
	r3, err := engine.Advance(ctx, store, doc, "gcg-run-cuf", nil, opts)
	if err != nil {
		t.Fatalf("advance 3: %v", err)
	}
	if !r3.Sealed {
		t.Fatalf("advance 3 = %+v, want Sealed", r3)
	}
	settled := settledIDs(t, r3.Run.Events)
	assertSettled(t, settled, "clean", engine.OutcomeFailed) // guarded's failure propagates through a passing finally
}
