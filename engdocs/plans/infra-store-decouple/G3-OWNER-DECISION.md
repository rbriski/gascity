---
title: "Track G — OWNER DECISION required before G3 (delete coordrouter)"
date: 2026-06-27
branch: plan/decouple-infra-beads
head: 92f0870ac
status: BLOCKED on owner decision — recon wf_83318c7f-4ff complete
evidence: engdocs/plans/infra-store-decouple/raw/g2g3-adjudication.json
---

## ⚠️ CORRECTION (verification pass wf_3c6c2ee8-e09, code-verified)

The first recon **overstated** the cross-class claim for two of the three sites. A focused
adversarial re-verification (3 fresh traces) + direct code checks corrected it:

- **molecule.Instantiate — NOT cross-class.** Every molecule is homogeneous: a single
  compile-time bit (`UsesGraphCompiler`/`IsCompiledGraphWorkflow`) fixes the root AND all
  children to one class, enforced by `validateExplicitGraphCompilerRequirement`
  (formula/fragment.go:116, compile.go). A legacy recipe is *forbidden* from carrying graph
  steps. The only "mixed" case is a non-functional hand-authored pathology no caller
  produces. → **caller picks the store** (it already holds the recipe; sling.go:1247 computes
  the bit). Verdict: yes-with-churn, not irreducible.
- **molecule.CloseSubtree — NOT cross-class (always ClassWork).** `CloseAttachedSubtree`
  **diverts** graph.v2 roots to `sourceworkflow.CloseWorkflowSubtree` (sling_attachment.go:151);
  `molecule.CloseSubtree` only ever sees legacy `type=molecule` subtrees, uniformly ClassWork.
  The recon conflated the two closers. → caller passes the work store unconditionally.
- **drain / convoy membership — GENUINELY cross-class (recon was right here).** The real
  seam is the shared primitive `convoy.Members`/`TrackItem` (internal/convoy/membership.go):
  a **synthetic** convoy (ClassGraph: drain-unit convoys drain.go:845, graph.v2 input convoys
  invocation.go:393) holds a `tracks` edge to **ClassWork** members. `TrackItem` does
  `Get(member)`[work] + `DepAdd(convoy[graph], member[work])`; `Members` does `DepList(convoy)`
  [graph] + `Get(member)`[work] — both in one call with one store. The code already guards it:
  `drainUnresolvedMemberError` = "…cross-store member…" (drain.go:259).

**Net:** the routing requirement collapses from "3 scattered sites" to **ONE well-defined
seam — convoy membership** (exercised by drain-unit + graph.v2 input convoys). Everything
else is class-aware: graph reads (G1, done), graph creates (caller picks via the class bit),
by-id (G2b / `storeref`), graph List (G2c). This materially de-risks the ship and the
decision below should be read through this correction.

### Refined options (post-correction)
- **(b1) Delete coordrouter; resolve the convoy seam with `internal/storeref`.** Class-aware
  everywhere; at `convoy.Members`/`TrackItem` resolve the cross-class member by-id via the
  existing conformance-pinned `storeref` helper (PrefixOwner/Resolve — stateless, not a
  router). Cost: thread a `[work,graph]` store-set into `convoy` membership + its synthetic-
  convoy callers (drain, graphv2 invocation, dispatch ProcessControl). Achieves the goal; uses
  existing primitives; bounded surface. **My recommendation.**
- **(b2) Delete coordrouter; tiny storeref-backed wrapper at the convoy seam.** Same, but the
  by-id resolution lives in a ~80-line fork-owned wrapper handed to drain/invocation instead
  of threaded params. Less call-site churn; the wrapper is a *narrow* routing object (one
  seam), materially different from the rejected broad graphRoutedStore.
- **(a) Keep coordrouter, scoped to the convoy seam only.** Everything else class-aware; the
  Router survives but backs only convoy membership. Smallest diff, lowest live-dispatcher risk;
  package not deleted.

## The finding (8-agent recon, current-code-verified — see CORRECTION above)

Track G's plan was **"class-aware callers, delete coordrouter"** (the owner explicitly
**rejected** the Path-B `graphRoutedStore` routing object). The recon traced every
single-store graph op in `molecule`/`dispatch`/`sling`/`convoy` and found the plan is
**not fully achievable**. Two surfaces go class-aware fine; **three sites cannot**:

| Surface | Verdict |
|---|---|
| sling / order-dispatch | ✅ yes-with-churn (separate work+graph params, no object) |
| dispatch graph-PURE handlers (retry/ralph/check/fanout/tally/scope-check/finalize) | ✅ yes-with-churn |
| **dispatch drain** (`convoy.Members`/`TrackItem`) | ❌ no — needs a by-id routing object |
| **molecule.Instantiate recipe sequential-fallback `Create`** + `markFailed` | ❌ no |
| **molecule `CloseSubtree`/`CloseAll`** over cross-class subtrees | ❌ no |

