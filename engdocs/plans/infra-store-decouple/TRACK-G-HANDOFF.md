---
title: "Retire the Router — HANDOFF after Track S complete + Track G G0/G1 (graph G2/G3 remain)"
date: 2026-06-27
branch: plan/decouple-infra-beads
head: 685cafc0e
epic: ga-pd6tcg
status: HANDOFF — Track S DONE; Track G G0+G1 DONE; G2 + G3 remain (the irreversible live core)
---

> This hands off the `coordrouter.Router` retirement after a long session that **completed Track S
> (sessions fully off the Router)** and the **foundation + read paths of Track G (graph)**. What
> remains is the irreversible, LIVE-RISKY core of graph: G2 (create-chokepoint + by-id + List-restore)
> and G3 (delete the package). Read this, then `NEXT-SESSION-TRACK-G-PROMPT.md` (the copy-paste launcher).

## 0. TL;DR
- **Sessions: DONE.** `routedPolicyStore` is graph-only; sessions reach their store via class-aware
  accessors (`resolveSessionStore` / `cr.sessionBeadStore()`). The recorder is threaded. All
  reviewed SAFE-TO-PROCEED, byte-identical at the live config (maintainer-city = sessions=bd → inert).
- **Graph: G0 (resolveGraphStore foundation) + G1 (Ready readers) DONE.** G2 + G3 remain.
- **The whole prod coordrouter delete-surface is ONE file: `cmd/gc/api_state.go`.** The Router now
  serves ONLY graph. `internal/storeref` (PrefixOwner+Resolve) is ready and conformance-pinned.
- ~51 commits UNPUSHED (owner-gated). Live city UNTOUCHED. The destructive maintainer-city dolt→pg
  migration is a SEPARATE owner-gated track (not started).

## 1. Authoritative docs (read in order)
1. `RETIRE-ROUTER-PROGRESS.md` — the live phase-by-phase tracker (every commit hash + review id).
2. `raw/track-g-design.md` — the verified G2/G3 implementation design (355 lines, file:line anchors).
3. `raw/track-g-create-surface.json` + `raw/track-g-byid-surface.json` — the graph create/apply and
   by-id caller maps.
4. `raw/retire-router-trackg.json` — the graph read-path + coordrouter delete-surface map.
5. `raw/retire-router-g1-review.md` + `raw/g1-adjudication-trace.md` — the G1 review (2 live blockers
   it caught + the federation correction).
6. Auto-memory `infra-beads-decoupling-plan.md` — binding decisions; the 2026-06-27 entry = this session.

## 2. The method that worked (keep using it)
- **Class-aware callers, NOT a dispatcher.** Each caller uses the typed store for its class. The
  by-id-agnostic case (worker `bd close gcg-N`) uses `internal/storeref` (prefix→store), not a
  stateful Router. This is explicitly NOT the rejected Path-B `graphRoutedStore`. If a caller
  genuinely cannot be made class-aware, STOP and report — do not silently build a dispatcher.
- **Convention (locked):** two-store functions take `(sessionStore/graphStore, workStore, ...)` —
  the class store FIRST, work SECOND. Pure-class helpers keep a single param (callers pass the class
  store). Pure-work functions are UNTOUCHED.
- **Misclassification is INVISIBLE to byte-identity tests** (at the default backend the class store
  == the work store). So **run an adversarial review workflow per landmine-prone phase** — green
  tests alone do NOT prove the class/work split. The review caught real LIVE bugs in G1 that the
  implementer + green tests missed. This is non-negotiable for the remaining graph phases (graph is
  LIVE-relocated, so bugs ship live).
- Per phase: ≤5 files, byte-identical at the default (graph=bd) backend, `go build ./...` + `go vet`
  + targeted tests green, commit `--no-verify` (stale `core.hooksPath`).

