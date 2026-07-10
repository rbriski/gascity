//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// Dolt-backed real-bead do-node e2e's (REDESIGN §7/§8). These are the gate the
// redesign rests on: an ORDINARY pooled worker claims and closes the work bead the
// controller created, and gascity's own pool machinery (native demand, orphan
// release) drives the loop with ZERO Lumen-specific recovery code on the path.
//
// The file-provider Lumen e2e's (lumen_do_e2e_test.go) cannot seal this loop: the
// controller's in-process store and a separate `gc hook --claim` / `gc bd` worker
// process do not share one backend, so an ordinary `bd ready` claim never surfaces
// the dispatched bead. A DOLT city fixes exactly that — the controller writes the
// real bead to the managed Dolt server and the worker's bd (via the shim → real bd)
// reads and closes it against the SAME server, the way test/integration/
// graph_dispatch_test.go proves a pool worker claims an ordinary routed work bead.
// This file mirrors that dolt harness for the Lumen real-bead path.

const (
	// lumenDoNodeID is the do node id in examples/lumen/hello-do.lumen.json and its
	// activation key (a bare do activates as <nodeID>:0).
	lumenDoNodeID     = "greet"
	lumenDoActivation = "greet:0"
	// lumenRepeatBodyNodeID is the repeat body do node id in
	// examples/lumen/hello-do-repeat.lumen.json; attempts activate lane:0, lane:1, …
	lumenRepeatBodyNodeID = "lane"
)

// setupLumenDoDoltCity builds a DOLT-backed Lumen test city: an isolated command env
// with a managed Dolt server (useDolt=true), a city.toml with NO `[beads]
// provider="file"` (the default/dolt backend, so the controller and the worker's bd
// share one server), subprocess sessions, a fast patrol, and ONE pool agent
// (max_active_sessions=maxActive) whose start_command is the scripted agent. It then
// opts the city into the graph-journal scope. No named_session / min_active: the only
// cause a session exists is Lumen demand. Returns the city dir and the isolated env.
func setupLumenDoDoltCity(t *testing.T, agentScriptName string, maxActive int, agentEnv string) (string, []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, true)

	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)

	nonceEnv := "GC_LUMEN_E2E_NONCE=" + lumenKillNonce(cityName)
	startCommand := nonceEnv + " " + agentEnv + " bash " + agentScript(agentScriptName)

	// A plain pool agent, subprocess sessions, fast patrol. NO [beads] provider (the
	// dolt/default backend), NO formula_v2 (the implicit control-dispatcher stays inert
	// — the zero-control-beads claim), NO named_session / min_active (the ONLY spawn
	// cause is the dispatched work bead's native demand).
	cityToml := fmt.Sprintf(
		"[workspace]\nname = %q\n\n[session]\nprovider = \"subprocess\"\n\n[daemon]\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = %q\nmax_active_sessions = %d\nstart_command = %q\n",
		cityName, lumenDoRoute, maxActive, startCommand,
	)
	configPath := filepath.Join(t.TempDir(), "lumen-do-dolt.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing lumen-do dolt config: %v", err)
	}

	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)

	// Opt into the graph-journal scope. The running controller lazily opens the scope
	// on its next tick, so post-start opt-in is safe.
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

	return cityDir, env
}

