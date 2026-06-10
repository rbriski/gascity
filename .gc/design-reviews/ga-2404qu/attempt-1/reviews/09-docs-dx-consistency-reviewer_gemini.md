# Felix Moreau — Docs & DX Consistency Reviewer Perspective Independent Review (DeepSeek V4 Flash)

**Verdict:** BLOCK (Requires resolution of critical, operator-visible documentation gaps, dead links, and factual baseline mismatches before merging)

**Scope:** Terminology consistency (Core, public Gastown, retired Maintenance), operator terminology, tutorial integrity, and maintenance-word disambiguation.

This independent review evaluates the Iteration 22 draft of `design.md` (`.gc/design-review-inputs/core-gastown-pack-migration/design.md`) against the active repository checkout (`fix/required-artifact-store-errors-ga-ksno8` / `HEAD`) and the system's runtime configuration behaviors.

---

## Executive Summary

The Iteration 22 draft of the design represents an exceptionally sophisticated evolution. Transitioning the terminology contract into an automated System-Pack Wording Matrix (`system-pack-wording.generated.yaml`) with bidirectional linter enforcement and goldens in CI is a massive triumph for developer experience (DX). Furthermore, standardizing on **"maintenance worker"** as the primary operator noun and **"maintenance_worker"** as the configuration binding key surgically resolves prior-iteration terminology clashes.

However, a rigorous analysis from the DeepSeek V4 Flash perspective reveals **two critical blockers** and several major risks. Specifically, the design asserts a factual falsehood regarding the current existence and navigation registration of `docs/reference/system-packs.md` in this branch, creating a silent mis-scoping of documentation deliverables. Additionally, the design's named update lists leave major operator-facing files (including troubleshooting walkthroughs, CLI references, onboarding guides, and generated schemas) completely out of scope, guaranteeing that dead links and copy-paste traps containing retired local-pack paths will escape to production.

Accordingly, my verdict is a firm **BLOCK**. This review details these gaps and provides exact, executable remedies that must be incorporated into the design before implementation.

---

## Evaluation of the Three Key Questions

### 1. Do doctor output, import-state messages, docs, pack comments, and tutorials use the same language for Core, public Gastown, and retired Maintenance?
- **Finding:** **No. While the wording matrix provides an enforcing scaffold, the design contains a material baseline error regarding its canonical reference document.** 
- **The Gap:** The design claims in §3838–3843 that `docs/reference/system-packs.md` already exists, is registered in `docs/docs.json`, and only requires a content update. In reality, on this active target branch, `docs/reference/system-packs.md` **does not exist**, and there is no entry for it in the `docs/docs.json` navigation array. By assuming the file is already there, the design fails to scope its creation and nav registration, leaving the canonical anchor of the entire terminology system without a home.
- **Why it matters:** If the file does not exist, the documentation slice will fail low-level navigation or golden-reference check tests, halting CI on the very first behavior-changing slice.

### 2. Can a new operator complete tutorial 01 and troubleshooting flows without encountering retired local pack paths or contradictory Maintenance guidance?
- **Finding:** **No. Operators are guaranteed to encounter broken walkthroughs, 404 links, and copy-paste traps because high-priority documents are omitted from the named update lists.**
- **The Gap:** The design explicitly names only `docs/reference/system-packs.md`, `docs/guides/shareable-packs.md`, `docs/getting-started/troubleshooting.md`, and `docs/tutorials/01-cities-and-rigs.md` in its update scope (§3836–3852). It leaves the following operator-facing files untouched:
  1. `docs/getting-started/coming-from-gastown.md:545` – Links directly to `examples/gastown/packs/gastown/pack.toml` on GitHub `main`. Once Slice 7 deletes the in-tree Gastown source directory, this link will immediately **404**.
  2. `docs/troubleshooting/gc-start-walkthrough.mdx` – Instructs operators to resolve agent errors by adding `includes = ["packs/gastown"]` (line 262) and describes duplicate definitions as originating from `.gc/system/packs/gastown/` (lines 134–135). Both of these are retired/removed paths.
  3. `docs/getting-started/troubleshooting.md:275` – Instructs operators to check `packs/maintenance/jsonl-archive` inside the state directory, which is retired.
  4. `docs/reference/cli.md:2454,2456` – Provides examples of `gc rig add --include packs/gastown`, which will fail once local pack sources are deleted.
- **Why it matters:** These files are highly visible onboarding surfaces. Leaving them un-updated creates an immediate, highly frustrating degradation of operator DX on day one.

### 3. Do docs preserve store-maintenance or Dolt maintenance terminology only where it still refers to those systems rather than the retired Maintenance pack?
- **Finding:** **Partially. The principle is protected in prose, but a major generated schema contract remains unaddressed.**
- **The Gap:** The design correctly carves out `[maintenance.dolt]` and `gc maintenance` as store-maintenance exceptions (§3830). However, `docs/schema/pack-schema.json:872` (a generated schema artifact that serves as the API contract for pack config) teaches `"../maintenance"` as the canonical example for the `includes` field:
  `"description": "Includes lists other packs to compose into this one... Each entry is a local relative path (e.g. \"../maintenance\")"`
