# Adopting beads claims + CAS in Gas City: what it buys us, and what's missing

> **Status**: Research / analysis (2026-07-09). No code change proposed yet — this is
> the decision input.
> **Method**: 5 Opus exploration agents mapped the beads claim/CAS surface and Gas
> City's concurrency machinery; 2 Fable design lenses + 1 synthesis produced the
> adoption map and gap analysis; 3 Fable red-team lenses (over-claim, federation,
> TTL/liveness) produced 21 findings, of which the load-bearing ones are folded into
> the body below and logged in §9.
> **Ground truth verified against**: `/data/projects/beads` @ `local/deploy-current-integrated`
> (GC's deploy line) and `origin/cas/optimistic-concurrency` (worktree, HEAD `0980ced47`).

---

## 1. Executive summary

Gas City already stands on beads' one atomic ownership primitive — `ClaimIssueInTx`
via `gc hook --claim` — and hand-rolls **everything else** about ownership (release,
reclaim, reservations, session-create fencing, epoch advancement) as read‑modify‑write
with genuine TOCTOU windows. Beads has since grown the missing layers, but they land in
three separately-gated tiers, and **none of them are on GC's deploy branch today**:

| Layer | Upstream beads | GC deploy branch (`local/deploy-current-integrated`) | Gate on adoption |
| --- | --- | --- | --- |
| Atomic claim (`ClaimIssueInTx`) | merged | **present, in use** via `bd update --claim` | — |
| Lease TTL + heartbeat + reclaim (schema **v54**, `DefaultLeaseTTL=5m`, `bd heartbeat`, `bd reclaim`) | merged (`e97839a2e`) | **absent** | port + `0054` renumber + doltlite dialect + bd bump |
| `bd unclaim` (manual release) | merged (`31a6ec951`) | **absent** | port + bd bump |
| **CAS**: `revision` col (0055), `--if-revision`, `bd cas`, federation gate, merge resolver | **UNMERGED** (`f9a929f03`, branch only) | **absent** | land upstream first + bd bump + provider plumbing |

Adopting the full stack could delete a meaningful amount of brittle guard code and close
real races — but the honest, **scoped** picture after adversarial review is narrower than
the headline suggests. Four constraints dominate and none can be softened:

1. **The CAS surface is unmerged.** `revision`/`--if-revision`/`bd cas` exist only on
   one squashed commit on a feature branch. No production GC code should be built against
   it until it lands upstream.
2. **The live control-plane store is SQLite, and CAS is Dolt-only.** GC's coordination /
   graph / control-plane state (drain reservations, `gc.control_epoch`) lives on the
   `graph_store=sqlite` / doltlite path (`internal/storeref/storeref.go:5`). Beads
   implements `ConditionalWriter` **only** for `DoltStore` and `EmbeddedDoltStore`, with
   Dolt-flavored JSON SQL (`JSON_UNQUOTE`, which SQLite lacks). So the two *highest-confidence*
   metadata-CAS adopters return `exit 13` **exactly where that state physically lives** —
   until a doltlite `ConditionalWriter` (SQLite-dialect JSON + the 0054/0055 schema mirror)
   is built in the fork.
3. **The lease/reclaim backstop excludes wisps.** `ReclaimExpiredLeasesInTx` "only ever
   touches the permanent issues table: wisps are ephemeral and are never leased work"
   (`lease.go:142`). GC's controller-dispatched orchestration work materializes as **wisps**
   (order dispatch, molecule steps). So the beads TTL backstop covers **none** of the
   graph-orchestration workload — that work stays 100% on GC's session-death reclaim, forever.
4. **Beads TTL is not a drop-in for agent-shaped work.** GC workers are coding agents whose
   legitimate steps run minutes to hours (adopt-pr iterations ~8h); a `5m` default lease with
   no heartbeat mass-reclaims healthy work, and self-heartbeat can't guarantee cadence ≪ TTL
   during long generations. The TTL/heartbeat *mechanism* is sound; wiring it to GC's
   workload is a **blocking design question**, not tuning.

**Verdict: adopt-with-additions — but selectively and in a strict order.** The claim
primitive is already adopted and correct. The near-term, available-today wins are small and
high-confidence (typed claim-conflict errors; closing the drain race via GC's *existing*
guarded-SQL pattern on Dolt work stores). The headline wins — metadata CAS for control-plane
fences, and the lease backstop — are each blocked on a specific fork-side prerequisite
(doltlite `ConditionalWriter`; wisp lease coverage + a heartbeat driver). Sequence the
prerequisites first, or the adoption buys race-closure *everywhere except where GC runs*.

---

## 2. What beads offers: the primitives, precisely

Six adoptable buckets, with the caveats that survived review:

1. **`ClaimIssueInTx` / `ClaimReadyIssueInTx`** — atomic claim: sets `assignee` +
   `status='in_progress'` only `WHERE status='open' AND (assignee='' OR assignee=?actor)`;
   idempotent for same-actor re-claim; `ErrAlreadyClaimed` (other owner) vs `ErrNotClaimable`
   (wrong status). Merged, and **already GC's claim path**. The adoption surface is entirely
   on the *release / reserve / fence* side, not the claim side.

2. **`bd unclaim` / `UnclaimIssue`** — manual release: clears assignee, resets
   `in_progress→open`, optional reason comment. Merged upstream, absent from GC's deploy
   branch. **Unconditional** — no assignee/revision guard, so a race between `unclaim` and a
   legitimate re-claim is possible (this matters for §4). Two known warts: it refuses
   unassigned/closed issues (so it can't repair `in_progress`-with-empty-assignee, which GC's
   sweeps do), and it leaves stale `lease_expires_at`/`heartbeat_at` + an un-rewritten
   `row_lock` on the reopened row (latent hygiene bug, `unclaim.go`).

3. **Single-key metadata CAS** — `CompareAndSetMetadataKey` (set-if-absent / set-if-equals),
   surfaced as `bd cas set`. A lock-free fence/lease/epoch primitive. **Not federation-gated.**
   CAS-branch only.

4. **Guarded release** — `CompareAndClearMetadataKey`, surfaced as `bd cas unset --if`.
   Idempotent when the key is already absent. **Not federation-gated.** CAS-branch only.

5. **Whole-row optimistic concurrency** — `UpdateIssueIfMatch` / `CloseIssueIfMatch` /
   `DeleteIssueIfMatch` guarded by `--if-revision` over the schema-v55 `revision` column;
   `PreconditionFailedError` (exit 9) on mismatch. **Federation-gated** (see §2.1). CAS-branch
   only. Note: `revision` is an **opaque, non-monotonic** nonce (equality-only; `0` = pristine
   sentinel), and *every* write — including a metadata CAS on the same bead — bumps it, so an
   outstanding whole-row token is invalidated by any intervening write. This makes whole-row
   CAS **strictly stronger and more failure-prone** than a semantic `WHERE status=… AND
   assignee=…` guard (it fails on unrelated writes too).

6. **Lease TTL + heartbeat + reclaim** — schema v54: `lease_expires_at` (second-granular
   DATETIME), `DefaultLeaseTTL=5m`, `HeartbeatIssueInTx` (owner-only:
   `WHERE status='in_progress' AND assignee=?actor`), `ReclaimExpiredLeasesInTx` (reverts
   leases expired before `cutoff = now − graceWindow`, supervisor uses `graceWindow=2×TTL`),
   and a Dolt-specific `row_lock` anti-lost-update cell. Merged upstream, absent from GC's
   deploy branch. **A semantic change, not a drop-in** (§5): beads reclaims on *worker
   heartbeat silence*; GC reclaims on *session-bead death*. **Wisps are excluded** (`lease.go:142`).

### 2.1 The federation / topology caveat, stated correctly

- **Whole-row CAS gate** (`conditional_gate.go`): refused (`exit 13`,
  `ErrConditionalWriteUnsupported`) **iff a Dolt remote is configured for that DB**
  (`dolt_remotes` count / `HasPersistedRemote`), because a `bd dolt pull` 3-way merge can slip
  a foreign write past the revision guard. It gates on **remote-configured**, *not* on branch
  name and *not* on "split-store" per se. Override: `BD_ALLOW_UNSAFE_CAS=1` (sound only for a
  true single linear writer). **Implication:** GC's split-store shape (per-rig Dolt DBs, sqlite
  graph store, cross-store convoys with no remotes between them) does **not** trip the gate —
  each store is its own linear timeline. Since gascity Dolt is **local-only** (`ga-9wsri`), the
  gate should rarely fire on today's fleet. The real inverse hazard: `ga-9wsri`'s recurring
  *doomed `origin` remote* would silently flip whole-row CAS to `exit 13` on an otherwise
  single-writer DB — worth a doctor check for stale `dolt_remotes` rows before any
  `--if-revision` adoption.
