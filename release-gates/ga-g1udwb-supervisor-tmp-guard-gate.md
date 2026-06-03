# Release Gate: ga-g1udwb supervisor temp-binary install guard

Date: 2026-06-03
Branch: release/ga-g1udwb-supervisor-tmp-guard
Base: origin/main @ 141182a79
Head: 9fca6bf0f
Source commit: 37069a852

## Summary

This PR lands the P1 safe-cutover guard from ga-72abmr before the modernc
SQLite cutover proceeds. The guard makes `gc supervisor install` refuse to
install a supervisor unit when the currently running `gc` binary lives under
the system temp directory, preventing deploy smoke-test binaries from
overwriting the production supervisor unit.

## Gate Checklist

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Mayor-owned blocker ga-g1udwb explicitly instructs deployer to open a release PR for commit 37069a852 before modernc cutover. Source bead ga-72abmr is closed with fix committed and tested. |
| 2 | Acceptance criteria met | PASS | `cmd/gc/cmd_supervisor_lifecycle.go` rejects supervisor install from `os.TempDir()` via `supervisorTransientBinaryError`; `cmd/gc/cmd_supervisor_test.go` adds `TestDoSupervisorInstallRejectsTransientBinary`. |
| 3 | Tests pass | PASS | `go test ./cmd/gc -run '^TestDoSupervisorInstallRejectsTransientBinary$' -count=1`; `make test-fast-parallel`; `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | No existing PR or review findings for this standalone guard branch; change is limited to supervisor install path and one regression test. |
| 5 | Final branch is clean | PASS | `git status --short` clean before gate commit. |
| 6 | Branch diverges cleanly from main | PASS | Cherry-picked 37069a852 onto current `origin/main` cleanly as 9fca6bf0f. |
| 7 | Single feature theme | PASS | One subsystem and one behavior: refuse production supervisor install from transient `/tmp` deploy binaries. |

## Test Evidence

```text
$ go test ./cmd/gc -run '^TestDoSupervisorInstallRejectsTransientBinary$' -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	0.378s

$ make test-fast-parallel
All fast jobs passed

$ go vet ./...
PASS
```
