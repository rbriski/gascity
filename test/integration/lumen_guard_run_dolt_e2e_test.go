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

func guardRunDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "guard-run-do.lumen.json")
}

// TestLumenGuardRunDoltE2E_PerAttemptRedecision (GIS marquee, RULED Q-D) proves a guard
// INSIDE a repeat-run-body re-decides fresh per attempt on a real Dolt city:
// `repeat { run stage -> greeter{ draft: guard (reuse=="") -> do } } until stage.outcome
// == pass || iteration >= 2`. The sub-input `reuse` is UNBOUND -> defaulted "" by
// scopeFor's default-VALUE seeding (⚑B2 stamps only its OUTCOME "pass") -> the cond
// `reuse == ""` evaluates TRUE, so each attempt's guard dispatches its then-do. A fail-once-then-pass worker fails attempt 0's then-do; the
// guard settles failed transparently; the attempt aggregate settles failed; the cond
// re-mints attempt 1 under stage/1/, whose guard RE-decides fresh and dispatches a
// DISTINCT write-once then activation with a FRESH bead; it passes; the loop settles pass
// -> seal.
//
// It pins: (1) run.closed pass; (2) BOTH per-attempt then-dos are queryable as distinct
// work beads at distinct activations stage/0/draft.then:0 (closed/fail) and
// stage/1/draft.then:0 (closed/pass) — the compiler then-id convention <guardID>.then
// qualified under the attempt namespace; (3) the ⚑B1 env seam — the RE-MINTED attempt-1
// then-do's node.activated prompt rendered the bound env value ("Say hello to world, …"),
// not "" (a phantom-parent render); (4) exactly two dispatch facts (fresh bead per
// attempt); (5) both per-attempt guards settled (stage/0/draft failed, stage/1/draft
// pass); (6) zero control beads; Verify clean.
//
// Seal budget: 2 sequential legs (attempt 0 fail -> attempt 1 pass) + respawn ≈ 10-minute
// seal wait, -timeout 1200s.
func TestLumenGuardRunDoltE2E_PerAttemptRedecision(t *testing.T) {
	// 3 workers: sequential attempts run in SEPARATE pooled sessions (one then-do per
	// session); a small pool keeps a lane from being swept idle between attempts (the
	// RBL idle-sweep pool — lumen_repeat_run_dolt_e2e_test.go).
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-flaky.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, guardRunDoIRPath(t), "--input", `{"who":"world"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (guard-run-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF guard-run-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two attempts take ≥ two claim cycles: sling → dispatch stage/0/draft.then:0 → claim
	// → fail → observe → mint stage/1/draft.then:0 → dispatch → claim → pass → observe →
	// loop settle → seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (fresh bead per attempt), bound to the per-attempt
	// namespaced guard-then activations stage/0/draft.then:0 then stage/1/draft.then:0.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (fresh then-do per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Handle != "stage/0/draft.then:0" || a1.Handle != "stage/1/draft.then:0" {
		t.Fatalf("owned.admitted handles = {%q, %q}, want {stage/0/draft.then:0, stage/1/draft.then:0}", a0.Handle, a1.Handle)
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("attempt bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// The two then-do outcomes: stage/0/draft.then failed → stage/1/draft.then pass.
	if got := outcomeSettledFor(t, events, "stage/0/draft.then:0"); got != engine.OutcomeFailed {
		t.Fatalf("outcome.settled stage/0/draft.then:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "stage/1/draft.then:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled stage/1/draft.then:0 = %q, want pass", got)
	}
	// Both per-attempt GUARDS settled transparently from their then (the per-attempt
	// re-decision): stage/0/draft failed, stage/1/draft pass.
	if got := outcomeSettledFor(t, events, "stage/0/draft:0"); got != engine.OutcomeFailed {
		t.Fatalf("attempt-0 guard stage/0/draft:0 = %q, want failed (transparent from failed then)", got)
	}
	if got := outcomeSettledFor(t, events, "stage/1/draft:0"); got != engine.OutcomePass {
		t.Fatalf("attempt-1 guard stage/1/draft:0 = %q, want pass", got)
	}
	// Both attempt aggregates settled (stage:0 failed, stage:1 pass).
	if got := outcomeSettledFor(t, events, "stage:0"); got != engine.OutcomeFailed {
		t.Fatalf("attempt-0 aggregate stage:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "stage:1"); got != engine.OutcomePass {
		t.Fatalf("attempt-1 aggregate stage:1 = %q, want pass", got)
	}
	t.Logf("PROOF two fresh attempt namespaces: stage/0/draft.then=%s (fail) then stage/1/draft.then=%s (pass); per-attempt guard re-decision; run.closed pass", a0.BeadID, a1.BeadID)

	// ⚑B1 render pin: the RE-MINTED attempt-1 then-do's node.activated carried the env-
	// resolved prompt (name <- who = "world"), NOT the silent "" a phantom parent renders.
	if got := lumenActivatedPrompt(t, events, "stage/1/draft.then:0"); got != "Say hello to world, then settle this step." {
		t.Fatalf("attempt-1 rendered prompt = %q, want the env-bound %q (B1 seam)", got, "Say hello to world, then settle this step.")
	}
	t.Logf("PROOF B1: re-minted attempt-1 then-do prompt rendered the bound env value")

	// The VISIBILITY requirement: BOTH attempt then-do beads are queryable in the work
	// store, keyed by their per-attempt activations.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, ok0 := byActivation["stage/0/draft.then:0"]
	b1, ok1 := byActivation["stage/1/draft.then:0"]
	if !ok0 || !ok1 {
		t.Fatalf("attempt then-do beads not both queryable: have %v, want stage/0/draft.then:0 and stage/1/draft.then:0", keysOfBeads(byActivation))
	}
	if b0.ID != a0.BeadID || b1.ID != a1.BeadID {
		t.Fatalf("work-store bead ids {%q, %q} do not match dispatch facts {%q, %q}", b0.ID, b1.ID, a0.BeadID, a1.BeadID)
	}
	if beadStatus(b0) != "closed" || metaValue(b0, beadmetaOutcomeKey) != "fail" {
		t.Fatalf("attempt-0 then-do %s = {status:%q outcome:%q}, want {closed, fail}", b0.ID, beadStatus(b0), metaValue(b0, beadmetaOutcomeKey))
	}
	if beadStatus(b1) != "closed" || metaValue(b1, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("attempt-1 then-do %s = {status:%q outcome:%q}, want {closed, pass}", b1.ID, beadStatus(b1), metaValue(b1, beadmetaOutcomeKey))
	}
	t.Logf("PROOF both attempt then-do beads queryable: %s (closed/fail) and %s (closed/pass)", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
