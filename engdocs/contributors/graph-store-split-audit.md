# Graph-Store-Split Remediation Report (plan of record)

Audit scope: `/tmp/deploy-integ` @ `87a788381` (gascity deploy branch), `/data/projects/beads`, `/data/projects/workflows`. 73 confirmed findings, 0 uncertain-as-filed. Deduped into 15 fix groups below.

---

## 1. Executive summary

The `graph_store=sqlite` split moved every graph-resident (`gcg-`) bead into a single city-scope SQLite leg behind a `coordrouter.Router`, but the surrounding code retained three single-store assumptions: **(1)** a bead's physical store equals its executing agent's configured scope (so rig-scoped demand/wake/ownership filters compare a rig ref against the city ref `""` and always miss), **(2)** capability discovery survives store wrapping (it doesn't — `beadPolicyStore` forwards only `ReadyGraphOnlyHandle`, so `beads.GraphOnlyListFor(prod store)` is **ok=false everywhere in production**, making four shipped fixes — 08cdd75f3, 2a83e20bd's List half, df3f274ce, and the reconciler's in-progress drain guard — silently dead code), and **(3)** a federated read that returns rows is complete (Router `federateRead`/`DepList` swallow a failed leg's error, and a dozen API/CLI surfaces either double-count the shared graph leg or read only the Dolt leg via raw `bd`/SQL). The result is one systemic failure class — graph-resident work invisible to demand, wake, claim-protection, orphan-reclaim, dependency-release, and projection paths — expressed at ~40 distinct sites, three of which have already caused live maintainer-city incidents.

## 2. Why all tests pass

Every graph-capability test exercises the **wrong store shape**:

- **False-green fakes implementing the dropped capability directly:** `graphOnlyAssignedStore.ListGraphOnlyHandle` (`cmd/gc/assigned_work_scope_test.go:631/637`), `graphFederatingStore.ListGraphOnlyHandle` (`pool_session_name_test.go:2218`), `graphDemandStore` with a query-blind canned `ReadyGraphOnlyHandle` (`build_desired_state_graph_ready_test.go:42`), and the fully-stubbed `hookClaimOps` closures (`cmd_hook_claim_graph_verify_test.go:22`). Production never sees these types — it sees `wrapStoreWithBeadPolicies(Router)`, on which the capability is dropped. Empirically verified: wrapping `graphFederatingStore` in the policy wrapper makes the 08cdd75f3 regression test **fail today**.
- **Bare-Router tests bypassing the wrapper:** `runtime_listgraphonly_test.go:49`, `dispatch_router_sqlite_integration_test.go`, `bead_http_graph_store_test.go:44–275` (all set `state.cityBeadStore = router` and `state.stores = nil`), `router_claim_test.go`.
- **Missing harness:** the only deployed-city two-store tests (`test/integration/graph_store_sqlite_{convergence,dispatch}_test.go`) use `provider="file"`, no `[[rig]]`, no pool, a 1–2 bead backlog, and a worker script that *self-documents* avoiding `gc hook --claim`/`bd mol|gate` — structurally unable to reach the demand-wake, wake-window-eviction, warm-pool probe, claim, or orphan-release paths that produced all three live incidents.

