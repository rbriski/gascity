---
title: "Infra/beads decoupling — session handoff"
date: 2026-06-26
branch: plan/decouple-infra-beads
head: 1bf1cd3a5
epic: ga-pd6tcg
---

> **UPDATE 2026-06-26 (later session):** **NUDGES is now fully relocated**
> (`d722e45b9` seam + `1d45620f8` untangle) and **GRAPH P6 Step-0 (the
> config-conflict guard) is landed** (`1bf1cd3a5`). Both green at the default
> backend (byte-identical), verified by the Nudge/Wait/Sweep/Sling/Dispatch
> suites + `make test-cmd-gc-process-parallel`. §5.A (nudges) and §5.B-B0 below
> are DONE — see the per-item ✅ notes. Remaining in owner order: **graph P6
> Steps 1-5** (§5.B) then **sessions P5** (§5.C).

> Where the bd→embedded-store infra decoupling stands, what's left, and how to
> finish it without breaking anything. Read alongside `DESIGN.md` (hardened
> architecture + §10 phase plan), `PLAN.md` (task list), and `ORDERS-CUTOVER.md`
> (the worked example of a clean class flip). `raw/` holds the audit trail.

## 1. Orientation

The initiative moves the **infra bead classes** (sessions, mail, orders, nudges,
graph) off the bd/Dolt work store onto **per-class backends** selected by
`[beads.classes.<class>].backend`, while bd/Dolt continues to hold front-door
work. Each class can be `bd | sqlite | postgres`. The bar is **ZERO behavior
change at the DEFAULT backend (`bd`) — byte-identical, proven by conformance**,
with every step revertible until an explicit point-of-no-return.

The single per-class dispatch point is **`resolveClassStore`**
(`cmd/gc/class_store.go:158`). At `bd` it early-returns the work store unchanged
(`class_store.go:163-164`) → identical bytes. SQLite/Postgres open a per-class
store under `<cityPath>/.gc/<class>/` (distinct id prefix `gcm/gcs/gco/gcn/gcg`),
and controller-path writes emit `bead.*` via `beadEventRowRecorder`
(`class_store.go:30`, actor `cache-reconcile`). Shared cached handles are
neutralized for close via `noCloseSQLiteStore` (`class_store.go:120`) /
`noClosePostgresStore` (`postgres_class_store.go:35,83`) — never hand a raw
shared `*SQLiteStore`/`*PostgresStore` to a close-after-use caller.

## 2. Done

- **Mail (messaging) and orders are FLIP-READY** through clean store seams
  (`beadmail.MailStore`, `orders.OrderStore`) routed through `resolveClassStore`
  via `resolveMailMessagesStore` (`class_store.go:182`) and `resolveOrderStore`
  (`class_store.go:191`). Orders flip + gate cross-read landed (`817720e2a`,
  `56c9febeb`); the nudge oracle-leak fix landed (`e9f2ac00d`).
- **Postgres backend built + conformance-passing**
  (`internal/beads/postgres_store.go`): schema-per-class, provisioned via
  `gc beads postgres init <class>`, registry openers in
  `cmd/gc/postgres_class_store.go`. Conformance ported from SQLite
  (`internal/beads/postgres_store_conformance_test.go:25`).
- **This session's two commits:**

| Commit | What | Why |
|---|---|---|
| `78f77a966` | Generalized `gc beads migrate` to dispatch the dest per configured backend (sqlite **or** postgres). | One migrate command now follows `NormalizedClassBackend`; `migrate-sqlite` kept as alias only (`cmd_beads_migrate.go:39-41`). |
| `b47f5daa3` (HEAD) | Postgres class-handle cache returns `noClosePostgresStore` instead of the raw `*PostgresStore`. | A close-after-use caller was closing the SHARED pool; now neutralized, mirroring `noCloseSQLiteStore`. Guarded by `TestOpenClassPostgresStore_CloseSafe`. |

> NOTE: `HANDOFF.md`/`NEXT-SESSION.md` task ledgers from before this session are
> one epoch stale (they say "15 commits", `bd|sqlite`, `migrate-sqlite`). The
> reality is **33 commits since the initiative baseline `204b66aee`** (the full
> `main..HEAD` is 739 — the whole fork-integration branch), `bd|sqlite|postgres`,
> `gc beads migrate`.