Why the three are irreducible (file:line verified):
- `molecule.go:663` `store.Create(b)` services graph.v2 steps, convergence wisps, AND
  legacy work recipes through **one** line — per-bead class only knowable via
  `coordclass.Classify(b)`. Threading a fixed graph store is *wrong* for the work pours.
- `cleanup.go:144` `store.CloseAll(ids)` closes a subtree whose ids span **both** stores.
  A single-store `CloseAll` closes only its own ids and silently leaves the other class
  open — the molecule never converges (mass-non-closure). `Router.CloseAll` groups by
  `backendForID`; a single param cannot reproduce it.
- `convoy/membership.go:68` `Members` = graph `DepList(convoyID)` + work `Get(memberID)`;
  `:24` `TrackItem` = work `Get(itemID)` + cross-class `DepAdd(convoyID,itemID)`. Convoys
  **inherently** link a graph convoy bead to work member beads across the two physical
  stores. `convoy` is a **shared primitive** — changing its signature ripples everywhere.

**Bottom line:** the graph/work *physical* store split is inherently cross-class at
convoys, cross-class subtree closes, and mixed-class molecule creates. A **by-id routing
object at the boundary is architecturally required.** "Delete coordrouter via class-aware
callers" cannot be met without *replacing* it with an equivalent routing object.

## The decision

| Option | What | Risk on live 6-rig city | Meets "delete coordrouter"? |
|---|---|---|---|
| **A** | Build a **focused** fork-owned `graphRoutedStore{work,graph}` — by-id `Get/DepList/DepAdd/CloseAll` + graph-targeted `Create/ApplyGraphPlan` over `[work,graph]`, **no** federated List/Ready (those go class-aware via G1/G2c). Make sling+graph-pure handlers class-aware. Delete coordrouter. | Medium — must **faithfully port** the Router's `CloseAll`/`backendForID`/`Members` logic; byte-identity at graph=bd is **blind** to misroutes (there the two stores are one) → relies entirely on the new relocated-graph conformance test + adversarial review. | ✅ yes |
| **B** | **Keep coordrouter, scoped to graph only** (sessions already off it). Stop Track-G's deletion goal. Optionally still land G2b (API by-id class-aware) as hygiene. | **Lowest** — zero churn to molecule/dispatch/sling/convoy; the Router's cross-class logic is already verified-correct in production. | ❌ no (Router survives as the graph router) |
| **C** | Churn `molecule`/`dispatch`/`convoy` signatures to thread both stores + push `coordclass.Classify` into Layer-2 shared primitives. | **Highest** — re-implements Router logic in 4+ packages, touches shared primitives, per-id classification = judgment-in-Go the project forbids, blows the ≤5-file budget. | ✅ yes (but not advisable) |

**Note:** Option A is a *narrower* object than the rejected Path-B (by-id only; List/Ready
handled class-aware), so it may be acceptable where the full Path-B was not. The decoupling
north-star ("infra→Postgres, only work in Dolt") is **already met** — graph is off Dolt
regardless of this choice. This decision is purely about the *cleanup* goal (retire the
routing indirection) vs. live-system risk.

**My recommendation:** lean **B** (lowest risk; the goal was cleanup, not capability; a
routing object is architecturally required either way so "deletion" only renames it). **A**
is viable and achieves the stated goal if the owner accepts the port + heavy conformance/review.
**C** is off the table for an irreversible live migration.

## What is safe to land regardless (if the owner wants progress now)
- **G2b** (by-id `[graph, work]` arm in `beadStoresForID`) — byte-identical at graph=bd,
  correct under both A and B. **NB:** order is `[graph, work]` (graph-first), not the
  literal `[work, graph]` in the old prompt — graph-first faithfully replaces
  `Router.backendForID` (prefix-owner-first), avoids a wasted work-store `bd` Get on every
  graph mutation, and keeps the leaf capability-assert on the graph store. `[work,graph]`
  is a *membership* requirement, not an ordering one.
- The additive **followups** (two-store wait/extmsg test; doctor/status under-count; PG
  read-after-write test).

## What is BLOCKED on the decision (do NOT land standalone)
- **G2a** double-routes if it lands while the Router still exists → must land *with* G3a
  (or under Option B, is unnecessary).
- **G2c**'s `ListGraphOnly` forwarder goes dead again post-G3 unless G3 makes the wrapped
  graph store advertise `GraphOnlyListStore` → coupled to the G3 shape.
- **G3** itself.
