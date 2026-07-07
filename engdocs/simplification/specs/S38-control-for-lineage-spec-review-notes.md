# S38 review notes — simplify-land/s38-full

## Verdict: NEEDS-REWORK (mechanical, not semantic)
The S38 content itself is sound — every attempt/iteration mint path is
stamped, the legacy cascade is preserved verbatim as a counted fallback, both
clone variants remap bead-ID pointers, and all affected suites pass under
-race. The rework driver is landing safety, not code correctness: the branch
is based 5 commits behind origin/main, so the requested
origin/main..branch diff REVERTS S09/S04/S37/S13/S12 (workquery split,
beadmeta dispatch keys, sleep_reason vocabulary, sling attachment collapse,
scope reconciler). Rebase onto origin/main (S37/S13 touch the same dispatch
files — conflicts likely mechanical but must be re-verified), re-run the
dispatch/formula suites, then this is approvable. Secondary: the spec's I1
deletion gate (full-corpus shadow parity) is only a single narrow fixture —
must be strengthened before Phase 4 deletes the cascade, not before this
lands.

## Branch hygiene
- Branch base = 3dad18341, which is 5 commits BEHIND origin/main (missing S09,
  S04, S37, S13, S12). `git diff origin/main..simplify-land/s38-full` therefore
  shows reversions of unrelated landed work (workquery.go split, beadmeta
  dispatch keys, sleep_reason, sling attachment collapse, scope reconciler).
  Merging this branch as-is via a plain merge would be fine (merge-base diff is
  clean 7 files), but any squash/rebase-land tooling that applies the two-dot
  diff would REVERT those five simplifications. Needs rebase onto origin/main
  before landing. Real S38 diff (merge-base..branch):
  control.go, control_for_lineage_test.go, ralph.go, formula/ralph.go+test,
  formula/retry.go+test. 505 insertions / 6 deletions.

## Findings (running)

### Verified sound
- W1-W5 stamps present and correctly placed: expandRetry (attempt.1),
  expandRalph simple (iteration.1), expandNestedRalph (scope root only, body
  children unstamped), buildAttemptRecipe rootMeta (written after the
  step.Metadata copy loop so authored values cannot shadow — test covers it),
  buildNestedControlSeed free via W4 (synthetic control.ID = namespaced ref,
  covered by identity set).
- Read side: controlIdentitySet = {control.ID, gc.step_ref, gc.step_id},
  equality match, infrastructure-kind + molecule_failed skips retained,
  max-attempt `>` tie-break preserved, empty → legacy fallback (verbatim old
  body) → same ErrControlGraphMalformed quarantine on total miss.
  latestAttemptFromDependencies untouched and inherits the new matcher. I5 ok.
- KindScope is NOT in ControlKinds → primary path can select ralph iteration
  scope roots (checked kindsets.go). The dropped scope-unless-ralph skip is
  safe on the primary path: only a ralph's own scope roots carry its identity.
  Primary is actually MORE deterministic than legacy for nested ralph: legacy
  stage-1 prefix also matched body children (candidate-order-dependent at
  equal gc.attempt); primary matches only the stamped scope root.
- Pre-existing gc.control_for population: enumerated ALL writers at the branch
  commit — control.go:858 (KindFanout), control.go:928 (KindScopeCheck),
  formula/graph.go:40 (KindFanout), :84 (KindScopeCheck), fragment.go:228
  (KindScopeCheck). All control kinds → excluded by the retained
  infrastructure skip. I2/risk-3 holds.
- W6 (appendRalphRetryLegacy): post-create SetMetadata remap keyed off the OLD
  bead's control_for through the old→new mapping; step-ref values are not
  mapping keys → untouched (still string-rewritten at clone time). Also remaps
  prevCheck. Mirrors the existing logical_bead_id remap exactly.
- W7 (buildRalphRetryGraphNode): bead-ID-valued control_for in attemptIDs →
  moved to MetadataRefs, key = old bead ID = plan node Key; verified the
  native applier substitutes keyToID[refKey] generically for arbitrary
  metadata keys (native_dolt_store.go:348-368, plus second path ~:1320).
  attemptIDs covers the whole attempt set + prevCheck, so a cloned nested
  control's old ID is always a plan key → no empty-substitution hole.
- rewriteRetryControlFor passes bead IDs through unchanged (verified:
  rewriteRetryStepRef prefix checks + rewriteRalphAttemptRef marker rewrites
  can't fire on `gcg-`/`ga-` IDs with no `.attempt.N`/`.iteration.N`/scope
  prefix segments).
- Legacy-hit counter: atomic package-level int64, incremented only when the
  legacy cascade actually FINDS an attempt — this also catches the
  stale-pointer crash-window case (below), so Phase-4 gating on counter drain
  is self-protecting.
- Step-id collision surface (two controls sharing bare gc.step_id under one
  workflow root, e.g. non-target-namespaced expansion templates): NOT a
  regression — legacy stage 2 already prefix-matched bare
  `stepID+".attempt."` refs with the same exposure; expansion instances with
  duplicate IDs would already break fragment resume-matching before this ever
  mattered.

### Issues / gaps
1. (MEDIUM, process) Branch is based 5 commits behind origin/main (base
   3dad18341; missing S09/S04/S37/S13/S12). The requested two-dot diff
   contains reversions of all five. Must be rebased before squash-landing;
   any tooling that applies origin/main..branch as a patch reverts landed
   work. Merge-base diff itself is clean (7 files, dispatch/formula only).
