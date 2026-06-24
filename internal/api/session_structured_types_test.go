package api

import (
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/worker"
)

func TestHistorySnapshotStructuredMessagesPreferWorkerCarriedStructuredData(t *testing.T) {
	exitCode := 7
	replaceAll := false
	userModified := false
	snapshot := &worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:         "assistant-1",
			Kind:       "assistant",
			Actor:      worker.ActorAssistant,
			Status:     worker.ResultStatusFinal,
			Model:      "claude-sonnet",
			StopReason: "tool_use",
			Usage: &worker.HistoryUsage{
				InputTokens:         100,
				OutputTokens:        20,
				ReasoningTokens:     7,
				CacheReadTokens:     5,
				CacheCreationTokens: 3,
				ContextWindowTokens: 200000,
				ContextUsedTokens:   108,
				ContextPercent:      1,
			},
			Blocks: []worker.HistoryBlock{{
				Kind:      worker.BlockKindToolUse,
				ToolUseID: "call-1",
				Name:      "exec_command",
				Input: mustMarshalForStructuredTest(t, struct {
					Command string `json:"cmd"`
				}{Command: "cat wrong.txt"}),
				StructuredInput: &worker.StructuredToolInput{
					Kind:     "command",
					Command:  "go test ./internal/api",
					FilePath: "typed-input.txt",
					Language: "text",
					Arguments: []worker.StructuredArgument{{
						Name:  "cwd",
						Value: "/tmp/project",
					}},
				},
			}},
		}, {
			ID:     "tool-1",
			Kind:   "tool",
			Actor:  worker.ActorTool,
			Status: worker.ResultStatusFinal,
			Blocks: []worker.HistoryBlock{{
				Kind:      worker.BlockKindToolResult,
				ToolUseID: "call-1",
				Name:      "exec_command",
				Content:   mustMarshalForStructuredTest(t, "fallback output"),
				StructuredResult: &worker.StructuredToolResult{
					Kind:        "bash",
					Command:     "npm test",
					TaskID:      "shell-123",
					TaskStatus:  "completed",
					Stdout:      "typed stdout",
					Stderr:      "typed stderr",
					ExitCode:    &exitCode,
					StdoutLines: 2,
					StderrLines: 1,
					Timestamp:   "2026-06-01T00:00:02Z",
					Language:    "text",
					FilePaths:   []string{"typed-output.txt"},
					Error: &worker.StructuredToolError{
						Category:   "command_failure",
						Message:    "npm ERR! test failed",
						UserReason: "asked to stop",
					},
					OldString:    "old typed text",
					NewString:    "new typed text",
					OriginalFile: "old typed text\n",
					ReplaceAll:   &replaceAll,
					UserModified: &userModified,
					Counts: []worker.StructuredArgument{{
						Name:  "typed-output.txt",
						Value: "2",
					}},
					ResultItems: []worker.StructuredSearchResultItem{{
						Title:   "Typed result item",
						URL:     "https://example.com/typed",
						Snippet: "Provider-neutral item.",
					}},
					AppliedLimit:      100,
					TotalDurationMs:   1234,
					TotalTokens:       321,
					TotalToolUseCount: 4,
					Questions: []worker.StructuredQuestion{{
						Question:    "Select rollout scope",
						Header:      "Scope",
						MultiSelect: true,
						Options: []worker.StructuredQuestionOption{{
							Label:       "All providers",
							Description: "Validate first-class and graceful providers",
						}},
					}},
				},
			}},
		}},
	}

	messages, ids := historySnapshotStructuredMessages(snapshot, false)
	if len(ids) != 2 || ids[0] != "assistant-1" || ids[1] != "tool-1" {
		t.Fatalf("ids = %#v, want assistant/tool IDs", ids)
	}
	if len(messages) != 2 {
		t.Fatalf("messages = %d, want 2", len(messages))
	}
	if messages[0].Model != "claude-sonnet" || messages[0].StopReason != "tool_use" {
		t.Fatalf("message metadata = model %q stop %q, want claude-sonnet/tool_use", messages[0].Model, messages[0].StopReason)
	}
	if messages[0].Usage == nil || messages[0].Usage.InputTokens != 100 || messages[0].Usage.OutputTokens != 20 || messages[0].Usage.ReasoningTokens != 7 || messages[0].Usage.CacheReadTokens != 5 || messages[0].Usage.CacheCreationTokens != 3 {
		t.Fatalf("message usage = %+v, want typed token usage", messages[0].Usage)
	}
	if messages[0].Usage.ContextWindowTokens != 200000 || messages[0].Usage.ContextUsedTokens != 108 || messages[0].Usage.ContextPercent != 1 {
		t.Fatalf("message context usage = %+v, want context fields", messages[0].Usage)
	}
	input := messages[0].Blocks[0].Input
	if input == nil {
		t.Fatal("tool-use input = nil")
	}
	if input.Command != "go test ./internal/api" || input.FilePath != "typed-input.txt" || input.Language != "text" {
		t.Fatalf("tool-use input = %+v, want worker-carried structured input", input)
	}
	if len(input.Arguments) != 1 || input.Arguments[0].Name != "cwd" || input.Arguments[0].Value != "/tmp/project" {
		t.Fatalf("tool-use arguments = %#v, want converted worker arguments", input.Arguments)
	}

	result := messages[1].Blocks[0].Structured
	if result == nil {
		t.Fatal("tool result structured = nil")
	}
	if result.Stdout != "typed stdout" || result.Stderr != "typed stderr" {
		t.Fatalf("tool result = %+v, want worker-carried stdout/stderr", result)
	}
	if result.Command != "npm test" || result.TaskID != "shell-123" || result.TaskStatus != "completed" {
		t.Fatalf("tool result bash metadata = command %q task %q status %q, want npm test/shell-123/completed", result.Command, result.TaskID, result.TaskStatus)
	}
	if result.StdoutLines != 2 || result.StderrLines != 1 || result.Timestamp != "2026-06-01T00:00:02Z" {
		t.Fatalf("tool result bash lines/timestamp = stdout %d stderr %d timestamp %q, want 2/1/2026-06-01T00:00:02Z", result.StdoutLines, result.StderrLines, result.Timestamp)
	}
	if result.ExitCode == nil || *result.ExitCode != 7 {
		t.Fatalf("tool result exit = %v, want 7", result.ExitCode)
	}
	if result.Error == nil {
		t.Fatal("tool result error = nil, want worker-carried structured error")
	}
	if result.Error.Category != "command_failure" || result.Error.Message != "npm ERR! test failed" || result.Error.UserReason != "asked to stop" {
		t.Fatalf("tool result error = %+v, want worker-carried error classification", result.Error)
	}
	if len(result.FilePaths) != 1 || result.FilePaths[0] != "typed-output.txt" {
		t.Fatalf("tool result file paths = %#v, want typed-output.txt", result.FilePaths)
	}
	if result.Language != "text" {
		t.Fatalf("tool result language = %q, want text", result.Language)
	}
	if result.OldString != "old typed text" || result.NewString != "new typed text" || result.OriginalFile != "old typed text\n" {
		t.Fatalf("tool result edit metadata = old %q new %q original %q, want worker-carried edit context", result.OldString, result.NewString, result.OriginalFile)
	}
	if result.ReplaceAll == nil || *result.ReplaceAll != replaceAll {
		t.Fatalf("tool result replace_all = %v, want explicit false", result.ReplaceAll)
	}
	if result.UserModified == nil || *result.UserModified != userModified {
		t.Fatalf("tool result user_modified = %v, want explicit false", result.UserModified)
	}
	if len(result.Counts) != 1 || result.Counts[0].Name != "typed-output.txt" || result.Counts[0].Value != "2" {
		t.Fatalf("tool result counts = %#v, want typed-output.txt:2", result.Counts)
	}
	if len(result.ResultItems) != 1 || result.ResultItems[0].Title != "Typed result item" || result.ResultItems[0].URL != "https://example.com/typed" || result.ResultItems[0].Snippet != "Provider-neutral item." {
		t.Fatalf("tool result items = %#v, want typed title/url/snippet", result.ResultItems)
	}
	if result.AppliedLimit != 100 {
		t.Fatalf("tool result applied_limit = %d, want 100", result.AppliedLimit)
	}
	if result.TotalDurationMs != 1234 || result.TotalTokens != 321 || result.TotalToolUseCount != 4 {
		t.Fatalf("tool result aggregate metrics = duration %d tokens %d tools %d, want 1234/321/4", result.TotalDurationMs, result.TotalTokens, result.TotalToolUseCount)
	}
	if len(result.Questions) != 1 || result.Questions[0].Question != "Select rollout scope" || result.Questions[0].Header != "Scope" || !result.Questions[0].MultiSelect {
		t.Fatalf("tool result questions = %#v, want typed multi-select question", result.Questions)
	}
	if len(result.Questions[0].Options) != 1 || result.Questions[0].Options[0].Label != "All providers" || result.Questions[0].Options[0].Description != "Validate first-class and graceful providers" {
		t.Fatalf("tool result question options = %#v, want typed label/description", result.Questions[0].Options)
	}
}

