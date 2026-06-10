# Iris Kowalski — Rollout and Decomposition Gates Reviewer (Iteration 12 / Attempt 12, Independent DeepSeek V4 Flash Style)

**Verdict:** block

> **Lane:** Independently deployable slices, decomposition readiness, prerequisite honesty, exact gates, cross-repo sequencing, and test coverage.
> 
> Reviewed against the Iteration 12 / Attempt 12 implementation plan (`plans/core-gastown-pack-migration/implementation-plan.md`, 310 lines, `updated_at: 2026-06-10T08:17:00Z`) — specifically §"GC Import Launch Implementation Plan" and Slices/Tasks 1 through 8.
> 
> This independent review is produced using the DeepSeek V4 Flash style, focusing rigorously on first-principles trust boundaries, cross-document state consistency, and unstated runtime assumptions.

---

## Schema Conformance

**Conforms with reservations.** The Iteration 12 / Attempt 12 implementation plan structurally aligns with the required top-level section ordering mandated by `implementation-plan.schema.md`, includes correct front matter, and lists `Open Questions` as `None`. However, the drastic rescoping of the plan has resulted in a complete regression of the rollout architecture. Specifically, the comprehensive **Decomposition Readiness Gate** and **Slice-to-gate table** from the previous iterations have been entirely stripped. Replacing rigorous, physical verification matrices with abstract prose-based phases violates the core engineering intent of the schema.

---

## Top Strengths of the Design

- **Clear Phase Sequencing of Cleanups (Task 3, lines 114–133):** Postponing the package-registry and implicit-import cleanup to Phase 2 ensures that the foundation contract (Phase 1) is stable before removing legacy artifacts, minimizing the risk of partial loader failures.
- **Wizard/Init PackV2 Transition (Task 5, lines 154–172):** Moving `gc init` Gastown generation to write PackV2-native imports directly in `pack.toml` rather than relying on legacy city fields is a robust end-state design choice that prevents configuration drift.
- **Explicit Authoritative Doc Convergence (Task 8, lines 220–240):** Embedding documentation updates as an explicit, gated task in Phase 5—rather than treating it as post-implementation debt—is excellent practice for operator-facing rollouts.

---

## Critical Risks & Consensus Blockers

### 1. Structural Disconnect Between Slice Graph and Task List (Lane Mandate / Q1)

The plan introduces a conceptual "Slice Graph" (lines 43–63) outlining Slices A through E and their topological dependencies. However, the subsequent **Task List** (lines 63–247) completely discards this taxonomy, organizing the work instead into five **Phases** (Phases 1–5) containing Tasks 1 to 8.
- **The Risk:** There is no mapping showing which tasks belong to which slices. For instance, it is impossible to determine whether Task 2 belongs to Slice A or Slice E, or how Phase 3 maps to Slice C. This disconnect makes independent slice-level deployment and testing unprovable.
- **Required Resolution:** Re-align the Task List so that every Task is explicitly bound to a named Slice from the graph, or replace the abstract Slice Graph with a concrete, linear Phase-gate dependency table.

### 2. Elimination of the Decomposition Readiness Gate and Rollback Boundaries (Lane Mandate / RF1 / Q1)

In the Iteration 12 rescoped plan, the entire **Decomposition Readiness Gate** and per-slice **Rollback/One-way boundary** machinery have been completely removed.
- **The Risk:** Slices/Phases are planned to be merged sequentially (via Checkpoints A, B, and C) without defining any physical start gates, rollback commands, or one-way upgrade boundaries. This re-introduces **Red Flag #1 (Fragile Batching)**: if Phase 3 fails in production, there is no documented or verified recovery path to restore the system to Phase 2 state.
- **Required Resolution:** Restore a structured **Slice-to-gate table** mapping every deployable unit to:
  1. A physical start gate (e.g., checked-in prerequisite commits or proof artifacts).
  2. A physical merge gate.
  3. A concrete rollback mechanism or one-way boundary predicate.

### 3. "Unit-Only Proof" for High-Risk Loader and Doctor Changes (RF3)

