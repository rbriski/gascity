---
title: "Infra/beads decoupling — execution plan (work list)"
status: Ready-to-file
date: 2026-06-24
design: ./DESIGN.md
branch: plan/decouple-infra-beads
---

> The dependency-ordered, bd-fileable work list for the hardened design in
> [`DESIGN.md`](./DESIGN.md). Produced by an 8-phase decomposition + topological
> synthesis. **102 tasks, 8 phases.** Every task is tests-first, ≤5 files, with
> explicit acceptance criteria + dependencies. All `file:line` references are
> pinned to branch HEAD `204b66aee` — re-verify at execution time.

## How to use this plan

1. Each task becomes one `bd` issue (title = task title; body = intent + files +
   test-first + acceptance; deps = `dependsOn`). The work list is real front-door
   work, so `bd` is its home.
2. **TDD is mandatory** — write the named failing test first. Under *conformance-only
   validation*, the per-class conformance subtests (a–i in DESIGN §7) are the ONLY
   correctness oracle; treat them as load-bearing.
3. Respect the **four hard gates** and the **points of no return** below.
4. P0 is done; P1/P3/P6 are GO-with-corrections; **P2/P4/P5 are NO-GO** until their
   corrections (folded into the tasks) land. P3.5 is a prerequisite of any physical move.

## The four hard gates

- **P1-T1 + P1-T2** (versioned-extras codec + conformance harness) gate every P1 adapter.
- **P1-T15** (P1 byte-identical full-suite join) gates entry into the cutover phases.
- **P3.5-T13** (grep-guard) closes the cross-store foundation.
- **P4-T1** is the **NO-GO gate**: requires P1 + P2 + P3 + P3.5 all landed before any physical move.

## Phase map

| Phase | Theme | Status |
|---|---|---|
| P0 | graph already on SQLite (proof-of-pattern) | DONE |
| P1 | domain-typed interfaces + bd-delegating adapters (byte-identical) | GO w/ corrections |
| P2 | greenfield Layer-0 event emission + autoclose re-home | NO-GO until corrections |
| P3 | full typed wire + dashboard regen (every class) | GO w/ corrections |
| P3.5 | cross-store foundation (resolver, write API, retention, reader/writer inventory) | prerequisite of any move |
| P4 | SQLite cutover: mail → orders → nudges | NO-GO until P1/P2/P3/P3.5 |
| P5 | SQLite cutover: sessions (live-adopt) | NO-GO until corrections |
| P6 | graph finalize + Router deletion | GO w/ corrections (final PONR) |
| P7 | extmsg follow-on (history backfill) | deferred after mail |

## Executive summary

The 98 tasks across 8 phases form a strangler-style arc: build fork-owned domain-typed store interfaces (P1), greenfield Layer-0 event emission (P2), full typed wire (P3), the cross-store foundation (P3.5), then physically relocate each infra class to SQLite (P4 mail/orders/nudges, P5 sessions) and finally delete the Router and finalize the graph class (P6), with extmsg as a deferred follow-on epic (P7). Four hard gates structure execution: P1-T1+P1-T2 (the versioned-extras codec and conformance harness) gate every P1 adapter and are the safety net under conformance-only validation; P1-T15 is the P1 byte-identical join into P4; P3.5-T13 closes the cross-store foundation; and P4-T1 is the NO-GO gate that requires P1+P2+P3+P3.5 all landed before any physical move. The critical path runs P1 codec/harness/session-store -> P1 join -> P4 config/scaffold/order-impl/gate -> serialized mail->orders->nudges cutovers -> P5 sessions (live-adopt is the heaviest single irreversible step). The first batch is wide (14 independent entry points across all phases) because P2, P3-T0, P3.5 foundations, P6-graphstore creation, and the P4/P7 oracle-leak fixes have no upstream phase dependency. The physical cutovers (P4-T6/T10/T15, P5-T13) and the Router deletion (P6-T10) are points of no return and must serialize through the controller wiring with stabilization windows for fork-rate attribution; everything before them is a byte-identical refactor on one bd backend. Read the plan as: swarm the foundations and per-class interface tracks in parallel, converge at each phase join gate, then walk the irreversible cutovers one class at a time with conformance suites (subtests a-i) Skip:false as the only oracle. The two cross-phase seams to watch are P2-T8 adopting the P3.5-T2 resolver after a stub, and the order-gate graph read path being refined twice (P4-T9 then P6-T8) on the same call site."

## Critical path

`P1-T1 → P1-T2 → P1-T3 → P1-T4 → P1-T5 → P1-T6 → P1-T15 → P4-T1 → P4-T2 → P4-T8 → P4-T9 → P4-T10 → P4-T15 → P5-T8 → P5-T11 → P5-T12 → P5-T13`

## Points of no return

- P4-T6 (mail cutover: first bd-row drain-then-switch; new mail rows only exist in SQLite — revert is quiesce-then-flip-back at a maintenance window)
- P4-T7 / P4-T11 / P6-T12 (readyExcludeTypes deletions — each is the deletion-is-the-proof step; reverting requires re-adding the exclusion and is only safe while beads of that class can still land in the work store)
- P4-T10 (orders cutover) and P4-T15 (nudge cutover: flock-source vs SQLite-shadow drift window — crash mid-flip can strand the shadow)
- P5-T11 (sessions live-adopt atomic copy — the open-session metadata long-tail cannot be reconstructed by crash-adoption; a missed key or non-atomic copy permanently loses state)
- P5-T13 (sessions cutover + session/gc:session exclusion deletion)
- P6-T10 (coordrouter.Router deletion — removes the federation safety net, retroactively making earlier per-class reverts unsafe; the FINAL point of no return)
- P7-T12 / P7-T13 (extmsg history backfill + cutover — bd-row deletion after backfill is irreversible; history-tier so not clean-drain)

## First batch — startable immediately (no open deps)

`P1-T1`, `P2-T1`, `P2-T4`, `P2-T7`, `P3-T0`, `P3.5-T1`, `P3.5-T4`, `P3.5-T8`, `P3.5-T10`, `P3.5-T11`, `P4-T13`, `P6-T1`, `P7-T1`, `P7-T2`

## Suggested execution waves

> Waves are a swarm-scheduling aid; the authoritative constraint is each task's own `dependsOn`. **Caveat:** the synthesis placed some non-destructive P6 prep tasks early to de-risk, but the *destructive* P6 steps (Router deletion P6-T10, graph exclusion deletions) are the FINAL points of no return and run after P5 regardless of wave label — see Known ordering caveats.

- **B0-foundations**: P1-T1, P2-T1, P2-T4, P2-T7, P3-T0, P3.5-T1, P3.5-T4, P3.5-T8, P3.5-T10, P3.5-T11, P4-T13, P6-T1, P7-T1, P7-T2
- **B1-harness+resolver**: P1-T2, P2-T2, P2-T5, P3.5-T2, P3.5-T9, P6-T2, P6-T3, P7-T3, P7-T8
- **B2-P1-classes-swarm+P2+P3-entries**: P1-T3, P1-T9, P1-T10, P1-T13, P1-T14, P2-T3, P2-T6, P2-T8, P2-T10, P3-T2, P3-T4, P3-T6, P3-T8, P3.5-T3, P3.5-T5, P3.5-T6, P3.5-T7, P6-T9, P7-T5
- **B3-P1-tracks+P3-endpoints**: P1-T4, P1-T11, P3-T3, P3-T5, P3-T7, P6-T4, P6-T5, P6-T7, P6-T8, P7-T4
- **B4-P1-sessions+orders+P3-wire+resolverconsumers**: P1-T5, P1-T7, P1-T8, P1-T12, P3-T1, P3-T9, P3.5-T12, P6-T6, P7-T6
- **B5-P1-join-prep+dashboard+grep**: P1-T6, P3-T10, P3.5-T13, P6-T10, P7-T7
- **B6-P1-join+graph-finalize**: P1-T15, P6-T11, P6-T12, P7-T9
- **B7-P4-config+scaffold**: P4-T1, P7-T10
- **B8-P4-impls+revert**: P4-T2, P7-T11
- **B9-P4-class-impls**: P4-T3, P4-T4, P4-T8, P4-T14, P7-T12
- **B10-P4-conformance+gate**: P4-T5, P4-T9
- **B11-mail-cutover**: P4-T6
- **B12-orders-cutover+mail-delete+extmsg-cut**: P4-T10, P4-T7, P7-T13
- **B13-nudge-cutover+orders-delete**: P4-T15, P4-T11
- **B14-P4-soak+P5-foundation**: P4-T16, P5-T1, P5-T2
- **B15-P5-store**: P5-T3
- **B16-P5-repoint-swarm**: P5-T4, P5-T5, P5-T7, P5-T9
- **B17-P5-writers+events+wait**: P5-T6, P5-T8, P5-T10
- **B18-P5-liveadopt**: P5-T11
- **B19-P5-conformance**: P5-T12
- **B20-P5-cutover**: P5-T13

## Known ordering caveats

- P3.5-T4 (controller write API contract) is listed with no upstream dependency because it is contract-first (defines new Huma write endpoints), but the plan flags the exact endpoint set must be confirmed against the P3 typed-wire endpoints landing in parallel; if the write endpoints reuse P3 DTOs, add a soft dep P3.5-T4 -> P3-T3/T5. Treated as independent for ordering.
- P2-T8 (cross-store autoclose conformance) is authored against a two-MemStore federation STUB and only adopts the real P3.5-T2 (storeRef,id) resolver once it lands. It is therefore orderable immediately after P2-T7, but its assertions are not final until P3.5-T2 exists. Flagged as a deliberate stub-then-adopt seam, not a cycle.
- P4-T9 reproduces hasOpenWorkStrict over OrderStore AND reads graph wisp roots via storesForGate; in P4 this still uses the bd graph read path. P6-T8 later re-points that same gate graph leg to the real GraphStore. The two are sequential refinements of the same call site across phases (P4 first, P6 second) — not a duplicate; verify the P4 graph read path stays valid until P6-T8 lands.
- P5-T2 depends on the P1 domain-typed session.SessionStore interface (P1-T4). P5 phaseNotes flag that the interface may not yet exist if P1 only shipped the coordrouter.SessionsStore subset seam; if so P1-T4 must declare the domain-typed interface before P5-T3. Treated here as P1-T4 satisfying it (P1-T4 explicitly declares the domain-typed SessionStore).
- P7-T7/P7-T8/P7-T13 depend on P3.5 infra (resolver, write API, retention, storehealth *.sqlite). Because P7 is gated after mail (P4-T6), all P3.5 tasks are already landed by then, so no cross-phase cycle; the dependency is satisfied transitively via the P4 gate. P7-T13 carries an explicit dep on P4-T6 to enforce 'mail proves the pattern first'.
- P6-T1/T2/T3 (graphstore creation) have no hard dependency on P1-P5 and could in principle start in the first batch; they are scheduled early (B0-B1) to de-risk the P6 critical section, but the destructive P6-T10 Router deletion is gated behind P3.5-T2 (via P6-T6) and all graph-read re-points, so it cannot run until P3.5 lands.
- No cycles detected across the 8 phases. The only back-references (P2-T8->P3.5-T2 stub adoption, P4-T9 vs P6-T8 same-callsite refinement) are forward-only refinements, not circular.

## Task catalog

### P1 — Domain-typed interfaces + bd-delegating adapters

> Introduce fork-owned, domain-typed Store interfaces IN each owning package (session.SessionStore + WAIT, mail.MailStore, orders.OrderStore over a NEW orders.OrderTracking, nudgequeue.NudgeStore, convoy.ConvoyStore), each with a bd-delegating adapter that translates beads.Bead↔domain ONLY at the storage-row edge via a per-entity versioned typed-extras codec ({v; known; unknown}). Sessions keep infoFromBead impure in Manager — only the pure persisted-field metadata-decode subset moves to the row edge; ProjectLifecycle is untouched and pinned by a projection-invariance subtest. Each subsystem is migrated behind its interface via constructor injection (SessionStore threaded at internal/api/session_manager.go; OrderStore consumed at the order_dispatch decide+gate / recency-state site). Per-class conformance suites — reconstruct-union contract, golden round-trip incl. unknown passthrough, projection-invariance — are written FIRST. The racy cross-controller idempotency gate (session canonical-election / ensureSessionNameAvailable*, order hasOpenWorkStrict) is carried VERBATIM behind a Skip+Reason escalation bead; no UNIQUE drop-ins. End state is byte-identical: one physical bd backend, coordclass/coordrouter untouched, no physical move.

#### P1-T1 — Add versioned typed-extras codec primitive with unknown-key passthrough  _(size M)_
- **Intent:** Every bd-delegating row codec needs the {v int; known typedStruct; unknown map[string]string} envelope so the bd-delegating phase round-trips every metadata key losslessly (DESIGN §4 modeling rule). This is the shared building block all six adapters reuse; building it once keeps the reconstruct-union contract identical across classes.
- **Files:** internal/beads/extras/extras.go, internal/beads/extras/extras_test.go
- **Test-first:** extras_test.go: TestExtrasRoundTripPreservesUnknownKeys — encode a known typed struct plus a set of unenumerated metadata keys, decode, and assert the reconstructed metadata map equals the original key-for-key; TestExtrasForbidsKeyInBothKnownAndUnknown asserts encode errors (or panics in test) if any promoted/known key also appears in unknown (double-write guard).
- **Acceptance:**
  - extras package compiles with no dependency on internal/api, internal/session, or any Layer-1 package (it is a Layer-0/substrate helper)
  - Encode/Decode are pure functions (no I/O, no time, no Manager receiver); unknown map carries every metadata key the caller's known struct did not enumerate
  - No map[string]any anywhere; the blob is map[string]string for unknown plus a typed struct for known
  - go vet ./internal/beads/extras/... clean; go test ./internal/beads/extras/... green
- **Depends on:** —

#### P1-T2 — Write reconstruct-union + golden round-trip + projection-invariance conformance subtests FIRST  _(size M)_
- **Intent:** Conformance is the ONLY oracle under conformance-only validation (DESIGN §7). Extend the existing coordtest harness with the load-bearing subtests (a) golden round-trip incl. unknown passthrough, (b) reconstruct-union key-for-key equality, (e) projection-invariance over the persisted subset — written before any adapter so each adapter is built against a failing suite (TDD).
- **Files:** internal/coordrouter/coordtest/conformance.go, internal/coordrouter/coordtest/conformance_test.go
- **Test-first:** conformance_test.go drives the new subtests against a MemStore with Skip:false: GoldenRoundTrip asserts domainToRow(rowToDomain(b)).Metadata == b.Metadata key-for-key including injected unknown keys; ReconstructUnion asserts row→domain unions promoted-columns + extras.known + extras.unknown into one metadata map equal to original and forbids any key in BOTH known and unknown; ProjectionInvariance asserts the chosen pure projection (e.g. ProjectLifecycle for sessions) yields identical output before vs after the codec round-trip over the persisted subset.
- **Acceptance:**
  - RunClassedStoreTests gains GoldenRoundTrip, ReconstructUnion, ProjectionInvariance subtests parameterized by a per-class codec + projection closure
  - coordtest's own _test.go runs the new subtests with Skip:false against MemStore and they pass (harness non-vacuous, matching the existing P0 pattern)
  - Subtests accept a Skip+Reason via the existing Options struct so a class not yet wired stays documented-skipped, never silently absent
  - go test ./internal/coordrouter/coordtest/... green; TestEveryKnownEventTypeHasRegisteredPayload unaffected (no event work in P1)
- **Depends on:** P1-T1

#### P1-T3 — Extract the pure session persisted-field row codec (no runtime enrichment)  _(size M)_
- **Intent:** DESIGN §4 correction: the session row edge is a PERSISTED-FIELD CODEC ONLY. Split the pure metadata-decode subset out of infoFromBead (manager.go:1543) — the part that reads b.Metadata into Info fields — leaving transportForBead/routeACPIfNeeded/IsRunning/IsAttached/GetLastActivity enrichment in the impure *Manager method. Add the encode side mirroring the session-bead create/SetMetadata writes.
- **Files:** internal/session/row_codec.go, internal/session/row_codec_test.go
- **Test-first:** row_codec_test.go: TestSessionRowCodecRoundTrip builds a session bead with the full persisted metadata key set (template, alias, agent_name, provider, transport, command, work_dir, session_name, session_key, resume_flag, resume_style, resume_command, state, last_nudge_delivered_at, plus unenumerated keys) and asserts decode→encode preserves every key via the T1 extras envelope; TestSessionRowCodecIsPure asserts the decode function has no *Manager receiver and does not call any runtime.Provider method.
- **Acceptance:**
  - A pure func decodes a beads.Bead into the persisted subset of session.Info (no Attached/LastActive runtime enrichment, no transport resolution) and an encode func produces the metadata map written today
  - infoFromBead in manager.go is refactored to CALL the pure decode then apply runtime enrichment, with byte-identical Info output for the same inputs (characterization test over existing fixtures stays green)
  - Unknown/unenumerated metadata keys survive the round-trip via the T1 extras blob
  - go test ./internal/session/... green; ProjectLifecycle (lifecycle_projection.go) is not modified
- **Depends on:** P1-T1, P1-T2

#### P1-T4 — Declare session.SessionStore interface + bd-delegating adapter in internal/session  _(size M)_
- **Intent:** DESIGN §3 Layer-1: the domain-typed store interface lives IN the owning package. Declare SessionStore (the surface manager.go + waits.go actually use: Create/Get/List/ListByLabel/ListByMetadata/Update/SetMetadata/SetMetadataBatch/Close/CloseAll/Reopen) and a bd adapter that wraps beads.Store and translates at the row edge via the T3 codec. First impl is byte-identical delegation.
- **Files:** internal/session/store.go, internal/session/store_bd.go, internal/session/store_bd_test.go
- **Test-first:** store_bd_test.go runs coordtest.RunClassedStoreTestsWithOptions for ClassSessions with Skip:false against the bd adapter over a MemStore, asserting GoldenRoundTrip/ReconstructUnion/ProjectionInvariance (ProjectLifecycle as the projection) all pass.
- **Acceptance:**
  - SessionStore is a domain-typed interface in internal/session (NOT the coordrouter.SessionsStore P0 subset alias); the bd adapter satisfies it and delegates to beads.Store with row-edge translation
  - The bd adapter passes the full T2 conformance suite for ClassSessions
  - Compile-time assertion var _ SessionStore = (*bdSessionStore)(nil); no behavior change vs raw beads.Store path (characterization)
  - go vet ./internal/session/... clean; go test ./internal/session/... green
- **Depends on:** P1-T3

#### P1-T5 — Inject SessionStore into session.Manager (replace the raw beads.Store field)  _(size L)_
- **Intent:** Constructor injection per DESIGN §10 P1 row: Manager.store (beads.Store, ~50 call sites in manager.go) becomes a SessionStore. Each NewManager* constructor accepts/wraps a SessionStore. Pure mechanical field-type swap; one file dominates so it is irreducibly one logical edit.
- **Files:** internal/session/manager.go, internal/session/store.go
- **Test-first:** manager characterization test (existing session manager tests) re-run unchanged: assert create/list/close/lifecycle behavior is byte-identical with the Manager backed by the bdSessionStore wrapper vs the prior raw beads.Store; add TestManagerUsesSessionStore asserting NewManager* accepts a SessionStore-satisfying value.
- **Acceptance:**
  - Manager.store field type is SessionStore; all m.store.* call sites compile against the interface (extend SessionStore only if a real call site needs a method, never speculatively — per the §3 no-speculative-methods rule)
  - All NewManager* constructors thread SessionStore; a bd-backed value is the default first impl so existing callers are unchanged in behavior
  - internal/session existing test suite green (byte-identical behavior)
  - go vet ./internal/session/... clean
- **Depends on:** P1-T4

#### P1-T6 — Thread SessionStore at internal/api/session_manager.go (NOT api_state.go)  _(size S)_
- **Intent:** DESIGN §3 + §10: SessionStore is injected at the per-request session.Manager construction in internal/api/session_manager.go's sessionManager(store), never at cmd/gc/api_state.go, and stays below the worker boundary. This keeps TestGCNonTestFilesStayOnWorkerBoundary green.
- **Files:** internal/api/session_manager.go
- **Test-first:** Add/extend an api test asserting sessionManager(store) constructs a Manager backed by a SessionStore derived from the passed beads.Store; rely on TestGCNonTestFilesStayOnWorkerBoundary as the guard that no cmd/gc non-test file constructs SessionStore directly.
- **Acceptance:**
  - sessionManager(store) wraps the incoming beads.Store in the bd SessionStore adapter and passes it to NewManagerWith*; all 9 consumer call sites (huma_handlers_sessions_*, handler_session_create.go, session_resolution.go) compile unchanged
  - TestGCNonTestFilesStayOnWorkerBoundary green (SessionStore injection does not introduce a forbidden session.NewManager/sessionlog import in cmd/gc non-test files)
  - TestOpenAPISpecInSync unaffected (no wire change in P1); make dashboard-check green
  - go test ./internal/api/... green
- **Depends on:** P1-T5

#### P1-T7 — Fold the WAIT sub-entity (durable session waits) into SessionStore surface  _(size M)_
- **Intent:** DESIGN §4 WAIT row: durable session waits (type=gate + gc:wait, waits.go) ride in SessionStore. Declare the wait surface (ListSessionWaitBeads/CancelWaitsAndCollectNudgeIDs/ReassignWaits/WakeSession already take beads.Store) on SessionStore so the close→WithdrawWaitNudges cross-store bridge inherits the typed store. In P1 sessions+nudges share one bd backend, so the bridge is a verbatim no-op move.
- **Files:** internal/session/waits.go, internal/session/store.go, internal/session/waits_test.go
- **Test-first:** waits_test.go: TestWaitClosePassesNudgeStore asserts the session-close path (CancelWaitsAndCollectNudgeIDs) terminalizes wait beads and returns nudge IDs via the SessionStore, and that a separate nudge-store handle (same bd backend in P1) can be passed for withdrawal — pinning the future cross-store boundary now.
- **Acceptance:**
  - waits.go helpers accept SessionStore (or its wait subset) instead of raw beads.Store where they operate on session/wait beads; behavior byte-identical
  - The conformance suite gains a wait subtest: session close terminalizes wait-linked beads; the nudge-withdrawal target is an explicitly-passed store handle (proves the close path can cross to a separate nudge store in P4/P5)
  - ReassignWaits / WakeSession / CancelWaits behavior unchanged (existing waits_test.go green)
  - go test ./internal/session/... green
- **Depends on:** P1-T4

#### P1-T8 — Carry the racy session canonical-election idempotency gate verbatim behind Skip+Reason  _(size S)_
- **Intent:** DESIGN §7 Step 1 + §0.6: the bd phase does NOT close the cross-controller race. The session_name canonical-election / ensureSessionNameAvailable* + withSessionMutationLock logic (names.go:316-324, manager.go) is kept verbatim; the conformance subtest that would assert true single-flight uniqueness is gated behind beadstest/coordtest Options{Skip,Reason} naming an escalation bead. NO UNIQUE(session_name) drop-in.
- **Files:** internal/session/store_bd_test.go, internal/session/names.go
- **Test-first:** store_bd_test.go: add an IdempotencyUnderConcurrentCreate subtest gated with Skip:true and Reason naming the escalation bead (cross-controller race not closed in bd phase); a non-skipped sibling asserts the single-controller ensureSessionNameAvailable path still rejects a duplicate open name (verbatim behavior preserved).
- **Acceptance:**
  - The cross-controller single-flight subtest is present but Skip:true with a Reason citing the escalation bead and DESIGN §7
  - ensureSessionNameAvailable* and withSessionMutationLock are unchanged (no UNIQUE constraint, no new election logic) — grep-proven no diff to the election bodies
  - The single-controller duplicate-name rejection test passes (verbatim behavior intact)
  - go test ./internal/session/... green
- **Depends on:** P1-T4

