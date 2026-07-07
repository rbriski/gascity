# S01 review notes — branch simplify-land/s01-full vs origin/main

Reviewer: adversarial pass, 2026-07-07.

## Finding 0 (scope) — branch bundles far more than S01

`git diff origin/main..simplify-land/s01-full --stat` touches:
- internal/beads/* (S01 proper)
- internal/config/config.go (+1001), pack.go (+158), workquery.go DELETED (-719) — NOT S01
- internal/session/sleep_reason.go + test DELETED — NOT S01
- cmd/gc/session_reconciler*.go, session_lifecycle_parallel.go, session_wake.go, pool_desired_state.go — NOT S01
- internal/sling/* — NOT S01

Spec mandates: "Each phase is a separate PR, ≤5 files". This branch is a
single land of 32 files / +2084 -1549 mixing at least 3 unrelated
simplification items. Violation of the staging plan on its face.

(to be continued — walking the beads diff next)
