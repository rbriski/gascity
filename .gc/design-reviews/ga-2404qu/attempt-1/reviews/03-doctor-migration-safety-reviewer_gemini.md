# Sofia Khoury — DeepSeek V4 Flash Independent Review (Iteration 21 / Attempt 21)

**Verdict:** BLOCK

**Lane:** doctor fix idempotency, legacy import rewrites, custom data preservation, operator-safe diagnostics.

Reviewed strictly against the Iteration 21 / Attempt 21 draft of `design-before.md` and `requirements.md` in the active repository workspace.

---

## Executive Summary

The Iteration 21 draft of the Core/Gastown pack split migration design integrates significant progress over previous iterations—notably around structural ignores, CST-preserving TOML edits, and the OS advisory lock. However, under close inspection, several critical safety-critical design-level conflicts and architectural contradictions remain.

Most notably, there is a **direct structural contradiction** between the read-only mandate of the plain `gc doctor` diagnostics and the materializing loader invoked by the new `Core Presence Doctor` check. Additionally, the concurrency safety model relies on an advisory lock on the city directory that the live daemon/controller does not respect, and the idempotency model allows for redundant write operations that trigger file-system watcher reload storms.

Consequently, my verdict remains a firm **BLOCK** until these safety boundaries are rigorously aligned and codified.

---

## Top Strengths

1. **Structured Runtime-State Migration Binding Table (§2564–2574):**
   The binding table elegantly separates the legacy read paths, Core write paths, and rollback paths for JSONL files and spawn-storm ledgers, ensuring that transition boundaries are crisp and manual-only until migration markers are cleanly verified.
2. **CST-Preserving TOML Editor Integration (§3132–3134):**
   Requiring TOML edits to be localized and CST-based, rather than whole-file serialization, preserves formatting comments and layout. This is crucial for avoiding silent loss of custom operator changes.
3. **Retired-Source Classifier and Custom Fork Refusal (§2577–2579):**
   Classifying files with non-standard digests as "custom forks" and redirecting them to manual operator-safe diagnostics is a strong defense-in-depth policy.

---

## Fix Validations

We validated the proposed `gc doctor --fix` and Core Presence Doctor against the active implementation design and found severe safety and consistency gaps. Specifically, the proposed Core Pack check directly breaks the read-only boundary of plain `gc doctor` by invoking a materializing loader, which performs writes, prunes, and quarantines.

---

## Findings

### [Pattern Drift / Architectural Coherence] Contradiction on Plain-Doctor Read-Onliness
- **Severity:** blocker
- **Confidence:** high
- **Quality dimension:** correctness
- **Gate impact:** blocker
- **Evidence:** `.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`:2109-2122, :3061-3062
- **Pattern comparison:** The `Read-Only Doctor Diagnostic Boundary` (§2106–2122) explicitly mandates that plain `gc doctor` is strictly report-only and "must not call materializing runtime loaders, ... repair helpers, ... or quarantine/prune writers." It permits only non-refresh API calls.
- **Why it matters:** The new `Core Presence Doctor` check (§3050–3068) violates this invariant by instructing the check to load the config through `internal/systempacks.LoadRuntimeCity` (line 3061), which is the materializing/writing entrypoint that performs on-disk repairs and quarantines.
- **Suggested fix:** Change the Core Presence Doctor check instruction in §3061 to strictly call `internal/systempacks.LoadRuntimeCityNoRefresh` and check file sets via `ValidateRequiredFileSetsNoRefresh`, leaving all materialization and repair exclusively to `gc doctor --fix` under the `doctor.MutationCoordinator`.

### [Architectural Coherence / Concurrency] The Concurrency Lock Illusion
- **Severity:** blocker
- **Confidence:** high
- **Quality dimension:** security
- **Gate impact:** blocker
- **Evidence:** `.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`:2555-2562, :3108-3111
- **Pattern comparison:** The design relies on the `MutationCoordinator` taking an OS advisory lock on the city directory during preflight and publish to serialize mutations.
- **Why it matters:** The active daemon/controller (`gc start` / daemon) does not participate in or respect this city-directory advisory lock when reading or reloading configurations. This leaves a severe TOCTOU (Time-of-Check to Time-of-Use) window where the daemon can read torn, half-written configurations or write state concurrently during doctor execution, causing silent configuration corruption or daemon crashes.
- **Suggested fix:** Mandate a two-tier locking model where the daemon takes a shared lock (SH) during config loading/reloads, and the `MutationCoordinator` takes an exclusive lock (EX) before staging any edits, or require the coordinator to verify via live process table scans that no controller process is active before executing any writes.

### [Correctness / Idempotency] False Idempotency & Filesystem Watcher Thrashing
- **Severity:** major
- **Confidence:** high
- **Quality dimension:** correctness
- **Gate impact:** major
- **Evidence:** `.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`:3120, :3129-3131
- **Pattern comparison:** The design specifies that "staged writes plus temp-file-plus-rename" (§3120) must be executed to ensure the final file state is byte-identical.
- **Why it matters:** Overwriting an already healthy and byte-identical file via `write` and `rename` syscalls updates the file's modification time (`mtime`) and inode number on the disk. This thrashes filesystem watchers (`fsnotify`), triggering endless cascade reloads, daemon restarts, and active session terminations in production.
- **Suggested fix:** Force the `MutationCoordinator` to perform a preflight digest compare-and-bypass. If the target files are already healthy and byte-identical, skip all write/rename system calls entirely (zero-write bypass), keeping `mtime` and inodes untouched.

