# M2 — deployed `gc start` city e2e (graph_store=sqlite) — grounded build plan

> **UPDATE — M2 DONE.** The full deployed-city **convergence** is proven by a
> green, non-flaky (3×) integration test:
> `test/integration/graph_store_sqlite_convergence_test.go`
> (`TestGraphStoreSQLiteDeployedCityConverges`, ~3.5s/run). A real `gc start`
> city on disk (`[beads] provider="file"` + `graph_store="sqlite"`, no Dolt)
> with the **gc bd-shim installed as the city's `bd`** and a **no-LLM scripted
> worker** slings a minimal durable graph.v2 formula and the molecule
> **FINISHES** — the control-dispatcher auto-closes the workflow root to
> `gc.outcome=pass` with every graph bead (root + work step + workflow-finalize)
> resident in the on-disk `<scope>/.gc/beads.sqlite`. Two fixes were required to
> make a deployed graph_store=sqlite city converge (both landed with this work):
>
> 1. **gc-N id-namespace collision (the `ga-y5pwx3` slice convergence needs).**
>    The file work store and the SQLite graph store both minted `gc-N`; their
>    independent sequences overlap, and `Router.backendForID` (Get-first-hit,
>    work first) misrouted a worker's `bd close gc-2` of a graph step to the
>    work store's `gc-2`, leaving the graph step open forever. Fix: the graph
>    SQLite store now mints a **distinct `gcg-` prefix**
>    (`graphStoreIDPrefix` → `WithSQLiteStoreIDPrefix` in
>    `cmd/gc/api_state.go:registerGraphStoreBackend`). Guarded by
>    `TestRoutedGraphStoreByIDRoutingSurvivesNumericIDOverlap` in
>    `cmd/gc/api_state_router_test.go`. Note the native Dolt store ALSO defaults
>    to `"gc"`, so this is not a file-provider-only concern — it blocks
>    convergence for any work backend sharing the prefix.
> 2. **Durable-workflow declaration.** The minimal formula must declare
>    `[requires] formula_compiler = ">=2.0.0"` (as `mol-scoped-work` does), NOT
>    the legacy `contract = "graph.v2"`. The legacy form makes `gc sling --on`
>    create an **ephemeral wisp-tier** molecule; with `bd_compatibility` at its
>    `bd-1.0.4` default the controller's discovery does NOT pass
>    `--include-ephemeral`, so it can never see the molecule's control beads.
>    The `formula_compiler` form yields a **durable** workflow in the main
>    (issues) tier where the worker's `bd ready` and the controller's discovery
>    both read.
>
> The shim install (so BOTH the controller's `bd ready` discovery subprocess and
> the spawned worker resolve `bd`→gc-shim) runs the supervisor+controller from a
> per-test `gc` copy and swaps the package-global `gcBinary`, so
> `prependGCBinDirToPATH` fronts the dir holding the `bd`→gc symlink (see
> `newGraphStoreSQLiteShimEnv`); `GC_BD_REAL` points at the filebdshim for
> passthrough. The worker performs ONLY routable mutations
> (`bd update <id> --set-metadata gc.outcome=pass --status closed`) and never
> `--claim`/`gc hook --claim`/`bd mol|gate` (all of which bypass or are refused
> by the shim). The rest of this doc is the original build plan + findings.

**Status:** the deployed-city **pour** is proven by a green, non-flaky (3×)
integration test — `test/integration/graph_store_sqlite_dispatch_test.go`
(`TestGraphStoreSQLiteDeployedCityPour`): a real `gc start` city on disk with
`[beads] provider="file"` + `graph_store="sqlite"` (no Dolt), `gc convoy create`
+ `gc sling worker <convoy> --on=mol-scoped-work`, then asserts the molecule's
graph beads (the `workflow` root **and** the compiler-generated
`workflow-finalize` control bead — 29 graph beads total) are resident in the
on-disk `<scope>/.gc/beads.sqlite`, and that `workflow-finalize` (an
unambiguous graph-only `gc.kind`, immune to the gc-N id collision between the
two stores) is **absent** from the file work store. The remaining DELTA to full
M2 is the molecule *finishing* (controller auto-closes the root) — which needs
the shim-as-city-`bd` install + a worker (below). The hermetic full-chain proof is done
(`internal/dispatch/formula_sling_lifecycle_sqlite_integration_test.go`,
commit `fac171833`) — it proves the formula-sling lifecycle (instantiate →
discover → complete → converge → terminal) keeps every graph create/mutation in
SQLite via the real `molecule.Instantiate`/`dispatch.ProcessControl`/`Router`
code (both hand-built and real-compiler `Cook` variants). This doc is the
ordered plan + empirical findings for the **literal deployed-city** version
(milestone M2, `ga-2gap48.24`): a real `gc start` daemon city on disk with
`[beads] graph_store="sqlite"`, a slung graph.v2 formula, asserting the molecule
finishes with graph beads in the on-disk `<scope>/.gc/beads.sqlite`.

