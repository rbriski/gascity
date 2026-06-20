---
title: "Structured Session Stream and Transcript Format (`format=structured`)"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-20 |
| Author(s) | Claude, Codex |
| Issue | N/A |
| Supersedes | N/A |

## Summary

The supervisor session stream and transcript endpoints accept two requested
formats today: `conversation` (the default) and `raw`. The transcript/peek path
can also return `format: "text"` when no transcript exists yet and pane output
is the only observable source; that is a fallback response shape, not a third
requested query format. `conversation` is **lossy by design** — it flattens
every provider's content blocks into a single `outputTurn.Text` string,
replacing a tool call with `[name]`, truncating a tool result to 500
characters, and redacting thinking to the literal `[thinking]`
(`internal/api/handler_agent_output_turns.go:34-49,81-94`). `raw` is the
opposite: provider-native JSON forwarded as `SessionRawMessageFrame` values.
The raw contract is full-fidelity pass-through for valid provider frames; the
current implementation has one transport-validity caveat, escaping malformed
literal control characters inside JSON strings before emission so SSE never
sends invalid JSON. The enclosing stream/transcript envelope carries only a
`provider` identifier — so **every consumer must re-implement per-provider
frame parsing** to recover structure
(`internal/api/session_frame_types.go:26-150`;
`internal/api/supervisor_city_routes.go:13-21`).

The result is that a rich chat UI cannot be built on the supervisor API
without each client owning a fragile, untested, client-side normalization
layer for ~17 provider dialects. This is precisely what one downstream
consumer does today — its server reads `format=raw` and reconstructs full
fidelity (tool inputs, structured tool results, thinking, usage, subagent
nesting) before the browser ever sees it. That work is duplicated in every
consumer that wants a rich UI, and it lives one layer removed from the
provider knowledge that already exists *inside* Gas City.

This design adds a third requested format, **`format=structured`**, that emits
Gas City's already-parsed content blocks as a typed, versioned schema — text,
thinking (with signature when available and allowed), tool calls (with full
input), tool results (with structured Bash/Grep/Read shapes where GC can derive
them), interactions, usage, model, stop reason, and subagent relationships. The
per-provider knowledge stays in **one tested place near the source**
(`internal/sessionlog` / `internal/worker`), and **any** consumer — a chat UI,
the dashboard, an external client — gets a rich, provider-agnostic event model
without duplicating provider parsers.

The headline finding from the cross-codebase and provider-format audit behind
this spec: Gas City already parses provider frames into typed blocks for 8
transcript families, covering 11 of the 17 built-in provider profiles once
OpenCode-backed and Pi-family aliases are routed correctly. The dominant gap
is not a lack of a structured wire shape — it is that Gas City's API flattens
the structure it already has, and several providers need either modernized
readers or managed capture because their public persisted transcript is not a
rich complete trace. One cross-cutting carrier/schema change therefore unlocks
most fidelity immediately; targeted provider adapters close the remainder.
This is **transcript-schema parity**, not end-to-end provider parity: provider
resume, hooks, MCP projection, skills, and runtime behavior remain governed by
their own provider-specific designs.

## Background: what already exists

### Provider frames are already parsed per family

`ReadProviderFile` dispatches by `ProviderFamily` to a dedicated reader per
vendor (`internal/sessionlog/reader.go:153-172,1283-1303`):
`ReadCodexFile`, `ReadGeminiFile`, `ReadKimiFile`, `ReadMimoCodeFile`,
`ReadOpenCodeFile`, `ReadPiFile`, `ReadAntigravityFile`, and the default
Claude JSONL DAG reader (`ReadFile`). Each one translates that vendor's
native transcript into the common `sessionlog.Entry` / `ContentBlock` shape
(`internal/sessionlog/entry.go:60-77`) **and** preserves the original
provider-frame bytes in `Entry.Raw` for raw pass-through. Valid raw frames are
byte-preserved by the API; malformed string control characters are escaped as a
transport-validity repair before emission.

This is the per-provider translation layer the structured format needs. It
is not greenfield.

### The fidelity is parsed, then discarded

`sessionlog.ContentBlock` already retains full tool input
(`Input json.RawMessage`, `entry.go:73`), full tool-result content
(`Content json.RawMessage`, `entry.go:75`), `IsError` (`entry.go:76`), text,
and structured interaction fields (`entry.go:64-71`). The normalized
`worker.HistoryBlock` mirrors the core block shape and has a first-class
`HistoryInteraction` pointer (`internal/worker/types.go:160-170,199-210`).
The structure survives all the way to the API edge, where `entryToTurn` /
`historyEntryToTurn` deliberately throw it away into a flat string
(`internal/api/handler_agent_output_turns.go:26-54,73-101`).

