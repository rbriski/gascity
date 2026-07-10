# Work-Ownership Integrity: claim fence, guarded verbs, tier-complete leases

> **Status**: Design v2 (2026-07-10). Epic `ga-furrj5`. v1 was red-teamed by a
> 5-lens adversarial workflow (`wf_ee0a8783-375`: 5 fatal / 29 major / 14 minor
> findings); every fatal and major is folded into this revision — the largest
> being §2.3's authorization model, which v1 lacked entirely.
> **Decision inputs**: `engdocs/research/beads-claims-cas-adoption.md` (R1),
> `engdocs/research/beads-leases-for-failure-recovery.md` (R2),
> `engdocs/research/beads-failure-recovery-owned.md` (R3), a 9-agent
> verification pass over all three (`wf_74917d1c-1f4`), and exploration passes
> over gascity @ `738c11517`/`origin/main 64414cf18`, beads @ `origin/main
> 2f778fa8c`, and `internal/rollout` @ `62e4c6828`.
> **Owner ruling (Julian, 2026-07-09)**: leases must be tier-complete — they
> work on **all tables** (`issues` and `wisps`).

---

## 1. Problem

Gas City has exactly one atomic ownership primitive — the beads claim — and
implements every other ownership transition as read-modify-write with real
races, over an identity model that cannot distinguish process incarnations.
Three problem classes, all verified against source:

- **P1 — Release stomping.** Orchestrator release paths (`ReleaseWorkBead`
  `cmd/gc/work_assignment.go:134`, `releaseOrphanedPoolAssignment`
  `cmd/gc/pool_session_name.go:362`, retired/closed-session sweeps in
  `cmd/gc/session_beads.go`, generated `on_death`/`on_boot` shell in
  `internal/config/workquery.go:654,699`) re-read, compare, then issue an
  **unconditional** `store.Update`. A worker re-claiming in the window is
  silently stomped.
- **P2 — Zombie mutation.** A reclaimed-but-still-live worker can mutate or
  close a bead it no longer owns. beads' close/update statements carry no
  ownership predicate; the core work formula completes via plain
  `bd update "$WORK_BEAD_ID" --status=closed`
  (`internal/bootstrap/packs/core/formulas/mol-do-work.toml:95-123`).
  `row_lock` forces only *concurrent* transactions to conflict — a
  *sequential* zombie write lands cleanly.
  **Scope note (red-team):** P2 closure in this program applies to **claimed
  rows**. Two bead shapes are outside the Require predicate and are named
  residual exposure until their follow-ups land: (a) convoy *target* beads,
  which sling deliberately leaves unassigned (`internal/sling/sling_core.go:150-158`)
  and workers close without claiming — follow-up: formulas claim the target
  bead immediately before closing it; (b) open+preassigned attempt beads
  (`internal/dispatch/retry.go:551`, continuation preassignments) — follow-up:
  the `ready_assignment` adoption branch performs the same fenced
  claim/transfer as the `existing_assignment` branch.
- **P3 — Incarnation blindness.** The claim assignee is the incarnation-stable
  session identity (`session_name` preferred, `cmd/gc/cmd_hook.go:367`);
  named/singleton sessions keep the same session bead across wake/restart
  (`cmd/gc/session_reconciler.go:3541`). A fresh incarnation — or a same-name
  replacement — is indistinguishable from the stale one. GC already mints a
  per-incarnation credential (`GC_INSTANCE_TOKEN`,
  `internal/session/lifecycle.go:21`, re-minted per wake in
  `cmd/gc/session_wake.go:43`, persisted on the session bead as
  `instance_token`) but it never reaches the beads claim.

