package workertest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	worker "github.com/gastownhall/gascity/internal/worker"
)

func TestStructuredCatalogStaysAligned(t *testing.T) {
	catalog := StructuredCatalog()
	want := []RequirementCode{
		RequirementStructuredToolResult,
		RequirementStructuredNoNativeLeak,
		RequirementStructuredEditEvidence,
	}
	if len(catalog) != len(want) {
		t.Fatalf("catalog entries = %d, want %d", len(catalog), len(want))
	}
	seen := map[RequirementCode]bool{}
	for _, requirement := range catalog {
		if requirement.Group != "structured" {
			t.Fatalf("requirement %s group = %q, want structured", requirement.Code, requirement.Group)
		}
		if requirement.Description == "" {
			t.Fatalf("requirement %s has empty description", requirement.Code)
		}
		seen[requirement.Code] = true
	}
	for _, code := range want {
		if !seen[code] {
			t.Fatalf("catalog missing requirement %s", code)
		}
	}
}

// structuredConformanceProfile pairs a worker profile with a loader that
// materializes a normalized history carrying a structured tool result. Only
// profiles whose provider-native structured fixture exists are listed; the
// remaining canonical profiles (codex, gemini) graduate as their native
// structured fixtures land (tracked in worker-conformance.md).
type structuredConformanceProfile struct {
	profile ProfileID
	load    func(t *testing.T) *worker.HistorySnapshot
}

func structuredConformanceProfiles() []structuredConformanceProfile {
	return []structuredConformanceProfile{
		{profile: ProfileClaudeTmuxCLI, load: loadClaudeStructuredHistory},
	}
}

func TestStructuredConformance(t *testing.T) {
	reporter := NewSuiteReporter(t, "structured", map[string]string{"tier": "worker-core"})

	for _, tc := range structuredConformanceProfiles() {
		tc := tc
		t.Run(string(tc.profile), func(t *testing.T) {
			history := tc.load(t)

			t.Run(string(RequirementStructuredToolResult), func(t *testing.T) {
				reporter.Require(t, StructuredToolResultResult(tc.profile, history))
			})
			t.Run(string(RequirementStructuredNoNativeLeak), func(t *testing.T) {
				reporter.Require(t, StructuredNoNativeLeakResult(tc.profile, history))
			})
			t.Run(string(RequirementStructuredEditEvidence), func(t *testing.T) {
				result := StructuredEditEvidenceResult(tc.profile, history)
				if result.Status == ResultUnsupported {
					reporter.Record(result)
					t.Skip(result.Detail)
					return
				}
				reporter.Require(t, result)
			})
		})
	}
}

// loadClaudeStructuredHistory writes a Claude JSONL transcript containing an
// Edit tool call whose result carries provider-side patch evidence, then
// normalizes it through the real worker adapter.
func loadClaudeStructuredHistory(t *testing.T) *worker.HistorySnapshot {
	t.Helper()
	lines := []string{
		`{"uuid":"u1","type":"user","message":{"role":"user","content":"Edit README.md."},"timestamp":"2026-06-01T00:00:00Z","sessionId":"struct-1"}`,
		`{"uuid":"a1","parentUuid":"u1","type":"assistant","message":{"role":"assistant","id":"m1","model":"claude-sonnet-4-6","content":[{"type":"tool_use","id":"call-edit","name":"Edit","input":{"file_path":"README.md","old_string":"old line","new_string":"new line"}}]},"timestamp":"2026-06-01T00:00:01Z","sessionId":"struct-1"}`,
		`{"uuid":"r1","parentUuid":"a1","type":"tool_result","toolUseID":"call-edit","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-edit","content":"The file README.md has been updated successfully."}]},"toolUseResult":{"filePath":"README.md","oldString":"old line","newString":"new line","originalFile":"export const message = \"old line\";\n","structuredPatch":[{"oldStart":1,"oldLines":1,"newStart":1,"newLines":1,"lines":["-export const message = \"old line\";","+export const message = \"new line\";"]}],"userModified":false,"replaceAll":false},"timestamp":"2026-06-01T00:00:02Z","sessionId":"struct-1"}`,
	}
	path := filepath.Join(t.TempDir(), "session-struct.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	history, err := (worker.SessionLogAdapter{}).LoadHistory(worker.LoadRequest{
		Provider:       "claude",
		TranscriptPath: path,
		GCSessionID:    "struct-1",
	})
	if err != nil {
		t.Fatalf("LoadHistory: %v", err)
	}
	return history
}
