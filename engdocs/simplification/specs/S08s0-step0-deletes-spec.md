# S08s0 — S08 Step-0: pure trace dead-symbol + LegacyArms deletes

Status: spec (correctness contract). Scope note: `S08s0` is not a separate
backlog entry — it is the Step-0 slice of backlog item `S08`, further
narrowed by the executor directive to **trace dead symbols + the LegacyArms
dual field ONLY**. The wire-or-delete judgment for lifecycle-fold,
wake-engine, and convergence is HELD (Julian) and is explicitly out of
scope, as are S08's other Step-0 arms (convergence dead exports,
lifecycle-fold DELETE arm).

Baseline: **current `origin/main`** (12d800e6b, 2026-07-07), i.e. AFTER
S26/#4036 (`65899d317`, "collapse the trace double-record API to one typed
surface"). All line numbers below are against that commit. S26 renumbered
`session_reconciler_trace_types.go` (the evaluation-status const block moved
from ~:222 to :283-291) but did **not** touch any symbol in this delete set —
all four remain present and remain dead on main.

## Target design

One pure-delete, behavior-preserving commit against `origin/main` that
removes four provably-dead trace symbols and the LegacyArms dual-field
JSON compat shim:

1. **`TraceEvaluationCapRejected`** const — deleted.
2. **`TraceEvaluationMissingTemplate`** const — deleted.
3. **`TraceTextBlob`** type + **`NewTraceTextBlob`** constructor — deleted.
4. **`cmdTraceStatus`** wrapper func — deleted (its one test caller is
   repointed to `cmdTraceStatusWithJSON(false, ...)`, preserving the
   text-mode assertion).
5. **`LegacyArms`** field (`json:"arms"`) on `traceStatusJSON`, the
   hand-rolled `(*traceStatusJSON).UnmarshalJSON` compat shim, the
   `LegacyArms:` write in `traceStatusFromState`, and the then-orphaned
   `traceArmsJSONSlice` helper — all deleted. The controller→CLI
   control-socket reply carries `active_arms` only; decoding uses plain
   `encoding/json` struct-tag semantics (no custom methods). Per the
   executor directive this takes the FULL-drop option (stop writing `arms`
   AND drop read tolerance), not the "read-only-tolerate for one release"
   variant; the version-skew analysis justifying that is in Risks.

After the change the trace subsystem has: no unreferenced
`TraceEvaluationStatus` values in the delete set, no unused blob type, one
`gc trace status` entry point, and zero hand-written
`UnmarshalJSON`/dual-key logic on the trace control wire (strengthening the
typed-wire invariant). Net effect: ~55 prod LOC + ~75 test LOC deleted, no
observable behavior change for any supported binary pairing.

Out of scope / MUST NOT touch (explicitly):
- `TraceEvaluationDependencyBlocked` / `TraceEvaluationStorePartial`
  (types.go:285,287) — verified equally declaration-only-dead by the same
  grep proof, but NOT in the backlog's named set. Listed as an optional
  same-proof follow-up; the implementer may include them ONLY as a separate
  commit so the named Step-0 slice stays byte-auditable against the backlog.
- `TraceArmState.Arms` (`json:"arms"`, types.go:483) — a DIFFERENT struct:
  the persisted arm-store state file on disk. Live. Untouched.
- `MissingTemplate bool` / `DependencyBlocked bool` record fields
  (types.go:406-407) — persisted trace-record fields, unrelated to the
  evaluation-status constants. Untouched.
- `TraceReasonStorePartial` / `TraceReasonDependencyBlocked`
  (`TraceReasonCode`s, types.go:141-142) — live (collector.go:854). Untouched.
- Everything in lifecycle fold, wake engine, convergence, and the S26
  recording surface. Untouched.

## Current behavior (site-by-site enumeration)

Each entry: symbol → every reference on `origin/main` (full-repo
`git grep`, all file types, tests included) → deadness proof → exact edit.

### D1. `TraceEvaluationCapRejected` (cmd/gc/session_reconciler_trace_types.go:286)