- **Metadata-key CAS is deliberately NOT gated** — its guard cell (the metadata JSON blob)
  conflicts whole-cell on merge, so it stays sound across branches. This is why most GC
  coordination state (which is metadata) is the *safe* adoption target — **where a
  `ConditionalWriter` exists at all** (see the SQLite blocker, §1 point 2).
- **"Proxied/server store" is not a separate store type** (correcting an earlier draft claim):
  it is a config flag on the same `DoltStore`, which fully implements `ConditionalWriter`; the
  `dbproxy` daemon is a *local* unix-socket sharer, not the hosted gateway. The hosted path
  (bd → Dolt sql-server over TCP via `BEADS_DOLT_CREDENTIAL_COMMAND`) is served by `DoltStore`
  and therefore **does** support CAS. So "CAS is unavailable on hosted" is **not** established —
  the real hosted questions are (a) whether the hosted/HA Dolt has a **remote/replica**
  configured (trips the whole-row gate), and (b) revision-guard soundness across HA failover
  (`corp-public-ha` runs a replicated beads-gateway). Both are open questions, not facts.

**Net:** the popular framing "beads claims never expire / no CAS" is true for **what GC runs
today** and false for beads upstream. Most of the adoption problem is *fork integration*
(land three tiers into the deploy line, teach the doltlite backend the new columns + dialect,
renumber `0054`, bump two pins) plus a short list of genuinely-missing primitives (§5).

