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

func sharedLaneIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "shared-lane.lumen.json")
}

// TestLumenSharedLaneDoltE2E_IndexedDrain (SLX marquee, §3) proves the two shared-lane
// expression extensions — the closed-expr `length()` cond and the indexed-member render
// `{{ items[iteration - 1] }}` — drive end-to-end on a real Dolt city: main `dispatch mode
// { same-session -> run sharedLane -> doWorkShared{ repeat lane_loop { do lane "Work item:
// {{ items[iteration - 1] }}" } until lane.outcome == failed || iteration >= length(items) }
// }`, with items env-bound from the root array input work_items = ["alpha","beta"]. The
// repeat leaf-loop dispatches two SEQUENTIAL lane beads: attempt 0 (iteration 1) renders
// element 0 (alpha), attempt 1 (iteration 2) renders element 1 (beta), then `iteration >=
// length(items)` (2 >= 2) exits and the loop settles pass -> seal.
//
// It pins: (1) run.closed pass; (2) EXACTLY two dispatch facts at the per-attempt lane
// activations sharedLane/lane:0 and sharedLane/lane:1 (the length cond exits at 2, so no
// third attempt mints); (3) the indexed render on real beads — attempt-0's node.activated
// prompt is "Work item: alpha", attempt-1's is "Work item: beta" (per-element, the
// silent-misrender killer); (4) both lane beads closed/pass in the work store; (5) the loop
// aggregate seals pass; (6) zero control beads (a pure Lumen run, no control-dispatcher);
// Verify clean.
//
// Seal budget: two sequential lane legs + respawn ≈ ~350s, -timeout 1200s, ISOLATION.
func TestLumenSharedLaneDoltE2E_IndexedDrain(t *testing.T) {
	// 2 workers: the two lanes are SEQUENTIAL (a leaf loop mints one attempt at a time), each
	// in its own pooled session; a small pool keeps a lane from being swept idle between them.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 2, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, sharedLaneIRPath(t),
		"--input", `{"mode":"same-session","work_items":["alpha","beta"]}`)
	if err != nil {
		t.Fatalf("gc lumen sling (shared-lane) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF shared-lane streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two sequential attempts take >= two claim cycles: sling -> dispatch sharedLane/lane:0
	// -> claim -> pass -> observe -> length cond (1 >= 2 false) re-mints sharedLane/lane:1 ->
	// dispatch -> claim -> pass -> observe -> length cond (2 >= 2 true) -> loop settle -> seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (one per attempt) at the per-attempt lane activations. A
	// third would mean the length cond failed to exit (a lexicographic/string-length bug).
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (length cond exits at 2 attempts)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	byHandle := map[string]lumenOwnedAdmitted{}
	for _, a := range admits {
		oa := decodeOwnedAdmitted(t, a.Payload)
		byHandle[oa.Handle] = oa
	}
	a0, ok0 := byHandle["sharedLane/lane:0"]
	a1, ok1 := byHandle["sharedLane/lane:1"]
	if !ok0 || !ok1 {
		t.Fatalf("dispatch handles = %v, want sharedLane/lane:0 and sharedLane/lane:1", keysOfAdmits(byHandle))
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("lane bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// Both lanes settled pass; the loop aggregate settled pass (exit via the length clause).
	if got := outcomeSettledFor(t, events, "sharedLane/lane:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled sharedLane/lane:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "sharedLane/lane:1"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled sharedLane/lane:1 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "sharedLane/lane_loop:0"); got != engine.OutcomePass {
		t.Fatalf("loop aggregate sharedLane/lane_loop:0 = %q, want pass", got)
	}
	t.Logf("PROOF two sequential indexed lanes: sharedLane/lane:{0,1} both pass; loop exits via length; run.closed pass")

	// THE INDEXED RENDER pin on real beads: attempt 0 renders element 0 (alpha), attempt 1
	// renders element 1 (beta) — verbatim per the fork-defined index semantic, NOT the
	// literal source text "items[iteration - 1]" (the silent-misrender FALSE-POOL-ok killer).
	if got := lumenActivatedPrompt(t, events, "sharedLane/lane:0"); got != "Work item: alpha" {
		t.Fatalf("attempt-0 rendered prompt = %q, want %q (indexed element 0)", got, "Work item: alpha")
	}
	if got := lumenActivatedPrompt(t, events, "sharedLane/lane:1"); got != "Work item: beta" {
		t.Fatalf("attempt-1 rendered prompt = %q, want %q (indexed element 1)", got, "Work item: beta")
	}
	t.Logf("PROOF indexed render at depth: lane:0=%q lane:1=%q", "Work item: alpha", "Work item: beta")

	// The VISIBILITY requirement: BOTH lane beads are queryable in the work store, keyed by
	// their per-attempt activations, closed/pass.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, hasB0 := byActivation["sharedLane/lane:0"]
	b1, hasB1 := byActivation["sharedLane/lane:1"]
	if !hasB0 || !hasB1 {
		t.Fatalf("lane beads not both queryable: have %v, want the two sharedLane/lane:N activations", keysOfBeads(byActivation))
	}
	if beadStatus(b0) != "closed" || metaValue(b0, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("lane-0 bead %s = {status:%q outcome:%q}, want {closed, pass}", b0.ID, beadStatus(b0), metaValue(b0, beadmetaOutcomeKey))
	}
	if beadStatus(b1) != "closed" || metaValue(b1, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("lane-1 bead %s = {status:%q outcome:%q}, want {closed, pass}", b1.ID, beadStatus(b1), metaValue(b1, beadmetaOutcomeKey))
	}
	t.Logf("PROOF both lane beads queryable: %s and %s (closed/pass)", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
