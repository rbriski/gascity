# Handoff — finish the non-work field-door cleanup (P4–P6) + open PR #3839

## SESSION UPDATE 2026-06-30 — corrected scope + progress (read this first)

The original framing below ("P4 is a *safe mechanical migration*") is **only
true for the localized sites**. The audit found, and this session confirmed, that
the **bulk of P4 is a coupled type cascade**, not isolated field swaps:

- The session snapshot is handed across function boundaries as **`[]beads.Bead`**
  into the pool-demand / work-scope / desired-state engine. To stop the leak,
  those signatures must flip to **`[]session.Info`** atomically (you cannot
  half-change a signature). The connected component spans ~8 files:
  `pool_desired_state.go` (`ComputePoolDesiredStates*`,
  `canonicalSingletonAliasHeldTemplates`, `poolInFlightNewRequests`),
  `assigned_work_scope.go` (`filterAssignedWorkBeadsForPoolDemand`,
  `sessionAgentConfig`), `session_reconcile.go` (`capWakeConfigByDemand`,
  `applyDependencyWakeReasons`, `preferredDependencySessions`, `topoOrder`),
  `pool_session_name.go` (`GCSweepSessionBeads`), `usage_compute.go`
  (`emitDueComputeFacts`), plus the `build_desired_state.go` / `city_runtime.go`
  / `cmd_start.go` callers that pass `Open()` in.
- The **reconciler threads `*beads.Bead session`** through dozens of helpers
  (`healState`, `checkStability`, `checkChurn`, …). Converting
  `isNamedSessionBead(*session)` → `isNamedSessionInfo(info)` means the reconciler
  must carry the `Info` alongside/instead of the bead — another cascade.
- Several sites need **new foundation first** (additive `Info` fields + a sibling
  + an equivalence case): `started_config_hash` (soft_reload), the MCP-key
  cluster + `beadUsesACPTransportInfo` (providers `observedACPSessionNames`),
  `Status`/`Assignee`/raw-metadata-map for `sessionBeadSnapshotFingerprint`
  (city_runtime), and `Info` forms of `sessionCoreConfigForHash`,
  `lookupSessionBeadByID`, `IsSessionBeadOrRepairable`, the soft-reload drain
  helpers, the wait-nudge helpers, and the `*ForAgent` family
  (`isManualSessionBeadForAgent`/`isEphemeralSessionBeadForAgent`/
  `isLegacyManualSessionBeadForAgent`), `sessionAgentMetricIdentity`,
  `existingPoolSlot`, `namedSessionMode`/`Identity`/`ContinuityEligible`,
  the wake helpers, `isRetiredSessionModelOwner`.

**Foundation correction:** the original handoff undercounted the existing `*Info`
siblings (the `session` vs `sessionpkg` import alias hid them). There are **23**
siblings already; `isNamedSessionInfo`, `isFailedCreateSessionInfo`,
`infoOwnsPoolSessionName`, `isPendingPoolCreateInfo`,
`sessionBeadAssigneeIdentitiesInfo` all exist. The list above is what is *still*
missing.

**Done this session (all byte-identical, build+equivalence-green, committed,
pushed):** the localized snapshot-consumer sites — trace open-counts,
`template_resolve`, `city-status` Find* lookups, `cmd_wait` wait-diag loops,
`city_runtime` `poolSweepWouldDrain`, `openSessionNameTaken`, the reaper
`FindInfoByID`. **P6 read-guard landed:**
`TestSnapshotInfoOnlyFilesStayOnInfoAccessors` (in `frontdoor_di_guard_test.go`)
pins the 4 files that are now fully accessor-free (`template_resolve`,
`session_name_lookup`, `cmd_citystatus`, `session_reconciler_trace_cycle`);
add files to `snapshotInfoOnlyFiles` as each becomes accessor-free. Shared
contract for the per-file work: `P4-CONVERSION-CONTRACT.md` (this directory).

**Remaining raw-accessor surface (~33 uses):** `city_runtime.go` (10),
`build_desired_state.go` (10), then `session_beads.go`, `named_sessions.go`
(session-pkg `[]beads.Bead` API — needs an `Info`-returning
`FindCanonicalNamedSession`/`FindNamedSessionConflict`), `cmd_wait.go` (2
FindByID → wait-nudge cascade), `cmd_start.go`, `soft_reload.go`,
`session_lifecycle_parallel.go`, `providers.go`, `nudge_dispatcher.go`,
`city_status_snapshot.go` (1 each). Nearly all are the cascade or a
foundation-gap above — **not** further free swaps.

