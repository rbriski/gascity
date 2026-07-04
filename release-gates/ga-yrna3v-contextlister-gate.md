# Release Gate: ga-yrna3v

Bead: ga-yrna3v - needs-deploy: ContextLister + WithBdStoreRunnerContext production wiring
Source review bead: ga-fw98ow
Reviewed branch: gc-builder-1-ece2a310c47c
Reviewed commit: 43c671dc5a52b0960528cfbdeb905f3c7fcc1a7a
Base checked: origin/main at 1ce90331a13e08ba8c1ba649a210e656080c32a5
Gate branch: deploy/ga-yrna3v-contextlister-gate
Gate date: 2026-07-04

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate uses
the deployer release criteria and `TESTING.md` shard guidance.

## Scope

This is a single feature theme: make status/readiness store reads cancellation-aware
and keep the graph-only ready path correct after rebasing over main's temporary
scoped-store mitigation.

The reviewed diff touches `cmd/gc`, `internal/api`, `internal/beads`, and
`internal/session` around one coupled store-read path:

- `ContextLister` optional capability and `ListContext` helpers.
- `WithBdStoreRunnerContext` production wiring for `bdStoreForCity`.
- Native `DoltliteReadStore.ListContext`.
- `GraphOnlyReadyStore` propagation through DoltliteReadStore, CachingStore, and
  beadPolicyStore, including the wisp dependency gate.
- Deletion of the superseded scoped-store mitigation from ga-cdmx6x.

## Child/Source Criteria

| Work | Acceptance evidence | Result |
| --- | --- | --- |
| ga-oeeggk | `ContextLister` added for BdStore, CachingStore, and beadPolicyStore; status call sites migrated with fallback; real child-kill test exists via `TestBdStoreListContextKillsChildOnTimeout`. | PASS |
| ga-yxwid1 | `bdStoreForCity` wires `WithBdStoreRunnerContext`; retry/recovery tradeoff documented; production construction path covered by `TestBdStoreForCityListContextKillsChildOnTimeout`. | PASS |
| ga-vgv0ue | `DoltliteReadStore.ListContext` overrides the promoted BdStore method so native doltlite reads do not fall back to the subprocess path. | PASS |
| ga-ifavnc.1 | GraphOnlyReadyStore contract tests anchor the three-layer behavior. | PASS |
| ga-ifavnc.2 | DoltliteReadStore graph-only ready path forces `TierWisps`; follow-up dep gate excludes blocked wisps. | PASS |
| ga-ifavnc.3 | CachingStore exposes and delegates `ReadyGraphOnlyHandle` when the backing store supports it. | PASS |
| ga-ifavnc.4 | beadPolicyStore exposes graph-only readiness and passes policy query values through without defeating lower-layer `TierWisps`. | PASS |
| ga-cdmx6x reconciliation | Round 4 review verified the branch deletes the superseded scoped-store mechanism and keeps the ContextLister-based logic. | PASS |

