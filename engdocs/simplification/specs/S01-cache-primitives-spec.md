# S01 — Beads cache: absorb/evict primitives + one merge algorithm

Backlog item: `engdocs/simplification/backlog.json` id `S01` (risk: **high**, effort L,
disposition needs-julian). Bug lineage: #2987, #2210, #2927, #3789, #2153.

Scope of THIS spec: approaches (a)+(b) from the backlog — the two locked
primitives and the collapse of `runReconciliation`'s two branches into one
per-bead guarded merge loop. Approach (c) (ApplyEvent collapse to
invalidate-or-absorb) is sequenced strictly AFTER and is only sketched in
the staging section; it does not land under this spec.

## Target design

### The six parallel maps (caching_store.go:33-39)

| Map | Meaning | Written when |
| --- | --- | --- |
| `beads map[string]Bead` | cached row | absorb / evict |
| `deps map[string][]Dep` | cached down-deps for the row | absorb / evict |
| `dirty map[string]struct{}` | row known-stale; reads must bypass cache | write-failure & event-conflict marks; cleared on absorb/evict |
| `beadSeq map[string]uint64` | last local `mutationSeq` that touched the row (staleness fence for in-flight snapshots) | `noteMutationLocked`; cleared on absorb (guarded) / evict |
| `localBeadAt map[string]time.Time` | wall-clock of last LOCAL write-through (5 s recency window, #2210 class) | `noteLocalMutationLocked`; cleared on absorb (guarded) / evict |
| `deletedSeq map[string]uint64` | tombstone: row deleted at seq N; fences resurrection by older snapshots | delete paths; cleared on absorb / evict |

Plus two scalars that interact: `mutationSeq uint64` (global monotonic write
counter) and `depsComplete bool` (whether `deps` is authoritative for ready
computation).

### The two primitives

Both REQUIRE `c.mu` held in WRITE mode (suffix `Locked`, enforced by
convention + a mutex-held assertion in `-race` test builds via
`c.mu.TryLock()` == false checks in debug-tagged tests only — no production
assertion). After this change they are the ONLY code that writes
`beads/deps/dirty/beadSeq/localBeadAt/deletedSeq` for a single row.
`noteMutationLocked`/`noteLocalMutationLocked` remain the only writers of
the seq/recency maps for the LOCAL-write direction (they are "the third
primitive" that already exists and is already shared; this spec does not
touch them). Whole-map replacement (`c.beads = nextBeads` in `prime()`
fast path) is eliminated by the branch collapse (below), leaving zero
non-primitive writers.

```go
// evictLocked removes every trace of id from the six per-row maps.
// Exactly reproduces the hand-copied 6-line cascade. It does NOT touch
// mutationSeq, depsComplete, state, or stats. NO options — all evict
// sites are byte-identical today.
func (c *CachingStore) evictLocked(id string) {
    delete(c.beads, id)
    delete(c.deps, id)
    delete(c.dirty, id)
    delete(c.deletedSeq, id)
    delete(c.beadSeq, id)
    delete(c.localBeadAt, id)
}

// tombstoneLocked is evict + a deletion fence. The three local/event
// delete sites (writes.go:82-87, :898-903, events.go:261-266) do the five
// deletes then SET deletedSeq[id]=seq; delete-then-set under the held
// lock is byte-equivalent (nothing observes intermediate state).
func (c *CachingStore) tombstoneLocked(id string, seq uint64) {
    c.evictLocked(id)
    c.deletedSeq[id] = seq
}

// absorbDepsMode: how the deps map row is sourced on absorb.
type absorbDepsMode int
const (
    depsExplicit          absorbDepsMode = iota // c.deps[id] = cloneDeps(opts.deps)
    depsFromBeadFields                          // c.deps[id] = depsFromBeadFields(bead), unconditional
    depsFromFieldsIfCarried                     // only when beadCarriesDependencyFields(bead); else keep
    depsKeepCached                              // leave c.deps[id] untouched
    depsDrop                                    // delete(c.deps, id)  (CloseAll closed-row site W11)
)

// absorbSeqMode: what happens to beadSeq/localBeadAt on absorb.
type absorbSeqMode int
const (
    seqKeep             absorbSeqMode = iota // touch neither (write/event paths: noteMutation just set them)
    seqClearGuarded                          // if !recentLocalMutation(localBeadAt[id], now): delete both
    seqClearBeadSeqOnly                      // delete(beadSeq, id) unconditionally; localBeadAt untouched
)

type absorbOpts struct {
    depsMode absorbDepsMode
    deps     []Dep // depsExplicit only
    seqMode  absorbSeqMode
    // clearDirty is true at every site except A1 (PrimeActive) and A2
    // (prime slow path), which today do NOT clear the dirty mark.
    clearDirty bool
}

// absorbFreshLocked installs a fresh row for id per opts. now is the
// caller's single time.Now() for the whole merge pass (one clock read
// per pass, matching today's per-branch `now`). Always clears the
// deletedSeq tombstone (every absorb site does).
func (c *CachingStore) absorbFreshLocked(id string, bead Bead, now time.Time, opts absorbOpts) {
    c.beads[id] = cloneBead(bead)
    // deps per opts.depsMode (see table above)
    if opts.clearDirty { delete(c.dirty, id) }
    delete(c.deletedSeq, id)
    switch opts.seqMode {
    case seqClearGuarded:
        if !recentLocalMutation(c.localBeadAt[id], now) {
            delete(c.beadSeq, id); delete(c.localBeadAt, id)
        }
    case seqClearBeadSeqOnly:
        delete(c.beadSeq, id)
    }
}
```

**Why `seqKeep` for write/event paths is byte-equivalent AND required:**
write paths run `noteLocalMutationLocked` immediately before absorbing, so
`localBeadAt[id] == now` and a `seqClearGuarded` would be a no-op — but
event paths run `noteMutationLocked` (beadSeq only, NO localBeadAt), so a
guarded clear would DELETE the fence the event just set, breaking the
#2210 staleness defense. Event and write sites therefore MUST use
`seqKeep`; this is the single most dangerous silent divergence in the
whole refactor and gets a dedicated test (T7).

**Scope of the "only writers" claim.** absorb/evict/tombstone become the
only writers of ROW-LIFECYCLE state: row install/remove plus the
dirty/deletedSeq/beadSeq/localBeadAt transitions listed per-site below.
Three narrow writer classes remain OUTSIDE the primitives, unchanged:
(1) `noteMutationLocked`/`noteLocalMutationLocked` (the seq/recency
setters), (2) deps-overlay writers (`setEventDepsLocked`,
`updateEventDepsLocked`, `ApplyDepEvent`, DepAdd/DepRemove in-place
fallbacks, DepList lazy fill at reads.go:628) — they write `deps` (and
DepAdd/DepRemove fallbacks clear dirty/deletedSeq; those two clears route
through a tiny `clearStalenessMarksLocked(id)` helper so no bare deletes
remain), (3) the ready-projection overlay (`clearReadyProjectionLocked`
rewrites `beads[id].IsBlocked` in place) and dirty-marking
(`c.dirty[id] = struct{}{}` at the 5 mark sites, routed through
`markDirtyLocked(id)`). These are enumerated as non-goals; folding them
is follow-up work, not S01.

Design notes:

- **The guards stay at the call sites.** `deletedSeq[id] > startSeq`,
  `beadSeq[id] > startSeq`, and `recentLocalBeadConflictLocked` checks run
  BEFORE deciding to absorb/evict — the primitives are unconditional
  executors, not policy. This keeps them tiny and byte-auditable, and keeps
  per-site policy differences visible at the site (they differ; see the
  enumeration).
- **`absorbOpts` is deliberately narrow**: exactly the two axes of observed
  variation (deps sourcing, seq-clear aggressiveness). No third axis exists
  in the current code; do not add one speculatively.
- **Eviction is symmetric everywhere.** All ~7 evict sites are literally
  the same 6 deletes today; `evictLocked` has NO options.

### One merge algorithm (runReconciliation collapse)

Today `runReconciliation` forks on `c.mutationSeq != startSeq`:

- **Branch A (in-place, :346-451):** per-row guarded merge into the LIVE
  maps; skips rows fenced by `deletedSeq/beadSeq > startSeq` or the recency
  window; evicts cached rows absent from the snapshot (unless fenced);
  leaves `deletedSeq` entries `<= startSeq` un-GC'd only for rows absent
  from both snapshot and cache (they are deleted for present rows and
  evicted rows). Sets `depsComplete` pessimistically (`nextDepsComplete`
  can degrade to false when a fenced row has no cached deps).
- **Branch B (rebuild, :453-548):** builds `next*` maps from scratch;
  carries recent-local rows forward; wholesale-resets `dirty/beadSeq/
  localBeadAt/deletedSeq` (implicit GC of everything not carried).

The collapse: **always run Branch A's per-row guarded loop** (the guards
degrade to no-ops when `mutationSeq == startSeq`, because no fence value
can exceed `startSeq` without a concurrent mutation), **plus an explicit
`deletedSeq` GC** that Branch B performed implicitly:

