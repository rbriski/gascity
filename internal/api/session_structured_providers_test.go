package api

import (
	"context"
	"crypto/md5" //nolint:gosec // Kimi transcript fixtures use the provider's MD5 workdir layout.
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandleSessionTranscriptStructuredNormalizesFirstClassProviders(t *testing.T) {
	resume := session.ProviderResume{
		ResumeFlag:    "--resume",
		ResumeStyle:   "flag",
		SessionIDFlag: "--session-id",
	}

	tests := []struct {
		name          string
		provider      string
		writeFixture  func(t *testing.T, root, workDir, sessionKey string)
		toolCallID    string
		toolName      string
		inputKind     string
		inputFilePath string
		inputCommand  string
		inputPattern  string
		inputText     string
		resultKind    string
		resultFile    string
		resultContent string
		resultStdout  string
		resultExit    *int
		resultFiles   []string
	}{
		{
			name:          "claude read",
			provider:      "claude",
			writeFixture:  writeStructuredClaudeReadFixture,
			toolCallID:    "call-claude-read",
			toolName:      "Read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Gas City README",
		},
		{
			name:          "codex patch",
			provider:      "codex",
			writeFixture:  writeStructuredCodexPatchFixture,
			toolCallID:    "call-codex-patch",
			toolName:      "apply_patch",
			inputKind:     "patch",
			inputFilePath: "city.toml",
			resultKind:    "edit",
			resultFile:    "city.toml",
			resultContent: "Updated the following files",
		},
		{
			name:         "gemini grep",
			provider:     "gemini",
			writeFixture: writeStructuredGeminiGrepFixture,
			toolCallID:   "call-gemini-grep",
			toolName:     "grep_search",
			inputKind:    "search",
			inputPattern: "needle",
			resultKind:   "grep",
			resultFiles:  []string{"README.md", "main.go"},
		},
		{
			name:          "kimi read",
			provider:      "kimi",
			writeFixture:  writeStructuredKimiReadFixture,
			toolCallID:    "call-kimi-read",
			toolName:      "Read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Kimi file data",
		},
		{
			name:          "opencode edit",
			provider:      "opencode",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:          "groq opencode alias edit",
			provider:      "groq",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:          "cerebras opencode alias edit",
			provider:      "cerebras",
			writeFixture:  writeStructuredOpenCodeEditFixture,
			toolCallID:    "call-opencode-edit",
			toolName:      "Edit",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "edit",
			resultFile:    "README.md",
			resultContent: "Edited README.md",
		},
		{
			name:         "mimocode bash",
			provider:     "mimocode",
			writeFixture: writeStructuredMimoCodeBashFixture,
			toolCallID:   "call-mimocode-bash",
			toolName:     "Bash",
			inputKind:    "command",
			inputCommand: "go test ./...",
			resultKind:   "bash",
			resultStdout: "ok ./...",
			resultExit:   intPtr(0),
		},
		{
			name:          "pi read",
			provider:      "pi",
			writeFixture:  writeStructuredPiReadFixture,
			toolCallID:    "call-pi-read",
			toolName:      "read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Pi file data",
		},
		{
			name:          "omp pi alias read",
			provider:      "omp",
			writeFixture:  writeStructuredPiReadFixture,
			toolCallID:    "call-pi-read",
			toolName:      "read",
			inputKind:     "file",
			inputFilePath: "README.md",
			resultKind:    "read",
			resultFile:    "README.md",
			resultContent: "Pi file data",
		},
		{
			name:          "antigravity write",
			provider:      "antigravity",
			writeFixture:  writeStructuredAntigravityWriteFixture,
			toolCallID:    "call-antigravity-write",
			toolName:      "Write",
			inputKind:     "file",
			inputFilePath: "notes.txt",
			inputText:     "hello structured world",
			resultKind:    "edit",
			resultFile:    "notes.txt",
			resultContent: "wrote notes.txt",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fs := newSessionFakeState(t)
			searchBase := t.TempDir()
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)
			srv.sessionLogSearchPaths = []string{searchBase}

			mgr := session.NewManager(fs.cityBeadStore, fs.sp)
			workDir := t.TempDir()
			info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", tt.provider, workDir, tt.provider, nil, resume, runtime.Config{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			tt.writeFixture(t, searchBase, info.WorkDir, info.SessionKey)

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp sessionTranscriptGetResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Format != "structured" {
				t.Fatalf("Format = %q, want structured", resp.Format)
			}
			if resp.SchemaVersion != sessionStructuredSchemaVersion {
				t.Fatalf("SchemaVersion = %q, want %q", resp.SchemaVersion, sessionStructuredSchemaVersion)
			}
			if resp.History == nil || resp.History.TranscriptStreamID == "" {
				t.Fatalf("structured response missing history envelope: %+v", resp.History)
			}

			toolUse, toolResult := findStructuredToolPair(resp.StructuredMessages, tt.toolCallID)
			if toolUse == nil {
				t.Fatalf("missing tool_use %q in structured messages: %+v", tt.toolCallID, resp.StructuredMessages)
			}
			if toolResult == nil {
				t.Fatalf("missing tool_result %q in structured messages: %+v", tt.toolCallID, resp.StructuredMessages)
			}
			if toolUse.Name != tt.toolName {
				t.Fatalf("tool name = %q, want %q", toolUse.Name, tt.toolName)
			}
			if toolUse.Input == nil {
				t.Fatalf("tool input is nil")
			}
			assertStructuredInput(t, toolUse.Input, tt.inputKind, tt.inputFilePath, tt.inputCommand, tt.inputPattern, tt.inputText)
			assertStructuredResult(t, toolResult.Structured, tt.resultKind, tt.resultFile, tt.resultContent, tt.resultStdout, tt.resultExit, tt.resultFiles)

			wire, err := json.Marshal(resp)
			if err != nil {
				t.Fatalf("marshal structured response: %v", err)
			}
			for _, forbidden := range []string{"tool_use_id", "functionResponse", "toolCallId", "callID", "filePath", "oldString", "newString"} {
				if strings.Contains(string(wire), forbidden) {
					t.Fatalf("structured response leaked provider-native key %q: %s", forbidden, wire)
				}
			}
		})
	}
}