#### P1-T9 — Declare mail.MailStore + bd adapter; model Rig and collapse the read double-encode  _(size M)_
- **Intent:** DESIGN §4 mail row: mail.Message is the one place the domain type IS the wire type. Declare MailStore in internal/mail; the beadmail adapter is the bd impl. Correct two unmodeled facts: model mail.Message.Rig (mail.go:54, dropped today in beadToMessage) and collapse the read label/metadata double-encode (beadmail.go:896-902) into a single canonical source at the row edge.
- **Files:** internal/mail/mail.go, internal/mail/beadmail/beadmail.go, internal/mail/beadmail/conformance_test.go, internal/mail/mailtest/conformance.go
- **Test-first:** Extend mailtest/conformance.go (or beadmail conformance_test.go) with GoldenRoundTrip incl. Rig and unknown passthrough, and a ReadFlagSingleSource subtest asserting beadToMessage derives Read from exactly one canonical source (no label/metadata divergence); assert Message.Rig survives a Send→Get round-trip.
- **Acceptance:**
  - MailStore interface declared in internal/mail; beadmail satisfies it (mail.Provider is the service ABOVE this seam, unchanged)
  - beadToMessage populates Message.Rig; the read flag has a single canonical encode/decode (the label/metadata double-write is collapsed) with a conformance subtest pinning it
  - beadmail passes the T2 conformance suite for ClassMessaging incl. golden round-trip; existing mailtest suite green
  - go vet ./internal/mail/... clean; go test ./internal/mail/... green
- **Depends on:** P1-T1, P1-T2

#### P1-T10 — Introduce orders.OrderTracking domain struct + row codec  _(size M)_
- **Intent:** DESIGN §4 orders row: a NEW orders.OrderTracking struct (NOT orders.Order, which is the TOML def) maps the order-tracking / order-run:<scoped> / NoHistory bead currently read as raw beads.Bead in order_dispatch.go (orderTrackingSummary at :309). Add the struct + a pure row codec via T1 extras.
- **Files:** internal/orders/tracking.go, internal/orders/tracking_test.go
- **Test-first:** tracking_test.go: TestOrderTrackingRowCodecRoundTrip builds an order-tracking bead (labels order-run:<scoped> + order-tracking, NoHistory tier, outcome/cursor/gc.routed_to metadata) and asserts decode→encode preserves every key incl. unenumerated ones via the T1 envelope; assert OrderTracking carries scoped name, run labels, outcome, cursor, routed_to.
- **Acceptance:**
  - orders.OrderTracking is a new struct distinct from orders.Order; a pure codec maps it to/from a beads.Bead with unknown passthrough
  - The codec round-trips the order-run:<scoped> + order-tracking label set and the outcome/cursor metadata used by historyEntriesForStore/lastRunForStore
  - No UNIQUE(scoped_name, run_key) modeled (run_key does not exist; single-flight stays the predicate) — codec adds no uniqueness
  - go test ./internal/orders/... green; go vet clean
- **Depends on:** P1-T1, P1-T2

#### P1-T11 — Declare orders.OrderStore interface + bd-delegating adapter  _(size M)_
- **Intent:** DESIGN §3 Layer-1: OrderStore in internal/orders over OrderTracking. Surface = what the dispatch gate + recency + retention perform today (Create/Get/List/ListByLabel/Update/Close/CloseAll/DepList/DepRemove/Delete). bd adapter delegates to beads.Store, translating at the OrderTracking row edge.
- **Files:** internal/orders/store.go, internal/orders/store_bd.go, internal/orders/store_bd_test.go
- **Test-first:** store_bd_test.go runs coordtest.RunClassedStoreTestsWithOptions for ClassOrders with Skip:false against the bd adapter, asserting GoldenRoundTrip/ReconstructUnion pass; add a recency subtest asserting List by order-run:<scoped> returns the most-recent tracking in created-desc order (parity with lastRunForStore).
- **Acceptance:**
  - OrderStore interface declared in internal/orders; bd adapter satisfies it with row-edge OrderTracking translation; compile-time var _ OrderStore assertion present
  - bd adapter passes the T2 conformance suite for ClassOrders incl. golden round-trip
  - Interface surface grows only to methods a real order_dispatch call site uses (no speculative FindOrCreateByKey/SweepStale in P1)
  - go vet ./internal/orders/... clean; go test ./internal/orders/... green
- **Depends on:** P1-T10

#### P1-T12 — Consume OrderStore at the order_dispatch decide+gate / recency-state site  _(size M)_
- **Intent:** DESIGN §10 P1: decide the OrderStore consumption site. order_dispatch.go threads beads.Store PER-TARGET (storesForGate, dispatch(store)), not a dispatcher field — so OrderStore is derived at the gate boundary: the decide path hasOpenWorkStrict (:1468) / hasOpenWorkInStoresStrict (:1750) and the state path historyEntriesForStore (:844) / lastRunForStore (:830). Wrap each per-target beads.Store in the bd OrderStore adapter there. The graph read path for wisp roots stays beads.Store (graph relocation is P6).
- **Files:** cmd/gc/order_dispatch.go, cmd/gc/order_dispatch_test.go
- **Test-first:** order_dispatch_test.go: TestGateUsesOrderStore asserts hasOpenWorkStrict and the recency/history reads route through an orders.OrderStore-derived view with byte-identical single-flight + last-run results vs the prior raw beads.Store path over the same fixtures (open tracking, orphan all-closed root, open-descendant wisp cases preserved verbatim).
- **Acceptance:**
  - The single-flight gate (decide) and recency/history (state) reads consume orders.OrderStore derived from each per-target beads.Store; the wisp-root open-descendant check that crosses into graph stays on beads.Store (deferred to P6)
  - hasOpenWorkStrict semantics byte-identical: open tracking → true, orphan all-closed root → false, open-descendant wisp → true (existing comments/cases preserved)
  - No new dispatcher struct field invented; injection is at the gate boundary as documented in the task intent
  - go test ./cmd/gc/... order dispatch tests green; go vet clean
- **Depends on:** P1-T11

#### P1-T13 — Declare nudgequeue.NudgeStore interface + bd adapter over the shadow bead  _(size M)_
- **Intent:** DESIGN §4 nudges row: NudgeStore maps the type=chore + gc:nudge shadow bead (the flock file stays source of truth; the table is its shadow). Declare NudgeStore in internal/nudgequeue (surface = ensure/terminalize/sweep: Create/Get/List/SetMetadata/SetMetadataBatch/Close) and a bd adapter; re-point waits.go terminalization helpers (markTerminal*, terminalNudgeBeads) to it. The oracle-leak fix is explicitly DEFERRED to P4 (DESIGN §9.10) — do NOT add the readyExclude here.
- **Files:** internal/nudgequeue/store.go, internal/nudgequeue/store_bd.go, internal/nudgequeue/waits.go, internal/nudgequeue/store_bd_test.go
- **Test-first:** store_bd_test.go runs coordtest.RunClassedStoreTestsWithOptions for ClassNudges with Skip:false against the bd adapter, asserting GoldenRoundTrip/ReconstructUnion (incl. nudge_id, terminal_reason, state, nudge:<id> label) pass; a terminalize subtest asserts markTerminalBeadByID via NudgeStore matches prior beads.Store behavior.
- **Acceptance:**
  - NudgeStore interface in internal/nudgequeue; bd adapter satisfies it; waits.go markTerminal*/terminalNudgeBeads route through NudgeStore with byte-identical behavior
  - bd adapter passes the T2 conformance suite for ClassNudges incl. golden round-trip with the nudge:<id> label and terminal metadata
  - Nudge oracle-leak fix (type=chore+gc:nudge readyExclude) is NOT added in P1 — a comment/skip cites it as P4 §9.10 scope
  - go vet ./internal/nudgequeue/... clean; go test ./internal/nudgequeue/... green
- **Depends on:** P1-T1, P1-T2

#### P1-T14 — Declare convoy.ConvoyStore interface + bd adapter (split is a static call-site fact)  _(size M)_
- **Intent:** DESIGN §4 convoy row: ConvoyStore in internal/convoy. User convoys = ClassWork (bd, beads.Bead wire); synthetic (gc.synthetic) travel with the graph. The split is the CALLER's choice of which ConvoyStore to construct — no runtime sniff. Declare the interface over what convoy.go/membership.go use (Create/Get/List/Update/SetMetadata/Close + Members/TrackItem/UntrackItem) and a bd adapter; the ownership-split call-site work is deferred to its own gated phase (DESIGN §10 P3 note), so P1 only lands the interface + adapter.
- **Files:** internal/convoy/store.go, internal/convoy/store_bd.go, internal/convoy/store_bd_test.go
- **Test-first:** store_bd_test.go runs coordtest.RunClassedStoreTestsWithOptions for ClassWork (user convoy representative) with Skip:false against the bd adapter asserting GoldenRoundTrip/ReconstructUnion; a Members subtest asserts membership tracking round-trips through ConvoyStore identically to the raw beads.Store path.
- **Acceptance:**
  - ConvoyStore interface in internal/convoy; bd adapter satisfies it; convoy.go/membership.go helpers compile against it with byte-identical behavior
  - bd adapter passes the T2 conformance suite (user-convoy representative bead); no runtime synthetic-vs-user sniff is introduced (split deferred to its own phase)
  - Compile-time var _ ConvoyStore assertion; existing convoy_test.go / membership tests green
  - go vet ./internal/convoy/... clean; go test ./internal/convoy/... green
- **Depends on:** P1-T1, P1-T2

#### P1-T15 — Full-suite + CI-gate green join: prove P1 is byte-identical with all adapters wired  _(size S)_
- **Intent:** DESIGN §10 P1 exit criteria: each subsystem compiles against its typed store, each bd adapter passes its conformance suite, lifecycle_projection.go unchanged, full existing suite green, worker-boundary green. This is the join point after the swarmable per-class tracks land — it is the go/no-go gate into P2.
- **Files:** cmd/gc/worker_boundary_import_test.go, internal/session/lifecycle_projection.go
- **Test-first:** No new test logic — this task RUNS the gates: the full unit baseline, the per-class conformance suites (now Skip:false), TestGCNonTestFilesStayOnWorkerBoundary, TestOpenAPISpecInSync, TestEveryKnownEventTypeHasRegisteredPayload, make dashboard-check, go vet. lifecycle_projection.go is touched ONLY to confirm (git diff) it is byte-identical (zero diff) and pinned by the projection-invariance subtest.
- **Acceptance:**
  - make test (or make test-fast-parallel) green; go vet ./... clean
  - All five non-graph + convoy bd adapters pass their T2 conformance suites with Skip:false (golden round-trip, reconstruct-union, projection-invariance)
  - TestGCNonTestFilesStayOnWorkerBoundary, TestOpenAPISpecInSync, TestEveryKnownEventTypeHasRegisteredPayload all green; make dashboard-check green (no wire change in P1)
  - git diff shows lifecycle_projection.go and coordclass/coordrouter unchanged; no physical SQLite move occurred (byte-identical end state)
- **Depends on:** P1-T6, P1-T7, P1-T8, P1-T9, P1-T12, P1-T13, P1-T14

### P2 — Greenfield Layer-0 event emission + autoclose re-home

> Greenfield event emission for relocated SQLite infra stores: add a Layer-0 raw RowChanged emitter to SQLiteStore (emit after commit, outside the retry/Unlock closure), define the internal/events-safe RowChanged type, build Layer-2/3 translation from raw row-changes into typed convoy/order/nudge domain payloads (mail/session already exist) with RegisterPayload + auto-joined SSE union, extract the three on_close autoclose cascades (convoy/wisp/molecule) into store-aware, cross-store-traversing helpers with three cross-store conformance tests, add the watch-coherence + emit-after-commit-ordering conformance subtests (subtests c and d from DESIGN §7), and design (not yet flip) the atomic bd-hook-removal / in-process-autoclose cutover. This phase changes NO physical storage location and precedes all data moves (P4/P5).

#### P2-T1 — Define RowChanged type + RowChangeEmitter in internal/beads (Layer-0-safe, no events import)  _(size S)_
- **Intent:** Introduce the low-level row-change value emitted by SQLiteStore after every committed mutation. It must carry primitive/row data only (StoreRef, ID, Op, and the marshaled bead JSON mirroring the existing onChange json.RawMessage contract) and live where Layer 0 can emit it without an upward dependency. internal/beads imports neither internal/events nor internal/api today (verified), so the type lives in internal/beads as a pure data struct; the emitter is a func(RowChanged) callback mirroring CachingStore's onChange func(eventType, beadID string, payload json.RawMessage) shape. This is the contract every Layer-2/3 translator (T5) and conformance subtest (T8/T9) reads.
- **Files:** internal/beads/row_change.go, internal/beads/row_change_test.go
- **Test-first:** internal/beads/row_change_test.go::TestRowChangedCarriesPrimitiveRowData — construct a RowChanged{StoreRef, ID, Op: RowOpCreate, BeadJSON: json.RawMessage(...)} and assert: (a) the struct has no field whose type is from internal/events or internal/api (compile-time, plus a doc-comment invariant), (b) Op is one of the typed RowOp constants RowOpCreate/RowOpUpdate/RowOpClose/RowOpDelete, (c) BeadJSON round-trips to a beads.Bead key-for-key. Assert via go list that internal/beads does not import internal/events.
- **Acceptance:**
  - RowChanged + RowChangeEmitter + RowOp* constants compile in internal/beads with zero new imports of internal/events or internal/api
  - RowOp enumerates exactly create/update/close/delete (matching the four CachingStore notifyChange event types: bead.created/updated/closed/deleted)
  - go vet ./internal/beads/... clean; make test green for internal/beads
  - A package-doc comment states the Layer-0-safe invariant (no upward deps) so reviewers can grep it
- **Depends on:** —

#### P2-T2 — Add WithSQLiteStoreRecorder option + store the emitter on SQLiteStore  _(size S)_
- **Intent:** Add the constructor option WithSQLiteStoreRecorder(emit beads.RowChangeEmitter) to SQLiteStoreOptions/SQLiteStore (alongside WithSQLiteStoreIDPrefix/WithSQLiteStoreRetention at sqlite_store.go:48-65) and a nil-safe stored field on the struct (sqlite_store.go:96-108). No emission wiring yet — this is the injection seam only, so T3 can call it. Default (no option) keeps the emitter nil and behavior byte-identical to today, preserving the graph_store=sqlite production path that already runs SQLiteStore with no events.
- **Files:** internal/beads/sqlite_store.go, internal/beads/sqlite_store_test.go
- **Test-first:** internal/beads/sqlite_store_test.go::TestWithSQLiteStoreRecorderInjectsEmitter — open a store with WithSQLiteStoreRecorder(fn) and assert the stored emitter field is non-nil; open without the option and assert it is nil and Create/Update/Close still succeed (no panic, no emission attempted).
- **Acceptance:**
  - WithSQLiteStoreRecorder is an exported SQLiteStoreOption; the emitter field is nil-safe when unset
  - Existing SQLiteStore tests + the live graph_store=sqlite path remain byte-identical (no emission without the option)
  - go vet + make test green for internal/beads
  - Option doc comment cross-references DESIGN §6 point 1 (Layer-0 raw emit only)
- **Depends on:** P2-T1

#### P2-T3 — Emit RowChanged after commit, OUTSIDE retryOnBusy / after Unlock, on every SQLiteStore mutation  _(size M)_
- **Intent:** Wire the injected emitter to fire one RowChanged per committed mutation in Create, Update, ReleaseIfCurrent, Close, Reopen, CloseAll, and Delete (sqlite_store.go:304,578,645,689,702,715,971). Per DESIGN §6 point 3, emission MUST happen after tx.Commit succeeds and OUTSIDE the retryOnBusy closure (MaxOpenConns=1 makes an in-closure emit deadlock-prone): capture the committed bead + RowOp inside the closure, return cleanly, then emit on the success path before returning to the caller. No emit on rollback/error. Close/Reopen/CloseAll currently delegate to Update — ensure the emitted Op reflects the semantic op (close vs update), not the underlying Update, so translators (T5) see bead.closed not bead.updated.
- **Files:** internal/beads/sqlite_store.go, internal/beads/sqlite_store_test.go
- **Test-first:** internal/beads/sqlite_store_test.go::TestSQLiteStoreEmitsRowChangedAfterCommit — with a recording emitter: assert Create emits exactly one RowOpCreate with the final stored ID; Update emits one RowOpUpdate; Close emits one RowOpClose (not Update); Delete emits one RowOpDelete; a rolled-back/failed mutation emits nothing; and (ordering) the emitted bead JSON reflects the committed state (re-Get inside the emitter callback returns the same bead, proving emit is post-commit).
- **Acceptance:**
  - Exactly one RowChanged per committed mutation; zero on rollback; Op is semantic (close/delete distinct from update)
  - Emission provably occurs after tx.Commit and outside the retryOnBusy closure (a callback that calls store.Get sees the committed row; no deadlock under MaxOpenConns=1)
  - Idempotent Close/Reopen no-ops (status already terminal, sqlite_store.go:694,707) emit nothing
  - go vet + make test green for internal/beads; the existing graph_store=sqlite differential remains byte-identical when no emitter is injected
- **Depends on:** P2-T2

#### P2-T4 — Add typed convoy/order/nudge event payloads + RegisterPayload; keep registry-coverage gate green  _(size M)_
- **Intent:** Add the Layer-3 typed domain payloads the relocated stores will emit, so the new event types auto-join the SSE union (T6) and TestEveryKnownEventTypeHasRegisteredPayload stays green. mail (MailEventPayload) and session (SessionLifecyclePayload) already exist (event_payloads.go:35,278). ConvoyClosed/ConvoyCreated are currently registered as events.NoPayload (event_payloads.go:522-523) — promote ConvoyClosed to a typed payload carrying the aggregated convoy identity (per DESIGN §5 aggregated-members direction, minimal here: convoy id + reason). Add new order/nudge event-type constants to internal/events/events.go + KnownEventTypes, register typed OrderTrackingEventPayload and NudgeEventPayload in event_payloads.go init().
- **Files:** internal/events/events.go, internal/api/event_payloads.go, internal/api/event_payloads_coverage_test.go, internal/api/event_payloads_test.go
- **Test-first:** internal/api/event_payloads_test.go::TestConvoyOrderNudgePayloadsRegistered — assert events.LookupPayload returns the concrete typed struct (not NoPayload) for ConvoyClosed and for each new order/nudge event type; assert each payload satisfies events.Payload (IsEventPayload) and contains no map[string]any/json.RawMessage field (wire-typing invariant). Run TestEveryKnownEventTypeHasRegisteredPayload and assert green with the new constants added to KnownEventTypes.
- **Acceptance:**
  - New order/nudge event-type constants added to both events.go and events.KnownEventTypes
  - OrderTrackingEventPayload / NudgeEventPayload / typed ConvoyClosed payload implement events.Payload with no map[string]any or json.RawMessage fields
  - TestEveryKnownEventTypeHasRegisteredPayload passes (no missing registration)
  - No payload-type collision panic from RegisterPayload (reflect.Type uniqueness, payload.go:54)
- **Depends on:** —

#### P2-T5 — Build the Layer-2/3 RowChanged → typed domain event translator  _(size M)_
- **Intent:** Translate a beads.RowChanged emitted by a relocated SQLite store into the typed domain event (events.Event with the registered Payload) and feed it to the events.Recorder — the inverse of today's CachingStore onChange→recorder.Record path (api_state.go:176-185). Per DESIGN §6 point 2 this lives in Layer 2/3 (internal/api), NOT Layer 0. The translator maps RowOp→event type per class (e.g. order RowOpClose→OrderCompleted-style typed payload, nudge RowOpDelete→nudge-terminalized, convoy RowOpClose→typed ConvoyClosed) and decodes RowChanged.BeadJSON into the owning domain type at the row edge. This is the only place that knows the bead→domain-payload mapping; the store stays class-blind.
- **Files:** internal/api/row_change_translator.go, internal/api/row_change_translator_test.go
- **Test-first:** internal/api/row_change_translator_test.go::TestTranslatorEmitsTypedPayloadPerClass — feed a RowChanged for a convoy close, an order tracking close, and a nudge delete into the translator with a fake Recorder; assert each recorded events.Event has the correct Type and a Payload that DecodePayload (payload.go:94) round-trips to the registered typed struct with the expected fields; assert an unrecognized/work-class RowChanged is dropped (no event) so the work store never double-emits.
- **Acceptance:**
  - Translator produces exactly one typed events.Event per relocated-class RowChanged; work-class/unknown row-changes produce none
  - Emitted Payload decodes via events.DecodePayload to the T4 typed structs
  - Translator imports the owning domain packages (mail/orders/nudgequeue/convoy) for the row→domain decode, not map[string]any
  - go vet + make test green for internal/api; TestEveryKnownEventTypeHasRegisteredPayload still green
- **Depends on:** P2-T1, P2-T4

#### P2-T6 — Assert new payloads auto-join the SSE EventPayloadUnion oneOf  _(size M)_
- **Intent:** The SSE union is generated from events.RegisteredPayloadTypes() at EventPayloadUnion.Schema (convoy_event_stream.go:175-203), so any RegisterPayload from T4 auto-joins the oneOf and dedups by Go type. This work item is the explicit verification + OpenAPI/TS regen the design calls for: prove the new convoy/order/nudge payloads appear as oneOf members and that wireEventFrom (convoy_event_stream.go:207) decodes them, then regenerate the 3 openapi files and the dashboard TS so the schema-drift gates stay green.
- **Files:** internal/api/convoy_event_stream_test.go, internal/api/openapi.json, docs/reference/schema/openapi.json, cmd/gc/dashboard/web/src/generated/schema.d.ts
- **Test-first:** internal/api/convoy_event_stream_test.go::TestNewInfraPayloadsJoinEventUnion — build the EventPayloadUnion schema via a huma.Registry and assert the oneOf contains the T4 OrderTracking/Nudge/typed-Convoy payload type names; round-trip an events.Event of each new type through wireEventFrom and assert EventPayloadUnion.Value is the typed struct (not the raw custom-event fallback).
- **Acceptance:**
  - New payloads present as oneOf members in the generated EventPayload union; deduped by Go type (no duplicate entries)
  - wireEventFrom emits the typed variant for each new event type
  - TestOpenAPISpecInSync passes; npm run gen committed; Vitest + make dashboard-check green (the TS-schema-drift gate)
  - go vet + make test green for internal/api
- **Depends on:** P2-T4, P2-T5

#### P2-T7 — Extract the three on_close cascades into a store-aware, cross-store-capable autoclose package  _(size L)_
- **Intent:** Today the on_close hook shells THREE separate cross-store cascades — gc convoy autoclose, gc wisp autoclose, gc molecule autoclose (hooks.go:156-167) — each re-opening the store from cwd (doConvoyAutoclose/doWispAutoclose/doMoleculeAutoclose). Per DESIGN §6 point 5, extract the three *With cores (doConvoyAutocloseWith cmd_convoy.go:1816, doWispAutocloseWith wisp_autoclose.go:63, doMoleculeAutocloseWith molecule_autoclose.go:102) into a Layer-1/2 package that takes the store(s) explicitly and makes subtree/convoy traversal store-spanning via (storeRef,id). The dominant molecule trigger is a work-bead close (autocloseRootsForSourceBead, molecule_autoclose.go:181), which crosses the work↔graph boundary. This is a pure extract-and-rehome refactor; the cmd/gc commands become thin shims calling the new package, keeping TestGCNonTestFilesStayOnWorkerBoundary green. OnNodeClosed is net-new (verified absent) and is NOT introduced here — only the store-explicit cores.
- **Files:** internal/autoclose/autoclose.go, cmd/gc/cmd_convoy.go, cmd/gc/wisp_autoclose.go, cmd/gc/molecule_autoclose.go
- **Test-first:** internal/autoclose/autoclose_test.go::TestAutocloseCoresAreStoreInjected — call the extracted CloseConvoyCascade/CloseWispCascade/CloseMoleculeCascade with an injected store (MemStore) + recorder and a closed bead; assert the same close + ConvoyClosed/BeadClosed events fire as the current *With functions (golden-equivalence against a captured fixture from the existing cmd/gc tests), proving byte-identical behavior after the move.
- **Acceptance:**
  - The three *With cores live in internal/autoclose taking store(s) + recorder as parameters; cmd/gc autoclose commands are thin shims over them
  - Traversal helpers (subtreeTerminalExcludingRoot, listConvoyChildren, ListLiveRoots use) accept the resolving store so a (storeRef,id) cross-store walk is possible
  - Existing cmd/gc autoclose tests pass unchanged; TestGCNonTestFilesStayOnWorkerBoundary green
  - go vet + make test green for cmd/gc and internal/autoclose
- **Depends on:** —

