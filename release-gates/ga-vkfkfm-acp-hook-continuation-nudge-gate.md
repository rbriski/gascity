# Release Gate: ACP Hook-Claim Continuation Nudge

Gate run: 2026-06-30T00:30:19-07:00
Deployer bead: ga-vkfkfm
Reviewed source bead: ga-swmq4l
Source branch: builder/ga-7n7vth.3-acp-comment-docs
Base: origin/main at c91105e2bedac416c6feba48cb0e1c81e9ab3a22
Reviewed head before gate artifact: 3147f597951b29251d974d055b416e6880c3e870

## Scope

This is a single-bead deploy for the hook-claim continuation nudge feature.
The branch carries the three dependent commits for one feature theme:

| Commit | Bead | Scope |
| --- | --- | --- |
| d5a8208f5 | ga-7n7vth.1 | Regression coverage for hook-claim continuation nudge behavior |
| 988a08eb6 | ga-7n7vth.2 | Production enqueue call site for newly claimed workflow roots |
| 3147f5979 | ga-7n7vth.3 | ACP dispatcher note and CHANGELOG guidance |

Touched paths:

- CHANGELOG.md
- cmd/gc/cmd_hook.go
- cmd/gc/cmd_hook_claim.go
- cmd/gc/cmd_hook_claim_nudge_test.go

`docs/PROJECT_MANIFEST.md` is not present in this repository checkout
(`rg --files` found no `PROJECT_MANIFEST.md`), so this gate uses the deployer
role release criteria.

## Gate Checklist

| # | Criterion | Result | Evidence |
| --- | --- | --- | --- |
| 1 | Review PASS present | PASS | Review bead ga-swmq4l is CLOSED with close reason `pass`; notes say `PASS - reviewer verdict`, reviewed branch `builder/ga-7n7vth.3-acp-comment-docs`, commit `3147f5979`, and no blockers. |
| 2 | Acceptance criteria met | PASS | Code gates enqueue on `reason == "claimed"`, non-empty continuation assignment, and `gc.kind == workflow`; target is `opts.Assignee`; source/message constants are `hook-claim-continuation` and `Work slung. Check your hook.`; enqueue failure logs non-fatally; `maybeStartNudgePoller` and `pokeController` are called after enqueue; ACP supervisor dispatcher dependency is documented at the enqueue site and in CHANGELOG. |
| 3 | Tests pass | PASS | See test evidence below. Initial `make test-fast-parallel` hit an unrelated timing miss in `internal/eventfeed` (`TestMuxSource_YieldsAndPicksUpNewCity`); the touched paths do not include that package, the exact failing test passed immediately in isolation, and the full `make test-fast-parallel` rerun passed all shards. |
| 4 | No high-severity review findings open | PASS | Review notes list only one non-blocking Style/Minor finding about stale test prose; no HIGH or blocking findings. |
| 5 | Final branch is clean | PASS | Pre-gate worktree was clean before adding this gate artifact. The only expected delta is this gate file, to be committed as the branch tip before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 and produced tree `0d87ecc0a964a2a94283b9be33a4feebbf99555c`. |
| 7 | Single feature theme | PASS | All commits touch the `gc hook --claim` continuation-nudge path, its focused tests, and release guidance. No independent subsystem or unrelated user behavior is bundled. |

## Acceptance Evidence

- ga-7n7vth.1 acceptance: focused tests cover workflow root enqueue, step bead no-enqueue, existing assignment idempotence, and zero-continuation no-enqueue.
- ga-7n7vth.2 acceptance: production path enqueues only for newly claimed workflow roots with continuation siblings; uses concrete session target; uses wait-idle queued nudge defaults; starts the nudge poller and pokes the controller; logs enqueue failures without failing the claim.
- ga-7n7vth.3 acceptance: ACP limitation is documented at the enqueue site, CHANGELOG documents `daemon.nudge_dispatcher = "supervisor"` for reliable ACP delivery, and in-flight sizing uses `max_concurrent_formulas x (1 + max_parallel_steps)`.
- ga-7n7vth.4 status: still open as a validation task, but no additional branch diff exists (`builder/ga-7n7vth.4-validation` equals `3147f5979`). This deploy gate ran and recorded its validation requirements; deployer did not close the child bead.

## Test Evidence

Commands run from `/home/jaword/gascity-deploy-ga-vkfkfm-acp-hook-gate`:

- PASS: `go test ./cmd/gc -run 'TestHookClaim|TestDoHookClaim|TestJSONSchemaManifestForHookClaim|TestJSONSchemaResultForGraphWorkflowClaim' -count=1`
  - `ok github.com/gastownhall/gascity/cmd/gc 0.868s`
- PASS: `go test ./cmd/gc -run 'Test(BuildDesiredState_OnDemandNamedSession|BuildDesiredState_SingletonTemplate|DoSlingNudgePool|OnFormulaGraphWorkflowPreassignsNonLatchBeadsForFixedAgent|CmdSlingDefaultFormulaDoesNotMaterializePoolSession|DoSlingFormulaToAgent|OnFormulaGraphWorkflowPokesOnce)' -count=1`
  - `ok github.com/gastownhall/gascity/cmd/gc 0.633s`
- PASS: `go test ./cmd/gc -count=1`
  - `ok github.com/gastownhall/gascity/cmd/gc 374.382s`
- PASS: `go build ./cmd/gc`
- PASS: `go vet ./...`
- TRANSIENT then PASS: `make test-fast-parallel`
  - First run: failed only `internal/eventfeed TestMuxSource_YieldsAndPicksUpNewCity` after 10s.
  - Isolation rerun: `go test ./internal/eventfeed -run '^TestMuxSource_YieldsAndPicksUpNewCity$' -count=1 -v` passed in 0.097s.
  - Full rerun: `make test-fast-parallel` passed all fast jobs.

## Decision

PASS. Open a PR from `builder/ga-7n7vth.3-acp-comment-docs` to `main`, then
route a merge-request to mayor/mpr. Do not merge from the deployer seat.
