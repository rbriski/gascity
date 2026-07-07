# G13 — `request_id` idempotency state machine (spec, pinned)

**Status:** SPEC-FIRST (C1). Code-free. This is the contract C4/C6/C7 build against.
**Parent:** `DESIGN-BRIEF.md` v2 §7.3 (this file is the full text §7.3 points to). Decisions **6** (client-generated `request_id`, all shapes, Slice 1) and **9** (async `202` + atomic rollback; a re-run is a clean re-clone).
**Gate:** G13. Related: G14 (async/rollback), G16 (per-rig-name lock), G22 (wire), G21 (client wait).
**Grounded against HEAD** (`worktree-gc-remote`); every anchor was read, not assumed. This revision folds an adversarial review (concurrency / beads-realism / wire-Huma lenses) — see the change note at the end.

---

## 0. Why this exists (the contradiction it resolves)

Decision 6 wants **idempotent retries**: the same logical `rig add` retried after a
dropped connection must not clone twice or mint a phantom rig. Decision 9 wants
**atomic rollback**: a failed provision leaves *no* rig, and a re-run re-clones cleanly.
Naively these contradict — if a `request_id` is remembered forever, a failed-then-rolled-back
attempt can never re-run under the same id (Decision 9 broken); if it is never remembered,
a mid-flight retry double-clones (Decision 6 broken).

The reconciliation is a **`(city, request_id)` + body-digest state machine** with a
**re-executable terminal state** for rollback, decided against an **in-process live index**
(strong consistency) and backed by a **durable bead record** (crash recovery). Rollback
moves the record to a state a same-id retry treats as *absent* — so the retry re-clones,
exactly once.

---

## 1. Where `request_id` comes from (the inversion)

**This diverges from the session async precedent — do not copy it blindly.**

| | Session async ops (create/submit/message) | Rig-create with `git_url` (this spec) |
|---|---|---|
| `request_id` origin | **Server-minted** `newRequestID()` (`internal/api/request_id.go:14`), `"req-"+hex(12)` | **Client-generated**, carried in the request body (Decision 6) |
| Purpose | Correlation only (match the terminal SSE event) | Correlation **and** idempotency dedup |
| Dedup | none | `(city, request_id)`+digest state machine (this doc) |
| Echoed in 202 | the minted id | **the client's id, verbatim** |

Rules:

- **`request_id` present in the body ⇒ dedup engaged.** The server validates it (§2),
  runs the state machine (§4), and echoes it verbatim in the 202/200 body and every event.
- **`request_id` absent ⇒ no dedup, back-compat.** The server mints one (`newRequestID()`) for
  event correlation, exactly as the session path does; it does **not** create a durable
  idempotency record and does **not** dedup retries. Preserves today's behavior for local
  callers and any client that doesn't opt in. (A client that wants idempotency must supply the
  id and reuse it across retries — that is the whole contract.) **An absent-id *async*
  provision still registers a live `byName` marker under its synthetic id (§3.5)** so the
  name-collision axis (§4.4) protects it — name protection never depends on the dedup opt-in.
- The field is added to **both** `RigCreateInput.Body` and `SlingInput.Body` (Decision 6,
  G22) as `RequestID string \`json:"request_id,omitempty"\`` — optional, so existing callers
  that omit it still validate. (`SlingInput` at `internal/api/huma_types_sling.go:14`;
  `RigCreateInput` at `internal/api/huma_types_rigs.go:24`.) This doc specifies the rig-create
  machine; the sling machine is the same shape over the sling body (§9).

---

## 2. `request_id` validation

`request_id` becomes a bead **metadata value** (§3) and is echoed on the wire; it must be
safe for the digest preimage, the DoltLite metadata JSON column, and — critically — the
**external bd-CLI `--metadata-field` equality filter, which type-infers its value**
(`internal/beads/bdstore.go:2159`; a numeric- or boolean-looking value can be parsed as a
JSON number/bool and then fail to match the JSON-*string*-stored value, so the
`(city,request_id)` lookup would MISS and the request would re-clone — Decision 6 broken).
The native DoltLite path re-filters string-exact in Go (`native_dolt_store.go:761
matchesMetadata`) and is immune, but the spec must be correct for both backends.

Validate at the handler edge, **before** any store/index access:

- Charset/length: `^[A-Za-z0-9._~:-]{8,200}$` (opaque, client-generated).
- **Must contain at least one non-`[0-9]` character**, and must not match
  `^(true|false|null|-?\d+(\.\d+)?)$`. (A UUIDv4, or any hex/base32url id, satisfies this
  trivially; recommend clients use a UUIDv4.) This defeats the bd type-inference miss above.
- Reject control chars / whitespace — the same class of validation `gc context add` applies
  to city names (brief G5), for the same "don't poison a digest/filter preimage" reason.
- On violation ⇒ **400** `invalid_request_id` (typed Huma error), never a 500, never a
  silently-minted substitute (a supplied-but-invalid id is a client bug, not a no-dedup
  opt-out).

