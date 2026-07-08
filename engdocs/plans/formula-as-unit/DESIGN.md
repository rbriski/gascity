# Formula as the Unit of Work in Gas City

**Status:** design + shipped Slice 1/2a + a **verified end-to-end demo** (see below)
**Date:** 2026-07-08
**Method:** 20-agent workflow — 9 Opus explorers (architecture, source-grounded) → 6 Fable designers → 4 Fable red-teamers. Raw agent outputs in `_raw/`.

---

## 0. Verified end-to-end (2026-07-08)

A real formula was driven to completion in a **transient, supervisor-isolated** city, with a real provider in the loop — the true e2e the `gc run` execution slice productizes:

```
$ bash engdocs/plans/formula-as-unit/demo/oneshot-e2e-demo.sh
1/5 manufacture isolated Dolt city (no supervisor)…
2/5 start the STANDALONE controller…
3/5 sling mol-do-work (1-member convoy)…   workflow root: ci-a4e
4/5 drive to completion…
5/5 SUCCESS: workflow root ci-a4e closed with gc.outcome=pass
```

**The proven recipe** (what `gc run <formula>` will do in-process): `gc init --no-start` mints a Dolt-backed city that **never registers with the shared supervisor**; `gc start --controller` runs the **standalone** controller (own lock+socket); it spawns a real **subprocess** worker + a providerless **control-dispatcher** (`gc convoy control --serve`); the worker claims and closes the routed step bead with `gc.outcome=pass`; `workflow-finalize` closes the root; the city is stopped and reaped. Every phase — manufacture → run → finalize → teardown — executed against a real store with real processes; maintainer-city was untouched throughout. The completion signal is exactly the finalize-close watch (`cmd/gc/run_execute.go:watchWorkflowRoot`) the in-process executor uses. Formulas that need an agent to *reason* swap the deterministic worker for an LLM provider; the orchestration is identical.

Key lessons banked from the demo: (a) `gc init` touches the **shared** supervisor — the transient path MUST use `--no-start` + the standalone controller; (b) the file bead provider cannot back a self-closing agent worker (`bd`/`gc bd` are Dolt-only), so a real run needs the Dolt provider; (c) a worker must discover routed work via `bd ready --json` filtered on `gc.routed_to` (the `--assignee=<name>` fast-path errors for named sessions).

---

## 1. Executive summary

The proposal is to make the **Formula** — not the City/Rig — the thing a user runs, carrying its own pack config, agents, **one or more folders**, and **one or more beads databases**, with the city *manufactured on demand* from that declaration and torn down when the work drains.

**The verdict is a qualified yes, and the shape of the answer is smaller and more elegant than the framing suggests.**

Five independent designers converged, without coordination, on the same core fact:

> **"A formula carrying N folders + N beads DBs" is structurally almost identical to "a city with N rigs."**

`config.Rig` is *already* the bundle of one folder (`Path`), one bead-id namespace (`Prefix`), one store endpoint (`DoltHost/DoltPort`), its own pack imports, and its own formula vars (`config.go:571-649`). Every downstream mechanism — prefix→rig→folder routing (`sling.go:389-424`), per-rig store construction (`city_runtime.go:3314`), workdir resolution (`workdir.go:241`), session env injection (`template_resolve.go:315`), cross-store point reads (`storeref.Resolve`) — already works against *any* city that declares the right rigs.

So formula-as-unit is implemented as a **compiler, not a rewrite**: a run manifest → a transient city-as-directory that synthesizes **one rig per declared folder**, runs the **existing** standalone controller, and reaps the directory when the workflow root closes. **Rig is not dissolved into a "folder resource" — it is demoted to a compilation target.** "Sling a convoy to a rig" becomes the degenerate one-folder manifest; hand-authored `city.toml` remains supported forever, because it is literally the compiler's output format. Convergence, not replacement.

The red-team's decisive contribution is a scope correction: **the capability already exists** as a standing multi-rig city (`gc rig add` per repo), so transient manufacturing buys *ergonomics, not capability*. The four designers producing four incompatible schemas across five new packages is itself the sizing signal — the real problem is small. The recommendation below is therefore **kernel-first**: ship the small, safe, standalone-valuable core, then let real demand — not architectural symmetry — pull in the rest.

