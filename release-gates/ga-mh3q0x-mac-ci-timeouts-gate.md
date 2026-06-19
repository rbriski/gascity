# Release Gate: Mac Nightly CI Timeouts

Bead: `ga-mh3q0x`

Source review bead: `ga-et8utx`

Branch under review: `builder/ga-ftgzxd`

Local deploy branch: `deploy/ga-mh3q0x-mac-ci-timeouts`

Candidate head: `1277da70273043c7b018c698fb8ead7802552682`

Current base checked: `origin/main` at `81579e6118e06506d1333e69366c0bc465bb3247`

Merge base: `0a33ae3980026b0e50e48a90d7fb4f5d2102ca46`

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-et8utx` is closed with close reason `pass`; notes contain `REVIEW PASS (gascity/reviewer, 2026-06-19)`. `ga-mh3q0x` was created by `gascity--reviewer` with reviewed/PASSED handoff. |
| 2 | Acceptance criteria met | PASS | `Makefile:67` includes `test-mac` in `.PHONY`. `Makefile:312-317` defines `test-mac` as the fast unit sweep with `cmd/gc` excluded by exact package path. `.github/workflows/mac-regression.yml:172` raises `mac-unit` to 30 minutes, line 185 runs `make test-mac`, and line 242 raises `mac-cover` to 60 minutes. |
| 3 | Tests pass | PASS | `make build` passed. `make test-mac` passed with `observable go test: PASS log=/tmp/gascity-test.jsonl.lns1Co`. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Source review notes list findings as clear; no HIGH findings were recorded on `ga-et8utx` or `ga-mh3q0x`. |
| 5 | Final branch is clean | PASS | Before writing this gate artifact, `git status --short --branch` showed a clean branch at `1277da702`. This artifact is the only deployer-authored file to be committed. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `d0629086d7822868ac48a51c7feee5efde6e15a6`; no merge conflicts. `git diff --check origin/main...HEAD` exited 0. |
| 7 | Single feature theme | PASS | The effective merge diff against current `origin/main` is only `.github/workflows/mac-regression.yml` and `Makefile`, both for Mac CI timeout headroom and the new `make test-mac` target. |

## Scope Notes

The branch contains two commits ahead of its merge base:

- `40de01b56` raises the review-formulas shard timeout.
- `1277da702` adds the Mac CI timeout and `test-mac` changes.

Current `origin/main` already contains the review-formulas timeout via
`81579e611`, so the final merged tree differs from `origin/main` only in:

- `.github/workflows/mac-regression.yml`
- `Makefile`

This was verified with:

```bash
MERGE_TREE=$(git merge-tree --write-tree HEAD origin/main)
git diff --stat origin/main "$MERGE_TREE"
git diff --name-only origin/main "$MERGE_TREE"
```

Result:

```text
.github/workflows/mac-regression.yml | 6 +++---
Makefile                             | 9 ++++++++-
2 files changed, 11 insertions(+), 4 deletions(-)
```

## Commands Run

```bash
gh auth status
git fetch origin main
git fetch fork builder/ga-ftgzxd
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
make build
make test-mac
go vet ./...
```

## Decision

Gate PASS. Open a PR from `quad341:builder/ga-ftgzxd` to `gastownhall/gascity:main`, then route the merge-request to mayor/mpr. Do not merge from the deployer session.