The **rig name** used as the name-collision filter value (§4.4) and the lock key (§7) must
likewise be validated **non-empty** and pass the same non-numeric-inferable constraint before
use (a purely-numeric rig name would hit the identical bd filter foot-gun).

---

## 3. Storage substrate — the durable record

**A bead of a LEGAL type carrying the key in metadata. NEVER a new `issue_type`.**
bd validates `issue_type` against `{bug, feature, task}` + the store's registered
`types.custom` list and hard-fails `invalid issue type: <T>` on DoltLite
(`internal/doctor/checks_custom_types.go:29`). A new type would need a config migration across
every city **and** rig store plus DoltLite snapshots. Use `task` + metadata.

### 3.1 Where it lives

The **city's own bead store** (the city ledger), reached server-side via the same `Store` the
controller holds for that city (`internal/beads/beads.go:329`). Per-city, so `(city,
request_id)` is unique within the store. It MUST be a normal durable (`TierIssues`) bead —
**not** an ephemeral/wisp bead (TTL-GC'd, not git-synced); the record must survive a crash
between admission and completion.

### 3.2 Shape

Created in a **single `Store.Create`** call (atomic on every backend for a single create):

```
Bead{
  Type:   "task",                                  // legal type — beads.go:54
  Title:  "idem: rig-create <request_id>",
  Labels: []string{"gc-idem", "gc-idem-rig-create"}, // coarse labels for the §6 boot sweep only
  Metadata: map[string]string{                     // flat strings only
    "gc.idem.kind":         "rig-create",
    "gc.idem.city":         "<cityName>",
    "gc.idem.request_id":   "<request_id>",         // §2-validated non-numeric-inferable
    "gc.idem.digest":       "<hex sha256, §3.3>",
    "gc.idem.state":        "in_flight",            // in_flight | succeeded | rolled_back
    "gc.idem.event_cursor": "<decimal seq, §5>",
    "gc.idem.rig_name":     "<name>",
    // on success (merged later):
    // "gc.idem.result.rig": "<name>", "gc.idem.result.prefix": "...", "gc.idem.result.branch": "..."
  },
}
```

`request_id` lives in **metadata only** — there is no per-request_id label (the labels are the
coarse `gc-idem*` markers used only to **sweep orphan `in_flight` records at boot**, §6; the
live index is **never** rebuilt from them — §3.5 starts it empty). The lookup is metadata-only
(§3.4).

### 3.3 The digest (`gc.idem.digest`)

Binds a `request_id` to the exact request it first named (§4.3):

```
digest = hex( sha256( canonicalRequestBytes ) )
canonicalRequestBytes = json.Marshal( <the Body struct with RequestID zeroed> )
```

- Go's `encoding/json` marshals struct fields in declaration order and **sorts map keys**, so
  this is deterministic for `RigCreateInput.Body` (no maps) and `SlingInput.Body` (its `vars`
  map marshals sorted). No hand-rolled canonicalization needed.
- Zero `RequestID` before marshaling so the digest covers only the provisioning-relevant
  fields (name/path/prefix/default_branch/git_url), not the key.
- Distinct from `citywriteauth.ReqDigest` (`internal/citywriteauth/citywriteauth.go:261`,
  which digests HTTP method\npath\nbodyhash for the *grant*). Reference it as prior art; do
  not conflate. Weaker in-memory analog to mirror (not copy codes from):
  `internal/api/idempotency.go`.

### 3.4 Read/write API (grounded)

- **Look up** `(city, request_id)` in the durable store:
  `Store.List(beads.ListQuery{ Metadata: {"gc.idem.kind":"rig-create", "gc.idem.city":city, "gc.idem.request_id":id}, IncludeClosed: true, Limit: 2 })`
  (`internal/beads/query.go:66`; conjunctive AND equality; a metadata filter alone satisfies
  the query — no `AllowScan` needed). **`IncludeClosed:true` is mandatory** for succeeded
  records (a closed record vanishes otherwise). `Limit:2` so a >1 result is a detectable
  invariant violation. **This durable lookup is only consulted for records NOT present in the
  live index (§3.5)** — i.e. succeeded/rolled_back/orphan records, all of which were committed
  before any concurrent decision, so the store's read-after-write lag does not affect them.
- **Transition** in_flight→succeeded / →rolled_back and merge result fields:
  `Store.SetMetadataBatch(id, map[string]string{...})` (`internal/beads/beads.go:398`) —
  read-modify-**merge** on the single JSON metadata column. No metadata-key delete exists;
  model every transition as an overwrite of `gc.idem.state` (+ additive result keys).
- **Atomicity:** `StoreSupportsAtomicTx` is **false only for the external bd-CLI store
  (`BdStore`) and the exec store**; `NativeDoltStore.AtomicTx()` returns **true**
  (`internal/beads/native_dolt_store.go:978`), and there a single `SetMetadataBatch` is one
  atomic JSON-column write and `Store.Tx` rolls back atomically. Design the transition to
  survive the non-atomic bd-CLI path (each write idempotent, record reconstructable), but do
  not falsely assume no backend is atomic.

### 3.5 Consistency model — in-process live index + durable record (the fix that defeats ledger lag)

The hosted city ledger is a bd/DoltLite **gateway with documented cross-connection
read-after-write lag**: a just-`Create`d row is not immediately visible via `List` on another
pooled connection (`internal/sling/sling_core.go:24-30`; `waitForSourceWorkflowLaunchVisible`,
`sling_core.go:964`, exists precisely to retry 5×100ms around this). **Therefore
lookup-then-Create — even under a lock — is NOT mutual exclusion**: two retries within the lag
window both `List`→miss and both `Create` → double-clone.

Fix (aligned with the *accepted single-replica residual risk*, brief §8, and the existing
process-local `idempotencyCache` / `MemoryReplayGuard` precedents):

- **Live index = authoritative for admission decisions.** An in-process structure, guarded by
  the same lock as admission (§7), holding **only currently-running** provisions:
  - `inflight: map[key(city,request_id)] → { digest, eventCursor, rigName, done chan }`
  - `byName:   map[key(city,rigName)]   → request_id`  (the name-collision axis)
  Reads are strongly consistent — no ledger round-trip, no lag.
- **Every async provision registers a `byName` entry — even without a client `request_id`.**
  An absent-id `rig add --git-url` mints a **synthetic** correlation id (`newRequestID()`) that
  keys its `inflight`/`byName` entry so §4.4 protects the name, but creates **no durable
  idempotency record** (no dedup — §1). Name protection never depends on the dedup opt-in.
- **Durable bead record = authoritative for crash recovery and succeeded-replay.** Written
  alongside every *id-bearing* live entry, and the sole survivor of a crash.
- **Admission reads the live index FIRST** (strong consistency), and only falls back to the
  durable store for keys the index does not hold — exactly the records committed in the past
  (succeeded / rolled_back / pre-crash orphan), where lag is irrelevant.
- **Terminal — rollback:** roll back all state (§6), write durable `rolled_back`, then remove
  the `inflight`/`byName` entries and close `done`.
- **Terminal — success (ORDER IS LOAD-BEARING):** the durable `succeeded` write **and** the
  `inflight`/`byName` removal happen **only after the G17 visibility barrier is satisfied** —
  `s.state.Config()` shows the rig **and** `state.BeadStore(rigName)!=nil` — the same barrier
  G17 puts before the success event. Removing `byName` before the rig is visible opens a window
  where a concurrent different-id POST for the same name misses config *and* `byName` and
  double-clones, and a same-id retry 200-existings a rig a follow-on `sling` then 404s on (the
  exact G17 flakiness, re-introduced on the retry path). Keep the entry until the rig is real.
- **Boot:** the live index starts **empty** (no live provisions exist) and is **never** rebuilt
  from durable records — the `gc-idem*` labels drive only the §6 orphan sweep. `in_flight`
  durable orphans are reconciled to `rolled_back` (§6), never loaded as live; `succeeded`
  records are served from the store on demand (200-existing), not preloaded.
- **Liveness limitation:** a live entry is removed only by its goroutine's terminal step
  (backstopped by `defer recoverAsRequestFailed` for panics). The **boot sweep covers only
  full-process-exit crashes**; an in-process goroutine lost without a process exit would leak a
  live `in_flight` entry (hung replays + a wedged name). C4 SHOULD give live entries a bounded
  liveness signal (a watchdog over `done`, or a heartbeat) that reaps a stale entry to the
  re-clone path. A known single-replica limitation.

This makes the live decision independent of ledger lag on the single-replica target. A second
controller replica against the same write-auth city is **refused / boot-warned** (accepted
constraint, brief §8/§10); a cross-replica shared index is Slice 3 (§12).

---

## 4. The state machine

### 4.1 States

```
        (no live entry, no durable record)             digest mismatch, any state ─────► 409 body-mismatch
             │                                          (request_id ↔ digest is a stable binding, §4.3)
   request_id present
             │
             ▼
        ┌──────────┐  live entry present    ┌───────────┐
        │ in_flight │ (goroutine running)   │ succeeded │ ── same-id retry (durable) ──► 200 existing
        └──────────┘  same-id retry ─► 202   └───────────┘
             │  replay (cursor from index)        ▲
   provision │                                    │ provision ok (goroutine → durable + drop live entry)
   fails +   │                                    │
   rollback  ▼                                    │
        ┌─────────────┐  same-id retry (durable, live entry ALREADY gone)
        │ rolled_back │ ── digest match ──► re-clone (reset durable to in_flight, new live entry, 202)
        └─────────────┘    (re-executable terminal state — treated as ABSENT for a same-digest
                            retry; still 409 for a different-digest reuse)

  orphan in_flight durable record with NO live entry (crash survivor)  ── swept to rolled_back at boot (§6),
                                                                          then behaves as rolled_back above.
```

Key rule enforced by §3.5: **`in_flight` replay (202) is returned only when the LIVE INDEX
holds the key** (a goroutine is actually running). An `in_flight` *durable* record with no live
entry is an orphan, never a passive replay — it is the re-clone path. This closes the
"replay-202 with no goroutine → client hangs forever" hole.

### 4.2 The six responses

| Situation | HTTP | How it's produced | Notes |
|---|---|---|---|
| **new** — no live entry, no durable record | **202** | output struct `Status=202`, body `{status:"accepted", request_id, event_cursor}` | create durable in_flight + live entry, capture cursor, spawn goroutine |
| **in-flight replay** — **live entry** present, digest matches | **202** | output struct `Status=202`, body with the **live entry's** `event_cursor` | do NOT re-spawn; return the original cursor |
| **existing** — no live entry; durable `state=succeeded`, digest matches | **200** | output struct `Status=200`, body `{status:"exists", request_id, rig, prefix, default_branch}` | synchronous, served from the record |
| **re-clone** — no live entry; durable `state=rolled_back` (or orphan in_flight swept), digest matches | **202** | output struct `Status=202` | reset durable to in_flight **with a fresh `event_cursor` (§5)**, new live entry, spawn |
| **body-mismatch** — live or durable record exists, digest **differs** (any state) | **409** | Huma typed error `request_id_conflict` (ext: `request_id`) | reused id for a different request; refuse in all states, incl. rolled_back |
| **name-collision** — no request_id match, rig name taken/in-flight under another id | **409** | Huma typed error `rig_name_conflict` (ext: `rig`, optional in-flight `request_id`) | distinct code (§4.4) |

**Wire mechanics (see §10 — this is load-bearing):** `202` and `200` are **both** produced by
the operation's single output struct via a runtime `Status int \`json:"-"\`` field (the
session pattern, `huma_types_sessions.go:85`). Only `400`/`409` use Huma's typed-**error**
return path. `200` and `202` **cannot** ride the error path (it emits `huma.ErrorModel`, not a
success body). A runtime `Status int` documents nothing in OpenAPI by itself — the codes are
added manually (§10).

