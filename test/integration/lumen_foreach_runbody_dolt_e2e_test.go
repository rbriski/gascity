//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func forEachRunBodyIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "for-each-run-body.lumen.json")
}

// TestLumenForEachRunBodyDoltE2E_MintsPerMemberSubGraphs (FBR acceptance, RULED Q-A — ROOT
// depth) proves a for-each whose MEMBER is a `run` sub-formula call mints a fresh per-member
// sub-graph CONCURRENTLY on a real Dolt city: `fanout: scatter reviewer in reviewers { lane:
// run reviewLane given {reviewer: <binder>, target: <input>} }`, reviewLane = one `do review`
// binding the per-element reviewer AND the env-bound target. Both members' sub-graphs fan in
// one pass; each sub-do is a distinct concurrent work bead.
//
// It exercises the novel durable seams (mintRunBody per fan element, the FBR parentNS
// override on the member's env-eval path, two-level '/'-bearing dynamic member-aggregate +
// sub-node rows as real Tier-A rows under the live FK, the member-agg-activates-LAST
// ordering) and pins: (1) run.closed pass; (2) BOTH fan members' sub-dos are queryable as
// distinct work beads at fanout/0/review:0 and fanout/1/review:0, closed/pass; (3) their
// prompts render DISTINCT elements (member 0 has alpha NOT beta, member 1 has beta NOT alpha)
// plus the env-bound target; (4) exactly two dispatch facts (one per element); (5) the two
// member aggregates (fanout/<i>:0, transparent run seals) AND the fan aggregate all seal
// pass; (6) zero control beads; Verify clean.
//
// Seal budget: one concurrent fan of two do members ≈ 8-minute seal wait, -timeout 1200s.
func TestLumenForEachRunBodyDoltE2E_MintsPerMemberSubGraphs(t *testing.T) {
	// 3 workers: the two member sub-dos claim CONCURRENTLY in separate pooled sessions; the
	// spare lane keeps the pool warm through the seal.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, forEachRunBodyIRPath(t),
		"--input", `{"reviewers":["alpha","beta"],"target":"release-7"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (for-each-run-body) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF for-each-run-body streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (one per element), at the '/'-bearing dynamic member sub-do
	// activations. The members fan CONCURRENTLY, so index by handle rather than order.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (one dispatch per fan member sub-do)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	byHandle := map[string]lumenOwnedAdmitted{}
	for _, a := range admits {
		oa := decodeOwnedAdmitted(t, a.Payload)
		byHandle[oa.Handle] = oa
	}
	a0, ok0 := byHandle["fanout/0/review:0"]
	a1, ok1 := byHandle["fanout/1/review:0"]
	if !ok0 || !ok1 {
		t.Fatalf("dispatch handles = %v, want fanout/0/review:0 and fanout/1/review:0", keysOfAdmits(byHandle))
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("member sub-do bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// Both member sub-dos settled pass.
	if got := outcomeSettledFor(t, events, "fanout/0/review:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled fanout/0/review:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "fanout/1/review:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled fanout/1/review:0 = %q, want pass", got)
	}
	// The two member aggregates (transparent run seals) AND the fan aggregate all sealed pass.
	if got := outcomeSettledFor(t, events, "fanout/0:0"); got != engine.OutcomePass {
		t.Fatalf("member 0 aggregate fanout/0:0 = %q, want pass (transparent run seal)", got)
	}
	if got := outcomeSettledFor(t, events, "fanout/1:0"); got != engine.OutcomePass {
		t.Fatalf("member 1 aggregate fanout/1:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "fanout:0"); got != engine.OutcomePass {
		t.Fatalf("fan aggregate fanout:0 = %q, want pass", got)
	}
	t.Logf("PROOF concurrent per-member sub-graphs: fanout/{0,1}/review pass; member aggs fanout/{0,1}:0 pass; run.closed pass")

	// The DISTINCT-element render pin (the qualified-key binder render on real beads, both
	// directions): each member's sub-do node.activated prompt carries its OWN element plus
	// the env-bound target, and NOT the sibling's element.
	p0 := lumenActivatedPrompt(t, events, "fanout/0/review:0")
	p1 := lumenActivatedPrompt(t, events, "fanout/1/review:0")
	if !strings.Contains(p0, "alpha") || strings.Contains(p0, "beta") || !strings.Contains(p0, "release-7") {
		t.Fatalf("member 0 prompt = %q, want alpha + release-7 and NOT beta (per-member binder + env value)", p0)
	}
	if !strings.Contains(p1, "beta") || strings.Contains(p1, "alpha") || !strings.Contains(p1, "release-7") {
		t.Fatalf("member 1 prompt = %q, want beta + release-7 and NOT alpha", p1)
	}
	t.Logf("PROOF distinct per-member render: [0]=%q [1]=%q", p0, p1)

	// The VISIBILITY requirement: BOTH member sub-do beads are queryable in the work store,
	// keyed by their dynamic '/'-bearing activations, closed/pass.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, has0 := byActivation["fanout/0/review:0"]
	b1, has1 := byActivation["fanout/1/review:0"]
	if !has0 || !has1 {
		t.Fatalf("member sub-do beads not both queryable: have %v, want fanout/0/review:0 and fanout/1/review:0", keysOfBeads(byActivation))
	}
	if b0.ID != a0.BeadID || b1.ID != a1.BeadID {
		t.Fatalf("work-store bead ids {%q, %q} do not match dispatch facts {%q, %q}", b0.ID, b1.ID, a0.BeadID, a1.BeadID)
	}
	if beadStatus(b0) != "closed" || metaValue(b0, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("member 0 bead %s = {status:%q outcome:%q}, want {closed, pass}", b0.ID, beadStatus(b0), metaValue(b0, beadmetaOutcomeKey))
	}
	if beadStatus(b1) != "closed" || metaValue(b1, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("member 1 bead %s = {status:%q outcome:%q}, want {closed, pass}", b1.ID, beadStatus(b1), metaValue(b1, beadmetaOutcomeKey))
	}
	t.Logf("PROOF both member sub-do beads queryable closed/pass: %s and %s", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
