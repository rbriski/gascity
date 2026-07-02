# Release Gate: bd schema-skew work-query hard failure

Date: 2026-07-02
Deployer: gascity/deployer
Deploy bead: `ga-nyjnlo`
Source implementation bead: `ga-qyw3wn`
Source review bead: `ga-th3zom`

## Candidate

- PR branch: `release/ga-qyw3wn-bd-schema-skew-fix`
- Reviewed source branch: `builder/ga-qyw3wn-bd-schema-skew-fix`
- Reviewed source commit: `5bea1506d7de141a35c0d7eecf8582302d59e2c3`
- Base: `origin/main` at `88edd472a20fea791c402520f7ad875ea436f93b`
- Reviewer-visible diff:
  - `internal/config/config.go`
  - `internal/config/config_test.go`
  - `cmd/gc/doctor_bd_schema_skew.go`
  - `cmd/gc/doctor_bd_schema_skew_test.go`
  - `cmd/gc/cmd_doctor.go`
  - `cmd/gc/testdata/doctor_check_names.golden`
  - `cmd/gc/graph_dispatch_mem_test.go`

## Gate Results

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-th3zom` is closed with `REVIEW VERDICT: PASS` for commit `5bea1506d7de141a35c0d7eecf8582302d59e2c3`. The deploy handoff `ga-nyjnlo` records the same reviewed commit and branch. |
| 2 | Acceptance criteria met | PASS | `ga-qyw3wn` required schema-skewed `bd` failures to surface as work-query errors while genuine empty results still return `[]`; the branch adds the shared `bd_or_fatal` guard and tests covering the assigned, routed-pool, dispatcher, ephemeral, migration, and legacy-ephemeral query paths. It also required a `gc doctor` warning naming the stale `bd` path; the branch adds `bd-schema-skew`, registers it, updates the doctor check-name golden, and tests warning/OK/not-on-PATH/alternate-signature cases. |
| 3 | Tests pass | PASS | `go build ./...` passed. `go vet ./...` passed. `make test-fast-parallel` passed on a full rerun after an initial unrelated timing failure in `internal/beads TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand`; the exact failing test passed with `go test ./internal/beads -run '^TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand$' -count=1 -v` before the clean full rerun. |
| 4 | No high-severity review findings open | PASS | Review notes list no blockers and no HIGH findings. The only filed follow-up is `ga-9ex12k`, a LOW/P3 latent `poolDemandCountShell` scale-check gap that the reviewer explicitly marked non-blocking and outside `ga-qyw3wn` scope. |
| 5 | Final branch is clean | PASS | After committing this checklist, `git status --short --branch` reported only `## release/ga-qyw3wn-bd-schema-skew-fix...origin/builder/ga-qyw3wn-bd-schema-skew-fix [ahead 1]`, with no file changes. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` returned success at the reviewed commit. The candidate branch is a direct descendant of `origin/main` plus the reviewed fix and this gate checklist. |
| 7 | Single feature theme | PASS | The commit set is one feature: make bd schema-skew visible instead of silently idling agents. The `internal/config` changes surface the failure in work queries; the `gc doctor` check gives operators visibility into the same failure mode. The one-line `graph_dispatch_mem_test.go` change is a behavior-identical staticcheck cleanup in a touched test package, not a separate feature. |

## Validation

- `PATH="/home/jaword/.local/bin:$PATH" make test-fast-parallel` - PASS on rerun.
- `PATH="/home/jaword/.local/bin:$PATH" go test ./internal/beads -run '^TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand$' -count=1 -v` - PASS after the first full run's unrelated timing failure.
- `PATH="/home/jaword/.local/bin:$PATH" go vet ./...` - PASS.
- `PATH="/home/jaword/.local/bin:$PATH" go build ./...` - PASS.

## Non-Blocking Environment Notes

- The first `make test-fast-parallel` run failed only in `internal/beads TestExecCommandRunnerStopsBDSlowTimerForFastBDCommand`; the isolated rerun passed and the full baseline rerun passed.
- A later `git push --dry-run origin HEAD` attempted to run the pre-push fast suite with `/tmp` as `TMPDIR` and failed from `no space left on device` linker and fixture writes. That failure occurred after the clean full baseline and is treated as host tmpfs pressure, not branch behavior. Subsequent push checks should use a home-backed `TMPDIR`.

## Deploy Decision

PASS. Commit this gate checklist to `release/ga-qyw3wn-bd-schema-skew-fix`, push the branch, open a PR, record the PR URL on `ga-nyjnlo`, close the deploy bead, and route a merge-request to mayor. Deployer does not merge.
