# HQ Coordination-State Store — Discovery Document

> **Status:** Round-1 synthesis (S7). Sources: S1 (entities), S2 (volume/churn),
> S3 (read profile), S4 (write profile), S5 (durability), S6 (anti-requirements).
> Observation rig: gascity, 2026-05-22. Author: gascity/architect.
>
> This document is the authoritative consolidated output of the round-1 discovery
> spikes. It drives round-2 (solution landscape) scope decisions.

---

## Goal

Right-size the HQ coordination-state store away from Dolt's full feature set. The
coordinator for the Gas City orchestration runtime (sessions, tasks, mail, molecules,
convoys, orders) needs a store that is **fast, local, and bounded** — not a versioned,
branch-capable, git-under-the-hood database whose differentiating features are 100%
unused. The goal is to characterize what the store actually needs so we can pick the
right technology and set defensible performance and retention targets.

**What this document is NOT:** a storage-technology recommendation (that is round-2).
This document is tech-agnostic requirements, data, and constraints.

---

## Entity catalog

Full detail in `findings/S1-entities.md`. Summary by section:

### Layer 0 — Work primitives (§I)
- **Task / Bug / Feature / Chore / Epic / Step** — the canonical "unit of work". All
  share the `issues` table, differ only by `issue_type`. 21,286 closed tasks (91% of
  issues table) with no retention policy today — the largest dead-weight pool.
- **Intent:** durable while active; archivable/purgeable once closed.

### Layer 1 — Runtime identity (§II)
- **Session** — the richest lifecycle in the system (13+ base states). The bead
  serializes ephemeral runtime state across controller ticks and restarts. Terminal
  sessions (drained/closed) have no operational value.
- **Agent / Role / Rig** — long-lived configuration records, very low churn.
- **Intent:** Session: ephemeral runtime with a durable projection. Agent/Role/Rig:
  durable configuration.

### Layer 2 — Workflow execution (§III)
- **Molecule (root + steps)** — a DAG of step beads instantiated from a formula. 11
  open today, cleanup logic exists. Medium-durable while active.
- **Convoy** — cross-rig batch tracker. 22 open, 6 closed.
- **Spec** — frozen spec-doc-as-bead. Durable by design.

### Layer 3 — Communication (§IV)
- **Mail message wisp** — addressed agent-to-agent comms. 2,365 OPEN messages today,
  oldest 15 days old, no auto-archive policy. **Single clearest unbounded-growth
  pattern in user-facing data.** Intent: mostly ephemeral; notification value decays
  within days.
- **Order-tracking wisp** — per-order-fire audit bead. 3,500/day created and closed
  within 24h. `wisp_events` and `wisp_labels` missing FK constraints → 45–46% orphan
  rows.
- **Field-change event (issues.events)** — append-only per-bead mutation audit.
  75,933 rows, FK cascade in place. Largest persistent growth driver (9k rows/day
  sustained, 17k/day peak).
- **System event (events.jsonl)** — separate file-based pub/sub log. Bounded by
  rotation already. Does not load the store.

### Layer 4 — Coordination state (§V)
- **Gate** — async wait condition. TOML-defined; schema columns for bead-modeled
  gates are all empty.
- **Dependency edge** — directed bead DAG. 13 rows on HQ (sparse coordination DB);
  5,520 in the rig DB. Load-bearing for ready semantics.
- **Label** — many-to-many tags. 447 issue labels (sparse); 23,106 wisp labels (dominated
  by order-tracking churn).
- **Nudge queue item** — filesystem-only (`.gc/runtime/nudges/state.json`). Not
  mirrored to Dolt.
- **Session-name lock** — kernel flock. Not a store concern.
- **Convergence** — idempotency-guard for molecule instantiation. Rare.

### Layer 5 — Routing & sync (§VI)
- **Rig route** — 5 rows in `.beads/routes.jsonl`. `routes` Dolt table is empty.
- **Federation peer** — table empty; federation not active.

### Layer 6 — Memory & config (§VII)
- **bd memory (kv.memory.*)** — 24 entries. Durable, manually managed.
- **Custom status / type** — 3 statuses + 12 types. Very low churn, durable.
- **Counters** — monotonic ID allocators. Durable, append-only.
- **Schema migrations** — ledger, append-only.

### Layer 7 — Snapshots & retention (§VIII)
- **Compaction snapshot** — 0 rows; compaction not producing tier rows on this rig.
- **Issue snapshot** — 0 rows; feature unused.
- **Comment** — 6 rows; `notes` field is preferred in practice.
- **Repo mtime** — 0 rows; feature unused.

### Layer 8 — Filesystem-only process state (§IX)
Controller lock, Dolt server lock/port/log, pack runtime state — all **ephemeral
process coordination**, not store concerns. `.beads/interactions.jsonl` (5,606 lines,
no reaper) duplicates the `events` table with session-actor attribution. `.gc/beads.json`
is a stale 21-day-old orphaned snapshot.

### Defined-but-unused schema surfaces (§X)
9 unused columns on `issues` (wisp_type, mol_type, event_kind, actor, target,
payload, await_type/id/timeout_ns/waiters, role_type, agent_state) and 6 entire
empty tables (routes, interactions, federation_peers, compaction_snapshots,
issue_snapshots, repo_mtimes). Pure schema weight.

