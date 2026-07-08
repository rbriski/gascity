package engine_test

// Crash-injection harness (blueprint §5, slice P4.4). It kills the Lumen executor
// at each decide -> persist -> act -> persist boundary via the in-process crash
// seam (engine.SetCrashHookForTest), then resumes over the surviving journal and
// proves the run converges (DET-T-1 diagonal) with honest effect semantics: a do
// effect is AT-MOST-once across a crash (host called ≤1), while an exec is
// AT-LEAST-once (it re-runs, carrying no effect record). The injection is
// deterministic (a sentinel error at the boundary + a StubHost) and runs under
// `go test -race` — no subprocess, no wall clock, no randomness beyond the
// engine's own stream nonce, which the harness reads back through the hook.
//
// COVERAGE: all eight declared crash boundaries (crash_seam.go) are exercised.
// The six per-node executor boundaries — before/after activate, before/after act,
// after-settle, and the snapshot pair — are driven by the DET-T-1 diagonal and the
// snapshot test. The two RUN-LEVEL boundaries are covered separately, because they
// have no per-node activation: after-run-started by TestCrashAfterRunStartedConverges,
// and before-run-closed (in its only on-disk-distinct form, with snapshotting
// enabled) by TestCrashBeforeRunClosedFromSealSnapshot. No declared boundary is
// left unexercised.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// reviewDoc is the representative DAG the diagonal crashes across: an exec root,
// an agent `do` leaf, a scatter/gather pair, and a failing exec that
// skip-cascades its dependent — every executor arm in one formula.
//
//	A (exec)  ──▶ D  (do, leaf)
//	          ──▶ S  (scatter[m1,m2]) ──▶ G (gather over S, combine: gc exec)
//	          ──▶ X  (exec, exit 1)   ──▶ Y (exec, skip-cascaded)
func reviewDoc(t *testing.T) *ir.IR {
	t.Helper()
	return decodeIR(t, blockDoc("review",
		execNode("A", `echo a`, nil),
		doNode("D", "Summarize {{A}}.", []string{"A"}),
		scatterNode("S", []string{"A"}, "continue", settleNode("m1", "pass"), settleNode("m2", "pass")),
		gatherNode("G", "S", []string{"S"}, execNode("gc", `echo g`, nil)),
		execNode("X", `exit 1`, []string{"A"}),
		execNode("Y", `echo y`, []string{"X"}),
	))
}

// passDoStub returns a fresh StubHost scripting each named do node to pass. A
// fresh instance per crash cell keeps its RunDo call count isolated so the
// at-most-once assertion counts only that cell's invocations.
func passDoStub(nodes ...string) *enginehost.StubHost {
	res := map[string]enginehost.DoResult{}
	for _, n := range nodes {
		res[n] = enginehost.DoResult{Outcome: enginehost.OutcomePass, Output: "summary:" + n}
	}
	return &enginehost.StubHost{Results: res}
}

// injectCrashThenResume runs doc under host until the crash seam fires at the
// first (boundary, activation) match — abandoning the run mid-cycle — then clears
// the seam and resumes over the surviving journal with the SAME host, so RunDo
// invocations accumulate across the crash and the resume. An empty activation
// matches the boundary regardless of activation (for snapshot / run-level
// boundaries whose label is the stream id). It fails the test if the boundary
// never fires (a misconfigured cell) or the resume errors. Returns the resumed
// result, its store, and the crashed stream id.
func injectCrashThenResume(t *testing.T, doc *ir.IR, host enginehost.AgentHost, boundary, activation string, snapEvery int) (engine.RunResult, *graphstore.Store, string) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	errCrash := errors.New("crash injected at " + boundary + " @ " + activation)
	var crashedStream string
	fired := false
	restore := engine.SetCrashHookForTest(func(b, streamID, act string) error {
		if b == boundary && (activation == "" || act == activation) && !fired {
			fired = true
			crashedStream = streamID
			return errCrash
		}
		return nil
	})

	_, runErr := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: host, SnapshotEvery: snapEvery})
	restore() // the seam MUST be clear before resume: resume runs the real cycle
	if engine.CrashHookInstalled() {
		t.Fatalf("crash seam still installed after restore — teardown leak")
	}
	if !errors.Is(runErr, errCrash) {
		t.Fatalf("crash at (%s,%s): run returned %v, want the injected sentinel (boundary never fired?)", boundary, activation, runErr)
	}
	if crashedStream == "" {
		t.Fatalf("crash at (%s,%s): no stream id captured through the hook", boundary, activation)
	}

	resumed, err := engine.Resume(ctx, store, doc, crashedStream, nil, engine.Options{Host: host, SnapshotEvery: snapEvery})
	if err != nil {
		t.Fatalf("crash at (%s,%s): resume: %v", boundary, activation, err)
	}
	return resumed, store, crashedStream
}

