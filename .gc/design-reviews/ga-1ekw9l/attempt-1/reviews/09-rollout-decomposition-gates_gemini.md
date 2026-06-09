# Iris Kowalski - DeepSeek V4 Flash (Gemini Run)

**Verdict:** block

**Lane:** independently deployable slices, decomposition readiness, prerequisite honesty, exact gates, cross-repo sequencing and test coverage.

Reviewed against the Iteration 2 / Attempt 2 draft of `plans/core-gastown-pack-migration/implementation-plan.md` and `plans/core-gastown-pack-migration/requirements.md` in the active repository workspace.

---

## Executive Summary

As Iris Kowalski, conducting the **Rollout Decomposition Gates (DeepSeek V4 Flash)** review, my verdict is **Verdict: block**.

The Iteration 2 (Attempt 2) draft has made exceptional progress in structural compliance. It fully restructured the plan into the required Mayor schema sections, added the critical `## Data And State` segment, and purged the previous attempt-resolution ledgers and commentary. It also defines outstanding transactional safety mechanisms for the doctor (such as the `FixIntent` coordinator and CST-based TOML preservation).

However, from the perspective of rollout slicing, test-suite integrity, and decomposition readiness, the design contains several critical blocker gaps that make it impossible to safely cut implementation beads. The most severe issues are a total lack of explicit sequencing for the runtime-state migration, unresolved architectural contradictions regarding active controller triggers and locking, and an active test-suite breakage vector from narrowing `GC_BOOTSTRAP=skip` without providing lightweight test fixtures. We must resolve these concerns before unblocking decomposition.

---

## Top Strengths

- **Strict Mayor Schema Compliance (§1–11, §344–392, §453–529)**: The document now perfectly matches the `gc.mayor.implementation-plan.v1` layout. Restructuring `## Data And State` and renaming sections directly resolves the Iteration 1 schema blocker.
- **Purged Historical Commentary and Provenance Debt**: Moving the oversized, non-schema attempt resolution logs out of the implementation plan leaves a clean, highly readable, and focused engineering specification.
- **Phenomenal Transactional Doctor Safety (§246–264)**: Transitioning from legacy direct check mutations to a single `FixIntent` coordinator with advisory lock-before-preflight, TOCTOU closure, and CST-based comment/formatting preservation represents stellar first-principles systems engineering.
- **Rigorously Gated Transitional Loader Invariant (§180–186)**: Rejecting test-only loader bypasses for the no-Maintenance gate and requiring candidate branches to validate real production loading paths prevents silent regressions.

---

## Critical Risks & Gaps (The Blockers)

### 1. [Blocker] Complete Absence of Runtime-State Migration in Rollout Slices (§266–274 vs §453–503)
- **The Gap**: While Proposed Implementation (§266–274) and Data And State (§377–381) describe the runtime-state migration (JSONL archives, spawn-storm ledgers, refs/remotes, etc.) in detail, **the Rollout And Recovery timeline (§453–503) completely fails to sequence or execute it.**
- **The Impact**: Slices 1–7 do not mention where the runtime-state migration script/logic is introduced, nor when the active migration is triggered on existing cities. A decomposer cannot cut beads from this plan because it is undefined whether migration is a Slice 4 (coordinator introduction) or Slice 5 (activation pin & Maintenance removal) responsibility, or if it requires a dedicated intermediate slice. 

### 2. [Blocker] Unresolved Concurrency and Trigger Contradictions in Migration Domain (§256–257 vs §268–269)
- **The Gap**: The plan mandates that the mutation coordinator "refuses automatic fix with manual guidance" if "a controller for the same city is running" (§256–257). However, runtime-state migration is a startup-time concern. When a new-binary controller boots, it must migrate legacy old-path state before running.
- **The Conflict**: If the controller triggers the coordinator at startup, then the controller is active, which violates the coordinator's own rule to refuse automatic fixes.
- **The Decomposition Failure**: The locking and trigger domains are self-contradictory. The design must explicitly define a safe bypass for the controller's own startup-migration path under the shared advisory lock, or explicitly mandate that migration is strictly offline and doctor-only, with the controller refusing to run on legacy un-migrated paths.

### 3. [Blocker] Narowed `GC_BOOTSTRAP=skip` Test-Suite Breakage (§318–322)
- **The Gap**: The plan narrows `GC_BOOTSTRAP=skip` so that it may only skip empty bootstrap fixture materialization, explicitly prohibiting it from skipping `internal/systempacks` materialization, Core file-set integrity, retired-source classification, or typed participation validation (§318–322).
- **The Conflict**: Fast unit tests in `main_test.go` and testscripts run in isolated, empty directories or fake environments lacking Core packs or global caches.
- **The Risk**: Forcing every fast unit test to execute production validation and materialization gates will cause massive failures across hundreds of previously green tests, destroying the fast unit test baseline. The plan must specify lightweight, mapFS-backed, or pre-populated in-memory fixtures specifically for test isolation under narrowed skip semantics.