---

## Volume / churn

Full detail in `findings/S2-volume-churn.md`.

### Point-in-time snapshot (2026-05-22)

| Table | Rows | Open | Dead-weight |
|---|---:|---:|---|
| `issues` | 23,373 | 224 | 91% closed tasks (21,286) |
| `wisps` | 6,364 | 2,407 | 2,365 open mail messages with no reaper |
| `events` (issue audit) | 75,933 | — | FK cascade in place; grows with issues |
| `wisp_events` | 47,334 | — | **45% orphans** (21,445) — no FK |
| `labels` | 447 | — | FK cascade, sparse |
| `wisp_labels` | 23,106 | — | **46% orphans** (10,706) — no FK |
| `.beads/interactions.jsonl` | 5,606 lines | — | No reaper; duplicates events table |

### Steady-state churn rates (post-burst, May 19+)

| Entity | Create/day | Close/day | Notes |
|---|---:|---:|---|
| Task bead | ~20 | ~20 | Balanced; May 14–18 burst of 21k was a one-off |
| Session bead | ~20 | ~20 | Balanced; closed sessions accumulate |
| Order-tracking wisp | **~3,500** | **~3,500** | Dominant producer; labels/events leak |
| Mail message wisp | 200–500 | 0–50 | **Broken reaper; +200/day net** |
| Issue audit events | ~9,000 | n/a | FK cascade; grows with issue writes |
| Wisp audit events | ~17,000 | n/a | **No FK; orphan-accumulating** |
| Wisp labels | ~10,500 | n/a | **No FK; orphan-accumulating** |

### 365-day projection at current rates (no intervention)

| Surface | Today | +365 days |
|---|---:|---:|
| `wisp_events` | 47k | **~6.4M** (orphan-dominated) |
| `wisp_labels` | 23k | **~3.85M** (orphan-dominated) |
| Open mail wisps | 2.4k | **~75k** (no reaper) |
| `events` (issue audit) | 76k | **~3.36M** |
| `issues` (closed tasks) | 21k | **~30k** (slow; steady-state ~20/day) |

The store is bounded today only by its youth. **Two of the five largest tables are
already on unbounded-growth trajectories within the first 24 hours of order-tracking
activity.**

---

## Read profile

Full detail in `findings/S3-read-profile.md`.

### Aggregate load (live, 2026-05-21)

- **~586 selects/sec** sustained; lifetime read:write ≈ **265:1**.
- **~143 new connections/sec** — almost entirely from bd-CLI fork-per-call overhead.
  Each fork issues ~8 handshake/setup queries before the actual read.
- **CPU at 86–90% of one core** — dominated by short fast reads, NOT slow queries.
  Zero slow queries in the observation window.

### Hot read paths (ranked by cost)

| # | Path | Frequency | Shape | Today's pain |
|---|---|---|---|---|
| R1 | **Mail/inbox poll** | ~150/s (dominant) | Full-scan of `wisps` (~6.4k rows) + SELECT DISTINCT | **The single largest read cost.** Ephemeral tier bypasses in-memory cache entirely. |
| R2 | **Work-finding (`bd ready`)** | Per-agent per reconcile tick | Filter scan over open `issues` (~200 rows) + dep join | Cheap today; scales with open-bead count. |
| R3 | **CachingStore prime/reconcile** | Startup + every 30–120s | Full scan of open beads | Critical path at startup; scales with open-bead count. |
| R4 | **Bead hydration (labels/deps/comments)** | Batched IN-clause per list result | Batch-by-id-set | Structurally fine; index-served. |
| R5 | **Point lookup (bd show / cache miss)** | High volume on cache miss | Point read by PK | Fast; cost multiplied by fork overhead. |
| R11 | **Connection handshake noise** | 1:1 with bd forks (~150/s) | Setup queries per connection | ~1,200 q/s of pure overhead. **Elimination of fork-per-call removes this entirely.** |

**The dominant problem is not query complexity — it is the per-CLI fork + connection
overhead and one full-scan predicate (R1, mail poll on ephemeral tier).**

---

## Write profile

Full detail in `findings/S4-write-profile.md`.

### Aggregate load (live, 2026-05-21)

- **~2.2 writes/sec average** (bursty; most seconds = 0 writes).
- **Lifetime read:write ≈ 265:1.** The store is read-saturated; writes are not the
  bottleneck.
- No writes were caught in 300 processlist samples — all sub-millisecond.
- Insert:update ratio ≈ 1:0.34 ⇒ each bead is updated ~3× on average before close.
- `Com_commit = 0` — all writes under auto-commit; no explicit transactions issued.

### Write paths and consistency contracts

| Path | Frequency | Consistency required |
|---|---|---|
| W1 Work-bead create | ~3/min | Per-record atomic; server-generated ID; read-after-write |
| W2 Bead update | ~19/min | Per-record atomic; LWW between concurrent writers |
| W3 **SetMetadataBatch** | High on session transitions | **Intra-record multi-key atomic** — the ONLY cross-key consistency contract in production |
| W4 Bead close/delete | ~3/min | Per-record atomic; idempotent |
| W5 Event emission | ~33/min | Per-line O_APPEND atomic; per-writer FIFO |
| W8 Order-tracking lifecycle | ~10/min | Per-record atomic; at-most-one-extra-fire on crash |
| W13 wispGC purge | Hourly sweep | Batch-non-atomic (retry-safe) |

