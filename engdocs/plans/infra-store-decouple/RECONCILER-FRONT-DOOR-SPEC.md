# Reconciler Front-Door Spec — reads + write-through under persist-and-re-Get

Status: DRAFT v2 (spec only — no production code changed by this document).
Hardened against an adversarial 4-lens review (staleness / byte-identical /
ordering / performance) — see §9 for the findings folded in.
Addendum to: `OBJECT-MODEL-FRONT-DOOR-DESIGN.md` (§3.1 session, §5 reconciler split, §7 Phases 4–5)
Branch: `upstream/object-front-doors-cleanup` (PR #3839, DRAFT)
Worktree: `.claude/worktrees/object-front-doors`, HEAD `733812a11`

This spec settles *how* the controller/reconciler stops touching raw
`beads.Bead.Metadata` — the densest leak in the front-door design — and reframes
the in-flight "spine-flip" read-routing so it lands as the design's Phase 5 rather
than a parallel mechanism.

---

## 1. Decisions locked (this conversation)

1. **Store-centric front door, not an entity handle.** The typed seam is
   `session.Store` (domain wrapper in `internal/session` holding
   `beads.SessionStore` by value, owning the codec). Callers speak
   `store.Method(id, …)`. No `Session`/`BeadSession` object is introduced.
2. **The map is hidden inside `session.Store`.** Above the codec edge nothing speaks
   `beads.Bead`, `map[string]string`, or a metadata-key literal.
3. **Every mutation is a persistent write; reads re-read from the coherent view.**
   The store is the single source of truth. No shared mutable in-memory bead graph
   and no `session.Metadata[k]=v` lockstep in the end state.
4. **`BeadSession`/backend split deferred.** Backend-identical stored form; the
   Dolt/SQLite distinction stays inside `beads.SessionStore`.
5. **The `InfoFromPersistedBead(*session)` re-derive pattern is retired** as the
   reconciler's read path — it re-projected the raw working copy instead of going
   through the front door. Its `*Info` classifier siblings and the
   `TestSessionClassifierInfoEquivalence` oracle are **kept** as the accessor logic;
   only the *source* of the `Info` changes. Spine-flip clusters 1–4e are not
   reverted.

---

## 2. GOVERNING SAFETY PRINCIPLE (added after review)

> **Never drop a lockstep until its dependent same-tick reads are already on the
> coherent snapshot.** Convert each write and every non-`continue` read of the
> same bead later in that iteration as **one unit, in one commit.**

The byte-identical **write** oracle (recording fake store) is **blind to same-tick
stale reads** — a persisted write is unchanged even when an in-memory read of it
goes stale. So write-parity is necessary but **not sufficient**. Every step that
drops a lockstep needs a **multi-session / read-after-write same-tick test**, or it
must not drop the lockstep until the dependent read is on the snapshot. This
principle drives the ordering in §6 and is why the naive "writes first, reads
later" split (v1 §6) was inverted and unsafe.

**Non-`continue` read-after-write sites (must convert as a unit with their write):**

| write | dependent read (no `continue` between) |
| --- | --- |
| `healStateWithRollback` heal + lockstep (`session_reconcile.go:1063-1067`) | `infoPostHeal := InfoFromPersistedBead(*session)` (`session_reconciler.go:~1545`) → drives the whole post-heal switch |
| zombie-capture `markProviderTerminalError` (`~1763`) | `infoPostZombie` rollback reads (`~1793`) — cluster 4e |
| desired-path mutations above `~2457` | `infoAsleepDrift` (`~2457`) — cluster 4c |
| progress-stall in-memory `restart_requested="true"` (`~2038`) | `beadRequested := …["restart_requested"]` (`~2057`) — see §5 |
| `recordChurn`/`checkChurn` chain after heal (`~2133-2172`) | `clearChurn` reads `churn_count` same iteration |

---

## 3. What already exists (do not rebuild)

- **Read half** — `session.Store.Get(id) → session.Info`, `List(...) → []Info`, via
  `InfoFromPersistedBead` (backend-invariant). `internal/session/info_store.go`.
  (There is **no** `GetInfo`/`ListInfo` — those are `Get`/`List`; a promoted
  `ListInfo(ListFilter)` is unbuilt.)
- **Write half** — `session.Store.ApplyPatch(id, MetadataPatch)` (single chokepoint)
  plus ~20 typed methods (`SetState`, `Sleep`, `BeginDrainAckStopPending`,
  `RequestRestart`, `ResetConfigDrift`, `Close`, `GetState`,
  `CircuitResetGeneration`, `RepairType`, …). `internal/session/store.go`.
- **Write vocabulary** — 25 `MetadataPatch` builders (`lifecycle_transition.go`,
  `lifecycle_exits.go`).
- **Partial Phase-4 wiring** — many writes already route through
  `sessFront *sessionpkg.Store.ApplyPatch` — **each still followed by a raw
  `session.Metadata[k]=v` lockstep** (that lockstep is what §2/§6 retire *last*).
- **`LifecycleInput` is already a typed struct** (`lifecycle_projection.go:161`)
  with `WaitHold`/`RestartRequested`/`SleepReason`/`DependencyOnly`/… fields — but
  `compute_awake_bridge.go:97-141` **cracks raw `b.Metadata[...]` to populate it.**

---

## 4. The reconciler read-consistency model

### 4.1 Per-session decision reads

Read off the session's current `session.Info` (from the coherent snapshot, §4.3),
never `InfoFromPersistedBead(*session)`. **Trace-payload caveat:** keys projected to
a typed bool/int in `Info` (e.g. `pending_create_claim` → `Info.PendingCreateClaim`
bool) lose the raw persisted bytes. Trace/diagnostic payloads that emit the verbatim
string (`session_reconciler.go:~1575` `TrimSpace(Metadata["pending_create_claim"])`)
must keep reading the raw string via a **named raw accessor**, and are excluded from
the Info-routing conversion. Behavior is unaffected; the reconciler's trace surface
is not silently normalized.

### 4.2 Cross-session aggregate reads (FOUR scans, not three)

| scan | location | when | reads |
| --- | --- | --- | --- |
| **`buildAwakeInputFromReconciler`** (PRIMARY) | `session_reconciler.go:~2670` → `compute_awake_bridge.go:18` | **after** the per-session loop | whole post-loop metadata map of every session via `ProjectLifecycle` (`b.Status`, `sleep_reason`, `wait_hold`, `restart_requested`, `continuation_reset_pending`, `dependency_only`, …); output gates **every** wake/sleep branch AND Phase-2 `advanceSessionDrains` |
| `computeNamedSessionProgressSignatures(ordered)` | reconciler Phase 0.5 | **before** the loop | progress signatures (no per-loop staleness) |
| min-floor `openInPool` (`for j := range ordered`) | inside the loop | mid-loop | `ordered[j].Status` (raw open/closed) + template |
| `advanceSessionDrainsWithSessionsTraced(..., ordered)` | Phase 2 | after the loop | drain state via `beadByID` aliased pointers |

`buildAwakeInputFromReconciler` is the load-bearing one: it reads **post-Phase-1
mutated** state for every session and drives all wake/drain decisions. It cannot be
made snapshot-coherent while it still cracks raw `b.Metadata` through
`ProjectLifecycle`, so **populating `LifecycleInput` from `Info` (§5) must land
before/with converting this scan.**

**Status has no raw `Info` mirror** — `Info` carries `Closed bool` only. The
min-floor scan's `ordered[j].Status != "closed"` becomes `!Info.Closed`, and every
close site (`finalizeDrainAckStoppedSession:~344`, failed-create close `~1509`,
`~1744`) must set `Closed` on the refreshed snapshot. Over-counting open sessions
makes `isMinFloorIdleWorker` (fires on `open <= minFloor`) wrongly recycle a
legitimate floor worker — so this scan must move onto the coherent snapshot before
the close lockstep is dropped.

### 4.3 The coherent snapshot + refresh-on-write

The tick loads a typed working set once (`ListInfo` promoting `session.Store.List` →
`[]Info` / `map[id]Info`). After any per-session mutation (which persists), the
reconciler refreshes **that session's** entry from the store (one `Get`, or a
write method that returns the new `Info`). Cross-session scans read the coherent
snapshot. This is the typed replacement for the raw `session.Metadata[k]=v`
lockstep — same coherence, typed unit, store authoritative.

