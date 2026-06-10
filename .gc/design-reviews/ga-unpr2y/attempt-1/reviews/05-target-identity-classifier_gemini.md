# Amara Diallo — DeepSeek V4 Flash (Independent Review, Attempt 19)

**Verdict:** approve-with-risks

**Persona:** target taxonomy, alias precedence, conflict cases, caller behavior preservation, cross-document consistency.

**Reviewed against:** `internal/session/DESIGN.md` (attempt 19, `.gc/design-reviews/ga-unpr2y/attempt-19/design-before.md`), `internal/session/resolve.go`, `internal/session/named_config.go`, `internal/api/session_resolution.go`, and attempt-17 feedback loops.

---

## Top Strengths

The Attempt 19 revision represents an exceptional, high-fidelity response to the Attempt 17 feedback, resolving every critical blocker with meticulous precision:

- **Elegantly Decoupled Repair Diagnostic Model:** Transforming `repair-needed` from a fragile, error-prone result kind into an orthogonal `repair_pending` diagnostic carried on a successful `selected` result (lines 363-366) is a structural triumph. This guarantees that read-only API queries remain 100% successful even when repair writes are outstanding or database write-persistence fails.
- **Exhaustive Configured-Name Fallback Matrix:** Incorporating the complete, multi-tiered `findNamedSessionSpecForTarget` fallback sequence (lines 327-331) directly into Step 3 of the precedence rules prevents any incorrect fallback behavior and satisfies the most stringent requirements of the configuration layer.
- **Dual-Error Identity Preservation:** Resolving config-orphans as a `rejected` result kind that renders a 404 while preserving the server-side dual `errors.Is` behavior (matching both `session.ErrSessionNotFound` and the config-orphan rejection marker, lines 340-344) is highly elegant and maintains complete backward compatibility with existing error routing.
- **Deterministic Path-Alias Tie-Breaker:** Formally specifying that the newest `CreatedAt` timestamp is tie-broken by alphabetical ordering of bead IDs (lines 347-349) brings absolute determinism to path-alias matching.

---

## Critical Risks (Remaining risks to be addressed during implementation)

### 1. [Major] Stampeding Async Repair Commands under Concurrent Read-Only Queries
- **Evidence:** `internal/session/DESIGN.md` lines 364-374 (`RepairEmptyType` quarantine and asynchronous repair scheduling).
- **Why it matters:** 
  Because synchronous read-path repair is forbidden to maintain read-only purity, the API query adapter must trigger the audited repair command asynchronously when a read-only query returns a `repair_pending` diagnostic. 
  However, in a high-concurrency production environment (such as a dashboard pool refresh or parallel client requests), an unrepaired empty-type bead may receive dozens of concurrent read-only query requests. If each query independently detects `repair_pending` and asynchronously schedules a repair command, this will trigger a **stampede of duplicate repair commands** on the same session bead. 
  Since the underlying bead store does not support native transaction-level compare-and-swap fences, this stampede will result in redundant audit log entries, database lock contention, and potential database write races.
- **Mitigation/Required Change:** 
  The design must mandate a debouncing or deduplication mechanism at the scheduler level (e.g., an in-memory lock/in-progress map, or scheduler-level dedup by bead ID) to ensure that only a single active asynchronous repair command is spawned for a given bead ID, regardless of how many concurrent queries detect `repair_pending`.

### 2. [Major] Wire Serialization and Client-Side Loss of Dual Error Identity on 404
- **Evidence:** `internal/session/DESIGN.md` lines 340-344 (Step 5: config-orphan error preserves dual `errors.Is` identity).
- **Why it matters:** 
  While wrapping the error on the server ensures `errors.Is` works for local Go callers, once that error is serialized over the wire via `writeResolveError` or `humaResolveError` as a standard `404 Not Found`, the specific `ErrRejectedByConfig` identity is lost to client-side consumers. 
  Unless the custom error response body explicitly serializes the config-orphan/rejected classification detail, a CLI client (using `apiClient()`) or external integration will only see a generic `404` and will be unable to reconstruct the dual error identity, breaking client-side scripts, assertions, or diagnostic tools.
- **Mitigation/Required Change:** 
  The design must explicitly specify that the `writeResolveError` and `humaResolveError` wire-serialization formats must include a machine-readable rejected/config-orphan code in the HTTP JSON problem body, and the `apiClient()` parser must deserialize this detail to reconstruct the dual error so that `errors.Is(err, ErrRejectedByConfig)` still evaluates to `true` on the client side of the wire.

