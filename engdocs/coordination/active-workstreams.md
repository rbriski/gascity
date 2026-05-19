# Active Workstream Coordination

Last updated: 2026-05-18 17:40 PT by Mabel

This is a temporary cross-agent coordination channel, not product documentation.
Do not merge this file into public docs unless we explicitly promote it.

Use this file for concise handoffs between active agents. Prefer factual state,
links, branch names, and explicit interface constraints over narrative.

Severity labels:

- `red`: blocks another workstream.
- `yellow`: coordinate before touching the affected area.
- `green`: informational.

## Attention Protocol

Every workstream handoff should include an attention block so agents can poll
this file without D. Box becoming the notification bus.

Use this shape:

```markdown
### Attention Needed

Needs Mabel: yes/no

Needs D. Box: yes/no

Urgency: red/yellow/green

Reason: short factual reason, or "none".
```

If a workstream is blocked on another agent, mark the urgency and name the
needed owner in `Reason`.

## Current Attention Summary

- `yellow`: JSON rollup is partially assembled and pushed, but not yet
  machine-move ready. Jasmine still needs to finish the first-rollup boundary,
  run full validation, and publish the final abandon/close list for old JSON
  PRs/branches.
- `yellow`: Registry-gc-pack needs Mabel to flag any #2126 constraints that
  affect `gc import`, legacy `gc pack fetch/list`, or PackV2 import fields.
- `green`: gc4gc / Operational Substrate is portable through
  `https://github.com/donbox/gc4gc`; stable and producer/dev branches are
  published separately.
- `green`: Cleo's dirty registry work has been pushed to a preservation /
  workstream branch; no meaningful registry local-only state is expected.
- `yellow`: before retiring the old machine, Jasmine and Cleo should each
  publish one final "ready for machine move" checkpoint after any current local
  edits are pushed or explicitly marked disposable.
- `yellow`: Mabel's coordination state is portable through this branch, but the
  new machine should bootstrap from this file before resuming pack work.

## Machine Move Readiness

### Current State

This file is the canonical handoff for moving active Gas City pack/package work
to a new machine.

Mabel / coordination state:

- Source of truth: `gastownhall/gascity:codex/workstream-coordination`.
- Coordination file: `engdocs/coordination/active-workstreams.md`.
- Current branch is not a product PR and should not be merged unless explicitly
  promoted.
- Mabel can resume from this file plus live GitHub PR state.

Known portable workstreams:

- JSON: `gastownhall/gascity:codex/json-rollup` exists and is populated with
  most intended first-rollup work through pushed commit `d3014963`; it is not
  yet final machine-move ready.
- Registry/gc pack: `donbox/gascityworkplace:codex/pack-registry-workstream`
  exists at Cleo's pushed checkpoint.
- gc4gc: `donbox/gc4gc:master`, `donbox/gc4gc:codex/gc4gc-producer-dev`, and
  `donbox/gc4gc:codex/gc4gc-producer-snapshot-20260518` exist.
- Pack deprecation: #2126 is the source of truth for the deprecation train.
- Docs/source reconciliation: #2318 is the source of truth for PackV2 docs
  source reconciliation.

Remaining move-readiness asks:

- Jasmine: finish JSON rollup boundary/validation and publish a final
  machine-move checkpoint that names incorporated, excluded, disposable, and
  abandoned old JSON branches/PRs.
- Cleo: confirm no meaningful registry/gc pack work is local-only after her
  next checkpoint.
- Grace: no blocking ask; gc4gc is portable.
- Penelope: intentionally separate on another machine.

### New Machine Bootstrap For Mabel / Coordination

```sh
mkdir -p /Users/dbox/repos/gc
cd /Users/dbox/repos/gc

git clone https://github.com/gastownhall/gascity.git gascity-workstream-coordination
cd gascity-workstream-coordination
git fetch origin codex/workstream-coordination
git switch codex/workstream-coordination

sed -n '1,220p' engdocs/coordination/active-workstreams.md
```

Suggested first prompt for Mabel on the new machine:

```text
Mabel, resume Gas City pack/package coordination from:

- repo: /Users/dbox/repos/gc/gascity-workstream-coordination
- branch: codex/workstream-coordination
- file: engdocs/coordination/active-workstreams.md

Please read the coordination file, refresh live PR/branch state for #2126,
#2318, #2119, #2129, Jasmine's JSON rollup, Cleo's registry/gc pack workstream,
and Grace's gc4gc handoff, then tell me where we are and what is safe to do
next on this machine.
```

## Communication Mechanism

Chosen mechanism: repo-backed coordination branch.

- Repository: `gastownhall/gascity`
- Branch: `codex/workstream-coordination`
- File: `engdocs/coordination/active-workstreams.md`

Agents should fetch this branch when they need the latest shared coordination
state. Agents may propose updates on their own branches or directly update this
coordination branch when asked, but this branch is not a product PR.

## Workstream Handoff

### Workstream

JSON

### Current Branch / PR

