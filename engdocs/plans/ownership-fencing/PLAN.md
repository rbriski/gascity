# Work-Ownership Integrity — Implementation & Phasing Plan

> Companion to `DESIGN.md` (v2 — red-team findings folded). Epic `ga-furrj5`.
> One bd bead per slice. Every slice: TDD, red-team review before commit,
> quality gates green before "done".

## Repos, bases, coordination

| Workstream | Repo | Base | Notes |
| --- | --- | --- | --- |
| Beads fence/guards (A-B*) | `gastownhall/beads` | PR **#4675** head `7d7b064f1` (`pr-4675-base`; worktree `.worktrees/claim-fence`, branch `feat/claim-fence`) | #4675 = owner-checked wisp-routed unclaim; rebase when it merges |
| Beads leases (B-L*) | `gastownhall/beads` | A-B* tip | |
| GC provider/rollout (A-G*, B-S*) | `gastownhall/gascity` | `worktree-reconciler` @ `62e4c6828` (rollout PR-1b, **unpushed**) | push/land PR-1b, then PR-1c (explicit predecessor of the A-G2 doctor deliverable) |
| Deploy-line ports (A-B4, A-G4) | both deploy lineages | see slices | the live fleet runs deploy lineages; Stage A soak is unreachable without these |

**Cross-repo version discipline** (hard invariants, checked at every pin
bump, not just end-state):
- `go.mod` + `deps.env` move in lockstep (`TestBDVersionPins`).
- **No bd pin GC ships may combine unconditional lease stamping with a live
  `bd reclaim` and no renewal driver.** Consequence: the lease-disarm
  (`lease.auto`, A-B2.5) lands **before** the first GC pin bump that crosses
  the lease stack (A-G1).
- Capability probes key on column/verb presence, never on schema_version/bd
  version integers (line-relative after renumbering).
- Typed JSON error bodies are additive; existing conflict message phrasings
  frozen until fleet-min gc parses the typed body.
- Rollback posture for every schema slice: roll-forward only (down.sql is
  documentation); first schema slice adds an old-binary-vs-migrated-DB
  cross-version test.

**Migration numbers**: A-B1 = `0055_add_claim_fence`; A-B3a = `0056_add_holder_token`;
CAS PR #4682 rebases `0055_add_revision` → next free. Canonical trailing
column order: `..., lease_expires_at, heartbeat_at, claim_fence,
holder_token, revision` (+ value-level sentinel scan test). Deploy-line port
(A-B4) renumbers as one block **after** the bd open-time content-hash
divergence check lands (A-B4 pre-item).

---

## Stage A — Fencing & guarded execution

### A-B1: `claim_fence` column + semantic bump discipline (beads) — `ga-n6kekp`
- Migration `0055_add_claim_fence` (guarded ALTER, issues + wisps-if-present)
  + `cliMigrations` case.
- Thread: `IssueSelectColumns` (canonical trailing order), `ScanIssueFrom`,
  `types.Issue.ClaimFence`, JSON output; value-level sentinel scan test
  (guards against fence↔revision swap on the CAS rebase).
- Bump (semantic, per DESIGN §2.1): claim; unclaim; reclaim; assignee-change
  in `updateIssueInTx`; **closed→open status transition in `updateIssueInTx`**
  (the real reopen path on dolt/embeddeddolt; `ReopenIssueInTx` kept in
  sync); import/upsert assignee-change (`insertIssueIntoTable`
  ON-DUPLICATE-KEY + domain/db upsert) with holder_token clear;
  promote/demote table moves **carry** fence+token; domain/db hand-rolled
  `Claim`/assignee-`Update`.
- Package-guard test: enumerates transitions by **row delta** (status/assignee
  change), asserts bump AND same-statement `row_lock` rewrite; asserts
  same-owner idempotent re-claim does NOT bump.
- Conformance: `testClaimBumpsFence`, `testUnclaimBumpsFence`,
  `testReclaimBumpsFence`, `testReopenViaUpdateBumpsFence`,
  `testFenceStableOnPlainUpdate`, `testFenceSurvivesImportAndTableMove`.
- Cross-version test: pre-fence bd binary against migrated DB (roll-forward
  tolerance).

### A-B2: guarded verbs + typed conflict contract (beads)
- `IfAssignee`/`IfFence` on unclaim/close/update per DESIGN §2.2; typed
  `PreconditionFailedError`; exit 9 + `{code:"ownership_conflict",...}`;
  exit 13 unsupported; `{code:"already_claimed", id, holder}` on claim loss
  (message text frozen/additive).
