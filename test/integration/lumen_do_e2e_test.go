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
	"strconv"
	"strings"
	"syscall"
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
// L5 fail-then-pass worker for the retry demo). It keeps the L3/L5 defaults:
// max_active_sessions=1 and a 3s work window (the no-mid-do-drain window, ≪ the 60s
// firewall grace). New L4 rows use setupLumenDoCityWithOptions directly.
func setupLumenDoCityWithAgent(t *testing.T, provider, agentScriptName string) (string, []string) {
	t.Helper()
	return setupLumenDoCityWithOptions(t, provider, agentScriptName, 1, "GC_LUMEN_E2E_WORK_SECONDS=3")
}

// lumenKillNonce is the per-city process-table token the firewall e2e's hang agent
// carries in its command line (an env assignment + its re-exec'd argv[0]) so a
// `pgrep -f` can find and SIGKILL exactly that session process. The lumenhang- prefix
// keeps the token off the controller/supervisor argv (which carries only the plain
// city dir path), so the kill never touches the infrastructure processes.
func lumenKillNonce(cityName string) string {
	return "lumenhang-" + cityName
}

// setupLumenDoCityWithOptions is the general Lumen test-city builder: it adds the
// pool cap (maxActive) and the per-agent env prefix the L4 concurrency / firewall
// cities need (barrier size, work window). Every start command also carries
// GC_LUMEN_E2E_NONCE=<lumenKillNonce> — inert for most agents, the SIGKILL target
// token for the firewall e2e's hang agent. The scripted agent IS the start_command
// (the subprocess provider drops startup prompts, plan correction #2). Otherwise it
// mirrors the L3 shape exactly: [beads] file, subprocess session (default variant),
// 100ms patrol, a plain pool agent, no scale_check / named_session / min_active.
func setupLumenDoCityWithOptions(t *testing.T, provider, agentScriptName string, maxActive int, agentEnv string) (string, []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, false)

	var cityName string
	if provider == "tmux" {
		cityName = tmuxtest.NewGuard(t).CityName()
	} else {
		cityName = uniqueCityName()
	}
	cityDir := filepath.Join(t.TempDir(), cityName)

	nonceEnv := "GC_LUMEN_E2E_NONCE=" + lumenKillNonce(cityName)
	startCommand := nonceEnv + " " + agentEnv + " bash " + agentScript(agentScriptName)

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
		"[workspace]\nname = %q\n\n[beads]\nprovider = \"file\"\n%s\n[daemon]\npatrol_interval = \"100ms\"\n\n[[agent]]\nname = %q\nmax_active_sessions = %d\nstart_command = %q\n",
		cityName, sessionBlock, lumenDoRoute, maxActive, startCommand,
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
	Handle    string `json:"handle"`
	Kind      string `json:"kind"`
	Outcome   string `json:"outcome"`
	Output    string `json:"output"`
	Retryable bool   `json:"retryable"`
	Assignee  string `json:"assignee"`
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

// ============================================================================
// L4 — multi-do DAG concurrency e2e (S-H2 helpers + T-E1/T-E1t/T-E2/T-E3/T-E4)
// ============================================================================

// --- S-H2: count-parameterized wait helpers ---------------------------------

