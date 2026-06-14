---
title: "Graph Store Rollout Plan"
---

| Field | Value |
|---|---|
| Status | Proposed |
| Date | 2026-06-14 |
| Author(s) | Claude Opus 4.8 |
| Relates to | `graph-store-backend-selection.md` (the backend), `beads-work-infra-split.md` (the seam + P0-P5), `beads-dolt-contract-redesign.md` (governance) |

## Goal (the testable end state)

Graph-class beads (the formula-v2 execution explosion) live in a fast embedded
SQLite store; every other class stays on beads-backed-by-Dolt; **every** bead
mutation — including from arbitrary user shell (`scale_check`/`work_query`/prompts)
— is routed and cached through one in-process control point, with **no path to an
unmediated `bd` mutation** in a managed shell. Success is measured by a
`doctor_fork_rate` soak: the fork storm gone, the recovered p99 targets met
(FilterScan ≤10ms, Ready ≤10ms, point-read ~1ms), with byte-identical behavior.

## Architecture (settled — see the two related design docs)

- **One object model** (`beads.Bead`) + **two physical stores** (Dolt for work/
  messaging/sessions/orders/nudges; embedded SQLite for graph), split by
  `coordclass.Classify` behind the `coordrouter.Router`.
- **Two caller classes, two mediation paths:**
  1. **gascity's own Go code** (the default demand scan / hook oracle, all Go
     callers) → the **in-process Router** directly. No shell, no spawn.
  2. **User-supplied shell** (custom `scale_check`/`work_query`, prompts, packs,
     formulas) → cannot be internalized (ZFC). Its `bd` resolves to a **thin
     client** to the controller's long-lived, cached, **routed** store over a
     local socket. The flexibility is preserved; correctness and perf come from
     *what `bd` resolves to*, not from constraining what users write.
- **Total mutation enforcement** is at binary resolution: in managed shells `bd`
  is the thin client; the Dolt-bound `bd` is unreachable; a lint guard forbids raw
  `bd` in prompts/packs/Go/scripts. The Router is **the** canonical write path
  (not a parallel control plane).
- **Opt-in boundary (load-bearing).** The *mediation layer* — the Router (as an
  identity transform), the thin `bd` client/shim, `gc ready`, and PATH enforcement
  — ships **default-on**. It is byte-identical to today (every class still routes
  to bd) but always mediated, cached, and observable; the shim is *not* opt-in. The
  *storage relocation* — registering a **non-bd backend** for a class (e.g.
  `ClassGraph`→SQLite) — is the **only opt-in part**: a config flag, default off,
  reversible by a one-line re-register. So the whole pipeline can ship and run in
  production with graph still on Dolt; flipping the flag is what moves graph to
  the fast store.

## Workstreams

Each task ends in a test. `[x]` = landed.

### A. Graph backend (the SQLite store behind GraphStore)
- [x] **A1** Recover `internal/beads/sqlite_store.go` (baseline, green).
- [x] **A2** `ApplyGraphPlan` / `ApplyGraphPlanWithStorage` (atomic pour; white-box green).
- [ ] **A3** Atomic `Claim(id, assignee)` — CAS-if-unassigned (idempotent for the same assignee, conflict for another), the acquire-dual of `ReleaseIfCurrent`, serialized by the store's single write connection. *Test: single-winner under concurrent claims; idempotent self; conflict; not-found.* (The routed-read filter moves to B2, since it touches the shared `ReadyQuery` and the federation, not the store alone.)
- [ ] **A4** Conformance: run `coordtest.RunGraphStoreTests` + `RunClassedStoreTests(ClassGraph)` against the SQLite store (green); recover `internal/benchmarks/coordstore/` harness + the Dolt-baseline soak; re-establish p99 targets on current hardware. *Test: conformance green; bench numbers recorded.*

### B. Router mediation (in-process) — `beads-work-infra-split.md` P1–P2
- [ ] **B1** Re-point `wrapStoreWithBeadPolicies` → `coordrouter.Router`, register every class to the same cs-wrapped bd store (identity transform). *Test: full existing suite + a byte-identical differential test for a full graph.v2 pour.*
- [ ] **B2** Read federation: `Router.List/Ready/Get/DepList` fan out across backends. *Test: `union(work,graph) == legacy` over an all-classes fixture.*
- [ ] **B3** **Per-id by-class mutation routing** (the correctness keystone): `Router` resolves a by-id op's class (`Get`→`Classify`) and routes `close/update/dep/claim` to the owning backend. *Test: a mutation to a graph-id lands in SQLite, a work-id in Dolt; no split-brain.*
- [ ] **B4** Extend `beads.ReadyQuery` with the routed predicate (`gc.routed_to` / `run_target` / `exclude-type`) so a backend's `Ready` filters by route (the routed-read from A3); then `RouterReady = union(work.Ready, graph.Ready[routed])` as the single Go oracle, and route the controller demand scan (`build_desired_state.go`) + the default oracle (`pool.go`, `cmd_hook.go`) through it **in-process** (kills the per-tick controller fork). *Test: `RouterReady == legacy bd ready` byte-identical across branches.*

