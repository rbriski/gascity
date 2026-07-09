//go:build integration

package integration

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/test/tmuxtest"

	_ "modernc.org/sqlite" // read-only journal.db introspection for the zero-control-beads asserts
)

// L3 — the first demonstrable e2e: a Lumen do-node spawning ONE real pooled city
// session, fold-orchestrated, zero control beads. The controller loop drives an
// enqueued do-only run to run.closed as a REAL pooled session claims the
// materialized Tier-B work off the graph journal, reads its prompt from the claim
// JSON, and settles it — no control beads anywhere. See
// .scratch/lumen-integration/substrate/plan/L3-e2e-demo-plan.md.

const (
	lumenDoPrompt     = "Say hello to Gas City, then settle this step."
	lumenDoRoute      = "workers"
	lumenDoStreamHint = "gcg-run-"
)

// lumenDoIRPath is the committed, side-by-side IR the demo slings (no Node at test
// time — the sibling .lumen.json convention, S20).
func lumenDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "hello-do.lumen.json")
}

// setupLumenDoCity mirrors setupGraphWorkflowCity (graph_dispatch_test.go) plus the
// one extra `gc migrate graph-journal init` step Lumen needs. provider is
// "subprocess" (the deterministic default variant) or "tmux" (the real-session
// variant on the city's isolated -L socket). It returns the city dir and the
// isolated command env (whose supervisor is the controller running the lumen tick).
func setupLumenDoCity(t *testing.T, provider string) (string, []string) {
	return setupLumenDoCityWithAgent(t, provider, "lumen-do.sh")
}

// setupLumenDoCityWithAgent is setupLumenDoCity parameterized by the pool agent
// script — "lumen-do.sh" (the L3 always-pass worker) or "lumen-do-flaky.sh" (the
// L5 fail-then-pass worker for the retry demo).
func setupLumenDoCityWithAgent(t *testing.T, provider, agentScriptName string) (string, []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	var cityName string
	if provider == "tmux" {
		cityName = tmuxtest.NewGuard(t).CityName()
	} else {
		cityName = uniqueCityName()
	}
	cityDir := filepath.Join(t.TempDir(), cityName)

	// The scripted agent IS the start_command: the subprocess provider drops
	// startup prompts (plan correction #2), so the claim→read→close loop must be
	// the command, not a prompt asset. The 3s work window holds the in_progress claim
	// across ~30 patrol passes (100ms patrol) — the no-mid-do-drain observation
	// window (plan §3.4) — while staying ≪ the firewall grace floor (60s), so step
	// (5)'s pass settle proves the worker settled it, not the firewall.
	startCommand := "GC_LUMEN_E2E_WORK_SECONDS=3 bash " + agentScript(agentScriptName)

	// A plain pool agent: max_active_sessions=1 (the ONE-session cap + pool shape),
	// no scale_check (else it leaves the native default-scale probe the Lumen demand
	// rides), no min_active_sessions (min=0 ⇒ the ONLY spawn cause is the frontier
	// row), no [[named_session]] (the spawn must be DEMAND-driven), and no formula_v2
	// (so the implicit control-dispatcher agent stays inert — it serves no control
	// bead; the zero-control-beads claim is verified in assertZeroControlBeads).
	sessionBlock := ""
	if provider != "tmux" {
		sessionBlock = "\n[session]\nprovider = \"subprocess\"\n"
	}
	cityToml := fmt.Sprintf(
		"[workspace]\nname = %q\n\n[beads]\nprovider = \"file\"\n%s\n[daemon]\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = %q\nmax_active_sessions = 1\nstart_command = %q\n",
		cityName, sessionBlock, lumenDoRoute, startCommand,
	)
	configPath := filepath.Join(t.TempDir(), "lumen-do.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing lumen-do config: %v", err)
	}

	out, err := runGCWithEnv(env, "", "init", "--skip-provider-readiness", "--file", configPath, cityDir)
	if err != nil {
		t.Fatalf("gc init --file failed: %v\noutput: %s", err, out)
	}
	// Opt the city into the graph-journal scope. lumenEnqueue refuses without it;
	// the running tick lazily opens the scope on its next pass, so post-start opt-in
	// is safe (S2).
	if out, err := runGCWithEnv(env, cityDir, "migrate", "graph-journal", "init"); err != nil {
		t.Fatalf("gc migrate graph-journal init failed: %v\noutput: %s", err, out)
	}
	registerCityCommandEnv(cityDir, env)

	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		runGCWithEnv(env, "", "stop", cityDir)                //nolint:errcheck // best-effort cleanup
		runGCWithEnv(env, "", "supervisor", "stop", "--wait") //nolint:errcheck // best-effort cleanup
		deadline := time.Now().Add(10 * time.Second)
		for time.Now().Before(deadline) {
			cleanupTestCityDir(cityDir)
			if _, err := os.Stat(cityDir); os.IsNotExist(err) {
				return
			}
			time.Sleep(100 * time.Millisecond)
		}
		beadsEntries, _ := os.ReadDir(filepath.Join(cityDir, ".beads"))
		t.Fatalf("lumen-do city cleanup did not quiesce; .beads entries=%v", beadsEntries)
	})

	return cityDir, env
}

