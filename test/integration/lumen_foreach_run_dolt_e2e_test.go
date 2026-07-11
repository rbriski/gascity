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

func forEachRunDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "for-each-run-do.lumen.json")
}

// TestLumenForEachInRunDoltE2E_FansInsideAttemptNamespace (FIS acceptance, RULED Q-A —
// attempt depth) proves a for-each fans CONCURRENTLY inside a repeat-run-body ATTEMPT
// namespace on a real Dolt city: `repeat { run stage -> reviewer{ fanout: scatter item
// in items { do review } } } until stage.outcome == pass || iteration >= 2`, with the
// sub-do's prompt binding the per-element item AND the env-bound label. All members pass
// on the first attempt, so the loop exits after one attempt.
//
// It exercises the novel durable seams (registerRunBodyEnv + the parentNS override on the
// over-eval path, '/'-bearing dynamic member activations as real Tier-A rows, the
// qualified-key binder render on real beads) and pins: (1) run.closed pass; (2) BOTH fan
// members are queryable as distinct work beads at stage/0/fanout/0:0 and
// stage/0/fanout/1:0, closed/pass; (3) their prompts render DISTINCT elements (member 0
// has alpha NOT beta, member 1 has beta NOT alpha) plus the env-bound label; (4) exactly
// two dispatch facts (one per element); (5) the fan aggregate, attempt aggregate, loop,
// and run all seal pass; (6) zero control beads; Verify clean.
//
// Seal budget: one concurrent fan of two do members ≈ 8-minute seal wait, -timeout 1200s.
func TestLumenForEachInRunDoltE2E_FansInsideAttemptNamespace(t *testing.T) {
	// 3 workers: the two fan members claim CONCURRENTLY in separate pooled sessions; the
	// spare lane keeps the pool warm through the seal.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, forEachRunDoIRPath(t),
		"--input", `{"items":["alpha","beta"],"label":"release-7"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (for-each-run-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF for-each-run-do streamID = %s", streamID)

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

	// Exactly two dispatch facts (one per element), at the '/'-bearing dynamic member
	// activations. The members fan CONCURRENTLY, so index by handle rather than order.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (one dispatch per fan element)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	byHandle := map[string]lumenOwnedAdmitted{}
	for _, a := range admits {
		oa := decodeOwnedAdmitted(t, a.Payload)
		byHandle[oa.Handle] = oa
	}
	a0, ok0 := byHandle["stage/0/fanout/0:0"]
	a1, ok1 := byHandle["stage/0/fanout/1:0"]
	if !ok0 || !ok1 {
		t.Fatalf("dispatch handles = %v, want stage/0/fanout/0:0 and stage/0/fanout/1:0", keysOfAdmits(byHandle))
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("fan member bead ids = {%q, %q}, want two distinct store-minted ids", a0.BeadID, a1.BeadID)
	}

	// Both fan members settled pass.
	if got := outcomeSettledFor(t, events, "stage/0/fanout/0:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled stage/0/fanout/0:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "stage/0/fanout/1:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled stage/0/fanout/1:0 = %q, want pass", got)
	}
	// The fan aggregate, attempt aggregate, and loop all sealed pass.
	if got := outcomeSettledFor(t, events, "stage/0/fanout:0"); got != engine.OutcomePass {
		t.Fatalf("fan aggregate stage/0/fanout:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "stage:0"); got != engine.OutcomePass {
		t.Fatalf("attempt aggregate stage:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "loop:0"); got != engine.OutcomePass {
		t.Fatalf("loop:0 = %q, want pass", got)
	}
	t.Logf("PROOF concurrent fan inside attempt ns: stage/0/fanout/{0,1} both pass; run.closed pass")

	// The DISTINCT-element render pin (the qualified-key binder render on real beads): each
	// member's node.activated prompt carries its OWN element plus the env-bound label, and
	// NOT the sibling's element.
	p0 := lumenActivatedPrompt(t, events, "stage/0/fanout/0:0")
	p1 := lumenActivatedPrompt(t, events, "stage/0/fanout/1:0")
	if !strings.Contains(p0, "alpha") || strings.Contains(p0, "beta") || !strings.Contains(p0, "release-7") {
		t.Fatalf("member 0 prompt = %q, want alpha + release-7 and NOT beta (per-member binding + env value)", p0)
	}
	if !strings.Contains(p1, "beta") || strings.Contains(p1, "alpha") || !strings.Contains(p1, "release-7") {
		t.Fatalf("member 1 prompt = %q, want beta + release-7 and NOT alpha", p1)
	}
	t.Logf("PROOF distinct per-member render: [0]=%q [1]=%q", p0, p1)

	// The VISIBILITY requirement: BOTH fan member beads are queryable in the work store,
	// keyed by their dynamic '/'-bearing activations, closed/pass.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, has0 := byActivation["stage/0/fanout/0:0"]
	b1, has1 := byActivation["stage/0/fanout/1:0"]
	if !has0 || !has1 {
		t.Fatalf("fan member beads not both queryable: have %v, want stage/0/fanout/0:0 and stage/0/fanout/1:0", keysOfBeads(byActivation))
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
	t.Logf("PROOF both fan member beads queryable closed/pass: %s and %s", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// keysOfAdmits returns the handle keys of an admitted-by-handle map, for diagnostics.
func keysOfAdmits(m map[string]lumenOwnedAdmitted) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