---

## 2. The three hard questions, answered

### Q1 — What does it mean to attach a beads DB (or a folder) to a formula?

**The formula never attaches a concrete DB or folder.** It stays a pure method — "defined independently of how its work is stored" (`formula-spec-v2.md:42-44`; the `Formula` struct at `types.go:73-146` has zero fields for rig/store/folder/agent, and that is correct). Attachment happens at **invocation**, exactly as it does today via sling. The manifest simply lets the user declare the *whole* binding in one portable place instead of pre-building a standing city.

The two viable authoring shapes (this is an **open decision**, §7):
- **Pure invocation-side binding** — resources live only in the run manifest (`[[folders]]`/`[[stores]]`); the formula says nothing. Simplest; what the kernel uses.
- **Path-free named slots on the formula** — the formula declares *typed slots* (`[requires.folders.src]`, `[requires.stores.work]`) with no path or DSN ever, and the manifest binds them. Costs a small schema addition but buys a real compatibility gate: `requirements.go:47` already hard-fails unknown axes, so an old `gc` rejects a slot-declaring formula cleanly instead of misrunning it — the foundation for portable/shareable formulas.

Either way: **the forge realizes each bound (folder, store) pair as a synthesized rig** in the transient city.

### Q2 — How does a bead know which database to talk to?

**Attach the interface, not the DB — but the interface must be pointer-first and fail-loud, not search-first.**

The "shim that searches all referenced databases" the vision hypothesizes **already exists**: `storeref.Resolve` (`storeref.go:55`) routes by prefix-owner then probes every store. It is the documented successor to the retired `coordrouter`. **But it must never be the primary path**, because (red-team, FATAL-1) it is *first-hit-wins with no ambiguity detection*: attach two DBs that share a bead ID (a fork/clone), reference that ID, and you silently read/write the wrong ledger — the worst failure class in a work store.

The correct resolver is three layers, strict priority:
1. **Ref-addressed (hot path, probe-free):** every bead minted under the run carries its owning store-ref. The stamps already exist (`gc.root_store_ref` `keys.go:152`, `gc.source_store_ref` `keys.go:172`, stamped per-step by `graphroute.go:584`). A reader holding a ref does an exact lookup — no probing.
2. **Prefix-owned (bare-ID path, validated):** route by prefix, legal *only because* prefix-disjointness is validated at manufacture. A store may opt out (`refonly`) when its prefix collides.
3. **Probe (recovery only):** `storeref.Resolve`, hardened to `ResolveStrict` that collects **all** hits and returns a typed `ErrAmbiguousBead` on ≥2 — never first-hit-wins for user-supplied DBs.

The **coordination store / work store split** is the load-bearing rule: molecule beads (root + steps + all control beads), the convoy bead and its `tracks` edges, and mail live in **one coordination store per run** — so **molecules never span stores** (preserving graph-apply atomicity `graph_apply.go:60`, `gc.graphv2_root_key` idempotency, and intra-molecule dep edges). Only *convoy members* — the things that genuinely live in different repos — flow through machinery that is **already** multi-store-federated (`ConvoyDeps.GetStore/FindStore`, `memberStores` variadics, `drainMemberOwningStore`).

### Q3 — How do work items in different repos map to folders?

Because **each declared folder is a synthesized rig with a disjoint prefix**, a convoy member routes to its folder simply by living in that folder's store: the existing prefix→rig→`Path` resolution (`RigDirForBead`, `sling.go:389-401`) delivers per-item folders with **zero new mechanism**. Cross-repo convoys already work *when each repo is a rig*. For steps inside one molecule, the per-step channel already exists (`gc.execution_rig_context` is per-step, `graphroute.go:251`).

