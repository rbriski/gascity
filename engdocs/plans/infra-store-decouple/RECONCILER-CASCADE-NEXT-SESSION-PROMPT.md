# Next-session prompt — finish the non-work field-door cleanup (reconciler cascade → ready)

Paste the block below into a fresh session.

---

Finish the non-work-bead field-door cleanup on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `e6f3d2b1e`).

**Read first, in order:** `engdocs/plans/infra-store-decouple/RECONCILER-CASCADE-HANDOFF.md`
(the authoritative "finish it out" guide — grounded surface, the reconciler
working-copy model, the remaining-20-sites unblock map, P5/P6, ready), then
`P4-CONVERSION-CONTRACT.md` (per-site swap rules + sibling table + RAW
fidelity-field rules) and `NONWORK-BEAD-FIELDDOOR-PLAN.md` (architecture).
`P4-CASCADE-HANDOFF.md` is the completed-history record.

Confirm a green baseline:
```
go build ./cmd/gc/ ./internal/session/
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence|TestNudgeTargetInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree' -count=1
go test ./internal/session/ -run 'TestNamedSessionInfoEquivalence' -count=1
git checkout go.sum
```

**Principle (hard rule):** direct read of metadata/bead FIELDS on any NON-WORK
object (session/nudge/mail/order/graph) is illegal — only generic WORK beads read
raw. This is the precondition for a per-class backend swap.

**What's DONE:** foundation P1–P3, the pool-demand cascade, and five small
cascades (nudge dispatcher, named-session lookups, the 3 pure build_desired_state
loops, the wait config-drift loop). Raw-accessor surface is **20** non-test
sites. The Info codec carries every consumed session attribute (incl. the
fidelity-trap raw mirrors + `Type`/`ContinuityEligible`/`TransportMetadata`); ~25
`*Info` classifier siblings exist; 7 files are guard-pinned in
`snapshotInfoOnlyFiles`.

**What REMAINS (in order — each is ONE atomic, carefully-reviewed change; do NOT
fan parallel agents at a single connected component):**

1. **The reconciler `*beads.Bead session` cascade — the primary unlock.** The
   reconcile tick holds a `*beads.Bead session` as a MUTABLE per-tick working
   copy (not a read-leak): pure classifiers read its fields, `sessFront.ApplyPatch`
   writes (already the front door), then `session.Metadata[k]=v` lockstep-mutates
   so intra-tick reads see the heal (see `healStateWithRollback`,
   `session_reconcile.go:1025`). Make `session.Info` the working copy: add
   Info-form patch computers (`healStatePatchInfo`, …) + the missing pure
   classifiers, each with an equivalence oracle; flip the mutating spine
   (`healState`/`checkStability`/`checkRateLimitStability`/`checkChurn`/
   `markProviderTerminalError`/`record*`/`clear*`/`healExpiredTimers`/…) + the
   drivers (`reconcileSessionBeads*`, `session_reconciler.go:800–942`) together;
   re-project or lockstep-apply the batch onto the working Info (prove
   apply-to-bead-then-project == apply-to-Info with a recording test). Then add
   the `*ForAgent` Info forms (`isManualSessionBeadForAgent`/
   `isEphemeralSessionBeadForAgent`/`isLegacyManualSessionBeadForAgent`/
   `sessionAgentMetricIdentity`; `existingPoolSlotInfo` already exists).
2. **The rest of the 20 sites**, as each blocking helper gains its Info form —
   the recovery loop (`build_desired_state.go` ~2079), the sweep loop
   (`city_runtime.go` ~2658), `filterSessionBeadsByName` (~3056), `soft_reload.go`
   103 (needs a `template_overrides`/raw-metadata accessor on Info + the drain
   helpers' Info forms), `cmd_wait.go` 1164 (the wait-nudge helper family). Leave
   the rule-3 store/candidate-slice sites raw (they thread the bead into store
   ops or raw `[]beads.Bead` helpers). Add each newly-accessor-free file to
   `snapshotInfoOnlyFiles`.
3. **P5 `closeBead` cross-class split** (LANDMINE — isolated, last; recording-fake
   oracle; close-THEN-release; preserve skip-if-already-closed idempotence).
4. **P6** delete dead bead classifiers/`Open()`/`FindSessionBeadBy*` (codec edge
   `session_bead_snapshot.go` is EXEMPT) + widen the guard to forbid
   `.Store().Store` in converted files.

**DO NOT convert (RAW-BY-DESIGN, not leaks):** `city_status_snapshot.go:411`
(`countCitySessionsFromSnapshot` — prove the snapshot invariant first),
`city_runtime.go:2153` (`emitDueComputeFacts` — usage bookkeeping),
`city_runtime.go:3217` (`sessionBeadSnapshotFingerprint` — hashes ALL raw
metadata), and the `session_bead_snapshot.go` codec edge.

**Method (proven):** keep each original classifier untouched + ADD the typed
sibling + ADD an equivalence case (byte-identical oracle), THEN flip the
signature with ALL callers in the SAME commit (the reconciler mutation spine is
the one cluster that must flip together). `OpenInfos()[i]` is the precomputed
projection of `Open()[i]`, so raw + Info slices coexist during partial migration.
For foundation gaps, add the Info field + codec population + equivalence case
BEFORE the site that needs it. Test call sites project fixtures via the package
helper `sessionInfosFromBeads([]beads.Bead) []session.Info`.

**Build/commit hygiene:** `git checkout go.sum` after builds; commit AND push with
`--no-verify` (stale hooksPath + the pre-push hook runs the full suite and times
out — run gates manually). Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is
LOCAL-ONLY (no `bd dolt push`).

**Gates before ready:** `go build ./...` · `go vet ./...` ·
`golangci-lint run ./cmd/gc/... ./internal/session/...` (0) · the equivalence +
guard tests · targeted subject suites (reconcile/pool/wait). `make dashboard-check`
not needed (`Info` additions stay internal — empty openapi/docs-schema/
generated-TS diff). The build host is oversubscribed — targeted `-run` locally;
CI on dedicated runners is the byte-identical gate.

**Finish (only when #3839 CI is verified GREEN — no premature ready):**
- `gh pr checks 3839 --watch`
- ready (gh pr ready aborts on projectCards — use the API): `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
- label: `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

**Done =** every non-work consumer reads via `session.Info` (grep-clean of raw
snapshot accessors + `.Store().Store`), the guard forbids regression, full gates
+ #3839 CI green, #3839 ready + labeled. Update
`memory/infra-beads-decoupling-plan.md`.

---
