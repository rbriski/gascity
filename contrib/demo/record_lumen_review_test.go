package demo_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

const helperLibrary = "lumen-review-common.sh"

func TestCanonicalDemoBaseRejectsEscapes(t *testing.T) {
	t.Parallel()

	allowed := t.TempDir()
	got, err := runHelper(t, nil, "lumen_demo_canonical_base", filepath.Join(allowed, "runs"))
	if err != nil {
		t.Fatalf("canonical allowed base: %v\n%s", err, got)
	}
	if strings.TrimSpace(got) != filepath.Join(allowed, "runs") {
		t.Fatalf("canonical base = %q, want %q", strings.TrimSpace(got), filepath.Join(allowed, "runs"))
	}

	for _, escaped := range []string{"/tmp", "/data/tmp", "/tmp/../data/projects"} {
		if out, err := runHelper(t, nil, "lumen_demo_canonical_base", escaped); err == nil {
			t.Fatalf("canonical base accepted unsafe path %q: %s", escaped, out)
		}
	}

	symlink := filepath.Join(allowed, "escape")
	if err := os.Symlink("/data/projects", symlink); err != nil {
		t.Fatalf("creating escape symlink: %v", err)
	}
	if out, err := runHelper(t, nil, "lumen_demo_canonical_base", filepath.Join(symlink, "runs")); err == nil {
		t.Fatalf("canonical base accepted symlink escape: %s", out)
	}
}

func TestCodexGatewayURLRequiresHTTPSAndNormalizesV1(t *testing.T) {
	t.Parallel()

	for input, want := range map[string]string{
		"https://gateway.example.test":        "https://gateway.example.test/v1",
		"https://gateway.example.test/api/":   "https://gateway.example.test/api/v1",
		"https://gateway.example.test/api/v1": "https://gateway.example.test/api/v1",
	} {
		out, err := runHelper(t, nil, "lumen_demo_codex_gateway_url", input)
		if err != nil {
			t.Fatalf("normalize %q: %v\n%s", input, err, out)
		}
		if strings.TrimSpace(out) != want {
			t.Fatalf("normalize %q = %q, want %q", input, strings.TrimSpace(out), want)
		}
	}

	for _, invalid := range []string{
		"", "http://gateway.example.test", "https://user@gateway.example.test",
		"https://gateway.example.test?route=other", "https://gateway.example.test/#fragment",
	} {
		if out, err := runHelper(t, nil, "lumen_demo_codex_gateway_url", invalid); err == nil {
			t.Fatalf("unsafe gateway URL %q accepted: %s", invalid, out)
		}
	}
}

func TestSessionSnapshotContracts(t *testing.T) {
	t.Parallel()

	snapshot := writeFixture(t, `{
  "sessions": [
    {"id":"s-one","template":"laneOneAgent","state":"active","closed":false,"session_name":"one","created_at":"2026-07-14T10:00:00Z"},
    {"id":"s-two","template":"laneTwoAgent","state":"awake","session_name":"two","created_at":"2026-07-14T10:00:01Z"},
    {"id":"s-synth","template":"synthesisAgent","state":"active","running":false,"session_name":"synth","created_at":"2026-07-14T10:01:00Z"},
    {"id":"s-verify","template":"verifierAgent","state":"asleep","session_name":"verify","created_at":"2026-07-14T10:02:00Z"},
    {"id":"other","template":"unrelated","state":"active","closed":false,"session_name":"other","created_at":"2026-07-14T10:03:00Z"}
  ]
}`)

	rows, err := runHelper(t, nil, "lumen_demo_reviewers_concurrent", snapshot)
	if err != nil {
		t.Fatalf("simultaneous reviewers not detected: %v\n%s", err, rows)
	}
	for _, id := range []string{"s-one", "s-two"} {
		if !strings.Contains(rows, id) {
			t.Errorf("reviewer rows omit %q: %s", id, rows)
		}
	}

	seen, err := runHelper(t, nil, "lumen_demo_phase_row", snapshot, "synthesisAgent")
	if err != nil || !strings.Contains(seen, "s-synth") {
		t.Fatalf("active synthesis lifecycle row not discoverable: err=%v out=%s", err, seen)
	}

	ids := `["s-one","s-two","s-synth","s-verify"]`
	if out, err := runHelper(t, nil, "lumen_demo_sessions_returned", snapshot, ids); err == nil {
		t.Fatalf("active tracked reviewers incorrectly classified as returned: %s", out)
	}

	returned := writeFixture(t, `{
  "sessions": [
    {"id":"s-one","template":"laneOneAgent","state":"active","closed":true},
    {"id":"s-two","template":"laneTwoAgent","state":"drained"},
    {"id":"s-synth","template":"synthesisAgent","state":"asleep","closed":false},
    {"id":"s-verify","template":"verifierAgent","state":"archived"},
    {"id":"other","template":"unrelated","state":"active","closed":false}
  ]
}`)
	if out, err := runHelper(t, nil, "lumen_demo_sessions_returned", returned, ids); err != nil {
		t.Fatalf("returned sessions rejected because of unrelated activity: %v\n%s", err, out)
	}

	captured := writeFixture(t, `[
  {"id":"s-one","template":"laneOneAgent","session_name":"one"},
  {"id":"s-two","template":"laneTwoAgent","session_name":"two"},
  {"id":"s-synth","template":"synthesisAgent","session_name":"synth"},
  {"id":"s-verify","template":"verifierAgent","session_name":"verify"}
]`)
	emptyProjection := writeFixture(t, `{"sessions":[]}`)
	if out, err := runHelper(t, nil, "lumen_demo_session_projection_returned", emptyProjection, captured); err != nil {
		t.Fatalf("empty final session projection rejected: %v\n%s", err, out)
	}
	unexpectedProjection := writeFixture(t, `{"sessions":[{"id":"replacement","state":"asleep"}]}`)
	if out, err := runHelper(t, nil, "lumen_demo_session_projection_returned", unexpectedProjection, captured); err == nil {
		t.Fatalf("unexpected final session projection accepted: %s", out)
	}
}