#### P2-T8 — Three cross-store autoclose conformance tests (convoy, wisp, molecule across work↔graph)  _(size M)_
- **Intent:** Per DESIGN §6 point 5, add THREE conformance tests — one per cascade — each crossing the work↔graph store boundary via the (storeRef,id) resolver, with the molecule case driven by the dominant trigger (a work-bead close re-resolving graph-resident workflow roots, autocloseRootsForSourceBead). These pin that the extracted store-aware cores (T7) close the correct subtree when members live in a DIFFERENT store than the trigger bead. They are the load-bearing safety net for the eventual physical split (P4/P5) and run against two stores wired through the prefix-resolver shape (P3.5-resolver), stubbed here with a two-MemStore federation so the test is runnable before P3.5 lands.
- **Files:** internal/autoclose/cross_store_conformance.go, internal/autoclose/cross_store_conformance_test.go
- **Test-first:** internal/autoclose/cross_store_conformance_test.go::TestMoleculeAutocloseCrossesWorkGraphBoundary (+ convoy + wisp siblings) — put a workflow root + steps in store A (graph) and the source/work bead in store B (work); close the work bead in B; assert via the cross-store resolver that the molecule root in A is closed only when its A-subtree is terminal, and that a dangling/absent FK returns not-found cleanly (no panic, no wrong-store close). Mirror for convoy members split across stores and wisp attachments split across stores.
- **Acceptance:**
  - Three named cross-store conformance subtests (convoy/wisp/molecule) each assert correct close across a work↔graph store boundary
  - Dangling-FK (matching prefix, absent row) and unrecognized-prefix cases both resolve safely without mis-routing (DESIGN §7 subtest h shape)
  - Tests are runnable now against a two-store federation stub and adopt the real (storeRef,id) resolver when P3.5-resolver lands
  - go vet + make test green for internal/autoclose
- **Depends on:** P2-T7

#### P2-T9 — Watch-coherence + emit-after-commit-ordering conformance subtests (DESIGN §7 c, d)  _(size M)_
- **Intent:** Add conformance subtests (c) watch-coherence and (d) emit-after-commit ordering to the shared classed-store suite (coordtest/conformance.go RunClassedStoreTestsWithOptions, which today has no event subtests). Per DESIGN §6 point 4, watch-coherence asserts the emitter never goes silent on Create/Update/Close and meets a latency bound; emit-after-commit asserts the emitted row reflects committed state (a Get from inside the emitter sees the mutation) and that emission ordering matches commit ordering under MaxOpenConns=1. These run against any store wired with a RowChangeEmitter (the SQLiteStore via T2/T3) so both the bd-delegating and SQLite impls of each class run the IDENTICAL subtests.
- **Files:** internal/coordrouter/coordtest/conformance.go, internal/coordrouter/coordtest/conformance_test.go
- **Test-first:** internal/coordrouter/coordtest/conformance_test.go (Skip:false against an emitter-wired SQLiteStore) — WatchCoherence subtest: perform Create then Update then Close and assert exactly 3 RowChanged in that order with no gaps; EmitAfterCommit subtest: from the emitter callback call store.Get(rc.ID) and assert it returns the just-committed state (proving post-commit emit), and assert two concurrent committed writes emit in commit order.
- **Acceptance:**
  - RunClassedStoreTestsWithOptions gains WatchCoherence and EmitAfterCommitOrdering subtests, executed when an emitter is supplied
  - Subtests forbid silent Create/Update/Close (no missing emission) and assert emit-after-commit (Get inside callback sees committed row)
  - coordtest's own Skip:false self-test passes against an emitter-wired SQLiteStore, proving non-vacuity (mirrors existing P0 self-test discipline)
  - go vet + make test green for internal/coordrouter/coordtest
- **Depends on:** P2-T3

#### P2-T10 — Design the atomic bd-hook-removal / in-process-autoclose flip (design + guarded toggle, no live cutover)  _(size M)_
- **Intent:** Per DESIGN §6 point 6, the per-class bd-hook removal must flip ATOMICALLY with the in-process autoclose so exactly one path is live (avoids the double-emit/double-autoclose window). This phase produces the DESIGN of that flip plus a guarded, default-OFF toggle and the seam where the controller drives the extracted cores (T7) + the translator (T5) in-process — it does NOT remove hooks or flip any class live (that happens at P4/P5 cutover). beadHooks/closeHookScript (hooks.go:31-34,137-170) install the three shelled cascades; the design must specify: which controller loop calls the in-process autoclose, how installBeadHooks (hooks.go:182) becomes per-class hook-suppression, and the single switch that disables on_close shelling exactly when the in-process path turns on for that class.
- **Files:** engdocs/plans/infra-store-decouple/P2-autoclose-flip.md, cmd/gc/hooks.go, cmd/gc/controller.go, cmd/gc/hooks_test.go
- **Test-first:** cmd/gc/hooks_test.go::TestAutocloseFlipIsMutuallyExclusive — with the new default-OFF toggle, assert: toggle OFF installs the on_close cascade shelling AND the controller does NOT run in-process autoclose (current behavior, byte-identical); toggle ON suppresses the three shelled cascade lines in the on_close script AND wires the controller to call the T7 cores; assert no configuration yields BOTH (no double-autoclose window).
- **Acceptance:**
  - A written flip design (P2-autoclose-flip.md) names the controller loop, the per-class hook-suppression mechanism, and the single mutually-exclusive switch
  - A default-OFF toggle exists; OFF is byte-identical to today's shelled cascades; no setting enables both paths simultaneously
  - TestAutocloseFlipIsMutuallyExclusive green; existing hook-install/version-stamp tests (hooks.go:200-220) unchanged
  - No live class is flipped here; go vet + make test green for cmd/gc
- **Depends on:** P2-T5, P2-T7

### P3 — Full typed wire + dashboard regen (every class)

> Full typed wire + dashboard regen for every infra class, projection-only and revertible: re-root the existing sessionResponse DTO on session.Info (metadata decode at the row edge, runtime enrichment kept in Manager); build new typed GET /v0/orders/tracking, /v0/nudges, /v0/convoys DTOs with Huma registration; keep mail.Message as the direct wire type; replace convoy member-by-bead with an aggregated-members array (status/assignee/created_at per member) consumed by convoys.ts; add typed convoy/order/nudge SSE event payload structs; and pass the complete per-PR CI gate set (openapi regen + npm run gen committed + Vitest + dashboard-check) on every endpoint. No physical store move; ConvoyStore ownership-split is explicitly OUT of P3.

#### P3-T0 — Pin the per-PR CI gate harness for typed-wire PRs (golden gate doc + smoke test)  _(size S)_
- **Intent:** Every P3 endpoint PR must run an identical, complete gate set; encode it once so each later work item references it instead of re-deriving. Establishes the canary that the openapi/genclient/TS regen + vitest + dashboard-check loop is wired before any DTO churn lands.
- **Files:** engdocs/plans/infra-store-decouple/p3-gate-checklist.md, internal/api/openapi_sync_test.go
- **Test-first:** Run `make spec-ci` and `make dashboard-check` on a clean tree (no source change) and assert both are green; add/extend a test in openapi_sync_test.go that fails if internal/api/openapi.json and docs/reference/schema/openapi.json diverge byte-for-byte. This is the no-op baseline proving the regen loop is healthy before P3 mutates DTOs.
- **Acceptance:**
  - p3-gate-checklist.md enumerates the exact ordered commands per DTO PR: `make spec-ci` (regenerates internal/api/openapi.json, docs/reference/schema/openapi.json, openapi.txt, events.json, events.txt, internal/api/genclient/client_gen.go), `cd cmd/gc/dashboard/web && npm run gen` then commit src/generated/{schema.d.ts,types.gen.ts,sdk.gen.ts,client.gen.ts}, `npm test` (Vitest), `make dashboard-check`
  - On an unchanged tree, `make spec-ci` produces zero git diff (TestOpenAPISpecInSync green) and `make dashboard-check` passes
  - go vet ./... and make test stay green
  - Checklist explicitly lists TestEveryKnownEventTypeHasRegisteredPayload and TestGCNonTestFilesStayOnWorkerBoundary as gates every P3 PR must keep green
- **Depends on:** —
- **Risk:** Low. Documentation + one assertion. Mitigates the high-likelihood under-budgeting of the regen loop flagged in the risk register (openapi/dashboard churn now in scope).

#### P3-T1 — Re-root sessionResponse on session.Info with metadata decode moved to the row edge  _(size M)_
- **Intent:** sessionResponse (internal/api/handler_sessions.go:22) is already built from session.Info plus a *beads.Bead; P3 moves the bead.Metadata decode down to the P1 persisted-subset row codec so the wire DTO consumes a typed session.Info, while runtime enrichment (transport/IsRunning/Attached/Activity/peek) stays in Manager via enrichSessionResponse/sessionResponseWithReason. Projection-only: JSON contract must not change.
- **Files:** internal/api/handler_sessions.go, internal/api/decode_sessions.go, internal/session/manager.go
- **Test-first:** Add internal/api/handler_sessions_wire_test.go: feed a representative session.Info (from the P1 SessionStore row codec) into sessionToResponse/sessionResponseWithReason and assert the emitted sessionResponse JSON is byte-identical to the pre-change output for the same underlying bead corpus (golden file). Assert no Go-only field (runtime handle, *time.Time) appears in the JSON and that the metadata-decode path no longer reads raw bead keys the row codec already decoded.
- **Acceptance:**
  - sessionResponse JSON shape is unchanged (TestOpenAPISpecInSync green with no openapi diff for the session schema)
  - Metadata decode (template_overrides, options, real_world_app_* filtering, Kind/provider resolution) is sourced from the P1 persisted-subset codec output, not re-decoded from raw bead metadata in the handler
  - infoFromBead stays an impure *Manager method (transportForBead/routeACPIfNeeded/IsRunning/IsAttached/GetLastActivity untouched); enrichment stays in Layer 2/3, not the row edge
  - decode_sessions.go (CLI mirror) stays in sync; go vet + make test green; TestGCNonTestFilesStayOnWorkerBoundary green (no new session.Manager construction in cmd/gc non-test files)
- **Depends on:** P3-T0, P1-SessionStore
- **Risk:** Medium. Session metadata fidelity is the highest correctness exposure (~55 churny keys). The golden byte-identical wire test over a real bead corpus is the safety net; the change is a pure relocation of decode, not a re-model.

#### P3-T2 — Add orders.OrderTracking domain struct + storage-row codec (no endpoint yet)  _(size S)_
- **Intent:** orders.OrderTracking does not exist (orders.Order is the TOML def). Introduce the NEW typed struct that maps the order-tracking/order-run:<scoped>/NoHistory bead, with a row-edge codec, so the typed GET /v0/orders/tracking endpoint and the typed order SSE payload have a domain type to project. No UNIQUE(scoped_name,run_key) — run_key does not exist.
- **Files:** internal/orders/order_tracking.go, internal/orders/order_tracking_test.go
- **Test-first:** internal/orders/order_tracking_test.go: golden round-trip — codec(rowFromTracking(trackingFromRow(b))).Metadata == b.Metadata key-for-key including the unknown passthrough, over a corpus of order-tracking beads; assert no projection key lives in both known and unknown (drift guard).
- **Acceptance:**
  - orders.OrderTracking is a NEW struct distinct from orders.Order; carries scoped name, run/tracking identity, status, created/updated timestamps, and a versioned typed extras blob {v,known,unknown}
  - Round-trip codec is lossless key-for-key incl. unknown passthrough (golden subtest a + reconstruct-union subtest b green)
  - NO UNIQUE(scoped_name, run_key) and no run_key field is introduced
  - go vet + make test green; no upstream-owned file rewritten (new files only)
- **Depends on:** P3-T0
- **Risk:** Low-medium. New file, new struct. The single subtle point is the extras-blob passthrough; the golden round-trip test pins it.

#### P3-T3 — Add typed GET /v0/orders/tracking endpoint (DTO over OrderTracking) + Huma registration + regen  _(size M)_
- **Intent:** Build the new typed orders-tracking wire endpoint required by the full-wire mandate, projecting orders.OrderTracking through a generated Huma DTO (toOrderTrackingResponse) so the JSON contract is stable across internal field churn. No map[string]any / json.RawMessage on the DTO.
- **Files:** internal/api/huma_handlers_orders.go, internal/api/huma_types_orders.go, internal/api/supervisor_city_routes.go, cmd/gc/dashboard/web/src/generated/schema.d.ts
- **Test-first:** internal/api/handler_orders_test.go: register the new GET /v0/orders/tracking handler against a fixture store and assert the response body matches a golden orderTrackingResponse JSON; assert OpenAPI registration exists (operation id present) and the DTO carries no map[string]any/json.RawMessage.
- **Acceptance:**
  - GET /v0/orders/tracking registered via cityGet in supervisor_city_routes.go near the existing /orders block; OrderTrackingListInput/Output in huma_types_orders.go
  - Wire is a generated DTO over orders.OrderTracking (toOrderTrackingResponse at the wire edge), not the raw struct; no bead-isms leak (wire round-trip unit test)
  - make spec-ci regenerates all openapi/genclient artifacts with the new path and produces no further diff (TestOpenAPISpecInSync green); npm run gen committed; Vitest + make dashboard-check green
  - go vet + make test green
- **Depends on:** P3-T2
- **Risk:** Medium. New endpoint touches routes + types + generated TS. Bounded to <=5 files by keeping handler logic thin over the P3-T2 codec.

#### P3-T4 — Add nudgequeue row-edge projection helper for nudgequeue.Item (no endpoint yet)  _(size S)_
- **Intent:** Reuse nudgequeue.Item (state.go:31) — do not invent a new type. Provide the row-edge projection (Item from the type=chore+gc:nudge shadow bead) plus a wire-safe view so the new GET /v0/nudges endpoint and the typed nudge SSE payload have a stable domain projection. The live flock-file queue stays source of truth; the bead table is its shadow.
- **Files:** internal/nudgequeue/wire.go, internal/nudgequeue/wire_test.go
- **Test-first:** internal/nudgequeue/wire_test.go: golden round-trip — projecting a gc:nudge shadow bead to nudgequeue.Item and back preserves nudge_id and all persisted fields key-for-key incl. unknown passthrough; assert the item carries no gc.routed_to (born-unrouted invariant proof staged here, hardened in P4).
- **Acceptance:**
  - nudgequeue.Item is reused unchanged; only a row-edge/wire projection is added
  - Golden round-trip lossless incl. unknown passthrough (subtest a/b green)
  - A born-unrouted assertion exists proving the projected item never carries gc.routed_to (documents the §9.10 oracle-leak invariant; the readyExcludeTypes fix itself is P4-scoped)
  - go vet + make test green; new files only
- **Depends on:** P3-T0
- **Risk:** Low. The oracle-leak FIX is correctly deferred to P4; this only stages the wire projection + the invariant assertion.

#### P3-T5 — Add typed GET /v0/nudges endpoint (DTO over nudgequeue.Item) + Huma registration + regen  _(size M)_
- **Intent:** Build the new typed nudges wire endpoint required by the full-wire mandate (no nudge endpoints exist today). Project nudgequeue.Item through a generated Huma DTO so the dashboard can read the durability-mirror without any bead.* shape.
- **Files:** internal/api/huma_handlers_nudges.go, internal/api/huma_types_nudges.go, internal/api/supervisor_city_routes.go, cmd/gc/dashboard/web/src/generated/schema.d.ts
- **Test-first:** internal/api/handler_nudges_test.go: register GET /v0/nudges against a fixture and assert the response matches a golden nudgeResponse list; assert the operation is OpenAPI-registered and the DTO has no map[string]any/json.RawMessage.
- **Acceptance:**
  - GET /v0/nudges registered via cityGet in supervisor_city_routes.go; NudgeListInput/Output in huma_types_nudges.go; handler in a new huma_handlers_nudges.go
  - Wire is a generated DTO over nudgequeue.Item (toNudgeResponse), not the raw struct; no bead-isms leak
  - make spec-ci clean after regen (TestOpenAPISpecInSync green); npm run gen committed; Vitest + make dashboard-check green
  - go vet + make test green
- **Depends on:** P3-T4
- **Risk:** Medium. Brand-new handler file + routes + TS regen. Kept <=5 files by deferring any dashboard panel for nudges to a follow-on (endpoint-only here).

#### P3-T6 — Replace convoy member-by-bead with aggregated-members array in convoyGetResponse (server)  _(size M)_
- **Intent:** convoyGetResponse currently emits Children as []beads.Bead (huma_handlers_convoys.go:32). Per the design, keep an aggregated members array carrying status/assignee/created_at per member (NOT member-by-id, which would break convoys.ts) so the convoy DTO stops leaking raw beads while preserving exactly the fields convoys.ts:128-145 reads.
- **Files:** internal/api/huma_handlers_convoys.go, internal/api/huma_types_convoys.go
- **Test-first:** internal/api/handler_convoys_test.go: assert GET /v0/convoy/{id} returns a members array where each entry has {id,status,assignee,created_at} populated from convoycore.Members, and that progress.total/closed are unchanged; assert the response no longer embeds raw beads.Bead children (no bead-only fields like metadata leak).
- **Acceptance:**
  - convoyGetResponse exposes an aggregated members []convoyMemberResponse{id,status,assignee,created_at,...} computed from convoycore.Members; progress {total,closed} preserved
  - Every field convoys.ts reads (child.status, child.assignee, child.created_at, progress.total, progress.closed, convoy.title, convoy.status) has a typed home in the new shape
  - Change is additive-compatible OR the convoys.ts repoint is shipped in the same PR (P3-T7); if member-by-id rewrite is chosen instead it is an explicit vitest-covered item (NOT this one — this one keeps the aggregated array)
  - make spec-ci clean after regen; TestOpenAPISpecInSync green; go vet + make test green
- **Depends on:** P3-T0
- **Risk:** Medium. Convoy member shape is a known breaking-change hazard for convoys.ts; the aggregated-members choice is the design-mandated non-breaking path. Pair tightly with P3-T7.

#### P3-T7 — Repoint convoys.ts off raw bead children onto the aggregated members array + Vitest  _(size M)_
- **Intent:** After P3-T6, convoys.ts buildConvoyRow (lines 128-145) reads detail.data.children[].status/.assignee/.created_at; repoint it to the new members array and regenerate the TS types so the dashboard consumes the typed shape with no behavior change in the rendered convoy rows.
- **Files:** cmd/gc/dashboard/web/src/panels/convoys.ts, cmd/gc/dashboard/web/src/generated/schema.d.ts, cmd/gc/dashboard/web/src/panels/convoys.test.ts
- **Test-first:** cmd/gc/dashboard/web/src/panels/convoys.test.ts (Vitest): given a mocked GET /v0/convoy/{id} response using the new members array, assert buildConvoyRow computes identical {total,closed,ready,inProgress,assignees,progressPct,lastActivity} to the pre-change behavior for an equivalent fixture.
- **Acceptance:**
  - convoys.ts reads members[].{status,assignee,created_at} (and progress) from the regenerated typed schema; no reference to a raw beads.Bead children field remains
  - npm run gen committed so schema.d.ts/types.gen.ts reflect the new members shape; the generated-TS-schema-drift gate passes
  - Vitest covers the row-aggregation logic and passes; make dashboard-check passes; dashboard builds and serves locally
  - Rendered convoy rows are visually unchanged (same badges, fractions, assignee chips)
- **Depends on:** P3-T6
- **Risk:** Medium. The breaking-change surface lives here. The Vitest equivalence test against the old aggregation output is the guard; ship in the same PR as P3-T6.

#### P3-T8 — Pin mail.Message as the direct wire type (no DTO-ification) with a guard test  _(size M)_
- **Intent:** mail.Message is already the wire type (huma_handlers_mail.go) and the design says it stays direct — the one place the domain type legitimately IS the wire type. Add a guard so a future contributor does not accidentally DTO-ify it, and model the Rig field + collapse the read label/metadata double-encode at the row edge per the design note (row-edge concern, projection-visible only as a stable field).
- **Files:** internal/api/huma_handlers_mail.go, internal/mail/mail.go, internal/api/handler_mail_wire_test.go
- **Test-first:** internal/api/handler_mail_wire_test.go: assert GET /v0/mail returns mail.Message directly (the list item type is mail.Message, not a mailResponse DTO), that mail.Message.Rig is present and populated, and that read state is exposed once (no double-encoded label+metadata read flag in the wire output).
- **Acceptance:**
  - Mail wire path keeps emitting mail.Message directly (no new mailResponse DTO introduced)
  - mail.Message.Rig (mail.go:54) is modeled and surfaced; the read label/metadata double-encode is collapsed to a single typed field at the row edge
  - TestOpenAPISpecInSync green (mail schema regen reflects Rig); make dashboard-check green; go vet + make test green
  - A guard test documents that mail intentionally bypasses the DTO rule (so the no-map[string]any gate reviewer understands the exception)
- **Depends on:** P3-T0
- **Risk:** Medium. Touching mail.go (heavily fork-diverged) risks rebase surface; keep the edit to a Rig field + read-flag collapse, constructor/codec-local, not a rewrite.

#### P3-T9 — Add typed convoy/order/nudge SSE event payload structs + RegisterPayload + union membership  _(size M)_
- **Intent:** Today ConvoyCreated/ConvoyClosed/OrderFired/OrderCompleted/OrderFailed register events.NoPayload{} (event_payloads.go:522,543). Per the full-wire mandate, add typed convoy/order/nudge payloads (mirroring the existing MailEventPayload/SessionLifecyclePayload) so each event carries a typed projection of the domain type; each named payload auto-joins the SSE EventPayloadUnion dedup. The Layer-0 raw RowChanged emit and Layer-2/3 translation wiring is P2 — this item declares and registers the payload STRUCTS and keeps the coverage test green.
- **Files:** internal/api/event_payloads.go, internal/api/event_payloads_coverage_test.go
- **Test-first:** Extend internal/api/event_payloads_coverage_test.go so TestEveryKnownEventTypeHasRegisteredPayload still passes after the new typed convoy/order/nudge payloads replace the NoPayload registrations; add an assertion that each new payload type implements IsEventPayload and serializes without map[string]any.
- **Acceptance:**
  - Typed ConvoyEventPayload / OrderEventPayload / NudgeEventPayload structs added, each projecting the domain type (convoy member summary / OrderTracking / nudgequeue.Item subset), registered via events.RegisterPayload for their KnownEventTypes
  - TestEveryKnownEventTypeHasRegisteredPayload green; each payload auto-joins EventPayloadUnion (convoy_event_stream.go) with no manual union edit needed
  - make spec-ci regen reflects the new payload schemas in events.json/events.txt + openapi; TestOpenAPISpecInSync green; npm run gen committed; Vitest + make dashboard-check green
  - No emit-path change here (translation of Layer-0 RowChanged -> these payloads is P2-owned); go vet + make test green
- **Depends on:** P3-T2, P3-T4, P3-T6
- **Risk:** Medium. Replacing NoPayload with a typed payload changes the SSE schema; the coverage test + spec regen gate catch omissions. Emit wiring deliberately deferred to P2 to keep P3 projection-only.

#### P3-T10 — Repoint dashboard infra components off bead.* fields onto the typed session/order/convoy schemas  _(size M)_
- **Intent:** Per the phase goal 'dashboard component repointing off bead.* for infra', sweep the dashboard panels (sessions, orders/tracking, convoys already done in T7) to consume the regenerated typed schema fields instead of any residual bead.metadata.* access, completing the projection-off-bead for infra classes.
- **Files:** cmd/gc/dashboard/web/src/panels/sessions.ts, cmd/gc/dashboard/web/src/panels/orders.ts, cmd/gc/dashboard/web/src/generated/schema.d.ts, cmd/gc/dashboard/web/src/panels/orders.test.ts
- **Test-first:** Add/extend Vitest specs (orders.test.ts, sessions panel spec) asserting the panels read typed fields (e.g. orderTrackingResponse.status, sessionResponse.activity) and contain zero references to a generic bead metadata bag for infra rows; assert render output is unchanged against fixtures.
- **Acceptance:**
  - grep across dashboard infra panels shows no bead.metadata.* / raw-bead field access for sessions, orders-tracking, or convoys
  - Panels compile against the regenerated typed schema (npm run gen committed); the TS-schema-drift gate passes
  - Vitest green; make dashboard-check green; dashboard builds and serves locally with unchanged rendered output
  - go vet + make test green (no Go change required beyond regen)
