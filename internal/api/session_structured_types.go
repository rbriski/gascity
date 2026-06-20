package api

import (
	"bytes"
	"encoding/json"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/sessionlog"
	"github.com/gastownhall/gascity/internal/worker"
)

const sessionStructuredSchemaVersion = "session.structured.v1"

// SessionStreamStructuredMessageEvent carries provider-normalized structured
// transcript messages on the session SSE stream.
type SessionStreamStructuredMessageEvent struct {
	ID                 string                     `json:"id"`
	Template           string                     `json:"template"`
	Provider           string                     `json:"provider" doc:"Producing provider identifier (claude, codex, gemini, open-code, etc.)."`
	Format             string                     `json:"format" doc:"Always structured for this event."`
	SchemaVersion      string                     `json:"schema_version" doc:"Structured session transcript schema version."`
	History            *SessionStructuredHistory  `json:"history,omitempty" doc:"Normalized worker-history envelope for this snapshot or stream batch."`
	StructuredMessages []SessionStructuredMessage `json:"structured_messages" doc:"Provider-normalized structured messages."`
	Pagination         *sessionlog.PaginationInfo `json:"pagination,omitempty"`
}

// SessionStructuredHistory is the normalized worker-history envelope projected
// onto the session transcript API.
type SessionStructuredHistory struct {
	GCSessionID           string                        `json:"gc_session_id,omitempty"`
	LogicalConversationID string                        `json:"logical_conversation_id,omitempty"`
	ProviderSessionID     string                        `json:"provider_session_id,omitempty"`
	TranscriptStreamID    string                        `json:"transcript_stream_id"`
	Generation            SessionStructuredGeneration   `json:"generation"`
	Cursor                SessionStructuredCursor       `json:"cursor"`
	Continuity            SessionStructuredContinuity   `json:"continuity"`
	TailState             SessionStructuredTailState    `json:"tail_state"`
	Diagnostics           []SessionStructuredDiagnostic `json:"diagnostics,omitempty"`
}

// SessionStructuredGeneration identifies a raw transcript stream instance.
type SessionStructuredGeneration struct {
	ID         string `json:"id"`
	ObservedAt string `json:"observed_at,omitempty"`
}

// SessionStructuredCursor identifies the normalized transcript tip.
type SessionStructuredCursor struct {
	AfterEntryID string `json:"after_entry_id,omitempty"`
}

// SessionStructuredContinuity describes compaction/branch evidence.
type SessionStructuredContinuity struct {
	Status          string `json:"status"`
	CompactionCount int    `json:"compaction_count,omitempty"`
	HasBranches     bool   `json:"has_branches,omitempty"`
	Note            string `json:"note,omitempty"`
}

// SessionStructuredTailState captures the current transcript tail state.
type SessionStructuredTailState struct {
	Activity              string   `json:"activity"`
	LastEntryID           string   `json:"last_entry_id,omitempty"`
	OpenToolCallIDs       []string `json:"open_tool_call_ids,omitempty"`
	PendingInteractionIDs []string `json:"pending_interaction_ids,omitempty"`
	Degraded              bool     `json:"degraded,omitempty"`
	DegradedReason        string   `json:"degraded_reason,omitempty"`
}

// SessionStructuredDiagnostic records normalized-history diagnostics.
type SessionStructuredDiagnostic struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
	Count   int    `json:"count,omitempty"`
}

// SessionStructuredMessage is one provider-normalized transcript message.
type SessionStructuredMessage struct {
	ID               string                   `json:"id"`
	Role             string                   `json:"role"`
	Provider         string                   `json:"provider,omitempty"`
	Timestamp        string                   `json:"timestamp,omitempty"`
	Model            string                   `json:"model,omitempty"`
	StopReason       string                   `json:"stop_reason,omitempty"`
	Status           string                   `json:"status"`
	IsSubagent       bool                     `json:"is_subagent,omitempty"`
	ParentToolCallID string                   `json:"parent_tool_call_id,omitempty"`
	Blocks           []SessionStructuredBlock `json:"blocks"`
}

// SessionStructuredBlock is one structured content/tool/interaction block.
type SessionStructuredBlock struct {
	Type        string                        `json:"type"`
	Text        string                        `json:"text,omitempty"`
	Thinking    string                        `json:"thinking,omitempty"`
	Signature   string                        `json:"signature,omitempty"`
	ID          string                        `json:"id,omitempty"`
	ToolCallID  string                        `json:"tool_call_id,omitempty"`
	Name        string                        `json:"name,omitempty"`
	Input       *SessionStructuredToolInput   `json:"input,omitempty"`
	Content     string                        `json:"content,omitempty"`
	IsError     bool                          `json:"is_error,omitempty"`
	Structured  *SessionStructuredToolResult  `json:"structured,omitempty"`
	Interaction *SessionStructuredInteraction `json:"interaction,omitempty"`
}

