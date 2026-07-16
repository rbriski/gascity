# S2 Design — thin-bd credential delegation; gasworks owns auth + destination trust

> Fable design, **v3 (thin-bd pivot)**, 2026-07-16. Supersedes v2. Grounded by
> two Fable workflows (a 4-reader current-state pass + a 3-reader pivot pass
> over the gc credential-provider pattern, bd's delegation surface, and gasworks
> as trust authority) and a 3-lens v2 red-team. Grounding dumps (session-local):
> `/data/tmp/s2-grounding/*.md`. Implements the SPEC's Rollout item **S2**
> (SPEC needs the companion edits in §10). Status: DESIGN — Opus impl, Fable
> red-team before each commit.

> **v3 changelog — the pivot.** Owner direction: *"the gasworks CLI should own
> the authentication to gasworks and beads; bd should just have support to
> delegate to a command line to fulfill the credential requirements."* This
> mirrors the parallel gc change (worktree `.../frm`, branch
> `feat/unified-city-resolver`) that reverts gc-native auth to a **credential
> command** delegating to `gasworks getToken` — the kubectl
> ExecCredential / git-credential-helper shape (`internal/clientauth`:
> versioned protocol, request passed to the helper via an **env var not argv**,
> `{token, expiration}` back, the client owns no key and makes **no
> destination-trust decision**).
>
> v2 built a large **bd-native** trust apparatus (a user-level trusted-gateway
> allowlist *in bd*, a `bd attach` trust ceremony, TOFU attachment records,
> `hosted.GuardDestination` inside bd, a terminal `hosted.Resolve`). This design
> **deletes all of it** and moves the trust decision into the **credential
> helper**. bd reverts to a pure delegator that **reports its true dial host to
> the helper** (`report == dial`); the helper makes the allowlist call.

> **v4 changelog (Fable red-team of v3 — 3 lenses, 7 blockers; the pivot
> direction survives but its "closes the CRITICAL at the root" claim did not,
> and one design premise was factually wrong).** Fixes folded in:
> - **The fleet helper is `eia-helper`, NOT `gasworks`.** v3 §4 wrongly said
>   `mirrorBeadsDoltEnv` produces a `gasworks getToken` command; it produces an
>   `eia-helper` command (an STS machine minter reading only
>   `ORCHESTRATOR_KEY_FILE`/`EIA_AUDIENCE`/…, no destination input). A
>   gasworks-only gate would leave the **fleet — the population that processes
>   untrusted repos in adopt-pr/PR-review — with zero destination gate.** v4
>   reframes destination enforcement as a **credential-helper CONTRACT that
>   EVERY helper implements** (gasworks AND eia-helper), not a bd-side allowlist.
>   Keeps bd thin (owner's intent) *and* covers the fleet.
> - **Drop the loopback exemption** — a prefix/CIDR-string loopback check lets
>   `127.0.0.1.attacker.example` suppress exec-info; bd now **always injects**
>   exec-info on hosted (credential-command) server-mode dials, and helpers
>   **fail closed** when a bd-originated mint carries no destination (a marker
>   distinguishes bd-originated from a direct human `gasworks getToken`, which
>   keeps minting).
> - **The dial host is a MANDATORY, security-load-bearing cache-key component**
>   in *both* bd's `internal/creds` cache and the gasworks EIA cache (v3's
>   "optimization" was the linchpin — a warm trusted token must never be served
>   for an untrusted dial). On the per-dial path bd derives the reported host
>   from the driver's `*mysql.Config.Addr` so report==dial is *structural*.
> - **Refresh-token serialization** must be the double-checked pattern (fast
>   unlocked freshness check → flock → re-check under lock → refresh only if
>   still stale, bounded); a naive lock-across-refresh serializes N Keycloak
>   RTTs and in-process single-flight is cross-process-useless.
> - **Warn-then-enforce rollout** + explicit neither-destination policy: ship
>   the helper gate warn-only until the exec-info-injecting bd tag is the
>   default, then flip to enforce (WP-A lands before the gate enforces).
> - **Two decisions are the owner's, not mine** (§11): collapsing v2's two
>   mandated destination layers into the helper contract (a SPEC CRITICAL #2
>   change), and whether `eia-helper` enforcement is in scope now or the fleet
>   gap is accepted meanwhile. I reverted my premature SPEC CRITICAL #2 edit to
>   a *proposed* framing pending that sign-off.
>
> Removed: v2 §2 (whole destination-trust model), §2.3 metadata helper fields,
> §4 (`bd attach`), `hosted.GuardDestination`/`hosted.Resolve`, attachment TOFU,
> `creds.ArgvSource`, `BD_TRUST_WORKSPACE`, second-user detection. Kept: the
> per-dial connector / 1045-invalidate / timeouts (v2 §3), #4823 gateway-init
> adoption, the doctor sweep (v2 §6). New: the **helper destination-enforcement
> contract** (§5.0) that gasworks and eia-helper both implement; bd's exec-info
> injection (§2.2). Grown: gasworks (v2 §5) implements the contract + gains two
> reliability fixes.

## 0. The model in one picture