---

## 3. What Gas City hand-rolls today

GC has exactly **one** atomic ownership primitive (the claim). Around it:

- **Drain reservations** (`internal/dispatch/drain.go:1227-1292`): `Get` member → check
  `gc.exclusive_drain_reservation` empty-or-self → `SetMetadata`. Real TOCTOU. **Note:** release
  writes the key to `""` (empty string) rather than removing it (`drain.go:1283`) — so a naive
  `--if-absent` acquire fails forever on legacy-released beads.
- **Hand-built conditional release** — `BdStore.ReleaseIfCurrent` (`internal/beads/bdstore.go:1095`,
  guarded SQL `UPDATE … WHERE status='in_progress' AND assignee=?` via `bd sql --json`) and
  `NativeDoltStore.ReleaseIfCurrent` (`native_dolt_store.go:626`, a native-tx read-check-update
  with a real window). Notably GC **already implements `ConditionalAssignmentReleaser`**
  (`native_dolt_store.go:168`) — it has a working conditional release on the Dolt path today.
- **Orphaned-work sweeps** (`cmd/gc/pool_session_name.go:114`, `session_beads.go:757`,
  `dead_assignee_event.go`): re-read, compare status+assignee to snapshot, then **unconditional**
  clear. A worker re-claiming between recheck and clear is silently stomped.
- **Async session-create fencing** — `internal/session/pending_create_lease.go` (119 LOC) +
  `session_reconcile.go:1105`: hand-rolled optimistic concurrency over
  `instance_token`/`generation`/`pending_create_claim`/`state`, with `CommitVerdict` compare
  logic and three hand-rolled TTLs (60s startup / 1m stale-creating / 10m never-started).
  ("Lease" here is a **misnomer** — it's a generation-compare CAS gate, not a TTL lease.)
- **Control-epoch fencing** (`internal/dispatch/control.go:304-336`): read metadata →
  `strconv.Atoi` → compare → **blind** `SetMetadata`. Lost-update-prone.
- **Claim-conflict detection by string matching** (`isBdClaimConflictMessage`,
  `bdstore.go:822`): substring-matches bd's error text ("already assigned" / "claimed by") —
  brittle across bd versions.
- **7+ scattered expiry timers** — but only the **work-strand** cases
  (`strandedRepairConfirmGrace` 2m, `idleClaimNudge` 90s/3m) exist "because a claim is
  permanent". The rest (`pendingCreateNeverStarted` 10m, `staleCreating` 1m,
  `postCreateProtection` 2m, circuit-breaker 30m/60m) are **session-lifecycle** timers that stay
  regardless of any work-bead lease.
- **Deliberate non-bead coordination that must stay**: `mcp_project_lock.go` (a `flock` on a
  *file*, not a bead), and the `gc.routed_to` visibility dance (routing decoupled from ownership
  so the single atomic claim is the only contended write — **correct architecture, not debt**).
- **`merge_slot`** is already a beads-side, tx-atomic primitive driven from pack formulas —
  correct as-is.

---

## 4. Adoption map (corrected confidences)

Confidence columns reflect the **live fleet** after adversarial review. "Blocked" = needs a
fork-side prerequisite before it works where the state lives.

