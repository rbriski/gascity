# S04b review notes (adversarial) — branch simplify-land/s04b

## Walk 1 — internal/config (the spec'd scope)

- workquery.go: table (queryKind/querySpec/queryTable), resolver
  `effectiveQuery`/`effectiveQueryForBeads`, 14 exported wrappers converted.
  Diff hunks show ONLY the function headers + override-check lines changed;
  builder bodies untouched in the diff => verbatim moves. Override fields per
  table row match spec (WorkQuery x4, ScaleCheck, OnDeath, OnBoot). GOOD.
- `_ = includeEphemeral*` discards in buildOnDeath/buildOnBoot: bodies not in
  diff hunks => preserved. VERIFY on branch file directly.
- session_capacity.go: 9 capacity helpers + DrainTimeoutDuration moved
  verbatim (matches deleted text 1:1 in diff). nil guards intact. GOOD.
- config.go: EffectiveDefaultSlingFormula moved verbatim. GOOD.
- `time` import dropped from workquery.go (only DrainTimeoutDuration used it). OK.

## Resolved concerns

1. **Scope creep vs spec — FALSE ALARM.** Branch merge base is 12d800e6b;
   origin/main has since merged S19 (c6b851a) + S34 (5094b81), which touch
   cmd/gc. `git diff origin/main..branch` showed those reversed. Against the
   TRUE merge base, the branch touches ONLY internal/config (+ testdata).
   Branch = exactly the spec's 3 commits (d70a3f9 oracle, 15ce6b1 table,
   39d51cc rehome). main's advance touches ZERO internal/config files =>
   rebase is trivial/clean.

## Mechanical byte-identity verification (the #1 thing)

- Extracted all 7 old private bodies from merge-base workquery.go and all 7
  new build* bodies from branch HEAD; after stripping only the func header +
  3-line override check (now in the resolver), **all 7 are byte-identical**
  (diff clean): Work, AssignedInProgress, AssignedReady, RoutedPool,
  PoolDemand, OnDeath, OnBoot. `_ =` flag discards preserved in
  buildOnDeath/buildOnBoot (I6).
- Oracle copies (oldEffective*) vs merge-base privates: 6 of 7 byte-verbatim;
  oldEffectiveOnDeath drops a 6-line COMMENT block (no code delta; the
  comment survives in production buildOnDeath). Cosmetic only. MINOR NIT:
  spec said "verbatim copies"; comment drop is harmless.
- Goldens: all 42 files created in commit 1 (pre-refactor, generated from
  OLD code paths), untouched in commits 2-3 => passing goldens at HEAD prove
  old==new bytes. 42 = 3 shapes x 7 kinds x 2 bd modes. Matches spec.
- Table rows: override fields correct per kind (WorkQuery x4, ScaleCheck for
  PoolDemand, OnDeath, OnBoot) — I3 holds; no crosswiring.
- Resolver preserves `!= ""` (not TrimSpace) and verbatim override return
  (I4); ForBeads == plain + UsesBD105ReadySemantics() exactly once (I5).
- Exported wrappers: all 14 names/signatures/doc comments kept; wrapper-only
  diffs. EffectiveScaleCheck / EffectiveSlingQuery / DefaultSlingQuery stay
  outside the table, unchanged. I2 holds.
- Rehome: session_capacity.go = verbatim move of DrainTimeoutDuration + 9
  capacity helpers (nil guards intact, I7); EffectiveDefaultSlingFormula
  moved verbatim into config.go; `time` import dropped from workquery.go
  (its only user left). Same package, zero caller edits.

## Execution verification (branch HEAD, throwaway worktree)

- `go test -race ./internal/config/` — PASS (7.7s).
- `go vet ./internal/config/` + `go build ./...` — clean.
- Per-commit greenness: commit 1 (d70a3f9) parity+golden+flag-blind tests
  PASS; commit 2 (15ce6b1) full config package PASS. Each commit
  independently green as spec staged.
- `-update` flag: single registration in package config; no collision.
- Rehome check (spec item 7): grep MaxActiveSessions|Supports|DrainTimeout
  in workquery.go → zero hits; file now contains only query builders +
  resolvers + shell/jq codegen.
- Golden sanity: 42 files; OnDeath/OnBoot bd104==bd105 byte-identical for
  all 3 shapes (I6 flag-blindness pinned in fixtures); Work bd104 vs bd105
  differ as expected (flag-sensitive).
- No `-update` churn: goldens created in commit 1 from OLD code, untouched
  after; HEAD tests pass against them = byte-identity proven by execution,
  not just review.

## Nits (non-blocking)

1. Oracle `oldEffectiveOnDeath` drops a 6-line comment block vs the
   merge-base private (spec said "verbatim copies"). Code is identical;
   comment survives in production buildOnDeath. Cosmetic.
2. TestWorkQueryGolden pins only the ForBeads form per kind. Plain forms
   are pinned via the parity oracle today; when the oracle copies are
   deleted (planned follow-up), plain variants lose their direct pin.
   Structurally plain(kind) == forBeads(kind, bd104) through the single
   resolver, so risk is negligible — but worth a golden row or a
   plain==forBeads(bd104) assertion in the follow-up.
3. Branch base (12d800e6b) is 3 commits behind origin/main; main's advance
   (S19/S34) touches zero internal/config files, so rebase/merge is clean.

## Verdict

APPROVE (with the nits above). I1-I9 all verified: byte-identical output
(mechanical body diff + goldens + parity, all three independent), frozen
exported surface (wrapper-only diffs, full-tree build green), per-kind
override fields correct, verbatim override return, ForBeads flag sourcing
single-point, OnDeath/OnBoot flag-blind preserved, nil guards moved intact,
no new imports/no role names/no wire types, same-package moves only.