func TestReviewerConcurrencyRequiresActiveDistinctLifecycleRows(t *testing.T) {
	t.Parallel()

	for _, payload := range []string{
		`{"sessions":[{"id":"one","template":"laneOneAgent","state":"active","session_name":"one","created_at":"2026-07-14T10:00:00Z"}]}`,
		`{"sessions":[{"id":"two","template":"laneTwoAgent","state":"active","session_name":"two","created_at":"2026-07-14T10:00:00Z"}]}`,
		`{"sessions":[{"id":"one","template":"laneOneAgent","state":"start-pending","session_name":"one","created_at":"2026-07-14T10:00:00Z"},{"id":"two","template":"laneTwoAgent","state":"creating","session_name":"two","created_at":"2026-07-14T10:00:01Z"}]}`,
		`{"sessions":[{"id":"one","template":"laneOneAgent","state":"active","closed":true,"session_name":"one","created_at":"2026-07-14T10:00:00Z"},{"id":"two","template":"laneTwoAgent","state":"active","session_name":"two","created_at":"2026-07-14T10:00:01Z"}]}`,
		`{"sessions":[{"id":"same","template":"laneOneAgent","state":"active","session_name":"same","created_at":"2026-07-14T10:00:00Z"},{"id":"same","template":"laneTwoAgent","state":"active","session_name":"same","created_at":"2026-07-14T10:00:01Z"}]}`,
	} {
		snapshot := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_reviewers_concurrent", snapshot); err == nil {
			t.Fatalf("non-concurrent reviewer snapshot accepted: %s", out)
		}
	}
}

func TestPhaseSelectionRequiresActiveOpenLifecycleAndPicksNewestReplacement(t *testing.T) {
	t.Parallel()

	snapshot := writeFixture(t, `{
  "sessions": [
    {"id":"closed-newest","template":"synthesisAgent","state":"active","closed":true,"session_name":"closed","created_at":"2026-07-14T10:03:00Z"},
    {"id":"replacement","template":"synthesisAgent","state":"active","running":false,"session_name":"replacement","created_at":"2026-07-14T10:02:00Z"},
    {"id":"first-attempt","template":"synthesisAgent","state":"active","session_name":"first","created_at":"2026-07-14T10:01:00Z"}
  ]
}`)
	got, err := runHelper(t, nil, "lumen_demo_phase_row", snapshot, "synthesisAgent")
	if err != nil {
		t.Fatalf("select newest live replacement: %v\n%s", err, got)
	}
	if !strings.Contains(got, "replacement") || strings.Contains(got, "first-attempt") {
		t.Fatalf("phase row did not select newest active replacement: %s", got)
	}

	inactive := writeFixture(t, `{"sessions":[{"id":"closed","template":"synthesisAgent","state":"asleep","session_name":"closed","created_at":"2026-07-14T10:03:00Z"}]}`)
	if out, err := runHelper(t, nil, "lumen_demo_phase_row", inactive, "synthesisAgent"); err == nil {
		t.Fatalf("inactive phase row accepted: %s", out)
	}
}

func TestReturnedSessionsRequireKnownInactiveStates(t *testing.T) {
	t.Parallel()

	ids := `["one","two","three","four"]`
	for _, payload := range []string{
		`{"sessions":[{"id":"one","state":"active","closed":false},{"id":"two","state":"asleep"},{"id":"three","state":"drained"},{"id":"four","state":"archived"}]}`,
		`{"sessions":[{"id":"one","state":"running"},{"id":"two","state":"asleep"},{"id":"three","state":"drained"},{"id":"four","state":"archived"}]}`,
		`{"sessions":[{"id":"one","state":"idle"},{"id":"two","state":"asleep"},{"id":"three","state":"drained"},{"id":"four","state":"archived"}]}`,
		`{"sessions":[{"id":"one","state":""},{"id":"two","state":"asleep"},{"id":"three","state":"drained"},{"id":"four","state":"archived"}]}`,
		`{"sessions":[{"id":"one","state":"asleep"},{"id":"two","state":"asleep"},{"id":"three","state":"drained"}]}`,
	} {
		snapshot := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_sessions_returned", snapshot, ids); err == nil {
			t.Fatalf("unsafe returned-session snapshot accepted: %s", out)
		}
	}
}

func TestTmuxSnapshotRequiresRuntimePresenceAndAbsence(t *testing.T) {
	t.Parallel()

	snapshot := writeFixture(t, "runtime-one\nruntime-two\nunrelated\n")
	if out, err := runHelper(t, nil, "lumen_demo_tmux_sessions_present", snapshot, `["runtime-one","runtime-two"]`); err != nil {
		t.Fatalf("coexisting runtime sessions rejected: %v\n%s", err, out)
	}
	if out, err := runHelper(t, nil, "lumen_demo_tmux_sessions_present", snapshot, `["runtime-one","missing"]`); err == nil {
		t.Fatalf("missing runtime session accepted as present: %s", out)
	}
	if out, err := runHelper(t, nil, "lumen_demo_tmux_sessions_absent", snapshot, `["runtime-one","runtime-two"]`); err == nil {
		t.Fatalf("live runtime sessions accepted as absent: %s", out)
	}

	empty := writeFixture(t, "")
	if out, err := runHelper(t, nil, "lumen_demo_tmux_sessions_absent", empty, `["runtime-one","runtime-two"]`); err != nil {
		t.Fatalf("empty runtime snapshot rejected as absent: %v\n%s", err, out)
	}
}

