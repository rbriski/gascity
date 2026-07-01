# Handoff — finish the non-work field-door cleanup: reconciler cascade → P5 → P6 → ready

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`,
**HEAD `e6f3d2b1e`** (pushed). This is the authoritative "finish it out" guide.
For the completed history read `P4-CASCADE-HANDOFF.md`; the architecture is in
`NONWORK-BEAD-FIELDDOOR-PLAN.md`; the per-site swap rules are in
`P4-CONVERSION-CONTRACT.md`.

**Hard rule (owner directive):** a direct read of metadata/bead FIELDS on any
NON-WORK object (session/nudge/mail/order/graph) is illegal — only generic WORK
beads read raw. This is the precondition for a per-class backend swap.

## Confirm a green baseline first

```
go build ./cmd/gc/ ./internal/session/
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence|TestNudgeTargetInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree' -count=1
go test ./internal/session/ -run 'TestNamedSessionInfoEquivalence' -count=1
git checkout go.sum   # churns spuriously on go test
```

## Where the migration stands

Raw-accessor surface is **20** non-test sites (was 33). Foundation P1–P3 + the
pool-demand cascade + five small cascades are DONE and guard-pinned where the
file is fully accessor-free (`template_resolve.go`, `session_name_lookup.go`,
`cmd_citystatus.go`, `session_reconciler_trace_cycle.go`, `providers.go`,
`nudge_dispatcher.go`, `named_sessions.go`). The Info codec
(`internal/session/info_store.go:InfoFromPersistedBead`) already carries every
consumed session attribute (incl. the fidelity-trap raw mirrors `MetadataState`,
`SessionNameMetadata`, `ManualSessionMetadata`, `TransportMetadata`, and
`Type`/`ContinuityEligible`). ~25 `*Info` classifier siblings exist.

**The 20 remaining sites almost all block on ONE thing: the reconciler's
`*beads.Bead session` working copy and its `*ForAgent` classifier family.**

## The reconciler cascade (the primary unlock — do this first)

### What it actually is (read this before touching code)

The reconcile tick does NOT hold a raw bead as a read-leak — it holds a
`*beads.Bead session` as a **mutable per-tick working copy**. The pattern
(see `healStateWithRollback`, `session_reconcile.go:1025`):

```go
batch := healStatePatchWithRollback(*session, alive, clk, …)   // PURE: reads fields, returns map[string]string
sessFront.ApplyPatch(session.ID, batch)                        // WRITE: through the InfoStore front door (already correct)
for k, v := range batch { session.Metadata[k] = v }            // LOCKSTEP: mutate the working copy so later reads this tick see the heal
```

So there are three concerns, and only the first is a field-door leak:

1. **Pure reads off `*session`** — the decision classifiers
   (`healStatePatch(*session…)`, `isKnownState`, `sessionMetadataState`,
   `sessionWakeAttempts`, `sessionIsQuarantined`, `staleCreatingState`,
   `pendingCreate*`, `isPoolExcess`, `productiveLongEnough`, `stableLongEnough`,
   `sessionStartRequested`, `sessionWithinDesiredConfig`, `sessionExitFacts`,
   `sessionHasProviderTerminalError`, …). Many already have `*Info` siblings.
   **These are the leak — route them through a `session.Info`.**
2. **Writes** — already go through `sessFront *session.InfoStore.ApplyPatch`
   (the write front door). Nothing to do; do NOT reintroduce raw store writes.
3. **The lockstep mutation** of `session.Metadata[k]=v` so intra-tick reads see
   prior heals. This is the ONLY reason the bead is `*` and not a value.

### Recommended approach — make `session.Info` the per-tick working copy

Convert the reconcile driver so the loop variable is a mutable `session.Info`
(not `*beads.Bead`). Each mutating helper:
- takes the working `*session.Info` + `sessFront`,
- computes the same `batch` from an **Info-form patch computer**
  (`healStatePatchInfo(info, …)`, etc. — mirror the existing
  `healStatePatch(beads.Bead,…)` byte-for-byte, proven by an equivalence
  oracle),
- writes via `sessFront.ApplyPatch(info.ID, batch)` (unchanged), then
- **re-projects or lockstep-applies** the batch onto the working Info so
  intra-tick reads converge — either re-run `InfoFromPersistedBead` on a
  patched bead, or (cheaper, byte-identical) add a small
  `Info.applyMetadataPatch(batch)` that maps the same keys the pure classifiers
  read (state→MetadataState/State, wake_attempts→WakeAttempts, …). Prove the
  lockstep equivalence with a recording test: apply batch to a bead + re-project
  vs. apply to Info directly → equal.

The pure classifiers then take `Info`. The driver functions
(`reconcileSessionBeads*`, `session_reconciler.go:800–942`) hand the working
`Info` down. Writes still funnel through `sessFront`, so the wire/runtime
byte-identity oracle is the existing reconciler/pool E2E suite + a recording
fake.

**Sequence it bottom-up, ONE atomic reviewed commit per coherent cluster
(do NOT fan parallel agents at this connected component):**
1. Add the Info-form patch computers + pure-classifier siblings that don't yet
   exist, each with an equivalence case (extend
   `TestSessionClassifierInfoEquivalence` / add a patch-equivalence test).
2. Flip the mutating spine (`healState`/`healStateWithRollback`/`checkStability`/
   `checkRateLimitStability`/`checkChurn`/`markProviderTerminalError`/
   `record*`/`clear*`/`healExpiredTimers`/`recordRateLimitQuarantine`/
   `markDrainAckStopPending`/`resetPendingCommittedAt`) to the working-Info model
   + the driver loop, with all callers, in the same commit.
3. Add the `*ForAgent` family Info forms (below) — they gate the desired-state
   loops.

### The `*ForAgent` classifier family (needed by the desired-state loops)

These have NO Info form yet (`session_origin.go` / `session_name_lookup.go`):
`isManualSessionBeadForAgent`, `isEphemeralSessionBeadForAgent`,
`isLegacyManualSessionBeadForAgent`, `sessionAgentMetricIdentity`.
(`existingPoolSlot` already has `existingPoolSlotInfo`.) Add Info siblings +
equivalence cases. Once they exist, the `build_desired_state.go` recovery loop
(~2079) and the `city_runtime.go` sweep loop (~2658) convert (the remaining
reads on those loops are `.Closed`/`.MetadataState`/`isFailedCreateSessionInfo`/
`resolvedSessionTemplateInfo`/`isPoolManagedSessionInfo`, which all exist).

## After the reconciler cascade — the rest of the 20 sites

Convert as their blocking helper gains an Info form; add each newly
accessor-free file to `snapshotInfoOnlyFiles`:

- `build_desired_state.go` 2079 (recovery loop) — unblocked by the `*ForAgent`
  family. 3341/3570/3816/4165 return/append raw `[]beads.Bead` candidate slices
  sorted + threaded into pool-create planning; these stay raw unless that
  planning takes Info (assess — likely a separate, later slice; contract rule 3).
- `session_beads.go` 2033 — read the loop; convert if pure, else leave (rule 3).
  `session_beads.go:57` returns `Open()` as `[]beads.Bead` to raw callers — a
  return-type cascade; do with its callers or leave.
- `city_runtime.go` 2658 (sweep) — unblocked by the `*ForAgent` family + a
  `pendingCreateClaimStillLeasedForSweep` Info form; note it threads into
  `GCSweepSessionBeads` (a store close op) — only the field-read guard converts,
  the close stays on the store.
- `city_runtime.go` 3056 (`filterSessionBeadsByName`) — its caller (2896) feeds
  `newSessionBeadSnapshot(open)` + the raw-bead reconciler; converts only once
  `reconcileSessionBeadsAtPathWithNamedDemand` takes Info (part of this cascade).
- `soft_reload.go` 103 — needs a `template_overrides` (or raw-metadata-map)
  accessor on `Info` for `sessionCoreConfigForHash`→`applyTemplateOverridesToConfig`
  (`sessionpkg.ParseTemplateOverrides(session.Metadata)`), plus Info forms of the
  drain helpers (`clearSoftReloadConfigDriftDrainAck`/
  `cancelSoftReloadConfigDriftDrain`→`cancelSessionConfigDriftDrain`).
- `cmd_wait.go` 1164 (ready-wait-nudge loop) — the wait-nudge helper family
  (`cachedSessionCanReceiveWaitNudge`, `waitNudgeAgent`, `sessionProviderFamily`,
  `waitNudgePollerKey`) needs Info forms; then convert the loop. Note the loop
  also does store ops (`startNudgePoller`, `enqueueQueuedNudge`) — those stay.
- `cmd_start.go` 904/918, `city_runtime.go` 1159/2246/2158,
  `session_lifecycle_parallel.go` 809 — thread the raw `open []beads.Bead` into
  store/reconciler/`resolvePreservedConfiguredNamedSessionTemplate` ops; stay raw
  (rule 3) until those ops take Info, or leave as documented allowances.

### RAW-BY-DESIGN — do NOT convert (not leaks)

- `city_status_snapshot.go:411` `countCitySessionsFromSnapshot` — needs
  `IsSessionBeadOrRepairableInfo` (now exists) but its fidelity hinges on the
  snapshot-only-holds-session-beads invariant; prove that invariant before ever
  touching it.
- `city_runtime.go:2153` `emitDueComputeFacts` — usage-bookkeeping metadata
  (`awake_started_at`/`slept_at`/`usage_compute_emitted_at`), not session
  identity.
- `city_runtime.go:3217` `sessionBeadSnapshotFingerprint` — hashes
  ID/Status/Assignee/ALL raw metadata; a whole-bead change fingerprint, not a
  session-attribute read.
- `session_bead_snapshot.go` — the codec edge (`newSessionBeadSnapshot`); always
  EXEMPT.

## P5 — the `closeBead` cross-class split (LANDMINE — isolated, last)

`closeBead(store, id, reason, now, stderr)` in `session_beads.go` decomposes into
SESSION close (`InfoStore.Close` — bundles skip-if-closed idempotence +
ClosePatch + CloseWithoutReason; deliberately OMITS work-release), EXTMSG
(`cancelStateAssignedToRetiredSessionBead` = `session.CancelWaits` +
`extmsg.CloseSessionBindings`), and WORK release (the `workAssignment` façade).
Order is **close-THEN-release**; **preserve skip-if-already-closed idempotence**
(it prevents the bead.updated storm across the 3 reconciler close paths). Prove
the exact op sequence with a recording-fake store. Also tidy
`createPoolSessionBead` to thread `sessFront` (`CreateSession`/`CreateSpec`
exist).

## P6 — close it out + enforce

1. As each consumer file becomes accessor-free, add it to `snapshotInfoOnlyFiles`.
2. Once every caller uses the Info forms, delete the now-dead bead classifiers /
   `Open()` / `FindSessionBeadBy*` — the snapshot codec edge
   (`newSessionBeadSnapshot`) legitimately keeps raw classifiers; it is EXEMPT.
3. Extend the guard to also forbid `.Store().Store` in the fully-converted files.

## Method (proven this stack)

Keep each original classifier UNTOUCHED + ADD the typed sibling + ADD an
equivalence case (byte-identical oracle), THEN flip the signature with ALL its
callers in the SAME commit. `snapshot.OpenInfos()[i]` is the precomputed
projection of `Open()[i]`, so raw and Info slices coexist during partial
migration — a full-component atomic flip is not required (except the reconciler
mutation spine, which must flip together). For foundation gaps, add the Info
field + codec population + equivalence case BEFORE the site that needs it. Test
call sites project fixtures via `sessionInfosFromBeads([]beads.Bead) []session.Info`.

## Build / commit / gate hygiene

- `git checkout go.sum` after builds; commit AND push with `--no-verify` (stale
  hooksPath + the pre-push hook runs the full suite and times out — run gates
  manually). Trailer:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Never `tmux kill-server`; never `go clean -cache` (`-testcache` ok; cold build
  `GOCACHE=$(mktemp -d)`); gascity Dolt is LOCAL-ONLY (no `bd dolt push`).
- Gates: `go build ./...` · `go vet ./...` ·
  `golangci-lint run ./cmd/gc/... ./internal/session/...` (0) · the equivalence +
  guard tests · targeted subject suites (reconcile/pool/wait). The build host is
  oversubscribed — run targeted `-run` locally; CI on dedicated runners is the
  byte-identical gate. **`make dashboard-check` not needed** (no `internal/api`
  wire change; `Info` additions stay internal — empty openapi/docs-schema/
  generated-TS diff).

## Finish (only when #3839 CI is verified GREEN — no premature ready)

- `gh pr checks 3839 --watch`
- ready (gh pr ready aborts on projectCards — use the API):
  `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
- label:
  `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

**Done =** every non-work consumer reads via `session.Info` (grep-clean of raw
snapshot accessors + `.Store().Store`), the guard forbids regression, full gates
+ #3839 CI green, #3839 ready + labeled. Update `memory/infra-beads-decoupling-plan.md`.
