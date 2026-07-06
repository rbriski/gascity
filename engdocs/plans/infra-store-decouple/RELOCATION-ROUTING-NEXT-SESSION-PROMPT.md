# Next-session prompt — CLI session relocation-routing

Paste the block below into a fresh session.

---

Continue the **CLI session relocation-routing** pass on branch
`upstream/object-front-doors-cleanup` (base `main`, DRAFT PR #3839, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`; run `git rev-parse HEAD` —
should be at/after `593310fe2`). **12 files routed so far** (CONT-41 added cmd_restart,
completion, providers; CONT-42 added cmd_session.go — all 10 gc session command roots).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/RELOCATION-ROUTING-HANDOFF.md` — the current-state
   handoff: why the access pass pivoted to relocation-routing, the seam
   (`cli_session_store.go` — `cliSessionStore`/`cliSessionFrontDoor`), the guard, the 10 roots
   DONE, and the REMAINING blind roots (with the completeness census). **START HERE.**
2. `engdocs/plans/infra-store-decouple/SESSION-PERIPHERY-CLOSURE-PLAN.md` Progress log
   (CONT-40) — the live status.
3. Memory `infra-beads-decoupling-plan.md` CONT-40.

**KEY DECISIONS (do not relitigate):**
- Route via `cliSessionStore`/`cliSessionFrontDoor` (= `resolveSessionStore`, identity today →
  byte-identical). Whole-store route only when EVERY consumer is session-class; else surgical
  (session calls → sessStore; work/rig/mail/nudge/dep → plain store).
- cfg-less / hot / hook / daemon paths load cfg via `loadCityConfigWithoutBuiltinPackRefresh(cityPath, io.Discard)`
  (NOT `loadCityConfig` — pack-refresh side effect).
- Mixed files (controller.go, cmd_start.go) route for correctness but stay OFF the guard list.
- DEFERRED (do not attempt piecemeal): cmd_wait.go (owner-approved), cmd_handoff.go+cmd_runtime_drain.go
  (paired shared-helper effort), cmd_nudge.go, cmd_sling.go, cmd_start.go reconcile cascade.

**IMMEDIATE WORK (pick with the owner):**
- **cmd_status.go → city_status_snapshot.go** — SURGICAL/multi-class: route the session consumers
  (loadStatusSessionSnapshot resolveSessionIDWithConfig@353 + store.Get@361, namedSessionStatusForCity,
  observeStatusTargetsParallel) but keep `buildCityStoreHealth`→`collectStoreHealth`@138/145 (store-maintenance
  health, NOT session) on the plain store. cfg+cityPath thread through collectCityStatusSnapshot.
- **cmd_mail.go** (12 subcommands) — the session reads are in the SHARED beadmail provider, not the
  subcommands. Route `openCityMailProvider` (providers.go@814) ONCE — but this is the two-store mail-provider
  follow-up (resolveMailMessagesStore), i.e. split the beadmail store into messaging-class + session-class.
  Larger than a substring route; scope with the owner.

**DONE at CONT-41 (do not redo):** cmd_restart.go (whole-store at the cmdRigRestart caller),
completion.go (whole-store), providers.go (PARTIAL — loadProviderSessionSnapshot routed; the
openCityMailProvider/beadmail session read+write is the deferred two-store-mail gap, documented in-code).

**DONE at CONT-42 (do not redo):** cmd_session.go — all 10 gc session command roots (9 whole-store +
cmdSessionClose surgical). A 10-agent census workflow proved cmdSessionKill is whole-store (NOT
multi-class as the old plan guessed) and only cmdSessionClose has the WORK-class work-release. If you
tackle cmd_mail.go, remember the session reads live in the shared beadmail provider (openCityMailProvider),
not the subcommands — it is the two-store-mail follow-up, larger than a substring route.

**Discipline (byte-identity is the bar):** per root — verified per-consumer census (re-grep; DON'T trust
prior classifications) → route → gofmt·build·vet·`golangci-lint 0`·targeted tests → **revert-canary**
(guard fails naming the file) → **fable adversarial byte-identity review BEFORE commit** (`model:'fable'`,
REFUTE; diff vs `git show HEAD:<file>`; for whole-store routes also confirm semantic session-class-correctness)
→ commit + push `--no-verify`. Trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Add fully-routed single-class files to `sessionRelocationRoutedFiles`; grow the list at each phase end.

**Guardrails:** `cmd/gc` test binary is huge — scope `go test -run`, isolated `GOCACHE=$(mktemp -d)`, run
build/vet/tests in the background (cold compile > 2 min). NEVER run the canary concurrently with golangci-lint
(torn read). `git push` always `--no-verify`; commit `--no-verify` too (stale absolute core.hooksPath breaks
commit). gascity Dolt LOCAL-ONLY. `#3839` stays DRAFT.

**Phase 6 (still TODO):** the end-to-end `[beads.classes.sessions]` relocation acceptance test — the
authoritative check the substring guard can't provide.

---
