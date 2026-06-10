# Liam Okonkwo — DeepSeek V4 Flash (Independent Review, Attempt 19)

**Verdict:** block

**Review scope:** Reconciler boundary, runtime-intent adapter ownership, fact isolation, health-gate split, and design-to-code alignment for the Reconciler Runtime Fact Reviewer mandate. Evaluated against the Attempt 19 iteration of `internal/session/DESIGN.md` (matching `.gc/design-reviews/ga-unpr2y/attempt-19-retry-20260610T065241Z/design-before.md`), `REQUIREMENTS.md`, the scoped `AGENTS.md`, and the active checkout source.

---

## Overview

The Attempt 19 streamline of `./internal/session/DESIGN.md` is a highly mature, conceptually robust iteration. Transitioning "Slice 0" from a passive planning concept into a mandatory, non-mutating delivery gate comprising physical symbols, key lists, and static tests is an exceptional defensive step. This is precisely what is needed to lock down the codebase before any mutation-owning changes are allowed to land.

However, from the perspective of the **Reconciler Runtime Fact Reviewer (Liam Okonkwo)**, the Technical Design cannot be approved for decomposition. It still contains critical fact-isolation violations, category errors at the prepare/observe boundary, and un-reconciled policy leaks. The pure session projection continues to fall back to machine wall-clock queries in `lifecycle_projection.go`. The awake-set decider still evaluates pool-level scheduling policy directly in `compute_awake_set.go` via `countMinActiveCovered`. The `RuntimeStartIntent` straddles intent and observation by carrying the post-start `session_key` field at prepare-time. Furthermore, critical operational evidence like W-013 detached probes lacks freshness bounds, supersession rules, and fail-closed timeout definitions.

To safeguard the core session primitive boundary and prevent runtime policy from leaking into `internal/session`, **this review must sustain a strict BLOCK.**

---

## Top Strengths

1. **Robust, Low-Coupling Reconciler/Session Split Matrix (lines 706–713):**
   The split matrix is exceptionally well-defined. It correctly restricts `internal/session` to pure lifecycle eligibility and identity classification over immutable facts, while preserving the controller/reconciler's ownership of policy and demand (work demand, dispatch scheduling, pool sizing, cold-start demand, restart budgets, and progress policy). This prevents structural policy leaks.
2. **Minimalist, Deferral-First Backlog Sequence (lines 752–758):**
   Prioritizing non-mutating Slice 0 preflights and side-effect-free Slice 1 read-only target classification before touching any mutating command or reconciler fact-moving path is a highly disciplined, TDD-friendly progression.
3. **Streamlined Destructive-Action Boundary (lines 730–733):**
   Forcing destructive branches to explicitly consume and validate the completion state of runtime observations (`complete-missing`, `stale-observation`, etc.) aligns perfectly with the GUPP and NDI (Nondeterministic Idempotence) principles.

---

## Critical Risks & Blockers

### 1. [Blocker] Fact Isolation Compromised: Load-bearing `ProjectLifecycle` clock fallback inside active codebase
* **Evidence:** `internal/session/lifecycle_projection.go` lines 381 and 609 (clock fallback: `now = time.Now().UTC()`).
* **Why it matters:** The design mandates that pure session deciders receive immutable facts and do not perform ambient clock reads (`DESIGN.md:745-746`, `759-762`). It explicitly states that "Mutation-feeding deciders take a mandatory non-zero `now` fact and reject zero values" (`DESIGN.md:765-767`). However, `ProjectLifecycle` (the central, load-bearing projection function) and `creatingStateIsStale` still contain local wall-clock fallbacks when `input.Now` is zero:
  ```go
  now := input.Now
  if now.IsZero() {
  	now = time.Now().UTC()
  }
  ```
  If a caller fails to pass an explicit timestamp, the decider yields non-deterministic results based on the local OS clock, destroying test replayability and violating decider purity. Fact isolation must be call-level and absolute.
* **Assumption other reviewers accept too quickly:** Other reviewers assume that removing the clock fallback is a trivial chore that can be deferred or easily patched. In reality, this fallback is a load-bearing crutch in current callers; removing it will cause instant nil/zero time failures unless a thorough audit of all calling code in `internal/api` and `cmd/gc` is executed to ensure `input.Now` is populated correctly.
* **Required Change:** Completely remove the `time.Now().UTC()` fallbacks from `lifecycle_projection.go`. Enforce `input.Now` as a mandatory, non-zero field, returning an error or failing fast if it is missing. Redefine any static AST guards to inspect pure decider files to reject direct calls to `time.Now()`.

