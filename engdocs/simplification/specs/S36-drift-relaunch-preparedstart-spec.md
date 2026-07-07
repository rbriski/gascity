# S36 — Route drift-Relaunch through buildPreparedStart

**Item:** S36 (backlog.json), approach A. **Bug family:** #3872 (durable-vs-runtime launch divergence).
**Files:** `cmd/gc/session_reconciler.go`, `cmd/gc/session_lifecycle_parallel.go`, `cmd/gc/session_reconciler_test.go`.

## 1. Problem statement (current behavior, precise)

The launch-only-drift branch of the session reconciler executes a config that was
built for **fingerprinting**, not for **execution**:

- `session_reconciler.go:2523` — `agentCfg := sessionCoreConfigForHash(tp, *session)`
  (`session_hash.go:15`). This is `templateParamsToConfig(tp)` + durable
  `template_overrides` and **nothing else**: full startup prompt still in
  `PromptSuffix`, `Command` is the bare provider command (no
  `--resume`/`--session-id`/fork rewrite), no runtime env
  (`GC_SESSION_ID`, instance token, `GC_PROVIDER`, trigger-bead env), no task
  work-dir override, no stale-resume-key probe.
- `session_reconciler.go:2623` and `:2694` — both call sites pass this hash-form
  `agentCfg` into `relaunchAgentForLaunchDrift(...)` (`:4828`), which hands it
  **verbatim** to `r.Relaunch(ctx, name, agentCfg)` (`:4848`) and then rebaselines
  hashes by recomputing `runtime.{Core,Provision,Launch}Fingerprint(agentCfg)` in
  `rebaselineLaunchDriftHashesWithBatch` (`:4898`).

Only `buildPreparedStartWithWorkDirResolver` (`session_lifecycle_parallel.go:828`)
knows how to turn a durable config into an executable one: schema + dispatch
option overrides, work-dir resolution + PreStart retarget, stale-resume-key
probe/clear, session-key mint, fork validation, `resolveSessionCommand` rewrite
(`:958-959`), the `!firstStart` prompt-strip + restart-nudge +
`startupPromptDeliveredEnv` block (`:962-975`), initial-message handling,
instance-token mint, and runtime env merge. Consequence of bypassing it: a
drift-relaunched agent starts an **untracked conversation** (no `--resume
<session_key>`), the durable `session_key` still points at the old transcript
(a later resume-based wake resumes the WRONG conversation), and the full
startup prompt is re-sent on relaunch.

`recoverRunningPendingCreate` (`session_lifecycle_parallel.go:2118`, call at
`:2129`) is the proven in-reconciler pattern: it calls
`buildPreparedStart(startCandidate{session, tp}, cfg, store)` and commits
`prepared.coreHash/liveHash/provisionHash/launchHash` + breakdown, folding
buildPreparedStart's persisted residue back onto the typed snapshot.

## 2. Target design

**One derivation of "how to launch this session."** The drift branch keeps
using `sessionCoreConfigForHash` for the drift **comparison** (unchanged), but
the **executed** config and the **rebaselined** hashes both come from
`buildPreparedStart`.

### 2.1 New shape of `relaunchAgentForLaunchDrift`

Signature grows the inputs needed to build and validate a prepared start; the
hash-form `agentCfg` parameter is dropped from the exec path (it survives only
as `currentHash`/`storedProvision`/`storedLaunch` scalars, already computed by
the caller):

```go
func relaunchAgentForLaunchDrift(
    ctx context.Context,
    sp runtime.Provider,
    sessFront *sessionpkg.Store,
    session *beads.Bead,
    name string,
    tp TemplateParams,
    cityPath string,
    cfg *config.City,
    store beads.Store,
    storedHash, currentHash string,          // core baseline vs hash-form current
    storedProvisionHash, storedLaunchHash string, // partition baselines
    driftedFields []string,
    rec events.Recorder,
    trace *sessionReconcilerTraceCycle,
    stdout, stderr io.Writer,
) (relaunched bool, foldBatch map[string]string)
```

Body, in order:

1. **Provider gate (unchanged, first — no side effects before it):**
   `r, ok := sp.(runtime.RelaunchProvider)`; `!ok` → `return false, nil`.
