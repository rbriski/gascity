# Two-Store Default: work + infra, always

Status: **Spec** (2026-07-10). Decisions locked by Julian:
- **Q1 — one infra db** for all coordination classes (per-class granularity deferred until needed).
- **Q2 — startup** auto-migration (transparent), not `gc doctor --fix`.
- **Q3 — fold** the graph class's legacy on-disk location into the one infra db.

## 1. Summary

Every city has **exactly two physical databases**: a **WORK** db and an **INFRA**
db. INFRA holds *all* coordination-class beads — graph, sessions, messaging,
orders, nudges — in **one** database. This holds **regardless of backend type**: a
Dolt city is `work` + `infra` (two Dolt databases), a SQLite city is two files, a
Postgres-infra city is `work.dolt` + `infra.postgres`. Single-store (everything in
one db) is a **legacy pre-migration state only**; on startup an existing single-store
city **auto-migrates 1→2** before it serves, and new cities are created two-db.

## 2. Motivation — kill the dual-mode

Today the split is **conditional**. `resolveClassStore` (`cmd/gc/class_store.go:164`)
collapses a class to the work store whenever its backend is `bd` (the default), and
`resolveGraphStore` *silently* falls back to the work store when the relocated store
can't open. That conditional is the root of every seam we hit:

- **Silent orphan** — a `ClassGraph` molecule poured onto the work store because the
  infra store wasn't wired (guarded loud in `ef53ba15f`, but the *cause* is the
  conditional).
- **Prefix ambiguity** — same-store-different-prefix vs cross-store were
  indistinguishable through a single federating handle (broke the `TrackItem` guard,
  `d3fadc832`), *because* a single store could legitimately hold heterogeneous prefixes.
- **Untested seams** — cross-store code paths were unreachable at the `bd` default, so
  they only ever surfaced on a live relocated deploy.

**Always-two-db makes the cross-store path the *only* path** — always wired, always
exercised, always tested. The seven fixes shipped this cycle
(`d6479fbc0`/`ccebee78b`/`e057a2f69`/`d3fadc832`/`cd62cf7b8`/`d71e054b7`/`ef53ba15f`)
become the *normal* runtime, and the orphan guard becomes a can't-fire invariant.

## 3. The model

| | WORK db | INFRA db |
|---|---|---|
| Holds | work-class beads (tasks, work) | ALL coord classes: graph, sessions, messaging, orders, nudges |
| Prefixes | work / rig / HQ (`ga-`, `r1-`, …) | reserved coord prefixes `gcg-/gcs-/gcm-/gco-/gcn-` |
| Backend | as configured (Dolt today) | **same as work by default**, co-located; may be sqlite/postgres |

- **Resolution reduces to one predicate:** `isReservedCoordPrefix(id) → INFRA, else →
  WORK`. Prefix-disjointness is already enforced
  (`internal/config/reserved_prefixes.go` + `ValidateReservedPrefixesIn`), so this is
  total and unambiguous — no per-store probing, no federation ambiguity.
- **Post-migration invariant:** the INFRA db is *always* a distinct physical store;
  `infraStore == workStore` is an error, not a fallback.

## 4. Config

- **New knob:** `[beads.infra]` with `backend = "bd" | "sqlite" | "postgres"` (default
  `bd` = a co-located database of the *work* backend). One knob, one infra db.
- **Default (no config):** two-db, infra = same backend as work, co-located.
- **The existing per-class `[beads.classes.<class>]` config and `GC_INFRA_POSTGRES_*`
  env are subsumed:** a per-class Postgres split maps to `[beads.infra] backend =
  postgres` (all coord in one Postgres infra db). Honored during transition; the
  target is the single infra knob. Per-class granularity is a future extension, not
  wired now.

## 5. Store resolution (runtime, post-migration)

- Introduce `resolveInfraStore(workStore, cfg, cityPath, rec) → beads.Store` — the
  single infra db, **always distinct from workStore**. It replaces and folds together
  `resolveGraphStore` / `resolveSessionStore` / `resolveClassStore` /
  `resolveOrderStore` / `resolveNudgesStore` / `resolveMailMessagesStore`.
- `beadPolicyStore.graphStore` becomes `infraStore`; `createTarget` /
  `writeOwner` / `federateGraphList` route by "reserved coord prefix → infra." (The
  code already does this shape; it just always has a real infra store now.)
- **Graph legacy location folded in:** the graph class currently lives at
  `<cityPath>/.gc/beads.sqlite` (`openGraphSQLiteStore`, the data-orphan invariant).
  In the two-db model graph beads live in the *infra* db; the migration (§6) moves
  existing `.gc/beads.sqlite` graph data into the infra db. The special-case opener is
  retired once migration completes.

