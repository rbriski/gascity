# Release gate - emergency relay, SupervisorHTTPCheck, and pack-release Mac symlink

Gate bead: `ga-1h3skj`  
Source build bead: `ga-frmdxd.1`  
Review bead: `ga-abcw35`  
PR: https://github.com/gastownhall/gascity/pull/3302  
Branch: `builder/ga-frmdxd.1`  
Head under gate: `eb9bb60c911fdf09f3a1cf7d2510b7d425c7ea47`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, matching prior
release-gate precedent in this repo. This gate uses the active deployer release
criteria plus `TESTING.md` and the acceptance criteria recorded on
`ga-frmdxd.1`.

## Release Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-abcw35` notes record `Review Verdict: PASS` from `gascity/reviewer` for PR #3302 and cover architecture, security, typed events/wire, and test coverage. |
| 2 | Acceptance criteria met | PASS | `ga-frmdxd.1` required a current-main branch limited to emergency relay, `SupervisorHTTPCheck`, macOS pack-release symlink handling, and generated artifacts. `git diff --name-only origin/main...HEAD` contains only those areas; native-store/autoclose paths are absent; `git merge-base --is-ancestor 630cef370 HEAD` exited 1, confirming the excluded native-store commit is not included. |
| 3 | Tests pass | PASS | Local gate commands passed: `make test-fast-parallel`; `go vet ./...`; `go build ./...`; focused `go test ./internal/emergency ./internal/doctor ./cmd/gc -run 'Test(NewRecord\|WriteSpool\|MarkNotifyDedupe\|SupervisorHTTP\|BuildDoctorChecks\|ResolveLocalPackReleaseSource)'`; `make dashboard-check`; `make spec-ci`; `make dashboard-smoke` exited 0. GitHub PR #3302 also reports required CI green. |
| 4 | No high-severity review findings open | PASS | Review bead `ga-abcw35` lists no open HIGH findings; GitHub PR #3302 has no PR review/comment threads. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean before writing this gate file: `## builder/ga-frmdxd.1...origin/main [ahead 9]`. After this file is committed, the branch contains the reviewed feature commits plus this gate artifact. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded with tree `4daf6ced72fbb54b41336473ba8038f8c1ec140c`; GitHub reports PR #3302 merge state `CLEAN`. |
| 7 | Single feature theme | PASS | The commit set is one deploy lane: emergency relay plumbing, supervisor HTTP doctor check, macOS symlink normalization for `gc pack release`, and generated API/dashboard artifacts required by the emergency event payloads. The prior independent native-store hook/autoclose work was split to `ga-frmdxd.2` and is not present. |

## Acceptance Evidence

`ga-frmdxd.1` acceptance criteria:

| Acceptance criterion | Result | Evidence |
|----------------------|--------|----------|
| Branch is based on current `origin/main`. | PASS | Branch is ahead of `origin/main` by 9 commits and merge-tree against `origin/main` succeeds. |
| Excludes `630cef370` and native-store/autoclose-only changes. | PASS | `git merge-base --is-ancestor 630cef370 HEAD` exited 1. Diff paths do not include `.beads/config.yaml`, `cmd/gc/hooks.go`, `cmd/gc/hooks_test.go`, `cmd/gc/beads_provider_lifecycle_test.go`, `cmd/gc/lifecycle_coordination_test.go`, or `cmd/gc/molecule_autoclose_test.go`. |
| Diff limited to emergency relay, `SupervisorHTTPCheck`, macOS pack-release symlink fix, and required generated artifacts. | PASS | Diff paths are under `internal/emergency`, `internal/doctor`, `cmd/gc` doctor/API state/pack-release files, `internal/api` event/openapi/genclient files, `internal/events`, `docs/schema`, and generated dashboard types. |
| PR description links prior PR/split context and says native-store work was split out. | PASS | PR #3302 already references the replacement/split context from `ga-frmdxd.1`; deployer will update the body with reviewer-facing release notes and link this gate. |
| Required test/build/schema checks pass or failures recorded. | PASS | All required local checks listed in release criterion 3 passed. |
| Builder handed off through normal review/deploy path and did not merge directly. | PASS | `ga-abcw35` is closed PASS by reviewer; `ga-1h3skj` was routed to deployer as `needs-deploy`; PR #3302 remains open. |

## Diff Summary

```text
cmd/gc/api_state.go
cmd/gc/api_state_emergency_relay.go
cmd/gc/cmd_doctor.go
cmd/gc/cmd_doctor_supervisor_http_test.go
cmd/gc/cmd_pack_release.go
cmd/gc/controller.go
cmd/gc/dashboard/web/src/generated/index.ts
cmd/gc/dashboard/web/src/generated/schema.d.ts
cmd/gc/dashboard/web/src/generated/types.gen.ts
cmd/gc/testdata/doctor_check_names.golden
docs/schema/openapi.json
docs/schema/openapi.txt
internal/api/event_payloads.go
internal/api/genclient/client_gen.go
internal/api/openapi.json
internal/doctor/checks_supervisor_http.go
internal/doctor/checks_supervisor_http_test.go
internal/doctor/warmup_eligible.go
internal/emergency/emergency.go
internal/emergency/emergency_test.go
internal/emergency/testenv_import_test.go
internal/events/events.go
```

