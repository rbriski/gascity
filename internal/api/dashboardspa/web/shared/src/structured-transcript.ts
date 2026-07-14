// Structured session transcript wire types (`session.structured.v1`) plus the
// shape guards and pure render helpers the dashboard needs to consume PR
// #3718's `format=structured` stream.
//
// These mirror `internal/api/session_structured_types.go` exactly. The
// gc-supervisor-client is vendored without in-tree codegen and does NOT carry
// the structured-message types, so — following the `pending.ts` precedent —
// the dashboard owns these wire shapes and translates at the edge. When the
// external client re-vendors against #3718's OpenAPI these become generated.
//
// Optionality tracks the Go `omitempty` tags: a field WITHOUT omitempty is
// always present (required here); a field WITH omitempty is optional. The Go
// pointer fields (`*int` exit_code, `*bool` replace_all/user_modified) carry
// tri-state semantics — model as optional and check presence, not truthiness.

/** The structured transcript schema version emitted on the wire. */
export const STRUCTURED_SCHEMA_VERSION = 'session.structured.v1';

/**
 * Diagnostic code the server attaches when the provider transcript is
 * unavailable and it falls back to provider-neutral text.
 */
export const STRUCTURED_TRANSCRIPT_UNAVAILABLE_CODE = 'transcript_unavailable';

/** Known block discriminators; the wire is an open string, so callers default-case unknowns. */
export type StructuredBlockType =
  | 'text'
  | 'thinking'
  | 'tool_use'
  | 'tool_result'
  | 'interaction'
  | 'image'
  | 'unknown'
  | string;

/** Known tool-input kinds (open string upstream). */
export type StructuredToolInputKind =
  | 'command'
  | 'code'
  | 'patch'
  | 'glob'
  | 'fetch'
  | 'search'
  | 'file'
  | 'arguments'
  | 'text'
  | string;

/** Known typed tool-result kinds (open string upstream). */
export type StructuredToolResultKind =
  | 'read'
  | 'edit'
  | 'bash'
  | 'python'
  | 'grep'
  | 'search'
  | 'glob'
  | 'fetch'
  | 'write'
  | 'todo'
  | 'plan'
  | 'question'
  | 'stdin'
  | 'task'
  | string;

// ---------------------------------------------------------------------------
// Top-level envelope (SSE structured frame == REST structured transcript body).
// ---------------------------------------------------------------------------

/**
 * The structured transcript envelope. Emitted both as the SSE `structured`
 * frame (`SessionStreamStructuredMessageEvent`) and as the REST
 * `GET …/transcript?format=structured` body — the fields are identical.
 */
export interface SessionStreamStructuredMessageEvent {
  id: string;
  template: string;
  /** Producing provider identifier (claude, codex, gemini, opencode, …). */
  provider: string;
  /** Always `"structured"` for this envelope. */
  format: string;
  schema_version: string;
  history?: SessionStructuredHistory;
  /** Always present (may be empty). */
  structured_messages: SessionStructuredMessage[];
  pagination?: SessionStructuredPagination;
}

/** REST `…/transcript?format=structured` response — same shape as the SSE frame. */
export type SessionStructuredTranscriptResponse = SessionStreamStructuredMessageEvent;

/** Pagination envelope (mirrors `sessionlog.PaginationInfo`). */
export interface SessionStructuredPagination {
  has_older_messages: boolean;
  total_message_count: number;
  returned_message_count: number;
  truncated_before_message?: string;
  total_compactions: number;
}

// ---------------------------------------------------------------------------
// Normalized worker-history envelope.
// ---------------------------------------------------------------------------

export interface SessionStructuredHistory {
  gc_session_id?: string;
  logical_conversation_id?: string;
  provider_session_id?: string;
  transcript_stream_id: string;
  generation: SessionStructuredGeneration;
  cursor: SessionStructuredCursor;
  continuity: SessionStructuredContinuity;
  tail_state: SessionStructuredTailState;
  diagnostics?: SessionStructuredDiagnostic[];
}

export interface SessionStructuredGeneration {
  id: string;
  /** RFC3339Nano timestamp. */
  observed_at?: string;
}

export interface SessionStructuredCursor {
  after_entry_id?: string;
}

export interface SessionStructuredContinuity {
  /** `unknown` | `continuous` | `compacted` | `degraded`. */
  status: string;
  compaction_count?: number;
  has_branches?: boolean;
  note?: string;
}

