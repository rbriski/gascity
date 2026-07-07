# Domain / Infra store split — handoff

**Branch** `upstream/object-front-doors-cleanup` (base `main`), **PR #3839 DRAFT**,
worktree `/data/projects/gascity/.claude/worktrees/object-front-doors`.
**HEAD moves fast — always `git rev-parse HEAD`; re-grep every line number below.**

> **AUTONOMOUS RUN (2026-07-06, "complete the full two-DB refactor, don't stop until
> done; Fable for design, opus for impl, fable red-team for review").** Pipeline per
> unit: fable DESIGN workflow (controller-validated plan) → opus IMPLEMENTATION (main
> ctx or opus subagent) → fable RED-TEAM workflow (adversarial byte-identity + class
> review) → fix → commit+push. **E1.6 DONE + E1.1 DONE (all 5 CLI files).** The red-team
> caught real defects at cmd_start (census wrongly deferred buildDesiredState) and
> cmd_nudge (session store derived from the nudges base) — both fixed; this is why the
> red-team gate is non-negotiable.
>
> **STRATEGY for the rest (E1.2–E1.5, E1.7, E2–E4).** E1.2 (graph/nudges/orders CLI
> periphery), E1.3 (~25 internal/api), E1.4 (internal/worker), E1.5 (~31 internal/session
> dogfood) are large. RATHER THAN blind-route all 50+ files, the reliable completeness
> gate is **E2.5's boundary-invariant test (classify every bead in each store, fail on
> a domain/infra mismatch) run on the REAL two-store shape (E4)** — misses are CAUGHT,
> not silent. So: do the tractable/obvious E1.2 routing, then BUILD E2 (two Dolt stores
> + non-identity resolveClassStore + the boundary-invariant test) and E4 (e2e on the
> two-store shape), and let the invariant test DRIVE completeness (TDD: it fails → route
> the surfaced site → passes). The controller is already fully class-routed; E1.1 closed
> the CLI session/mail/nudge/wait paths; the invariant test finds the residual graph/
> orders/api/session-dogfood misses more reliably than a blind sweep.
>
> **E2.1 MECHANISM LANDED (commit `8a529ce23`, byte-identical, fable-red-teamed).**
> Design of record: `raw/e2-design-wf_d170b33a.json` (`e2` object) + E1.2 census (`e12`).
> KEY INSIGHT: a "store" is a SCOPE-ROOT dir (its own Dolt db on the shared server), so
> the city INFRA store is just a 2nd scope `.gc/infra` — NO beads-factory change. E2.1
> added the `infraStore` param to resolveClassStore (class_store.go) with boundary
> dispatch (infra classes→infra store, work→work store; gate on infraStore-presence +
> class, NEVER backend) + infraScopeRoot/cityHasInfraStore/openCityInfraStoreResultAt/
> cachedCityInfraStore (main.go, same policy-wrap+CachingStore path) + controllerState.
> cityInfraStore / CityRuntime.standaloneCityInfraStore + accessor threading + the
> identity/routing test split. INERT until E2.4 creates the scope (cityHasInfraStore
> false → nil → identity).
>
> **DORMANT DEFECTS — MUST FIX BEFORE E2.4 flips any city (split-city-only, cannot
> trigger today; full detail in commit `8a529ce23`):** (1) api_state.go update()
> use-after-close when infra reopens but city fails; (2) update() provider/accessor
> split-brain when infra reopen FAILS while city succeeds (compute effective retain-old
> infra store before rebuilding the provider; distinguish present=false from failure);
> (3) cachedCityInfraStore caches open-error as nil permanently + is reachable in the
> controller (circuit-reset socket → cliSessionStore) → controller paths must source
> cs.cityInfraStore, not the memoized handle.
>
> **E2 REMAINING (implementationOrder in the design):** E2.1-harden (3 dormant defects)
> → **E2.2 = write the boundary-invariant test FIRST (TDD forcing function:
> TestNoInfraBeadInDomainStore/TestNoDomainBeadInInfraStore; its failing list IS the
> E2.3 worklist)** → E2.3 (sling/order split-brain: SlingDeps.GraphStore + nudge enqueue
> + coordClassStoreCandidates infra-arm collapse + openSourceWorkflowStores, gated on
> cityHasInfraStore) → E2.4 (gc init/start infra-scope seed + reserved prefixes gcg/…) →
> E2.5 (integration invariant test + TestClassStoreDispatchIsBackendBlind guard + backend
> audit). Then E3 (migrate 1-db→2-db) + E4 (e2e). **E1.2 census done** (18 graph/orders/
> nudges CLI sites in `e12`: cmd_convoy_dispatch/molecule_autoclose/wisp_autoclose/
> cmd_formula/cmd_order/order_store/nudge_beads; add cli_class_store.go cliGraphStore/
> cliOrderStore seam like cliSessionStore). E1.3(api)/E1.4(worker)/E1.5(session-dogfood):
> let the E2.2 invariant test drive.
>
> **E2.1-harden / E2.2 / E2.3 ALL DONE (commits `98bbdffb5`, `311d8ca12`, `41279174d`).**
> E2.1-harden fixed the 3 dormant defects (fail-on-baseline tests). E2.2 landed the
> BOUNDARY-INVARIANT TEST (`cmd/gc/infra_store_boundary_invariant_test.go`: real two-store
> harness, classify every bead via coordclass.Classify, negative control) — the TDD
> forcing function. E2.3 routed sling/order graph+nudge creation to the infra store
> (new `cmd/gc/cli_class_store.go` cliGraphStore/cliOrderStore/cliNudgesStore;
> slingSplitGraphStore = infra on split, nil on legacy = byte-identical). **THE INVARIANT
> TEST IS NOW FULLY GREEN — the write-side split is PROVEN CORRECT** (session/mail/nudge/
> wait/order/graph/sling→infra; task/convoy→domain). The MECHANISM is complete + byte-
> identical + red-teamed.
>
> **NEXT = E2.4 (ACTIVATION — the riskiest step; needs INTEGRATION testing, real dolt).**
> gc init/start seed the `.gc/infra` scope via initAndHookDir + reserved prefixes (gcg/…);
> once a city HAS `.gc/infra`, cityHasInfraStore→true and the split goes LIVE. Design:
> `raw/e2-design-wf_d170b33a.json` newCityCreation + reservedPrefixes + implementationOrder
> E2.4. Then E2.5 (integration-tier invariant test + TestClassStoreDispatchIsBackendBlind
> guard + backend-gating audit), E3 (migrate existing 1-db→2-db, idempotent), E4 (exhaustive
> e2e on the two-store shape — the authoritative gate). **DEFERRED read-side fan-out
> (byte-identical today, in backlog): coordClassStoreCandidates session-arm collapse (thread
> infra store into 3 build_desired_state.go sites) + openSourceWorkflowStores (split graph-
> read from by-id-work use).** E1.2 partial (sling/order done in E2.3; autoclose/formula-cook/
> cmd_convoy_dispatch graph reads remain — let the E2.5 integration invariant surface them).
> E1.3/E1.4/E1.5: let the invariant tests drive.
>
> **E2 + E3 COMPLETE + INTEGRATION-VALIDATED ON REAL DOLT (2026-07-07).** The earlier
> "blocked on a dolt/bd version skew" was WRONG — a pure harness bug (doltlite vs the
> working managed-Dolt harness setupManagedBdWaitTestCity). Commits: E2.4 activation
> `0537c8b03`; E2.5 integration test PASSES on real managed Dolt `9d673e811`; E2.5
> backend-blind guard `bc7d4cdf9`; E3 migration `e34971672` (both TestInfraStoreMigrate
> Integration 184s + CrashResume 149s PASS). **THE TWO-DATABASE REFACTOR IS FUNCTIONALLY
> COMPLETE + PROVEN ON REAL DOLT:** new cities split via E2 (GC_INFRA_STORE_SPLIT=1,
> boundary invariant green end-to-end), existing cities migrate in place via
> `gc migrate infra-store` (copy-then-delete, crash-safe, id-stable, cross-store deps
> preserved). Reserved prefix = gcg (scope-prefix-only; bd rejects cross-prefix --id
> without --force → ForeignIDCreator). New primitive beads.BatchDeleter.DeleteAllOrphaning
> (raw SQL DELETE — bd delete text-rewrites staying neighbors, verified live). E3 design:
> E3-MIGRATION-DESIGN.md.
>
> **REMAINING = E4.4 command-sweep e2e (E4.1/2/3/5/6 are covered by the E2.5 + E3
> integration tests) + polish:** E4.4 = run a representative gc command sweep on a
> two-store city, assert each succeeds AND the boundary invariant holds after (no
> leakage). Then the DEFERRED read-side fan-out (coordClassStoreCandidates session-arm
> collapse + openSourceWorkflowStores graph-vs-by-id split — byte-identical today) and
> the E1.2 tail (autoclose/formula-cook graph reads) + E1.3/E1.4/E1.5 — the boundary
> invariant + E4.4 sweep surface any real leak. Integration tests: `GC_FAST_UNIT=0 go
> test -tags integration ./cmd/gc/ -run 'InfraStore|Migrate' -timeout 20m` (managed Dolt).
> **HEAD ~`e34971672`.** Next: E4.4 command sweep, then read-side fan-out + E1 tail.

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
> of the callee, and not the census.**
>
> **CONT (same session) — E1.1 cmd_sling.go DONE (commit `cea22bf59`).** Routed the
> sling-nudge session arm (doSlingNudge lookups; deliverSlingNudge observe/handle/stamp via
> a sessStore from target.cfg+cityPath, NO signature change; printNudgePreview +cityPath
> param) through cliSessionStore/cliSessionFrontDoor; queued-nudge enqueue stays plain.
> **Census wrong a 3rd time** (called deliverSlingNudge "clean-surgical"; it is multi-class
> session+nudge). Fable COULD-NOT-REFUTE; documented 2 DEFERRED cross-package sling-root
> session sites (cliDirectSessionResolver, resolveGraphStepBinding*).
>
> **OWNER DECISION: complete ALL of E1, SEQUENTIAL FULL-QUALITY** (one verified+pushed
> commit per file/group; NO worktree fan-out; per file: re-verified per-consumer census
> vs the DAEMON/controller routing + byte-identity + revert-canary + fable review).
> E1 done so far: E1.6 (2 fixed, 1 deferred), E1.1 cmd_start + cmd_sling. **Remaining E1
> roadmap:** E1.1 → cmd_nudge, cmd_wait, cmd_handoff+cmd_runtime_drain; then E1.2 (graph/
> nudges/orders periphery), E1.3 (~25 internal/api), E1.4 (internal/worker), E1.5 (~31
> internal/session dogfood), E1.7 (delete storeref crutch + invariant guard).
>
> ### NEXT UNIT — cmd_nudge.go census (ready to execute)
> 2460 lines, multi-class. The ~8 PURE nudge-queue roots stay on the NudgesStore (leave
> untouched): claimDueQueuedNudgesMatching@1684, listQueuedNudges@1724,
> listQueuedNudgesForTarget@1764, enqueueQueuedNudgeWithStore@1857, ackQueuedNudgesWithOutcome@1959,
> releaseQueuedNudgeClaims@2012, recordQueuedNudgeFailureDetailed@2071 (all open
> `openNudgeBeadStore(cityPath)` → nudgeFrontDoor / markQueuedNudgeTerminal only).
> The DELIVERY roots open the nudge store AND reuse its `.Store` for SESSION ops — route
> those SESSION sites to `cliSessionStore(<nudgeStore>.Store, cfg, cityPath)` /
> `cliSessionFrontDoor(...)`, keep the nudge enqueue/record/ack on the NudgesStore:
>   - cmdNudgeDrainWithFormat@452: `sessionFrontDoor(deliveryStore.Store)`@455 → cliSessionFrontDoor; stamp@517/524.
>   - deliverSessionNudge@697: `workerHandleForNudgeTarget`@744; `requestManagedNudgeWake`@843 → `session.WakeSession(store,…)`@860 (SESSION write).
>   - sendMailNotify@1019: workerObserveNudgeTarget@995/1033, workerHandleForNudgeTarget@1038, `sessionFrontDoor(store)`@1050→stamp@1052.
>   - resolveNudgeTarget@1087: `resolveSessionIDMaterializingNamed(cityPath,cfg,store.Store,…)`@1089.
>   - the @1260 delivery root: `sessionFrontDoor(deliveryStore)`@1264, workerHandleForNudgeTarget@1297, stamp@1329.
>   - withNudgeTargetFence@1412: `loadSessionBeads(store)`@1422 (SESSION; shared with cmd_sling — leaf, route at call sites).
> Shared session leaves (workerObserveNudgeTarget@926, workerHandleForNudgeTarget@874,
> requestManagedNudgeWake@852, stampLastNudgeDeliveredAt@1333) take a `store` param and are
> ALSO called from cmd_sling.go (already routed there) — keep signatures, route at each
> call site. **VALIDATE the session/nudge split against nudge_dispatcher.go** (the controller
> delivery analog) before editing — the census was wrong on cmd_start AND cmd_sling.
> Guard-list cmd_nudge.go once its `sessionFrontDoor(store...)` needles are converted.

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
