# Next-session prompt — infra-store decouple (object-model front door)

Paste the block below into a fresh session.

---

Continue the **object-model front-door migration** on branch
`upstream/object-front-doors-cleanup` (base `main`, DRAFT PR #3839, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`; run `git rev-parse HEAD` —
should be at/after `e3031d8b8`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/INFRA-STORE-SESSION-HANDOFF.md` — the top-level
   session handoff: what the last session did (shape pass finished + proven exhausted; access
   pass batches 1–2), the current state of the three CI guard lists, the two distinct
   properties (store-free DI vs relocation-safe), and what's next. **START HERE.**
2. `engdocs/plans/infra-store-decouple/ACCESS-PASS-HARD-RIPPLE-HANDOFF.md` — the detailed
   handoff for the immediate next work (`cmd_session_wake.go`, `cmd_session.go`): the two proven
   byte-identical patterns, the already-decided exclusions, per-file blockers + options.
3. `engdocs/plans/infra-store-decouple/SESSION-PERIPHERY-CLOSURE-PLAN.md` Progress log
   (CONT-37→39) — the live status.
4. Memory `infra-beads-decoupling-plan.md` CONT-37/38/39.

**Immediate work: the access-pass HARD-ripple tranche.** Follow
`ACCESS-PASS-HARD-RIPPLE-NEXT-SESSION-PROMPT.md` for the scoped plan +
discipline. In short: `cmd_session_wake.go` and `cmd_session.go` are command-ROOT collections
(raw-bead escapes on the wake bead; a cross-class `rigStores` map) — split any pure receiver
LEAF helpers into store-free companion files (the proven SRP pattern), leave the roots unlisted.

**KEY DECISIONS (do not relitigate):** reach-through `store := sessFront.Store().Store` for
byte-identity (NOT the typed `sessFront.Get`); composition roots are "intentionally not listed"
(SRP-split their leaves, chosen over a guard-gaming factory); cmd_prime + session_resolve +
session_template_start are EXCLUDED as root/spine infra.

**FIRST, surface to the owner (open architectural question):** do the command ROOTS warrant
store-free listing at all, or is their real goal **relocation-safety** — better met by routing
each root's store through `resolveSessionStore` (a separate axis from the store-free guard)?
Decide the target property before grinding the HARD roots.

**Discipline (byte-identity is the bar):** per companion — move receiver leaves verbatim,
convert `store beads.Store`→`sessFront *session.Store` (reach-through), root passes
`sessionFrontDoor(store)` + prune unused imports, wrap test call sites, add the COMPANION to
`frontDoorStoreFreeFiles` → build/vet/`golangci-lint 0`/gofmt/targeted tests → **revert-canary**
(inject a `beads.Store` decl; guard must fail) → **fable adversarial behavior-identity review
BEFORE commit** (`model:'fable'`, effort high, REFUTE; diff moved funcs vs `git show HEAD:<root>`)
→ commit + push `--no-verify`. Trailer
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`. Update the plan Progress
log + memory as files close.

**Guardrails:** `cmd/gc` test binary is huge — scope `go test -run`, isolated
`GOCACHE=$(mktemp -d)`, run build/vet/tests in the background (cold compile > 2 min). Never run
the canary concurrently with golangci-lint (torn read). `git push` always `--no-verify` (7-min
pre-push hook). gascity Dolt LOCAL-ONLY — `git push` only. `#3839` stays DRAFT. For `internal/api`,
read `engdocs/architecture/api-control-plane.md` first.

---