export interface SessionStructuredTailState {
  /** `unknown` | `idle` | `in_turn`. */
  activity: string;
  last_entry_id?: string;
  open_tool_call_ids?: string[];
  pending_interaction_ids?: string[];
  degraded?: boolean;
  degraded_reason?: string;
}

export interface SessionStructuredDiagnostic {
  code: string;
  message?: string;
  count?: number;
}

// ---------------------------------------------------------------------------
// Messages and blocks.
// ---------------------------------------------------------------------------

export interface SessionStructuredMessage {
  id: string;
  /** Open string: `assistant` | `user` | `tool` | `system` | …. */
  role: string;
  provider?: string;
  /** RFC3339Nano timestamp. */
  timestamp?: string;
  model?: string;
  stop_reason?: string;
  usage?: SessionStructuredUsage;
  user_prompt?: SessionStructuredUserPrompt;
  system_event?: SessionStructuredSystemEvent;
  /** `unknown` | `final` | `partial` | `superseded` | `degraded`. */
  status: string;
  is_subagent?: boolean;
  parent_tool_call_id?: string;
  /** Always present (may be empty). */
  blocks: SessionStructuredBlock[];
}

export interface SessionStructuredSystemEvent {
  kind?: string;
  category?: string;
  code?: string;
  message?: string;
}

export interface SessionStructuredUserPrompt {
  text?: string;
  opened_files?: string[];
  uploaded_files?: SessionStructuredUploadedFile[];
  selections?: SessionStructuredIDESelection[];
}

export interface SessionStructuredUploadedFile {
  original_name?: string;
  /** Human-readable size, e.g. `"12 KB"`. */
  size?: string;
  mime_type?: string;
  file_path?: string;
  preview_url?: string;
}

export interface SessionStructuredIDESelection {
  text?: string;
}

export interface SessionStructuredUsage {
  input_tokens?: number;
  output_tokens?: number;
  reasoning_tokens?: number;
  cache_read_tokens?: number;
  cache_creation_tokens?: number;
  context_window_tokens?: number;
  context_used_tokens?: number;
  context_percent?: number;
}

export interface SessionStructuredBlock {
  type: StructuredBlockType;
  text?: string;
  /** Present only when includeThinking is set. */
  thinking?: string;
  /** Persists even when the thinking text is redacted. */
  signature?: string;
  id?: string;
  tool_call_id?: string;
  name?: string;
  file_path?: string;
  image_url?: string;
  mime_type?: string;
  input?: SessionStructuredToolInput;
  content?: string;
  is_error?: boolean;
  structured?: SessionStructuredToolResult;
  interaction?: SessionStructuredInteraction;
}

// ---------------------------------------------------------------------------
// Tool input / result projections.
// ---------------------------------------------------------------------------

export interface SessionStructuredToolInput {
  kind?: StructuredToolInputKind;
  text?: string;
  command?: string;
  linked_command?: string;
  code?: string;
  patch?: string;
  file_path?: string;
  language?: string;
  url?: string;
  prompt?: string;
  task_id?: string;
  task_type?: string;
  task_status?: string;
  description?: string;
  question?: string;
  options?: string[];
  query?: string;
  pattern?: string;
  plan?: string;
  explanation?: string;
  steps?: SessionStructuredPlanStep[];
  todos?: SessionStructuredTodoItem[];
  arguments?: SessionStructuredArgument[];
}

export interface SessionStructuredArgument {
  name: string;
  value: string;
}

export interface SessionStructuredPlanStep {
  step?: string;
  status?: string;
}

