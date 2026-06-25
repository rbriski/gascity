---
title: "Infra/beads decoupling — hardened design (domain-typed stores, full arc to SQLite)"
status: Approved-for-planning
date: 2026-06-24
supersedes_emphasis_of: engdocs/design/beads-work-infra-split.md
branch: plan/decouple-infra-beads (off fix/awake-readiness @ 204b66aee)
---

> Authoritative, adversarially-hardened design for removing beads/bd from Gas City
> *infrastructure* concerns. Produced by a 10-agent exploration → 4-architecture +
> 5-deep-dive design panel (judged) → 7-adversary + staff-eng hardening pass. Every
> `file:line` is pinned to the branch HEAD above and MUST be re-verified at execution
> time (line drift is expected across checkouts).

## 0. The mandate (binding owner decisions)

1. **Full domain types to the wire.** `session.Info`, `mail.Message`, `orders.OrderTracking`,
   `nudgequeue.Item` (and the rest) are the currency for consumers AND flow through the
   HTTP/SSE API and the dashboard. `beads.Bead` is translated to/from the domain type
   ONLY at the storage-row edge. OpenAPI + dashboard TS regen is in scope. **Full wire for
   every class now** — typed `GET /v0/sessions`, `/v0/orders/tracking`, `/v0/nudges`,
   `/v0/convoys` are all built, regardless of current consumer.
2. **Full arc to SQLite.** The plan covers BOTH the domain-interface extraction AND the
   physical relocation of every infra class onto SQLite.
3. **Design from first principles; delete the Router.** The `coordrouter.Router` is prior
   art, not a foundation. The central runtime `Classify(bead)->Class` router does NOT
   survive (see §2).
4. **extmsg: mail first, extmsg after.** The "messaging" first cut is **mail only**.
   extmsg (~10k LOC, 8 `gc:extmsg-*` families, all `type=task` on the **history** tier) is a
   separate follow-on epic (§11) with its own `ExtMsgStore` + history backfill — it cannot
   live in a `mail.Message`-shaped store.
5. **Controller-mediated writes for relocated infra.** Each relocated infra class's SQLite
   file has a **single in-process writer in the long-lived controller** (`MaxOpenConns=1`
   = in-process serialization). CLI/other-process *writes* route through a controller API;
   *reads* open their own read-only WAL handle per process. This eliminates cross-process
   WAL write contention by construction (§8).
6. **Conformance-only validation; accepted prod risk.** No dual-write, no shadow-read soak.
   The per-class conformance suites + the fork-rate soak are the ONLY oracle. The risk
   "first divergence is discovered in production; revert is a quiesce-then-flip-back at a
   maintenance window" is **explicitly accepted** — which raises the bar on conformance
   depth (the golden round-trip and projection-invariance subtests are load-bearing safety).

## 1. End state

Two physical persistence tiers, reached strangler-style (per-class, config-flagged):

1. **Front-door work store (bd/Dolt).** Holds ONLY true `ClassWork`:
   `task`/`epic`/`bug`/`feature`/`merge-request`/`spec`/pack types and **user/sling convoys**.
   `beads.Bead` is both the storage row and the wire type here; `GET /v0/beads` stays
   (`internal/api/huma_handlers_beads.go:18`). This store gets *simpler* as infra leaves.
2. **Infrastructure stores on embedded SQLite.** Sessions, mail, order-dispatch tracking,
   the nudge-queue durability mirror, durable session waits, and the formula-v2 graph engine
   each live behind a **fork-owned, domain-typed store interface** its owning subsystem calls
   directly, built on the proven `internal/beads/sqlite_store.go` substrate. The graph class
   is **already on SQLite** in production under `[beads] graph_store="sqlite"`
   (`cmd/gc/api_state.go:240`,`:317`) — the proof-of-pattern this generalizes.

## 2. The router verdict (sound — re-verified in hardening)

**No central runtime `Classify(bead)->Class` router survives.** It exists only because every
subsystem funnels through one `beads.Store`, forcing the store to re-derive ownership the
caller already knew. Three mechanisms — all verified — are dead weight once each subsystem
holds a typed store it calls directly:

