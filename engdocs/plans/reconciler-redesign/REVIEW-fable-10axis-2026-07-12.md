# Reconciler Redesign — Exhaustive 10-Axis Design Review (Fable)

| Field | Value |
|---|---|
| Date | 2026-07-12 |
| Reviewed artifacts | PROPOSAL.md + IMPLEMENTATION_PLAN.md, snapshot sha256 `c12ac67a5a29…` (4,687 lines, 131 tasks) at code head `fe4edc869` |
| Method | 10 independent axis reviewers → dedup (76 findings → 71 clusters) → adversarial verification: 2 independent lenses per blocker/high (factual-accuracy skeptic + already-addressed skeptic), 1 per medium → 2 completeness critics (coverage gaps, residual risk). 118 agents, ~13M tokens, every verdict grounded in exact quotes from the plan and `internal/{runtime,beads,session,worker}` / `cmd/gc` code. |
| Axes | concurrency-ordering · crash-durability · ha-split-brain · provider-seam-tmux-cli · migration-strangler · store-reality · performance-capacity · verification-program · complexity-operability · internal-consistency |
| **Important context** | The plan was **actively hardened while this review ran** (4,687 → 6,195+ lines, sha `a4a4e44ef6a5…` at report time, plus the new ACCEPTANCE_MATRIX.md). The verifiers checked every finding against the LIVE text, so this report separates: (1) what is still open, (2) what your concurrent edits already fixed — now independently verified with quotes, and (3) new gaps neither pass caught. Where line numbers are cited they may have drifted; task IDs are stable. |

## Verdict

The architecture is sound and the plan is unusually rigorous — several reviewers independently called it the strongest verification/migration program they had evaluated, and the review **confirmed the fixes you landed mid-review are substantive, not cosmetic**. No finding requires re-architecting. What remains falls in three buckets: (A) 21 confirmed open items + ~12 verified residual gaps in contested items — almost all fixable with contract-text amendments before implementation; (B) 4 genuinely new coverage gaps from the critics (authorization of the command surface, store restore/rewind, gate-artifact integrity, bd schema-skew fencing) — two of which map to twice-recurred production incidents; (C) a residual-risk posture the owner should sign consciously (migration-window exposure math, P5.4A blast radius, out-of-scope incident classes).

**Recommendation: address Part 1 + Part 2 as plan amendments before G0 ratification. Do not begin implementation of Phase 2+ until the Part 2 items have owning tasks.** P1.0B/W0-class safety fixes can proceed.

---

## Part 1 — OPEN items (verified absent from the current ~6,195-line text)

### 1.1 Confirmed by both adversarial lenses

**OPEN-1 [HIGH] Pre-HA two-controller startup refusal — verify P0.11A closes it completely.**
S9 (“Before HA: startup refusal”), ENT 26.4 (“Single-owner preflight”), and S31 (“a second executor fails closed”) promise a mechanism that, at review time, had **no owning task** — today it is a host-local flock on `.gc/controller.lock` (cmd/gc/controller.go) + port-bind check, which production already defeated (the ~74s competing-supervisor SIGKILL war: different GC_HOME/RuntimeDir/mount namespace, same store + tmux socket). `P0.11A` appeared during verification — confirm it includes ALL of: (a) inventory of the current exclusion surface and its known failure modes (per-inode flock scope, distinct runtime dirs, test binaries, PID reuse in doctor.IsControllerRunning); (b) a **store-anchored controller-instance claim** (heartbeat + CompareAndSetMetadataKey in the authoritative store, post-P2.0) so a second executor pointed at the same working set refuses effects regardless of host/mount topology; (c) the 74s-SIGKILL incident and the two-directory/one-store topology as named ENT regression scenarios; (d) S31’s checkbox citing the task. Keyed-executor overlap is far more destructive than serial-loop overlap — two executors race starts, stops, and witnessed closes.

**OPEN-2 [MEDIUM] S8 invariant “same key never reconciled concurrently” (INV-07) is unscoped across controllers.**
S6 gives lifecycle and nudge controllers the same session-ID keyspace; the invariant text doesn’t say per-logical-controller vs global. Both misreadings produce real defects (global → deadlock-prone cross-controller lock; per-controller assumed silently → unserialized same-session work outside the executor). Rewrite the invariant: “The same (logical controller, key) pair is never reconciled concurrently. Distinct controllers may reconcile the same session concurrently; their provider mutations are exclusive only through the shared per-session executor (S4.5/P5.4), their durable writes only through owned field groups and conditional writes (P2.9/P3.1).” Add a cross-controller interleaving test.

