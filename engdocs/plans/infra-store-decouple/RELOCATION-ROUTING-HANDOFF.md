# CLI session relocation-routing — handoff

**Branch** `upstream/object-front-doors-cleanup` (base `main`), **PR #3839 DRAFT**,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`.
**HEAD `6d432a0d3`** (always `git rev-parse HEAD`; re-grep every line number below).

> **CONT-41 (2026-07-06):** +3 files routed — cmd_restart.go (9th), completion.go
> (10th), providers.go (11th, PARTIAL). Two census corrections from the fable
> byte-identity passes: (1) providers.go was NOT "NON-SESSION safe" — its
> `loadProviderSessionSnapshot` reads gc:session beads off a raw store (now routed);
> (2) NEW deferred gap: `openCityMailProvider` → beadmail does session reads AND
> WRITES on its store (`session.ListAllSessionBeads` / `ResolveSessionID` /
> `RepairEmptyType`, beadmail.go:79,91,187,760,799) — the mail session-addressing
> layer beneath cmd_mail.go lives in the SHARED provider, not per-subcommand; route
> it at openCityMailProvider (the two-store mail-provider follow-up at
> `resolveMailMessagesStore`), not one subcommand at a time. Commits
> `d9486309c` (completion), `ba530c91f` (restart), `6d432a0d3` (providers).

This is the successor to the access-pass DI batches (store-free guard, CONT-37→39). The
**access pass PIVOTED** from store-free DI hygiene to the actual mission:
**relocation-safety**. This doc hands off the remainder of that pivot.

---

## Why the pivot (the finding that reframed the pass)

The store-free DI guard (`frontDoorStoreFreeFiles`) is compile-time hygiene and is
**orthogonal to the mission** for CLI command roots. The mission is: a
`[beads.classes.sessions]` relocation must capture 100% of session-bead access.

- The **controller/runtime is already relocation-safe** — `city_runtime.go` routes every
  session access through `cr.sessionsBeadStore()` → `resolveSessionStore(...)`.
- The **CLI one-shot roots are relocation-BLIND** — they do
  `sessionFrontDoor(openCityStore(...))`, and `openCityStore` (`main.go:1073`) returns the
  **generic work store**, never the session-class store. After a relocation their session
  reads/writes hit the wrong backend (split-brain).
- The fix is **byte-identical today**: `resolveSessionStore` → `resolveClassStore`
  (`class_store.go`) is pure identity, so wrapping only diverges once a relocation is
  configured.

Owner decision (this session): pivot to relocation-routing.

## The seam (landed, `cmd/gc/cli_session_store.go`)

```go
func cliSessionStore(store beads.Store, cfg *config.City, cityPath string) beads.Store {
	return resolveSessionStore(store, cfg, cityPath, nil) // identity today; recorder nil (no CLI event bus)
}
func cliSessionFrontDoor(store beads.Store, cfg *config.City, cityPath string) *session.Store {
	return sessionFrontDoor(cliSessionStore(store, cfg, cityPath))
}
```

**Routing patterns:**
- **Whole-store** (all consumers session-class): `sessStore := cliSessionStore(store, cfg, cityPath)`
  right after the open, replace every `store` use with `sessStore`. Used in cmd_stop, cmd_session_reset.
- **Surgical** (multi-class root): compute `sessStore` once, pass it only to session consumers,
  keep plain `store` for work/rig/mail/nudge/dep consumers.
- **cfg-less roots** must load cfg. Use `loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)`
  on **hot/hook/daemon** paths (NOT `loadCityConfig` — it triggers a builtin-pack refresh). nil cfg →
  identity, so byte-identical today regardless. (Owner-decided for cmd_prime; reused for the controller socket.)
  Note: the no-refresh loader still calls `applyFeatureFlags` (writes the formulaV2/graphApply global
  atomics) — proven inert where used (no reader on the path; value equals what the main load sets).

## The guard (`cmd/gc/frontdoor_di_guard_test.go`)

`TestSessionRelocationRootsRouteThroughSessionClassStore` over `sessionRelocationRoutedFiles`:
forbids `sessionFrontDoor(store)` / `sessionFrontDoor(store.Store)` / `sessionFrontDoor(openCityStore`
in listed files and requires `cliSessionStore(`/`cliSessionFrontDoor(` present (positive tripwire).
It is a **regression canary, not a completeness proof** — it can't see non-front-door session reads
(`store.Get`, `resolveSessionID*`). Mixed files (controller.go, cmd_start.go) are intentionally OFF the
list. `cli_session_store.go` is OFF it (the one legitimate `sessionFrontDoor` holder). **The authoritative
check is the end-to-end `[beads.classes.sessions]` relocation acceptance test — still TODO (Phase 6).**

---

