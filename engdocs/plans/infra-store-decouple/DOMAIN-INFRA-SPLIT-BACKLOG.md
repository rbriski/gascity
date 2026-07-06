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

- [ ] **E1.1 — Deferred CLI entangled set.** Route the parked roots the
  relocation pass deferred: `cmd_wait.go`, `cmd_handoff.go` + `cmd_runtime_drain.go`
  (paired, share `sessionRestartableByController`/`clearRestartRequest`),
  `cmd_nudge.go`, `cmd_sling.go`, `cmd_start.go` reconcile cascade.
  *Extends:* `RELOCATION-ROUTING-HANDOFF.md` "Deferred" set.
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
  two-store split. Remaining E1.1: cmd_sling.go (clean-surgical per census — the
  doSlingNudge hub + deliverSlingNudge sessionFrontDoor@~1495), then the entangled
  cmd_nudge / cmd_wait / cmd_handoff+cmd_runtime_drain set.
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

- [ ] **E2.1 — Open the second Dolt store at city scope.** Add the city INFRA
  Dolt store alongside the domain (HQ/city) store; thread both through the
  controller (`cityDomainStore`/`cityInfraStore`) and the CLI. `gc init` creates
  BOTH.
- [ ] **E2.2 — Non-identity `resolveClassStore`.** `resolveClassStore(infra-class)
  → city infra store`; `resolveClassStore(work) → rig/HQ domain store`. Remove
  the identity stub (`cmd/gc/class_store.go:231`). Wire the 5 typed views over the
  one infra store. Blocked by: E2.1. (Pinned identity tests
  `TestControllerStateClassAccessorsAreIdentity` get updated to boundary tests.)
- [ ] **E2.3 — Uniform infra scope (kill the sling split-brain).** Today the CLI
  sling creates infra beads in the *rig* store while the controller reads them
  from the city store — which is why `coordClassStoreCandidates` fans infra across
  city+rigs. Route infra creation to the city infra store everywhere; collapse the
  infra fan-out to city-only. Blocked by: E2.2.
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

- [ ] **E3.1 — Detect + create.** Detect a single-db city; create the infra Dolt
  store.
- [ ] **E3.2 — Move existing infra beads.** Move the comingled (HQ-prefixed) infra
  beads into the infra store. **Do NOT re-mint ids** (they're stable references) —
  legacy infra beads keep their HQ-era prefix in the infra store; the bare-CLI-id
  lookup + a bounded legacy scan cover reads of those until they age out.
- [ ] **E3.3 — Idempotent / resumable / crash-safe.** Re-run picks up where it
  stopped; no double-move; convergent (the design's "system converges because work
  persists" property). No status file — discover state by querying the stores.
- [ ] **E3.4 — New-city path verified 2-db.** Confirm `gc init` (E2.1) needs no
  migration.

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
