//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// L6 pack-dogfood e2e's. Unlike the per-kind acceptance e2e's (lumen_*_dolt_e2e),
// these sling the REAL compiled gas-city core-pack conversions from the L6 dogfood
// (copied into examples/lumen/pack-*.lumen.json) and drive them to seal on a live
// dolt city. They prove the pack ports run as whole formulas, not just the isolated
// kind fixtures. Same dolt harness (setupLumenDoDoltCity); file-provider can't do
// cross-process work-bead claiming.
//
// NOTE on the conversion: mol-review-quorum's TOML `synthesize` step (needs both
// lanes) is a REAL agent step, so the faithful POOLED conversion makes it a top-level
// pooled do that auto-chains after the scatter — NOT a `gather { collect { … } }`
// combine. A gather-combine do runs INLINE in an in-process Host (allowCombineDo),
// which the pooled real-bead controller lacks, so the gather form POOL-FAILs at the
// enqueue gate (buildUnits(true,false)). See L6-dogfood-catalog.md §"pooled vs host".

func packReviewQuorumIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "pack-review-quorum.lumen.json")
}

func packReviewQuorumRoutedIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "pack-review-quorum-routed.lumen.json")
}

// reviewQuorumInput is the required-input contract of the pooled mol-review-quorum
// conversion: the two reviewer-lane ids. base_ref defaults; convoy_id is optional.
const reviewQuorumInput = `{"convoy_id":"gcg-c1","lane_one_id":"L1","lane_two_id":"L2"}`

// TestLumenPackReviewQuorumDoltE2E (L6 marquee, single-pool) proves the REAL
// mol-review-quorum pack conversion seals on a live city: `scatter { retry{reviewLaneOne},
// retry{reviewLaneTwo} }` then a top-level `synthesize` do that auto-chains after the
// scatter. Both reviewer lanes dispatch as ordinary pooled work beads (a retry loop is
// a legal scatter member), a native worker claims+closes each pass, the scatter
// aggregate drains, the synthesize do (gated on the scatter) then dispatches and closes,
// and the run seals pass — the whole compiled pack, not a hand-crafted fixture.
func TestLumenPackReviewQuorumDoltE2E(t *testing.T) {
	// 3 workers: the two lanes run concurrently, then synthesize runs after both — three
	// SEQUENTIAL native-demand spawns (lanes, then synthesize on a fresh session after the
	// one-do-per-session workers exit). A larger cap keeps a worker available for each leg
	// so the run does not wait on session turnover, and the seal budget below covers the
	// three-leg dispatch→spawn→work→observe chain on a slow dolt city.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 3, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, packReviewQuorumIRPath(t), "--input", reviewQuorumInput)
	if err != nil {
		t.Fatalf("gc lumen sling (pack-review-quorum) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF pack-review-quorum streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 8*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	// Both reviewer lane bodies (retry attempt 0) settled pass on the pool.
	if got := outcomeSettledFor(t, events, "reviewLaneOne:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled reviewLaneOne:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "reviewLaneTwo:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled reviewLaneTwo:0 = %q, want pass", got)
	}
	// The scatter aggregated both retry-loop lanes → pass.
	if got := outcomeSettledFor(t, events, "lanes:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled lanes:0 = %q, want pass (scatter over two retry-do lanes)", got)
	}
	// The synthesize do (gated on the scatter, needs both lanes) dispatched + settled pass.
	if got := outcomeSettledFor(t, events, "synthesize:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled synthesize:0 = %q, want pass (top-level do after the scatter)", got)
	}
	// Three work beads dispatched: two lanes + the synthesize.
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 3 {
		t.Fatalf("owned.admitted count = %d, want 3 (two lanes + synthesize)", n)
	}
	t.Logf("PROOF pack-review-quorum: both lanes + synthesize sealed pass; scatter drained; run.closed pass")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}

