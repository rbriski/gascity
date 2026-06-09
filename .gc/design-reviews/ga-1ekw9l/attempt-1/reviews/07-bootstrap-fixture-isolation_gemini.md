# Hiroshi Tanabe — DeepSeek V4 Flash (Bootstrap Fixture Isolation Reviewer, Attempt 2, Independent Review)

**Verdict:** approve

**Lane:** production Core embed removal, non-nil empty bootstrap fs, fixture-only tests, GC_BOOTSTRAP skip containment, hidden dependency inventory.

Reviewed against the Iteration 2 / Attempt 2 draft of `plans/core-gastown-pack-migration/implementation-plan.md` and `plans/core-gastown-pack-migration/requirements.md` in the active repository workspace.

---

## Executive Summary

As Hiroshi Tanabe, the **Bootstrap Fixture Isolation Reviewer**, I have conducted an independent, evidence-backed safety and risk audit of the Iteration 2 (Attempt 2) design for the Core and Gastown Pack Split. My verdict is **Verdict: approve**.

In this iteration, the design team has directly and systematically resolved all of the critical risks, scheduling conflicts, and fixture-isolation gaps identified during the Iteration 1 review. Most notably, the rollout ordering conflict regarding implicit import collision gates has been fully resolved by keeping legacy bootstrap collision protection active until the replacement `internal/systempacks` gates are proven live. 

The proposed design is robust, provides compile-time and runtime backstops, and is fully ready for safe implementation.

---

## Top Strengths

- **Resolved Rollout Ordering & Collision Protection (§209–213)**: The design now explicitly mandates that existing bootstrap collision protection for `core`, `maintenance`, and `gastown` is kept active through Slice 3 (Core Extraction) and removed only in Slice 4 when the new `internal/systempacks` collision gates are live. This prevents the dangerous "collision-gap" window present in Attempt 1.
- **Bulletproof Compile-Time Go Import Gates (§79–84)**: Deleting the `internal/bootstrap/packs/core` Go package automatically makes any remaining production Go imports of the deleted source a compile-time error. This compile-time guard is paired with path scanner tests (modeled on `cmd/gc/worker_boundary_import_test.go`) to reject direct `config.Load*` bypasses, providing a highly resilient dual-layer guard.
- **Strict `GC_BOOTSTRAP=skip` Test-Only Containment (§318–322)**: The skip environment variable is structurally barred from bypassing required Core file-set validation, participation tracking, or systempacks loading. It is constrained strictly to skipping legacy empty bootstrap fixture materialization, preventing operators or tests from using it to bypass the required system-pack loader.
- **Fixture Isolation and Copy Prevention (§314–316)**: Bootstrap test fixtures are restricted to minimal or empty `fs.FS` inline fixtures whose `Stat`, `WalkDir`, and `ReadFile` behavior is explicitly asserted. Furthermore, the design introduces fixture guard tests that fail if allowed test paths copy production Core directories (`formulas/`, `orders/`, `overlay/`, `skills/`, or `assets/prompts/`), preventing test-only crutch drift.
- **Comprehensive Hidden-Dependency Audit (§307–310)**: The dependency audit has been expanded to explicitly include doctor checks (`internal/doctor/implicit_import_cache_check.go`), command tests, testscript defaults, and other prompt or config tests.

---

## Technical Audit & Verification of Changes

Compared to the Iteration 1 draft, the design now has concrete answers and explicit specifications for every critical risk:

| Legacy Risk (Iteration 1) | Iteration 2 Resolution | Evidence in Plan |
| --- | --- | --- |
| Rollout Contradiction / Collision Gap | Existing bootstrap collision protection remains active through Slice 3 and is removed only in Slice 4 when replacement gates are live. | §209–213, §470–476 |
| Uninventoried Doctor Env-Mutation | Explicit audit of `GC_BOOTSTRAP` includes doctor checks, command tests, and testscript defaults. | §307–310 |
| Test Harness Masking via Skip Defaults | Dual-layer scanner tests and explicit required-pack load/participation validation tests block bypasses. | §202–208, §318–322, §407–410 |
| Weak Fixture Minimality Guards | Fixture guard tests fail if allowed test paths copy production Core directories, preventing drift. | §314–316 |

---

## Operational Notes & Continuous Monitoring

While the design is fully approved, the following operational nuances should be verified in code reviews and test assertions:

1. **Delete the `//go:embed packs/**` Directive**: During Slice 3 (Core Extraction), when `internal/bootstrap/packs/core` is deleted, ensure that the `//go:embed packs/**` directive in `internal/bootstrap/bootstrap.go` is deleted alongside it (not just the files). An embed directive pointing to an empty or non-existent path is a compile error in Go.
2. **Exercise Empty FS Return Types**: Verify that tests using the empty/minimal `fs.FS` replacement for `bootstrapAssets` cover the `fs.ErrNotExist` error path across `Stat`, `WalkDir`, and `ReadFile` calls to guarantee that no silent panic occurs when a consumer attempts to read from an emptied bootstrap.

---

## Questions

### Detailed Responses to Lane-Specific Questions

#### Q1: After removing production Core from bootstrap embeds, what compile-time or CI check proves no production path imports the deleted bootstrap Core package?
**Answer**: Once the `internal/bootstrap/packs/core` package is deleted from the source tree, any lingering production import of it in Go code will result in an immediate compile-time error during standard compilation (`go build` or `go test`). For non-Go files, configuration strings, and document references, the string path scanner tests (modeled on `cmd/gc/worker_boundary_import_test.go` and integrated into `test/packlint`) provide a static path-guard check that fails if forbidden bootstrap paths are detected.

#### Q2: Are tests that need Core assets using minimal fstest fixtures or the relocated system-pack wrapper, not copied production Core snapshots?
**Answer**: Yes. The design specifies that tests requiring bootstrap assets must use an empty `fs.FS` fixture or a minimal inline fixture whose `Stat`, `WalkDir`, and `ReadFile` behavior is asserted. Copying production Core snapshots is forbidden, and the design mandates fixture guard tests that explicitly fail if any allowed test path copies production-only Core directories such as `formulas/`, `orders/`, `overlay/`, `skills/`, or `assets/prompts/`.

#### Q3: Is GC_BOOTSTRAP=skip narrowed to fixture or no-Core tests and structurally unreachable as a production required-system-pack bypass?
**Answer**: Yes, `GC_BOOTSTRAP=skip` is structurally constrained so that it may only skip empty legacy bootstrap fixture materialization. It is explicitly prevented from skipping `internal/systempacks` materialization, required Core file-set integrity checks, retired-source classification, collision checks, typed participation validation, provider-pack materialization, or doctor cleanup. This makes the skip switch structurally unreachable as a backdoor or production bypass.