// TestCrashSeamInertByDefault proves the seam is a no-op in production: crashHook
// is nil unless a test installs it, a normal run is unaffected, and teardown
// leaves nothing installed. Together with the whole existing suite still passing,
// this is the "production is provably unchanged" proof.
func TestCrashSeamInertByDefault(t *testing.T) {
	ctx := context.Background()
	if engine.CrashHookInstalled() {
		t.Fatalf("crash seam installed at test start — production default must be nil (inert)")
	}

	store := newStore(t)
	doc := reviewDoc(t)
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: passDoStub("D")})
	if err != nil {
		t.Fatalf("normal run with no seam: %v", err)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify of an uninstrumented run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed { // X fails → run fails, unchanged by the seam
		t.Errorf("run outcome = %q, want failed (seam must not alter behavior)", res.Outcome)
	}
	if engine.CrashHookInstalled() {
		t.Fatalf("crash seam installed after a normal run — the seam leaked into production paths")
	}

	// SetCrashHookForTest(nil) is a no-op that keeps the seam inert, and restore
	// never installs a hook.
	restore := engine.SetCrashHookForTest(nil)
	if engine.CrashHookInstalled() {
		t.Fatalf("SetCrashHookForTest(nil) installed a hook")
	}
	restore()
	if engine.CrashHookInstalled() {
		t.Fatalf("restore installed a hook")
	}
}