// SessionStructuredToolInput is a provider-neutral projection of a tool call's
// input. Provider-native input JSON is available only through format=raw.
type SessionStructuredToolInput struct {
	Kind      string                      `json:"kind,omitempty" doc:"Provider-neutral input kind such as command, code, patch, search, file, arguments, or text."`
	Text      string                      `json:"text,omitempty"`
	Command   string                      `json:"command,omitempty"`
	Code      string                      `json:"code,omitempty"`
	Patch     string                      `json:"patch,omitempty"`
	FilePath  string                      `json:"file_path,omitempty"`
	Query     string                      `json:"query,omitempty"`
	Pattern   string                      `json:"pattern,omitempty"`
	Arguments []SessionStructuredArgument `json:"arguments,omitempty"`
}

// SessionStructuredArgument is one provider-neutral string argument.
type SessionStructuredArgument struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// SessionStructuredToolResult is a typed structured tool-result projection.
// The Kind field discriminates which fields are populated.
type SessionStructuredToolResult struct {
	Kind        string   `json:"kind"`
	Text        string   `json:"text,omitempty"`
	Stdout      string   `json:"stdout,omitempty"`
	Stderr      string   `json:"stderr,omitempty"`
	ExitCode    *int     `json:"exit_code,omitempty"`
	Interrupted bool     `json:"interrupted,omitempty"`
	Truncated   bool     `json:"truncated,omitempty"`
	IsImage     bool     `json:"is_image,omitempty"`
	Mode        string   `json:"mode,omitempty"`
	Filenames   []string `json:"filenames,omitempty"`
	NumFiles    int      `json:"num_files,omitempty"`
	Content     string   `json:"content,omitempty"`
	NumLines    int      `json:"num_lines,omitempty"`
	FilePath    string   `json:"file_path,omitempty"`
	Code        string   `json:"code,omitempty"`
	Patch       string   `json:"patch,omitempty"`
	StartLine   int      `json:"start_line,omitempty"`
	TotalLines  int      `json:"total_lines,omitempty"`
}

// SessionStructuredInteraction is a provider-neutral required interaction
// embedded in normalized history.
type SessionStructuredInteraction struct {
	RequestID string   `json:"request_id,omitempty"`
	Kind      string   `json:"kind,omitempty"`
	State     string   `json:"state"`
	Prompt    string   `json:"prompt,omitempty"`
	Options   []string `json:"options,omitempty"`
	Action    string   `json:"action,omitempty"`
}

type structuredToolContext struct {
	Name  string
	Input *SessionStructuredToolInput
}

func structuredHistoryFromSnapshot(snapshot *worker.HistorySnapshot) *SessionStructuredHistory {
	if snapshot == nil {
		return nil
	}
	diagnostics := make([]SessionStructuredDiagnostic, 0, len(snapshot.Diagnostics))
	for _, d := range snapshot.Diagnostics {
		diagnostics = append(diagnostics, SessionStructuredDiagnostic{
			Code:    d.Code,
			Message: d.Message,
			Count:   d.Count,
		})
	}
	return &SessionStructuredHistory{
		GCSessionID:           snapshot.GCSessionID,
		LogicalConversationID: snapshot.LogicalConversationID,
		ProviderSessionID:     snapshot.ProviderSessionID,
		TranscriptStreamID:    snapshot.TranscriptStreamID,
		Generation: SessionStructuredGeneration{
			ID:         snapshot.Generation.ID,
			ObservedAt: formatOptionalTime(snapshot.Generation.ObservedAt),
		},
		Cursor: SessionStructuredCursor{
			AfterEntryID: snapshot.Cursor.AfterEntryID,
		},
		Continuity: SessionStructuredContinuity{
			Status:          string(snapshot.Continuity.Status),
			CompactionCount: snapshot.Continuity.CompactionCount,
			HasBranches:     snapshot.Continuity.HasBranches,
			Note:            snapshot.Continuity.Note,
		},
		TailState: SessionStructuredTailState{
			Activity:              string(snapshot.TailState.Activity),
			LastEntryID:           snapshot.TailState.LastEntryID,
			OpenToolCallIDs:       append([]string(nil), snapshot.TailState.OpenToolUseIDs...),
			PendingInteractionIDs: append([]string(nil), snapshot.TailState.PendingInteractionIDs...),
			Degraded:              snapshot.TailState.Degraded,
			DegradedReason:        snapshot.TailState.DegradedReason,
		},
		Diagnostics: diagnostics,
	}
}

