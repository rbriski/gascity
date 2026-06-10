# Natasha Volkov — DeepSeek V4 Flash (Independent Review, Attempt 19)

**Verdict:** block

**Scope:** REQUIREMENTS scenario parity, regression prevention, characterization tests, proof freshness — with evidence drawn from verification of `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` and the active workspace.

---

## Overview

Attempt 19 represents a highly mature and structured iteration that incorporates significant architectural improvements over prior attempts. By establishing the Slice 0 contract and its associated inventories (including `SESSION_BOUNDARY_SYMBOLS.yaml`, `WORKER_BOUNDARY_EXCEPTIONS.yaml`, and `SESSION_EVENT_INVENTORY.yaml`) as non-normative, schema-only preflights (DESIGN.md:230-237), the design successfully avoids premature enforcement while ensuring strict validation at the point of adoption. The formalized **Refactor Rules** (DESIGN.md:978-993) are exceptional and represent a massive step toward guaranteeing behavior-preserving extractions.

However, from the perspective of the Behavior Parity Guardian, several critical **traceability loops**, **verification contradictions**, and **brittle assertion paths** remain unresolved. Most notably, the **Stale Evidence Paradox** continues to trap the strictly non-mutating Slice 0 in a circular requirement to repair complex Layer 2-4 reconciler and provider-health integration tests. Additionally, the design still lacks design-time traceability of scenario-to-slice allocations, and the lack of a strict black-box mandate for characterization tests leaves the refactor highly vulnerable to silent behavioral drift.

---

## Top Strengths

- **Enforceable Refactor Rules (DESIGN.md:978-993):** The addition of step-by-step refactoring guidelines (requiring characterization tests, explicit requirements mapping, incremental caller adoption, and proof of parity before code deletion) is outstanding. It directly addresses the core concerns of regression prevention.
- **Non-Normative Schema preflights (DESIGN.md:230-237):** Shifting the extensive inventories to schema-only, non-normative artifacts in Slice 0 prevents the preflight from blocking early progress, while guaranteeing that full enforcement binds automatically as soon as a slice consumes the gated behavior.
- **Closed-World Parity Ledger (DESIGN.md:208-212):** Requiring `SCENARIO_PARITY.yaml` to catalog every active `SESSION-*` requirement with its touched surfaces, exact proof commands, current oracle, and owner-approved amendment state provides a highly rigorous framework for verifying behavior.
- **Explicit Requirement Preservation (DESIGN.md:38-53):** Establishing `REQUIREMENTS.md` as the absolute behavior source of truth, and forbidding developers or agents from labeling behavioral drift as requirements updates without owner sign-off, is a world-class governance standard.

---

## Blocking Findings

### 1. [Blocker] The Stale Evidence Paradox remains active in Slice 0
- **Evidence:** `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` lines 245-251.
- **Why it matters:** The design still mandates that the non-mutating, session-only Slice 0 "must repair or owner-retire the evidence for `SESSION-RECON-002`, `SESSION-RECON-003`, `SESSION-RECON-006`, and `SESSION-RECON-007` before a later slice cites those rows." 

This requirement contains a fatal architectural contradiction:
1. Slice 0 is strictly **non-mutating and session-only**; it does not touch reconciler policy or write to any non-session store.
2. The stale/missing evidence paths cited in `REQUIREMENTS.md` (such as `scale_from_zero_test.go`, `provider_health_gate_test.go`, and `session_progress_test.go`) belong to the reconciler and provider-health sub-systems (Layer 2-4), which are outside `internal/session`.
3. Repairing these tests requires restoring or writing complex reconciler scaling, health, and progress integration tests that require a functional store and runtime provider.
4. "Owner-retiring" these rows is unacceptable because they represent safety-critical production behaviors (such as cold-start clamping and health alerts) that must not be deleted or hidden just because their test evidence is currently missing on this branch.

Because Slice 0 cannot mutatively restore Layer 2-4 reconciler integration tests without violating its own "session-only, non-mutating" boundary, and cannot retire them without deleting product requirements, the Slice 0 validator is guaranteed to fail immediately and permanently, blocking all progress.

- **Required change:** 
  - Explicitly allocate the repair/restoration of `scale_from_zero_test.go`, `provider_health_gate_test.go`, and `session_progress_test.go` to a preceding or parallel **Reconciler Test-Hardening Slice (Slice 6 Backlog)** rather than the non-mutating Slice 0.
  - Provide a machine-readable **Transition Proof Allowlist** in Slice 0's validator allowing these specific reconciler rows to temporarily fall back to their historical commit citations in `REQUIREMENTS.md` until the corresponding test-hardening slice restores the files.
  - Annotate these four affected rows directly in `REQUIREMENTS.md` as `[STALE - REQUIRES SLICE 6 REPAIR]` so they are not mistaken for live executable evidence.

---

### 2. [Blocker] Deferred design-time traceability prevents backlog verification
- **Evidence:** `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` lines 994-1021.
- **Why it matters:** The backlog slices (Slices 1 to 6) still lack scenario-row mapping. By deferring the row-to-slice allocation to a future `SCENARIO_PARITY.yaml` file to be created during Slice 0 implementation, we cannot verify at this design gate that the refactor backlog is comprehensive. 