## Empirical findings (verified this session)

1. **Isolation is mandatory.** Running `gc init` / `gc start` outside the
   integration harness's isolated env **pollutes the shared user supervisor**
   (it reconciles other registered cities and tries to install/!overwrite the
   systemd `gascity-supervisor.service` unit). Confirmed by a manual smoke test
   that registered a city in the user's real supervisor (cleaned up via
   `gc city unregister`). **Always use `newIsolatedCommandEnv(t, ...)`** — it
   gives an isolated `GC_HOME`, an isolated supervisor on a reserved loopback
   port, and a shim PATH dir (`<root>/bin`) with `systemctl`/`launchctl` stubs.

2. **graph_store=sqlite is real and wired.** `[beads] graph_store = "sqlite"`
   (`config.BeadsConfig.GraphStore`) flows through the universal chokepoint
   `openStoreResultAtForCity` (cmd/gc/main.go) → `routedPolicyStore`
   (cmd/gc/api_state.go:233) → `registerGraphStoreBackend` →
   `beads.OpenSQLiteStore(<scopeRoot>/.gc, WithSQLiteStoreRetention(0,0))`,
   registered as `coordclass.ClassGraph`. So any in-process gc store op (sling,
   ready, close) on such a city routes graph-class beads to
   `<scope>/.gc/beads.sqlite`. The file work provider creates `<scope>/.gc/beads.json`.

3. **THE CRUX — the city's `bd` must be the gc shim.** The controller's serve
   loop discovers control beads via a `bd ready` **subprocess**
   (`cmd/gc/dispatch_runtime.go` `workflowServeControlReadyQueryForBeads`:
   `bd [--readonly --sandbox] ready --assignee=… --metadata-field gc.run_target=… --unassigned --exclude-type=epic --json --sort oldest`),
   and the scripted worker uses raw `bd` verbs. The convergence engine
   (`ProcessControl`) itself runs **in-process** and is SQLite-aware
   (`runControlDispatcherInStore` → `openControlStoreAtForCity`), so the ONLY
   thing standing between a deployed graph_store=sqlite city and convergence is
   that the `bd` resolved by the controller+worker sees SQLite. The harness's
   `bd` (`bdBinary`) is the **filebdshim** (a file-backed *work-only* stub), so
   it never sees SQLite graph beads. **Fix:** make the city's PATH resolve `bd`
   to the **gc bd-shim** (gc invoked as `bd`), which opens the in-process Router
   and federates SQLite. The shim's C3 commit (`db477621e`) already makes it
   route the controller's discovery predicates (`--metadata-field`/`--unassigned`/
   `--exclude-type` + leading `--readonly`/`--sandbox` global flags).

## Build plan (ordered)

### 1. Isolated no-Dolt city with graph_store=sqlite
Mirror `setupGraphWorkflowCity` (test/integration/graph_dispatch_test.go:170) but
use `newIsolatedCommandEnv(t, false)` (GC_DOLT=skip) and a hand-written city.toml
(fmt.Sprintf), adding `[beads]\ngraph_store = "sqlite"`. Keep
`[session] provider = "subprocess"`, `[daemon] formula_v2 = true,
patrol_interval = "100ms"`, a `[[agent]] worker` + `[[named_session]] template =
"worker" mode = "always"`. `runGCWithEnv(env, "", "init", "--skip-provider-readiness",
"--file", configPath, cityDir)` then `registerCityCommandEnv`, `waitForControllerReady`.

