# S04b — Table-driven Effective*Query resolver + rehome Agent helpers

Follow-on to S04 (approach "b" from `engdocs/simplification/backlog.json`).
S04's file move merged as `d26a54adb` (#4030): `internal/config/workquery.go`
(719 lines) now holds the bd/jq shell-codegen block, the 7 Effective*Query
triplets, AND a block of session-capacity Agent helpers that rode along.
This item does the real reduction: one table-driven resolver for the 7
query kinds, and rehoming the non-query helpers so workquery.go matches its
name. Strictly behavior-preserving: every generated query string is
byte-identical before and after.

All line numbers below refer to `internal/config/workquery.go` at
`origin/main` (`d26a54adb`).

## Target design

### Part 1 — table-driven resolver (workquery.go)

Replace the 7 private `effective*` functions (7 near-clones of the same
override-check → poolDemandTarget → build-script dance) with one resolver
driven by a table of query-kind descriptors. The 14 exported functions
(`Effective*Query` + `Effective*QueryForBeads`) survive as one-line
wrappers — the exported surface is frozen (S04 invariant: consumers
unchanged).

```go
// queryKind names one of the built-in agent query shapes.
type queryKind int

const (
	queryWork queryKind = iota
	queryAssignedInProgress
	queryAssignedReady
	queryRoutedPool
	queryPoolDemand
	queryOnDeath
	queryOnBoot
)

// querySpec describes how one query kind resolves: which user override
// field short-circuits the default, and how the default script is built.
type querySpec struct {
	// override returns the user-supplied command that replaces the
	// default entirely, or "" when the default applies.
	override func(*Agent) string
	// build returns the default command. includeEphemeralReady carries
	// beads.UsesBD105ReadySemantics(); onDeath/onBoot builders ignore it
	// today and MUST keep ignoring it (see Invariants I6).
	build func(a *Agent, includeEphemeralReady bool) string
}

var queryTable = map[queryKind]querySpec{
	queryWork:               {override: func(a *Agent) string { return a.WorkQuery }, build: buildWorkQuery},
	queryAssignedInProgress: {override: func(a *Agent) string { return a.WorkQuery }, build: buildAssignedInProgressQuery},
	queryAssignedReady:      {override: func(a *Agent) string { return a.WorkQuery }, build: buildAssignedReadyQuery},
	queryRoutedPool:         {override: func(a *Agent) string { return a.WorkQuery }, build: buildRoutedPoolQuery},
	queryPoolDemand:         {override: func(a *Agent) string { return a.ScaleCheck }, build: buildPoolDemandQuery},
	queryOnDeath:            {override: func(a *Agent) string { return a.OnDeath }, build: buildOnDeath},
	queryOnBoot:             {override: func(a *Agent) string { return a.OnBoot }, build: buildOnBoot},
}

// effectiveQuery is the single resolver behind every Effective*Query
// accessor: user override verbatim if set, else the kind's default builder.
func (a *Agent) effectiveQuery(kind queryKind, includeEphemeralReady bool) string {
	spec := queryTable[kind]
	if o := spec.override(a); o != "" {
		return o
	}
	return spec.build(a, includeEphemeralReady)
}

func (a *Agent) effectiveQueryForBeads(kind queryKind, beads BeadsConfig) string {
	return a.effectiveQuery(kind, beads.UsesBD105ReadySemantics())
}
```

The exported wrappers become:

```go
func (a *Agent) EffectiveWorkQuery() string                         { return a.effectiveQuery(queryWork, false) }
func (a *Agent) EffectiveWorkQueryForBeads(b BeadsConfig) string    { return a.effectiveQueryForBeads(queryWork, b) }
// ... same 2-line pattern for the other 6 kinds; all doc comments kept.
```

The 7 `build*` functions are the existing script-assembly bodies moved
VERBATIM out of today's `effective*` functions, minus the override check
(now in the resolver) — no string literal, helper call, or argument-order
change of any kind. All shell/jq helper functions
(`bdReadyPoolDemandShell`, `poolDemandFirstRowFunctionScript`,
`legacyWorkflowControlQualifiedName`, `poolDemandTarget`, etc.) are
untouched.

