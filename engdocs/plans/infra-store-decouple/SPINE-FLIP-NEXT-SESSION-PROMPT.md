# Next-session prompt — continue the reconciler spine flip (Fork B, incremental)

Paste the block below into a fresh session.

---

Continue the **reconciler spine flip** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `6c1e41d1b`).

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
- **Phase 1, cluster 2 (`6c1e41d1b`):** the pending-create rollback gate
  (`session_reconciler.go:~1338–1357`, first block inside `!desired`) → `info`
  (shouldRollback, leaseExpired, template, configuredNamedSpec). Added 5 Info
  siblings (`shouldRollbackPendingCreateInfo`, `pendingCreateNeverStartedExpiredInfo`,
  `pendingCreateLeaseExpiredForRollbackInfo`, `namedSessionIdentityInfo`,
  `configuredNamedSessionBeadHasSpecInfo`) + 5 equivalence cases + a real-cfg guard.
  **Key finding:** this block is PRE-heal, so it reuses the top-of-loop `info`
  with NO re-derive (the block's mutations all `continue`).

**KEY RE-FRAMING (the earlier plan was wrong about this):** the `!desired` branch
does NOT need re-derive until the heal. Everything from the top of `!desired`
(`~1330`) down to **`healStateWithRollback` (`session_reconciler.go:1441`)** is the
**pre-heal region** — same safety class as clusters 1–2, NO re-derive. The genuine
re-derive-after-mutation work is the **post-heal region** (after `1441`).

**First concrete increment (do this, as ONE verified commit): Phase 1, cluster 3
— the remaining pre-heal blocks (`session_reconciler.go:~1367–1436`).** Still NO
re-derive — reuse the top-of-loop `info`. Two sub-blocks:
1. **preserve-named + rate-limit (`~1367`):** `preserveConfiguredNamedSessionBead`
   → new `preserveConfiguredNamedSessionBeadInfo` (composes `isNamedSessionInfo` +
   `namedSessionIdentityInfo` [now exists] + `findNamedSessionSpec` +
   `info.SessionNameMetadata`/`MetadataState`/`SleepReason`/`LastWokeAt`);
   trace `normalizedSessionTemplate`→`normalizedSessionTemplateInfo(info,cfg)`
   [exists]. `checkRateLimitStability` stays raw (mutation).
2. **failed-create close (`~1405–1436`):** `isFailedCreateSessionBead`
   →`isFailedCreateSessionInfo(info)` [exists]; `pendingCreateSessionStillLeased`
   (`~1410`, PRE-heal)→ new `pendingCreateSessionStillLeasedInfo` (composes
   `pendingCreateLeaseActiveInfo` [exists] + new `sessionStartRequestedInfo` +
   `normalizedSessionTemplateInfo` + `findAgentByTemplate`); template reads [exists].
   The inline `session.Status="closed"` write + the read-before-heal snapshots
   (`~1438–1440`) stay raw. The `session.Metadata["pending_create_claim"]`
   trace-payload read stays raw (`Info.PendingCreateClaim` is a bool, not the raw
   string).
3. Verify: build + vet + lint + equivalence + trace-integration + the full
   `TestReconcileSessionBeads*` suite (205 tests; ≥420s timeout — the box overloads
   under `fork/exec`, split the run if it times out) + rollback/lease chaos + pool/
   named suites.

**Then:** cluster 4+ — the **post-heal region** (`1441`+), the FIRST genuine
re-derive cluster: after the heal, **re-derive
`info := sessionpkg.InfoFromPersistedBead(*session)`** and convert the switch/
`default` decision reads (post-heal `pendingCreateSessionStillLeased` at `~1476`,
the drain-ack block, the orphan-drain/suspend/close block). Add `Info.StartedConfigHash`
(raw) + a `pin_awake` mirror as later sites reach them (see the handoff field-gap
table). Leave the apply/write-back cluster + ProjectLifecycle + circuit breaker raw.

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
