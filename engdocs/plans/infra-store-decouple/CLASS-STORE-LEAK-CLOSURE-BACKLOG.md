# Per-class store-interface leak closure — backlog

**Goal.** Make each coordination class's store interface the *sole* access path to
its beads, so a per-class **backend swap** (a different data model for one class)
is a change at ONE seam, not a hunt across the tree. The end state: for every
class, `grep` finds no direct `beads.Bead` construction, raw `.Metadata[…]` crack,
or raw-`beads.Store` CRUD on that class's beads outside its store/projection —
CI-guard-enforced.

## Why this is the exact prerequisite for the backend swap (b)

The seam is **already built**: `cmd/gc/class_store.go` gives every class a named
accessor (`sessionsBeadStore()`, `mailBeadStore()`, `ordersBeadStore(scope)`,
`nudgesBeadStore()`, `graphBeadStore()`, `cityWorkStore()`), each delegating to a
`resolve<Class>Store(...)` that returns a **different** backend when
`[beads.classes.<X>]` relocates the class, else identity to the work store. The
typed wrappers in `internal/beads/class_store.go` (`SessionStore`, `MailStore`,
`OrdersStore`, `NudgesStore`, `GraphStore`, `WorkStore`) make the class statically
visible at compile time. So relocating a class = point its resolver at a new store.

**That relocation is only correct if every access already routes through the class
accessor + typed projection.** Any bypass — code holding the raw work store, or
constructing/cracking the bead directly — reads/writes the OLD backend after the
relocation, silently splitting the class's state across two stores. Driving those
bypasses to zero (this backlog) is what makes the swap safe.

Two layers must both be sealed per class:
1. **Access layer** — reach the store via `<class>BeadStore()` / the typed
   front door, never a raw `beads.Store` held for that class.
2. **Shape layer** — read/write via the typed projection (`session.Info`,
   `mail.Message`, `orders.OrderRun`, `nudgequeue.NudgeShadow`, …), never raw
   `beads.Bead{…}` construction or `.Metadata[…]` cracks.

Enforcement mechanism (already in place, extend per class):
`cmd/gc/frontdoor_di_guard_test.go` — `frontDoorStoreFreeFiles` (no raw store),
`snapshotInfoOnlyFiles` (no raw snapshot accessors), `metadataInfoOnlyFiles`
(no `.Metadata[` crack). A file is "closed" when it's added to the relevant list.

> Counts below are **indicative shapes** (scout sweep + spot-checks), NOT audited
> call-site inventories. Each class gets a verified per-file census in its own
> closure phase before edits — the byte-identity discipline from the reconciler
> front door (see `RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md`) applies here too.

---

## Ranked by remaining delta (smallest first = best proof-of-concept for the swap)

### 1. Nudge queue — `nudgequeue.Store` — **✅ VERIFIED SEALED (closed 2026-07-04)**
- **Interface:** complete. `Save`/`Terminalize`/`Find*`/`FindBead`/`DecodeShadow`; the
  bead is a *shadow/observer* of a `state.json` authority (`internal/nudgequeue/state.go`).
- **Access seam:** `nudgesBeadStore()` → `resolveNudgesStore`; `nudgeFrontDoor(` (the
  construction site, `cmd/gc/nudge_beads.go` — a thin adapter over the Store).
