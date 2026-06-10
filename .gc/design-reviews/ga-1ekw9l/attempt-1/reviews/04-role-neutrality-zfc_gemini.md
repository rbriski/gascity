# Anand Krishnaswamy — Role Neutrality & ZFC Invariant Reviewer (Attempt 12 / Independent DeepSeek V4 Flash Style)

**Verdict:** BLOCK (Iterate Required)

> **Lane:** Zero hardcoded roles in Go and assets, the symbolic maintenance-worker binding, SDK self-sufficiency, and ZFC (Zero Framework Cognition) judgment containment.
>
> Reviewed against the Attempt 12 implementation plan (`plans/core-gastown-pack-migration/implementation-plan.md`, 1416 lines, `updated_at: 2026-06-10T08:33:51Z`) — specifically §"Role Neutrality And Configurable Bindings" (lines 759–859), §"Bootstrap Fixture Isolation" (lines 860–924), and §"Operator Docs And Generated References" (lines 925–950).
>
> This independent review is produced using the DeepSeek V4 Flash style, focusing rigorously on first-principles ZFC compliance, cross-document consistency, identifying missing edge cases, and exposing hidden assumptions other reviewers may accept too quickly.

---

## Schema Conformance

**Conforms.** The Attempt 12 implementation plan contains all required top-level sections in the correct order, carries the required front matter (`phase: implementation-plan`), and correctly integrates the role neutrality, symbolic binding resolver, and active-root scanner designs into the Proposed Implementation, Data And State, Testing, and Rollout sections rather than appending them as unstructured prose.

---

## Top Strengths of the Proposed Design

1. **Clean Removal of Legacy Theme and Emoji Hardcoding (Slice 3):**
   The plan's commitment to delete `roleEmoji` (`internal/runtime/tmux/tmux.go:80-89`) and the `MayorTheme`/`DeaconTheme`/`DogTheme` functions (`internal/runtime/tmux/theme.go:33-47`) in Slice 3 is excellent. Replacing these compile-bound Go symbols with a generic, data-driven theming model declared via `[appearance.themes.<name>]` in the Gastown public pack and backed by a neutral Core default palette is a textbook ZFC-aligned solution.
2. **Comprehensive Role-Surface Manifest Scope:**
   Explicitly including API classifications (`classifyAgentKind`/`agent_kind` field), generated client/dashboard types, dashboard CSS classes (`dog-*`), and core-owned asset IDs (`mol-dog-jsonl`, `mol-dog-reaper`) in the role-surface manifest and regeneration gates (Slice 3 and 5b) addresses the previous gaps perfectly. It ensures that role-neutrality is treated as a systemic, compile-to-UI invariant rather than a superficial Go-source regex match.
3. **Explicit Data-Declared Binding Authority and Precedence:**
   Freezing binding declarations in `pack.toml` under `[bindings.<key>]` with `required` (bool), `default` (string), and `description` attributes (lines 810–814) successfully removes the Go runtime from the business of classifying binding requirements. Establishing a total precedence hierarchy (`city` > `system_pack` > `env` > `pack default`) provides a clear, predictable resolution pathway.
4. **Active Root Containment via `ActiveRootsFor`:**
   Enforcing that loaders, installers, cache readers, and doctor checks resolve pack paths strictly through `internal/packsource.ActiveRootsFor(kind)` prevents ad-hoc globbing and path-traversal bugs. The scanner check that rejects raw file-system walks over pack roots enforces this architectural boundary at compile time.

---

## Critical Risks & Architectural Gaps

### 1. [Blocker] Non-Generic Environment Override Layer (`GC_CORE_MAINTENANCE_WORKER`) (ZFC Violation)
* **The Risk:** The plan specifies that the environment injection layer resolves the environment variable `GC_CORE_MAINTENANCE_WORKER` (lines 819-820, 825).
* **The Impact:** This is a clear violation of **Zero Framework Cognition (ZFC)**. Hardcoding a specific binding-specific environment variable (`GC_CORE_MAINTENANCE_WORKER`) directly in the Go configuration loader means the Go framework retains explicit, compile-bound cognition of a specific role concept. If a user or an external pack introduces a new required binding (e.g., `escalation_recipient`), they cannot override it via the environment because the loader only knows how to inject `GC_CORE_MAINTENANCE_WORKER`.
* **Resolution:** Replace the single hardcoded `GC_CORE_MAINTENANCE_WORKER` variable with a generic, dynamic environment binding scheme. The configuration loader must translate all variables prefixed with `GC_BINDINGS_<KEY_IN_UPPERCASE>` into their respective binding overrides. Go source code must not contain any literal reference to a specific binding-named environment variable.

### 2. [Major] Cascading Failures for Downstream Steps with Disabled/Omitted Optional Bindings
* **The Risk:** The plan states: *"a disabled or omitted optional binding skips user-agent work with a typed diagnostic naming the binding key"* (lines 830–831). 
* **The Impact:** When a step's user-agent work is skipped because its binding is omitted or disabled, it produces no output data. If a downstream step has a control or data dependency on this step (e.g., reading its output files or relying on its completion status), the downstream step will crash or fail loudly with a missing-dependency or bad-input error.
* **The Gap:** The plan is silent on how downstream dependencies in a formula or molecule propagate when an upstream step is skipped. It does not define whether "skipping user-agent work" translates to a first-class `skipped` bead status, a cascading skip for downstream dependent steps, or a mock/neutral success state.
* **Resolution:** Define the exact execution-state and data-propagation rules for skipped optional steps. Specify whether downstream dependent steps are skipped automatically (cascading skip) or if they must handle empty inputs gracefully.