### 4.3 `request_id` ↔ digest is a stable binding

Once any record exists for `(city, request_id)` (live or durable), its digest is **fixed for
its lifetime**. A retry with a *different* digest is always **409 body-mismatch**, even after
rollback. Only a **same-digest** retry after rollback re-clones. A `request_id` names one
request, forever.

### 4.4 Name-collision is a SECOND dedupe axis (brief §7.3)

request_id dedup answers "is this the *same* request?". It does not stop *two different*
`request_id`s from both creating rig `foo`. Under the per-rig-name lock (§7), after the
request_id lookup misses, resolve the rig-name axis in this order:

- **live `byName` map** (strong consistency, §3.5) holds `foo` under a different id ⇒
  **409 `rig_name_conflict`**, ext carries that in-flight id + its `event_cursor` so a
  coordinating client can attach.
- rig `foo` already in `s.state.Config()` (pre-existing or a fully-visible prior success) ⇒
  **409 `rig_name_conflict`**.
- **durable records scanned by `gc.idem.rig_name == foo`** with state `in_flight` **OR
  `succeeded`** ⇒ **409 `rig_name_conflict`**. The `succeeded` scan is the required backstop for
  the window where a provision has committed `durable=succeeded` but the rig is not yet visible
  in config/`BeadStore` (§3.5 terminal barrier) — without it a different-id add could slip
  through and double-clone; it also covers a cloned-but-config-lost rig from a §7.1 lost update.

