//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func drainConvoyIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "drain-convoy.lumen.json")
}

// createDrainConvoyMember creates one work-item bead and returns its id.
func createDrainConvoyMember(t *testing.T, cityDir, title string) string {
	t.Helper()
	out, err := bdDolt(cityDir, "create", "--json", title)
	if err != nil {
		t.Fatalf("bd create %q failed: %v\noutput: %s", title, err, out)
	}
	var b graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &b); err != nil {
		t.Fatalf("unmarshal created member %q: %v\njson: %s", title, err, out)
	}
	if b.ID == "" {
		t.Fatalf("bd create %q returned empty id\njson: %s", title, out)
	}
	return b.ID
}

// TestLumenDrainConvoyDoltE2E_FansOneWorkBeadPerMember (convoy-drain input-set binding,
// §13 residue #1) proves the whole P0 slice on a real Dolt city: a PRE-EXISTING convoy of
// three work-item beads is drained by `gc lumen sling <route> drain-convoy.lumen.json
// --input-convoy members=<convoyID>`. The sling resolves the convoy to a canonically
// sorted member-id array, seeds it as the run input, and the already-landed `for-each
// over: input.members` member arm fans one `run impl` (= one `do work`) PER id.
//
// It pins: (1) run.closed pass; (2) exactly three dispatch facts — one work bead per
// member — each a distinct store-minted bead single-claimed at its '/'-bearing member
// activation fanout/<i>/work:0; (3) each member sub-do's prompt renders its OWN convoy
// member id (index i binds the i-th SORTED member id — the frozen, canonical snapshot);
// (4) the three member aggregates (fanout/<i>:0, transparent run seals) AND the fan
// aggregate all seal pass; (5) each work bead is queryable closed/pass in the work store;
// (6) zero control beads (the implicit v2 control-dispatcher stays inert); (7)
// graphstore.Verify clean — the hash-chained journal is the durable-determinism proof at
// this level (a crash-injected byte-identical resume is pinned at the engine level in
// internal/lumen/engine/crash_test.go + drain_convoy_fixture_test.go; it is not
// reproducible through the CLI harness).
//
// Seal budget: one concurrent fan of three do members ≈ 8-minute seal wait, -timeout 1200s.
func TestLumenDrainConvoyDoltE2E_FansOneWorkBeadPerMember(t *testing.T) {
	// 3 workers so the three member sub-dos claim CONCURRENTLY in separate pooled sessions.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	// A pre-existing convoy of three work-item beads (the build-from-convoy shape).
	m1 := createDrainConvoyMember(t, cityDir, "drain member one")
	m2 := createDrainConvoyMember(t, cityDir, "drain member two")
	m3 := createDrainConvoyMember(t, cityDir, "drain member three")

	convoyOut, err := gcDolt(cityDir, "convoy", "create", "Drain build-from-convoy", m1, m2, m3, "--json")
	if err != nil {
		t.Fatalf("gc convoy create failed: %v\noutput: %s", err, convoyOut)
	}
	var created graphConvoyCreateResult
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(convoyOut))), &created); err != nil {
		t.Fatalf("unmarshal created convoy: %v\njson: %s", err, convoyOut)
	}
	if created.ConvoyID == "" {
		t.Fatalf("gc convoy create returned empty convoy id\njson: %s", convoyOut)
	}

	// The sling freezes membership into the run input, sorted by ID ascending, so member
	// index i binds the i-th sorted id.
	sortedIDs := []string{m1, m2, m3}
	sort.Strings(sortedIDs)

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, drainConvoyIRPath(t),
		"--input-convoy", "members="+created.ConvoyID)
	if err != nil {
		t.Fatalf("gc lumen sling (drain-convoy) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF drain-convoy streamID = %s (convoy %s, members %v)", streamID, created.ConvoyID, sortedIDs)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)

	// (1) The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// (2) Exactly three dispatch facts — one work bead per member — each distinct.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 3 {
		t.Fatalf("owned.admitted count = %d, want 3 (one dispatch per convoy member)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	byHandle := map[string]lumenOwnedAdmitted{}
	seenBead := map[string]bool{}
	for _, a := range admits {
		oa := decodeOwnedAdmitted(t, a.Payload)
		byHandle[oa.Handle] = oa
		if oa.BeadID == "" || seenBead[oa.BeadID] {
			t.Fatalf("member sub-do bead id %q is empty or duplicated — each member must be one distinct single-claimed work bead", oa.BeadID)
		}
		seenBead[oa.BeadID] = true
	}

	// (3) + (4) Per member (index i = the i-th SORTED convoy id): the sub-do settled pass,
	// its prompt rendered its OWN member id, and the member aggregate sealed pass.
	for i, memberID := range sortedIDs {
		act := "fanout/" + itoaE2E(i) + "/work:0"
		oa, ok := byHandle[act]
		if !ok {
			t.Fatalf("no dispatch fact for member activation %q; have %v", act, keysOfAdmits(byHandle))
		}
		_ = oa
		if got := outcomeSettledFor(t, events, act); got != engine.OutcomePass {
			t.Fatalf("outcome.settled %s = %q, want pass", act, got)
		}
		prompt := lumenActivatedPrompt(t, events, act)
		if !strings.Contains(prompt, memberID) {
			t.Fatalf("member %d prompt = %q, want it to render its convoy member id %q (frozen sorted snapshot)", i, prompt, memberID)
		}
		if got := outcomeSettledFor(t, events, "fanout/"+itoaE2E(i)+":0"); got != engine.OutcomePass {
			t.Fatalf("member %d aggregate fanout/%d:0 = %q, want pass (transparent run seal)", i, i, got)
		}
	}
	if got := outcomeSettledFor(t, events, "fanout:0"); got != engine.OutcomePass {
		t.Fatalf("fan aggregate fanout:0 = %q, want pass", got)
	}
	t.Logf("PROOF one work bead per member, each single-claimed; member + fan aggregates pass; run.closed pass")

	// (5) Every member sub-do bead is queryable closed/pass in the work store.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	for i, memberID := range sortedIDs {
		act := "fanout/" + itoaE2E(i) + "/work:0"
		b, has := byActivation[act]
		if !has {
			t.Fatalf("member sub-do bead not queryable at %q: have %v", act, keysOfBeads(byActivation))
		}
		if beadStatus(b) != "closed" || metaValue(b, beadmetaOutcomeKey) != "pass" {
			t.Fatalf("member %d bead %s (%s) = {status:%q outcome:%q}, want {closed, pass}", i, b.ID, memberID, beadStatus(b), metaValue(b, beadmetaOutcomeKey))
		}
	}

	// (6) + (7) Zero control beads; Verify clean.
	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// itoaE2E renders a single-digit member index (this fan has three members).
func itoaE2E(i int) string { return string(rune('0' + i)) }
