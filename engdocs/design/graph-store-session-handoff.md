# Graph-store split — session handoff

**Mission:** relocate the high-churn *graph-class* beads (formula-v2 topology:
molecule/step/gate/scope/run beads, `gc.kind` control beads, wisp roots,
convergence beads, synthetic convoys) off the Dolt-backed work store onto a fast,
embedded, process-local **SQLite** store, behind a per-class **Router**, **opt-in**
via `[beads] graph_store = "sqlite"`. The work backlog (tasks/epics/convoys) stays
on Dolt. End goal: a real working isolated city that runs a formula sling through
the entire process with graph metadata in the in-process store.

**Epic:** `ga-2gap48`. **Plan:** `engdocs/design/graph-store-rollout-plan.md`.
**Backend ADR:** `engdocs/design/graph-store-backend-selection.md`.

---

## TL;DR

The whole **store/engine layer is done and proven**; what's left is the **literal
`gc start` runtime shell** for a *real external worker*.

Every link of the lifecycle has a green test against real production code:

| Link | Mechanism | Where |
| --- | --- | --- |
| pour | `molecule.Instantiate` → Router → SQLite | `internal/molecule/instantiate_router_integration_test.go` |
| discover | `gc ready` → `store.Ready()` federates SQLite | `cmd/gc/cmd_ready_test.go` |
| claim | `Router.Claim` (graph→SQLite explicit assignee, work→bd env-actor) | `internal/coordrouter/router_claim_test.go` |
| close | `gc close` → `Router.Close` → SQLite | `cmd/gc/cmd_close_test.go` |
| converge | `ProcessControl` drives a molecule → terminal in SQLite | `internal/dispatch/dispatch_router_sqlite_integration_test.go` |

So *a formula sling provably runs instantiate → discover → claim → close →
converge → terminal with graph metadata in SQLite* — at the engine level. The
remaining work is wiring a real `gc start` daemon + worker process to use that.

---

## State

- **Branch:** `feat/beads-work-infra-split` (worktree `/data/projects/gascity/.claude/worktrees/ov3`). Tracks `origin/main`.
- **Commits are LOCAL** (not pushed). Remotes are forks (`boylec`, `coreyjewett`); do not push to those. Per `AGENTS.md`, gascity Dolt is local-only — never `bd dolt push/pull/remote add`.
- **All green.** Each commit passed its full pre-commit suite (sharded unit + docsync). Verify quickly:
  ```bash
  cd /data/projects/gascity/.claude/worktrees/ov3
  go build ./... && go vet ./cmd/gc/ ./internal/coordrouter/ ./internal/beads/ ./internal/dispatch/ ./internal/molecule/
  go test ./internal/coordrouter/ ./internal/beads/ ./internal/molecule/ ./internal/dispatch/ -count=1
  go test ./cmd/gc/ -run 'Router|Ready|Close|GraphStore|GcReady|GcClose|Claim|WrapWithCaching' -count=1
  go test ./test/docsync/ -count=1
  ```

### This session's commits (newest first)

```
37bbd2938 feat(gc): gc close — route a bead close through the Router to the graph store
8d0b2fe46 feat(gc): gc ready — graph-store-aware demand probe via the Router (C3)
6ebb38853 test(dispatch): convergence engine drives a graph molecule to terminal in SQLite  (CAPSTONE)
9abb3a724 feat(coordrouter): route by-id Claim through the Router (C6 core)
59e50a6ff test(molecule): formula instantiation pours the graph molecule into SQLite
ea34c091d feat(beads): no-socket worker mediation — every gc process opens the graph Router
5558a8057 feat(beads): opt-in graph_store=sqlite registers a SQLite graph backend (E1)
311b295f7 test(coordrouter): M1 store-level federation proof (graph in SQLite)
640502c5e feat(coordrouter): complete the Router as the controller store (B1b+B2+B3)
f5ae2eeea feat(coordrouter): route tier-selected creates/pours by class (B1a)
6a7c459c2 test(beads): SQLite store passes the shared GraphStore + Classed conformance (A4)
```

---

## Architecture as it stands

**Layering** (per scope; the Router only appears when opted in — default cities are
byte-identical with no Router and zero per-op overhead):

```
CONTROLLER store:  policy( Router( caching(bd-work) , sqlite-graph ) )
WORKER gc process: policy( Router( bd-work        , sqlite-graph ) )
```

- The **Router sits ABOVE the cache** deliberately: the cache reconciles only the
  work backend via a `bd` subprocess, so graph reads must bypass it (else stale
  across the processes that share the graph file).
