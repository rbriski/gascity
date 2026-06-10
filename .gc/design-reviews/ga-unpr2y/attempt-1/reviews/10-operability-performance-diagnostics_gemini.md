# Ingrid Holm — DeepSeek V4 Flash (Independent Review, Attempt 19)

**Verdict:** approve

**Persona:** Ingrid Holm, Operability, Performance, and Diagnostics Reviewer. Lane: decision observability, trace and doctor diagnostics, fact read cost, and event fan-out load.

**Reviewed against:** `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md` (Attempt 19 Response Revision), `REQUIREMENTS.md`, `AGENTS.md`, and current checkout source.

---

## Overview

As the **10-operability-performance-diagnostics** lane reviewer (Ingrid Holm), this independent review evaluates the **Attempt 19 response revision** of the Session Refactor Design document. 

The Attempt 19 design is a major success for operability, performance safety, and diagnostics. It systematically addresses all six of the critical blockers and major findings identified in our Attempt 17 review, elevating performance safety from deferred-to-slice implementation details into strict, mechanically enforceable architectural boundaries.

By removing the dangerous all-session scans on lookup query hot-paths, establishing clean non-blocking repair lifecycles, and mandating a sanctioned caching/bulk mechanism for reconciler fact compilation, the design successfully protects host resources (CPU, I/O, process table) against the high costs of process fork fatigue. Furthermore, it preserves granular, operator-visible diagnostics, ensuring that explainability remains high.

Therefore, this lane enthusiastically **APPROVES** the design.

---

## Top Strengths

1. **Elimination of the Path-Alias All-Session Scan Hazard (`DESIGN.md:920`):**
   Striking `keep-with-budget` and mandating `Index or remove` for `resolveLiveSessionByPathAlias` is an outstanding win. The design explicitly recognizes that an unindexed full scan on API lookup hot-paths violates cost rules and cannot be budgeted into compliance. It forces the implementation to either index or remove the feature, completely neutralizing the host resource exhaustion risk.
2. **Clean Non-Blocking Repair Lifecycle (`DESIGN.md:359-379`):**
   Classifying empty-type repairs as an orthogonal `repair_pending` diagnostic carried on a successful `selected` result is a brilliant design choice. It preserves read-path purity and lookup availability, while enforcing that the actual write belongs to an audited, non-blocking repair command whose trigger, scheduler, and concurrency fences must be named before the adopter lands.
3. **Reconciler Hot Loop Protection (`DESIGN.md:903-908`):**
   Explicitly categorizing ordinary `bdstore` calls as subprocesses (which fork OS processes and suffer latency) and mandating that fact compilation must choose and prove a sanctioned mechanism (such as bulk reading, TTL/snapshot caching, or incremental compilation) provides a critical defense against host process table starvation.
4. **Preservation of Typed Wake/Blocker Observability (`DESIGN.md:859-863`):**
   The design successfully prohibits collapsing descriptive reason codes into flat generic strings, instead requiring that diagnostics carry structured `wake_causes[]` and `blockers[]` vectors. This preserves the rich, machine-readable diagnostic details that operators rely on for `gc trace` and `doctor` flows.

---

## Answers to Persona Questions

### 1. Can operators explain why a session was blocked, woken, drained, or closed from decider output and trace evidence alone?
* **Answer:** **Yes, absolutely.** The design preserves structured `wake_causes[]` (`WakeCause`) and `blockers[]` (`LifecycleBlocker`) vectors as machine-readable fields rather than hiding or collapsing them into generic reason strings. Additionally, the mandatory `DIAGNOSTICS_MANIFEST.yaml` maps every diagnostic outcome onto a `gc trace` rendering site, ensuring full operator visibility and machine-readable traceability.

### 2. What do gc trace, conflicts, and event logs show when a decision is rejected or an event is missed?
* **Answer:** 
  - **Rejections and Conflicts:** The `DIAGNOSTICS_MANIFEST.yaml` defines trace renderings and conflicts renderers specifically for operator-facing error codes such as `identity-conflict` and `duplicate-canonical`. 
  - **Missed Events:** The design establishes the **Durable Scan Contract** (`DESIGN.md:795-797`), ensuring that critical actions (such as work release, close, and wake) will converge from durable facts even if events are completely dropped. These background recovery scans will emit specific `scan-recovered` and `scan-failed` outcomes to `gc trace` so operators can distinguish silent background recovery from normal event-driven reactions.

### 3. What is the reconciler cost of materializing facts and emitting subscriber events across a large city?
* **Answer:** **Strictly budgeted and optimized.** Reconciler event emission now has its own budget row distinct from recovery scans, forcing tracking of per-pass emission count and cross-process advisory lock (`flock`) overhead. Fact compilation is also protected: because ordinary `bdstore` queries fork subprocesses, the design forbids arbitrary per-session loops and requires implementing a sanctioned bulk, cache, or incremental fact-compilation mechanism.

---

## Verifications & Parity

* **Requirements Alignment:** The target classifier's 8-step precedence resolver matches `REQUIREMENTS.md` query-side lookup behavior perfectly. The performance safeguards added to step 6 (`resolveLiveSessionByPathAlias`) ensure that preserving this lookup path does not introduce an operability or latency regression.
* **Reviewer Interlock:** This review aligns closely with **Takeshi Yamamoto's** pure decider mandates (taking a mandatory non-zero `now` fact and returning immutable mutation/intent descriptions) and **Ravi Krishnamurthy's** coexistence guidelines (preventing split-brain writes via strict, documented mutation-family boundaries and cross-process fences).

---

## Recommendations for Implementation Slices

To ensure successful delivery, we recommend the following during implementation:
1. **Prioritize the Bulk Reader:** To easily meet the Reconciler Fact Compilation budget on `bdstore`, implement a bulk session state reader in `beads.Store` early, allowing a single pass to fetch all active sessions in one query rather than running individual queries in a loop.
2. **Audit Caching TTLs:** When implementing a TTL cache or snapshot cache for reconciler facts, ensure that the TTL is configurable and matched against the reconciler tick interval to prevent stale-fact decisions while minimizing process forks.
3. **Leverage Non-blocking Background Queue for Repairs:** Ensure that when target classification returns `repair_pending`, the API handler delegates the repair write to an asynchronous background worker queue immediately so that user-facing read latencies remain minimal and unaffected by database write locks.