### [Backward Compatibility] Legacy Version Skew & Rollback Risk
- **Severity:** major
- **Confidence:** high
- **Quality dimension:** maintainability
- **Gate impact:** major
- **Evidence:** `.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`:2234, :3147-3149, :3469
- **Pattern comparison:** The compatibility matrix covers forward version skew but fails to specify safe failure modes for reverse version skew.
- **Why it matters:** If an operator accidentally executes an older `gc doctor --fix` (from binary `v1.2.1` or older) against a city already migrated by the new binary, the legacy code has no knowledge of public pins or migration markers. It will view the public pins as invalid local imports and blindly rewrite or strip `city.toml` using its legacy whole-file re-encoder, destroying comments, formatting, and the migrated structure.
- **Suggested fix:** Add a specialized comment marker or metadata field to `city.toml` that forces older binaries to fail-fast on load/fix with a clear "unsupported configuration version" error, rather than silently mutating and corrupting the file.

### [Custom Data Preservation / Edge Case] Silent Removal of User-Authored Import Edges
- **Severity:** major
- **Confidence:** high
- **Quality dimension:** correctness
- **Gate impact:** major
- **Evidence:** `.gc/design-reviews/ga-2404qu/attempt-21/design-before.md`:3091-3097
- **Pattern comparison:** Maintenance imports are auto-removed "when the source is `.gc/system/packs/maintenance` or `examples/gastown/packs/maintenance`" (§3091); redundant Core imports are removed when they point at generated/legacy Core paths (§3094–3097).
- **Why it matters:** While the generated-vs-custom discriminator (§2577–2578) classifies *pack directories*, an `[[imports.*]]` table in `city.toml` or rig `pack.toml` carries no generation marker of its own. If an operator has manually added an import pointing to a path that happens to match a legacy/system pattern (for custom tracking or local overrides), the doctor will silently remove or rewrite that import edge because it lacks a way to distinguish manual import entries from generated ones.
- **Suggested fix:** Require the doctor to inspect a comment-based annotation (e.g. `# gc:preserve`) or explicit whitelist metadata before removing/rewriting any import edge. If present, the doctor must skip the mutation and issue an operator-safe warning.

---

## Evaluation of Sofia's Critical Questions

### 1. Is the Core presence doctor fix a proven no-op on a healthy city, including repeated or concurrent runs with a controller active?
**No.** As currently drafted, even when the city is completely healthy, the `MutationCoordinator` will stage writes and perform renames, updating file modification times (`mtime`) and inodes. This is not a filesystem no-op and will trigger filesystem-watcher storms in active controllers. Furthermore, because active controllers do not participate in the advisory directory lock, concurrent runs remain highly vulnerable to TOCTOU read-tear windows during the publish phase.

### 2. When `gc doctor --fix` removes redundant Core or legacy Maintenance imports, what prevents it from deleting user-added imports or custom pack edits?
**The CST-preserving TOML editor and the Retired-Source Classifier (§2444, §2577–2579).** If the files have non-standard digests or custom edits, the classifier tags them as "custom local forks." The coordinator explicitly blocks automated mutations on custom forks, reporting manual diagnostics instead. However, without comment and whitespace normalization, minor non-semantic formatting edits (like trailing newlines) will trigger false "custom fork" detections, locking operators out of automatic fixes. Additionally, manual import edges pointing to legacy patterns lack a tracking mechanism and are vulnerable to silent erasure.

### 3. If a local Gastown import is rewritten to a public remote, does the fix verify reachability and immutable provenance or fail with explicit operator guidance?
**Yes, but the ordering is unsafe.** While preflight checks and `public-gastown-pins.yaml` verify immutable digests and reachability, the design lacks an explicit "read-only preflight gate." If the preflight checks run concurrently or after file edits have begun, a network or cache failure halfway through will leave the configuration in a partially mutated, corrupted state. The preflight phase must be executed as a strict, read-only transaction before any file modification begins.

---

## Required Changes for Finalization

1. **Enforce Read-Only Boundaries on Core Presence Doctor check:** Update §3061 to mandate that `cmd/gc/core_pack_doctor_check.go` loads the config strictly via `LoadRuntimeCityNoRefresh` and validates via `ValidateRequiredFileSetsNoRefresh`, ensuring that the report-only `gc doctor` command never triggers materialization or prune side effects.
2. **Implement Zero-Write Bypass for Idempotency:** Update §3120 to require the `MutationCoordinator` to perform digest-comparisons of candidate files against active disk files and completely bypass write/rename syscalls if they are byte-identical.
3. **Harden Concurrency Protection:** Update §3108 to require that both active controllers and the `MutationCoordinator` participate in a shared/exclusive advisory directory lock protocol, or that the coordinator performs strict live process table scans to block edits if a daemon is running.
4. **Define Reverse Version-Skew Fail-Fast:** Update §3147 to add a specialized fail-fast marker in migrated `city.toml` files to prevent legacy doctor fixes from corrupting the configuration.
5. **Protect User-Authored Import Edges:** Update §3091 to mandate that hand-authored imports containing preservation comments (e.g. `# gc:preserve`) are skipped by automatic doctor removal and flagged for manual confirmation.

---

## Consistency Report
- **Patterns checked:**
  - Read-Only Doctor Diagnostic Boundary (§2106–2122)
  - Core Presence Doctor Check (§3050–3068)
  - Doctor Mutation and Runtime-State Safety (§2547–2574)
  - Doctor Fix Safety Contract (§3103–3149)
- **Sibling files checked:**
  - `requirements.md` (Design-review inputs)
  - `03-doctor-migration-safety-reviewer_gemini.md` (Attempt 20)
- **Propagation verified:**
  - Handled the alignment of the `MutationCoordinator` and read-only diagnostic boundaries across the design document.
- **Drift detected:**
  - Detected direct drift/contradiction between the read-only boundary of plain `gc doctor` and the materializing config load inside the Core Presence Doctor check.

---

**Sofia Khoury blocks the design pending these critical safety modifications.**
