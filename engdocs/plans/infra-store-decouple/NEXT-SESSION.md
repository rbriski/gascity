# Infra-Store Decouple — Next Session

Branch `plan/decouple-infra-beads` @ `1bf1cd3a5` · worktree `/data/projects/gascity/.claude/worktrees/infra-store-plan`

> **STATUS 2026-06-26 (later session): §4 NUDGES ✅ DONE, §5 graph Step-0 guard ✅ DONE,
> §5 graph P6 Step 1 ✅ DONE (re-scoped → PostgresStore graph-apply parity, `4d77288b9`).**
> Nudges fully relocated (`d722e45b9` seam + `1d45620f8` untangle) and the graph
> config-conflict guard landed (`1bf1cd3a5`) — both byte-identical at the default
> backend, verified by the Nudge/Wait/Sweep/Sling/Dispatch suites + the sharded
> `make test-cmd-gc-process-parallel` guard. Graph P6 Step 1 landed as PostgresStore
> graph-apply parity (see the re-scope note under §5 Step 1) — the GraphStore interface
> is unchanged. **Pick up at §5 graph P6 Step 2** (MOVE `ClassifyGraphPlan` into a new
> `internal/graphstore`), then the ordered P6 steps, then §6 sessions P5. NOTE: §4's mixed-fn list was incomplete —
> the executed untangle also split `finalizeReadyWaitFromNudge` /
> `prepareWaitWakeStateForCityWithSnapshot` / `nextWaitDeliveryAttempt`
> (+`cmdWaitSetStateResult`/`retryClosedWait`); always grep EVERY nudge-bead op,
> don't trust a fixed list. New secondary `loadCityConfig` loads need `io.Discard`
> (arch guard `TestNonTestLoadCityConfigCallersPassWarningWriter`).

> **Mission.** Move infra bead classes (sessions, mail, orders, nudges, graph) off the bd/Dolt work store onto per-class backends (`bd|sqlite|postgres`) selected by `[beads.classes.<class>].backend`, keeping bd/Dolt for front-door work. **The bar is ZERO behavior change at the DEFAULT backend (byte-identical), proven by conformance.**

---

## 0. Read-me-first constraints (carry into every commit)

