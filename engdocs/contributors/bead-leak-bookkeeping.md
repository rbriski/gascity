# Bead Leak Bookkeeping Report

Date: 2026-06-01
Branch: `analysis/rescue-drain-rebase-main-20260530`

This report tracks the open-bead stability work for idle cities. The target
invariant is: after startup and any configured orders settle, an idle city must
have a bounded number of open issue-tier beads and wisp-tier beads. Any
infrastructure path that creates an open bead must have an owner that closes it
or a maintenance path that can safely reap it.

## Current Patch

`ga-k5ds4` identified that `reaper.sh` only closed stale wisps with a
`parent-child` edge to a closed target. Graph-v2 workflows and rescue-drain
flows commonly link step wisps to roots or peer steps with `tracks` and
`blocks`; those step wisps could remain open forever even after their targets
closed.

The reaper now treats `parent-child`, `tracks`, and `blocks` as reaper-owned
closure edges. A stale non-closed wisp is closed only when:

- it has at least one of those dependency edges to a closed issue or wisp
- it has no matching edge whose target is open, non-closed, missing, or
  unresolved

Closed-wisp purge protection now uses the same edge set, so a closed target is
not deleted while a non-closed wisp still depends on it through one of those
edges.

The live Dolt schema stores the wisp graph in `wisp_dependencies`, not in the
issue-level `dependencies` table. The reaper's wisp close and purge paths now
probe and query `wisp_dependencies`; the stale issue auto-close exclusion path
still uses `dependencies`. Every mutating SQL block now selects the target
database with `USE <db>` before `UPDATE` or `DELETE`; live Dolt rejected the
qualified `ga.*` update without that active database context.

Live inspection also found generated step-spec debris: old unassigned `spec`
wisps titled `Step spec for ...` with no incoming or outgoing wisp dependency
edges and no issue-level dependencies. Those rows are stranded before graph
wiring completes, so the reaper now closes only that isolated generated-spec
shape. Ordinary no-edge wisps remain report-only.

`ga-6pbt8` identified that `runControlDispatcherWithStoreAndConfig`
hard-quarantined every `ProcessControl` error except `ErrControlPending`.
That swallowed transient store/controller faults before the serve loop could use
`dispatch.IsTransientControllerError` to retry them. The dispatcher now returns
recognized transient controller errors without closing the control bead.
Deterministic hard errors, including malformed control graphs and unsupported
control kinds, still quarantine. If `ProcessControl` already terminally closed
the bead with `gc.final_disposition=controller_error`, the wrapper preserves
that disposition instead of re-closing the bead as `control_quarantined`.

`TestGastownIdleOpenBeadCountsStayBounded` now runs in Tier B nightly
acceptance. It starts an isolated Gastown city with fake sessions, shortens the
patrol interval, adds a fast formula order and a fast exec order, and samples
open issue-tier and wisp-tier counts across repeated controller cycles. The
test fails if either open-count series grows beyond a small transient jitter
window after warmup.

`ga-hiew1` split into two retention checks in this branch. First, the built-in
Dolt compactor order remains installed and dispatched through the managed Dolt
layout, so its `gc dolt compact` execution receives the live managed database
environment instead of poisoned shell overrides. Existing order/controller tests
cover that path. Second, `wisp gc` now treats expired closed graph-v2 workflow
roots as GC candidates by indexed workflow-root metadata
(`gc.kind=workflow` or `gc.formula_contract=graph.v2`) and validates matches
with `sourceworkflow.IsWorkflowRoot` before deleting the root closure. Open or
recent workflow roots are left alone, and roots matching both metadata queries
are de-duplicated.

The Dolt compactor also now accepts the order-dispatched explicit loopback
external target shape (`GC_DOLT_MANAGED_LOCAL=0`, local `GC_DOLT_HOST`, explicit
`GC_DOLT_PORT`) without requiring managed runtime state. Non-local external
targets still skip without querying. This covers cities such as
`/data/projects/maintainer-city`, where the canonical endpoint is a locally
managed-by-operator Dolt server on `127.0.0.1:3307`, not a `gc start` managed
runtime.

## Creation Paths

