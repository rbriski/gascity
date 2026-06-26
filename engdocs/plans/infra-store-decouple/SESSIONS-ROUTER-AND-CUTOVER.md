---
title: "Sessions on the Router — DONE + the cutover-prep (supersedes the seam plan)"
date: 2026-06-26
branch: plan/decouple-infra-beads
head: 3a6396ee7
epic: ga-pd6tcg
---

> **What changed (owner decision, 2026-06-26):** the planned per-subsystem seam for
> sessions (thread a `sessionStore` param through ~141 controller functions) was
> REPLACED — after an adversarial design review (workflow `wf_862d08cb`) — with the
> proven **graph Path-A pattern: register `ClassSessions` on the existing
> `coordrouter.Router`**. The owner approved this as the intermediate and flagged the
> 141-function pervasiveness as "a broad architecture issue"; the principled
> end-state is class-aware callers (see §4). This doc supersedes the sessions portions
> of `SESSIONS-P5A.md` / `FINISH-AND-MIGRATE.md §3`.

## 1. DONE this session (7 commits `49f921757..3a6396ee7`, byte-identical at default)

| Commit | What |
|---|---|
| `49f921757`..`fd6d0ac55` | **Step 4 (API):** every `internal/api` session/wait site → `SessionsBeadStore()`, in 4 sub-phases. Found + fixed scope the verified list MISSED on BOTH sides: the materialize work-entanglement chokepoint (`reassignContinuityIneligibleNamedSessionState` now sources the work store for work/extmsg reassign, session store for waits) AND 7 inverse-leak session-resolution sites in stay-on-city handlers (handler_beads/mail/extmsg, worker_operation_watch, session_runtime). Full `internal/api` suite green. |
| `043909f38` | **Step 5 (controller):** `registerSessionStoreBackend` + `sessionRelocated(cfg)` + `routedPolicyStore` builds the Router when graph OR sessions relocated. ~40 fork-owned lines in `api_state.go`, NO 141-fn threading. Keystone `TestSessionStoreBackendRoutesSessionAndWorkSplit`. |
| `691d3ba4f` | **Step 7:** classed-store conformance for `ClassSessions` (SQLite + Postgres, gc-pg-verified) + wait-bead round-trip + coordclass federation-correctness golden cases. |
| `3a6396ee7` | **Phase C:** tier + Ready-leak guards. Sessions are triply-safe (see §3). |

**Net:** sessions now relocate exactly like graph/nudges/orders/mail — flip
`[beads.classes.sessions].backend` and the city store routes them. No controller churn.

## 2. Why the Router is correct for sessions (verified, not assumed)

- `coordclass.Classify` already maps `type=session`/`gc:session` AND `gc:wait`/`type=gate`
  → `ClassSessions` (classify.go); so `Router.Create` routes session/wait beads to the
  registered backend automatically.
- `Router` **federates** `List`/`Ready` (router_federation.go) and routes by-id ops by
  **federated Get-probe** with a prefix short-circuit (router_mutation.go `backendForID`).
  So the ambiguous `List(Assignee,Status)` work-reads the close family issues resolve
  correctly via the EXISTING post-filters (`IsSessionBeadOrRepairable`,
  `hasNonSessionAssignedWork`) — the "fatal flaws" the attack agents found were against a
  strawman route-by-query adapter, NOT the federating Router.
- There is **no cross-class `Tx`** in the close family (it's sequential best-effort with an
  idempotent next-tick fallback), so nothing is downgraded.
- The mass-closure landmine is structurally impossible: work-reads route to the work
  backend by class/federation; they never hit an empty session store.

## 3. Phase C is a NON-ISSUE for sessions (unlike the seam classes)

