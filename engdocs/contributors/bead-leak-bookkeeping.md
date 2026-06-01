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

Live inspection found another boundedness gap in session beads. Several old
session wisps were `state=asleep` with `sleep_reason=drained` but lacked
`slept_at`, so `gc session prune --state asleep` skipped them forever.
`PruneDetailed` now treats that legacy drained-asleep shape as eligible for
`StateDrained` pruning and falls back to the bead `UpdatedAt` timestamp only
when `slept_at` is missing, not malformed. The reaper's live path now runs
`gc session prune --state drained --before ${GC_REAPER_SESSION_STATE_PRUNE_AGE:-24h} --json`,
adds the result to `sessions-pruned`, and escalates nonzero prune
failures. Dry-run skips this mutating CLI path because `gc session prune` has no
preview mode today.

After the safe-close backlog drained, live `ga` and `mc` still exceeded the
open-wisp alert threshold during active workflow load even though the stale
non-message counts were low. The reaper anomaly now counts only non-message
wisps older than `GC_REAPER_MAX_AGE`; fresh active workflow rows no longer page
as leak evidence. Mail wisps keep their separate optional backlog threshold.

`ga-vwnt1` fixed a deacon patrol formula leak source. The
`mol-deacon-patrol` final step used to pour the next patrol wisp, sleep for the
backoff interval, and only then burn the current wisp. A restart during that
sleep could strand the current patrol wisp open. Version 15 now resolves the
current wisp, pours and assigns the successor, burns the current wisp
immediately, then sleeps and re-enters `gc hook`; the backoff belongs to the
already-assigned successor instead of an open predecessor.

`ga-6pbt8` identified that `runControlDispatcherWithStoreAndConfig`
hard-quarantined every `ProcessControl` error except `ErrControlPending`.
That swallowed transient store/controller faults before the serve loop could use
`dispatch.IsTransientControllerError` to retry them. The dispatcher now returns
recognized transient controller errors without closing the control bead.
Deterministic hard errors, including malformed control graphs and unsupported
control kinds, still quarantine. If `ProcessControl` already terminally closed
the bead with `gc.final_disposition=controller_error`, the wrapper preserves
that disposition instead of re-closing the bead as `control_quarantined`.

`ga-eld2x` identified a persisted route-key leak for graph-v2 workflow roots.
The authoring key `gc.run_target` is useful inside formulas, but the runtime
claim path reads the persisted delivery key `gc.routed_to`. Before this patch,
graph workflow roots could persist only `gc.run_target`; scale checks could
spawn a worker for the root, but the worker could not claim it, so the work
could sit open until idle cleanup. Both graph workflow decorators now stamp
`gc.routed_to` on roots. `gc doctor --fix` also has a
`run-target-routed-to-backfill` check that repairs existing workflow roots by
copying `gc.run_target` to `gc.routed_to` when the canonical key is missing.
The hook regression coverage now encodes that boundary: run-target-only roots
are legacy repair candidates, not claimable work-query routes.
The companion reader cleanup now removes `gc.run_target` fallback from runtime
pool demand, named demand, pool assignment release, store selection, pool
desired-state wake, and workflow-run API projection readers. The graph-v2 root
decorators no longer persist `gc.run_target` on new workflow roots; they stamp
only the canonical `gc.routed_to` delivery key.
Live legacy roots were backfilled on 2026-06-01T10:58:02Z by grouping exact
`ga.wisps` workflow-root IDs through the `gc ... bd update` wrapper with
`--set-metadata gc.routed_to=<run_target>`. The first repair covered 139 `ga`
roots that still had `gc.run_target` with empty `gc.routed_to` (129 closed, 6
in progress, 4 open). A follow-up all-database scan on 2026-06-01T11:13:41Z
found 13 more legacy roots in configured rig or legacy stores (`bd=2`, `gp=1`,
`gt=5`, `my_db=5`); those were backfilled as well. Post-repair SQL across
`bd`, `ga`, `gg`, `gp`, `gt`, `mc`, `my_db`, and `rig` found `0` workflow
roots with `gc.run_target` and missing `gc.routed_to`.

