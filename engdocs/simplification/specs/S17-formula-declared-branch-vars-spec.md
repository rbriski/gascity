# S17 — Formula-declared base_branch/target_branch vars (remove hardcoded pack names)

Backlog: `engdocs/simplification/backlog.json` id `S17` (approach **a**, generalized with a declared-var predicate).
Subsystem: `internal/sling/sling.go` (+ call sites in `internal/sling/sling_core.go`), tests in `cmd/gc/cmd_sling_test.go` and `internal/sling/sling_test.go`.

## Target design

**Principle.** Branch-var injection is keyed off the formula's **declared var table**, not its name. Any compiled formula whose resolved var table declares `base_branch` receives the auto-resolved branch as `base_branch`; any that declares `target_branch` receives it as `target_branch`. `SlingFormulaUsesBaseBranch` and `SlingFormulaUsesTargetBranch` and their hardcoded name lists (`mol-polecat-` prefix, `mol-scoped-work`, `mol-refinery-patrol`) are **deleted**.

**Mechanism.**

1. Add a lightweight declared-var loader to `internal/formula` (no full compile):

   ```go
   // DeclaredVars returns the inheritance-resolved var table of a formula
   // (LoadByName + Resolve only — no control flow, expansion, or template
   // work). Returns nil, err if the formula cannot be located or parsed.
   func DeclaredVars(name string, searchPaths []string) (map[string]*VarDef, error)
   ```

   Implementation is stage 1+2 of `compileFormula` (`internal/formula/compile.go:66-79`): `NewParser(searchPaths...).SetSource(SourceFromEnv())`, `LoadByName(name)`, `parser.Resolve(f)`, return `resolved.Vars`. `parser.Resolve` already merges parent `Vars` across `extends` chains (parser.go:335-337 parent fill, 353-354 child override), so heirs like `mol-polecat-report`/`mol-polecat-commit` see `base_branch` inherited from `mol-polecat-base`.

2. In `buildSlingFormulaVars` (`internal/sling/sling.go:1061`), replace the name checks at lines 1093-1099 with a declaration check. `buildSlingFormulaVars` already receives everything needed to compute search paths — `deps` and `a` — via `SlingFormulaSearchPaths(deps, a)`, so **no call-site signature changes** in `sling_core.go`:

   ```go
   declared, err := formula.DeclaredVars(formulaName, SlingFormulaSearchPaths(deps, a))
   if err == nil {
       _, wantsBase := declared["base_branch"]
       _, wantsTarget := declared["target_branch"]
       if wantsBase || wantsTarget {
           autoBranch := SlingFormulaTargetBranch(beadID, deps, a)
           if wantsBase {
               addVar("base_branch", autoBranch)
           }
           if wantsTarget {
               addVar("target_branch", autoBranch)
           }
       }
   }
   ```

   On `DeclaredVars` error (formula missing/unparsable) injection is skipped silently: every production caller of `buildSlingFormulaVars` compiles or `PrepareInvocation`s the same formula immediately afterward and surfaces the real load error there (see site enumeration below), so no error is swallowed on any successful launch path.

3. Precedence is untouched: `addVar` still skips explicitly-set keys, so the documented order (`--var` > `rig.formula_vars` > routing-injected defaults > formula-level `[vars.*].default`) holds verbatim. The only change is *which formulas* qualify for the routing-injected `base_branch`/`target_branch` fill.

4. `SlingFormulaTargetBranch` (resolution order: bead `metadata.target` → rig `DefaultBranch` → live git probe) is unchanged. Today it runs unconditionally before the name checks; the new form may compute it lazily (only when a branch var is declared and not already set) — a strict reduction in git-probe work, no observable delta for launches that inject.

**Non-goals.** `defaultGitHubRepairWorkflow = "mol-polecat-work"` (`internal/config/github_pr_monitor.go:17`) is a separate hardcoded-name wart (a config *default value*, not an injection predicate) and is out of scope for S17. Approach (b) (spec-level `auto_branch` flag) and (c) (name lists in config) are rejected per backlog tradeoffs — (a) is the cleanest end state and needs no formula-spec version bump.

