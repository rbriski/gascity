# bd shim (C2 / `ga-2gap48.9`) — design & status

**Decision made:** the worker-integration model for the graph store is the
**bd shim** (model A in `graph-store-session-handoff.md`), not the prompt-flip
(D3). This doc records the shim's design and what's built.

## What it is

A bd-CLI-compatible **thin client**. When the gc binary is invoked as `bd`
(the PATH install symlinks `bd` → gc), it interprets the bd invocation and
routes a worker's bead operations through the in-process `coordrouter.Router`:
graph-class beads reach the embedded SQLite store, work beads reach the real
bd. So **both raw `bd ...` and `gc bd ...` route with zero prompt changes**
(`gc bd`'s own `exec.LookPath("bd")` resolves the shim).

It ships **default-on as an identity transform**: with `[beads] graph_store`
unset, the Router has a single backend and is byte-identical to raw bd. Only
`graph_store = "sqlite"` diverges graph ops to SQLite (the entire opt-in
boundary, per E1).

## Disposition policy (`classifyBdShimVerb`)

Each bd subcommand is **routed**, **passed through**, or **refused**:

- **route** — translated to an in-process Router store op (graph-aware):
  `close`, `show`, `ready`, `update`, `reopen`, `delete`, plus the gc-only
  `heartbeat` (rewritten to `update … gc.last_heartbeat_at`) and
  `release-if-current`. `ready` and `update` route **only** when their flags map
  cleanly; a `ready` with pool-demand predicates (`--metadata-field`,
  `--unassigned`, `--exclude-type`, …) or an `update` with an unmappable flag
  (`--claim`, `--notes`, `--persistent`, `--unset-metadata`) **passes through**
  instead of silently dropping the filter/effect (byte-identical in the identity
  phase; full predicate parity is C3/`ga-2gap48.11`).
- **passthrough** — execed to the real bd via `GC_BD_REAL` (absolute path,
  never `LookPath`). This is a **CLOSED allowlist**: in the split phase
  (`graph_store=sqlite` active) the known graph-touching-but-unrouted verbs
  (`mol`, `gate`, `query`-ephemeral) are **refused**, not silently passed to the
  work-only bd where they'd miss graph beads (§X2). In the identity phase they
  passthrough byte-identically.
- **refuse** — the split-phase case above.

## Recursion safety (the sharp edge)

The shim, installed as `bd` first on PATH, must never resolve `bd` back to
itself:

- `GC_BD_REAL` pins the real bd by **absolute path**; `resolveRealBdPath` /
  `execRealBd` use it for passthrough — never `LookPath`.
- `ensureRealBdResolvable` prepends `dir(GC_BD_REAL)` to `PATH` so the
  **in-process** work `BdStore`'s bare `bd` exec — including the Router's by-id
  backend probe (`bd show`) — resolves the real bd, not the shim.
- `dispatchBdShimArgv0` **refuses to run as `bd` when `GC_BD_REAL` is unset**,
  rather than recursing on a misinstall.

## Delivered (branch `feat/beads-work-infra-split`)

| Commit | Increment |
| --- | --- |
| `31a9be4ae` | spine — `GC_BD_REAL` passthrough + routed `close` + disposition policy |
| `db7c0b115` | routed reads — `show` + `ready` (C3-safe) |
| `977beaaa0` | routed writes — `update`/`reopen`/`delete` |
| `9c479bd18` | scope (rig vs city) + passthrough env (`bdCommandEnv`) + `heartbeat`/`release-if-current` |
| `adc99f0c1` | argv[0] mux (`gc` invoked as `bd`) + recursion safety |

End-to-end verified: a gc binary symlinked as `bd` dispatches `bd version`
through to a `GC_BD_REAL` fake bd; `bd` with no `GC_BD_REAL` refuses; gc under
its own name is unaffected.

## Key files

- `cmd/gc/cmd_bd_shim.go` — the whole shim: `runBdShim`, `dispatchBdShimVerb`
  (verb→store op), `classifyBdShimVerb` + the `bd*Routable` flag allowlists,
  `resolveRealBdPath`, `execRealBd`, `parseBdReadyQuery`, `parseBdUpdateOpts`,
  `isBdShimInvocation`, `dispatchBdShimArgv0`, `ensureRealBdResolvable`,
  `newBdShimCmd` (hidden `gc bd-shim`).
- `cmd/gc/main.go` — `main()` dispatches to the shim on argv[0]=="bd".
- Reuses `cmd_bd.go` (`extractBdScopeFlags`, `resolveBdScopeTarget`,
  `bdCommandEnv`, `rewriteBdHeartbeatArgs`, `parseBdReleaseIfCurrentArgs`,
  `doBdReleaseIfCurrent`) and `cmd_ready.go`/`cmd_close.go`
  (`writeReadyJSON`, `closeBeadThroughStore`, `closeBeadStoreHandle`).
- Tests: `cmd/gc/cmd_bd_shim_test.go`.

## Manual install (for testing in a real city)

```bash
ln -s "$(command -v gc)" /path/on/agent/PATH/bd
export GC_BD_REAL="$(command -v bd)"   # the real beads CLI, absolute path
# ensure /path/on/agent/PATH precedes the real bd dir
bd ready --assignee "$AGENT" --json     # routes through the in-process Router
```

## Remaining (follow-on beads, not this one)

- **C4 (`ga-2gap48.12`)** — the real install: inject the `bd`→gc symlink onto
  the **agent** PATH (at `template_resolve.go`'s env assembly, alongside
  `prependGCBinDirToPATH`) and set `GC_BD_REAL` in the agent env; plus the
  asset lint that forbids raw mutating bd verbs in pack `.sh`/`.toml`/`.md`.
- **`gc bd` Router-aware** — currently `gc bd` routes via the shim hop
  (`gc bd` → `LookPath("bd")` → shim). Drop its bd-only hard-reject so it routes
  directly without the extra process.
- **dep verbs** — `dep add`/`remove` route cleanly; `dep list --json` returns bd
  *issue* shape (not raw `Dep` records), so its routed output needs the C2a
  shape work. Currently dep is passthrough.
- **Claim actor (C6 / `ga-2gap48.14`)** — `bd update --claim` → `Router.Claim`
  with the correct actor (BdStore env-actor vs SQLite explicit assignee).
- **Silent-fallback exit parity** — passthrough does not yet reproduce
  `gc bd`'s exit-code 4 on bd's on-disk auto-import fallback.
- **C2a (`ga-2gap48.10`)** byte-identity corpus; **C3 (`.11`)** ready predicate
  parity; **X2 (`.19`)** routing `mol`/`gate`/`query` instead of refusing.

## Gotchas

- Reads federate (`router_federation.go`): `Get`/`List`/`Ready`/`DepList` fan
  out across work + graph. The stale comment at `router.go:18-20` (“by-id ops
  left to the primary”) refers to a superseded phase; mutations route by id via
  `router_mutation.go`.
- Ready tier expansion (`TierBoth`) lives in the **policy wrapper above** the
  Router, so the shim must open the store via `openStoreAtForCity`
  (→ `policy(Router(...))`), not a bare Router — which it does.
- The pre-commit `test-fast-parallel` runs 192-way; the timing-fragile
  `TestStartManagedDoltProcessWithOptions_RetryWindowZeroBumpsImmediately`
  flakes under that load (asserts wall-clock <200ms). Unrelated to the shim;
  retry the commit.
