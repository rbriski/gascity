---
title: "Orders cutover — execution spec (ga-cpbq45 follow-on)"
date: 2026-06-25
branch: plan/decouple-infra-beads
status: in-progress
---

> Concrete, file:line-pinned execution spec for the orders cutover. Confirmed
> against current HEAD (b3677ef4f). Splits into byte-identical seam work (O1) and
> the SQLite flip (O2). Read alongside DESIGN.md §4/§7/§14 and HANDOFF.md.

## The structural fact that drives the split

In `cmd/gc/order_dispatch.go` the dispatcher's `store` is **overloaded** — one
`beads.Store` value serves two ownership roles:

| Role | Sites (HEAD) | Target after flip |
|---|---|---|
| **order-tracking** (Create/Update/Close the `order-tracking` bead) | main loop `store.Create` @554,@618; `dispatchExec` `store.Update(trackingID,…)`; `dispatchWisp` `store.Update(trackingID,…)`+`markTrackingFailure`; deferred `closeOrderTrackingBead` | `orders.OrderStore` (SQLite, prefix `gco`) |
| **wisp/graph** (instantiate the molecule, label the wisp ROOT) | `dispatchWisp`: `prepareOrderWispRecipe`→`graphv2.PrepareInvocation`, `molecule.Instantiate`, `applyGraphRouting`, `store.Update(rootID,…)` | the work/graph store (`beads.Store`) |
| **gate read** (find open work for an order-run) | `hasOpenWorkStrict`/`hasOpenWorkInStoresStrict` over `storesForGate`; `lastRun`/`cursor` reads | union(order store ∪ graph store) |

Today `store = openStoreAtForCity(...) = routedPolicyStore(...)`, which **is the
`coordrouter.Router`** when `[beads] graph_store="sqlite"` (`main.go:1230`,
`api_state.go:241-244`). The Router's read-federation is what makes one `store`
find tracking beads (work store) *and* wisp roots (graph store) in one call. That
federation is deleted in P6 — so the gate must union the order store + graph
store explicitly. `hasOpenWorkInStoresStrict([]beads.Store,…)` already exists for
exactly this (`order_dispatch.go:1750`).

## O1 — Seam extraction (byte-identical, bd phase)

Goal: make the order-tracking ownership role a real typed seam (`orders.OrderStore`)
threaded separately from the wisp/graph `beads.Store`, wired to the *same* store in
the bd phase so behavior is byte-identical. Proven by a divergent-store routing test
(the orders analog of mail's two-store split, DESIGN §14).

Edits:
1. `internal/orders/store.go`: add `Create(beads.Bead) (beads.Bead, error)` to
   `OrderStore` (still a faithful `beads.Store` subset — `var _ OrderStore =
   beads.Store(nil)` stays true).
2. `cmd/gc/order_dispatch.go`: thread `orderStore orders.OrderStore` alongside
   `workStore beads.Store` through `launchDispatchOne` → `dispatchOne` →
   `dispatchExec`/`dispatchWisp`.
   - `dispatchExec` uses *only* tracking ops → takes `orderStore orders.OrderStore`
     (drops the `beads.Store` param; it never touches graph).
   - `dispatchWisp` needs both → `(orderStore orders.OrderStore, workStore beads.Store)`.
   - Route the two main-loop `store.Create` (@554,@618) through `orderStore`.
   - In the main loop, `orderStore := store` (same value) for the bd phase.
3. `cmd/gc/order_dispatch_seam_test.go` (NEW): a divergent-store routing test —
   construct the dispatcher with a *distinct* order store and work store, dispatch a
   formula (wisp) order, assert the `order-tracking` bead lands in the order store and
   the wisp ROOT lands in the work store. This is the load-bearing proof the seam is
   real, not cosmetic.

Byte-identical argument: in production wiring `orderStore` and `workStore` are the
same `store` value, so every call lands on the identical backend exactly as before.
The only new code path (distinct stores) is exercised solely by the new test.

Gate: `go build ./... && go vet ./internal/... ./cmd/gc/`; `go test
./internal/orders/ ./cmd/gc/ -run 'Order|Dispatch'`; then the full `go test ./cmd/gc/`.

## O2 — resolveOrderStore + SQLite flip (gated on conformance)

- `resolveOrderStore(workStore, cfg, cityPath, rec)` on the mail template
  (`class_store.go:136` `resolveMailMessagesStore`): SQLite store (prefix `gco`,
  retention 0, recorder) when `cfg.Beads.ClassUsesSQLite(config.BeadClassOrders)`,
  else `workStore`. Inject the resolved `orderStore` at the dispatcher construction
  (`order_dispatch.go:403`) / the controller wiring.
- Gate cross-read: add the **graph store** to `storesForGate` (so wisp roots are
  found once the Router federation is gone). Byte-identical while the Router exists
  (OR-semantics; a redundant graph read can only confirm what federation already
  reports — never flips true→false). Add the **order store** to `storesForGate` for
  tracking-bead reads once tracking moves to SQLite.
- Conformance: orders `RunClassedStoreTests`-style suite (golden round-trip,
  reconstruct-union, watch-coherence, emit-after-commit, projection-invariance,
  error-classification, `readyExcludeTypes`-deletion, cross-store FK, concurrent-proc)
  on BOTH bd and SQLite. Typed single-flight query reproducing `hasOpenWorkStrict`.
- Flip-safety (DESIGN §7 four checks): no non-transition `status=closed` writes (orders
  Close is a true transition); no uncovered re-stamps; retention stays disabled while a
  recorder is attached (self-GC deferred to the controller sweep); single controller-owned
  writer.

## Test-coverage oracle (what pins byte-identicality)

`cmd/gc/order_dispatch_test.go` (9476 LOC), `cmd_order_test.go` (3474),
`order_dispatch_gate_test.go`, `order_dispatch_gate_policy_test.go`,
`order_dispatch_close_race_test.go`, `order_dispatch_tracking_index_race_test.go`,
`order_scan_contract_test.go`, `doctor_order_tracking_retention_test.go`. The full
`go test ./cmd/gc/` (~60s) is the real guard for the threading change.