## Current behavior (site-by-site enumeration)

### The two matchers (to be deleted)

- `internal/sling/sling.go:965-969` — `SlingFormulaUsesBaseBranch(name)`: `strings.HasPrefix(name, "mol-polecat-") || name == "mol-scoped-work"`.
- `internal/sling/sling.go:971-975` — `SlingFormulaUsesTargetBranch(name)`: `name == "mol-refinery-patrol"`.
- Sole production consumers: `sling.go:1094` and `sling.go:1097` inside `buildSlingFormulaVars`. Repo-wide grep confirms **no other non-test caller** of either function; deletion breaks nothing else in-module (both are `internal/`, so no external Go consumers exist).

### buildSlingFormulaVars call sites (all in internal/sling, all compile after building vars)

Every site builds vars **first**, then compiles/instantiates the same formula name — this ordering is why the new predicate needs the standalone `DeclaredVars` loader rather than the compiled `Recipe.Vars` (recipe.go:32-34 does carry the resolved var table, but only after vars were already needed as compile input):

1. `sling_core.go:249` (`slingFormula`, `--formula` path): vars → `CompileWithoutRuntimeVarValidation` at :253. Load failure surfaces at :253.
2. `sling_core.go:298` (`slingOnFormula`, `--on` path): vars → `prepareGraphV2FormulaInvocation`/`InstantiateSlingFormula`. Load failure surfaces there.
3. `sling_core.go:410` (`slingDefaultFormula`, agent default-formula path): same shape as (2) with `a.EffectiveDefaultSlingFormula()`.
4. `sling_core.go:1075` (`attachBatchFormula`, per-batch-child): vars → `InstantiateSlingFormula`. isGraph precomputed by the caller; load failure surfaces in instantiate.
5. `sling_core.go:1137` (`prepareGraphV2FormulaInvocation` via `buildGraphV2SlingFormulaVars`, `includeIssue=false`): vars → `graphv2.PrepareInvocation` (which loads the formula). Same-name load, same failure surface.
6. `sling_core.go:1170` (`validateBatchSlingFormulaRuntimeVars`, per-child pre-validation): vars → `validateSlingFormulaRuntimeVars` → compile.
7. `sling_core.go:1323` (`DoSlingBatch` container path): vars → `isGraphSlingFormula` → compile.
8. `cmd/gc/cmd_sling.go:1137` — thin wrapper delegating to `sling.BuildSlingFormulaVars`; no logic, unaffected.

Sites 1-3 and 5 run once per launch; 4/6 run once per batch child. Adding one `LoadByName+Resolve` per call is bounded by the compiles these paths already perform (each compile itself does LoadByName+Resolve internally).

### Formulas currently matched by name — declaration audit (the correctness-critical inventory)

**In-repo (core pack, `internal/bootstrap/packs/core/formulas/`):**

| Formula | Matched by | Declares the var today? | Post-change |
|---|---|---|---|
| `mol-scoped-work` | base_branch (exact name) | **YES** — `[vars.base_branch]` at mol-scoped-work.toml:20 (default "main") | injection preserved, no pack change needed |
| `mol-polecat-base` | base_branch (prefix) | **YES** — `[vars.base_branch]` at mol-polecat-base.toml:35 (default "main") | preserved |
| `mol-polecat-report` | base_branch (prefix) | **YES via `extends = ["mol-polecat-base"]`** (mol-polecat-report.toml:29; Resolve merges parent Vars) | preserved |
| `mol-polecat-commit` | base_branch (prefix) | **YES via `extends = ["mol-polecat-base"]`** (mol-polecat-commit.toml:29; body uses `{{base_branch}}` throughout) | preserved |

**NOT in this repo (external Gas Town pack):**

