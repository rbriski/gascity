# Graph-Store-Split Fix — Session Handoff (Groups D–H)

**Purpose:** continue landing the graph-store-split remediation. Groups **A, B, C
are done and committed**; **D, E, F, G, H remain**. This doc is the single
source of truth for the next session.

**Plan of record:** `engdocs/contributors/graph-store-split-audit.md` (73 confirmed
findings, deduped into 15 fix groups A–O). Read it. This handoff layers the
*current state*, the *doctrine correction*, and the *process* on top of it.

---

## 0. The one thing that must not be forgotten (DOCTRINE)

> **Formula (ClassGraph) work beads must ALWAYS go to the specified graph store,
> never Dolt. This is a WRITE-PATH invariant, not something to tolerate on the
> read side.** — project owner, 2026-07-03

The audit's original framing for the *demand* group (C/§5) was to **union** the
Dolt work-leg into controller demand so Dolt-resident formula beads still get
picked up. **That was wrong** and was reverted: a fable red-team proved it
creates a **wake-without-claim livelock** (the demand oracle becomes a superset
of the claim oracle → the pool wakes for work the worker can never claim →
wake → empty-probe → sleep → repeat forever). The correct fix was write-path
(Group C below): make formula beads land in the graph store or **fail loud**,
never silently fall back to Dolt.

**Consequence for D–H:** several audit findings assume Dolt-resident formula
work is a thing to *tolerate*. Before implementing each remaining group,
**re-validate it against the doctrine**: does the problem disappear once formula
beads are always in the graph store, or is it real regardless of where they
live? (Notes per group in §4.)

---

## 1. Where the work is

- **Working tree / branch:** `/data/projects/gascity/.claude/worktrees/beads`
  on `deploy/sqlite-b36-probe-attribution` (the graph-store integration branch —
  it contains `a7f7b2bcd`, `08cdd75f3`, `2a83e20bd`, `df3f274ce`, beads
  `v1.1.0-rc.1`). **This branch does NOT contain `87a788381` (fix-a)** — the
  audit was run against a checkout that had it, so **re-locate every symbol/line
  yourself; do not trust audit line numbers.**
- **beads repo:** `/data/projects/beads` (Group H, part of E).
- **workflows repo:** `/data/projects/workflows` (Group G).
- **Shared build cache:** `export GOCACHE=/data/tmp/tmp.XgAejbMpvc` — reuse it,
  **NEVER `go clean -cache`** (corrupts the fleet cache; project hard-ban).
- `gcg-` graph beads are invisible to `gc bd show` (Dolt path). Live graph API:
  `http://127.0.0.1:8372/v0/city/maintainer-city/beads/graph/{rootID}`.

## 2. What's committed (A, B, C — all local, NOT pushed/deployed)

