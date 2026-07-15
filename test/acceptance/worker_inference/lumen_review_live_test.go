//go:build acceptance_c

package workerinference_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/session"
	workerpkg "github.com/gastownhall/gascity/internal/worker"
	helpers "github.com/gastownhall/gascity/test/acceptance/helpers"
)

const (
	liveReviewOptInEnv         = "GC_LUMEN_LIVE_REVIEW"
	liveReviewRunTimeout       = 40 * time.Minute
	liveReviewPhaseTimeout     = 15 * time.Minute
	liveReviewSessionPoll      = time.Second
	liveReviewCommandTimeout   = 2 * time.Minute
	liveReviewReturnTimeout    = 3 * time.Minute
	liveReviewHistoryGrace     = 2 * time.Minute
	liveReviewStablePolls      = 3
	liveReviewMinChangedLines  = 8
	liveReviewMinSummaryLength = 60
)

var liveReviewExpectedProviders = map[string]string{
	"laneOneAgent":   "claude",
	"laneTwoAgent":   "codex",
	"synthesisAgent": "claude",
	"verifierAgent":  "codex",
}

var liveReviewRuntimeOverrideKeys = []string{
	"GC_SESSION",
	"GC_DOLT",
	"GC_DOLT_REAL_BINARY",
	"GC_DOLT_MANAGED_LOCAL",
	"GC_BEADS",
	"GC_BEADS_BACKEND",
	"BEADS_BACKEND",
	"GC_BEADS_BD_SCRIPT",
	"GC_BEADS_FORCE_FALLBACK",
	"GC_DOLT_HOST",
	"GC_DOLT_PORT",
	"GC_DOLT_USER",
	"GC_DOLT_PASSWORD",
	"GC_DOLT_SOCKET",
	"GC_DOLT_SHARED_SERVER",
	"GC_DOLT_SERVER_HOST",
	"GC_DOLT_SERVER_PORT",
	"GC_DOLT_SERVER_USER",
	"GC_DOLT_SERVER_PASSWORD",
	"GC_DOLT_SERVER_SOCKET",
	"GC_DOLT_DATA_DIR",
	"GC_DOLT_STATE_FILE",
	"GC_DOLT_PROVIDER_STATE_FILE",
	"BEADS_DOLT_HOST",
	"BEADS_DOLT_PORT",
	"BEADS_DOLT_USER",
	"BEADS_DOLT_PASSWORD",
	"BEADS_DOLT_SOCKET",
	"BEADS_DOLT_SHARED_SERVER",
	"BEADS_DOLT_SERVER_HOST",
	"BEADS_DOLT_SERVER_PORT",
	"BEADS_DOLT_SERVER_USER",
	"BEADS_DOLT_SERVER_PASSWORD",
	"BEADS_DOLT_SERVER_SOCKET",
	"BEADS_DOLT_SERVER_MODE",
	"BEADS_DOLT_SERVER_DATABASE",
	"BEADS_DOLT_DATABASE",
	"BEADS_DOLT_AUTO_START",
	"DOLT_HOST",
	"DOLT_PORT",
	"DOLT_USER",
	"DOLT_PASSWORD",
	"DOLT_SOCKET",
	"DOLT_SHARED_SERVER",
	"DOLT_SERVER_HOST",
	"DOLT_SERVER_PORT",
	"DOLT_SERVER_USER",
	"DOLT_SERVER_PASSWORD",
	"DOLT_SERVER_SOCKET",
	"DOLT_CLI_PASSWORD",
}

// TestLumenRealDesignReview is the opt-in, live-inference proof for the public
// `gc run <file.lumen>` path. It deliberately uses the normal managed bd+Dolt
// backend and real tmux-backed Claude/Codex sessions; deterministic CI covers
// the same orchestration contract without spending inference tokens.
func TestLumenRealDesignReview(t *testing.T) {
	if os.Getenv(liveReviewOptInEnv) != "1" {
		t.Skipf("set %s=1 to run the real Claude/Codex Lumen design review", liveReviewOptInEnv)
	}

	for _, name := range []string{"bd", "claude", "codex", "dolt", "git", "jq", "tmux"} {
		if _, err := exec.LookPath(name); err != nil {
			t.Fatalf("%s=1 requires %s in PATH: %v", liveReviewOptInEnv, name, err)
		}
	}

	root, err := liveWorkerTempDir(t)
	require.NoError(t, err)
	if os.Getenv("GC_ACCEPTANCE_KEEP") != "1" {
		t.Cleanup(func() {
			_ = makeLiveReviewTreeOwnerWritable(root)
			_ = os.RemoveAll(root)
		})
	}
	t.Logf("live review workspace: %s", root)

	gcPath, err := helpers.ResolveGCPath(liveEnv)
	require.NoError(t, err)
	gcHome := filepath.Join(root, "gc-home")
	runtimeDir := filepath.Join(root, "runtime")
	for _, dir := range []string{gcHome, runtimeDir} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}
	require.NoError(t, helpers.WriteSupervisorConfig(gcHome))
	require.NoError(t, writeLiveReviewDoltConfig(gcHome))

	// NewEnv's defaults are intentionally test fakes. Removing all three
	// overrides selects the production defaults: tmux plus managed bd+Dolt.
	env := helpers.NewEnv(gcPath, gcHome, runtimeDir)
	scrubLiveReviewRuntimeOverrides(env)
	env.With("DOLT_ROOT_PATH", gcHome).
		With("BEADS_DOLT_AUTO_START", "0").
		With("GIT_OPTIONAL_LOCKS", "0")
	require.Empty(t, env.Get("GC_SESSION"))
	require.Empty(t, env.Get("GC_BEADS"))
	require.Empty(t, env.Get("GC_DOLT"))

	claudeAuth, err := stageClaudeAuth(gcHome, env)
	require.NoError(t, err, "stage real Claude authentication")
	env.With("CLAUDE_CONFIG_DIR", filepath.Join(gcHome, ".claude"))
	codexAuth, err := stageLiveReviewCodexAuth(gcHome, env)
	require.NoError(t, err, "stage real Codex authentication")
	t.Logf("provider auth staged: claude=%s codex=%s", claudeAuth, codexAuth)
	requireLiveReviewAuth(t, env, "claude", "auth", "status")
	requireLiveReviewCodexInference(t, env, root)

	repoRoot := helpers.FindModuleRoot()
	repositoryPath := filepath.Join(root, "repository")
	repositoryCommit, err := stageLiveReviewRepositorySnapshot(repoRoot, repositoryPath)
	require.NoError(t, err, "stage clean committed repository evidence")
	repositoryManifest, err := liveReviewContentManifest(repositoryPath)
	require.NoError(t, err, "manifest committed repository evidence")
	t.Logf("repository evidence: %s at %s (read-only)", repositoryCommit, repositoryPath)
	templateDir := filepath.Join(repoRoot, "examples", "lumen", "review-quorum-live")
	formulaSource := filepath.Join(repoRoot, "examples", "lumen", "review-quorum.lumen")
	formulaIR := formulaSource + ".json"
	designSource := filepath.Join(repositoryPath, "engdocs", "design", "gc-reload-design.md")
	require.DirExists(t, templateDir)
	for _, path := range []string{formulaSource, formulaIR, designSource} {
		require.FileExists(t, path)
	}

	city := helpers.NewCityInRoot(t, env, filepath.Join(root, "cities"))
	city.InitFromNoStart(templateDir)
	requireLiveReviewProviderRoutes(t, city.Dir)
	require.NoError(t, seedLiveProviderStateFor(workerpkg.ProfileClaudeTmuxCLI, gcHome, city.Dir))
	require.NoError(t, seedLiveProviderStateFor(workerpkg.ProfileCodexTmuxCLI, gcHome, city.Dir))
	requireLiveReviewProviderIsolation(t, env, gcHome, city.Dir)

	require.NoError(t, copyLiveReviewFile(formulaSource, filepath.Join(city.Dir, "review-quorum.lumen")))
	require.NoError(t, copyLiveReviewFile(formulaIR, filepath.Join(city.Dir, "review-quorum.lumen.json")))
	workDir := filepath.Join(city.Dir, "work")
	artifactDir := filepath.Join(city.Dir, "review-artifacts")
	require.NoError(t, os.MkdirAll(workDir, 0o755))
	require.NoError(t, os.MkdirAll(artifactDir, 0o755))
	documentPath := filepath.Join(workDir, "gc-reload-design.md")
	require.NoError(t, copyLiveReviewFile(designSource, documentPath))
	require.True(t, filepath.IsAbs(documentPath))
	require.True(t, filepath.IsAbs(artifactDir))
	pristineDocument, err := os.ReadFile(designSource)
	require.NoError(t, err)

	initOut, err := runGCWithTimeout(liveReviewCommandTimeout, env, city.Dir, "migrate", "graph-journal", "init")
	require.NoError(t, err, "gc migrate graph-journal init\n%s", initOut)
	city.StartWithSupervisor()
	requireLiveReviewManagedDolt(t, city.Dir)

	input, err := json.Marshal(map[string]string{
		"document_path":   documentPath,
		"repository_path": repositoryPath,
		"artifact_dir":    artifactDir,
		"objective":       "Make the gc reload design implementation-ready",
		"lane_one_id":     "implementation-realism",
		"lane_two_id":     "test-operability",
	})
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), liveReviewRunTimeout)
	gcRun := exec.CommandContext(ctx, gcPath,
		"run", "review-quorum.lumen",
		"--route", "synthesisAgent",
		"--input", string(input),
	)
	gcRun.Dir = city.Dir
	gcRun.Env = env.List()
	var runStdout, runStderr bytes.Buffer
	gcRun.Stdout = &runStdout
	gcRun.Stderr = &runStderr
	gcRun.WaitDelay = 2 * time.Second
	require.NoError(t, gcRun.Start())
	runDone := make(chan struct{})
	var runErr error
	go func() {
		runErr = gcRun.Wait()
		close(runDone)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
			return
		case <-time.After(10 * time.Second):
		}
		if gcRun.Process != nil {
			_ = gcRun.Process.Kill()
		}
		select {
		case <-runDone:
		case <-time.After(5 * time.Second):
		}
	})

	observed := make(map[string]liveReviewSession)
	for name, row := range waitForLiveReviewPhase(t, env, city.Dir, runDone, &runStdout, &runStderr, map[string]string{
		"laneOneAgent": "claude",
		"laneTwoAgent": "codex",
	}, true, liveReviewPhaseTimeout) {
		observed[name] = row
		requireLiveReviewPeek(t, env, city.Dir, row)
	}
	for name, row := range waitForLiveReviewPhase(t, env, city.Dir, runDone, &runStdout, &runStderr, map[string]string{
		"synthesisAgent": "claude",
	}, false, liveReviewPhaseTimeout) {
		observed[name] = row
		requireLiveReviewPeek(t, env, city.Dir, row)
	}
	for name, row := range waitForLiveReviewPhase(t, env, city.Dir, runDone, &runStdout, &runStderr, map[string]string{
		"verifierAgent": "codex",
	}, false, liveReviewPhaseTimeout) {
		observed[name] = row
		requireLiveReviewPeek(t, env, city.Dir, row)
	}
	requireLiveReviewDistinctSessions(t, observed)

	<-runDone
	require.NoError(t, runErr, "gc run failed\nstdout:\n%s\nstderr:\n%s", runStdout.String(), runStderr.String())
	require.NoError(t, ctx.Err(), "gc run exceeded %s\nstdout:\n%s\nstderr:\n%s", liveReviewRunTimeout, runStdout.String(), runStderr.String())
	cancel()
	require.Contains(t, runStdout.String(), "outcome: pass", "gc run did not report terminal pass\nstderr:\n%s", runStderr.String())
	for _, step := range []string{"reviewLaneOne", "reviewLaneTwo", "synthesize", "verify"} {
		require.Contains(t, runStdout.String(), step, "gc run completion omitted %s", step)
	}
	t.Logf("gc run output:\n%s", strings.TrimSpace(runStdout.String()))
	finalSessions := waitForLiveReviewSessionsReturned(t, env, city.Dir, observed, liveReviewReturnTimeout)
	requireLiveReviewSessionProviders(t, env, city.Dir, finalSessions, liveReviewExpectedProviders)
	finalRepositoryManifest, err := liveReviewContentManifest(repositoryPath)
	require.NoError(t, err, "manifest repository evidence after inference")
	require.NoError(t, compareLiveReviewContentManifests(repositoryManifest, finalRepositoryManifest))
	requireLiveReviewRepositorySnapshotUnchanged(t, repositoryPath, repositoryCommit)

	streamID := parseLiveReviewStreamID(t, runStdout.String())
	journalOut, err := runGCWithTimeout(liveReviewCommandTimeout, env, city.Dir, "graph", "journal", streamID)
	require.NoError(t, err, "gc graph journal %s\n%s", streamID, journalOut)
	require.Contains(t, journalOut, engine.EventRunClosed)
	require.Contains(t, journalOut, string(engine.OutcomePass))
	t.Logf("graph journal:\n%s", strings.TrimSpace(journalOut))

	laneOnePath := filepath.Join(artifactDir, "lane-one.json")
	laneTwoPath := filepath.Join(artifactDir, "lane-two.json")
	synthesisPath := filepath.Join(artifactDir, "synthesis.json")
	verificationPath := filepath.Join(artifactDir, "verification.json")
	laneOne := readLiveReviewJSON[liveReviewLaneArtifact](t, laneOnePath)
	laneTwo := readLiveReviewJSON[liveReviewLaneArtifact](t, laneTwoPath)
	requireLiveReviewLane(t, laneOne, "implementation-realism", "claude", documentPath, repositoryPath, laneOnePath)
	requireLiveReviewLane(t, laneTwo, "test-operability", "codex", documentPath, repositoryPath, laneTwoPath)

	synthesis := readLiveReviewJSON[liveReviewSynthesisArtifact](t, synthesisPath)
	requireLiveReviewSynthesis(t, synthesis, laneOne, laneTwo, artifactDir, documentPath)
	verification := readLiveReviewJSON[liveReviewVerificationArtifact](t, verificationPath)
	requireLiveReviewVerification(t, verification, artifactDir, documentPath)
	requireLiveReviewMeaningfulRevision(t, pristineDocument, documentPath, artifactDir)
	requireLiveReviewBeadOutputs(t, env, city.Dir, streamID, map[string]string{
		"reviewLaneOne:0": laneOnePath,
		"reviewLaneTwo:0": laneTwoPath,
		"synthesize:0":    synthesisPath,
		"verify:0":        verificationPath,
	})
	t.Logf("PROOF real Claude/Codex review passed through default managed bd+Dolt (stream %s)", streamID)
}

