# Step 6 design — retire the raw lockstep + raw working set

Status: DESIGN (fable 4-lens audit → opus synthesis). Branch
`upstream/object-front-doors-cleanup`, PR #3839, base HEAD `48ab9e107`.

Step 6 is the finale of the reconciler front-door migration: make the typed
`infoByID` snapshot the tick's single source of truth, drop the raw
`session.Metadata[k]=v` lockstep mirrors, remove the raw `ordered []beads.Bead` /
`beadByID` / `circuitSessionByIdentity` working set, and add the reconciler files
to the `snapshotInfoOnlyFiles` CI guard. It is a LANDMINE — a wrong Get-cutover
reintroduces the #2345 force-wake and #2574 phantom-restart production bugs.

This design was grounded by a fable 4-lens audit of the raw/lockstep surface
(`raw/step6-*` — audits A/B/C/D). Every claim below carries a verified file:line.

---

## 1. The Get-cutover exposure set (VERIFIED COMPLETE)

`refreshSessionInfo(id)` (session_reconciler.go:1353) today re-projects the snapshot
from the **raw in-memory bead** (`InfoFromPersistedBead(*beadByID[id])`). Flipping it
to the store-authoritative `sessFront.Get(id)` exposes exactly the keys that the raw
projection deliberately hid this tick. Audit A classified all 24 `ApplyPatch` sites +
7 `SetMarker` sites; the complete exposure set read off the `Info` snapshot is:

