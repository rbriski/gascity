# Next-session prompt — make the domain/infra split worker-complete (P0)

You are continuing the domain/infra store-split on branch
**`feat/domain-infra-store-split`** (worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`). The front-door
interface layer is already merged to OSS main (#4017) — do not touch it. The
split is rebased + validated, but an audit found it is **not worker-complete**:
16 cross-store landmines, one root pattern (writes were routed through the class
front doors, but cross-store reads/links/discovery were NOT → they fall through
to the wrong store and fail open).

## Read first (in order)
1. `engdocs/plans/infra-store-decouple/CROSS-STORE-COMPLETENESS-HANDOFF.md` — full
   state, settled design, branch hashes, gotchas.
2. `engdocs/contributors/cross-store-split-landmines.md` — the 16 landmines +
   16-test conformance suite + sequencing (plan of record).
3. Memory `gascity-frontdoor-layer-upstreamed` (via MEMORY.md).

## Settled design (do not relitigate)
Keep `ClassGraph` in infra. Unifying fix = a composite `claimableStore`
(work ∪ graph) + make **`gc ready` composite-aware** and switch the split-city
`work_query` from `bd ready` → `gc ready`. Control dispatch stays graph-only;
finalize parent-close stays domain-via-`source_store_ref`.

## Your task this session: P0 — make a formula RUN on a split city
Nothing else is testable until the dispatcher and workers can see infra beads.
Work TDD (write the failing split-city test first, then the fix):

1. **Landmine #1 — control-dispatch discovery.** Port the deployed control-ready
   pattern from worktree `/data/projects/gascity/.claude/worktrees/fix-main-ci`
   (branch `rebase/dispatch-control-ready-onto-main`, commits
   `95518bc3a..c2257d206`, READ-ONLY — it's a live deployment): supervisor-cached
   `ListReadyBeads` + `internal/beads/control_ready_filter.go` +
   `ErrControllerAPIUnavailable` transient mapping. Target on this branch:
   `cmd/gc/dispatch_runtime.go:771` (`controllerWorkQueryEnv` is city/rig only).
   Test: `TestSplitCity_DispatcherDiscoversInfraControlBeads` (integration).
2. **Landmine #2 — worker claim.** Build the composite `claimableStore`
   (work ∪ graph; Ready/List fan-out+merge; Get/claim route by owning store),
   make `gc ready` read it, and switch the split-city `work_query` to `gc ready`.
   Target: `cmd/gc/hook_cross_store.go:39`, the work_query in
   `cmd/gc/cmd_hook.go:272`, and the `gc ready` command. Test:
   `TestSplitCity_HookClaimFindsInfraStepBead` (integration).

A prior design pass produced a concrete env recipe for the infra work-query
(reuse `bdRuntimeEnvForRigWithErrorNoRecovery(cityPath, cfg, infraScopeRoot())` —
the same builder production uses to open the infra store) — see the
`design-graph-hook-federation` workflow output if you can find it; but the owner
chose the composite `gc ready` path over per-caller env federation, so prefer the
composite seam.

## Discipline
Use Fable for design, opus for implementation, fable red-team workflow for review
(ultracode is on — author workflows for substantive steps). `GOCACHE=/tmp/gc-reloc-cache`;
never `go clean -cache`. Integration tests: `GC_FAST_UNIT=0 ... -tags integration`
with `setupManagedBdWaitTestCity`. `git commit --no-verify`, trailer
`Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>`. `bd` is unusable here.
Do NOT push the branch until the owner says so.

## Definition of done for P0
A formula poured on a split city is DISCOVERED by the dispatcher (control beads in
infra) AND its routed step beads are CLAIMED by a worker via `gc hook --claim`
(→ `gc ready` composite), proven by the two integration tests green on real
managed Dolt. Then P1 (the DAG-complete parent lifecycle: source_store_ref
no-op→error, routes.jsonl, build progress + dep-unblocking) becomes testable.