type liveReviewSession struct {
	ID          string `json:"id"`
	Template    string `json:"template"`
	Provider    string `json:"provider"`
	State       string `json:"state"`
	SessionName string `json:"session_name"`
	CreatedAt   string `json:"created_at"`
	Closed      bool   `json:"closed"`
	Running     bool   `json:"running"`
}

type liveReviewPeek struct {
	SchemaVersion string `json:"schema_version"`
	SessionID     string `json:"session_id"`
	Target        string `json:"target"`
	Lines         int    `json:"lines"`
	LineCount     int    `json:"line_count"`
	Output        string `json:"output"`
}

type liveReviewFinding struct {
	ID             string   `json:"id"`
	Severity       string   `json:"severity"`
	Title          string   `json:"title"`
	Evidence       []string `json:"evidence"`
	Impact         string   `json:"impact"`
	Recommendation string   `json:"recommendation"`
}

type liveReviewLaneArtifact struct {
	Schema         string              `json:"schema"`
	Lane           string              `json:"lane"`
	Provider       string              `json:"provider"`
	Verdict        string              `json:"verdict"`
	Summary        string              `json:"summary"`
	Objective      string              `json:"objective"`
	DocumentPath   string              `json:"document_path"`
	RepositoryPath string              `json:"repository_path"`
	ArtifactPath   string              `json:"artifact_path"`
	Findings       []liveReviewFinding `json:"findings"`
	FailureClass   json.RawMessage     `json:"failure_class"`
}

type liveReviewSynthesisArtifact struct {
	Schema               string   `json:"schema"`
	Role                 string   `json:"role"`
	Provider             string   `json:"provider"`
	Verdict              string   `json:"verdict"`
	Summary              string   `json:"summary"`
	Objective            string   `json:"objective"`
	SourceReviews        []string `json:"source_reviews"`
	IncorporatedFindings []string `json:"incorporated_findings"`
	DeferredFindings     []struct {
		ID     string `json:"id"`
		Reason string `json:"reason"`
	} `json:"deferred_findings"`
	ChangedSections []string `json:"changed_sections"`
	Artifacts       struct {
		Document  string `json:"document"`
		Original  string `json:"original"`
		Diff      string `json:"diff"`
		Report    string `json:"report"`
		Synthesis string `json:"synthesis"`
	} `json:"artifacts"`
	FailureClass json.RawMessage `json:"failure_class"`
}

type liveReviewVerificationArtifact struct {
	Schema   string `json:"schema"`
	Role     string `json:"role"`
	Provider string `json:"provider"`
	Verdict  string `json:"verdict"`
	Summary  string `json:"summary"`
	Checks   struct {
		LaneArtifactsValid      bool `json:"lane_artifacts_valid"`
		SynthesisValid          bool `json:"synthesis_valid"`
		PropagatedOutputMatches bool `json:"propagated_output_matches"`
		ReportSubstantive       bool `json:"report_substantive"`
		DocumentChanged         bool `json:"document_changed"`
		RevisionMeaningful      bool `json:"revision_meaningful"`
	} `json:"checks"`
	Evidence  []string `json:"evidence"`
	Artifacts struct {
		Verification string `json:"verification"`
		Document     string `json:"document"`
		Diff         string `json:"diff"`
	} `json:"artifacts"`
	FailureClass json.RawMessage `json:"failure_class"`
}

type liveReviewBead struct {
	ID        string         `json:"id"`
	Status    string         `json:"status"`
	IssueType string         `json:"issue_type"`
	Metadata  map[string]any `json:"metadata"`
}

type liveReviewTreeManifest struct {
	Digest  string
	Entries map[string]string
}

func writeLiveReviewDoltConfig(gcHome string) error {
	dir := filepath.Join(gcHome, ".dolt")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "config_global.json"), []byte(`{"user.name":"gc-live-review","user.email":"gc-live-review@example.invalid"}`), 0o644)
}

func requireLiveReviewAuth(t *testing.T, env *helpers.Env, binary string, args ...string) {
	t.Helper()
	path, err := exec.LookPath(binary)
	require.NoError(t, err)
	out, err := runExternalWithTimeout(30*time.Second, env, "", path, args...)
	require.NoError(t, err, "%s authentication preflight failed\n%s", binary, out)
	t.Logf("%s authentication preflight: %s", binary, strings.TrimSpace(out))
}

func stageLiveReviewCodexAuth(gcHome string, env *helpers.Env) (string, error) {
	if strings.TrimSpace(os.Getenv("GC_WORKER_INFERENCE_CODEX_AUTH_JSON")) != "" ||
		strings.TrimSpace(os.Getenv("GC_WORKER_INFERENCE_CODEX_AUTH_FILE")) != "" {
		return stageCodexAuth(gcHome, env)
	}

	baseURL := strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	apiKey := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if baseURL == "" && apiKey == "" {
		return stageCodexAuth(gcHome, env)
	}
	if baseURL == "" || apiKey == "" {
		return "", fmt.Errorf("codex gateway auth requires both OPENAI_BASE_URL and OPENAI_API_KEY")
	}
	normalizedURL, err := liveReviewCodexGatewayURL(baseURL)
	if err != nil {
		return "", err
	}

	codexDir := filepath.Join(gcHome, ".codex")
	if err := os.MkdirAll(codexDir, 0o755); err != nil {
		return "", fmt.Errorf("creating isolated Codex home: %w", err)
	}
	catPath, err := exec.LookPath("cat")
	if err != nil {
		return "", fmt.Errorf("resolving command-backed Codex gateway auth helper: %w", err)
	}
	catPath, err = filepath.Abs(catPath)
	if err != nil {
		return "", fmt.Errorf("canonicalizing command-backed Codex gateway auth helper: %w", err)
	}
	tokenPath := filepath.Join(codexDir, "gateway-token")
	if err := os.WriteFile(tokenPath, []byte(apiKey), 0o600); err != nil {
		return "", fmt.Errorf("writing isolated Codex gateway token: %w", err)
	}
	const provider = "gc_lumen_gateway"
	config := fmt.Sprintf(`model_provider = %q

[model_providers.%s]
name = "OpenAI-compatible Lumen demo gateway"
base_url = %q
wire_api = "responses"

[model_providers.%s.auth]
command = %q
args = [%q]
timeout_ms = 5000
refresh_interval_ms = 0
`, provider, provider, normalizedURL, provider, catPath, tokenPath)
	if err := os.WriteFile(filepath.Join(codexDir, "config.toml"), []byte(config), 0o600); err != nil {
		return "", fmt.Errorf("writing isolated Codex gateway config: %w", err)
	}
	env.With("CODEX_HOME", codexDir).
		Without("OPENAI_API_KEY").
		Without("OPENAI_BASE_URL")
	return "env:OPENAI_GATEWAY", nil
}

func liveReviewCodexGatewayURL(raw string) (string, error) {
	parsed, err := url.ParseRequestURI(strings.TrimSpace(raw))
	if err != nil {
		return "", fmt.Errorf("parsing OPENAI_BASE_URL: %w", err)
	}
	if parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("OPENAI_BASE_URL must be an HTTPS origin or path without credentials, query, or fragment")
	}
	parsed.Path = strings.TrimSuffix(parsed.Path, "/")
	if !strings.HasSuffix(parsed.Path, "/v1") {
		parsed.Path += "/v1"
	}
	return parsed.String(), nil
}

func requireLiveReviewCodexInference(t *testing.T, env *helpers.Env, workDir string) {
	t.Helper()
	path, err := exec.LookPath("codex")
	require.NoError(t, err)
	const marker = "GC_CODEX_INFERENCE_AUTH_OK"
	lastMessage := filepath.Join(workDir, "codex-auth-preflight.txt")
	out, err := runExternalWithTimeout(2*time.Minute, env, workDir, path,
		"exec",
		"--dangerously-bypass-approvals-and-sandbox",
		"--skip-git-repo-check",
		"--color", "never",
		"--model", "gpt-5.5",
		"--output-last-message", lastMessage,
		"Reply with exactly "+marker+" and nothing else.",
	)
	require.NoError(t, err, "Codex real-inference authentication preflight failed\n%s", out)
	message, err := os.ReadFile(lastMessage)
	require.NoError(t, err, "read Codex real-inference authentication preflight output")
	require.Equal(t, marker, strings.TrimSpace(string(message)), "Codex preflight did not return the exact inference marker\n%s", out)
	require.NoError(t, os.Remove(lastMessage))
	t.Log("Codex real-inference authentication preflight passed")
}

func copyLiveReviewFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return fmt.Errorf("creating parent for %s: %w", dst, err)
	}
	if err := os.WriteFile(dst, data, 0o644); err != nil {
		return fmt.Errorf("writing %s: %w", dst, err)
	}
	return nil
}

