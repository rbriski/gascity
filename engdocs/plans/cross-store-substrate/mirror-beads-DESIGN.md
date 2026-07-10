# Mirror Beads: Cross-Store Blocking Dependencies That Preserve `ready`

**Status:** Design — ready for implementation
**Depends on:** cross-db resolution substrate (`engdocs/plans/metacity-federation/federation-architecture.md`): resolver `id → AuthorityID → owning CityRuntime`, in-process referral reads, request-bead write-through.
**Origin:** Group E recommendation, `engdocs/contributors/graph-store-split-audit.md` §Group E — "cross-leg blockers become a graph-resident proxy/gate bead the controller releases on blocker closure … never a raw cross-leg row."

## Problem

bd's `ready` is a denormalized `is_blocked` column, recomputed by INNER JOINs
on `depends_on_issue_id`/`depends_on_wisp_id`
(`beads:internal/storage/issueops/blocked_state.go:141`). A cross-prefix
dependency target is auto-classified to `depends_on_external`
(`beads:internal/storage/domain/db/dependency.go:49`), which is **never
joined** — so a cross-db `blocks` edge is silently non-blocking. The dependent
goes false-ready and gets dispatched while its blocker is still open in
another store.

Two fixes are rejected up front:

- **Ref-by-id** (metadata pointer instead of a dep row): breaks dep/ready
  semantics entirely — nothing gates dispatch. Already rejected for blocking
  edges; it survives only for non-blocking `tracks`
  (`gc.tracking_convoy_id`, `internal/beadmeta/keys.go:173`).
- **Live-resolving the foreign blocker at read time**: re-couples one leg's
  readiness to another store's availability — the exact coupling the
  graph-store split exists to remove. §6a takes this position formally.

The fix: a **mirror bead** — a real, local, ready-excluded issue row that
stands in for the remote blocker, kept in sync by the controller.

## 1. Model

A mirror bead is a normal bead created in the **dependent's** store:

| Field | Value | Why |
|---|---|---|
| `ID` | assigned by the dependent store's `Create` → carries the dependent db's **own prefix** | The edge must bind to `depends_on_issue_id`. A foreign prefix trips the cross-prefix check (`dependency.go:49`) and silently lands in `depends_on_external` — false-ready reintroduced. Prefix correctness is load-bearing, not cosmetic. |
| `Type` | `gate` | Already ready-excluded (`internal/beads/beads.go:182`, "async wait conditions") — the mirror never appears in `bd ready` itself. Matches Group E's "gate bead" name. No new type needed. |
| `Title` | `mirror: <remote-id> — <remote title>` (set once at creation, never synced) | Debuggability only. |
| `Status` | mirrors the remote blocker: `open` while remote open; `closed` when remote closed/pinned | The **only** synced field. |
| `CloseReason` | copied from the remote on close | `conditional-blocks` semantics key off the blocker's close reason; mirroring status alone would break them. |
| `Metadata["gc.mirror_of"]` | remote bead id | Identity link + dedup key half. |
| `Metadata["gc.mirror_authority"]` | AuthorityID (rig name in-city; city id cross-city) | Resolver input for reconcile referral-reads; survives prefix-table changes. |

Both keys are registered in `internal/beadmeta` (run
`go test ./internal/beadmeta/` — the pre-commit hook skips that guard).

**The invariant:** to the local bd engine, a mirror is indistinguishable from
any local blocker. `DepAdd`, FK validation (`fk_dep_issue_target`, migration
0041), `RecomputeIsBlockedInTx`, and the `is_blocked` joins all operate
unchanged. Zero bd query-path changes.

**What it is NOT:** not a ref-by-id pointer resolved at read time; not a
foreign read on the ready path; not a replica of the remote bead (one field
plus an identity link — title/description/labels are never synced).

## 2. Readiness integration

Creation order is the whole trick: **create the mirror row first, then
`DepAdd(dependent, mirror, "blocks")`**. The target is same-prefix and exists,
so bd's classifier picks `depends_on_issue_id` (`dependency.go:49`'s
cross-prefix branch doesn't fire; the wisp probe returns `ErrNoRows` → issue
column; the FK is satisfied). From that point readiness is 100% local:
`is_blocked` recompute joins the mirror row like any other blocker, and
`bd ready` never leaves the local db.

