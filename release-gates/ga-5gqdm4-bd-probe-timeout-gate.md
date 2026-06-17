# Release Gate: GC_BD_PROBE_TIMEOUT bd probe timeout

- Deploy bead: ga-5gqdm4
- Source/review bead: ga-e295ze
- Source branch: builder/ga-x5ocw1
- Base: origin/main @ 70519347fde7944f1aafb0e6792b64a6b24d34a8
- Reviewed candidate head: cb13cf1f9b6aa4bf3dc2d4d77bb43837fabf7ae9
- Scope: cmd/gc pool probe timeout configuration and review-formula test timeout wiring

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-e295ze contains `RE-REVIEW VERDICT: PASS` for commit cb13cf1f9 after the blocker fix. |
| 2 | Acceptance criteria met | PASS | `parseBdProbeTimeout(io.Writer)` reads `GC_BD_PROBE_TIMEOUT`, defaults to 180s, enforces a 5s floor, emits a warning when clamped, and keeps invalid values on the default. `setupReviewFormulaCity` sets `GC_BD_PROBE_TIMEOUT=30s` before creating the isolated supervisor environment. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run '^TestParseBdProbeTimeout' -count=1` PASS; `go test ./cmd/gc -run '^TestEvaluatePoolDefaultScaleCheckCountsRoutedReadyWork$' -count=1` PASS; `make test-fast-parallel` PASS; `go vet ./...` PASS; `make test-integration-review-formulas` PASS (`review-formulas-basic` 205.362s, `review-formulas-retries` 287.682s, `review-formulas-recovery` 63.581s). |
| 4 | No high-severity review findings open | PASS | Reviewer notes list one blocker, resolved by cb13cf1f9, and one low/info item acknowledged as intentional. No unresolved HIGH findings remain. |
| 5 | Final branch is clean | PASS | Gate worktree was clean before writing this artifact; final status is verified after committing the artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base --is-ancestor origin/main builder/ga-x5ocw1` exited 0; `git merge-tree` reported merged paths and no conflicts. |
| 7 | Single feature theme | PASS | Commit set touches one subsystem/theme: configurable pool `bd` probe timeout and the review-formula test environment that consumes it. Diff is limited to `cmd/gc/pool.go`, `cmd/gc/pool_test.go`, and `test/integration/review_formula_test.go`. |

## Notes

`docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses the deployer role release criteria and `TESTING.md` sharded test guidance.