**OPEN-3 [MEDIUM] P1.1 leaves `EventTerminated` emitted before the durable terminal write.**
internal/convergence/reconcile.go emits EventTerminated *before* state=terminated and CloseBead — the inverse of the events-after-commit invariant — and orders (P9.2) are event-triggered consumers of exactly this event. P1.1 already edits this function; extend its acceptance criteria to reorder emission after the durable terminal write in the same slice, or record a named legacy exception with owner + expiry (migration rule 7).

**OPEN-4 [MEDIUM] Per-shard lease epochs are not composable into per-key monotonic order.**
Epochs increment per shard-lease acquisition (P11.3), but the fence compares per key. When key K moves shard S1→S2 (rebalance, shard-map version change), S1/S2 counters are independent: the fence either rejects the legitimate new owner or admits the stale one. Define the token structurally: lexicographic (shard-map version, shard epoch), the map version itself fenced, and lease acquisition under a new map version starting above any epoch reachable under the old mapping.

**OPEN-5 [MEDIUM] Cold/host-pinned failover (P11.4) lacks a proof-of-death promotion precondition.**
For unfenceable providers (tmux/subprocess) P11.4 offers a “cold or host-pinned failover profile,” but no task defines what proves the old owner cannot act (SIGSTOP’d/D-state controller is indistinguishable from dead). Same-host: SIGKILL exact PID+start-time and confirm reaping before promotion, PID identity captured at lease acquisition. Cross-host: refuse automatic promotion for host-local providers. The profile is also absent from the S2 table — name it or delete it.

**OPEN-6 [MEDIUM] Human-attached sessions are entirely unmodeled.** *(provider seam — the axis you care most about)*
`IsAttached`, `#{session_attached}`, copy-mode scar tissue (tmux.go: keystrokes silently dropped when a human scrolled a pane into copy-mode; `cancelCopyModeIfParked` exists only on SendKeysDebounced — the interrupt path is unguarded raw C-c) — none of it appears in P4.9’s observation contract, S7.4 blockers, P6.3 delivery rules, P7.4C, or P1.8 conformance. Durable nudge commands will paste into panes humans are typing in. Add attach state to the P4.9 contract; make “send while pane in copy-mode” a P1.8 conformance case; make human-attached a named blocker condition with per-family delivery rules.

**OPEN-7 [MEDIUM] Activity self-echo attribution dropped.** *(provider seam)*
tmux’s only activity signal (`#{window_activity}`) advances on the controller’s own injected keystrokes; current code compensates via an in-process poke registry (tmux.go:233-244). P4.10 moves activity consumption into the new observation manager, and quiescence deadlines (S6), wait-idle (P6.1), and idle timers (P7.9) consume it — but the self-attribution requirement is not in the P4.9 contract. Once executor and observer are separate components, every controller nudge looks like agent activity: quiescence never satisfied / idle never fires. Promote self-attribution into P4.9 (executor reports each key-injection to the observation manager); add a P1.8 case.

**OPEN-8 [MEDIUM] Shadow parity lacks watermark matching and quantitative false-parity bounds.**
Legacy decides from pass-start snapshots up to ~4 min stale; the shadow decides from the fresh typed cache — so bulk timing divergence is adjudicated by an unbounded human exception list (“two-maintainer sign-off + expiry”, no volume bound). Require production shadow comparisons to be input-watermark-matched (the P0.6 fixture schema already defines the watermarks); every approved exception must be a mechanical matcher citing the specific watermark delta or fail-safe rule; add a quantitative unexplained-divergence budget to promotion gates.

**OPEN-9 [MEDIUM] P3.4’s “effect call” definition is a hand-maintained pattern list.**
cmd/gc has 31 non-test files with direct `exec.Command` — a session-affecting effect introduced through an unrecognized vehicle (fresh exec of tmux, os.RemoveAll of a runtime dir, a new provider wrapper) never joins the oracle. Strengthen to deny-by-default at the import/capability level: only registered files/packages may import os/exec, internal/proctable, provider packages, or reference tmux binary/socket constants — extend the existing `cmd/gc/worker_boundary_import_test.go` mechanism, which already proves this pattern works in CI.