`TestGastownIdleOpenBeadCountsStayBounded` now runs in Tier B nightly
acceptance. `.github/workflows/nightly.yml` schedules the Tier B job daily at
06:00 UTC and calls `make test-acceptance-b`; the Makefile target runs
`go test -tags acceptance_b -timeout 10m -v ./test/acceptance/tier_b/...`. The
test starts an isolated Gastown city with fake sessions, shortens the patrol
interval, adds a fast formula order and a fast exec order, and samples open
issue-tier and wisp-tier counts across repeated controller cycles. Local runs
keep the fast default window (`3s` warmup, `8` samples, `2s` interval). The
nightly workflow overrides that to `10s` warmup, `36` samples, and a `5s`
interval so the scheduled regression watches the idle city for roughly three
minutes. The test fails if either open-count series grows beyond a small
transient jitter window after warmup.

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
runtime. When a database is already under compaction quarantine, the command now
prints the marker path, reason, and creation timestamp so the required manual
review artifact is visible from the failed run output. Explicit
`GC_DOLT_COMPACT_ONLY_DBS` entries also seed database discovery, so an operator
can target a database even when `gc rig list` times out and the local metadata
fallback misses that rig. Pending-push dry-run now validates marker shape and
freshness before claiming it would retry a remote push, matching the live retry
guard. Legacy pending-push markers that predate the full marker contract can
self-heal only when the script can re-derive a remote, active branch/refspec,
remote branch head, and prove that remote head is reachable from the local
Dolt log. Unverified legacy markers still stop before any force-push.

The compactor now also purges per-database `.dolt/git-remote-cache`
directories before quarantine, pending-push, and commit-threshold checks. Those
directories are rebuildable fetch caches, not canonical table storage, and live
inspection found they can dwarf the actual Noms data even when a database is far
below the flatten threshold. This cleanup is independent of history flattening:
dry-run reports the cache it would purge, while a live run removes only that
cache directory and can still skip flattening for below-threshold databases.

## Creation Paths

| Path | Beads opened | Bookkeeping owner |
| --- | --- | --- |
| `internal/molecule/graph_apply.go` graph workflow instantiation | Wisp root plus logical/step wisps. Non-root graph steps are linked to the root with `tracks`; explicit ordering uses `blocks`; legacy containment uses `parent-child`. | Normal workflow execution closes runnable steps. `molecule.CloseSubtree` closes owned descendants during explicit cleanup. `reaper.sh` now closes stale leftovers when all reaper-owned dependency targets are closed. |
| `cmd/gc/order_dispatch.go` order dispatch | Ephemeral order-tracking bead labeled `gc:order-tracking`; wisp orders also create a molecule/wisp root via `molecule.Instantiate`. | `dispatchOne` defers `closeOrderTrackingBead`. Wisp roots are intentionally not auto-closed solely because descendants finish; the reaper handles stale roots/steps only when dependency evidence proves closure is safe. |
| `examples/gastown/packs/gastown/formulas/mol-deacon-patrol.toml` patrol loop | One root-only deacon patrol wisp per cycle. Each cycle pours the next patrol wisp and assigns it to the same deacon session. | Version 15 burns the current patrol wisp before the backoff sleep, then re-enters `gc hook` after sleeping. This keeps at most the successor wisp open across the backoff window. |
| Graph-v2 routing decorators in `internal/graphroute/graphroute.go` and `cmd/gc/cmd_sling.go` | Workflow roots plus routed child steps. `gc.run_target` remains a formula-authoring hint; `gc.routed_to` is the persisted claim key. | Patched roots now persist `gc.routed_to` so the runtime claim path can see them. Existing roots can be backfilled by `gc doctor --fix` through `run-target-routed-to-backfill`. |
| `cmd/gc/bead_policy_store.go` storage policy wrapper | Applies default ephemeral storage to wisp/order-tracking policies and no-history storage to session/wait/nudge policies. | Policy only selects storage tier; lifecycle is owned by the creating subsystem and maintenance scripts. |
| Session pool creation in `cmd/gc/build_desired_state.go` and lifecycle paths | Session beads, including generic ephemeral session beads for managed pools. | Session lifecycle/reconciler close or retire sessions. `reaper.sh` prunes closed `gm-*` session beads through `bd prune` and prunes terminal drained session states through `gc session prune`; orphan-sweep preserves live ephemeral session assignees. |
| Convoy and API helper paths under `cmd/gc/` and `internal/api/` | User-visible issue-tier convoys/tasks plus dependency edges such as `tracks`. | User/controller workflow owns closure. These are not age-reaped unless they are wisp-tier stale closure candidates. |

