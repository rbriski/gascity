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

### P2 — medium
8. CLI sling singleton/replacement scan excludes infra → duplicate workflows —
   `cmd/gc/cmd_sling.go:419`.
9. Cross-store parent close is event-silent + cache-invisible —
   `internal/dispatch/runtime.go:1068`.
10. Wisp-autoclose input-convoy reaping reads `tracks` from the wrong store —
    `cmd/gc/wisp_autoclose.go:176`.
11. Rig removed from `city.toml` → finalizer quarantined, parent open forever —
    `cmd/gc/cmd_convoy_dispatch.go:269`.
12. Cache-handle correlated staleness (whole-store dirty decline for
    List/DepList + mandatory live Ready in reconciler) —
    `internal/beads/caching_store_handles.go:233`.
13. Partial-federation blindness outside the control lane (a partial read
    omitting infra makes the whole DAG invisible with a 200) —
    `internal/api/types_read.go:58`. **fixed-on-deployed-branch**.

### P3 — low
14. `resolveRequiredArtifactWorktree` cross-store read with no resolver —
    `internal/dispatch/retry.go:418`.
15. Dangling cross-store `ParentID` on attach pours + unresolved convoy-member
    placeholders in graph views — `internal/molecule/molecule.go:569`.
16. Finalize close happy path + routed inventory — `internal/dispatch/runtime.go:711`
    (**handled-verify**).

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
3. **P2/P3** — the remaining reads/links, each a "route this read through the
   composite / the right store" change with its fast-unit guard.
4. Land `TestSplitCity_EndToEndFormulaLifecycle` as the standing regression that
   a formula runs discovery→claim→drain→finalize→parent-close on a real split
   city.
