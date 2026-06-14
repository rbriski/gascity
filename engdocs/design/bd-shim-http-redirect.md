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
- **Phase 3 — DONE (default flipped).** Pure-HTTP is now the **default**: a
  routed verb with no reachable controller errors rather than opening the Router
  locally (`bdShimAllowLocalFallback`). `GC_BD_SHIM_ALLOW_LOCAL` re-enables the
  local fallback as a TRANSITIONAL escape hatch for bootstrap. The startup
  "reorg" the redirect needs is **already in place** for the shim's consumers:
  the supervisor `publishManagedCity` (cmd_supervisor.go:1978) publishes a city's
  beads API BEFORE `cityRuntime.run` (:2206) spawns that city's control-dispatcher
  and agents — so every shim consumer finds the API up (the convergence e2e now
  runs pure-HTTP **by default**, no flag, and converges non-flaky).

## Phase 3 — remaining cleanup (the escape hatch + bootstrap)

Pure-HTTP is the default and proven for the shim's real consumers
(control-dispatcher + agents), whose API is up via the publish-before-spawn
ordering. What remains to delete the `GC_BD_SHIM_ALLOW_LOCAL` escape hatch
entirely (the literal "no local path at all"):

- **Bootstrap** — `gc init` and standalone `bd`/`gc bd` create beads before a
  per-city controller exists. They do NOT use the gc-as-`bd` shim today (real
  `bd`/filebdshim), so the escape hatch is unused in practice; it exists for the
  future where the shim is the universal `bd` (the C4 install). To remove it,
  init/standalone must route through a beads API: either `gc init` ensures the
  supervisor + city registration (then seeds via HTTP), or the supervisor serves
  a per-city beads API for any registered city on demand. The recon-grounded
  smallest reorg: the supervisor already serves per-city routes via one Huma mux
  (`NewSupervisorMux` → `serveCityRequest` → per-city `State`); bring the State up
  for a registered-but-not-fully-started city so its beads API answers before the
  full controller, with partial-startup cleanup.
- **`release-if-current`** (handled before the verb switch, opens the local
  store) and **`create`** (passthrough) need an API path for a fully pure shim.

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
