# Create-a-City — Feature Backlog & Burn-down

**Status:** in progress · **Owner:** this session · **Started:** 2026-06-27

A UI flow to create a Gas City. The wizard is a **multi-step workflow view in the Forge
app** (`gascity-design-system/apps/forge-web`); the end result is a **controller running
as a Crucible gVisor sandbox (Model B)** attached to a **fresh hosted beads ledger** and
**auto-minted crucible/beads/manifold credentials**, able to spawn child agents via the
Crucible API and do inference via manifold.

> Tracking lives here (not `bd`): this worktree's beads is wedged on the recurring `ga`-DB
> server-mode/metadata issue (memory `gascity-rig-beads-marker-recurring-removal`), and the
> feature spans 5 repos, so a single gascity-local ledger isn't its home. Mirror into `bd`
> once the `ga` server is healthy.

## Locked decisions (user, 2026-06-27)
- **UI home:** a workflow (multi-step view) in the **Forge app** — *not* the GC Dashboard.
  ("Workflow" in Forge today = a UI-wizard pattern, not a product primitive.)
- **Sequencing:** UX track and cross-cluster-provisioning track built **in parallel**.
- **Beads (v1):** **create-new only** (attach-existing / attach-by-url deferred).
- **Controller:** **Model B — controller-in-Crucible** (gVisor sandbox spawning children
  via the Crucible API).

## Confirmed auth path (browser → crucible `/v0/cities`)
```
forge-web: POST /forge/api/org/{org}/cities  (+ session cookie)
  → apex Caddy  handle /api/org/*  → shell-BFF (NodePort 30092)
  → shell-BFF: ResolveHuman + gateOrgAdmin, inject X-Gc-Keycloak-Sub/Org/Session
  → crucible-edge mints aud=crucible EIA (machinery already used by the run-plane)
  → crucible.ops /v0/cities  (verifies EIA + crucible:sandbox.create + gateOrgAdmin)
```
Net-new = a **shell-BFF cities route** (B7) + the **forge-web client** (A4). The EIA-minting
edge already exists. (Controllers keep using the machine-edge `eia-machine-proxy`; this is the
distinct *browser* path.)

## End-to-end flow (target)
```
wizard ──/v0/cities──▶ crucible: mint SIGNED INTENT (no god-token), persist "pending"
  identity-v0 provisioner ──pulls pending (one-way rail)──▶ mints crucible+beads+manifold
     SPs/keys via NEW provisioningToken → OpenBao ──ESO──▶ corp-public → POST /complete
  crucible orchestration: beads-web POST /api/projects (create-new) → bd_prj_<id>
     → launch controller as a Crucible SANDBOX (Model B) w/ {pack, hosted .beads, creds}
  controller boots (gc init → use-external → gc start) → hosted beads + manifold; spawns children
  wizard polls status: pending → provisioning → ready / error
```

## Slice backlog

Status: `TODO` · `WIP` · `REVIEW` (red-teamed) · `DONE` · `BLOCKED`. Red-team workflow runs
between slices; a design workflow is spun up when a slice is ambiguous.

### Track A — UX (forge-web wizard) — no cross-cluster blocker
| ID | Slice | Repo | Status | Depends |
|----|-------|------|--------|---------|
| A1 | Create-city API contract (request/response/status types) — `api/createCity.ts` | forge-web | ✅ DONE | — |
| A2 | Multi-step wizard view: name+workspace → beads(create-new) → pack → review — `views/CreateCity.tsx` | forge-web | ✅ DONE | A1 |
| A3 | Async status view: poll status; per-step checklist (creds → beads → controller) + cred summary | forge-web | ✅ DONE | A1 |
| A4 | `createCity()`/`getCityStatus()` client via `@gascity/auth` ApiClient (targets the B7 route) | forge-web | ◑ client done; real wiring needs B7 | A1, B7 |

> A1–A3 committed `0b904ab`+`cbe5f9e` (branch `feat/forge-web-create-city`); typecheck + vite build clean; 13 logic unit tests green; red-team **fix-then-ship** → must-fixes applied (bounded polling, org guard) + should-fixes (type guards, aria, Next disabled).