- **Class-T authorization** (DESIGN §2.3): satisfied guard authorizes
  cross-actor transition verbs; unguarded = owner-only (#4675 semantics);
  `--force` bypasses owner check only, never supplied guards; audit event
  enumerates bypassed checks.
- Dispatch: issueops (dolt/embedded/sqlkit free); **domain/db explicit +
  use-case threading** (`UpdateSpec`→`ApplyUpdate`→repo) or exit-13 refusal
  until threaded; HookFiringStore passthrough; proxied integration tests.
- Test rows: orchestrator guarded release of a dead worker's row (cross-actor,
  class T); guarded release loses to fresh re-claim; zombie close rejected
  by guard; `--force` + failing `--if-fence` still refuses.

### A-B2.5: lease disarm — `lease.auto` config + backfill (beads)
- `lease.auto = on|off` store config (upstream default **on**); GC stores set
  off. Claim stamps lease only when `lease.auto=on` or explicitly requested
  (`--lease-ttl`, public `WithLeaseTTL`).
- Upgrade backfill: NULL auto-stamped `lease_expires_at`/`heartbeat_at` on
  existing `in_progress` rows when a store flips auto→off.
- Heartbeat on unleased claim ⇒ typed `ErrUnleased` (never arms).
- Claim without lease request NULLs stale lease columns it finds.
- Rewrites the #4537/#4675 tests that assert unconditional stamping
  (documented in the RFC to Steve — the config leaves upstream default
  intact; fork fallback: config default off on the fork line only).
- **Blocks A-G1** (the pin-bump invariant).

### A-B3a: holder_token column + recording + advisory telemetry (beads)
- Migration `0056_add_holder_token`; **never surfaced** in
  reads/scan/JSON (package-guard test asserts absence from
  `IssueSelectColumns`); returned only in `ClaimResult` to the claimant.
- Claim records ambient `BEADS_HOLDER_TOKEN`; import assignee-change clears
  it; reserved sentinel `'!'` fails all matches.
- `claims.enforcement = off|advisory` (require comes in A-B3b): advisory
  emits typed `ownership_advisory` events **labeled by class** — (a)
  cross-actor infra, (b) empty-token legacy, (c) actor==assignee+token
  mismatch — on the full class-M mutation surface.
- Same-actor re-claim with differing non-empty ambient token ⇒ typed
  `incarnation_conflict` (no silent success/overwrite).

### A-B3b: `require` mode + `bd claim transfer` (beads) — **gated**
- Gates: ≥1 week fleet advisory data with the class taxonomy; class-(a)
  caller inventory dispositions decided (wisp-compact `--persistent`,
  reaper bd + raw-SQL closes, dispatcher `gc.outcome` stamps, orphan-sweep,
  human flows/OQ2); DESIGN D6/D9 semantics confirmed against the data.
- `require` per DESIGN §2.3 class M; beads-native precondition (refuse
  require unless recent claims all recorded non-empty tokens).
- `bd claim transfer <id> --to-actor --to-token [--if-assignee] [--if-fence]`
  (class T): re-stamp assignee+token, bump fence, rewrite row_lock, typed
  event. Zombie matrix tests: legitimate wake row; detached-executor row;
  transfer race convergence (both quote IfFence); sentinel-token behavior.
- Enforcement setter: gc-owned command only; doctor cross-check (A-G2/PR-1c).

### A-B4: beads deploy-line port (Stage A block)
- Pre-item: **open-time content-hash divergence check** in bd (same version
  number, different content ⇒ loud refusal, not silent skip) — lands
  upstream first.
- Port lease columns (disarmed via `lease.auto=off`) + fence + holder +
  guarded verbs onto `local/deploy-current-integrated` as one renumbered
  block; fleet migration runbook: enumerate per-rig DBs, pre-clean dirty
  working sets (DirtyTablesError wedge), pre-migrate via write-open before
  binary swap, dispatcher skip-rig (not hard-fail) for behind-schema rigs.
- Validate against every live per-rig DB schema state; `test-migration` /
  `test-upgrade` / cross-version green.

### A-G1: GC provider surface (gascity, base `worktree-reconciler`)
- `beads.Bead.ClaimFence`; BdStore parse; typed error mapping (exit 9 ⇒
  `ErrOwnershipConflict`, 13 ⇒ `ErrConditionalReleaseUnsupported`); delete
  `isBdClaimConflictMessage` once pin carries typed bodies.
- `ReleaseIfHeld(id, expectedAssignee, expectedFence)` across stores;
  MemStore/FileStore implement **fence bump-on-transition semantics**
  (claim/release/reassign) + GC-side conformance test so unit tests are
  non-vacuous.