- **One graph store per context:** `<scope>/.gc/beads.sqlite` for the city and for
  *each* rig (mirrors the per-scope Dolt work stores). The controller and a worker
  operating on the same scope open the **same** file in-process concurrently —
  SQLite **WAL + busy_timeout** makes that safe. This multi-process safety is *why
  no socket is needed* (see decisions).
- **Claim shapes bridged by the Router** (`internal/coordrouter/router_mutation.go`):
  `beads.Claimer{Claim(id, assignee)}` (SQLite, explicit) vs
  `beads.EnvActorClaimer{Claim(id)}` (BdStore, claims for its baked `BEADS_ACTOR`).
  `Router.Claim(id, assignee)` routes by id and calls the right one;
  `beads.ErrClaimUnsupported` if neither. Defined in `internal/beads/claimer.go`.
- **Worker verbs (in-process, Router-routed):** `gc ready` (`cmd/gc/cmd_ready.go`,
  federated `store.Ready()` → JSON `[]beads.Bead`, a drop-in for a `bd ready --json`
  work_query) and `gc close` (`cmd/gc/cmd_close.go`, `store.Close(id)` + optional
  `--outcome`). Both reach SQLite for graph beads because every `gc` process opens
  the Router.

### Key files

- `internal/config/config.go` — `BeadsConfig.GraphStore` (`graph_store` TOML field).
- `cmd/gc/api_state.go` — `wrapWithCachingStore` (caches the work backend, keeps the
  one open graph backend when the incoming store is already a Router),
  `routedPolicyStore` (conditional Router insert), `registerGraphStoreBackend`
  (opens `<scope>/.gc/beads.sqlite` with the retention sweeper **disabled**),
  `graphStoreSQLiteEnabled`, `closeBeadStoreHandle` (peels the Router via
  `Backends()`).
- `cmd/gc/main.go` — `openStoreResultAtForCity` (the universal store chokepoint; now
  wraps via `routedPolicyStore` so *all* gc processes get the Router when opted in).
