# Release Gate: resource-census subprocess baseline

Bead: `ga-2i5e2p` - Deploy: Bump resource-census subprocess baseline +1/+1
Source review bead: `ga-5rilt6`
Candidate branch: `builder/ga-5rilt6-resource-census-subprocess-baseline`
Gate staging branch: `deploy/ga-2i5e2p-resource-census-baseline`
Reviewed commit: `dc1a7fb6e7c1b907fe1b4d91e76f02ca5741602b`
Base checked: `origin/main` at `df0f81f56f974dadd9236cb0668ef7dc8cd8bd25`
Merge base: `c2f35f5f7`
Gate date: 2026-07-14
Verdict: PASS

## Summary

This release updates the checked test resource ledger for the subprocess call
introduced by the API URN guard's `git ls-files` scan. The change is limited to
the generated resource-census mirrors in `TESTING.md`,
`internal/testpolicy/resourcecensus/census.go`, and
`test/test-resources.toml`.

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria plus the sharded-runner guidance in `TESTING.md`.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-5rilt6` is closed and its notes contain `REVIEW: PASS` for commit `dc1a7fb6e7c1b907fe1b4d91e76f02ca5741602b`. |
| 2 | Acceptance criteria met | PASS | The three generated ledger mirrors agree on subprocess totals: all tracked test source `492 calls / 137 files`, source-debt untagged `375 / 98`, and small-debt untagged `375 / 98` with the historical-regex parenthetical preserved where required. `TestRepositoryLedgerMatchesCensusAndDocumentation` passes on the candidate branch and on a synthetic merge commit against current `origin/main`. |
| 3 | Tests pass | PASS | Branch-scoped checks passed: `go test ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1 -v`, `go test ./internal/testpolicy/... -count=1`, `go build ./cmd/gc`, `go vet ./...`, and `git diff --check origin/main...HEAD`. `make test-fast-parallel` failed in `unit-cmd-gc-5-of-6` at `TestSupervisorCreatesControllerSocketForManagedCity`; the same focused test fails on current `origin/main` with the same missing `controller.sock` symptom, so this is not introduced by the release branch. |
| 4 | No high-severity review findings open | PASS | Review notes record no actionable defects. The only finding is non-blocking commit-message attribution: the true source of the census growth is #4118 (`c2f35f5f7`), not #4240. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed a clean staging branch at the reviewed commit. After this gate file is committed, the branch should be clean again. |
| 6 | Branch diverges cleanly from main | PASS | Checked first. `git merge-tree --write-tree origin/main HEAD` succeeded against current `origin/main`, producing tree `8e91ddf2732638b2f6fb6cfa6d55c4863d096955`. No bounded self-rebase was needed. |
| 7 | Single feature theme | PASS | The candidate changes exactly one subsystem: test resource-census ledger data and its generated documentation mirror. |

## Acceptance Mapping

| Acceptance item | Result | Evidence |
|---|---|---|
| Resource-census ledger records the new subprocess call/file totals | PASS | `internal/testpolicy/resourcecensus/census.go` records `ResourceSubprocess` all-source `492/137`, source-debt untagged `375/98`, and small-debt untagged `375/98`. |
| TOML mirror matches the Go ledger | PASS | `test/test-resources.toml` contains the matching subprocess rows. |
| Documentation mirror matches generated ledger output | PASS | `TESTING.md` contains the same subprocess rows and adds the required historical-regex parenthetical on the small-debt row. |
| Current main integration remains valid | PASS | Synthetic merge commit `50a132991c7570247a25ae70fa782d1c5b21d46c` was created from merge tree `8e91ddf2732638b2f6fb6cfa6d55c4863d096955`; `TestRepositoryLedgerMatchesCensusAndDocumentation` passed there. |

## Commands Run

```text
gc prime
bd prime
gc hook --claim --json
bd show ga-2i5e2p
bd show ga-5rilt6
gh auth status
git fetch origin main builder/ga-5rilt6-resource-census-subprocess-baseline
gh pr list --state all --head builder/ga-5rilt6-resource-census-subprocess-baseline --json number,url,state,author,title,headRefName,headRepositoryOwner,updatedAt
git rev-list --left-right --count origin/main...HEAD
git merge-tree --write-tree origin/main HEAD
git diff --name-status origin/main...HEAD
git diff --stat origin/main...HEAD
git diff --check origin/main...HEAD
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp make test-fast-parallel
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp go test ./cmd/gc -run '^TestSupervisorCreatesControllerSocketForManagedCity$' -count=1 -v
TMPDIR=/var/tmp/gascity-ga-2i5e2p-main-tmp go test ./cmd/gc -run '^TestSupervisorCreatesControllerSocketForManagedCity$' -count=1 -v
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp go test ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1 -v
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp go test ./internal/testpolicy/... -count=1
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp go build ./cmd/gc
TMPDIR=/var/tmp/gascity-ga-2i5e2p-tmp go vet ./...
git commit-tree <merge-tree> -p origin/main -p HEAD
TMPDIR=/var/tmp/gascity-ga-2i5e2p-merge-tmp go test ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1 -v
git config core.hooksPath
```

## Test Results

```text
go test ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1 -v
PASS
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	1.369s

go test ./internal/testpolicy/... -count=1
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	1.375s
ok  	github.com/gastownhall/gascity/internal/testpolicy/timingsummary	0.008s

go build ./cmd/gc
PASS

go vet ./...
PASS

synthetic merge: go test ./internal/testpolicy/resourcecensus -run '^TestRepositoryLedgerMatchesCensusAndDocumentation$' -count=1 -v
PASS
ok  	github.com/gastownhall/gascity/internal/testpolicy/resourcecensus	1.309s

make test-fast-parallel
FAIL: unit-cmd-gc-5-of-6
  TestSupervisorCreatesControllerSocketForManagedCity

focused rerun on candidate:
FAIL: TestSupervisorCreatesControllerSocketForManagedCity
  controller.sock not created after reconcileCities

focused rerun on current origin/main:
FAIL: TestSupervisorCreatesControllerSocketForManagedCity
  controller.sock not created after reconcileCities
```

## Diff Scope

```text
TESTING.md
internal/testpolicy/resourcecensus/census.go
test/test-resources.toml
```

## Notes

The PR description should credit the root cause as #4118 (`c2f35f5f7`, the
API URN guard `git ls-files` scan), not #4240. The reviewed commit's numeric
ledger update is correct; only the commit-message attribution is stale.
