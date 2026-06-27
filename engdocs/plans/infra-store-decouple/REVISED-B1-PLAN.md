---
title: "Track G — REVISED execution plan (owner chose b1: delete coordrouter, storeref at the convoy seam)"
date: 2026-06-27
branch: plan/decouple-infra-beads
base: d319316a5
status: EXECUTING — GA (G2b) done; GB/GC/GD/GE/GF remain
recon: raw/g2g3-adjudication.json, raw/crossclass-verify.json, raw/b1-plan-recon.json
---

## End-state (post-cutover)
Delete `internal/coordrouter`. Graph ops become class-aware; the ONE irreducibly
cross-class seam (convoy membership: synthetic ClassGraph convoy ↔ ClassWork members)
resolves the member by-id via `internal/storeref` over `[graph, work]`.

**Three mechanisms, layered:**
1. **Class-aware callers** pass the graph store where graph creates/reads happen
   (molecule/sling/order/formula/convergence/dispatch). Graph store = `resolveGraphStore`
   / `cr.graphBeadStore()` / `state.GraphBeadStore()` (legacy `.gc/beads.sqlite`).
2. **Policy-wrapper create-chokepoint (G2a, lands at cutover):** `wrapStoreWithBeadPolicies`
   routes graph-class `Create`/`ApplyGraphPlan` to the resolved graph store (with tiers).
   Defense-in-depth so a missed/future/ad-hoc graph create cannot orphan onto Dolt.
3. **`storeref` at the convoy seam:** `convoy.Members`/`TrackItem` resolve the cross-class
   member `Get` via `storeref.Resolve` over `[graph, work]`; convoy `DepList`/`DepAdd`/`List`
   stay on the convoy's home store.

## Invariant (every phase, mirrors Track S)
With the **Router present** until the final cutover, a class-picked graph store == the
Router's graph leg == the same `.gc/beads.sqlite` file, so every intermediate phase is
**byte-identical at BOTH graph=bd AND graph=sqlite**. Only GF removes the Router. ≤5
files/phase; build+vet+targeted tests green; adversarial review per landmine-prone phase;
commit `--no-verify`.

## Graph create/mutate surface that must go class-aware (recon create-orphan-audit)
- `cmd/gc/order_dispatch.go` dispatchWisp (PrepareInvocation synthetic convoy + molecule.Instantiate)
- `cmd/gc/cmd_order.go` (gc order run), `cmd/gc/cmd_formula.go` (gc formula cook graph.v2)
- `internal/api/handler_sling.go` + `internal/sling/sling.go:1257,1287` + `sling_core.go:1118`; `cmd/gc/cmd_sling.go`
- `internal/dispatch` ProcessControl (control/retry/ralph/fanout/drain/tally creates) → GE
- `cmd/gc/convergence_store.go` CreateConvergenceBead + PourWisp → `cr.graphBeadStore()`
- `internal/api/huma_handlers_beads.go:684` humaHandleBeadCreate (POST /v0/beads + bd-shim) → classify + route
- `cmd/gc/molecule_autoclose.go`, `cmd/gc/wisp_autoclose.go`, wisp-GC `city_runtime.go:1236`
- convoy seam: `internal/convoy/membership.go` + drain.go + graphv2/invocation.go → GC

## Phases
- [x] **GA (G2b)** `d319316a5` — `beadStoresForID` `[graph, work]` arm (API by-id class-aware).
- [ ] **GB** — class-aware graph-only READS in cmd/gc: `session_reconciler.go` awake-probes
  (2748/2778 + helpers) + `pool_session_name.go:198` orphan-release, via `resolveGraphStore`
  (they already have cfg+cityPath). dispatch `liveListForRoot` → GE. [~3 files]
- [ ] **GC** — convoy seam: add a backward-compatible variadic `memberStores ...beads.Store`
  tail to `convoy.Members`/`TrackItem` (+ `TrackingConvoysForItem` follow-up), resolving the
  member `Get` via `storeref`; synthetic-convoy callers (drain.go, graphv2/invocation.go) pass
  `[graph, work]`. Same-class callers unchanged. Adversarial review. [convoy + 2 callers + tests]
- [ ] **GD** — class-aware graph creates/reads at the instantiation callers (pass
  `policy(graphStore)` when `IsCompiledGraphWorkflow`): SPLIT — GD-1 sling/order/formula;
  GD-2 convergence + molecule/wisp autoclose + wisp-GC; GD-3 humaHandleBeadCreate classify-route.
- [ ] **GE** — dispatch: `controlStoreWithGraphRouting` → `policy(graphStore)` primary; thread
  the work store into drain (member reads/reservations) + pass `[graph,work]` to convoy seam;
  `liveListForRoot` reads the graph store. Adversarial review. [dispatch + cmd_convoy_dispatch]
- [ ] **GF (cutover, IRREVERSIBLE, ships live)** — G2a policy create-chokepoint;
  `routedPolicyStore → policy(work)`; remove the 2 `*coordrouter.Router` assertions (caching
  builder + closeBeadStoreHandle); delete `internal/coordrouter`; retarget `storeref_test`
  off `Router.Get`; delete `coordclass.ClassifyGraphPlan` (coordclass survives). Relocated-graph
  CONFORMANCE test (physical residence at `.gc/beads.sqlite`) + ported Router behavioral tests
  + adversarial review + owner-aware. [split G3a wiring / G3b caching+close / G3c delete+test]

## Followups (additive): two-store wait/extmsg test; doctor/status under-count; PG read-after-write; cmd_sling.go:1485 stamp.
