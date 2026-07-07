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
   `cmd/gc/dispatch_runtime.go:771` (`controllerWorkQueryEnv` city/rig only).
   DAGs stall at dispatch. **fixed-on-deployed-branch** (supervisor-cached
   `ListReadyBeads` + `internal/beads/control_ready_filter.go`; commits
   `d9a23e6fb`/`3c2173765`/`c2257d206`) — port it.
2. **`gc hook --claim` never federates infra** — `cmd/gc/hook_cross_store.go:39`.
   Workers spawn, find no graph steps, exit (churn). **broken-on-split** →
   composite `gc ready`.

### P1 — DAG-complete parent lifecycle + link integrity
3. **Empty/mis-stamped `gc.source_store_ref` → silent `deleted_parent` no-op →
   domain parent stranded open forever** — `internal/dispatch/runtime.go:850`;
   writer `internal/sling/sling_core.go:658`, `cmd/gc/cmd_sling.go:1213`,
   `cmd/gc/cmd_convoy_dispatch.go:239`.
4. **`cook --attach` cross-store dep edge is fail-open** —
   `cmd/gc/cmd_formula.go:915`. Parent shows READY mid-DAG (double-execute);
   on `OutcomeFail` permanently blocked by an edge whose infra target can never
   close in the work store. E3-migrated cities worse (FK `ON DELETE CASCADE`
   drops work→infra blocking edges at migration).
5. **Parent PROGRESS update does not exist anywhere** —
   `internal/dispatch/runtime.go:1077`. Must be **built** (deployed branch is
   close-only too). No progress key in `beadmeta`/`dispatch`/`sourceworkflow`;
   `copyNonGCMetadata` strips all `gc.*` so even `failure_reason` never reaches
   the parent.
6. **`routes.jsonl` asymmetry** — `cmd/gc/rig_beads.go:95` (`collectRigRoutes`
   HQ+rigs only, never infra); infra routes written only by `gc migrate
   infra-store`. Fresh two-store-by-default cities have **no** infra routes →
   no domain store resolves `gcg-` ids. Root-cause multiplier.
7. **Drain convoy membership** — `internal/dispatch/drain.go:211`.
   `ProcessOptions.MemberStores` set by no production caller; input convoy +
   `tracks` edges created in the **work** store while drain reads the **graph**
   store → fail-*closed* kills the workflow.

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

1. **P0 first** — nothing else is testable until the dispatcher and workers can
   see infra beads: port the control-ready pattern (1) + composite `gc ready`
   (2). Write tests 1, 2 (fail → pass).
2. **P1** — `source_store_ref` no-op→error (3), routes.jsonl bidirectionality
   (6), then BUILD progress + dep-unblocking (4, 5), drain membership (7).
3. **P2/P3** — the remaining reads/links, each a "route this read through the
   composite / the right store" change with its fast-unit guard.
4. Land `TestSplitCity_EndToEndFormulaLifecycle` as the standing regression that
   a formula runs discovery→claim→drain→finalize→parent-close on a real split
   city.
