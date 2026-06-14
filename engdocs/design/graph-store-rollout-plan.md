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
mutation **within the managed execution surface** (agents, the reconciler,
controller exec-orders, and arbitrary user shell `scale_check`/`work_query`/prompts)
is routed and cached through one in-process control point — no path to an
unmediated `bd` mutation from a managed shell (humans/cron on their own login
shells are out of band; a `doctor` check warns on a non-thin `bd` on the operator
PATH). Success is measured by a `doctor_fork_rate` soak: the fork storm gone, the
recovered p99 targets met (FilterScan ≤10ms, Ready ≤10ms, point-read ~1ms), with
byte-identical behavior.

> **Hardened by red-team (2026-06-14).** A four-lens adversarial review confirmed
> the architecture and the A→B→C→D→E ordering, and reachability of M1 — but found
> the original plan mis-scoped B1 (the landed Router is a skeleton, not an identity
> transform) and missed enforcement holes (controller exec-orders, `bd mol`/`gate`/
> `query`, the `gc bd` recursion trap, ChangeFeed staleness, a governance conflict).
> Those corrections are folded into the workstreams below.

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
- [x] **A3** Atomic `Claim(id, assignee)` — CAS-if-unassigned (idempotent self, conflict for another), the acquire-dual of `ReleaseIfCurrent`, single-winner via the store's single write connection (race-tested). The signature differs from `BdStore.Claim(id)` (env-actor); reconciling them behind a shared capability interface and routing the by-id claim is **C6**.
- [ ] **A4** Grow `coordtest.RunGraphStoreTests` to actually exercise the seam — `Claim` (single-winner/idempotent/conflict/not-found), `ReadyCandidates` routed-vs-type filtering, and storage-tier round-trip — and run it + `RunClassedStoreTests(ClassGraph)` green against the SQLite store. (Today it only covers `ApplyResolvesEveryNodeKey`/`ApplyEmptyPlan` — too shallow to gate.) Bench-harness recovery is *optional* (perf numbers trusted; recover only on suspected regression). *Test: grown conformance green.*

### B. Router mediation (in-process) — `beads-work-infra-split.md` P1–P2
- [ ] **B1a** **Port the FULL `beadPolicyStore` behavior onto the Router** — this is NOT a one-liner; the landed Router is a deliberately-minimal skeleton (`router.go:21-24` admits it). Add: storage-tier selection on `Create` incl. **wisp-root `Get`-inheritance** (`bead_policy_store.go:182-194`); read-tier expansion `TierIssues→TierBoth` on `List/Ready/Children/ListByLabel/ListByMetadata/Count`; `Count`/`Handles`/`ReleaseIfCurrent` delegation; and **tier-forwarding on graph pours** — `routedGraphApplier` must classify the pour tier (mirror `policyNameForGraphPlan`+`effectiveBeadStorage`) and call `ApplyGraphPlanWithStorage`. The Router must carry the City `cfg` to compute the pour tier. *Test: per-bead storage tier (Ephemeral wisp / NoHistory workflow under BD105) preserved.*
- [ ] **B1b** Re-point `wrapStoreWithBeadPolicies` → Router **and** teach `unwrapBeadPolicyStore` + `closeBeadStoreHandle` to recognize `*Router` (else the CachingStore re-wrap silently drops the mediation layer, `api_state.go:155-198`). *Test: full suite + a byte-identical differential test running the FULL CachingStore-inside / Router-outside re-wrap, canonicalizing & comparing ids + edges + **tiers** + wire JSON of a graph.v2 pour.*
- [ ] **B2** Read federation (**strict prerequisite of B3**): `Router.Get/List/Ready/DepList` fan out **deterministically** — owning-class-first by routing hint, else all-backends with a single-owner assertion (NOT a try-work-then-graph not-found fallback, which is non-deterministic under same-id collision). *Test: `union(work,graph) == legacy` over an all-classes fixture.*
- [ ] **B3** **Per-id by-class mutation routing** (correctness keystone; depends on B2): `Router` resolves a by-id op's class (`Get`→`Classify`) and routes `close/update/setmetadata/claim` to the owning backend; `DepAdd/DepRemove` route by `(depType, Classify(a), Classify(b))` — blocks-edges pinned to Work, intra-graph tracks/parent-child to Graph (coupling #2). *Test: by-id close lands in the right store; dep cases work→work / graph→graph / work→graph blocks / graph→graph tracks (+ removes); + a cross-process variant via the thin client once C exists.*
- [ ] **B4** `RouterReady` = **merge-sort** of per-backend `Ready` streams on `(created_at,id)` with **dedup by id** (not naive concat); `graph.Ready` filtered to routed candidates. Extend `beads.ReadyQuery` with the routed predicate (`gc.routed_to`/`run_target`/`exclude-type`). Route the (already in-process) default demand scan + Go oracle through `RouterReady`. (Fork-elimination of the *per-tick shell* is a **C** deliverable — the default scan is already in-process today.) *Test: `RouterReady == legacy bd ready` byte-identical, incl. a same-backend identity-phase case.*