- References: **1** — the declaration itself. Full-repo grep for
  `CapRejected|cap_rejected` returns only types.go:286. No test, no doc,
  no dashboard, no schema reference.
- Deadness proof: zero readers, zero writers. `TraceEvaluationStatus` is a
  string type; the only production writer of that field is
  city_runtime.go:2412-2415, which assigns only `TraceEvaluationEligible`
  and `TraceEvaluationSkipped`. No `switch` anywhere ranges over
  `TraceEvaluationStatus` (grep for the type name: decl :281, field
  :384, `RecordTemplateSummary` param collector.go:601, two `==
  TraceEvaluationEligible` test comparisons — no switch, no
  exhaustiveness lint to break).
- Persisted-data safety: old trace JSONL records containing
  `"evaluation_status":"cap_rejected"` still parse identically after the
  delete — the field is a string-typed value, not validated against the
  const set anywhere.
- Edit: delete line 286.

### D2. `TraceEvaluationMissingTemplate` (cmd/gc/session_reconciler_trace_types.go:288)

- References: **1** — the declaration. Grep for
  `MissingTemplate|missing_template` hits only: this decl; the UNRELATED
  `MissingTemplate bool json:"missing_template,omitempty"` record field at
  types.go:407 (kept); unrelated prime/session tests
  (main_test.go:6399-6589, config/session_sleep_test.go:276,
  test/acceptance/session_test.go:53). None reference the constant.
- Deadness proof + persisted-data safety: identical to D1.
- Edit: delete line 288.

### D3. `TraceTextBlob` + `NewTraceTextBlob` (cmd/gc/session_reconciler_trace_types.go:323-343)

- References: **3**, all self-contained — type decl :323, constructor decl
  :330, struct literal inside the constructor :332. Full-repo grep for
  `TraceTextBlob`: nothing else. No struct embeds it, no field is typed as
  it, no test constructs it, no JSON on disk carries its shape (nothing
  ever wrote one).
- Deadness proof: a type with no uses and a constructor whose only caller
  set is empty. Compiler confirms: after deleting both, `go build ./cmd/gc`
  must succeed with zero further edits.
- Edit: delete the type + constructor block (:323 through the constructor's
  closing brace, ~:343).

### D4. `cmdTraceStatus` wrapper (cmd/gc/session_reconciler_trace_cmd.go:344-346)

- Definition: `func cmdTraceStatus(stdout, stderr io.Writer) int { return
  cmdTraceStatusWithJSON(false, stdout, stderr) }` — a 1-line
  delegation wrapper.
- References: **2** — the decl, and ONE test call at
  cmd/gc/cmd_trace_test.go:50-51 (inside `TestTraceStartStatusStop`'s live
  flow, asserting text-mode output contains "Head seq: 0" and
  "repo/polecat").
- Deadness proof (production): the only production entry point for
  `gc trace status` is the cobra `RunE` closure in `newTraceStatusCmd`
  (cmd.go:160), which calls `cmdTraceStatusWithJSON(jsonOut, ...)`
  directly. Cobra dispatch is closure-based — no string/reflective dispatch
  could reach `cmdTraceStatus`. Grep confirms no other caller.
- Edit: delete :344-346; change cmd_trace_test.go:50 to
  `cmdTraceStatusWithJSON(false, &stdout, &stderr)` — the text-mode
  assertions at :53-55 are kept verbatim (they now pin the same behavior
  through the surviving entry point).

### D5. LegacyArms dual field + compat shim (cmd/gc/session_reconciler_trace_cmd.go)

