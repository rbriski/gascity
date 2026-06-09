# Hiroshi Tanabe — Bootstrap Fixture Isolation Reviewer (Attempt 7, Independent DeepSeek V4 Flash Style)

**Verdict:** block

> **Lane:** Production Core embed removal, non-nil empty bootstrap `fs.FS`, fixture-only tests, `GC_BOOTSTRAP=skip` containment, hidden-dependency inventory.
>
> Reviewed against the Attempt 7 design document (`.gc/design-reviews/ga-1ekw9l/attempt-7/design-before.md`, 835 lines, `updated_at: 2026-06-09T13:20:59Z`) — specifically §"Bootstrap Fixture Isolation" (lines 459–494), §"Summary" (lines 15–30), and §"Proposed Implementation" (lines 78–150).
>
> This independent review is produced using the DeepSeek V4 Flash persona, focusing specifically on first-principles trust boundaries, cross-document state consistency, and unstated runtime assumptions.

---

## Schema Conformance

Conforms to `gc.mayor.implementation-plan.v1`. Front matter carries the required keys with `phase: implementation-plan` and no `design_file`; the eight required top-level sections appear once each in the required order, and `Open Questions` is `None`. No appended attempt/review prose in the artifact.

---

## Top Strengths of the Design

- **Non-nil production `bootstrap.EmptyFS` (Lines 482–484):** Explicitly specifying a custom empty `fs.FS` whose `Open(".")` returns an empty directory and whose `Open` for all other names returns `fs.ErrNotExist` successfully resolves the nil-`fs.FS` panic risks in compatibility lookups. This is a sound and robust structural solution.
- **Strict Copy-Ban Fixture Guard (Lines 478–480):** Asserting compile-time / test-time failures if test paths attempt to copy production-only Core directories (`formulas/`, `orders/`, `overlay/`, `skills/`, or `assets/prompts/`) kills potential copy-drift hazards at their root.
- **Exposing Overlays and Hook Dependencies (Lines 463–465):** Requiring an audit of hook references and prompt tests forces the relocation plan to recognize how deep Core asset consumption reaches, protecting the loader boundary.

---

## Critical Risks & Consensus Blockers (DeepSeek V4 Flash Style)

### 1. [Blocker] Unresolved Empty Embed Directive Causes Immediate Go Compilation Failure
- **The Risk:** Lines 468–469 state that "no production `//go:embed packs/**` may carry Core assets."
- **The Impact:** Under the current repository layout, `internal/bootstrap/packs/core` is the *only* subdirectory of `internal/bootstrap/packs/`. When Core is extracted, this directory will be completely deleted. The Go compiler's `//go:embed packs/**` directive in `internal/bootstrap/bootstrap.go:22` requires matching at least one file. Deleting the `core` subdirectory leaves `packs/` empty, causing a hard build error: `pattern packs/**: no matching files found` and blocking compilation of the master branch.
- **Required Resolution:** The design must couple directory deletion + directive removal + `bootstrapAssets`→`EmptyFS` initialization as a single, indivisible atomic edit in Slice 3. The directive must be deleted or replaced with an explicit mock placeholder file in `internal/bootstrap/packs/` (e.g., `packs/.placeholder` containing mock metadata) to keep the compiler happy.

### 2. [Blocker] Production-Side Test-Dependency Leak (The `fstest` Trap)
- **The Risk:** Lines 476–478 and 482–484 suggest that tests and production paths use an empty `fs.FS` fixture or `testing/fstest` structures for empty directory behavior.
- **The Impact:** If developers use `testing/fstest.MapFS{}` or any other test-only package to implement `bootstrap.EmptyFS` in `internal/bootstrap/bootstrap.go`, it will introduce an import of the `testing` package in production Go code. This is strictly prohibited; importing `testing` in production leaks test flags (like `-test.v`, `-test.run`) into the production CLI binary, causing CLI flag parsing failures and bloating binary size.
- **Required Resolution:** The design must explicitly mandate that `bootstrap.EmptyFS` is implemented using a custom, lightweight, package-private struct in `internal/bootstrap/bootstrap.go` (e.g., `type emptyFS struct{}` implementing `fs.FS`) that has zero imports of the standard `testing` or `testing/fstest` packages.

### 3. [Blocker] The Slice 3 Collision Protection Gap (Rollout Race Condition)
- **The Risk:** Slice 3 (lines 747–750 and the slice-to-gate table on lines 797–808) extracts Core and deletes old embeds. However, the new system-pack level collision gates are not fully specified until Slice 4a (lines 756–760).
- **The Impact:** In `internal/bootstrap/collision_test.go`, collision checks compare against embedded assets. Deleting the embedded `packs/core` files in Slice 3 immediately makes the old collision check toothless for Core. This creates a dangerous rollout window in Slice 3 where no collision protection is active for the Core system pack, before the replacement required-system-pack collision gates under `internal/systempacks` are active in Slice 4a.
- **Required Resolution:** The design must mandate that replacement system-pack collision gates are introduced in parallel with Core extraction (Slice 3) or explicitly document the risk boundary and a bounded mitigation keeping fail-closed protection active.

### 4. [Major] Conflated Test Guidance Risks Silent Coverage Loss
- **The Risk:** Lines 476–478 state that "Tests that need bootstrap assets use an empty `fs.FS` fixture or minimal inline fixture."
- **The Impact:** This blanket instruction conflates mechanism tests (validating bootstrap FS handling) with content-fidelity tests. Core content-fidelity tests (such as `cmd/gc/prompt_test.go:781-782` checking real prompts, or hook tests asserting overlays) *cannot* function with an empty or minimal FS. Banning copies and enforcing an empty FS on them would silently wipe out their assertion coverage.
- **Required Resolution:** The design must explicitly bifurcate its test-isolation directives:
  - **Mechanism-Only Tests:** Use empty or minimal inline `fs.FS` fixtures to prove bootstrap logic.
  - **Content-Fidelity Tests:** Must read the relocated Core assets through `internal/packs/core` or `internal/systempacks`, completely bypassing the bootstrap empty FS layer.