func liveReviewContentManifest(root string) (liveReviewTreeManifest, error) {
	entries := make(map[string]string)
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return fmt.Errorf("relativizing repository snapshot path %s: %w", path, err)
		}
		rel = filepath.ToSlash(rel)
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("reading repository snapshot entry %s: %w", path, err)
		}

		switch {
		case entry.Type()&os.ModeSymlink != 0:
			target, err := os.Readlink(path)
			if err != nil {
				return fmt.Errorf("reading repository snapshot symlink %s: %w", path, err)
			}
			entries[rel] = fmt.Sprintf("symlink mode=%04o target=%q", info.Mode().Perm(), target)
		case entry.IsDir():
			entries[rel] = fmt.Sprintf("directory mode=%04o", info.Mode().Perm())
		case info.Mode().IsRegular():
			file, err := os.Open(path)
			if err != nil {
				return fmt.Errorf("opening repository snapshot file %s: %w", path, err)
			}
			hash := sha256.New()
			_, copyErr := io.Copy(hash, file)
			closeErr := file.Close()
			if copyErr != nil {
				return fmt.Errorf("hashing repository snapshot file %s: %w", path, copyErr)
			}
			if closeErr != nil {
				return fmt.Errorf("closing repository snapshot file %s: %w", path, closeErr)
			}
			entries[rel] = fmt.Sprintf("file mode=%04o size=%d sha256=%x", info.Mode().Perm(), info.Size(), hash.Sum(nil))
		default:
			return fmt.Errorf("repository snapshot contains unsupported entry %s with mode %s", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		return liveReviewTreeManifest{}, fmt.Errorf("manifesting repository snapshot %s: %w", root, err)
	}

	paths := sortedLiveReviewKeys(entries)
	aggregate := sha256.New()
	for _, path := range paths {
		value := entries[path]
		_, _ = fmt.Fprintf(aggregate, "%d:%s%d:%s", len(path), path, len(value), value)
	}
	return liveReviewTreeManifest{
		Digest:  fmt.Sprintf("%x", aggregate.Sum(nil)),
		Entries: entries,
	}, nil
}

func compareLiveReviewContentManifests(before, after liveReviewTreeManifest) error {
	if before.Digest == after.Digest && len(before.Entries) == len(after.Entries) {
		return nil
	}
	allPaths := make(map[string]bool, len(before.Entries)+len(after.Entries))
	for path := range before.Entries {
		allPaths[path] = true
	}
	for path := range after.Entries {
		allPaths[path] = true
	}
	changes := make([]string, 0)
	for _, path := range sortedLiveReviewKeys(allPaths) {
		beforeValue, beforeExists := before.Entries[path]
		afterValue, afterExists := after.Entries[path]
		switch {
		case !beforeExists:
			changes = append(changes, "added "+path)
		case !afterExists:
			changes = append(changes, "removed "+path)
		case beforeValue != afterValue:
			changes = append(changes, "changed "+path)
		}
	}
	return fmt.Errorf("repository snapshot content changed (%s -> %s): %s", before.Digest, after.Digest, strings.Join(changes, ", "))
}

func scrubLiveReviewRuntimeOverrides(env *helpers.Env) {
	for _, key := range liveReviewRuntimeOverrideKeys {
		env.Without(key)
	}
}

func stageLiveReviewRepositorySnapshot(source, destination string) (string, error) {
	sourceCommit, err := liveReviewGit(source, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving source repository commit: %w", err)
	}
	if _, err := os.Stat(destination); err == nil || !os.IsNotExist(err) {
		return "", fmt.Errorf("repository snapshot destination %s already exists", destination)
	}
	if _, err := liveReviewGit("", "clone", "--quiet", "--local", "--no-hardlinks", "--single-branch", "--no-tags", source, destination); err != nil {
		return "", fmt.Errorf("cloning committed repository snapshot: %w", err)
	}
	snapshotCommit, err := liveReviewGit(destination, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("resolving repository snapshot commit: %w", err)
	}
	if snapshotCommit != sourceCommit {
		return "", fmt.Errorf("repository snapshot commit %s does not match source HEAD %s", snapshotCommit, sourceCommit)
	}
	status, err := liveReviewGit(destination, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", fmt.Errorf("checking repository snapshot cleanliness: %w", err)
	}
	if status != "" {
		return "", fmt.Errorf("repository snapshot is not clean: %s", status)
	}
	if _, err := liveReviewGit(destination, "config", "core.fileMode", "false"); err != nil {
		return "", fmt.Errorf("configuring repository snapshot file-mode handling: %w", err)
	}
	if err := makeLiveReviewTreeReadOnly(destination); err != nil {
		return "", fmt.Errorf("making repository snapshot read-only: %w", err)
	}
	if err := requireLiveReviewTreeReadOnly(destination); err != nil {
		return "", err
	}
	return snapshotCommit, nil
}

func liveReviewGit(dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	commandArgs := append([]string(nil), args...)
	if dir != "" {
		commandArgs = append([]string{"-C", dir}, commandArgs...)
	}
	cmd := exec.CommandContext(ctx, "git", commandArgs...)
	cmd.Env = liveReviewGitEnv()
	out, err := cmd.CombinedOutput()
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w\n%s", strings.Join(commandArgs, " "), err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

func liveReviewGitEnv() []string {
	env := make([]string, 0, len(os.Environ())+2)
	for _, entry := range os.Environ() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "GIT_") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "GIT_OPTIONAL_LOCKS=0", "LC_ALL=C")
}

func makeLiveReviewTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := os.FileMode(0o444)
		if entry.IsDir() || info.Mode().Perm()&0o111 != 0 {
			mode = 0o555
		}
		return os.Chmod(path, mode)
	})
}

func requireLiveReviewTreeReadOnly(root string) error {
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o222 != 0 {
			return fmt.Errorf("repository snapshot path %s is writable with mode %s", path, info.Mode().Perm())
		}
		return nil
	})
}

func makeLiveReviewTreeOwnerWritable(root string) error {
	if _, err := os.Lstat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		mode := info.Mode().Perm() | 0o600
		if entry.IsDir() {
			mode |= 0o100
		}
		return os.Chmod(path, mode)
	})
}

func requireLiveReviewRepositorySnapshotUnchanged(t *testing.T, repositoryPath, wantCommit string) {
	t.Helper()
	require.NoError(t, requireLiveReviewTreeReadOnly(repositoryPath))
	commit, err := liveReviewGit(repositoryPath, "rev-parse", "HEAD")
	require.NoError(t, err)
	require.Equal(t, wantCommit, commit, "repository evidence HEAD changed during inference")
	status, err := liveReviewGit(repositoryPath, "status", "--porcelain=v1", "--untracked-files=all")
	require.NoError(t, err)
	require.Empty(t, status, "repository evidence changed during inference")
}

func requireLiveReviewProviderIsolation(t *testing.T, env *helpers.Env, gcHome, cityDir string) {
	t.Helper()
	claudeDir := filepath.Join(gcHome, ".claude")
	codexDir := filepath.Join(gcHome, ".codex")
	require.Equal(t, claudeDir, env.Get("CLAUDE_CONFIG_DIR"), "Claude config must be isolated below GC_HOME")
	require.Equal(t, codexDir, env.Get("CODEX_HOME"), "Codex config must be isolated below GC_HOME")

	for _, statePath := range []string{filepath.Join(gcHome, ".claude.json"), filepath.Join(claudeDir, ".claude.json")} {
		data, err := os.ReadFile(statePath)
		require.NoError(t, err, "read isolated Claude project state %s", statePath)
		var state struct {
			HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
			Projects               map[string]struct {
				HasCompletedProjectOnboarding bool `json:"hasCompletedProjectOnboarding"`
				HasTrustDialogAccepted        bool `json:"hasTrustDialogAccepted"`
			} `json:"projects"`
		}
		require.NoError(t, json.Unmarshal(data, &state))
		require.True(t, state.HasCompletedOnboarding, "Claude onboarding is not staged in %s", statePath)
		project, ok := state.Projects[cityDir]
		require.True(t, ok, "Claude project trust for %s is missing in %s", cityDir, statePath)
		require.True(t, project.HasCompletedProjectOnboarding)
		require.True(t, project.HasTrustDialogAccepted)
	}

	codexConfig := filepath.Join(codexDir, "config.toml")
	data, err := os.ReadFile(codexConfig)
	require.NoError(t, err, "read isolated Codex project trust")
	header := fmt.Sprintf("[projects.%s]", strconv.Quote(cityDir))
	require.Contains(t, string(data), header, "Codex project trust is missing for %s", cityDir)
	require.Contains(t, string(data), `trust_level = "trusted"`, "Codex project is not trusted")
}

func requireLiveReviewManagedDolt(t *testing.T, cityDir string) {
	t.Helper()
	for _, rel := range []string{"city.toml", "pack.toml"} {
		data, err := os.ReadFile(filepath.Join(cityDir, rel))
		require.NoError(t, err)
		require.NotContains(t, strings.ToLower(string(data)), "doltlite", "%s must not select DoltLite", rel)
	}
	data, err := os.ReadFile(filepath.Join(cityDir, ".beads", "metadata.json"))
	require.NoError(t, err, "default managed bd initialization did not write .beads/metadata.json")
	var metadata map[string]any
	require.NoError(t, json.Unmarshal(data, &metadata))
	require.Equal(t, "dolt", strings.ToLower(strings.TrimSpace(fmt.Sprint(metadata["backend"]))))
	require.Equal(t, "server", strings.ToLower(strings.TrimSpace(fmt.Sprint(metadata["dolt_mode"]))))
	require.NotContains(t, strings.ToLower(string(data)), "doltlite")
}

func requireLiveReviewProviderRoutes(t *testing.T, cityDir string) {
	t.Helper()
	for template, provider := range liveReviewExpectedProviders {
		path := filepath.Join(cityDir, "agents", template, "agent.toml")
		data, err := os.ReadFile(path)
		require.NoError(t, err, "read provider route %s", path)
		require.Contains(t, string(data), fmt.Sprintf("provider = %q", provider), "route %s did not retain provider %s", template, provider)
	}
}

func waitForLiveReviewPhase(
	t *testing.T,
	env *helpers.Env,
	cityDir string,
	runDone <-chan struct{},
	runStdout, runStderr *bytes.Buffer,
	want map[string]string,
	requireActive bool,
	timeout time.Duration,
) map[string]liveReviewSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var lastOut string
	var lastErr error
	runFinished := false
	for time.Now().Before(deadline) {
		rows, out, err := listLiveReviewSessions(env, cityDir)
		lastOut, lastErr = out, err
		if err == nil {
			found := selectLiveReviewPhaseSessions(rows, want, requireActive)
			if len(found) == len(want) {
				if requireActive {
					tmuxSessions, tmuxErr := listTmuxSessionsOnCitySocketWithEnv(cityDir, env.List())
					if tmuxErr != nil {
						lastErr = tmuxErr
						time.Sleep(liveReviewSessionPoll)
						continue
					}
					if tmuxErr = validateLiveReviewTmuxSnapshot(found, tmuxSessions); tmuxErr != nil {
						lastErr = tmuxErr
						time.Sleep(liveReviewSessionPoll)
						continue
					}
				}
				requireLiveReviewSessionProviders(t, env, cityDir, found, want)
				if requireActive {
					t.Logf("PROOF one lifecycle snapshot and one tmux snapshot contain concurrently active phase %v", sortedLiveReviewKeys(want))
				} else {
					t.Logf("PROOF session lifecycle history contains phase %v", sortedLiveReviewKeys(want))
				}
				return found
			}
		}
		if !runFinished {
			select {
			case <-runDone:
				runFinished = true
				if requireActive {
					t.Fatalf("gc run exited before active phase %v was observed\nstdout:\n%s\nstderr:\n%s", sortedLiveReviewKeys(want), runStdout.String(), runStderr.String())
				}
				if historyDeadline := time.Now().Add(liveReviewHistoryGrace); historyDeadline.Before(deadline) {
					deadline = historyDeadline
				}
			default:
			}
		}
		time.Sleep(liveReviewSessionPoll)
	}
	if runFinished {
		t.Fatalf("phase %v did not appear in lifecycle history within %s after gc run exited (last error: %v)\nlast session list:\n%s\nstdout:\n%s\nstderr:\n%s", sortedLiveReviewKeys(want), liveReviewHistoryGrace, lastErr, lastOut, runStdout.String(), runStderr.String())
	}
	t.Fatalf("phase %v was not observable within %s (active required: %t; last error: %v)\nlast session list:\n%s", sortedLiveReviewKeys(want), timeout, requireActive, lastErr, lastOut)
	return nil
}