```
  bd (thin delegator)                         gasworks getToken (sole auth owner)
  ─────────────────────                       ──────────────────────────────────
  resolve dial host H (once)  ── exec, with ──▶ read BEADS_EXEC_INFO {dialHost:H}
  present token as wire user      env  ────────  H ∈ trusted-gateway allowlist?
       ▲                          BEADS_EXEC_INFO      │no → refuse (exit 1, stderr)
       │  {token, expiration}     (not argv)           │yes
       └──────────────────────────────────────────────┴─ mint EIA (refresh→session→
  dial H, present token                                   exchange, all noninteractive)
```

bd's entire security contribution is the invariant **"the destination I report
to the helper is the destination I dial"** — compute the host once, pass it,
dial it. Every trust judgment ("is `H` trusted?"), every credential
(refresh-token, DPoP key, EIA), and every mint lives in gasworks. bd holds no
key and no allowlist. This is the kubectl exec-credential-plugin idiom bd's own
`internal/creds` doc comments already claim (command.go:15-21) — v3 finishes it
by threading the one missing input (the dial host) into the helper.

## 1. Scope

**In — bd (OSS `gastownhall/beads`):** per-dial credential connector +
1045-invalidate-retry (port of `1f0ebe068`); **exec-info host threading** (the
one net-new security piece); env-tunable wire timeouts; doctor
gateway-blindness sweep + diagnostic contract. Plus the already-open #4823
gateway-init identity adoption (unchanged).

