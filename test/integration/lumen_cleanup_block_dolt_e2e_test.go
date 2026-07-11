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

// Dolt-backed cleanup-over-block (CUB) e2e. It is the acceptance gate for the
// generalized cleanup whose `guarded` is a multi-step BLOCK: the block's leaf do's are
// inlined as ORDINARY pooled work beads (BARE ids, parented to a synthetic transparent
// drain aggregate), drained in their authored chain order by native pooled workers, and
// only AFTER the aggregate settles does the always-run finally teardown dispatch. Nothing
// Lumen-specific drives the loop — the same native pool machinery that closes any routed
// work bead seals it. Mirrors TestLumenCleanupDoltE2E_BodyRunsThenSeals (the single-leaf
// guarded cleanup) for the block guarded shape.

func cleanupBlockDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "cleanup-block-do.lumen.json")
}

// TestLumenCleanupBlockDoltE2E_BlockRunsThenFinallySeals proves a cleanup whose guarded
// is a 3-do block (stepA→stepB→stepC, auto-chained) drives every block step as an ordinary
// pooled work bead in chain order, then — after the drain aggregate settles — ALWAYS runs
// the finally teardown do, then seals pass. Four owned.admitted (three block members + the
// finally) proves the block drained AND the finally ran; the cleanup settles transparently
// from the block's last-ran step; zero control beads; Verify clean.
func TestLumenCleanupBlockDoltE2E_BlockRunsThenFinallySeals(t *testing.T) {
	// 3 workers: the block drains stepA→stepB→stepC SEQUENTIALLY and the finally dispatches
	// only after the aggregate settles, so a worker can go idle between steps and be swept
	// before the next dispatches; extra pooled capacity keeps a claimant available.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, cleanupBlockDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling (cleanup-block-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF cleanup-block-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	// Seal budget 13 minutes: four SEQUENTIAL claim/close legs (three block steps + the
	// finally), each a native spawn→claim→2s-work→close→observe round that runs ≈2min on a
	// dolt city, on top of ≈1min controller startup. Forensics from the first run's journal
	// timestamps: stepA settle +127s, stepB +114s, stepC +190s — then the whole post-drain
	// agg→cleanup→teardown-dispatch cascade landed in 6ms. The engine is instant; the
	// city's claim cadence dominates, so the budget must cover ~4 legs × ~2min + startup.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 13*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Every block step ran and the finally teardown ran (the always-run edge); the cleanup
	// settled transparently from the block's last-ran step.
	for _, act := range []string{"stepA:0", "stepB:0", "stepC:0", "teardown:0", "cleanup_1:0"} {
		if got := outcomeSettledFor(t, events, act); got != engine.OutcomePass {
			t.Fatalf("outcome.settled %s = %q, want pass", act, got)
		}
	}

	// Four dispatch facts: three block members + one finally. The drain aggregate and the
	// cleanup settle inline (no pool dispatch), so they contribute no owned.admitted.
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 4 {
		t.Fatalf("owned.admitted count = %d, want 4 (3 block members + finally)", n)
	}
	t.Logf("PROOF cleanup block drained stepA→stepB→stepC as ordinary pooled work, then always-ran the finally, then sealed pass")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}