## DONE this session (8 commits, `0aa51fafd..3e05a03fe`, all pushed)

10 roots routed, each: gofmt · build · vet · golangci-lint 0 · targeted tests · revert-canary
· **fable adversarial byte-identity review (all COULD-NOT-REFUTE)**.

| File | Root(s) | Routing | Guard-listed |
| ---- | ------- | ------- | ------------ |
| cli_session_store.go | (seam) | new helpers | no (excluded) |
| cmd_session_wake.go | cmdSessionWake | sessStore | ✅ |
| cmd_session_pin.go | cmdSessionSetPin | sessStore | ✅ |
| cmd_skill.go | skill list | cliSessionFrontDoor | ✅ |
| cmd_mcp.go | mcp list | cliSessionFrontDoor | ✅ |
| cmd_session_logs.go | session logs | cliSessionFrontDoor | ✅ |
| cmd_prime.go | primeHookSessionTemplate + persistPrimeHookProviderSessionKey | sessStore + no-refresh cfg load | ✅ |
| cmd_stop.go | cmdStopBody | whole-store sessStore (all 5 consumers session-class) | ✅ |
| cmd_start.go | doStartStandalone | adoption barrier ONLY (reconcile cascade deferred) | ❌ (partial/mixed) |
| cmd_session_reset.go | cmdSessionReset | whole-store sessStore | ✅ |
| controller.go | handleSessionCircuitResetSocketCmd | cliSessionStore + no-refresh cfg load | ❌ (mixed file) |
| cmd_restart.go (CONT-41) | doRigRestart (via cmdRigRestart caller) | whole-store sessStore (all 5 consumers session-class, like gc stop) | ✅ |
| completion.go (CONT-41) | loadSessionsForCompletion | whole-store sessStore (ListAllSessionBeads + session catalog) | ✅ |
| providers.go (CONT-41) | loadProviderSessionSnapshot | surgical sessStore (opens own store; mail sibling deferred) | ✅ (PARTIAL — see note) |

`sessionRelocationRoutedFiles` (11): wake, pin, skill, mcp, session_logs, prime, stop,
session_reset, cmd_restart, completion, providers (providers is PARTIAL — the
openCityMailProvider/beadmail session read+write is a deferred two-store-mail gap;
the guard entry protects only the loadProviderSessionSnapshot route).

---

## REMAINING (next sessions) — the completeness census (Explore sweep this session)

