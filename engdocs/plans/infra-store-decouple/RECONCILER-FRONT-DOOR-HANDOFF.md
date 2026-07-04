# Reconciler Front-Door Handoff — the backlog to work through

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `.claude/worktrees/object-front-doors`, **HEAD `366e203f3`+** (6d PRE-PASS DELETION
COMPLETE — all 22 folds + blanket pre-pass + both aggregating refreshes + `refreshSessionInfo` gone).

This is the authoritative handoff for finishing the session reconciler's move off
raw `beads.Bead.Metadata`, onto the typed **`session.Store`** front door. It
**supersedes** `SPINE-FLIP-HANDOFF.md` / `SPINE-FLIP-NEXT-SESSION-PROMPT.md` (the
`InfoFromPersistedBead(*session)` re-derive approach — retired; see below).

**Status:** Steps 0–5 DONE. **Step 6 DESIGNED (dual-reviewed) + 6a/6b/6c DONE + 6d
FOUNDATION DONE (`b031a356d`) + 6d READ-AFTER-WRITE TEST HARNESS DONE (`4f0a6ea8b`) +
6d COMMIT 1 DONE (`cfd6893fb`).** Owner locked the 6d mechanism = **write-returns-`Info`**
(not targeted-`Get`, not a meta-accumulator). The `Info.ApplyPatch(patch)` primitive,
the `Info.MarkClosed()` status-close primitive, their equivalence oracles, and the
multi-session read-after-write test harness
(`cmd/gc/session_reconciler_read_after_write_test.go`, single-template deterministic
ordering, teeth-verified) are all landed — the foundation the rest of the wiring rests on.

**6d Commit 1 DONE (`cfd6893fb`).** Added `Info.MarkClosed()` and converted the two
**store-only** close refreshes (failed-create, orphan) from `refreshSessionInfo(id)` to
`infoByID[id] = infoByID[id].MarkClosed()`, KEEPING the raw lockstep. Teeth-verified
sibling tests (`…MidTickClose` + `…Orphan`).