- **Why it matters:** Since the design's inventory scanner focuses on source files, generated JSON/TXT schemas are missed. Teaching a retired import path as the primary example in our schema definition directly undermines the migration's objective.

---

## Critical Risks & Deep-Dive Findings

### 1. [BLOCKER] The `system-packs.md` Non-Existence Contradiction
The design treats `docs/reference/system-packs.md` as an existing page requiring a simple content update (§3840–3843).
- **The Evidence:** A search of the active checkout directory proves the file does not exist under `docs/reference/` (which only contains `index.md`, `cli.md`, `config.md`, `formula.md`, `trust-boundaries.md`, `api.md`, `events.md`, `exec-session-provider.md`, and `exec-beads-provider.md`). Line 145 of `docs/docs.json` confirms it has no nav registration.
- **The Impact:** Treating this as an edit rather than a creation mis-scopes the work. The documentation slice will fail to find the file or nav entry, breaking golden-test and navigation-index check gates.

### 2. [BLOCKER] The `shareable-packs.md` Copy-Paste Trap
The design only scopes removing the "core and maintenance stay implicit" text from `docs/guides/shareable-packs.md` (§3844).
- **The Evidence:** `docs/guides/shareable-packs.md:103-104` contains a live TOML copy-paste example:
  ```toml
  [imports.maintenance]
  source = "../maintenance"
  ```
- **The Impact:** This example is a copy-paste trap. If an operator copies this into a pack config post-migration, it will fail to resolve because `../maintenance` is deleted. The guide must use a valid public pack (e.g., `[imports.gastown]`) as its canonical example.

### 3. [MAJOR] Walkthrough Instruction & Path Obsolescence
`docs/troubleshooting/gc-start-walkthrough.mdx` instructs operators facing errors to add a retired in-tree include.
- **The Evidence:** Lines 262 (`includes = ["packs/gastown"]`) and 134–135 (`.gc/system/packs/gastown/...`) teach obsolete local paths and retired system-pack behaviors.
- **The Impact:** Because the wording linter is extension-agnostic, these references might trigger a lint error, but a simple token patch cannot fix the underlying issue. The prose instructions are wrong end-to-end and must be rewritten to point to the public import syntax.

### 4. [MAJOR] CLI Reference & Onboarding Guide 404s
The design's explicit docs update list does not include `coming-from-gastown.md` or `cli.md`.
- **The Evidence:**
  - `docs/getting-started/coming-from-gastown.md:545` contains a live link to `examples/gastown/packs/gastown/pack.toml` which is slated for deletion.
  - `docs/reference/cli.md:2454,2456` teaches `gc rig add --include packs/gastown`.
- **The Impact:** Once in-tree Gastown sources are purged, these links and commands will fail, producing dead ends for new operators.

---

## Required Changes & Actionable Recommendations

To transition this review to an approval, the following modifications must be made to the design document:

### 1. Re-Scope the `system-packs.md` Deliverable
- **Change:** Rewrite §3838–3843 to explicitly frame `docs/reference/system-packs.md` as a **new document creation** rather than an edit.
- **Requirement:** Explicitly mandate that the documentation slice must add the corresponding page registration to `docs/docs.json` under the "Reference" group.

### 2. Add Onboarding/Troubleshooting Files to the Named Update List
- **Change:** Add the following files explicitly to the "Update docs and generated references" section:
  1. `docs/getting-started/coming-from-gastown.md` – Require updating line 545 to point to the public `gascity-packs/gastown` repository instead of the deleted in-tree path.
  2. `docs/troubleshooting/gc-start-walkthrough.mdx` – Require rewriting the duplicate agent definition examples (lines 134–135) and includes instruction (line 262) to use the public pack import syntax.
  3. `docs/reference/cli.md` – Require updating lines 2454 & 2456 to show the public import syntax rather than the local relative include.
  4. `docs/schema/pack-schema.json` – Require updating the Go doc-comment source of the `includes` description to replace the legacy `../maintenance` example with a valid public-source pattern.

### 3. Replace the `shareable-packs.md` Import Example
- **Change:** Expand §3844 to require replacing the `[imports.maintenance]` code snippet at `docs/guides/shareable-packs.md:103-104` with a valid, non-retired import example (such as `[imports.gastown]` pointing to its public source URL).

### 4. Refine the `troubleshooting.md` Fallback Guidance
- **Change:** Expand §3846 to specify the exact disposition of the `packs/maintenance` command on line 275. It must be updated to `packs/core/jsonl-archive` or whatever primary state destination is chosen in the runtime-state migration table.

---

## Questions for Finalization

- **Does the wording matrix check generated JSON/TXT schemas?** If not, what mechanism ensures that future edits to Go doc comments (which generate fields in `pack-schema.json`) do not re-introduce retired terminology?
- **Is `gc-start-walkthrough.mdx` fully covered by the wording-linter golden test suite?** Ensuring it is a hard CI block will prevent future regression.