1. Create-time classification — `internal/coordrouter/router.go:113` `Create`→`coordclass.Classify`.
2. By-id Get-probing — `internal/coordrouter/router_mutation.go:21` `backendForID`.
3. Read federation — `internal/coordrouter/router_federation.go:242` `federateRead` union.

The Router is the LIVE `graph_store=sqlite` wiring today (`api_state.go:240-244`, the sole
non-test `coordrouter.New` call site; the default city has no Router), so it is **retired
LAST, class-by-class, never big-bang** (§ phase P6). Both `coordclass`/`coordrouter` are
fork-owned, so deletion is grep-provable and zero merge cost.

**What survives (NOT a classifier), each needing its own conformance coverage:**

| Residual | What it is | Verified site |
|---|---|---|
| Front-door **WorkStore** (bd) | serves `GET /v0/beads` + the `gc.routed_to` Ready/claim oracle | `huma_handlers_beads.go:18`; `config.go:3356` literal `bd ready --metadata-field gc.routed_to=…` |
| **`(storeRef,id)` FK resolver** | cross-store `workflow-finalize` lookup; the ~20-line `IDPrefix` switch (`gcg-`→graph, `gc-`/`ga-`→work, new `gcm-`/`gco-`/`gcn-`/`gcs-`) | `internal/dispatch/runtime.go:48-63`; template at `api_state.go:630` |
| **`ClassifyGraphPlan`** | plan-WHOLESALE routing of embedded work-typed steps — **MOVED** from `coordclass` into `internal/graphstore` (carries `classifyFields`/`isWispMetadata`) | `internal/coordclass/classify.go:88` |
| **`classifyBdShimVerb`** | the class-blind shell oracle `bd ready/close <id>` the agent emits — a CLI verb dispatcher, not a `beads.Store` Router | `cmd/gc/cmd_bd_shim.go:28-58` |

`coordclass.Classify(bead)` is **demoted to a test/audit-only census tool** (its golden table
stays the spec for each adapter's `ToBead`/`FromBead` and for the `readyExcludeTypes` deletion
proofs); it leaves the hot write path.

## 3. Layering (corrected)

```
Layer 0 — substrate (side effects)
  internal/beads — beads.Bead, beads.Store, BdStore→Dolt, SQLiteStore (+ a NEW injected
    raw-RowChanged emitter, §7), CachingStore, MemStore. UNCHANGED as a substrate; NO
    graph-only methods added to beads.go for the split.
Layer 1 — domain ownership (fork-owned types + interfaces, IN the owning package)
  internal/session    — session.Info (REUSE; do NOT invent session.Session) + SessionStore +
                        the WAIT sub-entity (durable session waits ride here).
  internal/mail       — mail.Message + MailStore (the existing mail.Provider is the service
                        ABOVE this seam).
  internal/orders     — orders.OrderTracking (NEW struct; NOT orders.Order the TOML def) + OrderStore.
  internal/nudgequeue — nudgequeue.Item (REUSE) + NudgeStore.
  internal/graphstore — NEW: GraphStore + the MOVED ClassifyGraphPlan.
  internal/convoy     — ConvoyStore (user→work, synthetic→graph; split is a static call-site fact).
Layer 2 — services
  session.Manager, mail services, order dispatch, nudge terminalization, dispatch/molecule/sling.
  Each receives its typed store via constructor injection. Row↔domain runtime ENRICHMENT
  (transport/ACP/IsRunning/Attached/LastActive) lives HERE, not in Layer 0 (§4 infoFromBead).
Layer 3 — projections
  internal/api — per-domain typed Huma handlers; wire = generated DTO over the domain type;
    the per-domain typed event PAYLOADS live here (event_payloads.go); GET /v0/beads stays
    beads.Bead. SessionStore is threaded at the per-request session.Manager construction
    (internal/api/session_manager.go) — NOT at cmd/gc/api_state.go.
  cmd/gc — CLI + composition root; picks bd|sqlite per class; controller-mediated infra writes.
```

