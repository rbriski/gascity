# Hosted local-bd access — Session Handoff (2026-07-15, updated same day after S1 completion)

Handoff for the next session continuing the 9-slice program in
[`SPEC.md`](./SPEC.md). Read `SPEC.md` (the contract), `S1-DEPLOY-RUNBOOK.md`
(the deploy vehicle + gotchas), and the memories listed at the bottom.

**Tracking:** epic **`mc-b3clt`** in the maintainer-city bd ledger
(`cd /data/projects/maintainer-city && bd show mc-b3clt`). Children:
`mc-b3clt.1` (S1 golden thread), `mc-b3clt.2` (backfill), `mc-b3clt.3` (S1 finish — CLOSED).

---

## TL;DR — where things stand

**S1 is COMPLETE and FULLY VERIFIED end-to-end (2026-07-15, session 2). S2 is in design.**

Session-2 completion evidence:
- Cherry's `beads-provisioner` deployed to `d090d99` (gasworks-internal PR #125,
  merged; Flux `platform-maintainer-dogfood-beads-provisioner` reconciled;
  NOTE: the GitHub repo is named `gasworks-control-plane` — `gasworks-internal`
  redirects). Cherry's beads stack is **Flux-GitOps from
  `github.com/gascity/gasworks-internal` main** (GitRepository `gasworks-k8s`),
  NOT the OCI-bundle vehicle corp-public-ha uses — image pins are one-line
  manifest edits in `gc-hosted/deploy/k8s/apps/`.
- Fresh Dolt project `prj_848513b16e7b5c43` (s1gt3, gascityinc,
  cherry.dolt.dedicated) **born with `_project_id`**; `bd init --server`
  **ADOPTED** it (`.beads/metadata.json` `project_id` matches); `bd list` +
  `bd create` work over WebPKI TLS (no CA bundle) + getToken EIA.
  **Keep `prj_848513b16e7b5c43` for S2 attach testing.** Session-1 throwaway
  `prj_b32adb114613402d` was DELETED (teardown exercised on the new image).
