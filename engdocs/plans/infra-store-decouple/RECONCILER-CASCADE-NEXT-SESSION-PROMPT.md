# Next-session prompt — finish the non-work field-door cleanup (recovery-loop slice + reconciler spine → ready)

> **The reconciler spine flip now has dedicated, self-contained docs** (design =
> Fork B, verified scope, incremental order): read
> **`SPINE-FLIP-HANDOFF.md`** and paste **`SPINE-FLIP-NEXT-SESSION-PROMPT.md`**
> for that work. The block below is the older broad-cascade prompt (recovery loop
> DONE; spine flip is the remaining major item).

Paste the block below into a fresh session.

---

Finish the non-work-bead field-door cleanup on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `69ccc13c6`).

**Read first, in order:** `engdocs/plans/infra-store-decouple/RECONCILER-CASCADE-HANDOFF.md`
(the authoritative "finish it out" guide — the verified 21-site census, the
reconciler-spine flip cluster + Phase 2 lockstep requirement, the re-projection-
primitive blocker, and the foundation tiers), then `P4-CONVERSION-CONTRACT.md`
(per-site swap rules + sibling table + RAW fidelity-field rules) and
`NONWORK-BEAD-FIELDDOOR-PLAN.md` (architecture). `P4-CASCADE-HANDOFF.md` is the
completed-history record.

Confirm a green baseline:
```
go build ./cmd/gc/ ./internal/session/
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence|TestNudgeTargetInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree|TestSweepUndesiredPoolSessionBeads' -count=1
go test ./internal/session/ -run 'TestNamedSessionInfoEquivalence' -count=1
git checkout go.sum
```

**Principle (hard rule):** direct read of metadata/bead FIELDS on any NON-WORK
object (session/nudge/mail/order/graph) is illegal — only generic WORK beads read
raw. This is the precondition for a per-class backend swap.

**Verified state (workflow `wf_7f806124-bcd`, adversarially cross-checked):** raw-
accessor surface is **20** non-test sites (was 21; the recovery loop was converted
— see below). The Info codec is RICH (already projects
state/sleep_reason/pool_slot/named/manual/health/lease clusters). ~30 `*Info`
siblings exist, incl. the whole pending-create lease family and the `*ForAgent`
family (both DONE this migration). The pool sweep loop AND the pool recovery loop
are DONE. Do NOT re-trust any agent that says "the `*Info` siblings don't exist" —
that was a stale out-of-tree checkout; pin `git rev-parse HEAD` if you re-run a
mapping agent.

**DONE (commit `1dbf692e7`): the recovery-loop slice.**
`discoverSessionBeadsWithRoots` (`build_desired_state.go:2079`) now iterates
`OpenInfos()` and recovers the raw bead via `FindByID(info.ID)` only for the
identity chain (`sessionBeadQualifiedName`, `canonicalSessionIdentityWithConfig`,
`resolveTemplateForSessionBead` — rule 3). Its two foundation siblings
(`scaleCheckPartialSessionPreservableInfo`, `staleNonExpandingPoolSessionBeadInfo`,
the latter equivalence-cased with a canonical-singleton agent fixture) landed and
were adversarially cross-verified byte-identical. This is the reference shape for
the remaining loops.

**What REMAINS (in recommended order — each is ONE atomic, carefully-reviewed
change; do NOT fan parallel agents at the reconciler connected component):**

1. **The reconciler spine flip — THE primary unlock (DESIGN = Fork B; Tier-0 reset
   fields DONE `69ccc13c6`; genuinely a 3–5 session effort — do NOT rush).** Read
   the "SESSION UPDATE 2026-07-01 (CONT-5)" banner atop the spine section of
   `RECONCILER-CASCADE-HANDOFF.md` — it supersedes the old Fork-A plan. Key facts:
   the spine has **two whole-metadata-map consumers** (`healStatePatchWithRollback`
   → `ProjectLifecycle`, and the circuit breaker) that read the raw `map[string]string`,
   which `session.Info` (no `Metadata` map) cannot feed without fragile
   reconstruction. **Fork B keeps ProjectLifecycle + CB + write-back lockstep on
   the raw bead** (accepted), so the raw bead stays the source of truth, the
   Phase-1↔Phase-2 aliasing is UNTOUCHED, and there is **no atomic-flip and no
   state-split risk**. The wrapper = a per-iteration
   `info := sessionpkg.InfoFromPersistedBead(*session)` derived alongside the raw
   working copy; the tick's classifier **DECISION reads** go through `info`
   (re-derive after a mutation). Each decision-read cluster converts INDEPENDENTLY
   and incrementally; the reconcile/pool E2E suites are the byte-identical oracle.
   Consequence: the reconciler files do NOT become accessor-free (they are NOT
   added to `snapshotInfoOnlyFiles`), and the 7 raw-`[]beads.Bead` entry-threading
   sites are rule-3-sanctioned (the entry needs raw beads), not converted.
   - **DONE (`69ccc13c6`):** `Info.ResetCommittedAt` + `Info.ContinuationResetPending`
     + `resetPendingCommittedAtInfo` + 4 equivalence fixtures (the byte-identical
     oracle for the `resetPendingCommittedAt` decision read at `session_reconciler.go:~1247`).
   - **Field-gaps still needed (decision-reads only):** `Info.Generation string`
     (RAW mirror — fidelity trap: `generation` is read both `strconv.Atoi` AND
     `strings.TrimSpace`, `session_wake.go:41/173/283/331/350/461`);
     `Info.StartedConfigHash` (raw; drift-detection decision reads at
     `session_reconciler.go:2026/2278/3571/3733`); a `pin_awake` mirror
     (`session_reconciler.go:2501`). `held_until`/`wake_request`/`churn_count`/
     `core_hash_breakdown` are ProjectLifecycle/CB/write-back machinery → stay raw.
   - **Incremental order (each its own verified commit):** (a) add `Info.Generation`
     + convert Phase 2 `advanceSessionDrainsWithSessionsTraced` (`session_wake.go:428–668`)
     decision reads — bounded, self-contained; (b) the Phase-1 driver decision-read
     clusters, cluster by cluster; (c) leave apply/write-back + ProjectLifecycle + CB raw.