**Creation-time guard (fail loud):** wrap the blocking-edge writer so that a
blocking dep (`beads.IsReadyBlockingDependencyType`, `beads.go:189`) whose
target prefix differs from the dependent's store prefix is an **error**, never
a raw `DepAdd`:

```go
// ensureBlockingDependencyVia: same-prefix → DepAdd as today
// (internal/dispatch/control.go:284). Foreign-prefix → ensureMirror +
// DepAdd(dependent, mirrorID). Foreign prefix with no resolvable owner
// (storeref.PrefixOwner == nil, internal/storeref/storeref.go:31) → error.
```

Prefix disjointness is already enforced city-wide
(`internal/config/reserved_prefixes.go:63` `ValidateReservedPrefixesIn`), so
"prefix differs from my store" is a sound cross-store detector. Belt and
braces: after `DepAdd`, assert via `DepList` that the edge did not land as
external; if it did, fail the wiring operation. A silently-external blocking
edge is the one bug class this design exists to kill.

## 3. Sync engine

Sync moves **one field** (status + close reason). Everything else follows for
free: closing the mirror through the normal bd close path triggers
`RecomputeIsBlockedInTx` for its dependents, and bd's recompute already
self-assigns `updated_at` to suppress `ON UPDATE CURRENT_TIMESTAMP`
(`blocked_state.go:134` — derived-state flips must not plant per-clone wall
clocks; that discipline caused Dolt merge conflicts once, bd-578h9.19).

### Remote → mirror (the release path)

Runs in the **controller** (SDK self-sufficiency: no user role involved).

- **Event-driven fast path:** controller subscribes to `bead.closed` /
  `bead.reopened` / `bead.updated` on the event bus. On an event for id `X`,
  it consults an in-memory index `remote-id → []mirror` (hydrated at startup
  by `ListByMetadata({"gc.mirror_of": ...})` per store for open mirrors,
  maintained on mirror create/close) and applies the transition: remote
  closed → close mirror with the remote's close reason; remote reopened →
  reopen mirror.
- **Catch-up reconcile (per controller tick):** events can be missed. Each
  tick, for every **open** mirror, referral-read the remote's status through
  the resolver (in-process function call to the peer store handle in v1 — no
  network, no bd daemon) and close if the remote is done. For **closed**
  mirrors that still have open dependents, verify the remote is still closed;
  if it reopened, reopen the mirror. This is the only place a foreign read
  happens, and it is on the controller's write path, never the ready path.

**Write discipline:** the sync writer only calls Close/Reopen on the mirror,
only when the status actually differs (idempotent no-op otherwise — no Dolt
merge churn), and never hand-writes `is_blocked` or any other field.

**Reopen propagation:** remote reopen → mirror reopens → bd recompute marks
dependents blocked again. If the dependent was dispatched inside the
event-latency window, that is the same staleness class as any distributed
close/reopen race — accepted, see §8.

**Source of truth is the remote.** Manually closing a mirror is not a
supported override; reconcile will reopen it while the remote is open. The
operator escape hatch for "waive this dependency" is removing the dep edge,
not lying about the blocker's status.

### Mirror → remote (the write-through path)

The mirror is locally read-only apart from controller sync. A local actor that
wants to *act on* the remote blocker (cancel, comment, reprioritize) enqueues
a **request bead** into the owner's store via the resolver; the owner's
controller applies it (single-writer, home-CAS — per the federation design).
The mirror's `gc.mirror_of` + `gc.mirror_authority` are exactly the addressing
information the request bead needs. No direct foreign writes, ever.

## 4. Lifecycle

