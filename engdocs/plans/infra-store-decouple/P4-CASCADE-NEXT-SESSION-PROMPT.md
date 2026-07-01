# Next-session prompt — finish the non-work field-door cleanup (post-cascade)

> **SUPERSEDED (2026-07-01):** the current authoritative pair is
> `RECONCILER-CASCADE-NEXT-SESSION-PROMPT.md` + `RECONCILER-CASCADE-HANDOFF.md`
> (HEAD `e6f3d2b1e`, 20 sites, reconciler cascade = the primary unlock). Use
> those. This file is kept for continuity.

Paste the block below into a fresh session.

---

Continue the non-work-bead field-door cleanup on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`, HEAD `f38938195`).

**Read first, in order:** `engdocs/plans/infra-store-decouple/P4-CASCADE-HANDOFF.md`
(the execution guide — the landed "Cascade session" block, the updated Suggested
order, the RAW-BY-DESIGN carve-outs, the P5/P6 sections), then
`P4-CONVERSION-CONTRACT.md` (per-site swap rules + sibling table + RAW
fidelity-field rules) and `NONWORK-BEAD-FIELDDOOR-PLAN.md` (architecture).
Confirm a green baseline:
`go build ./cmd/gc/` and
`go test ./cmd/gc/ -run 'TestSessionClassifierInfoEquivalence|TestSessionSnapshotInfoEquivalence|TestSnapshotInfoOnlyFilesStayOnInfoAccessors|TestFrontDoorStoreFreeFilesStayStoreFree' -count=1`.

**Principle (hard rule):** direct read of metadata/bead FIELDS on any NON-WORK
object (session/nudge/mail/order/graph) is illegal — only generic WORK beads read
raw. This is the precondition for a per-class backend swap.

**What's DONE (this stack):** foundation P1–P3 (Info codec + `*Info` classifier
siblings + typed snapshot accessors, equivalence-proven), the P4 LOCALIZED slice,
the P6 read-guard, **the pool-demand cascade** (`688d3b79f`, `6742a463b`,
`d789dc2a2`, `8609a5198`), and — most recently — **four more small cascades**
(`9a3380e0e` nudge dispatcher [+`Info.TransportMetadata`], `29a152836`
named-session snapshot lookups [+`Info.Type`/`Info.ContinuityEligible`,
`IsSessionBeadOrRepairableInfo`, the named-session Info classifier family,
`FindCanonicalNamedSessionInfo`/`FindNamedSessionConflictInfo`], `2f61a7bf0` the
3 pure build_desired_state loops [+`scaleCheckPartialSessionRetainableInfo`],
`f38938195` the wait config-drift loop [+`lookupSessionBeadByIDInfo`]).
`nudge_dispatcher.go` + `named_sessions.go` are guard-pinned. Raw-accessor
surface is down to **20** non-test sites.

**What REMAINS (in order — each is ONE atomic, carefully-reviewed change; do NOT
fan parallel agents at a single connected component):**
1. **reconciler `*beads.Bead session` Info-threading — NOW the primary unlock.**
   `session_reconciler.go`/`session_reconcile.go` thread a raw `*session` through
   `healState`/`checkStability`/`checkChurn`/`markProviderTerminalError`/… and the
   *ForAgent classifier family (`isManualSessionBeadForAgent`/
   `isEphemeralSessionBeadForAgent`/`isLegacyManualSessionBeadForAgent`). Converting
   these (carry the `Info` alongside/instead of the bead) is what unblocks the 20
   remaining sites: the rest of `build_desired_state.go` (2079/3341/3570/3816/4165),
   `city_runtime.go` (2658/3056), `session_beads.go` (2033), `soft_reload.go:103`
   (also needs a `template_overrides`/raw-metadata accessor on `Info` for
   `sessionCoreConfigForHash`→`applyTemplateOverridesToConfig`), and `cmd_wait.go`
   1164 (the wait-nudge helper family: `cachedSessionCanReceiveWaitNudge`/
   `waitNudgeAgent`/`sessionProviderFamily`/`waitNudgePollerKey`). See the
   handoff's "State (Post-cascade session)" block for each site's blocking reason.
   Add each newly-accessor-free file to `snapshotInfoOnlyFiles`.
2. **P5 `closeBead` cross-class split** (LANDMINE — isolated, last; recording-fake
   oracle; close-THEN-release; preserve skip-if-already-closed idempotence).
3. **P6** delete dead bead classifiers/`Open()`/`FindSessionBeadBy*` (codec edge
   `session_bead_snapshot.go` is EXEMPT) + widen the guard to forbid
   `.Store().Store` in converted files.

**DO NOT convert (RAW-BY-DESIGN, not leaks):** `usage_compute.go`
(`emitDueComputeFacts`/`emitComputeFactForBead` — usage-bookkeeping metadata, not
session-identity attrs) and `city_status_snapshot.go`
(`countCitySessionsFromSnapshot` — `IsSessionBeadOrRepairable` reads Type/labels
the Info projection drops; prove the snapshot-only-holds-session-beads invariant
first). Details in the handoff's RAW-BY-DESIGN section.

**Method (proven this session):** keep each original classifier untouched + ADD
the typed sibling + ADD an equivalence case (byte-identical oracle), THEN flip the
signature with ALL its callers in the SAME commit. `snapshot.OpenInfos()[i]` is
the precomputed projection of `Open()[i]`, so raw and Info slices coexist during
partial migration — a full-component atomic flip is NOT required. For foundation
gaps, add the Info field + codec population + equivalence case BEFORE the site
that needs it. Test call sites project fixtures via the package helper
`sessionInfosFromBeads([]beads.Bead) []session.Info`.

**Build/commit hygiene:** `git checkout go.sum` after builds; commit AND push with
`--no-verify` (stale hooksPath + the pre-push hook runs the full suite and
times out — run gates manually). Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt is
LOCAL-ONLY (no `bd dolt push`).

**Gates before ready:** `go build ./...` · `go vet ./...` ·
`golangci-lint run ./cmd/gc/... ./internal/session/...` (0) · the equivalence +
guard tests · targeted subject suites (pool/reconcile). The build host is
oversubscribed — targeted `-run` locally; CI on dedicated runners is the
byte-identical gate.

**Finish (only when #3839 CI is verified GREEN — no premature ready):**
- `gh pr checks 3839 --watch`
- ready (gh pr ready aborts on projectCards — use the API): `gh api graphql -f query='mutation($id:ID!){markPullRequestReadyForReview(input:{pullRequestId:$id}){pullRequest{isDraft}}}' -f id=$(gh api repos/gastownhall/gascity/pulls/3839 --jq .node_id)`
- label: `gh api --method POST repos/gastownhall/gascity/issues/3839/labels -f 'labels[]=status/needs-review-auto'`

**Done =** every non-work consumer reads via `session.Info` (grep-clean of raw
snapshot accessors + `.Store().Store`), the guard forbids regression, full gates
+ #3839 CI green, #3839 ready + labeled. Update
`memory/infra-beads-decoupling-plan.md`.

---