### 2. [Blocker] Policy Leakage: Required Matrix Rows Contradict Progress and Idle-Sleep Ownership
* **Evidence:** `DESIGN.md` lines 752–755 versus lines 708–710.
* **Why it matters:** The split table keeps progress policy and idle-sleep policy strictly in the `controller/reconciler` (`DESIGN.md:709`). However, "Required rows before behavior-moving slices" lists `progress and idle thresholds` as required `BOUNDARY_MATRIX.yaml` rows (`DESIGN.md:752-753`). If the decider receives or evaluates thresholds, it is executing progress/idle *policy*, which breaches the boundary and introduces red flag #3 (policy leakage). If instead the reconciler evaluates these thresholds and compiles them into simple opaque boolean facts (e.g. `ProgressStalled`), the required rows must be renamed to reflect that only precompiled *facts* cross the boundary, not the *thresholds* (policy parameters) themselves.
* **Assumption other reviewers accept too quickly:** Reviewers accept the inclusion of "thresholds" in the required rows list on the assumption that thresholds are merely data fields, ignoring that thresholds are policy configurations. Once a decider parses a threshold config, it assumes the responsibility of policy evaluation, making the decider coupled to environment configuration.
* **Required Change:** Rename "progress and idle thresholds" in the required rows list to "precompiled progress-stall and idle-sleep facts". Add an explicit rule stating that no session decider may receive, parse, or evaluate a progress or idle threshold value.

### 3. [Blocker] State-Tracking Ambiguity: Runtime-Missing Anti-Flap Rule Lacks a Clear Persistence and Mutation Contract
* **Evidence:** `DESIGN.md` lines 747–750 (runtime-missing anti-flap rule in boundary matrix).
* **Why it matters:** Gascity forbids status files, mandating that process liveness is discovered by querying the live system. Under heavy load, transient query failures can cause processes to appear missing. While the design correctly mandates a "runtime-missing anti-flap rule with a tested corroboration count or grace window", it does not specify where this state (e.g., consecutive misses) is tracked. Because session deciders must be pure and stateless, they cannot maintain an in-memory counter. If the counter is tracked in-memory by the reconciler, it will reset on crash, risking split-brain writes during restarts. If it is stored in session metadata (as a durable but transient counter), the pure decider must receive it and return an updated counter as part of its mutation output, which requires defining how this metadata field is protected from external writers.
* **Assumption other reviewers accept too quickly:** Reviewers assume a "tested anti-flap rule" is a self-contained logic block, failing to realize that stateful counters in a stateless decider system introduce a complex data-flow and persistence loop. If the state is not durably persisted, transient observer failures across reconciler restarts can cause premature destructive cleanup.
* **Required Change:** Explicitly specify the state-tracking contract for the anti-flap corroboration count: define whether it is stored durably in session metadata or tracked in-memory by the reconciler, and specify how the state is mutated during the observation-to-write lifecycle.

### 4. [Major] Prepare vs. Observe Boundary on `session_key` in `RuntimeStartIntent`
* **Evidence:** `internal/session/DESIGN.md` lines 714–722.
* **Why it matters:** The design states that provider-neutral runtime intent fields "may include... runtime session key" (which is `session_key`) (lines 719-721). However, the `session_key` is a dynamic, observer-driven runtime token generated during the *observed start* of the session, not during the preparation of the start intent. Carrying this observation token inside a prepare-time intent is a category error that violates the separation between intent-preparation and runtime-observation.
* **Assumption other reviewers accept too quickly:** Reviewers accept the inclusion of `session_key` in intent payloads under the assumption that it helps with routing or correlation, ignoring that it smuggles a post-start runtime state into a pre-start decider.
* **Required Change:** Explicitly state in the design that `session_key` must remain strictly an observation fact captured during or after start, unless a deterministic caller-side pre-generation formula is defined.

### 5. [Major] Hot-Loop Performance: Reconciler Fact Compilation Lacks a Batch-Query Mandate
* **Evidence:** `DESIGN.md` lines 921-922 (cost rules), lines 766-767 (fact compilation).
* **Why it matters:** The pure-decider boundary requires that all facts (such as work counts, pool size, active work beads) are pre-scanned and compiled before being passed into session-owned deciders. In a city with hundreds of sessions, compiling facts individually per session will trigger an $O(N)$ database read amplification on every tick of the reconciler loop. Although the design requires a budget row for reconciler fact compilation, it does not specify a physical implementation mandate (e.g., batch-querying the bead store) to prevent catastrophic performance scaling.
* **Assumption other reviewers accept too quickly:** Reviewers assume that performance concerns are low-level optimization details that can be ignored at the design stage. However, $O(N)$ read amplification in a continuous reconciler tick loop is an architectural failure mode that will cause high CPU and DB starvation on moderately sized cities.
* **Required Change:** The design must explicitly commit to a *batch fact compilation protocol* where the reconciler fetches all active session and work beads in a single batch-query/scan, rather than compiling facts individually per session.

