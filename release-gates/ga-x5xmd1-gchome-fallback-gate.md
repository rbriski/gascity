# Release Gate: ga-x5xmd1 GCHome fallback isolation

Gate run: 2026-06-22

## Scope

- Deploy bead: `ga-x5xmd1`
- Review bead: `ga-mxx6wy`
- Source commit reviewed: `48ac0ae9e`
- Final PR branch: `release/ga-x5xmd1-gchome-fallback`
- Final branch base: `origin/main`
- Applied commit on final branch: `57a935f24`

The reviewed commit was originally on `builder/ga-mjkqhb-extmsg-subscribe-clean`.
That source branch also contained unrelated extmsg/dashboard commits. The release
branch was cut cleanly from `origin/main` and contains only the reviewed GCHome
fallback patch plus this gate file.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-mxx6wy` is closed with review verdict PASS for commit `48ac0ae9e`. |
| 2 | Acceptance criteria met | PASS | `ga-hw9hfd`, `ga-6vrlw0`, and `ga-c7s52e` are closed. The final diff updates `defaultGCHomeFallback`, `implicitGCHomeFallback`, and `builtinDefaultHomeFallback` to use process-unique `gc-home-*` temp dirs with PID fallback, and adds the three requested tests. |
| 3 | Tests pass | PASS | `go test ./internal/bootstrap ./internal/config ./internal/supervisor`; `go vet ./internal/bootstrap ./internal/config ./internal/supervisor`; `make test-fast-parallel`; `go vet ./...`. |
| 4 | No high-severity review findings open | PASS | Review notes list only LOW informational findings: duplicate small helper bodies and temp-dir cleanup in fallback path. No HIGH findings. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## release/ga-x5xmd1-gchome-fallback...origin/main [ahead 1]`. The gate file is committed as the final branch tip. |
| 6 | Branch diverges cleanly from main | PASS | Branch was created from `origin/main`; `git cherry-pick 48ac0ae9e` completed without conflicts; `git merge-tree origin/main HEAD` reported no conflict records. |
| 7 | Single feature theme | PASS | Final branch touches only GCHome fallback path selection and adjacent tests in `internal/bootstrap`, `internal/config`, and `internal/supervisor`. The unrelated extmsg stack from the source branch is not included. |

## Test Output Summary

```text
go test ./internal/bootstrap ./internal/config ./internal/supervisor
ok  	github.com/gastownhall/gascity/internal/bootstrap	0.062s
ok  	github.com/gastownhall/gascity/internal/config	1.761s
ok  	github.com/gastownhall/gascity/internal/supervisor	0.468s

go vet ./internal/bootstrap ./internal/config ./internal/supervisor
PASS

make test-fast-parallel
[unit-cmd-gc-3-of-6] ok
[unit-cmd-gc-4-of-6] ok
[unit-cmd-gc-2-of-6] ok
[unit-cmd-gc-6-of-6] ok
[unit-cmd-gc-1-of-6] ok
[unit-cmd-gc-5-of-6] ok
[unit-core] ok
All fast jobs passed

go vet ./...
PASS
```