**The one real engine gap** (a *missing feature*, not a current bug — see Appendix B): `processDrain` creates unit convoys and item molecules in the **ambient graph store** (`drain.go:967`), so item step beads carry the graph prefix, and a single item-formula fans all members through one routing — per-member cross-repo folder binding isn't expressible. The correct fix is to create the item root in the **member's owning store** (which `drainMemberOwningStore`, `drain.go:307`, already computes) so prefix→rig resolves the member's folder, or add a new per-item folder directive key. This is genuine engine work with atomicity implications; it belongs in the formula-as-unit slices, **not** as a `gc.work_dir` stamp (that key is claim-time observational).

---

## 3. Architecture (the full design, for reference)

The forge/manifest design in full — this is the *destination*, most of which is deferred per §5–6:

- **Run manifest** (`<name>.run.toml`): a constrained projection of `city.toml` (legal because "the City is the local root pack", per AGENTS.md). `[run]` (formula, lifecycle, vars, `[run.on]` convoy/items) + `[imports.*]` (verbatim `config.Import`) + `[[agent]]` (verbatim `config.Agent`, city-scoped) + `[[folders]]` (each → a synthesized `[[rigs]]`) + `[[stores]]` (dir-local per folder, or external Dolt via the existing `Rig.DoltHost/DoltPort` vocabulary).
- **The forge**: temp cityRoot under a foundry root → `EnsureCityScaffoldFS` (`cityinit/layout.go:55`) + `doInit` split-writer → compose `config.City` in memory through the **existing** `ExpandCityPacks`/`mergeFragment` pipeline → `.gc/site.toml` late-binds rig names to the caller's real folders via `PersistRigSiteBindings` (`site_binding.go:292`) so **user repos are never copied or modified** → formula body materialized into `formulas/` so `Compile` search paths work unchanged.
- **Run**: foreground `runController` (`controller.go:1234`) guarded by `.gc/controller.lock` → standard `DoSling` → subscribe to the root bead.
- **Lifecycle**: **driver-owned** — the foreground driver watches the root bead and exits when `workflow-finalize` closes it. (Draining keys on **root-closed**, not ready-queue-empty — a run waiting on CI has an empty queue but an open root and must not be killed.)
- **Store resolution**: the three-layer `StoreSet` of §Q2.

CLI: `gc run <formula> --folder name=/path [--folder …] [--var k=v]` (verified no collision — only `gc supervisor run` exists today).

---

## 4. Composition — does it stay within the six primitives?

**Yes — no 7th primitive, and no primitive dissolves** (primitive-test red-team: QUALIFIED-YES).

- **Pack (CONFIGURES)** — the manifest *is* a per-run root pack; `[imports]`/`[[agent]]` are verbatim pack vocabulary composed by the existing pipeline.
- **Rig (WHERE)** — reused, not dissolved. Each folder compiles to a synthesized Rig. A parallel "folder-resource" concept would duplicate 13+ resolvers that all key off `config.Rig`, violating "no premature abstraction." Rig's *provenance* changes (manufactured, not hand-registered); its jobs don't.
- **Bead (WHAT)** — unchanged substrate; the store *set* is assembled from the manifest at run time instead of from `cfg.Rigs` at load time. Stores are plumbing *beneath* Bead — exactly like the existing (dormant) class-store seam — and correctly fail the Primitive Test's "more useful as models improve" condition, so they stay infrastructure.
- **Formula (HOW)** — stays a pure method; binding is invocation-side.
- **Agent (WHO)** / **Event (OBSERVE)** — untouched; lifecycle/failures surface as typed events. Zero role names in Go throughout.

**Four caveats the primitive-test lens raised that must be fixed before this becomes the architecture record:**
1. All four designs *misapply* the Primitive Test's Atomicity condition (it means concurrency/race safety, **not** conceptual decomposability). The conclusion survives; the reasoning must be re-derived.
2. The **drained predicate** must be **root-anchored** ("root open ⇔ formula in flight", an existing v2 invariant). Two other proposed variants — "no open beads" and "zero live sessions" — are disproven by this fork's own history ("no open beads" = the 138K/day wisp-flood; "zero sessions" contradicts "sessions come and go, work survives").
3. A `max_lifetime` **compare-and-destroy** backstop is a genuine "if stuck then X" cognition leak in Go whose failure mode is *data destruction on a timer*. Keep a timer if you must, but make its action **non-destructive** (stop sessions, retain everything, emit `city.expired`, leave reap to a human/model).
4. The council **contradicts itself** on whether formulas declare WHERE-slots (§7, Decision B).