The original census only grepped **direct** `sessionFrontDoor` sites and MISSED roots that reach
session state via **helpers** (that's how cmd_session_reset + cmd_runtime_drain surfaced). The full
remaining blind-root set:

### Phase 4 — cmd_session.go (BIG, its own session)
~9 in-file RunE roots (cmdSessionNew, doSessionListFallback, cmdSessionSuspend, **cmdSessionClose**,
**cmdSessionKill**, cmdSessionAttach, cmdSessionRename, cmdSessionSubmit, doSessionPeekFallback).
Multi-class: `cmdSessionClose` uses `store` for session reads AND `unclaimWorkAssignedToRetiredSessionBead(store, rigStores, …)` (WORK) — **surgical** routing (route session calls, keep work-release on plain store; `rigStores map[string]beads.Store` is a cross-class rig map, leave). **Verify each root's consumers per-consumer** — the plan's classifications proved unreliable (it was wrong about cmd_stop's consumers). cmdSessionKill reaches `resetSessionCircuitBreakerAfterExplicitKill(cityPath, store, …)` (session) + `store.SetMetadataBatch` (session). All roots have cfg+cityPath.

### NEW blind roots the plan never listed (found by the completeness census)
- **cmd_restart.go** `doRigRestart` — ✅ DONE (CONT-41). Whole-store route at the caller
  `cmdRigRestart` (cityPath@127, cfg@128; store used only to hand to doRigRestart, so its
  signature + ~15 test callers are untouched). All 5 consumers verified session-class
  (lookupSessionNameOrLegacy, workerSessionTargetRunningWithConfig, resolvePoolSessionRefs,
  selectRunningPoolSessionRefs, stopTargetsBounded → hydrateStopTargets/stopTargetThroughWorkerBoundary).
- **completion.go** `loadSessionsForCompletion` — ✅ DONE (CONT-41). Whole-store route
  (ListAllSessionBeads + workerSessionCatalogWithConfig→session catalog). Already used the
  no-refresh cfg loader. Only store-using root in the file.
- **providers.go** `loadProviderSessionSnapshot` — ✅ DONE (CONT-41, PARTIAL). This was
  MIS-CLASSIFIED as "NON-SESSION safe" below; it reads gc:session beads off `openSessionProviderStore`
  (= openCityStoreAt). Routed surgically (opens its own store, so it fixes CLI + controller
  provider-construction at once). **BUT the file is only partially routed** — see the beadmail
  gap under Deferred.
- **cmd_mail.go** (12 subcommands) — still DEFERRED, but the census sharpened: the session
  reads/writes are NOT per-subcommand — they live in the SHARED beadmail provider built by
  `openCityMailProvider` (see Deferred). Route that provider once, not 12 subcommands.
- **cmd_status.go** → `city_status_snapshot.go` — MULTI-CLASS (surgical). `loadStatusSessionSnapshot`
  (`resolveSessionIDWithConfig`@353, `store.Get`@361) is session, but `buildCityStoreHealth`→
  `collectStoreHealth`@138/145 reads store-maintenance health (NOT session), and `namedSessionStatusForCity`
  /`observeStatusTargetsParallel` are session. Route the session consumers, keep store-health on plain
  store. Indirect; route when the caller routes.

### Deferred (entangled — own coordinated efforts; owner-approved for cmd_wait)
- **cmd_handoff.go + cmd_runtime_drain.go** — PAIRED. Share the session helpers
  `sessionRestartableByController` / `clearRestartRequest` (both call them; also `sessionRestartPersister`).
  `doHandoffWithOutcome` mixes MAIL (`createHandoffMail`→`beadmail.New`) + SESSION in one tested helper
  (~10 test call sites). Clean routing needs a two-store split on `doHandoffWithOutcome`/`doHandoffRemote`
  (+ test updates) OR a control-flow hoist (byte-identity risk — fable-flagged). Route both roots together
  so the shared helpers receive a routed store from every caller.
- **cmd_wait.go** — DEFERRED (owner-approved). Multi-class machinery SHARED with the controller reconciler:
  `retryClosedWait` uses one `store` for BOTH nudge lookup AND session writes; dep reads are work-class +
  federated; wait-list reads deliberately use the federated store. Needs a per-class store split across shared
  helpers — its own "wait-machinery class-split" effort. (Closure plan treats "wait as a separate future class".)
- **cmd_nudge.go** (@455/1050/1264 build sessionFrontDoor from a NUDGES-class store), **cmd_sling.go** (@1495),
  **cmd_start.go reconcile cascade** (`beads.SessionStore{Store: oneShotStore}` — multi-class mirror-of-runtime).
- **openCityMailProvider → beadmail (providers.go@814)** — NEW (CONT-41, fable-found). The beadmail
  provider uses its ONE store for BOTH messaging-class ops AND session-class reads+WRITES:
  `session.ListAllSessionBeads` (beadmail.go:79,91), `session.ResolveSessionID` (:187),
  `session.RepairEmptyType` (:760,799 — writes). This is the real session-access layer beneath
  cmd_mail.go (all 12 subcommands share this provider). Deliberately NOT routed: the split belongs to
  the two-store mail-provider follow-up parked at `resolveMailMessagesStore`/`newCityMailProvider`
  (class_store.go). Route mail's session reads HERE (once), not per-subcommand. Documented in-code at
  openCityMailProvider.

### NON-SESSION (verified safe, no routing): cmd_prompt.go, cmd_start_warmup.go, dispatch_runtime.go.
  (providers.go was REMOVED from this list at CONT-41 — it had a session read; now partially routed.)

---

## Discipline (the bar — unchanged; every routed file)

Verified per-consumer census (re-grep; DON'T trust prior classifications) → route (whole-store if all
consumers session-class, else surgical) → gofmt · `go build ./cmd/gc/` · `go vet` · `golangci-lint run
./cmd/gc/` (0) · targeted `go test -run` → **revert-canary** (guard must fail naming the file) → **fable
adversarial byte-identity review BEFORE commit** (`model:'fable'`, REFUTE; diff vs `git show HEAD:<file>`;
confirm identity-today AND semantic session-class-correctness for whole-store routes) → commit + push
`--no-verify`. Trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.

**Guardrails:** `cmd/gc` test binary is huge — scope `go test -run`, isolated `GOCACHE=$(mktemp -d)` (this
session reused `/tmp/gc-reloc-cache`), run build/vet/tests in the background (cold compile > 2 min). NEVER
run the revert-canary concurrently with golangci-lint (torn read). `git push` always `--no-verify` (7-min
pre-push hook; stale absolute `core.hooksPath` also breaks `git commit` → commit `--no-verify`, gates run
manually). gascity Dolt LOCAL-ONLY — `git push` only. `#3839` stays DRAFT.

## Acceptance (Phase 6, TODO)

Add an end-to-end `[beads.classes.sessions]` relocation test: configure a distinct sessions backend, run
each routed root, assert session/wait beads land in the relocated store while work/dep/nudge/mail stay put.
This is the only thing that proves routing correctness for the mixed files (controller.go, cmd_start.go) and
the non-front-door session reads the substring guard can't see.