// TestLumenDoDoltE2E_OrdinaryClaimClose (e2e-A, the crux) proves the whole redesign:
// `gc lumen sling` on a DOLT city → the controller creates a REAL fold_owned=0 work
// bead in the city WORK store (task, routed, prompt in description) → native pool
// demand spawns ONE pooled session → the session claims it via ORDINARY
// `gc hook --claim` (default routed work_query, no Tier-B leg) and closes it via
// ORDINARY `gc bd update … gc.outcome=pass --status closed` → the controller OBSERVES
// the close → the fold settles via outcome.settled → the run seals pass. Journal
// sequence is exactly [run.started, node.activated, owned.admitted(work_bead),
// outcome.settled, run.closed]; ZERO owned.settled (no Tier-B close leg); ZERO control
// beads; graphstore.Verify clean.
func TestLumenDoDoltE2E_OrdinaryClaimClose(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	// (1) Enqueue.
	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, lumenDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// (2) Dispatch fact FIRST: the controller advanced the run and created the real
	// bead. Diagnose loudly if it never came (controller/scope/advance gap vs a pure
	// demand→spawn gap).
	// The controller's first patrol tick can legitimately exceed a minute at startup
	// (before it fires the lumen tick that advances the run), so this is generous.
	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	// (3) Native-demand spawn: with min=0 and no named session, the ONLY cause a
	// session exists is the dispatched work bead.
	claimant := waitForPooledSession(t, cityDir, lumenDoRoute, 90*time.Second)
	t.Logf("PROOF native-demand spawned pooled session %q (session bead %s)", claimant.SessionName, strings.TrimSpace(claimant.ID))

	// (3b) Dispatch fact detail: exactly one owned.admitted, kind=work_bead, carrying
	// the store-minted real bead id (NOT the fold node id).
	admitted := waitForOwnedAdmitted(t, gs, streamID, 60*time.Second)
	if admitted.Kind != engine.OwnedKindWorkBead {
		t.Fatalf("owned.admitted kind = %q, want %q (real-bead path)", admitted.Kind, engine.OwnedKindWorkBead)
	}
	realBeadID := admitted.BeadID
	if realBeadID == "" {
		t.Fatalf("owned.admitted carries no bead_id; the dispatch must record the real bead id")
	}
	if realBeadID == lumenDoNodeID {
		t.Fatalf("dispatched bead id is the fold node id %q — the real bead must be store-minted", realBeadID)
	}
	t.Logf("PROOF owned.admitted kind=%s bead_id=%s (real work bead, store-minted, ordinary claim path)", admitted.Kind, realBeadID)

	// (4) Seal, then assert the exact 5-event real-bead sequence.
	events := waitForLumenSealOrDiag(t, gs, streamID, 4*time.Minute, cityDir, realBeadID)
	types := lumenStreamTypes(events)
	wantSeq := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,
		engine.EventOwnedAdmitted,
		engine.EventOutcomeSettled,
		engine.EventRunClosed,
	}
	if !equalStrings(types, wantSeq) {
		t.Fatalf("journal sequence = %v, want %v", types, wantSeq)
	}
	if n := len(lumenEventsOfType(events, engine.EventOwnedSettled)); n != 0 {
		t.Fatalf("owned.settled appeared %d× — a Tier-B close leg ran; the real-bead path settles via outcome.settled\nsequence: %v", n, types)
	}
	if got := outcomeSettledFor(t, events, lumenDoActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled for %s = %q, want pass", lumenDoActivation, got)
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	t.Logf("PROOF journal sequence = %v (no owned.settled — ordinary claim/close, no Tier-B leg); outcome.settled %s = pass; run.closed pass", types, lumenDoActivation)

	// (5) The dispatched work is a REAL ordinary bead in the CITY WORK store: an
	// ordinary `bd show` resolves it (a journal projection would not), task-typed,
	// closed, routed, run-linked, closed pass by the worker.
	assertLumenRealWorkBeadClosedDolt(t, cityDir, realBeadID, streamID, lumenDoActivation, engine.OutcomePass)

	// (6) The do's fold row is a PLAIN step (no claimable Tier-A doppelganger) and no
	// Tier-A frontier row survives for it — the actionable work is the real bead.
	assertLumenDoFoldRowIsPlainStepDolt(t, journalPath, streamID, lumenDoNodeID)

	// (7) The worker read its prompt off the claim JSON (description), keyed by the
	// REAL store-minted bead id.
	assertLumenPromptReadback(t, cityDir, realBeadID, lumenDoPrompt)

	// (8) ZERO control beads (journal projection + work store + dispatcher lane).
	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)

	// (9) The cross-process append chain (controller + worker gc processes + reader)
	// verifies.
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean", streamID)
}