The checkpoints defined in the plan rely exclusively on fast, targeted unit tests to authorize merging and moving to subsequent tasks:
- **The Risk:** Checkpoint A (lines 105–111) and Checkpoint B (lines 188–195) list only targeted unit tests for `cmd/gc`, `internal/config`, `internal/packman`, `internal/migrate`, and `internal/doctor` as merge proof. These packages govern critical, high-risk loader and database mutation behaviors. Running only unit tests violates **Red Flag #3** by ignoring sharded process and integration coverage until Checkpoint C, allowing complex race conditions, lock contention, and version-skew failures to merge undetected into the mainline.
- **Required Resolution:** Mandate that intermediate Checkpoints A and B must execute the full sharded process and integration targets (`make test-cmd-gc-process-parallel` and `make test-integration-shards-parallel`) before any behavior-changing PR is merged.

### 4. Hidden Circularity Blocker for Offline CI and Gastown Init (Q2)

Task 5 (lines 154–172) changes Gastown init to write PackV2 imports pointing directly to `https://github.com/gastownhall/gascity-packs.git//gastown` with an immutable `sha:` pin.
- **The Risk:** To test this end-to-end (as required by Checkpoint B), the candidate binary must resolve this remote import. However, if the repository branch or commit does not yet exist on GitHub, or if the CI runner operates in an offline/sandboxed environment, the test will fail closed or stall. The plan fails to specify how local, offline test overrides (e.g., using a local checkout of `gascity-packs` via git redirects or mirror-seeding) are configured during the staging window.
- **Required Resolution:** Define a clear offline staging/testing mechanism (e.g., utilizing local Git directory mapping or a mock cache registry) that allows `init_lifecycle_test.go` to run without making live network requests to GitHub.

### 5. Silent Regression of the Bypass Scanner (Q3)

The previous implementation plan featured a strict "deny-by-default bypass scanner" to prevent code changes from bypassing the production loader via direct `config.Load` shortcuts. In Iteration 12, this scanner has been quietly removed.
- **The Risk:** Developers can now land direct configuration-load bypasses in `cmd/gc/` or supervisor packages undetected, eroding the SDK loading invariants and leading to silent, behavior-changing bugs.
- **Required Resolution:** Reintroduce a static token scanner or pre-commit hook that checks for unauthorized direct `config.Load` or `session.NewManager` calls outside the canonical `worker.Handle` boundary.

---

## Missing Evidence

1. **No Slice-to-Gate Mapping Table:** The plan does not map Slices (A–E) to Tasks (1–8) or define start/merge gates.
2. **No Intermediate Rollback Procedures:** No rollback commands are specified for intermediate checkpoint failures.
3. **No Offline Test Configuration:** The plan provides no instructions on how to mock or redirect GitHub PackV2 imports for offline or CI test execution.
4. **No Static Bypass Scanner:** The plan lacks any mechanical enforcer to prevent direct configuration loading bypasses.

---

## Required Changes

1. **Unify Slices and Tasks:** Explicitly map Tasks 1–8 to the Slices in the Slice Graph, or restructure the graph to match the Phase timeline.
2. **Restore the Gate Table:** Add a structured table defining the start gate, merge gate, and rollback mechanism for each deployable unit.
3. **Escalate Checkpoint Testing:** Add `make test-cmd-gc-process-parallel` and `make test-integration-shards-parallel` as required merge proof for Checkpoints A and B.
4. **Specify Offline Test Overrides:** Detail how the `gc init` tests bypass live network fetching of `gascity-packs` during development and CI runs.
5. **Re-implement Bypass Scanning:** Restore the bypass scanner to enforce loading invariants across all touched Go files.

---

## Responses to Lane-Specific Questions

### Q1: Can tasks be cut so each slice names concrete files, acceptance gates, cross-repo prerequisites, and a revert or one-way upgrade boundary before merge?

**Answer:** 
No. The Iteration 12 plan organizes tasks into broad Phases rather than independently deployable slices. The checkpoints lack physical start/merge gates, concrete file lists, or rollback boundaries, leaving the system highly vulnerable to fragile batching and unrecoverable intermediate deployment states.

---

### Q2: Are open questions truly resolved, or are ownership audits, generated artifacts, and gascity-packs branch availability deferred as hidden blockers?

**Answer:** 
No. The plan lists `Open Questions: None` but defers critical blockers, including the availability of the remote `gascity-packs` candidate commits and the mechanics of running end-to-end init tests in air-gapped CI environments.

---

### Q3: Does each intermediate commit pass the documented local gates and exercise production loaders rather than copied fixtures or direct config.Load shortcuts?

**Answer:** 
No. Because the static bypass scanner has been removed, there is no mechanical enforcement to prevent the introduction of direct `config.Load` shortcuts. Furthermore, intermediate checkpoints rely exclusively on targeted unit tests that do not exercise the full production loader or integration environment.