// waitForPooledSessions polls until at least n DISTINCT non-closed pooled sessions
// (by session bead id) for template have been observed, and returns those distinct
// rows. Distinctness is ACCUMULATED across polls so a fire-and-forget subprocess
// worker that briefly leaves the pool view still counts — the proof is "Lumen demand
// spawned N distinct sessions" (the concurrency-cap pin), not "N are listed at one
// instant". A transient list error while waiting is retried, never read as absence.
func waitForPooledSessions(t *testing.T, cityDir, template string, n int, timeout time.Duration) []sessionListRow {
	t.Helper()
	deadline := time.Now().Add(timeout)
	seen := map[string]sessionListRow{}
	for time.Now().Before(deadline) {
		rows, err := listSessionsForTemplate(t, cityDir, template)
		if err != nil {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		for _, s := range rows {
			if s.Closed || strings.TrimSpace(s.Template) != template {
				continue
			}
			id := strings.TrimSpace(s.ID)
			if id == "" || strings.TrimSpace(s.SessionName) == "" {
				continue
			}
			if _, ok := seen[id]; !ok {
				seen[id] = s
			}
		}
		if len(seen) >= n {
			out := make([]sessionListRow, 0, len(seen))
			for _, s := range seen {
				out = append(out, s)
			}
			return out
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("observed %d distinct pooled sessions for %q within %s, want >= %d (demand→spawn cap gap?)", len(seen), template, timeout, n)
	return nil
}

// waitForOwnedAdmittedCount polls until at least n owned.admitted events exist in the
// stream and returns them decoded, in seq order. Used to observe N concurrent claims.
func waitForOwnedAdmittedCount(t *testing.T, gs *graphstore.Store, streamID string, n int, timeout time.Duration) []lumenOwnedAdmitted {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		admits := lumenEventsOfType(lumenStreamEvents(t, gs, streamID), engine.EventOwnedAdmitted)
		if len(admits) >= n {
			out := make([]lumenOwnedAdmitted, len(admits))
			for i, e := range admits {
				out[i] = decodeOwnedAdmitted(t, e.Payload)
			}
			return out
		}
		time.Sleep(150 * time.Millisecond)
	}
	saw := lumenEventsOfType(lumenStreamEvents(t, gs, streamID), engine.EventOwnedAdmitted)
	t.Fatalf("observed %d owned.admitted for %s within %s, want >= %d", len(saw), streamID, timeout, n)
	return nil
}

// --- seq / decode helpers ----------------------------------------------------

type lumenNodeActivated struct {
	NodeID       string `json:"node_id"`
	Activation   string `json:"activation"`
	DispatchMode string `json:"dispatch_mode"`
	Route        string `json:"route"`
}

type lumenNodeDecision struct {
	NextMember string `json:"next_member"`
}

type lumenOutcomeSettled struct {
	Activation string `json:"activation"`
	Outcome    string `json:"outcome"`
}

func minSeqOf(events []graphstore.StoredEvent) uint64 {
	var m uint64
	for i, e := range events {
		if i == 0 || e.Seq < m {
			m = e.Seq
		}
	}
	return m
}

func maxSeqOf(events []graphstore.StoredEvent) uint64 {
	var m uint64
	for _, e := range events {
		if e.Seq > m {
			m = e.Seq
		}
	}
	return m
}

// nodeActivatedIDsBefore returns the set of node ids whose node.activated event
// committed strictly before seqCutoff — the "materialized in the same pass" proof.
func nodeActivatedIDsBefore(t *testing.T, events []graphstore.StoredEvent, seqCutoff uint64) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range events {
		if e.Type != engine.EventNodeActivated || e.Seq >= seqCutoff {
			continue
		}
		var p lumenNodeActivated
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		out[p.NodeID] = true
	}
	return out
}

// outcomeSettledFor returns the outcome of the outcome.settled event for activation,
// or "" if none.
func outcomeSettledFor(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p lumenOutcomeSettled
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return p.Outcome
		}
	}
	return ""
}

// gatherDecisionMembers returns, in seq order, the NextMember of each node.decision
// checkpoint — the head-of-line gather drain order.
func gatherDecisionMembers(t *testing.T, events []graphstore.StoredEvent) []string {
	t.Helper()
	var out []string
	for _, e := range events {
		if e.Type != engine.EventNodeDecision {
			continue
		}
		var p lumenNodeDecision
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.decision: %v", err)
		}
		if p.NextMember != "" {
			// next_member is the member ACTIVATION (e.g. "one:0"); the drain order is
			// over the bare member node ids.
			out = append(out, engine.ActivationNodeID(p.NextMember))
		}
	}
	return out
}

// assertPromptReadback checks the per-bead prompt-readback file the barrier/scripted
// agent wrote from the claim JSON.
func assertPromptReadback(t *testing.T, cityDir, beadID, want string) {
	t.Helper()
	p := filepath.Join(cityDir, ".gc", "lumen-e2e-prompt-"+beadID+".txt")
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("reading prompt readback %q: %v", p, err)
	}
	if got := strings.TrimSpace(string(data)); got != want {
		t.Fatalf("prompt readback %q = %q, want %q", beadID, got, want)
	}
}

// --- T-E1 / T-E1t: two do's genuinely concurrent -----------------------------

func lumenScatterPairIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "scatter-pair-do.lumen.json")
}

// TestLumenTwoDoConcurrentE2E (T-E1, scenario 1) proves TWO independent pool do's are
// genuinely in flight at once: a 2-member scatter-of-do's (the language's honest
// parallel form) slung into a max_active_sessions=2 city spawns two DISTINCT pooled
// sessions that both claim before either settles (the barrier guarantees the overlap),
// and the run seals pass with the §0.2 concurrent-close fix keeping both colliding
// closes landing (neither strands). Subprocess provider, no LLM.
func TestLumenTwoDoConcurrentE2E(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithOptions(t, "subprocess", "lumen-do-barrier.sh", 2, "GC_LUMEN_E2E_BARRIER=2")
	runLumenTwoDoConcurrentE2E(t, cityDir, "subprocess")
}