- **Depends on:** P3-T1, P3-T3, P3-T7
- **Risk:** Medium. Pure frontend repoint; risk is missing a residual bead access. The grep gate + Vitest render-equivalence tests bound it. Split per-panel if any single panel pushes the file budget.

### P3.5 — Cross-store foundation

> Build the cross-store foundation that every physical infra move (P4/P5) depends on: a reusable (storeRef,id) IDPrefix resolver with dangling/unknown-prefix semantics; reserved non-configurable infra prefixes (gcm-/gco-/gcn-/gcs-) validated with longest-prefix precedence; a controller-mediated infra-write API so each relocated class has a single in-process writer (CLI writes become controller calls); a single controller-owned per-class retention sweep plus a WAL-checkpoint owner (replacing per-process sweeps); storehealth enumeration of every *.sqlite; and a complete inventory + re-pointing of every cmd/gc + internal/api direct infra-bead READER and WRITER onto typed stores / federation accessors, guarded by a grep test. All of this stays byte-identical on the still-single-bd backend (pure plumbing) and must keep go vet, make test, TestOpenAPISpecInSync, make dashboard-check, the TS-schema-drift gate, Vitest, TestEveryKnownEventTypeHasRegisteredPayload, and TestGCNonTestFilesStayOnWorkerBoundary green.

#### P3.5-T1 — Reserve non-configurable infra ID prefixes (gcm/gco/gcn/gcs) with longest-prefix validation  _(size S)_
- **Intent:** Establish gcm-(mail), gco-(orders), gcn-(nudges), gcs-(sessions) as reserved, non-configurable bead-ID prefixes alongside the existing graphStoreIDPrefix=gcg (api_state.go:268), validated against rig.EffectivePrefix()/EffectiveHQPrefix() so a user-configured rig prefix can never collide with a reserved infra prefix. This is the namespace foundation the IDPrefix resolver (T2) and every per-class SQLite store (P4/P5) mint into.
- **Files:** internal/config/prefix_reserved.go, internal/config/prefix_reserved_test.go, cmd/gc/api_state.go
- **Test-first:** internal/config/prefix_reserved_test.go: TestReservedInfraPrefixesRejectUserCollision asserts (a) the four reserved prefixes gcm/gco/gcn/gcs plus gcg are returned by a single ReservedInfraPrefixes() accessor; (b) a rig EffectivePrefix() equal to or a prefix-of any reserved prefix is rejected by a ValidateRigPrefix() guard; (c) longest-prefix precedence: an id 'gcm-7' resolves to the gcm class even when a user prefix 'gc' also matches.
- **Acceptance:**
  - ReservedInfraPrefixes() returns exactly {gcg,gcm,gco,gcn,gcs} as centralized constants (no magic strings scattered)
  - ValidateRigPrefix rejects any rig/HQ prefix that collides with or is a prefix-of a reserved infra prefix, with an error message naming the offending prefix
  - Longest-prefix precedence rule is unit-tested and documented on the accessor
  - go vet ./... clean; make test green; no behavior change on existing single-bd path
- **Depends on:** —
- **Risk:** Low — additive constants + validation; the only live consumer today is graph (gcg already works). Risk is forgetting a collision case; the test enumerates them.

#### P3.5-T2 — Build reusable (storeRef,id) IDPrefix resolver with dangling/unknown-prefix conformance  _(size M)_
- **Intent:** Generalize the longest-prefix-match logic in beadEventConfiguredStoreLocked (api_state.go:630-647) into a reusable resolver that maps a bead-ID PREFIX (gcg-/gc-/ga-/gcm-/gco-/gcn-/gcs-) to the typed store that owns it. This is the ~20-line IDPrefix switch DESIGN §2/§7 names as the hard prerequisite of the FIRST physical move (order single-flight reads graph wisp roots; mail/nudge resolve session FKs). It is DISTINCT from makeStoreRefResolver (cmd_convoy_dispatch.go:325) which resolves city:/rig: scope refs, not ID prefixes.
- **Files:** internal/beads/idprefix_resolver.go, internal/beads/idprefix_resolver_test.go, internal/beads/storetest/idprefix_conformance.go
- **Test-first:** internal/beads/idprefix_resolver_test.go: TestIDPrefixResolver asserts (a) a registered prefix routes Get(id) to the correct store; (b) a matching-prefix-but-absent-row returns a clean typed not-found (ErrNotFound), NOT a mis-route; (c) an UNRECOGNIZED prefix returns a typed ErrUnknownPrefix and never silently falls through to the work store; (d) longest-prefix precedence (gcm- wins over gc-). The two negative cases (dangling FK, unknown prefix) ARE conformance subtest (h) from DESIGN §7.
- **Acceptance:**
  - Resolver registers (prefix -> store) pairs and resolves by longest-prefix match, reusing T1's ReservedInfraPrefixes
  - Dangling-FK (matching prefix, absent row) returns typed not-found cleanly; unknown-prefix returns typed ErrUnknownPrefix — both pinned by conformance subtest (h)
  - A shared conformance helper (idprefix_conformance.go) is callable by any class's store test so the bd impl and future SQLite impl run identical FK-resolve+dangling checks
  - Zero behavior change to beadEventConfiguredStoreLocked yet (that re-point is T3); go vet + make test green
- **Depends on:** P3.5-T1
- **Risk:** Medium — silent mis-route is the exact failure mode DESIGN §9.4 forbids; the unknown-prefix-never-falls-through assertion is the load-bearing guard.

#### P3.5-T3 — Re-point workflow-finalize ResolveStoreRef onto the IDPrefix resolver  _(size M)_
- **Intent:** Wire the dispatch workflow-finalize cross-store close (dispatch/runtime.go:48-63 ResolveStoreRef / SourceWorkflowStores, constructed at cmd_convoy_dispatch.go:205-210) to consult the T2 IDPrefix resolver for bead-ID-prefixed refs, while keeping the existing city:/rig: scope-ref scheme verbatim under the same lock (DESIGN §9.3: re-point ResolveStoreRef, do not build new machinery). Must land BEFORE Router deletion (P6) but is built and verified here.
- **Files:** cmd/gc/cmd_convoy_dispatch.go, cmd/gc/cmd_convoy_dispatch_test.go, internal/dispatch/runtime.go
- **Test-first:** cmd/gc/cmd_convoy_dispatch_test.go: TestStoreRefResolverHandlesIDPrefixAndScope asserts the resolver returned by makeStoreRefResolver still resolves city:/rig: refs identically AND now resolves a gcg-/gcm- ID-prefixed ref to the owning store via the T2 resolver; an orphaned-source finalize across the work<->graph boundary still closes the source bead (the orphaned-source finalize regression, DESIGN §9.3).
- **Acceptance:**
  - city:/rig: scope-ref resolution is byte-identical to today (existing makeStoreRefResolver tests stay green)
  - ID-prefixed refs route through the T2 resolver under the existing SourceWorkflowLock verbatim
  - Orphaned-source workflow-finalize across work<->graph still closes the source bead (regression test green)
  - go vet + make test green; no Router code touched (deletion is P6)
- **Depends on:** P3.5-T2
- **Risk:** Medium — finalize closes Work across the boundary; the live-root guard under the lock must stay verbatim. The orphaned-source regression is the safety net.

#### P3.5-T4 — Define controller-mediated infra-write API contract (single in-process writer)  _(size M)_
- **Intent:** Establish the Huma-registered controller write endpoints that make each relocated infra class have a single in-process writer (DESIGN decision 5 / §7): CLI/other-process WRITES route through the controller; READS open their own read-only WAL handle. This task defines the contract + handler skeleton only (typed request/response DTOs, no map[string]any), so the per-class CLI cutovers (T5/T6/T7) have a target. Reuses the existing apiClient(cityPath) (apiroute.go:42) + degraded-fallback pattern already used by mail check (cmd_mail.go mailCheckAPIClient).
- **Files:** internal/api/handler_infra_write.go, internal/api/handler_infra_write_test.go, internal/api/openapi.json, docs/reference/schema/openapi.json
- **Test-first:** internal/api/handler_infra_write_test.go: TestInfraWriteEndpointsRegistered asserts each new write endpoint (nudge enqueue, mail send, session/order repair-write) is Huma-registered with a typed DTO (no map[string]any / json.RawMessage), returns a typed response, and rejects an empty/unknown class with a 4xx — driving the failing handler into existence.
- **Acceptance:**
  - Write endpoints are Huma-registered with typed DTOs; TestOpenAPISpecInSync green after regenerating all 3 openapi files
  - npm run gen committed (schema.d.ts + openapi-ts SDK); the TS-schema-drift gate and Vitest green; make dashboard-check green
  - No map[string]any or json.RawMessage on any DTO; endpoints validate class and reject unknown class
  - Handlers serialize writes through the single controller store handle (MaxOpenConns=1 path); go vet + make test green
- **Depends on:** —
- **Risk:** Medium — touches the wire/OpenAPI gates. Endpoint set may need confirmation against P3's typed-wire endpoints landing in parallel; keep contract minimal (only the three write verbs the CLI cutovers need).

#### P3.5-T5 — Route gc nudge writes through the controller write API  _(size M)_
- **Intent:** Cut the nudge-queue durable-mirror WRITES (enqueue/terminalize) in cmd_nudge.go over to the T4 controller write API so the nudge store has a single in-process writer, with the degraded local-fallback pattern mirroring mail check. Reads (status/poll) keep their own read path. Part of eliminating cross-process WAL write contention by construction (DESIGN §8).
- **Files:** cmd/gc/cmd_nudge.go, cmd/gc/cmd_nudge_test.go
- **Test-first:** cmd/gc/cmd_nudge_test.go: TestNudgeEnqueueRoutesThroughController asserts that with a controller up, gc nudge enqueue/terminalize POSTs to the T4 endpoint (injected via a test client like mailCheckAPIClient) and does NOT open a direct writable store; with no controller, it emits exactly one route=fallback log line and uses the local path.
- **Acceptance:**
  - Nudge WRITES route through the controller API when one is up; reads unchanged
  - Degraded fallback emits exactly one route=... log line (GC_DEBUG-gated), matching the mail-check convention
  - No new direct writable-store open in cmd_nudge.go non-test code (sets up T13 grep-guard)
  - go vet + make test green; existing nudge tests stay green
- **Depends on:** P3.5-T4
- **Risk:** Medium — nudge has a live flock-file queue as source of truth (DESIGN §4); the controller mirror must not change that ordering. SWARMABLE with T6/T7.

#### P3.5-T6 — Route gc mail send/archive writes through the controller write API  _(size M)_
- **Intent:** Cut mail WRITES (send, archive) over to the T4 controller write API so mail has a single in-process writer, extending the existing mail-check controller route (cmd_mail.go mailCheckAPIClient / routeMailCheck) to the write verbs. Reads/checks keep their existing route. extmsg writers are explicitly OUT of scope (P7).
- **Files:** cmd/gc/cmd_mail.go, cmd/gc/cmd_mail_test.go, cmd/gc/providers.go
- **Test-first:** cmd/gc/cmd_mail_test.go: TestMailSendRoutesThroughController asserts gc mail send/archive POSTs to the T4 endpoint when a controller is up (injected client) and falls back to the local mail.Provider path with one route=fallback log line when not; assert no direct writable beads.Store is opened in the controller-up path.
- **Acceptance:**
  - Mail send + archive WRITES route through the controller API when up; mail check/read path unchanged
  - Degraded fallback preserves delivery side-effects (the existing reason mail check falls back for injecting hooks) and logs one route= line
  - No new direct writable-store open for mail writes in non-test cmd/gc code
  - go vet + make test green; existing mail tests green
- **Depends on:** P3.5-T4
- **Risk:** Medium — mail delivery has side effects after a successful write (existing routeMailCheck comment); the fallback must preserve them. SWARMABLE with T5/T7.

#### P3.5-T7 — Route order-tracking + session-repair infra writes through the controller write API  _(size L)_
- **Intent:** Cut the remaining cross-process infra WRITES — order-tracking create/close (order_dispatch / cmd_order sweep) and session-bead repair writes (session_beads.go store.Update/SetMetadata at :347,:459,:529,:861,:1156) — over to the T4 controller write API so orders and sessions each have a single in-process writer. Reads stay local. session_beads.go must stay on the worker boundary (TestGCNonTestFilesStayOnWorkerBoundary).
- **Files:** cmd/gc/order_dispatch.go, cmd/gc/session_beads.go, cmd/gc/cmd_order.go, cmd/gc/order_dispatch_test.go, cmd/gc/session_beads_test.go
- **Test-first:** cmd/gc/session_beads_test.go: TestSessionRepairWritesRouteThroughController asserts the session-bead repair writers (canonical-election repair, type repair, session_name set) route through the controller write API when up and fall back locally otherwise; order_dispatch_test.go: TestOrderTrackingWriteRoutesThroughController asserts order-tracking create/close routes through the controller.
- **Acceptance:**
  - Order-tracking and session-repair WRITES route through the controller API when up; reads unchanged
  - The canonical-election / closed-name-release logic (session_beads.go:878 region) is preserved verbatim — only the write transport changes
  - TestGCNonTestFilesStayOnWorkerBoundary stays green; no new bypass imports
  - go vet + make test green; existing order/session tests green
- **Depends on:** P3.5-T4
- **Risk:** High — session_beads.go is dense and worker-boundary-guarded; splitting writes from the election logic without changing semantics is delicate. If it exceeds 5 files, split into T7a(orders) + T7b(sessions). SWARMABLE with T5/T6 only after the split.

#### P3.5-T8 — Build the controller-owned per-class retention sweep; disable per-process sweeps  _(size M)_
- **Intent:** Replace the per-process order-tracking retention watchdog (city_runtime.go:1397 runOrderTrackingRetentionWatchdog, startup sweep at :244-259) with a single controller-owned per-class retention sweep covering every relocated ephemeral/no_history class (sessions/orders/nudges/mail), parameterized by class TTL. DESIGN §7: this sweep does not exist today (graph has zero retention; ga-2gap48 unbuilt) and must be pulled into this initiative. Disable per-process sweeping so the controller is the sole sweeper.
- **Files:** cmd/gc/city_runtime.go, internal/beads/retention_sweep.go, internal/beads/retention_sweep_test.go, cmd/gc/city_runtime_test.go
- **Test-first:** internal/beads/retention_sweep_test.go: TestPerClassRetentionSweep asserts a single sweep loop prunes closed/terminal records per class according to that class's TTL, runs only in the controller (a per-process invocation is a no-op when the controller owns it), and leaves live records untouched. city_runtime_test.go: TestOrderTrackingPerProcessSweepDisabled asserts the old per-process watchdog no longer runs when controller-owned sweep is active.
- **Acceptance:**
  - A single controller-owned sweep handles all relocated classes via per-class TTL config (reusing WithSQLiteStoreRetention semantics where applicable)
  - Per-process order-tracking watchdog is disabled when the controller sweep is active (no double-sweep)
  - Live records never pruned; closed/terminal pruned per TTL; sweep is idempotent
  - go vet + make test green; existing order-tracking retention tests adapted, not deleted
- **Depends on:** —
- **Risk:** Medium — moving the sweep owner risks a window with zero sweeping or double-sweeping; the disabled-per-process test pins single-owner. Independent of A/B tracks.

#### P3.5-T9 — Establish the WAL-checkpoint owner under many short-lived readers  _(size M)_
- **Intent:** Resolve DESIGN §12 open decision: under MaxOpenConns=1 writer + many short-lived read-only WAL handles per process, decide and implement who runs passive WAL checkpoints so the WAL does not grow unbounded. Default per DESIGN: the controller (sole writer) runs passive checkpoints. Includes a short analysis spike documenting the chosen owner before implementation.
- **Files:** internal/beads/sqlite_store.go, internal/beads/sqlite_store_checkpoint_test.go, engdocs/plans/infra-store-decouple/wal-checkpoint-owner.md
- **Test-first:** internal/beads/sqlite_store_checkpoint_test.go: TestControllerRunsPassiveCheckpoint asserts that after N committed writes the controller-owned checkpoint runs a PASSIVE wal_checkpoint and the WAL size returns to bounded; a read-only handle never attempts a checkpoint; concurrent short-lived readers do not block the checkpoint indefinitely.
- **Acceptance:**
  - Analysis doc names the checkpoint owner and the failure mode it avoids (WAL growth, reader starvation)
  - Controller runs passive checkpoints; read-only handles never checkpoint
  - WAL stays bounded under a write+many-reader test; no SQLITE_BUSY escapes retryOnBusy
  - go vet + make test green
- **Depends on:** P3.5-T8
- **Risk:** Medium — checkpoint contention under WAL is subtle; PASSIVE (not TRUNCATE/RESTART) avoids blocking readers but may not reclaim under a persistent reader. Spike resolves the tradeoff first.

