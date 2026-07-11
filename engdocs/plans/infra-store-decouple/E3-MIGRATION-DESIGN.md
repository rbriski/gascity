# E3 — Migration design (single-Dolt-db city → domain/infra two-store)

**Fable 5 design, 2026-07-07. Plan of record for E3 implementation.** E2 is DONE +
integration-validated on real Dolt (boundary invariant passes). E3 migrates an EXISTING
comingled single-db city to two stores in place.

> **IMPLEMENTATION DEVIATIONS (2026-07-07, validated on real Dolt bd 1.1.0-rc.1).**
> Two premises in §3/§7 were wrong against the real bd/Dolt code and were adapted to
> the design's INTENT ("delete moved beads without mutating staying neighbors"):
>
> 1. **The `bd delete` batch path is NOT the safe path.** bd's batch delete
>    (`cmd/bd/delete.go` `deleteBatch`) ALSO runs `updateTextReferencesInIssues`, so
>    it text-rewrites every connected STAYING bead's free-text to `[deleted:ID]` —
>    exactly the mutation bomb §7.1 attributed only to the single-id path. So
>    `BdStore.DeleteAllOrphaning` deletes via a raw `bd sql "DELETE FROM issues/wisps
>    WHERE id IN (...)"` (no `bd delete` at all): set-based row removal, FK cascades
>    clean the deleted beads' own dep/label/event rows, and NO application-level text
>    rewrite touches staying beads. The `≥2-ids-per-call` chunking rule is therefore
>    obsolete (there is no single-vs-batch bd-delete branch to feed); a raw SQL DELETE
>    is uniform for 1..N ids.
> 2. **Inbound dependency ROWS do NOT survive on bd.** Migration 0043 added
>    `fk_dep_issue_target FOREIGN KEY (depends_on_issue_id) REFERENCES issues(id) ON
>    DELETE CASCADE`, so deleting a moved bead cascade-drops the inbound edge from a
>    staying bead. That is the E2-native cross-boundary shape (risk #2: the edge stops
>    blocking), not a preservable dangling row. The MemStore fast tier still pins the
>    row-preserving contract for a non-cascading backend; the integration tier instead
>    proves the batch delete does not text-rewrite the staying neighbor.
>
> A `ForeignIDCreator` capability (`bd create --id <legacy-id> --force`) was also added:
> bd rejects a create whose --id prefix differs from the infra db's `gcg` prefix without
> `--force`, so copying a legacy HQ/rig-prefixed infra bead into the infra store needs the
> forced create to keep the stable id. Both integration tests
> (`TestInfraStoreMigrateIntegration`, `TestInfraStoreMigrateCrashResumeIntegration`) PASS
> on managed Dolt.

## North Star
After E3 the city's data is indistinguishable from a city born split under E2: infra-class
beads in the infra store (keeping HQ/rig-era ids — do NOT re-mint), work beads untouched,
cross-boundary refs in the E2 steady-state shape (metadata refs copied verbatim; dep edges
co-resident with their SOURCE bead, dangling across the boundary, resolved by the E1.6
Go-side seams). CORRECTION: comingled infra beads are not only HQ-prefixed / in the HQ
store — the CLI-sling split-brain created rig-prefixed molecules in rig stores, so the
migration sweeps EVERY domain store (city + all rigs = the coordClassStoreCandidates set).

## 1. Detect + command surface
- `cityNeedsInfraStoreMigration(cityPath)` — config-shape + live-state (NOT a marker file):
  can-migrate = cityUsesBdStoreContract && !isExternalDolt; needs = !cityHasInfraStore OR a
  domain store still holds infra-class beads (resumable case). Refuse external/hosted Dolt.
- Command: NEW `gc migrate infra-store` (`--dry-run`, `--json`); register newMigrateCmd in
  main.go:240-266 (gc migrate is free; the old shim is `gc import migrate`). NOT doctor --fix
  (this is owner-gated stop-the-world), NOT auto-migrate at gc start. ADD a read-only doctor
  ADVISORY ("this city predates the infra store split; run gc migrate infra-store").
