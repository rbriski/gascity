# S1 Golden-Thread — Deploy Runbook (v2, post red-team)

Operational sequence to make one tailnet user's local `bd` reach one hosted **Dolt** project over a
publicly-trusted (WebPKI) gateway cert. **v2 folds in the Fable red-team's 3 confirmed blockers.** Every
live step has a canary + rollback. Code fixes (bd ServerMode gate, provision compare-then-write, gateway
observability) are tracked separately and MUST be merged before their deploy step.

## Corrected framing (red-team, confirmed)
- **The maintainer-city controller IS flipped onto the new cert at rollout — this is NOT "additive, zero
  risk to the controller."** It dials `GC_DOLT_HOST=gw.beads.gascity.com:3306`, so its TLS ClientHello SNI
  equals `BEADS_GATEWAY_PUBLIC_HOST` → `sniGetCertificate` serves it the WebPKI leaf. It is SAFE only
  because the controller image trusts WebPKI roots (verify in step 2) — and only if a **production** LE
  cert (never staging) enters the Secret. A staging cert would break the one production client instantly.
- **Deploy vehicle is the digest-pinned cosign OCI release bundle, NOT Flux image-automation.** Live pin:
  `OCIRepository corp-public-ha-release` @ `sha256:afc06c39…`, consumed by `Kustomization corp-public-ha-oci`.
  A deliberate deploy = a manifest edit on infra main + the new bts image both cut into a bundle, then the
  OCIRepository digest bumped. There is no "drop 3 env vars = instant rollback"; rollback = re-pin the prior
  bundle digest (or, emergency-only, a direct `kubectl` patch that the next reconcile will revert).
- **Internal-cert hot-reload is a new (beneficial) delta**, not "byte-identical": the eager CertSource now
  reloads the internal cert on mtime change instead of at boot only. A loadable-but-wrong internal rotation
  would now go live on next handshake; torn/corrupt rotations stay safe (last-good retained). Documented, intended.
- **Backfill of the 15 existing Dolt projects is DEFERRED off S1** (separate task): S1 provisions a NEW
  born-with Dolt project, so nothing rewrites a live project's identity. The provision code fix
  (compare-then-write, fail-closed on mismatch) still lands and becomes the safe primitive that future backfill uses.

## Credential reality (verified) — the adaptation
- ✅ root/kubectl to corp-public-ha; git push all backend private repos; the CF token on corp-public-ha-1
  (`/etc/caddy/corp-public-cloudflare.env`, active, `gascity.com` zone `1a6de895…`).
- ❌ NO bao-write (`claude-wrapper-read`) and no SOPS age key → the fully-GitOps cert-renewal storage is a follow-up.
- **Bootstrap cert:** issue via `lego` DNS-01 using the CF token **transiently on the box** (do NOT copy the
  shared whole-zone token into a k8s Secret — red-team blast-radius). Deliver as a **directly-created** Secret
  `beads-gateway-tls-public`. Renewal CronJob + a **dedicated** CF token in bao/SOPS = the durable follow-up.

## Sequence (dependency-safe; ⚠ = live prod)

**1. Code merged / PR'd (no deploy yet).**
- bd (OSS `gastownhall/beads`): open PR, label `status/needs-review-auto`. NOT deployed to the fleet — the
  partner runs a pinned build. (ServerMode-gate + write-back-gate fixes included.)
- beads-team-server: merge `feat/s1-provision-contract-and-sni` → origin/main so CI builds the image. This
  ARMS the change for the next bundle promotion — it does not deploy by itself, but note anyone bumping the
  bundle ships it; diff the bundle delta before the digest bump in step 6.

**2. Trust confirmation (read-only, GATING):** `docker run --rm --entrypoint sh <controller-image> -c 'ls
/usr/local/share/ca-certificates; awk "/BEGIN/{n++}END{print n}" /etc/ssl/certs/ca-certificates.crt'` →
expect the gas-city CA + a full (~140+) Mozilla bundle. If the bundle is missing WebPKI roots, STOP — the
controller would break on the cert flip.

**3. ⚠ DNS:** CF API — create `gw.beads.gascity.com` A → `100.109.51.65`, DNS-only (grey). NXDOMAIN publicly
today (no wildcard covers it) → unroutable off-tailnet until S7. Fix cherry `/etc/hosts` stale
`100.119.244.94 gw.beads.gascity.com` → `100.109.51.65` (dead v0 IP; breaks host-side tooling). Canary:
`getent hosts gw.beads.gascity.com` on cherry + a tailnet host. mc controller unaffected (in-cluster coredns-custom).

**4. ⚠ Cert issuance:** `goacme/lego` container, DNS-01, `CF_DNS_API_TOKEN=<shared token, passed via env,
not persisted>`, LE **staging first** (dry run) → then **production**, for `gw.beads.gascity.com`. Keep
staging and production artifacts in SEPARATE directories.

