//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func repeatRunDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "repeat-run-do.lumen.json")
}

// TestLumenRepeatRunDoltE2E_FailThenPassReMints (RBL acceptance) proves a repeat whose
// body is a `run` re-mints its sub-graph under a FRESH namespace across attempts on a
// real Dolt city: `repeat { run stage -> greeter{ do hello } } until stage.outcome ==
// pass || iteration >= 2`, with the sub-do's prompt binding the run environment
// (name <- who). A fail-once-then-pass worker fails attempt 0's sub-do; the attempt
// aggregate settles failed; the cond re-mints attempt 1 under stage/1/ with a FRESH
// work bead; it passes; the aggregate settles pass; the cond exits and the loop settles
// pass → seal.
//
// It pins: (1) run.closed pass; (2) BOTH attempt sub-dos are queryable as distinct
// work beads at distinct activations stage/0/hello:0 (closed/fail) and stage/1/hello:0
// (closed/pass); (3) the ⚑B1 env seam — the RE-MINTED attempt-1 sub-do's node.activated
// prompt rendered the bound env value ("Say hello to world, …"), not "" (a phantom-
// parent render); (4) exactly two dispatch facts (fresh bead per attempt); (5) zero
// control beads; Verify clean.
//
// Seal budget: 2 sequential legs (attempt 0 fail → attempt 1 pass) + respawn ≈ 10-minute
// seal wait, -timeout 1200s.
func TestLumenRepeatRunDoltE2E_FailThenPassReMints(t *testing.T) {
	// 3 workers: sequential attempts run in SEPARATE pooled sessions (one sub-do per
	// session); a small pool keeps a lane from being swept idle between attempts.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-flaky.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, repeatRunDoIRPath(t), "--input", `{"who":"world"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (repeat-run-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF repeat-run-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two attempts take ≥ two claim cycles: sling → dispatch stage/0/hello:0 → claim →
	// fail → observe → mint stage/1/hello:0 → dispatch → claim → pass → observe → loop
	// settle → seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (fresh bead per attempt), bound to the per-attempt
	// namespaced sub-do activations stage/0/hello:0 then stage/1/hello:0.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (fresh sub-do per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Handle != "stage/0/hello:0" || a1.Handle != "stage/1/hello:0" {
		t.Fatalf("owned.admitted handles = {%q, %q}, want {stage/0/hello:0, stage/1/hello:0}", a0.Handle, a1.Handle)
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("attempt bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// The two sub-do outcomes: stage/0/hello failed → stage/1/hello pass.
	if got := outcomeSettledFor(t, events, "stage/0/hello:0"); got != engine.OutcomeFailed {
		t.Fatalf("outcome.settled stage/0/hello:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "stage/1/hello:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled stage/1/hello:0 = %q, want pass", got)
	}
	// Both attempt aggregates settled (stage:0 failed, stage:1 pass — the bare id is the
	// highest attempt, pass).
	if got := outcomeSettledFor(t, events, "stage:0"); got != engine.OutcomeFailed {
		t.Fatalf("attempt-0 aggregate stage:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "stage:1"); got != engine.OutcomePass {
		t.Fatalf("attempt-1 aggregate stage:1 = %q, want pass", got)
	}
	t.Logf("PROOF two fresh attempt namespaces: stage/0/hello=%s (fail) then stage/1/hello=%s (pass); run.closed pass", a0.BeadID, a1.BeadID)

	// ⚑B1 render pin: the RE-MINTED attempt-1 sub-do's node.activated carried the env-
	// resolved prompt (name <- who = "world"), NOT the silent "" a phantom parent renders.
	if got := lumenActivatedPrompt(t, events, "stage/1/hello:0"); got != "Say hello to world, then settle this step." {
		t.Fatalf("attempt-1 rendered prompt = %q, want the env-bound %q (B1 seam)", got, "Say hello to world, then settle this step.")
	}
	t.Logf("PROOF B1: re-minted attempt-1 sub-do prompt rendered the bound env value")

	// The VISIBILITY requirement: BOTH attempt sub-do beads are queryable in the work
	// store, keyed by their per-attempt activations.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, ok0 := byActivation["stage/0/hello:0"]
	b1, ok1 := byActivation["stage/1/hello:0"]
	if !ok0 || !ok1 {
		t.Fatalf("attempt sub-do beads not both queryable: have %v, want stage/0/hello:0 and stage/1/hello:0", keysOfBeads(byActivation))
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
	t.Logf("PROOF both attempt sub-do beads queryable: %s (closed/fail) and %s (closed/pass)", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenActivatedPrompt returns the rendered prompt carried by the node.activated event
// for a pool-dispatched activation — the ENGINE's env-resolved render (byte-identical to
// the dispatched work bead's Description), used for the B1 seam pin.
func lumenActivatedPrompt(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Prompt     string `json:"prompt"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		if p.Activation == activation {
			return p.Prompt
		}
	}
	t.Fatalf("no node.activated for activation %q", activation)
	return ""
}