- **Legacy `ReleaseIfCurrent` hygiene (pre-pin-bump, gate-independent):** the
  raw SQL adds fence bump + `row_lock` rewrite + lease-column clear;
  reimplemented on `bd unclaim --if-assignee [--if-fence]` once the pin
  carries it; doctor check for raw-SQL writes on claimed rows.
- `gc hook --claim` JSON `claim_fence`; session-bead pointer stamp;
  continuation preassign becomes guarded (`IfAssignee("")`).
- Env: `BEADS_HOLDER_TOKEN` wired in **`session.RuntimeEnv` only**; tmux
  `ensureInstanceToken` backstop sets it too and writes the minted token back
  to the session bead (or is removed for managed starts); testenv leak-vector
  entry.
- **Pin bump** (two knobs, lockstep) — requires A-B1+A-B2+**A-B2.5** in the
  pinned bd (the invariant); fleet migration runbook step from A-B4 applies
  to any schema-bearing bump.

### A-G2: rollout-gated release swap (gascity)
- Gate `beads.guarded_release` — full per-gate checklist (DESIGN §2.4):
  config field + compose merge + schema/docs regen + Flags field/accessor +
  OriginOf/ValueOf + resolver + fortest + flag constants + binding tests +
  CODEOWNERS registry entry. Doctor deliverable **depends on PR-1c**.
- Typed capability probe (column/verb presence; cache with re-probe on
  unknown-column/flag + migration events; Require+RefuseClosed operator
  recovery documented). **Fleet-level bd gate** for Auto/Require (all
  claim-capable binaries ≥ fence contract).
- Swap inventory (DESIGN §2.4 order): `releaseOrphanedPoolAssignment`,
  `ReleaseWorkBead`, `releaseWorkFromClosedSessionBead`, `ReassignWorkBead`
  (transfer consumer), `gc workflow reopen-source`, API assignee writes
  (`session_resolution.go:216`, `huma_handlers_beads.go:588`),
  `gc bd release-if-current`, generated `on_death`/`on_boot` (runtime
  capability branch in the emitted script; stderr logged; routed/unrouted
  branch split kept). Each swap: divergence event (legacy decision vs verb
  outcome) + conflict-rate metrics.
- Keep: detached-probe gate, degraded-read gating, 2m stranded grace,
  session close/slot recovery.

### A-G2.5: observability slice (gascity) — **precedes any soak gate**
- Project `ownership_advisory` (with class labels), `guard_degraded`,
  divergence events from per-rig stores into GC's event bus + monitor sweep
  (apply the sudo-audit grep lesson); beadmeta/events registrations.
- Named operator query per rollout gate; each gate decision produces a
  written evidence artifact. Doctor: enforcement-mode cross-check, mixed
  per-store state, stale-generation hooks, raw-SQL-on-claimed-rows.

### A-G3: fenced adoption + enforcement rollout (gascity)
- Adoption (`existing_assignment` **and** `ready_assignment` branches):
  verify ambient `GC_INSTANCE_TOKEN` == session bead `instance_token`, then
  fenced transfer; skip while `gc.detached` probe is live.
- Wake: reconciler re-stamps holder_token via fenced transfer at
  `preWakeCommit` (before process start) — "legitimate wake" test row.
- Advisory soak (fleet, via A-G2.5 pipeline): Require flip gated on class-(c)
  explained-to-zero AND class-(a) fully converted to guarded verbs — not raw
  event-watching. **Pre-Require inventory gate:** re-stamp sweep until zero
  in_progress rows with empty/mismatched tokens for live sessions; mixed
  fleet gate covers templates + runtime env outcomes + orchestrator paths.
- Enforcement flip: gc-owned command iterates all city stores in order;
  doctor mixed-state check. journey-lab adopt-pr e2e under advisory, then
  require, before any fleet flip.
- P2 scope follow-ups (DESIGN §1): formula claim-before-close for convoy
  target beads (owner call, OQ6); pack caller dispositions from A-B3b
  inventory (wisp-compact, reaper raw-SQL → routed through bd or documented
  bypass).
- Delete after soak: `isBdClaimConflictMessage`, subsumed re-read guards.

### A-G4: GC deploy-line port
- Land/port rollout PR-1b/1c + A-G1..A-G3 onto the gc deploy lineage
  (maintainer city runs `deploy/*`, not origin/main) with old-gc/new-bd
  compat gate — **or** a stated precondition with owner/date that the fleet
  moves to an origin/main-derived gc first. Without this slice the Stage A
  soak criteria are unreachable (Auto ⇒ perpetual loud-degrade) or vacuous
  (Off ⇒ zero fencing).

