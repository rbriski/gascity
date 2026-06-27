---
title: "Retire the Router — execution progress (Track S sessions, Track G graph)"
date: 2026-06-26
branch: plan/decouple-infra-beads
base_head: 2ea60d7a9
epic: ga-pd6tcg
---

> Live execution tracker for retiring `coordrouter.Router` by making the controller
> class-aware. THE PLAN (code-verified, 13-agent recon `wf_ed2319fa`) is
> `raw/retire-router-plan.md`; the inventory/sweep/track-G JSON is in `raw/retire-router-*.json`.
> Read `NEXT-AGENT-RETIRE-ROUTER-PROMPT.md` + `RETIRE-ROUTER-CLASS-AWARE-HANDOFF.md` first.

## Invariants (every phase)
- **Byte-identical at the default `bd` backend.** At default `sessionStore == workStore`, so
  every intermediate phase is a no-op there. Relocated correctness is proven ONLY at Phase 7.
- **maintainer-city runs sessions=bd today** → the whole Track-S threading is byte-identical
  no-op on the live city until the owner-gated sessions→postgres cutover. Low live risk.
- **Additive, never substitution.** Redirect only session/wait ops to `sessionStore`; work
  ops keep `store`/`rigStores`. A missed session op = benign leak; a misrouted work op =
  mass-closure outage.
- ≤5 files/phase; each phase ends green (build, vet, cmd/gc shards); commit `--no-verify`.
- **Convention (locked in):** two-store functions take `(sessionStore, workStore, ...)` — session
  FIRST, work SECOND — uniformly. The adversarial review flagged inconsistent order as the exact
  transposition foot-gun; keep this order for every new two-store signature. Pure-session helpers
  keep a single store param (callers pass the session store); pure-work fns are left untouched.
- **Misclassification is invisible to byte-identity tests** (at default both stores are the same
  handle), so run an adversarial review workflow per landmine-prone phase — do not trust green
  tests alone to prove the session/work split. Pattern: `raw/retire-router-phase-review` (the
  P1-3a run `wf_f745a537` returned SAFE-TO-PROCEED).

## Track S — phases
- [x] **P1 — `closeBead` two-store-aware** (`043ac2d7a`). 7 call sites pass `(store, store)`.
  Byte-identical. Seed guard `TestCloseBeadRoutesSessionAndWorkLegsToSeparateStores`
  (decoy work bead on the session store must survive). Build/vet/broad-session-suite green.
- [x] **P2 — close-family work-guards** (`a8b096d92`). `closeSessionBeadIfUnassigned`,
  `...ReachableStoreUnassigned`, `...RuntimeStoppedAndUnassigned` take `sessionStore` (after
  store); work-guard reads stay on store, close legs on sessionStore. 9 prod + 4 test sites
  pass `(store, store)`. `stopRuntimeBeforeSessionBeadMutation` deferred. Suite green.
- [x] **P3a — wait/extmsg split** (`58072d1a3`). `cancelStateAssignedToRetiredSessionBead`
  + `reassignStateAssignedToRetiredSessionBead` + `closeFailedCreateBead` now take
  `(sessionStore, workStore)`: waits→session, extmsg→work (no relocation seam). Byte-identical.
  Targeted tests green. Broad suite + adversarial review (wf_f745a537) running.
- [x] **P3b-1 — retire-named functions** (`350089dbb`). `retireDuplicateConfiguredNamedSessionBeads`
  + `retireRemovedConfiguredNamedSessionBead` take `(sessionStore, workStore, rigStores, ...)`:
  archive/wait legs→session, work reassign/unclaim + deferred runtime-stop→work. Callers
  (syncSessionBeads, reconciler heal-retire, 3 tests) pass `(store, store)`. Byte-identical.