Branch: `codex/json-rollup`

PR: not opened yet

Base: `origin/main`

Owner: Jasmine

Worktree: `/Users/dbox/repos/gc/gascity-json-rollup`

### Latest State

Jasmine owns the JSON rollout end to end. The previous many-small-PR strategy
is replaced by a single JSON rollup / review-train PR so Julian can review one
coherent `gc --json` / `--json-schema` surface instead of many small PRs.

`codex/json-rollup` now exists, is pushed, and is populated through commit
`d3014963` (`fix: align json rollup integration`). It is not yet a final
machine-move checkpoint because the first-rollup boundary and full validation
are still in progress.

Current JSON source of truth is this workstream section plus
`codex/json-rollup`, not any individual JSON PR.

Included provenance PRs already incorporated into `codex/json-rollup`:

- #2317: schema-platform plumbing plus native management action JSON.
- #2222: session detail JSON plus oddball/root command JSON.
- #2250: formula/order inspection JSON.
- #2257: convoy inspection JSON.
- #2258: agent/rig routing inspection JSON.
- #2259: mail/trace/events inspection JSON.
- #2265: miscellaneous inspection command JSON.
- #2266: runtime/nudge/drain inspection JSON.
- #2267: doctor diagnostics JSON.
- #2271: lifecycle action summary JSON.
- #2273: graph/converge/order/formula action summary JSON.
- #2274: convoy/mail action summary JSON.
- #2287: open passthrough/custom schema support.
- #2256: service/skill inspection JSON, incorporated after preserving the
  already-merged `skill list --json` contract and taking the service additions.

Integration fixes currently added directly on the rollup:

- Removed a duplicate `OrderFiringCurrentCheck.WarmupEligible` method that
  blocked package builds after combining branches.
- Aligned the `version --json` test with the preserved `versionJSONResult`
  payload name.
- Preserved the existing `skill list --json` payload (`count` / `entries`)
  instead of switching to the alternate #2256 shape (`city_path` / `skills`).
- Suppressed deprecated config warnings during chat auto-suspend config load so
  auto-suspend tests are not polluted by unrelated stderr.

Excluded from the first train unless repaired:

- #2288: superseded by #2317's adoption branch payload.
- #2270: local rebase branch had `TestAutoSuspendChatSessions` failure from
  deprecated `[[agent]]` warning leakage to stderr.
- #2291: same local `TestAutoSuspendChatSessions` failure family as #2270.

Not yet decided / remaining boundary work:

- Confirm whether #2270 and #2291 should stay excluded/disposable now that the
  auto-suspend warning leak is fixed in the rollup, or whether their useful
  payloads should be cherry-picked and revalidated.
- Confirm no remaining JSON PR branches contain useful first-rollup work outside
  the incorporated list above.
- Publish the old JSON PR/branch close-abandon list after the rollup PR exists.

### Interface Contracts Other Agents Must Honor

- Human-readable output remains default.
- `--json` emits deterministic machine-readable output.
- stdout must be JSON-only when `--json` is used.
- Human diagnostics and warnings go to stderr unless intentionally represented
  in JSON.
- `--json-schema` exposes command schema metadata. The role-specific form
  `--json-schema=result` is accepted for result schemas.
- Result schemas live under `schemas/<command-path>/result.schema.json`.
- Shared failure schema lives at `schemas/failure.schema.json`.
- Do not introduce `--format json`.

### Attention Needed

Needs Mabel: no

Needs D. Box: no

Urgency: yellow

Reason: Jasmine has pushed a substantial rollup checkpoint, but the branch is
not yet machine-move ready. Cleo should not freeze registry command
schemas/tests until Jasmine confirms the final rollup contract and validation
status.

Structured failure JSON policy:

- New JSON-enabled commands should use the shared failure schema where the
  platform path applies.
- Full structured failure JSON for every command is staged command-by-command,
  not a reason to block otherwise clean result-schema work.
- Commands with intentional command-authored nonzero JSON must preserve that
  behavior and declare compatible schemas/tests.

Schema extension conventions:

- JSON Schema remains the schema language.
- Gas City extensions use `x-gc-*`.
- `x-gc-jsonl` remains the convention for JSONL record-count metadata. Absence
  means a single JSON document unless command docs/schema say otherwise.
- Keep schemas open where the producer is a passthrough or custom command and
  Gas City does not own the payload shape.

Validation matrix for `codex/json-rollup`:

- `git diff --check`: passed at pushed checkpoint `d3014963`.
- `make fmt-check`: pending for assembled train.
- `make vet`: pending for assembled train.
- `make check-docs`: pending for assembled train.
- `GOOS=linux make lint`: pending for assembled train.
- `go test ./cmd/gc -run 'TestJSON|Test.*JSON|TestJSONSchema|TestJSONSchemaManifest|TestJSONCommandOutputMatchesDeclaredResultSchema|TestDo.*JSON|Test.*JSONOutput' -count=1`: passed at pushed checkpoint `d3014963`.
- `go test ./cmd/gc -run 'TestAutoSuspendChatSessions|TestSkill|TestService|TestMail|TestConvoy|TestConverge|TestGraph|TestOrder' -count=1`: passed at pushed checkpoint `d3014963`.
- `go test ./cmd/gc -count=1`: started after `d3014963`, but interrupted
  before completion; no pass/fail result yet.