// TestLumenTwoDoConcurrentE2E_RealTmux is T-E1's body on the default tmux provider,
// with an extra pane-count assert on the city's isolated -L socket. It skips without
// tmux or under GC_SESSION=subprocess. Guard-scoped cleanup only.
func TestLumenTwoDoConcurrentE2E_RealTmux(t *testing.T) {
	if usingSubprocess() {
		t.Skip("real-tmux variant needs the default tmux provider; suite runs GC_SESSION=subprocess")
	}
	tmuxtest.RequireTmux(t)
	cityDir, _ := setupLumenDoCityWithOptions(t, "tmux", "lumen-do-barrier.sh", 2, "GC_LUMEN_E2E_BARRIER=2")
	runLumenTwoDoConcurrentE2E(t, cityDir, "tmux")
}

func runLumenTwoDoConcurrentE2E(t *testing.T, cityDir, provider string) {
	t.Helper()
	ctx := context.Background()

	// (1) Sling the 2-member scatter-of-do's.
	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenScatterPairIRPath(t))
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

	// (2) Two DISTINCT pooled sessions spawn (demand→spawn=N pin).
	sessions := waitForPooledSessions(t, cityDir, lumenDoRoute, 2, 30*time.Second)
	names := map[string]bool{}
	ids := map[string]bool{}
	for _, s := range sessions {
		names[strings.TrimSpace(s.SessionName)] = true
		ids[strings.TrimSpace(s.ID)] = true
	}
	if len(names) < 2 || len(ids) < 2 {
		t.Fatalf("pooled sessions not distinct: names=%v ids=%v (want 2 distinct names + bead ids)", names, ids)
	}
	t.Logf("PROOF two distinct pooled sessions spawned: names=%v", keysOf(names))

	// (2b, tmux only) Two real panes on the city's isolated -L socket during the window.
	if provider == "tmux" {
		assertTmuxSessionCountOnCitySocket(t, cityDir, 2)
	}

	// (3) Seal, then read the whole sealed stream once.
	events := waitForLumenSeal(t, gs, streamID, 60*time.Second)

	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	settles := lumenEventsOfType(events, engine.EventOwnedSettled)
	if len(admits) != 2 || len(settles) != 2 {
		t.Fatalf("owned.admitted=%d owned.settled=%d, want 2 and 2 (write-once, two claims)\nsequence: %v", len(admits), len(settles), lumenStreamTypes(events))
	}

	// (4) Materialized-simultaneously pin: both member node.activated precede the first
	// admit (one Advance pass fanned out both).
	firstAdmitSeq := minSeqOf(admits)
	before := nodeActivatedIDsBefore(t, events, firstAdmitSeq)
	for _, m := range []string{"left", "right"} {
		if !before[m] {
			t.Fatalf("member %q was not node.activated before the first admit (seq %d) — not one-pass fan-out; activated-before=%v", m, firstAdmitSeq, keysOf(before))
		}
	}
	t.Logf("PROOF both members activated in one pass before the first admit (seq %d)", firstAdmitSeq)

	// (5) Real-overlap pin: max(admit seq) < min(settle seq) ⇒ both claims held
	// concurrently in the total-ordered stream; the two admit assignees are distinct.
	maxAdmit := maxSeqOf(admits)
	minSettle := minSeqOf(settles)
	if !(maxAdmit < minSettle) {
		t.Fatalf("overlap pin failed: max(admit seq)=%d not < min(settle seq)=%d — the two do's were NOT concurrent\nsequence: %v", maxAdmit, minSettle, lumenStreamTypes(events))
	}
	a0 := decodeOwnedAdmitted(t, admits[0].Payload)
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a0.Assignee == "" || a1.Assignee == "" || a0.Assignee == a1.Assignee {
		t.Fatalf("admit assignees = {%q, %q}, want two distinct non-empty session names", a0.Assignee, a1.Assignee)
	}
	t.Logf("PROOF genuine overlap: admits(seq<=%d) before settles(seq>=%d); assignees %q, %q", maxAdmit, minSettle, a0.Assignee, a1.Assignee)

	// (6) Both settles pass, NEITHER stranded (the §0.2 regression detector); the
	// aggregate `pair` settles pass; the run seals pass.
	for i, e := range settles {
		s := decodeOwnedSettled(t, e.Payload)
		if s.Outcome != engine.OutcomePass {
			t.Fatalf("settle[%d] outcome = %q, want pass", i, s.Outcome)
		}
		if strings.HasPrefix(s.Output, "stranded:") {
			t.Fatalf("settle[%d] output = %q starts with stranded: — the concurrent-close race stranded a healthy do (S-F1 regression)", i, s.Output)
		}
	}
	if o := outcomeSettledFor(t, events, "pair:0"); o != engine.OutcomePass {
		t.Fatalf("aggregate pair:0 settled %q, want pass", o)
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	t.Logf("PROOF both do's settled pass (no strand); aggregate pair pass; run.closed pass")

	// (7) Prompt readback for both members.
	assertPromptReadback(t, cityDir, "left", "Do the left task, then settle this step.")
	assertPromptReadback(t, cityDir, "right", "Do the right task, then settle this step.")

	// (8) Zero control beads + journal verify.
	assertZeroControlBeads(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence: %v", streamID, lumenStreamTypes(events))
}

// --- T-E2: a failing do skip-cascades its dependents -------------------------

func lumenSkipCascadeIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "skip-cascade-do.lumen.json")
}

