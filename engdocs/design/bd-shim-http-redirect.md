# bd shim → HTTP API redirect — design & status

**Goal (user-stated end state):** the bd shim (`cmd/gc/cmd_bd_shim.go`, gc-as-`bd`)
routes every bead op to the controller's **HTTP API** and **errors if no
controller is reachable** — no local in-process Router fallback. The controller
is the single store owner; every worker is a thin client. This reverses the
"no-socket worker mediation" decision (each gc process opening the Router
in-process) recorded as settled in `graph-store-session-handoff.md`. The user
accepts "reorganizing startup a bit so the beads subsystem is up immediately."

## Why it's viable (grounded)

- The controller's API already exposes the full bead surface
  (`internal/api/huma_handlers_beads.go`): GET `/v0/beads`, `/v0/beads/ready`,
  `/v0/beads/graph/{root}`, `/v0/bead/{id}`, `/v0/bead/{id}/deps`; POST
  `/v0/beads` (create), `/v0/bead/{id}/close|reopen|assign|update`; DELETE
  `/v0/bead/{id}`.
- The controller's **city store has the SQLite graph backend**:
  `newControllerStateOpenCityStore` → `openCityStoreResultAt` →
  `openStoreResultAtForCity` → `routedPolicyStore` (main.go:1234). The bead
  handlers mutate via `s.beadStoresForID(id)`, which includes the Router-wrapped
  city store, so an HTTP `bd close <gcg-id>` reaches SQLite.
  (An earlier recon agent claimed "not viable / feature not merged" — it read the
  wrong worktree (`/worktrees/beads`, not `ov3`). Refuted by
  `TestBeadCloseHandlerReachesSQLiteGraphBackend`.)

## Phases (status)

- **Phase 1 — DONE** (`33eba2008`). `api.Client` write methods: `CloseBead`,
  `ReopenBead`, `DeleteBead`, `UpdateBead` (maps `beads.UpdateOpts` → wire body),
  `ReadyBeads`. Viability test proves the HTTP close handler mutates a SQLite
  graph bead via the Router. `/v0/beads/ready` takes no predicate params, so
  callers post-filter client-side.
- **Phase 2a — DONE** (`313f69301`). `humaHandleBeadReady` now federates
  `CityBeadStore()` (it iterated only per-rig `BeadStores()`), so a single-HQ
  city's ready work is surfaced over HTTP. Guarded by
  `TestBeadReadyFederatesCityStore`.
- **Phase 2b — DONE** (`4206f70d1`). The shim's routed verbs
  (close/reopen/delete/update/show/ready) call the controller HTTP API when a
  controller is reachable. `bdShimAPIClient` prefers a standalone controller and
  otherwise reaches the **supervisor-served** per-city API — `apiClient` (read-path
  CLI, with a local fallback) deliberately does NOT route a supervisor-managed
  city to the supervisor client, so the shim needs its own getter.
  `GC_BD_SHIM_REQUIRE_API` made the shim refuse the local fallback (then gated).
- **Phase 3 — DONE (pure-HTTP, no local path).** The shim routes routed verbs
  AND `release-if-current` through the controller HTTP API only; a routed verb
  with no reachable controller errors. The `GC_BD_SHIM_ALLOW_LOCAL` escape hatch
  and the in-process `dispatchBdShimVerb` local dispatch were **removed**. Safe
  because the supervisor `publishManagedCity` (cmd_supervisor.go:1978) publishes a
  city's beads API BEFORE `cityRuntime.run` (:2206) spawns that city's
  control-dispatcher and agents — so every shim consumer finds the API up (the
  convergence e2e runs pure-HTTP and converges non-flaky). The shim's consumers
  today are agents + the control-dispatcher (a controller is always up); `gc init`
  and standalone `bd`/`gc bd` use the real `bd`/filebdshim, not the shim.

## Remaining (bootstrap, when the shim becomes the universal `bd`)

The shim is now pure-HTTP. The only thing that would break IF the gc-as-`bd` shim
became the universal `bd` (the future C4 install) is **bootstrap**: `gc init` and
standalone `bd`/`gc bd` create beads before a per-city controller exists, and the
shim now requires one.

- To support that, init/standalone must route through a beads API: either
  `gc init` ensures the supervisor + city registration (then seeds via HTTP), or
  the supervisor serves a per-city beads API for any registered city on demand.
  The recon-grounded smallest reorg: the supervisor already serves per-city routes
  via one Huma mux
  (`NewSupervisorMux` → `serveCityRequest` → per-city `State`); bring the State up
  for a registered-but-not-fully-started city so its beads API answers before the
  full controller, with partial-startup cleanup.
- **`release-if-current` — DONE** (`1ce3c77c3`): atomic POST
  `/v0/bead/{id}/release-if-current` + `api.Client.ReleaseBeadIfCurrent`; the shim
  routes it through the API. Reaches SQLite via the Router
  (`TestBeadReleaseIfCurrentHandlerReachesSQLiteGraphBackend`).
- **`create` — DONE** (`b604be74f`): `api.Client.CreateBead` + the shim routes
  `bd create` through the controller's create endpoint (work-bead creation goes
  through the single-owner controller). A create flag the API body cannot express
  (`--ephemeral`/`--no-history`/`--from`/...) passes through to the real bd
  (`bdCreateRoutable`). Graph beads still pour via graph-apply, not `bd create`.
- **C6 — DONE** (`3319b3b12` endpoint + `6ccd23cfd` hook rewire): atomic POST
  `/v0/bead/{id}/claim` + `api.Client.ClaimBead` (`beadPolicyStore.Claim` forwards
  to the Router); `gc hook --claim` routes through it when graph_store=sqlite, so a
  worker's graph-step claim reaches SQLite WITH its explicit assignee. Gated on
  graph_store=sqlite because the work-only BdStore is an EnvActorClaimer (claims
  for its baked actor, not a per-call assignee); non-graph cities keep the
  in-process claim. Verified by `TestBeadClaimHandlerReachesSQLiteGraphBackend`.

The shim's routed verbs are now ALL pure-HTTP. The only remaining local-store
access anywhere in the shim/hook path is the non-graph `gc hook --claim` BdStore
fallback (correct: the work BdStore must use the worker's baked actor).

## Files

- `internal/api/client.go` — bead write-path client methods.
- `internal/api/huma_handlers_beads.go` — ready federates the city store.
- `internal/api/bead_http_graph_store_test.go` — viability + ready-federation +
  client-method tests.
- `cmd/gc/cmd_bd_shim.go` — `bdShimAPIClient`, `bdShimRequireAPI`,
  `dispatchBdShimVerbViaAPI`, the apiClient-first route in `runBdShim`.
- `cmd/gc/cmd_bd_shim_api_test.go` — verb→endpoint mapping.
- `test/integration/graph_store_sqlite_convergence_test.go` — sets
  `GC_BD_SHIM_REQUIRE_API`, so convergence is the pure-HTTP proof.