Non-table accessors stay in workquery.go unchanged: `EffectiveSlingQuery`,
`DefaultSlingQuery` (they resolve a query string), and
`EffectiveScaleCheck` (documented back-compat pass-through to
`EffectivePoolDemandQuery`). They do not join the table: sling has no
ForBeads variant and no includeEphemeral axis; forcing it in adds a
degenerate row for zero dedup.

Net: 7 private near-clones → 1 resolver + 7 table rows + 7 verbatim
builders; the override-dispatch and ForBeads-flag plumbing exists exactly
once. The next routed_to-style migration or override-semantics change
touches one function instead of seven.

### Part 2 — rehome the non-query Agent helpers

S04 moved config.go:3504-4206 wholesale, which dragged along Agent helpers
that build no query. They move (verbatim, same package) to a new
`internal/config/session_capacity.go`, joining the existing
session-concern files (`named_sessions.go`, `session_setup_path.go`,
`session_sleep.go`):

- `EffectiveMaxActiveSessions` (workquery.go:505)
- `EffectiveMinActiveSessions` (:510)
- `SupportsGenericEphemeralSessions` (:519)
- `SupportsMultipleSessions` (:534)
- `UsesCanonicalSingletonPoolIdentity` (:548)
- `SupportsExpandedSessionIdentities` (:561)
- `SupportsInstanceExpansion` (:588)
- `HasUnlimitedSessionCapacity` (:612)
- `ResolvedMaxActiveSessions` (:622)

Two more non-query stragglers also leave workquery.go:

- `DrainTimeoutDuration` (:449) → session_capacity.go (session lifecycle
  knob, not a query)
- `EffectiveDefaultSlingFormula` (:437) → config.go beside the other
  formula-name accessors (it resolves a formula name, not a query)

Same package, so zero import/caller changes anywhere. After the move,
every function in workquery.go either builds a bd/jq/shell query string or
resolves which query string applies.

## Current behavior (site-by-site enumeration)

Each triplet = exported plain accessor → `resolve(false)`; exported
`...ForBeads(beads)` → `resolve(beads.UsesBD105ReadySemantics())`; private
`resolve(flag)`. The table below is the correctness contract per kind; the
new `build*` function must reproduce the "default script" column
byte-for-byte.

### Kind 1 — Work (`EffectiveWorkQuery` :294 / `...ForBeads` :300 / `effectiveWorkQuery` :304)

- Override: `a.WorkQuery != ""` → returned verbatim (flag never consulted).
- Default: `target := a.poolDemandTarget()` (QualifiedName, or PoolName if
  set); `legacyTarget := legacyWorkflowControlQualifiedName(target)`.
  - `legacyTarget == ""` (normal agents): script =
    `standardAssignedWorkQueryScript(flag)` +
    `poolDemandOriginGateScript()` +
    `poolDemandFirstRowFunctionScript(flag)` +
    `probe_pool_demand "$1"; ` + `printf "[]"`, wrapped
    `shellquote.Join(["sh","-c",script,"--",target])`.
  - `legacyTarget != ""` (target is/ends with the control-dispatcher
    qualified name): `legacyControlAssignedWorkQueryScript(flag)` variant
    with TWO probes (`"$1"`, `"$2"`) and args `target, legacyTarget`.
- Flag (`includeEphemeralReady`) consumed by: `bd ready
  --include-ephemeral` insertion, and suppression of the legacy-ephemeral
  jq probes/tiers when true.

### Kind 2 — AssignedInProgress (`EffectiveAssignedInProgressQuery` :331 / :337 / :341)

- Override: `a.WorkQuery` (same field as Kind 1 — a custom WorkQuery owns
  ALL discovery slots; documented at :327-330).
- Default: no positional args. Legacy-control branch selected by
  `legacyWorkflowControlQualifiedName(a.poolDemandTarget()) != ""`:
  - normal: `sh -c` of `standardAssignedInProgressWorkQueryScript(flag)` + `printf "[]"`.
  - legacy: `sh -c` of `legacyControlAssignedInProgressWorkQueryScript(flag)` + `printf "[]"`.
- Note: target feeds ONLY the branch choice; it is not embedded in the
  script (identity comes from `$GC_SESSION_ID/$GC_SESSION_NAME/$GC_ALIAS`
  at runtime).

### Kind 3 — AssignedReady (`EffectiveAssignedReadyQuery` :356 / :362 / :366)

