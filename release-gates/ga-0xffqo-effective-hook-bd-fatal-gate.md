# Release Gate: effectiveOnDeath/effectiveOnBoot bd_or_fatal guard

Bead: `ga-0xffqo`
Source bead: `ga-ooka7o`
Review bead: `ga-ggx90t`
Gate branch: `release/ga-0xffqo-effective-hook-bd-fatal`
Reviewed commit: `12dc191ad010cacaf4b9ae76ef751acdf968c06b`
Base checked: `origin/main` at `a2be625b91dd63c3e7cdea8c2a09edeb7717b10e`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria from the role prompt.

## Stack Context

This is a stacked config failure-surfacing branch. The final PR branch includes:

| Commit | Scope | Existing PR |
| --- | --- | --- |
| `5bea1506d` | work-query schema-skew hard failure | `#3870` |
| `43f972ab3` | legacy-ephemeral pool-demand fail-loud guard | `#3871` |
| `ed2c042b6` | release gate for `ga-ac6t6q` | `#3871` |
| `12dc191ad` | lifecycle on_death/on_boot bd_or_fatal guard | this gate |

## Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | `ga-ggx90t` is closed with `Review verdict: PASS`; reviewer found no blocking findings. |
| 2 | Acceptance criteria met | PASS | `effectiveOnDeath` and `effectiveOnBoot` guard bd read/write calls with `bd_or_fatal`; 7 schema-skew tests cover read and update failure paths through real `sh -c`; genuine-empty and happy-path lifecycle tests remain green; `legacyEphemeralPoolDemandShell` quiet=false reachability was already resolved by `ga-ac6t6q` and is in this branch ancestry; no downstream already-guarded jq-only suppressions were broadened. |
| 3 | Tests pass | PASS | `go test ./internal/config/...` PASS; `go test ./internal/config/... -run 'OnDeath|OnBoot'` PASS; `go test ./cmd/gc/... -run 'TestComputePoolDeathHandlers|TestRunPoolOnBoot|TestCityRuntimeTick'` PASS; `go vet ./...` PASS; `go build ./...` PASS; `make lint-changed` PASS; `make test-fast-parallel` first run hit known host-load flake `TestMuxSource_YieldsAndPicksUpNewCity`, isolated reruns of `go test ./internal/eventfeed -run TestMuxSource_YieldsAndPicksUpNewCity -count=1` and `go test ./internal/eventfeed -count=1` PASS, and full `make test-fast-parallel` retry PASS with all fast jobs green. |
| 4 | No high-severity review findings open | PASS | Review notes list `Findings: None blocking`; no HIGH findings are recorded on the deploy or review bead. |
| 5 | Final branch is clean | PASS | Clean status before writing this gate: `git status --short --branch` showed only `## release/ga-0xffqo-effective-hook-bd-fatal`; final status is rechecked after committing this file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree origin/main builder/ga-ooka7o-death-boot-bd-fatal-guard` completed without conflict output; `git diff --check origin/main...HEAD` PASS. |
| 7 | Single feature theme | PASS | The branch is one config failure-surfacing stack for bd schema-skew / bd command failure handling in work-query, legacy-ephemeral demand, and lifecycle recovery hooks. It does not mix unrelated package or user-facing themes. |

## Test Log Summary

- `go test ./internal/config/...` -> PASS (`ok github.com/gastownhall/gascity/internal/config 2.183s`)
- `go test ./internal/config/... -run 'OnDeath|OnBoot'` -> PASS (`ok github.com/gastownhall/gascity/internal/config 0.347s`)
- `go test ./cmd/gc/... -run 'TestComputePoolDeathHandlers|TestRunPoolOnBoot|TestCityRuntimeTick'` -> PASS (`ok github.com/gastownhall/gascity/cmd/gc 8.751s`)
- `go vet ./...` -> PASS
- `go build ./...` -> PASS
- `make lint-changed` -> PASS (`lint-changed: no changed Go files`)
- `make test-fast-parallel` -> first run failed only in `internal/eventfeed` on `TestMuxSource_YieldsAndPicksUpNewCity`; isolated eventfeed reruns passed; second full run passed (`All fast jobs passed`).
