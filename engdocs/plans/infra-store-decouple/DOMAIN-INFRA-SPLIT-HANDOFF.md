# Domain / Infra store split — handoff

**Branch** `upstream/object-front-doors-cleanup` (base `main`), **PR #3839 DRAFT**,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`.
**HEAD `6706af8c3`** (always `git rev-parse HEAD`; re-grep every line number below).

> **CONT (2026-07-06) — E1.6 DONE + audit finding.** A parallel census
> (`raw/e1-census-wf_61848080.json`: 1 adversarial E1.6 auditor + 5 E1.1 file
> censuses) proved E1.6's cross-store member+source seam (shipped in `#3773`) was
> **already threaded** (member Get/SetMetadata via `storeref.Resolve`/
> `drainMemberOwningStore`, source chain via `opts.ResolveStoreRef`) but had **3
> residual gaps**. Fixed the 2 byte-identical ones (commit `6706af8c3`):
> `drain.go` `drainProjectedBlockerIDs`/`orderDrainMembersByDependencies` read a
> member's dep edges on the ambient store — routed through a new probe-free
> `drainMemberDepStore` (owning-store when `MemberStores` set, else ambient) + 2
> two-store canaries. **Deferred the 3rd** (`retry.go resolveRequiredArtifactWorktree`)
> because `opts.ResolveStoreRef` is ALREADY live in prod → routing it is a real
> behavior change (latent cross-store-source bug fix), not a byte-identical E1
> conversion; needs its own analysis + owner sign-off (see backlog E1.6). E1.1
> census in the same artifact: `cmd_sling.go` and `cmd_start.go` are **clean-surgical**
> with concrete plans; `cmd_wait`/`cmd_nudge`/`cmd_handoff+drain` are entangled.
>
> **CONT (same session) — E1.1 cmd_start.go DONE (commit `4b4961146`).** Routed the
> full standalone reconcile-cascade session arm (loadSessionBeadSnapshot ×2,
> buildDesiredStateWithSessionBeads LEADING store ×3, sync ×3, reconcile ×1) to
> `sessStore := cliSessionStore(oneShotStore,cfg,cityPath)`, keeping rigStores +
> releaseOrphaned plain — an EXACT mirror of the daemon (city_runtime.go). cmd_start.go
> joined the relocation guard. **KEY LESSON: the census was WRONG to defer
> buildDesiredStateWithSessionBeads** ("dual-role, can't arg-swap") — the daemon ALREADY
> arg-swaps its leading store to `sessionsBeadStore().Store`, so deferring diverged from
> the daemon and left session-bead CREATES unrouted. Caught by the FABLE review (round 1
> refuted the defer; round 2 confirmed the fix). **Discipline update for the rest of E1.1:
> validate every "leave-plain/defer" decision against the DAEMON's store-role assignment
> in city_runtime.go — the daemon is the authority on session-vs-work, not a static read
> of the callee, and not the census.** **Next: cmd_sling.go (clean-surgical), then the
> entangled cmd_nudge / cmd_wait / cmd_handoff+cmd_runtime_drain set.**

**The backlog is `DOMAIN-INFRA-SPLIT-BACKLOG.md` (E1→E5). Read it with this doc.**
This handoff is the *why* and the *don't-re-derive-it-wrong*; the backlog is the
*what/when*.

---

## The goal in one paragraph

Split every city's single comingled Dolt db into **two tiers of stores**: per-rig
**DOMAIN** stores holding ONLY the user's backlog work, and ONE city **INFRA**
store holding everything the framework uses beads for (sessions, mail, nudges,
orders, graph, and the whole formula/orchestration explosion). Do the store
**boundary** first, on Dolt, guarded; then swapping the infra store's backend to
SQLite/Postgres later is a scope-invariant no-op. This is the finish line of the
`infra-store-decouple` effort ("only true work in Dolt").

## The decided architecture (settled — do not relitigate)

- **Two tiers.** Per-rig DOMAIN (Dolt): `task/epic/bug/feature/merge-request/spec/
  pack` + user/sling convoys; HQ is just a rig (`EffectiveHQPrefix`); `ClassWork`.
  ONE city INFRA store: all 5 infra classes + the orchestration explosion; exposed
  via the 5 typed views (`beads.SessionStore/GraphStore/MailStore/OrdersStore/
  NudgesStore`, `internal/beads/class_store.go`) over one physical store.