**Invariants:** no upward deps (the store emits only a raw row-change; domain-typed payloads
are built in Layer 2/3 — §7); side effects in Layer 0; api/cmd are projections; ZERO
hardcoded roles (routing = which typed store a constructor was handed); serialization at the
two edges (storage-row + wire), domain types as the type-safe middle.

## 4. Domain entities & the storage-row edge (corrected)

**Modeling rule.** Promote a field to a typed SQLite column ONLY when a store predicate
selects/orders on it OR a wire consumer needs it typed; everything else goes in a per-entity
**versioned typed extras blob** `{v int; known typedStruct; unknown map[string]string}`
(NOT `map[string]any`). The `unknown` passthrough captures every metadata key this binary did
not enumerate so the bd-delegating phase round-trips losslessly.

**The reconstruct contract (load-bearing, was missing).** `row → domain` MUST union
*promoted columns + extras.known + extras.unknown* into a single metadata map before any
projection runs. Conformance subtest asserts **key-for-key equality** of the reconstructed
map vs the original `b.Metadata` (not merely equal projection output), and forbids any
projection key living in BOTH `known` and `unknown` (double-write/drift guard).

| Entity | Package | Notes / corrections |
|---|---|---|
| `session.Info` (`manager.go:73`) | `internal/session` | Row edge = **PERSISTED-FIELD CODEC ONLY**. `infoFromBead` (`manager.go:1543`) is an **impure `*Manager` method** (calls `transportForBead`, `routeACPIfNeeded`, `sp.IsRunning/IsAttached/GetLastActivity`) — it stays in Layer 2. Only the pure metadata-decode subset moves to the row edge; `ProjectLifecycle` (`lifecycle_projection.go:374`, already pure) is unchanged and pinned by a projection-invariance subtest over the **persisted subset**. |
| `mail.Message` (`mail.go`) | `internal/mail` | The one place the domain type legitimately IS the wire type — do not DTO-ify. Promote `thread_id`/`read` to indexed columns (kills the label-as-index hack). **Model `mail.Message.Rig` (`mail.go:54`) and collapse the read label/metadata double-encode** (was unmodeled). |
| `orders.OrderTracking` (NEW) | `internal/orders` | Maps the `order-tracking`/`order-run:<scoped>`/NoHistory bead. **NO `UNIQUE(scoped_name, run_key)`** — `run_key` does not exist; single-flight is the multi-tier predicate `hasOpenWorkStrict` (`order_dispatch.go:1468`: open tracking OR wisp root with open descendants, EXCLUDING orphan all-closed roots). Re-spec single-flight as a typed Go query over the order store + a graph read path (§8). |
| `nudgequeue.Item` (`state.go:31`) | `internal/nudgequeue` | Maps `type=chore`+`gc:nudge` shadow. The live flock-file queue stays source of truth; the table is its shadow. **Fix the oracle-leak BEFORE relocation** (§9.10). |
| **WAIT** (durable session waits, `type=gate`+`gc:wait`) | `internal/session` (rides in SessionStore) | Was omitted. Session-close → `WithdrawWaitNudges` bridges to the NUDGE store across a boundary — the close path must pass the nudge store (or be cross-store-aware via the prefix resolver). Conformance: session close terminalizes its wait-linked nudge shadows when sessions and nudges are in different stores. |
| graph node set | `internal/graphstore` | Already on SQLite; `GraphStore` stays `beads.Bead`-shaped at its read surface (topology/edge-centric, embeds work-typed steps). |
| convoy | `internal/convoy` | User convoys = `ClassWork` (stay on bd, `beads.Bead` wire). Synthetic (`gc.synthetic`, `classify.go:156` `isSyntheticConvoy`) travel with the graph. The split is the caller's choice of which `ConvoyStore` to construct — no runtime sniff. |

**Per-class store scope.** Each relocated class is a **single city-scope SQLite store with one
global id sequence** (mirroring graph), one file `<cityPath>/.gc/<class>.sqlite`. Reserve
`gcm-`/`gco-`/`gcn-`/`gcs-` as **non-configurable** prefixes validated against
`rig.EffectivePrefix()` with a longest-prefix precedence rule. A concurrent-process conformance
test asserts non-colliding ids and no `SQLITE_BUSY` escaping `retryOnBusy`.