### C. Mediated `bd` + controller-served store (the per-tick + enforcement) — P3
- [ ] **C1a** Typed **mutating** store endpoints behind Huma (`Create/Update/Close/Claim/Dep`) — `api.Client` is read-only today; this regenerates `openapi.json`/genclient/dashboard types. *Gated by `TestOpenAPISpecInSync` + `make dashboard-check`.*
- [ ] **C1b** Serve the Huma store API over a **local unix-domain listener** (the controller line-socket is a one-shot 8-verb protocol; do NOT hand-roll JSON on it). **Calls out a default change**: this turns the API server on by default for managed cities. *Test: client round-trips create/get/update/close/claim over the socket.*
- [ ] **C2** Thin `bd` client → the socket: bd-CLI-arg → store-op translation preserving bd's stdout + **exit-code contract**. Invokes `bd.real` by an **install-time absolute path (`GC_BD_REAL`), never `LookPath`** (the `gc bd` passthrough `LookPath`s and would recurse). Passthrough is a **CLOSED allowlist** of ops that provably never touch graph-class data (e.g. dolt server lifecycle) — NOT an open "Dolt-specific" catch-all. `gc bd` becomes Router-aware (drop its bd-only hard-reject) or its pack-script deps move to the thin client. *Test: no-recursion test; raw-bd == thin-client equivalence.*
- [ ] **C2a** Custom-shell **byte-identity corpus**: replay real custom `work_query`/`scale_check` shells (incl. `bd query ephemeral`, `bd mol`, jq forms, non-zero exits) asserting byte-identical stdout+exit vs raw bd. Until green, the default-on byte-identity claim holds only for the standard predicate.
- [ ] **C3** `gc ready` narrow CLI — byte-identical predicate via `RouterReady`. *Test: `gc ready == bd ready` across all branches.*
- [ ] **C4** **Enforcement**: managed-shell `bd` = thin client (PATH). A **new lint** that walks `.sh`/`.toml`/`.md` pack assets (today's guard is Go-only) for raw bd mutating verbs + a tracked inventory/conversion of existing raw-bd sites (incl. the formula `$GC_BEAD_ID` graph mutations in `mol-do-work.toml`). **GOVERNANCE — resolve first:** "Dolt-bound `bd` unreachable" conflicts with the Accepted `beads-dolt-contract-redesign.md` GOAL "raw bd usable from each local scope" — either supersede that ADR, or keep raw `bd` reachable and rely on the lint + a Dolt-`bd` that refuses graph-class ids. *Test: lint fails on a planted raw `bd`; managed shell cannot reach Dolt `bd` (per the resolution).*
- [ ] **C5** **Benchmark (risk gate)** — the right unit: N agents running the **full `work_query` pipeline** at cadence + M pools running `scale_check` per patrol tick, concurrent with a graph.v2 write stream (cache invalidation); report **p99 end-to-end incl. shim cold-start spawn**. Plus an **availability** section: define controller-down behavior (fail-closed vs fall-through) + a worker-survives-controller-restart test (today `bd`→Dolt works without the controller; the socket funnel is a new SPOF).
- [ ] **C6** **Route the by-id `Claim` through the Router**: reconcile `BdStore.Claim(id)` (env-actor) vs `SQLiteStore.Claim(id,assignee)` behind a shared capability interface; re-point `hookClaimBdStore`'s claim (`cmd_hook_claim.go:262-303`, which builds a raw `*BdStore`) through the controller-served Router (`Get`→`Classify`→`backend.Claim`). Without this, `gc hook --claim` on a relocated graph node misses SQLite.

### D. Graph-only claim surface (workers touch only graph nodes) — claim-surface section
- [ ] **D1** Default sling + classic `--on` emit a routable **graph node** (the wisp-root shape) referencing the work bead by id — the net-new "adopt an existing bead as a graph node" primitive. *Test: a bare/`--on` sling stamps `gc.routed_to` on a graph node, never the work bead.*
- [ ] **D2** Deprecate Mode B for the **in-repo** packs (core + `examples/gascity`). **Note:** gastown's `polecat`/`dog`/`crew` formulas live in the **external `github.com/steveyegge/gastown` repo** — Mode-B deprecation there is a downstream coordination item with its own timeline, not an in-tree checkbox. *Test: no bare in-repo `ClassWork` bead is pool-claimable.*
- [ ] **D3** Flip in-repo prompts/packs off raw `bd ready`/mutations to the mediated path. *Test: prompt-render test asserts nothing emits raw `bd`.*

### X. Pre-relocation enforcement (gates E — the controller/Dolt-passthrough holes)
- [ ] **X1** Route every **controller `exec=` order** that mutates graph/wisp/gate beads — `reaper.sh`, `wisp-compact.sh`, `gate-sweep.sh` (run via `order_dispatch.go:127` with the controller's env, not a managed PATH) — through the mediated thin client via `mergeOrderExecEnv`; **rewrite `reaper.sh`'s raw `dolt sql`** into mediated/Go logic. *Test: reaper/wisp-compact reap SQLite-resident graph beads.*
- [ ] **X2** Route `bd mol` / `bd gate check` / `bd query ephemeral` (no `gc` impl today; the highest-value graph traffic; `bd gate check --escalate` is a **mutation**) through the Router — a first-class gated workstream (`gc mol` / mediated), with a byte-identical differential gate. **Not** left as C2 passthrough.

### E. Storage relocation (the payoff) — P4–P5
- [ ] **E0** (**hard prereq**) Graph-store **ChangeFeed**: wire SQLite GraphStore writes into the bd-hook→`ApplyEvent` event path, so cache freshness + event-triggered orders (`cascade-nudge-on-blocker-close`), molecule autoclose, and reconcile keep firing for graph beads. (The backend ADR marks this "deferrable" — **overridden** for E.) *Test: watch-coherence subtest — a graph `close` fires `bead.closed`.*
- [ ] **E1** Register the SQLite `GraphStore` for `ClassGraph` behind a config flag (default off — **this flag is the entire opt-in boundary**). Add a **data-migration/cutover** story: a dual-write window (write graph to both, read SQLite) **or** quiesce-drain-migrate-flip; cite R2.3 (≤60s swap / 48h Dolt cold-backup rollback). *Test: flag off → byte-identical (graph on Dolt); flag on → graph in SQLite, rest in Dolt; cutover preserves existing graph beads.*
- [ ] **E2** `doctor_fork_rate` soak vs the recovered p99 targets = ship gate; flip default on. *Test: fork storm gone; p99 met; split-topology `is_blocked` green.*

## Testable milestones (the "so we can test it" path)

- **M1 — store-level federation proof** (needs A4, B1a, B1b, B2, B3). A Go
  integration test: a Router over `{work: Dolt, graph: SQLite}` — graph beads route
  to SQLite, work to Dolt, reads federate deterministically, a by-id mutation lands
  in the correct store. **No production wiring; the earliest end-to-end test that
  the split works — reachable from A+B alone.**
- **M2 — end-to-end mediated city** (needs C + X + minimal D). A real city:
  controller-served store over socket, agent `bd` = PATH-enforced thin client,
  `gc ready`, a **custom `scale_check`** correctly seeing graph work, controller
  exec-orders mediated, workers operating. Test: run a formula → graph beads in
  SQLite, worker `bd close/claim` routes, scale_check scales, reaper reaps SQLite.
- **M3 — relocated + soak** (needs E0, E1, E2). Flip `ClassGraph`→SQLite on; the
  `doctor_fork_rate` soak meets p99 targets with the ChangeFeed live. The payoff.

## Dependency order

`A → B → C/X → D → E`, with A and B1a parallelizable. Sequence inside B is strict:
**B1a → B1b → B2 → B3** (byte-identity, then re-point, then federated reads, then
per-id routing). Keystones: **B1a** (the default-on byte-identity claim depends on
it), **B3** (correctness), **C + C6** (mediation + claim routing), **E0**
(ChangeFeed, or graph events go silent). The X enforcement phase + C **gate E1**.
**M1 is reachable from A+B alone** — start there.

## Open risks to retire with evidence

- **Socket funnel throughput + availability** (C5) — one controller serving the
  full `work_query`/`scale_check` pipelines concurrently with graph writes; plus the
  new SPOF (controller-down behavior).
- **`mol`/`gate`/`query-ephemeral` coverage** (X2) — no `gc` impl today; the
  highest-value graph traffic; must be routed, never Dolt-passthrough, or the split
  leaks (and `gate check --escalate` silently no-ops).
- **Controller exec-order mutators** (X1) — `reaper`/`wisp-compact`/`gate-sweep`
  mutate via raw `bd` + raw `dolt sql` outside any PATH guard.
- **ChangeFeed staleness** (E0) — without it the cache serves stale graph reads and
  event-triggered orders/autoclose/reconcile stop firing for graph.
- **Governance conflict** (C4) — PATH-unreachability vs the Accepted "raw bd usable
  from each local scope"; resolve before C4.
- **Per-id routing latency** — `Get`→`Classify` per mutation; the cache must absorb it.
- **Cross-store `is_blocked`** (E/P5) — blocks edge + projection stay in the Work
  store; split-topology test gates it.
- **E1 data migration** — what happens to graph beads already in Dolt at flip time;
  dual-write vs quiesce-drain; reversibility once live writes go to SQLite.
