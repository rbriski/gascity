# Reconciler Redesign — "Windshield GT"

Blue-sky, first-principles redesign of the Gas City reconciler cluster into a
simple, testable, high-assurance core (Erlang/OTP + Kubernetes-reconciler
inspired). The original proposal was produced 2026-07-08 by a 23-agent
multi-model workflow; the canonical target and delivery sequence were hardened
against current `origin/main` on 2026-07-12.

- **[IMPLEMENTATION_PLAN.md](IMPLEMENTATION_PLAN.md)** — the incremental,
  test-first delivery plan that preserves the proposal's safety kernel while
  replacing its fleet-global scheduler with revisioned projections, stable keyed
  queues, bounded concurrency, and independent anti-entropy. **Start here; this
  is the canonical target architecture.**
- **[ACCEPTANCE_MATRIX.md](ACCEPTANCE_MATRIX.md)** — named CLI, tmux, process,
  store, queue, migration, and failure scenarios every implementation slice
  must prove. Broad phase prose never overrides these contracts.
- **`PRE_G0_CANDIDATE_MANIFEST.json`** — the machine-checked, content-addressed
  contract for the four narrowly permitted pre-G0 safety slices. It binds the
  execution base and exact task/row hashes but explicitly is not G0
  ratification or a cryptographic owner signature.
- **[EXTERNAL-REVIEW-fable-10axis.md](EXTERNAL-REVIEW-fable-10axis.md)** — the
  independent 10-axis/adversarial review of the pre-ratification plan. Its
  verified open findings are incorporated into the canonical plan and matrix;
  the report remains immutable review evidence, not a competing contract.
- **[REVIEW-fable-10axis-2026-07-12.md](REVIEW-fable-10axis-2026-07-12.md)** —
  the compact review ledger and disposition source used during hardening.
- **[PROPOSAL.md](PROPOSAL.md)** — the original design seed and decision
  lineage. Its serial fleet scheduler, one-incarnation model, boolean probe
  adapters, and check-then-kill claims are superseded by the implementation
  plan and acceptance matrix.
- **`evidence/`** — raw structured outputs of all 23 agents:
  - `final.json` — the historical Fable-finalize output that seeded the
    proposal; it is evidence, not current authority
  - `redteam.json` — the 4 adversarial Opus passes that shaped it
  - `judges.json` — 3 Opus judges scoring the 4 competing designs (T3 won unanimously)
  - `designs.json` — the 4 competing Fable architectures (T1 generic / T2 OTP-FSM / T3 functional-core+DST / T4 SSA-pipeline)
  - `synth.json` — the pre-red-team synthesis
  - `explore.json` / `research.json` — the 6 current-state maps + 4 external-inspiration syntheses

## One-line north star

> Observe the world once and honestly per pass (doubt is `Unknown`, never
> `Dead`), decide purely over typed per-session state, and let every destructive
> or lossy action exist only as a value carrying proof of a corroborated
> observation — claiming exactly the guarantees the real store and runtime
> provide, and not one more.

## Key facts

- Much of the pure session core is already landed, but the production worker
  and runtime observation boundaries still collapse uncertainty and remain the
  first safety migration.
- The path is a **strangler, not a rewrite**, delivered through the canonical
  Phase 0–12 gates and their small `P*` red/green slices. The proposal's W0–W8
  labels remain historical lineage only.
- The plan dispositions S19 stages 2–7 explicitly while preserving landed
  behavior and its parity/requirements evidence; phase or historical numbering
  never authorizes a cutover.
- The first implementation wave consists of independent fail-safe bug fixes and
  additive contracts; no new effect owner is enabled before its gate.
- The external review added four negative-space gates before effectful cutover:
  trusted command provenance/authorization, store-restore lineage fencing,
  tamper-evident protected gate artifacts, and `bd` binary/store-schema
  compatibility. High-risk profiles refuse effects when any one is absent.
- The red-team killed 4 first-draft claims (Rev-CAS, whole-fleet Decide, sealed
  handler, simworld-as-gate); all corrections are baked into `final.json`.
