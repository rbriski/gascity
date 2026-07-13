//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func dispatchMetadataIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "dispatch-metadata.lumen.json")
}

// TestLumenDispatchMetadataDoltE2E_ContinuationGroupOnBead (ITEM B marquee, §7/§8)
// proves the static-metadata passthrough seam drives end to end on a real Dolt city: a
// do carrying `metadata: { "gc.continuation_group": "main" }` is dispatched, an ordinary
// pooled worker claims and closes it, the run seals pass — and the MINTED WORK BEAD
// carries gc.continuation_group=main (the v2-single-lane affinity vector
// preassignHookContinuationGroup reads at claim), stamped ALONGSIDE the four engine-owned
// routing keys. The node.activated payload carries the same map (observability parity),
// and the reducer never folds it (reducerVersion stays 4 — asserted in the unit tier).
//
// It pins: (1) run.closed pass; (2) exactly ONE dispatch fact whose node.activated payload
// carries metadata.gc.continuation_group=main; (3) the real work bead in the CITY WORK
// store carries gc.continuation_group=main next to gc.routed_to/gc.lumen_run/
// gc.lumen_activation; (4) zero control beads; Verify clean.
//
// Mirrors lumen_do_dolt_e2e_test.go / lumen_timeout_check_dolt_e2e_test.go structure.
// Seal budget: one pooled do ≈ 300s seal wait, -timeout 1200s, ISOLATION.
func TestLumenDispatchMetadataDoltE2E_ContinuationGroupOnBead(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, dispatchMetadataIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling (dispatch-metadata) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF dispatch-metadata streamID = %s", streamID)

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

	// (1) Sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, lumenDoActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass", lumenDoActivation, got)
	}

	// (2) The pool node.activated payload carries the static metadata (observability parity).
	if got := lumenActivatedContinuationGroup(t, events, lumenDoActivation); got != "main" {
		t.Fatalf("node.activated %s metadata.gc.continuation_group = %q, want main", lumenDoActivation, got)
	}
	t.Logf("PROOF node.activated %s carried metadata.gc.continuation_group=main (observability parity)", lumenDoActivation)

	// (3) THE PASSTHROUGH PROOF: the real work bead in the CITY WORK store carries
	// gc.continuation_group=main alongside the engine-owned routing keys.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b, has := byActivation[lumenDoActivation]
	if !has {
		t.Fatalf("do work bead not queryable: have %v, want %s", keysOfBeads(byActivation), lumenDoActivation)
	}
	if b.ID != realBeadID {
		t.Fatalf("work-store bead id %q != dispatch fact %q", b.ID, realBeadID)
	}
	if got := metaValue(b, "gc.continuation_group"); got != "main" {
		t.Fatalf("work bead %s gc.continuation_group = %q, want main (static passthrough onto the claim surface)", b.ID, got)
	}
	if got := metaValue(b, "gc.routed_to"); got != lumenDoRoute {
		t.Fatalf("work bead %s gc.routed_to = %q, want %q (engine key present alongside passthrough)", b.ID, got, lumenDoRoute)
	}
	if got := metaValue(b, "gc.lumen_run"); got != streamID {
		t.Fatalf("work bead %s gc.lumen_run = %q, want %q", b.ID, got, streamID)
	}
	t.Logf("PROOF work bead %s carries gc.continuation_group=main next to the engine-owned routing keys", b.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenActivatedContinuationGroup returns the gc.continuation_group carried by an
// activation's node.activated metadata map — the ITEM B observability-parity read.
func lumenActivatedContinuationGroup(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string            `json:"activation"`
			Metadata   map[string]string `json:"metadata"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		if p.Activation == activation {
			return p.Metadata["gc.continuation_group"]
		}
	}
	t.Fatalf("no node.activated for activation %q", activation)
	return ""
}
