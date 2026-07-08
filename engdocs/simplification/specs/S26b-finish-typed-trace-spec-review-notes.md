# S26b adversarial review notes — branch simplify-land/s26b

Reviewer scope: typed trace surface completion (pool-cap rejection + remaining outcomes).

## Base / diff hygiene

- Branch = single commit `8c4b657c3` on `12d800e6b`. `origin/main` has since advanced
  by one commit (S19 stage 1, `c6b851ac1`), so `git diff origin/main..simplify-land/s26b`
  includes the INVERSE of S19 as noise (prompt_delivery.go, session_identity.go,
  session_level_converge.go "deletions" are NOT part of S26b). Real diff:
  `git diff 12d800e6b..8c4b657c3` — 9 files, matches spec scope (cmd/gc only).
- S26 (`65899d317`) IS an ancestor of the base — spec's base requirement satisfied.
- NOTE: S26b touches session_reconciler.go / session_lifecycle_parallel.go /
  session_wake.go, all also touched by S19 on main → merge/rebase will conflict;
  needs a rebase before landing (mechanical, but recorded here).

## Commit 1 equivalent — constants (trace_types.go)

- 10 constants added: 3 reasons (max_session_age, user_hold, quarantine),
  7 outcomes (resolution_failed, start_error_converged, session_initializing,
  start_enqueued, deferred_user_hold, deferred_quarantine, deferred_busy).
  Values match spec verbatim. OK.
- TestTraceCodeConstantValues pins all 10 values. OK (spec test 1).

## Group A — pool-cap path (pool_desired_state.go)

- `nestedCapUsage.rejection` retyped; three literal pairs → constants. Verified
  constant values on branch: TraceSitePoolAgentCap="reconciler.pool.agent_cap",
  TraceReasonAgentCap="agent_cap", rig/workspace likewise — byte-identical. Predicate
  logic, order (agent→rig→workspace), payload contents UNCHANGED. OK.
- `newDemandBlockingScope` retyped, `string(...)` wrappers dropped, sentinel
  `("", "", 0, 0, nil)` preserved; `if site == "" { return }` guard intact. OK.
- A6 payload: `"active_capacity_kind": string(reason)` — explicit string, Go dynamic
  type preserved per spec. OK.
- Casts dropped at both RecordDecision sites (applyNestedCaps + recordNewDemandCapTrace). OK.

## Group B — outcome producers

- B1 build_desired_state.go scale-check: success/failed → TraceOutcomeSuccess/Failed
  ("success"/"failed" verified). OK.
- B2 session_wake.go GC_DRAIN_ACK: same swap; `fields["reason"] = ds.reason` untouched. OK.
- B3 session_reconciler.go rate-limit: TraceOutcomeHeld("held")/HoldDeferred("hold_deferred"). OK.
- B4 preserve-configured-named: map[bool]string idiom → explicit if; KeptOpen("kept_open")
  + NEW ResolutionFailed("resolution_failed"). OK.
- B5 session_lifecycle_parallel.go: startResult.outcome → TraceOutcomeCode. ALL producer
  assignments converted (panic_recovered, start_error_converged, session_initializing,
  deadline_exceeded, canceled, success, provider_error, session_exists{,_converged},
  start_enqueued, + async refresh 1509). All 4 RecordOperation casts dropped. Comparisons
  (1523, 1919, 2597, 2602) switched to constants. logLifecycleOutcome keeps string param;
  explicit string(result.outcome) at all 8 call sites — log bytes identical.
  IMPORTANT adversarial check (compiler does NOT catch stray literals — untyped string
  constants assign to defined string types): grepped all `outcome =`/`outcome:` string
  literals remaining in the file — every one belongs to stopResult/runResult (stop/interrupt
  wave, lines 2730-2816, 3104-3105) or the log-only stale_async_start local (1502-1505),
  all explicitly out of scope AND never traced (zero remaining TraceOutcomeCode casts in
  the file proves no trace path consumes them). NO missed startResult assignment. OK.
