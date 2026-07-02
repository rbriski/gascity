# Reconciler Front-Door Handoff — the backlog to work through

**PR #3839** (DRAFT, base `main`), branch `upstream/object-front-doors-cleanup`,
worktree `.claude/worktrees/object-front-doors`, **HEAD `990076d86`**.

This is the authoritative handoff for finishing the session reconciler's move off
raw `beads.Bead.Metadata`, onto the typed **`session.Store`** front door. It
**supersedes** `SPINE-FLIP-HANDOFF.md` / `SPINE-FLIP-NEXT-SESSION-PROMPT.md` (the
`InfoFromPersistedBead(*session)` re-derive approach — retired; see below).

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
- [ ] **Step 4 — `LifecycleInput` from `Info` + the four cross-session scans.**
      Populate `LifecycleInput` from `Info` fields (needs step 1), then convert the
      scans onto the coherent snapshot — **`buildAwakeInputFromReconciler` first**
      (the primary consumer; drives all wake/drain via `ProjectLifecycle`), then the
      min-floor scan (`Status != "closed"` → `!Info.Closed`), then progress-signatures
      + `advanceSessionDrains`. Byte-identical `LifecycleView` fixtures across the
      full bounded key set.
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