Because the primary exclusion is the in-process `byName` map (not a lagging store read), two
different-id/same-name retries within the lag window are correctly serialized (the earlier
double-clone finding is closed by §3.5). The durable scan backstops orphans and the
committed-but-invisible window.

---

## 5. `event_cursor` — why it is stored, and captured when

`event_cursor` is a decimal-uint64 **string** = the city event-log head captured **before**
the provisioning goroutine starts (`currentCityEventCursor()`, `internal/api/request_id.go:22`;
`"0"` if no provider). It is the `after_seq` the client passes to
`GET /v0/city/{city}/events/stream` so it catches the terminal
`request.result.rig.create` / `request.failed` event without replaying backlog (G21 wait).

- **Captured in the synchronous handler body, before `go func(){…}`** — the load-bearing
  ordering the session handlers use (`huma_handlers_sessions_command.go:315→323`).
- **Held in the live entry AND persisted in the durable record** (`gc.idem.event_cursor`).
  The in-flight replay returns the live entry's cursor. The durable copy is **write-only on the
  serving path in Slice 1** — no admission/serve handler reads it (a post-crash retry re-clones,
  §4.1, rather than replays); it exists for audit / the G23 runbook, and a re-clone refreshes it
  (§4.2) so it always matches the latest attempt.

---