func TestHandleSessionTranscriptStructuredGracefullyDowngradesAllBuiltinProviders(t *testing.T) {
	for _, provider := range config.BuiltinProviderOrder() {
		t.Run(provider, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			mgr := session.NewManager(fs.cityBeadStore, fs.sp)
			info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", provider, t.TempDir(), provider, nil, session.ProviderResume{}, runtime.Config{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fs.sp.SetPeekOutput(info.SessionName, provider+" pane output")

			w := httptest.NewRecorder()
			r := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/transcript?format=structured&tail=0", nil)
			h.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
			}

			var resp sessionTranscriptGetResponse
			if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if resp.Format != "structured" {
				t.Fatalf("Format = %q, want structured; body: %s", resp.Format, w.Body.String())
			}
			if resp.SchemaVersion != sessionStructuredSchemaVersion {
				t.Fatalf("SchemaVersion = %q, want %q", resp.SchemaVersion, sessionStructuredSchemaVersion)
			}
			if resp.History == nil {
				t.Fatal("History is nil, want degraded structured history")
			}
			if resp.History.Continuity.Status != "degraded" {
				t.Fatalf("History continuity = %q, want degraded", resp.History.Continuity.Status)
			}
			if len(resp.History.Diagnostics) == 0 || resp.History.Diagnostics[0].Code != structuredTranscriptUnavailableCode {
				t.Fatalf("Diagnostics = %+v, want transcript_unavailable", resp.History.Diagnostics)
			}
			if len(resp.StructuredMessages) != 1 {
				t.Fatalf("StructuredMessages len = %d, want 1: %+v", len(resp.StructuredMessages), resp.StructuredMessages)
			}
			msg := resp.StructuredMessages[0]
			if msg.Provider != provider {
				t.Fatalf("message provider = %q, want %q", msg.Provider, provider)
			}
			if len(msg.Blocks) != 1 || msg.Blocks[0].Type != "text" || !strings.Contains(msg.Blocks[0].Text, provider+" pane output") {
				t.Fatalf("message blocks = %+v, want provider-neutral text fallback", msg.Blocks)
			}
		})
	}
}

