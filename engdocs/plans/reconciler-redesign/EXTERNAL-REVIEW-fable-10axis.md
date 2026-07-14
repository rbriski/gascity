# External design review — Windshield GT reconciler redesign

**Reviewer:** independent Fable multi-agent workflow (not the plan authors, not window 50).
**Method:** 118 subagents — 10 parallel axis reviewers (Opus-class), findings clustered,
each blocker/high cluster adversarially verified by two independent lenses (factual-accuracy
+ already-addressed), each medium by one, plus a coverage-gap critic and a residual-risk
critic. ~13M review tokens total. Read-only against the worktree at the review head.
**Documents:** `PROPOSAL.md` + `IMPLEMENTATION_PLAN.md` (reviewed at 4687 lines; the plan has
since grown to ~6195 as you absorbed findings) + code grounding in `cmd/gc/`,
`internal/{session,beads,runtime,worker,convergence}`.

**Raw evidence** (full finding text, verifier verdicts with line-cited quotes):
`/tmp/review-synthesis/{digest-blocker-high.txt, digest-medium-compact.txt, critics.txt, axis-summaries.txt}`
and the workflow journal at
`~/.claude/projects/-data-projects-gascity/f03bf3b5-.../subagents/workflows/wf_d49f1adc-4e1/journal.jsonl`.

---

## Verdict

The architecture is sound and the correctness kernel is genuinely strong — the axis
reviewers independently praised the P5.2 queue contract (kills the client-go multi-queue
ownership trap), the S7.4 register-then-recheck lost-wakeup closure, the S7.3/S9 refusal to
equate caller timeout with call termination, the mechanically-derived effect inventory
(P0.1/P3.4), the P0.12 N/N-1 discipline, and the honest four-profile support model. **Do not
rearchitect.** Every finding below is a contract-text amendment or an added task, not a
structural objection.

You have been hardening the plan in real time: of the 5 blocker and 32 high raw findings,
the verifiers found the majority **already addressed** in the live document (P5.4A cross-owner
mutation bridge, P2.10A single physical writer, P5.5A ambiguity circuit, P3.10 independent
effect attribution, P0.11A pre-HA single-owner enforcement, P1.0B interim head-of-line relief,
P5.1N nudge subset, P5.10 per-key explanation surface, G4A/G4B split, INV-15/23/28, §27.5
stop-with-value). That is exactly the right response and it closed most of the sharp edges.

What remains falls in three buckets. **Bucket A is the important one** — four failure modes that
*all ten axes structurally missed* and that are absent from the plan today (verified by grep on
the current 6195-line file). Your own review loop will not surface these because they sit in the
negative space between the axes.

---

## A. Coverage gaps — absent from the plan, missed by every axis (fix these first)

