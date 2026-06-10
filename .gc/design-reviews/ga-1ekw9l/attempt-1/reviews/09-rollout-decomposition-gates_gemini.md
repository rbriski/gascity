# Iris Kowalski — DeepSeek V4 Flash (Rollout & Decomposition Gates Reviewer, Independent Review)

**Verdict:** block

Lane: independently deployable slices, decomposition readiness, prerequisite honesty, exact gates, cross-repo sequencing, test coverage. Judged against `gc.mayor.implementation-plan.v1` (`/data/projects/gascity-packs-worktrees/gc-plan-pack/gascity/assets/skills/mayor/implementation-plan.schema.md`).

---

## 1. Executive Summary

As Iris Kowalski, the **Rollout & Decomposition Gates Reviewer**, I have performed an independent, evidence-backed, and first-principles review of the current design document (`plans/core-gastown-pack-migration/implementation-plan.md`, 835 lines, `updated_at: 2026-06-09T01:20:00Z`). 

The current plan represents a quantum leap in quality. It introduces a comprehensive **Decomposition Readiness Gate** (lines 689–722) including a detailed AC-to-proof-artifact matrix, and a structured **Slice-to-gate table** (lines 793–808) mapping dependencies, test commands, and one-way boundaries for all twelve rollout phases (Slices 1a to 7). These additions directly resolve previous concerns about unstructured rollout prose and unprovable milestones.

However, a close examination of cross-slice dependencies, rollback mechanisms, and test execution environments reveals **three critical bootstrapping deadlocks** and **one major rollback vulnerability** that must block full implementation approval.

---

## 2. Grounding & Live Tree Verification

* **Present and Matching:** 
  * A structured `Decomposition Readiness Gate` table mapping AC1–AC17 to proof artifacts (lines 704–723).
  * A `Slice-to-gate table` binding Slices 1a–7 to start/merge gates and boundaries (lines 793–808).
  * Clear encapsulation of system-pack loading APIs (`LoadRuntimeCity` and `LoadRuntimeCityNoRefresh`) under `internal/systempacks` (lines 171–214).
* **Missing/Inconsistent Live State:**
  * `plans/core-gastown-pack-migration/support/` is partially absent; the required validators, schemas, and matrices (e.g., `acceptance-proof-matrix.yaml`, `source-consumer-closure.yaml`) are designated as external prerequisites rather than active, checked-in codebase files.
  * Dependency sequencing conflicts exist between Slice 3 and Slice 4a, as well as Slice 2 and Slice 4b.

---

## 3. Critical Risks & Too-Quickly Accepted Assumptions

### 3.1. [Blocker] Bootstrap Isolation Dependency Paradox (Slice 3 vs. Slice 4a)
* **The Assumption:** Under Slice 3 (lines 747–750), the plan specifies:
  > `ensureBootstrapForDoctor` is either deleted in the same slice or rewritten to call `internal/systempacks` diagnostics without materializing bootstrap Core.
* **The Reality:** The new `internal/systempacks` package and its diagnostic surfaces are only introduced in **Slice 4a** (lines 752–755 & 802).
* **The Blocker:** In Slice 3, `internal/systempacks` does not yet exist. Rewriting `ensureBootstrapForDoctor` to import or call `internal/systempacks` in Slice 3 creates a compile-time failure. This is a severe cross-slice dependency violation.
* **Required Change:** Defer the rewriting of `ensureBootstrapForDoctor` to Slice 4a, or introduce a stubbed/empty `internal/systempacks` package in Slice 3 that only supports the minimal diagnostics needed by the doctor.

### 3.2. [Blocker] Circular Bootstrapping/Coexistence Proof Deadlock (Slice 1b vs. Slice 2)
* **The Assumption:** Under Slice 1b (lines 729–732 & 798), `gascity-packs` must produce a `packcompat` transcript proving coexistence with the "current" Gas City loader.
* **The Reality:** The `packcompat` test harness and the new config-adopted remote resolution rules do not exist in the "current" Gas City loader—they are only introduced in Gas City in Slice 2 and Slice 4a.
* **The Blocker:** `gascity-packs` cannot run or generate a `packcompat` transcript using Gas City tooling that hasn't been written yet, creating an unresolvable chicken-and-egg deadlock between the two repos.
* **Required Change:** Explicitly define a dual-development or staging branch workflow where the `packcompat` harness can run against a local check-out of Gas City's candidate loader before the immutable compatibility pin is merged.

### 3.3. [Blocker] Rollback Vulnerability to Old Binary's Maintenance Dependency (Compatibility Matrix Row 5)
* **The Assumption:** The Release Compatibility Matrix (lines 818–819) asserts:
  > `rollback from new to old | existing city -> Doctor-mutated manifests are either readable by old binaries or release notes name explicit downgrade limits...`
