# Ingrid Kovac — ZFC & Role-Neutrality Guardian Perspective Independent Review (Iteration 20 / Attempt 1)

**Verdict:** BLOCK

**Scope:** Zero hardcoded roles, Core role neutrality, `dog` exception containment, SDK self-sufficiency, Go-source migration guard coverage, cross-document consistency, and architectural coherence.

This independent review evaluates the Iteration 20 / Attempt 1 draft of the Core and Gastown Pack Split design (`.gc/design-review-inputs/core-gastown-pack-migration/design.md`) against `requirements.md` and the live codebase at the `rig_root` (`/data/projects/gascity`).

---

## Executive Summary

As Ingrid Kovac, the **ZFC and Role-Neutrality Guardian**, I am issuing a firm **Verdict: BLOCK** for Attempt 20's design specifications under Iteration 20.

While the current design snapshot introduces exceptional architectural advancements—specifically the default-deny loader inventory (§2408), the ownership-aware scanner (§2483), the `dog` containerization via symbolic bindings (§2456), and the robust doctor mutation coordinator (§2547)—it contains critical internal contradictions, un-dispositioned Go file surfaces, and self-imposed deadlocks that prevent clean implementation.

The design cannot be declared final or implementable until the following contradictions and loopholes are resolved:
1. **The `gastown` Init and Config Wiring Deadlock (§2492 vs §2506):** The scanner rejects "`gastown` template wiring ... unless a row allows them" (§2492-2493). Yet, the Go disposition table (§2506-2514) provides no such row. This creates an immediate self-imposed deadlock on Go source files like `cmd/gc/cmd_init.go` and `internal/config/config.go` which hardcode `"gastown"`.
2. **The Dashboard `crew` Wire Vocabulary Contradiction (§2509 vs §3370):** The role-surface table specifies de-roling the API/OpenAPI/dashboard `crew` vocabulary and regenerating TypeScript schemas (§2509). However, under Testing, §3370 asserts: *"This migration should not require dashboard changes."* If wire types are regenerated and the `crew` or `agent_kind` fields change, the dashboard UI code (consuming these fields) will break unless it is updated.
3. **Go-Source Comments inside Scanner Scope:** Comments are explicitly within the scanner's matching scope. However, live code files such as `internal/api/handler_agents.go` at line 553 explicitly mention `mayor, witness` as role examples. Unless these comments are updated, removed, or allowlisted, they will trigger fatal scanner failures under the new rule.
4. **Core Asset Classification Specifics:** The design fails to provide a clear, exhaustive classification of Core maintenance assets into `controller_owned` (required SDK infrastructure) or `optional_core_maintenance_worker` (worker-bound). Without this explicit mapping, required SDK controller logic can easily be assigned to the optional maintenance worker, violating SDK self-sufficiency.

---

## Top Strengths of Attempt 20

* **`dog` Containerization via Bindings (§2456):** The transition to the `[gc.bindings.maintenance_worker]` table with default `"dog"` successfully contains role logic in configuration. This ensures SDK self-sufficiency when renamed or omitted, and prevents `"dog"` from becoming a hardcoded Go special case.
* **Default-Deny Production Loader Inventory (§2408):** Mandating `plans/core-gastown-pack-migration/loader-inventory.generated.yaml` as the binding source for all production config reads, and requiring them to route through `LoadRuntimeCity` or `LoadRuntimeCityNoRefresh`, ensures unauthorized config-loading bypasses cannot slip in.
* **Doctor Mutation Coordinator (§2547):** Confining all file mutations to `gc doctor --fix` through a failure-atomic, transactional coordinator prevents stale or corrupted system-pack directories from leading to runtime degradation or silent failures.
* **Generated Artifact Contracts (§2580):** Requiring generated, checked-in, and freshness-tested YAML artifacts (such as `role-surface.generated.yaml` and `loader-inventory.generated.yaml`) before implementation beads move behavior ensures the migration is fully traceable and independent of VCS history.

---

## Critical Risks & Gaps (The Blockers)

