# Reconciler Redesign Acceptance Matrix

| Field | Value |
|---|---|
| Status | Required contract; every implementation slice cites the rows it changes |
| Date | 2026-07-12 |
| Scope | CLI and API requests, runtime observation, tmux/process identity, provider effects, queues, feeds, nudge, close, shutdown, migration, and certification |
| Authority | Complements `internal/session/REQUIREMENTS.md`; where this matrix changes current behavior, the implementation PR updates the permanent scenario ledger at the same time |

This matrix turns the reconciler redesign's safety and compatibility claims into
named, fault-injectable outcomes. A phase cannot pass because its aggregate test
suite is green. It passes only when every applicable row below has evidence at
the required test tier and every intentional behavior change is recorded.

Machine references use one grammar: `RC-CLI-001..005` means the inclusive
same-prefix range `RC-CLI-001` through `RC-CLI-005`; comma-separated IDs mean a
noncontiguous set. The evidence registry expands and validates ranges. Wildcards
and slash shorthand are forbidden. `INV-*` ranges use the same rule.

## 1. Shared result vocabulary

Every mutation is correlated by one durable caller-generated operation ID.
Steps/attempts use deterministic suffixes of that ID unless a provider requires
a separate opaque token. Retries retain the operation ID and never mint a new
logical command or intent generation without new user/domain intent. Process-
local attempt IDs are telemetry only.

Requester provenance is orthogonal to the operation ID and target witness. In a
profile that claims authorization, the authenticated front door—not the
request body—stamps principal, tenant/city scope, credential class, policy
version/decision ID, and issuance time. Those values are immutable command
envelope fields and are revalidated before claim/provider entry.

A single overloaded `outcome` cannot distinguish commit ambiguity, caller wait
timeout, provider ambiguity, and action-specific delivery. The typed result and
additive JSON wire therefore have orthogonal dimensions:

| Dimension | Values | Meaning |
|---|---|---|
| `commit_state` | `not_committed`, `committed`, `unknown` | Whether authoritative durable acceptance is proven. JSON `accepted` is `false`, `true`, or `null` respectively. |
| `completion_state` | `not_requested`, `pending`, `completed`, `failed`, `unknown` | Whether command-specific convergence is proven. JSON `completed` is true only for `completed`; false for known nonterminal/failed; null for unknown. |
| `wait_result` | `not_requested`, `satisfied`, `timeout`, `canceled`, `interrupted`, `output_failed` | Why the caller returned. Timeout/cancellation is not a provider stage. |
| `provider_stage` | `not_entered`, `entered`, `accepted`, `observed`, `unknown` | Last externally proven provider boundary for the current action attempt. `entered` means provider-native mutation entry—the first point after which the effect may have occurred—not merely adapter/worker entry. |
| `action_result` | closed enum per action family | Examples: `target_missing`, `rejected`, `superseded`, `dead_lettered`, `queued`, `ready`, `injected_unconfirmed`, `delivery_unknown`. |

`error_class` is a bounded enum orthogonal to these states. Human output and
exit status are projections of the full result, never the source of truth.
Retry rules are mechanical:

- `not_committed` may retry with the same request ID;
- `commit_state=unknown` must resolve by request ID and cannot mint a new
  generation or provider effect;
- `committed` reconciles from the durable record;
- provider `entered`/`accepted` with unresolved result retains actual-call
  ownership and observes/adopts rather than blindly replaying;
- `superseded` and `dead_lettered` are terminal for that request/action.

## 2. CLI, API, and ownership

### RC-CLI-001 — Managed mutation ownership

For a configured managed city, CLI/API processes may commit durable requests,
query/wait for them, and attach/stream through a proven non-mutating connection.
They may not directly invoke runtime-mutating provider methods. Socket
reachability, a PID/status file, and a successful poke never choose the writer.

Prove controller death after preflight, controller start after preflight, stale
socket, refused socket, unresponsive socket, and process restart from empty
memory. Each case produces one durable request and at most one provider entry.

### RC-CLI-002 — Unmanaged one-shot ownership

An unmanaged CLI uses the same keyed executor as a one-shot owner after
acquiring a live kernel/store lease or conditional claim. If a managed owner
appears concurrently, exactly one enters or both fail closed. A path/inode may
remain after exit, but only a currently held lock/lease plus live-state proof
has ownership meaning. Legacy direct-manager fallback is not an owner protocol.

### RC-CLI-003 — Ambiguous durable commit

The caller creates the request ID before writing. Commit-then-timeout,
response loss, and read-back failure are resolved by that ID. Retrying the same
invocation does not advance the intent generation. If the result remains
unresolvable, the command exits nonzero with `accepted` unknown and the request
ID (`commit_state:"unknown"`, JSON `accepted:null`); it neither falls through
to a provider nor creates a fresh request. The canonical request fingerprint
includes action, stable target, and normalized parameters: reuse with an
identical fingerprint returns the original operation; reuse with a different
fingerprint is a deterministic conflict. The only exceptions are explicitly
enumerated monotonic upgrades such as shutdown/interaction `force`: they update
a versioned `force_revision`/policy field on the same operation through a
conditional transition, preserve immutable target/base parameters, and append
audit evidence. Before the revision advances, a fresh trusted authorization
decision must explicitly cover force, the current principal/target/scope, and
current policy; base-action authority or raw store write access is insufficient.
Denial/revocation races leave the original non-forced operation unchanged. They
never create a second owner. CLI/API expose caller-supplied request
IDs plus operation status/wait lookup; generated IDs are cryptographically
strong and size-bounded.

### RC-CLI-004 — Controller acknowledgement loss

When a controller accepts a request and the socket reply is lost, times out, or
ends at EOF, the CLI reads durable request/owner state. It never interprets the
lost acknowledgement as permission for direct cleanup. This row explicitly
covers city stop, where current code can send `stop`, lose `ok`, and then run a
second direct stop.

The pre-front-door P1.0D slice proves only the negative safety property: a
request that may have entered never falls through to direct cleanup, an
alternate stop request, or supervisor registry/reload mutation at any call site.
It cannot claim durable acceptance, operation lookup, or this row in full.
P2.11/P2.11A must supply those mechanisms before the row is complete.

### RC-CLI-005 — Cancellation and waiting

Cancellation before authoritative commit leaves no request/effect. For a
managed request, `SIGINT` after commit exits 130 while the durable owner
continues. A managed caller wait deadline after commit exits 1 with
`accepted:true`, `completed:false`, `wait_result:"timeout"`, and the operation
ID. An unmanaged one-shot is the owner: before native entry it may conditionally
release an unentered or dependency-blocked claim back to durable pending and
exit 130 on SIGINT or exit 1 on wait deadline, with the accurate wait result;
after native entry it records interruption but keeps the process/claim/permit
until actual return/fence and the required stable or failed milestone, then
projects exit 130 for SIGINT or 1 for timeout. Thus a post-entry unmanaged
deadline bounds caller-wait classification, not owner process lifetime. It never
abandons a mutating goroutine merely to honor the signal promptly. SIGKILL
recovery follows RC-TMUX-003/RC-CRASH-001, and a
provider without lookup/dedup/fencing/killable containment cannot certify that
unmanaged effect. Cancellation is not a provider failure, crash, churn, or
quarantine signal. A provider call that ignores cancellation retains its real
key/provider permit until it returns or is atomically fenced.

Before that durable front door, P1.0D's definite-pre-entry direct city-stop
fallback follows the same unmanaged rule: after native entry its CLI process
and continuously held same-path controller lock outlive any caller wait timeout
until the provider call returns or is independently fenced. It never abandons a
mutating stop goroutine merely to return at the wall-clock cap.

### RC-CLI-006 — Command completion milestones

The compatibility contract is explicit until a versioned CLI decision changes
it:

| Command shape | Managed exit 0 milestone | Unmanaged exit 0 milestone |
|---|---|---|
| wake/suspend/reset | Canonical intent committed to the live durable owner | The claimed one-shot executor reaches the command's required stable state; commit alone is insufficient because no owner remains after CLI exit |
| `session new --no-attach` | Session and start intent committed | Exact launch reaches ready (or the command's explicitly versioned started milestone) before the one-shot owner exits |
| ordinary `session new` | Exact launch readiness is the pre-attach gate; the CLI remains attached, normal detach returns 0, setup/transport failure returns 1, and pre-attach replacement returns stale/superseded without attaching | Same; attachment is non-mutating after exact binding |
| close | Durable close convergence reached its required terminal facts; self-close follows RC-CLI-010 | Same; no background cleanup goroutine is abandoned |
| kill | Exact prior incarnation stopped and killed/asleep bookkeeping committed | Same |
| rig restart | Old incarnations stopped and new restart generations committed | Same |
| city stop | Admissions stopped, sessions resolved, city provider teardown succeeded/already absent/explicitly unsupported for the certified profile, controller ownership released, and every required store shutdown succeeded | The one-shot owner resolves the same steps before exit |

Pre-commit failure, terminal required-store/provider failure, unresolved commit,
and ambiguous effect exit 1. A command never prints `closed`, `stopped`,
`delivered`, or equivalent for mere acceptance.

The requester-fence exception is explicit. When the CLI runs inside the exact
launch it asks close/suspend/reset/kill/restart/city-stop to terminate, successful
durable acceptance and one bounded result-write attempt return exit 0 with
`accepted:true`, `completed:false`; teardown begins only after fence release.
If that caller explicitly requests terminal waiting, the already-accepted
operation returns exit 1 with `error_class:"self_wait_unavailable"` and remains
pending—its own process cannot witness absence after it is killed. An external
caller/supervisor may return exit 0 only after the command-specific terminal
proof in the table.

### RC-CLI-007 — Human and JSON output

Existing version-1 JSON remains additive. Successful writes produce exactly one
JSONL object, no banner on stdout, and stable existing fields. Pre-commit
rejection preserves each command's characterized legacy stderr/empty-stdout
behavior. Once commit is proven or unknown, `--json` emits one result record
even on nonzero exit when stdout remains writable. Add `op_id`, nullable
`accepted`/`completed`, `commit_state`,
`completion_state`, `wait_result`, `provider_stage`, action-specific
`action_result`, and `error_class`. Human output distinguishes accepted from
complete in the same cases. A broken pipe/output-write failure may leave no
parseable object; it returns nonzero but never rolls back, repeats, or mints a
new request. `session new --json` continues to require `--no-attach`.

### RC-CLI-008 — Stable target binding

Each surface first applies its existing permanent resolution ladder
(`SESSION-ID-003/004/011`) without adding fallthrough. Path alias is available
only where the existing surface defines it. Unique resolution then binds the
stable session ID and any identity required by the command. Rename, alias reuse,
closure, or a same-name successor cannot retarget a bound request. Legacy alias-
only commands execute only after unique current binding proof; ambiguous legacy
records terminalize visibly rather than guessing.

### RC-CLI-009 — Poke semantics

A poke is only a post-commit hint. Poke failure after successful commit is a
warning while feed/audit recovery remains sufficient. Store failure before or
during commit is not hidden by a poke attempt. Pokes never confer action
ownership.

### RC-CLI-010 — Requester-safe self-termination

Close, suspend, reset, kill, rig restart, or city stop invoked inside a launch
installs a requester fence. The owner defers that exact launch's teardown until the CLI
performs one bounded result-write attempt and releases the fence, or exact
process observation proves the requester exited. EPIPE/output failure releases
the fence and exits nonzero. Lost release acknowledgement is recovered by
requester identity, never timeout-based blind kill. The fence is a durable,
configured lease with three wake paths: explicit release, exact requester
process exit, or boot-scoped monotonic expiry. The persisted clock tuple is
`host_boot_id` plus an OS boot-time deadline, so a controller process restart on
the same boot can reconstruct it. A changed boot ID may clear only after exact
requester/launch absence—processes cannot survive that reboot. Providers without
an equivalent restart-safe clock do not enable automatic expiry; they alarm and
wait for release/exact exit. Wall-clock forward jumps never release a fence and
backward jumps cannot hide its age. On valid expiry the owner records
`requester_result_abandoned`, performs/records one controller-owned durable
result attempt, revalidates the exact accepted operation and launch identity,
then releases the fence; output/return to the wedged caller is no longer
promised, and completion is never falsely reported. Any asynchronous final
teardown is accepted-not-complete rather than a false completed result. The
requester-fence exit and `--wait` rules are the explicit exception in
RC-CLI-006; they are covered for every self-terminating command family.

An unmanaged CLI cannot be both the requester that will be killed and the only
one-shot effect owner. Before durable acceptance it must hand the exact operation
and exclusive claim to a separately proven live supervisor/one-shot owner that
survives the target launch, or reject with
`error_class:"self_owner_unavailable"` and zero effect. A future manual rerun is
not automatic convergence and cannot justify acceptance.

### RC-AUTH-001 — Trusted command provenance

A control command is executable only when requester provenance was derived from
an authenticated trusted ingress and covers the exact action, stable target,
tenant/city scope, and bounded payload. Self-asserted fields written through
`bd`, metadata edits, copied authorization stamps, unknown principals, expired
decisions, and cross-city replay terminalize as typed authorization denial with
zero provider entries. A target identity/witness proves only what would be
mutated; it never proves who may request it.

The local single-tenant compatibility profile may explicitly configure
`store_writer_is_controller`, in which case every store credential holder is
reported as having full session-control authority. Hosted/multi-tenant profiles
must refuse this mode rather than silently treating shared work credentials as
authorization.

### RC-AUTH-002 — Claim-time authorization and policy change

Commit-time validation cannot authorize forever. The claimer verifies the
immutable ingress stamp and current mechanical policy immediately before
effect admission. Revocation, tenant move, policy-version change, target-scope
change, payload-limit change, and command expiry between commit and claim
produce a durable denied/superseded result; retry cannot mint a new principal or
bypass denial. A policy-provider timeout/outage is `authorization_unknown`, not
denial: it fails closed, parks without provider entry, preserves the command,
and registers a policy-version/health wake plus bounded detection/alarm/backoff.
Authorization failure is not provider failure and does not consume crash/churn/
quarantine budgets. Policy contains no hardcoded role name; it evaluates
configured principals/scopes/actions.

### RC-AUTH-003 — Credential and namespace separation

In every hosted/high-risk certified topology, work-plane agent credentials can
read/write their allowed work beads but cannot create, mutate, claim, or forge
protected lifecycle/nudge/control commands, authorization stamps, owner claims,
or terminal results. Only the control front door and owning reconciler receive
the minimum separate permissions. Direct/unpinned `bd` writes are rejected and
audited. Startup and periodic capability checks refuse effect admission if the
store/provider cannot enforce this separation.

An agent invoking an allowed self-termination command never receives controller-
wide store credentials. It uses a trusted local ingress (for example an
isolated UDS with peer-process verification) plus a controller-minted,
launch-bound, narrowly scoped self-control token. The token names exact city,
session, launch, permitted action set, audience, expiry, and nonce; it cannot
authorize another target, arbitrary nudge payload, owner claim, or result write.
Replay after launch replacement, token theft by another process/user, peer-
credential mismatch, and UDS/socket substitution are denied with zero provider
entry.

## 3. Runtime observation and cache truth

All liveness dimensions use the existing `runtime.ProbeResult` vocabulary. No
parallel boolean/tri-state taxonomy is introduced.

### RC-OBS-001 — Composite observation

Box/session presence, agent-process liveness, readiness, attachment/activity,
provider-scope identity/health, scope completeness, freshness, and error are
independently typed. A successful
tmux pane census plus failed process census means pane `Alive`, process
`Unknown`; it is neither whole-observation `Unknown` nor process `Dead`.
Provider-scope detail distinguishes current server, exact server absent, server
degraded, partial census, and an exited pane with status while retaining
`runtime.ProbeResult` as the sole Alive/Dead/Unknown liveness vocabulary.

### RC-OBS-002 — Genuine no-server versus lost server

A validated exact no-server result proves only that the tmux registry for the
explicit city socket is empty at that instant; it never proves tagged processes
absent. The observation carries tmux server PID plus process start identity. If
the server instance differs from the one that acknowledged a session's box,
registry omission is `RuntimeScopeLost`, never per-session corroborated death.
Classification depends on the current typed probe, not process-local
“primed” memory. Timeout, cancellation, permission failure, connection refusal,
malformed output, socket replacement, and substring-only error matching are
unavailable/degraded. Cache history may expose labeled last-known-good evidence
but cannot change the current fact.

### RC-OBS-003 — Partial lists and malformed rows

Returned names from a partial list prove only those exact names live. Omission
proves nothing. Only a complete, fresh list can prove registry absence.
`PartialListError`, malformed rows, cancellation, unsupported detail, and
unknown executor errors propagate typed uncertainty to destructive consumers.

### RC-OBS-004 — Cache generation races

Concurrent dirty readers cause one refresh per generation. Invalidation during
refresh causes exactly one follow-up generation; it does not defeat
singleflight. A Stop eviction cannot be resurrected by an older in-flight
refresh. Refresh failure preserves last-known-good; stale expiry becomes
`Unknown`, never an empty-success snapshot. Tests use an injected clock, not
wall-clock sleeps.

### RC-OBS-005 — Pane corpse, shell, and process distinctions

A remain-on-exit dead pane is a dead artifact, not a live runtime. A live shell
with no expected agent is `AgentMissing` only after a complete process census.
No process-name hints means agent liveness is `Unknown`/optimistic for legacy
compatibility, never confirmed dead. Every DeathWitness additionally requires a
complete targeted `GC_SESSION_ID` process scan; any live matching process with
validated start identity vetoes death. Runtime-scope-lost plus a surviving
process becomes an explicit adopt-or-supervised-stop/escalate decision that
preserves work; tmux registry absence alone never closes it.
`SESSION-RUNTIME-001` remains authoritative.

### RC-OBS-006 — Human attachment and copy-mode state

Observation carries exact-pane human attachment count/state and copy-mode state
with the same freshness, completeness, and launch identity as activity. Before
raw key injection, the provider atomically revalidates the pane identity and
interaction state. While a human is attached, queue/wait-idle nudge and prompt
families park and immediate raw nudge/interrupt returns a typed
`human_attached` conflict with zero injected keys unless an explicit, versioned
force policy upgrades the same operation. An unattached pane left in copy mode
uses a separate bounded orphan-mode recovery: after a configured grace and
second exact observation, use one server-serialized provider operation that
atomically conditions on still-unattached + exact pane/launch + still-copy-mode
and cancels it as an audited preparation step. If the condition changed, return
a typed conflict without canceling. Re-observe, then reevaluate the command. Recovery failure is
typed `copy_mode`; it never hot-polls. Graceful
lifecycle stop/close exposes attachment as an operator-visible blocker; an
explicit force may proceed against the exact witnessed target and never chooses
a different pane. Attach/detach, copy-mode exit, and detach-while-in-copy-mode
wake the exact key/recovery condition.

| Family/mode | Attached or copy-mode result | CLI projection |
|---|---|---|
| queued/wait-idle nudge, respond, prompt/prime while attached | Preserve `pending`, park on the exact interaction-state revision, wake on detach/mode exit | exit 0 for durable acceptance, `completed:false`; never claim injection |
| any raw interaction on unattached copy-mode pane | Run bounded orphan-mode grace → exact recheck → audited cancel → re-observe; then reevaluate | accepted-pending while recovery runs; typed failure if recovery cannot prove safety |
| immediate nudge/respond/interrupt while attached | Terminal typed `human_attached`, zero key injection | exit 1, `accepted:true`, `completion_state:"failed"` |
| explicit force upgrade of the same interaction operation | Fresh force-specific authorization, then conditionally advance `force_revision`; atomically revalidate target/state, cancel copy mode only here, then inject | result/exit follows RC-NUDGE-002; force is audited, never a new command |
| graceful suspend, reset/restart, close, rig restart, city stop, or provider swap | Accepted-pending interaction blocker; no raw key injection or replacement | exit/wait follows RC-CLI-005..006; config-driven swap remains visibly blocked |
| explicit kill or force-upgraded lifecycle operation | Act only on the exact witnessed target; a self-target still honors RC-CLI-010 before teardown | terminal success/failure follows RC-CLI-006 |

This is an intentional compatibility change from current tmux paths that cancel
copy mode implicitly. The owning slice characterizes old output first, adds the
versioned result/force surface, and updates permanent `SESSION-*` requirements;
no cutover inherits implicit cancellation accidentally.

### RC-OBS-007 — Controller self-echo attribution

Every controller-originated tmux injection reports operation ID, exact pane,
native-entry time, and bounded attribution window to the observation manager.
A provider may classify self-echo only with an origin-aware activity source or
output sequence/delta that can distinguish concurrent agent/human output. Tmux
`window_activity` alone is a coarse timestamp and cannot make that claim: an
activity sample overlapping the attribution window becomes `activity_unknown`,
not agent progress and not erased inactivity. A later independent output/activity
sample or a bounded quiet confirmation resolves it. Manager restart, delayed
observation, repeated injection, and attribution expiry fail conservative,
schedule a bounded re-observation, and cannot create permanent not-idle
treadmill behavior.

## 4. Runtime and process identity

### RC-ID-001 — Caller-owned operation and incarnation tokens

Create/relaunch allocates new box/launch tokens before entry and installs them
atomically with creation. Stop, teardown, nudge, prompt, and interrupt persist
the expected tokens observed from the authoritative existing binding; they never
mint or install replacement target tokens before the effect. Provider-internal
token generation that is not returned to the caller cannot certify adoption.

### RC-ID-002 — Box and launch identity are distinct

The box/provision incarnation changes on create or reprovision. The
launch/agent incarnation changes on every respawn/relaunch. Stop/teardown binds
the box; ready, prompt, nudge, interrupt, drain acknowledgement, and interaction
bind the launch. Identities are provider-owned opaque values scoped `box` or
`launch`, not universal tmux/process structs. Warm providers distinguish both;
a conjoined provider may use one value only when it proves both lifetimes are
identical.

### RC-ID-003 — Exact tmux target

All live/destructive tmux commands use a conditionally validated immutable
server/session/pane target and the applicable token, never a reusable raw name.
The concrete tmux witness contains the explicit city socket, server PID plus
process start identity, `$session_id`, agent `%pane_id`, pane PID plus process
start identity, and caller-owned scoped token. Where tmux accepts immutable IDs,
effects address `$session_id`/`%pane_id` rather than a name; server restart/ID
reuse is rejected by the server identity/token. Any additional precondition is
validated in the same server-serialized command or the action receives a weaker
profile.
Tests cover exact missing, prefix sibling, rename, destroy/recreate with the
same name, server restart/ID reuse, and out-of-band replacement between check
and effect. The strongest profile requires a single tmux server-side conditional
command/daemon or an equivalent atomic target mechanism; check-then-name is not
certified.

### RC-ID-004 — Multi-pane identity

Observation identifies the actual agent pane and its launch incarnation.
Changing focus cannot redirect nudge/interrupt/stop; pane respawn invalidates a
launch witness. Agent-pane discovery is part of the target proof, not a
best-effort convenience.

### RC-PROC-001 — Scan-time process identity

`LiveRuntime` carries PID, start time, command/executable identity, environment
session ID, normalized city, and `launch_identity` from one scan. Every TERM
and KILL validates the scan-time identity. The Linux certified path uses pidfd
or an equivalent atomic handle for the root; check-then-`kill(pid)` alone is not
PID-reuse proof. Exact process-tree teardown additionally requires provider-
owned cgroup/scope containment with freeze-and-kill or equivalent prevention of
fork/setsid escape. Without containment, repeated exact scans must reach bounded
quiescence; any new/unreadable/escaped root leaves teardown unresolved and
blocks replacement. macOS remains a weaker profile until equivalent primitives
exist.

### RC-PROC-002 — Partial process scan

Exact returned roots may be fenced/stopped, but a scan error cannot certify no
omitted orphan and cannot permit replacement Start until a complete targeted
rescan. Error logging plus a successful return is not fail-closed behavior.

### RC-PROC-003 — Process group and scope safety

Candidates match normalized city, stable session, and launch identity. PID/PGID
reuse between descendant discovery, TERM, grace, and KILL yields a stale no-op,
not a signal to the replacement. Cross-city, fork-between-scan-and-signal,
fork-during-grace, double-fork, `setsid`, exec, and subreaper cases remain in
the permanent corpus.

## 5. Provider mutation contracts

### RC-TMUX-001 — Start is non-destructive

`Provider.Start` never kills/recycles a colliding pane/session. It returns a
typed collision with observed identity. Only reconciliation may plan a separate
witnessed replace. A recreate race returning `ErrSessionExists` is success only
when exact intended operation/token observation proves adoption. Provider-
internal kill/signal sites participate in the effect inventory.

### RC-TMUX-002 — Degraded socket handling

An alive-but-slow server is degraded/blocked: no new server, socket unlink, or
crash-budget accrual. A healthy server and a currently validated exact
no-server result retain their explicit outcomes. No server cleanup ever targets
the personal/default tmux server.

### RC-TMUX-003 — Context-aware actual-call ownership

Every provider effect accepts caller context; subprocess deadlines compose with
it; lock waits, retry waits, debounce, and grace periods are interruptible.
Legacy/non-cooperative effects remain owned after the waiting caller times out.
No detached goroutine permits another same-key call, and provider-wide capacity
remains bounded. Adapter entry, provider-native mutation entry,
`waiter_detached`, actual return/panic, fencing, and final resolution are
distinct recorded transitions. Caller timeout/cancellation changes
`wait_result`; it is never a terminal provider result. Before native entry, an
effect that can outlive controller death durably records its operation, exact
target, and ownership/claim plus a production resolution mechanism (provider
lookup/deduplication, atomic fencing, or killable containment). After
controller `SIGKILL`, empty-memory restart blocks overlap until that exact
entry is proven absent, adopted, fenced, or resolved; process-local permit loss
is not proof.

### RC-START-001 — Collision and lost response

A collision converges only after exact intended operation/token observation;
otherwise it remains conflict/blocked. Lost response causes targeted exact
observation, never blind Start or hidden zombie recycling.

### RC-START-002 — Cancellation and cleanup

Cancellation before create leaves no runtime. Cancellation after create may
clean up only the exact operation-created incarnation. Cleanup uncertainty is
`ambiguous` and holds ownership; it is not an ordinary retry.

### RC-START-003 — Start phases

The result distinguishes staged, created/provider-accepted, initializing,
ready, canceled, deadline, crashed, collision, rejected, and ambiguous. Ready
requires observation of the exact launch identity plus the provider profile's
declared readiness witness; elapsed delay alone is initializing/unconfirmed. A
weaker compatibility profile may name delay-based readiness but cannot certify
the exact-ready milestone. A late nil result after an outer deadline is not
silently success. Any retained deadline-after-ready rule is named and tested.

### RC-START-004 — Error classification

Pre-start/staging failures are definite-before-create. A category table gives
provider-entry proof, durable transition, retry/backoff, and budget effect for
remain-on-exit, mouse, setup, prompt, session-setup, initializing, rate-limit,
and startup death. Unrecognized provider text is ordinary provider error; text
alone never selects rate-limit/fatal/quarantine handling. Independently observed
death/timing still follows `SESSION-RECON-010/011` and may accrue crash/churn.

### RC-START-005 — Warm relaunch

Missing box returns `ErrSessionNotFound`; respawn failure leaves the box; a new
launch token prevents stale prompt/nudge/ready confirmation. Provider swap
cannot start the replacement until the exact old box is stopped or fenced.

## 6. Durable writes, feeds, queues, and projections

### RC-STATE-001 — Commit before projection advance

Local/shadow projection advances only after a proven commit. Failed writes
return the unchanged projection plus typed retry/watermark; no later decision in
that reconcile consumes the proposed patch. Commit-with-lost-response resolves
by authoritative reread before projection advance. A cache-backed write that
returns an ambiguous error mutation-fences and dirties or invalidates the
affected row before returning. A concurrent refresh that captured the old row
before that fence cannot install it afterward and clear the invalidation. The
next ordinary projection load must fetch backing-store truth and must not replay
the pre-write cache image.

### RC-STATE-002 — Atomic lifecycle intent front door

Every lifecycle request is one total, versioned, idempotent command/intent with
request ID and monotonic generation. Torn legacy multi-key mirrors decode
`Unknown`; they never form a hybrid action. No mutating CLI or controller writes
lifecycle desired state through an unordered raw metadata batch after cutover.

### RC-STORE-001 — Working-set identity and restore lineage

Each authoritative store exposes a provisioned `store_uuid`, certified schema
version, restore epoch/lineage, and orderable high-water evidence appropriate to
its profile. The expected identity and an independently retained monotonic
restore/high-water anchor are verified at startup, connection replacement,
feed attach/resume, lease acquire/renew, and effect admission. UUID mismatch,
restore-epoch regression/change without a recovery record, or revision/epoch
regression freezes effects before claim. Identical-backup restoration without
an independent anchor is explicitly uncertifiable, never guessed safe.

### RC-STORE-002 — Explicit restore recovery

A sanctioned sqlite `VACUUM INTO` recovery, Dolt reset/file restore, or equivalent
uses one recovery operation. It stops admission, records a new restore epoch
above the external anchor, installs an authoritative snapshot, and quarantines
all recovered nonterminal edge commands plus every effectful level intent from
the old epoch—including start, wake, relaunch, pool allocation, stop, and close.
They become `delivery_unknown`/`expired`/`recovery_review_required`
as appropriate and never replay automatically. Epoch/intent floors are
re-anchored above the retained high-water mark; an operator or upstream source
must deliberately reissue current intent. Crash at every recovery step resumes
the same operation and cannot enable effects early.

### RC-STORE-003 — External binary and schema compatibility

The store capability reports actual schema version and writer/provider build,
not only what a parser accepts. Certified `bd` version/schema pairs are pinned
and tested N/N-1 in both directions. Startup or runtime mismatch, auto-migration
by an unapproved writer, successful but wrong-shape rows, and downgrade to an
older writer select observation-only/refusal below the total decoder. A shared
decoder/reference audit cannot overrule the version fence.

### RC-STATE-003 — Dedicated CAS coordination records

When metadata-key CAS is emulated by whole-bead revision, every claim, lease,
or reservation uses a dedicated low-write bead whose only writers are that
coordination protocol's participants. Colocation with lifecycle status,
operator annotations, or any unrelated high-frequency key is rejected by the
schema/registry guard. Adjacent-key write load cannot starve the claim within
its certified contention envelope.

### RC-FEED-001 — Poison and cursor transitions

A corrupt/unsupported feed record is never skipped and the cursor never advances
past it. The source becomes unsynced, emits a typed gap, and relists from a
consistent snapshot. Recurring poison remains terminal-unsynced with bounded
backoff/alarm and revokes feed-certified claims; it cannot create a relist loop.

### RC-FEED-002 — Commit/journal atomicity

A feed record is derived from the authoritative commit log or commits atomically
with the domain mutation. Crashes at every domain/journal boundary yield either
the record or a typed detectable gap, never silent omission.

### RC-QUEUE-001 — Dirty is not automatically runnable

Dirty/coalesced state is separate from runnable admission and retry eligibility.
Retry cause includes class, `intent_generation`, and the exact
`blocker_revision` or `provider_health_revision`. Duplicate observations at the same semantic revision
may mark dirty but cannot erase provider-error backoff. Only new intent or the
exact blocker recovery revision bypasses it.

### RC-QUEUE-002 — Stingy queue model

The deterministic model covers Add-before-Get, Add-during-processing, repeated
Add coalescing, Done, Forget/rate-limit races, timer supersession, shutdown,
restart reconstruction, hot-key fairness, and the invariant that one
`(logical controller, key)` never executes concurrently. Distinct logical
controllers may evaluate the same session key; their effects meet only at the
shared executor. Queue memory is bounded by unique keys per controller.

### RC-QUEUE-003 — Panic and retry treadmill bound

A permanently failing/panicking key has bounded attempt rate, logs, and restarts
per configured time window/`retry_cause_revision` plus retained diagnostic state. Durable
level intent is never abandoned because attempts were exhausted; new intent,
the exact blocker/provider-health revision, or bounded audit recovery makes it
eligible again. It cannot hot-loop or stop other keys. Ten thousand irrelevant
duplicate events with a frozen fake clock cause no extra provider entry. Only
structurally invalid edge commands may dead-letter.

### RC-QUEUE-004 — Parked-condition readmission

A key parked on dependency, capacity, provider health, identity conflict, or
ambiguous-own-call resolution is not dropped. A newer relevant semantic source
revision atomically clears the condition park and re-admits latest-state
evaluation; it may repark if the blocker still holds. Irrelevant same-generation
duplicates do not bypass provider-error retry delay, but their dirty replay is
retained. Actual detached-call return/fence/resolution enqueues the key and
releases its per-provider charge. Tests cover fresh stop/wake intent,
audit-enqueue, and call completion arriving exactly while park registration is
installed.

### RC-QUEUE-005 — Composite blocked-state liveness

The composed state machine covers every reachable combination of condition
park, provider ambiguity charge/circuit, write watermark, retry delay,
authorization hold, human-interaction hold, requester fence, and
`city_admission_generation`. Every nonterminal state has a named admissible wake
path; controller-owned blockers have a bounded resolution edge, while external
blockers (human attachment or provider/policy outage) have bounded change
detection plus age alarm/escalation and may remain blocked until reality
changes. No circuit may suppress the probe needed to clear its own ambiguity.
Both unclassified and classified blocked-key age are reported per class and
abort canary expansion when their class-specific detection/escalation limit is
exceeded; no contract pretends an infinite external outage resolves on a timer.

## 7. Nudge delivery

### RC-NUDGE-001 — Mode contract

`queue` only durably accepts. `wait-idle` never sleeps in a CLI, worker, or
provider permit; busy registers a keyed blocker/timer. `immediate` targets the
current exact launch now and does not silently become a delayed queue. Output
distinguishes queued, accepted, and delivered/unconfirmed. Immediate binds the
exact current launch at commit and rejects when none exists. Queue/wait-idle
commits stable session ID, `continuation_identity`, and an explicit target policy;
claim later atomically binds one eligible launch before provider entry.

### RC-NUDGE-002 — Typed delivery outcome

Providers return `accepted`, `duplicate`, `target_missing`, `rejected`, or
`ambiguous`, with stage evidence. Legacy tmux nil is not delivery proof.
`injected_unconfirmed` means injection/submit into the exact launch is proven,
but agent consumption is not observable; it is a terminal transport success
with `provider_stage:"accepted"`, `completion_state:"completed"`, exit 0, and
human text that explicitly says receipt is unconfirmed. `delivery_unknown`
means native entry may have injected but neither injection nor non-injection is
provable; it is a terminal no-replay result for a non-deduplicating provider,
with `completion_state:"unknown"`, exit 1, and no `delivered` wording. Only
command-ID-deduplicating providers may replay after ambiguity. The production tmux profile has no command-ID deduplication: its
durable attempt marker precedes paste, pre-paste definite failures may retry,
and any crash/error after paste may have occurred and terminalizes
`delivery_unknown` without blind re-paste. A provider-specific inspection/clear
recovery may retry only if its conformance proof establishes that no prior
submit/injection can survive; heuristic screen scraping is not proof.

### RC-NUDGE-003 — ACP attachment owner

A process without the in-memory ACP attachment cannot report delivery.
Immediate provider miss is an explicit CLI failure; queued wait-idle remains
durable for the eventual attachment owner. Controller restart/reroute preserves
the command.

### RC-NUDGE-004 — Launch fence and reuse

After a command binds a launch, it never retargets. Stop/relaunch/name/alias
reuse between binding and provider entry delivers nothing to the replacement
and terminalizes `superseded`. Before binding, a relaunch may select the new
launch only when the committed policy permits it; temporary lack of an eligible
launch remains `pending`, never an ambiguous “deferred” terminal result.

### RC-NUDGE-005 — Provider content semantics

Non-ACP deferred batching retains its sanitized system wrapper; ACP uses its
plain runtime message. Copy mode observation and explicit-force-only
cancellation, provider-specific Escape, detached wake, large bracketed paste,
and submit confirmation remain real-provider conformance cases. Tmux paste and Enter are separate native stages;
neither nil return nor a debounce sleep proves the agent consumed the command.

### RC-NUDGE-006 — Referenced waits

A valid pending/not-ready wait returns the nudge to pending without consuming an
attempt or terminalizing. Ready may deliver. Canceled, closed, expired, and
failed waits terminalize mechanically. Missing, malformed, and unsupported
references dead-letter. Store read or partial-list error leaves the source
unsynced/claim recoverable; it is not misclassified as command poison.

### RC-NUDGE-007 — Conservation and batching

Every claimed ID reaches exactly one durable terminal result or returns pending.
Partial bookkeeping cannot acknowledge uncalled commands. A bounded combined
injection maps one provider result only to the exact included IDs. Ack/event/
status-stamp failures never cause silent loss or blind redelivery.

### RC-NUDGE-008 — Total decoder and resource ownership

The sole durable ledger has a total versioned decoder. Malformed JSON, an
invalid known-version state/time/target, or an oversized payload dead-letters
with bounded evidence. A well-formed unsupported newer version/state is
preserved byte-for-byte, marked `upgrade_required`, and parked without claim or
normalization until a compatible owner arrives; it may block only the ordering
domain that cannot safely pass it, never unrelated session keys. No sidecar, per-session
process, PID/lock/log authority, or second ledger exists. Reconstruction after
controller kill and a 100K-command bound are certified.

### RC-NUDGE-009 — Delivery-quality conservation

Terminal-state conservation is necessary but not sufficient. Each provider
reports accepted, delivered/duplicate, `injected_unconfirmed`, rejected,
expired/superseded, and `delivery_unknown` counts under one certified envelope.
Each provider profile declares whether its quality numerator is proven delivery
or proven exact-launch injection; those metrics are never conflated. A provider fake whose
lookup/probe deterministically resolves ambiguity can never produce
`delivery_unknown`. The declared profile quality-numerator/accepted ratio,
separate raw delivered and exact-injection ratios, and unresolved-ambiguity age
remain permanent release SLIs; exceeding the profile
threshold freezes or rolls back that provider family even though every command
has a terminal row.

## 8. Close, kill, and city shutdown

### RC-CLOSE-001 — Durable close convergence

Close commits desired-closed generation before any Stop. It then exact-stops,
cancels waits, clears safe overrides, retires identities, closes the session,
releases assigned work across stores, and fires the convergence event. Every
step is derived level-triggered from authoritative facts and idempotently
resumable after crash; only ambiguous/non-idempotent boundaries and terminal
proof need progress markers. No generic step log is required. No intermediate
state respawns or frees an alias while the old runtime may live.

### RC-CLOSE-002 — Failure semantics

Stop failure leaves close requested/closing, not falsely closed. Work and alias
remain owned until their release preconditions are safe. A store failure after
runtime stop cannot cause a replacement wake. `SESSION-START-006` and
`SESSION-WORK-001..004` remain mandatory.

### RC-CLOSE-003 — Terminal idempotency

Repeated close returns the same terminal result without duplicate work release
or creation of a new logical event ID. Closing an already-closed record repairs
only safe leftover artifacts. Partial cross-store release is audited and
retried with per-step outcome. Best-effort event transport may deliver a stable
event ID zero or multiple times across publish/marker crash; it is not claimed
exactly once without a separately durable outbox.

### RC-CLOSE-004 — Explicit kill

Kill targets the exact old launch/box. Circuit reset and killed/asleep intent
are atomic/resumable; any automatic restart is a newer durable generation.
Poke or status failure cannot make the CLI claim a clean kill.

### RC-SHUT-001 — Provider-global admission barrier

Shutdown/provider swap stops lifecycle, nudge, and start admission city-wide;
waits for actual in-flight calls; runs reverse-dependency interrupt/stop waves;
target-probes unknowns; resolves orphans; tears down only the explicit city
provider; then shuts down stores. New starts remain durable but cannot enter.
Successful convergence requires every bound target proven absent, no unresolved
orphan/ambiguous call, provider teardown succeeded/already absent/explicitly
unsupported, ownership released, and every required store shutdown succeeded.
The dying controller may durably record only its pre-release
`ready_to_terminate` facts. An independent external CLI/supervisor proves
controller exit, live kernel-lock release, exact provider absence, and required
store-process shutdown before reporting terminal success; a requester inside a
terminated launch follows RC-CLI-010 and reports accepted-incomplete instead.

P1.0D proves only the interim same-path subset: foreground and supervisor
startup take the legacy controller lock before any materialization/store/
provider/watcher/`on_boot`/tmux effect, transfer it once to the controller, and
direct stop retains one acquired lock through every entered call and store
shutdown. Registration is read-only. This does not claim provider-global or
cross-path admission.

### RC-SHUT-002 — Force upgrades one operation

`--force` upgrades the same shutdown request and skips remaining grace. It does
not create a second stop owner or provider call. In P1.0D, ambiguous
`stop-force` acknowledgement at every CLI/supervisor call site fails closed
before unregister/reload or any alternate stop trigger.

### RC-SHUT-003 — Shutdown timeout

Managed timeout reports accepted-but-incomplete while the owner continues. An
unmanaged CLI cannot return while an unowned goroutine keeps mutating. A
noncooperative call remains explicitly ambiguous and blocks replacement. Only
an independent external waiter can convert the controller's pre-release facts
plus post-exit live-state observations into terminal shutdown success.

P1.0D's pre-front-door direct fallback retains its process and same-path lock
past a post-native-entry wall-clock deadline until actual provider return or an
independent fence; the elapsed deadline changes the eventual exit status, not
owner lifetime. Its nested interrupt/stop target waves use the same no-detach
rule: an inner per-target timeout cannot release the direct owner while an
entered provider goroutine is still running. Certification blocks `Interrupt`
and `Stop` separately past both the outer deadline and their respective inner
per-target deadline; releasing either fake before its inner deadline is not
evidence for this rule.

### RC-SHUT-004 — Partial observation during stop

Known exact targets may stop after targeted live proof; partial lists cannot
infer or kill omitted orphans. Independent confirmed targets progress, but city
stop is not successful while any target is unresolved. If the live owner still
has an admissible probe/retry/wake path, the result is `completion_state:pending`
and accepted-incomplete. If a permanent unsupported condition, explicit abort,
or exhausted bounded shutdown policy closes that path, the durable result is
`completion_state:failed` with its exact unresolved targets. Neither is ever
completed; callers project the distinction deterministically.

### RC-SHUT-005 — Socket isolation and invalid config

Teardown targets only the explicit city tmux socket, never the default/personal
server. Invalid-config stop succeeds only when a live owner retains the resolved
provider or authoritative discovery identifies exact city resources by stable
city ID. After restart with neither source, it fails closed with explicit
remediation and never guesses a provider/default socket. Provider teardown
follows orphan resolution and precedes required store shutdown. In the interim
same-path legacy-lock profile, managed/foreground start acquires the controller
lock before its first materialization, store, provider, watcher, `on_boot`, or
tmux mutation; registration does not perform those effects outside the locked
owner. A stop obtains the retained lock through an ownership-returning wait and
holds it through every post-controller store operation and shutdown, so it
excludes all same-path starter effects without a probe-close-reacquire gap.

## 9. Events and traces

### RC-EVENT-001 — Semantic event stages

Request accepted, provider-native mutation entered, provider accepted,
ambiguous, converged,
and failed are different typed events/results. Existing lifecycle completion
events remain convergence events, not request acceptance, and fire only after
their durable confirmation.

### RC-EVENT-002 — Correlation and deduplication

The durable request/action ID flows through retries and worker operation events.
Requeue does not mint a new logical operation or intentionally publish a new
terminal lifecycle event ID. Process-local random IDs may identify attempts
only. Observational transport is at-least/at-most best effort according to its
real contract: consumers deduplicate the stable event ID. An event type that
requires guaranteed publication must use a separately proven durable outbox;
the reconciler never infers exactly-once delivery from a marker.

### RC-EVENT-003 — Failure ordering

Domain commit survives recorder failure; event retry never repeats a provider
effect. Attempt telemetry may exist without a domain-success commit and is typed
separately. Events remain observation, never command truth.

### RC-EVENT-004 — Stable wire

Existing event types preserve their characterized Subject semantics. Stable
session identity travels in the existing `SessionID` field and typed payload.
New event types define Subject semantics explicitly; changing an existing
Subject requires a new versioned event type. Errors are single-line, sanitized,
and bounded. N/N-1 OpenAPI/SSE/CLI readers preserve the additive taxonomy.

## 10. Reconstruction, entropy, performance, and migration gates

### RC-CRASH-001 — Boundary registry

All provider, process, and authoritative-store mutation passes through
enumerated enforceable seams; static guards name temporary exceptions. Each seam
classifies applicable before/after checkpoints among durable intent commit,
local apply, provider adapter/native-mutation entry and return, outcome/status
write, and dedup marker.
Queue/cache internals prove restart reconstruction/model laws rather than each
receiving a durable crash ID. Generated completeness checks cover enforceable
call sites and make a new unclassified seam fail CI.

### RC-CRASH-002 — Marker-last terminal discovery

For convergence roots, crash after closing work but before the dedup/terminal
marker is discoverable by a fresh process and reaches durable terminal proof.
Failed pending-next cleanup is independently discoverable and replayable. If
closed incomplete roots are absent from normal queries, a durable transition
intent/index is required before marker-last can be claimed.

P1.1A may enforce checked writes and post-proof event ordering on currently
discoverable paths, but it does not satisfy this row. Its open-root recovery
must distinguish an empty-state partial creation missing required metadata
(terminalize and close without pouring) from a complete, idempotently adoptable
empty-state creation (resume/adopt). Only P1.1B may claim this row after every
supported store proves bounded empty-memory discovery.

### RC-ENTROPY-001 — Independent audit implementation

The audit independently enumerates raw authoritative objects and never consumes
incremental cursors, cached object sets, derived indexes, or incremental
appliers. It reuses the canonical total decoder and pure
`Contributions(object)`/lifecycle projector, then independently folds a fresh
projection. It compares full object sets, tombstones, secondary indexes,
checksums, and source cursors. Seeded mapper omission, early cursor advance,
stale resurrection, corrupt index, and wrong-source cursor are detected. Audit
never invokes a provider inline.

### RC-ENTROPY-002 — Bounded recovery

Every recovery bound is derived from partition count, scan budget, backlog, and
admitted load, then tested under that envelope. Partial/failed authority alarms
and defers; it never repairs from uncertainty. Repeated repair of the same
divergence escalates rather than becoming steady state.

### RC-PERF-001 — Honest measurement

Performance evidence pins hardware, store/provider versions, load envelope,
warm/cold/reconnect profile, sample count/window, and coordinated-omission-safe
first-ready timestamps. Durable-commit delay is separate from scheduling delay;
every excluded sample has one typed reason. Initial controller-overhead
certification applies p99 unblocked final-required-operation-commit or exact
blocker-clear to provider-native-mutation-entry `<100ms`; target certification
uses `<50ms`. Adapter entry and pre-native staging are separate stages and
cannot satisfy the SLO. The unconditioned request/need-to-native-entry
distribution is always reported and has a profile-specific bound equal to the
controller target plus explicit required-write count × certified store-write
p99; 50/100ms cannot be claimed for a store profile that cannot meet it. Each
in-memory blocker records a monotonic clear instant and exact revision/epoch;
each durable blocker uses authoritative commit/cursor evidence. Ready keys over
1s require classification; hot-path operation counts remain constant through
100K keys except true fanout. Reconnect/relist/restart RTOs are separate
thresholds. Current O(N) baselines are measured before future queue/feed/audit
benchmarks. Read-budget/coherence evidence enumerates every production Store
implementation selected by `OpenStoreAtForCity` on the execution head,
including a preflight-selected NativeDoltStore hosted-gateway profile and the
exec Store protocol; a wrapper substitute or an unnamed backend omission does
not satisfy the row unless the exact candidate records and reviews that
exclusion.

### RC-PERF-002 — Scoped-fanout and small-city budgets

Each ingress source class has a versioned maximum affected-key fanout,
enqueue-all fallback rate, and operation-count profile. The top recurring
config-edit shapes map exact keys; unknown broad changes may use the safe scoped
fallback only within budget. A budget breach is an automatic rollout abort, not
an informational metric. The zero-config small-city profile fixes conservative
defaults, passes the entropy suite, and meets its latency/resource envelope on
pinned modest hardware. Every evidence packet reports introduced knob count and
which knobs remain at certified defaults.

### RC-MIG-001 — Cold exclusive owner handoff

In single-controller phases, action-family mode is immutable for one process.
Fault config commit, stop-admission, actual-call drain/fence, old-process exit,
kernel-ownership release, new-process startup, and abort-to-last-safe-mode.
After each restart, at most one process can enter; uncertainty enables observers
only. Durable commands survive the downtime. Dynamic leases/epochs are Phase 11
only. Shadow performs no claims, acks, provider calls, status writes, or shared
acting-path queue/timer mutation. It may use an isolated bounded shadow queue
and manual/dedicated timers to exercise key→plan scheduling, provided they have
no production wake/admission effect; store/CPU/heap/FD overhead is measured
against an explicit budget and can abort expansion.

Config/environment mode is only a requested next mode; it never overrides live
ownership proof or enables a writer directly. Before HA, the certified topology
is bound to one stable city installation/host/runtime root and one authoritative
working-set identity. Same-host exclusion uses a live kernel lock; a static
store-anchored installation binding (where supported) makes a second directory/
host pointed at the same store refuse effects. It has no expiry/automatic
takeover; transfer is the same cold protocol. A long in-flight legacy pass must
finish or recheck `cold_owner_instance` at every native-effect seam before the new
owner starts.

### RC-MIG-002 — Incremental N/N-1 matrices

Resolver/basic-state compatibility lands first. Before each new schema or
writer, that exact mixed-version matrix and automated rollback drill land.
Unknown newer values remain raw/unknown and never destructively normalize.
Compatibility is not deferred to one all-future-schema epic.

### RC-MIG-003 — Watermark-matched shadow and bounded exceptions

Legacy and keyed shadow decisions/effects are compared only from the same
durable, config, runtime, and owner watermarks. Timing mismatch is data, not an
open-ended human exception. Every approved divergence is a mechanical matcher
that cites the exact watermark delta or named fail-safe semantic difference,
has owner and expiry, and is counted. Promotion requires a numeric maximum
matched-exception rate and zero unexplained effects under the certified load.

### RC-MIG-004 — Per-family bridge and cold un-bridge

The shared executor bridge is enabled one effect family at a time. Each family
retains a compiled, testable legacy direct adapter that can be selected only by
the normal cold owner-handoff protocol after all bridged native calls drain or
are fenced; it is never a live bypass. Before a second family bridges, canary
proves a cold un-bridge drill and a bounded mutation-stall-age alarm for the
bridged-but-legacy-decided configuration. Rollback cannot require editing dozens
of call sites during an incident.

### RC-CERT-001 — Exact certified topology

Certification names the store, runtime provider, OS, ownership topology,
enabled capabilities, and exclusions. Missing DoltLite CAS/feed, atomic tmux
targeting, provider dedup/fencing, pidfd/equivalent, or cooperative cancellation
selects a weaker profile or refuses high-risk startup. It never silently
weakens a claim.

### RC-CERT-002 — High-risk evidence

High-risk certification requires retained chaos seeds, long soak, race detector,
bounded heap/goroutine/FD growth, provider/store fault matrices, restart/relist/
failover RTO, and a proven rollback. “Tests pass” without the evidence packet is
not certification.

### RC-GATE-001 — Protected contract revision

G0 produces a content-addressed base manifest over the implementation plan,
acceptance matrix, permanent session requirement rows, evidence thresholds,
and effect-exception registry. Approved contract changes form an append-only,
signed delta lineage with stable per-row hashes. Every implementation bead/PR
and evidence packet cites the base, accepted delta head, and exact row/dependency
hashes it consumes. An unrelated additive delta does not invalidate parallel
work; a changed consumed row or dependency does. A slice may add coverage, but modification/deletion or
semantic weakening of a row it claims requires a distinct contract-delta review
approved by the human owner or an approver independent from the implementing
principal. CI rejects affected stale/superseded row hashes and same-principal
self-approval without serializing all work behind unrelated prose changes.

Before G0/P0.16 exists, only the explicitly named no-owner/no-schema fail-safe
slices P1.0A, P1.0C, P1.0D, and P1.1A may cite the immutable independently
reviewed candidate digest. They cannot modify a cited row or add an effect path,
and G0 must import their test evidence into the base manifest. P1.0D cannot
claim durable request acceptance; P1.1A cannot claim closed-root discovery. No
other slice uses this exception.

### RC-GATE-002 — Derived, tamper-evident evidence

Checkpoint and release packets are generated from actual CI job/test artifacts
against the cited manifest, content-addressed, and signed/attested by the CI
identity. Implementer-authored summaries are commentary only. Path/ownership
guards cover protected documents, threshold registries, required-test
manifests, skip status, and effect exceptions. Missing provenance, changed
thresholds, absent pinned artifacts, or a mutable `latest` dependency fails the
gate. Repository-admin compromise remains an explicit external trust boundary;
the design does not call signatures magic.

### RC-CERT-003 — Verification-set sustainability

Before final certification, permanent gates are right-sized from evidence
without weakening protected rows. The release packet reports verification LOC,
merge/nightly/RC minutes, retained artifact volume, flaky/retry rate, knob count,
and ownership cost. A test may move tiers only when the deterministic merge
tripwire that guards the same invariant remains. Zero-config and every supported
profile retain deterministic entropy, schema, restore, authorization, and
rollback coverage.

## 11. Mandatory test and static-guard layers

| Layer | Required coverage |
|---|---|
| Pure/model | State decoders, action decisions, queue linearizability, retry causes, multi-step terminal convergence, deterministic fake clocks |
| Fault-injected unit | Every enforceable store/effect seam, scripted process-census failure, response loss, partial result, cancellation before/after entry, replacement interleaving, stale `intent_generation`/incarnation, poison data |
| Re-exec process | Store commit then timeout, controller read then dropped ack, SIGINT before/after commit, controller SIGKILL, broken stdout, empty-memory restart |
| Real tmux on isolated `-L` socket | Atomic command shape, prefix sibling, same-name recreate, warm relaunch, server restart/ID reuse, dead pane, immutable target/token observation, multi-pane focus, attach/detach during entry, copy mode on every key-injection path, self-echo attribution, nudge |
| Process identity | Synthetic PID/PGID/fork/setsid reuse interleavings; Linux pidfd/managed-scope smoke; capability-negative profile tests on unsupported OSes |
| Store/CLI compatibility | Real bd-sqlite, BdStore/bd-Dolt, NativeDolt, and DoltLite seams as supported; pinned `bd` N/N-1 schema pairs; sanctioned restore/re-anchor drills; wrong-store and well-formed-wrong-schema fixtures |
| Performance/soak | Operation counts, admitted-load latency, hot retry, hung provider, rebuild/audit, retained failure seeds, leak bounds |

Static guards forbid, after the corresponding migration gate:

- managed CLI runtime mutation;
- lifecycle desired-state raw multi-key writes;
- raw reusable tmux-name targets for live/destructive effects;
- `context.Background` at provider effect entry;
- sleep-based wait-idle in controller workers;
- provider-internal kill/signal sites absent from the effect inventory;
- unregistered production packages/files importing `os/exec`, process-table or
  runtime-provider mutation packages, or referencing tmux binary/socket
  constants; allowlists are generated from the effect registry, not substring
  guesses;
- destructive boolean adapters that discard `ProbeUnknown` or observation errors;
- protected control-command creation/update from work-plane credentials or
  untrusted front doors;
- effect admission without current store UUID/restore/schema capability;
- deletion of a mapped historical test without a scenario-equivalent replacement.

## 12. Executable evidence registry

The prose rows above become gates only through a typed, machine-checked Go
registry. There is no separately maintained YAML inventory. Each applicable
`RC-*` × action-family × capability-profile entry records:

- store, runtime provider, OS, topology, and required capability set;
- exact Go package, top-level test name, build tag, invocation command, and CI
  job;
- named injected fault/crash boundary;
- exact durable snapshot, provider-adapter/native-entry counts, target identity,
  permit state,
  event ordering, CLI exit/stdout/stderr, and eventual convergence oracle;
- mandatory/optional status, whether skip is forbidden, owning `G*` gate, and
  retained artifact path.
- required proof seam (`fake-acceptable`, real bd-sqlite, real BdStore/Dolt,
  real DoltLite/NativeDolt, real isolated tmux, or real process/SIGKILL) and the
  conformance case that pins any fake-injected fault to observed real behavior.

CI validates that every referenced test exists, ran in its required job, did
not skip when mandatory, and emitted its declared artifact. A required platform
capability missing from a certification worker fails that profile job; it does
not turn into a green skip. Static guards record their activation gate and
temporary exception/removal owner in the same registry.

The registry also maps every stable `INV-*` in IMPLEMENTATION_PLAN §8 to at
least one named deterministic merge-gating test; real-process/nightly/RC
variants are additive evidence, not the only proof. A red nightly freezes owner
flips and merges touching implicated packages until its retained seed is a
deterministic regression and passes; a red mixed-version/release tier blocks the
release candidate.

### Deterministic re-exec protocol

Crash/cancellation tests never infer a cut point from timing or log polling.
The child process reports arrival at a named registered boundary over a barrier;
the parent injects return, error, panic, EOF, SIGINT, or SIGKILL. An independent
durable effect recorder stores `entered`/`returned` with request, action, box,
and launch IDs so evidence survives child death. It distinguishes adapter entry,
native mutation entry, waiter detachment, actual return/panic, fencing, and
resolution. Reconstruction starts a fresh process with empty queues, caches,
timers, expectations, and in-memory logs.

For each cut, the oracle captures the authoritative store snapshot, provider
entry/return counts, exact target identity, actual permit ownership, durable
terminal/pending result, emitted event sequence, and eventual convergence. A
generated action-family × registered-boundary table makes missing coverage fail
CI (`RC-CRASH-001..002`).

### Deterministic time and concurrency

The test kit supplies a timer-capable manual scheduler/clock—not merely
`Now()`—with timer/ticker/after semantics and an explicit “advance, then drain
ready callbacks” operation. Channel barriers establish entry/order; fixed sleeps
do not. It proves queue concurrency/coalescing, retry-cause preservation, timer
supersession, wait-idle, expectation expiry, shutdown cancellation, and poison
backoff. A targeted `-race` CI job covers queue/executor/cache/owner tests.

Determinism enforcement is transitive and semantic, not a source-substring
check: decision packages cannot range directly over maps; canonical sorted-key
helpers are required. A reflect/type walk rejects func, chan, and unapproved
interface fields in fact/action graphs. The guard scans transitive imports for
ambient I/O/clock/randomness and mutable package state. Every corpus fixture is
executed repeatedly in the merge tier and its normalized plan bytes must be
identical.

### Real versus synthetic provider proof

Scripted executor, process-table, and syscall seams prove partial output,
response loss, replacement between stages, cancellation, late mutation, and
PID/PGID reuse. Real isolated tmux tests prove command shape, immutable target
selection, token observation, rename/recreate, focus changes, copy mode, and
server restart. Linux real-process tests smoke-test pidfd/equivalent capability
and stale-handle safety; reuse interleavings remain synthetic and deterministic.
Unsupported OS tests assert explicit profile refusal/downgrade.

### Versioned numeric certification profiles

Before a row containing “bounded,” “fresh,” “fair,” or a scale number gates a
profile, a versioned profile fixes the freshness duration, backoff cap/jitter
seed, attempts/logs per window, poison threshold, batch ID/byte and evidence
caps, queue/provider capacity, finite workload and service-lag bound, audit
budget/recovery formula, 100K/1M payload distribution, sample count/window, soak
duration, RTO, and heap/goroutine/FD plateau or slope limit. PR CI uses virtual
time and operation counts; pinned nightly/RC workers own wall-clock and resource
thresholds (`RC-PERF-001`, `RC-CERT-001..002`).

### Version and audit independence proof

N/N-1 entries pin the artifact digest/commit, both reader/writer directions,
isolated shared-store copies, owner handoff, and pending operation. The oracle
compares raw unknown fields before/after the old binary and proves N recovers
them without destructive normalization or duplicate effect. Missing pinned
artifacts fail the relevant gate.

An import guard permits the authoritative reference fold to import the
canonical total decoder and pure `Contributions(object)`/lifecycle projector,
but forbids incremental traversal/cursor code, cached object sets, delta
appliers, derived indexes, and their checksum helpers. Independently authored
raw fixtures contain expected objects, tombstones, indexes, and cursors. Tests
compare full structure, inject incremental-mapper omissions, and assert the
audit enters zero providers (`RC-ENTROPY-001`).

### Multi-step action fault tables

Before Start, nudge batching, close/kill, or shutdown canary, its generated
state-by-state fault table names for every step/error: durable request and
convergence state/generation; provider entry/identity; work/alias/identity ownership;
permit/retry eligibility; event count/order; CLI result dimensions and exit;
and the next action after a fresh restart. “Resumable,” “owned,” and “resolved”
without this table are not evidence.
