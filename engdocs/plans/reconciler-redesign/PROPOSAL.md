# Reconciler Redesign — "Windshield GT"

> A first-principles, blue-sky redesign of the Gas City reconciler cluster into a
> simple, testable, military-grade core. Produced 2026-07-08 by a 23-agent
> multi-model workflow (6 Opus explorers + 4 Opus researchers → 4 Fable
> architects → 3 Opus judges → Fable synthesis → 4 Opus red-teamers → Fable
> finalize). Raw structured outputs preserved under `evidence/`.
>
> **Status:** historical design seed and decision lineage. The keyed target
> architecture in [IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md) and the
> executable contracts in [ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md) are
> canonical. They preserve this proposal's pure decisions, `Unknown`,
> proof-carrying destructive effects, marker-last writes, and strangler
> migration while superseding its fleet-global serial scheduler, single opaque
> incarnation, provider-timeout assumptions, and check-then-name/PID safety
> claims.

---

## 0. TL;DR

> **2026-07-12 hardening note:** Read the architecture below as the origin of
> the safety kernel, not as the final scheduler or provider contract. Current
> `origin/main` has distinct runtime/place/transport/attachment seams, warm
> relaunches that preserve boxes, CLI/controller ownership races, and tmux
> effects that target reusable names. The canonical design therefore uses
> stable keyed queues, separate box and launch incarnations, context-aware
> actual-call ownership, durable CLI requests, provider-capability profiles,
> and independent anti-entropy. Exact scenarios live in the acceptance matrix.

The reconciler's disease is not "it's messy" — it is **one specific, nameable
mistake repeated everywhere**: side effects are **edge-triggered and
path-dependent** on a substrate whose entire reliability story is
**level-triggered convergence**. Each session arrival path (fresh spawn,
adoption, resume, drift-relaunch, rollback) stamps *its own subset* of
identity/priming metadata, and most of the 100+ tracked bugs live in the gaps
between those stampings. A 2,188-line / 28-parameter function fuses observation,
decision, and mutation for ~12 concerns in one scope, and a 5,124-LOC WAL-grade
trace subsystem exists *because that fused path is otherwise undebuggable*.

The destination — **Windshield GT (Ground Truth)** — is one sentence:

> **Observe the world once and honestly per pass (doubt is `Unknown`, never
> `Dead`), decide purely over typed per-session state, and let every destructive
> or lossy action exist only as a value that carries proof of a corroborated
> observation — claiming exactly the guarantees the real Dolt/sqlite store and
> tmux runtime provide, and not one more.**

The single most important finding: **the destination is already half-built and
dormant in the tree.** S19 stage 1 (merged, `#4034`) landed the pure cores —
`deriveConvergeActions`, `durableFacts`/`runtimeFacts`, `desiredSessionIdentity`,
`promptDelivery`, the two-phase priming markers — as *pure, total, table-testable
functions with zero production callers.* The path forward is not a rewrite; it is
**"grow the action vocabulary and wire the act loop,"** an incremental strangler
behind a parity pin, arm by arm.

This proposal is deliberately **honest about its own limits.** The red-team
killed four load-bearing claims from the first-draft architecture — Rev-CAS
(vaporware: beads has no per-bead revision), whole-fleet `Decide` (a rewrite
disguised as a "widen"), the sealed 6-handler interface (relocates complexity),
and simworld-DST-as-a-merge-gate (blind to the Layer-0 seam where the real bugs
live). All four corrections are baked into the design below. What survives is
smaller, provable, and shippable.

---

## 1. The essential job (stripped of accidental complexity)

Per pass, for each session, the reconciler makes **exactly one decision** from a
small set — given (desired config, durable bead facts, observed runtime
liveness), choose one convergence action:

```
{ start/wake · drain · close · rollback-to-absent · heal-state · prime · restart-in-place · defer }
```

When the ledger, the runtime, and config all agree, the action list is **empty**.
**Boot, crash recovery, and steady state are the same pass.** Everything else in
the ~20,000 LOC cluster is accidental machinery: the 28-param threading and
5-overload wrapper chain, the hand-maintained snapshot-coherence protocol, the 32
raw/`Info` predicate twins, the unknown-state skip-forever, the inline
circuit-breaker/rate-limit/churn/drift accounting braided into a 1,600-line
`continue`-ladder, and the trace WAL that exists to make the fused path
observable.