#### P3.5-T10 — Extend storehealth to enumerate every *.sqlite infra store  _(size S)_
- **Intent:** Extend internal/storehealth (today .beads/dolt only — storehealth.go:41 StorePath returns .beads/dolt) to discover and report size/disk/GC for every <cityPath>/.gc/<class>.sqlite file (mail/orders/nudges/sessions/graph), so observability covers relocated infra stores (DESIGN §7). Fully independent — swarmable anytime.
- **Files:** internal/storehealth/storehealth.go, internal/storehealth/storehealth_test.go, cmd/gc/city_status_store_health_test.go
- **Test-first:** internal/storehealth/storehealth_test.go: TestEnumerateSQLiteStores asserts storehealth discovers each *.sqlite under .gc/, reports per-file size via WalkSize, and includes them in the Health output alongside the existing dolt entry; a missing/empty .gc dir yields the dolt-only result (no panic, no error).
- **Acceptance:**
  - storehealth enumerates every .gc/*.sqlite and reports size/disk per file
  - Existing dolt/.beads health output unchanged when no sqlite stores present
  - city status store-health surfaces the new sqlite entries (cmd test green)
  - go vet + make test green
- **Depends on:** —
- **Risk:** Low — additive enumeration; the only risk is the empty/missing dir case, pinned by the test. Fully independent.

#### P3.5-T11 — Produce the infra-bead reader/writer INVENTORY + typed federation accessors  _(size M)_
- **Intent:** Produce the authoritative inventory of EVERY direct infra-bead READER and WRITER in cmd/gc + internal/api (status/doctor/demand-scan/session_wake.go/session_beads.go/cmd_nudge/cmd_mail), classifying each as reader vs writer and target class, and introduce the typed federation accessor(s) readers will re-point onto (building on the existing AssignedWorkStores/collectAssignedWorkBeadsWithStores at build_desired_state.go:49,:660). This artifact is the contract that T12 (reader re-point) and T5-T7 (writer cutover) execute against, and the spec the T13 grep-guard enforces. extmsg writers recorded but NOT re-pointed (P7).
- **Files:** engdocs/plans/infra-store-decouple/p35-reader-writer-inventory.md, cmd/gc/infra_store_accessors.go, cmd/gc/infra_store_accessors_test.go
- **Test-first:** cmd/gc/infra_store_accessors_test.go: TestInfraStoreFederationAccessor asserts a single typed accessor returns the correct per-class store set for reads (sessions/orders/nudges/mail) and that demand-scan reads via it produce byte-identical results to today's collectAssignedWorkBeadsWithStores over an all-classes fixture (no behavior change on single-bd).
- **Acceptance:**
  - Inventory enumerates every direct infra-bead read/write site with file:line, target class, and reader|writer classification; extmsg sites marked deferred
  - A typed federation accessor exists for reads, reusing the existing AssignedWorkStores pattern; byte-identical to today on single-bd
  - The inventory explicitly lists the files T12 will touch and confirms each is <=5-file-splittable
  - go vet + make test green
- **Depends on:** —
- **Risk:** Medium — completeness is the risk (a missed reader becomes a silent stale-read after P4/P5). The T13 grep-guard is the backstop, but the inventory must be exhaustive first.

#### P3.5-T12 — Re-point all cmd/gc + internal/api infra-bead READERS onto typed stores / federation accessors  _(size L)_
- **Intent:** Re-point every direct infra-bead READER catalogued in T11 (status/doctor/demand-scan, session_wake.go, session_beads.go read paths, cmd_nudge/cmd_mail reads, internal/api session-resolution reads) onto the typed federation accessor from T11 / the T2 IDPrefix resolver, so no reader assumes one generic beads.Store. Byte-identical on single-bd. Must keep TestGCNonTestFilesStayOnWorkerBoundary green.
- **Files:** cmd/gc/build_desired_state.go, cmd/gc/session_wake.go, cmd/gc/cmd_status.go, cmd/gc/cmd_doctor.go, internal/api/session_resolution.go
- **Test-first:** cmd/gc/build_desired_state_test.go (extend): TestDemandScanReadsViaFederationAccessor asserts the demand scan and status/doctor readers resolve infra beads through the typed accessor (not a raw generic store), and that output is byte-identical to the pre-change path over an all-classes fixture; a session-wake wait read resolves its nudge-shadow FK via the T2 resolver.
- **Acceptance:**
  - Every reader in the T11 inventory reads via the typed accessor / IDPrefix resolver; none opens a raw generic infra store directly
  - Byte-identical demand-scan / status / doctor output on single-bd (differential test green)
  - TestGCNonTestFilesStayOnWorkerBoundary green; worker-boundary not bypassed
  - go vet + make test + TestOpenAPISpecInSync + make dashboard-check green
- **Depends on:** P3.5-T11, P3.5-T2
- **Risk:** High — this is the broadest re-point and likely exceeds 5 files; SPLIT per the T11 inventory into T12a(demand-scan+status+doctor), T12b(session_wake+session_beads reads), T12c(internal/api session_resolution reads), each <=5 files and independently byte-identical. Each split is swarmable.

#### P3.5-T13 — Add grep-guard test forbidding new generic-store infra reads/writes in cmd/gc non-test files  _(size S)_
- **Intent:** Add a CI grep-guard (modeled on TestGCNonTestFilesStayOnWorkerBoundary, worker_boundary_import_test.go:11) that fails the build if a non-test cmd/gc file opens a raw generic beads.Store for an infra class instead of routing through the typed federation accessor / controller write API / IDPrefix resolver. This locks in T5-T7 (writes) and T12 (reads) and prevents regression as P4/P5 land.
- **Files:** cmd/gc/infra_store_boundary_import_test.go
- **Test-first:** cmd/gc/infra_store_boundary_import_test.go: TestGCNonTestFilesStayOffGenericInfraStore enumerates non-test cmd/gc files and fails if any contains a forbidden generic-infra-store access pattern (direct OpenSQLiteStore for an infra class, direct writable-store open in cmd_nudge/cmd_mail/order/session write paths) outside the sanctioned accessor/write-API helpers; the test itself is the assertion.
- **Acceptance:**
  - Test fails on a deliberately-introduced forbidden generic infra-store access and passes on the current re-pointed tree
  - Allowlist for sanctioned construction sites (controller composition root) is explicit and documented, mirroring the worker-boundary allowlist
  - Test runs in the standard make test sweep; go vet green
  - Does not duplicate or weaken TestGCNonTestFilesStayOnWorkerBoundary
- **Depends on:** P3.5-T12, P3.5-T5, P3.5-T6, P3.5-T7
- **Risk:** Low — a static guard; main risk is over-broad matching producing false positives, mitigated by the explicit allowlist and the deliberate-violation test.

### P4 — SQLite cutover — mail → orders → nudges

> Physically relocate the three lowest-coupling, highest-churn, non-claimable infra classes — mail, order-dispatch tracking, nudge-queue mirror — off bd/Dolt onto per-class embedded SQLite stores, banking the fork-rate contention win. Each class gets a second SQLiteStore-backed impl behind its P1 domain interface (typed columns + indexes, distinct prefix gcm/gco/gcn, city-scope single id sequence), selected by a new per-class [beads.classes.<class>].backend config knob defaulting to bd. Writes route through the long-lived controller (MaxOpenConns=1 in-process serialization); reads open their own read-only WAL handle. Cutover is clean drain-then-switch (no dual-write, no backfill — all three are no_history/ephemeral). The nudge oracle-leak is fixed BEFORE nudges flip. Per-class readyExcludeTypes deletions land as separate revertible commits with deletion tests. Revert is quiesce-then-flip-back at a maintenance window, covered by a flip-back integrity test first applied to the already-landed graph class. The full conformance suite (subtests a-i) runs against each SQLite impl with Skip:false, and a fork-rate soak validates the win.

#### P4-T1 — Add per-class [beads.classes.<class>].backend config selector  _(size M)_
- **Intent:** Introduce the per-class backend selector the whole phase routes on. Today only a single [beads] graph_store="sqlite" string exists (config.go:1268); P4 needs an addressable [beads.classes.mail|orders|nudges].backend knob defaulting to "bd" so each class flips independently and reverts with a one-line config change. This replaces the graph-only special case with a general per-class map without disturbing the live graph_store wiring.
- **Files:** internal/config/config.go, internal/config/config_test.go, internal/config/compose.go, cmd/gc/api_state.go
- **Test-first:** TestBeadsClassBackendSelector in internal/config/config_test.go: load a city.toml with [beads.classes.mail] backend="sqlite" and assert (a) cfg.Beads.Classes["mail"].Backend=="sqlite", (b) unset classes resolve to "bd" via a BackendForClass(class) accessor, (c) an unknown backend value is rejected by validation with a typed error, (d) a pack-layer [beads.classes.*] arm deep-merges over the city layer (DESIGN §12 default).
- **Acceptance:**
  - New BeadsConfig.Classes map[string]BeadClassConfig{Backend string} with BackendForClass(class) returning "bd" default; jsonschema enum=bd,enum=sqlite on Backend
  - compose.go deep-merge arm added for [beads.classes.*] so packs can layer per-class backends (DESIGN §12)
  - graph_store remains untouched and continues to work (no regression in existing graph_store config_test.go cases)
  - go vet ./... clean; make test green for internal/config
  - Validation rejects unknown backend values with a contextual error (fmt.Errorf %q)
- **Depends on:** —
- **Risk:** Config-shape churn could touch the jsonschema generation; keep the map additive and behind a default so absent config is byte-identical to today.

#### P4-T2 — Build shared controller-mediated SQLite-class scaffolding (open + handle cache + read-only WAL reader + prefix validation)  _(size M)_
- **Intent:** Generalize the graph-only registerGraphStoreBackend pattern (api_state.go:276-335: OpenSQLiteStore + noCloseGraphStore + WithSQLiteStoreIDPrefix/WithSQLiteStoreRetention) into reusable scaffolding every relocated infra class uses: a controller-owned single in-process writer (MaxOpenConns=1) per <cityPath>/.gc/<class>.sqlite, a shared handle cache keyed on (cityPath,class), and a per-process read-only WAL handle for readers. Validate the reserved prefixes gcm-/gco-/gcn- as non-configurable against rig.EffectivePrefix() with longest-prefix precedence (DESIGN §4).
- **Files:** cmd/gc/api_state.go, cmd/gc/infra_sqlite_class.go, cmd/gc/infra_sqlite_class_test.go, internal/beads/sqlite_store.go
- **Test-first:** TestInfraSQLiteClassScaffolding in cmd/gc/infra_sqlite_class_test.go: (a) opening class "mail" twice returns the SAME cached writer handle (one MaxOpenConns=1 writer); (b) a reader handle opens read-only WAL and sees a committed write from the writer; (c) registering prefix "gcm" that collides with a rig.EffectivePrefix() is rejected; (d) longest-prefix precedence resolves gcm- before a shorter g- prefix.
- **Acceptance:**
  - openInfraClassStore(cityPath, class, prefix) returns a controller-owned writer (MaxOpenConns=1) and a separate read-only WAL reader factory, file at <cityPath>/.gc/<class>.sqlite
  - Reserved prefixes gcm-/gco-/gcn- validated non-configurable against rig.EffectivePrefix() with longest-prefix precedence; collision returns a typed error
  - Handle cache prevents a second writer connection per (cityPath,class); CloseStore is a no-op for shared handles (mirrors noCloseGraphStore)
  - WAL-checkpoint owner decision recorded (default: controller runs passive checkpoints, DESIGN §12) and wired or explicitly deferred with a tracking note
  - go vet clean; concurrent-process conformance prerequisite (subtest i scaffolding) compiles
- **Depends on:** P4-T1
- **Risk:** MaxOpenConns=1 + post-commit emit ordering: the writer must not hold the connection while the RowChanged emit (P2) runs — scaffolding must expose an emit-after-commit/after-Unlock hook (DESIGN §6.3). Mis-sequencing deadlocks.

#### P4-T3 — Port quiesce-then-flip-back revert + flip-back integrity test to the LANDED graph class  _(size M)_
- **Intent:** Prove the revert mechanism on real production wiring (the already-landed graph SQLite class) BEFORE any new class moves. DESIGN §7 mandates the flip-back integrity test be applied first to the graph class. This validates quiesce-the-class-then-flip-back at a maintenance window: stop writes, drain in-flight, flip backend config, assert no rows lost and projections equal. This is the safety net the accepted prod-divergence risk leans on.
- **Files:** cmd/gc/api_state.go, cmd/gc/infra_flip_back_test.go
- **Test-first:** TestGraphClassFlipBackIntegrity in cmd/gc/infra_flip_back_test.go: with graph_store=sqlite, write a graph plan; quiesce (stop new creates), flip backend selection back, and assert (a) the graph topology read back through the post-flip path is row-for-row equal to pre-flip, (b) no SQLITE_BUSY escapes retryOnBusy during quiesce, (c) the is_blocked/Ready projection over the corpus is unchanged across the flip.
- **Acceptance:**
  - A reusable quiesceClassThenFlipBack(class) helper that stops creates, waits for in-flight drain, and re-selects the backend
  - Flip-back over the graph class preserves topology row-for-row and projection-invariant (DESIGN §7 graph-first requirement)
  - Test documents the accepted risk: revert is a maintenance-window operation, not mid-flight rollback
  - make test green for the new test; existing graph_store tests still green
- **Depends on:** P4-T2
- **Risk:** If the graph class flip-back reveals a latent loss, it blocks ALL P4 cutovers (good — that is the point); budget for fixing the shared scaffolding before proceeding.

#### P4-T4 — Implement SQLite MailStore impl (typed columns + indexes, gcm- prefix)  _(size M)_
- **Intent:** Build the second MailStore impl over SQLiteStore behind the P1 mail interface. Promote thread_id and read to indexed typed columns (kills the label-as-index hack), model mail.Message.Rig (mail.go:54), and collapse the read label/metadata double-encode (DESIGN §4). mail.Message IS the wire type — no DTO. City-scope single id sequence, prefix gcm-. Reads delegate to the read-only WAL handle; writes route through the controller.
- **Files:** internal/mail/sqlitemail/sqlitemail.go, internal/mail/sqlitemail/sqlitemail_test.go, internal/mail/mail.go
- **Test-first:** TestSQLiteMailStore_GoldenRoundTrip in internal/mail/sqlitemail/sqlitemail_test.go (subtest a): send a mail.Message with Rig set + custom unknown metadata keys, read it back, assert key-for-key Metadata equality including the unknown passthrough (the loss detector), and assert thread_id/read are queried via indexed columns not labels.
- **Acceptance:**
  - SQLite schema: messages table with indexed thread_id, read, recipient columns + versioned typed extras blob {v,known,unknown} (DESIGN §4 modeling rule)
  - mail.Message.Rig modeled; read state single-encoded (no label/metadata double-write)
  - Implements the full P1 MailStore/mail.Provider surface; var _ mail.Provider assertion compiles
  - gcm- prefix; city-scope single sequence; no UNIQUE(session_name)-style drop-ins
  - go vet clean
- **Depends on:** P4-T2
- **Risk:** mail.Provider has a wide method set (Send/Inbox/Check/Read/Get/MarkRead/Reply/Thread per mailtest); missing a method fails compile, not silently — but watch the read/unread filter semantics that mailtest pins.

#### P4-T5 — Run full conformance suite (a-i) against SQLite MailStore with Skip:false  _(size M)_
- **Intent:** The conformance suite is the ONLY safety net under conformance-only validation (DESIGN §6 owner decision). Run the IDENTICAL suite (mailtest.RunProviderTests + the a-i classed subtests) that the bd MailStore impl runs, with Skip:false, against the SQLite impl. Subtests a-i are themselves the validation contract: golden round-trip, reconstruct-union, watch-coherence, emit-after-commit, projection-invariance, error-classification, readyExcludeTypes-deletion, cross-store FK resolve, concurrent-process.
- **Files:** internal/mail/sqlitemail/conformance_test.go, internal/mail/mailtest/conformance.go
- **Test-first:** TestSQLiteMail_Conformance in internal/mail/sqlitemail/conformance_test.go runs mailtest.RunProviderTests + RunClassedStoreTestsWithOptions(Skip:false) against a fresh SQLite MailStore; the first asserted-failing subtest to write is (f) error-classification: a UNIQUE/contention failure maps to typed ErrRetryableContention (not ErrHard).
- **Acceptance:**
  - All mailtest.RunProviderTests subtests pass against the SQLite impl (parity with beadmail)
  - Classed subtests a-i pass with Skip:false: (a) golden round-trip incl. unknown passthrough, (b) reconstruct-union equality, (c) watch-coherence (no silent Create/Update/Close), (d) emit-after-commit ordering, (e) projection-invariance, (f) error-classification typed, (g) readyExcludeTypes-deletion harness present, (h) cross-store FK resolve (mail→bd session) + dangling, (i) concurrent-process id non-collision + no SQLITE_BUSY escape
  - The IDENTICAL suite still passes against the bd MailStore impl (proves the suite is not SQLite-specific)
  - make test green
- **Depends on:** P4-T4, P2-rowchanged-emit, P3.5-resolver
- **Risk:** Subtest (h) mail→bd-session FK resolve depends on the P3.5 prefix resolver tolerating a SQLite-resident row pointing at a bd-resident session (sessions don't move until P5). If P3.5-resolver only handles same-store, this subtest fails and blocks the mail flip.

#### P4-T6 — Cut mail over: drain-then-switch bd→SQLite, controller-mediated writes, atomic bd-hook flip  _(size M)_
- **Intent:** Flip mail to the SQLite backend via clean drain-then-switch (no dual-write, no backfill — mail is Ephemeral: true, beadmail.go:137,510). Stop creating bd mail beads, let in-flight bd mail close naturally, switch new sends to the SQLite MailStore through the controller write API. Sequence the bd-hook removal to flip ATOMICALLY with the in-process autoclose/emit so exactly one event path is live (DESIGN §6.6). This is a point of no return for new mail rows.
- **Files:** cmd/gc/api_state.go, cmd/gc/infra_sqlite_class.go, internal/mail/beadmail/beadmail.go, cmd/gc/infra_cutover_test.go
- **Test-first:** TestMailCutoverDrainThenSwitch in cmd/gc/infra_cutover_test.go: with [beads.classes.mail].backend flipped to sqlite, assert (a) new Send writes a gcm- row in SQLite and zero new bd mail beads, (b) an in-flight bd mail bead created pre-flip is still readable through the federated read path until it closes, (c) exactly one event path fires per send (no double-emit), (d) writes from a non-controller process route through the controller write API.
- **Acceptance:**
  - New mail creates land in SQLite (gcm-), zero new bd mail beads after flip
  - In-flight bd mail beads readable until naturally closed (drain), no data copy
  - bd-hook removal for mail flips atomically with the in-process emit; watch-coherence subtest stays green (no double-emit, no silent window)
  - CLI/other-process mail writes route through the controller write API; reads use read-only WAL
  - doctor_fork_rate shows mail's bd write contribution dropping; stabilization window observed before the next class flip
- **Depends on:** P4-T5, P3.5-write-api, P2-autoclose-rehome
- **Risk:** ACCEPTED PROD-DIVERGENCE RISK: no in-prod oracle — the first mail divergence is discovered in production; revert is quiesce-the-mail-class-then-flip-back at a maintenance window (P4-T3 mechanism). The atomic bd-hook/in-process-emit flip is the highest-risk step (a non-atomic flip double-emits or goes silent).

#### P4-T7 — Delete mail's readyExcludeTypes entry (separate revertible commit + deletion test)  _(size S)_
- **Intent:** After mail leaves the work store, delete the message exclusion from readyExcludeTypes (beads.go:182) as a SEPARATE revertible commit. The deletion IS the proof the split is complete (DESIGN §9.10, coupling 10). A deletion test asserts the work store never receives a message-typed bead and Work().Ready is unchanged.
- **Files:** internal/beads/beads.go, internal/beads/beads_test.go
- **Test-first:** TestMailExclusionDeletion in internal/beads/beads_test.go: assert (a) "message" is no longer in readyExcludeTypes, (b) the work store never receives a message-typed bead post-cutover (creation paths route to SQLite), (c) Work().Ready output over a fixture is unchanged by the deletion.
- **Acceptance:**
  - "message": true removed from readyExcludeTypes in a commit that touches only the exclusion + its test (revertible)
  - Deletion test green: work store never receives message beads; Work().Ready unchanged
  - go vet clean; make test green
- **Depends on:** P4-T6
- **Risk:** Premature deletion (before drain completes) would un-exclude still-resident bd message beads and leak them into Ready. Must land strictly after P4-T6 confirms drain.

#### P4-T8 — Implement SQLite OrderStore impl (typed columns + indexes, gco- prefix)  _(size M)_
- **Intent:** Build the second OrderStore impl over SQLiteStore behind the P1 orders interface (over the NEW orders.OrderTracking struct, not orders.Order the TOML def). Maps order-tracking/order-run:<scoped>/NoHistory beads to typed columns indexed for the recency query. NO UNIQUE(scoped_name,run_key) — run_key does not exist (DESIGN §4). City-scope single sequence, prefix gco-.
- **Files:** internal/orders/sqliteorders/sqliteorders.go, internal/orders/sqliteorders/sqliteorders_test.go, internal/orders/order_tracking.go
- **Test-first:** TestSQLiteOrderStore_GoldenRoundTrip (subtest a): create an OrderTracking for scoped name X with order-run label + custom metadata, read back, assert key-for-key Metadata equality incl. unknown passthrough, and assert the recency-by-scoped query uses an indexed column.
- **Acceptance:**
  - SQLite schema: order_tracking table with indexed scoped_name, run label, created_at + versioned extras blob; NO UNIQUE(scoped_name,run_key)
  - Implements the P1 OrderStore surface incl. recency query and stale-sweep support; var _ OrderStore assertion compiles
  - gco- prefix; city-scope single sequence
  - go vet clean
- **Depends on:** P4-T2
- **Risk:** OrderTracking is a NEW struct (P1); if its field set under-models a metadata key the order gate reads, single-flight breaks. The golden round-trip unknown-passthrough subtest is the guard.

#### P4-T9 — Implement typed single-flight query reproducing hasOpenWorkStrict over OrderStore + graph read-path in storesForGate  _(size M)_
- **Intent:** Re-spec single-flight as a typed Go query (not a bare UNIQUE) reproducing hasOpenWorkStrict (order_dispatch.go:498-605: open tracking OR wisp root with open descendants, EXCLUDING orphan all-closed roots). The gate reads graph-resident wisp roots — hasOpenWorkInStoresStrict already exists (order_dispatch.go:605/1750-ish); storesForGate (order_dispatch.go:498) must include the graph read-path so the gate sees wisp roots after orders move to SQLite. Hard prereq + gate conformance (DESIGN §10 P4 row).
- **Files:** cmd/gc/order_dispatch.go, internal/orders/sqliteorders/sqliteorders.go, cmd/gc/order_dispatch_gate_test.go
- **Test-first:** TestOrderSingleFlightAcrossStores in cmd/gc/order_dispatch_gate_test.go: with orders in SQLite and wisp roots in the graph store, assert (a) an open SQLite tracking row suppresses a second dispatch, (b) a graph-resident wisp root with open descendants suppresses dispatch, (c) an orphan all-closed root does NOT suppress (matches hasOpenWorkStrict), (d) storesForGate includes both the order store and the graph read path.
- **Acceptance:**
  - Typed Go single-flight query over OrderStore reproduces hasOpenWorkStrict semantics exactly (open tracking OR open-descendant wisp root, excluding orphan all-closed)
  - storesForGate includes the graph read-path so cross-store wisp-root detection still works post-move (hasOpenWorkInStoresStrict reused)
  - Gate conformance test green for all three branches incl. the orphan-exclusion edge
  - go vet clean
- **Depends on:** P4-T8, P3.5-resolver
- **Risk:** Mis-reproducing the orphan-all-closed exclusion causes either duplicate order dispatch (storm) or permanent suppression (orders never fire). This is the single hardest correctness point in the orders cutover — the gate conformance test must cover every hasOpenWorkStrict branch.

#### P4-T10 — Run order conformance (a-i) + cut orders over: drain-then-switch, controller-mediated writes  _(size M)_
- **Intent:** Run the full a-i conformance suite against the SQLite OrderStore with Skip:false, then flip [beads.classes.orders].backend to sqlite via drain-then-switch (orders are NoHistory, verified bead_policy_store.go:326-327,333-334 — clean drain, no backfill). New tracking creates land in SQLite (gco-) through the controller; in-flight bd tracking closes naturally; bd-hook flips atomically with in-process emit. Observe a stabilization window for fork-rate attribution.
- **Files:** internal/orders/sqliteorders/conformance_test.go, cmd/gc/api_state.go, cmd/gc/order_dispatch.go, cmd/gc/infra_cutover_test.go
- **Test-first:** TestSQLiteOrders_Conformance (subtests a-i, Skip:false) PLUS TestOrderCutoverDrainThenSwitch: assert new tracking writes a gco- SQLite row + zero new bd tracking beads, in-flight bd tracking readable until close, single event path per create, single-flight gate still suppresses correctly across the boundary.
- **Acceptance:**
  - Conformance a-i green against SQLite OrderStore with Skip:false; identical suite still green against bd OrderStore
  - New tracking creates land in SQLite (gco-), zero new bd tracking beads after flip; in-flight bd tracking drains naturally
  - bd-hook removal for orders flips atomically with in-process emit (watch-coherence green)
  - Controller-owned retention sweep (P3.5-retention) handles closed SQLite tracking rows; per-process order sweep disabled to avoid double-sweep
  - doctor_fork_rate shows orders' high-churn bd contribution dropping; stabilization window observed
- **Depends on:** P4-T9, P3.5-write-api, P3.5-retention, P2-autoclose-rehome, P4-T6
- **Risk:** ACCEPTED PROD-DIVERGENCE RISK: first order-gate divergence is discovered in production (could be a dispatch storm or stall); revert is quiesce-then-flip-back at a maintenance window. Orders are ~3,500/day high-churn — the single-flight gate must be airtight before flip. Retention-sweep ownership transfer (per-process→controller) is a second moving part in this same flip.

#### P4-T11 — Delete orders' readyExcludeTypes entry (separate revertible commit + deletion test)  _(size S)_
- **Intent:** After orders leave the work store, delete the order-tracking exclusion from IsReadyExcludedBead's label switch (beads.go:243: "gc:order-tracking","order-tracking") as a SEPARATE revertible commit. The deletion proves the split is complete. Deletion test asserts the work store never receives an order-tracking bead and Work().Ready is unchanged.
- **Files:** internal/beads/beads.go, internal/beads/beads_test.go
- **Test-first:** TestOrderExclusionDeletion: assert (a) "order-tracking"/"gc:order-tracking" no longer trigger IsReadyExcludedBead, (b) the work store never receives an order-tracking bead post-cutover, (c) Work().Ready unchanged over a fixture.
- **Acceptance:**
  - order-tracking label arms removed from IsReadyExcludedBead in a commit touching only the exclusion + its test (revertible)
  - Deletion test green: work store never receives order-tracking beads; Work().Ready unchanged
  - go vet clean; make test green
- **Depends on:** P4-T10
- **Risk:** Must land strictly after drain confirmed; premature deletion leaks still-resident bd order-tracking beads into Ready.

#### P4-T13 — Fix the nudge oracle-leak BEFORE relocation (readyExcludeTypes + born-unrouted proof)  _(size S)_
- **Intent:** DESIGN §9.10 hard prereq: type=chore is NOT in readyExcludeTypes and gc:nudge is NOT in IsReadyExcludedBead's label switch (verified beads.go:177-247) — nudges stay non-claimable today only via never carrying gc.routed_to + the ephemeral-scan boundary. Add the exclusion (chore type and/or gc:nudge label) AND a born-unrouted conformance proof, BEFORE the nudge SQLite flip, so relocation cannot expose a claimable nudge.
- **Files:** internal/beads/beads.go, internal/beads/beads_test.go
- **Test-first:** TestNudgeOracleLeakFixed in internal/beads/beads_test.go: (a) assert a type=chore+gc:nudge bead is now IsReadyExcludedBead==true, (b) born-unrouted proof: a freshly created nudge mirror bead never carries gc.routed_to AND is excluded, (c) regression: a non-nudge chore (if any legitimate one exists) is handled per spec.
- **Acceptance:**
  - gc:nudge label (and/or type=chore guard) added to IsReadyExcludedBead so nudge beads are excluded by the oracle predicate, not just by routing-metadata absence
  - Born-unrouted conformance proof green: nudge mirror beads are created without gc.routed_to AND are excluded
  - Lands BEFORE P4-T15 (the nudge flip) — sequencing enforced via dependsOn
  - go vet clean; make test green; Work().Ready unchanged for non-nudge beads
- **Depends on:** —
- **Risk:** If a legitimate claimable chore type exists in any pack, a blanket type=chore exclusion would hide it — prefer the gc:nudge label arm over a type=chore blanket unless the census confirms chore is nudge-only. Verify against coordclass golden table before choosing.

#### P4-T14 — Implement SQLite NudgeStore impl (real unique index on nudge_id, gcn- prefix) + conformance a-i  _(size M)_
- **Intent:** Build the second NudgeStore impl over SQLiteStore behind the P1 nudgequeue interface (over nudgequeue.Item). The flock-file queue stays source-of-truth; the SQLite table is its durability SHADOW (DESIGN §4) — this is a mirror write, not a queue replacement. nudge_id is genuinely unique → a real UNIQUE index is fine (DESIGN §7 idempotency). gcn- prefix, city-scope single sequence. Run a-i conformance with Skip:false.
- **Files:** internal/nudgequeue/sqlitenudge/sqlitenudge.go, internal/nudgequeue/sqlitenudge/sqlitenudge_test.go, internal/nudgequeue/sqlitenudge/conformance_test.go
- **Test-first:** TestSQLiteNudge_UniqueIndexIdempotent (subtest f + a): ensure-by-nudge_id twice writes one row (UNIQUE index enforces single-flight; second write maps to typed ErrRetryableContention or is a no-op upsert per spec); golden round-trip asserts key-for-key Item fields incl. unknown passthrough.
- **Acceptance:**
  - SQLite schema: nudges table with UNIQUE index on nudge_id + indexed terminal/TTL columns + versioned extras blob
  - Implements the P1 NudgeStore surface (ensure-by-nudge_id, terminalize, TTL sweep); var _ NudgeStore assertion compiles
  - Conformance a-i green with Skip:false; identical suite still green against bd NudgeStore
  - gcn- prefix; flock queue remains source-of-truth (table is shadow); cross-store FK resolve to bd sessions (subtest h) green
  - go vet clean
- **Depends on:** P4-T2, P4-T13, P2-rowchanged-emit, P3.5-resolver
- **Risk:** The shadow-vs-source-of-truth split means the SQLite write must stay consistent with the flock queue; a divergence between flock state and SQLite shadow is a silent durability gap. Conformance must assert shadow == flock after each mirror write.

#### P4-T15 — Cut nudges over: drain-then-switch shadow mirror bd→SQLite + verify nudge exclusion holds  _(size M)_
- **Intent:** Flip [beads.classes.nudges].backend to sqlite. nudges are no_history (bead_policy_store.go:326-327,333-334) — clean drain-then-switch: stop creating bd nudge shadow beads, let in-flight close, switch new shadow mirror writes to SQLite (gcn-) through the controller; bd-hook flips atomically with in-process emit. Verify the gc:nudge oracle exclusion (P4-T13) holds post-flip via a deletion-style test. Observe the stabilization window.
- **Files:** cmd/gc/api_state.go, internal/nudgequeue/state.go, cmd/gc/infra_cutover_test.go, internal/beads/beads_test.go
- **Test-first:** TestNudgeCutoverDrainThenSwitch: assert (a) new nudge mirror writes a gcn- SQLite row + zero new bd nudge shadow beads, (b) the flock queue remains source-of-truth and stays consistent with the SQLite shadow across the flip, (c) in-flight bd nudge shadows drain naturally, (d) single event path per write, (e) the gc:nudge oracle exclusion (P4-T13) holds post-flip.
- **Acceptance:**
  - New nudge shadow creates land in SQLite (gcn-), zero new bd nudge shadow beads after flip; flock queue unchanged as source-of-truth
  - bd-hook removal for nudges flips atomically with in-process emit (watch-coherence green)
  - Controller-mediated writes; reads on read-only WAL; controller-owned sweep handles closed SQLite nudge rows
  - Nudge oracle-leak guard (P4-T13) verified still effective post-cutover via a deletion-style test
  - doctor_fork_rate shows nudges' high-churn bd contribution dropping; full P4 fork-rate soak meets recovered ga-aec8q p99 targets
- **Depends on:** P4-T13, P4-T14, P3.5-write-api, P3.5-retention, P2-autoclose-rehome, P4-T10
- **Risk:** ACCEPTED PROD-DIVERGENCE RISK: first nudge divergence found in production; revert is quiesce-then-flip-back at a maintenance window. Highest-subtlety risk: flock-source-of-truth vs SQLite-shadow drift during the flip window — a crash mid-flip could leave the shadow behind the flock state. The atomic bd-hook flip and the flock/shadow consistency assertion are load-bearing.

#### P4-T16 — P4 fork-rate soak + per-class flip-back integrity tests + accepted-risk documentation  _(size M)_
- **Intent:** Close the phase: run the doctor_fork_rate soak across all three flipped classes against the recovered ga-aec8q p99 targets (the contention-win oracle, DESIGN §6 owner decision 6), apply the quiesce-then-flip-back integrity test (P4-T3 mechanism) to each of mail/orders/nudges, and document the accepted prod-divergence risk at each cutover in the design/engdocs trail.
- **Files:** cmd/gc/doctor_fork_rate.go, cmd/gc/infra_flip_back_test.go, engdocs/plans/infra-store-decouple/DESIGN.md
- **Test-first:** TestP4FlipBackIntegrityAllClasses in cmd/gc/infra_flip_back_test.go: for each of mail/orders/nudges, quiesce-then-flip-back and assert no rows lost, projections equal, no SQLITE_BUSY escape — reusing the P4-T3 graph-class helper.
- **Acceptance:**
  - doctor_fork_rate soak green vs recovered ga-aec8q p99 targets with mail+orders+nudges on SQLite (the contention win is measured, not assumed)
  - Flip-back integrity test green for each of the three classes (revert mechanism proven per class)
  - Accepted prod-divergence risk documented at each cutover (no in-prod oracle; quiesce-then-flip-back at maintenance window) in DESIGN/engdocs
  - All CI gates green: go vet, make test, TestOpenAPISpecInSync, make dashboard-check, TS-schema-drift gate, Vitest, TestEveryKnownEventTypeHasRegisteredPayload, TestGCNonTestFilesStayOnWorkerBoundary
  - Per-class conformance suites a-i remain green against both bd and SQLite impls
- **Depends on:** P4-T7, P4-T11, P4-T15, P4-T3
- **Risk:** If the soak does NOT meet p99 targets after all three flips, DESIGN §12 raises whether sessions/graph relocation (P5/P6) is even needed — but that is a P5 decision; P4 must still report the measured numbers honestly rather than declare success.

### P5 — SQLite cutover — sessions (live-adopt)

> Cut sessions over to a fork-owned SQLite SessionStore (typed lifecycle columns, gcs- prefix, persisted-subset row codec) without a beads/bd backend, carrying the canonical-election + closed-name-release logic forward as a reconciler invariant (no bare UNIQUE), re-pointing every session WRITER and READER through the typed store, bridging the WAIT->nudge cross-store withdrawal on close, and performing a controller-coordinated atomic live-adopt copy of all OPEN sessions with post-copy projection-equality verification over a LIVE corpus. This is the point of no return: the open-session metadata long-tail cannot be reconstructed by crash-adoption (which only re-attaches the process).

#### P5-T1 — Add gcs- session prefix to the (storeRef,id) resolver with longest-prefix precedence  _(size S)_
- **Intent:** Sessions become a cross-store FK target (mail/orders resolve session FKs; workflow-finalize resolves by storeRef). Register the non-configurable gcs- prefix in the prefix resolver built in P3.5 so a session id routes to the SessionStore and a dangling-but-matching id returns not-found cleanly rather than mis-routing into the work store. This is a hard prerequisite of any physical session move.
- **Files:** internal/dispatch/runtime.go, cmd/gc/api_state.go
- **Test-first:** TestResolveStoreRef_SessionPrefixRoutesToSessionStore in cmd/gc (or dispatch): a 'gcs-...' ref resolves to the SessionStore handle; a matching-prefix-but-absent id returns a clean not-found (no panic, no fallthrough to work store); an unrecognized prefix never silently routes to sessions. Assert longest-prefix precedence so gcs- is not shadowed by gc-.
- **Acceptance:**
  - ResolveStoreRef (internal/dispatch/runtime.go:48-63) returns the SessionStore for gcs- ids and not-found for dangling gcs- ids
  - gcs- is validated against rig.EffectivePrefix() as non-configurable, mirroring graphStoreIDPrefix='gcg' (api_state.go:268)
  - go vet ./... clean; make test green for internal/dispatch and cmd/gc resolver tests
  - Conformance subtest (h) cross-store FK resolve + dangling passes for the session prefix
- **Depends on:** P3.5-resolver
- **Risk:** Prefix collision precedence; gcs- accidentally shadowed by gc-. Mitigated by the longest-prefix rule and the dangling-id test.

#### P5-T2 — Define typed SQLite session schema (lifecycle columns + versioned extras blob)  _(size M)_
- **Intent:** Promote only the fields a store predicate selects/orders on or a wire consumer needs typed (session_name, state, closed, template, alias, agent_name, provider, work_dir, created_at, last_active, generation, pool_slot) to typed SQLite columns; route every other churny metadata key (~50 keys) through the per-entity versioned typed extras blob {v int; known typedStruct; unknown map[string]string}. No map[string]any. This is the persisted-subset codec backing the SessionStore.
- **Files:** internal/session/sqlite_schema.go, internal/session/sqlite_schema_test.go, internal/session/codec.go
- **Test-first:** TestSessionRowCodec_ReconstructUnionEquality: encode a session.Info-derived persisted subset + a populated metadata map into the row (promoted columns + extras.known + extras.unknown), decode, and assert key-for-key equality of the reconstructed metadata map vs the original; assert NO key lives in both known and unknown (double-write/drift guard).
- **Acceptance:**
  - Reconstruct contract (DESIGN §4): row -> domain unions promoted columns + extras.known + extras.unknown into one metadata map, asserted key-for-key equal to original
  - extras blob is versioned typed struct, never map[string]any; unknown passthrough captures every un-enumerated metadata key losslessly
  - Schema declares typed columns + indexes on session_name and state; no UNIQUE(session_name)
  - go vet clean; codec_test + schema_test green
- **Depends on:** —
- **Risk:** Missing a churny key from the persisted subset silently loses session metadata at cutover (point of no return). Mitigated by the unknown-passthrough catch-all and the key-for-key reconstruct test over a live corpus (P5-T11).

#### P5-T3 — Implement SQLiteSessionStore over the beads SQLiteStore substrate (gcs- prefix, MaxOpenConns=1 writer)  _(size M)_
- **Intent:** Build the SQLite SessionStore implementation of the domain-typed session.SessionStore interface (declared in P1) on internal/beads/sqlite_store.go's proven substrate, using the persisted-subset codec from P5-T2, the gcs- id prefix, retention disabled (controller-owned sweep), and a single in-process writer. Keep infoFromBead impure in Manager (Layer 2) — only the pure persisted-field decode lives at this row edge; runtime enrichment (transport/ACP/IsRunning/Attached/LastActive) stays in Manager.
- **Files:** internal/session/sqlite_store.go, internal/session/sqlite_store_test.go
- **Test-first:** TestSQLiteSessionStore_CreateGetRoundTrip: Create a session via the typed store, Get it back, assert the persisted subset round-trips byte-identically through the codec; assert the store mints gcs- ids and never re-derives ownership/classification (no Classify call on the write path).
- **Acceptance:**
  - SQLiteSessionStore satisfies the session.SessionStore interface from P1 (constructor-injected, no central router)
  - Opens with WithSQLiteStoreIDPrefix('gcs') and WithSQLiteStoreRetention(0,0), MaxOpenConns=1 write conn, mirroring registerGraphStoreBackend (api_state.go:326)
  - infoFromBead stays an impure *Manager method (manager.go:1543); only ProjectLifecycle-relevant persisted decode lives at the row edge
  - go vet clean; sqlite_store_test green
- **Depends on:** P5-T2
- **Risk:** Accidentally pulling runtime enrichment into Layer 0 (upward dep). Mitigated by a layering test asserting the store package does not import runtime.Provider liveness calls.

#### P5-T4 — Carry session_name uniqueness as a reconciler invariant (canonical-election + closed-name-release) into the SQLite path  _(size L)_
- **Intent:** There is NO bare UNIQUE(session_name). Port the duplicate-then-elect canonical-election logic (canonicalDuplicateSessionBead, beadOwnsPoolSessionName, indexSessionBeadsByName, retireDuplicateConfiguredNamedSessionBeads, namedSessionBeadWinsCanonicalRepair) and the closed-name-release path (reopenClosedConfiguredNamedSessionBead) verbatim onto the typed SessionStore so the reconciler still elects one canonical bead per session_name and releases closed names. Optionally back it with a partial unique index scoped to OPEN+canonical rows only.
- **Files:** internal/session/canonical_election.go, internal/session/canonical_election_test.go, cmd/gc/session_beads.go
- **Test-first:** TestCanonicalElection_SQLite_DuplicateThenElect: insert two open rows sharing a session_name (one pool-owning, one not), run the election reconcile, assert exactly the name-owning row survives canonical and the loser is retired; TestClosedNameRelease_SQLite asserts a closed canonical name is releasable and a fresh bead can reclaim it.
- **Acceptance:**
  - Election logic from session_beads.go (canonicalDuplicateSessionBead:106, beadOwnsPoolSessionName:119, retireDuplicateConfiguredNamedSessionBeads:403, namedSessionBeadWinsCanonicalRepair:487) reproduces identical winner selection against the typed store
  - NO UNIQUE(session_name) constraint; uniqueness is a reconciler invariant (optional partial index scoped to OPEN+canonical only)
  - closed-name-release (reopenClosedConfiguredNamedSessionBead:297) works against the typed store
  - Existing session-reconciler tests stay green; go vet clean
- **Depends on:** P5-T3
- **Risk:** A bare UNIQUE would reject the legitimate duplicate-then-elect transient and wedge the pool. Splitting cannot go smaller than L because the election + closed-name-release logic is one coherent invariant spanning ~6 functions in session_beads.go; smaller splits would land a half-ported invariant. Mitigated by porting verbatim and the duplicate-then-elect test.

#### P5-T5 — Re-point session.Manager writes to the typed SessionStore  _(size L)_
- **Intent:** Replace the Manager's beads.Store write surface (m.store.Create/SetMetadata/SetMetadataBatch/Update/Close at manager.go:452,469,578,716,856,874,939,954,971,1176,1187,1225,229) with calls onto the injected typed SessionStore. infoFromBead stays impure in Manager; ProjectLifecycle (lifecycle_projection.go:374) is unchanged. Constructor injection only — no central router sniff.
- **Files:** internal/session/manager.go, internal/session/manager_test.go
- **Test-first:** TestManager_WritesGoThroughSessionStore: spy SessionStore; drive Create/BeginDrain/Archive/Quarantine/Reactivate/ConfirmStarted; assert every lifecycle mutation lands on the typed store and none falls through to a raw beads.Store; assert infoFromBead still enriches transport/Attached/LastActive from the provider.
- **Acceptance:**
  - All m.store.* write sites in manager.go route through the typed SessionStore
  - infoFromBead remains an impure *Manager method (manager.go:1543); ProjectLifecycle unchanged and pinned by projection-invariance
  - manager_test, manager_states_test, lifecycle_* tests stay green
  - go vet clean; make test green for internal/session
- **Depends on:** P5-T3
- **Risk:** Missing one write site leaves a session mutation on bd, splitting the source of truth. Mitigated by grep over m.store.* and the spy-store test asserting zero raw beads.Store writes.

#### P5-T6 — Re-point session_beads.go setMeta/setMetaBatch and session_wake.go PreWakePatch writers to the typed store  _(size M)_
- **Intent:** Re-point the remaining cmd/gc session WRITERS enumerated in DESIGN P5: session_beads.go setMeta/setMetaBatch call sites (lines ~383,455,525,1156,1410,1431,1704) and the syncSessionBeads create path, plus session_wake.go's PreWakePatch -> SetMetadataBatch (session_wake.go:68). These must write through the typed SessionStore (or the controller-mediated write API for non-controller processes), not a generic beads.Store.
- **Files:** cmd/gc/session_beads.go, cmd/gc/session_wake.go, cmd/gc/session_beads_test.go
- **Test-first:** TestSyncSessionBeads_WritesThroughSessionStore and TestPreWakePatch_WritesThroughSessionStore: assert setMetaBatch and the PreWakePatch commit (session_wake.go:60-68) land on the typed SessionStore; assert PreWakePatch metadata (generation/instance_token/continuation_epoch reset) round-trips through the persisted-subset codec.
- **Acceptance:**
  - setMeta/setMetaBatch and syncSessionBeads creates route through the typed store
  - PreWakePatch (session_wake.go) commits via the typed store; FreshWake conversation-reset keys survive the codec round-trip
  - P3.5 grep-guard against new generic-store infra writes in cmd/gc non-test files stays green
  - session_beads_test + session_wake tests + session_reconciler_restart_request_test stay green; go vet clean
- **Depends on:** P5-T3, P5-T4
- **Risk:** session_beads.go is 2438 LOC with many writer sites; touching >5 files is avoided by scoping to the two writer files + their tests. Missing a setMetaBatch site silently keeps a writer on bd.

#### P5-T7 — Re-point all session READERS (status/doctor/demand-scan/list/adoption-barrier) to the typed store or federation accessor  _(size M)_
- **Intent:** Complete the P3.5 reader inventory for sessions: re-point loadSessionBeads/snapshotOrLoadSessionBeads/findOpenSessionBeadBySessionName (session_beads.go:37,56,63), the adoption barrier's session listing (adoption_barrier.go), gc session list, and any status/doctor session reads from the generic beads.Store to the typed SessionStore read surface (own read-only WAL handle per process). Reads must not open the work store for sessions.
- **Files:** cmd/gc/session_beads.go, cmd/gc/adoption_barrier.go, internal/session/list_all.go, cmd/gc/session_beads_test.go
- **Test-first:** TestSessionReaders_UseSessionStore: assert loadSessionBeads/findOpenSessionBeadBySessionName and runAdoptionBarrier's listing read from the typed SessionStore (own read-only handle) and never from the work store; assert openSessionBeadExists (adoption_barrier.go:275) queries the session store.
- **Acceptance:**
  - All cmd/gc + internal/session session readers route through the typed store read surface (read-only WAL handle per process)
  - Adoption barrier (re-attach-only crash adoption) lists/checks open beads via the session store; behavior is byte-identical pre-cutover
  - P3.5 grep-guard against new generic-store infra reads in cmd/gc non-test files stays green
  - go vet clean; reader tests green
- **Depends on:** P5-T3
- **Risk:** A missed reader keeps querying the (now-empty) work store and sees zero sessions, breaking status/adoption. Mitigated by the P3.5 grep-guard and the reader-routing test.

#### P5-T8 — Bridge WAIT->nudge cross-store withdrawal on session close  _(size M)_
- **Intent:** Durable session WAITs (type=gate + gc:wait) ride in the SessionStore but their queued nudge shadows live in the NudgeStore (relocated in P4). The session-close path (CancelWaitsAndCollectNudgeIDs + WithdrawWaitNudges) must terminalize the wait-linked nudge shadows across the store boundary — pass the nudge store explicitly or resolve via the gcs-/gcn- prefix resolver. Sessions and nudges are now in different stores.
- **Files:** internal/session/waits.go, cmd/gc/cmd_wait.go, internal/nudgequeue/waits.go, internal/session/waits_test.go
- **Test-first:** TestSessionClose_TerminalizesWaitNudgesAcrossStores: with WAITs in the SessionStore and nudge shadows in the NudgeStore, close a session and assert WithdrawWaitNudges (cmd_wait.go:1443) terminalizes the wait-linked nudge shadows in the separate nudge store; assert no nudge shadow is orphaned and the queue stays intact on a mark-terminal failure (preserve existing semantics).
- **Acceptance:**
  - Session-close path passes/resolves the nudge store; WithdrawWaitNudges (nudgequeue/waits.go:16) terminalizes cross-store wait nudges
  - Existing wait/nudge withdrawal failure semantics preserved (queue kept on failure)
  - Conformance: session close terminalizes its wait-linked nudge shadows when sessions and nudges are in different stores
  - cmd_nudge_test / cmd_wait_test withdrawal tests stay green; go vet clean
- **Depends on:** P5-T1, P5-T5
- **Risk:** If the close path silently no-ops withdrawal when the nudge store is absent, wait nudges leak forever. Mitigated by the cross-store close conformance test and not swallowing the missing-store error.

#### P5-T9 — Wire Layer-2/3 typed session lifecycle event from the Layer-0 RowChanged emit  _(size M)_
- **Intent:** The relocated SQLite SessionStore fires no bd hooks. Wire WithSQLiteStoreRecorder (added in P2) on the session store so committed mutations emit a raw RowChanged{StoreRef,ID,Op} OUTSIDE the retry closure/after Unlock; translate it in Layer 2/3 into the existing SessionLifecyclePayload (internal/api/event_payloads.go) so bead.{created,updated,closed} watch coherence is preserved and TestEveryKnownEventTypeHasRegisteredPayload stays green.
- **Files:** cmd/gc/api_state.go, internal/api/event_payloads.go, internal/session/sqlite_store.go, internal/api/session_event_test.go
- **Test-first:** TestSessionStore_WatchCoherence: subtest (c) — drive Create/Update/Close on the SQLite SessionStore and assert a SessionLifecyclePayload event is emitted for each within the latency bound, the store never goes silent on a mutation, and emission happens after commit (subtest d, emit-after-commit ordering).
- **Acceptance:**
  - Layer-0 emits only raw RowChanged; SessionLifecyclePayload is built in Layer 2/3 (no upward dep)
  - Emission is outside retryOnBusy / after Unlock; sync emit is a latency opt over the serve-loop timer floor
  - TestEveryKnownEventTypeHasRegisteredPayload green; watch-coherence (c) + emit-after-commit (d) conformance subtests green
  - go vet clean
- **Depends on:** P5-T3
- **Risk:** In-closure emit with MaxOpenConns=1 deadlocks. Mitigated by emit-after-commit-pre-return and the ordering subtest.

#### P5-T10 — Inject SQLite SessionStore at internal/api/session_manager.go and the controller composition root  _(size M)_
- **Intent:** Thread the SQLite SessionStore into the per-request session.Manager construction at internal/api/session_manager.go:11 (sessionManager) and into the controller composition root in cmd/gc — NOT at cmd/gc/api_state.go top-level, per DESIGN §3/§P1. Keep the SessionStore OUTSIDE the work CachingStore (same rule as graph) so projection never sees a stale snapshot. Worker-boundary compliance must stay green.
- **Files:** internal/api/session_manager.go, cmd/gc/api_state.go, internal/api/session_manager_test.go
- **Test-first:** TestSessionManager_InjectsSQLiteSessionStore: assert sessionManager() constructs a session.Manager backed by the typed SQLite SessionStore (not the work beads.Store) when sessions are flagged onto SQLite; assert the store is constructed outside the work CachingStore.
- **Acceptance:**
  - SessionStore injected at internal/api/session_manager.go (per-request Manager construction), config-flagged per class
  - SessionStore kept OUTSIDE the work CachingStore (api_state.go:186-197 rule)
  - TestGCNonTestFilesStayOnWorkerBoundary stays green (production cmd/gc routes through worker.Handle; no new session.NewManager bypass)
  - go vet clean; make dashboard-check green (no wire change here); make test green
- **Depends on:** P5-T5, P5-T9
- **Risk:** Injecting at api_state.go top-level instead of per-request session_manager.go violates the layering decision and the worker-boundary test. Mitigated by the boundary test and injecting at the prescribed seam.

#### P5-T11 — Controller-coordinated atomic live-adopt copy of OPEN sessions with post-copy projection-equality over a LIVE corpus  _(size L)_
- **Intent:** Point of no return (DESIGN §8.2): the open-session metadata long-tail cannot be reconstructed by crash-adoption (which only re-attaches the process — adoption_barrier.go). Implement a controller-locked atomic copy of all OPEN session beads from bd into the SQLite SessionStore, then verify post-copy projection-equality (ProjectLifecycle output identical for every OPEN session) over a corpus dumped from a LIVE city before flipping. Closed sessions are not copied (clean drain).
- **Files:** cmd/gc/session_live_adopt.go, cmd/gc/session_live_adopt_test.go, internal/session/lifecycle_projection.go
- **Test-first:** TestLiveAdopt_ProjectionEqualityOverCorpus: load a live-dumped corpus of OPEN session beads, run the atomic copy under the controller lock, then assert for every session that ProjectLifecycle(bd-source) == ProjectLifecycle(sqlite-copy) key-for-key (subtest e projection-invariance over the persisted subset) AND reconstruct-union equality (subtest b); assert the copy is atomic (all-or-nothing under lock) and idempotent on re-run.
- **Acceptance:**
  - Atomic controller-locked copy of OPEN sessions only; closed sessions drain naturally (no copy-back needed for them)
  - Post-copy projection-equality verified over a corpus dumped from a LIVE city (golden round-trip a + reconstruct-union b + projection-invariance e all green)
  - Re-running the adopt is idempotent (no duplicate gcs- rows; canonical-election from P5-T4 holds)
  - Live-adopt test green; documented as a point-of-no-return at the cutover site
- **Depends on:** P5-T4, P5-T5, P5-T6, P5-T7
- **Risk:** Highest-risk item in the phase: a single missed metadata key or a non-atomic copy permanently loses open-session state with no mid-flight rollback (accepted prod risk, decision 6). Cannot be smaller than L — it spans the lock protocol, the copy, and the full projection-equality verification, which must land together to be safe. Mitigated by the unknown-passthrough codec, the live-corpus projection-equality gate, and quiesce-then-flip-back revert.

#### P5-T12 — Run the full session conformance suite (incl. crash-adoption re-attach + live-adopt) against bd and SQLite impls  _(size L)_
- **Intent:** Stand up the per-class session conformance suite (modeled on coordtest RunClassedStoreTests / mailtest) and run the IDENTICAL suite against the bd-delegating SessionStore (P1) and the SQLite SessionStore. Must include the mandatory subtests a-i from DESIGN §7 plus session-specific crash-adoption (re-attach-only) and live-adopt fidelity tests. This is the only safety net under conformance-only validation.
- **Files:** internal/session/sessiontest/conformance.go, internal/session/sessiontest/conformance_test.go, internal/session/sqlite_store_conformance_test.go
- **Test-first:** RunSessionStoreTests covering: (a) golden round-trip incl. unknown passthrough; (b) reconstruct-union equality; (c) watch-coherence; (d) emit-after-commit ordering; (e) projection-invariance over persisted subset; (f) error classification UNIQUE-failed->ErrRetryableContention vs ErrHard; (g) readyExcludeTypes-deletion (work store never receives session/gc:session and Work().Ready unchanged); (h) cross-store FK resolve + dangling; (i) concurrent-process id non-collision + no SQLITE_BUSY escape; plus crash-adoption-reattach-only and live-adopt-projection-equality.
- **Acceptance:**
  - Identical suite runs against both the bd adapter and the SQLite SessionStore with Skip:false
  - All subtests a-i green; crash-adoption test asserts re-attach-only (no metadata reconstruction); live-adopt test asserts projection equality
  - concurrent-process subtest (i) asserts non-colliding gcs- ids and no SQLITE_BUSY escaping retryOnBusy
  - go vet clean; make test green
- **Depends on:** P5-T3, P5-T9, P5-T11
- **Risk:** A vacuous/skipped subtest gives false confidence under conformance-only validation. Mitigated by the coordtest pattern (suite's own test runs with Skip:false against a reference store to prove non-vacuity). Cannot be smaller than L — the nine mandatory subtests plus two session-specific ones are one coherent suite that must run identically against both impls.

#### P5-T13 — Cutover: drain-then-switch session creates to SQLite and delete readyExcludeTypes session + gc:session entries  _(size M)_
- **Intent:** Flip new session creates to the SQLite SessionStore (clean drain-then-switch: stop creating session beads on bd, let in-flight bd session beads close naturally, switch new creates to SQLite). Once sessions no longer live in the work store, delete the readyExcludeTypes['session'] entry (beads.go:177-187) and the gc:session label arm in IsReadyExcludedBead (beads.go:243) — the deletion IS the proof the split is complete. Flip the bd-hook removal atomically with the in-process event path so exactly one path is live.
- **Files:** internal/beads/beads.go, cmd/gc/api_state.go, internal/beads/beads_test.go
- **Test-first:** TestReadyExcludeDeletion_Session: subtest (g) — after cutover the work store never receives a session/gc:session bead and Work().Ready is unchanged with the exclusion entries removed; a session bead can no longer leak into the work Ready oracle because it is structurally never in the work store (not because of the exclusion).
- **Acceptance:**
  - readyExcludeTypes['session'] (beads.go:183) and the 'gc:session' arm (beads.go:243) deleted; deletion is a separate revertible commit
  - Drain-then-switch: no new bd session beads; in-flight bd session beads close naturally; new creates go to SQLite
  - bd-hook removal flips atomically with the in-process event path (no double-emit window)
  - Work().Ready unchanged; readyExcludeTypes-deletion conformance (g) green; revert documented as quiesce-then-flip-back
  - go vet clean; make test + TestOpenAPISpecInSync green
- **Depends on:** P5-T10, P5-T11, P5-T12
- **Risk:** Deleting the exclusion before sessions fully leave the work store would expose session beads to the Ready oracle. Strictly sequenced after live-adopt + conformance; the deletion test asserts structural absence, not just the exclusion.

### P6 — Graph finalize + Router retirement

> Finalize the graph class: promote a real domain-typed GraphStore (read + finalize methods) in a new internal/graphstore package, MOVE ClassifyGraphPlan (with classifyFields/isWispMetadata) into it, make ResolveStoreRef provider/prefix-aware, cut every remaining dispatch/molecule/API/order read path that still depends on coordrouter.Router federation over to GraphStore, then delete coordrouter.Router, demote coordclass.Classify to a test/audit census tool, and delete the graph readyExcludeTypes entries — the final point of no return.

#### P6-T1 — Create internal/graphstore package with GraphStore interface (apply + read/finalize methods)  _(size M)_
- **Intent:** P6 promotes GraphStore from a thin coordrouter seam (only beads.GraphApplyStore) into a real domain-typed interface in its own package. Declare GetNode/ListNodesByRoot/ListNodeEdges/CloseSubtree/ReadyCandidates/FindOrCreateByKey alongside the existing ApplyGraphPlan/ApplyGraphPlanWithStorage surface so dispatch/molecule callers can hold a typed graph store instead of a generic beads.Store routed through the Router.
- **Files:** internal/graphstore/graphstore.go, internal/graphstore/graphstore_test.go, internal/coordrouter/stores.go
- **Test-first:** internal/graphstore/graphstore_test.go: compile-time interface assertion test asserting graphstore.GraphStore embeds beads.GraphApplyStore, the six new method signatures exist with the exact arg/return shapes from DESIGN §7, and var _ graphstore.GraphStore = (*fakeGraphStore)(nil) for a test double. Fails because the package does not exist.
- **Acceptance:**
  - internal/graphstore compiles with GraphStore embedding beads.GraphApplyStore plus the six read/finalize methods; each has a doc comment naming the dispatch/molecule call site it replaces.
  - The growth-path comment block in internal/coordrouter/stores.go is updated to point at internal/graphstore as the methods' new home (no duplicate interface definitions).
  - go vet ./... clean; make test green for internal/graphstore and internal/coordrouter.
  - No production importer added yet (interface + test double only).
- **Depends on:** —
- **Risk:** Choosing method signatures that don't match existing beads.Store read shapes (DepList direction string, ListByMetadata gc.root_bead_id) forces churn later; mirror the exact shapes the Router federation already serves.

#### P6-T2 — Move ClassifyGraphPlan (carrying classifyFields/isWispMetadata) into internal/graphstore  _(size M)_
- **Intent:** DESIGN §2 and the P6 row mandate moving ClassifyGraphPlan out of coordclass into internal/graphstore so plan-wholesale graph routing lives with the graph class, carrying classifyFields and isWispMetadata. The only production caller is coordrouter.Router (router.go:151,167), deleted in P6-T10, so this move is grep-provable.
- **Files:** internal/graphstore/classify_plan.go, internal/graphstore/classify_plan_test.go, internal/coordclass/classify.go, internal/coordclass/classify_test.go
- **Test-first:** internal/graphstore/classify_plan_test.go: port TestClassifyGraphPlan from coordclass/classify_test.go:77-104 (nil plan default, empty plan, embedded graph-typed node -> graph, wisp-metadata root -> graph) asserting graphstore.ClassifyGraphPlan returns the graph determination per row. Fails until the function and its helpers exist in internal/graphstore.
- **Acceptance:**
  - graphstore.ClassifyGraphPlan plus unexported classifyFields/isWispMetadata (mirroring classify.go:88-160) live in internal/graphstore; the plan classifier is removed from internal/coordclass.
  - grep for coordclass.ClassifyGraphPlan across internal+cmd returns zero non-test hits except coordrouter.Router (router.go:151,167), updated/removed in P6-T10.
  - Graph-plan golden table coverage now runs in internal/graphstore; coordclass keeps only the per-bead Classify table.
  - go vet ./... clean; make test green for internal/graphstore and internal/coordclass.
- **Depends on:** P6-T1
- **Risk:** classifyFields is shared by coordclass.Classify and ClassifyGraphPlan; moving only the plan path must not break the per-bead Classify census. Establish exactly one source of truth for the graph arm (no double-encode).

#### P6-T3 — Implement GraphStore read/finalize methods on the SQLite graph backend  _(size L)_
- **Intent:** The graph class is already on SQLite (api_state.go:326 OpenSQLiteStore, gcg prefix). Implement the six new GraphStore methods on the SQLite-backed graph store so dispatch/molecule call them directly instead of going through generic beads.Store ops behind the Router. Reads map to existing primitives (Get, ListByMetadata gc.root_bead_id, DepList, Ready); CloseSubtree carries the wisp-subtree walk; FindOrCreateByKey promotes the racy striped-mutex idempotency.
- **Files:** internal/beads/sqlite_store_graph_apply.go, internal/beads/sqlite_store_graphstore.go, internal/beads/sqlite_store_graphstore_test.go
- **Test-first:** internal/beads/sqlite_store_graphstore_test.go: run graphstore conformance subtests against a real SQLiteStore — GetNode returns a poured node and ErrNotFound for absent ids; ListNodesByRoot returns exactly the gc.root_bead_id set across both tiers; ListNodeEdges matches DepList(id,down/up); CloseSubtree closes every open descendant and returns the count; ReadyCandidates surfaces routed steps by gc.routed_to never by type=step. Fails until methods exist.
- **Acceptance:**
  - *beads.SQLiteStore (or a thin adapter over it) satisfies graphstore.GraphStore; var _ assertion present.
  - ListNodesByRoot/ListNodeEdges return byte-identical sets to the ListByMetadata/DepList the runtime.go topology walks (runtime.go:1168,1290,1453) perform today, proven by a differential subtest.
  - FindOrCreateByKey reproduces today's List(open)+Create idempotency (molecule.findExistingAttach molecule.go:325 / drain item_root_key) with the cross-controller race kept verbatim behind beadstest.Options{Skip,Reason}.
  - Conformance subtests (a) golden round-trip, (h) cross-store FK resolve+dangling, (i) concurrent-process id non-collision green.
  - go vet ./... clean; make test green for internal/beads.
- **Depends on:** P6-T1
- **Risk:** CloseSubtree must match storeHasOpenDescendants walk semantics (parent-child/tracks/blocks, order_dispatch isOrderWispDescendantDepType) or autoclose/finalize counts drift. Reuse the existing walk.

#### P6-T4 — Cut dispatch runtime topology reads (DepList/Children walks) over to GraphStore  _(size M)_
- **Intent:** internal/dispatch/runtime.go reads graph topology via generic store.DepList (runtime.go:1168,1290,1453) and graph-only List via the GraphIDPrefix/ListGraphOnly capability (runtime.go:439-440), served by Router federation today. Re-point these to graphstore.GraphStore (ListNodeEdges/ListNodesByRoot/GetNode) so the runtime holds the graph class directly and no longer depends on Router federation for control-bead processing.
- **Files:** internal/dispatch/runtime.go, internal/dispatch/runtime_test.go
- **Test-first:** internal/dispatch/runtime_test.go: a control-bead test injecting a graphstore.GraphStore fake, asserting ProcessControl resolves node topology (down-deps at runtime.go:1168/1290/1453) through GraphStore.ListNodeEdges/ListNodesByRoot not generic beads.Store.DepList, and finalize/tally outcomes are byte-identical to the pre-cut path over the same fixture. Fails until the call sites switch.
- **Acceptance:**
  - runtime.go control-bead topology reads call graphstore.GraphStore; the GraphIDPrefix/ListGraphOnly dispatch at runtime.go:439-440 is replaced by a direct GraphStore.ListNodesByRoot (or a documented shim delegating to GraphStore).
  - Differential test: workflow-finalize / tally / drain produce identical close sets and ControlResult over the same fixture before/after.
  - go vet ./... clean; make test green for internal/dispatch.
  - No new direct beads.Store topology read added in internal/dispatch non-test files (grep guard).
- **Depends on:** P6-T1, P6-T3
- **Risk:** runtime.go assumes a single store for both work-source close and graph topology in workflow-finalize; the cut must keep cross-store source close (ResolveStoreRef, P6-T6) intact while only graph-topology reads move.

#### P6-T5 — Cut molecule idempotency + cleanup reads over to GraphStore  _(size M)_
- **Intent:** internal/molecule reads graph topology via store.ListByMetadata(gc.root_bead_id) and store.Children (cleanup.go:38,58) and resolves attach idempotency via findExistingAttach's DepList walk (molecule.go:325,347). Re-point these to graphstore.GraphStore.ListNodesByRoot / FindOrCreateByKey so the molecule engine uses the typed graph store and the racy striped-mutex idempotency is promoted to FindOrCreateByKey (verbatim, no race closure).
- **Files:** internal/molecule/molecule.go, internal/molecule/cleanup.go, internal/molecule/molecule_test.go
- **Test-first:** internal/molecule/molecule_test.go: TestAttachIdempotentViaGraphStore injecting a graphstore.GraphStore fake, asserting a repeated Attach with the same IdempotencyKey resolves the existing sub-DAG through GraphStore.FindOrCreateByKey (created=false on second call) and cleanup's logical-member enumeration goes through GraphStore.ListNodesByRoot. Fails until findExistingAttach/cleanup switch.
- **Acceptance:**
  - molecule.findExistingAttach and cleanup logical-member/Children enumeration call graphstore.GraphStore instead of generic beads.Store ListByMetadata/Children/DepList for the graph class.
  - The cross-controller idempotency race is NOT newly closed — existing striped-mutex behavior preserved verbatim behind FindOrCreateByKey with a beadstest.Options{Skip,Reason} on the race-closure subtest.
  - Differential test: a full graph.v2 pour + re-attach produces byte-identical beads/edges before/after.
  - go vet ./... clean; make test green for internal/molecule.
- **Depends on:** P6-T1, P6-T3
- **Risk:** findExistingAttach also reads work-typed embedded steps inside a graph (steps stay type=task to remain claimable); ListNodesByRoot must surface those by routing metadata not by type=step, or attach dedup misses them.

#### P6-T6 — Make ResolveStoreRef provider-aware via the P3.5 (storeRef,id) prefix resolver  _(size M)_
- **Intent:** DESIGN §9.3 and the P6 row require ResolveStoreRef to become provider-aware so cross-store workflow-finalize resolves graph (gcg-) vs work (gc-/ga-) vs relocated infra prefixes through the prefix resolver, rather than only the city:/rig: scheme switch in makeStoreRefResolver (cmd_convoy_dispatch.go:324). Re-point ResolveStoreRef to consult the P3.5 prefix resolver so a ref/id crossing the graph boundary resolves to the GraphStore handle. Keep the live-root guard under SourceWorkflowLock verbatim.
- **Files:** cmd/gc/cmd_convoy_dispatch.go, internal/dispatch/runtime.go, cmd/gc/cmd_convoy_dispatch_test.go
- **Test-first:** cmd/gc/cmd_convoy_dispatch_test.go: TestResolveStoreRefProviderAware asserting makeStoreRefResolver routes a graph-prefixed ref/id (gcg-) to the GraphStore-backed store and a work-prefixed ref (gc-/ga-) to the work store, an unrecognized prefix returns a clean error (never silently mis-routes), and a matching-prefix-but-absent row returns ErrNotFound cleanly. Fails until the resolver consults the prefix resolver.
- **Acceptance:**
  - makeStoreRefResolver resolves by the P3.5 (storeRef,id) prefix resolver in addition to city:/rig:; graph-resident roots resolve to the GraphStore handle.
  - Dangling-FK behavior pinned: matching-prefix-but-absent -> ErrNotFound; unrecognized prefix -> explicit error, never a silent wrong-store hit (conformance subtest h).
  - The workflow-finalize live-root guard and SourceWorkflowLock path stay byte-identical; differential test over an orphaned-source finalize fixture green.
  - go vet ./... clean; make test green for cmd/gc and internal/dispatch.
- **Depends on:** P3.5-resolver, P6-T3
- **Risk:** If the P3.5 prefix resolver is not yet landed, this item is BLOCKED — the explicit cross-phase dependency. The resolver must apply longest-prefix precedence (gcg- before gc-) or graph ids mis-resolve to the work store.

#### P6-T7 — Rewire GET /v0/beads?type=molecule workflow-root augment to GraphStore  _(size M)_
- **Intent:** DESIGN §5 and §10 (P6 row) require the ?type=molecule graph-workflow-root augment (huma_handlers_beads.go:113-201) — which surfaces gc.kind=workflow graph roots by federating each store's List today — to read from GraphStore BEFORE the Router/federation is deleted. Re-point the augment query to graphstore.GraphStore so the molecule view does not depend on Router federation.
- **Files:** internal/api/huma_handlers_beads.go, internal/api/huma_handlers_beads_test.go
- **Test-first:** internal/api/huma_handlers_beads_test.go: TestMoleculeAugmentReadsGraphStore asserting GET /v0/beads?type=molecule returns gc.kind=workflow graph roots sourced from a GraphStore injected into API state, with the existing global-dedupe-by-id behavior (huma_handlers_beads.go:186-200) preserved, and the augment no longer iterates rig stores via Router federation. Fails until re-pointed.
- **Acceptance:**
  - The qi>0 gc.kind=workflow augment query resolves through GraphStore not per-rig store.List federation; the primary type-filtered query path is unchanged.
  - Global dedupe-by-id of workflow roots (the workflow key) and best-effort (never-record-outage) semantics preserved.
  - TestOpenAPISpecInSync stays green (no wire shape change); molecule runs view shows identical roots before/after.
  - go vet ./... clean; make test green for internal/api.
- **Depends on:** P6-T1, P6-T3
- **Risk:** The augment runs per federated store today; collapsing to a single GraphStore must still cover a multi-HQ city's city-store roots (federate(city,...) at huma_handlers_beads.go:368) — confirm the single city-scope graph store holds all workflow roots (api_state.go:317 keys on cityPath).

#### P6-T8 — Rewire the order single-flight gate (hasOpenWorkStrict) graph read path to GraphStore  _(size M)_
- **Intent:** DESIGN §7 and §10 (P6 row, P4 prereq carried into P6) require the order single-flight gate to read graph-resident wisp roots via GraphStore before the Router is deleted. storesForGate (order_dispatch.go:498-605) and hasOpenWorkInStoresStrict (order_dispatch.go:1750) iterate beads.Store handles; hasOpenWorkStrict (order_dispatch.go:1468) walks order-run wisp subtrees. Re-point the graph leg to graphstore.GraphStore.ListNodesByRoot + descendant check so the gate resolves graph wisp roots typed, not via Router federation.
- **Files:** cmd/gc/order_dispatch.go, cmd/gc/order_dispatch_test.go
- **Test-first:** cmd/gc/order_dispatch_test.go: TestOrderGateReadsGraphStoreWispRoots asserting hasOpenWorkInStoresStrict, given a graph-resident open wisp root with open descendants, reports in-flight work via GraphStore and correctly ignores an orphan all-closed root (the ga-jra/ga-lo8c regression), without iterating the work store for the graph leg. Fails until the graph leg uses GraphStore.
- **Acceptance:**
  - The graph leg of storesForGate resolves through graphstore.GraphStore; the work leg stays on the work store; hasOpenWorkInStoresStrict union semantics preserved.
  - Regression pins green: orphan all-closed wisp root does NOT block re-dispatch (ga-jra/ga-lo8c); open-descendant wisp DOES (tr-kds01); orderGateTimeout fail-open unchanged.
  - Differential test: gate decision identical over a graph+work fixture before/after.
  - go vet ./... clean; make test green for cmd/gc order_dispatch.
- **Depends on:** P6-T1, P6-T3
- **Risk:** hasOpenWorkStrict unions both tiers (TierBoth) and depends on isOrderWispRootCandidate/isOrderRootOnlyWispCandidate heuristics; the GraphStore read must preserve the both-tier union and the orphan-vs-in-flight classification or duplicate wisps re-pour.

#### P6-T9 — Split-topology is_blocked conformance test (blocks edge + projection stay in Work store)  _(size S)_
- **Intent:** DESIGN §9.2 and the P6 row require a split-topology is_blocked conformance test proving that with the graph class physically separate from the work store, the blocks edge + is_blocked projection ALWAYS live in the Work backend and GraphStore never owns a ready-blocking dep — so the single demand oracle can never split. This is the safety net under Router deletion and the final point of no return.
- **Files:** internal/graphstore/conformance_isblocked_test.go, internal/coordrouter/coordtest/conformance.go
- **Test-first:** internal/graphstore/conformance_isblocked_test.go: TestSplitTopologyIsBlocked constructing a work bead blocked-by a graph root across the store boundary, asserting (a) the blocks edge is recorded in the Work store, (b) is_blocked is computed by the Work store projection, (c) GraphStore never holds a readyBlockingDependencyType dep (beads.readyBlockingDependencyTypes), (d) Work().Ready reflects the cross-store block. Fails until the assertion exists.
- **Acceptance:**
  - The split-topology is_blocked test runs against the SQLite GraphStore + a separate work store and is green.
  - GraphStore is forbidden from owning ready-blocking deps (blocks/waits-for/conditional-blocks); enforced and tested.
  - The test is wired into the coordtest/graphstore conformance suite so it runs in CI (make test).
  - go vet ./... clean.
- **Depends on:** P6-T3
- **Risk:** is_blocked is a Work-store projection; the test must not require GraphStore to recompute is_blocked (DESIGN rejects federating the projection). Pin edge+projection to the Work store, address the foreign graph target by (storeRef,id).

#### P6-T10 — Delete coordrouter.Router behind the three-mechanism zero-production-callers gate  _(size L)_
- **Intent:** DESIGN §2 and §10 (final point of no return): once every graph read path is on GraphStore (P6-T4,T5,T7,T8) and ResolveStoreRef is prefix-aware (P6-T6), the Router's three mechanisms (create-time classification router.go:113, by-id Get-probing router_mutation.go:21 backendForID, read federation router_federation.go:242 federateRead) are dead weight. Re-point the sole production New call (api_state.go:244, registerGraphStoreBackend, the Backends() peel at api_state.go:736) to construct/close the GraphStore + work store directly, then delete internal/coordrouter.
- **Files:** cmd/gc/api_state.go, internal/coordrouter/router.go, internal/coordrouter/router_federation.go, internal/coordrouter/router_mutation.go
- **Test-first:** cmd/gc/api_state_test.go (or a new guard test): TestNoProductionCoordrouterCallers asserting grep over internal+cmd non-test files finds zero coordrouter.New / coordrouter.Router references AND the graph_store=sqlite wiring constructs the GraphStore + work store directly and closes both. Fails while api_state.go still calls coordrouter.New.
- **Acceptance:**
  - api_state.go routedPolicyStore/registerGraphStoreBackend wire the work store and the SQLite GraphStore directly (no Router); the close-path peel (api_state.go:736-747) reaches both backends.
  - internal/coordrouter is deleted (router.go, router_federation.go, router_mutation.go, the Router-via stores.go GraphStore, bdgraphstore.go, all router_*_test.go); the three-mechanism zero-callers grep guard green.
  - graph_store=sqlite cities still route graph ops to the SQLite store and work ops to the work store (wiring test); default cities unchanged. The ReadyGraphOnly/ListGraphOnly/GraphIDPrefix consumers (cmd_ready.go:182, build_desired_state.go:1667, huma_handlers_beads.go:342, runtime.go:439) are re-pointed to the direct GraphStore wiring.
  - go vet ./... clean; make test, TestGCNonTestFilesStayOnWorkerBoundary, TestOpenAPISpecInSync, make dashboard-check all green.
- **Depends on:** P6-T4, P6-T5, P6-T6, P6-T7, P6-T8, P6-T9
- **Risk:** Final point of no return — deleting federation removes the per-class revert safety net. The graph-only capability accessors the Router satisfied (internal/beads/ready_graph_only.go, graph_only_list.go) must be re-provided by the direct GraphStore wiring or their consumers regress; inventory and re-point them here (may warrant a pre-T10 split if >5 files).

#### P6-T11 — Demote coordclass.Classify to a test/audit census tool (off the hot path)  _(size S)_
- **Intent:** DESIGN §2: coordclass.Classify(bead) is demoted to a test/audit-only census tool once the Router (its only hot-path caller via router.go:113,122) is deleted. Its golden table stays the spec for each adapter's ToBead/FromBead and the readyExcludeTypes deletion proofs (P6-T12). Move/retag it so no production path calls Classify; keep the golden table as the census oracle.
- **Files:** internal/coordclass/classify.go, internal/coordclass/class.go, internal/coordclass/classify_test.go
- **Test-first:** internal/coordclass/classify_test.go: TestClassifyIsCensusOnly asserting the golden classify table (one row per census kind) still passes and a grep-style guard asserts zero production (non-test) callers of coordclass.Classify across internal+cmd. Fails if any production caller remains after Router deletion.
- **Acceptance:**
  - coordclass.Classify has zero production callers (only tests/audit); a guard test enforces this.
  - The golden classify table remains green and is documented as the spec for adapter ToBead/FromBead + the readyExcludeTypes deletion proofs.
  - coordclass still compiles (Classify retained as a census/audit export or moved into a test/audit-scoped file); no role-name or judgment logic re-introduced.
  - go vet ./... clean; make test green for internal/coordclass.
- **Depends on:** P6-T10
- **Risk:** If Classify is fully deleted, the readyExcludeTypes deletion proofs (P6-T12) lose their golden-table reference. Demote, do not delete — keep the table as the audit oracle per DESIGN.

#### P6-T12 — Delete graph readyExcludeTypes entries (molecule, step, gate) with deletion-is-the-proof tests  _(size M)_
- **Intent:** DESIGN §9.10 and §7 conformance subtest (g): after the graph class fully leaves the work store (Router deleted, GraphStore owns graph topology), the graph entries in beads.readyExcludeTypes (molecule, step, gate — beads.go:177) are deleted per the deletion-is-the-proof discipline. Each deletion is gated by a test asserting the work store never receives that type and Work().Ready is unchanged. Mirror the deletion in the DoltLite read-store exclusion (doltlite_read_store.go:105).
- **Files:** internal/beads/beads.go, internal/beads/doltlite_read_store.go, internal/beads/beads_readyexclude_test.go
- **Test-first:** internal/beads/beads_readyexclude_test.go: TestGraphTypesLeaveReadyExclude asserting molecule/step/gate are removed from readyExcludeTypes, the work store never receives a graph-typed bead (Create of type=molecule/step/gate routes to GraphStore against the post-P6 wiring), and Work().Ready over a work-only fixture is byte-identical to the pre-deletion baseline. Fails until the entries are deleted and the routing guarantee holds.
- **Acceptance:**
  - molecule, step, gate removed from beads.readyExcludeTypes and from the DoltLite read-store exclusion (doltlite_read_store.go:105-106); message/session/agent/role/rig retained.
  - Per-type deletion test green: work store provably never receives molecule/step/gate after the cut, and Work().Ready unchanged over the work corpus.
  - Conformance subtest (g) readyExcludeTypes-deletion green for the graph class; TestEveryKnownEventTypeHasRegisteredPayload still green.
  - go vet ./... clean; make test green for internal/beads.
  - Each type removed in its own revertible commit (molecule, step, gate) per the deletion-is-the-proof discipline.
- **Depends on:** P6-T10
- **Risk:** Deleting step/molecule from the exclusion list while ANY graph bead still lands in the work store makes graph scaffolding claimable in the work backend (the #1039 hazard). The routing guarantee (graph types -> GraphStore only) is the precondition the test must prove before the entry is removed.

### P7 — extmsg follow-on (history backfill)

> Extract a fork-owned, domain-typed ExtMsgStore in internal/extmsg with per-family record types (8 gc:extmsg-* families, ~12 record types, all type=task on the HISTORY tier), wire it via constructor injection, prove it with per-family conformance, fix the latent type=task+gc:extmsg-* Ready-leak, then physically relocate to SQLite via a HISTORY BACKFILL (not clean-drain) with post-backfill verification. Every pre-relocation step (P7-T1..T8) is a byte-identical refactor on one bd backend; only P7-T13 onward moves data. Shippable as its own epic.

#### P7-T1 — Fix the type=task+gc:extmsg-* Ready-leak (exclusion + born-unrouted proof)  _(size S)_
- **Intent:** DESIGN §9.10 mandates extending the nudge Ready-leak fix to extmsg BEFORE relocation. Today extmsg beads are type=task with gc:extmsg-* labels and are NOT in readyExcludeTypes nor matched by IsReadyExcludedBead (internal/beads/beads.go:177-247 only matches gc:session/gc:order-tracking/order-tracking); they stay non-claimable only by never carrying gc.routed_to. Once relocated this latent leak must be closed structurally.
- **Files:** internal/beads/beads.go, internal/beads/beads_test.go, internal/extmsg/labels.go
- **Test-first:** In internal/beads/beads_test.go add TestExtmsgLabelIsReadyExcluded asserting IsReadyExcludedBead(beads.Bead{Type:"task", Labels:[]string{"gc:extmsg-binding"}}) == true, and the same for each of the 8 families (-delivery,-group,-group-participant,-membership,-participant,-transcript,-transcript-state); plus a born-unrouted assertion that an extmsg-classed bead created via the service never carries gc.routed_to. Watch it fail (current loop only matches gc:session/order-tracking).
- **Acceptance:**
  - IsReadyExcludedBead returns true for type=task beads carrying any gc:extmsg-* family label (use a HasPrefix on "gc:extmsg-", matching coordclass labelExtmsgPrefix, not 8 string literals)
  - Work().Ready over a fixture containing one bead per extmsg family is unchanged-minus-extmsg (the deletion-is-the-proof invariant from §9/§10 coupling 10)
  - go vet ./... and make test green; no other readyExcludeTypes entries altered
- **Depends on:** —
- **Risk:** Low. Pure additive predicate; the born-unrouted half is already true today, the test just pins it.

#### P7-T2 — Specify the ExtMsgStore domain interface (per-family record types, no methods ahead of callers)  _(size S)_
- **Intent:** DESIGN §11 + §3 Layer-1 rule: declare the fork-owned, domain-typed store interface IN the owning package (internal/extmsg), keyed by the existing record structs (SessionBindingRecord, DeliveryContextRecord, ConversationGroupRecord, ConversationGroupParticipant, ConversationTranscriptRecord, ConversationMembershipRecord, ConversationTranscriptStateRecord — types.go) so services call it directly instead of beads.Store. Surface grows only as a caller needs it (no-speculative-methods rule from beads-work-infra-split.md), seeded from the exact beads.Store methods the four services use today (Create/Get/Update/Close/Reopen/List/SetMetadata/SetMetadataBatch — grounded at binding_service.go:154,285,307,403,878; transcript_service.go:116,133; delivery_service.go:96; group_service.go:111,218).
- **Files:** internal/extmsg/store.go, internal/extmsg/store_test.go, internal/extmsg/types.go
- **Test-first:** In internal/extmsg/store_test.go add a compile-time assertion test TestExtMsgStoreIsBeadStoreSubset with `var _ ExtMsgStore = (beads.Store)(nil)` (mirroring coordrouter stores.go's `var _ X = beads.Store(nil)` proof) so the first bd-delegating impl IS any beads.Store with zero wrapper code; assert it fails to compile if a method without a real call site is added.
- **Acceptance:**
  - ExtMsgStore declares ONLY methods with a cited current call site in extmsg services (each method doc-comments its call site, per the coordrouter precedent)
  - Compile-time subset assertion passes against beads.Store
  - No edits to internal/beads/beads.go (Layer-0 substrate unchanged, per §3 invariant)
  - go vet + make test green
- **Depends on:** —
- **Risk:** Low. Pure declaration; the subset proof guarantees the bd adapter is a no-op.

#### P7-T3 — Define the storage-row edge: per-family bead↔record codec with versioned typed-extras blob  _(size M)_
- **Intent:** DESIGN §4 modeling rule: today the row edge is the implicit meta.-prefixed flat map (helpers.go encodeMetadataFields/decodePrefixedMetadata + per-family decode*Bead funcs). Make it explicit per family: promote ONLY fields a store predicate/wire consumer needs to typed columns later; everything else into a per-entity versioned typed extras blob {v int; known typedStruct; unknown map[string]string} (NOT map[string]any). The `unknown` passthrough must capture every metadata key this binary did not enumerate so the bd-delegating phase round-trips losslessly. This is a codec extraction only — byte-identical bd behavior.
- **Files:** internal/extmsg/rowcodec.go, internal/extmsg/rowcodec_test.go, internal/extmsg/binding_service.go, internal/extmsg/transcript_service.go, internal/extmsg/group_service.go
- **Test-first:** In rowcodec_test.go add TestRowCodec_ReconstructUnionKeyForKey: for one representative bead per family, encode→decode→re-encode and assert key-for-key equality of the reconstructed metadata map vs the original b.Metadata (the §4 reconstruct contract), AND assert no projection key lives in BOTH extras.known and extras.unknown (double-write/drift guard). Seed an unknown synthetic key meta.future_field and assert it survives the round-trip.
- **Acceptance:**
  - Each of the ~12 record types has an explicit encode(record)->bead and decode(bead)->record at one row-edge site (replacing scattered decode*Bead + inline Create metadata maps)
  - Reconstruct-union key-for-key equality holds incl. unknown passthrough (the loss detector from §7 subtest b)
  - Existing internal/extmsg/extmsg_test.go and types_wire_test.go stay green (byte-identical behavior)
  - go vet + make test green
- **Depends on:** P7-T2
- **Risk:** Medium. ~12 record types touch 3 large service files; if irreducibly >5 files, split by family-group (T3a binding+delivery, T3b group+participant, T3c transcript+membership+state) — flagged because the codec must land atomically per family to keep round-trip tests meaningful.

#### P7-T4 — Land the bd-delegating ExtMsgStore impl and inject via NewServices (no behavior change)  _(size M)_
- **Intent:** DESIGN §7 Step 1: the first ExtMsgStore impl wraps beads.Store and translates at the row edge (T3 codec). Re-root NewServices (services.go) on ExtMsgStore; the bd adapter is the subset-proven beads.Store itself (T2), so injection is a pure extract-interface refactor. Production injection sites are cmd/gc/api_state.go:149 and :683 (extmsg.NewServices(cs.cityBeadStore/cityStore)); test sites enumerated at internal/api/handler_extmsg_test.go and cmd/gc/session_beads_test.go pass the same store.
- **Files:** internal/extmsg/services.go, internal/extmsg/store.go, internal/extmsg/binding_service.go, internal/extmsg/transcript_service.go, internal/extmsg/group_service.go
- **Test-first:** Add TestNewServicesAcceptsExtMsgStore asserting NewServices compiles/runs with an ExtMsgStore (bd adapter) and that the four service structs hold the typed store, not raw beads.Store; reuse the existing binding_survival_test.go + outbound_test.go as the byte-identical regression net (they must stay green unchanged).
- **Acceptance:**
  - All four services (Bindings/Delivery/Groups/Transcript) consume ExtMsgStore via constructor injection; no service holds beads.Store directly
  - cmd/gc/api_state.go:149 and :683 + delivery_service.go/binding_reaper.go compile and behave identically
  - Entire internal/extmsg/*_test.go suite + cmd/gc extmsg tests green with zero assertion changes (proof of byte-identity)
  - go vet + make test green
- **Depends on:** P7-T2, P7-T3
- **Risk:** Medium. binding_reaper.go and the package-level Reassign*/CloseSession* free functions (binding_service.go:438,579,682) also take beads.Store — they must move to ExtMsgStore in the same change or be left on beads.Store with a documented seam; flag the choice in the PR.

