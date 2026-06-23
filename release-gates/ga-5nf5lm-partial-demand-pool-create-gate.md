# Release Gate: ga-5nf5lm partial-demand pool create gate

Result: PASS

Date: 2026-06-23

## Candidate

- Bead: `ga-5nf5lm`
- Title: `needs-deploy: fail-closed partial demand pool create gate + retention narrowing`
- Source branch: `builder/ga-4qbgqf.2-partial-demand-create-gate`
- Candidate commit: `60b7d027926919f0cf8675fd7940cd65f3512502`
- Base: `origin/main` at `32ca47acd639b80eee37f4623d0277018b674c06`
- Merge base: `32ca47acd639b80eee37f4623d0277018b674c06`

## Changed Paths

- `cmd/gc/agent_build_params.go`
- `cmd/gc/build_desired_state.go`
- `cmd/gc/build_desired_state_legacy_bound_recovery_test.go`
- `cmd/gc/build_desired_state_test.go`

## Release Criteria Source

`docs/PROJECT_MANIFEST.md` is not present in this repository. This gate applies
the deployer release criteria and the repository testing guidance in
`TESTING.md`.

## Gate Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Deploy bead `ga-5nf5lm` records "Reviewed + PASSED" by `reviewer-gm-wisp-fgacy95` for commit `60b7d027926919f0cf8675fd7940cd65f3512502`. |
| 2 | Acceptance criteria met | PASS | `agentBuildParams.poolScaleCheckPartialTemplates` is package-private and assigned after `evaluatePendingPoolsMap`; `selectOrPlanPoolSessionBead` gates only fresh creates after reuse/resume tiers; the reserved slot is deleted before returning the partial sentinel; sentinel/log text match the accepted wording; `selectOrCreatePoolSessionBead` propagates the sentinel; retainable capacity counts active/awake and fresh `pending_create_claim=true` creates only; stale creating beads can roll back during a partial tick; draining/drained/archived sessions are not preserved by the partial path. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run 'TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained|TestRetainScaleCheckPartialPoolDesiredNormalizesLegacyBoundTemplate' -count=1` PASS; `make test-fast-parallel` PASS; `go vet ./...` PASS. |
| 4 | No high-severity review findings open | PASS | `ga-5nf5lm` has a PASS deploy handoff and no open HIGH findings in its notes. The earlier request-changes on the subset branch is superseded by this reviewed combined branch. |
| 5 | Final branch is clean | PASS | Candidate worktree `deploy-ga-5nf5lm` was clean before adding this gate file; `git config core.hooksPath` reports `.githooks`. This gate file is committed as release evidence before push. |
| 6 | Branch diverges cleanly from main | PASS | `git fetch origin main` completed, then `git merge-tree --write-tree origin/main HEAD` returned tree `c0a73bfc57617fc559e5a030667978c59ede03f6` with exit 0. |
| 7 | Single feature theme | PASS | The branch touches only `cmd/gc` pool session planning and regression tests for fail-closed handling of partial pool demand reads. |

## Acceptance Notes

- `errPoolSessionCreatePartial` message is exactly `pool session create skipped: demand read partial`.
- `realizePoolDesiredSessions` reports the distinct suffix `(partial demand read, fresh create blocked)`.
- The fresh-create gate sits after preferred/resume, canonical non-expanding reuse, and reusable pool-session selection, and before `tryClaimPoolSessionCreate`.
- `scaleCheckPartialSessionRetainable` counts only `active`, `awake`, and active `pending_create_claim=true` creates.
- `discoverSessionBeadsWithRoots` uses `poolPartialAlive` so stale creating/start-pending pool beads are not kept alive by a partial tick.

## Test Evidence

- `git diff --check origin/main...HEAD`: PASS.
- `go test ./cmd/gc -run 'TestBuildDesiredState_ScaleCheckPartialPoolBlocksNewCreates|TestRetainScaleCheckPartialPoolDesired_InFlightCreatingBeadRetained|TestRetainScaleCheckPartialPoolDesiredNormalizesLegacyBoundTemplate' -count=1`: PASS (`cmd/gc` in 0.503s).
- `make test-fast-parallel`: PASS (`All fast jobs passed`).
- `go vet ./...`: PASS.

## Deploy Notes

- `gh auth status` passed for account `quad341`.
- No open PR was found for head branch `builder/ga-4qbgqf.2-partial-demand-create-gate` before creating this deploy PR.
- The standing deployer worktree has an unrelated interrupted rebase on `deploy/ga-q32h2b-test-deflakes-clean`; this gate was evaluated in the clean `deploy-ga-5nf5lm` worktree at the candidate commit.
