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

func timeoutCheckIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "timeout-check.lumen.json")
}

// TestLumenTimeoutCheckDoltE2E_AdvisoryBudgetSeals (TNK marquee, §3) proves the timeout node
// kind — an advisory check-with-budget wrapper — drives end-to-end on a real Dolt city: main
// `repeat loop { run stage -> stageFormula{ draft: do "…"; check: timeout 5m { v: exec "echo
// checked" } after draft; report: exec "echo report {{ check }}" after check } } until
// stage.outcome == pass || iteration >= 2`. The sub-formula's `draft` do dispatches ONE pooled
// bead (the only pool work); the `check` timeout's body exec `v` runs ENGINE-SIDE (exec never
// pools), the wrapper settles TRANSPARENTLY from it, and the downstream `report` gate fires.
// The stage aggregate passes on attempt 0, so the repeat exits after one attempt.
//
// It pins: (1) run.closed pass; (2) the check WRAPPER settled from its exec body — stage/0/v
// (the body) and stage/0/check (the wrapper) both pass; (3) duration:"5m" on the wrapper's
// node.activated (stage/0/check:0) — the advisory budget as journal metadata (enforcement is
// gc-side, off this field, NEVER the bead — the do bead carries no budget); (4) the downstream
// gate fired — stage/0/report passed; (5) exactly ONE dispatch fact (the draft do — the exec
// check and report run engine-side); (6) zero control beads (a pure Lumen run); Verify clean.
//
// NOTE (WRITE-time): the corpus body is `exec test -s <artifact>`; here the engine-side exec is
// a deterministic `echo checked` (the worker's artifact path is not statically addressable from
// the controller-side exec), which exercises the identical wrapper-settle + advisory-budget +
// downstream-gate contract without a filesystem coupling.
//
// Seal budget: one pooled draft do + engine-side exec check/report + respawn ≈ ~300s,
// -timeout 1200s, ISOLATION.
func TestLumenTimeoutCheckDoltE2E_AdvisoryBudgetSeals(t *testing.T) {
	// 2 workers: only the draft do is pooled (one bead); a small pool keeps the lane from
	// being swept idle while the engine-side exec check settles.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 2, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, timeoutCheckIRPath(t), "--input", `{"who":"world"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (timeout-check) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF timeout-check streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// One claim cycle: sling -> dispatch stage/0/draft -> claim -> pass -> observe -> draft
	// settles -> check timeout activates (+duration) -> exec body settles engine-side -> check
	// settles -> report settles -> stage aggregate passes -> loop exits -> seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly ONE dispatch fact — the draft do. The check body exec and the report exec run
	// engine-side (exec never pools), so they mint NO work beads.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (only the draft do pools; check/report run engine-side)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	draft := decodeOwnedAdmitted(t, admits[0].Payload)
	if draft.Handle != "stage/0/draft:0" {
		t.Fatalf("dispatch handle = %q, want stage/0/draft:0", draft.Handle)
	}

	// The check WRAPPER settled TRANSPARENTLY from its exec body: both the body (stage/0/v) and
	// the wrapper (stage/0/check) passed, and the downstream gate (stage/0/report) fired.
	if got := outcomeSettledFor(t, events, "stage/0/v:0"); got != engine.OutcomePass {
		t.Fatalf("timeout body stage/0/v:0 = %q, want pass (the engine-side exec ran)", got)
	}
	if got := outcomeSettledFor(t, events, "stage/0/check:0"); got != engine.OutcomePass {
		t.Fatalf("timeout wrapper stage/0/check:0 = %q, want pass (transparent from the exec body)", got)
	}
	if got := outcomeSettledFor(t, events, "stage/0/report:0"); got != engine.OutcomePass {
		t.Fatalf("downstream gate stage/0/report:0 = %q, want pass (the wrapper gated it)", got)
	}
	// Wrapper-output PROPAGATION on the real-store path: report's `echo report {{ check }}`
	// exits 0 either way, so outcome alone cannot prove {{check}} resolved — its settled
	// OUTPUT must carry the wrapper's transparent output ("report checked", not the
	// unresolved "report {{ check }}" or an empty splice).
	if got := lumenSettledOutputFor(t, events, "stage/0/report:0"); !strings.Contains(got, "report checked") {
		t.Fatalf("stage/0/report:0 settled output = %q, want it to contain %q (wrapper output propagated)", got, "report checked")
	}
	t.Logf("PROOF check wrapper settled from its exec body; downstream report rendered %q; run.closed pass", "report checked")

	// THE ADVISORY BUDGET pin: the wrapper's node.activated carries duration:"5m" — the raw
	// literal VERBATIM, journal metadata only. Enforcement reads THIS field, never the bead.
	if got := lumenActivatedDuration(t, events, "stage/0/check:0"); got != "5m" {
		t.Fatalf("wrapper stage/0/check:0 node.activated duration = %q, want %q (advisory budget metadata)", got, "5m")
	}
	t.Logf("PROOF advisory budget: stage/0/check:0 node.activated duration=%q (enforcement reads the journal wrapper, never the bead)", "5m")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenSettledOutputFor returns the OUTPUT carried by an activation's outcome.settled — the
// TNK wrapper-output propagation pin (outcome alone cannot prove a {{ref}} splice resolved).
func lumenSettledOutputFor(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Output     string `json:"output"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled payload: %v", err)
		}
		if p.Activation == activation {
			return p.Output
		}
	}
	t.Fatalf("no outcome.settled for activation %q", activation)
	return ""
}

// lumenActivatedDuration returns the advisory duration carried by a timeout wrapper's
// node.activated (the raw literal string, e.g. "5m") — the TNK advisory-budget journal pin.
func lumenActivatedDuration(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Duration   string `json:"duration"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		if p.Activation == activation {
			return p.Duration
		}
	}
	t.Fatalf("no node.activated for activation %q", activation)
	return ""
}