func TestSessionBeadProvenanceAllowsCanonicalOmittedTmuxTransport(t *testing.T) {
	t.Parallel()

	for _, transport := range []string{"", `,"transport":"tmux"`} {
		bead := writeFixture(t, `[{"id":"session-one","issue_type":"session","metadata":{"template":"laneOneAgent","provider":"claude","session_name":"runtime-one"`+transport+`}}]`)
		if out, err := runHelper(t, nil, "lumen_demo_session_bead_provenance", bead, "session-one", "runtime-one", "laneOneAgent", "claude"); err != nil {
			t.Fatalf("canonical tmux provenance rejected: %v\n%s", err, out)
		}
	}

	contradictory := writeFixture(t, `[{"id":"session-one","issue_type":"session","metadata":{"template":"laneOneAgent","provider":"claude","session_name":"runtime-one","transport":"acp"}}]`)
	if out, err := runHelper(t, nil, "lumen_demo_session_bead_provenance", contradictory, "session-one", "runtime-one", "laneOneAgent", "claude"); err == nil {
		t.Fatalf("contradictory transport accepted: %s", out)
	}

	concatenated := writeFixture(t, `[]
[{"id":"session-one","issue_type":"session","metadata":{"template":"laneOneAgent","provider":"claude","session_name":"runtime-one"}}]`)
	if out, err := runHelper(t, nil, "lumen_demo_session_bead_provenance", concatenated, "session-one", "runtime-one", "laneOneAgent", "claude"); err == nil {
		t.Fatalf("concatenated provenance arrays accepted: %s", out)
	}
}

func TestSessionSetRequiresFourUniqueTemplatesIDsAndNames(t *testing.T) {
	t.Parallel()

	valid := writeFixture(t, `[
  {"id":"one","template":"laneOneAgent","session_name":"runtime-one"},
  {"id":"two","template":"laneTwoAgent","session_name":"runtime-two"},
  {"id":"three","template":"synthesisAgent","session_name":"runtime-three"},
  {"id":"four","template":"verifierAgent","session_name":"runtime-four"}
]`)
	if out, err := runHelper(t, nil, "lumen_demo_session_set", valid); err != nil {
		t.Fatalf("valid session set rejected: %v\n%s", err, out)
	}

	for _, payload := range []string{
		`[{"id":"one","template":"laneOneAgent","session_name":"one"}]`,
		`[{"id":"same","template":"laneOneAgent","session_name":"one"},{"id":"same","template":"laneTwoAgent","session_name":"two"},{"id":"three","template":"synthesisAgent","session_name":"three"},{"id":"four","template":"verifierAgent","session_name":"four"}]`,
		`[{"id":"one","template":"laneOneAgent","session_name":"same"},{"id":"two","template":"laneTwoAgent","session_name":"same"},{"id":"three","template":"synthesisAgent","session_name":"three"},{"id":"four","template":"verifierAgent","session_name":"four"}]`,
		`[{"id":"one","template":"laneOneAgent","session_name":"one"},{"id":"two","template":"laneTwoAgent","session_name":"two"},{"id":"three","template":"synthesisAgent","session_name":"three"},{"id":"four","template":"unexpected","session_name":"four"}]`,
	} {
		rows := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_session_set", rows); err == nil {
			t.Fatalf("invalid session identity set accepted: %s", out)
		}
	}
}

func TestSessionBeadHistoryRequiresExactCapturedReturnedSet(t *testing.T) {
	t.Parallel()

	captured := writeFixture(t, `[
  {"id":"one","template":"laneOneAgent","session_name":"runtime-one"},
  {"id":"two","template":"laneTwoAgent","session_name":"runtime-two"},
  {"id":"three","template":"synthesisAgent","session_name":"runtime-three"},
  {"id":"four","template":"verifierAgent","session_name":"runtime-four"}
]`)
	valid := writeFixture(t, `[
  {"id":"one","status":"closed","issue_type":"session","metadata":{"template":"laneOneAgent","session_name":"runtime-one","state":"drained"}},
  {"id":"two","status":"closed","issue_type":"session","metadata":{"template":"laneTwoAgent","session_name":"runtime-two","state":"drained"}},
  {"id":"three","status":"open","issue_type":"session","metadata":{"template":"synthesisAgent","session_name":"runtime-three","state":"asleep"}},
  {"id":"four","status":"closed","issue_type":"session","metadata":{"template":"verifierAgent","session_name":"runtime-four","state":"drained"}}
]`)
	if out, err := runHelper(t, nil, "lumen_demo_session_beads_returned", valid, captured); err != nil {
		t.Fatalf("exact returned session beads rejected: %v\n%s", err, out)
	}
	validData, err := os.ReadFile(valid)
	if err != nil {
		t.Fatal(err)
	}

	for _, payload := range []string{
		`[{"id":"one","status":"closed","issue_type":"session","metadata":{"template":"laneOneAgent","session_name":"runtime-one","state":"drained"}}]`,
		`[{"id":"one","status":"closed","issue_type":"session","metadata":{"template":"laneOneAgent","session_name":"runtime-one","state":"drained"}},{"id":"replacement","status":"closed","issue_type":"session","metadata":{"template":"laneTwoAgent","session_name":"replacement","state":"drained"}}]`,
		`[{"id":"one","status":"open","issue_type":"session","metadata":{"template":"laneOneAgent","session_name":"runtime-one","state":"active"}},{"id":"two","status":"closed","issue_type":"session","metadata":{"template":"laneTwoAgent","session_name":"runtime-two","state":"drained"}},{"id":"three","status":"closed","issue_type":"session","metadata":{"template":"synthesisAgent","session_name":"runtime-three","state":"drained"}},{"id":"four","status":"closed","issue_type":"session","metadata":{"template":"verifierAgent","session_name":"runtime-four","state":"drained"}}]`,
		"[]\n" + string(validData),
	} {
		beads := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_session_beads_returned", beads, captured); err == nil {
			t.Fatalf("invalid final session bead history accepted: %s", out)
		}
	}
}

