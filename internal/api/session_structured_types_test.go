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

func mustMarshalForStructuredTest(t *testing.T, value any) json.RawMessage {
	t.Helper()
	out, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal structured fixture: %v", err)
	}
	return out
}
