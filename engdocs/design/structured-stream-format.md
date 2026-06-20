---
title: "Structured Session-Stream Format (`format=structured`)"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-20 |
| Author(s) | Claude |
| Issue | N/A |
| Supersedes | N/A |

## Summary

The supervisor session stream exposes two formats today: `conversation`
(a.k.a. normalized) and `raw`. `conversation` is **lossy by design** — it
flattens every provider's content blocks into a single `outputTurn.Text`
string, replacing a tool call with `[name]`, truncating a tool result to 500
characters, and redacting thinking to the literal `[thinking]`
(`internal/api/handler_agent_output_turns.go:34-49,81-94`). `raw` is the
opposite: provider-native JSON forwarded verbatim, tagged only with a
`provider` identifier so that **every consumer must re-implement
per-provider frame parsing** to recover structure
(`internal/api/handler_session_stream.go:35-37`).

The result is that a rich chat UI cannot be built on the supervisor API
without each client owning a fragile, untested, client-side normalization
layer for ~17 provider dialects. This is precisely what one downstream
consumer does today — its server reads `format=raw` and reconstructs full
fidelity (tool inputs, structured tool results, thinking, usage, subagent
nesting) before the browser ever sees it. That work is duplicated in every
consumer that wants a rich UI, and it lives one layer removed from the
provider knowledge that already exists *inside* Gas City.

This design adds a third format, **`format=structured`**, that emits Gas
City's already-parsed content blocks as a typed, versioned schema — text,
thinking (with signature), tool calls (with full input), tool results (with
structured Bash/Grep/Read shapes), usage, model, stop-reason, and subagent
nesting. The per-provider knowledge stays in **one tested place near the
source** (`internal/sessionlog`), and **any** consumer — a chat UI, the
dashboard, an external client — gets a rich, provider-agnostic event model
for free, with no client-side enrichment.

The headline finding from the cross-codebase audit behind this spec: Gas
City **already parses** provider frames into typed blocks for 8 of the
relevant families. The dominant gap is not parsing — it is that Gas City's
block/entry model cannot *carry* the rich fields and the API *flattens*
what it does carry. One cross-cutting carrier change therefore unlocks most
of the fidelity at once; only Codex and Gemini need genuinely new
per-provider parsing.

## Background: what already exists

### Provider frames are already parsed per family

`ReadProviderFile` dispatches by `ProviderFamily` to a dedicated reader per
vendor (`internal/sessionlog/reader.go:153-172,1283-1303`):
`ReadCodexFile`, `ReadGeminiFile`, `ReadKimiFile`, `ReadMimoCodeFile`,
`ReadOpenCodeFile`, `ReadPiFile`, `ReadAntigravityFile`, and the default
Claude JSONL DAG reader (`ReadFile`). Each one translates that vendor's
native transcript into the common `sessionlog.Entry` / `ContentBlock` shape
(`internal/sessionlog/entry.go:60-77`) **and** preserves the original line
bytes in `Entry.Raw` for verbatim pass-through.

This is the per-provider translation layer the structured format needs. It
is not greenfield.

### The fidelity is parsed, then discarded

`sessionlog.ContentBlock` already retains full tool input
(`Input json.RawMessage`, `entry.go:73`), full tool-result content
(`Content json.RawMessage`, `entry.go:75`), `IsError` (`entry.go:76`), and
text. The normalized `worker.HistoryBlock` mirrors these
(`internal/worker/types.go:200-206`). The structure survives all the way to
the API edge, where `entryToTurn` / `historyEntryToTurn` deliberately throw
it away into a flat string
(`internal/api/handler_agent_output_turns.go:26-54,73-101`).

What the typed model **cannot** carry today:

- **thinking text + signature** — `ContentBlock` has no `thinking`/
  `signature` field, so Anthropic thinking blocks (whose text lives under a
  `thinking` key, not `text`) land nowhere; the API then stamps
  `[thinking]`.
- **usage / model / stop_reason** — available only via the tail-only
  `ExtractTailUsage` (`internal/sessionlog/tail_usage.go:12-98`), never
  joined to a `HistoryEntry`.
- **structured tool results** — there is no carrier field analogous to a
  parsed `{stdout, stderr, exitCode}`; tool-result content is an opaque
  blob.

### Coverage today