func selectLiveReviewPhaseSessions(rows []liveReviewSession, want map[string]string, requireActive bool) map[string]liveReviewSession {
	found := make(map[string]liveReviewSession, len(want))
	for _, row := range rows {
		if _, ok := want[row.Template]; !ok || strings.TrimSpace(row.ID) == "" || strings.TrimSpace(row.SessionName) == "" {
			continue
		}
		createdAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(row.CreatedAt))
		if err != nil {
			continue
		}
		active := liveReviewInferenceActive(row)
		if requireActive && (row.Closed || !active) {
			continue
		}
		if !requireActive && !active && !liveReviewSessionReturned(row) {
			continue
		}
		prior, exists := found[row.Template]
		if exists {
			priorActive := liveReviewInferenceActive(prior)
			if priorActive && !active {
				continue
			}
			if active == priorActive {
				priorCreatedAt, priorErr := time.Parse(time.RFC3339Nano, strings.TrimSpace(prior.CreatedAt))
				if priorErr == nil && (createdAt.Before(priorCreatedAt) || (createdAt.Equal(priorCreatedAt) && row.ID < prior.ID)) {
					continue
				}
			}
		}
		found[row.Template] = row
	}
	return found
}

func liveReviewInferenceActive(row liveReviewSession) bool {
	if row.Closed {
		return false
	}
	switch session.State(strings.TrimSpace(row.State)) {
	case session.StateActive, session.StateAwake:
		return true
	default:
		return false
	}
}

func validateLiveReviewTmuxSnapshot(rows map[string]liveReviewSession, sessionNames []string) error {
	present := make(map[string]bool, len(sessionNames))
	for _, name := range sessionNames {
		present[name] = true
	}
	for template, row := range rows {
		if strings.TrimSpace(row.SessionName) == "" || !present[row.SessionName] {
			return fmt.Errorf("active tmux snapshot is missing %s session %s", template, row.SessionName)
		}
	}
	return nil
}

func validateLiveReviewTmuxAbsence(rows map[string]liveReviewSession, sessionNames []string) error {
	present := make(map[string]bool, len(sessionNames))
	for _, name := range sessionNames {
		present[name] = true
	}
	for template, row := range rows {
		if strings.TrimSpace(row.SessionName) != "" && present[row.SessionName] {
			return fmt.Errorf("tmux snapshot still contains returned %s session %s", template, row.SessionName)
		}
	}
	return nil
}

func requireLiveReviewSessionProviders(t *testing.T, env *helpers.Env, cityDir string, rows map[string]liveReviewSession, want map[string]string) {
	t.Helper()
	for template, expectedProvider := range want {
		row := rows[template]
		out, err := runGCWithTimeout(30*time.Second, env, cityDir, "bd", "show", row.ID, "--json")
		require.NoError(t, err, "gc bd show session %s (%s)\n%s", row.ID, template, out)
		bead := decodeLiveReviewBead(t, out)
		require.NoError(t, validateLiveReviewSessionProvenance(row, expectedProvider, bead.Metadata), "session bead %s provenance does not match the observed runtime", row.ID)
	}
}

func validateLiveReviewSessionProvenance(row liveReviewSession, expectedProvider string, metadata map[string]any) error {
	expected := map[string]string{
		"template":     row.Template,
		"provider":     expectedProvider,
		"session_name": row.SessionName,
	}
	for key, want := range expected {
		got, err := liveReviewExactMetadataString(metadata, key)
		if err != nil {
			return err
		}
		if got != want {
			return fmt.Errorf("metadata %s = %q, want %q", key, got, want)
		}
	}
	if rawTransport, ok := metadata["transport"]; ok {
		transport, ok := rawTransport.(string)
		if !ok {
			return fmt.Errorf("metadata transport has type %T, want string", rawTransport)
		}
		if transport = strings.TrimSpace(transport); transport != "" && transport != "tmux" {
			return fmt.Errorf("metadata transport = %q, want tmux or omitted", transport)
		}
	}
	return nil
}

func listLiveReviewSessions(env *helpers.Env, cityDir string) ([]liveReviewSession, string, error) {
	out, err := runGCWithTimeout(20*time.Second, env, cityDir, "session", "list", "--state", "all", "--json")
	if err != nil {
		return nil, out, err
	}
	envelope, err := decodeLiveReviewStrictJSON[struct {
		Sessions []liveReviewSession `json:"sessions"`
	}]([]byte(out))
	if err != nil {
		return nil, out, fmt.Errorf("session list is not exactly one strict JSON envelope: %w", err)
	}
	if envelope.Sessions == nil {
		return nil, out, fmt.Errorf("session list JSON envelope has no sessions array")
	}
	return envelope.Sessions, out, nil
}

func listLiveReviewSessionBeads(env *helpers.Env, cityDir string) ([]liveReviewBead, string, error) {
	out, err := runGCWithTimeout(30*time.Second, env, cityDir, "bd", "list", "--all", "--include-infra", "--json", "--limit=0", "--type=session")
	if err != nil {
		return nil, out, err
	}
	beads, err := decodeLiveReviewStrictJSON[[]liveReviewBead]([]byte(out))
	if err != nil {
		return nil, out, fmt.Errorf("session bead list is not exactly one strict JSON array: %w", err)
	}
	if beads == nil {
		return nil, out, fmt.Errorf("session bead list JSON value is null, want array")
	}
	return beads, out, nil
}

func requireLiveReviewPeek(t *testing.T, env *helpers.Env, cityDir string, row liveReviewSession) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)
	var out string
	var err error
	for time.Now().Before(deadline) {
		out, err = runGCWithTimeout(30*time.Second, env, cityDir, "session", "peek", row.ID, "--lines", "80", "--json")
		if err == nil {
			var transcript string
			transcript, err = liveReviewPeekTranscript(out, row.ID)
			if err == nil {
				if len(transcript) > 4000 {
					transcript = transcript[len(transcript)-4000:]
				}
				t.Logf("session peek %s/%s:\n%s", row.Template, liveReviewExpectedProviders[row.Template], strings.TrimSpace(transcript))
				return
			}
		}
		time.Sleep(liveReviewSessionPoll)
	}
	t.Fatalf("gc session peek %s (%s) returned no evidence within 2m: %v\n%s", row.ID, row.Template, err, out)
}

func liveReviewPeekTranscript(out, expectedSessionID string) (string, error) {
	peek, err := decodeLiveReviewStrictJSON[liveReviewPeek]([]byte(out))
	if err != nil {
		return "", fmt.Errorf("session peek JSON for %s is not exactly one strict object: %w", expectedSessionID, err)
	}
	if peek.SchemaVersion == "1" && peek.SessionID == expectedSessionID && peek.Target == expectedSessionID &&
		peek.Lines == 80 && peek.LineCount > 0 && strings.TrimSpace(peek.Output) != "" {
		return peek.Output, nil
	}
	return "", fmt.Errorf("session peek JSON for %s has no non-empty provider transcript", expectedSessionID)
}

func requireLiveReviewDistinctSessions(t *testing.T, observed map[string]liveReviewSession) {
	t.Helper()
	require.Len(t, observed, len(liveReviewExpectedProviders))
	ids := make(map[string]string)
	names := make(map[string]string)
	for template := range liveReviewExpectedProviders {
		row, ok := observed[template]
		require.True(t, ok, "session %s was not observed", template)
		require.NotEmpty(t, row.ID)
		if prior := ids[row.ID]; prior != "" {
			t.Fatalf("templates %s and %s reused session bead %s", prior, template, row.ID)
		}
		if row.SessionName != "" {
			if prior := names[row.SessionName]; prior != "" {
				t.Fatalf("templates %s and %s reused runtime session %s", prior, template, row.SessionName)
			}
			names[row.SessionName] = template
		}
		ids[row.ID] = template
	}
}

func parseLiveReviewStreamID(t *testing.T, out string) string {
	t.Helper()
	const marker = "(stream "
	start := strings.Index(out, marker)
	if start < 0 {
		t.Fatalf("gc run output has no stream id: %q", out)
	}
	start += len(marker)
	end := strings.IndexByte(out[start:], ')')
	if end < 0 {
		t.Fatalf("gc run stream header is malformed: %q", out)
	}
	streamID := strings.TrimSpace(out[start : start+end])
	if streamID == "" {
		t.Fatalf("gc run stream id is empty: %q", out)
	}
	return streamID
}

func readLiveReviewJSON[T any](t *testing.T, path string) T {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err, "read live review artifact %s", path)
	value, err := decodeLiveReviewStrictJSON[T](data)
	require.NoError(t, err, "decode strict live review artifact %s", path)
	return value
}

func decodeLiveReviewStrictJSON[T any](raw []byte) (T, error) {
	var value T
	payload, err := canon.Canonicalize(bytes.TrimSpace(raw))
	if err != nil {
		return value, err
	}
	if err := json.Unmarshal(payload, &value); err != nil {
		return value, err
	}
	return value, nil
}

func requireLiveReviewLane(t *testing.T, lane liveReviewLaneArtifact, wantLane, wantProvider, documentPath, repositoryPath, artifactPath string) {
	t.Helper()
	require.Equal(t, "review-quorum.lane.v1", lane.Schema)
	require.Equal(t, wantLane, lane.Lane)
	require.Equal(t, wantProvider, lane.Provider)
	require.Contains(t, []string{"approve", "revise", "block"}, lane.Verdict)
	require.GreaterOrEqual(t, len(strings.TrimSpace(lane.Summary)), liveReviewMinSummaryLength)
	require.Equal(t, "Make the gc reload design implementation-ready", lane.Objective)
	require.Equal(t, documentPath, lane.DocumentPath)
	require.Equal(t, repositoryPath, lane.RepositoryPath)
	require.Equal(t, artifactPath, lane.ArtifactPath)
	require.GreaterOrEqual(t, len(lane.Findings), 3)
	require.LessOrEqual(t, len(lane.Findings), 7)
	requireLiveReviewNull(t, lane.FailureClass, "lane failure_class")

	seen := make(map[string]bool)
	var documentSpecific strings.Builder
	for _, finding := range lane.Findings {
		require.NotEmpty(t, strings.TrimSpace(finding.ID))
		require.False(t, seen[finding.ID], "duplicate finding id %q", finding.ID)
		seen[finding.ID] = true
		require.Contains(t, []string{"critical", "high", "medium", "low"}, finding.Severity)
		require.GreaterOrEqual(t, len(strings.TrimSpace(finding.Title)), 10)
		require.NotEmpty(t, finding.Evidence)
		require.GreaterOrEqual(t, len(strings.TrimSpace(strings.Join(finding.Evidence, " "))), 20)
		require.GreaterOrEqual(t, len(strings.TrimSpace(finding.Impact)), 20)
		require.GreaterOrEqual(t, len(strings.TrimSpace(finding.Recommendation)), 20)
		fmt.Fprintf(&documentSpecific, " %s %s %s", finding.Title, strings.Join(finding.Evidence, " "), finding.Recommendation)
	}
	lower := strings.ToLower(lane.Summary + documentSpecific.String())
	specificTerms := 0
	for _, term := range []string{"reload", "controller", "session", "config", "reconcile"} {
		if strings.Contains(lower, term) {
			specificTerms++
		}
	}
	require.GreaterOrEqual(t, specificTerms, 2, "lane review is not specific to the gc reload design")
}

