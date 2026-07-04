# Lockstep Drop — Final Implementation Plan (Steps 3.5 → 4 → 5 → 6e)

Verified against the live worktree at HEAD `0daa4fef7` (`/data/projects/gascity/.claude/worktrees/object-front-doors`). All line numbers re-confirmed by grep this session; re-grep before editing. Two audit-driven corrections to the doc's step boundaries, stated up front:

**Boundary correction 1 — Step 4 is real but SMALLER and should NOT use a store List.** `newSessionBeadSnapshot` IS on the decision path (exactly one reconciler call site, `session_reconciler.go:1587` inside `reconcileSessionBeadsTracedWithNamedDemand`, flipping the orphan-vs-preserve decision at ~:1711). But LOCKSTEP-DROP's "may need a store `List`" is wrong: a per-preserved-session List is a hot-path store read (violates settled decision (a)) and silently changes intra-tick visibility (the current `ordered` feed sees mid-tick closes/rollbacks). Feed it from the **live mid-tick `infoByID`** instead — byte-identical by construction because every mid-tick mutation already folds (MarkClosed + ApplyPatch).

**Boundary correction 2 — Step 5 "drop `ordered`" is over-scoped; re-scope to "demote `ordered`".** Two unenumerated consumers make physical deletion impossible without out-of-scope work: (a) `startCandidate.session *beads.Bead` (`session_lifecycle_parallel.go:145-146`, populated at `session_reconciler.go:3166` from `target.session`) escapes into `executePlannedStartsTraced`/`buildPreparedStart` — start **execution** (action, not decision) and absent from LOCKSTEP-DROP's consumer list; (b) the settled raw-by-design helpers (`resolveSessionSleepPolicy`/`configWakeSuppressed`/`persistSleepPolicyMetadata` in `session_sleep.go`) take the bead and must keep a source. Step 5's real deliverable: **zero raw decision reads + zero lockstep mirror writes on the decision path**; `ordered` survives as the load-time slice that (i) builds the tick-start snapshot and (ii) carries beads into the documented raw-by-design/action consumers. Physical deletion is a separate future initiative (see "Deferred" at the end).

Steps 3.5 and 4 are independent of each other; both must land before Step 5; 6e is strictly last.

---

## Step 3.5 — consumer #4: the wakeTargets apply loop + the awake bridge

**Goal:** no raw `target.session.Metadata[...]` decision reads remain in `session_reconciler.go`'s post-Phase-1 region (:3079-:3297), `selectIdleProbeTargets`/`launchIdleProbes` (:4369-:4434), or `compute_awake_bridge.go`; every mutation in the region folds onto `infoByID`. **Raw mirrors are NOT deleted in this step** (they die in Step 5) — 3.5 adds the typed replacements and flips the reads, per the governing principle (convert, verify, THEN delete).

### Commit 3.5a — additive codec prep (internal/session)

- Add `Info.PendingCreateClaimMetadata string` = `b.Metadata["pending_create_claim"]` **verbatim untrimmed** (the `WakeAttemptsMetadata` 6a precedent; `Info.PendingCreateClaim` at `info_store.go:91` is a bool and cannot reproduce a non-canonical raw value like `"yes"` in trace payloads). Wire into `InfoFromPersistedBead` + `info_apply_patch.go` + extend the codec oracle (`TestCircuitStateFromMetadataProjectsVerbatim`-style; add a non-canonical-value case).
- Do **NOT** add mirrors for the 7 sleep-policy keys, `worker_dir`, or `sleep_policy_fingerprint` — their only consumers stay raw-by-design (below).

Byte-identity: additive-only; zero behavior change. Verification: `go test ./internal/session/... -count=1`, oracle extended.

### Commit 3.5b — session_reconciler.go read flips + folds

Pure read flips (all Info fields are verbatim raw-string mirrors; no same-tick writer precedes the read, so `infoByID` == raw at each site):