// TestLumenFailingDoSkipCascadesE2E (T-E2, scenario 2) proves a failing pool do
// skip-cascades its transitive dependents to a failed seal: do a (worker-closed
// failed) → exec b (skipped, never runs) → do c (skipped, never claimable). One
// worker settle then ONE Advance pass does the whole cascade — the deterministic
// 9-event sequence.
func TestLumenFailingDoSkipCascadesE2E(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithOptions(t, "subprocess", "lumen-do-fail.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=1")
	ctx := context.Background()

	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenSkipCascadeIRPath(t))
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

	events := waitForLumenSeal(t, gs, streamID, 60*time.Second)

	// The exact deterministic 9-event sequence (§5.2).
	wantSeq := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,  // a (pool)
		engine.EventOwnedAdmitted,  // a claimed
		engine.EventOwnedSettled,   // a failed (worker)
		engine.EventNodeActivated,  // b
		engine.EventOutcomeSettled, // b skipped
		engine.EventNodeActivated,  // c
		engine.EventOutcomeSettled, // c skipped
		engine.EventRunClosed,      // failed
	}
	if got := lumenStreamTypes(events); !equalStrings(got, wantSeq) {
		t.Fatalf("journal sequence = %v,\n want %v", got, wantSeq)
	}
	t.Logf("PROOF deterministic skip-cascade sequence = %v", lumenStreamTypes(events))

	// a: worker verdict failed, NOT stranded (the firewall did not touch it).
	sa := decodeOwnedSettled(t, findEvent(t, events, engine.EventOwnedSettled).Payload)
	if sa.Outcome != engine.OutcomeFailed {
		t.Fatalf("owned.settled(a) outcome = %q, want failed", sa.Outcome)
	}
	if strings.HasPrefix(sa.Output, "stranded:") {
		t.Fatalf("owned.settled(a) output = %q starts with stranded: — want the worker's own fail close", sa.Output)
	}
	// b and c skip-cascaded.
	if o := outcomeSettledFor(t, events, "b:0"); o != engine.OutcomeSkipped {
		t.Fatalf("b:0 outcome.settled = %q, want skipped", o)
	}
	if o := outcomeSettledFor(t, events, "c:0"); o != engine.OutcomeSkipped {
		t.Fatalf("c:0 outcome.settled = %q, want skipped", o)
	}
	// Exactly ONE admit in the whole stream (c was never claimable).
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n != 1 {
		t.Fatalf("owned.admitted count = %d, want 1 (only a was ever claimable)", n)
	}
	// Projected c row: skipped, not task-typed, no route (the e2e twin of the engine pin).
	db, err := sql.Open("sqlite", "file:"+journalPath+"?_pragma=busy_timeout(5000)&mode=ro")
	if err != nil {
		t.Fatalf("opening journal read-only: %v", err)
	}
	defer func() { _ = db.Close() }()
	var cType, cStatus string
	if err := db.QueryRow(`SELECT bead_type, status FROM nodes WHERE id = 'c' AND fold_owned = 1`).Scan(&cType, &cStatus); err != nil {
		t.Fatalf("reading projected c row: %v", err)
	}
	if cStatus != "skipped" {
		t.Fatalf("c status = %q, want skipped", cStatus)
	}
	if cType == "task" {
		t.Fatalf("c bead_type = task — a skip-cascaded do must never be offered as claimable work")
	}
	var cRoute int
	if err := db.QueryRow(`SELECT COUNT(*) FROM node_metadata WHERE node_id = 'c' AND key = 'gc.routed_to'`).Scan(&cRoute); err != nil {
		t.Fatalf("counting c gc.routed_to: %v", err)
	}
	if cRoute != 0 {
		t.Fatalf("c has %d gc.routed_to rows, want 0 (never offered to a pool)", cRoute)
	}
	t.Logf("PROOF c skip-cascaded (status=%s bead_type=%s, no route), never claimable", cStatus, cType)

	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomeFailed {
		t.Fatalf("run.closed outcome = %q, want failed", closed.Outcome)
	}
	assertZeroControlBeads(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF run.closed failed; zero control beads; Verify clean")
}

// --- T-E3: scatter-of-do's fan-out + gather ----------------------------------

func lumenScatterGatherIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "scatter-gather-do.lumen.json")
}