What the typed model **cannot** carry today:

- **thinking signature + canonical thinking text** — `ContentBlock` and
  `HistoryBlock` can represent a `thinking` block kind, and several
  non-Claude readers currently put thinking text in `Text`, but there is no
  dedicated `thinking` field and no `signature` field. Anthropic thinking
  blocks whose text lives under `thinking` rather than `text` therefore still
  have no lossless carrier; the API then stamps `[thinking]`.
- **usage / model / stop_reason** — Claude and Codex usage are parsed through
  tail-only extractors (`ExtractTailUsage`, `ExtractCodexTailUsage`) and
  worker invocation telemetry, and tail metadata can infer model/context
  usage/activity, but those values are never joined to a `HistoryEntry`.
  `stop_reason` is inferred for activity, not preserved as entry data
  (`internal/sessionlog/tail.go:145-180`;
  `internal/worker/invocation_telemetry.go:188-225`).
- **structured tool results** — there is no carrier field analogous to a
  parsed `{stdout, stderr, exitCode}`; tool-result content is an opaque
  blob.
- **inline subagent nesting** — subagent mappings and transcript reads exist
  as separate worker/API surfaces (`AgentMappings`, `AgentTranscript`), but
  the session stream does not currently nest or reference subagent messages in
  the primary message payload. These are provider inline subagents linked by
  transcript metadata, not Gas City formula/drain fanout; drain correlation
  stays in beads, convoys, formulas, and events.

### Provider coverage contract

`format=structured` is a provider-neutral projection over all 17 built-in
provider profiles. The source adapter may read a native transcript, an official
export, a live NDJSON stream, an ACP event log, or a Gas City managed capture
file; the structured wire shape is the same either way. If a provider's native
local transcript omits tool output or file changes, Gas City must capture those
facts through that provider's hook/stream/ACP surface before claiming rich
structured coverage. It must not forward native provider JSON as a shortcut.

Builtin provider profiles live in `internal/worker/builtin/profiles.go`.
The current adapter/capture target is:

| Profile | Structured source | Expected v1 fidelity |
|---|---|---|
| claude | Claude JSONL under `~/.claude/projects/...` | rich messages, tool calls/results, thinking placeholder/text when allowed, raw only for native frames |
| codex | Codex rollout JSONL / `--json` event stream | rich tool calls/results; command and patch events normalize to Bash/Edit where derivable |
| gemini | Gemini session records | rich tool calls/results, thoughts, token usage; reader must support current JSONL session format |
| kimi | Kimi Code `agents/main/wire.jsonl` (and legacy context logs while supported) | rich main-agent and subagent messages, tool calls/results |
| opencode | OpenCode export/mirror JSON | rich tool calls/results and interactions from message parts |
| mimocode | MiMo Code export/session database mirror using the OpenCode-compatible shape | rich tool calls/results through the MiMo/OpenCode adapter |
| groq | OpenCode-backed Gas City profile | same as OpenCode; no Groq-specific transcript dialect |
| cerebras | OpenCode-backed Gas City profile | same as OpenCode; no Cerebras-specific transcript dialect |
| pi | Pi JSONL under `~/.pi/agent/sessions` | rich messages, tool calls/results, Bash/Python execution where present |
| omp | Oh My Pi JSONL under `~/.omp/agent/sessions` | same Pi-family adapter, including Bash/Python execution records |
| antigravity | Antigravity transcript JSONL / brain artifacts | rich messages, tool calls/results, interactions, artifacts where exposed |
| copilot | Copilot CLI session-state event log | rich once a dedicated reader maps `tool.execution_*`, terminal output, and write diffs |
| kiro | Kiro ACP JSONL event log or chat DB export | rich through ACP events (`ToolCall`, `ToolCallUpdate`, `TurnEnd`); DB-only chat export needs sampling |
| cursor | Cursor transcript plus Gas City hook capture | native JSONL has messages and tool-call inputs only; tool outputs require hook capture |
| amp | `amp --execute --stream-json` live capture | rich for execute/headless flows; retrospective local thread discovery is not a public stable source |
| grok | Grok `--output-format streaming-json` or ACP capture | rich for headless/ACP capture; persisted `~/.grok/sessions` schema is not public enough to rely on alone |
| auggie | Auggie saved session JSON or SDK/stream capture | rich through saved session `chatHistory` / node graph once a dedicated reader is added |