func historySnapshotStructuredMessages(snapshot *worker.HistorySnapshot, includeThinking bool) ([]SessionStructuredMessage, []string) {
	if snapshot == nil {
		return nil, nil
	}
	toolContexts := structuredToolContexts(snapshot.Entries)
	messages := make([]SessionStructuredMessage, 0, len(snapshot.Entries))
	ids := make([]string, 0, len(snapshot.Entries))
	for _, entry := range snapshot.Entries {
		msg := historyEntryToStructuredMessage(entry, includeThinking, toolContexts)
		if len(msg.Blocks) == 0 && msg.Role == "" {
			continue
		}
		messages = append(messages, msg)
		ids = append(ids, entry.ID)
	}
	return messages, ids
}

func structuredToolContexts(entries []worker.HistoryEntry) map[string]structuredToolContext {
	out := make(map[string]structuredToolContext)
	for _, entry := range entries {
		for _, block := range entry.Blocks {
			if block.Kind != worker.BlockKindToolUse || strings.TrimSpace(block.ToolUseID) == "" {
				continue
			}
			out[block.ToolUseID] = structuredToolContext{
				Name:  block.Name,
				Input: normalizeStructuredToolInput(block.Name, block.Input),
			}
		}
	}
	return out
}

func historyEntryToStructuredMessage(entry worker.HistoryEntry, includeThinking bool, toolContexts map[string]structuredToolContext) SessionStructuredMessage {
	role := entry.Kind
	if role == "" {
		role = string(entry.Actor)
	}
	msg := SessionStructuredMessage{
		ID:       entry.ID,
		Role:     role,
		Provider: entry.Provenance.Provider,
		Status:   string(entry.Status),
		Blocks:   make([]SessionStructuredBlock, 0, len(entry.Blocks)),
	}
	if entry.Timestamp != nil {
		msg.Timestamp = entry.Timestamp.Format(time.RFC3339Nano)
	}
	for _, block := range entry.Blocks {
		if structured := historyBlockToStructuredBlock(block, includeThinking, toolContexts); structured != nil {
			msg.Blocks = append(msg.Blocks, *structured)
		}
	}
	return msg
}

func historyBlockToStructuredBlock(block worker.HistoryBlock, includeThinking bool, toolContexts map[string]structuredToolContext) *SessionStructuredBlock {
	out := &SessionStructuredBlock{
		Type:       string(block.Kind),
		Text:       block.Text,
		ToolCallID: block.ToolUseID,
		Name:       block.Name,
		Input:      normalizeStructuredToolInput(block.Name, block.Input),
		Content:    structuredJSONText(block.Content),
		IsError:    block.IsError,
	}
	switch block.Kind {
	case worker.BlockKindThinking:
		out.Text = ""
		if includeThinking {
			out.Thinking = block.Text
		}
	case worker.BlockKindToolUse:
		out.ID = block.ToolUseID
	case worker.BlockKindToolResult:
		if out.Content == "" {
			out.Content = block.Text
		}
		context := toolContexts[block.ToolUseID]
		out.Structured = inferStructuredToolResult(block, context, out.Content)
	case worker.BlockKindInteraction:
		out.Interaction = structuredInteraction(block.Interaction)
	}
	return out
}

func normalizeStructuredToolInput(name string, raw json.RawMessage) *SessionStructuredToolInput {
	if len(raw) == 0 {
		return nil
	}
	text := structuredJSONText(raw)
	out := &SessionStructuredToolInput{}
	lowerName := strings.ToLower(strings.TrimSpace(name))
	if lowerName == "apply_patch" || looksLikePatch(text) {
		out.Kind = "patch"
		out.Patch = text
		out.FilePath = patchFilePath(text)
		return out
	}

	for _, field := range structuredJSONFields(raw) {
		switch normalizeStructuredFieldName(field.Name) {
		case "command":
			out.Command = firstNonEmptyString(out.Command, field.Value)
		case "code":
			out.Code = firstNonEmptyString(out.Code, field.Value)
		case "file_path":
			out.FilePath = firstNonEmptyString(out.FilePath, field.Value)
		case "query":
			out.Query = firstNonEmptyString(out.Query, field.Value)
		case "pattern":
			out.Pattern = firstNonEmptyString(out.Pattern, field.Value)
		default:
			out.Arguments = append(out.Arguments, field)
		}
	}

	switch {
	case out.Command != "":
		out.Kind = "command"
	case out.Code != "":
		out.Kind = "code"
	case out.Patch != "":
		out.Kind = "patch"
	case out.FilePath != "":
		out.Kind = "file"
	case out.Query != "" || out.Pattern != "":
		out.Kind = "search"
	case len(out.Arguments) > 0:
		out.Kind = "arguments"
	case text != "":
		out.Kind = "text"
		out.Text = text
	}
	if out.Kind == "" {
		return nil
	}
	return out
}

