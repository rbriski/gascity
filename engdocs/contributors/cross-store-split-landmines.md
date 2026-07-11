# Cross-store split landmines — audit + conformance test plan

**Fable audit, 2026-07-07, on `feat/domain-infra-store-split` + the live
`rebase/dispatch-control-ready-onto-main` deployment (sqlite graph split).**
Plan of record for making the domain/infra split *worker-complete*.

## The common pattern (root cause)

The split routed **writes** through the class front doors
(`resolveClassStore` / `cliGraphStore` / …), but cross-store **reads, links,
and discovery** were **not** routed. Every landmine is a discovery query, an
edge/backref read, or a by-id resolution that silently falls through to the
wrong store and **FAILS OPEN** — empty result sets, traced no-ops, premature
readiness — instead of erroring. (One inversion: drain member resolution fails
*closed* and kills workflows.) A formula's execution DAG (`ClassGraph` steps,
`gc.root_bead_id`) lives in the **infra** store; its human-readable parent
(`ClassWork`) lives in the **domain** store; nearly every controller/worker
read that must span that boundary doesn't.

## The unifying fix: a composite "claimable work" store + `gc ready`

Owner decision: introduce a composite `claimableStore` (work ∪ graph) —
`Ready()`/`List` fan out and merge; `Get`/claim route to the owning backing
store by id-prefix/class (a bead lives in exactly one store → no double-claim).
Make **`gc ready` read the composite** and switch the split-city `work_query`
from `bd ready` (single-store) → `gc ready`. The subprocess hook, the reconciler
pool-demand scan, and every future claimable reader then go through one seam;
per-caller federation is subsumed. **Control dispatch stays `ClassGraph`-only**
(point it at the graph store; do not federate). **Finalize parent-close** stays
domain-via-`source_store_ref`.

## Landmines (16)

Status: `broken-on-split` (open) · `fixed-on-deployed-branch` (proven port) ·
`handled-verify`.