### A1 — [HIGH/security] No authorization or requester-identity model on the durable command surface
The redesign converts session mutation (kill, close, nudge, interrupt) into durable store
objects the controller faithfully claims and executes. **Nothing models WHO may mint them.**
P2.11's request carries `request ID, stable session binding, desired phase, monotonic intent
generation, command-specific data, compatibility version` — no requester identity, no authz
hook. Every occurrence of "requester" in the plan/matrix is the self-termination *fence*, not
authorization. Every treatment of out-of-band writes (P2.9 drift, "Unknown never authorizes
destruction", P10 anti-entropy) treats them as *facts to converge*, never *commands to
validate* — but a well-formed command bead **is** the front door, so drift detection never
fires on it.

Grounding: commands are beads (`internal/nudgequeue/store.go`); every fleet agent runs `bd`
directly (AGENTS.md); `internal/api` has writeauth on the HTTP plane but `bd` bypasses it; the
production store is a network-reachable Dolt server (`dolt.port 3307`).

**Failure:** a prompt-injected worker agent — or anything holding the shared Dolt credential,
remotely — runs `bd create` to mint well-formed kill/close commands for every session, or nudge
commands pasting attacker-chosen text into other agents' panes. The keyed controller durably
claims and executes them *through the safety kernel*: witnesses prove target identity, never
requester authority. Fleet-wide destruction ships with perfect conservation evidence.

**Add:** a trust-model statement to S2/S5 (bead-store write access == full session-control
authority, accepted-or-not per profile); requester provenance fields on the P2.11/P6.1 schemas
+ a claim-time policy hook (v1: log+allow for local single-tenant, deny-unknown-requester for
hosted); separation of control-plane store credentials from work-plane agent credentials in the
profile taxonomy; an S28 row; and make P12.8 refuse the hosted/multi-tenant profile without an
authorization capability.

### A2 — [HIGH] Store restore-from-backup / wholesale rewind is unmodeled
The design's safety leans on durable monotonicity (intent generation "latest valid wins",
command terminal states keyed off the durable record, P11.3 epoch increments). The **only**
rewind ever tested is feed/cache-layer (P4.4 store rollback/token regression, P4.6 cursor
rewind). S9, the 26.2/26.4 matrices, S28, and P12.6 runbooks have no store-restore case; every
"corruption" row repairs projections *from* the store, never the store itself. But restoring the
store is *this deployment's sanctioned recovery*: sqlite infra store backup is `VACUUM INTO`
(your own ops rules), and Dolt resets/file-restores have happened in production.

**Failure:** infra sqlite corrupts at 3am; operator restores last night's backup. Delivered/
terminal nudge commands rewind to `pending` → the keyed controller re-claims and re-pastes
hours-old prompts into live agent sessions (the no-blind-re-paste discipline can't fire — the
durable evidence it consults is what rewound). A superseded destructive stop intent becomes
"latest valid generation" again and executes against the replacement session. Under P11 a
resumed follower and the restored store both hold "monotonic" epochs, defeating fencing.

**Add:** a per-store lineage/instance token checked at controller startup and every feed attach;
on detected rewind → refuse all effects, force full relist, quarantine every nonterminal command
to delivery-unknown/expired, and re-anchor epochs/intent generations above a recovered
high-water mark before any claim. Add a restore-drill row to 26.5 (run by P12.7 per RC), a
restore runbook to P12.6, and an S28 row.

### A3 — [HIGH] Gate-artifact tamper control is absent for an agent-executed program
131 tasks decompose into hundreds of beads executed by AI agents, and §27.1 hands each
implementer the pen on its own gate ("update the relevant SESSION-*/effect-exception rows in the
same change"). The only anti-weakening rule is scoped to Phase-12 gate *retirement*; §30 says the
plan "cannot silently supersede a SESSION-* row" but names no enforcing mechanism; checkpoint
evidence packets have no independent producer; nothing pins a ratified plan revision after G0.
Direct fleet precedent: a finalize agent squash-merged out-of-band with a shared PAT; verify
agents have mutation-tested in shared trees.

**Failure:** an implementing agent's red test is inconvenient; it edits the RC-*/matrix row or
the effect-exception inventory in the *same* PR to match its implementation; the reviewer sees a
green, internally-consistent diff; G7 later certifies a destructive family against the silently
weakened row. (Your runtime P3.10 fix covers self-reported *canary* signals — this is tamper of
the *gate artifacts themselves*, a different surface.)

**Add:** declare `IMPLEMENTATION_PLAN`/`ACCEPTANCE_MATRIX`/`internal/session/REQUIREMENTS.md`
rows and the 26.7 thresholds *protected artifacts* — a slice PR may ADD rows but not modify/delete
a row it is claimed against; row edits require the human owner (or an approver distinct from the
implementing agent), enforced by a CI path/ownership guard. Generate checkpoint evidence packets
from CI against a G0-pinned document revision, never authored by the implementing agent.
(Cheap: `cmd/gc/worker_boundary_import_test.go` already proves the CI-path-guard pattern works.)

### A4 — [MEDIUM→HIGH] Out-of-band `bd` binary / store-schema version skew is ungoverned
P0.12 pins the *gc* release (tag/commit/SHA-256); every mixed-version row concerns gc readers/
writers or feed schema. Nothing governs the **store schema**, which is created and migrated by
the external `bd` binary — and agents across the fleet run `bd` directly at whatever version
their environment ships, writing to the store that now carries the durable command ledger and
status bodies. This is a *twice-recurred* production class: control-dispatcher death via bd
schema skew minting phantom wisps (missing lease columns), and a bd v54 downgrade breaking worker
claims. `internal/beads/bdstore.go` parses `schema_version` only from *error* envelopes;
successful responses carry no version fence.

**Failure:** an agent env runs a newer `bd` that auto-migrates the schema (or older `bd` writes
pre-migration shapes); command/lease rows become invisible/malformed at the *store* layer —
below the total metadata decoder, which never sees them — so P6.1's conservation gate and the
decoder both stay green while accepted commands silently vanish. Phantom-wisp dispatcher death,
reproduced *inside* the new "durable" contract. This is common-mode invisible to the P10 audit by
construction (P10.1 reference builders reuse the same decoder over the same store).

**Add:** store-schema-version to the P4.0/P2.0 capability contracts — record the certified schema
version, fail to observation-only (never effects) on mismatch at startup/attach; extend the P0.12
manifest + 26.5 with bd-binary/schema N/N-1 cases both directions; wire the existing Beads↔GasCity
contract-test system + `deps.env BD_VERSION` pin into the T5 matrix as a *store axis*; state in S2
that unpinned external `bd` writers are outside supported profiles. Add an ENT row where the store
returns *well-formed-but-wrong* rows and require the version fence (not projection comparison) to
catch it.

---

## B. Confirmed defects still open in the current plan (verified by grep on 6195-line file)

Ordered by leverage. Each was CONFIRMED by the accuracy lens and re-checked against the live
document — none is present yet.

**B1 — [MEDIUM, provider seam] Human-attached tmux sessions entirely unmodeled.** `session_attached`,
`copy-mode`, `IsAttached` appear nowhere in either document. The tmux code carries years of scar
tissue (copy-mode cancel guard because "a human may have scrolled this pane into copy-mode" and
keystrokes were "silently dropped"; wake-if-detached), and the durable command ledger will hold
nudges targeting sessions a human is attached to and typing in. The interrupt path is unguarded
`SendKeysRaw C-c` while the copy-mode guard exists only on the debounced path. *This is squarely
the seam you were asked to bulletproof.* → Add attached-state to the P4.9 observation contract
(`#{session_attached}`); make copy-mode guard mandatory on every key-injection path as a P1.8
conformance case ("send while pane in copy-mode"); make human-attached a named S7.4 blocker
condition with an operator-visible reason.