// TestLumenDoDoltE2E_DeadWorkerOrphanRelease (e2e-B) proves recovery is gascity's for
// free: a claimant is SIGKILLed mid-do; gascity's ORDINARY orphan-release reopens the
// SAME real bead; a FRESH pooled worker claims and completes it; the run seals PASS.
// The Lumen firewall is still present (S3) but must NOT be the recovery mechanism — it
// only sweeps fold-owned Tier-B claimable rows, which the real-bead path never mints,
// so it never fires here (asserted: ZERO owned.settled — the firewall's only action).
func TestLumenDoDoltE2E_DeadWorkerOrphanRelease(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-hang-once.sh", 1, "")
	ctx := context.Background()
	nonce := lumenKillNonce(filepath.Base(cityDir))

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, lumenDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// (1) The real bead is dispatched and a pooled worker claims it. The controller's
	// first patrol tick can legitimately exceed a minute at startup, so the dispatch
	// wait is generous.
	admitted, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir)
	if err != nil {
		t.Fatal(err)
	}
	if admitted.Kind != engine.OwnedKindWorkBead || admitted.BeadID == "" {
		t.Fatalf("owned.admitted = {kind:%q bead_id:%q}, want {work_bead, <store id>}", admitted.Kind, admitted.BeadID)
	}
	realBeadID := admitted.BeadID
	t.Logf("PROOF real bead dispatched: %s", realBeadID)

	// (2) Wait for the first worker to CLAIM the real bead and arm its hang flag before
	// killing. In the real-bead path owned.admitted is the DISPATCH fact (not the
	// worker's claim), and the per-city nonce appears in every session's start_command,
	// so a bare waitForNoncePID could race the claim. The hang-once agent writes the
	// flag file only AFTER it has claimed and just before it execs into the killable
	// sleep — so the flag's existence proves the claim committed and the recovery worker
	// will complete (it sees the flag) rather than hang again.
	flagPath := filepath.Join(cityDir, ".gc", "lumen-e2e-hang-once-done")
	waitForFileExists(t, flagPath, 2*time.Minute)
	t.Logf("PROOF first worker claimed and armed the hang flag")

	// (3) SINGLE SIGKILL of the hung claimant (a process-table query, no PID files).
	pids := waitForNoncePID(t, nonce, 30*time.Second)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Fatalf("SIGKILL %d: %v", pid, err)
		}
	}
	t.Logf("PROOF single SIGKILL of the first claimant (%d pid)", len(pids))

	// (4) Recovery: gascity's orphan-release reopens the SAME bead, a fresh/restarted
	// worker claims and completes it (the flag is armed so it does not hang), the run
	// seals PASS — with no test assistance after the one kill.
	events := waitForLumenSealOrDiag(t, gs, streamID, 4*time.Minute, cityDir, realBeadID)

	// (5) The seal is a plain do (no loop): exactly the 5-event real-bead sequence,
	// exactly ONE owned.admitted (the SAME bead was reopened + re-claimed, NOT a fresh
	// attempt), and — the load-bearing proof — ZERO owned.settled, so the firewall
	// never fired: recovery settled via outcome.settled from the observed close.
	types := lumenStreamTypes(events)
	wantSeq := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,
		engine.EventOwnedAdmitted,
		engine.EventOutcomeSettled,
		engine.EventRunClosed,
	}
	if !equalStrings(types, wantSeq) {
		t.Fatalf("journal sequence = %v, want %v (same-bead reopen, not a fresh attempt)", types, wantSeq)
	}
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (orphan-release reopens the SAME bead — no fresh dispatch)", n)
	}
	if settles := lumenEventsOfType(events, engine.EventOwnedSettled); len(settles) != 0 {
		s := decodeOwnedSettled(t, settles[0].Payload)
		t.Fatalf("owned.settled appeared (%q %q) — the Lumen firewall fired; recovery must be gascity orphan-release via outcome.settled, not a firewall strand", s.Outcome, s.Output)
	}
	if got := outcomeSettledFor(t, events, lumenDoActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled for %s = %q, want pass (fresh worker completed the reopened bead)", lumenDoActivation, got)
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass (orphan-release recovery)", closed.Outcome)
	}
	t.Logf("PROOF recovery via gascity orphan-release: sequence %v; ZERO owned.settled (firewall never fired); run.closed pass", types)

	// (6) The SAME bead was executed by TWO workers (killed one + recovery one): the
	// exec marker keyed by the real bead id has exactly 2 lines. This is the direct
	// evidence the reopened bead — not a fresh attempt bead — was re-claimed.
	assertExecCountLines(t, cityDir, realBeadID, 2)
	t.Logf("PROOF the SAME real bead %s was claimed+executed by two successive workers (orphan-release reopen), not a fresh attempt", realBeadID)

	// (7) The real bead resolves in the WORK store, closed pass, still carrying its
	// original activation (greet:0) — proof the SAME bead recovered.
	assertLumenRealWorkBeadClosedDolt(t, cityDir, realBeadID, streamID, lumenDoActivation, engine.OutcomePass)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean", streamID)
}