func requireLiveReviewSynthesis(t *testing.T, got liveReviewSynthesisArtifact, laneOne, laneTwo liveReviewLaneArtifact, artifactDir, documentPath string) {
	t.Helper()
	require.Equal(t, "review-quorum.synthesis.v1", got.Schema)
	require.Equal(t, "synthesis", got.Role)
	require.Equal(t, "claude", got.Provider)
	require.Equal(t, "revised", got.Verdict)
	require.GreaterOrEqual(t, len(strings.TrimSpace(got.Summary)), liveReviewMinSummaryLength)
	require.Equal(t, "Make the gc reload design implementation-ready", got.Objective)
	require.ElementsMatch(t, []string{filepath.Join(artifactDir, "lane-one.json"), filepath.Join(artifactDir, "lane-two.json")}, got.SourceReviews)
	require.NotEmpty(t, got.IncorporatedFindings, "synthesis incorporated no reviewer finding")
	require.NotEmpty(t, got.ChangedSections)
	for _, deferred := range got.DeferredFindings {
		require.NotEmpty(t, strings.TrimSpace(deferred.ID))
		require.GreaterOrEqual(t, len(strings.TrimSpace(deferred.Reason)), 20)
	}
	require.NoError(t, validateLiveReviewFindingCoverage(laneOne, laneTwo, got))
	require.Equal(t, documentPath, got.Artifacts.Document)
	require.Equal(t, filepath.Join(artifactDir, "original.md"), got.Artifacts.Original)
	require.Equal(t, filepath.Join(artifactDir, "revision.diff"), got.Artifacts.Diff)
	require.Equal(t, filepath.Join(artifactDir, "synthesis-report.md"), got.Artifacts.Report)
	require.Equal(t, filepath.Join(artifactDir, "synthesis.json"), got.Artifacts.Synthesis)
	requireLiveReviewNull(t, got.FailureClass, "synthesis failure_class")
	report, err := os.ReadFile(got.Artifacts.Report)
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(strings.TrimSpace(string(report))), 500, "synthesis report is not substantive")
}

func requireLiveReviewVerification(t *testing.T, got liveReviewVerificationArtifact, artifactDir, documentPath string) {
	t.Helper()
	require.Equal(t, "review-quorum.verification.v1", got.Schema)
	require.Equal(t, "verification", got.Role)
	require.Equal(t, "codex", got.Provider)
	require.Equal(t, "pass", got.Verdict)
	require.GreaterOrEqual(t, len(strings.TrimSpace(got.Summary)), liveReviewMinSummaryLength)
	require.True(t, got.Checks.LaneArtifactsValid)
	require.True(t, got.Checks.SynthesisValid)
	require.True(t, got.Checks.PropagatedOutputMatches)
	require.True(t, got.Checks.ReportSubstantive)
	require.True(t, got.Checks.DocumentChanged)
	require.True(t, got.Checks.RevisionMeaningful)
	require.GreaterOrEqual(t, len(got.Evidence), 3)
	require.Equal(t, filepath.Join(artifactDir, "verification.json"), got.Artifacts.Verification)
	require.Equal(t, documentPath, got.Artifacts.Document)
	require.Equal(t, filepath.Join(artifactDir, "revision.diff"), got.Artifacts.Diff)
	requireLiveReviewNull(t, got.FailureClass, "verification failure_class")
}

func requireLiveReviewMeaningfulRevision(t *testing.T, pristine []byte, documentPath, artifactDir string) {
	t.Helper()
	original, err := os.ReadFile(filepath.Join(artifactDir, "original.md"))
	require.NoError(t, err)
	require.Equal(t, pristine, original, "synthesis original.md is not the pristine checked-in design")
	revised, err := os.ReadFile(documentPath)
	require.NoError(t, err)
	require.NotEqual(t, sha256.Sum256(pristine), sha256.Sum256(revised), "revised design hash did not change")
	require.NotEqual(t, strings.Join(strings.Fields(string(pristine)), " "), strings.Join(strings.Fields(string(revised)), " "), "revision changed only whitespace")
	require.Greater(t, len(revised), len(pristine)/2, "revision discarded most of the real design")

	diff, err := os.ReadFile(filepath.Join(artifactDir, "revision.diff"))
	require.NoError(t, err)
	require.NoError(t, validateLiveReviewRevisionDiff(filepath.Join(artifactDir, "original.md"), documentPath, diff))
	added, removed := 0, 0
	for _, line := range strings.Split(string(diff), "\n") {
		switch {
		case strings.HasPrefix(line, "+") && !strings.HasPrefix(line, "+++") && strings.TrimSpace(strings.TrimPrefix(line, "+")) != "":
			added++
		case strings.HasPrefix(line, "-") && !strings.HasPrefix(line, "---") && strings.TrimSpace(strings.TrimPrefix(line, "-")) != "":
			removed++
		}
	}
	require.Greater(t, added, 0, "revision diff has no substantive additions")
	require.GreaterOrEqual(t, added+removed, liveReviewMinChangedLines, "revision diff is too small to be meaningful")
	t.Logf("document changed: original_sha256=%x revised_sha256=%x changed_lines=%d", sha256.Sum256(pristine), sha256.Sum256(revised), added+removed)
}

func requireLiveReviewBeadOutputs(t *testing.T, env *helpers.Env, cityDir, streamID string, artifactByActivation map[string]string) {
	t.Helper()
	store, err := graphstore.Open(context.Background(), filepath.Join(cityDir, ".gc", "graph", "journal.db"), graphstore.Options{})
	require.NoError(t, err)
	defer func() { _ = store.Close() }()
	events, err := store.ReadStream(context.Background(), streamID, 1, 0)
	require.NoError(t, err)

	beadByActivation, err := validateLiveReviewJournal(events)
	require.NoError(t, err)
	require.Len(t, beadByActivation, len(artifactByActivation))

	for activation, artifactPath := range artifactByActivation {
		beadID := beadByActivation[activation]
		out, err := runGCWithTimeout(30*time.Second, env, cityDir, "bd", "show", beadID, "--json")
		require.NoError(t, err, "gc bd show %s (%s)\n%s", beadID, activation, out)
		bead := decodeLiveReviewBead(t, out)
		require.Equal(t, "closed", bead.Status)
		require.Equal(t, "pass", metaString(bead.Metadata, "gc.outcome"))
		artifact, err := os.ReadFile(artifactPath)
		require.NoError(t, err)
		var compact bytes.Buffer
		require.NoError(t, json.Compact(&compact, artifact))
		stamped, err := liveReviewExactMetadataString(bead.Metadata, "gc.output_json")
		require.NoError(t, err, "%s did not stamp string gc.output_json metadata", activation)
		require.Equal(t, compact.String(), stamped, "%s did not stamp the exact compact artifact bytes as gc.output_json", activation)
	}
}

func validateLiveReviewJournal(events []graphstore.StoredEvent) (map[string]string, error) {
	expected := map[string]bool{
		"reviewLaneOne:0": true,
		"reviewLaneTwo:0": true,
		"synthesize:0":    true,
		"verify:0":        true,
	}
	type fact struct {
		seq    uint64
		beadID string
	}
	admitted := make(map[string]fact, len(expected))
	settled := make(map[string]fact, len(expected))
	runClosedSeq := uint64(0)

	for _, event := range events {
		switch event.Type {
		case engine.EventOwnedAdmitted:
			var payload struct {
				Activation string `json:"activation"`
				Kind       string `json:"kind"`
				BeadID     string `json:"bead_id"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decoding %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if !expected[payload.Activation] {
				continue
			}
			if _, exists := admitted[payload.Activation]; exists {
				return nil, fmt.Errorf("duplicate admission for %s", payload.Activation)
			}
			if payload.Kind != engine.OwnedKindWorkBead || strings.TrimSpace(payload.BeadID) == "" {
				return nil, fmt.Errorf("%s admission is not a concrete work bead", payload.Activation)
			}
			admitted[payload.Activation] = fact{seq: event.Seq, beadID: payload.BeadID}

		case engine.EventOutcomeSettled:
			var payload struct {
				Activation string `json:"activation"`
				Outcome    string `json:"outcome"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decoding %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if !expected[payload.Activation] {
				continue
			}
			if _, exists := settled[payload.Activation]; exists {
				return nil, fmt.Errorf("duplicate settlement for %s", payload.Activation)
			}
			if payload.Outcome != string(engine.OutcomePass) {
				return nil, fmt.Errorf("%s settled %q, want pass", payload.Activation, payload.Outcome)
			}
			settled[payload.Activation] = fact{seq: event.Seq}

		case engine.EventRunClosed:
			var payload struct {
				Outcome string `json:"outcome"`
			}
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				return nil, fmt.Errorf("decoding %s at seq %d: %w", event.Type, event.Seq, err)
			}
			if runClosedSeq != 0 {
				return nil, fmt.Errorf("journal contains more than one terminal run closure")
			}
			if payload.Outcome != string(engine.OutcomePass) {
				return nil, fmt.Errorf("run closed %q, want pass", payload.Outcome)
			}
			runClosedSeq = event.Seq
		}
	}

	for activation := range expected {
		if admitted[activation].seq == 0 || settled[activation].seq == 0 {
			return nil, fmt.Errorf("journal is missing admission or pass settlement for %s", activation)
		}
		if admitted[activation].seq >= settled[activation].seq {
			return nil, fmt.Errorf("%s settled before its admission (admitted=%d settled=%d)", activation, admitted[activation].seq, settled[activation].seq)
		}
	}
	reviewerOneSettled := settled["reviewLaneOne:0"].seq
	reviewerTwoSettled := settled["reviewLaneTwo:0"].seq
	synthesisAdmitted := admitted["synthesize:0"].seq
	if synthesisAdmitted <= reviewerOneSettled || synthesisAdmitted <= reviewerTwoSettled {
		return nil, fmt.Errorf("synthesis admitted at seq %d before both reviewers settled at %d and %d", synthesisAdmitted, reviewerOneSettled, reviewerTwoSettled)
	}
	synthesisSettled := settled["synthesize:0"].seq
	verificationAdmitted := admitted["verify:0"].seq
	if verificationAdmitted <= synthesisSettled {
		return nil, fmt.Errorf("verification admitted at seq %d before synthesis settled at %d", verificationAdmitted, synthesisSettled)
	}
	if runClosedSeq == 0 {
		return nil, fmt.Errorf("journal has no terminal run closure")
	}
	for activation, fact := range settled {
		if runClosedSeq <= fact.seq {
			return nil, fmt.Errorf("run closed at seq %d before %s settled at %d", runClosedSeq, activation, fact.seq)
		}
	}
	last := events[len(events)-1]
	if last.Type != engine.EventRunClosed || last.Seq != runClosedSeq {
		return nil, fmt.Errorf("run closure at seq %d is not the final stream event", runClosedSeq)
	}

	beads := make(map[string]string, len(admitted))
	for activation, fact := range admitted {
		beads[activation] = fact.beadID
	}
	return beads, nil
}

