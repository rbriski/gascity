# S08s0 adversarial review notes — branch simplify-land/s08s0

Reviewer pass 2026-07-07. Branch = 2 commits on top of origin/main @12d800e6b
(exact spec baseline; merge-base == origin/main):
- b8154c389 delete dead trace symbols (D1-D4)
- 494a46489 delete LegacyArms dual field (D5)
Matches the spec's two-commit staging exactly.

## Diff-stat scope check — PASS
Only 3 files touched (cmd_trace_test.go, session_reconciler_trace_cmd.go,
session_reconciler_trace_types.go). Nothing in lifecycle-fold, wake engine,
convergence, S26 recording surface, internal/api, schemas/, events. The held
wire-or-delete judgment is untouched. Confirmed via `git diff --stat`.

## Deadness re-proof (my own greps, tip + origin/main) — PASS
Pre-state on origin/main (excluding engdocs):
- TraceEvaluationCapRejected: 1 ref (decl types.go:286) only.
- TraceEvaluationMissingTemplate: 1 ref (decl :288) only.
- TraceTextBlob/NewTraceTextBlob: 3 refs, all self-contained (:323,:330,:332).
- cmdTraceStatus (word-boundary, non-WithJSON): decl :344-346 + ONE test
  caller cmd_trace_test.go:50-51. Production entry is the cobra RunE →
  cmdTraceStatusWithJSON; no reflective dispatch possible on an unexported
  func in package main.
- LegacyArms: field :49, shim :59-63, write :739 — all intra-shim.
- traceArmsJSONSlice: callers exactly :60,:63,:739 + def :743.
Post-state on tip: ALL six greps return zero code hits. `"arms"` remains only
in the two inverted test assertions and TraceArmState.Arms (disk file,
types.go:459 post-delete — the LIVE out-of-scope struct, untouched).

## LegacyArms behavior preservation — PASS
- Wire confined: traceStatusJSON appears ONLY in cmd/gc trace files
  (traceControlReply.Status on the controller unix socket). No dashboard,
  no internal/api, no schema reference.
- Every reader of .ActiveArms on tip is len()-based (tests, cmdTraceStart/
  Stop "active trace arms: %d") or the nil-normalized copy in
  cmdTraceStatusWithJSON (`if activeArms == nil { activeArms = []TraceArm{} }`
  at :343-345). So the one decode-side delta (old shim backfilled nil
  ActiveArms from a non-nil "arms" mirror; also mirror-nonnil-ified [] cases)
  is unobservable for same-version pairs: nil vs [] never distinguished.
- Producer delta: reply loses the redundant "arms" mirror key only;
  ActiveArms assignment (`arms` direct, never through the helper) unchanged,
  so its nil/[] encoding is byte-identical pre/post.
- Version-skew: matches spec Invariant 6. Only pre-#2427 (2026-05-20)
  controller + post-change CLI degrades, display-only ("0 arms"), no error.

## Test changes — PASS with notes
- D4 repoint: test caller now cmdTraceStatusWithJSON(false,...) preserving
  the text-mode assertions ("Head seq: 0", "repo/polecat"). NOTE: spec names
  the test `TestTraceStartStatusStop`; actual test is
  `TestTraceStartStopStatusOfflineFallback`. Spec-doc naming nit only — the
  grep proof found exactly one caller, and it is this one.
- Inverted assertions (spec test-plan step 3) present in BOTH status and stop
  payload checks: payload must NOT contain `"arms"`. False-positive check:
  `"active_arms"` does NOT contain the byte sequence `"arms"` (needs
  quote-a-r-m-s-quote; active_arms has `_` before arms). TraceArm field tags
  (armed_at etc.) also cannot match. Assertion is sound and pins
  "stop writing arms".
- TestTraceStatusJSONAcceptsLegacySocketArms deleted wholesale — correct:
  it tested exclusively the deleted compat path (bare-"arms" reply decode).
- All positive active_arms/len(ActiveArms) assertions retained.

## Gates (run locally, checkout /data/tmp/simplify/exec/s08s0) — ALL PASS
- `go build ./cmd/gc/` + `go vet ./cmd/gc/` at tip: OK, zero extra edits
  needed (Invariant 9: compiler-as-deadness-oracle holds).
- Commit 1 (b8154c389) checked out alone: builds + vets clean —
  individually-compiling staging confirmed.
- `go test ./cmd/gc/ -run 'Trace' -count=1`: ok (14.6s).
- `go test ./cmd/gc/ -run 'Trace' -race -count=1`: ok (33.3s).
- Per-commit stats match the spec split: commit 1 = D1-D4 (types.go -24
  = 1+1+22 lines, cmd.go -4 wrapper, test repoint only); commit 2 = D5
  (cmd.go -25, test -47). No stray files.

## Findings
1. NIT (spec doc, not code): spec D4/test-plan names the repointed test
   `TestTraceStartStatusStop`; the actual (and only) caller is
   `TestTraceStartStopStatusOfflineFallback`. Grep proof still held —
   exactly one caller existed and it was repointed. No action needed on
   the branch; optionally fix the spec text.
2. Sibling dead consts TraceEvaluationDependencyBlocked/StorePartial
   correctly LEFT in place (spec's out-of-scope list) — verified present
   at tip.
3. TraceArmState.Arms (json:"arms" disk file), MissingTemplate bool
   record field, TraceReason* codes: all untouched at tip — verified.

## Verdict
APPROVE. Pure delete, behavior-preserving; every deleted symbol proved
dead by independent grep on both origin/main and tip; LegacyArms drop
unobservable for supported pairings (all ActiveArms readers len()-based
or nil-normalized); held wire-or-delete scope untouched; invariants 1-9
verified; tests inverted to pin "arms" cannot return.
