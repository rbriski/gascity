# Leah Okafor — Doctor and Runtime-State Mutation Safety Reviewer (Iteration 12 / Attempt 12, Independent DeepSeek V4 Flash Style)

**Verdict:** block

> **Lane:** Doctor `--fix` coordinator atomicity, byte-preserving TOML editing, concurrency with live controllers, advisory locks, idempotent recovery.
> 
> Reviewed against the Iteration 12 / Attempt 12 implementation plan (`plans/core-gastown-pack-migration/implementation-plan.md`, 310 lines, `updated_at: 2026-06-10T08:17:00Z`) — specifically §"GC Import Launch Implementation Plan" and Tasks 1, 2, and 6.
> 
> This independent review is produced using the DeepSeek V4 Flash style, focusing rigorously on first-principles trust boundaries, cross-document state consistency, and unstated runtime assumptions.

---

## Schema Conformance

**Conforms with reservations.** The Iteration 12 / Attempt 12 implementation plan contains all required top-level sections in the correct order, carries the required front matter, and lists `Open Questions` as `None`. However, by radically rescoping the plan to focus solely on the "GC Import Launch" tasks, the plan has completely omitted the previously drafted "Doctor And Runtime-State Mutation Safety" architecture, recovery schemas, and transaction coordination. While the file structurally conforms to `implementation-plan.schema.md`, dropping the entire safety infrastructure while keeping active mutating tasks represents a massive gap in engineering rigor.

---

## Top Strengths of the Design

- **Explicit, No-Fetch Remediation Model (Task 1, lines 67-79):** Eliminating implicit remote fetches on start, config, and supervisor paths removes uncontrolled network I/O from critical runtime loops. Failing with a clear `gc import install` hint enforces a clean boundary between read-only execution and explicit dependency resolution.
- **Idempotency Goal for Bootstrap/Repair (Task 2, lines 87-98):** Requiring repeated `gc import install` runs to be idempotent is a core safety practice that reduces the probability of duplicate write operations during bootstrap or repair.
- **Explicit Warnings for Stale Syntaxes (Task 6, lines 173-187):** Removing stale references to legacy structures like `[rig_defaults]` and pointing operators to the canonical `[defaults.rig.imports.<binding>]` syntax helps clean up the configuration space.

---

## Critical Risks & Consensus Blockers

### 1. Complete Abdication of the Doctor Mutation Safety Coordinator (Lane Mandate / RF1 / RF3)

In Iteration 12, the previously proposed `internal/doctorfix` coordinator, the write-ahead recovery journal, and directory advisory locks have been completely stripped from the design. The legacy `c.Fix(ctx)` execution path (at `internal/doctor/doctor.go:74`) remains active, uncoordinated, and lock-less.
With this change, Task 6 plans to run mutating doctor fixes and migrations (aligning migrate and doctor with the new default-rig syntax) through raw, uncoordinated file writes. This leaves the system completely exposed to:
- **Red Flag #1:** Cross-file failure-atomicity is claimed from per-file rename semantics with no WAL or journal.
- **Red Flag #3:** Network, lock loss, or live controller races can leave `city.toml` and runtime state inconsistent.
- **Required Resolution:** Restore a lock-first mutation coordinator (`internal/doctorfix`) as the sole path that writes city configurations, manifests, or lockfiles.

### 2. Multi-File Rename Atomicity is Unmitigated (RF1)

Task 6 modifies both `city.toml` and other configuration files during default-rig syntax alignment. Because POSIX renames are atomic only per-file, not across files, a crash or power loss during these multi-file migrations can leave the city's configurations half-updated. Without a durable commit point marker or a write-ahead log, there is no way for a restarted system to know whether to roll back or roll forward, violating basic safety invariants.
- **Required Resolution:** Define a single commit point filesystem operation (e.g., writing/renaming a specific commit marker file in the recovery ledger) that gates post-marker idempotent roll-forward.

### 3. Unlocked Bootstrap and Repair on the Read Path (Task 2 vs RF3)

Task 2 makes `gc import install` the single bootstrap and repair command, which writes `packs.lock` and installs cache state to `.gc/system/packs`. However, the plan specifies no lock or mutual exclusion for this write operation. 
If a concurrent controller or operator runs a command that triggers this bootstrap/repair path while another `gc` process is mutating `.gc/system/packs`, they will race on the filesystem. This can lead to partial pack installations, file lockups, or corrupted lockfiles.
- **Required Resolution:** Mandate that `gc import install` and all pack-installation paths must acquire and hold a directory-level advisory lock (`flock`) to ensure mutual exclusion.

### 4. Destructive TOML Rewrites of User Configurations (Task 6 vs RF2)

Task 6 requires migrating and fixing TOML configurations to match the new `[defaults.rig.imports.<binding>]` syntax. However, the standard library TOML library used in the codebase (`BurntSushi/toml`) does not preserve comments or key ordering during decode/encode round trips.
If the doctor automatically executes these fixes, it will destroy user-authored comments and formatting. This violates the safety requirement to never rewrite user-modified files without proof of system-generation.
- **Required Resolution:** Explicitly name a format-preserving TOML parser/editor or token-span patcher, or require the doctor to refuse automatic fixes on any TOML file containing comments, falling back to report-only mode with manual instructions.

---

## Missing Evidence

1. **No Named Format-Preserving TOML Library:** The plan fails to specify how Task 6 will perform TOML rewrites without destroying comments and formatting.
2. **No Concurrency Lock Specification:** No OS-level advisory locking (`flock`) is defined to prevent concurrent `gc import install` or doctor fixes from racing with live controllers.
3. **No Transactional Recovery Ledger:** There is no recovery or journal schema specified to handle crashes during multi-file migrations.

---

## Required Changes

1. **Reintroduce the Mutation Coordinator:** Restore `internal/doctorfix` with staging, preflight validation, and transactional rename logic for all mutating doctor checks.
2. **Implement OS-Level Advisory Locking:** Mandate that all mutating commands (`gc import install`, `gc doctor --fix`, and the live controller) acquire and hold a shared OS-level advisory lock (`flock`) to prevent concurrent write races.
3. **Establish a Format-Preserving TOML Strategy:** Specify a comment-preserving TOML editor or enforce a strict refusal gate for commented TOML files to prevent destructive rewrites.
4. **Define Crash-Recovery Invariants:** Require a durable write-ahead journal and a single, linearizing commit point filesystem operation for multi-file migrations.

---

## Responses to Lane-Specific Questions

### Q1: Do all doctor --fix paths stage FixIntent objects before mutation and hold a city advisory lock across stage, validate, compare-before-rename, and publish?

**Answer:** 
No. In the Iteration 12 design, the proposed `FixIntent` staging and advisory locks have been completely omitted. Mutating doctor paths (such as the default-rig alignment in Task 6) run directly and uncoordinated, creating severe concurrency and TOCTOU risks.

---

### Q2: When scoped TOML edits cannot preserve comments or unknown fields byte-for-byte outside intended changes, does the fixer refuse rather than rewrite whole files?

**Answer:** 
No. The Iteration 12 plan does not define any comment-preservation mechanism or refusal gates. Using the standard `BurntSushi/toml` library will result in the destructive erasure of user-authored comments during automatic fixes, violating the core mutation safety invariants.

---

### Q3: What recovery is specified for crashes or concurrent old and new binaries between per-file renames so cities cannot remain half-migrated?

**Answer:** 
None. The Iteration 12 plan provides no recovery ledger, write-ahead journal, or rollback/roll-forward invariants. A crash between per-file renames during default-rig migration will leave the city in a corrupt, half-migrated state with no automated recovery path.