| Group | Commit | Essence |
|---|---|---|
| A | `0195f407e` | `beadPolicyStore.ListGraphOnlyHandle` forwarder (mirrors the Ready one, applies `expandPolicyReadTier`) + compile-time provider assertions. Reactivates 4 shipped-but-dead fixes (08cdd75f3 orphan heal, 2a83e20bd List half, df3f274ce liveListForRoot, the reconciler drain guard). + drain-guard rider in `graphOnlyHasAwakeAssignedWork` (federated in-progress fallback when List absent) + `sessionHasOpenAssignedWorkForReachableStore` gated on `GraphIDPrefix() != ""` so a degraded identity-phase Router falls back to the rig fan-out. |
| B | `ec586c953` | `remapGraphResidentAssignedWorkStoreRefs` in `assigned_work_scope.go`, called at the **single** production consumer of collected storeRefs (`build_desired_state.go`, right after `collectAssignedWorkBeadsWithStores`). Retags city-tagged (`""`) graph beads to their routed rig so rig workers wake for their graph work. Guards: unresolvable route → `""`; direct session bind (`gc.session_id`) → `""` (owner scope governs, never the workflow root's rig). |
| C | `adc789f9a` | **The doctrine fix.** `registerGraphStoreBackend` registers a **self-healing `lazyGraphStore`** on `OpenSQLiteStore` failure (re-checks the handle cache to kill the open race, re-attempts on use with a 2s backoff, caches on heal; while unhealed all graph ops ERROR — writes fail loud, reads error into the reconciler's fail-safe branches). `openStoreResultAtForCity` stops swallowing `loadCityConfig`'s error — minimal raw `[beads]` parse decides; fails only when `graph_store` is set or `city.toml` is unparseable, else keeps nil-cfg tolerance so default-Dolt cities aren't bricked. |

**Red-team caught real defects in B and C that were fixed before commit** — do
not skip the red-team.

### Load-bearing facts the red-teams established (reuse, don't re-derive)
- **Erroring graph READS are fail-safe** in the reconciler: every decision site
  (drain-ack close, drain-cancel, max-age, pool-freeable) treats a graph-read
  error as `hasWork=true` → keeps sessions alive. **The empty-on-reads variant
  is DANGEROUS** (returns `(false,nil)` → drains workers mid-step). If any
  degraded-graph-store code path is touched, keep erroring reads, not empty.
- `mergeReadyRowsByID(primary, secondary)` emits **secondary first** (dedup).
- Default Dolt cities must stay **byte-identical** — every fix is gated on the
  graph capability / `graph_store=sqlite`, and there are existing default-city
  tests that enforce it.

## 3. Process (follow it for every group)

1. **fable design** (a `general-purpose` agent, `model: 'fable'`, background):
   investigate on *this* branch, pinpoint symbols/lines, produce a TDD-ready
   plan. Include the doctrine re-validation.
2. **opus implement, TDD** (you, or a `model: 'opus'` subagent for big/isolated
   files): write the failing test first, watch it fail, implement, green.
3. **fable red-team** (a `Workflow` with 2–3 fable lenses — correctness,
   security/safety/blast-radius, test-adequacy — then a synth verdict). Fix
   every MUST-FIX before commit. The red-team has caught a real regression in
   *every* non-trivial group so far.
4. **Commit locally** with `git commit --no-verify` (the shared pre-commit hook
   has a stale absolute `core.hooksPath` that fails; gates are run manually).
   End the message with:
   `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`
5. **Do NOT push or deploy** without explicit owner go-ahead. gascity Dolt is
   local-only (git only; never `bd dolt push/pull/remote`).

Gates before "done": `go build ./cmd/gc/`, `go vet ./...` (touched pkgs),
targeted `go test -run ...`, `-race` on new concurrent code.

## 4. Remaining groups (re-validate against the doctrine first)

Suggested order: **F, D** (self-contained, doctrine-independent) → **G, H**
(separate repos, independent) → **E** (largest, cross-repo, owner-gated).
Consider landing **O.4** (the two-store integration harness — see the audit §2
"the one test") alongside F/D to pin the whole cluster end-to-end.

### Group D — HIGH — Router federation lies about completeness *(doctrine-independent)*
`internal/coordrouter/router_federation.go` `federateRead` (~:246) and `DepList`
(~:221) skip a failing leg and return the survivor's rows with **nil error**.
Fix: when `lastErr != nil && merged != nil`, return merged rows wrapped in
`beads.PartialResultError` (the demand collectors already handle
`IsPartialResult` — keep rows AND set partial). Apply to both. **Interacts with
Group C:** the healed-vs-unhealed `lazyGraphStore` now makes a broken graph leg
error, so this partial-result plumbing is what lets reads degrade cleanly.

### Group F — HIGH — orphan reclaim gated on the wrong store's health *(doctrine-independent)*
`cmd/gc/pool_session_name.go` `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete`
(~:83): one global `StoreQueryPartial` bool disables ALL orphan release when any
rig Dolt leg flaps, including `gcg-` beads whose own store was complete. Fix:
have `collectAssignedWorkBeadsWithStores` return `partialByStoreRef`; skip only
beads whose owning source scope was partial; `gcg-` beads gate on city/graph
health alone.

### Group G — HIGH — workflows pack rig `bd` reads blind to graph beads *(doctrine-independent)*
`/data/projects/workflows/scripts/pr_merge.py` (`review_loop_done` ~:729,
`recovery_approval_gates_done` ~:753, `recover_source_from_finalizer` ~:313,
`cleanup_superseded_review_workflow` ~:1252, `MERGE_READY_HANDOFF_SKIP_CODES`
~:64) use `gc --rig bd list/show` = pure Dolt passthrough, so `gcg-` molecule
roots/steps return empty forever → auto-approved merge-ready PRs silently
skipped every 5-min patrol tick. Fix: one shared
`graph_children_for_root(city, rig, root_id)` helper reading `gcg-` roots via the
graph route (`GET /v0/city/<city>/beads/graph/<rootID>`) with a `bd` fallback;
split `source_confirmation_failed` out of the skip codes. **NOTE:** a related
patrol fix `922fb57f7` (`pr_merge.py` `MERGE_READY_HANDOFF_SKIP_CODES`) exists
on branch `fix/adopt-pr-root-id-from-convoy` but was **never pushed** (shared
remote gastownhall/workflows) — reconcile it here. Pushing to that shared remote
IS an outward action → get owner go-ahead.

### Group H — HIGH — bd proxied-server config skips custom_types sync *(fully independent)*
`/data/projects/beads/cmd/bd/config_proxied_server.go` (~:33) → `SetConfig`
updates only the `types.custom` string; the table is never re-synced, so
`invalid issue type: session` persists forever while `doctor` reports OK. Fix:
mirror `DoltStore.SetConfig` — call `issueops.SyncCustomTypesTable` /
`SyncCustomStatusesTable` in the same UOW. This is **the durable fix** for the
session-type regression (previously only live-DB-backfilled). Separate repo →
separate tag → gascity `go.mod` bump → `gc` rebuild if you want it in the binary.

### Group E — HIGH — cross-leg dependency edges *(LARGEST; cross-repo; OWNER-GATED)*
`internal/molecule/molecule.go` (ExternalDeps embed ~:555/:836, Attach ~:303) +
beads repo (`internal/storage/issueops/blocked_state.go`,
`internal/storage/domain/db/dependency.go`, `cmd/bd/doctor/…validation.go`,
`issueops/dependencies.go`). A `gcg-`→work edge on the SQLite leg never releases
(blocked forever); the reverse work→`gcg-` gate is silently inert on Dolt; `bd
doctor --fix` **deletes** cross-class edges; split cycles undetectable. Fix (one
design, three landings): partition ExternalDeps by `GraphIDPrefix()`; cross-leg
blockers become a graph-resident **proxy/gate bead** the controller releases on
blocker closure (not a raw cross-leg row); beads-side treat unresolvable
foreign-prefix targets as blocking + carve them out of doctor reclaim; cross-store
cycle check at the Router boundary. **Two open owner decisions** (audit §5):
proxy-gate bead vs teaching SQLite `Ready` to resolve cross-leg targets; and the
`#27` default-build-vs-`gascity_native_beads` semantic disagreement. **Coordinate
the gascity + beads halves in one arc** so the doctor carve-out lands no later
than the edge-writing change. Re-validate hard against the doctrine — with
formula beads always graph-resident, the exact set of cross-leg edges that occur
may narrow.

## 5. Deferred / owner-decision items (don't silently "fix")
- **L3 (Group C tail):** `privatizeAttachedRootOnlyWisp` (`internal/sling/sling.go`
  ~:1486) strips `gc.kind=wisp` on attached **v1** root-only wisps → ClassWork →
  Dolt. v2 immune. Moving it relocates live beads → owner call.
- **Group E** proxy-bead vs sqlite-Ready and the native-tag semantic — owner call.
- **`GraphIDPrefix()` after lazyGraphStore heal** stays `""` until the next config
  reload (safe, conservative). Optional follow-up: forward the graph capability
  through `lazyGraphStore` (audit them the prefix-gated sites first).

## 6. Open question for the owner (was asked, unanswered)
By *"the store specified"* the working assumption is **the sqlite graph store for
all ClassGraph beads**. If the owner instead means a per-formula/per-agent store
target, some fixes shift — but the Group C fail-loud fix is correct either way.