- [ ] **P3b-2 — session_beads.go remaining body** (NEXT). Two coherent bites:
  - **reapers** `reapStaleSessionBeads`, `cleanupDeadRuntimeSessionCorpses`,
    `reapRuntimesBoundToClosedBeads`, `sweepProcessTableOrphans` — each is session reads +
    `closeBead`(already two-store); add `sessionStore`, callers in city_runtime.go pass
    `(store, store)`. ⚠ **Q1 audit before relying post-relocation:** `sweepProcessTableOrphans`
    `store.Get(live.SessionID)` treats `ErrNotFound` as "absent → terminate runtime"; confirm
    the relocated session-store Get returns `ErrNotFound` only on true absence (not transient),
    else fail-closed. (At default it's the same bd store, so no behavior change yet.)
  - **`syncSessionBeadsWithSnapshotAndRigStores` :826 (~900 lines, many closures)** — the big
    mixed fn. Add `sessionStore`; route the session create/update/close/alias/setMeta closures
    + the pure-session helper calls (`loadSessionBeads`, `findOpenSessionBeadBySessionName`,
    `reopenClosedConfiguredNamedSessionBead`, `configuredSessionNames`, `snapshotOrLoadSessionBeads`)
    to sessionStore; keep work guards/release on store/rigStores. **Pure-session helpers keep
    ONE param** (callers pass the session store); only mixed fns get the 2nd. Also thread
    `stopRuntimeBeforeSessionBeadMutation` (session op, currently deferred on workStore).
    Callers: city_runtime.go + cmd_start.go pass `(store, store)` until P6.
- [x] **P3b-2 — syncSessionBeads + reapers** (`6f4a6f568`). The big session_beads.go surface
  class-aware; review `wf_63a72547` = SAFE-TO-PROCEED. session_beads.go DONE.
- [ ] **P4 — session-write surfaces** (split by landmine density):
  - **P4a** session_wake.go (pure) + session_reconcile.go (pure metadata writers) +
    session_sleep.go (pure writers) — ~0 work sites.
  - **P4b** session_lifecycle_parallel.go (74 session / 4 work / 6 mixed).
  - **P4c** session_reconciler.go (79 session / **34 work** / 9 mixed — the densest landmine;
    the `sessionHas*AssignedWork*` family stays pure-work single-param; only the orchestrator
    `reconcileSessionBeadsTracedWithNamedDemand` + drain-ack fns get sessionStore).
- [ ] **P5 — build_desired_state.go + agent_build_params.go + pool_session_name.go**
  (desired-state dual-class; `bp.sessionStore`). ⚠ Q3 gate: sessions city-only?
- [ ] **P6 — derive `sessionStore` at city_runtime.go entry points + CLI/sweep writers**
  (mirror `resolveNudgesStore`; add `cr.sessionBeadStore()`). May split 6a/6b. ⚠ Q2 gate:
  `runAdoptionBarrier` classification.
- [ ] **P7 — cutover: guard test + unregister ClassSessions** (`api_state.go`). The only
  phase with relocated-backend behavior change. ⚠ Q4: event emission policy.

## Track G — phases (small; graph reads ride interfaces, not Router asserts)
- [ ] **G1 — wire `storeref.Resolve`/`PrefixOwner` into a by-id explicit-handles store**
  (replaces `coordrouter.New`). Model on `order_dispatch.go storesForGate`.
- [ ] **G2 — confirm the 2 graph read paths need no change** (GraphOnlyReadyFor/ListFor
  interfaces; `?type=molecule` augment is a plain List).
- [ ] **G3 — delete coordrouter; fold graph into `resolveClassStore`** (one prod file:
  api_state.go). `coordclass` SURVIVES; `ClassifyGraphPlan` dies with the Router.

## Open questions for the owner (gates noted above)
1. `sweepProcessTableOrphans` (`session_beads.go:2162`) treats session-store `ErrNotFound`
   as "absent → terminate runtime". Confirm relocated-store Get semantics before P3.
2. `runAdoptionBarrier` (`city_runtime.go:480`): session-only or reads work? Audit before P6.
3. Sessions city-only (rig legs stay on work stores)? Decision gate for P5.
4. ~~Ship cutover with event-silent relocated session-row writes?~~ **RESOLVED (owner 2026-06-27):
   THREAD THE RECORDER.** Relocated session writes must emit `bead.*`. Implementation: at the
   controller entry points P6 derives `resolveSessionStore(cr.cityBeadStore(), cr.cfg, cr.cityPath,
   cr.rec)` WITH `cr.rec` → openClassSQLiteStore attaches the recorder. ⚠ cache-order gotcha
   (class_store.go:88-93): the first opener of the session dir bakes the recorder; ensure the
   recorder-ful controller open wins (the worker/Router event-silent opens must not pre-bake a
   nil-recorder handle). Cleanest: after P7 unregisters ClassSessions, the Router no longer opens
   the session dir, so only the recorder-ful entry-point open remains. Add a test asserting a
   relocated session write emits bead.*.
5. Phase-6 file overflow → split 6a (controller) / 6b (CLI). (Recommend: yes.)

## Goal (owner 2026-06-27): drive to the END — Track S + Track G + recorder + all followups
Followups in scope: Phase-C tier for sessions (off the policy-wrapped Router → resolveClassStore
opens RAW, so the tier divergence REINTRODUCES for sessions — write no-history tier or prove
exclusion); two-store wait/extmsg test; observability under-count (doctor/status); read-after-write
PG visibility test; stopRuntimeBeforeSessionBeadMutation threading. Out of scope: destructive live
maintainer-city migration (separate owner-gated); push (owner-gated).

## Log
- 2026-06-26: recon `wf_ed2319fa` (13 agents) → plan + inventory persisted to raw/. Build
  baseline green @ 2ea60d7a9.
- 2026-06-26: landed the full close/cleanup/retire family — S1 `043ac2d7a`, S2 `a8b096d92`,
  S3a `58072d1a3`, order-fixup `ab5ae0424`, S3b-1 `350089dbb`. Adversarial review of S1-3a
  (`wf_f745a537`) = SAFE-TO-PROCEED, zero blockers, mass-closure landmine confirmed absent.
  All byte-identical at default; broad cmd/gc session suites green at each phase. NEXT: P3b-2
  (reapers + the big syncSessionBeads). Run a fresh adversarial review after P3b-2/P4/P6.