2. **The remaining spine-blocked + other-blocked sites** as their ops take Info:
   `filterSessionBeadsByName` (`city_runtime.go:3085`), `cmd_wait.go:1164` (wait-
   nudge helper family), `soft_reload.go:103` (needs a `template_overrides`/raw-
   metadata accessor on `Info`), and the `open []beads.Bead` threads at
   `city_runtime.go:1159`/`:2158`/`:2246`, `cmd_start.go:904`/`:918`,
   `session_lifecycle_parallel.go:809`. Add each newly accessor-free file to
   `snapshotInfoOnlyFiles`.

3. **P5 `closeBead` cross-class split** (LANDMINE — isolated, last; recording-fake
   oracle; close-THEN-release; preserve skip-if-already-closed idempotence).

4. **P6** delete dead bead classifiers/`Open()`/`FindSessionBeadBy*` (codec edge
   `session_bead_snapshot.go:301` is EXEMPT) + widen the guard to forbid
   `.Store().Store` in converted files. When the pool sweep + recovery loops'
   raw siblings lose their last caller (`poolSessionBeadRuntimeRunning`,
   `scaleCheckPartialSessionPreservable`, `staleNonExpandingPoolSessionBead`),
   delete them here too.

**DO NOT convert (RAW-BY-DESIGN, not leaks):** `city_status_snapshot.go:411`
(`countCitySessionsFromSnapshot` — prove the snapshot invariant first),
`city_runtime.go:2153` (`emitDueComputeFacts` — usage bookkeeping),
`city_runtime.go:3246` (`sessionBeadSnapshotFingerprint` — hashes ALL raw
metadata), and the `session_bead_snapshot.go` codec edge. The 7 rule-3 store-op
sites (`build_desired_state.go:3341`/`:3570`/`:3816`/`:4165`,
`city_runtime.go:2752`, `session_beads.go:57`/`:2033`) stay raw.

**Method (proven):** keep each original classifier untouched + ADD the typed
sibling + ADD an equivalence case (byte-identical oracle), THEN flip the signature
with ALL callers in the SAME commit. `OpenInfos()[i]` is the precomputed
projection of `Open()[i]`, so raw + Info slices coexist during partial migration
(the spine mutation cluster is the one exception that must flip together). For
foundation gaps, add the Info field + codec population + equivalence case BEFORE
the site that needs it. Test call sites project fixtures via
`sessionInfosFromBeads([]beads.Bead) []session.Info`.

**Build/commit hygiene:** `git checkout go.sum` after builds; commit AND push with
`--no-verify` (stale hooksPath + the pre-push hook runs the full suite and times
out — run gates manually). Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is
LOCAL-ONLY (no `bd dolt push`).

**Gates before ready:** `go build ./...` · `go vet ./...` ·
`golangci-lint run ./cmd/gc/... ./internal/session/...` (0) · the equivalence +
guard tests · targeted subject suites (reconcile/pool/wait). `make dashboard-check`
not needed (`Info` additions stay internal). The build host is oversubscribed —
targeted `-run` locally; CI on dedicated runners is the byte-identical gate.

**Finish (only when #3839 CI is verified GREEN — no premature ready):**
- `gh pr checks 3839 --watch`
- ready (gh pr ready aborts on projectCards — use the API): `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
- label: `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

**Done =** every non-work consumer reads via `session.Info` (grep-clean of raw
snapshot accessors + `.Store().Store`), the guard forbids regression, full gates
+ #3839 CI green, #3839 ready + labeled. Update
`memory/infra-beads-decoupling-plan.md`.

---
