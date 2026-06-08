# Ingrid Kovac — ZFC & Role-Neutrality Guardian (Iteration 14 / Attempt 14, Independent DeepSeek V4 Flash Style Review)

**Verdict:** BLOCK

**Scope:** Zero hardcoded roles, Core role neutrality, `dog` exception containment, SDK self-sufficiency, Go-source migration guard coverage, cross-document consistency, and architectural coherence.

This independent review evaluates the Iteration 14 snapshot of the Core and Gastown Pack Split design (`plans/core-gastown-pack-migration/implementation-plan.md` / `attempt-14/design-before.md`) against `requirements.md` and the live codebase at the `rig_root` (`/data/projects/gascity`).

---

## Executive Summary

As Ingrid Kovac, the **ZFC and Role-Neutrality Guardian**, I am issuing a strict **Verdict: BLOCK** for Iteration 14 / Attempt 14 of the Core and Gastown Pack Split design.

Attempt 14 introduces highly valuable improvements, notably relaxing required provider pack (`bd` and `dolt`) byte immutability to permit explicit role-cleaning rewrites, and introducing tighter doctor mutation coordinators with OS-level concurrency locking. However, **the core blockers from previous reviews have not been integrated into the design text**. The design continues to carry forward systemic loopholes—specifically a blindspot in the role-token scanner's identifier-matching logic, un-enumerated hardcoded Go role surfaces, a lack of per-asset Core utility classification, and an active wire-level compatibility mismatch with the dashboard.

We cannot permit this design to proceed to implementation while these active risks threaten the Zero Framework Cognition (ZFC) invariant.

---

## Detailed Evaluation of the Three Key Questions

### 1. Does any Go change introduce role-conditional logic or a literal role name outside tests, migration docs, or pack configuration?
**No, but the target state design remains self-contradictory regarding pre-existing and permanent active Go literals.**
The design purports to establish absolute role neutrality in Go source code (§573–586). However, it introduces a severe taxonomy collision:
* **The Colliding Requirement:** The target-state `gc` command suite requires permanent, active `gastown` literals in production Go code—specifically, the template/init selection branches in `cmd_init.go` and the `PublicGastownPack*` version and dependency constants in `public_packs.go`.
* **The Self-Contradiction:** These literals cannot "expire" (they are permanent features of the initialization wizard and public pack pinning), yet they do not fit any of the four narrow allowed classes for role references (historical fixtures, docs examples, old-pack diagnostics, or Core worker compatibility).
* **The ZFC Risk:** This taxonomy blindspot forces a choice between two failures: either the allowlist must grow indefinitely to excuse convenient permanent violations (the "growing allowlist" anti-pattern), or the initialization wizard will be crippled. The design must resolve this by treating templates as purely dynamic, config-driven registry entries rather than hardcoded Go strings.

### 2. Does the Core role-name guard scan every asset type including scripts, overlays, orders, template fragments, doctor checks, metadata, and prompt snippets?
**Yes, but the scanner's scope is fundamentally conflated.**
The AST and plain-text token scanner described in §1910–1935 is designed to sweep all assets. However, it suffers from two critical integration flaws:
* **Conflated Scan Roots:** The design includes the "public Gastown checkout" within the scanner's direct search roots. Gastown is a configured pack of *roles* (Deacon, Witness, refinery, etc.). Treating the public Gastown checkout with the same zero-roles rules as Core means every legitimate Gastown-owned role reference would require an expiry date or compatibility allowlist row in Core. This is absurd and conflates "Core neutrality" with "external pack restriction."
* **Self-Collision on Manifests:** The scanner is configured to scan all markdown and YAML assets under `plans/` and reject role names (§1914–1918). Yet, the behavior-manifest and role-surface tables (`role-surface.generated.yaml`) *must* record the very roles they are tracking for deprecation (such as `mayor` or `deacon`). Without explicit, narrow file-set exemptions, the scanner will immediately fail on its own audit tables.