**Recommended next-session order:** (1) the providers MCP-key vertical slice as
a worked example of the full add-field→sibling→equivalence→convert pattern; (2)
the pool-demand `[]beads.Bead`→`[]session.Info` cascade (biggest unlock); (3) the
reconciler `*session` Info-threading; (4) P5 `closeBead`; (5) finish P6 deletion
+ widen the guard. Each cascade is one atomic, carefully-reviewed change — do
**not** fan parallel agents at a single connected component.

---


**Goal of the remaining work:** make direct reads of metadata/bead-fields on
non-work objects (session/nudge/mail/order/graph) *impossible to compile*. Then
mark **PR #3839** ready and label it `status/needs-review-auto`.

Read first: `NONWORK-BEAD-FIELDDOOR-PLAN.md` (the architecture + 4-layer model)
in this directory. This doc is the **execution guide for P4–P6**.

## Where things stand (start here)

- **Branch:** `upstream/object-front-doors-cleanup`, base `main`, **PR #3839 (DRAFT)**.
  Worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`.
- **HEAD `dd5496c16`** (pushed). Run `go build ./...` + the two equivalence
  tests to confirm a green baseline before starting:
  `go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence' -count=1`.
- **Foundation DONE (P1–P3), additive + byte-identical + equivalence-proven:**
  - P1 `a955e821f` — `session.Info` carries the full consumed session-attribute
    set (identity/pool/named + state/bookkeeping). `InfoFromPersistedBead`
    projects it. **Fidelity-trap fields** exist on purpose: `MetadataState` (raw,
    not normalized/closed-blanked), `SessionNameMetadata` (no `sessionNameFor(ID)`
    fallback), `ManualSessionMetadata` (untrimmed). Use the RAW field whenever the
    original classifier read raw metadata.
  - P2 `6399b8305` — **22 `*Info` classifier siblings** (e.g. `isPoolManagedSessionInfo`,
    `sessionOriginInfo`, `resolvedSessionTemplateInfo`, `sessionBeadAgentNameInfo`,
    `isDrainedSessionInfo`, `isNamedSessionInfo`, `isManualSessionInfo`,
    `isFailedCreateSessionInfo`, `isStaleCreatingInfo`, `isKnownStateInfo`,
    `sessionMetadataStateInfo`, `beadOwnsPoolSessionNameInfo`,
    `stampedPoolQualifiedIdentityInfo`, `sessionBeadAssigneeIdentitiesInfo`,
    `sessionWakeAttemptsInfo`, `sessionIsQuarantinedInfo`,
    `isPoolSessionSlotFreeableInfo`, `infoIdentifiesAsCanonical`, …). Originals
    UNCHANGED. `TestSessionClassifierInfoEquivalence` proves `isX(b) ==
    isXInfo(InfoFromPersistedBead(b))` over 25 bead shapes.
  - P3 `dd5496c16` — snapshot `*Info` accessors: `OpenInfos() []session.Info`,
    `FindInfoByID`, `FindInfoByTemplate`, `FindInfoByNamedIdentity`. Lockstep
    `openInfos` field (via `replaceOpenLocked`). Bead methods (`Open()`,
    `FindByID`, `FindSessionBeadBy*`) UNCHANGED. `TestSessionSnapshotInfoEquivalence`.

**The leverage:** because the `*Info` forms are equivalence-proven against the
originals, P4 is a *safe mechanical migration* — swap each consumer's raw read
for the typed form; behavior cannot change if the swap is faithful.

## P4 — migrate the ~167 consumer reads (the bulk)

**Mechanical rule, per consumer site:**
- `snapshot.Open()` → `snapshot.OpenInfos()`; the loop var is now `session.Info`.
- `snapshot.FindByID/FindSessionBeadByTemplate/FindSessionBeadByNamedIdentity` →
  `FindInfoByID/FindInfoByTemplate/FindInfoByNamedIdentity` (return `Info`).
- `b.Metadata["x"]` / `b.Status` / `b.Title` / `b.Labels` on a **session** bead →
  the matching `info.Field` (mind the fidelity-trap RAW fields).
- `isX(bead)` / `sessionOrigin(bead)` / `resolvedSessionTemplate(bead,cfg)` →
  `isXInfo(info)` / `sessionOriginInfo(info)` / `resolvedSessionTemplateInfo(info,cfg)`.

**Caveats that make it coupled (not blind find-replace):**
1. **Not compiler-forced** — the bead methods/classifiers still exist, so a
   missed site silently keeps leaking. After each file, grep to verify:
   `grep -nE '\.Metadata\[|\.Open\(\)|isPoolManagedSessionBead\(|sessionOrigin\(|resolvedSessionTemplate\(' <file>`
   and confirm every remaining hit is a WORK bead or genuinely raw-by-design.
