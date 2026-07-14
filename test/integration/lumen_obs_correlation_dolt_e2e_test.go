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

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumenrunproj"
	"github.com/gastownhall/gascity/internal/runproj"
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
// It also pins P5-OBS.3: the same seal emits a run.resolved event on events.jsonl
// carrying the run root + outcome (the molecule.resolved analog for a Lumen run).
//
// It pins: run.closed pass; the work bead's gc.root_bead_id/gc.step_id; the events.jsonl
// bead envelope; the run.resolved run-lifecycle event; zero control beads; Verify clean.
// Mirrors lumen_dispatch_metadata_dolt_e2e.
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

	// (3) THE RUN-LIFECYCLE PROOF (P5-OBS.3): the controller's seal emits a
	// run.resolved event onto .gc/events.jsonl carrying the run root + aggregated
	// outcome — the molecule.resolved analog a Lumen run otherwise lacks (no molecule
	// root to autoclose). Consumers (order-completion detection, honesty-gate) key on
	// root_id. The emit lands in the same advanceLumenRun tick as the journal seal, so
	// poll briefly for the events.jsonl flush.
	rootID, outcome := waitForLumenRunResolvedOrDiag(t, cityDir, streamID, time.Minute)
	if rootID != streamID {
		t.Fatalf("run.resolved payload root_id = %q, want the run streamID %q", rootID, streamID)
	}
	if outcome != engine.OutcomePass {
		t.Fatalf("run.resolved payload outcome = %q, want pass", outcome)
	}
	t.Logf("PROOF events.jsonl run.resolved for %s carries root_id=%s outcome=%s", streamID, rootID, outcome)

	// (4) THE DASHBOARD-TIMELINE PROOF (P5-OBS.4): the sealed run's REAL Dolt
	// journal + REAL do beads project into a dashboard run lane and a detail graph
	// via internal/lumenrunproj — the gc dashboard timeline, with zero frontend
	// change. This exercises the real FoldRunView → synthetic beads → runproj
	// path on a live sealed journal (what unit tests with hand-built views cannot).
	doBeads := lumenDoBeadsForProjection(byActivation)
	proj := lumenrunproj.New()
	defer func() { _ = proj.Close() }()

	lanes, err := proj.SummaryLanes(ctx, "lumene2e", cityDir, doBeads)
	if err != nil {
		t.Fatalf("lumenrunproj SummaryLanes: %v", err)
	}
	var lane *runproj.RunLane
	for i := range lanes {
		if lanes[i].ID == streamID {
			lane = &lanes[i]
		}
	}
	if lane == nil {
		ids := make([]string, len(lanes))
		for i := range lanes {
			ids[i] = lanes[i].ID
		}
		t.Fatalf("no Lumen lane for stream %s; got %v", streamID, ids)
	}
	if lane.Formula.Status != "known" {
		t.Fatalf("Lumen lane formula = %+v, want a known formula", lane.Formula)
	}
	t.Logf("PROOF lumenrunproj SummaryLanes surfaced lane %s (formula %s, phase %s)", lane.ID, lane.Formula.Name, lane.Phase)

	detail, isLumen, err := proj.Detail(ctx, "lumene2e", cityDir, streamID, doBeads)
	if err != nil {
		t.Fatalf("lumenrunproj Detail: %v", err)
	}
	if !isLumen {
		t.Fatalf("Detail(%s) reported not-a-Lumen-run for a real sealed Lumen stream", streamID)
	}
	if detail.RunID != streamID {
		t.Fatalf("detail.RunID = %q, want %q", detail.RunID, streamID)
	}
	var greet *runproj.RunDisplayNode
	for i := range detail.Nodes {
		if detail.Nodes[i].SemanticNodeID == lumenDoNodeID {
			greet = &detail.Nodes[i]
		}
	}
	if greet == nil {
		ids := make([]string, len(detail.Nodes))
		for i := range detail.Nodes {
			ids[i] = detail.Nodes[i].SemanticNodeID
		}
		t.Fatalf("no display node %q in detail; got %v", lumenDoNodeID, ids)
	}
	if len(greet.ExecutionInstances) == 0 {
		t.Fatalf("display node %q has no execution instances (the do-bead join is dark)", lumenDoNodeID)
	}
	t.Logf("PROOF lumenrunproj Detail(%s): node %q with %d execution instance(s), %d edge(s)",
		streamID, lumenDoNodeID, len(greet.ExecutionInstances), len(detail.Edges))

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenDoBeadsForProjection converts the e2e's queried run beads into the
// beads.Bead shape lumenrunproj joins against (the do-bead status/session
// overlay), flattening the string-valued metadata the fold projection reads.
func lumenDoBeadsForProjection(m map[string]graphBead) []beads.Bead {
	out := make([]beads.Bead, 0, len(m))
	for _, gb := range m {
		md := beads.StringMap{}
		for k, v := range gb.Metadata {
			if s, ok := v.(string); ok {
				md[k] = s
			}
		}
		typ := gb.Type
		if typ == "" {
			typ = gb.IssueType
		}
		out = append(out, beads.Bead{
			ID:       gb.ID,
			Title:    gb.Title,
			Status:   gb.Status,
			Type:     typ,
			Metadata: md,
		})
	}
	return out
}

// waitForLumenRunResolvedOrDiag polls .gc/events.jsonl until a run.resolved event
// for streamID appears (P5-OBS.3 emits it at the controller's seal, in the same tick
// as the journal run.closed, so it lands a beat after the seal wait returns) and
// returns its payload (root_id, outcome). It fails loud with the event log on timeout.
func waitForLumenRunResolvedOrDiag(t *testing.T, cityDir, streamID string, timeout time.Duration) (rootID, outcome string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		if r, o, found := lumenRunResolvedEvent(t, cityDir, streamID); found {
			return r, o
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
			t.Fatalf("no run.resolved event for stream %s within %s\nevents.jsonl:\n%s", streamID, timeout, string(data))
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// lumenRunResolvedEvent scans .gc/events.jsonl for the run.resolved event whose
// subject is streamID and returns its payload (root_id, outcome) and whether found.
func lumenRunResolvedEvent(t *testing.T, cityDir, streamID string) (rootID, outcome string, found bool) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(cityDir, ".gc", "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return "", "", false
		}
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
			Payload struct {
				RootID  string `json:"root_id"`
				Outcome string `json:"outcome"`
			} `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			continue // tolerate a partially-flushed trailing line
		}
		if e.Type == "run.resolved" && e.Subject == streamID {
			return e.Payload.RootID, e.Payload.Outcome, true
		}
	}
	return "", "", false
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
