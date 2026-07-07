# S14 — One `launchWorkflow()` Chokepoint: Correctness Contract

Status: DRAFT spec (disposition: needs-julian for one decision, marked D1 below)
Backlog: `engdocs/simplification/backlog.json` id `S14` · Bugs: #1053 (duplicate molecules), #720 (legitimate launch blocked)
Subsystem: `internal/sling` (sling.go, sling_core.go), `internal/graphv2/invocation.go`, `internal/dispatch/drain.go` (shape F only)

---

## 1. The two defects, precisely

**Defect 1 — split duplicate-launch guards.** Three partial mechanisms, none covering all shapes, one of them process-local:

- **G-root**: graphv2 RootKey dedupe — `graphv2.LockKey` (process-LOCAL striped mutex, `invocation.go:545-559`) + `existingGraphV2Root` read-then-create (`sling.go:1439-1470`, `ListByMetadata(Graphv2RootKeyMetadataKey)`), inside `InstantiateSlingFormula` (`sling.go:1262`). Active only when the compiled recipe is a graph workflow AND the input-convoy var is set (`stampGraphV2RootMetadata` no-ops otherwise, `sling.go:1477-1481`). Defeated by CLI+API in two processes: both pass the read, both create → #1053.
- **G-src**: source-workflow file lock — `sourceworkflow.WithLock` keyed on source bead: `withSourceWorkflowLaunchLock` (`sling_core.go:838-948`, legacy path, with ConflictError / force-replace / snapshot-restore / visibility-verify) and `withGraphV2SourceWorkflowLock` (`sling_core.go:950-963`, graph path, lock only — the checks live in the closure). Only for launches attached to a source bead.
- **G-legacy**: `checkLegacySourceWorkflowConflict` (`sling_core.go:1154-1166`) — a second live-roots listing run inside the graph closure, duplicating half of G-src's body.

**Defect 2 — the formula is compiled 2–4× per launch**, each compile from disk with an independently built var map:

1. `prepareGraphV2FormulaInvocation` → `graphv2.PrepareInvocation` → `compileValidationRecipe` (graph formulas; recipe then thrown away).
2. `slingFormula` :253 local `CompileWithoutRuntimeVarValidation` (ready-surface check; standalone shape only).
3. `validateSlingFormulaRuntimeVars` (`sling_core.go:1146-1152`) — compiles again just to validate runtime vars.
4. `InstantiateSlingFormula` (`sling.go:1268`) — compiles again to instantiate.
5. Batch: `isGraphSlingFormula` + `validateBatchSlingFormulaRuntimeVars` compile per child on top of the per-child instantiate compile.

Because `isGraph` can be decided on recipe A while instantiation runs on recipe B (built from a different var map), routing/guard decisions can diverge from what is materialized — the drift class the batch path already patched for its O(N) variant (comment at `sling_core.go:1317-1320`).

## 2. Launch-shape inventory (exhaustive) and current guard per shape

