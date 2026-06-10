# Yuki Hayashi — Rollout and Version Skew Perspective Independent Review (DeepSeek V4 Flash)

**Verdict:** BLOCK (Requires resolution of critical edge-case parser holes and version-skew definition gaps before merging)

**Scope:** Two-repo rollout sequencing, public pack pin integrity, intermediate state safety, and rollback/downgrade granularity.

This independent review evaluates the draft of `design.md` (specifically §2949-3033, §3983-4118) from the perspective of **Yuki Hayashi (Rollout and Version Skew Planner)**.

---

## Executive Summary

The proposed 7-slice rollout design (§3983–4096) is highly sophisticated and successfully avoids traditional "flag-day" deployment risks by decoupling the release of `gascity-packs` (Slice 1) from the adoption of the public pins in Gas City (Slices 2, 5, 6). The non-destructive startup policy (§2975) that preserves stale legacy directories is excellent and prevents catastrophic data loss.

However, a rigorous analysis from the DeepSeek V4 Flash perspective reveals **two critical blockers** and several major risks that compromise the fail-closed guarantees of the migration. Specifically, the design relies on an inactive-loader parser validation that does not cover the formula asset type, and it contains a logical flaw where post-activation binaries could run stale pins with warning-only loading, leading to silent, critical maintenance failures.

This review blocks the current design draft and provides exact, executable remedies that must be incorporated into the design document before proceeding with implementation.

---

## Evaluation of the Three Key Questions

### 1. What does a fresh `gc init --template gastown` produce between the `gascity-packs` landing and the Gas City `PublicGastownPackVersion` pin update, and is that state deployable?
- **Finding:** **Deployable and Stable.** During this transitional window, Gas City remains at the old commit pin (`current_baseline` in `public-gastown-pins.yaml`). Operators initializing new cities will resolve the bundled, in-tree Gastown template or synthetic cache alias. Since the in-tree assets are not purged until Slice 7 (§4084), this state is fully functional and deployable. It perfectly maintains the status quo without pre-maturely activating unpinned public behaviors.

### 2. Is `PublicGastownPackVersion` pinned to immutable content with materialization-time verification rather than a mutable branch or tag?
- **Finding:** **Yes, but with an offline verification gap.** The pin is explicitly defined as an immutable SHA commit hash (§11, §38). Materialization-time verification is performed via a unified `RepoCacheKey` composed of the normalized source, immutable version, and subpath (§3015). However, for offline/air-gapped environments utilizing the synthetic-cache promotion helper (§3008), the design fails to specify a hard offline trust anchor to verify the synthetic bytes against the pinned commit digest, introducing a risk of untrusted cache promotion.

### 3. Can Gas City registry changes be reverted after operators fetched the new public pack without leaving cities with neither Maintenance nor Gastown behavior?
- **Finding:** **No, not without manual operator intervention (Silent Capability Loss).** If an operator upgrades to a post-activation binary, the `gc doctor --fix` routine will remove the legacy `maintenance` import from `city.toml` (§4125). If the operator subsequently downgrades to an older binary (e.g., due to an incident), the old binary will load the city successfully but will run with *zero* maintenance processes (since the old binary expects the `maintenance` import to run maintenance, and does not have maintenance folded into its Core layer). This is a silent capability degradation that violates our safety invariants.

---

## Critical Risks & Deep-Dive Findings

### 1. [BLOCKER] The Ignored-Formula Parser Hole (Formula-Level Undecoded TOML Bypass)
The design asserts that the activation pin is unsupported by older binaries (`v1.2.1`) because activation assets carry `target_binding`/`gc.run_target_binding` keys which older binaries will reject as fatal undecoded TOML via `fatalUndecodedWarnings` (§2970–2975, §4112–4113).
- **The Gap:** This citation only applies to config/pack-level TOML files resolved by `internal/config`. In the actual `v1.2.1` codebase, formula parsing (resolved under `internal/formula/parser.go`) uses `toml.Unmarshal` without checking undecoded metadata, and the `Step` struct has no `target_binding` or `run_target_binding` fields.
- **The Impact:** Old binaries running against the activation public pack will silently unmarshal the formula files, ignore the unknown `target_binding` keys, and proceed with behavior execution. The expected "fail-closed before behavior discovery" behavior is bypassed entirely, leading to silent, unpredictable execution errors and state corruption in the field.

### 2. [BLOCKER] Stale-Pin "Warn-and-Load" Vulnerability under Post-Activation Binaries
The canonical version-skew rule states that any locked public Gastown commit equal to any pin-ledger row (including `current_baseline` and `compatibility`) loads normally with at most a warning (§2956–2960). The release compatibility matrix collapses this under "new binary | old pack" with normal load for ledger-listed commits (§4114–4115).
- **The Gap:** A post-activation binary (Slice 5+) has completely removed the Maintenance pack from its required system packs (§4058). If such a binary loads a city locked to the `current_baseline` or `compatibility` pin, those older packs do not carry the migrated maintenance behaviors, and the post-activation binary no longer bundles them.
- **The Impact:** The city will load successfully but will run with *neither* Core maintenance nor Gastown-specific maintenance. This creates the exact silent missing-maintenance behavior gap that the migration is designed to prevent.