## 3. 🛑 The two Track-G landmines (do not get these wrong — they cause LIVE data loss / outages)
1. **Data-orphan / legacy location.** The SQLite graph store is at the LEGACY
   `<cityPath>/.gc/beads.sqlite` (`filepath.Join(cityPath, citylayout.RuntimeRoot)`), NOT the
   class-store `.gc/graph/` path. `resolveGraphStore` (G0) preserves this; NEVER route graph through
   `openClassSQLiteStore`/`.gc/graph/` or the live graph_store=sqlite city is pointed at an empty
   store and its graph data is orphaned. Guarded by `cmd/gc/graph_store_resolver_test.go`.
2. **graph=sqlite is LIVE on maintainer-city (6 rigs).** Under it, EVERY store (city + every rig) is
   `policy(coordrouter.Router(rigWork + sharedGraph))`. The OLD graph-only reads probed
   `GraphOnlyReadyFor(store)` on every leg → the SHARED graph store's Ready ALONE (work kept out of
   the worker hot loop). Any rewrite that calls `HandlesFor(store).Live.Ready()` on a routed store
   gets `Router.Ready` = work∪graph and LEAKS work into worker readiness (this was the G1 blocker).
   Rule: when graphRelocated, read the shared graph store's Ready/List ALONE; do not iterate rig
   work stores. At graph=bd, the work federation is unchanged.

## 4. What's DONE (this session, on top of `2ea60d7a9`)
### Track S — sessions off the Router (P1–P7, all committed + reviewed)
S1 `closeBead(sessionStore, workStore,...)` · S2 close-family work-guards · S3a wait/extmsg split ·
S3b-1 retire-named · S3b-2 syncSessionBeads+reapers (session_beads.go done) · S4a wake/reconcile/sleep
rename · S4b lifecycle start path · S4c the reconciler (34 work sites) · S5 build_desired_state
(`bp.sessionStore`) · S6a controller entry-point activation (`cr.sessionBeadStore()`) · S6b-1/2 CLI +
gracefulStopAll + W1 + stop-path + runAdoptionBarrier · S7 unregister ClassSessions
(`routedPolicyStore` graph-only) + rewrite `session_router_test.go`. Reviews: wf_f745a537, wf_63a72547,
wf_6f2e3102, wf_0a636cc1, wf_3a185f81, wf_7366ba79 — all SAFE-TO-PROCEED.

### Track G — graph foundation + reads
- **G0** (`4d7641378`): `resolveGraphStore` (legacy `.gc/beads.sqlite`) + `openGraphSQLiteStore` +
  `cr.graphBeadStore()` + `api.State.GraphBeadStore()` (+ controllerState + fakeState). Additive,
  byte-identical. Landmine test in `cmd/gc/graph_store_resolver_test.go`.
- **G1** (`e3a7b036c`): the 3 graph-only **Ready** readers (`/v0/beads/ready`,
  `liveReadyForControllerDemandQuery`, `gc ready`) are class-aware. Review wf_23e8aef2 caught 2 LIVE
  blockers (work leak) → fixed (graph-only when relocated). New realistic-`BeadStores()` guard tests.

## 5. What REMAINS — G2 then G3 (per `raw/track-g-design.md`)
### G2 — graph CREATE/apply + by-id + List-restore (Router still present as a safety net; byte-identical-ish)
1. **CREATE/apply ORPHAN-CHOKEPOINT.** `cmd/gc/bead_policy_store.go:48` (`wrapStoreWithBeadPolicies`)
   sources its graph applier from `beads.GraphApplyFor(inner)`. Today inner=Router → graph store.
   After G3 (no Router) `GraphApplyFor(work)` → graph plans land on WORK = ORPHAN. Fix: source the
   applier from `resolveGraphStore(workBackend, cfg, cityPath, rec)` (legacy loc). This covers BOTH
   the graph-apply path AND `molecule.go`'s sequential-fallback `store.Create` (graph-apply disabled).
   Callers in the create-surface map: `molecule.Instantiate`/`InstantiateFragment`, `sling.go:1287`,
   `order_dispatch` `dispatchWisp`.