```
for id, seq := range c.deletedSeq {
    if seq <= startSeq { delete(c.deletedSeq, id) }
}
```

run at the END of the merge pass (after absorb/evict of all rows), which is
strictly more precise than Branch B's wholesale reset (B resets even
`> startSeq` tombstones — see Divergence D1 in the invariants section; the
collapse fixes a latent B bug rather than reproducing it, and this is the
one INTENTIONAL behavior delta, gated by Julian sign-off).

The diff decision itself is extracted as a **pure function**:

```go
type mergeDecision int // mergeSkipFenced | mergeSkipRecentLocal | mergeAbsorb
type mergeOutcome struct {
    decision     mergeDecision
    notification string // "", "bead.created", "bead.updated"
}
// pure: no locks, no maps mutated, no clock reads (now passed in)
func reconcileMergeDecision(id string, fresh Bead, freshDeps []Dep,
    cached Bead, cachedExists bool, cachedDeps []Dep,
    deletedAtSeq, beadAtSeq, startSeq uint64,
    localAt time.Time, now time.Time) mergeOutcome
```

`prime()` and `PrimeActive` re-expression over the same loop is IN scope
for approach (b) per the backlog, but staged as its own phase (Phase 3)
with its own parity gate, because their guard shapes differ (PrimeActive
uses `keepCached`-only absorption with no eviction pass; prime's fast path
today rebuilds). Until Phase 3 lands they keep their current algorithms but
route their per-row map writes through the primitives (Phase 1).

## Current behavior (site-by-site enumeration)

Line numbers are against worktree `simplification` HEAD (f6a0fbd5f). Every
site lists: guards evaluated before the mutation, exact map writes in
source order, and the primitive+opts that reproduces it. "guarded
seq-clear" = `if !recentLocalMutation(c.localBeadAt[id], now) { delete(beadSeq); delete(localBeadAt) }`.

### Evict / tombstone sites (7)

**E1 — reconcile Branch A missing-row eviction** (`caching_store_reconcile.go:400-428`)
- Guards (in order): row present in `c.beads` but absent from `freshByID`;
  skip if `deletedSeq[id] > startSeq || beadSeq[id] > startSeq`; skip if
  `old.Status != "closed" && recentLocalMutation(localBeadAt[id], now)`.
- Mutations: `removes++`; synthesize `bead.closed` notification when
  `old.Status != "closed"` (payload = `confirmedClosed[id]` if
  recoverMissingFromList confirmed, else cached row with Status flipped);
  then the 6 deletes: beads, deps, dirty, deletedSeq, beadSeq, localBeadAt.
- New form: guards + notification at site; `evictLocked(id)`. Byte-identical.

**E2 — refreshCachedBeads removedParents** (`caching_store_reads.go:265-278`)
- Guards: `deletedSeq[id] > startSeq || beadSeq[id] > startSeq` → skip;
  cached row exists ∧ `Status != "closed"` ∧ recent-local → skip.
- Mutations: same 6 deletes, same order. No notification.
- New form: `evictLocked(id)`. Byte-identical.