func inferStructuredToolResult(block worker.HistoryBlock, context structuredToolContext, content string) *SessionStructuredToolResult {
	if content == "" {
		return nil
	}
	name := strings.ToLower(strings.TrimSpace(firstNonEmptyString(block.Name, context.Name)))
	if looksLikePatch(content) || name == "apply_patch" || (context.Input != nil && context.Input.Kind == "patch") {
		return &SessionStructuredToolResult{
			Kind:     "edit",
			FilePath: firstNonEmptyString(patchFilePath(content), inputFilePath(context.Input)),
			Patch:    patchContent(content),
			Content:  content,
		}
	}
	if isPythonTool(name, context.Input) {
		stdout, stderr, exitCode, interrupted, truncated, isImage := commandResultFields(block.Content, content)
		return &SessionStructuredToolResult{
			Kind:        "python",
			Text:        content,
			Code:        firstNonEmptyString(inputCode(context.Input), resultCode(block.Content)),
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			Interrupted: interrupted,
			Truncated:   truncated,
			IsImage:     isImage,
		}
	}
	if isCommandTool(name, context.Input) {
		stdout, stderr, exitCode, interrupted, truncated, isImage := commandResultFields(block.Content, content)
		return &SessionStructuredToolResult{
			Kind:        "bash",
			Text:        content,
			Stdout:      stdout,
			Stderr:      stderr,
			ExitCode:    exitCode,
			Interrupted: interrupted,
			Truncated:   truncated,
			IsImage:     isImage,
		}
	}
	if isReadTool(name, context.Input) {
		return &SessionStructuredToolResult{
			Kind:     "read",
			FilePath: inputFilePath(context.Input),
			Content:  content,
			NumLines: countLines(content),
		}
	}
	if isSearchTool(name, context.Input) {
		return &SessionStructuredToolResult{
			Kind:     "grep",
			Mode:     searchMode(context.Input),
			Content:  content,
			NumLines: countLines(content),
		}
	}
	return &SessionStructuredToolResult{
		Kind:    "text",
		Text:    content,
		Content: content,
	}
}

func structuredInteraction(in *worker.HistoryInteraction) *SessionStructuredInteraction {
	if in == nil {
		return nil
	}
	return &SessionStructuredInteraction{
		RequestID: in.RequestID,
		Kind:      in.Kind,
		State:     string(in.State),
		Prompt:    in.Prompt,
		Options:   append([]string(nil), in.Options...),
		Action:    in.Action,
	}
}

func formatOptionalTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(time.RFC3339Nano)
}

func structuredJSONText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var textBlocks []struct {
		Text    string `json:"text"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &textBlocks); err == nil {
		parts := make([]string, 0, len(textBlocks))
		for _, block := range textBlocks {
			switch {
			case block.Text != "":
				parts = append(parts, block.Text)
			case block.Content != "":
				parts = append(parts, block.Content)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	var object struct {
		Output  string `json:"output"`
		Stdout  string `json:"stdout"`
		Stderr  string `json:"stderr"`
		Text    string `json:"text"`
		Content string `json:"content"`
		Error   string `json:"error"`
		Result  string `json:"result"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return strings.Join(nonEmptyStrings(
			object.Output,
			object.Stdout,
			object.Stderr,
			object.Text,
			object.Content,
			object.Error,
			object.Result,
		), "\n")
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return buf.String()
	}
	return string(raw)
}

func structuredJSONFields(raw json.RawMessage) []SessionStructuredArgument {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || len(object) == 0 {
		return nil
	}
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	fields := make([]SessionStructuredArgument, 0, len(keys))
	for _, key := range keys {
		fields = append(fields, SessionStructuredArgument{
			Name:  key,
			Value: structuredJSONText(object[key]),
		})
	}
	return fields
}

