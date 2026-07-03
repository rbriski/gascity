# Next-session prompt — reconciler front-door Step 6d WIRING (start the cutover)

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
only raw-bead change is `session.Status="closed"` → the byte-identical refresh is `markClosed`
ONLY** (Closed=true, State=""); the **`finalizeDrainAckStoppedSession` closes DO mirror a
`ClosePatch`** (@~372) → those need `ApplyPatch(closeBatch) + markClosed`.

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

**START HERE — Commit 1 (the model; @1590 is ALREADY guarded, so it's the safest first):**
Add `func (Info) markClosed() Info` (internal/session; returns the receiver with `Closed=true`,
`State=""` — exactly what `InfoFromPersistedBead` yields for a status-closed bead, since only
`State` is blanked on close; add a tiny oracle case). Convert the two **store-only** close
refreshes from `refreshSessionInfo(session.ID)` to `infoByID[session.ID] =
infoByID[session.ID].markClosed()`:
- **@~1590** failed-create close (`closeSessionBeadIfReachableStoreUnassigned`→`closeFailedCreateBead`).
  **Already guarded** by `TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose` — verify it
  stays green (no new test needed).
- **@~1834** orphan close (same helper → `closeBead`). Add a sibling harness test: an orphan
  companion (open, unassigned, not-desired, no live runtime) at slice index 0 that closes mid-tick,
  same min-floor assertion.
Byte-identical because both closes are store-only (the ClosePatch goes to the store, not the raw
bead, so today's raw reproject already only sees `Status=closed`). KEEP `session.Status="closed"`.

**Commit 2 — the drain-ack finalize closes (@~1456, @~1735, @~2045).** `finalizeDrainAckStoppedSession`
DOES mirror a `ClosePatch` (@~372), so change it to return its applied close batch and convert the
refresh to `infoByID[id] = infoByID[id].ApplyPatch(closeBatch).markClosed()`. Add a drain-ack
mid-tick-close harness test.

**Commit 3 — the nested-helper-write refreshes.** `markProviderTerminalError` (@~1886, feeds
`infoPostZombie`) already builds `batch` locally → change it to return `(sessionpkg.MetadataPatch,
error)` (3 callers: session_reconcile.go:687, session_reconciler.go:1853, session_lifecycle_parallel.go:1999
— only the 1853 caller uses the batch, the others take `_`); the @1886 refresh becomes conditional
`infoByID[id] = infoByID[id].ApplyPatch(terminalErrBatch)` (nil→no-op). This ALSO removes the
`Get`-consumes-injected-errors hazard (ApplyPatch never touches the store). Same for `healState`
(@~1628 `infoPostHeal`). Same-session read-after-write tests.

**Commit 4 — `restart_requested` @~2130** (in-memory-only write): `infoByID[id] =
infoByID[id].ApplyPatch(MetadataPatch{"restart_requested":"true"})`, and CLEAR it (empty) when a
persisted `restart_requested` batch lands (the 2144 consume / drain-ack clear / fresh-cycle) — else
#2574 phantom-restart. Add a kill-success-then-refresh test asserting it reads empty.

**Commit 5+ — retire the blanket pre-pass + working set (the deletions).** Once every forward-pass
writer self-refreshes, delete the blanket pre-pass `for i := range ordered { refreshSessionInfo }`
@~2774. Then convert the last raw consumers — `advanceSessionDrains` mutations (`completeDrain` off
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
prove the new test has TEETH: temporarily break the refresh (e.g. delete the `markClosed`/ApplyPatch
line) and confirm the sibling test FAILS, then restore.** **Run oracles under an isolated GOCACHE.**
`git checkout go.sum` after. Commit AND push `--no-verify`. Trailer:
`Co-Authored-By: Claude Opus 4.8 (1M context) <noreply@anthropic.com>`.
Never `tmux kill-server` / `go clean -cache` (`-testcache` ok); gascity Dolt LOCAL-ONLY (no
`bd dolt push`). #3839 stays DRAFT. Quote grep globs (`--include='*.go'`). Mapping agents
have read the WRONG worktree (`.worktrees/pack-crud`) — pin HEAD, verify line numbers.
Update the handoff + STEP6-DESIGN check boxes + memory (`infra-beads-decoupling-plan.md`).

---