### C. Mediated `bd` + controller-served store (the per-tick + enforcement) — P3
- [ ] **C1** Controller serves the routed+cached store over a **local unix socket** (extend the supervisor socket / add a store mux); add bead-**write** methods to `api.Client`. *Test: a client round-trips create/get/update/close over the socket.*
- [ ] **C2** Thin `bd` client → socket: bd-CLI-arg → store-op translation preserving bd's output + **exit-code contract** (incl. the silent-fallback code); Dolt-specific ops pass through to `bd.real` by absolute path (no recursion). *Test: the existing raw-bd == gc-bd == store equivalence tests pass against the thin client.*
- [ ] **C3** `gc ready` narrow CLI — byte-identical predicate to `bdReadyPoolDemandShell` across routed / run_target-fallback / ephemeral, resolving through `RouterReady`. *Test: `gc ready == bd ready` across all branches.*
- [ ] **C4** **PATH enforcement**: managed-shell `bd` (`.gc/system/bin/bd`) = thin client; Dolt-bound `bd` unreachable; CI lint guard forbids raw `bd` in prompts/packs/Go/scripts. *Test: a managed shell cannot reach Dolt `bd`; lint fails on a planted raw `bd`.*
- [ ] **C5** **Benchmark (risk gate)**: thousands of concurrent thin-client reads over the socket against the cached store. *Test: throughput/latency holds; documents the funnel ceiling.*

### D. Graph-only claim surface (workers touch only graph nodes) — claim-surface section
- [ ] **D1** Default sling + classic `--on` emit a routable **graph node** (the wisp-root shape) referencing the work bead by id — the net-new "adopt an existing bead as a graph node" primitive. *Test: a bare/`--on` sling stamps `gc.routed_to` on a graph node, never the work bead.*
- [ ] **D2** Deprecate Mode B: gastown polecat/dog/crew get a default graph formula; warrant beads become graph nodes. *Test: no bare `ClassWork` bead is pool-claimable.*
- [ ] **D3** Flip prompts/packs off raw `bd ready`/mutations to the mediated path. *Test: prompt-render test asserts nothing emits raw `bd`.*

### E. Storage relocation (the payoff) — P4–P5
- [ ] **E1** Register the SQLite `GraphStore` for `ClassGraph` behind a config flag (default off — **this flag is the entire opt-in boundary**; everything in A–D ships default-on as an identity transform). *Test: flag off → byte-identical to today (graph on Dolt); flag on → graph beads land in SQLite, the rest in Dolt.*
- [ ] **E2** `doctor_fork_rate` soak vs the recovered p99 targets = ship gate; flip default on. *Test: fork storm gone; p99 met; split-topology `is_blocked` green.*

## Testable milestones (the "so we can test it" path)

- **M1 — store-level federation proof** (needs A3, A4, B1–B3). A Go integration
  test: a Router over `{work: bd, graph: SQLite}` — graph beads route to SQLite,
  work to Dolt, reads federate, a by-id mutation lands in the correct store. **No
  production wiring; the earliest end-to-end test that the split works.**
- **M2 — end-to-end mediated city** (needs C, minimal D). A real city: controller-
  served store over socket, agent `bd` = PATH-enforced thin client, `gc ready`, a
  **custom `scale_check`** that correctly sees graph work, workers operating.
  Test: run a formula → graph beads in SQLite, worker `bd close/claim` routes,
  scale_check scales, raw `bd` unreachable.
- **M3 — relocated + soak** (needs E). Flip `ClassGraph`→SQLite default on; the
  `doctor_fork_rate` soak meets p99 targets. The payoff proof.

## Dependency order

`A → B → C → D → E`, with A and B1 parallelizable. The correctness keystone is
**B3** (per-id mutation routing); the perf-and-flexibility keystone is **C** (thin
client + controller-served store), which the custom-`scale_check` requirement makes
**mandatory, not deferrable**. **M1 is reachable from A+B alone** — start there.

## Open risks to retire with evidence

- **Socket funnel throughput** (C5) — one controller serving thousands of
  concurrent cached reads; the number to benchmark before committing.
- **`mol`/`query`/`sql` coverage** — the thin `bd` mediates store ops; `mol` (the
  highest-value graph traffic) must be composed or routed, not left as Dolt
  passthrough, or the graph split leaks. Scope during C2/D.
- **Per-id routing latency** — `Get`→`Classify` adds a read per mutation; the cache
  must absorb it.
- **Cross-store `is_blocked`** (E/P5) — the blocks edge + projection stay in the
  Work store; split-topology test gates it.
