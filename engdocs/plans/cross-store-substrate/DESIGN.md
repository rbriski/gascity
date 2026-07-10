# Cross-Store Substrate — removing the split-city violation class

Status: proposal (2026-07-09). Analysis by three parallel Fable passes
(inventory / layering root-cause / test-net + architecture), synthesized here.

## 0. Problem

A Gas City can be **multi-store**: each coordination class of bead (work,
graph, sessions, messaging, orders, nudges) may live on a different physical
backend (Dolt / SQLite / Postgres). We want to keep this — every object should
be able to choose whatever substrate it wants — but we keep discovering
**cross-store violations**: object-model code performs an operation through one
`beads.Store` handle assuming the beads it touches are co-resident, and in a
split city a related bead lives on a *different* store, so the op hard-fails or
silently returns wrong/empty results.

Three have been found and patched as point-fixes on
`fix/xstore-convoy-tracks-refbyid`:

- **convoy tracks** (`d6479fbc0`) — a `tracks` dep *edge* can't span two dep
  tables → ref-by-id metadata instead.
- **beadPolicyStore.Get federation** (`ccebee78b`) — graph beads created through
  the policy store were unreadable through it.
- **session lifecycle owner-routing** (`e057a2f69`) — a wisp-marked session bead
  routes to the graph store but the reconciler read+wrote it through the
  sessions/work handle.

