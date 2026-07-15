//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/session"
)

const (
	gcRunReviewQuorumInput       = `{"document_path":"/tmp/lumen-review-design.md","repository_path":"/tmp/lumen-review-repository","artifact_dir":"/tmp/lumen-review-artifacts","objective":"Make the design implementation-ready","lane_one_id":"implementation-realism","lane_two_id":"test-operability"}`
	gcRunReviewerCohort          = "reviewers"
	gcRunSynthesisCohort         = "synthesis"
	gcRunVerificationCohort      = "verification"
	gcRunReviewerReleaseFile     = ".gc/lumen-e2e-release-reviewers"
	gcRunSynthesisReleaseFile    = ".gc/lumen-e2e-release-synthesis"
	gcRunVerificationReleaseFile = ".gc/lumen-e2e-release-verification"
	gcRunAgentClaimTimeout       = 5 * time.Minute
	gcRunSessionSpawnTimeout     = 7 * time.Minute
	gcRunAgentReleaseTimeout     = 90 * time.Second
	gcRunSessionObserveTimeout   = 30 * time.Second
	gcRunSessionReturnTimeout    = 3 * time.Minute
	gcRunSessionCommandTimeout   = 10 * time.Second
	gcRunCommandShutdownTimeout  = 10 * time.Second
	gcRunCommandPostKillTimeout  = 5 * time.Second
)

func TestGCRunDemoReturnGateRequiresNoLifecycleOrProcessActivity(t *testing.T) {
	t.Parallel()

	active := map[string]sessionListRow{"laneOneAgent": {ID: "session-one"}}
	if gcRunDemoSessionsReturned(active, nil) {
		t.Fatal("return gate passed while a demo lifecycle row was active")
	}
	if gcRunDemoSessionsReturned(nil, []int{1234}) {
		t.Fatal("return gate passed while a demo subprocess was active")
	}
	if !gcRunDemoSessionsReturned(nil, nil) {
		t.Fatal("return gate rejected an empty lifecycle and process snapshot")
	}
}

