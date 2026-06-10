# Oleg Marchetti — DeepSeek V4 Flash (Behavior Evidence Chain Reviewer, Attempt 12, Independent Review)

**Verdict:** block

**Lane:** Gastown behavior inventory completeness, execution-level witnesses, cross-repo packcompat, old-to-new traceability, source deletion gate.

Reviewed the updated `plans/core-gastown-pack-migration/implementation-plan.md` (snapshot `attempt-12/design-before.md`) against `requirements.md` and live repository behavior, focusing on the `gc import` launch sweep.

---

## Executive Summary

As Oleg Marchetti, the **Behavior Evidence Chain Reviewer** (role: **cross-repo-evidence-chain-auditor**), my non-negotiable mandate is to guarantee that no behavioral triggers, prompt-fragments, detectors, mail routes, nudges, or scripts are lost, mutated, or silently degraded during architectural transitions. The move to consolidate our import system and launch the PackV2 `gc import` model represents a vital cleanup of Gas City's dependency story, but the design currently contains **severe architectural gaps, concurrency risks, and "drift escapes"** that other reviewers may accept too quickly.

Allowing a cached pack to bypass integrity audits simply because a lockfile exists, permitting concurrent writes to shared directories during background controller execution, deleting package-registry and implicit-import code without a formal source deletion gate, and ignoring the circular deadlock during offline/local testing are major hazards. 

Until these programmatic gaps are explicitly addressed and hardened in the design text, my verdict is a firm **block**. The requirements below outline the exact, non-negotiable modifications needed to achieve safe, auditable execution-level behavior preservation for the PackV2 import launch.

---

## Top Strengths & Design Evolution

1. **Explicit Remediation Gating (Task 1)**: Deleting legacy auto-fetch behavior from normal loading paths (`gc start`, `gc config`, and supervisor paths) and replacing it with explicit, actionable remediation text pointing to `gc import install` is a fantastic design improvement. This respects the fail-closed principle and prevents silent, untracked network operations.
2. **Consolidated Bootstrap and Repair (Task 2)**: Unifying first-time bootstrap and missing cache repair into a single command `gc import install` is a major improvement over fragmented commands and provides a clear operator narrative.
3. **PackV2-Native Defaults (Task 4)**: Consolidating default rig composition into `[defaults.rig.imports.<binding>]` in `pack.toml` replaces legacy, ad-hoc city-level defaults with a unified, packcompat-aware syntax.
4. **Preservation of Legacy Tests (Task 1 AC)**: Ensuring that legacy `gc pack` compatibility does not silently reintroduce network fetching on schema-2 code paths protects older installations from regression.

---

## Critical Risks & Core Blockers

### 1. [Blocker] Content Hash Drift Escape (Task 2)
*   **The Assumption**: The plan specifies that an existing `packs.lock` simply restores state, and a missing lockfile resolves and installs cache state (lines 93–96).
*   **The Reality**: If an operator or a buggy process corrupts, edits, or deletes files inside the local cache directories under `.gc/system/packs` or the downloaded imports, but `packs.lock` remains present on disk, a subsequent run of `gc import install` will see the lockfile and blindly assume the cache is valid, silently skipping repair.
*   **The Blocker**: This allows the behavioral evidence chain to drift. Gas City will run with stale, corrupt, or modified behavior files with zero verification, completely subverting type-safety and reproducibility.
*   **Required Change**: `gc import install` must programmatically verify cache integrity. If `packs.lock` is present, the installer must compute the SHA256 content hashes of the cached directories/files on disk and compare them against the lockfile digests. Any mismatch or missing file must trigger auto-repair/re-materialization of the affected pack.

### 2. [Blocker] Concurrency Race on Shared Pack/Import Directories (Task 2)
*   **The Assumption**: `gc import install` modifies cache and install state under `.gc/system/packs` and download paths (lines 87–104).
*   **The Reality**: A city's controller is a long-running background process, and multiple CLI commands can run concurrently. If `gc import install` is executed concurrently while the controller or another CLI invocation is reading the configuration or resolving imports, it will trigger severe filesystem concurrency races.
*   **The Blocker**: Downstream processes will read partially-materialized or half-written config files, leading to random configuration loading crashes, process stalls, or corrupted behavioral state.
*   **Required Change**: `gc import install` must strictly acquire a city-level advisory lock (`.gc/system/packs.lock` or similar) before writing or pruning anything in the cache or install directory. Furthermore, the installer must use atomic directory swaps (staging to a unique temporary directory, then performing an `os.Rename`) to guarantee that concurrent read-only paths never observe partial state.