The inventory below shows **at least twelve more** live sites, and the fixes
themselves interact to reintroduce the original bug (§2, finding #1). Point-fixes
are not converging. This document proposes the layering + test infrastructure
that makes the violation class **unrepresentable**, while keeping any-class →
any-backend fully configurable.

## 1. Root cause — the missing invariant

Bead **identity is city-global**, but the bead **operation surface is
backend-local**, and no layer binds the two.

Ownership is *already* a pure static function of a bead id: every relocated
class mints a reserved, globally-disjoint id prefix (`gcg/gcm/gcs/gco/gcn`,
`internal/config/reserved_prefixes.go:16`; disjointness enforced by
`ValidateReservedPrefixesIn`), so `owner(id)` is decidable without a read. But
nothing computes it uniformly. Routing responsibility is smeared across callers
under three inconsistent regimes:

- **Create** routes by *value-classification* (`coordclass.Classify`, via
  `beadPolicyStore.createTarget`, `cmd/gc/bead_policy_store.go:116`).
- **Reads** route by id **only if the caller remembered to assemble a store
  set** and call `storeref.Resolve` (`internal/storeref/storeref.go:51`).
- **Every other write** (`Update`, `Close`, `SetMetadata`, `SetMetadataBatch`,
  `Delete`, `DepAdd`, `Claim`, and all `List`/`Ready`) routes to **whatever
  handle the caller happens to hold** — `beadPolicyStore` promotes them straight
  to the embedded work store.

The type system enforces the wrong contract: `func F(store beads.Store, id
string)` typechecks with *any* store, works in a single-store city, and silently
violates ownership in a split one. Because **class is a property of bead
*content*, not nominal type** (a wisp-marked *session* bead is `ClassGraph`;
`internal/coordclass/classify.go:112` puts the wisp arm before the session arm),
"I am the sessions subsystem so my beads are on the sessions store" is false —
any per-subsystem single-store handle is unsound by construction.

There are today **four parallel ad-hoc federations**, each covering a different
subset of ops: the policy-store `Get`, the session wrapper, `storeref`
threading, and the API `beadStoresForID` store-loop. Each new bug is the same
defect re-instantiated.

### The invariant

> Ownership is a total, static function of bead id — `owner(id)` via reserved
> disjoint prefixes. **Every operation naming an existing bead id executes on
> `owner(id)`.** Corollary: an operation naming *two* bead ids is legal only
> when `owner(a) == owner(b)`; a cross-owner relationship must be represented as
> ref-by-id metadata, never as a store row.

The *precondition* (prefix disjointness) is already enforced. The *conclusion*
is enforced nowhere. The class decomposes cleanly along the **one-id / two-id**
line:

- **one-id ops** → *federate the store* (owner-route reads AND writes). The
  session fix is the prototype.
- **two-id ops** (dep edges, parent-child) → *change the representation* to
  ref-by-id, because a dep row lives in exactly one store's table and no store
  owns a cross-store edge. The convoy fix is the prototype.

## 2. Blast radius (inventory, ranked worst-first)

"Mitigated" = already threads stores / owner-routes / ref-by-id. "Exposed" =
still latent.

1. **TrackItem's same-store guard is defeated by the federated Get — LIVE
   REGRESSION of this branch** (hard fail). `internal/convoy/membership.go:48`
   uses `store.Get(convoyID)` success as the "co-resident, safe to `DepAdd`"
   signal; `ccebee78b` made `beadPolicyStore.Get` federate `[work, graph]`, so a
   graph-resident synthetic convoy now resolves through the policy handle → the
   guard passes → `store.DepAdd(gcg-convoy, work-item)` fires cross-store → the
   original `resolving issue ID gcg-2` failure returns. The regression test
   `membership_refbyid_test.go` models the *pre*-`ccebee78b` work-only Get, so it
   passes while production fails. **Fix: probe residency by physical store
   (`storeref.PrefixOwner(convoyID) == PrefixOwner(itemID)`), not by federated
   Get** — or drop the guard once §3's `Federated.DepAdd` enforces same-store.
2. **Attach blocking-dep crosses classes** (hard fail). `internal/molecule/
   molecule.go:303` and `cmd/gc/cmd_formula.go:893` add a `blocks` edge from a
   work attach-bead to a graph sub-DAG root (`gc formula cook --attach
   <work-bead>`). No store owns the edge. Dispatcher-internal `molecule.Attach`
   is safe (both endpoints graph). Exposed.
3. **Order-dispatch wisp root: label Update through work store + gate blind to
   graph** (hard fail + silent duplicates). `cmd/gc/order_dispatch.go:1435`
   `workStore.Update(rootID)` on a graph-resident wisp root → not found → order
   fails every interval; the single-flight gate (`:512`, `:1538`) scans work
   handles only → duplicate wisps + stale-wisp leak. Tracking-bead seam is
   mitigated; wisp-side exposed.
4. **Drain projects work-class blockers into graph-store dep edges** (silent
   permanent stall). `internal/dispatch/drain.go:628` /
   `internal/dispatch/control.go:285` add `blocks(graphBead, workBlocker)` on the
   graph store; SQLite inserts the dangling edge, and SQLite `Ready`
   (`sqlite_store.go:917`) treats a missing blocker row as *permanently
   blocking*. Knock-on hard fail: `ralph.go:795` `Get`s the work blocker through
   the graph store. Exposed.
5. **Source-workflow singleton/liveness scans use work-only List** (silent
   duplicates + premature source close). `internal/sourceworkflow/
   sourceworkflow.go:173` `ListLiveRoots`; every workflow root is ClassGraph but
   the handles passed never touch the graph store. Sites: `sling_core.go:682/707`,
   `sling.go:1316/1404/1469`, `dispatch/runtime.go:925`, `cmd_formula.go:699`,
   `cmd_convoy_dispatch.go:1520/1605`, API `handler_convoy_dispatch.go:72`.
   `molecule_autoclose.go:197` is the mitigated contrast. Exposed.
6. **walkSourceBeadChain first hop reads the work source through the graph
   primary** (silent non-close). `internal/dispatch/runtime.go:816`: empty
   `gc.source_store_ref` → reads the work source through the graph handle → not
   found → workflow's source bead never closed. Ref'd hops are mitigated.
   Partially exposed.
7. **API by-id federation covers only the graph class** (hard fail, latent).
   `internal/api/handler_beads.go:165` `beadStoresForID` has a graph-only arm;
   relocated `gcs-/gco-/gcn-/gcm-` ids reach no candidate (city Get spans only
   `[work, graph]`). Controller has the accessors but the loop doesn't consult
   them. Exposed when those classes relocate.
8. **Convoy read/mutate surfaces that never see the graph store** (silent empty /
   hard fail). `internal/api/huma_handlers_convoys.go` (get/Members/TrackItem/
   UntrackItem/detach, `:167/182/238/338/726`); `handler_beads.go:405`
   collectBeadGraph; `api_state.go:616` autoclose ("convoys are ClassWork" is
   false for synthetic convoys); `cmd_convoy.go:1817`; `graphv2/
   invocation.go:195` legacy alias. Mitigated contrast: `drain.go:201/919`,
   `wisp_autoclose.go:170`.
9. **beadmail pins sessions to the work store** (silent, latent). `cmd/gc/
   class_store.go:315` passes `workStore` as beadmail's session seam; relocating
   `[beads.classes.sessions]` moves session beads away → sender/recipient
   resolution misses. Exposed when sessions relocate.
10. **extmsg beads classified ClassMessaging but served from work/city store**
    (silent, catastrophic-on-migration). `extmsg.NewServices(cityBeadStore)`
    (`api_state.go:149`) + `session_beads.go:765`; `gc beads migrate --class
    messaging` moves the beads to gcm while services keep reading work. Needs an
    extmsg store seam. Exposed.
11. **molecule.Instantiate non-graph-apply fallback wires graph deps through the
    work handle** (hard fail, conditional on the graph-apply kill switch / a
    non-GraphApply store / double-transient fallback). `molecule.go:695/713/917`.
    Partially exposed.
12. **Peripheral**: `sling/cycle.go:44` DetectCycle (silent), `sling_core.go:565`
    auto-convoy stamp side (soft finalize churn), `order_dispatch.go:1749`,
    extmsg session lookups, `cmd_bd_shim.go` (deliberately allowlisted).

**Mitigated already** (for contrast): convoy ref-by-id representation, session
owner-routing, the whole dispatcher (`dispatcherControlStores`, drain threading,
`getControlBeadByID`, graph-only ready/list capabilities), autoclose graph
threading, order/nudge leaf seams, mail message persistence seam.

The spread across `molecule`, `dispatch`, `sling`, `sourceworkflow`, `api`,
`order_dispatch`, `beadmail`, `extmsg` confirms this is a **substrate/layering**
problem, not a bug in any one package.

## 3. Solution — kill the class, keep the freedom

Combine both directions; neither alone suffices.

### 3.1 `storefed.Federated` — the owner-routing substrate (one-id ops)

New package **`internal/storefed`** (imports `beads`, `storeref`, `coordclass`,
`config`; imported only by the composition root — it cannot live in
`internal/beads` because `coordclass` imports `beads`, and Layer N never imports
Layer N+1). Object-model packages never import it — they keep receiving a plain
`beads.Store`.

```go
type Route struct { Class string; Prefix string; Store beads.Store }
func New(primary beads.Store, classify func(beads.Bead) coordclass.Class, routes ...Route) beads.Store
```

- Collapses to `primary` unwrapped when routes dedupe to one physical store —
  the byte-identical-at-bd invariant (same shape as
  `session_store_federation.go:35`). Every default-bd city and test is untouched.
- **Create / ApplyGraphPlan**: classify → owning class store (generalizes
  `createTarget`/`applierForPlan` from graph-only to all classes). Create is the
  *only* class-routed op — it is the only op with no id yet.
- **All by-id ops** — `Get, Update, Close, Reopen, SetMetadata,
  SetMetadataBatch, Delete, CloseAll` — owner-route via `storeref.PrefixOwner`
  with the probe fallback (generalizes `classFederatedSessionStore.owner`, which
  today covers 5 ops for one handle).
- **DepAdd / parent-set**: resolve issueID's owner; **require** the other
  endpoint to resolve in the same store, else return typed
  `storefed.ErrCrossStoreDep` telling the caller to use a ref-by-id key. This is
  the runtime enforcement of the modeling rule — loud on *every* backend
  (closing the RC2 leniency gap, §4).
- **List/Ready/ListByMetadata**: pin to one store when class-pinnable, else union
  across routes (what `convoy.Members` hand-rolls). Fail closed on partial error.
- **Tx**: single-store only; cross-store atomicity is explicitly out of contract.

**Capability preservation (no wrapper-drops-capabilities bug).** Implement the
optional capabilities explicitly, owner-routing then delegating — exactly
`beadPolicyStore.Claim`'s pattern: `Claim`/`ReleaseIfCurrent`, `Counter.Count`
(sum/delegate, `ErrCountUnsupported` fallback), `Handles()` returning federated
readers, and the `GraphApplyHandle`/`ReadyGraphOnlyHandle`/`ListGraphOnlyHandle`
providers. Pin permanently with a `beadstest.RunStoreTests` run plus a
capability-matrix subtest over `Federated(Mem, SQLite…)`.

**Layering fit.** `beads` stays the untouched substrate; `Federated` is
composition *of* substrate, built in cmd/gc Layer 0 at the single wiring
chokepoint `routedPolicyStore` / `wrapStoreWithBeadPolicies` (`api_state.go:236`,
reached by `openStoreAtForCity` for every gc process) over the full resolved
class set. The `resolve*Store` helpers stay as per-class *openers* but stop being
handed out as primaries. The controller and the API projection both reach stores
only through `state.CityBeadStore()/GraphBeadStore()/BeadStores()`, so both
inherit it with zero object-model changes.

### 3.2 The modeling rule (two-id ops)

> Cross-class relationships are ref-by-id metadata (`gc.<x>_id` beadmeta keys),
> never bd dep edges. Dep edges are same-store facts.

Already proven twice (`gc.root_bead_id`, `gc.tracking_convoy_id`). Codify in
AGENTS.md invariants + `internal/beadmeta/keys.go`; enforce statically
(Guard 1), at runtime (`Federated.DepAdd → ErrCrossStoreDep`), and in CI (the
harness dangling-edge sweep). Reverse lookups use federated `ListByMetadata`.
The classifier must be closed over dep/parent-connected components — anything
that must stay edge-linked must co-classify (as `ClassifyGraphPlan` already does
for whole plans), or the link degrades to ref-by-id.

### 3.3 "Every object gets whatever substrate it wants" — preserved

The class→backend map stays `[beads.classes.<class>].backend`; openers stay
pluggable via `classBackendOpeners`; the legacy `graph_store` knob and the graph
on-disk location invariant are untouched. `Federated` is constructed *from*
whatever the resolvers return — adding a backend or moving a class changes one
`Route`, and no object-model code notices. The split is invisible to callers;
the mapping is fully free.

## 4. Test infrastructure — why `go test` never catches these

Three stacked root causes:

- **RC1 — topology collapse.** `resolveClassStore` returns the work store when
  a class backend is `bd` (`class_store.go:164`), which is the default for unset
  classes; nearly every test uses `&config.City{}`, so all six classes collapse
  to one handle and the split machinery self-bypasses. The only two-store
  fixture (`newCutoverEnv`) is graph-only.
- **RC2 — fidelity gap (the deepest).** `MemStore.DepAdd`
  (`memstore.go:453`) appends unconditionally and `SQLiteStore.DepAdd`
  (`sqlite_store.go:1066`) is `INSERT … ON CONFLICT DO NOTHING` with **no
  endpoint check**, while `BdStore.DepAdd` (Dolt, `bdstore.go:2465`) validates
  both endpoints and hard-fails. **Even a correctly split test topology would
  not have caught the convoy bug** — the cross-store dep-add silently "succeeds"
  as a dangling edge in-process and only fails on live Dolt.
- **RC3 — bugs live in composition.** Every fix patched object-model code but
  every violation was *created* by cmd/gc wiring; single-MemStore unit tests and
  per-store conformance are both blind to it.

### 4.1 Split-topology conformance harness

**Level 1 — `cmd/gc/split_topology_env_test.go`** (package main, where the real
wiring lives):

```go
type splitEnv struct { cityPath string; cfg *config.City; work, store, session beads.Store
                       class map[string]beads.Store; split bool }
func forEachTopology(t *testing.T, fn func(t *testing.T, e splitEnv)) {
    t.Run("bd",    func(t){ fn(t, newSplitEnv(t, false)) }) // &config.City{} — byte-identical
    t.Run("split", func(t){ fn(t, newSplitEnv(t, true)) })  // every class on a distinct store
}
```

`newSplitEnv(t, true)` builds every class store through the *production* openers
(`openClassSQLiteStore`, `resolveGraphStore`), prefix-disjoint, and asserts
distinctness. `TestSplitTopologyConformance` exercises each object-model surface
through the same call shapes production uses (convoy membership as drain does;
sling auto-convoy + `hasLiveTrackingConvoy`; the exact session Get→
SetMetadataBatch→SetMetadata→Update→Close reconciler sequence; drain control;
mail; orders; nudges). **Acceptance: reverting any of `d6479fbc0` / `ccebee78b`
/ `e057a2f69` makes `go test ./cmd/gc` fail.**

**Strict-store mode (closes RC2):** a ~20-line `strictDepStore` test wrapper
whose `DepAdd` `Get`s both endpoints in the same store and fails with the
bd-shaped `resolving issue ID %s: no issue found` before delegating, so
in-process tests reproduce the live Dolt failure.

**Residence + integrity sweep** (a `t.Cleanup` post-condition): every bead's
`Classify` maps to the store it lives in; every dep edge's endpoints both
resolve in that store; every id carries that store's prefix. Catches silent
misroutes and dangling edges that federation could otherwise hide.

**Level 2 — `internal/beads/splittest`** (exported kit): `NewSplitStores(t)`
returning prefix-disjoint strict stores so object-model packages run their own
tables under both topologies without importing cmd/gc.

**Opt-in:** table tests switch `beads.NewMemStore()` → `forEachTopology(...)`;
the bd row stays byte-identical, the split row is new coverage. A
`check-split-topology-rows.sh` guard (modeled on `check-routed-test-rows.sh`)
enforces both rows on any `topology:split`-marked file. Zero new CI jobs.

### 4.2 Static / CI guards (modeled on `TestNoUndeclaredMetadataKeys` +
the worker-boundary guard)