| key | why the raw bead hid it | who reads it off the snapshot | handling |
| --- | --- | --- | --- |
| `reset_committed_at` | persisted by `RestartRequestPatch` (lifecycle_transition.go:387) at **two** sites — session_reconciler.go:2144 (mirror skips it @2156) AND session_bead_cycle.go:100 (mirror skips it @114); the durable marker is for the NEXT tick (#2345 force-wake prevention) | `compute_awake_bridge.go:140` (`info.ResetCommittedAt`) + `resetPendingCommittedAtInfo`@126 (via `infoByID`@1411) | **freeze to tick-start value** (see §2) |
| `restart_requested` | set **in-memory only** at session_reconciler.go:2098 (progress-stall), never persisted | `compute_awake_bridge.go:138` (`info.RestartRequested`) | **in-memory intra-tick overlay** (see §2) |

**Everything else persisted-without-mirror is UNREAD off `Info`, so needs NO
suppression** (Audit A/D confirmed by grep):
- `started_live_hash` — `Info.StartedLiveHash` exists but has **zero** readers; the
  only reader is the raw `session.Metadata["started_live_hash"]`@2455. `live_hash` is
  not an `Info` field. (persist-without-mirror @2462-2465, 2488-2491)
- `config_drift_deferred_*` / `attached_config_drift_deferred_*` — zero `Info`
  readers (raw reads @3703-3713 are a §4 6c concern, not a Get-flip exposure).
- the `session_circuit_*` cluster cleared by `clearPersistedSessionCircuitBreakerMetadata`
  — not in the `Info` codec (Step 5 owns it via `CircuitState`).

**`reset_committed_at` is tick-start-invariant** (VERIFIED): the *only* writer is
`RestartRequestPatch`, both call sites skip the mirror, and no locksteped batch ever
writes it → the raw bead's value never changes during the tick. So a value captured
once at snapshot build equals the raw projection at every refresh point.

**`restart_requested` in-memory set is the only one** (Audit B): 2098 is the sole
in-memory-only decision write read off `Info`. `stranded_event_emitted_at`@3374 is
in-memory-first but read-off-info=NO. The store-persisted writers (drain-ack clear
@395 lockstep, cmd_handoff @378) are seen by a Get.

**Persist-error caveat (opus review, HIGH-adjacent):** the byte-identical guarantee is
**happy-path only**. Several sites run the raw-mirror loop **unconditionally even when
`ApplyPatch` errors** — notably `healStateWithRollback` (session_reconcile.go:1063-1068:
error only logged @1064, mirror @1066-1068 runs regardless), writing `state` /
`pending_create_claim`, both read off `Info` via `infoPostHeal` (~1596). On a persist
FAILURE a store `Get` would diverge from the raw projection. The **write-returns-`Info`**
cutover (§2) sidesteps this entirely (it never `Get`s — it reflects the patch that was
returned), so this is a doc-scoping caveat, not a cutover blocker. The recording-fake
write oracle (healthy persists) proves the happy path.

**`Status`/`Closed` is a THIRD divergence class, NOT a metadata key (opus review, HIGH).**
`Info.Closed` derives from `b.Status == "closed"` (info_store.go:28), never from an
`ApplyPatch` batch — so no returned patch and no metadata overlay can carry it. In-memory
status closes (`session.Status = "closed"` after
`closeSessionBeadIfReachableStoreUnassigned`, ~1553-1554) and, worse,
`rollbackPendingCreate` (session_lifecycle_parallel.go, closes in the **store only**,
never sets the raw bead's Status — so today's raw-bead pre-pass does not even reflect it)
mean any refresh whose job is `Info.Closed` needs a **store re-read**, not a returned
patch. `!Info.Closed` is read cross-session by `openPoolSessionCountForTemplate`
(session_progress.go:21) mid-forward-pass (~2058). The cutover model (§2, §3 6d) must
treat status-close refreshes as a read case.

---

## 2. The intra-tick model + why the Get-cutover is DEFERRED (evidence-based correction)

The intra-tick divergences a store-authoritative refresh must reproduce are exactly
two (§1): `reset_committed_at` (freeze to its tick-start value) and `restart_requested`
(in-memory overlay). That model is correct. **But the naive "flip `refreshSessionInfo`
to `sessFront.Get`" implementation was tried and REVERTED** — it is the wrong shape for
the cutover:

- **Zero benefit during coexistence (DECISIVE).** While the lockstep is still mirroring,
  the raw bead is authoritative-equivalent, so refreshing the snapshot *from the raw bead*
  is correct AND free AND perturbs no store I/O. The store-authoritative refresh is only
  *needed* once the lockstep is removed — so there is nothing to gain by flipping now.
- **Consumes test-injected Get-errors (REAL fail-safe hazard).** The refresh at
  session_reconciler.go:1854 (feeds `infoPostZombie`) is **unconditional per forward-pass
  iteration**; `sessFront` and the raw attachment-check store are the same injected
  wrapper, so a `Get` there consumes a `store.Get(sessionID)` error meant for a downstream
  fail-safe check *before* it fires — changing production read semantics, not just a test.
  This is verified end-to-end: the reverted attempt failed
  `TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions/attachment_check_error_fails_safe`.
  **Scale (corrected, opus review):** exactly **3 injection cases in 2 files**
  (`session_reconciler_test.go:7661,7833`; `session_reconciler_progress_test.go:202`) —
  the mechanism is real, the blast radius is not suite-wide.
- **Suspected per-tick Get storm (benchmark pending, NOT load-bearing).** The 1854 refresh
  + the blanket pre-pass @2743 would add ≥1 store read per session per tick. Spec §4.3/§8
  pre-accepts "refresh-on-write, fix if hot" — gated on a benchmark **never run** — so this
  is "suspected," not a proven regression. The revert rests on the two reasons above.

**Conclusion:** the cutover belongs bundled with the lockstep drop (§3 6d), done
primarily via **write-returns-`Info`** (spec §4.3 escalation) — the ~24 `ApplyPatch`
sites know the patch they wrote, so `refreshSessionInfo` reconstructs the post-write
`Info` from the returned patch instead of a blanket `Get`. **BUT write-returns-`Info` does
NOT cover every refresh (opus review, load-bearing):** the ~11 refresh sites split into
- **adjacent-single-write** (e.g. ~1596 post-heal) — write-returns-`Info` works;
- **status-close** (1558/1703/1802/2013 + the rollback half of the 2743 pre-pass) — need a
  **targeted store re-read** (`Info.Closed` from `Status`, §1);
- **aggregating** (2521 asleep-drift, 2767 wakeTargets) — reflect cumulative prior-block
  mutations with no single returnable write; need a **targeted re-read** too.

So 6d is write-returns-`Info` **for adjacent writes + a targeted `Get` for status-close /
aggregating refreshes** — not a pure returned-patch model. The two overlays
(`reset_committed_at` freeze at snapshot build, `restart_requested` at 2098) still apply.

---

## 3. Ordered sub-phase backlog (re-sequenced: safe conversions first, cutover last)

- [ ] **6a — codec fidelity gaps** (the new first step; additive, byte-identical, NO
  store-I/O change). Add the `Info` mirrors the remaining raw decision reads need,
  each with a `TestSessionClassifierInfoEquivalence` case. The three genuine codec gaps
  (verified absent from `Info`): `session_id_flag` (freshRestartSessionKey ~2139),
  `template_overrides` (ParseTemplateOverrides ~3918), and the `wake_attempts` fidelity
  gap (raw `!="" && !="0"` ~857 vs the int-parsed `Info.WakeAttempts`, which needs a
  raw-string mirror to stay identical). NOTE (opus review): `isDrainAckStopPending`
  (~54) is NOT a codec gap — `Info.MetadataState`/`StateReason` already exist, so its
  `*Info` sibling belongs in 6b.
- [ ] **6b — convert the residual raw decision reads to `Info`** (Audit D `note`
  cluster: `lifecycleTimerBlocker`, `evaluateWakeReasons`, `healExpiredTimers`,
  `sessionExitFacts`, `recordWakeFailure`, `healStatePatchWithRollback`, the
  pendingCreate*/config-drift/hash reads). Flip callers to the existing `*Info`
  siblings, delete dead raw siblings. Each a small, oracle-backed commit. Trace
  payloads that need the raw verbatim string (`pending_create_claim` bool gap) keep
  a named raw accessor (spec §4.1).
- [x] **6c — retire the raw working set's READ-SIDE consumers** (DONE, commit
  `3b7795598`). Per §5 this is READ-SIDE ONLY. After Steps 4/5/6b landed the
  decision-read conversions, an exhaustive audit (opus + a 4-lens fable panel, §7)
  found the raw working set (`ordered`/`beadByID`/`circuitSessionByIdentity`/
  `sessionLookup`) had **exactly one** pure read-side consumer left:
  `clearMissingIdleProbes`, which used `beadByID` only as a presence oracle. It now
  reads `infoByID` (presence-identical: same 1:1 `ordered` build, no deletes, refresh
  only overwrites existing keys). Every other consumer is already on `Info`
  (raw-as-domain-only: `computeNamedSessionProgressSignatures`,
  `openPoolSessionCountForTemplate`, `buildAwakeInputFromReconciler`,
  `advanceSessionDrains`), a write/lockstep/invariant site (the forward-pass loop,
  the wakeTargets loop, the CB persist, `sessionLookup`→drain mutations,
  `refreshSessionInfo`), or the whole-bead template subsystem
  (`resolvePreservedConfiguredNamedSessionTemplate`). Those conversions §3's original
  text bundled into 6c are all write/lockstep and **belong to 6d** (they cannot be
  done read-side without dropping a lockstep early). So the raw working-set *deletion*
  (`ordered`/`beadByID`/`circuitSessionByIdentity` removal) is 6d, not 6c.