| Tier | Providers | GC reader | Notes |
|---|---|---|---|
| Both full | claude, codex, gemini | dedicated | The enrichment frontier |
| **GC already structured** | kimi, opencode, mimocode, pi, antigravity | dedicated | Emit tool_use / tool_result / thinking / interaction blocks today; only the carrier hides them |
| Thin | grok, kiro, cursor, copilot, amp, groq, cerebras, auggie, omp | Claude-fallback | Structured only if the provider writes Claude-shaped JSONL |

Builtin list: `internal/worker/builtin/profiles.go:83-87`. The practical
consequence: a carrier + stop-flatten change immediately lights up **8
families** (claude + the 5 "already structured" + the 2 of {codex, gemini}
that need only their gaps closed).

## The streaming ceiling (read before scoping "streaming")

Both Gas City and the audited consumer operate on **whole committed
frames**, not token/sub-frame deltas:

- Claude Code writes whole JSONL message lines (a `tool_use` block is
  committed with its `input` already complete; the `tool_result` arrives as
  one later whole line). There are no `content_block_delta` /
  `input_json_delta` frames in the transcript.
- Codex writes a whole `function_call_output` per result; its
  `exec_command_begin/end` progress frames are dropped today
  (`internal/sessionlog/codex_reader.go:317-327`).
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
via `os.ReadFile` (`internal/sessionlog/gemini_reader.go:19`) and cannot
consume partial frames at all — even frame-granular live updates require an
incremental gemini parser. That is a Phase 1 task for Gemini specifically.

## Goals

- A third session-stream/transcript format, `format=structured`, emitting a
  typed, versioned, provider-agnostic message + block schema.
- Full fidelity for the three frontier providers (claude, codex, gemini):
  tool inputs, **structured tool results**, thinking + signature, usage,
  model, stop-reason, subagent nesting.
- Zero client-side per-provider enrichment required to render a rich UI.
- The 5 "already structured" long-tail families light up for free.
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

## Proposed schema

Typed wire only — no `map[string]any` / `json.RawMessage` on the wire path
beyond the deliberately-opaque `input` / `opaque` carriers, consistent with
the typed-wire invariant (AGENTS.md). The schema is the source for the
generated OpenAPI; `TestOpenAPISpecInSync` must pass.

```
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
  status      string                 // final | in_turn | ...
  blocks      []StructuredBlock

StructuredUsage
  inputTokens         int
  outputTokens        int
  cacheReadTokens     int?
  cacheCreationTokens int?

StructuredBlock  (discriminated on `type`)
  type "text"        => { text }
  type "thinking"    => { thinking, signature? }     // gated, see policy
  type "tool_use"    => { id, name, input, caller? } // input = typed JSON
  type "tool_result" => { toolUseId, content, isError, structured? }
  type "interaction" => { requestId, kind, state, prompt, options, action, metadata }
  type "image"       => { ... }

StructuredToolResult  (the `structured?` on tool_result; discriminated on `kind`)
  kind "bash"   => { stdout, stderr, exitCode?, interrupted, isImage? }
  kind "grep"   => { mode, filenames[], numFiles, content, numLines }
  kind "read"   => { filePath, content, numLines, startLine?, totalLines? }
  kind "opaque" => { raw }                            // fallback, never worse than raw
```

The SSE envelope reuses the existing stream event kinds (`message`,
`activity`, `pending`, heartbeat); `format=structured` changes only the
`message` payload to `StructuredMessage`. A `schemaVersion` field on the
envelope lets client and server evolve independently (today's `conversation`
and `raw` have no version).

### Thinking-exposure policy

`conversation` redacts thinking by deliberate policy
(`handler_agent_output_turns.go:46-48`). `format=structured` is itself
opt-in, but to avoid silently reversing that stance for consumers who don't
want reasoning, thinking blocks are gated behind an explicit
`include_thinking=true` query parameter; default omits the `thinking`
block's text (keeping a typed placeholder block so ordering is preserved).
This is a product/safety decision flagged for sign-off, not just a code
toggle.

## Phasing

### Phase 1 — Structured, frame-granular format

**1A. Carrier + schema + stop-flatten (cross-cutting; unlocks 8 families).**

- `internal/sessionlog/entry.go:60-77` — add `Thinking`, `Signature`, and a
  structured `ToolResult` field to `ContentBlock`; map Anthropic `thinking`
  → the new field in `Entry.ContentBlocks()`.
- `internal/worker/types.go:187-210` — add `Usage`, `Model`, `StopReason`
  to `HistoryEntry`; `Thinking` / `Signature` / structured-result to
  `HistoryBlock`.
- `internal/worker/sessionlog_adapter.go:293-368` — decode
  `message.usage` / `model` / `stop_reason` (reuse the cache-aware parser in
  `tail_usage.go`) and populate the new fields instead of only `cloneRaw`.