// TestLumenScatterOfDosE2E (T-E3, scenario 3) proves a scatter(members)-of-do's fans
// out through the pool and gathers: 3 do members materialize in one pass, are claimed
// by pooled sessions (min(members, cap)=2 concurrent), settle pass, then the aggregate
// + the attached gather (head-of-line member-order drain + an exec combine) settle pass
// and the run seals pass.
func TestLumenScatterOfDosE2E(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithOptions(t, "subprocess", "lumen-do-barrier.sh", 2, "GC_LUMEN_E2E_BARRIER=2")
	ctx := context.Background()

	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenScatterGatherIRPath(t))
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

	// At least 2 distinct pooled sessions serve the route (cap=2 concurrency).
	sessions := waitForPooledSessions(t, cityDir, lumenDoRoute, 2, 45*time.Second)
	if len(sessions) < 2 {
		t.Fatalf("distinct pooled sessions = %d, want >= 2", len(sessions))
	}
	t.Logf("PROOF >= %d distinct pooled sessions served the route", len(sessions))

	events := waitForLumenSeal(t, gs, streamID, 90*time.Second)

	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	settles := lumenEventsOfType(events, engine.EventOwnedSettled)
	if len(admits) != 3 || len(settles) != 3 {
		t.Fatalf("owned.admitted=%d owned.settled=%d, want 3 and 3\nsequence: %v", len(admits), len(settles), lumenStreamTypes(events))
	}

	// (1) One-pass fan-out: all 3 member node.activated precede the first admit.
	firstAdmitSeq := minSeqOf(admits)
	before := nodeActivatedIDsBefore(t, events, firstAdmitSeq)
	for _, m := range []string{"one", "two", "three"} {
		if !before[m] {
			t.Fatalf("member %q not node.activated before the first admit (seq %d); activated-before=%v", m, firstAdmitSeq, keysOf(before))
		}
	}

	// (2) All 3 settles pass.
	for i, e := range settles {
		s := decodeOwnedSettled(t, e.Payload)
		if s.Outcome != engine.OutcomePass {
			t.Fatalf("member settle[%d] = %q, want pass", i, s.Outcome)
		}
		if strings.HasPrefix(s.Output, "stranded:") {
			t.Fatalf("member settle[%d] stranded (%q) — a concurrent-close strand", i, s.Output)
		}
	}

	// (3) 2-concurrent pin: the two EARLIEST admits precede the earliest settle.
	admitSeqs := seqsOf(admits)
	minSettle := minSeqOf(settles)
	if n := countBelow(admitSeqs, minSettle); n < 2 {
		t.Fatalf("only %d admits precede the first settle, want >= 2 (min(members,cap)=2 concurrency)\nsequence: %v", n, lumenStreamTypes(events))
	}
	t.Logf("PROOF fan-out: 3 members activated in one pass; >=2 admits held before the first settle")

	// (4) The drain: 3 node.decision checkpoints in member order, combine tally + the
	// aggregate + the gather all pass, run seals pass.
	if got := gatherDecisionMembers(t, events); !equalStrings(got, []string{"one", "two", "three"}) {
		t.Fatalf("gather decision member order = %v, want [one two three]", got)
	}
	if o := outcomeSettledFor(t, events, "tally:0"); o != engine.OutcomePass {
		t.Fatalf("combine tally:0 = %q, want pass", o)
	}
	if o := outcomeSettledFor(t, events, "lanes:0"); o != engine.OutcomePass {
		t.Fatalf("aggregate lanes:0 = %q, want pass", o)
	}
	if o := outcomeSettledFor(t, events, "lanes_gather:0"); o != engine.OutcomePass {
		t.Fatalf("gather lanes_gather:0 = %q, want pass", o)
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	t.Logf("PROOF gather drained [one two three]; tally/lanes/lanes_gather pass; run.closed pass")

	// (5) Prompt readback for all three members.
	assertPromptReadback(t, cityDir, "one", "Do lane one, then settle.")
	assertPromptReadback(t, cityDir, "two", "Do lane two, then settle.")
	assertPromptReadback(t, cityDir, "three", "Do lane three, then settle.")

	assertZeroControlBeads(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence: %v", streamID, lumenStreamTypes(events))
}

// --- Single-kill firewall e2e: strand-seal (Variant A) and recovery (Variant B) ---

// TestLumenSingleKillStrandsAndSealsE2E (Variant A) is the HONEST firewall-wedge proof:
// a claimant is SIGKILLed exactly ONCE (no re-kill), and the run must still seal
// failed-stranded. It replaces the re-kill T-E4, which masked the wedge by killing every
// respawn each tick. Three independent tripwires for the firewall-wedge HIGH:
//   - the run SEALS at all (a wedge — a respawn adopting the row and resetting the
//     name-keyed grace clock — never seals and times out here);
//   - the adopted work's side effect executed EXACTLY ONCE (a script-side exec counter);
//   - respawn churn is bounded (≤ 1 non-original session for the route).
//
// A hang agent claims the do and never closes; after the 60s grace floor the firewall's
// instance-keyed dead-claimant verdict strands it and the bare do seals failed. ~90s.
func TestLumenSingleKillStrandsAndSealsE2E(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithOptions(t, "subprocess", "lumen-do-hang.sh", 1, "")
	ctx := context.Background()
	nonce := lumenKillNonce(filepath.Base(cityDir))

	slingOut, err := gc(cityDir, "lumen", "sling", lumenDoRoute, lumenDoIRPath(t))
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

	// (1) Spawn + claim observed; record the claimant session + bead id.
	claimant := waitForPooledSession(t, cityDir, lumenDoRoute, 30*time.Second)
	claimantBeadID := strings.TrimSpace(claimant.ID)
	admitted := waitForOwnedAdmitted(t, gs, streamID, 30*time.Second)
	if admitted.Assignee == "" {
		t.Fatalf("owned.admitted has no assignee; cannot pin the stranded closer")
	}
	t.Logf("PROOF claim observed: assignee %q (session bead %s)", admitted.Assignee, claimantBeadID)

	// (2) SINGLE SIGKILL of the hang claimant, by the per-city nonce (a process-table
	// query, no PID files). The nonce process exists only after the agent re-execs into
	// the tagged sleep, so waiting for it guarantees the claim has already committed.
	// It is killed EXACTLY ONCE — never re-killed — so a wedge is not masked.
	pids := waitForNoncePID(t, nonce, 30*time.Second)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Fatalf("SIGKILL %d: %v", pid, err)
		}
	}
	t.Logf("PROOF single SIGKILL of the hang claimant %q (%d pid)", admitted.Assignee, len(pids))

	// (3) No wedge, no re-kill: the run seals failed-stranded within the grace floor +
	// slack. A timeout here is the firewall-wedge HIGH (a respawn adopted the row / reset
	// the grace clock). respawns tracks non-original sessions for the churn bound.
	events, respawns := waitForLumenSealTrackingRespawns(t, gs, streamID, cityDir, lumenDoRoute, claimantBeadID, 4*time.Minute)

	// (4) Exactly the 5-event stranded sequence — in particular EXACTLY ONE owned.admitted
	// (an adopted re-execution appends no event, so the exec counter below is the adoption
	// tripwire); the settle is the firewall's.
	wantSeq := []string{
		engine.EventRunStarted,
		engine.EventNodeActivated,
		engine.EventOwnedAdmitted,
		engine.EventOwnedSettled,
		engine.EventRunClosed,
	}
	if got := lumenStreamTypes(events); !equalStrings(got, wantSeq) {
		t.Fatalf("journal sequence = %v, want %v", got, wantSeq)
	}
	settled := decodeOwnedSettled(t, findEvent(t, events, engine.EventOwnedSettled).Payload)
	if settled.Outcome != engine.OutcomeFailed {
		t.Fatalf("owned.settled outcome = %q, want failed (firewall strand)", settled.Outcome)
	}
	if !strings.HasPrefix(settled.Output, "stranded: ") {
		t.Fatalf("owned.settled output = %q, want a \"stranded: <assignee>\" prefix (the firewall settle)", settled.Output)
	}
	if !strings.Contains(settled.Output, admitted.Assignee) {
		t.Fatalf("stranded output = %q does not name the killed assignee %q", settled.Output, admitted.Assignee)
	}
	if !settled.Retryable {
		t.Fatalf("firewall strand retryable = false, want true (lumen_firewall settle stamps retryable)")
	}
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomeFailed {
		t.Fatalf("run.closed outcome = %q, want failed", closed.Outcome)
	}
	t.Logf("PROOF stranded seal: owned.settled failed %q retryable=%v; run.closed failed", settled.Output, settled.Retryable)

	// (5) The adopted work executed EXACTLY ONCE — no respawn adopted the claimed row and
	// re-ran its side effect (the firewall-wedge adoption hazard).
	assertExecCountLines(t, cityDir, "greet", 1)
	t.Logf("PROOF side effect executed exactly once (no adopted re-execution)")

	// (6) Bounded churn: at most one pre-verdict respawn beyond the original claimant.
	if len(respawns) > 1 {
		t.Fatalf("respawns = %d (%v), want ≤ 1 (no churn: original + at most one pre-verdict drain)", len(respawns), respawns)
	}
	t.Logf("PROOF bounded churn: %d respawn(s) beyond the original claimant", len(respawns))

	assertZeroControlBeads(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean; sequence: %v", streamID, lumenStreamTypes(events))
}

