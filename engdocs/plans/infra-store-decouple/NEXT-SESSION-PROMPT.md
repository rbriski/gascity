# Next-session pickup prompt — finish infra-store decouple & migrate sessions+graph to Postgres

> Paste the block below to launch the next session. It is self-orienting; it points at the
> authoritative docs and carries the load-bearing constraints. Branch `plan/decouple-infra-beads`
> @ `1ac4e0113`, worktree `/data/projects/gascity/.claude/worktrees/infra-store-plan`.

---

You are continuing the **infra/beads → Postgres decouple**. The mission is to **take it to the
finish: migrate sessions AND graph to Postgres, leaving only true work in Dolt.** Nudges, mail,
and orders are already relocation-ready; sessions (P5) and graph (P6) are NOT, and migrating them
now would orphan beads (sessions) or be rejected by a config guard (graph). That is the whole job.

**Read first (authoritative, in order):**
1. `engdocs/plans/infra-store-decouple/FINISH-AND-MIGRATE.md` — the master finish-and-migrate plan
   (status table, the full arc, the rebase/migration mechanics, the review's deferred findings).
2. `engdocs/plans/infra-store-decouple/NEXT-SESSION.md` §5 (graph P6) + §6 (sessions P5) — the
   per-class untangle mechanics with verified file:line anchors.
3. `engdocs/plans/infra-store-decouple/DESIGN.md` §5/§6/§8/§10 — hardened design + point-of-no-return.
4. Recall the auto-memory `infra-beads-decoupling-plan.md` (binding owner decisions + this session's
   API fix + the 75-agent desync review findings).

**Execute in this order (each step byte-identical at the default `bd` backend until the explicit,
owner-sequenced cutover; re-verify every file:line against live code before editing — the plan's
anchors drift, and its mixed-function lists have proven INCOMPLETE; grep EVERY read/write of the
class's beads, do not trust a fixed list):**

1. **Graph P6, Steps 1→5** (FINISH-AND-MIGRATE §2). Additive promotion of the `GraphStore`
   surface (incl. a `*beads.PostgresStore` impl) → move `ClassifyGraphPlan` to a new
   `internal/graphstore` → provider-aware `ResolveStoreRef` → rewire the `?type=molecule` augment
   + order gate to `GraphStore` UNDER federation → DELETE `coordrouter`/`coordclass` (irreversible;
   never collapse Steps 4+5) → add the `postgres` branch to `registerGraphStoreBackend` → relax the
   `ValidateBeadsClasses` guard to allow `graph=postgres`.
2. **Sessions P5(a) then P5(b)** (FINISH-AND-MIGRATE §3). Net-new `SessionStore` seam +
   `infoFromPersisted` codec extract + inject at `internal/api/session_manager.go:11` + NO
   `UNIQUE(session_name)` schema + the **projection-invariance conformance** (bd==sqlite==pg). Then
   build (do NOT run blind) the IRREVERSIBLE `gc beads adopt sessions` live-adopt command with the
   post-copy projection-equality ABORT gate. **Audit ALL `internal/api` session-bead read/write
   sites for the same cross-surface miss that bit the nudge withdraw this session (the CLI copy was
   migrated, the `internal/api` copy was not).**
3. **Tier fix** (FINISH-AND-MIGRATE §4) — make the relocated class stores write the correct
   per-class tier (sessions/nudges/orders → no-history) OR prove the label exclusion covers every
   relocated `Ready()`/open-scan path. This is a migration prerequisite (else infra beads can leak
   into Ready at the relocated backend).
4. **Verify** at each phase: `make test-cmd-gc-process-parallel`, `go vet ./...`, the PG-gated
   suites with `GC_TEST_POSTGRES_DSN` set (disposable `gc-pg` on :55460 — `billing-pg-gb` on :55455
   is someone else's). Commit each green phase with `git commit --no-verify`.

**Then the cutover (DESTRUCTIVE/IRREVERSIBLE — confirm with the owner before each live step; do not
run blind):**

5. **Rebase onto the LIVE branch.** Installed `gc` = `/opt/gascity/current → dev-2a83e20bd` =
   `deploy/sqlite-b36-probe-attribution`. `plan/decouple-infra-beads` is 139 ahead / 375 BEHIND it
   (merge-base `fc1f581ed`; deploy has NO infra-store work). **Re-confirm the live branch first**
   ("window 3" is whatever worktree is actively moving it — `git worktree list`, `gc version`,
   `readlink /opt/gascity/current`). Assess cherry-pick-the-initiative-commits vs rebase vs merge
   (high conflict risk on graph/coordrouter, session reconciler, api_state). Keep it green.
6. **Build + install** the way `dev-2a83e20bd` was built (do not hand-roll the install).
7. **Migrate maintainer-city** (`/data/projects/maintainer-city`, today `[beads]
   graph_store="sqlite"` + `[dolt]`, project_id b2269d7c, DB `ga`). Start/optimize Postgres
   (UNLOGGED for nudge/wisp shadows, LOGGED for sessions; autovacuum tuned). `gc beads postgres
   init` → set the five infra classes to `backend="postgres"` → `gc beads postgres migrate`
   (ID-preserving) for closed/historical + `gc beads adopt sessions` for OPEN sessions. **Never
   force-migrate dolt rig DBs (prior outage).**
8. **Fixup bead references if necessary** — refs-by-id are fine under class-routing; AUDIT
   cross-store deps + the `(storeRef,id)` resolver + graph prefix resolution (the latter handled by
   P6 Step 5). Repair any dangling cross-store dep/ref.
9. **Bring the city back up** (`gc start` / supervisor restart) and verify end-to-end: sessions
   adopt with continuity, nudge dispatch + wait satisfy, orders gate, Ready shows only true work,
   `/status` + dashboard sane. Watch for the deferred review items (FINISH-AND-MIGRATE §6).

**Hard constraints:** commit `--no-verify` (stale `core.hooksPath`); gascity Dolt is LOCAL-ONLY
(never `bd dolt push/pull/remote`); never `tmux kill-server`; never `go clean -cache` (`-testcache`
ok); cold build `GOCACHE=$(mktemp -d) go build ./cmd/gc/`. Byte-identical at `bd` is the bar until
the config-gated, owner-sequenced cutover. Stop/build/install/migrate/bring-up are the owner's
calls — prepare and verify; do not execute the irreversible live steps without explicit go-ahead.