func TestHistorySnapshotStructuredMessagesCarriesUserPromptMetadata(t *testing.T) {
	snapshot := &worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:     "user-1",
			Kind:   "user",
			Actor:  worker.ActorUser,
			Status: worker.ResultStatusFinal,
			UserPrompt: &worker.HistoryUserPrompt{
				Text:        "Please inspect this.",
				OpenedFiles: []string{"/tmp/project/src/app.ts"},
				UploadedFiles: []worker.HistoryUploadedFile{{
					OriginalName: "diagram.png",
					Size:         "12 KB",
					MIMEType:     "image/png",
					FilePath:     "/tmp/uploads/diagram.png",
				}},
				Selections: []worker.HistoryUserSelection{{
					Text: "const answer = 42;",
				}},
			},
			Blocks: []worker.HistoryBlock{{
				Kind: worker.BlockKindText,
				Text: "raw prompt text with metadata",
			}},
		}},
	}

	messages, _ := historySnapshotStructuredMessages(snapshot, false)
	if len(messages) != 1 {
		t.Fatalf("messages = %+v, want one message", messages)
	}
	got := messages[0].UserPrompt
	if got == nil {
		t.Fatal("UserPrompt = nil, want projected prompt metadata")
	}
	if got.Text != "Please inspect this." {
		t.Fatalf("prompt text = %q, want cleaned text", got.Text)
	}
	if len(got.OpenedFiles) != 1 || got.OpenedFiles[0] != "/tmp/project/src/app.ts" {
		t.Fatalf("opened files = %#v, want projected file path", got.OpenedFiles)
	}
	if len(got.UploadedFiles) != 1 || got.UploadedFiles[0].OriginalName != "diagram.png" || got.UploadedFiles[0].MIMEType != "image/png" || got.UploadedFiles[0].FilePath != "/tmp/uploads/diagram.png" {
		t.Fatalf("uploaded files = %#v, want projected upload metadata", got.UploadedFiles)
	}
	if len(got.Selections) != 1 || got.Selections[0].Text != "const answer = 42;" {
		t.Fatalf("selections = %#v, want projected IDE selection", got.Selections)
	}
}