**B2 — [MEDIUM, provider seam] Self-echo activity attribution is lost when observation moves to
P4.10.** tmux's only activity signal (`#{window_activity}`) advances on the controller's *own*
injected keystrokes; today an in-process poke registry subtracts self-echo. P4.10 moves activity
consumption into a new manager and P4.9's contract says only "activity where supported" — the
self-attribution requirement is dropped, but quiescence deadlines (S6), idle-gated wake (P7.9),
and drain-ack timing all consume it. → Promote self-attribution into the P4.9 contract; have the
P5.4 executor report each key-injection (session, ts) to the observation manager, replacing the
package-local pokes map; add a P1.8/P4.9 conformance case.

**B3 — [MEDIUM, verification] ENT scenarios never declare a required real seam.** The ~25-row
S26.4 entropy matrix has no seam/profile column; the only real-seam commitment is the aggregate
T4 line. An implementer can run every ENT row on fakes, keep the manifest green, and satisfy
S31 — re-opening the exact Layer-0 tmux/DoltLite seam the proposal itself says the confirmed bugs
live at. → Add a mandatory "required seam" column (real-DoltLite / real-tmux / real-process-SIGKILL
/ fake-acceptable), CI-check the manifest against it, and require every fault a fake injects for an
ENT row to also appear in the corresponding P4.4/P1.8 conformance suite so the fake is pinned to
observed real behavior.

