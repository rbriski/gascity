# Next-session pickup prompt — finish sessions wiring + tier fix, then migrate maintainer-city to Postgres

> Paste the block below to launch the next session. It is self-orienting. Branch
> `plan/decouple-infra-beads` @ `dcdcc7502`, worktree
> `/data/projects/gascity/.claude/worktrees/infra-store-plan`. 9 local commits, unpushed.

---

You are continuing the **infra/beads → Postgres decouple** for maintainer-city. **Graph is
already fully migrated to Postgres (Path A — Router kept).** The **sessions seam FOUNDATION is
done** (byte-identical). Your job: **finish the sessions wiring (Steps 4–5), do the tier fix
(Phase C), then take it home — rebase, build/install, and run the owner-gated cold migration of
maintainer-city to Postgres, bring it up, babysit, and tune PG.**

**Read first (authoritative, in order):**
1. `engdocs/plans/infra-store-decouple/HANDOFF-2026-06-26.md` — the master handoff (ground truth,
   what's done, what remains, the migration mechanics, the decision log, constraints).
2. `engdocs/plans/infra-store-decouple/SESSIONS-P5A.md` — the sessions seam plan, and
   `raw/sessions-p5a-writers-to-route.txt` — the **VERIFIED site list** for Steps 4–5. USE IT.
   Do NOT re-derive the list (the nudge list proved incomplete; the recon found ~25 internal/api
   session sites map #2/#3 missed, incl. the legacy net/http `handler_sessions.go` still registered).
3. `engdocs/plans/infra-store-decouple/FINISH-AND-MIGRATE.md` — the graph Path A outcome + arc.
4. Recall auto-memory `infra-beads-decoupling-plan.md` (binding decisions + this session's deltas).

**Execute in this order (byte-identical at the default `bd` backend until the explicit,
owner-sequenced cutover; re-verify every file:line against live code — anchors drift):**

1. **Sessions Step 4 (api routing).** Route the ~60 `internal/api` session-handler sites
   `s.state.CityBeadStore()` → `s.state.SessionsBeadStore()` (session/wait surface ONLY, per
   `raw/sessions-p5a-writers-to-route.txt`). KEEP work/mail/order/convoy/formula handlers on
   `CityBeadStore()`; KEEP `withdrawQueuedWaitNudges` on `NudgesBeadStore()`. **Watch the
   second-copy bug:** legacy `handler_sessions.go` (net/http) is STILL registered
   (`server.go:220-234`). Sub-phase by file group; each is byte-identical at default
   (`SessionsBeadStore()==CityBeadStore()`), so commit incrementally — but the seam is only
   FUNCTIONAL once ALL sites are routed.
2. **Sessions Step 5 (controller two-store untangle — THE LANDMINE).** The controller threads ONE
   `store` for BOTH session-bead writes AND work-bead assignment reads (`workAssignmentStores`
   prepends it; `sessionHas*AssignedWorkForReachableStore` reads WORK beads). A naive substitution
   makes the reaper close LIVE working sessions. Derive `sessionStore = resolveSessionStore(
   cr.cityBeadStore(), cr.cfg, cr.cityPath, cr.rec)` once per tick and thread `sessionStore` +
   `workStore` as SEPARATE params through `session_beads.go`, `session_reconciler.go`,
   `session_wake.go`, `session_lifecycle_parallel.go`, `cmd_wait.go` (the `store` arg, NOT
   nudgeStore), `cmd_session_wake.go`, `cmd_nudge.go`. Session/wait writes→sessionStore,
   work-assignment reads→workStore(+rigStores). **Pointer-equality test at default = byte-identity guard.**
   Keep `TestGCNonTestFilesStayOnWorkerBoundary` green (no `session.NewManager(` bypass).
3. **Sessions Step 7 (rest).** Classed-store conformance for `ClassSessions` (mirror the graph/
   orders conformance in `internal/beads/{sqlite,postgres}_store_conformance_test.go`). NO
   `UNIQUE(session_name)` — the duplicate-then-elect reconciler (`session_beads.go:884-919`)
   requires a transient duplicate; the EAV schema has none. Do not add one.
4. **Phase C — tier fix (migration prerequisite).** The relocated session/nudge/order stores open
   RAW (no policy wrapper), so writes land on `main` tier not `no-history`; the label exclusion is
   the SOLE Ready guard at the relocated backend. The sessions seam REINTRODUCES this. Make the
   class store write the correct per-class tier OR prove `IsReadyExcludedBead`/
   `IsReadyCandidateForTier` cover every relocated `Ready()`/open-scan path (incl. `ReadyGraphOnly`,
   doctor/`/status` censuses). Add a guard test asserting a relocated session/nudge bead is
   no-history (or relocated `Ready()` excludes it).
5. **Verify at each phase:** `make test-cmd-gc-process-parallel`, `go vet ./...`, the PG-gated
   suites with `GC_TEST_POSTGRES_DSN` (disposable `gc-pg` on :55460 — `billing-pg-gb` on :55455 is
   someone else's). Commit each green phase with `git commit --no-verify`.

**Then the cutover (DESTRUCTIVE/IRREVERSIBLE — confirm with the owner before each live step; do
not run blind). Full mechanics in HANDOFF §3.C:**

6. **Rebase** onto the LIVE branch. Installed = `deploy/sqlite-b36-probe-attribution @ 2a83e20bd`
   (375 behind; high conflict risk on graph/api_state/session reconciler). **RE-CONFIRM the live
   branch + tmux window 3 first** (`recovered-interactive-20260613:3 [beads-graph-work]`; coordinate
   via send-keys). Assess cherry-pick the initiative commits vs rebase vs merge; keep green.
7. **Build + install** the way `dev-2a83e20bd` was built (do not hand-roll).
8. **Stop maintainer-city** (`/data/projects/maintainer-city`, LIVE — interrupts agents).
9. **Provision + optimize PG** (`gc beads postgres init`; UNLOGGED nudge/wisp shadows, LOGGED
   sessions, tuned autovacuum). Set the 5 infra classes `backend="postgres"`.
10. **Migrate** dolt→pg via `gc beads postgres migrate` (ID-preserving, idempotent). NB graph's
    source is the SQLite `.gc/beads.sqlite`, not dolt — verify the graph sqlite→pg path. Sessions
    cold-migrate (city stopped). **Never force-migrate dolt rig DBs (prior outage).**
11. **Fixup refs if needed**, **bring up** (`gc start`/supervisor), **babysit** end-to-end
    (controller reconciles; sessions adopt; nudges/waits/orders satisfy; Ready = only true work;
    `/status` + dashboard sane), then **tune Postgres** + measure.

**Hard constraints:** commit `--no-verify` (stale `core.hooksPath`); gascity Dolt is LOCAL-ONLY
(never `bd dolt push/pull/remote`); never `tmux kill-server`; never `go clean -cache` (`-testcache`
ok); cold build `GOCACHE=$(mktemp -d) go build ./cmd/gc/`. Byte-identical at `bd` is the bar until
the config-gated, owner-sequenced cutover. Stop/rebase/build/install/migrate/bring-up are the
owner's calls — prepare and verify; do not execute the irreversible live steps without explicit
go-ahead. Push the 9 local commits only on the owner's word.