| Site | Conversion |
|---|---|
| :3083 `name := target.session.Metadata["session_name"]` | `info.SessionNameMetadata` (the loop already builds `info := infoByID[target.session.ID]` at :3038 in the first loop; hoist the same lookup at the top of the second loop) |
| :3095 `isFailedCreateSessionBead(*target.session)` | `isFailedCreateSessionInfo(info)` (`build_desired_state.go:3825`, equivalence-proven) |
| :3098, :3109 trace `strings.TrimSpace(...Metadata["pending_create_claim"])` | `strings.TrimSpace(info.PendingCreateClaimMetadata)` (needs 3.5a) |
| :3103 `sessionIsQuarantined(*target.session, clk)` | `sessionIsQuarantinedInfo(info, clk)` (`session_reconcile.go:1062`) |
| :3106 `pendingCreateStartInFlight(...)` | `pendingCreateStartInFlightInfo(info, clk, startupTimeout)` (`session_reconciler.go:786`) |
| :3110 trace `Metadata["last_woke_at"]` | `info.LastWokeAt` |
| :3120 `namedSessionIdentity(*target.session)` | `namedSessionIdentityInfo(info)` (`named_sessions.go:59`) |
| :3181 `Metadata["wake_mode"] == "fresh"` | `info.WakeMode == "fresh"` |
| :3193 `cancelSessionDrain(*target.session, sp, dt)` | new thin `cancelSessionDrainInfo(info, sp, dt)` wrapping the existing `cancelSessionDrainIfInfo` (`session_wake.go:295`) with the `drainReasonCancelable` predicate — the raw version already re-derives Info internally, and with all folds landed `infoByID` is byte-identical to that re-derive |
| :3195 sleep_intent read, :3203 `intent :=` | `info.SleepIntent` (keep the local `intent` capture at :3203 — the :3232-era trace deliberately prints the PRE-write value) |
| :3221 `shouldBeginIdleDrain(target.session, ...)` (def :4314) | new sibling `shouldBeginIdleDrainInfo(info, eval, dt, sp)` — reads only `ID` + `SessionNameMetadata` |
| :3228 `beginSessionDrain(*target.session, ...)` | new sibling `beginSessionDrainInfo(info, ...)` — reads only session_name + generation, both verbatim on Info (keep the raw form for its other caller until Step 5) |
| :3229, :3231, :3258 prints/trace `Metadata["session_name"]` | `info.SessionNameMetadata` |
| :3253 `isPoolSessionSlotFreeable` + `isPoolManagedSessionBead` | `isPoolSessionSlotFreeableInfo` (`session_state_helpers.go:86`) + `isPoolManagedSessionInfo` (`session_name_lookup.go:41`) |
| :3285 `TrimSpace(Metadata["sleep_reason"])` | `strings.TrimSpace(info.SleepReason)` (all same-tick sleep_reason writers already fold) |
| :4372 `selectIdleProbeTargets` sleep_intent, :4426 `launchIdleProbes` session_name | pass `infoByID` (or `infoLookup func(string)(Info,bool)`, the Step-2b pattern) into both helpers; read `info.SleepIntent` / `info.SessionNameMetadata`. **The audits under-enumerated :4426** — it is a live raw read; include it. |

Fold additions (write+fold as ONE unit; raw mirrors stay):