> **Both the raw lockstep and the snapshot+refresh coexist during migration.** The
> raw lockstep keeps raw reads coherent; the snapshot+refresh keeps typed reads
> coherent. The raw side is retired only after *all* reads are on the snapshot
> (§2). This makes every intermediate state behavior-identical, not just
> write-identical.

Performance: one `Get` per *mutation* (most `continue` right after), not per read.
The performance-critic lens errored mid-run; **before landing §4.3, benchmark the
`Get`-refresh against the current load-once raw snapshot** (what backend
`openSessionProviderStore` reads from; per-tick mutation count). Escalation, if hot:
write methods **return** the post-write `Info` (no extra `Get`). Deferred until a
benchmark shows it.

---

## 5. Missing `Info` mirrors + the two whole-map consumers

### 5.1 Missing decision-read mirrors (the REAL read-side blockers)

`PoolSlot` / `CommonName` / `ConfiguredNamedIdentity` **already exist**
(`info_store.go:59/62/63`) — v1's "largest read-side change" pointed at done work.
The genuinely-missing mirrors the reconciler decision paths + `LifecycleInput`
population need (all verified absent from `Info` at HEAD):

| key | consumer | note |
| --- | --- | --- |
| `held_until` | `evaluateWakeReasons` suppresses ALL wake reasons; `healExpiredTimers` clears | drives desired/awake |
| `wait_hold` | `LifecycleInput.WaitHold` → `projectBlockers` | lifecycle blocker |
| `restart_requested` | `LifecycleInput.RestartRequested`; restart handoff | see §5.2 (intra-tick) |
| `churn_count` | `recordChurn`/`clearChurn`/`checkChurn` same-tick after heal | death-spiral quarantine; tightest read-after-write |
| `wake_mode` | `finalizeDrainAckStoppedSession` freshWake | drain finalize |
| `session_name_explicit` | retire/name paths | |

