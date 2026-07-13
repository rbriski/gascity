//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// Dolt-backed v1 AGENT-DRIVEN STEPPER e2e (open decision 8). This is the gate the v1
// self-drive shape rests on: `gc lumen sling --v1` mints ONE ordinary fold_owned=0 work
// bead; native pool demand spawns ONE session; the driver-loop agent (test/agents/
// lumen-v1-driver.sh) claims it and drives the linear-3-do run turn by turn IN THAT ONE
// SESSION via `gc lumen step`/`gc lumen settle` (no per-do beads, no continuation group,
// no sub-session spawns); the run seals and the run-bead closes with the aggregated
// gc.outcome. The controller NEVER drives the run (Driver=="self" is skipped by
// lumenRunsTick), yet the run completes — SDK self-sufficiency.
//
// It mirrors lumen_do_dolt_e2e_test.go (the pool real-bead harness) + the driver-loop
// worker-script pattern of lumen-do-chain.sh. A DOLT city is required for the same reason
// the pool e2e needs one: the controller's demand loop and the worker's `gc hook --claim`
// / `gc bd` must share ONE backend. The graph journal itself stays sqlite (journal.db) and
// is written by BOTH the controller's list pass and the worker's `gc lumen step`/`settle`
// — concurrent sqlite handles fenced per-stream by the writer lease.

// linearThreeDoIRPath is the committed linear 3-do fixture the v1 e2e slings.
func linearThreeDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "linear-three-do.lumen.json")
}

// v1LinearScript is the scripted outcome/output per do node the driver reports (the EMIT=
// token in each prompt) — the determinism oracle's script.
var v1LinearScript = map[string][2]string{
	"produce":   {engine.OutcomePass, "v1"},
	"transform": {engine.OutcomePass, "v2"},
	"summarize": {engine.OutcomePass, "v3"},
}

// v1LinearWantTypes is the canonical linear-3-do stepper journal sequence — identical,
// event-type for event-type, to a synchronous engine.Run of the same formula.
var v1LinearWantTypes = []string{
	engine.EventRunStarted,
	engine.EventNodeActivated, engine.EventEffectScheduled, engine.EventEffectSettled, engine.EventOutcomeSettled,
	engine.EventNodeActivated, engine.EventEffectScheduled, engine.EventEffectSettled, engine.EventOutcomeSettled,
	engine.EventNodeActivated, engine.EventEffectScheduled, engine.EventEffectSettled, engine.EventOutcomeSettled,
	engine.EventRunClosed,
}