| Formula | Matched by | Where it lives |
|---|---|---|
| `mol-polecat-work` | base_branch (prefix) | **External** — no TOML in this repo. `internal/bootstrap/packs/core/formulas/mol-do-work.toml:11` says explicitly: "or mol-polecat-work from the gastown pack". Referenced by name from Go as `defaultGitHubRepairWorkflow` (github_pr_monitor.go:17) and in CLI help text (cmd_convoy.go:1079, cmd_formula.go:102). |
| `mol-polecat-arm` (and any other `mol-polecat-*`) | base_branch (prefix) | **External** — mentioned only in a doc comment (internal/formula/types.go:755); shipped by the gastown pack. |
| `mol-refinery-patrol` | target_branch (exact name) | **External** — zero TOML hits in this repo; exists only in Go string literals and tests. Shipped by the gastown pack. |

**Consequence (stated explicitly per the S17 contract):** the `target_branch` matcher and part of the `base_branch` matcher exist *solely* for formulas defined outside this repo. This repo cannot prove those external formulas declare `base_branch`/`target_branch` in their var tables. Therefore **the Go change MUST NOT land until the gastown pack (github.com/steveyegge/gastown, or wherever the deployed pack is vendored) is audited and, where missing, patched to declare `[vars.base_branch]` (mol-polecat-work, mol-polecat-arm, every other mol-polecat-*) and `[vars.target_branch]` (mol-refinery-patrol)** — otherwise those launches silently lose branch injection and fall back to formula defaults (or unresolved placeholders), producing wrong-branch worktrees with no error.

## Invariants — the correctness contract

1. **Var-parity for previously-matched formulas.** For every formula matched by the old name predicates that declares the corresponding var (directly or via `extends`), the final vars map produced by `buildSlingFormulaVars` is **byte-identical** before and after the change, for all combinations of {bead `metadata.target` set/unset, rig `DefaultBranch` set/unset, live-probe available/absent, explicit `--var` present/absent, rig `formula_vars` present/absent}. This is the primary regression gate.
2. **Explicit `--var` always wins.** `addVar` skip semantics unchanged: an explicit `base_branch`/`target_branch` in `userVars` or `rig.formula_vars` is never overwritten by injection. (Precedence doc at sling.go:1050-1052 stays literally true.)
3. **No launch may silently lose injection.** A formula that received `base_branch`/`target_branch` injection under the name predicates must still receive it — enforced by the pack-declaration audit (in-repo: complete, all four declare; external: landing precondition). "Declares the var" means: present in `parser.Resolve`-merged `Vars`, i.e. own `[vars.X]` table **or inherited through any `extends` chain**.
4. **Injection never errors a launch that previously succeeded.** `DeclaredVars` failure (missing/unparsable formula) degrades to no-injection; the subsequent compile at the same call site reports the authoritative error. No new error return is added to `buildSlingFormulaVars`.
5. **Empty auto-branch stays a no-op.** When `SlingFormulaTargetBranch` resolves to `""`, `addVar` drops it (value=="" guard at sling.go:1071) — exactly as today. A declared var with no resolvable branch falls through to its formula default at instantiation.
6. **Intentional generalization (documented delta, not a regression):** a formula *not* named like Gas Town's that declares `base_branch`/`target_branch` now *gains* injection, and the auto-resolved branch outranks its formula-level default (routing-injected > formula default, per existing precedence). This is the feature. It is a behavior delta only for packs that declared these exact var names, relied on the formula default, *and* have a rig/probe that resolves a different branch — see risk #2.
7. **Zero hardcoded roles (strengthened).** After this change, `grep -rn "polecat\|refinery\|scoped-work" internal/sling/*.go` (non-test) returns nothing. Pack vocabulary exits SDK Go.
8. **Repo-wide invariants preserved:** no wire/event/schema changes (nothing in `internal/api`, `internal/events`, OpenAPI, or dashboard types is touched); no session-lifecycle paths touched (worker boundary intact); change confined to `internal/formula` (new pure loader) + `internal/sling` (predicate swap) — no upward imports (`sling` already imports `formula`); `cmd/gc` remains a projection (its wrapper at cmd_sling.go:1137 is unchanged); no `config.Agent`/`config.Rig` fields added (field-sync rules not implicated).