**Cross-record transactions are NOT used anywhere in production.** `BdStore.Tx`
exists as plumbing but has zero production call sites that require multi-bead atomicity.

---

## Durability matrix

Full detail in `findings/S5-durability.md`.

| Entity class | Must survive restart? | Loss tolerance | Recovery time |
|---|---|---|---|
| Work beads (open tasks, molecules, steps, convoys) | **YES — hard requirement** | At-most-once on crash-during-write | Seconds (PrimeActive sweep) |
| Session beads (open, with state metadata) | **YES — hard requirement** | At-most-once; reconciler converges | 30–60s (first reconciler tick) |
| Mail messages / wisps (unread) | **YES — agent work assignment** | Best-effort; loss of in-transit message tolerable | Seconds (next inbox poll) |
| Order-tracking beads (within cooldown window) | **YES — cooldown correctness** | At-most-one-extra-fire on crash | Seconds |
| System event log (events.jsonl) | YES | Sub-millisecond kernel-buffer window | Immediate (tail resume) |
| Session logs (JSONL transcripts) | YES | Last partial line on SIGKILL | Immediate |
| CachingStore (in-memory) | NO — ephemeral cache | Entire cache | Seconds (PrimeActive rebuild) |
| Session-name lock files | NO — kernel flock | All locks | Zero |
| Dolt server PID/port files | NO — advisory | Entire state | Seconds (probed on startup) |
| Closed beads (historical) | Optional | Entire closed history | N/A |

### What "gc restarts but continues" minimally requires

1. All non-closed **session beads** with state metadata.
2. All non-closed **work beads** (tasks, molecules, step beads).
3. All **unread mail messages** (wisps with `status=open`).
4. **Order-tracking beads** within their cooldown window.

### Restart-recovery SLA targets

| Phase | Target |
|---|---|
| Store available after gc start | ≤ 5 s |
| Open-bead catalog readable (PrimeActive) | ≤ 5 s at 10k open records |
| First reconciler tick (session state computed) | ≤ 30 s |
| Session re-create / drain-completion in flight | ≤ 60 s |
| City fully resumed, all agents awake | ≤ 120 s |

A right-sized store (no fork-per-call) would collapse T+5s → T+0.5s and T+30s → T+5s.

---

## Anti-requirements

Full detail in `findings/S6-anti-requirements.md`.

**Every Dolt differentiating feature is unused by HQ coordination state:**

| Feature | Used in production? | Cost paid anyway |
|---|---|---|
| Commit history / time-travel | **NO** | git-object write per commit; commit-graph scan on every query; unbounded storage |
| Branch / merge | **NO** | Merge-resolution overhead; non-fast-forward error surface |
| Cross-node sync / remotes | **NO** | Local-only; `bd dolt push` errors are expected and documented |
| ACID multi-record transactions | **NO** | SQL engine overhead; `Tx()` is simulated sequential bd-update calls, no BEGIN/COMMIT |
| SQL DDL / schema migrations | **NO** | Schema versioning complexity |
| Full SQL (ad-hoc queries) | **NO** | SQL engine overhead for KV-filtered scans |

**The overkill cost is concrete:**
- ~1.6s per dolt-commit (write latency)
- ~30–80ms per bd-CLI fork (connection tax)
- ~990 bd calls/reconcile-tick × 68–120s wall-clock per tick
- Unbounded storage growth from git-object history
- CPU degradation over time as the commit graph grows

---

## EMERGING REQUIREMENTS

Consolidated, de-duplicated across S1–S6. These are the requirements a replacement
store must satisfy; all others are anti-requirements (things we deliberately do NOT
need to support).

### Functional requirements