// TestLumenV1StepperDoltE2E_SelfDriveSeal (the crux) proves the whole v1 slice end-to-end:
// `gc lumen sling --v1` on a DOLT city mints one driver bead → native demand spawns ONE
// session → the driver-loop agent claims it and self-drives the linear-3-do run via
// step/settle → the run seals pass, values plumb (v1→v2→v3), and the run-bead closes
// gc.outcome=pass. The controller never advances the run (Driver=self). The final fold
// facts equal a genesis engine.Run scripting the same outcomes — the determinism oracle.
func TestLumenV1StepperDoltE2E_SelfDriveSeal(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-v1-driver.sh", 1, "")
	ctx := context.Background()

	// (1) Enqueue a v1 run: mints the ordinary driver bead, stamps run.started Driver=self.
	slingOut, err := gcDolt(cityDir, "lumen", "sling", "--v1", lumenDoRoute, linearThreeDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling --v1 failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	driverBeadID := parseV1DriverBeadID(t, slingOut)
	t.Logf("PROOF v1 run streamID=%s driver-bead=%s", streamID, driverBeadID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// (2) Native-demand spawn: with min=0 and no named session, the ONLY cause a session
	// exists is the born-claimable driver bead.
	claimant := waitForPooledSession(t, cityDir, lumenDoRoute, 3*time.Minute)
	t.Logf("PROOF native-demand spawned pooled driver session %q", claimant.SessionName)

	// (3) The agent self-drives to the seal — NO controller advance, NO per-do work beads.
	events := waitForLumenSeal(t, gs, streamID, 4*time.Minute)

	// (3a) Exactly the linear-3-do stepper journal sequence — byte-for-byte the event types
	// a synchronous engine.Run of the same formula writes.
	types := lumenStreamTypes(events)
	if !equalStrings(types, v1LinearWantTypes) {
		t.Fatalf("v1 journal sequence = %v\nwant %v", types, v1LinearWantTypes)
	}
	// (3b) ZERO per-do work beads dispatched (v1 has no owned.admitted — the agent IS the
	// executor, not a pool dispatch).
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 0 {
		t.Fatalf("owned.admitted appeared %d× — a v1 run dispatches NO per-do work beads", n)
	}

	// (4) Per-node settled facts: outcome pass, values plumbed produce=v1 → transform=v2 →
	// summarize=v3 (the {{ref}} chain resolved because the agent passes --output directly).
	facts := v1SettledFacts(t, events)
	for node, want := range v1LinearScript {
		got, ok := facts[node]
		if !ok {
			t.Fatalf("do %q never settled (facts: %v)", node, facts)
		}
		if got[0] != want[0] || got[1] != want[1] {
			t.Fatalf("do %q settled = {outcome:%q output:%q}, want {%q %q}", node, got[0], got[1], want[0], want[1])
		}
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// (5) DETERMINISM ORACLE: a genesis engine.Run of the SAME formula with a StubHost
	// scripting the same outcomes folds to the SAME settled facts and the SAME event-type
	// sequence — v1 self-drive IS engine.Run with the agent substituted for the host.
	oracleFacts, oracleTypes := v1GenesisOracle(t)
	if !equalStrings(types, oracleTypes) {
		t.Fatalf("v1 sequence != genesis engine.Run sequence:\n v1: %v\n gen:%v", types, oracleTypes)
	}
	for node, want := range oracleFacts {
		if facts[node] != want {
			t.Fatalf("determinism oracle mismatch for %q: v1=%v genesis=%v", node, facts[node], want)
		}
	}
	t.Logf("PROOF v1 stepper journal == genesis engine.Run (types + per-node settled facts)")

	// (6) The run-bead closed with the aggregated gc.outcome=pass (the driver's final act).
	assertV1DriverBeadClosed(t, cityDir, driverBeadID, streamID, engine.OutcomePass)

	// (7) The cross-process append chain (controller list pass + the worker's gc lumen
	// step/settle processes + this reader) verifies.
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean", streamID)
}

// TestLumenV1StepperDoltE2E_KillMidRunResumes (the kill/adopt leg) proves recovery is
// gascity's for free: a driver session is SIGKILLed BETWEEN turns (after settling the
// first do, during the deterministic step pause); gascity's ORDINARY orphan-release
// reopens the run-bead; a FRESH driver session re-claims and RESUMES the run from the
// journal (re-stepping the surviving state) to the seal — with no Lumen-specific recovery.
// The final fold facts equal a genesis engine.Run of the same scripted outcomes.
func TestLumenV1StepperDoltE2E_KillMidRunResumes(t *testing.T) {
	// A deterministic 6s between-turn pause opens the kill window; maxActive=1 so the
	// reopened bead is re-claimed by a restarted session of the same pool.
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-v1-driver.sh", 1, "GC_LUMEN_V1_STEP_SLEEP=6")
	ctx := context.Background()
	nonce := lumenKillNonce(filepath.Base(cityDir))

	slingOut, err := gcDolt(cityDir, "lumen", "sling", "--v1", lumenDoRoute, linearThreeDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling --v1 failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	driverBeadID := parseV1DriverBeadID(t, slingOut)
	t.Logf("PROOF v1 run streamID=%s driver-bead=%s", streamID, driverBeadID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// (1) Wait until the FIRST do (produce) has settled — the driver made durable progress
	// and is now in its deterministic between-turn pause: a clean point to kill.
	waitForV1Settled(t, gs, streamID, "produce:0", 3*time.Minute)
	t.Logf("PROOF first do settled; driver paused between turns")

	// (2) SINGLE SIGKILL of the paused driver session (a process-table query, no PID files).
	pids := waitForNoncePID(t, nonce, 30*time.Second)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Fatalf("SIGKILL %d: %v", pid, err)
		}
	}
	t.Logf("PROOF single SIGKILL of the mid-run driver (%d pid)", len(pids))

	// (3) Recovery: orphan-release reopens the SAME run-bead, a fresh driver session
	// re-claims it and RESUMES the run from the journal (re-steps produce as settled, drives
	// transform + summarize) to the seal — no test assistance after the one kill.
	events := waitForLumenSeal(t, gs, streamID, 5*time.Minute)
	types := lumenStreamTypes(events)
	if !equalStrings(types, v1LinearWantTypes) {
		t.Fatalf("resumed v1 sequence = %v\nwant %v (produce survived, transform+summarize by the fresh session)", types, v1LinearWantTypes)
	}

	// (4) The resumed run's facts equal the genesis oracle — resume is byte-identical.
	facts := v1SettledFacts(t, events)
	oracleFacts, _ := v1GenesisOracle(t)
	for node, want := range oracleFacts {
		if facts[node] != want {
			t.Fatalf("resumed run fact mismatch for %q: got=%v genesis=%v", node, facts[node], want)
		}
	}
	assertV1DriverBeadClosed(t, cityDir, driverBeadID, streamID, engine.OutcomePass)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) after resume failed: %v", streamID, err)
	}
	t.Logf("PROOF v1 run resumed across a mid-run SIGKILL to a byte-identical seal (SDK self-sufficiency)")
}

// --- v1 stepper e2e helpers --------------------------------------------------

// parseV1DriverBeadID extracts the driver bead id from `gc lumen sling --v1` output
// ("… driver bead <id>)").
func parseV1DriverBeadID(t *testing.T, slingOut string) string {
	t.Helper()
	marker := "driver bead "
	i := strings.Index(slingOut, marker)
	if i < 0 {
		t.Fatalf("could not find a driver bead id in sling output: %q", slingOut)
	}
	rest := slingOut[i+len(marker):]
	rest = strings.TrimRight(strings.TrimSpace(rest), ")")
	if f := strings.Fields(rest); len(f) > 0 {
		return f[0]
	}
	t.Fatalf("empty driver bead id in sling output: %q", slingOut)
	return ""
}

// v1SettledFacts returns bare-node-id → {outcome, output} for every outcome.settled.
func v1SettledFacts(t *testing.T, events []graphstore.StoredEvent) map[string][2]string {
	t.Helper()
	out := map[string][2]string{}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
			Output     string `json:"output"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		out[engine.ActivationNodeID(p.Activation)] = [2]string{p.Outcome, p.Output}
	}
	return out
}

// v1GenesisOracle runs the SAME linear-3-do formula in-process via a synchronous
// engine.Run with a StubHost scripting the v1LinearScript outcomes, and returns its
// per-node settled facts and event-type sequence — the determinism oracle the real v1
// run must match.
func v1GenesisOracle(t *testing.T) (facts map[string][2]string, types []string) {
	t.Helper()
	ctx := context.Background()
	store, err := graphstore.Open(ctx, filepath.Join(t.TempDir(), "v1-oracle.db"), graphstore.Options{CityID: "oracle"})
	if err != nil {
		t.Fatalf("open oracle store: %v", err)
	}
	defer func() { _ = store.Close() }()

	raw, err := os.ReadFile(linearThreeDoIRPath(t))
	if err != nil {
		t.Fatalf("read fixture IR: %v", err)
	}
	doc, err := ir.Decode(raw)
	if err != nil {
		t.Fatalf("decode fixture IR: %v", err)
	}
	results := map[string]enginehost.DoResult{}
	for node, oc := range v1LinearScript {
		results[node] = enginehost.DoResult{Outcome: oc[0], Output: oc[1]}
	}
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: &enginehost.StubHost{Results: results}})
	if err != nil {
		t.Fatalf("genesis oracle run: %v", err)
	}
	return v1SettledFacts(t, res.Events), lumenStreamTypes(res.Events)
}

// waitForV1Settled blocks until the given activation has an outcome.settled in the journal.
func waitForV1Settled(t *testing.T, gs *graphstore.Store, streamID, activation string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := lumenStreamEvents(t, gs, streamID)
		if outcomeSettledFor(t, events, activation) != "" {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("activation %s never settled within %s\nlast: %v", activation, timeout, lumenStreamTypes(lumenStreamEvents(t, gs, streamID)))
}

// assertV1DriverBeadClosed asserts the ordinary v1 driver run-bead is closed with the
// aggregated gc.outcome and is still linked to the run.
//
// Unlike the pool path — where the controller seals the run (run.closed) only AFTER
// observing the work bead's close, so the bead is already closed the instant
// waitForLumenSeal returns — a v1 run is sealed by the agent's own final `gc lumen settle`
// and the run-bead close is a SEPARATE, trailing act (`gc bd update … --status closed`).
// So the bead close lags the journal seal by one subprocess: poll for it (waitForBeadClosed)
// rather than racing a single read the instant run.closed appears.
func assertV1DriverBeadClosed(t *testing.T, cityDir, beadID, streamID, wantOutcome string) {
	t.Helper()
	bead := waitForBeadClosed(t, cityDir, beadID, 60*time.Second)
	if got := metaValue(bead, "gc.lumen_run"); got != streamID {
		t.Fatalf("v1 driver bead %q gc.lumen_run = %q, want %q", beadID, got, streamID)
	}
	if got := metaValue(bead, "gc.outcome"); got != wantOutcome {
		t.Fatalf("v1 driver bead %q gc.outcome = %q, want %q", beadID, got, wantOutcome)
	}
	t.Logf("PROOF v1 driver bead %s closed, run=%s, outcome=%s", beadID, streamID, wantOutcome)
}