### 5. [Major] `GC_BOOTSTRAP=skip` Containment is Asserted, Not Enforced
- **The Risk:** Lines 489–493 assert that `GC_BOOTSTRAP=skip` may skip only empty bootstrap fixture setup and must never bypass required system-pack validation.
- **The Impact:** Currently, `GC_BOOTSTRAP` is set as a blanket suite-wide default in `cmd/gc/main_test.go:45` and `cmd/gc/main_test.go:62`. Furthermore, `ensureBootstrapForDoctor` in `internal/doctor/implicit_import_cache_check.go:235-247` executes an environment-variable unset/restore dance to defeat skip. Without explicit enforcement, there is a risk that `skip` remains an active required-pack bypass in production, violating the SDK's self-sufficiency invariant.
- **Required Resolution:**
  - Define a positive containment test that runs a `gc` command or loader path with `GC_BOOTSTRAP=skip` set, with Core assets missing/corrupt, and asserts that it still fails closed.
  - Require the rewritten doctor check to completely delete the vestigial environment-variable mutation dance.
  - Document whether the blanket suite-wide default in `main_test.go` will be narrowed or retired.

### 6. [Minor] Fragile Hardcoded Guard Lists
- **The Risk:** Lines 478–480 specify a hand-curated prohibited list of Core directories (`formulas/`, `orders/`, `overlay/`, `skills/`, `assets/prompts/`).
- **The Impact:** If Core adds a new subdirectory in the future, a static hand-curated list in the test guard will fail to protect it, allowing silent test-copy drift to re-emerge.
- **Required Resolution:** Mandate that the fixture guard derives its list of prohibited directories dynamically from the actual directories present in `internal/packs/core` rather than using a hardcoded list.

---

## Detailed Responses to Lane-Specific Questions

### Q1: After removing production Core from bootstrap embeds, what compile-time or CI check proves no production path imports the deleted bootstrap Core package?

**Answer:**
1. **Compilation-Failure Proof:** Deleting `internal/bootstrap/packs/core/` (its `embed.go` and assets) provides a hard compile-time proof. Any production file still importing `github.com/gastownhall/gascity/internal/bootstrap/packs/core` will trigger an immediate Go build failure.
2. **Scanner Gate:** A static path-string scanner (similar to `worker_boundary_import_test.go`) must cover the `internal/` packages (not just `cmd/gc`) to scan for legacy string literals (like `"internal/bootstrap/packs/core"`, `"packs/core"`, and legacy `Subpath` values), asserting zero matches outside historical/migration documentation.

---

### Q2: Are tests that need Core assets using minimal fstest fixtures or the relocated system-pack wrapper, not copied production Core snapshots?

**Answer:**
Not yet separated in the current design. To avoid critical coverage loss:
1. **Mechanism tests** (e.g., `bootstrap_test.go`) must use the custom `bootstrap.EmptyFS` or mock fixtures.
2. **Content-fidelity tests** (e.g., prompt tests, hook tests) must read relocated assets from `internal/packs/core` or the `internal/systempacks` wrapper.
3. **Seam Overrides:** The package-private `bootstrapAssets` variable in `internal/bootstrap` must be exposed as an internal test injection seam (or per-call parameter) to allow collision tests to inject custom mock FS instances without reaching production-side globals.

---

### Q3: Is GC_BOOTSTRAP=skip narrowed to fixture or no-Core tests and structurally unreachable as a production required-system-pack bypass?

**Answer:**
It is asserted but lacks the concrete, fail-closed negative test required to prove it. Since `skip` is read only at `internal/bootstrap/bootstrap.go:72`, it is structurally isolated from `internal/systempacks` loading. However, to guarantee containment, a negative integration test must be added to assert that `skip` cannot bypass missing/corrupt required system packs. The doctor unset/restore dance must be completely deleted, and `cmd/gc/main_test.go` suite-wide defaults must be retired or documented as no-ops.

---

## Evaluation Against Lane Anti-patterns

| Anti-pattern / Red Flag | Mitigation in Current Design | Status |
| :--- | :--- | :--- |
| **Silent production-embed bloat** | **Excellent.** Banned Core embeds from production bootstrap. | **Pass** |
| **Copy-drift on Core directories** | **Excellent.** Copy-ban guard covers Core dirs. | **Pass** |
| **Production `testing` imports** | **Missing.** Suggestions of using testing FS helpers in production. | **Fail (Blocker)** |
| **Unresolved embed directive failures** | **Missing.** No directive removal/retargeting in relocation commits. | **Fail (Blocker)** |
| **Toothless rollout collision window** | **Missing.** Slice 3 Core deletion runs before Slice 4a collision gates. | **Fail (Blocker)** |

---

## Final Verdict: Block

While the high-level goals of "Bootstrap Fixture Isolation" are extremely precise and welcome, I must **Block** the plan in its current form due to three severe compile-time and rollout-safety defects: the **unresolved empty embed directive pattern** causing immediate build failures, the **production-side test-dependency leak hazard** (`fstest` in production code), and the **Slice 3 toothless collision window**. Implementing a custom empty FS struct, coupling directive removal with asset deletion, closing the Slice 3 rollout gap, and explicitly bifurcating mechanism versus content test guidance are necessary to make this migration secure and buildable.
