# S19 Spec — Level-Triggered Session Convergence

**Status:** spec (synthesizes spike branches `simplify/s19-a`, `simplify/s19-b`,
`simplify/s19-c` into one staged plan)
**Bug family:** #3872 (all three incidents), #3849, #2073, #2285, #2083, #2112, #1029
**Scope:** `cmd/gc/{session_reconciler,session_reconcile,session_lifecycle_parallel,
session_beads,adoption_barrier,build_desired_state,template_resolve}.go` +
`internal/session` projection.

This document is a **correctness contract**. Every stage below must build, pass
the full gate set, and be behavior-preserving except where a stage explicitly
lists an activated bug fix. The complexity payoff (deleting the per-path
edge-triggered stamping) is deliberately staged LAST (Stage 6) — no stage before
it may claim a simplification win.

---

## 1. Target design: observe → diff → act (builds on s19-b)

The reconciler's Phase 1 (`session_reconciler.go:1435`) today applies session
side effects edge-triggered and path-dependent: fresh spawn, adoption, resume,
drift-relaunch, and rollback each stamp their own subset of identity/priming
metadata. The target replaces the *decision* with a pure per-tick core; the
reconciler keeps ownership of all I/O.

```
observe : build, once per session per tick,
            durableFacts   — from persisted session-bead metadata only
            runtimeFacts   — from one already-performed runtime probe
diff    : deriveConvergeActions(durable, runtime) -> ordered idempotent []action
act     : reconciler executes actions under the EXISTING alias/identifier locks,
          through sessFront (bead writes) and worker.Handle / runtime.Provider.Nudge
          (runtime effects). No new lock, no new I/O path.
```

### 1.1 Fact vocabulary (normative)

`durableFacts` — each field maps to exactly ONE bead-metadata key (no
precedence ladder):

| field               | key                              | meaning |
|---------------------|----------------------------------|---------|
| `startedConfigHash` | `started_config_hash`            | `""` = no durable record of a completed start |
| `canonicalIdentity` | `canonical_instance_name` + `canonical_pool_slot` (s19-c record, decoded by `session.CanonicalIdentityFromMetadata`) | absent ⇒ heal |
| `primedAt`          | `primed_at`                      | non-empty = delivery **confirmed** durably |
| `primingAttemptedAt`| `priming_attempted_at`           | non-empty = an attempt was durably declared (see §2) |
| `primedPromptHash`  | `prompt_hash`                    | hash of the rendered prompt the markers refer to |
| `promptConfigured`  | (derived from resolved template `tp.Prompt != ""`) | false ⇒ priming actions never emitted |
| `absent`            | bead closed / `state=failed_create` rollback landed | durable intent says "not running" |

`runtimeFacts` — observed only, never inferred; the core performs **zero I/O**:

| field        | source | meaning |
|--------------|--------|---------|
| `observed`   | whether the runtime was actually probed this tick | false ⇒ NO live-only or start action may be emitted (s19-c reviewer item 3, adopted) |
| `live`       | provider probe | matching runtime session alive |
| `transcript` | tri-state `unknown/present/absent` from `staleResumeKeyProbe` | `unknown` MUST reproduce the durable-only legacy decision exactly |
| `primedEnv`  | `GC_STARTUP_PROMPT_DELIVERED` in live runtime env | prompt landed this runtime; may not be durably recorded yet |

### 1.2 Action vocabulary (normative, ordered)

The action list is ordered and the order is load-bearing:

1. `actionRollbackRuntimeToAbsent` — dominates everything. `durable.absent &&
   runtime.observed && runtime.live` ⇒ this is the ONLY action (#2073 pane leak).
   Never emitted when `!runtime.observed`.
2. `actionStampCanonicalIdentity` — canonical record absent ⇒ stamp it (the
   heal that collapses the read ladders). Runs before priming so downstream
   readers agree on identity first.
3. `actionStampPrimedFromRuntime` — `runtime.live && primedEnv && primedAt==""`
   ⇒ persist the confirmation with NO nudge (the runtime already got the prompt
   at launch; we only heal the durable world).
4. `actionAttemptPrime` — the two-phase delivery of §2. Emitted only when
   `runtime.observed && runtime.live && durable.promptConfigured &&
   primedAt=="" && !primedEnv && attemptEligible(durable)`.

`deriveFirstStart(startedConfigHash, transcript)` remains a separate pure
predicate feeding the EXISTING launch path (it does not become a converge
action in cmd/gc stages — see §5 decision D1):

```
firstStart ⇔ startedConfigHash == ""            (legacy signal, exact)
           ∨ transcript == sessTranscriptAbsent (the #3849 fix — active only
                                                 when a real probe is passed)
```

`transcript == sessTranscriptUnknown` reproduces the legacy durable-only
behavior byte-for-byte. This is the **legacy-parity pin** and it is a permanent
test, not a temporary scaffold (§7.2).

### 1.3 Purity contract

- The diff core (`deriveConvergeActions`, `deriveFirstStart`,
  `desiredSessionIdentity`, `promptDelivery`, `session.Converge` in the
  endgame) performs no I/O, reads no globals, no clocks (time enters as a fact),
  and is total: every `(durable × runtime)` combination returns a defined list.
- Idempotence: re-running the diff after its actions have durably landed yields
  the empty list. This is a mandatory table test for every action.
- Zero role names, zero template-content judgment: Go routes an
  already-rendered prompt and tracks delivered/not-delivered; the prompt's
  content and any "should this agent do X" judgment stays in templates.

---

## 2. THE FIX: stamp-vs-nudge ordering (priming_attempted + confirmation)

### 2.1 The bug being fixed

s19-b as spiked stamps `primed_at` BEFORE delivering the nudge. That is
**at-most-once** delivery: a crash (or store/provider failure) between the
stamp and the nudge leaves a durable record claiming "primed" for a session
that never received its startup prompt — and because the converge pass is
keyed off `primed_at`, no observer will EVER re-deliver. The startup prompt is
permanently lost (worse than today's #3872-3, where at least nothing claims it
was delivered). The naive inversion (nudge first, stamp after) is at-least-once
but unbounded: a persistent stamp failure re-nudges every tick forever.

### 2.2 The contract: two markers, write-ahead attempt, explicit confirmation

Split the single marker into an **attempt declaration** and a **delivery
confirmation**:

| marker | key | stamped | meaning |
|--------|-----|---------|---------|
| attempt | `priming_attempted_at` (RFC3339) + `prompt_hash` | durably committed **BEFORE** any delivery I/O (write-ahead intent) | "a delivery was initiated for this prompt at this time" |
| confirmation | `primed_at` (RFC3339) + `prompt_hash` | durably committed **AFTER** the delivery mechanism reported success | "the prompt reached a first-turn delivery mechanism" |

**Confirmation signals (exhaustive):**

1. **Launch path** — `promptDelivery(...)` returned `Delivered==true` AND the
   provider start succeeded: confirmation is folded into the SAME atomic
   `CommitStartedPatch` batch that stamps `started_config_hash`
   (`internal/session/lifecycle_transition.go:261`; call sites
   `session_lifecycle_parallel.go:1942,2151`). Priming thereby inherits the
   start path's existing crash semantics: a crash between provider start and
   commit already re-runs create recovery today, so delivery is at-least-once
   with the duplicate bounded by the existing pending-create machinery.
2. **Awake-scan re-delivery** — `worker.Handle.Nudge()` (routing to
   `runtime.Provider.Nudge`) returned nil: stamp `primed_at` immediately after.

**Re-attempt eligibility (pure, part of the diff core):**

```
attemptEligible(d) ⇔ d.primingAttemptedAt == ""
                   ∨ now − d.primingAttemptedAt ≥ primeReattemptInterval
                   ∨ d.primedPromptHash ≠ hash(current rendered prompt)   // stale attempt for an old prompt
```

`now` enters `durableFacts` as a fact (the observe step captures it);
`primeReattemptInterval` is a single named constant (default: 2× the reconcile
tick interval, minimum 60s). This is mechanical delivery backoff — transport,
not judgment.

**Marker lifecycle:** both markers (and `prompt_hash`) are cleared exactly
where `started_config_hash` is cleared today — `clearStaleResumeKeyMetadata`
(`session_lifecycle_parallel.go:935` region) and the pending-create reset at
`:1868` — plus on `wake_mode=fresh`. A fresh incarnation re-primes; a resumed
incarnation does not.

### 2.3 Crash-window analysis (must hold at every stage after activation)

| crash point | durable state next tick | converge behavior | outcome |
|---|---|---|---|
| after attempt stamp, before nudge | attempted, unconfirmed | re-attempt after `primeReattemptInterval` | **prompt not lost** (fixes the s19-b flaw) |
| after nudge success, before confirm stamp | attempted, unconfirmed | re-attempt after interval ⇒ one duplicate | ≤1 duplicate per crash, per interval |
| attempt-stamp write fails | unattempted | retry next tick, no nudge was sent | no loss, no duplicate |
| launch delivers prompt, crash before CommitStartedPatch | no hash, no markers | create recovery re-runs launch | existing start semantics, unchanged |
| runtime has `primedEnv` but bead lost markers | live+primedEnv, unconfirmed | `actionStampPrimedFromRuntime` (no nudge) | heal without duplicate |

### 2.4 Priming invariants (normative)

- **P1 (no loss / level trigger):** a live session whose template carries a
  startup prompt and whose `primed_at` is empty receives a delivery attempt
  within `primeReattemptInterval + one tick`, regardless of arrival path or
  crash history.
- **P2 (bounded duplicates):** at most one delivery per session per
  `primeReattemptInterval`; in a crash-free run, exactly one delivery total.
- **P3 (write-ahead ordering):** no nudge I/O is issued before the attempt
  marker's store write has returned success. Enforced by an order-recording
  fake in tests (§7.3).
- **P4 (confirmation honesty):** `primed_at` is written only in the same batch
  as a successful start commit or after a nil-error Nudge. Nothing else may
  write it.
- **P5:** neither marker is ever written when `tp.Prompt` is empty.

---

## 3. Exact mapping: current behavior → new form

| # | Current (file:line, origin/main) | New form | Stage |
|---|---|---|---|
| M1 | `firstStart := session.Metadata["started_config_hash"] == ""` (`session_lifecycle_parallel.go:926`) | `deriveFirstStart(hash, transcriptState)`; `sessTranscriptUnknown` at first (parity), real probe from `staleResumeKeyProbe` later | 1 (pin), 4 (fix #3849) |
| M2 | Inline prompt routing across launch / ACP / prompt-mode branches (`template_resolve.go`, marker `startupPromptDeliveredEnv=:37`) | one pure `promptDelivery(prompt,isACP,rp,nudge) -> {PromptSuffix,PromptFlag,Nudge,Delivered}` (s19-a), byte-identical wiring | 1 |
| M3 | Priming recorded ONLY as runtime env `GC_STARTUP_PROMPT_DELIVERED` — undetectable "live but never primed" (#3872-3) | two durable markers per §2, confirmed in `CommitStartedPatch` batch; awake-scan re-delivery via existing `runtime.Provider.Nudge` | 2 (write), 4 (re-deliver) |
| M4 | `restartPromptNudge` special case + env delete/set choreography (`session_lifecycle_parallel.go:960-977,1359`) | subsumed by `promptDelivery` + durable markers; deleted | 6 |
| M5 | Adoption barrier hand-rolled 6-key identity map omitting alias (`adoption_barrier.go:47-49` region; #3872-1) | `desiredSessionIdentity(inputs)` (s19-a) already wired; alias added to the derivation as a flagged behavior fix | 1 (wired), 4 (alias fix) |
| M6 | Identity re-inferred per tick via ladders: `existingPoolSlotWithConfig` (`build_desired_state.go:3397`), `resolvePersistedPoolIdentitySlot` (`:3323`), `sessionBeadQualifiedName` 3 legacy cases (`:3188`), 40 `UsesCanonicalSingletonPoolIdentity` sites | ONE durable record `canonical_instance_name`/`canonical_pool_slot` (s19-c `CanonicalIdentity`), stamped at create, healed by `actionStampCanonicalIdentity`; ladders become the quarantined legacy fallback decoder consulted only when `!Present` | 2 (write), 5 (read cutover), 6 (delete) |
| M7 | `syncSessionBeads` create stamping (`session_beads.go` create block) + ~400-line backfill machine + adoption map — 3+ divergent identity derivations (#2285/#2083) | all call `desiredSessionIdentity`; backfill machine collapses into "diff durable record vs derivation, patch the difference" | 1 (create/adopt), 6 (backfill deleted) |
| M8 | `rollbackPendingCreate` (`session_lifecycle_parallel.go:2292`) rolls back the bead but leaks the tmux+CLI pane (#2073) | rollback marks `durable.absent`; `actionRollbackRuntimeToAbsent` tears the runtime down on the next tick through `worker.Handle` | 3 (derive), 4 (fix #2073) |
| M9 | Post-helper stamping of `live_hash`/`session_origin`/`synced_at`, pool_slot after base resolve | **stays outside the identity derivation, documented:** these are start-outcome/audit facts owned by `CommitStartedPatch` and sync bookkeeping, not identity facts (resolves the s19-a reviewer nit by documentation, not folding) | 1 |
| M10 | `TrimSpace` divergence: spiked `deriveFirstStart` trims, legacy `:926` compares `== ""` exactly | parity mode keeps EXACT `== ""`; TrimSpace semantics adopted deliberately at Stage 4 with its own test (whitespace-only hash ⇒ unstarted) | 1 (exact), 4 (trim) |

---

## 4. Invariants that MUST hold (every stage)

Repo invariants (violating any fails CI or review):

- **R1 — zero hardcoded roles.** No role name in any new Go. Derivations are
  config/template-driven only.
- **R2 — typed wire.** No hand-written JSON, `map[string]any`, or
  `json.RawMessage` on HTTP/SSE paths. `Info.CanonicalIdentity` is
  internal-only and MUST NOT appear on the wire (s19-c already documents it as
  not folded by ApplyPatch; `TestInfoApplyPatchMatchesReprojection` stays green).
- **R3 — typed events.** Any new event type gets `events.RegisterPayload`
  (`TestEveryKnownEventTypeHasRegisteredPayload`). This spec adds NO new event
  types through Stage 5; if Stage 6 adds a converge-observability event it
  registers a payload.
- **R4 — worker boundary.** All runtime effects (start, nudge, teardown) go
  through `worker.Handle` / existing provider plumbing; no new
  `session.NewManager(`/`worker.SessionHandle`/`sessionlog` imports in
  non-test `cmd/gc` (`TestGCNonTestFilesStayOnWorkerBoundary`). Bead writes go
  through `sessFront` (the session front door).
- **R5 — no upward imports.** `internal/session` additions (s19-c record,
  eventual converge core) import nothing from `cmd/gc` or higher layers.
- **R6 — projections.** `cmd/gc` and `internal/api` remain projections; the
  diff core added to `cmd/gc` in Stages 1–5 is pure decision code and migrates
  to `internal/session` in Stage 7.
- **R7 — config.Agent field-sync.** This spec adds no `config.Agent` field. If
  any knob is ever promoted to config, it must ride `AgentPatch`/
  `AgentOverride`/apply functions/`poolAgents` deep-copy per `TestAgentFieldSync`.

Design invariants (new, enforced by the test plan):

- **C1 — parity pin.** In every behavior-preserving stage, the derived decision
  for every session equals the legacy decision byte-for-byte
  (`transcript=unknown`, shadow compare in Stage 3). A divergence is a bug in
  the derivation, never "close enough."
- **C2 — idempotence.** `derive*(d, r)` after its actions land returns ∅.
- **C3 — single-writer for `started_config_hash`.** The identity heal and the
  priming markers NEVER write `started_config_hash`; it remains owned by
  `CommitStartedPatch` and its existing clear sites.
- **C4 — identity agreement.** `canonicalIdentity.Present ⇒` every reader
  resolves the same qualified instance name + slot (one field read; the ladders
  are unreachable for that session).
- **C5 — observed gating.** No start, teardown, or nudge action derives from
  unobserved runtime facts (`observed==false` ⇒ only durable-only heals are
  permitted, and in cmd/gc stages: none).
- **C6 — lock discipline.** Actions execute under the existing
  `WithCitySessionAliasLock`/identifier locks at the same acquisition points
  Phase 1 uses today; the diff core itself takes no locks. Alias-lock traffic
  must not increase in the steady state (actions are empty for a converged
  session — measure in Stage 3 shadow mode).
- **C7 — rollback dominance.** `durable.absent` suppresses all identity/priming
  actions; the only permitted action is runtime teardown, and only when
  observed-live.
- **P1–P5** from §2.4.

---

## 5. Settled design decisions (from the reviewer roadmap)

- **D1 — who restarts dead-with-transcript sessions (s19-c item 1):** the
  existing wake/relaunch machinery, unchanged. Through Stage 6 the converge
  core NEVER emits a start action; `deriveFirstStart` only classifies
  fresh-vs-resume for the launch path that already decided to launch.
  `ConvergeStartFresh` (s19-c) is deferred to Stage 7 and requires its own spec
  addendum.
- **D2 — StartFresh vs live pane (s19-c item 2):** moot until Stage 7 by D1.
  For Stage 7: teardown-before-start, i.e. a fresh start on a session with an
  observed-live runtime first emits `runtime-to-absent`, then starts — never
  both in one tick.
- **D3 — empty-hash actions gated on observed (s19-c item 3):** adopted as C5.
- **D4 — TrimSpace (s19-b item 2):** M10. Exact `== ""` until Stage 4; then
  TrimSpace with a dedicated test.
- **D5 — post-helper keys (s19-a nit):** M9 — documented as non-identity facts,
  not folded.
- **D6 — duplicate-delivery tolerance:** one duplicate per crash per
  `primeReattemptInterval` is accepted; permanent loss is not. (Inversion of
  the s19-b spike's trade.)
- **D7 — parity mechanism:** shadow compare + permanent pin tests; NO config
  flag (avoids `config.Agent` sync surface and genschema churn). Stage flips
  are code changes reviewed as such.

---

## 6. Migration plan — staged, each stage builds and lands independently

Every stage: `make test` (or `test-fast-parallel`) green, `go vet ./...`
clean, `.githooks/pre-commit` run, and the stage's own tests. Stages 1–3 are
behavior-preserving by C1. Stage 4 activates named bug fixes one commit each.

### Stage 1 — the smallest coherent building slice (BUILD)

Unify the s19-a and s19-b pure cores into one landable slice, with the §2
priming vocabulary corrected from the start:

1. Land `cmd/gc/session_identity.go` (`desiredSessionIdentity`) and
   `cmd/gc/prompt_delivery.go` (`promptDelivery`) exactly as spiked in s19-a,
   wired byte-identically into `syncSessionBeads` create, the adoption
   barrier, and `templateParamsToConfig`. (Preserves the alias omission
   as-is — the fix is Stage 4.)
2. Land `cmd/gc/session_level_converge.go` from s19-b **amended**: replace the
   spike's `actionStampPrimed`-before-`actionRedeliverPrompt` pair with the §2
   two-phase vocabulary (`actionStampPrimedFromRuntime`, `actionAttemptPrime`
   with `attemptEligible`); add `now` to `durableFacts`; keep
   `deriveFirstStart` with exact `== ""` (M10) and the
   `sessTranscriptUnknown` parity pin.
3. Wire `session_lifecycle_parallel.go:926` through
   `deriveFirstStart(hash, sessTranscriptUnknown)` — behavior-preserving by the
   pin test.
4. Truth-table tests for both cores (§7.1) + parity pin tests (§7.2).

Nothing reads the new action list yet; `deriveConvergeActions` is exercised
only by tests. Net LOC goes UP; that is expected and is not a win claim.

### Stage 2 — durable schema, write-only (behavior-preserving)

1. Land `internal/session/canonical_identity.go` from s19-c
   (`CanonicalIdentity`, `CanonicalIdentityFromMetadata`, the two metadata
   keys) + the additive `Info.CanonicalIdentity` projection in
   `InfoFromPersistedBead` (R2 wire exclusion documented, ApplyPatch oracle
   green).
2. Stamp `canonical_instance_name`/`canonical_pool_slot` at create and
   adoption from the already-wired `desiredSessionIdentity` inputs.
3. Extend the `CommitStartedPatch` batch (new optional input fields) to stamp
   `primed_at` + `prompt_hash` when the launch path delivered
   (`promptDelivery(...).Delivered && start succeeded`), and clear all three
   priming keys at the existing `started_config_hash` clear sites (M3, §2.2
   lifecycle).
4. NO reader changes: every ladder and every priming decision still runs on
   legacy signals. Writes are additive metadata invisible to all decision paths.

### Stage 3 — act loop in shadow, then double-write (behavior-preserving)

1. Build `durableFacts`/`runtimeFacts` in Phase 1 from the per-session typed
   snapshot + the tick's existing runtime observation (no new probes;
   `transcript=unknown`).
2. **Shadow mode:** compute `deriveConvergeActions` per session, compare
   against what the legacy paths actually did this tick, log divergences
   (stderr + trace, no new event type). Soak until divergence rate is zero on
   real cities; every divergence is fixed in the derivation or reclassified as
   a legacy bug with a bead filed.
3. Flip the executor on: actions drive the identity-heal and priming stamps
   under the existing locks, while legacy per-path stamping REMAINS in place
   (double-write; all writes idempotent, C2 makes the second writer a no-op).
   `actionAttemptPrime`'s nudge stays DISABLED (no runtime effects yet);
   `actionRollbackRuntimeToAbsent` derived but not executed.
4. Measure C6: alias-lock acquisitions per tick before/after must be equal in
   steady state.

### Stage 4 — activate the bug fixes (one commit per fix, each with a test)

1. **#3849:** pass real `sessTranscriptState` from `staleResumeKeyProbe` into
   `deriveFirstStart` at `:926` (probe already runs just above; no new I/O).
   Adopt TrimSpace (D4) in the same commit.
2. **#3872-1:** add alias to `desiredSessionIdentity` adoption inputs.
3. **#3872-3:** enable `actionAttemptPrime` execution on the awake scan: stamp
   attempt (write-ahead, P3) → `worker.Handle.Nudge` → stamp confirmation.
4. **#2073:** execute `actionRollbackRuntimeToAbsent` through `worker.Handle`
   teardown; `rollbackPendingCreate` itself stops being expected to leave a
   pane behind (its bead-side semantics unchanged).

### Stage 5 — read cutover (behavior-preserving for recorded sessions)

Route identity reads through `Info.CanonicalIdentity` when `Present`;
quarantine the ladders (`existingPoolSlotWithConfig`,
`resolvePersistedPoolIdentitySlot`, `sessionBeadQualifiedName` legacy cases)
behind ONE fallback decoder consulted only when `!Present`, immediately
followed by `actionStampCanonicalIdentity` healing the record. Parity: for
every session, ladder result == record (asserted in shadow during Stage 3+).

### Stage 6 — the deletion payoff (the actual simplification)

Only now delete: the per-path edge-triggered stamping in create/adopt/resume/
drift paths; the ~400-line backfill machine and the adoption identity map
(both become "record vs derivation" diffs); `restartPromptNudge` and the env
delete/set choreography (M4); the ladder bodies (fallback decoder keeps a
minimal legacy parse); the `aliasGuardedBatch`/`mergeAliasGuardedBatch`
choreography where the act loop has subsumed it; shrink the 40
`UsesCanonicalSingletonPoolIdentity` sites to the derivation + decoder.
Target: net −300 to −400 LOC vs pre-S19. S19 is "done" only when this lands.

### Stage 7 — layering endgame (s19-c, separate follow-up)

Move the converge core into `internal/session` (`session.Converge`,
`DurableFacts`/`ConvergeRuntimeFacts`), including start-decision ownership
(D1/D2 addendum required), leaving cmd/gc a thin driver. Out of scope for the
S19 correctness contract; gated on Stage 6 having proven the shape.

---

## 7. Test plan

### 7.1 Truth-table unit tests (pure core; extend the spike tests)

Full cross-product tables for `deriveConvergeActions` covering: rollback
dominance (C7, incl. `observed=false` ⇒ ∅); identity heal on/off; every
priming row of §2.3's table (attempt-eligible, attempt-fresh backoff,
prompt-hash-changed re-eligibility, `primedEnv` heal-without-nudge,
`promptConfigured=false` ⇒ never); idempotence (C2) asserted for EVERY row by
re-deriving with the row's actions applied to the facts.

### 7.2 Legacy-parity pins (permanent)

- `deriveFirstStart(h, sessTranscriptUnknown) == (h == "")` for h ∈
  {"", "x", " ", "\t"} — exact until Stage 4, then the trim variant pinned.
- `promptDelivery` byte-parity vectors for every branch (empty, ACP, none,
  arg, flag±flag, nil provider) — carried from s19-a.
- `desiredSessionIdentity` golden maps for create vs adoption shapes,
  updated (not deleted) when the Stage-4 alias fix lands.

### 7.3 Crash-window / ordering tests (P1–P4)

Fake sessFront + fake worker.Handle that record operation ORDER and can be
programmed to fail at each step: assert attempt-stamp precedes nudge (P3);
assert crash-after-stamp leads to re-attempt after `primeReattemptInterval`
(P1, no loss); assert crash-after-nudge yields exactly one duplicate then
convergence (P2); assert `primed_at` written only via the two confirmation
paths (P4). Clock injected via the facts.

### 7.4 Shadow-parity harness (Stage 3)

Reconciler-level test that runs Phase 1 over fixture cities (fresh spawn,
adoption, resume, drift relaunch, rollback) and asserts zero derived-vs-legacy
divergences; plus the lock-count regression check (C6).

### 7.5 Regression tests for the activated fixes (Stage 4)

- #3849: hash stamped + transcript deleted ⇒ next launch uses `--session-id`
  (fresh), no crash loop (integration, `//go:build integration`).
- #3872-1: adopted session's identity metadata includes alias; no duplicate
  spawn on the following tick.
- #3872-3: live session with empty `primed_at` and prompt-bearing template
  receives exactly one Nudge; marker confirmed; second tick emits nothing.
- #2073: rollbackPendingCreate followed by a tick ⇒ runtime pane gone (tmux
  integration, city-scoped socket only).

### 7.6 Invariant guards (existing CI, must stay green)

`TestGCNonTestFilesStayOnWorkerBoundary`, `TestInfoApplyPatchMatchesReprojection`,
`TestOpenAPISpecInSync` (no wire change expected — the run proves it),
`TestEveryKnownEventTypeHasRegisteredPayload`, `TestAgentFieldSync`.

---

## 8. Non-goals

- No new config surface, no new event types (through Stage 5), no wire
  changes, no changes to alias-uniqueness locking, no changes to conflict/
  deferred-singleton machinery semantics (shadow mode must prove
  non-interference before the Stage 3 flip).
- No converge-owned session starts before Stage 7 (D1).
- gc doctor "live-but-unprimed" surfacing (s19-a phase 2 idea) is subsumed by
  the durable markers — any observer can now compute it; a doctor check may be
  added opportunistically but is not part of this contract.
