# S28 — Typed `PendingCreateLease` protocol spec

Status: DRAFT (design for backlog item S28, disposition needs-julian)
Approach: **(a)** pure value type + explicit persist step (patch-returning
transitions), per `backlog.json` S28. Approach (b) — store front-door methods —
is deliberately NOT chosen here to avoid colliding with the in-flight
infra-store-decouple front-door work (PR #3839); the value type can later be
lifted behind a store method without changing its semantics.

Source of truth for current behavior:

- `cmd/gc/session_lifecycle_parallel.go` — `asyncStartSessionStillCurrent`
  (:1603), `asyncStartIdentityMatches` (:1652),
  `asyncStartStaleRuntimeCleanupAllowed` (:1630), `refreshAsyncStartResult`
  (:1532), `clearPendingStartInFlightLease` (:1564), `confirmPendingStart`
  (:1900), `shouldRollbackPendingCreate`/`Info` (:2234/:2245),
  `runningSessionMatchesPendingCreate` (:2249), `rollbackPendingCreate`
  (:2292), `rollbackPendingCreateClearingClaim` (:2315),
  `recoverRunningPendingCreate` (:2118), `stopStaleAsyncStartRuntime` (:1578)
- `cmd/gc/session_reconciler.go` — `pendingCreateStartInFlight`/`Info` (:767/:795),
  `pendingCreateLeaseActive`/`Info` (:819/:833), `pendingCreateNeverStartedExpired`/`Info`,
  `pendingCreateNeverStartedLeaseExpired`/`Info` (:881/:904),
  `pendingCreateNeverStartedTimeout = 10 * time.Minute`
- `cmd/gc/session_reconcile.go` — `pendingCreateAttemptStale`/`Info` (:1317/:1333),
  `staleCreatingState`/`Info`, `staleCreatingStateTimeout = time.Minute`
- `internal/session/lifecycle_transition.go` — Acquire-side patches
  (`state=start-pending` + `pending_create_claim=true` at :96-:99;
  `state=creating` + `pending_create_started_at` at :133-:134),
  `CommitStartedPatch` (+`ClearPendingCreateClaim`), `ClosePatch`
- `internal/session/manager.go` — `NewInstanceToken`, `DefaultGeneration`,
  claim mint at :969-:970, claim clear at :1238-:1239

---

## 1. The typed value: `internal/session/pending_create_lease.go`

### 1.1 Fields

```go
// PendingCreateLease is the typed projection of the optimistic-concurrency
// tuple a session bead carries around a create/start attempt. It is a pure
// value: constructed from a bead or Info snapshot, never holding a store.
// All persisted keys are unchanged on disk; this type only centralizes the
// reads and the transition decisions.
type PendingCreateLease struct {
	SessionID string // bead ID ("" allowed: store-less callers)
	Closed    bool   // bead Status == "closed" (trimmed compare)

	// Identity fence. InstanceToken is authoritative when non-empty;
	// Generation is the legacy fallback, compared as a TRIMMED STRING,
	// never parsed (preserves today's semantics exactly).
	InstanceToken string // strings.TrimSpace(metadata["instance_token"])
	Generation    string // strings.TrimSpace(metadata["generation"])

	// Claim. Claim is the boolean the protocol keys on; ClaimRaw preserves
	// the raw metadata value (Info.PendingCreateClaimMetadata parity — a
	// non-"true" garbage value like "yes" must round-trip observably).
	Claim    bool   // strings.TrimSpace(metadata["pending_create_claim"]) == "true"
	ClaimRaw string // metadata["pending_create_claim"] verbatim

	// Timestamps, kept RAW + parsed-on-demand so unparseable values keep
	// today's per-call-site behavior (some sites treat parse failure as
	// "not in flight", the expiry sites fall back to CreatedAt).
	ClaimStartedAtRaw   string // metadata["pending_create_started_at"] verbatim
	AttemptStartedAtRaw string // metadata["last_woke_at"] verbatim (the in-flight attempt lease)

	// State gate input. StateRaw is the VERBATIM metadata (Info.MetadataState
	// parity); State is the trimmed typed form used by every gate.
	StateRaw string
	State    State // State(strings.TrimSpace(metadata["state"]))

	CreatedAt time.Time // bead CreatedAt (legacy expiry anchor)
}
```

Constructors (these kill the Bead/Info sibling duplication):

```go
func LeaseFromBead(b beads.Bead) PendingCreateLease
func LeaseFromInfo(i Info) PendingCreateLease
```

`LeaseFromBead(b)` and `LeaseFromInfo(InfoFromPersistedBead(b))` MUST be
byte-for-byte equal for every bead (property test, sec 5). `Info` already
projects every field needed (`PendingCreateClaim`, `PendingCreateClaimMetadata`,
`PendingCreateStartedAt`, `MetadataState`, `LastWokeAt`, `InstanceToken`,
`Generation`... — see `internal/session/info_store.go:93-95`).

### 1.2 The verdict enum (state gate return)

```go
// LeaseCommitVerdict is what the async-start commit gate returns when an
// in-flight start result meets the CURRENT bead. Exactly three outcomes;
// the two boolean helpers it replaces are provably mutually exclusive
// (see sec 3.3), so this enum is total and non-overlapping.
type LeaseCommitVerdict int

const (
	// LeaseCommit: the result is still current — commit it against the
	// current bead (adopting the current bead's metadata as base).
	LeaseCommit LeaseCommitVerdict = iota
	// LeaseDiscardStopRuntime: the result is stale AND no live owner claims
	// the runtime — discard the result and stop the spawned runtime
	// (subject to runningSessionMatchesPendingCreate, which stays a
	// separate runtime-side probe; see sec 2 note on stopStaleAsyncStartRuntime).
	LeaseDiscardStopRuntime
	// LeaseDiscardKeepRuntime: the result is stale but the runtime may now
	// belong to a live/committed owner — discard the result and leave the
	// runtime alone. This verdict is the #2073 pane-safety arm.
	LeaseDiscardKeepRuntime
)
```

### 1.3 The ONLY legal transitions / queries

All transitions are pure; mutating ones return a **must-use metadata patch**
(`type LeasePatch map[string]string` with a `//nolint`-free `//go:generate`-less
must-use convention: returned, never ignored — enforced by making the callers'
tests assert the patch is applied). No transition touches a store.

```go
// Acquire mints the claim for a create attempt. It is the typed form of the
// two existing acquire sites (lifecycle_transition.go:96-99 start-pending
// patch; :133-134 creating patch; manager.go:969-970). The patch it returns
// is EXACTLY the keys those sites write today:
//   pending_create_claim   = "true"
//   pending_create_started_at = now.UTC().Format(time.RFC3339)
// (state transitions remain owned by the existing patch constructors; Acquire
// composes INTO them, it does not replace them.)
func (l PendingCreateLease) Acquire(now time.Time) LeasePatch

// Confirm reports the commit-time decisions CommitStartedPatch needs:
//   ConfirmState            = confirmPendingStart(l.StateRaw)
//   ClearPendingCreateClaim = l.Claim
//   StartsAwakeInterval     = confirmPendingStart(l.StateRaw)
// plus the recover-path variant (allowAwake=true adds StateAwake to
// ConfirmState only — NOT to StartsAwakeInterval; see recoverRunningPendingCreate
// :2157-2168). Confirm returns decisions, not a patch: CommitStartedPatch
// stays the single patch mint so the commit write remains ONE atomic batch.
func (l PendingCreateLease) Confirm(allowAwake bool) ConfirmDecision

type ConfirmDecision struct {
	ConfirmState            bool
	ClearPendingCreateClaim bool
	StartsAwakeInterval     bool
}

// Rollback returns the claim-side metadata patch of a rollback:
//   last_woke_at = ""                       (always)
//   session_name = ""                       (iff session_name_explicit == "true")
// and, for the claim-clearing variant (WithClaimClear):
//   ClosePatch(now, StateFailedCreate) keys + pending_create_claim = "" +
//   pending_create_started_at = ""
// The bead CLOSE itself (closeBead / closeFailedCreateBead) stays a caller
// side effect in cmd/gc — store I/O is not a lease concern (Layer 0 confinement).
func (l PendingCreateLease) Rollback(now time.Time, opts RollbackOpts) LeasePatch

// --- Expiry family (Expired(now)) ---

// InFlight: is a start attempt currently in flight? (pendingCreateStartInFlight)
//   (Claim || State == StateCreating) && AttemptStartedAt parses &&
//   now < attempt + effStartupTimeout + staleKeyDetectDelay + 5s
//   where effStartupTimeout = startupTimeout if > 0 else time.Minute.
func (l PendingCreateLease) InFlight(now time.Time, startupTimeout, staleKeyDetectDelay time.Duration) bool

// NeverStartedExpired: claim held, never attempted, past the 10m floor.
// (pendingCreateNeverStartedLeaseExpired) Anchor = ClaimStartedAt if
// parseable, else CreatedAt; zero anchor => expired.
func (l PendingCreateLease) NeverStartedExpired(now time.Time) bool

// AttemptStale: attempt aged past staleCreatingStateTimeout (1m).
// (pendingCreateAttemptStale) Anchor = ClaimStartedAt if parseable, else
// CreatedAt; zero CreatedAt => stale. NOTE: nil-clock short-circuit stays
// at the call sites (clk == nil => false) — the lease method takes a real now.
func (l PendingCreateLease) AttemptStale(now time.Time) bool

// Active: the composite liveness gate (pendingCreateLeaseActive):
//   Claim && ( InFlight || (attempt unset ? !NeverStartedExpired : !AttemptStale) )
func (l PendingCreateLease) Active(now time.Time, startupTimeout, staleKeyDetectDelay time.Duration) bool

// --- Identity + commit gate ---

// SameIdentity: token-authoritative, generation-fallback, vacuous-true when
// the PREPARED side has neither. (asyncStartIdentityMatches — receiver is
// the PREPARED lease, argument is the CURRENT lease.)
func (l PendingCreateLease) SameIdentity(current PendingCreateLease) bool

// CommitVerdict: the fused state gate replacing the
// asyncStartSessionStillCurrent / asyncStartStaleRuntimeCleanupAllowed pair.
// Receiver = PREPARED (snapshot at enqueue), argument = CURRENT (fresh read).
func (l PendingCreateLease) CommitVerdict(current PendingCreateLease) LeaseCommitVerdict
```

`confirmPendingStart`'s state list becomes one unexported function on the
lease file — `stateConfirmsPendingStart(s State) bool` — with the exhaustive
table test; the exported `Confirm` and `CommitVerdict` both call it, so the
state list exists in exactly one place.

`CommitVerdict` decision table (this IS `asyncStartSessionStillCurrent` +
`asyncStartStaleRuntimeCleanupAllowed`, fused, in evaluation order):

| # | Condition (evaluated top-down)                                    | Verdict |
|---|-------------------------------------------------------------------|---------|
| 1 | `current.Closed`                                                   | DiscardStopRuntime |
| 2 | `!l.SameIdentity(current)`                                         | DiscardStopRuntime |
| 3 | `current.State` is awake or active                                 | **Commit** (commit-anyway exception) |
| 4 | `l.Claim && !current.Claim` (claim cleared from under us)          | DiscardStopRuntime — current state is neither awake nor active here (row 3 already caught those), which is exactly today's `currentState != Awake && != Active` guard |
| 5 | `stateConfirmsPendingStart(current.State)` ("", start-pending, creating, asleep, drained) | **Commit** |
| 6 | otherwise (draining, archived, quarantined, ...)                   | DiscardKeepRuntime... **see caveat** |

**Row 6 caveat (load-bearing, verify in implementation):** today's
`asyncStartStaleRuntimeCleanupAllowed` row-6 equivalent returns
`!confirmPendingStart && state != awake && state != active`, i.e. cleanup IS
allowed for draining/archived/quarantined. So row 6 is
**DiscardStopRuntime**, and `LeaseDiscardKeepRuntime` is produced only by rows
3-analog in the *cleanup* path — which cannot be reached because row 3
already returned Commit. Resolution: the two booleans are mutually exclusive
but NOT complementary. The exact fusion is:

```
verdict(l, current):
  if current.Closed            -> DiscardStopRuntime        // StillCurrent=false, cleanup=true
  if !SameIdentity             -> DiscardStopRuntime        // false, true
  if current awake|active      -> Commit                    // true, (cleanup=false)
  if l.Claim && !current.Claim -> DiscardStopRuntime        // false, true  (awake/active excluded by prev row)
  if confirms(current.State)   -> Commit                    // true, (cleanup=false)
  else                         -> DiscardStopRuntime        // false, true
```

Therefore **`LeaseDiscardKeepRuntime` is unreachable through `CommitVerdict`
on the pure state gate** — it exists for the REFRESH-level wrapper (sec 1.4)
where the store `Get` fails: today that path discards WITHOUT cleanup
(`refreshAsyncStartResult` returns `cleanupRuntime=false` on Get error) and
releases the in-flight lease instead. Keeping the three-valued enum makes
that refresh outcome typed instead of a `(bool, bool, bool)` tuple.

(Implementation MUST re-derive this table from the code at migration time
with the parity test in sec 5.2 — the table above is the contract, the parity
test is the proof.)

### 1.4 Refresh-level outcome (thin wrapper in cmd/gc, not in the lease)

`refreshAsyncStartResult`'s 4-tuple `(result, ok, cleanupRuntime,
releaseInFlight)` becomes:

```go
type asyncRefreshOutcome int