- Preflight in doMigrateInfraStore: refuse if controllerAlive(cityPath)!=0 (stop first,
  live-state); ensure managed Dolt up (initDirIfReadyEnsureBeadsProvider +
  initDirIfReadyWaitForManagedDolt).

## 2. Create the infra scope
seedInitInfraScope(cityPath) (cmd_init.go:1573) + initAndHookDir(cityPath,
infraScopeRoot(cityPath), config.InfraScopePrefix) (beads_provider_lifecycle.go:511) — both
idempotent, the exact E2.5 calls. PLUS one NEW step E2 didn't need:
`writeInfraScopeRoutes(cityPath, cfg)` — build via rig_beads.go collectRigRoutes + append
{Prefix: InfraScopePrefix, AbsDir: infraScopeRoot}, generateRoutesFor + writeRoutesFile.
LOAD-BEARING: bd dep add hard-fails when the target neither resolves nor differs in prefix
(bd cmd/bd/dep.go:338-360); a same-prefix cross-boundary edge (gcy-cv1→gcy-45) needs bd's
prefix routing to find the target read-only in the hq db, which routes.jsonl provides
(local-first then routes). Likely fixes a latent E2 attach gap too — flag as an E2.3 follow-up.

## 3. Move (copy-then-delete)
- Enumerate each domain store (openCityStoreAt + each rig via openStoreResultAtForCity) with
  List{IncludeClosed, TierBoth, AllowScan}; classify each via coordclass.Classify;
  class.IsInfrastructure() ⇒ moved set. List the infra store into a map[id]Bead = the
  idempotency mechanism.
- **M1 copyBeadPreservingID(dst, src)**: dst.Create(b) with b.Needs=nil, b.ParentID="",
  b.Dependencies=nil (BdStore passes --id for a non-empty id; policy wrapper mintInfraBeadID
  only fires on empty id → legacy ids never re-minted). Then restore status: closed →
  dst.Close(id) (reads metadata.close_reason → --reason); other non-open → dst.Update(id,
  {Status}). Accepted losses (document): created_at/updated_at re-stamped; bd events/comments
  not carried; is_blocked recomputed.
- **M2 edges**: DepListBatch (assert) / DepList fallback on the SOURCE store for each moved
  bead's OUTgoing edges. Skip if pair already in infra (dependencies PK (issue,depends_on)).
  Skip dotted parent-child (HasPrefix(A, B+".")). Else infraStore.DepAdd(A,B,type) — A local
  (copied), B intra-set local or cross-boundary read-only via routes (no FK on depends_on_id
  → dangling target OK). Parent links restored as parent-child DepAdd (NOT bd update --parent,
  which has no routing). INcoming edges from staying work beads need NO action (co-resident
  with source, survive as dangling rows).