2. **Prepare:** `prepared, err := buildPreparedStartWithWorkDirResolver(
   startCandidate{session: session, tp: tp}, cityPath, cfg, store, nil)`.
   `cityPath` is passed (both call sites have it in scope) so
   `session.Metadata["work_dir"]` resolves against the city exactly as the
   fresh-start path (`prepareStartCandidateForCity:783`) does; `nil`
   work-dir resolver is correct because both call sites sit behind the
   no-open-assigned-work / not-active deferral guards. On error: log to
   stderr, `return false, relaunchPrepareResidueFold(session)` — caller falls
   through to the existing full restart (which fails loud itself if the
   config is genuinely unlaunchable). NOTE: deliberately `buildPreparedStart*`,
   NOT `prepareStartCandidateForCity` — no `preWakeCommit`, no named-template
   refresh; the session is alive, not waking.
3. **Precondition re-check (the anti-skew gate):** relaunch only if
   `prepared.coreHash == currentHash && prepared.provisionHash ==
   storedProvisionHash && prepared.launchHash != storedLaunchHash`.
   Any mismatch means the launch-only-drift verdict computed from the
   hash-form config does not hold for the prepared config (concurrent bead
   mutation, or a derivation divergence between
   `applyTemplateOverridesToConfigInfo` and
   `applySchemaOptionOverridesForLaunch`) → log, `return false,
   relaunchPrepareResidueFold(session)` → **full restart**, never a relaunch
   against an unverified baseline.
4. **Exec:** `r.Relaunch(ctx, name, prepared.cfg)`. Error handling unchanged
   (ErrRelaunchUnsupported silent, others logged; both `return false, ...`).
5. **Rebaseline from prepared, not recomputed:**
   `rebaselineLaunchDriftHashesWithBatch` changes to accept explicit values
   `(coreHash, provisionHash, launchHash string, breakdown runtime.BreakdownV1)`
   instead of recomputing `runtime.*Fingerprint(agentCfg)`; called with
   `prepared.coreHash / prepared.provisionHash / prepared.launchHash /
   prepared.coreBreakdown`. Patch keys unchanged: `started_config_hash`,
   `started_provision_hash`, `started_launch_hash`, `core_hash_breakdown`.
   `started_live_hash`/`live_hash` remain DELIBERATELY untouched (relaunch
   does not reliably re-apply SessionLive; the live-drift clause self-heals
   next tick — existing comment at `:4858-4865` stays valid; `prepared.liveHash`
   is ignored here).
6. **Fold residue:** the returned batch = rebaseline patch, augmented with
   buildPreparedStart's persisted residue exactly as
   `recoverRunningPendingCreate` does (`:2185-2192` and
   `pendingCreateResidueFold`): `instance_token` if minted, and the current
   `started_config_hash` value when the stale-resume guard fired
   (`clearStaleResumeKeyMetadata` writes the raw bead + store outside any
   batch). On the success path the rebaseline overwrites
   `started_config_hash` anyway; on the failure paths (steps 2-4) a small
   `relaunchPrepareResidueFold(session)` returns
   `{"started_config_hash": session.Metadata["started_config_hash"],
   "instance_token": ...if minted}` so the caller's typed snapshot never
   diverges from the raw bead buildPreparedStart mutated.
   Trace/event/stdout emissions unchanged.

### 2.2 Caller changes (`session_reconciler.go:2623`, `:2694`)

Both sites pass `tp, cityPath, cfg, store, storedProvision, storedLaunch`
instead of `agentCfg`, and fold the returned batch **unconditionally**
(`ApplyPatch(nil)` is a no-op) so the failure-path residue also lands on the
snapshot:

```go
relaunched, launchBatch := relaunchAgentForLaunchDrift(ctx, sp, sessFront, session, name,
    tp, cityPath, cfg, store, storedHash, currentHash, storedProvision, storedLaunch,
    driftedFields, rec, trace, stdout, stderr)
infoByID[session.ID] = infoByID[session.ID].ApplyPatch(launchBatch)
if relaunched {
    continue
}
```

The `launchOnlyDrift` pre-screen at `:2565-2569` stays exactly as is — it is a
cheap filter over the hash-form config; the authoritative check is step 3.

### 2.3 What does NOT change

- `sessionCoreConfigForHash` reverts to hashing-only use on this path; drift
  COMPARISON (`:2523-2524`, `:2553-2554` breakdown diagnostics, live-drift at
  `:2732`) is untouched.
- All deferral guards (attached / named-active / pending-interaction /
  open-assigned-work) run before the relaunch branch — untouched.
- `runtime.RelaunchProvider` seam and provider-side contract — untouched.
- Other `sessionCoreConfigForHash` call sites (`:2809` asleep-named repair,
  `:4202`, `soft_reload.go:128`, `session_beads.go:1703`) — out of scope
  (audit note: `:4202` should be checked for the same category confusion in a
  follow-up; do not touch here).