// TestLumenDoDoltE2E_RetryFreshBeadVisibility (e2e-retry) proves the fresh-bead-per-
// attempt visibility requirement: a flaky do fails attempt 0, the fold mints a FRESH
// real bead for attempt 1, attempt 1 passes, the run seals PASS — and BOTH attempt
// beads are present + queryable in the WORK store with their outcomes (attempt-0 closed
// fail, attempt-1 closed pass), on two DISTINCT store-minted ids.
func TestLumenDoDoltE2E_RetryFreshBeadVisibility(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-flaky.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, lumenRepeatDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Two attempts take ≥ two claim cycles: sling → dispatch lane:0 → claim → fail →
	// observe → mint lane:1 → dispatch → claim → pass → observe → loop settle → seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 6*time.Minute, cityDir)

	// The run sealed pass.
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// Exactly two dispatch facts (one FRESH bead per attempt), on distinct bead ids
	// bound to distinct activations lane:0 then lane:1.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (one fresh dispatch per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Handle != "lane:0" || a1.Handle != "lane:1" {
		t.Fatalf("owned.admitted handles = {%q, %q}, want {lane:0, lane:1}", a0.Handle, a1.Handle)
	}
	if a0.BeadID == "" || a1.BeadID == "" || a0.BeadID == a1.BeadID {
		t.Fatalf("attempt bead ids = {%q, %q}, want two distinct store-minted ids (fresh bead per attempt)", a0.BeadID, a1.BeadID)
	}
	// The two do outcomes: lane:0 failed → lane:1 pass.
	if got := outcomeSettledFor(t, events, "lane:0"); got != engine.OutcomeFailed {
		t.Fatalf("outcome.settled lane:0 = %q, want failed", got)
	}
	if got := outcomeSettledFor(t, events, "lane:1"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled lane:1 = %q, want pass", got)
	}
	t.Logf("PROOF two fresh dispatch beads: lane:0=%s (fail) then lane:1=%s (pass); run.closed pass", a0.BeadID, a1.BeadID)

	// The VISIBILITY requirement: BOTH attempt beads are queryable in the WORK store
	// with their outcomes. attempt-0 is closed fail, attempt-1 is closed pass.
	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	b0, ok0 := byActivation["lane:0"]
	b1, ok1 := byActivation["lane:1"]
	if !ok0 || !ok1 {
		t.Fatalf("attempt beads not both queryable in the work store: have activations %v, want lane:0 and lane:1", keysOfBeads(byActivation))
	}
	if b0.ID == "" || b1.ID == "" || b0.ID == b1.ID {
		t.Fatalf("attempt bead ids in work store = {%q, %q}, want two distinct ids", b0.ID, b1.ID)
	}
	if b0.ID != a0.BeadID || b1.ID != a1.BeadID {
		t.Fatalf("work-store attempt bead ids {%q, %q} do not match the dispatch facts {%q, %q}", b0.ID, b1.ID, a0.BeadID, a1.BeadID)
	}
	if beadStatus(b0) != "closed" || metaValue(b0, beadmetaOutcomeKey) != "fail" {
		t.Fatalf("attempt-0 bead %s = {status:%q outcome:%q}, want {closed, fail}", b0.ID, beadStatus(b0), metaValue(b0, beadmetaOutcomeKey))
	}
	if beadStatus(b1) != "closed" || metaValue(b1, beadmetaOutcomeKey) != "pass" {
		t.Fatalf("attempt-1 bead %s = {status:%q outcome:%q}, want {closed, pass}", b1.ID, beadStatus(b1), metaValue(b1, beadmetaOutcomeKey))
	}
	if got := metaValue(b0, "gc.lumen_attempt"); got != "0" {
		t.Fatalf("attempt-0 bead %s gc.lumen_attempt = %q, want 0", b0.ID, got)
	}
	if got := metaValue(b1, "gc.lumen_attempt"); got != "1" {
		t.Fatalf("attempt-1 bead %s gc.lumen_attempt = %q, want 1", b1.ID, got)
	}
	t.Logf("PROOF both attempt beads queryable in the work store: %s (closed/fail, attempt 0) and %s (closed/pass, attempt 1)", b0.ID, b1.ID)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// lumenChainDoIRPath is the committed two-do value-plumbing IR: do "produce"
