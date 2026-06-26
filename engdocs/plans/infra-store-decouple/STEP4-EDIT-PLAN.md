---
title: "Sessions Step 4 — verified edit plan (recon-confirmed 2026-06-26)"
date: 2026-06-26
branch: plan/decouple-infra-beads
---

> Built from the adversarial recon (workflow `wf_5bb2b5c6-f3e`, 15 agents, high
> confidence). The recon CONFIRMED the verified site list's session-surface flips
> AND found scope the list missed: (1) the materialize work-entanglement chokepoint,
> (2) **7 inverse-leak sites** in "stay-on-city" files that read session beads off
> the work store (the same cross-surface miss that bit the nudge withdraw).
> Every change is byte-identical at the default backend (SessionsBeadStore()==CityBeadStore()).

## Root insight
The ONLY work-bead entanglement on the whole API session surface is one chokepoint:
`session_resolution.go::reassignContinuityIneligibleNamedSessionState` →
`reassignOpenWorkAssignedToSession` (work List+Update) + `extmsg.ReassignSession*`
(extmsg beads, not relocated) + `session.ReassignWaits` (wait beads, relocate WITH
sessions). Because it's a `*Server` method, source the work store directly
(`s.state.CityBeadStore()`); no param threading needed. Reached only via the
materialize path (`resolveSessionIDMaterializingNamed*`).

The inverse leaks all share one vector: `resolveSessionTargetIDWithContext` /
`session.ResolveSessionID*` / `store.Get(sessionID)` handed `CityBeadStore()`.
Fix: hand them `SessionsBeadStore()` for the session-resolution portion only.

## Sub-phase 4a — materialize chokepoint + command/query/interaction (4 files)
1. `session_resolution.go` — in `reassignContinuityIneligibleNamedSessionState`
   (~L181): add `workStore := s.state.CityBeadStore()`; route
   `reassignOpenWorkAssignedToSession(workStore,…)` (L187),
   `extmsg.ReassignSessionBindings(ctx, workStore,…)` (L193),
   `extmsg.ReassignSessionParticipants(ctx, workStore,…)` (L196) to workStore;
   KEEP `session.ReassignWaits(store,…)` (L190) on the session `store`.
   (All other session_resolution.go funcs are param-threaded — fixed by callers.)
2. `huma_handlers_sessions_command.go` — flip 12 `store := s.state.CityBeadStore()`
   → `SessionsBeadStore()` at 38,397,467,557,604,705,730,761,793,816,862,916.
   KEEP `withdrawQueuedWaitNudges(s.state.NudgesBeadStore(),…)` at 840,888.
3. `huma_handlers_sessions_query.go` — flip 7 at 24,108,140,287,347,429,470.
4. `handler_session_interaction.go` — flip 4 at 28,72,97,122,161.

## Sub-phase 4b — handler_sessions + stream/agents/transcript/create (5 files)
5. `handler_sessions.go` — flip 7 decls at 225,290,323,347,434,480,665 + inline
   `s.workerFactory(s.state.CityBeadStore())` at 625. KEEP nudges at 367,465.
6. `handler_session_stream.go` — flip decl at 68 + inline workerHandleForSession at 938,1019.
7. `handler_session_agents.go` — flip 2 at 15,52.
8. `handler_session_transcript.go` — flip 1 at 30.
9. `handler_session_create.go` — flip 1 at 37.

## Sub-phase 4c — agent-output + agents + status + stream (5 files)
10. `handler_agent_output.go` — flip 4 inline at 78,87,106,226.
11. `handler_agent_output_stream.go` — flip 2 inline at 136,329.
12. `handler_agents.go` — flip 1 inline at 448 (session catalog). KEEP L292 `BeadStores()` (work census).
13. `handler_status.go` — flip 1 at 444 (sessionReadModelRows). **KEEP L68** (city-cache liveness gate — must stay CityBeadStore).
14. `huma_handlers_sessions_stream.go` — flip 1 at 19.

## Sub-phase 4d — inverse-leak stay-on-city files (5 files)
15. `handler_beads.go` — `beadListAssigneeTerms` (~L52) + `normalizeRawBeadAssignee`
    (~L94): the assignee RESOLUTION (`resolveSessionTargetIDWithContext`, `store.Get`,
    `session.RepairEmptyType`) must use a sessions store; the WORK bead create/update
    stays on CityBeadStore. Within-function split.
16. `handler_mail.go` — `resolveMailSendRecipientWithContext` (~L77) +
    `resolveMailQueryRecipientsWithContext` (~L119): recipient session-resolution → sessions store.
17. `handler_extmsg.go` — `extmsgSessionHandleForSelector` (~L56),
    `extmsgSessionHandleForResolvedID` (~L68), `extmsgNotifyMembers` (~L103):
    session resolution/handle/nudge → sessions store. (extmsg binding writes stay city.)
18. `worker_operation_watch.go` — `resolveAgentSessionSubjects` (~L27):
    `resolveSessionIDWithConfig` → sessions store.
19. `session_runtime.go` — `sessionMetadata` (~L218): `store.Get(sessionID)` → sessions store.

## Verify per sub-phase
`go build ./internal/api/... ./cmd/gc/...`; commit `--no-verify`. After 4d:
`go test ./internal/api/...`, `go vet ./...`, then `make test-cmd-gc-process-parallel`.

## Guard (added in 4a or 4d)
A test asserting that at a RELOCATED sessions backend, the API session-resolution
path reads from the sessions store and the materialize work-reassign reads from the
work store (pointer/behavioral inequality) — the byte-identity guard for the split.