- **Commit with `git commit --no-verify`.** `core.hooksPath` points at the MAIN checkout's stale pre-commit hook (old `git add docs/schema/openapi.json` path); it ABORTS any `.go` commit from this worktree. Run the quality gate manually instead.
- **gascity Dolt is LOCAL-ONLY** — never `bd dolt push/pull/remote add` (re-introduces a doomed `origin`, ga-9wsri). Use `git push` only.
- **Never `tmux kill-server`** (destroys every shell on the host). If tmux cleanup is needed, target the explicit city/test socket with `tmux -L <socket>`.
- **Never `go clean -cache`** (corrupts the shared fleet GOCACHE). `go clean -testcache` is allowed. Cold builds: `GOCACHE=$(mktemp -d) go build ./cmd/gc/`.
- **Disposable Postgres for verification** (the `billing-pg-gb` container on **:55455 is SOMEONE ELSE'S — DO NOT TOUCH**):
  ```bash
  docker run -d --rm --name gc-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gascity_internal -p 55460:5432 postgres:16
  export GC_TEST_POSTGRES_DSN="postgres://postgres:test@127.0.0.1:55460/gascity_internal?sslmode=disable"
  ```
- **Sharded tests** (not a monolithic `go test ./...`): `make test-cmd-gc-process-parallel` (~60-70s cmd/gc guard for order/nudge/mail/migrate wiring), `make test-fast-parallel`, `make test-integration-shards-parallel`, `make test-local-full-parallel`.

---

## 1. THE HARD LESSON — shared-store conflation (do NOT repeat the revert)

The infra-class CONSUMER code conflates **one shared `store`** (the work store) across multiple bead classes. The single per-class dispatch point is **`resolveClassStore`** (`cmd/gc/class_store.go:158`). Default backend (`bd`) early-returns the work store unchanged → byte-identical (`class_store.go:163-164`).

**The trap that was already tried and REVERTED:** making a general-store *chokepoint* backend-aware misroutes other classes. `openNudgeBeadStore` (`cmd/gc/nudge_beads.go:43-49`) returns the **work store**, and its callers use it for **session reads** (`resolveNudgeTarget` `cmd_nudge.go:939`; the prime fence `cmd_prime.go:291`), worker observe/handle, and mail-notify. Making that opener backend-aware is WRONG.

**The rule:** thread the resolved class store **ONLY into the leaf class-bead operations.** Leave every session/wait/mail op on the work store. Concretely, split each mixed function so it takes **both** a `workStore` (session/wait/mail) and a `<class>Store` (the shadow). At the default backend, callers pass the same value twice → identical bytes.

Shared cached handles are neutralized for close via `noCloseSQLiteStore` (`class_store.go:120`) / `noClosePostgresStore` (`postgres_class_store.go:35,83`) — **never hand a raw shared `*SQLiteStore`/`*PostgresStore` to a close-after-use caller** (regression guard: `TestOpenClassPostgresStore_CloseSafe`, `postgres_class_store_test.go:109`).

---

## 2. What is ALREADY LANDED (do not re-litigate)

The initiative is **33 commits** since its baseline `204b66aee` (the full `main..HEAD` is 739 — the whole fork-integration branch). The architecture, the 6 binding decisions, and the per-class dispatch seam are settled.

- **mail** (`beadmail.MailStore`) and **orders** (`orders.OrderStore`) are routed through `resolveClassStore`; orders is **flipped** (`817720e2a` O2 `resolveOrderStore` + gate cross-read; `56c9febeb` cutover), mail is **flip-ready**, nudge oracle-leak fixed (`e9f2ac00d` excludes `gc:nudge` from Ready).
- **Postgres backend** built + conformance-passing (`internal/beads/postgres_store.go`), schema-per-class, provisioned via `gc beads postgres init`; registry openers in `cmd/gc/postgres_class_store.go`.
- **`gc beads migrate`** (alias `migrate-sqlite`, `cmd_beads_migrate.go:39-41`) dispatches dest per the configured backend — sqlite **or** postgres (`78f77a966`).
- **Close-safe Postgres class handle** (`b47f5daa3`): `noClosePostgresStore` neutralizes `CloseStore` so a close-after-use caller can't close the SHARED pool.

**Gotchas that bite:** `MemStore` does NOT preserve caller-pinned IDs; `SQLiteStore` AND `PostgresStore` DO (PG bumps its native sequence — `TestPostgresStorePinnedIDBumpsSequence`). Tests asserting specific IDs must use SQLite/Postgres. The order single-flight gate uses `beads.HandlesFor(store)` → it must stay `beads.Store` (do NOT narrow it to a class seam). `genschema` / `go run ./cmd/genspec` regenerates `docs/reference/schema/*` on config changes — commit them with the change.

> **Note:** `HANDOFF.md` and `NEXT-SESSION.md` predate the Postgres epoch — their "15 commits / `bd|sqlite` / `migrate-sqlite`" ledger is one epoch stale. This file supersedes them for the landed/remaining ledger. Refresh those two before trusting their task lists.

---

## 3. THE OWNER DECISION — do all three remaining classes "full"

Execute the remaining classes **full**, phased + verified, in this order: **nudges** (reversible), then **graph** (P6, highest blast radius), then **sessions** (P5, irreversible). Sessions live-adopt is BUILT + conformance-tested but **NOT run against a live city** (none available here).

---

## 4. NUDGES — full relocation ✅ DONE (`d722e45b9` + `1d45620f8`)

> **✅ LANDED 2026-06-26.** Executed as written PLUS the three sites the list
> below missed (`finalizeReadyWaitFromNudge`,
> `prepareWaitWakeStateForCityWithSnapshot`, `nextWaitDeliveryAttempt` +
> `cmdWaitSetStateResult`/`retryClosedWait`). Byte-identical at default; full
> suite + sharded guard green. The rest of this section is the executed record.

The nudge **bead is a pure shadow**: `dispatchAllQueuedNudges` (`nudge_dispatcher.go:122`) loads dispatch state from the flock-guarded queue file via `nudgequeue.LoadState(cityPath)`; the bead is "its persistent shadow" (`nudgequeue/store.go:11-13`). Relocating the bead cannot change dispatch semantics. The leaf ops already take `nudgequeue.NudgeStore` (`var _ NudgeStore = beads.Store(nil)`, `internal/nudgequeue/store.go:14-25`); the P1-nudges seam landed byte-identically in `303a80158`.

### The two correctness traps (most important facts in this section)

1. **`blockedQueuedNudgeReason` reads a WAIT bead, which is SESSION-class.** `splitQueuedNudgesForDelivery` (`cmd_nudge.go:1252`) calls `blockedQueuedNudgeReason` (`:1272`), which does `store.Get(item.Reference.ID)` where the reference is a wait bead (`session.IsWaitBead`, `:1283`). Wait beads are created on the **work/session store** (`cmd_wait.go:258`, `waitBeadType`) — they are **NOT** nudge-class. This `Get` MUST stay on the **work store**, never the relocated nudge store. This is the dominant correctness constraint of the whole relocation.
2. **`stampLastNudgeDeliveredAt` is a SESSION-bead write currently routed on the nudge variable.** It appears as `stampLastNudgeDeliveredAt(deliveryStore, …)` at `cmd_nudge.go:1131` and `stampLastNudgeDeliveredAt(store, …)` at `cmd_sling.go:1563` — both write **session** metadata. They must move to `workStore`, NOT the nudge store.

### Caller classification (verified)

`openNudgeBeadStore` (`nudge_beads.go:43-49`) = work store. Its callers split into:

- **Pure session-op (leave on `openNudgeBeadStore` / work store):** `resolveNudgeTarget` (`cmd_nudge.go:939` — `resolveSessionIDMaterializingNamed` + `store.Get(sessionID)`), prime fence `withNudgeTargetFence(openNudgeBeadStore(...))` (`cmd_prime.go:291` — reads `loadSessionBeads`).
- **Pure nudge-shadow (relocate wholesale via a new `openNudgesClassStore`):** `claimDueQueuedNudgesMatching` (`cmd_nudge.go:1467`), `listQueuedNudges` (`:1503`), `listQueuedNudgesForTarget` (`:1539`), `enqueueQueuedNudgeWithStore` self-open (`:1628`), `ackQueuedNudgesWithOutcome` (`:1728`), `releaseQueuedNudgeClaims` (`:1777`), `recordQueuedNudgeFailureDetailed` self-open (`:1832`), `withdrawQueuedWaitNudges` (`cmd_wait.go:1443` — `nudgequeue.WithdrawWaitNudges` touches only `nudge:` beads).
- **Mixed (split into `workStore` + `nudgeStore`):** `cmdNudgeDrain` (`:422` — `stampLastNudgeDeliveredAt` is the session write), `cmdNudgePoll` (`:537`), `deliverSessionNudge` (`:598`), `sendMailNotify` (`:875`), `tryDeliverQueuedNudgesByPoller` (`:1052`).

### Split signatures (two-store; tests pass the same store twice)

1. **`tryDeliverQueuedNudgesByPoller(target, workStore, nudgeStore, sp, …)`** (`cmd_nudge.go:1052`): `nudgeStore` for `recordQueuedNudgeFailureWithStore` (`:1075/:1118`) and the nudge-shadow concerns of `splitQueuedNudgesForDelivery` (`:1080`); `workStore` for `workerHandleForNudgeTarget` (`:1099`), the wait-bead read inside `blockedQueuedNudgeReason`, AND `stampLastNudgeDeliveredAt` (`:1131`). Update `nudge_dispatcher.go:189` and 6 test callers (`cmd_nudge_test.go:2363, 2475, 2520, 2570, 2649, 2726`) to pass `store, store`.
2. **`dispatchReadyWaitNudgesWithSnapshot(cityPath, cfg, workStore, nudgeStore, now, sessionBeads)`** (`cmd_wait.go:1132`): `workStore` for `loadWaitBeadsForOpenSessions` (`:1140`), `stampWaitLookupCapDiagnostic` (`:1166/:1233`), `store.SetMetadata(wait.ID, "nudge_id", …)` (`:1188`), `finalizeReadyWaitFromNudge` (`:1222`); `nudgeStore` for `findQueuedNudgeBead` (`:1163`) and `enqueueQueuedNudgeWithStore` (`:1185`). Update the `dispatchReadyWaitNudges` shim (`:1128`) to pass `store, store`. (Includes the second finalize interleave at `:1222-1233`.)
3. **`sweepStaleNudgeMail(nudgeStore, mailStore, nudgeState, now, …)`** and **`countStaleNudgeMail(nudgeStore, mailStore, …)`** (`nudge_mail_sweep.go:46/:143`): Phase 1 (`nudgeBeadLabel` list + `SetMetadataBatch` + close, `:59-96`) → `nudgeStore`; Phase 2 (`type=message label=read`, `:106-133`) → `mailStore`. Update `city_runtime.go:1486`, the gc-order CLI wrappers `cmdOrderSweepNudgeMailDryRun`/`Run` (`cmd_order.go:1755/:1773`), and **all 15** `nudge_mail_sweep_test.go` callers (lines 57, 86, 120, 157, 183, 207, 250, 308, 329, 370, 395, 423, 641, 657, 702 — pass `store, store`).
4. **`splitQueuedNudgesForDelivery` / `blockedQueuedNudgeReason`** (`cmd_nudge.go:1252/:1272`): the wait-bead `Get` (`:1276-1283`) MUST use `workStore`. There is no nudge-shadow op inside `blockedQueuedNudgeReason`, so keep it on `workStore` explicitly at every call site (tests already pass the work store).
5. **`deliverSlingNudge(target, sp, workStore, nudgeStore, cityPath, …)`** (`cmd_sling.go:1547`): session ops `workerObserveNudgeTarget`/`workerHandleForNudgeTarget`/`stampLastNudgeDeliveredAt` (`:1549/:1553/:1563`) → `workStore`; `enqueueQueuedNudgeWithStore` (`:1570`) → `nudgeStore`. Callers `cmd_sling.go:1460/:1487` compute `nudgeStore := resolveNudgesStore(sessionStore, cfg, cityPath, nil)`. `buildSlingNudgeTarget` (`:1535`) keeps `withNudgeTargetFence(store, …)` (`:1537`) on `workStore`.
6. **`enqueueManagedNudgeThenWake(target, workStore, nudgeStore, item)`** (`cmd_nudge.go:695`): `enqueueQueuedNudgeWithStore`/`rollbackQueuedNudge` (`:696/:700`) → `nudgeStore`; `requestManagedNudgeWake` (`:699` → `store.Get(target.sessionID)` + `session.WakeSession`, `:712-716`) → `workStore`. Propagate two stores up through `deliverSessionNudgeWithWorker` (`:606`), `queueManagedSessionNudgeWake` (`:683`), and the `sendMailNotifyWithWorker` mail path (`:886`, session writes at `:904/:911/:921`).

### The seam + helpers

- Add **`resolveNudgesStore`** to `cmd/gc/class_store.go` (after `:193`), mirroring `resolveOrderStore` (`:191`), returning `beads.Store` (satisfies `nudgequeue.NudgeStore` for free):
  ```go
  func resolveNudgesStore(workStore beads.Store, cfg *config.City, cityPath string, rec events.Recorder) beads.Store {
      return resolveClassStore(workStore, cfg, cityPath, config.BeadClassNudges, rec)
  }
  ```
- Add **`openNudgesClassStore(cityPath)`** next to `openNudgeBeadStore` (`nudge_beads.go:43`): loads cfg, calls `resolveNudgesStore(workStore, cfg, cityPath, nil)`. Repoint ONLY the pure-nudge self-openers to it (list in §4 caller classification). **Leave `resolveNudgeTarget` (`:939`) and the prime fence (`cmd_prime.go:291`) on `openNudgeBeadStore`.**

### Entry points → how each gets its nudge store

- **Controller (carries `cr.rec`):** `nudgeDispatchTick` (`city_runtime.go:2674`) → `resolveNudgesStore(cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)`, pass `(workStore, nudgeStore)` into `dispatchAllQueuedNudges`→`tryDeliverQueuedNudgesByPoller`. Wait dispatch (`city_runtime.go:2312`) → pass `nudgeStore` into `dispatchReadyWaitNudgesWithSnapshot`. `runNudgeMailSweepWatchdog` (`city_runtime.go:1463/:1486`) → `(resolveNudgesStore(…,cr.rec), resolveMailMessagesStore(…,cr.rec), workStore)`.
- **CLI (no bus → recorder-less):** pass `rec=nil` to `resolveNudgesStore`. Acceptable: the recorder uses actor `cache-reconcile` (`class_store.go:76`); CLI nudge-shadow writes are transient and gate-excluded, and the controller stays the single durable writer.

### Multi-writer ops constraint (non-default backend only)

Postgres handles concurrent writers natively (shared pool, schema-per-class). SQLite is process-local: with `gc nudge poll`/`drain` sidecars writing shadows from separate processes AND the controller writing from its own, two processes open distinct SQLite handles to the same file → `SQLITE_BUSY`. Nudges-on-SQLite is safe only when shadow writes funnel through **one** process (supervisor dispatcher mode, `cfg.Daemon.NudgeDispatcherMode()=="supervisor"`); legacy per-session pollers should stay on `bd` or use Postgres. No code gate is required for the byte-identical default; this is an ops note for the non-default backend.

### Verify (nudges)

```bash
make test-cmd-gc-process-parallel      # cmd_nudge / nudge_mail_sweep / nudge_dispatcher / cmd_wait / cmd_sling / class_store
go test ./internal/nudgequeue/...      # seam conformance
go vet ./...
```
- Keep `class_store_test.go:198` green (asserts unregistered/`bd` nudges return `workStore` — the byte-identical default).
- Postgres end-to-end: extend `TestBeadsMigrate_ToPostgres` (`postgres_class_store_test.go:45`, already parameterized on `BeadClassNudges`) with a relocated-store ensure→terminalize→sweep round-trip; a SQLite analogue (`openClassSQLiteStore`) proves the shadow lands in `.gc/nudges/`.

### Step-by-step checklist (nudges)

1. Add `resolveNudgesStore` (`class_store.go` after `:193`) + `openNudgesClassStore(cityPath)` (`nudge_beads.go:43`).
2. Repoint pure-nudge self-openers to `openNudgesClassStore`: `cmd_nudge.go:1467, 1503, 1539, 1628, 1728, 1777, 1832`; `cmd_wait.go:1443`. **Leave `cmd_nudge.go:939` and `cmd_prime.go:291`.**
3. Split `sweepStaleNudgeMail`/`countStaleNudgeMail` → `(nudgeStore, mailStore, …)` (`nudge_mail_sweep.go:46/:143`); update `city_runtime.go:1486`, `cmd_order.go:1755/:1773`, 15 callers in `nudge_mail_sweep_test.go`.
4. Split `tryDeliverQueuedNudgesByPoller` → `(workStore, nudgeStore)` (`cmd_nudge.go:1052`); route the wait-read in `blockedQueuedNudgeReason` (`:1272`) and `stampLastNudgeDeliveredAt` (`:1131`) on `workStore`. Update `nudge_dispatcher.go:189` + 6 test callers.
5. Thread two stores through `dispatchAllQueuedNudges` (`nudge_dispatcher.go:115/:179/:189`) and `nudgeDispatchTick` (`city_runtime.go:2674` → `resolveNudgesStore(…,cr.rec)`).
6. Split `dispatchReadyWaitNudgesWithSnapshot` (`cmd_wait.go:1132`, shim `:1128`); update `city_runtime.go:2312`.
7. Split `deliverSlingNudge` (`cmd_sling.go:1547`) + callers (`cmd_sling.go:1460/:1487`).
8. Split `enqueueManagedNudgeThenWake` (`cmd_nudge.go:695`); propagate through `deliverSessionNudgeWithWorker` (`:606`), `sendMailNotifyWithWorker` (`:886`), `queueManagedSessionNudgeWake` (`:683`).
9. `runNudgeMailSweepWatchdog` (`city_runtime.go:1463`): pass `resolveNudgesStore(…,cr.rec)` + `resolveMailMessagesStore(…,cr.rec)`.
10. Run the verify block. Commit with `git commit --no-verify`.

---

## 5. GRAPH — P6 (highest blast radius; final point-of-no-return)

> **FLAG:** P6 is the single highest-blast-radius milestone. Its last step deletes `coordrouter.Router` — the **read-federation safety net that retroactively backstops mail/orders/nudges/sessions** (DESIGN §8, `:274-277`). Every prior phase's revert assumed federation still existed. Execute strictly in order; each step is independently green and revertible **until** the final Router deletion.

### Why graph is different (verified)

Graph routes via `coordrouter.Router`, **NOT** `resolveClassStore` — there is no `resolveGraphStore`. The Router is built only in `routedPolicyStore` (`cmd/gc/api_state.go:240-247`), gated on `graphStoreSQLiteEnabled` (`:272-274`) which reads ONLY the legacy `cfg.Beads.GraphStore` knob; `registerGraphStoreBackend` (`:318-346`) registers only a **SQLite** backend (no Postgres branch). Routing happens at coarser granularity than per-bead: whole-plan `ClassifyGraphPlan` (`internal/coordclass/classify.go:88`, dispatched at `internal/coordrouter/router.go:151,167`), the order-gate multi-store read set `storesForGate` (`cmd/gc/order_dispatch.go:512-532`), and the capability seam `beads.GraphApplyFor(store)` (`internal/molecule/molecule.go:476,785`; `internal/dispatch/ralph.go:394`). A `resolveClassStore`-style per-bead split would tear an atomic graph apply across two stores.

### The config-conflict bug ✅ FIXED (`1bf1cd3a5`)

> **✅ Step 0 LANDED 2026-06-26.** `ValidateBeadsClasses` rejects `graph=postgres`
> at config load (wired into `Load` + compose root-validation); shared
> `normalizeBackend` extracted; `validate_beads_classes_test.go` added. Proceed to
> the ordered P6 execution below (Step 1). The analysis below is retained as the
> rationale.

### The config-conflict bug (CONFIRMED — fix it FIRST, before any P6 wiring)

`NormalizedClassBackend("graph")` honors an explicit `[beads.classes.graph].backend` (`internal/config/config.go:1362-1378`: `sqlite`→sqlite `:1366`, `postgres`→postgres `:1368`, legacy fallback only when no explicit entry `:1374`), but `graphStoreSQLiteEnabled` ignores `Classes` entirely. So `[beads.classes.graph].backend="postgres"` makes `NormalizedClassBackend` report **postgres** (and `gc beads postgres init graph` provisions a PG schema — `cmd_beads_postgres.go:63,70`) while the Router builds **no Router / SQLite only** → **silent wrong-store, no error, no log.** Seven production consumers gate on the legacy knob alone and will all disagree with `NormalizedClassBackend(graph)`: `routedPolicyStore` (`api_state.go:241`), `registerGraphStoreBackend` (`api_state.go:319`), `controlStoreWithGraphRouting` (`cmd_convoy_dispatch.go:458`), graph claim routing (`cmd_hook_claim.go:276`), `work_query_probe.go:89`, `classifyBdShimVerb` (`cmd_bd_shim.go:1049`), `readyOracleCmd` (`internal/config/config.go:3731`).

**Step 0 fix (small, safe, do-it-first):** reject the dueling state at config-load. Add `internal/config/validate_beads_classes.go` mirroring `ValidateDoltConfig` (`internal/config/validate_dolt.go:1-27`); wire it into `Load` after `ValidateDoltConfig` (`config.go:5076`). While the Router is the only graph wiring, `graph` may only be `bd` or `sqlite`; reject `postgres`.

```go
// internal/config/validate_beads_classes.go (new)
package config

import "fmt"

// ValidateBeadsClasses rejects per-class backends the runtime cannot honor.
// Graph is wired through coordrouter.Router (gated on the legacy graph_store
// knob), which only registers a SQLite backend — so graph=postgres would set
// NormalizedClassBackend(graph)=postgres while the Router still serves
// SQLite/Dolt: a silent wrong-store. Reject it until graph routes through
// resolveClassStore (P6).
func ValidateBeadsClasses(cfg *City, source string) error {
	if cfg == nil {
		return nil
	}
	if c, ok := cfg.Beads.Classes[BeadClassGraph]; ok {
		switch normalizeBackend(c.Backend) { // "" / bd / sqlite / postgres
		case BeadsBackendBD, BeadsBackendSQLite, "":
		default:
			return fmt.Errorf(
				"%s: [beads.classes.graph] backend=%q is not honored by the graph router "+
					"(graph supports only \"bd\" or \"sqlite\" today); remove it or use graph_store=\"sqlite\"",
				source, c.Backend)
		}
	}
	return nil
}
```

Extract the lowercasing inlined in `NormalizedClassBackend` (`config.go:1365`) into a shared `normalizeBackend(string) string` so the guard and the dispatcher can't drift (DRY). **Behavior-change risk: none at default** — no `[beads.classes.graph]` entry and existing `graph_store="sqlite"` cities are untouched (the guard inspects only `Classes["graph"]`). The guard *permits* `Classes["graph"]=sqlite` (harmless, the future P6 spelling; still a no-op divert today since `graphStoreSQLiteEnabled` reads only `GraphStore`) and rejects only `postgres`. **Do NOT** instead "make `graphStoreSQLiteEnabled` consistent with `NormalizedClassBackend`" as the first step — that would make `Classes["graph"]=sqlite` start building a Router on cities that had none (new store file, new `gcg` id-prefix, federation overhead): a real broad behavior change that belongs inside P6 under conformance. Verify: `go test ./internal/config/` + `make test-cmd-gc-process-parallel`. One commit, pure addition.

### Ordered P6 execution (strictly in order)

**Step 1 — Give `*beads.PostgresStore` graph-apply parity with SQLite ✅ DONE (`4d77288b9`, re-scoped 2026-06-26).**

> **RE-SCOPE (owner-approved).** The plan's literal Step 1 — "promote `GetNode` /
> `ListNodesByRoot` / `ListNodeEdges` / `CloseSubtree` / `ReadyCandidates` /
> `FindOrCreateByKey` onto the `GraphStore` interface" — was **rejected as written**: it is
> NOT additive (it breaks three implementors that satisfy `GraphStore` today via the single
> `ApplyGraphPlan` — `*BdGraphStore`, `*fakeGraphStore` `stores_test.go:57`, `fakeGraph`
> `conformance_test.go:15`) and it directly violates the interface's OWN written contract
> (`stores.go:21-25,52-64`: these six are a growth path promoted "only as a consumer is
> migrated behind the seam … **never ahead of a caller**"). Several also have no atomic backing
> and live above the beads layer (`CloseSubtree`/`ListNodesByRoot` in `internal/molecule`).
>
> The genuine, caller-independent, design-compliant slice — the real intent behind the plan's
> "(and `*beads.PostgresStore` — new for this migration)" — was the only thing actually
> blocking `graph=postgres`: **`*PostgresStore` had no `ApplyGraphPlan` at all** (no method, no
> compile-time assertion, unlike `*SQLiteStore`). That is now implemented
> (`internal/beads/postgres_store_graph_apply.go`): `ApplyGraphPlan` +
> `ApplyGraphPlanWithStorage`, mirroring the SQLite three-pass shape but minting final ids in-tx
> via `nextval('bead_seq')` (no mint-then-remap retry; tier mapping identical). Proven by the
> DSN-gated shared `RunGraphStoreTests` conformance for PostgresStore plus white-box parity
> tests (edge→dep wiring, parent linkage, atomic rollback, ephemeral tier). The `GraphStore`
> **interface is unchanged**, so default (`bd`) is byte-identical and the Step-0 guard still
> rejects `graph=postgres` until Step 5.
>
> **The six read/finalize methods move to Step 4**, where the `?type=molecule` augment + order
> gate become their real callers (honoring "never ahead of a caller"). Use `beads.Bead` for
> nodes, `beads.Dep` for edges, `beads.ReadyQuery` for candidates — there are no bespoke
> node/edge/candidate types in `stores.go` to invent.

**Step 2 — MOVE `ClassifyGraphPlan` into `internal/graphstore` (net-new package; does not exist yet).** Move `ClassifyGraphPlan` + `classifyFields`/`isWispMetadata` from `internal/coordclass/classify.go:88`; update the two Router call sites (`internal/coordrouter/router.go:151,167`) and test imports. Grep-sweep `ClassifyGraphPlan` and `coordclass.ClassGraph` (No-Semantic-Search rule). Verify: `go test ./internal/graphstore/ ./internal/coordrouter/ ./internal/coordclass/`; `TestClassifyGraphPlan` moves with it (`classify_test.go:77`).

**Step 3 — Provider-aware `ResolveStoreRef`.** Re-point the `(storeRef,id)` prefix resolver (`internal/dispatch/runtime.go:48-63`; template `api_state.go:630`) onto the existing `internal/storeref` package (`internal/storeref/storeref.go` — already exists, the P3.5 F3 resolver), extending the prefix switch for the class prefixes (`gcm-`/`gco-`/`gcn-`/`gcs-` alongside `gcg-`→graph, `gc-`/`ga-`→work; DESIGN §9, `:286`). Live callers to keep green: `cmd/gc/cmd_convoy_dispatch.go:205`, `internal/dispatch/runtime.go:796-802`. Verify: `go test ./internal/storeref/ ./internal/dispatch/`.

**Step 4 — Promote ONLY the read methods a migrated caller needs, then rewire the `?type=molecule` augment AND the order gate to `GraphStore` (BEFORE any Router deletion).** This is where the deferred interface promotion lands, one method per real caller: add `GetNode`/`ListNodesByRoot`/`ListNodeEdges`/`ReadyCandidates` (the READ subset the two paths actually use) to `GraphStore`, and in the SAME commit add their implementations to `*BdGraphStore` + the test fakes (`*fakeGraphStore`, `fakeGraph`) so the build stays green — `*beads.SQLiteStore`/`*beads.PostgresStore` get them for free where a 1:1 leaf exists (`Get`, `DepList`). Defer `CloseSubtree`/`FindOrCreateByKey` further still (finalize/idempotency — no read-path caller here; they live above the beads layer in `internal/molecule`). This is the load-bearing step: both read paths must stop relying on Router federation *while federation still exists as a fallback*, so a defect surfaces under the safety net. `?type=molecule` + `gc.kind=workflow` augment: `internal/api/huma_handlers_beads.go:113-181` and the `ReadyGraphOnly` hot loop `:341-342` → read the promoted `GraphStore` directly. Order gate `storesForGate` assembly: `cmd/gc/order_dispatch.go:512-532` → source the graph-read store from `GraphStore`, not the Router-wrapped `store`. Verify: split-topology `is_blocked` conformance test (DESIGN §9.2/§10 gate, `:284`, `:315`) proves `GraphStore` forbids ready-blocking deps identically to federation; order-gate conformance (the P4 hard-prereq) stays green; `make test-cmd-gc-process-parallel`.

**Step 5 — Delete `coordrouter` (final point-of-no-return).** Confirm zero non-test callers of all three mechanisms — create-time classify (`router.go:113`), by-id probing (`router_mutation.go:21`), read federation (`router_federation.go:242`) — and the sole `coordrouter.New` site (`api_state.go:240-244`). Demote `coordclass.Classify` to test/audit-only (DESIGN §2, `:83-85`). Delete `internal/coordrouter` + `internal/coordclass` (both fork-owned → grep-provable, zero merge cost). Retire `graphStoreSQLiteEnabled` and fold graph into `resolveClassStore` so the seven legacy-knob consumers and `NormalizedClassBackend(graph)` finally read one source of truth. Verify: grep-guard for `coordrouter.New` / `coordclass.` in non-test files returns zero; full `make test-cmd-gc-process-parallel` + integration shards; the Step-0 guard can now relax to *allow* `graph=postgres` (it routes through `resolveClassStore`).

**Blast-radius rationale:** Steps 1-3 are additive/mechanical (independently revertible, no federation dependency). Step 4 is the behavioral pivot done *under* the federation net. Step 5 removes the net and is irreversible — gated on Step 4's split-topology proof. **Never collapse Steps 4 and 5 into one change.**

---

## 6. SESSIONS — P5 (IRREVERSIBLE point-of-no-return; do last)

> **IRREVERSIBILITY (blunt):** Sessions is the **one class where a botched flip loses RUNNING-AGENT CONTINUITY, not just history.** The OPEN-session metadata long-tail (`session_key`, `resume_*`, `transport`, drain/wake/quarantine accrual, continuity epoch) is **persisted-only** state. Crash-adoption (`dolt_scope_watchdog.go`, `dolt_cleanup_reaper.go`, the `syncSessionBeads` adoption barrier) reconstructs identity + liveness from the **process table** — it does NOT rebuild that long-tail. So a flip cannot be a lazy drain-then-switch for already-open sessions: it needs an **atomic, controller-locked copy of the live OPEN corpus** + a **post-copy projection-equality** pass, and **once the read path flips the bd rows are abandoned with no automatic reverse.** Build it, prove it offline, config-gate it, then STOP — there is no live city here and no cheap undo.

### Four corrections to carry forward (verified)

1. **`waits.go` is `internal/session/waits.go`** (313 LOC, controller + per-request shared helpers) — NOT `cmd/gc/waits.go`. There is a sibling `internal/nudgequeue/waits.go`; do not confuse them.
2. **No `SessionStore` seam exists yet.** Unlike `beadmail.MailStore` (`internal/mail/beadmail/beadmail.go:41`) and `orders.OrderStore` (`internal/orders/store.go:22`), there is ZERO `SessionStore` interface in the tree — the seam is **net-new work**, not a flip of an existing one.
3. **The live-adopt atomic-copy command is NOT built.** What exists is `internal/storemigrate.Migrate` (`migrate.go:34`) — an **offline bulk list-and-copy** (ID-preserving, idempotent, **no controller lock, no quiesce, no projection-equality**), driven by `gc beads migrate` (`cmd_beads_migrate.go:37`). Its header (`migrate.go:6-7`) calls it "distinct from the clean drain-then-switch path." The irreversible live-adopt op is **green-field**.
4. **Conformance does not yet cover projection-invariance.** Existing suites (`coordtest.RunGraphStoreTests`, the classed-store suite `sqlite_store_conformance_test.go:31,40`) assert store CRUD/graph fidelity, NOT `ProjectLifecycle`/`infoFromBead` projection-equality across backends. That assertion is a **new P5(a) deliverable.**

### Key verified facts

- **`infoFromBead` is IMPURE → stays in Manager; only the pure persisted subset goes to the codec.** (`internal/session/manager.go:1543`.) It reads live runtime via `m.sp` (`IsRunning`/`IsAttached`/`GetLastActivity`) and mutates the ACP router (`transportForBead` `:196`, `routeACPIfNeeded` `:263`). Pure persisted subset (`:1564-1587`, straight `b.Metadata[...]`/`b` reads): `ID, Template, State(pre-runtime), Closed, Title, Alias, AgentName, Provider, Transport(metadata-only), Command, WorkDir, SessionName, SessionKey, ResumeFlag, ResumeStyle, ResumeCommand, CreatedAt, LastNudgeDeliveredAt`. Runtime enrichment (`:1550-1562, 1590-1595`) stays in Manager.
- **`session_name` uniqueness is a RECONCILER invariant — NO bare `UNIQUE(session_name)`.** Canonical-election lives in the reconciler (`cmd/gc/session_beads.go:884-919` index/elect/close-losers; named-session repair `:443-499`). Both backends key `beads` only on `id TEXT PRIMARY KEY` (`sqlite_store.go:191-192`, `postgres_store.go:199-200`); `session_name` is a non-unique EAV row (`idx_metadata_key_value`, `sqlite_store.go:233`, `postgres_store.go:239`). A bare UNIQUE would reject the transient duplicate that duplicate-then-elect *requires* — the new session schema MUST keep `session_name` non-unique.
- **Injection point = `internal/api/session_manager.go:11`** (`s.sessionManager(store)`), the SINGLE per-request construction chokepoint, called with the work store at 17 sites. The seam swaps `store` → `resolveSessionStore(store, cfg, cityPath, rec)` here, once. `cmd/gc/api_state.go` does NOT construct managers (only holds `cityBeadStore` `:50` + `SessionProvider()` `:1063`).
- **`ProjectLifecycle` is PURE** (`internal/session/lifecycle_projection.go:374`): operates solely on `LifecycleInput`; no `store`/`sp`/`runtime`/`os`/`exec`. The only nondeterminism is the `now.IsZero()→time.Now().UTC()` default (`:379-382, :609`), which callers kill by passing `input.Now`. This is the projection-invariance conformance target.

### Session-bead writers (controller vs per-request)

- **Controller (reconciler loop):** `syncSessionBeads`/`…WithSnapshotAndRigStores` (`cmd/gc/session_beads.go:792, 809, 826`, driven from `city_runtime.go:2719, 2823`); `session_reconcile.go` hot path (`:568, 578, 664, 676, 706, 713, 737, 760, 793, 810, 819, 913`); `preWakeCommit`/`PreWakePatch` (`cmd/gc/session_wake.go:31, 60`).
- **Both (explicit `store beads.Store` arg):** `internal/session/waits.go` mutators `WakeSession` (`:203`), `ReassignWaits` (`:153`), `CancelWaits[AndCollectNudgeIDs]` (`:147/:301`), `StampWaitLookupCapMetadata` (`:239`).
- **Per-request (API handlers via `s.sessionManager(store)`):** `session.Manager` mutators `Create*` (`:351-376`), `Suspend` (`:780`), `Close[Detailed]` (`:882-888`), `BeginDrain` (`:1003`), `Archive` (`:1018`), `Quarantine` (`:1033`), `Reactivate` (`:1049`), `ConfirmCreation` (`:1076`), `UpdatePresentation` (`:1140`), `UpdateTemplateOverrides` (`:1192`). All go through `m.store` (`manager.go:140`) — the single injection target.

### (a) Buildable + verifiable WITHOUT a live city

**P5a-1 — Extract the `SessionStore` seam over the persisted-field codec.** Add `type SessionStore interface` in `internal/session/` mirroring the leaf ops the writers use (`Create, Get, List, SetMetadata, SetMetadataBatch, Update, Close`, plus the dep/wait reads `ListSessionWaitBeads`/`ReassignWaits` touch); prove `var _ SessionStore = beads.Store(nil)` (the narrowing pattern from `orders/store.go:32`). Factor the pure persisted subset of `infoFromBead` (`manager.go:1564-1587`) into `infoFromPersisted(b beads.Bead) Info`; `Manager.infoFromBead` calls the codec then enriches. *Verify:* `go test ./internal/session/...`; assert `infoFromBead` byte-identical pre/post for a closed bead (no runtime) and an active bead (enrichment unchanged). *Risk:* LOW.

**P5a-2 — Inject at the single API chokepoint.** In `internal/api/session_manager.go:11`, wrap `store` with `resolveSessionStore(store, cfg, cityPath, rec)` calling `resolveClassStore(workStore, cfg, cityPath, config.BeadClassSessions, rec)` (`cmd/gc/class_store.go:158`), mirroring `resolveOrderStore` (`:191`). Default `bd` returns the work store → byte-identical, all 17 call sites unchanged. *Verify:* `go test ./internal/api/...`; assert default path returns the same store instance (the `BeadsBackendBD` early-return, `class_store.go:163-164`). *Risk:* LOW.

**P5a-3 — SQLite + Postgres session schema with the reconciler invariant.** Reuse the generic `beads` EAV schema (`sqlite_store.go:191`, `postgres_store.go:199`) — **no new DDL, explicitly NO `UNIQUE(session_name)`**. Register the `gcs` prefix (already reserved, `internal/config/reserved_prefixes.go:19`) for the migrate dest. Add a guard test asserting no backend schema introduces a unique index on a `session_name` metadata value. *Verify:* run `syncSessionBeads` canonical-election (`session_beads.go:884-919`) against a SQLite-backed store; assert duplicates coexist then elect identically to bd. *Risk:* LOW–MED.

**P5a-4 — Conformance incl. projection-invariance (NEW).** Extend the classed-store conformance (`sqlite_store_conformance_test.go:40`, `postgres_store_conformance_test.go`) for `BeadClassSessions`. Add a **projection-invariance suite:** seed an identical session corpus into bd, SQLite, PG; assert `ProjectLifecycle(input)` (`lifecycle_projection.go:374`) and `infoFromPersisted(b)` are equal across all three for every bead (fixed `input.Now` kills the `time.Now` default). *Verify:* `go test ./internal/beads/... ./internal/session/...` with `GC_TEST_POSTGRES_DSN` set. *Risk:* MED — this suite is the load-bearing proof of ZERO behavior change; it must pass before any live flip is contemplated.

### (b) Live-adopt atomic-copy COMMAND — BUILD + conformance-test, do NOT run

**P5b-1 — Build `gc beads adopt sessions` (controller-locked atomic copy).** Net-new; `storemigrate.Migrate` (`migrate.go:34`) is an offline copier with no lock/quiesce and is **not** sufficient for open sessions. Against the live controller, the new op must: (1) acquire a controller lock / quiesce the session reconciler, (2) snapshot the OPEN session+wait corpus, (3) ID-preserving copy into the configured SQLite/PG session store, (4) run a **post-copy projection-equality gate** (`ProjectLifecycle` old==new for every open bead) and ABORT on any mismatch, (5) flip the read path only on a clean gate, (6) leave bd rows intact (no auto-retire). Reuse `Migrate`'s ID-preserving copy core for closed/historical beads; the open-corpus copy is the new lock-gated leg. *Verify offline:* unit + conformance against fake/in-memory stores with a simulated open corpus, including the **abort-on-projection-mismatch** branch and the controller-lock contract. **Config-gate it** so it cannot run unless `[beads.classes.sessions].backend` is sqlite/postgres. *Risk:* **HIGH / IRREVERSIBLE — the P5 point-of-no-return.**

**P5b-2 — Hand the command to the user; do NOT execute here.** No live city is available in this environment. Running `gc beads adopt sessions` against a real city is the user's config-gated, irreversible op. Deliver it built + green on the offline conformance/projection suites with the abort path proven, and STOP.

### Key file:line anchors (sessions)

- Seam patterns to mirror (SessionStore does not exist yet): `internal/orders/store.go:22-32`, `internal/mail/beadmail/beadmail.go:41-51`.
- Codec extract: `internal/session/manager.go:1543-1598` (impure) / `:1564-1587` (pure subset).
- Injection: `internal/api/session_manager.go:11`.
- Reconciler invariant: `cmd/gc/session_beads.go:884-919`, `:443-499`.
- Pure projection: `internal/session/lifecycle_projection.go:374`.
- Offline copier (NOT live-adopt): `internal/storemigrate/migrate.go:34`; `cmd/gc/cmd_beads_migrate.go:37`.
- Schema (no UNIQUE on session_name): `internal/beads/sqlite_store.go:191`, `internal/beads/postgres_store.go:199`.

---

## 7. DESIGN.md §10 — remaining-phase ledger (refreshed)

`engdocs/plans/infra-store-decouple/DESIGN.md:304-316`. **P0/P1/P2 DONE.** Remaining:

- **P3** full typed wire + dashboard regen (`:311`) — re-root `sessionResponse` on `Info`; typed `GET /v0/sessions`, `/v0/orders/tracking`, `/v0/nudges`, `/v0/convoys`; SSE payloads; full CI gate set (openapi×3 + `npm run gen` + vitest + dashboard-check). Convoy ownership-split moved OUT to its own gated phase.
- **P3.5** cross-store foundation (`:312`) — `(storeRef,id)` resolver (partially landed `630c474e2`/`809cd3dd0`), controller-mediated write API, controller-owned retention sweep, storehealth `*.sqlite`, and **inventory + re-point ALL cmd/gc + internal/api direct infra-bead readers/writers** (status/doctor/demand-scan/`session_wake.go`/`session_beads.go`). Prerequisite of any physical move.
- **P4** SQLite cutover mail/orders/nudges (`:313`) — mail flip-ready, orders flipped, nudge oracle-leak fixed; remaining gated on P3.5 + the three prereqs (mail session read-path, orders graph read-path in `storesForGate`, nudge index).
- **P5** sessions cutover (`:314`) — REMAINING, **irreversible** (see §6).
- **P6** graph finalize + Router retirement (`:315`) — REMAINING (see §5).
- **P7** extmsg, deferred (`:316`, §11 `:318-325`) — own `ExtMsgStore`, per-family record types, **history backfill (NOT clean-drain)**, `type=task`+`gc:extmsg-*` Ready exclusion.

**§12 residual open decisions** (`:327-337`): SQLite idempotency mechanism finalized per class in P4/P5; WAL-checkpoint owner = controller (default); per-class `[beads.classes.*]` config deep-merge in `compose.go`; name the live integration upstream baseline before pricing a rebase. **§13 accepted risks** (`:339-348`): no in-prod oracle (revert = quiesce-then-flip-back), controller as single writer; all `file:line` pinned to `204b66aee` → re-verify before editing.

---

## 8. Verify harness (reference)

**`GC_TEST_POSTGRES_DSN`-gated suites** (skip when unset):
- `cmd/gc/postgres_class_store_test.go` → `TestBeadsMigrate_ToPostgres` (`:48`), `TestOpenClassPostgresStore_CloseSafe` (`:109`, the `b47f5daa3` regression guard), `TestBuildPostgresDSN_RequiresDatabase` (`:147`), `TestOpenClassPostgresStore_RoundTrip` (`:157`).
- `internal/beads/postgres_store_conformance_test.go` → `TestPostgresStoreSatisfiesClassedStoreConformance` (`:25`).
- `internal/beads/postgres_store_integration_test.go` → `TestPostgresStoreSchemaIsolation` (`:35`), `…OpenRequiresProvisionedSchema` (`:66`), `…PinnedIDBumpsSequence` (`:91`), `…FullSurface` (`:118`).

**Standard non-PG verify:**
```bash
GOCACHE=$(go env GOCACHE) go build ./cmd/gc/ && go vet ./internal/... ./cmd/gc/
go test ./internal/beads/... ./internal/coordrouter/... ./internal/mail/... \
        ./internal/nudgequeue/ ./internal/orders/ ./internal/storemigrate/ ./internal/config/
make test-cmd-gc-process-parallel    # guards order / nudge / mail / migrate wiring (~60-70s)
```

**Bottom line:** refresh `HANDOFF.md`/`NEXT-SESSION.md` to the Postgres epoch if you rely on them, then drive the still-open phases in the owner's order — **nudges** (§4, reversible) → **graph/P6** (§5, delete Router last) → **sessions/P5** (§6, irreversible; live-adopt unproven) — plus P3 typed wire, P3.5 reader/writer re-point, P7 extmsg.