func validateLiveReviewRevisionDiff(originalPath, revisedPath string, recorded []byte) error {
	original, err := os.ReadFile(originalPath)
	if err != nil {
		return fmt.Errorf("reading original document: %w", err)
	}
	revised, err := os.ReadFile(revisedPath)
	if err != nil {
		return fmt.Errorf("reading revised document: %w", err)
	}
	recordedHunks, err := liveReviewDiffHunks(recorded)
	if err != nil {
		return fmt.Errorf("reading recorded revision diff: %w", err)
	}

	applyDir, err := os.MkdirTemp("", "gc-live-review-diff-*")
	if err != nil {
		return fmt.Errorf("creating isolated diff application directory: %w", err)
	}
	defer os.RemoveAll(applyDir)
	targetPath := filepath.Join(applyDir, "document.md")
	if err := os.WriteFile(targetPath, original, 0o600); err != nil {
		return fmt.Errorf("staging original document for diff application: %w", err)
	}
	patchFile, err := os.CreateTemp("", "gc-live-review-*.diff")
	if err != nil {
		return fmt.Errorf("creating retargeted revision diff: %w", err)
	}
	patchPath := patchFile.Name()
	defer os.Remove(patchPath)
	retargeted := "--- a/document.md\n+++ b/document.md\n" + recordedHunks + "\n"
	if _, err := patchFile.WriteString(retargeted); err != nil {
		_ = patchFile.Close()
		return fmt.Errorf("writing retargeted revision diff: %w", err)
	}
	if err := patchFile.Close(); err != nil {
		return fmt.Errorf("closing retargeted revision diff: %w", err)
	}
	if _, err := liveReviewGit(applyDir, "apply", "--check", "--recount", "--whitespace=nowarn", "-p1", patchPath); err != nil {
		return fmt.Errorf("revision.diff does not apply to original.md: %w", err)
	}
	if _, err := liveReviewGit(applyDir, "apply", "--recount", "--whitespace=nowarn", "-p1", patchPath); err != nil {
		return fmt.Errorf("applying revision.diff to original.md: %w", err)
	}
	patched, err := os.ReadFile(targetPath)
	if err != nil {
		return fmt.Errorf("reading patched document: %w", err)
	}
	entries, err := os.ReadDir(applyDir)
	if err != nil {
		return fmt.Errorf("checking isolated diff application directory: %w", err)
	}
	if len(entries) != 1 || entries[0].Name() != "document.md" {
		return fmt.Errorf("revision.diff targets files other than the explicit document")
	}
	if !bytes.Equal(patched, revised) {
		return fmt.Errorf("revision.diff does not agree with original.md and the revised document")
	}
	return nil
}

func liveReviewDiffHunks(diff []byte) (string, error) {
	lines := strings.Split(strings.ReplaceAll(string(diff), "\r\n", "\n"), "\n")
	for i, line := range lines {
		if strings.HasPrefix(line, "@@ ") {
			return strings.TrimRight(strings.Join(lines[i:], "\n"), "\n"), nil
		}
	}
	return "", fmt.Errorf("unified diff contains no hunk")
}

func liveReviewExactMetadataString(metadata map[string]any, key string) (string, error) {
	value, ok := metadata[key]
	if !ok {
		return "", fmt.Errorf("metadata %q is missing", key)
	}
	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("metadata %q must be a string, got %T", key, value)
	}
	return text, nil
}

func validateLiveReviewFindingCoverage(laneOne, laneTwo liveReviewLaneArtifact, synthesis liveReviewSynthesisArtifact) error {
	reviewerIDs := make(map[string]bool, len(laneOne.Findings)+len(laneTwo.Findings))
	for _, lane := range []liveReviewLaneArtifact{laneOne, laneTwo} {
		for _, finding := range lane.Findings {
			if strings.TrimSpace(finding.ID) == "" {
				return fmt.Errorf("reviewer finding id is empty")
			}
			if finding.ID != strings.TrimSpace(finding.ID) {
				return fmt.Errorf("reviewer finding id %q has surrounding whitespace", finding.ID)
			}
			if reviewerIDs[finding.ID] {
				return fmt.Errorf("duplicate reviewer finding id %q", finding.ID)
			}
			reviewerIDs[finding.ID] = true
		}
	}

	covered := make(map[string]bool, len(synthesis.IncorporatedFindings)+len(synthesis.DeferredFindings))
	cover := func(id string) error {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("synthesis finding id is empty")
		}
		if id != strings.TrimSpace(id) {
			return fmt.Errorf("synthesis finding id %q has surrounding whitespace", id)
		}
		if !reviewerIDs[id] {
			return fmt.Errorf("synthesis references unknown reviewer finding %q", id)
		}
		if covered[id] {
			return fmt.Errorf("synthesis classifies reviewer finding %q more than once", id)
		}
		covered[id] = true
		return nil
	}
	for _, id := range synthesis.IncorporatedFindings {
		if err := cover(id); err != nil {
			return err
		}
	}
	for _, deferred := range synthesis.DeferredFindings {
		if err := cover(deferred.ID); err != nil {
			return err
		}
	}
	for id := range reviewerIDs {
		if !covered[id] {
			return fmt.Errorf("reviewer finding %q is neither incorporated nor deferred", id)
		}
	}
	return nil
}

func decodeLiveReviewBead(t *testing.T, out string) liveReviewBead {
	t.Helper()
	beads, err := decodeLiveReviewStrictJSON[[]liveReviewBead]([]byte(out))
	require.NoError(t, err, "gc bd show did not return exactly one strict JSON array")
	require.Len(t, beads, 1, "gc bd show did not return one JSON bead")
	return beads[0]
}

func waitForLiveReviewSessionsReturned(t *testing.T, env *helpers.Env, cityDir string, observed map[string]liveReviewSession, timeout time.Duration) map[string]liveReviewSession {
	t.Helper()
	deadline := time.Now().Add(timeout)
	stable := 0
	var finalRows map[string]liveReviewSession
	var lastOut string
	var lastErr error
	for time.Now().Before(deadline) {
		beads, out, err := listLiveReviewSessionBeads(env, cityDir)
		lastOut, lastErr = out, err
		if err != nil {
			stable = 0
			time.Sleep(liveReviewSessionPoll)
			continue
		}
		finalRows, err = validateLiveReviewFinalSessionBeads(beads, observed)
		if err != nil {
			lastErr = err
			stable = 0
			time.Sleep(liveReviewSessionPoll)
			continue
		}
		tmuxSessions, err := listTmuxSessionsOnCitySocketWithEnv(cityDir, env.List())
		if err != nil {
			lastErr = err
			stable = 0
			time.Sleep(liveReviewSessionPoll)
			continue
		}
		if err := validateLiveReviewTmuxAbsence(observed, tmuxSessions); err != nil {
			lastErr = err
			stable = 0
			time.Sleep(liveReviewSessionPoll)
			continue
		}
		stable++
		if stable >= liveReviewStablePolls {
			t.Logf("PROOF exactly the four captured inference sessions returned in lifecycle and tmux for %d consecutive snapshots", liveReviewStablePolls)
			return finalRows
		}
		time.Sleep(liveReviewSessionPoll)
	}
	t.Fatalf("live review sessions did not return with exact captured history within %s: last_error=%v\nlast session list:\n%s", timeout, lastErr, lastOut)
	return nil
}

func validateLiveReviewFinalSessionBeads(beads []liveReviewBead, observed map[string]liveReviewSession) (map[string]liveReviewSession, error) {
	if len(observed) != len(liveReviewExpectedProviders) {
		return nil, fmt.Errorf("captured session count = %d, want %d", len(observed), len(liveReviewExpectedProviders))
	}
	capturedTemplateByID := make(map[string]string, len(observed))
	for template := range liveReviewExpectedProviders {
		row, ok := observed[template]
		if !ok {
			return nil, fmt.Errorf("captured sessions are missing template %s", template)
		}
		if row.Template != template {
			return nil, fmt.Errorf("captured session %s changed template from map key %s to %s", row.ID, template, row.Template)
		}
		if strings.TrimSpace(row.ID) == "" {
			return nil, fmt.Errorf("captured session for template %s has an empty id", template)
		}
		if prior := capturedTemplateByID[row.ID]; prior != "" {
			return nil, fmt.Errorf("captured templates %s and %s share session id %s", prior, template, row.ID)
		}
		capturedTemplateByID[row.ID] = template
	}

	finalRows := make(map[string]liveReviewSession, len(observed))
	for _, bead := range beads {
		capturedTemplate, capturedID := capturedTemplateByID[bead.ID]
		if !capturedID {
			return nil, fmt.Errorf("final history contains unexpected session bead %s", bead.ID)
		}
		if bead.IssueType != "session" {
			return nil, fmt.Errorf("captured bead %s has issue type %q, want session", bead.ID, bead.IssueType)
		}
		template, err := liveReviewExactMetadataString(bead.Metadata, "template")
		if err != nil {
			return nil, fmt.Errorf("captured session %s: %w", bead.ID, err)
		}
		if template != capturedTemplate {
			return nil, fmt.Errorf("captured session %s changed template from %s to %s", bead.ID, capturedTemplate, template)
		}
		captured := observed[template]
		sessionName, err := liveReviewExactMetadataString(bead.Metadata, "session_name")
		if err != nil {
			return nil, fmt.Errorf("captured session %s: %w", bead.ID, err)
		}
		if sessionName != captured.SessionName {
			return nil, fmt.Errorf("captured session %s changed runtime name from %s to %s", bead.ID, captured.SessionName, sessionName)
		}
		state, err := liveReviewExactMetadataString(bead.Metadata, "state")
		if err != nil && bead.Status != "closed" {
			return nil, fmt.Errorf("captured session %s: %w", bead.ID, err)
		}
		row := liveReviewSession{
			ID:          bead.ID,
			Template:    template,
			State:       state,
			SessionName: sessionName,
			Closed:      bead.Status == "closed",
		}
		if _, duplicate := finalRows[template]; duplicate {
			return nil, fmt.Errorf("final history contains duplicate bead for captured template %s", template)
		}
		if !liveReviewSessionReturned(row) {
			return nil, fmt.Errorf("captured session %s/%s has not returned (state=%s running=%t closed=%t)", row.Template, row.ID, row.State, row.Running, row.Closed)
		}
		finalRows[row.Template] = row
	}
	for template, captured := range observed {
		if _, ok := finalRows[template]; !ok {
			return nil, fmt.Errorf("final history is missing captured session %s for template %s", captured.ID, template)
		}
	}
	return finalRows, nil
}

func TestListLiveReviewSessionBeadsUsesSupportedInfrastructureQuery(t *testing.T) {
	root := t.TempDir()
	argsPath := filepath.Join(root, "args.txt")
	gcPath := filepath.Join(root, "gc")
	const fakeGC = `#!/bin/sh
set -eu
printf '%s\n' "$*" >"$GC_TEST_ARGS_FILE"
if [ "$*" != "bd list --all --include-infra --json --limit=0 --type=session" ]; then
  printf 'unsupported gc arguments: %s\n' "$*" >&2
  exit 64
fi
printf '%s\n' "$GC_TEST_BD_OUTPUT"
`
	require.NoError(t, os.WriteFile(gcPath, []byte(fakeGC), 0o755))
	env := helpers.NewEnv(gcPath, filepath.Join(root, "gc-home"), filepath.Join(root, "runtime"))
	validOutput := `[{"id":"session-one","status":"closed","issue_type":"session","metadata":{"template":"laneOneAgent","session_name":"runtime-one","state":"drained"}}]`
	env.With("GC_TEST_ARGS_FILE", argsPath).With("GC_TEST_BD_OUTPUT", validOutput)

	beads, out, err := listLiveReviewSessionBeads(env, root)
	require.NoError(t, err, out)
	require.Len(t, beads, 1)
	require.Equal(t, "session-one", beads[0].ID)
	require.Equal(t, "session", beads[0].IssueType)
	args, err := os.ReadFile(argsPath)
	require.NoError(t, err)
	require.Equal(t, "bd list --all --include-infra --json --limit=0 --type=session", strings.TrimSpace(string(args)))

	for name, invalid := range map[string]string{
		"concatenated": "[]\n" + validOutput,
		"trailing":     validOutput + " trailing",
		"duplicate":    `[{"id":"session-one","id":"replacement","issue_type":"session"}]`,
	} {
		t.Run(name, func(t *testing.T) {
			env.With("GC_TEST_BD_OUTPUT", invalid)
			_, _, err := listLiveReviewSessionBeads(env, root)
			require.Error(t, err)
		})
	}
}

func liveReviewSessionReturned(row liveReviewSession) bool {
	if row.Running {
		return false
	}
	if row.Closed {
		return true
	}
	switch session.State(strings.TrimSpace(row.State)) {
	case session.StateAsleep, session.StateSuspended, session.StateDrained,
		session.StateArchived, session.StateFailedCreate:
		return true
	default:
		return false
	}
}