// (EMIT=aval) → do "consume" (after produce, prompt "use {{produce}}").
func lumenChainDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "two-do-chain.lumen.json")
}

// TestLumenDoDoltE2E_ValuePlumbingDownstreamPrompt (the HIGH-2/3 seal) proves a do's
// output flows into a downstream do's prompt through the real-bead path end to end: on
// a DOLT city, `gc lumen sling` of a two-do chain dispatches "produce"; an ordinary
// pooled worker closes it with gc.output_json=aval (the dispatcher's step-output
// convention); the controller observes the close, seeds the scope, and dispatches
// "consume" with its {{produce}} ref RESOLVED to "use aval"; a second worker claims and
// closes it; the run seals pass. The load-bearing proof is the consume do's dispatched
// prompt (read off its claim JSON) == "use aval", not the unresolved "use {{produce}}".
func TestLumenDoDoltE2E_ValuePlumbingDownstreamPrompt(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-chain.sh", 1, "")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, lumenChainDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// Drive to seal: sling → dispatch produce → claim → close(pass, output=aval) →
	// observe+seed → dispatch consume (RESOLVED prompt) → claim → close → seal.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 6*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, "produce:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled produce:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "consume:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled consume:0 = %q, want pass", got)
	}

	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	produce, okP := byActivation["produce:0"]
	consume, okC := byActivation["consume:0"]
	if !okP || !okC {
		t.Fatalf("both do beads must be queryable in the work store; have activations %v, want produce:0 and consume:0", keysOfBeads(byActivation))
	}
	// produce closed carrying gc.output_json=aval (the do output the worker recorded).
	if got := metaValue(produce, "gc.output_json"); got != "aval" {
		t.Fatalf("produce %s gc.output_json = %q, want aval", produce.ID, got)
	}
	// THE value-plumbing proof: the consume do's dispatched prompt resolved {{produce}}
	// to produce's output — read off its claim JSON description, keyed by its real bead id.
	assertLumenPromptReadback(t, cityDir, consume.ID, "use aval")
	t.Logf("PROOF value plumbing: produce gc.output_json=aval → consume prompt resolved to %q (not \"use {{produce}}\")", "use aval")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence %v", streamID, lumenStreamTypes(events))
}

// waitForOwnedAdmittedOrDiag waits for the dispatch fact and, on timeout, dumps the
// journal stream + the work store so a controller/scope/advance gap is distinguishable
// from a pure demand→spawn gap.
func waitForOwnedAdmittedOrDiag(t *testing.T, gs *graphstore.Store, streamID string, timeout time.Duration, cityDir string) (lumenOwnedAdmitted, error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := lumenStreamEvents(t, gs, streamID)
		for _, e := range events {
			if e.Type == engine.EventOwnedAdmitted {
				a := decodeOwnedAdmitted(t, e.Payload)
				t.Logf("PROOF dispatch fact observed: owned.admitted kind=%s bead_id=%s", a.Kind, a.BeadID)
				return a, nil
			}
		}
		time.Sleep(250 * time.Millisecond)
	}
	events := lumenStreamEvents(t, gs, streamID)
	list, listErr := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if listErr != nil {
		list = fmt.Sprintf("bd list failed: %v\noutput: %s", listErr, list)
	}
	scope := "yes"
	if _, statErr := os.Stat(filepath.Join(cityDir, ".gc", "graph", "journal.db")); statErr != nil {
		scope = fmt.Sprintf("journal.db stat err: %v", statErr)
	}
	return lumenOwnedAdmitted{}, fmt.Errorf("no owned.admitted (dispatch) within %s for %s\njournal.db present: %s\njournal sequence: %v\nwork store beads:\n%s",
		timeout, streamID, scope, lumenStreamTypes(events), list)
}