### 3. [Minor] Ambiguous Representation of Unmaterialized Configured Targets in `match_vectors[]`
- **Evidence:** `internal/session/DESIGN.md` lines 332-333 and lines 390-392 (`config_state` and `match_vectors[]` schema).
- **Why it matters:** 
  Step 3 correctly states that non-materializing query paths return not-found for reserved configured identities without a canonical bead. 
  However, it is silent on how this "reserved but unmaterialized" state is represented in the raw classifier output. If the classifier returns `result_kind: not-found`, does it still populate `match_vectors[]` with a `configured-name` vector? If so, what is the `candidate ID`? 
  If the representation is not explicit, the surface adapter may fail to recognize that a configured-name match occurred, and incorrectly fall through to live lookup or path-alias matching.
- **Mitigation/Required Change:** 
  Explicitly specify that when a configured named session has no canonical bead, the classifier includes a `configured-name` vector in `match_vectors[]` with a null/empty `candidate ID`, and sets the `reserved-unmaterialized` flag to `true` in `config_state`. The surface adapter must immediately short-circuit upon seeing this vector to prevent live fall-through.

### 4. [Minor] Split-Brain Identity Resolution between API Query and Characterization-Only Surfaces
- **Evidence:** `internal/session/DESIGN.md` lines 353-354 and lines 439-447 (Historical aliases and per-surface delegation matrix).
- **Why it matters:** 
  Historical aliases are explicitly excluded from query-side lookup (Step 7). However, Mail and Extmsg are "Characterization only" and continue to use legacy resolution logic. 
  If Mail or Extmsg still resolve historical aliases (or use different case-normalization rules) during this transitional phase, this creates a temporary "split-brain" state. For example, sending mail to a historical alias might succeed, but querying the session's details via the API using that same alias will fail with a 404, confusing operators.
- **Mitigation/Required Change:** 
  Add a requirement for the first adopter to include a documented operational warning regarding this temporary split-brain resolution behavior, and add an anti-drift test assertion verifying how historical aliases are rejected on the query path versus how they are handled on the legacy paths.

---

## Required Changes

1. **Deduplicate Async Repair Triggers:** In the asynchronous repair contract/lifecycle section, add: *"The asynchronous repair scheduler must deduplicate concurrent repair triggers for the same bead ID, ensuring that multiple concurrent query lookups detecting `repair_pending` do not spawn duplicate repair commands before the first write commits."*
2. **Specify Client-Side Wire Reconstruction of Dual Error:** Under error projection (Step 8), specify: *"The JSON problem response body for config-orphans must include a machine-readable reject code. The API client must parse this code and return a wrapped client-side error that preserves the dual error identity, allowing `errors.Is(err, ErrRejectedByConfig)` to succeed on the client."*
3. **Formalize Unmaterialized Vector Output:** Under the `match_vectors[]` schema, specify: *"If a configured target is unmaterialized, the classifier must return a `configured-name` vector with an empty `candidate ID` and set `config_state.reserved_unmaterialized = true`, signaling to the surface adapter to short-circuit resolution and return Not Found."*

---

## Answers to Persona Questions

### 1. Does the classifier preserve direct bead ID, live session_name, live alias, named config, template target, path alias, not-found, and ambiguous cases with one precedence order?
**Answer:** Yes, the unified 8-step precedence order perfectly preserves these cases in a deterministic sequence. The unmaterialized named-session fall-through bug from Attempt 17 is resolved by step 3's explicit fallback matrix, which guarantees configured bare targets cannot fall through to live lookup on non-materializing paths.

### 2. How are overlapping alias, config name, and template name inputs represented without misrouting?
**Answer:** Overlapping inputs are represented as ordered entries inside the `match_vectors[]` schema, tagged with explicit `vector_kind` identifiers (e.g., `configured-name`, `live-session-name`). This allows the calling surface to inspect all potential matches, identify overlap, detect conflict groups, and return `ambiguous` or `rejected` results cleanly.

### 3. Which API, mail, assignee, and extmsg behaviors change when classification centralizes, and which tests catch each divergence?
**Answer:** In Slice 1, only the read-only API query endpoints are migrated, while mail, extmsg, and assignee remain "Characterization only". This temporary divergence is protected against regression by **anti-drift tests** (which compare match vectors and selected IDs between read-only and materializing modes) and **wire-parity/no-delta tests** (which assert identical HTTP response structures for `writeResolveError` and `humaResolveError`).