There is no structural proof in `DESIGN.md` that critical scenario rows (such as `SESSION-LIFE-001` legacy state projection, `SESSION-LIFE-002` pending-create claim, `SESSION-LIFE-008` user-facing projection guard, or `SESSION-RUNTIME-004` stop turn) are actually covered by any future slice.

- **Required change:** Re-introduce a high-level **Scenario Allocation Matrix** directly in `DESIGN.md` that maps groups of `SESSION-*` requirements to their target backlog slices (e.g., Slices 1 & 2 own Target Identity and Surfaces; Slice 3 owns Wake; Slice 4 owns Close; Slice 5 owns Runtime Start; Slice 6 owns Reconciler Facts). This ensures completeness is proven at the design gate before implementation begins.

---

### 3. [Blocker] Lack of Black-Box Assertion Rules for Characterization Tests
- **Evidence:** `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` lines 978-993.
- **Why it matters:** Refactor Rule 3 states that "The test should prove the behavior the user sees, not every internal branch." However, this is too weak to prevent regression. 

Without a strict prohibition against white-box mock assertions (such as mocking internal store interfaces or asserting internal function call chains), developers or agents will write brittle mocks that pass during the refactor even if the user-visible product behavior (exit codes, output payloads, or database commit states) is completely broken.

- **Required change:** Add an explicit, non-negotiable rule to the `Refactor Rules`:
  > "Characterization tests must be black-box, end-to-end, or integration-level tests asserting user-visible or system-level outputs (such as exit codes, stdout/stderr shape, API status codes, and database commit states) rather than white-box mocks of internal interfaces. The exact same characterization tests must run unchanged against both the legacy baseline and the refactored path to prove parity."

---

## Major Findings

### [Major] Lack of assertion-level verification in Proof-Freshness validation
- **Evidence:** `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` lines 249-251 and lines 259-266.
- **Why it matters:** The Slice 0 validator `TestScenarioParityFreshness` checks if cited file paths exist. However, a file-existence check cannot detect if the tests inside that file have been renamed, gutted, or bypassed using `t.Skip()`. The fact that reconciler requirements were allowed to go completely missing while their citations remained in `REQUIREMENTS.md` proves that path-level checks are insufficient to prevent proof rot.

- **Required change:** Require that `SCENARIO_PARITY.yaml` specifies both the file path and the **exact test function symbol(s)** (e.g., `TestSessionLifecycle/Wake_Held_Until`). The Slice 0 freshness validator must dynamically parse or execute the tests to verify that the named test functions exist and do not contain hardcoded skips.

---

## Minor Findings & Questions

- **Dynamic Key Static Guard:** How will the static guard specified in Slice 0 handle dynamic metadata key writes (e.g., loops writing variable key patterns)? Will it enforce that any `SetMetadata` with a non-literal key is a violation? We recommend that dynamic-key patterns must be explicitly registered as exceptions in `SESSION_BOUNDARY_SYMBOLS.yaml`.

---

## Summary of Required Changes

1. **Resolve the Stale Evidence Paradox:** Move reconciler integration test restoration to Slice 6 Backlog, add a **Transition Proof Allowlist** to Slice 0's validator allowing missing paths to fall back to historical commit hashes, and annotate the affected rows in `REQUIREMENTS.md`.
2. **Re-introduce the Scenario Allocation Matrix:** Add a high-level table to `DESIGN.md` mapping all `SESSION-*` requirements to their target backlog slices to ensure design-time coverage verification.
3. **Mandate Black-Box Characterization Tests:** Add a strict rule to the Refactor Rules forbidding white-box mock assertions.
4. **Verify Assertion-Level Freshness:** Update the Slice 0 freshness validator to check exact test function symbols and skip-states, not just file existence.

---

## Answers to Persona Questions

### 1. Which REQUIREMENTS.md scenario rows does each extraction slice touch, and where is parity proven before the slice lands?
**Answer:** The design currently delegates this mapping entirely to a future `SCENARIO_PARITY.yaml` file to be created in Slice 0 (`DESIGN.md:208-212`). This is a critical traceability gap; a high-level Scenario Allocation Table must be included in `DESIGN.md` itself before the design is approved.

### 2. Are characterization tests captured before moving logic out of reconciler or manager code?
**Answer:** Yes, Refactor Rule 3 (`DESIGN.md:984`) requires keeping or adding characterization tests before moving logic. However, this is currently a recommendation; it must be upgraded to a hard rule enforcing black-box assertions rather than fragile white-box mocks.

### 3. How does the plan prevent cited proof paths from going stale as implementation files change?
**Answer:** The plan uses `SCENARIO_PARITY.yaml` and a freshness validator in Slice 0 (`DESIGN.md:259-266`). However, this validator only checks file existence; it must be upgraded to verify exact test function symbols to prevent tests from being silently skipped or gutted.