### 3. [MAJOR] Concurrent Materialization of Shared Public Cache (Global Lock Gap)
The mutation coordinator uses a city-local lock (`.gc/controller.lock`) to serialize doctor fixes. However, the ordinary remote cache for public packs (e.g., `~/.gc/cache/` or similar) is shared globally across multiple cities on the same host machine.
- **The Gap:** If two separate cities on the same machine are initialized or updated concurrently (e.g., during parallel task execution or concurrent CI runs), they will both attempt to resolve, download, and materialize the same `PublicGastownPackVersion` to the shared cache folder.
- **The Impact:** In the absence of a global, cross-process cache-write lock, this concurrency creates a race condition that can result in a corrupted, partially materialized public pack directory, causing subsequent loads to fail closed.

### 4. [MAJOR] Missing Offline Trust Anchor for Synthetic-Cache Promotion
The design allows offline operators to promote a legacy synthetic public Gastown cache into the ordinary remote cache using a digest-validated helper (§3008–3014).
- **The Gap:** On an air-gapped host, there is no network connection to fetch and verify the content digest of the pinned commit. If the offline promotion helper only validates the cache against its own self-computed hash, there is no cryptographically secure chain of custody ensuring that the promoted bytes actually match the true git commit's subpath.
- **The Impact:** A corrupted or tampered local cache could be promoted to a "validated" remote cache entry, bypassing the content-verification checks.

### 5. [MINOR] Mutable Reachability vs Immutable Content
Each ledger row records a "durable public ref" (§30), but there is no explicit requirement for the `gascity-packs` repository to enforce branch protection, tags, or releases for the pinned commits.
- **The Gap:** While the content is immutable via SHA-1 hashes, the reachability of that content is mutable. If a developer force-pushes `main` or deletes a branch on `gascity-packs` before the pin is consumed in Slice 5, the commit could be garbage-collected by GitHub, causing fresh inits and cache-evicted cities in the field to fail network resolution.

---

## Required Changes & Actionable Recommendations

To transition this review to an approval, the following modifications must be made to the design document:

### 1. Fix the Old-Binary Fail-Closed Mechanism (Formula Level)
- **Change:** Do not rely on `fatalUndecodedWarnings` on formula assets. Add a pack-level minimum schema or binary version constraint field (e.g., `minimum_gc_binary = "v1.3.0"`) inside the `pack.toml` file of the activation pack.
- **Requirement:** Add an executable gate in Slice 1 that explicitly runs a compiled `v1.2.1` binary against the actual activation checkout to prove that it fails-closed immediately on load, and document this execution transcript in the ledger.

### 2. Implement a Phase-Aware Compatibility Matrix
- **Change:** Rewrite the canonical version-skew rule (§2956–2960) to be phase-aware. Replace "any ledger row loads normally" with a structured mapping where each ledger row is mapped to its supported loader phases:

| Gas City Binary Family | `current_baseline` | `compatibility` | `activation` |
| --- | --- | --- | --- |
| **Old Binary (`v1.2.1`)** | Supported (Warn-free) | Supported (Warn-only) | Unsupported (Fatal) |
| **Compatibility-Era Binary** | Supported (Warn-only) | Supported (Warn-free) | Unsupported (Fatal) |
| **Post-Activation Binary** | Unsupported (Fatal) | Unsupported (Fatal) | Supported (Warn-free) |

- **Requirement:** Ensure that if a post-activation binary encounters a city locked to `current_baseline` or `compatibility`, it fails-closed immediately with diagnostic `public_gastown.pin_skew` and error-severity doctor output.

### 3. Add Downgrade Guidance and Old-Binary Warning
- **Change:** Update the rollback guidance (§4117) to explicitly state:
  > [!IMPORTANT]
  > "If a city was doctor-fixed under a post-activation binary (removing the `maintenance` import) and is subsequently rolled back to a pre-activation binary, the operator must manually restore the `maintenance` import to `city.toml` to prevent silent loss of maintenance execution."
- **Requirement:** Ensure this warning is output by the doctor tool of the pre-activation tree if it detects a city with no maintenance import.

### 4. Implement a Global Cache Write Lock
- **Change:** Add a requirement in the Registry and Cache section (§2987) mandating that any download, extraction, or materialization of a remote pack into the shared global cache directory must obtain a global, cross-process file-level lock (e.g., via `flock` or equivalent) on the cache directory.

### 5. Define the Offline Trust Anchor
- **Change:** Explicitly define the expected content digest of the pinned commit as a binary constant beside `PublicGastownPackVersion` in `internal/config/public_packs.go` (e.g., `PublicGastownPackDigest`).
- **Requirement:** The offline promotion helper must validate the synthetic cache's files against this hardcoded digest before promoting the cache, guaranteeing a cryptographically secure offline chain of custody.

### 6. Protect Durable Refs on Gascity-Packs
- **Change:** Mandate that for every commit SHA recorded as a pin in `public-gastown-pins.yaml`, a corresponding immutable git tag (e.g., `v1.0.0-compat` or similar) must be created and protected in the `gascity-packs` repository to prevent deletion or garbage collection.
- **Requirement:** Add a live network-reachability check of these tags to the Slice 5 and Slice 6 gate lists.