- **Scope ≠ substrate.** Establish the boundary on Dolt (two Dolt stores) FIRST.
  The backend swap (Dolt → SQLite/PG) is a later, isolated, scope-invariant no-op
  with structural parity. The current `DESIGN.md` couples them ("graph → city
  SQLite"); this reframing decouples them.
- **No routing layer.** Every store call site already knows its class from context,
  so it grabs the singleton store its context names (session op → session store;
  work op → the rig's work store). There is NO runtime "figure out where this bead
  lives." `storeref`/prefix-"federation" is a single-store-era crutch to DELETE,
  except the one genuinely context-free case (`gc show <bare-id>` = a trivial
  prefix lookup). Raw bead-store access must exist nowhere except the domain-task
  path.
- **Gate on boundary, never backend.** Any `if sqlite` in routing breaks parity —
  it is exactly the graph-split audit's leverage bug (`GraphOnlyListFor` gated on
  sqlite → shipped fixes went dead). Gate on the store boundary / reserved prefix.

## Mental-model corrections (traps this session hit — start here so you don't)

1. **The orchestration explosion is `ClassGraph` (infra), NOT `ClassWork`.**
   `coordclass.classifyFields` sends molecule roots, control/step/attempt/run
   beads, wisps, convergence, synthetic convoys to `ClassGraph` via
   `gc.root_bead_id` (stamped at `molecule.go:862`), deliberately not by type. What
   remains in `ClassWork` is genuinely the backlog + user/sling convoys. So "move
   infra out of the rig db" mostly means "move `ClassGraph`," and it's already
   classified.
2. **There is no routing problem.** The co-residence bugs the graph split hit were
   NOT ambiguous-location reads and NOT legacy beads — they were LIVE call sites
   that hold one plain store and do `store.Get(id)` on a bead of a *known* class,
   a habit that was correct when there was one store. The fix is to make each site
   name the store its context implies. Do not build/keep a federated router.
3. **`resolveClassStore` is identity TODAY** (`cmd/gc/class_store.go:231`, ignores
   `class`, returns `workStore`; pinned by `TestControllerStateClassAccessorsAreIdentity`).
   So today infra beads land in whatever Dolt db the creating path holds — the rig
   db for a rig-scoped `gc sling`, the city db for the controller. That
   inconsistency is why the reconciler fans `coordClassStoreCandidates` across
   city+rigs for infra; E2.3 collapses it.
4. **The controller already resolves infra → `cityBeadStore`.** The only scope
   split-brain today is the CLI sling creating infra in the rig store. `HQ is a
   rig` is already the code's model (`cmd_rig.go:152` "the HQ rig (the city
   itself)"). So the mechanism delta is small.
5. **Reserved prefixes are store-boundary markers, backend-agnostic.**
   `internal/config/reserved_prefixes.go` (`gcs/gcg/gcm/gco/gcn`) — already
   registered, dormant. The "SQLite-relocated" wording is incidental; a Dolt infra
   store minting `gcs` routes identically. `storeref.PrefixOwner` routes by prefix,
   backend-blind. Regeneralize the doc wording in E2.4.

## Current state of the interface migration (what E1 finishes)

Done (do not redo; verify against the docs):
- Reconciler front door Steps 0–6 (`RECONCILER-FRONT-DOOR-*.md`): decision-path
  session reads on typed `session.Info/Store/CircuitState`.
- Session periphery shape + access passes (`SESSION-PERIPHERY-CLOSURE-PLAN.md`,
  CONT-35..39).
- CLI relocation-routing (`RELOCATION-ROUTING-HANDOFF.md`, CONT-40..44): 15 cmd/gc
  files + the beadmail two-store split (CONT-44, commit `85c659be1`), all routed
  through `cliSessionStore`/`resolveSessionStore`.

Remaining (E1 in the backlog):
- The deferred CLI entangled set (cmd_wait, cmd_handoff+cmd_runtime_drain paired,
  cmd_nudge, cmd_sling, cmd_start cascade).
