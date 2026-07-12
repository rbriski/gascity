//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func dispatchRunArmIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "dispatch-run-arm.lumen.json")
}

// TestLumenDispatchRunArmDoltE2E_MintsChosenArm (DAR acceptance, RULED Q-D — ONE arm) proves a
// dispatch whose MATCHED arm is a `run` sub-formula call mints ONLY that arm's fresh sub-graph
// on a real Dolt city: `dispatch policy { "separate": run drainSeparate given {reviewer: lit,
// target: <input>}, "same-session": run drainShared given {…} }` with policy="separate" —
// drainSeparate = one `do drain` binding the arm's literal reviewer AND the env-bound target.
// The chosen arm's sub-do dispatches as a real bead, renders its env, and settles; the OTHER
// arm (same-session) has ZERO journal rows and ZERO beads (asserted in the SAME run).
//
// It exercises the novel durable seams (mintRunBody per matched arm, the DAR parentNS
// override on the arm's env-eval path, the two-level '/'-bearing dynamic arm-aggregate +
// sub-node rows as real Tier-A rows under the live FK, the arm-agg-activates-LAST ordering,
// the stateless re-select) and pins: (1) run.closed pass; (2) the chosen arm's sub-do is
// queryable as a work bead at sepLane/drain:0, closed/pass; (3) its prompt renders the arm's
// literal reviewer AND the env-bound target; (4) exactly ONE dispatch fact (one arm, one
// sub-do); (5) the arm aggregate (sepLane:0, transparent run seal) AND the dispatch (d:0)
// settle pass; (6) the UNCHOSEN arm (sharedLane) has ZERO activations/settles/beads; (7) zero
// control beads; Verify clean.
//
// Seal budget: one chosen-arm sub-do ≈ 260s (FIS class) seal wait, -timeout 1200s.
func TestLumenDispatchRunArmDoltE2E_MintsChosenArm(t *testing.T) {
	// 3 workers: the chosen arm's sub-do claims in a pooled session; the spare lanes keep the
	// pool warm through the seal (FIS-class budget).
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, dispatchRunArmIRPath(t),
		"--input", `{"policy":"separate","target":"release-7"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (dispatch-run-arm) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF dispatch-run-arm streamID = %s", streamID)

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

	// Exactly ONE dispatch fact, at the chosen arm's '/'-bearing dynamic sub-do activation.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (only the chosen arm's sub-do dispatches)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	oa := decodeOwnedAdmitted(t, admits[0].Payload)
	if oa.Handle != "sepLane/drain:0" {
		t.Fatalf("dispatch handle = %q, want sepLane/drain:0 (the chosen arm's sub-do)", oa.Handle)
	}
	if oa.BeadID == "" {
		t.Fatalf("chosen arm sub-do bead id is empty, want a store-minted id")
	}

	// The chosen arm's sub-do settled pass; the arm aggregate (transparent run seal) AND the
	// dispatch settled pass.
	if got := outcomeSettledFor(t, events, "sepLane/drain:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled sepLane/drain:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "sepLane:0"); got != engine.OutcomePass {
		t.Fatalf("arm aggregate sepLane:0 = %q, want pass (transparent run seal)", got)
	}
	if got := outcomeSettledFor(t, events, "d:0"); got != engine.OutcomePass {
		t.Fatalf("dispatch d:0 = %q, want pass (transparent from the chosen arm)", got)
	}
	t.Logf("PROOF chosen-arm sub-graph: sepLane/drain pass; arm agg sepLane:0 pass; dispatch d:0 pass; run.closed pass")

	// The env render pin (the qualified-key arm render on a real bead): the chosen arm's
	// sub-do node.activated prompt carries the arm's LITERAL reviewer ("fanout") plus the
	// env-bound target ("release-7"), and NOT the other arm's literal ("shared").
	p := lumenActivatedPrompt(t, events, "sepLane/drain:0")
	if !strings.Contains(p, "fanout") || !strings.Contains(p, "release-7") || strings.Contains(p, "shared") {
		t.Fatalf("chosen arm prompt = %q, want fanout + release-7 and NOT shared (per-arm literal + env value)", p)
	}
	t.Logf("PROOF chosen-arm env render: %q", p)

	// The UNCHOSEN arm (same-session / sharedLane) has ZERO journal rows: no node.activated,
	// no outcome.settled at any sharedLane activation.
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var na struct {
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal(e.Payload, &na); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		if na.NodeID == "sharedLane" || strings.HasPrefix(na.NodeID, "sharedLane/") {
			t.Fatalf("unchosen arm node %q was activated; want ZERO rows for the unmatched arm", na.NodeID)
		}
	}
	if got := outcomeSettledFor(t, events, "sharedLane:0"); got != "" {
		t.Fatalf("unchosen arm aggregate sharedLane:0 settled %q, want ZERO (never minted)", got)
	}
	if got := outcomeSettledFor(t, events, "sharedLane/drain:0"); got != "" {
		t.Fatalf("unchosen arm sub sharedLane/drain:0 settled %q, want ZERO (never minted)", got)
	}

	// The VISIBILITY requirement: the chosen arm's sub-do bead is queryable in the work store
	// keyed by its dynamic '/'-bearing activation, closed/pass; and there is NO bead for the
	// unchosen arm's sub-do.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b, has := byActivation["sepLane/drain:0"]
	if !has {
		t.Fatalf("chosen arm sub-do bead not queryable: have %v, want sepLane/drain:0", keysOfBeads(byActivation))
	}
	if b.ID != oa.BeadID {
		t.Fatalf("work-store bead id %q does not match dispatch fact %q", b.ID, oa.BeadID)
	}
	if beadStatus(b) != "closed" || metaValue(b, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("chosen arm bead %s = {status:%q outcome:%q}, want {closed, pass}", b.ID, beadStatus(b), metaValue(b, beadmetaOutcomeKey))
	}
	if _, hasOther := byActivation["sharedLane/drain:0"]; hasOther {
		t.Fatalf("unchosen arm sub-do bead sharedLane/drain:0 exists in the work store; want ZERO beads for the unmatched arm")
	}
	t.Logf("PROOF chosen arm bead queryable closed/pass: %s; unchosen arm ZERO beads", b.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