## 6. Auto-migration 1→2 (startup gate)

Runs **before the city serves**, so post-startup every city is two-db.

1. **Detect** single-store: no infra db present / no `migrated` marker.
2. **Backup** the work db (and the legacy `.gc/beads.sqlite` if present).
3. **Create** the infra db (same backend as work, co-located; or the configured
   infra backend).
4. **Move** every coord-class bead from work → infra, **ID-preserving**, reusing the
   `gc beads migrate` engine (`cmd/gc/cmd_beads_migrate.go` — already ID-preserving +
   idempotent). Classification by `coordclass.Classify` / reserved prefix.
5. **Fold** the legacy `.gc/beads.sqlite` graph beads into the infra db.
6. **Verify** counts (source coord-bead count == infra count; no coord beads left in
   work; no work beads moved).
7. **Mark** migrated (config/marker) and proceed to serve two-db.

**Safety contract:** backup-first · ID-preserving · idempotent (re-run skips already
moved) · **hard-fail-don't-serve** (if migration can't complete, refuse to start
rather than serve a half-migrated / orphaned city) · resumable (a partial run
re-completes on the next start). One-way (1→2); rollback is via the backup.

## 7. Runtime simplification (Phase C)

- Delete the `bd`-collapse (`class_store.go:164`) and `resolveGraphStore`'s silent
  fallback (`class_store.go:299/304/306`) — post-migration the infra store is always
  wired; inability to open it is a hard start-time failure (the migration gate owns
  that).
- The conditional "relocated?" branches (`graphRelocated`, `graphStore == Store`,
  "byte-identical when not relocated") collapse to a single always-two-db path. The
  federation (by-id write federation, scan union, session owner-routing) is
  unconditional.
- The orphan guard (`ef53ba15f`) is kept as an **invariant assertion** — it can no
  longer fire in a migrated city, but it catches a regression that breaks the invariant.

## 8. Cross-store relationships (unchanged, now always-on)

- Non-blocking links (convoy `tracks`) → ref-by-id metadata (`gc.tracking_convoy_id`).
- By-id ops on a bead in the other store → owner-routing (the write/read federation).
- Cross-store **blocking** deps do not occur on automated paths (certified this cycle;
  only manual `formula cook --attach`). Cross-**rig** blocking (multiple work dbs) is
  the separate **mirror-beads / bd-federation** track — orthogonal and future. This
  spec is the **work/infra** split *within one city*, which becomes the consistent
  baseline that cross-rig work builds on.

## 9. Phasing

- **Phase A — two-db default.** Flip the default so a city with no infra config yields
  a distinct co-located infra db (retire the `bd`-collapse for the default path). New
  cities are created two-db. Existing relocated (`[beads.classes.*]`/postgres) cities
  keep working (infra = their relocated store).
- **Phase B — auto-migration 1→2 on startup.** The gate in §6. This is where legacy
  single-store cities become two-db transparently.
- **Phase C — retire the conditionals.** Make always-two-db the only runtime path;
  demote the single-store code to the migration reader; the orphan guard → invariant.

## 10. Testing

- The **split-topology conformance harness** (from
  `engdocs/plans/cross-store-substrate/DESIGN.md`) becomes the **default** test mode —
  every city test runs two-db, with a **strict-store** wrapper so a cross-store dep-add
  fails in-process (the Mem/SQLite leniency gap). The `bd`/single-store rows are dropped
  (no such runtime target) except in the migration test.
- **Migration test:** seed a single-store city → run the startup gate → assert coord
  beads moved to infra (ID-preserved), work beads untouched, legacy `.gc/beads.sqlite`
  folded, idempotent on re-run, and a mid-migration crash resumes cleanly.
- **Invariant test:** `infraStore != workStore` after startup; the orphan guard never
  fires in a migrated city.

## 11. Open questions / risks

- **Infra db naming/location convention** — co-located Dolt database name (e.g.
  `<work>_infra` on the same Dolt server) vs a sibling `.beads` dir; SQLite file path.
- **Two co-located Dolt dbs** — connection/resource overhead on one Dolt server;
  credential reuse. (Same backend ≠ same database; each needs its own connection.)
- **Migration atomicity** across two backends when infra ≠ work backend (e.g. Dolt →
  Postgres) — the backup + resumable + verify contract covers it; confirm no partial
  visibility.
- **Rollback** — backup enables manual 2→1; no automated reverse needed (assumed).
- **Ordering vs the existing infra-store lineage** (`plan/decouple-infra-beads` /
  `feat/domain-infra-store-split`) — this spec *is* the destination of that lineage,
  generalized (same-backend default + auto-migration); align rather than fork.