// setupLumenRoutedQuorumCity builds a DOLT-backed city with THREE pool agents —
// laneOneAgent, laneTwoAgent, and the sling default (lumenDoRoute) — each running the
// scripted lumen-do worker. It mirrors setupLumenDoDoltCity but with the extra pools
// the routed marquee needs: each reviewer lane's work bead routes to its OWN agent's
// pool (gc.routed_to = the agentRef), so only that pool's worker can claim it, while
// the unbound synthesize routes to the default. Returns the city dir.
func setupLumenRoutedQuorumCity(t *testing.T, maxActive int) string {
	t.Helper()
	env := newIsolatedCommandEnv(t, true)

	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)

	nonceEnv := "GC_LUMEN_E2E_NONCE=" + lumenKillNonce(cityName)
	startCommand := nonceEnv + " GC_LUMEN_E2E_WORK_SECONDS=2 bash " + agentScript("lumen-do.sh")

	// Three plain pool agents (the two lane agents + the sling default), subprocess
	// sessions, fast patrol, dolt/default backend. NO named_session / min_active: the
	// only spawn cause is a dispatched work bead's native demand, per pool.
	agentBlock := func(name string) string {
		return fmt.Sprintf("\n[[agent]]\nname = %q\nmax_active_sessions = %d\nstart_command = %q\n", name, maxActive, startCommand)
	}
	cityToml := fmt.Sprintf("[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\npatrol_interval = \"100ms\"\n", cityName) +
		agentBlock(lumenDoRoute) + agentBlock("laneOneAgent") + agentBlock("laneTwoAgent")

	configPath := filepath.Join(t.TempDir(), "lumen-routed-quorum.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing routed-quorum config: %v", err)
	}

	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	if out, err := runGCDoltWithEnv(env, cityDir, "migrate", "graph-journal", "init"); err != nil {
		t.Fatalf("gc migrate graph-journal init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCDoltWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			cleanupTestCityDir(cityDir)
			if _, err := os.Stat(cityDir); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	return cityDir
}

// TestLumenPackReviewQuorumRoutedDoltE2E (L6 marquee, THE multi-agent proof) proves
// per-lane agent routing end-to-end on a live city: the routed mol-review-quorum
// conversion binds reviewLaneOne `with laneOneAgent` and reviewLaneTwo `with
// laneTwoAgent`, so each lane's real work bead is routed (gc.routed_to) to its OWN pool
// and can only be claimed+closed by that pool's worker; the unbound synthesize routes to
// the sling default. Distinct reviewer agents per lane, one synthesizer, all sealing pass
// — the whole multi-agent-pack thesis on real beads.
func TestLumenPackReviewQuorumRoutedDoltE2E(t *testing.T) {
	cityDir := setupLumenRoutedQuorumCity(t, 3)
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, packReviewQuorumRoutedIRPath(t), "--input", reviewQuorumInput)
	if err != nil {
		t.Fatalf("gc lumen sling (pack-review-quorum-routed) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF pack-review-quorum-routed streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 8*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	for _, act := range []string{"reviewLaneOne:0", "reviewLaneTwo:0", "lanes:0", "synthesize:0"} {
		if got := outcomeSettledFor(t, events, act); got != engine.OutcomePass {
			t.Fatalf("outcome.settled %s = %q, want pass", act, got)
		}
	}

	// THE routing proof: each lane's real work bead carries gc.routed_to = its bound
	// agent; the unbound synthesize carries the sling default. Map activation → bead id
	// from the journal's owned.admitted events (the source of truth), then read each
	// bead's gc.routed_to with an ordinary per-bead `bd show`. We deliberately avoid
	// `bd list --all`, whose count query hits an environmental lease-column schema skew
	// on some dolt builds; per-bead `bd show` is unaffected.
	beadByActivation := map[string]string{}
	for _, e := range lumenEventsOfType(events, engine.EventOwnedAdmitted) {
		a := decodeOwnedAdmitted(t, e.Payload)
		beadByActivation[a.Activation] = a.BeadID
	}
	wantRoute := map[string]string{
		"reviewLaneOne:0": "laneOneAgent",
		"reviewLaneTwo:0": "laneTwoAgent",
		"synthesize:0":    lumenDoRoute,
	}
	for act, want := range wantRoute {
		beadID, ok := beadByActivation[act]
		if !ok {
			t.Fatalf("no owned.admitted for activation %s (have %d admits)", act, len(beadByActivation))
		}
		bead, err := tryShowBead(cityDir, beadID)
		if err != nil {
			t.Fatalf("bd show %s (activation %s): %v", beadID, act, err)
		}
		if got := metaValue(bead, "gc.routed_to"); got != want {
			t.Fatalf("activation %s (bead %s) routed_to = %q, want %q (per-lane agent binding)", act, beadID, got, want)
		}
	}
	t.Logf("PROOF per-lane routing: reviewLaneOne→laneOneAgent, reviewLaneTwo→laneTwoAgent, synthesize→%s; run.closed pass", lumenDoRoute)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}