// TestCrashDiagonalConvergesDETT1 is the DET-T-1 diagonal: for the representative
// DAG, a crash injected at EVERY executor boundary of EVERY node, followed by
// resume, converges to the canonical terminal — Verify passes, the frontier
// drains, no activation is left dangling, and the resumed projection is
// byte-identical to a clean genesis rebuild of the surviving journal. Every node
// converges to the genesis outcome EXCEPT the `do` at a scheduled-but-unsettled
// boundary (b/c), which settles FAILED under at-most-once (the honest cost of
// never re-running an effect a crash left mid-flight).
func TestCrashDiagonalConvergesDETT1(t *testing.T) {
	ctx := context.Background()
	doc := reviewDoc(t)

	// Genesis reference: the uninterrupted terminal the diagonal converges to.
	gStore := newStore(t)
	gRes, err := engine.RunWithOptions(ctx, gStore, doc, nil, engine.Options{Host: passDoStub("D")})
	if err != nil {
		t.Fatalf("genesis run: %v", err)
	}
	if err := gStore.Verify(ctx, gRes.StreamID); err != nil {
		t.Fatalf("genesis Verify: %v", err)
	}
	genesis := settledOutcomeByID(t, gRes.Events)
	for id, want := range map[string]string{
		"A": "pass", "D": "pass", "m1": "pass", "m2": "pass",
		"S": "pass", "G": "pass", "gc": "pass", "X": "failed", "Y": "skipped",
	} {
		if genesis[id] != want {
			t.Fatalf("genesis %s = %q, want %q — the representative formula regressed", id, genesis[id], want)
		}
	}
	if gRes.Outcome != engine.OutcomeFailed {
		t.Fatalf("genesis outcome = %q, want failed", gRes.Outcome)
	}

	cells := []struct {
		activation, boundary string
		doInterrupted        bool // the do's effect was scheduled but never settled
	}{
		{"A:0", engine.CrashBeforeActivate, false},
		{"A:0", engine.CrashBeforeAct, false},
		{"A:0", engine.CrashAfterAct, false},
		{"A:0", engine.CrashAfterSettle, false},
		{"D:0", engine.CrashBeforeActivate, false}, // not scheduled → resume runs it fresh
		{"D:0", engine.CrashBeforeAct, true},       // scheduled, not acted → interrupted
		{"D:0", engine.CrashAfterAct, true},        // acted once, not settled → interrupted
		{"D:0", engine.CrashAfterSettle, false},    // fully settled → reloaded
		{"m1:0", engine.CrashBeforeActivate, false},
		{"m1:0", engine.CrashAfterSettle, false},
		{"S:0", engine.CrashBeforeActivate, false},
		{"S:0", engine.CrashAfterSettle, false},
		{"gc:0", engine.CrashBeforeActivate, false},
		{"gc:0", engine.CrashBeforeAct, false},
		{"gc:0", engine.CrashAfterAct, false},
		{"gc:0", engine.CrashAfterSettle, false},
		{"G:0", engine.CrashBeforeActivate, false},
		{"G:0", engine.CrashAfterSettle, false},
		{"X:0", engine.CrashBeforeActivate, false},
		{"X:0", engine.CrashBeforeAct, false},
		{"X:0", engine.CrashAfterAct, false},
		{"X:0", engine.CrashAfterSettle, false},
		{"Y:0", engine.CrashBeforeActivate, false},
		{"Y:0", engine.CrashAfterSettle, false},
	}

	for _, c := range cells {
		t.Run(c.activation+"/"+c.boundary, func(t *testing.T) {
			stub := passDoStub("D")
			resumed, store, stream := injectCrashThenResume(t, doc, stub, c.boundary, c.activation, 0)

			// Convergence invariants that hold for every cell.
			if err := store.Verify(ctx, stream); err != nil {
				t.Fatalf("Verify after crash+resume: %v", err)
			}
			assertDrained(t, store, stream)
			assertNoDangling(t, resumed.Events)
			assertProjectionSelfConsistent(t, store, stream)
			if got := len(stub.Calls()); got > 1 {
				t.Errorf("do host called %d times across crash+resume — at-most-once VIOLATED", got)
			}

			// Convergence to the canonical terminal (genesis, with the at-most-once
			// do override at scheduled-but-unsettled boundaries).
			want := map[string]string{}
			for k, v := range genesis {
				want[k] = v
			}
			if c.doInterrupted {
				want["D"] = engine.OutcomeFailed
			}
			if got := settledOutcomeByID(t, resumed.Events); !mapEq(got, want) {
				t.Errorf("settlements after crash+resume = %v, want %v", got, want)
			}
			if resumed.Outcome != engine.OutcomeFailed {
				t.Errorf("run outcome = %q, want failed (X dominates the aggregate at every boundary)", resumed.Outcome)
			}
		})
	}
}

