# Reconciler Front-Door Handoff — the backlog to work through

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `.claude/worktrees/object-front-doors`, **HEAD `4617c0821`**.

This is the authoritative handoff for finishing the session reconciler's move off
raw `beads.Bead.Metadata`, onto the typed **`session.Store`** front door. It
**supersedes** `SPINE-FLIP-HANDOFF.md` / `SPINE-FLIP-NEXT-SESSION-PROMPT.md` (the
`InfoFromPersistedBead(*session)` re-derive approach — retired; see below).

**Status (as of `4617c0821`):** Steps 0–4 DONE. Next actionable = **Step 5**
(circuit-breaker typed `CircuitState` accessor over the Phase-0.5 CB reads). Session
commits `cece437df`..`4617c0821` (16).
`RECONCILER-FRONT-DOOR-NEXT-SESSION-PROMPT.md` is paste-ready for Step 5.

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
- [ ] **Step 5 — circuit-breaker typed accessor.** Add `session.Store.CircuitState
      (id) (CircuitState, error)` reading the full `session_circuit_*` key cluster
      (progress_signature/restarts/last_restart/last_progress/last_observed/opened_at/
      open_restart_count/state/reset_generation) — a dedicated typed value, **NOT**
      `Info`. Route `restoreFromMetadata`/`observeResetGenerationFromMetadata` through
      it. Breaker-restore fixture in the oracle. (Blocks step 6 — do not defer.)
- [ ] **Step 6 — drop the lockstep + remove the raw working set + cut refresh over
      to `Get`.** Now that all dependent reads are on the snapshot: drop every
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
