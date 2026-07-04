# Next-session prompt — reconciler front-door Step 6d WIRING (Commits 1–4 done; Commit 5 = pre-pass deletion next)

Paste the block below into a fresh session.

---

Continue the **session reconciler front-door migration** on **PR #3839** (branch
`upstream/object-front-doors-cleanup`, base `main`, DRAFT, worktree
`/data/projects/gascity/.claude/worktrees/object-front-doors`; `git rev-parse HEAD`).

**Read first, in order:**
1. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-STEP6-DESIGN.md` — the
   Step-6 design + backlog. **Read §2 (intra-tick model + why the Get-cutover is
   write-returns-`Info`, not a blanket `Get`), §5 (fable red-team constraints — the
   9-refresh-site set, ~15 nested-helper writers, the restart_requested overlay
   lifecycle, the store-only-close family), AND §7 (6c execution + the 6d
   carry-forward landmines).** READ THIS FIRST.
2. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-HANDOFF.md` — status.
   Steps 0–5 DONE, 6a/6b/6c DONE, **6d foundation + read-after-write harness DONE**; you
   are on the **6d WIRING** (then 6e). **STEP6-DESIGN §8 is the authoritative per-site plan.**
3. `engdocs/plans/infra-store-decouple/RECONCILER-FRONT-DOOR-SPEC.md` — §2 governing
   principle (never drop a lockstep before its same-tick reads are on the snapshot).

**Where things stand.** The reconciler decision path is fully on `Info`. 6b landed the
flippable decision-read conversions; **6c** (`3b7795598`) converted the sole remaining pure
read-side raw-working-set consumer (`clearMissingIdleProbes`→`infoByID` presence,
byte-identical), verified by an opus audit + a 4-lens fable adversarial panel (0 defects).
What remains raw is exactly the **write/lockstep machinery**: the forward-pass loop
`for i := range ordered`/`&ordered[i]`, the CB persist, the blanket refresh pre-pass @2774,
the wakeTargets loop, `sessionLookup`→drain mutations, and `refreshSessionInfo`'s raw
source. Removing all of it is **6d — the LANDMINE cutover.**

**Both 6d enablers are LANDED — you are past the risky-to-design phase, now execute the wiring.**
1. **Foundation `b031a356d`** — `Info.ApplyPatch(patch) Info` (internal/session/info_apply_patch.go):
   the OWNER-LOCKED write-returns-`Info` primitive. Folds a metadata patch onto a projected
   `Info` byte-identically to a full re-projection (oracle `TestInfoApplyPatchMatchesReprojection`,
   normalizer-branch coverage mutation-verified; 3-lens fable panel, 0 impl defects). UNWIRED.
2. **Read-after-write harness `4f0a6ea8b`** — `cmd/gc/session_reconciler_read_after_write_test.go`.
   The write oracle is blind to same-tick stale reads (SPEC §2); this harness runs the REAL tick
   over a **single-template** working set (so `topoOrder` returns input in **slice order** —
   `buildDepsMap` is empty with no `DependsOn` → `session_reconcile.go:1289` fast path), letting
   you place a mutation EARLIER in the slice than a dependent read and assert an outcome that
   flips iff the mutation reached the read through `infoByID`. First test
   `TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose` (teeth-verified via bug injection;
   0-defect 3-lens fable review). **Each wiring commit adds its sibling read-after-write test in
   this file, same pattern.**

**The authoritative per-site wiring plan is STEP6-DESIGN §8 — read it first.** Key §8 facts you
will rely on: (a) under write-returns-`Info` the snapshot only ever receives MIRRORED batches, so
the `reset_committed_at` freeze overlay is UNNEEDED — only `restart_requested` (the in-memory-only
write @~2130) needs an explicit ApplyPatch + clear-on-persisted; (b) the close sites split by
whether the close helper mirrors a ClosePatch onto the raw bead: **store-only closes
(`closeFailedCreateBead`@1890, `closeBead`@2387 — both take an `id`, never a `*beads.Bead`) → the
only raw-bead change is `session.Status="closed"` → the byte-identical refresh is `MarkClosed`
ONLY** (Closed=true, State=""); the **`finalizeDrainAckStoppedSession` closes DO mirror a
`ClosePatch`** (@~372) → those need `ApplyPatch(closeBatch) + MarkClosed`.

**Confirm a green baseline (use an ISOLATED GOCACHE — shared-cache stale-object hazard):**
```
go build ./... && go vet ./cmd/gc/... ./internal/session/...
golangci-lint run ./cmd/gc/... ./internal/session/...   # expect 0
ISO=$(mktemp -d); GOCACHE=$ISO go test ./internal/session/ -run 'TestInfoApplyPatch' -count=3 \
  && GOCACHE=$ISO go test ./cmd/gc/ -run 'TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose|TestSessionClassifierInfoEquivalence' -count=3; rm -rf "$ISO"
git checkout go.sum
```

