# S38 — Durable `gc.control_for` lineage instead of ref-string surgery

## Target design

**One durable pointer replaces four string heuristics.** Every attempt/iteration
root bead carries `gc.control_for = <control bead ID>` (the *bead ID* of the
retry/ralph control bead, not a step ref). Attempt recovery becomes:

```go
// findLatestAttempt, new primary path
for _, b := range beadsUnderWorkflowRoot {
    if isFailedPartialMolecule(b) { continue }
    if b.Metadata[beadmeta.ControlForMetadataKey] != control.ID { continue }
    n, _ := strconv.Atoi(b.Metadata[beadmeta.AttemptMetadataKey])
    if n > latestAttempt { latest, latestAttempt = b, n }
}
```

One ID equality + one integer max. No ref parsing, no kind-based skip rules
needed on the primary path (only the control's own attempt roots carry its ID).

**Key semantic decision — two populations of `gc.control_for`:**
`ControlForMetadataKey` today holds *step refs / step IDs* when written by
fanout (`control.go:850` — sourceRef), scope-check (`control.go:920` — step.ID),
and the compiler (`formula/graph.go:40,84`, `formula/fragment.go:228` — step.ID).
S38 adds a second population where the value is a *bead ID* (control bead IDs
are store IDs like `gcg-...`/`ga-...`, syntactically disjoint from dotted step
refs). The consumer added by S38 matches by exact equality against
`control.ID`, so the existing step-ref population can never collide — a step
ref never equals a bead ID. Existing consumers (fanout.go:64,
graphroute.go:373/383, runproj detail views, ralph.go rewriter) are keyed off
beads that S38 does not restamp, except the ralph retry-rewrite path, which is
handled explicitly (see enumeration).

Chosen approach: **backlog approach (a)** — metadata pointer with the current
four-stage cascade demoted to a single clearly-marked legacy fallback for
pre-stamp molecules, deleted one release later. (Approach (c)'s dep-walk
secondary is retained *as it already exists* — `latestAttemptFromDependencies`
— but not promoted; no new mechanism.)

Producers to stamp (proved exhaustive in the enumeration section):
1. `buildAttemptRecipe` (`internal/dispatch/control.go`) — every runtime-minted
   attempt N≥2 root (retry) and iteration root (ralph), including nested seeds.
2. Compile-time first-attempt seeds: `internal/formula/retry.go` (expandRetry)
   and `internal/formula/ralph.go` (iteration-1 scope/task roots).
3. `internal/dispatch/ralph.go` retry-scope re-mint path must keep the pointer
   coherent (rewriteRetryControlFor already rewrites `gc.control_for` values
   across retries — the new bead-ID population must pass through unchanged,
   since the control bead ID is stable across attempts).

## Current behavior (site-by-site enumeration)

### Read side (the code being simplified)

| # | Site | Current behavior | New form |
|---|------|------------------|----------|
| R1 | `internal/dispatch/control.go:1348` `findLatestAttempt` | Lists beads under `gc.root_bead_id` (fallback `control.ID`), runs `latestAttemptFromCandidates`; on empty, falls back to `latestAttemptFromDependencies` (blocks-dep walk, :1378). | Same skeleton. `latestAttemptFromCandidates` gets a new primary match (below); the dep-walk fallback is *unchanged* — it feeds candidates through the same matcher, so it inherits the new match for free. |
| R2 | `control.go:1394-1472` `latestAttemptFromCandidates` — the four heuristics: (1) `controlRef+".attempt."/".iteration."` prefix (:1423), (2) `stepID` prefix (:1426), (3) after-last-`.iteration.N.`/`.attempt.N.` marker surgery for compose.expand children (:1437), (4) last-dot-segment fallback (:1454). Plus skips: `molecule_failed` (:1404), infrastructure kinds (:1410, `latestAttemptCandidateIsControlInfrastructure` = `IsControlKind ∨ KindWorkflow`), scope-unless-ralph (:1413). | Selects max `gc.attempt` among matches. | **Primary:** keep the `molecule_failed` + infrastructure-kind skips; match `cf := b.Metadata[gc.control_for]` by exact equality against the identity set `{control.ID, control.Metadata[gc.step_ref], control.Metadata[gc.step_id]}` (non-empty members only); max `gc.attempt`. **Legacy fallback (one release):** if the primary finds nothing, run the existing four-stage cascade unchanged, behind a clearly named `latestAttemptFromCandidatesLegacyRefSurgery`. The scope-unless-ralph skip stays legacy-only — on the primary path only this control's own attempt roots carry its identity. |
| R3 | Callers `control.go:35` (processRetryControl) and `:165` (processRalphControl) | Empty result → `ErrControlGraphMalformed` quarantine (#2798 semantics). | Unchanged. Quarantine semantics, error strings, and `ErrControlPending` flows are out of scope. |

### Why the identity *set* (not bead ID alone)

Compile-time seeds are `formula.Step`s — no store bead ID exists yet, and
`molecule.Instantiate` does not rewrite metadata values into bead IDs.
`buildNestedControlSeed` (control.go:806) likewise stamps via a *synthetic*
control whose `.ID` is the namespaced step ref (`childID`), not a store ID.
So the stamp's value is "the control's identity as known at mint time":

- runtime top-level mint (`buildAttemptRecipe` called from `spawnAttempt`,
  control is a real bead) → **control bead ID** (durable, survives every
  ref-shape change);
- compile-time seeds and nested runtime seeds → **the control's namespaced
  step ref** (= `step.ID` at compile time; Instantiate fills the control's
  `gc.step_ref` from the same string, so read-side equality holds).

Exact equality against ≤3 known strings — still zero parsing, zero prefix
surgery. Collision with the *existing* step-ref-valued `gc.control_for`
population (check/fanout/scope-check controls pointing at their source step,
which may BE this retry step) is excluded by the retained
infrastructure-kind skip: those carriers all have control kinds.

### Write side — every attempt/iteration-root creation path

| # | Site | What it mints today | S38 change |
|---|------|---------------------|------------|
| W1 | `internal/formula/retry.go:48` `expandRetry` | Compile-time `<step>.attempt.1` work bead (`run.Metadata`: gc.attempt=1, gc.step_id). Control keeps `step.ID`. | Add `run.Metadata[gc.control_for] = step.ID`. |
| W2 | `internal/formula/ralph.go` simple-ralph branch (~:92-113) | Compile-time `<step>.iteration.1` work bead. | Add `iteration.Metadata[gc.control_for] = step.ID`. |
| W3 | `internal/formula/ralph.go:116` `expandNestedRalph` (~:128-150) | Compile-time `<step>.iteration.1` **scope** root (body children hang off it via gc.scope_ref). | Add `iteration.Metadata[gc.control_for] = step.ID` on the scope root only. Body children are NOT attempt roots — do not stamp them. |
| W4 | `internal/dispatch/control.go:600` `buildAttemptRecipe` root step (:627-653) | Runtime attempt/iteration N root (`rootMeta`: gc.attempt=N, gc.step_id, gc.step_ref=attemptPrefix; kind task or scope). | Add `rootMeta[gc.control_for] = control.ID` **after** the `step.Metadata` copy loop so a formula-authored value cannot shadow it. Uniform for W4/W5: `control.ID` is a real bead ID at W4, the namespaced ref at W5. |
| W5 | `control.go:806` `buildNestedControlSeed` | Nested ralph's iteration-1 sub-DAG during outer re-spawn, via `buildAttemptRecipe(child, synthetic, 1)` with `synthetic.ID = childID` (namespaced step ref). | No extra code — W4's stamp yields `gc.control_for = childID`, which equals the inner control bead's `gc.step_ref` at read time. Covered by the identity set. |
| W6 | `internal/dispatch/ralph.go:424` `appendRalphRetryLegacy` | In-iteration ralph retry: **clones** every bead of the attempt set (incl. nested control beads and their attempt roots) with NEW bead IDs; already pipes `gc.control_for` through `rewriteRetryControlFor` (:446,:483,:516). | Bead-ID-valued `gc.control_for` passes `rewriteRetryControlFor` unchanged (no scope-ref prefix, no `.attempt./.iteration.` markers — verified reading ralph.go:1076-1104). **But** a cloned attempt root pointing at a cloned nested control's OLD bead ID is now stale. Add a post-create remap pass mirroring `remappedLogicalBeadID` (:541-556): for each clone, if `old.Metadata[gc.control_for] ∈ mapping` → `SetMetadata(newID, gc.control_for, mapping[oldValue])`. Step-ref-valued entries are not in `mapping` (keys are bead IDs) → untouched, still rewritten by the existing string rewrite. |
| W7 | `ralph.go:583` `appendRalphRetryViaGraphApply` → `buildRalphRetryGraphNode` (:649) | Graph-apply twin of W6; same `rewriteRetryControlFor` call (:656); has a `MetadataRefs` intra-plan ID-remap mechanism already used for `gc.logical_bead_id` (:658-668). | If `old.Metadata[gc.control_for] ∈ attemptIDs` → move it to `MetadataRefs[gc.control_for]` (applier substitutes the new ID) instead of the string rewrite; else keep current behavior. |
| W8 | `internal/formula/compile.go:582` parity contract | Comment: compile-time expansion and `buildAttemptRecipe` must mint identical shapes. | Both sides now mint `gc.control_for`; the parity test (see test plan) must assert the key exists on both and that read-side equality holds for each mint origin. |

Non-paths (checked, no stamp needed): `buildAttemptRecipe` child steps
(:685-784, members not attempt roots — matched today only via the legacy
cascade's short-ref stages when they are nested controls' *children*, never
selected as the control's own attempt); fanout-minted spawn beads
(fanout.go — their controls are fanout-kind, different recovery path);
`clearRetryEphemera` (ralph.go:1024) does not clear `gc.control_for`;
`beadmeta.KnownMetadataKeys` already contains `ControlForMetadataKey`
(keys.go:269) — no registry change.

## Invariants — the correctness contract

**I1 — Total stamp coverage (the deletion precondition).** Every bead that
`latestAttemptFromCandidates` can legitimately select as a control's latest
attempt carries `gc.control_for` equal to a member of that control's identity
set, on every creation path W1-W7. PROVEN BEFORE the cascade is deleted by
the shadow-parity test (below): primary-path result == full-cascade result on
every existing control fixture in `control_test.go`,
`attempt_control_routing_test.go`, and the ralph/compose integration suites.

**I2 — No false positives.** The primary path never selects a bead the
cascade would not have: (a) infrastructure-kind and `molecule_failed` skips
retained; (b) equality (not prefix) match; (c) step-ref identity values are
namespaced refs — unique per control instance within a workflow root because
`molecule.Instantiate`/`Attach` already require ref uniqueness for dep
wiring. Nested controls re-seeded per outer iteration get distinct namespaced
refs (`mol.outer.iteration.N.inner`), so an inner control never matches a
sibling iteration's seeds.

**I3 — Pointer coherence across clones.** Any operation that re-mints a bead
carrying `gc.control_for` under a new bead ID must remap bead-ID-valued
pointers through its old→new mapping (W6 post-pass, W7 MetadataRefs), exactly
as `gc.logical_bead_id` is remapped today. Step-ref-valued pointers continue
through `rewriteRetryControlFor` unchanged.

**I4 — Existing `gc.control_for` consumers unaffected.** fanout.go:64,
graphroute.go:373/383 gate on control kinds (fanout/scope-check) — attempt
roots are kind task/scope, never routed there. runproj hidden-badge helpers
(detail_groups.go:390/427, detail_nodeshape.go:269) apply only to beads
classified hidden (control infrastructure); runproj golden tests must stay
byte-identical (verified in test plan, not assumed).

**I5 — Attempt selection semantics unchanged.** Selection is still
max(`gc.attempt`) with ties broken as today (first-seen wins at equal
attempt, i.e. `>` not `>=`); empty-result quarantine (`ErrControlGraphMalformed`,
#2798) and the dep-walk fallback ordering in `findLatestAttempt` are
untouched.

**I6 — Idempotent re-entry.** The stamp is part of the minted metadata map —
written through the same idempotent create paths (`molecule.Attach` with
`<controlID>:attempt:N` idempotency keys, `resolveExistingRalphRetryFromBeads`
re-detection). No new write-after-create except the W6 remap pass, which is a
deterministic `SetMetadata` re-runnable to the same value.

**I7 — Project invariants.** No role names in any new Go; no wire surface
touched (metadata is store-side, not HTTP/SSE — typed-wire and typed-events
untouched, no new event types); `internal/dispatch` + `internal/formula`
only, no upward imports (beadmeta ← formula ← dispatch is the existing
direction); worker boundary, config.Agent field-sync, cmd/gc projections all
untouched.

## Behavior-preserving migration/staging

Phased so every commit is releasable and the stamp is PROVEN before any
string logic is deleted. Each phase ≤5 files.

**Phase 1 — Stamp everywhere (write side only, zero read-side change).**
Edits: `internal/formula/retry.go` (W1), `internal/formula/ralph.go`
(W2, W3), `internal/dispatch/control.go` `buildAttemptRecipe` (W4; W5 free),
plus unit tests asserting the key on each mint. Read path untouched →
behavior identical by construction. Ship.

**Phase 2 — Clone coherence.** Edits: `internal/dispatch/ralph.go` (W6
post-pass remap + W7 MetadataRefs) + tests that a ralph in-iteration retry
of a nested control yields clones whose `gc.control_for` points at the NEW
control bead ID (legacy and graph-apply variants). Still no read-side
change. Ship.

**Phase 3 — Read side flips, cascade demotes to guarded fallback.**
Edits: `internal/dispatch/control.go` — `latestAttemptFromCandidates`
becomes primary-equality-match; on empty result it calls
`latestAttemptFromCandidatesLegacyRefSurgery` (the current body, moved
verbatim, with a `// DEPRECATED: remove after <release N+1> — serves only
molecules minted before the gc.control_for stamp` marker). Add a trace line
(`opts` isn't plumbed here, so a package-level counter or debug log is
acceptable — NOT a new event type) when the legacy path is the one that
finds the attempt, so operators can observe pre-stamp traffic drain.
Includes the **shadow-parity test** (I1): for every fixture corpus, assert
primary-path result == legacy-cascade result whenever the stamp is present.
Ship in release N.

**Legacy population handling (the one-release guarded fallback).** In-flight
molecules minted before release N have unstamped attempt roots; the guarded
fallback recovers them exactly as today. No data migration is run — beads
are append-heavy and molecules are comparatively short-lived; a store-wide
rewrite is riskier than a one-release fallback. Optional cheap self-heal
(recommended): when the legacy path finds the attempt, `SetMetadata(attempt.ID,
gc.control_for, control.ID)` so each legacy molecule is touched at most once
and long-lived pre-stamp ralphs converge onto the primary path before N+1.

**Phase 4 — Deletion (release N+1).** Delete
`latestAttemptFromCandidatesLegacyRefSurgery` (~78 LOC incl. the
`.iteration.N.` marker-surgery block and last-dot fallback) and the
scope-unless-ralph skip it carried. `latestAttemptFromDependencies` stays —
it is an independent recovery mechanism for list failures, now feeding the
equality matcher. Precondition for shipping Phase 4: the Phase-3 legacy-hit
observation shows no hits, and one full release has elapsed. Any molecule
older than one release that still lacks stamps quarantines with the existing
#2798 `no attempt found` classification — the documented, operator-visible
failure mode, recoverable by retry (which mints stamped attempts).

Rollback: each phase is independently revertable; Phases 1-2 are pure
additive metadata, Phase 3 keeps the legacy body callable, Phase 4 is a
plain deletion commit that can be reverted wholesale.

## Test plan (incl. -race/parity if applicable)

**T1 — Stamp-coverage units (Phase 1, one per mint path).**
- `formula/retry_test.go`: `expandRetry` output — attempt.1 carries
  `gc.control_for == step.ID`; control/spec steps do NOT.
- `formula/ralph_test.go`: simple ralph iteration.1 and nested-ralph scope
  root carry the stamp; body children do NOT.
- `dispatch/control_test.go`: `buildAttemptRecipe` root step carries
  `gc.control_for == control.ID`; a formula-authored `gc.control_for` in
  `step.Metadata` is overridden (W4 ordering); nested seed
  (`buildNestedControlSeed`) root carries `childID`.

**T2 — Read-side table test (Phase 3).** `latestAttemptFromCandidates` as a
pure function over (control, candidates): match by bead ID; by step_ref; by
step_id; skip `molecule_failed`; skip infrastructure kinds carrying the same
value (a check control whose `gc.control_for` = the retry's step ref must
NOT be selected); max-attempt selection and `>` tie-break; empty → legacy
fallback engaged.

**T3 — Shadow parity (the I1 deletion gate).** Test-only harness: for every
control/candidates fixture in `control_test.go` and
`attempt_control_routing_test.go` where stamps are present, assert
primary == legacy result. Run entire dispatch package under `-race` (store
fakes are exercised concurrently by controller tests already).

**T4 — Legacy-population regression.** Fixtures with NO stamps (hand-built
pre-release-N shapes, incl. the #2798 compose.expand short-ref corpus and
the last-dot single-segment corpus) still resolve via the guarded fallback
in Phase 3 — these existing test cases must pass UNMODIFIED until Phase 4
deletes them together with the fallback. If self-heal is implemented: assert
the backfilled stamp equals `control.ID` and second call hits primary.

**T5 — Clone coherence (Phase 2).** Ralph in-iteration retry with a nested
retry control: after `appendRalphRetryLegacy`, the cloned attempt root's
`gc.control_for` == the cloned control's NEW bead ID; same for
`appendRalphRetryViaGraphApply` via `MetadataRefs` (both variants
parameterized, matching existing dual-path ralph tests). Step-ref-valued
control_for on cloned scope-checks still rewritten exactly as today
(existing `rewriteRetryControlFor` tests unmodified).

**T6 — End-to-end lifecycle integration.** Existing retry/ralph controller
integration tests (attempt exhaustion, ralph iteration loop, nested ralph
re-spawn #2798, crash-adoption re-entry) pass unmodified in every phase —
they are the behavioral parity oracle. Plus one new E2E: retry step through
attempt.1-fail → attempt.2-pass where attempt.2 recovery is forced through
the primary path (assert legacy counter == 0).

**T7 — Projection goldens (I4).** `internal/runproj` golden/detail tests and
dashboard fold tests pass unmodified — attempt roots gaining
`gc.control_for` must not change hidden-badge targeting or node shaping. If
a golden legitimately shifts, that is an I4 violation to fix in the stamp
consumer gating, not a golden to re-record.

**Gates per phase:** `make test` + `go vet ./...`; `make
test-cmd-gc-process-parallel` for the dispatch integration shards; no
dashboard/API surface touched so `dashboard-check` not required (confirm no
openapi.json diff in CI).

## Top correctness risks

1. **Stale bead-ID pointers across ralph in-iteration clones (highest).**
   `appendRalphRetryLegacy`/`ViaGraphApply` re-mint nested controls under
   new bead IDs; without the W6/W7 remap, a cloned attempt root points at a
   dead control ID, silently rides the legacy fallback through release N,
   then quarantines healthy workflows at N+1. Mitigation: Phase 2 lands
   before Phase 3; T5 covers both clone variants; Phase-4 deletion is gated
   on observed zero legacy hits, which would expose any missed clone path in
   production before the cascade is gone.

2. **An unenumerated mint path.** If any attempt-root creator outside W1-W7
   exists (or is added later without the stamp), its molecules work today
   via the cascade and break at Phase 4. Mitigation: T3 shadow parity across
   the full existing fixture corpus, the Phase-3 legacy-hit counter in real
   fleets, and the compile.go:582 parity contract test extended to assert
   stamp presence — a new mint path that forgets the stamp fails T1-style
   assertions if built on `buildAttemptRecipe`/`expandRetry`, the only
   recipe factories.

3. **False-positive equality match via the pre-existing step-ref-valued
   `gc.control_for` population.** A check/fanout/scope-check control whose
   source IS the retry step carries `gc.control_for == <retry step ref>` and
   would equality-match if the infrastructure-kind skip were dropped or a
   non-control bead ever inherited that metadata (e.g. via the W4
   `step.Metadata` copy loop from a formula-authored value). Mitigation: the
   skip is retained on the primary path (T2 case), and W4 writes the stamp
   AFTER the copy loop so authored values cannot shadow it.

4. **Identity-set drift between mint and read.** The read side matches
   `{control.ID, gc.step_ref, gc.step_id}`; if a future change rewrites a
   control's `gc.step_ref` after its seeds were minted (as ralph retries do
   for members today), step-ref-valued stamps could orphan. Currently only
   W6/W7 rewrite refs and they rewrite both sides in lockstep
   (`rewriteRetryControlFor`), but this coupling is the residual stringly
   surface — documented at both sites; the self-heal backfill (Phase 3)
   progressively converts step-ref stamps to bead IDs, shrinking exposure.