### 3. [Major] Unaddressed Conflict Resolution for Duplicate Binding Declarations
* **The Risk:** In a multi-pack city, multiple active packs might declare the same binding key in their respective `pack.toml` files.
* **The Impact:** If Pack A declares `foo` as `required = true` with no default, while Pack B declares `foo` as `required = false` with a default `"helper"`, the system is in an unresolvable transitive conflict state.
* **The Gap:** While AC3 addresses pack-resolution diamond conflicts, the plan does not define how the binding parser resolves conflicting metadata (requiredness, defaults, descriptions) for identical binding keys across different active packs.
* **Resolution:** Add a strict conflict-resolution rule for binding declarations. If two active packs declare the same binding key with conflicting `required` flags or different `default` values, the configuration loader must fail-closed during preflight loading with a descriptive duplicate-binding diagnostic.

### 4. [Major] In-Flight Beads with Legacy Route Literals (`dog`) Stalled Mid-Flight
* **The Risk:** The plan introduces a fail-closed rule for persisted bead/molecule metadata: *"unresolvable literals fail that step closed with a typed diagnostic... and are never silently re-routed"* (lines 837–843).
* **The Impact:** If an operator upgrades a running city and renames the maintenance worker from `dog` to `reconciler`, all active, in-flight beads that were routed to `dog` will immediately fail closed upon the next evaluation loop.
* **The Gap:** While the plan specifies that `mol-dog-jsonl` and `mol-dog-reaper` are renamed and aliased in the order skip ledger (lines 786–790), it provides no migration path for live, active database beads carrying `gc.routed_to = "dog"`. Expecting operators to manually edit database tables or abort all running molecules mid-flight is a severe operational hazard.
* **Resolution:** Extend `gc doctor --fix` to safely rewrite `gc.routed_to`, `gc.run_target`, mail recipients, and nudge targets in active, open beads from legacy literal roles (e.g., `dog`) to the configured symbolic binding names during upgrade.

### 5. [Minor] Test-Fixture "Dog" Hardcoding and Leakage
* **The Risk:** The plan allows `dog` to appear in configuration tests (lines 806–808).
* **The Impact:** If all tests continue to use `dog` as their primary target, the system may pass CI while retaining implicit string-matching or prefix-matching dependencies on `dog` in active behavior.
* **Resolution:** Enforce that all core maintenance and routing tests run under at least one randomized, non-default executor name (e.g., `reconciler-8f9d`) to prove that active routing is completely independent of the literal string `"dog"`.

---

## Detailed Responses to Lane-Specific Questions

### Q1: After binding indirection, does any Go, prompt asset, script, formula, order, generated help, or API route still branch on dog, Mayor, Maintenance, or another concrete role name?
* **Answer:** With the Attempt 12 updates, the compile-bound branching on tmux themes, emoji maps, and warmup mail defaults has been successfully removed from Go and shifted to data-driven config declarations. However, a ZFC-violating exception remains in the Go environment loading layer, which still explicitly references the concrete role variable `GC_CORE_MAINTENANCE_WORKER` instead of a generic binding override prefix.

### Q2: Can controller-owned SDK operations still run when the configured maintenance worker is renamed or omitted, with no dependency on a user agent entry?
* **Answer:** Yes. The introduction of `[bindings.<key>]` tables in `pack.toml` with declared requiredness ensures that the Go controller executes generic loading and scheduling rules over metadata, preventing hardcoded Go-level judgment calls. However, cascading control and data dependencies for downstream steps when optional bindings are omitted or disabled remain undefined and present a runtime risk.

### Q3: Are role-name allowlists narrow, time-bounded, and failing when compatibility fixtures leak into live behavior?
* **Answer:** Yes. Constraining allowlist rows to specific contexts (narrowly scoped migration docs, generated review artifacts, source-attribution examples, and absence-test fixtures) aligns with AC8. The inclusion of the active behavior scanner and the wording matrix with build-time failure on expired rows provides the required enforcement. However, tests must be forced to use randomized names to ensure compatibility fixtures do not mask hidden string dependencies.

---

## Required Changes

Before the implementation plan can be approved for decomposition, the following changes must be made:

1. **Generalise the Environment Layer (ZFC Correctness):**
   * Change the env override logic in the config parser to resolve environment variables generically using a `GC_BINDINGS_<KEY>` pattern (e.g., `GC_BINDINGS_CORE_MAINTENANCE_WORKER` or `GC_BINDINGS_MAINTENANCE_WORKER`). Remove all literal references to `GC_CORE_MAINTENANCE_WORKER` from production Go source files.
2. **Define Downstream Skipped Propagation:**
   * Add a paragraph in the "Role Neutrality And Configurable Bindings" section defining how downstream dependent steps behave when an upstream step is skipped due to a disabled/omitted optional binding (e.g., cascading skips vs empty inputs).
3. **Add Binding Declaration Collision Rules:**
   * Explicitly state that the configuration loader will fail-closed during preflight loading if two active packs declare the same binding key with conflicting properties (`required` flag or `default` values).
4. **Provide Task-Store (Bead) Migration Path:**
   * Add a task in Slice 5b or Slice 10 to extend `gc doctor --fix` to remediate active, in-flight beads in the Dolt task store whose route metadata (`gc.routed_to`, `gc.run_target`, mail, nudge) still contains legacy role names (`dog`, `mayor`).
5. **Mandate Randomized Test-Fixture Names:**
   * Add an assertion under "Testing" requiring that core-owned maintenance and routing tests run under randomized worker names to prove the system is robust against non-default configurations.

---

## Questions

1. Will the environment override layer support all declared bindings generically via a `GC_BINDINGS_<UPPERCASE_KEY>` scheme?
2. How does a downstream step detect that an upstream optional step was skipped vs failed vs completed?
3. Will `gc doctor --fix` be extended to remediate in-flight task-store (bead) route literals?