| GC mechanism (file) | Today | Beads primitive | Complexity removed | Confidence (live fleet) |
| --- | --- | --- | --- | --- |
| **`reserveDrainMember`** (`drain.go:1227-1292`) | Get→check→`SetMetadata` on `gc.exclusive_drain_reservation`; TOCTOU | Metadata CAS `set --if-absent` + guarded `unset --if` | ~46 LOC → ~20; race **closed** (post-migration) | **Blocked → Low on live fleet**: this bead lives on `graph_store=sqlite`; beads metadata-CAS is Dolt-only. High **once** doltlite `ConditionalWriter` exists. Also handle the `""`-vs-absent release value and mixed-version rollout. |
| **`gc.control_epoch` sync** (`control.go:304-336`) | read→`Atoi`→compare→blind `SetMetadata`; lost-update race | Metadata CAS `set --if <observed>` | ~10 LOC; real race closed | **Blocked → Low on live fleet** (same SQLite store). Guard must use the *observed snapshot* value (epoch can jump by >1), with a re-read/retry on miss. |
| **`ReleaseIfCurrent`** (`bdstore.go:1095`, `native_dolt_store.go:626`) | GC's own guarded-SQL / native-tx conditional release | *Not* a like-for-like: `--if-revision` guards the **revision nonce**, not `status+assignee`, and any intervening write invalidates it. The true match is **guarded unclaim (`IfAssignee`)** — a **proposed** beads addition (§5 gap 7). | ~80 LOC deletable **iff** guarded unclaim ships; otherwise a semantic change (revision-match ≠ assignee-match) | **Medium, contingent.** GC already has the guarded-SQL version working on Dolt today; swapping to a supported primitive waits on §5 gap 7. |
| **Session-death reclaim** (`session_beads.go:757`, `pool_session_name.go:114`, `work_assignment.go:134`) | unconditional clear of assignee + affinity metadata + `run_target` fallback stamp | `bd unclaim` for the clear; **guarded unclaim** to make it atomic vs re-claim | stomp window closes; **`ReleaseWorkBead` does not disappear** — fallback-`run_target` + affinity clearing are GC routing semantics | **Medium** for `bd unclaim`; guarded variant contingent on §5 gap 7. Route the write to the bead's **owning** store. |
| **Route-reclaim order** (`order_dispatch.go:1946`) | config-driven idempotent sweep; verbs are hand-written `bd update`/SQL | `bd unclaim <id> --reason` as the formula verb (assigned-work branch only) | removes hand-written guarded SQL from packs; Go dispatch unchanged (correctly) | **Medium.** Keeps a second verb for the empty-assignee repair branch (`unclaim` refuses unassigned). Cannot "retire the sweep" — see §5 gap 3. |
| **`PendingCreateLease` commit** (`pending_create_lease.go`, `session_reconcile.go:1105`) | Get→`CommitVerdict`→Update RMW window | Metadata CAS on `pending_create_claim` makes the **fence acquire/handoff** atomic | fence transition atomic; **`CommitVerdict`'s `Closed`/identity/`state` checks + the commit write stay** (not expressible in one-key CAS); requires reshaping the key from boolean→token | **Medium, partial.** Reduces, not eliminates, the #1542/#2073-class race. Same SQLite-store availability caveat if the session store is sqlite. |
| **`gc hook --claim` + `isBdClaimConflictMessage`** (`cmd_hook_claim.go`, `bdstore.go:822`) | atomic claim (already beads) + **string-matched** conflict detection | Replace string match with **typed** `PreconditionFailedError` / exit-9 | ~30 LOC of brittle text-matching → exit-code check | **High** for the typed-error swap (needs the typed contract, §5 gap 8). Candidate selection / route filtering / multi-store iteration **stay**. |
| **`gc.routed_to` dance** (`sling_core.go`, `graphroute.go`) | routing decoupled from ownership | **Keep it.** Correct architecture around the single atomic claim. | ~0 | **High (that it stays).** Do *not* move routing stamps onto whole-row CAS. |
| **`mcp_project_lock`** (`materialize/mcp_project_lock.go`) | `flock` on a file | **None** — not bead state | 0 | **High (that it stays).** |
| **`merge_slot`** (beads-side) | tx-atomic holder+waiters RMW inside `RunInTransaction` | Mostly already served; CAS would simplify the holder cell only | modest, in beads not GC | **Low-Medium.** Correct as-is. Waiters queue isn't a single-key-CAS shape. |

### Quantification (net, not gross)

The gross "compare/guard code that becomes deletable" is ~250–400 LOC across 8 mechanisms, but
that is **contingent and offset**. The **unconditionally deletable-now** subset is closer to
**~120–150 LOC** (drain ~46, string-matching ~30, `control_epoch` ~10, recheck guards ~30,
unclaim-family partial) — and even that is gated on the SQLite `ConditionalWriter` for the two
metadata-CAS items. Meanwhile the **provider-boundary work** to *enable* CAS (§5 gap 5:
`Bead.Revision`, three `Store` methods, implementations + cache invalidation across `BdStore` /
`NativeDoltStore` / `CachingStore` / memstore / filestore, exit-9/13 mapping, owning-store
routing, tests) plausibly **adds as much code as it removes**. So the honest near-term LOC
effect is **roughly neutral**; the real payoff is **race closure + typed errors**, not deletion.

### Top-3 by value (corrected)

1. **Typed claim-conflict errors** (`isBdClaimConflictMessage` → exit-9). Small, high-confidence,
   available as soon as the typed contract lands, no topology dependency. *This is the best
   first adoption*, not the drain race.
2. **Drain reservation → metadata CAS** — genuinely closes a dispatcher-vs-dispatcher race and is
   the documented use case, **but blocked** on a doltlite `ConditionalWriter` because the bead
   lives on the SQLite control store. Until then, the interim option is GC's *existing*
   guarded-SQL pattern (`ReleaseIfCurrent`-style) applied to the reservation key on Dolt stores.
3. **Guarded-release family** (`ReleaseIfCurrent` + orphan sweeps) → **guarded unclaim** (§5 gap 7,
   a proposed addition). Largest LOC removal and closes the reclaim-stomps-fresh-claim TOCTOU —
   but contingent on upstream accepting the guard, and on non-proxied stores.

---

## 5. Gaps & recommendations