func TestHandleSessionStreamStructuredGracefullyDowngradesWithoutTranscript(t *testing.T) {
	for _, provider := range config.BuiltinProviderOrder() {
		t.Run(provider, func(t *testing.T) {
			fs := newSessionFakeState(t)
			srv := New(fs)
			h := newTestCityHandlerWith(t, fs, srv)

			mgr := session.NewManager(fs.cityBeadStore, fs.sp)
			info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", provider, t.TempDir(), provider, nil, session.ProviderResume{}, runtime.Config{})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			fs.sp.SetPeekOutput(info.SessionName, provider+" pane output")

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			rec := newSyncResponseRecorder()
			req := httptest.NewRequest("GET", cityURL(fs, "/session/")+info.ID+"/stream?format=structured", nil).WithContext(ctx)
			done := make(chan struct{})
			go func() {
				h.ServeHTTP(rec, req)
				close(done)
			}()

			body := waitForRecorderSubstring(t, rec, `"format":"structured"`, 500*time.Millisecond)
			if !strings.Contains(body, `"format":"structured"`) {
				t.Fatalf("stream body missing structured fallback event: %s", body)
			}
			if !strings.Contains(body, structuredTranscriptUnavailableCode) {
				t.Fatalf("stream body missing degraded diagnostic: %s", body)
			}
			if !strings.Contains(body, provider+" pane output") {
				t.Fatalf("stream body missing text fallback: %s", body)
			}
			cancel()
			<-done
		})
	}
}

func TestLegacySessionTranscriptStructuredGracefullyDowngrades(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "cursor", t.TempDir(), "cursor", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs.sp.SetPeekOutput(info.SessionName, "cursor pane output")

	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/transcript?format=structured&tail=0", nil)
	srv.legacySessionHandler().ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("got status %d, want %d; body: %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp sessionTranscriptGetResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Format != "structured" {
		t.Fatalf("Format = %q, want structured; body: %s", resp.Format, w.Body.String())
	}
	if resp.History == nil || resp.History.Continuity.Status != "degraded" {
		t.Fatalf("History = %+v, want degraded structured fallback", resp.History)
	}
	if len(resp.StructuredMessages) != 1 || !strings.Contains(resp.StructuredMessages[0].Blocks[0].Text, "cursor pane output") {
		t.Fatalf("StructuredMessages = %+v, want cursor pane output text fallback", resp.StructuredMessages)
	}
}

func TestLegacySessionStreamStructuredGracefullyDowngrades(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	mgr := session.NewManager(fs.cityBeadStore, fs.sp)
	info, err := mgr.Create(context.Background(), "myrig/worker", "Chat", "cursor", t.TempDir(), "cursor", nil, session.ProviderResume{}, runtime.Config{})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	fs.sp.SetPeekOutput(info.SessionName, "cursor pane output")

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rec := newSyncResponseRecorder()
	req := httptest.NewRequest("GET", "/v0/session/"+info.ID+"/stream?format=structured", nil).WithContext(ctx)
	done := make(chan struct{})
	go func() {
		srv.legacySessionHandler().ServeHTTP(rec, req)
		close(done)
	}()

	body := waitForRecorderSubstring(t, rec, `"format":"structured"`, 500*time.Millisecond)
	if !strings.Contains(body, "event: structured") {
		t.Fatalf("stream body missing structured event name: %s", body)
	}
	if !strings.Contains(body, structuredTranscriptUnavailableCode) {
		t.Fatalf("stream body missing degraded diagnostic: %s", body)
	}
	if !strings.Contains(body, "cursor pane output") {
		t.Fatalf("stream body missing text fallback: %s", body)
	}
	cancel()
	<-done
}

func findStructuredToolPair(messages []SessionStructuredMessage, toolCallID string) (*SessionStructuredBlock, *SessionStructuredBlock) {
	var toolUse *SessionStructuredBlock
	var toolResult *SessionStructuredBlock
	for i := range messages {
		for j := range messages[i].Blocks {
			block := &messages[i].Blocks[j]
			switch block.Type {
			case "tool_use":
				if block.ID == toolCallID || block.ToolCallID == toolCallID {
					toolUse = block
				}
			case "tool_result":
				if block.ToolCallID == toolCallID {
					toolResult = block
				}
			}
		}
	}
	return toolUse, toolResult
}