Wire context (proved, not assumed): `traceStatusJSON` travels on exactly one
wire — the controller control **unix socket** (same host, same city), inside
`traceControlReply.Status` (cmd.go:38). Producer: controller side,
`applyTraceControlLocal` → `traceStatusFromState` (:728), marshalled with
plain struct tags (no custom `MarshalJSON`). Consumers: CLI side only —
`traceSocketControl` (:750) and `traceSocketStatus` (:772) unmarshal the
reply; the only field reads downstream are `status.ActiveArms` (nil-normalized
at :364-367 of `cmdTraceStatusWithJSON`), `HeadSeq`, `ControllerRunning`,
`ControllerPID`, `AsOf`. **User-facing** `gc trace status --json` emits the
separate `traceStatusResultJSON` (:69-77), which has had `active_arms` only
since #2427 (fd93c22d7, 2026-05-20; CHANGELOG:490) and is pinned by
`schemas/trace/status/result.schema.json` — the `arms` key never reaches
users, scripts, or the dashboard (grep for `"arms"` outside this shim:
only `TraceArmState` disk file + that schema's `active_arms`).

Delete set, with per-site proof:

- **:49** `LegacyArms []TraceArm json:"arms"` field. Readers of
  `.LegacyArms` in Go: only the shim itself (:59-63). Readers of the wire
  key `"arms"` from a reply: only `(*traceStatusJSON).UnmarshalJSON` in
  the SAME repo (backlog: "whose only reader is the same binary");
  compat tests cmd_trace_test.go:187,217,235 assert the shim, they are not
  independent consumers. → delete field.
- **:52-67** custom `UnmarshalJSON` (alias-decode + two mirror-backfill
  branches). Sole purpose is populating/backfilling the deleted field pair.
  → delete method; decoding reverts to stock struct-tag semantics.
- **:739** `LegacyArms: traceArmsJSONSlice(arms),` in
  `traceStatusFromState` — the only write of the field ("stop writing
  'arms'"). → delete line.
- **:743-748** `traceArmsJSONSlice` — callers are exactly :60, :63, :739
  (grep; no test refs), all deleted above → helper becomes dead → delete.
  Note `ActiveArms` at :738 is assigned `arms` directly (never went through
  the helper), so its nil-vs-empty JSON encoding is UNCHANGED by removing
  the helper.
- **Tests OF the shim** (they test the deleted compat path, not live
  behavior): cmd_trace_test.go:187-188 (status reply must contain `"arms"`),
  :217-218 (stop reply must contain `"arms":[]`), and the legacy-only-reply
  decode case around :235-259 (a hand-built reply with only `"arms"` must
  surface in `ActiveArms`). → delete these assertions/case; the surviving
  assertions on `active_arms` in the same tests are kept.

## Invariants — the correctness contract

MUST hold after the change (each with its checking mechanism):

1. **Pure delete / behavior-preserving.** No production code path computes a
   different value for any supported binary pairing (see I6 for the skew
   carve-out). No new symbols, no renames, no logic edits — only removals
   plus the one test-callsite repoint (D4).
2. **User-facing `gc trace status` output byte-identical.** Text mode and
   `--json` mode both flow through `cmdTraceStatusWithJSON` /
   `traceStatusResultJSON`, which this change does not edit. Check: existing
   `TestTraceStartStatusStop` assertions +
   `TestTraceStatusJSONEmptyArmsConformsToSchema` (schema validation against
   `schemas/trace/status/result.schema.json`) pass unmodified (except the
   D4 entry-point repoint).
3. **Control-socket reply semantics preserved for same-version pairs.** A
   same-build CLI↔controller round-trip yields identical `ActiveArms`,
   `HeadSeq`, `ControllerRunning`, `ControllerPID`, `AsOf` before and after
   (the reply merely loses the redundant `"arms"` mirror key that nothing
   same-version reads). Check: the trace control-socket tests minus the
   deleted alias assertions.
4. **Typed wire strengthened, not weakened.** One hand-written
   `UnmarshalJSON` on a wire type is removed; no `map[string]any`,
   `json.RawMessage`, or hand-built JSON is introduced anywhere. Nothing in
   this change touches HTTP/SSE, `internal/api`, or OpenAPI —
   `TestOpenAPISpecInSync` trivially unaffected.
5. **Typed events untouched.** No `events.KnownEventTypes` constant or
   `RegisterPayload` call is added/removed;
   `TestEveryKnownEventTypeHasRegisteredPayload` unaffected.
6. **Version-skew floor is explicit.** After this change the supported skew
   window for the trace control socket is "both sides ≥ #2427 (fd93c22d7,
   2026-05-20)": old-CLI↔new-controller works (old shim backfills
   `LegacyArms` from `active_arms`; old CLI reads `ActiveArms` anyway);
   new-CLI↔old-controller works (old controller dual-writes; new CLI reads
   `active_arms` with stock decoding). Only a pre-2026-05-20 controller
   (bare `"arms"` writer) paired with a post-change CLI degrades — to a
   "0 active arms" display, never an error (missing key → nil → the :364
   normalization yields `[]`). Display-only degradation, ≥7-week-old
   controller, same-host socket, CLI and controller ship in one binary.