**The one test:** `TestGraphStoreSQLitePoolDemandOrphanClaimClose` (finding #72) — a deployed city with `[beads] provider=file graph_store="sqlite"`, one `[[rig]]`, a `min_active_sessions=0` pool, ~20 work-leg beads + 1 routed-unassigned `gcg-` bead; assert the pool wakes, claim via `gc hook --claim --json`, kill the session, assert the bead reopens within a patrol interval, then assert root convergence. This single test traverses the a7f7b2bcd, 87a788381, 08cdd75f3 incident paths **through the production policy-wrapped store**, and would have caught the entire cluster. Its cheap unit-level sentinel is the capability-parity test in Group A below.

## 3. Confirmed gaps, ranked and grouped by shared fix

### Group A — HIGH — `beadPolicyStore` drops `ListGraphOnlyHandle` (the root capability bug)
*Findings: #9, #21, #33, #66; consumers #22, #34, #35, #68; #10, #12, #25, #30, #36, #37, #70 inherit.*

**Sites:** `cmd/gc/bead_policy_store.go:108` (only `ReadyGraphOnlyHandle` forwarded); dead consumers: `internal/dispatch/runtime.go:441 liveListForRoot`, `cmd/gc/session_reconciler.go:2738 sessionHasOpenAssignedWorkForReachableStore`, `:2843 graphOnlyHasAwakeAssignedWork`, `cmd/gc/pool_session_name.go:198` (the 08cdd75f3 orphan heal), `internal/dispatch/control.go:565`, `internal/molecule/molecule.go:326/414/1360`.

**Failure mode:** `GraphOnlyListFor(prod store)=false` at every production call site (empirically proven with a probe test). Consequences: the 08cdd75f3 gcg-orphan heal is fully inert (strands never released); `:2738` falls back to rig-only reach so sessions holding open graph work are closed/recycled (the 12a0d09b6 strand re-introduced); `:2843` is worst — Ready **is** forwarded so the graph-only branch is entered, then the in_progress leg is silently skipped, letting the reconciler drain a worker mid-step on a claimed `gcg-` bead; every dispatcher root-scoped scope-check forks `bd` into Dolt per tick (the gc-hook 15s-timeout class) and couples wholly-sqlite molecules to Dolt outages.

**Single fix:** add `func (s *beadPolicyStore) ListGraphOnlyHandle() (beads.GraphOnlyListStore, bool)` mirroring `ReadyGraphOnlyHandle` — probe `beads.GraphOnlyListFor(s.Store)`, wrap with `expandPolicyReadTier(query)` and pass-through `GraphIDPrefix()`. Hardening rider: in `graphOnlyHasAwakeAssignedWork`, when Ready is present but List absent, fall back to a federated `List{Assignees, Status:in_progress}` instead of skipping the leg. Guard test: assert `GraphOnlyReadyFor` **and** `GraphOnlyListFor` are both true on `wrapStoreWithBeadPolicies(coordrouter.New(mem)+graph backend)` — table-driven over all optional capabilities (Claimer, GraphApplyFor, Counter too, per #73).

### Group B — HIGH — graph-resident beads collected with storeRef `""` fail every rig-scoped reachability gate
*Findings: #2, #3, #8, #23, #24.*

**Sites:** `cmd/gc/assigned_work_scope.go:82 assignedWorkIndexReachableFromAgent`, `:226–228 filterAssignedWorkBeadsForSessionWake`, `cmd/gc/pool_session_name.go:303 openSessionOwnsWork` (via `makeOpenSessionStoreRefIndex`), `cmd/gc/build_desired_state.go:858` (namedWorkReady gate), `:773` (pool demand).

**Failure mode:** `collectAssignedWorkBeadsWithStores` (`build_desired_state.go:1107`) registers the city store with ref `""`; every claimed/assigned `gcg-` bead therefore carries `""`, while rig-scoped agents index under their rig name. Result: an asleep rig worker owning an in_progress `gcg-` step gets **no wake demand, no pool demand, no orphan release** (`liveOpenSessionAssignmentExists` sees the open session bead and skips) — a stable deadlock, the assigned-work sibling of a7f7b2bcd. Also degrades claim-protection to per-tick live queries and (under alias + label-loss) permits release under a live owner (#8).

**Single fix:** at collection time (`appendWorkUnique`), remap the storeRef of graph-prefixed beads from `gc.root_store_ref`/`gc.routed_to` (`"rig:NAME"` → `"NAME"`), mirroring the ownerStore remap in `pool_session_name.go:198`. One change heals all five gates.

### Group C — HIGH — routed demand: custom scale_check cold gate + graph-only replace-not-union
*Findings: #1; #5, #18, #19.*

**Sites & failure modes (two related demand losses, three fixes):**
- `build_desired_state.go:660/610` (#1): warm custom-`scale_check` pools never get the fix-a city-store graph probe (`cityStoreProbeForRigPool` was wired only into the `!hasCustomScaleCheck` branches at :603/:655). A ready routed `gcg-` bead strands while the bd-based custom check structurally can't see it — 87a788381 re-introduced for the custom-check variant. **Fix:** change the gate to `isCold || GraphOnlyReadyFor(store).ok`, append the city target with `coldWakeTemplates[template]=true` (clamped to 1, cannot override the authoritative custom count).
- `build_desired_state.go:1748/1803 readyForControllerDemand` (#5): when graph-capable, the graph-only slice **replaces** the federated ready read, so plain ClassWork beads with `gc.routed_to` (or assigned to on_demand named sessions) become invisible to controller demand. **Fix:** union `ReadyGraphOnly` with the work-leg ready via the existing `mergeReadyRowsByID` — or formally reject plain routed work-leg beads at creation and document the doctrine.
- `internal/config/config.go:3573/3733` (#18) + `:3564` (#19): `rewriteReadyOracle`'s blanket `bd ready`→`gc ready` swap flips the Tier-2 *assigned-ready* probe onto the graph-only path (pre-assigned Dolt work beads stranded on a hook nobody can see), and Tier-1 crash-recovery has **no** in_progress probe for durable (history-tier) graph steps, so a respawned session can't resume its own claim. **Fix:** scope the token swap to the pool-demand predicates and add a `gc ready --assignee` probe *beside* `bd ready --assignee` (union); add a graph in_progress mode to Tier-1 under `graph_store=sqlite`.

### Group D — HIGH — Router federation lies about completeness
*Findings: #4; contributes to #29.*

**Sites:** `internal/coordrouter/router_federation.go:246 federateRead`, `:221–237 DepList`.

**Failure mode:** a failing leg is skipped and the survivor's rows return with **nil error** (and rows accompanying a leg's `PartialResultError` are discarded). A locked sqlite leg + healthy Dolt makes every in_progress `gcg-` bead vanish from a "complete" read: `storePartial` never set, the drain-suppression fail-safe (`build_desired_state.go:689`) never engages, sessions holding live graph work become drainable, orphan-release input truncates; the mirror Dolt-down case never reaches the `:1732` live fallback. Also defeats `ListSubtree`'s walk-error fail-safe so autoclose can close a split root over a transiently missing graph child (#29).

**Single fix:** when `lastErr != nil && merged != nil`, return the merged rows wrapped in `beads.PartialResultError` — the demand collectors already handle `IsPartialResult` correctly (keep rows AND set the partial flag). Apply to `federateRead` and `DepList`.

### Group E — HIGH — cross-leg dependency edges: written blind, never release, reclaimed as garbage
*Findings: #26, #27, #57, #58, #59, #60.*

**Sites:** `internal/molecule/molecule.go:555/836` (ExternalDeps embed), `:303 Attach`; beads repo: `internal/storage/issueops/blocked_state.go:141+` (is_blocked ignores `depends_on_external`), `internal/storage/domain/db/dependency.go:105` (proxied Insert, no classification/validation), `cmd/bd/doctor/fix/validation.go:43` + `doctor/validation.go:61` (orphan reclaim deletes cross-store edges), `issueops/dependencies.go:181` (cycle check local-only; sqlite `DepAdd` has none).

**Failure mode(s):** a `gcg-`→work edge on the SQLite leg never releases (SQLite `Ready` COALESCEs a missing blocker to open → drain-item workflows blocked **forever**, drain tally stalls permanently, #26); the reverse work→`gcg-` gate is **silently inert** on the Dolt leg (external targets never block → gated steps dispatch early, voiding attach/formula ordering, #27/#57/#59); `bd doctor --fix` **deletes** the Router-pinned cross-class edges wholesale (#58); split cycles are undetectable (#60).

**Fix (one design, three landings):** (a) at instantiation/attach, partition ExternalDeps/edges by `GraphIDPrefix()` — same-leg embeds as today; cross-leg blockers become a graph-resident proxy/gate bead the controller releases on blocker closure (the projected-root pattern drains already use), never a raw cross-leg row; (b) beads side: for blocking dep types, treat unresolvable targets whose prefix is a configured same-city foreign prefix as **blocking**, and extend the doctor's `external:` carve-out to non-local prefixes (minimally: never reclaim `depends_on_external` rows or foreign-prefix issue-id rows); (c) cross-store cycle check at the Router boundary (federated DepList before DepAdd).

### Group F — HIGH — orphan reclaim gated on the wrong store's health
*Finding: #7.*

**Site:** `cmd/gc/pool_session_name.go:94 releaseOrphanedPoolAssignmentsWhenSnapshotsComplete`.

**Failure mode:** one global `StoreQueryPartial` bool — any flaky rig Dolt leg (the chronic bd/dolt EOF class) disables **all** orphan release, including for `gcg-` beads whose own store and session snapshot were complete. Molecules stall as long as the Dolt leg flaps.

**Fix:** have `collectAssignedWorkBeadsWithStores` return `partialByStoreRef` (per-goroutine errs already keyed by source ref); keep `SessionQueryPartial` global, but skip only beads whose owning source scope was partial — `gcg-` beads gate on city/graph health alone.

### Group G — HIGH — workflows pack: rig `bd` reads structurally blind to graph beads
*Findings: #62, #63 (HIGH); #61, #64, #65 (MEDIUM).*

**Sites:** `/data/projects/workflows/scripts/pr_merge.py:729 review_loop_done`, `:753 recovery_approval_gates_done`, `:313 recover_source_from_finalizer`, `:1252 cleanup_superseded_review_workflow`, `:64 MERGE_READY_HANDOFF_SKIP_CODES`.

**Failure mode:** all use unrouted `gc --rig bd list/show` (pure passthrough to the rig Dolt store); molecule step/root beads are `gcg-` SQLite residents, so recovery gates return empty **forever** → auto-approved merge-ready PRs are silently skipped every 5-minute patrol tick with a green summary (`review_not_done`/`approval_gates_not_done` are skip codes); superseded roots leak open ready-frontier steps that inflate run-operator pool demand; `source_bead_not_found` conflates true orphans with graph-invisibility.

**Single fix:** one shared `graph_children_for_root(city, rig, root_id)` helper — `gcg-` roots read via the graph-aware controller route (`GET /v0/city/<city>/beads/graph/<rootID>`), else fall back to `bounded_rig_bd_list` (the exact capability-probe-then-fallback shape). Use it in all four readers; in cleanup, run `gc convoy delete <root_id> --force` first (mirroring pr_review's 922fb57f7-era `convoy_delete_workflow`); split `source_confirmation_failed` out of the skip codes.

### Group H — HIGH (independent, co-deployed) — bd proxied-server config write skips custom_types sync
*Finding: #56.*

**Site:** `/data/projects/beads/cmd/bd/config_proxied_server.go:33` → `configSQLRepositoryImpl.SetConfig` (also SetMany/Unset variants).

**Failure mode:** graph-store-**independent**. The proxied write path updates only the `types.custom` string; reads are table-first (`GetCustomTypes`, `ResolveCustomTypesInTx`), the backfill never re-syncs a non-empty table, and doctor re-verifies against the string — so `invalid issue type: session` persists forever while doctor reports all-OK, breaking every session-lifecycle-projection bead write.

**Fix:** mirror `DoltStore.SetConfig`: after the config write, call `issueops.SyncCustomTypesTable`/`SyncCustomStatusesTable` inside the same UOW (or intercept in the repository impl). Belt-and-braces: make reads union table ∪ string.

### Group I — MEDIUM — API/read-plane duplication and inflation over the shared graph leg
*Findings: #38, #39, #40, #41, #42, #43, #44, #45, #48, #13.*

**Sites:** `internal/api/huma_handlers_beads.go:195`, `orders_feed.go:168/262`, `huma_handlers_convoys.go:96`, `handler_status.go:584`, `huma_handlers_orders.go:460/511`, `convoy_event_stream.go:449/481`, `store_health.go:89`; `internal/sling/sling_core.go:716`.

**Failure mode:** every rig store is (contra the stale contract) also a Router over the **same** city sqlite leg, so per-store loops return each `gcg-` bead once per store: duplicated bead/convoy rows and inflated totals, doubled run feeds and RunCount, ~2× status Open counts, order-history duplicates, `len(matches)==1` gates dropping SSE workflow projections, store-health ratio skew, and sling conflict/force loops keyed on `(storeRef, rootID)`.

**Single fix pattern:** dedupe graph-prefixed rows **globally by bead ID** (gate on `GraphIDPrefix() != ""`), or query the graph leg exactly once from the city store and skip graph-prefixed rows when scanning rig stores. The `qi>0` augment in `huma_handlers_beads.go:188–193` already does this — extend the same key to the primary query and siblings. For the `len==1` gates, dedupe matches by ID before the uniqueness check. Byte-identical for disjoint (default) stores.

### Group J — MEDIUM — workflow-snapshot Dolt SQL fast path blind to sqlite members
*Findings: #46, #47, #54, #55.*

**Sites:** `internal/api/convoy_sql.go:380/460/540 tryFullWorkflowSQL/tryWorkflowSQL`, `handler_convoy_dispatch.go:166–171 snapshotFromStore (usedSQL)`.

**Failure mode:** raw Dolt SQL treated as the complete subtree: cutover-straddling workflows render frozen topology; one stray Dolt bead carrying `gc.root_bead_id=gcg-…` flips `usedSQL=true` and serves a confidently-wrong 1-bead "workflow" (root + ~67 sqlite steps dropped, `Partial=false`); scoped fetches of live graph runs spam the #2940 "SQL fast path failed" warning every poll.

**Single fix:** gate/augment on residency — skip `tryWorkflowSQL` when the root ID carries the graph prefix (quiet fallback), and after any successful Dolt snapshot on a graph-capable store, merge `ListGraphOnly({Metadata: gc.root_bead_id, IncludeClosed})` by bead ID. Apply to both fast and slow paths.

### Group K — MEDIUM — graph leg emits no bead events; failed runs project as running forever
*Findings: #52 (root), #50, #51, #53.*

**Sites:** `cmd/gc/api_state.go:323 registerGraphStoreBackend` (no recorder/hook analog); `internal/dispatch/runtime.go:737/759 processWorkflowFinalize` (pass-only propagation); `internal/runproj/phasemapping.go:62 mapRunPhase`; `dashboardbff/runtailer.go:176`.

**Failure mode:** every ClassGraph mutation is invisible to `events.jsonl` ("the fold sees ~2 of ~67"), SSE workflow watchers freeze, and — the documented live incident — a root closed `gc.outcome=fail` leaves the Dolt source bead at `pr_review.workflow_status="running"` with **no arm** in the phase switch for failure, driving the retry-treadmill misdiagnosis.

**Fix:** (a) wrap the registered SQLite leg in an eventing decorator recording `bead.created/updated/closed` with full bead payloads (parity with the bd hook contract); (b) on `outcome != "pass"`, do a propagate-only (non-closing) write-back stamping a terminal failure projection onto the source chain; (c) summary path mirrors the detail path's graph read-back and maps a failed projection to a terminal phase.

### Group L — MEDIUM — hook claim/continuation fallbacks are Dolt-only
*Findings: #16, #17, #20.*

**Sites:** `cmd/gc/cmd_hook_claim.go:361/367 hookClaimDefault→hookClaimWithBdStore`, `:537 hookListContinuationWithBdStore`, `reportHookClaimRejected`.

**Failure mode:** discovery is in-process (`gc ready` sees sqlite) but the nil-API-client claim fallback is `bd update --claim` against Dolt → ErrNotFound → drain-ack churn during supervisor restarts (persistent under GC_NO_API/standalone binds); continuation-group siblings on the default history tier are invisible to both legs (session affinity silently lost); `bead.claim_rejected` never fires on the sqlite leg (ADR-0009 signal absent).

**Fix:** never fall back to Dolt-only `bd --claim` for a graph-prefixed id — open the routed city store in-process (mirror `readyStoreSet`); make `ListContinuation` graph-aware via the same capability probe; return the current bead on lost claims so the rejection event fires.

### Group M — MEDIUM — sling: graph beads break shell routing, prefix guard, and root identity
*Findings: #14, #15 (+ #13's identity key, fixed in Group I).*

**Sites:** `internal/sling/sling_core.go:524/1423 finalize` (custom `sling_query` shell fallback), `sling.go:652 CrossRigRouteError` via `BeadPrefixForCity:507`, `sling_core.go:1273`.

**Failure mode:** custom-`sling_query` slings render `bd update gcg-… --meta …` against Dolt → deterministic failure **after** the wisp is materialized (orphan unrouted root, config combination can never work); re-slinging a stranded `gcg-` step to a rig pool is refused as "cross-rig routing" (prefix `gcg` ≠ rig prefix) — the 87a788381 recovery class blocked without `--force`.

**Fix:** before shelling out, detect graph residency (`GraphOnlyListFor` + prefix) and route through the typed `Store.SetMetadata` write (or reject custom sling_query for graph beads loudly); exempt graph-prefixed beads from the prefix→rig inference in `CrossRigRouteError` — rig affinity for graph beads rides `gc.routed_to`/`gc.root_store_ref`, not store partitioning.

### Group N — MEDIUM — molecule root-scoped reads pay pointless Dolt forks
*Findings: #29, #30, #31 (perf legs; #29's correctness leg is Group D).*

**Sites:** `internal/molecule/cleanup.go:38 ListSubtree`, `molecule.go:326/414 findExistingAttach`, `:1360 existingLogicalBeadIDIndex`.

**Failure mode:** every root-scoped read on a `gcg-` root federates into the Dolt leg (~1s `bd` fork) for structurally-impossible matches — autoclose per close event, attach idempotency per spawn, N forks per fanout — the dispatcher-tick-starvation class.

**Single fix:** the standard gate — `if gol, ok := beads.GraphOnlyListFor(store); ok && strings.HasPrefix(rootID, gol.GraphIDPrefix()+"-") { gol.ListGraphOnly(q) }` — inert until Group A lands, byte-identical for default cities.

### Group O — Test gaps (land alongside the code fixes)
*Findings: #6, #12, #25, #37, #49, #67, #68, #69, #70, #71, #72, #73.*

1. **Capability-parity guard** (cheap sentinel, fails today): assert every optional capability (`GraphOnlyReadyFor`, `GraphOnlyListFor`, `Claimer`, `GraphApplyFor`, …) reports identically on `wrapStoreWithBeadPolicies(Router)` vs the bare Router; table-driven so a new capability without a forwarder fails CI.
2. **Prod-shape re-runs**: re-run `TestLiveListForRootGraphRootedSkipsWorkLeg` (`listCalls==0`) and the 08cdd75f3/2a83e20bd regression tests with `controlStoreWithGraphRouting`/`wrapStoreWithBeadPolicies` stores instead of bare Routers/fakes; add the asymmetric case (Ready present, List absent) for `graphOnlyHasAwakeAssignedWork`; a real routed-store claim test for `doHookClaim`.
3. **Two-Router shared-graph API fixture** (#49): two policy(Router) stores over one sqlite backend; assert single occurrence of a `gcg-` bead in `/v0/beads`, `/v0/convoys`, runs feed, status counts.
4. **The integration harness** (#72): `TestGraphStoreSQLitePoolDemandOrphanClaimClose` as described in §2.

## 4. Fix sequence

**Already committed (verify, don't re-land):** a7f7b2bcd (graph-aware routed pool demand), **fix-a 87a788381** (cityStoreProbeForRigPool cold-wake — but see Group C #1 for its custom-scale_check hole), 08cdd75f3 (orphan-release ownerStore heal — **currently inert in prod**, activated by Step 1), 2a83e20bd (close-check graph scope — List half inert), df3f274ce (liveListForRoot fast path — inert), **pr_merge/pr_review 922fb57f7** (patrol order.failed fix; `convoy_delete_workflow` exists in pr_review but was never applied to pr_merge cleanup — Group G).

1. **Group A + Group O.1** — the `ListGraphOnlyHandle` forwarder plus the capability-parity guard test. One small change that *activates three already-shipped fixes* (08cdd75f3, 2a83e20bd List half, df3f274ce) and stops the mid-step drain at `:2843`. Highest leverage, lowest risk; land first.
2. **Group O.4** — the two-store integration harness (#72), immediately after A so it runs against the repaired wrapper and pins the whole cluster before consumer changes churn.
3. **Group B** — the storeRef remap in `collectAssignedWorkBeadsWithStores` (heals five gates: wake, pool demand, named demand, ownership index, reachability).
4. **Group C** — demand-side: #1's custom-scale_check gate mirror, then the #5 union and the #18/#19 oracle scoping (these three change wake semantics; land after the harness exists).
5. **Groups D + F** — federateRead/DepList `PartialResultError` and per-store-ref partial gating for orphan release (fail-safe correctness under degraded legs).
6. **Group E** — cross-leg dependency design (proxy-gate beads + beads-repo is_blocked/doctor/cycle changes). Largest design surface; coordinate the gascity and beads halves in one arc so the doctor exclusion lands no later than the edge-writing changes.
7. **Groups G + H** — workflows-pack graph helper + skip-code split (unblocks live merge recovery) and the bd proxied-server config sync (separate repo, independent, can land in parallel any time — it is currently breaking session bead creation on any stale-table proxied deployment).
8. **Groups L + M** — hook-claim/continuation fallbacks and sling routing/guard exemptions.
9. **Groups I + J + K + N** — read-plane dedupe, workflow-snapshot residency gating, graph-leg eventing + failure write-back, molecule perf gates (N is a near-no-op once A is in). Remaining O.2/O.3 tests ride with their groups.

## 5. Uncertain / needs-human

No findings were filed UNCERTAIN. Judgment calls surfaced during verification that need an owner decision:

- **#5 doctrine question:** is "all real work flows through formulas" (graph-only replace in `readyForControllerDemand`) intended deployment doctrine? If yes, the fix is *reject* plain routed work-leg beads at creation and document it, not union. The FR-S0.1 comment at `build_desired_state.go:631` and `sling_core.go:~360` currently contradict the doctrine.
- **Group E representation choice:** proxy/gate bead vs teaching SQLite `Ready` to resolve absent dep targets through the Router — the latter couples the graph leg's readiness to Dolt availability (the coupling the split exists to remove). Recommend proxy beads; needs sign-off.
- **#27 default-build semantics:** the Attach cross-leg gate is *silently inert* on the default build but *permanently blocking* under the `gascity_native_beads` tag — the two read paths disagree. Decide the intended semantic before fixing, or the fix bakes in one of two current behaviors.
- **#17 tier defaults:** continuation-sibling visibility depends on `bd_compatibility` (bd-1.0.4 history tier is invisible; bd-1.0.5 no_history is visible). Confirm what the live maintainer-city actually pours before sizing this fix.
- **#65 skip-code semantics:** splitting `source_confirmation_failed` out of the skip set will make the pr-merge-ready-patrol go red on a currently-latent class; confirm the patrol's alert consumers can absorb that before flipping.