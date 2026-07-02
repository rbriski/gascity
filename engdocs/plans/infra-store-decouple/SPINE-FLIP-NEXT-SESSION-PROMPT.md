# Next-session prompt — continue the reconciler spine flip (Fork B, incremental)

> **⚠️ SUPERSEDED (2026-07-02).** Do NOT continue the re-derive clusters below. The
> reconciler read migration now goes through the typed **`session.Store`** front
> door. Use **`RECONCILER-FRONT-DOOR-NEXT-SESSION-PROMPT.md`** instead.


Paste the block below into a fresh session.

---

Continue the **reconciler spine flip** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `806de56f5`).

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

- **Phase 1, cluster 4c-foundation (`4dedfa476`):** the two DESIRED-branch field
  mirrors — `Info.StartedConfigHash` + `Info.PinAwake` (RAW string mirrors, Generation
  pattern, no json tag → internal-only) on struct+codec + a whitespace-padded
  `config-hash-and-pin` fixture + `sessionStartedConfigHash`/`sessionPinAwake`
  stringChecks. No reconcile-driver change.
- **Phase 1, cluster 4c (`6e65e7f69`):** FIRST desired-branch conversion.
  `pendingResumePreservingNamedRestart` (pure classifier, `session_reconciler.go:897`)
  → `…Info` (uses the new `StartedConfigHash`; `pendingCreateLeaseActiveInfo` tail) +
  a `pending-resume-preserve` true-branch fixture (guarded). Single call site (`~2444`)
  with a **fresh local re-derive** `infoAsleepDrift := InfoFromPersistedBead(*session)`
  (top-of-loop info is stale that deep; the sibling is `||`-short-circuited so the
  fresh projection is byte-identical).
- **Phase 1, cluster 4d (`806de56f5`):** the wake-pass sleep-policy loop (`~2672`,
  `for _, target := range wakeTargets`) — read-only over the bead, so ONE loop-top
  `info` (no re-derive). Converted `session_name`→`info.SessionNameMetadata`,
  `pin_awake != "true"`→`info.PinAwake != "true"`, `normalizedSessionTemplate`→
  `…Info`. `resolveSessionSleepPolicy`/`configWakeSuppressed` (whole-bead+runtime) +
  `sleep_intent` (no mirror yet) stay raw.

**KEY RE-FRAMING (settled):** the **entire `!desired` branch is now Info-routed**
(clusters 1–4b), and the DESIRED branch is started (4c resume-preserve gate + 4d
wake-pass). **RE-SCOPING (4c/4d fresh map):** most `started_config_hash` "decision
reads" are NOT convertible — the config-drift machinery (`~2165/2417`,
`sessionConfigDriftKey` @`3705`, `resetConfiguredNamedSessionForConfigDrift` @`3833`,
`session_reconcile.go:832`) is entangled with `sessionCoreConfigForHash` whole-bead
config derivation + unmirror­ed sub-hashes → **stays RAW** (Fork B). `Info.StartedConfigHash`
has exactly one consumer (4c); `Info.PinAwake` has exactly one (4d).

**First concrete increment (do this as verified commits): Phase 1, cluster 4e+ —
the rest of the DESIRED branch. MAP IT FRESH** (re-run the census greps; pin
`git rev-parse HEAD` first). Candidate clusters, each its own verified commit:
- **Zombie/rollback fast-path (`~1753–1777`):** `shouldRollbackPendingCreate` (2 sites)
  + `pendingCreateLeaseExpiredForRollback` — siblings EXIST. **Needs a re-derive after
  the zombie-capture block:** `markProviderTerminalError` (`~1732`, NO `continue`
  after) writes `pending_create_claim`/`pending_create_started_at`/`last_woke_at` = the
  read keys, so top-of-loop info is stale. Audit `recordResetStallIfDue` (`~1726`,
  by-value bead → shares Metadata map) for bead writes. Medium risk.
- **`namedSessionIdentity(*session)` (`~2024`, restart-requested block):** sibling
  EXISTS; after mutations → re-derive at the call site.
- **`sleep_intent` field-gap (`~2696`):** add `Info.SleepIntent` (raw mirror) bottom-up
  + equivalence case, then convert the wake-pass `hasExplicitSleepIntent` read.
- **maxAge/progress-stall/pendingInteraction reads** (`~1922–2001`, `~2463`): map fresh.
- **STAY RAW:** the config-drift machinery + all its `started_config_hash`/sub-hash reads;
  the apply/write-back cluster (`healState*`, `checkStability`, `checkChurn`,
  `record*`/`clear*`, `persistSessionCircuitBreakerMetadata`, inline
  `session.Status`/`restart_requested` writes); `ProjectLifecycle`; the circuit breaker.
Method unchanged: add each missing sibling bottom-up (equivalence-cased), THEN convert
cluster by cluster, **re-deriving `info` after each mutation** exactly as 4a/4b/4c did.

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