### 1. The `gastown` Init and Config Wiring Deadlock
* **The Risk:** Go files contain hardcoded `"gastown"` template tokens and public pack references, which the scanner rejects as a "sub-identifier surface ... unless a row allows them" (§2492-2493).
* **The Gap:** The live tree hardcodes `"gastown"` in:
  - `cmd/gc/cmd_init.go:134` (`case "2", "gastown"`) and `:975` (`wiz.configName == "gastown"`)
  - `internal/config/config.go:3706-3738` (`GastownCity` hardcodes the `"gastown"` import and `DefaultRigImportOrder: []string{"gastown"}`)
  - `internal/config/public_packs.go:7,11` (`PublicGastownPackSource`/`PublicGastownPackVersion`)
  - `cmd/gc/import_state_doctor_check.go:141` (`case "gastown"`)
  
  The Go disposition table (§2506-2514) supplies no row allowing these files, and the rollout plan does not address this. This creates an immediate self-imposed deadlock on Go source files.
* **Recommendation:** Explicitly decide: Is the SDK permitted to have a structural carve-out for the canonical public `gascity-packs/gastown` identity and template token, or must template->source resolution become fully data/registry-driven? If carved out, add a row in `role-surface.generated.yaml` to authorize these specific lines.

### 2. Dashboard `crew` Wire Vocabulary Contradiction
* **The Risk:** Wire-level fields like `crew` or `agent_kind` must be de-roled (§2509). Yet, the design asserts "this migration should not require dashboard changes" (§3370).
* **The Gap:** The live dashboard filters on `session.agent_kind === "crew"` (`cmd/gc/dashboard/web/src/panels/crew.ts:45`) and reads the OpenAPI schema for `agent_kind` (`docs/schema/openapi.json`). Changing the Go wire vocabulary without modifying the dashboard will immediately break API compatibility.
* **Recommendation:** Resolve this contradiction. Either authorize dashboard API updates in the rollout plan (including running `make dashboard-check`), or formally allowlist the specific Go wire/JSON struct tags (like `crew` and `agent_kind` as neutral structural labels) required for backward compatibility with the current dashboard version.

### 3. Go-Source Comments inside Scanner Scope
* **The Risk:** The scanner is described as scanning "comments" and "prompt prose", rejecting sub-identifier surfaces of hardcoded roles (`mayor`, `witness`, etc.) unless a row allows them (§2489).
* **The Gap:** Active code files such as `internal/api/handler_agents.go` at line 553 contains the comment:
  `//   - "role" otherwise — a singleton agent (e.g. mayor, witness) that lives`
  This comment resides in Gas City Core Go code. Under Attempt 20's strict rules, this will trigger a fatal role-neutrality scanner violation when the Go files are scanned.
* **Recommendation:** Explicitly mandate in the design's Go migration plan that such comments must be updated or cleaned up to use neutral terms (e.g., "singleton agent" instead of "e.g. mayor, witness") prior to running the scanner.

### 4. Core Asset Classification Missing
* **The Risk:** Omitting the Core maintenance worker disables worker-bound tasks while leaving controller-owned operations active (§2468).
* **The Gap:** The design lists generic infrastructure orders and scripts such as gate sweep, blocker-close cascade, stale cleanup/reaper, spawn storm detection, and binary doctor checks. However, it never classifies which assets are controller-owned versus worker-bound. Without a per-asset classification table, an implementation can easily break controller self-sufficiency by assigning a required SDK operation to the optional `core.maintenance_worker` binding.
* **Recommendation:** Extend the behavior manifest or role-surface table with a mandatory per-asset classification: `controller_owned`, `optional_core_maintenance_worker`, `public_gastown`, or `retired`. Mandate negative tests showing that when `maintenance_worker = ""` is configured, all `controller_owned` operations execute successfully.

### 5. Tmux and Theme Disposition Imprecision
* **The Risk:** Theme and display helpers are role-named and hardcoded in Go, violating role-neutrality.
* **The Gap:** While §2508 requires replacing Go theme/icon helpers with config-driven display metadata, it doesn't specify that `MayorTheme`, `DeaconTheme`, and `DogTheme` (`theme.go:33-46`) have **no non-test callers** and are actually dead code that should be *deleted*, not wrapped.
* **Recommendation:** Update the Go migration table to specify the deletion of these dead theme functions and clarify that `roleEmoji` (`tmux.go:80`, consumed as `roleIcons` at `:2823`) must be replaced with config-supplied display metadata.

---

## Evaluation of the Three Key Questions