**5. ⚠ Cert delivery — with the production-chain GATE (red-team blocker):** BEFORE creating the Secret, verify
the production fullchain: `openssl verify -CApath /etc/ssl/certs fullchain.pem` (chains to a real WebPKI root,
NOT staging "Fake LE"); `openssl x509 -in fullchain.pem -noout -ext subjectAltName -dates` shows
`DNS:gw.beads.gascity.com` + sane NotBefore/NotAfter; confirm the intermediate is present (fullchain, not
leaf-only). Only then: `kubectl -n beads create secret generic beads-gateway-tls-public
--from-file=crt=fullchain.pem --from-file=key=privkey.pem` — **generic with `crt`/`key` keys** (matches the
gateway env paths + the internal-cert convention; NOT `create secret tls`, which forces `tls.crt`/`tls.key`
and would silently no-op the mount). No canary yet — the gateway ignores it until step 6's SNI code is live.

**6. ⚠ Gateway deploy (SNI code + manifest, via the release bundle):**
- Edit infra `corp-public/flux/apps/beads/beads-gateway.yaml`: add env `BEADS_GATEWAY_TLS_PUBLIC_CERT=/etc/beads-gateway-tls-public/crt`,
  `BEADS_GATEWAY_TLS_PUBLIC_KEY=/etc/beads-gateway-tls-public/key`, `BEADS_GATEWAY_PUBLIC_HOST=gw.beads.gascity.com`;
  volume `gateway-tls-public` from Secret `beads-gateway-tls-public` (`optional: true`, `defaultMode: 292`,
  whole-secret mount — NOT subPath); commit to infra main.
- Cut/refresh the release bundle so it contains BOTH the manifest edit and the new bts image; diff the bundle
  delta for anything else queued.
- **Deliberate deploy = bump `OCIRepository corp-public-ha-release` digest** to the new bundle (`kubectl apply`).
- CANARY, continuous around the bump: mc controller `beads cache: reconciled` keeps flowing
  (`kubectl -n gc-controller logs controller-maintainer-city-34471ec7935fc6 --since=5m`); haproxy
  `beads_gateway_in` rate steady; NO new gateway TLS errors; the gateway logs the served public leaf's
  SAN/Issuer/NotAfter at first lazy load (the observability fix) — confirm Issuer = production LE + SAN
  gw.beads.gascity.com. Immediately: `openssl s_client -starttls mysql -connect gw.beads.gascity.com:3306
  -servername gw.beads.gascity.com </dev/null | openssl x509 -noout -issuer` → Let's Encrypt; and
  `-servername beads-gateway.beads.svc` → Gas City Beads Data-Plane CA (internal path unchanged).
- **Rollback:** re-pin the prior OCIRepository digest (reverts manifest + image together). Emergency: `kubectl
  -n beads set env deploy/beads-gateway BEADS_GATEWAY_PUBLIC_HOST-` (reconcile reverts it, but buys minutes).

**7. ⚠ Provisioning-contract deploy + NEW partner project (no live-ledger backfill):** the provision.go change
rides the same bundle/image as step 6. Provision a NEW **Dolt** partner project: org-default (`workspace_id=''`
— `BEADS_WORKSPACE_TIER_ENABLED=1` fences human EIAs out of workspace-owned projects), backend pinned
`cherry.dolt.dedicated` at create (default is pg — assert `backend_id` on the row). It is born with `_project_id`
+ `issue_prefix` (compare-then-write, so a re-drive is a safe no-op). Canary: raw SQL session via the gateway →
`SELECT` metadata → `_project_id` present + matches, `issue_prefix` set. DO NOT run the 15-project backfill in S1.

**8. Exit tests:** (A) fresh tailnet host: `gasworks login` → env (HOST=gw.beads.gascity.com:3306, TLS=true,
CREDENTIAL_COMMAND="gasworks getToken beads --org <partner>") → `bd init --server --database bd_<partner-id>`
(adopts identity, no generation) → `bd list` → `bd create` → `bd list`. No CA bundle, no fingerprint, no
skip-verify. (B) second entitled org's token → `bd list` fails scrubbed 1045 (not a hang); non-entitled org →
mint-side 403. (C) no-production-client-broke: the step-6 canaries, before/during/after. (D) born-with: the step-7 SQL check.

**9. Renewal (durable follow-up, needs a credential I lack):** CronJob `beads-acme-renew` in ns beads runs
lego (a **dedicated** CF token, not the shared Caddy one), patches `beads-gateway-tls-public` (`crt`/`key`)
via a SA scoped to patch that one Secret, renews at `--days 30`, and an expiry probe fails <21d wired to a
named alert. Store the CronJob + RBAC in infra Flux; the dedicated token in bao→ESO or SOPS. **Blocked on
bao-write or the SOPS age key — surface to the owner.** Until then, the bootstrap cert is valid 90 days.

## Deferred off S1 (separate tasks)
- **Backfill** `_project_id`/`issue_prefix` over the 15 existing Dolt projects (incl. live mc
  `prj_36e911632d13aae6`): implement as an explicit compare-then-write dry-run tool (absent→write, equal→noop,
  different→skip+log, NEVER a ProvisionProject re-drive), with a pre-flight evidence dump. Not needed for S1.
- **Durable cert renewal** (step 9) — needs bao-write / SOPS.