Practical consequence: the carrier + stop-flatten change immediately lights up
the 8 existing transcript families and 11 built-in profiles. The remaining 6
native profiles require reader/capture work, not client-side provider parsers.

## The streaming ceiling (read before scoping "streaming")

Both Gas City and the audited consumer operate on **whole committed
frames**, not token/sub-frame deltas:

- Claude Code writes whole JSONL message lines (a `tool_use` block is
  committed with its `input` already complete; the `tool_result` arrives as
  one later whole line). There are no `content_block_delta` /
  `input_json_delta` frames in the transcript.
- Codex writes a whole `function_call_output` per result; its
  `exec_command_begin/end` progress frames are dropped today in
  `skipCodexEventMsgType` (`internal/sessionlog/codex_reader.go:315-327`).
- The consumer's streaming-delta assemblers exist but are **bypassed** in
  production — they consume the same whole frames.

Gas City's session stream already delivers these frames **live as they
commit**, keyed by a cursor. So "streaming tool results" realistically
means: *the tool call appears live, then its complete result appears live* —
not bytes accumulating character-by-character. True sub-frame streaming
would require tapping the provider's live API wire, which is outside the
transcript-reader model.

This spec therefore defines streaming in two phases (per direction set in
review):

- **Phase 1 — frame-granular streaming.** Structured blocks delivered live
  as each transcript frame commits. This matches the best fidelity any
  consumer ships today and is the v1 target.
- **Phase 2 — sub-frame streaming.** Token/delta-level streaming by tapping
  providers' live API output. A materially larger effort, scoped as future
  work, not blocking Phase 1.

One asymmetry to note: for **Gemini**, the GC reader reads the *whole file*
via `os.ReadFile` (`internal/sessionlog/gemini_reader.go:19`). Existing stream
machinery can re-read a valid whole-file snapshot on change and emit new entries
by cursor, but a tolerant/incremental Gemini parser is still needed for robust
frame-granular behavior while the provider file is being written. That is a
Phase 1 hardening task for Gemini specifically.

## Goals

- A third session-stream/transcript format, `format=structured`, emitting a
  typed, versioned, provider-agnostic message + block schema.
- Highest available fidelity for all 17 built-in provider profiles: tool
  inputs, **structured tool results**, file edits/diffs, thinking + signature,
  usage, model, stop-reason, and provider subagent relationships where those
  fields are present or derivable.
- Zero client-side per-provider enrichment required to render a rich UI.
- Provider-specific parsing/capture remains inside Gas City adapter code. A
  client asking for `format=structured` must never need to know a provider's
  native transcript dialect.
- Existing long-tail families and aliases (`kimi`, `opencode`, `mimocode`,
  `groq`, `cerebras`, `pi`, `omp`, `antigravity`) gain useful structured
  output without inventing new wire contracts.
- A golden-fixture regression guard so server-side translation is safe
  across provider/version drift.

## Non-goals

- Removing or changing `conversation` / `raw` (both remain; back-compat).
- Sub-frame/token streaming (Phase 2, documented but not built here).
- Passing provider-native JSON, provider-native metadata maps, or
  provider-specific field names through `format=structured`. That remains the
  purpose of `format=raw`.
- Presentation concerns — markdown→HTML, syntax highlighting, diff
  rendering, visual tool_use/result pairing. These stay in the consumer;
  `format=structured` ships *data*, not HTML.

## Architectural alignment

Scoped to respect Gas City's load-bearing invariants (AGENTS.md):

- **Object model at the center.** The per-provider translation and the new
  carrier fields land in the domain (`internal/sessionlog`,
  `internal/worker`); `internal/api` only *projects* the normalized history
  into the structured wire shape. No domain logic moves into the API layer,
  and the carrier extends the canonical normalized-history model rather than
  forking a parallel one. The stream path already uses `worker.Handle.History`;
  the transcript path still reads `sessionlog` directly today, and AGENTS.md
  explicitly says the worker-boundary migration is not a sessionlog read-site
  inventory. Structured transcript implementation should prefer the worker
  history/transcript boundary where practical, but any remaining direct
  `sessionlog` use in `internal/api` must stay a projection over the domain
  parser, not a second parser.
- **Transport, not judgment.** Frame→block translation and Codex
  command-shape classification must remain deterministic adapter
  normalization, not role logic or user-work reasoning. `format=structured`
  is a projection of existing domain data, not a new SDK primitive.