**B4 — [MEDIUM, verification] Determinism lint has enumerable bypasses + no replay gate.** The
lint is import/source-based and misses three routes needing no forbidden import: `for range` over
a map in the decision core (Go randomizes order); `func`/interface fields smuggled into fact
structs (a `func() time.Time` passes "time is a value" textually while re-introducing ambient
reads); mutable package-level state in transitively imported helpers. → Ban `range` over map
types in decision packages (provide sorted-key helpers); reflect-walk fact/action types and reject
func/chan/unapproved-interface fields; apply the import/state scan transitively; add a T1 job that
executes every corpus fixture N times and asserts byte-identical normalized plans.

**B5 — [MEDIUM, verification] Post-migration nudge liveness gated only by terminal-state
conservation.** After P12.1 deletes the legacy owner, the only permanent nudge-liveness gate is
"every command reaches a visible terminal state" — and `delivery-unknown` is a *legal* terminal
state. No delivered/accepted ratio SLI survives into the permanent metric set. → Add a permanent
per-provider delivered/accepted SLI with a certified-envelope threshold to §10 and the 26.7
abort table; add a conformance case where, with a fake whose probes resolve, `delivery-unknown`
is *unreachable* (may appear only when resolution is made impossible).

**B6 — [MEDIUM, migration] Shadow parity lacks input-watermark matching + a quantitative
false-parity bound.** Promotion requires "no unexplained effect diff", but legacy decides from a
pass-start snapshot up to ~4 min stale while the shadow decides from the fresh typed cache, so
under normal churn a large fraction of comparisons diverge for pure timing reasons, and the
"explained" exception list is open-ended human sign-off with no volume bound. → Require production
shadow comparisons to be computed from the *same* durable/config/runtime watermarks (P0.6 fixture
schema already defines them); require every approved exception to be a mechanical matcher citing
the specific watermark delta or fail-safe rule; add a quantitative false-parity ceiling as a gate.

**B7 — [MEDIUM, migration] P3.4 completeness is only as complete as its hand-maintained
effect-call pattern list.** `cmd/gc` already has 30+ direct `exec.Command` sites (bd, dolt, pack
tooling), so arbitrary process execution is normal; a session-affecting effect introduced through
an unrecognized vehicle (fresh `exec.Command` of tmux, `os.RemoveAll` of a runtime dir, a new
helper wrapping the provider) never registers and escapes the differential oracle. → Make P3.4
deny-by-default at the import/capability level: only registered files may import `os/exec`,
`internal/proctable`, provider packages, or reference tmux binary/socket constants — extend the
existing `worker_boundary_import_test.go` mechanism (already proven in CI).

**B8 — [MEDIUM, store] Metadata-CAS emulation makes colocated claims/leases starvable.** The only
production-capable `CompareAndSetMetadataKey` (BdStore) is emulated over the whole-bead revision
fence: any unrelated-key write to the same bead between read and fenced write is a spurious
precondition, bounded at 4 attempts then error. The plan prescribes metadata-key CAS for
claims/leases/reservations (P6.5/P8.6/P11.3) with no rule constraining those keys to dedicated
low-write beads. → Add a normative key-placement rule to P0.8: every CAS-emulated claim/lease/
reservation key lives on a dedicated bead whose only writers are that key's CAS participants;
forbid colocation with the owned status map or any high-frequency writer; add a CAS-under-
adjacent-write conformance/perf row.

**B9 — [MEDIUM, perf] Enqueue-every-key fallback is bounded only by a metric.** The safe
"broad/unclassified change → enqueue every key in scope" fallback (S6/P4.7/P5.1) has no threshold,
no promotion gate, no 26.7 abort row, and no PERF profile exercising the shape most likely to be
lazily mapped: config edits (frequent in this fleet via fsnotify reload). → Add a per-source-class
fanout budget certified in P0.7; a 26.7 row aborting canary expansion when the fallback rate
exceeds budget; a PERF-OPCOUNT variant for the top config-edit shapes requiring exact-key mapping.

