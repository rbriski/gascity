# Next-session prompt — continue the reconciler spine flip (Fork B, incremental)

Paste the block below into a fresh session.

---

Continue the **reconciler spine flip** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `6ccf9d698`).

**Read first:** `engdocs/plans/infra-store-decouple/SPINE-FLIP-HANDOFF.md` — the
authoritative, self-contained guide (design = Fork B, verified scope, field-gap
table, incremental order, method, gates). It supersedes the Fork-A material in
`RECONCILER-CASCADE-HANDOFF.md`.

**Design (Fork B, owner-decided):** the reconcile spine has two whole-metadata-map
consumers — `healStatePatch→ProjectLifecycle` and the circuit breaker — that
`session.Info` (no `Metadata` map) can't feed. Fork B **keeps ProjectLifecycle +
circuit breaker + write-back lockstep on the raw bead**, so the raw bead stays the
single source of truth, the Phase-1↔Phase-2 aliasing is untouched, and there is
**NO atomic-flip and NO state-split risk**. The wrapper = a per-iteration
`info := sessionpkg.InfoFromPersistedBead(*session)` derived alongside the raw
working copy; convert the tick's **classifier DECISION reads** to `info`
(re-derive after a mutation), one cluster per commit, verified against the
reconcile/pool E2E. The reconciler files do NOT become accessor-free (they are NOT
added to `snapshotInfoOnlyFiles`); the raw-`[]beads.Bead` entry-threading sites
are rule-3-sanctioned, not converted.

**Confirm a green baseline:**
```
go build ./cmd/gc/ ./internal/session/
go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree' -count=1
git checkout go.sum
```

**DONE already:**
- **Tier-0 (`69ccc13c6`):** `Info.ResetCommittedAt` + `Info.ContinuationResetPending`
  + `resetPendingCommittedAtInfo` + 4 equivalence fixtures.
- **Phase 2 (`a6dea375a`):** `Info.Generation` (RAW string mirror, NOT `int`) +
  fixture + `sessionGeneration` case; `advanceSessionDrainsWithSessionsTraced`
  (`session_wake.go`) decision reads → `info` (session_name, generation, 8
  template sites); param `sessions`→`sessionBeads`.
- **Phase 1, cluster 1 (`6ccf9d698`):** the reconciler loop preamble
  (`session_reconciler.go:~1246–1275`) → `info` (name, reset-pending,
  known-state, unknown-state trace); proven mutation-free-prefix.

**First concrete increment (do this, as ONE verified commit): Phase 1, cluster 2
— the `!desired` orphan/suspend branch (`session_reconciler.go:~1277`+).**
This is the FIRST re-derive-after-mutation cluster (do it with fresh context):
1. The branch mutates the session mid-iteration: `attemptRollbackPendingCreate`,
   the inline `session.Status = "closed"` (`~1369`), and `healStateWithRollback`
   (`~1382`, mutates `session.Metadata` in lockstep via `sessFront`). So the
   top-of-loop `info` from cluster 1 is STALE after each mutation.
2. Audit each helper's mutation behavior first (as cluster 1 audited
   `reconcileDrainAckStopPending`): `checkRateLimitStability`,
   `isFailedCreateSessionBead`, `preserveConfiguredNamedSessionBead`,
   `shouldRollbackPendingCreate`, `pendingCreateLeaseExpiredForRollback`.
   `stateBeforeHeal`/`pendingCreateStartedAtBeforeHeal`/`lastWokeAtBeforeHeal`
   (`~1379–1381`) are read-before-heal snapshots — keep them pre-heal (raw or a
   pre-heal `info`).
3. Convert only the pre-mutation decision reads in this branch, and/or
   **re-derive `info := sessionpkg.InfoFromPersistedBead(*session)` after each
   mutation** for the reads that follow it. The template reads here still use
   `normalizedSessionTemplate(*session, cfg)` — convert to
   `normalizedSessionTemplateInfo(info, cfg)` only where `info` is current.
4. Verify: build + vet + lint + equivalence + trace-integration + the full
   `TestReconcileSessionBeads*` suite (205 tests; run with a ≥420s timeout — the
   box overloads under `fork/exec`, split the run if it times out).

**Then:** the remaining Phase-1 clusters (heal/stability, pool-demand,
named-identity), cluster by cluster, adding `Info.StartedConfigHash` (raw) and a
`pin_awake` mirror as their sites are reached (see the handoff field-gap table).
Leave the apply/write-back cluster + ProjectLifecycle + circuit breaker raw.

**Method:** keep original + ADD the `Info` field/sibling + ADD an equivalence case
(byte-identical oracle) + THEN convert the decision read via the per-iteration
`InfoFromPersistedBead(*session)`. Run the reconcile/pool E2E after each cluster.

**Gates/hygiene:** `go build ./...` · `go vet ./...` ·
`golangci-lint run ./cmd/gc/... ./internal/session/...` (0) · equivalence + guard
+ reconcile/pool suites. `git checkout go.sum` after tests. Commit AND push with
`--no-verify`. Trailer: `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is
LOCAL-ONLY (no `bd dolt push`). If you re-run a mapping agent, pin
`git rev-parse HEAD` first.

**Do NOT rush.** This is a 3–5 session effort on the reconciler core; one
decision-read cluster per verified commit. Do not fan parallel implementation
agents at the reconcile driver. #3839 stays DRAFT (no premature ready).
Update memory (`infra-beads-decoupling-plan.md`) and the handoff as you land
each increment.

---
