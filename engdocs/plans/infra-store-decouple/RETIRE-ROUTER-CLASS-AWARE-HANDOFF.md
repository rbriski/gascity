---
title: "Retire the Router — graph + sessions to class-aware callers (the principled end-state)"
date: 2026-06-26
branch: plan/decouple-infra-beads
head: c75bd2ad6
epic: ga-pd6tcg
status: HANDOFF — not started
---

> **Owner directive (2026-06-26):** the `coordrouter.Router` is the INTERMEDIATE that
> got graph + sessions onto separate stores fast. The end-state is **class-aware
> callers**: each caller uses the typed store for the class it operates on, so the
> Router's federation + by-id Get-probe can be **retired**. This is the resolution of
> the architecture issue the owner named — *"why doesn't each caller know the class it
> wants to read?"* This doc + `NEXT-AGENT-RETIRE-ROUTER-PROMPT.md` hand that work off.

## 0. TL;DR for the next agent

Take **graph** and **sessions** OUT of `coordrouter.Router` by making the controller's
callers class-aware (typed store per class), then delete the Router. The API layer is
already class-aware (Step 4). The controller is the last class-agnostic layer. This is
CLEANUP that follows the maintainer-city PG migration — it is **NOT a migration blocker**
(the migration runs fine on the Router intermediate). No deadline; do it right.

## 1. Why the Router exists, and why it should go

The Router (`internal/coordrouter`) is a **workaround for class-agnostic callers**. The
controller threads ONE raw `beads.Store` (the Router) through ~141 functions; the Router
then guesses each op's class at runtime:
- `Create` → `coordclass.Classify(bead)` → owning backend.
- `List`/`Ready` → **federate** (union every backend, dedup, re-sort) + the callers'
  existing post-filters (`IsSessionBeadOrRepairable`, `hasNonSessionAssignedWork`).
- `Get`/`Update`/`Close`/`SetMetadata` by id → **prefix short-circuit then federated
  Get-probe** (`router_mutation.go backendForID`).

Costs of keeping it: runtime (federation + double-probes on every by-id op; a `bd`-exec
work backend pays ~1s per probe-miss), cognitive (no compile-time class boundary), and a
standing **mass-closure landmine** surface (a work read that accidentally routes to the
empty session store closes live sessions). Class-aware callers eliminate all three: the
compiler enforces the boundary, there's no federation/probe, and a work read **cannot**
reach the session store (wrong type).