// TestCrashDoEffectAtMostOnce is the effect-semantics proof for `do`: across a
// crash at each boundary of its cycle, the agent host (a call-counting StubHost)
// is invoked AT MOST ONCE. The critical case is CrashAfterAct — the agent ran but
// its settlement never committed: resume settles the node FAILED without
// re-invoking the host, so the count stays at exactly 1 (never 2). A single
// effect.settled and a single outcome.settled survive — no re-acted duplicate.
func TestCrashDoEffectAtMostOnce(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("agentwork",
		execNode("A", `echo a`, nil),
		doNode("D", "Summarize {{A}}.", []string{"A"}),
	))

	cases := []struct {
		boundary string
		wantHost int    // total RunDo invocations across crash + resume
		wantD    string // D's settled outcome after resume
	}{
		{engine.CrashBeforeActivate, 1, engine.OutcomePass}, // not scheduled → resume runs it once
		{engine.CrashBeforeAct, 0, engine.OutcomeFailed},    // scheduled, never acted → 0 calls, interrupted
		{engine.CrashAfterAct, 1, engine.OutcomeFailed},     // acted once, not settled → 1 call, interrupted (the proof)
		{engine.CrashAfterSettle, 1, engine.OutcomePass},    // fully settled → reloaded, 1 call
	}
	for _, tc := range cases {
		t.Run(tc.boundary, func(t *testing.T) {
			stub := passDoStub("D")
			resumed, store, stream := injectCrashThenResume(t, doc, stub, tc.boundary, "D:0", 0)

			if got := len(stub.Calls()); got != tc.wantHost {
				t.Errorf("host RunDo calls = %d, want %d", got, tc.wantHost)
			}
			if got := len(stub.Calls()); got > 1 {
				t.Errorf("host called %d times — at-most-once VIOLATED (a do must never re-run across a crash)", got)
			}
			if got := settledOutcomeByID(t, resumed.Events)["D"]; got != tc.wantD {
				t.Errorf("D settled %q, want %q", got, tc.wantD)
			}

			full, err := store.ReadStream(ctx, stream, 1, 0)
			if err != nil {
				t.Fatalf("read stream: %v", err)
			}
			if n := countSettlements(t, full, "D:0"); n != 1 {
				t.Errorf("D settlements = %d, want exactly 1 (settled once across crash+resume)", n)
			}
			if got := effectSettledResults(t, full); len(got) != 1 {
				t.Errorf("effect.settled records = %v, want exactly 1 (no re-acted duplicate)", got)
			}
			if err := store.Verify(ctx, stream); err != nil {
				t.Errorf("Verify: %v", err)
			}
		})
	}
}

// TestCrashExecAtLeastOnceAcrossActBoundary documents the OTHER honest half of
// the effect contract: exec carries no effect record, so a crash after the shell
// ran but before its settlement (CrashAfterAct) re-runs the shell on resume — it
// is AT-LEAST-once, not at-most-once. A side-effect counter proves the shell ran
// TWICE, while the Lumen outcome still converges (a deterministic shell re-runs to
// the same result) and no effect.scheduled/settled pair exists for exec.
func TestCrashExecAtLeastOnceAcrossActBoundary(t *testing.T) {
	ctx := context.Background()
	counter := filepath.Join(t.TempDir(), "runs.log")
	script := `printf 'ran\n' >> ` + counter + `; echo done`
	doc := decodeIR(t, blockDoc("execwork", execNode("E", script, nil)))

	resumed, store, stream := injectCrashThenResume(t, doc, nil, engine.CrashAfterAct, "E:0", 0)

	if runs := countLines(t, counter); runs != 2 {
		t.Fatalf("exec ran %d time(s) across crash-at-act + resume, want 2 — exec is at-LEAST-once (no effect record to memoize)", runs)
	}
	if got := settledOutcomeByID(t, resumed.Events)["E"]; got != engine.OutcomePass {
		t.Errorf("E settled %q, want pass (the re-run converged to the same outcome)", got)
	}
	full, err := store.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	if got := effectSettledResults(t, full); len(got) != 0 {
		t.Errorf("exec wrote %d effect.settled record(s), want 0 — exec has no effect memoization, which is WHY it is at-least-once", len(got))
	}
	if n := countSettlements(t, full, "E:0"); n != 1 {
		t.Errorf("E outcome.settled count = %d, want 1 (re-run settled once)", n)
	}
	if err := store.Verify(ctx, stream); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// TestCrashSnapshotInteraction exercises the snapshot boundaries through the
// harness: a crash right AFTER a snapshot.anchored (resume loads from the anchor)
// and right BEFORE one (resume folds from genesis / the prior anchor) both
// converge to the uninterrupted terminal. Indivisibility is asserted directly —
// every persisted snapshot row hashes to its state_hash AND has a matching
// snapshot.anchored event, so no crash left a partial snapshot (the WriteSnapshot
// row+anchor transaction is all-or-nothing).
func TestCrashSnapshotInteraction(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"B"}),
	))

	// Uninterrupted, snapshotting reference terminal.
	refStore := newStore(t)
	ref, err := engine.RunWithOptions(ctx, refStore, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("reference run: %v", err)
	}
	wantSettled := settledOutcomeByID(t, ref.Events)

	for _, boundary := range []string{engine.CrashAfterSnapshot, engine.CrashBeforeSnapshot} {
		t.Run(boundary, func(t *testing.T) {
			// Empty activation: match the first time this snapshot boundary fires.
			resumed, store, stream := injectCrashThenResume(t, doc, nil, boundary, "", 1)

			if err := store.Verify(ctx, stream); err != nil {
				t.Fatalf("Verify after snapshot-crash+resume: %v", err)
			}
			assertDrained(t, store, stream)
			assertNoDangling(t, resumed.Events)
			assertProjectionSelfConsistent(t, store, stream)
			if got := settledOutcomeByID(t, resumed.Events); !mapEq(got, wantSettled) {
				t.Errorf("settlements after snapshot crash+resume = %v, want %v", got, wantSettled)
			}
			if resumed.Outcome != engine.OutcomePass {
				t.Errorf("run outcome = %q, want pass", resumed.Outcome)
			}
			assertSnapshotsWellFormed(t, store, stream)
		})
	}
}

