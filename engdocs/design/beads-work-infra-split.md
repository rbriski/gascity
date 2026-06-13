---
title: "Splitting the beads substrate: work beads vs infrastructure data"
status: Proposed
date: 2026-06-13
---

> Plan to extract clean provider interfaces over the single `beads.Store`
> substrate so infrastructure beads (the formula-v2 graph explosion, sessions,
> mail, trackers) can move to a faster backend without a big-bang rewrite, while
> the real backlog (tasks/epics/convoys) stays in `bd`. Produced from a 10-agent
> code survey + a 4-architecture design panel; every claim is grounded in
> `file:line` evidence in this tree.

## Problem

Everything persists through one interface — `beads.Store`
(`internal/beads/beads.go:290-396`), backed by the `bd` CLI / Dolt. Formula v2
("graph.v2") runs explode bead counts: one bead per graph node (workflow root,
every step, gate, check, fanout, tally, drain, retry-eval, scope-check,
workflow-finalize, spec sidecar) plus a dense edge set (blocks, parent-child,
waits-for, and a `tracks` edge from every non-root node to the root). A realistic
run ≈ **11–29 beads + ~32 edges** at pour, +fragment-size×k per k-item fanout,
+2–3 beads per retry/ralph attempt. These land in the **same** issues/labels/
dependencies tables as human backlog tasks and trigger the same Error-1213
serialization-failure retry loop (`bdstore.go:1753`) against backlog writes.

Field-measured contention (`cmd/gc/doctor_fork_rate.go`): at load ~25, CPU ~66%
busy, **~96% of host forks come from bd.real + dolt + gc**, ~0.4% from agents.
`/status` was formerly 5–40s hydrating ~38k beads/rig/request (mostly NOT
backlog). `CachingStore` reconcile cadence keys off **total** bead count
(≥1000→60s, ≥5000→120s, `caching_store_reconcile.go:86-95`), so infra-bead volume
degrades work-bead cache freshness. Read:write ≈ **265:1** (ga-aec8q census).

## Desired end state

Two classes of persistence:

1. **Work beads** — the real backlog: tasks/subtasks/epics/convoys/
   merge-requests. Stay in `bd` (git-synced, human-visible). This store gets
   *simpler*.
2. **Infrastructure data** — everything that merely uses beads as a convenient
   store. Moves behind clean provider interfaces so a faster backend can be
   swapped in later.

## Ground-truth taxonomy (census)

The split is **not type-shaped** — this is the central constraint:

- **Work** (stay in `bd`): `type=task` (real backlog), `epic`, `bug`, `feature`,
  `merge-request`, `convoy` (user/sling), `spec`, pack types.
- **Infrastructure** (move behind providers), by **owning subsystem**. A class
  names *who owns the data*, not how it is stored — `Class` is orthogonal to
  `beads.StorageClass` (history/no_history/ephemeral), which is the physical
  tier. "Ephemeral" is a storage tier, never a class.
  - **graph** (the explosion): the formula-v2 engine's topology + control lane —
    `type=molecule/step/gate/scope/cleanup/run/retry-run`, every `gc.kind`
    control bead (`retry, ralph, check, retry-eval, fanout, tally, drain,
    scope-check, workflow-finalize, workflow, wisp, spec`), convergence beads,
    spec sidecars, and **synthetic convoys** (graph.v2 input + drain-unit,
    `type=convoy` + `gc.synthetic`). Owner: dispatch/molecule/formula.
  - **messaging**: `type=message` (mail), extmsg 8 families (**all `type=task`**,
    distinguished only by `gc:extmsg-*` labels; one transcript bead per message).
    Owner: internal/mail + internal/extmsg.
  - **sessions**: `type=session` (~50 churny metadata keys) and durable session
    waits (`type=gate` + `gc:wait`). Owner: internal/session. (`agent/role/rig`
    are config records with **no Go creation site** — read-only, not a routed
    class.)
  - **orders**: order-dispatch tracking (`order-tracking` label, NoHistory,
    ~3,500/day). Owner: internal/orders.
  - **nudges**: the nudge-queue durability mirror (`type=chore` + `gc:nudge`).
    Owner: the nudge-queue subsystem.