### 4. [Major] Slogan-Level "Push-Cursor Reconciliation" (§267, §378)
- **The Gap**: The plan names "push-cursor reconciliation" as a recorded field but never defines the actual rule or algorithm to make duplicate offsite pushes impossible when upgraded and downgraded binaries alternate. Suppressing diagnostics is not a sufficient safeguard against corrupting shared remote git refs.

### 5. [Major] Spawn-Storm Ledger Split-Brain Throttling Bypass (§380–381)
- **The Gap**: If the new binary ignores the legacy ledger count during version skew, neither old nor new binaries see the combined spawn-storm count. A genuine spawn storm driven by both binaries will stay under their respective individual thresholds, bypassing the throttling safety mechanism. The design must specify that the new binary's threshold evaluator read-unions the retained legacy ledger counts during the skew window.

### 6. [Major] Overpromised "Deterministic Re-Upgrade Flow" (§271–273, §379–380)
- **The Gap**: If both legacy append-style stores and the new Core-owned stores have post-marker writes, their histories have diverged. Merging them automatically is mathematically impossible without data loss. The design must restrict "deterministic re-upgrade" strictly to cases where the new Core-owned paths are untouched (pure rollback-then-re-upgrade), and enforce manual operator intervention if both sides have diverged.

---

## Missing Evidence

- **Checked-in Asset Migration Ledger (AC6) and Behavior-Preservation Manifest (AC7)**: The plan states these are stored under `plans/core-gastown-pack-migration/artifacts/` during planning. However, no such directory or files exist in the active repository workspace.
- **Proof of Fast Unit Test Preservation**: There is no proof-of-concept or schema showing how offline/fake-backend tests will remain green and fast without full Core materialization under the narrowed `GC_BOOTSTRAP` semantics.
- **Source Grounding of Runtime-State Migration**: The plan fails to ground the migration in concrete script names (e.g., `jsonl-export.sh`, `spawn-storm-detect.sh`), Go files, specific JSON fields (`pending_archive_push`), or paths (`.gc/jsonl-archive`).

---

## Required Changes

1. **Explicitly Sequence Runtime-State Migration**: Clearly sequence the runtime-state migration within the rollout slices (e.g., as part of Slice 4, Slice 5, or an explicit Slice 4b), and define its exact gating and rollback invariants.
2. **Reconcile Trigger and Locking Domains**: Define how the controller's startup migration bypasses the coordinator's "running controller" check under the shared advisory lock, or declare the migration strictly offline.
3. **Provide Lightweight Test Fixtures for Systempacks**: Specify lightweight, mapFS-backed or pre-populated memory mocks for `internal/systempacks` specifically for unit test suites to preserve test isolation.
4. **Define Push Reconciliation and Read-Union Throttling Rules**: Explicitly define the duplicate push prevention rule and the read-union algorithm for the spawn-storm ledger.
5. **Limit Re-Upgrade and Clarify Copy-vs-Move**: Define migration as a non-destructive copy-then-mark protocol, and restrict automatic re-upgrade to untouched new paths, requiring manual operator reconciliation for diverged states.
6. **Ground Implementation Details**: Restore concrete filenames, JSON fields, and paths to the Proposed Implementation section.

---

## Lane-Specific Questions

### Q1: Can tasks be cut so each slice names concrete files, acceptance gates, cross-repo prerequisites, and a revert or one-way upgrade boundary before merge?
**Answer**: No, not yet. Because the runtime-state migration is completely unsequenced in the rollout timeline (§453–503), and because the transition rules for `ensureBootstrapForDoctor` and `dolt-target.sh` are undefined or generalized out of the slices, a decomposer cannot cut safe beads from this plan. Slices 4 and 5 must be expanded to explicitly place these tasks and define their rollback boundaries.

### Q2: Are open questions truly resolved, or are ownership audits, generated artifacts, and gascity-packs branch availability deferred as hidden blockers?
**Answer**: They are deferred as hidden blockers. The requirements are in `status: questions`, and the required AC6 asset ledger and AC7 behavior manifest are absent in the planning directory. Saying "Open Questions: None" while these planning-phase artifacts do not exist violates prerequisite honesty. The plan must remain in `status: blocked` / `blocked:prerequisite` until the planning artifacts are checked-in or generated.

### Q3: Does each intermediate commit pass the documented local gates and exercise production loaders rather than copied fixtures or direct `config.Load` shortcuts?
**Answer**: As written, they would fail. Narrowing `GC_BOOTSTRAP=skip` without specifying lightweight mocks or mapFS fixtures for the required loader will break the fast unit baseline (`make test-fast-parallel`) across intermediate commits, violating the rollout's own test-green invariant.