- **Upstream alignment.** The carrier touches upstream-owned files
  (`sessionlog/entry.go`, `worker/types.go`, `internal/api` handlers). Keep
  each edit minimal and idiomatic so it rebases cleanly, and treat this
  feature as a strong candidate to propose upstream rather than carry as
  fork-only divergence.
- **Required reading before implementing.**
  `engdocs/architecture/api-control-plane.md` (typed-wire rules and the
  raw-frame opacity exception) and `engdocs/contributors/huma-usage.md`
  (SSE/Huma registration), per AGENTS.md.

## Proposed schema

Typed wire only — no bare `map[string]any` / `json.RawMessage` on the API or
SSE wire path. The concrete Huma-registered Go wire structs are the source for
the generated OpenAPI; the pseudocode below is a design sketch for those
structs, and `TestOpenAPISpecInSync` must pass.

`format=structured` has an additional provider-neutrality contract:
**it must not send provider-native transcript frames, provider-native JSON
leaves, or provider-specific dialect shapes over the wire.** The point of the
format is that clients can render rich session output without knowing that
Codex calls a field `call_id`, Claude calls it `tool_use_id`, or Gemini stores
tool arguments under a different object shape. Exact provider frames remain
available only through `format=raw`.

Every tool input, tool result, interaction, usage record, and diagnostic exposed
by `format=structured` must therefore be one of:

- a provider-neutral typed shape Gas City owns (preferred for known tool
  families such as Bash/Grep/Read/Edit/Search and known interaction fields);
  or
- a provider-neutral fallback shape made from typed fields such as
  `{ kind: "text", text }` or `{ kind: "arguments", arguments: [{ name, value }] }`.

Fallbacks preserve useful display data, but they do not preserve arbitrary
provider-native JSON. Consumers that need byte-level provider evidence must ask
for `format=raw`.

Do not use anonymous maps, raw JSON fields, unregistered unions, or "any JSON"
carriers directly on the structured API types.

```
StructuredHistory
  gcSessionId           string?
  logicalConversationId string?
  providerSessionId     string?
  transcriptStreamId    string
  generation            { id, observedAt? }
  cursor                { afterEntryId? }
  continuity            { status, compactionCount?, hasBranches?, note? }
  tailState             { activity, lastEntryId?, openToolCallIds?, pendingInteractionIds?, degraded?, degradedReason? }
  diagnostics           []StructuredDiagnostic?

StructuredMessage
  id          string
  role        "user" | "assistant" | "system" | "tool"
  provider    string                 // claude, codex, gemini, ...
  timestamp   string (RFC3339Nano)
  model       string?                // assistant turns
  stopReason  string?
  usage       StructuredUsage?
  isSubagent  bool?
  parentToolCallId string?
  status      string                 // final | partial | superseded | unknown
  blocks      []StructuredBlock

StructuredUsage
  inputTokens         int
  outputTokens        int
  cacheReadTokens     int?
  cacheCreationTokens int?
  contextWindowTokens int?           // when derivable server-side
  contextUsedTokens   int?
  contextPercent      int?

StructuredBlock  (discriminated on `type`)
  type "text"        => { text }
  type "thinking"    => { thinking, signature? }     // gated, see policy
  type "tool_use"    => { id, name, input, caller? } // input = provider-neutral typed shape
  type "tool_result" => { toolCallId, content, isError, structured? }
  type "interaction" => { requestId, kind, state, prompt, options, action }
  type "image"       => { ... }

StructuredToolInput  (the `input` on tool_use; discriminated on `kind`)
  kind "command"   => { command, args? }
  kind "code"      => { code }
  kind "patch"     => { patch, filePath? }
  kind "search"    => { query?, pattern? }
  kind "file"      => { filePath }
  kind "arguments" => { arguments: [{ name, value }] } // provider-neutral strings
  kind "text"      => { text }

StructuredToolResult  (the `structured?` on tool_result; discriminated on `kind`)
  kind "bash"   => { stdout, stderr, exitCode?, interrupted, truncated?, isImage? }
  kind "python" => { code?, stdout, stderr, exitCode?, interrupted, truncated? }
  kind "grep"   => { mode, filenames[], numFiles, content, numLines }
  kind "read"   => { filePath, content, numLines, startLine?, totalLines? }
  kind "edit"   => { filePath?, patch?, content? }
  kind "search" => { query?, content, numResults? }
  kind "text"   => { text, content? }                 // provider-neutral fallback
```

