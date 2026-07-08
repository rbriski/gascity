# S19 Stage 3 — Shadow → Double-Write Rollout Plan (hardened v1)

_For Julian's sign-off. Stage 3 is the first stage where the new level-triggered
converge loop **executes** (Stage 1 merged, Stage 2 = PR #4064, write-only).
This version folds in the 5-angle Fable red-team council review (divergence
soundness, acceptance bar, double-write safety, kill-switch/rollback,
concurrency/determinism). All 5 angles returned blocking gaps against draft v0;
every gap is addressed below or listed in the blocking callout._

---

## Red-team hardenings applied (delta vs v0)

1. **Legacy capture rebuilt.** v0 sourced legacy actions from "the existing
   trace surface." Verified against origin/main: cmd/gc has exactly **3**
   `RecordMutation` sites, **none** in `session_beads.go` — the trace surface is
   structurally blind to the identity-stamp and priming writes Stage 3 compares,
   and it is env-disableable, budget-capped (4000 rec/cycle), and drop-prone
   under load. Replaced with an **in-process synchronous recorder** at every
   legacy write site of the compared keys + an **owned-key state-diff oracle**
   (3b). Trace is demoted to a reporting channel.
2. **Denominator-first acceptance.** "0 divergences" is accepted only with a
   proven comparison count: `converge.compare.sessions_evaluated` must match the
   expected session-tick volume; any tick with incomplete capture is
   **INCOMPARABLE**, never clean (3c).
3. **Evidence-based bar, not calendar.** 48h/3-cities becomes a **floor**; the
   gate is per-action-type comparison quotas (rule of three) reached via a
   mandatory **fault-injection runbook**, plus comparator self-tests
   (write-site completeness audit + seeded-mutation canary) that must pass
   before the soak clock starts (3c).
4. **Whitelist constrained.** Free-text "explained divergence" entries are
   banned. Expected-divergence classes are typed, machine-checked predicates
   verified per instance, fixture-backed, hard-capped, second-reviewer signed;
   any action-set divergence is a derivation bug fixed pre-flip or a named
   Stage-4 behavior change — never a pass-through (3c).
5. **Enabled set shrunk to the minimum.** v0 enabled "identity/canonical
   stamps, primed_at marker, routing-visibility registration." Routing-
   visibility registration **is not a derived action** (the Stage-1 enum has
   exactly 4 variants) — deleted. `actionStampPrimedFromRuntime` violates P4 as
   written, stamps an unverifiable prompt_hash, and diverges from legacy by
   construction — **moved to Stage 4** with the rest of the priming machine.
   Stage-3 enabled set = **{`actionStampCanonicalIdentity`}**, triple-gated
   (stability precondition, write-if-absent CAS under the held lock,
   continuous ladder==record parity assert). Enforced by an exhaustive switch
   + CI test: unclassified enum variants default to disabled-with-counter (3d).
6. **Comparator redesigned for real concurrency.** Per-tick set equality is
   structurally unreachable (edge-vs-level one-tick lag by design — spec M8;
   TOCTOU with mid-tick legacy mutation; tick-global couplings like
   `maxRollbacksPerTick=5` and `storeQueryPartial`). Replaced with a
   bounded-window [N, N+1] match keyed by instance_token, **deterministic
   replay** ("world-moved" auto-classification), tick-context suppression
   predicates, end-of-tick re-derive == ∅ fixpoint check, and FOREIGN_WRITE
   attribution for the other writers of these keys (3b).
7. **primedEnv honesty.** `GC_STARTUP_PROMPT_DELIVERED` is launch-env-only;
   nothing in the tick reads a live session's env, so `runtimeFacts.primedEnv`
   is unobservable under "no new probes" — the v0 shadow would have emitted
   `actionAttemptPrime` for every live pre-Stage-2 session every ~60s, a
   permanent divergence flood. The **priming action family is excluded from
   real-city comparison** in Stage 3 (fixture/crash-fake validated only, plus
   an optional post-Stage-2 cohort); real-city priming coverage starts at
   Stage 4 (3a/3c).
8. **Kill-switch made real.** Defined the flag substrate (per-city durable
   bead-metadata key, read once per reconcile pass by **every** entry point —
   controller, `gc start`, supervisor — latched at tick start, fail-closed to
   OFF), the honest SLO ("writes stop within one tick per process", not
   "instantly"), writer **provenance stamps** on every converge write,
   **compare-before-write** for any key legacy also writes, and a mandatory
   **post-revert scrub audit** — flip-OFF alone leaves poisoned records dormant
   until Stage 5 read-cutover (3e).
9. **Write-volume budget.** Flipping identity heals on a real city mass-
   backfills every legacy session bead; this fleet has a 138K-events/day
   metadata-flood incident on record. Heals are rate-limited per tick and the
   per-city flip is gated on a measured `bead.updated` budget (3d).
10. **Divergence sink resolved toward the spec.** v0's dedicated
    `session.converge_divergence` event contradicted spec §6/§8 ("stderr +
    trace, no new event type through Stage 5"). Resolution: **no new event
    type in Stage 3**; the authoritative signal is monotonic in-process
    counters on the 3f metrics surface; divergence *records* are rate-limited
    samples with a `records_dropped` counter that must be 0 for a soak to
    count (3f). Registering a typed event later is an open question (Q3).
11. **Post-flip and per-city rollout bars added.** Stage 4 no longer activates
    on "the flip happened": an explicit 3d→Stage-4 gate (double-write clean at
    quota + second-writer no-op measured + zero unscrubbed revert windows) and
    a per-city N+1 quota are defined (3d/3e).

---

## BLOCKING gaps — resolved in this plan or requiring sign-off before Stage 3 starts

All five council angles returned at least one blocking finding against v0.
Status:

- **[RESOLVED in plan] Trace-blind comparator** → in-process recorder +
  state-diff oracle + CI write-site guard (3b). No Stage-3 code may be written
  against the trace surface as capture.
- **[RESOLVED in plan] Phantom enabled action + unsafe primed_at heal** →
  enabled set shrunk to `{actionStampCanonicalIdentity}`; exhaustive-switch CI
  pin (3d).
- **[RESOLVED in plan] Per-tick equality unreachable** → replay/window
  comparator (3b) and "zero divergences that survive replay" bar (3c).
- **[NEEDS JULIAN — Q1] primedEnv observability**: this plan excludes the
  priming family from real-city shadow. The alternative (a shadow-only,
  batched, read-only env probe as a declared exception to "no new probes")
  buys Stage-3 coverage at probe cost. Decide before the harness PR.
- **[NEEDS JULIAN — Q2] Flag substrate vs D7**: the kill-switch below uses a
  per-city durable bead-metadata key. Spec D7 says "NO config flag" (it was
  aimed at `config.Agent`/genschema churn, which this avoids), and AGENTS.md
  bans *status files* (this is intent, not liveness — but the spec text must
  be amended to say so explicitly). One of D7 or this plan must be amended in
  writing before the flip PR.
- **[NEEDS JULIAN — Q3] Divergence event type**: stay counters-only through
  Stage 5 (spec-compliant, chosen here) or amend §8/R3 now.

Stage 3 implementation may start once Q1/Q2 have answers; Q3 can trail until
the flip PR.

---

## Goal

Prove `deriveConvergeActions` (merged Stage 1) reproduces the legacy per-path
reconciler behavior **exactly** — with a harness that provably *sees* the legacy
behavior and provably *detects* injected divergence — then begin executing the
**minimal provably-safe action subset** as a guarded, provenance-stamped,
value-guarded double-write, with **zero observable behavior change** including
write volume, lock traffic, and event-bus load.

Scope note: because the priming family is deferred (hardening 5/7), Stage 3
proves the identity-heal and rollback *derivations* on real cities and the
priming derivations on fixtures + crash-window fakes only. Stage 4 inherits the
priming activation together with its shadow coverage. This narrows Stage 3 and
is deliberate.

---

## 3a — Build the facts (capture-at-probe-site, not a Phase-1 prefix)

v0 said "assemble facts early in Phase 1 from the existing runtime probe, before
the legacy branches run." The council showed this is unsatisfiable: there is no
single existing probe. Liveness is probed per-branch, mid-loop, with different
semantics (`workerSessionTargetRunningWithConfig` by bead ID on the !desired
path at `session_reconciler.go:1538`; `observeRuntimeProviderLiveness` by
name+ProcessNames on the desired path ~`:1967`, returning **two** bits —
running, alive — for zombie detection). Amended design:

- **Capture at the branch's own probe site.** The per-session capture struct is
  filled by passing the *exact probe result variable* the legacy branch used —
  never a re-probe. Facts record `probeSite` and the resolved probe-target
  name. Invariant preserved: **no new probes, no writes** in shadow.
- **Tri-state, two-bit runtime facts.** Extend the captured runtime facts to
  `{runtimePresent, processAlive}`, each tri-state (`unknown` when that path
  didn't probe), so zombies (`running && !alive`) are expressible. Zombie rows
  are added to the §7.1 truth table. The comparator marks any tick where
  derived facts and the legacy decision used different probe results as
  **NOT-COMPARABLE** (counted, never "divergent", never "clean").
- **`primedEnv` is pinned `false` in real-city shadow** (unobservable —
  hardening 7). Consequence handled in 3b/3c: the priming action family is
  excluded from real-city comparison. Fixtures drive `primedEnv` explicitly.
- **Tick-pinned clock.** One `tickNow` is captured at Phase-1 entry and
  threaded into fact assembly. Legacy predicates keep their own `clk.Now()`
  reads (changing them is not behavior-preserving); the comparator
  auto-tolerates any divergence where a threshold predicate
  (pending-create lease, `staleCreatingStateTimeout`, `primeReattemptInterval`)
  flips sign within the measured |tickNow − branchNow| window, tagged
  `boundary` with both timestamps in the record.
- **Snapshot vintage recorded.** Facts carry `(observerID, tickSeq,
  instance_token)` so async-committing legacy actions (start waves, idle
  probes, drain-ack) can be joined to the tick that *enqueued* them.
- Compute `deriveConvergeActions(durableFacts, runtimeFacts)` per session —
  **do not act on it** (shadow).
- **Identity-skew guard:** the probe-target name resolved for fact capture and
  the name the legacy branch probed are asserted equal FIRST; mismatch is
  classified `identity-skew` (its own counter — positive evidence for Stage 5's
  C4, not comparator noise).

## 3b — Shadow-comparison harness (recorder + state-diff oracle + replay comparator)

Three layers, none of which is the trace surface:

**(1) In-process legacy-action recorder (the capture channel).**
A plain per-session, per-tick slice appended **synchronously at the write
site** — no arming, no budget, no async queue, cannot be env-disabled. Wired
into every legacy write of the compared keys:

> compared keys: `canonical_instance_name`, `canonical_pool_slot`,
> `primed_at`, `priming_attempted_at`, `prompt_hash`
> plus the choreography points: `syncSessionBeads` create-stamping, the
> backfill machine, the adoption identity map (M7), `CommitStartedPatch`
> priming-stamp extension (Stage 2), `clearStaleResumeKeyMetadata` /
> pending-create reset (clear sites), and the launch-env / `restartPromptNudge`
> priming choreography (`template_resolve.go:794`,
> `session_lifecycle_parallel.go:970,1364`).

Completeness is CI-enforced in the style of
`TestGCNonTestFilesStayOnWorkerBoundary`: **no non-test cmd/gc code may write a
compared metadata key except via the recording wrapper.** This test is also the
permanent writer inventory the double-write safety review demanded.

**(2) Owned-key state-diff oracle (the ground truth).**
Snapshot the compared keys per session at tick start and tick end. Assert:

- `end == apply(derivedActions, start)` restricted to owned keys;
- any owned-key delta not predicted, or prediction not realized, is a **typed**
  divergence;
- **fixpoint check:** re-run `deriveConvergeActions` on END-of-tick facts and
  assert ∅ (live C2). Non-empty ⇒ derivation gap or mid-tick mutation, counted
  in its own bucket, never folded into "timing noise";
- an owned-key delta with neither a recorder entry nor a derived prediction is
  **FOREIGN_WRITE** (other observers, `gc prime` CLI, wake path) — its own
  counter and triage lane. Soak requirement: foreign writes to keys the
  converge loop will own under P4 must be **zero**. Writer attribution
  (actor/process) is stamped into the metadata-patch path where available.

**(3) Replay comparator (the judge).**
Every divergence record carries `(city, session, observerID, tickSeq,
instance_token, probeSite, derived-facts fingerprint, AND the values the legacy
path actually read at decision time)`. The comparator:

- matches derived ↔ legacy within a **bounded [N, N+1] tick window** keyed by
  `(instance_token, facts-fingerprint)` — the designed edge-vs-level one-tick
  lag (rollback→teardown, close→absent per spec M8) is enumerated **up front**
  as typed expectations, not post-hoc whitelist entries;
- **deterministic replay:** re-runs `deriveConvergeActions` on the values
  legacy actually read; if that reproduces the legacy action, the record is
  auto-classified `world-moved` and suppressed. The bar becomes "zero
  divergences that survive replay";
- models tick-global legacy couplings as **suppression predicates**, not core
  facts (core stays pure): `rollbackBudgetExhausted` (`maxRollbacksPerTick=5`),
  `storeQueryPartial`, `deferSessionClosesOnBoot`. Derived-present /
  legacy-deferred under a true predicate is EXPECTED, logged with the reason;
- treats time-gated actions (`attemptEligible`) as **windowed invariants**
  (at most one attempt per interval; no interval with a due-but-missing
  attempt), never per-tick presence, and exempts them from cross-tick
  reorder flagging;
- compares only within one observer's tick; cross-observer interleaving is
  routed to the snapshot-skew category, and all counters are keyed per
  `(city, session, tick, observer-instance)` so rates are well-defined;
- **excludes the priming action family from real-city comparison** (3a). On
  fixtures the family is fully compared.

**Emission (spec-compliant, flood-proof).** No new event type. Divergences
increment monotonic in-process counters (`converge.divergence.total` by class —
the counter may never sample) exported via 3f; divergence *records* go to
stderr+trace as **rate-limited samples** (first N per session per hour,
deduped by `(session, action, facts-fingerprint)` with a repeat counter), with
`converge.divergence.records_dropped` — which must be 0 for a soak window to
count. The shadow harness itself sits behind its own `gc.converge.shadow` flag
on the 3e substrate (the 138K/day wisp-flood precedent says the *observer*
needs a kill-switch too).

**Runs in:** fixtures (golden corpus, CI-blocking) AND real cities
(shadow-only, zero behavior impact).

## 3c — Divergence-acceptance bar ("zero" defined, with a denominator)

**Pre-soak self-tests (the soak clock does not start until these pass, and they
stay in CI permanently):**

1. **Write-site completeness audit** — the grep-style CI guard from 3b(1):
   every non-test write site of a compared key goes through the recorder.
2. **Seeded-mutation canary** — run the harness with deliberately broken
   derivations (e.g. drop `actionStampCanonicalIdentity`; emit a wrong slot)
   against each fixture; the comparator must flag each within one tick. A dead
   comparator and a perfect derivation both report 0 — this proves which one
   we have.
3. **Live detection proof** — during soak, ≥1 deliberately-injected divergence
   (shadow-only knob) per city must traverse the full counter/alert pipeline.

**Fixtures (blocking CI gate):** full corpus → 0 divergences. The corpus is
derived from the truth table, not intuition: a **coverage manifest** maps every
row of the §7.1 cross-product tables and every §2.3 crash-window row to ≥1
named fixture, enforced by a test that fails on unmapped rows. v0's 5 scenarios
are extended with: conflict/deferred-singleton interaction (spec §8 requires
proving non-interference), drain-ack stop-pending, ghost sessions
(`session_beads.go` asleep/runtime-missing), zombie (running && !alive),
primedEnv-present-with-lost-markers heal, rollback-budget exhaustion (>5
simultaneous stale pending-creates), duplicate SessionName beads (topo-order
last-write-wins pin), storeQueryPartial ticks, and **one multi-session fixture
under a seeded randomized-interleaving scheduler** (replayable) for
lock-order-dependent divergences.

**Real-city shadow — evidence-based gate (time is a floor, never the
criterion):**

- **Denominator first.** `converge.compare.sessions_evaluated` must equal the
  expected session-tick count for the window;
  `converge.compare.sessions_skipped{reason}` (not-comparable, capture-loss,
  early-continue) is triaged and capture-loss must be 0. Zero divergences with
  a flatlined denominator is a harness failure, not a pass.
- **Per-action-type quotas** (rule of three: 0 divergences in N verified
  comparisons bounds the true rate below ~3/N at 95%). Flip gate is a query
  over `converge.compare.total{action_type}`, floors proposed:

  | comparison class | min N | expected source |
  |---|---|---|
  | ∅-on-live (converged steady state) | 10,000 | ambient |
  | `actionStampCanonicalIdentity` (heal, derived-only path) | 1,000 | ambient backfill population |
  | identity stamp at create/adopt (legacy-paired) | 300 | ambient churn + runbook |
  | `actionRollbackRuntimeToAbsent` derived (vs legacy rollback w/ suppression modeling) | 100 | runbook |
  | rollback-budget-exhausted ticks | 25 | runbook |
  | storeQueryPartial ticks | 10 | runbook |
  | crash-adoption / controller-restart ticks | 25 | runbook |
  | priming family | fixtures only (real-city N/A in Stage 3) | — |

  (N values are proposals — Q4.)
- **Mandatory fault-injection runbook** on ≥1 soak city, each scenario checked
  off and required to move its counters: kill -9 the controller mid-launch
  (§2.3 crash windows); controller restart forcing adoption; `bd close` beads
  under live runtimes to exceed the rollback budget of 5; transcript deletion
  under a stamped hash (#3849 shape); template startup-prompt flip mid-soak
  (fixture-cohort only in Stage 3); induced provider start failures / 429s;
  store partial for ≥2 ticks.
- **"Under load" defined numerically:** ≥30 concurrent sessions and ≥100
  session create/close transitions/day on the load city.
- **Floors:** soak ≥48h (diurnal effects), ≥3 cities incl. the load city —
  necessary, never sufficient.

**Whitelist rules (hard):** an expected-divergence class is admissible only if
it is (a) a **typed predicate in the comparator code**, verified per instance —
e.g. a derived-only `actionStampCanonicalIdentity` is "expected" ONLY if
(canonical record absent) AND (stamp value == what the legacy ladder resolves
right now — the Stage-5 C4 parity assert pulled forward); (b) **write-
equivalent** — final durable state of the compared keys provably identical
under both paths; (c) reproduced by a checked-in fixture; (d) second-reviewer
signed. Hard cap: **5 classes**; exceeding it blocks the flip and forces a
design revisit. Free-text whitelisting of individual records is banned. A
divergence reclassified "legacy bug" is fixed in legacy BEFORE the flip or
moved to Stage 4 as a named, tested behavior change — because double-write
makes every whitelisted action divergence a real behavior change at flip time
(C1 is not negotiable).

## 3d — Double-write flip: minimal enabled set, complete disable list

Once the 3c gate passes, the per-city flag (3e) makes derived actions execute
as a double-write (new path writes; legacy still writes).

**ENABLED in Stage 3 — exactly one action:** `actionStampCanonicalIdentity`,
triple-gated:

1. **Stability precondition** — stamp only in steady state:
   `started_config_hash` present, stored template matches the current pool
   template, and the ladder returns an unambiguous non-zero/non-fallback
   result. Transient ladder output (mid-rollout, mid-adoption) is never frozen
   into the durable record; legacy's per-tick self-correction is preserved
   until stability.
2. **Write-if-absent CAS under the already-held legacy lock** — the executor
   runs inside the same alias/identifier lock scope where the legacy branch for
   that session ran (zero NEW acquisition points), re-validates the
   precondition at write time (key still empty, bead still open,
   `durable.absent` still false), and never overwrites a present value.
   Concurrent observers cannot interleave different values.
3. **Compare-before-write + continuous parity** — for the create/adopt sites
   where legacy also stamps identity, the executor compares its value against
   the legacy-stamped value for that session this tick; on mismatch it emits a
   divergence and **skips the write** (legacy wins throughout Stage 3 — a
   differing value never reaches the store; turns C2's "second writer is a
   no-op" from assumption into enforced invariant). From flip onward, the
   Stage-5 `ladder == record` assert runs **continuously** in shadow; any
   mismatch alerts, with a defined repair (clear record + restamp under the
   stability gate).

Every converge-executed write carries **provenance**
(`canonical_written_by = converge@<git-sha>+<flip-generation>`) so any revert
window is enumerable (3e), and writes **byte-identical values** to what legacy
would write (timestamps enter facts and are reused; hashes carried, never
recomputed) so double-application is a literal store-level no-op.

**DISABLED in Stage 3 — everything else, enforced intensionally in Go:** the
executor is an **exhaustive switch over `sessConvergeAction`**; every variant
not explicitly enabled hits a disabled branch incrementing
`converge.actions.skipped{action}`. A CI test pins the enabled set to exactly
`{actionStampCanonicalIdentity}` and fails when a new enum variant is added
until it is explicitly classified — the disable list can never again be
vacuously "complete" in prose. Disabled today:

- `actionAttemptPrime` (nudge — Stage 4 via `worker.Handle`, executed inside
  `WithCitySessionIdentifierLock` with post-acquisition re-read; noted now
  because Stage 4 inherits this plan's flag/lock machinery and C6's lock
  baseline will legitimately grow there by one acquisition per real attempt)
- `actionStampPrimedFromRuntime` (moved out of v0's enabled set — P4 names two
  sanctioned `primed_at` writers and this would be an unsanctioned third; its
  healed `prompt_hash` cannot be verified against the actually-delivered
  rendering; and it has no legacy counterpart, contradicting the 0-divergence
  bar by construction. Ships in Stage 4 with the priming state machine and the
  P1–P5 battery; if it ever needs to ship earlier, P4 must be amended in the
  spec and the healed stamp must carry a sentinel hash treated as
  always-stale-on-template-change.)
- `actionRollbackRuntimeToAbsent` (runtime teardown — Stage 4)
- any future variant, by default (the switch)

**Write-volume budget (138K-incident guard):** identity heals are rate-limited
(proposed: ≤25 heals per city per tick — the backfill population drains over
hours, not one tick); the per-city flip is gated on measured `bead.updated`
and store-write deltas staying within an explicit budget during a canary
window. Event-bus load is a 3f metric alongside divergence counters.

**C6 measurement:** alias-lock acquisitions counted by a **plain in-process
counter at the `WithCitySessionAliasLock` acquisition points** (not trace
records), compared as steady-state rates over a window pre/post flip — not
per-tick equality. A §7.3-style order-recording fake asserts lock acquisition
**order** is identical pre/post flip and that no enabled write occurs outside
the lock (count parity alone misses hold-time growth and order inversion).

**Post-flip comparator mode:** after the flip, "precondition already satisfied
by the other writer" is modeled as legal; the authoritative post-flip check is
**converged-final-state matching** (the state-diff oracle), with
`converge.actions.executed{action}` expected to trend to ~0 as the backfill
drains (second writer is a no-op — measured, not assumed).

**3d → Stage-4 gate (new):** Stage 4 activates only after double-write runs on
all target cities with 0 surviving divergences, second-writer no-op confirmed
(executed-writes-per-tick delta → 0 post-backfill), C6 order+rate clean, and
zero unscrubbed revert windows fleet-wide (3e) — measured over a further
quota-based window using the same per-action-type counters, not a calendar.

## 3e — Kill-switch / rollback (a procedure, not a bit-flip)

**Flag substrate (Q2 — needs D7 amendment in the spec):** `gc.converge.doublewrite`
and `gc.converge.shadow` are **per-city durable bead-metadata keys on a
coordination-class store front door** — beads are the sanctioned durable
substrate; this is declared intent, not process liveness, so it does not
violate the no-status-files rule. Not a `config.Agent` field (no
genschema/AgentPatch surface — preserving what D7 actually protects).

- **Every reconcile entry point reads it** — the controller tick loop
  (`city_runtime.go:2294`, `:2980`), the `gc start` path (`cmd_start.go:961`),
  and the supervisor build path (`cmd_supervisor.go:2527`) — so no observer
  process keeps double-writing after a flip (no split-brain).
- **Latched once per reconcile pass, at tick start**, into the tick context:
  all sessions in a pass see one value; an in-flight per-session action list
  always completes under the value it started with (a mid-list flip in Stage 4
  would otherwise cancel between a write-ahead attempt stamp and its nudge —
  manufacturing the §2.3 crash window on every flip). Enforced by test: a fake
  flag source asserts exactly one read per pass.
- **Fail-closed:** read error, absent, or unparseable value ⇒ hard OFF
  (legacy-only). Mixed-binary rule: an older binary that doesn't know the key
  is implicitly OFF — the safe direction.
- **Honest SLO:** flipping OFF stops converge writes **at the next tick
  boundary in every observer process — ≤1 patrol interval + flag-propagation
  lag** — not "instantly". The runbook must not assume writes stop at the
  moment of the flip.
- **Audited flips:** every flip writes `flipped_at`/`flipped_by`/`generation`
  into the flag metadata, making each city's double-write window a queryable
  fact.
- `converge.doublewrite.enabled` exported **per process** (not just per city)
  so residual split-brain is visible in metrics.

**Revert = flip OFF + scrub, in that order — flip-OFF alone is not a revert:**
the trigger for reverting is precisely "the derivation wrote wrong values",
and `canonical_*` heal-site records have **no legacy writer** — legacy neither
overwrites nor detects them; they lie dormant until Stage 5's read-cutover
(`canonicalIdentity.Present` short-circuits the ladders) detonates them, and a
bogus `primed_at` would permanently suppress Stage-4 re-priming (#3872-3
recreated). Therefore:

1. Flip OFF (writes cease within the SLO).
2. Run the comparator in **audit mode** over all sessions of the reverted
   city: enumerate provenance-stamped records from the flip generation(s);
   **clear** every record that disagrees with the live legacy-ladder
   derivation (clearing is safe by design — absence re-arms the heal). A
   `gc doctor`-style one-shot sweep implements this.
3. Revert is **complete** only when the audit is clean. **Stage 5 cutover is
   gated on zero unscrubbed revert windows in any city.**

**Per-city rollout:** city N+1 flips only after city N accumulates a defined
quota at zero surviving divergence (proposed: 24h AND ≥5k comparisons
including ≥1 rollback-class and — Stage 4 — ≥1 priming attempt). All 3f
metrics and the C6 comparison are keyed by (city, flag state); the rollout bar
is city N's per-city post-flip record, never a fleet aggregate. Shadow stays
ON after the flip (post-flip divergences remain caught), behind its own flag.

## 3f — Observability

Counters (in-process, monotonic; the authoritative soak/flip signals — records
may sample, counters never):

- `converge.compare.sessions_evaluated` / `sessions_skipped{reason}` — the
  denominator; alert if `evaluated` flatlines.
- `converge.compare.total{action_type}` — the per-action-type quota inputs.
- `converge.divergence.total{class}` + `converge.divergence.records_dropped`
  (must be 0 for a window to count) + `converge.divergence.foreign_write` +
  `identity_skew` + `boundary` + `world_moved` (suppressed-but-counted).
- `converge.actions.derived{action}` / `executed{action}` / `skipped{action}`
  (the exhaustive-switch disabled branch) / `skipped_value_mismatch`
  (compare-before-write refusals — any nonzero value alerts).
- `converge.doublewrite.enabled` per process; flip audit trail in flag
  metadata.
- Alias-lock acquisition counter at the acquisition points (C6 rate basis).
- Event-bus load: `bead.updated` rate per city across the flip (write-volume
  budget guard).

Alerts: any post-flip divergence that survives replay; any
`skipped_value_mismatch`; any FOREIGN_WRITE to P4-owned keys; ladder==record
parity mismatch; `records_dropped > 0`; `bead.updated` budget breach;
denominator flatline. Alert routing must not be stderr-grep (known
sudo-audit false-positive channel in this fleet).

## Open questions for Julian (deduplicated, sharpest first)

1. **primedEnv observability (blocking, decides Stage-3 scope).** Accept this
   plan's choice — priming family excluded from real-city shadow, coverage via
   fixtures/crash-fakes, real-city priming validation deferred to Stage 4 — or
   authorize a shadow-only, batched, read-only env probe as a declared
   exception to "no new probes" (cost: extra tmux/env inspection per live
   session per tick during soak)?
2. **Flag substrate vs D7 (blocking for the flip PR).** Amend spec D7 to
   sanction the per-city durable bead-metadata flag (read-latched per tick,
   fail-closed) as *the* Stage-3+ flip mechanism — or insist on
   restart-required env/code flips and accept a much slower revert SLO and
   no-per-city granularity?
3. **Divergence event type.** Stay counters+sampled-records through Stage 5
   (spec-compliant; chosen here), or amend §8/R3 now and register a typed
   `session.converge_divergence` payload?
4. **Quota numbers.** Sign off the per-action-type minimum-N table in 3c and
   the load-city definition (≥30 concurrent sessions / ≥100 transitions/day),
   or set different escape-rate targets?
5. **Heal rate limit / write budget.** ≤25 identity heals per city per tick
   and a `bead.updated` canary budget before city-wide flip — acceptable
   backfill drain time (hours) vs flood risk?
6. **PR split.** Recommend three: (i) recorder + oracle + comparator +
   self-tests (soak starts), (ii) flag substrate + provenance + scrub sweep,
   (iii) the flip itself (executor switch + CAS write). Agreed?