// TestCrashAfterRunStartedConverges exercises the run-level after-run-started
// boundary: a crash the instant run.started commits, before the first unit
// activates. On disk this leaves the same journal ([run.started]) a
// before-activate crash of the FIRST node leaves, so it converges the same way;
// exercising it nonetheless proves the seam call at that boundary actually FIRES
// (injectCrashThenResume fails the test if it never does) and the label is live,
// not dead. Resume finds nothing settled, runs every unit fresh, and seals.
func TestCrashAfterRunStartedConverges(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, blockDoc("started",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
	))

	// Empty activation: match the first (only) fire of this run-level boundary.
	resumed, store, stream := injectCrashThenResume(t, doc, nil, engine.CrashAfterRunStarted, "", 0)

	if err := store.Verify(ctx, stream); err != nil {
		t.Fatalf("Verify after after-run-started crash+resume: %v", err)
	}
	assertDrained(t, store, stream)
	assertNoDangling(t, resumed.Events)
	assertProjectionSelfConsistent(t, store, stream)
	if n := countEventsOfType(resumed.Events, engine.EventRunClosed); n != 1 {
		t.Fatalf("run.closed count after resume = %d, want exactly 1 (resume must seal the run)", n)
	}
	want := map[string]string{"A": engine.OutcomePass, "B": engine.OutcomePass}
	if got := settledOutcomeByID(t, resumed.Events); !mapEq(got, want) {
		t.Errorf("settlements after after-run-started crash+resume = %v, want %v", got, want)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", resumed.Outcome)
	}
}

