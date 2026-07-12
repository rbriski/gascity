//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func loopInSubIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "repeat-run-in-sub.lumen.json")
}

// TestLumenLoopInSubDoltE2E_ReMintsAtDepth (LIS marquee, §3) proves a repeat run-body loop
// INSIDE a run sub-formula re-mints its sub-graph under a FRESH depth namespace across
// attempts on a real Dolt city: main `run wrapper -> reviewWrapper{ repeat rounds { run
// round -> reviewRound{ do review } } until round.outcome == pass || iteration >=
// max_review_rounds }`, with the budget (max_review_rounds = 12) bound via the wrapper's
// env as a TYPED number sub input and the sub-do's prompt binding name <- who. A
// fail-once-then-pass worker fails attempt 0's sub-do at the DEPTH prefix
// wrapper/round/0/review:0; the attempt aggregate settles failed; the cond (evaluated over
// the namespace-local loop scope — numeric, not lexicographic) re-mints attempt 1 under
// wrapper/round/1/ with a FRESH work bead; it passes; the aggregate settles pass; the cond
// exits and the loop settles pass -> seal.
//
// It pins: (1) run.closed pass; (2) BOTH attempt sub-dos are queryable as distinct work
// beads at distinct DEPTH activations wrapper/round/0/review:0 (closed/fail) and
// wrapper/round/1/review:0 (closed/pass); (3) the ⚑B1 env seam one namespace deeper — the
// RE-MINTED attempt-1 sub-do's node.activated prompt rendered the bound env value, not "";
// (4) exactly two dispatch facts (fresh bead per attempt); (5) zero control beads; Verify.
//
// Seal budget: 2 sequential legs (attempt 0 fail -> attempt 1 pass) + respawn ≈ RBL budget
// (~350s), -timeout 1200s, ISOLATION.
func TestLumenLoopInSubDoltE2E_ReMintsAtDepth(t *testing.T) {
	// 3 workers: sequential attempts run in SEPARATE pooled sessions (one sub-do per
	// session); a small pool keeps a lane from being swept idle between attempts.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-flaky.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, loopInSubIRPath(t), "--input", `{"who":"world"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (repeat-run-in-sub) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF repeat-run-in-sub streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two attempts take >= two claim cycles: sling -> dispatch wrapper/round/0/review:0 ->
	// claim -> fail -> observe -> mint wrapper/round/1/review:0 -> dispatch -> claim -> pass
	// -> observe -> loop settle -> seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (fresh bead per attempt), bound to the DEPTH per-attempt
	// namespaced sub-do activations wrapper/round/0/review:0 then wrapper/round/1/review:0.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (fresh sub-do per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Handle != "wrapper/round/0/review:0" || a1.Handle != "wrapper/round/1/review:0" {
		t.Fatalf("owned.admitted handles = {%q, %q}, want {wrapper/round/0/review:0, wrapper/round/1/review:0}", a0.Handle, a1.Handle)
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("attempt bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// The two sub-do outcomes: attempt 0 failed -> attempt 1 pass, at depth.
	if got := outcomeSettledFor(t, events, "wrapper/round/0/review:0"); got != engine.OutcomeFailed {
		t.Fatalf("outcome.settled wrapper/round/0/review:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "wrapper/round/1/review:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled wrapper/round/1/review:0 = %q, want pass", got)
	}
	// Both attempt aggregates settled (wrapper/round:0 failed, wrapper/round:1 pass).
	if got := outcomeSettledFor(t, events, "wrapper/round:0"); got != engine.OutcomeFailed {
		t.Fatalf("attempt-0 aggregate wrapper/round:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "wrapper/round:1"); got != engine.OutcomePass {
		t.Fatalf("attempt-1 aggregate wrapper/round:1 = %q, want pass", got)
	}
	t.Logf("PROOF two fresh DEPTH attempt namespaces: wrapper/round/0/review=%s (fail) then wrapper/round/1/review=%s (pass); run.closed pass", a0.BeadID, a1.BeadID)

	// ⚑B1 render pin one namespace deeper: the RE-MINTED attempt-1 sub-do's node.activated
	// carried the env-resolved prompt (name <- who = "world"), NOT the silent "".
	if got := lumenActivatedPrompt(t, events, "wrapper/round/1/review:0"); got != "Review the work for world, then settle this step (fail the first attempt, pass after)." {
		t.Fatalf("attempt-1 rendered prompt = %q, want the env-bound review prompt (B1 seam at depth)", got)
	}
	t.Logf("PROOF B1 at depth: re-minted attempt-1 sub-do prompt rendered the bound env value")

	// The VISIBILITY requirement: BOTH attempt sub-do beads are queryable in the work
	// store, keyed by their per-attempt DEPTH activations.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, ok0 := byActivation["wrapper/round/0/review:0"]
	b1, ok1 := byActivation["wrapper/round/1/review:0"]
	if !ok0 || !ok1 {
		t.Fatalf("attempt sub-do beads not both queryable: have %v, want the two wrapper/round/N/review:0 activations", keysOfBeads(byActivation))
	}
	if b0.ID != a0.BeadID || b1.ID != a1.BeadID {
		t.Fatalf("work-store bead ids {%q, %q} do not match dispatch facts {%q, %q}", b0.ID, b1.ID, a0.BeadID, a1.BeadID)
	}
	if beadStatus(b0) != "closed" || metaValue(b0, beadmetaOutcomeKey) != "fail" {
		t.Fatalf("attempt-0 sub-do %s = {status:%q outcome:%q}, want {closed, fail}", b0.ID, beadStatus(b0), metaValue(b0, beadmetaOutcomeKey))
	}
	if beadStatus(b1) != "closed" || metaValue(b1, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("attempt-1 sub-do %s = {status:%q outcome:%q}, want {closed, pass}", b1.ID, beadStatus(b1), metaValue(b1, beadmetaOutcomeKey))
	}
	t.Logf("PROOF both attempt sub-do beads queryable at depth: %s (closed/fail) and %s (closed/pass)", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