- Override: `a.WorkQuery`.
- Default: exact mirror of Kind 2 with the `...AssignedReady...` script
  bodies (`bd ready --assignee=` tier + optional ephemeral-ready probe,
  suppressed when flag=true).

### Kind 4 — RoutedPool (`EffectiveRoutedPoolQuery` :380 / :386 / :390)

- Override: `a.WorkQuery`.
- Default: `routedPoolWorkQueryCommand(flag, target)` or, when
  legacyTarget non-empty, `routedPoolWorkQueryCommand(flag, target,
  legacyTarget)` — origin gate + `probe_pool_demand` function + one probe
  per positional target + `printf "[]"`.

### Kind 5 — PoolDemand (`EffectivePoolDemandQuery` :476 / :482 / `effectivePoolDemandQuery` :486)

- Override: **`a.ScaleCheck`** — NOT WorkQuery. The only kind with a
  different override field.
- Default: `poolDemandCountShell(a.poolDemandTarget(), flag)` — the
  count-form with NO stderr suppression on the bd calls and `|| exit $?`
  chaining (a failed `bd ready` must surface, not read as "no demand";
  pinned by `TestEffectiveScaleCheckUsesReadyOnly`).
- No legacy-control branch at this level (the migration + legacy-ephemeral
  predicates live inside `poolDemandCountShell` itself).
- `EffectiveScaleCheck` (:499) is a pass-through to
  `EffectivePoolDemandQuery` — unchanged, stays outside the table.

### Kind 6 — OnDeath (`EffectiveOnDeath` :644 / :650 / `effectiveOnDeath` :654)

- Override: `a.OnDeath`.
- Default: unclaim script keyed on `a.QualifiedName()` as assignee (NOT
  poolDemandTarget), with `route` = QualifiedName-or-PoolName (recomputed
  inline at :658-661; identical value to `poolDemandTarget()` — the new
  builder MAY call `poolDemandTarget()` since the strings are provably
  equal, or keep the inline form; either way output is unchanged).
- **Flag is IGNORED**: `_ = includeEphemeralInProgress` at :662. The
  ForBeads variant currently returns the SAME string as the plain variant.
  The builder keeps the `_ =` discard.

### Kind 7 — OnBoot (`EffectiveOnBoot` :689 / :695 / `effectiveOnBoot` :699)

- Override: `a.OnBoot`.
- Default: reopen script with `template` = QualifiedName-or-PoolName
  (again inline, == poolDemandTarget()); shell `template=<quoted>` prefix
  then routed_to / run_target+workflow / legacy-ephemeral reads deduped by
  `awk 'NF && !seen[$0]++'`.
- **Flag is IGNORED** (`_ =` at :707), same contract as Kind 6.

### Cross-kind facts the resolver relies on

- All 7 privates check the override FIRST and return it verbatim — the
  flag, target, and legacy computations are dead when an override is set.
  `poolDemandTarget()` and `legacyWorkflowControlQualifiedName()` are pure,
  so hoisting/keeping the short-circuit order is observationally identical.
- All 7 ForBeads variants derive the flag exclusively from
  `beads.UsesBD105ReadySemantics()`; plain variants pass literal `false`.
- Non-test callers of the 14 exported names: `cmd/gc/`
  (build_desired_state.go, cmd_agent.go, cmd_hook.go, cmd_lint.go,
  cmd_prime.go, cmd_start.go, dispatch_runtime.go, pool.go, prompt.go,
  template_resolve.go, work_query_probe.go), `internal/graphroute`,
  `internal/materialize/mcp_runtime.go`. None change.

## Invariants — the correctness contract

- **I1 — Byte-identical output.** For every kind K, every agent shape A,
  and both flag values: `new(K, A, flag) == old(K, A, flag)` as exact Go
  strings. No re-quoting, no whitespace normalization, no "while we're
  here" script edits.
- **I2 — Frozen exported surface.** All 14 `Effective*Query[ForBeads]`
  names plus `EffectiveScaleCheck`, `EffectiveSlingQuery`,
  `DefaultSlingQuery`, `EffectiveDefaultSlingFormula`,
  `DrainTimeoutDuration`, and the 9 session-capacity helpers keep their
  exact signatures, receivers, and doc comments. Callers in `cmd/gc`,
  `internal/graphroute`, `internal/materialize`, `internal/agentutil`,
  `internal/api` compile without edits.
