# S26b — Finish the typed trace surface (pool-cap rejection + remaining outcomes)

**Base:** current `origin/main` (S26 #4036, commit `65899d317`, already merged — typed
`TraceSiteCode`/`TraceReasonCode`/`TraceOutcomeCode` exist, normalize allowlists deleted).
**Scope:** `cmd/gc` only. Behavior-preserving: every RECORDED trace code (the string that
lands in `SessionReconcilerTraceRecord.SiteCode/ReasonCode/OutcomeCode` and in the JSONL
segments) must be byte-identical before and after.

> NOTE: the `simplification` worktree branch predates S26. This change MUST be built on
> `origin/main`, not on the worktree branch state (which still contains the deleted
> normalize functions).

## Target design

Two finishing moves, both pure type-tightening — no logic, ordering, or payload changes.

### (1) Type the pool-cap rejection path (`cmd/gc/pool_desired_state.go`)

Change the signature of the one remaining stringly decision producer:

```go
// BEFORE
func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (string, string, traceRecordPayload, bool)
// AFTER
func (u nestedCapUsage) rejection(req SessionRequest, limits nestedCapLimits) (TraceSiteCode, TraceReasonCode, traceRecordPayload, bool)
```

Inside `rejection`, replace the three literal pairs with the constants that ALREADY exist
and have identical string values (no new constants needed for this half):

| literal returned today            | constant (same value)        |
|-----------------------------------|------------------------------|
| `"reconciler.pool.agent_cap"`     | `TraceSitePoolAgentCap`      |
| `"agent_cap"`                     | `TraceReasonAgentCap`        |
| `"reconciler.pool.rig_cap"`       | `TraceSitePoolRigCap`        |
| `"rig_cap"`                       | `TraceReasonRigCap`          |
| `"reconciler.pool.workspace_cap"` | `TraceSitePoolWorkspaceCap`  |
| `"workspace_cap"`                 | `TraceReasonWorkspaceCap`    |

Same treatment for the sibling producer `newDemandBlockingScope(...)` (feeds
`recordNewDemandCapTrace`, pool_desired_state.go:709): its `site`/`reason` returns become
typed and its `RecordDecision(TraceSiteCode(site), TraceReasonCode(reason), ...)` caller
drops the casts. The empty-string "no rejection / no blocking scope" sentinel stays the
zero value (`TraceSiteCode("")`), preserving `if site == "" { return }` guards.

Callers that only consume the boolean (`canAccept`, the min-fill loop, line 494) are
signature-compatible via `_` discards — no behavior change possible.

### (2) Constant-ize the remaining dynamic OUTCOME strings

Every remaining `TraceOutcomeCode(<dynamic string>)` cast at a record call site is
replaced by either (a) plumbing a typed `TraceOutcomeCode` from the producer, or (b) a
tiny local mapping to existing constants — whichever keeps the diff smallest. Where a
produced string has no existing constant, ADD the constant with the exact current string
value (never rename a recorded value). The full per-site enumeration is in the next
section; the rule is:

- **Producer fields that exist only to be traced** (e.g. `dec.TraceOutcome`,
  `result.outcome`) change type `string` → `TraceOutcomeCode` at the struct field, so the
  compiler enforces the vocabulary end-to-end.
- **Producer strings that are ALSO used for non-trace purposes** (logs, metadata writes)
  keep their string type at the source; the trace call site maps them through a
  constant-returning switch (exhaustive over the values production emits, with the raw
  value passed through unchanged as `TraceOutcomeCode(s)` in the default arm ONLY if the
  value set is provably closed — otherwise add the missing constants and make the switch
  total).

**Non-goals (explicitly out of scope):**
- `TraceReasonCode(<dynamic>)` casts fed by drain/orphan state machines
  (`ds.reason`, `ackReason`, `reason`, `action`, `dec.TraceReason`,
  `sessionpkg.StateFailedCreate`). These are reason-plumbing refactors of the drain
  state machine, a different blast radius. They are enumerated below for completeness
  and left as a documented follow-up (S26c candidate).
- `session_reconciler_trace_cmd.go:417 TraceReasonCode(reason)` — CLI filter parse
  boundary; user input is legitimately dynamic there.
- No changes to record schema, JSONL format, `gc trace` CLI flags/JSON, arms/auto-arm
  logic, or collector internals.

## Current behavior (site-by-site enumeration)

All line numbers are `origin/main` @ the S26 merge lineage (post-`65899d317`).
The complete set of remaining stringly trace conversions in non-test `cmd/gc` code was
found by `git grep -n "TraceOutcomeCode(\|TraceSiteCode(\|TraceReasonCode(" origin/main
-- 'cmd/gc/*.go'` — 33 hits, grouped below.

