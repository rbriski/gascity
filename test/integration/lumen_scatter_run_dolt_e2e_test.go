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

func scatterRunDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "scatter-run-do.lumen.json")
}

// TestLumenScatterRunDoltE2E (run-in-scatter acceptance) proves a `run` (sub-formula
// call) is a legal scatter member on a real city: `scatter { direct(do), extra(run ->
// subwork{ inner(do) }) }` then `wrap(do)`. The direct lane do and the run member's
// NAMESPACED sub-do (extra/inner) dispatch as ordinary pooled work beads CONCURRENTLY;
// the transparent run aggregate (extra) drains its sub-do and the scatter (lanes) drains
// both members; then the downstream wrap do dispatches after the scatter and the run
// seals. Assertions: run.closed pass; every activation (direct, extra/inner, the
// transparent run member extra, the scatter lanes, wrap) settled pass; exactly 3 owned.
// admitted (direct + inner CONCURRENT, wrap after). Zero control beads; Verify clean.
//
// Seal budget: 2 concurrent legs (direct, extra/inner) + 1 sequential leg (wrap) at
// ~2min/leg + ~1min startup — so a 10-minute seal wait covers the cadence with margin.
func TestLumenScatterRunDoltE2E(t *testing.T) {
	// 3 workers: direct and extra/inner run CONCURRENTLY (two scatter lanes), then wrap
	// runs after the scatter drains — a third worker keeps a lane from being swept idle.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, scatterRunDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling (scatter-run-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF scatter-run-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	// 2 concurrent legs + 1 sequential leg → use a 10-minute seal wait (see the doc note).
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 10*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// The direct do lane, the run member's namespaced sub-do, the transparent run member,
	// the scatter aggregate, and the downstream wrap do all settled pass.
	for _, activation := range []string{"direct:0", "extra/inner:0", "extra:0", "lanes:0", "wrap:0"} {
		if got := outcomeSettledFor(t, events, activation); got != engine.OutcomePass {
			t.Fatalf("outcome.settled %s = %q, want pass", activation, got)
		}
	}

	// Exactly 3 ordinary work beads dispatched: direct + extra/inner (the two scatter
	// lanes, concurrent) + wrap (after the scatter). The transparent run aggregate and the
	// scatter aggregate settle from the fold, dispatching nothing.
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 3 {
		t.Fatalf("owned.admitted count = %d, want 3 (direct + extra/inner + wrap)", n)
	}
	t.Logf("PROOF scatter{do, run->sub{do}} + wrap sealed pass; 3 work beads (2 concurrent lanes + wrap)")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}