---

## 2. The disease (eight error classes, code-confirmed at HEAD 7efe9935f)

The explorers verified every claim against live code. "Reconciler" is overloaded
across **6 subsystems** (session, convergence, dispatch-control, beads-cache,
rig-registry, controller-loop); the pain concentrates in the session reconciler.

| # | Error class | Mechanical root cause (verified) | Bug refs |
|---|---|---|---|
| 1 | **Path-dependent stamping** | Identity/priming/firstStart stamped inside whichever arrival path ran; a new path silently ships a missing side effect. Bugs live in the gaps. | #3872 family, #3849, #2073, #1893 |
| 2 | **Durable-vs-runtime divergence** | ~14 ad-hoc comparison sites across 3 engines, with **4 competing runtime-fact types** (`runtime.Liveness`, `session.RuntimeObservation`, `worker.LiveObservation`, `session.RuntimeFacts`). | #1029, #2285, #2083 |
| 3 | **Stringly state / skip-forever** | State is a raw metadata string; unknown → `continue` (`session_reconciler.go:1523-1532`) with no TTL, no escalation, re-logged every tick forever. | #2085, #2389, #1496, #1497 |
| 4 | **Swallowed error on destructive path** | `workerSessionTargetRunningWithConfig` error collapses to `providerAlive=false` (`:1538-1540`), which then **authorizes an orphan-bead CLOSE** of a live-runtime session. | gc-hz0nu, gc-kkgak, #2987 |
| 5 | **N drifting implementations** | 32 raw/`Info` predicate twins, each "equivalence-proven" by a *comment*, not construction; 3 scope-close copies; triplicated attach pipelines. | structural |
| 6 | **Hand-maintained dual representation** | Raw `beads.Bead` + typed `session.Info` snapshot kept coherent by 32 mid-tick `infoByID[id] = …ApplyPatch(…)` folds; a missed fold silently corrupts the cross-session min-floor scan. | ga-ebb62d, #1029 |
| 7 | **Cross-cutting concerns inlined into one spine** | Circuit breaker, rate-limit, churn, drain-ack, drift, progress-stall all interleaved as inline branches with early `continue`s inside the 1,600-line loop. | #2574, #3630, #2345, #127 |
| 8 | **Delivery ordering (at-most-once vs at-least-once)** | Stamp-primed-*before*-nudge loses the prompt forever on a crash; naive inversion re-nudges every tick unbounded. | #3872, #3279 |

**The load-bearing asset already in-repo:** `internal/convergence/reconcile.go`
(546 LOC) is the sound crash-safe pattern — typed-state switch, `(action, error)`
wrapped at one site, idempotency keys, marker-last write ordering, recovery =
convergence. S35 (landed) says *copy its idioms, don't merge the domains.* One
caveat the explorer found: **even the blueprint has drifted** — its terminate
path writes `last_processed_wisp` via a best-effort `_ = SetMetadata(...)` outside
`handler.commit()` (`reconcile.go:496-511`). The original proposal labeled that
work W0; current execution uses the Phase 1 task IDs in the canonical plan.

---

## 3. Windshield GT — the architecture

### 3.1 The loop shape

**Level-triggered, crash-only, serial.** One pass =

```
Observe   →   Decide (pure, per session)   →   Apply (serial)   →   Record
(1 impure     (0 impure reads;                (1 impure writer;     (telemetry +
 read)         total; idempotent)             marker-last)          replay corpus)
```

- **Observe** — exactly one frozen batch `list-sessions` + one proctable scan per
  pass. The *only* impure read. Produces honest **tri-state liveness**: a fetch
  error, a stale cache, or a mid-pass refresh yields `Unknown`; a config-known key
  absent from a *successful* frozen list is `Unknown-until-corroborated`.
- **Decide** — the landed per-session `deriveConvergeActions(durable, runtime,
  fleet) []action`, grown arm by arm. No store, provider, clock, or context — a
  CI determinism lint makes I/O in the core package *uncompilable*. Total: every
  `(durable × runtime)` combination returns a defined, ordered, idempotent list.
- **Apply** — the serial reconciler pass, the *only* writer of `gc.status.*`.
  Marker-last; two-phase intent around lossy effects; **witnesses** around
  destructive ones.