2. **by-id gcg-N.** `internal/api/handler_beads.go` `beadStoresForID`: add a class-prefix arm
   returning `[work, graph]` for `gcg-` ids; the existing per-store Get-then-mutate loop federates
   exactly like the Router. The bd-shim is pure-HTTP (no change). Guard: at graph=bd skip the arm
   (`graph == cityStore`) so default cities are byte-identical.
3. **List-restore (the G1 finding).** The 3 graph-only LIST sites (session_reconciler.go ~:2804,
   pool_session_name.go ~:199, dispatch/runtime.go ~:441) get graph beads only via Router.List
   federation today (their `GraphOnlyListFor` branch is dead — `beadPolicyStore` forwards
   `ReadyGraphOnlyHandle` but not a `ListGraphOnly` handle). After G3 they'd see work-only.
   **Fix BEFORE G3:** add a `ListGraphOnly` forwarder to `beadPolicyStore` (mirror its
   `ReadyGraphOnlyHandle`) OR make the 3 sites class-aware via `cr.graphBeadStore()`/`storeref`.
Review G2 adversarially (orphan-chokepoint = highest stakes; a wrong location orphans live graph data).

### G3 — delete coordrouter (IRREVERSIBLE; ships LIVE — owner should be aware before this lands)
- `cmd/gc/api_state.go`: `routedPolicyStore` returns `policy(workBackend)` always (drop the
  graphRelocated gate + `coordrouter.New`); registerGraphStoreBackend/SQLite logic already lives in
  `resolveGraphStore`/`openGraphSQLiteStore` (G0). Remove the 2 `*coordrouter.Router` assertions
  (~:199 caching-store builder — graph leg stays uncached; ~:860 closeBeadStoreHandle). Drop the
  coordrouter import.
- Delete `internal/coordrouter/` (router*.go, stores.go, bdgraphstore.go, coordtest). Retarget
  `internal/storeref/storeref_test.go` off `Router.Get` (delete `TestResolve_MatchesRouterGet`; keep
  `TestPrefixOwner`/`TestResolve_FederationFallback`). Delete `coordclass.ClassifyGraphPlan` (only the
  Router calls it). **`coordclass` SURVIVES** (`Classify`/`Classes`/`ClassWork` used by
  `internal/storemigrate`).
- **Relocated-graph CONFORMANCE test** (the gate): graph=sqlite — (a) a graph-plan apply + the
  sequential fallback land on `.gc/beads.sqlite`; (b) by-id close of `gcg-N` lands on the graph store;
  (c) `resolveGraphStore(...).Ready()` == the graph store's Ready and excludes work; (d) byte-identical
  at graph=bd. Plus the existing Router behavioral tests (router_byid/federation/storage/claim) ported
  to a non-Router differential so coverage isn't lost.

## 6. Followups (per /goal "all followups"; non-blocking, additive)
- two-store wait/extmsg test (assert cancelState/reassignState route waits→session, extmsg→work with
  distinct stores — the P3a review's suggestion).
- observability under-count: doctor backlog-depth / HTTP /status / storehealth read only the work
  store → under-count relocated infra beads (union the class stores or confirm).
- PG read-after-write: a controller-terminalized shadow visible to a fresh CLI PG connection
  (needs `GC_TEST_POSTGRES_DSN`; disposable `gc-pg` on :55460 — `billing-pg-gb` :55455 is someone else's).
- `cmd_sling.go:1485` stampLastNudgeDeliveredAt → sessionStore when the sling seam is converted.

## 7. Verify / constraints
- `go build ./...`, `go vet ./...`, `make test-cmd-gc-process-parallel`, `go test ./internal/api/ -count=1`,
  PG-gated conformance with `GC_TEST_POSTGRES_DSN`. cold build `GOCACHE=$(mktemp -d) go build ./cmd/gc/`.
- Commit `--no-verify`. NEVER `go clean -cache` (`-testcache` ok), `tmux kill-server`, `bd dolt push/pull/remote`.
- Push only on the owner's word. Do NOT start the destructive maintainer-city migration (separate track).
