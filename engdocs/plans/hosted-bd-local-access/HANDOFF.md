# Hosted local-bd access — Session Handoff (2026-07-15, updated same day after S1 completion)

Handoff for the next session continuing the 9-slice program in
[`SPEC.md`](./SPEC.md). Read `SPEC.md` (the contract), `S1-DEPLOY-RUNBOOK.md`
(the deploy vehicle + gotchas), and the memories listed at the bottom.

**Tracking:** epic **`mc-b3clt`** in the maintainer-city bd ledger
(`cd /data/projects/maintainer-city && bd show mc-b3clt`). Children:
`mc-b3clt.1` (S1 golden thread), `mc-b3clt.2` (backfill), `mc-b3clt.3` (S1 finish — CLOSED).

---

## TL;DR — where things stand

**S1 is COMPLETE and FULLY VERIFIED end-to-end (2026-07-15). S2 was REDESIGNED
around a thin-bd pivot (2026-07-16) — design `S2-DESIGN.md` v4, grounded +
red-teamed; blocked on TWO OWNER DECISIONS before implementation (see below).**

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

## ▶ IMMEDIATE NEXT ACTION — S2 needs TWO OWNER DECISIONS, then implement

**S2 was REDESIGNED (2026-07-16) around a thin-bd pivot** and is at
`S2-DESIGN.md` **v4** in this dir (bead `mc-b3clt.6`). Do NOT implement from the
old v2 plan (`bd attach` + bd-side allowlist) — it was superseded.

**The pivot (owner direction):** *"the gasworks CLI owns authentication; bd only
delegates to a credential command."* This mirrors the parallel gc change
(worktree `.../frm`, branch `feat/unified-city-resolver`) that reverts gc-native
auth to a **credential command** (`internal/clientauth`, kubectl-exec-plugin
shape: versioned request passed to the helper via an **env var not argv**,
`{token, expiration}` back, the client makes **no** trust decision). bd already
has ~80% of this via `BEADS_DOLT_CREDENTIAL_COMMAND`; v4 adds the one missing
input — bd injects its **true dial host** into the helper env (`BEADS_EXEC_INFO`)
so the helper owns the destination decision. Deleted from bd: the whole v2/v3
trust apparatus (`bd attach`, bd-side allowlist, TOFU, `hosted.GuardDestination`,
`hosted.Resolve`).

**Design → grounded (2 Fable workflows) → red-teamed (v3 → 3 lenses, 7
blockers) → v4.** The v3 red-team was decisive and its fixes are in v4:
- The **fleet helper is `eia-helper`, NOT `gasworks`** (my v3 premise was
  factually wrong). A gasworks-only gate leaves the fleet — which processes
  untrusted repos in adopt-pr/PR-review — with **zero** destination gate. v4
  reframes enforcement as a **helper CONTRACT (§5.0) that EVERY helper
  implements** (gasworks + eia-helper).
- Dropped the **loopback exemption** (`127.0.0.1.attacker.example` dodged it) —
  bd ALWAYS injects; helpers **fail closed** on an `origin:bd` mint with no
  destination.
- **Dial host is a security-load-bearing cache key** in both bd and gasworks (a
  warm trusted token must never be served for an untrusted dial); per-dial host
  derived from `mysql.Config.Addr` so report==dial is structural.
- **Double-checked** refresh-token serialization; **warn-then-enforce** rollout.

### ⚠ TWO OWNER DECISIONS gate implementation (S2-DESIGN §11):
1. **Collapse v2's two mandated destination layers into the single helper
   contract?** SPEC CRITICAL #2 mandates two independent layers; the pivot
   deletes the bd one. Recommendation: (a) single helper-contract layer,
   *conditional on Decision 2 = yes*. Alt: (b) keep a thin bd/gc second layer
   (version-skew-proof, covers a non-enforcing helper).
2. **Is `eia-helper` destination-enforcement (WP-E) in scope now?** Without it
   the fleet has no gate. Recommendation: (a) in scope (crucible/gc change,
   fleet allowlist keyed on the split-DNS short name `beads`, not the FQDN); or
   at minimum (b) a compensating env-pin control before any fleet exposure.

**Do not implement until these are ruled.** I reverted my premature SPEC
CRITICAL #2 edit to a **PROPOSED** framing pending sign-off.

### Work packages once decided (Opus impl, Fable red-team before each commit):
- **WP-A (bd, OSS, no deps):** `BEADS_EXEC_INFO` always-inject + `origin:bd`
  marker + no-loopback-exemption + structural report==dial (`mysql.Config.Addr`)
  + host in bd's `internal/creds` cache key + narrow `strippedEnv` + shared
  canon; per-dial `openSQLDB` connector (port `1f0ebe068`, incl. mandatory
  `transaction.go:147`), gated ONE-shot 1045-invalidate, sentinel-DSN redaction,
  env timeouts.
- **WP-B (gasworks, PUBLIC repo, no deps):** §5.0 contract impl;
  `--gateway`/`--project` (+ hoistPositional); read exec-info-or-flag +
  neither-present fail-closed-for-bd + version-tolerance; gate before the cache
  read; gateway cache-key dim; `trust-gateway` (locked store); resilience
  (per-attempt STS timeout + budget + jitter + serve-last-good floor);
  `expires_in` fix; **§5.5 double-checked refresh serialization**. Ships
  **warn-only**, flips to enforce once WP-A is the default bd install.
- **WP-E (crucible/gc `eia-helper`) — pending Decision 2:** the §5.0 contract in
  `eia-helper`.