`routedPolicyStore` wraps the Router as `policy(Router(...))`, so the policy computes the
no-history tier and `Router.CreateWithStorage` forwards it to the session backend — sessions
do NOT have the seam's RAW-store tier divergence. Belt-and-suspenders: `readyExcludeTypes`
excludes `session` AND `gate` (store-independent), plus the `gc:session` label exclusion.
Guards: `TestSessionBeadsRelocateOnNoHistoryTierThroughPolicyRouter`,
`TestRelocatedSessionBeadsNeverLeakIntoReady`. (The seam-relocated nudge/mail/order classes
still rely on their label exclusions — already in place per prior work — that is THEIR Phase C.)

## 4. The principled end-state (owner's architecture point — follow-on, NOT done)

The owner is right that "each caller should know the class it wants." The Router is the
pragmatic intermediate; it federates/probes BECAUSE the controller's callers don't declare
their class (they thread a raw `beads.Store`, a legacy of the single-store era). The API
layer already moved to class-awareness in Step 4 (`SessionsBeadStore()` vs `CityBeadStore()`).
The follow-on is to make the **controller** class-aware the same way — typed session-store
boundary at the call sites that know they're touching session beads — so the Router's
federation/probe can eventually be retired. Track as a separate initiative; do NOT do it
under the migration deadline.

## 5. Cutover-prep — sessions-specific deltas (the rest is unchanged in FINISH-AND-MIGRATE §5)

The owner-gated cold migration (stop city → provision PG → set backends → migrate → bring up)
is unchanged EXCEPT:

1. **Set `[beads.classes.sessions].backend="postgres"`** (the Router registers the gcs backend;
   validator already allows it). No `graph_store`-style legacy knob needed.
2. **Migrate session beads dolt→pg** via `gc beads postgres migrate` (ID-preserving, idempotent;
   the migration selector uses `coordclass.Classify`, so session/wait beads are selected and
   moved). Sessions cold-migrate (city stopped) — no live-adopt needed.
3. **By-id federation cost (verify, likely cheap):** migrated session beads keep their
   `ga-`/`gc-` ids, so `backendForID` prefix-routes to the WORK backend first, misses, then
   federates to the session backend. The work `Get` goes through the in-memory `CachingStore`
   (api_state.go:198) → a cache miss is in-memory, NOT a `bd` exec → cheap. CONFIRM at cutover
   on maintainer-city; if the work backend is an uncached `bd` exec on that path, consider a
   `gcs-` re-prefix at migrate (NB: re-prefix needs reference fixup — wait `session:<id>` labels,
   extmsg bindings; work-assignment matches by session_name/alias, mostly prefix-stable).
4. **Event emission (decide before live cutover):** the relocated session backend is registered
   event-SILENT (nil recorder, like graph) because the Router is built at the shared
   controller+worker chokepoint (no controller recorder) and the class-store handle caches the
   recorder at first open. So relocated session writes won't emit `bead.*` (the generic bead
   feed / cache observers). Session LIFECYCLE events come from the reconciler independently.
   Before flipping maintainer-city: confirm the dashboard session view consumes `session.*`
   (API SSE) not raw `bead.*`, OR thread the recorder (re-register ClassSessions with the
   controller's recorder in the api_state.go existingRouter branch — note the cache-baking
   order constraint in class_store.go:88-93).

## 6. Remaining owner-gated steps (unchanged, DESTRUCTIVE — confirm each)

Rebase onto the live branch (`deploy/sqlite-b36-probe-attribution`, re-confirm tmux window 3)
→ build + install the `dev-2a83e20bd` way → **stop maintainer-city** → `gc beads postgres init`
+ optimize → set the infra-class backends (incl. sessions now) → `gc beads postgres migrate`
→ fixup refs if needed → bring up + babysit → tune PG. See `FINISH-AND-MIGRATE.md §5` + the
HANDOFF for the full mechanics and constraints (commit `--no-verify`; Dolt local-only; never
force-migrate rig DBs; never `tmux kill-server`; never `go clean -cache`).

## 7. Push status

17 local commits, branch `plan/decouple-infra-beads` (local-only, no upstream). Per the
standing constraint, push only on the owner's word.
