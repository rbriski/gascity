# Session-class periphery closure plan (fresh-session handoff)

**Goal.** Drive every direct SESSION-bead reference behind the typed
`session.Info` / `session.Store` / `session.CircuitState` surface, so a per-class
session backend swap (`resolveSessionStore` + `[beads.classes.sessions]`,
`cmd/gc/class_store.go`) captures 100% of session access. This continues the
reconciler front-door work (Steps 1–6e, `RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md`)
onto the periphery.

**This is a real front-door continuation, not mechanical swaps.** Almost every
periphery site reads a field that *already exists on `session.Info`* but off the
raw bead — and its bead flows into a raw-bead HELPER that lacks an `Info`-form
sibling. So each conversion is: (1) build the `Info` sibling for the helper,
(2) flip the read, (3) guard the file, (4) fable-review for byte-identity. The
same discipline that governed the reconciler applies here.

> **Provenance / caveat.** The inventory below is a scout sweep (3 parallel
> Explore agents, 2026-07-05) plus spot-checks — **line numbers and counts are
> indicative, not audited**. Re-grep + verify each file's exact sites before
> editing it (files drift). The classification (session vs other-class,
> convertible vs raw-by-design) is the load-bearing part.

---

## Dependency ordering (do phases in this order)

```
Phase A  Info projection additions (additive, zero-risk foundation)
Phase B  Info-sibling helpers for the raw-bead helpers (per target file)
Phase C  cmd/gc periphery conversions  — small/util first, big decision files LAST
Phase D  internal/api session handlers
Phase E  internal/worker session reads
Phase F  internal/session OWN runtime/lifecycle (the package eating its own Info)
Guard    extend frontdoor_di_guard_test.go per file as it goes needle-clean
```

Rationale: helpers in `build_desired_state.go` are called from multiple files, so
their `Info` siblings must land before dependents. Big decision files
(`build_desired_state.go` ~4520 lines, `city_runtime.go` ~3477) are the riskiest —
convert them last, after the pattern + siblings are proven on small files.

---

## Phase A — `session.Info` field additions (additive)

Confirm each is absent, then add verbatim mirror + `InfoFromPersistedBead` wiring +
`info_apply_patch.go` + the codec oracle case (the 6a precedent). Some may already
exist — grep `internal/session/manager.go` `type Info struct` first.

- `provider_kind` (worker/invocation_telemetry.go:122) — verify vs existing `Provider`.
- `MetadataKeyInvocationUsageCursor` (invocation_telemetry.go:143).
- `beadmeta.ActiveWorkBeadMetadataKey` = `gc.active_work_bead` (invocation_telemetry.go:213).
- `real_world_app_session_kind`, `worker_profile` (worker/factory.go:154-155).
(Others — `last_woke_at`, `session_key`, `state`, `session_name`, `alias`,
`template`, `agent_name`, `provider`, `transport`, `mcp_servers_snapshot`,
`continuation_epoch`, `configured_named_*`, `pool_*`, `sleep_reason`,
`started_config_hash` — already on `Info`; use them.)

---

## Phase C — cmd/gc periphery (the bulk; ~30 files, ~120 sites)

**Tier 4 — small/util, low risk, do FIRST (each: convert reads → Info, guard):**
`cmd_prime.go`, `cmd_session_logs.go`, `cmd_session_wake.go`, `cmd_skill.go`,
`doctor_session_model.go`, `mcp_integration.go`, `session_index.go`,
`session_origin.go`, `session_resolve.go`, `session_state_helpers.go`,
`session_template_start.go`, `usage_compute.go`, `assigned_work_scope.go`,
`adoption_barrier.go`, `pool_session_name.go`, `pool_desired_state.go`.

**Tier 2 — medium (session lifecycle/CLI):**
- `soft_reload.go` (203 ln): `.Open()`@103 + session_name/started_config_hash reads;
  helpers needing `Info` siblings: `sessionCoreConfigForHash(beads.Bead)` (session_hash.go),
  `clearSoftReloadConfigDriftDrainAck(beads.Bead)`.
- `cmd_start.go` (1529 ln): `.Open()`@904/918 feed
  `releaseOrphanedPoolAssignmentsWhenSnapshotsComplete([]beads.Bead)`
  (pool_session_name.go:108 — needs Info form). Note: already uses `OpenInfos()`@922.
- `cmd_session.go` (2541 ln): state/session_name reads (~1354/2313/2321/2325);
  verify 1354 is session vs work bead.

**Tier 1 — CRITICAL, big decision files, do LAST:**
- `build_desired_state.go` (~4520 ln): `.FindByID`@2197 + 4×`.Open()`
  (3408/3637/3883/4232) + ~21 metadata cracks. Helpers needing `Info` siblings:
  `poolRuntimeAliasIsDeferred`, `canonicalSessionIdentity[WithConfig]`,
  `sessionBeadQualifiedName`, `claimPoolSlotWithConfig`,
  `controllerDemandRouteTarget/Candidates`, `openControlDispatcherDemand`
  (`staleNonExpandingPoolSessionBead` already has an Info mirror @~2995).
