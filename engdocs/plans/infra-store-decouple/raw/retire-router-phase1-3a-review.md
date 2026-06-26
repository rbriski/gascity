The only `sessionStore` match near a work-read helper is the doc comment at 2305 (describing the boundary itself), not a call. No work-read helper ever receives a `sessionStore` handle. The Router (`coordrouter`) is still present across 8 files — correct for this intermediate phase where sessions are not yet relocated, exactly as the contract states.

I have independently verified every contract clause against the code. All four lenses are correct: this diff is byte-identical at the default backend and routes every work op to the work store at the relocated backend. The one substantive finding (Lens 4's argument-order asymmetry) is real but not a blocker — I confirmed no transposition exists at any of the 27 call sites, and the guards' internal forwarding correctly re-maps across the reversed orderings. My adjudication:

---

# ADJUDICATION: Phase 1-3a Review — coordrouter.Router Retirement (Sessions Class-Awareness)

## VERDICT: **SAFE-TO-PROCEED**

All four lenses returned `clean`. I independently re-derived the routing of all seven changed functions from source and traced all 27 non-test call sites. **The critical failure mode — a WORK-assignment read or write routed to a session store — is not present anywhere in the diff.** No blocker findings stand up to code inspection.

---

## 1. MUST-FIX (blockers): **NONE**

A finding qualifies as a blocker only if a concrete work op reaches a session store, or a correctness break manifests at the relocated backend. No lens raised one, and my own trace confirms none exists:

- **The mass-closure boundary (`closeBead`, session_beads.go:2310-2354) is wired correctly.** Session legs → `sessionStore` (`Get` 2328, `setMetaBatch` 2335, `Close` 2338, `cancelState` 2349); work-release leg → `workStore` (`releaseWorkFromClosedSessionBead` 2351). `releaseWorkFromClosedSessionBead` (2366) does its `store.List`/`store.Update` on the passed handle = `workStore`. Verified: `grep` for `sessionStore.List|sessionStore.Update` returns **NONE** package-wide; `releaseWorkFromClosedSessionBead` has exactly one call site (2351), always with `workStore`.
- **All three close-family guards keep the work read on the WORK store.** `sessionHasOpenAssignedWorkForConfig(store, rigStores, …)` / `…ForReachableStore(…, store, rigStores, …)` at session_work_guard.go:40,74 and session_beads.go:2215,2226 — all read `store` (WORK), never `sessionStore`. The WORK-read helpers and `workAssignmentStores` (session_beads.go:611) have signatures **untouched** by the 3 commits (verified via `git diff`) and build their store list from `store`+`rigStores`, never a session handle.
- **State helpers split per contract** (session_beads.go:745, 767): durable waits → `sessionStore` (`ReassignWaits`, `CancelWaits`, `ListSessionWaitBeads`, `stampWaitLookupCapDiagnostic`); extmsg bindings/participants (no relocation seam) → `workStore`.
- **Retire-named mixed functions** (463/464, 533/534): WORK-reassign/unclaim legs left on `store`+`rigStores` unchanged; only the WAIT-state helpers gained the second store arg.
- **Byte-identity holds.** Every non-threaded caller passes `(store, store)` (or forwards a pair bottoming out at `(store, store)`); the two MemStore handles only diverge inside the new regression test.

---

## 2. WARNINGS WORTH ADDRESSING (non-blocking, defer to later phases)

1. **Argument-order asymmetry between the two function families** (Lens 4 — the one finding worth real scrutiny). The close-family takes `(sessionStore, workStore, …)` (session first) while the guards take `(store /*WORK*/, sessionStore, …)` (work first). **Confirmed correct today** — every guard call passes `(store, store)` and forwards `closeBead(sessionStore, store, …)`, re-mapping correctly across the reversed orderings — but once the stores diverge, a maintainer threading a real `sessionStore` could transpose the two, and that transposition is *exactly* the invisible mass-closure misroute. **Fix (later phase):** reorder the `closeSessionBeadIf*` guards to `(sessionStore, workStore /* =store */, rigStores, …)` to match the close-family before sessions are actually relocated.

2. **No two-store test locks the wait→sessionStore / extmsg→workStore split** (Lens 3 nit). The new `TestCloseBeadRoutesSessionAndWorkLegsToSeparateStores` (test:4445) correctly proves the session-close vs work-release boundary (decoy work on the session store survives — the precise regression). But the `cancelState`/`reassignState` wait/extmsg split is still only exercised with `(store, store)`. **Fix (later phase):** add a two-store test seeding a wait bead in `sessionStore` and an extmsg binding in `workStore`; assert `cancelState` cancels the wait from `sessionStore`, closes the binding in `workStore`, and leaves a decoy binding on `sessionStore` untouched.

3. **Benign session-op-on-work leaks (documented, intentional, NOT blockers).** `stopRuntimeBeforeSessionBeadMutation(store, …)` (session_beads.go:2223) is a session-class op left on the work store, explicitly deferred. The extmsg legs in `cancelState`/`reassignState` are deliberately work-routed (no relocation seam). Per the contract these are the benign direction (a misrouted session op cannot close a live session). Thread `sessionStore` into `stopRuntimeBeforeSessionBeadMutation` in the phase that relocates session-bead mutations.

---

## Verification performed
- Read all seven changed functions + `releaseWorkFromClosedSessionBead` + `workAssignmentStores` + `sessionHasOpenAssignedWork*` in source.
- Traced all 27 non-test call sites: every position correct, zero transpositions, all non-threaded callers pass `(store, store)`.
- `git diff 043ac2d7a~1..58072d1a3` confirms WORK-helper signatures untouched; diff scoped to 6 files (5 non-test session files).
- `GOCACHE=$(mktemp -d) go build ./cmd/gc/` → exit 0.
- `go test ./cmd/gc/ -run '…close-family…'` → ok (1.5s), including the new boundary test.
- Negative sweeps: no `sessionStore.List/Update` anywhere; no WORK-read helper ever receives a `sessionStore` arg; `coordrouter` still present in 8 files (correct — sessions not yet relocated this phase).

Relevant files: `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_beads.go`, `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_work_guard.go`, `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_reconciler.go`, `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_lifecycle_parallel.go`, `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/pool_session_name.go`, `/data/projects/gascity/.claude/worktrees/infra-store-plan/cmd/gc/session_beads_test.go`.