### 3. [Blocker] Deletion Gate Absence for Registry & Implicit Imports (Task 3)
*   **The Assumption**: Task 3 specifies the removal of package-registry and implicit-import launch artifacts from code, tests, and docs (lines 114–133).
*   **The Reality**: Removing these legacy layers without a formal behavior inventory or deletion gate risks silently dropping essential behaviors, triggers, mail routes, or nudge configurations that were previously registered or resolved implicitly.
*   **The Blocker**: Developers can delete this code and greenlight the PR while quietly regressing user rigs that relied on implicit-import defaults.
*   **Required Change**: Mandate a **Source Deletion Gate check** in CI for Task 3. A programmatic audit must verify that every behavior (e.g., triggers, nudges, prompts, mail routes) previously supplied by implicit-import or package-registry defaults is fully accounted for and verified to be present either in a system-pack wrapper (`.gc/system/packs`) or inside the explicit PackV2 default imports.

### 4. [Blocker] Cross-Repo Circular Deadlock and Missing Local Overrides
*   **The Assumption**: Loader paths and default-rig configurations (`[defaults.rig.imports.<binding>]`) resolve and pin imports from remote repositories (lines 38, 136–147).
*   **The Reality**: To test and verify a new behavior or bug fix across the `gascity` and `gascity-packs` repositories, a developer must commit and push changes to `gascity-packs`, wait for a public commit SHA, and update the Gas City pin. This creates a circular deadlock during development and blocks offline testing.
*   **The Blocker**: The lack of a local development override slows the feedback loop down to a crawl, leading to hand-curated "shortcuts" or copied assets that skip verification.
*   **Required Change**: The design must specify a first-class local override mechanism. Both `gc import install` and the config loader must support local overrides (e.g., via a `local_path` field in `pack.toml` imports or a `--local-override` environment variable/flag) that allows developers to point an import to a local checkout of the pack repository, bypassing remote fetch entirely for local development and offline testing.

### 5. [Major] Rig Import Immutability vs. Silent Default Drift (Task 4)
*   **The Assumption**: `gc rig add` inherits defaults from `[defaults.rig.imports.<binding>]` when `--include` is omitted (lines 143–144).
*   **The Reality**: If rigs resolve these default imports dynamically at runtime from the root `pack.toml`, any subsequent change or version update in the root `pack.toml` defaults will silently alter the rig's behavior.
*   **The Blocker**: This violates the principle of behavioral traceability and immutability. Rig behavior must be explicitly pinned and predictable; silent drift of inherited imports is unacceptable.
*   **Required Change**: Specify that when `gc rig add` is run, the inherited defaults are frozen and written explicitly into the newly created rig's config file (`rigs/<name>/pack.toml` or similar), or if resolved dynamically, the system must enforce a strict, unoverrideable version lock to prevent silent, untracked changes when the root defaults are modified.

### 6. [Major] Insufficient Integration Test Coverage for Legacy Compatibility
*   **The Assumption**: Legacy `gc pack` compatibility is preserved on schema-2 code paths (lines 77–78).
*   **The Reality**: The plan mentions targeted unit tests but is silent on how the extensive suite of pre-existing integration and acceptance tests (under `test/acceptance/` and `test/integration/`) are updated to prove that legacy behaviors actually execute correctly when loaded through the consolidated PackV2 loader.
*   **The Blocker**: We run the risk of greenlighting config validation tests while legacy behaviors quietly break during real process execution.
*   **Required Change**: Add an explicit acceptance criterion to Task 1: execute the complete suite of integration and acceptance tests under both *compatibility-pin mode* and *activation-pin mode* to prove zero behavioral regressions across the entire transition.

---

## Missing Evidence & Requirements Gaps

1. **Lockfile Schema and Digest Specification**: The plan does not specify how `packs.lock` represents the exact digests of explicit imports, nor how it tracks transitive dependencies.
2. **Explicit Diagnostic Error Messages**: There is no specification of the actionable diagnostic messages shown to operators when an import is missing, including whether it prints the exact command (`gc import install`) and context.
3. **Lock Acquisition Timeout Rules**: Specify a timeout and auto-release mechanism for the city-level advisory lock used during `gc import install` to prevent infinite hangs if a process crashes mid-operation.

---

## Answers to Oleg Marchetti's Persona Questions

1. **Does the generated inventory enumerate every requester, detector, notification, prompt, order, mail, nudge, and script trigger removed from Core?**
   * *Answer*: **No.** The design removes implicit-import and package-registry defaults in Task 3 without providing an inventory or completeness scan of the behaviors being cleaned up, leaving a critical traceability gap.
2. **For each moved behavior, what old Gas City witness and final gascity-packs witness prove the same trigger fires through normal resolution from the exact public pin?**
   * *Answer*: **Unaddressed.** The plan is silent on how the transition of default behaviors from legacy registries to system-packs or explicit imports is verified at the execution level using unit or integration tests.
3. **How does CI prevent deleting or generalizing a behavior before the gascity-packs landing commit and trace row are present?**
   * *Answer*: **Unaddressed.** There is no source deletion gate specified in Phase 2, meaning registry-import behavior could be deleted before its replacement has been verified as functional and pinned in the public pack repository.