**DO — the 6d wiring, ONE small commit per site (KEEP the raw `session.Metadata[k]=v` mirror on
every one until the final deletion; each is byte-identical + gets a read-after-write test):**

**Commit 1 DONE (`cfd6893fb`).** `Info.MarkClosed()` + the two **store-only** close refreshes
(failed-create, orphan) → `infoByID[id] = infoByID[id].MarkClosed()`, keeping the raw lockstep.
Teeth-verified (`…MidTickClose` + `…Orphan`).

**Commit 2 DONE (`e2f1f4adf`).** The three **drain-ack `finalize*` closes** wired via
`drainAckFinalizeResult{batch, closed, witnessInfo}` + `result.applyTo(infoByID[id])`.
Correction to the original one-liner plan: `finalizeDrainAckStoppedSession` has FOUR exit
shapes — Path A (ClosePatch mirror → `ApplyPatch.MarkClosed`), Path B (NDI witness wholesale
metadata swap → full reprojection, the one path still reading the raw bead), Path C (non-close
drain-ack incl. `restart_requested` clear → `ApplyPatch`, no MarkClosed), and
early/error/async → zero result. `reconcileDrainAckStopPending` returns `(bool, result)`; the
two statement-call sites discard the new return. THREE teeth-verified per-site tests
(`…DrainAck` site 1, `…DrainAckOrphan` site 2, `…DrainAckReconciler` site 3) +
`reconcileAtPathWithDrainOps` helper. 6-lens fable panel (wf_3d1f12c0): 0 defects.
**Line numbers shifted after Commit 2 — re-grep every anchor below before editing.**