- [ ] **6d — the cutover: `refreshSessionInfo` off the raw bead + drop the lockstep +
  remove `ordered []beads.Bead`/`beadByID`.** Primarily **write-returns-`Info`** (§2)
  for adjacent-single-write refreshes, PLUS a **targeted store re-read for the
  status-close and aggregating refreshes** (§1/§2 — write results cannot reconstruct
  `Info.Closed`, which derives from `Status`). Build the two intra-tick overlays
  (`reset_committed_at` freeze at snapshot build; `restart_requested` overlay at 2098).
  **Before deleting the blanket pre-pass @2743, regenerate the COMPLETE set of
  forward-pass sites that write an `Info`-read key and lack a per-site refresh** — the
  opus review found the set under-specified: at minimum `SleepPatch`@2631 (max-age kill)
  + `SleepPatch`@2705 (idle kill) write `state=asleep`/`sleep_reason` (read via
  `LifecycleInputFromInfo`→awake scan), and `RestartRequestPatch`@2144 (restart handoff,
  `continue`@2173) writes `continuation_reset_pending`/clears `started_config_hash` — all
  seen TODAY only via the blanket pre-pass. Each must gain a write-returns-`Info` (or
  re-read) refresh. Drop each `session.Metadata[k]=v` lockstep + its dependent same-tick
  reads as ONE commit with a read-after-write test (incl. an idle/max-age-kill test that
  asserts the awake scan sees `state=asleep`). LANDMINE.
- [ ] **6e — join the guard.** Extend `snapshotInfoOnlyFiles`
  (frontdoor_di_guard_test.go:83) to ALSO forbid raw `.Metadata[` on session beads
  (Audit C blocker: today it only forbids the 4 raw snapshot accessors), and add
  the reconciler files once raw-free.

**Realistic scope per session:** 6a is one small, safe, verifiable commit set (additive
mirrors + oracle). 6b is several small oracle-backed commits. 6c and 6d are the large
landmines and likely span multiple sessions each. Checkpoint after each.

**Reverted-attempt note:** the naive Get-cutover (`refreshSessionInfo → sessFront.Get`
+ the two overlays) was implemented and reverted this session (uncommitted experiment —
no git artifact; traced by inspection: the unconditional refresh at
session_reconciler.go:**1854** feeds `infoPostZombie`, and the injection wrapper
`sessionObservationGetErrorStore{remaining:1}` is shared by `sessFront` and the
attachment check). It built and passed the reset-pending/awake/classifier suites but
failed
`TestReconcileSessionBeads_ProgressStallDoesNotRecycleExemptOrSafeSessions/attachment_check_error_fails_safe`
because the refresh-`Get` consumed the injected attachment-check error. That is the
evidence behind moving the cutover to 6d via write-returns-`Info`.

**Line-number caveat:** this doc's anchors were captured against base `48ab9e107`; the
live tree has drifted (e.g. the unconditional refresh is `1854` not `1876`; the blanket
pre-pass is `2743` not `2742`). Re-grep before editing. **Design status: GO-WITH-CHANGES**
per the opus red-team (`step6-design-redteam`) — all findings above folded in; the
store-centric front door, two-overlay intra-tick model, lockstep-last ordering, and
write-returns-`Info`-for-adjacent-writes are validated; the required changes were doc
tightenings (status-close/aggregating read case, persist-error scoping, complete 6d
refresh set), not architecture rejections.

---

## 4. Guard note (6e)

`snapshotInfoOnlyFiles` (frontdoor_di_guard_test.go:83-91) currently lists
`{template_resolve.go, session_name_lookup.go, cmd_citystatus.go,
session_reconciler_trace_cycle.go, providers.go, nudge_dispatcher.go,
named_sessions.go}` and its guard only forbids the four raw snapshot accessors
(`.Open()/.FindByID(/.FindSessionBeadByTemplate(/.FindSessionBeadByNamedIdentity(`).
To make the reconciler files meaningfully raw-free, 6e must extend the guard to also
forbid raw session-bead `.Metadata[` reads/writes (minus the documented raw-by-design
exceptions: the witness full-resync @361-363 and work-bead reads @3391/4213).

---

## 5. Fable red-team (deeper pass) — corrections folded in

A second red-team (fable, `step6-design-fable-redteam`, GO_WITH_CHANGES) went deeper
than the opus pass and found two landmine-class gaps + inventory errors. Dispositions
(the two starred items are the ones that "otherwise detonate the #2345/#2574 class"):

- **★ LIVE min-floor divergence FIXED pre-6a.** `reconcileDrainAckStopPending`
  (session_reconciler.go:1424, `continue`@1425) reaches
  `finalizeDrainAckStoppedSession` (445) which closes a pool session in-memory
  (`Status=closed`) — but Step 4D P2 added the close-refresh to the 1697/1802 finalize
  sites and MISSED this one, so `openPoolSessionCountForTemplate` (`!Info.Closed`,
  session_progress.go:21) over-counted a drain-ack-finalized pool worker as open for the
  rest of the tick. Fixed by `refreshSessionInfo(session.ID)` before the 1425 continue
  (byte-identical-restoring: the raw bead is already closed).