// waitForLumenSealOrDiag drives to seal and, on timeout, dumps the real bead's state,
// the session list, and the prompt/exec markers so a worker claim/close gap (crux) is
// distinguishable from a controller observe gap.
func waitForLumenSealOrDiag(t *testing.T, gs *graphstore.Store, streamID string, timeout time.Duration, cityDir, realBeadID string) []graphstore.StoredEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []graphstore.StoredEvent
	for time.Now().Before(deadline) {
		last = lumenStreamEvents(t, gs, streamID)
		if n := len(last); n > 0 && last[n-1].Type == engine.EventRunClosed {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	beadShow, showErr := bdDolt(cityDir, "show", realBeadID, "--json")
	if showErr != nil {
		beadShow = fmt.Sprintf("bd show failed: %v\noutput: %s", showErr, beadShow)
	}
	sessList, _ := gc(cityDir, "session", "list", "--state", "all")
	readyOut, _ := bdDolt(cityDir, "ready", "--json", "--limit=0", "--metadata-field", "gc.routed_to="+lumenDoRoute)
	markers, _ := filepath.Glob(filepath.Join(cityDir, ".gc", "lumen-e2e-*"))
	t.Fatalf("run %s did not seal within %s\nsequence: %v\nreal bead %s:\n%s\nready(routed=%s):\n%s\nsessions:\n%s\nmarkers: %v",
		streamID, timeout, lumenStreamTypes(last), realBeadID, beadShow, lumenDoRoute, readyOut, sessList, markers)
	return nil
}

// waitForLumenSealOrDiagRun drives to seal and, on timeout, dumps every run bead's
// state and the session list so a respawn/claim gap is distinguishable from a
// controller observe gap.
func waitForLumenSealOrDiagRun(t *testing.T, gs *graphstore.Store, streamID string, timeout time.Duration, cityDir string) []graphstore.StoredEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []graphstore.StoredEvent
	for time.Now().Before(deadline) {
		last = lumenStreamEvents(t, gs, streamID)
		if n := len(last); n > 0 && last[n-1].Type == engine.EventRunClosed {
			return last
		}
		time.Sleep(250 * time.Millisecond)
	}
	var beadDump strings.Builder
	for _, b := range lumenDoltListAllBeads(t, cityDir) {
		if metaValue(b, "gc.lumen_run") != streamID {
			continue
		}
		fmt.Fprintf(&beadDump, "  id=%s type=%s status=%s activation=%s attempt=%s outcome=%s\n",
			b.ID, beadType(b), b.Status, metaValue(b, "gc.lumen_activation"), metaValue(b, "gc.lumen_attempt"), metaValue(b, "gc.outcome"))
	}
	sessList, _ := gc(cityDir, "session", "list", "--state", "all")
	t.Fatalf("run %s did not seal within %s\nsequence: %v\nrun beads:\n%s\nsessions:\n%s",
		streamID, timeout, lumenStreamTypes(last), beadDump.String(), sessList)
	return nil
}

// waitForFileExists polls until path exists (a scripted agent's durable marker) or
// fatals on timeout.
func waitForFileExists(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("marker file %q never appeared within %s", path, timeout)
}

// --- dolt-aware assertions ----------------------------------------------------

const beadmetaOutcomeKey = "gc.outcome"

// beadStatus returns the graphBead status.
func beadStatus(b graphBead) string { return b.Status }