---

## 5. The red-team, integrated honestly

Three lenses returned SERIOUS-CONCERNS and one QUALIFIED-YES. Taking the findings at face value:

### FATAL (all source-verified) — these are *preconditions*, not backlog
- **F1 — silent wrong-DB read.** `storeref.Resolve` is first-hit-wins, no ambiguity detection. *Precondition for any multi-store attach:* pointer-mandatory routing + `ResolveStrict` + `refonly` for external DBs unless proven prefix-and-content disjoint.
- **F2 — `ValidateRigs` does not cover the actual hazard.** It only checks *declared* rig prefixes, never opens attached DBs, and *explicitly permits* the reserved coordination prefixes `gcg/gcm/gcs/gco/gcn`. Attach a DB that used to be a graph store and its `gcg-` beads collide with the run's fresh coordination beads → control-bead misroute. *Precondition:* audit attached stores' *actually-minted* prefixes at manufacture; promote the reserved-prefix check to fatal.
- **F3 — the manifest is an unauthenticated capability grant with an RCE tail.** `[[folders]].path` → arbitrary `Rig.Path`; `[[stores]].dolt_host` → arbitrary endpoint; gate scripts exec with `cmd.Dir` set to the declared folder (`condition.go:319`, trusted roots in `ralph.go:272`); `gc.work_dir` is an inherited directive any bd-writing agent can stamp. *Precondition for any shared/hosted host:* a manifest-validation authz gate **before** scaffold I/O + `work_dir` containment enforced at route- and spawn-time. (Hook exists in OSS; commercial policy stays in private repos per the no-commercial-code directive.)