// TestCrashBeforeRunClosedFromSealSnapshot exercises the run-level
// before-run-closed boundary in its ONLY on-disk-distinct form: with
// snapshotting ENABLED. A crash after the seal snapshot anchors but before
// run.closed commits leaves a durable state no other boundary produces — the
// fully-settled pre-close state is memoized in the seal snapshot, run.closed is
// absent, and the frontier is drained. Resume must load the seal anchor, memoize
// every already-settled unit WITHOUT re-running it, and append only run.closed.
// It converges to the genesis terminal: run.closed present, Verify passes, the
// projection byte-identical to a clean genesis rebuild of the surviving journal,
// and the outcome matches genesis.
func TestCrashBeforeRunClosedFromSealSnapshot(t *testing.T) {
	ctx := context.Background()
	// Exec-only all-pass chain; snapshotting at every unit boundary means the seal
	// snapshot anchors the fully-settled state the crash strands unsealed.
	doc := decodeIR(t, blockDoc("seal",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"B"}),
	))

	// Genesis reference: an uninterrupted snapshotting run to the seal.
	gStore := newStore(t)
	gRes, err := engine.RunWithOptions(ctx, gStore, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("genesis run: %v", err)
	}
	if err := gStore.Verify(ctx, gRes.StreamID); err != nil {
		t.Fatalf("genesis Verify: %v", err)
	}
	wantSettled := settledOutcomeByID(t, gRes.Events)

	// Crash at before-run-closed WITH snapshotting (SnapshotEvery=1): the seal
	// snapshot is anchored, run.closed is not yet written. Empty activation matches
	// the first (only) fire of this run-level boundary.
	resumed, store, stream := injectCrashThenResume(t, doc, nil, engine.CrashBeforeRunClosed, "", 1)

	// The seal snapshot the crash left behind is the resume source: at least one
	// well-formed snapshot exists (blob hashes to its state_hash, matching anchor).
	if len(allSnapshots(t, store, stream)) == 0 {
		t.Fatalf("no snapshot anchored before the crash — the resume-from-seal path was not exercised")
	}
	assertSnapshotsWellFormed(t, store, stream)

	// Resume loaded the seal anchor and MEMOIZED every settled unit: each node
	// settled exactly once (before the crash) and was never re-run on resume — a
	// re-run would append a second outcome.settled.
	for _, act := range []string{"A:0", "B:0", "C:0"} {
		if n := countSettlements(t, resumed.Events, act); n != 1 {
			t.Errorf("%s settlements = %d, want exactly 1 (resume must memoize from the seal snapshot, not re-run)", act, n)
		}
	}
	// run.closed was ABSENT at the crash; resume appended it exactly once — the
	// distinct durable state of this boundary (work sealed, run not).
	if n := countEventsOfType(resumed.Events, engine.EventRunClosed); n != 1 {
		t.Fatalf("run.closed count after resume = %d, want exactly 1 (resume must seal a before-run-closed crash)", n)
	}

	// Convergence to the genesis terminal.
	if err := store.Verify(ctx, stream); err != nil {
		t.Fatalf("Verify after before-run-closed crash+resume: %v", err)
	}
	assertDrained(t, store, stream)
	assertNoDangling(t, resumed.Events)
	assertProjectionSelfConsistent(t, store, stream)
	if got := settledOutcomeByID(t, resumed.Events); !mapEq(got, wantSettled) {
		t.Errorf("settlements after before-run-closed crash+resume = %v, want %v", got, wantSettled)
	}
	if resumed.Outcome != gRes.Outcome {
		t.Errorf("run outcome = %q, want %q (genesis)", resumed.Outcome, gRes.Outcome)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", resumed.Outcome)
	}
}

// --- assertions -------------------------------------------------------------

