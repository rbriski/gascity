# Ritu Raman — Bootstrap Fixture Isolation Perspective Independent Review (Iteration 18 / Attempt 1)

**Verdict:** approve-with-risks

**Lane:** Bootstrap embed cleanup, deterministic test fixtures, test-only no-Core path containment, hidden-dependency discovery.

This independent review evaluates the Iteration 18 / Attempt 1 draft of the Core and Gastown Pack Split design (`.gc/design-review-inputs/core-gastown-pack-migration/design.md`) against the `requirements.md` and the live codebase at the `rig_root` (`/data/projects/gascity`).

---

## Executive Summary

As Ritu Raman, the **Bootstrap Fixture Isolation Reviewer**, I am issuing a **Verdict: Approve-With-Risks** for the Iteration 18 / Attempt 1 draft of the Core and Gastown Pack Split design. 

The latest Attempt 17 additions—specifically the transition to a **source-symbol guarded, symbol-aware scanner** and the explicit definition of `bootstrapAssets` as a private, non-nil empty filesystem returning `fs.ErrNotExist`—represent monumental steps forward. They successfully resolve previous blockers around path-string-only guards and nil-FS panic vectors. 

However, several critical "mechanism-coupled" dependencies and test-path hardcodings remain unaddressed in the explicit inventory and slice dispositions. If handed to developers as-is, they will cause immediate test breakages, compiler errors, or stale runtime state. Specifically, `cmd/gc/prompt_test.go` reads Core prompt assets directly from disk using the old path, and several Go functions still reference retired system packs. These must be resolved and explicitly inventoried before the design can be considered fully complete.

---

## Top Strengths of Current Design

* **Symbol-Aware AST-Based Scanner (§2609–2614):** Upgrading from fragile path-string matches to a source-symbol scanner that tracks references to `bootstrap.BootstrapPacks`, `bootstrap.PackNames`, `bootstrapSkillDirs()`, and `GC_BOOTSTRAP` provides a robust, future-proof discovery mechanism that prevents silent coupling regressions.
* **Deterministic, Non-Nil Private Empty Filesystem (§2616–2619):** Specifying that production `bootstrapAssets` defaults to a private empty filesystem returning `fs.ErrNotExist` eliminates the risk of latent `nil` pointer dereference panics while providing a clear diagnostic when no fixture is injected.
* **Hermetic Inline Fixtures (§2619–2621):** Shifting test fixtures to inline `fstest.MapFS` with synthetic paths (e.g., `packs/test-core`) ensures that low-level config/bootstrap tests can run in fully air-gapped CI environments without copying or drifting against production Core assets.
* **Negative Read-Guard Assertion (§2627–2628):** Adding a negative materialization test that explicitly fails if any production code path reads `internal/bootstrap/packs/core` ensures absolute physical containment of the legacy bootstrap source tree.

---

## Critical Risks & Gaps (Evidence-Based)

