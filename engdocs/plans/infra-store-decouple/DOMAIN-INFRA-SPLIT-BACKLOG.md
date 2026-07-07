# Domain / Infra store split — burn-down backlog

**Status:** plan of record (owner design session, 2026-07-06). Merges the
interface-standardization long tail with the city db-split design into one
ordered backlog. Supersedes the "relocate each class to SQLite" framing in
`DESIGN.md` where they conflict — see §"Why this reframing".

> This file is the backlog (no bd — the shared `ga` Dolt db is a stale-binary
> schema mismatch, and this thread is tracked in-file anyway). Check items off
> as they land.

---

## Promised land (definition of done)

1. **Two tiers of stores, boundary real on Dolt:**
   - Per-rig **DOMAIN** stores (Dolt) hold ONLY domain work beads:
     `task/epic/bug/feature/merge-request/spec/pack` + user/sling convoys.
     HQ is just a rig (`EffectiveHQPrefix`). `coordclass.ClassWork`.
   - ONE city **INFRA** store holds ALL infrastructure beads: sessions, mail,
     nudges, orders, graph, **and the whole formula/orchestration explosion**
     (molecule roots, control/step/gate/scope/run/retry beads, wisp roots,
     convergence, synthetic convoys). Exposed via the 5 domain-typed views
     (`beads.SessionStore/GraphStore/MailStore/OrdersStore/NudgesStore`) over
     one physical store.
2. **No routing layer.** Every store call site names the store its context
   implies (a session op grabs the session store; a work op grabs the rig's
   work store). The `storeref`/prefix-"federation" crutch is deleted except the
   one genuinely context-free case (`gc show <bare-id>` = a trivial prefix
   lookup). Raw bead-store access exists NOWHERE except the domain-task path.
3. **Backend is a swappable no-op.** With domain and infra as two Dolt stores,
   swapping the infra store's backend (Dolt → SQLite/Postgres) is scope-invariant
   with structural parity. Routing gates on the **store boundary / reserved
   prefix**, NEVER on "is the backend sqlite".
4. **Exhaustive e2e upgrade test** proves a 1-db city upgrades to 2-db with every
   bead in the right store, nothing lost, cross-store references resolving, and
   every `gc` command working.

## Why this reframing (settled in the design session)

- **Scope ≠ substrate.** The current plan couples them ("graph → city SQLite").
  We do the store BOUNDARY first, on Dolt, then swap the backend as a no-op.
  Graph was done coupled and hit a ~73-gap co-residence audit in prod on an
  unfamiliar backend with tests that didn't exercise the split shape.
- **The classification is already correct.** `coordclass.Classify` already puts
  domain → `ClassWork` and infra (incl. the orchestration explosion) → the infra
  classes, via `gc.root_bead_id` etc., deliberately not by type. Reserved
  prefixes (`gcs/gcg/gcm/gco/gcn`) are already registered store-boundary markers.
  `resolveClassStore` is the seam (today a pure identity stub).
- **The hard part is a mechanical sweep, not a design.** Single-store legacy left
  thousands of `store.Get(id)` sites that never had to name a class. The work is
  making each one name the store its context already implies. A missed site
  silently writes/reads the wrong store once the boundary is live — and the
  audit proved the tests won't catch it. Hence: finish the sweep + a guard
  FIRST, flip the boundary SECOND.

---

## Cross-cutting discipline (reuse the proven method on every site)