## 5. Wire strategy (full wire, every class)

The wire type is a **per-domain generated Huma DTO over the domain type**, EXCEPT where the
domain type is already serialization-clean (`mail.Message`). Translation lives at two edges
per domain: the storage-row edge (bead↔domain) and the wire edge (domain↔DTO, in
`internal/api/handler_<x>.go`'s `to<X>Response`).

- A DTO (not the raw struct) keeps the JSON contract stable across internal field churn —
  `session.Info` carries Go-only fields (runtime handles, `*time.Time`) that must not hit JSON.
  Re-root the EXISTING `sessionResponse` DTO on `session.Info` and move the metadata-decode
  translation down to the store row edge (runtime enrichment stays in Manager).
- New typed endpoints (all built now): `GET /v0/sessions`, `GET /v0/orders/tracking`,
  `GET /v0/nudges`, `GET /v0/convoys`. `mail.Message` flows directly; `GET /v0/beads` stays
  `beads.Bead`.
- **Convoy member shape:** keep an **aggregated members array** (status/assignee/created_at per
  member) in `convoyResponse` — member-by-id alone is a BREAKING change to `convoys.ts`. If a
  rewrite is chosen instead, it is an explicit, vitest-covered work item.
- **CI gate inventory (complete):** per DTO PR you must (a) regen the 3 openapi files
  (`TestOpenAPISpecInSync`), (b) `npm run gen` and **commit** `src/generated/schema.d.ts` + the
  openapi-ts SDK (the generated-TS-schema-drift gate), (c) pass **Vitest**, (d) `make
  dashboard-check`. No `map[string]any`/`json.RawMessage` on any DTO; all Huma-registered.
- The `GET /v0/beads?type=molecule` graph-workflow-root augment (`huma_handlers_beads.go:114-130`)
  relies on Router federation today — it is explicitly re-wired to `GraphStore` in P6 before the
  Router is deleted.

## 6. Event & projection strategy (corrected — Layer-0 raw emit only)

Today two change-feeds feed the bus, both assuming one `beads.Store`: the bd-hook path
(`bd` mutation → `.beads/hooks/*` → `gc event emit`; `on_close` also shells
`gc convoy/wisp/molecule autoclose`) and the controller-cache path
(`CachingStore` write → `onChange` → `Recorder`; `startBeadEventWatcher` re-applies
`bead.{created,updated,closed,deleted}` via `ApplyEvent`). A relocated SQLite store fires NO
bd hooks. **`ChangeFeed` does not exist anywhere in `internal/` and `SQLiteStore` fires zero
events today — this is GREENFIELD.**

Corrected strategy:

1. **Layer-0 emits ONLY a raw row-change.** Add `WithSQLiteStoreRecorder(emit RowChangeEmitter)`
   to `SQLiteStore`. After every committed mutation it emits a low-level `RowChanged{StoreRef,
   ID, Op}` carrying primitive/row data (an `internal/events`-safe type, mirroring the existing
   `json.RawMessage` `onChange` contract). **It does NOT emit domain-typed payloads** — those
   live in `internal/api` and emitting them from Layer 0 is an upward dependency.
2. **Layer 2/3 translates** the row-change into the domain-typed event
   (`MailEventPayload`/`SessionLifecyclePayload` already exist at `internal/api/event_payloads.go`;
   add typed convoy/order/nudge payloads; `RegisterPayload` is at `internal/events/payload.go:51`;
   each new named payload auto-joins the SSE `EventPayloadUnion` dedup at
   `convoy_event_stream.go`). Keeps `TestEveryKnownEventTypeHasRegisteredPayload` green.
3. **Emit OUTSIDE the `retryOnBusy` closure / after `Unlock`.** `MaxOpenConns=1` makes an
   in-closure emit deadlock-prone; emit-after-commit-pre-return preserves ordering.
4. **Synchronous emission is a latency optimization, not a correctness gate** — the serve loop
   has a timer/idle-sweep wake fallback (verify the exact fallback before finalizing). The
   watch-coherence conformance subtest asserts latency bounds + forbids going silent on
   Create/Update/Close.
5. **Autoclose re-homing (corrected scope).** `on_close` shells THREE cross-store cascades
   (`gc convoy/wisp/molecule autoclose`), and the dominant molecule trigger is a **work-bead
   close**, not `GraphStore.Close`. Extract the three `*With` functions into a Layer-1/2 package
   taking the store(s) explicitly; make subtree/convoy traversal store-spanning via
   `(storeRef,id)`; **three** conformance tests (convoy, wisp, molecule) each crossing the
   work↔graph boundary. `OnNodeClosed` is net-new, not an existing seam.
6. **Sequence the per-class bd-hook removal to flip ATOMICALLY with the in-process autoclose**
   so exactly one path is live (avoids the double-emit/double-autoclose window).
7. **Lifecycle projection unchanged**; keep the relocated SessionStore OUTSIDE the work
   `CachingStore` (same rule as graph, `api_state.go:186-197`) so projection never sees a stale
   snapshot. The `is_blocked` projection backing the Ready oracle stays a Work-store concern
   through every infra phase.

## 7. Substrate & translation strategy (controller-mediated writes; conformance-only)

**bd-delegating adapter FIRST, then SQLite, per class — wired by constructor injection, no
central router.**

- **Step 1 (interface + bd adapter):** declare the domain-typed interface in the owning package;
  the first impl wraps `beads.Store` and translates at the row edge (persisted subset for
  sessions). Inject via constructor. Pure extract-interface refactor; byte-identical. The racy
  idempotency gate is kept **verbatim** behind a `beadstest.Options{Skip,Reason}` escalation
  bead (the bd phase does not close the cross-controller race).
- **Step 2 (SQLite impl + cutover):** same interface over `SQLiteStore`, real per-entity schema
  (typed columns + indexes), distinct id prefix, per-class file. **Writes route through the
  long-lived controller** (single in-process writer, `MaxOpenConns=1` = serialization); CLI/other
  processes write via a controller API; reads open their own read-only WAL handle. **No
  dual-write, no backfill** for the four `no_history`/ephemeral classes (verified
  `no_history`: sessions/orders/nudges at `bead_policy_store.go:326-327,333-334`; mail ephemeral
  at `beadmail.go:117,457`): clean **drain-then-switch** — stop creating bd beads of that class,
  let in-flight bd beads close naturally, switch new creates to SQLite.
- **Idempotency (SQLite phase):** NOT a bare `UNIQUE`. Sessions = `session_name` uniqueness as a
  **reconciler invariant** (carry the duplicate-then-elect / canonical-election + closed-name
  release logic, `session_beads.go:878`, verbatim into the SQLite path; optionally a partial
  unique scoped to OPEN+canonical rows). Orders = typed Go single-flight query reproducing
  `hasOpenWorkStrict`. Nudges = `nudge_id` is genuinely unique → a real unique index is fine.
- **Cross-store reads:** the `(storeRef,id)` prefix resolver (§2) is a hard prerequisite of the
  FIRST physical move (the order single-flight gate reads graph-resident wisp roots —
  `hasOpenWorkInStoresStrict` already exists at `order_dispatch.go:1750`; mail/nudge resolve
  session FKs — `beadmail.go:67,78,130`, `cmd_mail.go`/`cmd_nudge.go` recipient resolution).
  Dangling-FK behavior: a matching-prefix-but-absent-row returns not-found cleanly; an
  unrecognized prefix never silently mis-routes (both pinned by conformance).
- **Retention:** the controller-owned per-class sweep **does not exist** (graph has zero
  retention; `ga-2gap48` is unbuilt). Pull a single controller-owned sweep into THIS initiative
  for every relocated ephemeral/`no_history` class; disable per-process sweeping. Analyze the
  WAL-checkpoint owner (who runs passive checkpoints under many short-lived readers).
- **Observability:** extend `internal/storehealth` (today `.beads`/dolt only) to enumerate each
  `*.sqlite` for size/disk/GC.
- **Validation & revert (per owner decision 6):** conformance-only — the per-class suite is the
  oracle; the fork-rate soak measures the contention win. **No shadow-read.** Revert =
  **quiesce-the-class-then-flip-back** at a maintenance window, covered by a flip-back integrity
  test (applied first to the already-landed graph class). The accepted risk ("first divergence
  found in production; no mid-flight rollback") is documented at each cutover.

**Conformance discipline (the ONLY safety net — must be rigorous).** Every class has a
`RunClassedStoreTests`-style suite (modeled on the existing `mail/mailtest/conformance.go` +
`coordtest`); the bd impl AND the SQLite impl run the IDENTICAL suite. Mandatory subtests:
**(a) golden round-trip** key-for-key incl. `unknown` passthrough (the loss detector);
**(b) reconstruct-union** equality (§4); **(c) watch-coherence** (no silent mutations);
**(d) emit-after-commit ordering**; **(e) projection-invariance** over the persisted subset;
**(f) error classification** (`UNIQUE constraint failed` → typed `ErrRetryableContention` vs
`ErrHard`); **(g) `readyExcludeTypes`-deletion** (work store never receives the type;
`Work().Ready` unchanged); **(h) cross-store FK resolve + dangling** behavior;
**(i) concurrent-process** id non-collision + no `SQLITE_BUSY` escape.

## 8. Points of no return

1. **bd-row deletion per class** (after drain — once deleted, revert needs copy-back).
2. **Sessions live-adopt copy** (P5) — the metadata long-tail cannot be reconstructed by
   crash-adoption (which only re-attaches the process). Atomic controller-locked copy of OPEN
   sessions + **post-copy projection-equality verification over the LIVE corpus**.
3. **Router deletion** (P6) — removes the federation safety net, retroactively making earlier
   per-class reverts unsafe. Sequence accordingly.

## 9. The 10 hard couplings (status after corrections)

1. **Ready/claim oracle** — `bdReadyPoolDemandShell` (`config.go:3356`) selects on
   `gc.routed_to`, never type; the `rewriteReadyOracle`/`readyOracleCmd` swap (`config.go:3633`)
   turns `bd ready`→`gc ready` under SQLite. Worker-claim + reconciler-count derive from one
   helper so they cannot diverge; a render test + `gc ready == bd ready` differential pin it.
2. **Cross-class dep edges** — the `blocks` edge + `is_blocked` projection ALWAYS stay in the
   work store; `GraphStore` forbidden ready-blocking deps (gated by a split-topology test at P6).
3. **`workflow-finalize` cross-store close** — already `(storeRef,id)`-mediated
   (`runtime.go:48-63`); re-point `ResolveStoreRef` to the prefix resolver under the existing
   lock verbatim; rewire BEFORE Router deletion.
4. **FKs = string `(storeRef,id)`** — never SQLite `FOREIGN KEY` across separate db files;
   resolve path tolerates dangling ids gracefully (conformance).
5. **Wire type = domain DTOs** (§5); `GET /v0/beads` stays `beads.Bead`.
6. **Idempotency** — §7 (reconciler invariant / typed query / real unique index, per class).
7. **Watch/notify coherence** — §6 (Layer-0 raw emit + Layer-2/3 translation; watch-coherence gate).
8. **Transient-error typing** — `ErrRetryableContention` vs `ErrHard` at each backend edge;
   `IsTransientControllerError` checks `errors.Is` first.
9. **Storage-class** — caller-supplied param; computed once at the root, graph children inherit
   in-memory (orthogonal to ownership).
10. **`readyExcludeTypes` deletions** — per class, after it leaves the work store; the deletion
    IS the proof. **Nudge oracle-leak (verified latent):** `type=chore` NOT in `readyExcludeTypes`
    and `gc:nudge` NOT in `IsReadyExcludedBead` (`beads.go:177-248`); nudges stay non-claimable
    only via never carrying `gc.routed_to` + the ephemeral-scan boundary. **Fix BEFORE relocating
    nudges**: add the exclusion AND a born-unrouted conformance proof. (Extend the same to
    `type=task`+`gc:extmsg-*` when extmsg is scoped, §11.)

## 10. Phase plan (corrected; with go/no-go)

| Phase | Goal | Key corrections folded in | Gate |
|---|---|---|---|
| **P0** Landed baseline | graph on SQLite; coordclass/coordrouter live under `graph_store=sqlite`; conformance skeletons | — | **DONE** |
| **P1** Domain interfaces + bd adapters (no behavior change) | SessionStore/MailStore/OrderStore/NudgeStore (+ WAIT, ConvoyStore) in owning packages; bd-delegating impls; constructor injection | session row edge = persisted-subset codec (infoFromBead stays impure in Manager); reconstruct-union contract specified FIRST; **SessionStore threaded at `internal/api/session_manager.go`**, OrderStore consumption site decided; NO `UNIQUE` drop-ins; racy gate kept behind Skip+Reason | **GO** w/ corrections |
| **P2** Greenfield event emission | Layer-0 `WithSQLiteStoreRecorder` raw `RowChanged`; Layer-2/3 typed payloads; autoclose 3-cascade store-aware extraction | emit OUTSIDE retry closure/after Unlock; sync-emit = latency opt over timer floor; three cross-store autoclose tests; bd-hook removal flips atomically with in-process autoclose | **NO-GO** until layering + autoclose corrections land |
| **P3** Full typed wire + dashboard regen (every class) | re-root `sessionResponse` on Info; new typed endpoints for orders-tracking/nudges/convoys; SSE payloads | convoy = aggregated members array (or scoped `convoys.ts` rewrite + vitest); complete CI gate set (openapi×3 + `npm run gen` commit + vitest + dashboard-check); move ConvoyStore ownership-split OUT of P3 into its own gated phase | **GO** w/ corrections |
| **P3.5** Cross-store foundation | the `(storeRef,id)` prefix resolver; the controller-mediated infra write API; the controller-owned retention sweep; storehealth `*.sqlite`; **inventory + re-point ALL cmd/gc + internal/api direct infra-bead READERS and WRITERS** (status/doctor/demand-scan/`session_wake.go`/`session_beads.go`) to typed stores/federation accessors | this is its own phase — larger than the Layer-2 injection; grep-guard against new generic-store infra reads/writes in cmd/gc non-test files | **NEW — prerequisite of any physical move** |
| **P4** SQLite cutover: mail / orders / nudges | SQLite impls; controller-mediated writes; distinct prefixes; clean drain-then-switch; typed single-flight; readyExcludeTypes deletions | mail needs the session read-path (resolves to bd sessions during P4); orders needs the graph read-path in `storesForGate` (hard prereq + gate conformance); **fix nudge oracle-leak before flipping nudges**; idempotency re-spec'd (no run_key / no bare session UNIQUE); revert = quiesce-then-flip-back | **NO-GO** until P3.5 + the three prereqs land |
| **P5** SQLite cutover: sessions | SQLite SessionStore (typed lifecycle columns); controller-coordinated atomic live-adopt of OPEN sessions + post-copy projection-equality over LIVE corpus | enumerate & re-point ALL session WRITERS (Manager + `session_beads.go` + `session_wake.go`) and READERS; worker-boundary green; session_name uniqueness as reconciler invariant; point-of-no-return | **NO-GO** until session writer/reader inventory + live-adopt fidelity specified |
| **P6** Graph finalize + Router retirement | promote `GraphStore` read/finalize (`GetNode`/`ListNodesByRoot`/`ListNodeEdges`/`CloseSubtree`/`ReadyCandidates`/`FindOrCreateByKey`); **MOVE `ClassifyGraphPlan` into `internal/graphstore`**; provider-aware `ResolveStoreRef`; rewire the `?type=molecule` augment + order gate to GraphStore BEFORE deleting federation; delete Router via the three-mechanism zero-callers gate | split-topology `is_blocked` test; final point-of-no-return | **GO** w/ corrections (strongest part) |
| **P7** (follow-on) extmsg | own `ExtMsgStore` with per-family record types + **history backfill** (NOT clean-drain) + per-family conformance; add `type=task`+`gc:extmsg-*` Ready exclusion | scoped after mail proves the pattern (owner decision 4) | deferred |

## 11. extmsg follow-on (deferred, decision 4)

extmsg is ~10k LOC, 8 `gc:extmsg-*` families, ~12 record types, all `type=task` on the
**history** tier. It cannot live in a `mail.Message`-shaped store and is NOT clean-drain (it
needs a real history backfill). Treated as its own epic AFTER mail lands: model `ExtMsgStore`
with per-family record types + a storage-row edge + history backfill + per-family conformance,
and extend the Ready-leak fix (§9.10) to `type=task`+`gc:extmsg-*`. Until then extmsg stays on
bd; the "messaging" first cut is mail only.

## 12. Residual open decisions (carried, with defaults)

- **SQLite idempotency mechanisms** finalized per class in P4/P5 (sessions: reconciler invariant
  / partial index; orders: typed query; nudges: real unique index).
- **WAL-checkpoint owner** under many short-lived readers — analyze in P3.5; default: the
  controller (sole writer) runs passive checkpoints.
- **Per-class `[beads.classes.*]` config merge** — confirm `compose.go` deep-merge vs
  city-layer-only; default: deep-merge arm for `[beads]` (so packs can layer per-class backends).
- **Upstream baseline** — name the live integration remote/branch and re-measure divergence
  before pricing any rebase-surface tradeoff; the constructor-injection / fork-owned-package
  discipline holds regardless.

## 13. Accepted risks (explicit)

- **No in-prod correctness oracle** (decision 6): first divergence is discovered in production;
  revert is quiesce-then-flip-back at a maintenance window. Mitigated only by conformance depth
  + the fork-rate soak. The golden round-trip / reconstruct-union / projection-invariance
  subtests are therefore non-negotiable and must run against a corpus dumped from a LIVE city.
- **Controller as single writer** per relocated class (decision 5): a controller outage stalls
  infra writes for relocated classes (reads continue on WAL). Acceptable because the controller
  is already the orchestration spine; CLI write commands become controller API calls.
- **Citation drift**: all `file:line` are pinned to `204b66aee`; re-verify at execution time.

## 14. P1 implementation notes (milestone-review reconciliations)

Settled during the P1 foundation + mail-pilot milestone review; the other four
domain extractions follow these as the canonical template:

- **P1 seam currency is `beads.Bead` by design.** Each domain seam (e.g.
  `beadmail.MailStore`) is declared in the adapter package as a faithful subset of
  `beads.Store`, proven by `var _ Seam = beads.Store(nil)` so the bd-delegating
  first impl needs no wrapper. The richer **domain-typed** interface and the
  lossless row codec fold in at the **SQLite cutover (P4/P5)**, not P1 — the bd
  phase is a pure, byte-identical extract-interface refactor. (The literal PLAN
  P1-T9/DESIGN §4 phrasing "domain-typed `mail.MailStore` in `internal/mail`" is
  superseded by this: shape is bead-subset-in-adapter for P1.)
- **`mail.Message` is the domain type AND the direct wire type** (no DTO; the API
  serves `[]mail.Message`). It omits bead routing metadata, which lives only at the
  persistence edge — so the seam currency is `beads.Bead` while the service/wire
  above speaks `mail.Message`.
- **The two-seam split is the template.** A domain that reads another class's beads
  (mail reads sessions; orders read graph wisp roots) splits its store dependency
  into its own owned-class seam plus a cross-class `beads.Store` read seam, with an
  injectable constructor (`NewWithStores`) and a divergent-store routing test.
  `New`/`NewCached` wire both to the same store for the byte-identical bd phase.
- **Deferred behavior changes are owned by P4, not P1.** Populating `Message.Rig`
  and collapsing the read label/metadata double-encode are *behavior changes* and
  were deliberately NOT done under the P1 byte-identical bar (pinned by
  characterization tests). They belong to the mail SQLite cutover (P4).
- **The codec-fidelity conformance suite lives in `internal/beads/codectest`**
  (a Layer-0 package), NOT in `internal/coordrouter/coordtest` — so it survives the
  Router deletion (P6). It asserts FULL-bead invariance (metadata key-for-key plus
  every non-metadata field a projection reads) and requires a projection; metadata-
  only validation was insufficient for the session domain.