## Behavior-preserving migration/staging

**Stage 0 — external pack audit (BLOCKING precondition, outside this repo).**
Audit the gastown pack for every formula named `mol-polecat-*` and `mol-refinery-patrol`. For each, verify the resolved var table declares the matched var; where missing, land a pack change adding `[vars.base_branch]` (polecat family) / `[vars.target_branch]` (refinery-patrol) with the description and current formula-default semantics. Only after the deployed pack versions carry these declarations may Stage 2 land. If deployed cities pin older pack versions, the Go change must wait for those pins to advance (or the pack change must be backported to the pinned versions). Record the audited pack commit in the S17 PR description.

**Stage 1 — this repo, additive only (safe to land immediately).**
- Add `formula.DeclaredVars` + unit tests (own decl, `extends`-inherited decl, missing formula → error, override in child).
- Add the var-parity test harness (below) that exercises *both* the old name predicate and the new declaration predicate against real core-pack formula files and asserts identical vars maps. This proves parity before the cutover commit.

**Stage 2 — the cutover (single commit, this repo).**
- Swap the predicate in `buildSlingFormulaVars`; delete `SlingFormulaUsesBaseBranch` / `SlingFormulaUsesTargetBranch`.
- Migrate the ~20 existing tests in `cmd/gc/cmd_sling_test.go:7807-8300` and `internal/sling/sling_test.go:1164-1230` that call `buildSlingFormulaVars` with bare formula-name strings (`"mol-polecat-work"`, `"mol-refinery-patrol"`) and **no formula file on disk**: under the new predicate these get no injection, so each test must stage a minimal formula TOML declaring the relevant var into a `t.TempDir()` search path wired through `deps` (or switch to a fixture name the core pack ships, e.g. `mol-scoped-work`). This is a test-infrastructure change, not a semantics change — the assertions themselves stay identical.
- Grep sweep per no-semantic-search rule: `SlingFormulaUsesBaseBranch`, `SlingFormulaUsesTargetBranch`, `mol-polecat-`, `mol-scoped-work`, `mol-refinery-patrol` across Go, docs, and templates; update `engdocs/` prose that documents name-based injection (the precedence comment at sling.go:1050 needs no change; any doc describing "polecat formulas get base_branch" changes to "formulas declaring base_branch get it").

**No feature flag, no dual-predicate transition period in Go.** The transition window lives in Stage 0 (packs declare vars while name-matching still runs — declarations are inert additions under the old code). This ordering makes both intermediate states safe: packs-with-declarations + old Go = unchanged behavior (name match still fires); packs-with-declarations + new Go = injection via declaration. The only unsafe state — new Go + packs without declarations — is excluded by the Stage 0 gate.

**Rollback:** revert the Stage 2 commit; pack declarations are harmless under old Go.

## Test plan (incl. -race/parity if applicable)

**1. Var-parity table test (the core gate, lands in Stage 1, kept after cutover).**
For every previously-matched in-repo formula — `mol-scoped-work`, `mol-polecat-base`, `mol-polecat-report`, `mol-polecat-commit` (real files from `internal/bootstrap/packs/core/formulas/` staged into the test search path) — assert the post-change `buildSlingFormulaVars` output equals the pre-change output (golden maps captured from the name-predicate implementation) across the resolution matrix: bead `metadata.target` set / rig `DefaultBranch` set / live probe only / nothing resolvable (empty → var absent). Include one `extends` heir explicitly to lock the inheritance-merge path.

**2. Explicit-override precedence tests (extend existing).**
- `--var base_branch=X` with a declaring formula → X survives (existing `TestBuildSlingFormulaVarsPreservesExplicitValues` re-pointed at a real declaring fixture).
- `rig.formula_vars.base_branch` → wins over injection (existing `TestBuildSlingFormulaVarsRigDefaults` cases).
- Same pair for `target_branch` with a declaring fixture.