| # | Gap | Why GC needs it | beads? | Recommendation | Owner / proposed API |
| --- | --- | --- | --- | --- | --- |
| 1 | **Lease TTL / auto-expiry** | dead worker strands a bead `in_progress` forever on GC's deploy line | v54 upstream; **absent** on deploy branch; **excludes wisps** | **Adopt from beads (integration)** + surface TTL to packs | Port v54; add `bd update/ready --claim --lease-ttl <dur>` exposing `WithLeaseTTL`. Scope: **permanent issues only** — wisps unaffected. |
| 2 | **Heartbeat / liveness** | TTL alone reclaims live-but-slow workers | `HeartbeatIssueInTx` owner-only; no batch form | **Hybrid — but feasibility is a blocker** | See §5.1. Owner-only semantics + agent step durations make this the hard part, not a footnote. |
| 3 | **Reclaim on session death** | GC's real liveness signal is the *session bead*, not worker heartbeat; must fire immediately, faster than any TTL | **NO — and shouldn't** (sessions are a GC concept) | **Keep in GC**, re-plumbed onto guarded release so the sweep stops being a TOCTOU | `session_beads.go:757`, `pool_session_name.go:114`, route-reclaim orders. `bd reclaim` is a **backstop for the work-bead half only**. |
| 4 | **Control-plane CAS on the live (SQLite) store** | drain reservation + `control_epoch` live on `graph_store=sqlite`; beads CAS is Dolt-only (`JSON_UNQUOTE`) | metadata-CAS: Dolt/embedded only | **Add (fork): doltlite `ConditionalWriter`** with SQLite-dialect JSON + 0054/0055 schema mirror | **Prerequisite** for the two flagship metadata-CAS adopters. Without it they return exit 13 on the live fleet. |
| 5 | **Revision / CAS through GC's provider boundary** | `internal/beads/beads.go` exposes no conditional surface; conflicts detected by string-match | CAS-branch only | **Keep in GC** (plumbing), gated on `f9a929f03` landing | Add `Bead.Revision`; `CompareAndSetMetadata` / `CompareAndClearMetadata` / `UpdateIfRevision`; map exit 9→`ErrPreconditionFailed`, exit 13→`ErrConditionalWriteUnsupported`; implement + invalidate in `CachingStore`/memstore/filestore; route to the **owning** store; bump `go.mod` + `deps.env BD_VERSION` in lockstep (`TestBDVersionPins`). |
| 6 | **Multi-bead / cross-store atomic claim** | convoys span stores | **NO** — CAS is single-timeline, single-row | **Keep in GC — do NOT ask beads for distributed txns** | `gc.routed_to` already funnels every contended write through one atomic per-bead claim; cross-store stays saga-style. Optional narrow add *if needed*: `ClaimIssuesInTx(ids)` all-or-nothing **within one store**. |
| 7 | **Guarded release (unclaim-if-still-owned)** | `ReleaseWorkBead` is read-recheck-then-**unconditional** clear; a re-claim in the window is stomped | **NO** — `bd unclaim` is unconditional | **Add to beads** | `UnclaimIssueInTx(…, opts…)` with `IfAssignee(v)` / `IfRevision(n)` folded into the WHERE clause; mismatch → `PreconditionFailedError`. The single clearest CAS-adoption target. |
| 8 | **Typed claim-conflict on the CLI wire** | `isBdClaimConflictMessage` string-matches | partial (CAS branch adds exit 9/13 for `cas`/`--if-revision`; `--claim` still text) | **Add to beads** (small) | `bd update --claim` conflict → distinct exit + JSON `{"code":"already_claimed","holder":"<actor>"}`; GC drops the matcher. |
| 9 | **Metadata-CAS keys never expire** | a crashed holder of `gc.exclusive_drain_reservation` leaves the key set forever | NO (by design) | **Keep in GC** | reconciler-driven expiry stays; don't push per-key TTL into beads. If a fence needs auto-expiry, model it as a real claim (assignee) instead. |
| 10 | **`bd unclaim` lease-column hygiene** | reopened rows keep stale `lease_expires_at`/`heartbeat_at` + un-rewritten `row_lock` | bug in merged upstream code | **Fix in beads** | extend the unclaim UPDATE to null the lease cols + fresh `row_lock`; file upstream before GC builds on unclaim. |
| 11 | **Lease/CAS correctness on DoltLite + backend conformance** | `row_lock` zombie-merge guard proven only on Dolt 2.1.x; the fork runs DoltLite + per-rig Dolt; conformance suite has **zero** claim/lease coverage | gap | **Hybrid** | add claim/lease/heartbeat/reclaim cases to the backend conformance suite; run against the fork's doltlite path before relying on TTL reclaim there. |
| 12 | **Migration `0054` collision** | deploy line has `0054_ready_work_indexes`; upstream v54 is `0054_add_lease_columns` (+`0055_add_revision`) | n/a | **Fork integration task** | renumber during the lease-stack port; validate against **live per-rig DBs** (schema skew has killed the dispatcher before). |
| 13 | **Wisp lease coverage** | controller-dispatched orchestration work is **wisps**; `ReclaimExpiredLeasesInTx` excludes wisps entirely | **NO — excluded by design** | **Open design question** | either accept that wisp work stays 100% on GC session-death reclaim (likely correct — wisps are ephemeral/session-scoped), or propose upstream wisp-lease support. Do **not** claim the beads backstop covers controller work. |