2. **Bead-passing helpers** — if a consumer passes a session bead to a *non-classifier*
   helper that takes `beads.Bead`, that helper must also gain an `Info` form
   (extend the P2 pattern) before the consumer can drop the bead. Convert the
   helper, re-prove equivalence, then migrate the caller.
3. Some consumers need BOTH the bead and Info (e.g. they read fields AND pass the
   bead to a work/by-id op). Those stay mixed; only the *field reads* convert.

**Suggested shard order (one sub-agent per file, verify each, commit per file or
small cluster):** start lower-risk → `cmd_wait.go`, `providers.go`,
`nudge_dispatcher.go`, `adoption_barrier.go`, `soft_reload.go`,
`session_name_lookup.go` (also closes 1 of the residual `.Store().Store`); then
the controller core → `build_desired_state.go`, `city_runtime.go`,
`session_beads.go`, `session_reconciler.go`, `session_reconcile.go`,
`session_lifecycle_parallel.go`, `city_status_snapshot.go`,
`session_reconciler_trace_cycle.go`, `template_resolve.go`.
**Oracle:** the existing reconciler/session/snapshot suites + the P2/P3
equivalence tests. The build host is oversubscribed (`fork/exec: resource
temporarily unavailable`) — CI on dedicated runners is the byte-identical gate;
locally run targeted `-run` filters + the equivalence tests.

**Residual `.Store().Store` (6) closed across P4/P5:** `adoption_barrier.go`
(ListAllSessionBeads → add an `Info`-returning list form, or use the snapshot),
`soft_reload.go` (loadSessionBeadSnapshot consumers → `OpenInfos`),
`session_name_lookup.go` (createPoolSessionBead — P5/D), `session_beads.go`
(cancel — P5), `session_lifecycle_parallel.go` (closeBead — P5).

## P5 — the `closeBead` cross-class split (LANDMINE — do isolated, last)

`closeBead(store, id, reason, now, stderr)` (`session_beads.go`) decomposes into:
- SESSION: idempotence `Get` (skip if `Status=="closed"`) + `ClosePatch` +
  `CloseWithoutReason` — **`InfoStore.Close(id, stateCode, now)` already bundles
  exactly this** (and deliberately omits work-release).
- EXTMSG: `cancelStateAssignedToRetiredSessionBead` (waits + slack bindings —
  `session.CancelWaits` + `extmsg.CloseSessionBindings`, both exist).
- WORK: `releaseWorkFromClosedSessionBead` (the `workAssignment` façade — exists).
**Order is close-THEN-release; preserve the skip-if-already-closed idempotence**
(it prevents the bead.updated event storm across the 3 reconciler close paths).
Prove the exact bead-op sequence with a recording-fake store. The
`session_lifecycle_parallel.go:closeBead(sessFront.Store().Store, …, StateFailedCreate)`
site is the failed-create branch (delegates to `closeFailedCreateBead`).
Also tidy `createPoolSessionBead` (P-D): thread `sessFront`; `CreateSession`/
`CreateSpec` already exist and are used.

## P6 — close it out + enforce

1. Migrate any remaining non-work classes (the audit's broad sweep found ~47
   reads beyond session — confirm nudge/mail/order; most route through their
   front doors already).
2. **Delete the now-dead bead paths**: once every caller uses the `Info` forms,
   remove the bead classifiers/`Open()`/`FindSessionBeadBy*`/the `open []beads.Bead`
   source (snapshot holds only `[]Info`). The equivalence tests can then be
   deleted or repurposed.
3. **Tighten the arch guard** (`cmd/gc/frontdoor_di_guard_test.go` exists from
   #3800): extend `frontDoorForbiddenInStoreFreeFiles` to also forbid
   `.Store().Store` and, in the fully-converted files, raw session-bead
   `b.Metadata[`/`.Status`/`.Labels` reads. Add the converted consumer files to
   the guarded set as each becomes raw-free.

## Finish

- Full gates: `go build ./...`, `go vet ./...`, `make lint`/golangci-lint,
  `make check-docs` (none expected — wire byte-identical), and the sharded test
  targets. `git checkout go.sum` after builds; commit `--no-verify` (stale
  hooksPath); trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Push; **watch CI green on #3839**: `gh pr checks 3839 --watch`.
- Only when CI is green: mark ready + label (gh pr ready/edit ABORT on the
  projectCards deprecation — use the API):
  - ready: `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
  - label: `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

## Invariants (hold throughout)

Wire byte-identical (empty openapi/docs-schema/generated-TS diff; `Info`
additions stay internal-only); runtime byte-identical (the equivalence tests +
recording-fake are the oracle); no typed-nil traps; never `tmux kill-server`;
never `go clean -cache` (`-testcache` ok); gascity Dolt is LOCAL-ONLY (no `bd
dolt push`).