**6d Commit 2 DONE (`e2f1f4adf`).** Converted the three **drain-ack `finalize*` closes**
(`refreshSessionInfo` @1456/@1735/@2045 pre-Commit-1) to write-returns-`Info`.
`finalizeDrainAckStoppedSession` now returns `drainAckFinalizeResult{batch, closed,
witnessInfo}`; callers fold via `result.applyTo(infoByID[id])` (ApplyPatch(batch) +
MarkClosed on a close, witness reprojection outright, no-op for the zero value). Four
exit shapes, all byte-identical: Path A (ClosePatch+MarkClosed), Path B (NDI witness
reproject — the one path still reading the raw bead, reworked at the lockstep drop),
Path C (non-close drain-ack batch incl. #2574 `restart_requested` clear), and
early/error/async → zero result. `reconcileDrainAckStopPending` (site 1's wrapper) now
returns `(bool, result)`; the two statement-call sites discard the new return unchanged.
Coherence verified at all three sites (top-of-loop / post-heal / post-zombie refresh, no
un-refreshed `*session` mutation reaches any finalize call). **Three teeth-verified
per-site read-after-write tests** (`…DrainAck` site 1, `…DrainAckOrphan` site 2,
`…DrainAckReconciler` site 3) — disabling ONLY a given site's fold fails ONLY its test
(new `reconcileAtPathWithDrainOps` harness helper injects the controller drain-ack sites
2/3 gate on). **Reviewed by a 6-lens fable adversarial panel (wf_3d1f12c0): 0
byte-identity/coherence defects; the sole confirmed finding — sites 2/3 lacked per-site
guards — is closed by the two added tests.** Gates green (build/vet/lint 0/gofmt/oracles/
all five harness tests -count=5/whole reconciler+drain+pool+named+wake+circuit suites).

**6d Commit 3 DONE (`a7edb1edc`).** Converted the two **nested-helper-write refreshes**
to `ApplyPatch(batch)`: the HEAL refresh (`healStateWithRollback` already returns its
mirrored batch → `infoByID[id] = infoByID[id].ApplyPatch(healBatch)`) and the ZOMBIE
refresh (`markProviderTerminalError` changed to return `(map[string]string, error)`; the
reconciler captures `terminalErrBatch`, nil when the zombie path didn't run; 2 other
callers take `_`). Both byte-identical (each helper returns exactly its mirror; coherence
verified — heal: top-of-loop unmutated because pre-heal mutating sites `continue`; zombie:
desired fast path + the sole non-continue `case preserveNamed` arm, heal-folded above).
**Two teeth-verified per-site tests** (`…ZombieTerminalErrorReflectedOnSnapshot` via
pending-create rollback suppression; `…HealStateReflectedOnSnapshot` via a live
start-pending orphan drain). **KEY: the heal fold is NEWLY load-bearing** — the old
zombie-site full re-projection used to mask a stale heal snapshot, but the zombie fold is
now `ApplyPatch(nil)` on the no-terminal-error path. A first draft claimed the heal fold
had no observable + shipped no test; the **6-lens fable panel (wf_1cfcf522) empirically
refuted that** (0 byte-identity/coherence defects, but a CONFIRMED missing heal teeth test
+ 2 inaccurate coherence comments — all fixed).

**6d Commit 4 DONE (`556f02696`).** Folded the `restart_requested` overlay: the in-memory-only
SET @~2259 (`ApplyPatch(MetadataPatch{"restart_requested":"true"})`) and the CONSUME @~2331
(`restartFold` = RestartRequestPatch minus `ResetCommittedAtKey`, folded so a consumed restart
clears the marker on the snapshot; the overlay survives only the failure `continue`s = the #2574
lifecycle). Byte-identical + coherent. **These folds are CURRENTLY MASKED by the blanket pre-pass
@~2913** (it re-projects every session before the awake scan, the only `Info.RestartRequested`
reader), so there is NO behavior change and NO isolated teeth test — the whole-tick suite confirms
no regression but cannot verify the folds; they are prerequisite setup for the pre-pass deletion.
Verified by a **4-lens fable panel (wf_06452ded): 0 confirmed defects** (byte-identity + coherence
clean; #2574 overlay lifecycle + masking confirmed correct). Also updated the pre-pass rationale
comment to record which writers self-refresh vs. which the pre-pass still masks.

**6d Commit 5 = DELETE the blanket pre-pass — DONE (the LANDMINE, landed as 7 commits
`901b3c6b8`..`366e203f3`, + a residue-doc follow-up).** The COMPLETE fold set was audited from
code (wf_df3cae94: 12 mechanisms / 22 sites, enumerated in `RECONCILER-FRONT-DOOR-STEP6-PREPASS-AUDIT.md`).
Every one now folds its mutation onto the typed snapshot via write-returns-Info; the blanket
pre-pass, both per-session aggregating refreshes (infoAsleepDrift, wakeTargets), AND the
`refreshSessionInfo` closure are ALL DELETED. Fold batches: 1 = heal#2 + SleepPatch kills;
2 = drain-ack-stop-pending + idle-recover (deterministic reconstruction, no sig change);
3 = detach/clears/rebaseline/relaunch/recover; 4 = rate-limit/stability/churn (sub-helper batch
threading + `mergeMetadataPatch`); 5 = config-drift #2574 + rollback (**NO MarkClosed** — the
close is store-only, so the raw bead stays open and the pre-pass saw `Closed=false`; the audit
table's "+ MarkClosed" was a Get-cutover note, corrected). Batches 3-4 delegated to sonnet
subagents against the audit spec, each fable-panel-reviewed; batches 1/2/5 + the deletion done by
hand. **Verified: the comprehensive reconciler suite (211-212s green with every refresh gone — a
stale snapshot from any missed fold flips an awake/sleep/recycle/drain decision and fails) + a
4-lens capstone fable review (wf_e8507262: 0 confirmed defects).** One KNOWN-INERT residue
documented at the fold site + in the audit: `buildPreparedStart`'s extra keys on the
`recoverRunningPendingCreate` failure path (decision-inert — the block is `pending_create_claim`-gated,
which drives the wake; deferred to the Get-cutover).

**NEXT = the LOCKSTEP DROP.** Every snapshot update is now write-returns-Info (no raw-bead
re-derive anywhere on the decision or refresh path), so the raw working set can go: drop every
`session.Metadata[k]=v` lockstep mirror, remove `beadByID` / `circuitSessionByIdentity` / the raw
`ordered []beads.Bead` (replace the iteration domain with an ORDER-PRESERVING `[]Info`/`[]string`
— NOT map iteration: `buildAwakeInputFromReconciler` appends to `input.SessionBeads` in slice order
and `ComputeAwakeSet` does `SessionName`-keyed last-write-wins over it), convert
`advanceSessionDrains` / `newSessionBeadSnapshot` off the raw bead, and cut the snapshot source to
store-authoritative `Get` (handling the reset_committed_at / started_live_hash / buildPreparedStart
intra-tick divergences the raw refresh preserved — the exposure set). Then **6e** — the guard
forbidding raw `session.Metadata[` writes on the decision path. **The focused per-step plan +
paste-ready prompt for this phase are `RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md` +
`RECONCILER-FRONT-DOOR-NEXT-SESSION-PROMPT.md`.** See also STEP6-DESIGN §8 deletion-order
steps 4-6; STEP6-PREPASS-AUDIT.md records the exposure set.
Design + sub-phase backlog:
`RECONCILER-FRONT-DOOR-STEP6-DESIGN.md` (fable 4-lens
audit + opus synthesis, opus red-team GO-WITH-CHANGES, then a deeper **fable red-team**
GO_WITH_CHANGES folded into §5; §6 has the 6b audit corrections + landed commits). Step 6
commits so far: `212581818` (fix the 1424 drain-ack min-floor snapshot divergence),
`20ee1a125` (fold fable §5), `ea5103b96` (**6a** codec-gap mirrors), then the **6b**
conversions: `7b5dbc64d` (`lifecycleTimerBlocker`→Info), `9a7bfe650`
(`isDrainAckStopPending`→Info), `bd9da510a` (template-override consumers→Info),
`5968a1a32` (oracle fixture-vitality guard).

**6b review verdict (fable review/red-team workflow, 8 agents, 0 confirmed defects).**
All 3 conversions validated byte-identical + §5-compliant (read-side only; raw classifier
siblings kept as oracle ground truth). 4 findings, ALL refuted: an observed oracle flake
(`isDrainAckStopPending: info=false bead=true`) was traced to **stale shared-GOCACHE
objects** — independently re-verified 5/5 green under a fresh isolated GOCACHE — plus two
LOW polish nits (commit-message wording; a missing fixture-vitality guard, since added).
**6b scope finding (opus audit, §6):** the reconciler decision path is *already* ~fully on
`Info`; the 3 landed conversions were the ones whose call sites are genuinely flippable in
6b. The remaining 6b-listed helpers (`freshRestartSessionKeyInfo`,
`recentlyDeferredSessionAttachedConfigDriftInfo`, `resetPendingCommittedAtInfo`-wiring) are
"add sibling+oracle now, flip the call site in 6d" scaffolding whose call sites live inside
the frozen forward-pass loop / write-path helpers — optional 6d prep, not required 6b.
Two audit corrections locked: **`resume_*` are NOT codec gaps** (already on `Info` since the
base codec) and **`evaluateWakeReasons`/`wakeReasons`/`computeWakeEvaluations` are NOT dead**
(live nil-guard fallback `session_wake.go:443` + `gc session list` column
`cmd_session.go:1305`) — do NOT delete.

**Step 6 sequencing correction (evidence-based):** the naive keystone "flip
`refreshSessionInfo` to `sessFront.Get`" was implemented and **REVERTED** — the
unconditional per-iteration refresh at `session_reconciler.go:1854` consumes the
`store.Get(sessionID)` errors reconciler fail-safe tests inject (failed
`ProgressStallDoesNotRecycle/attachment_check_error_fails_safe`) for zero benefit while
the lockstep still maintains the raw bead. Re-sequenced: **6a** codec-gap mirrors (DONE)
→ **6b** convert residual raw decision reads to existing `*Info` siblings (**substantively
DONE** — the flippable-in-6b call sites landed; see §6) → **6c** retire
the raw `ordered[]`/`beadByID` working set (READ-SIDE ONLY — never convert a
lockstep-mirroring loop early) → **6d** the cutover via **write-returns-`Info`** +
targeted re-reads for status-close/aggregating refreshes, bundled with the lockstep drop
→ **6e** join the guard. Exposure set = `{reset_committed_at (frozen), restart_requested
(overlay)}` + the `Status`/`Closed` reconstruction case + the store-only-close key family
(masked by `Closed`). See STEP6-DESIGN §5 for the fable-review corrections (9 refresh
sites, ~15 nested-helper writers, restart_requested overlay lifecycle).
`RECONCILER-FRONT-DOOR-NEXT-SESSION-PROMPT.md` is paste-ready for Step 6b.

**Read first:** `RECONCILER-FRONT-DOOR-SPEC.md` (the design, review-hardened v2) and
`OBJECT-MODEL-FRONT-DOOR-DESIGN.md` (the parent design; §3.1 session, §7 Phases 4–5).

---

## What changed in direction (why the spine-flip approach was retired)

The spine-flip routed reconciler decision reads through
`info := InfoFromPersistedBead(*session)` re-derived snapshots + `*Info` classifier
siblings, re-deriving after each mutation. Owner review (Julian) found this
**re-projects the raw working copy instead of going through a front door** — it
doesn't hide the map, and the "re-derive after the right mutation" invariant is
fragile. The map must be hidden behind a typed store.

**The front door already exists:** `session.Store` (renamed this session from
`InfoStore`) — a domain wrapper in `internal/session` holding `beads.SessionStore`
by value, owning the codec: `Get → Info`, `List`, `ApplyPatch` + ~20 typed write
methods, over the 25 `MetadataPatch` builders. The reconciler already routes its
**writes** through it (`sessFront *session.Store`). The remaining work is Phase 5
(reads) + retiring the raw lockstep — done **through the front door**, not the
re-derive shortcut.

**Not wasted:** the `*Info` classifiers + `TestSessionClassifierInfoEquivalence`
become the accessor logic behind `Info`; clusters 1–4e are **not reverted** — their
call sites just switch the `Info` source from re-derive to the snapshot/`Get`.

**Design decisions locked (this session):** store-centric front door (`store.Method
(id, …)`, no entity handle); every mutation persists + reads re-`Get`; **proceed
with refresh-on-write, fix only if a benchmark later shows it hot** (owner: "we'll
fix if hot"); `BeadSession`/backend split deferred (stored form is backend-identical).

---

## GOVERNING SAFETY PRINCIPLE (do not violate)

> **Never drop a `session.Metadata[k]=v` lockstep until its dependent same-tick reads
> are already on the coherent snapshot.** Convert each write + every non-`continue`
> read of the same bead later in that iteration as **one unit, one commit.**

The byte-identical **write** oracle (recording fake store) is **blind to same-tick
stale reads**. Every lockstep drop needs a **multi-session / read-after-write
same-tick test**. Non-`continue` read-after-write sites: `infoPostHeal` (~1545),
`infoPostZombie` (~1793), `infoAsleepDrift` (~2457), `restart_requested` read (~2057),
`churn_count` (~2133-2172). See spec §2.

---

## THE BACKLOG (ordered; one verified commit per item)

- [x] **Step 0 — rename `InfoStore` → `session.Store`** (`990076d86`, this session).
- [x] **Step 1 — missing `Info` mirrors.** DONE. Regenerated the exhaustive key
      inventory (`raw/step1-key-inventory.md`) — the handoff's "6 known-missing" was
      wrong on three counts: `session_name_explicit` is a **PHANTOM** (nonexistent in
      the repo — dropped); `restart_requested` is the §5.2 **intra-tick special**
      (deferred to Step 3, not a codec mirror); and the real set is **17**, not 6.
      Landed 17 raw-string `Info` mirrors (12 core lifecycle keys +
      `config_drift_deferred_{at,key}` / `attached_config_drift_deferred_{at,key}` /
      `stranded_event_emitted_at`), each with a `TestSessionClassifierInfoEquivalence`
      `stringChecks` case (symbolic-key cases feed the cmd/gc constant → guards the
      `info_store.go` literal against drift) + hold/quarantine, wait-hold,
      churn-spiral (padded), wake-mode/intents, and config-drift-full fixtures.
      Excluded: `detachedProbeMetadataKey` (reads an **assigned-work** bead, not a
      session bead). No call-site change (4c-foundation shape).
      **`PoolSlot`/`CommonName`/`ConfiguredNamedIdentity` already existed.**
- [x] **Step 2 — coherent snapshot, alongside the existing lockstep** (additive,
      behavior-identical). DONE. Built the tick working set once as
      `infoByID map[string]session.Info` from `ordered` (post-Phase-0.5, in
      `session_reconciler.go` right after `beadByID`) and re-sourced the top-of-loop
      pre-mutation `info` (was `InfoFromPersistedBead(*session)`) onto it. Verified
      byte-identical: Phase 1 mutates only the current iteration's session (no
      cross-session writes — grep-confirmed), and the snapshot is built after
      Phase-0.5, so no entry goes stale before it is visited. Lockstep + raw
      `ordered`/`beadByID` untouched. **Two justified refinements of the literal
      plan (flagged):** (1) **`ListInfo(ListFilter)` deferred** — `session.Store.List`
      has ZERO production callers; the reconciler's working set is the in-memory
      `ordered` (topo-ordered / healed / retired / CB-restored), NOT a fresh
      `store.List`, so promoting `List` now would add an unconsumed method (YAGNI).
      (2) **Refresh-on-write moved to Step 3** — it has no consumer until a
      post-mutation read migrates onto the snapshot; wiring it per-unit in Step 3
      (each `write + refresh + dependent-read` as ONE commit) honors §2's governing
      principle *better* than blanket-wiring unconsumed refreshes now. Gates:
      build ./... · vet · golangci-lint=0 · gofmt · `TestReconcileSessionBeads*` +
      reconciler/phase0/chaos/named (427 PASS) + trace green.
- [x] **Step 3 — per-session reads onto the snapshot + refresh-on-write.** DONE (4
      commits). Introduced `refreshSessionInfo(id)` and folded all four post-mutation
      re-derives onto the snapshot: `infoPostHeal` (`2f5fef84f`, cluster 1/4 — also
      added `TestGetReflectsApplyPatch`), then `infoPostZombie` + `infoAsleepDrift`
      (`94f39e538`, clusters 2-3), then the wake-pass `info` + `Info.SleepIntent`
      (`3d1725abf`, cluster 4/4). No `InfoFromPersistedBead(*session|*target.session)`
      re-derive remains in the reconciler. **KEY DECISION (owner, via the
      reset_committed_at audit):** `refreshSessionInfo` refreshes from the **raw
      working copy** (`InfoFromPersistedBead(*beadByID[id])`), NOT `sessFront.Get`,
      during the coexistence phase — byte-identical BY CONSTRUCTION and it preserves
      the reconciler's deliberate intra-tick raw/store divergences (the restart
      handoff persists `reset_committed_at` but the lockstep skips it, #2145/#2345
      force-wake prevention; the RunLive re-apply persists `started_live_hash` without
      a lockstep). A `Get` refresh would pull those hidden keys into the snapshot and
      break Step 4's wake scan. `restart_requested` stays intra-tick (§5.2, unbuilt).
- [x] **Step 4 — `LifecycleInput` from `Info` + the four cross-session scans.** DONE
      (4A–4D, `af9471021`..`4617c0821`). All four reconciler session scans now read
      typed `Info`; no raw session-bead metadata cracking remains in them.
      Approach chosen by owner: **Full typed `LifecycleInput`** (replace its
      `Metadata map[string]string` with typed fields; rewrite `ProjectLifecycle` +
      callers). Analysis (this session) narrowed the surface: **`ProjectLifecycle`
      itself reads only 13 keys** — `state`, `sleep_reason`, `continuity_eligible`,
      `configured_named_identity`, `held_until`, `quarantined_until`,
      `pending_create_claim`, `last_woke_at`, `session_key`, `started_config_hash`,
      `pending_create_started_at`, `pin_awake`, `wake_request` — **all now mirrored**
      (`wake_request` added phase A, `af9471021`). The `session_circuit_state` /
      `restart_requested` / `wait_hold` / `alias` / `session_name` reads live in the
      **post-view display helpers** (`lifecycleDisplayReasonFromView`,
      `lifecycleResetPendingReasonVisible`, `LifecycleIdentifiersReleased`) — those
      are display/API paths, NOT the reconciler scan, so they KEEP their `map`
      params (out of scope). Phases:
      - [x] **4A** — add `Info.WakeRequest` mirror (`af9471021`).
      - [x] **4B** — typed `LifecycleInput` core (`17f138775`). Dropped
        `.Metadata`, added the 13 typed fields (12 raw-string + `PendingCreateClaim
        bool`), converted `ProjectLifecycle` + helpers (`projectBlockers`,
        `projectWakeCauses`, `projectRuntimeProjection`, `creatingStateIsStale`,
        `shouldResetContinuation`) to read them — byte-identical (source moved from
        `meta[k]` to a field, every `TrimSpace`/`== "true"`/`time.Parse` kept in
        place). Added `LifecycleInputFromMetadata(status, meta)` +
        `LifecycleInputFromInfo(info)` constructors (both in internal/session; key
        literals below the codec edge; `FromInfo` reconstructs `Status` from
        `Info.Closed`, reads `Info.MetadataState` for the raw `state`). Routed the
        internal wrappers + `manager.go`/`waits.go` through `FromMetadata`.
        **Byte-identical oracle landed** (`lifecycle_input_test.go`,
        `TestLifecycleInputConstructorsProjectIdentically`): 15 shapes,
        `ProjectLifecycle(FromMetadata(b))` ≡ `ProjectLifecycle(FromInfo(
        InfoFromPersistedBead(b)))`. **NOTE — the cmd/gc construction sites
        (`compute_awake_bridge`, `cmd_session`, `session_sleep`, `session_reconcile`,
        chaos test) were routed through `FromMetadata` in THIS commit as the
        mechanical, behavior-identical compile-fix that dropping the struct field
        forces for `go build ./...`.** So 4C is now purely the SEMANTIC conversion
        (below), not the mechanical routing.
      - [x] **4C** — `compute_awake_bridge.go` `buildAwakeInputFromReconciler` off
        `Info` (`6843e8607`). Added `Info.RestartRequested` (struct + codec + a
        `TestSessionClassifierInfoEquivalence` `sessionRestartRequested` case, with
        `restart_requested` on the wake-mode-and-intents fixture). The session-beads
        loop now derives `info := InfoFromPersistedBead(*b)` and reads every fact from
        it — `Closed`, `SessionNameMetadata`, `Template`, `SleepReason`, `WaitHold`,
        `RestartRequested`, `ContinuationResetPending`+`ResetCommittedAt`,
        `CurrentlyProcessingBeadID`, `DetachedAt`, `CreatedAt`, and the manual/named
        classifiers (`isManualSessionInfo`/`isNamedSessionInfo`, bead siblings
        oracle-proven equivalent); the lifecycle view is fed by
        `LifecycleInputFromInfo(info)`. **One deliberate normalization:**
        `DependencyOnly` moved from the ad-hoc untrimmed `== "true"` to the trimmed
        `info.DependencyOnly` (the codec-canonical projection; invisible — no padded
        fixture). 4C re-derives `Info` LOCALLY (self-contained diff); **4D** swaps the
        source to the passed-in `infoByID` snapshot. The
        `shouldProbeAttachmentForAwakeInput`/`wakeTargets` reads (`target.session`, a
        different data source) stay raw — out of scope.
      - [x] **4D** — snapshot plumbing + the simpler scans (3 commits,
        `84c5987ba`/`f11867ef0`/`4617c0821`). **Phase 1** (`84c5987ba`):
        `buildAwakeInputFromReconciler` takes a `sessionInfoByID` param and reads
        `infoByID[b.ID]` (fallback to a per-bead projection when nil, for unit
        tests) instead of re-deriving; the reconciler re-syncs the snapshot to the
        beads with a blanket `refreshSessionInfo(ordered[i].ID)` pass right before
        the scan (the forward pass's late un-lockstepped mutations — the §5.2
        `restart_requested` marker and pending-create rollback that `continue`s
        without a refresh — must land first), which runs after the whole forward
        pass so it perturbs no earlier same-iteration read and is byte-identical to
        the 4C re-derive by construction. New test
        `TestBuildAwakeInputFromReconcilerReadsInfoSnapshot`. **Phase 2**
        (`f11867ef0`): min-floor scan → `openPoolSessionCountForTemplate(ordered,
        infoByID, cfg, template)` reading `!Info.Closed` +
        `normalizedSessionTemplateInfo`; the four forward-pass in-memory close sites
        (failed-create @1548, orphan @1786, two `finalizeDrainAckStoppedSession`
        @1687/@1989) now `refreshSessionInfo` after the close so the cross-session
        count excludes a session closed this tick. Guarded by
        `TestOpenPoolSessionCountForTemplateExcludesClosed` (a full mid-tick-close
        integration test is impractical — `topoOrder` hides processing order — so
        it's guarded by construction + the consumer-side unit test). **Phase 3**
        (`4617c0821`): `computeNamedSessionProgressSignatures` reads
        `info.ConfiguredNamedIdentity`/`SessionNameMetadata`/`Alias`/`ID` via a
        per-bead projection (Phase 0.5 — no snapshot yet — the same shape
        `advanceSessionDrains` already uses). **`advanceSessionDrains` needed no
        change**: it was already converted to `InfoFromPersistedBead(*session)`
        Info reads in a prior sub-cluster. Switching the two per-bead projections to
        the snapshot follows in steps 5/6.
- [x] **Step 5 — circuit-breaker typed accessor.** DONE (2 commits). **Phase 1**
      (`aba514233`): `internal/session/circuit_state.go` — `type CircuitState` (9
      raw-string fields), the 7 previously-bare `session_circuit_*` key literals
      promoted to exported session-package constants (the state + reset-generation
      keys already lived in `lifecycle_projection.go`), `CircuitStateFromMetadata`
      (pure codec) and `Store.CircuitState(id)` (store-authoritative Get accessor,
      the Step-6 destination — NOT wired into the reconciler hot path). Codec-edge
      byte-identical oracle (`circuit_state_test.go`,
      `TestCircuitStateFromMetadataProjectsVerbatim` across
      open/closed/reset-gen/missing-vs-empty/malformed + store accessor Get-only /
      error-surfacing). CircuitState is a **distinct concern from `Info`** (breaker
      timers/counters are lifecycle-**safety** state, not decision facts — spec
      §5.3). **Phase 2** (`59af3b856`): `restoreFromMetadata` /
      `observeResetGenerationFromMetadata` / `hasSessionCircuitMetadata` now take
      `session.CircuitState` instead of `map[string]string` — every parse/trim/
      compare kept in place reading `cs.Field` (byte-identical); the two
      `len(meta)==0` fast-paths dropped (an all-empty `CircuitState` already falls
      through `hasSessionCircuitMetadata` / yields an ignored empty reset
      generation). The cmd/gc key constants now **alias** the session constants (one
      source of truth). The two reconciler Phase-0.5 reads build
      `sessionpkg.CircuitStateFromMetadata(ordered[i].Metadata)` — a pure per-bead
      projection of the in-memory working copy (the same shape
      `computeNamedSessionProgressSignatures` uses; **no store Get on the hot
      path**), byte-identical by construction. Breaker restore/round-trip/
      stale-snapshot/auto-reset tests + the classifier-info oracle + front-door
      guards routed through the typed path; whole-tick reconcile/circuit/named/pool/
      wake/sleep/drain/trace suite green (86s). **Fable 5-lens red-team: 0 confirmed
      defects.** This was the LAST raw session-metadata read cluster on the
      reconciler decision path.
- [~] **Step 6 — drop the lockstep + remove the raw working set + cut refresh over
      to `Get`.** Sub-phases 6a/6b/6c DONE; **6d/6e remain** (STEP6-DESIGN §3, §7).
      **6c** (`3b7795598`) converted the sole pure read-side raw-working-set consumer
      (`clearMissingIdleProbes`→`infoByID` presence, byte-identical) after an
      opus+4-lens-fable audit confirmed nothing else was read-side-convertible; the raw
      working set itself (`ordered`/`beadByID`/`circuitSessionByIdentity`/`sessionLookup`)
      and every lockstep stay until 6d. **6d must** (STEP6-DESIGN §5/§7): drop every
      `session.Metadata[k]=v` lockstep, remove the raw `ordered []beads.Bead` +
      `beadByID` / `circuitSessionByIdentity` aliasing, and switch
      `refreshSessionInfo` from the raw-bead projection to `sessFront.Get` (its sole
      remaining source once the raw working set is gone). **CRITICAL:** the raw-bead
      refresh currently preserves the reconciler's deliberate intra-tick raw/store
      divergences (`reset_committed_at` kept off the in-memory bead, #2145/#2345;
      un-locksteped `started_live_hash` re-apply). A `Get`-based refresh exposes
      those — so Step 6 must add **explicit intra-tick suppression** of those keys
      (an in-memory "hidden this tick" set, analogous to `restart_requested`'s
      intra-tick field §5.2) or the #2345 force-wake regression returns. Only now do
      the reconciler files become raw-free and join `snapshotInfoOnlyFiles`.

Out of scope here: the cross-class WORK/assignment split (design §5 / Phase 6).

---

## Gates + hygiene (per commit)

- `go build ./...` · `go vet ./cmd/gc/... ./internal/session/...` ·
  `golangci-lint run ./cmd/gc/... ./internal/session/...` (**0**).
- **Byte-identical bead writes** (recording fake store) — necessary, NOT sufficient.
- **Per lockstep drop: a multi-session / read-after-write same-tick test** (the write
  oracle is blind to stale reads).
- Whole-tick `TestReconcileSessionBeads*` (205 tests; ≥420s timeout, split if it
  overloads under fork/exec) + pool/named/chaos/trace after every read/scan change.
- `git checkout go.sum` after tests. Commit AND push with `--no-verify`. Trailer:
  `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
- Never `tmux kill-server` / `go clean -cache` (`-testcache` ok). gascity Dolt is
  LOCAL-ONLY (no `bd dolt push`). #3839 stays DRAFT.

## Provenance / cautions

- Design hardened by a 4-lens adversarial review (`reconciler-front-door-spec-review`,
  16→10 findings) folded into spec v2 §9. The performance lens errored mid-run; its
  only ask (benchmark refresh-on-write) is downgraded to "fix if hot" per owner.
- **Mapping agents have repeatedly read the wrong worktree** (`.worktrees/pack-crud`).
  Pin `git rev-parse HEAD` and restrict any read-only agent to this worktree; verify
  their line numbers before acting.
- Spine-flip landed commits (Tier-0 `69ccc13c6` … cluster 4e `733812a11`) stay on the
  branch as the equivalence foundation; only their re-derive *call sites* get
  rewritten in step 3.