- **:3164 / :3190 `recordCurrentBeadIDOnWake`** (`session_bead_cycle.go:21`): have it return the mirrored batch (or fold at call site): `infoByID[id] = infoByID[id].ApplyPatch({CurrentBeadIDKey: beadID})` on success. Idempotence read inside converts to `Info.CurrentlyProcessingBeadID`. **Keep the raw mirror**: at :3164 the freshly-mutated bead pointer is appended to `startCandidates` at :3166 and `buildPreparedStart` reads its metadata later this tick — a live same-tick raw coupling.
- **:3182 `cycleAliveSessionForFreshReassign`** (`session_bead_cycle.go:59`): fold its returned/mirrored batch onto `infoByID` **EXCLUDING `ResetCommittedAtKey`** — exactly mirror the helper's own existing exclusion (#2345: `reset_committed_at` must stay off this tick's snapshot; the `restartFold` at :2337 is the precedent). Currently benign un-folded (branch `continue`s), but fold now so the snapshot never diverges.
- **:3195-3197 sleep_intent clear**: read → `info.SleepIntent`, keep `SetMarker` + raw mirror, add `infoByID[id] = infoByID[id].ApplyPatch({"sleep_intent": ""})`. Same-tick safe: :3203 is the mutually-exclusive `!shouldWake` arm; `selectIdleProbeTargets` already ran at :3068; Phase-2 never reads `Info.SleepIntent`.
- **:3225 `markIdleSleepPending`** (`session_sleep.go:312`): fold `{"sleep_intent": "idle-stop-pending"}` onto `infoByID` (return-batch shape). Idempotence read → `Info.SleepIntent`.
- **:3272 `emitSessionStrandedDiagnostic`** (def :3606): throttle read → `Info.StrandedEventEmittedAt` (`info_store.go:128`); have the helper return the marker fold and apply it to `infoByID` **UNCONDITIONALLY, before/regardless of the SetMarker result** — the in-memory-marker-BEFORE-store-write ordering is a deliberate emit-once guard and the fold must reproduce it. The internal `collectSessionAssignedWork` whole-bead pass stays raw (see leaves, below).
- **:3289 `closeBead`**: on `true`, `infoByID[id] = infoByID[id].MarkClosed()` (store-only close family → MarkClosed ONLY, no ApplyPatch of a store-only batch — the settled close-family rule).