#### P7-T5 — Build the per-family conformance suite skeleton (extmsgtest), default-skipped  _(size M)_
- **Intent:** DESIGN §7 conformance discipline + §11 per-family conformance: create an exported RunExtMsgStoreTests(t, family, factory) modeled on internal/mail/mailtest/conformance.go and internal/coordrouter/coordtest/conformance.go (Options{Skip,Reason}, factory returns fresh empty store). One representative record per family drives the generic subtests. P0-style: default Skip:true with a Reason naming this epic; the package's own test runs Skip:false against a MemStore to prove non-vacuity (the coordtest precedent).
- **Files:** internal/extmsg/extmsgtest/conformance.go, internal/extmsg/extmsgtest/conformance_test.go, internal/extmsg/extmsgtest/testenv_import_test.go
- **Test-first:** extmsgtest/conformance_test.go runs RunExtMsgStoreTestsWithOptions(..., Options{Skip:false}) against a MemStore-backed bd adapter for all 8 families and asserts every subtest executes and passes — proving the harness is non-vacuous before any real backend exists (mirrors coordtest/conformance_test.go).
- **Acceptance:**
  - RunExtMsgStoreTests covers the §7 mandatory subtests applicable to a history-tier class: (a) golden round-trip incl. unknown passthrough, (b) reconstruct-union equality, (f) error classification (UNIQUE→ErrRetryableContention vs ErrHard), (h) cross-store FK resolve+dangling (session FK), (g) readyExcludeTypes-deletion proof
  - One representativeRecord(family) helper maps each of 8 families to a concrete record (mirrors coordtest representativeBead)
  - Suite default-skipped with Reason naming the extmsg epic; non-vacuity test green
  - go vet + make test green
