---
title: "Infra/beads decoupling — session handoff"
date: 2026-06-25
branch: plan/decouple-infra-beads (off fix/awake-readiness @ 204b66aee)
epic: ga-pd6tcg
---

> Where the bd→SQLite infra decoupling stands, what's left, and exactly how to
> finish it without breaking the system. Read alongside `DESIGN.md` (the hardened
> architecture) and `PLAN.md` (the 102-task list). `raw/` holds the audit trail
> (explore → design → adversarial-harden → reviews).

## TL;DR

Goal: bd/Dolt holds ONLY front-door work; sessions/mail/orders/nudges/graph move
behind fork-owned **domain-typed store seams** onto embedded SQLite, with a
dolt→sqlite migration command, and the `coordrouter` Router ultimately deleted.
Bar: **zero behavior change**, conformance-only validation, each step revertible.

**Mail is flip-ready end-to-end.** Nudges + orders have their seams extracted.
The remaining work is the orders cutover, the nudge/orders SQLite flips, then
sessions, then graph finalize + Router deletion, then extmsg.

## What's landed (15 commits since 204b66aee — all green, byte-identical)

Shared machinery:
- **`internal/beads/extras`** — versioned `{v;known;unknown}` codec (lossless
  metadata round-trip + drift guard). `internal/beads/codectest` — codec-fidelity
  conformance (full-bead invariance + mandatory projection), survives Router delete.
- **`config.BeadsConfig.Classes`** — `[beads.classes.<class>].backend = bd|sqlite`
  + `NormalizedClassBackend`/`ClassUsesSQLite`. Default `bd` = byte-identical.
- **`internal/storemigrate`** + **`gc beads migrate-sqlite [class...]`** —
  ID-preserving, idempotent dolt→sqlite copy per class (the user-requested command).
- **`internal/beads.SQLiteStore` store-edge events (P2)** — `WithSQLiteStoreRecorder`
  emits `RowChange{ID,Type,Op}` after commit, outside the lock; default no-op.
  `RowClosed` only on a true open→closed transition; no-op metadata re-stamps are
  suppressed (matches CachingStore, kills the heartbeat storm).
- **`cmd/gc/class_store.go`** — `openClassSQLiteStore` (per-class `<cityPath>/.gc/<class>/`,
  distinct prefix gcm/gcs/gco/gcn/gcg, retention off, shared handle) +
  `beadEventRowRecorder` (RowChange → `bead.created/updated/closed/deleted` on the
  bus, Actor `cache-reconcile`). Prefix-disjointness guard test.

Per domain:
- **Mail (flip-ready):** `beadmail.MailStore` seam (two-store split: messages +
  sessions); controller routes mail→SQLite when `messaging=sqlite`
  (`newCityMailProvider`/`resolveMailMessagesStore`), events wired, migration works.
  **Reviewed: flip-ready, parity confirmed** (bd path = CachingStore, which emits
  bead.deleted on Delete, so SQLite matches op-for-op).
- **Nudges (seam only):** `nudgequeue.NudgeStore` — 12 nudge-bead functions narrowed
  (waits.go + nudge_beads.go), byte-identical, characterized.
- **Orders (leaf seam only):** `orders.OrderStore` (minimal Get/Update/Close/CloseAll)
  — 4 leaf tracking-bead helpers narrowed. Gate + listCanonical stay `beads.Store`
  (they use `beads.HandlesFor().Live` + union the graph store cross-store).

## What remains (in dependency order)

1. **Orders cutover** (`ga-cpbq45`): narrow the tracking-bead CREATE sites
   (dispatchExec/dispatchWisp `store.Create`) onto OrderStore; wire the gate's
   `storesForGate` to include the graph store (cross-read — `hasOpenWorkInStoresStrict`
   already takes `[]beads.Store`); `resolveOrderStore` + SQLite wiring + flip.
2. **Nudges cutover** (`ga-c2rt46`): the **oracle-leak fix FIRST** — `type=chore` is
   NOT in `readyExcludeTypes` and `gc:nudge` NOT in `IsReadyExcludedBead`
   (`beads.go:177-248`); characterize current Ready behavior for nudge beads, then add
   the exclusion (+ a born-unrouted proof). Then `resolveNudgeStore` + SQLite wiring + flip.
