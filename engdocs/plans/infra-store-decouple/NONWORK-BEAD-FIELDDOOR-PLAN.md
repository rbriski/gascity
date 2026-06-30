# Plan — eliminate direct field access on non-work beads (the session read surface)

Status: PLAN (no production code yet — for review/sign-off before implementation)
Branch: `upstream/object-front-doors-cleanup` (stacked on #3800; #3800 stays as-is)
Builds on: `OBJECT-MODEL-FRONT-DOOR-DESIGN.md` (§3.1 was the write half + the API/response read half; this is the **controller read half** it deferred as "the single largest read-side change").

## 1. The principle (now a hard rule)

It is **illegal** for any caller to read or write a **non-work** object's bead fields
(`b.Metadata[...]`, `b.Status`, `b.Title`, `b.Labels`, `b.CreatedAt`, …) directly.
Non-work objects = session, nudge, mail, order, graph. Only the generic **work**
bead may be read raw (it is the substrate + the HTTP/SSE wire contract). This is
the precondition for swapping the bead backend per class: if a backend changes how
an attribute is stored/serialized, exactly **one** codec must change, not N call sites.

#3800 closed the **write** leaks (typed front doors route every write) and the
**API-response read** leak (`session.Info`). What remains is the **controller read
surface**, dominated by the session-bead snapshot.

## 2. The audit (factual basis)

A 6-agent sweep inventoried every non-work raw read:

- **~256 raw-read occurrences** across ~167 sites (cmd/gc ≈ 240, internal/api ≈ 16),
  resolving to **27 distinct session-bead fields**.
- **21 of the 27 are already on `session.Info`** (the codec already projects them in
  `InfoFromPersistedBead`). The genuinely-missing cluster is small and cohesive:
  **identity/pool/named-session** — `configured_named_identity`, `common_name`,
  `pool_slot`, `pool_managed`, `session_origin`, the `agent:<name>` label fallback,
  plus a handful of state/bookkeeping keys (`last_woke_at`, `generation`,
  `dependency_only`, `pool_alias_conflict*`, `close_reason`/`closed_at`).
- The **leak surface is two-layered**:
  1. The snapshot `*sessionBeadSnapshot` leaks raw beads: `Open() []beads.Bead`,
     `FindByID`/`FindSessionBeadByTemplate`/`FindSessionBeadByNamedIdentity` all
     return `beads.Bead`; callers then read `.Metadata`. (Its **string-returning**
     `FindSessionNameBy*` methods are already clean typed answers.)
  2. **~15-20 classifier free-functions** take a `beads.Bead` and read its metadata:
     `isPoolManagedSessionBead`, `isNamedSessionBead`, `isManualSessionBead`,
     `isDrainedSessionBead`, `isFailedCreateSessionBead`, `sessionOrigin`,
     `resolvedSessionTemplate`/`normalizedSessionTemplate`, `sessionBeadAgentName`,
     `sessionBeadStoredTemplate`, `sessionBeadAssigneeIdentities`, `isPendingPoolCreate`,
     `beadOwnsPoolSessionName`, … These are the **encapsulation that already exists** —
     most of the ~167 raw reads funnel through them.

**Conclusion: this is NOT "bolt 20 fields onto a god object."** `session.Info` *is*
the session domain object; carrying session attributes is its job (it's the codec
edge). The real shape of the work is: finish the `Info` codec, and move the
classifier predicates from `(bead) → read metadata` to `(Info) → read field`.

## 3. Target layering

```
  bead substrate (work/session beads — beads.Bead)
        │  CODEC EDGE — the ONLY place a session bead is decoded
        ▼
  session.Info            ← InfoFromPersistedBead(bead) projects ALL consumed fields
        │  + domain predicates: Info.IsPoolManaged()/IsNamed()/Origin()/Template()/
        ▼                        AgentName()/IsDrained()/IsFailedCreate()/… (the
  sessionView / snapshot        classifiers, now reading Info fields not metadata)
        │  Open() []session.Info ; Find*(…) (session.Info, bool) ; FindName*(…) string
        ▼
  consumers (build_desired_state, city_runtime, session reconciler, cmd_wait, …)
        read info.Field / call info.IsX() — NEVER a raw bead
```

Four layers, one decode point. No `beads.Bead` of a session escapes the snapshot.

## 4. The fix, phased (each phase byte-identical, build+test-green, ≤~5 files where possible)

**P1 — Extend the `Info` codec (foundation).** Add the missing fields to
`session.Info` + `InfoFromPersistedBead` (identity/pool/named cluster first, then the
state/bookkeeping keys as consumers need them). Pure additive projection; the
wire stays byte-identical (`Info` already has additive internal-only fields absent
from response builders). Unit-test each new field's projection.

**P2 — Move the classifier predicates onto `Info`.** Convert the ~15-20 classifier
helpers from `func isX(b beads.Bead) …` to `func (i Info) IsX() …` (or `func isX(i
Info)`), reading the typed fields P1 added. Keep the bead-taking forms as thin
shims *temporarily* (`isX(b) { return InfoFromPersistedBead(b).IsX() }`) so callers
migrate incrementally. This is where the semantics consolidate.

**P3 — Re-type the snapshot.** `sessionBeadSnapshot` holds `[]session.Info`
(built via `InfoFromPersistedBead` at load — the codec edge moves into
`newSessionBeadSnapshot`). `Open() []Info`; the `Find*Bead` methods return
`(Info, bool)`; indexes key off `Info` fields. The `configured_named_identity`
linear scan becomes an `Info`-field scan (or a new index). `loadErr` stays.

**P4 — Convert the consumers.** Replace every `snapshot.Open()`-then-`.Metadata`
read and every direct session-bead field read with `info.Field` / `info.IsX()`.
Most are already routed through the P2 classifiers, so they convert for free once
the classifier takes `Info`; the residual direct reads convert site-by-site. Drop
the temporary bead-taking shims. This is the largest phase — shard by consumer file
(session_beads.go, session_reconciler.go, build_desired_state.go, city_runtime.go,
cmd_wait.go, …), compiler-driven, recording-fake/existing-suite as the oracle.

**P5 — The residual `.Store().Store` + cross-class sites** (the 6 from #3800):
- adoption/soft_reload snapshot reads → fall out of P3/P4 (use the typed snapshot).
- `closeBead` → split: `InfoStore.Close` (session, exists) + `workAssignment`
  release (exists) + `cancelStateAssignedToRetiredSessionBead` (extmsg, exists) —
  sequence close-then-release (the §5 mass-closure join point; preserve the
  skip-if-already-closed idempotence).
- `createPoolSessionBead` → thread `sessFront`; `CreateSession`/`CreateSpec` already
  exist and are used; the wrapper just stops unwrapping.

**P6 — Other non-work classes + guard.** The broad sweep found ~47 non-work raw
reads beyond session (audit residual — confirm nudge/mail/order). Close them through
their front doors. Then **tighten the arch guard** to also forbid `.Store().Store`
and direct `beads.Bead` session reads in the converted files, so the boundary is
compile-enforced and cannot regress.

## 5. Sequencing / risk

- P1–P2 are low-risk and unlock everything; land them first.
- P3–P4 are the controller-central change — shard, build-green per shard, lean on the
  reconciler/snapshot suites (byte-identical oracle). This is where the care goes.
- P5 `closeBead` split is the documented landmine — do it last, isolated, with the
  recording-fake proving the exact bead ops + ordering.
- Everything stays on `upstream/object-front-doors-cleanup`, stacked on #3800.

## 6. Open questions for sign-off

1. **Field set in `Info`**: OK to let `Info` carry the full consumed session-attribute
   set (~27 fields)? It's the domain object, so yes per the principle — confirming the
   "not a god object" framing is accepted.
2. **`Info` value vs pointer / mutation**: snapshot consumers today mutate
   `b.Metadata[...]` in place after reads (e.g. local snapshot bookkeeping). Converting
   to `Info` must decide value-copy semantics; flag any in-place-mutation consumers in P4.
3. **Scope of this PR**: land P1–P5 (session, the bulk) here; split P6 (other classes)
   into a follow-up, or include it?
