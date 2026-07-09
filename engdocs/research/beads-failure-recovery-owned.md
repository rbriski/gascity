# If we own beads: does that change the failure-unclaim → lease recommendation?

> **Status**: Decision synthesis (2026-07-09). Follow-up to
> `beads-claims-cas-adoption.md`, under the assumption **we own beads and can
> make any change**. The narrow question: *replace GC's failure-driven unclaim
> flows with leases (or a better beads primitive)?*
> **Method**: Opus grounding → 3 Fable candidate architectures (holder-principal
> lease A / extend-v54 B / event-sourced atomic reclaim C), cross-judged → Fable
> synthesis → 3 Fable red-team lenses (24 findings). This document is the
> **reconciled** result: the synthesis chose Design C, but the red-team
> demonstrated it was oversold; the load-bearing corrections are folded in and
> the bottom line is pulled back accordingly. Red-team claims marked ✓ were
> re-verified against source.

---

## 1. The answer

**Owning beads changes the *tactics*, not the *recommendation*.**

- **The division of labor is unchanged** — and every one of the nine agents
  converged on it: **GC keeps liveness authority, the fast path, and every
  judgment; beads is at most the atomic *execution* layer for a decision GC has
  already made.** Leases (claim-TTL + heartbeat + reclaim) still **cannot
  replace** the failure-unclaim flows. The blockers that mattered in the prior
  report — agent steps run silent 30 min–8 h (adopt-pr ~8 h), any
  supervisor-driven heartbeat is a strictly lossier re-projection of the
  session-liveness signal GC already acts on *faster* (2 m/90 s vs 5 m TTL +
  2×TTL grace), and "no status files — query live state" — are **physics and
  principles, not upstream-politeness**. Owning the code doesn't move them.

- **What ownership genuinely unblocks is incremental, not architectural.** Two
  things the prior report could only *propose to upstream* become buildable by
  us, plus one genuinely new primitive:
  1. **An atomic guarded release** (prior gap 7): `UnclaimIssueInTx` /
     `UpdateIssueInTx` with an `IfAssignee` (and/or `IfFence`) guard folded into
     the `WHERE`. This closes the one real bug the flows carry today — the
     unguarded `ReleaseWorkBead` read-recheck-then-clear TOCTOU
     (`work_assignment.go:134`, a plain `store.Update`) that can stomp a fresh
     re-claim. **~dozens of beads LOC. This is the whole win.**
  2. **Typed reclaim events** (prior gap 8): `work_reclaimed` through
     `HookFiringStore`, enabling dispatch-on-reclaim and dropping the
     string-matched conflict brittleness. A *feature* with its own cost, not a
     deletion.
  3. **A per-bead fence token** (`claim_fence`, monotonic, bumped on ownership
     transitions): genuinely new — but, per §3, **defense-in-depth, not a
     guarantee**.

**So the honest verdict:** the prior report's conclusion stands. Ownership means
we *build* the guarded release + typed events we previously could only ask for,
and we *may* add a fence. That is unblocking, not a change of direction. Anyone
describing this as "beads now owns failure recovery" or "leases replace the
sweeps" is overselling it.

**Recommended scope:** ship the **minimal slice** — atomic guarded release +
`claim_fence` bound to *authenticated holder identity* — and **do not** build the
holder-scoped bulk verb, the `ReclaimPatch` DSL, the lease/TTL reaper, or the v54
lease-stack port. Each of those was shown (below) to add net complexity or a
regression that ownership does not justify.

---

## 2. Which prior constraints dissolve under ownership — and which don't

**Dissolve (become schedulable work, no longer negotiations):**

| Prior constraint | Under ownership |
| --- | --- |
| No guarded unclaim (gap 7 was contingent on upstream) | We build it — per-bead `IfAssignee`/`IfFence`. |
| Typed errors/events (gap 8) | We build `ErrNotHolder`/`ErrFenceSuperseded` + `work_reclaimed`. |
| Dolt-only conditional writes; live control plane is `graph_store=sqlite` | We implement the guard SQL in the sqlite/doltlite dialect ourselves — **but see §3 finding: this must precede any flow swap, not ride alongside it, or the flagship deployment silently gets zero fencing.** |
| Wisp exclusion of *autonomous* reclaim | Irrelevant for GC-*triggered* reclaim: GC already reverts wisps in place today; a guarded release is tier-complete via `WispTableRouting`. |

**Do NOT dissolve (ownership-invariant):**