- `gc4gc` smoke tests: pending for assembled train.

Local-only JSON work state:

- The rollup branch is pushed at `origin/codex/json-rollup` through `d3014963`.
- No meaningful rollup code changes are currently local-only.
- This coordination update is the current local-only state until pushed.
- Existing local worktrees for #2270 and #2291 must not be deleted yet; they
  contain the last known source state for excluded-but-not-finally-disposed
  JSON work.

### Blockers / Cross-Workstream Risks

- `yellow`: Registry/gc pack command schemas/tests should not freeze until
  Jasmine confirms the rollup branch has assembled schema-platform plumbing and
  the validation matrix is passing.
- `yellow`: Pack-defined commands may eventually need schema discovery rules;
  flag pack-facing schema changes to Jasmine rather than patching JSON rollout
  branches directly.
- `yellow`: Do not introduce `--format json` or command-specific schema
  discovery conventions in registry work.
- `yellow`: If registry commands need JSON schemas before the rollup lands, use
  `schemas/<command-path>/result.schema.json`, shared failure schema
  compatibility, and `x-gc-jsonl` for JSONL record-count metadata.

### Needed From Other Agents

- Jasmine: finish first-rollup boundary decisions for #2270/#2291, run full
  validation, open the rollup PR, and publish the branch/PR close-abandon list.
- Cleo: flag any registry/gc pack command schema needs before freezing command
  output shapes.
- Mabel: no blocking help needed yet; use this section as the current JSON
  status if Donna asks from the Mabel thread.

### Last Updated

2026-05-18 17:43 PT by Jasmine

### New Machine Bootstrap

Repos to clone:

- `gastownhall/gascity`
- `gastownhall/gc4gc` or the available local equivalent for smoke testing, if
  needed.

Branches to fetch / checkout:

- `origin/main`
- `origin/codex/workstream-coordination`
- `origin/codex/json-rollup`
- Provenance branches for included PRs:
  - `origin/adopt/ga-nqfs0pd-pr2288`
  - `origin/codex/json-schema-platform`
  - `origin/codex/json-wave2-formula-order`
  - `origin/codex/json-convoy-workflow`
  - `origin/codex/json-rig-agent-routing`
  - `origin/codex/json-mail-events-trace`
  - `origin/codex/json-misc-inspection`
  - `origin/codex/json-runtime-nudge-drain`
  - `origin/codex/json-doctor-diagnostics`
  - `origin/codex/json-lifecycle-city-actions`
  - `origin/codex/graph-converge-order-actions`
  - `origin/codex/json-convoy-mail-actions`
  - `origin/codex/open-schema-passthrough-custom`
  - optional after revalidation: `origin/codex/json-pack-service-skill`

Worktrees to create:

- `/Users/dbox/repos/gc/gascity-workstream-coordination` on
  `codex/workstream-coordination`.
- `/Users/dbox/repos/gc/gascity-json-rollup` on `codex/json-rollup`.

Local-only state:

- None for rollup code through pushed commit `d3014963`.
- This coordination checkpoint must be pushed before it is portable.
- #2270 and #2291 old-machine worktrees have local/rebased state with known
  failing tests and are intentionally excluded from the first train unless
  Jasmine decides to salvage them after the current rollup validation.

Commands to validate setup:

```sh
git -C /Users/dbox/repos/gc/gascity-workstream-coordination status --short --branch
git -C /Users/dbox/repos/gc/gascity-json-rollup status --short --branch
git -C /Users/dbox/repos/gc/gascity-json-rollup fetch origin --prune
git -C /Users/dbox/repos/gc/gascity-json-rollup log --oneline -1
```

Old-machine worktrees safe to ignore:

- Individual clean JSON shard worktrees after their commits are represented in
  the rollup branch.
- Deleted/gone provenance branches for already-merged JSON PRs.

Old-machine worktrees that must not be deleted yet:

- `/Users/dbox/repos/gc/gascity-json-rollup`
- `/Users/dbox/repos/gc/gascity-workstream-coordination`
- `/Users/dbox/repos/gc/gascity-json-session-mutation-actions` until #2270 is
  fixed or explicitly discarded.
- `/Users/dbox/repos/gc/gascity-json-gnarly-session-order-actions` until #2291
  is fixed or explicitly discarded.
- Any Cleo/Mabel/Penelope pack worktrees they own.

Exact first prompt for Jasmine on a new machine:

> Jasmine, continue the JSON rollup from
> `engdocs/coordination/active-workstreams.md` on
> `origin/codex/workstream-coordination`. Clone/fetch `gastownhall/gascity`,
> create worktrees for `codex/workstream-coordination` and
> `codex/json-rollup`, then continue from pushed commit `d3014963`. Finish the
> #2270/#2291 boundary decision, run the documented validation matrix, smoke
> test against `gc4gc`, open the rollup PR, and publish the old JSON PR/branch
> close-abandon list. Preserve the accepted `--json` / `--json-schema`
> contract.

## Workstream Handoff

### Workstream

Pack Deprecation

### Current Branch / PR

Branch: `codex/packv2-wave2-goodbye-packv1`

PR: #2126, <https://github.com/gastownhall/gascity/pull/2126>

Base: `main`

Owner: Mabel / relevant implementation agents

### Latest State

#2126 is the source of truth for PackV1/PackV2 deprecation enforcement. It is
green and mergeable as of this update. It should remain conceptually separate
from registry/gc pack implementation.

Related docs/source reconciliation:

- #2318, <https://github.com/gastownhall/gascity/pull/2318>

### Attention Needed

Needs Mabel: yes

Needs D. Box: no

Urgency: yellow

Reason: Mabel should confirm whether #2126 introduces any constraints that
affect `gc import`, legacy `gc pack fetch/list`, or PackV2 import fields before
Cleo freezes related registry compatibility behavior.

### Interface Contracts Other Agents Must Honor

- Do not remove or change `gc import migrate` semantics until doctor /
  `doctor --fix` parity exists for the migrate corpus.
- No new `gc pack` replacement command for `gc import migrate`.
- Remediation messaging must remain actionable for hard-failed legacy
  constructs.
- Coordinate before changing legacy `gc pack fetch` or `gc pack list`
  compatibility.

### Blockers / Cross-Workstream Risks

- `red`: Removing `gc import migrate` before doctor parity would break the
  migration contract.
- `yellow`: Registry/gc pack work may touch compatibility messaging around
  `gc import` and legacy `gc pack` commands; coordinate before changing those
  behaviors.
- `green`: Pack deprecation can proceed independently from registry/gc pack as
  long as compatibility invariants are preserved.

### Needed From Other Agents

- Cleo: keep deprecation/remediation changes out of the registry workstream
  unless a compatibility invariant directly affects canonical `gc pack`
  behavior.
- Jasmine: flag if JSON diagnostics or stderr behavior affects deprecation
  warning/error tests.

### Last Updated

2026-05-18 12:10 PT by Mabel

## Workstream Handoff

### Workstream

Registry-gc-pack

### Current Branch / PR

Branch: `codex/pack-registry-workstream`

PR: not opened yet

Base: `upstream/main` / `gastownhall/gascity@03c80562`

Owner: Cleo

Current implementation worktree:

- Worktree: `/Users/dbox/repos/gc-pr2119`
- Current branch: `codex/pack-registry-workstream`
- Pushed branch: `donbox/gascityworkplace:codex/pack-registry-workstream`
- Current checkpoint commit: `a64fb1ba`
- State: clean and pushed after registry hardening, first `gc pack`
  dependency-command bridge, docs/reference update, and doctor guard for
  durable `registry:` selectors.

Older local branches are not current:

- `codex/pack-registry-1a-core`
- `codex/pack-registry-mainline`
- `codex/pack-registry-latest-main`

### Latest State

Cleo will maintain one long-lived registry/gc pack workstream branch for
several days rather than preparing small immediate review PRs. Registry
operations still come first inside that workstream.

The registry/gc pack source of truth is now
`donbox/gascityworkplace:codex/pack-registry-workstream`.

Dirty/unpushed work has been migrated and pushed. No meaningful local-only
registry work should remain on the old machine.

Completed inside the workstream since the preservation checkpoint:

- Registry operations hardening: remote-cache trust boundary, per-registry
  cache locking, safer config/cache add ordering, source validation on
  hand-edited `registries.toml`, and richer registry schema conformance tests.
- `gc pack add/remove/sync/upgrade/why` command bridge. `add` supports registry
  selectors and writes concrete durable import sources while preserving
  registry/ref/hash metadata in `packs.lock`.
- Legacy `gc pack fetch/list` remain on the old `[packs]` behavior and have
  regression coverage.
- `gc import` output remains stable through shared handlers; it has not entered
  warning/removal mode.
- `gc doctor` now flags durable `registry:` sources with remediation text.

Next milestone:

- Review/fill any remaining command-surface gaps (`gc pack check`, future
  dependency `list/show/outdated` shape, or explicit deferral), then run the
  final review-pass matrix before opening a workstream PR.

### Attention Needed

Needs Mabel: yes

Needs D. Box: no

Urgency: yellow

Reason: Mabel should confirm whether Pack Deprecation wants any compatibility
messaging changes before PR prep. Jasmine should flag any JSON rollup convention
changes that affect registry schemas; current registry JSON tests compose with
the branch-local platform.

### Interface Contracts Other Agents Must Honor

- Registry operations land first.
- Dependency mutation must not race ahead of registry config/catalog
  correctness.
