# Hiroshi Tanabe - DeepSeek V4 Flash

**Verdict:** block

Lane: production Core embed removal, non-nil empty bootstrap fs, fixture-only tests, GC_BOOTSTRAP skip containment, hidden dependency inventory. Reviewed `plans/core-gastown-pack-migration/implementation-plan.md` against `requirements.md` and the `gc.mayor.implementation-plan.v1` schema.

While the design makes commendable structural changes toward emptying the legacy bootstrap directory and removing embeds, several critical inconsistencies in rollout scheduling, silent dependency leaks, and a lack of precise verification mechanisms block approval. The current plan relies too heavily on static assertions without establishing runtime backstops or reconciling timeline contradictions that expose the city to duplicate Core collisions in intermediate slices.

---

## Top Strengths

- **Structural Removal of Embeds and Non-Nil ErrNotExist FS:** The plan avoids the hazard of a nil `fs.FS` or `AssetDir` panic by replacing the legacy embed with a private, non-nil, explicitly erroring `fs.FS` (lines 506-508, 910-912, 1427-1428) which returns `fs.ErrNotExist` for any read. This safely satisfies the "non-nil empty bootstrap fs" requirement and is verified via `TestProductionBootstrapAssetsIsEmpty` (lines 528, 916, 2512).
- **Test-Only Containment of Skip Semantics:** Retiring `GC_BOOTSTRAP=skip` as a production switch and narrowing its semantics strictly to empty legacy bootstrap fixture materialization (lines 533-535, 1715-1717) ensures that operators cannot use it as a backdoor to bypass required Core loading. 
- **Hidden Dependency Discovery and Old-Path Guarding:** The plan rightly identifies the direct import of `internal/bootstrap/packs/core` in `internal/hooks/hooks.go` (lines 2062-2065), mandates its relocation to `internal/packs/core`, and sets up a string-path search guard over test/doc files to detect lingering references (lines 268-270, 920-921).

---

## Critical Risks

### 1. [Blocker] Rollout Contradiction & Collision Gate Vulnerability in Slice 3
The design contains a severe scheduling conflict regarding when legacy implicit imports are disabled, which opens a critical collision vulnerability:
- **Contradiction:** Attempt 10 (line 1721), Attempt 4 (lines 520-521), and the "Bootstrap Cleanup" section (line 2523) all assign the removal of `"core"` and `"registry"` from `bootstrapManagedImportNames` to **Slice 3 (Core Extraction)**.
- However, Attempt 11 (lines 1903-1904) and the disposition table in Attempt 8 (line 1143) state that `bootstrapManagedImportNames` can *only* be emptied **after** required-Core collision enforcement and fatal gates are live, which occurs in **Slice 4 (Core Loading/Doctor)**.
- **Impact:** If `bootstrapManagedImportNames` is emptied in Slice 3 before the new `internal/systempacks` fatal gates are live in Slice 4, the existing composer-level collision gates (`internal/config/compose.go:827-846`) will ignore implicit `core` and `registry` imports. During this intermediate slice, the system will allow duplicate Core definitions and shadowing to land unchallenged—violating the core invariant that every intermediate and rollback state must remain "fatal to collisions".

### 2. [Major] Unhandled Production Env-Mutation Consumer of `GC_BOOTSTRAP`
The retirement inventory for `GC_BOOTSTRAP` is incomplete. It completely overlooks `internal/doctor/implicit_import_cache_check.go:235-249`, where `ensureBootstrapForDoctor` actively unsets, saves, and restores `GC_BOOTSTRAP` around a direct call to `bootstrap.EnsureBootstrap`. 
- Once `BootstrapPacks` is permanently empty and production skip behavior is deleted, this function remains in production code as a vestigial, dead env-var mutation wrapper around a no-op.
- Leaving this active in the doctor check can confuse maintenance utilities and operators, and the design fails to name this call site or specify a clear disposition (e.g., routing through `internal/systempacks` or deleting the check).