**Stage A exit criteria** (revised): P1 closed on all swapped paths with the
fleet-level bd gate green and fence monotonicity sampled across worker-PATH
claims; P2/P3 closed **for claimed rows** under `require` in ≥1 soaked city
(named residual exposures tracked); zero unexplained `guard_degraded`; class-(c)
advisory zero; all quality gates green; A-B* upstream-merged or carried with
a rebase plan.

---

## Stage B — Tier-complete leases

### B-L1: requested-lease surface (beads)
- Public `--lease-ttl` / exported lease types; RFC framing per DESIGN §2.5
  (config already landed in A-B2.5; this slice completes the opt-in surface
  and docs).
### B-L2: tier-complete lease ops (beads)
- Heartbeat/reclaim/transfer via `WispTableRouting` on both tables for
  requested leases; `wisp_events` routing; `ReclaimedLease` + tier + fence;
  reclaim bumps fence; conformance per tier per backend (R2 gates 1–3,
  13–14).
### B-L3: chunked renewal + owner queries (beads)
- `RenewLeases(refs []LeaseRef{ID,Fence}, ttl)`: bounded chunks, per-chunk
  retry, per-row outcomes, renewal horizon; scale test (N workers mutating
  during renewal, bounded latency). Owner-query index for "does this owner
  still hold claims".
### B-L4: deploy-line lease block (beads fork line)
- Port B-L1..B-L3 onto the deploy line (A-B4 runbook + content-hash check
  already in place).
### B-S1: orchestrator renew-before-reclaim, shadow (gascity)
- Reconciler phase per DESIGN §2.5; gate `beads.lease_renewal` (full per-gate
  checklist); shadow metrics via A-G2.5 pipeline (candidate-vs-detector
  divergence, renewal latency, partial-snapshot skips, unleased managed
  claims); renewal cadence decision (OQ4) from shadow data.

**Stage B exit**: requested leases live for GC-managed claims on both tiers
in ≥1 city; ≥2 weeks renewal soak with zero live-work `would_reclaim` from
renewal gaps; upstream state resolved.

---

## Stage C — Authority decision (gated, owner call)

Gates G-parity / G-latency / G-value on Stage-B evidence artifacts; Julian
decides. Go ⇒ enable reclaim mutation, detectors to audit-only soak, then the
**scoped** deletion list (DESIGN §2.6 — on_boot no-assignee repair and
non-leased-shape sweeps stay under every outcome; `repairStrandedPoolWorkerBead`
`Failed==0` precondition re-derivation specified before deletion). No-go ⇒
Stage A+B is the permanent posture.

---

## Testing & verification (cross-cutting)

- TDD; beads conformance suite grows claim/fence/guard/lease cases per slice
  (zero claim coverage on main today); proxied path in every verb slice.
- Race tests on real Dolt + embedded: guarded release vs re-claim; transfer
  vs zombie close; heartbeat vs reclaim vs close with fence; concurrent
  fence bumps (row_lock pairing).
- Zombie matrix (A-B3/A-G3): {stale token, new token, no token} ×
  {off, advisory, require} × {update, close, metadata} + legitimate-wake +
  detached-executor + same-actor-differing-token rows.
- GC: package guards (beadmeta, events, rollout binding), fence semantics in
  MemStore/FileStore, divergence assertions, `make test` + sharded targets,
  `go vet`, dashboard-check for API-type changes, worker-boundary test.
- Red-team workflow before every commit; findings folded or explicitly waived
  in the PR body.
- e2e: journey-lab adopt-pr under advisory → require.

## Sequencing (dependency graph)

```
#4675 ─► A-B1 ─► A-B2 ─► A-B2.5 ─► A-B3a ─►(advisory data)─► A-B3b ─► B-L1 ─► B-L2 ─► B-L3 ─► B-L4
                                    │
content-hash check ─► A-B4 (deploy-line Stage A block)
rollout PR-1b (push/land) ─► A-G1 ─► A-G2 ─► A-G2.5 ─► A-G3 ─► B-S1 ─► Stage C
rollout PR-1c ─► A-G2 doctor deliverable
A-G1 pin bump REQUIRES A-B1+A-B2+A-B2.5 in the pinned bd
A-G3 fleet soak REQUIRES A-B4 + A-G4 + A-G2.5
CAS #4682: rebase revision migration + column order after A-B1
```

Parallelizable: A-B1/A-B2 alongside rollout PR-1b/1c push; A-B3a alongside
A-G1; A-B4 alongside A-G2. Stage B after Stage A beads slices merge or are
explicitly carried.
