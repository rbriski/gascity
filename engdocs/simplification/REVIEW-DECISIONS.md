# Simplification Review — Decisions & Action Queue

_Live doc for the walkthrough (started 2026-07-07). Records Julian's decision + commentary per item during the one-by-one review. The **Action queue** at the bottom is what I execute after we finish all 34 items._

**PR strategy (confirmed):** **one PR per approved item**, follow-ups folded into that same PR, each PR → `gastownhall/gascity` `main`. Mechanics per item: branch off latest `origin/main`, cherry-pick the item's commit(s) onto that clean base (drops the proposal-docs commit), add follow-ups, gate, push, PR, then either label `status/needs-review-auto` (label-PR) or wait-for-green-and-merge (fast-track).

## Execution control
**EXECUTION STATUS: LIVE** — a background **executor** agent processes each Action-queue item marked QUEUED that it hasn't already logged: opens the PR, labels `status/needs-review-auto` (label-PR items) or waits for CI green then merges (fast-track items, respecting branch protection — never force). It logs progress to `EXECUTION-LOG.md`, processes items **sequentially**, and does NOT touch items tagged ⚠ LARGE/RISKY (those await Julian's go). Controls: set STATUS to `PAUSED` to halt; `DONE` when the walkthrough is complete (executor finishes pending non-risky items, then exits). The executor only READS this file; it writes only to `EXECUTION-LOG.md`.

## Decision legend
- **LAND** — cherry-pick the commit(s) onto the target branch as-is
- **LAND+FU** — cherry-pick, and I file the noted follow-ups (as beads/notes)
- **CHANGES** — needs rework before landing; I make the noted edits in the branch, re-gate, re-present
- **HOLD** — keep the branch, decide later (not landing this pass)
- **SKIP** — don't land; branch stays for reference (delete only if told)
- **PICK `<key>`** — (competing sets) choose that approach; the losers → HOLD unless told to SKIP

---

## Item log

### 1 · S16 — Surface the seven swallowed errors on destructive/routing paths  (`simplify/s16`, `856f0d5a1`)
- **Decision:** LAND via PR — complete all 3 follow-ups first, then open PR labeled `status/needs-review-auto`.
- **Comments:** Julian: do the followups, then PR. (F1 add direct test for reconciler skipped_liveness_error; F2 extend the liveness-error fail-closed gate to the sibling destructive paths — failed-create close + drain-ack finalize + pending-create rollback — not just fix the comment; F3 distinguish beads.ErrNotFound in needsConvoyRecovery.)
- **Actions:** QUEUED — execute at end of walkthrough (see Action queue › A1).