- **★ Exposure set — add the store-only close family.** `closeFailedCreateBead`
  (session_beads.go) persists `state=failed_create` + clears
  `pending_create_claim`/`pending_create_started_at`/`sleep_intent` via
  `setMetaBatch(sessFront, id, …)` (store-only — takes an `id`, cannot mirror the raw
  bead); `rollbackPendingCreate` (session_lifecycle_parallel.go) mirrors only
  `session_name`. When 6d's refresh reads these from the store, `Info.Closed` flips true
  → the session is **evicted from `AwakeInput.SessionBeads`** (compute_awake_bridge.go
  ~129-131), which makes the other keys moot for the awake scan — but 6d must BLESS that
  eviction as its own tested commit (or add a same-tick stale-open overlay), and add
  `MetadataState`/`PendingCreateClaim`/`PendingCreateStartedAt`/`SleepIntent` to the
  exposure table with that disposition. So the exposure set is `{reset_committed_at,
  restart_requested}` for *metadata* divergence **+ the `Status`/`Closed`
  reconstruction case + the store-only-close key family (masked by `Closed`)**.
- **6d refresh set is 9 sites, not "~11", and the pre-pass-dependent writers are ~15,
  not 3.** Verified refresh sites: 1558, 1596, 1703, 1802, 1854, 2013, 2521, 2743, 2767.
  Before deleting the 2743 pre-pass, regenerate the FULL writer set at implementation
  time (checkRateLimitStability ×4, markDrainAckStopPending ×2, healStateWithRollback,
  recoverPendingIdleSleep, reconcileDetachedAt, checkStability, checkChurn,
  silentRebaselineSessionHashes ×2, resetConfiguredNamedSessionForConfigDrift,
  relaunchAgentForLaunchDrift ×2, + the 1424 finalize). Many sit **2-3 helper layers
  deep**, so add a **fourth "nested-helper-write" bucket** to §2's classification and
  pick a mechanism (helpers return their applied batch, or take a refresh callback) —
  adjacent write-returns-`Info` cannot reach them. Classify **1854** explicitly as
  conditional write-returns-`Info` (`markProviderTerminalError` returns its batch; the
  refresh moves inside the write's success path; the unconditional refresh is deleted).
  **Invariant: no 6d mechanism may add an unconditional per-iteration `Get` on the
  forward pass** — a re-read default at 1854 reproduces the reverted failure.
- **restart_requested overlay lifecycle (spec it in §2).** SET at 2098; CLEARED whenever
  a persisted batch containing `restart_requested` lands for that session (the 2144
  consume, the drain-ack clear ~394, the fresh-cycle patch); SURVIVES only the
  kill-failure `continue`@2122. An overlay-always-wins impl re-creates the #2574
  phantom-restart — 6d needs a kill-success-then-refresh test asserting
  `restart_requested` reads empty.
- **6a decision-read gaps DONE (commit `ea5103b96`);** the sleep-policy cluster is
  RE-CLASSIFIED to 6d (opus refinement of the fable finding). The 3 audited gaps —
  `Info.SessionIDFlag`/`TemplateOverrides`/`WakeAttemptsMetadata` — landed with oracle
  cases; their consumers (`freshRestartSessionKey`@2139 + session_bead_cycle.go,
  `clearWakeFailures` dual-form check @857, `applyTemplateOverridesToConfig`@3918,
  `parseSessionTemplateOverridesForLaunch`) join 6b. The fable review's **7-key
  sleep-policy cluster** (`sleep_policy_fingerprint` + the diff-gated sleep-policy keys)
  is NOT a 6a/6b decision-read gap: every reader is in `session_sleep.go`'s write/persist
  path (`persistSleepPolicyMetadata`@255 + the fingerprint compares @223/268/273/327),
  i.e. a **write-path idempotence + lockstep read**. It belongs in **6d**, where
  write-returns-`Info` decides whether it needs an `Info` mirror at all (the diff-gate may
  read the returned/re-read `Info` directly). `state`/`sleep_reason`/`sleep_intent` it
  also reads already exist on `Info`.
- **6c is READ-SIDE ONLY.** Its current text would convert loops that CONTAIN lockstep
  mirrors (2151-2159, 2635-2636, 2709-2710, 2821), de-facto dropping locksteps early and
  silently staling converted reads (invisible to the write oracle). Invariant: `for i :=
  range ordered`, `&ordered[i]`, `beadByID`, refreshSessionInfo's raw read, and every
  lockstep mirror stay UNTOUCHED until 6d. Resolve the
  `computeNamedSessionProgressSignatures` ordering contradiction (scan @1300 vs snapshot
  built @1335 after the CB mutates `ordered[i]`): keep it on per-bead
  `InfoFromPersistedBead` until 6d (exclude from 6c) unless the snapshot is hoisted with
  a post-CB refresh + oracle.
- **Evidence/wording fixes.** All 3 injection tests are exposed
  (session_reconciler_test.go:7661,7833; session_reconciler_progress_test.go:202) and are
  6d gates; downgrade "verified end-to-end" to observed-once/by-inspection (6d must
  re-derive reproducibly); the `raw/step6-*` audit artifacts are IN-MEMORY workflow
  outputs (no committed files — do not cite as paths); `sleep_intent` IS read (@2787), so
  it is reachability-safe not unread; amend SPEC §8 Q1 (still defaults to naive Get) to
  the write-returns-`Info` default.

## 6. 6b execution (in progress) + audit corrections (opus general-purpose audit, HEAD 7b5dbc64d)

An independent full-surface audit of the residual raw decision reads (7 reconciler
files) landed two corrections and a scope refinement that override earlier §5 wording:

- **`resume_flag`/`resume_command`/`resume_style` are NOT codec gaps.** They have been
  on `Info` (`Info.ResumeFlag`/`ResumeStyle`/`ResumeCommand`, info_store.go:50-52) since
  the base codec. So `freshRestartSessionKey` needs **no new mirror** — only a `*Info`
  sibling. The earlier "resume_* codec gap" note (in the 6b prompt) was wrong.
- **`evaluateWakeReasons`/`wakeReasons`/`computeWakeEvaluations` are NOT dead — do NOT
  delete.** `computeWakeEvaluations` is a live nil-guard fallback (`session_wake.go:443`,
  fires when `wakeEvals==nil`) and `wakeReasons` feeds the `gc session list` wake column
  (`cmd_session.go:1305`). They are dead only on the *production reconciler decision
  path*. Their raw reads stay until `evaluateWakeReasons` is removed on its own TODO.
- **The raw classifier siblings stay — they are the oracle's byte-identity ground
  truth.** `lifecycleTimerBlocker`/`isDrainAckStopPending` (raw) are called by
  `TestSessionClassifierInfoEquivalence` as the `bead` side of each pair; deleting them
  breaks the oracle. (The audit's "delete dead raw siblings" suggestion is rejected.)
- **Scope refinement: the decision path is already ~fully on `Info`.** The genuinely
  flippable-in-6b conversions are the ones LANDED this session (below). The remaining
  6b-listed helpers (`freshRestartSessionKeyInfo`, `recentlyDeferredSessionAttachedConfigDriftInfo`,
  `resetPendingCommittedAtInfo`-wiring) are "add sibling+oracle now, flip the call site
  in 6d" scaffolding — their call sites live inside the frozen forward-pass loop or
  write-path helpers (§5). Adding un-wired siblings is optional 6d prep, not required 6b.

**6b conversions LANDED (byte-identical, oracle-backed, gates green):**
- `7b5dbc64d` **6b-A** `lifecycleTimerBlocker`→`lifecycleTimerBlockerInfo` (max-age @2614 +
  idle @2686 reads `infoByID[session.ID].HeldUntil/QuarantinedUntil`; snapshot fresh at
  2553, timer keys never re-written in the forward pass; verified independently).
- `9a7bfe650` **6b-C** `isDrainAckStopPending`→`isDrainAckStopPendingInfo` (drain callers
  @436/@476 project `InfoFromPersistedBead(*session)` locally, §5-blessed drain-path
  pattern; new `drain-ack-stop-pending` fixture + true-branch guard).
- `bd9da510a` **6b-B** template-override consumers → `ParseTemplateOverridesFromInfo`
  (shared decode core + `internal/session` byte-identity test; leaf-helper local
  projection, read-side only).

## 7. 6c execution (DONE, commit `3b7795598`)

**Deliverable: one read-side conversion.** `clearMissingIdleProbes(dt, beadByID)` →
`clearMissingIdleProbes(dt, infoByID)`. The helper used `beadByID[id]==nil` purely as
a working-set presence test; it now checks `_, ok := infoByID[id]`. Presence-identical
by construction: both maps are built 1:1 from `ordered` (`beadByID`@1344,
`infoByID`@1359), neither is ever `delete`d, `refreshSessionInfo` only overwrites
existing `infoByID` keys (guarded by `beadByID[id]!=nil`), and every `beadByID` value
is a non-nil `&ordered[i]` — so `keys(infoByID)==keys(beadByID)` at the call site @3100.
Guarded by `TestClearMissingIdleProbes` (session_wake_test.go). The raw working set is
NOT deleted — `beadByID` is still built and still consumed by `sessionLookup`@3096 and
`refreshSessionInfo`@1377 (both 6d).

**Audit (opus + a 4-lens fable adversarial panel — byte-identity, invariant-compliance,
test-adequacy, scope-completeness — 0 defects, all high confidence).** Full inventory of
the four raw aggregates confirmed `clearMissingIdleProbes` was the SOLE pure read-side
(bucket-B) consumer. Everything else classifies as:
- **A (already-on-Info, raw-as-domain-only):** `computeNamedSessionProgressSignatures`
  @1324 (per-bead `InfoFromPersistedBead`, pre-snapshot), `openPoolSessionCountForTemplate`
  @2090 (reads `infoByID`), `buildAwakeInputFromReconciler` @2779 (reads `sessionInfoByID`),
  `advanceSessionDrains` @3099 (per-bead `InfoFromPersistedBead(*session)` for decisions).
- **C (write/lockstep/invariant — 6d):** the forward-pass loop `for i := range ordered`,
  the CB Phase-0.5 `persistSessionCircuitBreakerMetadata(&ordered[i])` @1326, the blanket
  refresh pre-pass @2774, the wakeTargets loop @2790, `sessionLookup`→`completeDrain`/
  `cancelSessionDrainFor*` mutations, `refreshSessionInfo`'s raw source @1377.
- **D (whole-bead template subsystem):** `resolvePreservedConfiguredNamedSessionTemplate`
  @1539 (feeds `newSessionBeadSnapshot`; second caller session_lifecycle_parallel.go:809).

**6d carry-forward notes surfaced by the audit (do NOT lose these):**
- **`openPoolSessionCountForTemplate`'s `ordered` param is a SAFE domain-switch** to
  `range infoByID` (proven unique IDs: `ListAllSessionBeads` dedupes by ID, and neither
  `retireDuplicateConfiguredNamedSessionBeads` nor `topoOrder` re-introduces dup IDs; the
  count is order-independent) — but it is domain-only raw, so retire it *with* the working
  set in 6d, not as a read-side conversion.
- **`buildAwakeInputFromReconciler`'s `ordered` param must NOT be domain-switched** to
  `range infoByID`: it appends to `input.SessionBeads` in slice order, and that order is
  load-bearing downstream — `ComputeAwakeSet` does `SessionName`-keyed last-write-wins and
  first-match `resolveNamedSessionBeadName`/`findBeadBySessionName` over it, and
  `SessionName` is NON-unique across a retired-duplicate + winner pair. Map iteration would
  reorder and could flip an outcome. Keep the ordered domain; retire in 6d with the loop.
- **`advanceSessionDrainsWithSessionsTraced`'s `ordered`/`sessionBeads` param is DEAD in the
  production call** (`wakeEvals` is always non-nil there, so the `computeWakeEvaluations`
  fallback @session_wake.go:443 never fires), but cannot be dropped in 6c because non-prod
  callers pass `wakeEvals==nil`. Retire with the working set in 6d.
- **Derived `wakeTargets` aggregate still reads raw** (`sleep_intent`@4125,
  `session_name`@2845/@4090/@4179, and `shouldProbeAttachmentForAwakeInput`'s
  state/detached_at/template at compute_awake_bridge.go:200-210). `wakeTarget` must keep a
  raw `*beads.Bead` for the `persistSleepPolicyMetadata` write @2853, so these are 6d, not
  part of the 6c four-aggregate scope.

## 8. 6d execution plan (foundation + Commits 1–4 LANDED; wiring continues)

**Owner decision (this session): mechanism = write-returns-`Info`** (not the
targeted-`Get`-everywhere variant, not a snapshot meta-accumulator). Scope directive:
"drive as far as gates stay green."

**Commit 4 LANDED (`556f02696`).** Folded the `restart_requested` overlay — the SET @~2259
(in-memory-only `ApplyPatch(MetadataPatch{"restart_requested":"true"})`) and the CONSUME @~2331
(`restartFold` = RestartRequestPatch minus `ResetCommittedAtKey`; clears the marker on the
snapshot on consume-success; survives only the failure `continue`s = the #2574 lifecycle).
Byte-identical + coherent. **KEY: these folds are MASKED by the blanket pre-pass @~2913** (it
re-projects every session before the awake scan, the only `Info.RestartRequested` reader), so
there is no behavior change and no isolated teeth test — the pre-pass overwrites them; they are
prerequisite setup for the pre-pass deletion (where a comprehensive read-after-write test becomes
possible). Verified by a **4-lens fable panel (wf_06452ded): 0 confirmed defects** (byte-identity
+ coherence clean; #2574 overlay lifecycle + masking confirmed). **Next: Commit 5 = DELETE the
pre-pass** — the LANDMINE, gated on folding the COMPLETE forward-pass writer set (re-enumerate from
code per §5). Review head-start (all masked, unfolded): pending-create rollback
(`rollbackPendingCreate`), `resetConfiguredNamedSessionForConfigDrift` (@~2538/@~2726, mirrors
`ConfigDriftResetPatch`), SleepPatch max-age/idle kills (~2801/~2875), stability/churn/detach writes.

**Commit 3 LANDED (`a7edb1edc`).** The two nested-helper-write refreshes → `ApplyPatch(batch)`:
HEAL (`healStateWithRollback` already returns its mirrored batch) and ZOMBIE
(`markProviderTerminalError` changed to return `(map[string]string, error)`; reconciler
captures `terminalErrBatch`, nil when the zombie path didn't run; 2 non-reconciler callers
take `_`). Byte-identical (each helper returns exactly its mirror, incl. on persist error;
coherence verified). Two teeth-verified per-site tests (`…ZombieTerminalErrorReflectedOnSnapshot`,
`…HealStateReflectedOnSnapshot`). **LANDMINE surfaced by the fable review (wf_1cfcf522): the
heal fold is NEWLY load-bearing** — the old zombie-site refresh was a full raw re-projection
that masked a stale heal snapshot, but the zombie fold is now `ApplyPatch(nil)` on the
no-terminal-error path, so the heal fold alone carries the healed state to the post-zombie
rollback read on the `case preserveNamed` fall-through. The first draft's "no heal observable"
claim was empirically FALSE (reviewers reproduced a close-vs-open flip in scratch copies); the
missing heal teeth test + 2 inaccurate coherence comments were fixed. 0 byte-identity/coherence
defects in the code. **Next: Commit 4 = the `restart_requested` in-memory write** (~2247):
it's written in-memory only (not a mirrored ApplyPatch batch), so it must also
`ApplyPatch(MetadataPatch{"restart_requested":"true"})` and CLEAR on a persisted
`restart_requested` batch (else #2574 phantom-restart); add a kill-success-then-refresh test.

**Commit 2 LANDED (`e2f1f4adf`).** The three drain-ack `finalize*` closes wired via
`drainAckFinalizeResult{batch, closed, witnessInfo}` + `result.applyTo(infoByID[id])`.
Correction to the naive "return the close batch" plan: `finalizeDrainAckStoppedSession`
has FOUR exit shapes, not one — Path A (ClosePatch mirror → `ApplyPatch(closePatch).MarkClosed()`),
Path B (NDI witness, wholesale `session.Metadata = latest.Metadata` swap → full reprojection,
NOT a patch fold; the one path still reading the raw bead, reworked at the lockstep drop),
Path C (non-close `AcknowledgeDrain`/`CompleteDrain` incl. the `restart_requested` clear →
`ApplyPatch(batch)`, no MarkClosed), and early-return/persist-error/async-stop → zero result.
`reconcileDrainAckStopPending` returns `(bool, result)` (single caller = site 1); the two
statement-call sites (`finalizeDrainAckStopPendingSessions`@574, tests) discard the return.
Byte-identity rests on the ApplyPatch+MarkClosed oracles AND per-site coherence (verified:
top-of-loop / post-heal@1701 / post-zombie@1972, no un-refreshed `*session` mutation reaches
any finalize call on its reaching path). **Three teeth-verified per-site read-after-write
tests** (site 1 `…DrainAck`, site 2 `…DrainAckOrphan`, site 3 `…DrainAckReconciler`) +
`reconcileAtPathWithDrainOps` harness helper. **6-lens fable adversarial panel
(wf_3d1f12c0): 0 byte-identity/coherence defects**; the one confirmed finding (sites 2/3
lacked per-site guards) is closed. Design/analysis in `raw/step6d-commit2-analysis.md`.
**Commit 3 (`a7edb1edc`) LANDED the nested-helper-write refreshes** — see the Commit-3
note at the top of this section. Line numbers below are pre-Commit-2 — re-grep.

**Commit 1 LANDED (`cfd6893fb`).** Added `Info.MarkClosed()` (Closed=true, State="";
oracle `TestInfoMarkClosedMatchesReprojection`) — the status-close counterpart to
`ApplyPatch` — and converted the two **store-only** close refreshes (`closeFailedCreateBead`
@~1590 and `closeBead`/orphan @~1834) from `refreshSessionInfo(id)` to `infoByID[id] =
infoByID[id].MarkClosed()`, keeping the raw `session.Status="closed"` lockstep. Byte-identical
(store-only closes stamp their ClosePatch on the store, not the raw bead, so the raw reproject
already only saw `Status=closed`). Both sites teeth-verified by sibling read-after-write tests
(`…MinFloorCountReflectsMidTickClose` + new `…Orphan`). **Commit 2 (`e2f1f4adf`) LANDED
the drain-ack `finalize*` closes** — see the Commit-2 note at the top of this section.

**Foundation LANDED (`b031a356d`):** `Info.ApplyPatch(patch MetadataPatch) Info`
(internal/session/info_apply_patch.go) — folds a patch onto a projected `Info` by
re-deriving only the patched keys' fields (from the raw patch value, mirroring
`InfoFromPersistedBead` per-key), carrying the rest forward, never flipping `Closed`
(status close is a separate refresh case). Byte-identical to a full re-projection, proven
by `TestInfoApplyPatchMatchesReprojection` (set+clear for every projected key + coupling
edges). Unwired (no reconciler behavior change), landed exactly as Step 5 landed
`Store.CircuitState`.

**KEY SIMPLIFICATION discovered (vs the reverted Get-cutover): under write-returns-`Info`
the snapshot only ever receives MIRRORED batches (via `ApplyPatch`).** So the
persisted-WITHOUT-mirror keys never enter the snapshot on their own — the
**`reset_committed_at` freeze overlay is UNNEEDED** here (it was only needed under the
Get-cutover, where `Get` reads the store copy that carries it; §1/§2). `started_live_hash`
likewise. The ONLY intra-tick carrier still needed is **`restart_requested`**: it is
written in-memory-only as a direct `session.Metadata["restart_requested"]="true"` (@~2130),
NOT via a mirrored `ApplyPatch` batch, so the snapshot won't see it unless that write ALSO
does `infoByID[id] = infoByID[id].ApplyPatch(MetadataPatch{"restart_requested":"true"})`
(and it must CLEAR when a persisted `restart_requested` batch later lands — the 2144
consume / drain-ack clear / fresh-cycle — else #2574 phantom-restart).

**Refresh-site classification (VERIFIED this session, live HEAD `b031a356d` line numbers —
re-grep before editing).** Every site is a nested-helper-write: the batch/close is NOT
visible at the `refreshSessionInfo` call, so the helper must return what it wrote. There is
NO by-construction status-close shortcut (the close helpers stamp a `ClosePatch` too).
- **markProviderTerminalError refresh** — @1886 `infoPostZombie`. `markProviderTerminalError`
  (session_reconcile.go:754) already builds `batch` locally → change it to return
  `(sessionpkg.MetadataPatch, error)`; 3 callers (session_reconcile.go:687,
  session_reconciler.go:1853, session_lifecycle_parallel.go:1999) — only the 1853 caller
  needs the batch, the other two take `_`. The unconditional @1886 refresh becomes a
  conditional `ApplyPatch(terminalErrBatch)` (nil→no-op when it didn't run). This is §5's
  model conversion AND it dodges the injected-error hazard (ApplyPatch never touches the
  store, unlike the reverted `Get`).
- **heal refresh** — @1628 `infoPostHeal` ← `healState`/`healStateWithRollback` (state,
  pending_create_claim, …). Helper must return its applied batch.
- **status-close family (ClosePatch batch + `Status=closed` — refresh =
  `ApplyPatch(closeBatch)` then a new tiny `Info` close helper `Closed=true; State=""`):**
  @1456 (`reconcileDrainAckStopPending`→`finalizeDrainAckStoppedSession`, ClosePatch
  "drained" @372), @1735 / @2045 (`finalizeDrainAckStoppedSession`), @1590
  (`closeSessionBeadIfReachableStoreUnassigned`→`closeFailedCreateBead`/`closeBead` +
  `session.Status="closed"` @1589), @1834 (same, reason path, @1833).
  - **SUBTLE — the store-only closes diverge from the raw-mirror closes.**
    `closeFailedCreateBead` / `rollbackPendingCreate` persist via `setMetaBatch(sessFront,
    id, …)` (store-only, take an `id`, do NOT touch the raw bead). So TODAY the raw
    reproject reflects ONLY the `Status=closed` set at the call site, NOT their
    state=failed_create / cleared keys. Byte-identical conversion for those sites is
    **MarkClosed ONLY** (do NOT ApplyPatch their store-only batch). The `finalize*` /
    `completeDrain` closes DO mirror a ClosePatch onto the raw bead, so those need
    `ApplyPatch(closeBatch)` + MarkClosed. Match each site to its close family exactly.
- **aggregating** — @2553 `infoAsleepDrift` and the wakeTargets refresh @2799 reflect
  cumulative prior-block mutations; they resolve once the writers before them self-refresh.
- **blanket pre-pass** — @2775 `for i := range ordered { refreshSessionInfo }` is the
  linchpin: it exists ONLY to catch un-self-refreshed forward-pass mutations (the
  restart_requested marker + the pending-create rollback that `continue`s). Once every
  forward-pass writer self-refreshes (via ApplyPatch/MarkClosed) AND restart_requested@2130
  ApplyPatches, it is redundant → delete it (with a read-after-write test that the awake
  scan sees the self-refreshed values).

**Deletion order (final commits):**
1. Thread batches out of every nested helper; convert each refresh site to
   `ApplyPatch`/MarkClosed (KEEP the raw mirror throughout — byte-identical, whole-tick
   suite + the ApplyPatch oracle as evidence, PLUS a bespoke same-tick read-after-write
   test per site since the write oracle is blind to stale reads).
2. ApplyPatch the `restart_requested`@2130 in-memory write onto the snapshot (+ clear-on-persisted).
3. Delete the blanket pre-pass @2775.
4. Convert the remaining raw consumers: `advanceSessionDrains` mutations (`completeDrain`
   etc. off the raw bead) so `sessionLookup` retires, and feed `newSessionBeadSnapshot`
   (via `resolvePreservedConfiguredNamedSessionTemplate`, bucket D) from a store source
   rather than `ordered` — the HARDEST, may need a store `List`.
5. Now nothing reads the raw bead → drop every `session.Metadata[k]=v` lockstep, delete
   `refreshSessionInfo` (raw source), `beadByID`, `circuitSessionByIdentity`, and `ordered
   []beads.Bead`. Replace `ordered` as the iteration domain with an ORDER-PRESERVING
   `[]Info`/`[]string` (NOT map iteration): `buildAwakeInputFromReconciler` appends to
   `input.SessionBeads` in slice order and `ComputeAwakeSet` does `SessionName`-keyed
   last-write-wins over it (6c-audit landmine). `openPoolSessionCountForTemplate` may
   domain-switch to `infoByID` (unique IDs proven).
6. **6e** — extend `snapshotInfoOnlyFiles` (frontdoor_di_guard_test.go) to forbid raw
   session `.Metadata[`; add the reconciler files.

**Read-after-write test harness LANDED (`4f0a6ea8b`).** The per-site read-after-write test
harness the wiring needs is built: `cmd/gc/session_reconciler_read_after_write_test.go`. It
runs the REAL tick over a single-template multi-session working set and exploits topoOrder's
determinism (session_reconcile.go:1289 — empty `deps` returns `sessions` unchanged, so
processing order == input slice order) to place a write before a dependent read in one tick,
then asserts an OBSERVABLE outcome that flips iff the write reached the read through the
`infoByID` snapshot. First test `TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose`
guards the cross-session min-floor read (openPoolSessionCountForTemplate @~2090): a
failed-create worker closed earlier in the tick (slice index 0) must drop the open count so a
stalled worker (index 1) is min-floor exempt. This is the mid-tick-close integration test 4D
deferred as "impractical — topoOrder hides processing order"; single-template ordering makes
it deterministic. **Teeth verified** — removing the failed-create close-refresh @~1590 flips
the stalled worker to wrongly-recycled and fails the test; stable under `-count=50`. Each 6d
lockstep drop lands its sibling read-after-write test following this pattern (a mutation
earlier in the slice, a dependent decision later, outcome-observable).

**Why the wiring did not proceed piecemeal this session:** every conversion above is a
nested-helper-batch-threading whose byte-identity is INVISIBLE to the byte-identical write
oracle (blind to same-tick stale reads — SPEC §2 governing principle). The harness above is
what closes that gap; with it in place the wiring is now a tractable, guarded sequence (each
site: thread the batch out, convert the refresh, add its read-after-write test), but it must
still be landed as a cohesive unit ending in the lockstep+working-set removal (nothing is
deleted until then). The foundation primitive and the test harness it rests on are landed and
adversarially verified.