// TestLumenSingleKillRetryRecoversE2E (Variant B) is the correct-behavior end-to-end:
// a SINGLE kill of a repeat-loop's first attempt drives A dies → firewall strands
// lane:0 at the floor (retryable) → the loop mints lane:1 (fresh tokens) → a FRESH
// pooled worker claims lane:1 and completes it → the run seals PASS — with NO test-side
// assistance after the one kill. The hang-once agent hangs on attempt :0 (killable) and
// completes every later attempt. ~90-150s.
func TestLumenSingleKillRetryRecoversE2E(t *testing.T) {
	cityDir, _ := setupLumenDoCityWithOptions(t, "subprocess", "lumen-do-hang-once.sh", 1, "")
	ctx := context.Background()
	nonce := lumenKillNonce(filepath.Base(cityDir))

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

	// (1) lane:0 claim observed, then a SINGLE SIGKILL of its claimant. The repeat
	// loop materializes lane:0 through an extra layer (loop node → attempt → lane:0),
	// so the first spawn+claim is a touch slower than a bare do — allow generous headroom.
	admitted := waitForOwnedAdmitted(t, gs, streamID, 90*time.Second)
	if admitted.Handle != "lane:0" || admitted.Assignee == "" {
		t.Fatalf("first owned.admitted = {handle:%q assignee:%q}, want {lane:0, <a worker>}", admitted.Handle, admitted.Assignee)
	}
	pids := waitForNoncePID(t, nonce, 60*time.Second)
	for _, pid := range pids {
		if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			t.Fatalf("SIGKILL %d: %v", pid, err)
		}
	}
	t.Logf("PROOF single SIGKILL of lane:0's claimant %q", admitted.Assignee)

	// (2) The run recovers and seals PASS with no further kills.
	events := waitForLumenSeal(t, gs, streamID, 4*time.Minute)

	// The firewall stranded lane:0 (retryable), then the loop re-attempted lane:1.
	settles := lumenEventsOfType(events, engine.EventOwnedSettled)
	if len(settles) != 2 {
		t.Fatalf("owned.settled count = %d, want 2 (lane:0 strand, lane:1 pass)\nsequence: %v", len(settles), lumenStreamTypes(events))
	}
	s0 := decodeOwnedSettled(t, settles[0].Payload)
	if s0.Handle != "lane:0" || s0.Outcome != engine.OutcomeFailed || !s0.Retryable || !strings.HasPrefix(s0.Output, "stranded: ") {
		t.Fatalf("lane:0 settle = {%q %q retryable=%v %q}, want {lane:0, failed, true, stranded:...}", s0.Handle, s0.Outcome, s0.Retryable, s0.Output)
	}
	s1 := decodeOwnedSettled(t, settles[1].Payload)
	if s1.Handle != "lane:1" || s1.Outcome != engine.OutcomePass {
		t.Fatalf("lane:1 settle = {%q, %q}, want {lane:1, pass}", s1.Handle, s1.Outcome)
	}

	// A fresh claim minted the re-attempt: two attempt.minted, a SECOND owned.admitted for
	// lane:1 by a real worker (fresh claim, not adoption — adoption appends no event).
	if n := len(lumenEventsOfType(events, engine.EventAttemptMinted)); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2 (lane:0 then the re-attempt lane:1)", n)
	}
	admits := lumenEventsOfType(events, engine.EventOwnedAdmitted)
	if len(admits) != 2 {
		t.Fatalf("owned.admitted count = %d, want 2 (one fresh claim per attempt)", len(admits))
	}
	a1 := decodeOwnedAdmitted(t, admits[1].Payload)
	if a1.Handle != "lane:1" || a1.Assignee == "" {
		t.Fatalf("second owned.admitted = {handle:%q assignee:%q}, want {lane:1, <a fresh worker>}", a1.Handle, a1.Assignee)
	}
	t.Logf("PROOF recovery: lane:0 stranded → lane:1 claimed fresh by %q → pass", a1.Assignee)

	// The loop settled pass and the run sealed pass.
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
		t.Fatalf("run.closed outcome = %q, want pass (recovery)", closed.Outcome)
	}

	// Each attempt's side effect ran exactly ONCE — both attempts claim the bare id "lane",
	// so two legit attempts append two lines; an adopted re-execution of lane:0 would add a
	// third.
	assertExecCountLines(t, cityDir, "lane", 2)
	t.Logf("PROOF two legit attempts, no adopted duplicate execution of lane:0")

	assertZeroControlBeads(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF run sealed PASS after a single kill; Verify(%s) clean", streamID)
}