**E3 — refreshCachedBeads removedLiveMissing** (`caching_store_reads.go:297-310`)
- Identical guards and deletes to E2. New form: `evictLocked(id)`.

**E4 — Update → post-write Get returns ErrNotFound** (`caching_store_writes.go:72-95`)
- Guards: none (backing said the row is gone after our own write).
- Mutations: `seq := noteLocalMutationLocked(id)` FIRST (sets beadSeq +
  localBeadAt, bumps mutationSeq); synth close notification if cached row
  was non-closed; then 5 deletes (beads, deps, dirty, beadSeq,
  localBeadAt) + `deletedSeq[id] = seq`; then
  `clearDependentReadyProjectionsLocked(id)`, markFresh, stats.
- New form: `tombstoneLocked(id, seq)` after the note call. The note call
  sets beadSeq/localBeadAt then tombstone deletes them — preserved
  verbatim (net effect: only the deletedSeq fence survives; mutationSeq
  bump is what makes the fence > any in-flight startSeq).

**E5 — Delete** (`caching_store_writes.go:896-907`)
- Same shape as E4 (note → 5 deletes + `deletedSeq[id]=seq` →
  clearDependentReadyProjections → markFresh/stats). Notification
  `bead.deleted` AFTER unlock using a backing snapshot taken BEFORE the
  backing delete. New form: `tombstoneLocked(id, seq)`.

**E6 — ApplyEvent bead.deleted** (`caching_store_events.go:259-271`)
- Guards: full ApplyEvent front-half (ownership, state, conflict lattice —
  untouched by this spec).
- Mutations: `noteMutationLocked(b.ID)` (beadSeq only, NO localBeadAt);
  5 deletes + `deletedSeq[b.ID] = c.mutationSeq`; stats;
  clearDependentReadyProjections.
- New form: `tombstoneLocked(b.ID, c.mutationSeq)`. Note the seq source is
  `c.mutationSeq` (== the value noteMutationLocked just returned) —
  identical fence value to E4/E5's `seq`.

**E7 — reconcile Branch B implicit eviction** (`caching_store_reconcile.go:496-528`)
- Not a delete cascade: missing rows are simply not copied into the
  `next*` maps, and `dirty/beadSeq/localBeadAt/deletedSeq` are wholesale
  replaced. Eliminated by the branch collapse (Phase 2); its semantics are
  proven equivalent to E1 + the two GC passes in the invariants section.

### Snapshot-merge absorb sites (5)

**A1 — PrimeActive per-row merge** (`caching_store.go:411-437`)
- Guards: only when `mutationSeq != startSeq`: skip if
  `deletedSeq[b.ID] > startSeq`, skip if row already in `c.beads`
  (PrimeActive never overwrites an existing row under concurrent
  mutation); ALWAYS: skip if `recentLocalBeadConflictLocked(b.ID, b, now,
  skipLabels=false)` keeps.
- Mutations: `beads[id]=cloneBead(b)`; deps = `cloneDeps(depMap[id])` when
  `depsComplete && depErr == nil` else `depsFromBeadFields(b)`;
  `delete(deletedSeq, id)`; guarded seq-clear. Does NOT touch `dirty`.
  No notifications (PrimeActive never notifies).
- New form: `absorbFreshLocked(id, b, now, {depsMode: depsExplicit|depsFromBeadFields per
  the same condition, seqMode: seqClearGuarded, clearDirty: false})`.
- ⚠ `clearDirty:false` is load-bearing: a dirty mark set before PrimeActive
  must survive it, because PrimeActive's row may already be staler than the
  write that set the mark (cache goes `cachePartial`, dirty forces backing
  reads until reconcile). Harmonizing this is NOT allowed in S01.

**A2 — prime() slow path (concurrent-mutation branch)** (`caching_store.go:571-589`)
- Guards: `deletedSeq[id] > startSeq` → skip; row already in `c.beads` → skip
  (add-only, like A1).
- Mutations: `beads[id] = b` (b already cloned into beadMap at :523-526 —
  no second clone; primitive's cloneBead adds one defensive clone, see
  Parity note P1); `delete(deletedSeq, id)`; `delete(beadSeq, id)`
  UNCONDITIONALLY; deps = `cloneDeps(depMap[id])` or
  `depsFromBeadFields(b)`; does NOT touch `dirty` or `localBeadAt`; then
  `depsComplete = false` for the whole cache (outside the loop).
- New form: `absorbFreshLocked(id, b, now, {depsMode as A1, seqMode:
  seqClearBeadSeqOnly, clearDirty: false})`.
- ⚠ differs from A1 in BOTH remaining axes; do not merge A1/A2 call shapes.

**A3 — prime() fast path (rebuild)** (`caching_store.go:539-570`)
- Whole-map replacement: `next*` maps built from the fresh snapshot;
  recent-local conflicting rows carried via
  `recentLocalBeadConflictLocked(skipLabels=true)` +
  `carryRecentLocalMutationLocked`; non-closed recent-local cached rows
  absent from the snapshot carried likewise; `deletedSeq` reset to empty.
  No notifications (prime never notifies).
- Phase 1: unchanged (whole-map replacement is not a per-row writer).
  Phase 3 re-expresses prime over the shared merge loop; parity gate
  required because prime's carry uses `skipLabels=true` while A1 uses
  `false` (labels-only drift on a recent-local row: prime keeps the FRESH
  row where PrimeActive would keep the CACHED one — enumerate in the
  differential test corpus).

**A4 — reconcile Branch A per-row absorb** (`caching_store_reconcile.go:351-398`)
- Guards (exact order): `deletedSeq[id] > startSeq || beadSeq[id] >
  startSeq` → skip (and if the fenced row exists but has no deps entry,
  degrade `nextDepsComplete=false`); `recentLocalBeadConflictLocked(id,
  freshBead, now, skipLabels=true)` keeps → skip (same depsComplete
  degradation).