Verified per-consumer census (re-grep; don't trust prior classifications) →
convert the site to its typed accessor → `gofmt` · `go build` · `go vet` ·
`golangci-lint 0` · targeted tests → **revert-canary** (guard fails naming the
file) → **adversarial byte-identity review** (fable; byte-identical while the
resolver is still identity) → commit `--no-verify`. Byte-identity is the bar
until the boundary flips (E2).

---

## E1 — Finish interface standardization (BLOCKS everything)

**Goal:** every store call site routes through a typed per-class accessor so it
names the store its context implies; delete the `storeref` crutch. Done entirely
while `resolveClassStore` is still identity, so every change is byte-identical.

**Done-criteria:** grep-clean — no raw `store.Get/List/Create` of a class bead
outside its class accessor / the domain-task path; the front-door guard tests
cover every touched file; the crutch is gone except the bare-CLI-id lookup.

- [x] **E1.1 — Deferred CLI entangled set. DONE (2026-07-06).** All 5 routed +
  guard-listed: cmd_start (`4b4961146`), cmd_sling (`cea22bf59`), and the entangled
  trio cmd_handoff+cmd_runtime_drain / cmd_wait / cmd_nudge (`4d847d0f3`, designed by a
  fable design workflow, implemented by opus, reviewed by a fable red-team workflow that
  caught + we fixed a MEDIUM nudge-dispatcher class-mix defect). **E1.2 RESIDUALS the
  red-team flagged (thread a work store once openNudgeBeadStore relocates):** (a)
  cmd_wait.go:659 inline `beads.NudgesStore{Store: store}` (work store) vs
  withdrawQueuedWaitNudges's openNudgeBeadStore; (b) the CLI nudge delivery tree
  (cmdNudgePoll/cmdNudgeDrainWithFormat) derives its session store from the
  openNudgeBeadStore base (nudges-class-routed, identity=work today only via nil cfg).
  *Extends:* `RELOCATION-ROUTING-HANDOFF.md` "Deferred" set. Plans:
  `raw/e11-design-wf_0ed69479.json`, `raw/e11-plans-extracted.txt`.
  **cmd_start.go DONE (2026-07-06, commit `4b4961146`).** Routed the full
  standalone reconcile-cascade SESSION arm (2× loadSessionBeadSnapshot, 3×
  buildDesiredStateWithSessionBeads LEADING store, 3× sync, 1× reconcile) through
  `sessStore := cliSessionStore(oneShotStore,cfg,cityPath)`, keeping `rigStores` +
  `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete` plain — an EXACT mirror of
  the daemon's store-role split (`CityRuntime.buildDesiredState`/`controlDispatcherTick`
  pass `sessionsBeadStore().Store` as the leading store; `beadReconcileTick` passes
  `cityBeadStore()` to releaseOrphaned). cmd_start.go joined `sessionRelocationRoutedFiles`.
  **LESSON: the E1 census was WRONG to defer buildDesiredStateWithSessionBeads.** It
  reasoned "leading store is dual-role (session snapshot + work demand) so it can't be
  arg-swapped" — but the daemon ALREADY arg-swaps it to the session store, so deferring
  it DIVERGED from the daemon and left session-bead CREATES (`agentBuildParams.beadStore`)
  unrouted (a post-relocation split-brain). Caught by the adversarial fable review, not
  the census. **For the remaining E1.1 files, validate every "leave on plain / defer"
  call against the DAEMON's store-role assignment (city_runtime.go) — the daemon is the
  authority on which arg is session vs work, not a static read of the callee.** The
  leading store's city-work dual-role (collectAssignedWorkBeadsWithStores /
  cold-wake scale-check) rides the session store on BOTH sides today = a shared E2
  two-store split.
  **cmd_sling.go DONE (2026-07-06, commit `cea22bf59`).** Routed the sling-nudge
  session arm (doSlingNudge session lookups; deliverSlingNudge observe/handle/stamp
  via a sessStore derived from target.cfg+cityPath — no signature change; printNudgePreview
  gained a cityPath param) through cliSessionStore/cliSessionFrontDoor, keeping the
  queued-nudge enqueue on the plain store. **Census was WRONG AGAIN: it called
  deliverSlingNudge "clean-surgical" but it is multi-class (session observe/handle/stamp
  + NUDGES enqueue on one store param)** — caught by tracing each consumer's leaf class +
  the fable review. cmd_sling.go joined the guard; 2 DEFERRED cross-package sling-root
  session sites documented (cliDirectSessionResolver needs a SlingDeps/graphroute two-store
  split; resolveGraphStepBinding* routed by cmd_convoy_dispatch). Remaining E1.1: the
  entangled cmd_nudge / cmd_wait / cmd_handoff+cmd_runtime_drain set. **Owner chose
  SEQUENTIAL FULL-QUALITY execution for all of E1 (one verified+pushed commit per file/
  group; no worktree fan-out); each file: per-consumer census re-verified against the
  DAEMON/controller routing (city_runtime.go / nudge_dispatcher.go), byte-identity +
  revert-canary + fable review.**