## 3. The owner decision

Do **all three** remaining classes "full": **nudges, graph, sessions** — informed
that **sessions is irreversible** and **graph is P6** (the highest-blast-radius
milestone). Execute **phased + verified**. The sessions live-adopt code is the one
piece that is **BUILT + conformance-tested but NOT executed here** — there is no
live city in this environment, and the flip has no cheap undo.

## 4. The hard lesson (carry forward — do NOT repeat)

The infra-class CONSUMER code **conflates one shared `store` (the work store)
across multiple classes**. `openNudgeBeadStore` (`cmd/gc/nudge_beads.go:43-49`)
returns `openCityStoreAt(cityPath)` — a **GENERAL store** that also serves
**session reads** (`resolveNudgeTarget`, `cmd_nudge.go:939`; the prime fence,
`cmd_prime.go:291`) and mail-notify. Making that chokepoint backend-aware
**misroutes session/mail ops** — it was tried and **REVERTED**.

Consequence: relocating nudges/sessions/graph is an **untangle**, not a seam
insertion. Each mixed function must be split so it takes BOTH a `workStore`
(session/wait/mail ops) and the relocated class store (leaf shadow ops); at the
default backend both args are the same value → byte-identical. Two traps that
look like nudge ops but are NOT:

- `stampLastNudgeDeliveredAt` writes a **session bead** but is currently routed on
  the nudge `deliveryStore` (`cmd_nudge.go:1131`, `cmd_sling.go:1563`) → must move
  to `workStore`.
- `blockedQueuedNudgeReason`'s `store.Get(item.Reference.ID)` reads a **wait bead**
  (session-class: `waitBeadType`, created on the work store at `cmd_wait.go:258`)
  → must stay on `workStore` (`cmd_nudge.go:1272-1283`).

---

## 5. Remaining work, in order

### A. NUDGES — all-or-nothing untangle ✅ DONE (`d722e45b9` + `1d45620f8`)

> **✅ LANDED 2026-06-26, byte-identical at default.** The full untangle is in:
> `resolveNudgesStore`/`openNudgesClassStore` added; every leaf nudge-bead op
> routes through the nudge store; every session/wait/mail op stays on the work
> store; the `(workStore, nudgeStore)` pair is threaded through tryDeliver /
> dispatchAll / deliverSessionNudgeWithWorker / queueManagedSessionNudgeWake /
> enqueueManagedNudgeThenWake / sendMailNotifyWithWorker / deliverSlingNudge /
> dispatchReadyWaitNudgesWithSnapshot, and the sweep is split `(nudgeStore,
> mailStore)`. **Plan-list gaps found by grepping ALL nudge-bead ops (not just
> the listed fns):** also split `finalizeReadyWaitFromNudge` +
> `prepareWaitWakeStateForCityWithSnapshot` and `nextWaitDeliveryAttempt` +
> its CLI callers (`cmdWaitSetStateResult`/`retryClosedWait`). Controller paths
> pass `cr.rec`; CLI/secondary loads pass `io.Discard` to `loadCityConfig` (arch
> guard `TestNonTestLoadCityConfigCallersPassWarningWriter`). The checklist below
> is the historical record of what was executed.



The seam already exists: leaf ops take `nudgequeue.NudgeStore`
(`internal/nudgequeue/store.go:14-25`, `var _ NudgeStore = beads.Store(nil)`), and
the P1 seam landed byte-identically (`303a80158`). What's missing is the consumer
dispatch + the mixed-function splits. Dispatch is enumerated from the flock queue
(`nudgequeue.LoadState`); the bead is a pure shadow, so relocating it cannot change
dispatch semantics.

**The seam:** add `resolveNudgesStore(workStore, cfg, cityPath, rec)` to
`class_store.go` (after `:193`), mirroring `resolveOrderStore`, returning
`beads.Store` (satisfies `NudgeStore` for free). Add an `openNudgesClassStore(cityPath)`
helper next to `openNudgeBeadStore` (`nudge_beads.go:43`). Controller paths pass
`cr.rec`; CLI paths pass `rec=nil` (nudge shadows are transient + gate-excluded).

**Checklist (file:line):**

