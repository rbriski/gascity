# Next-session prompt — finish Track G: retire coordrouter for GRAPH (G2 + G3) + followups

> Paste the block below to launch the agent that finishes the `coordrouter.Router` retirement.
> Branch `plan/decouple-infra-beads` @ `685cafc0e`, worktree
> `/data/projects/gascity/.claude/worktrees/infra-store-plan`. ~51 local commits, UNPUSHED (hold).

---

You are finishing the **`coordrouter.Router` retirement**. The previous session **completed Track S
(sessions are fully off the Router)** and **Track G G0 (the `resolveGraphStore` foundation) + G1 (the
graph-only Ready readers)**. Your job is **Track G G2 + G3** — making graph fully class-aware and then
**deleting `coordrouter`** — plus the documented code followups.

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/TRACK-G-HANDOFF.md` — THE handoff (status, the two landmines,
   the exact G2/G3 plan, file:line anchors). Start here.
2. `engdocs/plans/infra-store-decouple/raw/track-g-design.md` — the verified G2/G3 implementation design.
3. `engdocs/plans/infra-store-decouple/raw/track-g-create-surface.json` + `track-g-byid-surface.json`
   + `retire-router-trackg.json` — the graph create/by-id/read + delete-surface maps.
4. `engdocs/plans/infra-store-decouple/RETIRE-ROUTER-PROGRESS.md` — the live tracker (mark G2/G3 as you go).
5. `engdocs/plans/infra-store-decouple/raw/g1-adjudication-trace.md` — the G1 review's live-bug analysis
   (the federation correction you must not re-break).
6. Recall auto-memory `infra-beads-decoupling-plan.md` (the 2026-06-27 entry = the prior session).

**The method (it caught real LIVE bugs last session — keep it):**
- **Class-aware callers, NOT a dispatcher.** Each graph caller uses the graph store
  (`cr.graphBeadStore()` / `s.state.GraphBeadStore()` / `resolveGraphStore`); the by-id-agnostic case
  uses `internal/storeref`. This is explicitly NOT the rejected Path-B `graphRoutedStore`. If a caller
  genuinely cannot be class-aware, STOP and report — do not silently build a dispatcher.
- **Misclassification is invisible to byte-identity tests.** Run an **adversarial review workflow per
  landmine-prone phase** (the pattern in `raw/retire-router-g1-review.md`). graph=sqlite is LIVE on
  maintainer-city (6 rigs), so bugs ship live — green tests are necessary but NOT sufficient.
- ≤5 files/phase; byte-identical at the default `bd` (graph=bd) backend; `go build ./...` + `go vet` +
  targeted tests green each phase; commit `--no-verify`.

**🛑 The two landmines (LIVE data loss / outage if wrong):**
1. The SQLite graph store is the LEGACY `<cityPath>/.gc/beads.sqlite` (NOT `.gc/graph/`).
   `resolveGraphStore` preserves it (guarded by `cmd/gc/graph_store_resolver_test.go`). Never route
   graph through `.gc/graph/` or the live city's graph data is orphaned.
2. Under graph=sqlite, EVERY store (city + every rig) is `policy(Router(rigWork+sharedGraph))`. The
   old graph-only reads returned the SHARED graph store's Ready/List ALONE (work kept out of the
   worker hot loop). When graphRelocated, read the shared graph store ALONE — never iterate rig work
   stores' `Live.Ready/List` (= `Router.Ready/List` = work∪graph → leaks work into worker readiness).

**Execute, in order (each phase: implement → adversarial review → verify → commit):**

- **G2a — CREATE/apply orphan-chokepoint.** `cmd/gc/bead_policy_store.go:48` (`wrapStoreWithBeadPolicies`)
  must source its graph applier from `resolveGraphStore(workBackend, cfg, cityPath, rec)` (legacy loc)
  instead of `beads.GraphApplyFor(inner)` — else, once the Router is gone, graph plans orphan onto the
  WORK store. Cover the `molecule.go` sequential-fallback `store.Create` path too. Highest stakes —
  review hard.
- **G2b — by-id `[work, graph]`.** `internal/api/handler_beads.go` `beadStoresForID`: add a class-prefix
  arm returning `[work, graph]` for `gcg-` ids (skip when `graph == cityStore`). The bd-shim is pure-HTTP.
- **G2c — List-restore** (the G1 finding): the 3 graph-only LIST sites (session_reconciler ~:2804,
  pool_session_name ~:199, dispatch/runtime ~:441) rely on Router.List federation today; restore graph
  List for them (add a `ListGraphOnly` forwarder to `beadPolicyStore` mirroring `ReadyGraphOnlyHandle`,
  OR make the 3 sites class-aware) BEFORE G3.
- **G3 — delete coordrouter (IRREVERSIBLE; ships LIVE).** `routedPolicyStore` → `policy(workBackend)`
  always; remove the 2 `*coordrouter.Router` assertions in `api_state.go` (caching-store builder; the
  graph leg stays uncached) + `closeBeadStoreHandle`; drop the import. Delete `internal/coordrouter/`;
  retarget `internal/storeref/storeref_test.go` off `Router.Get`; delete `coordclass.ClassifyGraphPlan`.
  **`coordclass` SURVIVES** (storemigrate). Gate: a **relocated-graph conformance test** (graph=sqlite:
  apply/create + by-id close land on `.gc/beads.sqlite`; `resolveGraphStore(...).Ready()` == graph Ready
  excludes work; byte-identical at graph=bd) + the ported Router behavioral tests + whole-tree green +
  adversarial review. Because G3 is irreversible and ships live on a production city, surface it to the
  owner before landing if anything is uncertain.

- **Followups** (additive): two-store wait/extmsg test; doctor/status observability under-count; PG
  read-after-write test (`GC_TEST_POSTGRES_DSN`, disposable `gc-pg` :55460); `cmd_sling.go:1485` stamp.

**Hard constraints:** byte-identical at graph=bd until the owner-gated cutover; commit `--no-verify`
(stale `core.hooksPath`); gascity Dolt is LOCAL-ONLY (never `bd dolt push/pull/remote`); never
`tmux kill-server`; never `go clean -cache` (`-testcache` ok; cold build `GOCACHE=$(mktemp -d) go build
./cmd/gc/`); ~51 local commits, push ONLY on the owner's word. **Do NOT** start the destructive
maintainer-city dolt→pg migration — that's a separate owner-gated track.
