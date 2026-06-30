# Release Gate: hook-claim pool graph.v2 root nudge

Bead: ga-sxe4sc.2
Candidate branch: fix/ga-cx1hbq-hook-claim-pool-graph-v2-nudge
Candidate input head: 4daf5e1fda5151d2b3f1f8eda99e18198131a579
Base: origin/main
Gate run: 2026-06-29T23:16:51-07:00

`docs/PROJECT_MANIFEST.md` was not present in this Gas City checkout. This
gate uses the deployer release criteria from the active rig prompt.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Source review bead ga-an3le0 contains `REVIEW VERDICT: PASS` from gascity/reviewer for commit 3ec937bd30c325e8f827c47d92babd50e97e9a45. Parent deploy bead ga-sxe4sc records the same reviewed PASS and was split only because the original branch mixed independent themes. |
| 2 | Acceptance criteria met | PASS | Builder handoff ga-sxe4sc.1 produced clean branch `fix/ga-cx1hbq-hook-claim-pool-graph-v2-nudge` at 4daf5e1fda5151d2b3f1f8eda99e18198131a579. Diff against origin/main is limited to hook-claim continuation nudge behavior and tests. |
| 3 | Tests pass | PASS | `go build ./cmd/gc` passed. `go vet ./...` passed. Focused hook-claim/nudge tests passed: `ok github.com/gastownhall/gascity/cmd/gc 20.985s`. Full rig baseline `make test` passed with `observable go test: PASS log=/tmp/gascity-test.jsonl.yzmc4z`. |
| 4 | No high-severity review findings open | PASS | Review bead ga-an3le0 records Style, Security, Architecture, Spec Compliance, and Coverage as PASS; no HIGH findings are recorded. |
| 5 | Final branch is clean | PASS | Clean gate worktree before the gate: `git status --short --branch` showed only `## HEAD (no branch)`. The gate file is the only added artifact. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-base --is-ancestor origin/main fix/ga-cx1hbq-hook-claim-pool-graph-v2-nudge` exited 0. |
| 7 | Single feature theme | PASS | Diff is one hook-claim release theme: enqueue a continuation nudge when a pool session self-claims a graph.v2 workflow root, plus regression coverage and shared metadata constants. No extmsg/SSE/API/dashboard files are present. |

## Candidate Diff

`git log origin/main..HEAD --oneline`:

```text
4daf5e1fd fix(hook-claim): enqueue continuation nudge for self-claimed pool graph.v2 roots (ga-cx1hbq)
```

`git diff --name-status origin/main..HEAD`:

```text
M	cmd/gc/cmd_hook_claim.go
M	cmd/gc/cmd_hook_claim_test.go
M	cmd/gc/session_progress_test.go
M	internal/beadmeta/values.go
M	internal/beadmeta/values_test.go
M	internal/graphroute/graphroute.go
```

## Commands

```text
git fetch origin main
git merge-base --is-ancestor origin/main fix/ga-cx1hbq-hook-claim-pool-graph-v2-nudge
go build ./cmd/gc
go vet ./...
go test ./cmd/gc -run 'TestHookClaimPoolGraphV2Root_EnqueuesNudge|TestHookClaimNonGraphV2Bead_NoNudge|TestHookClaimPoolStepBead_NoNudge|TestHookClaimNamedSessionBead_NoNudge|TestHookClaimPoolGraphV2Root_NudgeError|TestHookClaimPoolGraphV2Root_NilNudgeSeam|TestHookClaimJSONPassesRootJSONContract|TestPoolGraphV2AutoAdvance_NudgeEnqueuedAtClaim|TestSessionProgressStalled'
make test
```

Gate result: PASS.