### 6. [Minor] Routing Leakage: Missing Routing-Only Restriction on `provider family`
* **Evidence:** `DESIGN.md` lines 714-722 (`RuntimeIntent` fields).
* **Why it matters:** The design allows `provider family` to cross into session-owned commands under `RuntimeIntent` to support start routing (`DESIGN.md:717-718`). However, this field can easily become a policy seam (e.g., if a decider branches its lifecycle policy based on whether the provider is local tmux vs remote Kubernetes). To preserve provider-neutrality, the design must explicitly restrict what the decider may do with this field.
* **Required Change:** Add an explicit caveat: `provider family` may be carried in `RuntimeIntent` for routing/identity purposes only; session deciders must not branch lifecycle policy or state transitions based on it.

---

## Missing Evidence

- **Pure Decider AST Guard Test:** An automated AST parser test in `internal/session` that scans the pure-decider files and fails the build if any direct call to `time.Now()` or store-query patterns are found.
- **Reconciler Batch Fact Compilation Proof:** Detailed budget specifications or mock implementations demonstrating how the reconciler can avoid individual store scans for each session on hot loops.
- **Physical Slice 0 Test Skeletons:** The Go test code files defining the minimum proof targets (`TestSessionBoundaryGuard`, `TestSlice0Contract`, etc.). Citing a minimum proof command containing 12+ non-existent test targets is a cosmetic success that provides no physical protection against pattern drift.

---

## Required Changes

1. **Enforce Absolute Decider Purity:** Remove `time.Now().UTC()` clock fallbacks from `lifecycle_projection.go`, making `input.Now` mandatory and non-zero.
2. **Clarify Threshold Ownership:** Rename "progress and idle thresholds" to "precompiled progress-stall and idle-sleep facts" in the required `BOUNDARY_MATRIX.yaml` rows, and forbid deciders from evaluating threshold parameters.
3. **Define Anti-Flap State Contract:** Codify the state-tracking contract for the runtime-missing anti-flap corroboration count (durable metadata vs in-memory).
4. **Specify Batch Compilation:** Explicitly commit to a single, batch-based fact query compilation step in the reconciler to prevent $O(N)$ database scan overhead.
5. **Add Routing-Only Caveat:** Add a routing-only constraint to `provider family` in `RuntimeIntent` to prevent provider-specific policy branching inside deciders.

---

## Answers to Persona Questions

### 1. Which wake, hold, drain, provider-health, and progress decisions move into session deciders, and which scheduling or budget responsibilities remain in the reconciler?
**Answer:**
- **Move to session deciders:** Determining basic transition eligibility (such as wake blockers, terminal states, configured identity conflicts, and hold/drain timeouts) based on immutable input facts.
- **Remain in reconciler:** Aggregating work demand, computing desired pool counts, handling cold-start scaling, tracking provider health, progress policy, restart/rollback budgets, and orchestrating destructive actions.

### 2. Are work counts, pool size, runtime liveness, and progress facts precomputed by adapters instead of queried from deciders?
**Answer:** Yes. Pure session deciders perform zero store queries or I/O. All required operational facts—such as active work counts, pool sizing, and observed runtime liveness—must be pre-scanned and compiled by callers/adapters and passed to the decider via copyable, immutable structures.

### 3. Can RuntimeIntent express adapter needs without smuggling provider policy into `internal/session`?
**Answer:** Yes. `RuntimeIntent` is a pure declarative state representation (e.g. specifying `session_id`, deterministic `session_key`, config hash, etc.) that specifies *what* is intended. The runtime provider adapters (tmux, subprocess, k8s) consume this intent and translate it into provider-specific actions and policies, keeping `internal/session` completely free of provider leakages.

---

## Consistency Report

* **Pattern Alignment (with Elena Marchetti - Wave 1, Mutation Boundary Auditor):**
  * We strongly align with Elena's finding that the AST static guards are under-specified and currently exist only as prose. Citing non-existent test files like `TestSessionBoundaryGuard` in the minimum proof command is a massive consistency issue.
  * We also agree with Elena's identification of the multi-writer concurrency and split-brain risks during transition; our finding on the lack of a concrete cooldown/anti-flap rule for transiently missing runtime observations highlights a similar split-brain risk on the query-and-observe boundary.