Add these as raw-string mirrors (Generation/StartedConfigHash pattern) **first**,
each with a `TestSessionClassifierInfoEquivalence` case, **before** any read
conversion. **Regenerate the exhaustive key inventory** by grepping every
`Metadata[...]` read reachable from the reconciler decision paths +
`lifecycle_projection.go` — do not trust this table as complete; the fixture oracle
must cover the full set (add hold/quarantine/wait-hold and churn-spiral parity
fixtures).

Once mirrored, `compute_awake_bridge.go` builds `LifecycleInput` from `Info` fields
instead of `b.Metadata[...]` — that is what makes the primary scan (§4.2) leak-free.
`ProjectLifecycle`'s `LifecycleView` output must be **byte-identical** (fixture table
across the full bounded key set, incl. missing-key-vs-empty-string semantics).

### 5.2 `restart_requested` is an intra-tick control marker (EXCEPTION)

`restart_requested="true"` is written **in-memory only** (`~2038`, no persist) and
consumed at `~2057` **same iteration, no `continue`**, then re-derived from
progress-stall detection every tick. It is an ephemeral intra-tick signal, not
durable session state. **Do NOT convert it to a persist-only typed method** — that
would (a) make `~2057` read the stale store → session not recycled
(`TestReconcileSessionBeads_ProgressStallRecyclesStaleClaimlessHealthySession`
fails), and (b) newly persist a durable flag that re-fires next tick if the kill at
`~2060` fails (today it evaporates). **Keep it as a typed intra-tick field on the
working snapshot `Info`** (set and read within the tick, never written to the
store). Documented exception to decision #3's "every mutation persists": pure
intra-tick control markers stay in-memory.

### 5.3 Circuit breaker — dedicated typed accessor, NOT `Info` (pin now)

`restoreFromMetadata` / `observeResetGenerationFromMetadata`
(`session_circuit_breaker.go:320/508`) read a **cluster of `session_circuit_*` keys**
(progress_signature, restarts, last_restart, last_progress, last_observed, opened_at,
open_restart_count, state, reset_generation/_state). `Info` exposes only
`CircuitResetGeneration`. Routing the breaker through the `Info` snapshot before all
those keys are mirrored would restore with zeroed timers/state → circuits re-open or
fail to open (silent lifecycle-safety regression). This **blocks the raw-bead removal
(§6 step 6)**, so pin it now, do not defer:

> Add a dedicated typed `session.CircuitState` value read via a named front-door
> accessor (`session.Store.CircuitState(id) (CircuitState, error)`) — **not** `Info`
> fields. Add a breaker-restore fixture to the byte-identical oracle.

`LifecycleInput` is already typed (§3); the leak is only its **population** — that
converts with §5.1, not as a separate map-typing effort.

---

## 6. Incremental order (revised — lockstep-preserving)