func requireLiveReviewNull(t *testing.T, raw json.RawMessage, field string) {
	t.Helper()
	require.Equal(t, "null", strings.TrimSpace(string(raw)), "%s must be explicit JSON null", field)
}

func sortedLiveReviewKeys[V any](values map[string]V) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func TestLiveReviewExactMetadataStringPreservesCompactBytes(t *testing.T) {
	t.Parallel()

	want := `{"second":2,"first":1}`
	got, err := liveReviewExactMetadataString(map[string]any{"gc.output_json": want}, "gc.output_json")
	require.NoError(t, err)
	require.Equal(t, want, got)

	_, err = liveReviewExactMetadataString(map[string]any{"gc.output_json": map[string]any{"first": 1}}, "gc.output_json")
	require.ErrorContains(t, err, "must be a string")
}

func TestLiveReviewFindingCoverageRequiresEveryDistinctReviewerFinding(t *testing.T) {
	t.Parallel()

	laneOne := liveReviewLaneArtifact{Findings: []liveReviewFinding{{ID: "L1-001"}, {ID: "L1-002"}}}
	laneTwo := liveReviewLaneArtifact{Findings: []liveReviewFinding{{ID: "L2-001"}}}
	complete := liveReviewSynthesisArtifact{
		IncorporatedFindings: []string{"L1-001", "L2-001"},
		DeferredFindings: []struct {
			ID     string `json:"id"`
			Reason string `json:"reason"`
		}{{ID: "L1-002", Reason: "Deferred with concrete evidence."}},
	}
	require.NoError(t, validateLiveReviewFindingCoverage(laneOne, laneTwo, complete))

	omitted := complete
	omitted.DeferredFindings = nil
	require.ErrorContains(t, validateLiveReviewFindingCoverage(laneOne, laneTwo, omitted), "L1-002")

	invented := complete
	invented.IncorporatedFindings = append(append([]string(nil), complete.IncorporatedFindings...), "not-a-review-finding")
	require.ErrorContains(t, validateLiveReviewFindingCoverage(laneOne, laneTwo, invented), "not-a-review-finding")

	whitespaceSynthesis := complete
	whitespaceSynthesis.IncorporatedFindings = append([]string(nil), complete.IncorporatedFindings...)
	whitespaceSynthesis.IncorporatedFindings[0] = " L1-001 "
	require.ErrorContains(t, validateLiveReviewFindingCoverage(laneOne, laneTwo, whitespaceSynthesis), "surrounding whitespace")

	whitespaceLane := laneOne
	whitespaceLane.Findings = append([]liveReviewFinding(nil), laneOne.Findings...)
	whitespaceLane.Findings[0].ID = " L1-001 "
	require.ErrorContains(t, validateLiveReviewFindingCoverage(whitespaceLane, laneTwo, complete), "surrounding whitespace")

	duplicateLaneID := laneTwo
	duplicateLaneID.Findings = []liveReviewFinding{{ID: "L1-001"}}
	require.ErrorContains(t, validateLiveReviewFindingCoverage(laneOne, duplicateLaneID, complete), "duplicate reviewer finding id")
}

func TestLiveReviewJournalValidationRejectsEarlyDependentAdmission(t *testing.T) {
	t.Parallel()

	event := func(seq uint64, eventType, payload string) graphstore.StoredEvent {
		return graphstore.StoredEvent{
			Seq: seq,
			JournalEvent: graphstore.JournalEvent{
				Type:    eventType,
				Payload: []byte(payload),
			},
		}
	}
	valid := []graphstore.StoredEvent{
		event(1, engine.EventOwnedAdmitted, `{"activation":"reviewLaneOne:0","kind":"work_bead","bead_id":"one"}`),
		event(2, engine.EventOwnedAdmitted, `{"activation":"reviewLaneTwo:0","kind":"work_bead","bead_id":"two"}`),
		event(3, engine.EventOutcomeSettled, `{"activation":"reviewLaneOne:0","outcome":"pass"}`),
		event(4, engine.EventOutcomeSettled, `{"activation":"reviewLaneTwo:0","outcome":"pass"}`),
		event(5, engine.EventOwnedAdmitted, `{"activation":"synthesize:0","kind":"work_bead","bead_id":"synthesis"}`),
		event(6, engine.EventOutcomeSettled, `{"activation":"synthesize:0","outcome":"pass"}`),
		event(7, engine.EventOwnedAdmitted, `{"activation":"verify:0","kind":"work_bead","bead_id":"verification"}`),
		event(8, engine.EventOutcomeSettled, `{"activation":"verify:0","outcome":"pass"}`),
		event(9, engine.EventRunClosed, `{"outcome":"pass"}`),
	}
	beads, err := validateLiveReviewJournal(valid)
	require.NoError(t, err)
	require.Equal(t, "verification", beads["verify:0"])

	early := append([]graphstore.StoredEvent(nil), valid...)
	early[4].Seq = 4
	_, err = validateLiveReviewJournal(early)
	require.ErrorContains(t, err, "before both reviewers settled")

	settledBeforeAdmission := append([]graphstore.StoredEvent(nil), valid...)
	settledBeforeAdmission[0].Seq = 4
	_, err = validateLiveReviewJournal(settledBeforeAdmission)
	require.ErrorContains(t, err, "settled before its admission")

	afterClosure := append(append([]graphstore.StoredEvent(nil), valid...), event(10, "lumen.node.activated", `{}`))
	_, err = validateLiveReviewJournal(afterClosure)
	require.ErrorContains(t, err, "final stream event")
}

func TestLiveReviewRevisionDiffMustMatchOriginalAndRevised(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	originalPath := filepath.Join(dir, "original.md")
	revisedPath := filepath.Join(dir, "revised.md")
	require.NoError(t, os.WriteFile(originalPath, []byte("old line\n"), 0o644))
	require.NoError(t, os.WriteFile(revisedPath, []byte("new line\n"), 0o644))

	matching := []byte("--- original.md\n+++ revised.md\n@@ -1 +1 @@ optional GNU diff heading\n-old line\n+new line\n")
	require.NoError(t, validateLiveReviewRevisionDiff(originalPath, revisedPath, matching))

	fake := []byte("--- original.md\n+++ revised.md\n@@ -1 +1 @@\n-old line\n+unrelated line\n")
	require.ErrorContains(t, validateLiveReviewRevisionDiff(originalPath, revisedPath, fake), "does not agree")
}

func TestLiveReviewPhaseSelectionPrefersActiveReplacement(t *testing.T) {
	t.Parallel()

	rows := []liveReviewSession{
		{ID: "old-one", Template: "laneOneAgent", State: "drained", SessionName: "old-one", CreatedAt: "2026-01-01T00:00:00Z"},
		{ID: "newest-one", Template: "laneOneAgent", State: "active", SessionName: "newest-one", CreatedAt: "2026-01-01T00:00:03Z"},
		{ID: "replacement-one", Template: "laneOneAgent", State: "active", SessionName: "replacement-one", CreatedAt: "2026-01-01T00:00:02Z", Running: true},
		{ID: "not-inference", Template: "laneOneAgent", State: "draining", SessionName: "not-inference", CreatedAt: "2026-01-01T00:00:04Z", Running: true},
		{ID: "lane-two", Template: "laneTwoAgent", State: "awake", SessionName: "lane-two", CreatedAt: "2026-01-01T00:00:01Z"},
		{ID: "not-started", Template: "laneTwoAgent", State: "creating", SessionName: "not-started", CreatedAt: "2026-01-01T00:00:05Z", Running: true},
	}
	want := map[string]string{"laneOneAgent": "claude", "laneTwoAgent": "codex"}
	selected := selectLiveReviewPhaseSessions(rows, want, true)
	require.Equal(t, "newest-one", selected["laneOneAgent"].ID)
	require.Equal(t, "lane-two", selected["laneTwoAgent"].ID)
	require.NoError(t, validateLiveReviewTmuxSnapshot(selected, []string{"newest-one", "lane-two", "unrelated"}))
	require.Error(t, validateLiveReviewTmuxSnapshot(selected, []string{"newest-one"}))
	require.Error(t, validateLiveReviewTmuxAbsence(selected, []string{"newest-one", "unrelated"}))
	require.NoError(t, validateLiveReviewTmuxAbsence(selected, []string{"unrelated"}))
}

func TestLiveReviewPhaseHistoryExcludesProvisionalSessionRows(t *testing.T) {
	t.Parallel()

	rows := []liveReviewSession{
		{ID: "completed", Template: "synthesisAgent", State: "drained", SessionName: "synthesis-runtime", CreatedAt: "2026-01-01T00:00:01Z"},
		{ID: "pending", Template: "synthesisAgent", State: "start-pending", SessionName: "synthesis-pending-token", CreatedAt: "2026-01-01T00:00:02Z"},
		{ID: "creating", Template: "synthesisAgent", State: "creating", SessionName: "synthesis-creating-token", CreatedAt: "2026-01-01T00:00:03Z"},
	}
	selected := selectLiveReviewPhaseSessions(rows, map[string]string{"synthesisAgent": "claude"}, false)
	require.Equal(t, "completed", selected["synthesisAgent"].ID)
}

func TestLiveReviewTmuxSnapshotUsesIsolatedEnvironment(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	require.NoError(t, os.MkdirAll(binDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "tmux"), []byte(`#!/bin/sh
if [ "$TMUX_TMPDIR" != "/isolated/tmux-root" ]; then
  printf 'unexpected TMUX_TMPDIR: %s\n' "$TMUX_TMPDIR" >&2
  exit 9
fi
printf 'reviewer-one\nreviewer-two\n'
`), 0o755))
	t.Setenv("PATH", binDir+":"+os.Getenv("PATH"))

	cityDir := filepath.Join(dir, "city")
	require.NoError(t, os.MkdirAll(cityDir, 0o755))
	sessions, err := listTmuxSessionsOnCitySocketWithEnv(cityDir, []string{
		"PATH=/usr/bin:/bin",
		"TMUX_TMPDIR=/isolated/tmux-root",
	})
	require.NoError(t, err)
	require.Equal(t, []string{"reviewer-one", "reviewer-two"}, sessions)
}

func TestStageLiveReviewCodexAuthPrefersConfiguredGateway(t *testing.T) {
	t.Setenv("GC_WORKER_INFERENCE_CODEX_AUTH_JSON", "")
	t.Setenv("GC_WORKER_INFERENCE_CODEX_AUTH_FILE", "")
	t.Setenv("OPENAI_BASE_URL", "https://gateway.example.test/api")
	t.Setenv("OPENAI_API_KEY", "gateway-key")
	staleCodexHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(staleCodexHome, "auth.json"), []byte(`{"tokens":{"access_token":"expired"}}`), 0o600))
	t.Setenv("CODEX_HOME", staleCodexHome)

	gcHome := t.TempDir()
	env := helpers.NewEnv("", gcHome, t.TempDir())
	source, err := stageLiveReviewCodexAuth(gcHome, env)
	require.NoError(t, err)
	require.Equal(t, "env:OPENAI_GATEWAY", source)
	require.Empty(t, env.Get("OPENAI_API_KEY"), "gateway token must not be exposed in the worker process environment")
	require.Empty(t, env.Get("OPENAI_BASE_URL"))
	require.NoFileExists(t, filepath.Join(gcHome, ".codex", "auth.json"))
	tokenPath := filepath.Join(gcHome, ".codex", "gateway-token")
	token, err := os.ReadFile(tokenPath)
	require.NoError(t, err)
	require.Equal(t, "gateway-key", string(token))

	config, err := os.ReadFile(filepath.Join(gcHome, ".codex", "config.toml"))
	require.NoError(t, err)
	require.Contains(t, string(config), `model_provider = "gc_lumen_gateway"`)
	require.Contains(t, string(config), `base_url = "https://gateway.example.test/api/v1"`)
	require.Contains(t, string(config), `wire_api = "responses"`)
	require.Contains(t, string(config), `[model_providers.gc_lumen_gateway.auth]`)
	require.Contains(t, string(config), `command = `)
	require.Contains(t, string(config), `args = [`+strconv.Quote(tokenPath)+`]`)
	require.NotContains(t, string(config), "gateway-key")
	require.NotContains(t, string(config), `env_key = "OPENAI_API_KEY"`)
}