- **Guard 1 — `TestNoCrossStoreUnsafeDepAdd`** (AST): flag every `DepAdd`
  callsite outside the substrate/federation packages and a small audited
  allowlist (graph-apply plan builders, `molecule.Instantiate` — same-store by
  construction). Object-model code must call
  `storefed.SameStoreDepAdd(store, a, b, typ)`, which runtime-verifies
  co-residence uniformly.
- **Guard 2 — `TestStorerefStaysInsideFederation`** (import freeze): after the
  migration, freeze the current non-test importers of `internal/storeref` as a
  must-only-shrink allowlist. A *new* ad-hoc federation (the exact way the last
  three bugs were "fixed") becomes a build failure pointing at the unified layer.
- **Guard 3 — class/prefix coherence**: assert `ReservedClassPrefixes()`, the
  `coordclass.Class` enum, and `classBackendOpeners`/`resolve*Store` coverage
  stay in bijection. A new class without a prefix or resolver is a build failure.

Static guards enforce *chokepoints* and *vocabulary coherence*; they cannot
decide data-dependent facts (does this id resolve elsewhere; does this
Get-then-write span stores) — those are what the harness + sweep cover. Both are
required.

## 5. Phased migration

- **Phase 0 — harness** (pure test code). `splitEnv` + `forEachTopology` +
  strict DepAdd + residence sweep + `TestSplitTopologyConformance`. Acceptance:
  reverting any of the three fixes fails `go test ./cmd/gc`.