- **Census result (corrected):** the nudge shadow bead is `Type:"chore"` + `gc:nudge` label.
  The shadow-bead **lifecycle** (create/terminalize/find) was already sealed behind
  `nudgequeue.Store.Save/Terminalize/Find/FindBead`. But an earlier "fully sealed" claim was
  wrong: the **maintenance sweep** (`nudge_mail_sweep.go` phase-1) constructed a raw
  `nudgeStore.List(ListQuery{Label:nudgeBeadLabel, …})` in package main — a nudge-bead query
  the lifecycle API doesn't expose. **Closed:** added `nudgequeue.StaleCandidatesBefore(store,
  before, limit)` encapsulating that query (the `gc:nudge` label + wisp tier stay in
  `internal/nudgequeue`); routed both sweep sites (live + dry-run) through it.
- **Residual (cross-class, not nudge):** the `.Metadata["nudge_id"]` reads in `cmd_wait.go` /
  the sweep are on **wait/mail** beads that *reference* a nudge — wait/mail-bucket reads. One
  `doctor_backlog_depth.go` `title/label` classification heuristic is a read-only diagnostic
  (acceptable raw-by-design). `nudge_beads.go` legitimately holds the `nudgeFrontDoor(`
  construction (can't join a store-free guard).
- **Outcome:** nudge-bead lifecycle AND maintenance query now confined to `internal/nudgequeue`;
  `resolveNudgesStore` relocation captures nudge access. Seal is structural (the `Type:"chore"`
  literal legitimately lives in the package; no clean substring guard).

### 2. Orders — `orders.Store` — **re-scoped: an access-layer Store-routing refactor, not a label cleanup**
- **Interface:** strong. `CreateRun`/`SetOutcome`/`SetCursor`/`CloseRun`/`RecentRuns`;
  label-codec (`order-run:<scoped>` + outcome labels) confined to `decodeRun`/`baseLabels`.
- **Access seam:** `ordersBeadStore(scope)` → `resolveOrderStore` /
  `resolveOrderStoreTarget` (per-order federation for rig/pool scope).
- **Census correction (2026-07-04):** the `order-run:`/`order:`/`order-tracking`/`seq:`
  label literals are a **DELIBERATE, drift-test-guarded triple-declaration** — private
  consts in `cmd/gc/order_dispatch.go` (canonical), `internal/orders/store.go` (codec), and
  `internal/coordclass/classify.go` (classification), kept in sync by the coordclass drift
  test to avoid import cycles. That is NOT a leak to centralize; leave it (touching it fights
  the layering + the guard).
- **The real leak = raw order-bead CRUD/query that bypasses `orders.Store`:**
  - `cmd/gc/order_dispatch.go:1454` raw `store.Update(id, {Labels:["order-run:"+scoped]})`
    (→ a Store outcome/label method); the `orders_feed.go:318/350/386` raw `ListQuery{Label:…}`
    (→ `RecentRuns`/a Store query); `huma_handlers_orders.go:420/484` label-built queries;
    `cmd_order.go:731` labels a **molecule wisp root** (rootID from `molecule.Instantiate`)
    with order labels — a cross-concern with molecule, NOT a `CreateRun`.
  - These are entangled with molecule/wisp-root labeling, the manual-vs-dispatcher paths,
    and `resolveOrderStoreTarget` federation — a control-plane refactor, byte-identity-gated.
- **Close work (own focused phase):** per-site, map each raw op to the `orders.Store` method
  that is its byte-identical replacement (the store.go docstrings already name these), preserving
  the wisp-root labeling + federation semantics; then guard raw order-bead ops outside `orders`.
- **Acceptance:** order-bead CRUD/query only through `orders.Store`; guard-enforced.
  **Size: M (control-plane; do as its own pass with fresh context, not a tail-end quick close).**

### 3. Mail — `mail.Provider` + `internal/mail/beadmail` — **✅ CLOSED (2026-07-04)**
- **Interface:** complete for user-facing ops; one ~60-line conversion edge
  (`createMessageBead`/`beadToMessage`). `beadmail` docstring already asserts the invariant
  "callers above this package never construct a message bead directly."
- **Access seam:** `mailBeadStore()` → `resolveMailMessagesStore`. Already sealed — both GC
  sweeps (`nudge_mail_sweep.go`, `wisp_gc.go`) take the typed `beads.MailStore` from the
  controller paths, so a mail relocation already captures them.
- **Census result:** the remaining leak was the **shape layer** — the sweepers constructed raw
  `beads.ListQuery{Type:"message", …}` in package main (violating the `beadmail` invariant), and
  the `mail.read` metadata key was defined in `cmd/gc/wisp_gc.go`.
- **Closed:** added `mail.ReadMetadataKey` (exported) + `beadmail.ReadMessagesBefore(store,
  before, limit)` and `beadmail.ReadMessageWispEntries(store)` encapsulating the two `Type:"message"`
  maintenance queries; routed all 3 sweeper sites (2 in nudge_mail_sweep live+dry-run, 1 in wisp_gc)
  through them; dropped the cmd/gc `mailReadMetadataKey`. Byte-identical (queries moved field-for-field;
  fable 2-lens review 0 findings; mail/beadmail/nudgequeue + sweep tests green).
- **Residual (not mail-bead access):** `b.Type=="message"` *classification* checks in
  `order_dispatch.go`/`doctor_backlog_depth.go` are read-only type routing (coordclass-adjacent),
  acceptable raw-by-design. Seal is structural (the `Type:"message"` literal lives in `beadmail`;
  no clean substring guard — the `"message"` needle is noisy).

### 4. Sessions — `session.Store` + `session.Info` + `session.CircuitState` — **mid-migration**
- **Interface:** exists; the **reconciler decision path is sealed** (Steps 1–6e,
  this branch — see `RECONCILER-FRONT-DOOR-LOCKSTEP-DROP.md`). Guards live in
  `frontdoor_di_guard_test.go` (`compute_awake_bridge.go`, `session_progress.go`,
  `session_circuit_breaker.go`, + the store-free/snapshot lists).
- **Access seam:** `sessionsBeadStore()` → `resolveSessionStore`; `worker.Handle`.
- **Leak shape (real delta), three buckets:**
  - **(a) raw-by-design, stays** — `InfoFromPersistedBead` (the codec), start
    execution (`buildPreparedStart` cracks `candidate.session.Metadata`, consumer
    #7), classifier oracle siblings, sleep-policy helpers, `newSessionBeadSnapshot`.
  - **(b) not-yet-migrated periphery** — CLI: `cmd_session.go`, `cmd_prime.go`,
    `cmd_nudge.go`, `session_resolve.go` (`store.Get(id)` → identity/state crack);
    decision-adjacent: `build_desired_state.go`, `city_runtime.go` iterating
    `snapshot.Open()`/`FindByID(` instead of `OpenInfos()`/`FindInfoBy*`; CLI:
    `cmd_start.go`, `cmd_wait.go`, `city_status_snapshot.go`; API:
    `internal/api/handler_sessions.go` + siblings; worker:
    `internal/worker/factory.go`, `handle_construct.go`, `invocation_telemetry.go`.
  - **(c) `ordered` physical deletion** — blocked on converting start execution
    (consumer #7); demoted, not deleted.
- **Close work:** convert periphery reads to `session.Info` accessors + `session.Store`
  writes, file-by-file, adding each to `snapshotInfoOnlyFiles` / `metadataInfoOnlyFiles`
  as it goes raw-free. Start with read-only status/CLI files (lowest blast radius).
- **Acceptance:** every non-raw-by-design session file guard-listed; the raw-by-design
  set is the documented census. **Size: L (partly done).**
- **Progress:** `city_status_snapshot.go` closed — its two raw session reads
  (`bead.Metadata["state"]`, `snapshot.Open()`) now go through
  `session.InfoFromPersistedBead(...).MetadataState` / `OpenInfos()` +
  `sessionMetadataStateInfo` / `IsSessionBeadOrRepairableInfo` (proven mirrors);
  added to both `snapshotInfoOnlyFiles` and `metadataInfoOnlyFiles`.

### 5. Convoy — *no typed interface* — **weak seal**
- **Interface:** functions over raw `beads.Store`; `ConvoyFields` unexported;
  `Type:"convoy"` checked in ~10+ files (`cmd/gc/cmd_convoy.go`,
  `internal/api/huma_handlers_convoys.go`, `internal/sling/sling_core.go` +
  `sling_attachment.go`, `internal/graphv2/invocation.go`, `internal/dispatch/drain.go`,
  `internal/api/handler_beads.go`/`handler_status.go`).
- **Close work:** introduce `convoy.Store` + exported `Convoy` projection FIRST,
  then refactor call-sites; add a `convoyBeadStore()` accessor to `class_store.go`.
- **Acceptance:** `Type:"convoy"` and convoy-bead construction confined to
  `internal/convoy`; guard-enforced. **Size: L.**

### 6. Formula / molecule / wisp — *no typed interface* — **largest**
- **Interface:** none. Molecule/step state is raw `beads.Bead.Metadata["gc.*"]`
  (`gc.step_ref`, `gc.outcome`, `gc.control_epoch`, `gc.attempt_log`, …) across
  ~70 files in `internal/dispatch`, `internal/molecule`, `internal/sling`,
  `internal/api`, `cmd/gc`. State-machine logic reads raw metadata directly.
- **Close work:** define `molecule.Workflow` (root projection) + `dispatch.Step`
  (step projection) typed views with a codec that hides the `gc.*` namespace;
  migrate readers/writers; the graph store seam (`graphBeadStore`) already exists.
- **Acceptance:** `gc.*` metadata keys read/written only through the projection;
  guard-enforced. **Size: XL (weeks) — do LAST.**

---

## Phased close order (recommendation)

1. **Nudge** (XS) → **Orders** (S) → **Mail** (S–M): finish the near-sealed classes;
   each ends with a guard entry. Fast wins that also exercise the
   `[beads.classes.<X>]` relocation end-to-end (the cheapest way to validate the
   backend seam actually captures a whole class).
2. **Sessions periphery** (L, incremental): file-by-file, read-only/CLI first, each
   added to the guard. Continues the reconciler front-door work already landed.
3. **Convoy** (L): introduce the interface, then refactor.
4. **Formula/molecule** (XL): define the projections, then migrate. Landmine — its
   metadata namespace encodes routing/dedup/retry semantics; byte-identity gate.

## Discipline (inherited from the reconciler front door)

- Verified per-file census before edits; convert → build/vet/lint/gofmt/tests →
  guard entry + **revert-canary** → fable adversarial review → commit + push
  `--no-verify`. Byte-identity is the gate. `[beads.classes.<X>]` relocation is the
  end-to-end acceptance test per class. Update this backlog as each class closes.
</content>