The concrete Go wire structs should use Gas City's normal JSON spelling
(`schema_version`, `stop_reason`, `is_subagent`, `parent_tool_call_id`,
`tool_call_id`, `is_error`, etc.). The camel-case pseudocode above names the
concepts, not the literal tags. Use `tool_call_id` even when the native
provider calls the value `tool_use_id`, `call_id`, or something else; native
spelling belongs only in `format=raw`.

The structured transcript response and the structured stream must preserve the
worker history envelope, not only individual messages. Transcript snapshots can
return one `history` object next to `messages`; SSE can either include the
relevant history cursor/generation fields on each structured message or add a
typed snapshot/reset event. In either case, generation resets, cursor
invalidations, degraded continuity, and tail state must remain visible on the
wire, matching `worker-conformance.md` §4.3 rather than silently replaying or
dropping history.

The stream keeps the existing lifecycle event kinds (`activity`, `pending`,
`heartbeat`) and adds a versioned structured message payload. Current Huma SSE
registration maps **one concrete Go payload type to one SSE event name**
(`internal/api/sse.go:257-283,366-369`), and the session stream already maps
`turn` to `SessionStreamMessageEvent` and `message` to
`SessionStreamRawMessageEvent` (`internal/api/supervisor_city_routes.go:13-21`).
Implementation must therefore choose one of two explicit paths:

- add a distinct event name for structured frames (for example,
  `event: structured`) and register `StructuredMessageEvent` in
  `sessionStreamEventMap`; or
- refactor the SSE helper/event map to support multiple schema variants under
  the same semantic/default SSE `message` event key without emitting an
  unregistered concrete type. This path must also preserve generated-client
  narrowing by adding a schema-supported discriminator inside `data` or proving
  the TS/Go generated clients handle the resulting union.

Either way, add `schema_version` to the structured payload itself. Today's
`conversation` and `raw` payloads remain unversioned for back-compat.

When the transcript path has no structured history yet, `format=structured`
must not pretend pane text is structured data. Reuse the existing transcript
fallback behavior by returning `format: "text"` for peek-only responses, or
emit only lifecycle frames (`pending` / `activity` / heartbeat) on the live
stream until provider history exists. The implementation choice must be
documented in tests because this is the first edge most clients will hit when
they attach to a brand-new session.

### Thinking-exposure policy

`conversation` redacts thinking by deliberate policy
(`handler_agent_output_turns.go:46-49`). `format=structured` is itself
opt-in, but to avoid silently reversing that stance for consumers who don't
want reasoning, thinking blocks are gated behind an explicit
`include_thinking=true` query parameter; default omits the `thinking`
block's text (keeping a typed placeholder block so ordering is preserved).
The parameter must be declared on the Huma input structs so it appears in
OpenAPI; do not read it through raw URL inspection. This is a product/safety
decision flagged for sign-off, not just a code toggle.

## Phasing

### Phase 1 — Structured, frame-granular format

**1A. Carrier + schema + stop-flatten (cross-cutting; unlocks 8 transcript
families and 11 profiles).**

- `internal/sessionlog/entry.go:60-77` — add `Thinking`, `Signature`, and a
  structured `ToolResult` field to `ContentBlock`; map Anthropic `thinking`
  → the new field in `Entry.ContentBlocks()`.
- `internal/worker/types.go` — add `Usage`, `Model`, `StopReason` to
  `HistoryEntry` (187-197); `Thinking` / `Signature` / structured-result to
  `HistoryBlock` (200-210).
- `internal/worker/sessionlog_adapter.go:293-368` — decode
  `message.usage` / `model` / `stop_reason` (reuse the cache-aware parser in
  `tail_usage.go`) and populate the new fields instead of only `cloneRaw`.
  Preserve context-window/fullness data from `TailMeta.ContextUsage` when the
  server can derive it; do not make clients reimplement model-window lookup.
- `internal/api/` — register `format=structured` on the Huma session stream
  (`huma_handlers_sessions_stream.go`) and transcript
  (`huma_handlers_sessions_query.go`) handlers, emitting `StructuredMessage`;
  add the typed wire types, the Huma input docs/validation, and the OpenAPI
  SSE schema; leave `conversation`/`raw` untouched.
- `cmd/gc/dashboard/web/src/generated/` and `internal/api/genclient/` —
  regenerate clients from the Huma spec. The dashboard uses generated
  `streamSession` SSE types and currently bridges heterogeneous frame payloads
  opaquely, so this is a generated-client/API-shape change, not just a server
  change.
- Live delivery reuses the existing tail/cursor stream machinery — frames
  are emitted structured as they commit (frame-granular streaming).