3. **Sessions (P5, hardest):** `SessionStore` seam in `internal/session`; the row codec
   is the PERSISTED-FIELD subset only (`infoFromBead` at `manager.go:1543` is impure —
   keep enrichment in Manager); re-point ALL session-bead writers (`session_beads.go`,
   `session_wake.go` PreWakePatch) AND readers; SessionStore injected at the
   `internal/api` per-request `session_manager.go` seam (NOT cmd/gc); session_name
   uniqueness as a reconciler invariant (no bare UNIQUE — breaks duplicate-then-elect);
   live-adopt cutover (atomic copy of OPEN sessions, projection-equality verified).
4. **Graph finalize + Router deletion (P6):** route `registerGraphStoreBackend`
   through the recorder helper (graph emits no events today); rewire the
   `GET /v0/beads?type=molecule` augment + the order gate to GraphStore BEFORE deleting
   federation; MOVE `ClassifyGraphPlan` into a graphstore home; delete `coordrouter.Router`
   + demote `coordclass.Classify` via the three-mechanism zero-callers gate; delete
   `readyExcludeTypes` entries per relocated class.
5. **extmsg (P7):** own `ExtMsgStore` with per-family record types + HISTORY backfill
   (NOT clean-drain) + the `type=task`+`gc:extmsg-*` Ready-leak exclusion.
6. **Full wire (P3, owner decision: all classes):** typed Huma endpoints
   `GET /v0/sessions`, `/v0/orders/tracking`, `/v0/nudges`, `/v0/convoys` +
   openapi/dashboard regen. (Mail already wire-typed.)
7. **Convoy** seam (user-vs-synthetic split) and **CLI parity** for the remaining domains.
8. **Live soak** — there is NO running city here; mail's flip is unit/integration-verified
   but never exercised end-to-end against a live deployment. Soak each flip vs the
   `doctor_fork_rate` targets before deleting bd rows.

## Binding decisions (do NOT relitigate — see memory `infra-beads-decoupling-plan`)
1. Full domain types to the wire (every class). 2. Full arc to SQLite. 3. Delete the
Router (design from first principles). 4. extmsg deferred (after mail). 5.
Controller-mediated infra writes (relocated store opened in the long-lived controller;
CLI writes route through it; reads per-process WAL). 6. Conformance-only validation
(no dual-write/shadow-read; revert = quiesce-then-flip-back at a maintenance window).

## Four flip-safety assumptions (check EACH class before flipping it live)
From the P2 events review (`raw/p2-review.json`): a relocated class is safe to flip
only if (a) it does no `status=closed` writes that aren't true transitions
(RowClosed handles this), (b) no metadata re-stamping the no-op guard doesn't cover,
(c) it self-GCs (retention stays disabled while a recorder is attached — purgeTerminal
would emit a bead.deleted storm), (d) a single controller-owned writer.

## Gotchas (will bite you)
- **Pre-commit hook**: `core.hooksPath` points at the MAIN checkout's stale hook
  (`git add docs/schema/openapi.json` — old path). Commits staging `.go` files from
  this worktree ABORT. Run the gate manually (gofmt/vet/`go build ./...`/tests/
  `go run ./cmd/genspec` no-drift) and commit with **`--no-verify`**. Filed: hook bug.
- **MemStore does NOT preserve caller-pinned IDs; SQLiteStore DOES** (`autoID := b.ID==""`).
  Tests asserting specific IDs must use a SQLite store or assert via List/count.
- The order single-flight gate uses `beads.HandlesFor(store)` → must stay `beads.Store`.
- `genschema` regenerates `docs/reference/schema/*` on config changes — commit them.
- cmd/gc full test suite ~60-70s; it's the real guard for order_dispatch changes.

## How to verify
```
go build ./...
go vet ./internal/... ./cmd/gc/
go test ./internal/beads/... ./internal/coordrouter/... ./internal/mail/... \
        ./internal/nudgequeue/ ./internal/orders/ ./internal/storemigrate/ ./internal/config/
go test ./cmd/gc/            # ~60s; covers order dispatch, nudge, mail wiring
```
Each domain flip is gated on its conformance passing against BOTH the bd and SQLite
backends (the codectest + the per-domain Provider/conformance suites).