### Group A — pool-cap rejection path (scope item 1)

**A1. `pool_desired_state.go:637` — `nestedCapUsage.rejection`** (producer).
Returns `(site string, reason string, payload, rejected bool)` with literals:
`("reconciler.pool.agent_cap","agent_cap")`, `("reconciler.pool.rig_cap","rig_cap")`,
`("reconciler.pool.workspace_cap","workspace_cap")`, or `("","",nil,false)`.
→ Returns `(TraceSiteCode, TraceReasonCode, traceRecordPayload, bool)` using
`TraceSitePoolAgentCap/TraceReasonAgentCap` etc. Recorded bytes identical (constants
carry the exact same strings).

**A2. `pool_desired_state.go:460-462` — `applyNestedCaps` reject arm** (consumer).
`trace.RecordDecision(TraceSiteCode(site), TraceReasonCode(reason), TraceOutcomeRejected, ...)`
→ drop both casts; pass typed returns straight through.

**A3. `pool_desired_state.go:494` — `applyNestedCaps` min-fill probe** (consumer,
boolean-only): `if _, _, _, rejected := usage.rejection(...)` → unchanged shape.

**A4. `pool_desired_state.go:629` — `canAccept`** (consumer, boolean-only) → unchanged
shape.

**A5. `pool_desired_state.go:721-750` — `newDemandBlockingScope`** (producer).
Already selects constants but flattens them: returns
`string(TraceSitePoolNewDemandCap), string(TraceReasonAgentCap|RigCap|WorkspaceCap)`;
"no blocking scope" sentinel is `("", "", 0, 0, nil)`.
→ Return `(TraceSiteCode, TraceReasonCode, int, int, []SessionRequest)`; drop the
`string(...)` wrappers; sentinel becomes typed zero values (`""` compares unchanged).

**A6. `pool_desired_state.go:695-718` — `recordNewDemandCapTrace`** (consumer).
`if site == "" { return }` keeps working on the typed zero value. The
`RecordDecision(TraceSiteCode(site), TraceReasonCode(reason), TraceOutcomeRejected, ...)`
casts drop. Payload key `"active_capacity_kind": reason` currently carries a `string`;
with a typed `reason` the payload value becomes `TraceReasonCode` — JSON-identical
(defined-string type marshals as the same JSON string). To keep the payload's Go dynamic
type unchanged for map-equality tests, write it as `string(reason)` explicitly.

### Group B — dynamic OUTCOME casts (scope item 2)

**B1. `build_desired_state.go:369-378` — scale-check exec** (inside
`evaluatePendingPools` goroutine).
`outcome := "success"; if err != nil { outcome = "failed" }` then
`TraceOutcomeCode(outcome)`.
→ `outcome := TraceOutcomeSuccess; if err != nil { outcome = TraceOutcomeFailed }`;
cast drops. Values identical.

**B2. `session_wake.go:632-645` — GC_DRAIN_ACK mutation record.**
Same `"success"`/`"failed"` local-string pattern feeding
`RecordMutation(..., TraceOutcomeCode(outcome), ...)`.
→ same constant swap as B1. (The `fields["reason"] = ds.reason` payload string is
untouched.)

**B3. `session_reconciler.go:1631-1635` — rate-limit preserve decision.**
`result := "held"; if rateLimitErr != nil { result = "hold_deferred" }` then
`TraceOutcomeCode(result)`.
→ `TraceOutcomeHeld` / `TraceOutcomeHoldDeferred` (both exist). Cast drops.

**B4. `session_reconciler.go:1745-1748` — preserve-configured-named resolution.**
`TraceOutcomeCode(map[bool]string{true: "kept_open", false: "resolution_failed"}[desired])`.
→ `TraceOutcomeKeptOpen` exists; **ADD** `TraceOutcomeResolutionFailed TraceOutcomeCode
= "resolution_failed"` (missing). Replace the map-index idiom with an explicit
`outcome := TraceOutcomeResolutionFailed; if desired { outcome = TraceOutcomeKeptOpen }`.