- **M4 delete from work stores** (after verify). THE MISSING PRIMITIVE: single-id
  beads.Store.Delete is a mutation bomb (bd delete single-id strips INbound edges +
  text-rewrites neighbors, cmd/bd/delete.go:175-209). Add `BatchDeleter.DeleteAllOrphaning(ids)
  (int,error)` → BdStore chunked `bd delete --force --json` with ≥2 ids per call (batch path:
  orphans external dependents instead of touching them, no text rewrite, drops only moved
  beads' own edges via fk_dep_issue cascade, preserves inbound dangling rows, routes wisps).
  Degenerate singleton → fall back to Delete + WARN. Delete set = infra-classified in that
  store ∩ verified present in infra. Never delete anything not proven copied.

## 4. Idempotent/resumable/crash-safe
No status file. Recompute plan from live state each run: partition classified-infra beads by
{work-only → copy; both (crash between copy+delete) → skip copy, idempotent edges, delete;
infra-only → done}. Phases GLOBAL in order (all M1 → all M2 → verify → all M4) so an edge
never precedes its endpoints, delete never precedes verify. Crash inside any phase leaves
only re-runnable states. Re-run on a migrated city → moved:0 (convergent no-op). Dual-presence
safe because city is stopped.

## 5. Verify (gates M4; re-run after)
verifyStoreClassBoundary(store, wantInfra) — same logic as assertStoreClassBoundary. After M4
every domain store verifies wantInfra=false, infra store wantInfra=true. Count reconciliation:
work_after == work_before − moved; infra_after == infra_before + Σmoved; each moved id Gets
from infra with matching (Type,Status,Labels,Metadata⊇). Emit ledger as --json. Do NOT apply
the reserved-PREFIX invariant to migrated beads (they keep HQ ids by design; prefix boundary
is only for the post-split delta).

## 6. Integration test (real managed Dolt) — cmd/gc/infra_store_migrate_integration_test.go //go:build integration
Comingled city via setupManagedBdWaitTestCity (NO GC_INFRA_STORE_SPLIT, no infra seed) → seed
a mixed population in the WORK stores (session, mail single-store provider, queued nudge,
order run, wait bead, molecule.Instantiate, molecule.Attach for a work→graph blocks edge,
synthetic convoy tracking a work task, plain tasks + user convoy stay-behind controls, one
closed infra + one closed work, one ephemeral wisp; repeat a subset in the rig for
rig-prefixed molecules) → doMigrateInfraStore → assert cityHasInfraStore + assertStoreClassBoundary
(domain=false ×stores, infra=true) + count reconciliation + id stability (infraStore.Get(origID))
+ cross-store deps (convoy.Members cross-store; attach task's blocks edge STILL in work store,
dangling infra target = orphan-preserving-delete proof) + re-run no-op (moved==0) + crash-resume
arm (hand-copy one bead pre-run → converges, no dup) + --dry-run zero-writes. PLUS a fast-tier
MemStore version (work=wrapStoreWithBeadPolicies(NewMemStore), infra=wrapInfraStoreWithBeadPolicies(
NewMemStoreHonoringIDs)) pinning classification/ordering/idempotency in-sandbox. Gate via
GC_FAST_UNIT=0 / skipSlowCmdGCTest.

## 7. Risks
(1) bd delete single-id bomb — mitigated by BatchDeleter; never loop Delete on a move set.
(2) is_blocked flips for work→infra blockers (dangling target stops blocking in bd ready) —
E2's native cross-boundary shape; inventory + print work→infra blocking edges; verify a
cross-boundary drain still completes (E4.3). (3) same-prefix cross-boundary DepAdd needs the
routes.jsonl (§2). (4) closed/tombstone: Close covers closed; pinned/tombstone via Update — if
bd rejects, map to closed + gc.migrated_status. (5) copy-then-delete is correct (close-then-
recreate mutates source before dest proven, leaves a ghost). (6) mail/session refs are metadata
strings, copied verbatim, resolved by E1 two-store-safe accessors/storeref. (7) by-id reads of
HQ-prefixed migrated infra beads rely on the bare-id probe-all fan-out — its candidate set must
include the infra store (E4.4 gate). (8) hosted/external Dolt — refuse in v1 (tenancy: one db
per project). (9) volume/latency — offline, tens of min for 10k beads; optional
--skip-closed-wisps (TTL garbage). (10) hooks fire per mutation (no-op with controller down).
(11) wisp-tier deletes route to deleteWispBatch — assert inbound edges survive.

## 8. Deliverables
- cmd/gc/cmd_migrate_infra_store.go (newMigrateCmd + infra-store subcommand + registration).
- cmd/gc/infra_store_migrate.go (doMigrateInfraStore, cityNeedsInfraStoreMigration,
  writeInfraScopeRoutes, copyBeadPreservingID, migratePlan/CopyBeads/CopyEdges/Verify/
  DeleteFromWork, verifyStoreClassBoundary).
- internal/beads: BatchDeleter capability + BdStore.DeleteAllOrphaning (chunked, ≥2 ids) +
  MemStore/FileStore impls for the fast tier.
- Doctor advisory check.
- Tests: fast-tier migrator-core (MemStore), integration (§6), a tripwire unit test that
  single-id Delete is never called on a multi-bead move set.
- Backlog E3.1–E3.4 checkboxes; E3.4 re-assert via cityNeedsInfraStoreMigration==false on a
  fresh split city.