## 6. Rollback & crash recovery (drop-then-mark, both paths)

**Invariant: a record never reaches `rolled_back` until the partial clone dir / rig DB / any
config registration for its `gc.idem.rig_name` is fully removed.** Otherwise a same-digest
re-clone admitted immediately (§4.2) starts a fresh clone into a staging dir the failed op has
not finished tearing down. This ordering is pinned for **both**:

- **Runtime rollback (Decision 9, in the goroutine, detailed in G14):** on any provisioning
  step failure — (1) roll back dir + Dolt DB + config; (2) `SetMetadataBatch(state=rolled_back)`;
  (3) remove the live-index entry; (4) emit terminal `request.failed`.
- **Boot sweep (G14 orphan reconcile):** the live index is empty at boot, so every durable
  `in_flight` record is an orphan. For each: (1) drop any partial dir / DB / config for its
  `rig_name`; (2) `SetMetadataBatch(state=rolled_back)`. **NORMATIVE: the sweep MUST complete
  before the rig-create/sling handlers are admitted to serve.** (The live-index gating
  (§3.5/§4.1) additionally guarantees an un-swept orphan routes to *re-clone* rather than a hung
  replay — but that is strictly weaker: it does **not** remove the orphan's partial
  dir/DB/config, and a re-clone must never begin over un-cleaned staging (the top-of-section
  invariant). So sweep-before-serve is the requirement, not an option.)

`rolled_back`-as-re-executable (vs a hard delete) is the default because it needs no delete
primitive, keeps an audit trail, and is found deterministically by the durable lookup. (A
durable bead **delete**, if used, is behaviorally equivalent: the lookup then returns nothing
⇒ 202-new. Either satisfies the contract.)

---

## 7. Concurrency & locking (what C1 pins for C4)

Admission (validate → consult live index → durable fallback → name-collision → create/reset
record + live entry + capture cursor) runs **inside the per-rig-name lock (G16)**; the handler
then returns 202 and does the actual clone/init/compose in a detached `context.Background()`
goroutine (session pattern). The **live-index entry** — not a held lock — excludes concurrent
same-name work across the goroutine's lifetime.

- **The per-rig-name lock serializes admission** so the live-index reads/writes for one rig
  name are a critical section: same `request_id` (⇒ same name) ⇒ same lock ⇒ the second sees
  the live entry and replays 202; different `request_id`, same name ⇒ same lock ⇒ the second
  hits `rig_name_conflict`. Strong consistency comes from the **in-process index (§3.5)**, not
  the lock alone — the lock without the index does not defeat ledger lag.
- **Lock model:** the two-tier `sourceworkflow.WithLock` (refcounted channel-token in-process
  mutex + on-disk flock, `internal/sourceworkflow/sourceworkflow.go:228`), **not** a leaking
  `map[string]*sync.Mutex`. **Gotcha: `WithLock` early-returns `fn()` with NO lock when the id
  is empty (`sourceworkflow.go:230`).** The rig name MUST be validated non-empty (§2) before it
  keys the lock; an empty key is a programming error, not a no-op-opt-out.

### 7.1 C4 caveat — the per-rig-**name** lock does NOT serialize city-scoped writes

A per-rig-name lock lets two adds for **different** names (`foo`, `bar`) run fully concurrently.
But a `git_url` provision performs several **city-scoped** read-modify-writes that then race and
lose updates. **C4 MUST wrap the entire city-scoped critical section in a DISTINCT per-city
lock** (in addition to the per-rig-name admission lock). Enumerated city-scoped resources:

1. **`city.toml`** — appended via `AppendRigAndWriteSiteBindingsForEdit`
   (`internal/config/site_binding.go:356`, read-modify-write of the whole file) /
   `writeCityConfigForEditFS`. Two concurrent different-name adds read the same base and the
   second overwrites the first's append ⇒ a fully-cloned on-disk rig **absent from config**.
2. **Routes file** — regenerated wholesale from the full cfg by `writeAllRoutes` /
   `collectRigRoutes` (`cmd/gc/rig_beads.go:48,95`). Same lost-update race.
3. **`cityDoltConfigs`** register/clear (`cmd/gc/beads_provider_lifecycle.go:109`), keyed by
   `normalizePathForCompare(cityPath)`. The `defer clearCityDoltConfig` at `cmd_rig.go:276`
   fires at function return with no surrounding lock; a concurrent add's clear can wipe the
   shared entry mid-provision.

Additional C4 requirements for that per-city lock:

- **Do NOT reuse `providerOpSemaphores` / `acquireProviderSemaphore`
  (`beads_provider_lifecycle.go:2075`).** It is a non-reentrant 1-slot channel
  (`:2101`), and the provisioning path re-enters it: the inner bead-store init calls
  `ensureBeadsProvider` → `acquireProviderSemaphoreForOp(cityPath,"start")` (`:720`). Holding
  the outer slot deadlocks the inner acquire until the 120s `providerOpTimeout` (`:2132`), then
  the provision fails. Use a **separate** per-city lock map.