**OPEN-10 [MEDIUM] No park-state table for the migration.**
§27.5 stop-with-value landing points (added mid-review) cover value banking, but nothing declares which configurations are safe to hold **indefinitely** vs states that must not persist. Mid-P7.13 (some families flipped) is the most complex state in the entire arc; it currently has no wall-clock budget and no defined collapse action (roll forward to next park state or LIFO-rollback). Add the park-state table to §27 and reconcile with §27.5.

**OPEN-11 [MEDIUM] CAS-emulated claims/leases are starvable via colocated-key interference.**
The only production CompareAndSetMetadataKey (BdStore) is emulated over the whole-bead revision fence: any unrelated-key write to the same bead between read and fenced write is a spurious conflict (4 attempts, 25-200ms backoff, then CASRetriesExhaustedError). The plan prescribes metadata-key CAS for claims/leases/slot reservations (S4.6, P8.6, P11.3) with no key-placement rule. Add: every CAS-emulated claim/lease/reservation key lives on a dedicated low-write bead whose only writers are the CAS participants; add a conformance row exercising CAS under adjacent-key write load.

**OPEN-12 [MEDIUM] Enqueue-every-key fallback is bounded only by a metric.**
“Must not remain the normal path” has no threshold, no promotion gate, no S26.7 abort row, and no PERF shape for the change class most likely to be lazily mapped — config edits (P4.7 explicitly permits broad unclassified scopes; fsnotify reload makes them frequent). Add a per-source-class fanout budget certified in P0.7, an S26.7 abort row on budget breach, and PERF-OPCOUNT variants for the top three config-edit shapes requiring exact-key mapping.

**OPEN-13 [MEDIUM] ENT scenarios never declare required real seams.**
No ENT row states whether real DoltLite/real tmux is required or a fake is acceptable; the only real-seam commitment is the aggregate T4 line. The proposal’s own red-team finding (“confirmed bugs live at the Layer-0 seam the fake stubs out”) is re-openable: all ~25 ENT rows can run green on fakes. Add a mandatory required-seam column to the S26.4 manifest, CI-checked, and require every fake-injected fault class to also appear in the corresponding P4.4/P1.8 conformance suite so fakes stay pinned to reality.

**OPEN-14 [MEDIUM] Determinism lint has enumerable bypasses.**
Import/source lint misses: `range` over maps in decision packages (randomized iteration), func/interface fields smuggled into fact structs (`func() time.Time` passes “time is a value” textually), and mutable package-level state in transitively imported helpers. Add: map-range ban with sorted-iteration helpers, reflect-walk of fact/action types rejecting func/chan fields, transitive import/state scan, and a T1 job executing every corpus fixture N times asserting byte-identical normalized plans.

**OPEN-15 [MEDIUM] Post-migration nudge liveness is gated only by terminal-state conservation — and `delivery-unknown` is a legal terminal state.**
After P12.1 deletes the legacy owner, unbounded delivery-unknown inflation passes every permanent gate (S26.7 conservation needs *missing* commands; latency SLOs stop at T8). Add a permanent per-provider delivered/accepted ratio SLI with certified-envelope threshold to §10 and the S26.7 abort table, plus a conformance case: with a provider fake whose probes resolve, delivery-unknown must be unreachable.

**OPEN-16 [MEDIUM] Config-knob sprawl with no safe-defaults requirement.**
Dozens of new tunables (lanes, reserves, budgets, TTLs, intervals, envelopes) + a 4-profile × per-store × per-provider matrix, in an SDK whose typical city is laptop-scale. Require: every knob ships with a small-city-certified default; a zero-config default install passes the full entropy suite and meets the SLO on reference hardware; knob count reported per G* evidence packet.

**OPEN-17 [MEDIUM] The permanent verification program is never right-sized.**
Seven tiers, N/N-1 binary matrix, bounded-model reporting, ten PERF profiles — permanent per the DoD, with deletion tasks only for migration gates and the oracle. Add a P12 task that right-sizes the permanent set from evidence and reports verification LOC / CI minutes / artifact storage per checkpoint.

**OPEN-18 [MEDIUM] “Controller epoch” has three incompatible definitions.**
§7.2 (HA-only) vs §2 (unconditional safety property) vs P0.11 (per-action-family owner epoch). P1.5 witnesses (Phase 1, pre-HA) must carry “controller epoch” and fail wrong-epoch validation — which epoch? Define once in §7: pre-HA the controller epoch IS the P0.11 owner epoch (or explicitly absent, with validation defined to skip); under HA it becomes the P11.3 lease epoch, with a compatibility rule and a pre-HA test vector.