1. Add `resolveNudgesStore` (`class_store.go` after `:193`) + `openNudgesClassStore` (`nudge_beads.go:43`).
2. Repoint **pure-nudge** self-openers to `openNudgesClassStore`: `cmd_nudge.go:1467, 1503, 1539, 1628, 1728, 1777, 1832`; `cmd_wait.go:1443`. **Leave `cmd_nudge.go:939` and `cmd_prime.go:291` on `openNudgeBeadStore`** (session reads).
3. Split `sweepStaleNudgeMail`/`countStaleNudgeMail` → `(nudgeStore, mailStore, …)` (`nudge_mail_sweep.go:46/143`): Phase 1 (`gc:nudge`, lines 59-96) → `nudgeStore`; Phase 2 (`type=message label=read`, 106-133) → `mailStore`. Update `city_runtime.go:1486`, `cmd_order.go:1755/1773`, and **15** callers in `nudge_mail_sweep_test.go`.
4. Split `tryDeliverQueuedNudgesByPoller` → `(workStore, nudgeStore)` (`cmd_nudge.go:1052`). Route on `workStore`: `workerHandleForNudgeTarget` (1099), the wait-read in `splitQueuedNudgesForDelivery`/`blockedQueuedNudgeReason` (1252/1272), and `stampLastNudgeDeliveredAt` (1131). Update `nudge_dispatcher.go:189` + 6 test callers (`cmd_nudge_test.go:2363/2475/2520/2570/2649/2726`).
5. Thread two stores through `dispatchAllQueuedNudges` (`nudge_dispatcher.go:115/179/189`) and `nudgeDispatchTick` (`city_runtime.go:2674`, compute `resolveNudgesStore(…,cr.rec)`).
6. Split `dispatchReadyWaitNudgesWithSnapshot` (`cmd_wait.go:1132`, shim `:1128`); `workStore` for `loadWaitBeadsForOpenSessions` (1140), `stampWaitLookupCapDiagnostic` (1166/1233), `SetMetadata(wait.ID,…)` (1188), `finalizeReadyWaitFromNudge` (1222); `nudgeStore` for `findQueuedNudgeBead` (1163) + `enqueueQueuedNudgeWithStore` (1185). Update `city_runtime.go:2312`.
7. Split `deliverSlingNudge` (`cmd_sling.go:1547`): session ops (1549/1553/1563) → `workStore`; `enqueueQueuedNudgeWithStore` (1570) → `nudgeStore`. Callers `cmd_sling.go:1460/1487`.
8. Split `enqueueManagedNudgeThenWake` (`cmd_nudge.go:695`): enqueue/rollback → `nudgeStore`; `requestManagedNudgeWake` (`store.Get`+`WakeSession`) → `workStore`. Propagate through `deliverSessionNudgeWithWorker` (606), `sendMailNotifyWithWorker` (886), `queueManagedSessionNudgeWake` (683).
9. `runNudgeMailSweepWatchdog` (`city_runtime.go:1463`): pass `resolveNudgesStore(…,cr.rec)` + `resolveMailMessagesStore(…,cr.rec)`.
10. Verify (§6) + `git commit --no-verify`.

> **Ops constraint for the non-default backend:** SQLite is process-local — nudge
> sidecars (`gc nudge poll`/`drain`) writing shadows from separate processes would
> contend (`SQLITE_BUSY`). Nudges-on-SQLite is only safe in supervisor dispatcher
> mode (`cfg.Daemon.NudgeDispatcherMode()=="supervisor"`); otherwise use `bd` or
> Postgres (native concurrent writers). No code gate needed for the default.

### B. GRAPH — P6, config-conflict guard FIRST (highest blast radius)

Graph routes through the legacy `[beads] graph_store="sqlite"` knob and
`coordrouter.Router` (`api_state.go:240-247`), **never** `resolveClassStore`. The
Router only ever registers a SQLite backend (`registerGraphStoreBackend`,
`api_state.go:318-346` — no Postgres branch). But `NormalizedClassBackend("graph")`
honors `[beads.classes.graph].backend` first (`config.go:1362-1378`). So
`graph=postgres` makes `NormalizedClassBackend` say postgres while the Router serves
SQLite/Dolt → **silent wrong-store**, reachable via `gc beads postgres init graph`
(`cmd_beads_postgres.go:63`). Seven consumers gate on the legacy knob alone:
`api_state.go:241`, `api_state.go:319`, `cmd_convoy_dispatch.go:458`,
`cmd_hook_claim.go:276`, `work_query_probe.go:89`, `cmd_bd_shim.go:1049`,
`config.go:3731`.

