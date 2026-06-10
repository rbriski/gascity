# Ritu Raman — DeepSeek V4 Flash Perspective Independent Review (Iteration 21 / Attempt 21)

**Verdict:** approve

**Scope:** Bootstrap embed cleanup, deterministic test fixtures, test-only no-Core path containment, hidden dependency discovery.

This independent review evaluates the Iteration 21 / Attempt 21 draft of the Core and Gastown Pack Split design (`.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`) against the approved `requirements.md` and the active codebase at the `rig_root` (`/data/projects/gascity`).

---

## Executive Summary

As Ritu Raman, the **Bootstrap Fixture Isolation Reviewer**, I am issuing a **Verdict: Approve** for the Iteration 21 design.

This design presents an exceptionally elegant, disciplined, and structurally sound plan for decoupling the production binary from legacy global implicit imports and isolating bootstrap fixtures. By shifting from standard file-path matching to a comprehensive **source-symbol guarded AST scanner** and explicitly defining the production default of `bootstrapAssets` as a private, non-nil, empty filesystem that returns `fs.ErrNotExist` (§3159–3161), the design ensures absolute architectural hygiene.

The critical blocking gaps identified in early iterations have been completely resolved:
1. **Opaque Nil-FS Panics Avoided:** The production fallback for `bootstrapAssets` is explicitly specified as a non-nil FS implementation returning `fs.ErrNotExist`, preventing runtime crashes on any standard loader paths that execute with empty bootstrap pack lists.
2. **`GC_BOOTSTRAP=skip` Production Escape Hatch Closed:** The environment variable is fully retired from production code paths, ensuring it cannot be exploited as a production bypass while strictly isolating its legacy utility to local test-fixture setup.
3. **Contradictory "Tiny Embed" Clause Removed:** The conflicting legacy compatibility embed has been fully purged. Low-level test suites are strictly mandated to use synthetic inline `fstest.MapFS` configurations, preventing local disk-state leakage and drift in tests.
4. **Comprehensive Hidden-Dependency Map:** Critical files that traditionally read files directly from the bootstrap tree—such as `cmd/gc/prompt_test.go` and `internal/config/bundled_import_test.go`—are formally integrated into the Slice 3/Core-extraction checklist (§3180–3185).

---

## Top Strengths of Current Design

* **Absolute Production Decoupling (§3170–3171):** Completely deleting the `//go:embed packs/**` directive from `internal/bootstrap/bootstrap.go` guarantees that no Core assets are accidentally bundled into the production binary.
* **Hermetic, Non-Nil Private File System (§3159–3161):** Hardcoding `bootstrapAssets` to default to an empty/erroring filesystem prevents standard loading routines from throwing nil-pointer panics, while providing clean, predictable `fs.ErrNotExist` errors if accessed.
* **Inline `fstest.MapFS` Test Isolation (§3162–3164):** Eliminating file copies or caches inside bootstrap unit tests and using mock virtual filesystems instead prevents test-suite divergence and local disk-drift.
* **Negative Asset-Presence Guard (§3186–3188):** Implementing `TestBootstrapFixtureIsMinimal` (which explicitly fails if synthetic fixtures include production-only directories such as `formulas/`, `orders/`, `overlay/`, `skills/`, or `assets/prompts/`) guarantees that synthetic test configurations stay minimal.
* **Astute Symbol-Aware Guarding (§3176–3179):** The guard scans imports, symbol usage, generated references, and hook configurations for explicit couplings (e.g., `bootstrap.BootstrapPacks`, `bootstrap.PackNames`), neutralizing the risk of hidden runtime dependencies breaking compiled builds.

---

## Nuanced Risks & Operational Recommendations

While the design is fully mature and approved, the following specialized technical recommendations are provided from the DeepSeek V4 Flash perspective to ensure flawless execution and long-term maintainability:

### 1. Centralize Synthetic Fixture Construction to Prevent Inline Literal Duplication
* **The Risk:** Defining inline `fstest.MapFS` literals separately in each test file (such as `bundled_import_test.go`, `prompt_test.go`, and legacy collision suites) can lead to micro-divergence in test-suite expectations. If the core parsing schema of `pack.toml` changes in the future, developers will have to manually update multiple inline string blocks, introducing a maintenance bottleneck.
* **Recommendation:** Implement a centralized, test-only helper function (e.g., `testhelper.NewMinimalCoreMapFS()`) in an unexported, test-internal file. This helper should return the canonical, valid minimal schema for a Core-like fixture. All unit tests that require synthetic mock configurations should consume this helper to enforce uniform expectations.

### 2. Guard Against Test-Binary Reassignment of `bootstrapAssets`
* **The Risk:** If unit tests reassign `bootstrapAssets` globally during initialization (e.g., via `init()` or `TestMain`), there is a risk that `TestProductionBootstrapAssetsIsEmpty` could evaluate against a reassigned/test-mocked filesystem in unified test binaries. This would create a false-positive success where the production-empty invariant is not actually checked.
* **Recommendation:** Mandate that `TestProductionBootstrapAssetsIsEmpty` verifies the state of a freshly constructed or un-overridden production variable, or ensure that the test executes in a clean, non-overridden environment (for example, by verifying `bootstrapAssets`' actual type/identity before walking it).

### 3. Verify Hook-Overlay Loading Behavior in Slice 3
* **The Risk:** The hooks package (`internal/hooks/hooks.go`) relies on loading provider-specific overlays. As Core assets move from `internal/bootstrap/packs/core` to `internal/packs/core`, unstated string constants or legacy path references within the hook-loading subsystem might escape standard compilation errors.
* **Recommendation:** Ensure that the Slice 3 gate checks include a specific integration-level verification verifying that `internal/hooks` cleanly walks and materializes overlays from the new `core.PackFS` filesystem without assuming disk locations.

---

## Technical Evaluation of Invariant Questions

### Q1. Does `internal/bootstrap` stop embedding production Core while keeping bootstrap tests deterministic through explicit isolated fixtures?
* **DeepSeek V4 Flash Finding:** **Yes.** Deleting the `//go:embed packs/**` directive and reassigning `bootstrapAssets` to an erroring filesystem successfully purges production Core. Bootstrap tests are fully isolated and deterministic using synthetic, inline `fstest.MapFS` configurations that simulate a minimal Core pack structure without depending on real physical files.

### Q2. How is fixture drift against the shipped Core pack detected without causing low-level config tests to exercise production assets accidentally?
* **DeepSeek V4 Flash Finding:** **Satisfactory.** The strict inline nature of tests ensures they never interact with production files. Schema drift or behavioral drift is validated at the higher-level integration layer via the `test/packcompat` suite. Meanwhile, synthetic unit-test fixture growth is restricted by `TestBootstrapFixtureIsMinimal` (§3186–3188), which fails if any production-only files or folders are accidentally included.

### Q3. Are tests needing no-Core behavior using structurally test-only lower-level loaders rather than runtime flags or environment switches?
* **DeepSeek V4 Flash Finding:** **Yes.** The complete retirement of `GC_BOOTSTRAP=skip` as a production bypass (§3189–3193) ensures that the normal runtime loading paths cannot be bypassed using environment flags. Any test needing specialized "no-Core" behavior must load the configuration using low-level, internal package-level loaders explicitly, maintaining strict structural separation.

---

## Recommendations for Finalization

1. **Centralize Test Mocks:** Explicitly include the creation of a centralized `NewMinimalCoreMapFS()` helper in the Slice 3 refactoring tasks.
2. **Double-Check Hook Path Constants:** Ensure AST scanner checks specifically target any occurrences of `"internal/bootstrap/packs/core"` or `"packs/core"` within the `internal/hooks` directory.
3. **Verify No-Op in Doctor Environment Dance:** Reconcile `internal/doctor/implicit_import_cache_check.go`'s environment variable manipulation to ensure that unsetting `GC_BOOTSTRAP` behaves as a safe, explicit no-op once the production switch is retired.
