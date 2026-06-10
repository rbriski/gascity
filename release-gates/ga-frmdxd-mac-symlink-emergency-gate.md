# Release Gate: ga-frmdxd mac-symlink + emergency relay

Date: 2026-06-10
PR: https://github.com/gastownhall/gascity/pull/3301
Deploy bead: ga-frmdxd
Source review bead: ga-gllg5b
Reviewed head: d74e1b96a550949b4aa16d3e05c345fc2b962550
Base checked: origin/main at c5ce770923a04fdbc552e5102d56e4b42140bb31

## Gate Result

FAIL

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | ga-gllg5b is closed with reason `pass`; notes contain `Verdict: PASS` and confirm all three previous blockers resolved. Reviewer mail gm-wisp-wt2gu05 routed ga-frmdxd to deploy. |
| 2 | Acceptance criteria met | PASS | Emergency relay/API wiring present under `internal/emergency`, `cmd/gc/api_state_emergency_relay.go`, and generated OpenAPI/clients. `EmergencySignaled` and `EmergencyAcked` are registered typed events. Spool and dedupe writes use 0o700 directories and 0o600 files. `MarkNotifyDedupe` rejects `/` and `\\` in keys before path joins. `SupervisorHTTPCheck` probes 127.0.0.1 only. `resolveLocalPackReleaseSource` canonicalizes symlinks with `filepath.EvalSymlinks` before repo-relative path normalization. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all fast shards. `go vet ./...` passed. `go build ./...` passed. `make dashboard-check` passed. `make dashboard-smoke` passed. `make spec-ci` passed. Focused checks passed: `go test ./cmd/gc -run 'TestPackReleaseValidateRejectsHashMismatch|TestPackReleaseValidateSkipsWithdrawnByDefault'`, `go test ./internal/doctor ./internal/emergency` (`internal/emergency` has no package-local test files; follow-up ga-guopsu tracks that coverage gap). |
| 4 | No high-severity review findings open | PASS | Current re-review bead ga-gllg5b records all prior blockers resolved. Remaining observations are explicitly non-blocking; coverage follow-up ga-guopsu is open as `needs-tests`, not a release blocker. |
| 5 | Final branch is clean | PASS | Gate worktree was clean after test/generator commands before adding this gate file. |
| 6 | Branch diverges cleanly from main | FAIL | After refetching current `origin/main`, `git merge-tree --write-tree --messages origin/main HEAD` reports conflicts in `cmd/gc/dashboard/web/src/generated/index.ts`, `cmd/gc/dashboard/web/src/generated/schema.d.ts`, and `cmd/gc/dashboard/web/src/generated/types.gen.ts`. GitHub reports PR #3301 as `mergeable_state=dirty`. |
| 7 | Single feature theme | PASS | The commit set is one release theme: emergency relay and supervisor health plumbing plus the Mac pack-release symlink fix required for this PR's CI/release readiness. The touched files stay within API/event/dashboard generation, emergency/doctor infrastructure, and pack-release path normalization. |

## Local Commands

```text
make test-fast-parallel
go vet ./...
go build ./...
make dashboard-check
make dashboard-smoke
make spec-ci
go test ./cmd/gc -run 'TestPackReleaseValidateRejectsHashMismatch|TestPackReleaseValidateSkipsWithdrawnByDefault'
go test ./internal/doctor ./internal/emergency
git merge-tree --write-tree --messages HEAD origin/main
```

## Notes

`docs/PROJECT_MANIFEST.md` is not present in this checkout. The release gate used the deployer role's release-gate criteria and the repository's `TESTING.md`/Makefile guidance.

The first local merge-tree check was clean against `origin/main` at
3f77a844faacd646db2ca9f70c835759e35b96c0. `main` advanced during the deploy
run; the current base c5ce770923a04fdbc552e5102d56e4b42140bb31 conflicts in
generated dashboard API types. This requires builder rebase/regeneration.