- **Record** — a small flight recorder (ships last); every incident becomes a
  permanent table-test row.

The 30s ticker becomes a **resync floor** that enqueues all keys; bead events,
fsnotify, and pokes are **enqueue-only hints** — arrivals never write. Per-key
jittered exponential backoff + a global start token-bucket bound respawn herds.
**Boot is the first tick** — the 6-phase startup ladder and the separate recovery
path are *deleted* because recovery *is* convergence. Deliberately **no worker
pool** (parallelism is a deferred scheduling decision, revisited on measured
fleet size).

### 3.2 The state model — three prefixes, honest ownership

Bead metadata splits into three prefixes with explicit, honestly-scoped ownership
(this is "server-side-apply-lite" — grafted from thesis T4, downgraded from true
SSA per the red-team):

| Prefix | Contents | Writer | Guarantee |
|---|---|---|---|
| `gc.spec.*` | desired: name, alias, rig, pool slot, config hash, prompt ref, deleted | **create-once**, by the single shared synchronous create path (`session_resolution.go:349`); post-create mutations only through the codec | Immutable outside the codec |
| `gc.status.*` | observed: 13-state typed lifecycle, hashes, instance token, intent markers, `quarantined_until`, generation trailer | **only** the serial reconciler pass, as whole-struct `ApplyStatus` | Serial single-writer + read-back drift events (**not** CAS — see §5) |
| `gc.ext.*` | operator annotations | anyone | never read by the core |

Decode is **total**; the raw map is confined to one codec package. In-memory
state is fail-safe timing scratch (absence/intensity counters — a restart only
*delays* verdicts, never corrupts them). Quarantine verdicts are durable with TTL
+ reset-generation.

---

## 4. The core primitives

Eight primitives. Each names what it replaces and which error class it kills.
Provenance: **T3** = functional-core/DST spine (the winning thesis); **T4** =
write-side (SSA-lite, finalizers); **T2** = compile-time totality + recovery=boot;
**kit** = shared mechanics (S35-faithful: mechanics only, not state machines).

1. **`FrozenObservation` — corroborated tri-state probe** *(T3, hardened by DS
   red-team)*. `Observe(ctx, deps) (Observation, error)`; `Liveness = Alive |
   Dead | Unknown`. `Dead` requires **N consecutive absences from successful
   frozen lists AND a per-key `HasSession` confirm AND a proctable cross-check.**
   Replaces the `Liveness{Running,Alive}` bool pair, the staleTTL→`Running=false`
   collapse, and the per-question re-probes.

2. **Typed session codec (total decode, real state space)** *(T2 + T3)*.
   `package sessioncodec`: `DecodeSpec`/`DecodeStatus`; `SessionState` enumerates
   **all 13 observed states** + `StateUnrecognized{Raw, FirstSeen}`;
   exhaustive-linted switches. Unknown strings become a *first-class typed state*
   whose only legal outputs are `defer + TTL + escalation event` — never
   destructive, never quarantine (rolling-deploy forward-compat). Confines ~29 raw
   keys and (gradually) the ~50-field `Info` mirror + ~3,019 read sites.
   *(The sealed 6-method handler was dropped — a 13→6 partition just relocates
   complexity into `OnActive`/`OnUnrecognized`. A Go enum + exhaustive lint gives
   the same compile-forced totality without inventing a taxonomy.)*