func normalizeStructuredFieldName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "cmd", "command", "shell_command":
		return "command"
	case "code", "python", "script":
		return "code"
	case "file", "file_path", "filepath", "path":
		return "file_path"
	case "q", "query", "search_query":
		return "query"
	case "pattern", "regexp", "regex":
		return "pattern"
	default:
		return name
	}
}

func looksLikePatch(text string) bool {
	return strings.Contains(text, "*** Begin Patch") || strings.Contains(text, "\n@@")
}

func patchFilePath(patch string) string {
	for _, line := range strings.Split(patch, "\n") {
		line = strings.TrimSpace(line)
		for _, prefix := range []string{"*** Update File: ", "*** Add File: ", "*** Delete File: "} {
			if strings.HasPrefix(line, prefix) {
				return strings.TrimSpace(strings.TrimPrefix(line, prefix))
			}
		}
	}
	return ""
}

func patchContent(content string) string {
	if looksLikePatch(content) {
		return content
	}
	return ""
}

func inputFilePath(input *SessionStructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.FilePath
}

func inputCode(input *SessionStructuredToolInput) string {
	if input == nil {
		return ""
	}
	return input.Code
}

func resultCode(raw json.RawMessage) string {
	var object struct {
		Code   string `json:"code"`
		Script string `json:"script"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &object) == nil {
		return firstNonEmptyString(object.Code, object.Script)
	}
	return ""
}

func isPythonTool(name string, input *SessionStructuredToolInput) bool {
	if input != nil && input.Kind == "code" {
		return true
	}
	switch name {
	case "python", "python_execution", "pythonexecution":
		return true
	default:
		return false
	}
}

func isCommandTool(name string, input *SessionStructuredToolInput) bool {
	if input != nil && input.Kind == "command" {
		return true
	}
	switch name {
	case "bash", "shell", "sh", "run_command", "exec_command", "terminal", "terminal.exec":
		return true
	default:
		return false
	}
}

func isReadTool(name string, input *SessionStructuredToolInput) bool {
	if input != nil && input.Kind == "file" {
		return true
	}
	switch name {
	case "read", "read_file", "view", "cat", "open_file":
		return true
	default:
		return false
	}
}

func isSearchTool(name string, input *SessionStructuredToolInput) bool {
	if input != nil && input.Kind == "search" {
		return true
	}
	return strings.Contains(name, "grep") ||
		strings.Contains(name, "search") ||
		name == "rg"
}

func searchMode(input *SessionStructuredToolInput) string {
	if input == nil {
		return ""
	}
	if input.Pattern != "" {
		return "pattern"
	}
	if input.Query != "" {
		return "query"
	}
	return ""
}

func commandResultFields(raw json.RawMessage, content string) (stdout string, stderr string, exitCode *int, interrupted bool, truncated bool, isImage bool) {
	var object struct {
		Output        string `json:"output"`
		Stdout        string `json:"stdout"`
		Stderr        string `json:"stderr"`
		ExitCode      *int   `json:"exit_code"`
		ExitCodeCamel *int   `json:"exitCode"`
		Interrupted   bool   `json:"interrupted"`
		Canceled      bool   `json:"canceled"`
		Truncated     bool   `json:"truncated"`
		IsImage       bool   `json:"is_image"`
		IsImageCamel  bool   `json:"isImage"`
	}
	if len(raw) > 0 && json.Unmarshal(raw, &object) == nil {
		stdout = firstNonEmptyString(object.Stdout, object.Output)
		stderr = object.Stderr
		exitCode = object.ExitCode
		if exitCode == nil {
			exitCode = object.ExitCodeCamel
		}
		interrupted = object.Interrupted || object.Canceled || structuredJSONBool(raw, "cancel"+"led")
		truncated = object.Truncated
		isImage = object.IsImage || object.IsImageCamel
	}
	if stdout == "" && stderr == "" {
		stdout = content
	}
	return stdout, stderr, exitCode, interrupted, truncated, isImage
}

func structuredJSONBool(raw json.RawMessage, field string) bool {
	var object map[string]bool
	if len(raw) == 0 || json.Unmarshal(raw, &object) != nil {
		return false
	}
	return object[field]
}

func countLines(content string) int {
	if content == "" {
		return 0
	}
	return strings.Count(content, "\n") + 1
}

func nonEmptyStrings(values ...string) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			out = append(out, value)
		}
	}
	return out
}
