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
- [ ] **Step 1 — missing `Info` mirrors.** FIRST regenerate the exhaustive key
      inventory: grep every `Metadata[...]` read reachable from the reconciler
      decision paths + `lifecycle_projection.go`. Known-missing at HEAD:
      `held_until`, `wait_hold`, `restart_requested`, `churn_count`, `wake_mode`,
      `session_name_explicit`. Add each as a raw-string `Info` mirror
      (Generation/StartedConfigHash pattern) + a `TestSessionClassifierInfoEquivalence`
      case + hold/quarantine/wait-hold/churn-spiral parity fixtures.
      **`PoolSlot`/`CommonName`/`ConfiguredNamedIdentity` already exist — do NOT
      re-add.** No call-site change. (Same shape as the 4c-foundation commit.)
- [ ] **Step 2 — coherent snapshot + refresh-on-write, alongside the existing
      lockstep** (additive, behavior-identical). Promote `session.Store.List` to a
      `ListInfo(ListFilter) ([]Info,error)` and load the tick working set as
      `[]Info`/`map[id]Info`; after any mutation, refresh that session's entry via
      `Get`. Keep the raw lockstep in place (retired in step 6). Proceed without an
      upfront benchmark; add a follow-up only if it shows hot.
- [ ] **Step 3 — per-session reads onto the snapshot.** Fold clusters 1–4e off the
      `InfoFromPersistedBead(*session)` re-derive onto the snapshot `Info`; convert
      each write + its non-`continue` dependent read as a unit. `restart_requested`
      stays an **in-memory intra-tick field** (spec §5.2 — do NOT persist it).
      Trace-payload reads of bool/int-mirrored keys keep the raw string (spec §4.1).
      E2E after each cluster.
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
- [ ] **Step 6 — drop the lockstep + remove the raw working set.** Now that all
      dependent reads are on the snapshot: drop every `session.Metadata[k]=v`
      lockstep, and remove the raw `ordered []beads.Bead` + `beadByID` /
      `circuitSessionByIdentity` aliasing. Only now do the reconciler files become
      raw-free and join `snapshotInfoOnlyFiles`.

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