- `internal/coordrouter/` — `router.go` (New/Register/Backend/Backends/Create/
  GraphApplyHandle), `router_federation.go` (Get/List/Ready/... fan-out + dedup),
  `router_mutation.go` (by-id Update/Close/Dep*/ReleaseIfCurrent/Count/**Claim**).
- `internal/beads/sqlite_store*.go` — the SQLite backend (Tx, graph-apply, Claim,
  CreateWithStorage, conformance). `claimer.go` — claim capability interfaces.

---

## Design decisions (settled — do not relitigate)

1. **No-socket worker mediation.** SQLite is multi-process-safe (WAL), so each `gc`
   process opens the Router (work + SQLite graph) directly in-process. The
   originally-planned controller-served unix socket + Huma store endpoints + thin
   socket client (**C1a/C1b and the socket half of C2**) are **NOT needed** for the
   graph-store goal. User-confirmed.
2. **Router is conditional on `graph_store`.** No Router (and no overhead) for
   default cities — refines B1b's once-unconditional Router. The federation/mutation/
   capability code still runs, only when opted in.
3. **Do not retrofit `BdStore.Claim(id)` to take an assignee.** `bd update --claim`
   has no per-call assignee (only `BEADS_ACTOR` baked into the runner), so a unified
   signature would need a breaking `CommandRunner` change. Route past the gap with
   the two capability interfaces instead.
4. **Graph SQLite opens with the retention sweeper disabled.** N short-lived gc
   processes each sweeping the same file is wrong; controller-owned retention is a
   follow-up (`ga-7hxo6p`).

---

## What remains (for the literal `gc start` city)

> **UPDATE (2026-06-14): option 1 (the full-chain integration test) is DONE.**
> `internal/dispatch/formula_sling_lifecycle_sqlite_integration_test.go` (commit
> `fac171833`, bead `ga-1gyv1m`) chains the WHOLE formula-sling lifecycle —
> instantiate (pour) → discover (`Ready`) → worker complete (mutate + close) →
> controller converge (`ProcessControl` → `workflow-pass`) → terminal — through a
> `Router{work: MemStore, graph: SQLite}` and asserts **every** graph create and
> mutation lands in SQLite, work backend untouched. Two variants: a hand-built
> graph.v2 recipe AND the **real compiler** via `molecule.Cook` (so
> `applyGraphControls` emits the `workflow-finalize` bead). This proves *"a simple
> formula sling runs through the entire process with graph metadata in the
> in-process store"* end to end, not link by link.
>
> What is still NOT done is **option 2**: a literal `gc start` daemon city with a
> real/scripted external worker process. That runtime harness does not exist yet
> (a live `gc start` needs an agent API). It is the remaining heavier item if a
> deployed-city demo (vs. the hermetic engine-level proof) is required.

The store/engine layer and the full-chain lifecycle proof are done. The remaining
work is the runtime shell (option 2) + making an **unmodified real worker** reach
SQLite (the bd shim, now built — see `bd-shim.md` — supplies that worker path).

### UPDATE (2026-06-14): decision made — bd shim built

The worker-integration model is **the bd shim** (option A below). The shim's
verb engine is built and end-to-end verified — see
**`engdocs/design/bd-shim.md`** for design + status + remaining work. Commits
`31a9be4ae`→`adc99f0c1` on this branch. The original analysis below is kept for
context; the remaining shim work (C4 PATH install, gc-bd direct routing, dep,
claim-actor, byte-identity corpus) is tracked in that doc.

### THE KEY DECISION first: worker-integration model (shim vs prompt-flip)

How does a real worker's bead ops reach the Router/SQLite? Prompts/packs today use a
**mix of raw `bd` and `gc bd`** (e.g. `pool-worker.md`/`graph-worker.md`/`mol-*.toml`
use raw `bd close`/`bd update`; `gc-work/SKILL.md` uses `gc bd ...`). Neither routes
through the Router today (`gc bd` just execs the real `bd`). Two models:

- **(A) `bd` shim on PATH (recommended, = C2 `ga-2gap48.9` + C4, no-socket variant).**
  A `bd` executable on the agent's PATH that opens the in-process Router per call and
  routes by id (graph → SQLite, work → real `bd` via `GC_BD_REAL`). Then **both raw
  `bd` and `gc bd` route transparently with ZERO prompt changes** — one chokepoint,
  nothing leaks. Hard part: it must cover the bd CLI surface prompts/work_query use;
  `close`/`ready`/`update`/`show` route cleanly by id, but `bd sql` (the pool-demand
  work_query) resists federation and would pass through to real `bd` (work-only).
- **(B) flip all prompts off raw `bd` (= D3 `ga-2gap48.17`).** Brittle: many sites
  across per-provider overlays + scripts; any missed site silently hits Dolt and
  never sees the graph bead.

**`gc ready` / `gc close` are INTERIM and largely REDUNDANT under the shim.** Nothing
in prompts references them; they were built before this decision as the bounded,
testable verbs of the prompt-flip path and to let a *scripted* worker exercise the
Router-routed discover/close path without first building the full shim. Their logic
(`store.Ready()` render; `store.Close()` + outcome stamp) is reusable by the shim's
`bd ready`/`bd close` handlers. **Decide A vs B before the runtime work; if A, drop
`gc ready`/`gc close` (or demote to convenience) and keep only the shared store-op
helpers.** (This session left the two commands in place — `cmd/gc/cmd_ready.go`,
`cmd/gc/cmd_close.go` — pending that decision.)

### Then the runtime, two ways to push (priority order)

1. **Full-chain integration test** (recommended first; self-contained, no runtime).
   One test that chains the worker verbs + controller engine end-to-end through
   `Router{work, graph:SQLite}`: hand-build (or `molecule.Instantiate`) a minimal
   graph.v2 molecule with an actionable work step → discover it via `store.Ready()`
   → close it via `closeBeadThroughStore` (gc close core) → drive `ProcessControl`
   to converge the molecule → assert terminal in SQLite. The deep part is the real
   molecule's control-bead/convergence structure — study
   `internal/dispatch/dispatch_router_sqlite_integration_test.go` (the capstone,
   which hand-built a retry molecule) and `internal/dispatch/retry_test.go` for the
   `ProcessControl` drive, and `internal/molecule/molecule_test.go:TestCookEndToEnd`
   for `Cook`-ing a real formula.

2. **Literal `gc start` daemon city** (largest; closest to the literal ask).
   Build a runtime e2e: a one-rig city with `[beads] graph_store = "sqlite"`, a
   **scripted/exec worker** (no LLM — set the agent's `work_query = "gc ready"` and
   have it close steps with `gc close <id> --outcome pass`), sling a trivial
   formula, tick the controller loop, assert the molecule finishes with graph beads
   in `<rig>/.gc/beads.sqlite`. **No gc-start e2e harness exists yet** — this is the
   real lift (fake/exec runtime provider wiring + controller daemon lifecycle in
   a test). Running a *live* `gc start` here is uncertain (real agents need an API).

### Deeper backlog (needed for production cities, not the proof)

- **C6 hook rewire** (`ga-2gap48.14`, ◐): `gc hook --claim` still opens a raw
  `*beads.BdStore` (`cmd/gc/cmd_hook_claim.go:301`). Rewire `hookClaimWithBdStore`
  to build a Router (`coordrouter.New(hookClaimBdStore(...))` + `registerGraphStoreBackend`)
  and call `router.Claim`. Needs `cityPath` + `cfg` threaded into `hookClaimOptions`
  (see the `hook-assignee-flow` finding summarized in the rollout plan / the analysis
  below). The Router.Claim mechanism is already done.
- **C2 full bd-shim** (`ga-2gap48.9`): a `bd`-CLI shim (PATH override) that maps an
  *unmodified* `bd ready/close/update/...` to in-process Router ops, so existing
  prompts work without changes. `gc ready`/`gc close` are the narrow verbs; this is
  the transparent path. Hard part: the default work_query is a sophisticated bd shell
  script (`config.Agent.effectiveWorkQuery` → `standardAssignedWorkQueryScript` +
  pool-demand probing), possibly using `bd sql`; federating that is non-trivial.
- **C3 predicate parity** (`ga-2gap48.11`, ◐): `gc ready` is the narrow CLI but does
  NOT yet replicate the full default work_query (pool-demand) byte-for-byte. Fine for
  a proof (set `work_query = "gc ready"`); needed for drop-in production use.
- **D1/D2/D3** (`.15/.16/.17`): make default/classic sling emit a routable graph node
  referencing the work bead by id; deprecate Mode B; flip in-repo prompts off raw bd.
- **X1/X2** (`.18/.19`): route controller `exec=` orders and `bd mol`/`bd gate check`/
  `bd query-ephemeral` through the Router.
- **E0 ChangeFeed** (`ga-2gap48.20`, HARD pre-relocation prereq): the graph SQLite
  store needs a ChangeFeed so the controller's event bus / SSE observe graph bead
  changes. Today graph reads are fresh (no cache) but there's no change *event*.
  Required before flipping `graph_store=sqlite` on by default.
- **E1 cutover** (`ga-2gap48.21`, ◐): registration is done; the on-by-default cutover
  + soak (E2, `.22`) remain.
- **Follow-ups:** `ga-7hxo6p` (controller-owned retention sweeper),
  `ga-y5pwx3` (robust Router id-namespace separation — see Gotchas).

---

## Gotchas / lessons (read before editing)

- **Serialize commits.** The pre-commit hook runs `golangci-lint`, which refuses
  concurrent runs. Run ONE commit at a time; wait for `EXIT=0` AND `git log` HEAD to
  move before the next. Background-command "exit 0" reports the shell wrapper, not the
  commit — always verify `grep EXIT=` in the task output and that HEAD advanced.
- **Never edit tracked files while a background commit hook is running.** Its
  `check-schema`/build can see your working-tree changes and mismatch the staged set.
- **Id-namespace collision (`ga-y5pwx3`).** `MemStore`, `FileStore`, and `SQLiteStore`
  all mint `gc-N` ids. In tests with a Router over a MemStore work backend + SQLite
  graph backend, offset the MemStore: `beads.NewMemStoreFrom(1000, nil, nil)`.
  Production work backends (bd `bd-`, native rig-prefix) differ from SQLite `gc`, so
  this is mainly a test/file-provider concern — but `Router.backendForID` does
  Get-first-hit, so overlapping namespaces are a real latent risk.
- **`bd`/`gc bd` bypass the Router.** `gc hook --claim` opens a raw `*beads.BdStore`;
  `gc bd` execs the external `bd` binary. Those are the worker write paths still to
  mediate (C6 hook rewire, C2 bd-shim).
- **Regenerate docs** after adding a CLI command or config field: `go run ./cmd/genschema`
  (updates `docs/reference/cli.md`, `docs/schema/city-schema.*`, `docs/reference/config.md`),
  else `test/docsync` fails. Handoff/engineering docs go under `engdocs/` (unpublished),
  never `docs/` (must be in the published-site nav).
- **No push.** No appropriate remote for this branch (forks only; `origin` is the
  warned-off doomed remote). Commits are preserved locally for the next session.

---

## Concrete next step (recommended)

Build **option 1 (full-chain integration test)** first — it's the strongest single
artifact and needs no runtime. Then scope **option 2 (gc-start daemon city)**.

```bash
cd /data/projects/gascity/.claude/worktrees/ov3
bd show ga-2gap48          # epic + children
bd ready                   # (note: in a real city this would be the worker probe)
# study these before writing the full-chain test:
#   internal/dispatch/dispatch_router_sqlite_integration_test.go  (capstone pattern)
#   internal/dispatch/retry_test.go                               (ProcessControl drive)
#   internal/molecule/molecule_test.go  (TestCookEndToEnd, TestInstantiateUsesGraphApplyStoreWhenAvailable)
```