**Commit 3 DONE (`a7edb1edc`).** The two **nested-helper-write refreshes** → `ApplyPatch(batch)`:
HEAL (`healStateWithRollback` already returns its mirrored batch) and ZOMBIE
(`markProviderTerminalError` changed to `(map[string]string, error)`; reconciler captures
`terminalErrBatch`, nil when the zombie path didn't run; 2 callers take `_`). Byte-identical +
coherence-verified. Two teeth-verified tests (`…ZombieTerminalErrorReflectedOnSnapshot`,
`…HealStateReflectedOnSnapshot`). **LANDMINE the fable panel (wf_1cfcf522) caught: the heal fold
is NEWLY load-bearing** — the old zombie-site full re-projection masked a stale heal snapshot, but
the zombie fold is now `ApplyPatch(nil)` on the no-terminal-error path, so the heal fold alone
carries the healed state to the post-zombie rollback read on the `case preserveNamed` fall-through.
The first draft's "no heal observable" claim was empirically FALSE; the missing heal test + 2
inaccurate comments were fixed. **Line numbers shifted after Commit 3 — re-grep every anchor.**

**Commit 4 DONE (`556f02696`).** Folded the `restart_requested` overlay: the in-memory-only SET
@~2259 (`ApplyPatch(MetadataPatch{"restart_requested":"true"})`) + the CONSUME @~2331 (`restartFold`
= RestartRequestPatch minus `ResetCommittedAtKey`; clears the marker on consume-success; survives
only the failure `continue`s = #2574 lifecycle). Byte-identical + coherent. **These folds are MASKED
by the blanket pre-pass @~2913** (re-projects every session before the awake scan), so no behavior
change + no isolated teeth test — prerequisite setup for the pre-pass deletion. 4-lens fable panel
(wf_06452ded): 0 defects. **Line numbers shifted after Commit 4 — re-grep every anchor.**

**START HERE — Commit 5 — DELETE the blanket pre-pass @~2913 (the LANDMINE).** Gated on folding the
COMPLETE forward-pass writer set — **re-enumerate from code (STEP6-DESIGN §5), do NOT trust a list.**
Head-start from the Commit-4 review (all masked, unfolded): (1) **pending-create rollback**
(`rollbackPendingCreate`, `continue`d @~2002/@~2022, mutates `session_name` + closes in-store);
(2) **`resetConfiguredNamedSessionForConfigDrift`** (@~2538 / @~2726, mirrors `ConfigDriftResetPatch`
@~4201: state/session_key/last_woke_at/pending_create_*/continuation_reset_pending, then `continue`);
(3) **SleepPatch max-age/idle kills** (raw-mirror ~2801 / ~2875); (4) **stability/churn/detach
writes**. Fold each (byte-identical + coherence-checked, like Commits 1–4), THEN delete the pre-pass,
THEN add the comprehensive read-after-write test now possible (kill-success-then-awake-scan asserts
`restart_requested` empty; config-drift-repair asserts the reset-pending wake rung fires). Verify each
fold is teeth-testable ONLY after the pre-pass is gone (before that they are masked). Then the
aggregating refreshes + working-set removal (below).

**Then — retire the blanket pre-pass + working set (the deletions).** After the pre-pass is deleted,
convert the last raw consumers — `advanceSessionDrains` mutations (`completeDrain` off
the raw bead → retire `sessionLookup`) and feed `newSessionBeadSnapshot` (via
`resolvePreservedConfiguredNamedSessionTemplate`, bucket-D, HARDEST — may need a store `List`).
Only THEN drop every `session.Metadata[k]=v` lockstep, delete `refreshSessionInfo`, `beadByID`,
`circuitSessionByIdentity`, and `ordered []beads.Bead` — replacing `ordered` as the iteration
domain with an **ORDER-PRESERVING** `[]Info`/`[]string` (NOT map iteration:
`buildAwakeInputFromReconciler` appends to `input.SessionBeads` in slice order and `ComputeAwakeSet`
does `SessionName`-keyed last-write-wins over it — 6c-audit landmine; `openPoolSessionCountForTemplate`
MAY domain-switch to `infoByID`, unique IDs proven).

**Guard rails (all in STEP6-DESIGN §5/§8):** **NO unconditional per-iteration `Get` on the forward
pass** — the @~1854 refresh is unconditional and a `Get` consumes the injected attachment-check
errors (3 fail-safe tests: session_reconciler_test.go:7661,:7833; session_reconciler_progress_test.go:202);
write-returns-`Info` avoids this. Before deleting the pre-pass @~2774, regenerate the COMPLETE
forward-pass writer set (§5 lists ~15 writers, many 2–3 helper layers deep). The store-only-close
family's `Info.Closed=true` evicts the session from `AwakeInput.SessionBeads` — bless that eviction
in a test.

**6d carry-forward from the 6c audit (STEP6-DESIGN §7):** the `ordered` domain params on
`openPoolSessionCountForTemplate` (safe domain-switch — unique IDs proven) and
`buildAwakeInputFromReconciler` (**NOT** safe — `input.SessionBeads` slice order is load-bearing
for `SessionName`-keyed last-write-wins in `ComputeAwakeSet`; keep the ordered domain) and
`advanceSessionDrains` (dead `ordered` param in the prod call) all retire WITH the working set,
not before. The derived `wakeTargets` aggregate keeps a raw `*beads.Bead` for the
`persistSleepPolicyMetadata` write @~2853.

**Then 6e** (extend `snapshotInfoOnlyFiles` in frontdoor_di_guard_test.go to forbid raw session
`.Metadata[` and add the reconciler files once raw-free).

**Optional 6d-prep siblings (additive, byte-identical; land only if useful):**
`freshRestartSessionKeyInfo` (reads `SessionIDFlag`/`ResumeFlag`/`ResumeStyle`/`ResumeCommand`
— all already on `Info`, NO codec gap), `recentlyDeferredSessionAttachedConfigDriftInfo`
(pure read), wire the existing `resetPendingCommittedAtInfo`. Their call sites are frozen
(forward pass / write-path) so the sibling lands in 6b-style but the flip is part of 6d.

**DO NOT** delete the raw classifier siblings (`lifecycleTimerBlocker`,
`isDrainAckStopPending`, `ParseTemplateOverrides`) — they are the oracle's byte-identity
ground truth. **DO NOT** delete `evaluateWakeReasons`/`wakeReasons`/`computeWakeEvaluations`
— they are live (nil-guard fallback + `gc session list`).

**Gates per commit:** `go build ./...` · `go vet` · `golangci-lint ./cmd/gc/...
./internal/session/...`=0 · gofmt · **the `Info.ApplyPatch` oracle (`TestInfoApplyPatch*`) + the
read-after-write harness (`TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose` + the new
sibling for this commit)** + whole-tick `TestReconcileSessionBeads*` + circuit/named/pool/wake/
sleep/drain/trace (heavy suites in the background). **For every conversion, before committing,
prove the new test has TEETH: temporarily break the refresh (e.g. delete the `MarkClosed`/ApplyPatch
line) and confirm the sibling test FAILS, then restore.** **Run oracles under an isolated GOCACHE.**
`git checkout go.sum` after. Commit AND push `--no-verify`. Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt LOCAL-ONLY (no
`bd dolt push`). #3839 stays DRAFT. Quote grep globs (`--include='*.go'`). Mapping agents
have read the WRONG worktree (`.worktrees/pack-crud`) — pin HEAD, verify line numbers.
Update the handoff + STEP6-DESIGN check boxes + memory (`infra-beads-decoupling-plan.md`).

---