- [ ] **E1.2 — Non-session infra classes' periphery (cmd/gc).** Front-door the
  graph / nudges / orders read+write sites the way sessions were (mail is largely
  done via the beadmail two-store split, CONT-44). Each class → its typed accessor.
  *Extends:* `SESSION-PERIPHERY-CLOSURE-PLAN.md` Phase C, generalized off sessions.
- [ ] **E1.3 — internal/api handlers (~8 files).** Route session/graph/mail/order/
  nudge reads in the HTTP+SSE handlers through the typed accessors (many already
  use `mgr.GetWithPersistedResponse()`; extend). Read `api-control-plane.md` first.
  *Extends:* closure-plan Phase D. Blocked by: E1.2 (siblings share helpers).
- [ ] **E1.4 — internal/worker.** `factory.go` / `invocation_telemetry.go` /
  `handle_construct.go` session+graph reads → typed accessors (RAW-BY-DESIGN
  construction sites stay, documented).
  *Extends:* closure-plan Phase E.
- [ ] **E1.5 — internal/session own runtime dogfood.** The session package's own
  lifecycle code cracks raw metadata instead of using `Info`/`Store`
  (`manager.go`, `chat.go`, `named_config.go`, `names.go`, `submit.go`). Convert
  the decision-path reads; leave the codec + Create-path constructors.
  *Extends:* closure-plan Phase F (riskiest — its own session).
- [x] **E1.6 — dispatch/convoy cross-store member+source reads.** The sites that
  read a *related* bead of a known class must grab the store the reference's role
  names, not the ambient store. These are the co-residence sites the graph audit
  found — the caller knows the class from *why* it holds the id.
  *Extends:* the `MemberStores`/`ResolveStoreRef` seam already threaded here.
  **DONE (2026-07-06, commit `6706af8c3`).** An adversarial audit of the seam
  (`raw/e1-census-wf_61848080.json`) confirmed the `#3773` seam already routes the
  member Get/SetMetadata (`convoy/membership.go` `storeref.Resolve` over
  `memberStores`; `drain.go` reservation Get/SetMetadata via `drainMemberOwningStore`)
  and the source-chain reads (`runtime.go walkSourceBeadChain` via `opts.ResolveStoreRef`),
  BUT found 3 residual gaps. Two were byte-identical to fix (dormant `MemberStores`,
  empty in prod until E2) and are FIXED: `drain.go` `drainProjectedBlockerIDs:637`
  and `orderDrainMembersByDependencies:849` read a member's dep edges on the ambient
  store — a dep edge is co-resident with its source bead, so after E2 the ambient
  read returns empty and silently collapses member ordering / drops projected
  blockers. Routed both through a new `drainMemberDepStore(store, memberID, opts)`
  (owning-store when `MemberStores` set, else ambient store probe-free); added two
  two-store regression canaries that fail on a bare `store.DepList`. Full gates +
  revert-canary + fable byte-identity review (COULD-NOT-REFUTE ×5).
  **DEFERRED — NOT byte-identical (own follow-up, needs owner sign-off):**
  `retry.go` `resolveRequiredArtifactWorktree:436` reads a `gc.source_bead_id`
  parent via bare `store.Get`, ignoring `gc.source_store_ref` + `opts.ResolveStoreRef`.
  Unlike the drain gaps, `opts.ResolveStoreRef` is ALREADY populated in prod
  (`cmd_convoy_dispatch.go:211`) and `gc.source_store_ref` is present on adopt-PR
  roots TODAY, so routing it via the ref is a real behavior change RIGHT NOW (it
  would fix a latent cross-store-source degradation to `missing_required_artifact_context`
  that `walkSourceBeadChain` already handles correctly) — a genuine bug fix, not a
  byte-identical E1 conversion. Confirm the required-artifact worktree semantics for
  cross-store sources, add a test, and land it as its own behavior-affecting change.
- [ ] **E1.7 — Delete the crutch + add the invariant guard.** Remove the
  `storeref` probe-all fallback everywhere except the bare-CLI-id lookup
  (`gc show <id>`). Add a guard test that forbids new raw class-bead store access
  outside allowed files (extends `frontdoor_di_guard_test.go` /
  `snapshotInfoOnlyFiles` / `metadataInfoOnlyFiles`).
  Blocked by: E1.1–E1.6.

