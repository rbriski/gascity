# Reconciler Implementation Plan — Keyed, Incremental, Entropy-Resistant

| Field | Value |
|---|---|
| Status | Independent review integrated; G0 ratification/hash and owner sign-off remain required before any new effect owner, schema, or concurrency change, while P1.0A/P1.0C/P1.0D/P1.1A may proceed as narrowly scoped no-owner fail-safe slices |
| Date | 2026-07-12 |
| Scope | Session lifecycle, pool demand, nudge delivery, controller scheduling, observation, and anti-entropy |
| Starting point | Current `origin/main`, the active `internal/session` extraction, the conditional-writes rollout branch, and the Windshield GT proposal |
| Supersedes | Windshield GT W7's fleet-global serial scheduler and 30-second action path |
| Preserves | Windshield GT's typed state, pure decisions, `Unknown`, witnesses, intent markers, parity oracle, and fail-safe migration discipline |

The named behavior contract is
[ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md). Every implementation bead and
checkpoint cites the applicable `RC-*` rows. If broad prose in this plan can be
read more weakly than a named row, the named row wins. Permanent session
behavior also updates `internal/session/REQUIREMENTS.md` in the same PR.

At G0, CI records a base content manifest over this plan, the acceptance matrix,
and the permanent session requirements rows. Approved changes form an append-
only delta lineage with per-row hashes. Every implementation bead and evidence
packet cites the base, accepted delta head, and exact row hashes it implements;
an unrelated additive delta does not churn open beads, while changing a cited
row or dependency forces only affected work to revalidate. These are protected
gate artifacts: a slice may add a new row, but may not weaken, rewrite, or
delete a row it claims to satisfy without a separately reviewed, human-owner-
approved contract delta. Evidence packets are generated from test artifacts by
CI rather than authored by the implementer (`RC-GATE-001..002`).

Pre-G0 work is a narrow exception, not an unversioned loophole. Only P1.0A,
P1.0C, P1.0D, and P1.1A may land against the immutable independently reviewed
candidate digest, and only when they add no owner, schema, command, or provider-
effect path and do not weaken a cited row. P1.0D may only fail closed after
acknowledgement ambiguity; it cannot claim durable acceptance or an operation
ID. P1.1A may only make already-discoverable writes checked and move success
events after proven state; it cannot claim empty-memory discovery of a closed
incomplete root. Their red/green evidence is imported into the eventual G0 base
manifest. Every other implementation waits for P0.16/G0.

## 1. Outcome

The reconciler's normal scheduling unit becomes one stable key, not one city.
Every locally committed or feed-certified change that can alter actionability
marks the smallest affected key dirty. Audit-only external stores rediscover the
change within their declared recovery bound and cannot claim the low-latency
external-write profile. A ready lifecycle action never waits for a patrol
interval, a full desired-state rebuild, or unrelated maintenance.

The end state is:

```text
durable bead/config commit                 runtime observation
          │                                       │
          ├─ same-process post-commit hint        ├─ provider watch
          └─ resumable per-store change feed      └─ bounded observer
                              │
                    serialized cache appliers
                              │
                 typed objects + secondary indexes
                              │
                    affected stable keys only
                              │
                  typed keyed workqueues
                              │
                 pure latest-state reconciliation
                              │
            shared per-session mutation executor
                              │
         generation/incarnation-fenced `worker.Handle`
                              │
             provider acknowledgement/observation
                              │
                         requeue key

startup snapshot + cursor-gap relist + slow partitioned audit ───────┘
```

Mutating CLI/API calls enter through the same durable intent/command front
door. A managed CLI is never a second runtime writer merely because a socket
probe failed. An unmanaged CLI may run the same executor as a one-shot owner
only after an exclusive live claim. Pokes and socket replies are latency hints
and acknowledgements, not ownership or command truth (`RC-CLI-001..005`).

“Runtime incarnation” is deliberately plural. A provisioned box and the agent
launch inside it have separate identities. Stop/teardown fence the box;
nudge/interrupt/prompt/readiness/drain acknowledgement fence the launch. A
provider effect is admitted only for a capability profile that can validate the
required identity at entry (`RC-ID-001..004`).

The patrol becomes a repair mechanism. It never remains the normal path by
which an action starts.

## 2. What “perfect” means here

Literal zero latency, infinite capacity, and exactly-once external side effects
are impossible contracts. This plan uses stronger, testable definitions.

### Safety

- No destructive action is constructible from `Unknown`, stale, partial, or
  errored observation.
- A delayed action that has not entered a provider cannot enter for a stale
  `intent_generation`, and a stale result cannot commit. An effect already
  accepted by a provider may finish after supersession and must be observed and
  corrected unless provider cancellation/fencing makes it harmless.
- At most one mutation is locally dispatched for a session key and controller
  epoch. Cross-process/late-call overlap is prevented only by cooperative
  cancellation, provider fencing/deduplication, or killable isolation; the
  strongest profile requires one of those mechanisms.
- A caller timeout may release a waiting CLI/worker, but never the actual
  same-key/provider permit while an entered call can still mutate. Managed CLI
  and controller paths share that ownership; acknowledgement loss cannot create
  a second writer (`RC-CLI-001..005`, `RC-TMUX-003`).
- Box and launch identities are independently revalidated at provider entry.
  Check-then-effect against a reusable tmux name or PID is not certified as an
  exact-incarnation effect. The strongest local profile requires a provider-
  atomic conditional target and pidfd/equivalent managed scope
  (`RC-ID-001..004`, `RC-PROC-001..003`).
- A controller crash after an ambiguous provider effect causes re-observation
  and adoption, not blind repetition.
- Every accepted edge command remains durable until it has a visible terminal
  outcome.

### Liveness

- Every committed and unblocked need is eventually reconciled while capacity
  and its dependencies are available.
- A lost notification cannot lose work: the resumable feed or audit rediscovers
  it.
- A key dirtied while it is processing receives a follow-up reconciliation.
- A new intent generation or the exact blocker-recovery revision bypasses retry
  delay inherited from an obsolete cause. Irrelevant same-generation events do
  not erase provider-error backoff (`RC-QUEUE-001..003`).

### Scalability

- Steady-state cost is proportional to changed objects and their affected keys,
  not total fleet size.
- Different session and pool keys reconcile concurrently within explicit
  provider and storage bounds.
- A hot rig, provider, retry stream, or maintenance task cannot consume all
  interactive capacity.
- Caches, queues, and audits partition by stable keys and can later move between
  owners without changing domain semantics.

### Entropy resistance

Correctness has two prompt ingress routes and one independently traversed
checker:

1. Same-process post-commit application gives minimum latency.
2. A resumable revisioned feed is the normal durable recovery path.
3. An authoritative snapshot audit independently enumerates raw objects and
   folds a fresh projection. It reuses only the canonical total decoder and pure
   `Contributions(object)` semantics, detects supported projection divergence,
   repairs mechanically recoverable cache/index faults, and fails closed plus
   alarms on persistent, partial, authoritative, or common-mode uncertainty.

The first two routes share the serialized applier and key mappers, so they are
not independent implementations. No ingress route is trusted without the audit.
An audit repair is observable and normally zero.

### Operability

- An operator can trace one intent from request through commit, detection,
  queueing, provider entry, acknowledgement, and convergence.
- Capacity, dependency, retry, durability, and provider delays are reported
  separately.
- Every rollout step has a deterministic rollback path and retains the legacy
  reader until the new owner has completed a production soak.

### Fault envelope and explicit non-guarantees

“Entropy-resistant” has one precise meaning in this plan:

> After any finite sequence of supported crashes, missed or duplicated wakeups,
> stale observations, cache/index corruption, provider outages, provider-
> supported out-of-band mutations, and controller replacement, once faults stop and the authoritative
> store/runtime are available, the system converges within its declared
> recovery bound to the latest durable intent, without a stale destructive
> action or a lost accepted edge command.

The guarantee is conditioned on fair worker scheduling and the certified
store/provider profile. It does not claim survival of permanent authoritative
data loss, Byzantine or compromised providers, an unbounded arrival rate,
network partitions without a linearizable coordination store, or exactly-once
external effects from a provider that cannot accept an idempotency/fencing key.
Those conditions fail closed or select a weaker, explicitly named support
profile. They are never silently marketed as the high-risk profile.

A wholesale authoritative-store restore is not an ordinary cache/feed rewind.
No implementation can infer an undetectable restore of an identical backup
without an independently retained monotonic anchor. Certified operation
therefore requires a store UUID, restore epoch/lineage, schema version, and
revision/epoch high-water evidence checked at startup, feed attach, and before
effect admission. An out-of-band reset without that proof freezes effects and
enters explicit recovery; recovered nonterminal edge commands and every
effectful level intent are quarantined until re-anchored or deliberately reissued
(`RC-STORE-001..003`).

| Profile | Minimum capabilities | Claims available |
|---|---|---|
| Compatibility single-owner | Durable beads, canonical projection, tri-state observation, one live owner, bounded authoritative audit | Fail-safe uncertainty and bounded rediscovery; no exact-target claim under concurrent out-of-band replacement, no external-write low-latency claim |
| Exact-target single-owner | Compatibility profile plus separate box/launch identities and provider-atomic conditional target/process handle | Stale-generation/incarnation effects are refused for the exact certified provider/OS topology |
| Single-owner low-latency | Exact-target profile plus conforming snapshot/feed and runtime watch/bounded observer | Steady-state commit/detect-to-provider SLO and bounded gap/restart RTO |
| Deduplicated command | Low-latency profile plus provider command-ID acceptance/dedup | Deduplicated nudge acceptance; still no generic exactly-once downstream effect claim |
| Fenced HA | Low-latency profile plus linear conditional store, lease time model, shard feed/cache, and provider/status epoch enforcement | Advertised split-brain/failover guarantees only for the exact certified topology |

Every checkpoint and metric report names its profile. Unsupported capability
selects a weaker profile or refuses startup; it never silently weakens one of
these claims.

Control authorization is a separate causal-path capability. In the local
single-tenant compatibility profile, granting a principal raw write access to
the command store explicitly grants full session-control authority and is
reported as such. Hosted/multi-tenant profiles require a trusted front door
that stamps non-self-asserted requester provenance, a claim-time authorization
decision, and distinct work-plane credentials that cannot create or rewrite
protected lifecycle/nudge/control commands. Direct or unpinned external `bd`
writers are outside those profiles. No witness proves requester authority; it
only proves the target (`RC-AUTH-001..003`).

Profiles are resolved per action-family causal path, not once per city. A live
topology may combine a bd-sqlite city/graph store, Dolt or DoltLite rig stores,
no-history/command storage, and wrappers; its claim is the weakest required
store/provider capability on that action's path. Evidence reports every
component verdict and the derived composite profile. A fast MemStore or one
feed-capable rig cannot upgrade a slower/unsupported command or city store.

## 3. Current baseline

The current implementation is a saturated serial city loop:

- `cmd/gc/city_runtime.go` is approximately 3,500 lines and hosts patrol,
  poke, nudge, control dispatch, convergence, order work, reload, GC, and
  lifecycle activity in one `select` loop.
- `cmd/gc/build_desired_state.go` is approximately 4,300 lines and performs
  global reads plus idempotent writes while described as a desired-state build.
- `cmd/gc/session_reconciler.go` is approximately 5,600 lines.
- `cmd/gc/session_lifecycle_parallel.go` already proves bounded cross-session
  provider concurrency and dependency-wave execution, but candidate planning
  still waits behind the global pass.
- `internal/session` already owns the canonical lifecycle projection, pure
  timer/exit decisions, transition patches, typed `Info` decoding at the store
  edge, `ReconcileSession`, and the reconcile snapshot front door. The
  extraction ledger in `internal/session/REQUIREMENTS.md` is the behavior
  source of truth; stale pre-`25d395fc0` extraction branches are archaeology,
  not port candidates.
- `cmd/gc/session_level_converge.go` and its shadow harness contain a dormant
  pure action vocabulary that can be grown behind parity checks.
- `internal/beads/CachingStore` already has write-through mutation, external
  event application, mutation fencing, and a watchdog scan. It is not yet a
  no-gap informer because its external event path is best-effort.
- Runtime providers already expose `Runtime`/`Place` and
  `Transport`/`Attachment` seams. `runtime.ProbeResult` already names
  `Alive`/`Dead`/`Unknown`, but legacy adapters, `session.Manager`, and
  `worker.LiveObservation` collapse it back to booleans. Observation work
  extends those seams and the worker boundary; it does not add a second
  tri-state vocabulary.
- The reviewed conditional-writes series ending at `1f8596d1c` implements an optional
  `beads.ConditionalWriter`, opaque per-bead revisions, value CAS, cache
  invalidation on conflicts, provider conformance tests, and rollout modes.
  It is archaeology based on the landed typed-session head and must be replayed
  in small slices onto the execution head; generated API artifacts are
  regenerated rather than conflict-resolved by hand. It remains single-working-
  set optimistic concurrency, not distributed consensus.

Production evidence from 46 completed cycles in the 2026-07-12 trace segment:

| Metric | Duration |
|---|---:|
| p50 cycle | 142.889s |
| p90 cycle | 194.355s |
| p95 cycle | 221.307s |
| p99 cycle | 255.390s |
| maximum cycle | 256.644s |
| mean cycle | 137.800s |

In the maximum cycle, drain/stop finalization consumed 114.120s; demand began
around 120.6s; planned starts did not begin until roughly 202.5s. Faster ticker
selection cannot remove that head-of-line blocking.

## 4. Architectural decisions

### 4.1 Preserve the Windshield correctness kernel

Keep and strengthen:

- total typed lifecycle state based on the current `internal/session`
  vocabulary;
- pure fact-to-decision functions with no store, provider, clock, or ambient
  process access;
- `Unknown` as a first-class observation that can only defer;
- proof-carrying destructive effects;
- two-phase intent around lossy effects;
- one owned status writer, marker-last ordering, and drift detection;
- effect-boundary differential comparison;
- real-provider conformance at the observation/effect seam.

Do not create a second `sessioncodec` or a competing 13-state taxonomy. Extend
`ProjectLifecycle`, the scenario ledger, and existing typed transition inputs.

### 4.2 Replace Windshield's scheduler

Reject fleet-global serial apply, enqueue-all-on-every-tick, and “no worker
pool.” The replacement is:

- stable domain keys;
- Kubernetes-style dirty/processing queue semantics;
- one in-flight reconciliation per key;
- bounded concurrency across keys;
- one standard stingy workqueue per controller, with a shared per-session
  mutation executor;
- per-provider capacity limits;
- delayed retry without sleeping workers;
- permanent slow anti-entropy outside the action path.

### 4.3 Queue keys, never actions

An in-memory work item contains identity only:

```text
{city, store-class/scope, controller-kind, stable-id}
```

The worker reads the newest cached state when it runs. Precomputed actions,
snapshots, event bodies, and counts are never queued. Queue loss is harmless
because desired state and edge commands remain durable.

### 4.4 Separate level state from edge commands

| Need | Durable representation | Queue behavior |
|---|---|---|
| Start/stop/sleep/wake/close target | One total versioned lifecycle intent with request ID, desired phase, and monotonic intent generation | Latest valid generation wins; retrying one request does not mint another |
| Pool allocation | Desired count plus pool `intent_generation` | Latest state wins |
| Nudge | Immutable command ID, target, payload, sequence, incarnation fence, outcome | Wake key coalesces; commands do not |
| Deadline/retry | Ephemeral delayed key tied to retry class, intent generation, and exact blocker/provider-health revision | New intent or exact blocker recovery wakes immediately; duplicates do not erase backoff |

Exactly-once nudge delivery is claimed only for providers that accept and
deduplicate the durable command ID. Other providers expose bounded at-least-once
or at-most-once delivery honestly.

### 4.5 Separate detection controllers from provider mutation

Session lifecycle, nudge delivery, pool demand, formula/control dispatch, and
maintenance retain separate queues, retry rules, and capacity budgets. Any path
that mutates a session runtime passes through one shared per-session executor.
Managed CLI/API commands submit to that owner rather than falling back to direct
mutation. Separate queues and processes must never become separate writers for
the same session. Provider-global operations such as city tmux-server teardown
also acquire an admission barrier that stops new per-session entries and drains
actual in-flight calls first (`RC-CLI-001..005`, `RC-SHUT-001..005`).

### 4.6 Use conditional writes where they are real

- Land and graduate the existing conditional-writes work instead of creating a
  second revision scheme.
- Use whole-row revision preconditions for owned status updates and metadata-key
  CAS for claims/leases.
- Where metadata-key CAS is emulated by a whole-bead revision, every claim,
  lease, or reservation key lives on a dedicated low-write bead whose only
  writers are that coordination operation's participants. It is never colocated
  with owned status or unrelated high-frequency metadata.
- Keep the `status_body_hash` torn-write marker until every enabled store proves
  atomic whole-status writes.
- In single-controller compatibility mode, unsupported CAS may degrade loudly
  to reread plus idempotent convergence.
- Key sharding or multi-controller execution requires `require` mode and a
  linear single working set. No fallback is allowed there.
- A per-bead revision is not a resumable store cursor; change feeds need a
  separate commit-sequenced contract.

### 4.7 Keep provider details behind provider boundaries

Runtime incarnation identity is provider-specific but has two semantic levels:

- box/provision identity, used by stop and teardown;
- launch/agent identity, used by ready, prompt, nudge, interrupt, interaction,
  and drain acknowledgement.

These are provider-owned opaque scoped witnesses, not universal structs full of
tmux/PID fields. Warm/separable providers must distinguish both. A conjoined
provider may return the same opaque identity for both scopes when its capability
contract proves they share one lifetime.

The provider-specific evidence is:

- tmux: explicit city-server identity plus immutable session/pane IDs and
  caller-owned box/launch tokens, conditionally validated in the same server-
  side command as the effect; raw reusable name targeting is insufficient;
- subprocess: caller-owned tokens plus an atomic process handle/managed scope;
  PID, start time, and command identity are scan evidence but a check-then-signal
  race is not the strongest profile;
- Kubernetes: object UID;
- T3 bridge: durable thread/session incarnation;
- other providers: an equally strong token or an explicit inability to support
  the requested destructive profile.

`Start` is non-destructive on collision. It never hides zombie/dead-pane cleanup
or treats another creator's same-name runtime as success without exact operation
adoption. Replace/relaunch/cleanup are separate witnessed effects. Every effect
accepts context and returns a typed last-proven stage; deadline alone does not
mean the provider stopped (`RC-TMUX-001..003`, `RC-START-001..005`).
The safety kernel needs only `not-entered`, `may-have-entered`, and
`observed-exact`; provider acceptance/dedup/substage detail is added only where
the real provider exposes it and otherwise stays telemetry.

### 4.8 Keep events observational

Events are fired after durable facts for humans and agents to observe. They may
accelerate cache invalidation, but they are not a fence, command ledger, or the
only source of reconcile correctness.

### 4.9 Keep the current trace until replacement is proven

The trace stream is the production evidence and incident-corpus stream during
migration; it is not a complete parity oracle. The mandatory effect boundary in
Phase 3 becomes the oracle. A smaller flight recorder may run beside the trace,
but deletion waits until every operator workflow, API consumer, parity reader,
and incident tool has migrated and completed at least one full release of
shadow operation.

### 4.10 Use one name per ordering domain

The implementation and wire schemas never use bare `generation` or `epoch`:

| Name | Meaning |
|---|---|
| `intent_generation` | Monotonic latest desired lifecycle/pool generation for one stable domain key |
| `config_generation` | Immutable validated configuration publication sequence |
| `projection_generation` | Process-local immutable cache/index publication sequence; disposable on restart |
| `runtime_observation_revision` | Provider-owned or manager-assigned ordering for one immutable runtime observation batch |
| `source_revision` / `source_cursor` | Store-owned object revision or resumable feed position; not interchangeable |
| `blocker_revision` / `capacity_revision` / `provider_health_revision` | Exact semantic revision that registered or cleared dependency/capacity/provider-health eligibility |
| `city_admission_generation` | Monotonic generation of the provider-global admit/drain/shutdown barrier |
| `force_revision` | Monotonic conditional upgrade revision on one immutable operation; never a new request or intent generation |
| `continuation_identity` | Equality-only nudge/work continuation binding; never an ordering epoch |
| `drain_operation_id` | Stable operation identity for one drain attempt/acknowledgement sequence; not a free-running generation |
| `retry_cause_revision` | Exact class + intent/blocker/provider-health tuple whose attempts/backoff are counted |
| `store_uuid` / `restore_epoch` | Provisioned working-set identity and externally anchored restore lineage; equality/monotonic recovery fence, not an object revision |
| `store_schema_version` | Certified external-store shape written by the pinned provider/tool; never inferred only from successful decoding |
| `status_body_hash` | Torn-write checksum marker for one canonical status body; never an ordering generation |
| `box_identity` / `launch_identity` | Provider-owned scoped incarnation values; equality only |
| `owner_epoch` | Per-key-comparable HA fencing tuple `(shard_map_version, shard_epoch)` introduced only in Phase 11 |
| `cold_owner_instance` | Pre-HA process/install ownership token checked at native entry; it is not an expiring lease epoch |
| `operation_id` | Stable idempotency/correlation identity; retries do not change it |

Every task, trace, API field, and test uses the qualified name. Conversion
between domains is explicit and cannot compare a hash, cursor, incarnation, and
monotonic counter as if they were one clock.

## 5. Target boundaries

The import DAG is frozen before the first type lands:

```text
internal/operation    internal/reconcilekey
       │                       │
       ├──── internal/runtime  ├──── internal/beads projection callbacks
       ├──── internal/session  └──── cmd/gc controller composition
       └──── internal/worker
                    \              /
                         cmd/gc
```

`internal/operation` contains only cross-boundary operation identity, native
effect-stage, and timeline value types; it contains no domain decisions or
provider I/O. `internal/reconcilekey` contains versioned stable key/fanout value
types and no queue. Lifecycle actions remain in `internal/session`, nudge
commands remain in `internal/nudgequeue`, provider evidence remains in
`internal/runtime`, and `cmd/gc` only composes/adapts them. A lower package never
imports `cmd/gc` to obtain a canonical type.

| Boundary | Owns | Must not own |
|---|---|---|
| `internal/operation` | Cross-boundary operation ID, caller/provider stage and correlated timeline value types | Domain action policy, queueing, provider I/O, wire output |
| `internal/reconcilekey` | Stable versioned controller key and scoped-fanout value types | Queue ownership, store/runtime I/O, domain decisions |
| `internal/session` | Lifecycle vocabulary, durable lifecycle-request semantics/front door, fact structs, pure decisions, transition plans, action generations, session mutation contracts | Store discovery, config loading, provider I/O, queue scheduling |
| `internal/nudgequeue` | Sole durable nudge command/codec, target policy, claim/terminal conservation | Provider I/O, session lifecycle decisions, second sidecar/file ledger |
| `internal/beads` | Store CRUD, conditional-write capability, snapshot/change-feed capability, cache application, typed object revisions | Session decisions, event-bus policy, Dolt assumptions outside providers |
| `internal/runtime` | Context-aware provider operations, composite typed observations, separate box/launch identities, conditional target capability, optional watches | Durable desired state or session policy; hidden collision cleanup |
| `internal/worker` | Canonical session effect boundary, actual-call/permit ownership, operation-stage telemetry | Reconciliation judgment or global scheduling |
| `cmd/gc` composition | CLI/API adapters over domain request front doors, per-city controller lifecycle, cache/watch wiring, keyed queues, bounded admission, rollout ownership | Durable request semantics, duplicate session/domain rules, raw lifecycle writes, or managed direct provider mutation |
| Event bus | Post-commit outbound observation | Authoritative mutation feed or command durability |

Generic scheduler mechanics are extracted only after at least two domain
controllers use the same proven behavior. The first controller may use
`client-go/util/workqueue` directly with domain-local wiring.

## 6. Stable controller keys

| Controller | Key | Enqueued by |
|---|---|---|
| Session lifecycle | session bead/canonical session ID | session desired/status change, relevant config generation, runtime observation, dependency/capacity change, deadline |
| Nudge delivery | target session ID | durable nudge create/retry, runtime readiness, quiescence deadline |
| Pool demand | qualified pool/template plus scope/store class | relevant work readiness/route/assignee change, session membership, config, external scale-check deadline |
| Formula/control | run or control-bead ID | graph/control bead change, dependency completion, retry deadline |
| Order | qualified order ID and scope | schedule deadline, relevant event, config generation |
| Maintenance | partition/store ID | timer, store health transition, manual request |

Broad or unclassified changes may enqueue every key in one affected scope as a
safe fallback. Each source class has a certified maximum fanout and fallback-
rate budget; breaching it aborts expansion. The top recurring config-edit
shapes require exact-key mappers before their family graduates.

## 7. Action and observation model

### 7.1 Correlated timeline

Every accepted intent records or derives these stages:

```text
T0 request received
T1 durable commit
T2 change detected
T3 cache and indexes applied
T4 key enqueued
T5 reconcile dequeued
T6 worker.Handle entered
T7 provider adapter entered
T8 provider-native mutation entered      <- effect may have occurred; action started
T9 provider accepted/returned with last-proven stage
T10 matching runtime/status observed
T11 converged
```

The primary `schedule_to_start` measure is `T8 - action_ready_at`, where
`action_ready_at` is the later of durable commit and the controller-observed,
durably attributable removal of the last blocker. Request-to-provider
`T0 -> T8`, request-to-commit, feed lag, queue delay, worker-boundary delay,
provider-adapter staging, provider duration, and convergence are all reported
separately. Each action/provider profile declares the exact native-entry
milestone: the first boundary after which its external mutation may have
occurred. Lock acquisition, file staging, debounce, and subprocess setup before
that boundary cannot satisfy the action-start SLO.

`action_ready_at` is operational, never inferred after the fact. For a durable
dependency/config/command change it is the authoritative commit/cursor time. For
capacity, retry, or provider-health recovery it is a mandatory monotonic
in-process clearing instant correlated to the exact `capacity_revision`,
`retry_cause_revision`, or `provider_health_revision`. Every blocked interval records its
class, start, clearing evidence, and duration. A missing/unknown ready proof is
reported as unknown and cannot be excluded from the outer `T0 -> T8`
distribution.

Performance has two gates:

1. **Controller overhead:** final required durable operation commit or exact
   blocker-clear (whichever is later) to T8. This is the `<100ms` initial and
   `<50ms` target fast-path gate.
2. **Need-to-native-entry:** T0/change-detected to T8. Its certified bound is
   controller overhead plus the explicit count × p99 budget of any required
   durable writes for that store/profile. This outer distribution is always
   reported and prevents blocker/store classification from hiding user-visible
   lag.

The single-owner design minimizes pre-entry writes: caller/domain intent already
contains the operation ID and exact target/recovery anchor when it commits.
Another durable claim is required only when the action/profile truly needs
cross-process claiming or a later target binding. Deduplicated-command and HA
profiles may therefore have a larger declared write budget than the local
single-owner path.

### 7.2 Per-session action fence

Every provider mutation carries:

- stable session ID;
- `intent_generation`;
- expected box incarnation, when the effect targets the box;
- expected launch incarnation, when the effect targets the agent/interaction;
- one durable operation/request ID with deterministic step/attempt suffixes
  where needed;
- `owner_epoch` when HA is enabled;
- bounded deadline.

The executor revalidates the generation and applicable incarnation immediately
before provider entry. The provider conditionally validates its native target in
the same atomic effect when the certified profile requires exact targeting. A
stale action completes as a typed no-op and requeues the key.

### 7.3 Expectations

Install an in-flight expectation for the exact generation/box/launch identity
before provider-native entry, anchored by the already-durable action intent.
When a native effect can survive controller death, its operation/target/claim
state is durable before entry and its family supplies at least one production
recovery mechanism: provider operation lookup/deduplication, an atomic fence,
or killable provider-owned containment. The test recorder is evidence, not this
production ownership record. Definite
pre-entry/definite provider failure clears it; acceptance or ambiguous completion
retains it; matching observation satisfies it. Expiry triggers a targeted
authoritative probe, never a fleet scan or blind retry. A caller deadline does
not prove that a provider call stopped: until cooperative cancellation,
provider-side fencing, or killable isolation proves the old call can no longer
act, the exact key remains `AmbiguousInFlight` and cannot enter another provider
mutation. Its waiting worker/CLI may return, but the actual call owner and
provider permit remain charged; repeated caller timeouts cannot accumulate
detached mutations or goroutines.

After controller `SIGKILL`, the fresh process reconstructs unresolved native
entries from the durable operation/claim plus provider/runtime observation. If
it cannot prove `not_entered`, adopt the exact operation, or atomically fence the
old effect, it blocks overlap and the provider/action cannot claim the high-risk
profile. A released process-local lock or empty in-memory permit is never proof
that an orphan native effect stopped.

After a backing-store write returns, its key cannot reconcile from a cache view
known to precede the returned revision/cursor. Failed same-process application
blocks the key behind a write watermark until feed catch-up or an authoritative
reread satisfies that watermark. A completed serialized authoritative reread of
the key's full object set clears the watermark even when revision tokens are
incomparable—the reread is current truth. A completed consistent gap/relist
clears every watermark for that store. Stores that expose no trustworthy
revision never install revision watermarks; they serialize apply/reread and
advertise the weaker profile. Watermark age is visible and exceeding its
profile bound blocks/aborts that family rather than silently wedging a key.

### 7.4 Blockers

Blocked keys leave runnable queues and register against a condition:

- dependency readiness;
- provider health;
- provider/global capacity;
- ambiguous own-call completion or provider-operation resolution;
- held/quarantined deadline;
- retry deadline;
- config or identity conflict.
- human attachment/copy-mode interaction conflict for raw key-injection
  families.

Registration records the condition's source revision and rechecks after the
index update, closing the “condition changed just before registration” lost-
wakeup race. A new semantic source revision for a condition-parked key atomically
clears that park and re-admits latest-state evaluation; it may immediately
register the still-valid blocker again. Retry-delay eligibility is stricter:
only new intent or the registered blocker/provider-health recovery revision
bypasses the delay, while irrelevant duplicates retain dirty replay without
erasing backoff. The actual call owner enqueues the key and releases its
per-provider charge when a detached/ambiguous call really returns or becomes
fenced. Timer conditions use a delay queue or timer heap, not sleeping workers.

## 8. Executable invariants

The implementation and test suites must make these contracts explicit.

1. **INV-01** — In a feed-certified profile, every successful durable mutation either
   appears in a resumable change stream or causes a typed gap that forces
   relist. An audit-only profile instead guarantees rediscovery within its
   declared recovery bound.
2. **INV-02** — Same-process application and later watch replay are semantically idempotent.
3. **INV-03** — A change known to precede applied state never overwrites it; incomparable
   ordering causes an authoritative reread or typed gap/relist. Deletes carry
   tombstones and stale updates cannot resurrect them.
4. **INV-04** — One source commit batch—its objects, tombstones, and every affected
   secondary index—is published atomically from readers' perspective before
   its dirty keys/cursor are released. Cross-store atomicity is not promised.
5. **INV-05** — Every index continuously matches a fresh full-snapshot fold that is
   independent of incremental cursors, cached object sets, derived indexes, and
   incremental appliers while reusing canonical decoding and pure contribution
   semantics.
6. **INV-06** — Every relevant object/config/runtime change maps to every affected key.
7. **INV-07** — The same `(logical controller, key)` pair is never reconciled
   concurrently. Distinct controllers may reconcile the same session key;
   provider mutation is exclusive only through the shared per-session executor
   and durable mutation is exclusive through owned field groups/conditional
   writes.
8. **INV-08** — Adds during one processing interval coalesce into one dirty replay for that
   interval; later adds remain eligible for later passes and are never lost.
9. **INV-09** — Dirty does not imply runnable: irrelevant same-generation duplicates cannot
   bypass the active retry cause or exact blocker revision.
10. **INV-10** — No provider call starts without a current `intent_generation` and applicable
    box/launch identity check.
11. **INV-11** — No destructive call starts without a fresh witness and a provider-native
    target mechanism certified for the exact topology; check-then-name/PID is
    not silently labeled exact.
12. **INV-12** — Provider acceptance followed by process death is recovered by observation.
13. **INV-13** — Queue loss and controller restart do not lose desired work or edge commands.
14. **INV-14** — A blocked key cannot miss the transition that unblocks it.
15. **INV-15** — Marking a parked/blocked key dirty from any source atomically
    re-admits it to runnable evaluation. A condition registration suppresses
    only timer-less polling; it never swallows fresh intent, audit, or unrelated
    semantic dirtiness.
16. **INV-16** — A hot retry or tenant cannot consume all fresh interactive capacity.
17. **INV-17** — A caller timeout never releases actual-call ownership while a provider can
    still mutate, and repeated timeouts cannot leak unbounded goroutines, file
    descriptors, or permits.
18. **INV-18** — Managed CLI/API mutation never bypasses the durable owner because a poke,
    socket reply, or controller probe failed.
19. **INV-19** — A local projection or differential shadow advances only after a proven
    commit; a failed/ambiguous write cannot feed later same-pass decisions.
20. **INV-20** — Feed poison never advances the cursor; it causes typed unsynced/relist or a
    bounded terminal alarm, never skip or relist treadmill.
21. **INV-21** — Anti-entropy repairs are idempotent, rate-limited, and never execute
    lifecycle actions inline.
22. **INV-22** — Old and new action owners are never simultaneously active for one action
    family.
23. **INV-23** — During mixed-family migration, every lifecycle metadata
    mutation uses one physical per-session writer and every provider mutation
    uses one shared per-session executor across legacy, keyed, CLI, and sidecar
    processes.
24. **INV-24** — Unrecognized newer lifecycle state defers safely during mixed-version
    rollout.
25. **INV-25** — Domain/convergence events describing a mutation occur after its durable
    commit. Pre-commit provider-attempt telemetry is typed as an attempt and can
    never be consumed as domain success.
26. **INV-26** — Every accepted nudge ID reaches one durable terminal result or returns
    pending; provider ambiguity never causes blind replay on a non-deduplicating
    transport.
27. **INV-27** — Every lifecycle desired-state write goes through the total versioned
    request/intent front door; torn compatibility mirrors decode as `Unknown`.
28. **INV-28** — An external effect that may survive controller death has a
    durable operation/target/claim before native entry plus provider lookup/
    deduplication, atomic fencing, or killable containment; empty process memory
    never authorizes overlap.
29. **INV-29** — A durable control command is executable only when its trusted,
    non-self-asserted requester provenance and current policy authorize that
    command, target, tenant/city, and payload. Target witnesses do not substitute
    for requester authority.
30. **INV-30** — Store UUID, restore lineage, or authoritative high-water
    regression freezes effect admission. Recovered edge commands and all
    effectful intents cannot execute until an explicit recovery operation re-anchors or
    reissues them above the retained high-water mark.
31. **INV-31** — A store-schema or external-writer-version mismatch is detected
    below decoding and selects observation-only/refusal; well-formed-but-wrong
    rows cannot authorize effects.
32. **INV-32** — The ratified contract hash and protected acceptance rows cannot
    be weakened by the same implementation principal whose change they gate;
    release evidence is CI-derived, content-addressed, and provenance-verified.
33. **INV-33** — Raw tmux key injection never interleaves with an unapproved
    human-attached/copy-mode interaction. Controller-originated injection is
    attributed separately and cannot reset agent-idle/quiescence evidence as if
    it were agent output.
34. **INV-34** — Every reachable composite blocked-key state has at least one
    admissible wake path. Controller-owned blockers resolve on a bounded edge;
    external blockers have bounded change detection and age escalation but may
    persist until reality changes. Classified blocked states alarm independently
    of runnable queue age.

## 9. Failure model and recovery owner

| Failure | Required recovery |
|---|---|
| Commit succeeds; local enqueue fails | Feed-certified profile replays it; audit-only profile's bounded relist rediscovers it; audit remains final checker |
| Local apply and feed both deliver | Revision/idempotent applier drops duplicate semantics |
| Watch disconnects | Resume after last cursor; backoff does not block local commits |
| Feed record is corrupt/unsupported | Do not advance; mark source unsynced; install a consistent relist or remain bounded-terminal with alarm/profile downgrade |
| Cursor expired/compacted | Stop incremental application, relist consistent snapshot, replace indexes, resume from returned cursor |
| Partial/out-of-order store delivery | Per-object revision and tombstone rules reject regression; unsupported ordering returns gap |
| Cache process crashes | Rebuild from snapshot plus feed; queues are repopulated from mismatches and pending commands |
| Controller dies before provider call | Durable intent remains and is re-enqueued |
| Controller dies after provider effect | Runtime observation adopts or retries based on exact incarnation and command ID |
| Desired state changes during effect | Old result cannot commit across generation/CAS fence; observe and reconcile latest |
| Stop races replacement with same name | Provider-atomic immutable target/incarnation condition rejects the old stop; unsupported tmux/OS topology cannot claim this profile |
| Provider hangs | Deadline yields `AmbiguousInFlight` and schedules targeted observation; the worker returns, but the key stays mutation-blocked and the provider permit stays charged until the call ends or a fence/isolated transport proves it cannot act |
| Managed CLI loses controller acknowledgement | Read the durable request by ID; return accepted/incomplete or unknown; never enter a direct provider fallback |
| Durable command lacks trusted requester provenance or current authorization | Reject before claim/provider entry with a durable typed denial; never trust requester fields supplied inside the command body |
| Status/intent write returns error or loses response | Keep local projection unchanged until authoritative reread proves the commit; no later same-pass effect consumes the proposal |
| Store identity/restore epoch/revision regresses | Stop effect admission, force authoritative relist, quarantine pre-recovery nonterminal commands and every effectful level intent, and require explicit re-anchor/reissue above an independent high-water mark |
| External `bd` binary/store schema is uncertified or changes while running | Mark the source schema-mismatched and observation-only; do not let a permissive decoder or audit certify it |
| Store write conflicts | Re-read latest state, reset obsolete retry state, and reconcile; never overwrite blindly |
| One rig floods keys | Start with the standard queue and provider/session bounds; add the smallest measured per-flow cap or reserved permit only after a reproducible starvation test |
| Maintenance is continuously preempted | Separate maintenance controller/queue plus a measured minimum provider/store budget; accepted durable work is never dropped |
| Anti-entropy finds drift | Repair projection, enqueue affected key, emit high-signal metric/event; never act directly |
| Two controllers overlap | Before HA: startup refusal. After HA: lease + monotonic fence + provider enforcement |
| Provider cannot enforce fences | Provider/city is not eligible for active-active execution |
| Human attaches or enters copy mode before raw key injection | Apply the action-family interaction policy atomically at native entry: queue/park, typed refusal, or explicit force on the same operation; never silently interleave |

## 10. Performance and capacity contract

### SLOs

- Initial controller-overhead gate: p99 unblocked final-required-operation-
  commit/blocker-clear to provider-native-entry below 100ms
  when a provider slot is continuously available.
- Target controller-overhead gate: p99 local final-required-operation-commit/
  blocker-clear to provider-native-entry below 50ms
  under the same condition; `worker.Handle`, provider-adapter, and native-entry
  stages are also reported
  separately.
- The user-visible need/T0-to-native-entry gate is profile-specific: it adds the
  measured count × p99 of required post-need durable writes for the exact store
  class. P0.7 publishes that write budget for Mem/File, bd-sqlite, BdStore,
  NativeDolt, DoltLite, and every configured composite path. A profile cannot
  claim 50/100ms when its store budget makes that physically impossible.
- External writes report commit-to-feed-detection separately. A backend that
  cannot expose a trustworthy commit timestamp cannot claim an external-write
  commit-to-start SLO; it can claim only detection-to-start plus its measured
  audit recovery bound.
- Under the certified load and health envelope, any ready key older than 1s
  triggers an alert and must have exactly one explicit dependency, capacity,
  durability, retry, recovery, or overload classification. The SLO/error budget,
  rather than an impossible universal maximum, gates promotion.
- Anti-entropy repair count is zero during normal canary operation.
- Per-source scoped-fanout/enqueue-all fallback rates stay below the P0.7
  `RC-PERF-002` budgets; top recurring config shapes use exact mapping.
- Each nudge provider meets its declared proven-delivery or proven-injection
  quality ratio and ambiguity-age threshold from `RC-NUDGE-009`.

Promotion gates both controller-overhead p99 and per-blocker-class blocked-time
budgets, while retaining the unconditioned T0/change-to-T8 histogram as the
non-gameable outer signal.

Steady-state latency and reconnect/relist recovery latency are separate service
indicators; excluding a feed outage from the steady-state histogram must not
make it disappear from the recovery SLO or error budget. P0.7 pins the SLO
window, minimum sample count, certified admitted-load envelope, and exact sample
eligibility. Every excluded sample receives one visible reason; an unknown
reason is an error, not silent histogram loss. First-ready time survives
coalescing and requeues.

Separate RTO indicators cover retained-cursor reconnect, cursor-gap relist, cold
restart, warm failover, and shard transfer.

### Required metrics

- stage latency for T0–T11 by action class and provider, including request to
  provider-adapter and provider-native entry;
- oldest ready key and queue depth by controller; add flow metrics only when a
  measured fairness mechanism exists;
- workers active/max and provider semaphore saturation;
- store-head cursor versus applied cursor by store, plus reconnect/gap/relist;
- retry count and actual delay by reason;
- blocker age and unblock source;
- classified blocked-key age/detection/escalation by blocker class, including
  authorization, human interaction, requester fence, ambiguity, and watermark;
- scoped-fanout size and fallback count by bounded source class;
- stale-generation and stale-incarnation suppressions;
- expectation waits/expiries and ambiguous provider outcomes;
- anti-entropy checks, mismatches, and repairs;
- coalesced wake count and bounded nudge batch continuation;
- nudge accepted, delivered/duplicate, `injected_unconfirmed`,
  `delivery_unknown`, denied, expired, and superseded counts per provider
  profile;
- store UUID/restore/schema mismatch and protected-contract delta/provenance
  failures;
- controller/shard lease transitions and fencing conflicts when enabled.

Session IDs, bead IDs, and command IDs stay out of metric labels. They belong in
structured traces.

### Admission

- Reserve capacity before dispatching a provider effect; do not let workers
  pile up blocked on one provider semaphore.
- Keep domain controllers on separate standard workqueues. Retry eligibility is
  generation-aware and delayed without occupying a worker; maintenance has its
  own queue. Begin without borrowing, weights, aging, or a custom scheduler.
- If a deterministic admitted-load test proves starvation, add only the
  smallest bounded mechanism that makes that test pass—normally one reserved
  fresh permit or a provider/flow cap—and certify it before rollout. Progress
  claims assume admitted load or a finite backlog.
- Reserve correctness-plane capacity for feed/relist/audit and HA lease renewal;
  provider effects cannot starve their own discovery/fencing machinery.
- Durable intent remains accepted in beads when in-memory capacity is full.
  If durability itself cannot accept the request, return explicit overload.

## 11. Migration rules

1. TDD: failing characterization or contract test before behavior changes.
2. One action family and one owner flip per PR.
3. No action cutover without effect-boundary completeness for that family.
4. Shadow computes but never consumes a command or invokes a provider.
5. Legacy and new readers coexist; legacy and new provider-effect owners do not
   overlap for one family. During family-by-family decision migration, every
   lifecycle metadata mutation—legacy or keyed—uses the same P2.9/P2.10A
   per-session physical writer at the store's real atomicity unit. Per-family
   labels never justify concurrent whole-blob writers.
6. Additive metadata and wire changes first; destructive migrations last.
7. Every temporary rollout gate has an owner, expiry, version anchor, rollback
   command, and deletion task.
8. Every phase leaves the existing city runnable and the default path unchanged
   until its checkpoint is approved.
9. New behavior updates the relevant `SESSION-*` requirement rows and evidence
   in the same change.
10. Full scans remain until the independent change feed and audit path prove
    coverage; they are demoted only after evidence, never by assumption.
11. Production cutover begins with one low-risk city/provider/action family,
    expands by explicit gates, and includes an automatic abort threshold.
12. Legacy deletion waits at least one full release after default-on graduation.
13. Every implementation bead cites its `RC-*` acceptance rows and supplies a
    red test artifact; a broad phase label is not acceptance evidence.
14. Every destructive-family cutover requires, for that family, mechanical
    effect-site completeness, tri-state provenance, exact-target capability or
    explicit weaker profile, pre-entry generation/identity validation, and an
    injected ambiguous timeout.
15. A waiting timeout never transfers ownership. Handoff drains or fences the
    actual entered provider calls before another owner activates.
16. Inventory and N/N-1 proof are slice-local and cumulative: establish the
    base machinery in Phase 0, then extend it before each new effect or schema.
    A speculative inventory/matrix for all future code never blocks an
    independent fail-safe fix.

### 11.1 Acceptance-row ownership map

Architecture tasks may own a row through this map; every decomposed
implementation bead still cites the exact applicable IDs and candidate/G0 row
hashes. Checkpoint packets mechanically compare the evidence registry with this
map, so a category cannot disappear between prose and execution.

| Contract rows | Primary owning tasks/gates |
|---|---|
| `RC-CLI-001..010` | P0.13 characterization; P1.0D negative no-fallback proof; P2.11/P2.11A durable front door; P7.7A/P7.8 lifecycle; P9.5A shutdown |
| `RC-AUTH-001..003` | P0.14 trust boundary; P2.11/P6.1 implementation; P12.8 certification |
| `RC-OBS-001..007` | P1.2–P1.8 source/provider safety; P4.9–P4.10 observation manager |
| `RC-ID-001..004` | P1.4–P1.5 identities/witnesses; P1.12 atomic tmux targeting |
| `RC-PROC-001..003` | P1.11A partial-scan safety; P1.11B exact handles/containment |
| `RC-TMUX-001..003` | P1.9–P1.10 and P1.12; family adapters in P6/P7 |
| `RC-START-001..005` | P1.9 provider contract; P7.4A/P7.5/P7.6 cutover |
| `RC-STATE-001..003` | P1.0C; P2.0/P2.9/P2.11; claim/reservation consumers P6.1/P8.6/P11.3 |
| `RC-STORE-001..003` | P0.15; P2.0/P4.5 runtime enforcement; P12.6–P12.8 drills/certification |
| `RC-FEED-001..002` | P4.2/P4.4/P4.6 and G4B |
| `RC-QUEUE-001..005` | P5.2/P5.6 queue/retry; P5.11 composed liveness; P9.5A shutdown combinations |
| `RC-NUDGE-001..009` | P6.1–P6.6 and G6 |
| `RC-CLOSE-001..004` | P2.12 terminal convergence; P7.8 cutover |
| `RC-SHUT-001..005` | P1.10 primitives; P7.7A requester fence; P9.5A composed owner |
| `RC-EVENT-001..004` | P1.1A ordering; P1.1B durable recovery/publication; P2.11/P2.12/P6.5 domain commits; P3 effect oracle |
| `RC-CRASH-001..002` | P0.10 registry; P1.1B/P5.9 and every family fault table |
| `RC-ENTROPY-001..002` | P10.1–P10.7 and G10 |
| `RC-PERF-001..002` | P0.3/P0.4/P0.7 baselines; phase-specific PERF gates; P12.8 defaults |
| `RC-MIG-001..004` | P0.11/P0.12; P3.9 watermark parity; P5.4A bridge; P7.13 flips |
| `RC-CERT-001..003` | P0.9 evidence policy; P12.8 profile and P12.9 sustainability |
| `RC-GATE-001..002` | P0.16 protected lineage and every checkpoint packet |

New independent-review invariants are owned explicitly: `INV-29` by
P0.14/P2.11; `INV-30` by P0.15/P12.7; `INV-31` by P0.15/P2.0; `INV-32` by
P0.16; `INV-33` by P1.8/P4.9/P4.10/P6; and `INV-34` by P5.11/P9.5A.

## 12. Dependency graph

```text
DG0  current-head baseline, stable keys, SLO/evidence/crash policy, durable
   CLI/control ownership, owner handoff, incremental N/N-1 compatibility, and
   effect inventory
├─ DG1  fail-safe safety fixes (`Unknown`, witnesses, incarnation)
├─ DG2  canonical typed state + session-owned decisions/writes
├─ DG3  complete effect-boundary differential oracle
├─ DG4  conditional-write graduation + immutable projection/local hint/bounded relist (G4A)
│   └─ DG4B optional no-gap store feed/runtime watch certification (G4B)
└─ DG5  runtime observation/incarnation conformance + watch/bounded observer

DG2 + DG3 ──────────── pure per-session plan in shadow
DG4 + DG5 ──────────── revisioned cache appliers and indexes
cache + plan + metrics ─ typed keyed queue + per-session executor (G5A/G5B)

G4A + nudge-family effect oracle + queue/executor subset + nudge commands
    └─ nudge vertical cutover (does not wait for unrelated lifecycle deciders)
G4B ─ feed-certified external-write latency and later full-discovery removal
executor + session plan ─── lifecycle arm-by-arm cutover
indexes + pool projection ─ pool-demand cutover
queue infrastructure ────── control/order/maintenance split
all session owners + global admission ─ provider shutdown/teardown owner

all cutovers ─ anti-entropy-only patrol ─ legacy deletion

conditional writes + provider fences + production scale evidence
    └─ optional shard leases, warm followers, and multi-controller scale
```

Graph nodes DG1, DG2/DG3, DG4, and DG5 may progress in parallel. No provider-effect cutover
occurs until its dependencies reach the named checkpoints below.

## 13. Phase 0 — Ratify the contract and establish the baseline

This phase changes no reconcile ownership. Its output is the evidence and
rollout substrate required to tell whether later work is safer and faster.

### P0.1 Rebase the plan inventory onto the execution head

**Goal:** Replace historical counts and line references with a mechanically
generated inventory from the exact `origin/main` commit implementation will use.

**Changes:**

- Re-run the effect/read/write inventory over current `cmd/gc`,
  `internal/session`, `internal/worker`, and `internal/runtime`.
- Classify every lifecycle store write, provider mutation, destructive process
  action, event emission, and wake source by owner, action family, and executing
  process (`controller`, API-in-controller, foreground CLI, sidecar/poller, or
  provider child). Process-local locks are recorded explicitly as non-exclusion
  across those writers.
- Record existing rollout gates, async continuations, token fences, and direct
  `session.Manager`/provider bypasses.
- Reconcile this plan with any `internal/session/REQUIREMENTS.md` rows and
  accepted design changes added since 2026-07-12.
- Establish one canonical effect-site/owner/temporary-exception registry and
  analyzer. P2.10, P3.3–P3.4, P7.14, and P12.1 extend or consume this registry;
  they do not create competing scanners.

**Acceptance criteria:**

- Every current production effect site is classified; there are no “unknown”
  sites on the execution head. The inventory is type-aware so unrelated `Stop`
  or `Nudge` method names do not become false effects.
- Counts are generated by a Go/AST test or query, not hand-maintained prose.
- The inventory identifies the exact legacy owner that must be disabled for
  each future cutover.
- Before each later slice, its family inventory is regenerated and extended;
  an independent fail-safe P1 fix need not wait for speculative classification
  of code that does not exist yet.

**Verification:** focused inventory test; `go test ./internal/session ./cmd/gc`;
review against `git diff <inventory-base>...HEAD`.

**Dependencies:** None.

**Likely files:** `cmd/gc/reconciler_effect_inventory_test.go`,
`internal/session/REQUIREMENTS.md`, this plan.

**Estimated scope:** M.

**Rollback:** Delete the generated inventory test; no runtime change.

### P0.2 Ratify semantic decisions and non-guarantees

**Goal:** Resolve the older proposal's open forks before code encodes them.

**Recommended decisions:**

- Current `internal/session` lifecycle vocabulary is canonical; do not add the
  proposal's parallel 13-state enum.
- API/CLI session creation synchronously commits durable intent before provider
  execution. `--no-attach` may return accepted after that milestone; ordinary
  create preserves its ready-then-attach milestone from `RC-CLI-006`.
- `Unknown` never quarantines and never authorizes destruction.
- Corroborated death uses an immediate targeted confirmation after the first
  authoritative-list absence; it does not wait for a second 30-second patrol.
- Quarantine remains durable, config-driven, generation-scoped, TTL-bound, and
  operator-resettable; transient store/provider errors never accrue it.
- Single-controller-per-city remains mandatory until the HA phase's leases and
  provider fencing are enabled.
- Current trace taxonomy remains supported through migration and at least one
  release after final cutover.
- “Exactly once” is unavailable for provider effects without provider-side
  command deduplication.
- Local single-tenant operation explicitly treats raw command-store write
  access as full session-control authority. Hosted/multi-tenant operation
  requires trusted requester provenance, claim-time authorization, and
  credential/namespace separation (`RC-AUTH-001..003`).
- Every tunable has a conservative small-city default. A zero-config install
  must pass the certified entropy and latency/resource profile; no user must
  understand scheduler internals to run one city safely.
- The active `internal/session/PLAN.md` extraction order and accepted
  `engdocs/design/session-store-fences.md` remain controlling for current code
  until this plan is approved and the conditional-write capability actually
  lands. Ratification maps every active step/bead into this plan, preserves its
  characterization-first discipline, and explicitly amends the “no reconciler
  rewrite/no CAS” non-goals rather than silently contradicting them.

**Acceptance criteria:**

- Each decision is reflected in a `SESSION-*` scenario or accepted contributor
  design record with cited evidence.
- Every active session-plan step and accepted store-fence claim is marked
  preserved, completed, superseded, or blocked with its replacement task.
- No unresolved decision can change the action schema, key identity, safety
  direction, or storage contract in Phases 1–7.

**Verification:** document link check plus two-maintainer architecture review.

**Dependencies:** P0.1.

**Likely files:** `internal/session/REQUIREMENTS.md`, an approved successor
ratification/ownership ADR, and a disposition table in `internal/session/PLAN.md`.
Historical Windshield evidence remains unchanged; accepted store-fence docs are
marked superseded only through the successor record when the new capability
lands.

**Estimated scope:** S.

**Rollback:** Revert documentation before implementation begins.

### P0.3 Define the typed action-latency timeline

**Goal:** Give legacy and new paths one correlated T0–T11 vocabulary without
changing scheduling.

**Changes:**

- Add closed action/stage enums and a correlation struct carrying action class,
  stable key, generation, provider, source cursor where available, and stage
  timestamps.
- Keep high-cardinality identity in traces only; metric adapters emit bounded
  labels.
- Define blocked time separately from schedule-to-start time.

**Acceptance criteria:**

- Every stage can be absent explicitly; zero timestamps are never silently
  interpreted as success.
- Stage order validation rejects impossible timelines and accepts retries with
  distinct attempts.
- The type is wire-neutral unless an API change is separately approved.

**Verification:** table/fuzz tests for valid and invalid timelines.

**Dependencies:** P0.2.

**Likely files:** `internal/operation/timeline.go` and focused tests;
`cmd/gc/telemetry_lifecycle_metrics.go` is only the metrics adapter.

**Estimated scope:** M.

**Rollback:** Remove unused types; no behavior change.

### P0.4 Instrument the current action path

**Goal:** Measure current commit/detection/planning/provider latency before the
new scheduler can improve it.

**Changes:** Instrument explicit start, stop/drain, nudge, pool-demand wake, and
one control-dispatch action through the current trace and metrics boundary.

**Acceptance criteria:**

- A test can correlate durable intent with provider entry and observed outcome.
- Unknown or unavailable commit time is reported as unknown, not inferred from
  trace emission time.
- Instrumentation respects existing trace non-interference budgets.

**Verification:** trace schema tests, lifecycle metric tests, one isolated
process test, and comparison against the live trace CLI.

**Dependencies:** P0.3.

**Likely files:** `cmd/gc/session_reconciler_trace_types.go`,
`cmd/gc/session_lifecycle_parallel.go`, `cmd/gc/cmd_nudge.go`, associated tests.

**Estimated scope:** M; split by action family if more than five files.

**Rollback:** Remove instrumentation; runtime semantics unchanged.

### P0.5 Add one expiring reconciler migration gate

**Goal:** Provide `legacy`, `shadow`, and `keyed` ownership without permanent
capability configuration.

**Changes:** Replay the reviewed rollout work ending at `1f8596d1c` only in
small, revalidated slices: (a) docs/types/registry, (b) config resolution, (c)
composition binding, (d) status/doctor visibility, and (e) lifecycle
freeze/expiry/deletion enforcement. Regenerate moved OpenAPI/dashboard artifacts
from the execution head. Add per-action-family enablement as internal graduation
state, not user role/capability flags. The resolved owner mode is immutable for
one controller-process lifetime; changing it uses P0.11's cold handoff. Never
have more than two overlapping action-family migrations; delete a graduated
sub-gate before introducing the next.

The resolved config/environment mode is a requested mode for the next cold
owner transition, never authority to execute. Startup still must satisfy
P0.11/P0.11A; an environment break-glass may force observe-only freeze but
cannot directly enable a writer whose live ownership/topology proof is absent.
Gate expiry is doctor warning plus merge-CI enforcement only; it never changes
runtime behavior during an incident.

**Acceptance criteria:**

- Default remains legacy.
- Invalid config fails loudly; invalid environment override warns and preserves
  config according to rollout conventions.
- Every store/controller open receives the same resolved mode.
- Tests scrub the environment override.
- Config/env disagreement with live owner state remains observe-only or retains
  the proven owner; it never selects a second writer. Expiry never flips a live
  process.

**Verification:** `internal/rollout` registry, resolution, boundary, graduation,
doctor, and config composition tests.

**Dependencies:** P0.2.

**Likely files:** `internal/rollout/registry.go`,
`internal/rollout/resolve.go`, `internal/config/config.go`, focused tests.

**Estimated scope:** Five S/M merge units; never one rollout-framework PR.

**Rollback:** Set legacy via config/environment; revert the additive gate.

### P0.6 Freeze a parity and incident corpus

**Goal:** Make historical failures permanent regression inputs before changing
owners.

**Changes:**

- Select representative complete production cycles, including slow, partial,
  crash, drain, pool, unknown-state, and nudge cases.
- Scrub customer/session identifiers while preserving causal structure.
- Add one named table row per relevant historical incident from the proposal.
- Record exact code/config/store/provider provenance for every fixture.
- For parity-capable fixtures, capture the scrubbed durable snapshot, immutable
  config generation, runtime tri-state/incarnation, source watermarks, expected
  normalized plan/effects, and causal outcome. Trace-only incidents without
  complete decision inputs remain diagnostic evidence until recreated as an
  explicit scenario.
- Every diagnostic-only incident names an owner, missing evidence, reconstruction
  recipe, deadline, and the action-family gate it blocks. No historical
  destructive/lost-command incident—including the gc-hz0nu orphan-close class—
  may satisfy G3/G7/G12 as diagnostic-only; it becomes executable before that
  family's canary or the family remains legacy/weaker-profile.
- Classify every named incident as prevented, detected faster, or out of scope,
  with owner sign-off. Out-of-scope worker/workflow stalls retain an independent
  work-progress SLI (oldest hooked bead without agent output, per rig) so a
  healthy reconciler cannot mask a stopped software factory.

**Acceptance criteria:**

- Fixtures contain inputs and outcomes, not only trace prose.
- Corrupt/incomplete cycles are labeled and cannot count as parity success.
- A fixture missing any decision input cannot count as effect parity success.
- Sensitive identifiers cannot be recovered from committed fixtures.
- The final incident-coverage report lists executable versus diagnostic cases;
  a zero-executable corpus or an overdue blocking diagnostic fails its gate.

**Verification:** fixture schema, redaction, replay-load, and provenance tests.

**Dependencies:** P0.3.

**Likely files:** `cmd/gc/testdata/reconciler-corpus/`,
`cmd/gc/reconciler_corpus_test.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M.

**Rollback:** Remove fixtures; no production effect.

### P0.7 Establish repeatable performance and scale baselines

**Goal:** Prevent correctness work from hiding latency or allocation regressions.

**Changes:** Add deterministic operation-count and latency benchmarks for the
current legacy paths: one changed session among N, one work change affecting
one pool among N, current nudge target lookup, and the current cache/watchdog.
Include injected store latencies matching the observed 1–2 second Dolt
advisories. Queue coalescing, timer heaps, cursor-gap relist, anti-entropy, and
non-cooperative-provider profiles land with the phases that introduce those
mechanisms; Phase 0 records their common report schema only.

**Acceptance criteria:**

- Benchmarks separate durable commit latency from post-commit scheduling.
- Baselines cover 100, 1,000, 10,000, and 100,000 keys; release-candidate
  profiles add 1,000,000 where memory allows, without wall-clock sleeps in PR
  correctness tests.
- Results include operations, allocations/key, resident memory, goroutines,
  file descriptors, CPU, throughput, queue age, and stage latency, and prove
  which current operations are O(N).
- First-ready time survives requeue/coalescing. Sample eligibility and every
  exclusion reason follow the mechanical §10 definition.
- Pin per-source scoped-fanout and enqueue-all fallback budgets, including
  exact-key operation-count profiles for the top three recurring config edits.
  `RC-PERF-001..002` defines honest timing, the abort, zero-config, and
  knob-count evidence.

**Verification:** focused `go test -bench` runs under an isolated cache; store
results as CI/nightly artifacts rather than flaky PR thresholds initially.

**Dependencies:** P0.3.

**Likely files:** `cmd/gc/reconciler_bench_test.go`,
`internal/beads/caching_store_bench_test.go`.

**Estimated scope:** M.

**Rollback:** Remove benchmarks.

### P0.8 Freeze stable key and scoped-fanout contracts

**Goal:** Define identity before post-commit, config, feed, and runtime paths
need to name affected work, without building the scheduler early.

**Changes:** Define typed stable keys for the controllers in §6 plus a
`ScopedFanoutKey` that represents a large/broad affected scope. Keys use durable
IDs and immutable scope/store identity, never aliases, runtime names, or current
worker ownership. Define versioned string/golden encoding and the mapper result
contract (`exact keys`, `paged scope fanout`, or `unknown -> safe scope`).

**Acceptance criteria:** key identity is deterministic across processes and
restarts; renames/replacements do not collide; fanout work is reconstructable
and cannot be lost if a queue disappears; no queue/provider dependency enters
the key package. It defines stable identity for future dedicated coordination
records but does not allocate/write them; P2.0 and each claim/lease/reservation
consumer implement `RC-STATE-003`.

**Verification:** golden round-trip/collision tests, rename/replacement/split-
store tables, and compatibility vectors.

**Dependencies:** P0.1, P0.2.

**Likely files:** `internal/reconcilekey/key.go` and focused tests; domain
mappers remain with their owning controller/applier.

**Estimated scope:** S.

**Rollback:** Additive types remain unused.

### P0.9 Codify verification tiers and evidence retention

**Goal:** Turn §26.1 from prose into one versioned release policy before
implementation starts producing incomparable test claims.

**Changes:** For every VT0–VT6 verification tier, record commands, deterministic state/time
budget, pass threshold, artifact location, scrub rules, and retention. Add a
typed Go evidence registry with one record per applicable `RC-*` × action family
× store/runtime/OS/capability profile: exact package/top-level test/build tag/
CI job, fault boundary, durable/effect/runtime/event/CLI oracle, mandatory/skip
policy, owning gate, and artifact. Define the bounded-model report schema:
dimensions, bounds, explored states/transitions, path depth, reductions, and
invariants. Add a targeted `-race` job for queue/executor/cache/owner mechanics.
The registry also maps every stable `INV-*` invariant to at least one named,
deterministic VT1–VT3 merge-gating test/package plus any real-process, nightly, or
release-candidate variants. Generated seam fault tests run deterministically at
merge tier; nightly keeps real SIGKILL/Dolt/tmux and broad seeded histories
rather than being the only proof of an invariant. Each entry declares its
mandatory real proof seam; every fake-injected seam fault is pinned by a
corresponding real provider/store conformance case. The G0 manifest, thresholds,
requirements rows, and effect exceptions are protected under
`RC-GATE-001..002`, and each bead cites the ratified content hash.

**Acceptance criteria:** every later checkpoint can link machine-readable
artifacts for the exact commit; referenced tests exist, ran in the declared job,
did not skip when mandatory, and emitted their artifact; a failed random seed
cannot be discarded; a green job with missing required artifacts fails the
gate. Missing capability fails a required certification profile rather than
turning into a green skip. Every `INV-*` has deterministic merge evidence. A red
VT4/nightly freezes owner flips and any PR touching the implicated packages until
the seed is retained as a deterministic fixture and fixed; a red VT5 blocks the
release candidate. CI enforces the freeze state rather than relying on a human
remembering a dashboard.

**Verification:** policy schema/self-tests and one deliberately incomplete run.

**Dependencies:** P0.1, P0.7.

**Likely files:** typed test registry/helper under `internal/reconciletest` or
`test/`, release policy under `engdocs/contributors/`, CI helper schema/tests.

**Estimated scope:** M.

**Rollback:** Policy-only; no production behavior.

### P0.10 Create the crash-point registry and reconstruction harness

**Goal:** Give every durable/effect boundary a stable ID so new paths cannot
silently omit fault coverage.

**Changes:** First confine provider, process, and authoritative-store mutations
to enforceable wrappers. Static analysis derives each owner/action family's
reachable seam-call sequence and the harness automatically creates before/after
injection points for every call. Human `B*`/semantic labels map onto that
derived set; developer registration is not the source of completeness. Cover
durable intent/local apply, provider adapter/native entry/return,
outcome/status commit, and dedup marker. Land the registry plus one convergence
re-exec consumer first. Later families extend their actual seams when
introduced. Queue/cache/timer internals use deterministic
model laws and empty-memory reconstruction rather than receiving a parallel
semantic crash ledger. For an operation spanning stores, the derived sequence
includes every ordering between store-A/store-B commits and their independently
applied watermarks; one generic “store write” point cannot hide partial success.
The harness can panic, return an injected
error, `os.Exit`, or signal a re-exec process at each supported point, then
reconstruct with empty ephemeral state. The child reports boundary arrival over
a barrier; the parent injects return/error/panic/EOF/SIGINT/SIGKILL without
sleeps. An independent durable test recorder persists provider-adapter entry,
native-mutation entry, waiter detachment, actual return/panic, fencing, and
resolution with request/action/box/launch IDs so the oracle survives child
death. It is test evidence, never the production ownership record required by
§7.3. Add a timer-capable manual scheduler (`After`/timer/ticker plus
advance-and-drain), not only an injected `Now()` clock.

**Acceptance criteria:** each enforceable effect/status/feed seam exposes its
derived before/after points; a direct call outside a named temporary exception
fails a generated completeness/static test; the exercised injection-point set
must equal the current-head derived set for every registered owner path, so a
new interior write creates a red missing point automatically; every cut
asserts store snapshot, effect counts/identity,
actual permit, durable outcome, events, and eventual convergence after an empty-
memory restart; process tests use isolated cities/stores/tmux sockets; the
harness never touches the personal tmux server. No signal-timing/log-polling
test counts as deterministic boundary proof.
The generated registry and reconstruction oracle satisfy
`RC-CRASH-001..002`; later owners extend, never fork, it.

**Verification:** seeded known-bad mini state machine and one real re-exec smoke
case.

**Dependencies:** P0.1, P0.9.

**Likely files:** test-only `internal/reconciletest/crashpoints/`, registry
completeness tests.

**Estimated scope:** M.

**Rollback:** Test-only.

### P0.11 Define cold exclusive owner handoff and automatic abort

**Goal:** Make single-controller migration/rollback crash-safe without building
the distributed lease/epoch protocol reserved for Phase 11.

**Protocol:** resolve one action-family mode immutably at controller-process
startup. To change it, persist/validate the new config, stop old admission,
drain or fence actual in-flight calls, stop the old controller process, prove its
live kernel ownership is released, then start one process in the new mode.
Durable work tolerates the brief gap. If exclusivity is uncertain, start only
observation and refuse effects. An automatic abort freezes/drains the affected
mode and requests a cold restart into the last-known-safe configured mode;
telemetry alone is never ownership truth. Dynamic leases/epochs enter only with
P11.

For today's 142–256 second monolithic legacy pass/build, handoff either waits
for that pass to finish before starting keyed effects or uses the P3 interposed
effect boundary to recheck the cold owner's immutable instance/mode token and refuse every
later native entry from the old pass. Config/environment change alone never
cuts through an in-flight pass. Automatic abort uses the symmetric rule.

**Acceptance criteria:** fault config commit, stop-admission, drain/fence,
process exit, new startup, and abort. At most one live process can enter the
family; no PID/status file stores ownership; a leftover lock path has no meaning
without a held live kernel lock; shadow makes no effects or shared acting-path
queue/timer changes, while an isolated bounded no-effect shadow scheduler is
allowed and stays within its measured overhead budget. An ambiguous old call blocks new
effect startup until drained/fenced.

**Verification:** deterministic cold-handoff/restart table, re-exec crash
harness, ambiguous provider call, durable nudge/request reconstruction, and
unavailable telemetry; flip and abort at every phase of a deliberately blocked
legacy pass. `RC-MIG-001` governs.

**Dependencies:** P0.5, P0.10.

**Likely files:** rollout mode/startup tests plus one controller composition
adapter; no generic owner-state machine.

**Estimated scope:** M, then one S/M family adapter at each cutover.

**Rollback:** Stop/drain the keyed process and cold-start legacy. Never activate
legacy while a keyed actual call may still mutate.

### P0.11A Enforce the pre-HA single-owner topology

**Goal:** Make “a second controller refuses effects” an owned mechanism rather
than an assumption about one flock path/port.

**Changes:** Inventory every current controller/supervisor/test/API start path
and its exclusion scope (host, mount namespace, `GC_HOME`, runtime directory,
working set). Same-host execution requires one live kernel lock at the canonical
city identity. Bind the authoritative working set to a stable city installation
ID and, where conditional store writes exist, a static exclusive owner record.
This record has no timeout/heartbeat/automatic takeover: same installation may
recover after crash through P0.11/P1.10 ambiguity checks; transfer to another
installation is an explicit cold operation after old effects/processes are
proven absent. Dynamic leases/expiry remain Phase 11.

**Acceptance criteria:** two directories/runtime roots pointing to one store,
different hosts/mount namespaces, duplicate supervisors, stale lock path, PID
reuse, and the historical long supervisor-SIGKILL overlap all admit at most one
effect owner. If the topology/store cannot prove this binding, the
Compatibility single-owner profile is restricted to the named host/runtime
root and refuses a shared-working-set startup rather than guessing. No PID/
status file confers ownership.

**Verification:** multi-process/two-directory/one-store barriers, kernel-lock
release, static-owner conditional conflict, crash/restart by same installation,
manual cold transfer, N/N-1 owner matrix, and inaccessible store fail-closed.

**Dependencies:** P0.1, P0.11, P2.0 for store-anchored conditional ownership;
the same-host kernel guard can land earlier. Required before any keyed provider
effect canary.

**Likely files:** controller/supervisor ownership bootstrap, a small store-
anchored installation binding behind `internal/beads`, doctor/status tests.

**Estimated scope:** M same-host guard plus M store-topology binding.

**Rollback:** Cold handoff to legacy on the same certified installation; never
erase/replace the binding as an automatic crash reaction.

### P0.12 Establish continuous N/N-1 compatibility

**Goal:** Prove rollback before any new production write protocol or feed schema
appears.

**Matrix:** resolve the current N-1 release through a checked-in immutable
manifest containing exact release tag and commit, OS/architecture, Go/toolchain
and build tags, artifact URL or hermetic source-build recipe, SHA-256 digest,
and provenance/signature policy. Never resolve mutable “latest.” Establish the
basic resolver/status/owner-handoff harness on isolated copies of the same
authoritative store. Before each new status body/marker, command, config,
conditional-write, feed schema, external `bd` binary, or store-schema version
lands, add that exact N/N-1 reader/writer case and rollback drill in both
directions, including a pending operation. Pin `deps.env`/provider artifacts and
exercise bd-sqlite, BdStore/bd-Dolt, NativeDolt, and DoltLite as separate store
axes rather than treating the `gc` binary matrix as store compatibility. Crash the
handoff cases applicable to the schema being introduced; do not pretend Phase 0
can test every future record.

**Acceptance criteria:** the resolver verifies digest and provenance before
execution and fails the gate—never skips—when the pinned artifact/source is
unavailable or mismatched. Old code safely ignores/preserves/decodes new data,
or an explicit idempotent downgrade migration and rollback barrier is approved;
raw unknown fields/values are byte-compared before and after the old writer;
N-1 never treats a torn compatibility status as authoritative; N recovers the
preserved data without duplicate effects; caches/queues rebuild after both
directions of handoff. A successful response from a mismatched writer/schema is
still rejected by the `RC-STORE-003` version fence; error-envelope parsing alone
is not certification.

**Verification:** hermetic binary/process matrix in CI with the pinned manifest,
isolated store copies, both reader/writer directions, owner handoff, pending
operation, raw-data diff, and one automated rollback drill.

**Dependencies:** P0.2, P0.9–P0.11.

**Likely files:** integration harness, release-binary resolver, test fixtures,
and compatibility policy.

**Estimated scope:** Epic; decompose into M slices.

**Rollback:** Harness-only; a failed case blocks the new writer/schema.

### P0.13 Freeze mutating CLI/API semantics and control ownership

**Goal:** Remove socket timing and direct fallback from the definition of who
may mutate a managed runtime while preserving deliberate user-visible command
milestones.

**Changes:** Inventory `new`, `attach`/resume, `submit`, `nudge`, `suspend`,
`wake`, `reset`, `kill`, `close`, rig restart, and city stop. For each, record
the durable acceptance milestone, optional completion milestone, exit code,
human/JSON output, cancellation behavior, stable target binding, and managed/
unmanaged owner. Define one request-ID protocol that distinguishes controller
unavailable from request acknowledgement unknown. Non-mutating terminal attach
and streaming may remain in the CLI after exact binding; runtime mutation may
not.

**Acceptance criteria:** `RC-CLI-001..010` and `RC-EVENT-001..004` are mapped
to existing characterization tests or new red fixtures. In particular, ack loss
never enables direct cleanup; commit ambiguity retains one request ID; SIGINT
before/after commit has distinct outcomes; JSON remains one additive v1 record;
and self-close can report before its launch is stopped.

The matrix distinguishes an independent external waiter from a requester inside
the exact launch being terminated. The former may report terminal completion
only from post-exit live proof; the latter uses the `RC-CLI-006`, `RC-CLI-010`
requester
fence, returns exit 0 accepted-incomplete after one bounded result-write attempt,
and returns exit 1 `self_wait_unavailable` if it requested an impossible
terminal wait. City-stop terminal proof is assembled by the external
CLI/supervisor, never asserted by the dying controller.

**Verification:** unit decision tables, current JSON golden tests, and a re-exec
CLI harness for commit-response loss, socket response loss, controller SIGKILL,
SIGINT, broken stdout, and empty-memory restart.

**Dependencies:** P0.1–P0.3. The characterization work may run beside P1
fail-safe fixes; the command front door implementation is P2.11.

**Likely files:** `cmd/gc/reconciler_cli_contract_test.go`, existing command
JSON tests, `internal/session/REQUIREMENTS.md`, acceptance matrix.

**Estimated scope:** Epic; one command family per S/M child, no runtime behavior
change in this task.

**Rollback:** Test/docs only.

### P0.14 Freeze the command trust and authorization boundary

**Goal:** Prevent the durable command ledger from turning shared Beads write
access into an undocumented fleet-wide kill/nudge credential.

**Changes:** Inventory every principal/credential that can create or mutate
lifecycle, nudge, control, claim, and result records. Define the trusted ingress
stamp and claim-time authorization input/output under `RC-AUTH-001..003`:
authenticated principal, tenant/city scope, credential class, policy
version/decision ID, action/target/payload scope, issuance/expiry, and denial
result. Requester fields inside an untrusted command body are never evidence.
Define the explicit local `store_writer_is_controller` profile and the hosted
credential/namespace split; keep policy configurable and free of hardcoded role
names. Hosted self-termination uses a trusted local peer-verified ingress plus a
controller-minted token bound to the exact city/session/launch and permitted
self-action set; no controller-wide store credential is exposed inside agents.

**Acceptance criteria:** a complete threat table covers direct `bd`, copied or
self-asserted stamps, cross-city replay, policy revocation between commit/claim,
unknown principal, payload escalation, compromised work credential, N/N-1
principal schema, authorization-provider outage, self-token replay after launch
replacement, peer mismatch, and local socket substitution. Hosted/high-risk profile
refuses startup/effects until work credentials cannot write protected command
types and the controller can stamp/revalidate trusted provenance. Local
single-tenant status warns that every raw store writer has full control.

**Verification:** red authorization decision tables, store/provider capability
tests, credential-negative integration, and static protected-record writer
inventory.

**Dependencies:** P0.1, P0.2, P0.12.

**Likely files:** an approved security/trust ADR, acceptance/requirements rows,
store capability fixtures, and later P2.11/P6.1 implementation children.

**Estimated scope:** M contract/threat slice plus M children per store/front
door; no provider effect is enabled here.

**Rollback:** Refuse hosted profile; local trust mode remains explicit.

### P0.15 Freeze store identity, restore, and external-schema recovery

**Goal:** Make authoritative restore/version skew a first-class fence rather
than treating a rewound store as current truth.

**Changes:** For bd-sqlite, BdStore/bd-Dolt, NativeDolt, and DoltLite, define
how `store_uuid`, `restore_epoch`, `store_schema_version`, writer build, source
revision/cursor, and an independently retained high-water anchor are obtained
and compared. Specify the sanctioned restore operation and its crash-resumable
admission freeze, snapshot install, old-epoch command/intent quarantine,
generation/epoch re-anchor, and explicit reissue steps (`RC-STORE-001..003`).
State the impossibility boundary: an identical-backup restore without an
independent anchor is not detectable and cannot be certified.

**Acceptance criteria:** wrong store, `dolt.port` fallback, sqlite backup
restore, Dolt reset/file restore, schema auto-migration/downgrade, well-formed-
wrong rows, lost recovery acknowledgement, and crash at every recovery step
all fail closed. No old pending nudge or effectful level intent executes
after restore without deliberate reissue above the retained high-water mark.

**Verification:** real-store capability matrix, version fence below decoding,
restore/re-anchor re-exec matrix, and N/N-1 `bd` binary/schema tests in both
directions.

**Dependencies:** P0.1, P0.10, P0.12; store-specific implementation lands in
P2.0/P4/P11 and the operator drill in P12.6–P12.7.

**Likely files:** store capability contracts/conformance, dependency manifest,
recovery fixtures/runbook source.

**Estimated scope:** Epic; one M profile/restore slice per store class.

**Rollback:** Effects remain frozen/observation-only; never waive identity or
schema checks to recover availability.

### P0.16 Protect the ratified contract and derive gate evidence

**Goal:** Prevent an implementation agent from weakening its own acceptance
row or manufacturing the packet that approves its cutover.

**Changes:** Generate a G0 base manifest over this plan, the acceptance matrix,
permanent session requirements, numeric thresholds, required-test registry, and
effect exceptions. Approved contract changes append a signed delta with stable
per-row hashes. Require every bead/PR/evidence packet to cite the base, accepted
delta head, and exact rows/dependencies it consumes; unrelated additive deltas
do not invalidate it, while a changed consumed row does.
Add path/semantic guards: additions are allowed, but modifying/deleting a cited
row requires a separate contract-delta approval by the human owner or an
independent approver. Generate content-addressed checkpoint packets from actual
CI artifacts and attest them with the CI identity (`RC-GATE-001..002`).

**Acceptance criteria:** stale plan hash, changed threshold, removed test,
green skip, implementer-authored packet, same-principal self-approval, mutable
dependency tag, missing artifact, or invalid signature fails the gate. The
trust boundary explicitly excludes repository-admin/CI-key compromise rather
than claiming cryptography solves it.

**Verification:** positive/negative manifest/ownership fixtures and a seeded
self-weakening PR simulation.

**Dependencies:** P0.9, P0.12.

**Likely files:** typed evidence registry/manifest tool, CI path guard,
CODEOWNERS/branch-policy documentation, signed artifact verifier.

**Estimated scope:** M in small tool/CI slices.

**Rollback:** Block owner flips; protected contract remains readable.

### P0.17 Quantify migration exposure and out-of-scope progress risk

**Goal:** Make the long mixed-owner program and its non-reconciler failure
classes conscious operating decisions rather than optimism hidden in phases.

**Changes:** Publish an owner-signed incident-class × retirement phase ×
projected date ledger, expected incident exposure over the migration window,
and stop-with-value dates. Extend P0.6's incident coverage classification. Add
an independent per-rig work-progress SLI (oldest hooked bead without agent
output) so a fast/healthy controller cannot mask wedged workers, provider-wide
LLM stalls, or retry-root treadmills.

**Acceptance criteria:** G0 names the residual incident rate, staffing/release
assumptions, and risk accepted through each checkpoint. Out-of-scope incidents
have an owner and independent detection path. Forecast drift is refreshed at
every checkpoint and can stop expansion.

**Verification:** ledger completeness against the incident corpus, SLI fixture,
and owner sign-off.

**Dependencies:** P0.6–P0.7.

**Likely files:** evidence packet schema, incident ledger, metric contract/tests.

**Estimated scope:** S governance plus a separately decomposed S/M SLI slice.

**Rollback:** Observability-only; no effect ownership.

### Checkpoint G0 — Baseline approved

- Current-head effect inventory is complete.
- Semantic decisions are approved.
- Named `RC-*` contracts and the mutating CLI ownership/milestone matrix are
  approved.
- Legacy latency is measurable end to end.
- Corpus and scale baseline reproduce.
- Stable keys, verification/evidence policy, initial crash registry, owner-
  handoff protocol, and incremental N/N-1 harness are approved and runnable.
- Command trust/authorization, store identity/restore/schema recovery, and the
  protected-contract/evidence-provenance manifest are approved with executable
  negative fixtures (`P0.14..P0.16`).
- The migration exposure/incident-coverage ledger and independent work-progress
  SLI have explicit owner sign-off (`P0.17`).
- A delivery forecast records child-bead count/range per phase, staffing and
  parallelism assumptions, cross-repository dependencies, release cadence,
  mandatory soak-window arithmetic, and projected checkpoint dates. It is
  refreshed at every checkpoint; “Epic” without decomposed cost cannot enter
  implementation.
- Default production behavior is unchanged.

No Phase 4 or later provider-effect work begins before G0.

## 14. Phase 1 — Make destructive behavior fail-safe

These changes ship independently and reduce risk even if the redesign stops.

### P1.0A Share one coherent Ready snapshot per store/cycle

**Goal:** Remove the measured repeated full-store Ready queries immediately,
without changing reconcile ownership or demand semantics.

**Changes:** Recreate the reviewed archaeology series ending at `93fbf7519`
(which includes `e5149c5ab` and `9d3d7b538`) red-first on the execution head.
Read Ready once per distinct store for the pre-canonicalization demand view and,
only when canonicalization can mutate assignment/readiness inputs, once for the
post-canonicalization view. Share each immutable snapshot only among consumers
of that exact store and phase; never reuse across stores or across the mutation
boundary. Give each pass-local store handle an internal comparable identity;
never use an unconstrained interface value as a map key or assume a provider's
dynamic type is comparable. A canonicalization **attempt** invalidates that
store even when `Update` returns an error, because commit acknowledgement may
have been lost. Preserve error identity and fail-safe demand behavior through
one shared cached/live merge helper. Post-canonicalization consumers—including
control-dispatch demand—must consume the post-boundary snapshot rather than a
stale local slice collected before route repair.

**Acceptance criteria:** output matches the characterized legacy path for
multi-rig, multi-store, error, duplicate/canonical bound, and scale-from-zero
cases except one named fail-safe divergence: an unlimited shared read may expose
corruption beyond the old limited query and set `StoreQueryPartial=true`, which
conservatively suppresses drain rather than hiding uncertainty. That row updates
the permanent behavior evidence in the same slice. Ready calls are O(distinct stores × required phase), not O(agents or
pools). A full pass performs no more than two live Ready reads per distinct
store—one pre-write assignment snapshot and one post-write demand snapshot—plus
at most one post-write cached-tier read; phases that cannot mutate reuse one.
MemStore, FileStore, BdStore (including its BeadsLibStore wrapper),
CachingStore, NativeDoltStore, the exec Store, and the existing DoltLite read
path all prove filter/order/limit/error equivalence. A factory-selection
regression must force the external hosted-gateway/preflight-eligible path to
select NativeDoltStore, feed that selected store through a complete desired-
state pass, and prove the same coherence and read budget; testing a
CachingStore-wrapped substitute does not satisfy this row. The exec Store uses
a hermetic protocol fixture and proves both success and error preservation.
No production Store implementation selected by `OpenStoreAtForCity` on the
execution head may be omitted without an explicit reviewed exclusion in this
task. An error cannot become an empty-success snapshot; no caller mutates a
shared result. The post-canonicalization test would fail if the earlier snapshot
or pre-repair unassigned-route slice were reused. Filtered allocation is capped
by the query limit. An uncomparable custom store cannot panic or silently bypass
the read budget. Repeated consumers report one store/phase read error without
losing `errors.Is`/`errors.As` identity (`RC-PERF-001..002`).

One timing-semantic change is explicit: an external write that races after a
phase snapshot is published is observed on the next pass, not opportunistically
by a later consumer in the same phase. Controller-owned canonicalization is the
only same-pass invalidation source. Tests freeze this coherent-snapshot rule so
future callers cannot accidentally reintroduce mid-phase mixed generations.

**Verification:** red call-count/coherence/error tests, including
`scale_from_zero_test.go`; focused `cmd/gc` process/unit shard and before/after
operation-count benchmark.

**Dependencies:** Current-head characterization only. It does not wait for G0
and introduces no new owner/schema.

**Likely files:** `cmd/gc/build_desired_state.go` and focused tests; use
`93fbf7519` as reviewed evidence, not as an unexamined cherry-pick.

**Estimated scope:** S/M.

**Rollback:** Revert the optimization; output parity remains the oracle.

### P1.0B Relieve current start head-of-line blocking

**Goal:** Bank a user-visible latency win in the existing loop while the keyed
strangler is built.

**Changes:** Instrument the current phase boundaries, then dispatch only planned
starts proven independent of the current drain/stop finalization wave through
the existing bounded target-wave executor instead of waiting behind its observed
114-second tail. Independence requires a distinct stable session/slot, satisfied
dependencies/capacity from the correct post-canonicalization snapshot, no
provider-global shutdown/swap, and no outcome from the outstanding finalization
that can change the candidate's validity. Anything uncertain preserves current
ordering. Re-measure after this slice; the residual latency sizes Phases 4–8.

**Acceptance criteria:** independent starts reach native entry while an
unrelated stop/drain is deliberately blocked; same session/pool slot,
replacement, min-floor/capacity, dependency, provider-swap, and shutdown cases
retain legacy ordering; bounded provider/session concurrency is unchanged; all
outputs/status/events match characterization except the approved earlier start
time. No detached goroutine outlives ownership.

**Verification:** red barrier test reproducing the current head-of-line delay,
cross-dependency/pool tables, existing bounded lifecycle suite, race detector,
and before/after T0–T11 trace/operation counts.

**Dependencies:** G0, P0.1/P0.4 effect and ordering baseline, and P1.0A's
coherent snapshots. It does not wait for the keyed scheduler, but it is not a
pre-G0 fail-safe exception because it deliberately introduces concurrent
start/finalization ordering.

**Likely files:** current lifecycle phase orchestration and bounded
`executeTargetWave` adapter/tests; no new generic scheduler.

**Estimated scope:** M; if independence cannot be proven locally, stop and keep
the evidence rather than widening the change.

**Rollback:** Restore serial phase order; measurements remain.

### P1.0C Do not fold a failed heal into same-pass projection

**Goal:** Fix the confirmed `healStateWithRollbackInfo` commit-before-projection
violation before the general status writer exists.

**Changes:** When `Store.ApplyPatch` fails or its commit is ambiguous, return the
original `Info`/projection plus the typed error/retry state. Only a proven commit
may produce the patched local view consumed by later decisions in that pass; a
lost response requires authoritative reread. “Authoritative” excludes the
pre-write cache image: a cache-backed store marks the row dirty before returning
any `SetMetadataBatch` error that may follow a full or partial commit and records
a per-row mutation-sequence fence before releasing the cache lock. The fence
prevents an in-flight prime/reconcile that captured the pre-write backing row
from installing that row later and clearing the dirty bit. The next projection
therefore refreshes the row from the backing store before any decision.

**Acceptance criteria:** an injected heal failure leaves every field of the
input projection unchanged and no later same-pass start/stop/close/identity
decision consumes the proposed patch. Proven success returns the committed
view; commit-unknown blocks and rereads (`RC-STATE-001`). A CachingStore backed
by a fault injector that commits all keys or an arbitrary subset and then
returns an error keeps the same-pass Info unchanged, mutation-fences and marks
the cached row dirty, and exposes the backing-store truth on the next normal
session snapshot load; the test must fail if that load is served from the stale
pre-write cache. A deterministic barrier case captures the old backing row in a
concurrent refresh, lets the batch commit or partially commit and return an
error, then resumes the stale refresh; the refresh cannot erase the fence/dirty
state or make the following snapshot return the old row.

**Verification:** red regression at the current helper and caller, failed/
ambiguous/success tables, a CachingStore commit-then-error/partial-commit
refresh regression plus the stale-refresh barrier interleaving, both caller
paths, and an end-to-end same-pass effect-count assertion.

**Dependencies:** None beyond current session characterization.

**Likely files:** `cmd/gc/session_reconcile.go`,
`cmd/gc/session_reconciler.go`, `internal/beads/caching_store_writes.go`, and
their focused tests.

**Estimated scope:** S.

**Rollback:** Do not restore speculative folding; conservative retry is the
safe compatibility behavior.

### P1.0D Fail closed after managed stop acknowledgement ambiguity

**Goal:** Eliminate the current acknowledgement-loss dual-writer window before
the durable request front door exists, without pretending the interim wire can
resolve durable acceptance.

**Changes:** Classify the existing stop wire as `acknowledged`,
`definite_pre_entry_unavailable`, or `may_have_entered`. Once a managed
controller stop request may have entered the socket, EOF, timeout, partial or
malformed response, reset, and write/read-deadline failure return a typed
nonzero ambiguity result and perform zero direct provider/store cleanup. Only
the definite-pre-entry branch may retain the characterized direct fallback,
and then only while holding the existing **same-path legacy** city controller
lock continuously across store marking, provider/orphan/server cleanup, and
required store shutdown. That lock excludes processes resolving the same lock
inode; it does not exclude a different runtime directory or mount pointed at
the same store. This limitation is named
`EXC-STOP-DIRECT-SAMEPATH-001`, expires at P2.11A, and cannot certify hosted,
cross-path, or HA ownership; P0.11A owns those topologies. The acknowledged
branch waits for controller exit through an ownership-returning primitive that
acquires and returns the still-held same-path lock; a successful probe never
closes and reacquires it before post-controller work. If a
concurrent starter wins, the CLI fails closed and performs no post-controller
store shutdown. Only when the CLI acquires the lock does it retain that lock
through every post-controller store write and required store shutdown,
including the supervisor-managed stop path. This is safe race detection, not a
gap-free ownership handoff. Every same-path foreground or supervisor-managed
start acquires that legacy lock before pack/config/formula/skill/MCP
materialization, bead lifecycle, provider health/warmup or construction,
controller-state/watchers/maintenance, `on_boot`, or tmux mutation, then
transfers the still-held lock exactly once into the controller owner; acquiring
it only inside `runController` is too late. Registration performs no such
materialization outside the locked supervisor initialization. Therefore a stop
that owns the lock excludes the starter's pre-controller side effects as well
as the controller loop. The stop request is classified before any store open or
write. Exact response equality, not substring matching,
recognizes `ok`. Every `stop` and `stop-force` call site consumes the tri-state;
`may_have_entered` returns nonzero without an alternate stop request, registry
mutation/reload, provider/store construction, or cleanup. This includes the
supervisor force-unregister and invalid-config paths; neither may collapse the
result back to a boolean. The definite-pre-entry direct owner never runs behind
a detachable outer timeout: before native entry a deadline may cancel and
release, but after entry the process and continuously held lock remain until the
provider call actually returns or is independently fenced. Only then does the
CLI project an elapsed deadline as exit 1. The direct lock-owning path selects
a no-detach mode for every nested bounded interrupt/stop/provider target wave;
an inner per-target timeout may classify the eventual result but cannot return
the wave while an entered provider goroutine continues. Controller-owned waves
remain outside this interim direct-owner change.

**Acceptance criteria:** drop the reply before/after controller acceptance,
inject short writes, partial/malformed replies, EOF, reset, and deadline errors,
and exercise stale/refused sockets with the controller lock both free and held.
Every may-have-entered case exits nonzero, constructs no provider/store writer,
and produces zero direct cleanup entries. A definite-pre-entry case enters the
legacy direct path only while its same-path lock remains held; a held/erroring
lock fails closed. A two-directory/one-store test proves the exception does not
overclaim cross-path exclusion. After acknowledged controller exit, a barrier
lets a concurrent starter win the same-path lock and proves the CLI returns
nonzero without shutting down its store; the CLI-owned branch retains the lock
through required store shutdown. The inverse barrier lets the CLI acquire first
and proves a concurrent foreground/managed start records zero store, provider,
or tmux effects before failing lock acquisition. The starter-owned branch proves
stop skips shutdown. Separate post-native-entry timeout barriers block
`Interrupt` and `Stop` past the configured outer wall-clock cap and,
respectively, `interruptPerTargetTimeout(cfg)` and
`stopPerTargetTimeoutDefault`. At every boundary the command has not returned,
the same lock remains held, a concurrent start has zero materialization/store/
provider/tmux effects, and no entered provider goroutine was abandoned. After
provider release the command returns 1 and only then releases the lock;
releasing either fake before its inner cap is insufficient coverage. A
supervisor `stop-force` acknowledgement-loss barrier proves registration is
unchanged and no reload, alternate stop, store, provider, or tmux call occurs;
the invalid-config path has the same fail-closed assertion. Foreground and
supervisor startup barriers pause before lock acquisition and prove zero materialization,
store, provider, watcher, `on_boot`, or tmux effects; registration itself is
read-only. The acknowledged supervisor-stop case proves one retained lock spans
controller exit through store shutdown without a close/reacquire gap. No
default tmux socket is touched. This slice proves the scoped same-path part of
`RC-SHUT-001`, negative parts of `RC-CLI-004..005` and `RC-SHUT-002..003`, plus
the local socket-isolation part of `RC-SHUT-005`; it explicitly does **not**
claim provider-global/cross-path admission, durable acceptance, operation
lookup, or full `RC-CLI-001..005` conformance.

**Verification:** deterministic socket, timeout, registry/reload, and re-exec
barriers plus an isolated city provider/store spy. No timing/log-polling
assertion counts.

**Dependencies:** P0.13 characterization may land beside it; the safe patch
does not wait for P2.11 and adds no request schema.

**Likely files:** current `gc start`/`gc stop` commands, controller lock/client
adapters, direct-mode lifecycle target waves, supervisor city unregister path,
and focused process tests.

**Estimated scope:** L, landed as separately green internal commits for typed
transport/call-site fail-closed behavior, continuous start/stop lock ownership,
and ownership-preserving direct waves. The direct fallback is not certified
until all three are green together.

**Rollback:** Never restore direct fallback after possible managed acceptance;
use explicit unmanaged ownership instead.

### P1.1A Make discoverable convergence writes and event ordering structural

**Goal:** Remove ignored convergence progress writes on paths that remain
discoverable today, and stop publishing terminal success before its durable
proof, without claiming crash discovery the current store query cannot provide.

**Acceptance criteria:** partial-create rollback, explicit-ID terminal recovery,
and pending-next cleanup route progress persistence through the checked commit
helper or return the cleanup error; original and rollback failures use
`errors.Join`, and a terminal-state write failure never closes an undiscoverable
partial root. A transient pending-wisp or child lookup error is returned and
cannot clear a marker, establish marker inapplicability, or masquerade as
absence; only proven not-found/stale/mismatched/closed evidence permits cleanup.
An already-active adopted successor equal to `pending_next_wisp` clears the
pending marker without repouring. No `_ = SetMetadata` remains for convergence
progress markers. Every production terminal emitter—including normal handler,
recovery, manual approve, and manual stop paths—fires `EventTerminated`,
terminal-success/stopped `EventIteration`, and manual terminal events only after
state, close, and applicable dedup marker proof. Recorder failure after proof
never rolls domain state back (`RC-EVENT-001..003`, scoped `INV-25`). A fresh
`Reconciler` given the explicit ID repairs a close-success/marker-failure row,
but this slice does not claim that an empty-memory startup can discover it. If
the initial `creating` write and the attempted `terminated` rollback both fail,
the still-open root with empty state and incomplete required formula/target,
iteration, gate, or trigger metadata is recognized on a fresh reconcile as
partial creation, terminalized, and closed without pouring a wisp. That
classifier must preserve the legitimate empty-state root whose complete
metadata and idempotent wisp evidence make it safe to adopt or resume.

**Verification:** inject failure before state, at close, at the marker, and at
pending-next cleanup. Assert zero terminal-success event before durable proof,
joined original/rollback errors for partial creation, explicit-ID repair of a
closed incomplete transition, and fresh-reconciler repair of an open durable
pending-next marker without repouring. A double-fault case fails both the
initial-state and rollback-state writes, recreates the Reconciler after faults
stop, and proves the open empty-state/incomplete-metadata root reaches terminal
proof with zero wisp pours; its complete-metadata/adoptable-wisp control case
must still resume. Normal, recovery, manual-approve, and manual-stop fault tables
inject state, child-query, close, and marker failures and assert zero terminal
events; success observers read complete proof at every emission. Pending-next
tables cover transient lookup failure, active-successor cleanup, and no repour.
An AST guard rejects ignored `SetMetadata` progress writes. Record the startup
query characterization that closed rows are excluded; it is the red input to
P1.1B, not evidence for `RC-CRASH-002`.

**Dependencies:** Current-head failing test and P0.1 inventory for the affected
sites; the remaining G0 evidence work may proceed in parallel.

**Likely files:** `internal/convergence/{create,handler,manual,reconcile,events}.go`
and focused tests; retry delegates through `CreateHandler` and is not a second
rollback owner.

**Estimated scope:** S.

**Rollback:** Revert the isolated fix.

### P1.1B Add bounded terminal-transition discovery and publication recovery

**Goal:** Make close-success/marker-or-publication failure reconstructable from
empty memory without an unbounded scan of all historical closed convergence
roots.

**Changes:** Before terminal close, persist an open durable transition intent
or install an equivalently bounded store-certified index that survives close.
The pending record identifies the stable root/event ID and required terminal
proof. Startup drains that bounded set, verifies state and close, repairs the
dedup marker, and retires the domain-recovery intent marker-last. It attempts
post-commit replay with the stable event ID under the recorder's honest delivery
contract. A store profile may instead use `IncludeClosed` only if
P0.7 certifies a hard bound/paging contract and every supported store proves the
same filter/order/cursor semantics; an unbounded historical scan is not an
acceptable shortcut. Guaranteed publication beyond stable-ID best effort
requires a durable outbox and is claimed only for profiles that implement it.

**Acceptance criteria:** `RC-CRASH-002` passes from a fresh process with no
caller-supplied root ID for crash after close and before marker and during
pending-next cleanup. Domain recovery is mandatory, idempotent, and bounded by
pending transitions rather than historical roots. Event attempts occur only
after terminal proof and reuse the same stable ID. A best-effort recorder may
still lose an event after domain completion; that failure never reopens or
repeats the domain effect. Crash after marker and before **eventual** publication
is a required recovery case only for a durable-outbox profile. Store/profile
capability output distinguishes best effort from durable-outbox-backed
publication without weakening domain reconstruction.

**Verification:** deterministic re-exec crash matrix, MemStore/FileStore/
BdStore/DoltLite discovery conformance, operation-count and historical-root
scale tests, event dedup replay, and startup leak bounds.

**Dependencies:** G0, P0.7, P0.10, P2.0 conditional-write capability, and an
approved transition-index/outbox schema.

**Likely files:** convergence store contract/adapter, startup recovery index,
schema/version compatibility, and process tests.

**Estimated scope:** M; separate domain recovery from optional durable outbox.

**Rollback:** Keep P1.1A's checked/post-proof behavior and alarm on pending
closed transitions; never revert to pre-commit domain-success events.

### P1.2 Introduce source-level tri-state runtime observation

**Goal:** Make `Alive`, `Dead`, and `Unknown` impossible to conflate while
typing provider-scope outcomes separately.

**Changes:** Extend the existing `runtime.ProbeResult` through the already-landed
`Runtime`/`Place`, `Transport`/`Attachment`, legacy provider adapter,
`session.Manager`, and `worker.LiveObservation` boundaries. A composite result
independently types box presence, agent-process liveness, readiness,
attachment/activity, source scope/completeness, freshness, error classification,
and optional identities. Add provider-scope detail for current tmux server,
exact server absent, server degraded, partial census, exited pane/status, and
`RuntimeScopeLost`; do not introduce another liveness tri-state enum.

**Acceptance criteria:**

- Fetch error, partial list, stale cache, unsupported probe, and cancellation
  yield `Unknown` for the affected dimension. A pane census may remain `Alive`
  while a failed process census makes agent liveness `Unknown`.
- `Dead`/registry-missing requires a successful authoritative observation in
  the relevant scope. A changed tmux server PID+process-start identity yields
  `RuntimeScopeLost`, not per-session death; a live tagged process remains an
  independent veto.
- Existing bool helpers remain compatibility adapters and may not feed a
  destructive decision.

**Verification:** table/fuzz tests over every error/freshness/scope combination,
including server restart with SIGHUP-ignoring tagged process, no-server with
process-census failure, degraded server, partial census, and exited pane; static
guard against `err -> false` on destructive paths.

**Dependencies:** P0.1 and the approved fail-safe semantics in P0.2; the
remaining G0 evidence work may proceed in parallel.

**Likely files:** `internal/runtime/liveness.go`, `internal/worker/observe.go`,
`internal/session/lifecycle_projection.go`, focused tests.

**Estimated scope:** Four S/M slices: type/totality, runtime seams, manager/
worker compatibility, then one destructive consumer plus the static guard.

**Rollback:** Compatibility adapters retain old readers while new type is
unused.

### P1.3 Correct tmux stale-cache semantics

**Goal:** Stop representing an old or failed tmux census as an empty fleet.

**Acceptance criteria:** `RC-OBS-001..005` hold. Stale-but-last-known-good and
unavailable remain distinguishable; stale expiry returns `Unknown`;
`Invalidate` never erases the last observation before a replacement is fetched;
64 dirty readers perform one fetch; invalidation during that fetch causes one
follow-up generation; Stop eviction is not resurrected. A currently validated
exact city-socket no-server result may establish registry-empty while process
truth remains independent; process-local cache history cannot change that fact.
If the last acknowledged box belongs to a different tmux server PID/process-
start identity, classify `RuntimeScopeLost` and run the tagged-process veto—do
not turn whole-registry absence into per-session death.

**Verification:** fake-clock tests for fresh, dirty, stale, failed refresh,
single-flight invalidation, and recovery; real tmux conformance case.

**Dependencies:** P1.2.

**Likely files:** `internal/runtime/tmux/state_cache.go`,
`internal/runtime/tmux/state_cache_test.go`, `internal/runtime/tmux/lifecycle_test.go`.

**Estimated scope:** M.

**Rollback:** Revert provider-local change; fail-safe readers can still map
legacy ambiguity to `Unknown`.

### P1.4 Define the runtime incarnation contract

**Goal:** Give every provider effect a target stronger than a reusable name.

**Changes:** Add opaque box/provision and launch/agent identities to runtime/
worker observations and document provider derivation/comparison. Caller-owned
operation and intended identity tokens are durable before provider entry and
atomically embedded at create/launch. Do not expose provider internals to
session decision code.

For tmux, the provider-owned opaque values are derived from the explicit city
socket; server PID plus `/proc` start identity; `$session_id`; actual agent
`%pane_id`; pane PID plus process start identity; and caller-owned scoped token.
They never rely on one-second `session_created` or a reusable name. The provider
also declares each call's survival scope: process-scoped client calls that are
proven ended by controller death versus remote/server-queued calls requiring
operation lookup, fencing, or bounded ambiguity recovery.

**Acceptance criteria:**

- Equality is the only cross-provider operation.
- Reprovision under the same name changes box and launch identities; warm
  relaunch changes launch identity while preserving box identity.
- Absence/unsupported identity is explicit and makes destructive HA use
  unavailable.
- No PID-only/check-then-PID incarnation is accepted as exact targeting; scan
  evidence includes start/command/environment identity and the strongest Linux
  profile requires an atomic process handle/managed scope.
- tmux server restart with reused `$session_id`/`%pane_id`, same-name replacement
  within one second, focus change, and pane respawn all change or invalidate the
  relevant opaque identity.

**Verification:** provider conformance tests for fake, subprocess, tmux, T3
bridge, and Kubernetes where available.

**Dependencies:** P1.2.

**Likely files:** `internal/runtime/runtime.go`, `internal/worker/types.go`,
provider-specific observation files and tests; implement one provider per PR.

**Estimated scope:** M per provider.

**Rollback:** Additive field can be ignored by legacy callers.

### P1.5 Add unforgeable observation witnesses

**Goal:** Make destructive and live-target actions require proof from the
current `runtime_observation_revision`.

**Changes:** Define `DeathWitness` and `LiveTargetWitness` with unexported
constructors carrying stable key, `runtime_observation_revision`, `intent_generation`,
applicable box/launch identity, native conditional-target capability,
observation time, cold owner instance, and `owner_epoch` only when HA is enabled.
Witnesses are opaque,
copyable proof values and are not persisted; constructor ownership is a compile
boundary, while runtime validation supplies safety.

**Acceptance criteria:** `Unknown`, partial, stale, or mismatched observation
cannot construct a witness; tests outside the owning package cannot forge one;
the zero value is invalid; copied, stale, expired, wrong-generation,
wrong-incarnation, and wrong-epoch values fail executor validation; targeted
confirmation plus a complete `GC_SESSION_ID` process scan is required after
list absence. Any live tagged process vetoes death. A server-instance change
produces `RuntimeScopeLost` and an adopt-or-supervised-stop/escalate plan; it
cannot construct DeathWitness or release work/alias.

**Verification:** compile-boundary tests, fuzzed no-destruction-on-unknown law,
and expiration/generation tests.

**Dependencies:** P1.2, P1.4.

**Likely files:** `internal/session/witness.go`,
`internal/session/witness_test.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M.

**Rollback:** Additive until consumers adopt it.

### P1.6 Protect reconciler close/orphan effects with witnesses

**Goal:** Apply the witness requirement to the highest-incident destructive
session paths before moving their decision logic.

**Acceptance criteria:** orphan close, dead pool cleanup, and runtime teardown
cannot call their effect without a current witness; store/provider errors defer;
the `intent_generation` and applicable identity are rechecked at executor entry,
and the provider conditionally validates its immutable native target at effect
entry for profiles claiming exactness. The family effect inventory is complete
before cutover.

**Verification:** existing close/orphan suites plus false-negative list,
provider error, PID reuse, and replacement-race tests.

**Dependencies:** P1.5.

**Likely files:** `cmd/gc/session_reconciler.go`,
`cmd/gc/session_beads.go`, `internal/session/witness.go`, focused tests.

**Estimated scope:** M; split orphan and pool cleanup if needed.

**Rollback:** Gate witness path while retaining fail-safe defer semantics.

### P1.7 Protect bulk stop and direct kill effects

**Goal:** Cover shutdown, provider-swap, restart, and proctable kills without
serializing unrelated sessions.

**Acceptance criteria:** each kill has the strongest capability-supported box/
launch/process witness; subset/reverse dependency ordering remains intact;
inability to confirm one target does not block independent confirmed targets;
diagnostics identify deferred targets. Shutdown/provider swap stops new city-
wide admissions, drains actual calls, exact-stops, verifies, and only then tears
down the explicit city provider/socket.

**Verification:** lifecycle parallel tests, shutdown/restart coordination tests,
PID reuse tests, and explicit tmux socket isolation.

**Dependencies:** P1.5.

**Likely files:** `cmd/gc/session_lifecycle_parallel.go`,
`cmd/gc/controller.go` or current shutdown owner,
`internal/runtime/proctable`, focused tests.

**Estimated scope:** M per effect family.

**Rollback:** Revert one protected family; do not remove P1.2 safety mapping.

### P1.8 Establish observation/effect provider conformance

**Goal:** Make fakes conform to the real safety boundary rather than define it.
Land observation conformance first, then effect/identity conformance, one real
provider per slice.

**Contract cases:** successful census, partial census, stale census, not-found,
replacement under same name, PID/name reuse, delayed visibility after start,
ambiguous timeout, response lost after effect, success returned without effect,
stop success while runtime remains, watcher/list disagreement, cancellation
before/after entry, a non-cooperative late mutation after caller timeout,
idempotent stop, concurrent distinct-session effects, repeated timeout/reconnect
resource leaks, and unsupported incarnation fencing. Preserve the process-table
fallback required by `SESSION-RUNTIME-001`. Tmux additionally covers
attach/detach during native entry, every key-injection path while in copy mode,
detach-while-in-copy-mode plus bounded orphan-mode recovery, multi-pane focus,
and attribution of controller-injected activity versus real
agent/human output (`RC-OBS-006..007`).

**Acceptance criteria:** fake and subprocess pass in unit/process tiers; tmux
passes an isolated integration tier; T3/Kubernetes adapters pass where present;
unsupported providers fail closed for destructive HA operation.

**Verification:** shared conformance harness plus provider-specific fixtures.
Repeated fault cases assert bounded goroutines/file descriptors and that no
second same-key call overlaps a late first call.

**Dependencies:** P1.3–P1.5.

**Likely files:** `internal/runtime/runtimetest/`, provider tests,
`internal/worker/workertest/`.

**Estimated scope:** M per provider.

**Rollback:** Harness is additive.

### P1.9 Make Start non-destructive and adopt only exact operations

**Goal:** Remove hidden teardown/recycle from provider Start and make response-
loss adoption provable.

**Changes:** tmux Start stages files, attempts create with caller-owned operation
and box/launch tokens, and returns typed collision/last-proven stage. It never
kills a dead pane, zombie agent, or colliding session. Cleanup/replacement is a
separate reconciler action with a witness. A same-name `ErrSessionExists` race
is adopted only after exact tokens/native identity match. Failed-start cleanup
never kills another concurrent caller's accepted runtime merely because both
share a logical intent token.

**Acceptance criteria:** `RC-TMUX-001..002` and `RC-START-001..005` pass,
including collision, response loss, cancel before/after create, concurrent same-
operation callers, late nil after deadline, initializing, dead pane, and warm
relaunch. Any intentional divergence from current startup compatibility is
recorded in `internal/session/REQUIREMENTS.md`.

**Verification:** red unit tests at `startOps`, provider integration on an
isolated tmux socket, and effect-inventory guard covering nested kill/signal
sites.

**Dependencies:** P1.2–P1.4 and P0.10 boundary IDs.

**Estimated scope:** Multiple S/M slices in this mandatory order: typed
collision/result and operation ownership; explicit witnessed dead-pane/zombie
cleanup action; exact adoption/caller wiring; only then remove hidden recycle
and make provider Start non-destructive. Today pending creates rely on hidden
`ensureFreshSession` cleanup while the corpse sweeper skips their claim, so
flipping Start first would create a permanent `ErrSessionExists` loop.

**Rollback:** Retain legacy Start only behind the old owner; never restore hidden
cleanup after the new contract owns effects.

### P1.10 Introduce the context-aware actual-effect seam

**Goal:** Make deadlines/cancellation real inputs without pretending they stop
non-cooperative providers.

**Changes:** Add context-aware Stop, Interrupt, Nudge, SendKeys, metadata, and
other mutating effect methods at the runtime/worker seam; compose caller context
with provider subprocess timeouts and make sleeps/locks interruptible. Legacy
methods adapt temporarily but retain actual-call ownership until return. Wait-
idle becomes a keyed blocker/timer. Add provider/global admission barriers for
shutdown and provider swap. Before each migrated family reaches native entry,
persist its operation/target/claim and define restart resolution through
provider lookup/dedup, fencing, or killable containment; an in-memory permit is
never the only surviving owner record.

**Acceptance criteria:** `RC-TMUX-003` and the actual-call portions of
`RC-NUDGE-001..003` pass. This task exposes the admission/drain primitives used
by `RC-SHUT-001..005`; it does not claim requester fences, ordered city-wide
shutdown, external completion proof, or store teardown before P7.7A/P9.5A. A provider ignoring cancellation forever receives one
entry across repeated CLI/bulk/lifecycle calls; other providers progress; heap,
goroutine, FD, and permit counts stay bounded; release of the late call cannot
commit against a replacement identity. Controller `SIGKILL` after native entry
then empty-memory restart either adopts/fences/resolves that exact operation or
keeps the key blocked; it never overlaps because a process-local lock vanished.

**Verification:** deterministic blocking providers, tmux executor cancellation,
re-exec CLI timeout tests, leak assertions, and a static guard against
`context.Background` at migrated effect entry.

**Dependencies:** P0.13, P1.4, P0.10 initial boundary registry. Full
`RC-SHUT-001..005` evidence depends on P7.7A and P9.5A.

**Estimated scope:** Epic, one method/provider family per S/M slice.

**Rollback:** Compatibility adapter remains only until each family handoff; an
entered legacy call is drained/fenced before rollback.

### P1.11A Fail closed on partial orphan/process scans

**Goal:** Ship the current safety bug fix without waiting for pidfd or managed
scope support.

**Changes:** Preserve exact returned candidates for diagnostics, but propagate a
typed incomplete-scan result from `Manager.killExistingOrphans` and every
equivalent caller. A failed/partial targeted census cannot return nil, certify
absence, or permit replacement Start until a complete targeted rescan.

**Acceptance criteria:** the current log-and-success path has a red regression
test; any unreadable/malformed/omitted candidate leaves replacement blocked;
independent exact targets may progress without turning the overall scan into
success (`RC-PROC-002`).

**Verification:** scripted process-table errors at each enumeration/detail
stage and the pending-create Start caller test.

**Dependencies:** P1.2 only.

**Estimated scope:** S.

**Rollback:** Do not restore false success; the conservative block is the safe
compatibility behavior.

### P1.11B Add exact root handles and managed process-tree containment

**Goal:** Stop claiming PID/process-tree reuse immunity from non-atomic
check-then-signal or an escapable process group.

**Changes:** Extend `LiveRuntime` with one-scan start/command/env/city/session/
launch evidence. Validate it before TERM and KILL. Use pidfd or an equivalent
atomic handle for the root. For the strongest Linux profile, launch the full
tree in a provider-owned cgroup/scope that can be frozen and killed without
fork, double-fork, or `setsid` escape. Platforms without containment use
repeated exact scans to bounded quiescence and explicitly cannot certify exact
tree teardown.

**Acceptance criteria:** `RC-PROC-001` and `RC-PROC-003` pass for the advertised
profile, including PID/PGID reuse between scan, TERM, grace, and KILL;
fork-between-scan, fork-during-grace, double-fork, `setsid`, exec, and subreaper
fixtures remain; macOS/unsupported Linux hosts refuse or downgrade rather than
claiming containment.

**Verification:** deterministic syscall/process-table seams, Linux pidfd and
cgroup/scope real-process smokes, capability-negative OS matrix, and retained
escape seeds.

**Dependencies:** P1.4, P1.10 effect contexts, P1.11A.

**Estimated scope:** M per OS/provider; root-handle and containment support are
separate red/green slices.

**Rollback:** Weaker explicit profile and fail-closed replacement; never a
silent check-then-kill exactness claim.

### P1.12 Add provider-atomic tmux target/effect capability

**Goal:** Make same-name, same-pane-ID, and server-restart replacement races
reject stale stop/nudge/interrupt/teardown operations.

**Changes:** Bind the explicit city socket/server identity, immutable tmux
session/pane IDs, and caller-owned scoped token in one server-serialized
conditional effect. Address commands by witnessed `$session_id`/`%pane_id`,
never reusable name, and pair them with server PID/process-start identity so a
server restart with ID-counter reuse is stale. Implement and conformance-test
the exact mechanism for
stop, interrupt, nudge paste/submit, pane/session teardown, and relaunch; if
stock tmux cannot make an action atomic across all stages, expose that action as
unsupported for the strongest profile or route it through a provider-owned
daemon that can. Raw-name or client-side check-then-effect remains compatibility
only.

**Acceptance criteria:** `RC-ID-003..004`, `RC-TMUX-001..003`, `RC-NUDGE-001`,
`RC-NUDGE-002`, `RC-NUDGE-004`, and `RC-NUDGE-005` pass under rename,
destroy/recreate, server restart/ID reuse,
same-name replacement inside one second, focus change, response loss, and
replacement exactly between every client-side stage. Capability negotiation is
action-specific; one exact Stop cannot
silently certify a multi-stage nudge. `RC-OBS-006` orphan copy-mode recovery
uses the same server-serialized condition: still unattached, exact pane/launch,
and still in mode are checked atomically with cancel or nothing changes.

**Verification:** scripted command boundary races plus real isolated `tmux -L`
command-shape/identity tests. Missing required tmux semantics fails the exact
profile job rather than skipping.

**Dependencies:** P1.3–P1.5 and P1.10. It may land after G1's weaker profile,
but exact-target single-owner/G12 certification depends on it.

**Estimated scope:** Epic; one S/M action family per slice.

**Rollback:** Select the weaker named profile and defer unsafe destructive or
delivery claims; never fall back silently to reusable names.

### Checkpoint G1 — Destruction is fail-safe

- No destructive session path maps error/staleness to absence.
- Every migrated destructive family has mechanical effect completeness,
  tri-state provenance, pre-entry generation/identity validation, ambiguous-
  timeout coverage, and a fresh witness.
- Real provider conformance covers observation, context/actual-call ownership,
  Start collision, and the native target/process capabilities actually enabled.
- The checkpoint names the exact provider/OS profile. Raw-name tmux destruction,
  check-then-PID signaling, legacy tmux “delivered,” and non-context-aware
  timeout cannot receive the strongest certification. G1 may approve a weaker
  fail-safe profile before P1.11B/P1.12, but G12 cannot certify exact-target
  tmux/process teardown until those tasks pass for the named topology.
- Legacy scheduling remains active.

## 15. Phase 2 — Complete the session-owned correctness core

The existing `internal/session` extraction is the ownership spine. Work moves
one decision cluster at a time; callers gather facts and execute returned plans.

### P2.0 Rebase and land the conditional-write foundation

**Goal:** Turn the existing Gas City and Beads CAS branches into reviewed,
current-head capabilities before any task assumes they exist.

**Changes:** Use the reviewed series ending at `1f8596d1c`—not the stale
`fe4edc869` worktree head—as archaeology. Recreate and revalidate small slices:
interface/revision type, MemStore conformance, FileStore persistence, BdStore
probe/classifier, BdStore verbs/emulation, cache forward-and-evict semantics,
factory capability stamping, then rollout/doctor and individual consumers.
Split any Beads-side schema/revision, backend CAS, CLI conditional exit contract,
value CAS, and merge semantics into separate commits. Regenerate API/dashboard
artifacts from the new head. Default behavior remains current single-owner
compatibility until explicitly graduated.

Capability is certified per real store class, not once for “P2.0”:

| Store class/path | Required slice before `require` |
|---|---|
| MemStore | In-memory revision/CAS conformance and deterministic conflict tests |
| FileStore | Persisted revision, atomic conditional update, crash/reopen conformance |
| BdStore over bd-sqlite or bd-Dolt | Pin and land the required Beads conditional CLI/storage contract (including the current #4682/revision work), deadline/error classification, and both backend matrices |
| NativeDoltStore/beadslib | Implement `ConditionalWriter` at its storage boundary; do not infer it from BdStore support |
| DoltLite read/provider path | Populate trustworthy revisions and enable conditional mutation only through its actual writer boundary; a read-only adapter remains unsupported |
| CachingStore and wrappers | Forward exact preconditions, publish only committed revisions, and evict/reread on conflict without fabricating capability |

The configured city graph store, every rig store, command store, and wrapper is
reported independently. A dependent task names the exact causal-path stores it
requires; one passing Mem/File fake never unlocks a production path.

Every store capability report also carries actual `store_uuid`,
`restore_epoch`, `store_schema_version`, writer/provider build, and available
revision/cursor/high-water semantics. These are checked below decoding and
cannot be synthesized by a permissive adapter. P0.15/`RC-STORE-001..003` gates
effectful use; missing conditional writes may select compatibility, but identity
or schema mismatch always selects observation-only/refusal.

**Acceptance criteria:** current-head shared conformance passes for every
implemented store/wrapper; unsupported stores are explicit; no branch-local
assumption appears in production callers; the P0.2 successor decision
distinguishes legacy and conditional profiles without rewriting historical
evidence. G2's evidence packet includes a verdict and exact dependency
commit/digest for every configured store class. Missing Beads #4682-equivalent,
NativeDolt, or DoltLite support is a named blocked capability, not emulation
silently marketed as CAS. Whole-bead metadata-CAS emulation passes
`RC-STATE-003`: dedicated coordination records remain live under adjacent
unrelated write load, and registry/schema guards reject colocation with status
or operator metadata.

**Verification:** conditional-write unit/conformance/process suites, N/N-1
matrix, cache-conflict races, and current upstream range-diff review.

**Dependencies:** P0.1, P0.2, P0.12.

**Likely files:** multiple repository-scoped S/M PRs; Gas City
rollout/conformance wiring is separate from Beads implementation. NativeDolt/
DoltLite remains explicitly unsupported for `require` until its revision/CAS
substrate proves conformance. The Beads dependency is pinned in §29 rather than
assumed from a branch name.

**Estimated scope:** Existing epic, landed as enumerated M slices.

**Rollback:** Leave capability disabled; revert dependency pin without changing
legacy data.

### P2.1 Make lifecycle decoding total over the existing vocabulary

**Goal:** Decode once into canonical typed values without adding a competing
state model.

**Changes:** Extend `LifecycleInput`/`LifecycleView` with explicit unrecognized,
partial, torn-status, generation, and ownership information. Unknown stored
states preserve raw value and first-seen evidence but produce only defer plus a
rate-limited typed event.

**Acceptance criteria:** every stored status/metadata combination produces a
defined view; no default branch silently continues; existing compatibility
states preserve byte/behavior parity; rolling-forward state never quarantines
or destroys under an older binary.

**Verification:** exhaustive table, fuzz totality, raw/typed equivalence, and
mixed-version fixture tests.

**Dependencies:** G0.

**Likely files:** `internal/session/lifecycle_projection.go`,
`internal/session/lifecycle_projection_test.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M.

**Rollback:** Additive fields; legacy projections remain until consumers flip.

### P2.2 Extract wake eligibility with a domain-specific contract

**Goal:** Continue `internal/session/PLAN.md` with the first remaining decision
cluster without inventing the final generic action vocabulary prematurely.

**Changes:** Re-extract the current typed-`Info` wake decision using a small
`WakeDecisionInput`/`WakeDecision` pair. Use `cfdae10e39` and `bfa827fd51` only
as archaeology; rebase against the landed `25d395fc0` session domain rather than
porting stale branches.

**Acceptance criteria:** current holds, quarantine, pending interaction,
configured/named/work/scale causes, and wait-only exceptions remain identical;
fact gathering and failure mapping stay in the caller; returned decision
contains no writes or provider/store handles.

**Verification:** same-SHA characterization parity, direct decision tables, and
the determinism/import lint.

**Dependencies:** P2.1.

**Likely files:** `internal/session/wake_decision.go`, tests,
one caller adapter, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M, split decision from caller wiring if more than five
files.

**Rollback:** Revert caller wiring; pure decider may remain unused.

### P2.3 Generalize the minimal session facts and action vocabulary

**Goal:** Extract shared facts/actions only after wake plus one existing or newly
extracted decider demonstrate the same shape.

**Changes:** Compose only the common value fields from wake and the canonical
identity/priming or timer/exit decider into minimal `SessionFacts` and ordered
`SessionPlan` types. Add action kinds as families migrate; do not predeclare
unused heal/start/drain/close abstractions.

**Acceptance criteria:** imports exclude store/runtime/config loader/I/O; time
is passed as a value; ordering is deterministic; each action cites the durable-
vs-observed disagreement it repairs; every shared field has at least two real
consumers.

**Verification:** determinism/import lint, action-order tests, and a review check
that no unused generic action/fact exists.

**Dependencies:** P2.2 plus a second canonical decider.

**Likely files:** `internal/session/reconcile_types.go`, tests, current
`cmd/gc/session_level_converge.go` compatibility adapter.

**Estimated scope:** S/M.

**Rollback:** Shadow-only types are additive.

### P2.4 Extract close and identity-retirement decisions

**Goal:** Put terminal/continuity/identity rules behind one session-owned plan.

**Acceptance criteria:** stop failure cannot close; assigned work release remains
idempotent; configured named identity retirement and continuity behavior match
scenario rows; destructive execution still requires P1 witnesses.

**Verification:** close cleanup, named session, work release, stale token, and
identity conflict suites.

**Dependencies:** P1.6, P2.2.

**Likely files:** `internal/session/close_decision.go`, tests,
`cmd/gc/session_reconciler.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M.

**Rollback:** Restore legacy decision call; witness protection remains.

### P2.5 Extract pending-create and start-confirmation decisions

**Goal:** Centralize create lease, start-pending, stale-create rollback,
continuation, and confirmation semantics.

**Acceptance criteria:** a stale async result cannot confirm a newer generation;
pending-create metadata clears atomically or through a torn-detectable protocol;
late provider visibility is adopted; failed create retains retryable durable
intent.

**Verification:** existing start boundary/deadline/relaunch/read-after-write
suites plus crash injection at every prewake/start/commit boundary.

**Dependencies:** P1.4, P2.2.

**Likely files:** `internal/session/start_decision.go`, tests,
`cmd/gc/session_lifecycle_parallel.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M per subcluster.

**Rollback:** Route decisions back to legacy; additive generation data ignored.

### P2.6 Extract drain decisions

**Goal:** Make begin, acknowledge, cancel, timeout, stop-pending, and completion
one pure state machine over explicit facts.

**Acceptance criteria:** assigned-work return and pending interaction cancel only
eligible drains; `drain_operation_id` prevents stale acknowledgement; timeout does
not imply provider absence; all terminal paths are explicit.

**Verification:** drain chaos, coordination, acknowledgment, timeout, and
restart recovery tests with a fake clock.

**Dependencies:** P2.2.

**Likely files:** `internal/session/drain_decision.go`, tests,
`cmd/gc/session_reconciler.go`, `cmd/gc/session_lifecycle_parallel.go`.

**Estimated scope:** M; split begin/cancel and completion if needed.

**Rollback:** Restore legacy decisions; typed transition patches remain.

### P2.7 Extract config-drift, restart, and provider-health decisions

**Goal:** Remove the remaining session-local decision ladders while keeping
config/prompt judgment out of Go.

**Acceptance criteria:** config generation mismatch is mechanical; provider red
blocks respawn without consuming restart budget; absent/stale/unknown health
preserves documented behavior; progress/idle decisions remain the already-
extracted config-driven deciders. Provider-health “unknown fails open” from
`SESSION-RECON-006` remains distinct from runtime-liveness `Unknown`, which can
never prove absence or authorize destruction.

**Verification:** drift defer/resume, restart request, provider health, progress,
idle, and max-age suites.

**Dependencies:** P2.2.

**Likely files:** `internal/session/drift_decision.go`, tests,
`cmd/gc/session_reconciler.go`, `internal/session/REQUIREMENTS.md`.

**Estimated scope:** M.

**Rollback:** Revert one decision family.

### P2.7A Preserve and canonicalize the landed identity/priming core

**Goal:** Move the dormant Windshield stage-1 rules into the canonical session
plan instead of reimplementing them during P7.

**Changes:** Port/preserve `deriveConvergeActions`, `desiredSessionIdentity`,
prompt-delivery/first-start rules, priming attempted/confirmed markers, and their
legacy parity pin into `internal/session`. Reuse the current typed lifecycle
projection and action ordering; do not add a parallel state vocabulary.

**Acceptance criteria:** identity values are identical on fresh spawn, adoption,
resume, drift, rollback, and replacement; attempted precedes delivery and
confirmation follows evidence; first-start versus continuation behavior matches
the existing tables; `SESSION-STATE-001..003` legality remains owned by the
canonical transition reducer.

**Verification:** existing `session_level_converge` laws and identity/priming
fixtures, historical #3849/#3872 cases, marker crash boundaries, and direct
canonical-decider tables.

**Dependencies:** P2.1–P2.2 and the relevant P1 witness types.

**Likely files:** `internal/session` identity/priming decider and tests plus a
thin compatibility adapter; split identity and prompt delivery if more than one
M slice.

**Estimated scope:** Epic, decomposed into M slices.

**Rollback:** Keep the canonical decider shadow-only and retain the parity pin.

### P2.8 Compose a pure per-session plan in shadow

**Goal:** Compose the extracted deciders into one deterministic plan without
changing effect ownership.

**Acceptance criteria:** composition has explicit precedence and no hidden
`continue`; converged input returns an empty plan; every legacy session evaluated
produces a shadow plan or an explicit unsupported classification; no action is
executed. Composition reuses `ProjectLifecycle`, lifecycle timers, lifecycle
exits, target classification, and P2.7A identity/priming rules as inputs; it does
not reimplement them.

**Verification:** fixpoint, totality, no-destruction-on-unknown, permutation, and
historical corpus tests.

**Dependencies:** P2.3–P2.7A.

**Likely files:** `internal/session/reconcile.go`,
`internal/session/reconcile_test.go`, `cmd/gc/session_converge_shadow.go`.

**Estimated scope:** M.

**Rollback:** Disable shadow invocation.

### P2.9 Build an atomic or torn-detectable owned-status writer

**Goal:** Replace unordered per-key lifecycle writes with a contract that can
prove whether a complete status was committed.

**Changes:**

- Preferred path: one conditional whole-row update through the existing
  `ConditionalWriter`, guarded by observed revision and `intent_generation`.
- Compatibility path: first invalidate the old marker, write the status body,
  read back the exact canonical body, then write a `status_body_hash`
  marker in a separate checked operation. Decoder hashes the body and treats a
  missing/old/mismatched marker as `Unknown` and heals. A concurrent foreign
  write between read-back and marker must either fail a conditional marker write
  where supported or make the marker/body hash mismatch at the next read; the
  compatibility path never claims it can atomically condition across that gap.
  Never rely on “final map key” ordering in `SetMetadataBatch`; Go map order and
  external sequential batches provide no such guarantee.
- The compatibility path is production-eligible only if P0.12 proves every
  intermediate state is safe under N-1. If an old reader ignores the marker and
  can act on a torn body, require an atomic whole-row backend or declare a
  rollback barrier instead of weakening the protocol.
- On precondition failure, evict/re-read and return a typed requeue result.

**Acceptance criteria:** failure after every possible partial body write is
detected; an old valid marker cannot certify a partial new body; marker cannot
certify anything except the exact read-back body and its separately named
`intent_generation`; foreign writes are
never silently overwritten in conditional mode; misleading partial-success
errors are handled; identical status causes no write. Local/shadow projections
advance only after proven commit. Failed/ambiguous writes return the unchanged
projection plus typed requeue/watermark, and no later action in the reconcile
consumes the proposed patch (`RC-STATE-001`).

“Identical status” is a no-op only when the full canonical body and its marker
are both valid and matching. An intact body with missing/stale/mismatched marker
performs a checked marker-only repair; otherwise a torn prefix would remain
`Unknown` forever. The marker covers the one full canonical lifecycle body.
Before mixed family ownership, P2.10A routes every surviving legacy and keyed
status mutation through one physical per-bead writer/serializer, so a legacy
disjoint-key merge cannot race between body and marker.

**Verification:** shared store conformance across Mem/File/Bd/native/Caching
stores, every body-write permutation/prefix, foreign write between body/read-
back/marker, marker success plus event failure, stale-revision races, N-1 reads
of every intermediate state, conditional-vs-compatibility final equivalence,
marker-only repair after an intact-body crash, concurrent legacy disjoint-field
writes, and crash recovery.

**Dependencies:** P2.0, P2.1, and P0.12.

**Likely files:** `internal/session/status_writer.go`, tests,
`internal/beads` conformance helpers, one caller adapter.

**Estimated scope:** M per store path.

**Rollback:** Use compatibility writer in single-controller mode; HA remains
disabled.

### P2.10 Guard lifecycle ownership mechanically

**Goal:** Stop new raw lifecycle reads/writes from rebuilding the problem during
migration.

**Changes:** Add AST/static tests that allow construction/repair/migration
exceptions explicitly and reject new production `SetMetadata*`, raw state
switches, direct provider lifecycle effects, or status event emission outside
owned boundaries.

**Acceptance criteria:** existing exceptions are named with removal phases; a
new violation fails with the canonical API to use; wait/nudge bead state is not
misclassified as session lifecycle state.

**Verification:** guard self-tests with positive/negative fixtures.

**Dependencies:** P2.1, P2.9.

**Likely files:** `internal/session/lifecycle_projection_test.go`,
`cmd/gc/worker_boundary_import_test.go`, new guard test.

**Estimated scope:** M.

**Rollback:** Narrow false-positive rules; do not delete ownership APIs.

### P2.10A Route all mixed-era lifecycle writes through one physical writer

**Goal:** Preserve per-bead metadata-blob atomicity while decision families move
one at a time.

**Changes:** Before P7.3, route every surviving legacy and keyed lifecycle
status mutation through P2.9's single per-session writer/serializer at the
existing session front door. Decision ownership may split by family, but the
physical read-merge-write/checksum operation does not. Enumerate the exact
owned-field groups for diagnostics/parity without allowing independent blobs or
writers. Stores that cannot condition or serialize concurrent disjoint-key
updates refuse mixed-family cutover.

**Acceptance criteria:** concurrent legacy write of family B between keyed
family A cache-read/body/marker points cannot lose either update or certify a
hybrid body; compatibility and conditional modes pass the same interleavings;
every raw bypass is a named temporary exception with removal owner. Migration
rule 5 holds at the store's actual atomicity unit (whole bead/metadata blob),
not merely an action-family label.

**Verification:** per-store concurrent disjoint-field lost-update tests,
body/readback/marker barriers, race detector, and static reachability guard for
the session front door.

**Dependencies:** P2.0, P2.9, P2.10.

**Likely files:** `internal/session/status_writer.go`, current session store
front door/legacy adapters, and focused tests.

**Estimated scope:** M, split by store adapter if needed.

**Rollback:** Mixed-family canary is blocked until the shared writer remains;
rollback changes decision ownership, never reinstates a second blob writer.

### P2.11 Build the atomic/idempotent lifecycle request front door

**Goal:** Replace torn multi-key desired-state writes and socket-selected CLI
ownership with one durable command/intent contract.

**Changes:** Define one total versioned lifecycle request carrying request ID,
stable session binding, desired phase, monotonic intent generation, command-
specific data, compatibility version, trusted immutable requester provenance,
and authorization decision reference. Provenance is stamped by the trusted
front door from authenticated context, never copied from caller JSON/bead
metadata; the claimer revalidates current policy before effect admission.
Commit/read-back by request ID is
idempotent; retrying the same request never advances generation. Managed CLI/API
submits only; nudge, interrupt/stop-turn, respond, wake/suspend/reset, kill,
close, restart, and stop cannot reach `worker.Handle`/provider mutation from the
foreground process once their family has a managed owner. Unmanaged one-shot
execution claims the same key/owner protocol using a live cross-process kernel/
store claim and the shared session executor; a path or process-local mutex alone
is not ownership. Any whole-bead-emulated claim uses the P2.0 dedicated
coordination record and key-placement guard; it never shares the lifecycle
status bead (`RC-STATE-003`).
Compatibility mirrors are marker/checksum guarded and old-reader-safe or blocked
by an explicit rollback barrier.

**Acceptance criteria:** `RC-CLI-001..010`, `RC-STATE-002`, and
`RC-AUTH-001..003`, `RC-EVENT-001..004`, and `RC-STORE-001..003` pass for each
migrated command family. Store commit response
loss, controller acknowledgement loss, SIGINT, output failure, alias reuse, and
controller appearance during unmanaged execution never create a second effect.
Static guards forbid migrated lifecycle desired `SetMetadataBatch` call sites
and managed foreground/API direct provider mutations. Every family cutover
closes or explicitly retains each executing-process inventory row.

**Verification:** pure codec/transition tests, conditional/compatibility store
conformance, N/N-1 per schema, re-exec CLI harness, and one command family
vertical slice before reuse.

**Dependencies:** P0.13–P0.15, P2.0–P2.1, P0.11–P0.12, and P1.10 for effectful
families.

**Likely files:** `internal/session/lifecycle_request.go`, store/front-door
adapter and tests, one `cmd/gc` command family.

**Estimated scope:** Epic; codec/store first, then one S/M family per PR.

**Rollback:** Old reader remains; exclusive handoff returns ownership only after
all new actual calls/claims drain. No dual writer.

### P2.11A Complete durable stop request ownership

**Goal:** Replace P1.0D's deliberately conservative ambiguity exit and legacy
direct exception with one reconstructable stop operation and a real one-shot
owner protocol.

**Changes:** Route managed stop through the P2.11 durable request front door
with a caller-generated operation ID, exact target/fingerprint, commit-state
readback, completion lookup, and force revision. An unmanaged stop uses the
same executor only after a live conditional claim/lease; it never infers
ownership from socket failure. Remove the expiring direct-stop exception from
P1.0D when every supported entry point uses this path.

**Acceptance criteria:** `RC-CLI-001..005`, the stop request/acceptance portion
of `RC-CLI-006`, `RC-SHUT-002`, `RC-SHUT-005`, and applicable requester-
authorization rows pass for stop and stop-force. The matrix includes commit-
then-timeout, acknowledgement loss, controller death/restart, force upgrade,
caller cancellation, competing one-shot/controller ownership, stable operation
lookup, and same-path/default-socket isolation. This task does not claim the
provider-global admission owner, full shutdown timeout/partial-observation
semantics, or city-stop terminal completion; P9.5A closes those rows. The
exception inventory has no unexpired foreground direct provider/store stop
writer.

**Verification:** P0.13 re-exec matrix, P2.11 store/CLI conformance, real
isolated-socket stop/force tests, and N/N-1 request readers.

**Dependencies:** G0, P0.13–P0.15, P2.0, and P2.11. Full city-stop completion
remains dependent on P5.11, P7.7A, and P9.5A.

**Likely files:** durable lifecycle request front door, stop CLI adapter,
one-shot claim/executor adapter, and focused process tests.

**Estimated scope:** M, decomposed by managed request then unmanaged one-shot.

**Rollback:** Cold-select the P1.0D fail-closed adapter; never restore may-
have-entered direct fallback.

### P2.12 Define level-triggered terminal convergence for close/kill

**Goal:** Make terminal lifecycle cleanup recoverable across every current
Stop/cancel/identity/close/work-release boundary.

**Changes:** Close commits one desired-closed generation, then each reconcile
derives the next idempotent exact-stop, wait cancellation, safe override cleanup,
identity retirement, bead close, cross-store work release, or event step from
authoritative facts. Persist only ambiguous/non-idempotent boundaries and the
terminal proof; do not build a generic multi-step workflow engine or duplicate facts as a step
log. Kill binds the exact old box/launch, then converges killed/asleep/circuit
state; restart is a new `intent_generation`.

**Acceptance criteria:** `RC-CLOSE-001..004` and `RC-CLI-010` pass. Crash after
every step resumes; stop failure never closes; store failure after stop never
wakes a replacement; repeated terminal commands do not duplicate work release
or mint a new logical terminal event ID; self-close reports before its own
launch is terminated. Best-effort event delivery may be zero-or-duplicate
across publish/marker crash unless that event family separately adopts a durable
outbox; consumers deduplicate the stable event ID.

**Verification:** pure terminal-convergence model, generated per-step fault
table, fresh-process crash matrix, provider/store
fault injection, `SESSION-START-005..006`, `SESSION-WORK-001..004`, and the
self-close regression.

**Dependencies:** P1.6–P1.10, P1.11A/B, and P1.12 as applicable to the named
provider profile; P2.4, P2.9, and P2.11.

**Estimated scope:** Epic; close intent, exact stop, bookkeeping steps, and kill
are separate M slices.

**Rollback:** Legacy remains sole owner until the terminal-action family passes
exclusive handoff; in-progress durable convergence state remains readable by
the rollback release or blocks rollback explicitly.

### Checkpoint G2 — Correctness core is independently testable

- Canonical decoding is total.
- Session facts produce a deterministic complete shadow plan.
- All current decision clusters live in or are explicitly excluded from
  `internal/session`.
- Status writes are atomic or provably torn-detectable.
- Lifecycle desired requests are total, idempotent, and torn-safe; CLI/control
  ownership is explicit.
- Close/kill terminal transitions are model-tested and restart-resumable before
  their production cutover.
- New raw lifecycle ownership violations fail CI.
- Legacy effects still own production.

## 16. Phase 3 — Build a trustworthy differential oracle

The current trace remains operational evidence, but it is not a complete effect
oracle. Parity counts only after actual effect boundaries are structurally
interposed.

### P3.1 Define canonical effect and plan-step types

**Goal:** Compare legacy and new behavior semantically rather than by timestamp,
map order, or goroutine completion order.

**Changes:** Define closed effect kinds for durable create/update/close, provider
start/stop/interrupt/nudge, process kill, assignment/hook mutation, and externally
visible events. Each normalized step carries key, action family, desired
generation, target incarnation, owned field group, and required happens-before
edges. Extend the P0.3 `ActionFamily`/operation context rather than defining a
second taxonomy.

**Acceptance criteria:** timestamps/tokens can be normalized; per-key ordering is
retained; cross-key order is explicitly irrelevant unless a dependency edge
requires it; nudge command IDs/outcomes compare exactly.

**Verification:** canonicalization tables and property tests for map/input order.

**Dependencies:** P0.1, P2.2.

**Likely files:** cross-boundary identity/stage types extend
`internal/operation`; domain plan/effect types remain in `internal/session`,
`internal/nudgequeue`, and the relevant tests. `cmd/gc` owns only normalized
comparison/adaptation.

**Estimated scope:** M.

**Rollback:** Types remain test-only until interception.

### P3.2 Interpose durable session-status effects

**Goal:** Observe actual status/spec/command writes at their owner boundary.

**Acceptance criteria:** every session lifecycle write produces exactly one
normalized attempted/result step; precondition failure, partial failure, and
no-op diff are distinguishable; trace failure cannot alter the write result.

**Verification:** spy writer and injected-error tests; compare P0.1 inventory to
registered sites.

**Dependencies:** P3.1, P2.9.

**Likely files:** `internal/session/status_writer.go`, one effect adapter,
focused tests.

**Estimated scope:** S.

**Rollback:** Disable recorder; writer behavior unchanged.

### P3.3 Interpose provider and process effects

**Goal:** Make `worker.Handle` and the audited proctable boundary the mandatory
observation points for session mutations.

**Acceptance criteria:** start/stop/interrupt/nudge/kill carry mandatory effect
context; direct provider calls outside explicit temporary exceptions fail CI.
Every call records entry and either return/panic or an explicit still-in-flight
state. Caller timeout/cancellation is not mislabeled as provider termination:
an entered non-cooperative call retains its key/provider permit and may record
an ambiguous caller result until the actual call returns or is atomically fenced.

**Verification:** worker fake/spy tests and an intentionally unregistered fixture
that proves the guard fails.

**Dependencies:** P3.1, G1.

**Likely files:** `internal/worker/operation_effects.go`, tests,
`cmd/gc/worker_boundary_import_test.go`.

**Estimated scope:** M.

**Rollback:** Context can be supplied by compatibility adapters while legacy
owners remain.

### P3.4 Add the mechanical completeness gate

**Goal:** Prevent false-green shadow results from incomplete instrumentation.

**Changes:** An AST/import test compares production effect calls to registered
owner boundaries and named temporary exceptions. The count is derived from code,
never frozen as “57.” Each exception names its removal phase and action family.
Enforcement is deny-by-default at capability/import boundaries: only registered
packages/files may import `os/exec`, process-table or runtime-provider mutation
packages, or reference tmux binary/socket constants. This extends the existing
worker-boundary guard and catches a new wrapper or `os.RemoveAll`/subprocess
vehicle that a hand-maintained call-name list would miss.

**Acceptance criteria:** adding any new unregistered provider/store/process
effect fails CI; deleting a site requires deleting its registration; optional
trace checks cannot satisfy completeness.

**Verification:** positive/negative fixture packages and current-head inventory
comparison.

**Dependencies:** P3.2, P3.3.

**Likely files:** `cmd/gc/reconcile_effect_boundary_test.go`, test fixtures.

**Estimated scope:** M.

**Rollback:** Narrow incorrect classifications; do not disable for convenience.

### P3.5 Compare legacy and typed fact gathering

**Goal:** Prove the new input view matches current durable/config/runtime facts
before comparing decisions.

**Compared facts:** decoded lifecycle, desired/config generation, runtime state
and incarnation, pool membership, dependencies, work demand, holds/quarantine,
pending interaction, drain/start expectations, and partial-source flags.

**Acceptance criteria:** every mismatch is either an approved fail-safe
divergence or a blocking defect; missing source data is explicit; comparison
never performs a write.

**Verification:** corpus replay, generated metadata cases, partial store/runtime
cases, and production shadow counters.

**Dependencies:** P2.8, P3.4.

**Likely files:** `cmd/gc/session_converge_shadow.go`, tests,
`internal/session/reconcile.go`.

**Estimated scope:** M.

**Rollback:** Disable fact shadow.

### P3.6 Compare normalized plans and effects

**Goal:** Produce one per-action-family parity report with declared equivalence.

**Acceptance criteria:** level-triggered actions compare eventual owned state;
nudge commands compare exact conservation/outcome; `Unknown -> defer` versus a
legacy destructive action is an approved fail-safe divergence; every other
exception requires two-maintainer sign-off and an expiry.

**Verification:** deliberately injected mismatches for each action family,
corpus replay, and stable report schema tests.

**Dependencies:** P3.5.

**Likely files:** `cmd/gc/session_converge_shadow.go`,
`cmd/gc/session_converge_shadow_test.go`, trace report types.

**Estimated scope:** M.

**Rollback:** Shadow-only.

### P3.7 Build the independent bounded-state reference model

**Goal:** Prove bounded domain/action recovery laws without calling production
decision functions as the oracle. Scheduler mechanics are added only after P5
defines them.

**Model state:** durable generations, durable nudge commands, runtime
incarnations, action intents/outcomes, virtual time, and emitted effects.

**Acceptance criteria:** exhaustive enumeration covers one/two keys and two
generations; larger generated histories shrink and preserve replay seeds;
quiescence means no domain plan remains. The report publishes state dimensions,
bounds, explored state/transition counts, path depth, pruning/symmetry rules, and
invariant results. An import guard forbids production deciders or their decision
helpers; shared value types are allowed.

**Verification:** model self-tests with seeded known-bad implementations.

**Dependencies:** P3.1.

**Likely files:** `internal/reconcilemodel/` or test-local package; do not create a
production generic engine. P5.9 and P10.6 extend this one model/replay format.

**Estimated scope:** M, test-only.

**Rollback:** Remove test model.

### P3.8 Add pure-core laws and determinism lint

**Goal:** Make totality, fixpoint, and safe ordering structural merge gates.

**Acceptance criteria:** required laws are never panic/silent-skip;
deterministic for identical facts;
input permutation invariance; apply/re-observe fixpoint; no destructive action on
`Unknown`; action order absoluteness; new `intent_generation` supersedes old retry. The
guard forbids direct map range in decision code (sorted helpers only), rejects
func/chan/unapproved-interface fields throughout fact/action graphs, and scans
transitive imports plus mutable package state. Every corpus fixture produces
byte-identical normalized plans across repeated merge-tier runs.

**Verification:** fuzz/property tests plus an import/source lint forbidding I/O,
ambient clock, randomness, goroutines, and global mutable state in the core.

**Dependencies:** P2.8. P3.7 supplies independent evidence but is not required
to run the production pure-core laws.

**Likely files:** `internal/session/reconcile_test.go`,
`internal/session/reconcile_determinism_test.go`.

**Estimated scope:** M.

**Rollback:** None expected; fix false assumptions in facts or laws explicitly.

### P3.9 Run production decision shadow

**Goal:** Exercise typed facts and plans continuously with zero new effects.

**Acceptance criteria:** shadow never claims/acks a command, writes status, calls
a provider, or changes iteration/scheduling; budgets cap CPU/memory/trace volume;
loss is explicit; reports group by action family and reason without high-cardinality
metrics. Legacy and typed results are compared from identical durable/config/
runtime/owner watermarks. Every exception is a mechanical matcher citing the
specific watermark delta or named fail-safe difference, with owner/expiry and a
numeric aggregate ceiling; promotion permits zero unexplained effects
(`RC-MIG-003`).

**Verification:** non-interference tests, resource budgets, and a canary city
showing legacy behavior byte-for-byte unchanged.

**Dependencies:** P0.5, P3.4–P3.8.

**Likely files:** `cmd/gc/session_converge_shadow.go`,
`cmd/gc/city_runtime.go`, trace tests.

**Estimated scope:** M.

**Rollback:** Set migration mode to legacy.

### P3.10 Attribute native effects with an independent runtime observer

**Goal:** Ensure zero-tolerance canary safety signals are not self-reported only
by the acting code path.

**Changes:** Consume the mandatory worker/provider-native effect record and an
independently collected runtime scope/incarnation history. For each effect,
reconstruct whether the targeted box/launch was alive, current, and owned at its
native-entry instant; flag missing witness provenance, wrong generation/
incarnation, server-scope change, wrong target address, or effect after owner
handoff. This is the minimal read-only attribution slice of P10.4 pulled forward
before any nudge/destructive canary; it never decides or executes an action.

**Acceptance criteria:** injected acting-path lies/omissions are detected by the
observer; a safety abort counter cannot originate solely from the code it
certifies; partial observer history is inconclusive and blocks promotion rather
than producing zero. Nudge wrong-launch, stop after same-name replacement, and
legacy effect after owner flip are permanent cases.

**Verification:** independently authored runtime histories, real isolated tmux
server restart/rename/recreate, effect-recorder omission fixture, and cross-
process clock/order correlation bounds.

**Dependencies:** P1.2–P1.5, P3.3–P3.4, and the action family's exact effect
schema. It may run beside P3.5–P3.9.

**Likely files:** read-only effect-attribution test/canary observer and bounded
trace types; no provider mutation path.

**Estimated scope:** M.

**Rollback:** Disable canary, not the detector requirement; acting-path counters
alone cannot certify a family.

### Checkpoint G3 — Shadow evidence is trustworthy

- Effect-boundary completeness is 100% for audited action families.
- Fact and plan parity have no unexplained differences in corpus and canary.
- The independent model and pure-core laws pass.
- Shadow mode has no observable side effects or material latency impact.
- Families approaching canary have independent native-effect attribution; G3
  may approve other shadow-only families while P3.10 completes.

## 17. Phase 4 — Add durable incremental change capture

This is the largest substrate risk, so it is capability-profiled rather than
placed wholesale on the first vertical critical path. Local same-process hints
plus bounded authoritative relist/audit are enough for an audit-only keyed
canary. A no-gap DoltLite feed is required only for feed-certified external-
write latency and before deleting the corresponding full discovery path.

### P4.0 Define the shared immutable projection-install contract

**Goal:** Give store and runtime observation managers one small, tested handoff
mechanic without coupling either manager to the other's source implementation.

**Contract:** prepare an immutable source batch off-lock; atomically publish its
objects/tombstones and secondary indexes; record exact dirty keys or a
reconstructable scoped-fanout marker; then advance the source watermark and
handler-sync state. Readers see the complete old or new `projection_generation`. Source-
specific cursor, freshness, and gap semantics remain in their adapters.

**Acceptance criteria:** cache objects are immutable after publication; object/
index/fanout publication cannot be partially visible; a crash or panic before
publication leaves the old generation, while one after publication reconstructs
dirty work; installer performs no backing I/O or domain decisions under its
publication lock; two source managers consume the contract before it graduates
as shared code.

**Verification:** old-or-new reader barriers, panic at each prepare/publish/
dirty/watermark point, large scoped fanout, concurrent readers, race detector,
and a seeded intentionally in-place bad implementation.

**Dependencies:** P0.8 stable key/fanout contract and P0.10 crash registry.

**Likely files:** a small immutable projection installer package plus contract
tests; no store/runtime provider code in this slice.

**Estimated scope:** M.

**Rollback:** Keep equivalent domain-local installers until both consumers pass;
shared contract is additive.

### P4.1 Timebox a Beads/DoltLite change-feed spike

**Goal:** Select a no-gap mechanism based on current Beads and Dolt behavior.

**Evaluate:** native transaction/commit log, database diff cursor, transactional
outbox/change table, commit hook plus journal, and blocking tailer wakeups. Test
durable issues, no-history rows, wisps, deletes, direct external `bd` writes,
rollback, compaction, and split stores across bd-sqlite, BdStore/bd-Dolt,
NativeDolt, and DoltLite. Treat a Beads-owned transactional outbox/change table
as the backend-neutral primary candidate because the live topology is not
Dolt-only; provider-native diff feeds remain valid optimizations only after the
same contract passes.

**Acceptance criteria:** a prototype proves snapshot+watch without a list/watch
race and reports exact behavior under crash/reconnect; direct `bd` mutation is
covered; event JSONL/socket hooks alone are rejected as authority; one mechanism
is chosen with retention and backpressure semantics.

**Failure branch:** If no no-gap mechanism is viable, mark the production store
audit-only, retain same-process fast application plus bounded authoritative
polling, publish its worst-case detection bound, and block the external-write
low-latency profile. Do not weaken the snapshot/watch contract to call the spike
successful.

**Verification:** real isolated DoltLite concurrency harness and written evidence.

**Dependencies:** G0.

**Likely files:** Beads spike tests and
`engdocs/plans/reconciler-redesign/change-feed-spike.md`; no production Gas City
code.

**Estimated scope:** M, timeboxed.

**Rollback:** Delete spike code.

### P4.2 Define the optional snapshot/watch capability

**Goal:** Express a backend-neutral feed without adding it to the mandatory
`Store` interface.

**Contract types:** opaque per-store cursor, consistent snapshot,
`ChangeBatch{cursor, changes}`, object revision/lineage, tombstone, watcher,
typed cursor-gap, source ID, source-owned position comparison, optional
progress/bookmark, and terminal watch errors.

**Acceptance criteria:** snapshot returns the cursor from which subsequent watch
cannot miss a commit; cursor ordering is opaque; no cross-store order is
promised; slow consumers either backpressure within a bound or receive a gap;
rollback emits nothing; duplicate replay is legal. Every change can be related
to applied state by source-owned commit position, predecessor lineage, or a
provider comparison operation; incomparable order forces reread/gap instead of
guessing. One backing transaction's batch is prepared then atomically published
with its indexes before effects can run. Progress/bookmark records advance only
the known stream position and promise no cadence.

A malformed/unsupported/poison record is never skipped and cannot advance the
cursor. It marks the source unsynced and forces a consistent relist. If the same
poison cannot be bypassed by a snapshot at a later valid cursor, retry is bounded,
the source remains terminal-unsynced with alarm/profile downgrade, and no
dependent effect runs (`RC-FEED-001..002`).

**Verification:** contract/fake tests written before providers.

**Dependencies:** P4.1.

**Likely files:** `internal/beads/change_source.go`,
`internal/beads/change_source_test.go`, `internal/beads/beadstest/change_source_conformance.go`.

**Estimated scope:** M.

**Rollback:** Optional capability is additive.

### P4.3 Implement same-process post-commit application

**Goal:** Eliminate avoidable watch latency for writes made through the current
process.

**Changes:** After backing commit succeeds, submit the committed typed object and
source sequencing token to the same serialized applier used by watch delivery,
then record affected keys/fanout. Never enqueue before commit. The fast path
mutates the cache only when its returned token can be ordered against applied
state; otherwise it installs a write watermark and triggers authoritative reread
or waits for feed catch-up. If application cannot proceed, leave the durable
feed/audit to recover and record the failure.

Watermark clearing follows §7.3 exactly: a serialized full authoritative reread
of the key clears it regardless of token comparability; a complete consistent
relist clears all watermarks for that store; a store with no trustworthy token
does not install one. Every watermark has age/profile-bound telemetry.

**Acceptance criteria:** local and later feed duplicate are idempotent; callback
failure cannot change write success; no backing I/O occurs while a cache lock is
held; deletes use tombstones; a delayed older external update cannot overwrite a
newer local apply; the key cannot reconcile below the committed write watermark.
No watermark can survive its authoritative reread/relist or exist indefinitely
without an alarm/family block.

**Verification:** commit-failure, callback-failure, replay-before/after-local,
`external rev2 -> local rev3 -> delayed rev2`, incomparable-token reread/gap,
concurrent write, failed local apply plus feed catch-up, and process-restart
tests, plus store rollback/token regression, gap/relist clearing, no-revision
profile, and watermark-age abort.

**Dependencies:** P4.0 and the current store's orderable revision/dirty-overlay
contract. A feed-certified implementation also consumes P4.2; the first local
audit-only slice does not.

**Likely files:** `internal/beads/caching_store_writes.go`,
`internal/beads/caching_store_events.go` or new applier file, focused tests.

**Estimated scope:** M.

**Rollback:** Disable callback; watchdog/feed retain correctness.

### P4.3A Certify the bounded relist profile

**Goal:** Make local post-commit keyed scheduling safe before a cross-repository
no-gap feed exists.

**Changes:** Reuse the current CachingStore watchdog/authoritative list path to
periodically rebuild a fresh projection by independently enumerating raw store
objects, applying the canonical total decoder and pure
`Contributions(object)`, and atomically swapping. Same-process commits mark
exact keys immediately; unknown/external changes are rediscovered within a
declared bound and may enqueue a reconstructable scope.

**Acceptance criteria:** missed local callback, external write, cache loss, and
mapper omission converge within the numeric audit-only recovery bound; partial/
failed authority yields unsynced/defer; action workers never wait for or execute
inside the relist; one changed local object reaches its key without a fleet scan.
This profile makes no external-write low-latency claim.

**Verification:** fake-clock relist, operation counts, empty-memory restart,
seeded cache/index corruption, partial list, and one real isolated store case.

**Dependencies:** P4.0, P4.3, P0.7/P0.9 profile schema.

**Estimated scope:** M for one store/controller family; share only after a
second consumer.

**Rollback:** Existing global reconciliation/watchdog remains active.

### P4.3B Establish one serialized projection owner per store

**Goal:** Prevent CachingStore's existing mutation ingresses from racing the new
applier and manufacturing the entropy audit is supposed to detect.

**Changes:** Inventory write-through, `ApplyEvent`, reconcile/watchdog merge,
prime/install, conditional refresh/conflict eviction, and every store-specific
cache mutation ingress. For each store class, route all enabled paths through
one revision/lineage-ordered serialized applier or disable the old ingress under
the migration gate before claiming `INV-02`, `INV-03`, and `INV-04`. Record whether that owner is
CachingStore-internal or a `cmd/gc` watch projection; never both.

**Acceptance criteria:** one source record cannot advance cursor/dirty state
before all object/index changes publish; old local/event/refresh delivery cannot
overwrite a newer projection; every ingress participates in the same generation
barriers and operation-count metrics; direct mutation fails a static guard.

**Verification:** generated ingress inventory, current advance-before-apply
hazard regression, all pairwise ordering barriers, race detector, and per-store
conformance.

**Dependencies:** P4.0, P4.3, and current CachingStore characterization.

**Likely files:** `internal/beads/caching_store*.go`, one applier/inventory guard,
and focused tests.

**Estimated scope:** M per store ingress group.

**Rollback:** Keep the legacy projection owner exclusively; never enable both
paths.

### P4.4 Implement the selected durable feed in Beads

**Goal:** Make external/direct mutations observable without relying on Gas City
events.

**Acceptance criteria:** all committed user-visible mutations and deletes emit a
change after commit; transaction rollback emits none; restart resumes; retained
history expiry produces typed gap; schema migration is additive and downgrade
behavior is tested; outbox/journal cleanup cannot race an active cursor silently;
domain mutation and journal/outbox commit atomically or derive from the
authoritative commit log. Poison cannot advance or relist-loop.

**Verification:** shared conformance plus real DoltLite tests, crash after commit
before response, concurrent writers, direct CLI write, retention, compaction, and
restart. Add poison/corrupt change, cursor rewind/reuse/fork, duplicate cursor
with different payload, tombstone then stale resurrection, journal cleanup racing
a slow watcher, split-store watermark skew, backlog exhaustion, silent omitted
record found by audit, and every apply-versus-cursor-advance crash boundary.

**Dependencies:** P4.2 and Beads repository approval.

**Likely files:** Beads provider/schema/CLI implementation; Gas City dependency
pin in a separate follow-up.

**Estimated scope:** Cross-repository epic; enumerate M PRs per repository.

**Rollback:** Old readers ignore additive journal; disable feed and relist.

### P4.5 Implement capability/degradation across Gas City stores

**Goal:** Give each store an honest result: conforming feed or explicit
unsupported capability.

**Store policy:** Mem/File may implement deterministic test/dev feeds;
bd-sqlite, BdStore/bd-Dolt, NativeDolt, and DoltLite each implement only what
P4.1/P4.4 proves for that exact backend/tier; no “native Dolt” assumption grants
a feed. CachingStore delegates through the single P4.3B projection owner;
read-only and exec stores reject with a typed sentinel. Compatibility
controllers retain audit correctness but cannot claim low-latency SLO for
unsupported external writes.

**Acceptance criteria:** every configured store resolves exactly one
feed-certified or audit-only profile; wrappers preserve the backing capability
and source identity; unsupported capability is typed, visible in doctor/status,
and never silently promoted to low-latency. The common deployed composite
(`bd`/sqlite graph or city store plus Dolt/DoltLite rig stores) has an explicit
per-store verdict and derives the weakest causal-path profile. Durable,
no-history, and ephemeral/wisp tiers each pass visibility/delete/retention
conformance; nonterminal command records are never GC-eligible.

**Verification:** one shared conformance suite per implementing store and
explicit unsupported tests for every other store/wrapper/class store.

**Dependencies:** P4.3B and P4.4 for feed implementations; G4A may record an
audit-only verdict without P4.4.

**Likely files:** one provider implementation/test pair per PR,
`internal/beads/factory.go`, resolution tests.

**Estimated scope:** M per provider.

**Rollback:** Capability is optional; factory falls back loudly.

### P4.6 Build the per-store watch manager

**Goal:** Own initial snapshot, watch resume, gap recovery, and shutdown without
blocking domain queues.

**Handoff order:** prepare and atomically publish a complete snapshot batch plus
indexes; establish a no-gap watch (or discover a gap); record initial exact keys
or paged scope-fanout markers; mark source projection and each handler synced;
only then start effect workers. “Synced” means this population/handoff completed,
not that the source will remain current forever.

**Acceptance criteria:** changes arriving during snapshot are not lost;
reconnect uses the last fully handed-off cursor; `lastAppliedCursor` advances
only after object/index publication and every affected key is recorded in a
reconstructable dirty/fanout structure. Capacity exhaustion backpressures before
cursor advancement, records a scoped overflow/fanout marker, or returns a typed
gap—never drops a key. Gap pauses the stream, relists, atomically replaces the
projection, and resumes; cached objects are immutable outside the serialized
applier; stale source readiness degrades destructive decisions; one failed store
does not freeze other stores; no cursor status file is required.

Direct mapper work has a bounded allocation/time budget. A broad config or
unclassified change emits a paged `ScopedFanoutKey` processed outside the
serialized applier. Once that reconstructable marker is recorded, the feed cursor
may advance without materializing every child key; the marker remains dirty
until paging completes and exposes age/lag telemetry.

For `good(c1), poison(c2), good(c3)`, the applied cursor stays at `c1` until a
consistent snapshot installs state at/after `c3`. Recurring poison stays
bounded-terminal unsynced; it never creates an unbounded relist/log loop.

**Verification:** fake source scripts for disconnect, duplicate, gap, slow
consumer, poison record, snapshot failure, apply-before-cursor crash,
cursor-before-dirty attempted crash, overflow/backpressure/fanout, handler-sync
barrier, shutdown, and multi-store independent progress.

**Dependencies:** P4.0, P4.2, P4.3B, P4.5.

**Likely files:** `cmd/gc/store_watch_manager.go`, tests,
`cmd/gc/controller_state` wiring.

**Estimated scope:** M.

**Rollback:** Manager disabled; existing cache reconciliation remains.

### P4.7 Publish immutable config generations and deltas

**Goal:** Give later key mappers one atomic validated config delta without
prematurely coupling publication to the scheduler.

**Changes:** Parse/validate config off the hot action path, publish one immutable
config generation, and emit a typed diff of affected agents/pools/providers/
orders. Preserve current atomic old-or-new config behavior. P5.1 maps that delta
to stable exact/fanout keys; this task does not enqueue scheduler work.

**Acceptance criteria:** invalid reload keeps the old generation; one generation
is visible atomically; config name/city identity rules remain; broad unclassified
diff is represented as an affected scope; provider/store reconstruction is
explicit and fenced. A periodic independent fingerprint/reparse of the city and
imported pack targets repairs a missed fsnotify/debounce edge and publishes the
latest generation; partial/invalid audit input fails closed on the old config.

**Verification:** existing reload suites plus concurrent write/debounce,
generation supersession, invalid reload, and affected-key mapping tests.

**Dependencies:** P0.3; may proceed before durable store feed.

**Likely files:** `cmd/gc/config_generation.go`, tests,
`cmd/gc/city_runtime.go`, `cmd/gc/api_state.go`.

**Estimated scope:** M; split parse/publication and key mapping.

**Rollback:** Existing reload/tick remains behind migration mode.

### P4.8 Add feed telemetry and incident commands

**Goal:** Make cursor lag and repair visible before action ownership depends on
it.

**Acceptance criteria:** metrics cover source head/applied cursor where the
backend exposes head, age lag, reconnect, gap, relist duration, dropped/duplicate
changes, and synced state; traces correlate changes to enqueued keys; `gc doctor`
reports unsupported/degraded feeds without dumping opaque cursor internals.

**Verification:** metric reader tests, low-cardinality lint, doctor/status tests,
and simulated gap incident workflow.

**Dependencies:** P4.6.

**Likely files:** watch-manager metrics, doctor gate, trace types/tests.

**Estimated scope:** M.

**Rollback:** Observability-only.

### P4.9 Define the optional runtime observation-change capability

**Goal:** Give runtime readiness, death, replacement, activity, and provider
health a prompt typed ingress path without making every provider implement a
watch.

**Contract:** provider-scoped observation batches with stable session identity,
tri-state liveness, opaque incarnation, freshness/`runtime_observation_revision`, activity
where supported, exact-pane human attachment/copy-mode state, controller-self-
echo attribution, typed gap/resync, watcher shutdown, and explicit unsupported
capability. A watch event is a current-state hint, never proof by itself for a
destructive action. Key-injection families use the explicit interaction-policy
table in `RC-OBS-006`; attachment is never silently discarded as generic
activity.

**Acceptance criteria:** provider watches and census snapshots normalize through
the P1 types; partial/stale/error observations remain `Unknown`; replacement
under one name changes incarnation; slow consumers gap/rebuild rather than drop
silently; provider-local details stay inside `internal/runtime`. Tmux attach/
detach and copy-mode transitions wake the exact key; an unattached stale copy-
mode pane uses bounded grace and a server-serialized conditional cancel that
atomically verifies still-unattached/exact-pane/exact-launch/still-in-mode,
followed by re-observation rather than an infinite park; injected activity is
correlated by operation/pane/native-entry and cannot masquerade as agent output
or perpetually reset idle/quiescence (`RC-OBS-006..007`).

**Verification:** shared fake contract for duplicate, reorder, disconnect, gap,
late old incarnation, partial census, and shutdown; provider-specific
conformance builds on P1.8.

**Dependencies:** G1, P0.8.

**Likely files:** optional `internal/runtime` observation-source types and
`runtimetest` conformance.

**Estimated scope:** M.

**Rollback:** Optional capability; bounded census remains.

### P4.10 Build the runtime observation manager and adapters

**Goal:** Maintain an immutable per-provider runtime projection and mark only
affected session keys when runtime reality changes.

**Changes:** Supervise provider watches where supported; otherwise run a
bounded, jittered, single-flight census at the provider's declared recovery
interval. Apply complete observation batches through a serialized immutable
projection, map deltas to stable session/fanout keys, track freshness/gaps, and
reserve observation capacity outside provider-effect permits. Implement providers
one at a time; tmux/subprocess/fake are the minimum certification set, with T3
and Kubernetes adapters staying behind their provider packages. The shared
session executor reports each key injection to this manager with operation ID,
pane identity, and native-entry time so the current package-local poke registry
can be retired only after self-echo parity passes.

**Acceptance criteria:** one provider failure cannot stale another; observation
never performs a lifecycle effect; cache readers see one complete generation;
provider watch/census gaps trigger scoped rebuild; activity/health changes wake
only relevant keys; no polling work runs in `CityRuntime` or sleeps an action
worker.

**Verification:** background watch and bounded-census suites, false negative,
out-of-band start/stop/replacement, freshness expiry, disconnect/gap, restart,
provider independence, key mapping, leak, and telemetry tests.

**Dependencies:** P4.0, P4.9, and P0.8 stable key/fanout contracts. P4.6 and
this task are independent consumers of P4.0.

**Likely files:** `cmd/gc/runtime_observation_manager.go`, tests, and one adapter/
test pair per provider PR.

**Estimated scope:** Epic, decomposed into manager plus M provider slices.

**Rollback:** Disable watch adapter and use the bounded observer/audit profile.

### Checkpoint G4A — Local keyed capture is safe

- One production store/controller family publishes immutable old-or-new
  projection generations from same-process post-commit application.
- Missed hints, cache loss, external writes, and process restart converge through
  a numerically bounded authoritative relist/audit; no external-write low-
  latency claim is made.
- Partial/failed authority marks the source unsynced and blocks dependent unsafe
  effects.
- Runtime changes for the first vertical family use the P1 typed observation
  path plus a bounded observer; runtime watch is optional.

G4A is sufficient for the first local single-owner keyed vertical canary.

### Checkpoint G4B — Feed-certified change capture is proven

- Snapshot/watch conformance passes for each feed-certified production store.
- Same-process and external writes both reach the serialized applier.
- Disconnect, poison, gap/relist, and process restart lose no committed mutation
  and cannot create a treadmill.
- Unsupported stores remain explicitly audit-only.
- Feed telemetry is live before a feed-dependent cutover or external-write SLO.
- Runtime watch conformance is required only for profiles claiming its latency;
  bounded observation remains a valid weaker profile.

G4B is required before removing the corresponding full discovery path or
advertising feed-certified external-write latency; it is not a prerequisite for
the first local keyed cutover.

## 18. Phase 5 — Introduce keyed scheduling and the session executor

Build only the scheduler mechanics needed by the next vertical family. The
standard `client-go` workqueue, one domain controller loop, a shared per-session
executor, and provider semaphores are the starting point. Lanes, borrowing, and
cross-flow fairness are added only when measured starvation or a second family
requires them.

### P5.1N Map only the first nudge family's actionable sources

**Goal:** Make the first vertical slice explicit without pulling the full
runtime-observation/all-domain mapper program onto its critical path.

**Sources:** command create/retry/terminal write in the chosen command store;
stable target session `intent_generation`/`continuation_identity`; exact launch identity
and provider/ACP attachment readiness needed by that nudge mode; quiescence/
wait-idle transition; deadline/retry; and provider result. Unsupported/broad
runtime change enqueues the one affected session scope through G4A's bounded
observer/relist. It does not wait for unrelated pool/order/config sources or all
providers in P4.10.

**Acceptance criteria:** every listed source maps deterministically to exactly
the stable target key or a reconstructable scoped fallback; a missing listed
source fails the family inventory; one command/session change is O(1); queue
loss reconstructs pending commands and actionable waits.

**Verification:** one table per source, missing-mapper negative fixture,
external/audit-only rediscovery, runtime-readiness change, and operation counts.

**Dependencies:** P0.8, G4A for the command store, and only the provider-specific
runtime observation slice required by the canary.

**Estimated scope:** S/M.

**Rollback:** Additive mapper remains shadow-only.

### P5.1 Complete source-to-key mapping and fanout

**Goal:** Implement the P0.8 key contract for the first vertical family's
current store, config, runtime, deadline, and result sources. Each later family
extends the mapper before its own cutover; no all-system horizontal registry is
required first.

**Acceptance criteria:** every source relevant to the canaried family has a deterministic
affected-key mapper; exact mapping stays inside the serialized applier's bounded
budget; large/broad mapping emits a paged scoped-fanout key processed outside
the applier; unclassified changes choose a safe scoped fallback and increment a
metric; pending fanout age and mapper duration are visible; no cursor can forget
unfinished fanout once its reconstructable marker is recorded.

**Verification:** round-trip, collision, rename, split-store, and mapping tables.

**Dependencies:** P0.8, P4.7, P4.10.

**Likely files:** domain-local mapper registry/fanout worker and focused tests;
key type remains owned by P0.8.

**Estimated scope:** S/M per controller/source family.

**Rollback:** Additive.

### P5.2 Add typed stingy workqueues with metrics

**Goal:** Use `client-go/util/workqueue` for dirty/processing, delay, and retry
semantics where it fits, without building a general reconcile framework or
assuming multiple queues preserve same-key exclusion.

**Contract:** use one standard typed rate-limiting workqueue per logical
controller; its built-in dirty/processing set owns coalescing. Add only the
minimal per-key retry gate needed to distinguish dirty from runnable:
`{retryClass, intentGeneration, blockerRevision/providerHealthEpoch,
nextEligible}`. There is no second custom ownership queue or lane scheduler.
`workqueue.Len()` is informational, not durability/admission truth.
Condition-park registration lives in the same per-key scheduling record as
dirty/retry state. A newer relevant Add atomically clears a condition park and
re-admits evaluation; it cannot race a separate blocker map and disappear.

**Acceptance criteria:** duplicate add coalesces; adds during one processing
interval yield one replay; new intent or the exact blocker-recovery revision
clears obsolete delay without duplicating ownership; irrelevant duplicates do
not hot-loop past backoff; panic/error/cancel always calls `Done`; success calls
`Forget`; different keys can run concurrently; reconstruction from durable
truth/scoped fallback recovers queue loss; condition-parks follow RC-QUEUE-004;
metrics use bounded labels.

**Verification:** the `RC-QUEUE-001..004` model with channel barriers and manual
time: Add-before-Get, Add-during-processing, repeated Add, Done/Forget/retry
races, exact retry promotion, irrelevant duplicates, timer supersession,
shutdown/restart, add/audit-dirty exactly while condition park installs, fresh
intent behind a park, hot-key progress, panic, and race detector.

**Dependencies:** P0.8 and the manual-time test support. A production family
adds its own P5.1/P5.1N mapper before shadow/canary; queue mechanics do not wait
for it.

**Likely files:** one domain controller queue/retry adapter and tests; extract a
shared wrapper only after a second controller proves identical mechanics.

**Estimated scope:** M.

**Rollback:** No production consumers yet.

### P5.3 Run logical controllers as supervised child loops

**Goal:** Remove queue service from the monolithic `select` without losing
controller lifecycle management.

**Acceptance criteria:** each child has start/sync/run/shutdown state; one child
panic cancels or restarts according to explicit policy without silently stopping
others; shutdown stops admission, waits/cancels bounded work, and leaves durable
intent recoverable; repeated panic/failure uses bounded backoff, exposes
unready/unhealthy state, and eventually fails the affected child closed without
silently dying; shutdown during restart backoff is prompt; `CityRuntime` remains
the composition owner.

**Verification:** startup barrier, panic, cancellation, shutdown, and partial
child failure tests, including a panic loop and supervisor restart.

**Dependencies:** P5.2.

**Likely files:** `cmd/gc/reconcile_supervisor.go`, tests,
`cmd/gc/city_runtime.go`.

**Estimated scope:** M.

**Rollback:** Child loops disabled under legacy mode.

### P5.4 Build the shared per-session mutation executor

**Goal:** Serialize lifecycle and nudge provider mutations for one session while
allowing different sessions to progress.

**Changes:** The executor accepts a key plus current action reference, reserves a
minimal fixed global/provider nonblocking permit, rereads current `intent_generation`/
incarnation, installs its expectation, enters `worker.Handle`,
records acknowledgement/ambiguity, and releases ownership. It never decides the
action from a queued payload.

**Acceptance criteria:** one provider mutation per session key; cross-key
concurrency; stale action no-op before provider; context timeout cannot leak key
ownership or permit accounting; a non-cooperative timed-out call can never
overlap a replacement call for the same key; all outcomes requeue, remain
explicitly ambiguous, or terminalize. On waiter detachment, the worker/global
admission permit is released, but the exact key and per-provider actual-call
charge remain held until actual return/fence/resolution. The call-owner path
must enqueue on that transition. Sum of provider caps bounds surviving call
goroutines even if every provider hangs; one provider's charges cannot consume
another provider's capacity.

**Verification:** barriers prove same-key exclusion and cross-key concurrency;
generation races, panic, timeout, provider replacement, and a provider that
ignores cancellation then mutates after caller timeout.

**Dependencies:** G1, P0.11A, P3.3, P5.2, P5.3. P5.5 may add measured fairness policy later;
the executor's basic provider permit does not depend on it.

**Likely files:** `cmd/gc/session_action_executor.go`, tests,
`internal/worker` effect context.

**Estimated scope:** M.

**Rollback:** Executor remains shadow/unused.

### P5.4A Bridge every legacy and keyed session effect through one executor

**Goal:** Prevent months of cross-family migration from becoming two
uncoordinated provider writers for the same session.

**Changes:** Before the first P6/P7 provider canary, make every legacy and keyed
session mutation acquire the same per-session cross-owner ownership at P5.4's
effect boundary, one effect family at a time. Each bridge adapter retains a
compiled cold-unbridge path back to its characterized legacy direct call; that
path can be selected only after P0.11 drains/fences the bridge and restarts the
sole owner, never as a live bypass. The bridge covers start, stop, close,
relaunch, prompt, nudge, interrupt/stop-turn, drain interaction, direct CLI
one-shot execution, and provider swap. A per-instance `worker.Handle` mutex or
process-local tmux lock does not satisfy it. Each keyed family continues writing every durable
in-flight/lease marker that any still-legacy decider reads until that reader is
deleted.

**Acceptance criteria:** keyed start × legacy close, keyed nudge × legacy
relaunch, CLI interrupt × controller stop, and provider swap × pending start
never overlap native entry for one stable session. Different sessions still
progress. The generated legacy read-site inventory covers pending-create claim,
`drain_operation_id`, prompt/nudge attempts, expectations, and every later marker;
no family canary removes or changes a marker while a legacy reader remains.
Before bridging the second family, the first family completes a canary soak in
bridged-but-legacy-decided mode, a mutation-stall-age abort test, and a full cold
unbridge drill (`RC-MIG-004`).

**Verification:** deterministic cross-owner barriers, process/re-exec cases,
legacy-read inventory guard, race detector, and actual-call ambiguity after one
owner process dies.

**Dependencies:** P3.3–P3.4, P5.4, P2.10A, and P0.11 cold ownership.

**Estimated scope:** Epic; one effect-family adapter per S/M slice, with the
bridge gate complete before P6.6.

**Rollback:** Cold-select the compiled legacy adapter only after actual calls
drain/fence and the old bridged process exits. The kill switch is drilled per
family and never drops the shared executor while another process may enter.

### P5.5 Add measured fairness only when simple queues starve

**Goal:** Protect fresh interactive work from retries, noisy rigs, and slow
providers only after the minimal one-queue-per-controller/provider-semaphore
design demonstrates a service-lag violation or multiple families contend.

**Changes:** First measure separate controller queues plus provider semaphores.
If PERF-FAIRNESS misses its numeric bound, add the smallest demonstrated
mechanism—usually one reserved fresh permit or per-provider flow cap. Borrowing,
round-robin, weights, and aging require separate evidence and are never a
preemptive general scheduler. Strict priority alone is forbidden.

**Acceptance criteria:** configured/provider maximum is never exceeded; blocked
provider work does not occupy all controller workers; fresh work and correctness
relist/audit retain measured capacity; every introduced fairness rule fixes a
reproduced bound with no starvation regression; no per-session metric labels.

**Verification:** deterministic manual-time operation-count stress with the
measured adversarial arrival that justified the mechanism, overload, maximum
service lag, and a deliberately hung provider.

**Dependencies:** P5.4.

**Likely files:** `cmd/gc/reconcile_admission.go`, tests,
`cmd/gc/session_action_executor.go`.

**Estimated scope:** Optional S/M per proven bottleneck.

**Rollback:** Conservative one-worker bounds preserve correctness.

### P5.5A Add a bounded per-provider ambiguity circuit and recovery surface

**Goal:** Keep retained actual-call charges from silently wedging a provider or
the whole city.

**Changes:** Each provider pool tracks configured cap, charged actual calls,
oldest charge age, operation/target IDs, and resolution capability. A mechanical
configured count/age threshold opens the provider circuit, refuses new native
entries, and schedules typed health/operation probes; unrelated providers keep
running. Actual return/fence/proven absence releases the charge and may close
the circuit. Expose live inspection and a guarded operator resolve/reset command
that requires exact operation evidence; it never blindly drops a charge or
declares an effect stopped.

**Acceptance criteria:** PERF-HUNG bounds goroutines/FDs/charges; repeated caller
timeouts cannot exceed the provider cap; circuit open/close transitions are
typed and generation-aware; controller restart reconstructs unresolved charges
from durable operations and provider capability; operator output names why a
charge is safe or unsafe to resolve. No Go heuristic judges whether customer
work is “stuck”—only configured mechanical thresholds control admission.

**Verification:** manual clock, permanently/temporarily hung providers, late
return after circuit open, restart, exact/ambiguous operator resolution, and
independent-provider progress.

**Dependencies:** P1.10, P5.4, P5.6, and provider survival/resolution profiles.

**Likely files:** provider admission/circuit state beside the session executor,
typed status adapter, and focused tests.

**Estimated scope:** M.

**Rollback:** Conservative provider cap remains; circuit/status can be disabled
only before provider-effect canary.

### P5.6 Make retry and deadlines generation-aware

**Goal:** Prevent obsolete exponential backoff from delaying fresh intent.

**Acceptance criteria:** retry state carries class, intent generation, and exact
`blocker_revision`/`provider_health_revision`; new intent or that exact blocker recovery
resets/forgets the old limiter and enqueues immediately; irrelevant status/
runtime/feed duplicates may mark dirty but cannot bypass provider-error delay;
dependency/capacity blocks do not consume provider-error budget; workers never
sleep; delay callbacks validate the full cause when firing. Supersession resets
only obsolete provider-error retry state. Durable quarantine, authorization,
human-interaction, and operator holds clear only through their own revision/TTL/
reset contract. Writers use a semantic diff gate so pool/config churn cannot
mint no-op `intent_generation`s that erase retry evidence.

**Verification:** manual-time tests for supersession, cap, jitter bounds,
cancellation, 10,000 irrelevant duplicates causing one provider entry, exact
health/blocker recovery, permanent panic rate bound, and other-key progress.

**Dependencies:** P5.2. Any evidence-triggered fairness budget integrates with
P5.5 only when enabled.

**Likely files:** `cmd/gc/reconcile_retry.go`, tests.

**Estimated scope:** S.

**Rollback:** Default to capped standard rate limiter.

### P5.7 Add generation-scoped expectations

**Goal:** Avoid duplicate effects while provider/store observation lags.

**Acceptance criteria:** expectation is recorded before provider entry, anchored
by the durable action intent; matching observation satisfies it; new
generation supersedes it; expiry performs targeted probe; missed observation
cannot wait forever; a new `intent_generation` does not bypass an actually running old
provider call; expectation state is disposable and reconstructable while
provider operation identity/fencing supplies the durable ambiguity boundary. A
definite pre-entry/provider failure clears the expectation; acceptance or any
ambiguous completion retains it. Reconcile also respects P4.3 write watermarks.

**Verification:** lost observation, late observation, response loss, expiry,
generation replacement, and restart-with-empty-expectations tests.

**Dependencies:** P1.4, P5.4.

**Likely files:** `cmd/gc/session_expectations.go`, tests,
`cmd/gc/session_action_executor.go`.

**Estimated scope:** M.

**Rollback:** Targeted probe before each retry is slower but safe.

### P5.8 Run a no-effect keyed shadow for each concrete family

**Goal:** Prove change-to-key-to-plan scheduling without invoking the executor.

**Acceptance criteria:** the concrete family's store/config/runtime changes
enqueue the correct key;
queue coalescing does not lose latest plan; fleet size does not change operation
count for one key; full scoped fallback is visible; legacy writer remains sole
owner.

**Verification:** component tests, corpus playback through fake feeds, race
detector, and production shadow metrics.

**Dependencies:** Family-specific G3 evidence, G4A, P5.3, P5.6, and that
family's shadow-only controller implementation (P6.3 for the first nudge
family). G4B is needed only if this shadow claims feed-certified external-write
latency.

**Likely files:** the family controller/shadow adapter and tests plus
`cmd/gc/city_runtime.go`; no generic shadow framework is required.

**Estimated scope:** M.

**Rollback:** Migration mode legacy stops the child.

### P5.9 Exhaust the queue/executor crash matrix

**Goal:** Reconstruct the controller at every boundary with empty in-memory
state and prove convergence.

**Boundaries:** Register enforceable provider/process/authoritative-store seams:
intent commit/local apply, provider entry/return, outcome/status commit, and
dedup marker. Queue/cache internals use deterministic model and empty-memory
reconstruction laws rather than a second semantic crash ledger. Feed/status/
handoff/pool/audit add seam checkpoints only when their owning slice exists.

**Acceptance criteria:** no lost durable intent/command; no stale-generation
provider call; no leaked key ownership; queue rebuild reaches quiescence.

**Verification:** reference model, fault injection, and re-exec/SIGKILL harness
using an explicit test city/tmux socket.

**Dependencies:** P5.4–P5.8.

**Likely files:** `cmd/gc/session_action_executor_crash_test.go`, test helper,
integration shard.

**Estimated scope:** Epic, decomposed into one M slice per named matrix.

**Rollback:** Test-only.

### P5.10 Ship the per-key explanation surface before canary

**Goal:** Let an operator answer “why has this action not started?” without
source archaeology or stale files.

**Changes:** Project live object-model/controller state through a typed status
API and CLI/`gc trace` view: current action-family owner/mode; stable key;
T0–T11 timeline and ready proof; queue dirty/processing/parked state; exact
blocker/revision and blocked duration; retry cause/eligibility; expectation and
durable operation; write watermark/source sync; provider permit/ambiguity
charge; last observation scope/incarnations; and next mechanical wake source.
High-cardinality IDs stay in the requested record, never metric labels.

**Acceptance criteria:** every ready key over 1s renders exactly one typed
dependency/capacity/durability/retry/recovery/overload/unknown classification;
unknown or contradictory state is visible and blocks promotion. Output queries
live state, never PID/status files, and does not mutate or wake the key. Human
and JSON forms are golden-tested and bounded/scrubbed.

**Verification:** one fixture per classification, blocked/ambiguous/watermark/
unsynced cases, missing controller, N/N-1 additive wire, Huma/OpenAPI/dashboard
gates if exposed there, and an operator incident drill.

**Dependencies:** P0.3/P0.4, P5.2–P5.7, and current trace/status object model.

**Estimated scope:** M typed object/CLI view; API/dashboard are separate slices
only if needed for the first canary.

**Rollback:** Keep `gc trace`; no owner canary proceeds without an equivalent
live explanation surface.

### P5.11 Prove composed blocked-key liveness

**Goal:** Prevent individually correct parks, ambiguity charges, watermarks,
retry delays, authorization/interaction holds, and shutdown admission from
forming a treadmill with no admissible wake.

**Changes:** Define one composed blocked-key state machine over every mechanism
introduced through P5 plus authorization, human-interaction, and shutdown
holds. Enumerate legal combinations and the mandatory wake edge/release order
for each controller-owned blocker and the admissible change-detection path for
each external blocker; the mechanism that owns a hold must never suppress the observation or
probe required to clear it. Extend P5.10 with per-class age for classified as
well as unclassified blocked keys.

**Acceptance criteria:** `RC-QUEUE-005` and `INV-34` pass in the merge tier.
Property/model exploration finds no reachable nonterminal composite state
without an admissible wake path. Controller-owned resolution is bounded;
external human/provider/policy blockers may persist but have bounded detection
and escalation. A provider circuit cannot block its resolution
probe; a write watermark cannot block its authoritative reread; shutdown cannot
hold a resource needed to drain itself. Every blocked class has a numeric max-
age alarm and canary abort.

**Verification:** deterministic manual-time model/property tests, pairwise and
all-mechanism histories, seeded known-deadlock fixtures, restart with empty
ephemeral state, and explanation-surface goldens.

**Dependencies:** P5.4–P5.10 and the P9.5A admission contract for final shutdown
combinations. The pre-shutdown subset gates the first family canary; the full
model gates G9.

**Likely files:** test-owned composed model plus typed blocked-state/explanation
values; no generic production workflow engine.

**Estimated scope:** M test/model slice plus small production wake fixes found.

**Rollback:** Block canary; do not remove individual safety holds.

### Checkpoint G5A — One vertical family is ready for canary

- One-key changes perform constant scheduling work.
- Same-key single-flight and cross-key concurrency are proven.
- Minimal retry gates, expectations, actual-call ownership, panic, shutdown, and
  rebuild tests pass for the first family; per-provider retained-charge circuit
  and recovery visibility are live before a non-cooperative provider canary.
- Production shadow shows no lost keys or unexplained plans.
- P5.10 explains every old/blocked key and current owner from live state.
- The pre-shutdown P5.11 composed-state model has no wake-free reachable state;
  classified blocked-key age remains within its profile.
- Legacy remains the only provider-effect owner.

G5A plus G4A and the family's G1/G3 evidence may enter Phase 6; it does not wait
for every source mapper, a durable external feed, or speculative fairness lanes.

### Checkpoint G5B — Shared scheduling capacity is certified

- Each added controller family has complete source mapping and reconstruction.
- Measured cross-family/provider contention either meets its service-lag bound
  with separate queues/semaphores or has the minimal proven P5.5 reservation.
- Feed/relist/audit correctness capacity remains independent of effect traffic.
- Full admitted-load, hot retry, and hung-provider profiles pass.

G5B gates broader lifecycle/pool cutover and removal of global discovery, not
the first bounded local nudge canary.

## 19. Phase 6 — First vertical cutover: nudge delivery

Nudge is first because it already has durable IDs and a wake socket, has clear
edge-command semantics, and currently suffers an obvious all-session scan.

### P6.1 Normalize the durable nudge command contract

**Goal:** Make `internal/nudgequeue` the sole durable command ledger and owner of
identity, lease, ordering, retry, expiry, target `intent_generation`/launch
identity, and
terminal outcome.

**Ownership:** Session/API/CLI code submits through `internal/nudgequeue`; the
keyed controller claims and the worker executes through it. No session-owned,
worker-owned, file-sidecar, or provider-local second command ledger is allowed.
When the managed family is enabled, foreground `nudge`/`respond`/interaction
paths never call a provider directly; they durably submit and send a wake hint.
Unmanaged execution must hold the cross-process one-shot claim from P2.11 and
uses the same executor—today's process-local tmux nudge mutex is insufficient.

**Acceptance criteria:** every accepted command has a unique stable ID and one of
pending/in_flight/delivered/injected_unconfirmed/delivery_unknown/expired/
superseded/dead_lettered/upgrade_required;
`delivery_unknown` is distinct from delivered for a provider whose ambiguous
response cannot be deduplicated or safely retried; process-local queue state is
not authoritative; old file compatibility is migrated or read through one front
door; no silent deletion. The total versioned decoder mechanically dead-letters
malformed JSON, invalid known-version state/time/target, and oversized payload.
A well-formed unsupported newer version/state is preserved byte-for-byte and
parked `upgrade_required` without claim/normalization until a compatible owner
arrives; only its required ordering domain blocks. Store/partial
read uncertainty marks the source unsynced and does not dead-letter. Immediate
commands bind the current launch; queued/wait-idle commands persist session ID,
`continuation_identity`, and target policy, then bind once at claim. Preserve ACP
routing from `SESSION-RUNTIME-003`. The storage tier is explicit: command beads
are durable or no-history with proven feed/relist visibility and are exempt from
TTL/GC until terminal. Wisp/ephemeral storage is forbidden unless its command
conservation, feed visibility, and nonterminal GC exemption independently pass.
Every command also carries trusted requester provenance/authorization reference
and its accepted `restore_epoch`; unknown/unauthorized requesters are denied and
pre-restore commands never claim after re-anchor (`RC-AUTH-001..003`,
`RC-STORE-001..003`).

Provider delivery profiles are fixed before canary. Tmux is non-deduplicating,
multi-stage paste→submit: the durable may-enter attempt precedes paste; definite
pre-paste failures may retry; any crash/error after possible paste becomes
`delivery_unknown` and never blind re-pastes. T3/ACP and other providers claim
deduplication only through P6.4 conformance. G6 names the exact provider profile
instead of treating discovery as a later optimization.
Raw tmux interaction obeys `RC-OBS-006..007`: queue/wait-idle parks on human
attachment/copy mode, immediate injection returns a typed conflict unless the
same operation has an explicit force policy, and controller activity attribution
never claims more precision than the provider exposes.

**Verification:** state transition tables, persistence/restart, duplicate ID,
lease expiry, and conservation property tests.

**Dependencies:** P0.5, P0.11–P0.15, P2.11's trusted front-door/claim subset,
and current `internal/nudgequeue` characterization; no lifecycle-decider
checkpoint is required.

**Likely files:** `internal/nudgequeue` canonical front door,
`cmd/gc/nudge_beads.go`, session submit adapter, and focused tests split to the
five-hand-edited-file limit.

**Estimated scope:** M.

**Rollback:** Additive command fields remain readable by legacy dispatcher.

### P6.2 Build the target-session nudge index

**Goal:** Map a command change directly to its target without scanning every
open session.

**Acceptance criteria:** pending or in-flight commands index by canonical session
ID and ordered sequence; target rename/replacement updates or supersedes
correctly; incremental index equals an independent full rebuild; tombstones
remove entries. The independent builder is a production-capable read-only audit
used by the nudge canary and later scheduled by P10; a completed audit watermark,
not merely a zero repair counter, gates promotion.

**Verification:** generated mutation histories, stale/duplicate/gap events,
corruption plus anti-entropy repair, and split-store cases.

**Dependencies:** G4A, P6.1. A feed-certified profile additionally requires
G4B; the first local canary uses post-commit hints plus bounded full rebuild.

**Likely files:** `cmd/gc/nudge_index.go`, tests, cache change mapper.

**Estimated scope:** M.

**Rollback:** Legacy scan remains available.

### P6.3 Implement the keyed nudge controller

**Goal:** Reconcile one target session and drain a bounded ordered command batch.

**Acceptance criteria:** queue wakeups coalesce but commands do not; batch count
or time slice is bounded and requeues if work remains; lifecycle mutation shares
P5 executor ownership; a stopped/replaced target defers or terminalizes according
to explicit command semantics; no full session snapshot scan.

**Verification:** multiple commands/one key, many keys, batch continuation,
lifecycle race, provider busy, and constant-operation tests.

**Dependencies:** P5.1N, P5.2–P5.4, P5.6, and P6.2. This task lands
shadow-only and supplies the concrete production shadow needed by P5.8/G5A;
G5A gates P6.6 canary, not this implementation. The all-system source map,
fairness scheduler, G4B feed, and full lifecycle shadow are not prerequisites.

**Likely files:** `cmd/gc/nudge_key_controller.go`, tests,
`cmd/gc/session_action_executor.go`.

**Estimated scope:** M.

**Rollback:** Shadow-only first.

### P6.4 Add provider command-id deduplication capability

**Goal:** Improve delivery from bounded at-least-once to deduplicated acceptance
where a provider can support it.

**Acceptance criteria:** optional provider operation accepts command ID and
returns accepted/duplicate/unsupported; T3/Kubernetes/other capable providers
persist or delegate dedup; unsupported providers retain explicit delivery
contract; no generic claim of exactly-once effect.

**Verification:** provider conformance, response-loss retry, duplicate command,
and restart tests.

**Dependencies:** P6.1. This task is profile-specific and does not block the
bounded weaker-delivery G6 profile.

**Likely files:** `internal/runtime/runtime.go`, `internal/worker/handle_interaction.go`,
one provider implementation/test per PR.

**Estimated scope:** M per provider.

**Rollback:** Fallback to the explicitly certified weaker contract. A legacy
tmux path without dedup cannot claim retryable at-least-once after possible
injection; it reports `injected_unconfirmed`/`delivery_unknown`.

### P6.5 Make nudge acknowledgement marker-last and durable

**Goal:** Ensure command outcome cannot claim stronger delivery than provider
evidence proves.

**Acceptance criteria:** claim precedes provider call; provider acceptance is
persisted at the exact quality proven by that provider profile. `delivered`
requires delivery proof; exact-launch injection without consumption proof is
`injected_unconfirmed`; a duplicate lookup inherits the original command's
proven quality and is not automatically delivered. Provider results distinguish definite pre-entry,
entered/may-have-entered, accepted/duplicate, target missing, and exact observed
outcome. Tmux inject stages (buffer load, paste, submit, wake, confirmation) are
faulted individually; any unresolved post-entry stage becomes
`injected_unconfirmed`/`delivery_unknown` and never blindly re-pastes. Ambiguous response
retries only under provider command-ID dedup; event follows durable terminal
commit; terminal persistence failure does not lose the command. The nudge-owned
command writer uses P2.0 conditional capabilities or its own torn-detectable
protocol; it does not depend on the lifecycle status writer.

**Verification:** failure at each claim/call/ack/event boundary, partial store
write, and process restart.

**Dependencies:** P2.0, P0.12, P6.3.

**Likely files:** nudge command front door, keyed controller, focused tests.

**Estimated scope:** M.

**Rollback:** Legacy ack path remains behind owner gate.

### P6.5A Fence and drain legacy poller ownership before canary

**Goal:** Prove the keyed controller is the only possible delivery owner before
it claims a command.

**Changes:** Stop new sidecar spawns from both CLI and `internal/session` call
paths; discover existing pollers by live process/cmdline identity, not PID-file
truth; stop/drain/fence them; cold-stop the legacy controller/poller mode and
start one controller process in keyed mode; make old PID/lock files
informational garbage only. Pending commands remain in the canonical front door.

**Acceptance criteria:** a crash at every spawn-disable/drain/old-process-exit/
new-process-start step
leaves at most one claimer; an ambiguous old poller keeps the system observe-only
rather than enabling keyed delivery; stale files cannot block or enable a
poller; old binary compatibility follows P0.12.

**Verification:** P0.10/P0.11 handoff matrix, live-process identity, killed/
paused old poller, pending lease, and restart tests.

**Dependencies:** P6.1, P6.3, P6.5, P0.11.

**Likely files:** rollout owner adapter and both poller-start call sites plus
focused process tests; split call-path disable and handoff if needed.

**Estimated scope:** Epic, decomposed into M slices.

**Rollback:** Re-enable legacy spawn only after keyed ownership is provably
inactive; PID files never regain authority.

### P6.6 Shadow and canary the nudge owner

**Goal:** Flip exactly one action family after parity.

**Gates:** exact command conservation; zero wrong-incarnation deliveries; no
unexplained effect diff; SLO under capacity; at least one completed P6.2 nudge
index audit with zero unexplained drift; bounded queue age/depth; permanent
per-provider declared quality-numerator/accepted ratio and ambiguity-age
thresholds. Tmux reports a separate exact-launch `injected_unconfirmed` ratio;
it never labels that delivered. With a resolvable provider fake,
`delivery_unknown` is unreachable
(`RC-NUDGE-009`).

**Acceptance criteria:** handoff first stops legacy claim/delivery, waits for or
recovers its in-flight leases, then enables keyed ownership; rollback performs
the inverse and never runs two claimers.

**Verification:** single-city canary, forced rollback during in-flight delivery,
and N-1 compatibility test.

**Dependencies:** G4A, G5A, P0.11A, P0.14–P0.15, P3.10, P5.4A, P5.10–P5.11, P6.3, P6.5, P6.5A, and nudge-family P3 effect
completeness/parity. G4B is required only for feed-certified latency; P6.4 only
for deduplicated delivery.

**Likely files:** rollout owner wiring and tests.

**Estimated scope:** M.

**Rollback:** Explicit migration gate/handoff.

### P6.7 Remove the all-session nudge scan from the active path

**Goal:** Delete the latency source only after default-on soak.

**Acceptance criteria:** `dispatchAllQueuedNudges` is unreachable in keyed mode;
the Unix socket feeds the target-key ingress/tailer rather than the main city
loop; patrol does not deliver nudges; legacy code remains one release for
rollback, then is removed with its tests migrated.

**Verification:** static reachability/ownership guard, no-fleet-scan operation
test, production traces, and rollback drill before deletion.

**Dependencies:** P6.6 plus one release of default-on soak.

**Likely files:** `cmd/gc/nudge_dispatcher.go`, `cmd/gc/city_runtime.go`, migrated
tests.

**Estimated scope:** M.

**Rollback:** Before deletion, gate; after deletion, release rollback only.

### P6.8 Eliminate per-session nudge pollers and PID-file coordination

**Goal:** After the keyed owner has soaked, physically delete the already-disabled
sidecar implementation and stale process-state files.

**Changes:**

- Remove the compatibility branches already disabled by P6.5A; both call paths
  remain on the durable command front door plus target-key wakeup.
- Stop reading or writing `.gc/runtime/nudges/pollers/*.pid` and sibling
  `.pid.lock` files. Process liveness is discovered from the live process table
  and command identity only during the compatibility drain.
- Delete duplicate spawn/lease/reap code from `cmd/gc/cmd_nudge.go` and
  `internal/session/submit.go`, then remove the poller command contract after old
  binaries can no longer own delivery. Historical files are ignored or removed
  by a bounded one-time cleanup; they never participate in ownership.
- Keep diagnostic logs in the normal structured trace rather than one log file
  per poller.

**Acceptance criteria:** no production path creates, trusts, or reaps a nudge
poller PID/lock file; one session cannot acquire a second delivery owner after a
crash; deleting all `.gc/runtime/nudges/pollers` files has no behavioral effect;
an AST/path guard prevents reintroduction.

**Verification:** enqueue/wakeup tests from both call paths, controller restart
with pending commands, old-poller compatibility drain, process-table identity
tests, path guard, and an isolated process test proving no sidecar remains.

**Dependencies:** P6.5A, P6.6 default-on soak, P6.7, and the P0.12 deletion
compatibility window.

**Likely files:** split into one M PR for call-path migration and one M deletion
PR covering `cmd/gc/cmd_nudge.go`, `internal/session/submit.go`,
`internal/nudgepoller`, and their focused tests.

**Estimated scope:** Two M slices.

**Rollback:** Before deletion, exclusive owner handoff; after deletion, release
rollback only. Never restore PID files as authoritative state.

### Checkpoint G6 — Nudge is fully keyed

- No nudge waits behind the city tick or scans the session fleet.
- Under finite outage or configured expiry/dead-letter policy, every command
  reaches a visible terminal state, including honest `delivery_unknown` where
  provider ambiguity cannot be resolved.
- Wrong-incarnation and lost-command counts are zero.
- Delivery has no per-session poller, PID file, or process-file reaper.
- Provider limitations are explicit.
- Rollback and restart recovery have been exercised in production canary.

## 20. Phase 7 — Cut session lifecycle over arm by arm

The keyed session controller initially consumes a generation-fenced projection
from the existing desired-state path. Phase 8 replaces that global producer.
This deliberately avoids changing decision, scheduling, and demand derivation in
one cutover.

### P7.1 Build the per-session desired/actual projection

**Goal:** Give one session reconcile a bounded, immutable input view.

**Projection contents:** canonical lifecycle view, `intent_generation` and source
watermarks, relevant config/template facts, runtime tri-state/incarnation,
pending commands/expectations, dependency facts, provider health/capacity, and
legacy demand generation while it remains.

**Acceptance criteria:** one session read performs no fleet/store scan; source
watermarks identify stale/partial facts; object and indexes update atomically;
independent full-snapshot reconstruction matches exactly.

**Verification:** generated delta histories, split stores, stale/duplicate/gap,
and one-key operation-count tests.

**Dependencies:** G4A, G2. Feed-certified lifecycle latency additionally
requires G4B.

**Likely files:** `cmd/gc/session_projection.go`, tests, cache change mapper.

**Estimated scope:** M.

**Rollback:** Shadow projection only.

### P7.2 Map every session actionability source to the stable key

**Goal:** Ensure a session is re-evaluated whenever any relevant fact changes.

**Sources:** session bead desired/status, config generation, runtime observation,
provider health, dependency readiness, work/explicit wake, hold/quarantine/timer,
start/drain expectation, and action result.

**Acceptance criteria:** a source-to-key truth table exists; each row has a unit
test; unknown broad source uses scoped fallback and metric; anti-entropy can
reconstruct all actionable keys from the projection.

**Verification:** mutation-source matrix and missing-mapper negative test.

**Dependencies:** P7.1, P5.1, P4.10.

**Likely files:** `cmd/gc/session_key_mapper.go`, tests.

**Estimated scope:** M.

**Rollback:** Broad scoped enqueue is a safe temporary fallback.

### P7.3 Cut over no-op and status-heal actions

**Goal:** Make the keyed controller the first production owner of a
non-provider, non-destructive action family.

**Acceptance criteria:** converged key performs no write; torn/legacy/expired
metadata heals through P2.9; precondition conflict rereads/requeues; legacy loop
skips the owned heal family; shadow still compares results.

**Verification:** status writer conformance, corpus parity, concurrent external
write, and rollback handoff.

**Dependencies:** family-specific G3, G5B, P7.1–P7.2.

**Likely files:** `cmd/gc/session_key_controller.go`, rollout owner,
`cmd/gc/session_reconciler.go`, tests.

**Estimated scope:** M.

**Rollback:** Stop keyed owner, drain, enable legacy heal.

### P7.4 Cut over identity-heal actions

**Goal:** Eliminate path-dependent identity stamping before moving prompt
delivery or provider start.

**Acceptance criteria:** fresh spawn, adoption, resume, drift, rollback, and
replacement use the P2.7A identity plan; stale `intent_generation` cannot stamp; legacy
identity arms are disabled for owned keys; prompt delivery remains a separately
owned family.

**Verification:** historical #3849/#3872 identity cases, generation race, and
owner-handoff crash boundaries.

**Dependencies:** P7.3, P2.3, P2.5, P2.7A.

**Likely files:** keyed controller, `internal/session` identity plan, focused
tests.

**Estimated scope:** M.

**Rollback:** Owner handoff after in-flight intent recovery.

### P7.4A Extract lifecycle provider preparation/result adapters

**Goal:** Reuse the current bounded-parallel provider-call preparation and
result application before start cutover, without retaining global wave
scheduling.

**Acceptance criteria:** one-session preparation consumes the immutable P7.1
projection and produces executor inputs; result application is generation/
incarnation checked; no mutable whole-pass bead map or completion-order fold is
required; current provider-specific error mapping remains characterized.

**Verification:** extracted-adapter parity, current
`session_lifecycle_parallel` cases, stale result, and isolated provider tests.

**Dependencies:** P5.4, P2.5, P7.1.

**Likely files:** adapter extracted from
`cmd/gc/session_lifecycle_parallel.go`, executor integration, tests.

**Estimated scope:** Epic, split preparation and result application into M
slices.

**Rollback:** Legacy parallel owner continues using compatibility adapters.

### P7.4B Cut over priming/prompt delivery

**Goal:** Activate the bounded two-phase first-start/prompt-delivery protocol as
its own family.

**Acceptance criteria:** attempted is durable before delivery and confirmed only
after provider evidence; duplicate prompt acceptance is bounded/documented;
stale `intent_generation` cannot confirm; first-start/continuation rules come from
P2.7A; legacy prime arm is disabled independently from identity and start.

**Verification:** marker/delivery/response-loss crash matrix, duplicate
acceptance, historical #3872 cases, and rollback handoff.

**Dependencies:** P7.4, P7.4A, P2.7A.

**Likely files:** keyed controller, session prime plan, worker interaction, tests.

**Estimated scope:** M.

**Rollback:** Recover/fence in-flight prompt intent before prior owner enables.

### P7.4C Cut over interrupt and stop-turn interaction commands

**Goal:** Route every remaining direct interactive provider mutation through the
shared per-session executor so P7.14 can prove complete ownership.

**Acceptance criteria:** stop-turn/interrupt and related direct interaction
surfaces have durable operation identity where needed, share same-key exclusion,
revalidate generation/incarnation at provider entry, preserve ACP rerouting from
`SESSION-RUNTIME-003`, and preserve allowed stop-turn behavior from
`SESSION-RUNTIME-004`.

**Verification:** effect inventory, ACP route-before-interaction, pool-managed/
slot-only stop-turn, timeout/late mutation, and owner-handoff tests.

**Dependencies:** P5.4–P5.7 and P3.3–P3.4 completeness.

**Likely files:** worker interaction boundary, keyed interaction adapter, focused
CLI/API/session tests.

**Estimated scope:** M per interaction family.

**Rollback:** Per-family exclusive owner handoff.

### P7.5 Cut over provider start initiation

**Goal:** Enter the provider as soon as current desired state is running and all
blockers/capacity permit.

**Acceptance criteria:** durable intent precedes provider entry; deterministic
session identity, operation identity, and provider conformance ensure repeated
attempts cannot create two live current-generation incarnations in a certified
provider profile; provider calls themselves may repeat after ambiguous outcomes;
a provider slot is reserved before dispatch; dependency and provider health
blockers do not consume retry budget; different session keys start concurrently.

**Verification:** start crash matrix, same-name collision, start→stop→start,
capacity/fairness, hung provider, and current lifecycle parallel regression suite.

**Dependencies:** P5.4–P5.7, P7.4, P7.4A, and P4.10 runtime observation.

**Likely files:** `cmd/gc/session_key_controller.go`,
`cmd/gc/session_action_executor.go`, `internal/session/start_decision.go`, tests.

**Estimated scope:** M.

**Rollback:** Stop new admissions; recover/observe in-flight starts; enable
legacy start owner.

### P7.6 Cut over start acknowledgement and adoption

**Goal:** Separate provider entry, accepted response, and observed readiness.

**Acceptance criteria:** provider return is not treated as readiness unless its
contract proves it; matching runtime observation completes status; response loss
or timeout observes/adopts the exact incarnation; stale completion cannot commit;
`session.woke` fires only after durable confirmation.

**Verification:** delayed visibility, lost response, provider returns success but
runtime missing, late old result, event failure, and restart tests.

**Required sub-slices:** (1) accepted/ambiguous result persistence and (2)
runtime-observation adoption/readiness confirmation. Each flips separately.

**Dependencies:** P7.5, P5.7, P4.10.

**Likely files:** executor, expectations, status writer, focused tests.

**Estimated scope:** Epic, decomposed into two M slices.

**Rollback:** Compatibility commit adapter retained under owner gate.

### P7.7 Cut over drain begin, cancellation, and acknowledgement

**Goal:** Advance one session drain from its own events/deadlines instead of
waiting for subsequent patrol passes.

**Acceptance criteria:** begin writes one generation; agent acknowledgement,
assigned-work return, pending interaction, timeout, and explicit cancel enqueue
the key immediately; stale ack/cancel is ignored; no worker sleeps until a
deadline.

**Verification:** full drain decision table, chaos tests, fake-clock deadlines,
ack during controller restart, and work-return races.

**Required sub-slices:** (1) begin/cancel and (2) acknowledgement/timeout/
completion. Each preserves the same durable `drain_operation_id`.

**Dependencies:** P2.6, P7.3, P5.6.

**Likely files:** `cmd/gc/session_key_controller.go`,
`internal/session/drain_decision.go`, wait/event mapper, tests.

**Estimated scope:** Epic, decomposed into two M slices.

**Rollback:** Handoff preserves durable `drain_operation_id` for legacy continuation.

### P7.7A Install requester fences for self-terminating lifecycle commands

**Goal:** Let a CLI running inside the affected launch report its durable result
before close, suspend, reset/restart, kill, rig restart, or city stop tears that
launch down.

**Changes:** At request commit, bind the caller's exact process/launch identity
and persist a requester fence when the command can terminate it. The effect
owner defers that launch's teardown until one bounded stdout/stderr result-write
attempt completes and the fence is released, or exact process observation proves
the requester exited. The durable fence lease also has a configured restart-
safe expiry: persist `host_boot_id` plus an OS boot-time monotonic deadline.
Same-boot controller restart reconstructs it; changed boot ID clears only after
exact requester/launch absence. Providers without an equivalent clock alarm and
wait for release/exact exit, and wall-clock jumps never release the fence. On a
valid expiry the owner records `requester_result_abandoned`, performs one
controller-owned durable result attempt, revalidates the exact operation/launch,
and releases the fence. It never claims the wedged CLI received output or that
teardown completed. Lost release acknowledgement is recovered by identity.
External callers install no fence. A self-targeting default command returns the
accepted-incomplete exit/result from `RC-CLI-006`; an explicit terminal wait
returns `self_wait_unavailable` without canceling the accepted request. City
stop reuses the same contract in the Phase 9 shutdown owner.
An unmanaged self-targeting CLI first delegates the operation/exclusive claim
to a separately proven live supervisor outside the target launch; without one,
it rejects pre-commit as `self_owner_unavailable` rather than accepting work
whose only owner it will kill.

**Acceptance criteria:** `RC-CLI-006..007`, `RC-CLI-010`, and the requester-
fence combinations in `RC-QUEUE-005` pass for close, suspend, reset, kill, rig
restart, and city stop. EPIPE releases the fence and exits nonzero;
SIGKILL before/after result attempt, stale PID/launch reuse, lost release ack,
wedged live caller through fence expiry, forward/backward wall jumps, host
reboot, and controller restart never cause a blind timeout kill or retarget a
successor. Unmanaged self-owner/no-supervisor has zero durable acceptance and
zero provider entry.

**Verification:** deterministic subprocess/result-writer barriers, empty-memory
restart, isolated tmux self-target tests, and command-specific human/JSON/exit
goldens.

**Dependencies:** P0.13, P2.11–P2.12, P5.4, P5.7, and family-specific G3
effect coverage.

**Estimated scope:** M contract/executor slice, then S per CLI family.

**Rollback:** Keep the new family shadow-only until the legacy owner is proven
to honor the same fence; never canary self-termination through two owners.

### P7.8 Cut over stop and close last

**Goal:** Move destructive lifecycle only after all safety, observation, and
executor infrastructure has soaked.

**Acceptance criteria:** current `DeathWitness` or explicit desired-stop live
witness is mandatory; stop targets exact incarnation; absent matching target is
idempotent success only under the same provider-scope identity and a complete
tagged-process scan; `ErrNoServer`/server replacement alone is
`RuntimeScopeLost`, not absence. Different/replacement incarnation is stale no-op; stop
failure leaves bead open; successful close releases work and commits before
events.

**Verification:** false-negative census, provider error, replacement race,
assigned work release, stop failure, requester self-close, bulk/shutdown, crash
matrix, and real tmux tests. Preserve `SESSION-WORK-001..004`
release/cancel/orphan guarantees and `RC-CLI-010`.

**Required sub-slices:** (1) exact-incarnation stop and (2) durable close/work
release. Stop must soak before close becomes owned.

**Dependencies:** G1, family-specific G3/P3.10, P2.9, P2.11–P2.12, P5.4, P5.7,
P7.6–P7.7A, and the exact-target capability required by the advertised profile.

**Likely files:** keyed controller/executor, `internal/session/close_decision.go`,
worker lifecycle boundary, tests.

**Estimated scope:** Epic, decomposed into M stop and close/release slices.

**Rollback:** New owner stops admission and resolves all in-flight witnesses
before legacy enables.

### P7.9 Move timers to keyed delayed reconciliation

**Goal:** Make hold/quarantine expiry, idle/max-age evaluation, create/drain
deadline, and retry wake exact without full patrol.

**Acceptance criteria:** each deadline registers one generation-scoped delayed
key; earlier fact change wakes immediately; fake-clock behavior is deterministic;
restart reconstructs deadlines from durable state; backward/forward wall-clock
jumps cannot execute obsolete intent.

**Verification:** timer ladder tests, clock jumps, supersession, restart, and
large timer population operation counts. Specify monotonic-duration versus
durable wall-clock semantics; cover timezone/DST irrelevance, large forward and
backward jumps, timer versus generation change, restart immediately before/after
deadline, and mass expiry without starving interactive work.

**Dependencies:** P5.6, P7.7.

**Likely files:** `cmd/gc/session_deadlines.go`, tests,
`internal/session/lifecycle_timers.go`.

**Estimated scope:** M.

**Rollback:** Slow audit/patrol can rediscover durable deadlines.

### P7.10 Cut over config drift, restart, and provider swap

**Goal:** Route each structural change through generations and the same session
executor.

**Acceptance criteria:** config-only live update and restart-required update are
distinct plans; provider swap stops the exact old incarnation before starting
the new target unless make-before-break is explicitly supported; superseding
config generation cancels obsolete work; dependent sessions observe readiness
edges, not sleeps.

**Verification:** drift defer/resume, soft reload, provider swap, restart request,
rollback, and dependency tests.

**Required sub-slices:** live config application, restart-required generation,
and provider swap are separate owner flips.

**Dependencies:** P2.7, P4.7, P7.8.

**Likely files:** session plan/controller, config mapper, worker boundary, tests.

**Estimated scope:** Epic, decomposed into M per live/restart/swap family.

**Rollback:** Config generation persists; legacy re-observes and continues.

### P7.11 Replace dependency polling with reverse-indexed blockers

**Goal:** Wake dependents on actual prerequisite readiness and preserve reverse
stop ordering.

**Acceptance criteria:** dependency graph validates at config load; forward and
reverse indexes update with generation; a blocked dependent registers then
rechecks source revision; prerequisite ready/missing transition enqueues only
dependents; cycle/invalid graph remains a config error; bulk stops reuse stable
reverse-wave helper. Ordering is level-triggered from a durable
`no_live_dependents` blocker for each target, not remembered only in an
in-memory wave; after crash, a target cannot stop while any dependent remains
live/unresolved.

**Verification:** lost-wakeup race, prerequisite dies between observations,
pool-template dependency, transitive subset stop, graph permutation, and crash/
restart after every target in a non-city-wide bulk stop.

**Dependencies:** P4.7, P7.1.

**Likely files:** `cmd/gc/session_dependency_index.go`, tests,
existing lifecycle graph helper.

**Estimated scope:** M.

**Rollback:** Scoped dependency rescan fallback remains until soak.

### P7.12 Adapt the existing bounded-parallel lifecycle machinery

**Goal:** After P7.4A extraction and start cutover, remove the remaining global
wave coupling while preserving bulk-operation dependency ordering.

**Changes:** Route all remaining callers through the P7.4A preparation/result
adapters. Keep dependency-wave construction for explicit bulk operations and
startup catch-up where useful. Remove dependence on mutable whole-tick bead maps
and completion-order commits.

**Acceptance criteria:** current dependency-aware parallel tests remain or move
to equivalent contracts; normal single-key start does not wait for a fleet wave;
bulk operations retain stable dependency order and bounded parallelism.

**Verification:** existing `session_lifecycle_parallel` suite, new one-key
constant-operation tests, and commit-order race tests.

**Dependencies:** P7.5–P7.11.

**Likely files:** `cmd/gc/session_lifecycle_parallel.go`, executor, focused tests;
split into multiple PRs.

**Estimated scope:** Epic; enumerate and decompose into M slices.

**Rollback:** Legacy helper remains callable until lifecycle graduation.

### P7.13 Graduate action families one at a time

**Goal:** Transfer production ownership in the safest dependency order, with one
independently reversible family per handoff.

**Authoritative session-family census/order:**

1. status heal;
2. identity heal/retirement;
3. prime/prompt delivery;
4. interrupt/stop-turn interaction;
5. start initiation;
6. start confirmation/adoption;
7. drain begin/cancel;
8. drain acknowledgement/completion;
9. timers/wake;
10. live config application;
11. restart-required generation;
12. provider swap;
13. stop;
14. close/work release.

P7.10 and P12.1 reference this census; they do not maintain shorter competing
lists. Each numbered row is a distinct owner flip/deletion even when one P7
implementation task supplies shared code.

**Gate per family:** current-head scenario coverage, effect completeness,
fact/plan parity, crash matrix, provider conformance, canary SLO, zero unsafe
divergence, at least one completed P7.1 independent session-projection audit with
zero unexplained drift, P3.10 independent effect attribution, P2.10A/P5.4A
physical-writer/effect serialization, and tested handoff/rollback. Before the
flip, rerun the P0.6 corpus with that family disabled in the legacy pass and
shadow-validate every residual legacy family; this exact mixed configuration is
part of the evidence packet.

**Acceptance criteria:** exactly one writer owns each family; a machine-readable
status reports owner; flip is reversible before deletion; unexpected diff or
unwitnessed attempt automatically aborts canary. The handoff either waits for
the current 142–256s legacy pass/build to exit or uses the P3 effect-boundary
immutable owner instance/mode token to reject every later legacy native entry; it never enables
keyed effects merely because config changed mid-pass. Rollback is reverse-
graduation (LIFO) by default. A non-LIFO target first runs the same residual-
configuration corpus/parity proof for that exact owner set. Every durable
marker still read by a legacy family remains written and N/N-1 compatible.

**Verification:** P0.11 handoff crash matrix, relevant family conformance/parity/
crash suite, production canary evidence packet, and forced automatic abort/
rollback.

**Dependencies:** Corresponding P7 task, G3, P2.10A, P3.10, and P5.4A.

**Likely files:** rollout owner table, controller wiring, tests.

**Estimated scope:** S per flip.

**Rollback:** Per-family handoff protocol.

### P7.14 Disable the legacy session action path

**Goal:** Leave the old pass as a read-only parity/audit producer after every
session family graduates.

**Acceptance criteria:** no legacy provider/status effect is reachable in keyed
mode; static effect ownership proves it; legacy shadow still compares facts and
plans for one release; startup/recovery uses cache sync plus enqueue, not a
special mutation ladder.

**Verification:** reachability/spy tests, process restart with empty queues,
production shadow, rollback drill.

**Dependencies:** P7.13 all families.

**Likely files:** `cmd/gc/session_reconciler.go`,
`cmd/gc/city_runtime.go`, rollout tests.

**Estimated scope:** M.

**Rollback:** Re-enable legacy owner before its later deletion.

### Checkpoint G7 — Session lifecycle is keyed

- Explicit/session-derived actions begin without waiting for patrol.
- One session is serialized; unrelated sessions run concurrently.
- Every action is generation/incarnation fenced and traced T0–T11.
- Dependency, timer, retry, restart, and controller-crash recovery pass.
- Legacy session effects are disabled but retained as shadow for one release.

## 21. Phase 8 — Replace global desired-state and pool scans

This phase removes the remaining large latency source for work-driven starts.

### P8.1 Split `buildDesiredState` into pure projection and enactment

**Goal:** Make it safe to evaluate or replace demand without background side
effects racing config generations.

**Changes:** Separate all hook installation, route registration/canonicalization,
session materialization/stamping, and persistence from pure input-to-desired
projection. Capture immutable `config_generation`/`source_revision` values. Result publication
rejects stale inputs and dirty-during-build schedules one follow-up.

**Acceptance criteria:** pure function has no store/provider/filesystem calls;
every former side effect has a named current owner; unchanged input is
deterministic; stale build cannot publish or enact.

**Verification:** determinism/import guard, side-effect spy, generation race, and
existing desired-state suite.

**Dependencies:** G2, P4.7.

**Likely files:** `cmd/gc/build_desired_state.go`, new projection file/tests,
side-effect owner adapter; split into small PRs.

**Estimated scope:** Epic; enumerate and decompose into M slices.

**Rollback:** Legacy wrapper composes pure projection plus enactment synchronously.

### P8.2 Create keyed configuration-materialization owners

**Goal:** Reconcile hooks, routes, and session specs independently from demand
calculation.

**Acceptance criteria:** each materialized resource has a stable key/generation;
idempotent reapply; config removal cleans only owned artifacts; no provider start
occurs here; DoltLite/T3 details remain behind their boundaries.

**Verification:** current hook/route/session stamp characterization, config
generation races, partial failure, and independent retry tests.

**Dependencies:** P8.1, P5.3.

**Likely files:** one logical materializer/controller and test per resource
family; do not combine all three in one PR.

**Estimated scope:** M per family.

**Rollback:** Synchronous enactment wrapper remains until each owner graduates.

### P8.3 Build the work readiness/routing index

**Goal:** Update affected pool demand from one work-bead delta.

**Index fields:** store/scope, status/readiness, assignee, route/execution route,
template/pool target, dependency-blocked state, and ownership class.

**Acceptance criteria:** incremental index exactly matches a fresh `List`/
`Ready` reference fold for all supported stores; dependency changes update dependents;
partial/unknown projection defers scale-down and triggers repair; one delta maps
only affected pools.

**Verification:** reuse CachingStore generators/differential fixtures, external
write/gap, split-store, dependency, route canonicalization, and scale-from-zero
cases, preserving `SESSION-RECON-002..005`. The fresh reference fold is
production-capable, shares only canonical decoding/contribution semantics, and
is later scheduled by P10.

**Dependencies:** G4A, P8.1; G4B for feed-certified external work-routing
latency or before deleting the corresponding bounded relist.

**Likely files:** `cmd/gc/work_demand_index.go`, tests, cache change mapper.

**Estimated scope:** M.

**Rollback:** Global ready snapshot remains as shadow/reference.

### P8.4 Build the session membership/capacity index

**Goal:** Count pool/template members without scanning every open session per
agent.

**Acceptance criteria:** canonical/historical/creating/active/draining/closed
membership follows current lifecycle projection; each session delta updates only
old/new memberships; pool capacity and min-floor reference computations match;
partial data fails safe against destructive scale-down.

**Verification:** pool replacement, named collapse, mid-transition, alias conflict,
and generated index-vs-scan histories. The fresh reference fold is
production-capable, shares only canonical decoding/contribution semantics, and
is later scheduled by P10.

**Dependencies:** P7.1, P8.3.

**Likely files:** `cmd/gc/pool_membership_index.go`, tests.

**Estimated scope:** M.

**Rollback:** Reference scan remains.

### P8.5 Implement the pure per-pool desired allocation

**Goal:** Replace fleet-global demand inputs with one deterministic `PoolKey`
decision over indexed pool/work facts, outside the session domain.

**Acceptance criteria:** current min/max/floor, cold-scale, dependency-only,
manual slot, suspended, cap, and routing semantics have named table rows; result
is desired count plus pool `intent_generation` only; no session starts or writes; input
order does not affect chosen stable slots.

**Verification:** existing pool/scale tests moved to pure tables, projection
fixpoint, permutation, and historical incidents.

**Dependencies:** P8.3–P8.4.

**Likely files:** existing `cmd/gc/pool_desired_state.go` or a separately
approved pool/demand package, plus tests. Pool policy does not enter
`internal/session`; session plans receive already-derived assignment/wake facts.

**Estimated scope:** M.

**Rollback:** Shadow-only decision.

### P8.6 Add the keyed pool controller and durable pool intent

**Goal:** Reconcile one pool's desired slots; session controller remains the
only provider-start owner.

**Acceptance criteria:** one in-flight reconcile per pool; concurrent work/config
updates coalesce to latest; slot/session materialization uses conditional
reservation where available; duplicate pool controllers cannot over-allocate;
changed desired sessions enqueue their session keys. The pool controller owns
pool `intent_generation` and slot reservation only; it calls the P8.2 session-spec
materializer and never creates/stamps a session through a parallel front door.

The duplicate-controller claim is valid only on one linear working set with the
P2.0 conditional reservation capability. Unsupported stores remain single-owner
and fail closed rather than relying on an in-memory mutex. Whole-bead-emulated
reservations use one dedicated low-write coordination bead per pool/slot domain
and pass `RC-STATE-003` under adjacent-write contention.

**Verification:** concurrent scale-up/down, slot CAS conflict, controller restart,
two pools parallel, same pool serialized, and no direct provider effect. Crash
after slot reservation, session bead creation, allocation-generation commit,
and session-key dirty registration, reconstructing from empty ephemeral state.

**Dependencies:** P5.2–P5.6, P8.2, P8.5, P2.0 conditional-write capability, and
the P0.11 pool-allocation owner protocol.

**Likely files:** `cmd/gc/pool_key_controller.go`, tests,
session materialization front door.

**Estimated scope:** M.

**Rollback:** Legacy demand producer remains owner until cutover.

### P8.7 Handle arbitrary `scale_check` inputs honestly

**Goal:** Bound custom external checks without pretending their external state is
observable.

**Rules:** Work/config/session changes enqueue immediately. An arbitrary check
also receives a configured/derived refresh deadline because Gas City cannot know
when hidden external state changes. Checks run per pool, single-flight, with
deadline, output validation, generation fence, and dedicated capacity.

**Acceptance criteria:** hung/invalid check blocks only that pool; stale result
cannot publish; check errors preserve last known desired state according to
current contract and are visible; no global `WaitGroup` blocks lifecycle.

**Verification:** timeout, supersession, malformed output, external-state timer,
provider saturation, and parallel independent checks.

**Dependencies:** P8.5–P8.6.

**Likely files:** pool controller/check runner/tests.

**Estimated scope:** M.

**Rollback:** Existing synchronous check path behind owner gate.

### P8.8 Index pool blockers and dependency readiness

**Goal:** Remove repeated demand polls for conditions that can emit changes.

**Ownership:** Reuse the dependency graph and reverse index owned by P7.11;
P8 adds pool blocker registrations/consumers and does not construct a second
dependency graph.

**Acceptance criteria:** quota/capacity, dependency template readiness, provider
health, config conflict, and check retry each register by source revision;
unblock enqueues affected pools; registration/recheck closes missed wake; timed
conditions use delay queue.

**Verification:** Nomad-style unblock-before-registration race, multiple blockers,
provider recovery, and fairness tests.

**Dependencies:** P7.11, P8.6.

**Likely files:** pool blocker index/controller tests.

**Estimated scope:** M.

**Rollback:** Per-pool capped retry remains a safe fallback.

### P8.9 Shadow incremental demand against the full builder

**Goal:** Prove indexed pool output before it owns materialization.

**Acceptance criteria:** every evaluated pool compares desired count, slot
identity, blocker reason, cap decision, and `source_revision`; incomplete full
builder cycles cannot count as parity; intentional fail-safe divergence is
reviewed; operation counts prove no hidden scans.

**Verification:** current scale/pool corpus, generated histories, production
shadow, and index corruption/rebuild.

**Dependencies:** P8.3–P8.8, G3.

**Likely files:** demand shadow comparator/reports/tests.

**Estimated scope:** M.

**Rollback:** Shadow-only.

### P8.10 Cut over one pool class/scope at a time

**Goal:** Transfer allocation-generation ownership incrementally without
changing every pool/store topology in one release.

**Order:** fixed/named materialization → default work-derived pools → custom
scale-check pools → split-store/rig pools → dependency pools.

**Gate per class:** zero unexplained diff, exact index parity, no over-allocation,
constant-work proof, action SLO, completed P8.3/P8.4 reference audits with zero
unexplained drift, rollback drill.

**Acceptance criteria:** one producer owns pool `intent_generation`; legacy builder
skips owned pools; session controller consumes both sources through one desired
projection during migration.

**Verification:** per-class owner-handoff crash matrix, shadow/reference report,
allocation conflict tests, canary performance, and forced rollback.

**Dependencies:** P8.9.

**Likely files:** owner routing and tests.

**Estimated scope:** S per flip.

**Rollback:** Stop keyed producer, settle/CAS current pool `intent_generation`, enable legacy.

### P8.11 Remove global demand from the action path

**Goal:** Ensure a routed work commit can start its session without a city-wide
build.

**Acceptance criteria:** all pool classes are keyed; `loadDemandSnapshot` is not
on the provider-start causal path; a one-work change performs bounded index/pool/
session operations; full builder runs read-only only as audit/shadow.

**Verification:** causal trace, no-scan guard, 100/1K/10K/100K operation-count
profiles, direct external `bd` write, and canary p99.

**Dependencies:** P8.10 all classes.

**Likely files:** `cmd/gc/city_runtime.go`, `cmd/gc/build_desired_state.go`, owner
guards/tests.

**Estimated scope:** M.

**Rollback:** Full builder can regain ownership before later deletion.

### Checkpoint G8 — Work-driven starts are incremental

- One work delta updates one bounded set of indexes/pools/sessions.
- No pool action waits for a fleet build or unrelated external check.
- Incremental and independent full-scan results match.
- Custom hidden inputs have honest bounded polling semantics.
- Global demand builder is shadow/audit only.

## 22. Phase 9 — Split the remaining city-loop services

The goal is not a generic engine. Each service gets its own small loop because
it already has different keys, durability, retry, and capacity semantics.

### P9.0 Generate the `CityRuntime` phase-to-owner ledger

**Goal:** Ensure every current select case, tick phase, startup step, and reaper
has an explicit long-term owner before the central loop is reduced.

**Changes:** Extend the P0.1 canonical analyzer/registry to enumerate all current
`CityRuntime` work, including managed-Dolt preflight, workspace-service ticks,
runtime/process/worktree reapers, route/extmsg/nudge-mail recovery, GC/retention,
orders/control/convergence, auto-suspend if present, config reload, health/
observation, and shutdown. Classify each row as composition/supervision,
provider/store manager, named P9 child controller, already-migrated P6–P8 owner,
or justified retained bounded operation.

**Acceptance criteria:** there are zero unmapped domain phases; every row names
authoritative state, stable key/scope, retry/capacity, owning task, cutover gate,
and deletion/retention disposition; adding a new inline domain phase fails the
registry guard; any missing controller discovered by the ledger receives a
separate P9 child task before G9.

**Verification:** generated current-head ledger, intentionally unregistered
select/tick fixture, runtime reachability/effect inventory cross-check, and
maintainer review against `city_runtime.go`/supervisor call graphs.

**Dependencies:** P0.1 canonical registry, P3.4 completeness gate, and P6–P8
owner tables at their current checkpoint.

**Likely files:** canonical analyzer policy, generated/test-only ledger, and
`CityRuntime` ownership guard.

**Estimated scope:** M inventory slice; newly discovered owners are separate M
tasks.

**Rollback:** Test/ledger-only; no runtime change.

### P9.1 Move control/formula dispatch to a keyed child controller

**Goal:** Prevent graph/control operations from blocking session/nudge queues and
vice versa.

**Acceptance criteria:** control bead/run changes map to stable keys; one run/key
is serialized; distinct runs progress concurrently within limits; durable state
drives retry; current dispatcher semantics and trace correlation remain. Every
cross-store child materialization, dispatch, tally, drain, and finalize sequence
names one durable idempotency/operation key and passes B17/B18 in both store
orderings; no implicit process memory completes the protocol.

**Verification:** existing control dispatcher/convergence suites, dependency
unblock, retry, crash, and queue fairness tests.

**Dependencies:** G5B, the P0.1 canonical effect inventory for control/formula
effects, and the P3.4/P3.6 completeness/parity rows for those action families.

**Likely files:** current control dispatcher wiring, new child loop/tests.

**Estimated scope:** Epic; enumerate one M slice per action family.

**Rollback:** Current channel/loop owner retained until graduation.

### P9.2 Move orders to schedule/event keys

**Goal:** Evaluate only the due/relevant order rather than every order inside
patrol.

**Acceptance criteria:** config generation builds order index; schedules use
delay queue; relevant post-commit events enqueue matching orders; duplicate
triggers coalesce according to durable order-run idempotency; one slow order does
not block others or lifecycle. Event delivery is a latency hint only. An order
that promises guaranteed event triggering derives it from a durable event/change
cursor; otherwise periodic evaluation of its durable gate is the bounded
rediscovery path and the weaker bound is documented.

**Verification:** current order tests, timer fake clock, event duplicate/loss,
retry, suspended scope, and restart reconstruction.

**Dependencies:** P4.7, P5.2–P5.6.

**Likely files:** order dispatcher/controller/tests.

**Estimated scope:** M.

**Rollback:** Legacy order scan remains behind owner gate.

### P9.3 Move GC and retention to partitioned maintenance work

**Goal:** Remove multi-second GC/retention work from action scheduling.

**Acceptance criteria:** work partitions by store/scope and bounded page;
deadlines enqueue the maintenance queue; no lifecycle worker executes GC; retry is
store-specific; shutdown cancellation leaves work rediscoverable; maintenance
gets minimum capacity without starving fresh work. GC/retention cannot delete a
nudge/lifecycle/control command whose durable state is nonterminal; command
tiers are structurally exempt or the collector consults the total state machine
under the same snapshot. Every tier's race is in the conformance suite.

**Verification:** existing GC/retention tests, large paged store, slow/failing
store, cancellation, fairness, and no-head-of-line trace.

**Dependencies:** G5B and P5.3; P5.5 only if its measured fairness mechanism is
activated for this workload.

**Likely files:** `cmd/gc/wisp_gc.go`, maintenance controller/tests.

**Estimated scope:** M per retention family.

**Rollback:** Legacy timer may own before deletion.

### P9.4 Move route recovery, extmsg reaping, and nudge-mail sweep

**Goal:** Give each watchdog a key/schedule and remove it from full tick.

**Acceptance criteria:** each operation declares authoritative state, stable key,
retry/deadline, and idempotent result; store/event changes enqueue affected scope;
periodic fallback remains low priority; failure does not block other services.

**Verification:** existing watchdog/sweep tests, partial stores, duplicate events,
and independent failure tests.

**Dependencies:** P4.6, P5.3.

**Likely files:** one watchdog/controller pair per PR.

**Estimated scope:** S/M per service.

**Rollback:** Existing watchdog retained until its flip.

### P9.5 Replace convergence request handling in the main select

**Goal:** Ensure a synchronous API/control request cannot wait behind patrol.

**Acceptance criteria:** request commit/enqueue/reply semantics are explicit;
request cancellation does not cancel durable work; same-key requests serialize;
response identifies accepted versus converged; no channel receive depends on
finishing unrelated work.

**Verification:** concurrent requests, cancellation, controller restart, queue
saturation, and API contract tests.

**Dependencies:** P9.1, API control-plane review.

**Likely files:** convergence request adapter/controller tests, Huma types only if
wire changes.

**Estimated scope:** M.

**Rollback:** Compatibility adapter can proxy to old handler before deletion.

### P9.5A Compose one provider-global shutdown/admission owner

**Goal:** Make city stop and provider swap one durable operation across every
session mutation family instead of a collection of partially ordered loops.

**Protocol:** Atomically close city admission and advance
`city_admission_generation`; all workers recheck that value immediately before native effect entry. Drain or
fence actual in-flight lifecycle, nudge, start, and direct interaction calls.
Then execute reverse-dependency exact interrupt/stop waves through the same
per-session executor, resolve targeted unknowns/orphans, tear down only the
explicit city provider, record `ready_to_terminate`, and shut down required
stores. The independent CLI/supervisor completes post-exit proof from
RC-SHUT-001. `--force` conditionally advances `force_revision` on this operation
ID and skips grace; it never
creates another owner.

**Acquisition order:** normal and shutdown effects use
`city_admission_generation` → per-session executor ownership → nonblocking provider permit →
provider-native entry. No path waits for per-session ownership while holding a
provider permit, waits for capacity while holding a global teardown lock, or
holds a resource required by shutdown while sleeping. Shutdown closes admission
without waiting, then observes/drains existing owners; its privileged stop wave
uses the same per-session/provider order.

**Acceptance criteria:** `RC-SHUT-001..005`, `RC-CLI-004..006`,
`RC-CLI-010`, and the full shutdown combinations in `RC-QUEUE-005` pass. New durable starts remain pending but cannot enter; an
ambiguous call blocks provider replacement; independent confirmed targets make
progress; unresolved targets prevent terminal success; invalid config never
guesses a provider/socket; required store failure is terminal non-success. A
self-targeting caller reports accepted-incomplete before fence release, while an
external waiter proves controller exit, kernel ownership release, provider
absence, and store-process shutdown.

**Verification:** generated step/fault table; barriers at admission close,
session ownership, provider permit/native entry, each stop wave, teardown,
pre-release marker, store close, controller exit, and external proof; force
upgrade, deadlock model, SIGKILL/re-exec, invalid config, isolated tmux, and
blocked-output requester tests.

**Dependencies:** P0.13–P0.15, P1.7, P1.10, P2.11–P2.12, P5.4/P5.7/P5.11, P7.7A/P7.8,
P9.0, and the family-specific effect completeness/capability gates.

**Likely files:** one shutdown coordinator/composition adapter in `cmd/gc`,
domain request semantics in `internal/session`, worker/runtime effect seams, and
focused tests. Decompose by admission, stop waves, teardown, and external proof.

**Estimated scope:** Epic; four M red/green slices, never one PR.

**Rollback:** Cold-stop the keyed owner only after every actual call is
drained/fenced; retain the current shutdown path as sole owner until the full
fault table passes.

### P9.6 Reduce `CityRuntime` to composition and supervision

**Goal:** Remove domain work from the central `select` loop.

**Target responsibilities:** load/publish config generation, open stores and
providers, start/sync/supervise child controllers, expose status, coordinate
shutdown, and run anti-entropy scheduling. It does not build desired state,
deliver nudges, dispatch control work, execute lifecycle, or run GC inline.

**Acceptance criteria:** no selected case performs unbounded store/provider I/O;
one child slowdown does not delay ingress for another; startup sync and shutdown
have explicit state/timeout; old compatibility foreground and supervisor paths
compose the same children.

**Verification:** child lifecycle tests, blocked child, panic, reload, shutdown,
and supervisor integration.

**Dependencies:** P7.14, P8.11, P9.0–P9.5A with every P9.0 ledger row disposed.

**Likely files:** `cmd/gc/city_runtime.go`, controller composition tests.

**Estimated scope:** Epic; enumerate deletion-focused M PRs.

**Rollback:** Keep old loop selectable for one release.

### Checkpoint G9 — No domain action shares the city loop

- The P9.0 current-head ledger has zero unmapped domain phases.
- Session, nudge, pool, control, orders, and maintenance have isolated queues and
  capacity.
- Main-loop ingress remains responsive under deliberately hung child work.
- Every child reconstructs from durable/cache state after restart.
- The old full tick has no production side effects.

## 23. Phase 10 — Make anti-entropy permanent and independently trustworthy

Anti-entropy is an independent enumeration and fold, not a call back into the
incremental traversal, cursor, cached object set, derived index, or applier. It
reuses the canonical total decoder and pure `Contributions(object)` semantics;
duplicating those semantics would create a second domain model and a different
source of entropy.

### P10.1 Assemble independent authoritative reference projections

**Goal:** Assemble production-capable reference folds shipped before each index
cutover, then add remaining config-generation, dependency, order, and control
references without using incremental traversal or update functions.

**Acceptance criteria:** reference builders independently enumerate raw
authoritative objects, reuse the canonical decoder and pure contribution
function, and fold into fresh object/index/tombstone sets. They are
deterministic; partial snapshots cannot authorize removal/destruction; each
result carries store cursor/source watermark; generated histories compare
incremental and reference results structurally, not by checksum alone. An
import/static guard forbids incremental cursors, cached object sets, derived
indexes, appliers, and their checksum helpers while allowing the canonical
decoder/projector/contribution package.
This is the production reference required by `RC-ENTROPY-001`; a shared decoder
does not permit shared traversal or derived state.

**Verification:** full-snapshot fixtures, permutation, partial failure, split
stores, and deliberate incremental-index bug fixtures.

**Dependencies:** P4.7 config audit, P6.2, P7.1, P7.11, P8.3–P8.4, and the
read-only reference/full-scan fixtures retained by P9.1–P9.4.

**Likely files:** one `*_reference.go` test/maintenance builder per projection,
plus shared snapshot inputs only after two consumers justify it.

**Estimated scope:** S/M per projection.

**Rollback:** Reference builders are read-only.

### P10.2 Add a budgeted partitioned audit scheduler

**Goal:** Cover every authoritative object eventually without fleet-sized CPU,
memory, I/O, or enqueue bursts.

**Changes:** Partition by stable store/scope/key hash, page consistent snapshots,
jitter work, cap per-run I/O/time/enqueues, and schedule through maintenance
capacity. Audit progress is disposable; restart safely begins a new traversal.

**Acceptance criteria:** no status/PID/progress file; every partition is covered
under fair scheduling; one partition failure does not reset completed in-memory
work or block others; cursor gap/relist can request focused audit; large repairs
are rate-limited. Completion telemetry records expected/completed/failed
partitions, snapshot watermarks, last complete audit time, maximum traversal
age, and partial/inconclusive outcome. Zero repairs without a completed current
traversal cannot pass a canary.
The certified recovery formula and persistent-drift escalation satisfy
`RC-ENTROPY-002`.

**Verification:** fake clock, 100K/1M generated keys, restart mid-traversal,
failure/fairness, and resource-bound tests.

**Dependencies:** P9.3, P10.1.

**Likely files:** `cmd/gc/anti_entropy_scheduler.go`, tests.

**Estimated scope:** M.

**Rollback:** Disable scheduled audits; startup/full scans remain temporarily.

### P10.3 Repair store projections minimally

**Goal:** Turn one detected mismatch into cache/index correction plus the
smallest affected key set.

**Acceptance criteria:** repair never writes authoritative domain state; object
and all indexes change atomically; stale reference watermark cannot overwrite a
newer cache object; deletion/tombstone mismatches are handled; repeated repair
is idempotent; each repair records source, old/new revision, affected controller
kinds, and reason.

Repair uses the same monotonic projection applier owned by P4.3/P4.6; it does
not create a second mutation path. The reference computation remains
independent.

**Verification:** direct cache/index corruption, concurrent newer delta, missed
delete, duplicate object, wrong reverse edge, and minimal-requeue tests.

**Dependencies:** P10.1–P10.2.

**Likely files:** cache applier/anti-entropy repair/tests.

**Estimated scope:** M.

**Rollback:** Fall back to atomic projection replacement and scoped enqueue.

### P10.4 Audit runtime reality independently

**Goal:** Detect out-of-band session creation, death, replacement, and observer
watch gaps.

**Acceptance criteria:** provider census is bounded/partitioned where supported;
unknown/partial census never mints death; observed replacement updates
incarnation and enqueues exact key; orphan candidates require targeted witness
before any later action; one provider outage does not erase last-known state.

**Verification:** out-of-band start/stop/restart, partial census, stale watch,
name reuse, provider outage/recovery, and multi-provider independence.

**Dependencies:** G1, P4.9–P4.10.

**Likely files:** runtime observer/audit controller/tests.

**Estimated scope:** M per provider class.

**Rollback:** Existing provider census remains as slow compatibility observer.

### P10.5 Unify startup, cursor-gap recovery, and audit application

**Goal:** Ensure recovery paths use the same projection replacement and key
enqueue semantics as normal operation.

**Acceptance criteria:** both capability profiles reuse P4.0's sole atomic
snapshot installer. A feed-certified source installs objects/indexes,
establishes a no-gap watch or typed gap, records initial exact/fanout dirty
state, marks source/handler sync, and only then starts dependent effect workers.
An audit-only source installs one complete authoritative snapshot, schedules
its versioned bounded relist before effects start, and publishes the weaker
recovery/latency profile without inventing a cursor. Changes during
feed-certified startup are not lost; feed gaps use the same replacement
function. Audit-only external changes are rediscovered within the P4.3A bound.
Actions do not run from unsynced/partial sources except explicitly safe local
commands with all required facts.

**Verification:** writes during snapshot, watch before/after install, one store
unsynced, restart with pending commands, and cursor compaction tests.

**Dependencies:** P4.0, P4.3A, and P10.3–P10.4. Feed-certified startup also
requires P4.6/G4B; audit-only startup requires G4A and its bounded relist
profile, not G4B.

**Likely files:** watch manager, projection installer, reconcile supervisor tests.

**Estimated scope:** M.

**Rollback:** Block actions until old full startup completes.

### P10.6 Build the full entropy fault suite

**Goal:** Exercise the declared fault envelope through the one deterministic/
generated/process replay format and retain every failure as evidence.

**Faults:** all hints dropped; all duplicated; old update delayed after new;
watch disconnect/gap; partial store list; false-negative runtime list; direct
external mutation; out-of-band runtime change; corrupt object/index/reverse edge;
controller restart under saturation; rapid generation churn; nudge during
stop/restart; concurrent source dirties; provider/store outage; disk-full/event
failure; clock jumps; audit interruption; poison feed/config/command objects;
cursor fork/rewind and tombstone resurrection; interrupted schema migration;
file-descriptor/goroutine/memory pressure; audit repeatedly interrupted before
late partitions; unsupported-version objects; split-store snapshot disagreement;
repair crash after projection replacement before enqueue; independent trace-disk
and authoritative-store disk exhaustion; and an old controller writing during a
supposedly read-only mixed-version period.

**Acceptance criteria:** after finite faults stop and authoritative sources become
available, the system reaches the latest durable intent with no stale destructive
effect, no lost edge command, matching projections, and an empty next plan.

**Verification:** bounded exhaustive model in PR CI; seeded randomized histories
with shrinking; real process/Dolt/tmux nightly soak; every failure seed retained.

**Dependencies:** G9, P10.5.

**Likely files:** test-only model/fault harness, integration fixtures.

**Estimated scope:** Test epic; enumerate M slices by fault domain.

**Rollback:** Test-only.

### P10.7 Add anti-entropy observability and escalation

**Goal:** Treat repair as a correctness alarm, not ordinary noise.

**Acceptance criteria:** healthy audit emits cheap completion metrics; every
repair increments a high-signal counter and trace with layer/source/category;
repair storms coalesce alerts but retain counts; cursor-gap loop, repeated same
repair, and inability to complete a traversal page trigger an alert; runbook gathers live
state via `gc trace` and status APIs.

**Verification:** metric/event registration, alert-state tables, no-cardinality
lint, and runbook incident drill.

**Dependencies:** P10.3, P10.6.

**Likely files:** anti-entropy metrics/events, `reconciler-debugging.md`, tests.

**Estimated scope:** M.

**Rollback:** Observability-only.

### P10.8 Demote patrol to audit scheduling only

**Goal:** Remove the final timer dependency from action latency.

**Acceptance criteria:** patrol/audit enqueues keys or projection repair only; it
never calls lifecycle, nudge, pool, control, order, or provider effects inline;
increasing audit interval does not change steady-state schedule-to-start; all
lost-signal tests still recover.

**Verification:** effect-boundary guard, patrol disabled/very-long interval E2E,
lost-hint recovery, and action latency comparison.

**Dependencies:** P10.5–P10.7 and G9.

**Likely files:** `cmd/gc/city_runtime.go`, audit scheduler, tests.

**Estimated scope:** M deletion.

**Rollback:** Restore faster audit cadence, not monolithic inline effects.

### Checkpoint G10 — Bounded projection entropy is repaired or alarmed

- Every projection has an independently enumerated fresh reference fold.
- Under the declared fault model, lost/duplicate/gapped signals and deliberate
  mechanically recoverable projection corruption converge; persistent,
  authoritative, partial, or common-mode uncertainty fails closed and alarms.
- Audit repair is normally zero and operationally loud when nonzero.
- Startup, relist, and periodic audit share proven projection replacement.
- Patrol frequency no longer affects action-start latency.

## 24. Phase 11 — Optional horizontal sharding and high availability

This phase is gated by evidence. A fast, robust single-owner controller is the
default target. Active-active is prohibited until storage and providers can
fence stale owners.

### HA feasibility preconditions

- Conditional writes are in `require` mode on one linear working set.
- The production change feed, projection rebuild, and keyed executor have
  completed a release of soak.
- P11.0 feasibility passes for every proposed store/provider; final integrated
  conformance remains mandatory in P11.5 before any second effect executor.
- Single-controller performance data demonstrates a real capacity or failover
  need that city-level distribution cannot solve.
- Lease time/source and clock-skew semantics are proven against the actual
  hosted store; leader election without fencing is explicitly insufficient.

### P11.0 Prove store/provider epoch-fencing feasibility

**Goal:** Establish that each proposed topology has a real compare-and-act seam
before building leases or claiming an HA path.

**Spikes:** first select and prove one durable-write fencing mechanism: either a
cross-bead transactional conditional (`write target X iff lease L epoch == E`)
implemented by every eligible backend/Beads verb, or a per-bead epoch-stamping
protocol whose takeover quiesces/stamps every target before old-epoch writes are
considered fenced. Stock per-bead P2.0 CAS does not condition on a separate
lease and is insufficient by itself. Then prove conditional epoch checks on
every owned durable write family and a provider-local operation that rejects a stale controller epoch together
with the target incarnation. Kubernetes object UID/resource preconditions prove
incarnation, not stale-controller fencing for a new create by themselves; the
adapter also needs an epoch-aware admission/operation resource or equivalent
atomic provider mechanism. T3, tmux, subprocess, and other providers receive the
same honest feasibility test and remain single-owner when it fails.

**Acceptance criteria:** every proposed store/provider and selected cross-bead/
stamping mechanism is classified
`feasible`, `single-owner-only`, or `unsupported` with executable evidence; no
memory-only lease check counts; a paused old caller released after a simulated
epoch transfer is rejected or proven to have no semantic effect at the real
seam—idempotency alone is insufficient; no mandatory generic runtime interface
is enlarged before two implementations exist.

**Verification:** provider/store proof-of-concept conformance, stale create/
stop/nudge/status/command-ack attempts, stale control/order/GC delete and extmsg
write, response loss, quiescent untouched target during takeover, and negative
fixtures showing UID/incarnation or target-only CAS without lease epoch is
insufficient.

**Dependencies:** G10, P1.8 provider conformance, P2.0 target-only conditional
writes as a substrate (not complete lease fencing), and
the P11 feasibility preconditions not involving provider fencing.

**Likely files:** provider-local prototype adapters/tests, conditional-writer
conformance extensions, and an HA feasibility report; no production enablement.

**Estimated scope:** Epic; one bounded M spike per proposed store/provider.

**Rollback:** Spike-only; unsupported profiles remain single-owner.

### P11.1 Publish an HA eligibility/support matrix

**Goal:** Refuse unsafe topologies before starting a second executor.

**Checks:** one shared linear database working set, no unsupported branch/
federation exposure, conditional-write capability, change-feed capability,
server-time/lease semantics, provider namespace sharing, provider fence support,
selected cross-bead/stamping fence support, and store/provider latency budget.
City provisioning mints a durable working-set/cluster UUID pinned in config and
embedded in every lease; every connection/acquire/renew verifies it. Controllers
retain epoch/revision high-water marks and treat any regression/fork as loss of
linear authority.

The current sqlite city/graph plus Dolt/DoltLite rig/work split is permanently
single-owner for this design unless it first consolidates the HA-owned causal
path into one linear coordination working set or a separately approved design
proves an atomic fence in every participating store. A lease in store A cannot
fence writes in store B; matching UUID strings or per-store CAS does not make a
cross-store transaction.

**Acceptance criteria:** unsupported topology fails closed with actionable
diagnostics; compatibility single-owner mode remains available; no `auto`
degradation can enable active-active. Wrong working-set identity or coordination
epoch/revision regression fails closed at runtime, not merely startup.

**Verification:** matrix tests for every production store/provider/topology,
wrong-store/config drift while running, epoch/revision rollback/fork, and
doctor/preflight tests.

**Dependencies:** P11.0 and all HA feasibility preconditions.

**Likely files:** HA preflight package, doctor/status, tests.

**Estimated scope:** M.

**Rollback:** Disable HA; single owner continues.

### P11.2 Introduce fixed virtual shard identity

**Goal:** Partition stable controller keys without tying durable state to the
current replica count.

**Changes:** Hash keys into a fixed virtual shard set; include controller kind/
scope so global/pool/session/maintenance ownership is explicit; changing replica
membership reassigns shards, not key identity.

**Acceptance criteria:** deterministic across processes/versions; balanced under
representative keys; no role/provider-specific hashing; shard count/version is
durable config with migration test; one key maps to exactly one action owner.

**Verification:** golden hash vectors, distribution, upgrade, and split-store
tests.

**Dependencies:** P5.1.

**Likely files:** `internal/reconcileshard/key.go`, tests, and the normal typed
config/patch/merge path for an approved durable shard version.

**Estimated scope:** S.

**Rollback:** One replica owns all shards.

### P11.3 Implement durable shard leases with comparable fencing tokens

**Goal:** Transfer action ownership without relying on process discovery or a
per-host flock.

**Contract:** acquire/renew/release through conditional writes. The per-key
comparable `owner_epoch` is the lexicographically ordered tuple
`(shard_map_version, shard_epoch)`; the map version is itself fenced and any new
mapping starts above every token reachable under the old mapping. The shard
epoch increments on every acquisition. Expiry uses an authoritative time source or proven bounded
clock model; lease loss immediately stops admission; store connections/capacity
are reserved for renewal; release/transfer waits for effects to drain or become
provably provider-fenced; durable lease is coordination state, not a status file.
Every lease carries the P11.1 working-set UUID. Target writes use the P11.0
selected cross-bead conditional or complete per-bead stamping protocol; a lease
record plus unrelated target CAS is never described as fencing. Whole-bead-
emulated lease CAS uses a dedicated coordination bead and passes
`RC-STATE-003`. The certified maximum clock skew, how it is measured, and the
runtime threshold response are part of the profile; exceeding it freezes lease
admission rather than guessing expiry.

**Acceptance criteria:** one current lease record exists under the supported
store model; safety does not rely on that fact alone—every provider effect and
owned durable write rejects a stale epoch. Ambiguous renew stops new effects
until reread; lease renewal cannot be starved by action traffic; lease load is
bounded and partitionable. Working-set mismatch or any observed epoch/revision
regression immediately stops admission and disables the HA profile.

**Verification:** CAS races, response loss, delayed renew, clock skew, store
partition, process death, and rapid transfer tests.

**Dependencies:** P11.0 selected fencing mechanism, P11.1–P11.2, and its exact
store implementation; P2.0 target-only CAS alone is insufficient.

**Likely files:** lease domain/front door/tests, store conformance.

**Estimated scope:** Epic; enumerate and decompose into M slices.

**Rollback:** One configured owner; leases disabled.

### P11.4 Keep followers warm without executing

**Goal:** Reduce failover recovery time while preserving single effect owner.

**Acceptance criteria:** followers maintain store/runtime watches and projections,
run read-only plan shadow, and expose lag; they never claim commands, write owned
status, or enter providers without shard lease; takeover verifies synced cursors
or relists before enqueue. “Warm” runtime observation is claimed only when the
follower reaches the same provider reality; host-local tmux/subprocess state that
cannot be observed remotely selects a cold or host-pinned failover profile.
Promotion for an unfenceable host-local provider requires positive proof the old
owner cannot act: on the same host, kill the exact controller PID/start identity
and confirm reaping before takeover; across hosts, refuse automatic promotion.
A paused/D-state old owner is not proof of death.

**Verification:** spy effects on follower, lagged follower takeover, cursor gap,
and warm/cold failover timing.

**Dependencies:** P11.3, G10.

**Likely files:** reconcile supervisor ownership adapter/tests.

**Estimated scope:** M.

**Rollback:** Cold standby or single owner.

### P11.5 Enforce epoch fencing at every external and durable mutation

**Goal:** Ensure an old lease holder cannot start, stop, or nudge after takeover.

**Provider work:** add/verify epoch/operation preconditions using provider-native
atomic identity where possible. Kubernetes uses UID/resource preconditions; T3
uses thread/incarnation/idempotency contracts; tmux/subprocess require a proven
provider-local compare-and-act mechanism or remain single-owner-only.

Integrate the P11.0 provider-local optional epoch-fenced mutation capability with
the real lease epoch. If two providers converge on the same shape, extract a
small shared interface then; otherwise keep adapters local. Kubernetes stale
create remains unsupported until the epoch-aware mechanism proven in P11.0 is
enforced atomically—it is not satisfied by UID alone.

Owned session status, lifecycle request/result, command claim/ack, and allocation
writes use the selected P11.0 cross-lease or stamping mechanism—not merely P2.0
target revision CAS. Provider command-ID deduplication from P6.4 is useful but is
not stale-controller epoch fencing and does not satisfy this task by itself.

Generate an exhaustive table from P0.1/P9.0 for session/provider effects;
status/request/result; nudge claim/ack; pool allocation; control/formula child
creation, tally, and root transition; order run state; GC/retention delete;
route/mail/extmsg writes; and event publication. Each row is classified
`lease-epoch-fenced`, `per-target-epoch-stamped`, `revision-only + mechanically
idempotent`, or `shard-ineligible/singleton lease`, with proof and owning test.
No Phase 9 writer is omitted because it is not session-shaped.

**Acceptance criteria:** lease check only in controller memory is insufficient;
stale epoch reaches provider test seam and is rejected; exact incarnation remains
mandatory; stale epoch also fails every owned durable write in the generated
table or the row remains singleton/ineligible; unsupported provider or store
blocks HA eligibility. G11 mechanically compares table coverage to the current
effect inventory.

**Verification:** pause old controller after lease read, transfer lease, then let
old effect proceed; it must affect nothing. Include stale delete, stale
dispatch/child materialization/tally, order transition, extmsg, and quiescent
untouched-target takeover cases; run per provider/store family.

**Dependencies:** P11.0 selected fencing mechanism, P11.3, P1.4, P2.0 as a
target-revision substrate, and the complete P0.1/P9.0 inventory.

**Likely files:** runtime/worker provider-specific operations/tests.

**Estimated scope:** Epic; decompose by provider mutation family and owned
durable-writer family, each as an M slice.

**Rollback:** Provider marked single-owner-only.

### P11.6 Dispatch queues only for owned shards

**Goal:** Scale executor workers horizontally without duplicate provider work.

**Acceptance criteria:** only current shard owner enqueues/executes or a
non-owner forwards a wake hint without durable loss; ownership change rebuilds
dirty keys from projection; in-flight old-epoch results cannot commit. A
shard-filtered snapshot/watch/cache bounds observation memory/decoding, or the
topology is explicitly classified as failover HA rather than observation-plane
horizontal scale. Provider-global concurrency is enforced through a shared
provider mechanism or admission service; local semaphores alone do not claim a
global limit.

**Verification:** rebalance under load, dirty key during transfer, old delayed
queue item, restart, and no duplicate effect tests.

**Dependencies:** P11.3–P11.5.

**Likely files:** queue/supervisor shard adapter/tests.

**Estimated scope:** M.

**Rollback:** Assign all shards to one owner.

### P11.7 Partition formerly global controllers

**Goal:** Avoid reintroducing a global leader bottleneck.

**Rules:** session/nudge keys shard directly; pool/order/control keys shard by
their stable key; maintenance audits shard by partition; config generation is
immutable replicated input. Any genuinely singleton mutation gets a dedicated
lease and is kept off lifecycle capacity.

**Acceptance criteria:** no fleet-global mutex/leader on session hot path;
cross-shard dependency changes wake the owning shard through durable/cache state;
singleton failure cannot block unrelated shards.

**Verification:** multi-shard dependency, pool-to-session, config reload, order,
and maintenance tests.

**Dependencies:** P11.6.

**Likely files:** controller-specific shard adapters/tests.

**Estimated scope:** M per controller.

**Rollback:** One owner for all shards.

### P11.8 Prove split-brain and failover behavior

**Goal:** Demonstrate the fenced HA profile against every enumerated model and
real-process fault before any destructive shard is canaried.

**Faults:** duplicate lease acquisition attempts, paused old leader, network/
store partition, clock skew, slow renew, process SIGKILL, cursor lag, provider
timeout, shard rebalance, simultaneous configuration change, working-set UUID
switch, epoch/revision rollback, stale GC/delete, stale control child/tally,
stale order transition, and stale extmsg write.

**Acceptance criteria:** zero stale-epoch provider effects; zero stale status
commits; pending commands conserved; failover RTO measured; unsupported provider
cannot enter test topology; quiescence matches latest durable state.

**Verification:** extend the one replay/model format to two owners, lease epochs,
partitions, delayed calls, provider acceptance, and status/command commits. Pause
the old owner after lease read, provider dispatch, provider acceptance, before
status commit, and before command acknowledgement; then run real multi-process/
store/provider integration and long soak.

**Dependencies:** P11.3–P11.7.

**Likely files:** HA integration harness and fixtures.

**Estimated scope:** Test epic; enumerate M slices by fault domain.

**Rollback:** Disable multi-owner rollout.

### P11.9 Roll out scale in the cheapest order

**Goal:** Add distribution only where measured need justifies it, graduating
read-only and non-destructive topology before destructive actions.

**Order:** distribute cities across supervisors/hosts → one warm follower per
high-risk city → shard read-only shadow → canary one non-destructive shard →
session starts → nudges → destructive actions last.

**Gates:** all G10 invariants, zero fence rejection anomaly, bounded lease/feed
lag, explicit warm/cold failover and shard-transfer RTO targets, measured need,
provider support, rollback drill, and customer-specific maintenance window.

**Acceptance criteria:** every promoted topology is named in the support matrix;
unsupported providers remain single-owner; each step meets its RTO/SLO and fence
gates before the next action family begins.

**Verification:** staged deployment test, shard read-only shadow, canary/abort,
warm/cold failover, destructive-action-last rollback, and evidence packet.

**Dependencies:** P11.8.

**Likely files:** deployment/runtime configuration and runbooks; no role names or
provider assumptions in generic code.

**Estimated scope:** Operational phases.

**Rollback:** Collapse all shards to one fenced owner.

### Checkpoint G11 — Horizontal scale is honest

- Single-owner mode remains the safe universal default.
- Enabled HA topologies have linear store CAS, warm synced followers, shard
  leases, and provider-recognized fences.
- Split-brain model/conformance/integration tests demonstrate for every
  enumerated supported fault that stale owners cannot affect runtime or status.
- The P11.5 fencing/classification table is complete against the current
  provider/durable effect inventory, including all Phase 9 writers; working-set
  identity and epoch/revision regression guards are live.
- Unsupported providers/topologies fail closed rather than degrading silently.

## 25. Phase 12 — Delete legacy complexity and certify operations

Deletion is a feature. It occurs in small PRs after ownership has been unreachable
for the required release soak and rollback drills have passed. P12.1 child
deletions may run as rolling cleanup immediately after their own family's soak;
they do not wait for unrelated pool/control/HA work. This phase is where the
final aggregate certification is recorded.

### P12.1 Delete legacy effect arms by family

**Goal:** Remove each unreachable legacy writer and its ownership exception as
soon as its own soak/rollback window permits.

**Order:** nudge scan/pollers → every numbered P7.13 session family in its exact
authoritative order → pool demand → control/orders/watchdogs/remaining P9.0
rows. A generated registry comparison makes a missing, merged, or extra family
fail CI rather than relying on this prose list.

**Acceptance criteria:** effect completeness proves no caller; all scenario rows
map to new tests; legacy rollout option has been disabled in production for one
release; each deletion reduces exceptions in ownership guards. Every P7.13
graduated family and P9.0 phase maps to exactly one P12.1 child deletion or an
explicitly justified retained non-domain path; no family disappears between the
owner table and deletion ledger. A family is not physically deleted while any
pre-graduation family shares its status-writer/provider-shutdown abort-coupling
domain, unless P12.7 certifies and operators accept a measured release-rollback
RTO for the resulting no-owner freeze. Calendar soak alone is insufficient:
each family records minimum observed/injected occurrence counts for its rare
cursor-gap, clock-jump, provider-swap, replacement, ambiguity, and mass-expiry
paths before deletion. Stop/close and the P9.5A shutdown owner remain compilable
but disabled for at least two releases, with a drilled cold resurrection path,
unless owner-approved evidence proves a shorter retention safer.

**Verification:** focused tests, full sharded gates, production trace reader
inventory, and N-1 rollback policy review.

**Dependencies:** P6.6–P6.8 for nudge; the applicable P7.13 family plus P7.14
reachability for session effects; P8.10–P8.11 for pool demand; the applicable
P9 owner flip for control/order/watchdogs; and each family's required release
soak/P0.12 deletion window.

**Likely files:** legacy files/tests per family.

**Estimated scope:** S/M per deletion.

**Rollback:** Release rollback, not runtime dual writer.

### P12.2 Remove raw lifecycle mirrors and mid-pass mutation folds

**Goal:** Delete the raw/`Info` dual representation, predicate twins, and
`ApplyPatch` snapshot-coherence protocol from reconciler decisions.

**Acceptance criteria:** canonical projection/index is the only decision input;
raw metadata access remains only in codec/storage/migration/diagnostic exceptions;
all 32-style twin equivalence tests either migrate to canonical tables or are
deleted with their code; no projected-world behavior depends on mutation order.

**Verification:** ownership AST guard, projection/reference tests, full session
suite, and LOC/dependency report.

**Dependencies:** G8, P12.1 relevant families.

**Likely files:** `cmd/gc/session_reconciler.go`, snapshot helpers,
`internal/session` projection tests.

**Estimated scope:** Epic; enumerate one M deletion PR per family.

**Rollback:** Release rollback.

### P12.3 Delete global desired-state/action scheduling machinery

**Goal:** Remove demand snapshot age, tick debounce as action policy, global
session action planning, patrol-triggered delivery, and obsolete startup/recovery
ladders.

**Acceptance criteria:** `CityRuntime` composition responsibilities match §5;
changing patrol interval does not alter action latency; no async stale
`buildDesiredState` path remains; full-scan reference code is owned solely by
anti-entropy/tests.

**Verification:** no-action-in-patrol guard, startup/gap/audit tests, full sharded
suites, and trace causal-path check.

**Dependencies:** G9–G10, P12.1–P12.2.

**Likely files:** `cmd/gc/city_runtime.go`, `cmd/gc/build_desired_state.go`, legacy
tests.

**Estimated scope:** Epic; enumerate one M deletion per subsystem.

**Rollback:** Release rollback.

### P12.4 Retire temporary migration gates

**Goal:** Make the keyed path unconditional and remove configuration debt.

**Acceptance criteria:** default was keyed for one release; break-glass was
unused or understood; registry expiry/graduation tests point to deletion;
config/doc/generated schema fields disappear with compatibility diagnostics if
needed; no stale environment override remains a test leak vector.

**Verification:** rollout graduation, config unknown/retired key, docs/schema,
and clean environment tests.

**Dependencies:** P12.1–P12.3 and the P12.7/P0.12 N/N-1 case for every removed
config field, schema reader, migration gate, and rollback binary. Compatibility
proof gates deletion; it is not a post-deletion discovery pass.

**Likely files:** rollout registry/config/generated docs/tests.

**Estimated scope:** M.

**Rollback:** Release rollback only.

### P12.5 Decide trace consolidation from reader evidence

**Goal:** Simplify tracing only if the smaller recorder demonstrably replaces
all current guarantees and workflows.

**Acceptance criteria:** enumerate every WAL/API/CLI/operator reader; run new
recorder in parallel for a release; preserve framing, corruption/loss accounting,
rotation, causal IDs, and incident commands or explicitly retain the current
trace; mapping for historical reason/site codes remains for the approved period;
scrubbing policy covers customer data.

**Verification:** reader reachability tests, corrupt/rotation/tail recovery,
incident drill, and production dual-record comparison.

**Dependencies:** G10 and no migration oracle reader.

**Likely files:** trace subsystem/projection/runbook; may conclude “keep current
implementation.”

**Estimated scope:** Separate reviewed project.

**Rollback:** Keep current WAL authoritative.

### P12.6 Publish operator status, dashboards, and runbooks

**Goal:** Give operators the live evidence and practiced recovery controls needed
to run each certified profile without source-code archaeology or stale files.

**Required views:** controller child sync/health; action owner; per-controller
queue depth/oldest age; provider capacity; blockers; cursor lag/gaps; expectations;
anti-entropy repairs; store UUID/restore epoch/schema/writer version; authorization
mode/denials/unknown holds; protected-contract/delta digest; lease/shard state if
enabled; SLO burn; last rollback/restore drill.

**Acceptance criteria:** user-facing API remains typed/Huma-generated; events
have registered typed payloads; dashboard generated types/spec are fresh;
runbooks use live state and `gc trace`, never status/PID files; one-click/CLI
evidence bundle is bounded and scrubbed. Runbooks include sanctioned sqlite/Dolt
restore/re-anchor, wrong-store/schema freeze, command quarantine/reissue,
credential compromise/revocation, and cold un-bridge/resurrection procedures.

**Verification:** OpenAPI/dashboard gates, docs links, live preview/API contract,
and incident tabletop exercises.

**Dependencies:** Stable metrics/contracts from G10/G11.

**Likely files:** internal API/status, generated schema/types, dashboard, docs and
engdocs; split by audience and surface.

**Estimated scope:** Epic; enumerate and decompose into M slices.

**Rollback:** Additive views; retain CLI fallback.

### P12.7 Prove upgrade, downgrade, and rollback compatibility

**Goal:** Never discover version skew during a customer incident.

**Matrix:** N-1 reader/controller against N additive status/command/feed data; N
against N-1 store/provider; pinned `bd` binary × store-schema versions in both
directions; rollback with pending start/drain/nudge; sanctioned restore from an
older sqlite/Dolt backup and re-anchor; feed schema upgrade/downgrade;
conditional-write mode; mixed-version unrecognized state; trace reader
compatibility.

**Acceptance criteria:** old code safely ignores or understands new fields, or a
declared rollback barrier has a tested downgrade migration; fallback stops new
writer before old writer starts; queues/caches rebuild; provider in-flight state
is observed/adopted. Restore/schema cases satisfy `RC-STORE-001..003` and never
replay recovered effectful intent automatically.

**Verification:** binary matrix integration and automated rollback drill on each
release candidate.

**Dependencies:** P0.12. The exact case is a prerequisite before every default
flip and physical deletion, especially P12.4; an aggregate final pass runs after
all approved deletions without weakening those earlier gates.

**Likely files:** integration harness/release scripts/runbook.

**Estimated scope:** M.

**Rollback:** This task defines it.

### P12.8 Certify the supported high-risk profiles

**Goal:** State exactly which store/provider/topology combinations meet which
guarantees.

**Profiles:** exactly the §2 names: Compatibility single-owner (including its
audit-only bounded-relist shape); Exact-target single-owner; Single-owner
low-latency; Deduplicated command; and Fenced HA. Unsupported/degraded settings
are explicit refusals or exclusions, not silently renamed profiles.

**Acceptance criteria:** each profile lists safety, delivery, latency, failover,
and recovery guarantees plus exclusions; conformance artifacts are attached to
the release; high-risk profile refuses unsupported settings; support can
reproduce the customer topology in an isolated test. Hosted/multi-tenant
certification requires `RC-AUTH-001..003`; every profile requires
`RC-STORE-001..003`, `RC-GATE-001..002`, and `RC-CERT-001..003`. A zero-config
small-city install uses only certified conservative defaults, passes the full
entropy suite, meets its pinned latency/resource envelope, and reports total
new knob count.

**Verification:** profile matrix test and release checklist sign-off.

**Dependencies:** G10 and optionally G11.

**Likely files:** contributor/operator docs, doctor/profile tests, release
artifacts.

**Estimated scope:** M.

**Rollback:** Downgrade advertised profile, never silently weaken runtime checks.

### P12.9 Right-size the permanent verification program

**Goal:** Keep military-grade evidence maintainable after migration without
turning CI/runtime cost into its own treadmill.

**Changes:** Measure verification LOC, VT1/VT2/VT3/nightly/RC minutes, artifact
storage/retention, flake/retry rate, profile/knob count, and owner load. Move or
deduplicate expensive cases only from evidence and only while each protected
invariant retains a deterministic merge-tier tripwire. Delete migration-only
oracles after their mapped owner is gone; retain entropy, restore, schema,
authorization, rollback, and rare-event regressions (`RC-CERT-003`).

**Acceptance criteria:** the permanent set fits approved budgets with no
unmapped `INV-*`/`RC-*`; no green skip or missing platform artifact; every tier
has an owner and failure SLA. Cost reduction cannot modify a protected row in
the same implementation change without the P0.16 delta process.

**Verification:** generated registry/cost report and seeded test-removal/
retiering negative fixtures.

**Dependencies:** P12.1–P12.8 and one complete release evidence cycle.

**Likely files:** evidence registry, CI shard policy, retention configuration,
contributor runbook.

**Estimated scope:** M policy/tooling slices.

**Rollback:** Restore the prior tier assignment; never waive missing evidence.

### Checkpoint G12 — Complete

- No normal action depends on patrol, full scan, or unrelated I/O.
- Durable intent/commands survive loss of every in-memory component.
- Every destructive action is witnessed and fenced.
- Incremental projections are independently audited and self-repairing.
- Supported scale/HA profiles pass fault, rollback, and provider/store
  conformance.
- Legacy writers, mirrors, migration gates, and obsolete loop machinery are
  deleted.
- Operator SLOs, diagnostics, and runbooks are live and exercised.
- The protected contract lineage is current, P12.9's permanent evidence set is
  within budget, and authorization/store-restore/schema gates are exercised.

## 26. Cross-cutting verification and certification program

Verification is an implementation subsystem, not a final testing phase. Every
layer proves a different claim:

1. Pure laws prove decision totality, determinism, ordering, and fixpoint.
2. Component conformance proves the store, feed, queue, cache, runtime, and
   worker boundaries.
3. Differential tests protect the strangler migration against behavior drift.
4. Crash/re-exec tests prove that durable truth reconstructs disposable state.
5. Anti-entropy tests prove independent discovery and repair.
6. Performance profiles prove bounded work and latency under admitted load.
7. Canary telemetry proves the assembled production path and drives automatic
   aborts.

Passing one layer never substitutes for another. In particular, a green shadow
comparison cannot prove crash recovery, and a soak that did not panic cannot
prove convergence.

### 26.1 CI and soak tier matrix

| Tier | When | Required coverage | Required artifact |
|---|---|---|---|
| VT0 — task-local red/green | Every implementation slice | New failing characterization/contract test; focused package tests; formatter/static checks | Test name linked to the task's acceptance criterion |
| VT1 — pull request fast | Every PR | Pure-core laws; codecs; key mapping; queue wrapper; status writer; AST ownership/effect guards; bounded-state smoke enumeration; deterministic in-process `INV-*` proofs; `make test-fast-parallel` where applicable | JUnit/test log, invariant registry coverage, plus operation-count diff |
| VT2 — pull request targeted | Every reconciler/store/runtime PR | Touched package process tests; every derived before/after seam fault for touched owners using fakes; targeted `-race` for cache, queue, executor, worker, and runtime packages; relevant store/provider conformance; docs check for changed prose | Race/conformance/crash-point report and replay seed for every generated failure |
| VT3 — protected-branch merge | Every merge | `make test-fast-parallel`, `make test-cmd-gc-process-parallel`, affected `make test-integration-shards-parallel` shards, `go vet ./...`, effect inventory, differential corpus, and `make check-docs` | Shard reports, inventory diff, parity summary |
| VT4 — nightly entropy | Nightly | Seeded state-machine histories with shrinking; real-process/SIGKILL variants of every applicable crash boundary; real isolated DoltLite; fake/subprocess/tmux providers on explicit sockets; 100K-object scale, retry storm, hung provider, cache corruption | Seed/event history, causal trace, goroutine/process dump, store snapshot, metrics snapshot |
| VT5 — release candidate | Every RC | `make test-local-full-parallel`; N/N-1 binary matrix; rollback drill; 1M-object rebuild/audit profile; supported provider/store matrix; multi-process split-brain suite if HA is advertised; long soak | Signed profile report, rollback transcript, SLO report, support matrix |
| VT6 — production canary | Every owner flip | One low-risk city/action family; live T0–T11 latency; owner exclusivity; command conservation; feed/index/audit health; automatic abort rules | Scrubbed evidence bundle and explicit promote/abort decision |

Wall-clock latency thresholds run only on pinned, otherwise-idle performance
workers. PR CI uses deterministic operation counts, allocations, virtual time,
and channel barriers. Any cold-cache experiment uses an isolated temporary
`GOCACHE`; it never clears the shared Go build cache.

A red VT4 run freezes owner flips and merges touching the implicated packages
until its seed is minimized, retained, and fixed behind a deterministic VT1–VT3
fixture. A red VT5 blocks the release candidate. The freeze is a checked CI state,
not an advisory dashboard signal.

Every randomized failure stores the original seed, minimized transition list,
binary commit, config, provider/store profile, and virtual-clock history. A
failure becomes a permanent named regression fixture before its fix merges.

### 26.2 Exact crash-boundary matrix

The start, stop, nudge, status, pool-allocation, control/formula, and order
suites inject failure at each boundary below. Cross-store actions additionally
inject between each authoritative store commit in both orders. Except B0, every
case reconstructs a fresh controller with empty
queues, caches, indexes, timers, expectations, and admission state. Durable
store/runtime state is the only input retained.

| ID | Boundary | Required behavior after crash/restart |
|---|---|---|
| B0 | Before durable intent/command commit | No provider effect; no terminal success; request may be safely retried |
| B1 | Commit returned, before same-process application | Feed detects the commit; audit is the bounded final fallback; action remains attributable to the committed `intent_generation`/operation ID |
| B2 | Same-process application, before/during later feed duplicate | One semantic cache state and one dirty key; duplicate delivery is observable but harmless |
| B3 | Feed receive, before cache application/cursor advance | Resume/replay applies the change; cursor never advances past unapplied data |
| B4 | Between object mutation and secondary-index publication | Readers observe the old immutable projection or the complete new one, never a mixed projection; restart rebuilds both |
| B5 | Projection published, before key enqueue | Startup actionable-key reconstruction or audit enqueues the key; no effect is inferred from cache state alone |
| B6 | Key enqueued, before dequeue | Queue loss is harmless; durable mismatch/pending command reconstructs the key |
| B7 | Dequeue, before latest-state reread | No provider effect; `Done`/restart cannot erase durable actionability |
| B8 | Latest-state read, before attempt/expectation persistence | Reconcile rereads and replans; stale snapshots/actions are never resumed as payloads |
| B9 | Attempt/claim persisted, before provider entry | Recovery inspects current `intent_generation`, runtime, and operation identity; it retries the same idempotent operation or safely releases the unentered claim |
| B10 | Provider accepted/effected, response lost or deadline elapsed | Targeted observation adopts the exact incarnation/outcome; dedup/fence handles retry; a still-running non-cooperative call blocks overlapping mutation |
| B11 | Provider returned, before status/command outcome commit | Observation plus operation ID reconstructs the outcome; stale `intent_generation` cannot commit; edge command remains pending or in flight, never disappears |
| B12 | Status/outcome committed, before event publication | Domain state remains correct; no rollback or duplicate provider call occurs; any required delivery-grade notification must come from a durable outbox/change record rather than assumed event replay |
| B13 | Event fired, before queue `Done`/`Forget` | Reconcile may repeat, but level effects are idempotent and edge commands see their terminal outcome |
| B14 | Matching observation applied, before expectation cleanup | Next reconcile clears/reconstructs disposable expectation state and emits no duplicate effect |
| B15 | Lease lost after plan/read, before provider entry | HA-only: provider and status fences reject the old epoch even if the process resumes later |
| B16 | Lease lost after provider acceptance, before status commit | HA-only: runtime is observed, but old-epoch status cannot commit; new owner adopts only through the provider-recognized operation/incarnation |
| B17 | Cross-store commit A succeeded, before B; repeat with B before A | The completed side is durably discoverable and the shared operation/idempotency key resumes or safely compensates the missing side; no child/work/assignment is duplicated or silently stranded |
| B18 | Control/order root or child transition committed, before counterpart materialization/tally/marker | Fresh dispatch reconstructs the exact open transition; closed roots never strand dispatchable children; marker/event retry cannot duplicate child creation or tally |

Fault injection also covers every partial metadata-body write and permutation,
transaction rollback, commit-with-lost-response, disk-full/fsync failure, panic,
`os.Exit`, `SIGKILL`, event-recorder failure, and process restart. B4 is a design
gate: an implementation that mutates a published cache and indexes in place
cannot pass.

### 26.3 Action-specific crash outcomes

| Action | Required safety result | Required liveness result |
|---|---|---|
| Start | No stale `intent_generation` confirms current status; no provider profile is certified unless deterministic identity/idempotency prevents two authoritative current incarnations | Latest desired-running `intent_generation` eventually has one observed current incarnation when dependencies/provider recover |
| Stop/close | Only the exact witnessed incarnation can be stopped; unknown/partial observation and name reuse never close a replacement | Latest desired-absent generation eventually observes absence and then closes/releases work |
| Drain | Stale acknowledgement/cancel cannot cross generation; timeout alone never proves runtime absence | Durable drain resumes at its explicit state and reaches cancel, stop, or acknowledged terminal outcome |
| Nudge | Command is conserved; wrong incarnation receives zero deliveries; dedup-capable providers accept one command ID at most once | Every accepted command reaches `delivered`, `injected_unconfirmed`, `delivery_unknown`, `expired`, `superseded`, or `dead_lettered`; `delivery_unknown` is allowed only after ambiguity cannot be resolved or safely deduplicated/retried |
| Pool allocation | A stale pool `intent_generation` cannot publish; concurrent owners cannot reserve the same stable slot | Desired allocation reconstructs and enqueues each missing/excess session key without direct provider mutation |
| Control/formula dispatch | One operation/idempotency key prevents duplicate child materialization, dispatch, tally, or finalize across every cross-store ordering | Open roots/steps rediscover the missing transition; a closed root cannot strand a dispatchable child or require session memory |
| Order | Duplicate schedule/event hints create at most one durable run identity; stale `source_revision` cannot publish a newer order state | Due/open order work reconstructs after timer/event loss and every root/child commit boundary |
| Owned status | Torn, old-marker, or revision-conflicted status decodes `Unknown`; foreign data is never blindly overwritten | Complete status can be retried/healed idempotently after store recovery |
| Event/trace | Failure cannot undo a committed fact or authorize an effect | Current state remains discoverable; delivery-grade consumers use a separately proven durable stream |

### 26.4 Entropy scenario matrix

Each row receives a stable `ENT-*` scenario ID in the executable manifest. The
final assertion is quiescent convergence and projection equality, not merely
process survival.

| Scenario | Primary recovery owner | Required proof | Required seam/profile |
|---|---|---|---|
| All same-process hints dropped | Durable feed or audit-only bounded relist, then audit | Commit is detected and reaches provider without manual poke within the named profile bound | Fake acceptable at VT1; every certified store at VT2/VT4 |
| Every hint duplicated | Revisioned applier and stingy queue | One semantic projection; bounded queue memory; no duplicate level effect | Fake acceptable; real feed/relist adapter conformance |
| Old local/feed update arrives after a newer update | Source-owned sequencing/lineage or typed gap/reread, plus tombstones | A known-older update cannot regress the projection; incomparable order never guesses | Fake ordering plus real store cursor/revision seam |
| Watch disconnect/reconnect | Watch manager | Resume after applied cursor without list/watch race | Real selected watch/feed adapter |
| Cursor compaction/history gap | Watch manager plus projection replacement | Incremental application pauses, consistent relist replaces all indexes, then watch resumes | Real selected store feed/history seam |
| Cache object or reverse index corrupted | Independent anti-entropy reference | Exact mismatch repaired; smallest affected key set enqueued; repair is loud | Fake corruption plus real store raw enumeration |
| Store list is partial/stale/errors | Snapshot contract and fail-safe projection | No removal/destruction; source remains unsynced/unknown and retries | Fake fault pinned to each real store conformance |
| Authorized external domain/work `bd`/sqlite/Dolt mutation | Durable feed or audit-only bounded relist, then audit | Same typed projection/key mapping as in-process mutation within the named profile bound | Real bd-sqlite, BdStore/bd-Dolt, NativeDolt, or DoltLite; local/authorized profile |
| Untrusted external protected-command mutation | Authorization/credential boundary | Command is denied/audited with zero claim/provider entry; unrelated work mutation remains allowed | Real credential/ACL seam; hosted/multi-tenant profile |
| Sanctioned store restore from older sqlite/Dolt backup | Restore admission owner | Every recovered command/effectful intent remains frozen until explicit re-anchor/reissue above retained high-water | Real sqlite and Dolt restore drill at VT5 |
| External `bd` binary/store-schema upgrade or downgrade | Store version fence | Mismatch is caught below decoder and selects observation-only even for well-formed wrong rows | Pinned real `bd` N/N-1 × store matrix |
| Runtime census false-negative/stale/errors | Tri-state observer plus targeted probe | `Unknown` cannot mint a death witness or destructive plan | Fake fault plus real isolated tmux/process seam |
| Tmux server restarts while tagged agent process survives/reparents | Provider-scope identity plus complete `GC_SESSION_ID` process scan | `RuntimeScopeLost`, no DeathWitness/work release; explicit adopt-or-supervised-stop/escalation preserves work | Real isolated tmux socket + real process scan |
| Human attach/copy-mode races key injection | Runtime observation plus interaction policy | Default park/refusal or explicit same-operation force exactly matches RC-OBS-006; zero silent interleave | Real isolated tmux attach/copy-mode |
| Controller injection overlaps activity observation | Observation manager attribution | Coarse timestamp yields `activity_unknown`; only origin-aware/output evidence classifies self-echo | Real isolated tmux plus capable structured provider |
| Out-of-band runtime start/stop/replacement | Runtime observer/audit | Exact stable key is enqueued; incarnation changes; stale effect is rejected | Real provider seam per certified profile |
| Rapid start→stop→start `intent_generation`s | Intent fence and retry reset | Only latest intent can enter/commit; obsolete delay is canceled | Fake/model plus one real provider |
| Nudge concurrent with stop/restart | Durable command plus shared session executor | No wrong-incarnation delivery; terminal outcome is explicit | Fake barriers plus real provider nudge seam |
| Non-cooperative provider call mutates after timeout | Actual in-flight permit plus provider fence/isolation | No overlapping same-key mutation; unsupported provider cannot enter high-risk profile | Real subprocess/SIGKILL or provider-native operation lookup |
| Provider outage/recovery or hung key | Provider circuit/admission plus expectations | Other providers/keys progress; recovery probes and resumes without blind replay | Fake fault pinned to real provider failure modes |
| Store outage/conflict/partial status write | Conditional/torn-detectable writer | No pre-commit provider effect; conflict rereads; partial status is `Unknown` | Real store conformance per enabled writer |
| Write watermark sees token rollback/incomparability or gap relist | Authoritative serialized reread/snapshot installer | Full reread/relist clears the correct watermark; no-token profile installs none; age alarm prevents a silent parked key | Fake/model plus real no-token/revisioned store profiles |
| Retry storm plus fresh interactive work | Generation-aware delayed retry plus separate controller queues; add a reserved permit/cap only if the pinned starvation case requires it | Fresh work meets SLO; retry and maintenance still eventually progress | Deterministic model; pinned capacity profile |
| One rig floods unique keys | Standard queue and provider/session admission first; evidence-triggered per-flow cap only if required | Quiet rig retains capacity under the certified admitted workload; accepted durable work is not dropped | Deterministic/performance profile |
| Controller restart while queues are saturated | Startup snapshot/feed and actionable-key rebuild | Empty in-memory state reconstructs all latest needs with bounded admission | Re-exec process + real store snapshot |
| Fresh intent/audit dirties a condition-parked key | Unified queue/park state | Key is re-admitted atomically, reevaluates latest facts, and cannot be swallowed behind the old park | Deterministic model/fake acceptable |
| Composite holds overlap on one key | P5.11 composed blocked-state model | Every state has an admissible wake path; owned resolution is bounded and external blockers alarm | Deterministic model plus provider/policy fault conformance |
| Keyed and legacy families mutate one session during migration | Shared physical status writer and per-session effect executor | No whole-blob lost update or overlapping native effect; every legacy-read marker remains compatible | Re-exec mixed-owner process + real provider canary |
| Cross-store control/order operation crashes after either first commit | Durable operation/idempotency key plus B17/B18 reconstruction | Missing counterpart resumes without duplicate child/tally/finalize or stranded work | Real configured split-store topology |
| Forward/backward clock jump or requester-fence controller restart | Qualified durable deadlines and injected boot/wall clocks | No obsolete timer effect or blind fence release; due work eventually wakes; reboot requires exact requester absence | Deterministic clock model plus re-exec/boot-ID fixture |
| Event/trace publication fails or blocks | Non-interfering observer boundary | Durable mutation result is unchanged; failure is counted and diagnosable | Fake recorder plus configured real recorder |
| Mixed-version unknown status/command field | Total decoder and additive schema | Older reader preserves/parks safely; no normalization/destruction of newer state | Real pinned N/N-1 binary/store copy |
| Audit crashes midway or repeatedly sees same mismatch | Disposable partition traversal plus escalation | Traversal restarts safely; repeated repair alerts; no progress file is trusted | Re-exec + real raw store enumeration |
| Stale nudge poller files/legacy sidecar exits | P6.8 keyed ownership | Files have no ownership meaning; pending commands still converge | Re-exec process; isolated runtime root |
| Two controllers overlap before HA | Single-owner preflight | Second executor refuses effects | Two-process/two-directory/one-store real topology |
| Old shard owner resumes after HA handoff | Lease epoch plus provider/status fencing | Zero stale-epoch runtime or status effect | Real selected HA store/provider topology |

### 26.5 Upgrade, downgrade, and owner-handoff matrix

| Case | Required result |
|---|---|
| N reader/controller with N additive data | Normal operation and round-trip preservation |
| N-1 reader/controller with N status/command fields | Safely ignore/preserve or decode `Unknown`; never destructively normalize |
| N controller with N-1 store/provider | Capability resolution selects an explicit compatible profile or fails closed |
| Rollback with pending start | Stop keyed admission, observe/adopt ambiguous start, prove no keyed writer, then enable legacy |
| Rollback with pending drain/stop | Preserve generation/witness; do not recreate a stopped replacement or enable dual stop owners |
| Rollback with pending or in-flight nudge | Recover or expire lease/claim; conserve command before legacy claim owner starts |
| Change-feed schema upgrade/downgrade | Old reader ignores additive records or a tested rollback barrier/migration is declared |
| Conditional-write `off`/`warn`/`require` transition | One resolved mode per store; HA refuses anything except proven `require` |
| Mixed lifecycle vocabulary | New value remains raw+unknown under old binary and is never quarantined/closed |
| Trace/flight-recorder version skew | Current incident tools read required history or the old recorder remains authoritative |
| HA lease/shard version change | Stable hash golden vectors and epochs prevent double ownership; downgrade collapses to one fenced owner |

P12.7 executes this matrix on every release candidate. Any case that cannot be
made backward-compatible declares a visible rollback barrier before rollout;
the barrier is never discovered during an incident.

### 26.6 Performance and scale profiles

| Profile | Shape | Gate |
|---|---|---|
| PERF-OPCOUNT | One session/work/nudge delta among 1, 100, 1K, 10K, and 100K keys, plus 1M on RC workers | Hot-path store reads, probes, index changes, enqueues, and allocations remain constant in fleet size except the true affected fan-out |
| PERF-IDLE | Large quiescent city for many audit intervals | No periodic fleet work on action queues; audit stays within its configured I/O/CPU budget; zero repairs |
| PERF-LOCAL-LATENCY | Local durable commits for start, stop, nudge, pool wake, and control dispatch with available capacity | §10 p50/p95/p99; no hidden blocker time; T0–T11 stages complete |
| PERF-EXTERNAL-FEED | Direct external writes through real isolated DoltLite | Commit-to-detect and detect-to-provider are reported separately; no socket/event shortcut is required |
| PERF-BURST | A configurable fraction of keys becomes dirty at one commit boundary | Queue memory is bounded by unique keys, work is coalesced, admission stays bounded, and oldest-age classifications remain correct |
| PERF-FAIRNESS | One flow supplies most keys/retries while a quiet flow submits interactive work | Quiet-flow p99 stays within its certified target; noisy and maintenance flows still progress through aging/reservation |
| PERF-HUNG | One or more providers ignore cancellation and/or return slowly | Actual in-flight calls remain within provider limits; unrelated providers/keys retain capacity; circuit opens before total exhaustion |
| PERF-REBUILD | Empty process state over 100K nightly and 1M RC objects | Snapshot/watch sync, memory peak, key reconstruction, and recovery time are bounded and recorded; effects wait for required sync |
| PERF-AUDIT | Full partitioned reference traversal over 100K nightly and 1M RC objects | Every partition covered; per-window budget honored; no enqueue herd; zero healthy repairs |
| PERF-HA | Warm follower takeover and virtual-shard rebalance under admitted load | Feed lag and failover RTO measured; no stale effect; unaffected shards retain latency |

P0.7 pins hardware, store size, provider fakes/real providers, concurrency, and
test duration for each certified profile. Certification operates below a
measured saturation point with explicit headroom; it does not extrapolate from
an idle microbenchmark. At and above admission capacity, the required behavior
is bounded memory, explicit durability/overload classification, preserved safety,
and recovery—not an impossible unchanged latency promise.

No benchmark may hide a full scan behind a cache warmup. Operation-count guards
instrument store `List`/`Get`, runtime census/probe, index traversal, queue adds,
provider entry, and allocations for the complete causal path.

### 26.7 Automatic rollout abort conditions

Safety thresholds are zero-tolerance and abort immediately. Performance/health
thresholds use windows established by P0.7 and stored with the rollout gate.

| Signal | Abort threshold | Automated response |
|---|---|---|
| Unwitnessed destructive attempt | Any | Fence the action family; stop new keyed admission; preserve evidence; do not enable another writer until in-flight state is resolved |
| Wrong generation/incarnation/epoch provider entry | Any | Immediate family/city abort and provider circuit open |
| Dual action owner or dual command claimer | Any | Stop both new admissions, prove exclusive ownership, then select the last known-safe owner |
| Accepted command conservation mismatch | Any missing or multiply terminal command | Stop nudge canary; snapshot command ledger and provider acknowledgements |
| Torn status trusted or conflict overwritten | Any | Stop all status-owning keyed families for the store; force safe read-only/unknown projection |
| Effect inventory/completeness gap | Any production effect without the canonical owner/context registration | Block merge or freeze the affected canary before trusting shadow results |
| Unexplained fact/plan/effect shadow diff | Any in the canaried family | Hold promotion; revert family owner after exclusive handoff |
| Projection/reference mismatch | Any unexplained mismatch in an owned projection; any repair burst over the approved baseline | Pause affected owners, repair/relist, retain feed and audit, investigate mapper/applier |
| Cursor regression/fork or unsynced required source | Any untyped regression/fork; repeated typed gap/relist without forward progress; sync age over profile bound | Block effect owners that need the source; force gap/relist; local unrelated stores continue |
| Ready-key/SLO burn | Under the certified envelope, oldest unclassified ready key over 1s or p99/error-budget burn over configured window | Stop expansion; reduce admission/restore prior owner if safe; capture stage breakdown |
| Queue/memory/provider permit exhaustion | Profile bound crossed or permits remain charged past circuit threshold | Reject/retain work according to durability contract; open provider/flow circuit; never spawn unbounded goroutines |
| Anti-entropy cannot complete | Traversal misses its bound or repeats the same repair | Keep action safety gates, alert, and force scoped authoritative rebuild |
| HA lease/fence anomaly | Any overlapping valid lease, stale-epoch acceptance, or unbounded follower lag | Collapse to one fenced owner; HA profile disabled |
| Coordination working-set/epoch/revision regression | Any UUID mismatch, lower previously-seen lease epoch/revision, or forked working set | Stop all HA admission immediately; retain evidence; collapse only after one authoritative working set and fenced owner are proven |

Abort is a tested state machine:

1. Freeze promotion and stop new admissions for the affected keyed owner.
2. Leave observation, feed, trace, and anti-entropy running.
3. Resolve or fence every in-flight/ambiguous provider operation and durable
   claim.
4. Prove the keyed writer is inactive through the effect boundary.
5. Enable the prior owner only if its reader is compatible with current data.
6. Retain a bounded scrubbed evidence bundle and open a blocking incident task.

Fallback is never “turn legacy back on” while the new writer may still act.

## 27. Delivery strategy, critical path, and parallel work

### 27.1 Merge-unit discipline

The `P*` entries are architecture-level tasks. Before a phase begins, its tasks
are decomposed into implementation beads that each:

- fit one focused red/green/review session;
- touch no more than about five hand-edited files total; generated outputs may
  accompany their source without hiding a broader hand-written change;
- change one contract, one action family, or one provider/store implementation;
- leave the default path runnable and all prior checkpoints green;
- name its rollback and the exact owner before/after merge;
- update the relevant `SESSION-*` rows and effect exception inventory in the
  same change.

Any task labeled `Epic`, “Multiple M,” or “M per provider/family” is an umbrella
and may not be assigned as one PR. Its child beads and exact dependency edges are
enumerated before implementation begins. Deletion PRs do not mix with behavior
changes. Generic scheduler abstractions wait until two domain controllers exhibit
the same tested mechanics.

### 27.2 Parallel workstreams

| Workstream | Scope | May run beside | Merge constraint |
|---|---|---|---|
| A — baseline and governance | P0 inventory, corpus, SLOs, migration gate | Nothing initially; then all streams consume it | G0 ratified first |
| B — fail-safe runtime safety | P1 tri-state observation, incarnation, witnesses | C, D, E | Ships independently; cannot weaken legacy safety |
| C — session semantic extraction | P2 pure decisions and canonical projection | B, D, E | One decision cluster at a time; requirements ledger is authoritative |
| D — conditional/status writes | Existing CAS branch plus P2.9 | B, C, E | Rebase/port smallest proven slice; whole-status ordering proven before writer cutover |
| E — change capture and cache | P4 spike, contract, provider feed, watch manager | B, C, early F | Backend contract precedes production implementation |
| F — differential/model verification | P3 effect interception, reference model, parity | B–E | Completeness gate before any production effect cutover |
| G — queue/executor mechanics | P5 keys, queue, supervisor, admission, retry | Late C, E, F | First-family no-effect shadow requires G1/G3/G4A; G4B gates only feed-certified claims/removal |
| H — provider/store conformance | P1.8, P4.5, P6.4, later P11.5 | B–G by provider | One provider/store per PR; unsupported capability is explicit |
| I — operations | Metrics, trace, doctor, runbook scaffolding | Every phase after type contracts stabilize | Observability must precede its corresponding canary |

The current conditional-writes branch and Windshield worktree are evidence and
source material, not merge bases to combine blindly. Each slice starts from the
current integration head, uses feature archaeology, and ports the smallest
proven change behind existing boundaries.

### 27.3 Promotion waves and critical path

```text
G0 baseline
 ├─ G1 fail-safe observation/witnesses ───────────────┐
 ├─ G2 session core/status writer ────────────────────┤
 ├─ G3 complete family differential oracle ──────────┼─ G5A first-family shadow ── G6 nudge
 └─ G4A local capture + bounded relist ───────────────┘                  │
      ├─ G4B optional feed certification ─ external-write SLO/removal   │
      └─ G5B shared scheduling capacity ────────────────────────────────┤
                                                                          │
                                                           G7 lifecycle cutover
                                                   │
                                                  G8 pool/demand cutover
                                                   │
                                                  G9 city-loop split
                                                   │
                                                  G10 anti-entropy certification
                                                   ├─ G12 single-owner completion
                                                   └─ G11 optional HA → G12 HA profile
```

| Wave | Work | Parallelism rule | Human checkpoint |
|---|---|---|---|
| 0 | P0 | Inventory, semantic decisions, and baseline are sequential enough to establish one execution head | Approve G0 and unresolved spike questions |
| 1 | P1, P2, P3 foundations, P4.1–P4.2, existing CAS graduation, provider conformance | Separate packages/owners may proceed; status schema and action vocabulary are coordinated contracts | Approve G1/G2 and feed choice |
| 2 | Complete family-specific P3, G4A, optional G4B, and P5 no-effect mechanics; begin P6 nudge-specific slices when their exact dependencies pass | Oracle/feed/provider work parallelizes; unrelated lifecycle extraction and G4B do not block the local nudge canary, but no owner flips before the family oracle/handoff gates | Approve G3, G4A/G4B, and G5A/G5B independently; approve G6 after its canary and rollback drill |
| 3 | Finish P6 nudge and prepare P7 | One command owner flip at a time; session families remain shadow-only | Confirm G6 before the first lifecycle provider-effect canary |
| 4 | P7 lifecycle | One action family at a time; destructive stop/close last | Approve each P7.13 family, then G7 |
| 5 | P8 plus eligible P9 child services | Pool classes flip one at a time; non-session maintenance isolation may proceed if it shares no owner | Approve G8 and G9 separately |
| 6 | P10 | Reference builders may parallelize by projection; repair/apply contract is shared | Approve G10 after entropy/nightly evidence |
| 7 | P11, only if entry gate is met | Provider fencing work parallelizes by provider; no active-active action until all seams pass | Approve each certified HA topology |
| 8 | P12 | Delete one unreachable family per PR; no simultaneous migration experiments | Approve release rollback barrier and final profiles |

At most two non-overlapping action-family migrations may exist, and only one
destructive family may be in canary. Phase numbers do not authorize skipping a
checkpoint dependency.

### 27.4 Checkpoint evidence packet

Every applicable checkpoint review—G0–G3, G4A/G4B, G5A/G5B, and G6–G12—includes:

- exact commits and current owner table;
- scenario/requirements coverage diff;
- effect-site inventory and temporary exceptions;
- component conformance and differential summaries;
- crash/entropy seeds and failures, including fixed regressions;
- operation-count and latency comparison to the previous checkpoint;
- feed/index/audit health and any repair explanation;
- upgrade/rollback drill result;
- remaining risks, unsupported profiles, and next rollback command.

No checkpoint is approved from a prose assertion or aggregate “tests green”
badge alone.

### 27.5 Stop-with-value landing points

G12 is the complete target, but the program never needs to keep two partially
owned systems alive merely to preserve sunk cost. At three explicit reviews the
current state is independently supportable and the remaining program is
re-costed:

| Landing point | Banked value | Required cleanup before pausing | What remains non-final |
|---|---|---|---|
| After G1 | Fail-safe observation, no destructive error→absence, partial-scan and current performance bug fixes | Remove unused additive scaffolding; keep legacy sole owner | Global latency/scans and entropy bounds remain legacy |
| After G6 | Keyed durable nudge with exact owner, explanation surface, canary/rollback proof | Complete nudge-specific P12.1 deletion/compatibility window; no dormant second claimer | Lifecycle/pool still global; full anti-entropy target not reached |
| After G7 | Keyed session lifecycle with one physical writer/executor and family deletion windows | Delete graduated legacy families only within abort-coupling domains; retain bounded relist/audit | Pool/global services and G10 permanent entropy certification remain |

At each landing review, publish actual bead count, elapsed effort, incident/SLO
change, remaining cross-repo dependencies, next release/soak cost, and a
continue/pause decision. Pausing means one production owner and closed migration
experiments, never an indefinite shadow/dual-writer state. These are safe
intermediate products, not permission to claim the Phase 12 guarantees.

### 27.6 Park-state and collapse table

A release may remain indefinitely only in a state with one physical writer,
one provider-effect owner per family, closed compatibility bounds, and an owned
verification/operations cost. Transitional states have wall-clock and evidence
budgets and a pre-drilled collapse action.

| State | Safe to hold? | Maximum exposure / required signals | Collapse action |
|---|---|---|---|
| Legacy sole owner plus P1.0A/P1.0C/P1.0D/P1.1A fixes | Yes | Existing support envelope; G0 work may pause; acknowledgement ambiguity is fail-closed and closed-root discovery remains explicitly uncertified | Remove unused additive scaffolding; keep fixes and legacy owner |
| No-effect isolated shadow | Yes while within resource budget | Shadow CPU/heap/FD/store budget; no claims/shared queue/timer mutation | Disable shadow child; no handoff needed |
| One P5.4A family bridged but still legacy-decided | No | Short canary window; mutation-stall age, charge age, zero dual entry; cold unbridge proven before second family | Stop admission, drain/fence, exit bridge owner, cold-select compiled legacy adapter |
| Keyed nudge owner after G6, legacy disabled but retained | Yes through compatibility window | Command quality/conservation, audit completion, N/N-1 and rollback drills | Cold handoff to legacy before deletion, or complete nudge P12.1 cleanup and park at G6 |
| Mixed P7 family ownership | No indefinite hold | At most two non-overlapping migrations and one destructive canary; checkpoint forecast/abort thresholds | LIFO cold rollback to last approved owner set, or roll forward to next G7/P12.1 park state |
| All session families keyed after G7, legacy unreachable but retained | Yes through deletion windows | Per-family rare-event/rollback evidence and one physical status writer/executor | Complete safe P12.1 deletions within abort-coupling domains or cold rollback before incompatible deletion |
| Pool/P9 partial split | Only when each flipped domain has one owner and no shared shutdown/status abort coupling | Explicit owner table, cross-store B17/B18 evidence, full P5.11 shutdown combinations before G9 | Roll back the last independent domain or finish G9 composition; never leave a provider-global half-owner |
| HA feasibility/follower shadow only | Yes, read-only | Zero claim/write/provider effect and bounded lag | Disable follower; single owner unchanged |
| Partial HA provider/store enablement | No | One certified topology at a time; lease/fence anomalies zero | Collapse all shards to one fenced owner |

Every transitional release records entry time, expiry, owner, abort command,
and the exact evidence needed to leave the state. Expiry freezes expansion; it
does not flip ownership automatically.

## 28. Risk register

| Risk | Early signal | Mitigation and stop gate |
|---|---|---|
| Durable change feed cannot cover direct external writes | Spike misses a committed mutation or cannot establish snapshot cursor | Stop at P4.1; retain local fast path plus measured audit profile; do not claim external low-latency or cut global discovery |
| Per-row revisions are mistaken for a global feed cursor/consensus | Cross-object ordering or branch merge produces ambiguous sequence | Keep contracts separate; HA/feed eligibility fails closed |
| Status batch is torn or marker order is assumed | Fault injection finds a marker certifying partial body | P2.9 blocks cutover; use atomic conditional whole-row write or separate checked marker operation only |
| Effect inventory is incomplete | New direct provider/store/process call bypasses recorder | AST/import gate fails merge; no shadow parity credit until registered |
| Differential oracle shares production bugs | Legacy/new agree but independent model or incident fixture disagrees | Maintain independent model/reference builders and real effect interception |
| Runtime provider lacks strong incarnation identity | Name/PID reuse or response-loss test cannot distinguish replacement | Provider gets weaker profile; destructive HA and high-risk certification are refused |
| Provider ignores cancellation and mutates late | Timeout test sees effect after caller returned | Keep actual permit/key ambiguous; require fencing or killable isolation; circuit provider before capacity exhaustion |
| Queue becomes a second work ledger | Command/action data or correctness-only state appears in queue payload | Static/type review: keys only; delete queues and prove rebuild in every crash suite |
| Cache/index lost-wakeup race | Blocker changes between check and registration | Register with source revision, recheck, and test the exact interleaving |
| Incremental mapper and audit share the same traversal/applier bug | Corruption is reproduced in both outputs | Independent raw enumeration/fold with canonical decoder/contributions only, import guard, structural comparison, and deliberately seeded incremental omissions |
| Anti-entropy creates a thundering herd | Audit spikes store I/O or enqueues fleet | Partition, page, jitter, budget, and enqueue keys only; PERF-AUDIT gate |
| Fairness/admission becomes overengineered or starves a controller | Quiet-flow latency or maintenance age grows unbounded | Start with standard workqueues, shared session exclusion, and provider semaphores; add one measured cap/reserved permit only for a reproducible starvation case; no generic policy engine |
| Dual writers during cutover/rollback | Two effect records/claims for one family | Mechanical owner table, stop/drain/prove protocol, zero-tolerance abort |
| Hidden mid-pass/global-builder semantics break when parallelized | Differential output depends on iteration/completion order | Projected-world/model tests; move cross-key ownership to pool/dependency controllers before deleting folds |
| Mixed binaries destructively normalize new state | N-1 writes over unknown field/value | Additive schema, total unknown decode, N/N-1 matrix before every default flip |
| PID/status files recreate stale ownership | Reaper or stale file changes delivery behavior | P6.8 deletion and path guard; live process discovery only |
| Metrics/traces overload or leak customer data | Cardinality/scrub budget test fails | Bounded labels, sampled/budgeted traces, fixture redaction, evidence bundle limits |
| Performance test is flaky or hides store delay | Wall-clock PR variance or inferred commit times | Operation-count PR gates; pinned performance workers; explicit unknown timestamps |
| HA lease is treated as fencing | Paused owner acts after lease transfer | Provider-recognized epoch test is mandatory; unsupported provider stays single-owner |
| Sharding is added before simpler distribution is exhausted | Single city has no measured capacity need | P11 entry gate; distribute independent cities/rigs first |
| Generic framework increases upstream divergence | Large cross-package abstractions or role/provider names enter generic code | New files/small adapters, zero hardcoded roles, extract only after two consumers, rebase each phase on current upstream |
| Worktree/branch archaeology is mistaken for current truth | Plan task references missing/moved code on execution head | P0.1 current-head inventory and smallest-slice port; never merge worktrees wholesale |
| Scope expansion stalls safety wins | Feed/HA work blocks `Unknown` or witness fixes | P1 ships independently; G1 does not wait for optional scale work |
| Shared work-store credentials mint valid control commands | Unknown/self-asserted principal reaches claim/provider entry | P0.14/P2.11 trusted ingress, claim-time authorization, protected namespace/credential split; hosted profile refuses shared control authority |
| Store restore resurrects old starts/stops/nudges | UUID/restore/high-water regression or recovery drill finds provider entry | P0.15/RC-STORE freeze every effectful recovered intent, re-anchor above independent high-water, deliberate reissue only |
| Unpinned `bd` migrates or writes the wrong schema | Runtime schema/writer version differs while decoders stay green | Below-decoder version fence; observation-only; pinned N/N-1 store matrix |
| Implementer weakens its own gate/evidence | Cited row/threshold/test changes with implementation or packet lacks CI provenance | P0.16 append-only protected delta lineage, independent approval, CI-derived attested packets |
| P5.4A bridge stalls all mutations before value appears | Oldest bridge charge/blocked key grows or cold unbridge cannot complete | One family at a time; compiled cold adapter, mutation-stall abort, soak and unbridge drill before the second family |
| Long mixed-owner window creates more incidents than it retires | Exposure ledger forecast or checkpoint date slips | P0.17 owner-signed incident math, stop-with-value gates, at most two non-overlapping migrations |
| Independently safe holds compose into a wake-free state | Classified blocked-key age grows while every component says healthy | P5.11 composed model, admissible wake-path law, per-class detection/escalation alarm |
| Calendar soak misses rare-event defects before deletion | Required cursor-gap/clock/swap/ambiguity event count is zero | P12.1 occurrence-count gates, synthetic canary injection, retain stop/close/shutdown cold resurrection for two releases |
| Contract hash churn serializes parallel work | Unrelated additive delta marks every bead stale | Base + append-only delta lineage and per-row hashes; only affected consumers revalidate |
| Human attachment/copy mode causes silent interleave or permanent park | Key injection during attach or detached pane remains in mode | RC-OBS-006 family table, explicit force revision, bounded orphan-mode recovery, real isolated tmux tests |
| Configuration knobs make the safe path inoperable | Zero-config small city misses SLO/entropy or knob count rises unchecked | RC-PERF-002/RC-CERT-003 conservative defaults and checkpoint knob/cost reports |

## 29. Bounded unknowns and decision deadlines

These are discoveries with a safe default and a task that closes them. They are
not permission to improvise during a cutover.

| Unknown | Deciding task/checkpoint | Safe default if unresolved | Blocks |
|---|---|---|---|
| Exact DoltLite/Beads no-gap feed mechanism | P4.1 before P4.2 implementation | Unsupported external fast path; local hint plus bounded relist/audit | G4B and every feed-dependent low-latency claim; G4A remains available |
| Pinned Beads conditional-write contract (#4682/revision/CAS successor) and bd-sqlite/bd-Dolt support | P2.0 per-store matrix, decided before G2 and before the first conditional-writer consumer PR | BdStore stays compatibility-only; no emulated `require` | G2 conditional status/claim for that store, every dependent HA profile |
| Which stores can atomically condition a whole owned status | P2.9 conformance | Torn-detectable compatibility writer, single owner only | Status cutover for failing store; all HA |
| Provider incarnation, cancellation, start idempotency, and nudge dedup strength | P1.8/P6.4 provider matrix | Weaker explicit profile; defer unsafe destruction | Provider high-risk/HA certification |
| Canonical migration from current nudge file/sidecar state | P6.1/P6.8 | Legacy remains sole owner; no dual migration | G6 |
| External facts hidden inside arbitrary `scale_check` | P8.7 | Per-pool bounded periodic check with visible staleness | Custom-check pool cutover only |
| Authoritative time and lease behavior | P11.1/P11.3 | One configured controller; no active-active | G11 |
| Current sqlite graph/infra plus Dolt/DoltLite work-store split under HA | Settled by P11.1: consolidate the HA-owned causal path or prove a separately approved atomic fence in every store | Permanently single-owner; a lease in one store never fences another | G11 for the split topology |
| Independent monotonic restore anchor unavailable or identical-backup rewind undetectable | P0.15/P12.7 | Effects frozen; explicit recovery/reissue only; profile uncertified | Every effectful high-risk profile |
| Store/provider cannot separate work-plane credentials from protected command writers | P0.14/P12.8 | Explicit local `store_writer_is_controller` only; hosted profile refused | Hosted/multi-tenant certification |
| Sustainable throughput/concurrency on certified hardware | P0.7 and PERF profiles | Conservative admission; no unmeasured capacity claim | Performance certification, not safety work |
| Event types requiring delivery rather than observation | P0.2/effect inventory | Events remain best-effort observation; durable consumers read state/feed | Any feature claiming guaranteed notification |
| Old-binary compatibility window for poller/schema deletion | Release policy plus P6.8/P12.7 | Retain disabled reader/code, never a second writer | Physical deletion only |
| Whether a smaller flight recorder can replace the current trace WAL | P12.5 reader evidence | Keep current WAL | Trace deletion only |

No unresolved item may cross the checkpoint named in its “Blocks” column. If a
spike disproves the preferred design, the plan is updated and re-approved before
implementation continues.

## 30. Authoritative sources and decision lineage

### Repository sources

- [Windshield proposal](PROPOSAL.md) — typed state, pure decisions, witnesses,
  marker ordering, differential migration, and fail-safe principles.
- [Session requirements](../../../internal/session/REQUIREMENTS.md) — canonical
  behavior ledger; this plan cannot silently supersede a `SESSION-*` row.
- [Session extraction plan](../../../internal/session/PLAN.md) — current
  incremental ownership sequence.
- Conditional-write rollout archaeology (`d87745644` through `9f7d7f2f1`) —
  reviewed revision/CAS branch evidence that P2.0 must revalidate against the
  execution head before porting; it is not a hidden local-plan dependency.
- [Controller architecture](../../architecture/controller.md) — current
  orchestration responsibilities and layering.
- [Reconciler debugging](../../contributors/reconciler-debugging.md) — required
  production trace/evidence workflow.
- [Testing guide](../../../TESTING.md) — test tiers and sharded local runners.
- `engdocs/plans/reconciler-redesign/evidence/` — the original multi-model
  exploration, red-team, and design artifacts. They are supporting evidence,
  not a substitute for current-head code/tests.

The 2026-07-12 production latency segment summarized in §3 contains customer
context and is intentionally not committed raw. P0.6 must produce a scrubbed,
provenance-tagged fixture before it is used as a parity gate.

### External reconciler/platform sources

| Source | Decision taken into this plan |
|---|---|
| [Kubernetes API concepts: efficient change detection](https://kubernetes.io/docs/reference/using-api/api-concepts/#efficient-detection-of-changes) | Consistent snapshot plus resumable watch; typed gap/relist after retained history expires |
| [client-go SharedInformer contract](https://github.com/kubernetes/client-go/blob/877f5359348b5df85619f2aa379abc5bd74bca2d/tools/cache/shared_informer.go#L45-L168) | Eventual cache, per-object order only, handler after cache update, and resync as replay—not an authoritative read |
| [client-go workqueue contract](https://github.com/kubernetes/client-go/blob/877f5359348b5df85619f2aa379abc5bd74bca2d/util/workqueue/queue.go#L190-L302) | Dirty/processing key coalescing, same-key serialization, and cross-key concurrency; commands remain durable outside the queue |
| [controller-runtime Reconcile contract](https://github.com/kubernetes-sigs/controller-runtime/blob/3be3f1bf2b2fcc6b5c9510d55c6a9972294653d0/pkg/reconcile/reconcile.go#L62-L105) | Reconcile identity/current level, not a captured event or action payload |
| [Kubernetes controller expectations](https://github.com/kubernetes/kubernetes/blob/24e2b02af5543d7910c2bb074c7264df5a8f0467/pkg/controller/controller_utils.go#L61-L226) and [consistency store](https://github.com/kubernetes/kubernetes/blob/24e2b02af5543d7910c2bb074c7264df5a8f0467/pkg/controller/util/consistency/consistency.go#L28-L194) | Suppress duplicate work while own writes lag in the cache; expectations expire and never become permanent truth |
| [client-go leader election contract](https://github.com/kubernetes/client-go/blob/877f5359348b5df85619f2aa379abc5bd74bca2d/tools/leaderelection/leaderelection.go#L17-L48) | Election alone is not fencing; stale owners require store/provider-recognized epochs |
| [Kubernetes API Priority and Fairness](https://kubernetes.io/docs/concepts/cluster-administration/flow-control/) | Separate concurrency classes and fair flows rather than assuming FIFO prevents tenant starvation |
| [Nomad scheduling](https://developer.hashicorp.com/nomad/docs/concepts/scheduling/how-scheduling-works) | Separate durable evaluations from broker scheduling; explicitly park blocked work and reawaken it from the blocking condition |
| [Temporal durable execution](https://docs.temporal.io/encyclopedia/durable-execution) | Persist intent/outcomes so process loss can replay decisions; do not treat an in-memory worker queue as history |
| [Envoy xDS protocol](https://www.envoyproxy.io/docs/envoy/latest/api-docs/xds_protocol) | Versioned state/delta application, acknowledgements, and explicit stale/update rejection inform cursor, generation, and projection contracts |

The design deliberately does not copy Kubernetes' global API server, informer
resync cadence, leader-election defaults, or rate-limit constants. It adopts the
semantic contracts that match Gas City's beads/runtime boundaries and verifies
them against the actual providers.

## 31. Final definition of done

The high-risk single-owner reconciler is complete only when all of the following
are true. Optional HA claims additionally require G11 for each advertised
store/provider topology.

### Architecture and maintenance

- [ ] The hot path is commit/watch → typed projection/index → stable key → pure
  reconcile → shared fenced executor; no normal action waits for patrol, full
  desired-state build, or unrelated I/O.
- [ ] Queues contain keys only and can be deleted/rebuilt without losing work.
- [ ] Session, nudge, pool, control/order, and maintenance controllers have
  explicit ownership, retry, and bounded capacity; any fairness mechanism is
  justified by a retained starvation test.
- [ ] `CityRuntime` composes/supervises child controllers and performs no
  unbounded domain work in its ingress loop.
- [ ] Provider/store/T3/DoltLite details remain behind existing boundaries; no
  hardcoded role or model judgment enters Go.
- [ ] Per-session nudge pollers, PID/lock status files, global action scans,
  raw lifecycle mirrors, dual writers, and expired migration gates are removed.
- [ ] Each generic helper has at least two real consumers or remains domain-local.
- [ ] The final diff is rebased on current upstream and its fork-specific seams
  are small, named, and independently removable.

### Safety and recovery

- [ ] Every §8 invariant is executable and green in its owning test layer.
- [ ] Every destructive effect carries a fresh exact-incarnation witness plus
  `intent_generation` and, where applicable, HA `owner_epoch`.
- [ ] `Unknown`, partial, torn, stale, and unrecognized state can only defer and
  emit bounded diagnostics.
- [ ] All B0–B18 crash boundaries pass for every supported action/provider/store
  profile to which they apply (B15–B16 are HA-only), including non-cooperative
  provider timeout; every historical incident has a complete reproducible
  scenario or is explicitly diagnostic-only.
- [ ] Every `ENT-*` scenario converges after faults stop; projections match an
  independently enumerated fresh reference fold and the next plan is empty.
- [ ] Every accepted nudge/edge command is conserved to a visible terminal
  outcome; `delivered`, `injected_unconfirmed`, and `delivery_unknown` remain
  distinct and the advertised quality ratio matches provider proof.
- [ ] Protected commands have trusted requester provenance, claim-time/force-
  specific authorization, and hosted credential separation; policy outage is a
  visible parked unknown with an admissible wake, not denial or execution.
- [ ] Store UUID/restore epoch/schema/writer version is verified continuously;
  sanctioned restore drills freeze and re-anchor every recovered effectful
  intent without replay.
- [ ] Empty caches, queues, timers, expectations, and audit progress reconstruct
  solely from authoritative beads/config/runtime state.
- [ ] Before HA, a second executor fails closed. In HA profiles, stale epochs are
  rejected by both provider effects and owned status writes.

### Performance and capacity

- [ ] The certified profile meets the §10 p99 action-start SLO with every stage
  measured and blocker/durability time classified separately.
- [ ] One-object changes pass constant-operation gates through 100K objects;
  1M-object RC operation/rebuild/audit profiles stay within their ratified
  operation/time/memory/resource budgets.
- [ ] No hung key/provider, retry storm, noisy rig, audit, order, or GC job can
  consume all interactive capacity.
- [ ] Every composite blocked-key state has an admissible wake path; controller-
  owned resolution is bounded and external blockers have bounded detection/age
  escalation with no hot poll.
- [ ] Saturation causes bounded queue/goroutine/memory growth and explicit
  overload/durability behavior, never silent loss or unsafe concurrency.
- [ ] Patrol/audit interval changes do not affect healthy steady-state action
  latency.

### Migration and operational proof

- [ ] All mandatory VT0–VT6 tiers are green for the exact release commit, with
  required commands/budgets/artifacts retained; bounded-model reports disclose
  dimensions, explored state/transition counts, depth, reductions, and zero
  invariant violations.
- [ ] Effect-site completeness is 100%; all shadow differences are explained,
  approved, and expired where temporary; no production effect site is unknown.
- [ ] Every action family and pool class completed canary, automatic-abort test,
  exclusive handoff, and rollback drill before its legacy owner was deleted.
- [ ] N/N-1 upgrade/downgrade cases pass or have a declared, tested rollback
  barrier, including pinned `bd`/store-schema pairs and restore lineage.
- [ ] The G0 base + approved contract delta lineage and cited row hashes pass
  independent approval; evidence packets are CI-derived and provenance-
  verified rather than implementer-authored.
- [ ] Feed cursor lag, queue age, capacity, blockers, expectations, repair,
  ownership, fencing, and T0–T11 latency are visible with bounded-cardinality
  metrics and correlated scrubbed traces.
- [ ] Every advertised store/provider/profile passes its conformance and
  performance matrix; canary/soak includes completed independent audit cycles
  with zero unexplained drift, not merely zero repair counters.
- [ ] Safety telemetry reports zero dual-owner, unwitnessed, stale-fence,
  wrong-incarnation, lost-command, and unexplained-shadow events for the
  certification window and minimum sample count.
- [ ] Operators have exercised evidence capture, feed-gap recovery,
  anti-entropy repair, provider circuit, owner rollback, and—if enabled—shard
  failover runbooks.
- [ ] The certified support matrix states exact store/provider/topology safety,
  latency, delivery, recovery, and failover guarantees plus every exclusion.
- [ ] G0–G3, the applicable G4A/G4B and G5A/G5B gates, G6–G10, and G12
  evidence packets are approved; G11 is also approved for any profile
  advertised as horizontally active-active.
- [ ] No unresolved critical/high review finding remains. Any lower-severity
  waiver has an owner and expiry; legacy deletion and profile certification have
  maintainer and operations sign-off.

The result is not literally infinite or failure-proof. It is scale-independent
on the hot path, horizontally partitionable where the real storage/provider
contracts support fencing, and self-correcting within a measured fault envelope.
That is the strongest honest foundation on which high-risk customers can depend.