func TestHistorySnapshotStructuredMessagesCarriesThinkingSignature(t *testing.T) {
	snapshot := &worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:     "assistant-thinking",
			Kind:   "assistant",
			Actor:  worker.ActorAssistant,
			Status: worker.ResultStatusFinal,
			Blocks: []worker.HistoryBlock{{
				Kind:      worker.BlockKindThinking,
				Text:      "private reasoning",
				Signature: "encrypted",
			}},
		}},
	}

	redacted, _ := historySnapshotStructuredMessages(snapshot, false)
	if len(redacted) != 1 || len(redacted[0].Blocks) != 1 {
		t.Fatalf("redacted messages = %+v, want one thinking block", redacted)
	}
	if redacted[0].Blocks[0].Thinking != "" || redacted[0].Blocks[0].Text != "" {
		t.Fatalf("redacted block leaked thinking text: %+v", redacted[0].Blocks[0])
	}
	if redacted[0].Blocks[0].Signature != "encrypted" {
		t.Fatalf("redacted signature = %q, want encrypted", redacted[0].Blocks[0].Signature)
	}

	included, _ := historySnapshotStructuredMessages(snapshot, true)
	if included[0].Blocks[0].Thinking != "private reasoning" {
		t.Fatalf("included thinking = %q, want private reasoning", included[0].Blocks[0].Thinking)
	}
	if included[0].Blocks[0].Signature != "encrypted" {
		t.Fatalf("included signature = %q, want encrypted", included[0].Blocks[0].Signature)
	}
}