- Mutations: compute `freshDeps = depsForReconcileLocked(...)`; classify
  notification BEFORE mutating (`!exists` → bead.created;
  `beadChanged(old, fresh, skipLabels=true)` → bead.updated;
  `depsChanged(c.deps[id], freshDeps)` → bead.updated); then
  `beads[id]=cloneBead(freshBead)`, `deps[id]=cloneDeps(freshDeps)`,
  `delete(dirty)`, `delete(deletedSeq)`, guarded seq-clear.
- New form: pure `reconcileMergeDecision(...)` for the guards+notification
  classification, then `absorbFreshLocked(id, freshBead, now, {depsExplicit
  freshDeps, seqClearGuarded, clearDirty: true})`. This is THE canonical
  merge-loop body after the collapse.

**A5 — reconcile Branch B per-row build** (`caching_store_reconcile.go:461-527`)
- Same classification as A4 with two differences: recent-local keep does
  not skip — it installs the CACHED row into `nextBeads` (carrying
  dirty/beadSeq/localBeadAt via carryRecentLocalMutationLocked) while
  still emitting bead.created if the row was uncached (unreachable: an
  uncached row can never be recent-local-kept — recentLocalBeadConflictLocked
  returns false on cache miss); and update notifications are suppressed
  for kept rows (`!preservedRecentLocal` guards). Branch A reproduces
  both: its `continue` on keep suppresses the update notification, and the
  kept row's maps are simply left in place (equivalent to carrying).
  Eliminated by the collapse; equivalence obligations in Invariants D2/D3.

### Read-path absorb sites (4)

**R1 — refreshCachedBeads items loop** (`caching_store_reads.go:210-246`)
- Guards (exact order — the order IS the semantics): `deletedSeq[item.ID]
  > startSeq` → drop item from results; `beadSeq[item.ID] > startSeq` →
  serve CACHED row instead (if it matches query); recent-local conflict
  (`skipLabels=false`) → serve cached row; `beadSeq[item.ID] == startSeq`
  ∧ cached row closed ∧ item non-closed → drop item (a just-closed row
  must not be resurrected by an equal-seq snapshot).
- Mutations on absorb: `beads[id]=cloneBead(item)`; deps ONLY
  `if beadCarriesDependencyFields(item)` → `depsFromBeadFields(item)`;
  `delete(dirty)`, `delete(deletedSeq)`, guarded seq-clear.
- New form: guards at site (including the `== startSeq` closed-row check,
  which exists NOWHERE else — do not fold it into the shared decision
  function without a dedicated test), then `absorbFreshLocked(id, item,
  now, {depsFromFieldsIfCarried, seqClearGuarded, clearDirty: true})`.

**R2 — refreshCachedBeads refreshedParents loop** (`caching_store_reads.go:247-264`)
- Guards: `deletedSeq > startSeq || beadSeq > startSeq` → skip;
  recent-local conflict (`skipLabels=false`) → skip.
- Mutations: `beads[id] = bead` — NOTE: no clone at the assignment; the
  clone happened at fetch time (`refreshedParents[id] = cloneBead(fresh)`,
  :181). The primitive's internal clone is an extra defensive copy —
  allowed (see P1). Deps/dirty/deletedSeq/seq identical to R1.
- New form: `absorbFreshLocked(id, bead, now, {depsFromFieldsIfCarried,
  seqClearGuarded, clearDirty: true})`.

**R3 — refreshCachedBeads refreshedLiveMissing loop** (`caching_store_reads.go:279-296`)
- Byte-identical to R2. Same new form.