func TestSingleJSONObjectValidation(t *testing.T) {
	t.Parallel()

	pretty := writeFixture(t, "{\n  \"ok\": true\n}\n")
	if got, err := runHelper(t, nil, "lumen_demo_single_json_object", pretty, "any"); err != nil || strings.TrimSpace(got) != `{"ok":true}` {
		t.Fatalf("single pretty object rejected: err=%v out=%q", err, got)
	}
	if out, err := runHelper(t, nil, "lumen_demo_single_json_object", pretty, "compact"); err == nil {
		t.Fatalf("pretty object accepted as compact: %s", out)
	}

	compact := writeFixture(t, "{\"ok\":true}\n")
	if got, err := runHelper(t, nil, "lumen_demo_single_json_object", compact, "compact"); err != nil || strings.TrimSpace(got) != `{"ok":true}` {
		t.Fatalf("compact object rejected: err=%v out=%q", err, got)
	}
	goEscaped := writeFixture(t, "{\"output\":\"\\u003ccommand\\u003e \\u0026\",\"number\":1e0}\n")
	if got, err := runHelper(t, nil, "lumen_demo_single_json_object", goEscaped, "compact"); err != nil || strings.TrimSpace(got) != `{"output":"\u003ccommand\u003e \u0026","number":1e0}` {
		t.Fatalf("compact Go-style object rejected: err=%v out=%q", err, got)
	}

	for _, payload := range []string{
		"{\"one\":1}\n{\"two\":2}\n",
		"[{\"one\":1}]\n",
		"{\"one\":1} trailing\n",
		"{\"one\":1,\"one\":2}\n",
		"{\"one\":NaN}\n",
		"{\"one\":Infinity}\n",
		"{\"one\":-Infinity}\n",
	} {
		path := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_single_json_object", path, "any"); err == nil {
			t.Fatalf("non-single-object JSON accepted: %s", out)
		}
	}
	for _, payload := range []string{
		"{\"one\": 1}\n",
		"{\"one\":1} \n",
		"{\n\"one\":1}\n",
		"{\"one\":1}\n\n",
	} {
		path := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_single_json_object", path, "compact"); err == nil {
			t.Fatalf("JSON with insignificant whitespace accepted as compact: %s", out)
		}
	}
}

func TestSingleJSONArrayValidation(t *testing.T) {
	t.Parallel()

	pretty := writeFixture(t, "[\n  {\"id\": \"one\"},\n  {\"id\": \"two\"}\n]\n")
	got, err := runHelper(t, nil, "lumen_demo_single_json_array", pretty)
	if err != nil || strings.TrimSpace(got) != `[{"id":"one"},{"id":"two"}]` {
		t.Fatalf("single pretty array rejected: err=%v out=%q", err, got)
	}

	for _, payload := range []string{
		"[]\n[{\"id\":\"replacement\"}]\n",
		"[{\"id\":\"one\"}] trailing\n",
		`{"id":"not-an-array"}`,
		`[{"id":"one","id":"replacement"}]`,
		`[{"score":NaN}]`,
	} {
		path := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_single_json_array", path); err == nil {
			t.Fatalf("invalid single-array evidence accepted: %s", out)
		}
	}
}

