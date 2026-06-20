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

The headline finding from the cross-codebase audit behind this spec: Gas
City **already parses** provider frames into typed blocks for 8 of the
relevant families. The dominant gap is not whole-reader creation — it is that
Gas City's block/entry model cannot *carry* several rich fields, the API
*flattens* what it does carry, and a few provider-specific gaps remain. One
cross-cutting carrier change therefore unlocks most of the fidelity at once;
Codex and Gemini need targeted gap work, not brand-new reader families.
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

### Coverage today

| Tier | Providers | GC reader | Notes |
|---|---|---|---|
| Frontier dedicated | claude, codex, gemini | dedicated | Dedicated readers exist; remaining fidelity gaps are listed below |
| **GC already structured** | kimi, opencode, mimocode, pi, antigravity | dedicated | Emit tool_use / tool_result blocks today, and thinking / interaction blocks where the provider records them; the carrier/API projection hides common block structure, while model/usage/stop metadata still needs provider-specific work where available |
| Thin | grok, kiro, cursor, copilot, amp, groq, cerebras, auggie, omp | Claude-fallback | Structured only if the provider writes Claude-shaped JSONL |

Builtin list: `internal/worker/builtin/profiles.go:83-87`. The practical
consequence: a carrier + stop-flatten change immediately lights up **8
families** (claude + codex + gemini + the 5 "already structured" long-tail
families), while the provider-specific work below closes remaining gaps.

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
- Highest available fidelity for the three frontier providers (claude, codex,
  gemini): tool inputs, **structured tool results**, thinking + signature,
  usage, model, stop-reason, and provider subagent relationships where those
  fields are present or derivable.
- Zero client-side per-provider enrichment required to render a rich UI.
- The 5 "already structured" long-tail families gain useful structured output
  without new reader families.
- A golden-fixture regression guard so server-side translation is safe
  across provider/version drift.

## Non-goals

- Removing or changing `conversation` / `raw` (both remain; back-compat).
- Sub-frame/token streaming (Phase 2, documented but not built here).
- Dedicated readers for the 9 "thin" providers (deferred pending a fixture
  capture that reveals whether they are Claude-shaped).
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
structs, and `TestOpenAPISpecInSync` must pass. Arbitrary tool input,
tool-result content, interaction metadata, and `opaque.raw` are the only
pressure points. They need one of two treatments before implementation:

- model the shapes as concrete discriminated unions where GC owns the shape
  (preferred for Bash/Grep/Read and known interaction metadata); or
- introduce a named `StructuredJSONValue` / `StructuredOpaqueJSON` wire type
  with Huma schema support and document it as a narrow API control-plane
  exception, analogous in spirit to `SessionRawMessageFrame` but scoped to
  structured payload leaf values.

Do not use anonymous maps, raw JSON fields, or unregistered unions directly on
the structured API types.

```
StructuredHistory
  gcSessionId           string?
  logicalConversationId string?
  providerSessionId     string?
  transcriptStreamId    string
  generation            { id, observedAt? }
  cursor                { afterEntryId? }
  continuity            { status, compactionCount?, hasBranches?, note? }
  tailState             { activity, lastEntryId?, openToolUseIds?, pendingInteractionIds?, degraded?, degradedReason? }
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
  parentToolUseId string?
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
  type "tool_use"    => { id, name, input, caller? } // input = typed union or named opaque JSON value
  type "tool_result" => { toolUseId, content, isError, structured? }
  type "interaction" => { requestId, kind, state, prompt, options, action, metadata }
  type "image"       => { ... }

StructuredToolResult  (the `structured?` on tool_result; discriminated on `kind`)
  kind "bash"   => { stdout, stderr, exitCode?, interrupted, isImage? }
  kind "grep"   => { mode, filenames[], numFiles, content, numLines }
  kind "read"   => { filePath, content, numLines, startLine?, totalLines? }
  kind "opaque" => { raw }                            // leaf fallback; full raw mode remains separate
```

The concrete Go wire structs should use Gas City's normal JSON spelling
(`schema_version`, `stop_reason`, `is_subagent`, `parent_tool_use_id`,
`tool_use_id`, `is_error`, etc.). The camel-case pseudocode above names the
concepts, not the literal tags.

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

**1A. Carrier + schema + stop-flatten (cross-cutting; unlocks 8 families).**

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

Outcome: claude + kimi + opencode + mimocode + pi + antigravity emit useful
structured blocks immediately, with fidelity bounded by the carrier fields they
can populate. gemini and codex emit structured blocks too, minus their specific
gaps (closed below).

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

**1C. Gemini gaps** (`internal/sessionlog/gemini_reader.go`).

- add `tokens` → usage (absent today);
- set `tool_result.is_error` from `toolCall.status`;
- handle `type:"error"` messages (silently dropped today);
- preserve any provider-native model/stop metadata that appears in messages;
- add an **incremental parser** so live frame-granular streaming works
  (today it is whole-file `os.ReadFile`).

**1D. Long-tail polish (optional, cheap).**

- add `is_error` to kimi/antigravity tool_results;
- note: per-invocation usage is absent for long-tail readers today. Pi exposes
  compaction `PreTokens`, but that is context-boundary evidence, not response
  usage.

**1E. Golden-fixture test corpus (cross-cutting; do alongside 1A–1C).**

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
- **Schema churn** — `StructuredToolResult` kinds (bash/grep/read/opaque)
  are Codex-derived; confirm they generalize before freezing the wire.
  `opaque` prevents losing an unclassified tool-result payload, but it does
  not replace `format=raw` unless the structured envelope also carries the
  whole provider-native frame.
- **Sensitive payload exposure** — full tool inputs, tool results,
  interaction metadata, and thinking can contain secrets. Any remote or
  multi-tenant deployment needs the same auth, redaction, log-scrubbing, and
  file-permission discipline used for projected MCP files and other
  secret-bearing runtime surfaces.
- **Typed-wire opacity boundary** — `input`, `content`, metadata, and
  `opaque.raw` need either concrete typed variants or a named, documented
  "any JSON value" carrier with an explicit API control-plane exception. Do
  not put bare `json.RawMessage`, `map[string]any`, or unregistered unions
  directly on API structs.
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
- **Thin providers (9)** — deferred; a fixture-capture pass decides whether
  they need dedicated readers or already ride the Claude shape.

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