// TestLumenDoE2E_ScriptedPoolSession is THE demo (T1 + T3 folded in): an enqueued
// do-only Lumen run spawns ONE real pooled subprocess session that claims the
// materialized Tier-B work, reads its prompt off the claim JSON, and settles it —
// driving the run to run.closed with ZERO control beads. Subprocess provider, no
// LLM creds.
func TestLumenDoE2E_ScriptedPoolSession(t *testing.T) {
	cityDir, _ := setupLumenDoCity(t, "subprocess")
	runLumenDoE2E(t, cityDir, "subprocess")
}

// TestLumenDoE2E_RealTmuxSocket is the same demo body over a REAL pooled tmux
// session on the city's isolated -L <cityName> socket (the default provider). It
// skips cleanly when tmux is unavailable or when the suite runs
// GC_SESSION=subprocess. Teardown is guard-scoped only (never the default server).
func TestLumenDoE2E_RealTmuxSocket(t *testing.T) {
	if usingSubprocess() {
		t.Skip("real-tmux variant needs the default tmux provider; suite runs GC_SESSION=subprocess")
	}
	tmuxtest.RequireTmux(t)
	cityDir, _ := setupLumenDoCity(t, "tmux")
	runLumenDoE2E(t, cityDir, "tmux")
}

// runLumenDoE2E is the shared body T1 (subprocess) and T2 (tmux) both drive. Each
// numbered step pins one L3 exit criterion / §3.4 prereq.
func runLumenDoE2E(t *testing.T, cityDir, provider string) {
	t.Helper()
	ctx := context.Background()

	// (1) Enqueue: the demo's entry command. Parse the run stream id from stdout.
	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	// Open the run's journal once (a fresh cross-process WAL reader — the designed
	// mode). Every ReadStream re-reads the latest committed state.
	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// (2) Spawn — kills the assignedWorkBeads:0 routed-pool-demand gap. With min=0
	// and no named session, the ONLY cause a session exists is the Lumen frontier
	// demand. If demand→spawn regressed, NOTHING spawns and we fail here. We observe
	// the spawn as a non-closed session bead for the route (any lifecycle state):
	// under tmux the pooled worker's "active" window is too brief to reliably catch
	// with an active-only poll, but its mere EXISTENCE is the spawn.
	claimant := waitForPooledSession(t, cityDir, lumenDoRoute, 30*time.Second)
	sessionName := claimant.SessionName
	claimantBeadID := strings.TrimSpace(claimant.ID)
	if claimantBeadID == "" {
		t.Fatalf("pooled session row for %q has no bead id; cannot pin no-drain by bead identity", lumenDoRoute)
	}
	t.Logf("PROOF pooled session (Lumen demand spawn) = %s (session bead %s)", sessionName, claimantBeadID)

	// (2b, T2 only) The real-tmux variant additionally proves a REAL pane on the
	// city's isolated -L socket.
	if provider == "tmux" {
		assertTmuxSessionOnCitySocket(t, cityDir, sessionName)
	}

	// (3) Claim (owned.admitted, assignee == session). Observe owned.admitted while
	// the worker holds the claim; its assignee is the live session name and there is
	// exactly one admit (ONE session, ONE claim).
	admitted := waitForOwnedAdmitted(t, gs, streamID, 30*time.Second)
	if admitted.Assignee != sessionName {
		t.Fatalf("owned.admitted assignee = %q, want the live pooled session %q", admitted.Assignee, sessionName)
	}
	if admitted.Kind != engine.OwnedKindTierB {
		t.Fatalf("owned.admitted kind = %q, want %q", admitted.Kind, engine.OwnedKindTierB)
	}
	t.Logf("PROOF owned.admitted assignee = %s (kind %s)", admitted.Assignee, admitted.Kind)

	// (4) No mid-do drain (S12): across patrol passes while the worker holds the
	// in_progress claim, the SAME session bead stays the claimant — it is not
	// drained and REPLACED. A NAME-only check is blind here: max_active_sessions=1
	// with no namepool gives the pool a canonical singleton identity, so a spurious
	// drain→respawn carries the IDENTICAL session name, and a same-name respawn
	// would even complete the run byte-identically via silent adoption
	// (hookClaimExistingOrAssigned, reason existing_assignment). So we pin session
	// BEAD IDENTITY instead: a drain→respawn mints a NEW session bead, so any bead
	// for the route whose id differs from the original claimant's is a drain. We
	// scan the RAW list (closed rows included) so a drained-then-recreated bead is
	// caught even if the respawn is itself fire-and-forget. Presence is deliberately
	// NOT asserted: a fire-and-forget subprocess worker's bead legitimately leaves
	// the pool view while the process keeps working, and a tmux worker's bead reads
	// "asleep" — neither is a drain, and both keep the SAME id. A list error in this
	// negative window is fatal, never read as "no drain" (L1fix). The complementary
	// drain-with-NO-respawn case is caught by step (5): a drained mid-do worker
	// strands (owned.settled output "stranded:") instead of settling pass itself.
	const drainSamples = 10
	for i := 0; i < drainSamples; i++ {
		time.Sleep(200 * time.Millisecond)
		rows, err := listSessionsForTemplate(t, cityDir, lumenDoRoute)
		if err != nil {
			t.Fatalf("listing sessions in the no-mid-do-drain window: %v", err)
		}
		for _, row := range rows {
			if strings.TrimSpace(row.Template) != lumenDoRoute {
				continue
			}
			if id := strings.TrimSpace(row.ID); id != "" && id != claimantBeadID {
				t.Fatalf("a different session bead %q (name %q, state %q) churned in while claimant bead %q held the mid-do claim (drain+respawn)", id, row.SessionName, row.State, claimantBeadID)
			}
		}
	}
	t.Logf("PROOF no-mid-do-drain: claimant session bead %s never replaced by a new bead across ~%d patrol passes (definitive settle check follows)", claimantBeadID, drainSamples*2)

	// (5) Close → seal. Poll until the stream is EXACTLY the 5-event sequence, then
	// pin the settle outcome (pass, NOT stranded:) and the run terminal (pass).
	events := waitForLumenSeal(t, gs, streamID, 60*time.Second)
	types := lumenStreamTypes(events)
	wantSeq := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,
		engine.EventOwnedAdmitted,
		engine.EventOwnedSettled,
		engine.EventRunClosed,
	}
	if !equalStrings(types, wantSeq) {
		t.Fatalf("journal sequence = %v, want %v", types, wantSeq)
	}
	t.Logf("PROOF ReadStream sequence = %v", types)

	settled := decodeOwnedSettled(t, findEvent(t, events, engine.EventOwnedSettled).Payload)
	if settled.Outcome != engine.OutcomePass {
		t.Fatalf("owned.settled outcome = %q, want pass", settled.Outcome)
	}
	if strings.HasPrefix(settled.Output, "stranded:") {
		t.Fatalf("owned.settled output = %q starts with stranded: — the firewall settled it, not the worker", settled.Output)
	}
	t.Logf("PROOF owned.settled outcome = %s (output %q — worker-settled, not stranded)", settled.Outcome, settled.Output)

	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	// (6) Prompt readback: the worker read its prompt off the claim JSON description,
	// not a store read.
	promptFile := filepath.Join(cityDir, ".gc", "lumen-e2e-prompt-greet.txt")
	promptData, err := os.ReadFile(promptFile)
	if err != nil {
		t.Fatalf("reading prompt readback %q: %v", promptFile, err)
	}
	if got := strings.TrimSpace(string(promptData)); got != lumenDoPrompt {
		t.Fatalf("prompt readback = %q, want %q", got, lumenDoPrompt)
	}
	t.Logf("PROOF prompt readback = %q", strings.TrimSpace(string(promptData)))

	// (7) Provenance CLI: the front-door proof. The do-only run folds exactly one
	// fact (correction #1): lumen.run.closed / pass.
	journalCLI, err := gc(cityDir, "graph", "journal", streamID)
	if err != nil {
		t.Fatalf("gc graph journal failed: %v\noutput: %s", err, journalCLI)
	}
	if !strings.Contains(journalCLI, "stream "+streamID) {
		t.Fatalf("gc graph journal output missing stream header for %s:\n%s", streamID, journalCLI)
	}
	if !containsRunClosedPassRow(journalCLI) {
		t.Fatalf("gc graph journal output missing a lumen.run.closed / pass row:\n%s", journalCLI)
	}
	t.Logf("PROOF gc graph journal:\n%s", strings.TrimRight(journalCLI, "\n"))

	// (8) ZERO control beads (S16) — three layers.
	assertZeroControlBeads(t, cityDir, journalPath, streamID)

	// (T3) Journal hash-chain verify: the cross-process appends (controller + the
	// session's gc processes + this reader) produced a verifiable chain.
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean", streamID)

	// (9) Reaped (§3.4): the session is gone post-seal (demand is 0), and no new
	// session respawns.
	assertSessionReapedNoRespawn(t, cityDir, lumenDoRoute)
	t.Logf("PROOF session reaped post-seal, no respawn")
}