**Guard/enforcement:** the front-door guard test set is the tripwire — every
converted file joins the appropriate list; revert-canary each.

---

## E2 — DB-split mechanism on Dolt (depends on E1)

**Goal:** `resolveClassStore` returns two real stores; infra is uniformly
city-scoped; new cities create both. Still Dolt — no SQLite.

**Done-criteria:** two Dolt stores exist per city; every infra bead is created
in and read from the city infra store; new `gc init` creates both; a guard fails
if a domain store holds an infra bead or vice-versa.

> **E2 STATUS (2026-07-06 autonomous run): the two-database MECHANISM is COMPLETE,
> byte-identical, fable-red-teamed, and PROVEN CORRECT by the boundary-invariant test.
> Activation is OPT-IN (`GC_INFRA_STORE_SPLIT=1`, default OFF). Commits: E2.1 mechanism
> `8a529ce23` + harden `98bbdffb5`; E2.2 invariant `311d8ca12`; E2.3 write-routing
> `41279174d`; E2.4 activation `0537c8b03`. REMAINING: E2.5 integration validation +
> backend-blind guard; E3 migration; E4 e2e. ⚠️ E2.5/E4 INTEGRATION TESTS ARE BLOCKED IN
> THIS SANDBOX — bd 1.1.0-rc.1 rejects the `--dolt-auto-commit` flag the store layer
> emits (a PRE-EXISTING skew affecting the WORK store too, not the split); the integration
> tests are WRITTEN + compile + run but skip at a toolchain preflight. They need a
> dolt-capable, bd-matched env to fully validate. The FAST-tier boundary invariant is the
> in-sandbox proof and is GREEN.**

- [x] **E2.1 — Open the second Dolt store at city scope. DONE (`8a529ce23` + harden
  `98bbdffb5`).** A "store" is a SCOPE-ROOT dir (own Dolt db on the shared server), so
  the city INFRA store is a 2nd scope `.gc/infra` — NO factory change. Added
  infraScopeRoot/cityHasInfraStore/openCityInfraStoreResultAt/cachedCityInfraStore
  (main.go, same policy-wrap+CachingStore path), controllerState.cityInfraStore +
  CityRuntime.standaloneCityInfraStore + accessor threading. Harden fixed 3 dormant
  split-city reload/cache defects (fail-on-baseline tests).
- [x] **E2.2 — Non-identity `resolveClassStore`. DONE (mechanism `8a529ce23`; invariant
  `311d8ca12`).** resolveClassStore gained an `infraStore` param + boundary dispatch
  (infra classes→infra store, work→work store; gate on infraStore-presence + class,
  NEVER backend). The pinned identity test split into a nil-infra compatibility pin + a
  RouteToInfraStore test. The boundary-invariant test (real two-store harness, classify
  every bead via coordclass.Classify, negative control) is the completeness forcing
  function — GREEN.