**B10 — [MEDIUM, crash] P1.1 still emits `EventTerminated` before the durable terminal write.**
In `internal/convergence/reconcile.go` the terminate path publishes `EventTerminated` before
writing `state=terminated` and before `CloseBead` — the inverse of INV-23 (events after durable
commit) — and orders (P9.2) are event-triggered consumers of exactly this event. P1.1's acceptance
covers only *marker* persistence. → Since P1.1 already edits this function, extend its acceptance
to reorder emission after the durable write + CloseBead in the same slice (a two-line move under
the same crash-injection verification).

**B11 — [MEDIUM, consistency] Three terminology collisions that will drift a multi-agent build.**
No glossary exists (verified: 0 hits). (a) **"generation"** means both a monotonic staleness fence
(~40 uses) and a *body checksum* for torn-write detection (§4.6 "hash/generation trailer", P2.9).
Rename the latter to "status body checksum" and reserve "generation" for monotonic fences.
(b) **"controller epoch"** has three readings: §7.2 makes it HA-only, §2 states a core safety
property in terms of it *unconditionally*, P1.5 requires Phase-1 witnesses to carry it, P0.11 uses
a per-family owner "epoch" for handoff. Define one Epoch: pre-HA it *is* the P0.11 owner epoch (or
explicitly absent with witness validation defined to skip it); under HA it becomes the P11.3 lease
epoch. (c) **action-family census** disagrees across P7.10 (live/restart/provider-swap are separate
flips), P7.13 (collapses them, omits provider swap), and P12.1 (re-expands them) — make P7.13's
order the single authoritative census and have the others reference it by name. Also: §12
dependency-branch letters (B/C/D/E/F) collide with §27.2 workstream letters with different
meanings — renumber one. Add one terminology table to §7.

**B12 — [MEDIUM, consistency] S8.7/INV-07 same-key exclusion is unscoped across controllers.** The
lifecycle and nudge controllers share the session-ID keyspace (S6); the invariant text — what test
suites are built from — doesn't say whether exclusion is per-logical-controller or global. The real
cross-controller same-session guarantees (executor-only provider mutation, owned field groups) live
scattered in S4.5/P3.1/P2.9. → Rewrite the invariant where it lives: "The same (logical controller,
key) pair is never reconciled concurrently; distinct controllers may reconcile the same session
concurrently, exclusive only through the shared per-session executor (S4.5) and owned field
groups/conditional writes (P2.9/P3.1)." Add a cross-controller interleaving test.

**B13 — [MEDIUM, complexity] Config-knob sprawl with no small-city safe-defaults gate.** Dozens of
new tunables (lane reserves/borrowing, provider caps, retry caps/jitter, audit budgets/partition
sizes, feed backpressure, census/watch intervals, expectation expiry, quarantine TTL, SLO windows)
+ a 4-profile × per-store × per-provider matrix, on an SDK where most cities are laptop-scale, with
no zero-config default requirement. → Add to P0.2/P12.8: every knob ships a small-city-certified
default; a default zero-config install must pass the full entropy suite + meet SLO on reference
small hardware; knob count is reported in every G* evidence packet.

**B14 — [MEDIUM] HA fencing precision gaps (2 confirmed, both real but Phase-11-deferred).**
(a) *Per-shard lease epochs are not composed into per-key monotonic order.* When key K moves
between shards, S1 and S2 have independent epoch counters, so a per-key fence either rejects the
legitimate new owner or requires a takeover write-storm. Define the fencing token structurally in
P11.3 as a per-key-comparable tuple `(shard-map version, shard epoch)` compared lexicographically,
with the shard-map version itself fenced. (b) *Cold/host-pinned failover for unfenceable providers
(tmux/subprocess) lacks a proof-of-death promotion precondition* — a paused (SIGSTOP/D-state) old
owner is indistinguishable from a dead one. Add to P11.4: takeover requires positive proof the old
owner cannot act (same-host: SIGKILL the exact PID+start-time and confirm reaping; cross-host:
refuse automatic promotion for host-local providers). These are correctly Phase-11-gated but should
be pinned in the contract now so P11.0's spike tests the right thing.