func TestResolveCodexNativeExecutableBehindBunWrapper(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	launcher := filepath.Join(home, ".local", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(launcher), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(launcher, []byte("#!/bin/bash\nexec ~/.bun/bin/bun ~/.bun/bin/codex \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	jsEntrypoint := filepath.Join(home, ".bun", "install", "global", "node_modules", "@openai", "codex", "bin", "codex.js")
	if err := os.MkdirAll(filepath.Dir(jsEntrypoint), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jsEntrypoint, []byte("#!/usr/bin/env node\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(home, ".bun", "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(jsEntrypoint, filepath.Join(home, ".bun", "bin", "codex")); err != nil {
		t.Fatal(err)
	}
	native := filepath.Join(home, ".bun", "install", "global", "node_modules", "@openai", "codex-linux-x64", "vendor", "x86_64-unknown-linux-musl", "bin", "codex")
	if err := os.MkdirAll(filepath.Dir(native), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(native, []byte{'\x7f', 'E', 'L', 'F', 't', 'e', 's', 't'}, 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := runHelper(t, nil, "lumen_demo_resolve_codex_native", launcher, home, "x86_64")
	if err != nil {
		t.Fatalf("resolve native Codex behind Bun wrapper: %v\n%s", err, got)
	}
	if strings.TrimSpace(got) != native {
		t.Fatalf("native Codex = %q, want %q", strings.TrimSpace(got), native)
	}

	direct, err := runHelper(t, nil, "lumen_demo_resolve_codex_native", native, home, "x86_64")
	if err != nil || strings.TrimSpace(direct) != native {
		t.Fatalf("direct native Codex rejected: err=%v out=%q", err, direct)
	}

	if out, err := runHelper(t, nil, "lumen_demo_resolve_codex_native", launcher, t.TempDir(), "x86_64"); err == nil {
		t.Fatalf("wrapper without a pinned native Codex was accepted: %s", out)
	}
}

func TestFileMatchesSHA256RejectsMutationAndSymlink(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "design.md")
	original := []byte("committed design\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(original)
	expected := hex.EncodeToString(digest[:])
	if out, err := runHelper(t, nil, "lumen_demo_file_matches_sha256", path, expected); err != nil {
		t.Fatalf("matching committed file rejected: %v\n%s", err, out)
	}
	if err := os.WriteFile(path, []byte("replacement design\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runHelper(t, nil, "lumen_demo_file_matches_sha256", path, expected); err == nil {
		t.Fatalf("mutated baseline accepted: %s", out)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(path), "target.md")
	if err := os.WriteFile(target, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if out, err := runHelper(t, nil, "lumen_demo_file_matches_sha256", path, expected); err == nil {
		t.Fatalf("symlink baseline accepted: %s", out)
	}
}

func TestProcessExecutableMatchesPinnedPathAndBytes(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("process executable identity proof requires Linux /proc")
	}

	sleepSource, err := exec.LookPath("sleep")
	if err != nil {
		t.Skipf("sleep executable unavailable: %v", err)
	}
	sleepSource, err = filepath.EvalSymlinks(sleepSource)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(sleepSource)
	if err != nil {
		t.Fatal(err)
	}
	pinned := filepath.Join(t.TempDir(), "sleep")
	pinnedTemp := pinned + ".tmp"
	pinnedFile, err := os.OpenFile(pinnedTemp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o555)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pinnedFile.Write(payload); err != nil {
		_ = pinnedFile.Close()
		t.Fatal(err)
	}
	if err := pinnedFile.Sync(); err != nil {
		_ = pinnedFile.Close()
		t.Fatal(err)
	}
	if err := pinnedFile.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(pinnedTemp, pinned); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	expected := hex.EncodeToString(digest[:])
	cmd := exec.Command(pinned, "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	pid := cmd.Process.Pid
	if out, err := runHelper(t, nil, "lumen_demo_process_executable_matches",
		strconv.Itoa(pid), pinned, expected); err != nil {
		t.Fatalf("pinned executable rejected: %v\n%s", err, out)
	}
	if out, err := runHelper(t, nil, "lumen_demo_process_executable_matches",
		strconv.Itoa(pid), sleepSource, expected); err == nil {
		t.Fatalf("different executable path accepted: %s", out)
	}
	if out, err := runHelper(t, nil, "lumen_demo_process_executable_matches",
		strconv.Itoa(pid), pinned, strings.Repeat("0", 64)); err == nil {
		t.Fatalf("wrong executable digest accepted: %s", out)
	}
}

func TestWaitPIDGoneRequiresTheCapturedProcessToExit(t *testing.T) {
	t.Parallel()
	if runtime.GOOS != "linux" {
		t.Skip("PID lifecycle proof requires Linux /proc")
	}

	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	t.Cleanup(func() {
		if !stopped {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	})
	pid := strconv.Itoa(cmd.Process.Pid)
	if out, err := runHelper(t, nil, "lumen_demo_wait_pid_gone", pid, "2", "0.01"); err == nil {
		t.Fatalf("live captured PID was reported gone: %s", out)
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err == nil {
		t.Fatal("killed process unexpectedly returned success")
	}
	stopped = true
	if out, err := runHelper(t, nil, "lumen_demo_wait_pid_gone", pid, "2", "0.01"); err != nil {
		t.Fatalf("exited captured PID was not reported gone: %v\n%s", err, out)
	}
}

func TestLaneValidationRequiresFindingsArray(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	artifact := filepath.Join(dir, "lane-one.json")
	objective := "Make the design implementation-ready"
	document := filepath.Join(dir, "design.md")
	repository := filepath.Join(dir, "repository")
	finding := func(id string) map[string]any {
		return map[string]any{
			"id":             id,
			"severity":       "high",
			"title":          "Concrete implementation gap",
			"evidence":       []string{"Section State transitions omits the durable ownership handoff"},
			"impact":         "A crash can leave two workers believing they own the same operation.",
			"recommendation": "Specify one compare-and-swap transition and its recovery invariant.",
		}
	}
	payload := map[string]any{
		"schema":          "review-quorum.lane.v1",
		"lane":            "implementation-realism",
		"provider":        "claude",
		"verdict":         "revise",
		"summary":         "The design is directionally sound but leaves durable ownership and crash recovery behavior underspecified for an implementer.",
		"objective":       objective,
		"document_path":   document,
		"repository_path": repository,
		"artifact_path":   artifact,
		"findings": []any{
			finding("L1-001"),
			finding("L1-002"),
			finding("L1-003"),
		},
		"failure_class": nil,
	}
	writeJSON := func(value map[string]any) {
		t.Helper()
		encoded, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(artifact, append(encoded, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	validate := func() (string, error) {
		return runHelper(t, nil, "lumen_demo_validate_lane", artifact,
			"implementation-realism", "claude", objective, document, repository, artifact)
	}

	writeJSON(payload)
	if out, err := validate(); err != nil {
		t.Fatalf("valid lane artifact rejected: %v\n%s", err, out)
	}
	payload["findings"] = map[string]any{
		"first":  finding("L1-001"),
		"second": finding("L1-002"),
		"third":  finding("L1-003"),
	}
	writeJSON(payload)
	if out, err := validate(); err == nil {
		t.Fatalf("object-shaped findings accepted: %s", out)
	}
}

func TestFindingCoverageRequiresExactOneToOneClassification(t *testing.T) {
	t.Parallel()

	laneOne := writeFixture(t, `{"findings":[{"id":"L1-001"},{"id":"L1-002"}]}`)
	laneTwo := writeFixture(t, `{"findings":[{"id":"L2-001"}]}`)
	valid := writeFixture(t, `{
  "incorporated_findings":["L1-001","L2-001"],
  "deferred_findings":[{"id":"L1-002","reason":"Deferred for a concrete evidence-based reason."}]
}`)
	if out, err := runHelper(t, nil, "lumen_demo_validate_finding_coverage", laneOne, laneTwo, valid); err != nil {
		t.Fatalf("exact finding classification rejected: %v\n%s", err, out)
	}

	for name, payload := range map[string]string{
		"invented": `{"incorporated_findings":["L1-001","L1-002","L2-001"],"deferred_findings":[{"id":"L1-002/L2-001-extension"}]}`,
		"omitted":  `{"incorporated_findings":["L1-001","L2-001"],"deferred_findings":[]}`,
		"duplicate": `{"incorporated_findings":["L1-001","L1-002","L2-001"],
  "deferred_findings":[{"id":"L1-002"}]}`,
	} {
		synthesis := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_validate_finding_coverage", laneOne, laneTwo, synthesis); err == nil {
			t.Fatalf("%s finding classification accepted: %s", name, out)
		}
	}

	spacedLane := writeFixture(t, `{"findings":[{"id":" L1-001 "}]}`)
	spacedSynthesis := writeFixture(t, `{"incorporated_findings":[" L1-001 ","L2-001"],"deferred_findings":[]}`)
	if out, err := runHelper(t, nil, "lumen_demo_validate_finding_coverage", spacedLane, laneTwo, spacedSynthesis); err == nil {
		t.Fatalf("finding IDs with surrounding whitespace accepted: %s", out)
	}
}

func TestTreeDigestChangesWithContentAndSymlinkTargets(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "document.md"), []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "alternate.md"), []byte("alternate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("document.md", filepath.Join(root, "current.md")); err != nil {
		t.Fatal(err)
	}
	before, err := runHelper(t, nil, "lumen_demo_tree_sha256", root)
	if err != nil {
		t.Fatalf("initial tree digest: %v\n%s", err, before)
	}
	if err := os.WriteFile(filepath.Join(root, "document.md"), []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after, err := runHelper(t, nil, "lumen_demo_tree_sha256", root)
	if err != nil {
		t.Fatalf("updated tree digest: %v\n%s", err, after)
	}
	if strings.TrimSpace(before) == strings.TrimSpace(after) {
		t.Fatal("tree digest did not change with file content")
	}
	if len(strings.TrimSpace(after)) != 64 {
		t.Fatalf("tree digest = %q, want 64 hex characters", strings.TrimSpace(after))
	}
	if err := os.Chmod(filepath.Join(root, "document.md"), 0o700); err != nil {
		t.Fatal(err)
	}
	modeChanged, err := runHelper(t, nil, "lumen_demo_tree_sha256", root)
	if err != nil {
		t.Fatalf("mode-updated tree digest: %v\n%s", err, modeChanged)
	}
	if strings.TrimSpace(after) == strings.TrimSpace(modeChanged) {
		t.Fatal("tree digest did not change with file permission bits")
	}
	if err := os.Remove(filepath.Join(root, "current.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("alternate.md", filepath.Join(root, "current.md")); err != nil {
		t.Fatal(err)
	}
	targetChanged, err := runHelper(t, nil, "lumen_demo_tree_sha256", root)
	if err != nil {
		t.Fatalf("symlink-updated tree digest: %v\n%s", err, targetChanged)
	}
	if strings.TrimSpace(modeChanged) == strings.TrimSpace(targetChanged) {
		t.Fatal("tree digest did not change with a symlink target")
	}
}

func TestEvidenceSecretScanRejectsCredentialMaterialWithoutEchoingIt(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "safe.json"), []byte(`{"provider":"codex","outcome":"pass"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := runHelper(t, nil, "lumen_demo_scan_evidence_secrets", root)
	if err != nil {
		t.Fatalf("safe evidence rejected: %v\n%s", err, got)
	}
	var summary struct {
		ScannedFiles int `json:"scanned_files"`
		Matches      int `json:"matches"`
	}
	if err := json.Unmarshal([]byte(got), &summary); err != nil {
		t.Fatalf("decode safe scan summary: %v\n%s", err, got)
	}
	if summary.ScannedFiles != 1 || summary.Matches != 0 {
		t.Fatalf("safe scan summary = %+v, want one file and zero matches", summary)
	}

	secret := "sk-ant-api03-THIS_VALUE_MUST_NEVER_APPEAR_IN_OUTPUT"
	if err := os.WriteFile(filepath.Join(root, "leak.cast"), []byte("Bearer "+secret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, err := runHelper(t, nil, "lumen_demo_scan_evidence_secrets", root)
	if err == nil {
		t.Fatal("credential-shaped evidence was accepted")
	}
	if strings.Contains(out, secret) {
		t.Fatal("secret scanner echoed rejected credential material")
	}

	if err := os.Remove(filepath.Join(root, "leak.cast")); err != nil {
		t.Fatal(err)
	}
	opaque := "opaque-environment-credential-7a6f3e2d"
	if err := os.WriteFile(filepath.Join(root, "environment.log"), []byte(opaque), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runHelper(t, []string{"CLAUDE_CODE_OAUTH_TOKEN=" + opaque}, "lumen_demo_scan_evidence_secrets", root); err == nil {
		t.Fatal("exact inherited environment credential was accepted")
	} else if strings.Contains(out, opaque) {
		t.Fatal("secret scanner echoed rejected environment credential")
	}

	if err := os.Remove(filepath.Join(root, "environment.log")); err != nil {
		t.Fatal(err)
	}
	credentialFile := filepath.Join(t.TempDir(), "auth.json")
	credentialValue := "opaque-credential-file-value-93f9fbc1"
	if err := os.WriteFile(credentialFile, []byte(`{"access_token":"`+credentialValue+`"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "provider.log"), []byte(credentialValue), 0o600); err != nil {
		t.Fatal(err)
	}
	if out, err := runHelper(t, nil, "lumen_demo_scan_evidence_secrets", root, credentialFile); err == nil {
		t.Fatal("exact credential-file value was accepted")
	} else if strings.Contains(out, credentialValue) {
		t.Fatal("secret scanner echoed rejected credential-file material")
	}

	if err := os.Remove(filepath.Join(root, "provider.log")); err != nil {
		t.Fatal(err)
	}
	if mkfifo, err := exec.LookPath("mkfifo"); err == nil {
		fifo := filepath.Join(root, "unexpected.fifo")
		if out, err := exec.Command(mkfifo, fifo).CombinedOutput(); err != nil {
			t.Fatalf("create FIFO fixture: %v\n%s", err, out)
		}
		if out, err := runHelper(t, nil, "lumen_demo_scan_evidence_secrets", root); err == nil {
			t.Fatalf("special filesystem entry was accepted into retained evidence: %s", out)
		}
	}
}

func TestRetargetDiffRequiresOneWellFormedUnifiedFile(t *testing.T) {
	t.Parallel()

	valid := writeFixture(t, "--- original.md\n+++ revised.md\n@@ -1,2 +1,2 @@\n before\n-old\n+new\n")
	got, err := runHelper(t, nil, "lumen_demo_retarget_diff", valid)
	if err != nil {
		t.Fatalf("valid unified diff rejected: %v\n%s", err, got)
	}
	if !strings.HasPrefix(got, "--- document.md\n+++ document.md\n@@ -1,2 +1,2 @@\n") {
		t.Fatalf("retargeted diff has unsafe or missing headers: %q", got)
	}

	for _, payload := range []string{
		"diff --git a/original.md b/revised.md\n--- original.md\n+++ revised.md\n@@ -1 +1 @@\n-old\n+new\n",
		"--- original.md\n+++ revised.md\n@@ -1 +1 @@\n-old\n+new\n--- ../../outside\n+++ ../../outside\n@@ -1 +1 @@\n-safe\n+pwned\n",
		"--- original.md\n+++ revised.md\n@@ -1,2 +1,2 @@\n-old\n+new\n",
		"--- original.md\n+++ revised.md\nnot a hunk\n",
	} {
		path := writeFixture(t, payload)
		if out, err := runHelper(t, nil, "lumen_demo_retarget_diff", path); err == nil {
			t.Fatalf("malformed or multi-file diff accepted: %s", out)
		}
	}
}

func TestBinaryCommitMustMatchCleanCommittedSource(t *testing.T) {
	t.Parallel()

	fake := filepath.Join(t.TempDir(), "gc")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$FAKE_VERSION_JSON\"\n"), 0o755); err != nil {
		t.Fatalf("writing fake gc: %v", err)
	}
	repoCommit := "0123456789abcdef0123456789abcdef01234567"

	for _, commit := range []string{repoCommit, repoCommit[:12]} {
		out, err := runHelper(t, []string{"FAKE_VERSION_JSON={\"commit\":\"" + commit + "\"}"}, "lumen_demo_binary_commit", fake, repoCommit)
		if err != nil {
			t.Fatalf("matching binary commit %q rejected: %v\n%s", commit, err, out)
		}
	}
	for _, commit := range []string{"unknown", repoCommit + "-dirty", "deadbeefdeadbeef"} {
		if out, err := runHelper(t, []string{"FAKE_VERSION_JSON={\"commit\":\"" + commit + "\"}"}, "lumen_demo_binary_commit", fake, repoCommit); err == nil {
			t.Fatalf("unverifiable binary commit %q accepted: %s", commit, out)
		}
	}
}

func TestRecorderRequiresTruthfulRunEvidence(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"lumen_demo_canonical_base",
		"mktemp -d",
		"diff --quiet --",
		"diff --cached --quiet --",
		"session list --state all --json",
		"lumen_demo_reviewers_concurrent",
		"lumen_demo_tmux_sessions_present",
		"lumen_demo_tmux_sessions_absent",
		"lumen_demo_session_beads_returned",
		"lumen_demo_single_json_array",
		"lumen_demo_session_projection_returned",
		"tmux-sessions-reviewers.txt",
		"tmux-sessions-synthesis.txt",
		"tmux-sessions-verifier.txt",
		"tmux-sessions-final.txt",
		"session-beads-final.json",
		"repo_commit",
		"binary_commit",
		"binary_sha256",
		"repo_snapshot",
		"repo_snapshot_sha256",
		`init_template="$root/init-template"`,
		`cp -a -- "$repo_snapshot/examples/lumen/review-quorum-live" "$init_template"`,
		`chmod -R u+w "$init_template"`,
		`--from "$init_template"`,
		"dolt-provider-state.json",
		"LUMEN_DEMO_CODEX_BASE_URL",
		"gc_lumen_gateway",
		"GC_CODEX_INFERENCE_AUTH_OK",
		"codex-auth-preflight.txt",
		"timeout --kill-after=5s 30s claude auth status </dev/null",
		"gateway-token",
		"model_providers.gc_lumen_gateway.auth",
		"refresh_interval_ms = 0",
		`stop_tmux_socket_bounded "$city_tmux_socket" City`,
		"removing failed-run evidence because its credential scan failed",
		"session peek",
		"peek-",
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
		"hasTrustDialogAccepted",
		`trust_level = "trusted"`,
		"agents/laneOneAgent/agent.toml",
		"agents/laneTwoAgent/agent.toml",
		"agents/synthesisAgent/agent.toml",
		"agents/verifierAgent/agent.toml",
		"lane_artifacts_valid",
		`(.deferred_findings | type) == "array"`,
		"lumen_demo_validate_finding_coverage",
		"patch --batch",
		"evidence-sha256.json",
		"tracked_tree_clean",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("recorder is missing evidence contract %q", required)
		}
	}
	if strings.Contains(text, `rm -rf "$root"`) {
		t.Error("recorder still recursively deletes a caller-controlled root")
	}
	for _, forbidden := range []string{
		`cp -f "$HOME/.codex/auth.json"`,
		`cp -f "$HOME/.claude/.credentials.json"`,
		`(.running|tostring)`,
		`codex login status`,
		`env_key = "OPENAI_API_KEY"`,
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("recorder retains forbidden demo behavior: %s", forbidden)
		}
	}
}

func TestRecorderPinsProviderAndDoltExecutableProvenance(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"#!/bin/bash -p",
		"toolchain_dir",
		"toolchain.json",
		"trusted_system_path",
		"BASH_ENV",
		"PYTHONNOUSERSITE",
		"PYTHONSAFEPATH",
		"TAR_OPTIONS",
		"RIPGREP_CONFIG_PATH",
		"python3 -I",
		"GC_TMUX_SESSION",
		"TMUX_PANE",
		"OPENAI_BASE_URL",
		"ANTHROPIC_BASE_URL",
		"BD_* | BEADS_*",
		`/proc/$pid/exe`,
		"lumen_demo_process_executable_matches",
		"dolt-process-executable",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("recorder is missing pinned executable or routing proof %q", required)
		}
	}
	if strings.Contains(text, `:$PATH`) {
		t.Error("recorder retains an untrusted ambient PATH tail")
	}
}

func TestRecorderBoundsSessionObservers(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		`timeout --kill-after=2s 15s "$gc_bin" session list`,
		`timeout --kill-after=2s 15s "$gc_bin" session peek`,
		`timeout --kill-after=2s 10s tmux -L "$city_tmux_socket" list-sessions`,
		`timeout --kill-after=2s 10s tmux -L "$recorder_socket" capture-pane`,
		`timeout --kill-after=2s 10s "$gc_bin" supervisor status`,
		`timeout --kill-after=2s 30s "$gc_bin" bd show`,
	} {
		if !strings.Contains(text, required) {
			t.Errorf("recorder has an unbounded observer; missing %q", required)
		}
	}
}

func TestRecorderValidatesTheExactSessionOutputShownInTheCast(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"display-session-list-",
		"peek-display-",
		`tee $quoted_display_file`,
		`lumen_demo_single_json_object "$display_file"`,
		"validate_displayed_session_list",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("recorder does not validate its displayed session output; missing %q", required)
		}
	}
}

func TestRecorderFetchesSessionHistoryWithSupportedInfrastructureQuery(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, `"$gc_bin" bd list \`) ||
		!strings.Contains(text, `--all --include-infra --json --limit=0 --type=session)`) {
		t.Fatalf("recorder does not fetch closed and open infrastructure beads with the supported bd list contract")
	}
}

func TestRecorderSecuresEvidenceAndWaitsForTheContinuousRecorder(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	text := string(data)
	for _, required := range []string{
		"umask 077",
		`mkdir -- "$evidence"`,
		"wait_recording_attached",
		"tmux -L \"$recorder_socket\" list-clients",
		"--env SHELL,TERM",
		"--idle-time-limit 86400",
		"sessions-pre-shutdown.json",
	} {
		if !strings.Contains(text, required) {
			t.Errorf("recorder is missing secure/continuous recording guard %q", required)
		}
	}
}

func TestRecorderSocketFitsUnixPathLimit(t *testing.T) {
	t.Parallel()

	data, err := os.ReadFile("record-lumen-review.sh")
	if err != nil {
		t.Fatalf("reading recorder: %v", err)
	}
	if !strings.Contains(string(data), `recorder_socket="recorder"`) {
		t.Fatal("recorder socket name is not the pinned short name")
	}

	defaultRoot := filepath.Join(
		"/data/tmp/lumen-review-real",
		"run-20060102T150405Z-4194304-XXXXXX",
	)
	socketPath := filepath.Join(defaultRoot, "runtime", "tmux", "tmux-4294967295", "recorder")
	if len(socketPath) >= 104 {
		t.Fatalf("default recorder socket path is %d bytes, want fewer than 104: %s", len(socketPath), socketPath)
	}
}

func TestRecorderPrivilegedShebangDoesNotSourceBashEnv(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	marker := filepath.Join(dir, "bash-env-ran")
	bashEnv := filepath.Join(dir, "bash-env.sh")
	if err := os.WriteFile(bashEnv, []byte("/usr/bin/touch \""+marker+"\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("./record-lumen-review.sh")
	cmd.Env = append(os.Environ(), "BASH_ENV="+bashEnv)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("recorder accepted an injected BASH_ENV: %s", out)
	}
	if !strings.Contains(string(out), "refusing shell or dynamic-loader injection") {
		t.Fatalf("recorder failed for the wrong reason: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("privileged recorder sourced BASH_ENV before rejection: %v", err)
	}
}

func TestRecorderShellSyntax(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("bash", "-n", "record-lumen-review.sh", helperLibrary)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("recorder shell syntax: %v\n%s", err, out)
	}
}

func runHelper(t *testing.T, extraEnv []string, function string, args ...string) (string, error) {
	t.Helper()
	command := "source ./" + helperLibrary + "\n" + function + " \"$@\""
	cmd := exec.Command("bash", "-c", command, "bash")
	cmd.Args = append(cmd.Args, args...)
	cmd.Env = append(os.Environ(), extraEnv...)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func writeFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "sessions.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writing fixture: %v", err)
	}
	return path
}