func TestHistorySnapshotStructuredMessagesCarriesImageBlockMetadata(t *testing.T) {
	snapshot := &worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:     "user-image",
			Kind:   "user",
			Actor:  worker.ActorUser,
			Status: worker.ResultStatusFinal,
			Blocks: []worker.HistoryBlock{{
				Kind:     worker.BlockKindImage,
				FilePath: "screens/shot.png",
				ImageURL: "https://example.com/shot.png",
				MIMEType: "image/png",
			}},
		}},
	}

	messages, ids := historySnapshotStructuredMessages(snapshot, false)
	if len(ids) != 1 || ids[0] != "user-image" {
		t.Fatalf("ids = %#v, want user-image", ids)
	}
	if len(messages) != 1 || len(messages[0].Blocks) != 1 {
		t.Fatalf("messages = %+v, want one image block", messages)
	}
	block := messages[0].Blocks[0]
	if block.Type != "image" || block.FilePath != "screens/shot.png" || block.ImageURL != "https://example.com/shot.png" || block.MIMEType != "image/png" {
		t.Fatalf("image block = %+v, want provider-neutral image metadata", block)
	}
}

func TestHistorySnapshotStructuredMessagesDoNotInferProviderNativeFallbacks(t *testing.T) {
	snapshot := &worker.HistorySnapshot{
		Entries: []worker.HistoryEntry{{
			ID:     "assistant-1",
			Kind:   "assistant",
			Actor:  worker.ActorAssistant,
			Status: worker.ResultStatusFinal,
			Blocks: []worker.HistoryBlock{{
				Kind:      worker.BlockKindToolUse,
				ToolUseID: "call-1",
				Name:      "exec_command",
				Input: mustMarshalForStructuredTest(t, struct {
					Command string `json:"cmd"`
				}{Command: "cat provider-native.txt"}),
			}},
		}, {
			ID:     "tool-1",
			Kind:   "tool",
			Actor:  worker.ActorTool,
			Status: worker.ResultStatusFinal,
			Blocks: []worker.HistoryBlock{{
				Kind:      worker.BlockKindToolResult,
				ToolUseID: "call-1",
				Name:      "exec_command",
				Content: mustMarshalForStructuredTest(t, struct {
					ToolUseResult struct {
						Stdout string `json:"stdout"`
					} `json:"toolUseResult"`
				}{
					ToolUseResult: struct {
						Stdout string `json:"stdout"`
					}{Stdout: "native stdout"},
				}),
			}},
		}},
	}

	messages, _ := historySnapshotStructuredMessages(snapshot, false)
	if got := messages[0].Blocks[0].Input; got != nil {
		t.Fatalf("tool input = %+v, want nil without worker-carried structured input", got)
	}
	resultBlock := messages[1].Blocks[0]
	if resultBlock.Structured != nil {
		t.Fatalf("structured result = %+v, want nil without worker-carried structured result", resultBlock.Structured)
	}
	if resultBlock.Content != "" {
		t.Fatalf("content = %q, want empty string for provider-native object without generic content/text", resultBlock.Content)
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
