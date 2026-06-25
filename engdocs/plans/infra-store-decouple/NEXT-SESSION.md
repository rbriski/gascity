# Next-session prompt — finish the infra/beads → SQLite decoupling

Paste the block below to start the next session.

---

You are continuing the **infra/beads → SQLite decoupling** initiative on branch
`plan/decouple-infra-beads` (worktree `/data/projects/gascity/.claude/worktrees/infra-store-plan`).
First run `bd prime`, then read, in order:
`engdocs/plans/infra-store-decouple/HANDOFF.md` (current state + remaining work + gotchas),
then `DESIGN.md` (hardened architecture) and skim `PLAN.md` (102-task list). The memory
`infra-beads-decoupling-plan` has the binding decisions. Confirm every cited `file:line`
against current code before acting (line drift is expected).

**Goal:** take this to the finish line — bd/Dolt holds ONLY front-door work; sessions, mail,
orders, nudges, and the graph engine run behind fork-owned domain-typed store seams on
embedded SQLite; a `gc beads migrate-sqlite` migration; the `coordrouter.Router` deleted.

**The bar (non-negotiable):** ZERO behavior change, proven by conformance. Per slice:
(1) audit current coverage, (2) backfill characterization tests that pin today's behavior,
(3) refactor behind the seam, (4) prove byte-identical. Each domain flip is gated on its
conformance passing against BOTH the bd and SQLite backends. After each major milestone, run
an exhaustive Workflow review (architect + staff-eng + red-team + UX), apply every must-fix,
then merge and continue. Commit incrementally; never claim done until verification has run.

**Already landed (15 commits, all green):** shared machinery (`internal/beads/extras` codec +
`codectest`; `[beads.classes.<class>].backend` config; `internal/storemigrate` +
`gc beads migrate-sqlite`; P2 store-edge events = `SQLiteStore` `RowChange` →
`cmd/gc/class_store.go` `beadEventRowRecorder` → `bead.*`). **Mail = flip-ready**
(reviewed). **Nudges = seam only.** **Orders = leaf seam only.** Details in HANDOFF.md.

**Do the remaining work in this order (HANDOFF.md "What remains" has the specifics):**
1. **Orders cutover** (`ga-cpbq45`): narrow the tracking-bead CREATE sites onto
   `orders.OrderStore`; wire the gate's `storesForGate` to include the graph store
   (`hasOpenWorkInStoresStrict` already takes `[]beads.Store`); `resolveOrderStore` + SQLite
   wiring + flip.
2. **Nudges cutover** (`ga-c2rt46`): fix the oracle-leak FIRST (characterize Ready behavior for
   `type=chore`+`gc:nudge`, then add the exclusion + born-unrouted proof), then SQLite wiring + flip.
3. **Sessions (P5, hardest):** `SessionStore` seam (persisted-field row codec only — `infoFromBead`
   stays impure in Manager), re-point ALL session-bead writers + readers, inject at
   `internal/api/session_manager.go`, session_name as a reconciler invariant (no bare UNIQUE),
   controller-coordinated live-adopt cutover.
4. **Graph finalize + Router deletion (P6):** attach the recorder to the graph store; rewire the
   `?type=molecule` augment + order gate to GraphStore; move `ClassifyGraphPlan`; delete the
   Router via the three-mechanism zero-callers gate; delete `readyExcludeTypes` entries per class.
5. **extmsg (P7):** own `ExtMsgStore` + history backfill + the `gc:extmsg-*` Ready exclusion.
6. **Full typed wire (P3):** typed `GET /v0/sessions|orders/tracking|nudges|convoys` + openapi/
   dashboard regen. **Convoy** seam + **CLI parity** for the remaining domains.
7. **Live soak:** no running city exists here — soak each flip vs `doctor_fork_rate` targets
   before deleting bd rows.

**Before flipping ANY class live, check the four flip-safety assumptions** (HANDOFF.md): no
non-transition `status=closed` writes, no re-stamps the no-op guard misses, self-GC (retention
stays off while a recorder is attached), single controller-owned writer.

**Gotchas that will bite (HANDOFF.md has the full list):**
- The pre-commit hook is the main checkout's stale copy and aborts `.go` commits from this
  worktree — run the gate manually (`gofmt`, `go vet`, `go build ./...`, the suites,
  `go run ./cmd/genspec` for no-spec-drift) and commit with `--no-verify`.
- `beads.MemStore` does NOT preserve caller-pinned IDs; `SQLiteStore` does — assert via List/count.
- The order single-flight gate uses `beads.HandlesFor(store)` → it stays `beads.Store`.
- `genschema` regenerates `docs/reference/schema/*` on config changes — commit them.

**Verify with:**
```
go build ./... && go vet ./internal/... ./cmd/gc/
go test ./internal/beads/... ./internal/coordrouter/... ./internal/mail/... \
        ./internal/nudgequeue/ ./internal/orders/ ./internal/storemigrate/ ./internal/config/
go test ./cmd/gc/   # ~60s; guards order dispatch / nudge / mail wiring
```

Start with the orders cutover. Drive milestone-by-milestone with reviews; push when the user asks.