### 1. Does any Go change introduce role-conditional logic or a literal role name outside tests, migration docs, or pack configuration?
**No, but active Go code today still has these role names, which must be systematically removed.**
The design asserts that absolute role neutrality will govern all Go source code (§2484–2514). Active role-name violations exist in the production tree today:
* **The Tmux Special Themes (`internal/runtime/tmux/theme.go:33,39,43`):** `MayorTheme()`, `DeaconTheme()`, and `DogTheme()` return hardcoded role names.
* **The Tmux Status Format Map (`internal/runtime/tmux/tmux.go:80–89`):** `roleEmoji` explicitly maps `"mayor"`, `"deacon"`, `"witness"`, `"refinery"`, `"crew"`, and `"polecat"` to emojis.
* **Warmup Defaults (`cmd/gc/cmd_start_warmup.go:33`):** `defaultWarmupMailTo = "mayor"` is hardcoded.
* **Sling/Formula Heuristics (`internal/sling/sling.go:888`):** Conflates formulas by prefix matching (`mol-polecat-*` and `mol-refinery-patrol`), hardcoding framework cognition in the Go dispatch path.

The Go migration table correctly targets these for replacement/deletion, but the scanner must be configured to split camelCase/PascalCase sub-identifiers to ensure these are caught.

### 2. Does the Core role-name guard scan every asset type including scripts, overlays, orders, template fragments, doctor checks, metadata, and prompt snippets?
**Yes, but the scanner's matching rules must explicitly exclude the audit tables themselves.**
The scanner described in §2484–2518 correctly spans all asset types. However:
* **Audit Manifest Exception:** The role-surface table (`role-surface.generated.yaml`) and the behavior manifest (`behavior-manifest.generated.yaml`) *must* record literal role names for tracking purposes. The design must explicitly exempt these generated YAML metadata files from self-collision, otherwise the scanner will fail on its own audit tables.

### 3. Can Core infrastructure still run when the default maintenance agent is removed or renamed by configuration?
**Yes, but only if the boundary between controller-owned and worker-bound code is strictly enforced.**
The symbolic binding model (`[gc.bindings.maintenance_worker]`) is sound (§204–215). However, to prevent a regression where a developer writes a required controller-side SDK feature (like gate-sweeps or blocker-close cascades) to depend on the maintenance worker, a rigid per-asset classification and test asserting full functionality with an omitted worker are required.

---

## Required Changes for Finalization (Actionable Gates)

To lift this block, the design document must be updated to resolve these issues with the following concrete, actionable gates:

1. **Resolve Dashboard Wire Alignment:** Explicitly authorize the dashboard API and generated type updates in the rollout plan under a validated gate, or formally allowlist/preserve the exact JSON struct tags (like `crew` and `agent_kind` as neutral structural labels) required for backward compatibility with the current dashboard version.
2. **Clean/Neutralize Go Comments:** Mandate the cleanup of hardcoded role references in Core Go comments (such as `internal/api/handler_agents.go:553`) prior to running the role-neutrality scanner.
3. **Classify Core Assets:** Provide an exhaustive, per-asset table classifying moved/retired Maintenance assets into `controller_owned` vs. `optional_core_maintenance_worker`. Require omitted-worker negative tests for all `controller_owned` assets.
4. **Self-Referential & Pack Exemptions:** Formally exempt the generated metadata audit tables (`role-surface.generated.yaml`, `behavior-manifest.generated.yaml`) from role-name rejection checks. Clarify that public Gastown assets are scanned for preservation inventory, not role-name rejection.
5. **Decide `gastown` Init/Config Policy:** Resolve the template init/config deadlock. Explicitly state whether `gastown` is a sanctioned system-pack token with a documented structural carve-out, or make template->source resolution registry-driven, and add the corresponding scanner/disposition row.

---

## Questions for Clarification

* **Is the SDK permitted to hard-encode the canonical public Gastown identity (`PublicGastownPackSource`/`Version`, `gc init --template gastown`, `GastownCity`), or must template→source resolution become data-driven to satisfy ZERO hardcoded roles?** This is the core structural question that must be answered to clear the scanner deadlock.
* **Is `crew`/`agent_kind` a forbidden role name or an acceptable neutral structural grouping, and does the answer change the dashboard scope?** If it is an acceptable grouping, we can keep `classifyAgentKind` without changing the dashboard wire types.
* **When the maintenance worker is omitted in a Core-only city, is losing orphan-sweep / wisp-compaction / order-tracking an accepted operator tradeoff, or are any of those SDK infrastructure that must run controller-side regardless of the configured worker?**