// --- T-E3/T-E4 support helpers ----------------------------------------------

func seqsOf(events []graphstore.StoredEvent) []uint64 {
	out := make([]uint64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

func countBelow(seqs []uint64, cutoff uint64) int {
	n := 0
	for _, s := range seqs {
		if s < cutoff {
			n++
		}
	}
	return n
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// pgrepNonce returns the PIDs whose command line matches nonce (a process-table
// query — the house "query live state" rule). pgrep exits 1 with no matches, which is
// not an error here.
func pgrepNonce(t *testing.T, nonce string) []int {
	t.Helper()
	out, err := exec.Command("pgrep", "-f", nonce).CombinedOutput()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil // no matches
		}
		t.Fatalf("pgrep -f %q: %v (out=%s)", nonce, err, out)
	}
	var pids []int
	for _, line := range strings.Fields(strings.TrimSpace(string(out))) {
		if pid, cerr := strconv.Atoi(line); cerr == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}

// waitForNoncePID polls the process table until at least one hang session process
// carrying nonce exists (it appears only after the agent re-execs into the tagged
// sleep, i.e. after its claim has committed), then returns those PIDs so the caller can
// SIGKILL exactly once.
func waitForNoncePID(t *testing.T, nonce string, timeout time.Duration) []int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if pids := pgrepNonce(t, nonce); len(pids) > 0 {
			return pids
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no hang session process (nonce %q) appeared within %s", nonce, timeout)
	return nil
}

// waitForLumenSealTrackingRespawns drives a SINGLE-KILL run to seal WITHOUT re-killing
// anything — the honest firewall discipline. It returns the sealed event stream plus the
// distinct non-original session beads observed for the route (the churn bound). A timeout
// is the firewall-wedge HIGH: a respawn adopted the claimed row or reset the name-keyed
// grace clock, so the run never seals.
func waitForLumenSealTrackingRespawns(t *testing.T, gs *graphstore.Store, streamID, cityDir, template, claimantBeadID string, timeout time.Duration) ([]graphstore.StoredEvent, []string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	respawns := map[string]bool{}
	for time.Now().Before(deadline) {
		last := lumenStreamEvents(t, gs, streamID)
		if n := len(last); n > 0 && last[n-1].Type == engine.EventRunClosed {
			return last, keysOf(respawns)
		}
		if rows, err := listSessionsForTemplate(t, cityDir, template); err == nil {
			for _, r := range rows {
				if strings.TrimSpace(r.Template) != template {
					continue
				}
				if id := strings.TrimSpace(r.ID); id != "" && id != claimantBeadID && !respawns[id] {
					respawns[id] = true
					t.Logf("DIAG session bead %q (name %q) appeared for the route", id, r.SessionName)
				}
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	execFiles, _ := filepath.Glob(filepath.Join(cityDir, ".gc", "lumen-e2e-exec-count-*.txt"))
	execDump := map[string]string{}
	for _, f := range execFiles {
		if b, rerr := os.ReadFile(f); rerr == nil {
			execDump[filepath.Base(f)] = strings.TrimSpace(string(b))
		}
	}
	openSessions, _ := listSessionsForTemplate(t, cityDir, template)
	t.Fatalf("single-kill run %s did NOT seal within %s — WEDGE. Non-original session beads seen: %v; open sessions now: %v; exec markers: %v\nlast sequence: %v",
		streamID, timeout, keysOf(respawns), openSessions, execDump, lumenStreamTypes(lumenStreamEvents(t, gs, streamID)))
	return nil, nil
}

// assertExecCountLines asserts the per-bead exec-count marker file has exactly `want`
// non-empty lines — the "side effect ran exactly N times" proof. An adopted re-execution
// of a claimed row appends an extra line, so a mismatch is the wedge-adoption tripwire.
func assertExecCountLines(t *testing.T, cityDir, beadID string, want int) {
	t.Helper()
	path := filepath.Join(cityDir, ".gc", "lumen-e2e-exec-count-"+beadID+".txt")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading exec-count %q: %v", path, err)
	}
	got := 0
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			got++
		}
	}
	if got != want {
		t.Fatalf("exec-count for %q = %d lines, want %d (an adopted re-execution appends an extra line)", beadID, got, want)
	}
}

// assertTmuxSessionCountOnCitySocket proves >= n real tmux panes on the city's
// isolated -L socket (socket name == city name). Best-effort within a window so the
// brief pooled-worker panes are caught. Guard-scoped socket only; never the default
// server.
func assertTmuxSessionCountOnCitySocket(t *testing.T, cityDir string, n int) {
	t.Helper()
	socket := filepath.Base(cityDir)
	deadline := time.Now().Add(20 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		raw, err := exec.Command("tmux", "-L", socket, "list-sessions", "-F", "#{session_name}").CombinedOutput()
		last = string(raw)
		if err == nil {
			count := 0
			for _, line := range strings.Split(strings.TrimSpace(last), "\n") {
				if strings.TrimSpace(line) != "" {
					count++
				}
			}
			if count >= n {
				t.Logf("PROOF >= %d real tmux panes on isolated socket -L %s:\n%s", n, socket, strings.TrimRight(last, "\n"))
				return
			}
		}
		time.Sleep(150 * time.Millisecond)
	}
	t.Fatalf("did not observe >= %d tmux panes on isolated socket -L %s within window\nlast list-sessions:\n%s", n, socket, last)
}