### P0 — nothing is reachable until dispatcher + workers see infra beads
1. **Control-dispatcher serve-loop discovery never scans infra** —
   `cmd/gc/dispatch_runtime.go` `runWorkflowServe` (env from `controllerWorkQueryEnv`,
   city/rig only). DAGs stall at dispatch. **FIXED (this branch).** The deployed
   supervisor-cached `ListReadyBeads` port was rejected: the reference is
   single-store (`GraphBeadStore()==CityBeadStore()`), its ready handler federates
   city+rigs only (never a distinct infra store), and it routes through a
   supervisor API the managed-Dolt test harness never starts (untestable per the
   DoD). Fixed instead with the settled design ("control dispatch stays graph-only
   → point it at the graph store; do NOT federate"): a targeted env/scope swap in
   `runWorkflowServe`, gated to the control-dispatcher agent on a split city, that
   points both discovery (`workEnv`→BEADS_DIR) and the per-bead control-store open
   (`workDir`) at `infraScopeRoot`. Test:
   `TestSplitCity_DispatcherDiscoversInfraControlBeads` (integration, managed Dolt).
2. **`gc hook --claim` never federates infra** — `cmd/gc/hook_cross_store.go`
   (rig/city only). Workers spawn, find no graph steps, exit (churn).
   **FIXED (this branch)** via the composite `claimableStore` + `gc ready`:
   `cmd/gc/claimable_store.go` (work ∪ infra Ready/List fan-out, by-prefix Get,
   fail-loud), `gc ready` (`cmd/gc/cmd_ready.go`, bd-shaped array, raw-JSON
   passthrough), the split-city work_query/count-form switched to `gc ready`
   (`cmd/gc/split_city_work_query.go`), and the claim mutation routed by prefix to
   the infra store (`cmd/gc/split_city_claim.go`). Test:
   `TestSplitCity_HookClaimFindsInfraStepBead` (integration, managed Dolt).

### P1 — DAG-complete parent lifecycle + link integrity — ALL FIXED (this branch)
3. **Cross-store source close silently stranded the domain parent** —
   `closeSourceBeadChain`/`walkSourceBeadChain` (`internal/dispatch/runtime.go`)
   returned nil when a cross-store `gc.source_store_ref` had no resolver.
   **FIXED (`5b03f0686`):** the close (mutate) path fails loud (finalizer stays
   open for retry); the read-only preflight keeps its no-op. Test:
   `TestProcessWorkflowFinalizeFailsLoudWhenCrossStoreRefUnresolvable`.
4. **`cook --attach` cross-store dep edge was fail-open** — `cmd/gc/cmd_formula.go`
   wired a `blocks` edge on the work-store parent to a `gcg-` infra root; bd
   stores it non-blocking → parent READY mid-DAG. **FIXED (`32bb…`, landmine #4
   commit):** cross-store attach stamps `gc.attached_workflow_root` linkage
   (fail-loud on absent root) and the composite `claimableStore.Ready` enforces
   it (parent blocked until the root closes). Same-store keeps the local edge.
   Tests: `TestClaimableReady_AttachParentBlockedWhileInfraRootOpen`,
   `TestEnsureFormulaCookAttachDep_CrossStoreStampsLinkageNotDanglingEdge`.
5. **Parent PROGRESS/failure propagation did not exist** — a failed DAG gave the
   domain parent no cross-store signal. **FIXED (`69634ef7a`):** a failed
   finalize stamps `gc.failure_reason/class/subject` on the domain parent
   (`resolveFinalizeFailureDiagnostics` + `annotateSourceBeadFailure`), leaving
   it open/redispatchable; fail-loud on a cross-store ref with no resolver. Test:
   `TestProcessWorkflowFinalizeStampsFailureOnOpenDomainParent`. Mid-DAG progress
   + the pass-path `gc.*` allowlist remain follow-ons.
6. **`routes.jsonl` asymmetry** — `collectRigRoutes` emitted HQ+rigs only.
   **FIXED (`917a2c1e6`):** `collectRigRoutes` adds the infra scope on a split
   city (bidirectional via `writeAllRoutes`); rigless split cities write routes
   at start; migrate is bidirectional; the API same-rig route fallback is
   tightened so a `gcg` route falls through to the class arm. Tests:
   `TestCollectRigRoutes_IncludesInfraScopeOnSplitCity`,
   `TestBeadStoresForIDClassArmWinsOverGcgRouteInRig`.
7. **Drain convoy membership** — drain read membership from the wrong store and
   MemberStores was unset. **FIXED (`302fe258c`):** read membership from the
   convoy's owning store, thread `MemberStores` into the unit-convoy tracks
   write, wire `MemberStores` in the production drain caller, fail loud on an
   invisible convoy. Tests: `TestDrain_CrossStoreConvoyMembership`,
   `TestDrain_WorkStoreConvoyWithoutMemberStoresFailsLoud`.

### P1.5 — sling routes v1 molecules to the wrong store (found by manual testing)
17. **v1 (plain) formula sling stranded work-class beads in the infra store** —
    `internal/sling/sling.go` `InstantiateSlingFormula`. It materialized EVERY
    molecule through `deps.graphStore()` (infra on a split city), but a plain v1
    formula produces a WORK-class molecule (root type `molecule`, steps type
    `step`, no graph markers), so its beads were stranded in the infra store,
    violating the boundary. The per-class routing inside the policy store
    (`createTarget`/`graphApplierFor`) is identity today, so nothing corrected it.
    **FIXED (this branch):** the sling now routes the whole molecule by its
    wholesale class (`recipeMaterializesInfraClass`, mirroring
    `coordclass.ClassifyGraphPlan`): graph.v2/wisp/convergence → infra, plain v1 →
    work. `IsCompiledGraphWorkflow` was rejected as the selector (workflow-only →
    would misroute wisps). Tests: `TestRecipeMaterializesInfraClass`,
    `TestInstantiateSlingFormulaRoutesMoleculeByClass` (internal/sling).

### P1.6 — deferred session-create waited on the wrong store (found by RC gate / acceptance)
18. **`WaitForSessionCommandable` read the work store, not the infra store** —
    `cmd/gc/api_state.go` `(*controllerState).WaitForSessionCommandable`. A
    deferred session create (`POST /session` → `handle.Create(CreateModeDeferred)`
    → `WaitForSessionCommandable`) built its commandability catalog over
    `cs.CityBeadStore()` (the work store), but session beads are the sessions
    coordination class and live in the infra store on a split city. So the wait
    Get-missed its own session id and the create failed with
    `create_failed: getting session: getting bead "gcg-…": bead not found`
    (`internal/session/chat.go` `loadSessionBead`). Not in the #1–#16 audit;
    surfaced only once new cities defaulted to the split (`5b4a7dff5`). Found by
    the RC-gate/acceptance run of `TestGCLiveContract_BeadsAndEvents` (fails
    split-default, passes with `GC_INFRA_STORE_SPLIT=0` — confirmed split fallout,
    not a flake or a P2/P3 regression). **FIXED (this branch):** read
    `cs.SessionsBeadStore().Store` (the infra-routed sessions store; identity to
    `CityBeadStore` on a single-store city). Test:
    `TestWaitForSessionCommandable_ReadsInfraStoreSessionOnSplitCity` (fast-unit,
    reproduces the exact production error on the old code) + integration
    `TestGCLiveContract_BeadsAndEvents` green.

### P2 — medium — #8, #10, #11, #13 FIXED (this branch); #9, #12 handled-verify
8. **FIXED (`34a9faf43`).** CLI sling singleton/replacement scan excluded infra
   → duplicate workflows. The inline `SourceWorkflowStores` closure at
   `cmd/gc/cmd_sling.go` used `openSourceWorkflowStores` (work-class); extracted
   into a testable `slingSourceWorkflowStores` helper routed through the
   graph-root opener (`openSourceWorkflowGraphStores`, includeInfra=true), fail
   loud on a broken infra store, warn-and-continue on a broken rig. Tests:
   `TestSlingSourceWorkflowStoresIncludesInfraOnSplitCity`,
   `…FailsLoudOnBrokenInfra`, `…LegacyIsWorkOnly`.
9. **handled-verify (no code change).** Cross-store parent close is event-silent
   *at write time*, but the controller's CachingStore watchdog reconciler
   synthesizes a verified `bead.closed` (actor `cache-reconcile`) within one
   reconcile cadence (30–120s) and drives cache invalidation + the autoclose
   cascade off it. Per-write event hooks were deliberately removed
   (`cmd/gc/hooks.go`); every one-shot close (finalizer, quarantine, `gc close`)
   is in the same eventual-consistency class, and the chain walk itself always
   re-reads through raw (uncached) stores, so it is fail-safe, not fail-open.
   Pinned by `TestReconcileEmitsCloseWhenGetReturnsClosed` +
   `…FreshClosePayload…` (`internal/beads`).
10. **FIXED (`3bfeaff84`).** Wisp-autoclose input-convoy reaping read `tracks`
    from the graph store, but a post-split input convoy + tracks edge live in the
    WORK store. `collectInputConvoyWorkflowRoots` now union-probes both owning
    stores (work ∪ graph, fail-closed, migrated-legacy convoy still found); the
    root `ListByMetadata` stays on the graph store. Tests:
    `TestWispAutocloseClosesRootOnlyWispViaInputConvoyAcrossStores`,
    `…ViaMigratedInfraConvoy`.
11. **FIXED (`a67c44ef7`).** Rig removed from `city.toml` → the resolver's plain
    "rig not found" error hit the cmd-layer quarantine catch-all and terminally
    closed the finalizer (root + parent stranded open forever). `makeStoreRefResolver`
    now classifies rig-not-found as `dispatch.ErrControlPending` (retryable; heals
    when the rig is re-added). Not split-specific. Test:
    `TestFinalize_RigRemovedFromConfig_RetriesNotQuarantines`.
12. **handled-verify on this branch (no code change) + merge-order guard.** No
    fallback-free cached reader exists here (every `Cached.Ready`/`List` consumer
    falls back to live; `Cached.DepList` has zero callers), the reconciler's ready
    verdicts are mandatory-live-authoritative, and no decision path joins the two
    stores' caches — so the whole-store dirty decline can only cause a
    conservative fall-back-to-live, never a stale verdict. The real instance (the
    supervisor-cached, fallback-free control-ready lane) exists only on the
    deployed branch and was fixed there by `c2257d206` (per-bead dirty scoping).
    **GUARD:** any future port of the supervisor-cached control-ready stack
    (`d9a23e6fb`/`3c2173765`) MUST carry `c2257d206` and its tests, or the
    fallback-free lane starves on a split city. Do NOT blanket per-bead-scope
    `cachedListOnly`/`cachedDepListOnly` (fail-open for complete-list callers).
13. **FIXED (`c224a9792`).** Partial-federation blindness: HTTP `/v0/beads/ready`
    and `/v0/beads` federated city + rigs only, so a split city's whole graph DAG
    was invisible behind an authoritative 200 (not even flagged Partial). Added
    the infra federation arm to both handlers, gated like the by-id class arm; an
    infra-leg hard failure is an authoritative 503 (not a degraded Partial 200),
    a `PartialResultError` still flows as Partial. The deployed-branch label was
    misleading — the fix lived in `coordrouter` (absent here), so it was written
    new. Tests: `TestBeadReadyFederatesInfraStore`,
    `TestFederatedReady_InfraPartialIsAuthoritativeFailure`,
    `TestBeadListFederatesInfraStore`, `…HardFailIs503`, `…PartialPreservesRows`,
    `TestBeadReadySingleStoreCityUnchanged`.

### P3 — low — #14, #15(Half B) FIXED (this branch); #15(Half A), #16 handled
14. **FIXED (`ebeba2a55`).** `resolveRequiredArtifactWorktree` point-read the
    source bead through the ambient graph store; on a split city the source /
    input convoy live in the work store, so it got ErrNotFound and misclassified
    a passing retry as transient `missing_required_artifact_context` (fail-open,
    burns retries). Now threads `ProcessOptions` and federates the source read
    like `walkSourceBeadChain` (`gc.source_store_ref` via `ResolveStoreRef`, then
    `storeref.Resolve` over `[sourceStore] + MemberStores`); `MemberStores` wired
    for the `retry`/`retry-eval` control kinds. Tests:
    `TestClassifyRetryAttemptWithPostconditionsResolvesSourceBeadAcrossStores`,
    `…CrossStoreSourceWithoutResolverFailsLoud`, `…ResolvesInputConvoyViaMemberStores`.
15. **Half B FIXED (`5fb0888cd`); Half A no live path (documented).**
    - Half B (unresolved convoy-member placeholders in graph/convoy views):
      `collectBeadGraph` + both API convoy views + the CLI `gc graph` convoy
      expansion now probe the class complement (`Server.memberStoreComplement` /
      `graph{Infra,Work}MemberStores`), and `openRigAwareStore` gained a
      reserved-class arm so `gc graph gcg-…` opens the infra store instead of
      NotFound. Tests: `TestCollectBeadGraph_CrossStoreConvoyMembersResolved`,
      `TestMemberStoreComplement_SingleStoreIsNil`,
      `TestResolveGraphInput_ExpandsCrossStoreConvoyMembers`.
    - Half A (dangling cross-store `ParentID` at `molecule.go:569`): no live
      producer on this branch — the workflow class that routes to infra is
      guarded out of the `opts.ParentID` stamp, and `cook --attach` uses
      `gc.attached_workflow_root` (landmine #4), not `ParentID`. Left as a
      latent seam; optional one-line `Get`-guard noted but not landed (defends a
      path with zero callers).
16. **handled-verify confirmed (no code change).** The finalize happy path closes
    the domain parent via `gc.source_store_ref` (`makeStoreRefResolver` → work
    store) and the live-root "routed inventory" scan is federated work ∪ infra
    (`openSourceWorkflowGraphStores`, gated on `cityHasInfraStore`), fail-loud on
    a degraded scan. Pinned by `TestProcessWorkflowFinalizeClosesCrossStoreSourceBead`,
    `…FailsLoudWhenCrossStoreRefUnresolvable`, `…LeavesAncestorOpenWhenLiveRootExistsInAnotherStore`.

## Conformance test suite (16) — TDD, must fail on a split city first

Integration (managed Dolt): `TestSplitCity_DispatcherDiscoversInfraControlBeads`
(1), `TestSplitCity_HookClaimFindsInfraStepBead` (2),
`TestFinalize_ClosesDomainParent_SetsProgress_UnblocksDep` (3+4),
`TestFailedDAG_ParentRedispatchableWithFailureMarker` (4+5),
`TestSplitCity_EndToEndFormulaLifecycle` (whole-suite anchor:
discovery→claim→drain→finalize→parent-close).
Fast-unit: `TestWalkSourceBeadChain_MissingRef_IsErrorNotSilentNoop` (3),
`TestSourceStoreRefStamping_AllLaunchPaths` (3),
`TestFinalize_RigRemovedFromConfig_RetriesNotQuarantines` (11),
`TestInit_E2SplitCity_WritesBidirectionalRoutes` (6),
`TestDrain_CrossStoreConvoyMembership` (7),
`TestCLISling_SingletonScanSeesInfraRoots` (8),
`TestCrossStoreClose_EmitsEventAndDirtiesCache` (9),
`TestCachedListOnly_PerBeadDirtyScope` (12),
`TestFederatedReady_InfraPartialIsAuthoritativeFailure` (13),
`TestWispAutoclose_InputConvoyEdgesFromWorkStore` (10),
`TestRetryEval_SourceArtifactResolvedCrossStore` (14).

## Sequencing

1. **P0 — DONE (this branch).** A formula now RUNS on a split city: the
   control-dispatcher discovers infra control beads (#1, env/scope swap) and a
   worker claims infra graph steps (#2, composite `gc ready` + by-prefix claim
   routing). Both proven by `TestSplitCity_DispatcherDiscoversInfraControlBeads`
   and `TestSplitCity_HookClaimFindsInfraStepBead` on real managed Dolt
   (fail-on-split-first, then green). Commits `13626f769` (#1),
   `63235fe0a`/`2ed2fc961` (#2a/#2b), `3cc26e376` (gc ready passthrough),
   `4d99a00cc` (#2c).
2. **P1 — DONE (this branch).** All five fixed: source_store_ref no-op→loud
   error (3), routes.jsonl bidirectionality (6), cook --attach cross-store
   linkage + composite enforcement (4), parent failure-marker propagation (5),
   drain cross-store membership (7). Each TDD'd with fast-unit coverage; the
   real-bd repros (parent READY mid-DAG, drain over managed Dolt) remain as E2E
   anchors on the standing integration suite.
3. **P2/P3 — DONE (this branch).** Real read/link fixes landed for #8
   (`34a9faf43`), #10 (`3bfeaff84`), #11 (`a67c44ef7`), #13 (`c224a9792`), #14
   (`ebeba2a55`), #15 Half B (`5fb0888cd`), each TDD'd fast-unit. #9, #12, #16,
   and #15 Half A are handled-verify / no-live-path (no code change), with a
   merge-order guard recorded for #12. Investigated via a 9-agent parallel audit
   (each: current trace → confirm/already-handled → minimal fix reusing existing
   seams, fail-loud, gated on `cityHasInfraStore` → TDD plan).
4. Land `TestSplitCity_EndToEndFormulaLifecycle` as the standing regression that
   a formula runs discovery→claim→drain→finalize→parent-close on a real split
   city.

## Soundness — invariant coverage + revert-matrix falsification (2026-07-11)

The landmines above are not merely "fixed"; the fix set is now
*invariant-covered* and *falsifiable*. Three artifacts turn "the split-store
architecture is sound" from a claim into a standing, reproducible test.

### 1. The conformance suite — 11 invariants × 2 topologies

`cmd/gc/split_topology_conformance_test.go` (`TestSplitTopologyConformance`)
runs every store-ownership invariant on **both** topologies via
`forEachTopology` / `forEachTopologyWithRig` over the `splitEnv` fixture:

- **single-store** — `infra == nil`; `resolveClassStore` collapses to the work
  store. This is the legacy, pre-split city and doubles as the byte-identity
  regression (a fix must not change legacy behavior).
- **split** — `infra != nil`; coordination classes (`gcg`/`gcs`/`gcm`/…)
  resolve to the infra store. The two-database city under test.

The `internal/beads/splittest` strict-store leaf decorator fails loud on any
cross-store dependency edge, so an invariant that silently resolved a bead in
the wrong store reds instead of passing on an empty result.

| Invariant | Guards |
| --- | --- |
| I1 ready-federation | #2 — `gc hook --claim` / composite `gc ready` surfaces infra work |
| I2 assigned-work-capture | post-claim capture of infra `in_progress` wisps (orphan-release/treadmill family) |
| I3 by-id-write-residence | by-id writes land in the owning store, never the wrong one |
| I4 materialization-residence | #17 — a v1 formula molecule materializes in the work store |
| I5 claim-routing | #2/#19 — claim mutation routes by id-prefix incl. all wisp-id shapes |
| I6 strict-cross-store-deps | #4 — `cook --attach` cross-store dep fails loud, not open |
| I7 by-id-read-federation | by-id reads federate across work∪infra |
| I8 residence-sweep | integrity backstop: class ⇔ owning store, swept over all beads |
| I9 warm-tick-demand | the spawn/drain treadmill driver (mc-wisp-orphan RCA) |
| I10 wake-ownership-fast-path | the session-wake ownership filter (`7ee481bf2`) |
| I11 read-path-consistency | the operator-confusion class (#19 read-path) |

### 2. The row-guard — `make check` enforces both topologies

`scripts/check-split-topology-rows.sh` (wired into `make check` beside
`check-routed-test-rows`) statically forbids adding a one-topology invariant:
every `t.Run("I…")` must route through `forEachTopology`, and the suite may not
call `newSplitEnv` directly. Cheap and always-on. It prevents the slow rot where
a new invariant quietly covers only the split path and a single-store
regression sails through (or vice versa).

### 3. The revert-matrix — falsifies each guard, on demand

`scripts/check-split-topology-reverts.sh` (`make check-split-topology-reverts`)
is the soundness ledger made executable. For each split-store fix commit it
reverts the production change in a throwaway detached worktree — narrow
production-hunk reverse-apply first, 3-way whole-commit revert (with the
commit's own test files restored from `HEAD`) as fallback — then runs the named
guarding test and asserts it **reds**. A fix whose test still passes with the
fix reverted is a **HOLE**: the guard is decorative and the soundness claim over
that landmine is unbacked. Expensive (a worktree + per-package build per entry),
so it is on-demand, not part of `check`.

**Last run — 2026-07-11, HEAD `0de8cc719`: `checked=8 guarded=8 holes=0
conflicts=1`.**

| Fix commit | Landmine | Guarding test (package) | Outcome |
| --- | --- | --- | --- |
| `5b03f0686` | #3 fail-loud cross-store source close | `TestProcessWorkflowFinalizeFailsLoudWhenCrossStoreRefUnresolvable` (internal/dispatch) | RED |
| `3bfeaff84` | #10 reap root-only wisp via owning store | `TestWispAutocloseClosesRootOnlyWispViaInputConvoyAcrossStores` (cmd/gc) | RED |
| `c224a9792` | #13 federate infra in HTTP ready/list | `TestBeadReadyFederatesInfraStore` (internal/api) | RED (build) |
| `237c2fc9c` | #17 v1 molecule → work store | `TestInstantiateSlingFormulaRoutesMoleculeByClass` (internal/sling) | CONFLICT |
| `09890178b` | #18 session commandability from infra | `TestWaitForSessionCommandable_ReadsInfraStoreSessionOnSplitCity` (cmd/gc) | RED |
| `180ad7dd8` | spawn/drain treadmill (isCold gate) | `TestBuildDesiredState_WarmTick_TreadmillSessionsStayDesired` (cmd/gc) | RED (build) |
| `6b58b621b` | #19 reserved-class id by first segment | `TestReservedClassBeadIDPrefix` (internal/config) | RED (build) |
| `6b58b621b` | #19 wisp-id claim routing | `TestClaimableStoreRoutesWispIDsToInfra` (cmd/gc) | RED (build) |
| `7ee481bf2` | wake-filter infra store-ref leg | `TestReleaseOrphanedPoolAssignments_OwnsClaimedInfraWispWithoutLiveProbe` (cmd/gc) | RED |

Outcome legend:

- **RED** — the test fails *behaviorally* with the fix reverted. The strongest
  signal: the guard demonstrates the bug returning.
- **RED (build)** — the test cannot *compile* without the fix, because it is
  coupled to a symbol/signature/new-file the fix introduced. Still guarded (the
  test cannot pass without the fix), but it does not demonstrate the behavior
  breaking. Typical for new-file fixes (`reserved_prefixes.go`) and
  new-signature reconciler helpers.
- **CONFLICT** — the fix's production lines were superseded by later refactors,
  so neither revert method can isolate this historical commit. The behavior
  stays guarded by the standing green test; this row is simply not
  revert-falsifiable. #17's sling routing was subsequently reworked.

Keep the `MATRIX` in the script in sync: when a new split-store fix lands, add
its commit + the regression test that must red without it, and run the matrix.

### Named residuals — the honest boundary

The architecture is sound **for the store-ownership-resolution class** on this
branch's bd-backed (Dolt / sqlite / postgres) topology. This is *not* a claim
that every path is covered. The known, deliberately-deferred gaps:

- **Fixture fidelity.** The fast conformance fixtures use `MemStore`, not real
  Dolt/Postgres/`CachingStore`. Backend-contract drift (a bd metadata-format
  change, a real-Dolt-only claim race) is caught only by the standing managed
  integration tests, not the conformance suite. A real-bd contract pin is a
  named follow-up.
- **#12 merge-order guard is prose-only.** No test enforces the finalize/merge
  ordering; it is documented, not gated.
- **`resolveBdScopeTarget` arg-scan hijack.** A reserved-shaped *value* on a
  `gc bd` command line (e.g. `--label gcg-wisp-ta1`) can reroute the exec's
  scope. By-id routing is guarded (I5); the arg-scan surface is not.
- **The named-session assignee arm.** `assigneePreservesNamedSessionRoute`
  lacks the `""` infra leg its siblings received.
- **The city WORK-store drop (E2).** `rigBeadStores` deletes the `cityName`
  key, so a city-scope *non-workflow* pool bead orphaned by a dead worker can
  miss orphan-release. Confirmed empirically a non-issue for the maintainer-city
  workload (0 `in_progress` non-`gcg` city-scope work beads) and deferred, but a
  real residual on the general model.

These are the boundary, stated plainly rather than hidden. The revert-matrix
reds the moment a mapped fix regresses; the residuals list is where to look when
a *new* class of split bug appears.