| # | Requirement | Priority | Source |
|---|---|---|---|
| FR-1 | **CRUD by stable string ID** — create, read, update, delete individual records; server-generated UUID on create | P0 | S4 WP-1, WP-2 |
| FR-2 | **Filter scan** — by label, assignee, status, type, metadata-key=val, parent-id, created_before, limit | P0 | S3 AP-2, S6 |
| FR-3 | **Point read by PK** — p99 ≤ 1 ms at 25k records | P0 | S3 AP-1 |
| FR-4 | **Batch-by-id-set fetch** — `id IN (…16–64…)` for hydration (labels, deps, comments), index-served, p99 ≤ 5 ms | P0 | S3 AP-3 |
| FR-5 | **Intra-record multi-field atomic write** — all metadata keys in `SetMetadataBatch` commit as one observable change | P0 | S4 WP-4, W3 |
| FR-6 | **Read-after-write within same process** | P0 | S4 WP-3 |
| FR-7 | **Two-tier storage** — durable "main" tier + ephemeral "wisps" tier with configurable TTL, same read/write API | P0 | S3 AP-6, S4 WP-7, S5 |
| FR-8 | **Filter scan on ephemeral tier** — by (issue_type, status, assignee), index-served, p99 ≤ 10 ms at 10k wisps | P0 | S3 AP-4 (mail-poll hot path) |
| FR-9 | **Ready semantics** — open records with no unresolved blocking deps, filterable by assignee/label/metadata | P0 | S3 AP-5, S6 |
| FR-10 | **Dependency graph** — directed edges between record IDs; add, remove, list per-record | P1 | S1 §V.3, S6 |
| FR-11 | **Per-record metadata** — `map[string]string`; filterable on arbitrary keys | P0 | S6 |
| FR-12 | **TTL-based expiry** for ephemeral tier; bulk close/delete sweep | P1 | S4 W13, S2 §III |
| FR-13 | **Append-only event log** with per-line atomicity and per-writer FIFO; multi-writer via OS serialization | P0 | S4 WP-8 (events.jsonl — separate from store, already satisfied) |
| FR-14 | **Advisory ephemeral locks** (kernel flock); NOT a store concern | P0 | S4 WP-9 (already satisfied) |
| FR-15 | **Background sweep / prime** of all open records in ≤ 5s at 10k rows | P1 | S3 AP-9, S5 |
| FR-16 | **Zero-fork in-process access** — no CLI subprocess per operation; persistent handle or in-process library | P0 | S3 AP-8 (eliminates R11 overhead) |
| FR-17 | **Label FK integrity** — cascade delete of labels/events on parent record delete (wip: missing for wisp tier) | P1 | S2 §III.1, III.2 |
| FR-18 | **Range scan by recency** (`created_at DESC` with limit) for inbox-replay / archive views | P1 | S3 AP-11 |

### Non-requirements (anti-requirements, explicitly excluded)