- **Depends on:** P7-T2, P7-T3
- **Risk:** Medium. Transcript family carries sequence/hydration state subtests that mail's flat conformance lacks; those are family-specific and must be added beyond the generic CRUD shape.

#### P7-T6 — Run the per-family conformance against the bd adapter (flip Skip off, prove green)  _(size S)_
- **Intent:** DESIGN §7: the bd impl AND the future SQLite impl run the IDENTICAL suite. Flip Skip:false for the bd-delegating ExtMsgStore so the bd path is proven before SQLite exists — this is the oracle that lets a later swap be safe (conformance-only validation, owner decision 6).
- **Files:** internal/extmsg/store_conformance_test.go
- **Test-first:** Add store_conformance_test.go calling RunExtMsgStoreTestsWithOptions(t, family, bdFactory, Options{Skip:false}) for each of the 8 families against the bd-delegating adapter over a MemStore/bd test store; it must pass (this IS the test).
- **Acceptance:**
  - All 8 families pass the full conformance suite against the bd adapter with Skip:false
  - Any clause the bd adapter cannot meet (e.g. the cross-controller idempotency race) is kept behind extmsgtest Options{Skip,Reason} naming an escalation bead — gaps documented, never hidden (§7 Step 1)
  - go vet + make test green
- **Depends on:** P7-T4, P7-T5
- **Risk:** Low. Pure test wiring; if a subtest fails it reveals a real codec defect from T3 to fix before proceeding.