### 5.1 The TTL / heartbeat / reclaim question, resolved honestly

The mechanism (lease expiry as a wall-clock fact + a reclaim sweep) belongs in beads, exactly
like the atomic claim. *How long*, and *what else happens on reclaim*, is judgment/config and
belongs in GC packs. But three facts make this a **hybrid with a hard boundary**, not a
migration:

- **Different liveness signals.** Beads' reaper keys on *worker-heartbeat silence*; GC's reclaim
  keys on *session-bead death* (lifecycle projection + detached-probe + snapshot-partial gating).
  A confirmed-dead session must free its work **immediately** — not after `5m` TTL + `2×TTL`
  grace (~15 min). Conversely a live session with a wedged worker never fires GC's session-death
  path but does heartbeat-expire — *if* heartbeats are actually emitted (see below).
- **`bd reclaim` only reverts the bead.** It cannot close the session bead, free the pool slot,
  clear affinity metadata, stamp the `run_target` fallback, or respect GC's degraded-read posture.
  **`repairStrandedPoolWorkerBead` therefore cannot be retired** by `bd reclaim`: it fires at 2m
  *and closes the session bead* (freeing the slot). Retiring it would be a ~7× work-recovery
  latency regression and a *permanent* slot-recovery regression. `bd reclaim` backstops **only
  the work-bead half GC's sweeps miss.**
- **Heartbeat feasibility is the blocker, not tuning.** `HeartbeatIssueInTx` is owner-only
  (`WHERE assignee=?actor`), so a single-actor **batch** call can only heartbeat one worker's
  beads — the "batch to avoid a Dolt commit-flood" idea doesn't pay unless a central supervisor
  heartbeats *all* leases, which is ownership-spoofing **and** converts the lease into a mirror of
  GC's session-liveness (killing the "catch the wedged worker" benefit). And GC workers are coding
  agents: a step mid-generation emits no tool calls for 30+ min, so `gc hook --heartbeat`
  self-heartbeat can't guarantee cadence ≪ TTL for any minutes-scale TTL. The dilemma:
  **TTL = 2–3× worst legitimate step ⇒ hours-scale ⇒ the backstop is strictly slower than every
  existing GC window (2m/90s) and adds ~zero value over session-death reclaim; TTL = minutes ⇒
  healthy long-running work gets mass-reclaimed.** Pick the workload-shaped answer before wiring
  anything.

**Concrete division (corrected):** **GC = fast path** — session-death reclaim keeps firing
immediately, re-plumbed onto guarded release so it can't stomp a concurrent re-claim; **beads =
backstop for the permanent-issue work-bead half only**, with a *generous* TTL and a heartbeat
driver *if and only if* §5.1's feasibility question resolves; **wisp orchestration work stays
entirely on GC.** The "one-clock" idea is real but was mis-drawn in the draft: the binding
constraint is worst-case **heartbeat gap < TTL + grace** (long generations, commit latency,
controller downtime, cross-node clock skew — timestamps are client-side `time.Now().UTC()`), not
"grace > cadence"; and GC's event-triggered fast path can't be *ordered* against a wall-clock path
by a doctor check — a doctor check can only compare configured durations and flag stale
`dolt_remotes`. `bd reclaim` already tolerates clock races structurally (per-row predicate
re-check + `row_lock` rescue), so the failure mode is mis-sized windows, not "two clocks racing."

---

## 6. Migration path (ordered, dependency-aware)

Everything `f9a929f03`-independent goes first; prerequisites for the live fleet go before the
adopters that need them.

1. **Fix gap 10 upstream** (unclaim lease-column hygiene) and **port the merged layers into the
   deploy branch**: v54 lease stack (`#4537`) + `bd unclaim` (`#4614`). Resolve the **0054
   collision** (renumber, validate against every live per-rig DB). Teach the **fork's doltlite
   backend** the v54 columns + claim/lease SQL in **SQLite dialect** (gap 11). No dependence on CAS.
   **Interlock:** the claim UPDATE stamps `lease_expires_at = now + 5m` unconditionally, so after
   this step every `gc hook --claim` bead carries a 5-minute lease with **no heartbeat wired** —
   a single `bd reclaim` in the step-1→step-4 window would mass-reclaim the in-flight fleet.
   *Either pass a disabled/very-long TTL from day one, or ship the "nothing runs `bd reclaim`
   until the heartbeat driver exists" rule (ideally the doctor check) in this step.*
2. **Resolve §5.1** (heartbeat-driver feasibility for agent-shaped work) as an explicit design
   decision **before** enabling any reclaim. This is a gate, not a tuning pass.
3. **Propose the small upstream additions** (CAS-independent): guarded unclaim
   (`UnclaimIssueInTx` w/ `IfAssignee`/`IfRevision`, gap 7), typed claim-conflict JSON (gap 8),
   batch heartbeat *only if* §5.1 lands on the supervisor model (gap 2). Land the **typed
   claim-conflict** swap in GC (best first, topology-free) win.