### 1. Incomplete Hidden-Dependency Inventory & Code Dispositions
While the new symbol guard catches these dependencies reactively, the design's explicit tables and prose inventory (§3180–3185 / Attempt-14 table) have not been reconciled to include the following mechanism-coupled production files and functions:
* **`requiredBuiltinPackNames` ([cmd/gc/embed_builtin_packs.go:237](file:///data/projects/gascity/cmd/gc/embed_builtin_packs.go#L237)):** Still hardcodes `required := []string{"core", "maintenance"}`. When `maintenance` is retired as a standalone pack, this function must be updated or it will cause fatal startup failures due to the absence of the `maintenance` pack.
* **`publicSubpathForPack` ([internal/builtinpacks/registry.go:126](file:///data/projects/gascity/internal/builtinpacks/registry.go#L126)):** Maps pack names to public repo subpaths, explicitly mapping `"maintenance"` and `"gastown"`. This must be updated to remove `"maintenance"` once the pack is retired.
* **`internal/materialize/skills.go` (Lines 46, 206, 684–685):** Sources Core skills through `bootstrapSkillDirs()` and `bootstrap.PackNames()`. The design states that Core skill resolution moves to systempack sources (§2627) but never assigns a slice to rewrite this file.

### 2. `cmd/gc/prompt_test.go` Direct Disk-Read of Core Prompts
* **The Risk:** [cmd/gc/prompt_test.go:781–782](file:///data/projects/gascity/cmd/gc/prompt_test.go#L781-L782) reads two Core prompt files directly from disk:
  ```go
  "internal/bootstrap/packs/core/assets/prompts/pool-worker.md",
  "internal/bootstrap/packs/core/assets/prompts/graph-worker.md",
  ```
* **The Gap:** This is a `cmd/gc` test reading files directly from the source tree via `os.ReadFile`. It bypasses the `internal/config` test-only loaders and is not covered by the synthetic test-only hatch. The moment `internal/bootstrap/packs/core` is deleted, this test will fail with `ENOENT`.
* **Required Change:** Mandate that `cmd/gc/prompt_test.go` is refactored to read prompt assets from the embedded `core.PackFS` filesystem instead of relative disk paths. This aligns the test with the required single-embed model and makes it immune to source-tree deletions.

### 3. Stale "Dual Embed" Documentation Rationale
* **The Risk:** [internal/bootstrap/packs/core/embed.go:1–5](file:///data/projects/gascity/internal/bootstrap/packs/core/embed.go#L1-5) contains the following comment:
  ```go
  // Package core embeds the core bootstrap pack for bundling into the gc
  // binary. The same content is also reachable through the bootstrap's global
  // packs/** embed, but exposing a dedicated PackFS lets cmd/gc's per-city...
  ```
* **The Gap:** This comment explicitly documents the current dual-embed behavior as an intentional design. Once the production `//go:embed packs/**` is removed, this comment becomes incorrect and misleading.
* **Required Change:** Explicitly mandate that this comment is deleted or rewritten during the Core extraction slice to reflect the single-embed model.

### 4. Obsolete `GC_BOOTSTRAP` Env-Mutation Scaffolding
* **The Risk:** [internal/doctor/implicit_import_cache_check.go:235–249](file:///data/projects/gascity/internal/doctor/implicit_import_cache_check.go#L235-L249) defines `ensureBootstrapForDoctor` which unsets, saves, and restores `GC_BOOTSTRAP` around `bootstrap.EnsureBootstrap`.
* **The Gap:** Once `bootstrap.EnsureBootstrap` becomes a no-op (except for pruning retired entries) and `GC_BOOTSTRAP=skip` is deleted from production semantics (§2623), this environment-variable dance is obsolete and potentially harmful to state tracking.
* **Required Change:** Formally slate `ensureBootstrapForDoctor` for deletion or refactoring in the Core extraction slice, routing implicit checks through standard packsource classifiers instead.

---

## Technical Evaluation of Invariant Questions

### Q1. Does `internal/bootstrap` stop embedding production Core while keeping bootstrap tests deterministic through explicit isolated fixtures?
* **Yes:** The removal of `//go:embed packs/**` and the default initialization of `bootstrapAssets` to a private `errFS` returning `fs.ErrNotExist` successfully prevents the production binary from carrying duplicate Core assets. Tests achieve determinism by explicitly injecting inline `fstest.MapFS` fixtures.

### Q2. How is fixture drift against the shipped Core pack detected without causing low-level config tests to exercise production assets accidentally?
* **Decoupled Verification:** Core fidelity verification is moved entirely to systempacks integration tests, while low-level tests verify parsing/loading behavior on synthetic schemas.
* **Accidental Leakage Prevention:** The transition to AST source-symbol scanning (monitoring symbols like `bootstrap.PackNames` and `GC_BOOTSTRAP`) and the negative read-guard assertion (§2628) ensure that low-level config tests do not accidentally read or couple with real production Core.

### Q3. Are tests needing no-Core behavior using structurally test-only lower-level loaders rather than runtime flags or environment switches?
* **Yes:** Production CLI pathways do not support environment switches or runtime flags for skipping Core. Tests requiring no-Core behavior must call lower-level config loader packages directly.

---

## Required Changes Checklist

1. **Complete Hidden-Dependency Tables:** Reconcile the explicit tables with the symbol guard by adding rows for `requiredBuiltinPackNames` (`embed_builtin_packs.go`), `publicSubpathForPack` (`registry.go`), and `internal/materialize/skills.go`, assigning them clear refactoring dispositions and slice assignments.
2. **Refactor `prompt_test.go`:** Explicitly mandate that `cmd/gc/prompt_test.go` is refactored to read prompts from the embedded `core.PackFS` instead of relative disk paths.
3. **Clean Up Dual-Embed Comments:** Require the deletion or modification of the stale comment in `internal/bootstrap/packs/core/embed.go` that describes the dual-embed design.
4. **Retire Doctor Env Mutation Helper:** Remove the `ensureBootstrapForDoctor` environment-variable mutation logic in `implicit_import_cache_check.go` since `GC_BOOTSTRAP=skip` is deleted from production semantics.
5. **Pruning Exclusions for Retired Directories:** Ensure `pruneStaleGeneratedPackFiles` is updated to explicitly ignore/skip retired systempack directories under `.gc/system/packs/` (e.g., `maintenance`, `gastown`) to prevent accidental deletion of user-modified custom configurations in former system pack folders.

---

## Reflective Questions

* **Can we delete `internal/bootstrap` entirely?** Once the Core pack is fully extracted and the Maintenance pack is retired, `internal/bootstrap` is reduced to a skeleton package carrying out retired implicit-import pruning. Could this pruning logic be consolidated into `internal/systempacks` or `internal/packsource`, allowing us to delete the entire `internal/bootstrap` package and clean up the Go codebase?
* **Air-Gapped Pack Verification:** How will the `test/packcompat` integration suite verify remote public Gastown versions in fully air-gapped CI environments? Is a local vendor-caching or registry-mirror fallback explicitly planned for the cache/rollback ledger?