- Preserve current PackV2 import fields: `source`, `version`, `export`,
  `transitive`, `shadow`.
- Do not implement #2129 `[[exports]]` in this workstream; treat it as design
  input/future direction.
- Registry handles such as `main:lighthouse` are command-time selectors only.
- Durable `pack.toml` imports must store concrete `source` plus optional
  `version`, not `registry:<registry>:<pack>`.
- Lock/cache internals may preserve registry/ref/commit/hash metadata.
- Preserve `gc import` compatibility and legacy `gc pack fetch/list`
  compatibility.
- `gc import migrate` has no `gc pack` replacement; doctor / `doctor --fix`
  must reach parity before removal.
- Compose with Jasmine's JSON rollup conventions once stable.

File ownership boundaries for Cleo's workstream:

- Cleo owns new registry/gc pack implementation files and tests:
  `internal/gchome`, `internal/packregistry`, `internal/packsource`,
  packman registry/hash/lock additions, `cmd/gc/cmd_pack.go`,
  `cmd/gc/cmd_pack_registry*_test.go`, and `schemas/pack/**`.
- Other agents should not edit `gc pack registry` command behavior or
  `schemas/pack/**` without coordinating here first.
- Pack deprecation agents may edit deprecation docs/doctor surfaces, but should
  coordinate before touching `gc import`, legacy `gc pack fetch/list`, or
  shared PackV2 import semantics.

### Blockers / Cross-Workstream Risks

- `red`: Do not base registry command JSON/schema tests on an unstable or
  superseded JSON branch without Jasmine confirmation.
- `red`: Do not change `gc import migrate` removal semantics in registry work.
- `yellow`: Coordinate with Pack Deprecation before changing legacy `gc pack`
  `fetch/list` behavior.
- `yellow`: Coordinate with Jasmine before freezing registry command JSON
  schemas or failure behavior.
- `green`: Registry/gc pack overlap with Pack Deprecation is small and should
  be managed through compatibility checkpoints, not branch merging.

### Needed From Other Agents

- Jasmine: confirm JSON rollup branch and schema/failure conventions.
- Mabel: keep Pack Deprecation source-of-truth visible and flag compatibility
  drift.
- Mabel: confirm whether #2126 introduces any hard compatibility constraints
  that affect `gc pack fetch/list` or `gc import` wrapper behavior.
- Cleo: continue from `a64fb1ba`, finish review-prep gaps, then open/update the
  workstream PR when the final matrix is green.

### JSON Assumptions

- Use `--json`; do not add `--format json`.
- Registry result schemas live under
  `schemas/pack/registry/<command>/result.schema.json`.
- Every public and nested public schema field needs `description`.
- Real `--json` stdout must validate against `--json-schema=result`.
- Failure behavior must follow Jasmine's JSON rollup once stable; until then,
  treat structured failure JSON as a coordination point, not a unilateral
  registry decision.

Needed from Jasmine:

- Current JSON rollup branch.
- Final source of truth for `--json-schema=result` and shared failure schema.
- Whether `x-gc-jsonl` is accepted, and its exact shape if accepted.
- Whether new commands should require structured failure JSON immediately or
  only when they opt into schema-backed buffering.

### Pack Deprecation Assumptions

- #2126 remains a separate PackV1/PackV2 deprecation train.
- Registry/gc pack preserves current PackV2 `source`, `version`, `export`,
  `transitive`, and `shadow` behavior for now.
- #2129 `[[exports]]` is design input/future direction, not this workstream's
  implementation scope.
- `gc import migrate` remains until doctor / `doctor --fix` parity exists.

Needed from Mabel:

- Flag any deprecation-train change that would alter `gc import`,
  `gc pack fetch/list`, or PackV2 import field semantics.

### First Stable Checkpoint Validation Gates

Run from `/Users/dbox/repos/gc-pr2119` on `codex/pack-registry-workstream`:

```sh
go test ./internal/packsource ./internal/packregistry ./internal/packman ./internal/config
go test ./cmd/gc -run 'TestPackRegistry|TestPackRegistryJSON|TestPackAdd|TestPackSync|TestPackCommandTree|TestDoImport|TestImport|TestImportStateDoctor|TestDoDoctor|TestJSONSchema|TestJSONUnsupported|TestJSONExecutionFailure|TestSyncLock|TestCheckInstalled'
make check-docs
git diff --check
```

These targeted gates passed on the old machine at `a64fb1ba`. A broader
`go test ./cmd/gc -count=1` attempt was stopped after running long with no
additional output; use the targeted matrix above plus CI/full package testing as
review prep.

Additional required gates:

- `gc pack registry` text behavior covers list/add/remove/refresh/search/show.
- Registry JSON output validates against checked-in result schemas.
- Unsupported JSON command paths use platform behavior.
- Diagnostics do not pollute JSON stdout.
- Registry add/search/show cover stale caches, partial reachability, ambiguous
  bare names, removed snapshots, and invalid registry/catalog inputs.

