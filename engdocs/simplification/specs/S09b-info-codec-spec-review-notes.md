# S09b review notes (adversarial) — branch simplify-land/s09b

Reviewer working notes. Verdict at end.

## Setup facts

- Branch = merge-base 12d800e6b + ONE commit b1b9fb4cb (spec asked 4 staged
  commits M-1..M-4; landed as one — staging deviation, not correctness).
- merge-base 12d800e6b is BEHIND current origin/main → `git diff
  origin/main..simplify-land/s09b` is polluted with reverse-diffs of main's
  newer commits (prompt_delivery/session_identity/session_level_converge
  deletions are NOT part of this change). Reviewing 12d800e6b..b1b9fb4cb.
- be1d46258 (#4033, SleepReason constants) IS an ancestor — spec baseline
  requirement satisfied.
- TODO: check branch still merges/rebases cleanly onto current origin/main
  and tests pass post-rebase.

## Findings (running)

### Codec parity walk (Part B) — verified line-by-line

- Table has all 72 keys of the old ApplyPatch switch; every setter body is
  byte-identical to the old switch case. Unknown keys: index miss == old
  `default:` no-op. OK.
- Projection: prologue sets ID/Type/Title/Labels/CreatedAt/Closed BEFORE the
  loop (session_name reads i.ID, state reads i.Closed) — order requirement met.
- provider/transport: table order provider→transport; final Transport =
  normalizeTransport(provider, transport) == old transportFromMetadata(b)
  (verified helper source: transport wins; "acp" fallback). Intermediate wrong
  Transport after provider setter is overwritten by transport setter, which
  ALWAYS runs in projection (every key runs). OK.
- alias_history: normalizeAliasList returns nil `var out []string` when nothing
  survives, so Split("",",")→[""]→nil == AliasHistory()'s len==0 fast path.
  nil-vs-empty parity holds incl. sparse beads. OK.
- wake_attempts / LastNudgeDeliveredAt total forms: agree with old projection
  on fresh zero Info (explicit =0 / =time.Time{} are no-ops there). OK.
- normalizeInfoState / LifecycleReasonRuntimeMissing ("runtime-missing")
  verified against branch source. OK.
- Oracle files (info_apply_patch_test.go, info_store_test.go) UNTOUCHED in the
  commit (R4 satisfied — fold oracle independently gates the swap).
- New tests T1-T4 + provider/transport order-convergence present, incl. a
  frozen verbatim copy of the OLD projection as independent oracle (T2).

### Part A (sleep reasons) — verified constants

- session.SleepReason* values match every replaced literal exactly (city-stop,
  idle-timeout, idle, drained, failed-create, provider-terminal-error,
  wait-hold, runtime-missing via LifecycleReasonRuntimeMissing). Compile-time
  equality; zero on-store string change.
- Vocabulary discipline spot-checked: state=="drained" (state_helpers:12,27),
  meta["state"]=="failed-create" (session_reconcile), sleep_intent "wait-hold"
  left as literals — correct per I8.

### Verification results

- **Frozen oracle fidelity:** `infoFromPersistedBeadFrozen` in
  info_codec_test.go is BYTE-IDENTICAL (modulo comments/whitespace) to the
  pre-change `InfoFromPersistedBead` body at 12d800e6b — diff-verified. So T2
  really pins new-vs-OLD, not table-vs-table.
- **oracleBaseBeads()[3]** is the acp base (provider="acp", no transport) —
  the fallback-sensitive region — as the order-convergence test assumes.
- **Key set:** table = 72 entries = old switch key set = allProjectedMetadataKeys
  (T1 gates duplicates + drift both ways, and index-size vs table-size).
- **molecule_id repoint complete:** only non-test `"molecule_id"` remaining is
  the beadmeta constant definition itself. handler_beads keeps bare
  `"workflow_id"` with the do-not-substitute NOTE (R3 honored).
- **Sleep-reason completeness:** no remaining literal sleep_reason WRITES in
  cmd/gc (only a "" clear). "quarantine" returns at session_reconciler.go:48/63
  are the lifecycleTimerBlocker vocabulary (sibling "user_hold" ≠ "user-hold")
  — correctly untouched. ClosePatch(now,"drained") takes a STATE code (writes
  "state"/"close_reason", not sleep_reason) — spec's :420/:422 convert
  suggestion was wrong; implementation correctly verified and left it.
- **Tests run:** go build ./... clean; go vet clean on all touched pkgs;
  `go test -race ./internal/session/` PASS (126s) incl. all 5 new codec tests
  + unmodified fold oracles; runproj/t3bridge/api PASS; targeted cmd/gc tests
  (slot-freeable matrix, heal-state, churn, city-stop, close-gate,
  terminal-error, awake-set) PASS.
- **Merge safety:** `git merge-tree origin/main simplify-land/s09b` exits 0
  with 0 conflicts; `git diff 12d800e6b origin/main` on info_store.go /
  info_apply_patch.go / info_apply_patch_test.go / sleep_reason.go /
  beadmeta/keys.go is EMPTY — no main-side semantic drift the merge could
  silently combine.

### Nits (no behavior change)

1. **Staging deviation:** landed as ONE commit instead of the spec's four
   (M-1..M-4). Revert story still clean (single coherent commit).
2. **Incomplete Part A at drain-reason sites:** session_reconciler.go:3269-3277
   `reason = "idle"` / `"no-wake-reason"` literals (spec-listed :3269/:3273)
   flow via the drain tracker into `CompleteDrainPatch(clk.Now(), ds.reason,…)`
   → sleep_reason. Strings equal constants, so behavior identical; convert in
   a follow-up for typo-proofing.
3. **Pre-existing vocabulary gap (out of scope):** cmd_session.go:2306
   `SleepPatch(now, "killed")` — "killed" has NO SleepReason constant (S09
   omission, present on origin/main unchanged). Follow-up: add
   SleepReasonKilled.
4. **TestInfoCodecFieldsDisjoint sentinel "1":** the
   MetadataLastNudgeDeliveredAt setter touches zero fields under "1" (parse
   fails), so disjointness is vacuous for that key; acknowledged in the test
   comment. Minor test-strength nit.
5. Projection now runs 72 closure calls + map reads per bead vs one struct
   literal — negligible vs the map reads the old code already did.

## Verdict

**approve-with-nits.** Codec byte-parity is demonstrated three independent
ways (line-by-line setter identity, frozen-old-code oracle T2, unmodified fold
oracle I2), -race clean; sleep-reason migration converts every direct
sleep_reason read/write with compile-time-equal constants and honors the
vocabulary discipline (I8) — including correctly REJECTING two spec-suggested
conversions that were actually state/blocker vocabulary; molecule_id repoint
complete with the workflow_id trap avoided. Merges conflict-free onto current
origin/main with zero main-side drift in the codec files.