Outcome: claude + codex + gemini + kimi + opencode + mimocode + groq +
cerebras + pi + omp + antigravity emit useful structured blocks immediately,
with fidelity bounded by the carrier fields they can populate and by targeted
reader gaps closed below.

**1B. Codex structured tool results** (`internal/sessionlog/codex_reader.go`).

The load-bearing prerequisite is a `call_id → {toolName, readShellInfo}`
context map threaded through `ReadCodexFile`'s loop — without it a
`function_call_output` cannot know whether it was Bash/Grep/Read, which is
exactly why Codex results are opaque today. Then:

- tool-name canonicalization (`apply_patch`→Edit, `shell_command` /
  `exec_command`→Bash, `web_search_call` / `search_query`→WebSearch);
- command-shape reclassification (`cat`/`sed`/`nl`→Read, `rg`→Grep) with a
  quote-aware tokenizer;
- structured result parsers: Bash (strip `Output:\n`, split stdout/stderr,
  exit code), ripgrep (content vs files-with-matches; no-match ≠ error),
  read (line numbering, strip `nl` prefixes);
- exit-code / `is_error` derivation from text and fields;
- `web_search_call` handling; reasoning fallback to the item's `content`
  when `summary` is empty, and `encrypted_content` → `signature: "encrypted"`;
- `token_count` event → per-entry usage; capture `model`.
- extend `codexResponseItem` only for the genuinely missing fields —
  `arguments` and `encrypted_content`. `content` (the typed `[{text}]`
  slice), `summary`, `input`, and `output` already exist as struct fields
  (`codex_reader.go:373-391`), so the reasoning `content`-fallback is a
  read-path change — reasoning items read only `summary` today
  (`codex_reader.go:220-234`) — not a new field.

**1C. Gemini and Kimi current-format gaps.**

- Gemini: support the current JSONL session format, add `tokens` → usage,
  set `tool_result.is_error` from `toolCall.status`, handle `type:"error"`
  messages, preserve model/stop metadata, and add an **incremental parser** so
  live frame-granular streaming works (the legacy reader is whole-file).
- Kimi: prefer current `~/.kimi-code/sessions/<workDirKey>/<sessionId>/agents/main/wire.jsonl`
  and subagent `wire.jsonl` files, while retaining legacy context-log support
  until it is no longer useful.

**1D. Alias and Pi-family hardening.**

- Route `groq` and `cerebras` through the OpenCode adapter. They are Gas City
  OpenCode-backed profiles, not distinct transcript dialects.
- Route `omp` through the Pi-family adapter and normalize OMP `bashExecution`
  / `pythonExecution` messages to provider-neutral Bash/Python tool-result
  shapes with output, exit code, cancellation/interruption, and truncation.
- Add `is_error` where omitted today for kimi/antigravity tool results.
- Note: per-invocation usage is absent for some long-tail readers today. Pi
  exposes compaction `PreTokens`, but that is context-boundary evidence, not
  response usage.

**1E. Native reader/capture work for the remaining 6 profiles.**

- Copilot: add a reader for `~/.copilot/session-state/<sessionId>/events.jsonl`
  that maps assistant/user events, `tool.execution_start`,
  `tool.execution_complete`, terminal output blocks, and write diffs to the
  neutral block/result schema.
- Kiro: prefer ACP JSONL event logs under `~/.kiro/sessions/cli/` because ACP
  explicitly exposes `ToolCall`, `ToolCallUpdate`, and `TurnEnd`; sample and
  document the chat database/export path before using it.
- Cursor: native local transcripts intentionally omit tool outputs, so rich
  structured support requires Gas City-managed hook capture (for example
  `postToolUse`) or sidecar output reconstruction. The native JSONL alone may
  provide tool-call intent but is insufficient for rich results/diffs.
- Amp: capture `amp --execute --stream-json` / `--stream-json-input` stdout for
  structured non-interactive sessions. Do not claim retrospective rich local
  thread discovery unless Amp publishes a stable local transcript source.
- Grok: capture `--output-format streaming-json` or ACP `session/update`
  events for headless/ACP sessions. The documented `~/.grok/sessions` path is
  not enough by itself without a stable persisted schema.
- Auggie: add a saved-session reader for `chatHistory` / node graph data, or
  use SDK/stream capture, mapping `tool_use`, `tool_result_node`, command
  output, edit metrics, and diffs into neutral tool-result shapes.

**1F. Golden-fixture test corpus (cross-cutting; do alongside 1A–1E).**

Capture real `format=raw` transcripts per provider/version into a fixture
corpus and snapshot-test the structured producer. A provider CLI bump
becomes "regenerate fixtures + diff." This is the regression guard that
makes one-place server-side translation safe — and is exactly the
discipline consumers reverse-engineering frames never had.