// TestGCRunLumenReviewQuorumE2E is the public-front-door proof for a
// City-backed Lumen run. The real gc binary resolves the City, enqueues the
// sibling IR, blocks for the durable terminal outcome, and fans the two bound
// reviewer routes to simultaneously observable subprocess sessions before the
// default-route synthesis step and explicitly routed verification step start in
// dependency order. The subprocesses prove orchestration plumbing only; live
// inference acceptance is covered separately.
func TestGCRunLumenReviewQuorumE2E(t *testing.T) {
	cityDir, env := setupGCRunLumenCity(t)

	// The existing routed-quorum integration gate budgets eight minutes after
	// the first journal admission. This front-door test starts its clock before
	// enqueue, so include the initial controller admission window as well.
	ctx, cancel := context.WithTimeout(context.Background(), 17*time.Minute)
	defer cancel()
	var stdout, stderr bytes.Buffer
	runCmd := exec.CommandContext(ctx, gcBinary,
		"run", "examples/lumen/review-quorum.lumen",
		"--route", lumenDoRoute,
		"--input", gcRunReviewQuorumInput,
	)
	runCmd.Dir = repoRoot(t)
	runCmd.Env = replaceEnv(env, "GC_CITY_PATH", cityDir)
	runCmd.Stdout = &stdout
	runCmd.Stderr = &stderr
	runCmd.WaitDelay = 2 * time.Second
	if err := runCmd.Start(); err != nil {
		t.Fatalf("starting gc run: %v", err)
	}
	runDone := make(chan error, 1)
	go func() { runDone <- runCmd.Wait() }()
	runFinished := false
	stopRun := func() (error, bool) {
		runErr, joined := cancelAndJoinGCRun(cancel, runCmd, runDone)
		if joined {
			runFinished = true
		}
		return runErr, joined
	}
	t.Cleanup(func() {
		if runFinished {
			return
		}
		_, _ = stopRun()
	})

	// Synchronize on the workers' durable claim markers, then require one
	// unfiltered session-list response to contain both bound reviewer
	// templates at once. Separate template-filtered polls would only prove two
	// sessions existed at different times.
	// A saturated default-backend host can spend several minutes between
	// admission and the two subprocess runtimes reaching their claim loops.
	// Keep this below the overall run deadline while matching the existing
	// routed-quorum gate's deliberately generous startup budgets.
	if err := waitForGCRunClaims(cityDir, gcRunReviewerCohort, 2, gcRunSessionSpawnTimeout); err != nil {
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("%v; gc run did not exit after cancellation", err)
		}
		t.Fatalf("%v; gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", err, runErr, stdout.String(), stderr.String())
	}
	var reviewerSnapshot map[string]sessionListRow
	deadline := time.Now().Add(gcRunSessionObserveTimeout)
	var lastListErr error
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			runFinished = true
			t.Fatalf("gc run exited before both reviewers were concurrently observable: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		default:
		}

		rows, err := listAllGCRunSessions(cityDir)
		if err != nil {
			lastListErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		reviewerSnapshot = currentGCRunSessions(rows, "laneOneAgent", "laneTwoAgent")
		if len(reviewerSnapshot) == 2 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(reviewerSnapshot) != 2 {
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("one gc session list --json snapshot never contained both claimed reviewers within %s (last error: %v); gc run did not exit after cancellation", gcRunSessionObserveTimeout, lastListErr)
		}
		t.Fatalf("one gc session list --json snapshot never contained both claimed reviewers within %s (last error: %v); gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", gcRunSessionObserveTimeout, lastListErr, runErr, stdout.String(), stderr.String())
	}
	one := reviewerSnapshot["laneOneAgent"]
	two := reviewerSnapshot["laneTwoAgent"]
	if strings.TrimSpace(one.ID) == "" || strings.TrimSpace(two.ID) == "" || one.ID == two.ID || strings.TrimSpace(one.SessionName) == "" || strings.TrimSpace(two.SessionName) == "" || one.SessionName == two.SessionName {
		t.Fatalf("reviewer sessions are not distinct: laneOne=%+v laneTwo=%+v", one, two)
	}
	t.Logf("PROOF one unfiltered session snapshot contains laneOneAgent=%s and laneTwoAgent=%s", one.SessionName, two.SessionName)
	releaseGCRunAgents(t, cityDir, gcRunReviewerReleaseFile)

	// The synthesis Agent uses the default --route. Hold its claimed work until
	// one public session-list snapshot observes that exact template, then release
	// it to settle the run. This proves the third Agent existed; a final absence
	// check alone would be vacuous if synthesis were never spawned.
	if err := waitForGCRunClaims(cityDir, gcRunSynthesisCohort, 1, gcRunSessionSpawnTimeout); err != nil {
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("%v; gc run did not exit after cancellation", err)
		}
		t.Fatalf("%v; gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", err, runErr, stdout.String(), stderr.String())
	}
	var synthesisSnapshot map[string]sessionListRow
	deadline = time.Now().Add(gcRunSessionObserveTimeout)
	lastListErr = nil
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			runFinished = true
			t.Fatalf("gc run exited before the claimed synthesis session was observable: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		default:
		}

		rows, err := listAllGCRunSessions(cityDir)
		if err != nil {
			lastListErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		synthesisSnapshot = currentGCRunSessions(rows, lumenDoRoute)
		if len(synthesisSnapshot) == 1 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(synthesisSnapshot) != 1 {
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("gc session list --json never contained claimed synthesis route %q within %s (last error: %v); gc run did not exit after cancellation", lumenDoRoute, gcRunSessionObserveTimeout, lastListErr)
		}
		t.Fatalf("gc session list --json never contained claimed synthesis route %q within %s (last error: %v); gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", lumenDoRoute, gcRunSessionObserveTimeout, lastListErr, runErr, stdout.String(), stderr.String())
	}
	synthesis := synthesisSnapshot[lumenDoRoute]
	if strings.TrimSpace(synthesis.ID) == "" || strings.TrimSpace(synthesis.SessionName) == "" || synthesis.ID == one.ID || synthesis.ID == two.ID || synthesis.SessionName == one.SessionName || synthesis.SessionName == two.SessionName {
		t.Fatalf("synthesis session is not distinct from both reviewers: synthesis=%+v laneOne=%+v laneTwo=%+v", synthesis, one, two)
	}
	t.Logf("PROOF claimed synthesis route %s is concurrently observable as %s", lumenDoRoute, synthesis.SessionName)
	releaseGCRunAgents(t, cityDir, gcRunSynthesisReleaseFile)

	// Verification is a separate final Agent over the revised document. Observe
	// its claimed session before release so a terminal pass cannot be mistaken
	// for proof that the phase ran.
	if runErr, exited, err := waitForGCRunClaimsWhileActive(runDone, cityDir, gcRunVerificationCohort, 1, gcRunSessionSpawnTimeout); err != nil {
		if exited {
			runFinished = true
			t.Fatalf("%v: %v\nstdout:\n%s\nstderr:\n%s", err, runErr, stdout.String(), stderr.String())
		}
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("%v; gc run did not exit after cancellation", err)
		}
		t.Fatalf("%v; gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", err, runErr, stdout.String(), stderr.String())
	}
	var verificationSnapshot map[string]sessionListRow
	deadline = time.Now().Add(gcRunSessionObserveTimeout)
	lastListErr = nil
	for time.Now().Before(deadline) {
		select {
		case err := <-runDone:
			runFinished = true
			t.Fatalf("gc run exited before the claimed verification session was observable: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		default:
		}

		rows, err := listAllGCRunSessions(cityDir)
		if err != nil {
			lastListErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		verificationSnapshot = currentGCRunSessions(rows, "verifierAgent")
		if len(verificationSnapshot) == 1 {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if len(verificationSnapshot) != 1 {
		runErr, joined := stopRun()
		if !joined {
			t.Fatalf("gc session list --json never contained claimed verification route within %s (last error: %v); gc run did not exit after cancellation", gcRunSessionObserveTimeout, lastListErr)
		}
		t.Fatalf("gc session list --json never contained claimed verification route within %s (last error: %v); gc run after cancellation: %v\nstdout:\n%s\nstderr:\n%s", gcRunSessionObserveTimeout, lastListErr, runErr, stdout.String(), stderr.String())
	}
	verification := verificationSnapshot["verifierAgent"]
	if strings.TrimSpace(verification.ID) == "" || strings.TrimSpace(verification.SessionName) == "" ||
		verification.ID == one.ID || verification.ID == two.ID || verification.ID == synthesis.ID ||
		verification.SessionName == one.SessionName || verification.SessionName == two.SessionName || verification.SessionName == synthesis.SessionName {
		t.Fatalf("verification session is not distinct from prior phases: verification=%+v synthesis=%+v laneOne=%+v laneTwo=%+v", verification, synthesis, one, two)
	}
	t.Logf("PROOF claimed verification route is concurrently observable as %s", verification.SessionName)
	releaseGCRunAgents(t, cityDir, gcRunVerificationReleaseFile)

	if err := <-runDone; err != nil {
		runFinished = true
		if ctx.Err() != nil {
			t.Fatalf("gc run timed out: %v\nstdout:\n%s\nstderr:\n%s", ctx.Err(), stdout.String(), stderr.String())
		}
		t.Fatalf("gc run failed: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	runFinished = true
	if !strings.Contains(stdout.String(), "outcome: pass") {
		t.Fatalf("gc run exited zero without a pass outcome\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
	}

	streamID := parseGCRunStreamID(t, stdout.String())
	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(context.Background(), journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()
	events := lumenStreamEvents(t, gs, streamID)
	assertGCRunReviewPhaseOrdering(t, events)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}

	waitForGCRunDemoSessionsReturned(t, cityDir, lumenKillNonce(filepath.Base(cityDir)), gcRunSessionReturnTimeout)
	t.Logf("PROOF gc run exited 0 after synthesis and all demo sessions returned (stream %s)", streamID)
}

func setupGCRunLumenCity(t *testing.T) (string, []string) {
	t.Helper()
	env := newIsolatedCommandEnv(t, true)
	cityName := uniqueCityName()
	cityDir := filepath.Join(t.TempDir(), cityName)

	nonceEnv := "GC_LUMEN_E2E_NONCE=" + lumenKillNonce(cityName)
	barrierCommand := func(cohort, releaseFile string, barrier int) string {
		return fmt.Sprintf(
			"%s GC_LUMEN_E2E_CLAIM_TIMEOUT_SECONDS=%d GC_LUMEN_E2E_RELEASE_TIMEOUT_SECONDS=%d GC_LUMEN_E2E_COHORT=%s GC_LUMEN_E2E_RELEASE_FILE=%s GC_LUMEN_E2E_BARRIER=%d bash %s && gc runtime drain-ack",
			nonceEnv,
			int(gcRunAgentClaimTimeout/time.Second),
			int(gcRunAgentReleaseTimeout/time.Second),
			cohort,
			releaseFile,
			barrier,
			agentScript("lumen-do-barrier.sh"),
		)
	}
	reviewerCommand := barrierCommand(gcRunReviewerCohort, gcRunReviewerReleaseFile, 2)
	synthesisCommand := barrierCommand(gcRunSynthesisCohort, gcRunSynthesisReleaseFile, 1)
	verificationCommand := barrierCommand(gcRunVerificationCohort, gcRunVerificationReleaseFile, 1)
	agentBlock := func(name, command string) string {
		return fmt.Sprintf("\n[[agent]]\nname = %q\nmax_active_sessions = 1\nstart_command = %q\n", name, command)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = %q

[session]
provider = "subprocess"

[daemon]
# The demo exercises event-driven Lumen and work-close wakeups. Keep the
# correctness patrol slower than a saturated default-backend reconciliation
# cycle so a perpetually-ready ticker cannot starve those queued wakeups.
patrol_interval = "1m"
`, cityName) +
		agentBlock("laneOneAgent", reviewerCommand) +
		agentBlock("laneTwoAgent", reviewerCommand) +
		agentBlock(lumenDoRoute, synthesisCommand) +
		agentBlock("verifierAgent", verificationCommand)

	configPath := filepath.Join(t.TempDir(), "gc-run-lumen.toml")
	if err := os.WriteFile(configPath, []byte(cityToml), 0o644); err != nil {
		t.Fatalf("writing gc-run config: %v", err)
	}
	initCityWithManagedDoltRecovery(t, env, configPath, cityDir)
	registerCityCommandEnv(cityDir, env)
	t.Cleanup(func() {
		unregisterCityCommandEnv(cityDir)
		_, _ = runGCDoltWithEnv(env, "", "stop", cityDir)
		_, _ = runGCDoltWithEnv(env, "", "supervisor", "stop", "--wait")
		cleanupTestCityDir(cityDir)
	})

	if out, err := runGCDoltWithEnv(env, cityDir, "migrate", "graph-journal", "init"); err != nil {
		t.Fatalf("gc migrate graph-journal init failed: %v\noutput: %s", err, out)
	}
	waitForControllerReady(t, cityDir, 30*time.Second)
	return cityDir, env
}

func waitForGCRunClaims(cityDir, cohort string, count int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		markers, err := filepath.Glob(filepath.Join(cityDir, ".gc", fmt.Sprintf("lumen-e2e-claimed-%s-*", cohort)))
		if err != nil {
			return fmt.Errorf("glob %s claim markers: %w", cohort, err)
		}
		if len(markers) >= count {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%d %s claim markers did not appear within %s", count, cohort, timeout)
}

func waitForGCRunClaimsWhileActive(runDone <-chan error, cityDir, cohort string, count int, timeout time.Duration) (runErr error, exited bool, err error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case runErr := <-runDone:
			return runErr, true, fmt.Errorf("gc run exited before %d %s claim markers appeared", count, cohort)
		default:
		}

		markers, globErr := filepath.Glob(filepath.Join(cityDir, ".gc", fmt.Sprintf("lumen-e2e-claimed-%s-*", cohort)))
		if globErr != nil {
			return nil, false, fmt.Errorf("glob %s claim markers: %w", cohort, globErr)
		}
		if len(markers) >= count {
			return nil, false, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return nil, false, fmt.Errorf("%d %s claim markers did not appear within %s", count, cohort, timeout)
}

func releaseGCRunAgents(t *testing.T, cityDir, releaseFile string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(cityDir, filepath.FromSlash(releaseFile)), nil, 0o644); err != nil {
		t.Fatalf("releasing agents through %q: %v", releaseFile, err)
	}
}

func listAllGCRunSessions(cityDir string) ([]sessionListRow, error) {
	out, err := runCommand(
		cityDir,
		commandEnvForDir(cityDir, true),
		gcRunSessionCommandTimeout,
		gcBinary,
		"session", "list", "--json",
	)
	if err != nil {
		return nil, fmt.Errorf("gc session list --json: %w\noutput: %s", err, out)
	}
	var envelope struct {
		Sessions []sessionListRow `json:"sessions"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &envelope); err != nil {
		return nil, fmt.Errorf("decoding gc session list --json: %w\noutput: %s", err, out)
	}
	return envelope.Sessions, nil
}

func cancelAndJoinGCRun(cancel context.CancelFunc, runCmd *exec.Cmd, runDone <-chan error) (error, bool) {
	cancel()
	select {
	case err := <-runDone:
		return err, true
	case <-time.After(gcRunCommandShutdownTimeout):
	}
	if runCmd.Process != nil {
		_ = runCmd.Process.Kill()
	}
	select {
	case err := <-runDone:
		return err, true
	case <-time.After(gcRunCommandPostKillTimeout):
		return nil, false
	}
}

func currentGCRunSessions(rows []sessionListRow, templates ...string) map[string]sessionListRow {
	want := make(map[string]bool, len(templates))
	for _, template := range templates {
		want[template] = true
	}
	present := make(map[string]sessionListRow, len(templates))
	for _, row := range rows {
		if !row.Closed && want[row.Template] && strings.TrimSpace(row.SessionName) != "" {
			present[row.Template] = row
		}
	}
	return present
}

func isGCRunSessionWorkActive(row sessionListRow) bool {
	state := session.State(strings.TrimSpace(row.State))
	if session.IsTemplateOverrideRuntimeActive(state) {
		return true
	}
	switch state {
	case session.StateAsleep, session.StateSuspended, session.StateDrained,
		session.StateArchived, session.StateFailedCreate:
		return false
	default:
		// Treat an unrecognized state conservatively so schema drift cannot make
		// the final absence assertion pass without proving that work returned.
		return true
	}
}

func currentGCRunWorkActiveSessions(rows []sessionListRow, templates ...string) map[string]sessionListRow {
	present := currentGCRunSessions(rows, templates...)
	for template, row := range present {
		if !isGCRunSessionWorkActive(row) {
			delete(present, template)
		}
	}
	return present
}

func TestGCRunSessionWorkActiveClassification(t *testing.T) {
	t.Parallel()
	tests := []struct {
		state string
		want  bool
	}{
		{state: "start-pending", want: true},
		{state: "creating", want: true},
		{state: "active", want: true},
		{state: "awake", want: true},
		{state: "draining", want: true},
		{state: "asleep", want: false},
		{state: "suspended", want: false},
		{state: "drained", want: false},
		{state: "archived", want: false},
		{state: "failed-create", want: false},
		{state: "quarantined", want: true},
		{state: "future-state", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.state, func(t *testing.T) {
			row := sessionListRow{State: tt.state}
			if got := isGCRunSessionWorkActive(row); got != tt.want {
				t.Fatalf("isGCRunSessionWorkActive(state=%q) = %t, want %t", tt.state, got, tt.want)
			}
		})
	}
}

func TestGCRunNonceProcessPatternMatchesOnlyTaggedArgv0(t *testing.T) {
	t.Parallel()
	nonce := "lumenhang-city.42"
	pattern := regexp.MustCompile(lumenNonceProcessPattern(nonce))
	tests := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "tagged argv zero", command: nonce + " /tmp/lumen-do-barrier.sh", want: true},
		{name: "tag only", command: nonce, want: true},
		{name: "persisted command argument", command: "bd update --set-metadata command=GC_LUMEN_E2E_NONCE=" + nonce, want: false},
		{name: "longer tag prefix", command: nonce + "-other /tmp/lumen-do-barrier.sh", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := pattern.MatchString(tt.command); got != tt.want {
				t.Fatalf("pattern %q match for %q = %t, want %t", pattern, tt.command, got, tt.want)
			}
		})
	}
}

func parseGCRunStreamID(t *testing.T, output string) string {
	t.Helper()
	for _, field := range strings.Fields(output) {
		candidate := strings.Trim(field, "()")
		if strings.HasPrefix(candidate, lumenDoStreamHint) {
			return candidate
		}
	}
	t.Fatalf("could not parse a %s… stream id from gc run output: %q", lumenDoStreamHint, output)
	return ""
}

func assertGCRunReviewPhaseOrdering(t *testing.T, events []graphstore.StoredEvent) {
	t.Helper()
	expected := map[string]bool{
		"reviewLaneOne:0": true,
		"reviewLaneTwo:0": true,
		"synthesize:0":    true,
		"verify:0":        true,
	}
	admitted := make(map[string]uint64, len(expected))
	settled := make(map[string]uint64, len(expected))
	var runClosed uint64
	for _, event := range events {
		switch event.Type {
		case engine.EventOutcomeSettled:
			var outcome lumenOutcomeSettled
			if err := json.Unmarshal(event.Payload, &outcome); err != nil {
				t.Fatalf("decoding outcome.settled: %v", err)
			}
			if !expected[outcome.Activation] {
				continue
			}
			if settled[outcome.Activation] != 0 {
				t.Fatalf("duplicate settlement for %s", outcome.Activation)
			}
			if outcome.Outcome != string(engine.OutcomePass) {
				t.Fatalf("%s settled %q, want pass", outcome.Activation, outcome.Outcome)
			}
			settled[outcome.Activation] = event.Seq
		case engine.EventOwnedAdmitted:
			owned := decodeOwnedAdmitted(t, event.Payload)
			if !expected[owned.Activation] {
				continue
			}
			if admitted[owned.Activation] != 0 {
				t.Fatalf("duplicate admission for %s", owned.Activation)
			}
			if owned.Kind != engine.OwnedKindWorkBead {
				t.Fatalf("%s admitted kind %q, want %q", owned.Activation, owned.Kind, engine.OwnedKindWorkBead)
			}
			admitted[owned.Activation] = event.Seq
		case engine.EventRunClosed:
			if runClosed != 0 {
				t.Fatal("journal contains duplicate lumen.run.closed events")
			}
			closed := decodeRunClosed(t, event.Payload)
			if closed.Outcome != engine.OutcomePass {
				t.Fatalf("run closed %q, want pass", closed.Outcome)
			}
			runClosed = event.Seq
		}
	}
	for activation := range expected {
		if admitted[activation] == 0 || settled[activation] == 0 {
			t.Fatalf("missing admission or settlement for %s; sequence=%v", activation, lumenStreamTypes(events))
		}
		if admitted[activation] >= settled[activation] {
			t.Fatalf("%s admitted at seq %d but settled at seq %d", activation, admitted[activation], settled[activation])
		}
	}
	for _, reviewer := range []string{"reviewLaneOne:0", "reviewLaneTwo:0"} {
		if admitted["synthesize:0"] <= settled[reviewer] {
			t.Fatalf("synthesis admitted at seq %d before %s settled at seq %d", admitted["synthesize:0"], reviewer, settled[reviewer])
		}
	}
	if admitted["verify:0"] <= settled["synthesize:0"] {
		t.Fatalf("verification admitted at seq %d before synthesis settled at seq %d", admitted["verify:0"], settled["synthesize:0"])
	}
	if runClosed <= settled["verify:0"] {
		t.Fatalf("run closed at seq %d before verification settled at seq %d", runClosed, settled["verify:0"])
	}
	if len(events) == 0 || events[len(events)-1].Seq != runClosed {
		t.Fatalf("run.closed seq %d is not the final stream event", runClosed)
	}
	t.Logf("PROOF reviewers settled at %d/%d; synthesis admitted/settled at %d/%d; verification admitted/settled at %d/%d; run closed at %d",
		settled["reviewLaneOne:0"], settled["reviewLaneTwo:0"], admitted["synthesize:0"], settled["synthesize:0"], admitted["verify:0"], settled["verify:0"], runClosed)
}

func waitForGCRunDemoSessionsReturned(t *testing.T, cityDir, nonce string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastPresent map[string]sessionListRow
	var lastPIDs []int
	var lastErr error
	var absentSince time.Time
	for time.Now().Before(deadline) {
		rows, err := listAllGCRunSessions(cityDir)
		if err != nil {
			lastErr = err
			absentSince = time.Time{}
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastErr = nil
		lastPresent = currentGCRunWorkActiveSessions(rows, "laneOneAgent", "laneTwoAgent", lumenDoRoute, "verifierAgent")
		lastPIDs = pgrepNonce(t, nonce)
		if gcRunDemoSessionsReturned(lastPresent, lastPIDs) {
			if absentSince.IsZero() {
				absentSince = time.Now()
			} else if time.Since(absentSince) >= 500*time.Millisecond {
				return
			}
		} else {
			absentSince = time.Time{}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("demo subprocesses did not return after gc run exited: pids=%v lifecycle=%v (last list error: %v)", lastPIDs, lastPresent, lastErr)
}

func gcRunDemoSessionsReturned(active map[string]sessionListRow, pids []int) bool {
	return len(active) == 0 && len(pids) == 0
}
