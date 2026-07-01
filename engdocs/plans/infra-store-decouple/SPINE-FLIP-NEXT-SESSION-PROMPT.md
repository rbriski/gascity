# Next-session prompt — continue the reconciler spine flip (Fork B, incremental)

Paste the block below into a fresh session.

---

Continue the **reconciler spine flip** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `aea0e50fa`).

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

**DONE already (`69ccc13c6`, Tier-0):** `Info.ResetCommittedAt` +
`Info.ContinuationResetPending` + `resetPendingCommittedAtInfo` + 4 equivalence
fixtures (the oracle for the `resetPendingCommittedAt` decision read).

**First concrete increment (do this, as ONE verified commit):**
1. Add **`Info.Generation string`** — a RAW mirror of `generation`, **not `int`**
   (fidelity trap: `generation` is read both `strconv.Atoi` AND `strings.TrimSpace`,
   `session_wake.go:41/173/283/331/350/461`). Struct (`internal/session/manager.go`)
   + codec (`internal/session/info_store.go:InfoFromPersistedBead`) + an
   equivalence case/fixture in `cmd/gc/session_classifier_info_equiv_test.go`.
2. Convert **Phase 2** `advanceSessionDrainsWithSessionsTraced`
   (`cmd/gc/session_wake.go:428–668`): at the top of the drain loop derive
   `info := sessionpkg.InfoFromPersistedBead(*session)` and read
   `info.SessionNameMetadata` (session_name), `info.Generation` (Atoi/TrimSpace as
   the raw sites do), and `normalizedSessionTemplateInfo(info, cfg)` (template).
   The mutations (`completeDrain`, `cancelSessionDrainForPending/ForAssignedWork`)
   and `session.ID` **stay raw**.
3. Verify: build + vet + lint + equivalence + the reconcile/pool E2E suites.

**Then:** the Phase-1 driver decision-read clusters
(`reconcileSessionBeadsTracedWithNamedDemand`, `session_reconciler.go:1005`+),
cluster by cluster, adding `Info.StartedConfigHash` (raw) and a `pin_awake`
mirror as their sites are reached (see the handoff field-gap table). Leave the
apply/write-back cluster + ProjectLifecycle + circuit breaker raw.

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
