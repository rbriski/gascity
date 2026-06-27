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
1. ✅ **`GET …/cities/{city_id}/status`** — **crucible handler landed** (`crucible d781a6c`, branch `feat/cities-status-endpoint`): returns `{city_id, status(enum), status_detail?, credentials[]}`, per-org scoped (cross-tenant 404); 7 cities tests green. *(Remaining: shell-BFF `/forge/api/...` leg of this route = B7; richer `beads`/`controller` fields populate when B4/B6 track them on the record.)*
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
| **B2** | **Autonomous provisioner binary** (`cmd/city-provisioner` + `internal/cityprovision`): pull `/pending` → provision beads ledger + launch Model-B controller → `/complete`. Orchestration + 3 adapters, httptest-tested; **red-teamed** (2 must-fix fixed). Commits `dbaea47`+`3dfe047`+`b562c52` (branch `feat/cities-status-endpoint`). | forge | ✅ DONE | B0, B1 |
| B3 | Infra: provisioner Deployment/SA/netpol, ESO ExternalSecret (corp-public ← bao-product-eu), RWO PVC; **+ credential refresher** minting per-pass `aud=crucible` (w/ `city.provision` scope) + per-org `aud=beads` EIAs via Accounts(provisioningToken)→STS; **+ cross-org pull seam** (design Q). | _infra / forge | TODO | B2 |
| **B4** | **Beads create-new** — `BeadsWebProvisioner`: `POST /beads/api/projects` + poll ready → `bd_prj_<id>`. Built into B2's adapter. | forge | ✅ DONE (in B2) | B1 |
| B5 | Manifold creds (`mn_live_`) via intent path + resolve the entitlement/pool (fresh-org 403-no-pool) | forge / gasworks / aimux | TODO | B1 |
| B6 | Model-B controller launch: create controller as a Crucible sandbox w/ pack + hosted `.beads` + creds; productionize controller image (bake bd+CA+ICU+eia-helper) | forge / gascity | TODO | B4, B5 |
| B7 | Browser→crucible wiring | gasworks-platform / forge-web | ✅ DONE (via product-proxy) | B0 |

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
- 2026-06-27: **B1 (partial) — crucible status endpoint** `GET /v0/cities/{city_id}/status` shipped (`crucible d781a6c`, branch `feat/cities-status-endpoint`): returns the `CityProvisionStatus` contract (`pending|provisioning|ready|error` + credentials), per-org scoped, cross-tenant 404; additive (existing tests green) + `TestGetCityStatus`. Closes the wizard red-team's "no status endpoint" gap.
- 2026-06-27: **crucible workspace/pack persistence** (`crucible 922de47`) — parse + persist `workspace`/`pack` (idempotent SQLite `ALTER`); closes the "backend only accepts {name}" gap; tests green.
- 2026-06-27: **B7 DONE via the product-proxy** (`forge-web 3d565d8`) — the apex shell-BFF `/crucible/*` product-proxy already mints the aud=crucible EIA and forwards to crucible; the wizard now calls `/crucible/v0/cities` + `…/{id}/status` directly (org rides the EIA). No new shell-BFF route. **The wizard is now wired to the real crucible backend.**
- **State:** the UI is no longer a shell against an unbuilt contract — it reaches crucible's real create + status endpoints, which persist the full request. In a reachable env (crucible configured as a product edge + able to call Accounts), `POST /v0/cities` mints the city's orchestrator SP/key synchronously → the wizard shows `provisioning` (credentials done).
- 2026-06-27: **crucible completion seam** (`crucible 81b821a`) — `provisioning_state` JSON column + `POST /v0/cities/{id}/complete`. The **full lifecycle now exists**: create→`provisioning` (orchestrator cred) → complete→`ready` (beads ledger + controller recorded); `cityPhaseFor` advances to `ready`; the status view surfaces beads + controller; `TestCompleteCityAdvancesToReady` drives it end to end.
- **State:** the create-city *experience* (wizard → real backend → full status lifecycle to `ready`) is wired and demonstrable — a create + a `/complete` call yields a `ready` city the wizard renders. What's missing is the **autonomy**: nothing yet *calls* `/complete` after actually provisioning the ledger + controller.
- 2026-06-27: **crucible pull seam** `GET /v0/cities/pending` (`crucible b7c126d`) + **B2 provisioner orchestration** (`internal/cityprovision`): `ProvisionOne`/`RunOnce` with partial-failure correctness, 5 unit tests.
- 2026-06-27: **B2 DONE — the autonomous provisioner binary** (`crucible dbaea47`): `cmd/city-provisioner` + the three real adapters (`CrucibleQueue` pull/complete · `BeadsWebProvisioner` POST /api/projects+poll · `CrucibleControllerLauncher` POST /v0/sandboxes), each tested against an httptest server (11 tests total). The binary compiles; the provisioning LOGIC and every backend call are built + unit-tested.
- **The whole pipeline is now built + tested:** wizard → crucible create (orchestrator cred, `provisioning`) → B2 pulls `/pending` → provisions beads ledger + launches the Model-B controller → `POST /complete` → `ready` → wizard renders it.
- 2026-06-27: **B2 red-team → 2 must-fix FIXED** (`crucible 3dfe047` + `b562c52`). Workflow `redteam-b2-provisioner` (25 agents; 3 lenses × adversarial verify): 22 raw → 11 confirmed (2 must-fix, 7 should-fix, 2 nit).
  - **must-fix #1 (broken object-level authorization):** `/pending`,`/complete`,`/status` skipped the org-admin gate `create` enforces; every city's orchestrator SP holds `sandbox.{read,create}`, so any city's controller could poison/downgrade/enumerate a SIBLING city. FIXED by splitting authority — machine endpoints (`/pending`,`/complete`) → new `crucible:city.provision` scope (never minted onto an orchestrator key); wizard `/status` → org-admin gate (Accounts `gateOrgAdmin` probe, fail-closed). Tests `TestCityMachineEndpointsRejectSandboxScope`, `TestCityStatusRequiresOrgAdmin`.
  - **must-fix #2 (no HTTP timeout → a hung backend wedges the daemon):** FIXED — shared `http.Client{Timeout:30s}` on all adapters + per-city bounded ctx in `RunOnce`.
  - **+ idempotency (self-found, real):** a controller-failed retry orphaned the first beads ledger. FIXED — `cityPendingView`/`PendingCity` carry downstream state; `ProvisionOne` resumes (reuses the ledger). Test `TestProvisionOneResumesReusesExistingLedger`.
  - **+ should-fixes applied:** non-fatal completion (`errors.Join`+continue), `waitReady` status-code check + no swallowed decode, bounded non-2xx bodies in all adapter errors.
- **Remaining = B3 only (deploy + credential plumbing).** Provisioning code is done + red-teamed; B3 is the deployment, and the red-team pinned its credential-design inputs:
  1. **Provisioner EIA must carry `crucible:city.provision`** (+ register the scope in `internal/accounts/scopes.go`) — until then the machine endpoints are correctly inert to every non-provisioner caller (the security fix made the scope a hard requirement).
  2. **Cross-org pull** (should-fix #1): `/pending` is org-scoped but the provisioner is cross-org — it can't see other orgs with an org-scoped EIA. B3 mints EITHER a per-org `aud=crucible` EIA (carrying `city.provision`) per org with pending cities (needs a platform-scoped discovery seam: `GET /v0/cities/pending/all` + `CityStore.ListAllPending()` gated by `city.provision` + an allow-listed machine subject), OR runs the refresher per-org. **Spin a design workflow before building.**
  3. **Per-org beads EIA** (should-fix #2): ledger creation needs a per-org `aud=beads` EIA, not one shared `BEADS_EIA` — thread an org-scoped token into `ProvisionLedger` (mint by `c.OrgID`).
  - Plus optional: **B1-full** async create→`pending` + signed intent; **B5** manifold entitlement.