- **WP-D (bd, OSS):** doctor exhaustive gateway-blindness sweep + diagnostic
  contract (fixes `mc-b3clt.4`); doctor injects a representative
  `BEADS_EXEC_INFO` for its helper dry-run. Dep #4823.

Ship order **A ∥ B(warn-only) → flip B to enforce (once WP-A is default) →
E (if approved) → D**. Grounding dumps (session-local):
`/data/tmp/s2-grounding/{bd-main,credcmd,gasworks,pr4823}.md` +
`v3-redteam-{0,1,2}.json`. Gasworks origin/main ground worktree
`/tmp/gasworks-main-ground` (canonical `/data/projects/gasworks-go-build` was 7
commits stale). gc credential-provider pattern: worktree
`/data/projects/gascity/.claude/worktrees/frm`, branch
`feat/unified-city-resolver`, `internal/clientauth/clientauth.go`.

**Other owner flags (not decisions):** gasworks CLI has NO machine leg
(`GASWORKS_API_KEY`/`/sts/v0/machine` absent — "gasworks owns all auth" is
human-leg-only today, fleet uses `eia-helper`); Keycloak offline-session pinning
is a server-side change; gasworks repo is PUBLIC; bd release vehicle undecided.

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
- **`mc-b3clt.4`** — bd doctor/init-postcheck Gateway/TLS threading bug; fix = S2 v4 WP-D (exhaustive doctor sweep). Field report on PR #4823.
- **`mc-b3clt.5`** — beads-team-server create-request `issue_prefix` not persisted (slug-derived on re-drive).
- **`mc-b3clt.6`** — S2 (thin-bd pivot). Design v4 DONE + red-teamed; **blocked on the two owner decisions** above.
- Durable cert auto-renewal (CronJob + dedicated CF token; needs bao-write/SOPS) — see runbook §9. Bootstrap cert valid to 2026-10-13.
- beads-web connection recipe emits stale `gateway_host=100.119.244.94` (set `-gateway-endpoint` on the beads-web Deployment) — folds into S5's panel work.

---

## Remaining slices S2–S9 (from `SPEC.md` Rollout)
Each is its own **Fable design → Opus impl → Fable red-team → deploy/PR** cycle. Model policy stands: Fable arch/design, Opus impl, Fable red-team before commit; backend private repos push+deploy; bd = OSS PR (contributors.md + template, `status/needs-review-auto`, no commercial language).

- **S2** — thin-bd credential delegation; the credential helper owns auth + destination trust (v4 pivot). Design done + red-teamed; blocked on two owner decisions. Superseded the old `bd attach` + bd-side-allowlist scoping.
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
`s1-hosted-bd-webpki-gateway-live`, `hosted-bd-s2-design-redteamed`,
`hosted-bd-local-access-spec`, `beads-awsusw2-backend-unroutable-fix`,
`beads-1045-hosted-controller-rca`, `gascity-scope-taxonomy`,
`mc-exports-rehomed-gascityinc`.

---

## NEXT-SESSION PROMPT (copy-paste)

> Continue the hosted-local-bd program (epic `mc-b3clt`, SPEC at
> `engdocs/plans/hosted-bd-local-access/SPEC.md`). Read `HANDOFF.md`,
> `S2-DESIGN.md` (**v4**), and `S1-DEPLOY-RUNBOOK.md` in that dir first, plus
> memories `s1-hosted-bd-webpki-gateway-live` and `hosted-bd-s2-design-redteamed`.
> Model policy: Fable arch/design, Opus implementation, Fable red-team before
> every commit; backend private repos push+deploy; bd = OSS PR (contributors.md +
> template, `status/needs-review-auto`, NO commercial language). Constraints: no
> bao-write, no SOPS key; corp-public-ha deploys via OCI release-bundle digest
> bump; cherry runs its own k3s (Flux-GitOps from gasworks-internal main) for
> Dolt backends.
>
> **S1 is COMPLETE + verified** (WebPKI gateway live; cherry provisioner on
> `d090d99` so fresh Dolt projects are born with `_project_id`; `bd init --server`
> adopts + `bd list`/`create` work over WebPKI TLS + a getToken EIA on test
> project `prj_848513b16e7b5c43`). Verify the gateway is still healthy first
> (commands under "Re-establish state").
>
> **S2 is the thin-bd pivot (bead `mc-b3clt.6`, design v4).** bd becomes a pure
> credential-delegator that injects its true dial host into the helper env
> (`BEADS_EXEC_INFO`); the credential helper (gasworks AND fleet `eia-helper`)
> owns the destination-trust decision via the §5.0 contract. The old `bd attach`
> + bd-side-allowlist plan is DELETED. Design is done + red-teamed.
>
> **BEFORE implementing, get the owner to rule on the two decisions in
> `S2-DESIGN.md §11`:** (1) collapse v2's two mandated destination layers into the
> single helper contract? (2) is `eia-helper` enforcement (WP-E) in scope now, or
> is the fleet gap accepted with a compensating env-pin control? These change the
> work-package set. Then implement WP-A (bd exec-info + connector) ∥ WP-B
> (gasworks contract + resilience, warn-only) → flip B to enforce once WP-A is the
> default install → WP-E (if approved) → WP-D (doctor sweep = fixes `mc-b3clt.4`).
>
> Also open: bd PR #4823 review status (OPEN, `status/reviewing`, checks green —
> do NOT merge without review); backfill `mc-b3clt.2` (do NOT run against the live
> mc ledger without the dry-run tool); durable cert renewal (needs bao-write/SOPS
> I lack — flag to the owner; bootstrap cert good to 2026-10-13).