**3. New-behavior tests.**
- Formula declaring `base_branch` with a name matching *nothing* Gas-Town-like (e.g. `my-pack-flow`) → injection fires (the generalization works; this is the zero-hardcoded-roles proof).
- Formula declaring **neither** var → no `base_branch`/`target_branch` key in output, and (via a counting `Branches` fake) **no live git probe** if the lazy-compute option is taken.
- Formula declaring both vars → both injected with the same auto-branch.
- Formula name that doesn't resolve to any file → no injection, no panic, remaining vars (issue/rig_name/binding_*) still populated.
- `[vars.base_branch]` declared with a default + no explicit var + resolvable rig branch → rig branch wins over formula default (documents invariant 6).

**4. `formula.DeclaredVars` unit tests** (`internal/formula/`): own declaration; inherited via single and chained `extends`; child override of parent VarDef; missing formula → error; malformed TOML → error; search-path layering (highest-priority layer wins, matching `Resolve` semantics).

**5. Deletion sweep test.** Adapt or add a lint-style test asserting `internal/sling` non-test sources contain none of: `mol-polecat`, `mol-scoped-work`, `mol-refinery-patrol` (mirrors existing invariant-grep tests in this repo; keeps the wart from regressing).

**6. Integration smoke.** One end-to-end sling of `mol-scoped-work` through `slingFormula` (existing test infra) asserting the instantiated molecule's rendered step bodies contain the resolved branch, not the `main` default — proves injection reaches instantiation, not just the vars map.

**7. Race/regression sweeps.** `go test -race ./internal/sling/... ./internal/formula/... ./cmd/gc/` (buildSlingFormulaVars is called concurrently from batch paths; `DeclaredVars` constructs a fresh Parser per call — no shared state, but -race confirms). Then `make test` (fast baseline) + `go vet ./...` per repo gates. No dashboard/API surfaces touched, so `make dashboard-check` is not implicated.

## Top correctness risks

1. **External-pack silent injection loss (highest).** `mol-polecat-work`, `mol-polecat-arm`, and `mol-refinery-patrol` are *not in this repo*; if the deployed gastown pack versions don't declare the vars when the Go change ships, those launches lose branch injection with **no error** — steps render the formula default (`main`) or leave `{{target_branch}}` unresolved, producing wrong-branch worktrees/patrols. Mitigation: Stage 0 blocking audit + recorded pack commit; the unsafe state is unreachable if the landing order is respected.
2. **Unintended injection gain flipping a formula default.** Any existing pack formula that already declares `[vars.base_branch]`/`[vars.target_branch]`, was never name-matched, and depended on its *formula-level default* now gets the rig/probe-resolved branch instead (routing-injected > formula default). In-repo audit shows no such formula (only the four matched ones declare these names), but third-party packs can't be enumerated. Mitigation: call the delta out in release notes; the escape hatch is explicit `--var`/`rig.formula_vars`, which still win.
3. **`extends`-resolution divergence.** Parity for `mol-polecat-report`/`mol-polecat-commit` depends on `parser.Resolve` merging parent `Vars` — if `DeclaredVars` were implemented with `LoadByName` only (skipping `Resolve`), heirs would silently lose injection. Locked by the dedicated inheritance tests in plans 1 and 4.
4. **Search-path skew.** `DeclaredVars` must resolve the formula through the *same* `SlingFormulaSearchPaths(deps, a)` + `SourceFromEnv()` pipeline the subsequent compile uses; a different path set could check a different layer's var table than the one compiled (last-wins layering). Locked by reusing the exact parser construction from `compileFormula` and the layering test in plan 4.
5. **Test-fixture illusion.** The ~20 existing tests pass bare names with no formula files; naively migrating them by adding declarations to fixtures *changes what the tests prove*. The parity harness (plan 1) against real core-pack files is the safeguard that the production formulas — not just fixtures — keep their injection.