const (
	refreshAdoptCurrent   asyncRefreshOutcome = iota // ok=true: commit against current bead
	refreshNoStore                                    // ok=true, store-less/ID-less path
	refreshGetFailed                                  // discard, NO runtime stop, release in-flight lease
	refreshCommandStale                               // discard, stop runtime, release in-flight lease
	refreshLeaseStale                                 // discard, stop-runtime per CommitVerdict, do NOT release lease
)
```

This wrapper stays in `cmd/gc/session_lifecycle_parallel.go` (it owns store
I/O and the `command` metadata compare, which is not part of the lease
tuple). Only `refreshLeaseStale`'s stop/keep decision comes from
`CommitVerdict`.

---

## 2. Mapping table (the correctness contract)

Every existing function maps to exactly one lease operation/verdict. "Same
bytes" means the persisted keys and values are identical before/after.

| Existing function (file:line) | Becomes | Notes |
|---|---|---|
| `asyncStartIdentityMatches` (:1652) | `prepared.SameIdentity(current)` | Pure rename; trimming + vacuous-true preserved |
| `asyncStartSessionStillCurrent` (:1603) | `prepared.CommitVerdict(current) == LeaseCommit` | |
| `asyncStartStaleRuntimeCleanupAllowed` (:1630) | `prepared.CommitVerdict(current) != LeaseCommit` implies stop-runtime today (sec 1.3 caveat); wrapper consumes the verdict | The two booleans fuse; mutual exclusion proven by test sec 5.1-T7 |
| `confirmPendingStart` (:1900) | `stateConfirmsPendingStart(State)` (unexported, single site) via `Confirm()` / `CommitVerdict()` | Callers at :1948, :1953, :2157, :2168 route through `Confirm(allowAwake)` |
| `refreshAsyncStartResult` (:1532) | thin wrapper returning `asyncRefreshOutcome` (sec 1.4) | Store `Get` + command-stale check stay here |
| `clearPendingStartInFlightLease` (:1564) | `lease.Rollback(now, RollbackOpts{AttemptOnly: true})` producing `{last_woke_at: ""}`; the SetMarker write + raw-bead mirror stay in cmd/gc | Return-batch (Step 6d fold) contract unchanged: batch on persisted write, nil otherwise |
| `shouldRollbackPendingCreate` (:2234) | `lease.Claim` (via `LeaseFromBead`) | |
| `shouldRollbackPendingCreateInfo` (:2245) | `lease.Claim` (via `LeaseFromInfo`) | The Bead/Info sibling pair collapses |
| `rollbackPendingCreate` (:2292) | `lease.Rollback(now, RollbackOpts{})` patch + caller-side `closeBead` (store-only close unchanged) | Patch = last_woke_at clear + conditional session_name clear; NO Closed change in fold (doc contract at :2285-2291 preserved verbatim) |
| `rollbackPendingCreateClearingClaim` (:2315) | `lease.Rollback(now, RollbackOpts{ClearClaim: true})` + caller-side `closeFailedCreateBead`; claim/ClosePatch keys mirrored only when the close succeeds | Ordering preserved: close-success gates the claim-clear mirror |
| `recoverRunningPendingCreate` (:2118) | unchanged shape; its `CommitStartedPatchInput` fields come from `lease.Confirm(allowAwake=true)` | `ClearPendingCreateClaim: true` hard-code preserved (caller gate guarantees Claim) — assert `lease.Claim` in a debug-only check, do not re-derive |
| `pendingCreateStartInFlight` (:767) / `Info` (:795) | `lease.InFlight(now, startupTimeout, staleKeyDetectDelay)` | The `startupTimeout<=0 => 1m` floor and `+staleKeyDetectDelay+5s` window move into the method VERBATIM; sibling pair collapses |
| `pendingCreateLeaseActive` (:819) / `Info` (:833) | `lease.Active(now, ...)` | Sibling pair collapses |
| `pendingCreateNeverStartedLeaseExpired` (:881) / `Info` (:904) | `lease.NeverStartedExpired(now)` | 10m const moves to internal/session as `PendingCreateNeverStartedTimeout` |
| `pendingCreateAttemptStale` (:1317) / `Info` (:1333) | `lease.AttemptStale(now)` | 1m `staleCreatingStateTimeout` moves alongside; `clk == nil => false` short-circuit STAYS at call sites |
| `pendingCreateNeverStartedExpired` / `Info` | `lease.Claim && pendingCreateRollbackState(lease.StateRaw) && lease.NeverStartedExpired(now)` | `pendingCreateRollbackState` may stay in cmd/gc or move; either way single-sited |
| `staleCreatingState` / `Info` | `lease.State == StateCreating && lease.AttemptStale(now)` | |
| `runningSessionMatchesPendingCreate` (:2249) | **UNCHANGED** — stays in cmd/gc | It probes the LIVE RUNTIME (`sp.GetMeta`), a Layer-0 side effect, not lease state. It consumes `lease.InstanceToken`/`lease.Generation`/`lease.SessionID` as inputs only |
| `stopStaleAsyncStartRuntime` (:1578) | **UNCHANGED** — consumes the verdict | Provider `Stop` is Layer-0 |
| Acquire sites (`lifecycle_transition.go:96-99, :133-134`; `manager.go:969-970`) | compose `lease.Acquire(now)` into their existing patches | Optional in phase 1; may remain literal keys with a sync test |

---

## 3. Invariants that MUST be preserved (exact boolean conditions)

### 3.1 #1542-family — stuck-creating zombies (commit-anyway + token identity)

1. **Token-authoritative identity.** Today:
   `prepared.instance_token != "" => match iff current.instance_token == prepared.instance_token`
   (generation IGNORED). Lease: `SameIdentity` identical. A generation bump
   with matching token MUST still commit — rejecting on generation drift alone
   is the original #1542 zombie bug (comment :1598-1602).
2. **Vacuous-true legacy path.** `prepared.token == "" && prepared.generation == "" => match`.
   Preserved bit-for-bit (pre-instance_token snapshots).
3. **Commit-anyway on awake/active** (:1611-1619). Today:
   `identityMatch && (state==awake || state==active) => commit`, EVEN IF the
   claim was cleared. The result's metadata (creation_complete_at,
   runtime_epoch) must land. Lease: `CommitVerdict` row 3 fires BEFORE the
   claim-cleared row 4. Order is load-bearing.

### 3.2 #2073-family — pane leak on rollback/stale discard

4. **Claim-cleared-from-under-us** (:1620-1627). Today:
   `identityMatch && state not in {awake,active} && preparedClaim && !currentClaim => discard`
   — a concurrent reconciler already rolled the create back; committing would
   stomp its decision. AND cleanup is allowed in exactly this case only when
   `state != awake && state != active` (already guaranteed by row order).
5. **Runtime stop is double-gated.** A stale discard only stops the runtime if
   `runningSessionMatchesPendingCreate(bead, name, sp)` ALSO matches
   (`stopStaleAsyncStartRuntime` :1583) — i.e. we never kill a pane whose live
   `GC_SESSION_ID`/`GC_INSTANCE_TOKEN` says it belongs to someone else. The
   lease verdict does NOT absorb this probe; the two-gate structure survives.
6. **Get-error path never stops the runtime** and DOES release the in-flight
   lease (`refreshAsyncStartResult` :1540 → `(false, false, true)` →
   outcome `async_start_refresh_failed` + `clearPendingStartInFlightLease`).
   A lease-stale path (:1546-1548) does NOT release the in-flight lease
   (the current owner may hold it) — `releaseInFlight=false`. This asymmetry
   is the "discarded result leaves claim set forever" vs "stomped a live
   owner's lease" tension; `asyncRefreshOutcome` encodes it as two distinct
   enum values.
7. **Rollback patch shape.** `rollbackPendingCreate` clears ONLY
   `last_woke_at` (+ `session_name` iff `session_name_explicit=="true"`) on
   the raw bead and returns that batch; the close is store-only and the
   returned fold carries NO Closed change (:2285-2291). The claim-clearing
   variant mirrors ClosePatch + `pending_create_claim=""` +
   `pending_create_started_at=""` ONLY when `closeFailedCreateBead` succeeds.

### 3.3 #2895-family — TTL-reap stuck start-pending / dedupe pending creates

8. **In-flight window arithmetic** (exact):
   `inFlight := (claim=="true" || state=="creating") && parse(last_woke_at) ok && now < lastWoke + effTimeout + staleKeyDetectDelay + 5s`
   with `effTimeout = startupTimeout if startupTimeout > 0 else 1m`.
   Unparseable/empty `last_woke_at` => not in flight.
9. **Never-started expiry** (exact):
   `claim=="true" && last_woke_at trimmed == "" && (anchor.IsZero() => expired; else now.After(anchor + 10m))`
   with `anchor = parse(pending_create_started_at) if ok else CreatedAt`.
10. **Attempt staleness** (exact):
    `anchor := parse(pending_create_started_at) if ok else CreatedAt; CreatedAt.IsZero() && !parsed => stale; else !now.Before(anchor + 1m)`
    — note `!now.Before(...)` (>=), not `now.After(...)` (>); off-by-one at
    the boundary instant is observable in table tests and must match.
11. **Composite Active gate** (exact, :819):
    `claim && (inFlight || (last_woke_at=="" ? !neverStartedExpired : !attemptStale))`.
12. **Mutual exclusion, not complementarity:** for all (prepared, current):
    `asyncStartSessionStillCurrent && asyncStartStaleRuntimeCleanupAllowed`
    is NEVER true, but both-false IS reachable only via... (verify: today
    both-false requires `identityMatch && !closed && state not
    awake/active && !claimClearedCase && !confirms(state)` on the
    stillCurrent side vs the SAME condition returning true on the cleanup
    side — i.e. both-false is UNREACHABLE too; the pair is an exact
    complement on the pure-state domain). The parity test (5.2-P1) proves
    whichever holds and freezes it; the enum then encodes it structurally.

### 3.4 #3849-family — crash-loop on resume through the commit path

13. **Atomic confirm batch.** The state->active transition, hash writes,
    `pending_create_claim` clear, and `creation_complete_at` marker land in
    ONE `ApplyPatch` (`CommitStartedPatch`, :1942-1955). The lease refactor
    must keep `Confirm` returning DECISIONS consumed by that single patch —
    never split into two writes (splitting reopens the sweep-observes-
    transient-state window documented at :1938-1941).
14. **Recover path awake-exception.** `recoverRunningPendingCreate` confirms
    state when `confirmPendingStart(state) || state == awake` but keys
    `StartsAwakeInterval` on `confirmPendingStart(state)` ONLY (:2157-2168) —
    recovering an already-awake runtime must not reset the awake interval.
    `Confirm(allowAwake=true)` must preserve this split exactly.
15. **Residue folds.** `pendingCreateResidueFold` (:2223) and the
    `instance_token` mint fold (:2185-2192) are Step-6d snapshot-coherence
    contracts orthogonal to the lease; they must survive untouched.
16. **State gate list frozen:** `{"", "start-pending", "creating", "asleep", "drained"} => confirm; awake/active => special; all else => no`.
    Adding/removing a state is a semantic change, out of scope for S28.

### 3.5 Repo-wide invariants

- **Zero hardcoded roles** — the lease is role-free by construction.
- **Worker boundary** — no new `session.NewManager(`/`worker.SessionHandle`/
  `sessionlog` imports in non-test `cmd/gc`; no new `session.Manager.Create*`
  call sites. The refactor only moves PURE decision logic into
  `internal/session`; all store/provider I/O stays at today's call sites.
- **Layering** — `pending_create_lease.go` imports only stdlib (+`beads` for
  the constructor, which `internal/session` already imports). No upward
  imports; side effects stay in Layer 0 (`sp.Stop`, `ApplyPatch`, `closeBead`
  remain caller-side).
- **Typed wire / typed events** — untouched (no HTTP/SSE/event surface in
  this refactor; no new event types, so no `RegisterPayload` changes).
- **`config.Agent` field-sync** — untouched (no config fields added).
- **Persisted keys unchanged** — no bead-format migration; every key name and
  value format ("true"/"", RFC3339) is byte-identical.

---

## 4. Behavior-preserving migration plan

Phased per repo rules (≤5 files per phase, verify between phases).

**Phase 1 — the type + exhaustive tests (new files only).**
`internal/session/pending_create_lease.go` +
`pending_create_lease_test.go`. Implement `PendingCreateLease`,
`LeaseFromBead/Info`, `SameIdentity`, `CommitVerdict`,
`stateConfirmsPendingStart`, `Confirm`, `Rollback`, `InFlight`,
`NeverStartedExpired`, `AttemptStale`, `Active`, and move the two duration
constants (exported: `PendingCreateNeverStartedTimeout`,
`StaleCreatingStateTimeout` — cmd/gc aliases them). No caller changes; the
parity tests (sec 5.2) run the NEW code against the OLD functions
side-by-side in a `cmd/gc` test file. Gate: `make test`, `go vet ./...`.

**Phase 2 — cut over the async-start commit gate (1 file).**
`cmd/gc/session_lifecycle_parallel.go`: `asyncStartSessionStillCurrent`,
`asyncStartIdentityMatches`, `asyncStartStaleRuntimeCleanupAllowed`,
`confirmPendingStart`, `shouldRollbackPendingCreate/Info` become one-line
delegations (`func asyncStartIdentityMatches(p, c beads.Bead) bool { return
sessionpkg.LeaseFromBead(p).SameIdentity(sessionpkg.LeaseFromBead(c)) }`).
`refreshAsyncStartResult` internally switches to `asyncRefreshOutcome` but
keeps its public 4-tuple signature this phase. All existing tests
(`TestReconcileSessionBeads_*`, async-start tests) pass UNCHANGED — that is
the parity proof. Gate: full fast suite.

**Phase 3 — cut over the expiry family (2 files).**
`cmd/gc/session_reconciler.go` + `cmd/gc/session_reconcile.go`: the
`pendingCreate*`/`Info` sibling pairs delegate to lease methods; the `clk ==
nil` short-circuits stay at the call sites. Delete nothing yet. Gate: full
fast suite + `make test-cmd-gc-process-parallel`.

**Phase 4 — collapse the delegation shims where call-site count is small.**
Inline `shouldRollbackPendingCreateInfo`-style trivial shims at their callers
(grep protocol: direct calls, type refs, string literals, test files — per
No-Semantic-Search rule). Keep `asyncStart*` names if they read better as
domain-named wrappers; deleting them is optional, not the goal. Gate: full
fast suite; `git diff` review that NO persisted key or write ordering moved.

**Explicit non-goals:** no store front-door methods (approach b), no change
to `runningSessionMatchesPendingCreate`, `stopStaleAsyncStartRuntime`,
`commitStartResultTraced` write ordering, rollback close semantics, residue
folds, or any acquire-site patch. No new events, no wire changes.

---

## 5. Test plan

### 5.1 Table-driven lease tests (`internal/session/pending_create_lease_test.go`)

- **T1 `SameIdentity`:** all 9 combos of {prepared token empty/set,
  current token empty/set/mismatched} × generation {empty/match/mismatch},
  incl. whitespace-padded values (trim semantics) and the vacuous-true row.
- **T2 `CommitVerdict`:** exhaustive grid — {closed, identity-mismatch} ×
  states {"", start-pending, creating, asleep, drained, awake, active,
  draining, archived, quarantined, garbage} × claim {prepared∈{t,f}} ×
  {current∈{t,f}} — every cell asserts one of the three verdicts, with the
  #1542/#2073 rows called out by name (commit-anyway-awake,
  claim-cleared-discard, generation-drift-with-token-commits).
- **T3 `stateConfirmsPendingStart`:** the frozen list (invariant 16).
- **T4 expiry family:** boundary-instant cases for the >= vs > distinction
  (invariant 10), the `startupTimeout<=0 => 1m` floor, unparseable/empty
  timestamps, zero CreatedAt, `pending_create_started_at` overriding
  CreatedAt as anchor, the exact `+staleKeyDetectDelay+5s` window edge.
- **T5 `Confirm(allowAwake)`:** awake confirms state but NOT
  StartsAwakeInterval (invariant 14); claim drives ClearPendingCreateClaim.
- **T6 `Rollback` patches:** attempt-only vs full; `session_name_explicit`
  gating; claim-clear variant's ClosePatch keys byte-compared against
  `sessionpkg.ClosePatch(now, StateFailedCreate)`.
- **T7 verdict totality:** property test over generated leases proving each
  input yields exactly one verdict and (per invariant 12) the fused verdict
  equals the frozen relationship between the two legacy booleans.
- **T8 constructor parity:** property test `LeaseFromBead(b) ==
  LeaseFromInfo(InfoFromPersistedBead(b))` over randomized metadata
  (incl. garbage values like claim="yes", padded tokens, non-RFC3339 times).

### 5.2 Migration parity tests (`cmd/gc`, written in Phase 1, kept through Phase 4)

- **P1 old-vs-new oracle:** for a generated corpus of (prepared, current)
  bead pairs (randomized over the 4 keys + status + the state list + garbage),
  assert `asyncStartSessionStillCurrent(p,c) == (verdict==LeaseCommit)` and
  `asyncStartStaleRuntimeCleanupAllowed(p,c) == (verdict==LeaseDiscardStopRuntime)`
  (or whatever exact relationship P1 discovers — the test freezes it BEFORE
  the cutover and must not be edited during it).
- **P2 expiry oracles:** same corpus against
  `pendingCreateStartInFlight/LeaseActive/NeverStartedLeaseExpired/AttemptStale`
  and their Info siblings, with randomized `clk` times and startupTimeouts
  (incl. 0 and negative).
- **P3 existing behavior tests stay green untouched:**
  `TestReconcileSessionBeads_RollsBackPendingCreateOnProviderError`, the
  stale-async-start tests around `refreshAsyncStartResult`, the
  recover-running tests — zero edits to their assertions is the
  behavior-preservation gate.
- **P4 write-shape regression:** a fake-store test asserting the rollback and
  confirm paths issue the SAME patch batches (key sets + values) before and
  after, including the fold-batch return contracts (nil on failed persist).

### 5.3 Suites

`make test` per phase; `make test-cmd-gc-process-parallel` after Phases 2-3;
`go vet ./...`; pre-commit hook. No dashboard/API surface touched, so
`make dashboard-check` is not required.

---

## 6. Open questions for Julian

1. Row-6/invariant-12 resolution: the spec predicts the legacy boolean pair is
   an exact complement on the pure-state domain, making
   `LeaseDiscardKeepRuntime` reachable only from the refresh wrapper. If P1
   falsifies this, the enum stays but the table gets the corrected row.
2. Should `Acquire` cut over in this item at all, or stay a documented
   composition target (the acquire sites already live in internal/session and
   are single-sited)? Spec marks it optional (Phase 4+).
3. Exported vs unexported constants: exporting the two timeouts moves them out
   of cmd/gc; alternative is lease methods taking them as params everywhere
   (more explicit, noisier call sites).
