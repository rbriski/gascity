# Simplification — Morning Brief

_Overnight run 2026-07-07. Base `b56c4cc43` on `simplify/audit-2026-07-07`. All work is LOCAL — nothing pushed._

**34 branches** built + reviewed. Gates: every code branch `build/vet/tests = pass` (S35 is docs-only). Reviews: **34/34 approved** (9 clean, 25 approve-with-nits), **0 invariant violations**, 19 flagged as genuine complexity reductions.

The proposal (`SIMPLIFICATION-PROPOSAL.md`) has the full analysis, 7 themes, and per-item detail. This brief maps what got built and how it reviewed, so you can walk it commit-by-commit.

## How to review

```bash
# the proposal + this brief are on the base branch:
git log --oneline simplify/audit-2026-07-07 -3
# see any branch's change:
git diff b56c4cc43..simplify/<id>   # e.g. simplify/s16, simplify/s19-b
# build/test a branch in a scratch worktree (temp impl worktrees were cleaned up):
git worktree add /tmp/rev-s16 simplify/s16 && (cd /tmp/rev-s16 && go build ./... && go test ./...)
# cherry-pick the ones you like onto your working branch:
git cherry-pick <commit>
```

## A. Auto-land wins — ready to cherry-pick

Low-risk, invariant-preserving, gated clean. Ordered by priority.