### 3. [Major] Test Harness Masking via Default `GC_BOOTSTRAP=skip`
A major risk is that the test suite itself will mask broken skip containment:
- `cmd/gc/main_test.go` sets `GC_BOOTSTRAP="skip"` as a default in both `configureTestscriptEnvDefaults()` (line 45) and `configureIsolatedRuntimeEnv()` (line 62).
- If tests default to skipping bootstrap, and there is no explicit command-line test enforcing that an execution with `GC_BOOTSTRAP=skip` still triggers full Core materialization and participation tracking, the suite can remain green while production skip behavior silently continues to bypass validation.

### 4. [Minor] Non-Existent Verification Mechanism for Fixture-Minimality
The plan specifies that bootstrap test fixtures must not contain copies of production Core (line 511), yet it lacks an actual mechanism to enforce this:
- Attempt 4 (line 529) defines `TestBootstrapFixtureIsMinimal` as a simple directory denylist. A denylist is easily bypassed if a test script or helper copies the *contents* of a production `pack.toml` (such as bindings or agent definitions) under an allowed or inline filename.
- There is no test specified that compares fixture content digests against production `internal/packs/core` digests to enforce true structural isolation.

---

## Missing Evidence

- **Comprehensive Reference Inventory:** No generated inventory is provided for references to `internal/bootstrap/packs/core` in tests. Specifically, `internal/remotesource/remotesource_test.go` (which pins the old subpath fixture) and `internal/bootstrap/collision_test.go` are omitted from the list of files to update.
- **No-Bypass Verification Test:** The plan asserts that a normal command run with `GC_BOOTSTRAP=skip` will still materialize and validate Core (line 923), but fails to define the exact test script or assertion checking for the `RequiredSystemPackParticipation` typed record under this condition.
- **Bootstrap Package End-State:** The plan describes the end state of `cmd/gc/embed_builtin_packs.go` but fails to specify what happens to the rest of the `internal/bootstrap` package once all materialization logic is obsolete.

---

## Required Changes

1. **Reconcile `bootstrapManagedImportNames` Scheduling:** Keep `"core"` and `"registry"` in `bootstrapManagedImportNames` until **Slice 4**, when the new collision/fatal gates are fully live in `internal/systempacks`. Update all conflicting sections (Attempt 4, Attempt 10, and Bootstrap Cleanup) to reflect this ordering.
2. **Dispose of Doctor Env-Mutation:** Include `internal/doctor/implicit_import_cache_check.go` in the `GC_BOOTSTRAP` retirement inventory. Rewrite or remove `ensureBootstrapForDoctor` so it no longer performs env-var manipulation or direct `EnsureBootstrap` calls, and route any surviving cache checks through `internal/systempacks`.
3. **Upgrade Fixture Minimality to an Exact Allowlist & Hash Check:** Redefine `TestBootstrapFixtureIsMinimal` to enforce an exact allowlist of permitted files (`pack.toml` + one minimal agent table) and add a digest-inequality check comparing fixture hashes against the production Core `internal/packs/core` digests to detect copied content.
4. **Authoritative Generated Path-String Guard:** Expand the path-string guard to cover and resolve `internal/remotesource/remotesource_test.go` and `internal/bootstrap/collision_test.go`.
5. **Define `internal/bootstrap` Post-Migration End-State:** Document whether the empty materialization machinery in `internal/bootstrap` will be permanently deleted once `RetiredBootstrapPacks` classification moves to `internal/packsource`, or explain why retaining empty vestigial scaffolding is necessary.

---

## Questions

1. Once legacy implicit imports are disabled and collision checks are driven by `internal/systempacks`, does the implicit-import cache doctor check have any ongoing utility, or should it be retired entirely in the Core extraction slice?
2. Is there any production path where `bootstrap.EnsureBootstrap` is still expected to perform a non-empty, functional operation after Slice 3? If not, why is the package itself preserved instead of deleted?
3. For the remote source pin `gascity.git//internal/bootstrap/packs/core#main` in `internal/remotesource_test.go`, should older lockfiles referencing this old path be rejected with an upgrade diagnostic, or must historical compatibility be maintained?