**B0 — config-conflict guard ✅ DONE (`1bf1cd3a5`):** `internal/config/validate_beads_classes.go` `ValidateBeadsClasses` rejects `graph` backend other than `bd|sqlite|""`, wired into BOTH `Load` (config.go, after `ValidateDoltConfig`) AND compose root-validation (`compose.go:656`). Shared `normalizeBackend` extracted from `NormalizedClassBackend`. Tests in `validate_beads_classes_test.go` (graph=postgres rejected; graph∈{"",bd,sqlite} + orders/nudges=postgres allowed; normalizeBackend canonicalization). No behavior change at any existing config. Relaxes to allow `graph=postgres` after P6 Step 5. Original spec retained below for reference:
- New `internal/config/validate_beads_classes.go`: `ValidateBeadsClasses(cfg, source)` rejects `graph` backend other than `bd|sqlite|""` until graph routes through `resolveClassStore`. Wire into `Load` (`config.go:5061`) after `ValidateDoltConfig` (`config.go:5082`). Extract a shared `normalizeBackend(string)` helper from `NormalizedClassBackend` (`config.go:1365`) so guard + dispatcher share one canonicalizer.
- Tests (`validate_beads_classes_test.go`): graph=postgres rejected; graph ∈ {"",bd,sqlite} allowed; orders=postgres allowed (it routes through `resolveClassStore`).
- Do **NOT** instead "make `graphStoreSQLiteEnabled` honor `Classes[graph]=sqlite`" — that would start building a Router on cities that had none (new store file, prefix `gcg`, federation overhead) = real behavior change. That belongs inside P6 under conformance.
- Verify: `go test ./internal/config/` + `make test-cmd-gc-process-parallel`.

**P6 ordered execution (strictly in order; revertible until Step 5):**
1. **Promote `GraphStore` read/finalize** — add `GetNode`/`ListNodesByRoot`/`ListNodeEdges`/`CloseSubtree`/`ReadyCandidates`/`FindOrCreateByKey` to the interface (`internal/coordrouter/stores.go:65`), implement on `BdGraphStore` (`bdgraphstore.go:25`) and `*beads.SQLiteStore` (`sqlite_store_graph_apply.go`). Fill the skipped conformance skeleton (`coordtest/conformance.go:172-181`, skip reason `:46`). Additive only.
2. **MOVE `ClassifyGraphPlan`** (+ `classifyFields`/`isWispMetadata`) from `coordclass/classify.go:88` into a **new** `internal/graphstore` package (does not exist yet). Update Router call sites (`router.go:151,167`) and `TestClassifyGraphPlan` (`classify_test.go:77`). Grep-sweep `ClassifyGraphPlan` + `coordclass.ClassGraph`.
3. **Provider-aware `ResolveStoreRef`** — re-point the `(storeRef,id)` resolver (`internal/dispatch/runtime.go:48-63`, template `api_state.go:630`) onto the existing `internal/storeref` package; extend the prefix switch (`gcm-/gco-/gcn-/gcs-` alongside `gcg-`→graph, `gc-/ga-`→work). Keep `cmd_convoy_dispatch.go:205` + `dispatch/runtime.go:796-802` green.
4. **Rewire both graph read paths to `GraphStore` BEFORE deleting federation** (load-bearing — done under the federation net): the `?type=molecule` augment + `ReadyGraphOnly` hot loop (`huma_handlers_beads.go:113-181, 341-342`) and the order gate `storesForGate` assembly (`order_dispatch.go:512-532`). Gate: split-topology `is_blocked` conformance (DESIGN §9.2, `:284`).
5. **Delete `coordrouter`** (final point-of-no-return, DESIGN §8 `:274-277`): confirm zero non-test callers of all three mechanisms — create-time classify (`router.go:113`), by-id probing (`router_mutation.go:21`), read federation (`router_federation.go:242`) — and the sole `coordrouter.New` site (`api_state.go:240`). Delete `internal/coordrouter` + `internal/coordclass`, retire `graphStoreSQLiteEnabled`, fold graph into `resolveClassStore` so the seven consumers + `NormalizedClassBackend` read one source. Then relax the B0 guard to allow `graph=postgres`.

