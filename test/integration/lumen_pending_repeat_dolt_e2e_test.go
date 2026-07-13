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

func checkRepairPendingIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "check-repair-pending.lumen.json")
}

// TestLumenPendingRepeatDoltE2E_PollThenPassNonConsuming proves the `pending` NON-CONSUMING
// repeat continue end-to-end on a real Dolt city:
// `repeat lane: <do> until lane.outcome == pass || iteration >= 1`, driven by a worker that
// closes the FIRST claim gc.outcome=pending (a poll — the check's CI is still running) and
// every later claim gc.outcome=pass (the repair passed).
//
// The tight budget `iteration >= 1` is the discriminator: a pending poll must NOT consume
// the budget. With the correct consuming-count decoupling the poll (lane:0) does not burn
// iteration, so the loop re-polls, mints lane:1, passes, and seals PASS in TWO physical
// attempts. With the old attempt+1 iteration the very first pending poll would trip
// `iteration >= 1` and settle the loop `pending` after ONE attempt — no lane:1, no pass.
//
// It pins: (1) run.closed pass; (2) the physical namespace advanced across the poll —
// lane:0 settled PENDING then lane:1 settled PASS as two distinct work beads; (3) exactly
// two dispatch facts (fresh bead per attempt); (4) the loop settled pass; (5) zero control
// beads; Verify clean.
//
// Seal budget: 2 sequential legs (poll → repair-pass) + respawn ≈ a ~10-minute seal wait,
// -timeout 1200s.
func TestLumenPendingRepeatDoltE2E_PollThenPassNonConsuming(t *testing.T) {
	// 3 workers: sequential attempts run in SEPARATE pooled sessions (one do per session);
	// a small pool keeps a lane from being swept idle between the poll and the repair.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-pending-then-pass.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, checkRepairPendingIRPath(t), "--input", `{}`)
	if err != nil {
		t.Fatalf("gc lumen sling (check-repair-pending) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF check-repair-pending streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two attempts take ≥ two claim cycles: sling → dispatch lane:0 → claim → close pending
	// → observe → mint lane:1 (the poll did NOT consume the budget) → dispatch → claim →
	// close pass → observe → loop settle pass → seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass (the pending poll must not consume the budget)", closed.Outcome)
	}

	// The physical namespace advanced across the poll: lane:0 PENDING, then lane:1 PASS.
	if got := outcomeSettledFor(t, events, "lane:0"); got != engine.OutcomePending {
		t.Fatalf("outcome.settled lane:0 = %q, want pending (the first close is a poll)", got)
	}
	if got := outcomeSettledFor(t, events, "lane:1"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled lane:1 = %q, want pass (the repair after a non-consuming re-poll)", got)
	}

	// Exactly two dispatch facts (fresh bead per physical attempt), lane:0 then lane:1.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (poll then repair, fresh bead per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Handle != "lane:0" || a1.Handle != "lane:1" {
		t.Fatalf("owned.admitted handles = {%q, %q}, want {lane:0, lane:1}", a0.Handle, a1.Handle)
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("attempt bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// The loop settled pass (the passing attempt's outcome), NOT pending.
	if got := outcomeSettledFor(t, events, "repeat_1:0"); got != engine.OutcomePass {
		t.Fatalf("loop repeat_1:0 settle = %q, want pass (the poll did not exhaust the budget at iteration 1)", got)
	}
	t.Logf("PROOF pending non-consuming: lane:0=pending (poll) → lane:1=pass (repair) → loop pass; run.closed pass")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