Each step: build-green, TDD, **byte-identical bead writes** (recording fake store)
AND — per §2 — a **multi-session/read-after-write same-tick test** wherever a lockstep
is dropped. One step per verified commit. The raw lockstep is retired **last**.

1. **Add the missing `Info` mirrors** (§5.1) + equivalence cases + parity fixtures.
   No call-site change. (4c-foundation shape.)
2. **Introduce the coherent snapshot + refresh-on-write** (§4.3) *alongside* the
   existing raw lockstep — additive, behavior-identical (benchmark first, §4.3).
   Promote `ListInfo`.
3. **Move reads onto the snapshot, write+dependent-read as a unit** (§2 table),
   including folding clusters 1–4e off the re-derive onto the snapshot `Info`.
   `restart_requested` stays the intra-tick field (§5.2). Lockstep still present
   (harmless). E2E after each cluster.
4. **Populate `LifecycleInput` from `Info`** and convert the four cross-session
   scans (§4.2) — primary `buildAwakeInputFromReconciler` first — onto the coherent
   snapshot. Byte-identical `LifecycleView` fixtures.
5. **Circuit breaker typed accessor** (§5.3).
6. **Drop the raw `session.Metadata[k]=v` lockstep** at every write site (now safe:
   all dependent reads are on the snapshot) and **remove the raw `ordered
   []beads.Bead` working set + `beadByID`/`circuitSessionByIdentity` aliasing.** Only
   now do the reconciler files become raw-free and join `snapshotInfoOnlyFiles`.

The cross-class WORK/assignment split (design §5 / Phase 6) is out of scope here.

---

## 7. Invariants

1. **Byte-identical bead writes** (recording fake store) — necessary, **not
   sufficient** (§2: blind to stale reads).
2. **Same-tick read coherence.** Per lockstep drop, a multi-session/read-after-write
   test proving the dropped lockstep's coherence is preserved. Whole-tick reconcile/
   pool E2E after every read/scan conversion.
3. **Persist-before-read** for durable state; the sole exception is the intra-tick
   `restart_requested` field (§5.2).
4. **Projection-invariance** across bd/sqlite/PG for `InfoFromPersistedBead` and every
   new mirror.
5. **Empty-string-clears** preserved on every backend.
6. **No wire change** (`Info` additions stay internal, no json tag).
7. **Trace fidelity** — bool/int-mirrored keys keep raw-string reads in trace payloads
   (§4.1).
8. **No judgment in Go** — front-door methods move serialization, never decisions.

---

## 8. Open questions

1. **Refresh-on-write vs write-returns-`Info`** — start with `Get` refresh; add the
   returning form only if the §4.3 benchmark shows it hot.
2. **`CircuitState` shape** — dedicated typed value + named accessor (§5.3); confirm
   the full key list against `session_circuit_breaker.go` before coding.
3. **Snapshot identity** — `map[id]Info` refreshed on write; confirm no consumer needs
   pointer identity (they need values now that mutations persist).

---

## 9. Review findings folded in (v1 → v2)

Adversarial 4-lens review (`reconciler-front-door-spec-review`, 16 raw → 10
surviving; performance lens errored mid-run — its benchmark ask is captured in §4.3):

- **[high] Missed the primary cross-session scan** `buildAwakeInputFromReconciler`
  → §4.2 (now four scans), §6 step 4.
- **[high] `restart_requested` intra-tick marker** would break if persisted → §5.2
  exception; §2 table.
- **[high] §6 ordering inverted** (drop lockstep before reads move) → §2 governing
  principle; §6 rewritten lockstep-last.
- **[high] Missing decision-read mirrors** (`held_until`/`wait_hold`/`churn_count`/
  `wake_mode`/`restart_requested`/`session_name_explicit`) → §5.1.
- **[high] Circuit breaker key cluster** not `Info`-feedable → §5.3 dedicated
  `CircuitState` accessor, pinned not deferred.
- **[med] min-floor scan reads raw `Status`** (Info has `Closed` only) → §4.2.
- **[med] Write oracle blind to stale reads** → §2, §7.1-2.
- **[med] §3.3 stale facts** (PoolSlot/CommonName already exist) → §5.1 corrected.
- **[low] Non-existent `GetInfo`/`ListInfo`** → §3 (they are `Get`/`List`).
- **[low] Trace-payload normalization** → §4.1 caveat.