| # | Shape | Entry point | Compiles today | Guard today | Gap |
|---|-------|-------------|----------------|-------------|-----|
| A1 | Standalone `gc sling <formula>`, graph recipe **with** input-convoy var | `slingFormula` (`sling_core.go:241`) | 3 (prepare, :253, instantiate) | G-root only (process-local lock + read-then-create) | cross-process race → **#1053** |
| A2 | Standalone `gc sling <formula>`, non-graph recipe or no convoy var | `slingFormula` | 3 | **NONE** | any concurrent or repeated launch duplicates |
| B | `--on <formula>` on bead, formula declares graph compiler (`UsesGraphCompiler`) | `slingOnFormula` isGraph branch (`sling_core.go:306-345`) | 3–4 (prepare×2-internal, validate, instantiate) | G-src (file lock on source bead) + G-legacy + G-root | RootKey lookup runs under a *source-bead* lock; a concurrent A1 launch with the same RootKey holds a *different* (process-local) lock → cross-process/cross-shape race |
| C | `--on <formula>` on bead, formula does NOT declare graph compiler but recipe compiles to a graph workflow | `slingOnFormula` `run()` → `IsGraphWorkflowAttachment` post-hoc (`sling_core.go:366-372`) | 3 | `CheckNoMoleculeChildren` only. `withSourceWorkflowLaunchLock` at :404 is **unreachable** (the isGraph branch returned at :312) | late-detected graph workflow launches with no workflow lock at all |
| C' | `--on <formula>` on bead, plain legacy wisp | `slingOnFormula` `run()` | 3 | `CheckNoMoleculeChildren` (molecule_id attachment check) | acceptable today (wisp attach is per-bead metadata, second attach fails the check) but check is outside any lock → TOCTOU |
| D | Default formula attach (`slingDefaultFormula`, `sling_core.go:406`) | mirrors B/C/C' | mirrors B/C/C' | mirrors B/C/C' incl. the same dead :510 lock call | same as B/C |
| E | Batch container sling, per child | `DoSlingBatch` → `attachBatchFormula` (`sling_core.go:1074`) | 1 + 2N (validate per child + instantiate per child) | `CheckBatchNoMoleculeChildren` (pre-loop, outside lock) | per-child TOCTOU; graph containers escape to DoSling (shape A/B) before this point |
| F | Drain item root (controller) | `internal/dispatch/drain.go:1005-1067` | 1 | `graphv2.LockKey(ItemRootKey)` (process-local) + `ListByMetadata(ItemRootKeyMetadataKey, IncludeClosed)` lookup-before-create + `closeFailedDrainItemRoots` | process-local lock; safe only under the single-controller assumption |
| — | API surface `Sling.LaunchFormula/AttachFormula/RouteBead` (`sling.go:274-325`) | delegate to `DoSling` | — | inherits A–E | the second *process* of the #1053 race |

Two shapes are the proven #1053 trigger: A1×A1 across processes, and A1×B on the same RootKey (different lock domains).

## 3. Target design

### 3.1 One prepared launch, one compile

```go
// internal/sling — package-private.
type launchShape int // shapeStandalone, shapeAttach, shapeDefaultAttach, shapeBatchChild

type preparedLaunch struct {
    shape       launchShape
    formulaName string
    searchPaths []string
    sourceBeadID string            // "" for standalone
    inv         graphv2.Invocation // Deprecations, InputConvoy, Targeted
    recipe      *formula.Recipe    // THE recipe. Compiled exactly once.
    vars        map[string]string  // THE var map the recipe was compiled with
    isGraph     bool               // UsesGraphCompiler(inv.Formula) || graphroute.IsCompiledGraphWorkflow(recipe)
    opts        SlingOpts          // Title, Force, ScopeKind/Ref, DryRun…
    agent       config.Agent
}

func prepareLaunch(ctx, shape, formulaName, targetID string, opts SlingOpts, deps SlingDeps, a config.Agent) (preparedLaunch, error)
func launchWorkflow(ctx context.Context, p preparedLaunch, deps SlingDeps) (SlingResult, error)
```

- `graphv2.Invocation` gains a `Recipe *formula.Recipe` field; `PrepareInvocation` stops discarding `compileValidationRecipe`'s output and returns it. For non-graph formulas (where PrepareInvocation compiles nothing today) `prepareLaunch` performs the single `formula.CompileWithoutRuntimeVarValidation` itself, using the canonical var map for that shape (graph → `inv.Vars`; legacy → `BuildSlingFormulaVars(...)` — exactly the map the instantiate call uses today, so compiled output is bit-identical to current behavior).
- **`isGraph` is decided on `p.recipe`** — the same object later instantiated. This merges shapes B and C: a late-detected graph workflow (C) takes the graph guard path, closing C's no-lock gap. (This is a deliberate, small behavior *strengthening*: C currently has no guard; it gains one. It is not a loosening anywhere.)
- `InstantiateCompiledSlingFormula(ctx, recipe *formula.Recipe, formulaName string, opts molecule.Options, …)` — body of today's `InstantiateSlingFormula` minus the compile and minus the internal `lockGraphV2Root` (locking moves to the chokepoint). `InstantiateSlingFormula` remains during migration as `compile → InstantiateCompiledSlingFormula` for the two `dispatch`-independent legacy call sites, then is deleted when call-site count reaches zero.
- Runtime-var validation becomes `molecule.ValidateRecipeRuntimeVars(p.recipe, molecule.Options{Title, Vars: p.vars})` inside `launchWorkflow`, **before any store mutation**. `validateSlingFormulaRuntimeVars` is deleted. `validateBatchSlingFormulaRuntimeVars` validates each child's recipe (compiled once per child — the per-child compile is irreducible because vars differ per child; the *extra* validate-only compile is deleted).