- `city_runtime.go` (~3477 ln): 4×`.Open()` + 2×`.FindByID` + ~7 cracks. Helpers:
  `poolSessionBeadRuntimeRunning`, `pendingCreateClaimStillLeasedForSweep`,
  `isStaleCreating` → `isStaleCreatingInfo`, `filterSessionBeadsByName` →
  `filterSessionInfosByName`.
- `cmd_nudge.go` (~2460 ln): `resolveNudgeTargetFromSessionBead(...beads.Bead)` @1121-1135
  reads session_name/alias/agent_name/template → build `...FromSessionInfo`; verify @1503
  (session vs wait). (The `nudge_id` reads elsewhere are wait/mail cross-refs — not session.)

**WAIT-CLASS caveat (`cmd_wait.go`, ~1459 ln):** MOST `.Metadata[` there are on WAIT
beads (Type "wait": session_id/state/kind/dep_ids/nudge_id/registered_epoch) — those
STAY (wait is a separate future class). Only the SESSION-bead reads convert:
`.FindByID`@1164 + `sessionBead.Metadata` in `cachedSessionCanReceiveWaitNudge`/
`waitNudgeProviderNeedsPoller`/`waitNudgeAgent`/`sessionProviderFamily` (each needs
an `Info` sibling). Split carefully.

---

## Phase D — internal/api session handlers (~8 files, ~16 store.Get + ~18 cracks)

Biggest offenders (mutation sites — convert the reads, keep the lifecycle calls):
- `huma_handlers_sessions_command.go` (~967 ln): store.Get@419/869/926 →
  session.WakeSession/TerminateSession/UpdatePresentation; `agent_name`@433 (ownership
  gate), `session_name`@890 (ClearCrashHistory).
- `handler_sessions.go` (~815 ln): store.Get@469/740; `session_name`@495 (ClearCrashHistory),
  `agent_name`@760 (alias-mutation gate).
- `session_resolution.go` (~680 ln): `session_name`@166 (worker `handle.Kill`), `state`@435;
  store.Get@565. (Note: `session_resolution.go` still calls
  `mgr.CreateAliasedNamedWithTransportAndMetadata` per the worker-boundary migration — leave that.)
- `huma_handlers_sessions_query.go`@296 (`state=="creating"` fast-path), `session_runtime.go`@222
  (`getSessionMetadata` returns the raw dict — audit consumers), `handler_status.go`,
  `handler_beads.go`, `handler_mail.go` (read-only session_name/alias for routing/search).
Route session reads through `session.Info` (many handlers already use
`mgr.GetWithPersistedResponse()` — extend that). Read `engdocs/architecture/api-control-plane.md`
before touching internal/api.

---

## Phase E — internal/worker (few sites)

- `factory.go`:154-155 `real_world_app_session_kind` / `worker_profile` → `Info` (needs Phase A).
- `invocation_telemetry.go`:122/143/213/324/328 — `provider_kind`/usage-cursor/active-work-bead
  (Phase A) + `last_woke_at`/`session_key` (already on Info; flip source).
- `handle_construct.go`:32-38 — session_origin/worker_profile WRITES at the construction
  boundary = **RAW-BY-DESIGN** (the spec builder); leave.

---

## Phase F — internal/session own runtime/lifecycle (riskiest; the package doesn't dogfood Info)

Significant category: the session package's OWN code cracks raw metadata instead of
using `Info`. Highest value (hot lifecycle paths) AND highest risk.
- `manager.go`: runtime transport detection (`transportForBead` ~451-463), session-name
  detach/reattach overlay (727-749), overlay-apply loop (836), close-path clears (1221-1224),
  scattered state reads. **RAW-BY-DESIGN: the Create-path bead construction (~668-699) + the
  `Info` struct itself — leave.**
- `chat.go`: resume/start/transcript metadata reads+writes (154-156 stale-resume clear, 169-340,
  955-1049). Lifecycle-critical — careful.
- `named_config.go`: `IsNamedSessionBead`/`NamedSessionIdentity`/`...Mode`/`NamedSessionBeadMatchesSpec`/
  continuity checks (163-628) read raw; all fields on Info. Used in reconciler repair paths.
- `names.go`: Create/Alias collision checks (361-616) read raw session_name/alias/state/pool/etc.
- `submit.go`: message-submit flow reads (105-561).
- **RAW-BY-DESIGN (leave):** `info_store.go` `InfoFromPersistedBead` + `sessionMatchesFilters`
  (the codec), `store.go` facades (`CircuitResetGeneration`, `PersistedMarkers`).

---

## Guard extension (per file, as it goes clean)

`cmd/gc/frontdoor_di_guard_test.go`: add each converted file to `snapshotInfoOnlyFiles`
(no raw `.Open()`/`.FindByID(`) and/or `metadataInfoOnlyFiles` (no `.Metadata[`).
Revert-canary each. Files still holding a raw session bead for a raw-by-design consumer
(start execution, codec, constructor) stay off the lists — document them as census.
`internal/api`/`internal/session`/`internal/worker` are different packages; either extend
the guard's file resolution to those dirs or add sibling guards there.

## Discipline (unchanged)