- **Key the per-city lock on `normalizePathForCompare(cityPath)`** — the same normalized key
  `cityDoltConfigs` writes under. (Latent existing bug to fix in passing: the four
  `cityDoltConfigs` *readers* Load with the RAW cityPath (`:354,:1019,:1232,:2033`) while
  register/clear write the normalized key; unify them or the dolt env silently never applies.)
- Register and its matching clear must both sit inside the held per-city lock span; a `defer
  clear` that outlives the lock re-opens the window.

---

## 8. Sync (config-append) vs async (`git_url`) — where the machine engages

Per G14, the response mode is conditional on `git_url`:

- **`git_url` present** ⇒ full provisioning ⇒ **async 202** ⇒ the machine runs with a real
  in_flight window + live entry (this whole doc).
- **no `git_url`** (bare config-append) ⇒ **sync 201** (unchanged). If `request_id` is present
  the machine still dedups, but there is no in_flight window and no live entry. **Ordering that
  keeps the "no orphan on failure" invariant true:** perform the config append FIRST, then
  `Store.Create` the durable record **already `state=succeeded`** — so a failure at/before the
  append leaves **no record** (→ 400/500, clean retry) and a success leaves a `succeeded` record
  a same-id retry serves as **200 existing**. Do **not** create an `in_flight` record on the
  sync path (that would strand an orphan whose recovery rules are async-only). Dedup here covers
  **sequential** retries; two *concurrent* same-id sync requests fall back to the per-rig-name
  lock + name-collision axis (§4.4) — §3.5's lag lemma is out of scope because there is no
  in-flight window to protect. One machine, uniform; the async path adds the in_flight/rollback
  states + the live index.

---

## 9. `SlingInput` gets the same field (Decision 6), same machine shape

`request_id` is added to `SlingInput.Body`. Sling is **synchronous** today
(`ConflictError`→409 already makes source-bead launches retry-safe,
`internal/sling/sling_core.go:835`), so its machine is the degenerate sync case (§8): dedup by
`(city, request_id)`+digest over the sling body, `succeeded`→200-existing on retry, `409`
body-mismatch on a reused id with a different body. No async/in_flight/rollback/live-index for
sling in Slice 1.

**Brief §7.3 requires `request_id` echoed in success bodies**, so C6 MUST add
`RequestID string \`json:"request_id,omitempty"\`` to `slingResponse`
(`internal/api/handler_sling.go:39`) and populate it on both the fresh and the 200-existing
paths, plus a sling `request_id_conflict` typed 409 (mirroring rig-create). Sling's digest uses
§3.3 (its `vars` map marshals sorted → stable). One idempotency mechanism across both mutating
endpoints.

---

## 10. Wire additions this spec implies (for G22 / C6)

**Inputs:** `RigCreateInput.Body.RequestID` and `SlingInput.Body.RequestID`, both
`json:"request_id,omitempty"` (optional keeps existing callers + `TestOpenAPISpecInSync` happy).

**Rig-create output — ONE unified Go output struct** (Huma binds exactly one output type per
operation; three distinct success bodies is infeasible):

```go
type RigCreateOutput struct {
    Status int `json:"-"`            // runtime code: 200 | 201 | 202 (huma reads this)
    Body   RigCreateResponseBody
}
type RigCreateResponseBody struct {
    Status        string `json:"status"                doc:"created | accepted | exists"`
    Rig           string `json:"rig,omitempty"`
    RequestID     string `json:"request_id,omitempty"`
    EventCursor   string `json:"event_cursor,omitempty"`
    Prefix        string `json:"prefix,omitempty"`
    DefaultBranch string `json:"default_branch,omitempty"`
}
```

The handler populates the relevant subset and sets `Status` (201 sync config-append, 202 async
provisioning, 200 existing). `asyncAcceptedBody` / a `RigProvisionExistingBody` may exist as
documentation sub-schemas but NOT as the operation's Go output type. Only `400`/`409` use the
Huma **error** return.

**Documenting the codes (critical — a runtime `Status int` adds NOTHING to OpenAPI):** Huma's
spec generator only schematizes `op.DefaultStatus` (`huma.go` ~1528/1547); the runtime `Status`
field is read separately (~1141) and never touches the spec. So:

- Keep `DefaultStatus = http.StatusCreated` (201) → Huma auto-schematizes the unified body for
  201.
- **Manually add `op.Responses["200"]` AND `op.Responses["202"]`** with hand-authored Content
  schemas (registry `SchemaRef` for the exists/accepted shapes). Both — not just 202.
