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
- [x] **P3b-2 — syncSessionBeads + reapers** (`6f4a6f568`). The big session_beads.go surface
  class-aware (reapers + sync mixed → two-store; reapRuntimesBoundToClosedBeads/
  sweepProcessTableOrphans/stopRuntimeBeforeSessionBeadMutation pure-session). Q1 resolved:
  sweepProcessTableOrphans preserves ErrNotFound-vs-transient exactly. review `wf_63a72547` =
  SAFE-TO-PROCEED. **session_beads.go DONE.**
- [x] **P4a** (`c0a309a9d`) session_wake.go/session_reconcile.go/session_sleep.go — pure `store`→`sessionStore`
  rename (24 fns, 69/69, no caller change). Byte-identical; no review needed (pure rename).
- [x] **P4b** (`5ba9666c4`) session_lifecycle_parallel.go start path — review `wf_6f2e3102` SAFE-TO-PROCEED.
- [x] **P4c** (`f4ab04955`) session_reconciler.go — review `wf_0a636cc1` SAFE-TO-PROCEED (no work-read method ever on sessionStore). **All Phase-4 session-write surfaces DONE.** The
  `sessionHas*AssignedWork*` family + `reachableStoresForSession` (the in-caller work federation)
  stay pure-work single-param; only `reconcileSessionBeadsTracedWithNamedDemand` + the drain-ack
  fns (finalizeDrainAckStoppedSession/reconcileDrainAckStopPending/finalizeDrainAckStopPendingSessions)
  + emitSessionStrandedDiagnostic get sessionStore.
- [ ] **P4-followup** STOP-path session helpers in session_lifecycle_parallel.go (~2827-2861:
  stopTargetThroughWorkerBoundary / cityStopSessionMarked / markCityStopSessionAsAsleep) are still
  single-`store` session ops — thread to sessionStore (flagged by the P4b review).
- [x] **P5** (`bec4104fa`) build_desired_state.go + agent_build_params.go — `bp.sessionStore`; review
  `wf_3a185f81` SAFE-TO-PROCEED. Q3 resolved: sessions city-only (rig legs work). **All controller
  threading P1–P5 DONE.**