### 2. Install the gc shim as the city's `bd`
In the test: make a bin dir, symlink `bd` → `gcBinary`, **prepend it to the city
env PATH ahead of `integrationToolBinDir`**, and set `GC_BD_REAL=bdBinary` (the
filebdshim — satisfies the shim's recursion guard `dispatchBdShimArgv0` and
handles passthrough work verbs). The controller/worker then resolve `bd` to the
shim, whose routed `ready`/`show`/`update`/`close` reach SQLite. NB: the env that
the controller subprocess inherits is what matters — confirm the PATH override
reaches the spawned controller + worker (passthroughEnv → prependGCBinDirToPATH;
the shim dir must be on the AGENT env, see cmd/gc/template_resolve.go:417-418).

### 3. Worker that needs no LLM and no claim
Write a scripted always-on worker: discover via `bd ready` (now SQLite-aware via
the shim), then complete with `bd update <id> --set-metadata gc.outcome=pass
--status closed` (routed). **Avoid `bd update --claim`** — it is NOT routed yet
(C6 gap: it passes through to the work-only bd and misses the SQLite step). If
the spawn/dispatch path REQUIRES a claim, add claim routing to the shim first:
in `cmd/gc/cmd_bd_shim.go`, route `bd update <id> --claim` to `Router.Claim(id,
assignee)` (internal/coordrouter/router_mutation.go:126) using `BEADS_ACTOR`/
`GC_ACTOR` as the assignee, with a unit test (a `*coordrouter.Router` over
MemStore+SQLite, claim a graph step, assert assignee in SQLite).

### 4. Sling a graph.v2 formula
`bd create` two convoy parts + `gc convoy create … --json` + `gc sling worker
<convoyID> --on=<formula>`. The only built-in graph.v2 formula is
`mol-scoped-work` (heavy: worktrees, 7 steps, 6-min timeout). **Recommend
authoring a minimal graph.v2 formula** (root + one actionable work step; the
compiler adds `workflow-finalize`) staged into the city's pack/formula path, so
the test is fast. The sling's gc process pours the molecule to SQLite via the
Router (by class; graph-apply or store.Create both land in SQLite).

### 5. Assert on the ON-DISK SQLite (not `bd list`)
`bd list` is passthrough (work-only) and won't see SQLite. Instead open the file
directly: `graph, _ := beads.OpenSQLiteStore(filepath.Join(cityScope, ".gc"))`
(package integration already imports internal/beads). Poll `graph.Get(rootID)`
until `Status=="closed" && Metadata["gc.outcome"]=="pass"`; assert the work step
and `workflow-finalize` are also closed there; assert the file work store
(`<scope>/.gc/beads.json`) never held them. (`bd show <id>` is routed and also
reaches SQLite — usable for the root-id lookup.)

## Remaining shim gaps for full convergence
- **Claim (C6)** — `bd update --claim` → `Router.Claim` (only if the worker/spawn
  path requires claiming; otherwise a no-claim worker avoids it).
- Possibly `bd list` / `bd dep list` routing if the harness or worker relies on
  them to observe graph beads (the test itself should read SQLite directly).

## Reliable intermediate (if full convergence proves too flaky)
A **pour-only** deployed test — isolated graph_store=sqlite city + `gc sling` →
assert the molecule's graph beads are resident in the on-disk
`<scope>/.gc/beads.sqlite` — proves the deployed city + real store wiring + real
on-disk SQLite without the worker/controller-convergence timing flakiness. It is
a legitimate deployed-city proof of "graph creates/mutations live in this new
store"; the controller auto-closing the root is the delta to full M2.

## Prerequisites status
- ✅ C2 bd shim (close/show/ready/update/reopen/delete + scope + argv0 mux + recursion safety).
- ✅ C3 ready discovery predicates + leading global flags (`db477621e`) — controller can discover SQLite graph beads.
- ✅ Hermetic full-chain lifecycle proof (`fac171833`).
- ⬜ Claim routing (C6) — only if needed by the worker/spawn path.
- ⬜ The deployed-city integration test itself (this plan) — heavy/iterative against the live daemon.