**B5. `session_lifecycle_parallel.go` — `startResult.outcome` (4 trace sites: 2050,
2096, 2114, 2592).**
Producer field `startResult.outcome string` (line 201). Full assignment inventory
(all producers, verified by grep over the file):
`"panic_recovered"` (1228), `"start_error_converged"` (1294),
`"session_initializing"` (1304), `"deadline_exceeded"` (1307), `"canceled"` (1312),
`"success"` (1317), `"provider_error"` (1322, 1334), `"session_exists_converged"`
(1324, 1512), `"session_exists"` (1328, 1330), `"start_enqueued"` (1423).
(`"stale_async_start"` / `"async_start_refresh_failed"` at 1502-1505 are a LOCAL
log-only variable, never stored in `.outcome`; `"timed_out"` / `"force_requested"`
belong to `stopResult.outcome`, which is never traced — both stay strings, out of
scope.)
→ Change the field: `outcome TraceOutcomeCode`. Assignments become constants; **ADD**
three missing constants with identical values:
`TraceOutcomeStartErrorConverged = "start_error_converged"`,
`TraceOutcomeSessionInitializing = "session_initializing"`,
`TraceOutcomeStartEnqueued = "start_enqueued"`.
The four `TraceOutcomeCode(result.outcome)` casts drop. Knock-on edits, all
compile-checked:
- comparisons `result.outcome == "session_initializing"` (1526, 1922, 2602) and
  `== "start_enqueued"` (2597) still compile against untyped string constants, but
  switch them to the new constants for the compiler-enforced vocabulary win;
- `logLifecycleOutcome(w, op, wave, name, template, outcome string, ...)` KEEPS its
  `string` parameter (it also receives log-only ad-hoc strings:
  `"blocked_on_dependencies"` 2483, `"context_canceled"` 2493,
  `reserveAsyncStartSlot` outcomes 2497, `"stale_async_start"` path 1507). Call sites
  passing `result.outcome` add an explicit `string(result.outcome)` conversion. Log
  bytes identical.