**OPEN-19 [MEDIUM] “Generation” names both a monotonic fence and a body checksum.**
§4.6/P2.9 “generation/body checksum marker” (content integrity) vs ~40 other uses (monotonic staleness fence). Rename the trailer to “status body checksum”; add one glossary table to §7 defining generation / incarnation / epoch / revision / cursor / watermark (three unlabeled senses!) / expectation / witness.

**OPEN-20 [MEDIUM] Action-family census disagrees across P7.10, P7.13, and P12.1.**
P7.10 mandates three separate flips (live-config, restart-required, provider swap); P7.13’s authoritative graduation order collapses them and omits provider swap; P12.1’s deletion ledger re-expands them while asserting “no family disappears between the owner table and deletion ledger.” Make P7.13’s order the single census at full sub-family granularity; make the other lists reference it; add a mechanical registry check.

**OPEN-21 [LOW] §12 dependency-branch letters collide with §27.2 workstream letters** (D/E/F have different meanings). Renumber one namespace or replace §12 letters with task IDs.

### 1.2 Verified residual gaps in contested items (the fix landed but a specific hole remains)

**RES-1 [MEDIUM] Cross-bead epoch fencing mechanism unresolved (P11.5/P11.3).** P2.0’s vocabulary is strictly per-bead (UpdateIfMatch/CloseIfMatch/DeleteIfMatch/CompareAndSetMetadataKey all take one bead id) — there is no “write bead X iff lease bead L holds epoch E.” A quiescent bead’s unchanged revision accepts a stale owner’s UpdateIfMatch after lease transfer. P11.0/P11.5 now demand the right *tests*, but the *mechanism* is still misattributed to “P2.0 conditional operations.” Decide now: (a) a cross-bead/transactional conditional as a new beads verb + named HA store capability, or (b) per-bead epoch-stamping with the takeover write-storm and stamping-window measured. Delete the wording implying stock P2.0 suffices.

**RES-2 [MEDIUM] Phase-9 sharded writers still unenumerated in fence verification.** P11.0 spike verbs, P11.8 fault list, and G11 acceptance name only create/stop/nudge/status/command-ack. Stale-GC-delete (DeleteIfMatch batch under a lost lease = permanent work-bead loss), stale-dispatch-tally (double-completed workflow step), stale-order, and stale-extmsg (unretractable external double-delivery — extmsg has no fencing treatment at all) are never exercised. Add the exhaustive durable-write/external-effect family table derived from the P0.1 inventory, each classified {epoch-fenced | revision-fenced-only + why safe | shard-ineligible → dedicated singleton lease}, and extend P11.0/P11.8 with those cases.

**RES-3 [MEDIUM] Profile enforcement is still startup-only for two real failure modes.** (a) No durable working-set identity token: the CAS probe parses `bd --help` flags, not which database the CLI is bound to — the mc `dolt.port`-drop incident (silent fallback to a different store, all probes green) reproduces as fenced-HA split-brain across two databases with every fence green. (b) No epoch/revision regression detection: a store restored from backup regresses lease epochs below cached values with no signal (S26.7’s fork row covers feed cursors only). Add: store/cluster UUID minted at provisioning, pinned in city config, embedded in the lease record, verified per connection and per lease renewal; a high-water-mark regression rule (“coordination-store epoch/revision regression → collapse to one fenced owner; HA disabled”) as an S26.7 row; the config-drift/wrong-store topology as a P11.8 fault case.

**RES-4 [MEDIUM] Crash-boundary program still omits control/formula + order suites and cross-store rows.** 26.2’s preamble names five suites; 26.3 has no control/order row; no B-row models a two-store sequence (create children in rig Dolt store, close control bead in sqlite graph store — the phantom-wisp / stranded-children incident family). P0.10’s derived-injection rewrite mitigates mechanically *once those owners register*, but the outcome rows (no duplicate child materialization; open roots re-dispatch idempotently; a closed root never strands dispatchable children) and named cross-store idempotency keys for P9.1 child slices are still missing.