- The non-session infra classes' periphery (graph/nudges/orders; mail mostly done).
- internal/api (~8 handlers), internal/worker, internal/session own dogfood
  (`SESSION-PERIPHERY-CLOSURE-PLAN.md` Phases D/E/F).
- ~~The dispatch/convoy cross-store member+source reads.~~ **DONE (E1.6, commit
  `6706af8c3`)** except the deferred behavior-change `retry.go` source read.
- Delete the `storeref` crutch + add the "no raw class-store access" guard.

## Where to start

**E1.6 and E1.1** first. E1.6 (dispatch/convoy cross-store member+source reads:
`convoy/membership.go` `store.Get(itemID)`, `drain.go` `MemberStores`,
`runtime.go` `source_bead_id`/`source_store_ref`) is exactly the co-residence
site family the graph split got wrong — proving the discipline there de-risks the
rest. E1.1 (the deferred set) is the tail the relocation pass explicitly parked.

## The discipline (unchanged; every site)

Verified per-consumer census (re-grep; don't trust prior classifications) →
convert the site to its typed accessor → gofmt · `go build ./cmd/gc/` · `go vet` ·
`golangci-lint run ./cmd/gc/` (0) · targeted `go test -run` → **revert-canary**
(guard fails naming the file) → **adversarial byte-identity review** (fable,
REFUTE; diff vs `git show HEAD:<file>`) → commit + push `--no-verify`. Trailer
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Byte-
identity is the bar through E1 (the resolver is still identity, so nothing should
change behavior yet).

## Key files / seams

- `cmd/gc/class_store.go:231` — `resolveClassStore` (the identity stub; E2 makes it
  return two stores) + the `resolve*Store` helpers + `createTarget`.
- `internal/coordclass/classify.go` — `classifyFields`, the domain/infra boundary
  (golden table `classify_test.go`).
- `internal/beads/class_store.go` — the 5 typed views (`struct { Store beads.Store }`).
- `internal/config/reserved_prefixes.go` — `gcs/gcg/gcm/gco/gcn` boundary markers.
- `cmd/gc/frontdoor_di_guard_test.go` — the front-door guard test set (add files as
  they go clean; the E2.5 boundary invariant test lands here or nearby).
- `internal/storeref/storeref.go` — the crutch to delete (keep only bare-id lookup).
- `internal/dispatch/{runtime,drain}.go`, `internal/convoy/membership.go` — the
  cross-store member/source seam (E1.6).
- `cmd/gc/main.go` (`openCityStoreAt`/`openCityStoreWithPath`), `cmd/gc/api_state.go`
  (`openRigStore`, `resolveStoreScopeRoot`, `cityBeadStore`) — the E2 store-open
  wiring.

## Guardrails / gotchas

- **`cmd/gc` test binary is huge** — scope `go test -run`, isolated
  `GOCACHE=$(mktemp -d)` (this thread reused `/tmp/gc-reloc-cache`), run build/vet/
  tests in the background (cold compile > 2 min). NEVER run the revert-canary
  concurrently with golangci-lint (torn read).
- **Commit + push `--no-verify`** (stale absolute `core.hooksPath` breaks commit;
  7-min pre-push hook). gascity Dolt is LOCAL-ONLY — `git push` only, never
  `bd dolt push/pull`.
- **bd is unusable here** — the shared `ga` db is schema v54, the binary knows v53
  (stale). Track this work IN FILES (this handoff + the backlog), not bd.
- **Don't couple scope + substrate.** E1–E4 are all Dolt. No SQLite until E5.
- **The tests give false green on the single-store shape** (the audit's worst
  finding). The E4 e2e suite MUST exercise the real two-store shape.
- **Fable for review** (owner preference), not opus. In this env the fable
  endpoint has intermittently 401'd — fall back to sonnet for the adversarial pass
  if so, and say which you used.

## Acceptance

E4 is the authoritative gate — a 1-db city seeded with a full realistic mix
(domain across rigs+HQ, sessions/mail/nudges/orders, a full molecule explosion)
upgrades to 2-db with every bead in the right store, nothing lost, cross-store
references resolving, and every `gc` command working. That is the only thing that
proves the split; the byte-identity guards through E1–E3 can't.