7. **Zero hardcoded roles / layering / worker boundary / config field-sync:
   trivially preserved** — no role strings, no cross-layer imports added or
   removed, no session-lifecycle code, no `config.Agent`/`config.Rig`
   fields touched. `TestGCNonTestFilesStayOnWorkerBoundary` and
   `TestAgentFieldSync` unaffected.
8. **Persisted state unaffected.** The arm-store disk file
   (`TraceArmState`, `json:"arms"` at types.go:483) and trace JSONL records
   (including any historical `"evaluation_status":"cap_rejected"` /
   `"missing_template"` values) parse identically — no schema, validation
   set, or decoder for either is edited.
9. **Compiler as deadness oracle.** `go build ./...` and `go vet ./...`
   pass with ONLY the enumerated deletions applied. If any additional edit
   is needed to compile, a reference was missed — stop and re-verify; that
   symbol was then NOT provably dead as specced.

## Behavior-preserving migration/staging

Branch from `origin/main` (NOT from this worktree's docs branch — the
worktree trace files predate S26 and have different line numbers). Two
incremental, individually-compiling commits:

**Commit 1 — dead trace symbols (D1-D4).**
1. Re-run each deadness grep from the enumeration ON THE BRANCH TIP
   (mandatory freshness re-check; main moves fast — S26/S31/S33/S34 all
   landed this week).
2. Delete D1, D2, D3 in `session_reconciler_trace_types.go`; delete D4 in
   `session_reconciler_trace_cmd.go`; repoint cmd_trace_test.go:50 to
   `cmdTraceStatusWithJSON(false, ...)`.
3. `go build ./... && go vet ./...` — must pass with zero further edits
   (Invariant 9).

**Commit 2 — LegacyArms full drop (D5).**
1. In `session_reconciler_trace_cmd.go`: delete field :49, method :52-67,
   write-site line :739, helper :743-748.
2. In `cmd_trace_test.go`: remove the `"arms"` alias assertions
   (:187-188, :217-218) and the legacy-only-reply decode case (:235-259);
   keep every `active_arms` assertion.
3. Build + vet + package tests.

Ordering rationale: Commit 1 is pure unreferenced-symbol removal (zero wire
effect); Commit 2 is the only part with ANY externally visible delta (the
mirror key disappearing from the same-host socket reply), so it is isolated
for trivial revert. No flag, no deprecation window, no data migration:
nothing persisted contains the deleted shapes (Invariant 8), and the
socket-skew analysis (Invariant 6) shows the read-tolerance release is
unnecessary — the "optionally read-only-tolerate for one release" variant
from the S08 proposal is REJECTED for this slice because (a) the executor
directive mandates the full drop, (b) both compat directions that matter
survive without it, and (c) keeping half the shim would leave a custom
UnmarshalJSON whose deletion is the point.

Rollback: `git revert` of either commit restores the exact prior state;
no state written by the new binary is unreadable by the old one.

## Test plan (incl. -race/parity if applicable)

Pure deletes need no NEW tests for deleted code; the plan is (a) prove the
survivors still pin behavior, (b) prove nothing else moved.

1. **Deadness re-proof at branch tip** (pre-edit gate), quoted greps —
   each must return ONLY the lines enumerated above:
   - `git grep -nE 'CapRejected|cap_rejected'`
   - `git grep -nE 'TraceEvaluationMissingTemplate'`
   - `git grep -n 'TraceTextBlob'`
   - `git grep -n 'cmdTraceStatus\b'` (word-boundary: must NOT match
     `cmdTraceStatusWithJSON`)
   - `git grep -nE 'LegacyArms|traceArmsJSONSlice'`
   - `git grep -n '"arms"'` (post-edit: only `TraceArmState` + arm-store
     tests + CHANGELOG remain)
   Per the no-semantic-search rule, these cover direct refs, string
   literals, and test/mock files in one pass since Go has no dynamic
   dispatch reaching these unexported/const symbols; there are no barrel
   files or re-exports in `cmd/gc` (package main).