#### P7-T7 — Pin the session FK cross-store resolve+dangling behavior for extmsg  _(size S)_
- **Intent:** DESIGN §7 cross-store reads + §9 coupling 4: bindings/participants FK to sessions via resolveLiveSessionID = session.ResolveSessionID (live_session.go:14,36) and overlayLiveSessionID (binding_service.go:229). Under relocation sessions and extmsg live in different stores, so this FK becomes a (storeRef,id) cross-store resolve that must tolerate dangling ids: matching-prefix-but-absent-row returns not-found cleanly; unrecognized prefix never silently mis-routes (§7, §9 coupling 4, conformance subtest h).
- **Files:** internal/extmsg/live_session.go, internal/extmsg/live_session_test.go, internal/extmsg/extmsgtest/conformance.go
- **Test-first:** Add TestResolveLiveSessionID_DanglingFK: when the session name resolves to no live bead (absent row), overlayLiveSessionID leaves *target unchanged and does not panic/mis-route; when the store handed in is a foreign-prefix store, resolution returns not-found rather than a wrong bead. Add the same as conformance subtest (h) in extmsgtest.
- **Acceptance:**
  - Dangling session FK resolves to not-found cleanly (no silent wrong-bead routing)
  - overlayLiveSession/overlayLiveSessionID and the binding reaper preserve current healing behavior on the happy path (binding_survival_test.go green)
  - Subtest (h) wired into extmsgtest and green against the bd adapter
  - go vet + make test green
- **Depends on:** P7-T5, P7-T6
- **Risk:** Medium. The reaper depends on P3.5 (storeRef,id) prefix resolver for true cross-store healing; if P3.5 is not yet landed, document the dependency and keep bd-resolve behavior verbatim (this stays a pre-relocation pin).

#### P7-T8 — Reserve the non-configurable gce- id prefix and id-sequence model for the extmsg SQLite store  _(size S)_
- **Intent:** DESIGN §4 per-class store scope: each relocated class is a single city-scope SQLite store with one global id sequence and one file, with a reserved non-configurable prefix validated against rig.EffectivePrefix() under longest-prefix precedence. The four reserved in §4 are gcm-/gco-/gcn-/gcs-; extmsg needs its own (propose gce-). Reserve it and add the precedence/validation so it can never collide with a rig prefix.
- **Files:** internal/config/config.go, internal/config/config_test.go, internal/extmsg/store.go
- **Test-first:** Add TestExtmsgPrefixReservedAndNonColliding: gce- is rejected as a rig EffectivePrefix (or never wins longest-prefix against it), and an extmsg id like gce-000123 routes only to the extmsg store; assert a concurrent-process id-non-collision contract (one global sequence) as a unit-level assertion stub for the later §7(i) subtest.
- **Acceptance:**
  - gce- (or chosen prefix) reserved as non-configurable; rig.EffectivePrefix validation rejects/deprioritizes it with longest-prefix precedence (matches the §4 gcm-/gco-/gcn-/gcs- treatment)
  - Prefix resolver routes gce- ids to the extmsg store and no other (sets up §9 coupling 3 ResolveStoreRef extension)
  - go vet + make test green; TestOpenAPISpecInSync unaffected (no wire change)
- **Depends on:** P7-T2
- **Risk:** Low-medium. Must coordinate with the P3.5 prefix resolver inventory — the new prefix must be added to the (storeRef,id) IDPrefix switch (dispatch/runtime.go:48-63 per §2) so workflow-finalize/cross-store lookups recognize it.

#### P7-T9 — Design the per-family SQLite schema (typed columns + indexes + versioned extras)  _(size M)_
- **Intent:** DESIGN §7 Step 2: real per-entity schema on the proven internal/beads/sqlite_store.go substrate. Promote to typed columns only the fields the services SELECT/order on: the locator-label hashes today drive every lookup (labels.go: binding-conv/binding-session/binding-sessionname, delivery-route/session, group-root/participant/participant-session, transcript-conv/bucket/msg, membership-conv/session/exact, transcript-state). Those become indexed columns; transcript sequence (next_sequence) and bucket are hot-path ordered columns; everything else → versioned extras blob (T3).
- **Files:** internal/extmsg/sqlite_schema.go, internal/extmsg/sqlite_schema_test.go
- **Test-first:** Add TestExtmsgSqliteSchema_IndexesCoverLookups: assert every service lookup path (the ~16 label-keyed List queries grounded in labels.go) maps to an indexed column or composite index; assert transcript sequence ordering uses an index (no full scan), reproducing the bucket/sequence access pattern from transcript_service.go.
- **Acceptance:**
  - Schema defines columns+indexes for all 8 families' lookup keys; non-selected fields in the versioned extras blob
  - Transcript next_sequence single-writer increment + ordered list are index-backed (the hot mutate path)
  - Schema migrations are idempotent (CREATE TABLE IF NOT EXISTS) and applied via the sqlite_store substrate, MaxOpenConns=1
  - go vet + make test green
- **Depends on:** P7-T3, P7-T8
- **Risk:** Medium. The label-hash → typed-column mapping is the highest-judgment step; an over-aggressive promotion breaks the reconstruct-union guard. Keep promotions minimal and lean on extras.unknown.

#### P7-T10 — Implement the SQLite ExtMsgStore over the substrate (controller-mediated writes)  _(size L)_
- **Intent:** DESIGN §7 Step 2 + decision 5: same ExtMsgStore interface over SQLiteStore with the T9 schema; writes route through the long-lived controller (single in-process writer, MaxOpenConns=1 = serialization), reads open their own read-only WAL handle. Build on internal/beads/sqlite_store.go (the graph proof-of-pattern under graph_store=sqlite, api_state.go).
- **Files:** internal/extmsg/sqlite_store.go, internal/extmsg/sqlite_store_test.go, internal/extmsg/sqlite_schema.go
- **Test-first:** Add TestSqliteExtMsgStore_BasicCRUD per family as a smoke layer, then immediately gate it under the shared conformance: it is the SAME suite, so the real test-first is wiring RunExtMsgStoreTestsWithOptions(t, family, sqliteFactory, Options{Skip:false}) and watching CRUD subtests fail until the impl lands.
- **Acceptance:**
  - SQLite ExtMsgStore satisfies ExtMsgStore for all 8 families
  - Writes serialize through one in-process writer (MaxOpenConns=1); reads use a separate read-only WAL handle (decision 5)
  - Idempotency uses real unique indexes where keys are genuinely unique (binding-conv, transcript provider-message, membership-exact) and a typed Go query where they are not (active-binding selection) — per §7 idempotency-per-class; error classification maps UNIQUE→ErrRetryableContention vs ErrHard (subtest f)
  - go vet + make test green
- **Depends on:** P7-T9, P7-T6
- **Risk:** High. Largest unit; if it exceeds 5 files split by family-group (T10a binding/delivery, T10b group/participant/membership, T10c transcript/transcript-state) sequenced behind the shared conformance — irreducibly large because all families share one store file/sequence and the controller-write path.

#### P7-T11 — Run the full per-family conformance against the SQLite impl + concurrent-process subtest  _(size M)_
- **Intent:** DESIGN §7 conformance discipline + subtest (i): the SQLite impl must pass the IDENTICAL suite the bd impl passed (T6), plus the §7(i) concurrent-process id non-collision + no SQLITE_BUSY-escaping-retryOnBusy subtest (the §4 per-class concurrent-process test).
- **Files:** internal/extmsg/sqlite_conformance_test.go, internal/extmsg/extmsgtest/conformance.go
- **Test-first:** sqlite_conformance_test.go runs RunExtMsgStoreTestsWithOptions(Skip:false) for all 8 families against the SQLite factory; add subtest (i): spawn N concurrent writers via the controller-write path, assert all ids unique under one global sequence and no SQLITE_BUSY escapes retryOnBusy.
- **Acceptance:**
  - All 8 families pass the identical suite on SQLite that passed on bd (a swap cannot silently change semantics)
  - Concurrent-process subtest (i) green: id non-collision + no SQLITE_BUSY escape
  - Watch-coherence/emit-after-commit (subtests c,d) green if event emission is in scope for this epic, else explicitly Skip+Reason deferring to the shared §6 event work
  - go vet + make test green
- **Depends on:** P7-T10
- **Risk:** Medium. Reuses the same suite; failures here are real divergences that must block cutover (this is the only oracle under conformance-only validation).

#### P7-T12 — Build the HISTORY backfill copier with post-backfill projection-equality verification  _(size L)_
- **Intent:** DESIGN §11 + §7: extmsg is NOT clean-drain (it is history-tier — confirmed: type=task falls to bead_policy_store.go default storage-class = history, unlike sessions/orders/nudges no_history and mail ephemeral). A real history backfill must copy all OPEN AND historical extmsg beads (all 8 families incl. closed bindings, full transcripts) from bd→SQLite, then verify. This is a §8 point-of-no-return analog: bd-row deletion per class is irreversible after backfill.
- **Files:** cmd/gc/extmsg_backfill.go, cmd/gc/extmsg_backfill_test.go, internal/extmsg/sqlite_store.go
- **Test-first:** Add TestExtmsgHistoryBackfill_ProjectionEquality: seed a bd store with a corpus spanning all 8 families incl. closed/historical rows and a deep transcript with sequence gaps; run the backfill into a fresh SQLite store; assert key-for-key reconstruct-union equality (§4) for every record AND that every service read (ResolveByConversation, ListTranscript asc+desc, ListMemberships, group routing) returns identical results from both stores over the LIVE corpus.
- **Acceptance:**
  - Backfill copies ALL extmsg beads (open + closed + full transcript history) for all 8 families, preserving ids/sequence/status
  - Post-backfill verification asserts projection-equality over the corpus before any bd-row deletion (the §13 accepted-risk mitigation — must run against a corpus dumped from a LIVE city)
  - Backfill is resumable/idempotent (re-running does not duplicate)
  - go vet + make test green
- **Depends on:** P7-T11
- **Risk:** High. The distinguishing P7 risk vs mail's clean-drain. Transcript sequence integrity and closed-binding history must survive exactly. Irreducibly large: backfill + verification must land together to be meaningful.

#### P7-T13 — Cut over extmsg to SQLite behind a config flag (default bd) and delete the readyExcludeTypes proof  _(size M)_
- **Intent:** DESIGN §7 + §10 coupling 10: flip extmsg creation to the SQLite ExtMsgStore behind a per-class config flag defaulting to bd, after T12 backfill+verify. Then the §9.10/§10-coupling-10 deletion-is-the-proof: with extmsg out of the work store, the extmsg Ready-exclusion from T1 becomes a deletion proof (work store never receives type=task+gc:extmsg-* claimable beads). Revert = quiesce-then-flip-back at a maintenance window (§7, §13 accepted risk).
- **Files:** cmd/gc/api_state.go, internal/config/config.go, cmd/gc/extmsg_cutover_test.go, internal/storehealth/storehealth.go
- **Test-first:** Add TestExtmsgCutover_WorkStoreNeverReceivesExtmsg: with the flag set to sqlite, assert extmsg.NewServices is constructed over the SQLite ExtMsgStore at api_state.go:149/:683, that the work bd store receives zero new gc:extmsg-* beads, and Work().Ready is unchanged (the deletion proof); plus a flip-back integrity test (sqlite→bd quiesce-then-flip) per §7 revert.
- **Acceptance:**
  - Per-class config flag selects bd|sqlite for extmsg; default bd; cutover wired at both api_state.go injection sites
  - Deletion proof green: work store never receives extmsg beads under sqlite; Work().Ready unchanged
  - storehealth enumerates the extmsg *.sqlite file for size/disk/GC (§7 observability); controller-owned retention sweep covers it (§7 retention)
  - Flip-back integrity test green (§7 revert); go vet + make test green; TestOpenAPISpecInSync + make dashboard-check green (extmsg HTTP handlers wire unchanged — no DTO change in this epic)
- **Depends on:** P7-T12, P7-T1
- **Risk:** High. The production flip; first divergence is discovered in prod (accepted risk §13). Mitigated only by T11 conformance depth + T12 projection-equality. Must coordinate the controller-owned retention sweep (history-tier extmsg accumulates — unlike the no_history classes) so the new SQLite file does not grow unbounded.