## Cleanup Paths

| Path | Responsibility | Current status |
| --- | --- | --- |
| `examples/gastown/packs/maintenance/assets/scripts/reaper.sh` | Close stale non-closed wisps with closed dependency targets; close isolated generated step-spec debris; purge old closed wisps; auto-close stale city issues; prune closed `gm-*` session beads; prune terminal drained session-state beads; escalate only stale non-message open-wisp backlog. | Patched for `parent-child`/`tracks`/`blocks` closure and purge protection through `wisp_dependencies`, plus a narrow unassigned `Step spec for ...` no-edge cleanup, a `gc session prune --state drained` pass for legacy drained-asleep session rows, and a stale-only alert query so fresh workflow load is not reported as a reaper leak. |
| `examples/gastown/packs/maintenance/assets/scripts/wisp-compact.sh` | Promote old non-closed ephemeral beads for stuck detection and delete expired closed wisps. | Still separate from the safe-close decision. It must not become an age-only closer. |
| `internal/molecule/cleanup.go` | Close molecule subtrees by ownership metadata and parent-child descendants. | Handles explicit teardown, not abandoned workflow drift. |
| `cmd/gc/wisp_gc.go` / `wisp autoclose` | Close attached workflow roots and owned workflow beads from CLI-driven cleanup. Purge expired closed wisps, order-tracking beads, and closed graph-v2 workflow-root closures. | Patched to include workflow-root closure GC through indexed metadata queries guarded by `sourceworkflow.IsWorkflowRoot`. |
| `cmd/gc/order_dispatch.go` | Close order-tracking beads after dispatch attempt completion. | Existing defer is the primary owner; stale tracking-bead bugs should be treated as order-dispatch defects. |
| `cmd/gc/doctor_run_target_backfill.go` | Mechanical repair for workflow roots with `gc.run_target` but missing `gc.routed_to`. | New `gc doctor --fix` check backfills the canonical claim key without touching non-workflow beads or already-routed roots. |
| `examples/dolt/commands/compact/run.sh` | Bound Dolt storage by flattening high-commit databases, running full GC, retrying safe pending-push/pending-GC markers, and pruning rebuildable `.dolt/git-remote-cache` directories. | Patched so remote-cache cleanup runs before commit-count skips and before blocking quarantine markers; dry-run reports the exact cache path without deleting it. |

## Verification Snapshot

- `go test ./examples/gastown -count=1` passed for the reaper and wisp-GC
  changes and the deacon patrol burn-before-backoff regression.
- `go test ./examples/gastown -run TestDeaconPatrolNextIterationBurnsCurrentBeforeBackoff -count=1`
  failed before the `mol-deacon-patrol` version 15 change and passed after it.
- `go test ./internal/session -count=1` passed for the session prune timestamp
  and drained-asleep alias changes.
- `go test ./cmd/gc -run 'TestCmdSessionPrune|TestSessionActionJSONSchema' -count=1`
  passed for the session prune CLI surface.
- `go test ./examples/dolt -count=1` passed for the compactor endpoint,
  explicit target discovery, pending-push dry-run marker checks, and safe
  legacy pending-push marker recovery.
- `go test ./examples/dolt -run 'TestCompactScript(PurgesRemoteCacheBelowThresholdWithoutFlattening|DryRunReportsRemoteCacheWithoutRemoving)$' -count=1`
  failed before the remote-cache cleanup patch and passed after it.
- `go test -tags acceptance_b -timeout 10m -v ./test/acceptance/tier_b -run TestGastownIdleOpenBeadCountsStayBounded`
  passed on 2026-06-01, proving the idle Gastown regression itself against the
  current branch. Nightly coverage is wired through
  `.github/workflows/nightly.yml` -> `make test-acceptance-b` -> the
  `acceptance_b` Tier B package, with nightly-only long-run overrides for the
  idle stability window.
- `go test -tags acceptance_b -timeout 3m ./test/acceptance/tier_b -run 'Test(IdleBeadStabilityProbeConfigReadsNightlyOverrides|GastownIdleOpenBeadCountsStayBounded)$' -count=1`
  passed for the nightly idle-probe override parser and the default-duration
  idle stability probe.
