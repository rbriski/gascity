# C2 — `internal/rig` extraction plan (G12)

**Status:** PLAN (pre-code). Grounded against HEAD by a 6-reader pass. Decision 7:
one provisioning path; retire `controllerState.CreateRig`/`initializeRigStoreForCreate`.
**Parent:** `DESIGN-BRIEF.md` §7.2 + `PHASE-2-HANDOFF.md` C2.

---

## 0. Reality check (the brief's "pure mechanical" is optimistic)

`doRigAddWithResult` (`cmd/gc/cmd_rig.go:215-648`, ~430 LOC) is **not** liftable whole.
Grounding facts that shape the plan:

- **~⅓ is CLI presentation** (the `w := func(s){ Fprintln(stdout,…) }` banner at :468 + ~15
  `Fprintf(stderr,…)` sites). None is load-bearing; it must become **structured return
  fields + a push callback**, and `cmd/gc` renders the strings.
- **The heavy deps live in `package main`** and `internal/rig` **cannot import `cmd/gc`**, so
  they MUST be injected as func deps (the `SlingDeps` *pattern*, not its fields):
  - store init: `initDirIfReady(cityPath, dir, prefix) (deferred bool, err)`
    (`beads_provider_lifecycle.go:241`) — already injected on the API side via the
    `controllerStateInitRigDirIfReady` var (`api_state.go:82`); mirror that.
  - pack compose/commit: `ensureBundledRigImportsInstalled(cityPath, imports) ([]BoundImport, commit func() error, err)` (`cmd_rig.go:726`) — its internals (`internal/packman`
    `SyncLock`/`InstallLocked`/`WriteLockfile`, `internal/builtinpacks`) import cleanly, but
    two helpers (`collectAllImportsFS`, `bundledSourcePinnedVersion` — the latter reads the
    binary's `go:embed` packs) are `cmd/gc`-local ⇒ inject.
  - routes: `collectRigRoutes` is pure (moveable); **`writeAllRoutes`/`writeRoutesFile`
    hardcodes `fsys.OSFS{}` + raw `os.*`** (`rig_beads.go:63`) ⇒ inject (or thread `fsys.FS`).
  - post-write side effects (`installBeadHooks`, `hooks.InstallWithResolver`,
    `ResolveFormulas`, `writeBeadsEnvGTRoot`) + controller talk (`rigReloadControllerConfig`,
    `rigWaitForStoreAccessible`) ⇒ inject / run in `cmd/gc` after the core returns.
- **What imports cleanly into `internal/rig`:** `internal/config` (incl. the
  comment-preserving `AppendRigAndWriteSiteBindingsForEdit`, already `fsys.FS`-injected),
  `internal/packman`, `internal/builtinpacks`, `internal/git`. Module path is
  `github.com/gastownhall/gascity`.
- **There is NO git clone today** — the only git op is a read-only
  `git.New(rigPath).ProbeDefaultBranch()`. The `--git-url` clone is **C3/C4**, not C2. C2
  extracts exactly what exists.

## 1. Ordering & atomicity invariants the extraction MUST preserve (byte-identical)

From the 17-step flow (`cmd_rig.go:215-648`), these are load-bearing:

1. **"city.toml written LAST"** (doc :206-209): pack imports are *resolved* early
   (`ensureBundledRigImportsInstalled`) but the **`packs.lock` write is deferred** to a
   `commit()` closure run *after* the city.toml append (:575), and routes (:583) after that.
   Order = beads-init → snapshot → config append → `commit()` packs.lock → routes.
2. **Rollback snapshot taken AFTER beads init, BEFORE the first config write** (:544). It
   restores only *topology files* (city.toml, packs.lock, site.toml, routes canonical,
   dolt-server.port) — it does **not** undo a created store/DB. (C4's clone/DB rollback is
   separate — noted in G13 §6, not C2's job.)
3. **`registerCityDoltConfig` + `defer clearCityDoltConfig`** (:276) is process-global and
   consumed by the later beads-init step; the register/clear pair must stay in one lexical
   scope (do not split across the goroutine boundary — that's a C4 concern, but the extraction
   must keep them paired).
4. **`deferred bool` from `initDirIfReady`** is real control flow (managed-dolt punts live
   init to the controller); preserve the deferred-vs-immediate branch + its messaging.
5. **Surgical comment-preserving append** (`AppendRigAndWriteSiteBindingsForEdit`) for new
   rigs; the re-add branch re-serializes (`writeCityConfigForEditFS`). Collapsing the two
   config writers to the surgical append is explicitly *safer* (append allows unknown keys;
   rewrite's `GuardCityRewriteKeyLoss` was the ga-lurp5d incident) — but the append **cannot
   do in-place edits**, so the re-add path needs handling (keep the rewrite for re-add, or
   teach append to replace). **Decision A below.**

## 2. `RigDeps` (the injected-deps struct — `SlingDeps` pattern)

`internal/sling.SlingDeps` requires only `Cfg/Store/Runner`; everything else is nil-optional
(`validateDeps`, `sling_core.go:40`). Mirror that discipline. Warnings ride back on the
`SlingResult` return struct — but a long-running async rig-add (C4/G20) needs **incremental
progress**, so `RigDeps` adds a **push callback** sling lacks:

```go
// internal/rig
type Deps struct {
    FS       fsys.FS          // required — sling instantiates OSFS inline; we inject it (testable)
    CityPath string           // required
    Cfg      *config.City     // required (loaded by the caller)

    // Injected cmd/gc-resident funcs (nil = fatal for the steps that need them):
    InitStore   func(cityPath, dir, prefix string) (deferred bool, err error) // initDirIfReady
    InitAndHook func(cityPath, dir, prefix string) error                      // initAndHookDir (deferred fallback)
    ComposePacks func(cityPath string, imports []config.BoundImport) (pinned []config.BoundImport, commit func() error, err error) // ensureBundledRigImportsInstalled
    WriteRoutes func(cityPath string, cfg *config.City) error                 // collectRigRoutes+writeAllRoutes
    ProbeBranch func(rigPath string) string                                   // git default-branch (nil = skip)

    // Post-provision side effects (nil = skip) — run inside the core so rollback ordering holds
    // where needed, else the caller runs them after. (hooks / formulas / .env / reload)
    PostProvision func(ctx ProvisionContext) error

    // The divergence from SlingDeps: incremental step/warning PUSH sink for async progress (G20).
    OnStep    func(step ProvisionStep)     // nil = no-op; CLI renders strings, API emits events
}
```

- `Provision(deps Deps, req ProvisionRequest) (config.Rig, ProvisionResult, error)` —
  `req` carries name/path/prefix/defaultBranch/includes/startSuspended/adopt.
  `ProvisionResult` carries the structured warnings/deferred/steps `cmd/gc` renders and the
  API turns into events.
- `validateDeps`-style guard: `FS`, `CityPath`, `Cfg` required; injected funcs required only
  for the steps that use them (document which nil means "skip" vs "fatal").
- The **rollback primitives** (`fileSnapshot`, `snapshotResolvedFile`,
  `snapshotOptionalFile`, `restoreSnapshots`, currently in `cmd_rig_endpoint.go`, **shared
  with `rig set-endpoint`**) — **Decision B below**.

## 3. Retire strategy (Decision 7)

- `internal/rig.Provision` is the single orchestration.
- **`cmd/gc` CLI** (`doRigAddWithResult`): construct `Deps` (all the cmd/gc funcs +
  stderr/stdout renderers over `OnStep`/`ProvisionResult`) → call `Provision`. Signature of
  `doRigAdd`/`doRigAddWithResult` **preserved** so the 22 `TestDoRigAdd_*` tests move verbatim.
- **`cmd/gc` API** (`controllerState.CreateRig`, `api_state.go:1493`): construct `Deps`
  (`InitStore=controllerStateInitRigDirIfReady`, plus a `PostProvision`/commit that runs the
  **`mutateAndPoke` snapshot+refresh+restore + reconciler Poke + `configDirty`** handshake —
  the behavior CreateRig has that doRigAdd lacks, locked by
  `TestControllerStateCreateRigPokesReconciler`/rollback tests) → call `Provision`. **Delete
  `initializeRigStoreForCreate`** (folded into `Deps.InitStore` + the pre-init dup-check).
- **`internal/api`** (`humaHandleRigCreate`): **unchanged** — still calls
  `StateMutator.CreateRig` (`state.go:309`), now a thin adapter over `Provision`. Keeps the
  interface + `fakeMutatorState` test double intact.
- **Consequence (intended):** the API path gains packs/routes/hooks parity it lacks today —
  that IS Decision 7 ("one path"), and the byte-identical local-vs-API artifact test proves it.
  Existing API tests assert a subset (rig ∈ config) that full provisioning still satisfies.

## 4. Sub-phases (each ≤5 files, TDD, `go test ./… && go vet` green before the next)

- **C2.0 — scaffold.** `internal/rig/{doc.go, deps.go (Deps + validateDeps + Provision stub),
  rig_test.go, testenv_import_test.go}` (last via `go run scripts/add-testenv-import.go`).
  Unit-test `validateDeps`. *All-new files; zero blast radius.* Verify `go test ./internal/rig/`.
- **C2.1 — rollback primitives** (Decision B). Land the generic snapshot/restore in
  `internal/rig/rollback.go`; repoint `snapshotRigAddTopologyFiles` + (if shared)
  `rig set-endpoint`. Port `TestSnapshotRigAddTopologyFilesCoversPacksLock`.
- **C2.2 — pure orchestration.** Move steps 1-2/4-10/12/14-17 into `Provision`, calling
  injected deps. Unit tests for prefix-collision, re-add detection, the deferred branch, and
  the "city.toml-last" ordering.
- **C2.3 — repoint CLI.** Rewrite `doRigAddWithResult` to build `Deps` + call `Provision` +
  render. **All 22 `TestDoRigAdd_*` pass byte-identically** (move `writeSchema2RigCity*`
  helpers verbatim); `TestRigAddJSONEmitsOnlySummary` green.
- **C2.4 — repoint API + retire + guards.** Rewrite `controllerState.CreateRig` to build
  `Deps` (with the `mutateAndPoke` commit) + call `Provision`; delete
  `initializeRigStoreForCreate`; add the **byte-identical local-vs-API artifact test**
  (city.toml/packs.lock/routes) + the **import-guard test** (mirror
  `TestGCNonTestFilesStayOnWorkerBoundary`, forbidding `cmd/gc` non-test files from calling
  the moved primitives directly). `api_state_test.go` CreateRig Poke/rollback tests pass.

## 5. Decisions to confirm before coding

**A. Re-add path when collapsing the two config writers.** The surgical append can't do
in-place edits. Options: (A1, recommended) keep the rewrite writer *only* for the re-add
branch and route new-rig adds through the surgical append (status quo minus nothing — the
"collapse" is that `internal/rig` owns the single call site, not that we delete the rewrite);
(A2) teach the append to replace an existing block (larger, riskier, unneeded for Group C).
**Recommend A1** — it satisfies "one provisioning path" without a risky config-writer rewrite.

**B. Ownership of the shared rollback primitives** (`fileSnapshot` et al., shared with
`rig set-endpoint`). Options: (B1, recommended) move them into `internal/rig` and repoint
`set-endpoint` to import them — one home, clean upstream-candidate; (B2) leave them in
`cmd/gc` and have `internal/rig` take snapshot/restore as injected funcs — smaller diff now
but keeps two owners. **Recommend B1.**

**C. Scope of C2 vs C4.** C2 extracts + retires + achieves local provisioning parity
(no clone, sync). C4 adds the `--git-url` clone, the async 202, the request_id machine, and
the clone/DB rollback on top of the extracted `Provision`. C2 does **not** add git-url. Confirm.
