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

func dispatchAtDepthIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "dispatch-at-depth.lumen.json")
}

// TestLumenDispatchAtDepthDoltE2E_ArmMintsInDeepNS (DAD acceptance, §3) proves a dispatch that
// lives INSIDE a run sub-formula's namespace — reached through two STATIC run hops
// (continue-chain → midChain → continue-chain → leafChain) — selects its matched RUN arm off the
// env-bound sub-scope and mints that arm's fresh sub-graph at the TWO-HOP-qualified coordinates
// on a real Dolt city: `implement: dispatch drain_policy { "separate": run drainSeparate given
// {reviewer: "fanout", target: <input>}, "same-session": <do> }` with drain_policy passed
// EXPLICITLY = "separate" (§0.6c — the bound-"" chain would otherwise no-match on the unseeded
// root default). The chosen RUN arm's sub-do dispatches as a real bead at the DEEP activation
// continue-chain/continue-chain/lanes/drain:0, renders its env, and settles; the UNCHOSEN LEAF
// arm (same-session / sharedLane) has ZERO journal rows and ZERO beads.
//
// It exercises the DAD seams (the deleted prefix fence; the namespace-aware matchingArm subject
// eval via scopeFor(deep ns); the deep parentNS override on the arm's env-eval path; the
// three-level '/'-bearing dynamic arm-aggregate + sub-node rows as real Tier-A rows under the
// live FK; the stateless deep re-select) and pins: (1) run.closed pass; (2) the chosen arm's
// sub-do is queryable as a work bead at continue-chain/continue-chain/lanes/drain:0, closed/pass;
// (3) its prompt renders the arm's literal reviewer AND the env-bound target threaded through
// both hops; (4) exactly ONE dispatch fact; (5) the deep arm aggregate + the deep dispatch settle
// pass; (6) the UNCHOSEN LEAF arm (sharedLane) has ZERO activations/settles/beads; (7) zero
// control beads; Verify clean.
//
// Seal budget: one chosen-arm deep sub-do ≈ 300s seal wait, -timeout 1200s, ISOLATION.
func TestLumenDispatchAtDepthDoltE2E_ArmMintsInDeepNS(t *testing.T) {
	// 2 workers: the chosen arm's deep sub-do claims in a pooled session; the spare lane keeps
	// the pool warm through the seal.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 2, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	// drain_policy passed EXPLICITLY (§0.6c): the two hops bind it by ref, so an omitted root
	// default would render "" and deep-no-match. The explicit value drives the marquee.
	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, dispatchAtDepthIRPath(t),
		"--input", `{"drain_policy":"separate","target":"release-7"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (dispatch-at-depth) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF dispatch-at-depth streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	const deepSub = "continue-chain/continue-chain/lanes/drain:0"
	const deepArm = "continue-chain/continue-chain/lanes:0"
	const deepDispatch = "continue-chain/continue-chain/implement:0"

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly ONE dispatch fact, at the chosen arm's DEEP '/'-bearing dynamic sub-do activation.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (only the chosen deep arm's sub-do dispatches)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	oa := decodeOwnedAdmitted(t, admits[0].Payload)
	if oa.Handle != deepSub {
		t.Fatalf("dispatch handle = %q, want %s (the chosen deep arm's sub-do, activation-form)", oa.Handle, deepSub)
	}
	if oa.BeadID == "" {
		t.Fatalf("chosen deep arm sub-do bead id is empty, want a store-minted id")
	}

	// The chosen arm's deep sub-do settled pass; the deep arm aggregate (transparent run seal)
	// AND the deep dispatch settled pass.
	if got := outcomeSettledFor(t, events, deepSub); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass", deepSub, got)
	}
	if got := outcomeSettledFor(t, events, deepArm); got != engine.OutcomePass {
		t.Fatalf("deep arm aggregate %s = %q, want pass (transparent run seal)", deepArm, got)
	}
	if got := outcomeSettledFor(t, events, deepDispatch); got != engine.OutcomePass {
		t.Fatalf("deep dispatch %s = %q, want pass (transparent from the chosen arm)", deepDispatch, got)
	}
	t.Logf("PROOF deep chosen-arm sub-graph: %s pass; arm agg %s pass; dispatch %s pass; run.closed pass", deepSub, deepArm, deepDispatch)

	// The env render pin at DEPTH (the qualified-key arm render on a real bead): the chosen arm's
	// deep sub-do node.activated prompt carries the arm's LITERAL reviewer ("fanout") plus the
	// env-bound target ("release-7") threaded through BOTH hops, and NOT the other arm's "shared".
	p := lumenActivatedPrompt(t, events, deepSub)
	if !strings.Contains(p, "fanout") || !strings.Contains(p, "release-7") || strings.Contains(p, "shared") {
		t.Fatalf("chosen deep arm prompt = %q, want fanout + release-7 and NOT shared (per-arm literal + env value threaded)", p)
	}
	t.Logf("PROOF deep chosen-arm env render: %q", p)

	// The UNCHOSEN LEAF arm (same-session / sharedLane) has ZERO journal rows: no node.activated
	// at any sharedLane activation in the deep namespace.
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
		if na.NodeID == "continue-chain/continue-chain/sharedLane" {
			t.Fatalf("unchosen deep arm node %q was activated; want ZERO rows for the unmatched arm", na.NodeID)
		}
	}
	if got := outcomeSettledFor(t, events, "continue-chain/continue-chain/sharedLane:0"); got != "" {
		t.Fatalf("unchosen deep arm sharedLane settled %q, want ZERO (never minted)", got)
	}

	// The VISIBILITY requirement: the chosen arm's deep sub-do bead is queryable in the work
	// store keyed by its dynamic three-level '/'-bearing activation, closed/pass; and there is NO
	// bead for the unchosen leaf arm.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b, has := byActivation[deepSub]
	if !has {
		t.Fatalf("chosen deep arm sub-do bead not queryable: have %v, want %s", keysOfBeads(byActivation), deepSub)
	}
	if b.ID != oa.BeadID {
		t.Fatalf("work-store bead id %q does not match dispatch fact %q", b.ID, oa.BeadID)
	}
	if beadStatus(b) != "closed" || metaValue(b, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("chosen deep arm bead %s = {status:%q outcome:%q}, want {closed, pass}", b.ID, beadStatus(b), metaValue(b, beadmetaOutcomeKey))
	}
	if _, hasOther := byActivation["continue-chain/continue-chain/sharedLane:0"]; hasOther {
		t.Fatalf("unchosen leaf arm bead exists in the work store; want ZERO beads for the unmatched arm")
	}
	t.Logf("PROOF deep chosen arm bead queryable closed/pass: %s; unchosen leaf arm ZERO beads", b.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}