export interface SessionStructuredToolResult {
  kind: StructuredToolResultKind;
  text?: string;
  command?: string;
  stdout?: string;
  stderr?: string;
  /** `*int` upstream — presence is meaningful (absent vs explicit 0). */
  exit_code?: number;
  interrupted?: boolean;
  truncated?: boolean;
  is_image?: boolean;
  mode?: string;
  query?: string;
  url?: string;
  task_id?: string;
  task_type?: string;
  task_status?: string;
  description?: string;
  total_duration_ms?: number;
  total_tokens?: number;
  total_tool_use_count?: number;
  output?: string;
  question?: string;
  questions?: SessionStructuredQuestion[];
  answer?: string;
  options?: string[];
  answers?: SessionStructuredArgument[];
  counts?: SessionStructuredArgument[];
  status_code?: number;
  status_text?: string;
  bytes?: number;
  filenames?: string[];
  num_files?: number;
  num_results?: number;
  duration_ms?: number;
  applied_limit?: number;
  stdout_lines?: number;
  stderr_lines?: number;
  timestamp?: string;
  result_items?: SessionStructuredSearchResultItem[];
  content?: string;
  num_lines?: number;
  file_path?: string;
  file_paths?: string[];
  language?: string;
  code?: string;
  plan?: string;
  explanation?: string;
  steps?: SessionStructuredPlanStep[];
  patch?: string;
  patch_hunks?: SessionStructuredPatchHunk[];
  old_string?: string;
  new_string?: string;
  original_file?: string;
  /** `*bool` upstream — presence is meaningful. */
  replace_all?: boolean;
  /** `*bool` upstream — presence is meaningful. */
  user_modified?: boolean;
  old_todos?: SessionStructuredTodoItem[];
  new_todos?: SessionStructuredTodoItem[];
  start_line?: number;
  total_lines?: number;
  error?: SessionStructuredToolError;
}

export interface SessionStructuredToolError {
  /**
   * `user_rejection` | `user_rejection_with_reason` | `command_failure` |
   * `file_error` | `validation_error` | `timeout` | `network_error` | `unknown`.
   */
  category?: string;
  message?: string;
  user_reason?: string;
}

export interface SessionStructuredPatchHunk {
  file_path?: string;
  old_start?: number;
  old_lines?: number;
  new_start?: number;
  new_lines?: number;
  lines?: string[];
}

export interface SessionStructuredSearchResultItem {
  title?: string;
  url?: string;
  snippet?: string;
}

export interface SessionStructuredQuestionOption {
  label?: string;
  description?: string;
}

export interface SessionStructuredQuestion {
  question?: string;
  header?: string;
  options?: SessionStructuredQuestionOption[];
  multi_select?: boolean;
}

export interface SessionStructuredTodoItem {
  id?: string;
  content?: string;
  status?: string;
  active_form?: string;
  priority?: string;
}

export interface SessionStructuredInteraction {
  request_id?: string;
  kind?: string;
  /** `unknown` | `opened` | `pending` | `resolved` | `dismissed` | `resumed_after_restart`. */
  state: string;
  prompt?: string;
  options?: string[];
  action?: string;
}

// ---------------------------------------------------------------------------
// Non-message stream frames consumed alongside structured frames. Dashboard-
// owned (the `pending.ts` precedent); the pending frame reuses pending.ts.
// ---------------------------------------------------------------------------

/** SSE `activity` frame: `idle` | `in-turn` (worker may also emit `unknown`). */
export interface SessionActivityEvent {
  activity: string;
}

/** SSE `heartbeat` keepalive frame. */
export interface SessionHeartbeatEvent {
  timestamp: string;
}

// ---------------------------------------------------------------------------
// Shape guards. These reproduce the old dashboard's sse.ts/crew.ts guards'
// accept/reject behavior for real wire frames: shallow envelope discriminators
// that trust the server contract for the remaining fields. One intentional
// hardening: `isRecord` excludes arrays (matching this dashboard's pending.ts
// convention), so an array supplied where an object is expected is rejected.
// The server never sends arrays for these fields, so real traffic is unchanged.
// ---------------------------------------------------------------------------

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === 'object' && value !== null && !Array.isArray(value);
}

/** True for a `structured` SSE frame / structured transcript body. */
export function isSessionStructuredEvent(
  data: unknown,
): data is SessionStreamStructuredMessageEvent {
  return isRecord(data) && data.format === 'structured' && Array.isArray(data.structured_messages);
}

/** True for an `activity` SSE frame. */
export function isSessionActivityEvent(data: unknown): data is SessionActivityEvent {
  return isRecord(data) && typeof data.activity === 'string';
}

/** True for a `heartbeat` SSE frame. */
export function isSessionHeartbeatEvent(data: unknown): data is SessionHeartbeatEvent {
  return isRecord(data) && typeof data.timestamp === 'string';
}

/**
 * True for a renderable history envelope — requires the load-bearing nested
 * fields the renderer reads (the old `isSessionStructuredHistory`, with the
 * array-excluding `isRecord` above).
 */
