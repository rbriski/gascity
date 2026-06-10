# Elias Sato

**Persona verdict:** block

**Sources:** Claude, Codex, DeepSeek V4 Flash. The current-iteration DeepSeek V4
Flash review (explicitly labeled "Iteration 21") is stored in
`02-required-core-loading-invariant-reviewer_gemini.md`; the file named
`02-required-core-loading-invariant-reviewer_deepseek.md` is a stale 2026-06-05
review of the earlier `assertRequiredSystemPackProvenance` design revision and
is used here only as corroborating historical signal, not as a current verdict
source. All current sources verdict `block`; the stale artifact also blocked.

**Consensus findings:**

- [Blocker] A live, behavior-driving production path resolves config with no
  Core and swallows the error, and no concrete inventory in the design names
  it. Claude and DeepSeek V4 Flash independently identify
  `internal/dispatch/control.go:832 loadAttemptRouteConfig`: it calls
  `config.LoadWithIncludes(fsys.OSFS{}, .../city.toml)` with no builtin
  includes (required Core omitted entirely) and returns `nil` on error
  (silent degradation), while driving attempt-route binding, pool routing,
  and assignee retry from five call sites (`control.go:424`,
  `control.go:1028`, `fanout.go:299`, `ralph.go:358`, `ralph.go:626`). The
  design names `internal/dispatch` only in scanner-scope prose; the only
  worked disposition table (attempt-4 "Required Core Loader Bypass
  Inventory") is `cmd/gc`-only. This is exactly the violation class the lane
  exists to kill, present in-tree today and unnamed.
- [Blocker] The Core Presence Doctor section contradicts the read-only doctor
  boundary. Codex and DeepSeek V4 Flash both find that the design first
  mandates plain `gc doctor` use only no-refresh/read-only APIs (no
  materializing loaders, repair helpers, or writers; ~L2109-2122), then
  instructs `cmd/gc/core_pack_doctor_check.go` to "Load resolved config
  through `internal/systempacks.LoadRuntimeCity`" (~L3055-3068) — a
  materializing, self-healing loader. Implementers get two incompatible
  contracts for the same check, and the concrete file-level instruction can
  silently repair missing/corrupt Core during report-only diagnostics,
  bypassing `doctor.MutationCoordinator` and hiding the corruption the check
  exists to expose.
- [Major, verdict-driving] The linchpin mechanism — resolver-produced,
  digest-keyed `RequiredSystemPackParticipation` — is asserted but never
  designed against the resolver that exists. Claude and DeepSeek V4 Flash
  both show the real resolver (`config.LoadWithIncludes`) returns
  `*Provenance` (`internal/config/compose.go:72-91`) keyed by binding name →
  source string with an `"(implicit)"` sentinel: no digests, no layer ids, no
  import-edge ids. The design forbids provenance-string proof but never
  specifies how `internal/config` is extended, which package owns the
  participation type (avoiding a `config → systempacks` import inversion), or
  the blast radius on existing `*Provenance` consumers. Both note the
  existing `Provenance` already carries `Sources` and
  `sourceContents map[string][]byte`, so digest-keyed participation can be
  built on it — but unless the design says so, implementation will degrade to
  exactly the string matching the design rejects. The stale DeepSeek artifact
  reached the same conclusion from the other direction: path/provenance
  presence cannot prove integrity or effective participation.
- [Major] The named enforcement model cannot carry the stated containment
  requirement. All three current sources converge: the bypass scanner is
  "modeled on `TestGCNonTestFilesStayOnWorkerBoundary`" — a single-directory
  substring scan with no alias or wrapper resolution — while the contract
  requires following aliases and wrappers across all production `internal/`
  and emitting a row for every call that can reach `config.Load`, including
  exported helpers returning `*config.City`. Claude: that needs a type-aware
  call-graph analyzer (`go/packages` + `go/types`) with a stated
  false-negative story, or an explicitly narrowed contract. Codex: the
  generator/test entrypoints and negative fixture list (aliases, wrapper
  methods, function values, package aliases, raw TOML decoders, hand-built
  include lists, no-refresh variants, newly exported loaders) are unnamed, so
  the default-deny promise can regress into a hand-maintained allowlist.
  DeepSeek V4 Flash: the scanner must be AST-based, not substring-based.
  Compounding this, the scanner contract repeatedly forbids a phantom symbol:
  `config.LoadCity` does not exist in the tree (Claude and DeepSeek V4 Flash
  both verified), direct evidence the inventory was authored from memory
  rather than generated — in tension with the "generated, default-deny,
  source-derived" guarantee.
- [Major] Required provider host-pack selection is underspecified and
  circular. Codex: the `RequiredPackPlan` prepass never specifies its inputs,
  fragment/include/env handling, malformed-partial-config classification, or
  what happens when prepass and final resolved provider disagree. Claude
  sharpens it into circularity: packs must be selected "after reading the
  final effective beads provider," but the final effective provider is only
  knowable after resolution, which needs the includes selection produces.
  Today's peek (`cmd/gc/providers.go:461-468
  configuredBeadsProviderValue`) is a raw top-level read that also honors a
  `GC_BEADS` env override mentioned nowhere in the document. The
  post-resolution gate as written validates only the packs that *were*
  selected, so a city missing its actual provider pack passes both fatal
  gates — reproducing the "degraded behavior detectable only later" shape
  this design exists to eliminate. The peek is also a partial read that
  drives runtime behavior, contradicting the partial-read allowlist contract.
- [Major] Runtime self-heal concurrency, read-only filesystems, and
  validation cost have no contract. DeepSeek V4 Flash: `LoadRuntimeCity`
  mutates `.gc/system/packs/` with no synchronization, so concurrent `gc`
  invocations (hook storms, cron loops, API requests) racing repair can
  collide and corrupt state; on read-only/immutable filesystems (k8s
  read-only root) fail-closed becomes permanent lockout even though good
  bytes are embedded in the binary. Claude corroborates: no section states
  which advisory lock the runtime repair path takes or what a contending load
  does (wait / fail closed / validate-only); "read-only city" appears in the
  broken-state matrix with no defined required outcome per loader class; and
  strict full file-set digest validation on every load has no cost/
  amortization story, inviting ad-hoc weakening under latency pressure.
- [Minor] The attempt-4 call-site inventory materially understates the live
  surface. Claude counts 32 direct `config.Load*` sites in non-test `cmd/gc`
  spanning files absent from the table (`cmd_agent.go` — host of the de facto
  common load helpers — `cmd_convoy.go`, `controller.go:904`, `apiroute.go`,
  `api_state.go:767`, `cmd_register.go:105`, `doctor_v2_checks.go`), plus
  `internal/dispatch/control.go:836`, `internal/doctor/checks.go` (×4), and
  `internal/configedit/configedit.go` (×3) — slice sizing from the named
  table underestimates the migration roughly 2-3x. The stale DeepSeek
  artifact corroborates the same class of gap from the earlier revision (raw
  `config.Load` surface, `effectiveCityName` materialize-but-not-include,
  unclassified no-refresh completion/stop callers, divergent include-builder
  families including `tryReloadConfig`) — those classification demands remain
  live against the current text.
- [Minor] A second production `GC_BOOTSTRAP` consumer is unaddressed
  (Claude): `internal/doctor/implicit_import_cache_check.go:236-245`
  unsets/restores `GC_BOOTSTRAP` around its check, while the Bootstrap
  Cleanup section treats only `internal/bootstrap/bootstrap.go:72`.
  Retirement must name this site so the doctor check's semantics are
  consciously preserved or changed.

**Disagreements:**

- No verdict disagreement: all three current sources block, and the stale
  artifact also blocked. The blocks are narrow and convergent — contracts
  judged right, bridge-to-code missing at the load-bearing points.
- Scanner remedy depth. Claude requires a type-aware call-graph analysis (or
  an explicitly narrowed direct-reference contract plus curated wrapper
  allowlist); DeepSeek V4 Flash asks for AST parsing of direct calls plus a
  corrected symbol list; Codex focuses on named generator/test artifacts and
  negative fixtures. Assessment: complementary, not contradictory. Adopt
  Claude's two-mechanism split — (a) AST-feasible direct-reference denial,
  (b) an explicit wrapper/reachability mechanism with a stated
  false-negative story — and apply Codex's named-artifact requirement to
  both.
- Runtime repair policy. DeepSeek V4 Flash prescribes mechanisms: a fast
  short-timeout file lock around mutative materialization and an
  embedded-VFS fallback when the filesystem is read-only. Claude
  deliberately does not prescribe — it requires the design to choose the
  behavior per loader class (wait / fail closed / validate-only) and define
  read-only outcomes. Assessment: the design must first decide whether
  ordinary runtime loads repair at all; if repair stays, a named lock and an
  explicit read-only-filesystem outcome become contract elements, but the
  embedded-VFS fallback is a real semantic change (resolved config served
  from memory while disk diverges) that must be an explicit design decision,
  not a synthesis default. If runtime loads instead fail closed and delegate
  repair to `gc doctor --fix`, the prescribed mechanisms become moot.
- Doctor final validation. Codex permits `LoadRuntimeCity` as post-`--fix`
  final validation under `doctor.MutationCoordinator`; DeepSeek V4 Flash
  states the doctor check must call `LoadRuntimeCityNoRefresh` and does not
  address the post-fix path. Assessment: compatible once scoped — plain
  `gc doctor` is no-refresh/read-only only; materializing validation is
  reachable solely through the explicit fix path under the coordinator.
- Forbidden-symbol scope. DeepSeek V4 Flash additionally wants
  `config.LoadRootPackDefaultRigImports` on the forbidden list; Claude
  enumerates `config.Load`, `config.LoadWithIncludes`,
  `config.LoadWithIncludesOptions`. Assessment: derive the symbol set from
  the source tree (that is the point of the freshness test) rather than
  litigating the list by hand; include every exported resolver entrypoint
  that returns config or contributes includes.
- Artifact hygiene worth surfacing to the workflow: the third-reviewer
  artifact for this iteration was written to the `_gemini.md` filename slot
  while a stale `_deepseek.md` from 2026-06-05 (reviewing a superseded design
  revision) remains in the same directory. A synthesizer that keyed on
  filenames alone would have merged a review of the wrong document.

**Missing evidence:**

- The `internal/config` resolver API change producing
  `RequiredSystemPackParticipation`: owner package, fields, how layer/edge
  ids and file-set digests derive from or alongside the existing
  `*Provenance` (`Sources`, `sourceContents`), blast radius on existing
  consumers (`gc config explain`, import-state doctor), and a worked example
  showing a materialized-but-shadowed (or overridden-to-empty) required pack
  reported as non-participating.
- A concrete loader disposition for the behavior-driving `internal/` sites
  that exist now — at minimum `internal/dispatch/control.go:
  loadAttemptRouteConfig` and its five call sites, the `internal/doctor`
  callers, and `internal/configedit` — rather than total deferral to the
  not-yet-existing `loader-inventory.generated.yaml`.
- The scanner/generator analysis mechanism with a false-negative story (or an
  explicit narrowing of the reachability claim), plus named generator
  command/package, schema test, freshness test, and the negative fixture
  suite.
- The `RequiredPackPlan` algorithm: exact prepass inputs; handling of layered
  city/rig config, fragments/includes, env overrides, missing or malformed
  TOML; which diagnostics are fatal; and prepass-vs-final mismatch handling.
- Any treatment of `GC_BEADS` / `GC_BEADS_SCOPE_ROOT` influence on required
  host-pack selection, and how typed participation represents an
  env-overridden provider — including whether `GC_BEADS` survives the
  migration at all.
- Whether `LoadRuntimeCityNoRefresh` returns a participation record for an
  already-invalid tree or only structured diagnostics without a resolved
  config (Codex), and the authoritative comparison proving already-
  materialized on-disk Core matches the embedded manifest digest for
  no-refresh loads, given a long-lived controller may hold in-memory config
  across an on-disk corruption (Claude).
- A defined outcome per loader class for read-only city directories, the
  named concurrency protocol for runtime-path repair, and a measurement,
  budget, or caching policy for per-load strict integrity validation.
- The intended operator flow when a binary upgrade makes a running
  controller's no-refresh reload see "stale" Core (content matches the old
  binary's manifest) and fail closed — pinned by a test so the disruption is
  not later fixed by weakening the stale check.

**Required changes:**

- Name `internal/dispatch/control.go:loadAttemptRouteConfig` and its five
  call sites in a concrete disposition inventory with a target loader class,
  and state explicitly that both current behaviors — omitting required
  includes and swallowing the load error — are violations the Core loading
  slice must convert to wrapper-routed, fail-loud loads.
- Rewrite the Core Presence Doctor section so plain `gc doctor` uses only
  read-only/no-refresh APIs (`ValidateRequiredFileSetsNoRefresh`,
  `LoadRuntimeCityNoRefresh`, raw import parsing, classifiers) and cannot
  materialize, repair, promote cache entries, prune/quarantine, or rewrite
  imports; reserve staged repair and `LoadRuntimeCity` final validation for
  `gc doctor --fix` under `doctor.MutationCoordinator`.
- Add a "Resolver Participation API" subsection to System Pack Loading
  specifying how digest-keyed import-edge participation is emitted from
  resolution (extending or wrapping the existing `*Provenance`), which
  package owns the type so there is no `config → systempacks` import
  inversion, the blast radius on existing `*Provenance` consumers, and a
  test asserting a materialized-but-shadowed required pack fails
  post-resolution.
- Close the provider-selection circularity with a post-resolution consistency
  gate: recompute the required host-pack set from the resolved effective
  config and require a participation record for every pack in the recomputed
  set, any mismatch with the pre-resolution `RequiredPackPlan` fatal. Record
  the provider-value source (raw peek vs `GC_BEADS` override), decide whether
  `GC_BEADS` survives the migration, give the peek an honest loader-inventory
  classification, and add the full `RequiredPackPlan` algorithm with tests
  (root config, rig config, fragments/includes, env overrides, malformed
  partial config, `bd`, `dolt`, exec/no-provider, stale retired sources named
  like provider packs).
- Split loader-bypass enforcement into two named mechanisms: (a) deny direct
  `config.Load*` references outside `internal/systempacks` plus the generated
  allowlist (substring/AST-feasible), and (b) a type-aware call-graph check —
  or an explicit curated-wrapper rule — preventing wrappers and allowlisted
  partial helpers from being reused on behavior paths. Stop citing the
  single-directory substring worker-boundary test as the model for the
  whole-production reachability requirement. Name the generator entrypoint,
  schema test, freshness test, and negative fixture suite; stale rows and
  newly exported config-returning helpers fail CI by default.
- Correct the scanner symbol set: drop the phantom `config.LoadCity`,
  enumerate the real entrypoints (`config.Load`, `config.LoadWithIncludes`,
  `config.LoadWithIncludesOptions`; assess
  `config.LoadRootPackDefaultRigImports`), and add a freshness test asserting
  the inventory references only loader symbols that exist in the tree.
  Restamp the attempt-4 table as a non-exhaustive illustration subordinate to
  the generated inventory.
- Specify the runtime repair policy explicitly: either ordinary runtime loads
  fail closed and delegate all mutation to `gc doctor --fix`, or — if
  self-heal remains — name the advisory lock and contention behavior per
  loader class, define the read-only-filesystem and read-only-city outcome
  per loader class (treating any embedded-memory fallback as an explicit
  decision), and extend failure-injection fixtures to concurrent runtime
  repairs. Name the permitted integrity-validation amortization (e.g.,
  validated-freshness cache keyed by binary build id, doctor/repair always
  full) or explicitly accept per-invocation full-digest cost with a budget.
- Name `internal/doctor/implicit_import_cache_check.go` in the `GC_BOOTSTRAP`
  retirement so its semantics are consciously preserved or changed.