- mc controller canary unaffected (18 reconciles/10m during + after).
- Two findings filed as beads under `mc-b3clt`: (1) `bd doctor` + init
  post-check rebuild store config without Gateway/TLS → false
  "Failed to connect"/1045 "TLS required" on a WORKING hosted workspace
  (fix in S2's doctor work; field report posted on PR #4823);
  (2) beads-team-server does not persist the create-request `issue_prefix` —
  provisioner re-drives derive it from the slug (s1gt3 → S1GT3, requested
  s1gt ignored; bts TODO(phase5)).

The hard part — the TLS impasse (a laptop `bd` can't verify the gateway's
internal-CA cert) — is SOLVED and LIVE: the beads gateway now serves a
publicly-trusted (Let's Encrypt) certificate via SNI for `gw.beads.gascity.com`,
and a real `bd` client connected end-to-end over it with **no CA bundle**
(`TLS=true`, system roots) + a `gasworks getToken beads` EIA. Proven: DNS →
WebPKI TLS → EIA auth → gateway routing all work.

**Live state (verify before trusting):**
- `OCIRepository corp-public-ha-release` pinned to bundle `sha256:2f76a203a409…`
  (was `afc06c39…`). `kubectl -n beads get deploy beads-gateway` image = `@sha256:17ca015dacda`.
- Gateway logs `now serving public WebPKI leaf issuer-cn=YR1`. Pods 2/2.
- Public cert Secret `beads/beads-gateway-tls-public` (keys `crt`/`key`, LE prod, exp 2026-10-13).
- DNS `gw.beads.gascity.com A → 100.109.51.65` (Cloudflare, grey). cherry `/etc/hosts` fixed.

---

## ▶ IMMEDIATE NEXT ACTION — S2 IMPLEMENTATION (design + red-team DONE)

**S2 design is complete and red-teamed** — `S2-DESIGN.md` v2 in this dir
(bead `mc-b3clt.6`). A 3-lens Fable red-team broke v1 (all three lenses hit the
same CRITICAL: the env-credential carve-out reopened the SPEC exfil vector);
v2 fixed it at the root (unconditional destination gate at the mint boundary +
terminal hosted resolution). **Next is implementation** (Opus impl, Fable
red-team before each commit):

- **WP-A (bd, OSS, no deps):** `creds.Source` interface + `ArgvSource` +
  `ResolveForDial`/`Invalidate`, per-dial `openSQLDB` connector (port from
  `1f0ebe068` incl. the mandatory transaction.go:147 conversion), gated
  one-shot 1045-invalidate, sentinel-DSN token redaction, env-tunable timeouts.
- **WP-B (gasworks, PUBLIC repo, no deps):** `--gateway`/`--project` flags
  (+ hoistPositional), UNCONDITIONAL destination gate before the cache read,
  `trust-gateway` (locked store), mint resilience (per-attempt STS timeout +
  budget + jitter + serve-last-good floor), `expires_in` envelope fix.
- Then **C1** (trust stores + `hosted.GuardDestination` + terminal
  `hosted.Resolve` + second-user detection; dep A+B, NOT #4823) → **C2**
  (`bd attach` + factored #4823 adoption helpers; dep C1+#4823-merge) → **D**
  (doctor exhaustive sweep = fixes `mc-b3clt.4`; dep #4823+C).

Ship order **A ∥ B → C1 → C2 → D**. Grounding dumps (session-local):
`/data/tmp/s2-grounding/{bd-main,credcmd,gasworks,pr4823}.md`. Gasworks
origin/main ground worktree: `/tmp/gasworks-main-ground` (canonical checkout
`/data/projects/gasworks-go-build` was 7 commits stale). Verified
provisioner-path fact: `provisionerLoop → resumeProvisioning → driveProvision →
runProvision → provision.ProvisionProject`.

**Open owner flags from the red-team (in S2-DESIGN §9):** gasworks CLI has NO
machine leg (`GASWORKS_API_KEY` unbuilt) so `BD_TRUST_WORKSPACE=1` ships
degraded; gasworks repo is PUBLIC (hosted defaults in OSS); bd release vehicle
undecided.

**Golden-thread test recipe (S1-proven; reuse project `prj_848513b16e7b5c43`):**
```bash
# provision a born-with Dolt project (in-cluster, via a gascityinc beads EIA):
EIA=$(gasworks getToken beads --org gascityinc --json | python3 -c 'import json,sys;print(json.load(sys.stdin)["access_token"])')
# ssh root@100.109.51.65: kubectl -n beads port-forward svc/beads-web 18099:80 &
# curl -X POST http://127.0.0.1:18099/beads/api/projects -H "X-Gc-Identity: $EIA" -H 'Content-Type: application/json' \
#   -d '{"name":"s1gt2","prefix":"s1gt","workspace_id":"","token":"none","backend":{"history":true,"zone":"cherry","tenancy":"dedicated"}}'
# then from cherry (gw.beads resolves to 100.109.51.65 via /etc/hosts):
export BEADS_DOLT_SERVER_HOST=gw.beads.gascity.com BEADS_DOLT_SERVER_PORT=3306 BEADS_DOLT_SERVER_TLS=true
export BEADS_DOLT_CREDENTIAL_COMMAND='gasworks getToken beads --org gascityinc'
/data/tmp/bd-s1-gateway-init/bd init --server --database bd_prj_<id>   # must ADOPT identity, not error
/data/tmp/bd-s1-gateway-init/bd list
```
Live S2 test project: `prj_848513b16e7b5c43` (s1gt3, gascityinc, ready, 4 issues).
Initialized workspace on cherry: `/data/tmp/s1gt3-goldenthread` (adopted identity).

---

## Open PRs
| PR | Repo | State | Notes |
|----|------|-------|-------|
| **#4823** | `gastownhall/beads` (OSS) | **OPEN, in review** | `feat/gateway-init-adoption`. Label moved to `status/reviewing` (auto-reviewer picked it up); all checks green. Field report on the doctor TLS gap posted as a comment. DO NOT merge without review. |
| #125 | `gascity/gasworks-internal` (= `gasworks-control-plane`) | MERGED 2026-07-15 | cherry beads-provisioner pin → d090d99 (born-with `_project_id`) |
| #67 | `gascity/beads-team-server` | MERGED (`d090d99`) | provision compare-then-write + dual-cert SNI |
| #1228 | `gascity/infra` | MERGED | gateway SNI env + lazy volume |

## Worktrees (uncommitted-safe; all pushed except infra which is merged)
- `/data/tmp/bd-s1-gateway-init` — bd branch `feat/gateway-init-adoption` (pushed, PR #4823). **Contains the built `bd` binary used for the golden thread.**
- `/data/tmp/bts-s1-provision-sni` — beads-team-server (merged; keep for reference to the code).
- `/data/tmp/infra-s1-gateway` — infra (merged).

## ⚠ Access / credential constraints (LOAD-BEARING — don't rediscover)
- **NO bao-write.** My token is `claude-wrapper-read` (`BAO_ADDR=https://bao.ops.gascity.com`). Can read some paths, cannot write. This blocks the fully-GitOps durable cert renewal.
- **No SOPS age key** for infra-encrypted secrets.
- **Cloudflare:** the token in `/etc/caddy/corp-public-cloudflare.env` on corp-public-ha-1 edits `gascity.com` DNS + drives ACME DNS-01. It is the SHARED Caddy token — for durable renewal, mint a DEDICATED token (red-team blast-radius).
- **HAVE:** root SSH + kubectl to corp-public-ha (`root@100.109.51.65`) and identity-ha (`root@100.126.130.105`); `sudo k3s kubectl` to cherry's local k3s; git push to all backend private repos (gascity org) + gastownhall/beads; `gasworks getToken beads --org <org>` works.

## ⚠ Deploy-vehicle gotchas
- **corp-public-ha deploy = OCI release-bundle DIGEST bump, NOT Flux image-automation.** Resolve `:main` via the GHCR registry API (see `S1-DEPLOY-RUNBOOK.md` §6 for the exact curl), VERIFY the bundle's rendered manifest contains your changes (pull the layer, grep), then `kubectl -n flux-system patch ocirepository corp-public-ha-release --type merge -p '{"spec":{"ref":{"digest":"<new>"}}}'` + reconcile. Rollback = re-pin the prior digest.
- **cherry runs its OWN k3s** for Dolt backends (dolt-prj-* + beads-provisioner + beads-gateway-pg). Deploys that touch Dolt provisioning must target BOTH clusters.
- `gh pr edit --add-label` is broken (projects-classic) — use `gh api -X POST repos/<o>/<r>/issues/<n>/labels -f "labels[]=<label>"`.
- Infra PRs need path-gated checks DISPATCHED (`gh workflow run gates.yml/manifests.yml/iac.yml --ref <branch>`) then admin-merge.

## Follow-up beads (under `mc-b3clt`)
- **`mc-b3clt.3`** — CLOSED 2026-07-15 (deployed + golden thread verified).
- **`mc-b3clt.1`** — golden thread PROVEN; stays open only until bd PR #4823 merges.
- **`mc-b3clt.2`** — backfill `_project_id`/`issue_prefix` over the existing Dolt projects (incl. LIVE mc `prj_36e911632d13aae6`). Compare-then-write dry-run tool; NEVER a ProvisionProject re-drive. Not on S1's path.
- bd doctor/init-postcheck Gateway/TLS threading bug (bead filed; fix rides S2's doctor work).
- beads-team-server create-request `issue_prefix` not persisted — slug-derived on provisioner re-drive (bead filed).
- Durable cert auto-renewal (CronJob + dedicated CF token; needs bao-write/SOPS) — see runbook §9. Bootstrap cert valid to 2026-10-13.
- beads-web connection recipe emits stale `gateway_host=100.119.244.94` (set `-gateway-endpoint` on the beads-web Deployment) — folds into S5's panel work.

---

## Remaining slices S2–S9 (from `SPEC.md` Rollout)
Each is its own **Fable design → Opus impl → Fable red-team → deploy/PR** cycle. Model policy stands: Fable arch/design, Opus impl, Fable red-team before commit; backend private repos push+deploy; bd = OSS PR (contributors.md + template, `status/needs-review-auto`, no commercial language).

- **S2** — `bd attach` + per-dial minting + the destination-trust allowlist (+ gasworks `--gateway`). The next real client slice after S1.
- **S3** — least-privilege pinned scopes (`beads:read#prj_<id>`) + membership severance (GA gate).
- **S4** — credential custody: DPoP key → OS keyring, no plaintext EIA at rest, server-side EIA exp-iat ceiling (GA gate).
- **S5** — connection API + Connection panel + docs (retire the fictional panel; fix the stale `gateway_host`).
- **S6** — Postgres-engine projects tailnet-first + bd hosted-pg mode (the new-project majority).
- **S7** — public exposure (PROXY-v2 anti-spoof, public A-record + haproxy bind).
- **S8** — splice/session tuning (no TTL raise).
- **S9** — enterprise CA-pin knob + bts_ sunset + cleanup.

## Re-establish state (run these first next session)
```bash
cd /data/projects/maintainer-city && bd show mc-b3clt        # program state
gh pr view 4823 --repo gastownhall/beads --json state,statusCheckRollup   # bd PR review status
ssh root@100.109.51.65 'kubectl -n beads get deploy beads-gateway -o jsonpath="{.spec.template.spec.containers[0].image}"; kubectl -n beads logs deploy/beads-gateway -c beads-gateway --since=10m | grep -i "public WebPKI"'
# SNI still serving WebPKI (from cherry):
openssl s_client -starttls mysql -connect gw.beads.gascity.com:3306 -servername gw.beads.gascity.com </dev/null 2>&1 | grep -iE 'verify return|issuer'
```

## Related memories
`s1-hosted-bd-webpki-gateway-live`, `hosted-bd-local-access-spec`,
`beads-awsusw2-backend-unroutable-fix`, `beads-1045-hosted-controller-rca`,
`gascity-scope-taxonomy`, `mc-exports-rehomed-gascityinc`.

---

## NEXT-SESSION PROMPT (copy-paste)

> Continue the hosted-local-bd program (epic `mc-b3clt`, SPEC at
> `engdocs/plans/hosted-bd-local-access/SPEC.md`). Read `HANDOFF.md` and
> `S1-DEPLOY-RUNBOOK.md` in that dir first, plus memory
> `s1-hosted-bd-webpki-gateway-live`. Model policy: Fable arch/design, Opus
> implementation, Fable red-team before every commit; backend private repos
> push+deploy; bd = OSS PR (contributors.md + template, `status/needs-review-auto`,
> NO commercial language). Constraints: no bao-write, no SOPS key; corp-public-ha
> deploys via OCI release-bundle digest bump (not Flux image-automation); cherry
> runs its own k3s for Dolt backends.
>
> **Step 1 — finish S1 (`mc-b3clt.3`):** deploy the provision-contract image
> (`d090d99`/`17ca015`) to cherry's `beads-provisioner` so hosted Dolt projects are
> born with `_project_id`, then re-run the golden thread (recipe in HANDOFF.md) and
> confirm `bd init --server` adopts identity + `bd list` works against a fresh Dolt
> project. Verify the live WebPKI gateway is still healthy first.
>
> **Step 2 — S2:** design (Fable) → implement (Opus) → red-team (Fable) → ship
> `bd attach` + per-dial credential minting + the destination-trust allowlist
> (+ gasworks `--gateway`), per the SPEC's S2 slice.
>
> Also open: bd PR #4823 review status; backfill `mc-b3clt.2` (do NOT run against
> the live mc ledger without the dry-run tool); durable cert renewal (needs a
> credential I lack — flag to the owner).
