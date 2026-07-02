# Next-session prompt — continue the reconciler spine flip (Fork B, incremental)

Paste the block below into a fresh session.

---

Continue the **reconciler spine flip** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `8c3e600ae`).

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
- **Phase 1, cluster 3 (`937beeb13`):** the rest of the `!desired` pre-heal region
  (`session_reconciler.go:~1414–1457`) → `info`, no re-derive. preserve-named
  (`preserveConfiguredNamedSessionBead`→`…Info`) + failed-create close
  (`isFailedCreateSessionBead`→`isFailedCreateSessionInfo`,
  `pendingCreateSessionStillLeased`→`…Info`) + their trace templates
  (`normalizedSessionTemplateInfo`+`info.Template`). Added the 4 checklist siblings
  (`staleCreatingStateInfo`, `sessionStartRequestedInfo`,
  `pendingCreateSessionStillLeasedInfo`, `preserveConfiguredNamedSessionBeadInfo`) +
  4 equivalence cases (`pendingCreateSessionStillLeased` under a worker-resolving
  `leaseCfg`) + a keep-alias real-cfg guard. No `Info` struct/codec change.
  **Verified pre-heal safety:** checkRateLimitStability writes no
  template/agent_name/alias key; the failed-create reads sit behind its
  non-mutating `(false,nil)` return. Trace-payload `pending_create_claim`/`state` +
  inline `session.Status="closed"` + read-before-heal snapshots stay raw.
- **Phase 1, cluster 4a (`dac68d506`):** the FIRST genuine re-derive-after-mutation
  increment. Re-derived `infoPostHeal := sessionpkg.InfoFromPersistedBead(*session)`
  right after `healStateWithRollback` (`session_reconciler.go:~1514`; the intervening
  `traceHealClearedPendingCreateLease` takes the bead by value, cannot mutate) and
  routed the two non-`default` post-heal switch arms through it: the `preserveNamed`
  case template + the `pendingCreateSessionStillLeased(*session,cfg,clk)` guard →
  `pendingCreateSessionStillLeasedInfo(infoPostHeal,cfg,clk)` + its case template. Go
  switch cases don't fall through, so both arms read the byte-identical post-heal
  bead. No new siblings/codec change. The `default` block stays raw (→ cluster 4b).
- **Phase 1, cluster 4b (`8c3e600ae`):** the post-heal `default` block
  (`session_reconciler.go:~1550–1717`) — drain-ack / orphan-drain / suspend / close —
  converted through the 4a `infoPostHeal`. `isNamedSessionBead(*session)`→
  `isNamedSessionInfo(infoPostHeal)` + all 8 `normalizedSessionTemplate` trace reads→
  `normalizedSessionTemplateInfo(infoPostHeal,cfg)`. **A SINGLE top-of-switch
  re-derive proved sufficient (no per-branch re-derive):** full write-set audit
  showed the in-place mutators (`markDrainAckStopPending`,
  `finalizeDrainAckStoppedSession`) run AFTER their path's read then `continue`, and
  the by-value-bead helpers (`cancelSessionDrain*`, `beginSessionDrain`) write no bead
  metadata (only dt/sp/telemetry). **Trap noted for future clusters: passing
  `*session` by value still shares the `Metadata` map — safety rests on the write-set
  audit, not the value copy.** No new siblings/codec change.

**KEY RE-FRAMING (settled):** the **entire `!desired` branch is now Info-routed**
(pre-heal clusters 1–3 + post-heal 4a switch guards + 4b default block). The
remaining reconciler decision reads live in the **desired path** (after
`session_reconciler.go:~1718`).

**First concrete increment (do this as verified commits): Phase 1, cluster 4c+ —
the DESIRED branch + field-gaps. This is a NEW, less-mapped region — MAP IT FRESH
before converting** (re-run the census greps; do not assume the `!desired` anchors
carry over). The decision reads now live in the desired path: the "Liveness includes
zombie detection" fast-path (`~1718`+) and the stability/churn/drain-advance branches
below it, plus the scattered field-gaps: `started_config_hash` drift-detection reads
(`~2026/2278/3571/3733`, `session_reconcile.go:814`) — **add `Info.StartedConfigHash
string` (raw mirror) first** (struct+codec+equivalence fixture); `pin_awake`
(`~2501`) — add a mirror. Method unchanged: add each missing sibling bottom-up
(equivalence-cased), THEN convert cluster by cluster, **re-deriving `info` after each
mutation** in the desired path exactly as 4a/4b did post-heal. Leave the
apply/write-back cluster (`healState*`, `checkStability`, `checkChurn`,
`record*`/`clear*`, `persistSessionCircuitBreakerMetadata`, inline
`session.Status`/`restart_requested` writes) + `ProjectLifecycle` + the circuit
breaker RAW.

Verify each cluster: build + vet + lint + equivalence + guards + trace-integration +
the full `TestReconcileSessionBeads*` suite (205 tests; ≥420s timeout — the box can
overload under `fork/exec`, split the run if it times out) + rollback/lease chaos +
pool/named suites. Re-run the handoff's sanity greps before starting in case HEAD
moved; if you re-run a mapping agent, pin `git rev-parse HEAD` first.

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
