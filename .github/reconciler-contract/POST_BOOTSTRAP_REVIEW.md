# Reconciler pre-G0 bootstrap — exact-object review

| Field | Value |
|---|---|
| Date | 2026-07-14 |
| Reviewer | Independent exact-object reviewer (`ga-f7v2ft.12`) |
| Candidate digest | `351f8a2f24a29087e4f6286af177569421eea10cb54e102c8768e711df281c26` |
| Bootstrap commit | `614a8ebd62cf230de226213f302ed3002dddea61` |
| Bootstrap tree | `2c1c9d4a2aeaf8188793a26554ec7ea5f9b7cc48` |
| Ordered parent 1 — execution base | `d36a8ccadf63c9c782b799e2a02ffbfce12c7dd4` |
| Ordered parent 2 — reviewed source | `8f46e6ed3930f32e2ec59ac0a70b7328558bfa41` |
| Merge base | `4e90d8378949e3bf73b4e67f0f61e5130a87b91e` |
| Verdict | **APPROVE** |

The reviewer inspected the exact Git objects with replacement objects disabled
and independently confirmed that the bootstrap is one two-parent, append-only
merge. Parent order is material: the pinned `d36a8cca` execution base is first
and the approved candidate source is second. The mutable `origin/main` ref is
not an input. At review time, the source ref
`origin/feature/reconciler-keyed-preg0-final-20260713` resolved to the exact
candidate source and `origin/feature/reconciler-g0-bootstrap-d36a8cca` resolved
to the exact bootstrap.

A clean temporary remerge from the exact merge base reproduced the four
textual conflicts below. Applying the recorded dispositions and the
clean-merge correction produced tree
`2c1c9d4a2aeaf8188793a26554ec7ea5f9b7cc48`, exactly matching the reviewed
bootstrap tree.

## Conflict dispositions

| Path | Reviewed result blob | Disposition |
|---|---|---|
| `TESTING.md` | `62ac13ec9fc3897b01269ee377a7c84fe3e65f3a` | Preserve the execution base's version-2 Medium/HTTP resource-ledger model and regenerate its checked Markdown block from the final combined tree. |
| `internal/bdflags/bdflags.go` | `a5b3e4fb4701627ffaa65711df26e0161dedbe8d` | Retain the reviewed-source implementation. Both parents recognize `bd create -s/--status`; this blob is the source's canonical v1.1.0 form and remains paired with its stronger test. |
| `internal/testpolicy/resourcecensus/census.go` | `cf2ad511ce76cc0630edff2ec0b9f46e952db487` | Preserve the execution base's version-2 exact-Medium and HTTP-test-server scanner, then set only census baselines to observations from the final combined tree. |
| `test/test-resources.toml` | `5450456ff422c2b0374245dd89d71b2482a4ecc0` | Preserve the version-2 ledger schema and ownership rows, with baselines regenerated from the final combined tree in lockstep with the Go policy and generated documentation. |

There was also a clean textual-merge trap outside Git's conflict list. Parent 1
blob `03e41c3ad1f38e012078f837d9d786edfcb64f05` and parent 2 blob
`a7d6303916ac4a445839de2834f88addcd371e42` independently add
`TestCreateStatusFlagsConsumeValues` at different locations. Accepting the
automatic merge would compile two functions with the same name. The bootstrap
correctly retains the stronger reviewed-source test as blob
`a7d6303916ac4a445839de2834f88addcd371e42`, which checks both value-flag
presence and boolean-flag absence.

The final combined-tree census was regenerated twice with identical results:

| Ledger | Resource | Calls / files |
|---|---|---|
| Audit | subprocess | 492 / 137 |
| Audit | fixed sleep | 438 / 153 |
| Source debt | subprocess | 375 / 98 |
| Source debt | fixed sleep | 286 / 110 |
| Source debt | environment | 4173 / 185 |
| Source debt | CWD | 210 / 40 |
| Source debt | slow process gate | 77 / 26 |
| Source debt | HTTP test server | 256 / 56 |
| Small debt | subprocess | 375 / 98 |
| Small debt | fixed sleep | 286 / 110 |
| Small debt | environment | 4167 / 185 |
| Small debt | CWD | 210 / 40 |
| Small debt | slow process gate | 77 / 26 |
| Small debt | HTTP test server | 256 / 56 |

## Immutable v1 evidence

The four protected v1 artifacts are byte-identical between reviewed source and
bootstrap:

| Path | Blob |
|---|---|
| `.github/CODEOWNERS` | `675e7f1cff3ee31e70f7e31d8d5363a85e88d781` |
| `engdocs/plans/reconciler-redesign/PRE_G0_CANDIDATE_MANIFEST.json` | `87e2f7acd36bdca96fcfecd090257409d8e31ea5` |
| `engdocs/plans/reconciler-redesign/POST_INTEGRATION_REVIEW.md` | `295d7112e248c68ba0dbedb55352d5bc345b3f2c` |
| `test/docsync/reconciler_candidate_manifest_test.go` | `07496a454de90bf1f1cc6727f9ba754ced644ec8` |

## Verification evidence

The reviewed bootstrap passed the candidate-manifest verifier; two independent
resource-census regenerations; `internal/bdflags` unit tests and the real-`bd`
integration test; focused `internal/beads`, `internal/convergence`, and
`cmd/gc` tests; the pre-commit and documentation-sync checks; `go vet ./...`;
`make test-fast-parallel`; all six `make test-cmd-gc-process-parallel` shards;
and the push-time fast suite. No verification or cleanup touched the default
tmux server.

This approval is limited to the exact bootstrap object and its additive
execution binding. It authorizes only append-only bootstrap evidence and
default-inert Phase-0 work. It does not ratify operational G0 and does not
authorize a production owner cutover, provider effects, runtime action
concurrency, or schema mutation.