1. **Layering / ZERO hardcoded roles.** Beads is Layer 1; sessions/pools/slots
   are Layer 0 (Agent) concepts. A holder must be an opaque assignee string; a
   reclaim patch must be opaque caller data. Owning the repo licenses no upward
   dependency.
2. **Heartbeat-feasibility physics.** No renewal scheme survives 30 min–8 h
   silent agent steps, and any supervisor renewal is a lossier copy of GC's
   session probe. This is why **Design A's principal-lease and Design B's renew
   tick were both rejected regardless of ownership.**
3. **Query-live-state.** A heartbeat lease is a written liveness marker — the
   opposite of the principle; its only legitimate niche is the un-queryable
   crashed-host case, which (§3) is nearly empty in practice.
4. **What beads can never own:** freeing the pool slot (closing the session
   bead), worktree prune, run_target/affinity *policy*, the `snapshotQueryPartial`
   degraded-read posture, the 2 m stranded-repair grace, detached-executor
   probing, cross-store iteration, and the wedged-worker-in-a-live-session case
   (undetectable by any lease or fence — stays Health Patrol's job).

---

## 3. Why the bulk verb, the reaper, and the v54 port are rejected (red-team, verified)

The synthesis's Design C proposed a holder-scoped atomic **bulk-reclaim** verb, a
`ReclaimPatch` micro-DSL, an opt-in **TTL reaper** on a ported **v54 lease
stack**, and sequenced the v54 port first. The red-team dismantled each; the
decisive, source-verified findings:

- **The fence is not a capability — it's world-readable state presented
  *behaviorally* by an LLM.** A resumed/compacted worker either omits `--fence`
  (advisory mode → the write lands unguarded) or runs `bd show` and quotes the
  *current* fence, defeating fencing while looking compliant. **A fence must bind
  to authenticated holder identity server-side (`IfAssignee` with an
  incarnation-unique canonical session ID), not to a caller-quoted number.** Fence
  is defense-in-depth. *(GC sessions support resume + crash adoption — this is a
  designed path.)*
- **✓ A fence on status/assignee does not close the corruption class.** GC's
  control state is substantially *metadata* (`gc.outcome`, routing keys,
  `step_ref`) and graph edges. Verified: `update.go:221` is
  `UPDATE … SET … WHERE id = ?` — metadata writes carry no guard, and
  `lease.go:58` explicitly **exempts orthogonal cells (metadata, deps, reopen)**
  from the `row_lock` conflict guard, so a zombie's `bd update --meta
  gc.outcome=fail` cell-merges silently. Require-mode fencing must cover *any
  mutation of a claimed bead*, enumerated from `issueops/`, not four verb names.
- **Fencing protects only the store; GC workers' primary outputs are external
  side effects** (git pushes, PR creation). A reclaimed-but-still-live worker
  keeps executing → the reopened bead dispatches to a second worker → duplicate
  PRs/merges (the logged retry-treadmill class). **Duplicate-execution safety is
  a separate, unsolved problem the fence does not touch.**
- **The resumed-same-holder race is not closed "by construction."** Crash
  adoption between GC's 2 m non-liveness confirmation and the verb presents an
  identical assignee+fence+status → the guarded `WHERE` matches → the verb
  reverts a *live* worker. Deleting the adjacent liveness re-read
  (`pool_session_name.go:197`) removes the mitigation. Keep a cheap recheck, or
  make adoption itself a fenced transition.
- **✓ Per-store "atomic-or-error" is an availability regression.** Today one
  poisoned row fails alone (`Failed=1`) while the rest release, and
  `repairStrandedPoolWorkerBead` closes the session bead only on `Failed==0`; an
  all-or-nothing bulk verb wedges pool-slot recovery *permanently* on one bad
  row, and on Dolt a racing claim on any row forces a `1213` whole-batch replay
  (livelock surface). To avoid this the verb must commit partial progress and
  return per-row outcomes — i.e., reproduce today's semantics inside beads, at
  which point the "win" is one round-trip, not a simpler failure model.
- **The `ReclaimPatch` DSL can't express the flow it claims to absorb.**
  `releaseOrphanedPoolAssignment` applies a *per-bead* patch (`clearDetached`
  varies per row); a per-call patch forces either GC-side batch-splitting or the
  third DSL shape the design swears would fail the primitive test. Simpler:
  fenced revert only; GC applies its own metadata patch in a follow-up
  `IfFence`-guarded update (a racing fresh claim makes the patch a correct no-op).
- **✓ The v54 lease substrate writes stale markers from day one.** Verified:
  `claim.go:48` applies `leaseSetClause` in both claim branches, so every claim
  stamps `lease_expires_at = now+TTL` with no feasible heartbeat — stale-by-design
  fleet-wide. Porting it (kept disabled forever) also **front-loads the fork's
  worst-documented failure class** (schema skew killed the control dispatcher
  twice). **Decouple:** ship one migration adding `claim_fence` + guarded verbs,
  **no lease columns, no reaper, no interlock to police**, gated on a hard
  fleet-wide `bd` version check.
- **The reaper's niche is nearly empty.** With no heartbeat the TTL must be
  12–24 h; in the crashed-host case either the co-located store is down with the
  host (reaper can't run) or the store is hosted and GC's *restart* session-death
  sweep does the identical reclaim anyway. The lease adds a redundant backstop
  that ships permanently off. Its legitimate home, if ever needed, is the
  existing `route-reclaim` order + `bd reclaim` CLI (config, not Go).
- **Clock authority was silently dropped.** Lease expiry is stamped from client
  `time.Now()`; with bd on workers, controller, and the gateway these are
  different clocks → skew mass-reclaims healthy work or makes the backstop
  vacuous. If any lease ships, stamp from DB server time. *(Moot if we drop the
  lease, per above.)*
- **Legacy unguarded `unclaim` must still bump the fence** (else a zombie's
  `close --fence 7` matches a bead reverted via the compatibility path). Encode
  "every assignee-clearing/reopen path bumps `claim_fence`" as a package-guard
  test, not an audit bullet.
- **✓ Net LOC is +400–700, not neutral.** GC nets ~−150–200; beads adds
  ~600–900 plus a new cross-system contract (verbs, wire semantics, event bridge,
  Off/Auto/Require gate + differential harness, two-dialect conformance, fleet
  fence plumbing). The flows "keep their skeletons and lose their racy mutation
  bodies." The one bug genuinely closed (`ReleaseWorkBead` TOCTOU) is obtainable
  from the minimal guarded-unclaim slice alone.

---

## 4. Recommended minimal slice

1. **`claim_fence`** — monotonic `BIGINT` on `issues` + `wisps`, bumped on every
   ownership transition (claim, reclaim, **and legacy unclaim/reopen** — guarded
   by a package test). Surfaced on `ClaimResult`/`gc hook --claim`. Additive;
   unguarded calls byte-unchanged.
2. **Atomic guarded release** — `IfAssignee(v)` / `IfFence(n)` on
   unclaim/update/close, typed `ErrNotHolder`/`ErrFenceSuperseded`. Swap
   `ReleaseWorkBead`'s unguarded `store.Update` for it behind an Off/Auto/Require
   differential gate (repo precedent: S01). **This closes the TOCTOU** and is the
   core deliverable.
3. **Fence bound to authenticated holder identity**, not a caller-quoted number;
   forbid worker templates from recovering a fence via `bd show` (re-hook
   instead). Treat `--fence` as defense-in-depth layered on `IfAssignee`.
4. **Dialect first:** implement the guard in the sqlite/doltlite dialect *before*
   swapping any control-plane flow; make the "store can't guard" fallback **loud**
   (doctor ERROR, refuse Require) so the sqlite control plane doesn't silently run
   unguarded while operators believe the class is closed.
5. **Typed `work_reclaimed` events** (optional, as a *feature*): enables
   dispatch-on-reclaim; do **not** lower sweep cadence in the same slice (the
   sweep is the convergence backstop; post-commit hooks are at-most-once until the
   events table is consumed as a replayable journal).

**Explicitly not building:** the holder-scoped bulk verb, the `ReclaimPatch`
DSL, the TTL reaper, the v54 lease-stack port, and Design A's principal
registry. Each adds net complexity or a regression ownership does not justify.
`repairStrandedPoolWorkerBead`, the orphan-sweep judgment stack, degraded-read
posture, cross-store iteration, and all routing/affinity policy **stay in GC** —
they are orchestration and cognition, which the primitive test keeps out of the
ledger.

---

## 5. Bottom line

Owning beads lets us *build* the guarded release + typed events the prior report
could only propose, and add a per-bead fence as defense-in-depth. It does **not**
let leases replace GC's failure-unclaim flows, does **not** move liveness
authority or judgment into the ledger, and does **not** make the bulk verb or the
TTL reaper worth their cost. The recommendation is the same shape as before —
**GC decides, beads (now) executes atomically** — realized as a small guarded-release
+ fence slice, with the ambitious machinery deliberately left on the table.