### Last Updated

2026-05-18 18:35 PT by Cleo

## New Machine Bootstrap

### Repos To Clone

- Main implementation repo:
  `https://github.com/donbox/gascityworkplace.git`
- Upstream remote to add/fetch:
  `https://github.com/gastownhall/gascity.git`

### Branches To Fetch / Checkout

```sh
git clone https://github.com/donbox/gascityworkplace.git /Users/dbox/repos/gc-pr2119
cd /Users/dbox/repos/gc-pr2119
git remote add upstream https://github.com/gastownhall/gascity.git
git fetch upstream main
git fetch origin codex/pack-registry-workstream
git switch codex/pack-registry-workstream
```

Coordination branch:

```sh
git fetch https://github.com/gastownhall/gascity.git codex/workstream-coordination:refs/remotes/upstream/workstream-coordination
git worktree add -B codex/workstream-coordination /Users/dbox/repos/gc-workstream-coordination upstream/workstream-coordination
```

### Worktrees To Create

- `/Users/dbox/repos/gc-pr2119` for registry/gc pack implementation.
- `/Users/dbox/repos/gc-workstream-coordination` for coordination updates.

### Local-Only State

None required. Registry/gc pack implementation state is pushed to
`origin/codex/pack-registry-workstream` at
`a64fb1ba`.

Old stashes on the old machine are preservation artifacts only:

- `registry workstream migration to codex/pack-registry-workstream`
- `pack registry work before true latest main`
- `pack registry work before main upmerge`

They should not be needed unless the pushed branch is lost.

### Setup Validation Commands

```sh
git status --short --branch --untracked-files=all
git log --oneline -5 --decorate
go test ./internal/packsource ./internal/packregistry ./internal/packman ./internal/config
go test ./cmd/gc -run 'TestPackRegistry|TestPackRegistryJSON|TestPackAdd|TestPackSync|TestPackCommandTree|TestDoImport|TestImport|TestImportStateDoctor|TestDoDoctor|TestJSONSchema|TestJSONUnsupported|TestJSONExecutionFailure|TestSyncLock|TestCheckInstalled'
make check-docs
git diff --check
```

### Old-Machine Worktrees Safe To Ignore

- `/Users/dbox/repos/gc-pr2119` branches
  `codex/pack-registry-1a-core`,
  `codex/pack-registry-mainline`, and
  `codex/pack-registry-latest-main` are obsolete for active work.

### Old-Machine Worktrees Not Safe To Delete Yet

- `/Users/dbox/repos/gc-pr2119` should not be deleted until the new machine has
  fetched and validated `codex/pack-registry-workstream`.
- `/Users/dbox/repos/gc-workstream-coordination` should not be deleted until
  this coordination update is pushed and visible from the new machine.

### First Prompt For Cleo On The New Machine

```text
Cleo, continue the registry/gc pack workstream from:

- repo: /Users/dbox/repos/gc-pr2119
- branch: codex/pack-registry-workstream
- checkpoint commit: a64fb1ba
- coordination file: /Users/dbox/repos/gc-workstream-coordination/engdocs/coordination/active-workstreams.md

First, refresh upstream/main and the coordination branch, verify the setup with
the commands in the New Machine Bootstrap section, then continue review-prep
for the registry/gc pack workstream. Keep registry/gc pack separate from
PackV2 deprecation except for explicit compatibility checkpoints.
```

## Workstream Handoff

### Workstream

Pack Reuse / Customization Design

### Current Branch / PR

Branch: managed by Penelope on another machine

PR: feeds into #2119 / #2129 as appropriate

Base: not tracked in this coordination file

Owner: Penelope

### Latest State

Penelope is continuing the user-facing pack reuse/customization guide and
design exploration on a separate machine. Do not migrate or interrupt that
context from this coordination branch.

### Attention Needed

Needs Mabel: no

Needs D. Box: no

Urgency: green

Reason: Penelope is intentionally staying on a separate machine; only update
this coordination file if her guide decisions affect #2119, #2129, registry/gc
pack CLI wording, or import/export semantics.

### Interface Contracts Other Agents Must Honor

- Treat #2129 `[[exports]]` as future design input, not as implemented registry
  behavior.
- Keep user-facing guide language aligned with actual implementation state.

### Blockers / Cross-Workstream Risks

- `yellow`: Reuse/customization guide may update terminology or examples used
  by #2119 and future registry docs.

### Needed From Other Agents

- Penelope: surface guide decisions that change registry/gc pack CLI wording or
  import/export semantics.

### Last Updated

2026-05-18 12:10 PT by Mabel

## Workstream Handoff

### Workstream

gc4gc / Operational Substrate

### Current Branch / PR

Coordination branch: `codex/workstream-coordination`

PR: none expected

Stable consumer repo: `https://github.com/donbox/gc4gc` on `master`

Producer/dev repo: `/Users/dbox/repos/gc/gc4gc-grace` on
`codex/gc4gc-producer-dev`

Owner: Grace

