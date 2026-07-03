# Release Gate: ga-sbitbl hook-time bd schema-compat guard

Date: 2026-07-03
Deployer: gascity/deployer
Bead: ga-sbitbl
Source review bead: ga-hutkxc
Feature branch: builder/ga-ooka7o-death-boot-bd-fatal-guard

## Decision

FAIL. Do not push or open a PR from the current feature branch.

The deploy handoff and reviewer PASS cover commit `5822cb991`
(`test(cmd/gc): update stale codex hooks fixture for bd schema-compat guard`),
stacked on `e524284e8`. After fetching, the remote feature branch resolves to
`7a3080015`, which is an additional in-progress follow-up commit for
`ga-3w44ma` (`feat(hooks): add bd-schema-compat guard to Antigravity
gascity-prime hook`). `ga-3w44ma` is still in progress and has no review PASS
on its bead, so the current branch head is not the reviewed release candidate.

The branch is also stale relative to current `origin/main`: merge base
`88edd472a20fea791c402520f7ad875ea436f93b`, branch head `7a3080015`, current
`origin/main` `e152df850`. The deployer guardrail says a branch that has
diverged from main is routed back to builder rather than rebased from the
deployer seat.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | FAIL | `ga-hutkxc` has re-review PASS for `5822cb991`; current branch head is `7a3080015` for `ga-3w44ma`, whose bead is still `IN_PROGRESS` and lacks reviewer PASS. |
| 2 | Acceptance criteria met | FAIL | Acceptance was reviewed for the Claude/native Codex SessionStart guard at `5822cb991`. The current branch includes extra Antigravity hook behavior outside this deploy bead's reviewed scope. |
| 3 | Tests pass | FAIL | Full release tests were not run because the release candidate failed the review/scope gate before test execution. Prior reviewer evidence covers `5822cb991`, not current head `7a3080015`. |
| 4 | No high-severity review findings open | PASS | `ga-hutkxc` blocking finding was fixed by `5822cb991` and re-reviewed PASS. No unresolved high-severity finding is recorded for that reviewed candidate. |
| 5 | Final branch is clean | FAIL | Target branch worktree has untracked files and the remote branch head no longer matches the reviewed candidate. |
| 6 | Branch diverges cleanly from main | FAIL | `origin/main` is not contained in the feature branch (`origin/main` currently `e152df850`; branch merge base `88edd472a`). Deployer must not rebase this branch. |
| 7 | Single feature theme | PASS | The stacked commits are all in the bd schema-skew / hook visibility family, but the branch contains an unreviewed in-progress follow-up and cannot ship as this deploy bead. |

## Required Builder Action

Prepare a release branch whose head is exactly the reviewed candidate for
`ga-sbitbl` plus any intended gate commit, or submit the `ga-3w44ma`
Antigravity follow-up through review and deploy it separately. Rebase or
re-cut from current `origin/main` in the builder seat; the deployer does not
resolve that branch freshness here.