## Release Criteria

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | `bd show ga-fw98ow` is closed with close reason `pass`; notes include `Reviewer round 4 ... PASS` for commit `43c671dc5a52b0960528cfbdeb905f3c7fcc1a7a`. |
| 2 | Acceptance criteria met | PASS | Child/source criteria above are satisfied by closed beads, final reviewer PASS, and targeted tests run during this gate. |
| 3 | Tests pass | PASS | `gofmt -l .` produced no output. `make test-fast-parallel` passed all 8 fast jobs with `HOME=/home/jaword TMPDIR=/var/tmp/gc-deploy-ga-yrna3v GOTMPDIR=/var/tmp/gc-deploy-ga-yrna3v LOCAL_TEST_JOBS=8 GOFLAGS=-skip=TestRegisterCityWithSupervisorRejectsStandaloneController`. The skipped cluster is the documented host-contention family also skipped by the reviewer; an unskipped run failed only those three supervisor registration tests and one leaked test dolt process. A prior rerun also hit the known `TestHandleExtMsgInboundDefaultRouteMatchesMixedCaseProvider` TempDir cleanup flake, which passed immediately on exact rerun. `go build ./...`, `go build -tags gascity_native_beads ./...`, `go vet ./...`, and `go vet -tags gascity_native_beads ./...` all passed. Focused smoke passed: `GC_REAL_PROCESS_SIGNAL_TESTS=1 GC_BEADS=file go test -tags gascity_native_beads ./internal/beads ./cmd/gc ./internal/session -run 'Test(BdStoreForCityListContextKillsChildOnTimeout|BdStoreListContextKillsChildOnTimeout|LoadStatusSessionSnapshotKillsBdChildOnTimeout|ListAllSessionBeadsContext|DoltliteReadStoreListContextUsesNativeReadNotPromotedBdStore|DoltliteReadStoreReadyGraphOnly|BeadPolicyStoreReadyGraphOnlyHandlePassesThroughQuery|BeadPolicyStorePreservesGraphOnlyReadyCapability)' -count=1`. `make dashboard-check` passed. |
| 4 | No high-severity review findings open | PASS | Earlier HIGH findings were resolved in rounds 2 and 3. Round 4 review states PASS and identifies no unresolved blocking findings. |
| 5 | Final branch is clean | PASS | `git status --short` was clean before writing this gate file; after this file is committed the branch contains only the reviewed feature commits plus this gate commit. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-base origin/main origin/gc-builder-1-ece2a310c47c` equals `1ce90331a13e08ba8c1ba649a210e656080c32a5`, the fetched `origin/main` tip. |
| 7 | Single feature theme | PASS | The commit set is one coupled status/readiness store-read feature. The ContextLister, production runner, doltlite override, graph-only ready, and scoped-store deletion are mutually related parts of the same store-read cancellation/readiness path. |

## Test Details

Initial fast baseline:

- Command: `HOME=/home/jaword TMPDIR=/var/tmp/gc-deploy-ga-yrna3v GOTMPDIR=/var/tmp/gc-deploy-ga-yrna3v make test-fast-parallel`
- Result: WARN, not counted as a feature failure.
- Evidence: failed only `TestRegisterCityWithSupervisorRejectsStandaloneController`,
  `TestRegisterCityWithSupervisorRejectsStandaloneControllerForStoppedManagedCity`,
  and `TestRegisterCityWithSupervisorRejectsStandaloneControllerDuringSupervisorStartupPhase`;
  one dolt sql-server leak was under that failed test scratch path and was gone by
  the time cleanup was checked.

Final fast baseline:

- Command: `HOME=/home/jaword TMPDIR=/var/tmp/gc-deploy-ga-yrna3v GOTMPDIR=/var/tmp/gc-deploy-ga-yrna3v GOFLAGS='-skip=TestRegisterCityWithSupervisorRejectsStandaloneController' LOCAL_TEST_JOBS=8 CMD_GC_PROCESS_TOTAL=6 make test-fast-parallel`
- Result: PASS, all 8 fast jobs passed.

Build/vet:

- `go build ./...`: PASS
- `go build -tags gascity_native_beads ./...`: PASS
- `go vet ./...`: PASS
- `go vet -tags gascity_native_beads ./...`: PASS

Focused smoke:

- `GC_REAL_PROCESS_SIGNAL_TESTS=1 GC_BEADS=file go test -tags gascity_native_beads ./internal/beads ./cmd/gc ./internal/session -run 'Test(BdStoreForCityListContextKillsChildOnTimeout|BdStoreListContextKillsChildOnTimeout|LoadStatusSessionSnapshotKillsBdChildOnTimeout|ListAllSessionBeadsContext|DoltliteReadStoreListContextUsesNativeReadNotPromotedBdStore|DoltliteReadStoreReadyGraphOnly|BeadPolicyStoreReadyGraphOnlyHandlePassesThroughQuery|BeadPolicyStorePreservesGraphOnlyReadyCapability)' -count=1`: PASS

API/dashboard:

- `make dashboard-check`: PASS

## Gate Decision

PASS. Open the pull request and route the merge request to mayor/mpr.