- `go test ./internal/graphroute -run 'Test(DecorateGraphWorkflowRecipe_(SetsRootMetadata|RootStampsRoutedToForClaim)|StampLegacyRecipeRouting_RespectsPerStepRunTarget)$' -count=1`
  passed for graph workflow root route stamping.
- `go test ./cmd/gc -run 'Test(BatchOnGraphWorkflowStartsWorkflowWithoutRoutingChild|DefaultScaleCheckCountsIgnoresRunTargetOnlyPersistedWork|DefaultScaleCheckCountsAndNamedDemandIgnoresRunTargetOnlyReadyWork|FilterAssignedWorkBeadsForPoolDemandIgnoresRunTargetOnlyWork|StoreForPoolAssignment_IgnoresRunTargetForStoreRouting|ComputePoolDesiredStates_IgnoresRunTargetOnlyWakeDemand|RunTargetRoutedToBackfillCheck|InstantiateSlingFormulaGraphWorkflowPreservesRoutedTo|DoctorCheckNamesGolden|CmdHookIgnoresRunTargetOnlyRoot)$' -count=1`
  passed for the sling-side route stamping, doctor backfill path, hook
  boundary, and routed_to-only runtime reader cleanup.
- Live route-key verification on 2026-06-01T11:13:41Z found `0` workflow roots
  with `gc.run_target` and missing `gc.routed_to` across `bd`, `ga`, `gg`,
  `gp`, `gt`, `mc`, `my_db`, and `rig`. Before repair, `ga` had `139` such
  roots (`129` closed, `6` in progress, `4` open), and the later all-database
  scan found `bd=2`, `gp=1`, `gt=5`, and `my_db=5`.
- `go test ./internal/api -run TestWorkflowProjectionTargetIgnoresRunTarget -count=1`
  passed for workflow-run API projection using the canonical delivery key.
- `go test ./internal/api -count=1` passed after updating order-feed workflow
  fixtures to use persisted `gc.routed_to`.
- `go test ./internal/dispatch -count=1` passed, preserving compile-time
  formula `gc.run_target` handling for fanout/control paths.
- `make dashboard-check` passed; generated dashboard TypeScript schema/types
  are in sync with the committed OpenAPI contract.
- `go vet ./...` and `git diff --check` passed.
- `.githooks/pre-commit` ran with `core.hooksPath=.githooks`; it failed in
  unrelated baseline `cmd/gc` shards. Latest log directory:
  `/data/tmp/gc-local-tests.P48tn7`.

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
  `created_at=2026-05-20T11:14:29Z`; the dry-run output now reports those marker
  details directly. That marker requires separate manual integrity review before
  live compaction or full GC can run for `mc`.
- Live session-bead inspection on 2026-06-01 found stale `mc` session wisps
  shaped as `state=asleep` plus `sleep_reason=drained` with missing `slept_at`;
  the current patch makes those rows closeable by the reaper's drained-state
  prune pass. A non-mutating SQL check at 2026-06-01T09:05:58Z found `0`
  drained-session candidates older than 24h by `updated_at` in both `ga` and
  `mc`, so no destructive prune was run. `mc` still has `21` created-old
  drained-asleep rows whose newest `updated_at` is `2026-06-01 00:40:42`;
  the reaper intentionally ages them from the terminal-state update timestamp.
- Live measurement at 2026-06-01T09:05:58Z: raw `status='open'` wisp counts
  were `ga=645` and `mc=575`, so `ga-k5ds4` remains open under its literal raw
  threshold AC. The stale leak surface was bounded: `ga` had `661`
  open/hooked/in-progress non-message rows but only `13` older than 24h; `mc`
  had `447` open/hooked/in-progress non-message rows plus `136` mail rows, with
  only `24` non-message rows older than 24h and `0` drained-session candidates
  older than 24h by `updated_at`. This is the evidence for changing the reaper
  page from total open non-message rows to stale non-message rows.