// assertDrained fails unless the stream's frontier is empty — a converged run
// leaves no ready-but-unsettled activation.
func assertDrained(t *testing.T, store *graphstore.Store, stream string) {
	t.Helper()
	var rows int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM frontier WHERE root_id = ?`, stream).Scan(&rows); err != nil {
		t.Fatalf("count frontier: %v", err)
	}
	if rows != 0 {
		t.Errorf("frontier rows after resume = %d, want 0 (frontier must drain)", rows)
	}
}

// assertNoDangling fails if any activated node was never settled, or any
// scheduled effect was never settled — the "no lost/dangling state" invariant.
func assertNoDangling(t *testing.T, events []graphstore.StoredEvent) {
	t.Helper()
	activated := map[string]bool{}
	settled := map[string]bool{}
	scheduled := map[string]bool{}
	effSettled := map[string]bool{}
	for _, e := range events {
		switch e.Type {
		case engine.EventNodeActivated:
			var p struct {
				Activation string `json:"activation"`
			}
			mustJSON(t, e.Payload, &p)
			activated[p.Activation] = true
		case engine.EventOutcomeSettled:
			var p struct {
				Activation string `json:"activation"`
			}
			mustJSON(t, e.Payload, &p)
			settled[p.Activation] = true
		case engine.EventEffectScheduled:
			var p struct {
				IdemToken string `json:"idem_token"`
			}
			mustJSON(t, e.Payload, &p)
			scheduled[p.IdemToken] = true
		case engine.EventEffectSettled:
			var p struct {
				IdemToken string `json:"idem_token"`
			}
			mustJSON(t, e.Payload, &p)
			effSettled[p.IdemToken] = true
		}
	}
	for a := range activated {
		if !settled[a] {
			t.Errorf("dangling activation: %q was activated but never settled", a)
		}
	}
	for tok := range scheduled {
		if !effSettled[tok] {
			t.Errorf("dangling effect: %q was scheduled but never settled", tok)
		}
	}
}

// assertProjectionSelfConsistent proves the resumed Tier-A projection is
// byte-identical to a clean genesis rebuild of the surviving journal (DET-T-17) —
// the projection is a pure function of the log, so a crash/resume left no drift.
func assertProjectionSelfConsistent(t *testing.T, store *graphstore.Store, stream string) {
	t.Helper()
	ctx := context.Background()
	live := dumpTierA(t, store, stream)
	if err := store.RebuildTierA(ctx, engine.Reducer(), stream); err != nil {
		t.Fatalf("RebuildTierA: %v", err)
	}
	if rebuilt := dumpTierA(t, store, stream); live != rebuilt {
		t.Errorf("resumed projection not byte-identical to genesis rebuild:\n--- live ---\n%s\n--- rebuilt ---\n%s", live, rebuilt)
	}
}

// assertSnapshotsWellFormed proves no crash left a partial snapshot: every
// persisted snapshot row hashes to its stored state_hash and has a matching
// snapshot.anchored event at covered_seq+1 (the row+anchor transaction is
// indivisible).
func assertSnapshotsWellFormed(t *testing.T, store *graphstore.Store, stream string) {
	t.Helper()
	ctx := context.Background()
	rows := allSnapshots(t, store, stream)
	if len(rows) == 0 {
		return // a before-snapshot crash may leave none; that is still consistent
	}
	events, err := store.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	anchorAt := map[uint64]string{} // covered_seq -> anchored state_hash (hex)
	for _, e := range events {
		if e.Type != engine.EventSnapshotAnchored {
			continue
		}
		var p struct {
			CoveredSeq uint64 `json:"covered_seq"`
			StateHash  string `json:"state_hash"`
		}
		mustJSON(t, e.Payload, &p)
		anchorAt[p.CoveredSeq] = p.StateHash
	}
	for _, r := range rows {
		if canon.Hash(r.state) != r.stateHash {
			t.Errorf("snapshot@%d: blob does not hash to its stored state_hash (partial/corrupt snapshot)", r.covered)
		}
		if _, ok := anchorAt[r.covered]; !ok {
			t.Errorf("snapshot@%d: no matching snapshot.anchored event (row without anchor — not indivisible)", r.covered)
		}
	}
}

// --- small helpers ----------------------------------------------------------

// countEventsOfType counts committed events of a given type in a stream slice.
func countEventsOfType(events []graphstore.StoredEvent, eventType string) int {
	n := 0
	for _, e := range events {
		if e.Type == eventType {
			n++
		}
	}
	return n
}

// mapEq reports whether two string maps are equal.
func mapEq(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// countLines counts newline-terminated lines in a file, returning 0 when it does
// not exist (the shell never ran).
func countLines(t *testing.T, path string) int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0
		}
		t.Fatalf("read %s: %v", path, err)
	}
	return strings.Count(string(b), "\n")
}

// mustJSON unmarshals payload into v, failing the test on error.
func mustJSON(t *testing.T, payload []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(payload, v); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
}