func assertStructuredInput(t *testing.T, input *SessionStructuredToolInput, kind, filePath, command, pattern, text string) {
	t.Helper()
	if input.Kind != kind {
		t.Fatalf("input kind = %q, want %q; input = %+v", input.Kind, kind, input)
	}
	if filePath != "" && input.FilePath != filePath {
		t.Fatalf("input file_path = %q, want %q; input = %+v", input.FilePath, filePath, input)
	}
	if command != "" && input.Command != command {
		t.Fatalf("input command = %q, want %q; input = %+v", input.Command, command, input)
	}
	if pattern != "" && input.Pattern != pattern {
		t.Fatalf("input pattern = %q, want %q; input = %+v", input.Pattern, pattern, input)
	}
	if text != "" && input.Text != text {
		t.Fatalf("input text = %q, want %q; input = %+v", input.Text, text, input)
	}
}

func assertStructuredResult(t *testing.T, result *SessionStructuredToolResult, kind, filePath, content, stdout string, exitCode *int, filenames []string) {
	t.Helper()
	if result == nil {
		t.Fatal("structured result is nil")
	}
	if result.Kind != kind {
		t.Fatalf("result kind = %q, want %q; result = %+v", result.Kind, kind, result)
	}
	if filePath != "" && result.FilePath != filePath {
		t.Fatalf("result file_path = %q, want %q; result = %+v", result.FilePath, filePath, result)
	}
	if content != "" && !strings.Contains(result.Content, content) {
		t.Fatalf("result content = %q, want substring %q; result = %+v", result.Content, content, result)
	}
	if stdout != "" && result.Stdout != stdout {
		t.Fatalf("result stdout = %q, want %q; result = %+v", result.Stdout, stdout, result)
	}
	if exitCode != nil {
		if result.ExitCode == nil || *result.ExitCode != *exitCode {
			t.Fatalf("result exit_code = %v, want %d; result = %+v", result.ExitCode, *exitCode, result)
		}
	}
	for _, want := range filenames {
		if !stringSliceContains(result.Filenames, want) {
			t.Fatalf("result filenames = %#v, missing %q; result = %+v", result.Filenames, want, result)
		}
	}
}

func writeStructuredClaudeReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	writeNamedSessionJSONL(t, root, workDir, sessionKey+".jsonl",
		`{"uuid":"claude-1","parentUuid":"","type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","id":"call-claude-read","name":"Read","input":{"file_path":"README.md"}}]},"timestamp":"2026-06-01T00:00:00Z"}`,
		`{"uuid":"claude-2","parentUuid":"claude-1","type":"tool_result","toolUseID":"call-claude-read","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-claude-read","content":"Gas City README\n"}]},"timestamp":"2026-06-01T00:00:01Z"}`,
	)
}

func writeStructuredCodexPatchFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	dir := filepath.Join(root, "2026", "06", "01")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir codex dir: %v", err)
	}
	payload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-06-01T00:00:00Z","type":"session_meta","payload":{"cwd":%q}}`, workDir),
		`{"timestamp":"2026-06-01T00:00:01Z","type":"response_item","payload":{"type":"custom_tool_call","call_id":"call-codex-patch","name":"apply_patch","input":"*** Begin Patch\n*** Update File: city.toml\n@@\n+[workspace]\n*** End Patch\n"}}`,
		`{"timestamp":"2026-06-01T00:00:02Z","type":"response_item","payload":{"type":"custom_tool_call_output","call_id":"call-codex-patch","output":"{\"output\":\"Success. Updated the following files:\\nM city.toml\\n\"}"}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(dir, "rollout-2026-06-01T00-00-00-structured.jsonl"), []byte(payload), 0o644); err != nil {
		t.Fatalf("write codex fixture: %v", err)
	}
}

func writeStructuredGeminiGrepFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	projectDir := filepath.Join(root, "gemini-project")
	chatsDir := filepath.Join(projectDir, "chats")
	if err := os.MkdirAll(chatsDir, 0o755); err != nil {
		t.Fatalf("mkdir gemini chats: %v", err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, ".project_root"), []byte(workDir), 0o644); err != nil {
		t.Fatalf("write gemini project root: %v", err)
	}
	body := `{
  "sessionId": "gemini-structured",
  "messages": [
    {"id":"gemini-1","timestamp":"2026-06-01T00:00:00Z","type":"gemini","content":"searching","toolCalls":[{"id":"call-gemini-grep","name":"grep_search","args":{"pattern":"needle"},"result":[{"functionResponse":{"id":"call-gemini-grep","response":{"output":"main.go:7:needle\nREADME.md:1:needle\n"}}}]}]}
  ]
}`
	if err := os.WriteFile(filepath.Join(chatsDir, "session-structured.json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write gemini fixture: %v", err)
	}
}

func writeStructuredKimiReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	sum := md5.Sum([]byte(filepath.Clean(workDir)))
	workHash := hex.EncodeToString(sum[:])
	path := filepath.Join(root, workHash, sessionKey, "context.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir kimi context dir: %v", err)
	}
	payload := strings.Join([]string{
		`{"role":"assistant","content":[],"tool_calls":[{"type":"function","id":"call-kimi-read","function":{"name":"Read","arguments":"{\"path\":\"README.md\"}"}}]}`,
		`{"role":"tool","content":[{"type":"text","text":"Kimi file data"}],"tool_call_id":"call-kimi-read"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(payload), 0o644); err != nil {
		t.Fatalf("write kimi fixture: %v", err)
	}
}

func writeStructuredOpenCodeEditFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"opencode-structured","directory":%q},
  "messages": [
    {"info":{"id":"opencode-1","sessionID":"opencode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-opencode-edit","tool":"Edit","state":{"status":"completed","input":{"filePath":"README.md","oldString":"old","newString":"new"},"output":"Edited README.md"}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "opencode", "session-structured.json"), body)
}

func writeStructuredMimoCodeBashFixture(t *testing.T, root, workDir, _ string) {
	t.Helper()
	body := fmt.Sprintf(`{
  "info": {"id":"mimocode-structured","directory":%q},
  "messages": [
    {"info":{"id":"mimocode-1","sessionID":"mimocode-structured","role":"assistant","time":{"created":1780272000000}},"parts":[{"id":"part-tool","type":"tool","callID":"call-mimocode-bash","tool":"Bash","state":{"status":"completed","input":{"command":"go test ./..."},"output":{"stdout":"ok ./...","exitCode":0}}}]}
  ]
}`, workDir)
	writeStructuredOpenCodeExport(t, filepath.Join(root, "mimocode", "session-structured.json"), body)
}

func writeStructuredPiReadFixture(t *testing.T, root, workDir, sessionKey string) {
	t.Helper()
	body := fmt.Sprintf(`{"type":"session","version":3,"id":%q,"timestamp":"2026-06-01T00:00:00.000Z","cwd":%q}
{"type":"message","id":"pi-user-1","parentId":null,"timestamp":"2026-06-01T00:00:00.000Z","message":{"role":"user","content":"read the file","timestamp":1780272000000}}
{"type":"message","id":"pi-assistant-1","parentId":"pi-user-1","timestamp":"2026-06-01T00:00:01.000Z","message":{"role":"assistant","content":[{"type":"toolCall","id":"call-pi-read","name":"read","arguments":{"path":"README.md"}}],"timestamp":1780272001000}}
{"type":"message","id":"pi-tool-1","parentId":"pi-assistant-1","timestamp":"2026-06-01T00:00:02.000Z","message":{"role":"toolResult","toolCallId":"call-pi-read","toolName":"read","content":[{"type":"text","text":"Pi file data"}],"isError":false,"timestamp":1780272002000}}
`, sessionKey, workDir)
	path := filepath.Join(root, "pi", sessionKey+".jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir pi fixture dir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write pi fixture: %v", err)
	}
}

func writeStructuredOpenCodeExport(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir opencode export: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write opencode export: %v", err)
	}
}

func writeStructuredAntigravityWriteFixture(t *testing.T, root, _ string, sessionKey string) {
	t.Helper()
	path := filepath.Join(root, sessionKey, ".system_generated", "logs", "transcript.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir antigravity logs: %v", err)
	}
	body := strings.Join([]string{
		`{"step_index":1,"type":"PLANNER_RESPONSE","created_at":"2026-06-01T00:00:00Z","content":"writing","tool_calls":[{"id":"call-antigravity-write","name":"Write","args":{"path":"notes.txt","content":"hello structured world"}}]}`,
		`{"step_index":2,"type":"WRITE_FILE","created_at":"2026-06-01T00:00:01Z","tool_call_id":"call-antigravity-write","content":"wrote notes.txt"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write antigravity fixture: %v", err)
	}
}

func stringSliceContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