**B6. `session_reconciler.go:2898/2903 (max_session_age) and 2979/2987 (idle_timeout)
— `dec.TraceOutcome` / `dec.TraceReason` from `internal/session/lifecycle_timers.go`.**
`TimerDecision.TraceReason/TraceOutcome` are `string` fields in `internal/session`
(Layer 0-1) — they CANNOT become `Trace*Code`, which is defined in `package main`
(no upward import; trace vocabulary is a cmd/gc projection concern).
Reachable value sets (closed — proven by reading `DecideMaxSessionAge`,
`DecideIdleTimeout`, `deferDecision`, and the only `Blocker` producers
`lifecycleTimerBlocker{,Info}` in session_reconciler.go:43-67 which return exactly
`"user_hold" | "quarantine" | ""`):
- `TraceReason ∈ {"max_session_age", "idle_timeout", "user_hold", "quarantine",
  "pending", "assigned_work"}`
- `TraceOutcome ∈ {"stop", "deferred_user_hold", "deferred_quarantine",
  "deferred_pending", "deferred_busy"}` (the `"deferred_"+f.Blocker` composite is
  closed because Blocker is closed).
→ Design: keep `TimerDecision` fields as strings at the layer boundary. In `cmd/gc`,
add ONE total mapping helper next to the reconciler call sites:

```go
func timerTraceCodes(dec sessionpkg.TimerDecision) (TraceReasonCode, TraceOutcomeCode) {
    // identity conversions; the switch exists so the compiler+test pin the vocabulary
    ...exhaustive switch over the closed sets above...
    default: return TraceReasonCode(dec.TraceReason), TraceOutcomeCode(dec.TraceOutcome)
}
```

The default arm is an IDENTITY passthrough (never a rewrite to "unknown" — that was
the S26 bug class), so recorded bytes are provably identical even if the ladder grows
a value before the map does; a unit test walks every ladder output and asserts the
switch is total (default unreachable today).
**ADD** missing constants used by the mapping:
`TraceOutcomeDeferredUserHold = "deferred_user_hold"`,
`TraceOutcomeDeferredQuarantine = "deferred_quarantine"`,
`TraceOutcomeDeferredBusy = "deferred_busy"`,
`TraceReasonMaxSessionAge = "max_session_age"`,
`TraceReasonUserHold = "user_hold"`,
`TraceReasonQuarantine = "quarantine"`.
(Existing: `TraceOutcomeStop`, `TraceOutcomeDeferredPending`, `TraceReasonIdleTimeout`,
`TraceReasonPending`, `TraceReasonAssignedWork`.)
The four call sites become `reason, outcome := timerTraceCodes(dec)` +
`RecordDecision(site, reason, outcome, ...)` — casts drop.

### Group C — remaining REASON-only casts (enumerated; explicitly OUT of scope)

Left as-is (dynamic reason plumbing through drain/orphan state machines; candidate
S26c). For the record, the full list of remaining non-test `TraceReasonCode(...)`
casts after this change:
- `session_reconciler.go:1457,1467` — `TraceReasonCode(action)` (pending-create
  rollback action)
- `session_reconciler.go:1661` — `TraceReasonCode(sessionpkg.StateFailedCreate)`
- `session_reconciler.go:1807,2102` — `TraceReasonCode(ackReason)`
- `session_reconciler.go:1877,1904,1920,1938,1984,3295` — `TraceReasonCode(reason)`
- `session_wake.go:563,575,587,606,663,681` — `TraceReasonCode(ds.reason)`
- `session_reconciler_trace_cmd.go:417` — CLI filter parse (legitimately dynamic;
  permanent)

After S26b, the ONLY remaining `TraceOutcomeCode(...)`/`TraceSiteCode(...)`
conversions in non-test cmd/gc code are the two identity default-arms inside
`timerTraceCodes` and the trace_cmd CLI parse boundary. Everything else is a constant.

## Invariants — the correctness contract

1. **Byte-identical recorded codes.** For every trace record emitted on any path, the
   JSON values of `site_code`, `reason_code`, `outcome_code` (and all payload `Fields`)
   are identical before/after. No new constant may introduce a NEW string value — every
   added constant's value is the exact string production emits today (B4, B5, B6 lists).
2. **No silent rewrites — ever.** No path may map an unrecognized dynamic value to
   `unknown` (the bug class S26 killed). The single permitted dynamic→typed conversion
   (`timerTraceCodes` default arm, trace_cmd CLI parse) is an identity conversion.
3. **Rejection/acceptance decisions unchanged.** `nestedCapUsage.rejection` and
   `newDemandBlockingScope` keep identical predicate logic, evaluation order
   (agent → rig → workspace), payload contents, and boolean results. Only return TYPES
   change. `canAccept`, min-fill, and duplicate-suppression behavior are untouched.
4. **Sentinel semantics preserved.** Empty-string sentinels (`site == ""`) behave
   identically as typed zero values.
5. **Lifecycle log lines unchanged.** `logLifecycleOutcome` output is byte-identical
   (its parameter stays `string`; conversions are explicit at call sites).
6. **Layering.** `internal/session` gains NO dependency on cmd/gc trace types (no
   upward import). `TimerDecision` fields stay `string`. Trace types remain a cmd/gc
   projection concern.
7. **Repo invariants untouched:** zero hardcoded roles; no HTTP/SSE wire or
   `RegisterPayload` surface touched (trace JSONL is a local diagnostic file, not the
   event bus); session lifecycle stays behind `worker.Handle` (no session-creation code
   touched); `config.Agent` fields untouched (no field-sync obligation); cmd/gc remains
   a projection (no domain logic added — only type tightening).
8. **Trace subsystem semantics untouched:** best-effort recording, baseline/detail
   gating, auto-arm triggers, `gc trace` CLI flags/JSON, arms/head/segment file formats
   — all unchanged.

## Behavior-preserving migration/staging

Small enough to land as ONE PR, staged as three self-verifying commits (each compiles,
vets, and passes the trace/reconciler test slice on its own):

**Commit 1 — constants.** Add the 10 missing constants to
`cmd/gc/session_reconciler_trace_types.go` (7 outcomes: `ResolutionFailed`,
`StartErrorConverged`, `SessionInitializing`, `StartEnqueued`, `DeferredUserHold`,
`DeferredQuarantine`, `DeferredBusy`; 3 reasons: `MaxSessionAge`, `UserHold`,
`Quarantine`). Values copied verbatim from the producer literals. Pure addition — zero
behavior change possible.

**Commit 2 — pool-cap path (Group A).** Retype `nestedCapUsage.rejection` and
`newDemandBlockingScope`; drop the casts at A2/A6; `string(reason)` for the
`active_capacity_kind` payload value. Existing `pool_desired_state_test.go` tests that
assert on recorded site/reason strings run unmodified and green (they compare against
the same string values).

**Commit 3 — outcome producers (Group B).** B1-B4 constant swaps; B5 field retype +
comparison-constant swap + explicit `string(...)` at `logLifecycleOutcome` call sites;
B6 `timerTraceCodes` helper + the four call-site rewrites + its exhaustiveness test.

No migration window, feature flag, or data migration: trace JSONL segments are
append-only diagnostics; old and new records are indistinguishable (identical bytes).
Rollback = revert the PR.

Ordering rule inside commit 3: never change a producer literal and its consumer
constant in different commits — each swap is atomic per value so `git bisect` can never
land on a state that records a different string.

## Test plan (incl. -race/parity if applicable)

**New tests:**

1. **Constant-value pinning** (`session_reconciler_trace_types_test.go`): table test
   asserting each of the 10 new constants equals its exact legacy string (guards
   against typo'd values — the one way this change can silently corrupt records).
2. **`TestNestedCapUsageRejectionTyped`** (`pool_desired_state_test.go`): for each cap
   kind (agent/rig/workspace), drive `rejection` over the limit and assert the typed
   returns equal `TraceSitePoolAgentCap`/`TraceReasonAgentCap` (etc.) AND that
   `string(site)`/`string(reason)` equal the pre-change literals
   (`"reconciler.pool.agent_cap"`, `"agent_cap"`, ...).
3. **`TestTimerTraceCodesTotal`** (`session_reconciler_test.go`): enumerate EVERY
   reachable `TimerDecision` from `DecideMaxSessionAge`/`DecideIdleTimeout` (drive all
   `TimerFacts` combinations incl. both blocker values `"user_hold"`/`"quarantine"`);
   assert `timerTraceCodes` (a) hits a named constant (not the default arm — expose via
   a matched bool or by asserting the output is in the known set) and (b) round-trips
   to the exact input strings.
4. **Recorded-bytes parity for one representative site per group**: existing
   trace-cycle test harness (`session_reconciler_trace_test.go` patterns) — record via
   the collector, decode the JSONL, assert `site_code`/`reason_code`/`outcome_code`
   fields equal the legacy strings for: a pool-cap rejection (A), a scale-check failure
   (B1), a max-session-age deferral with blocker `"user_hold"` (B6 — covers the
   composite `deferred_user_hold`).

**Existing tests as the parity oracle (must pass UNMODIFIED except where they name Go
types, not strings):** `pool_desired_state_test.go`, `session_reconciler_trace_test.go`,
`build_desired_state_test.go`, `session_lifecycle_parallel` tests, and
`internal/session/lifecycle_timers_test.go` (untouched — layer unchanged). Any existing
test that needed a STRING expectation edited is a spec violation — stop and re-examine.

**Gates:**
- `go build ./cmd/gc && go vet ./cmd/gc ./internal/session`
- `go test ./cmd/gc -run 'Trace|NestedCap|PoolDesired|Reconcile|Lifecycle' -count=1`
- `go test ./internal/session -count=1`
- `go test -race ./cmd/gc -run 'Trace|Lifecycle'` — B1/B5 outcomes are written inside
  wave/eval goroutines (`evaluatePendingPools` semaphore workers,
  `executePreparedStartWave` workers); the retype must not alter any
  synchronization, and -race proves the constant swap introduced no new shared state.
- `make test` (fast unit baseline) before merge; no dashboard/API surface → no
  `make dashboard-check` needed.
- grep gate (manual or in the PR description): `grep -n "TraceOutcomeCode(" cmd/gc/*.go
  | grep -v _test | grep -v trace_types` returns ONLY the `timerTraceCodes` default arm.

## Top correctness risks

1. **A typo'd constant value silently changes recorded codes.** Ten new constants are
   transcribed from producer literals; one wrong character (e.g.
   `"deferred_userhold"`) records a different string forever and breaks
   `gc trace reasons` groupings without any test failing — UNLESS test 1
   (constant-value pinning) and test 4 (recorded-bytes parity) exist. Mitigation:
   those tests are written FIRST (TDD) against the legacy literals.
2. **The `"deferred_"+Blocker` composite is only closed today.** `timerTraceCodes`
   assumes Blocker ∈ {user_hold, quarantine}. If a future blocker kind is added in
   `lifecycleTimerBlocker*` without updating the map, the identity default arm keeps
   recorded bytes CORRECT (by design), but the vocabulary silently un-types.
   Mitigation: `TestTimerTraceCodesTotal` fails when a new ladder output appears,
   converting silent drift into a red test; the default arm's identity passthrough
   guarantees the failure mode is "untyped but truthful", never "rewritten".
3. **B5 knock-on edits touch live lifecycle logic.** Retyping `startResult.outcome`
   forces edits at ~15 assignment/comparison/log sites in
   `session_lifecycle_parallel.go` (2,700+ LOC, concurrency-heavy). A missed
   comparison site can't miscompile (untyped constants still compare), but an
   accidental `stopResult` conflation or a changed log string could slip through.
   Mitigation: `stopResult` is explicitly out of scope (never traced); log parity is
   pinned by keeping `logLifecycleOutcome`'s `string` signature; the -race lifecycle
   slice plus existing wave tests cover the goroutine paths.
4. **Payload dynamic-type drift (A6).** Putting a `TraceReasonCode` into
   `traceRecordPayload` marshals identically but changes `map[string]any` Go-level
   equality for tests using `reflect.DeepEqual`. Mitigation: explicit `string(reason)`
   at the payload write.
