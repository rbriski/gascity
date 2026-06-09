# Petra Novak — DeepSeek V4 Flash (Embed, Materialization & Build Review)

**Verdict:** approve-with-risks

**Scope:** Builtinpacks registry, embed path migration, Maintenance retirement, and downstream reference safety.

Reviewed against the **current** requirements document (`plans/core-gastown-pack-migration/requirements.md` updated 2026-06-09T11:35:32Z, status: draft, Open Questions: None) and the live Go codebase.

---

## Executive Summary

As the **Embed, Materialization & Build Reviewer**, I am updating my verdict from **BLOCK** to **APPROVE-WITH-RISKS** for this requirements iteration. 

The previous blocking issues have been thoroughly and brilliantly addressed in this draft. Specifically, the introduction of **AC5's `source-consumer-closure.yaml`** successfully gates downstream reference closure for every retired-Maintenance, stale-Core, and in-tree-Gastown consumer script, test, fixture, and doctor state path. This ensures that the migration is verified by comprehensive "representative consumer checks with Maintenance absent," preventing runtime failures in first-party and provider scripts (such as Dolt and Gastown).

While the foundational reference closure is now gated, there are still critical, low-level build-safety and materialization risks that must be pinned in the requirements before moving to the implementation phase. These include:
1. **Symlink and Hardlink Portability:** Prohibiting the use of symlinks, hardlinks, or pointer files in both embedded and materialized pack layouts to ensure cross-platform compatibility and compile-time predictability.
2. **Atomic Materialization Semantics:** Requiring temporary directory staging plus atomic directory rename semantics for the embedded pack extraction path, guarding against directory corruption during crash or concurrency events.
3. **Core Relocation Ambiguity:** Resolving whether Core's source-tree pack location actually relocates, and ensuring that all 12 Core-pinned test, README, and markdown document reference paths are properly swept.

---

## Lane-Specific Detailed Responses

### Q1: When Core moves to its canonical embedded location, are all importers, embed.go files, registry entries, hook code, and generation commands updated together?

**Yes, with minor ambiguities.** The requirements now have the gates to ensure consistency, but the text is still slightly ambiguous regarding Core's target home and deprecation.
- **The Live State:** `internal/builtinpacks/registry.go:20` compile-imports `github.com/gastownhall/gascity/internal/bootstrap/packs/core`. Line 53 hardcodes the subpath `{Name: "core", Subpath: "internal/bootstrap/packs/core", FS: core.PackFS}`.
- **The Improvements:** AC5's `source-consumer-closure.yaml` and AC6's `asset-migration-ledger.yaml` require tracking all "stale-Core" and "all Core/Gastown/retired output paths" respectively. AC10 and AC11 cover diagnostic source attribution.
- **The Risk:** While the implementation plan defines the target path as `internal/packs/core`, the requirements document still lists `internal/bootstrap/packs/core` as legacy without stating whether Core relocates at all. We must clarify this intent in the requirements. Furthermore, there are **12 live test/readme/doc sites** hard-pinned to this path:
  - `registry_test.go:23,52-58,197,214,281`
  - `bundled_import_test.go:44,68`
  - `remotesource_test.go:16,18`
  - `prompt_test.go:781-782`
  - `hooks/config/README.md:18,59`
- **The Recommendation:** Define Core's canonical post-migration location, and ensure that AC13 requires a full stale-reference sweep covering these 12 Core-pinned test and markdown files so that stale path references do not rot silently.

### Q2: Does builtin materialization stop embedding and auto-including retired Maintenance while still materializing Core, bd, and dolt correctly?

**Yes.** The requirements document now includes extremely clean, explicit negative constraints on Maintenance.
- **The Live State:**
  - `cmd/gc/embed_builtin_packs.go:237` hardcodes `required := []string{"core", "maintenance"}`.
  - `cmd/gc/embed_builtin_packs.go:265` comments: `"Core and maintenance are always included."`
  - `internal/builtinpacks/registry.go:19` compile-imports `"github.com/gastownhall/gascity/examples/gastown/packs/maintenance"`.
  - `internal/builtinpacks/registry.go:56` lists `maintenance` in the `All()` slice.
  - `internal/builtinpacks/registry.go:128-129` hardcodes `case "gastown", "maintenance"` to return their public subpaths in `publicSubpathForPack`.
- **The Improvements:** AC5 explicitly mandates that Maintenance is neither bundled, public-source recognized, auto-included, materialized as an active system pack, selected by lock refresh, nor presented as an implicit dependency.
- **The Risk:** The Go-level bundling code and compilation imports (e.g., `registry.go` compile-imports, the `All()` list, and `embed_builtin_packs.go` required literals) are only *implicitly* covered under AC5's "test consumer" or "retired-Maintenance" categories. A leftover compile-time import will break compilation, which self-enforces, but leaving it un-gated can result in incomplete reference cleanups in tests (like `internal/runtime/k8s/provider_test.go` or `cmd/gc/order_dispatch_test.go`).
- **The Recommendation:** Make the bundled Go layer (`registry.go`, `embed_builtin_packs.go`, per-pack `embed.go`, and embedding tests) an explicit target of AC5, or add a compile-time negative gate ensuring the binary compiles with no embed/import referencing a retired pack directory.

### Q3: Are downstream references to moved Maintenance scripts repointed to Core homes without dangling paths?

**Yes.** This is the most significant improvement in this iteration. The addition of AC5's reference closure matrix elegantly solves the runtime coupling risk.
- **The Live State:** The script/doctor couplings previously raised have been confirmed:
  - `examples/dolt/assets/scripts/port_resolve.sh:6` coupling with `.gc/system/packs/maintenance/assets/scripts/dolt-target.sh`.
  - `status-line.sh:14-16` sourcing `_bd_trace.sh` from `packs/maintenance`.
  - `jsonl_archive_doctor_check.go:71,:97` resolving `packs/maintenance/jsonl-export-state.json`.