> Never collapse Steps 4 and 5. Step 4 is the behavioral pivot under the safety
> net; Step 5 removes the net and is irreversible.

### C. SESSIONS — P5 (offline-buildable vs irreversible live-adopt)

No `SessionStore` seam exists yet (the grep hits are test fixtures) — it is
**net-new**, mirroring `orders/store.go:22-32` and `beadmail/beadmail.go:41-51`.
`infoFromBead` (`manager.go:1543`) is **impure** (reads `m.sp`, mutates the ACP
router) — only the pure persisted subset (`manager.go:1564-1587`) goes to the
codec; runtime enrichment (`:1550-1562, 1590-1595`) stays in Manager.
`ProjectLifecycle` (`lifecycle_projection.go:374`) is **pure** — the
projection-invariance target. `session_name` uniqueness is a **reconciler
invariant** (`session_beads.go:884-919, 443-499`); both schemas key only on
`id TEXT PRIMARY KEY` (`sqlite_store.go:191`, `postgres_store.go:199`) with
`session_name` as a **non-unique** metadata row — duplicate-then-elect REQUIRES
the transient duplicate, so **NO bare `UNIQUE(session_name)`**.

**Part (a) — offline-buildable + verifiable WITHOUT a live city:**
1. **Extract `SessionStore` seam** in `internal/session/` (`Create/Get/List/SetMetadata/SetMetadataBatch/Update/Close` + the wait reads `ListSessionWaitBeads`/`ReassignWaits` use); prove `var _ SessionStore = beads.Store(nil)`. Factor the pure subset of `infoFromBead` into `infoFromPersisted(b)`; assert byte-identical output pre/post for closed + active beads. Risk LOW.
2. **Inject at the single chokepoint** `internal/api/session_manager.go:11` — wrap `store` with `resolveSessionStore(store, cfg, cityPath, rec)` → `resolveClassStore(…, config.BeadClassSessions, rec)`. Default `bd` returns the same instance (all 17 call sites unchanged). NOT `api_state.go`. Risk LOW.
3. **SQLite+Postgres session schema** — reuse the generic EAV schema (no new DDL, explicitly NO unique session_name); register the `gcs` prefix (`reserved_prefixes.go:19`). Add a guard test that no schema introduces a unique index on `session_name`. Run `syncSessionBeads` canonical-election (`session_beads.go:884-919`) against a SQLite store; assert duplicates coexist then elect identically to bd. Risk LOW–MED.
4. **Conformance incl. projection-invariance (NEW)** — extend the classed-store conformance for `BeadClassSessions`; add a suite that seeds an identical corpus into bd/SQLite/PG and asserts `ProjectLifecycle(input)` + `infoFromPersisted(b)` are equal across all three (fixed `input.Now` to kill the `time.Now` default at `:381`). This is the load-bearing zero-behavior-change proof. Risk MED.

**Part (b) — live-adopt atomic-copy COMMAND: BUILD + conformance-test, do NOT run:**
- `storemigrate.Migrate` (`migrate.go:34`) is an **offline bulk copier** (no lock/quiesce/projection-equality) and is **NOT sufficient** for open sessions. Build a **net-new** `gc beads adopt sessions` that, against the live controller: (1) acquires a controller lock / quiesces the session reconciler, (2) snapshots the OPEN session+wait corpus, (3) ID-preserving copy into the configured store, (4) post-copy projection-equality gate (`ProjectLifecycle` old==new for every open bead) — **ABORT on mismatch**, (5) flip the read path only on a clean gate, (6) leave bd rows intact. Reuse `Migrate`'s ID-preserving core for closed/historical beads.
- Verify offline against fake/in-memory stores incl. the abort-on-mismatch branch + the controller-lock contract. Config-gate so it cannot run unless `[beads.classes.sessions].backend` is sqlite/postgres.
- **HIGH / IRREVERSIBLE.** Open-session metadata is persisted-only; crash-adoption cannot rebuild it. Once the read path flips, bd session rows are abandoned with no automatic reverse. **Build it green offline, hand it to the user, and STOP** — no live city here.