* **The Reality:** Old Gas City binaries hardcode a strict dependency on the `maintenance` system pack under `requiredBuiltinPackNames` (lines 37–41). The new binary's doctor mutation (Slice 4b/5b) removes Maintenance from required host packs and rewrites city configurations.
* **The Blocker:** If an operator rolls back a mutated city from the new binary to the old binary, the old binary will fail closed because it cannot find or load the required Maintenance system pack. Rolling back the binary *without* rolling back the city's files is a silent deployment failure mode.
* **Required Change:** The rollback contract must explicitly state that a downgrade to an old binary requires running a coordinator-driven restore/downgrade command that re-injects the Maintenance system pack and its imports back into the city files.

### 3.4. [Major] The `GC_BOOTSTRAP=skip` / Gate 1 Paradox
* **The Assumption:** Under §"Bootstrap Fixture Isolation" (lines 531–538), `GC_BOOTSTRAP=skip` must not skip `internal/systempacks` materialization and strict required Core file-set integrity.
* **The Reality:** Unit and testscript tests use `GC_BOOTSTRAP=skip` specifically to run in lightweight, isolated environments where the production Core fileset is intentionally *not* materialized on disk.
* **The Blocker:** If Gate 1 (pre-resolution fileset integrity) is strictly enforced under `GC_BOOTSTRAP=skip` without exception, then every lightweight isolated unit test in the suite will fail closed immediately because there is no production Core directory to validate.
* **Required Change:** Specify that in test mode under `GC_BOOTSTRAP=skip`, the fileset validator (Gate 1) runs against the empty/minimal mock bootstrap fixture (`bootstrap.EmptyFS`) or accepts a pre-computed test digest, rather than demanding a full production fileset.

### 3.5. [Major] Slice 5a Duplicate-Active Deadlock
* **The Assumption:** Slice 5a (lines 766–770) loads the public activation pin while local legacy Maintenance and Gastown directories are still present on disk.
* **The Reality:** The zero-duplicate-active gate (lines 764–774) triggers if same-named behavior IDs or packs are loaded from multiple sources.
* **The Blocker:** Loading the public activation pin (which contains the moved Gastown/Maintenance assets) while the legacy local folders still exist will trigger a collision, crashing the loader and blocking the verification step.
* **Required Change:** Specify that during Slice 5a, the `internal/packsource` classifier must explicitly ignore or suppress active discovery of local legacy folders when the loader is operating under an activation pin.

---

## 4. Evaluation against Lane Anti-patterns

| Anti-pattern / Risk | Mitigation in Plan | Status |
| :--- | :--- | :--- |
| **Fragile Batching:** Bundling pin changes, source deletion, doctor mutation, and activation into one landing. | **Resolved.** Slices 2 (pin), 3 (Core), 4a/b (systempacks/doctor), 5a/b (activation/Maintenance fold), and 7 (deletion) provide excellent separation. | **Pass** |
| **Prerequisite Dishonesty:** Claiming decomposition readiness while required templates/manifests are missing. | **Resolved.** The plan now explicitly blocks all downstream behavior-changing slices behind the physical validation and citation of AC6, AC7, and AC14–AC17 support artifacts. | **Pass** |
| **Unit-Only Proof:** Relying only on fast unit tests for complex integration boundaries. | **Resolved.** Testing (lines 681–685) mandates running the sharded process and integration targets (`make test-integration-shards-parallel`) for high-risk slices. | **Pass** |

---

## 5. Required Plan Updates

1. **Fix Slice 3 vs. 4a Dependency:** Move the rewriting of `ensureBootstrapForDoctor` to Slice 4a, or define a stub package in Slice 3.
2. **Resolve Circular Bootstrapping:** Specify a dual-development or staging checkout workflow to allow `packcompat` to run on draft pins before they are merged.
3. **Define Binary Downgrade Repair:** Require a dedicated doctor command or explicit recovery guide to re-inject Maintenance imports when rolling back from a mutated city to an old binary.
4. **Resolve `GC_BOOTSTRAP=skip` Paradox:** Clarify that Gate 1 validates against `bootstrap.EmptyFS` or a test digest when running unit tests under `GC_BOOTSTRAP=skip`.
5. **Avert Duplicate-Active Collisions:** Require `internal/packsource` to ignore legacy local directories during Slice 5a's public-pin validation.

---

## 6. Questions

1. **Slice 3 Import:** How can Slice 3 import `internal/systempacks` to rewrite `ensureBootstrapForDoctor` when that package is not created until Slice 4a?
2. **Old Binary Rollback:** If a city's files are mutated to remove Maintenance, how can an old binary (which requires Maintenance) successfully load that city without a manual or automated restoration step?
3. **Test Mode Gate 1:** What is the exact expected behavior of Gate 1 when a test runs with `GC_BOOTSTRAP=skip` and no production Core assets are present on disk?
