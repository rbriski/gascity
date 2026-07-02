# Spine-Flip Handoff — reconciler decision-reads → `session.Info` (Fork B)

> **⚠️ SUPERSEDED (2026-07-02).** The `InfoFromPersistedBead(*session)` re-derive
> approach documented here was retired by owner review: it re-projects the raw
> working copy instead of hiding the map behind a front door. The reads now route
> through the typed **`session.Store`** front door. **Use
> `RECONCILER-FRONT-DOOR-HANDOFF.md` + `RECONCILER-FRONT-DOOR-SPEC.md` instead.**
> This doc is kept as history: the landed commits (Tier-0 `69ccc13c6` … cluster 4e
> `733812a11`) and their `*Info` classifiers + equivalence oracle stay on the branch
> as the accessor foundation; only their re-derive *call sites* get rewritten.


**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`,
**HEAD `806de56f5`** (pushed; Phase 2 + Phase-1 clusters 1+2+3 + 4a+4b landed the
entire `!desired` branch, then the cluster-4c field foundation + clusters 4c/4d
started the DESIRED branch). Self-contained guide for finishing the reconciler
spine flip. It supersedes the Fork-A material in `RECONCILER-CASCADE-HANDOFF.md`
(kept only as background).

> **Do not rush this.** It is genuinely a **3–5 session effort** on the system's
> most correctness-critical component (the reconcile tick silently manages live
> agent sessions). Convert **one decision-read cluster per commit**, each verified
> against the reconcile/pool E2E suites. A rushed mega-edit here is the worst bug
> class in the project. Do NOT fan parallel implementation agents at the reconcile
> driver — it is one connected component. (Read-only mapping agents are fine.)

## Goal

Route the reconcile tick's **classifier DECISION reads** of session-bead
metadata through the typed `session.Info` projection instead of raw
`beads.Bead.Metadata`, per the non-work-bead field-door contract (non-work beads
are read via `Info`; only generic WORK beads read raw).

## Design — Fork B (working-copy wrapper), owner-decided

The reconcile spine has **two whole-metadata-map consumers** that read the raw
`map[string]string`, which `session.Info` (a curated typed struct with **no
`Metadata` map**, by design) cannot feed without a fragile field→map
reconstruction:

1. `healStatePatchWithRollback` → `sessionpkg.ProjectLifecycle(LifecycleInput{Metadata: …})`
   (`session_reconcile.go:1058`). `ProjectLifecycle` reads a **bounded** key set:
   `state`, `sleep_reason`, `pending_create_claim`, `pending_create_started_at`,
   `last_woke_at`, `continuity_eligible`, `held_until`, `pin_awake`,
   `quarantined_until`, `session_key`, `started_config_hash`, `wake_request`,
   `NamedSessionIdentity`.
2. The circuit breaker `restoreFromMetadata` / `observeResetGenerationFromMetadata`
   (`session_circuit_breaker.go:320/508`), fed `ordered[i].Metadata` at
   `session_reconciler.go:1146/1155`.

**Fork B keeps ProjectLifecycle + the circuit breaker + the write-back lockstep
on the raw bead (accepted).** Therefore:

- The raw bead stays the **single source of truth**. The Phase-1↔Phase-2
  aliasing is **untouched**: `beadByID` and `circuitSessionByIdentity` both hold
  `&ordered[i]` (`session_reconciler.go:1183/1145`), the working copy is
  `session := &ordered[i]` (`:1227`), and `advanceSessionDrainsWithSessionsTraced`
  reads through `beadByID`. Because we do NOT retype `ordered`/the maps, **there
  is no atomic-flip requirement and no state-split risk** (the key de-risking vs
  Fork A).
- The "working-copy wrapper" is realized as a **per-iteration**
  `info := sessionpkg.InfoFromPersistedBead(*session)` derived alongside the raw
  working copy. The tick's **classifier decision reads** go through `info`;
  **re-derive `info` after any mutation** within the iteration (the apply
  functions mutate `session.Metadata` in lockstep, so a fresh
  `InfoFromPersistedBead(*session)` is always current). Each decision-read
  cluster converts **independently and incrementally**.

### Consequences (important)

- The reconciler files (`session_reconciler.go`, `session_reconcile.go`,
  `session_wake.go`) will **NOT** become accessor-free — ProjectLifecycle, the
  circuit breaker, the pure patch computers, and the write-back lockstep keep
  reading raw metadata **by design**. Do **NOT** add them to
  `snapshotInfoOnlyFiles`.
- The 7 previously "reconciler-spine-blocked" sites that thread raw
  `[]beads.Bead` into the reconcile entry (`city_runtime.go:1159/2158/2246/3085`,
  `cmd_start.go:904/918`, `session_lifecycle_parallel.go:809`) are **reclassified
  as rule-3-sanctioned** — the entry legitimately needs raw beads for
  ProjectLifecycle/CB, so those sites **stay raw**, they are not converted.

## Status

**DONE — Tier-0 foundation (`69ccc13c6`):**
- `Info.ContinuationResetPending` (raw `continuation_reset_pending`) +
  `Info.ResetCommittedAt` (raw `reset_committed_at` / `session.ResetCommittedAtKey`)
  added to the struct (`internal/session/manager.go`) + codec
  (`internal/session/info_store.go:InfoFromPersistedBead`).
- `resetPendingCommittedAtInfo` (`cmd/gc/session_reconciler.go`, next to
  `resetPendingCommittedAt`) — mirrors the trim + RFC3339-parse rules.
- Equivalence oracle: `TestSessionClassifierInfoEquivalence` now compares the
  `resetPendingCommittedAt` (raw, parsed-time, pending) tuple across 4 new
  fixtures (`reset-pending-committed`, `reset-pending-no-committed`,
  `reset-pending-invalid-committed`, `reset-not-pending`). This is the
  byte-identical oracle for the `resetPendingCommittedAt` decision read at
  `session_reconciler.go:~1247`.

**DONE — Phase 2: drain-advance (`a6dea375a`):**
- `Info.Generation` — a RAW string mirror of `generation` (NOT `int`, per the
  Atoi/TrimSpace fidelity trap) — added to the struct + `InfoFromPersistedBead`
  codec, plus a whitespace-padded (`" 3 "`) equivalence fixture and a
  `sessionGeneration` stringChecks case pinning the raw codec mirror.
- `advanceSessionDrainsWithSessionsTraced` (`session_wake.go`) decision reads
  routed through a per-iteration `info := session.InfoFromPersistedBead(*session)`:
  `session_name`→`info.SessionNameMetadata`, `generation`→
  `strconv.Atoi(info.Generation)`, template (8 trace sites)→
  `normalizedSessionTemplateInfo(info, cfg)`. Mutations (`completeDrain`,
  `cancelSessionDrainFor*`) + `session.ID` stay raw. The `sessions []beads.Bead`
  param was renamed `sessionBeads` to un-shadow the `session` package alias.
  Byte-identical: `completeDrain` (the only in-loop mutation) writes only
  sleep/state keys (SleepPatch/CompleteDrainPatch never touch
  template/alias/agent_name/session_name) and always `continue`s.

**DONE — Phase 1, cluster 1: reconciler loop preamble (`6ccf9d698`):**
- The mutation-free top of the `reconcileSessionBeadsTracedWithNamedDemand`
  per-session loop (`session_reconciler.go:~1246–1275`): `name`←
  `strings.TrimSpace(info.SessionNameMetadata)`, reset-pending→
  `resetPendingCommittedAtInfo(info)`, known-state→`isKnownStateInfo(info)`,
  unknown-state trace→`info.SessionNameMetadata`/`info.MetadataState`/`info.Template`.
- Proven mutation-free-prefix: `reconcileDrainAckStopPending` (called at ~1259,
  between the reset read and the known-state read) mutates ONLY on its
  true/continue paths — its sole false return is the non-mutating
  `!isDrainAckStopPending` early-out (line 437) — so when control falls through
  to the known-state check the session is still unmutated and the top-of-loop
  projection is byte-identical. Verified: trace-integration suite (asserts the
  `unknown_state` decision + template values) + all 205
  `TestReconcileSessionBeads*`.

**DONE — Phase 1, cluster 2: pending-create rollback gate (`6c1e41d1b`):**
- The pending-create rollback block in the `!desired` branch
  (`session_reconciler.go:~1338–1357`) — the FIRST block inside `!desired`,
  ending in `continue`. Converted the four pure decision reads:
  `shouldRollbackPendingCreate`→`shouldRollbackPendingCreateInfo(info)`,
  `pendingCreateLeaseExpiredForRollback`→`…Info(info,…)`,
  `normalizedSessionTemplate`+`session.Metadata["template"]`→
  `normalizedSessionTemplateInfo(info,cfg)`+`info.Template`,
  `configuredNamedSessionBeadHasSpec`→`…Info(info,…)`.
- **Pre-heal safety class (same as cluster 1, NO re-derive):** this whole block
  is before the heal. The only mutations reachable here —
  `checkRateLimitStability` (on its hit/err path) and `attemptRollbackPendingCreate`
  — each `continue`, and `workerSessionTargetRunningWithConfig` (`~1331`) reads by
  ID, so control only reaches the next decision read on the still-unmutated bead;
  the top-of-loop `info` stays byte-identical. The two mutation calls keep the raw
  `*session` pointer they write through.
- New equivalence-proven Info siblings (each composes proven leaves):
  `shouldRollbackPendingCreateInfo` (`session_lifecycle_parallel.go`),
  `pendingCreateNeverStartedExpiredInfo` + `pendingCreateLeaseExpiredForRollbackInfo`
  (`session_reconciler.go`), `namedSessionIdentityInfo` +
  `configuredNamedSessionBeadHasSpecInfo` (`named_sessions.go`). Equivalence test
  gained 5 cases + a real-cfg guard (`namedSpecCfg`) asserting the named fixture
  hits the has-spec true branch (not a trivial both-false pass). Verified: build/
  vet/lint clean; equivalence + 205 `TestReconcileSessionBeads*` + rollback/lease
  chaos + trace-integration + pool/named suites.

**DONE — Phase 1, cluster 3: remaining pre-heal blocks (`937beeb13`):**
- The rest of the `!desired` pre-heal region (`session_reconciler.go:~1414–1457`),
  reusing the top-of-loop `info` with NO re-derive. Two sub-blocks:
  - **preserve-named:** `preserveConfiguredNamedSessionBead`→
    `preserveConfiguredNamedSessionBeadInfo(info,cfg,cityName)`; the rate-limit-hit
    trace template→`normalizedSessionTemplateInfo(info,cfg)`+`info.Template`.
  - **failed-create close:** `isFailedCreateSessionBead`→`isFailedCreateSessionInfo(info)`;
    `pendingCreateSessionStillLeased`→`pendingCreateSessionStillLeasedInfo(info,cfg,clk)`;
    its trace template→`normalizedSessionTemplateInfo(info,cfg)`+`info.Template`.
- **Pre-heal safety (verified):** these reads run on a byte-identical top-of-loop
  bead. `preserveNamed` (`~1414`) runs before any in-region mutation (the rollback
  block only mutates on `continue` paths). `checkRateLimitStability` (`~1433`) is the
  one in-region mutation, and both its write paths (`markProviderTerminalError`,
  `RateLimitQuarantinePatch`) touch only state/sleep/health/quarantine keys — **never
  template/agent_name/alias** — so the template trace read on its hit/err path stays
  byte-identical against `info`. The failed-create-close reads (`~1452+`) are reached
  ONLY via checkRateLimitStability's non-mutating `(false,nil)` return at
  `session_reconcile.go:699` (any mutating path sets hit/err → `continue` at `~1450`),
  so the bead is fully unmutated there. The two trace-payload raw-string reads
  (`pending_create_claim`, `state`) stay raw: `pending_create_claim` has no raw-string
  Info mirror (`Info.PendingCreateClaim` is a bool). The inline `session.Status="closed"`
  write + the read-before-heal snapshots stay raw.
- **New Info siblings (all 4 from the cluster-3 checklist, equivalence-proven):**
  `staleCreatingStateInfo` + `sessionStartRequestedInfo` (`session_reconcile.go`),
  `pendingCreateSessionStillLeasedInfo` (`session_reconciler.go`),
  `preserveConfiguredNamedSessionBeadInfo` (`session_beads.go`). No `Info` struct/codec
  change — all fields already existed. Equivalence test gained 4 cases (the
  `pendingCreateSessionStillLeased` case runs under a worker-resolving `leaseCfg` to
  exercise the `!agent.Suspended` tail) + a keep-alias real-cfg guard so the preserve
  case is a true-branch comparison, not a both-false pass. Verified: build/vet/lint
  clean; equivalence + guards + 205 `TestReconcileSessionBeads*` +
  preserve/failed-create/pending-create/stale-creating + `TestReconciler_*`
  rollback-deferral + `TestSessionLifecycleChaos*` + trace-integration suites.

**DONE — Phase 1, cluster 4a: post-heal switch guards (`dac68d506`):**
- The **FIRST genuine re-derive-after-mutation** increment. `healStateWithRollback`
  (`session_reconciler.go:1491`) mutates `session.Metadata` in lockstep, so the
  top-of-loop `info` (`~1296`) is stale for the switch that follows. Re-derive
  `infoPostHeal := sessionpkg.InfoFromPersistedBead(*session)` immediately after the
  heal (`~1514`) — the intervening `traceHealClearedPendingCreateLease` takes the
  bead **by value** and cannot mutate — and route the two non-`default` switch arms
  through it: the `preserveNamed` case template trace read, and the
  `pendingCreateSessionStillLeased(*session,cfg,clk)` switch guard →
  `pendingCreateSessionStillLeasedInfo(infoPostHeal,cfg,clk)` + its case template.
- **Safety:** Go switch cases do not fall through, so reaching either arm means no
  mutation happened between the heal and the read → `infoPostHeal` is byte-identical
  for both. No new siblings/codec change (both siblings already exist,
  equivalence-proven). Correctness of the re-derive placement is proven by the
  whole-tick E2E. The trace-payload raw reads (`pending_create_claim`, `state`) stay
  raw.
- The **`default` block stays raw this commit** — see cluster 4b below.

**DONE — Phase 1, cluster 4b: post-heal `default` block (`8c3e600ae`):**
- The drain-ack / orphan-drain / suspend / close block (`session_reconciler.go:
  ~1550–1717`), converted through the `infoPostHeal` re-derived in 4a. Converted
  the decision read `isNamedSessionBead(*session)`→`isNamedSessionInfo(infoPostHeal)`
  + all 8 `normalizedSessionTemplate(*session,cfg)` trace reads (+ their
  `Metadata["template"]` fallbacks)→`normalizedSessionTemplateInfo(infoPostHeal,cfg)`
  +`infoPostHeal.Template`.
- **Verified a SINGLE top-of-switch re-derive suffices (no per-branch re-derive):**
  every converted read is on the byte-identical post-heal bead. Full write-set audit:
  the in-place mutators (`markDrainAckStopPending`→`DrainAckStopPendingPatch` =
  state/state_reason/drain_at/pending-create keys; `finalizeDrainAckStoppedSession`)
  each run AFTER their path's converted read then `continue`; the by-value-bead
  helpers (`cancelSessionDrainForAssignedWork`, `cancelRecoveredDrainForAssignedWork`,
  `beginSessionDrain`) write NO bead metadata (only drainTracker/provider/telemetry);
  and no converted-read key (template/agent_name/alias/configured_named_session) is
  ever rewritten by drain/close logic. **NOTE the by-value-map trap:** passing
  `*session` (a `beads.Bead` value) still shares the `Metadata` map — "by value" does
  NOT by itself prevent Metadata mutation; the safety rests on the write-set audit,
  not the value copy. The inline `session.Status="closed"` write + trace-only bool
  payloads stay raw. No new siblings/codec change; correctness proven by the E2E.

**DONE — Phase 1, cluster 4c foundation (`4dedfa476`):** the two DESIRED-branch
field-gap mirrors. `Info.StartedConfigHash` (raw `started_config_hash`) +
`Info.PinAwake` (raw `pin_awake`), both RAW string mirrors (Generation pattern, no
json tag → internal-only, absent from the HTTP wire) on the struct
(`internal/session/manager.go`) + codec (`info_store.go`). Added a whitespace-padded
`config-hash-and-pin` equivalence fixture (proves the raw mirror preserves the bytes
the `TrimSpace` reader depends on) + `sessionStartedConfigHash`/`sessionPinAwake`
stringChecks cases. No reconcile-driver change this commit. Verified
`Info` has no json tags (wire uses a separate DTO) and the `get_persisted_response`
`DeepEqual` compares two codec-derived values, so additive fields stay balanced.

**DONE — Phase 1, cluster 4c: asleep-drift resume-preserve gate (`6e65e7f69`):**
the FIRST desired-branch decision-read conversion. `pendingResumePreservingNamedRestart`
(`session_reconciler.go:897`) is a PURE classifier (state / pending-create claim /
session_key / `started_config_hash` / `pending_create_started_at`, lease tail →
`pendingCreateLeaseActive`). Added the Info sibling
`pendingResumePreservingNamedRestartInfo` (uses the new `Info.StartedConfigHash`,
`pendingCreateLeaseActiveInfo` tail) + a `pendingResumePreservingNamedRestart`
clkBoolChecks case + a `pending-resume-preserve` **true-branch fixture** (creating +
claim + session_key + started_config_hash + a recent `pending_create_started_at`, so
the lease is start-in-flight) guarded by an assert that the raw form returns true.
Converted the single call site (`~2444`) with a **fresh local re-derive**
`infoAsleepDrift := InfoFromPersistedBead(*session)` — the top-of-loop info is stale
that deep in the desired path (drain-ack/restart-request/alive-config-drift ran
above). The sibling is only evaluated when `driftRestartedInPlace` is false
(short-circuit `||`), so the fresh projection reflects the current bead exactly.

**DONE — Phase 1, cluster 4d: wake-pass decision reads (`806de56f5`):** consumed the
`Info.PinAwake` mirror. The post-loop sleep-policy pass
(`session_reconciler.go:~2672`, `for _, target := range wakeTargets`) writes ONLY
`wakeEvals`/`eval` — never the bead — so a single loop-top
`info := InfoFromPersistedBead(*target.session)` is byte-identical throughout (no
re-derive, lowest-risk shape). Converted the three pure decision reads with mirrors:
`session_name`→`info.SessionNameMetadata`, `pin_awake != "true"`→`info.PinAwake !=
"true"` (the sole `PinAwake` decision read), `normalizedSessionTemplate`→
`normalizedSessionTemplateInfo(info,cfg)`. The sleep-policy resolvers
(`resolveSessionSleepPolicy`, `configWakeSuppressed` — whole-bead + runtime:
`sleep_reason`/`sleep_policy_fingerprint`/`sessionIdleReference`) stay RAW under
Fork B; `sleep_intent` has no mirror yet and stays raw. No new siblings/codec change
(PinAwake covered by 4c's fixture; template/SessionNameMetadata already proven).

**KEY RE-SCOPING (cluster-4c/4d fresh map, correcting the field-gap table):** most of
the `started_config_hash` "decision reads" the earlier census listed
(`~2165/2417`, `sessionConfigDriftKey` @`3705`, `resetConfiguredNamedSessionForConfigDrift`
@`3833`, `session_reconcile.go:832`) are NOT cleanly convertible — they are entangled
with `sessionCoreConfigForHash` (→ `applyTemplateOverridesToConfig`, a whole-bead
config derivation) and the unmirror­ed sub-hash reads (`core_hash_breakdown`,
`started_provision_hash`, `started_launch_hash`). That machinery is a **Fork-B
whole-bead consumer and stays RAW**, same class as ProjectLifecycle. The ONLY genuinely
pure `started_config_hash` decision read was `pendingResumePreservingNamedRestart`
(converted in 4c). So `Info.StartedConfigHash` has exactly one consumer; the config-drift
block stays raw by design.

**Verified scope (census at HEAD `6ccf9d698`):** 194 raw `.Metadata[` reads at the
CONT-5 census — reconciler 123 / reconcile 50 / wake 21 — plus 6 `.Status`.
Most are inside the raw machinery that stays. Only DECISION reads convert.
**Done so far (HEAD `806de56f5`):** Phase 2 drain-advance + the **entire `!desired`
branch** (clusters 1–4b) + the DESIRED-branch **field foundation** (4c-foundation:
`Info.StartedConfigHash`+`Info.PinAwake`) and **two desired-branch conversions** —
4c (`pendingResumePreservingNamedRestart` asleep-drift resume-preserve gate, with a
fresh local re-derive) and 4d (the wake-pass sleep-policy loop:
`session_name`/`pin_awake`/`template`). **Remaining (cluster 4e+):** the rest of the
desired branch — the zombie/rollback fast-path (`~1753–1777`), `namedSessionIdentity`
in the restart-requested block (`~2024`), the maxAge/progress-stall/pendingInteraction
reads, and the `sleep_intent` field-gap (add `Info.SleepIntent` first). The config-drift
machinery + its `started_config_hash` reads (`~2165/2417`, `sessionConfigDriftKey`,
`resetConfiguredNamedSessionForConfigDrift`, `session_reconcile.go:832`) stay RAW
(whole-bead config derivation, Fork B). The apply/write-back cluster + `ProjectLifecycle`
+ circuit breaker stay raw (by design).

## Field-gaps still needed (decision-reads only)

| Key | Disposition | Sites |
| --- | --- | --- |
| `reset_committed_at`, `continuation_reset_pending` | **DONE** (`69ccc13c6`); used in Phase-1 cluster 1 (`6ccf9d698`) | `resetPendingCommittedAt`/`Info` @`session_reconciler.go:~1249` |
| `generation` | **field DONE** (`a6dea375a`): `Info.Generation string` RAW mirror (NOT `int`; Atoi/TrimSpace fidelity trap) added + the drain-advance loop site converted. The sibling wake-helper raw sites (`preWakeCommit`/`queueDrainAck…`) stay raw until later Phase-2 sub-clusters. | remaining raw: `session_wake.go:41/173/283/331/350` |
| `started_config_hash` | **field DONE** (`4dedfa476`): `Info.StartedConfigHash string` raw mirror. **RE-SCOPED:** the ONLY pure decision read was `pendingResumePreservingNamedRestart` (converted in 4c, `6e65e7f69`). The rest (`~2165/2417`, `sessionConfigDriftKey`, `resetConfiguredNamedSessionForConfigDrift`, `session_reconcile.go:832`) are entangled with `sessionCoreConfigForHash` whole-bead config derivation + unmirror­ed sub-hashes → **stay RAW** (Fork B). | done: `pendingResumePreservingNamedRestart`. raw: config-drift machinery |
| `pin_awake` | **DONE** (field `4dedfa476`, converted `806de56f5`): `Info.PinAwake` mirror; the sole decision read (wake-pass, now `~2678`) routes through it. | `session_reconciler.go:~2678` |
| `sleep_intent` | **NEW field-gap** (found in 4d): the wake-pass `hasExplicitSleepIntent` read (`~2696`) stays raw. Add `Info.SleepIntent` (raw mirror) bottom-up, then convert. | `session_reconciler.go:~2696` |
| `held_until`, `wake_request` | ProjectLifecycle machinery → **stay raw**. | — |
| `churn_count`, `core_hash_breakdown`, `started_provision_hash`, `started_launch_hash` | Write-back / config-drift-derivation machinery → **stay raw**. | `session_reconcile.go:895/923-934`, `session_reconciler.go:~2194/2206/2207` |

## Incremental execution order (each its own verified commit)

1. **DONE (`a6dea375a`) — `Info.Generation` + Phase 2 drain-advance.** See the
   Status "Phase 2" block above.
2. **The Phase-1 driver decision-read clusters** (`reconcileSessionBeadsTracedWithNamedDemand`,
   `session_reconciler.go:1024`+), cluster by cluster — derive `info` per
   iteration, **re-derive after each mutation**, convert the classifier decision
   reads (`isKnownState`→`isKnownStateInfo`, `name := info.SessionNameMetadata`,
   `resetPendingCommittedAt`→`resetPendingCommittedAtInfo`, template/named-identity
   reads, etc.). Add each remaining field-gap (`StartedConfigHash`, `pin_awake`)
   as its cluster reaches it.
   - **DONE — cluster 1 (`6ccf9d698`): loop preamble (`~1246–1275`).** The
     mutation-free top of the per-session loop (see the Status "cluster 1" block).
   - **DONE — cluster 2 (`6c1e41d1b`): pending-create rollback gate (`~1338–1357`).**
     The first block inside `!desired`. See the Status "cluster 2" block.

   **KEY RE-FRAMING (settled by clusters 2–4b):** the `!desired` orphan/suspend
   branch does NOT need re-derive until the heal. Everything from the top of
   `!desired` down to **`healStateWithRollback` (`session_reconciler.go:1491` at the
   current HEAD)** is the **pre-heal region**: every mutation reachable before the
   heal (`checkRateLimitStability` on hit/err, `attemptRollbackPendingCreate`, the
   inline `session.Status="closed"` at the failed-create close) is immediately
   followed by `continue`, and `workerSessionTargetRunningWithConfig` reads by ID.
   So control only reaches the next decision read on the still-unmutated bead, and
   the **top-of-loop `info` stayed byte-identical for the whole pre-heal region**
   (clusters 1–3, NO re-derive). The **post-heal region** (after `1491`) needed the
   `infoPostHeal` re-derive — done in clusters 4a (switch guards) + 4b (`default`
   block), which found a single top-of-switch re-derive sufficed.

   - **DONE — cluster 3 (`937beeb13`): the remaining pre-heal blocks
     (`~1414–1457`), no re-derive.** preserve-named + failed-create-close, both
     reusing the top-of-loop `info`. See the Status "cluster 3" block above for the
     converted reads, the 4 new siblings, and the verified pre-heal safety argument
     (checkRateLimitStability writes no template/agent_name/alias key; the
     failed-create reads sit behind its non-mutating `(false,nil)` return).
   - **DONE — cluster 4a (`dac68d506`): post-heal switch guards.** Re-derive
     `infoPostHeal` after the heal; converted the `preserveNamed` + post-heal
     `pendingCreateSessionStillLeased` switch arms. See the Status "cluster 4a"
     block above.
   - **DONE — cluster 4b (`8c3e600ae`): the post-heal `default` block
     (`~1550–1717`).** drain-ack / orphan-drain / suspend / close, converted through
     the 4a `infoPostHeal`. A single top-of-switch re-derive proved sufficient (full
     write-set audit; no per-branch re-derive). See the Status "cluster 4b" block.
   - **DONE — cluster 4c-foundation (`4dedfa476`): `Info.StartedConfigHash` +
     `Info.PinAwake` raw mirrors** + equivalence fixtures/cases. No reconcile-driver
     change. See the Status "cluster 4c foundation" block.
   - **DONE — cluster 4c (`6e65e7f69`): asleep-drift resume-preserve gate.**
     `pendingResumePreservingNamedRestart`→`…Info` (pure, uses `StartedConfigHash`),
     single call site with a fresh local re-derive. See the Status "cluster 4c" block.
   - **DONE — cluster 4d (`806de56f5`): wake-pass decision reads.** The read-only
     sleep-policy loop (`~2672`): `session_name`/`pin_awake`/`template`→`info`, one
     loop-top projection (no re-derive). See the Status "cluster 4d" block.
   - **NEXT — cluster 4e+: the rest of the DESIRED branch (map fresh; re-run the
     census greps, pin `git rev-parse HEAD` first).** Candidate clusters, each its own
     verified commit:
     - **Zombie/rollback fast-path (`~1753–1777`):** `shouldRollbackPendingCreate`
       (2 sites) + `pendingCreateLeaseExpiredForRollback` — siblings EXIST. **Needs a
       re-derive after the zombie-capture block:** `markProviderTerminalError` (`~1732`,
       no `continue` after it) writes `pending_create_claim:""`,
       `pending_create_started_at:""`, `last_woke_at:""` — exactly the read keys — so
       the top-of-loop info is stale for these reads. Also audit `recordResetStallIfDue`
       (`~1726`, takes the bead by value → shares the Metadata map, the by-value trap)
       for whether it writes bead metadata. Medium risk.
     - **`namedSessionIdentity(*session)` (`~2024`, restart-requested block):** sibling
       EXISTS; sits after mutations → re-derive at the call site.
     - **`sleep_intent` field-gap (`~2696`):** add `Info.SleepIntent` (raw mirror)
       bottom-up + equivalence case, then convert the wake-pass `hasExplicitSleepIntent`
       read.
     - **maxAge / progress-stall / pendingInteraction reads** (`~1922–2001`, `~2463`):
       map these fresh — `pendingInteractionKeepsAwake`, `pendingCreateStartInFlight`
       (siblings exist), `creation_complete_at` (mirror exists), `lifecycleTimerBlocker`
       (whole-`Metadata` consumer? verify). 
     - **STAY RAW:** the config-drift machinery (`~2156–2450`, `sessionConfigDriftKey`,
       `resetConfiguredNamedSessionForConfigDrift`) + all its `started_config_hash`/
       sub-hash reads (whole-bead config derivation, Fork B); the apply/write-back
       cluster (`healState*`, `checkStability`, `checkChurn`, `record*`/`clear*`,
       `persistSessionCircuitBreakerMetadata`, inline `session.Status`/`restart_requested`
       writes); `ProjectLifecycle`; the circuit breaker.
     Method unchanged: derive `info` per iteration, **re-derive after each mutation**,
     add each missing sibling bottom-up (equivalence-cased) before converting.
3. **Leave raw:** the apply/write-back cluster (`healState*`, `checkStability`,
   `checkChurn`, `record*`/`clear*`, `markProviderTerminalError`, `healExpiredTimers`,
   `persistSessionCircuitBreakerMetadata`, the inline `session.Status="closed"` /
   `restart_requested` writes), `ProjectLifecycle`, and the circuit breaker.

## Cluster 3 foundation gaps — LANDED (`937beeb13`)

All 4 siblings below shipped in cluster 3 (`937beeb13`), each equivalence-proven.
Kept as the build-order record. The original checklist follows.

Exact sibling-mirror checklist for cluster 3. All `Info` **fields** the cluster
needs already exist (`MetadataState`, `PendingCreateClaim`, `SessionNameMetadata`,
`SleepReason`, `LastWokeAt`, `ConfiguredNamedIdentity`, `Template`, `CreatedAt`) —
**no codec/struct change needed**. Add these 4 `*Info` siblings (each with an
equivalence case; build bottom-up so each composes proven leaves):

1. **`staleCreatingStateInfo(i, clk)`** — sub-leaf, TRIVIAL. Mirrors
   `staleCreatingState` (`session_reconcile.go:1201`): `clk==nil`→false;
   `strings.TrimSpace(i.MetadataState) != StateCreating`→false; else
   `pendingCreateAttemptStaleInfo(i, clk)` [EXISTS]. → `clkBoolChecks`.
2. **`sessionStartRequestedInfo(i, clk)`** — mirrors `sessionStartRequested`
   (`session_reconcile.go:187`): `TrimSpace(i.MetadataState)==StateStartPending`→
   true; `i.PendingCreateClaim`→true; `TrimSpace(i.MetadataState)!="creating"`→
   false; else `!staleCreatingStateInfo(i, clk)`. → `clkBoolChecks`.
3. **`pendingCreateSessionStillLeasedInfo(i, cfg, clk)`** — mirrors
   `pendingCreateSessionStillLeased` (`session_reconciler.go:600`): `i.PendingCreateClaim`
   branch → `pendingCreateLeaseActiveInfo` [EXISTS] + `normalizedSessionTemplateInfo`
   [EXISTS] + `i.Template` fallback + `findAgentByTemplate(cfg, …).Suspended`; else
   `sessionStartRequestedInfo(i, clk)` [gap #2] + same template/agent tail. → a new
   `cfgClkBoolChecks` group (or fold into `clkBoolChecks` with a captured `cfg`).
4. **`preserveConfiguredNamedSessionBeadInfo(i, cfg, cityName)`** — mirrors
   `preserveConfiguredNamedSessionBead` (`session_beads.go:265`): `cfg==nil ||
   !isNamedSessionInfo(i)` [EXISTS] → false; `namedSessionIdentityInfo(i)` [EXISTS] +
   `findNamedSessionSpec` + `i.SessionNameMetadata==spec.SessionName` gate; the
   terminal-state switch reads `i.MetadataState`/`i.SleepReason`/`i.LastWokeAt` via
   `parseRFC3339Metadata` + `staleCreatingStateTimeout`. → the `namedSpecCfg`-based
   `cfgBoolChecks` (reuse the real-cfg guard pattern cluster 2 added, so the "named"
   fixture actually hits the keep-alias true branch, not a trivial both-false pass).

Sanity re-check before you start (guards against staleness): the 4 above report
MISSING and their leaves (`pendingCreateAttemptStaleInfo`, `pendingCreateLeaseActiveInfo`,
`isNamedSessionInfo`, `namedSessionIdentityInfo`, `normalizedSessionTemplateInfo`,
`isFailedCreateSessionInfo`) report EXISTS via
`grep -rn 'func <name>\b' cmd/gc/ internal/session/ | grep -v _test`.

## Method (proven this stack)

Keep each original classifier/read UNTOUCHED + ADD the typed `Info` field/sibling
+ ADD an equivalence case in `TestSessionClassifierInfoEquivalence`
(`cmd/gc/session_classifier_info_equiv_test.go`) — byte-identical oracle — THEN
convert the decision read to the `Info` form via a per-iteration
`info := sessionpkg.InfoFromPersistedBead(*session)`. Raw-string mirror fields
(`Info.MetadataState`, `Info.Alias`, …) are the pattern for keys read with
`TrimSpace`/`Atoi` at the call site. For a tuple-returning classifier, add an
inline comparison in the test loop (see the `resetPendingCommittedAt` case).
Test call sites project fixtures via `session.InfoFromPersistedBead(b)`.

## Byte-identical oracle

`TestSessionClassifierInfoEquivalence` (per-classifier) **plus** the reconcile /
pool E2E suites (whole-tick behavior). Run the E2E suite after each Phase-1/Phase-2
cluster conversion — it is the real proof that the tick's decisions are unchanged.

## Gates + commit hygiene

- `go build ./...` · `go vet ./...` ·
  `golangci-lint run ./cmd/gc/... ./internal/session/...` (**0**) · the
  equivalence + guard tests · targeted reconcile/pool suites.
- `git checkout go.sum` after `go test` (spurious churn).
- Commit AND push with `--no-verify` (stale hooksPath + the pre-push hook runs
  the full suite and times out — run gates manually). Trailer:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Never `tmux kill-server`; never `go clean -cache` (`-testcache` ok); gascity
  Dolt is LOCAL-ONLY (no `bd dolt push`). `make dashboard-check` not needed
  (`Info` additions stay internal).
- #3839 stays **DRAFT** — no premature ready; the flip is multi-session and the
  broader migration (P5 closeBead, P6 delete + guard-widen) is still open.

## Provenance

Design + scope verified 2026-07-01 (CONT-5) by reconnaissance at HEAD `aea0e50fa`
(the ProjectLifecycle/CB whole-map finding, the 194-read count, the `generation`
Atoi/TrimSpace fidelity trap, the field-gap read-site classification). If you
re-run a mapping agent, pin `git rev-parse HEAD` first — an earlier census
(CONT-3) once ran against a stale out-of-tree checkout and reported false
absences.