## 3. Exact mapping: current → new

| Current | New |
|---|---|
| `r.Relaunch(ctx, name, agentCfg)` where `agentCfg = sessionCoreConfigForHash(tp, *session)` | `r.Relaunch(ctx, name, prepared.cfg)` where `prepared = buildPreparedStartWithWorkDirResolver(startCandidate{session,tp}, cityPath, cfg, store, nil)` |
| `Command` = bare provider command | `Command` = `resolveSessionCommand(...)` output: `--resume <session_key>` on the normal alive path (`firstStart=false`), `--session-id <fresh>` after a stale-key clear, fork form never (firstStart=false unless stale-clear fired, then fork gates re-validate) |
| `PromptSuffix` = full startup prompt (re-sent on relaunch) | `PromptSuffix=""`, `PromptFlag=""`, `Nudge=restartPromptNudge(...)`, `startupPromptDeliveredEnv=1` (the `!firstStart` block, `:962-975`) |
| No runtime env | `RuntimeEnvWithSessionContext` + `GC_PROVIDER` + trigger-bead env + `SyncWorkDirEnv` merged |
| Rebaseline = `runtime.*Fingerprint(agentCfg)` recomputed inside `rebaselineLaunchDriftHashesWithBatch` | Rebaseline = `prepared.coreHash/provisionHash/launchHash` + `prepared.coreBreakdown`, passed in explicitly |
| No skew check between compared and executed config | Step-3 precondition re-check; mismatch → full restart |
| Fold only on `relaunched==true` | Fold returned batch unconditionally; failure paths return prepare-residue fold |

## 4. Invariants (MUST hold)

1. **Fingerprint stability / no re-drift loop.** The rebaselined
   `started_config_hash` MUST equal what the next tick's
   `runtime.CoreFingerprint(sessionCoreConfigForHash(tp', session'))` computes
   for unchanged config. Guaranteed by: (a) `prepared.coreHash` is computed
   from the durable config BEFORE dispatch `opt_` overrides, work-dir
   override, and the command rewrite (`session_lifecycle_parallel.go:848-856`);
   (b) the step-3 check pins `prepared.coreHash == currentHash` this tick;
   (c) the existing equality pin `per_dispatch_model_test.go:234` asserts the
   two derivations agree. Session-key mint does not perturb the hash (hashes
   are taken before the rewrite; `session_key` is not a fingerprint input).
2. **Hash-form purity.** Drift comparison still uses
   `sessionCoreConfigForHash` output only; `prepared.cfg` never feeds a
   fingerprint that is persisted as a baseline compared against hash-form
   output. Only `prepared.{core,provision,launch}Hash` (pre-rewrite) are
   persisted.
3. **Relaunch exec config ≡ start exec config.** For the same bead state,
   `prepared.cfg` handed to `Relaunch` MUST be the same value
   `buildPreparedStart` would hand a fresh/recover start (assertable via
   `runtime.Fake.LastRelaunchConfig`).
4. **Live-hash abstinence.** `started_live_hash`/`live_hash` are not written
   by the relaunch rebaseline (provider-independence of the live half).
5. **No relaunch on unverified baseline.** If the step-3 check fails, the
   session takes the full-restart path; there is no code path that calls
   `Relaunch` and then rebaselines to hashes ≠ the compared `currentHash`.
6. **Snapshot/raw-bead coherence.** Any raw-bead mutation buildPreparedStart
   persists (instance_token mint, stale-key `started_config_hash` clear) is
   folded onto `infoByID` in the same tick, on success AND failure paths
   (mirrors `pendingCreateResidueFold`, STEP6 write-returns-Info discipline).
7. **Guard ordering.** No buildPreparedStart side effect occurs before the
   `RelaunchProvider` type assertion; conjoined runtimes
   (subprocess/acp/t3bridge) exit with zero writes, byte-identical to today.
8. **Repo invariants.** Zero hardcoded roles; no wire/JSON changes (this is
   all in-process); `events.SessionUpdated`/`SessionDraining` emissions
   unchanged (already registered); worker boundary intact —
   `buildPreparedStart*` is in-package `cmd/gc`, no new
   `session.NewManager(`/`worker.SessionHandle`/`sessionlog` imports
   (`TestGCNonTestFilesStayOnWorkerBoundary` must stay green); no
   `config.Agent` field changes; no upward layer imports (uses existing
   `internal/runtime`, `internal/session`, `internal/beads` only).