Extend the existing gates, not just new golden tests:

- API session stream/transcript tests for thinking redaction defaults,
  `include_thinking`, raw preservation, `text` fallback, and structured
  no-history behavior (`internal/api/handler_sessions_test.go` already covers
  adjacent stream cases).
- Worker conformance helpers for open-tail history, tool calls/results,
  interactions, and the new usage/model/stop-reason carriers
  (`internal/worker/workertest/phase2_result_helpers_test.go`).
- Huma SSE schema coverage (`internal/api/huma_sse_test.go`), OpenAPI sync
  (`internal/api/openapi_sync_test.go`), generated Go client sync
  (`internal/api/genclient/genclient_test.go`), and dashboard TypeScript type
  generation (`make dashboard-check` when generated web types change).

### Phase 2 — Sub-frame streaming (future)

Tap providers' live API output for token/delta-level streaming
(`content_block_delta` / `input_json_delta` equivalents). Materially larger
than Phase 1: it steps outside the transcript-reader model and needs a live
provider-wire tap plus delta-assembly state. Documented here so the schema
(`StructuredBlock` deltas, `schema_version`) is designed to admit it later
without a breaking change; not built in this spec.

## Consumer impact

Any consumer that today reads `raw` and re-derives structure can switch to
`format=structured` and **delete its per-provider transcript translation
layer**, keeping only genuine presentation (markdown→HTML, highlighting, diffs,
visual pairing). The division of labor is explicit:

- **Gas City owns** canonical structured *data* — typed blocks, structured
  tool results, usage, thinking (gated), subagent nesting.
- **Consumers own** presentation.

This is mode-agnostic: a self-hosted supervisor on loopback and a future
remote/managed control plane forwarding streams to many clients both
benefit — a multi-tenant forwarder wants a clean versioned typed format even
more than a loopback one does.

The session stream/transcript endpoints are city-scoped today and inherit
city/tenant identity from the `/v0/city/{cityName}/...` route context. Any
future aggregated forwarder must add its own source-city/tenant envelope and
composite cursor semantics, like the machine-wide event stream, rather than
pretending a per-city structured message is globally unique on its own.

It is still an observability/data projection. It is not checkpointing, durable
execution, formula/drain correlation, inter-session messaging, or an
authorization/redaction layer.

## Risks & open questions

- **Thinking exposure** — `include_thinking` default and whether the gated
  default omits text or the whole block. Needs sign-off (policy, not code).
- **Schema churn** — `StructuredToolInput` and `StructuredToolResult` kinds
  (bash/grep/read/edit/search/text/etc.) need fixture pressure before freezing
  the wire. The fallback must stay provider-neutral (`kind:"text"` or
  normalized argument strings), not raw provider JSON.
- **Sensitive payload exposure** — full tool inputs, tool results,
  interaction prompts/options, code/diffs, and thinking can contain secrets.
  Any remote or multi-tenant deployment needs the same auth, redaction,
  log-scrubbing, and file-permission discipline used for projected MCP files
  and other secret-bearing runtime surfaces.
- **Provider-neutrality boundary** — `format=structured` must normalize
  provider-specific fields into typed provider-neutral shapes. Do not put bare
  `json.RawMessage`, `map[string]any`, unregistered unions, "any JSON" carriers,
  or provider-native field objects directly on structured API structs.
- **SSE event-name compatibility** — preserving the current default SSE
  `message` event semantics for raw frames while adding structured frames
  requires either a new event name or an intentional SSE helper refactor, as
  described in the schema section. If raw and structured share the same
  semantic event key, the replacement discriminator must remain visible to
  generated clients; otherwise dashboard consumers lose `switch (frame.event)`
  narrowing.
- **`worker-conformance.md` alignment** — the carrier change extends the
  canonical normalized model that doc designates as core; the new fields
  should land as conformance assertions, not incidental observability.
- **Provider capture completeness** — Cursor, Amp, Grok, and some Kiro paths
  are not safely solved by generic local transcript discovery. Their rich
  structured coverage depends on managed hook/stream/ACP capture, and tests
  must distinguish "message/tool-call intent available" from "full rich
  result/diff trace available."

## Appendix: source facts

- Formats accepted today: only `raw` is branched on; anything else is the
  `conversation` default (`huma_handlers_sessions_stream.go:40,123-140`;
  `huma_handlers_sessions_query.go:161-230`). No `format` enum exists
  (`huma_types_sessions.go:101,110`), and the generated OpenAPI currently
  documents it only as free-form prose.