### 3. Can Core infrastructure still run when the default maintenance agent is removed or renamed by configuration?
**Theoretically yes, but the design introduces a dangerous unmodeled escape hatch and lacks negative test validation.**
The symbolic binding schema (`[gc.bindings.maintenance_worker]`) supports renamed and omitted workers (§1796–1811). However, two severe holes remain:
* **The `optional_for_controller` Escape Hatch:** The schema carries the boolean `optional_for_controller = true` (§1801) but defines no runtime semantics or state machine for when this is set to `false`. If an operator overrides it to `false` and omits the worker, does the controller block its own startup? If so, the controller becomes role-cognizant and violates SDK self-sufficiency.
* **Lack of Negative Assertions:** The proposed test suite (§1842–1846) asserts that renamed/omitted configurations "work," but does not mandate negative assertions. We must explicitly test and assert that when the worker is omitted, there are *zero* dispatch actions or event payloads routed to the old default name (`dog`), preventing silent fallback heuristics in the Go execution path.

---

## Top Strengths of Attempt 14

* **Symbolic Binding Model & Omitted-Worker Self-Sufficiency:** The `[gc.bindings.maintenance_worker]` symbolic binding model (lines 1796–1845) is mathematically sound. It completely decouples Go execution logic from concrete pool names and ensures Core can safely run controller-owned infrastructure without a worker.
* **Provider Pack Conformance (Attempt 14 Improvement):** Attempt 14 correctly resolves the required provider pack de-roling contradiction. By amending the required-pack continuity clause (lines 2665–2670) to authorize role-cleaning and target-binding byte rewrites on `bd` and `dolt` assets, the design ensures required host packs can run in Core-only environments without leakage.
* **Preflight Mutation Coordinator Security (Attempt 14 Improvement):** The new `doctor.MutationCoordinator` (lines 1936–1960) with OS-level concurrency locking and preflight validation prevents recovery tools from causing deadlocks, safely managing transitions for legacy local directories.

---

## Critical Risks & Remaining Gaps

### 1. [Blocker] Scanner Sub-Identifier Blindspot
* **The Risk:** The scanner is specified as tokenizing "identifiers and string literals" (§581-584). In standard Go tokenization, a camelCase or PascalCase identifier is treated as a single token.
* **The Gap:** A whole-token word match on role names (e.g. `\b(mayor|deacon|crew)\b`) will **silently fail to catch** role-keyed Go APIs such as `MayorTheme()`, `DeaconTheme()`, `ConfigureGasTownSession`, `isCrewDir`, or `defaultWarmupMailTo`. 
* **Required Change:** The scanner specification must explicitly require splitting camelCase/PascalCase sub-identifiers (or utilizing substring token scans against a strict allowlist) to enforce absolute role neutrality on Go APIs. Negative test fixtures must include camelCase role-bearing Go names to ensure this check fails CI.

### 2. [Blocker] Un-enumerated Go Role Violations & Self-Imposed Deadlock
* **The Risk:** The design gates source tree deletion (slice 7) and public Gastown pinning on resolving "Hardcoded role-theme/tmux APIs" (§2632). However, it never enumerates the pre-existing Go role violations in the tree.
* **The Gap:** The actual violations in the repository are:
  * `internal/runtime/tmux/theme.go:33,39,43`: `MayorTheme()`, `DeaconTheme()`, `DogTheme()` returning hardcoded role names.
  * `internal/runtime/tmux/tmux.go:80,2823`: `roleEmoji`/`roleIcons` maps containing role display keys (`mayor`, `deacon`, `witness`, `refinery`, `crew`, `polecat`).
  * `cmd/gc/cmd_start_warmup.go:33`: `defaultWarmupMailTo = "mayor"` hardcoded warmup fallback.
  * `internal/config/config.go:3671,3700`: `DefaultCity` and `WizardCity` hardcoding `{Name: "mayor", PromptTemplate: "prompts/mayor.md"}`.
  * `internal/sling/sling.go:888,894`: `SlingFormulaUsesBaseBranch` and `SlingFormulaUsesTargetBranch` heuristics branching on `mol-polecat-*` and `mol-refinery-patrol` formula names.
  * `cmd/gc/cmd_prompt.go:631-632`: CLI fallback defaulting to `prompts/mayor.md`.