2. **Targeted package run:**
   `go test ./cmd/gc/ -run 'Trace' -count=1` — surviving trace tests green:
   `TestTraceStartStatusStop` (now via `cmdTraceStatusWithJSON(false,...)`,
   still asserting the text output lines), the JSON-schema conformance test,
   and the control-socket tests minus alias assertions.
3. **Round-trip pin for the socket reply (modify-in-place, not new):** the
   control-socket test that captures `statusPayload` keeps its positive
   assertions (`"active_arms"` present, arm fields correct) and gains the
   inverted one: payload must NOT contain `"arms":` outside `active_arms`
   — pinning "stop writing 'arms'" so the compat key cannot silently
   return.
4. **-race:** `go test ./cmd/gc/ -run 'Trace' -race -count=1`. No new
   concurrency is introduced, but the trace collector tests exercise the
   arm store under the reconciler; -race is cheap insurance that no
   deleted code was, unexpectedly, a synchronization participant (it is
   not — all deleted code is straight-line).
5. **Parity:** not applicable — no dual implementation remains to compare;
   the compat shim IS the thing removed. The version-skew matrix
   (Invariant 6) is analyzed, not tested, because CI cannot build
   pre-fd93c22d7 binaries; the analysis is table-stakes reviewable.
6. **Repo gates:** `make test` (fast baseline), `go vet ./...`,
   `.githooks/pre-commit`. `make dashboard-check` NOT required — no
   `internal/api`/openapi/dashboard paths touched (Invariant 4). Grep
   `schemas/trace/status/result.schema.json` unchanged
   (`git diff --stat` must show only the 2 prod files + 1 test file).

## Top correctness risks

1. **Version-skew on the control socket (the only real behavior delta).**
   A post-change CLI querying a controller older than 2026-05-20
   (pre-#2427, bare-`"arms"` replies) silently shows "Active trace arms: 0"
   instead of the real arms. Mitigation: analyzed bound (Invariant 6) —
   both realistic pairings (old-CLI/new-controller, new-CLI/≤7-week-old
   controller) are proven safe; the degradation is display-only on a
   same-host socket; call it out in the PR body so operators restart
   ancient controllers rather than debug a phantom "arms lost" report.
2. **Wrong-baseline execution.** This spec's line numbers and proofs are
   against `origin/main` @ 12d800e6b (post-S26); the simplification
   worktree still carries pre-S26 trace files where the same symbols live
   at different lines (e.g. the const block at :222 vs :286). Executing the
   edits against the stale tree, or trusting these line numbers after main
   moves again, deletes the wrong lines. Mitigation: the mandatory
   branch-tip grep re-proof (Test plan step 1) is the gate; symbols, not
   line numbers, are the contract.
3. **Adjacent-symbol confusion — three near-collisions exist by design:**
   `TraceArmState.Arms` (`json:"arms"` disk file, LIVE),
   `TraceReasonStorePartial`/`TraceReasonDependencyBlocked` (LIVE reason
   codes with the same string values as the dead evaluation consts), and
   the `MissingTemplate bool` record field. Deleting any of these breaks
   the arm store or S26's typed recording surface. Mitigation: the
   out-of-scope list in Target design names each with its line; the
   `git diff --stat` check (3 files only) catches any stray edit.
4. **Residual risk accepted:** deleting `TraceEvaluationCapRejected`/
   `MissingTemplate` removes vocabulary a future evaluation writer might
   have used; git preserves them, and re-adding a const is one line —
   YAGNI applies. The equally-dead sibling consts
   (`DependencyBlocked`/`StorePartial` evaluation values) are deliberately
   left for a follow-up so this slice matches the backlog's named set
   exactly.