- `text` fallback: transcript and live peek can return `format: "text"` when
  there is no provider transcript yet and pane output is the only available
  source (`huma_handlers_sessions_query.go:264`;
  `streamSessionPeekHuma` in `handler_session_stream.go:1051`, def at `:1014`).
- Worker-boundary caveat: the migration governs session creation/lifecycle;
  AGENTS.md explicitly says stream and transcript readers in `internal/api/`
  still read session logs directly (`AGENTS.md:248-265`;
  `huma_handlers_sessions_query.go:156-209`).
- Flattening: `entryToTurn` / `historyEntryToTurn`
  (`handler_agent_output_turns.go:26-101`).
- Typed block fidelity retained pre-flatten: `ContentBlock`
  (`entry.go:60-77`), `HistoryBlock` (`worker/types.go:199-210`).
- Per-family dispatch: `reader.go:153-172,1283-1303`.
- Tail-only usage: `tail_usage.go:12-98`, `codex_usage.go:31-123`,
  `worker/invocation_telemetry.go:188-225`.
- Codex opacity: `codex_reader.go:254-270,315-327`.
- Gemini whole-file: `gemini_reader.go:18-28`.
- Raw-frame "honest opacity" rationale: `engdocs/architecture/api-control-plane.md`
  §3.6. Normalized-history alignment: `engdocs/design/worker-conformance.md`
  §4.1–4.2.
- Provider-format research:
  - Claude documents continuous local JSONL transcript storage under
    `~/.claude/projects/<project>/<session-id>.jsonl`, with each line a JSON
    object for message, tool use, or metadata:
    <https://code.claude.com/docs/en/sessions>.
  - Codex documents `--json` as newline-delimited JSON events and
    `--ephemeral` as disabling persisted rollout files:
    <https://developers.openai.com/codex/cli/reference>.
  - Gemini CLI session management records prompts/responses, tool execution
    inputs/outputs, token usage, and assistant thoughts:
    <https://developers.googleblog.com/pick-up-exactly-where-you-left-off-with-session-management-in-gemini-cli/>.
  - Kimi Code stores sessions under `~/.kimi-code/sessions/...`, with
    `agents/main/wire.jsonl` as the main agent communication record:
    <https://www.kimi.com/code/docs/en/kimi-code-cli/configuration/data-locations.html>.
  - OpenCode exports session data as JSON and exposes session/database
    commands, including `opencode export [sessionID]`:
    <https://opencode.ai/docs/cli/>.
  - MiMo Code persists session data in `MIMOCODE_HOME` and supports JSON
    export/import:
    <https://mimo.xiaomi.com/mimocode/sessions>.
  - Oh My Pi documents JSONL session storage under
    `~/.omp/agent/sessions/...` and hook events for tool calls/results:
    <https://github.com/can1357/oh-my-pi/blob/main/docs/session.md>,
    <https://github.com/can1357/oh-my-pi/blob/main/docs/hooks.md>.
  - Copilot CLI documents local session data and full-history resume behavior:
    <https://docs.github.com/en/copilot/how-tos/copilot-cli/use-copilot-cli/chronicle>.
  - Kiro documents local database-backed chat sessions and ACP JSONL session
    logs with `ToolCall`, `ToolCallUpdate`, and `TurnEnd` updates:
    <https://kiro.dev/docs/cli/chat/session-management/>,
    <https://kiro.dev/docs/cli/acp/>.
  - Cursor staff state that local JSONL transcripts include messages,
    assistant text, and tool-call inputs but intentionally omit tool-call
    outputs; they recommend hooks for full output capture:
    <https://forum.cursor.com/t/accessing-the-full-agent-transcript-in-cursor/157311>.
  - Amp documents `--execute --stream-json` as line-delimited structured
    output, plus `--stream-json-input` for programmatic conversations:
    <https://ampcode.com/manual>.
  - Grok documents headless sessions under `~/.grok/sessions`,
    `--output-format streaming-json`, and ACP `session/update` chunks:
    <https://docs.x.ai/build/cli/headless-scripting>.
  - Auggie documents saved session resume/list commands and cache relocation;
    package inspection confirms saved session `chatHistory`/node graph shape:
    <https://docs.augmentcode.com/cli/reference>.
  - Google documents Antigravity CLI as the Gemini CLI successor with hooks,
    subagents, and extensions:
    <https://developers.googleblog.com/an-important-update-transitioning-gemini-cli-to-antigravity-cli/>.