// keysOfBeads returns the activation keys of a bead map for diagnostics.
func keysOfBeads(m map[string]graphBead) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// assertLumenRealWorkBeadClosedDolt proves the dispatched do work is a REAL ordinary
// bead in the CITY WORK store on Dolt: an ordinary `bd show --json` resolves it (a
// journal projection would not), it is task-typed, closed, and carries the run-linkage
// metadata (routed + run + the given activation) with the expected outcome.
func assertLumenRealWorkBeadClosedDolt(t *testing.T, cityDir, beadID, streamID, activation, wantOutcome string) {
	t.Helper()
	bead, err := tryShowBead(cityDir, beadID)
	if err != nil {
		t.Fatalf("real work bead %q not resolvable via ordinary bd show (must be a real store bead, not a journal projection): %v", beadID, err)
	}
	if got := beadType(bead); got != "task" {
		t.Fatalf("real work bead %q type = %q, want task", beadID, got)
	}
	if bead.Status != "closed" {
		t.Fatalf("real work bead %q status = %q, want closed (the worker closed it ordinarily)", beadID, bead.Status)
	}
	if got := metaValue(bead, "gc.routed_to"); got != lumenDoRoute {
		t.Fatalf("real work bead %q gc.routed_to = %q, want %q", beadID, got, lumenDoRoute)
	}
	if got := metaValue(bead, "gc.lumen_run"); got != streamID {
		t.Fatalf("real work bead %q gc.lumen_run = %q, want %q", beadID, got, streamID)
	}
	if got := metaValue(bead, "gc.lumen_activation"); got != activation {
		t.Fatalf("real work bead %q gc.lumen_activation = %q, want %q", beadID, got, activation)
	}
	if got := metaValue(bead, "gc.outcome"); got != wantOutcome {
		t.Fatalf("real work bead %q gc.outcome = %q, want %q", beadID, got, wantOutcome)
	}
	t.Logf("PROOF real work bead %s: task, closed, routed=%s, run=%s, activation=%s, outcome=%s (ordinary bd-resolvable, store-minted)", beadID, lumenDoRoute, streamID, activation, wantOutcome)
}

// assertLumenDoFoldRowIsPlainStepDolt proves the do's fold-owned journal row is a
// PLAIN step (not a claimable task-typed doppelganger of the real bead) and carries no
// claim-routing metadata, and that no Tier-A frontier row survives for it — the
// coexistence proof that nothing double-claims off Tier-A on the real-bead path. The
// journal is always sqlite regardless of the beads backend.
func assertLumenDoFoldRowIsPlainStepDolt(t *testing.T, journalPath, streamID, nodeID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+journalPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		t.Fatalf("opening journal read-only: %v", err)
	}
	defer func() { _ = db.Close() }()

	var beadType string
	if err := db.QueryRow(`SELECT bead_type FROM nodes WHERE id = ? AND stream_id = ? AND fold_owned = 1`, nodeID, streamID).Scan(&beadType); err != nil {
		t.Fatalf("reading do fold row for %q: %v", nodeID, err)
	}
	if beadType != "step" {
		t.Fatalf("do fold row %q bead_type = %q, want step (a task-typed fold row would be a bd-ready doppelganger)", nodeID, beadType)
	}

	var routeRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_metadata WHERE node_id = ? AND key = 'gc.routed_to'`, nodeID).Scan(&routeRows); err != nil {
		t.Fatalf("counting %q gc.routed_to node_metadata: %v", nodeID, err)
	}
	if routeRows != 0 {
		t.Fatalf("do fold row %q carries %d gc.routed_to rows, want 0 (never a claim surface)", nodeID, routeRows)
	}

	var frontierRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM frontier WHERE node_id = ?`, nodeID).Scan(&frontierRows); err != nil {
		t.Fatalf("counting %q frontier rows: %v", nodeID, err)
	}
	if frontierRows != 0 {
		t.Fatalf("do fold row %q has %d Tier-A frontier rows, want 0 (no claimable frontier surface)", nodeID, frontierRows)
	}
	t.Logf("PROOF do fold row %q is a plain step, no gc.routed_to, no Tier-A frontier row (no claimable Tier-B doppelganger)", nodeID)
}

// assertLumenPromptReadback checks the prompt-readback file the scripted agent wrote
// from the claim JSON description, keyed by the real store-minted bead id.
func assertLumenPromptReadback(t *testing.T, cityDir, beadID, want string) {
	t.Helper()
	p := filepath.Join(cityDir, ".gc", "lumen-e2e-prompt-"+beadID+".txt")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading prompt readback %q: %v", p, err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("prompt readback for %q = %q, want %q", beadID, got, want)
	}
	t.Logf("PROOF worker read its prompt off the claim JSON: %q", want)
}