- **Registration mechanics:** create-rig is registered via `cityRegister`
  (`supervisor_city_routes.go:118`), whose signature (`city_scope.go:175`) takes **no**
  op-modifier closure and passes `op` **by value** into `huma.Register` (`city_scope.go:182`).
  So the manual `op.Responses` entries must be built on the `huma.Operation` **literal before
  it is passed to `cityRegister`** (Register only fills the DefaultStatus response and won't
  regenerate after). Either pre-populate there, or extend `cityRegister` to accept
  `opts ...func(*huma.Operation)` like `cityPost`. Note the `sse.go:228` precedent mutates a
  schema-**less** body-func response, so it under-represents the hand-authored-schema work
  needed here (create-rig has a real typed body).
- 400/409 covered via `op.Errors` / the error return.

**Events (C5 / G20):** `RequestResultRigCreate` const + `RigCreateSucceededPayload{RequestID,
Rig, Prefix, DefaultBranch}` + extend `RequestFailedPayload.Operation` enum with `rig.create`
+ register in `event_payloads.go init()` + add to `events.KnownEventTypes` + a case in
`requestIDFromPayload` (`TestEveryKnownEventTypeHasRegisteredPayload` gates the build).

Regenerate `internal/api/openapi.json` + `docs/reference/schema/openapi.json` + `genclient` +
dashboard TS **in one commit** (`go run ./cmd/genspec` + `go generate ./internal/api/genclient`
+ `make dashboard-check`; `TestOpenAPISpecInSync`).

---

## 11. Test matrix (maps to brief §10)

State machine (durable record + live index, `t.TempDir()` city store unless noted):

- **202 new** creates exactly one durable in_flight record + one live entry with digest + cursor.
- **202 in-flight replay**: a second identical POST while the live entry exists returns 202 with
  the **same** `event_cursor` and spawns **no** second goroutine (assert one clone call).
- **200 existing**: retry after success returns 200 + stored rig result, no wait.
- **202 re-clone**: retry after rollback (same digest) returns 202 and re-executes exactly once.
- **409 body-mismatch**: same `request_id`, changed `name`/`git_url` ⇒ 409 in in_flight,
  succeeded, and rolled_back states.
- **409 name-collision**: different `request_id`, same rig name (pre-existing and
  concurrent-live) ⇒ 409 `rig_name_conflict` carrying the in-flight id.
- **400 invalid_request_id**: control chars, too short/long, **and pure-numeric / `true` /
  `null`** (the bd type-inference guard).
- **absent request_id**: no record created; two POSTs both proceed (back-compat).
- **ledger-lag double-clone guard (the critical regression):** simulate a `Store` whose `List`
  lags a just-`Create`d row (a fake store that returns stale reads for N ms, modeling
  `sling_core.go:24-30`); fire two identical retries within the lag window; assert the
  **in-process live index** yields exactly ONE provision (not two). This is the test the
  original spec lacked — a plain `t.TempDir()` store has strong RYOW and hides the bug.
- **orphan never replays**: a durable in_flight record with **no live entry** (simulated crash)
  ⇒ a same-id retry routes to re-clone (or is swept first), never a hung 202 replay; and a
  `bd close`d in_flight record is likewise not served as a live replay.
- **crash reconcile**: boot sweep drops partial dir then marks rolled_back (assert order); a
  same-id retry then re-clones.
- **runtime rollback order**: assert `state=rolled_back` is written only after the partial
  dir/DB/config is gone (a re-clone admitted immediately finds no leftover staging dir).
- **city-scoped concurrency (C4)**: two concurrent **different-name** adds on one city both land
  in `city.toml` + routes (no lost update) — guards §7.1.
- **empty rig name**: rejected before the lock (never runs unserialized via the `WithLock`
  empty-id early return).
- **digest determinism**: percent-encoded / unicode rig name, and a sling body with a populated
  `vars` map, produce a stable digest across marshals.
- **IncludeClosed**: a closed succeeded record is still found on 200-existing replay.

Wire (C6): `TestOpenAPISpecInSync` shows create-rig documenting **200, 201, 202** + 400/409;
genclient/dashboard TS carry the unified body + both input `request_id` fields; sling success
body echoes `request_id`.

---

## 12. Explicitly deferred (Slice 3)

- **Cross-replica shared index / `request_id` store.** The live index (§3.5) is process-local
  and the durable record lives in the city's single bead store; with a single controller
  replica (accepted residual risk, brief §8) that IS the shared state. A second replica against
  the same write-auth city is refused / boot-warned. A cross-replica coordination layer
  (shared `ReplayGuard` + shared idem store) is Slice 3 — **the single-replica constraint is
  load-bearing for §3.5 and must stay documented in the runbook (G23).**
- **`scope` claims / per-city keys** — Slice 3, unrelated to this machine.

---

## 13. Anchor index (seams C4/C6/C7 build against)