Separately, per the owner ruling, the beads lease stack (upstream #4537) must
become **tier-complete**: today claims stamp lease columns on wisp-table rows
that heartbeat rejects and reclaim never scans — durable `no_history` workflow
beads (GC's bd-1.0.5 default for workflow policy,
`cmd/gc/bead_policy_store.go:304-325`) land in that excluded table.

### Non-goals (explicit)

- **Not** replacing GC's session-death detection with lease expiry. Detection
  authority stays in GC; leases are executed state. The Stage C "authority
  flip" is a separate, evidence-gated decision.
- **Not** solving duplicate *external* side effects (git pushes, PR creation
  by a zombie). Fencing protects the store only. Named open problem.
- **Not** whole-row CAS (`revision`, PR #4682) on this path — the revision
  nonce is invalidated by any write and is the wrong ownership token. CAS
  proceeds independently for control-plane metadata fences.
- **Not** a bulk atomic-or-error release verb, a `ReclaimPatch` DSL, or a
  GC-side parallel lease table (rejected with evidence in R3 §3).
- **Not** (yet) fencing dep/label mutations or `bd sql` passthrough — raw SQL
  is dispatch layer 5 (§2.2) and is *explicitly unenforceable*; its GC uses
  are inventoried and migrated off (§2.4), and a doctor check flags residual
  use against claimed rows.

---

## 2. Design

### 2.0 Overview

| Problem | Mechanism | Enforced where |
| --- | --- | --- |
| P1 stomping | **Guarded transition verbs** — `IfAssignee`/`IfFence` folded into the WHERE | beads storage, all dispatch layers |
| P2 zombie writes | **Holder token** — ambient per-incarnation credential recorded at claim, checked on in-place mutations of claimed rows under enforcement | beads storage + enforcement mode |
| P3 incarnation blindness | **Fenced adoption/transfer**, gated GC-side by the session bead's current `instance_token` | GC hook/reconciler + beads transfer verb |
| Tier-complete leases | **Auto-lease config + requested leases**, lease ops routed to both tables | beads (Stage B) |

### 2.1 The fence: `claim_fence`

A **monotonic `BIGINT NOT NULL DEFAULT 0`** on `issues` **and** `wisps`
(guarded-ALTER pattern of `0054_add_lease_columns`; `cliMigrations` case).
Distinct from `row_lock` (random, anti-cell-merge, every write) and CAS
`revision` (random nonce, every write): `claim_fence` increments **only on
ownership transitions**, defined *semantically* as any write that changes
`(status ∈ {claimable set}, assignee)` ownership context:

- claim (`ClaimIssueInTx`) — new owner
- unclaim/release (`UnclaimIssueInTx`)
- lease reclaim (`ReclaimExpiredLeasesInTx`)
- assignee change through `updateIssueInTx`
- **reopen = the closed→open status transition inside `updateIssueInTx`**
  (red-team: `ReopenIssue` on dolt/embeddeddolt is implemented as a
  status-only `UpdateIssue`, so keying the bump to the dedicated verb would
  miss the primary path; `ReopenIssueInTx` kept in sync for the proxied path)
- transfer (§2.3)
- **import/upsert that changes the assignee** (`insertIssueIntoTable`
  ON-DUPLICATE-KEY path and the domain/db upsert): bump fence and **clear
  `holder_token`** when `VALUES(assignee) != assignee` — otherwise an import
  could change ownership fence-invisibly, or strand a stale token that locks
  out the new legitimate assignee
- table moves (`DemoteToWisp`/`PromoteFromEphemeral` rebuild rows through
  explicit column lists): `claim_fence`/`holder_token` are **carried across**,
  with a conformance case proving survival

Same-owner idempotent re-claim (no-write path) does **not** bump — except see
§2.3 for the differing-ambient-token case.

**Invariants (package-guard-tested, semantic not name-based):**
1. every ownership transition bumps `claim_fence`;
2. **every fence bump rewrites `row_lock` in the same statement** (a
   monotonic cell is exactly what Dolt cell-merges silently — two concurrent
   N→N+1 bumps produce identical cells and no conflict without `row_lock`);
3. close does not bump (status predicates carry that transition — documented);
4. the guard test enumerates transitions by row-delta (status/assignee
   change), so path-divergent entry points cannot skip it.

Surfacing: `claim_fence` joins the **canonical trailing column order**
`..., lease_expires_at, heartbeat_at, claim_fence, holder_token, revision`
(coordinated with CAS PR #4682, which rebases `revision` to the tail; a
value-level sentinel scan test asserts each trailing column lands in the right
`types.Issue` field — the const-equality parity test alone would pass a silent
fence↔revision swap). Exposed on `types.Issue.ClaimFence`, `ClaimResult`,
`bd --json`, GC `beads.Bead.ClaimFence`, and the `gc hook --claim` JSON.

`holder_token` is the opposite: **never surfaced** in any read/scan/JSON
output (OQ3 closed — a `bd show`-recoverable token collapses into R3's
defeated quoted-fence class). It exists only in the enforcement WHERE and in
the `ClaimResult` returned to the stamping claimant; a package-guard test
asserts it stays out of `IssueSelectColumns`.

### 2.2 Guarded verbs and dispatch coverage

WHERE-folding pattern from the CAS branch (`issueops/conditional.go` @
`f9a929f03`): fold the precondition into the single UPDATE; on
`RowsAffected()==0`, re-read in the same tx and return a typed
`*storage.PreconditionFailedError{ID, ExpectedAssignee, ExpectedFence,
CurrentAssignee, CurrentFence}`.

- `UnclaimIssueInTx(..., IfAssignee(v), IfFence(n))` — extends PR #4675
  (owner-checked, wisp-routed, lease-clearing, `row_lock`-rewriting unclaim).
- `CloseIssueInTx` / `updateIssueInTx` gain the same optional guards
  (`--if-assignee` / `--if-fence`; wiring model `cmd/bd/if_revision.go`).
- `bd claim transfer` (§2.3).
- CLI contract: conflict ⇒ exit 9 + JSON `{code:"ownership_conflict", id,
  expected_assignee|expected_fence, current_assignee, current_fence}`;
  unsupported store ⇒ exit 13 + `{code:"conditional_write_unsupported"}`.
  Claim loss ⇒ `{code:"already_claimed", id, holder}` + non-zero exit
  (#4675). **Back-compat rule:** typed JSON bodies are additive; the existing
  human-readable conflict phrasings ("already assigned"/"claimed by") are
  frozen until the fleet-minimum gc carries the typed-body parser.

**Dispatch layers (all five):**

1. `issueops.*InTx` — covers `DoltStore`, `EmbeddedDoltStore`, and the
   backend branch's `sqlkit` stores.
2. **domain/db (proxied-server UOW)** — hand-rolled `Claim`/`Update` bypass
   issueops; guards, fence bumps, and enforcement checks are added there
   explicitly, **and threaded through the use-case plumbing**
   (`update_proxied_server.go` → `UpdateSpec` → `ApplyUpdate` → repo). Until
   threaded, a supplied `--if-*` flag on the proxied path **refuses with exit
   13** (mirroring `asConditionalWriter`) rather than silently dropping the
   guard. Proxied integration tests in every slice that touches verbs.
3. `HookFiringStore` decorator passthrough.
4. Stores that cannot guard fail closed with the typed unsupported error.
5. **Raw SQL (`bd sql`, GC's `ReleaseIfCurrent` UPDATE)** — *unenforceable by
   construction*. Consequence: GC's `BdStore.ReleaseIfCurrent` raw UPDATE is
   itself an ownership transition that would violate invariant 1 (no fence
   bump) and — from the moment lease columns exist — beads' own `row_lock`
   invariant. Plan: (a) immediately (pre-pin-bump) the legacy SQL adds
   `row_lock` rewrite + lease-column clearing + fence bump to its UPDATE;
   (b) once the pinned bd carries guarded unclaim, `ReleaseIfCurrent`/
   `ReleaseIfHeld` are reimplemented on the verb and the raw SQL is deleted;
   (c) a doctor check flags raw-SQL writes against claimed rows (reaper.sh's
   raw wisp closes are inventoried and either routed through bd or documented
   as an accepted, monitored bypass).

### 2.3 Authorization model (the red-team's central fix)

v1 had no principal model and would have blocked GC's own recovery paths
under Require (controller writes as `BEADS_ACTOR=controller`
`cmd/gc/bd_env.go:229`; orders as `order:<name>`; assignees are worker session
names). v2 defines **two verb classes with different authorization channels**:

**Class T — ownership-transition verbs** (`unclaim`, `transfer`, `reclaim`):
- Authorized by a **satisfied explicit guard**: a transition verb carrying
  `IfAssignee` and/or `IfFence` that matches is authorized *regardless of
  caller actor/token* — the guard **is** the credential (a fenced third-party
  release is precisely the P1 mechanism; the caller proves it acted on a
  current read of the ownership state).
- An **unguarded** transition verb keeps #4675 semantics: owner-only
  (`actor == assignee`), `ErrNotOwner` otherwise.
- `--force` (ownership override): bypasses the owner check **only**; supplied
  `--if-*` guards are still evaluated (`--force` never skips a guard). The
  audit event enumerates exactly which checks were bypassed, with
  expected/current values. Routine orchestrator paths use guards, **never**
  `--force`; `--force` is for human/admin repair.

**Class M — in-place mutations** (`update` fields/metadata, `close`,
heartbeat) on rows with `status='in_progress' AND assignee != ''`:
- Under `enforcement=require`: caller must present `BEADS_ACTOR == assignee`
  **and** ambient `BEADS_HOLDER_TOKEN == holder_token` (the zombie axis).
  Empty recorded token ⇒ actor-only (legacy rows; see the pre-Require
  inventory gate, §2.4). Cross-actor class-M writes (e.g. dispatcher stamping
  `gc.outcome` on another actor's row) must either move to a class-T shape,
  present a satisfied `--if-*` guard (same channel as class T), or `--force`.
- Under `advisory`: mismatches land but emit a typed `ownership_advisory`
  event **labeled with its class**: `(a) cross-actor infra`, `(b) empty-token
  legacy`, `(c) actor==assignee + token mismatch` — class (c) is the true
  zombie signal; the Require flip is gated on (c) explained-to-zero and (a)
  fully converted to guarded verbs, not on raw event-watching.
- Under `off`: token recorded when present, never checked (byte-compatible).

**Transfer and adoption (P3), precisely:**

- `bd claim transfer <id> --to-actor <a> --to-token <t> [--if-assignee]
  [--if-fence]` — class T. Atomically re-stamps `assignee`+`holder_token`,
  bumps `claim_fence`, rewrites `row_lock`, records a typed event. The
  **recipient token is stamped at transfer time** — the orchestrator always
  knows it (it minted the recipient's `instance_token` and persisted it on
  the session bead). **Never** an empty-token handoff window (v1's OQ1 answer
  is rejected: empty ⇒ actor-only reopens P3 for same-name replacement, with
  an unbounded window). For a not-yet-existing recipient, use
  guarded-release→claim, or stamp the reserved sentinel token
  (`holder_token='!'`) that fails **all** matches until a fenced adoption
  re-stamps it.
- **Adoption is gated GC-side** — only GC can distinguish incarnations:
  `gc hook --claim` adoption (both the `existing_assignment` *and*
  `ready_assignment` branches) verifies the caller's ambient
  `GC_INSTANCE_TOKEN` equals the session bead's **current** `instance_token`
  before issuing the fenced transfer. A zombie incarnation running the normal
  worker loop fails this check (its env token is stale) — this kills the
  zombie/replacement transfer ping-pong the red team found.
- **Wake invariant:** the reconciler re-stamps `holder_token` **at
  `preWakeCommit` time** via fenced transfer (it just minted the new token and
  persisted it to the session bead, before the new process starts) — so a
  continuation worker resuming mid-molecule presents a token that already
  matches, and its completion is never rejected as a zombie write. This is
  the "legitimate wake" row of the zombie test matrix.
- **Detached executors:** while `gc.detached` probe metadata is live,
  adoption **skips** the fenced transfer (the detached executor holds the
  spawning incarnation's token and is still legitimate); transfer happens
  only after the probe reports dead. Test row: detached executor completes
  after same-name wake.
- **Same-actor re-claim with a differing non-empty ambient token** (zombie or
  pre-adoption resume hitting the idempotent no-write path): returns a typed
  `incarnation_conflict` instead of silent success, prompting re-hook (which
  performs the verified adoption). Never a silent token overwrite; never a
  silent success that leaves the fence/token stale.
- **Residual risk (named):** after any release, a still-live zombie can
  *fresh-claim* the open row and legitimately stamp its own token — beads
  cannot distinguish incarnations at claim time. Mitigation is GC's existing
  discipline: release only after death confirmation, plus routing/candidate
  filters. This is accepted and monitored, not solved.

**Enforcement-mode governance:** `claims.enforcement` is a per-store beads
config knob, but the **only sanctioned setter is a gc-owned command**;
`gc doctor` cross-checks every store's mode against the city's rollout-spec
state (mismatch = ERROR), and a city flip iterates all N+1 stores (city +
rigs) in order with a mixed-state doctor check. The beads-side Require
precondition is expressed in beads-native data — refuse `require` unless
recent claims all recorded non-empty holder tokens — never in GC fleet
concepts (no upward dependency).

### 2.4 GC integration: rollout-gated swap

Rides `internal/rollout` (PR-1b @ `62e4c6828`; PR-1c doctor wiring is an
explicit predecessor for the doctor deliverable). **Per-gate checklist** (all
test-guarded; from the actual API, not the one-liner v1 implied):
`config.Beads` field + compose-layer merge + `city-schema.json/.txt` +
`docs/reference/config.md` regen; `Flags` field + typed accessor +
`OriginOf`/`ValueOf` arms; per-gate resolver in `Resolve`;
`flag_<gate>.go` constants; `ForTest` option; registry↔resolver binding-test
rows; CODEOWNERS-reviewed registry entry.

- Gate `beads.guarded_release` (Off/Auto/Require, env
  `GC_BEADS_GUARDED_RELEASE`).
- **Capability probe keys on column/verb presence (typed probe), never on
  schema_version or bd-version integers** (both become line-relative after
  the deploy-line renumber). Probe cache has an invalidation story: re-probe
  on unknown-column/unknown-flag error class and on migration events;
  Require+RefuseClosed has a defined operator recovery (doctor remediation).
- **Fleet-level bd gate:** Auto/Require additionally require every
  claim-capable bd binary in the city ≥ the fence contract — a fence is
  vacuous if any claim path doesn't bump it (P1 resurrects via old-PATH-bd
  re-claim with an unchanged fence). Stage-A exit samples fence monotonicity
  across real claims from worker PATH bd.
- Swap inventory (each behind the gate, with legacy-decision vs verb-outcome
  divergence events): `releaseOrphanedPoolAssignment` (keep the
  detached-probe gate) → `ReleaseWorkBead` (both callers) →
  `releaseWorkFromClosedSessionBead` → **`ReassignWorkBead`** (becomes a
  transfer consumer) → **`gc workflow reopen-source`**
  (`cmd_convoy_dispatch.go:1340`) → **API-layer assignee writes**
  (`internal/api/session_resolution.go:216`,
  `internal/api/huma_handlers_beads.go:588`) → `gc bd release-if-current`
  (fence arg; then reimplemented on the verb, §2.2 layer 5) → generated
  `on_death`/`on_boot` shell.
- **Generated shell hardening:** the emitted script carries a runtime
  capability branch (guarded verb if the PATH bd supports it, legacy
  otherwise), stops discarding stderr (logs to the session recovery log), and
  keeps the **routed/unrouted branch split** — the unrouted branch's atomic
  `--set-metadata gc.run_target=<route>` backfill is not expressible via
  `bd unclaim`, so that branch keeps its current form until the verb grows a
  metadata-preserving release. Gate flips require hook regeneration; doctor
  flags live sessions carrying stale-generation hooks.
- Env plumbing: **`session.RuntimeEnv` is the single authoritative wiring
  point** for `BEADS_HOLDER_TOKEN` (derived from the `instanceToken`
  parameter — `template_resolve.go` agentEnv is built before any token
  exists). The tmux `ensureInstanceToken` backstop either also sets
  `BEADS_HOLDER_TOKEN` and writes the minted token back to the session bead,
  or is removed for managed starts — a backstop-minted divergent token is a
  silent actor-only downgrade the template-inspecting gate can't see.
- **Pre-Require inventory gate:** before any city flips to `require`, an
  orchestrator sweep re-stamps in-flight claims (fenced transfer for live
  owners) until zero `in_progress` rows carry empty/mismatched tokens for
  live sessions; the mixed-fleet gate covers worker templates *and* runtime
  env outcomes *and* orchestrator/order paths converted to guarded verbs.
- GC in-memory stores (MemStore/FileStore) implement fence
  bump-on-transition semantics so GC unit tests of guarded paths are
  non-vacuous; a GC-side conformance test mirrors the beads bump tests.
- Observability is a **named deliverable, not a vibe**: `ownership_advisory`
  / `guard_degraded` / divergence events are projected into GC's event bus
  and the monitor sweep, with a defined operator query per rollout gate and a
  written evidence artifact per gate decision.

### 2.5 Stage B: tier-complete leases via auto-lease config + requested leases

Honest framing (red-team): making leases opt-in **changes the shipped
issues-table default** of #4537 (claim stamps a 5m lease; `bd reclaim` from a
supervisor recovers it — the commit and CLI help document that contract).
"Requested leases preserve Steve's default" was wrong. The design therefore
splits the knob from the semantics:

1. **`lease.auto = on|off` store config, upstream default `on`** — upstream
   users keep #4537 behavior verbatim. GC stores set `off`: claims stamp
   nothing unless requested. The RFC to Steve proposes the *config*, not a
   default flip; if upstream declines even that, the fork carries the config
   default off on its line (small, isolated divergence). Either way **no bd
   pin GC ships may combine unconditional lease stamping with a live
   `bd reclaim` and no renewal driver** — this is a hard invariant of the
   cross-repo version discipline, and it must hold at *every* pin bump, not
   just at end-state (v1's sequencing violated it; the plan now lands
   lease-disarm before the first schema-bearing GC pin bump).
2. **Upgrade backfill:** the migration landing `lease.auto` NULLs
   auto-stamped `lease_expires_at`/`heartbeat_at` on existing `in_progress`
   rows, so nothing pre-existing is reclaimable without opt-in.
3. **Requested leases are tier-complete** (the owner ruling): heartbeat,
   reclaim, transfer route via `WispTableRouting` on both tables;
   reclaim writes `wisp_events` for wisp rows; `ReclaimedLease` gains tier +
   fence; **reclaim bumps the fence** (a reclaimed zombie is fenced out).
   Heartbeat on an **unleased** claim is a typed rejection (`ErrUnleased`) —
   it must not arm a lease as a side effect.
4. **Renewal:** `RenewLeases(refs []LeaseRef{ID, Fence}, ttl)` — **chunked
   transactions with bounded chunk size and per-chunk retry** (a single
   batch tx rewrites `row_lock` on every renewed row and would replay the
   whole batch on any concurrent worker write — the livelock v1 reintroduced
   while citing it); per-row outcomes (renewed / lost-fence-superseded /
   not-found / unleased / error); renewal horizon (only rows within the
   renewal window are touched per tick). Scale test: N workers mutating
   claimed rows during renewal, bounded renewal latency.
5. **Server-time authority** for stamps/cutoffs; documented max-skew fallback
   where the backend can't provide it.
6. Claim without a lease request **NULLs stale lease columns** it finds
   (post-legacy-release hygiene).

GC adds the ordered renew-before-reclaim phase (complete snapshot →
batch-renew confirmed-live → detached renew-after-probe → partial snapshot ⇒
skip reclaim → `would_reclaim` shadow → typed events), gate
`beads.lease_renewal`. GC session-death reclaim stays authoritative
throughout Stage B.

### 2.6 Stage C: authority decision (deferred, gated)

Decided after Stage-B shadow data, on gates: (G-parity) shadow candidates
match detectors with explained divergences; (G-latency) tuned TTL/grace
reaches the 2-minute stranded window under documented skew; (G-value) the
deletion ledger nets positive. Julian decides.

**Deletion list is scoped to what lease reclaim provably covers** (red-team:
v1's list contradicted its own stays-list): `on_boot`'s no-assignee repair
targets rows that by construction carry no claim and no lease — it **stays**
under every outcome, as do sweeps covering non-leased assignment shapes,
`repairStrandedPoolWorkerBead`'s session-close/slot-recovery half (with its
`Failed==0` precondition re-derived from typed reclaim outcomes — specified
before any deletion), affinity/`run_target` semantics, degraded-read posture,
detached probes, and health patrol. The go-path deletions: the shell orphan
sweep + order, generated `on_death` release bodies (routed branch), the
failure-release branch of `releaseOrphanedPoolAssignments` for leased shapes,
the legacy `ReleaseIfCurrent` family, and the dead-assignee projection — each
only after its replacement is proven in soak.

---

## 3. Decisions

| # | Decision | Rationale |
| --- | --- | --- |
| D1 | Fence is a beads **column**, not `gc.*` metadata | must bump on beads-side transitions; metadata is the owner-blind class |
| D2 | Fence is monotonic, bumps **only on ownership transitions**, and **every bump rewrites `row_lock` in the same statement** | ABA under Dolt cell-merge otherwise |
| D3 | Primary enforcement = server-side **ambient holder token**; caller-quoted `--fence` is defense-in-depth | LLM workers omit/re-derive quoted tokens (R3) |
| D4 | `BEADS_HOLDER_TOKEN` = `GC_INSTANCE_TOKEN`, wired **only** via `session.RuntimeEnv` | crypto-random, per-incarnation, on both sides; agentEnv predates the token |
| D5 | Require covers the **full in-place mutation surface** of claimed rows (class M) | metadata writes are part of the corruption class |
| D6 | **Two-class authorization**: satisfied explicit guards authorize transition verbs cross-actor; `--force` never skips supplied guards | the orchestrator is a legitimate non-holder principal; v1 blocked it |
| D7 | Batch ops: **chunked, per-row outcomes**, never atomic-or-error, never one giant tx | availability + `row_lock` replay livelock |
| D8 | GC swaps ride `internal/rollout` with loud degrade + fleet-level bd gate + typed capability probe | silent-unguarded and vacuous-fence windows otherwise |
| D9 | Adoption/wake/transfer are **fenced transitions gated GC-side** by the session bead's current `instance_token`; recipient token always stamped at transfer (sentinel for not-yet-existing recipients) | beads can't distinguish incarnations; empty-token windows reopen P3 |
| D10 | Detection authority stays in GC; Stage C flip is evidence-gated | leases add consolidation+fencing, ~zero detection value |
| D11 | Leases: `lease.auto` config (upstream default on, GC off) + tier-complete requested leases; hard invariant: no armed-lease + live-reclaim + no-renewal pin ever ships | tier-complete ruling; honest upstream framing; interlock holds at every intermediate state |
| D12 | `holder_token` never appears in any read/scan/JSON surface | otherwise it collapses into the quoted-fence defeated class |
| D13 | Advisory telemetry ships **before** Require/transfer semantics are frozen (A-B3 split) | the hardest open questions are empirical; advisory data answers them |
| D14 | `claims.enforcement` flips only via a gc-owned command; doctor cross-checks store mode vs rollout state; beads-side preconditions stay beads-native | no upward dependency; no bypassable side-channel flip |

## 4. Alternatives rejected

Unchanged from v1 (composite assignee; `gc.*` metadata fence; verbatim v54
port; bulk verb / ReclaimPatch / reaper-as-authority / parallel lease table;
worker-quoted fence as primary), plus:
- **Empty-token transfer handoff** (v1 OQ1) — reopens P3 with an unbounded
  window; replaced by stamp-recipient-token / sentinel (D9).
- **"Requested leases preserve upstream defaults"** — false for the issues
  table; replaced by the `lease.auto` config split (D11).
- **Single-tx batch renewal** — replaced by chunked renewal (D7).

## 5. Risks

1. **Upstream coordination** (Steve): `lease.auto` + tier-complete requested
   leases + guarded verbs + fence. Honest framing: the config leaves his
   default intact; the fork sets it off on its stores. Fallback: fork-side
   config default with isolated divergence. Engage now — he's active on this
   surface; we have co-maintainer standing.
2. **Migration numbering / schema skew**: fence+holder take 0055/0056 on
   main; CAS PR #4682 rebases to the next free; the deploy line diverges at
   0054 **and version numbers are a pure integer cursor** — same-number-
   different-content silently skips DDL on a DB touched by both lines.
   Mitigation: an **open-time content-hash divergence check** lands in bd
   before any deploy-line port; the port ships with a fleet migration runbook
   (enumerate per-rig DBs, pre-clean dirty working sets, pre-migrate before
   binary swap, dispatcher skip-rig behavior for behind-schema rigs).
   Rollback posture for every schema slice: **roll-forward only** — down.sql
   is documentation; old binaries tolerate the additive columns
   (cross-version test in the first schema slice).
3. **PR #4675 dependency**: mergeable, review-active; if it stalls we carry
   its commits and rebase.
4. **Enforcement blast radius**: three-mode rollout, advisory-first with
   class taxonomy, pre-Require inventory gate, journey-lab e2e under
   advisory and require before any fleet flip; wisp-compact/reaper/dispatcher
   caller inventory with explicit dispositions.
5. **Proxied/domain-db drift**: explicit slices + proxied integration tests;
   `--if-*` on unthreaded proxied paths refuses (exit 13), never drops.
6. **Unpushed pre-reqs**: rollout PR-1b/1c unpushed; PR-1c is an explicit
   predecessor of the doctor deliverable. **The live fleet runs deploy
   lineages of both repos** — Stage A adds explicit deploy-line ports for
   both (beads A-B4, gc A-G4) or the soak gates are unreachable/vacuous.

## 6. Open questions

1. ~~Transfer token handoff~~ — resolved (D9).
2. Human-claimed rows under Require: actor-match-with-empty-token is the
   floor; is a per-bead opt-out needed, or does the advisory soak show human
   flows never hit class M on claimed rows? (Answer from advisory data;
   A-B3b gate.)
3. ~~`bd show` redaction of holder_token~~ — resolved (D12: never surfaced).
4. Stage B renewal cadence source: patrol tick vs dedicated loop (decide in
   B-S1 with shadow data).
5. Dep/label mutations under Require: excluded until evidence demands
   (advisory taxonomy will show whether zombies touch them).
6. Formula-side claim-before-close for convoy target beads (P2 scope note):
   land as a core-pack change in Stage A or defer to Stage B? (Owner call;
   default: Stage A follow-up after advisory data confirms the exposure
   rate.)