4. **Build the fork doltlite `ConditionalWriter`** (gap 4) — the prerequisite for control-plane
   metadata CAS. Add conformance lease/CAS coverage (gap 11).
5. **Bump the two beads knobs in lockstep** (`go.mod` + `deps.env BD_VERSION`/`BD_CURRENT_REF`,
   `TestBDVersionPins`) to a fork build containing the ported layers (and CAS once landed).
6. **Wire the merged stack:** claim-with-TTL, session-death reclaim onto guarded unclaim,
   `bd unclaim` as the route-reclaim formula verb (assigned branch; keep the empty-assignee verb),
   `bd reclaim` **backstop** for permanent-issue work only, doctor checks (stale `dolt_remotes`;
   TTL vs GC-window sanity).
7. **After `f9a929f03` lands upstream** (do not build production code against `/tmp/beads-cas`
   before then): extend the provider boundary (gap 5); adopt CAS call sites **in risk order** —
   typed claim-conflict → drain reservation (once doltlite CAS exists) → `ReleaseIfCurrent`
   replacement + guarded release in the sweeps (unguarded fallback where a store reports
   unsupported) → `PendingCreateLease` fence → `gc.control_epoch`. Whole-row `--if-revision` last,
   behind the `ErrConditionalWriteUnsupported` seam. Cross-store atomicity: **declined** (gap 6).

### Residues that stay in GC no matter what

All expiry/TTL judgment for metadata fences (gap 9); routing (`gc.routed_to`, run-target
fallback, cross-store candidate iteration); session-lifecycle liveness derivation
(`ProjectLifecycle`, `openSessionOwnsWork`) — beads only makes the resulting write atomic;
**all wisp/orchestration reclaim** (gap 13); filesystem locks (`mcp_project_lock`); and the
unguarded fallbacks wherever a store reports `ErrConditionalWriteUnsupported`.

---

## 7. Risks & open questions

**Risks.** (1) **Unmerged-branch risk** — gap 5 + whole-row CAS ride on one squashed commit;
if it's reworked, provider work churns → mitigated by sequencing (merged layers + doltlite CAS
first). (2) **Live-fleet SQLite blocker** — the two flagship metadata-CAS adopters are unavailable
until the fork doltlite `ConditionalWriter` exists. (3) **Wisp coverage** — the lease backstop
covers no orchestration work; overstating it would give false confidence. (4) **Heartbeat
feasibility** — agent-shaped step durations may make the TTL backstop either too slow or actively
harmful. (5) **Behavior change** — the lease stack changes *when* work is reclaimed
(worker-heartbeat vs session-death); ship it as an explicit decision. (6) **Merge friction** —
once every write stamps a fresh revision nonce, same-bead concurrent edits always conflict on the
revision cell (mostly moot while gascity Dolt is local-only, `ga-9wsri`). (7) **Schema-skew blast
radius** — the `0054` renumber + doltlite column add must be validated against live per-rig DBs
(prior incidents: v53/v54 skew killed the dispatcher). (8) **Two-knob bump** must move in lockstep.

**Open questions.** (1) Will `f9a929f03` land as-is or be reworked? (2) Does the hosted/HA
beads-gateway have a remote/replica configured (trips the whole-row gate), and is revision-guard
sound across HA failover? (3) Does the `row_lock` guard hold on DoltLite's merge semantics?
(4) What TTL/heartbeat model fits agent-shaped work (§5.1) — supervisor vs self, and at what TTL?
(5) Will upstream accept guarded unclaim / typed claim-conflict / batch heartbeat / conformance
coverage, or does the fork carry them as patches (against the upstream-alignment rules)?
(6) How disruptive is the `0054` renumber + doltlite column add against live per-rig Dolt DBs?
(7) `PendingCreateLease` fence flavor — metadata-key CAS (un-gated) vs whole-row `--if-revision`,
given the session bead also takes concurrent non-fence metadata writes that invalidate revision
tokens? (8) Could `bd ready --claim`'s `WorkFilter` grow `gc.routed_to` route filtering upstream,
collapsing `cmd_hook_claim.go`'s candidate loop onto `ClaimReadyIssueInTx`?

---

## 8. Verdict & top recommendations

**Verdict: adopt-with-additions — selectively and in order.** The claim primitive is already
adopted and correct. The available-today wins are small and high-confidence; the headline wins are
each blocked on a specific fork-side prerequisite, and one popular framing (a beads TTL backstop
for controller work) is largely **unavailable** because that work is wisps. Do not soften: **(a)**
CAS is unmerged — no production code against it until it lands; **(b)** the live control-plane
store is SQLite and beads CAS is Dolt-only — build the doltlite `ConditionalWriter` first or the
flagship adopters fail exactly where the state lives; **(c)** the lease backstop excludes wisps and
GC's session-death reclaim stays the fast path; **(d)** heartbeat feasibility for agent-shaped work
is a design gate, not tuning.

**Top recommendations:**