| branch | id | title | commit | verdict | cx↓ | LOC |
|--------|----|-------|--------|---------|-----|-----|
| `simplify/s16` | S16 | Surface the seven swallowed errors on destructive/routing… | `856f0d5a1` | approve-with-nits | yes | +295/-67 |
| `simplify/s20-a` | S20 | Typed SessionState enum with mandatory handlers, plus an … | `2dc0915a3` | approve-with-nits | no | +759 net (hand-… |
| `simplify/s35` | S35 | Record negative result: do NOT unify internal/convergence… | `02d4f8a34` | approve | yes | +116 |
| `simplify/s23-a` | S23 | Finish the Info front-door migration: one tick-scoped mut… | `998be6290` | approve-with-nits | yes | +279/-56 |
| `simplify/s12` | S12 | One legacy-state canonicalizer + one shared list filter (… | `24cf16fbe` | approve | yes | -16 net (53 ins… |
| `simplify/s13` | S13 | Collapse the three formula-attachment pipeline copies int… | `79a7c2a54` | approve | yes | sling_core.go -… |
| `simplify/s37` | S37 | Extract a single scope-close/scope-abort reconciler in in… | `4b7d6e7cf` | approve | yes | +264 / -116 (ne… |
| `simplify/s32` | S32 | Collapse duplicated attempt/create paths: RetryHandler de… | `621720905` | approve-with-nits | yes | +528/-308 acros… |
| `simplify/s04-a` | S04 | Extract the ~700-line bd/jq shell-codegen block out of co… | `202e6fd79` | approve-with-nits | no | config.go -710 … |
| `simplify/s06` | S06 | Collapse config accessor boilerplate: generic cachedPack*… | `7dda6206a` | approve-with-nits | yes | +188 / -353 (ne… |
| `simplify/s03` | S03 | Advertise a bounded recency-query capability for closed h… | `b42f8ab09` | approve-with-nits | no | +213 |
| `simplify/s07` | S07 | Guardrail note: leave AgentDefaults triple-merge and lega… | `e7879dcb2` | approve-with-nits | no | +211/-186 |
| `simplify/s05-a` | S05 | Unify Agent patch/override merge and deep-clone into sing… | `a72dd5d53` | approve | yes | +224 / -333 (ne… |
| `simplify/s09-a` | S09 | Kill the stringly-typed session/dispatch metadata layer: … | `d00260ce9` | approve-with-nits | yes | +182/-51 |
| `simplify/s26-a` | S26 | Collapse the trace double-record API: one typed surface, … | `1df389949` | approve | yes | +276/-427 (net … |
| `simplify/s11` | S11 | Collapse session.Manager's telescoping API: 9 Create vari… | `3254b80e1` | approve-with-nits | yes | +375 -94 |
| `simplify/s18` | S18 | Deduplicate the triple-pasted routed-state warning block … | `9d2f5e33c` | approve | yes | -24 in sling_at… |
| `simplify/s31` | S31 | One childStats() scan replacing six duplicate child-proje… | `a52aab358` | approve | yes | +227 / -138 (ne… |
| `simplify/s33` | S33 | Flatten reconcile.go error plumbing: paths return (action… | `87f7c7b53` | approve | yes | +131 / -218 (ne… |
| `simplify/s30-a` | S30 | Structural commit-point helper: make the convergence-loop… | `ff936b715` | approve-with-nits | yes | +308 / -158 (ne… |

### Reviewer notes (auto-land)

- **S16** (`simplify/s16`, approve-with-nits): Approve for auto-land — genuine correctness fix, not cosmetic; both fail-closed paths adversarially checked and confirmed not to wedge legitimate cleanup (absent tmux sessions don't error the probe; sling fail-closed is stateless per invocation), the drain fix removes a real complete-against-stale-snapshot bug, and route-config consolidation is behavior-identical with a perf win (1 city.toml parse per ProcessControl vs ~4). Non-blocking follow-ups: F1 add a direct test for the reconciler skipped_liveness_error path; F2 extend the liveness-error gate to failed-create close and drain-ack finalize (or fix the comment) since protection is currently partial; F3 distinguish beads.ErrNotFound in needsConvoyRecovery so a persistently-deleted parent still triggers recovery.
- **S20** (`simplify/s20-a`, approve-with-nits): Land it. The behavior change (per-tick stderr spam -> durable throttled session.unknown_state event with 30m escalation) is exactly the item's stated intent, reuses the proven stranded-diagnostic marker pattern, and all CI invariants hold. Before or shortly after landing: (1) clear the unknown-state markers when a session re-enters a known state so a recurrence of the same unknown value isn't silent; (2) file/track the slice-b bead for the typed SessionState enum so the 'simplification' half of S20 doesn't evaporate — as shipped this branch adds capability rather than removing complexity.
- **S35** (`simplify/s35`, approve): Land as-is. The negative result is well-evidenced and each idiom/gap citation checks out against live code. Optional follow-up when the #3872 rewrite starts: refresh the LOC/line-number citations, and consider adding the marker-last ordering test the doc itself recommends to internal/convergence so the blueprint's contract is enforced rather than commented.
- **S23** (`simplify/s23-a`, approve-with-nits): Land this slice. It is a safe, behavior-preserving enabling move that makes the forgotten-fold coherence bug class unrepresentable via one fold path plus a source-scan guard. Do not mark item S23 complete: Phases 2 (predicate-pair collapse) and 3 (god-function split) remain. When Phase 3 splits the file, widen TestReconcileTickFoldFrontDoor to package-wide scanning, and consider retiring the infoByID alias by consuming apply()'s return value.
- **S12** (`simplify/s12`, approve): Land as-is. Independently verified: build, vet, and internal/session tests pass on the branch; diff is a pure deduplication (-16 net lines) with no invariant or behavior impact.
- **S13** (`simplify/s13`, approve): Land as-is. Optional follow-up: delete the dead `isGraph && opts.Force` checkAttachments branch inside attachFormulaToBead's non-graph region, and consider a future hook-parameterization item if attachBatchFormula's divergence is ever unified.
- **S37** (`simplify/s37`, approve): Land as-is. Verified independently: go build/vet clean, go test ./internal/dispatch -count=1 ok. 3 close copies -> 1 (closeScopeAsPassed), 2 abort copies -> 1 (abortScope), 3 fanout epilogues -> one-line calls to pre-existing reconcileClosedScopeMemberWithOptions; error wrapping and skip ignore-ids byte-matched against b56c4cc43. New table tests cover both helpers including the already-closed-body branch.
- **S32** (`simplify/s32`, approve-with-nits): Land as-is. Optionally follow up: derive RetryResult.Iteration/FirstWispID from the actual create outcome (0/empty when trigger-gated) and wrap CreateHandler validation errors in RetryHandler with the source bead ID for context.
- **S04** (`simplify/s04-a`, approve-with-nits): Land it. Verbatim same-package extraction, gates independently verified, zero behavior risk. Track two follow-ups: (1) approach (b) table-driven triplet collapse to actually reduce complexity, (2) rehome the session-capacity Agent helpers that rode along in the contiguous move so workquery.go matches its name.
- **S06** (`simplify/s06`, approve-with-nits): Land after moving the stranded SetupTimeoutDuration doc comment from above durationOr onto the method itself (two-line fix). Everything else is clean: ~250 production lines deleted, single ownership of the parse-fallback and cache-miss protocols, gates independently re-verified (go vet, go build ./..., go test ./internal/config all pass on /data/tmp/simplify/wt/s06).
- **S03** (`simplify/s03`, approve-with-nits): Land as-is. Track the follow-up that consumes the seam (orders_feed/huma_handlers_beads O(limit) rewrite for #3253) so the enabler does not remain dead code; optionally add the ErrQueryRequiresScan guard to CachingStore.ListBoundedHistory when the first non-bd implementation appears.
- **S07** (`simplify/s07`, approve-with-nits): Auto-land. Pure same-package file move with all gates green and guardrails respected. At land time, file the deferred pack.go god-function-split bead in bd and optionally trim the in-source deferral comment to a one-line pointer.
- **S05** (`simplify/s05-a`, approve): Land as-is. Genuine simplification: net -109 lines, two duplicate merge bodies and two duplicate deep-copy bodies collapsed to single test-guarded implementations, with the aliasing bug class made unrepresentable by TestAgentCloneIsDeep and the adapter guarded by new append-modifier assertions. The Clone tombstone deep-copy is a latent bug fix, not a regression. Optional follow-up: drop or comment the dead tombstone copies in toAgentPatch.
- **S09** (`simplify/s09-a`, approve-with-nits): Land as-is. What's committed is a byte-identical, well-tested constant unification with the deliberate list divergence now locked by test. File two follow-ups: (1) migrate cmd/gc's parallel sleep-reason literals/constants onto session.SleepReason (mechanical, ~6 files); (2) the deferred Part 1 Info codec as its own parity-gated change, plus the molecule_id reader repoints in t3bridge/runproj/api.
- **S26** (`simplify/s26-a`, approve): Land as-is. Optional follow-up: type the pool-cap rejection path (usage.rejection returning TraceSiteCode/TraceReasonCode) and constant-ize the remaining dynamic outcome strings to finish closing the typed surface.
- **S11** (`simplify/s11`, approve-with-nits): Land it. The core simplification is real — positional-arg soup eliminated at the workers, both external callers migrated, defaulting locked by tests, zero invariant or behavior regressions found. Before or immediately after merge: (1) fix the 'CreateSpec' doc-comment typo in manager.go, (2) update the AGENTS.md worker-boundary migration note that names the old wrapper call, (3) file the follow-up bead to delete the 9 retained Create* wrappers and 5 NewManager* presets (or at least mark them Deprecated) so the telescoping surface actually shrinks rather than growing by one.
- **S18** (`simplify/s18`, approve): Land as-is. Mechanical, verified-byte-identical dedup (27 lines deleted net of the doc comment) exactly matching S18 approach (a); new table-driven test locks the extracted helper's wording, ordering, and nil semantics. No invariants touched, no wire/event surface involved.
- **S31** (`simplify/s31`, approve): Land as-is. Independently verified: go build ./... OK, go vet clean, go test ./internal/convergence -count=1 OK (4.6s) in /data/tmp/simplify/wt/s31. Filter-drift semantics preserved verbatim; fetch consolidation is safe because intervening writes (SetMetadata/CloseBead) touch only the parent bead. New childstats_test.go covers the drift cases (closed-without-parseable-iteration, zero-timestamp skip, open/in_progress max). Genuine reduction: six duplicated loops and two store-fetch helpers deleted, one shared pure scan added, 3-4 Children() round-trips per terminal transition collapsed to 1.
- **S33** (`simplify/s33`, approve): Merge as-is. This is exactly what S33 approach A asked for: one wrap site, typed action set, 87 net lines deleted from reconcile.go with zero behavior change. Optional follow-up (non-blocking): consider renaming the ReconcileAction constants with a Reconcile-specific prefix to visually distinguish them from the HandlerAction family in the same package.
- **S30** (`simplify/s30-a`, approve-with-nits): Land as-is. Optionally tighten the commit doc comment to say the contract is enforced by convention-at-call-site (marker as dedicated trailing param) rather than made type-impossible, and add a one-line note that new commit points must not write the marker key via the writes slice.

## B. Competing approaches — your call

Each structural (`needs-julian`) item was spiked as 2–3 competing branches so you can compare. These are SPIKES (sound direction, not merge-ready); the reviews flag what each needs before wiring.

### S24 — Pool satisfaction as a level (bead-open AND runtime-alive, with hysteresis) plus a ghost-session ledger healer

_Bugs: #1029, #2083, #2285, #3554, #3872, #1542_

| approach | branch | verdict | cx↓ | LOC |
|----------|--------|---------|-----|-----|
| a | `simplify/s24-a` | approve-with-nits | no | +421 |
| b | `simplify/s24-b` | approve-with-nits | no | +270 -7 |
| c | `simplify/s24-c` | approve-with-nits | no | +206 / -10 |

- **s24-a**: Direction is sound and worth pursuing: the single satisfaction predicate (bead-open AND runtime-observably-alive, unknown fail-safe alive) plus level-based hysteresis is the right fix class for #2083/#3872/#1029, and keeping both debouncers as in-memory tick state preserves the no-status-files and demand-is-pure-over-beads invariants. Tests are thorough for the pure mechanism. Approve the spike; before wiring, hoist the per-session ListRunning partial-check to once per tick, rate-limit the #2285 diagnostic, and require the wiring phase to DELETE the superseded demand-decider paths so the item actually reduces complexity rather than adding a third decider.
- **s24-b**: Accept as spike evidence for the needs-julian decision, but do NOT land on main or pursue activation of approach b. The spike confirms the backlog's prediction: making demand runtime-aware (approach b) fights a codebase-wide assumption that bead-open == staffed, requiring a fake-provider test sweep, hysteresis hardening, and unresolved stranded-work re-dispatch semantics — while still leaving the ghost bead open in the ledger. Recommend Julian pick approach a (a level-style healer that observes runtime death and closes the ghost session bead, letting the existing pure-bead demand path recover naturally): it heals the source of truth instead of filtering around it, needs no demand-path changes, and reuses the dead-pool sweep's existing protection-window guards instead of duplicating them.
- **s24-c**: Direction is sound for a stopgap: the liveness-as-satisfaction-level idea from S24 is validated with a conservative, well-guarded implementation, correct nil-disable plumbing, and passing targeted tests (verified: go vet clean, 4 targeted tests pass in the s24-c worktree). Merge-worthy as a stopgap only if (1) Julian accepts the missing hysteresis for the tmux provider, and (2) a follow-up bead is filed for the cap-saturated case where the ghost bead still blocks its replacement until the sweep closes it. The approach-(a) ledger healer remains the real fix; this branch should carry an expiry condition, not become the resting state.

### S19 — Level-triggered session convergence: one idempotent per-tick converge pass unifying identity, priming, and durable-vs-runtime reconciliation

_Bugs: #3872, #3849, #2073, #2112, #1029, #2285, #2083_

| approach | branch | verdict | cx↓ | LOC |
|----------|--------|---------|-----|-----|
| a | `simplify/s19-a` | approve-with-nits | no | +337/-46 (net +… |
| b | `simplify/s19-b` | approve-with-nits | no | +313 / -1 |
| c | `simplify/s19-c` | approve-with-nits | no | +430 |

- **s19-a**: Direction is sound and worth pursuing: the two pure derivations are the right seams for the S19 converge pass, tests are real, and all preserved invariants hold. Approve as a spike/scaffolding slice only — do not count it as a simplification win yet (net LOC up, identity still co-authored per path). Before merging any of approach a, require the follow-up phases that actually consume these seams (adoption-alias fix, primed_at marker, ladder unification), and either fold the post-helper key stamping (live_hash/session_origin/synced_at, pool_slot) into the derivation or document why it stays outside.
- **s19-b**: Direction is sound and worth pursuing: the pure observe->diff->act core with a legacy-parity pin is exactly the right way to approach restructuring the hottest code in the repo, and the truth-table tests are good. Approve the spike, but before wiring deriveConvergeActions into Phase 1: (1) fix the stamp-vs-nudge ordering (or redefine the marker as 'priming_attempted' with explicit delivery confirmation) so a crash cannot permanently lose the startup prompt; (2) decide deliberately on the TrimSpace divergence rather than inheriting it; (3) keep the follow-up scoped to actually deleting the per-path edge-triggered stamping so complexity genuinely drops. Gates independently verified on the branch (build/vet/new tests pass).
- **s19-c**: Approve as a direction spike (needs-julian). The pure CanonicalIdentity record + table-tested Converge(durable,runtime)->actions shape is the right Layer-1 foundation, invariants are untouched, and the code is dormant so merge risk is near zero. Before the real reconciler wiring: (1) decide who restarts dead-with-transcript sessions, (2) pin down StartFresh-vs-live-pane teardown semantics, (3) gate empty-hash StartFresh on Observed. Do not count this as a completed simplification — the deletion payoff is entirely in the deferred cmd/gc migration.

### S22 — Convergence sweep for ownerless open coordination beads + structural 'abandoned workflow root' predicate

_Bugs: #3407, #2895, #1542, #2112, #2987_

| approach | branch | verdict | cx↓ | LOC |
|----------|--------|---------|-----|-----|
| a | `simplify/s22-a` | approve-with-nits | no | +252 |
| b | `simplify/s22-b` | approve-with-nits | no | +358 (no deleti… |
| c | `simplify/s22-c` | approve-with-nits | yes | +358 -5 |

- **s22-a**: Approve the spike direction; the structural predicate is the right unification and lives in the correct shared package. Before wiring: (1) audit all control-bead close orderings for the transient all-closed window and add a second-read confirm or dependency-aware check in the sweep; (2) fold the disposition stamp into the close (updateMetadataAndClose-style) and add gc.outcome, testing against a strict store; (3) land the four consumer rewirings in the same PR so the change is a net simplification rather than additive.
- **s22-b**: Approve the direction. The structural WorkflowRootAbandoned predicate + min-age gate is sound, well-tested, and inert by default. Before any merge: wire or drop DefaultAbandonedRootMinAge, add a test for the activated listLiveSourceWorkflowRoots exclusion path, and design the reaper follow-up around store-consistency (partial subtree reads) and per-tick query cost before it starts closing roots.
- **s22-c**: Direction is sound and worth pursuing: the structural abandoned-root predicate is judgment-free and count-only, and oldest-first ranking fixes the real #2112 starvation class. Before promoting beyond spike: (1) make the budget gate failure-closed for unranked beads, (2) either narrow the rank candidate set to actually-eligible beads or soften the ceil(N/B) claim in comments/tests, (3) decide the v1-root finalize-less race with Julian, (4) add one dispatch-level test through listLiveSourceWorkflowRoots.

### S21 — One typed relevance predicate shared by the dispatcher wake filter and the claim query

_Bugs: #3892, #3964, #1938_

| approach | branch | verdict | cx↓ | LOC |
|----------|--------|---------|-----|-----|
| a | `simplify/s21-a` | approve-with-nits | no | +152 / -8 (net … |
| b | `simplify/s21-b` | approve-with-nits | no | +395 / -6 |
| c | `simplify/s21-c` | approve-with-nits | no | +341 -6 |

- **s21-a**: Direction is sound and worth pursuing — approve the spike for continuation, not merge. Require the claim-side half (retire the jq/shell ready query onto a typed beads.Store query consuming the same predicate) before claiming S21 done; until then the shared-definition win is aspirational. Before merge: (1) decide whether the self-actor guard should compare against a stamped recorded-by identity rather than env-derived eventActor(), since the real self-echo arrives under cache-reconcile and the current guard mostly hits announceClosedMolecule instead; (2) add a test for the empty-GC_ALIAS/human-self edge; (3) unexport IsBeadLifecycleWakeType unless the claim side uses it.
- **s21-b**: Direction is sound and worth pursuing: the typed ControlTarget/RoutedControlBead predicate is pure, well-documented, table-tested (including the empty-target-claims-nothing guard), faithfully mirrors the shell claim query, and the wake wiring fixes the #3964 self-trigger with correct fail-open semantics. Before merge: (1) decide the dependency-close wake question — as written, worker-finish events no longer wake the dispatcher and the 5s sweep carries the primary flow; (2) commit to the second half (retire workflowServeControlReadyQueryForBeads through the same predicate) or the branch adds duplication without deleting anything; (3) validate Dolt contention of the persistent store handle alongside bd subprocesses; (4) add a direct test for buildControlWakeTarget. Behavior change (dropping non-routed/self events) is the item's stated intent, hence behavior_preserved=true within that intent.
- **s21-c**: Direction is sound and worth pursuing: relevance as a typed events.WatchFilter value, defined once in internal/dispatch and applied at the subscription boundary, is the right shape and keeps the Watcher contract additive. Before merging beyond spike status: (1) build the claim-side half (typed store query derived from the same predicate) or soften the 'unrepresentable' doc claims; (2) confirm the self-actor string and activate ExcludeActor or file a bead for it; (3) either use WatchFiltered/SubjectPrefix in production or trim them; (4) restore ignored-event observability; (5) delete the now-vestigial workflowEventRelevant guard once the subscription filter is the sole gate.

### S34 — Rebuild convergence scopes on config reload; delete the #2403 staleness-detection taxonomy

_Bugs: #2403, #2357, #2285_

| approach | branch | verdict | cx↓ | LOC |
|----------|--------|---------|-----|-----|
| a | `simplify/s34-a` | approve-with-nits | yes | +145 / -149 (ne… |
| b | `simplify/s34-b` | approve-with-nits | yes | +108/-151 (net … |

- **s34-a**: Merge after (or with) two one-line hardening tweaks: (1) in adopt(), also require existing.store == store before preserving a scope, so store-object replacement (cs.update closes old handles) rebuilds instead of keeping a dead store; (2) call rebuildConvergenceScopes in the storeMetadataChanged rev-unchanged branch. Direction and execution are sound: the #2403 detect-and-punt taxonomy is genuinely deleted, convergenceScopeForRig collapses to lookup + #2357 fail-loud, the rebuild runs on the event loop under convScopesMu, and new scopes correctly reuse the needsStartupReconcile contract. Gates verified independently (build/vet/targeted tests pass at 958b16500).
- **s34-b**: Merge. Genuine simplification with the #2403 bug class made unrepresentable: scopes are re-derived from config on every reload (rebuildConvergenceHandler reuses the existing needsStartupReconcile/tick machinery), so the entire staleness-detection taxonomy (validateConvergenceScopeCurrent + four restart-the-controller branches, both stale-scope skip guards in tick and startup reconcile) is deleted rather than moved; convergenceScopeForRig collapses to a map lookup plus the preserved #2357 fail-loud check. Single-writer/mutex discipline verified at the call site; gates independently re-run and pass. Before merge, consider adding one rebound-rig test and a one-line comment on the nil-activeIndex window; neither blocks.

## C. Proposal-only (not implemented — your direction)

Lower-priority `needs-julian` items left as proposal-only to keep the branch set reviewable. See `SIMPLIFICATION-PROPOSAL.md` for full detail.

- **S29** — Retire the three legacy dual-mode machineries: per-tick migration scans, store==nil desired-state mode, and v1 check/retry-eval control engine  _(bugs: #3892, #2987, #3872, #3789)_
- **S08** — Wire-or-delete sweep: retire dead/phantom half-built machinery across lifecycle fold, wake engine, trace, and convergence  _(bugs: #3789, #3872, #3554, #1029)_
- **S28** — Extract a typed PendingCreateLease protocol from the async-start staleness machinery  _(bugs: #1542, #2073, #2895, #3849)_
- **S01** — Beads cache: absorb/evict primitives, one reconcile merge algorithm, and ApplyEvent collapsed to invalidate-or-absorb  _(bugs: #2987, #2210, #2927, #3789, #2153)_
- **S10** — Make read/plan paths pure: side-effect-free session Info projection and Observe/Plan/Materialize split for desired-state construction  _(bugs: #3872, #2987, #3789)_
- **S02** — Per-bead dirty overlay: stop declining the entire cache when one bead is dirty  _(bugs: #2987, #2153)_
- **S15** — Kill stringly routing duplication: one typed Router at both edges, and control beads claimed by kind not route string  _(bugs: #2987, #1938, #3964, #3892)_
- **S14** — One launchWorkflow chokepoint: single duplicate-launch guard and one formula compile per launch  _(bugs: #1053, #720)_
- **S36** — Route launch-only-drift Relaunch through buildPreparedStart: one launch-config derivation, not two  _(bugs: #3872)_
- **S38** — Replace findLatestAttempt's four-stage ref-string-surgery cascade with durable gc.control_for lineage metadata  _(bugs: #3789)_
- **S27** — Downgrade the WAL-grade trace store to a rotating JSONL log and make flush fire-and-forget with an explicit Flush() barrier  _(bugs: #3789)_
- **S25** — Decompose the reconciler mega-function and buildDesiredStateWithSessionBeads along their existing trace-phase seams  _(bugs: #3789, #3892)_
- **S17** — Replace formula-NAME matching for base_branch/target_branch injection with formula-declared vars

## D. Caveats & how this was built

- **All local, nothing pushed.** 34 `simplify/s*` branches off `b56c4cc43`. Temp build worktrees were removed; branch refs kept.
- **Cap for reviewability:** 20 auto-land (1 branch each) + top-5 structural items as competing branches. 13 lower-priority `needs-julian` left proposal-only (section C).
- **Spikes add code before they subtract it.** Several `needs-julian` branches (and S20) show `cx↓ = no`: they build scaffolding/pure seams first; the complexity win comes in the follow-up that *deletes* the superseded paths. The reviews call this out per item.
- **Every branch built and passed its targeted tests; every branch was independently reviewed by a separate model; invariants held across all 34.** Reviews are advisory — trust but verify the ones you want to land.
- Pipeline: Fable discovery (11 subsystem maps → 66 candidates) → dedup/rank/tag → Opus implementation (isolated worktree, TDD, gated) → Fable adversarial review per branch.