### Latest State

The gc4gc side quest is in producer/consumer split mode.

Stable consumer state:

- `/Users/dbox/repos/gc/gc4gc` is the stable consumer-facing copy.
- Branch: `master`.
- Latest known stable commit: `8d992e5 Point gc4gc at agent runtime checkout`.
- Remote: `https://github.com/donbox/gc4gc.git`.
- Mabel/Codex may consume stable artifacts here without Grace mediating.

Producer/dev state:

- `/Users/dbox/repos/gc/gc4gc-grace` is Grace's producer worktree.
- Branch: `codex/gc4gc-producer-dev`.
- Remote branch for current clean producer/dev baseline:
  `codex/gc4gc-producer-dev` at commit `52e6ec3`.
- Remote archival snapshot of Grace's old exact dev worktree:
  `codex/gc4gc-producer-snapshot-20260518` at commit `e38b97b`.
- The snapshot branch preserves the old dirty/untracked producer state as Git
  history. Prefer the clean `codex/gc4gc-producer-dev` branch for new work.
- Producer/dev may contain unpromoted or temporarily unstable producer changes.
- Do not ask Mabel/Codex to consume dev-worktree runs unless explicitly
  requested.

Gas City runtime used by stable gc4gc:

- Stable gc4gc invokes `gc` through
  `/Users/dbox/repos/gc/gc4gc/assets/scripts/gc-json.sh`.
- That helper defaults to
  `/Users/dbox/repos/gc/gascity-agent-runtime`.
- Expected runtime branch: `codex/gc4gc-agent-runtime-dolt-leak`.
- Expected managed-Dolt leak fix commits include:
  - `bd6b0152 Fix managed Dolt test process leaks`
  - `9c205d19 Tighten Dolt leak guard cleanup`
  - `5694e03f Clean up city init managed Dolt test`

Stable run artifact contract:

- Run artifacts live under `.runtime/runs/<run-id>/`.
- Stable core artifacts are:
  - `input.json`
  - `status.json`
  - `execution-manifest.json`
  - `result.json`
  - `findings.json`
  - `summary.md`
  - `proposed-comment.md`
- Optional additive artifacts are allowed.
- Do not rename, remove, or semantically repurpose stable core artifacts without
  an explicit compatibility plan.
- Do not write into or inspect `.gc/`; it is opaque Gas City-owned state.

Current stable lanes:

- `pack-pr-review` has a stable canary run for PR #2117:
  `.runtime/runs/20260515-005739-pack-pr-review-2117`.
- `gc-json-audit` is promoted as an additive audit lane with docs, skill,
  auditor agents, runbook, and experimental formula.
- Jasmine manually validated two JSON audit shards from stable gc4gc:
  - `.runtime/json-audit/20260516/status-config-supervisor/report.md`
  - `.runtime/json-audit/20260516/formula-order-dispatch/report.md`
- Remaining first-wave JSON audit shards:
  - `session-runtime-wait`
  - `convoy-workflow`
  - `mail-events-trace`

Current stable rigs:

- `gc4gc` points at `/Users/dbox/repos/gc/gc4gc`, prefix `gc`, initialized.
- `agent-runtime` points at `/Users/dbox/repos/gc/gascity-agent-runtime`,
  prefix `rt`, suspended, not initialized.
- `json-platform` points at
  `/Users/dbox/repos/gc/gascity-json-schema-platform`, prefix `jp`,
  suspended, not initialized.
- Do not casually initialize or resume suspended rigs during read-oriented
  audit work.

Known unpromoted work:

- `pack-design-drift-check` exists in Grace's dev worktree and produced a valid
  canary against PR #2119, but it is not promoted into stable gc4gc yet.
- Do not treat `pack-design-drift-check` as stable consumer surface until it
  goes through promotion and validation.

### Interface Contracts Other Agents Must Honor

- gc4gc exists to let Codex users get Gas City benefits inside their existing
  Codex workflow.
- Stable consumer worktree remains the only consumer-facing runtime unless
  explicitly promoted.
- Do not make gc4gc a parallel human-facing operating surface.
- Do not change JSON or registry implementation from this lane unless Jasmine
  or Cleo asks.
- Use `.runtime/` for gc4gc-produced artifacts, not `assets/` and not `.gc/`.
- Use Gas City surfaces honestly where that is the product path. Wrappers should
  be explicit, boring, and transparent.
- For promotion, validate in Grace dev, run a canary, produce a promotion packet,
  then have the consumer side verify before treating the change as live.

### Attention Needed

Needs Mabel: no

Needs D. Box: no

Urgency: green

Reason: Handoff is published. Stable gc4gc can be consumed for current artifact
inspection and JSON audit prep. No immediate human decision is required.

### Blockers / Cross-Workstream Risks

- `yellow`: gc4gc should validate against Jasmine's JSON rollup once that
  branch is assembled, but should not block on #2222 queue timing.
- `yellow`: formula-driven JSON audit fanout is not stable yet; keep using
  manual or bead-per-shard routing until another canary proves the lane.