// lumenRepeatDoIRPath is the committed repeat-wrapped do IR (compiled 0.2.5).
func lumenRepeatDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "hello-do-repeat.lumen.json")
}

// TestLumenRepeatDoE2E_FailThenPassOneRetry (L5e / T-E2E) is the retry demo: a
// `repeat lane: prompt … until lane.outcome == pass || iteration >= 3` run whose
// pooled worker closes gc.outcome=fail on the FIRST attempt and pass on the SECOND.
// The fold re-attempts on a FRESH activation with fresh claim tokens (lane:0 → lane:1;
// the pooled session may be reused under the singleton identity) and seals the run
// pass with ZERO control beads — attempts lane:0 (failed) → lane:1 (pass) → loop pass.
func TestLumenRepeatDoE2E_FailThenPassOneRetry(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithAgent(t, "subprocess", "lumen-do-flaky.sh")
	ctx := context.Background()

	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenRepeatDoIRPath(t))
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

	// Drive to seal: two attempts take ≥ two claim cycles, so allow a generous window.
	events := waitForLumenSeal(t, gs, streamID, 120*time.Second)

	// Exactly two claims (owned.admitted) — one per attempt.
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (one pooled session per attempt)\nsequence: %v", len(admits), lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	// A real pooled session claimed each attempt. The session IDENTITY is incidental:
	// the recovery contract is a fresh ACTIVATION (lane:0 → lane:1) with fresh claim
	// tokens, not a fresh session. Under max_active_sessions=1 + canonical singleton
	// identity, the pooled session is legitimately reused (or respawned under the same
	// name) for attempt 2 — that is still correct recovery (no zombie: the live claimant
	// settles each attempt). Distinct-session recovery is the firewall STRAND path
	// (a dead claimant replaced), proven in-process by the closer-identity guard tests.
	if a0.Assignee == "" || a1.Assignee == "" {
		t.Fatalf("attempt assignees = {%q, %q}, want a real pooled session claiming each attempt", a0.Assignee, a1.Assignee)
	}
	t.Logf("PROOF a pooled session claimed each attempt: %q then %q", a0.Assignee, a1.Assignee)

	// The two settlements: lane:0 failed → lane:1 pass.
	settles := lumenEventsOfType(events, engine.EventOwnedSettled)
	if len(settles) != 2 {
		t.Fatalf("owned.settled count = %d, want 2", len(settles))
	}
	s0 := decodeOwnedSettled(t, settles[0].Payload)
	s1 := decodeOwnedSettled(t, settles[1].Payload)
	if s0.Handle != "lane:0" || s0.Outcome != engine.OutcomeFailed {
		t.Fatalf("attempt 0 settle = {%q, %q}, want {lane:0, failed}", s0.Handle, s0.Outcome)
	}
	if s1.Handle != "lane:1" || s1.Outcome != engine.OutcomePass {
		t.Fatalf("attempt 1 settle = {%q, %q}, want {lane:1, pass}", s1.Handle, s1.Outcome)
	}
	t.Logf("PROOF attempts: lane:0 %s -> lane:1 %s", s0.Outcome, s1.Outcome)

	// Two attempt.minted (bookkeeping), the loop settled pass, and the run sealed pass.
	if n := len(lumenEventsOfType(events, engine.EventAttemptMinted)); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2", n)
	}
	loopPass := false
	for _, e := range lumenEventsOfType(events, engine.EventOutcomeSettled) {
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == "repeat_1:0" && p.Outcome == engine.OutcomePass {
			loopPass = true
		}
	}
	if !loopPass {
		t.Fatalf("loop node repeat_1:0 did not settle pass\nsequence: %v", lumenStreamTypes(events))
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	t.Logf("PROOF run sealed pass after 2 attempts")

	// Per-attempt prompt readback: the worker read its prompt off each attempt's claim
	// JSON (two distinct per-attempt files).
	for _, attempt := range []string{"1", "2"} {
		promptFile := filepath.Join(cityDir, ".gc", "lumen-e2e-prompt-lane-attempt-"+attempt+".txt")
		data, rerr := os.ReadFile(promptFile)
		if rerr != nil {
			t.Fatalf("reading per-attempt prompt readback %q: %v", promptFile, rerr)
		}
		if got := strings.TrimSpace(string(data)); got != lumenDoPrompt {
			t.Fatalf("attempt %s prompt readback = %q, want %q", attempt, got, lumenDoPrompt)
		}
	}
	t.Logf("PROOF per-attempt prompt readback written for both attempts")

	// ZERO control beads (the L3 invariant holds across the retry).
	assertZeroControlBeads(t, cityDir, journalPath, streamID)

	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; full sequence: %v", streamID, lumenStreamTypes(events))
}