- **I3 — Override fields stay per-kind.** WorkQuery gates kinds 1-4;
  ScaleCheck gates kind 5; OnDeath gates 6; OnBoot gates 7. Empty-string
  test (`!= ""`, not TrimSpace) is preserved exactly.
- **I4 — Override returned verbatim** — never wrapped, never combined with
  the default, never flag-adjusted.
- **I5 — ForBeads == plain + `UsesBD105ReadySemantics()`.** The only
  difference between the two exported forms of a kind is the flag source.
- **I6 — OnDeath/OnBoot flag-blindness is load-bearing.** Their ForBeads
  variants intentionally equal the plain variants today. The refactor must
  NOT start honoring the flag there; that would be a behavior change
  smuggled in as cleanup.
- **I7 — nil-receiver semantics unchanged.** The session-capacity helpers
  that guard `if a == nil` keep those guards through the file move. (The
  query resolvers have no nil guard today; do not add one.)
- **I8 — Project invariants.** Zero hardcoded roles (builders stay keyed
  on beadmeta constants and `ControlDispatcherAgentName` — an existing SDK
  infrastructure constant, not a role; do not touch it). No wire types, no
  events, no session-lifecycle paths, no new imports into
  `internal/config`, no upward imports; `cmd/gc` and `internal/api` remain
  projections; `config.Agent` fields untouched so `TestAgentFieldSync` and
  patch/override apply functions are unaffected.
- **I9 — Same package, no file renames of shared helpers.** All moves are
  within `internal/config`; git history stays traceable
  (`git log --follow`).

## Behavior-preserving migration/staging

Three commits on one branch (`simplify/s04b`), each independently green.