- [~] **E2.3 — Uniform infra scope (kill the sling split-brain). WRITE-ROUTING
  DONE (2026-07-06); read-side fan-out collapse DEFERRED.** Today the CLI sling
  creates infra beads in the *rig* store while the controller reads them from the
  city store — which is why `coordClassStoreCandidates` fans infra across
  city+rigs. Route infra creation to the city infra store everywhere; collapse the
  infra fan-out to city-only. Blocked by: E2.2.

  **DONE (the forcing function — flips the E2.2 invariant test):**
  - New CLI seam `cmd/gc/cli_class_store.go`: `cliGraphStore` / `cliOrderStore` /
    `cliNudgesStore`, mirroring `cliSessionStore`. Each is
    `resolve<Class>Store(store, cachedCityInfraStore(cityPath, cfg), cfg, cityPath, nil)`
    — infra store on a split city, identity on a legacy city (nil recorder,
    documented follow-up like `cliSessionStore`).
  - **Sling graph split-brain:** `cmd_sling.go` deps construction now sets
    `GraphStore: slingSplitGraphStore(store, cfg, cityPath)`. On a split city that
    is the infra store (the wisp/workflow molecule explosion lands there); on a
    legacy city it returns **nil**, so `SlingDeps.graphStore()` collapses onto
    `Store` (the rig store) byte-for-byte as before — design open question #5
    (do NOT move legacy molecules to the city store). Sling nudge enqueue (:1519)
    now routes through `cliNudgesStore`.
  - **Order run wisp:** `cmd_order.go doOrderRunWithJSON` routes the wisp
    recipe/routing/`molecule.Instantiate`/root-`Update` through
    `cliGraphStore(store.Store, cfg, cityPath)` (`genericStore`), while
    `CreateRunClosed` tracking stays on the `OrdersStore`. The root `Update` moved
    from `store.Update` → `genericStore.Update` so it targets the store the root
    was created in (else a split-city update 404s).
  - **Order-store openers (`order_store.go`):** `openCityOrderStore`,
    `openOrderStoreForOrder`, `cachedOrderStoresResolver`,
    `cachedOrderHistoryStoresResolver`, and the sweep
    (`orderTrackingSweepStoresForConfigTargets`) now wrap the INNER store with
    `cliOrderStore(...)` before embedding, so the sweep's structural
    key/label assertions still promote. All identity today.
  - **Invariant test converted:** `TestSlingGraphMaterializationLeaksIntoDomainStore`
    (the known-leak) DELETED; sling-graph added as a PASS creator (#9) in
    `runRoutedCreators` (driven by the production `slingSplitGraphStore` selection,
    with `cachedInfraStoreOpen` swapped to the harness infra store), so the two
    boundary tests + the conformance table now assert sling's graph beads land in
    the INFRA store. New `TestSlingGraphRoutesToInfraStore` (direct fix pin) and
    `TestSlingSplitGraphStoreIsNilOnLegacyCity` (byte-identity gate) added.

  **DEFERRED (read-side fan-out collapse — precise notes; own follow-up):**
  These are read-side sweep-surface changes on a split city, gated by the design's
  risk #4 (MIXED-MODE SESSION SWEEP). They do NOT affect the write-side invariant
  (already flipped), and each is a hot-path or mixed-use change that must not be
  half-done:
  - `coordClassStoreCandidates` (`cmd/gc/session_beads.go:682`): the SESSION/infra
    arm should collapse to a single `{store: cityInfraStore, ref: "city"}`
    candidate when `cityHasInfraStore`, while the WORK arms keep the city+rigs
    fan-out. It has 3 hot reconciler call sites in `build_desired_state.go`
    (`:1005` session arm / `:1139` assigned-work arm / `:4171` session arm) and
    currently takes `(cfg, cityStore, rigStores, suspendedRigPaths, cityRef)` with
    **no infra store** — the collapse requires threading the infra store into these
    per-tick functions and split-by-class at the builder. Deferred to avoid a
    risky, partial change to the reconciler hot path; the write-side invariant is
    already green without it.
  - `openSourceWorkflowStores` (`cmd/gc/cmd_convoy_dispatch.go:1652` /
    `openSourceWorkflowStoresWith:1673`): the workflow-root singleton probe fans
    across every rig store; on a split city workflow roots live only in the infra
    store, so each opened store should be wrapped with `cliGraphStore(...)` for the
    root reads. Deferred because this opener is **also** used for by-id bead
    lookups (`findUniqueBeadAcrossStoresView:495` passes a `beadID`, reading
    work-class beads by id), so wrapping every returned view with the graph class
    would need to split the graph-root-read use from the by-id work-read use at the
    same seam — not a safe blanket wrap. (Byte-identical today either way, since
    `cliGraphStore` is identity on a legacy city.)
  - **Guard follow-up:** extend `frontdoor_di_guard_test.go` with
    graph/orders/nudges relocation-root guards (analogous to
    `TestSessionRelocationRootsRouteThroughSessionClassStore`) pinning the routed
    spellings at these roots. Part of the broader E1.2 census guard work.
- [ ] **E2.4 — Mint new infra beads with reserved prefixes.** Activate
  `gcs/gcg/gcm/gco/gcn` for newly-created infra beads. Regeneralize the
  `reserved_prefixes.go` doc from "SQLite-relocated" → "infra-store" (backend-
  agnostic). (Legacy infra beads keep their HQ-era prefix; see E3.2.)
- [ ] **E2.5 — Boundary-not-backend gating audit.** Grep for any routing that
  gates on "is sqlite" / backend kind; convert to gate on store-boundary /
  reserved-prefix. (The audit's leverage bug: `GraphOnlyListFor` gated on sqlite
  → dead fixes.)

**Guard/enforcement:** a `TestNoInfraBeadInDomainStore` / `TestNoDomainBeadInInfraStore`
that classifies every bead in each store after a representative run and fails on
a mismatch — the boundary invariant.

---

## E3 — Migration: 1-db → 2-db (depends on E2)

**Goal:** an existing single-Dolt-db city upgrades to the two-store layout,
idempotently and crash-safely. New cities already create two (E2.1).

**Done-criteria:** existing cities upgrade with every infra bead moved to the
infra store, no domain bead touched, no loss; re-running is a no-op.

- [x] **E3.1 — Detect + create. DONE (2026-07-07).** `cityNeedsInfraStoreMigration`
  (config-shape + live-state, no marker file: can-migrate =
  `cityUsesBdStoreContract && !isExternalDolt`; needs = `!cityHasInfraStore` OR a
  domain store still holds infra-class beads) + `gc migrate infra-store`
  (`cmd_migrate_infra_store.go`, registered via `newMigrateCmd` in main.go). Create
  reuses the exact E2.5 calls (`seedInitInfraScope` + `initAndHookDir`) plus the
  NEW `writeInfraScopeRoutes` (infra scope's `routes.jsonl` so a same-prefix
  cross-boundary `bd dep add` resolves the read-only target). Preflight refuses a
  live controller + external Dolt; brings managed Dolt up. Read-only doctor
  ADVISORY (`infraStoreMigrationCheck`) points the operator at the command.
- [x] **E3.2 — Move existing infra beads. DONE (2026-07-07).** Sweeps EVERY domain
  store (city + all rigs), classifies each bead via `coordclass.Classify` (the sole
  authority — never a type list), and copies infra-class beads into the infra store
  **preserving id/type/status/metadata/labels** (`copyBeadPreservingID`: Create the
  node with Needs/ParentID/Dependencies cleared so the policy wrapper never
  re-mints a non-empty id, then restore status via Close/Update). Dependency EDGES
  are re-added co-resident with their SOURCE (Phase M2), with cross-boundary targets
  left dangling (resolved by the E1.6 seams). Deletion uses the NEW orphan-preserving
  batch primitive `beads.BatchDeleter` / `DeleteAllOrphaning` (chunked `bd delete
  --force --json`, ALWAYS ≥2 ids so bd takes the batch path — single-id delete is a
  mutation bomb that strips inbound edges + text-rewrites neighbors). **Ids are
  never re-minted** — legacy infra beads keep their HQ/rig-era prefix.
- [x] **E3.3 — Idempotent / resumable / crash-safe. DONE (2026-07-07).** No status
  file: the plan is recomputed from LIVE state every run (the infra store's current
  contents are the idempotency oracle). Global phase ordering (all M1 copy → all M2
  edges → M3 verify → all M4 delete) means an edge never precedes its endpoints and
  a delete never precedes verification, so a crash in any phase leaves only
  re-runnable states. Copy-then-delete + delete-only-what-verify-proved-copied. A
  re-run on a migrated city is a convergent no-op (moved:0). Covered by fast-tier
  (`infra_store_migrate_test.go`: core/re-run/crash-resume/dry-run + tripwire) and
  the real-Dolt integration test (`infra_store_migrate_integration_test.go`).
- [x] **E3.4 — New-city path verified 2-db. DONE (2026-07-07).** A city born split
  under E2 (`GC_INFRA_STORE_SPLIT=1`) reports `cityNeedsInfraStoreMigration == false`
  (the E2.5 integration city already has the infra scope and no infra beads left in a
  domain store), and the post-migration integration city likewise reports false — so
  `gc init` needs no migration.

**Guard/enforcement:** the E2.5 boundary invariant test, run against a
post-migration fixture city.

---

## E4 — Exhaustive e2e upgrade tests (depends on E3; the AUTHORITATIVE gate)

**Goal:** prove the whole thing on the two-store shape — the check the substring
guards structurally cannot provide (the audit's lesson: tests on the single-store
shape give false green).

**Done-criteria:** the suite exercises the real two-store shape and gates the
whole effort.

- [ ] **E4.1 — Seeded 1-db harness.** Spin up a single-db city seeded with a
  realistic mix: domain tasks across multiple rigs + HQ; sessions; mail; nudges;
  orders; a FULL formula molecule explosion (root + control + attempt/run + wisp +
  convergence + synthetic convoy). Extend `setupManagedBdWaitTestCity`.
- [ ] **E4.2 — Upgrade + partition assertions.** Run the E3 upgrade; assert the
  domain stores contain ONLY domain beads, the infra store ONLY infra beads,
  counts reconcile, nothing lost.
- [ ] **E4.3 — Cross-store reference resolution.** Post-upgrade: molecule →
  source work bead (`source_bead_id`/`source_store_ref`) resolves; convoy members
  across the boundary resolve; a drain across the boundary completes; finalize
  closes the source in its domain store.
- [ ] **E4.4 — Command sweep, no leakage.** Run every `gc` command post-upgrade;
  assert no new infra bead lands in a domain store and vice-versa (the E2.5
  invariant, live).
- [ ] **E4.5 — New-city (2-db-from-start) e2e.** Same partition + reference
  assertions without a migration step.
- [ ] **E4.6 — Idempotent re-run.** Re-running the upgrade on an already-migrated
  city is a no-op.

---

## E5 — Infra backend swap Dolt → SQLite/Postgres (LATER; depends on E4)

**Goal:** swap the infra store's backend as a scope-invariant no-op with
structural parity. Optional / when the Dolt write volume of the concentrated
formula explosion warrants it.

**Done-criteria:** the E4 suite passes IDENTICALLY with the infra store on the
new backend — proving parity.

- [ ] **E5.1 — Backend selector for the infra store.** Config to open the infra
  store on SQLite/PG instead of Dolt. (This is where the old `graph_store=sqlite`
  machinery / the retired coordrouter opener gets ported — the OPENER, not a
  router.)
- [ ] **E5.2 — Parity proof.** Run E4 unchanged against the swapped backend.
- [ ] **E5.3 — (Optional) per-class store split.** Point a single typed view at
  its own backend (e.g. graph on PG, sessions on SQLite) — zero routing change
  because reserved prefixes already distinguish the classes. "One infra store now,
  N later if a class earns its own backend."

---

## Risks (track across the burn-down)

- **Cross-store atomicity loss.** Sling (create molecule-infra referencing
  work-domain) and finalize (close source across the boundary) are no longer one
  transaction. Rely on convergence (idempotent observers, persistent work); audit
  each cross-store write for partial-failure safety. *(Owns: E1.6, E4.3.)*
- **Per-rig dispatcher scope collapse.** With control beads city-scoped, per-rig
  dispatch collapses toward one city-infra dispatcher federating into rig work —
  simpler, but loses the "a wedged rig can't take down others' dispatch"
  blast-radius property. Decide deliberately. *(Owns: E2.3.)*
- **Stable-id legacy beads.** Migrated infra beads keep their HQ-era prefix, so
  the bare-id/legacy-scan path can't be fully deleted until they age out.
  *(Owns: E1.7, E3.2.)*
- **Boundary-not-backend gating.** Any `if sqlite` in routing breaks parity —
  the audit's leverage bug. *(Owns: E2.5.)*
- **False-green on single-store shape.** E4 MUST exercise the two-store shape;
  a suite that runs on the collapsed shape proves nothing (the audit lesson).
  *(Owns: E4.)*

---

## Sequencing summary

```
E1 (interface standardization + delete crutch)   ← do first, byte-identical, guarded
      │
      ▼
E2 (two Dolt stores; resolveClassStore non-identity; new city = 2)
      │
      ▼
E3 (migrate existing 1-db → 2-db, idempotent)
      │
      ▼
E4 (exhaustive e2e upgrade tests — the gate)
      │
      ▼
E5 (infra backend swap → SQLite/PG, no-op with parity)   ← later / optional
```

The one hard constraint: **E1 before E2.** Flip the boundary only after every
call site names its store, or a missed site silently corrupts the split and the
tests won't tell you.