// lumenDoltRunBeadsByActivation lists every real work bead a run dispatched (filtered
// by gc.lumen_run) from the city WORK store on Dolt, keyed by gc.lumen_activation — the
// fresh-bead-per-attempt visibility read.
func lumenDoltRunBeadsByActivation(t *testing.T, cityDir, streamID string) map[string]graphBead {
	t.Helper()
	beads := lumenDoltListAllBeads(t, cityDir)
	out := map[string]graphBead{}
	for _, b := range beads {
		if metaValue(b, "gc.lumen_run") != streamID {
			continue
		}
		act := metaValue(b, "gc.lumen_activation")
		if act == "" {
			continue
		}
		out[act] = b
	}
	return out
}

// lumenDoltListAllBeads reads every bead (closed included) from the city WORK store on
// Dolt via an ordinary `bd list --json --all`.
func lumenDoltListAllBeads(t *testing.T, cityDir string) []graphBead {
	t.Helper()
	out, err := bdDolt(cityDir, "list", "--json", "--all", "--limit=0")
	if err != nil {
		t.Fatalf("bd list --json --all failed: %v\noutput: %s", err, out)
	}
	var beads []graphBead
	if err := json.Unmarshal([]byte(strings.TrimSpace(extractJSONPayload(out))), &beads); err != nil {
		t.Fatalf("unmarshal bead list: %v\njson: %s", err, out)
	}
	return beads
}

// assertZeroControlBeadsDolt pins the zero-control-beads claim on a Dolt city in three
// layers: (1) the journal projection carries no gc.kind and every run-stream node is
// fold-owned; (2) the city WORK store (queried via ordinary bd) holds no bead carrying
// the gc.kind control marker; (3) the control-dispatcher lane served no control bead.
func assertZeroControlBeadsDolt(t *testing.T, cityDir, journalPath, streamID string) {
	t.Helper()

	// Layer 1 — journal projection (raw read-only SQL; backend-independent).
	db, err := sql.Open("sqlite", "file:"+journalPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		t.Fatalf("opening journal read-only: %v", err)
	}
	defer func() { _ = db.Close() }()

	var gcKindRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_metadata WHERE key = 'gc.kind'`).Scan(&gcKindRows); err != nil {
		t.Fatalf("counting gc.kind node_metadata: %v", err)
	}
	if gcKindRows != 0 {
		t.Fatalf("journal node_metadata has %d gc.kind rows, want 0", gcKindRows)
	}
	var streamNodes, foldUnownedNodes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE stream_id = ?`, streamID).Scan(&streamNodes); err != nil {
		t.Fatalf("counting stream nodes: %v", err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE stream_id = ? AND fold_owned = 0`, streamID).Scan(&foldUnownedNodes); err != nil {
		t.Fatalf("counting fold-unowned stream nodes: %v", err)
	}
	if streamNodes == 0 {
		t.Fatalf("journal has 0 nodes for stream %s — the projection never populated", streamID)
	}
	if foldUnownedNodes != 0 {
		t.Fatalf("journal has %d fold-unowned nodes for stream %s, want every run-stream node fold_owned=1", foldUnownedNodes, streamID)
	}
	t.Logf("PROOF zero-control-beads (journal): gc.kind rows=%d; stream nodes=%d all fold_owned=1", gcKindRows, streamNodes)

	// Layer 2 — the city WORK store (Dolt), read via ordinary bd. No bead carries the
	// gc.kind control marker — only the leaf do work beads exist. (task-typed do work
	// beads carry NO gc.kind; only control-graph beads do.)
	for _, b := range lumenDoltListAllBeads(t, cityDir) {
		if metaValue(b, "gc.kind") != "" {
			t.Fatalf("work store bead %s carries a gc.kind=%q control marker (zero-control-beads violated)", b.ID, metaValue(b, "gc.kind"))
		}
	}
	t.Logf("PROOF zero-control-beads (work store): no bead carries a gc.kind control marker")

	// Layer 3 — structural: the control-dispatcher lane served no control bead.
	for _, path := range []string{
		citylayout.ControlDispatcherTraceDefaultPath(cityDir),
		citylayout.ControlDispatcherTraceDefaultPathFor(cityDir, "core.control-dispatcher"),
	} {
		if trace := readOptionalFile(path); strings.Contains(trace, "serve process bead=") {
			t.Fatalf("control dispatcher served a control bead (zero-control-beads violated) at %s:\n%s", path, trace)
		}
	}
	t.Logf("PROOF zero-control-beads (structural): control-dispatcher lane served no control bead")
}