- Live re-measurement at 2026-06-01T09:54:06Z: raw `status='open'` wisp counts
  were `ga=576` and `mc=614`. The rows above 500 were still dominated by live
  workflow/mail load rather than reaper-eligible stale step wisps: `ga` open
  rows were `spec=444`, `task=129`, `convoy=3`, with only `task=9` and
  `convoy=3` older than 24h by `created_at`; `mc` open rows were `task=344`,
  `message=142`, `spec=89`, `session=39`, with only `message=78` and
  `session=24` older than 24h by `created_at`. This live city is not idle, so
  the raw threshold remains a live backlog acceptance gap rather than proof of
  an idle-city leak.
- Live route-key inspection and repair at 2026-06-01T10:58:02Z found `139`
  `ga.wisps` workflow roots with `gc.run_target` and missing `gc.routed_to`:
  `129` closed, `6` in progress, and `4` open. A later all-database scan at
  2026-06-01T11:13:41Z found 13 more legacy roots outside `ga`/`mc`: `bd=2`,
  `gp=1`, `gt=5`, and `my_db=5`. Registered rig stores were repaired through
  the `gc ... bd update` wrapper with
  `--set-metadata gc.routed_to=<run_target>`; the legacy `my_db` rows were
  repaired with the same scoped SQL predicate. Post-repair SQL found `0`
  matching rows across `bd`, `ga`, `gg`, `gp`, `gt`, `mc`, `my_db`, and `rig`.
- Branch reaper dry-run on the same live server after the stale-only alert
  patch reported `stale_wisps:115`, `mail_wisps:134`, `would_close_wisps:0`,
  and made no escalation mail call with `GC_REAPER_ALERT_THRESHOLD=500`.
- Dolt compaction remains blocked for `mc` by
  `/data/projects/maintainer-city/.gc/runtime/packs/dolt/compact-quarantine/mc`
  (`post-flatten value hash changed without row-count increase`,
  `created_at=2026-05-20T11:14:29Z`). A branch dry-run with
  `GC_DOLT_COMPACT_ONLY_DBS=ga,mc` reached that marker and failed before any GC.
- `ga` has a stale pending-push marker at
  `/data/projects/maintainer-city/.gc/runtime/packs/dolt/compact-pending-push/ga`
  (`flatten and full GC succeeded but remote push failed`,
  `created_at=2026-05-16T18:03:26Z`). The marker is also legacy/incomplete: it
  contains no `remote=` field. After the legacy-marker recovery patch, a branch
  dry-run with `GC_DOLT_COMPACT_ONLY_DBS=ga` discovers the explicit `ga` target
  even though `gc rig list` times out, derives `remote=origin`,
  `local_branch=main`, `remote_branch=main`, proves remote head
  `7kon6u7jt09nhukq4urpqc598am91u5o` is in the local `ga` Dolt log, and exits
  cleanly with `pending_push=present — dry-run (would retry remote push)`. No
  mutating remote push was run from this report pass. As of
  2026-06-01T11:24:25Z, `ga` still has a rebuildable
  `.dolt/git-remote-cache` directory of about `1.2G`; a branch dry-run reports
  it would purge that cache before retrying the pending remote push, but the
  live purge was intentionally not run because the non-dry path would also
  execute the pending-push retry. A combined
  `GC_DOLT_COMPACT_ONLY_DBS=ga,mc` dry-run now exits nonzero only because of the
  remaining `mc` quarantine; `ga` reaches the recoverable dry-run retry path.
- Live remote-cache remediation on 2026-06-01T11:24:25Z used the branch
  compactor against `/data/services/gascity-local-dolt` with
  `GC_DOLT_MANAGED_LOCAL=0` and explicit `127.0.0.1:3307`. Dry-run first proved
  `gt` would only purge
  `/data/services/gascity-local-dolt/gt/.dolt/git-remote-cache` and then skip
  flattening at `133` commits. The live run purged that cache and left `gt`
  unchanged at `133` commits, `0` status rows, `15` issues, `1381` wisps, and
  database hash `3mkjngdd1bt41a7absg1acpngnkbkht4`. Additional live runs
  purged remote caches for `mc`, `bd`, `gp`, and `my_db`; `mc` still stopped on
  its integrity quarantine before any GC. Local Dolt storage dropped from about
  `21G` before the `gt` purge to `3.1G` after the cache purges. The remaining
  per-database sizes are dominated by `ga=2.3G` and `mc=1.8G`, with only `ga`
  still showing a `.dolt/git-remote-cache` directory.