2. (LOW-MEDIUM, spec deviation) W6 crash window: appendRalphRetry re-entry
   via resolveExistingRalphRetryFromBeads returns EARLY without re-running
   the control_for SetMetadata remap. Crash between clone-create and remap →
   permanently stale bead-ID pointer. Mitigants: identical pre-existing
   window for gc.logical_bead_id (same pattern); stale pointer resolves via
   legacy fallback AND increments the counter, blocking Phase-4 deletion
   until visible. Spec I6 calls the remap "re-runnable" — it is, but the
   re-entry path never reaches it. Not a behavior regression today.
3. (MEDIUM, test gap vs spec) T3 shadow parity implemented as ONE hand-built
   2-candidate fixture, not "every fixture corpus in control_test.go and
   attempt_control_routing_test.go" as the spec's I1 deletion gate demands.
   Acceptable for Phase 1-3 landing, but the I1 deletion precondition is NOT
   yet proven; must be strengthened before Phase 4.
4. (LOW, spec deviation) W8 parity-contract extension (compile.go:582 —
   assert stamp presence on both mint origins) not implemented. Compile-time
   stamps step.ID, runtime stamps control.ID — a naive metadata-equality
   parity check would diverge; need to confirm no existing parity test
   compares gc.control_for values byte-for-byte (tests pass → it doesn't).
5. (LOW) Optional Phase-3 self-heal backfill (spec-recommended) not
   implemented. Long-lived pre-stamp ralphs will hold the counter above zero
   and delay Phase 4. Deliberate per spec ("optional").
6. (INFO) legacyAttemptLineageHitCount is only read by tests — the operator
   observability story ("watch it drain") has no exposure surface (no debug
   log/metric). Spec allowed "package-level counter or debug log"; a counter
   nobody can read from outside the process is weaker than the spec intent.
7. (INFO) W6 remap loop iterates `ordered` (includes prevSubject) — subject
   clone remap covered; prevCheck handled separately. No gap.

### Additional exhaustiveness sweep (the #1 question)
Enumerated every non-test writer of gc.attempt at the branch commit:
- control.go:634 (W4 root — stamped), control.go:696 (buildAttemptRecipe
  CHILD steps — scope members / nested-control re-mints, correctly NOT
  stamped; nested ralph children get iteration seeds via
  buildNestedControlSeed → stamped through W4; nested RETRY control children
  get no attempt seed at re-mint — findLatestAttempt empty → quarantine —
  IDENTICAL pre/post S38, not a regression).
- formula/retry.go:90 (W1 — stamped), formula/ralph.go:100 (W2 — stamped),
  :146 (W3 — stamped), :208 (ralph BODY children — correctly unstamped; see
  behavior-difference note below).
- dispatch/ralph.go:442/477/511/660 (W6/W7 clones — stamps carried via
  cloneMetadata + remap; clearRetryEphemera does not clear control_for).
- dispatch/retry.go:553/574 (v1 retry-eval run/eval re-mints) — OUT OF SCOPE
  and safe: findLatestAttempt is called ONLY from processRetryControl (:36)
  and processRalphControl (:166); the retry-eval path resolves by exact
  step-ref match and its cloneMetadata carries any stamp forward unchanged
  (its control identity is stable, no re-mint of the control bead).
No unstamped attempt-root mint path found. I1 coverage holds for the
runtime paths.

### Behavior difference (improvement, not regression)
Legacy stage-1 prefix (`controlRef+".iteration."`) also matched ralph BODY
children (e.g. mol.loop.iteration.2.review, gc.attempt=2). At equal
gc.attempt the `>` tie-break made the winner candidate-ORDER-dependent —
legacy could return a body child instead of the scope root, corrupting
oldScopeRef derivation in appendRalphRetry. The primary path matches only
the stamped scope root → deterministic and correct. Strict shadow parity
(I1: primary == legacy on ALL fixtures) is therefore not literally
achievable for this shape; the branch's narrow shadow-parity fixture
sidesteps it. Fine for landing, but Phase 4's deletion gate should compare
against intended semantics, not raw legacy output.

### Verification run (detached worktree at ca59e0f57)
- `go build ./...` clean.
- `go test -race -count=1 ./internal/dispatch/ ./internal/formula/` PASS
  (includes the #2798 fixtures and both ralph clone variants).
- `go test ./internal/molecule/ ./internal/runproj/ ./internal/graphroute/`
  PASS (I4/T7: projections and consumers unmodified and green).
- `go vet` clean on touched packages.
- compile.go "parity contract" at the cited line is the DRAIN metadata shape
  owner (ApplyDrainControlMetadata), not a retry/ralph metadata-equality
  test — no parity test compares gc.control_for byte-for-byte, so the
  step.ID-vs-control.ID two-population difference breaks nothing (W8 test
  extension still unimplemented, see gap 4).

### Residual (accepted) risks
- Formula-authored gc.control_for on a non-control WORK step that also
  carries gc.attempt >= the real latest attempt could false-positive-match on
  the primary path (legacy ignored control_for on work beads). Requires
  authored gc.control_for + gc.attempt + identity collision — contrived;
  the W4 shadow-protection covers the attempt-root case.
- Identity-set drift (spec risk 4) remains as documented: step-ref-valued
  stamps depend on rewriteRetryControlFor keeping control gc.step_ref and
  stamps in lockstep across ralph retries.