- **Creator:** whichever code path wires a cross-store blocking edge. Today
  that is the drain projection (`ensureDrainRowDependencyProjection`,
  `internal/dispatch/drain.go:543`) and `ensureBlockingDependency`
  (`internal/dispatch/control.go:284`) when the blocker's owning store
  (`drainMemberOwningStore` / `storeref.PrefixOwner`) ≠ the dependent's
  store; later, any cross-rig sling wiring. All route through
  `ensureBlockingDependencyVia` (§2).
- **Creation-time status:** referral-read the remote once at creation. If the
  remote is already closed, create the mirror already-closed (with its close
  reason) so the dependent is never spuriously blocked. Creation is a
  write-path operation; a foreign read here is fine.
- **Dedup:** key = `(gc.mirror_of, local store)`. Guard with
  `ListByMetadata` before create — same pattern as `ensureDrainUnitConvoy`
  (`drain.go:857`). Under a concurrency race, two mirrors may both be
  created; both are correct blockers that sync identically, so nothing
  breaks — the reconcile pass detects duplicates, repoints edges to the
  oldest, and closes the extras. Convergence over locking, consistent with
  the SDK's idempotent-observer model.
- **Close, never delete:** `fk_dep_issue_target … ON DELETE CASCADE`
  (migration 0041) means deleting a mirror silently deletes its dep rows and
  the dependent goes false-ready. Mirrors are closed and left in place.
- **Orphan cleanup:** reconcile closes (reason `orphaned`) any mirror whose
  dependents are all closed or whose blocking edges are gone. A mirror whose
  authority no longer resolves (rig removed from `city.toml`) stays **open =
  blocking** and emits a `gc.mirror.unresolvable` event — fail-closed per
  §6b; a human or order releases it deliberately.

## 5. Cross-rig convoy walkthrough

Convoy with members in two work rigs: `r1-42` (rig-1 store, prefix `r1-`) and
`r2-77` (rig-2 store, prefix `r2-`), where `r2-77` must not start until
`r1-42` lands.

1. **Wiring.** Drain projects the manifest edge `r2-77 blocks-on r1-42`.
   `ensureBlockingDependencyVia` sees blocker prefix `r1-` ≠ dependent store
   prefix `r2-` → resolves rig-1 as the authority, referral-reads `r1-42`
   (open), creates mirror `r2-90x` in the rig-2 store (`type=gate`,
   `gc.mirror_of=r1-42`, `gc.mirror_authority=rig-1`), then
   `DepAdd(r2-77, r2-90x, "blocks")`. bd classifies to
   `depends_on_issue_id`, recompute sets `r2-77.is_blocked=1`.
2. **Steady state.** `bd ready` in rig-2 excludes `r2-77` (blocked) and
   `r2-90x` (gate type). No process touches rig-1's db to answer that.
   Rig-1's Dolt can be down; rig-2 readiness is unaffected.
3. **Release.** A rig-1 worker closes `r1-42`. The controller receives
   `bead.closed`, hits the mirror index, closes `r2-90x` in rig-2 with
   `r1-42`'s close reason. `RecomputeIsBlockedInTx` flips
   `r2-77.is_blocked=0` — it appears in rig-2's `bd ready` and dispatch picks
   it up. If the event is lost, the next reconcile tick's referral-read
   closes the mirror instead.
4. **Convoy bookkeeping** (`tracks`, non-blocking) stays on the existing
   ref-by-id path (`gc.tracking_convoy_id`) — the division of labor is
   crisp: *blocking* semantics get mirrors; *tracking* semantics get refs.

## 6. The two open sign-offs — resolved

**(a) Mirror bead vs teaching `Ready` to resolve foreign targets: mirror
bead.** Teaching `Ready` to resolve means every readiness evaluation on the
graph leg can fan out to N rig Dolt databases (later: N cities over a
network). That re-couples the leg's availability and latency to every remote
store — precisely the coupling the graph-store split removed — and it fights
bd's own design, where `ready` is a denormalized column, not a query-time
computation (`blocked_state.go:141`). The mirror keeps readiness a local
column read, works identically across sqlite/dolt/MemStore, and confines
foreign I/O to the controller's write path where retries and backoff are
cheap. Tradeoff: a bounded staleness window and a sync loop to own — accepted
(§8).