- B6 timerTraceCodes: total switch over reasons {max_session_age, idle_timeout, user_hold,
  quarantine, pending, assigned_work} and outcomes {stop, deferred_user_hold,
  deferred_quarantine, deferred_pending, deferred_busy}; default arms are IDENTITY
  passthrough (never "unknown" rewrites) — spec invariant 2 satisfied. Closed sets
  RE-VERIFIED against internal/session/lifecycle_timers.go (DecideMaxSessionAge/
  DecideIdleTimeout/deferDecision) and the only Blocker producers
  lifecycleTimerBlocker{,Info} (return exactly ""/"user_hold"/"quarantine"; only
  non-test `Blocker:` construction sites feed from them). Empty-string trace codes
  (gather/none actions) never reach the traced call sites (Defer/Stop arms only), and
  would pass through identically anyway. All 4 call sites rewritten. No upward import:
  internal/session untouched. OK.

## Out-of-scope inventory check (Group C)

Remaining non-test TraceReasonCode casts on the branch match the spec's Group C list
exactly (pending-create action, StateFailedCreate, ackReason x2, reason x6, ds.reason x6,
trace_cmd:417). Remaining TraceOutcomeCode/TraceSiteCode casts: ONLY the two
timerTraceCodes identity default arms. Spec grep gate satisfied.

## Tests

- Test 1 (constant pinning): TestTraceCodeConstantValues pins all 10 new constants. OK.
- Test 2: TestNestedCapUsageRejectionTyped — 3 cap kinds, asserts typed constants AND
  legacy literal strings. OK.
- Test 3: TestTimerTraceCodesTotal — enumerates all 3x3x3 TimerFacts x both ladders,
  asserts round-trip identity + membership in named set. Note: membership check cannot
  distinguish "named arm" from "default arm returning a known value", but since default
  is identity this only matters for NEW values, which the test does catch. Adequate.
- Test 4 (JSONL recorded-bytes parity per group): NOT delivered as a new test.
  Mitigation assessment: constants pinned byte-exact (test 1) + producer returns pinned
  to legacy literals (test 2) + round-trip identity (test 3) + record call sites pass
  the same values through the same unchanged Record* APIs. The composition covers the
  same corruption class; residual gap is theoretical (a Record* serialization change,
  out of scope/untouched). MINOR gap vs spec letter, not a correctness hole.

- No existing test file was modified except pool_desired_state_test.go, which is
  pure ADDITION (+71/-0) — no string expectation edits anywhere (spec rule honored).
- Existing collector-level oracles run unmodified: poolTraceDecision helper asserts
  recorded SiteCode on trace records; session_reconciler_trace_test.go +
  session_reconciler_trace_integration_test.go cover record/decode paths.

## Gates (run locally in throwaway worktree at 8c4b657c3)

- `go build ./cmd/gc` — OK
- `go vet ./cmd/gc ./internal/session` — OK
- 3 new tests pass (-v verified)
- `go test ./cmd/gc -run 'Trace|NestedCap|PoolDesired' -count=1` — ok 14.7s
- `go test ./internal/session -count=1` — ok 20.0s (untouched layer)
- `go test -race ./cmd/gc -run 'Trace|Lifecycle' -count=1` — ok 66.1s
- Grep gate: remaining TraceOutcomeCode/TraceSiteCode casts = ONLY timerTraceCodes
  identity default arms + trace_cmd CLI parse. PASS.

## Verdict

APPROVE-WITH-NITS.

Findings (all minor):
1. (nit/process) Branch base is 12d800e6b; origin/main has since gained S19
   (c6b851ac1) which touches session_reconciler.go / session_lifecycle_parallel.go /
   session_wake.go — rebase before merge; the raw `origin/main..branch` diff is
   misleading (contains S19's inverse as noise).
2. (nit/test-coverage) Spec test 4 (explicit JSONL recorded-bytes parity for one
   representative site per group, esp. B1 scale-check and B6 deferred_user_hold) was
   not added as a NEW test. Compensated by: constant pinning (test 1), producer
   literal pinning (test 2), round-trip identity (test 3), and unchanged existing
   collector-level trace tests. Corruption class is covered; the letter of the spec
   is not.
3. (nit) TestTimerTraceCodesTotal's "not default arm" check is membership-based; it
   cannot distinguish the named arm from the default arm returning an already-known
   value — harmless because the default is identity, and NEW values are still caught.

No behavior deviations found: every recorded site/reason/outcome code is
byte-identical (constants verified against replaced literals character-for-character);
predicate logic, evaluation order, sentinels, payload dynamic types, log lines, and
layering all preserved.

