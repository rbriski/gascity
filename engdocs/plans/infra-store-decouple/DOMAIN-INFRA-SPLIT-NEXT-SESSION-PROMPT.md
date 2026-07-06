# Next-session prompt — domain/infra store split

Paste the block below into a fresh session.

---

You are continuing the **domain / infra store split** — the finish line of the
`infra-store-decouple` effort. Branch `upstream/object-front-doors-cleanup` (base
`main`, PR #3839 DRAFT), worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`. Run
`git rev-parse HEAD` (should be at/after `7a1869e38`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/DOMAIN-INFRA-SPLIT-HANDOFF.md` — the *why*,
   the decided architecture, and the **mental-model corrections** (read those
   before touching code; three of them are traps that were hit and corrected in
   the design session). **START HERE.**
2. `engdocs/plans/infra-store-decouple/DOMAIN-INFRA-SPLIT-BACKLOG.md` — the ordered
   burn-down (E1→E5) with per-task detail, dependencies, guards, risks.
3. Skim `RELOCATION-ROUTING-HANDOFF.md` (the proven per-site discipline + the
   deferred set) and `SESSION-PERIPHERY-CLOSURE-PLAN.md` Phases D/E/F.

**The goal:** split each city's single comingled Dolt db into per-rig DOMAIN
stores (only the user's backlog work) + ONE city INFRA store (sessions, mail,
nudges, orders, graph, and the whole formula/orchestration explosion). Do the
store BOUNDARY on Dolt first (guarded), then the backend swap to SQLite/PG is a
later scope-invariant no-op.

**Decided principles (do NOT relitigate — see the handoff):**
- Scope ≠ substrate. E1–E4 are all Dolt; no SQLite until E5.
- No routing layer. Every call site already knows its class → it grabs the
  singleton store its context names. `storeref`/"federation" is a crutch to
  delete (except `gc show <bare-id>`). Raw bead-store access nowhere except the
  domain-task path.
- Gate on the store boundary / reserved prefix, NEVER on "is the backend sqlite"
  (that was the graph-split audit's leverage bug).
- The orchestration explosion is already `ClassGraph` (infra), not `ClassWork`.
  `resolveClassStore` (`cmd/gc/class_store.go:231`) is an identity stub TODAY.

**What to do this session — E1 (it BLOCKS E2; do it fully first).** Finish
interface standardization so every store call site names its store, all while the
resolver is still identity (so every change is byte-identical). **Start with E1.6**
(dispatch/convoy cross-store member+source reads — `convoy/membership.go`
`store.Get(itemID)`, `drain.go` `MemberStores`, `runtime.go`
`source_bead_id`/`source_store_ref`; these are the exact co-residence sites the
graph split got wrong) **and E1.1** (the deferred CLI set: cmd_wait,
cmd_handoff+cmd_runtime_drain paired, cmd_nudge, cmd_sling, cmd_start cascade).
Then E1.2–E1.5 (non-session infra classes' periphery; internal/api; internal/worker;
internal/session dogfood), then E1.7 (delete the `storeref` crutch + add the "no
raw class-store access" guard). Do NOT flip the boundary (E2) until E1 is complete
— a missed site silently corrupts the split and the tests won't catch it.

**Discipline (byte-identity is the bar through E1):** per site — verified
per-consumer census (re-grep; don't trust prior classifications) → convert to the
typed accessor → gofmt · `go build ./cmd/gc/` · `go vet` · `golangci-lint 0` ·
targeted `go test -run` → **revert-canary** (guard fails naming the file) →
**fable adversarial byte-identity review** (REFUTE; diff vs `git show HEAD:<file>`;
fall back to sonnet if the fable endpoint 401s, and say which you used) → commit +
push `--no-verify` (trailer `Co-Authored-By: Claude Opus 4.8 (1M context)
<noreply@anthropic.com>`). Add each cleaned file to the front-door guard lists in
`cmd/gc/frontdoor_di_guard_test.go`.

**Guardrails:** `cmd/gc` test binary is huge — scope `-run`, isolated
`GOCACHE=$(mktemp -d)`, build/vet/tests in the background (cold compile > 2 min);
never run the canary concurrently with golangci-lint. Commit AND push
`--no-verify`. gascity Dolt is LOCAL-ONLY (`git push` only; never `bd dolt
push/pull`). **bd is unusable** (shared db is schema v54, binary knows v53) —
track progress by updating the handoff + backlog files, not bd. `#3839` stays
DRAFT.

**When you finish a batch:** update `DOMAIN-INFRA-SPLIT-BACKLOG.md` (check items
off) and add a progress note to `DOMAIN-INFRA-SPLIT-HANDOFF.md`, then commit +
push.

---
