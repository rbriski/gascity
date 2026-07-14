//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestLumenObsCorrelationDoltE2E_RunAndStepEnvelope (P5-OBS.1 seal) proves the
// observability-correlation stamps drive end to end on a real Dolt city: a plain do is
// dispatched, an ordinary pooled worker claims and closes it, the run seals pass — and
// the two correlation keys land where the cost/session and event planes read them:
//
//	(1) the MINTED WORK BEAD in the city work store carries gc.root_bead_id = the run
//	    streamID and gc.step_id = the BARE node id ("greet"), stamped ALONGSIDE the four
//	    engine-owned routing keys (the claim/demand/orphan path);
//	(2) that bead's already-firing bead.* event on .gc/events.jsonl carries the derived
//	    ENVELOPE run_id = streamID and step_id = "greet" — the notifyChange →
//	    ResolveRunID/StepID path, i.e. the "emit step events correctly" proof. This is the
//	    events-plane half of "match up sessions/costs" and needs zero new event types.
//
// The per-run cost rollup off the SESSION bead (gc.current_run_id in the ResolveRunID
// chain) is P5-OBS.2's seal — the session bead's own event envelope does not resolve to
// the run until that chain fix lands, so it is deliberately NOT asserted here.
//
// It pins: run.closed pass; the work bead's gc.root_bead_id/gc.step_id; the events.jsonl
// envelope; zero control beads; Verify clean. Mirrors lumen_dispatch_metadata_dolt_e2e.
// Seal budget: one pooled do ≈ 300s seal wait, -timeout 1200s, ISOLATION.
func TestLumenObsCorrelationDoltE2E_RunAndStepEnvelope(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, lumenDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling (hello-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF obs-correlation streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	admitted, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Kind != engine.OwnedKindWorkBead || admitted.BeadID == "" {
		t.Fatalf("owned.admitted = {kind:%q bead_id:%q}, want {work_bead, <store id>}", admitted.Kind, admitted.BeadID)
	}
	realBeadID := admitted.BeadID

	events := waitForLumenSealOrDiag(t, gs, streamID, 4*time.Minute, cityDir, realBeadID)

	// (0) Sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, lumenDoActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass", lumenDoActivation, got)
	}

	// (1) THE STAMP PROOF: the real work bead in the CITY WORK store carries
	// gc.root_bead_id = the run streamID and gc.step_id = the bare node id, alongside the
	// engine-owned routing keys. A do's identity is (run, step); the attempt axis stays on
	// gc.lumen_attempt, so gc.step_id is the BARE node id, never the nodeID:attempt activation.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b, has := byActivation[lumenDoActivation]
	if !has {
		t.Fatalf("do work bead not queryable: have %v, want %s", keysOfBeads(byActivation), lumenDoActivation)
	}
	if b.ID != realBeadID {
		t.Fatalf("work-store bead id %q != dispatch fact %q", b.ID, realBeadID)
	}
	if got := metaValue(b, "gc.root_bead_id"); got != streamID {
		t.Fatalf("work bead %s gc.root_bead_id = %q, want the run streamID %q (run-id correlation spine)", b.ID, got, streamID)
	}
	if got := metaValue(b, "gc.step_id"); got != lumenDoNodeID {
		t.Fatalf("work bead %s gc.step_id = %q, want the bare node id %q (NOT the activation %q)", b.ID, got, lumenDoNodeID, lumenDoActivation)
	}
	// The activation axis is untouched — attempt still rides gc.lumen_activation/gc.lumen_attempt.
	if got := metaValue(b, "gc.lumen_activation"); got != lumenDoActivation {
		t.Fatalf("work bead %s gc.lumen_activation = %q, want %q (activation axis unperturbed)", b.ID, got, lumenDoActivation)
	}
	t.Logf("PROOF work bead %s carries gc.root_bead_id=%s + gc.step_id=%s alongside the routing keys", b.ID, streamID, lumenDoNodeID)

	// (2) THE ENVELOPE PROOF: the do bead's bead.* event on events.jsonl carries the
	// derived envelope run_id = streamID and step_id = the bare node id — the events-plane
	// correlation the runs view and usage facts join on, flowing through the EXISTING
	// notifyChange emitter (ResolveRunID over gc.root_bead_id; StepID over gc.step_id).
	runID, stepID := lumenBeadEventEnvelope(t, cityDir, realBeadID)
	if runID != streamID {
		t.Fatalf("events.jsonl bead event for %s: run_id = %q, want the run streamID %q", realBeadID, runID, streamID)
	}
	if stepID != lumenDoNodeID {
		t.Fatalf("events.jsonl bead event for %s: step_id = %q, want the bare node id %q", realBeadID, stepID, lumenDoNodeID)
	}
	t.Logf("PROOF events.jsonl bead event for %s carries envelope run_id=%s step_id=%s", realBeadID, runID, stepID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenBeadEventEnvelope reads the city event log (.gc/events.jsonl) and returns the
// derived (run_id, step_id) envelope of the FIRST bead-lifecycle event whose subject is
// beadID. These envelope fields are stamped by CachingStore.notifyChange from the bead's
// metadata (ResolveRunID / gc.step_id), so every bead.* event for a given bead carries the
// same values — the first is representative. It fails loud if no bead event names the bead.
func lumenBeadEventEnvelope(t *testing.T, cityDir, beadID string) (runID, stepID string) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		t.Fatalf("reading event log: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var e struct {
			Type    string `json:"type"`
			Subject string `json:"subject"`
			RunID   string `json:"run_id"`
			StepID  string `json:"step_id"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a partially-flushed trailing line
		}
		if strings.HasPrefix(e.Type, "bead.") && e.Subject == beadID {
			return e.RunID, e.StepID
		}
	}
	t.Fatalf("no bead.* event for subject %q in events.jsonl:\n%s", beadID, string(data))
	return "", ""
}