- **Phase 1 — guards.** Guard 1 (allowlist = today's callers), Guard 3, Guard 2
  (frozen at today's importers).
- **Phase 2 — introduce `storefed.Federated`.** Construct in `routedPolicyStore`:
  `policy(Federated(work, classify, routes…))`. Policy wrapper drops its private
  `graphStore`/`getForPolicy`/`createTarget` in favor of delegation. Run the full
  cutover + split suites.
- **Phase 3 — retire the ad-hoc federations, one PR each, each deleting code:**
  `classFederatedSessionStore`; `getControlBeadByID` + `findBeadAcrossStores`
  graph probing; `beadStoresForID`'s class-prefix arm; the `memberStores`
  variadic threading (then delete the params); the `(sessionStore, workStore)`
  pair-threading where it was purely residence-driven.
- **Phase 4 — enforce.** Flip `Federated.DepAdd` to `ErrCrossStoreDep`; shrink
  Guard 1's allowlist to the graph-apply builders; run the sweep over a live
  split city once as an audit (pre-fix dangling `tracks` edges are already
  tolerated by dual-read — no data migration).
- **Phase 5 — spread topology rows** across high-value object-model tables
  (session reconciler, dispatch control, sling finalize, order gates); enable the
  row guard in `make check`.

## 6. Immediate hotfix (independent of the migration)

Finding #1 is a **live regression on this branch** and should be fixed now,
before it bites a graph-resident-convoy deployment:

`internal/convoy/membership.go` — replace the `store.Get(convoyID)`-success
guard with a **physical-residency** check: add the legacy dep only when
`storeref.PrefixOwner(convoyID, stores) == storeref.PrefixOwner(itemID, stores)`
(or both resolve to the same handle), never on federated-Get success. Update the
`workOnlyReadStore` regression test to also cover a **federated-Get** policy
store (the post-`ccebee78b` reality), so the interaction is pinned. This is
subsumed by §3.1's `Federated.DepAdd` same-store enforcement once Phase 2 lands.

## 7. Residual risks

1. **Misclassification becomes silent routing, not an error** — federation finds
   the bead anyway; a Classify bug then only shows as wrong event/retention
   behavior. The residence sweep + a classify golden table are the oracle; keep
   them mandatory.
2. **Un-pinnable federated reads cost O(#stores)** and add partial-failure
   semantics (fail-closed chosen). Surface per-route health in city status.
3. **Prefix discipline is the routing keystone** — enforced by
   `ValidateReservedPrefixesIn` + Guard 3, but a migrated bead retaining a
   foreign prefix rides the slow probe path; `cmd_beads_migrate` should re-mint.
4. **Cross-process writers** (CLI subprocess bd writes) still route by env/cwd
   and stay a parallel mechanism until controller-mediated writes land.
5. **No cross-store transactions** — `Tx` stays single-store; multi-class
   invariants remain reconciler-repairable (they already are).
6. **Relation to `infra-store-decouple/DESIGN.md`** — frame `Federated` as the
   *seam-safety substrate* the typed-store extraction runs on top of: each class
   later removed from the shared surface shrinks `Federated`'s route table toward
   that doc's end state, rather than fighting it.
