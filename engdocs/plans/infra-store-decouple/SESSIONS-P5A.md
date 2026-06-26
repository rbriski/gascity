---
title: "Sessions P5(a) — the SessionStore seam (per-subsystem, owner-chosen)"
date: 2026-06-26
branch: plan/decouple-infra-beads
---

> **Approach (owner decision):** sessions relocate via the **per-subsystem seam**
> (`resolveClassStore`), NOT the Router. Sessions go on "the common interface" like
> nudges/mail/orders — every session/wait bead op routes through a session store;
> cross-class WORK-bead reads stay on the work store.
>
> **Live-adopt (P5b) is DESCOPED** — the goal stops the city before migrating, so a
> cold bulk `gc beads migrate` (sessions, bd→pg) + the seam suffices; there are no
> running agents whose open-session continuity must be preserved. Revisit only if a
> future requirement demands zero-downtime relocation.

## DONE (byte-identical at default, committed)

| Commit | Step | What |
|---|---|---|
| `2b2302970` | 1+2 | `resolveSessionStore` helper (mirrors resolveOrderStore/resolveNudgesStore) + all-class typo validator (`ValidateBeadsClasses` rejects an unrecognized `[beads.classes.*].backend` for every class) |
| `3ab94d546` | 3 | `SessionsBeadStore()` on `api.State` + `controllerState` impl (`resolveSessionStore(cityBeadStore,cfg,cityPath,eventProv)`) + fake fallback |
| `a01dd5a19` | 6+7 | pure `infoFromPersisted` codec extracted from `infoFromBead` (runtime overlay stays in Manager); projection-invariance proof (SQLite≡Postgres infoFromPersisted) |

Already wired (no code change): `gcs` reserved prefix + `classSQLitePrefix[sessions]`
(`reserved_prefixes.go:19`); the postgres opener (`classBackendOpeners[postgres]`).

## REMAINING (the WIRING — the risky bulk)

### Step 4 — route the ~60 `internal/api` session-handler sites to `SessionsBeadStore()`
Replace `store := s.state.CityBeadStore()` with `s.state.SessionsBeadStore()` in the
SESSION/WAIT surface ONLY. **The exhaustive, verified site list is in
`raw/sessions-p5a-writers-to-route.txt`** (do NOT re-derive — the nudge list proved
incomplete). Key files: `handler_sessions.go` (legacy net/http, STILL registered —
`server.go:220-234` — a second-copy bug if missed), `huma_handlers_sessions_command.go`,
`_query.go`, `_stream.go`, `handler_session_create.go`, `handler_session_interaction.go`,
`handler_session_agents.go`, `handler_session_stream.go`, `handler_session_transcript.go`,
`handler_agent_output*.go`, `handler_agents.go:448`, `session_resolution.go` (incl.
the `session.ReassignWaits` bypass at :190), `handler_status.go:456` (sessionReadModelRows).
**KEEP** `withdrawQueuedWaitNudges` on `NudgesBeadStore()` and the nudgeStore threading.
**KEEP** work/mail/order/convoy/formula/rig/sling/extmsg handlers on `CityBeadStore()`.

### Step 5 — controller two-store untangle (THE LANDMINE)
The controller threads ONE `store` that serves BOTH session-bead writes AND
work-bead assignment reads (`workAssignmentStores` PREPENDS it to rigStores;
`sessionHas*AssignedWorkForReachableStore`/`closeSessionBeadIfReachableStoreUnassigned`
read WORK beads from it). **A naive substitution makes every assigned-work probe hit
the empty sessions store → the reaper closes LIVE working sessions.** Thread
`sessionStore` and `workStore` as SEPARATE params: session/wait writes→`sessionStore`,
work-assignment reads→`workStore`(+rigStores). Pointer-equality test at default
guards byte-identity. Sites in `raw/sessions-p5a-writers-to-route.txt` (controller
sections) — `session_beads.go`, `session_reconciler.go`, `session_wake.go`,
`session_lifecycle_parallel.go`, `cmd_wait.go` (the `store` arg, NOT nudgeStore),
`cmd_session_wake.go`, `cmd_nudge.go`. Derive `sessionStore = resolveSessionStore(
cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)` once per tick.

### Step 7 (remaining half) — classed-store conformance for ClassSessions
Add `TestSQLiteStoreSatisfiesClassedStoreConformanceForSessions` + the Postgres
sibling (mirror the graph/orders conformance) so both backends round-trip + classify
session(type=session/gc:session) AND wait(gc:wait) beads.

## Risks (full list in `raw/sessions-p5a-risks.txt`)
- Mass-closure landmine (Step 5 — above).
- Second-copy regression: legacy net/http `handler_sessions.go` still registered.
- Re-breaking nudge routing: keep `withdrawQueuedWaitNudges` on NudgesBeadStore();
  nudgeStore is a SEPARATE value from sessionStore.
- NO `UNIQUE(session_name)` — the duplicate-then-elect reconciler (`session_beads.go:884-919`)
  requires a transient duplicate; the EAV schema has none. Do not add one.
- Tier divergence (Phase C): the relocated session store opens RAW (no policy wrapper),
  so session beads land on 'main' tier not no-history — fix in Phase C or prove the
  label exclusion covers every relocated Ready/scan path before the real cutover.
- `TestGCNonTestFilesStayOnWorkerBoundary`: the seam must not add a `session.NewManager(`
  bypass in cmd/gc — `resolveSessionStore` is a store wrapper, compliant.

## After P5a
Phase C tier fix → then the owner-gated cold migration: stop city → `gc beads postgres
init` → set the 5 infra classes `backend="postgres"` → `gc beads postgres migrate`
(closed/historical; sessions cold-migrate too since the city is stopped) → bring up →
tune PG.