### 2 · S20 — Unknown-state signal + escalation for the reconciler  (`simplify/s20-a`, `2dc0915a3`)
- **Decision:** APPROVED — land, with the follow-up folded into the same PR (no separate follow-up PR).
- **Comments:** Julian: "do the followup in the same pr, approved." Follow-up = clear the `unknown_state_first_seen` marker when a session re-enters a known state (so a recurrence isn't silently suppressed) + file the slice-b bead for the actual typed `SessionState` enum. NOTE as-shipped this is the signal/escalation slice (adds a `session.unknown_state` event + durable marker + 30m escalation; touches the typed-event/openapi wire surface); the titular enum simplification is deferred to slice-b.
- **Actions:** QUEUED — execute at end (see Action queue › A2).

### 3 · S35 — Decision record: don't unify internal/convergence with cmd/gc session_reconciler  (`simplify/s35`, `02d4f8a34`)
- **Decision:** SKIP — not landing. Branch kept for reference.
- **Comments:** Julian: "skip." (Docs-only negative-result note + convergence-idiom blueprint; zero risk, but not landing the directional "we won't unify" record for now.)
- **Actions:** none (no PR).

### 4 · S23 — Finish the Info front-door migration  (`simplify/s23-a`, `998be6290`)
- **Decision:** ~~APPROVED + implement Phase 2 & 3 in the same PR~~ → **REVISED to (1)** once S19 became the reconciler-rewrite umbrella: **land Phase 1 alone** as a label-PR (safe, behavior-preserving front-door + guard, mirror retained); **fold Phase 2** (mirror removal, with S10 read-purity) **& Phase 3** (god-file split = S25) **into the S19 roadmap** (parity-pinned stages). Avoids colliding with the live S19/S36 reconciler work.
- **Comments:** Julian: "implement 2 & 3 in same pr." Phase 2 = delete the ~26 raw/Info predicate pairs, collapsing to the single fold path (⚠ this REMOVES the raw-bead mirror Phase 1 kept — the behavior-sensitive step). Phase 3 = split the god files (`session_reconciler.go` ~4900 LOC, `session_reconcile.go`, `build_desired_state.go`) along their trace-phase seams. ⚠ Large, higher-risk PR — surface to Julian before pushing.
- **Actions:** ✅ Phase-1 landed — **PR #4045** (`status/needs-review-auto`; clean 3-way cherry-pick onto advanced main, no manual conflicts; 3 files +279/−56, 44 tick-routed fold sites + `reconcile_tick.go`/`_test.go`; gates + full pre-push suite green). Phases 2 & 3 → S19 roadmap (amend `specs/S19-...md` §6). Supersedes A3.

### 5 · S12 — Legacy-state canonicalizer + shared list filter (dedup)  (`simplify/s12`, `24cf16fbe` +1)
- **Decision:** LAND — **fast-track merge**: PR → wait for CI green → merge directly. **No** `status/needs-review-auto` label (already Fable-reviewed, clean −16-line dedup).
- **Comments:** Julian: "land w/o full label review, just merge after green since already fable reviewed."
- **Actions:** QUEUED — execute at end (see Action queue › A4).

### 6 · S13 — Collapse three formula-attachment pipelines into one  (`simplify/s13`, `79a7c2a54`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label) **+ fold in the dead-branch deletion** (the reviewer-flagged dead `isGraph && opts.Force` branch). Delete PR branch on merge.
- **Comments:** Julian: "we fast track with branch deletion." (Kills #1053 duplicate-molecule vector; behavior-preserving.)
- **Actions:** QUEUED — execute at end (see Action queue › A5).

### 7 · S37 — Single scope-close/abort reconciler in dispatch  (`simplify/s37`, `4b7d6e7cf`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete PR branch on merge.
- **Comments:** Julian: "fast track." Byte-matched behavior-preserving dedup (3 close→1, 2 abort→1, 3 fanout epilogues→one-liners), independently verified.
- **Actions:** QUEUED — execute at end (see Action queue › A6).

### 8 · S32 — RetryHandler delegates to CreateHandler (fixes live trigger-config-loss)  (`simplify/s32`, `621720905` +1)
- **Decision:** LAND via **label PR** (`status/needs-review-auto`) — behavior change on the retry/create control path warrants the auto-review pass (not a silent fast-track).
- **Comments:** Julian: "label PR." ⚠ Behavior change (intended fix): retried beads with trigger config now respect the trigger instead of dropping it. Optional nits (NOT folded — leave for auto-review): derive `RetryResult.Iteration`/`FirstWispID` from actual create outcome; wrap CreateHandler validation errors with source bead ID.
- **Actions:** QUEUED — execute at end (see Action queue › A7).

### 9 · S04 — Move the 700-line shell-codegen block out of config.go  (`simplify/s04-a`, `202e6fd79`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Verbatim same-package file move, zero behavior risk (cx↓=no — navigability only). Left as future follow-ups (NOT folded): approach-b table-driven `Effective*Query` collapse (the real win); rehome the stray session-capacity Agent helpers out of `workquery.go`.
- **Actions:** QUEUED — execute at end (see Action queue › A8).

### 10 · S06 — Collapse config accessor boilerplate (generic cachedPackField + durationOr)  (`simplify/s06`, `7dda6206a`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Real reduction (~250 production lines deleted, net −165), behavior-preserving, gates verified. Trivial nit NOT folded per Julian: move the stranded `SetupTimeoutDuration` doc comment onto its method (cosmetic; gates already green).
- **Actions:** QUEUED — execute at end (see Action queue › A9).

### 11 · S03 — Bounded recency-query capability for the beads store  (`simplify/s03`, `b42f8ab09`)
- **Decision:** SKIP — not landing. Branch kept for reference.
- **Comments:** Julian: "skip." Typed enabler only — dead code until the #3253 consumer rewrite lands; no runtime benefit solo. Revisit if/when doing the #3253 feed-scan O(limit) rewrite.
- **Actions:** none (no PR).

### 12 · S07 — Guardrail note + relocate legacy import block  (`simplify/s07`, `e7879dcb2`)
- **Decision:** SKIP — not landing. Branch kept for reference.
- **Comments:** Julian: "skip." Guardrail note ("leave AgentDefaults triple-merge + legacy shim alone") + cosmetic legacy-block file move (cx↓=no). Deferral bead for the pack.go god-function split NOT filed.
- **Actions:** none (no PR).

### 13 · S05 — Unify Agent patch/override merge + deep-clone  (`simplify/s05-a`, `a72dd5d53`)
- **Decision:** LAND via **label PR** (`status/needs-review-auto`) — field-sync-sensitive area + a Clone-semantics fix warrant the auto-review.
- **Comments:** Julian: "label-PR." Genuine −109 reduction (2 merge + 2 clone bodies → 1 each); FIXES a latent tombstone-aliasing bug in `Clone`; replaces the manual field-sync convention with a reflect-based `TestAgentCloneIsDeep` (stronger guard). Touches the `config.Agent` field-sync zone (AgentPatch/merge/Clone/poolAgents, `cmd/gc/pool.go`, `AGENTS.md`). Nit NOT folded: drop/comment the dead tombstone copies in `toAgentPatch`.
- **Actions:** QUEUED — execute at end (see Action queue › A10).

### 14 · S09 — Kill stringly-typed session/dispatch metadata (constants slice)  (`simplify/s09-a`, `d00260ce9`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Byte-identical typed-constant unification (SleepReason + attachment keys); the table-driven Info codec (the big piece) is deferred. Follow-ups NOT folded: migrate `cmd/gc` parallel sleep-reason literals (~6 files); the deferred Part-1 Info codec.
- **Actions:** QUEUED — execute at end (see Action queue › A11).

### 15 · S26 — Collapse the trace double-record API to one typed surface  (`simplify/s26-a`, `1df389949`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Real −151-line reduction in the trace subsystem (typed `TraceSiteCode`/`ReasonCode`/`OutcomeCode`, deletes the normalize allowlists + a file); diagnostic layer, low blast radius; behavior-preserving.
- **Actions:** QUEUED — execute at end (see Action queue › A12).

### 16 · S11 — Collapse session.Manager telescoping Create API  (`simplify/s11`, `3254b80e1`)
- **Decision:** LAND via **label PR** (`status/needs-review-auto`) **+ FOLD IN the wrapper/preset deletion** (so the surface actually shrinks, not grows).
- **Comments:** Julian: "include the deletion in the same PR and label review." ⚠ Worker-boundary-sensitive (`TestGCNonTestFilesStayOnWorkerBoundary` must stay green). Also fold nits: `CreateSpec` doc-comment typo, `AGENTS.md` worker-boundary note.
- **Actions:** ⚠ EXECUTOR REFUSED (correct judgment) — cherry-pick was clean, but the folded wrapper-deletion is a **~440-call-site / ~270-file** repoint on the worker-boundary surface (the decision underestimated the base-method fanout) with high silent-transposition risk → too big/risky for one safe reviewable fold. **NEEDS RESCOPE:** land the base `CreateSpec`/`CreateSession` addition as its own label-PR, and phase the wrapper deletion in batches (or mark the 9 `Create*`/5 `NewManager*` `Deprecated` first). → **(2)+DO-IT: DISPATCHED `delete-S11` pipeline.** Opus completes the FULL deletion (base `CreateSpec` + delete 9 `Create*`/5 `NewManager*`, ~440 sites/270 files — compiler-guided, so `go build ./...` green guarantees coverage; only real risk = arg-transposition) → **Fable RED-TEAM** (4 adversarial angles: arg-transposition, worker-boundary invariant, defaults-preservation, coverage/field-sync) → synthesize → ✅ **SHIPPED PR #4047**. 9 `Create*` wrappers deleted, **534 sites repointed across 37 files** (compiler-verified 0 residual refs — the ~270-file estimate was a big overcount), worker-boundary + field-sync invariants pass. **Red-team: all 4 angles = nits (none blocking), `transposition_safe=true`** → approve-with-nits. Base + deletion landed together.

### 17 · S18 — Dedup triple-pasted routed-state warning block  (`simplify/s18`, `9d2f5e33c`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Byte-identical dedup (−24), no wire/event surface, new table test locks behavior.
- **Actions:** QUEUED — execute at end (see Action queue › A14).

### 18 · S31 — One childStats() scan replacing six duplicate loops  (`simplify/s31`, `a52aab358`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Real dedup (6 loops + 2 helpers → 1 pure `childStats`); behavior-preserving (filter-drift verbatim, fetch consolidation verified safe); perf win (3–4 `Children()` round-trips → 1/transition); new tests.
- **Actions:** QUEUED — execute at end (see Action queue › A15).

### 19 · S33 — Flatten reconcile.go error plumbing  (`simplify/s33`, `87f7c7b53`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Net −87, typed `ReconcileAction` set + single wrap site, zero behavior change. (Same `internal/convergence` cluster as S31/S30 — executor may hit cherry-pick/merge conflicts; resolve or log.)
- **Actions:** QUEUED — execute at end (see Action queue › A16).

### 20 · S30 — Convergence commit-point helper (crash-safe write ordering)  (`simplify/s30-a`, `ff936b715`)
- **Decision:** LAND — **fast-track merge** (PR → CI green → merge, no label). Delete branch on merge.
- **Comments:** Julian: "fast track." Centralizes marker-last write ordering (crash-safety contract), zero interface change, behavior-preserving. Doc-overclaim nit ("type-impossible" → actually convention-enforced) NOT folded. Same `internal/convergence` cluster conflict caveat as S31/S33.
- **Actions:** QUEUED — execute at end (see Action queue › A17).

### 21 · S24 — Pool satisfaction as a level + ghost-session healer ⟨3 approaches⟩  (needs-julian)
- **Decision:** PICK approach **a** (ledger healer, `simplify/s24-a` `c508bfbca`) — **HOLD** (don't productionize now; keep the spike branch for a dedicated later session). Approaches **b** (`simplify/s24-b`) and **c** (`simplify/s24-c`) not pursued.
- **Comments:** Julian: "a hold." `a` = the real #3872 ghost-session-starvation fix (heals the source of truth by closing the runtime-dead bead). When resumed: wire `a` in, hoist the per-session `ListRunning` check to once/tick, rate-limit the #2285 diagnostic, and DELETE the superseded demand-decider paths (that deletion is the actual complexity win).
- **Actions:** none now (HOLD). Future work: graduate `simplify/s24-a` → real implementation + label-PR.

### 22 · S19 — Level-triggered session convergence ⟨3 approaches⟩  (needs-julian)
- **Decision:** BUILD via **FULL PIPELINE, STARTED NOW** (Fable spec synthesizing the 3 spikes → Opus build of **stage 1 only** → Fable review → label-PR). Supersedes the earlier "dedicated session" hold — Julian: "start the same fable/opus/fable process for that big reconciler change to get it started."
- **Comments:** Reviewer roadmap the spec must follow: adopt **b**'s observe→diff→act shape (`s19-b`) as the target, using **a**'s pure identity/priming functions (`s19-a`) as building blocks, with **c** (`s19-c`) as the Layer-1 end-state. ⚠ MUST-FIX: `b`'s stamp-vs-nudge ordering — a crash between stamping the prime-marker and delivering the nudge can permanently lose the startup prompt (redefine as `priming_attempted` + delivery confirmation). Build ships STAGE 1 (must keep the full reconciler suite green, legacy-parity pin); remaining stages documented. Label-PR only, never auto-merge.
- **Actions:** ✅ STAGE 1 DONE — **PR #4034** (`status/needs-review-auto`). Spec = a **7-stage migration** (`specs/S19-level-triggered-convergence-spec.md`); Stage 1 = pure observe→diff→act cores behind the legacy-parity pin (dormant, zero behavior risk). Gates green incl. reconciler suite + parity-pin + worker-boundary/field-sync guards; review approve-with-nits, behavior preserved. **Remaining Stages 2–7** (durable canonical-identity schema → shadow-mode deriveConvergeActions → activate the 4 bug fixes #3849/#3872/#2073 → read cutover → the −300/−400-LOC deletion payoff → move to internal/session). S10 & S25 fold into this roadmap (see their entries). → **Stage 1 MERGED (#4034)** → **Stage 2 DISPATCHED** (`graduate-S19s2`, builds on merged main): land `canonical_identity.go` + `Info.CanonicalIdentity` projection + stamp `canonical_instance_name`/`canonical_pool_slot` at create/adoption + fold `primed_at`/`prompt_hash` into `CommitStartedPatch` — WRITE-ONLY, dormant, behavior-preserving. → Stage 2 BUILT (write-only claim holds, behavior-preserving, gates + reconciler pin + invariant guards green) but review=**needs-rework**: the spec's clear-site inventory MISSED a 7th `started_config_hash` clear site (`RestartRequestPatch` in `lifecycle_transition.go`) — inert now (write-only) but a latent bug once Stage 4 activates readers; also update 2 `TestHealStatePatch` want-maps + `make test-cmd-gc-process-parallel` green + rebase. **Rework DISPATCHED** (`rework-S19s2`) → ✅ **PR #4064** (approve-with-nits, write-only confirmed, all 5 gates + `-race` green). Added the 7th clear site (`RestartRequestPatch`) + a repo-wide AST **lifetime-rule gate** (`TestEveryStartedConfigHashClearAlsoClearsPriming`, catches all 7 sites) + adoption/recovery tests; rebased clean. **NEXT: Stage 3** (build durableFacts/runtimeFacts in reconciler Phase 1 + run `deriveConvergeActions` in SHADOW mode vs legacy per-path stamping until zero divergence) — actually restructures the reconciler → **needs Julian's review before go.** → Stage-3 plan drafted + hardened by a 5-angle Fable RED-TEAM council (all 5 found blocking gaps in v0: trace-blind comparator, phantom "routing" enabled action, unsafe primed_at heal, unreachable per-tick equality, dormant-poisoned-records-on-revert — all resolved in `specs/S19-stage3-plan.md` v1). **3 decisions pending Julian: Q1 (priming shadow probe vs exclude), Q2 (kill-switch flag substrate — needs a written spec-D7 amendment), Q3 (divergence event type).** Stage-3 impl may start once Q1/Q2 answered.

### 23 · S22 — Convergence sweep for ownerless coordination beads ⟨3 approaches⟩  (needs-julian)
- **Decision:** PICK approach **a** (full sweep-and-reap, `simplify/s22-a` `8feedee0f`) — **HOLD** (don't productionize now). Approaches **b** (`simplify/s22-b`), **c** (`simplify/s22-c`) not pursued.
- **Comments:** Julian: "a, hold." `a` = the real #3407 fix (actually **closes** abandoned roots). When resumed: fold the 4 consumer rewirings into the same PR; audit all control-bead close orderings for the transient all-closed window (add a second-read confirm / dependency-aware check in the sweep); fold the disposition stamp into the close (`updateMetadataAndClose`-style) + `gc.outcome`, tested against a strict store.
- **Actions:** none now (HOLD). Future work: graduate `simplify/s22-a`.

### 24 · S21 — One typed relevance predicate for wake + claim ⟨3 approaches⟩  (needs-julian)
- **Decision:** HOLD ALL — no approach picked. Needs a proper **analysis + spec of BOTH sides** (wake-side filter AND claim-side query, unified behind one predicate). The three spikes explored only the wake side; none did the claim-side conversion, so as-built they add code without deleting the duplicate. Keep `simplify/s21-a/b/c` as evidence.
- **Comments:** Julian: "hold all, we need a proper analysis and spec of both sides." For the spec: `c` (subscription-boundary `WatchFilter`) is the cleanest wake shape, but the real win requires converting the jq/shell claim query (`workflowServeControlReadyQueryForBeads`) to a typed store query on the SAME predicate. Also resolve: self-actor identity (stamped recorded-by vs env `eventActor()`), the dependency-close wake question, and Dolt-contention of a persistent store handle.
- **Actions:** none now (HOLD → needs spec of both sides).

### 25 · S34 — Rebuild convergence scopes on reload; delete the #2403 taxonomy ⟨2 approaches⟩  (needs-julian)
- **Decision:** PICK approach **b** (wholesale re-derive, `simplify/s34-b` `cc27f4188`) — LAND via **label PR** (`status/needs-review-auto`). Approach **a** (`simplify/s34-a`) not pursued.
- **Comments:** Julian: "b, label pr." Genuine simplification (net −43; deletes the whole #2403 detect-and-punt taxonomy + 4 restart-controller branches; `convergenceScopeForRig` → map lookup + #2357 fail-loud). Optional nits NOT folded (leave for auto-review): add a rebound-rig test; one-line comment on the nil-activeIndex window.
- **Actions:** QUEUED — execute at end (see Action queue › A18).

---
## §C proposal-only items (unbuilt — build / spec / hold / skip)

### 26 · S29 — Retire the three legacy dual-mode machineries  (PROPOSAL-ONLY, needs-julian, risk med / effort L)
- **Decision:** SPEC-FIRST — write a proper spec before building. Define the **proof-of-deadness** per legacy mode (per-tick migration scans; `store==nil` desired-state mode; v1 check/retry-eval engine) + the staged retirement, THEN build. Not building blind (L effort × med risk × hot-path deletions).
- **Comments:** Julian: "spec first." Biggest deletion on the board (~1,250 src + ~2,000 test LOC from `internal/dispatch`; `ralph.go` 1381→~450, `retry.go` 637→~300). Builds directly on S32 (already landing).
- **Actions:** SPEC — author the S29 retirement spec (deadness proof + staging). Deferred; Fable design, batched with other spec work.

### 27 · S08 — Wire-or-delete sweep across four subsystems  (PROPOSAL-ONLY, needs-julian, risk med / effort L)
- **Decision:** HOLD (whole). Keep in backlog; revisit after S26 (trace) lands and the S29 spec exists — S08 overlaps both (Step-0 trace deletes overlap S26; the dead-machinery theme overlaps S29).
- **Comments:** Julian: "hold whole." Step-0 pure deletes (safe) + wire-or-delete judgment across lifecycle-fold / wake / trace / convergence; ~940 prod + ~800 test LOC deletable.
- **Actions:** none now (HOLD).

### 28 · S28 — Typed PendingCreateLease protocol  (PROPOSAL-ONLY → FULL PIPELINE)
- **Decision:** BUILD via **FULL PIPELINE** — Fable spec → Opus build → Fable-workflow review → label-PR (`status/needs-review-auto`). Orchestrated separately (NOT the executor's cherry-pick queue).
- **Comments:** Julian: "full spec, build, and label review — Fable for design, Opus for implementation, Fable workflow for review." Typed lease over the async-start optimistic-concurrency (4 stringly keys + 3 boolean helpers → 1 typed lease); pending-create bug family (#1542/#2073/#2895/#3849). ⚠ concurrency-semantics-sensitive — review MUST verify transition equivalence.
- **Actions:** ✅ DONE — **PR #4024** (labeled `status/needs-review-auto`, not merged). Spec written; Opus built; Fable review **approve-with-nits, semantics preserved**; gates green (build/vet + all `TestAsyncStart*`/pending-create session tests + `TestGCNonTestFilesStayOnWorkerBoundary`).

### 29 · S01 — Beads cache: absorb/evict primitives + one merge  (PROPOSAL-ONLY → FULL PIPELINE, ⚠ HIGH risk)
- **Decision:** BUILD via **FULL PIPELINE with MAX review** — Fable spec (nails lock-ordering + merge invariants) → Opus build (2 primitives, repoint ~15 sites, TDD + `-race`) → Fable review (prove byte-for-byte equivalence; default needs-rework if not provable) → label-PR. STARTED NOW.
- **Comments:** Julian: "same thing for this cache fix which we should do." ⚠ HIGHEST-risk item — the beads read cache; a merge/evict divergence silently serves stale/wrong beads fleet-wide (#2987 class). Label-PR only, NEVER auto-merge; review dialed to max.
- **Actions:** RELAUNCHED on the hardened pipeline (first run's spec stalled the 180s watchdog; incremental-write fix cleared it). ⚠ **NEEDS-REWORK (correctly withheld — highest-risk item, NOT shipped).** Phase 1 built (2 locked primitives + all ~15 map-mutation sites repointed); gates green incl. `-race`; the existing ~6.8k-LOC white-box corpus passes UNMODIFIED = the byte-identical parity proof; + new primitives test. Max-scrutiny review said do NOT land as "S01 full": (1) migrate `refreshGraphAppliedBeads` onto `absorbFreshLocked` so the exclusivity contract holds; (2) add T7 (seqKeep guard) + T6 (ordering) tests; (3) rescope to "Phase 1: primitives" (landable after 1+2); (4) rebase onto `origin/main` (S26 landed). **Phase 2 (`runReconciliation` collapse) deferred → needs Julian's D2 sign-off.** Site-by-site notes: `specs/S01-cache-primitives-spec-review-notes.md`. → **(1) DISPATCHED** — `rework-S01` pipeline: Opus applies the punch-list (migrate `refreshGraphAppliedBeads`, add T6/T7, rebase onto `origin/main`, keep it "Phase 1: primitives", corpus must stay unmodified) → Fable max-scrutiny review → ✅ **PR #4046** (approve-with-nits; `refreshGraphAppliedBeads` migrated + T6/T7 added; rebased; corpus passes UNMODIFIED = byte-identical, `-race` clean, full pre-push suite green). **Phase 2 (`runReconciliation` collapse) held for Julian's D2 sign-off.**

### 30 · S10 — Make read/plan paths pure (Observe/Plan/Materialize)  (PROPOSAL-ONLY, needs-julian)
- **Decision:** FOLD INTO S19 — the S19 migration absorbs S10 as an explicit STAGE: split `infoFromBead` into `InfoFromPersistedBead` (pure) + `ObserveRuntimeForInfo`, move `routeACPIfNeeded` off the read path, delete the `BaseState`/overlay dual representation (~60 LOC) + 4 mid-tick rebuild sites.
- **Comments:** Julian: "fold into." Same edge-triggered thesis as S19 on the read/desired-state surface — a separate build would collide with the live S19 build in `manager.go`/`build_desired_state.go`. ⚠ graduate-S19 is already running; VERIFY its spec captures S10, else add as a documented S19 stage when it reports.
- **Actions:** FOLD — capture in the S19 spec stages (check when `graduate-S19` reports). No separate pipeline.

### 31 · S02 — Per-bead dirty overlay  (PROPOSAL-ONLY, needs-julian)
- **Decision:** FOLD INTO S01 (as **stage 2**) — per-bead dirty overlay built on S01's `absorbFreshLocked`: cached reads refresh only the dirty IDs via bounded `backing.Get`, serve the rest from cache, absorb+clear on `Get`. Replaces the global-cache-decline tripwire duplicated at 6 read sites.
- **Comments:** Julian: "fold into s01." Depends on S01's `absorbFreshLocked` (building now). Turns the biggest #2987 pain (one dirty bead → O(active-set) backing `List` per read) into a bounded per-bead refresh. ⚠ When `graduate-S01` reports, add S02 as stage 2 / follow-up PR on the S01 base.
- **Actions:** FOLD — S01 stage 2 (after S01 lands). No separate pipeline.

### 32 · S15 — Kill stringly routing duplication (one typed Router)  (PROPOSAL-ONLY, needs-julian, ⚠ HIGH risk)
- **Decision:** SPLIT — **(a)** claim-side (control beads claimed by KIND, not route string) MERGES into the S21 both-sides analysis+spec; **(b)** the work-bead Router half → **SPEC-FIRST** (own scoped spec, given HIGH misroute risk), then decide build.
- **Comments:** Julian: "merge into S21 then spec work router." Work-Router win: typed `Router` in `internal/sling` with validated pool targets, batch of N goes N subprocesses → N typed writes, deletes the 4 Router-vs-Runner forks. Claim-side overlaps S21 / #1938 / #3964 / #3892. HIGH risk = a misroute strands work (#2891 clobbers-routes class).
- **Actions:** SPEC — (a) S21 spec absorbs the claim-side; (b) author a work-bead-Router spec. Both deferred to the spec batch.

### 33 · S14 — One launchWorkflow chokepoint  (PROPOSAL-ONLY → FULL PIPELINE)
- **Decision:** BUILD via **FULL PIPELINE** — Fable spec → Opus build → Fable review → label-PR. STARTED.
- **Comments:** Julian: "full pipeline." Single `launchWorkflow()` chokepoint: 3 partial dedupe guards → 1, 2–4 compiles → 1/launch; closes #1053 + #720. Sibling of S13 (landing). ⚠ Medium risk: the one guard must cover every launch shape without blocking legitimate launches.
- **Actions:** ✅ DONE — **PR #4037** (`status/needs-review-auto`). Phase 1: compile-once (shape A) + the **#1053 fix** — shared `InstantiateCompiledSlingFormula` chokepoint with a cross-process `RootKey` file-lock (replaces the process-local mutex) covering ALL launch shapes; 4 TDD tests (dup→idempotent single root; cross-proc lock; legit launches/#720 never blocked; compile-once). Gates + pre-push fast suite green; review approve-with-nits, behavior preserved. Remaining Phases 2–4 (named `launchWorkflow()` adapters; thread compile-once into --on/default/batch; unify source+root lock, delete `graphv2.LockKey`; cross-proc integration test) deferred.

### 34 · S36 — Route drift-Relaunch through buildPreparedStart  (PROPOSAL-ONLY → FULL PIPELINE)
- **Decision:** BUILD via **FULL PIPELINE** (standalone). STARTED.
- **Comments:** Julian: "full pipeline." #3872 drift-relaunch misconfig fix — use the proven `buildPreparedStart` derivation (as `recoverRunningPendingCreate` does) instead of the fingerprinting config; 2 launch derivations → 1. ⚠ Touches `session_reconciler.go`/`session_lifecycle_parallel.go` — SAME files as the live S19 build; both are label-PRs, so resolve at merge (whichever merges 2nd rebases).
- **Actions:** ✅ DONE — **PR #4038** (`status/needs-review-auto`). Full S36 (Phase 1 + Phase 2 cutover) in one commit; drift-relaunch now derives its exec config via `buildPreparedStart` (Command gets `--resume`/`--session-id`/fork), hashes rebaselined (no re-drift loop); 2 TDD tests. Gates + reconciler suite green; review approve-with-nits, behavior preserved. Follow-up noted: `session_reconciler.go:4202` (asleep-named repair) may share the config-confusion — separate change.

### 35 · S38 — Durable gc.control_for lineage (retire findLatestAttempt ref-string surgery)  (PROPOSAL-ONLY → FULL PIPELINE)
- **Decision:** BUILD via **FULL PIPELINE** (hardened). STARTED.
- **Comments:** Julian: "s38 full pipeline." Replace the 4-stage ref-string surgery in `latestAttemptFromCandidates` with the durable `gc.control_for` metadata that ALREADY exists (`ControlForMetadataKey`, used by fanout/scope-check; `ralph.go` rewrites it). Deletes ~80 LOC of the densest dispatch code. ⚠ Medium risk: spec must prove the stamp is on EVERY attempt path + handle legacy attempts (one-release fallback). Same `control.go` engine as S32 (landing) / S29 (spec).
- **Actions:** ⚠ NEEDS REBASE (content correct) — build green incl. `-race`, review behavior_preserved=true, but verdict=needs-rework ONLY because `origin/main` advanced under it (fast-track merges S12/S13/S37/S04/S09 landed while S38 built off older main → two-dot diff shows spurious reverts; S37/S13 conflict in dispatch files). No semantic change needed. ✅ DONE — **PR #4044** (rebased clean onto `origin/main` — the S37/S13 conflict didn't materialize at file level; commit `fa86baba6` byte-identical in effect, 7 files +505/−6; gates green incl. `-race` + the full pre-push fast suite; labeled `status/needs-review-auto`). Phase 4 (delete ~78 LOC legacy) deferred (gated on 1 release + `legacyAttemptLineageHits`==0).

### 36 · S27 — Downgrade WAL-grade trace store to rotating JSONL  (PROPOSAL-ONLY, needs-julian)
- **Decision:** SKIP — not building. Trace-store durability downgrade (~400 LOC of CRC/head.json/quarantine for best-effort forensic data). Keep in backlog if trace startup cost ever bites; overlaps S26 (landing) / S08 (held).
- **Comments:** Julian: "skip."
- **Actions:** none (no PR).

### 37 · S25 — Decompose the reconciler mega-function  (PROPOSAL-ONLY, needs-julian)
- **Decision:** FOLD INTO S19 — the byte-identical `reconcileTick` decomposition (2,188-line `reconcileSessionBeadsTracedWithNamedDemand` + 540-line `buildDesiredStateWithSessionBeads` → `reconcileTick` struct + one method per `recordPhase` seam; trace output byte-identical / golden-diffable) becomes an explicit EARLY stage of the S19 roadmap (before the Stage-3 Phase-1 restructuring).
- **Comments:** Julian: "fold into s19." Same `session_reconciler.go` territory as S19 (Stage 1 landed #4034) + S23 Phase 3 (held). ⚠ Amend the S19 spec (`specs/S19-...md` §6) to insert S25 as the mechanical-decomposition stage when the S19 effort continues.
- **Actions:** FOLD — add to the S19 spec stages. No separate pipeline.

### 38 · S17 — Kill hardcoded formula-name matching (formula-declared vars)  (PROPOSAL-ONLY → FULL PIPELINE)
- **Decision:** BUILD via **FULL PIPELINE** (hardened). STARTED.
- **Comments:** Julian: "full pipeline." **ZERO-hardcoded-roles INVARIANT FIX** — removes pack vocabulary (`polecat`/`refinery`/`scoped-work`) from generic Go; injects `base_branch`/`target_branch` by DECLARED var, not name-match; deletes 2 exported fns + name lists. ⚠ KEY RISK: every formula that got injection via name-match must DECLARE the var (pack-config change) or it silently loses injection — if the packs live outside this repo, the Go change must NOT land alone. Review verifies var-parity for every previously-matched formula.
- **Actions:** ⛔ WITHHELD (correct call) — spec written (`specs/S17-formula-declared-branch-vars-spec.md`), but the Go cutover is UNLANDABLE from this repo: the name-matched formulas (`mol-polecat-work`/`-arm`/`*`, `mol-refinery-patrol`) live in the EXTERNAL gastown pack, so deleting the matchers would silently drop branch injection from live formulas. Pipeline refused to ship (pushed=false). **BLOCKED on Stage 0 (external, cross-repo):** the deployed gastown pack must first declare `[vars.base_branch]`/`[vars.target_branch]` on those formulas + advance city pins; THEN land the Go cutover here (`formula.DeclaredVars` + swap the checks at `sling.go:1093-1099` + migrate ~20 bare-name tests + deletion-sweep lint). Needs cross-repo coordination.

---

## §D — Unblocked follow-on PRs (post-merge, dispatched)
Julian: "create new PRs for the unblocked ones with label status/needs-review-auto." Follow-ons of MERGED items, buildable on current `main`, via graduate-hard pipeline → label-PR:
- **S04b** — table-driven `Effective*Query` resolver + rehome Agent helpers (follow-on to merged S04 #4030). → ✅ **PR #4060** (approve-with-nits; byte-identity proven — zero golden churn, existing tests unedited; `-race` green).
- **S09b** — table-driven Info codec (⚠ parity-gated) + `cmd/gc` sleep-reason migration (follow-on to merged S09 #4033). → ✅ **PR #4062** (approve-with-nits; Info codec BYTE-IDENTICAL parity held, `-race` + pre-push suite green; molecule_id readers repointed). Build correctly LEFT non-sleep_reason vocabularies (`state`/drain-reason/wake-blocker/drain-ack/`sleep_intent`) as-is — the spec over-enumerated; converting them would mislabel. Future **S09c**: fold the 2 other projection maps.
- **S26b** — finish the typed trace surface: pool-cap rejection + remaining outcome strings (follow-on to merged S26 #4036). → ✅ **PR #4061** (approve-with-nits; behavior-preserving, `-race` green). Remaining: Group-C reason casts (candidate **S26c**) + 1 legitimate CLI-boundary dynamic conversion.
- **S08-step0** — PURE DELETES ONLY (dead trace symbols + `LegacyArms`); wire-or-delete JUDGMENT stays HELD. → ✅ **PR #4059** (approve, clean; behavior-preserved, `-race` green; compiled with only the enumerated deletions = no missed refs). Optional follow-up deferred: 2 sibling dead consts (`TraceEvaluationDependencyBlocked`/`StorePartial`).

## Action queue (executed after the full walkthrough)

### A1 · S16 → PR (label `status/needs-review-auto`)
1. Fetch `origin/main`; branch off it; cherry-pick the s16 code commit `856f0d5a1` (clean base — no proposal docs; already verified it cherry-picks clean onto main `3dad18341` and builds).
2. Implement the 3 follow-ups (Opus, TDD, gate): **F1** direct test for the reconciler `skipped_liveness_error` path; **F2** extend the liveness-error fail-closed gate to the sibling destructive paths (failed-create close, drain-ack finalize, pending-create rollback), per-site wedge-verified, and fix the misleading comment; **F3** distinguish `beads.ErrNotFound` in `needsConvoyRecovery` so a persistently-deleted parent still triggers recovery.
3. Fable adversarial wedge-safety review of the F2 destructive-path changes.
4. Push; open PR → `gastownhall/gascity` `main`; add label `status/needs-review-auto` (use `gh api .../issues/N/labels` — `gh pr edit --add-label` is broken on this repo).

### A2 · S20 → PR (label `status/needs-review-auto`)
1. Branch off latest `origin/main`; cherry-pick the s20 commit `2dc0915a3` (clean base).
2. Follow-up in the same PR: **clear the `unknown_state_first_seen` marker when a session re-enters a known state** (so a later recurrence of the same unknown value isn't silently suppressed); add a test. Also **file a bead for slice-b** (the actual typed `SessionState` enum) so the simplification isn't lost.
3. Gate (build/vet/tests; note it regenerates `openapi.json` — run the dashboard/openapi gate if the event surface changed).
4. Push; PR → `main`; label `status/needs-review-auto`.

### A3 · S23 → PR (label `status/needs-review-auto`) — LARGE/RISKY, confirm before push
1. Branch off latest `origin/main`; cherry-pick s23-a commit `998be6290` (Phase 1: tick-scoped fold front-door + source-scan guard).
2. **Phase 2:** delete the ~26 raw/Info predicate pairs, collapse to the single fold path — this removes the retained raw-bead mirror (behavior-sensitive; must prove coherence preserved).
3. **Phase 3:** split `session_reconciler.go` / `session_reconcile.go` / `build_desired_state.go` god files along their trace-phase seams (no logic change).
4. Widen `TestReconcileTickFoldFrontDoor` to package-wide scanning; consider retiring the `infoByID` alias via `apply()`'s return value.
5. Adversarial review focused on Phase-2 behavior preservation; **full reconciler suite must pass.** Surface the diff to Julian before pushing.
6. Push; PR → `main`; label `status/needs-review-auto`.

### A4 · S12 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick the s12 commits (`24cf16fbe` + its sibling — 2 commits).
2. Push; open PR → `main` (no label).
3. Wait for CI green; then merge directly. ⚠ If branch protection requires an approval/label to merge, surface to Julian rather than forcing.

### A5 · S13 → fast-track merge (NO auto-review label) + dead-branch cleanup
1. Branch off latest `origin/main`; cherry-pick s13 commit `79a7c2a54`.
2. Fold in: delete the dead `isGraph && opts.Force` `checkAttachments` branch inside `attachFormulaToBead`'s non-graph region (reviewer-flagged); re-gate.
3. Push; open PR → `main` (no label); wait CI green; merge; **delete the branch on merge**. ⚠ Same branch-protection caveat as A4.

### A6 · S37 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s37 commit `4b7d6e7cf`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A7 · S32 → PR (label `status/needs-review-auto`)
1. Branch off latest `origin/main`; cherry-pick the s32 commits (`621720905` + sibling — 2 commits).
2. Push; PR → `main`; label `status/needs-review-auto`. (Nits left for auto-review, not folded.)

### A8 · S04 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s04-a commit `202e6fd79`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A9 · S06 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s06 commit `7dda6206a`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4. (Doc-comment nit not folded.)

### A10 · S05 → PR (label `status/needs-review-auto`)
1. Branch off latest `origin/main`; cherry-pick s05-a commit `a72dd5d53`.
2. Push; PR → `main`; label `status/needs-review-auto`. (Dead-tombstone-copy nit left for auto-review, not folded.)

### A11 · S09 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s09-a commit `d00260ce9`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A12 · S26 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s26-a commit `1df389949`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A13 · S11 → PR (label `status/needs-review-auto`) + wrapper deletion folded in
1. Branch off latest `origin/main`; cherry-pick s11 commit `3254b80e1` (adds `CreateSpec`/`CreateSession`, retains 9 `Create*` wrappers + 5 `NewManager*` presets as one-liners).
2. Fold in the DELETION: remove the 9 `Create*` wrappers + 5 `NewManager*` presets, repointing EVERY caller to `CreateSpec`/`Manager.CreateSession` (grep callers repo-wide; keep the worker-boundary invariant — cmd/gc non-test files route through `internal/worker/handle.go`). Fix nits: `CreateSpec` doc typo; update the `AGENTS.md` worker-boundary note. If a caller can't be cleanly repointed, mark that wrapper `Deprecated` instead of deleting, and note it.
3. Gate: `go build ./...`; vet; `go test` internal/session + internal/worker + internal/api + the cmd/gc worker-boundary test (`TestGCNonTestFilesStayOnWorkerBoundary` MUST pass).
4. Push; PR → `main`; label `status/needs-review-auto`.

### A14 · S18 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s18 commit `9d2f5e33c`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A15 · S31 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s31 commit `a52aab358`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4.

### A16 · S33 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s33 commit `87f7c7b53`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4. (May conflict with S31/S30 in `internal/convergence` — resolve trivially or log FAILED.)

### A17 · S30 → fast-track merge (NO auto-review label)
1. Branch off latest `origin/main`; cherry-pick s30-a commit `ff936b715`.
2. Push; PR → `main` (no label); wait CI green; merge; delete branch. ⚠ Same branch-protection caveat as A4. (Convergence-cluster conflict possible with S31/S33.)

### A18 · S34 → PR (label `status/needs-review-auto`)
1. Branch off latest `origin/main`; cherry-pick s34-b commit `cc27f4188` (touches `cmd/gc/convergence_tick.go` — distinct from the S31/S33/S30 `internal/convergence` files, so low conflict risk).
2. Push; PR → `main`; label `status/needs-review-auto`. (Optional nits left for auto-review, not folded.)