Verified per-file census → build `Info` sibling(s) → flip reads → build · `go vet` ·
`golangci-lint 0` · `gofmt` · targeted tests → guard entry + revert-canary → **fable
adversarial byte-identity review (0 findings bar)** → commit + push `--no-verify`
(trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`).
`[beads.classes.sessions]` relocation is the end-to-end acceptance test.

## Scale estimate

cmd/gc ~30 files / ~120 sites + ~8 Info-sibling helpers; internal/api ~8 files; worker
~2; internal/session ~6 files (the dogfood gap). This is **multiple focused sessions** —
sequence by the phase order, small→big, guarding as you go. The two `build_desired_state.go`
/ `city_runtime.go` conversions each warrant their own session (reconciler-grade care).

---

## SCOPE DECISION (2026-07-05): shape-first, access as a tracked pass

Owner call: seal each periphery file in **two separate passes**, not both at once.

- **Shape pass (this pass).** Route every raw session-bead field read through the
  `session.Info` codec (`InfoFromPersistedBead(bead).<Field>` / typed siblings) and
  add the file to `metadataInfoOnlyFiles`. This is backend-shape-invariant hygiene.
- **Access pass (separate, later).** Route the bead *LOAD* through
  `sessionsBeadStore()` / `resolveSessionStore` so a `[beads.classes.sessions]`
  relocation actually captures it; that is the `frontDoorStoreFreeFiles` boundary.

**Membership in `metadataInfoOnlyFiles` is SHAPE-SEALED, NOT relocation-safe.** A file
is only captured by the swap once BOTH passes close. The guard's doc comment states
this. Shape-first is the correct order because `session.Store.Get` returns `Info` (not
a raw bead), so a file must be shape-converted before its load can route through the
Info front door; files still needing the raw bead route their load through
`sessionsBeadStore()` (typed `beads.SessionStore`) in the access pass.

## Progress log

**Session 2026-07-05 (CONT-35) — Phase A + 8 Tier-4 files shape-sealed.** All verified
per-file (build/vet/gofmt/golangci-lint 0 + guard + revert-canary + targeted tests +
a fable adversarial byte-identity review, 0 findings each). Commits on
`upstream/object-front-doors-cleanup` (#3839 DRAFT):

- `1e1a80138` **Phase A**: added `Info.ProviderKind` (real persisted `provider_kind`
  family key, was MISSING) — full 6a wiring (struct + codec + ApplyPatch + oracle).
  Unblocks the logs/mcp/worker paths. Other census "MISSING" flags were wrong
  (`session_origin`/`pool_slot`/`pool_managed`/`generation`/`instance_token`/
  `sleep_reason` already on Info — re-verify census claims against the struct).
- `d3bc67ee3` session_template_start.go, adoption_barrier.go, cmd_prime.go, cmd_skill.go
- `d4b8bb88e` session_resolve.go (+ Info-sibling helper calls isNamedSessionInfo/…)
- `1fbcb7728` cmd_session_logs.go, mcp_integration.go (ProviderKind consumers;
  `sessionLogFallbackCandidateLive` signature → Info)
- `b5fb81b51` session_index.go (+ deleted dead `pool_template` field per no-ghosts)
- `6f60e2c4d` cmd_session_wake.go (two local helpers → Info form)
- `ccb793ef8` **soft_reload.go — FIRST FULLY-SEALED file** (Tier-2 + Phase B): on all
  three guard lists (store-free + snapshot-info-only + metadata-info-only) = shape AND
  access sealed. Added 3 additive Info-form helper wrappers (bead forms delegate,
  byte-identical, big-file callers untouched) that also unblock future big-file work:
  `sessionCoreConfigForHashInfo`, `applyTemplateOverridesToConfigInfo`,
  `cancelSessionConfigDriftDrainInfo`. `Open()`→`OpenInfos()` (lockstep-identical).

**9 files on `metadataInfoOnlyFiles` (shape-sealed) + soft_reload.go on ALL THREE lists
(fully sealed):** session_template_start, adoption_barrier, cmd_prime, cmd_skill,
session_resolve, cmd_session_logs, mcp_integration, session_index, cmd_session_wake,
soft_reload. Verified census artifact: `raw/session-tier4-census.json`.

**Remaining Tier-4 re-classified after direct inspection (all DEFER — not clean
this-pass wins):** `session_origin.go` = bead-form helper library whose bead forms are
STILL called by build_desired_state.go (×5) + session_reconcile.go + session_beads.go +
the classifier-equivalence oracle test → convert WITH the big files. `pool_desired_state.go`
`poolSessionConsumesNewDemand` is NOT dead — it's the oracle-equivalence reference (census
was wrong) → stays. `usage_compute.go` reads via a `meta := bead.Metadata` alias (bypasses
the `.Metadata[` guard) + needs Phase A for awake_started_at/slept_at/usage_compute_emitted_at
+ ResolveRunID run-chain keys → own effort. `pool_session_name.go`/`doctor_session_model.go`
= mixed (work/wait/opts `.Metadata[`) → session reads convertible but file stays OFF the
substring guard.

**KEY LESSON — clean Tier-4 criterion (sharper than the census):** a file is a clean
this-pass target only when its raw reads are on a bead **the function loaded itself**
(no external signature change). Files whose `.Metadata[` lives in a helper that takes a
`beads.Bead` **parameter** (assigned_work_scope's `sessionAgentConfig`/
`openSessionReachableStoreRef`, the `session_state_helpers.go` bead-form library) are
the `session_state_helpers` trap — their callers (the big decision files) pass raw beads,
so converting drags them in. Defer those with their callers. Also: a file is
guard-listable only if converting clears **all** its `.Metadata[` — files that also read
work/wait metadata (doctor_session_model's `routed_to`, pool_session_name,
pool_desired_state) get shape-converted for their session reads but stay OFF the
substring guard (documented census).

**Remaining Tier-4 (next):** doctor_session_model (mixed, no guard), usage_compute
(needs bookkeeping-key Phase A + work refs), session_origin (bead-form helper library),
pool_session_name / pool_desired_state (mixed + a dead `poolSessionConsumesNewDemand`
legacy helper to delete). Then Tier-2 (soft_reload/cmd_start/cmd_session), then the
Tier-1 giants.

---

**Session 2026-07-05 (CONT-37) — cmd_session.go fully shape-sealed + the
guard-earning shape pass is now EXHAUSTED in cmd/gc.** Commits on
`upstream/object-front-doors-cleanup` (#3839 DRAFT):

- `13a8a1731` **Phase A**: `Info.DependencyOnlyMetadata` raw mirror (the
  pin-awake path compares `dependency_only` UNTRIMMED, which the trimmed
  `DependencyOnly` bool cannot reproduce). Wired on both codec + apply-patch
  paths keyed on the same value, so `TestInfoApplyPatchMatchesReprojection`
  covers it automatically; explicit `TestDependencyOnlyMetadataIsVerbatim`
  mirrors the `PendingCreateClaimMetadata` precedent.
- `31cdf48a2` **cmd_session.go shape-sealed** (now on `metadataInfoOnlyFiles`;
  zero `.Metadata[` of any spelling). Two moves made it guard-eligible:
  (1) relocated `readyWaitSetForList` to `cmd_wait.go` (byte-identical body,
  already tested in `cmd_wait_test.go`) — it reads WAIT beads, a separate
  class, so it belongs with the wait loaders and its two residual wait
  `.Metadata[` (state/session_id) leave the file; (2) converted the three
  session helpers `pinAwakeWakeReasonVisible` / `sessionKillRuntimeAlreadyInactive`
  / `recordSessionKillStop` to the Info form (raw mirrors: `MetadataState` not
  the blanked `State`; `SessionNameMetadata` not the `s-<ID>`-fallback
  `SessionName`; `DependencyOnlyMetadata` for the untrimmed compare; siblings
  `sessionMetadataStateInfo`/`isDrainedSessionInfo`/`normalizedSessionTemplateInfo`/
  `sessionAgentMetricIdentityInfo`). Full gates (gofmt/build/vet/golangci-lint 0
  + guard + revert-canary + targeted tests + a fable adversarial byte-identity
  review, 11/11 confirmed, could-not-refute).

**STRATEGIC FINDING (settles the target question for the rest of the shape
pass).** A 4-agent census (wf_50fbaa2e-285) + direct inspection proves the
**guard-earning shape targets in `cmd/gc` are now EXHAUSTED**. A file joins
`metadataInfoOnlyFiles` only if converting clears EVERY `.Metadata[` spelling;
the remaining candidates each retain a permanent non-session `.Metadata[`:

- **build_desired_state.go** (75 `.Metadata[`): permanent WORK-bead reads
  (`wb.Metadata[RoutedToMetadataKey]`, `step.Metadata`, `root.Metadata`) AND
  session-bead WRITES (`sessionBead.Metadata[key]=value` ×4) → never guard-
  eligible; shape-value only, contained to one file, OWN-SESSION.
- **city_runtime.go** (12 session `.Metadata[`, all clean): blocked by the
  raw-by-design whole-map fingerprint `sessionBeadSnapshotFingerprint` (must
  hash the full metadata map) + a raw sweep close, plus 6 `.Open()` handing
  into 5 cross-file `[]beads.Bead` reconciler helpers with no Info siblings
  (`releaseOrphanedPoolAssignmentsWhenSnapshotsComplete`, `emitDueComputeFacts`,
  …). Orchestration hub, not a leaf → never guard-eligible; OWN-SESSION.
- **session_origin.go** (10 `.Metadata[`): permanently raw-by-design — all
  reads live in the bead-form classifier helpers the `TestSessionClassifierInfoEquivalence`
  oracle requires (its Info siblings already all exist). Same excluded family
  as session_reconcile.go / session_sleep.go. Nothing to convert.
- **cmd_start.go**: only `.Open()` (snapshot), but both feed
  `reconcileSessionBeadsAtPathWithNamedDemand` (a core reconciler entry taking
  raw `[]beads.Bead`) → library trap; DEFER with the reconciler.

The two remaining paths, both larger initiatives with no quick guard win:
1. **Access-pass DI** (the owner-deferred "separate, later"): route the 9
   shape-sealed files' LOADS through `sessionsBeadStore()`/`sessionFrontDoor`
   and make each store-free (`frontDoorStoreFreeFiles` forbids even holding
   `beads.SessionStore` / calling `sessionFrontDoor(` — the composition root
   threads in `*session.Store`). This is the ONLY remaining guard-earning +
   relocation-completing work, but it is a package-wide DI refactor (many
   cross-file call sites; `session.Store.Get` returns Info) — multi-session.
2. **Shape-value-only** conversions of the giants (drive their session reads
   behind Info, no guard entry) — prep for the eventual full seal; lower
   priority per the guard-eligibility lesson.

---

**Session 2026-07-05 (CONT-38) — ACCESS-PASS DI STARTED (owner-authorized).
Batch 1: 3 leaf files access-sealed onto the session front door.** Commit
`d7d0aa56b` on `upstream/object-front-doors-cleanup` (#3839 DRAFT). Added to
`frontDoorStoreFreeFiles`: **adoption_barrier.go, session_index.go,
mcp_integration.go** (now 5 total with session_circuit_breaker + soft_reload).

**THE PROVEN ACCESS-PASS PATTERN (byte-identical):** a file goes store-free by
taking `sessFront *session.Store` and reaching the raw session-class store via
`sessFront.Store().Store` (the soft_reload.go model — a method+field chain that
does NOT contain the forbidden `beads.Store` substring). Use the REACH-THROUGH,
**not** the typed `sessFront.Get(id)` — `session.Store.Get` (info_store.go:178)
adds an `IsSessionBeadOrRepairable` validation and re-wraps the error, so it is
NOT byte-identical to `store.Get` + `InfoFromPersistedBead`. Reintroduce a local
`store := sessFront.Store().Store` right after the guard so the rest of the body
stays byte-identical. Nil-guard: `if store == nil` → `if !sessFront.Backed()`
(Backed = `s != nil && s.store.Store != nil`, faithful to raw-store-nil). The
composition root (a non-listed file) constructs `sessionFrontDoor(store)`; tests
are unguarded and keep using `sessionFrontDoor(store)`. Per file: build/vet/
golangci-lint 0 + the `TestFrontDoorStoreFreeFilesStayStoreFree` guard +
revert-canary (inject a `beads.Store` decl, guard must fail) + a fable
adversarial behavior-identity review.

**REMAINING ACCESS-PASS FILES (survey wf_5bac5e83-758, tractability-ranked):**
- **MEDIUM** — do next, in this order:
  - `cmd_prime.go` (1 `beads.Store` literal; one raw-bead escape; root-in-file →
    needs an UNLISTED composition helper e.g. `primeSessionFrontDoor(cityPath)
    (*session.Store, error)` wrapping `openCityStoreAt`+`sessionFrontDoor`, since
    `sessionFrontDoor(` can't appear in the listed file).
  - `cmd_skill.go` (3 literals incl a `var store beads.Store` decl; raw-bead
    escape `normalizedSessionTemplate`→ Info sibling `normalizedSessionTemplateInfo`
    exists; 0 non-test caller ripple; root-in-file).
  - `cmd_session_logs.go` (4 store-param sigs, all in-file; `store.ListByMetadata`
    + the 2 shared callees `resolveSessionIDAllowClosedWithConfig` /
    `workerHandleForSessionWithConfig` via reach-through — no session-pkg escapes).
  - `session_template_start.go` (raw-bead escape via `RepairEmptyType`).
  - `session_resolve.go` (the SHARED `resolveSessionID*(...,store,...)` resolver —
    dependency of mcp/skill/logs; those files reach through it today, so converting
    it is not required for them, but doing so lets them drop the reach-through).
- **HARD-RIPPLE** — defer, each own session:
  - `cmd_session_wake.go` (3 raw-bead escapes on the WAKE bead: `WakeSession`,
    `RepairEmptyType`, `IsSessionBeadOrRepairable` — the raw bead can't become Info;
    would want `session.Store` wake/repair methods or reach-through the whole wake).
  - `cmd_session.go` (LARGEST: ~9 in-file RunE composition roots each opening a
    store; a `rigStores map[string]beads.Store` CROSS-CLASS map the session
    reach-through does NOT cover; 3 raw-bead escapes; ~30 sites).

**Cross-cutting:** the shared `beads.Store`-typed callees (`resolveSessionID*`,
`workerHandleForSessionWithConfig`, `workerSessionCatalogWithConfig`,
`session.ListAllSessionBeads` with rich ListQuery, `session.EnsureAlias*`) do NOT
need converting — pass them `sessFront.Store().Store` (reach-through) and the
dependent file still goes store-free. The one thing reach-through can't fix is
cmd_session.go's `map[string]beads.Store` rigStores (multi-class rig map, its own
ownership boundary).

---

**Session 2026-07-05 (CONT-39) — ACCESS-PASS batch 2: the MEDIUM tranche, via
SRP split (owner call over guard-gaming).** Commit `2fd4cbc5a`. `frontDoorStoreFreeFiles`
now **7**: session_circuit_breaker, soft_reload, adoption_barrier, session_index,
mcp_integration, **skill_visibility, session_logs_resolve**.

**KEY ARCHITECTURAL FINDING that reshaped the MEDIUM tranche:** the 5 survey-"MEDIUM"
files are NOT clean store-free receivers like the 3 leaves. Two kinds:
- **Composition ROOTS** (open the store in their own RunE): cmd_prime, cmd_skill,
  cmd_session_logs. The guard doc says roots that construct the front door inline are
  "intentionally not listed." The owner chose the **SRP split** (not a composition-factory
  that would game the guard): move each root's session-store LEAF helpers (pure receivers)
  into a store-free companion file; the root stays and constructs `sessionFrontDoor(store)`
  at the call site. Done for cmd_skill (→ `skill_visibility.go`) and cmd_session_logs
  (→ `session_logs_resolve.go`). **cmd_prime EXCLUDED** — its hook helpers open the store
  from `cityPath` internally (no receiver leaf to split); it is a genuine root.
- **Shared SPINE** (raw-store infra, like `workerHandleForSessionWithConfig`):
  **session_resolve.go EXCLUDED** (the `resolveSessionID*` resolver, 20 non-test callers
  across 13 files — converting it is a large ripple that entangles the HARD files, and
  dependents already reach through it); **session_template_start.go EXCLUDED** (the
  creation spine session_resolve drives). Both legitimately keep a raw store.

**SRP-split mechanics (the pattern for future root files):** move the receiver leaf funcs
verbatim into a new companion file, convert `store beads.Store`→`sessFront *session.Store`,
reintroduce `store := sessFront.Store().Store` after the guard so bodies stay byte-identical,
`if store==nil`→`if !sessFront.Backed()`, forward `sessFront` to sibling moved funcs and
`sessFront.Store().Store` to unconverted callees (workerHandle/resolveSessionID/ListByMetadata).
Root's RunE passes `sessionFrontDoor(store)`; prune the root's now-unused imports; wrap test
call sites in `sessionFrontDoor(store)`; add the companion (not the root) to the guard list.

**NOTE (future guard tightening):** the reach-through `sessFront.Store().Store` puts a
`beads.Store`-typed local back in scope that the `beads.Store` substring needle can't see
(true of soft_reload/adoption_barrier/mcp too). A stricter guard could forbid `.Store().Store`
in listed files; today it's the sanctioned byte-identity escape hatch.

**REMAINING = the HARD-RIPPLE tranche (own session; see
`ACCESS-PASS-HARD-RIPPLE-HANDOFF.md` + `-NEXT-SESSION-PROMPT.md`):** cmd_session_wake.go
(3 raw-bead escapes on the WAKE bead) and cmd_session.go (~9 in-file RunE roots +
`rigStores map[string]beads.Store` cross-class + 3 escapes + ~30 sites). Plus the
documented EXCLUSIONS (cmd_prime root; session_resolve/session_template_start spine) which
are relocation-safe via reach-through by their dependents and need no store-free listing.

---

**Session 2026-07-05 (CONT-40) — ACCESS PASS PIVOTED to RELOCATION-ROUTING (owner
call); 10 CLI roots routed, 8 commits pushed.** Full detail:
`RELOCATION-ROUTING-{HANDOFF,NEXT-SESSION-PROMPT}.md`. Commits
`0aa51fafd..3e05a03fe` on `upstream/object-front-doors-cleanup` (#3839 DRAFT).

**THE PIVOT.** The store-free DI guard (`frontDoorStoreFreeFiles`) is compile-time
hygiene and is ORTHOGONAL to the mission for CLI command roots. The mission is
relocation-safety: a `[beads.classes.sessions]` swap must capture 100% of session
access. Grounding finding: the controller/runtime is ALREADY safe (routes via
`cr.sessionsBeadStore()` → `resolveSessionStore`), but the CLI one-shot roots are
relocation-BLIND — they do `sessionFrontDoor(openCityStore(...))` where `openCityStore`
(main.go:1073) returns the GENERIC work store, so after a relocation their session
reads/writes hit the wrong backend (split-brain). Fix is byte-identical today
(`resolveClassStore` is pure identity). Owner authorized the pivot.

**THE SEAM (landed, `cmd/gc/cli_session_store.go`):** `cliSessionStore(store,cfg,cityPath)
= resolveSessionStore(store,cfg,cityPath,nil)` and `cliSessionFrontDoor(...) =
sessionFrontDoor(cliSessionStore(...))`. Byte-identical today; diverges only under a
configured relocation. Recorder nil (CLI one-shots have no live event bus). Patterns:
WHOLE-STORE (all consumers session-class → `sessStore := cliSessionStore(...)`, replace all
`store`) vs SURGICAL (multi-class → route only session consumers). cfg-less/hot/hook/daemon
paths load cfg via `loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)` (NOT
`loadCityConfig` — pack-refresh side effect; owner-decided for cmd_prime).

**THE GUARD (`frontdoor_di_guard_test.go`):** `TestSessionRelocationRootsRouteThroughSessionClassStore`
over `sessionRelocationRoutedFiles` — forbids `sessionFrontDoor(store|store.Store|openCityStore`
and requires `cliSessionStore(`/`cliSessionFrontDoor(` present. Regression canary, NOT a
completeness proof (can't see `store.Get`/`resolveSessionID*` non-front-door reads). Mixed files
(controller.go, cmd_start.go) and `cli_session_store.go` are intentionally OFF the list.

**DONE (10 roots, each gofmt·build·vet·golangci-lint 0·tests·revert-canary·FABLE byte-identity
review [all could-not-refute]):** cmd_session_wake, cmd_session_pin, cmd_skill, cmd_mcp,
cmd_session_logs, cmd_prime (both hook helpers, no-refresh cfg), cmd_stop (whole-store, all 5
consumers verified session-class), cmd_start (adoption barrier only — cascade deferred),
cmd_session_reset (whole-store), controller.go (circuit-reset socket, no-refresh cfg).
`sessionRelocationRoutedFiles` (8): wake, pin, skill, mcp, session_logs, prime, stop, session_reset.

**COMPLETENESS CENSUS (this session, Explore sweep):** the ORIGINAL census only grepped DIRECT
`sessionFrontDoor` sites and MISSED roots reaching session state via HELPERS (that's how
cmd_session_reset + cmd_runtime_drain surfaced). REMAINING blind roots:
- **Phase 4 cmd_session.go** (BIG, own session): ~9 roots; cmdSessionClose/Kill multi-class
  (rigStores=WORK, leave). Verify each root's consumers per-consumer (prior classifications
  proved UNRELIABLE — the plan was wrong about cmd_stop's consumers).
- **NEW (plan never listed):** cmd_restart.go `doRigRestart` (session-name+runtime pattern like
  cmd_stop — likely clean whole-store); cmd_mail.go (12 subcommands, mail+session addressing,
  surgical); cmd_status.go→city_status_snapshot.go (`resolveSessionIDWithConfig`); completion.go
  `loadSessionsForCompletion`.
- **DEFERRED (entangled, own efforts):** cmd_handoff.go + cmd_runtime_drain.go (PAIRED — share
  `sessionRestartableByController`/`clearRestartRequest`; doHandoffWithOutcome mixes mail+session,
  ~10 test sites); cmd_wait.go (owner-approved defer — multi-class machinery shared with the
  reconciler); cmd_nudge.go/cmd_sling.go (NUDGES-class front doors); cmd_start.go reconcile cascade.
- **NON-SESSION (safe):** cmd_prompt, cmd_start_warmup, dispatch_runtime, providers.

**Phase 6 (TODO):** end-to-end `[beads.classes.sessions]` relocation acceptance test — the
authoritative check the substring guard cannot provide.

**Session 2026-07-06 (CONT-41) — 3 more CLI roots routed (11 total), 3 commits pushed.**
Detail in `RELOCATION-ROUTING-{HANDOFF,NEXT-SESSION-PROMPT}.md`. Commits
`ba530c91f` (cmd_restart), `d9486309c` (completion), `6d432a0d3` (providers) on
`upstream/object-front-doors-cleanup` (#3839 DRAFT). HEAD `6d432a0d3`.

Each root followed the full discipline: verified per-consumer census (re-grepped, did NOT
trust prior classifications) → whole-store/surgical route → gofmt · build · vet ·
golangci-lint 0 · targeted guard+root tests → revert-canary (guard fails naming the file via
the positive `cliSessionStore(` tripwire) → FABLE adversarial byte-identity review before commit.

- **cmd_restart.go** `doRigRestart` — whole-store route at the caller `cmdRigRestart` (it holds
  cityPath+cfg; store is used only to hand to doRigRestart, so the signature + ~15 test callers
  stay untouched). All 5 consumers verified session-class. Fable: COULD-NOT-REFUTE (both identity-today
  and whole-store session-class-completeness).
- **completion.go** `loadSessionsForCompletion` — whole-store route (ListAllSessionBeads + the session
  catalog); already on the no-refresh cfg loader (hot path). Fable: COULD-NOT-REFUTE both.
- **providers.go** `loadProviderSessionSnapshot` — surgical route (opens its own store → fixes CLI +
  controller provider-construction at once). CORRECTS the earlier "providers.go = NON-SESSION safe"
  census line. **PARTIAL:** fable refuted the FIRST-DRAFT rationale (not the code) — `openCityMailProvider`'s
  beadmail provider is NOT pure mail-class; it also does session reads+WRITES on its store
  (ListAllSessionBeads/ResolveSessionID/RepairEmptyType, beadmail.go:79,91,187,760,799). Left un-routed as
  the deferred two-store-mail follow-up (resolveMailMessagesStore); recorded in the guard-list doc and an
  in-code note at openCityMailProvider. This beadmail layer — shared by all 12 cmd_mail subcommands — is the
  right place to route mail's session reads (once), not per-subcommand.

Two-perspective note on providers.go: a perfectionist could argue a partially-routed file shouldn't sit on
`sessionRelocationRoutedFiles` (its doc says "session access routed"); a pragmatist keeps it there because the
positive `cliSessionStore(` tripwire gives real, false-positive-free regression protection for the one route
that IS done. Chose pragmatist + an explicit PARTIAL caveat in the doc. Revisit if the file accretes more
un-routed session reads.

**Session 2026-07-06 (CONT-42) — cmd_session.go DONE: all 10 gc session command roots routed (12th
routed file), 1 commit `593310fe2`.** The big Phase 4 target. Census ran as a 10-agent Workflow fan-out
(one agent per root, tracing every store consumer to its leaf bead-class), then I ground-truthed the two
flagged roots myself and ran a single fable byte-identity review over the whole diff (COULD-NOT-REFUTE on
all 4 claims: identity-today, no-missed/over-routing, the prune scope addition, zero residual).

- **9 whole-store roots:** cmdSessionNew (11 consumers across the reconciler-first + fallback paths, incl.
  3 sessionFrontDoor front doors + alias/session-name availability checks — used replace_all for the
  New-only function-anchored patterns + indent-distinct edits for the two multi-line handle-call store args),
  doSessionListFallback (goroutine captures the routed store; readyWaitSetForList→gc:wait is ClassSessions),
  cmdSessionAttach, cmdSessionSuspend (incl. held_until ApplyPatch), cmdSessionRename, cmdSessionPrune,
  doSessionPeekFallback, cmdSessionKill, cmdSessionSubmit.
- **1 surgical:** cmdSessionClose — routes its 3 session consumers but keeps
  unclaimWorkAssignedToRetiredSessionBead on the plain WORK store (beads.WorkStore assignment beads,
  skips session beads).
- **Corrected the plan's guesses:** the old census lumped close+kill as multi-class; the fan-out proved
  KILL is whole-store (no work-release) — only CLOSE has it. "Prior classifications proved unreliable"
  held again.
- **cmdSessionPrune** was cfg-less: hoisted resolveCity + loadCityConfigWithoutBuiltinPackRefresh (no
  pack-refresh side effect; the fable pass confirmed the applyFeatureFlags global write is already applied
  two lines later by newSessionProvider and has no reader on the prune path), reused cityPath for the
  existing withdraw-nudges block (no extra resolveCity call).
- **Bonus doc fix (fable-found):** cmd_wait.go's comment called wait beads "a separate coordination class
  from session beads" — false; coordclass maps gc:wait → ClassSessions. Corrected, since the routing relies
  on it.
- **Revert-canary** fired on BOTH the sessionFrontDoor(store) needle AND the cliSessionStore( tripwire
  (stronger than prior files); ran non-destructively via `git stash push -- cmd/gc/cmd_session.go` keeping
  the guard-list entry, then `git stash pop`.

Technique note for big multi-root files: a Workflow census fan-out (one agent per root, structured schema
classifying each store consumer to its leaf bead-class) is the right tool — it parallelizes the read-heavy
analytical work while the edits + gates + canary + single whole-file fable review stay sequential in the
main context.

**Session 2026-07-06 (CONT-43) — gc status trio DONE (13th/14th/15th routed files), 1 commit
`b7e359895`.** Detail in `RELOCATION-ROUTING-{HANDOFF,NEXT-SESSION-PROMPT}.md`. The `gc status`
(cmd_citystatus.go + city_status_snapshot.go) and `gc rig status` (cmd_status.go) roots open a
generic work store used for a MIX of session reads and one store-health read. SURGICAL/multi-class:

- **SESSION (routed → cliSessionStore):** `loadStatusSessionSnapshot` → `ListAllSessionBeads` (5 call
  sites: cmd_citystatus ×4, cmd_status ×1, plus the `collectCityStatusSnapshot` test entry);
  `namedSessionStatusForCity` → `resolveSessionIDWithConfig` + `store.Get` (routed inside the leaf, one
  `sessStore :=` local); `collectCitySessionCounts` → `workerSessionCatalogWithConfig`.
- **STORE-HEALTH (kept on plain store):** `buildCityStoreHealth` → `collectStoreHealth` → `liveRowCount`
  → `store.List(AllowScan,IncludeClosed)` measures the OPENED store's on-disk footprint (all classes),
  so it belongs on the work store, not the session store — semantically correct, not merely
  byte-identical.
- **DEAD (kept on plain store):** `observeSessionTargetWithWarning`'s store param is `_ beads.Store` and
  the inner `observeSessionTargetForStatus` call hardcodes nil.

Key findings: (1) `collectCityStatusSnapshot` looked dead (no non-test callers) but is a live TEST entry
(13 call sites in city_status_snapshot_test.go / city_status_store_health_test.go) — routed, not deleted.
(2) These files route session access via NON-front-door reads (no `sessionFrontDoor` anywhere), so the
guard's negative `sessionFrontDoor(store...)` needles are inert; the positive `cliSessionStore(` tripwire
is the protection (revert-canary fired for all 3, "did the routing get dropped?"). (3) The fable model
endpoint returned 401 in this env → ran the adversarial byte-identity review with sonnet instead: 0
behavioral defects (byte-identity + surgical correctness + nil/error/timeout paths all COULD-NOT-REFUTE;
its lone "structural" note was a non-defect contract-clarity observation about routing at the leaf vs the
call site, harmless because cliSessionStore is idempotent-identity).

Full gates: gofmt · build · vet · golangci-lint 0 · targeted status+guard tests (9.6s) · revert-canary ·
adversarial review. REMAINING immediate = cmd_mail.go (two-store mail-provider follow-up, the last
non-deferred blind root); then the deferred entangled set (cmd_wait, cmd_handoff+cmd_runtime_drain,
cmd_nudge, cmd_sling, cmd_start cascade) + Phase 6 acceptance test.