**Deliberately left raw in 3.5 (raw-by-design, documented in-code at :3036-3037 and in LOCKSTEP-DROP.md):**
- :3039 `resolveSessionSleepPolicy(*target.session, cfg, sp)` and :3043 `configWakeSuppressed(...)` — whole-bead + runtime/config helpers; `sleep_policy_fingerprint` has no Info mirror (codec gap left unpaid on purpose). Read-order safe: the :3043 fingerprint read is in the FIRST loop, before :3091 rewrites it in the second.
- :3091 `persistSleepPolicyMetadata(target.session, sessFront, ...)` (`session_sleep.go:263`) — write-path idempotence (7-key diff gate off raw metadata), reclassified by STEP6-DESIGN §5; keeps bead + its own mirror. This is why `wakeTarget` keeps carrying `session *beads.Bead`.
- :3256 `sessionHasOpenAssignedWorkForReachableStore` (def :3445) — read-only store-query helper with a wide identifier/rig-routing key surface; no mirror, no snapshot-divergence risk. **Decision: documented guard exception, not an Info sibling** (revisit only if identity keys ever gain same-tick writers — today they're written at create/rollback, and the rollback folds exist).
- :3294 `pruneAgentHomeWorktreeIfSafe` — action (fs/git pruning) on the freed slot, not a decision read; `worker_dir` codec gap left unpaid.
- :3316 `executePlannedStartsTraced(startCandidates)` and the :3166 append — start execution; addressed as the Step-5 demotion rationale.

### Commit 3.5c — compute_awake_bridge.go

- Build `infoBy := make(map[string]session.Info, len(sessionInfos)); for _, in := range sessionInfos { infoBy[in.ID] = in }` **inside** `buildAwakeInputFromReconciler` (keying by unique ID is safe; the never-`range infoByID` rule protects the non-unique SessionName ordering, and iteration here still walks `wakeTargets` in slice order). Do NOT widen the function signature.
- :168 → `info, ok := infoBy[target.session.ID]; name := strings.TrimSpace(info.SessionNameMetadata)`. Miss (`!ok`) → zero Info → `name==""` → `continue`, same skip as today; impossible in prod (wakeTargets appended only from `&ordered[i]` at :2985, every ordered ID keys infoByID). The one bridge test (`compute_awake_bridge_test.go:243`) already supplies the matching Info.
- `shouldProbeAttachmentForAwakeInput` (:188-210): signature → `(info session.Info, alive bool, cfg *config.City, poolDesired map[string]int)`. No external callers (grep: 2 hits, this file only). The `target.session == nil` guard (:189) becomes the caller's `!ok` → `false`.
  - :195 `Metadata["state"]` → **`info.MetadataState`, NOT `info.State`** — `Info.State` is the normalized/closed-blanked form; using it can flip the probe verdict for a closed bead whose raw state is `"active"`. This is the byte-identity landmine of the whole step.
  - :199 `Metadata["detached_at"] != ""` → `info.DetachedAt != ""`.
  - :202 `normalizedSessionTemplate` → `normalizedSessionTemplateInfo(info, cfg)` (`session_name_lookup.go:585`, equivalence-proven).
  - :204 fallback `template = Metadata["template"]` → `info.Template` — provably dead (normalizedSessionTemplate's own final fallback IS `Metadata["template"]`, so it returns `""` only when the key is `""`); convert mechanically, note deadness in the commit message, delete in a later cleanup if desired.

### Step 3.5 byte-identity argument
Every converted read maps to a **verbatim raw-string** Info field, and `infoByID` is coherent at every read point because all Phase-1 forward-pass mutations already fold (Steps 1-3 + 6d) and this step adds folds for the region's own mutations as one unit with their writes. The two non-obvious semantics are preserved explicitly: the `ResetCommittedAtKey` exclusion (cycleAlive fold) and the unconditional-before-store stranded fold.

### Verification
`gofmt -l cmd/gc internal/session` empty; `go vet ./cmd/gc/... ./internal/session/...`; `go build ./cmd/gc/`; targeted `go test ./cmd/gc/ -count=1 -run 'Reconcil|Awake|Drain|Sleep|Wake|IdleProbe|Stranded|Quarantin|PendingCreate|FreshReassign|PoolSession'` then the full comprehensive reconciler suite (the ~212s run) + `make test`. Fable red-team should try to refute: (1) `MetadataState` vs `State` at the probe gate; (2) a fold that includes `ResetCommittedAtKey` (force-wake #2345 repro: on-demand session + fresh-cycle same tick, assert not woken); (3) the stranded emit-once guard when `SetMarker` errors (assert single emit); (4) `launchIdleProbes`/`selectIdleProbeTargets` coverage (the audits under-counted these — re-grep `target.session` for stragglers, e.g. `:4381`+`:4388` are ID-only and fine).

### Risk call-out
The single most likely silent break: using `Info.State` (normalized) instead of `Info.MetadataState` (raw) anywhere in the probe/state gates — it type-checks, passes most tests, and flips attachment probing only for closed-blanked beads.

---

## Step 4 — consumer #5: the preserve-path template subsystem

**Goal:** the ONE reconciler call site (`session_reconciler.go:1587` → `resolvePreservedConfiguredNamedSessionTemplate` :3381-3423) stops consuming `ordered []beads.Bead` and the raw `*session`; fed from the live mid-tick `infoByID` instead. The bead-taking `newSessionBeadSnapshot` **survives** for the CLI/status/tick-entry/lifecycle-parallel callers (cmd_citystatus.go:389, cmd_session.go:920, providers.go, city_runtime.go:2932/3060, session_lifecycle_parallel.go:809, etc.) — those are store-List-sourced at entry and off the reconciler decision path; do not touch them.

### Commit 4a — additive codec: `Info.Pack`
`resolveTemplateForSessionBead` (`build_desired_state.go:3041-3069`) reads `beadmeta.PackMetadataKey` (:3065), which has ZERO Info presence (grep-confirmed: no pack mirror in `info_store.go`). Add `Info.Pack` verbatim mirror + ApplyPatch wiring + oracle case. (session_name → `SessionNameMetadata`, TriggerBeadID/StoreRef already mirrored at `info_store.go:82-83`.)

### Commit 4b — Info-source constructor + reconciler call-site flip
- `newSessionBeadSnapshotFromInfos(infos []session.Info) *sessionBeadSnapshot` beside the raw constructor (`session_bead_snapshot.go:97-163`): `b.Status=="closed"` filter → `info.Closed` (equivalent — every mid-tick close folds MarkClosed per the close-family discipline landed in 6d Commits 1-2); index builds via the existing proven siblings `sessionBeadAgentNameInfo` (:440), `isPoolManagedSessionInfo`, `isCanonicalPoolManagedSessionInfoForTemplate`, `stampedPoolQualifiedIdentityInfo` (:367). All index keys (session_name/template/alias/agent_name/common_name/configured_named_identity) are in the ApplyPatch codec (audit-verified against `info_apply_patch.go`), so mid-tick rollback folds keep the Info feed in lockstep.
- `resolvePreservedConfiguredNamedSessionTemplate` gains an Info-taking form: identity via `namedSessionIdentityInfo(info)` (:3399's `namedSessionIdentity(session)`), `bp.sessionBeads = newSessionBeadSnapshotFromInfos(openInfos)` (:3403). Add an Info sibling for `resolveTemplateForSessionBead`'s 4 reads (`SessionNameMetadata`, `TriggerBeadID`, `TriggerBeadStoreRef`, `Pack`); keep the bead-taking version for the apply-path callers (rediscovery/pool-realize/lifecycle-refresh).
- Reconciler site :1587: build the `[]session.Info` **in `ordered` order from the live `infoByID`** (`for i := range ordered { infos = append(infos, infoByID[ordered[i].ID]) }` or reuse the Step-3 pattern) — NOT a tick-start frozen copy, NOT a store List. The downstream `FindSessionNameByTemplate` (`session_name_lookup.go:399`) and `template_resolve.go:241-247` OpenInfos scan are unchanged semantically; the pre-existing store fallback at `template_resolve.go:249` stays as-is.

### Byte-identity argument
Today's feed is the mid-tick `ordered` slice, which sees earlier-iteration lockstep mutations (closes, rollback identity clears). `infoByID` reproduces exactly those mutations via the MarkClosed/ApplyPatch folds — byte-identical by construction. A store List would NOT be (it would also see store-only writes the raw feed doesn't, and misses nothing else), which is the second reason to reject it beyond the hot-path read ban.

### Dependency ordering
Independent of 3.5 (can land before/after). Must land before Step 5 (`:1587` is the last wholesale `ordered` flow into a whole-bead subsystem).

### Verification
Same gate battery; targeted `-run 'PreservedConfiguredNamed|SessionBeadSnapshot|ResolveTemplate|NamedSession'`; plus a new unit test: construct beads, mutate mid-tick (close one, rollback-clear another), assert `newSessionBeadSnapshotFromInfos(folded infos)` == `newSessionBeadSnapshot(mutated beads)` index-for-index (the equivalence oracle for this step). Fable red-team: try to find a snapshot index key written mid-tick by a fold-less path (the store-only close family — assert MarkClosed-only sites don't need the cleared keys because `Closed` eviction covers them); try a preserve decision that flips when fed tick-start-frozen Infos (proves the live-feed requirement).

### Risk call-out
Feeding a tick-start frozen `[]Info` (or a store List) instead of the live mid-tick `infoByID`: an earlier-iteration in-memory close would still look open to the preserve path, flipping orphan-vs-preserve. The equivalence unit test above is the tooth.

---

## Step 5 — consumer #6: mirror drop + `ordered` demotion

**Precondition:** 3.5 and 4 landed. Sequence as five commits, each independently green.

### Commit 5a — remaining live forward-pass decision reads → Info
All have verbatim Info fields; none has an unfolded same-tick writer:
- :2283 `beadRequested := session.Metadata["restart_requested"]=="true"` → `infoByID[session.ID].RestartRequested == "true"`, **in the same commit as deleting the raw write at :2252** (`session.Metadata["restart_requested"]="true"`; the fold at :2266 is already the carrier — the in-code comment documents exactly this plan). Exposure-set member: overlay must still CLEAR on every persisted restart_requested batch (already handled by the :2337 restartFold + drain-ack clears; do not change). Add/keep the kill-success-then-refresh test asserting restart_requested reads empty (#2574 gate).
- :2488/:2522-2523/:2534-2535/:2775/:2804/:2831 → `Info.StartedConfigHash`/`CoreHashBreakdown`/`StartedProvisionHash`/`StartedLaunchHash`/`CreationCompleteAt` (rebaseline folds @2511 etc. keep these current).
- :2700 `started_live_hash` → `Info.StartedLiveHash` — exposure-set member, but raw and snapshot go stale IDENTICALLY (its writers persist without mirror AND without fold; neither view sees them same-tick), so byte-identical. This becomes the field's first reader; note it in LOCKSTEP-DROP.md.
- :1659-1661/:2369-2371 pre-heal trace baselines → `Info.MetadataState`/`PendingCreateStartedAt`/`LastWokeAt`, **captured BEFORE applying the heal fold** (trivial order preservation).
- :1622-1624/:1730-1732 trace payloads → `Info.PendingCreateClaimMetadata` (3.5a) + `Info.MetadataState`.
- :4278-4279 `session_key`/`started_config_hash` reads in the config-drift reset → `Info.SessionKey`/`StartedConfigHash` (same point-in-time semantics the docstring already blesses).
- :4021-4024/:4050-4056 drift-deferral throttle reads → `Info.ConfigDriftDeferredAt/Key`/`AttachedConfigDriftDeferredAt/Key` (store-only keys, both views hold tick-start values; worst divergence = one redundant store write, never a decision flip).
- :1323 Phase-0 dedupe `b.Metadata["session_name"]` — runs pre-snapshot; convert via per-bead `InfoFromPersistedBead(b).SessionNameMetadata` (the blessed `computeNamedSessionProgressSignatures` pattern) OR document as a Phase-0 exception. Recommend converting (one line, kills a 6e carve-out).

### Commit 5b — the drain-ack finalize family off the raw bead
`finalizeDrainAckStoppedSession` (:363-) and `markDrainAckStopPending` (:82-105) drop their `*beads.Bead` params:
- :461-472 reads → `Info.WakeMode`/`RestartRequested` (verbatim).
- Witness arm :434-450 → `InfoFromPersistedBead(latest)` directly (source is the store bead, not `ordered`; the wholesale `session.Status/Metadata = latest.*` swap dies with the param).
- Mirror loops :414-418, :482-486, :96-105 die with the params; callers keep folding the returned `drainAckFinalizeResult`/reconstructed batch exactly as today.
- **Gate:** verify the non-reconciler caller `finalizeDrainAckStopPendingSessions` (:539-575, via `city_runtime.go:1153`) — its local `[]beads.Bead` lockstep must be read-dead, and it keeps the accepted per-bead `InfoFromPersistedBead` boundary projection (same pattern as the advanceSessionDrains wrappers). Its raw session_name read @:565 converts with the projection.

### Commit 5c — delete the raw lockstep mirror loops
Top-level in session_reconciler.go: :2327-2337 (restart handoff — **the raw mirror and `restartFold` share ONE loop with the `ResetCommittedAtKey` skip; when deleting the raw half, the surviving fold loop must retain the exclusion verbatim** — this is where #2345 lives), :2885-2898, :2966-2979, :3197 sleep_intent clear, :4301-4306, :4691-4698, :4805-4812; plus helper-internal mirror loops in session_reconcile.go/session_sleep.go (except `persistSleepPolicyMetadata`, raw-by-design)/session_lifecycle_parallel.go/session_bead_cycle.go/session_wake.go:76 **as their `*beads.Bead` params convert**. Per-key discipline before each deletion: grep the mirrored key for any remaining this-tick raw reader, **including the start-execution path** (`buildPreparedStart` reads/writes `session_key`/`instance_token`/`last_woke_at` at session_lifecycle_parallel.go:916/1005/1539...). Mirrors whose keys feed `startCandidates`-reachable reads **SURVIVE** (at minimum `recordCurrentBeadIDOnWake`'s CurrentBeadIDKey mirror at :3164 and any key `buildPreparedStart` consumes) — document each survivor as "start-execution coupling" in LOCKSTEP-DROP.md. `cycleAliveSessionForFreshReassign`'s mirror is droppable (its branch `continue`s; the bead never enters startCandidates).

### Commit 5d — drop the dead `sessionBeads` param
`advanceSessionDrainsWithSessionsTraced` (`session_wake.go:462-478`): prod-dead confirmed (`wakeEvals` built at :3022 via `awakeSetToWakeEvals`, always non-nil; the `wakeEvals==nil` fallback callers `advanceSessionDrains`/`WithSessions` have zero non-test callers). Move the `computeWakeEvaluations` fallback into those test-only wrappers (they keep their own `[]beads.Bead` + per-bead Info projection at the boundary — do NOT delete `computeWakeEvaluations`/`evaluateWakeReasons`; STEP6-DESIGN §6 keeps them for the CLI wake column).

### Commit 5e — demote `ordered`
- Introduce `orderedIDs []string` (or keep using `ordered[i].ID`) for the two order-sensitive rebuilds: the `sessionInfos` build (:3013-3015) and the Step-4 preserve feed. **Never `range infoByID`** for either (SessionName last-write-wins).
- `openPoolSessionCountForTemplate` (@:2212) may domain-switch to `range infoByID` (order-independent count, unique IDs — plan-blessed).
- After 5a-5d, grep `ordered` and `session.Metadata\[` in session_reconciler.go: the ONLY survivors must be (i) tick-start `infoByID`/topoOrder/Phase-0.5 builds (:1338, :1356-1386, :1412-1414 — pre-snapshot typed projections, blessed), (ii) `&ordered[i]` handoffs into the documented raw-by-design helpers and `startCandidate`, (iii) the surviving start-coupled mirrors from 5c. Document that list verbatim in LOCKSTEP-DROP.md as the final raw census.

### Byte-identity argument
Each mirror deleted in 5c has (by 3.5/4/5a/5b) zero remaining raw readers — deleting a write nobody reads cannot change behavior. Each read converted in 5a targets a field whose every same-tick writer folds. The two exposure-set members are handled by existing landed mechanics (restartFold exclusion; overlay-clear-on-persist), and `started_live_hash` diverges identically on both views.

### Verification
Full battery per commit; the comprehensive reconciler suite after 5c and 5e. **The three injected-error gates must stay green with zero modification**: `session_reconciler_test.go:7661/:7833` + `session_reconciler_progress_test.go:202` (ProgressStallDoesNotRecycle / attachment_check_error_fails_safe) — they prove no forward-pass store Get crept in. Fable red-team: (1) try to force-wake an on-demand session same-tick after a fresh-cycle (ResetCommittedAtKey exclusion survived the loop merge?); (2) kill-failure path: restart_requested must survive on the snapshot through the `continue` and re-fire correctly; (3) start a session whose bead was mutated by a dropped mirror — does `buildPreparedStart` see stale metadata? (the 5c per-key census tooth); (4) run the non-reconciler `finalizeDrainAckStopPendingSessions` pass end-to-end.

### Risk call-out
Deleting a mirror whose key the start-execution path reads through the escaped `startCandidate` pointer. It's invisible to the reconciler suite's decision assertions (start execution is downstream) and only surfaces as a wrong `session_key`/instance-token/lease on an actual start. The 5c per-key grep census — explicitly including session_lifecycle_parallel.go — is the only defense; do it key-by-key, not loop-by-loop.

---

## Step 6e — the guard (strictly last)

Extend `frontdoor_di_guard_test.go`:
- **Needle choice is load-bearing** (plain substring scan): use `"session.Metadata["` — it matches `session.Metadata[` and `target.session.Metadata[` but NOT the work-bead reads, which are `item.bead.Metadata[` (:3665), `b.Metadata[` (:4500, :4562), `bead.Metadata[` (:4597) — grep-verified. Also add `".session.Metadata["` for belt-and-braces on other receivers if any appear. The witness swap (`session.Metadata = latest.Metadata`, no bracket) is gone by 5b anyway.
- Files: add `session_reconciler.go`, `compute_awake_bridge.go`, and `session_reconcile.go`/`session_bead_cycle.go` if raw-free after 5c, to `snapshotInfoOnlyFiles` (:83-91) with the new needles alongside the existing 4 raw snapshot accessors (:97-102).
- **Do NOT list**: `session_sleep.go` (resolveSessionSleepPolicy/configWakeSuppressed/persistSleepPolicyMetadata), `session_wake.go`/`cmd_session.go` (evaluateWakeReasons family), `session_lifecycle_parallel.go` (start execution), `session_worktree_prune.go`, `session_beads.go`, files holding the raw classifier oracle siblings (STEP6-DESIGN §6 forbids deleting them), and `session_bead_snapshot.go` (bead constructor legitimately survives for CLI/tick-entry).
- If any surviving 5c start-coupled mirror keeps a `session.Metadata[` write in session_reconciler.go, either relocate that write into a named helper in an unguarded file or hold session_reconciler.go out of the needle list for the write needle only — record whichever in LOCKSTEP-DROP.md; do not silently widen exceptions.
- Verification: the guard test itself + a deliberate revert-canary (add a raw read locally, confirm the guard fails, remove). Fable red-team: hunt for raw session-bead reads reachable from `reconcileSessionBeadsTracedWithNamedDemand` in files NOT on the guard list (the guard's real hole is file scoping, not needles).

Risk call-out: a needle that matches the oracle siblings or work-bead reads gets "fixed" by weakening the guard instead of scoping it — scope by file + session-receiver needle, never by deleting oracle code.

---

## Definition of done

`grep -n 'session\.Metadata\[' cmd/gc/session_reconciler.go cmd/gc/compute_awake_bridge.go cmd/gc/session_reconcile.go` returns only the documented raw-by-design census recorded in LOCKSTEP-DROP.md (expected: zero in the bridge; only start-coupled survivor mirrors, if any, in the reconciler); `grep -n 'target\.session\.Metadata\|\*beads\.Bead' cmd/gc/session_reconciler.go` shows raw beads flowing ONLY into the census'd raw-by-design helpers, `startCandidate`, and the tick-start snapshot build; the extended `TestSnapshotInfoOnlyFilesStayOnInfoAccessors` enforces it in CI; the comprehensive reconciler suite, the three injected-error fail-safe tests, `TestSessionClassifierInfoEquivalence`, the new Step-4 snapshot-equivalence oracle, and `make test` + `go vet ./...` are all green; and a final fable 4-lens review over the Step-5 diff reports zero confirmed byte-identity defects.

**Deferred / raw-by-design forever:** (1) `startCandidate`/`executePlannedStartsTraced`/`buildPreparedStart` — start execution stays on raw store-loaded beads; converting it is a separate initiative and the sole blocker to physically deleting `ordered` (add it to LOCKSTEP-DROP.md as consumer #7, explicitly out of scope). (2) `resolveSessionSleepPolicy`/`configWakeSuppressed`/`persistSleepPolicyMetadata` + the `sleep_policy_fingerprint`/7-key codec gap — raw-by-design per settled decision (c). (3) `sessionHasOpenAssignedWorkForReachableStore`/`collectSessionAssignedWork` — read-only store-query helpers, documented guard exception. (4) `pruneAgentHomeWorktreeIfSafe` + the `worker_dir` codec gap — action helper. (5) The `evaluateWakeReasons`/`computeWakeEvaluations` fallback family and raw classifier oracle siblings — keep alive. (6) The dead `template` fallback at compute_awake_bridge.go:204 — converted mechanically; optional deletion later. (7) `started_live_hash`/`live_hash` folding — carry forward un-folded; re-confirm only if a per-refresh store Get is ever (dis)allowed, which it is not.