extmsg hides under `type=task`, nudge under `type=chore`, synthetic convoys under
`type=convoy`; and formula graphs **embed** work-typed `bug/epic` steps inside
infra graphs (`molecule.go:585-586` keeps them `type=task` so they stay
claimable). **You cannot classify by type alone.**

## Prior art — the decisive lesson

A previous program (**ga-aec8q**, "HQ coordination-state store", May 2026)
already censused every bead kind and measured the access profile (265:1 read; p99
targets: point-read 1ms, scans 10ms, create/update 5ms, mail-poll 150/s, restart
catalog ≤5s at 10k records). It then **built two full replacement backends**
(HQStore #2590, pure-Go SQLite coordstore #2990), soaked them, and **deleted both
within ~3 weeks** (#2873, #3151, #3155). `cmd/gc/main.go` now hard-errors on those
provider names. What *survived* was optimizing the **same** store in-process
(`DoltliteReadStore`, `NativeDoltStore`) plus storage-class routing.

**The lesson this plan is built around: a parallel backend without an
interface-first migration story, conformance tests, and an ownership story gets
removed.** Recoverable evidence (verified present in this repo's object DB):

- `quad341/builder/ga-aec8q-12`: `docs/coordination-store/discovery.md` +
  `findings/S1`–`S6` (commits `4ded75f23`, `e62abb98a`, `97fa6757f`). **Reuse this
  data; invert its "build HQStore" conclusion.**
- Removed 21-method `StoreAdapter` SPI: `git show
  beeac65b7^:internal/benchmarks/coordstore/adapter.go` (Create/Get/Update/Delete,
  FilterScan/BatchGet, SetMetadataBatch, Ready, Dep ops, PurgeExpired/
  PurgeTerminal, PrimeScan/RecentScan, Stats). Recover as a **reference checklist**
  for completeness — not to resurrect the monolithic adapter (that wide shape is
  what got deleted).
- `engdocs/design/beads-dolt-contract-redesign.md` (Accepted 2026-04-11): one
  canonical store-target contract, no new parallel control planes.

## Recommended architecture

**A storage-class-routed facade grown from the existing policy chokepoint, with
typed per-class backend interfaces, federate-before-relocate discipline, and the
Ready oracle extracted as its own gated workstream.** (Design panel: this spine
scored highest after grafting the typed interfaces from the maximal-separation
approach and the strangler/federate ordering from the explosion-first approach.)

Build on what already exists rather than inventing:

- `wrapStoreWithBeadPolicies` (`cmd/gc/bead_policy_store.go`) is **already** the
  single chokepoint that classifies every write into
  wisp/workflow/order_tracking/session/wait/nudge. Promote it from a tier-selector
  into a per-class **router**.
- `beads.GraphApplyStore` + `GraphApplyPlan/Node/Edge`
  (`internal/beads/graph_apply.go`) is **already** a working graph-provider
  contract with three implementations. It owns graph *writes*; it only needs
  read-back methods to become the full `GraphStore`.
- `mail.Provider` is **already** the message seam; `beadmail` is one impl.
- The optional-capability `For()` pattern (Counter, StoreHandles{Cached,Live},
  ConditionalAssignmentReleaser, ParentProjectionWaiter) is the established way to
  add capabilities without widening `beads.Store`.
- `StorageClass{history,no_history,ephemeral}` + `TierMode` already exist as the
  within-backend physical tier knob.

### The boundary rule

Classification stays a **runtime decision** (not a type) computed by one pure
function, the single source of truth for the router write path, the Ready
federation, and the wire-layer List federation:

```go
func Classify(b beads.Bead) Class          // Work | Graph | Messaging | Sessions | Orders | Nudges
                                           // (the OWNERSHIP axis — orthogonal to beads.StorageClass)
func ClassifyGraphPlan(p *beads.GraphApplyPlan) Class
```

Extracted from `policyNameForBead`/`policyNameForGraphPlan`/`IsReadyExcludedBead`.
**Honest correction the panel surfaced:** today's classifier returns `""` (→Work)
for `message`, extmsg, and synthetic-convoy beads
(`bead_policy_store.go:236-253` only handles wisp/order_tracking/session/wait/
nudge/workflow). So `Classify` **adds net-new arms** for `ClassMessaging`
(`type=message` OR `gc:extmsg-*` label) and folds synthetic convoys
(`gc.synthetic`) into `ClassGraph`. These carry real behavior risk and are pinned
by a **golden table with one row per census kind** (seeded from the recovered
ga-aec8q `S1-entities`).

Routing unit for a graph is the **plan** (via `ClassifyGraphPlan`), not the node —
so a recipe with an embedded work-typed step lands wholesale in `ClassGraph`,
preserving intra-graph edges; that step stays claimable because
`GraphStore.ReadyCandidates` surfaces it by **routing metadata** (`gc.routed_to`),
never by `type=step`. Conformance pins the `is_blocked`/ready-blocking projection
to **always** live in the Work backend, so the single demand oracle can never
split.

### Seams (interfaces)

Each class is its own typed module with first-class accessors. Every module's
first implementation is a thin bd-delegating adapter (a pure extract-interface
refactor); a faster backend slots in behind the same interface later.

| Module / interface | Owns | First impl |
|---|---|---|
| **`coordclass`** (`internal/coordclass`) | the `Classify`/`ClassifyGraphPlan` boundary function + the `Class` enum — the single source of truth, consulted by the router, the Ready federation, and the wire List federation | pure function, pinned by the golden table **(shipped — PR1)** |
| **`ClassRouter`** (`internal/coordrouter`) | the per-class backend registry; re-casts `beadPolicyStore` | `Register(every Class → same cs-wrapped bd store)` — provably an identity transform |
| **`GraphStore`** (ClassGraph) | the explosion: molecule/step/control beads, convergence, spec, synthetic convoys; the `gc.graphv2_root_key`/`item_root_key`/`drain_unit_key`/`input_convoy_id` idempotency namespaces | `bdGraphStore` over `GraphApplyFor(cachedBdStore)`; reads delegate to `Children`/`DepList`/`Get` |
| **`MessageStore`** (ClassMessaging) | mail + extmsg persistence seam (the services sit *on top*, not folded in) | `beadmail` (already satisfies `mail.Provider`) |
| **`SessionsStore`** (ClassSessions) | session lifecycle + durable session waits; `ResolveSessionID` makes the session-bead-ID FK explicit; `ReleaseIfCurrent` is today's CAS | bd-delegating; keyed-upsert + CAS + change-feed shape |
| **`OrdersStore`** (ClassOrders) | order-dispatch tracking; recency query by `order-run:<scoped>` label, stale sweep | bd-delegating; `FindOrCreateByKey` + recency/sweep |
| **`NudgesStore`** (ClassNudges) | nudge-queue durability mirror; ensure-by-`nudge_id`, terminalize, TTL sweep | bd-delegating |

`GraphStore` method sketch (the largest new interface):

```go
ApplyGraphPlan(ctx, plan) (*GraphApplyResult, error)              // exists today
ApplyGraphPlanWithStorage(ctx, plan, StorageClass) (...)          // exists today
GetNode(ctx, id) (beads.Bead, error)
ListNodesByRoot(ctx, rootID, opts) ([]beads.Bead, error)         // replaces runtime.go Children/DepList walks
ListNodeEdges(ctx, id, direction) ([]beads.Dep, error)
CloseSubtree(ctx, rootID, md) (int, error)
FindOrCreateByKey(ctx, idemField, key, plan) (*GraphApplyResult, bool, error)  // promotes the racy striped-mutex idempotency
ReadyCandidates(ctx, q ReadyQuery) ([]beads.Bead, error)         // claimable by routing metadata, NOT type=step
```

`WorkStore` stays **`= beads.Store`** — a documented marker alias, *not* a new
interface — so `beads.Bead` remains the wire type with zero OpenAPI/genclient/
dashboard churn. The policy-wrapped `beads.Store` stays the federation surface
(its `List`/`Ready` become per-backend fan-out); we deliberately **reject** a wide
umbrella `InfraStore` facade (the panel flagged it as the next god-object).

Every class gets a real typed module with first-class accessors (the goal:
`sessions`, `orders`, and `nudges` are owned subsystems, not a storage-tier
bucket). The interfaces are introduced as each class's adapter lands; the *first*
implementation behind each is always bd-delegating, so introducing the interface
is itself a no-op refactor. Interface *surface* grows only as a consumer needs a
method — no speculative methods ahead of a caller.

## Phase plan

Every phase is independently shippable, behind a config flag, revertible by a
one-line re-register. **Phases P0–P3 keep one physical bd backend** — they are
pure refactors verified to be byte-identical. Only P4–P5 move data.

| Phase | Goal | Exit criteria |
|---|---|---|
| **P0** Recover + freeze + harness | Land ga-aec8q docs as requirements-of-record; `internal/coordrouter` (Class enum, `Classify`, interface decls, `ClassRouter` skeleton); `RunGraphStoreTests`/`RunClassedStoreTests` (skipped); characterization test pinning today's `policyNameForBead` output. **Zero production call sites; zero `beads.go` edits.** | Golden classify table green (one row per census kind); suites compile; docs landed |
| **P1** Router as identity transform | Re-point `wrapStoreWithBeadPolicies` to construct the router; `Register` every class to the **same cs-wrapped bd store** (correction: wrap the *cached* store, not raw bd, per `api_state.go:188`). Land bd-delegating adapters. | **Entire existing suite green** (beadstest, storage-conformance, controller/demand, `TestOpenAPISpecInSync`, `make dashboard-check`) + differential byte-identical test for a full graph.v2 pour |
| **P2** Federate reads + Go demand scan | Make router `List/Ready/Get/DepList` fan-out-aware; route `GET /v0/beads`, `gc beads list`, `GET /v0/convoy/{id}`, and `collectAssignedWorkBeadsWithStores` (`build_desired_state.go:1005`) through it. Introduce `RouterReady = union(work.Ready, graph.ReadyCandidates)` as the **single named Go oracle**. | `union(work.List,graph.List)==legacy List` and `RouterReady==legacy bd ready` over an all-classes fixture, byte-identical; wire schema unchanged |
| **P3** The `gc ready` shell-oracle workstream | Build a `gc ready` CLI rendering the **identical** predicate to `bdReadyPoolDemandShell` (`config.go:3272-3439`, all three branches) but resolving through `RouterReady`. Then flip `EffectiveWorkQuery` tier-3, `EffectivePoolDemandQuery`, `gc hook --claim`, **and every agent-prompt/skill-pack template together** in one commit. | `gc ready` byte-identical to `bd ready` across routed/run_target-fallback/ephemeral branches; a prompt-render test asserts every templated query points at `gc ready` |
| **P4** First real swaps: Messaging → Orders → Nudges | Register a non-bd `MessageStore` (lowest coupling: mail beads are ephemeral-tier, never in Ready, FK by id only, existing `mailtest` conformance), then `OrdersStore` and `NudgesStore` (high-churn, NoHistory, no inbound blocks edges — directly attacks fork rate). Delete the matching `readyExcludeTypes` entries (coupling 10). | New backend passes `RunClassedStoreTests`; `doctor_fork_rate` soak green vs recovered p99 targets; exclusion-deletion test green |
| **P5** Relocate Sessions → Graph (the explosion) | Migrate sessions, then graph topology, to a faster backend (DoltLite-native or the recovered coordstore methodology). Graph **last** — its Ready/claim, cross-class deps, and finalize are all already exercised by P2–P4. Re-point `ResolveStoreRef`/`SourceWorkflowStores` to be provider-aware. | Split-topology `is_blocked` test green; orphaned-source finalize regression green against provider-routed path; cache cadence demonstrably keys off **work** count only |

## How the 10 hard couplings are solved

1. **Single Ready/claim oracle — the *shell string*.** The decisive,
   under-weighted coupling. The demand oracle is `bdReadyPoolDemandShell`
   (`config.go:3272-3285`) — a literal `bd ready --metadata-field
   gc.routed_to=$target --unassigned --exclude-type=epic --json` rendered **into
   agent prompts, skill packs, and the reconciler**, executed inside agent shells.
   When graph leaves bd, that CLI stops seeing claimable steps. Solved as two
   owned surfaces: (a) Go side — `RouterReady = union(work.Ready,
   graph.ReadyCandidates)` fed to the controller, `EffectiveWorkQuery`, and
   `EffectivePoolDemandQuery` so they cannot diverge (`config.go:3251-3380` warns
   against exactly this); (b) shell side — the dedicated `gc ready` workstream
   (P3). Claimable steps keep `type=task`, so the predicate selects on
   `gc.routed_to`, **not** `type=step`.
2. **Cross-class dependency edges** (`molecule.Attach` `DepAdd(workBead,
   infraRoot, 'blocks')` `molecule.go:299`; `Options.ParentID` parents infra root
   under work bead `molecule.go:564`). Through P0–P4 both endpoints share the one
   bd backend, so `is_blocked`/`closeorder` are unchanged. Pinned invariant: the
   blocks edge + `is_blocked` projection **always** live in the Work backend;
   `GraphStore` is forbidden from owning ready-blocking deps. Only bites at P5,
   gated by a split-topology `is_blocked` conformance test. Honest deferral — the
   genuinely hard part, flagged as a P5 risk.
3. **`workflow-finalize` closes Work across the boundary** (`runtime.go:673-915`).
   Already cross-store and interface-mediated via `ProcessOptions.ResolveStoreRef`
   + `SourceWorkflowStores` + `SourceWorkflowLock` (`runtime.go:55-63`). Re-point
   `ResolveStoreRef` to be provider-aware rather than building new machinery; the
   live-root guard stays verbatim under the lock. (`CloseSubtree` must slot behind
   a `beads.Store`-satisfying adapter — a small real refactor, accounted.)
4. **Bidirectional foreign keys.** All FKs are **string ids in metadata**, never
   DB constraints (`gc.root_store_ref`, `city:`/`rig:` already cross stores as
   strings). IDs stay stable because P0–P4 keep one ID space and adapters never
   re-mint. Canonical cross-provider FK = `(storeRef, id)`. `ResolveSessionID`
   makes the session-bead-ID FK explicit/store-stable. Conformance: every FK
   written by class A resolves through the router's `Get` for class B.
5. **`beads.Bead` is the wire type.** `WorkStore = beads.Store` marker alias →
   `TestOpenAPISpecInSync` + `dashboard-check` green by construction. `GET
   /v0/beads` = router `List` fan-out; `GET /v0/convoy/{id}` resolves to the owning
   backend on one response schema. Honest exception: `mail.Provider` traffics
   `mail.Message`, layered *above* the routed `beads.Store`.
6. **Idempotency (racy striped mutex).** `FindOrCreateByKey` promotes it to a
   first-class upsert. bd adapter wraps today's
   `ListByMetadata(WithBothTiers)`+mutex verbatim (zero behavior change; the
   cross-controller race is **not** closed yet — stated plainly). The interface
   makes store-level uniqueness a contract a future backend satisfies with a real
   unique index.
7. **Watch/notify coherence.** **Layering correction:** today is
   `wrapStoreWithBeadPolicies(CachingStore(bd))` — policy *outside* caching
   (`api_state.go:188`). The router wraps the **cached** store; the single bd-hook
   → event → `ApplyEvent` stream is preserved. The win banks at P5: the Work
   store's reconcile cadence then keys off **work** count only. Each relocated
   backend must expose a `ChangeFeed()` or document it reuses the bd hook stream;
   a watch-coherence subtest forbids going silent.
8. **Transient-error classification (string-matching).** Define typed
   `ErrRetryableContention` vs `ErrHard`. Each backend maps its native errors at
   its edge; `IsTransientControllerError` checks `errors.Is` first, string-needle
   fallback second — existing call sites unchanged.
9. **Storage-class metadata-sniffing** (synchronous `Get(rootID)` per child
   create, `bead_policy_store.go:182-194`). Make `StorageClass` an explicit
   caller-supplied param; the class is computed **once** at the root and graph
   children inherit it in-memory from the plan, eliminating the per-child fork.
10. **`readyExcludeTypes` + repairable taxonomy + custom-type registration** are
    the **inverse image** of the split. Treated as **deletions** scheduled
    per-class *after* the owning provider leaves the work store — never ported.
    Each deletion is a separate revertible commit gated by a test asserting the
    work store never receives that type and `Work().Ready` is unchanged. **The
    deletion is the proof the split is complete.**

## Test & migration story (the defense against the removed-backend pattern)

Three conformance layers, all built on the **existing**
`beadstest.RunStoreTests` / `mailtest` / `native_dolt_store_conformance_test.go`
patterns:

1. **Per-class conformance suites** (`RunClassedStoreTests`, `RunGraphStoreTests`)
   — the bd adapter *and* every future fast impl run the **identical** suite. A
   clause a backend cannot meet uses `beadstest.Options{Skip+Reason}` naming an
   escalation bead — gaps are documented and tracked, never hidden.
2. **No-behavior-change verification** (P1–P3) — a differential/golden test pours
   a full graph.v2 run through both the legacy `beadPolicyStore` path and the
   router against the **same** bd store, asserting byte-identical beads + edges +
   tiers + wire JSON after canonicalizing ids; `RouterReady == bd ready` and
   `gc ready == bd ready` across all predicate branches.
3. **Cross-class invariant tests** — orphaned-source finalize regression, the
   FK-resolvability invariant, and (P5) the split-topology `is_blocked` test.

**Migration:** the first real backend (P4 mail, then order-tracking) flips behind
a config flag whose default stays `bd` until a `doctor_fork_rate` soak meets the
recovered ga-aec8q p99 targets. Both impls pass the same suite, so a swap cannot
silently change semantics; a one-line re-register reverts.

## First three PRs

1. **PR1 (P0):** Recover ga-aec8q docs into `engdocs/` as requirements-of-record;
   add `internal/coordrouter/{classify.go,classify_test.go}` with the golden table
   (explicitly pinning the net-new message/extmsg/synthetic arms); interface
   declarations + skipped conformance skeletons; characterization test for today's
   classifier. Dead code under test — zero hot-path risk, zero `beads.go` edits.
2. **PR2 (P1):** Re-point `wrapStoreWithBeadPolicies` to the `ClassRouter`,
   register every class to the same cs-wrapped bd store, land bd-delegating
   adapters, behind a config flag defaulting off. Gated by the full existing suite
   + the differential byte-identical test.
3. **PR3 (P2):** Make router `List/Ready/Get/DepList` fan-out-aware; route the wire
   read endpoints + the controller demand scan through it; introduce `RouterReady`
   as the single named Go oracle. Still all-bd, so byte-identical. Sets up P3
   without touching agent prompts.

## Open questions (team decisions)

- **Config selector shape:** per-class provider names in `city.toml` (extend
  `Beads.Policies`) vs a single coord backend with capability negotiation? Decide
  before P4.
- **extmsg ownership:** does `ClassMessaging` own only the transcript-bead
  persistence seam (services keep their `beads.Store` handle), or a richer
  transcript interface? Recommend the narrow seam first.
- **Split-topology `is_blocked` (P5):** keep the blocks edge + projection in the
  Work store addressing a foreign target by id (chosen here), or federate
  `is_blocked` recomputation in Go? Confirm cross-store status-lookup latency
  before P5.
- **Does `ClassGraph` ever need to physically leave bd?** Most contention relief
  (fork rate, cadence) may be banked by relocating orders + nudges + messaging +
  sessions alone. If the P4 soak already meets the fork-rate target, P5's graph
  relocation (the hardest, all the cross-class coupling) may be deferrable — the
  interface still proves the seam.
- **Multi-controller idempotency timeline:** is closing the cross-controller
  `FindOrCreateByKey` race a hard requirement for the first fast backend, or
  deferrable behind the documented Skip+Reason escalation bead?
- **`agent`/`role`/`rig`:** no Go creation site today, so not a routed class. If
  a pack ever creates them in Go, add a read-mostly `registry` class.

**Decided:** `orders` and `nudges` stay separate modules (honest ownership —
distinct subsystems). `convergence` roots (`type=convergence`) **fold into
`graph`** — a deliberate net-new arm in `Classify`, pinned by the golden table;
the convergence engine's state travels with the graph it pours. (Class is
orthogonal to `StorageClass`, so this changes only the destination backend once
graph relocates, not the storage tier during the identity-transform phases.)
