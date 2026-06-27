# city-provisioner — deploy runbook (B3 INFRA 4)

The autonomous Gas City provisioner. Client-only daemon on identity-v0. All provisioning CODE
(discovery, mint spine, beads + controller adapters, RunPass) is built, red-teamed (SHIP), and
unit-tested in the crucible (forge) repo under `internal/cityprovision` + `cmd/city-provisioner`.
This runbook is the one-time bootstrap + deploy.

## Prerequisites (must be live first)

1. **Accounts machine-only scopes (B3 CODE 1)** — `crucible:city.provision` + `crucible:city.work`
   registered in `internal/accounts/scopes.go` (commit `dbc562a`) and deployed. Until live, no key
   can carry these scopes and the spine cannot mint.
2. **B0 `provisioningToken`** (Accounts `gateProvision` machine path) deployed — the per-org bearer
   the spine authenticates with.
3. **Crucible cities surface (B3 CODE 2-3)** deployed: `GET /v0/cities/pending/orgs` + the
   `city.provision`/`city.work` route gates (commit `e5d990b`). And the crucible server must be
   started with `UseCityDiscoverySubjects([...the platform SP subject...])` (step 3 below).
4. **STS signer for `beads`** — confirm `beads` is in the deployed STS `STS_PRODUCTS` /signer map
   (it is, per the sts app: `manifold,crucible,beads,recall`). If it were absent, the beads leg would
   fall back to the interim per-org `bts_` token (the beads adapter is agnostic). ✅ resolved: beads
   is a signer.
5. **Controller image (B6)** — `CONTROLLER_IMAGE` must exist and, on boot, run the controller against
   its injected env (`GC_CITY_NAME`, `GC_PACK`, `GC_DOLT_DATABASE`, `GC_WORKSPACE_ID`). Tracked
   separately; the provisioner launches it but does not build it.

## One-time bootstrap

### 1. Mint the durable platform discovery key
`city.provision` is platform/machine-only and cannot be self-minted through the org plane — seed it
via a break-glass/admin path (the same plane that seeds other platform SPs), NOT the org-bound machine
route:
- Create a platform ServicePrincipal (e.g. `city-provisioner-platform`).
- Mint a **crucible** per-app key on it carrying EXACTLY `crucible:city.provision` (no other scope).
- Store the reveal-once key secret in OpenBao:
  `bao kv put product-eu/apps/identity-v0/city-provisioner/platform key=<secret>`
- Note the platform SP's `sp_id` (its EIA subject) for step 3.

### 2. Enroll each org's provisioning token
For every org that may create cities, seed its B0 provisioningToken:
```
bao kv put product-eu/apps/identity-v0/city-provisioner/org-tokens/<org_id> value=<provisioningToken>
```
ESO `dataFrom.find` materializes each as a file named `<org_id>` under `PROVISIONING_TOKENS_DIR`; the
provisioner picks it up on the next pass. (De-enroll = delete the bao secret.)

### 3. Allow-list the platform subject on crucible
The discovery endpoint is double-gated: scope `city.provision` AND an allow-listed subject. Wire the
platform SP's `sp_id` (step 1) into crucible's `UseCityDiscoverySubjects(...)` (its deployment config)
so it is the only subject permitted to enumerate. Without this, discovery 403s (fail-closed).

### 4. ghcr-pull
Ensure `product-eu/apps/identity-v0/ghcr-pull` exists (shared with the other identity-v0 apps).

## Deploy

1. Resolve the REVIEW notes in `city-provisioner.yaml` (the `CRUCIBLE_URL` machine-edge route +
   `CONTROLLER_IMAGE`) and `city-provisioner-egress-netpol.yaml` (replace the broad ipBlock with the
   real public/tailnet egress, or attach the standard identity-v0 public-egress component).
2. Copy this dir to `_infra/identity-v0/flux/apps/city-provisioner/` and add it to the apps
   kustomization. CI must build + publish `cmd/city-provisioner` (crucible repo) and pin it via Flux
   image-automation (`id-city-provisioner`).
3. Let Flux sync.

## Verify

- Pod is Running (no Service/probes — it's a poller; check logs, not readiness).
- Logs show `city-provisioner started` and, when an org has a pending city,
  `provision pass complete cities=N` (or `provision pass had failures` with a per-org error).
- End-to-end: create a city via the Forge wizard → it shows `provisioning` → within a pass interval
  the provisioner discovers the org, provisions the beads ledger + controller, `POST .../complete`s →
  the wizard shows `ready`.
- Security spot-checks: the per-org work EIA carries `sandbox.* + city.work` (NOT `city.provision`);
  discovery returns org_ids only; no secret appears in logs.

## Rollback / kill-switch

- `flux suspend` the kustomization, or scale the deployment to 0.
- Revoke a compromised org: disable its provisioner SP / set its `provisioning_token.status=revoked`
  in Accounts (immediate; the next mint fails closed). Delete its bao org-token to de-enroll.
- Revoke discovery: remove the platform SP from crucible's `discoverySubjects` (discovery 403s).