## 5. Migration / staging plan (behavior-preserving steps)

Each phase compiles, passes `make test` + `go vet ./...`, and is separately
revertable.

- **Phase 1 (pure refactor, no behavior change):** change
  `rebaselineLaunchDriftHashesWithBatch` to take explicit
  `(coreHash, provisionHash, launchHash, breakdown)`; caller passes the
  recomputed `runtime.*Fingerprint(agentCfg)` values it computes today.
  Assert existing relaunch tests (`session_reconciler_test.go:7331` area)
  green.
- **Phase 2 (the cutover, TDD):** write the failing tests first (sec. 6, T1-T4),
  then thread `tp/cityPath/cfg/store/storedProvision/storedLaunch` into
  `relaunchAgentForLaunchDrift`, add the buildPreparedStart call + step-3
  re-check + prepared-hash rebaseline + residue folds, switch both call
  sites to unconditional fold. Delete the now-unused `agentCfg` parameter.
- **Phase 3 (verification sweep):** grep for remaining exec uses of
  `sessionCoreConfigForHash` output (`:4202` audit note), run
  `make test-fast-parallel`, confirm `per_dispatch_model_test.go` equality
  pin and the fork-launch reconciler tests
  (`session_reconciler_fork_launch_test.go`) unaffected.

Rollback: Phase 2 is a single commit touching two files; reverting restores
the hash-form exec path exactly.

## 6. Test plan

All in `cmd/gc/session_reconciler_test.go` unless noted, using the existing
reconciler test env + `runtime.Fake` (`LastRelaunchConfig`, `fake.go:761`).

- **T1 — resume rewrite reaches Relaunch (the #3872 kill-shot):** alive
  session with `session_key`, `started_config_hash`, and partition sub-hashes
  set; induce launch-only drift (change a launch-half field, e.g. the agent
  command flags); reconcile. Assert `LastRelaunchConfig(name).Command`
  contains `<ResumeFlag> <session_key>` and NOT the bare command; assert
  `PromptSuffix == ""` and `Env[startupPromptDeliveredEnv] == "1"` (no
  double prompt), `Nudge == restartPromptNudge(...)`.
- **T2 — relaunch config ≡ start config:** for the same bead, compute
  `buildPreparedStart(...)` directly in the test and assert
  `reflect.DeepEqual`-level equality (modulo nothing) with
  `LastRelaunchConfig(name)`.
- **T3 — no re-drift loop:** after the relaunch tick, run a second reconcile
  tick with unchanged config. Assert: no second `Relaunch` call (fake call
  count == 1), no drain, no `config_drift` trace decision, and
  `started_config_hash == prepared.coreHash == currentHash`,
  `started_launch_hash == prepared.launchHash`,
  `core_hash_breakdown == prepared.coreBreakdown` JSON; `started_live_hash`
  byte-identical to its pre-relaunch value.
- **T4 — skew falls back to full restart:** unit-test the step-3 gate: feed a
  prepared result whose `coreHash != currentHash` (via a seam or by
  table-testing the extracted precondition helper) and assert
  `(false, fold)` with zero `Relaunch` calls; integration variant: assert
  the session proceeds to drain/restart-in-place.
- **T5 — prepare error falls back:** make `buildPreparedStart` fail (e.g.
  session-key `SetMarker` failure via an erroring store wrapper, or a
  `gc.brain_parent_sid` bead with a fork-incapable provider) → no Relaunch,
  full-restart path taken, stderr log present.
- **T6 — stale resume key mid-drift:** delete the keyed transcript so
  `staleResumeKeyProbe` reports absent; assert relaunch command uses
  `SessionIDFlag <new_key>` (fresh conversation), a new `session_key` is
  persisted, and the tick's fold leaves the snapshot consistent (no wedge,
  next tick clean per T3).
- **T7 — provider gates unchanged:** non-RelaunchProvider provider and
  `ErrRelaunchUnsupported` both fall through to full restart with zero
  bead writes from the relaunch helper (assert no metadata delta beyond the
  restart path's own).
- **T8 — residue fold on failure paths:** with the stale-key clear having
  fired inside buildPreparedStart followed by a Relaunch error, assert
  `infoByID` fold reflects the cleared `started_config_hash` (typed snapshot
  == raw bead).
- **Regression suite:** `per_dispatch_model_test.go` (hash equality pin),
  `session_reconciler_fork_launch_test.go`, existing relaunch tests at
  `:7291-7500`, `TestGCNonTestFilesStayOnWorkerBoundary`, `make test`,
  `go vet ./...`.