3. **Per-session pure core (grown, not replaced)** *(T3; = S19 continued)*.
   `deriveSessionActions(d durableFacts, r runtimeFacts, f FleetFacts)
   []sessConvergeAction` — keeps the landed signature at
   `session_level_converge.go:214`, widened with one read-only frozen fleet
   aggregate; determinism-linted. Every branch must cite the durable-vs-observed
   disagreement it repairs. Idle/sleep/wake arms stay **bead-state-keyed** (the
   #312 lesson — no `if idle > N then act` heuristic may enter the core).
   Replaces the 2,188-line Phase-1 `continue`-ladder, arm by arm behind the pin.

4. **`FleetFacts` two-stage pure pipeline** *(explicit rewrite, phase-gated)*.
   `projectWorld(cells, sessionActions) ProjectedWorld` then
   `deriveFleetActions(projected, desired) []fleetAction`. Pool min-floor,
   undesired-pool sweep, and wake fairness as pure functions over the **post-plan
   projected world** — replacing the 32 mid-tick `ApplyPatch` mutations the
   min-floor scan currently reads. **Honestly labeled a rewrite** (per the
   code-reality red-team: cross-session arms have no landed pure counterpart),
   sequenced late (W5), with its own semantic-equivalence parity strategy.

5. **Witness types: `DeathCertificate` + `LiveTargetWitness`** *(the unanimous
   survivor across all four red-teams)*. `type DeathCertificate struct{ key
   SessionKey; pass uint64; _ noCopy }` — unexported constructor, minted **only**
   by `Observe` on corroborated-`Dead`; single-use, one pass; every kill site
   rechecks PID start-time/cmdline. Destructive effects (orphan-close, teardown,
   pane kill) are **struct-unconstructible without positively corroborated
   observation from this pass.** Replaces the `providerAlive` err→false collapse.
   Applied to **all** destructive paths including unrewritten legacy arms (W2), so
   it protects code not yet migrated.

6. **`IntentMarker` two-phase + named receiver-idempotency contracts** *(landed
   S19; honesty contract added)*. `AttemptedAt`/`ConfirmedAt`/`Attempts` +
   `Eligible(now, backoff)`. Bounds every lossy effect to **bounded
   at-least-once** and *names the receiver-side dedup for each non-idempotent
   effect*: Start → deterministic `SessionNameFor` + provider `ErrSessionExists`
   with a *probe-confirmed* (not stale-cache) `IsRunning`; Prime → bounded
   attempts with duplicates *explicitly accepted*; Close → naturally idempotent.
   **The claim is scoped honestly: bounded at-least-once, never exactly-once.**

7. **`ApplyStatus`: single-writer, torn-proof, drift-loud** *(T4, downgraded from
   Rev-CAS per three concurring red-teams)*. Encodes the whole struct, diffs
   before writing, writes via **one `SetMetadataBatch` whose final key
   `gc.status.gen` is a hash of the encoded struct**; decode treats a gen-mismatch
   as torn → `Unknown` → re-heal. A pre-write read-back compare against the
   pass-start snapshot emits a typed `session.status.drift` event and re-observes
   instead of overwriting foreign writes. **Rev-CAS is deleted** — beads exposes
   no per-bead revision (`PRAGMA data_version` is global). This is the honest
   replacement: serial single-writer + read-back drift, upgradeable to true CAS
   only if the gated per-bead-revision spike lands.

8. **`Quarantine Guard` (intensity-only, durable-with-reset)** *(T2/shell,
   every invariants red-team must-fix incorporated)*. `Admit(k, now) → Proceed |
   Backoff{Until} | Quarantine{TTL, EscalationEvent}` — counts **only
   crash/restart intensity**; `StoreErr`, `ProviderFlap`, and `Unknown` map to
   backoff and can *never* mint a quarantine. Verdict persists as
   `gc.status.quarantined_until` with TTL + reset-generation; a first-class
   **operator-reset effect** (`gc session reset`) mirrors the circuit breaker's
   reset knob. Replaces `crashTracker`, the backoff maps, the circuit-breaker
   singleton, and the skip-forever branch. `idleTracker`'s time-threshold judgment
   is **not** folded in — idle arms stay bead-state-keyed.

Plus the **flight recorder** (ships last, W8): a size-capped JSONL of decision
*inputs and outputs*; `gc trace` becomes a projection over it (with a mapping
layer to the existing 185-code taxonomy for on-call continuity). It replaces the
5,124-LOC bespoke trace WAL — **only after** the differential oracle's duty ends
and shadow logs confirm no remaining readers. *You cannot delete in phase 6 the
subsystem your phase-2/3 safety net runs on.*

---

## 5. Error classes made unrepresentable

This is the "new primitives that simplify entire classes of errors" you asked
for. Each row is a *class* turned into a compile error or a structural
impossibility — not a bug patched reactively.

| Error class killed | Mechanism (why the whole class becomes unrepresentable) |
|---|---|
| **Orphan-close / teardown of a LIVE session** (via swallowed error *and* false-negative list absence) — gc-hz0nu, #2073, #2987 | Destructive effect values have required witness fields with unexported constructors invoked only by `Observe` on *corroborated*-Dead. Errors, stale caches, and first absences yield `Unknown`, which mints nothing → the destructive value **cannot be built**; any bypass is a compile error. Kill sites recheck PID start-time/cmdline. |
| **Torn half-written status trusted as truth** — crash between `state=active` and `instance_token` | `ApplyStatus` is the only status-write API; it emits one batch whose final key hashes the struct; decode treats gen-mismatch as `Unknown`→heal. A torn struct can be *produced* by a crash but never *consumed* as truth. |
| **Unknown-state skip-forever** — #2085, #2389, #1496, #1497 | Unknown strings decode to `StateUnrecognized{raw, firstSeen}`; the exhaustive lint cannot omit it; its only constructible outputs are `defer + TTL + event`. **There is no default-`continue` arm left to write.** (And no rolling-deploy hazard — it never quarantines a newer controller's legitimate new states.) |
| **Silent at-most-once prompt loss / unbounded redelivery** — #3872, #3279 | The applier's only lossy-effect entry points stamp `attempted` *before* the I/O and `confirmed` after; call sites cannot reorder the pair. `Eligible()` converts crash windows to bounded backoff. Each non-idempotent effect declares its receiver dedup. |
| **Edge-triggered per-arrival-path stamping** — #3872, #3849-class, #1893 | Post-creation, **no arrival path has a status-write API**: events/pokes/fsnotify can only *enqueue* a key. Status keys exist solely as whole-struct `ApplyStatus` from the serial pass, so "path X forgot key Y" **has no code path to live in.** |
| **Silent clobber of foreign `gc.status.*` writes** — ga-ebb62d, #1029 | *Not* made structurally impossible (no per-bead revision exists — stated honestly). Made impossible to fail *silently*: `ApplyStatus` reads back, and a foreign diff emits `session.status.drift` + re-observe instead of overwrite. Upgrades to true CAS only if the gated beads spike lands. |
| **TOCTOU / hidden nondeterminism breaking replay** | `deriveSessionActions` admits no store/provider/clock/context — there is no API through which a decision can re-probe. A vet-style CI gate forbids the core package from importing `io`/`net`/`math-rand` or calling `time.Now`; inputs are sorted frozen values. |
| **Dedup/progress markers written out of order** (incl. the verified `reconcile.go:496-511` drift in the blueprint itself) | `commit` takes the marker as a dedicated trailing parameter and writes it last internally with a checked error. Stays convergence-local until a second domain adopts it (two-implementations rule). |

---

## 6. Military-grade testability

The distinction that makes this *credible* rather than aspirational: **which
layers are real shipping gates vs stretch goals.** (The YAGNI red-team's central
catch: simworld DST over `memstore`+`fake` proves `Decide` converges over a
*model*, but the confirmed bugs live at the Layer-0 tmux seam the fake stubs out.)

**Shipping gates (all real, none aspirational):**

1. **Table tests over the pure per-session core** — one row per historical bug,
   widening the *already-landed* S19 tables in place. Every incident becomes a
   permanent row.
2. **Laws as property tests** (no simulator needed): totality fuzz (never panic,
   never silent-skip), **fixpoint idempotence** (apply then re-observe → empty
   actions), no-destruction-on-`Unknown` (vacuous by witnesses, fuzzed as a
   tripwire), action-order absoluteness.
3. **The determinism lint** that makes replay a structural property.
4. **The effect-boundary differential oracle with a hard completeness gate** — all
   ~57 legacy effect sites are interposed to emit plan steps *at the write
   boundary*; CI asserts 1:1 site-to-step coverage **before any soak counts**
   (the existing best-effort `if trace != nil` stream provably under-reports — 47
   trace calls for 57 effect sites — and would green-light false parity).
5. **A tmux/subprocess conformance suite at the probe/witness boundary** — the
   Layer-0 seam where the confirmed bugs actually live; the fake provider is
   pinned to it.

**Stretch (explicitly *not* merge gates):** seeded simworld DST as a nightly
soak; flight-recorder replay of production passes as regression fixtures.

**Parity pin:** per-arm shadow-diff under a *declared semantic-equivalence
relation* (legacy nondeterminism makes byte-identity unreachable), owner-signed
per flip.

---

## 7. Historical migration path (W0–W8)

Sequenced so that each phase is independently valuable, fail-safe-directioned, and
parity-pinned. **W4 is literally S19 stages 3-5, resequenced.**

| Phase | Goal | Safety |
|---|---|---|
| **W0** — standalone bug-kills (ship alone) | Route convergence's terminate path through `handler.commit` (fixes the verified swallowed-marker drift); tri-state liveness at the source; PID recheck at kill sites. | Three independent, individually-revertable fixes; existing suites pin; each destroys *less* on doubt. |
| **W1** — typed codec over existing keys (absorbs S19 stage-2) | `SessionSpec`/`SessionStatus` total decode, 13-state enum + `StateUnrecognized`, predicates rewritten over typed state; raw map confined to the codec. | No key renames; existing raw/`Info` equivalence oracles pin the codec byte-for-byte before any predicate flips. |
| **W2** — corroborated observation + witnesses on **all** destructive paths | One frozen batch probe per pass; consecutive-absence corroboration; witnesses required by every destructive path, legacy arms included. | Witnesses protect legacy code immediately; the only behavior change is deferring destruction under doubt — the fail-safe direction, table-tested. |
| **W3** — differential oracle with a completeness gate | Interpose the ~57 legacy effect sites to emit plan steps; CI asserts 1:1 coverage before any soak counts. | Legacy logic untouched; oracle validity is itself gated, closing the false-green trap. |
| **W4** — grow the per-session core arm-by-arm (**= S19 stages 3-5**) | Widen `deriveConvergeActions` arms — rollback, identity heal, **priming activation (turns on the dormant #3849 fix)**, drain, close — behind the pin; delete each legacy arm on flip. | Per-arm shadow-diff against the W3 oracle under a declared equivalence relation; owner signs each flip; legacy arm deleted only after. |
| **W5** — fleet arms as an **explicit rewrite** | Two-stage pure pipeline (session actions → projected world → fleet actions) replacing min-floor/sweep/wake and the 32 mid-tick mutations. | Own parity strategy; `MinFloorCountReflectsMidTickClose*` tests become projection table rows; never borrows W4's per-arm pin. |
| **W6** — write-side consolidation | `ApplyStatus` = one batch + gen trailer; read-back drift events; operator-reset effect; front door warns (later rejects) raw `gc.status.*` writes. | Diff-before-write keeps Dolt churn identical; warn-first preserves the operator escape hatch. |
| **W7** — loop restructure | Tick becomes enqueue-all + serial drain; per-key jittered backoff; global start token bucket. No worker pool. | Scheduling-only; converged-world equality asserted under permuted schedules + injected crashes. |
| **W8** — delete and retarget | Remove legacy arms, `Info` mirror, equivalence oracles; WAL → flight recorder; `gc trace` becomes a projection with a 185-code mapping layer kept one release. | Only flip-proven-unreachable code removed; production shadow confirms no remaining WAL readers first. |
| **Gated side-quests** (unscheduled) | Per-bead-revision spike in beads (upgrades drift events to CAS); shared mechanics-kit extraction; simworld DST nightly soak; per-key worker pool. | Each gated on a proven substrate or a *second real consumer* — never on the critical path. |

**Honest accounting:** ~15K LOC deleted across the arc; ~3,019 raw-metadata read
sites migrate gradually through the codec; ~5-6K added (codec, witnesses, guard,
oracle, flight recorder) — which becomes the new hardest-to-hold code, so the
"easier at 3am" bet is only *clearly* won for the pieces already landed until W4
proves out.

### Relationship to the S-series

This **supersedes the content of S19 stages 2-7** while keeping S19's numbering,
parity pin, and landed `deriveConvergeActions` as the vehicle. It **absorbs**
S20 (typed state), S23 (Info front-door), S24 (pool-satisfaction level + ghost
healer), S28 (pendingCreate lease), S25 (god-function split) as consequences of
W1/W4/W5 rather than separate items. It **keeps** the already-merged wins (S06,
S09, S18, S26, S30, S31, S33, S34, S36) as stepping stones. S16 (swallowed
errors) was mapped largely to historical W0.

---

## 8. What the red-team killed (the honesty section)

Credibility rests on what got *cut*. The first-draft architecture ("Windshield")
made four claims that four independent Opus red-teamers broke against real code;
the finalize pass incorporated every one:

- **Rev-CAS → deleted.** "Clobbering a newer write is structurally impossible" was
  vaporware: `internal/beads` exposes no per-bead revision; `PRAGMA data_version`
  is global; `SetMetadataBatch` is a whole-blob read-merge-write. Downgraded to
  serial single-writer + read-back drift events + gen-trailer for torn writes.
- **Whole-fleet `Decide(Snapshot)` → cut as a category error.** The landed core is
  *per-session* (`deriveConvergeActions`, 4 actions); fleet arms (min-floor, sweep,
  wake) have no landed pure counterpart and depend on 32 mid-tick mutations. You
  cannot "widen" one into the other — so the per-session core is kept and grown
  (W4), and fleet arms are honestly labeled a rewrite (W5) with their own pin.
- **Sealed 6-method handler → dropped.** 13 live states → 6 handlers just
  relocates today's idle/sleeping/pool/ghost logic into `OnActive`/`OnUnrecognized`.
  Replaced by a Go enum over the real 13-state space + exhaustive lint.
- **simworld DST → demoted from merge-gate to nightly stretch.** It's blind to the
  Layer-0 tmux seam where the confirmed bugs live (`fake ≠ executes`). The real
  gates are table tests + the tmux conformance suite at the witness boundary.

Also hardened: the witness now requires *corroborated*-Dead (not single-list
absence — the DS red-team's sharpest catch: the witness killed act-on-*error* but
not act-on-*false-negative*); the guard is intensity-only with transient-error
exclusion + operator reset; `OnUnrecognized` never quarantines (rolling-deploy
forward-compat); the trace WAL deletion is resequenced to last.

---

## 9. Two-perspective read

**What a perfectionist rejects:** the design still leaves ~3,000 raw-metadata read
sites migrating "gradually"; single-writer is enforced by *convention* (serial
exclusivity) not by storage until the beads-revision spike lands, so a
multi-replica hosted topology can still clobber; `gc.spec.*` is create-once but
shared between API-create and the reconciler, so it's not *quite* single-owner;
and ~5-6K of genuinely new code (codec, oracle, recorder, guard) has to be
carried before the god function shrinks.

**What a pragmatist accepts:** the two highest-value, lowest-cost primitives
(witness-typed destruction, two-phase priming) are correct and *partly landed
already*; W0 was intended to ship real bug-kills alone with zero new architecture; every phase was
fail-safe-directioned (destroy less on doubt) and individually revertable; the
migration extends a pure core that *already exists dormant in the tree* rather
than starting a rewrite; and the "military-grade" claim is scoped to gates that
actually buildable, with DST explicitly a stretch. The workflow's historical
takeaway was that W0 could start immediately without making the rest a big-bang;
the current pre-G0 exception and task ordering are defined only in
IMPLEMENTATION_PLAN.

---

## 10. Historical decision questions

These were the proposal's unresolved forks. Their current disposition is in
IMPLEMENTATION_PLAN §§2–4, P0.2, and §29; this list no longer authorizes an
implementation choice (full historical list in `evidence/final.json`):

1. **Rev spike?** Fund a per-bead-revision column in beads? It's the only path
   from drift *events* to true CAS, and touches all four stores + the bd CLI
   schema. Otherwise multi-writer clobber stays *loud but possible*.
2. **Spec ownership.** Keep API/CLI synchronous create as the one shared
   `gc.spec.*` writer (create-once, recommended) vs. forcing async
   reconciler-mediated creation? And how does it sequence against the in-flight
   worker-boundary migration?
3. **Durable quarantine.** `quarantined_until` with TTL + reset-generation
   *reverses* the documented in-memory OTP-restart stance. Explicit reversal or
   veto?
4. **Multi-replica.** The `.gc/controller.lock` flock fences per *host*. Before
   hosted multi-replica ships, do we need a ledger-level leader lease, or do you
   declare single-controller-per-city an operational invariant enforced in the
   crucible provisioner?
5. **Corroboration latency.** N=2 consecutive absences at a 30s floor ≈ doubles
   time-to-teardown (~60s) for genuinely-dead sessions. Acceptable, or add an
   immediate per-key confirm probe on first absence?
6. **S-series governance.** Confirm this supersedes S19 stages 2-7's content while
   keeping the numbering + pin, so pin versioning stays coherent.

---

## 11. Historical immediate recommendation (superseded)

The original workflow recommended a W0 of three independent fail-safe fixes:

1. Route `internal/convergence/reconcile.go`'s terminate/creation marker writes
   through the existing `handler.commit()` helper (`:496-511`) — makes marker-last
   structural on the crash path too.
2. Tri-state liveness at the source: `error`/`staleTTL` → `Unknown`, never
   `Running=false` (`internal/runtime/liveness.go`, `StateCache`).
3. PID start-time/cmdline recheck at every `proctable.KillByPID` site.

Current execution follows IMPLEMENTATION_PLAN Phase 1 instead. That phase keeps
these safety directions but adds the confirmed failed-heal, partial orphan-scan,
managed-stop acknowledgement-loss, and shared-Ready-snapshot slices, with
current-head dependencies and named acceptance rows. Do not implement from this
historical list.

---

## 12. 2026-07-12 hardening addendum

Fresh review against current `origin/main` retained the proposal's correctness
kernel and replaced the unsafe or over-broad mechanics:

- One standard Kubernetes-style stingy workqueue per concrete controller is the
  starting scheduler. The first local vertical slice uses same-process
  post-commit hints plus bounded authoritative relist. A no-gap DoltLite feed,
  custom fairness lanes, sharding, and HA are separate evidence-gated
  capabilities, not prerequisites for local correctness or latency.
- Managed CLI/API processes submit one durable idempotent operation and never
  become direct provider writers after socket/poke/ack failure. Unmanaged CLI
  execution owns the same keyed path exclusively until its milestone. Result
  dimensions separate durable commit, completion, caller wait, provider-native
  stage, and action outcome. Self-terminating callers use requester fences and
  report accepted-incomplete before their launch is torn down.
- Provider-owned opaque box and launch identities replace the proposal's single
  incarnation. Start is non-destructive only after a separate witnessed
  dead-pane/zombie cleanup action is wired. Raw tmux names and check-then-PID
  cannot receive exact-target certification; the strong Linux profile requires
  provider-atomic tmux compare-and-effect plus pidfd/root identity and managed
  process-tree containment.
- Provider-adapter entry is not “action started.” Latency and crash evidence use
  provider-native mutation entry, the first point after which the effect may
  have occurred. Caller timeout never releases actual-call ownership. Effects
  that can survive controller death durably record their operation/target and
  use provider lookup/deduplication, fencing, or killable containment before
  their family can be certified.
- Status/projection state advances only after proven commit. Feed poison never
  advances its cursor. Retry state retains class, intent generation, and exact
  blocker/provider-health revision, so irrelevant duplicate hints cannot erase
  backoff. Close/kill/shutdown are level-triggered terminal convergence, not a
  generic multi-step effect log.
- Nudge has a total durable decoder, launch binding policy, exact conservation,
  typed ambiguity, and no blind tmux re-paste after possible injection. Events
  remain observational; a stable logical event ID supports deduplication, but
  exactly-once publication requires a separately proven durable outbox.
- Single-controller migration is a cold exclusive handoff. Phase 11 alone may
  introduce lease epochs and active-active claims. Provider-global shutdown has
  one admission/drain owner and an independent external completion witness.
- Anti-entropy independently enumerates raw authority and folds a fresh
  projection while reusing the canonical total decoder and pure
  `Contributions(object)` semantics. This catches traversal/index/cursor drift
  without creating a second domain model.
- Every safety, CLI/tmux edge case, crash boundary, numeric bound, mixed-version
  case, and capability profile maps to executable retained evidence in
  ACCEPTANCE_MATRIX. A phase label or aggregate green test run is never a
  cutover gate.
- The independent 10-axis/adversarial review added four negative-space fences
  absent from this seed: trusted command authorization, authoritative-store
  restore lineage, protected/tamper-evident gate artifacts, and external `bd`/
  store-schema compatibility. It also made human attachment/copy mode,
  controller activity self-attribution, per-family bridge rollback, composite
  blocked-key liveness, and rare-event deletion exposure explicit. Those
  contracts live only in the canonical plan/matrix.

---

*Evidence: `evidence/{final,synth,judges,redteam,designs,explore,research}.json` —
the full structured outputs of all 23 agents. `final.json` is the historical
workflow finalization; `redteam.json` is the adversarial pass that shaped it.
IMPLEMENTATION_PLAN and ACCEPTANCE_MATRIX are current authority.*
