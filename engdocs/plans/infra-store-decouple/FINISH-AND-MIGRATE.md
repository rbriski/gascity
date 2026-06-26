---
title: "Infra-store decouple — FINISH & MIGRATE (sessions + graph → Postgres)"
date: 2026-06-26
branch: plan/decouple-infra-beads
head: 1ac4e0113
epic: ga-pd6tcg
---

> **Mission (owner directive, 2026-06-26):** take the infra/beads decouple to the
> finish — **migrate sessions AND graph to Postgres**, leaving **only true work in
> Dolt**. This is the whole point; nudges/mail/orders are not enough. The arc is:
> finish **graph P6** → finish **sessions P5** → fix the **tier divergence** →
> **rebase onto the live branch** → build/install → **migrate maintainer-city
> dolt→postgres** → **fixup bead references** → bring the city back up.
>
> Read this with `DESIGN.md` (§5 P6 graph, §6 P5 sessions, §8/§10 phase plan),
> `NEXT-SESSION.md` (the per-class untangle mechanics + verified file:lines), and
> `HANDOFF.md`. The companion `NEXT-SESSION-PROMPT.md` is the copy-paste pickup.

## 0. Where we are (HEAD `a91782188`, 8 commits)

| Commit | What |
|---|---|
| `1ac4e0113` | **API fix**: route session close/wake wait-nudge withdrawal to the nudges store |
| `4d77288b9` | graph **Step 1** (re-scoped): `*beads.PostgresStore` graph-apply parity (`ApplyGraphPlan`) |
| `bd9cefaf2` | graph **Path A**: wire the PG graph backend behind the Router; `graphRelocated` gate unify (7 consumers) |
| `a91782188` | graph **Path A**: relax guard to allow graph=postgres; `TestRoutedGraphStorePostgresRoutesAndConverges` |

(Earlier: `d722e45b9`/`1d45620f8` nudges relocation, `1bf1cd3a5` graph Step-0 guard, `0dbd57f36` docs.)

**Relocation readiness by class (this is the load-bearing status):**

| Class | Routes via | Ready to migrate to PG? |
|---|---|---|
| nudges | `resolveClassStore` | ✅ |
| mail | `resolveMailMessagesStore` (flip-ready) | ✅ |
| orders | `resolveOrderStore` (flipped, O2) | ✅ |
| **graph** | **`coordrouter.Router` — PG backend wired (Path A), guard relaxed** | ✅ `graph=postgres` works; data migration sqlite→pg at cutover |
| **sessions** | **work store — NO seam yet (P5a unbuilt)** | ❌ seam needed; live-adopt likely UNNEEDED (goal stops city first) |

The exhaustive 75-agent desync review (this session) confirmed the relocation is
correct on the CLI surface + byte-identical at default; the one real bug (the API
withdraw split-write) is fixed. **Deferred review findings that are now migration
prerequisites or follow-ups are in §6.**

## 1. End state

```
Dolt (versioned, history)        Postgres (schema-per-class, no Dolt history)
─────────────────────────        ────────────────────────────────────────────
true work (issue-tier beads) ──▶ stays in Dolt
                                 sessions  (gcs schema)   [P5]
                                 graph     (gcg schema)   [P6]
                                 nudges    (gcn schema)   ✅
                                 mail/msg  (gcm schema)   ✅
                                 orders    (gco schema)   ✅
```
`[beads.classes.<c>].backend = "postgres"` for the five infra classes; work stays `bd`.

## 2. Phase A — GRAPH ON POSTGRES ✅ DONE via PATH A (Router KEPT; retirement deferred)

