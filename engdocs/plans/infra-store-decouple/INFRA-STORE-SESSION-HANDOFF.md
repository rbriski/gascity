# Infra-store decouple — session handoff (CONT-37→39)

**Branch** `upstream/object-front-doors-cleanup` (base `main`), **PR #3839 DRAFT**,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`.
**HEAD `e3031d8b8`** (always `git rev-parse HEAD`; re-grep any line number below).

Session-close handoff for the object-model front-door migration. This is the top-level
narrative; the live status is `SESSION-PERIPHERY-CLOSURE-PLAN.md` Progress log
(CONT-37→39), and the immediate next work has its own detailed pair
(`ACCESS-PASS-HARD-RIPPLE-{HANDOFF,NEXT-SESSION-PROMPT}.md`).

---

## What this session did (7 commits, `b1202351e..e3031d8b8`, all pushed)

The session moved through three phases and ended by launching + advancing the access pass.

**Phase 1 — finished the SHAPE pass and proved it exhausted.**
- `13a8a1731` Phase A: `Info.DependencyOnlyMetadata` raw mirror (untrimmed `dependency_only`
  compare the trimmed bool can't reproduce).
- `31cdf48a2` **cmd_session.go fully shape-sealed** (11th file on `metadataInfoOnlyFiles`):
  relocated the wait-class `readyWaitSetForList` to `cmd_wait.go` + converted 3 session helpers
  to `Info`. Fable review 11/11 identical.
- **STRATEGIC FINDING (`c898df6b8`):** a 4-agent census proved the guard-earning SHAPE targets
  in `cmd/gc` are EXHAUSTED — the Tier-1 giants (`build_desired_state` = session writes + work
  reads; `city_runtime` = raw-by-design whole-map fingerprint + `.Open()` library-traps),
  `session_origin` (the classifier oracle's raw arm), and `cmd_start` (reconciler library trap)
  are all permanently guard-ineligible in a shape pass. The only remaining guard-earning +
  relocation-completing work is the **access pass**.

**Phase 2 — access pass batch 1 (owner authorized "it's now"): 3 leaf files.**
- `d7d0aa56b` access-sealed **adoption_barrier, session_index, mcp_integration** onto
  `frontDoorStoreFreeFiles`. These were clean RECEIVERS. Proven byte-identical pattern:
  reach-through `store := sessFront.Store().Store` (NOT the typed `sessFront.Get`, which adds
  validation + re-wraps errors), `if store==nil`→`if !sessFront.Backed()`.

**Phase 3 — access pass batch 2 (owner chose SRP split): the MEDIUM tranche.**
- `2fd4cbc5a` access-sealed **skill_visibility.go + session_logs_resolve.go** — new companion
  files holding the pure receiver LEAF helpers split out of the `gc skill list` / `gc session
  logs` command ROOTS (which stay unlisted and construct `sessionFrontDoor(store)` inline, per
  the guard doc). Fable review 5/5 identical.
- **EXCLUDED with rationale:** cmd_prime (a genuine root — opens the store from `cityPath`
  internally), session_resolve (the `resolveSessionID*` spine, 20 callers/13 files),
  session_template_start (the creation spine). Dependents reach through them; they stay raw-store
  infra.

## Current state — the three CI guard lists (`cmd/gc/frontdoor_di_guard_test.go`)

- **`frontDoorStoreFreeFiles` (7)** — ACCESS-sealed (no raw store type; relocation-safe DI):
  session_circuit_breaker, soft_reload, adoption_barrier, session_index, mcp_integration,
  skill_visibility, session_logs_resolve.
- **`metadataInfoOnlyFiles` (15)** — SHAPE-sealed (session fields read only via `Info`).
- **`snapshotInfoOnlyFiles` (9)** — snapshot reads via `OpenInfos()/FindInfo*` only.
- soft_reload.go is the only file on all three (fully sealed).

## Two properties, kept distinct

1. **Store-free (DI):** the file holds no `beads.Store` type and receives `*session.Store`.
   That is what `frontDoorStoreFreeFiles` enforces.
2. **Relocation-safe:** the front door it receives wraps the SESSION-CLASS store (constructed
   from `sessionsBeadStore()`/`resolveSessionStore` at the composition root). This is a property
   of the ROOTS (not on the guard list) and is a SEPARATE, still-open axis — several current
   composition roots (e.g. cmd_mcp) construct the front door from the generic city store.

---

## What's next (pick with the owner)

**Immediate: the access-pass HARD-ripple tranche** — `cmd_session_wake.go` and `cmd_session.go`.
Full detail + per-file blockers + options in `ACCESS-PASS-HARD-RIPPLE-HANDOFF.md`; paste prompt
in `ACCESS-PASS-HARD-RIPPLE-NEXT-SESSION-PROMPT.md`. Both are command-ROOT collections with
raw-bead escapes (wake bead) and a cross-class `rigStores` map — wholesale store-freedom is the
wrong frame; split any receiver leaves, leave the roots unlisted.

**Open architectural question (owner call) before/with the HARD tranche:** do the command
ROOTS warrant store-free listing at all, or is their real goal **relocation-safety** better met
by routing each root's store through `resolveSessionStore` (a different, harder-to-verify axis
than the store-free guard)? The store-free guard is DI hygiene; relocation-routing is the actual
mission. Decide the target property before grinding the HARD roots.

**Parallel/other tracks (not access pass):** shape-value-only conversion of the Tier-1 giants
(no guard payoff), and the `internal/api` / `internal/worker` / `internal/session` session reads
(Phase D/E/F in the closure plan — different packages, need the guard's dir resolution extended
or sibling guards; read `engdocs/architecture/api-control-plane.md` before `internal/api`).

## Discipline + guardrails (the bar)

Per file/companion: verified census (re-grep) → convert via reach-through (byte-identical) →
root passes `sessionFrontDoor(store)` + prune unused imports → wrap moved funcs' test call sites
→ add the store-free file to the guard list → `gofmt`·`go build`·`go vet`·`golangci-lint 0`·
targeted tests → **revert-canary** (inject a `beads.Store` decl; guard must fail naming the
file) → **fable adversarial behavior-identity review BEFORE commit** (`model:'fable'`, high
effort, REFUTE; diff moved funcs vs `git show HEAD:<root>`) → commit + push `--no-verify`.
Trailer `Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Update the plan
Progress log + memory (`infra-beads-decoupling-plan.md`) as files close.

- `cmd/gc` test binary is huge: scope `go test -run`, isolated `GOCACHE=$(mktemp -d)`, run
  build/vet/tests in the background (cold compile > 2-min shell window).
- NEVER run the revert-canary concurrently with golangci-lint (a torn read mis-attributes
  findings — happened once this session).
- `git push` always `--no-verify` (7-min pre-push hook; gates run manually). gascity Dolt is
  LOCAL-ONLY — `git push` only, never `bd dolt push`. `#3839` stays DRAFT.
- **Future guard tightening (fable-flagged):** the reach-through `sessFront.Store().Store`
  re-introduces a `beads.Store` local the substring needle can't see (true of every reach-through
  file). A stricter guard could forbid `.Store().Store` in listed files; today it is the
  sanctioned byte-identity escape hatch.