**Commit 1 — parity oracle FIRST (test-only).** Add
`internal/config/workquery_parity_test.go` containing:
1. Verbatim copies of the seven current private functions renamed
   `oldEffectiveWorkQuery`, `oldEffectiveOnDeath`, … (test-file copies of
   today's bodies — the frozen oracle).
2. `TestEffectiveQueryParity`: a matrix of agent shapes × both flag values
   asserting `Effective*Query…` == the old copy, for all 7 kinds × plain
   and ForBeads forms (14 assertions per shape). Agent-shape matrix (each
   dimension exercised because each changes the output string):
   - plain agent (`Name`, no pool)
   - `PoolName` set (poolDemandTarget switch)
   - name == `ControlDispatcherAgentName` (legacy branch, bare)
   - qualified `rig/`+ControlDispatcherAgentName suffix (legacy branch,
     prefixed)
   - each override field set (`WorkQuery`, `ScaleCheck`, `OnDeath`,
     `OnBoot`) → verbatim passthrough, including for the ForBeads forms
   - override field set to `""` explicitly (default applies)
   - flag=false and flag=true (`BeadsConfig` stub driving
     `UsesBD105ReadySemantics`)
   At this commit the test trivially passes (new == old == same code);
   its value is that commit 2 must keep it green.
3. Golden pinning: `TestWorkQueryGolden` writing/comparing the generated
   shell for a canonical agent per kind × flag against
   `internal/config/testdata/workquery/*.golden` (update via the repo's
   usual `-update` flag convention). This survives after the oracle copies
   are eventually deleted and catches FUTURE accidental drift.

**Commit 2 — the table.** Introduce `queryKind`, `querySpec`,
`queryTable`, `effectiveQuery`, `effectiveQueryForBeads`; convert the 14
exported functions to wrappers; move the seven default-script bodies
verbatim into `buildWorkQuery` … `buildOnBoot`; delete the seven private
`effective*` functions. Zero edits to any string literal or helper. Parity
+ golden tests from commit 1 must pass unchanged, as must the 228 existing
references in `internal/config/config_test.go`.

**Commit 3 — rehome.** `git mv`-style extraction: cut the session-capacity
block (:449-458 DrainTimeoutDuration, :503-639 capacity helpers) from
workquery.go into `internal/config/session_capacity.go`, and
`EffectiveDefaultSlingFormula` (:437-445) into config.go. Pure text move,
same package, comments intact. Compile + full config package tests.

Optional follow-up commit (NOT this item): delete the `old*` oracle copies
once the change has soaked; the goldens remain as the permanent pin.

Rollback story: each commit reverts cleanly and independently; commit 2 is
the only one with semantic surface, and it is bracketed by the oracle
committed before it.

## Test plan (incl. -race/parity if applicable)

1. **Parity (the proof).** `TestEffectiveQueryParity` (commit 1, above):
   7 kinds × {plain, ForBeads} × {flag on/off} × the agent-shape matrix →
   `got == want` on exact strings against the frozen old-body copies.
   This is the "each of the 7 kinds → byte-identical query" requirement.
2. **Goldens.** `TestWorkQueryGolden` pins the literal generated shell per
   kind × flag × {normal, pool, legacy-control} in
   `internal/config/testdata/workquery/`. Review of commit 2 shows zero
   golden churn — that IS the byte-identity evidence in the PR diff.
3. **Existing suite.** `go test ./internal/config/` (config_test.go holds
   228 references to these accessors, incl.
   `TestEffectiveWorkQuerySkipsEpicLeafScenario`,
   `TestEffectiveScaleCheckUsesReadyOnly`) must pass with zero test edits
   in commits 2 and 3. Any needed test edit = a behavior change = stop.
4. **Table completeness.** `TestQueryTableCoversAllKinds`: every declared
   `queryKind` constant has a table row with non-nil `override` and
   `build` (guards against a future kind added to the enum but not the
   map, which would panic via nil `spec.override` at runtime).
5. **-race.** `go test -race ./internal/config/` — `queryTable` is a
   package-level map initialized at init and only read afterward; the race
   detector confirms no test mutates it. (No new concurrency is
   introduced; this is belt-and-braces because package-level maps invite
   test-time mutation.)
6. **Consumer sweep.** `make test` (fast baseline) + `go vet ./...`;
   grep-verify no non-config file referenced the deleted private names
   (they are unexported, so compile already proves it) and that
   `cmd/gc`, `internal/graphroute`, `internal/materialize`,
   `internal/agentutil`, `internal/api` build with zero diffs.
7. **Rehome check.** After commit 3, `grep -n 'MaxActiveSessions\|Supports\|DrainTimeout'
   internal/config/workquery.go` returns nothing; workquery.go contains
   only query builders + resolvers.

## Top correctness risks

1. **Silent script drift while "moving verbatim" (highest).** The builder
   bodies are dense single-quoted shell/jq with embedded `beadmeta`
   constants; a one-character slip (a lost trailing space in a `+` chain,
   a re-quoted argument) changes runtime behavior for every worker and
   reconciler in the fleet. Mitigated by committing the parity oracle
   BEFORE the refactor and by golden files making any byte change visible
   in the diff.
2. **Flag semantics accidentally "fixed" for OnDeath/OnBoot.** They ignore
   `includeEphemeral*` today (`_ =` discard), so ForBeads == plain. A
   well-meaning cleanup that threads the flag into their ephemeral reads
   is a behavior change to crash-recovery hooks — the worst place to
   discover one. Pinned by parity cases asserting
   `EffectiveOnDeathForBeads(bd105) == EffectiveOnDeath()` (and same for
   OnBoot).
3. **Override-field crosswiring in the table.** Kind 5's override is
   `ScaleCheck` while kinds 1-4 share `WorkQuery`; a copy-paste table row
   that gates PoolDemand on WorkQuery (or AssignedReady on ScaleCheck)
   compiles fine and only breaks cities using those overrides. Pinned by
   per-kind override cases in the parity matrix, including the
   "WorkQuery set but ScaleCheck empty" cross-shape.
4. **Legacy control-dispatcher branch divergence.** Kinds 1-4 each branch
   on `legacyWorkflowControlQualifiedName`; the table refactor must keep
   the per-kind branch INSIDE each builder (they differ: 2-arg probe vs
   no-arg script vs variadic). Hoisting the branch into the shared
   resolver would be wrong for kinds 5-7. Pinned by the two legacy-name
   shapes in the matrix.
5. **Rehome collides with in-flight branches.** workquery.go/config.go are
   hot (this fork + upstream churn); moving 200 lines to a new file will
   conflict textually with open branches touching those helpers. Pure-move
   commit 3 is kept mechanically trivial and separate so conflicts resolve
   by re-running the move.