* **Required Change:** Enumerate these Go violations explicitly in the design text. State their exact disposition (e.g., config-driven theming, dynamic lookup maps, registry-driven scaffolding) and the slice in which they are neutralized. This prevents developers from taking the path of least resistance by dumping them into a broad allowlist.

### 3. [Blocker] Core Asset Classification Missing (Codex Convergence)
* **The Risk:** The design states that omitting the Core maintenance worker disables "worker-bound Core maintenance orders and formulas" while ensuring "controller-owned SDK operations" still run (§1812–1818).
* **The Gap:** The design lists generic infrastructure orders and scripts such as gate sweep, blocker-close cascade, stale cleanup/reaper, spawn storm detection, and binary doctor checks (§2649). However, it never classifies which assets are controller-owned vs. worker-bound. Without a per-asset classification table, an implementation can easily break controller self-sufficiency by assigning a required SDK operation to the optional `core.maintenance_worker` binding.
* **Required Change:** Extend the behavior manifest or role-surface table with a mandatory per-asset classification: `controller_owned`, `optional_core_maintenance_worker`, `public_gastown`, or `retired`. Mandate negative tests showing that when `maintenance_worker = ""` is configured, all `controller_owned` operations execute successfully.

### 4. [Blocker] Dashboard `crew` Wire Vocabulary Contradiction (Claude/Codex Convergence)
* **The Risk:** To pass Go neutrality, wire-level fields like `crew` or `agent_kind` must be de-roled (§1929). Yet, the design asserts "this migration should not require dashboard changes" (§2720).
* **The Gap:** The live dashboard filters on `session.agent_kind === "crew"` (`cmd/gc/dashboard/web/src/panels/crew.ts:45`) and reads the OpenAPI schema for `agent_kind` (`docs/schema/openapi.json`). Changing the Go wire vocab without modifying the dashboard will immediately break API compatibility.
* **Required Change:** Resolve this contradiction. Either authorize dashboard API updates in the rollout plan (including running `make dashboard-check`), or formally allowlist the specific Go wire/JSON struct tags required for backward compatibility with the current dashboard version.

---

## Required Changes for Finalization (Actionable Gates)

To lift this block, the design document must be updated to resolve these issues with the following concrete, actionable gates:

1. **Codify Go Scanner Token Rules:** Specify that the Go role-name scanner must split camelCase/PascalCase sub-identifiers. Mandate a negative fixture per role token in camelCase form that must fail CI.
2. **Pre-enumerate Go Role Surfaces:** Add explicit, named rows in the `Cross-Pack Ownership Decisions` or role-surface table for `MayorTheme`, `DeaconTheme`, `roleEmoji`, `defaultWarmupMailTo`, `DefaultCity` / `WizardCity` scaffolding, and `internal/sling` heuristics, documenting their final disposition.
3. **Classify Core Assets:** Provide an exhaustive, per-asset table classifying moved/retired Maintenance assets into `controller_owned` vs. `optional_core_maintenance_worker`. Require omitted-worker negative tests for all `controller_owned` assets.
4. **Harmonize Dashboard Wire Types:** Resolve the `crew` wire-type conflict by either detailing the required dashboard compatibility allowlist or authorizing the exact dashboard/OpenAPI updates under a validated gate.
5. **Contain `dog` Literals:** Mandate that the scanner rejects the token `dog` in all Core-owned routing, pool, and dispatch metadata fields (requiring symbolic bindings instead).

---

## Questions

* **Sub-identifier Tokenization:** Will the Go scanner split sub-identifiers (camelCase/PascalCase) and assert on negative camelCase role fixtures?
* **Go Role Dispositions:** What are the exact structural replacement mechanisms for `roleEmoji`, `DefaultCity` / `WizardCity` scaffolding, and `internal/sling` branch heuristics?
* **Core Classification:** Which of the generic maintenance scripts listed in line 2146 are required SDK controller-owned infrastructure versus optional worker-bound tasks?
* **Dashboard Wire Alignment:** Will the dashboard API be updated to support the new neutral grouping metadata, or will we carry a narrow, expiring wire compatibility row for `crew`?
