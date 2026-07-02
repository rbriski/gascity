# Release Gate: poolDemandCountShell legacy-ephemeral fail-loud fix

Date: 2026-07-02
Deployer: gascity/deployer
Deploy bead: `ga-98cp91`
Source implementation bead: `ga-ac6t6q`
Source review bead: `ga-5tg9az`

## Candidate

- PR branch: `builder/ga-ac6t6q-legacy-ephemeral-fail-loud`
- Reviewed source commit: `43f972ab34e055a26fc0b5f4ccd249d88c27056b`
- Base: `origin/main` at `88edd472a20fea791c402520f7ad875ea436f93b`
- Prerequisite commit carried by this branch: `5bea1506d7de141a35c0d7eecf8582302d59e2c3` (`ga-qyw3wn`, currently open separately as PR #3870)
- Reviewer-visible delta for `ga-ac6t6q`:
  - `internal/config/config.go`
  - `internal/config/config_test.go`

## Gate Inputs

- `docs/PROJECT_MANIFEST.md` is not present in this worktree; release criteria were evaluated against the deployer role's explicit seven-point release gate.
- `TESTING.md` was read before selecting the broad local runner; it names `make test-fast-parallel` as the default broad fast-unit baseline.

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-5tg9az` is closed with `REVIEW VERDICT: PASS` for commit `43f972ab34e055a26fc0b5f4ccd249d88c27056b`. The deploy handoff `ga-98cp91` records the same reviewed branch and commit. |
| 2 | Acceptance criteria met | PASS | `ga-ac6t6q` required the `legacy_ephemeral_json` path in `EffectivePoolDemandQuery()` to fail non-zero on any hard `bd query` failure while preserving a genuine empty `[]` result as count `0`. The branch captures the legacy-ephemeral query output only after checking `bd`'s exit code, propagates that failure at the `poolDemandCountShell` call site, and adds regression tests for generic `bd` failure, schema skew, and genuine empty results. |
| 3 | Tests pass | PASS | `PATH="/home/jaword/.local/bin:$PATH" go test ./internal/config/...` passed. `PATH="/home/jaword/.local/bin:$PATH" go build ./...` passed. `PATH="/home/jaword/.local/bin:$PATH" go vet ./...` passed. `PATH="/home/jaword/.local/bin:$PATH" make test-fast-parallel` passed with all fast shards green. |
| 4 | No high-severity review findings open | PASS | Review notes list no HIGH findings or blockers. The only noted issue is LOW/P3 follow-up `ga-cs4a2a`, a stale comment in the untouched quiet branch, explicitly marked non-blocking. |
| 5 | Final branch is clean | PASS | Before writing this checklist, `git status --short --branch` reported only `## builder/ga-ac6t6q-legacy-ephemeral-fail-loud...origin/builder/ga-ac6t6q-legacy-ephemeral-fail-loud`. This checklist is the only deployer change and is committed as the final branch tip before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned success. `git merge-tree --write-tree origin/main HEAD` completed successfully, producing tree `42f4c16010b481c249a64923f36615e278467736` with no conflicts. |
| 7 | Single feature theme | PASS | The `ga-ac6t6q` delta is scoped to one subsystem, `internal/config` pool-demand work-query failure handling. The branch also carries the unmerged prerequisite `ga-qyw3wn`, which is the same work-query failure-surfacing theme and is required for this follow-up's current code shape. No independent feature theme is bundled by the follow-up commit. |

## Validation

- `PATH="/home/jaword/.local/bin:$PATH" go test ./internal/config/...` - PASS.
- `PATH="/home/jaword/.local/bin:$PATH" go build ./...` - PASS.
- `PATH="/home/jaword/.local/bin:$PATH" go vet ./...` - PASS.
- `PATH="/home/jaword/.local/bin:$PATH" make test-fast-parallel` - PASS.

## Deploy Decision

PASS. Commit this gate checklist to `builder/ga-ac6t6q-legacy-ephemeral-fail-loud`, push the branch, open a PR, record the PR URL on `ga-98cp91`, close the deploy bead, and route a merge-request to mayor. Deployer does not merge.