export function isSessionStructuredHistory(value: unknown): value is SessionStructuredHistory {
  if (!isRecord(value)) return false;
  if (typeof value.transcript_stream_id !== 'string') return false;
  const generation = value.generation;
  if (!isRecord(generation) || typeof generation.id !== 'string') return false;
  if (!isRecord(value.cursor)) return false;
  const continuity = value.continuity;
  if (!isRecord(continuity) || typeof continuity.status !== 'string') return false;
  const tailState = value.tail_state;
  if (!isRecord(tailState) || typeof tailState.activity !== 'string') return false;
  return true;
}

/** True for a structured message — requires the `blocks` array (matches old `isStructuredMessage`). */
export function isStructuredMessage(value: unknown): value is SessionStructuredMessage {
  return isRecord(value) && Array.isArray(value.blocks);
}

/**
 * Extract the renderable structured messages from an envelope, dropping any
 * element that is not a well-formed message. Mirrors the old
 * `structuredMessagesFromEnvelope` consumer helper.
 */
export function structuredMessagesFromEnvelope(
  event: SessionStreamStructuredMessageEvent,
): SessionStructuredMessage[] {
  if (!Array.isArray(event.structured_messages)) return [];
  return event.structured_messages.filter(isStructuredMessage);
}

// ---------------------------------------------------------------------------
// Pure render helpers (ported from the old dashboard crew.ts at parity).
// ---------------------------------------------------------------------------

function formatPatchRange(start: number | undefined, lines: number | undefined): string {
  const safeStart = start ?? 1;
  if (lines === undefined || lines === 1) return String(safeStart);
  return `${safeStart},${lines}`;
}

function formatPatchHunkHeader(hunk: SessionStructuredPatchHunk): string {
  const oldStart = hunk.old_start;
  const newStart = hunk.new_start;
  if (oldStart === undefined && newStart === undefined) return '@@';
  return `@@ -${formatPatchRange(oldStart, hunk.old_lines)} +${formatPatchRange(newStart, hunk.new_lines)} @@`;
}

/**
 * Render edit/write patch hunks to unified-diff text. Emits a
 * `*** Update File: <path>` separator each time the hunk's file_path changes,
 * a `@@ … @@` header per hunk, then the hunk's lines verbatim.
 */
export function patchTextFromHunks(
  hunks: readonly SessionStructuredPatchHunk[] | undefined,
): string {
  if (hunks === undefined || hunks.length === 0) return '';
  const lines: string[] = [];
  let lastFilePath = '';
  for (const hunk of hunks) {
    const filePath = hunk.file_path ?? '';
    if (filePath !== '' && filePath !== lastFilePath) {
      lines.push(`*** Update File: ${filePath}`);
      lastFilePath = filePath;
    }
    lines.push(formatPatchHunkHeader(hunk));
    if (hunk.lines !== undefined) {
      for (const line of hunk.lines) lines.push(line);
    }
  }
  return lines.join('\n');
}

function appendUsagePart(parts: string[], label: string, value: number | undefined): void {
  // Zero token counts are dropped (distinct from the context pair/percent below).
  if (value !== undefined && value !== 0) parts.push(`${label} ${value}`);
}

/**
 * Render provider-neutral token usage to the compact `tokens …` summary line.
 * Zero token counts are dropped; the context pair and percent render whenever
 * defined (including an explicit `0%`). Returns `""` when nothing renders.
 */
export function formatUsage(usage: SessionStructuredUsage | undefined): string {
  if (usage === undefined) return '';
  const parts: string[] = [];
  appendUsagePart(parts, 'in', usage.input_tokens);
  appendUsagePart(parts, 'out', usage.output_tokens);
  appendUsagePart(parts, 'reason', usage.reasoning_tokens);
  appendUsagePart(parts, 'cache', usage.cache_read_tokens);
  appendUsagePart(parts, 'write', usage.cache_creation_tokens);
  const contextUsed = usage.context_used_tokens;
  const contextWindow = usage.context_window_tokens;
  if (contextUsed !== undefined && contextWindow !== undefined) {
    parts.push(`${contextUsed}/${contextWindow}`);
  }
  const contextPercent = usage.context_percent;
  if (contextPercent !== undefined) parts.push(`${contextPercent}%`);
  return parts.length > 0 ? `tokens ${parts.join(' ')}` : '';
}