---

## 6. Verification harness

**Disposable Postgres** (the `billing-pg-gb` container on :55455 is SOMEONE
ELSE'S — do not touch it):

```bash
docker run -d --rm --name gc-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gascity_internal -p 55460:5432 postgres:16
export GC_TEST_POSTGRES_DSN="postgres://postgres:test@127.0.0.1:55460/gascity_internal?sslmode=disable"
```

**`GC_TEST_POSTGRES_DSN`-gated suites (skip when unset):**
- `cmd/gc/postgres_class_store_test.go` → `TestBeadsMigrate_ToPostgres` (`:48`, already exercises `BeadClassNudges`), `TestOpenClassPostgresStore_CloseSafe` (`:109`, the `b47f5daa3` guard), `TestBuildPostgresDSN_RequiresDatabase` (`:147`), `TestOpenClassPostgresStore_RoundTrip` (`:157`)
- `internal/beads/postgres_store_conformance_test.go` → `TestPostgresStoreSatisfiesClassedStoreConformance` (`:25`)
- `internal/beads/postgres_store_integration_test.go` → schema-isolation, provisioned-schema, pinned-id, full-surface (`:35/:66/:91/:118`)

**Sharded test targets (Makefile — prefer over monolithic `go test ./...`):**
`test-fast-parallel` (`:308`), **`test-cmd-gc-process-parallel`** (`:349`, ~60-70s,
the order/nudge/mail/migrate wiring guard), `test-integration-shards-parallel`
(`:410`), `test-local-full-parallel` (`:414`).

**Standard non-PG verify:**
```bash
go build ./... && go vet ./internal/... ./cmd/gc/
go test ./internal/beads/... ./internal/coordrouter/... ./internal/mail/... \
        ./internal/nudgequeue/ ./internal/orders/ ./internal/storemigrate/ ./internal/config/
go test ./cmd/gc/   # guards order dispatch / nudge / mail / migrate wiring
```

The default-backend byte-identical assertion (`class_store_test.go:198`) already
asserts unregistered/`bd` nudges return the work store — keep it green.

## 7. Gotchas + constraints

- **Commit with `git commit --no-verify`** — `core.hooksPath` points at the MAIN
  checkout's stale pre-commit hook (old `git add docs/schema/openapi.json` path);
  it ABORTS `.go` commits from this worktree. Run the gate manually instead.
- **gascity Dolt is LOCAL-ONLY** — never `bd dolt push/pull/remote` (re-introduces
  a doomed `origin`). `git push` only.
- **Never `tmux kill-server`** (destroys everyone's shells).
- **Never `go clean -cache`** (corrupts the shared fleet cache). `go clean -testcache`
  is allowed. For cold builds: `GOCACHE=$(mktemp -d) go build ./cmd/gc/`.
- **`billing-pg-gb` on :55455 is off-limits** — always use your own `gc-pg` on :55460.
- **MemStore does NOT preserve caller-pinned IDs; SQLiteStore AND PostgresStore DO**
  (`autoID := b.ID==""`; PG even bumps its native sequence,
  `TestPostgresStorePinnedIDBumpsSequence`). Tests asserting specific IDs must use
  SQLite/Postgres or assert via List/count.
- The order single-flight gate uses `beads.HandlesFor(store)` → it must stay
  `beads.Store`; do NOT narrow it to a class seam.
- `genschema`/`go run ./cmd/genspec` regenerates `docs/reference/schema/*` on config
  changes — commit them with the change.

## 8. Pointers

- `engdocs/plans/infra-store-decouple/DESIGN.md` — hardened architecture; §10 phase
  plan (`:304-316`); §8 point-of-no-return (`:268-277`); §9.2 split-topology gate (`:284`).
- `engdocs/plans/infra-store-decouple/PLAN.md` — task list.
- `engdocs/plans/infra-store-decouple/ORDERS-CUTOVER.md` — worked example of a clean
  class flip (the pattern nudges/sessions follow).
- Auto-memory `infra-beads-decoupling-plan.md` — binding decisions, branch context.