**Why the kernel is also the safe subset:** all three FATALs arise from *attaching multiple/external/pre-existing DBs* and from *shared-host multi-tenancy*. The kernel (one dir-local rig per folder, disjoint prefixes, one user's own machine) does none of that — which is precisely why it is safe to ship first while F1–F3 gate the phases that introduce those surfaces.

### Serious-but-addressable
- **Cross-repo convoys are not atomic** and the engine cannot make them so (per-item `workflow-finalize`, no cross-repo transaction). repoA merges, repoB's gate fails → half-applied change with no revert. *Do not let the manifest UX imply transactional semantics.* Surface partial-failure at the parent finalize; for coordinated changes require an explicit two-phase "prepare all, then merge all" formula shape.
- **Teardown vs newly-arriving work / ledger loss.** Gate teardown on a post-cancel re-check that refuses new slings to a draining city; never default the coordination store to ephemeral when retain can delete it.
- **Fork-fit collisions (SERIOUS):** (a) the multi-store work collides with the **unpushed `feat/domain-infra-store-split`** branch — `resolveClassStore` is dormant identity on the merge target and `MemberStores` has **zero production assignment sites**, so "cross-repo convoys already work in production" is *dormant-seam-only*; sequence StoreSet work **behind** that branch. (b) A drained-predicate branch in `CityRuntime.run` collides with the **S19/Windshield reconciler strangler** rewriting that exact loop; keep lifecycle in the **driver**. (c) The fleet runs `deploy/sqlite-b36-probe-attribution`, not `main`; slices 0–1 must be **new files only** to cherry-pick cleanly. `upstream/main` is a dead target (2306 commits behind).

### The YAGNI kernel (the red-team's central argument)
Strip the four competing schemas, five packages, three lifecycle vocabularies, `--detach`/`--reuse`, warm pools, marketplace slots, `StoreSet` package, workspaces, and the retention matrix. The irreducible, defensible kernel is **three things**:

1. **Close the drain per-item folder gap** — a *missing feature*, not a current bug (Appendix B): create drained item roots in the member's owning store (or add a per-item folder directive key) so cross-repo drains route each item to its folder. Genuine engine work with atomicity implications; belongs in the formula-as-unit slices, not a `gc.work_dir` stamp.
2. **Bless multi-rig standing cities as THE cross-repo answer** and scope the cross-rig/cross-store guards to `cfg.Rigs`-as-allowlist. Document `gc rig add` per repo.
3. **If one-shot DX is genuinely wanted:** a thin **foreground `gc run`** — scaffold a temp city with synthesized rigs, run the existing in-process `runController`, watch the root bead, exit, keep the directory. No manifest dialect, no lifecycle config keys, no drained predicate, no supervisor changes, no workspaces, no `StoreSet`, no retention matrix.

Plus two cheap hardenings worth keeping from the larger design because they fix latent bugs *today*: `storeref.ResolveStrict` (fail-loud ambiguity) and load-time prefix-disjointness validation.

---

## 6. Recommended path

**Phase A — standalone bug fixes (ship regardless of the vision; each cherry-picks onto the deploy branch):**
- A1. Drain per-item folder routing (§Q3, Appendix B) — a missing feature (not a current bug): create item roots in the member's owning store, or add a per-item folder directive key. Engine work; sequence with the store-split branch.
- A2. `storeref.ResolveStrict` + convert user-supplied/attached probe sites to fail-loud.
- A3. "Molecules never span stores" as an enforced test invariant.
- A4. Cross-rig/cross-store guards scoped to `cfg.Rigs`-as-allowlist; document multi-rig as the cross-repo answer.

**Phase B — the DX kernel (one new package + `cmd/gc/cmd_run.go`, foreground only, new files only):**
- B0. `gc run --dry-run` prints the synthesized `city.toml`/`site.toml`; zero engine edits; TDD in `t.TempDir()`.
- B1. `gc run <formula> --folder w=/repo --var k=v`: forge → foreground `runController` → watch root → exit → keep dir. Single-rig semantics. **This delivers "formula as the unit a user runs" — the requester's actual stated need.**
- B2. N folders → N synthesized rigs (mostly free; multi-rig federation is live). Adds manifest-scoped guard allowlists + load-time prefix validation.

**Phase C — pull in only on demonstrated demand, each gated on a named user and its precondition:**
- `.run.toml` file format (when a run def wants to live in a repo).
- Multi/external stores + the `store:` ref kind + full `StoreSet` — **sequenced behind `feat/domain-infra-store-split`**; **gated on F1+F2**.
- `--detach` + supervisor-hosted **root-anchored** drained predicate (emit-only soak first) — **sequenced behind S19/W0–W8**.
- Manifest authz gate + `work_dir` containment — **hard precondition (F3) before any shared/hosted manufacture.**
- Path-free formula slots + compatibility gate — when portability/marketplace/CI is a real need.

---

## 7. Open decisions for Julian

Each is a genuine either/or that changes what gets built; my recommendation leads.

**A. Scope — kernel-first, or build toward the full forge now?**
→ *Recommend kernel-first (Phase A + B).* The 5-designers-4-schemas divergence is the sizing signal; the kernel is also the FATAL-free subset.

**B. Do formulas declare WHERE-slots?**
→ *Recommend: no slots in the kernel (pure invocation-side binding); add path-free slots on `Requirements` later if portability is pursued.* Slots are the one thing that make formulas *travel* (and give the old-host compatibility gate for free), so this is a real fork in the road, not a detail.

**C. Cross-repo convoy semantics — accept best-effort, or require two-phase?**
→ *Recommend: document best-effort + surface partial-failure; never imply transactional cross-repo merges.* Add a two-phase formula shape only when a coordinated multi-repo change is a real workflow.

**D. Transient-city lifecycle owner — driver-only, or supervisor-hosted `--detach`?**
→ *Recommend: driver-only until detached runs have a named user.* Avoids colliding with the reconciler strangler and imports none of the drained-predicate failure classes.

**E. Backend for transient cities — same scoped-file provider as `gc init`, or the doltlite optimization?**
→ *Recommend: same as `gc init`; measure before optimizing.* A transient-vs-standing backend split is this fork's most expensive recurring incident class (bd schema-skew).

**F. Security posture — is manufacture ever going to run on a shared/hosted host?**
→ If **yes**, the F3 authz gate is a hard precondition (commercial policy in private repos). If **local-single-user only**, F3 is deferred but must be documented as an explicit boundary, not an omission.

---

## Appendix B — `gc run` mechanics (source-traced)

Full leg-by-leg trace in `_raw/04-gcrun-mechanics.md`. The three facts that make the forge cheap and the deployment real:

1. **A rig binds to `/repo` by a name-keyed overlay at config-load — never a copy.** `city.toml` holds a *pathless* `[[rigs]] {name, prefix}`; `.gc/site.toml` holds `[[rig]] {name, path=/repo}`; `ApplySiteBindings` (`compose.go:664` → `site_binding.go:197`) overwrites `cfg.Rigs[i].Path` by name at load. The forge writes name+prefix to `city.toml` and `{name,/repo}` to `site.toml`; the standard load path binds it for free. Teardown = `RemoveAll` on the manufactured dir; `/repo` is untouched.

2. **No RPC assigns work — the loop is a level-triggered diff on `gc.routed_to`.** Materialize *is* route: `molecule.Instantiate` persists step beads carrying `gc.routed_to` (`graphroute` stamps it). Each patrol tick (`city_runtime.go:929`), `scale_check` runs `bd ready --metadata-field "gc.routed_to=<template>" --unassigned` (`workquery.go:43`) → `poolDesired[T]=N` → `ComputeAwakeSet` → `startCandidate` → `worker.Handle.StartResolved` spawns a tmux pane with `cwd=/repo`, `BEADS_DIR=/repo/.beads`. The agent's rendered prompt runs the **same** `bd ready` query, finds the **same** bead, claims it, edits `/repo`, closes it. Two independent reads of one persisted bead close the loop. The poke on `.gc/controller.sock` is a latency accelerator, not correctness.

3. **A single-folder run is byte-identical to a standing single-store city, so it carries zero resolver risk.** `resolveClassStore` (`class_store.go:231`) is identity today, so coordination beads live on the same store as work beads; the graph-first branch in `workflowStores` is skipped (`GraphBeadStore()==CityBeadStore()`), and `storeref.Resolve` reduces to a one-element federation that never probes. The resolver's three-layer complexity only becomes load-bearing when a **second/external/pre-existing** DB is attached — which is exactly why the kernel (one dir-local rig per folder) sidesteps all three FATAL findings.

**Lifecycle:** `runController` (`controller.go:1234`) exits *only* on ctx-cancel (SIGINT or `stop` on `.gc/controller.sock`). There is no built-in stop-on-root-close, so the driver-owned lifecycle is: sling → watch the workflow root bead → on `workflow-finalize` close (`dispatch/runtime.go:716`) send `stop` → `RemoveAll`. Zero `city_runtime.go`/supervisor edits.

**Known gap (drain) — NOT a current bug (verified 2026-07-08).** An earlier trace claimed `stampDrainItemRecipe` (`drain.go:1146`) should stamp `work_dir` like retry/ralph. That is **wrong**: `gc.work_dir` is a **claim-time observational work-record** (ADR-0009, `keys.go:196-202`), stamped by the worker at claim, read by the close gate; **nothing** stamps it at bead creation (grep of sling/molecule/graphroute/fanout/drain is empty), and the spawn resolver (`session_reconciler.go:4708`) only reads it from *already-claimed* `in_progress` beads with an on-disk dir — a resume mechanism. retry (`retry.go:353`) and ralph (`ralph.go:172`) *read* `work_dir`; they don't propagate it at creation. So drain is consistent with the whole codebase, and a drained item's initial folder resolves the same way every work bead's does (agent rig scoping / prefix→rig).

The genuine gap is a **missing feature**: one drain item-formula fans all members through one routing, so per-member cross-repo folder binding isn't expressible. The correct fix is to create the item root in the **member's owning store** (so prefix→rig resolves the member's folder) or add a **new per-item folder directive key** — *never* to overload the observational `gc.work_dir` (the primitive-test red-team flagged that observational→directive promotion as security-relevant). This is formula-as-unit design work, not a standalone bugfix.