---

## C. Residual risks for explicit owner sign-off (largely inherent — more gates won't remove them)

These are for Julian to accept consciously, not necessarily to fix. The residual-risk critic
called this "one of the most rigorously self-aware migration plans I have reviewed" — the risk is
concentrated in what remains *after* every gate is green.

**C1 — P5.4A is the single highest new-bug-risk step, and its rollback is the least credible in
the plan.** Before any family cutover delivers value, P5.4A reroutes 100% of production session
mutations across four process classes through brand-new cross-process concurrency machinery
(permits, same-key exclusion, AmbiguousInFlight charges, cross-process claims) while legacy
decision logic still drives it. A subtle exclusion deadlock / permit leak / park-without-wake bug
presents as *fleet-wide mutation stall with new failure signatures nobody attributes to the
invisible substrate* (cf. the 114s drain head-of-line block; "molecule stalls while the dispatcher
looked healthy"). Its stated rollback ("ownership mode changes only after calls drain/fence")
offers no path *off* the bridge — reverting means un-rewiring dozens of call sites in multiple
processes under incident pressure, drilled by no 26.5 row. **Recommend:** bridge one effect family
at a time with a per-family compiled-in kill switch that restores the direct legacy call path; add
an explicit un-bridge rollback drill to 26.5 run in canary before the second family bridges; soak
the bridged-but-legacy-decided config with a dedicated mutation-stall-age abort signal before P6.6.

**C2 — The migration-window exposure integral is never computed for the owner.** At ~monthly
historical incident cadence over a plausible 12–24-month program, expected incidents *during* the
mixed-owner window likely exceed the incidents prevented in the first post-completion year, and
every intermediate mixed-owner config is a novel production state alive for weeks-to-months. P1.0B
now banks an interim head-of-line win (good — that was a top finding, addressed), but this is still
worth making explicit. **Recommend:** a G0 exposure ledger the owner signs — incident-class ×
phase-that-retires-it × projected calendar date, with the expected-incident math for the window.

**C3 — Roughly half the historical incident classes are outside the redesign's scope, and the new
control plane will look *healthier* during them.** Orphan-close (P1), stale nudge PIDs (P6.8),
head-of-line (P7/P9), isCold demand treadmill (P8) are addressed. But wedged-worker molecule
stalls, fleet-wide LLM/manifold 502 stalls, and retry-treadmill root-minting are worker-layer /
workflow-policy failures the reconciler cannot see — a perfect control plane keeps perfectly
healthy queues while no work progresses, *weakening* the operator's current "slow cycle = trouble"
heuristic. **Recommend:** an incident-coverage table in P0.6 (every named incident → prevented /
detected-faster / out-of-scope, owner-signed) + one small owned task: a work-progress SLI (oldest
hooked-bead-without-agent-output age, per rig) that survives patrol demotion and pages independently
of controller health.

**C4 — The composition of the new liveness mechanisms is the new god-function, tested only in
non-merge-gating tiers.** One session key can simultaneously be charged-ambiguous on a provider,
parked on a blocker, held behind a write watermark, and inside a closing admission generation — and
the plan defines each mechanism's wake rules *separately*, never the composed release order.
Cross-mechanism livelock (circuit refuses the probe that would resolve the charge that holds the
park that blocks the watermark reread) is exactly what unit gates can't see; §26.1 exercises the
assembled system only in T4 nightly + T6 canary, and 26.7's stall detector exempts these keys
because parked/charged keys are *classified*, hence not "ready". Production showed this grammar once
at lower mechanism count (isCold). **Recommend:** one owned artifact — a composed blocked-key state
machine (all mechanisms on one key, every legal combination + its mandatory wake edge) with a
model/property test that no reachable composite state lacks a live wake source, promoted to T1/T2
merge-gating; plus a max-age alarm on *classified* (not just unclassified) blocked keys per class.

**C5 — Post-P12 deletion is roll-forward-only, and the one-release soak is calibrated to calendar
time, not rare-event exposure.** The keyed architecture's most dangerous latent bugs live in
rare-event paths (cursor compaction/relist, backward clock jumps, provider swap racing a config
generation, mass timer expiry) that may occur zero times in a given release. **Recommend:** gate
each P12.1 family deletion on observed production *occurrence counts* of its rare-event classes
(inject synthetically in canary), not one calendar release; keep stop/close + the P9.5A shutdown
owner compilable-but-flag-disabled for two-plus releases with "resurrect deleted family owner" as
a drilled procedure.

**C6 — The plan is an unstable contract with no per-slice revision pinning.** It grew ~24% during
this review by absorbing findings; two beads executed months apart will silently target different
contracts (task IDs, invariant numbering, owner tables, dependency edges have already shifted — the
census/lettering collisions in B11 are early symptoms). §27.1 requires beads to cite RC-* rows but
not a plan revision. **Recommend:** at G0, content-hash the ratified plan + matrix; require every
implementation bead and evidence packet to cite the hash it targets; route post-G0 changes through
a delta-approval log with a CI check that a bead citing a superseded hash can't merge until rebased.
(This is the machine-enforcement half of A3 — one day of tooling that makes the plan the versioned
contract the rest of the design already assumes it is.)

---

## What the verifiers confirmed you already addressed (don't redo)

The following raw blocker/high findings were verified as **substantively fixed** in the live plan:
per-family two-writer window / same-bead status lost-update (P5.4A + P2.10A + INV-23);
tmux durable-identity fencing + `$session_id`/`%pane_id` provider-atomic addressing + same-name-
within-1s conformance (P1.4/S7.3/INV-28); tmux server-loss false-death (P1.2/P1.5 RuntimeScopeLost
+ GC_SESSION_ID process-scan veto); tri-state can't-model-tmux (P1.2 server-absent/degraded/exited-
pane/partial-census); AmbiguousInFlight wake + permit accounting + circuit (S7.4 blocker condition +
P5.4 + P5.5A); parked-key new-intent (INV-15 + S7.4 atomic clear); P2.9 marker-repair livelock +
non-CAS foreign-write criterion; write-watermark satisfaction under rollback/relist/no-revision;
config-rollback vs owner-handoff precedence (P0.5/P0.11); flip-vs-in-flight-pass fencing (P0.11);
residual-legacy validation + LIFO rollback (P7.13); pre-HA dual-controller refusal (P0.11A);
conditional-writes per-store-class matrix (P2.0); sqlite store class in taxonomy + composite profile
(S2/P4.1/P4.5); wisp-tier command durability (P6.1/P4.5/P9.3); CachingStore multi-ingress single
owner (P4.3B); 50/100ms SLO excludes pre-provider store writes (S10 controller-overhead gate);
action_ready_at per-blocker-class definition (S7.1); invariant→gate traceability + red-nightly
freeze (P0.9); self-reported canary signals (P3.10 independent observer); hand-registered crash
points → statically derived (P0.10); interim head-of-line relief (P1.0B); feed-gates-cutover →
G4A/G4B split; effort estimate + stop-with-value (G0 forecast + §27.5); operator diagnosis surface
(P5.10); nudge decoupling contradiction (P5.1N). **That is ~25 of the sharpest findings already
closed** — the review's main residue is Buckets A–C above.

---

*Full per-finding evidence, verifier verdicts, and the 21 medium-severity clusters not expanded
here are in `/tmp/review-synthesis/`.*