- [ ] **P6 — ACTIVATION: derive the real `sessionStore` (with `cr.rec`) at entry points.** Until
  here every caller passes `(store, store)`, so at relocation session ops still ride the Router
  (federation); P6 makes the threaded ops go DIRECT to the session store. Split:
  - [x] **P6a** (`275b2083a`) controller activation DONE (incl. reconcileSessionBeadsAtPathWithNamedDemand
    wrapper threading; verified manually — every flip session-leg, work legs preserved; broad suite green).
  - **P6a (controller, REF)**: add `func (cr *CityRuntime) sessionBeadStore() beads.Store { return
    resolveSessionStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec) }`; at each entry point
    (run ~537-580, tick ~1091-1216, beadReconcileTick ~2091, the watchdogs, nudge/control ticks,
    finalizeDrainAckStopPendingSessions caller ~1127, the buildDesiredState/refresh callers, the
    reconcile callers) derive `sessionStore := cr.sessionBeadStore()` once and pass it as the
    FIRST arg where the placeholder `(store, store)` / `(cityBeadStore, cityBeadStore)` is now.
    Byte-identical at default (resolveSessionStore returns the work store). **Recorder (Q4): cr.rec
    is passed → at relocation openClassSQLiteStore attaches the recorder. NOTE: takes effect only
    after P7 unregisters ClassSessions (else the Router's nil-recorder open wins the dir cache).**
  - [x] **P6b-1** (`c6fa1cec7`) adoption/W1/stop-path + **P6b-2** (`8957cf4a1`) CLI session/wait + gracefulStopAll;
    review wf_7366ba79 SAFE-TO-PROCEED (wait/dep boundary held). **Track-S threading DONE (controller+CLI).**
  - **P6b (CLI + leftovers, REF)**: cmd_wait.go (cmdSessionWait, cmdWaitSetStateResult), cmd_nudge.go
    (per-entry openers), cmd_session_wake.go, cmd_stop.go (markCityStopSessionSleepReason) derive
    `resolveSessionStore(store, cfg, cityPath, rec)` locally and pass to the threaded fns. PLUS
    **W1**: route session lookups in session_name_lookup.go:343 + template_resolve.go:250/366 to
    bp.sessionStore. PLUS **P4-followup**: stop-path helpers (session_lifecycle_parallel.go
    ~2827-2861) → sessionStore. ⚠ Q2 gate: audit `runAdoptionBarrier` (city_runtime.go:480) — does
    it read WORK? (if session-only, route S; if it reads work, two-store).
- [x] **P7** (`28b9910bb`) — **SESSIONS OFF THE ROUTER (cutover).** routedPolicyStore is graph-only;
  removed sessionRelocated + registerSessionStoreBackend. Byte-identical at maintainer-city
  (graph=sqlite, sessions=bd → session block never hit). session_router_test.go rewritten to
  the class-aware reality (no-Router-for-sessions, resolveSessionStore routing, recorder
  emits bead.*, session/wait isolated from work Ready). Whole-tree build+vet, cmd/gc broad,
  internal/api, coordrouter, coordclass green. **TRACK S COMPLETE.**

## Track S — DONE ✅ (P1–P7, 25 commits). Every session/wait op is class-aware (controller+CLI+API).

## Track G — graph off the Router → delete coordrouter (⚠ LIVE-RISKY: graph=sqlite is live on
maintainer-city, so unlike sessions this is NOT inert — byte-identity bar is graph=bd, but the
graph=sqlite path changes from Router-mediated to class-aware-direct; needs relocated conformance
+ adversarial review). The whole prod delete-surface is ONE file (api_state.go); graph read paths
ride GraphOnlyReadyFor/ListFor INTERFACES; storeref (PrefixOwner+Resolve) is ready+dark.
This is class-aware callers (NOT the rejected Path-B dispatcher): each graph caller opens the
graph store directly (resolveClassStore(graph)); the by-id-agnostic case uses storeref.
- [ ] **G1 — graph-only READ callers → class-aware.** The ~7 GraphOnlyReadyFor/GraphOnlyListFor
  sites (huma_handlers_beads.go:349, dispatch/runtime.go:441, build_desired_state.go:1762,
  cmd_ready.go:181, session_reconciler.go:2804/2853, pool_session_name.go:199) call
  GraphOnlyReadyFor(cityStore) — post-Router cityStore=policy(work) doesn't implement it →
  would fall through to work.Ready() (WRONG). Rewire each to open the graph store via
  resolveClassStore(graph) (byte-identical at graph=bd). bead_policy_store forwarding wrapper stays.
- [ ] **G2 — graph CREATE/apply + by-id → class-aware.** (a) molecule pour/ApplyGraphPlan: use
  GraphApplyFor(resolveClassStore(graph)) instead of the Router. (b) by-id gcg-N (bd-shim/worker
  close, cross-class Get): wire storeref.Resolve([work, graph], id) — the Router's backendForID
  successor. ClassifyGraphPlan stays in coordclass until its last Router caller dies.
- [ ] **G3 — delete coordrouter; fold graph into resolveClassStore.** api_state.go: drop the
  import + coordrouter.New (:250) + the 2 *Router assertions (:199 caching-store builder, :860
  closeBeadStoreHandle) + Register/Backend/Backends; registerGraphStoreBackend folds into
  resolveClassStore(graph). Delete internal/coordrouter/router*.go,stores.go,bdgraphstore.go +
  retarget storeref_test.go off Router.Get. coordclass SURVIVES (storemigrate); ClassifyGraphPlan
  dies with the Router. Gate: relocated graph conformance (graph=sqlite by-id close lands on graph
  store; ReadyGraphOnly == graph store Ready) + adversarial review + whole-tree green.

## Followups (per /goal "all followups")
- [x] Phase-C tier for SESSIONS — moot/resolved (session store is not a Ready source; guarded by
  TestRelocatedSessionBeadsExcludedFromWorkReady). NB nudge/mail/order classes still carry the
  RAW-tier divergence as a PRE-EXISTING followup (label-exclusion guarded today).
- [ ] two-store wait/extmsg test — assert cancelState/reassignState route waits→session,
  extmsg→work with distinct stores (P3a review suggestion). Additive, low-risk.
- [ ] observability under-count — doctor backlog-depth / HTTP /status / storehealth read only the
  work store → under-count relocated infra beads (cosmetic; union the class stores or confirm).
- [ ] PG read-after-write — test a controller-terminalized shadow is visible to a fresh CLI PG
  connection (Postgres-specific; needs GC_TEST_POSTGRES_DSN).
- [ ] cmd_sling.go:1485 stampLastNudgeDeliveredAt → sessionStore when the sling seam is converted.

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