**RES-5 [MEDIUM] Split-store rulings still owed.** S29 needs the owner’s ruling: does the production sqlite(graph/infra)+Dolt(work) split consolidate before HA, or stay permanently single-owner? And P11.1 must state which store class anchors the lease and that a lease in store A cannot fence writes to store B (per INV: cross-store atomicity is not promised) — otherwise an implementer can read “one linear working set” as satisfied per-store.

**RES-6 [LOW] Assorted, one line each:**
- **bd #4682**: give the cross-repo revision-column dependency an explicit S29 decision deadline (P2.0’s matrix now names it, but no date/owner).
- **Generation supersession vs quarantine** (P5.6/P8.5): state that supersession resets only provider-error retry state — durable holds/quarantine clear only via TTL or operator reset; add a writer-side diff-gate so pool churn cannot mint no-op generations (treadmill risk).
- **Bulk-stop ordering** (P7.11/P7.12): city-wide stop is now durable (P9.5A) — non-city-wide bulk stops still lose reverse-dependency order on crash; derive ordering from a “no live dependents” blocker over durable state, add a crash-mid-bulk-stop row.
- **Lease time model** (P11.3): define the assumed max clock skew as certified config, how it’s measured, and the runtime response when exceeded; add a clock-skew abort row. (Authoritative-time branch is feasible only via `bd sql` on hosted Dolt; sqlite/DoltLite are bounded-clock-only — say so.)
- **tmux census profile** (P4.9/P4.10): declare tmux census-only; pin census interval + measured cost in P0.7; codify the split the code already implements (census authoritative for session existence, `ProcessesAvailable=false` → Unknown only for process facts).
- **Ready-key alert** (S10 vs S26.7): S10 says *any* ready key >1s alerts; 26.7 keys on *unclassified*. Pick: alert immediately on unclassified only; per-classification age budgets for the rest.
- **Audit traversal** (P10.2): deterministic from partition zero + disposable progress + restart churn = the same late partitions starve forever. Randomize traversal order per traversal seed; track per-partition coverage age; couple declared-recovery-bound to measured restart cadence.
- **T0→T1 regression** (P4.4): outbox/change-table candidates add write amplification to a 1-2s store; bound commit-latency regression vs the P0.7 pre-feed baseline in PERF gates.
- **Incident corpus** (P0.6): the diagnostic-only escape hatch has no graduation obligation. Add the named-incident table — gc-hz0nu, #3872, #3849, isCold treadmill, dual-supervisor, stale nudge PIDs, bd schema-skew phantom wisps, zombie-mail flood, drain head-of-line — each mapped to an executable fixture/ENT row that must exist before the checkpoint of the phase owning that behavior.
- **Reference model axioms** (P3.7): add an axiom-fidelity gate — each environment rule the model assumes (write atomicity, feed ordering/duplication, ack ambiguity) names the conformance case that establishes it; dual-run shared histories through model and conformance-tested fakes.
- **Scale certification** (S26.6/DoD): 1M-object RC budgets are mandatory while the fleet is dozens of sessions; gate 100K/1M profiles and fanout paging on measured capacity need (the plan’s own P11 entry-gate logic).
- **Supersedes ledger** (P0.2): enumerate every PROPOSAL §3-§4 element with a disposition (preserved / superseded-by-task / renamed) — the prefix-ownership table, spec create-once immutability, `gc.ext` never-read rule, ApplyStatus and DeathCertificate renames currently have none.

---

## Part 2 — NEW gaps (found by neither the plan nor the axis reviews; from the completeness critics)

**NEW-1 [HIGH] No authorization/requester model on the durable command surface.**
The plan converts session mutation into durable store objects the controller faithfully executes, but never models WHO may mint them. P2.11’s request schema (request ID, session binding, phase, generation, compat version) has no requester identity; every “requester” in the plan is the self-termination *fence*, not authority. Commands are beads; every fleet agent runs `bd` directly; the production work store is a network-reachable Dolt server. A prompt-injected worker (or any credential holder) mints well-formed kill/close/nudge commands for every session and the keyed controller executes them *through* the safety kernel — witnesses prove target identity, never requester authority — with perfect conservation evidence. Add: a trust-model statement in S2/S5 (store write access = session-control authority, accepted or not per profile); requester identity/provenance fields in P2.11/P6.1 schemas + a claim-time policy hook (log+allow for local single-tenant; deny-unknown for hosted); control-plane vs work-plane credential separation in the profile taxonomy; P12.8 refuses hosted/multi-tenant profiles without it.

**NEW-2 [HIGH] Store restore-from-backup