> **OUTCOME (owner reversed Path B → Path A):** graph=postgres is reachable WITHOUT
> retiring the Router. The Router is the irreducible by-id/cross-class graph dispatcher
> (a worker's `bd close gcg-N` has only an id; class-agnostic callers can't "open the
> right store"). Deleting it (Path B) means re-implementing it as a 2-backend
> `graphRoutedStore` — same topology, irreversible, ~8 hidden couplings — purely to
> remove a fork-owned package. **Path A keeps the Router and adds a PG graph backend.**
> Steps 2–5 below are SUPERSEDED; coordrouter retirement is a separate later cleanup.
>
> **Shipped (`bd9cefaf2` + `a91782188`):** (1) Step 1 PostgresStore graph-apply parity
> (`4d77288b9`); (2) gate unify — `graphStoreSQLiteEnabled`→`graphRelocated` =
> `NormalizedClassBackend(graph)!=bd` across all 7 legacy-knob consumers; (3)
> `registerGraphStoreBackend` switches backend — SQLite keeps the EXACT legacy
> `.gc/beads.sqlite` location (`registerGraphStoreSQLite`), Postgres opens `gcg` via
> `openClassPostgresStore` (event-silent); (4) relaxed the guard to allow graph=postgres.
> Proven by `TestRoutedGraphStorePostgresRoutesAndConverges` (graph beads physically in
> `gcg.beads`; by-id close lands on PG). Byte-identical at default + graph_store="sqlite".
> Graph DATA migration (maintainer-city `.gc/beads.sqlite` → PG `gcg`) is sqlite→pg, at cutover.

The original strictly-ordered Step 1–5 plan (retire the Router) is retained below for
the eventual coordrouter-removal cleanup, but is NOT on the critical path to the migration:

1. **`*beads.PostgresStore` graph-apply parity ✅ DONE (`4d77288b9`, re-scoped).** The plan's
   literal "promote six `GraphStore` read/finalize methods" was rejected — it is not additive
   (breaks `*BdGraphStore`/`*fakeGraphStore`/`fakeGraph`) and violates `stores.go`'s written
   "never ahead of a caller" invariant. The real, caller-independent slice that unblocks
   `graph=postgres` was the missing `ApplyGraphPlan` on `*PostgresStore`; it is now implemented
   (`internal/beads/postgres_store_graph_apply.go`, in-tx `nextval` minting, SQLite-identical
   tier mapping) with DSN-gated shared conformance + white-box parity tests. **The six
   read/finalize methods move to Step 4** (promoted one-per-real-caller with the `?type=molecule`
   augment + order gate). GraphStore interface unchanged → default `bd` byte-identical, guard
   still rejects `graph=postgres` until Step 5. See `NEXT-SESSION.md §5 Step 1` for the full
   re-scope rationale.
2. **MOVE `ClassifyGraphPlan`** (+ helpers) from `internal/coordclass/classify.go` into a
   NEW `internal/graphstore` package; update Router call sites; grep-sweep.
3. **Provider-aware `ResolveStoreRef`** — re-point the `(storeRef,id)` resolver
   (`internal/dispatch/runtime.go`) onto the existing `internal/storeref`; extend the prefix
   switch (`gcm-/gco-/gcn-/gcs-/gcg-` → class, `gc-/ga-` → work).
4. **Rewire BOTH graph read paths to `GraphStore` BEFORE deleting federation** (load-bearing,
   done under the net): the `?type=molecule` augment + `ReadyGraphOnly` hot loop
   (`internal/api/huma_handlers_beads.go`) and the order gate `storesForGate`
   (`cmd/gc/order_dispatch.go`). Gate: split-topology `is_blocked` conformance.
5. **Delete `coordrouter` + `coordclass`** (irreversible): confirm zero non-test callers of
   create-classify / by-id-probe / read-federation + the sole `coordrouter.New`
   (`api_state.go`). Fold graph into `resolveClassStore`; retire `graphStoreSQLiteEnabled`;
   **add a `postgres` branch to `registerGraphStoreBackend`** (today it registers SQLite only).
   **Then relax the Step-0 guard** (`internal/config/validate_beads_classes.go`) to allow
   `graph=postgres`.

> NEVER collapse Steps 4 and 5. Step 4 is the behavioral pivot under federation; Step 5
> removes the net. After Step 5, graph routes by class and ID-preserving migration no longer
> breaks prefix resolution.

## 3. Phase B — SESSIONS P5 (irreversible live-adopt; build + conformance only, do not run blind)

Full mechanics: `NEXT-SESSION.md §6` and `DESIGN.md §6`. Sessions is the one class where a
botched flip loses RUNNING-AGENT CONTINUITY (open-session metadata long-tail is
persisted-only; crash-adoption can't rebuild it).

**(a) Offline-buildable + verifiable:**
1. Extract a net-new `SessionStore` seam in `internal/session/` (`var _ SessionStore =
   beads.Store(nil)`); factor the PURE persisted subset of `infoFromBead`
   (`manager.go:1564-1587`) into `infoFromPersisted(b)` — `infoFromBead` stays IMPURE in
   Manager (reads `m.sp`, mutates the ACP router).
2. Inject at the SINGLE chokepoint `internal/api/session_manager.go:11`:
   `resolveSessionStore(store, cfg, cityPath, rec)` → `resolveClassStore(...,
   config.BeadClassSessions, rec)`. Default `bd` returns the work store (byte-identical, all
   17 call sites unchanged). **NB: `internal/api` now has the `NudgesBeadStore()` precedent
   (this session) — add a sibling `SessionsBeadStore()` to `api.State` the same way if the
   API constructs managers, and audit ALL `internal/api` session-bead reads/writes (not just
   the manager chokepoint) for the same cross-surface miss that bit the nudge withdraw.**
3. SQLite+Postgres session schema reusing the generic EAV schema — **explicitly NO
   `UNIQUE(session_name)`** (breaks the duplicate-then-elect reconciler,
   `session_beads.go:884-919`). Register the `gcs` prefix.
4. **Projection-invariance conformance (NEW, load-bearing):** seed an identical session
   corpus into bd/SQLite/PG; assert `ProjectLifecycle(input)` (`lifecycle_projection.go:374`,
   PURE) and `infoFromPersisted(b)` are EQUAL across all three. This is the zero-behavior-change
   proof.

**(b) Live-adopt atomic-copy COMMAND — build + conformance-test, do NOT run blind:**
`storemigrate.Migrate` is an OFFLINE bulk copier (no lock/quiesce/projection-equality) — NOT
sufficient for OPEN sessions. Build `gc beads adopt sessions`: controller-lock/quiesce →
snapshot OPEN session+wait corpus → ID-preserving copy → **post-copy projection-equality gate
(ABORT on mismatch)** → flip read path only on clean gate → leave bd rows intact. Config-gate
it. **HIGH / IRREVERSIBLE.** For the maintainer-city cutover (§5) this is the command that
moves the LIVE open sessions; closed/historical sessions go via the plain `gc beads migrate`.

## 4. Phase C — TIER FIX (migration prerequisite, from the review)

The relocated class stores open RAW (`openClassSQLiteStore`/`openClassPostgresStore`, no
`bead_policy_store` wrapper), so infra-bead writes land on the **`main`/issues tier** instead
of **no-history**. At bd, tier+label doubly guard against Ready leak; on the relocated store
the `gc:nudge`/session/order label exclusion is the SOLE guard. Before a real PG cutover:
either make the class store write the correct per-class tier (mirror
`bead_policy_store.defaultBeadStorage`: sessions/waits/nudges/orders → no-history) **or** prove
`IsReadyExcludedBead` + `IsReadyCandidateForTier` cover every relocated `Ready()`/open-scan path
(incl. `ReadyGraphOnly`, the doctor/`/status` censuses if they ever union the relocated store).
Add a test asserting a relocated nudge/session bead is no-history (or relocated `Ready()`
excludes it) so the label guard isn't silently the only defense.

## 5. Phase D — REBASE → BUILD/INSTALL → MIGRATE maintainer-city → FIXUP → bring up

**This phase is DESTRUCTIVE/irreversible — the owner sequences it. Never force-migrate dolt
rig DBs (prior outage).**

### D1. Rebase onto the LIVE branch
- Installed `gc` = `/home/ubuntu/.local/bin/gc → /opt/gascity/current → releases/dev-2a83e20bd`
  = branch **`deploy/sqlite-b36-probe-attribution`** (`2a83e20bd`).
- `plan/decouple-infra-beads` is **139 ahead / 375 BEHIND** it (merge-base `fc1f581ed`); the
  deploy branch carries **no infra-store work**.
- **RE-CONFIRM the live branch first** ("window 3" is whatever worktree is actively moving it;
  it may have advanced past `2a83e20bd`). `git worktree list` + `gc version` + `readlink
  /opt/gascity/current`.
- Strategy decision (high conflict risk on graph/coordrouter, session reconciler, api_state,
  given 375 diverged commits): assess **cherry-pick the 39 initiative commits**
  (`204b66aee..HEAD`, but note 39 is post-finish it'll be more) vs **rebase** vs **merge**.
  Prefer the smallest proven slice; the initiative commits are mostly new files + small
  adapters (per the upstream-alignment rules), which favors cherry-pick/rebase over a fat merge.

### D2. Build + install
- `GOCACHE=$(mktemp -d) go build ./cmd/gc/` then the project's install path to `/opt/gascity`
  (mirror how `dev-2a83e20bd` was built/released; do NOT hand-roll).

### D3. Provision + migrate (maintainer-city, `/data/projects/maintainer-city`)
- maintainer-city today: `[beads] graph_store="sqlite"` + `[dolt]` (project_id b2269d7c, DB
  `ga`). Graph already on SQLite (`.gc/beads.sqlite`), work on Dolt.
- Start/optimize Postgres (see no_history note below). `gc beads postgres init` (creates DB +
  provisions schema-per-class). Set `[beads.classes.{sessions,graph,nudges,messaging,orders}].backend
  = "postgres"` (graph only AFTER P6 Step 5 relaxes the guard).
- `gc beads postgres migrate` (= `storemigrate.Migrate` dolt→pg, **ID-PRESERVING + idempotent**,
  verified by `TestBeadsMigrate_ToPostgres`). For OPEN sessions use the §3(b) `gc beads adopt
  sessions` live-adopt path, not the blind bulk copy.

### D4. Fixup bead references (if necessary)
- ID-preserving + class-routing (`resolveClassStore`) means **refs-by-id generally need NO
  fixup** for sessions/mail/orders/nudges (consumers query the class store, not by prefix).
- **AUDIT the real risk areas before/after migrate:** (1) graph prefix/`coordrouter` resolution
  — resolved by P6 Step 5 folding graph into class-routing; (2) **cross-store deps** (a Dolt
  work bead depending on a moved infra bead, or vice versa — deps are per-store and cannot
  span stores); (3) the `(storeRef,id)` resolver. Repair any dangling cross-store dep/ref.

### D5. Bring the city back up + verify
- `gc start` (or the maintainer-city supervisor restart). Verify: controller reconciles;
  sessions adopt with continuity (no lost open-session metadata); nudge dispatch + wait
  satisfy work end-to-end; orders gate; Ready shows only true work; `/status` + dashboard
  sane. Watch for the deferred review items (§6) surfacing live.

## 6. Review's deferred findings (this session) — carry into the migration

- **MUST (migration prereq):** the **tier divergence** (Phase C above).
- **SHOULD (relocated-backend observability):** doctor `backlog-depth` + HTTP `/status`
  work-count + `storehealth` row gauge read only the work store → under-count relocated infra
  beads. `/status` has a dashboard consumer — union the relocated class stores into the census
  (guard on handle-inequality so default stays byte-identical), or confirm those beads were
  never in the bd Open count.
- **VERIFY (Postgres-specific):** read-after-write visibility across the controller's cached
  noClose PG handle vs CLI fresh connections was NOT independently exercised — add a test that
  a controller-terminalized shadow is immediately visible to a fresh CLI connection.
- **OBSERVABILITY:** API `bd list --label gc:nudge` / `bd ready` return empty at relocated
  backends (federation omits the relocated stores) — a CLI/API inconsistency, not a dispatch bug.
- **CLEARED (no action):** the awake/`hasAssignedWork` precedent analog does NOT read nudge
  beads; molecule/convoy autoclose self-protect via Get-miss; order_dispatch/bead_policy_store
  are pure per-bead classifiers; no event-ghost (gcn prefix + `ownsBeadID` firewall).

## 7. no_history / Postgres-optimization (answered)

Not a hard PG constraint — the schema has a `tier` column and holds either tier. The relocated
classes are no-history-BY-POLICY at bd (`bead_policy_store.defaultBeadStorage`, configurable via
`[beads.policies.*].storage`); they move to PG because they don't need Dolt versioning. The bead
`tier` still governs Ready inclusion (Phase C). **Workflow-optimized Postgres:** UNLOGGED tables
for the ephemeral no-history shadow classes (nudges — the flock queue FILE is authoritative, so
crash-droppable, skips WAL, fast for create→terminalize→sweep churn); LOGGED/durable for
sessions (persisted-only state crash-adoption can't rebuild); aggressive autovacuum for churn;
schema-per-class isolation already built.

## 8. Constraints (carry into every commit)

- Commit with `git commit --no-verify` (the worktree's `core.hooksPath` is the main checkout's
  stale hook). gascity Dolt is LOCAL-ONLY (never `bd dolt push/pull/remote`). Never
  `tmux kill-server`. Never `go clean -cache` (`-testcache` ok); cold build `GOCACHE=$(mktemp -d)`.
- Disposable PG for tests (`gc-pg` on :55460; **`billing-pg-gb` on :55455 is someone else's**):
  `docker run -d --rm --name gc-pg -e POSTGRES_PASSWORD=test -e POSTGRES_DB=gascity_internal -p 55460:5432 postgres:16`;
  `GC_TEST_POSTGRES_DSN=postgres://postgres:test@127.0.0.1:55460/gascity_internal?sslmode=disable`.
- Verify shards: `make test-cmd-gc-process-parallel`, `make test-fast-parallel`,
  `make test-integration-shards-parallel`. PG-gated suites skip without the DSN.
- Byte-identical at the DEFAULT (`bd`) backend is the bar for every step until the explicit,
  config-gated, owner-sequenced cutover.
