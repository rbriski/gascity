# Next-agent prompt — retire the Router: graph + sessions to class-aware callers

> Paste the block below to launch the agent that completes the refactor. It is
> self-orienting. Branch `plan/decouple-infra-beads` @ `c75bd2ad6`, worktree
> `/data/projects/gascity/.claude/worktrees/infra-store-plan`. 20 local commits, UNPUSHED.

---

You are completing the **infra/beads decouple** by retiring the `coordrouter.Router` for
**graph + sessions**, replacing it with **class-aware callers** (each caller uses the typed
store for the class it operates on). This is the principled end-state the owner asked for
after the Router shipped as the intermediate: *"why doesn't each caller know the class it
wants to read?"* The Router is a workaround for class-agnostic callers; your job is to make
the callers class-aware so the Router's federation + by-id Get-probe can be deleted.

**This is CLEANUP, not a migration blocker.** The maintainer-city Postgres migration runs
fine on the Router intermediate (already prepped). No deadline — do it correctly and
incrementally, byte-identical at the default backend until any owner-gated cutover.

**Read first (authoritative, in order):**
1. `engdocs/plans/infra-store-decouple/RETIRE-ROUTER-CLASS-AWARE-HANDOFF.md` — THE plan
   (the two tracks, the why, the steps, the constraints, the references). Start here.
2. `engdocs/plans/infra-store-decouple/SESSIONS-ROUTER-AND-CUTOVER.md` — how sessions got
   onto the Router (the intermediate you're now dismantling) + §4 the class-aware end-state.
3. `engdocs/plans/infra-store-decouple/FINISH-AND-MIGRATE.md §2` — the retained
   coordrouter-retirement Step 1–5 (this is Track G; Step 1 is already done).
4. Recall auto-memory `infra-beads-decoupling-plan.md` — binding decisions (esp. #3:
   `coordclass` SURVIVES, only `coordrouter` is deleted) + the full session history.

**Ground truth to re-confirm (it drifts):**
- `internal/storeref` (`PrefixOwner`/`Resolve`) is the prefix→store by-id resolver that
  REPLACES the Router's `backendForID` for class-agnostic by-id callers (worker
  `bd close gcg-N`). It already exists.
- The Router is built ONLY in `cmd/gc/api_state.go::routedPolicyStore`. Register sites:
  `ClassSessions` @ `api_state.go:315,319`, `ClassGraph` @ `380,392,413`. The API layer is
  ALREADY class-aware (`SessionsBeadStore()` vs `CityBeadStore()` in `internal/api`).
- `coordclass.Classify` is foundational — keep it.

**Execute in this order (byte-identical at default; keep green; ≤5 files/phase; commit
`--no-verify`):**

1. **Track S — sessions out of the Router FIRST (cleaner; no class-agnostic by-id case).**
   Build/reconstruct the per-function session-bead-op vs work-bead-op inventory (a 10-agent
   recon produced it this session — reproduce it: fan out one agent per controller file
   over `session_beads.go`, `session_reconciler.go`, `session_wake.go`,
   `session_lifecycle_parallel.go`, `session_work_guard.go`, `build_desired_state.go`,
   `cmd_wait.go`, `cmd_session_wake.go`, `cmd_nudge.go`, `city_runtime.go`). Introduce a
   **typed session-store boundary** (ADDITIVE `sessionStore` param or a context struct):
   session/wait ops → session store, work-assignment reads → work store (NEVER the session
   store — that's the mass-closure landmine; the additive design makes it structural).
   Derive `sessionStore := resolveSessionStore(cr.cityBeadStore(), cr.cfg, cr.cityPath,
   cr.rec)` once per tick at the `city_runtime.go` entry points. Then UNREGISTER
   `ClassSessions` from the Router and drop `sessionRelocated` from the Router gate. Restore
   session `bead.*` emission via the typed store's recorder (cutover follow-up; mind the
   cache-baking order in `class_store.go:88-93`). Guard: work reads never touch the session
   store + byte-identity at default.

2. **Track G — graph out of the Router (the deferred Step 2–5).** Wire `storeref.Resolve`
   into the `(storeRef,id)` resolver (`internal/dispatch/runtime.go`) for class-agnostic
   by-id ops (extend the prefix switch). Rewire BOTH graph read paths to a typed graph
   accessor UNDER the net (the `?type=molecule` augment + `ReadyGraphOnly` in
   `internal/api/huma_handlers_beads.go`; the order gate `storesForGate` in
   `cmd/gc/order_dispatch.go`). THEN delete `coordrouter` and fold graph into
   `resolveClassStore` (retire the `ClassGraph` registration). NEVER collapse the
   rewire-then-delete steps. `coordclass` survives.

3. **Verify every phase:** `go build ./...`, `go vet ./...`,
   `make test-cmd-gc-process-parallel`, `go test ./internal/api/ -count=1`, and the
   PG-gated conformance with `GC_TEST_POSTGRES_DSN` (disposable `gc-pg` on :55460 —
   `billing-pg-gb` on :55455 is someone else's). Use workflow-based reviews for the
   conflict-prone / landmine-prone phases (the owner endorses this).

**Hard constraints:** byte-identical at the `bd` default until an explicit owner-gated
cutover; the mass-closure landmine (work reads must never reach the session/graph store —
prove it with a guard test); additive routing, never substitution; `coordclass` survives,
only `coordrouter` dies; commit `--no-verify` (stale `core.hooksPath`); gascity Dolt is
LOCAL-ONLY (never `bd dolt push/pull/remote`); never `tmux kill-server`; never
`go clean -cache` (`-testcache` ok); cold build `GOCACHE=$(mktemp -d) go build ./cmd/gc/`.
The branch has 20 UNPUSHED local commits — push only on the owner's word. The deploy line
(`deploy/sqlite-b36-probe-attribution`) is actively moving; if much time has passed, the
integration merge may need refreshing (see `RETIRE-ROUTER-CLASS-AWARE-HANDOFF.md §2`).

**Do NOT** start the destructive maintainer-city cutover (build/install → stop city →
migrate) — that's a separate owner-gated track (`SESSIONS-ROUTER-AND-CUTOVER.md §6`); this
prompt is ONLY the Router-retirement refactor.
