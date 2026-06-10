# Takeshi Yamamoto — DeepSeek V4 Flash (Independent Review, Attempt 19)

**Verdict:** approve-with-risks

**Review scope:** Pure decider enforcement, optimistic concurrency, commit-event-intent ordering, stale-fact defense, and boundary-inventory-enforceability for the Decider Atomicity Enforcer mandate. This reviews the current Attempt 19 iteration of [DESIGN.md](file:///data/projects/gascity/internal/session/DESIGN.md) against `REQUIREMENTS.md`, `AGENTS.md`, and the active checkout source.

---

## Top Strengths

- **Store Capability Matrix Requirement**: The introduction of `STORE_CAPABILITY_MATRIX.yaml` as an absolute preflight gate for mutating slices (documented in [DESIGN.md:95](file:///data/projects/gascity/internal/session/DESIGN.md#L95), [DESIGN.md:211](file:///data/projects/gascity/internal/session/DESIGN.md#L211), [DESIGN.md:229](file:///data/projects/gascity/internal/session/DESIGN.md#L229), [DESIGN.md:456](file:///data/projects/gascity/internal/session/DESIGN.md#L456), and [DESIGN.md:578](file:///data/projects/gascity/internal/session/DESIGN.md#L578)) is a major architectural improvement. Forcing each mutating operation to explicitly document and rely on the persistence layer's actual capabilities (rather than assuming the presence of transactions or versioned APIs) prevents speculative design drift.
- **Strict Reconciler-Session Split**: The demarcation of boundaries in "Reconciler, Runtime, And Session Split" (documented in [DESIGN.md:73](file:///data/projects/gascity/internal/session/DESIGN.md#L73), [DESIGN.md:100](file:///data/projects/gascity/internal/session/DESIGN.md#L100), [DESIGN.md:121](file:///data/projects/gascity/internal/session/DESIGN.md#L121), and [DESIGN.md:752-758](file:///data/projects/gascity/internal/session/DESIGN.md#L752-L758)) is clean. Session state is kept clear of scheduling, budget, and desired pool sizes, which are correctly maintained inside the reconciler/controller.
- **Typed Event Payload Requirement**: Requiring standard `session.*` events to register explicit typed payloads instead of default `events.NoPayload` envelopes (documented in [DESIGN.md:70](file:///data/projects/gascity/internal/session/DESIGN.md#L70), [DESIGN.md:216](file:///data/projects/gascity/internal/session/DESIGN.md#L216), and [DESIGN.md:775-785](file:///data/projects/gascity/internal/session/DESIGN.md#L775-L785)) ensures that public SSE/OpenAPI streams and safety-critical subscribers consume typed facts rather than relying on unmodeled envelope headers.

---

## Critical Risks

### 1. [Major] The TOCTOU "Read-Then-Write" Table Contradiction
- **Evidence:** [DESIGN.md:95](file:///data/projects/gascity/internal/session/DESIGN.md#L95) vs [DESIGN.md:66](file:///data/projects/gascity/internal/session/DESIGN.md#L66) and [DESIGN.md:555-560](file:///data/projects/gascity/internal/session/DESIGN.md#L555-L560).
- **Why it matters:** The table row on [DESIGN.md:95](file:///data/projects/gascity/internal/session/DESIGN.md#L95) lists "durable value/token/revision precondition with immediate reread" as one of the valid options for a fence strategy. However, [DESIGN.md:66](file:///data/projects/gascity/internal/session/DESIGN.md#L66) explicitly strikes "Durable precondition with immediate reread" from the valid fence list, and [DESIGN.md:555-560](file:///data/projects/gascity/internal/session/DESIGN.md#L555-L560) states that `"reread then write" is not a valid strategy because a client-side reread followed by an unconditional write remains a TOCTOU race across CLI/API/controller processes`. 
This intra-document contradiction introduces severe ambiguity. If a developer uses the table on line 95 as their reference, they might implement a race-prone "reread and check" sequence and label it a fence, which fails to protect against multi-process lost-update races due to `beads.Store`'s projection lag (`BdStore.waitForUpdateProjection` at [bdstore.go:1037](file:///data/projects/gascity/internal/beads/bdstore.go#L1037)).
- **Required Resolution:** Clean up the finding table's row on [DESIGN.md:95](file:///data/projects/gascity/internal/session/DESIGN.md#L95) to match the final contract on line 555, striking client-side "reread then write" entirely as a valid fence strategy.

### 2. [Major] Single-Process lock `WithSessionMutationLock` Cannot Protect Multi-Process Writes
- **Evidence:** [chat.go:165-199](file:///data/projects/gascity/internal/session/chat.go#L165-L199) and [DESIGN.md:555-560](file:///data/projects/gascity/internal/session/DESIGN.md#L555-L560).
- **Why it matters:** The design positions `WithSessionMutationLock` as a primary tool to serialize metadata mutations and avoid concurrent modification races. However, this is implemented as a purely **in-memory, single-process mutex lock** using Go's `sync.Mutex` and an in-process map. 
While this prevents concurrent writes within a single process (such as multiple threads inside the controller daemon), the **Gas City CLI runs in a completely separate OS process** from the controller daemon. When the operator runs a CLI command (like `gc stop`) and the daemon runs its reconciler loop concurrently on the same session bead:
  1. Both processes have entirely independent memory spaces and cannot see each other's in-memory mutexes.
  2. They will execute concurrent read-compare-write sequence blocks on `beads.Store` with no physical synchronization.
  3. Because `beads.Store` does not support atomic compare-and-swap (CAS) or transaction-level revision locking on metadata keys, both processes will race, leading to lost updates or corrupted states.
- **Required Resolution:** The design must acknowledge that single-process Go mutexes are insufficient for multi-process safety, and define a standard multi-process synchronization strategy (such as file locking, store-level validation hooks, or conditional-commit fences) to protect concurrent writers.

### 3. [Major] Pure Decider Purity Compromised by System Wall-Clock Reads
- **Evidence:** [lifecycle_projection.go:379-382](file:///data/projects/gascity/internal/session/lifecycle_projection.go#L379-L382) and lines [607-610](file:///data/projects/gascity/internal/session/lifecycle_projection.go#L607-L610).
- **Why it matters:** The core invariant of the decider model is that session deciders are pure functions consuming only immutable, explicit facts. However, `ProjectLifecycle` and `creatingStateIsStale` retain local wall-clock fallbacks when `input.Now` is zero:
  ```go
  now := input.Now
  if now.IsZero() {
      now = time.Now().UTC()
  }
  ```
  If a caller fails to pass an explicit timestamp, the decider yields non-deterministic projection results based on the local OS clock. This breaks test replayability and state-machine predictability. The design's new pure-decider guard requirement ([DESIGN.md:766](file:///data/projects/gascity/internal/session/DESIGN.md#L766)) explicitly states that "Mutation-feeding deciders take a mandatory non-zero `now` fact and reject zero values" and must not import "ambient time" ([DESIGN.md:762](file:///data/projects/gascity/internal/session/DESIGN.md#L762)). However, the code itself contains active clock reads. Purity must be call-level and absolute: the `now` field on `LifecycleInput` must be mandatory, and the system clock fallback must be completely removed from `lifecycle_projection.go`.
- **Required Resolution:** Completely remove the system clock fallback from [lifecycle_projection.go](file:///data/projects/gascity/internal/session/lifecycle_projection.go) and make the `now` timestamp a mandatory, non-zero field on `LifecycleInput`.

### 4. [Major] Lack of Unified Close-Path Command
- **Evidence:** [manager.go:862](file:///data/projects/gascity/internal/session/manager.go#L862) (`CloseDetailed`) versus [session_beads.go:2144](file:///data/projects/gascity/cmd/gc/session_beads.go#L2144) (`closeBead`).
- **Why it matters:** The codebase continues to contain two structurally divergent and concurrent close paths:
  - **`CloseDetailed`**: stops the provider, cancels waits, clears wake/hold overrides, retires named session identifiers, and closes the bead.
  - **`closeBead`**: directly writes basic metadata and closes the bead without wait cancellation, override clearing, or identifier retirement.
  
  Because `closeBead` is frequently invoked by the reconciler and lifecycle paths in `cmd/gc` without wait cancellation or override clearing, closed session beads will routinely retain active wake/hold overrides in the database, risking unintended recreations or misrouting on subsequent reconciler ticks. The design must mandate the complete unification of these close paths into a single session-owned command.
- **Required Resolution:** Fold `closeBead` and `CloseDetailed` into a single session-owned close command that guarantees wait cancellation, override clearing, and identity retirement are always applied atomically or resolved via an explicit, durable repair loop.

---

## Required Changes

1. **Enforce Call-Level Decider Purity**: Completely remove the local clock fallback from [lifecycle_projection.go](file:///data/projects/gascity/internal/session/lifecycle_projection.go) and make the `now` timestamp a mandatory, non-zero field on `LifecycleInput`.
2. **Standardize Cross-Process Concurrency Control**: Define a global architectural standard for cross-process concurrency control (e.g. dolt-level conditional write fences, store-level validation hooks, or file locks) to resolve the single-process limitation of `WithSessionMutationLock`.
3. **Eliminate the Client-Side Precondition Illusion**: Align the finding table's row on [DESIGN.md:95](file:///data/projects/gascity/internal/session/DESIGN.md#L95) with the contract on line 555, striking client-side "reread then write" entirely as a valid fence strategy.
4. **Unify Close Paths**: Fold `closeBead` and `CloseDetailed` into a single session-owned close command that guarantees wait cancellation, override clearing, and identity retirement are always applied atomically or resolved via an explicit, durable repair loop.

---

## Questions

1. Since `WithSessionMutationLock` only serializes access inside a single Go process, how will we prevent the `gc` CLI process and the controller reconciler daemon process from concurrently mutating the same session bead's metadata?
2. Should we specialize the `beads.Store` interface to support an atomic metadata batch update (e.g. utilizing an underlying transaction or database lock) for compatible backends to prevent partial-state failures?
3. How does the design justify placing reconciler-owned requirements (`SESSION-RECON-*`) as blocking criteria for the Session Slice 0 gate, given that they violate the Reconciler/Session split?