**R4 — Get dirty-row refresh** (`caching_store_reads.go:398-436`)
- Guards (under RE-ACQUIRED write lock after unlocked backing.Get —
  #2210-style re-verification): state still live/partial;
  `deletedSeq[id] > startSeq` → return ErrNotFound;
  `beadSeq[id] > startSeq` → serve current cache state or re-Get
  (no absorb).
- Mutations on absorb: `beads[id]=cloneBead(fresh)`;
  `deps[id]=depsFromBeadFields(fresh)` UNCONDITIONALLY (even when fresh
  carries no dep fields → deps entry set to nil — distinct from R1's
  if-carried!); `delete(dirty)`, `delete(deletedSeq)`, `delete(beadSeq)`
  unconditionally; localBeadAt NOT touched; markFresh + stats.
- New form: `absorbFreshLocked(id, fresh, now, {depsFromBeadFields,
  seqClearBeadSeqOnly, clearDirty: true})`.
- ⚠ two single-site semantics here: unconditional-nil deps write and
  beadSeq-only clear. Both preserved verbatim; both get parity tests.

Also in reads but NOT an absorb site (stays outside primitives):
`DepList` lazy dep fill (`caching_store_reads.go:627-629`) writes
`c.deps[id]` only, touching no other map.

### Write-path absorb sites (all preceded by `noteLocalMutationLocked` → `seqKeep`)

Common shape: backing write succeeds → unlocked backing.Get refresh →
Lock → `noteLocalMutationLocked(id)` → absorb/fallback → markFresh/stats
→ Unlock → notify. All these sites use `seqMode: seqKeep, clearDirty:
true` unless noted. None of them checks deletedSeq/beadSeq/recency guards
— a local write ALWAYS wins (it just bumped the fences itself).

**W1 — createWith** (`caching_store_writes.go:40-48`): absorb
`{depsFromBeadFields}` — beads=clone, deps=depsFromBeadFields(created),
delete dirty, delete deletedSeq. Notify bead.created.

**W2 — Update, refresh OK** (`:119-130`): absorb `{depsFromBeadFields}` on
`applyUpdateOptsToBead(fresh, opts)`; plus
`clearDependentReadyProjectionsLocked(id)` when `opts.Status != nil`
(site-level, before/after absorb is order-insensitive — projection
clearing touches other rows' IsBlocked + noteMutation). Notify
bead.updated.

**W3 — Update, refresh failed, row cached** (`:97-111`): synthesizes
`applyUpdateOptsToBead(current, opts)`; beads=clone,
deps=depsFromBeadFields; **SETS `dirty[id]`** (row is best-effort until
verified); `delete(deletedSeq)`. New form: `absorbFreshLocked(id, synth,
now, {depsFromBeadFields, seqKeep, clearDirty:false})` +
`markDirtyLocked(id)`. (absorb never sets dirty; the mark is site policy.)

**W4 — Update, refresh failed, row NOT cached** (`:112`): `markDirtyLocked`
only. Not an absorb.

**W5 — ReleaseIfCurrent, refreshed** (`:153-159`): absorb
`{depsFromBeadFields}`; then `clearDependentReadyProjectionsLocked`
(unconditional at :172).

**W6 — ReleaseIfCurrent, fallback on cached row** (`:160-168`): local
synth (Status=open, Assignee="", UpdatedAt=now); beads=b (no clone —
map-read copy is already a value copy; primitive clone is defensive-extra,
P1); **no deps write** (depsKeepCached); **SETS dirty**;
delete(deletedSeq). New form: absorb `{depsKeepCached, seqKeep,
clearDirty:false}` + markDirty.

**W7 — ReleaseIfCurrent, row unknown** (`:170`): markDirty only.

**W8/W9 — Close** (`:205-224`): cached row → flip Status=closed in the
value copy, beads=..., delete dirty, delete deletedSeq, **no deps write**
(depsKeepCached — the closed row's deps entry survives; contrast W11!);
uncached-but-backing-found → absorb clone of backing row with
Status=closed, same opts. Then clearDependentReadyProjections; markFresh
+ stats only if `found || dependentProjectionCleared`.

**W10 — Reopen** (`:248-267`): mirror of W8/W9 with Status=open and
bead.updated notification.

**W11 — CloseAll refreshed loop** (`:308-323`): per row: absorb
beads=clone(fresh), delete dirty, delete deletedSeq; if
`item.bead.Status == "closed"` ALSO `delete(c.deps, item.id)` +
clearDependentReadyProjections. New form: absorb `{depsMode:
fresh.Status=="closed" ? depsDrop : depsKeepCached, seqKeep,
clearDirty:true}`. ⚠ CloseAll DROPS deps where Close KEEPS them —
preserve; do not harmonize (Ready correctness tolerates both since closed
rows are never ready candidates, but depsComplete/CachedReady coverage
checks observe the difference).

**W12 — CloseAll refresh-failed rows** (`:305-307`): markDirty each.

**W13/W14 — SetMetadata / SetMetadataBatch** (`:352-376`, `:404-430`):
refreshed → absorb `{depsFromBeadFields}`; fallback on cached row →
mutate metadata in the value copy, beads=..., delete dirty, delete
deletedSeq (**clearDirty:true here, unlike W3/W6** — metadata-merge
fallback is trusted), depsKeepCached; unknown row → markDirty.

**W15 — refreshTxTouchedBeads** (`:576-629`): per touched id:
found → absorb `{depsFromBeadFields}` (+ conditional
clearDependentReadyProjections on status change);
closed-fallback on cached row → flip Status=closed, absorb
`{depsKeepCached}`; else if Get errored → markDirty.

**W16/W17 — DepAdd / DepRemove** (`:787-887`): refreshed → absorb
`{depsExplicit deps}` + clearReadyProjection; fallback paths are
deps-overlay writers (in-place mutate `c.deps[issueID]`) that also clear
dirty+deletedSeq → route those two deletes through
`clearStalenessMarksLocked(id)` (shared with absorb internals), leave the
deps mutation at site.

### Event-path absorb sites (3, all `seqKeep` — see Target design warning)

**EV1 — bead.created** (`caching_store_events.go:217-229`): if row not
cached: `noteMutationLocked`; beads=clone; `updateEventDepsLocked(...)`
(deps-overlay, stays at site — it is NOT expressible as a depsMode
because it can flip global depsComplete); delete dirty; delete
deletedSeq. New form: absorb `{depsKeepCached, seqKeep, clearDirty:true}`
+ site keeps updateEventDepsLocked BEFORE... — NOTE order today: beads
write happens BEFORE updateEventDepsLocked. updateEventDepsLocked reads
`c.deps[b.ID]` only; c.beads order-insensitive EXCEPT
`clearReadyProjectionLocked` (called inside setEventDepsLocked) reads AND
writes `c.beads[id]` — it must see the NEW row. Preserve exact order:
absorb first, then updateEventDepsLocked. LOCK-STEP ORDER CONTRACT OC-3.

**EV2 — bead.updated** (`:230-245`): if uncached or
`beadChanged(existing, b, false)`: noteMutation + beads=clone + delete
dirty + delete deletedSeq (absorb `{depsKeepCached, seqKeep}`); then
updateEventDepsLocked (may independently noteMutation);
then conditional clearDependentReadyProjections on "status" field.

**EV3 — bead.closed** (`:246-258`): unconditional noteMutation +
beads=clone + updateEventDeps + delete dirty + delete deletedSeq. Absorb
`{depsKeepCached, seqKeep}` — note today's source order here is beads →
updateEventDeps → dirty/deletedSeq deletes; absorb groups the deletes
with the beads write. Equivalent under the held lock (updateEventDeps
reads neither dirty nor deletedSeq) — assert in review, covered by T6.

**EV5 — conflict dirty-mark** (`:113-117`): `c.dirty[patch.ID] =
struct{}{}` under its own Lock/Unlock → `markDirtyLocked`. (#2927 path —
byte-identical.)

**EV6 — ApplyDepEvent** (`:345-358`): deps-overlay writer + clears
dirty/deletedSeq → `clearStalenessMarksLocked`. Not an absorb (never
touches beads row).

### Site census

7 evict/tombstone (E1-E6 + Branch-B implicit), 5 snapshot-merge absorbs
(A1-A5), 4 read-path absorbs (R1-R4), 13 write-path absorb/fallback
shapes (W1-W17 collapsing to ~9 distinct opt combinations), 3 event
absorbs + 1 tombstone + 1 dirty-mark. Distinct absorbOpts combinations
observed: **9** — enumerated exhaustively in test T2's table.

### Parity note P1 — extra defensive clones are allowed, missing clones are not

Several sites assign already-cloned values without a second clone (A2,
R2, R3, W6, W8, W15-closed-fallback). `absorbFreshLocked` always clones.
An EXTRA clone is behavior-neutral (Bead is a value type whose reference
fields — Labels/Needs/Metadata/Dependencies/pointers — are what cloneBead
deep-copies; double-copying is pure cost, measured in T1's benchmark as
noise at cache scale ≤5k rows). The reverse direction is FORBIDDEN: no
site may lose a clone it has today, because callers retain references to
the source value (e.g. notification payloads captured pre-absorb).

## Invariants — the correctness contract

### I1 — Map-motion invariants (which maps move together)

- **Absorb always**: `beads[id]` written ∧ `deletedSeq[id]` deleted.
  There is NO site that installs a row and leaves a tombstone. (A
  tombstone alongside a live row would make Get return ErrNotFound for a
  cached row — the exact #2987-class silent-wrong-read.)
- **Evict always**: all six entries for id removed atomically under one
  lock hold. There is NO partial evict anywhere (the pre-refactor risk was
  precisely a hand-copied cascade missing one line).
- **Tombstone = evict + `deletedSeq[id]=seq`** where `seq` is a
  mutationSeq value obtained under the SAME lock hold (E4/E5/E6). The
  fence is only meaningful if `seq` > any startSeq captured before this
  lock section — guaranteed because noteMutation increments under the lock.
- **`dirty` may exist without `beads[id]`** (W4/W7/W12/EV5 on uncached
  rows) — legal state; Get treats it as forced-backing-read, List treats
  ANY dirty entry as cache-decline.
- **`beadSeq` without `localBeadAt`**: event-path rows (noteMutationLocked).
  **`localBeadAt` without `beadSeq`**: only transiently impossible — every
  localBeadAt writer sets beadSeq first (noteLocalMutation = note + stamp).
  R4 (beadSeq-only clear) CAN produce localBeadAt-without-beadSeq; that
  state is read only by `recentLocalMutation` (still valid) — preserved.
- **`deps` may exist without `beads[id]`** (DepList lazy fill, closed-row
  Close keeping deps after a later eviction removes both — evict removes
  both, so only lazy fill creates this) — legal, read-only impact on
  CachedReady coverage checks.

### I2 — Lock-ordering contract

- `c.mu` is the ONLY lock the primitives may touch, and they REQUIRE it
  held in write mode. Primitives never do I/O, never call `backing.*`,
  never call `onChange`/`notifyChange`, never log.
- Backing I/O (List/Get/DepList) always happens with `c.mu` NOT held.
- **Guard-and-mutate under one continuous hold**: every site evaluates
  its fences (deletedSeq/beadSeq vs startSeq, recency conflict) and
  performs the absorb/evict inside the SAME Lock()..Unlock() section.
  Any path that drops the lock between backing I/O and mutation must
  re-capture fences after re-acquiring (Get R4 and ApplyEvent both do;
  #2210 lesson). The refactor may not introduce any new drop-reacquire.
- Notifications are collected under the lock, emitted after Unlock, in
  collection order (single goroutine → order preserved).
- `primeMu`, `lifecycleMu` are never held simultaneously with `c.mu`
  in either order EXCEPT read-then-release before acquiring the other
  (beginLazyFullPrime pattern). Primitives never touch them.

### I3 — Ordering contract per merge pass (OC-1..OC-4)

- **OC-1: one clock read per pass.** `now := time.Now()` captured once
  under the write lock; ALL recency guards in the pass use it. (Split
  clocks let a row pass the guard early in the pass and fail it later.)
- **OC-2: fence order per row**: (1) `deletedSeq[id] > startSeq`,
  (2) `beadSeq[id] > startSeq`, (3) recency conflict
  (`recentLocalBeadConflictLocked`), (4) mutate. Tombstone fence beats seq
  fence beats recency; re-ordering changes outcomes (e.g. a row both
  tombstoned and recent-local must SKIP, not keep-and-carry).
- **OC-3: absorb before deps-overlay** in ApplyEvent (EV1-EV3):
  `clearReadyProjectionLocked` inside the overlay must observe the newly
  absorbed row.
- **OC-4: notification classification BEFORE mutation.** `beadChanged(old,
  fresh)` / `depsChanged` compare the PRE-absorb cache row; classify, then
  absorb. Inverting reads the row against itself → zero updates emitted →
  run-view/event-feed silently freeze (fleet-visible).
- **skipLabels discipline**: reconcile + prime carry checks use
  `skipLabels=true` (labels excluded from change detection because the
  full-scan query is `SkipLabels: true` — fresh rows LACK labels; comparing
  them would flag every labeled bead changed forever). Read-path and
  PrimeActive use `skipLabels=false` (their rows carry labels). The
  primitive does not compare; sites keep their own flag. Any future shared
  decision function takes skipLabels as a parameter — never hardcode.

### I4 — Branch-collapse equivalence obligations (D1-D3)

- **D1 (deletedSeq GC).** Branch B wholesale-resets `deletedSeq`; under
  B's precondition (`mutationSeq == startSeq` re-checked under the SAME
  lock as the merge) every existing tombstone has `seq <= startSeq`, so
  the reset destroys no live fence. The collapsed loop's explicit GC —
  `delete tombstones with seq <= startSeq` after the merge — is therefore
  byte-equivalent in the B regime AND strictly correct in the A regime
  (preserves `> startSeq` fences that A preserves today). Net: no
  behavior change; A's tombstone leak (rows in neither snapshot nor
  cache kept forever) is fixed as a side effect.
- **D2 (depsComplete).** A degrades `nextDepsComplete=false` when a
  fenced/kept row lacks a deps entry; B sets `depsComplete=useFreshDeps`
  unconditionally (kept rows without deps can leave depsComplete=true
  with a coverage hole — CachedReady then treats missing deps as "no
  deps" and can serve a false-ready). Collapse adopts A's degradation in
  both regimes. This is a CONSERVATIVE delta vs B (more backing
  fallbacks, never wrong data) and fixes a latent B bug. Requires Julian
  sign-off as the one intentional behavior change. Soak metric: watch
  CachedReady decline-rate before/after.
- **D3 (dirty/beadSeq/localBeadAt GC).** B's rebuild implicitly GCs
  stale entries for rows in neither snapshot nor cache (e.g. W4's dirty
  mark on a row that got closed). A leaves them — and ONE leaked dirty
  entry makes List/CachedReady decline the whole cache forever. The
  collapsed loop MUST add: for ids present in `dirty`/`beadSeq`/
  `localBeadAt` but in neither `freshByID` nor `c.beads`: delete, UNLESS
  `beadSeq[id] > startSeq` or `recentLocalMutation(localBeadAt[id], now)`
  (in-flight local write to a not-yet-listed bead). Equivalent to B in
  the B regime; fixes A's leak in the A regime (same conservative
  direction as D1).

### I5 — Untouched defenses (must survive byte-for-byte)

- recoverMissingFromList (per-ID re-verify before synthesizing
  bead.closed; ErrNotFound → allow close; other errors → defer close by
  merging the cached row back into freshByID).
- The 5-second recency window constant and `recentLocalMutation` shape.
- ApplyEvent's entire conflict lattice (approach (c) is OUT of scope).
- `preserveCachedReadyProjectionLocked` and all IsBlocked overlay logic.
- Notification payload choices (confirmedClosed row over synthetic
  status-flip; fresh over preserved rows per site tables above).
- `ownsBeadID` filtering, cachePartial/cacheLive/cacheDegraded state
  machine, syncFailures/backoff, cadence/latency logic, stats counting
  (Adds/Removes/Updates increments exactly where notifications classify).

### I6 — Project-wide invariants (unchanged by construction)

Zero hardcoded roles (this layer has none); typed wire/events untouched
(cache notifications feed the existing typed onChange path; no
RegisterPayload changes); no session-lifecycle code touched; internal/
beads stays Layer 0-1 with no upward imports; cmd/gc and internal/api
remain projections (no call-site signature changes leak upward — the
primitives are unexported).

## Behavior-preserving migration/staging

Branch `simplify/s01-b` (approach b supersedes a; a is Phase 1 of b).
Each phase is a separate PR, ≤5 files, full gates green, and — for
Phases 2-3 — a 24h maintainer-city soak before the next phase starts.

**Phase 0 — pin current behavior (test-only PR).**
Add the differential/parity test harness (T1-T3 below) against the
UNCHANGED code: generated snapshot/cache-state corpus run through
`runReconciliation` via the existing white-box seams, golden end-state +
notification capture. Also add the per-site opts table test skeleton
(T2). No production code changes. This is the net everything else lands on.

**Phase 1 — primitives, mechanical (production PR #1).**
Introduce `evictLocked`/`tombstoneLocked`/`absorbFreshLocked`/
`markDirtyLocked`/`clearStalenessMarksLocked` in `caching_store.go`;
rewrite E1-E6, A1, A2, A4, R1-R4, W1-W17, EV1-EV3, EV5-EV6 as pure
call-site substitutions per the enumeration tables. NO guard moves, NO
branch changes, NO ordering changes. prime() fast path (A3) and
reconcile Branch B (A5/E7) keep their whole-map rebuilds. Reviewer
checklist = the enumeration section of this spec, one checkbox per site.
Expected diff: ~-120 LOC. `git diff` on tests: zero (Phase 0 tests pass
untouched — that IS the gate).

**Phase 2 — reconcile branch collapse (production PR #2).**
Delete Branch B (:453-548); Branch A body becomes the unconditional
loop; add the D1/D3 GC passes; extract `reconcileMergeDecision` as the
pure function; adopt D2 (A's depsComplete degradation) — flag D2 to
Julian in the PR description as the sole intentional delta. Gate:
differential test (T3) proves old-A ≡ old-B ≡ new loop over the corpus
in the `mutationSeq == startSeq` regime, and old-A ≡ new loop in the
mutated regime, modulo the enumerated D1/D3 GC deltas (asserted
EXACTLY — the test asserts the leaked entries ARE collected, not just
"end states equal").

**Phase 3 — prime()/PrimeActive over the shared loop (production PR #3).**
Re-express prime() fast path and PrimeActive's merge over
`reconcileMergeDecision` + primitives (keeping their per-site opts:
A1's clearDirty:false, A2's seqClearBeadSeqOnly, prime's
skipLabels=true vs PrimeActive's false, no notifications). Gate: T3
extended with prime/PrimeActive golden runs; the skipLabels drift case
(A3 note) gets an explicit corpus entry.

**Phase 4 — soak + cleanup.**
48h fleet soak (below); then delete any now-dead helpers
(`carryRecentLocalMutationLocked` dies with Branch B in Phase 2 —
verify no other callers via the No-Semantic-Search checklist: direct
calls, tests, mocks).

**Rollback**: each phase is a single revertable squash-merge; Phase 2/3
revert cleanly onto Phase 1 because primitives' signatures never change
after Phase 1.

**Explicit non-goals (do NOT do in S01):** ApplyEvent lattice collapse
(approach c — separate spec, lands after S02's dirty-overlay work);
folding the six maps into one `beadCacheRow` struct; harmonizing A1/A2
clearDirty, W3-vs-W13 dirty policy, or Close-vs-CloseAll deps policy
(file candidate-harmonization beads instead, one per divergence, with
the site table as evidence).

## Test plan (incl. -race/parity if applicable)

Existing net: ~6.8k LOC white-box tests (`caching_store_internal_test.go`
4,278 lines + siblings) must pass UNMODIFIED through Phase 1 and with
only D1/D3-GC additions in Phase 2/3.

**T1 — primitive unit tests** (`caching_store_primitives_test.go`, new).
Table-driven over all six maps: for each of the 9 observed absorbOpts
combinations × {row cached, uncached, tombstoned, dirty, recent-local,
stale-local, beadSeq-only}: assert the exact post-state of ALL SIX maps
(not just the touched ones — the contract is what does NOT move).
evict/tombstone: same grid.

**T2 — per-site opts conformance.** A compile-time-ish table test: one
named case per enumerated site (E1..EV6) constructing the site's
pre-state and invoking the REAL public entry point (Update, Close,
ApplyEvent, refreshCachedBeads via List, reconcile via test seam),
asserting the six-map post-state equals the hand-computed current
behavior recorded in this spec. Written in Phase 0 against OLD code —
they pin behavior; Phase 1 must not touch them.

**T3 — differential merge test (the Phase 2 gate).** Generator produces
(cacheState, freshSnapshot, fence-config) tuples covering: row in
{snapshot only, cache only, both, neither} × {tombstoned ≤/> startSeq,
beadSeq ≤/=/> startSeq, recent-local ±changed, dirty ±, closed ±,
deps present/absent/changed} × depsComplete ±. Run OLD Branch A, OLD
Branch B (kept in the test file as a frozen copy), and the NEW loop;
assert identical {six maps, depsComplete, stats deltas, ordered
notification list} modulo the exact D1/D3 GC set (computed
independently and asserted as the ONLY difference). Seeded
pseudo-random + the hand-written corpus; run count ≥10k cases in CI
fast tier (pure functions — no I/O).

**T4 — pure-function exhaustive test.** `reconcileMergeDecision` over
the full guard lattice (all fence orderings, OC-2): assert skip/absorb/
notification classification. Enumerable because pure.

**T5 — `-race` plan.** (a) All existing concurrency tests under `-race`
(already in `make test`); (b) NEW hammer test: one goroutine looping
reconcile (via seam), N goroutines doing Create/Update/Close/Delete, M
goroutines ApplyEvent with conflicting payloads, K goroutines
Get/List/CachedReady — against a fake backing with injected latency +
ErrNotFound/partial-result faults; run 30s under `-race` in the
integration tier; assert (i) no race reports, (ii) convergence: after
quiescence + one reconcile, cache state == backing state exactly, no
leaked dirty/beadSeq/localBeadAt/deletedSeq entries (D1/D3 assertions),
(iii) every served Get during the run returns either current or
a ≤5s-stale value (recency contract), never a tombstoned row.
Reproduce flaky failures inside the PID-namespace sandbox per fleet
memory (`sudo unshare --pid --fork --kill-child --mount-proc`).

**T6 — ApplyEvent ordering regression.** EV1/EV3 absorb-then-overlay
order (OC-3): event with deps fields on a row whose IsBlocked is set;
assert projection cleared against the NEW row. Plus the EV-seqKeep
trap (T7): apply event → immediately run refreshCachedBeads with a
STALE snapshot (startSeq captured pre-event) → assert the stale row
does NOT clobber (beadSeq fence held ⇒ seqKeep preserved it).

**T7 — seqKeep divergence guard** (named test, referenced from the
Target-design warning): absorb via each event site, then assert
`beadSeq[id]` still present; mutate a copy of absorbFreshLocked to use
seqClearGuarded and assert the test FAILS (meta-verified once during
development, comment records it).

**Parity/soak plan.**
- Local: `make test-fast-parallel` + `make test-cmd-gc-process-parallel`
  + integration shards per TESTING.md; `go vet ./...`.
- Cluster soak (Phases 2-3, 24-48h on maintainer-city): compare
  before/after windows on (a) `beads cache: reconciled` log-line
  adds/updates/removes rates, (b) CacheStats ProblemCount,
  ReconcileRecoveries, ReconcileCloseDeferrals, (c) CachedReady
  decline rate (D2 metric), (d) zero occurrences of the #2987
  signature (stale bead served after close — probe: `gc hook --claim`
  loop + bd close cross-checks), (e) memory of the six maps via the
  stats endpoint (D1/D3 leak fix should show monotone-bounded map
  sizes). Anomaly grep MUST exclude sudo audit noise
  (`grep -vE 'sudo\['`, per fleet memory).

## Top correctness risks

1. **seqMode misassignment at an event site (seqClearGuarded instead of
   seqKeep)** silently deletes the beadSeq fence noteMutationLocked just
   set; the next in-flight snapshot (refreshCachedBeads/reconcile with an
   older startSeq) then overwrites the event's row — stale bead served
   fleet-wide with zero errors. Exactly the #2210/#2987 class. Mitigation:
   T7 named guard test + per-site opts table (T2) + the enumeration
   tables as the review checklist.

2. **Branch-collapse GC over- or under-collection (D1/D3).**
   Over-collect: deleting a `> startSeq` tombstone or a recent-local
   beadSeq resurrects a deleted bead or clobbers an in-flight local
   write. Under-collect: one leaked dirty entry makes List/CachedReady
   decline the cache permanently (dispatcher falls to backing on every
   read — latency regression that presents as "dispatcher slow", not as
   an error). Mitigation: T3 asserts the GC set EXACTLY, both directions;
   soak metric (e) watches map sizes.

3. **depsComplete regime change (D2) interacting with CachedReady.**
   Adopting Branch A's degradation everywhere is conservative, but if the
   degradation fires persistently (e.g. a fenced row that never gets deps
   because its bd payload never carries them), depsComplete stays false,
   CachedReady declines forever, and ready-serving load shifts to bd —
   the #3789/dispatcher-idle failure smell. Mitigation: D2 sign-off +
   soak metric (c) with an explicit revert trigger (decline rate >2x
   baseline for 1h).

4. **Notification classification drift (OC-4)** — classifying against the
   post-absorb row yields zero bead.updated events; run-view projections
   and the event feed freeze while reads stay correct, so nothing errors.
   Mitigation: T3 compares ORDERED notification lists, not just map state.

5. **prime/PrimeActive re-expression (Phase 3) flattening their variance**
   (A1 clearDirty:false, A2 seqClearBeadSeqOnly, skipLabels split) — each
   flattened axis is an incident-hardened defense (dirty-survives-prime,
   labels-blind full scans). Mitigation: Phase 3 is separately gated,
   separately soaked, and each axis has a dedicated corpus entry; if
   pressure to ship rises, Phase 3 is droppable without weakening
   Phases 1-2.