**In — gasworks CLI (public `gascity/gasworks`):** `--gateway`/`--project`
flags; **the** trusted-gateway allowlist (sibling file) + `trust-gateway`;
destination gate (reads the flag or bd's exec-info env) before the mint/cache;
gateway dimension in the cache key; mint resilience (jitter/backoff/per-attempt
timeout/serve-last-good); `expires_in` envelope fix; and two reliability
fixes forced by sole-owner status: refresh-token rotation serialization and
Keycloak offline-session pinning.

**Out (unchanged from SPEC):** pinned scopes (S3 — already gasworks-side),
keyring custody (S4 — already gasworks-side), panel (S5), pg mode (S6), public
exposure (S7), splice tuning (S8). **Out (newly, by the pivot):** `bd attach`,
any bd-side allowlist/TOFU, persisted-helper metadata. **Out (gap, owner
flag):** the gasworks machine/CI leg (`GASWORKS_API_KEY` → `/sts/v0/machine`)
does not exist in the CLI yet; "gasworks owns ALL auth" is only true for the
human leg today.

## 2. The delegation contract (bd → credential command)

### 2.1 What exists today (already ~80% of thin-bd)

`BEADS_DOLT_CREDENTIAL_COMMAND` (env-only, configfile.go:485-495) →
`creds.CommandSource{Kind:KindIdentity}` → `creds.ResolveLadder` (fail-closed)
→ `ApplyGatewayCredential` (gateway_credential.go:30-62) stamps
`ServerUser=token`, `Gateway=true`, `DisableAutoStart`. The command runs `sh -c`
(command.go:82-102), output parsed as a bare token or `{token|access_token,
expirationTimestamp|expires_in}` (command.go:144-178), cached by command string
until `expiry−10s`. **The env-only getter is the current trust boundary** —
there is no metadata-persisted command to remove (that is precisely what
configfile.go:490-492 refuses, and v3 keeps that refusal). bd already knows
nothing of the issuer.

### 2.2 The one net-new piece — exec-info host threading

**Problem (verbatim SPEC CRITICAL, confirmed by the v2 red-team):** bd passes
ZERO destination context to the command. `credRunner` runs `sh -c <command>`
and never sets `cmd.Env`, so the helper cannot know where bd is about to send
the token. A hostile repo commits `.beads/config.yaml dolt.host: evil.example`
(+ `dolt_mode=server`, `backend=dolt`); a victim with an ambient
`BEADS_DOLT_CREDENTIAL_COMMAND` (the fleet/S1 shape) runs any `bd` command,
mints a real token, and dials `evil.example` over verified TLS → token
harvested. gasworks never sees the destination, so it cannot refuse.

**Fix — bd injects its true dial host into the helper's environment**, the
kubectl `KUBERNETES_EXEC_INFO` / gc `GC_EXEC_INFO` pattern (env, never argv, so
it is not visible in `ps` and cannot be spoofed through the freeform command
string):

- New env var **`BEADS_EXEC_INFO`**, a versioned JSON payload:
  `{"apiVersion":"beads.dev/credential-exec/v1","spec":{"dialHost":"<canon
  host>","dialPort":3306,"database":"bd_prj_…"}}`. Minimal viable field is
  `dialHost`; `dialPort`/`database` are forward-compat (let a helper mint a
  project-pinned token without re-parsing the command).
- **Source of `dialHost` = the exact value bd dials.** At both resolution sites
  `cfg.ServerHost` is assigned from `GetDoltServerHost()` *before*
  `ApplyGatewayCredential` runs (open.go:222-245; main.go:1169→1186), and that
  same `cfg.ServerHost` is what `buildServerDSN` bakes and dials
  (store.go:1349-1357). bd reads `cfg.ServerHost` **at the mint choke** (not a
  re-resolution) so report-and-dial are provably the same string. Canonicalize
  once (lowercase, strip trailing dot, IDNA) and use that one form for the
  env, the DSN, and the cache key.
- **Threading:** add a `DialHost`/`DialPort`/`Database` field to
  `creds.CommandSource`; thread through `resolveCredentialToken` → `credRunner`,
  which sets `cmd.Env = append(strippedEnv(), "BEADS_EXEC_INFO="+json)`. On the
  **per-dial connector path**, derive `dialHost` from the driver's
  `*mysql.Config.Addr` INSIDE the `BeforeConnect` hook (the literal dial
  target) rather than a parallel captured field — so report==dial is
  *structural* on that path, not two callers feeding one runner (red-team).
- **ALWAYS inject on hosted server-mode dials — no loopback exemption.** v3's
  "omit for loopback" is deleted: a prefix/CIDR-string loopback classifier lets
  `127.0.0.1.attacker.example` (attacker-owned, real A-record + WebPKI leaf)
  masquerade as loopback, suppress the env, and — because the helper then sees
  no destination — mint and exfil. Whenever a credential command is in play in
  server mode, bd injects `BEADS_EXEC_INFO` for EVERY host. (There is no
  legitimate `gasworks getToken`/`eia-helper` dial to loopback — hosted access
  is always the gateway.) If any loopback shortcut is ever wanted, it MUST use
  `net.ParseIP(host).IsLoopback() || host=="localhost"` EXACTLY — never
  `HasPrefix`/substring/CIDR-string (bd already ships that bug class at
  gitlab.go:315) — and it may only affect whether the helper *requires* a
  match, never whether bd *reports* the host.
- **Bd-originated marker → helpers fail closed on absent destination.** The
  exec-info payload carries `"origin":"bd"` (and the versioned apiVersion). A
  helper that receives `BEADS_EXEC_INFO` with `origin:bd` but cannot resolve a
  trusted destination MUST refuse (fail closed). A direct human
  `gasworks getToken beads --org X` (no exec-info, no marker) keeps minting
  (fail open) — the two paths diverge cleanly, closing the "gasworks can't tell
  a bd call from a human call, so it can't fail-closed on absence" hole.
- **Cache key gains the host dimension — this is a SECURITY control, not an
  optimization.** bd's process-global `internal/creds` cache is keyed by command
  string only today (command.go:79,112) and is consulted BEFORE the helper
  runs, so a warm token minted for the trusted gateway is a cache HIT for a
  later dial to an attacker host with the same command → the gasworks gate never
  executes and bd hands the trusted EIA to the attacker. The (canonical) dial
  host MUST be part of the cache key in bd AND in the gasworks EIA cache
  (§5.3), with an adversarial test that a warm trusted token is never served
  for a different-host dial.
- **Env hygiene (from gc's `strippedEnv`):** strip ONLY an inherited
  `BEADS_EXEC_INFO` (narrow, exact var) before setting bd's own — never a broad
  `BEADS_*`/"sensitive-key" filter, which would drop the eia-helper's own inputs
  (`ORCHESTRATOR_KEY_FILE`, `EIA_AUDIENCE`, `BEADS_DOLT_SERVER_TLS`) and break
  fleet mint. Everything else is inherited as today (bd sets no `cmd.Env` now,
  so the child inherits the full parent env).
- **Canonicalization is shared + byte-exact.** One canonical form (lowercase,
  strip trailing dot, IDNA-to-ASCII, reject un-IDNA-able) used for the exec-info
  host, the dialed DSN host, bd's cache key, and the helper's allowlist match —
  which MUST be byte-exact equality, never suffix/substring/port-insensitive. A
  shared golden-vector suite runs in both repos.

**bd's residual guarantee, with no allowlist of its own:** report == dial for
every host on every path. A repo-controlled host is injected faithfully into
exec-info; the *helper* makes the allowlist call. bd cannot cause a
mint-for-A/dial-to-B split because one canonical host feeds the env, the DSN,
and the cache key. The freeform command stays env-set (never repo-set), so the
*command* is not an attack surface; only the *host* was, and it is now surfaced
to the authority that judges it.

### 2.3 What bd does NOT do (deleted from v2)

No bd-side trusted-gateway allowlist. No `attachments.json` TOFU. No
`bd attach`. No `hosted.GuardDestination`/`hosted.Resolve`. No
`credential_helper`/`helper_org`/`helper_project` metadata fields. No
`creds.ArgvSource`. No `BD_TRUST_WORKSPACE`. No second-user detection. bd never
decides whether a host is trusted.

## 3. bd per-dial connector + 1045 + timeouts (survives v2 §3, + host threading)

Long-lived clients (gc-controller-class, >1h) need per-dial re-mint regardless
of who owns trust: the EIA is 90s, `ConnMaxLifetime` is 1h, and every
idle-churned (`ConnMaxIdleTime=20s`) or fresh dial after a 90s rotation would
1045 against the baked static-DSN token. Port `1f0ebe068`:

- **Connector:** `openSQLDB(dsn, src creds.Source)`: `src==nil` → `sql.Open`
  verbatim (static/local path untouched); else `ParseDSN` →
  `cfg.Apply(mysql.BeforeConnect(hook))` → `NewConnector` → `sql.OpenDB`. Driver
  v1.10.0 clones Config per Connect (verified), so per-dial `User` mutation is
  isolated. The hook re-runs the credential resolve (re-runs the command,
  honoring the cache), re-checks KindIdentity + `:@/`, sets `c.User`.
- **Config retention:** `dolt.Config` gains `CredentialSource creds.Source`
  (today `ApplyGatewayCredential` discards the command after the eager mint).
  `internal/creds` gains an exported per-dial `ResolveForDial` and `Invalidate`
  (neither exists today).
- **Host threading (pivot refinement):** the per-dial resolve carries the same
  `dialHost` (§2.2) so every re-mint injects `BEADS_EXEC_INFO` and the cache is
  keyed by (command, host). Structurally identical to v2 §3; the source just
  gains the host field.
- **Convert every raw open site** to the connector: store.go 1382/1409/1462/
  1499/1744/2770 and **transaction.go:147** (the ignored-tx fresh dial —
  mandatory; it dials the stale baked DSN, so post-rotation every write 1045s
  without it). Pool-borrow optimization NOT ported.
- **1045-invalidate:** port `isAuthError`; add invalidate+retry to `withRetry`
  AND `withRetryTx`. **Gated + bounded:** only when `CredentialSource != nil`
  (static-password users keep fail-fast), exactly ONE invalidate+re-mint+retry
  then `backoff.Permanent` (SPEC error table; the live mc controller runs bd
  under a bounded exec timeout, so a 30s retry loop would convert clean 1045s
  into timeout kills). Circuit breaker verified safe — `isConnectionError`
  (circuit.go:383-419) never matches 1045.
- **Token redaction:** bake a sentinel user (`token-per-dial`) into the
  retained `store.connStr` when a `CredentialSource` is present (the connector
  overwrites `User` every dial); kills the token-in-connStr leak on the
  re-parsed push/pull paths. Grep test: no token in bd log/error output.

### 3.1 Env-tunable timeouts

`BEADS_DOLT_SERVER_{CONNECT,READ,WRITE}_TIMEOUT` (positive int seconds, parsed
like `BEADS_DOLT_READY_TIMEOUT`); defaults unchanged. Note the read/write
override must also reach the long-timeout re-parse paths (execWithLongTimeout
hard-sets ReadTimeout=5m at store.go:1381,1408 after DSN build) or be documented
as exempt there.

## 4. Configuring a workspace to point at a hosted project (no `bd attach`)

- **Pure env — the human laptop recipe (works today, zero bd code):**
  `BEADS_DOLT_SERVER_HOST/PORT/DATABASE`, `BEADS_DOLT_SERVER_TLS=true`,
  `BEADS_DOLT_CREDENTIAL_COMMAND='gasworks getToken beads --org … --project …'`.
  Complete second-terminal story when the env family is exported.
- **The FLEET path is NOT gasworks (correction from v3).** `mirrorBeadsDoltEnv`
  (bd_env.go:1497-1515) mirrors `GC_DOLT_CRED_CMD` → `BEADS_DOLT_CREDENTIAL_COMMAND`,
  and the controller image sets `GC_DOLT_CRED_CMD=/usr/local/bin/eia-helper`
  (an STS machine minter reading `ORCHESTRATOR_KEY_FILE`/`EIA_AUDIENCE`/…). So
  the fleet's helper is `eia-helper`, and gasworks has no machine leg
  (`GASWORKS_API_KEY`/`/sts/v0/machine` absent from the CLI). The destination
  gate therefore only exists on the fleet if `eia-helper` implements the helper
  contract (§5.0) — see the owner decision in §11. Until it does, the fleet
  mints with no destination gate, which is exactly the population that processes
  untrusted repos (adopt-pr, PR-review).
- **`bd init --server` (#4823) as the pointer + identity anchor:** writes
  metadata (`DoltMode=server`, host/port/user from flags, `Backend=dolt`,
  `DoltDatabase`) and adopts `_project_id`/`issue_prefix` from the hosted DB,
  never minting. Two gaps for a clean-env second terminal: it never persists
  `dolt_server_tls` (TLS survives only via env) and the credential command is
  env-only by design. Persisting `dolt_server_port` in metadata trips a
  per-resolution deprecation warning (doltserver.go:558-571) — prefer
  env/port-file/config.yaml for the port.
- **Optional thin `bd setup-hosted` (NOT a trust ceremony):** if a one-command
  UX is wanted later, its only justified additions over `init --server` are
  persisting `dolt_server_tls=true` and printing the credential-command recipe.
  No prompt, no allowlist, no attachment record. Deferred unless the panel (S5)
  demands it; the recipe/env family is the honest S1(e)/S5 shape.

**Recipe = targeting data, not a secret.** The `gasworks getToken … --gateway
<host>` recipe (or the env family) carries no credential material; safety comes
from gasworks refusing to mint for an untrusted `--gateway`/exec-info host — not
from recipe secrecy. (Unchanged SPEC framing; the enforcing party moves to
gasworks.)

## 5. The credential helpers — auth + the destination-enforcement contract

### 5.0 The helper destination-enforcement contract (gasworks AND eia-helper)

Because bd is a pure delegator, the destination gate must live in **every**
helper bd can invoke, or the fleet (`eia-helper`) has no gate at all. v4 defines
a small, helper-agnostic contract; each helper implements it:

1. **Read `BEADS_EXEC_INFO`** (versioned JSON, §2.2): `spec.dialHost`,
   `spec.dialPort`, `spec.database`, `origin`.
2. **Resolve trust** for `dialHost` against the helper's own allowlist
   (gasworks: `~/.config/gasworks/trusted-gateways.json` + compiled default;
   eia-helper: a compiled/config allowlist of the fleet's gateway names — note
   the fleet dials the split-DNS short name `beads`→tailnet-IP, NOT the FQDN, so
   its allowlist entries differ from the human default; §11).
3. **Byte-exact match** on the shared canonical form (§2.2) — never
   suffix/substring/port-insensitive.
4. **Fail-closed for bd-originated mints on absent/untrusted destination:** if
   `origin==bd` and `dialHost` is missing or not in the allowlist → refuse
   (exit 1, scrubbed stderr bd surfaces verbatim), mint nothing. A direct human
   invocation with no exec-info keeps minting (fail-open) — the marker is what
   lets the two diverge.
5. **Host in the mint/cache key:** a token minted for host A is never returned
   for a dial to host B (gasworks EIA cache key gains the gateway dimension,
   §5.3; bd's own cache too, §2.2).
6. **Warn-then-enforce:** ship the check warn-only (log the would-refuse, still
   mint) until the exec-info-injecting bd tag is the default install, then flip
   to enforce — so a new-bd/old-helper or old-bd/new-helper skew never silently
   fails open OR breaks a working setup (§8 sequencing).

gasworks implements this in §5.1-5.4 below. `eia-helper` implementing it is the
open owner decision (§11) — the alternative is retaining a bd-side/gc-side gate
for the fleet, which reintroduces the machinery the pivot removed.

### 5.1 gasworks — already owns auth (keep as-is)

`getToken` already runs the full noninteractive mint chain: `ensureIDToken`
(Keycloak `refresh_token` grant, no browser) → `sts.Context` → `pickOrg` →
entitlement gate → EIA cache → `ensureSession` (8h DPoP session) →
`sts.Exchange` (RFC 8693, 90s EIA). All three token layers auto-refresh with no
interaction until the Keycloak offline session expires. **No new
token-ownership code is needed** — the pivot's "gasworks owns all auth" is
already true for the human leg.

### 5.2 Flags + the destination gate

- `--gateway <host>` and `--project <prj_id>` added to `getToken` and to
  `hoistPositional`'s `valueFlags` map (gettoken.go:257) or a flag-before-product
  argv mis-parses. `--project` is validated + recorded now; scope-pinning is S3.
- **Destination source, in order:** `BEADS_EXEC_INFO.spec.dialHost` (bd-injected,
  authoritative — the value bd actually dials) > `--gateway` (manual, for direct
  human use or an older bd). If both present and disagree → error (never
  silently pick). **Neither present (§5.0 rule 4):** if `origin==bd` → refuse
  (a bd invocation with no destination is a bug or an attack, never legitimate);
  if no `origin` marker (direct human `gasworks getToken beads --org X`) → mint,
  unchanged. Recipes SHOULD omit `--gateway` when bd drives exec-info, so the
  disagree/stale-`--gateway` branch is only ever hit by direct/older use — a
  hardcoded `--gateway gw.old` in a re-pointed workspace must not hard-fail the
  golden path.
- **Gate BEFORE the EIA cache read** (gettoken.go:80-86): the cache is served
  before any mint, so a mint-time-only check would still hand a cached token to
  an untrusted destination. Host ∉ allowlist → `die` → stderr `gasworks:
  refusing to mint a beads credential for unknown gateway '<host>' — trusted
  gateways: <list>. Add one with 'gasworks trust-gateway <host>' only if you
  operate it.` (exit 1; bd surfaces it verbatim).
- **Cache key gains the gateway dimension** (`org|product|scope|gateway`) so a
  token minted for host A is never served for host B (security control, §5.0.5).
- **Version-compat:** gasworks reads `dialHost`, IGNORES unknown exec-info
  fields, and tolerates a newer minor `apiVersion` (does not hard-fail) so a
  future bd exec-info bump never breaks the two-repo release coupling.

### 5.3 The allowlist + `trust-gateway`

- Sibling file `~/.config/gasworks/trusted-gateways.json` in `store.ConfigDir()`
  (NOT `credentials.json` — avoids entangling the S4 keyring migration), with a
  compiled-in default entry `gw.beads.gascity.com` merged at read time (golden
  path needs no file). Same canonical host form as bd (§2.2).
- **Concurrency:** reuse the store `flock` pattern (lock_unix.go:14-31) for the
  writer — this org runs many concurrent agents per machine, so a
  `trust-gateway` write racing an attach/other write must not drop an entry.
- New command `trust-gateway <host>` (`--yes`, `--list`, `--remove`; compiled
  defaults not removable), registered in the main.go:42-61 switch.

### 5.4 Mint resilience (greenfield; `httpc` has zero retry machinery)

- **Per-attempt STS timeout (5s), total budget ≤ ~15s** — httpc's default 30s
  per call means one hang consumes bd's whole 30s helper-exec cap and
  serve-last-good never runs. Budget the full ladder + serve-last-good inside
  the exec cap.
- **Jittered early refresh** (`15s + rand(0..15s)` instead of the fixed 15s
  skew) so a fleet does not re-mint on one boundary.
- **Bounded backoff** on STS 5xx/429/network (3 attempts, 250ms·2ⁿ + jitter);
  403 and the existing one-shot 401-relogin are not retried.
- **Serve-last-good** above a 15s floor (so bd's 10s expiry skew does not
  instantly re-stale it into a helper-exec storm): on exchange failure with a
  still-valid cached token, emit it with a stderr warning instead of dying.
- **`--json` emits real `res.ExpiresIn`** (gettoken.go:296 hardcodes 90) — a
  thin bd trusting the envelope for its cache TTL must get the truth.

### 5.5 Reliability fixes forced by sole-owner status (now load-bearing)

Concurrent delegation is the norm under the pivot (every bd/gc command spawns a
FRESH `gasworks getToken` OS process — cross-process, not goroutines), so two
edge cases become first-class:

- **Refresh-token rotation serialization — the DOUBLE-CHECKED pattern (not a
  naive lock, not in-process single-flight).** Today `ensureIDToken`
  (gettoken.go:136-174) does an UNLOCKED `store.Load` + UNLOCKED `oidc.Refresh`,
  locking only the final write. Keycloak rotates the refresh token on use; N
  racing callers present the same token, the 2nd..Nth are reuse-detected, and
  Keycloak can **revoke the whole family → the durable offline session is
  stranded** (forces interactive login fleet-wide). Fix: **(1) fast path** —
  unlocked Load; if the id-token has > skew remaining, return it (no lock, no
  refresh). **(2) slow path** — acquire the store flock (lock_unix.go), **RE-Load
  under the lock, RE-CHECK freshness** (a peer may have refreshed while we
  blocked), and refresh+persist only if still stale, with the `oidc.Refresh`
  bounded by a timeout well under bd's 30s exec cap. The first waiter refreshes
  once; all others re-read the fresh token and skip. (In-process single-flight
  is useless here — each getToken is a separate process — and a naive
  lock-across-refresh without the re-check serializes N Keycloak RTTs under one
  lock and can exceed the 30s cap → the 1045 storm §3 exists to avoid.) This is
  a gasworks-reliability fix, in WP-B because the pivot makes concurrent
  cross-process delegation the steady state.
- **Pin the Keycloak offline-session lifetime** server-side (auth-owner infra
  deliverable, not CLI code): it is the outer bound on the noninteractive chain;
  its expiry forces every delegating caller back to interactive login at once.

## 6. Doctor + diagnostic contract (bd; survives v2 §6, reframed)

- **Exhaustive gateway-blindness sweep** (grep-gated): route every hand-built
  server `dolt.Config`/`doltutil.ServerDSN` through the canonical
  `applyResolvedConfig` path so `BEADS_DOLT_SERVER_TLS` + the credential command
  are honored. Sites: doctor/federation.go:31-44 (`doltServerConfig`, ~10
  consumers), doctor/dolt.go:21-78 (`openDoltDB` — the init post-check noise),
  server.go:290, perf_dolt.go:122, fresh_clone_server.go:28, fix/metadata.go:500,
  fix/remotes.go:18, fix/validation.go:307, doctor_health.go:57, init_guard.go:40,
  cmd/bd/dolt.go:841/1902, bootstrap.go:62/872. Fixes bead `mc-b3clt.4` (the
  false "Failed to connect" on a working hosted workspace). Independent of trust
  — stands on its own as the #4823 field-report follow-up.
- **Diagnostic contract** — per-layer PASS/FAIL, one remedy each: workspace →
  helper present → **helper dry-run** (surfaces gasworks' own errors: destination
  refusal, expired session `gasworks login`, STS 403 entitlement stderr
  verbatim) → DNS/TCP → TLS (map the gateway's pre-auth "TLS required" to the
  TLS remedy) → auth → database. Note the trust decision now shows up as a
  **helper (gasworks) error**, not a bd-layer check — doctor surfaces it, it
  does not re-implement it.

## 7. Security model under the pivot (honest — not "closed at the root")

- **[CRITICAL] Recipe-directed exfiltration** — closed **when, and only when,
  the invoked helper implements the §5.0 contract.** bd's part (always inject
  the true dial host; report==dial; host in the cache key; fail-closed marker)
  is necessary but NOT sufficient — the *judgment* is the helper's. So the
  guarantee holds for: the human path with an S2 gasworks (enforces); and the
  fleet path IFF `eia-helper` gains the contract (§11 decision). It does NOT
  hold for: a fleet with an unpatched `eia-helper`; a wrapper/PATH-shadowed
  helper that ignores exec-info; a new-bd/old-helper version skew before the
  warn→enforce flip. Each of those is a full replayable-token exfil, because the
  EIA has no host binding (below). This is why v4 makes the contract a merge-gate
  with adversarial tests, and why deleting v2's independent bd-side layer is an
  **owner sign-off item, not a settled simplification** (§11). **Invariants to
  test:** report==dial for env/metadata/committed-config.yaml host shapes; the
  loopback-lookalike vectors (`127.0.0.1.evil.com`, `0.0.0.0`,
  `::ffff:127.0.0.1`); warm-trusted-token never served for an untrusted dial.
- **[HIGH → residual, honest] Token replay if leaked.** The allowlist is a
  **mint-time** gate. The minted EIA is `aud=beads` with **no host binding**
  (sts.go:121-126), so a token captured on any unverified path (or leaked cached
  EIA) still replays against the *real* gateway for its TTL. The pivot does not
  change this — bounded only by the 90s TTL + the 15m server ceiling. Every
  gate-miss above yields a *full replayable* token, not a scoped leak, which is
  why gate coverage must be exhaustive+tested rather than best-effort. **EIA
  channel/destination binding** is the real fix and is **deferred future
  hardening** (SPEC:231).
- **[Trade, owner-visible] Single enforcement layer replaces v2's two.** SPEC
  CRITICAL #2 mandates two INDEPENDENT destination layers ("so neither a
  compromised bd config nor a spoofed helper invocation can exfiltrate"). v4
  collapses to one (the helper contract). The eia-helper finding proves the two
  layers are NOT redundant — each covers a gap the other cannot. Within the
  threat model (untrusted repo, trusted user+tools) the surviving single layer
  is sufficient *when every helper enforces*; it is strictly weaker when a helper
  does not. Owner must accept the collapse or keep a second layer (§11).
- **Preserved invariants:** TLS terminates on the gateway; clients never learn
  backend coordinates; the credential command stays env-only (no repo-persisted
  helper — configfile.go:490-492 intent kept); `:@/` charset + KindIdentity
  checks stay; recipes carry no secrets. Threat model unchanged from v2: a
  hostile *user* who controls the machine defeats any client-side gate in either
  design; both defend only the hostile-*repo* case.

## 8. Work packages + sequencing

| WP | Repo | Contents | Depends on |
|----|------|----------|-----------|
| **A** | bd (OSS) | §2.2 exec-info host threading (`BEADS_EXEC_INFO` always-inject, `origin:bd` marker, no loopback exemption, structural report==dial from `mysql.Config.Addr`, host in bd cache key, narrow strippedEnv, shared canon); §3 connector + gated one-shot 1045 + transaction.go:147 + timeouts + sentinel redaction | none (independent of #4823) |
| **B** | gasworks | §5.0 contract impl + §5.1-5.4: `--gateway`/`--project` + hoistPositional; read exec-info-or-flag + neither-present policy + version-tolerance; destination gate before cache; gateway cache-key dim; `trust-gateway` (locked store); resilience; `expires_in`; §5.5 double-checked refresh serialization. **Ships warn-only, flips to enforce after WP-A is the default bd tag.** | WP-A tag exists before enforce-flip |
| **E** | crucible/gc (`eia-helper`) | §5.0 contract impl in `eia-helper`: read `BEADS_EXEC_INFO`, fleet-gateway allowlist (split-DNS short-name entries, §11), byte-exact match, fail-closed for `origin:bd`, warn-then-enforce | **owner decision (§11)**; WP-A |
| **D** | bd (OSS) | §6 doctor exhaustive sweep + diagnostic contract (fixes `mc-b3clt.4`; doctor injects a representative `BEADS_EXEC_INFO` so the helper dry-run reflects the real dial) | #4823 (field-report follow-up) |

**Deleted vs v2:** WP-C1/C2 (trust stores, `GuardDestination`, `hosted.Resolve`,
`bd attach`) are gone. The program is A ∥ B → (E, pending owner) → D, with the
**warn→enforce flip gated on WP-A being the default bd install**. #4823 is now only a
dependency of the doctor follow-up, not of the security core.

Ship order **A ∥ B(warn-only) → flip B to enforce once WP-A is the default bd
install → E (if owner approves) → D**. bd PRs: contributors.md + template,
`status/needs-review-auto`, no commercial language ("an authenticating SQL
gateway", never product names). gasworks follows its repo conventions (public).
Each PR gets a Fable red-team before commit.

## 9. Exit tests

1. **Exfil closed (all three host-source shapes):** env-host, metadata-host,
   and committed-`.beads/config.yaml`-host all set to an unlisted gateway with an
   ambient credential command → a canary helper records invocation and MUST see
   `BEADS_EXEC_INFO.dialHost == the unlisted host` (report==dial), and a real
   enforcing helper refuses before minting. No mint-for-A/dial-to-B split.
1b. **Loopback-lookalike vectors:** `dolt.host` set to `127.0.0.1.evil.com`,
   `0.0.0.0`, `::ffff:127.0.0.1`, `localhost.evil.com` → bd STILL injects
   exec-info with the real host (no exemption suppresses it) and the enforcing
   helper refuses.
1c. **Fail-closed on absent destination:** `origin:bd` exec-info with no
   `dialHost` → enforcing helper refuses; a direct human `gasworks getToken
   beads --org X` (no exec-info) still mints.
2. **Report == dial:** for each shape, assert the host in `BEADS_EXEC_INFO`
   equals the host in the dialed DSN — including the per-dial connector path
   (derived from `mysql.Config.Addr`).
2b. **Warm-cache cross-serve:** in one long-lived process, dial trusted host A
   (populates the cache), then dial untrusted host B with the same command →
   bd's cache MISSES on the host dimension and the helper refuses B (no warm
   trusted token served for B).
3. **Long-lived client:** gc-controller-class process >1h, zero 1045s across
   ≥40 rotations (per-dial re-mint) — against `prj_848513b16e7b5c43` on cherry.
4. **Write after rotation:** a writer idling >90s between writes succeeds
   (transaction.go:147 conversion).
5. **Cache-destination isolation:** getToken for gateway A then `--gateway`/
   exec-info B (both trusted) does not serve A's cached token; and bd's own
   per-dial cache keyed by (command, host) does the same.
6. **gasworks refusal e2e:** `gasworks getToken --gateway evil.example` (and
   via `BEADS_EXEC_INFO`) refused pre-cache, pre-exchange; bd surfaces the
   stderr verbatim.
7. **Concurrency:** N concurrent `gasworks getToken` processes sharing one
   credentials file do not strand the durable session (refresh serialization).
8. **Doctor truth:** healthy hosted workspace → `bd doctor` and the init
   post-check report zero connection failures (regression for `mc-b3clt.4`).
9. **Redaction:** grep test — no token in bd log/error output; `BEADS_EXEC_INFO`
   carries a host, never a token.

## 10. SPEC deltas (companion edits so SPEC and design agree)

The pivot changes load-bearing SPEC text. Apply these (done in this session
where marked):
- **S2 rollout item (SPEC line ~209):** retitle to thin-bd delegation +
  gasworks-owned trust; drop "`bd attach` + the user-level trusted-gateway
  allowlist + TOFU"; keep per-dial connector; add "bd exec-info host threading;
  gasworks owns the allowlist + `--gateway` refusal." **[patched this session]**
- **CRITICAL #2 mitigation (SPEC ~130-131):** reworded to the helper-contract
  model, but explicitly marked **PROPOSED, pending owner sign-off** on the
  two-layers→one-layer collapse (the red-team flagged that I must not
  unilaterally downgrade a CRITICAL mitigation). Keeps the 90s-TTL replay bound
  and the "destination binding is future hardening" note, and now records the
  fleet-helper-must-enforce dependency. **[patched this session, as PROPOSED]**
- **Connection model / Credential flow (SPEC ~89-93) & bd client changes
  (~113-124):** remove the bd-side allowlist, `bd attach`, ArgvSource, TOFU;
  replace with exec-info delegation + gasworks trust. **[flagged; larger edit,
  left for a focused SPEC pass with owner review]**
- **Goal (SPEC line 17)** "repo-local configuration can never select the mint
  destination" stays TRUE (the repo host is reported to and refused by gasworks)
  — no edit, but the mechanism note changes.
- S1, S3–S9 unaffected (S3 pinned scopes and S4 custody were already
  gasworks-side; the pivot only reinforces them).

## 11. Owner flags — two are genuine DECISIONS (please rule before implementation)

**DECISION 1 — collapse v2's two destination layers into the helper contract?**
SPEC CRITICAL #2 mandates two independent layers (bd allowlist AND gasworks).
The pivot deletes the bd layer. The red-team proved the layers are NOT redundant
(the fleet `eia-helper` case). Options: **(a)** accept single-layer = the §5.0
helper contract, on the condition that EVERY helper (gasworks + eia-helper)
enforces it (Decision 2 must then be "yes"); **(b)** keep a thin bd-side or
gc-side destination allowlist as the independent second layer (reintroduces some
of the machinery the pivot removed, but is version-skew-proof and covers a
non-enforcing helper). My recommendation: **(a)**, because it matches the owner's
"gasworks/helper owns auth" intent AND is uniform — but it is your call, and it
requires Decision 2.

**DECISION 2 — is `eia-helper` destination-enforcement (WP-E) in scope now?**
The fleet mints via `eia-helper`, not gasworks; without WP-E the fleet (which
processes untrusted repos in adopt-pr/PR-review) has NO destination gate under
the pivot. Options: **(a)** in scope — add the §5.0 contract to `eia-helper`
(crucible/gc change, needs a fleet-gateway allowlist keyed on the split-DNS
short name `beads`, not the FQDN); **(b)** deferred, and the fleet gap is
accepted meanwhile with a compensating control (e.g. the fleet keeps env-pinning
`GC_DOLT_HOST` so a repo-supplied host never reaches a mint — verify
`mirrorBeadsDoltEnv` actually enforces this, today it only *projects* the host
when `GC_DOLT_HOST` is set and DELETES it otherwise). My recommendation: **(a)**
if Decision 1 is (a); at minimum a compensating control before any fleet exposure.

Other flags (not decisions):
- **gasworks machine/CI leg is unbuilt** (`GASWORKS_API_KEY` → `/sts/v0/machine`
  absent from the CLI). "gasworks owns ALL auth" is only true for the human leg;
  the fleet uses `eia-helper`. Which slice builds the CLI machine leg, if ever?
- **Refresh-token serialization (§5.5, double-checked) + offline-session
  pinning** — now load-bearing under concurrent cross-process delegation; the
  pinning is a server-side Keycloak change.
- **gasworks repo is PUBLIC** — the allowlist + hosted defaults ship in OSS
  (already the repo's practice). Confirm vs "commercial code private."
- **bd release vehicle** (SPEC open question) undecided; the warn→enforce flip is
  gated on WP-A being the default install.
- **EIA channel binding** (the real replay fix) remains deferred future
  hardening — confirm it stays out of S2.
