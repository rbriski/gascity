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

**9 files now on `metadataInfoOnlyFiles`** (shape-sealed): session_template_start,
adoption_barrier, cmd_prime, cmd_skill, session_resolve, cmd_session_logs,
mcp_integration, session_index, cmd_session_wake. Verified census artifact:
`raw/session-tier4-census.json`.

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