| Path | Beads opened | Bookkeeping owner |
| --- | --- | --- |
| `internal/molecule/graph_apply.go` graph workflow instantiation | Wisp root plus logical/step wisps. Non-root graph steps are linked to the root with `tracks`; explicit ordering uses `blocks`; legacy containment uses `parent-child`. | Normal workflow execution closes runnable steps. `molecule.CloseSubtree` closes owned descendants during explicit cleanup. `reaper.sh` now closes stale leftovers when all reaper-owned dependency targets are closed. |
| `cmd/gc/order_dispatch.go` order dispatch | Ephemeral order-tracking bead labeled `gc:order-tracking`; wisp orders also create a molecule/wisp root via `molecule.Instantiate`. | `dispatchOne` defers `closeOrderTrackingBead`. Wisp roots are intentionally not auto-closed solely because descendants finish; the reaper handles stale roots/steps only when dependency evidence proves closure is safe. |
| `cmd/gc/bead_policy_store.go` storage policy wrapper | Applies default ephemeral storage to wisp/order-tracking policies and no-history storage to session/wait/nudge policies. | Policy only selects storage tier; lifecycle is owned by the creating subsystem and maintenance scripts. |
| Session pool creation in `cmd/gc/build_desired_state.go` and lifecycle paths | Session beads, including generic ephemeral session beads for managed pools. | Session lifecycle/reconciler close or retire sessions. `reaper.sh` prunes closed `gm-*` session beads through `bd prune`; orphan-sweep preserves live ephemeral session assignees. |
| Convoy and API helper paths under `cmd/gc/` and `internal/api/` | User-visible issue-tier convoys/tasks plus dependency edges such as `tracks`. | User/controller workflow owns closure. These are not age-reaped unless they are wisp-tier stale closure candidates. |

## Cleanup Paths

| Path | Responsibility | Current status |
| --- | --- | --- |
| `examples/gastown/packs/maintenance/assets/scripts/reaper.sh` | Close stale non-closed wisps with closed dependency targets; close isolated generated step-spec debris; purge old closed wisps; auto-close stale city issues; prune closed `gm-*` session beads. | Patched for `parent-child`/`tracks`/`blocks` closure and purge protection through `wisp_dependencies`, plus a narrow unassigned `Step spec for ...` no-edge cleanup. |
| `examples/gastown/packs/maintenance/assets/scripts/wisp-compact.sh` | Promote old non-closed ephemeral beads for stuck detection and delete expired closed wisps. | Still separate from the safe-close decision. It must not become an age-only closer. |
| `internal/molecule/cleanup.go` | Close molecule subtrees by ownership metadata and parent-child descendants. | Handles explicit teardown, not abandoned workflow drift. |
| `cmd/gc/wisp_gc.go` / `wisp autoclose` | Close attached workflow roots and owned workflow beads from CLI-driven cleanup. Purge expired closed wisps, order-tracking beads, and closed graph-v2 workflow-root closures. | Patched to include workflow-root closure GC through indexed metadata queries guarded by `sourceworkflow.IsWorkflowRoot`. |
| `cmd/gc/order_dispatch.go` | Close order-tracking beads after dispatch attempt completion. | Existing defer is the primary owner; stale tracking-bead bugs should be treated as order-dispatch defects. |

## Remaining Work

- Finish the companion rescue-drain bug `ga-ksno8`: required-artifact
  postcondition store errors must surface instead of consuming retry attempts.
  This code path is not present in this rebase-main worktree as of this report.
- Live dry-run evidence on 2026-06-01 against `/data/projects/maintainer-city`
  with `gc` shimmed to `/bin/true`:
  `reaper — stale_wisps:1397, closed_wisps:0, purged:0, sessions-pruned:0, closed:0, skipped_non_city_issues:0, mail_wisps:126, would_close_wisps:1235 (dry run)`.
- Direct SQL against the same Dolt server matched the dry-run first-pass
  close total: `bd=0`, `ga=1024`, `gg=0`, `gp=0`, `gt=81`, `mc=0`,
  `my_db=130`, `rig=0` for `1235` total. The first live run drained `gt`
  and `my_db` but proved `ga` mutation needed an explicit `USE ga;` before
  `UPDATE`; after that fix, a second live run closed `1031` `ga` wisps.
  Post-drain `ga` had `842` non-message open wisps but only `13` stale
  non-closed wisps older than 24h. The remaining above-threshold open count is
  dominated by fresh active workflow wisps from the non-idle live queue, not
  old reaper-eligible backlog.
- Live Dolt compactor dry-run evidence on 2026-06-01 against
  `/data/projects/maintainer-city` using the branch script and the
  order-dispatched explicit loopback external-target environment reached `mc`
  instead of skipping the endpoint. It then stopped on an existing integrity
  quarantine marker:
  `/data/projects/maintainer-city/.gc/runtime/packs/dolt/compact-quarantine/mc`
  with `reason=post-flatten value hash changed without row-count increase` and
  `created_at=2026-05-20T11:14:29Z`. That marker requires separate manual
  integrity review before live compaction or full GC can run for `mc`.