**Critical framing — this is NOT the rejected "Path B".** Earlier the owner rejected
"delete the Router" (Path B) because, with callers staying class-agnostic, deleting it
just means re-implementing a 2-backend `graphRoutedStore` dispatcher (same topology, ~8
hidden couplings). The end-state here is different: make the **callers** class-aware so
**no dispatcher is needed at all**. The by-id-agnostic case (a worker's `bd close gcg-N`)
is handled by `internal/storeref` (prefix→store), which already exists — not a stateful
Router.

## 2. Current state (what you inherit, HEAD c75bd2ad6)

- Graph + sessions ride the Router. `cmd/gc/api_state.go::routedPolicyStore` builds
  `policy(Router(work + graph + sessions))` when `graphRelocated(cfg) || sessionRelocated(cfg)`.
  Register sites: `ClassSessions` at `api_state.go:315,319`; `ClassGraph` at `380,392,413`.
- The deploy line is integrated (merge `90ab0506c`); everything green (build, vet,
  `cmd/gc` sharded, `internal/api`, dashboard-check). Backup ref
  `backup/decouple-infra-beads-pre-rebase-20260626`.
- `internal/storeref` (`PrefixOwner(id, stores)`, `Resolve(id, stores)`) is the
  prefix-based by-id resolver built for exactly this (P3.5 F3) — survives Router deletion.
- The API is already class-aware: `internal/api` session handlers call
  `s.state.SessionsBeadStore()` vs `CityBeadStore()` (Step 4, this session).
- `coordclass.Classify` (session/wait→ClassSessions, graph→ClassGraph, etc.) is
  FOUNDATIONAL and SURVIVES (binding decision #3) — it's the taxonomy the typed accessors
  and `storemigrate` selector use; do NOT delete it. Only `coordrouter` is retired.

## 3. The two tracks

### Track S — sessions out of the Router (do this FIRST; it's cleaner)

Sessions have **no class-agnostic by-id callers** — the controller and API always know
they're handling a session bead in context. So sessions need a typed store boundary, NOT
storeref. The recon this session (a 10-agent workflow) produced the **blueprint**: a
per-function inventory of all ~141 controller functions classifying each `store` use as a
**session/wait-bead op** (→ session store) or a **work-bead op** (→ work store, the
mass-closure-critical set: `workAssignmentStores`, `sessionHas*AssignedWork*`,
`closeSessionBeadIfReachableStoreUnassigned`, the close-family work release,
build-desired-state demand reads). Re-run that recon (it's reproducible) or reconstruct it.

Steps:
1. Introduce a typed session-store boundary in the controller. Either (a) thread a
   `sessionStore beads.Store` param (ADDITIVE — only session/wait ops use it; work ops keep
   the existing `store`; derive `sessionStore := resolveSessionStore(cr.cityBeadStore(),
   cr.cfg, cr.cityPath, cr.rec)` once per tick at the city_runtime.go entry points), or
   (b) carry both stores in a small context struct the tree already threads. Additive, never
   a substitution — a missed session op degrades to a benign leak (stays on work store), a
   misrouted work op is the catastrophic landmine.
2. Unregister `ClassSessions` from the Router (`api_state.go:315,319`); drop
   `sessionRelocated` from the Router gate. Sessions now reach their store only via the
   typed boundary.
3. Restore session `bead.*` event emission while you're here (the cutover follow-up): the
   typed session store should carry the controller recorder (see the cache-baking note in
   `class_store.go:88-93` and §4 of `SESSIONS-ROUTER-AND-CUTOVER.md`).
4. Guard: a test asserting work-assignment reads NEVER touch the session store (extend
   `cmd/gc/session_router_test.go::TestSessionStoreBackendRoutesSessionAndWorkSplit` into a
   no-Router form), and pointer-equality byte-identity at default.

### Track G — graph out of the Router (the original deferred "Phase A Steps 2-5")

Graph DOES have class-agnostic by-id callers (worker `bd close gcg-N`, cross-class
queries). The `gcg-` PREFIX is the class signal, so `storeref` handles them.

Steps (from `FINISH-AND-MIGRATE.md §2`, the retained Step 1–5 plan — Step 1 is DONE):
1. ✅ `*PostgresStore` graph-apply parity (done, `4d77288b9`).
2. Provider-aware by-id resolution: wire `internal/storeref.Resolve` into the
   `(storeRef,id)` resolver (`internal/dispatch/runtime.go`) and extend the prefix switch
   (`gcg-`→graph, `gcs-`→sessions-if-still-routed, `gc-/ga-`→work). Replaces the Router's
   `backendForID`.
3. Rewire BOTH graph read paths to a typed graph-store accessor BEFORE deleting federation
   (do it under the net): the `?type=molecule` augment + `ReadyGraphOnly`
   (`internal/api/huma_handlers_beads.go`) and the order gate `storesForGate`
   (`cmd/gc/order_dispatch.go`). Gate: split-topology `is_blocked` conformance.
4. Delete `coordrouter` (irreversible): confirm zero non-test callers of create-classify /
   by-id-probe / read-federation + the sole `coordrouter.New` (`api_state.go`); fold graph
   into `resolveClassStore` (class-aware); retire the `ClassGraph` registration.
   `coordclass` SURVIVES.

> NEVER collapse Track-G steps 3 and 4: step 3 is the behavioral pivot under federation;
> step 4 removes the net.

## 4. Constraints (carry into every commit)

- **Byte-identical at the default `bd` backend** until any explicit, config-gated,
  owner-sequenced cutover — same bar as the whole initiative.
- **Mass-closure landmine:** work-assignment reads must NEVER reach the session/graph
  store. The typed boundary makes this structural; add a guard test that proves it.
- **Additive, not substitution:** redirect class ops to the typed store; leave work ops
  on the work store. A missed class op is a benign leak; a misrouted work op closes live
  sessions.
- Keep green every phase: `go build ./...`, `go vet ./...`,
  `make test-cmd-gc-process-parallel`, `internal/api`, the PG-gated conformance
  (`GC_TEST_POSTGRES_DSN`, disposable `gc-pg` on :55460). ≤5 files/phase; commit
  `--no-verify` (worktree stale `core.hooksPath`).
- Don't drop the deploy-line features the merge integrated.
- This is CLEANUP — the maintainer-city PG migration runs on the Router intermediate and
  must not be blocked by it.

## 5. References

- `SESSIONS-ROUTER-AND-CUTOVER.md` — the Router intermediate + §4 the class-aware end-state.
- `FINISH-AND-MIGRATE.md §2` — the retained coordrouter-retirement Step 1–5 (Track G).
- `internal/storeref/storeref.go` — the prefix→store by-id resolver (Router-deletion replacement).
- `cmd/gc/api_state.go` — `routedPolicyStore` + the register sites to retire.
- `internal/coordrouter/{router.go,router_federation.go,router_mutation.go}` — what to delete.
- Auto-memory `infra-beads-decoupling-plan.md` — the binding decisions + this session's deltas.
- The session recon (141-function inventory) + the adapter-vs-threading design review:
  reproduce via the workflow patterns documented in the memory; both concluded the Router
  is correct AS AN INTERMEDIATE and this refactor is the principled step beyond it.