- **The Improvements:** **AC5 now explicitly mandates the `source-consumer-closure.yaml`**, which tracks every consumer class (scripts, configs, status-line tracing, doctor/runtime-state, test fixtures, and mock registries) with an owner, replacement or retirement decision, and proof command, verified with Maintenance absent. This guarantees that runtime-breaking script-coupling and silent tracing-drops are caught.
- **The Risk:** While the closure matrix is gated, the requirements do not surface where the default-activation behaviors (such as `dolt-target.sh` and `_bd_trace.sh`) rehome to ensure non-Gastown Dolt/provider cities operate seamlessly.
- **The Recommendation:** Explicitly define the rehoming destinations (to Core, Gastown, or fully inlined) in the behavior-preservation manifest (AC7) or the closure matrix (AC5).

---

## Deep-Dive Analysis: Cross-Document Consistency & Missing Edge Cases

### 1. Symlinks and Hardlinks Portability Hazard
Go's `//go:embed` directive cannot represent or preserve symlinks, hardlinks, or pointer files. Furthermore, materializing symlinks or hardlinks across different operating systems (especially Windows hosts) is highly unreliable and often causes permission errors or file corruption.
- **The Gap:** The current requirements are silent on this. If the public Gastown pack, Core pack, or any system packs utilize symlinks/hardlinks, cross-platform materialization is guaranteed to break.
- **The Risk:** Silent file corruption or materialization failures on Windows/macOS.
- **The Recommendation:** Add a validation rule to AC5 and AC16 requiring that embedded and materialized pack layouts contain absolutely no symlinks, hardlinks, or pointer-file stand-ins, enforced via static scanners or `packlint`.

### 2. Atomic Directory Materialization Semantics
While `WriteFileIfContentOrModeChangedAtomic` handles file-level atomic writes, the `MaterializeBuiltinPacks` extraction path operates file-by-file. 
- **The Gap:** If the process is killed midway during a fresh city startup, or if a concurrent background task reads `.gc/system/packs/core` during materialization, it can observe a partially materialized or corrupted directory.
- **The Risk:** Runtime crashes due to partial pack materialization.
- **The Recommendation:** Require that the pack materialization path (AC10/AC16) utilizes temporary directory staging (extracting the pack to a temporary sibling directory) followed by an atomic directory rename (`os.Rename`) to guarantee directory-level crash safety and concurrency safety.

### 3. Bidirectional Ledger Validation
AC6 ledger validation is unidirectional. It fails if an active source file is unrepresented in the ledger, but it does **not** fail if the ledger contains a "phantom row" pointing to a path that does not exist in the snapshot.
- **The Gap:** Any typo in the ledger's `current path` will result in a silent failure to migrate that file.
- **The Risk:** Orphaned files or silent migration omissions of critical assets.
- **The Recommendation:** Enforce **bidirectional** validation in AC6. The validation tool must fail if any row's `current path` does not resolve to an active file in the named `git ls-files` baseline snapshot.

### 4. Live Process Table Discovery vs. Stale Lock Files
AC10 demands the termination of running background processes and tmux sessions associated with retired Maintenance directories before isolation.
- **The Constraint:** Developers must not use stale lock files or PID files under `packs/maintenance` to find processes to kill. Per the design principle: **No status files — query live state**. The diagnostics/repair tool must scan the live process table (`ps`, `lsof`, or tmux socket queries) to identify and kill running Maintenance executors.

---

## Missing Evidence

1. **Explicit Core canonical path & deprecation policy** defined in AC2/AC13.
2. **Explicit prohibition of symlinks/hardlinks** in embedded or materialized layouts in AC5/AC16.
3. **Staging-based atomic directory materialization requirements** explicitly added to AC16.
4. **Go-level bundling/compilation removals** explicitly targeted in AC5's closure matrix.

---

## Required Changes

1. **Resolve Core's Target Path & Deprecation Policy:** Define Core's canonical post-migration location, and update AC13 to require a full stale-reference sweep covering all 12 Core-pinned test/doc files.
2. **Prohibit Symlinks and Hardlinks (AC5/AC16):** Explicitly require that embedded and materialized pack layouts contain absolutely no symlinks, hardlinks, or pointer-file stand-ins, validated by `packlint`.
3. **Detail Go-level removal contracts in AC5:** Explicitly list the deletion of `registry.go` compile-imports, `All()` entries, `publicSubpathForPack` cases, and `embed_builtin_packs.go` required lists.
4. **Enforce Bidirectional Ledger Validation in AC6:** The ledger validation tool must fail if any row's `current path` does not resolve to an active file in the named `git ls-files` baseline snapshot.
5. **Durable Advisory Locking & Live State Scan (AC10):** Require that `gc doctor --fix` uses live state queries (process table scans, tmux sockets) rather than PID/lock files to discover running processes, and holds an advisory lock to prevent concurrent mutation.

---

## Questions

- Post-split, will the Go compiler enforce that `core` does not contain any Gastown role names, or are we relying solely on the absence-scan test suite (AC8)?
- For scripts like `port_resolve.sh` and `status-line.sh`, which repo/pack will house the moved helper scripts (`dolt-target.sh`, `_bd_trace.sh`)? Will they go to Core, Gastown, or get fully inlined?
- Does first-time offline `gc init --template gastown` fail-closed with a clear diagnostic, or do we provide a binary-embedded bootstrap bundle for Gastown?