### 3.2 One dedupe identity, one lock discipline

```go
// launchIdentity returns the ordered lock/lookup keys for a launch. 0, 1, or 2 keys.
// Order is FIXED and total: [sourceKey?, rootKey?]. Every acquirer uses this order.
func launchIdentity(p preparedLaunch, deps SlingDeps) (sourceKey string, rootKey string)
```

- `sourceKey` — present iff `p.sourceBeadID != ""`: the existing `sourceworkflow` lock identity (`sourceWorkflowLockScope(deps)` + normalized source bead ID). Guards the **one-live-workflow-per-source-bead** invariant; conflicts surface as the existing typed `*sourceworkflow.ConflictError`.
- `rootKey` — present iff `p.isGraph && p.vars[graphv2.ConvoyIDVar] != ""`: `graphv2.RootKey(inputConvoyID, formulaName, vars, scopeKind, scopeRef)` (unchanged function, unchanged fingerprint exclusions for #2941). Guards the **at-most-one-live-root-per-RootKey** idempotency invariant; a live match is an idempotent success (return existing root), not an error — exactly today's `existingGraphV2Root` semantics.
- Both keys are guarded by **`sourceworkflow.WithLock` file locks** (cross-process), acquired in the fixed order source→root, released LIFO. The process-local `graphv2.LockKey` is removed from the sling path (`lockGraphV2Root` deleted). Fixed total ordering ⇒ no deadlock: every launch acquires a subset of `[source, root]` in that order.
- **D1 (needs-julian):** launches with *neither* key (shape A2: standalone, non-graph or no convoy var) have no workload identity. Default in this spec: they remain un-deduped — two identity-free launches are legitimately distinct runs, and refusing them is precisely the #720 failure mode. They still route through `launchWorkflow` (compile-once + validation), so a future opt-in strict mode (fingerprint key `launch:<formula>:<varsFingerprint>:<scope>`) is a one-line key-function change, not a new path. If Julian wants approach (b)'s strict refusal instead, only `launchIdentity` changes.

### 3.3 The chokepoint body (fixed order, all shapes)

```
launchWorkflow(p):
 1. ValidateRecipeRuntimeVars(p.recipe, p.vars)            // no store mutation yet
 2. shape-specific pre-checks with NO store writes          // ready-surface check (A), dry-run exit
 3. sourceKey, rootKey := launchIdentity(p)
 4. WithLock(sourceKey?) {                                  // skip frame if key absent
      WithLock(rootKey?) {
 5.     closeFailedGraphV2RootsByKey(rootKey?)              // failed roots never block (#720)
 6.     live conflicts:
          sourceKey: listSourceWorkflowRoots + workflow_id direct-match (verbatim from
                     withSourceWorkflowLaunchLock) → ConflictError unless Force
                     (absorbs checkLegacySourceWorkflowConflict — DELETED)
          rootKey:   existingGraphV2Root lookup → live match ⇒ idempotent return of the
                     existing root (unless Force ⇒ snapshotGraphV2ReplacementRoot path)
 7.     attachment checks (CheckNoMoleculeChildren[AllowLiveWorkflow]) — now inside the lock
 8.     Force: snapshot blocking state (snapshotBlockingWorkflowState /
               snapshotGraphV2ReplacementRoot — unchanged)
 9.     InstantiateCompiledSlingFormula(p.recipe, …)        // no recompile, no inner lock
10.     Force: close superseded roots; on any failure from here: existing rollback/restore
               (rollbackSourceWorkflowReplacement / rollbackGraphV2ReplacementLaunch — unchanged)
11.     post-launch dispatch by shape:
          graph  → doStartGraphWorkflow(rootID, sourceForAttach, …)
          legacy attach → SetMetadata(molecule_id) + finalize(sourceBeadID)   // route the
                     SOURCE bead — #2848 semantics preserved verbatim
          standalone legacy → finalize(rootID)
12.     sourceKey only: waitForSourceWorkflowLaunchVisible + invariant rollback (verbatim)
    }}
```

`slingFormula`, `slingOnFormula`, `slingDefaultFormula`, and `attachBatchFormula` reduce to `prepareLaunch` + `launchWorkflow` adapters. `DoSlingBatch` keeps its container logic and calls the chokepoint per child. Shape F (drain) keeps its own site in `internal/dispatch` (it cannot import `internal/sling` upward-free in that direction — dispatch already sits beside sling; re-hosting is out of S14 scope) but adopts the same discipline in place: `ItemRootKey` file lock via `sourceworkflow.WithLock` replacing `graphv2.LockKey`, lookup-before-create unchanged. `graphv2.LockKey` is deleted once both consumers are off it.

## 4. Invariants (the correctness contract)

**Side 1 — no duplicate slips through (#1053):**

- **I1** At most one live (non-closed) workflow root exists per RootKey, across all processes and all launch shapes. Two live matches is a hard error (unchanged).
- **I2** At most one live workflow root exists per source bead (absent `--force`), across all processes. Violation attempt ⇒ typed `*sourceworkflow.ConflictError`.
- **I3** Every live-root lookup that gates creation executes between file-lock acquisition and release of the key it gates; the created root's key metadata is stamped in the recipe *before* `molecule.Instantiate` (i.e., visible atomically with creation — `stampGraphV2RootMetadata` order preserved).
- **I4** No launch-path mutual exclusion relies on process-local state. (`grep -rn "graphv2.LockKey\|lockGraphV2Root" internal/sling internal/dispatch` → zero hits at end state.)
- **I5** Lock acquisition order is the fixed total order [sourceKey, rootKey]; no code path acquires them in any other order or holds one while waiting on an unordered third.

**Side 2 — no legitimate launch blocked (#720):**

- **I6** A closed, failed (`molecule_failed=true`), or force-replaced root NEVER blocks a new launch: `closeFailed*` runs before the live lookup; lookups are live-only (`existingGraphV2Root` semantics unchanged).
- **I7** Distinct identities never contend: different formula, different non-reserved vars, different scope, or different source bead ⇒ different key ⇒ no dedupe interaction.
- **I8** `--force` always either fully replaces (old subtree closed + `graphv2_force_replaced` / superseded handling, new root live) or fully rolls back (snapshot/restore) — never a state with zero or two live roots. Existing snapshot/rollback code is moved, not rewritten.
- **I9** Identity-free launches (A2, per D1 default) are never refused by the dedupe layer.
- **I10** A RootKey idempotent hit returns the existing root as *success* (same `molecule.Result` shape as today), never an error.

**Compile-once:**

- **I11** Exactly one `formula.Compile*` invocation per launch attempt (per child for batch). Enforceable via the existing `SlingTracef("instantiate compiled…")`/compile-counter test hook.
- **I12** The recipe that decides `isGraph`, the recipe that is validated, and the recipe that is instantiated are the same `*formula.Recipe` pointer, compiled from the same var map.
- **I13** Runtime-var validation completes before any store mutation (unchanged ordering).

**Repo invariants preserved (unchanged by S14, asserted for review):** zero hardcoded roles (no role names in any new code); typed wire — no new HTTP/SSE surface, no `map[string]any`/hand JSON (all changes are behind existing `DoSling`/`Sling` fronts; `internal/api` stays a projection); typed events — no new event types, existing `RegisterPayload` set untouched; session lifecycle stays behind `internal/worker/handle.go` — S14 touches no session creation, and no `cmd/gc` non-test file gains `session.NewManager(`/`worker.SessionHandle`/`sessionlog` imports; no upward layer imports — `internal/sling` keeps importing `graphv2`/`sourceworkflow`/`molecule` downward only, drain changes stay inside `internal/dispatch`; `config.Agent` field-sync untouched (no new agent fields).

## 5. Current behavior → new form (exact mapping)

| Current | New form |
|---|---|
| `prepareGraphV2FormulaInvocation` returns `(Invocation, isGraph, err)` | returns `preparedLaunch` (Invocation gains `Recipe`); isGraph = declared ∨ compiled |
| `graphv2.PrepareInvocation` discards `compileValidationRecipe` result | returns it in `Invocation.Recipe` |
| `slingFormula:253` ready-surface compile | reads `p.recipe` |
| `validateSlingFormulaRuntimeVars` (compile + validate) ×4 call sites | `molecule.ValidateRecipeRuntimeVars(p.recipe, …)` in chokepoint step 1 — helper deleted |
| `isGraphSlingFormula` (batch, compile) | `p.isGraph` from the child's `preparedLaunch` — helper deleted |
| `InstantiateSlingFormula` (compile inside) | `InstantiateCompiledSlingFormula(p.recipe, …)`; wrapper kept then deleted |
| `lockGraphV2Root` / `graphv2.LockKey` on sling path | file lock on rootKey inside chokepoint step 4 — deleted |
| `existingGraphV2Root` + `closeFailedGraphV2Roots*` inside instantiate | same functions, called by chokepoint steps 5–6 under the rootKey file lock |
| `withGraphV2SourceWorkflowLock` + closure body (B/D graph branches) | chokepoint steps 4–12 with sourceKey; helper deleted |
| `checkLegacySourceWorkflowConflict` | absorbed into step 6 sourceKey conflict check — deleted |
| `withSourceWorkflowLaunchLock` (incl. dead calls at `sling_core.go:404`, `:510`) | body becomes chokepoint steps 4–12; dead call sites deleted after characterization confirms unreachability |
| Shape C (late-detected graph, no lock) | takes the graph guard path (I1–I3 now apply) — deliberate strengthening |
| Drain `graphv2.LockKey(ItemRootKey)` | `sourceworkflow.WithLock` on ItemRootKey, in-place in `internal/dispatch/drain.go` |
| `snapshotGraphV2ReplacementRoot` / rollback helpers / `waitForSourceWorkflowLaunchVisible` / `doStartGraphWorkflow` / `finalize` / #2848 route-the-source-bead comment blocks | moved verbatim, semantics unchanged |

## 6. Migration plan (behavior-preserving, staged, each phase independently green + revertable)

**Phase 0 — pin current behavior.** Characterization table test over shapes A1/A2/B/C/C'/D/E: {first launch, duplicate attempt, relaunch after close, relaunch after fail, force replace, force + instantiate-error rollback, different vars}. Assert *current* outcomes, including A2's unguarded duplicate and C's unguarded launch (documented as pinned-bug tests). Add a compile-counter hook (test-only `formula.SetCompileObserver` or trace-scrape) recording per-launch compile counts (expect 2–4). Prove `sling_core.go:404`/`:510` are dead via a test that would fail if `withSourceWorkflowLaunchLock` executed on those paths.

**Phase 1 — compile once (no guard changes).** `Invocation.Recipe`; `prepareLaunch`; `InstantiateCompiledSlingFormula`; swap `validateSlingFormulaRuntimeVars`/`isGraphSlingFormula`/`:253` compile for the in-hand recipe. Guards untouched (LockKey and all three mechanisms still in place). Phase-0 outcomes byte-identical; compile counter drops to 1 (batch: 1/child). ≤5 files: `internal/graphv2/invocation.go`, `internal/sling/sling.go`, `internal/sling/sling_core.go`, tests.

**Phase 2 — one function, same guards.** Introduce `launchWorkflow` and move the three guard bodies into it verbatim in the fixed order of §3.3; convert `slingFormula`/`slingOnFormula`/`slingDefaultFormula`/`attachBatchFormula` to adapters; delete the dead lock calls. Semantics identical to Phase 1 by construction (code motion); Phase-0 table still passes unchanged, including the pinned-bug rows.

**Phase 3 — one lock discipline (the #1053 fix).** `launchIdentity`; rootKey moves from `graphv2.LockKey` to nested `sourceworkflow.WithLock`; delete `lockGraphV2Root`, `withGraphV2SourceWorkflowLock`, `checkLegacySourceWorkflowConflict`; shape C joins the graph path. Behavior deltas — exactly three, all strengthenings: (1) cross-process same-RootKey races now serialize (second caller gets idempotent existing-root result); (2) shape C gains the source/root guards; (3) attachment checks move inside the lock. Flip the two pinned-bug rows from "documents current gap" to asserting I1/I2. Drain LockKey→file-lock swap rides here or as a trailer commit.

**Phase 4 — D1 decision + cleanup.** If Julian opts into strict identity-free dedupe, extend `launchIdentity`; otherwise close D1 as "default stands". Delete `InstantiateSlingFormula` wrapper and `graphv2.LockKey` once call-site count is zero. Update `docs`/engdocs pointers.

Gates per phase: `make test` (or `test-fast-parallel`), `go vet ./...`, `.githooks/pre-commit`, plus `make test-integration-shards-parallel` for Phases 3–4 (file-lock behavior is integration-tier).

## 7. Test plan

**Duplicate-attempt matrix (Side 1, #1053):**
- T1 A1×A1 same process, concurrent goroutines: exactly one live root; loser receives the winner's root (I10), not an error, not a second root.
- T2 A1×A1 cross-process: integration test (build-tagged) running two `gc sling` subprocesses (or two lock-scope-sharing store handles in separate `exec.Command` children) against one city dir; assert one live root via `ListByMetadata(Graphv2RootKeyMetadataKey)`.
- T3 A1×B same RootKey (standalone convoy launch racing an attached launch): both terminate (I5, bounded by test timeout), one live root (I1).
- T4 B×B same source bead: second gets `*sourceworkflow.ConflictError` (I2), typed assertion via `errors.As`.
- T5 C duplicate (late-detected graph): now guarded — flip of pinned Phase-0 row.
- T6 E: two concurrent batch slings over the same container: per-child attachment check under lock, no child double-attached.
- T7 F: drain item root re-dispatch race: one root per ItemRootKey.
- T8 Multi-match hard error: pre-seed two live roots with the same key; launch fails with the existing "multiple live roots" error.

**Legitimate-relaunch matrix (Side 2, #720):**
- T9 Relaunch after root closed: succeeds, new root.
- T10 Relaunch after `molecule_failed=true`: succeeds; failed subtree closed by step 5.
- T11 Relaunch with different non-reserved var value / different scope / different formula: no interaction (I7).
- T12 `--force` over a live root: replaced; old root `graphv2_force_replaced`; exactly one live root after.
- T13 `--force` with instantiate failure: rollback restores the prior root exactly (snapshot equality assertion); exactly one live root, the ORIGINAL.
- T14 A2 (identity-free) twice: both succeed (I9) — pinned unless D1 flips.
- T15 Reserved-var fingerprint stability (#2941): launches differing only in `ConvoyIDVar`-adjacent legacy vars share a key (existing `varsFingerprint` tests extended to the identity function).

**Compile-once + drift:**
- T16 Compile counter == 1 per launch for shapes A/B/C/D; == N for batch of N children; 0 recompiles inside `InstantiateCompiledSlingFormula`.
- T17 Pointer identity: the recipe instantiated `==` the recipe that decided isGraph (test hook exposes both).
- T18 Var-map canon: for each shape, the vars used to compile equal the vars previously passed to `InstantiateSlingFormula` in Phase 0 (golden comparison), proving no compile-input drift during migration.

**Preservation:**
- T19 `TestOnFormulaAttachesAndRoutes` and the #2848 route-source-bead behavior green, untouched.
- T20 Existing `sourceworkflow` visibility-verify + rollback tests green against the moved code.
- T21 Ready-surface pool error (multi-session agent, non-Ready root) still raised, now from `p.recipe`.

## 8. Risks

1. **Lock-scope mismatch across stores**: `sourceWorkflowLockScope` derives the lock file location from store ref; a rootKey lock taken by a city-store launch and a rig-store launch for the same convoy must resolve to the same lock file or I1 silently degrades to per-scope. Mitigation: rootKey locks always use the *graph store*'s scope (`deps.graphStore()` is the single store both paths instantiate into); T3 runs once per store topology.
2. **Idempotent-return contract drift**: callers of `DoSling` that treat a duplicate as an error today (A1 losing racer previously created a second root "successfully") now receive the winner's root; any caller asserting `result.Created > 0` semantics must be audited (grep `Created` consumers) — audit is part of Phase 3 review.
3. **Dead-code assumption**: if `sling_core.go:404`/`:510` are reachable via a path the Phase-0 probe misses, deleting them changes behavior; Phase 0 must prove unreachability before Phase 2 deletes.
4. **File-lock latency under contention** (batch of N children on one source): fixed-order nested flocks add syscalls per child; measured in Phase 3 (T6 timing budget), acceptable because prior art (G-src) already flocks per launch.