**(b) "Silently inert" vs "permanently blocking" for an unresolvable
same-city foreign-prefix blocker: fail-closed — it BLOCKS until a mirror
exists.** False-ready dispatches work whose preconditions aren't met and
silently corrupts orchestration; false-blocked is visible, diagnosable, and
converges once a mirror is wired. This also ends the MemStore/bd divergence
in the right direction: MemStore already fails closed
(`internal/beads/memstore.go:312` — an absent blocker's status `"" != "closed"`
→ blocked), and bd is the outlier. Enforcement, in order:

1. **Now (no bd change):** the §2 creation guard makes it impossible for Gas
   City to write a blocking edge that lands in `depends_on_external`. New
   edges are mirror-backed or they error.
2. **Reconcile sweep:** detect pre-existing blocking-type
   `depends_on_external` rows in each store, emit a warning event, and
   auto-convert them to mirror-backed edges (create mirror, add local edge,
   drop the external row).
3. **bd substrate hardening (phase 3 ask):** blocking dep types on
   `depends_on_external` count as blocking in the `is_blocked` recompute
   (opt-in flag, default on for gc-managed stores). "Permanently blocking"
   is only a footgun when there is no release mechanism; with the reconcile
   auto-converting to mirrors, blocked rows unblock legitimately.

## 7. Phasing

- **v1 — co-located cross-rig, drain site only.** `ensureBlockingDependencyVia`
  + mirror creation in `ensureDrainRowDependencyProjection` /
  `ensureBlockingDependency`; controller sync (event fast path + tick
  reconcile) over the existing `rigStores` map
  (`buildStandaloneRigStores`) with direct in-process store-handle reads.
  Needs **nothing** from the bd substrate — plain `Create`/`DepAdd`/`Close`
  and the existing event bus.
- **v2 — all creation sites + hygiene.** Cross-rig sling wiring, API/CLI
  `DepAdd` surfaces routed through the guard; reconcile auto-conversion of
  stray external blocking deps; duplicate-mirror collapse; orphan cleanup;
  `gc.mirror.unresolvable` event. Needs from bd: nothing new (optionally the
  §6b fail-closed flag lands here).
- **v3 — cross-city.** Authority resolution goes through the federation
  resolver (`id → AuthorityID → CityRuntime`); reconcile referral-reads and
  creation-time reads use the referral API; mirror→remote actions use
  request-bead write-through. Needs from bd/federation substrate: the
  referral read call and the request-bead apply loop (designed, not built) —
  the mirror layer's shape does not change, only its resolver and transport.

## 8. Open questions / risks

- **Staleness window.** Missed close event → dependent stays blocked up to
  one reconcile tick (fail-closed, acceptable). Missed *reopen* event →
  false-ready until the closed-mirror re-verify pass runs; that pass is the
  price of reopen safety and must scan closed mirrors with open dependents,
  bounded by an age cutoff. Tick period is the knob.
- **Sync races.** Remote closes between creation-time read and event
  subscription → caught by first reconcile. Reopen racing dispatch → work
  may start once against a reopened blocker; same exposure as any
  distributed close/reopen, mitigated by tick frequency, not eliminated.
- **Orphan mirrors.** Reconcile cleanup is best-effort; a store with many
  dead mirrors adds tick cost. Bounded by close-not-delete plus the
  age-cutoff scan; revisit if mirror counts grow past ~10³ per store.
- **Dedup under concurrency.** ListByMetadata-then-Create is not atomic;
  duplicates are functionally harmless and collapsed by reconcile, but the
  collapse repoints edges — must be idempotent and crash-safe (repoint before
  close).
- **MemStore/bd divergence** persists for non-mirrored external deps until
  the §6b bd flag ships; until then the creation guard is the only fence —
  any blocking-edge writer that bypasses it reintroduces false-ready. Add a
  store-sweep assertion to the audit tests.
- **Referral-read auth (v3).** Cross-city reconcile reads cross a trust
  boundary; inherits the federation design's open auth questions. Not a v1/v2
  concern (in-process, one supervisor).