- `internal/api/` — register `format=structured` on the session stream
  (`huma_handlers_sessions_stream.go`) and transcript
  (`handler_session_transcript.go`) handlers, emitting `StructuredMessage`;
  add the typed wire types + OpenAPI; leave `conversation`/`raw` untouched.
- Live delivery reuses the existing tail/cursor stream machinery — frames
  are emitted structured as they commit (frame-granular streaming).

Outcome: claude + kimi + opencode + mimocode + pi + antigravity emit full
structured blocks immediately. gemini and codex emit structured blocks too,
minus their specific gaps (closed below).

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
- `web_search_call` handling; reasoning `content`-fallback and
  `encrypted_content` → `signature: "encrypted"`;
- `token_count` event → usage; capture `model`.
- extend `codexResponseItem` with `arguments`, `content`,
  `encrypted_content`.

**1C. Gemini gaps** (`internal/sessionlog/gemini_reader.go`).

- add `tokens` → usage (absent today);
- set `tool_result.is_error` from `toolCall.status`;
- handle `type:"error"` messages (silently dropped today);
- add an **incremental parser** so live frame-granular streaming works
  (today it is whole-file `os.ReadFile`).

**1D. Long-tail polish (optional, cheap).**

- add `is_error` to kimi/antigravity tool_results;
- note: usage is absent for all long-tail readers except pi (compaction
  `PreTokens`).

**1E. Golden-fixture test corpus (cross-cutting; do alongside 1A–1C).**

Capture real `format=raw` transcripts per provider/version into a fixture
corpus and snapshot-test the structured producer. A provider CLI bump
becomes "regenerate fixtures + diff." This is the regression guard that
makes one-place server-side translation safe — and is exactly the
discipline consumers reverse-engineering frames never had.

### Phase 2 — Sub-frame streaming (future)

Tap providers' live API output for token/delta-level streaming
(`content_block_delta` / `input_json_delta` equivalents). Materially larger
than Phase 1: it steps outside the transcript-reader model and needs a live
provider-wire tap plus delta-assembly state. Documented here so the schema
(`StructuredBlock` deltas, `schemaVersion`) is designed to admit it later
without a breaking change; not built in this spec.

## Consumer impact

Any consumer that today reads `raw` and re-derives structure can switch to
`format=structured` and **delete its entire client-side translation layer**,
keeping only genuine presentation (markdown→HTML, highlighting, diffs,
visual pairing). The division of labor is explicit:

- **Gas City owns** canonical structured *data* — typed blocks, structured
  tool results, usage, thinking (gated), subagent nesting.
- **Consumers own** presentation.

This is mode-agnostic: a self-hosted supervisor on loopback and a future
remote/managed control plane forwarding streams to many clients both
benefit — a multi-tenant forwarder wants a clean versioned typed format even
more than a loopback one does.

## Risks & open questions

- **Thinking exposure** — `include_thinking` default and whether the gated
  default omits text or the whole block. Needs sign-off (policy, not code).
- **Schema churn** — `StructuredToolResult` kinds (bash/grep/read/opaque)
  are Codex-derived; confirm they generalize before freezing the wire.
  `opaque` guarantees the format is never worse than `raw`.
- **`worker-conformance.md` alignment** — the carrier change extends the
  canonical normalized model that doc designates as core; the new fields
  should land as conformance assertions, not incidental observability.
- **Thin providers (9)** — deferred; a fixture-capture pass decides whether
  they need dedicated readers or already ride the Claude shape.

## Appendix: source facts

- Formats accepted today: only `raw` is branched on; anything else is the
  `conversation` default (`huma_handlers_sessions_stream.go:40,123-137`;
  `handler_session_transcript.go:63`). No `format` enum exists
  (`huma_types_sessions.go:101,110`).
- Flattening: `entryToTurn` / `historyEntryToTurn`
  (`handler_agent_output_turns.go:26-101`).
- Typed block fidelity retained pre-flatten: `ContentBlock`
  (`entry.go:60-77`), `HistoryBlock` (`worker/types.go:200-206`).
- Per-family dispatch: `reader.go:153-172,1283-1303`.
- Tail-only usage: `tail_usage.go:12-98`.
- Codex opacity: `codex_reader.go:254-270,317-327`.
- Gemini whole-file: `gemini_reader.go:18-28`.
- "Honest opacity" rationale: `engdocs/design/worker-conformance.md`
  §4.1–4.2.