| Concern | Anchor |
|---|---|
| Client-mint vs server-mint contrast | `internal/api/request_id.go:14` (`newRequestID`), `:22` (`currentCityEventCursor`) |
| 202 body/status shape to reuse | `internal/api/huma_types_sessions.go:77` (`asyncAcceptedBody`), `:85` (`Status int json:"-"`) |
| Async launch ordering to copy | `internal/api/huma_handlers_sessions_command.go:315→323` (cursor before goroutine; `defer recoverAsRequestFailed`) |
| **Ledger read-after-write lag (the §3.5 driver)** | `internal/sling/sling_core.go:24-30` (lag doc), `:964` (`waitForSourceWorkflowLaunchVisible`) |
| Process-local dedup precedents | `internal/api/idempotency.go:30` (`idempotencyCache`), `MemoryReplayGuard` (write-auth) |
| Bead record store API | `internal/beads/beads.go:333` (`Create`), `:398` (`SetMetadataBatch`), `query.go:66` (`List`/`IncludeClosed`) |
| Atomicity truth | `internal/beads/native_dolt_store.go:978` (`AtomicTx()==true`); `BdStore`/exec = false |
| bd filter type-inference (the §2 guard) | `internal/beads/bdstore.go:2159` (`--metadata-field`), `native_dolt_store.go:761` (`matchesMetadata`, safe) |
| DoltLite type trap | `internal/doctor/checks_custom_types.go:29` (`RequiredCustomTypes`) |
| Body-digest prior art | `internal/citywriteauth/citywriteauth.go:261` (`ReqDigest`) |
| Per-rig-name lock model + empty-key gotcha | `internal/sourceworkflow/sourceworkflow.go:228` (`WithLock`), `:230` (empty-id early return), `:36` (`ConflictError`) |
| City-scoped resources C4 must serialize | `internal/config/site_binding.go:356` (`AppendRigAndWriteSiteBindingsForEdit`), `cmd/gc/rig_beads.go:48,95` (`writeAllRoutes`/`collectRigRoutes`), `cmd/gc/beads_provider_lifecycle.go:109` (register/clear) |
| Per-city guard: what NOT to reuse | `cmd/gc/beads_provider_lifecycle.go:2075` (`acquireProviderSemaphore`, non-reentrant), `:720` (`ensureBeadsProvider` re-enters), `:2101/:2132` (cap-1 chan / 120s timeout) |
| Current rig-create shapes | `internal/api/huma_types_rigs.go:24` (`RigCreateInput`), `huma_types.go:263` (`RigCreatedOutput`), `huma_handlers_rigs.go:67` |
| Rig-create registration (no opts closure) | `internal/api/supervisor_city_routes.go:118` (`cityRegister`), `city_scope.go:175,182` |
| Sling shapes | `internal/api/huma_types_sling.go:14` (`SlingInput`), `handler_sling.go:39` (`slingResponse` — add `RequestID`) |
| Manual `op.Responses` precedent (caveat: schema-less) | `internal/api/sse.go:228` |
| Structured 409 rendering | `internal/api/huma_handlers_sling.go:102` |
| Client wait to mirror | `internal/api/client.go:1140` (`SubmitSession`), `:292` (`waitForEvent`), `:283` (`sseEnvelope` — add `Seq` for G21) |

---

### Change note (post-review revision)

Folded an adversarial review (concurrency / beads-realism / wire-Huma lenses, run over the
first draft). Material changes: **§3.5 in-process live index** added to defeat the hosted
ledger's cross-connection read-after-write lag (was a critical double-clone hole); admission and
the name-collision axis now read the live index first (§4/§7); **in-flight-replay gated on a
live entry** so orphans re-clone instead of hanging the client (§4.1/§6); **runtime + boot
rollback both pinned drop-then-mark** (§6); **§7.1 C4 caveat expanded** to `city.toml` + routes
+ the `providerOpSemaphores` self-deadlock + cityPath normalization + the `WithLock` empty-key
no-op; **§2 tightened** against bd `--metadata-field` type-inference and the stray "label"
claim dropped; **§3.4 atomicity corrected** (`NativeDoltStore.AtomicTx()==true`); **§10 wire
model rewritten** to one unified output struct + runtime `Status int` + manual
`op.Responses["200"|"202"]` (three documented 2xx codes) with the correct `cityRegister`
mechanics; **§9 sling** now echoes `request_id`. New regression tests in §11 (ledger-lag
double-clone, orphan-no-replay, city-scoped concurrency, numeric-id rejection).

A second (correctness + consistency) pass then folded: the async **success terminal is pinned
behind the G17 visibility barrier** and §4.4 gains a **`succeeded`-by-name durable backstop**
(closing a double-clone / 200→`sling`-404 window, §3.5/§4.4); **every async provision registers
a `byName` marker even absent a client id** via a synthetic id (§1/§3.5); the **sync-201 path
creates its record `succeeded`-after-append** so a failure strands no orphan (§8); re-clone
**refreshes the durable cursor** and §5 marks that cursor **write-only on the serving path**; a
**live-entry liveness limitation** is documented (§3.5); and two internal contradictions were
removed — §3.2 no longer claims the labels "rebuild the live index" (they only drive the §6
sweep) and §6 makes **sweep-before-serve normative** (not "equivalently preferred").
