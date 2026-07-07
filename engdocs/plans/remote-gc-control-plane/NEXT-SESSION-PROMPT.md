# Kickoff prompt for the next session

Paste the block below into a fresh session started **from the worktree
`/data/projects/gascity/.claude/worktrees/gc-remote`**.

---

We're implementing remote-city support for the `gc` CLI. The design is finished and
human-ratified — do NOT redesign. Continue the implementation.

Start by reading, in this order:
1. `engdocs/plans/remote-gc-control-plane/HANDOFF.md` — operational state + exact next steps.
2. `engdocs/plans/remote-gc-control-plane/DESIGN-BRIEF.md` — the contract. §1 = the 9 locked
   decisions (do not relitigate them), §3 = the gated build checklist (G1–G23), §8 = residual risks.

Then confirm the one landed unit is green before touching anything:
`go test ./internal/clientcontext/ && go vet ./internal/clientcontext/`

Then implement **Slice 0, Phase 1** exactly as the handoff §3 specifies: the `gc context`
command family (`cmd/gc/cmd_context.go`, modeled on `cmd_register.go`) plus the shared remote
target resolver (`cmd/gc/remote_target.go`: `DefaultPath()`, `resolveRemoteTarget()`, precedence,
loud conflict errors, credential-to-target binding), wired into `cmd/gc/main.go` (flags
`--city-url/--city-name/--context` + the remote head-branch in `resolveContext()` + the
capability gate) and `cmd/gc/apiroute.go` (step-0 remote branch BEFORE the `GC_NO_API` nil-return).

Work TDD (test first, watch it fail, make it pass), ≤5 files per phase, and run
`go test ./<pkg>/ && go vet ./<pkg>/` green before moving to the next phase. Honor the
landmines in handoff §4 — especially: no-fallback is a nil-safe `*Client` PARAMETER (compiler-
enforced), the three fallback planes (`GC_NO_API`, events `.jsonl`, localhost) each reroute a
remote op to LOCAL disk, do NOT enable any remote read command until no-fallback (G1) is in place,
and the env names are `GC_CITY_CONTEXT` / `GC_CITY_URL_TOKEN` (collision-avoidance).

Rules: NEVER `go clean -cache` (shared fleet cache); use bd for task tracking, not markdown TODOs;
gascity Dolt is local-only (no `bd dolt push/pull`); commit/push only when I ask; work only in this
worktree. Checkpoint after Slice 0's read plane is exercisable end-to-end (`gc --context … status`
hard-failing, never local-falling-back) and get my sign-off before starting Slice 1 (the capstone).

Use Opus for implementation; if you need design/red-team judgment, use fable-based subagents/workflows.

---
