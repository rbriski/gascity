# Release Gate: ga-j40p4l loop.count friendly error UX

Date: 2026-06-24

Branch: `release/ga-j40p4l-loop-count-friendly-error`
Head before gate commit: `0a2b1e7edac3c1c9bf8d716652965d5d10266bfb`
Base: `origin/main`
Deploy bead: `ga-j40p4l`
Source review bead: `ga-acfcqc`

Note: this repo does not contain `docs/PROJECT_MANIFEST.md`; no Gas City
project manifest was found with `rg --files -g PROJECT_MANIFEST.md` or `find`.
This gate uses the deployer prompt release criteria and the repo gates in
`TESTING.md` plus `gascity-docs` verification rules for the tutorial change.

## Summary

PASS. The branch is a single feature theme: make string-valued
`loop.count` fail with an actionable formula compile error and document the
integer-literal requirement in the formulas tutorial.

## Criteria

| # | Criterion | Result | Evidence |
|---|---|---|---|
| 1 | Review PASS present | PASS | `ga-acfcqc` is closed with reason `pass`; notes contain `Review Verdict: PASS` from `reviewer-gm-wisp-22n14q2` dated 2026-06-24. |
| 2 | Acceptance criteria met | PASS | `internal/formula/types.go` rejects string-valued `loop.count` before JSON decoding; `internal/formula/compile_test.go` adds `TestCompile_LoopCountStringParseError`; `docs/tutorials/05-formulas.md` notes integer-only `count` and points variable-driven counts to `range = "1..{n}"` with `var = "n"`. |
| 3 | Tests pass | PASS | Focused test, formula package, full fast baseline, vet, and docs sync all passed. Details below. |
| 4 | No high-severity review findings open | PASS | Review notes list one LOW style/wording finding; no HIGH findings. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file; the gate file is committed as the final branch tip. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main HEAD` exited 0; `git merge-tree origin/main HEAD` returned a tree id with no conflict output. |
| 7 | Single feature theme | PASS | Diff is limited to `internal/formula` parser behavior/tests and the matching formulas tutorial note. |

## Test Evidence

- PASS: `git diff --check origin/main..HEAD`
- PASS: `go test ./internal/formula/ -run TestCompile_LoopCountStringParseError -count=1`
  - `ok github.com/gastownhall/gascity/internal/formula 0.003s`
- PASS: `go test ./internal/formula/...`
  - `ok github.com/gastownhall/gascity/internal/formula 0.263s`
- PASS: `go vet ./internal/formula/...`
- PASS: `make check-docs`
  - `ok github.com/gastownhall/gascity/test/docsync (cached)`
- PASS: `go vet ./...`
- PASS: `make test-fast-parallel`
  - `All fast jobs passed`

## Commit Set

- `50bc876a4` - `test(formula): regression coverage for loop.count string parse errors (ga-sdv68f.1)`
- `2703d79d1` - `fix(formula): friendly error when loop.count is a string or template value (ga-sdv68f.3)`
- `0a2b1e7ed` - `docs(formula): note that loop.count is integer-only; point to range for variable counts (ga-sdv68f.2)`

## Changed Paths

- `internal/formula/types.go`
- `internal/formula/compile_test.go`
- `docs/tutorials/05-formulas.md`