1. **Land the topology-free win first:** typed claim-conflict errors (drop `isBdClaimConflictMessage`)
   once beads exposes a stable exit-9/JSON contract for `bd update --claim` conflicts.
2. **Port the merged v54 lease stack + `bd unclaim` into the deploy branch** — resolving the 0054
   collision, teaching the doltlite backend the columns/dialect, validated against live per-rig DBs —
   but treat it as a fork-integration project, and **do not enable `bd reclaim` until §5.1 is
   resolved** (interlock the default TTL or ship the doctor check in the same step).
3. **Keep session-death reclaim in GC as the fast path**, re-plumbed onto guarded release; use beads
   TTL/`bd reclaim` only as a **permanent-issue work-bead backstop**; `repairStrandedPoolWorkerBead`
   **stays** (it frees the slot, which `bd reclaim` cannot).
4. **Build the fork doltlite `ConditionalWriter`** (SQLite-dialect JSON + 0054/0055 mirror) as the
   prerequisite for control-plane metadata CAS; add claim/lease/CAS backend conformance coverage.
5. **Only after `f9a929f03` lands:** extend `internal/beads` with the conditional-write surface +
   typed errors, then adopt in risk order (drain reservation → `ReleaseIfCurrent` → guarded release
   in the sweeps → `PendingCreateLease` → `control_epoch`; whole-row `--if-revision` last, behind the
   degrade seam). **Do NOT** migrate `gc.routed_to` or `mcp_project_lock`, and do not ask beads for
   cross-store transactions.

**Four additions to propose upstream** (small, CAS-independent where possible): guarded unclaim
(`IfAssignee`/`IfRevision`), typed claim-conflict JSON, the `bd unclaim` lease-column hygiene fix,
and claim/lease/CAS backend conformance coverage.

---

## 9. Corrections applied from adversarial review

The synthesis draft was corrected on these load-bearing points (21 red-team findings; the
high/medium ones that changed a claim are listed). Each was re-verified against source:

1. **Proxied/server store DOES implement `ConditionalWriter`** — it's a config flag on `DoltStore`,
   not a separate store. The draft's "no `ConditionalWriter` on proxied → exit 13 on hosted" claim
   was wrong and is removed; the hosted risk is re-scoped to remote/replica configuration + HA
   failover soundness (§2.1, open Q2).
2. **The live control-plane store is SQLite, and beads CAS is Dolt-only** (`storeref.go:5`;
   `JSON_UNQUOTE` SQL). The two flagship metadata-CAS adopters (drain, `control_epoch`) are dropped
   from High to **Blocked/Low on the live fleet** and gated behind a new prerequisite (gap 4). *Most
   consequential correction.*
3. **The lease/reclaim backstop excludes wisps** (`lease.go:142`, verified verbatim). Gaps 1–3 are
   scoped to permanent issues; wisp orchestration reclaim stays 100% in GC (new gap 13). The claim
   that the backstop "retires `repairStrandedPoolWorkerBead`" is removed.
4. **`ReleaseIfCurrent` is not a byte-for-byte replacement** — `--if-revision` guards a revision
   nonce (invalidated by any write), not `status+assignee`; the true match is proposed guarded
   unclaim (gap 7). Downgraded from "clearest like-for-like/High" to contingent/semantic-change.
5. **`repairStrandedPoolWorkerBead` cannot be retired by `bd reclaim`** — it closes the session bead
   (frees the slot) at 2m; `bd reclaim` reverts only the work bead at ~15m. Self-contradiction in the
   draft fixed (§5.1).
6. **Batch heartbeat conflicts with owner-only lease semantics** and **self-heartbeat is infeasible
   for hour-scale agent steps** — reframed from a footnote to a **blocking design question** (§5.1,
   migration step 2).
7. **"Split-store" ≠ degraded** — the whole-row gate keys solely on a configured Dolt **remote**
   per DB, not topology; gascity is local-only so it rarely fires. The inverse hazard (a doomed
   `origin` remote silently disabling CAS, `ga-9wsri`) gets a doctor check (§2.1).
8. **Post-port interlock** — the v54 claim stamps a 5m lease unconditionally, so a stray `bd reclaim`
   before the heartbeat driver exists would mass-reclaim the fleet (migration step 1).
9. **Lower-severity precision fixes** folded in: drain release writes `""` not absent (breaks naive
   `--if-absent`); `control_epoch` can jump >1 and needs an observed-value guard; the `bd cas` doc
   key is `gc.drain.reserved_by` vs GC's real `gc.exclusive_drain_reservation`; the "7+ timers"
   motivation over-counts (only strand-case timers are claim-permanence artifacts); net LOC is
   ~neutral, not a 250–400 deletion; `bd unclaim` refuses unassigned/closed so the sweep keeps a
   second verb.

---

*Read surface for reproduction: beads CAS branch worktree `/tmp/beads-cas`
(`origin/cas/optimistic-concurrency`, HEAD `0980ced47`); GC deploy line
`/data/projects/beads` @ `local/deploy-current-integrated`. Workflow transcript:
`wf_f967d5df-276`.*