| Excluded capability | Reason |
|---|---|
| Commit history / time-travel | Zero consumers. Unused. |
| Branch / merge | Zero consumers. Unused. |
| Cross-node sync / replication | Local-only; store is city-scoped. |
| ACID multi-record transactions | Not used in production; reconciler provides convergence. |
| SQL DDL / schema migrations | Fixed schema sufficient. |
| Full SQL (ad-hoc joins, window functions, subqueries) | Not used in production queries. |
| Snapshot isolation / MVCC | LWW between concurrent writers is sufficient (S4 WP-5). |
| Exactly-once delivery | At-most-one-extra-fire on crash is acceptable (S4 WP-6, W8). |
| Strict result ordering | Not required unless caller sorts client-side (S3 AP-12). |
| Cross-record reads within a transaction | `Tx()` is not used in production (S4 finding #1). |

---

## TARGETS

### Latency SLAs

| Operation | Target (p99) | Basis |
|---|---|---|
| Point read by PK | ≤ 1 ms | S3 AP-1 |
| Filter scan (open subset, ≤ 1k results) | ≤ 10 ms | S3 AP-2 |
| Filter scan on ephemeral tier (mail poll) | ≤ 10 ms | S3 AP-4 (currently much worse) |
| Batch-by-id-set hydration | ≤ 5 ms per batch | S3 AP-3 |
| Per-record create/update/delete | ≤ 5 ms | S4 WP-1 |
| SetMetadataBatch (intra-record multi-key) | ≤ 5 ms | S4 WP-4 |
| Background sweep (open-bead prime) | ≤ 5 s at 10k rows | S3 AP-9 |
| Connection / session setup overhead | ≈ 0 (in-process) | S3 AP-8 |

### Throughput targets

| Direction | Sustained | Burst |
|---|---|---|
| Reads | 150+ ops/s (today's level; headroom needed) | 500+ ops/s |
| Writes | 10 ops/s sustained | 50 ops/s |

### Memory ceiling (in-process store design)

| Scope | Estimate | Basis |
|---|---|---|
| Hot open-bead catalog (10k rows × ~2 KB) | ~20 MB | S2 steady-state open counts |
| Hot open-bead catalog (100k rows, headroom) | ~200 MB | S3 AP-9 scaling target |
| Ephemeral wisp tier (10k active) | ~5 MB | S2 wisp volumes |
| Full store including closed (25k rows at 2 KB) | ~50 MB today; grows to ~300 MB/year without retention | S2 projections |

**Target: in-process store with full hot catalog ≤ 256 MB.** Achievable by implementing
per-entity retention so closed records exit the hot tier promptly.

### Restart-recovery SLAs

| Milestone | Target |
|---|---|
| Store available | ≤ 5 s from gc start |
| All open records readable | ≤ 5 s (10k open), ≤ 30 s (100k open) |
| First reconciler tick | ≤ 30 s |
| City fully resumed | ≤ 120 s |
| Data loss on ordered shutdown | Zero |
| Data loss on SIGKILL | At-most-once per in-flight write (~5–50 ms window) |

---

## Per-entity retention model

Bounds growth by establishing a lifecycle exit for every entity class. "Archive" means
move to a colder physical tier (read-only, compacted, may be on slower storage).
"Purge" means hard delete.

| Entity | Active retention | Closed/terminal retention | Rationale |
|---|---|---|---|
| **Task / Bug / Feature / Chore** | Indefinite (open) | Archive at 30 days; purge at 90 days | Retrospective value exists up to ~30 days; beyond that, git history is the audit trail |
| **Epic** | Indefinite (open) | Archive at 90 days; purge at 180 days | Epics span longer horizons; slightly more durable than leaf tasks |
| **Step bead** | Until parent molecule closes | Purge with parent molecule | Steps have no value after molecule closes |
| **Session bead** | Indefinite (open/active) | Purge at 7 days after closed/drained | Closed sessions have zero operational value; 7-day window covers incident review |
| **Mail message wisp (unread)** | Indefinite (open) | Archive at 30 days if still unread | Prevent inbox bloat while preserving unread mail for slow agents |
| **Mail message wisp (read)** | Until archived | Purge at 7 days after read | Notification value is gone; minimal retention for thread context |
| **Order-tracking wisp** | 24h TTL (already target) | Purge at TTL expiry | Debug window only; existing wispGC handles this |
| **Molecule (root)** | Indefinite (open) | Archive at 30 days; purge at 90 days | Same as tasks |
| **Convoy** | Indefinite (open) | Archive at 14 days; purge at 30 days | Shorter-lived than molecules; convoy is delivery tracking, not history |
| **Merge-request bead** | Until PR closed | Archive at 30 days; purge at 90 days | Review artefacts needed for post-mortems but bounded |
| **Issue field-change event (events table)** | Retained with parent bead | Cascade purge when parent bead is purged | FK already in place; retention bounded by parent |
| **Wisp field-change event (wisp_events)** | Retained with parent wisp | **Add FK constraint; cascade purge on wisp delete** | Missing FK is the #1 growth bug; fix yields immediate ~45% table reduction |
| **Issue label** | With parent bead | Cascade purge (already in place) | OK |
| **Wisp label (wisp_labels)** | With parent wisp | **Add FK constraint; cascade purge on wisp delete** | Missing FK is #2 growth bug; fix yields immediate ~46% table reduction |
| **Dependency edge** | With endpoints | Cascade on endpoint delete | OK (sparse at HQ) |
| **Comment** | With parent bead | Cascade purge | Rare; OK |
| **Compaction snapshot** | Until purged | Delete when parent bead is purged | Verify compaction is actually running |
| **bd memory (kv.memory.*)** | Until `bd forget` | Never expires | Intentionally durable; 24 entries now — no growth concern |
| **Custom status / type** | Until deregistered | Never | Config data; very low churn |
| **Counters** | Forever (monotonic) | Never | Append-only, small |
| **Schema migrations** | Forever | Never | Append-only ledger |
| **Rig route (.beads/routes.jsonl)** | Until deregistered | Never | 5 rows; config |
| **.beads/interactions.jsonl** | **Migrate to events table OR cap at 100k lines / 30 days** | Apply log rotation | Duplicates events; 5.6k lines now; no reaper |
| **.gc/beads.json** | **Remove or ratchet to current Event Bus seq** | n/a | 21-day-stale orphan |
| **.beads/backup/** | Latest 3 snapshots; 7-day window | Per PR #2478 (in flight) | Known growth bug |
| **Maintainer-PR-review run artefacts** | Until PR closed + 90 days | Purge after 90 days | Per-run JSON artefacts; bounded per PR |

### Immediate fixes (before right-sizing lands)

These are independent of the storage-technology decision and fix the two worst
unbounded-growth bugs today:

1. **Add FK (or explicit cascade reaper) on `wisp_events` → `wisps`** — eliminates
   orphan accumulation; reduces table by ~45% on first apply.
2. **Add FK (or explicit cascade reaper) on `wisp_labels` → `wisps`** — same;
   reduces wisp_labels by ~46% on first apply.
3. **Add auto-archive policy for mail wisps** — archive read messages after 7 days,
   unread after 30 days. Prevents inbox accumulation.

---

## Open questions

1. **Round-2 adopt vs. author:** Should round-2 evaluate adopting an existing
   embedded store (SQLite, bbolt/BoltDB, LMDB, BadgerDB) or authoring a thin
   native layer from scratch? What is the migration path from Dolt without disrupting
   live cities?

2. **Wisp FK gap — bridge fix or skip to migration?** Fixing the FK in Dolt is a
   one-day change that fixes the #1 and #2 growth bugs. Is it worth applying as a
   bridge fix, or should we skip it if the migration timeline is short?

3. **Is Dolt compaction active?** `compaction_snapshots` has 0 rows despite
   compaction config being present. Is the feature misconfigured, not yet triggered
   by thresholds, or silently broken? If it can be activated cheaply, it may bridge
   the closed-task bloat problem during round-2.

4. **interactions.jsonl fate:** Is the session-actor attribution in
   `.beads/interactions.jsonl` intentionally separate from the `events` table (i.e.,
   is its actor indexing load-bearing for any consumer?) or redundant? Should it be
   merged into the events table to eliminate the dual-write?

5. **beads.json:** Remove or ratchet? If no consumer reads it today, remove. If it's
   a planned feature surface, define its update contract.

6. **HQ vs. rig-DB separation:** This discovery is HQ-scoped. The rig DB (`gascity`)
   has 5,520 dependency rows and a much richer DAG. Should round-2 scope include
   rig-DB right-sizing (potentially the same solution, different data profile) or
   treat them as separate problems?

7. **Order-tracking exactly-once gap:** At-most-one-extra-fire is currently accepted.
   If a replacement store makes idempotency-key semantics cheap, should we close the
   gap as a free improvement?

8. **Mail tier caching:** S4 notes that mail R:W ≈ 75,000:1 (read-dominated, vanishing
   write rate). Extending the CachingStore to cover the ephemeral/wisps tier with
   synchronous invalidation on send would fix the dominant hot path (R1) without a
   store replacement. Should this be a short-term interim fix independent of round-2?

---

## Decisions

These are the settled conclusions from the round-1 investigation. They close the
question for round-2 scoping and technology evaluation.

### D-1: Dolt's differentiating features are unused by HQ coordination state. (CLOSED)

Evidence: S6 full audit. Zero production call sites use commit history, branch/merge,
cross-node sync, ACID multi-record transactions, or full SQL. The HQ store uses Dolt
as a slow, overweight KV store with filter queries.

**Implication:** Round-2 is unconstrained on storage technology; there is no
compatibility requirement to preserve Dolt-specific capabilities.

### D-2: The dominant performance problem is the per-CLI fork, not query shape. (CLOSED)

Evidence: S3 (143 new connections/sec, ~1,200 handshake queries/sec), S6
(~30–80ms fork tax per call, ~990 calls/reconcile tick). Eliminating fork-per-call
(FR-16) is as high-value as any query optimization.

**Implication:** Any replacement that provides an in-process API eliminates the
connection-overhead problem regardless of query implementation.

### D-3: The mail/inbox poll (R1) is the single largest read cost. (CLOSED)

Evidence: S3 (R1 = ~150 invocations/s; full-scan of ~6.4k wisps; ephemeral tier
bypasses CachingStore entirely). The fix is either (a) extend CachingStore to cover
the ephemeral tier, or (b) ensure the replacement store serves the ephemeral-tier
filter predicate from an index (not a full scan).

**Implication:** FR-8 (indexed filter scan on ephemeral tier) is P0, not P1.

### D-4: Missing FK constraints on wisp_events and wisp_labels are bugs. (CLOSED)

Evidence: S2 (45% and 46% orphan rates; projection to 6.4M and 3.85M rows in one year).
These are not feature-level decisions — the `events` and `labels` tables on the issue
side have working FK constraints. The wisp-side omission is an oversight.

**Implication:** This fix is independent of the store-technology decision. It should
be filed as a high-priority bug fix regardless of round-2 outcome.

### D-5: The HQ store is local-only. Cross-node sync is not a requirement. (CLOSED)

Evidence: S6 (`bd dolt push` errors are expected and documented; no remote configured;
CLAUDE.md confirms "skip bd dolt push"). The store is city-scoped and runs on a single
machine.

**Implication:** A replacement does not need replication, multi-writer conflict
resolution, or distributed consensus.

### D-6: Cross-record atomicity is not required. (CLOSED)

Evidence: S4 (`Com_commit = 0` lifetime, `Tx()` has zero production multi-bead call
sites). The system provides convergence via the reconciler (NDI), not via database
transactions.

**Implication:** A replacement does not need ACID across records. Per-record atomic
writes (WP-1) + intra-record multi-field atomic write (WP-4) are the only consistency
requirements.

### D-7: Mail-tier caching is a viable short-term fix independent of round-2. (OPEN)

The CachingStore does not cover the ephemeral/wisps tier today. Extending it would
fix R1 (the dominant CPU hot path) without a store replacement. This is an open
decision for round-2 scoping: extend the cache now as a bridge, or treat it as part
of the replacement.

---

## Round-2 Recommendation

> **Date:** 2026-05-23. Author: gascity/architect (R2.4 synthesis).
> Sources: R2.1 (harness + SQLite reference adapter), R2.1b (adapter sweep — 6 backends),
> R2.2 (author-own design), R2.3 (migration path).

### Decision: Author-own (HQStore)

**Build HQStore — the thin in-process custom store designed in R2.2.**

Do not adopt SQLite, PostgreSQL, CouchDB, or any other external database. Do not extend the Dolt/BdStore path.

### Evidence summary

#### Benchmarked performance vs targets (R2.1b RealWorldWorkload, 30 s, 20 goroutines)

| Backend | 9/9 targets? | Point-read p99 | Notes |
|---|---|---|---|
| **authorcore PoC** | **9/9** | **282 µs** | The R2.2 HQStore hot path. 3.5× headroom on the critical target. |
| bbolt | 9/9 | 537 µs | Application index/query layer required — not a general query engine. |
| SQLite (mattn CGo) | 8/9 | 1.22–1.63 ms | Fails point-read p99. Structural WAL shared-lock floor under concurrency. |
| PostgreSQL | 4/9 | 3.15 ms | Networked hop disqualifies it for local HQ hot path. |
| CouchDB | 1/9 | 17.6 ms | Document model mismatch; write-through bottleneck. |
| Dolt SQL baseline | 1/9 | 16.2 ms | Even with fork tax removed, fails every latency target. |

All six candidates passed correctness on all 18 FRs. Performance, not correctness, is the differentiator.

The **authorcore PoC satisfies all nine measured performance targets** including the critical point-read p99 at 282 µs (3.5× headroom vs the 1 ms target). SQLite's 1 ms miss is structural: under 20 goroutines, WAL shared-lock acquisition adds 0.6–1.0 ms to every point read (confirmed by R2.1). This floor does not improve with tuning and worsens as city concurrency grows.

bbolt passes 9/9 but is not meaningfully distinct from author-own: it provides persistence but requires building and owning the entire index/query layer atop its KV API. The Bead struct must still be serialized for bbolt storage. The ownership surface is the same as HQStore with less architectural upside.

Pure-Go SQLite (modernc.org) is not a viable path — R2.2 documents it as ~3× slower than the C version, which at SQLite's existing 1.22–1.63 ms point-read would produce ~3.6–4.9 ms point reads, failing by a large margin.

#### Build and maintenance cost

| Path | Build cost | Ongoing maintenance | Principal risk |
|---|---|---|---|
| HQStore (author-own) | ~9 days, ~1,580 LOC | ~15–20 days/year | Own WAL crash recovery |
| SQLite (CGo) | ~3 days, ~200 LOC | ~1–2 days/year | CGo dep; SQL↔Bead marshal layer; WAL concurrency floor |

HQStore's ~9 day build cost is the honest price of the performance advantage. The principal ongoing risk is owning WAL crash recovery. This risk is bounded: the WAL design in R2.2 is conservative (partial-line skip on SIGKILL; checkpoint + replay on recovery; atomic checkpoint writes), the existing MemStore/FileStore cover ~60–70% of the surface, and the R2.1 harness gates crash-recovery correctness before any production rollout.

SQLite's maintenance advantage (near-zero storage layer cost) is real but comes with two compounding architectural costs: the CGo dependency (the `gc` binary currently has zero C dependencies; adding CGo affects cross-compilation, sandboxing, and build reproducibility) and a SQL↔Bead marshaling layer on every read and write path.

#### Risk comparison

| Risk | HQStore | SQLite (CGo) |
|---|---|---|
| WAL corruption on SIGKILL | Low (partial-line skip; CRC-32; conservative lock protocol) | **Zero — SQLite owns it** |
| Index consistency bug | Medium (6 index types × 4 write paths; mitigated by harness + test suite) | **Zero — B-tree by SQLite** |
| Ready-set stale | Medium (incremental openUnblocked maintenance; cross-check in tests) | Low (query replays from index each time) |
| CGo build/runtime | **Zero** | High (mattn/go-sqlite3 requires C toolchain) |
| SQL↔Bead marshal layer | **Zero** | Medium (new per-path error surface; schema migration coupling) |
| Performance under growing concurrency | **Low — in-memory, no shared lock** | High — WAL shared-lock scales worse as goroutine count grows |

The crash-recovery ownership risk is acknowledged and not minimized. The mitigation: SIGKILL-injection tests at every write path, partial-line recovery fuzz testing, and the 48-hour rollback window during cutover (Dolt backup kept hot).

### Deferred bridge fixes — folded into migration, not pre-migration patches

The operator deferred three bridge fixes to the migration. They resolve for free during import:

1. **`wisp_events` FK cascade (bloat bug #1)** — HQStore enforces label/event cascade natively on delete. During import, orphan `wisp_events` rows (45% of 47,334 rows ≈ 21,300 rows eliminated) are simply not imported — they reference wisp IDs that don't exist.

2. **`wisp_labels` FK cascade (bloat bug #2)** — Same. Orphan `wisp_labels` rows (46% of 23,106 rows ≈ 10,600 rows eliminated) are dropped during import.

3. **Mail auto-archive** — the TTLSweeper enforces the retention model from the entity retention table. On import, mail wisps with `created_at > 30 days AND status=open` are archived; read mail older than 7 days is dropped. The +200/day net growth in open mail stops permanently on day one of the new store.

These are not items to schedule separately. The migration IS the fix. No bridge patch on Dolt is needed.

### Phased implementation plan

All work lives on the `experiment/coordination-store` branch. No PRs until the backend is shipped and the rationale is documented.

#### Phase 1 — Build HQStore (Weeks 1–3, ~9 days)

**Goal:** A complete `HQStore` satisfying all 18 FRs, passing the R2.1 harness at 9/9 targets.

| Step | Deliverable | Days |
|---|---|---|
| 1 | `internal/beads/hqstore_core.go` — IndexedMemCore: primary map, 6 secondary index types (label, assignee, status, type, parent, metadata-KV), two-tier routing, all 22 Store methods | 3 |
| 2 | `internal/beads/hqstore_wal.go` — JSONL WAL format, writer, fsync, seq counter, partial-line detection, replay | 2 |
| 3 | `internal/beads/hqstore_checkpoint.go` — checkpoint write/load, atomic rename, recovery orchestration | 1 |
| 4 | `internal/beads/hqstore_ttl.go` — TTLSweeper goroutine, 60 s cadence, expiry enforcement | 0.5 |
| 5 | `internal/beads/hqstore.go` — Open/Close lifecycle, goroutine management, config | 0.5 |
| 6 | `internal/beads/hqstore_*_test.go` — SIGKILL injection, partial-line recovery, checkpoint round-trip, TTL boundary, concurrent write correctness | 2 |

**Gate:** `COORDSTORE_BENCH=1 go test -run TestBenchmarkSuiteRealWorld` scores 9/9 with HQStore adapter registered.

**Pivot trigger (end of Week 3 only):** If WAL fsync latency adds >1 ms to point reads under harness load, evaluate batch-fsync (accumulate writes, fsync every N ms). If WAL complexity proves materially larger than ~420 LOC (>2×), escalate to operator with measurements. If 9/9 cannot be reached, pivot to SQLite-CGo (see Fallback below).

#### Phase 2 — bd shim + provider (Week 4, ~2 days)

**Goal:** Agents call `bd` transparently; it routes to HQStore.

| Step | Deliverable | Days |
|---|---|---|
| 1 | Complete `gc bd-store-bridge` operation coverage — add `show`, `stats`, `count`, `mol` operations | 1 |
| 2 | `gc-beads-hqstore` exec provider script — materializes `bd` shim at `.gc/system/bin/bd`, analogous to `gc-beads-bd` | 1 |

**Gate:** `bd create`, `bd ready`, `bd show`, `bd update`, `bd close`, `bd stats` all route to HQStore in a test city.

#### Phase 3 — Migration tooling + shadow validation (Week 5, ~2 days)

**Goal:** `gc store` subcommands ready; shadow-write running.

| Step | Deliverable | Days |
|---|---|---|
| 1 | `gc store export` — JSONL dump from BdStore (issues + wisps + labels + deps) | 0.5 |
| 2 | `gc store import` — load JSONL into HQStore; drop orphan wisp_events/wisp_labels; apply mail retention; set ID counter to `max(imported IDs) + 1000` | 1 |
| 3 | `gc store validate` — spot-check canonical queries against both stores; diff output | 0.5 |

Shadow-write starts as soon as Phase 2 gate passes. Run 24–48 h; accept if zero discrepancies.

**Gate:** zero discrepancies on shadow-write diff; all R2.3 spot-check queries match.

#### Phase 4 — Cutover (Week 6, ≤60 s downtime)

Sequence per R2.3:

1. `gc stop` (drain agents, 5–15 s)
2. `gc store export` (full JSONL dump, ~2 s)
3. `gc store import` (into HQStore; applies bridge fixes; ~5 s)
4. Rename Dolt data dir to `.gc/store/dolt.backup/` (rollback anchor)
5. `gc start` with `gc-beads-hqstore` provider (store warm-up, ~5 s)
6. Post-cutover spot checks (R2.3 validation protocol)
7. Monitor `gc bd trace` latency telemetry for 48 h

**Rollback (available 48 h):** `gc stop` → restore Dolt backup → swap provider back to `gc-beads-bd` → `gc start` → delta replay from WAL export (bounded by monitoring window writes).

#### Phase 5 — Cleanup (Weeks 7–8, ~3 days)

1. After 48 h monitoring passes: `rm -rf .gc/store/dolt.backup/`
2. Remove `gc-beads-bd` from city config (or no-op provider for legacy cities)
3. Deprecate `BdStore` in `internal/beads/`; keep MemStore/FileStore for tests and tutorial-01
4. File bead: agent prompt migration from `bd` → `gc bd ...` (multi-sprint; do not block cleanup)
5. File bead: rig-DB right-sizing (separate problem; same HQStore solution shape, different data profile per D-6's HQ-only scope)

#### Timeline summary

| Phase | What | Duration | Week |
|---|---|---|---|
| 1 | Build HQStore | 9 days | 1–3 |
| 2 | bd shim + provider | 2 days | 4 |
| 3 | Migration tooling + shadow | 2 days | 5 |
| 4 | Cutover | 1 day | 6 |
| 5 | Cleanup | 3 days | 7–8 |
| **Total** | | **~17 days** | **~8 weeks** |

### Fallback: SQLite (ADOPT) — when to pivot

If Phase 1 demonstrates WAL complexity exceeds the ~420 LOC estimate by 2× or harness 9/9 cannot be reached by end of Week 3, pivot to SQLite (mattn CGo) before Phase 2:

- Implement `BdStoreSQLite` adapter using the existing harness scaffolding (~3 days)
- Accept CGo dependency; document rationale (WAL complexity exceeded estimate; SQLite's 8/9 performance is operationally acceptable)
- Accept the point-read p99 target at 8/9 (1.22–1.63 ms vs 1 ms); the miss is close to machine-variance territory per R2.1b notes
- Phases 2–5 are unchanged — same migration strategy, same tooling

The pivot must happen by end of Week 3. Do not begin Phase 2 without a passing harness gate for the chosen backend.

Pure-Go SQLite (modernc.org) is **not a fallback** — it fails point-read p99 by 3–5× margin and must not be re-evaluated.

### Open decisions resolved by this recommendation

| D# | Question | Resolution |
|---|---|---|
| D-7 (open) | Mail-tier caching as short-term fix | **Closed: skip.** The HQStore's IndexedMemCore serves the ephemeral tier from in-memory indexes, eliminating the mail-poll full-scan hot path (R1) on day one. A CachingStore bridge patch on Dolt is not worth the effort given the 8-week migration timeline. |
| OQ-1 | Round-2 technology decision | **Closed: HQStore.** |
| OQ-2 | Wisp FK gap — bridge fix or skip | **Closed: skip.** Deferred bridge fixes are folded into migration import. |
| OQ-6 | HQ vs rig-DB separation | **Confirmed: treat separately.** File a follow-up bead for rig-DB right-sizing after HQ cutover. |
