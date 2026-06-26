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
- [ ] **P3b — session_beads.go body** (`syncSessionBeadsWithSnapshotAndRigStores` :826 + closures,
  `retireDuplicate*`/`retireRemoved*`, `reapStaleSessionBeads`, `cleanupDeadRuntimeSessionCorpses`,
  `reapRuntimesBoundToClosedBeads`, `sweepProcessTableOrphans`, `stopRuntimeBeforeSessionBeadMutation`).
  Mixed fns get `sessionStore`; pure-session helpers (`loadSessionBeads`, `reopen*`, `setMeta*`,
  `configuredSessionNames`) keep one param but callers pass session store. Callers in
  city_runtime.go/cmd_start.go pass `(store, store)` until P6. ⚠ Q1 gate: audit
  `sweepProcessTableOrphans` `ErrNotFound`-vs-transient before relying on it post-relocation.
- [ ] **P4 — session_reconciler.go + session_wake.go + session_lifecycle_parallel.go +
  session_reconcile.go + session_sleep.go** (session-write surfaces).
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
4. Ship cutover with event-silent relocated session-row writes (restore in follow-up)? P7.
5. Phase-6 file overflow → split 6a (controller) / 6b (CLI). (Recommend: yes.)

## Log
- 2026-06-26: recon `wf_ed2319fa` (13 agents) → plan + inventory persisted to raw/. Build
  baseline green @ 2ea60d7a9.