func TestLiveReviewSessionProvenanceBindsObservedRuntime(t *testing.T) {
	t.Parallel()

	row := liveReviewSession{Template: "laneTwoAgent", SessionName: "gc-lane-two"}
	metadata := map[string]any{
		"template":     "laneTwoAgent",
		"provider":     "codex",
		"session_name": "gc-lane-two",
	}
	require.NoError(t, validateLiveReviewSessionProvenance(row, "codex", metadata))

	wrongTransport := make(map[string]any, len(metadata)+1)
	for key, value := range metadata {
		wrongTransport[key] = value
	}
	wrongTransport["transport"] = "acp"
	require.ErrorContains(t, validateLiveReviewSessionProvenance(row, "codex", wrongTransport), "transport")

	wrongName := make(map[string]any, len(metadata))
	for key, value := range metadata {
		wrongName[key] = value
	}
	wrongName["session_name"] = "different-runtime"
	require.ErrorContains(t, validateLiveReviewSessionProvenance(row, "codex", wrongName), "session_name")
}

func TestLiveReviewRepositorySnapshotIsCommittedCleanAndReadOnly(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	source := filepath.Join(root, "source")
	snapshot := filepath.Join(root, "snapshot")
	require.NoError(t, os.MkdirAll(source, 0o755))
	runGit := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		out, err := cmd.CombinedOutput()
		require.NoError(t, err, "git %v\n%s", args, out)
		return strings.TrimSpace(string(out))
	}
	runGit(source, "init", "--quiet")
	runGit(source, "config", "user.name", "Live Review Test")
	runGit(source, "config", "user.email", "live-review@example.invalid")
	tracked := filepath.Join(source, "tracked.txt")
	require.NoError(t, os.WriteFile(tracked, []byte("committed\n"), 0o644))
	runGit(source, "add", "tracked.txt")
	runGit(source, "commit", "--quiet", "-m", "fixture")
	wantCommit := runGit(source, "rev-parse", "HEAD")
	require.NoError(t, os.WriteFile(tracked, []byte("dirty\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(source, "untracked-secret.txt"), []byte("do not copy\n"), 0o600))

	t.Cleanup(func() { _ = makeLiveReviewTreeOwnerWritable(snapshot) })
	gotCommit, err := stageLiveReviewRepositorySnapshot(source, snapshot)
	require.NoError(t, err)
	require.Equal(t, wantCommit, gotCommit)
	data, err := os.ReadFile(filepath.Join(snapshot, "tracked.txt"))
	require.NoError(t, err)
	require.Equal(t, "committed\n", string(data))
	require.NoFileExists(t, filepath.Join(snapshot, "untracked-secret.txt"))
	require.NoError(t, requireLiveReviewTreeReadOnly(snapshot))
	requireLiveReviewRepositorySnapshotUnchanged(t, snapshot, wantCommit)
}

func TestLiveReviewRuntimeOverrideScrubRejectsAmbientBackends(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	env := helpers.NewEnv("", filepath.Join(root, "gc-home"), filepath.Join(root, "runtime"))
	for _, key := range liveReviewRuntimeOverrideKeys {
		env.With(key, "poison")
	}
	scrubLiveReviewRuntimeOverrides(env)
	for _, key := range liveReviewRuntimeOverrideKeys {
		require.Empty(t, env.Get(key), "%s survived the live-review runtime override scrub", key)
	}
}

func TestLiveReviewGitEnvScrubsAmbientOverrides(t *testing.T) {
	t.Setenv("GIT_CEILING_DIRECTORIES", "/poison")

	gitEntries := make([]string, 0)
	for _, entry := range liveReviewGitEnv() {
		key, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(key, "GIT_") {
			gitEntries = append(gitEntries, entry)
		}
	}
	require.Equal(t, []string{"GIT_OPTIONAL_LOCKS=0"}, gitEntries)
}

func TestLiveReviewContentManifestIncludesIgnoredAndGitFiles(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	ignoredPath := filepath.Join(root, "ignored", "cache.bin")
	gitObjectPath := filepath.Join(root, ".git", "objects", "ab", "object")
	require.NoError(t, os.MkdirAll(filepath.Dir(ignoredPath), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Dir(gitObjectPath), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, ".gitignore"), []byte("ignored/\n"), 0o644))
	require.NoError(t, os.WriteFile(ignoredPath, []byte("ignored-before\n"), 0o444))
	require.NoError(t, os.WriteFile(gitObjectPath, []byte("git-before\n"), 0o444))

	ignoredInfoBefore, err := os.Stat(ignoredPath)
	require.NoError(t, err)
	before, err := liveReviewContentManifest(root)
	require.NoError(t, err)
	require.Contains(t, before.Entries, "ignored/cache.bin")
	require.Contains(t, before.Entries, ".git/objects/ab/object")

	unchanged, err := liveReviewContentManifest(root)
	require.NoError(t, err)
	require.NoError(t, compareLiveReviewContentManifests(before, unchanged))
	ignoredInfoAfter, err := os.Stat(ignoredPath)
	require.NoError(t, err)
	require.Equal(t, ignoredInfoBefore.Mode(), ignoredInfoAfter.Mode(), "manifesting changed file permissions")
	require.Equal(t, ignoredInfoBefore.ModTime(), ignoredInfoAfter.ModTime(), "manifesting changed file contents or timestamps")

	require.NoError(t, os.Chmod(ignoredPath, 0o644))
	require.NoError(t, os.WriteFile(ignoredPath, []byte("ignored-after\n"), 0o444))
	require.NoError(t, os.Chmod(ignoredPath, 0o444))
	afterIgnoredChange, err := liveReviewContentManifest(root)
	require.NoError(t, err)
	require.ErrorContains(t, compareLiveReviewContentManifests(before, afterIgnoredChange), "ignored/cache.bin")

	require.NoError(t, os.Chmod(ignoredPath, 0o644))
	require.NoError(t, os.WriteFile(ignoredPath, []byte("ignored-before\n"), 0o444))
	require.NoError(t, os.Chmod(ignoredPath, 0o444))
	require.NoError(t, os.Chmod(gitObjectPath, 0o644))
	require.NoError(t, os.WriteFile(gitObjectPath, []byte("git-after\n"), 0o444))
	require.NoError(t, os.Chmod(gitObjectPath, 0o444))
	afterGitChange, err := liveReviewContentManifest(root)
	require.NoError(t, err)
	require.ErrorContains(t, compareLiveReviewContentManifests(before, afterGitChange), ".git/objects/ab/object")
}

func TestLiveReviewFinalHistoryRequiresExactlyCapturedSessions(t *testing.T) {
	t.Parallel()

	observed := map[string]liveReviewSession{
		"laneOneAgent":   {ID: "lane-one", Template: "laneOneAgent", SessionName: "lane-one-runtime"},
		"laneTwoAgent":   {ID: "lane-two", Template: "laneTwoAgent", SessionName: "lane-two-runtime"},
		"synthesisAgent": {ID: "synthesis", Template: "synthesisAgent", SessionName: "synthesis-runtime"},
		"verifierAgent":  {ID: "verification", Template: "verifierAgent", SessionName: "verification-runtime"},
	}
	beads := []liveReviewBead{
		{ID: "lane-one", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "laneOneAgent", "session_name": "lane-one-runtime", "state": "drained"}},
		{ID: "lane-two", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "laneTwoAgent", "session_name": "lane-two-runtime", "state": "drained"}},
		{ID: "synthesis", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "synthesisAgent", "session_name": "synthesis-runtime", "state": "drained"}},
		{ID: "verification", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "verifierAgent", "session_name": "verification-runtime", "state": "drained"}},
	}

	final, err := validateLiveReviewFinalSessionBeads(beads, observed)
	require.NoError(t, err)
	require.Len(t, final, len(liveReviewExpectedProviders))

	withReplacement := append(append([]liveReviewBead(nil), beads...), liveReviewBead{
		ID: "lane-one-replacement", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "laneOneAgent", "session_name": "lane-one-replacement", "state": "drained"},
	})
	_, err = validateLiveReviewFinalSessionBeads(withReplacement, observed)
	require.ErrorContains(t, err, "unexpected session")

	withUnrelated := append(append([]liveReviewBead(nil), beads...), liveReviewBead{
		ID: "unrelated", Status: "closed", IssueType: "session", Metadata: map[string]any{"template": "other", "session_name": "other-runtime", "state": "drained"},
	})
	_, err = validateLiveReviewFinalSessionBeads(withUnrelated, observed)
	require.ErrorContains(t, err, "unexpected session")

	missingCaptured := append([]liveReviewBead(nil), beads[1:]...)
	_, err = validateLiveReviewFinalSessionBeads(missingCaptured, observed)
	require.ErrorContains(t, err, "missing captured session")

	wrongTemplate := append([]liveReviewBead(nil), beads...)
	wrongTemplate[0].Metadata = map[string]any{"template": "laneTwoAgent", "session_name": "lane-one-runtime", "state": "drained"}
	_, err = validateLiveReviewFinalSessionBeads(wrongTemplate, observed)
	require.ErrorContains(t, err, "changed template")

	notReturned := append([]liveReviewBead(nil), beads...)
	notReturned[0].Status = "open"
	notReturned[0].Metadata = map[string]any{"template": "laneOneAgent", "session_name": "lane-one-runtime", "state": "active"}
	_, err = validateLiveReviewFinalSessionBeads(notReturned, observed)
	require.ErrorContains(t, err, "has not returned")
}

func TestLiveReviewPeekRequiresStructuredNonEmptyTranscript(t *testing.T) {
	t.Parallel()

	valid := `{"schema_version":"1","session_id":"session-one","target":"session-one","lines":80,"line_count":2,"output":"real provider output\nsecond line"}`
	transcript, err := liveReviewPeekTranscript(valid, "session-one")
	require.NoError(t, err)
	require.Contains(t, transcript, "real provider output")

	for _, invalid := range []string{
		"(cache age: 45s — reconciler may be lagging)\n",
		`{"schema_version":"1","session_id":"session-one","target":"session-one","lines":80,"line_count":0,"output":""}`,
		`{"schema_version":"1","session_id":"other","target":"other","lines":80,"line_count":1,"output":"wrong session"}`,
		valid + "\n{}",
		`{"schema_version":"1","session_id":"session-one","target":"session-one","lines":80,"line_count":1,"output":"fake","output":"real provider output"}`,
	} {
		_, err := liveReviewPeekTranscript(invalid, "session-one")
		require.Error(t, err)
	}
}

func TestLiveReviewStrictJSONRejectsAmbiguousEvidence(t *testing.T) {
	t.Parallel()

	type providerArtifact struct {
		Provider string `json:"provider"`
	}
	valid, err := decodeLiveReviewStrictJSON[providerArtifact]([]byte(`{"provider":"claude"}`))
	require.NoError(t, err)
	require.Equal(t, "claude", valid.Provider)

	for name, raw := range map[string]string{
		"duplicate":    `{"provider":"codex","provider":"claude"}`,
		"concatenated": "{\"provider\":\"claude\"}\n{}",
		"trailing":     `{"provider":"claude"} trailing`,
		"non-finite":   `{"provider":"claude","score":NaN}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := decodeLiveReviewStrictJSON[providerArtifact]([]byte(raw))
			require.Error(t, err)
		})
	}
}
