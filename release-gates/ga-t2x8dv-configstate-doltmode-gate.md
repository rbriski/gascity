# Release Gate: ConfigState DoltMode Server Propagation

Date: 2026-06-25

Primary deploy bead: `ga-t2x8dv.3`
Duplicate deploy notification covered by this gate: `ga-59hpx3`
Root/source bead: `ga-t2x8dv`
Implementation source bead: `ga-yqn5py.2.1`
Original review bead: `ga-94h3qv`
Clean repackage review bead: `ga-t2x8dv.2`

Branch: `builder/ga-t2x8dv.1-configstate-doltmode-clean`
Reviewed implementation commit: `5bc45410676412e772b8a4e49688b2be254ce9e7`
Base: `origin/main` at `3b1391cf9cfbe249ad6d25617d0c391982ee2890`

`docs/PROJECT_MANIFEST.md` is not present in this checkout, so this gate uses
the deployer release criteria supplied by the active Gas City deployer prompt.

## Change Under Gate

The branch carries Dolt server-mode through the existing `ConfigState` and
canonical beads config paths. It writes or preserves `dolt.mode: server` for
managed and explicit Dolt-backed city/rig scopes so preflight checks and bd/gc
runtime context see server mode consistently.

Touched implementation paths:

- `cmd/gc/beads_provider_lifecycle.go`
- `cmd/gc/beads_provider_lifecycle_test.go`
- `cmd/gc/cmd_rig_endpoint.go`
- `internal/beads/contract/files.go`
- `internal/beads/contract/files_test.go`

## Acceptance Criteria

| Criterion | Result | Evidence |
| --- | --- | --- |
| Clean reviewed branch recorded | PASS | `ga-t2x8dv.2` records branch `builder/ga-t2x8dv.1-configstate-doltmode-clean`, commit `5bc45410676412e772b8a4e49688b2be254ce9e7`, and base `3b1391cf9cfbe249ad6d25617d0c391982ee2890`. |
| Reviewer PASS present | PASS | `ga-t2x8dv.2` is closed with `Reviewer Verdict: PASS`; original review bead `ga-94h3qv` is also closed with `Review Verdict: PASS`. |
| All four ConfigState constructor paths set or propagate server mode | PASS | Review evidence confirms `desiredCityDoltConfigState`, `desiredRigDoltConfigState`, `inheritedRigDoltConfigState`, and `requestedRigEndpointState` cover managed city, external city, explicit rig, inherited rig, and rig endpoint flows. Local code scan confirms `DoltMode: "server"` or inherited propagation at those call sites. |
| Canonical config writes and preserves `dolt.mode` | PASS | `internal/beads/contract/files.go` adds `ConfigState.DoltMode`, writes non-empty mode into canonical config, preserves existing mode when omitted, and excludes only the metadata key `dolt_mode` from cross-backend scrub lists. |
| Focused coverage present | PASS | New/updated tests cover server-mode constructors, inherited empty/server behavior, canonical config write/idempotency/preservation, preflight context, and scrub-key behavior. |
| Standard deploy gate run | PASS | `make test` completed successfully on the final feature branch. Observable log: `/tmp/gascity-test.jsonl.qSpgE3`. |
| Branch clean, mergeable, single theme | PASS | `git status --short --branch` was clean before writing this gate artifact; `git merge-tree --write-tree origin/main HEAD` exited 0; `origin/main...HEAD` contains one implementation commit and touches only the ConfigState/beads config subsystem. |

## Release Gate Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Clean review bead `ga-t2x8dv.2` is closed with `Reviewer Verdict: PASS`. Original review bead `ga-94h3qv` is closed with `Review Verdict: PASS`. |
| 2 | Acceptance criteria met | PASS | All deploy bead acceptance criteria are satisfied: clean repackage evidence exists, review PASS exists, deployer gate ran on `5bc45410676412e772b8a4e49688b2be254ce9e7`, tests passed, merge-tree is clean, and the diff is one ConfigState DoltMode feature theme. |
| 3 | Tests pass | PASS | `make test` returned exit 0 with `observable go test: PASS log=/tmp/gascity-test.jsonl.qSpgE3`. `go vet ./...` returned exit 0. |
| 4 | No high-severity review findings open | PASS | `ga-t2x8dv.2` records PASS for scope hygiene, behavior fidelity, API scope, tests, and security. The earlier `ga-94h3qv` LOW coverage note is resolved by the clean branch's direct tests. No unresolved HIGH findings are recorded. |
| 5 | Final branch is clean | PASS | Before gate artifact creation, `git status --short --branch` returned only `## builder/ga-t2x8dv.1-configstate-doltmode-clean...origin/builder/ga-t2x8dv.1-configstate-doltmode-clean`. This gate file is the only deployer-added file and is committed as the release-gate commit before push/PR. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` returned tree `2a6aa28b286ec1fecb0377701461f993a127acfa` with exit 0. |
| 7 | Single feature theme | PASS | `git log --oneline --no-merges origin/main..HEAD` contains one implementation commit: `5bc454106 feat(beads): propagate DoltMode server-mode through ConfigState (#3702, ga-yqn5py)`. `git diff --name-status origin/main...HEAD` touches only the ConfigState/beads config lifecycle files listed above. |

## Command Evidence

- `git cat-file -e 5bc45410676412e772b8a4e49688b2be254ce9e7^{commit}`: PASS
- `git fetch origin main`: PASS
- `git status --short --branch`: clean before gate artifact creation
- `git merge-tree --write-tree origin/main HEAD`: PASS, tree `2a6aa28b286ec1fecb0377701461f993a127acfa`
- `make test`: PASS, observable log `/tmp/gascity-test.jsonl.qSpgE3`
- `go vet ./...`: PASS

## Decision

PASS. Open a PR from `builder/ga-t2x8dv.1-configstate-doltmode-clean` to
`main`, record the PR URL on `ga-t2x8dv.3` and `ga-59hpx3`, close both deploy
beads, and route the merge-request to `mayor`. The deployer must not merge.
