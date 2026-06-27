# Release Gate: order gate-timeout backoff

- Deploy bead: `ga-5d03kz`
- Source review bead: `ga-rncogf`
- Source issue: <https://github.com/gastownhall/gascity/issues/3688>
- Candidate branch: `fix/ga-qnnz12-order-gate-backoff`
- Reviewed commit: `cb4c26cf9435ee46e43dfe0bf538cd9508a06372`
- Base checked: `origin/main` at `329d3e16c5b2e8c14449f986fdf13187211767d8`
- Merge base: `41c54dcddc241e5f7bdea6f9475efd194bdb6e93`
- Gate date: 2026-06-27

## Manifest

`docs/PROJECT_MANIFEST.md` was not present in this checkout, and `rg --files`
found no `PROJECT_MANIFEST.md` or `SOFTWARE_FACTORY_MANIFEST.md`. This gate
uses the deployer prompt criteria and the repo's `TESTING.md` guidance for
test command selection.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-rncogf` is closed with close reason `pass`; notes contain `REVIEWER VERDICT: PASS`. |
| 2 | Acceptance criteria met | PASS | The fix records `rememberLastRun(scoped, storeKeysForGate, now)` when `gateFailClosed` is caused by `errGateTimeout` at both open-work gate sites. `TestOrderDispatchNonIdempotentBackoffOnGateTimeout` verifies the non-idempotent order is skipped on timeout and is not immediately re-gated on the next tick. Existing idempotent timeout behavior remains covered by `TestOrderDispatchIdempotentFailsOpenOnGateTimeout`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run TestOrderDispatchNonIdempotentBackoffOnGateTimeout -count=1` passed (`ok github.com/gastownhall/gascity/cmd/gc 0.538s`). `go build ./cmd/gc/` passed. `make test-fast-parallel` passed all 8 fast jobs. `go vet ./...` passed. |
| 4 | No high-severity review findings open | PASS | Reviewer notes list no blockers or HIGH findings. One non-blocking test reliability note was filed for the sleep-based goroutine drain in the regression test. Unresolved HIGH count: 0. |
| 5 | Final branch is clean | PASS | Candidate worktree was clean before writing this gate file. The gate commit will contain only this release-gate artifact on top of the reviewed code. |
| 6 | Branch diverges cleanly from main | PASS | After `git fetch origin main`, `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `c057b979f15c8f75eadad41943adeef54246a35d`. The candidate is 44 commits behind `origin/main`; no deployer rebase was performed. |
| 7 | Single feature theme | PASS | Diff scope is one subsystem: `cmd/gc/order_dispatch.go` and `cmd/gc/order_dispatch_gate_policy_test.go`. The change is limited to order-dispatch gate-timeout backoff behavior and its regression coverage. |

## Diff Scope

```text
cmd/gc/order_dispatch.go
cmd/gc/order_dispatch_gate_policy_test.go
```

## Test Commands

```bash
go test ./cmd/gc -run TestOrderDispatchNonIdempotentBackoffOnGateTimeout -count=1
go build ./cmd/gc/
make test-fast-parallel
go vet ./...
```
