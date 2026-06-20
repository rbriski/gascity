package api

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

func TestInferStructuredToolResultNormalizesPythonExecution(t *testing.T) {
	exitCode := 0
	raw := mustMarshalForStructuredTest(t, struct {
		Code      string `json:"code"`
		Output    string `json:"output"`
		ExitCode  *int   `json:"exitCode"`
		Truncated bool   `json:"truncated"`
		Canceled  bool   `json:"canceled"`
	}{
		Code:      "print('hello')",
		Output:    "hello",
		ExitCode:  &exitCode,
		Truncated: true,
	})
	block := worker.HistoryBlock{
		Kind:    worker.BlockKindToolResult,
		Name:    "python",
		Content: raw,
	}

	got := inferStructuredToolResult(block, structuredToolContext{}, "hello")
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "python" {
		t.Fatalf("Kind = %q, want python", got.Kind)
	}
	if got.Code != "print('hello')" {
		t.Fatalf("Code = %q, want python source", got.Code)
	}
	if got.Stdout != "hello" {
		t.Fatalf("Stdout = %q, want hello", got.Stdout)
	}
	if got.ExitCode == nil || *got.ExitCode != 0 {
		t.Fatalf("ExitCode = %v, want 0", got.ExitCode)
	}
	if !got.Truncated {
		t.Fatal("Truncated = false, want true")
	}
	if got.Interrupted {
		t.Fatal("Interrupted = true, want false")
	}
}

func TestInferStructuredToolResultNormalizesSearchFilenames(t *testing.T) {
	raw := mustMarshalForStructuredTest(t, "cmd/gc/dashboard/web/src/panels/crew.ts:230:logButton\ninternal/api/session_structured_types.go:351:inferStructuredToolResult\n")
	block := worker.HistoryBlock{
		Kind:    worker.BlockKindToolResult,
		Name:    "rg",
		Content: raw,
	}
	context := structuredToolContext{
		Name: "rg",
		Input: &SessionStructuredToolInput{
			Kind:    "search",
			Pattern: "structured",
		},
	}

	got := inferStructuredToolResult(block, context, structuredJSONText(raw))
	if got == nil {
		t.Fatal("inferStructuredToolResult returned nil")
	}
	if got.Kind != "grep" {
		t.Fatalf("Kind = %q, want grep", got.Kind)
	}
	if got.Mode != "pattern" {
		t.Fatalf("Mode = %q, want pattern", got.Mode)
	}
	wantFiles := []string{
		"cmd/gc/dashboard/web/src/panels/crew.ts",
		"internal/api/session_structured_types.go",
	}
	if len(got.Filenames) != len(wantFiles) {
		t.Fatalf("Filenames = %#v, want %#v", got.Filenames, wantFiles)
	}
	for i, want := range wantFiles {
		if got.Filenames[i] != want {
			t.Fatalf("Filenames[%d] = %q, want %q; all = %#v", i, got.Filenames[i], want, got.Filenames)
		}
	}
	if got.NumFiles != 2 {
		t.Fatalf("NumFiles = %d, want 2", got.NumFiles)
	}
	if got.NumLines != 2 {
		t.Fatalf("NumLines = %d, want 2", got.NumLines)
	}
}

func mustMarshalForStructuredTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal structured fixture: %v", err)
	}
	return out
}