- `yellow`: gc4gc may surface product friction for Cleo's registry/gc pack work
  but should not directly alter implementation branches.
- `green`: stable gc4gc is no longer local-only; it is pushed to
  `https://github.com/donbox/gc4gc`.

Product gaps discovered while using gc4gc:

- Filed: #2140 separates configured suspension policy from operational pause
  state.
- Filed: #2144 makes `gc sling --json` expose partial dispatch failures and
  backend-readiness blockers.
- Still likely worth filing:
  - implicit HQ rig consistency between `gc rig list --json` and
    `gc status --json`
  - `gc supervisor status --json`
  - `gc config show --json`
  - broader `gc config explain --json`
  - `gc formula list/show --json`
  - `gc order list/show --json`
  - schema backfills for existing stable `--json`
  - repeated deprecated system order-path warnings on stderr

### Needed From Other Agents

- Jasmine: continue JSON audit work when ready. Assume #2222 schema-platform
  baseline will land; do not cram formula/order JSON into #2222.
- Cleo: notify Grace if registry/gc pack work needs dogfood validation through
  gc4gc.
- Mabel: safe to consume stable gc4gc artifacts without Grace mediating.
- Grace: keep producer changes additive, validated, and promoted deliberately.

Recommended next actions:

- Finish remaining first-wave JSON audit shards:
  `session-runtime-wait`, `convoy-workflow`, and `mail-events-trace`.
- After another successful canary, consider bead-per-shard routing for the
  remaining shards.
- Keep formula-driven fanout experimental until shard routing and artifact
  consumption are boring.
- Promote `pack-design-drift-check` only after a fresh validation/promotion pass.

New Machine Bootstrap:

Local-only state:

- Stable gc4gc is no longer local-only. Clone it from
  `https://github.com/donbox/gc4gc.git`.
- Grace's producer/dev state is also represented remotely:
  - clean producer/dev branch: `codex/gc4gc-producer-dev`
  - archival snapshot branch: `codex/gc4gc-producer-snapshot-20260518`
- Runtime artifacts under `.runtime/` are local run evidence, not durable GitHub
  records unless an agent explicitly summarizes them into issues, PR comments,
  or coordination docs.
- Portable machine-transition bundle created on the old machine:
  `/Users/dbox/repos/gc/gc4gc-machine-handoff-20260518`.
- The bundle includes stable and Grace-dev Git bundles plus overlays for
  `.runtime/`, `.beads/`, and uncommitted/untracked producer state.
- The bundle intentionally excludes `.gc/`; Gas City owns that opaque state.
- The bundle is fallback evidence only now that the repo/branches are pushed.
- `.runtime/` is not required for bootstrap, but it is useful canary evidence.

Clone/bootstrap commands:

```sh
mkdir -p /Users/dbox/repos/gc
cd /Users/dbox/repos/gc

git clone https://github.com/gastownhall/gascity.git gascity-agent-runtime
cd gascity-agent-runtime
git fetch origin codex/gc4gc-agent-runtime-dolt-leak
git switch codex/gc4gc-agent-runtime-dolt-leak

cd /Users/dbox/repos/gc
git clone https://github.com/donbox/gc4gc.git gc4gc
cd gc4gc
git switch master

cd /Users/dbox/repos/gc
git clone https://github.com/donbox/gc4gc.git gc4gc-grace
cd gc4gc-grace
git switch codex/gc4gc-producer-dev
```

Optional archival snapshot checkout:

```sh
cd /Users/dbox/repos/gc
git clone https://github.com/donbox/gc4gc.git gc4gc-grace-snapshot-20260518
cd gc4gc-grace-snapshot-20260518
git switch codex/gc4gc-producer-snapshot-20260518
```

Validation commands:

```sh
cd /Users/dbox/repos/gc/gc4gc
git status --short --branch
git log -4 --oneline
sed -n '1,30p' assets/scripts/gc-json.sh

git -C /Users/dbox/repos/gc/gascity-agent-runtime status --short --branch
git -C /Users/dbox/repos/gc/gascity-agent-runtime log --oneline -6

assets/scripts/gc-json.sh rig list --json
assets/scripts/gc-json.sh formula show gc-json-audit
printf 'gc4gc fixed-runtime verification\n' \
  | assets/scripts/gc-json.sh sling --json --dry-run --no-convoy --stdin json-auditor-1

git -C /Users/dbox/repos/gc/gc4gc-grace status --short --branch
git -C /Users/dbox/repos/gc/gc4gc-grace log -3 --oneline
```

If the fallback bundle is restored, also validate preserved runtime evidence:

```sh
cd /Users/dbox/repos/gc/gc4gc
assets/scripts/validate-run.sh .runtime/runs/20260515-005739-pack-pr-review-2117
find .runtime/json-audit/20260516 -maxdepth 3 -type f -name report.md -print
```

### Last Updated

2026-05-18 17:36 PT by Grace
