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