// lumenEventsOfType returns all events of a given type, in seq order.
func lumenEventsOfType(events []graphstore.StoredEvent, typ string) []graphstore.StoredEvent {
	var out []graphstore.StoredEvent
	for _, e := range events {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// --- assertions --------------------------------------------------------------

// assertZeroControlBeads pins the three-layer zero-control-beads claim: the journal
// projection carries no gc.kind and every run-stream node is fold-owned; the file
// work store holds no bead carrying gc.kind; and the config is structurally
// dispatcher-free.
func assertZeroControlBeads(t *testing.T, cityDir, journalPath, streamID string) {
	t.Helper()

	// Layer 1 — journal projection (raw read-only SQL, the in-test equivalent of the
	// manual demo's sqlite3 -readonly probes).
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
		t.Fatalf("journal has 0 nodes for stream %s — the projection never populated (nothing was asserted)", streamID)
	}
	if foldUnownedNodes != 0 {
		t.Fatalf("journal has %d fold-unowned nodes for stream %s, want every run-stream node fold_owned=1", foldUnownedNodes, streamID)
	}
	t.Logf("PROOF zero-control-beads (journal): gc.kind rows=%d; stream nodes=%d all fold_owned=1", gcKindRows, streamNodes)

	// Layer 2 — the file work store (.gc/beads.json is exactly the store
	// waitForBeadStatus/the filebdshim read). The file-store bd fallback drops
	// metadata from its list output, so the meaningful check reads the work store
	// directly and asserts no bead carries the gc.kind control marker.
	workStore := filepath.Join(cityDir, ".gc", "beads.json")
	if data, err := os.ReadFile(workStore); err == nil {
		if strings.Contains(string(data), `"gc.kind"`) {
			t.Fatalf("work store %s carries a gc.kind control marker:\n%s", workStore, string(data))
		}
		t.Logf("PROOF zero-control-beads (work store): %s carries no gc.kind", workStore)
	} else if !os.IsNotExist(err) {
		t.Fatalf("reading work store %q: %v", workStore, err)
	} else {
		t.Logf("PROOF zero-control-beads (work store): no work-store beads file at all")
	}

	// Layer 3 — structural: the control dispatcher never SERVED a control bead. gc
	// injects an implicit control-dispatcher agent, but with no formula_v2 / control
	// work it stays inert — its trace carries no "serve process bead=" evidence (the
	// inverse of assertControlDispatcherLane in graph_dispatch_test.go). This is the
	// meaningful structural proof: the dispatcher lane produced zero control beads.
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

// assertTmuxSessionOnCitySocket proves a real tmux pane on the city's isolated -L
// socket (socket name == city name, S7). Best-effort match on the session name so a
// tmux name-sanitization ('/'→'--') never false-fails.
func assertTmuxSessionOnCitySocket(t *testing.T, cityDir, sessionName string) {
	t.Helper()
	socket := filepath.Base(cityDir)
	deadline := time.Now().Add(10 * time.Second)
	sanitized := strings.ReplaceAll(sessionName, "/", "--")
	var last string
	for time.Now().Before(deadline) {
		raw, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_name}").CombinedOutput()
		out := string(raw)
		last = out
		if err == nil && (strings.Contains(out, sanitized) || strings.Contains(out, lumenDoRoute)) {
			t.Logf("PROOF real tmux pane on isolated socket -L %s:\n%s", socket, strings.TrimRight(out, "\n"))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no pooled tmux session on isolated socket -L %s within window\nlast list-sessions:\n%s", socket, last)
}

// assertSessionReapedNoRespawn waits for the pooled session to disappear post-seal
// (demand is 0), then samples ~10 patrol intervals to confirm no respawn storm.
func assertSessionReapedNoRespawn(t *testing.T, cityDir, template string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	reaped := false
	for time.Now().Before(deadline) {
		present, err := presentSessionsForTemplate(t, cityDir, template)
		if err != nil {
			// A list failure is not evidence of reaping; keep waiting (L1fix).
			time.Sleep(200 * time.Millisecond)
			continue
		}
		if len(present) == 0 {
			reaped = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !reaped {
		// Confirm reaping from an OBSERVED empty list — a list error here is fatal,
		// never silently read as "reaped" (L1fix).
		present, err := presentSessionsForTemplate(t, cityDir, template)
		if err != nil {
			t.Fatalf("confirming session reaped post-seal: %v", err)
		}
		if len(present) != 0 {
			t.Fatalf("session not reaped within 30s post-seal; present = %v", present)
		}
	}
	for i := 0; i < 10; i++ {
		time.Sleep(100 * time.Millisecond)
		present, err := presentSessionsForTemplate(t, cityDir, template)
		if err != nil {
			// A vacuous nil here would let a real respawn slip past (L1fix).
			t.Fatalf("polling for a post-seal respawn: %v", err)
		}
		if len(present) != 0 {
			t.Fatalf("a session respawned after seal (demand is 0); present = %v", present)
		}
	}
}

// --- journal helpers ---------------------------------------------------------

type lumenOwnedAdmitted struct {
	Handle     string `json:"handle"`
	Activation string `json:"activation"`
	Kind       string `json:"kind"`
	Assignee   string `json:"assignee"`
}

type lumenOwnedSettled struct {
	Handle  string `json:"handle"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

type lumenRunClosed struct {
	Outcome string `json:"outcome"`
}

func parseLumenStreamID(t *testing.T, slingOut string) string {
	t.Helper()
	for _, field := range strings.Fields(slingOut) {
		if strings.HasPrefix(field, lumenDoStreamHint) {
			return field
		}
	}
	t.Fatalf("could not parse a %s… stream id from sling output: %q", lumenDoStreamHint, slingOut)
	return ""
}

func lumenStreamEvents(t *testing.T, gs *graphstore.Store, streamID string) []graphstore.StoredEvent {
	t.Helper()
	events, err := gs.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("ReadStream(%s): %v", streamID, err)
	}
	return events
}

func lumenStreamTypes(events []graphstore.StoredEvent) []string {
	types := make([]string, len(events))
	for i, e := range events {
		types[i] = e.Type
	}
	return types
}

func waitForOwnedAdmitted(t *testing.T, gs *graphstore.Store, streamID string, timeout time.Duration) lumenOwnedAdmitted {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		events := lumenStreamEvents(t, gs, streamID)
		var admits []graphstore.StoredEvent
		for _, e := range events {
			if e.Type == engine.EventOwnedAdmitted {
				admits = append(admits, e)
			}
		}
		if len(admits) == 1 {
			return decodeOwnedAdmitted(t, admits[0].Payload)
		}
		if len(admits) > 1 {
			t.Fatalf("observed %d owned.admitted events, want exactly 1 (ONE session, ONE claim)", len(admits))
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("owned.admitted never appeared for %s within %s\nlast sequence: %v", streamID, timeout, lumenStreamTypes(lumenStreamEvents(t, gs, streamID)))
	return lumenOwnedAdmitted{}
}

func waitForLumenSeal(t *testing.T, gs *graphstore.Store, streamID string, timeout time.Duration) []graphstore.StoredEvent {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []graphstore.StoredEvent
	for time.Now().Before(deadline) {
		last = lumenStreamEvents(t, gs, streamID)
		if n := len(last); n > 0 && last[n-1].Type == engine.EventRunClosed {
			return last
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("run %s did not seal (run.closed) within %s\nlast sequence: %v", streamID, timeout, lumenStreamTypes(last))
	return nil
}

func findEvent(t *testing.T, events []graphstore.StoredEvent, typ string) graphstore.StoredEvent {
	t.Helper()
	for _, e := range events {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("event %q not found in stream", typ)
	return graphstore.StoredEvent{}
}

func decodeOwnedAdmitted(t *testing.T, payload []byte) lumenOwnedAdmitted {
	t.Helper()
	var p lumenOwnedAdmitted
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decoding owned.admitted payload: %v", err)
	}
	return p
}

func decodeOwnedSettled(t *testing.T, payload []byte) lumenOwnedSettled {
	t.Helper()
	var p lumenOwnedSettled
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decoding owned.settled payload: %v", err)
	}
	return p
}

func decodeRunClosed(t *testing.T, payload []byte) lumenRunClosed {
	t.Helper()
	var p lumenRunClosed
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("decoding run.closed payload: %v", err)
	}
	return p
}

// containsRunClosedPassRow reports whether the `gc graph journal` table contains a
// lumen.run.closed row whose outcome column is pass.
func containsRunClosedPassRow(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, engine.EventRunClosed) && strings.Contains(line, engine.OutcomePass) {
			return true
		}
	}
	return false
}

// --- session helpers ---------------------------------------------------------

type sessionListRow struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	Closed      bool   `json:"closed"`
	State       string `json:"state"`
	SessionName string `json:"session_name"`
}

// listSessionsForTemplate returns the session rows for template, surfacing any
// exec/unmarshal error instead of laundering it to an empty slice. The negative
// asserts (no-mid-do-drain, reaped-no-respawn) MUST treat a transient list
// failure as fatal, never as "no sessions" — otherwise a store lock/timeout mid
// window passes them without observing anything (L1fix).
func listSessionsForTemplate(t *testing.T, cityDir, template string) ([]sessionListRow, error) {
	t.Helper()
	out, err := gc(cityDir, "session", "list", "--json", "--template", template)
	if err != nil {
		return nil, fmt.Errorf("gc session list --template %s: %w\noutput: %s", template, err, out)
	}
	var sessionList struct {
		Sessions []sessionListRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &sessionList); err != nil {
		return nil, fmt.Errorf("unmarshaling session list --template %s: %w\noutput: %s", template, err, out)
	}
	return sessionList.Sessions, nil
}

// waitForPooledSession polls until exactly one non-closed pooled session for
// template exists and returns its row (name + session BEAD id). It observes the
// spawn by presence (any lifecycle state), not by an "active" state the tmux
// worker only briefly reports; more than one session for the route is a hard
// failure (the ONE-session cap). A transient list error while WAITING is retried,
// not read as absence. It fatals on timeout.
func waitForPooledSession(t *testing.T, cityDir, template string, timeout time.Duration) sessionListRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := listSessionsForTemplate(t, cityDir, template)
		if err != nil {
			// A transient list failure is not evidence of "no spawn": keep polling.
			time.Sleep(200 * time.Millisecond)
			continue
		}
		var present []sessionListRow
		for _, s := range rows {
			if s.Closed || strings.TrimSpace(s.Template) != template {
				continue
			}
			if strings.TrimSpace(s.SessionName) != "" {
				present = append(present, s)
			}
		}
		if len(present) > 1 {
			names := make([]string, len(present))
			for i, p := range present {
				names[i] = p.SessionName
			}
			t.Fatalf("more than one pooled session for %q (want exactly 1, the ONE-session cap): %v", template, names)
		}
		if len(present) == 1 {
			return present[0]
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("no pooled session spawned for template %q within %s (Lumen demand→spawn gap?)", template, timeout)
	return sessionListRow{}
}

// presentSessionsForTemplate returns the names of NON-CLOSED pooled sessions for
// template, in any lifecycle state (creating/active/awake/asleep). A drained
// session is closed/removed, so presence is the faithful "not drained" signal that
// tolerates the transient "asleep" a preserved busy worker can report. It
// propagates any list error so negative-assert callers can hard-fail rather than
// read a vacuous empty slice (L1fix).
func presentSessionsForTemplate(t *testing.T, cityDir, template string) ([]string, error) {
	t.Helper()
	rows, err := listSessionsForTemplate(t, cityDir, template)
	if err != nil {
		return nil, err
	}
	var present []string
	for _, s := range rows {
		if s.Closed || strings.TrimSpace(s.Template) != template {
			continue
		}
		if name := strings.TrimSpace(s.SessionName); name != "" {
			present = append(present, name)
		}
	}
	return present, nil
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