### Backend contract (derived by the wizard red-team — what B7 + crucible MUST provide)
The wizard's contract is the agreed target; the backend builds to it.
1. **`GET /forge/api/org/{org}/cities/{city_id}/status`** must exist end-to-end (shell-BFF + a crucible per-city status handler — today crucible has only `POST`/`GET /v0/cities`). Returns `CityProvisionStatus`: `{city_id, status, status_detail?, beads?{database?,status?}, credentials?[{product,sp_id?,key_id?}], controller?{status?,sandbox_id?}}`.
2. **`POST …/cities`** must accept + persist the full body `{name, workspace:{mode:new,name}|{mode:existing,workspace_id}, beads:{mode:create-new}, pack}` — crucible currently decodes only `{name}` (cities.go:136-147); extend `CityRecord` with `WorkspaceID` + `Pack`.
3. **status enum** on the wire is only `pending|provisioning|ready|error` — crucible writes literal `active` today (cities.go:201); map create-time → `pending`/`provisioning`.
4. **create returns 202** + minimal `{city_id, status}` (trim the internal cityView fields off the wire).
5. **workspace_id authz** at the shell-BFF leg — validate the caller may sponsor a city in that workspace BEFORE crucible mints creds (tenant boundary; the wizard can't enforce it).
6. **provisioning-state tracking** — populate `credentials[]` on mint, `beads.status=ready` when the ledger is up, `controller.status=ready`+`sandbox_id` on launch, to drive the 3-step checklist.

### Track B — Backend (cross-cluster minting + orchestration + Model-B launch)
| ID | Slice | Repo | Status | Depends |
|----|-------|------|--------|---------|
| **B0** | **Accounts `provisioningToken`** — DB-backed **per-org** token re-gating the 3 org-bound SP/key routes; human path keeps `adminToken` (defense-in-depth), machine path = per-org token; `created_by`=non-NULL system sentinel; `UNIQUE(org_id,name)`. Spec: [B0-provisioning-token-spec.md](B0-provisioning-token-spec.md). Commits `e283527`+`6fb52d6` (branch `feat/accounts-provisioning-token`); 5 unit + full suite green; red-team **ship**. | gasworks-platform | ✅ DONE | — (UNBLOCK) |
| B1 | crucible `/v0/cities` pull-intent rework: drop admin-token; mint eiasign intent (nonce+TTL); citystore pending→ready; `GET /v0/cities/pending` (mTLS) + `POST /v0/cities/{id}/complete` | forge | TODO | B0 |
| B2 | identity-v0 provisioner binary: pull intents → verify → mint crucible+beads+manifold keys via provisioningToken → OpenBao → `/complete` | forge | TODO | B0, B1 |
| B3 | Infra: provisioner Deployment/SA/netpol, ESO ExternalSecret (corp-public ← bao-product-eu), RWO PVC; remove admin-token env | _infra | TODO | B2 |
| B4 | Beads create-new: orchestration → `beads-web POST /beads/api/projects` → poll ready → capture `bd_prj_<id>` + gateway endpoint | forge | TODO | B1 |
| B5 | Manifold creds (`mn_live_`) via intent path + resolve the entitlement/pool (fresh-org 403-no-pool) | forge / gasworks / aimux | TODO | B1 |
| B6 | Model-B controller launch: create controller as a Crucible sandbox w/ pack + hosted `.beads` + creds; productionize controller image (bake bd+CA+ICU+eia-helper) | forge / gascity | TODO | B4, B5 |
| B7 | shell-BFF cities route `/api/org/{org}/cities` (gateOrgAdmin, EIA, proxy to crucible) + Caddy handle | gasworks-platform / _infra | TODO | B0 |

### Track C — Hardening (gated, post-convergence)
| ID | Slice | Repo | Status | Depends |
|----|-------|------|--------|---------|
| C1 | Controller-in-Crucible durability derisk spike (persistent `.gc/`, child reaping, <90s token rotation, restart recovery) — **required before production Model B** | gascity / forge | TODO | B6 |
| C2 | open-Q7 egress rail (per-cell Cilium allowlist → works.gascity.com:443) for in-sandbox cred exchange | _infra | TODO | B6 |
| C3 | Per-workspace EIA enforcement (inert today) + gc-sandbox default-deny netpols + StreamExec capability gate + controller node-split | _infra / forge | TODO | B6 |
| C4 | Credential lifecycle: rotation, re-show/regenerate, revocation (SP-disable + bao-leaf delete) | gasworks-platform / forge | TODO | B1 |

## Assumed defaults (adjustable)
- **Workspace:** new workspace per city by default (picker to attach existing); **soft**
  boundary (access-control) for v1; hard isolation opt-in later.
- **Creds in UI:** reveal-toggle, once-only secrets; re-show/regenerate deferred (C4).
- **Orchestration home:** the **crucible backend** owns orchestration for v1; a separate
  `city-svc` is a future extraction.

## Risks / blockers
1. **B0 `provisioningToken`** gates all hosted minting (B1–B6). First on Track B.
2. **Controller-in-Crucible durability** (C1) gates production Model B.
3. **Manifold entitlement/pool** for a fresh org (B5 — the 403-no-pool).
4. **Single-node cherry collapse** — where hosted controllers physically run for multi-tenant (C3).

## Process
- One slice at a time per track; **TDD** (failing test first).
- **Red-team workflow review between slices** (adversarial verify of the diff before marking REVIEW/DONE).
- **Design workflow** spun up whenever a slice's approach is ambiguous.
- Keep changes minimal-surface and upstream-friendly per `AGENTS.md`; ZERO hardcoded roles.

## Burn-down log
- 2026-06-27: backlog created; auth path confirmed (shell-BFF cities route).
- 2026-06-27: **B0 DONE** — provisioningToken design+adversarial-security workflow → red-teamed spec; impl (schema 061/062 + gateProvision + ResolveProvisioningToken + CreateServicePrincipalAs + 3-handler re-gate); 5 unit tests + full accounts suite green; diff red-team verdict **ship** (strictly narrower than adminToken, no bypass, no NULL-created_by, no regression). Improvement over spec: human path keeps the admin-token wire check.
- 2026-06-27: **A1/A2/A3 DONE** — forge-web create-city wizard (contract + tested logic + multi-step view + async status); typecheck/build/13-tests green; red-team fix-then-ship → fixes applied (`0b904ab`+`cbe5f9e`). Derived the backend contract (above) for B7/crucible.
- 2026-06-27: starting **B7** (shell-BFF `/api/org/{org}/cities` route → crucible) — makes the wizard's contract real via the confirmed browser→EIA→crucible path.
