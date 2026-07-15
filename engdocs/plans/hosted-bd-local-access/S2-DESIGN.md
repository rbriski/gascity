# S2 Design — bd attach, per-dial minting, destination trust

> Fable design, 2026-07-15, **v2 (post red-team)**. Grounded by a 4-reader
> workflow over: bd origin/main (`3fea70523`) + PR #4823 branch (`7075e2738`),
> the per-dial connector branch (`feat/dolt-credential-command` @ `1f0ebe068`),
> and the gasworks CLI (origin/main `4394951`). Full grounding dumps:
> `/data/tmp/s2-grounding/*.md` (session-local; load-bearing anchors inlined).
> Implements the SPEC's Rollout item **S2**. Status: DESIGN — implementation
> follows the Fable-design → Opus-impl → Fable-red-team cycle per work package.

> **v2 changelog (a 3-lens Fable red-team broke v1; all three lenses converged
> on the same CRITICAL, which is why it is fixed at the root here):**
> - **The v1 "env credential command bypasses the destination gate" carve-out
>   reopened the SPEC's CRITICAL exfil vector for the whole fleet/S1 cohort.**
>   Fixed by splitting two concepts v1 conflated (§2.2): who may set the
>   credential *command string* (env: always; repo: only an allowlisted helper
>   via TOFU) vs. the *destination gate* (UNCONDITIONAL — every
>   credential-command dial to a non-loopback host requires host ∈ allowlist,
>   whatever the source of command or host).
> - **Hosted resolution is now TERMINAL/exclusive** (§4.4), not "before existing
>   logic" — it owns host/port/tls/cred and the sites never fall through to
>   `GetDoltServerHost`/`.beads/config.yaml` (which is repo-committed, highest
>   config priority, and NOT in the TOFU hash — v1's second exfil hole).
> - `Config.CredentialSource` retyped to the `creds.Source` interface; the
>   persisted path does its OWN eager fail-closed mint (v1's "ApplyGatewayCredential
>   still gates open" was false for a clean-env attached workspace) (§3.1, §4.2).
> - attach verification connect skips the store-level `verifyProjectIdentity`
>   and does its own cross-check (v1 would self-conflict at open) (§4.2).
> - STS resilience gets a per-attempt HTTP timeout + total budget (v1's backoff
>   math ignored httpc's 30s per-call timeout → serve-last-good unreachable on a
>   hang) (§5.4).
> - gasworks version-skew (old CLI dies on `--gateway`) gets capability
>   detection + a named remedy (§4.2, §5.1).
> - Host canonicalization threaded to check+dial+hash; 1045-retry gated + ONE
>   attempt (not a 30s loop — matters for the bounded-exec mc controller);
>   doctor renders hosted.Resolve failures as a layer, not a crash. (§ throughout.)

## 0. Grounding corrections to the SPEC (must-know before implementing)

1. **The gasworks CLI has NO machine leg.** `GASWORKS_API_KEY` appears nowhere
   in `gascity/gasworks` on any branch; only the server endpoint
   (`/sts/v0/machine`, gasworks-platform) exists. The SPEC's CI golden path
   ("identical command exchanges via /sts/v0/machine") is unbuilt client code.
   **Decision: machine leg is OUT of S2** (flagged to owner). S2's
   `BD_TRUST_WORKSPACE=1` gate therefore cannot require `GASWORKS_API_KEY`
   presence yet; see §4.5.
2. **S1 workspaces already carry the host in repo metadata.** #4823's init
   persists `dolt_server_host/port/user` from flags (init.go:1451-1464) and
   never persists `dolt_server_tls`. S2's resolver must (a) never dial a
   metadata host for hosted workspaces, (b) tolerate S1-created metadata that
   has one.
3. **gasworks serves cached EIAs BEFORE any mint step** (gettoken.go:81-86) and
   the cache key (`org|product|scope`) has no gateway dimension — the
   destination gate must run before the cache read (§5.2).
4. **bd main's write path fresh-dials with the stale token-baked DSN**
   (transaction.go:147) — the connector port MUST convert it, or every write
   1045s after the first token rotation (§3.2).
5. **Three parallel config-resolution sites exist in bd** (cmd/bd/main.go
   hand-mirror :1122-1196, open.go `applyResolvedConfig` :202-280, init.go)
   plus ~8 gateway-blind doctor/fix DSN builders. Hosted resolution must be ONE
   shared function or the guard is bypassable via whichever site forgot it
   (§4.4, §6).

## 1. Scope

**In:** bd (OSS `gastownhall/beads`): user-level trusted-gateway allowlist +
per-workspace attachment (TOFU) records + `bd attach` + hosted-workspace
ambient resolution + per-dial credential connector + 1045-invalidate-retry +
doctor threading/diagnostic contract + env-tunable wire timeouts.
gasworks CLI (public `gascity/gasworks`): `--gateway`/`--project` on getToken +
destination allowlist + `trust-gateway` + jittered refresh / bounded backoff /
serve-last-good + `expires_in` envelope fix.

**Out (unchanged from SPEC):** pinned scopes (S3), keyring custody (S4), panel
(S5), pg mode (S6), public exposure (S7), splice tuning (S8). Plus, per §0.1:
the CLI machine leg (new owner decision needed on which slice carries it).

## 2. The destination-trust model (bd side)

### 2.1 Files (both under the existing `~/.config/beads/` precedent)

- **`~/.config/beads/trusted-gateways.json`** — the user-level allowlist.
  `{ "version": 1, "gateways": [ { "host": "gw.example.com", "added_at": …,
  "source": "attach|manual" } ] }`. A **compiled-in default list**
  (`gw.beads.gascity.com`) ships in the release binary and is merged at read
  time — the golden path needs no file at all. Env override for tests:
  `BEADS_TRUSTED_GATEWAYS_FILE`. Atomic temp+rename 0600 (same pattern as
  configfile.Save).
- **`~/.config/beads/attachments.json`** — per-workspace attachment records
  (the TOFU store + the host's actual home). Keyed by canonical absolute
  workspace path:
  `{ "<path>": { "hash": "<sha256 of helper|argv|host:port|database>",
  "host": …, "port": 3306, "engine": "dolt", "database": "bd_prj_…",
  "helper": "gasworks", "org": …, "project": "prj_…",
  "trusted_at": …, "accepted": "interactive|accept-flag|env" } }`.
  Env override: `BEADS_ATTACHMENTS_FILE`.

### 2.2 Invariants

The v1 design conflated two independent decisions. v2 separates them:

**Decision 1 — who may set the credential COMMAND (the string that gets
exec'd).** Env (`BEADS_DOLT_CREDENTIAL_COMMAND`) may always set it: it comes
from the user's shell/agent, not a cloned repo — it is the fleet/controller
path and the power-user escape hatch, and it stays freeform (`sh -c`). Repo
metadata may set it ONLY as an allowlisted helper (compiled-in binary names,
initially `["gasworks"]`), exact-argv, no shell, gated by the TOFU attachment
record. This preserves the configfile.go:490-492 security intent.

**Decision 2 — the DESTINATION GATE (the host a minted token is transmitted
to). This is UNCONDITIONAL and is the real exfil control.** Before ANY
credential command runs to mint a token for a non-loopback dial — env command
OR persisted helper, host from env OR metadata OR config.yaml — bd requires the
resolved dial host ∈ the user-level trusted-gateway allowlist, and hard-fails
otherwise. There is no carve-out: the env exemption in Decision 1 is about the
COMMAND string's provenance, never about skipping the destination check. (v1's
fatal bug was gating the destination check on `credential_helper` presence and
on env-var absence, so a hostile repo with committed `.beads/config.yaml
dolt.host: evil.example` + `dolt_mode=server` + no `credential_helper` made any
victim with an ambient credential command mint and exfil — verbatim SPEC
CRITICAL #2.)

Enforcement point: `hosted.GuardDestination(host, credentialPresent)` is called
inside `ApplyGatewayCredential` (and the persisted-helper resolve) at the single
choke every mint passes through — `internal/storage/dolt/gateway_credential.go`,
right before `creds.ResolveLadder` fires the command. Loopback hosts
(127.0.0.0/8, ::1, localhost) are exempt (local dolt is not a gateway). A
non-loopback host with a credential command and no allowlist entry → the named
error, no token minted.

Other invariants:

- **Repo-local metadata never selects a mint destination for the HOSTED
  (persisted-helper) path.** The gateway host lives ONLY in the attachment
  record. For a workspace with `credential_helper` metadata the resolver is
  terminal (§4.4) and never reads `dolt_server_host`/config.yaml. S1-compat: a
  workspace WITHOUT `credential_helper` (env-driven) that carries a metadata
  host still goes through Decision 2's unconditional gate, so a stale/hostile
  host is caught at the mint boundary regardless.
- **Ambient commands hard-fail (no prompt)** when a workspace has
  `credential_helper` metadata but no attachment record, a hash mismatch, or a
  record host missing from the allowlist. Error names the host and prints one
  remedy: `gasworks login && bd attach --accept` (second-user; the compiled
  default host makes this sufficient for the default gateway — for a
  non-default gateway the message includes the full `bd attach <uri>` form so
  the host is explicit), or the `bd attach <uri>` form on hash mismatch.
- **Host canonicalization is single-form:** one function lowercases, strips a
  trailing dot, IDNA/punycode-normalizes, and normalizes the port; the SAME
  canonical string is used for the allowlist check, the dial, and the TOFU
  hash. Check-vs-use divergence is a merge-gate test.
- **Trust is only ever established by `bd attach`** (§4.5).

### 2.3 Metadata additions (internal/configfile Metadata struct)

New optional fields, written only by attach:
`credential_helper` (string; validated against the compiled-in helper
allowlist, initially `["gasworks"]`), `helper_org` (string, strict charset
`[a-zA-Z0-9._-]{1,64}` — matches what getToken --org accepts),
`helper_project` (string, `^prj_[a-f0-9]{16}$`). Existing fields reused:
`backend=dolt`, `dolt_mode=server`, `dolt_database`, `project_id`.
NOT written: `dolt_server_host/port/user/tls` (hosted mode implies TLS=true
structurally; host/port live in the attachment record).
`GetDoltCredentialCommand` stays env-only — the persisted-helper path is a NEW
resolution branch (§4.4), not a widening of the freeform getter. This honors
the security intent documented at configfile.go:490-492.

## 3. Per-dial credential connector (bd, port of `1f0ebe068`)

### 3.1 Cache/creds seams (all net-new on main; internal/creds)

- Introduce a `creds.Source` INTERFACE (`Resolve(ctx) (Credential, error)`,
  `CacheKey() string`, `Kind() creds.Kind`) with two implementations:
  `creds.CommandSource` (existing freeform `sh -c` env command) and NEW
  `creds.ArgvSource{Argv []string, Kind, Label}` executing WITHOUT a shell
  (`exec.CommandContext(argv[0], argv[1:]…)`; SPEC: "allowlisted binary, no
  shell, exact argv"). CacheKey = label + argv joined with `\x00`.
- Export a per-dial resolve entry point and an invalidator on the EXISTING
  cache (internal/creds/command.go:71-137; the branch's dolt-local `credcmd.go`
  is NOT ported): `creds.ResolveForDial(ctx, src creds.Source) (Credential,
  error)` and `creds.Invalidate(src creds.Source)`, keyed by `src.CacheKey()`.
  Keep the 30s/60s/10s constants.
- `dolt.Config` gains `CredentialSource creds.Source` (INTERFACE type, so it
  carries either a CommandSource or an ArgvSource — v1's `*creds.CommandSource`
  could not hold the persisted-helper argv). Retention is required because
  today `ApplyGatewayCredential` discards the command after the eager mint
  (gateway_credential.go:58-60), leaving the connector nothing to re-run.
- **Two mint entry points, one gate.** (1) Env path:
  `ApplyGatewayCredential` continues to build a `CommandSource` from
  `GetDoltCredentialCommand()`, calls `hosted.GuardDestination` (§2.2), resolves
  fail-closed, stamps `ServerUser`/`Gateway`/`DisableAutoStart` AND
  `CredentialSource`. (2) Persisted path: `hosted.Resolve` (§4.4) builds the
  `ArgvSource`, calls the SAME `hosted.GuardDestination`, does its OWN eager
  fail-closed resolve (mirroring ApplyGatewayCredential's KindIdentity + `:@/`
  checks), and stamps the same fields. This fixes v1's false claim that "the
  eager ApplyGatewayCredential mint still gates open" on the persisted path —
  ApplyGatewayCredential reads env-only `GetDoltCredentialCommand`, which is
  empty for a clean-env attached workspace, so it no-ops; the persisted path
  must run its own eager mint or the first helper failure (expired session)
  surfaces as an unnamed dial error instead of the SPEC's mint-layer
  `gasworks login` remedy.

### 3.2 Connector wiring (internal/storage/dolt)

- `openSQLDB(dsn string, src creds.Source) (*sql.DB, error)`: src==nil →
  `sql.Open("mysql", dsn)` byte-identical (local/static path untouched).
  Else `mysql.ParseDSN` → `cfg.Apply(mysql.BeforeConnect(hook))` →
  `mysql.NewConnector` → `sql.OpenDB`. Driver v1.10.0 clones Config per
  Connect() (verified: connector.go:67-78 in the pinned driver), so per-dial
  User mutation is isolated.
- Hook: `creds.ResolveForDial` then re-run the KindIdentity + `:@/` charset
  checks per dial (closes the branch's unenforced-invariant gap — checks are
  cheap and the failure mode is a corrupted DSN slot) and set `c.User`.
  Resolve failure fails the dial closed (no stale fallback — serve-last-good
  lives in the HELPER, §5.4, where the true expiry is known).
- **Token redaction:** when a CredentialSource is present, `buildServerDSN`
  bakes the sentinel user `token-per-dial` into the stored DSN instead of the
  real token — the connector overwrites User on every dial anyway. This kills
  the token-in-`store.connStr` leak class (connStr is retained at store.go:1197
  and re-parsed by push/pull paths) and implements the SPEC's redaction
  CONSIDER for bd. The eager mint at open (env path: `ApplyGatewayCredential`;
  persisted path: `hosted.Resolve`, §3.1) still runs as the fail-fast
  "helper-works / session-live" gate; the sentinel is only what gets baked into
  the retained DSN string, not a skip of the mint.
- **Convert every raw open site** to `openSQLDB(…, s.credSource)`: store.go
  1382 (execWithLongTimeout, 5m), 1409, 1462, 1499, 1744, 2770, and
  **transaction.go:147** (the ignored-tx fresh dial — mandatory, §0.4). The
  pool-borrow optimization from the branch (transaction.go:163-230) is NOT
  ported (separable perf; keep the port minimal).
- **1045-invalidate:** port `isAuthError` (mysql 1045 / "access denied") and
  add invalidate+retry to `withRetry` (store.go:497-534) AND — closing the
  branch's known gap — to `withRetryTx`. **Gated and bounded:** only when
  `CredentialSource != nil` (static-password/self-hosted users keep fail-fast
  1045, not a 30s loop), and exactly ONE invalidate+re-mint+retry then
  `backoff.Permanent` (SPEC error table: "after ONE silent cache-invalidate +
  re-mint retry"). This also matters for the live mc controller, whose bd execs
  are timeout-bounded (a 30s in-bd retry loop would convert clean 1045s into
  timeout kills and change failure telemetry) and for S3/S4 fleet-wide revoke
  events (a full backoff window would re-mint per retry and trip S4's
  anomalous-mint alerting). Circuit breaker is safe as-is — verified:
  `isConnectionError` (circuit.go:383-419) matches only TCP/protocol-disconnect
  strings, never 1045/"access denied", so a shared /tmp breaker file is never
  tripped by an expired token.

### 3.3 Env-tunable timeouts

`BEADS_DOLT_SERVER_CONNECT_TIMEOUT`, `BEADS_DOLT_SERVER_READ_TIMEOUT`,
`BEADS_DOLT_SERVER_WRITE_TIMEOUT` — positive integer seconds, parsed like
`BEADS_DOLT_READY_TIMEOUT` (doltserver.go:188-194); defaults unchanged
(5s connect at dsn.go:26-29; 10s read/write at store.go:1363-1364). Read at
DSN build time; invalid values are a hard config error (fail closed, not
silent default). Note the read/write override must also be threaded into the
long-timeout re-parse paths (execWithLongTimeout/NoTx hard-set ReadTimeout=5m
at store.go:1381,1408 AFTER DSN build); otherwise the env var is silently dead
on the WAN push/pull paths that most need it (v1 said "defaults unchanged"
without noting this) — either the override wins there too or the doc states
the long-timeout paths are exempt.

## 4. `bd attach` (bd)

### 4.1 URI contract

`beads+dolt://<host>[:<port>]/<database>?org=<org>&project=<prj_id>`
(pg variant `beads+pg://…?schema=…` is parsed-and-rejected with the SPEC's
"rolling out" message until S6 — the parser is engine-aware from day one so
S6 only flips a switch). Validation: host RFC-1123; port default 3306;
database `^bd_[a-z0-9_]+$` + `ValidateDatabaseName`; org/project charsets per
§2.3. Unknown query params are an error (no silent forward-compat holes).

### 4.2 Command flow

Register `attach` in `noDbCommands` (main.go:856-885). Flow:
1. Parse+validate URI.
2. **Trust ceremony:** host ∈ allowlist (compiled default ∪ user file) → no
   prompt (golden path). Unknown host → interactive: display host, org,
   project, database prominently; explicit `yes` adds the host to the user
   allowlist (`source:"attach"`). Non-interactive rules in §4.5.
3. **Preflight:** helper binary in compiled helper-allowlist and on PATH;
   **capability check** — probe that the helper accepts `--gateway` (run
   `gasworks getToken --help` or a version gate; a stdlib `flag.ContinueOnError`
   CLI dies "flag provided but not defined: -gateway" on an old binary, which is
   a version-skew failure, not a mint failure). Then run the helper once through
   the creds ladder (ArgvSource); map failures: missing binary → install
   remedy; `--gateway` unsupported → "upgrade the gasworks CLI (>= <B version>)";
   expired/absent session (helper stderr pattern) → `gasworks login`; STS 403 →
   entitlement message with helper stderr verbatim (SPEC error table). Guard
   against a hostile helper echoing secrets: surface helper stderr only for the
   403 entitlement case, and scrub anything matching a token shape.
4. **Verification connect** with Gateway semantics (`Gateway=true`,
   `DisableAutoStart`, TLS=true, `CreateIfMissing=false`) but opened with
   **`BeadsDir` unset** so the store-level `verifyProjectIdentity`
   (store.go:1271-1273 skips when beadsDir=="") does NOT run — otherwise it
   would fire against a workspace that has no local `project_id` yet (or a
   different one on re-attach) and refuse the open before attach can adopt.
   attach performs the identity work itself: read `_project_id` +
   `issue_prefix` post-connect and apply the #4823 adopt-never-mint semantics
   (fail-closed provisioning-contract error when absent; transient read errors
   distinguished from absent; on a project mismatch vs the URI's `project`
   param apply §4.3's confirm / `--force-reattach`). Factor the #4823 init
   helpers (init.go:97-167) into a shared home (e.g. `internal/hostedadopt`) so
   init and attach share one implementation — the helpers are unexported and
   may be renamed under review, so reuse SEMANTICS and land the factor-out in
   the attach PR after #4823 merges (see §7 WP split).
5. **Write metadata.json** (§2.3 fields; atomic Save; preserves unrelated
   existing fields) + **write the attachment record** (§2.1) + create
   `.beads/` scaffolding (MkdirAll+guards reused from init; bypass init's
   dbName derivation, remote-bootstrap, port files, CreateIfMissing).
6. Print `Attached to <org>/<project> (read-write). Try: bd list`.

### 4.3 Idempotency

Re-run with an identical tuple → refresh the attachment record timestamp,
print "already attached", exit 0. Tuple differs from the existing record →
show old vs new (host/org/project/database) and require interactive confirm
(or `--force-reattach`); never silently rewrite. A workspace with existing
NON-hosted beads data → refuse with the existing-workspace guard (same
protection as init).

### 4.4 Ambient hosted resolution — TERMINAL, called from all resolution sites

One new function, called from BOTH config-resolution sites (open.go
`applyResolvedConfig` :202-280 and cmd/bd/main.go's hand-built path :1122-1196):

```
hosted.Resolve(ctx, beadsDir, fileCfg) → (resolved, applied bool, err)
```

**When metadata has `credential_helper`, hosted.Resolve is TERMINAL/exclusive
for the connection identity.** It (a) requires+validates the attachment record
(hash over `canon(helper)|argv|canon(host):port|database`), (b) requires the
record host ∈ allowlist, (c) builds the `ArgvSource`
`[helper, getToken, beads, --org, org, --project, project, --gateway,
canon(host)]`, calls `hosted.GuardDestination`, does the eager fail-closed
mint, and returns a fully-resolved set: `ServerHost`/`ServerPort` from the
RECORD, `ServerTLS=true`, `CredentialSource`, Gateway semantics. **When
`applied==true` the calling site MUST NOT consult `GetDoltServerHost` /
`GetDoltServerPort` / `doltserver.DefaultConfig` / `.beads/config.yaml` at
all** — those values are ignored. This closes v1's second exfil hole: project
`.beads/config.yaml` is repo-committed and highest-priority in the host chain
(configfile.go:426-437) and was NOT in the TOFU hash, so a "resolve before
existing logic" ordering let a committed `dolt.host: evil.example` overwrite
the record host AFTER the trust check passed. Terminal resolution + the
committed-`config.yaml`-host-absent assertion in attach validation both close
it; a merge-gate test asserts a committed `.beads/config.yaml dolt.host` never
changes an attached workspace's dialed host.

**Env interaction (corrected from v1 — no destination carve-out):**
- If metadata has `credential_helper` (attached workspace) and the env sets a
  *coherent* override, env wins per Decision 1 — but the destination gate
  (§2.2 Decision 2) still fires on the resulting host. "Coherent" =
  `BEADS_DOLT_SERVER_HOST` present (a deliberate full redirect). A
  credential-command-only or host-only partial env on an ATTACHED workspace is
  a hard config error naming the offending variable ("unset
  BEADS_DOLT_CREDENTIAL_COMMAND or also set BEADS_DOLT_SERVER_HOST — this
  workspace is attached"), because v1's "skip on either var" rule sent minted
  tokens to `127.0.0.1` (metadata carries no host) — a live fleet state
  (`mirrorBeadsDoltEnv` deletes `BEADS_DOLT_SERVER_HOST` when `GC_DOLT_HOST` is
  empty while keeping the ambient cred command, bd_env.go:1456-1460,1508-1515).
- If metadata has NO `credential_helper` (env-driven / S1 / fleet): hosted.Resolve
  returns `applied=false` and the existing path runs — BUT the destination gate
  in `ApplyGatewayCredential` (§2.2) still fires unconditionally on whatever
  host that path resolves (env, metadata, or config.yaml). So even the
  no-`credential_helper` hostile-repo shape is caught at the mint boundary.

The "no beads database found" hint (main.go:1046-1051, errors.go:36-55) gains
the attach remediation line. **init.go is a third resolution site:** `bd init`
in a `credential_helper`-bearing workspace must refuse ("already attached; use
`bd attach` to re-attach") rather than hand-build a CreateIfMissing=true config
that dials the wrong host and rewrites metadata (breaking the attachment hash).

### 4.5 Non-interactive trust

- `--accept`: succeeds only when the URI host is ALREADY in the allowlist
  (compiled default or user-added). It never adds a new host. This keeps the
  second-user flow one-command (`gasworks login && bd attach --accept` — the
  default gateway is compiled in) while a phishing URI to an unknown host
  still hard-fails.
- `BD_TRUST_WORKSPACE=1`: equivalent to `--accept` for ambient re-trust
  (hash-mismatch re-acceptance in CI). Per §0.1 the SPEC's "only honored with
  GASWORKS_API_KEY" cannot ship until the machine leg exists; until then it is
  honored unconditionally but — like `--accept` — can never trust a host
  outside the allowlist. **Owner flag:** revisit when the machine leg lands.
- Neither flag ever bypasses the allowlist. Adding a NEW host is always
  interactive (or a deliberate manual edit of the user file).

### 4.6 Second-user detection

In the workspace-discovery failure path AND on ambient commands that find
hosted metadata without a valid attachment record: print exactly the SPEC's
remediation (`gasworks login && bd attach --accept`). Detection keys on
metadata `credential_helper` presence — NOT on env — so a fresh clone of a
committed metadata.json is detected even with a clean environment. (S1-created
workspaces lack `credential_helper`; they keep working via env exactly as
today — no forced migration in S2, attach upgrades them opportunistically.)

## 5. gasworks CLI changes (public repo `gascity/gasworks`)

### 5.1 Flags

`getToken` gains `--gateway <host>` and `--project <prj_id>` (both added to
`hoistPositional`'s valueFlags map — gettoken.go:257 — or flag-before-product
silently mis-parses). `--project` in S2: validated (`^prj_[a-f0-9]{16}$`) and
recorded in the audit/cache key, NOT yet minted into scopes (that is S3's
pin-aware STS intersection; the flag ships now so recipes/attach are stable).

### 5.2 Destination gate (before the cache read)

Immediately after flag/product parsing — BEFORE the EIA cache check at
gettoken.go:80-86 — when `--gateway` is present: host must be in
(compiled-in default `gw.beads.gascity.com`) ∪ (user file
`~/.config/gasworks/trusted-gateways.json`). Refusal via `die` → stderr
`gasworks: refusing to mint a beads credential for unknown gateway '<host>' —
trusted gateways: <list>. Add a gateway explicitly with 'gasworks
trust-gateway <host>' only if you operate it.` (exit 1; bd surfaces it
verbatim). The EIA cache key gains the gateway dimension
(`org|product|scope|gateway`) so a cached token minted for one destination is
never served for another. `--gateway` absent → behavior unchanged (back-compat
for existing users; bd's persisted-helper path ALWAYS passes it).

### 5.3 Allowlist storage + trust-gateway

Sibling file in `store.ConfigDir()` (NOT inside credentials.json — avoids
entangling the S4 custody migration), own atomic-write helper (mirror
store.go's temp+rename+0600) **with the existing store lock pattern reused** —
this org runs many concurrent agents per machine, so a concurrent
`trust-gateway` write and an attach-triggered write must not drop an entry
(last-writer-wins loses data here). New command `trust-gateway` (+ alias
`trustGateway`) registered in the main.go:42-61 switch: `gasworks trust-gateway
<host>` prompts with the host and consequence, `--yes` for scripting; `--list`
prints compiled + user entries; `--remove <host>` edits the user file only
(compiled defaults are not removable). All host strings pass the same canonical
form as bd (§2.2) so the two allowlists agree byte-for-byte.

### 5.4 Mint resilience (all greenfield — httpc has zero retry machinery)

- **Jittered early refresh:** replace the fixed 15s EIA skew with
  `15s + rand(0..15s)` computed per invocation — fleet re-mints spread across
  the last 30s of the 90s TTL instead of synchronizing on one boundary.
- **Bounded backoff on STS 5xx/429/network:** 3 attempts, 250ms·2ⁿ + jitter
  (total budget ≤ ~2s — must fit inside bd's 30s helper exec timeout with wide
  margin). 4xx (401 handled by the existing one-shot re-login; 403) never
  retried.
- **Per-attempt HTTP timeout + total budget (v1 bug):** each STS exchange must
  use a SHORT per-attempt timeout (5s), not httpc's default 30s (httpc.go:27) —
  otherwise one hanging attempt (the classic STS brownout: LB dropping
  SYN-ACKs) consumes bd's entire 30s helper-exec cap and the ladder never
  reaches serve-last-good. Total getToken wall-clock (all attempts + sleeps) is
  budgeted ≤ ~15s so the full ladder AND serve-last-good complete inside bd's
  30s `credCommandTimeout`. Exit test: STS blackholed (iptables DROP), warm
  cache → getToken emits last-good in <15s.
- **Serve-last-good:** if the exchange still fails AND the cache holds a token
  with true remaining validity above a floor (`ExpiresAt-now > 15s`, so bd's
  10s expiry skew does not instantly re-stale it into a helper-exec storm),
  emit it with a stderr warning instead of dying — the SPEC's
  degrade-to-stale-within-TTL behavior.
- **Envelope honesty:** `--json` emits the server's `res.ExpiresIn`
  (gettoken.go:296 currently hardcodes 90) — bd's cache honors real expiry.

## 6. Doctor + diagnostic contract (bd; separate PR after #4823)

- Replace the internals of the two gateway-blind builders with resolution
  through the canonical path (`NewFromConfigWithCLIOptions` /
  `applyResolvedConfig`): `doltServerConfig` (doctor/federation.go:31-44 — ~10
  consumers incl. Sync Staleness :438, Federation Conflicts :531, Dolt Mode
  :637, maintenance.go:139, migration_validation.go:206, validation.go:25) and
  `openDoltDB` (doctor/dolt.go:21-78 — CheckDoltConnection/CheckDoltSchema =
  the init post-check noise). **Exhaustive sweep** (grep-verified — the v1 list
  was incomplete and exit test 8 would still fail through the misses): also
  server.go:290, perf_dolt.go:122, fresh_clone_server.go:28, fix/metadata.go:500,
  fix/remotes.go:18, fix/validation.go:307, doctor_health.go:57, init_guard.go:40,
  cmd/bd/dolt.go:841 and :1902 (the `bd dolt *` subcommand family — otherwise
  `bd dolt` stays broken for hosted users), bootstrap.go:62 and :872. The sweep
  is a grep gate: any hand-built `dolt.Config{...ServerHost...}` or
  `doltutil.ServerDSN{...}` outside the canonical path fails CI.
- **hosted.Resolve error rendering:** once hosted.Resolve lives in
  `applyResolvedConfig` (WP-C), every doctor store-open (SharedStore
  doctor.go:509-510 and each `NewFromConfigWithCLIOptions` check) will ABORT
  with the §2.2 trust error on a mismatched/unattached hosted workspace — a
  crash before the gateway-trust layer can report a clean FAIL. Doctor must
  catch hosted.Resolve errors and render them AS the gateway-trust layer result
  (per-layer PASS/FAIL), not let them crash the store open.
- New doctor layers for hosted workspaces, in pipeline order with one remedy
  each (SPEC error table verbatim): workspace → helper (binary present,
  allowlisted) → session (helper dry-run; 403 stderr surfaced verbatim) →
  gateway-trust (allowlist + attachment hash) → DNS/TCP → TLS (map the
  distinguishable pre-auth 1045 "TLS required" to the TLS remedy) → auth →
  database (project-identity cross-check).
- Tighten `verifyProjectIdentity`'s swallowed read-error (store.go:1286-1289)
  for Gateway configs only: a read ERROR is a transient failure (retryable
  message), distinct from ABSENT (provisioning-contract error) — mirroring the
  #4823 init semantics on the open path.

## 7. Work packages, PRs, sequencing

| WP | Repo | Contents | Depends on |
|----|------|----------|-----------|
| **A** | bd (OSS) | §3: `creds.Source` interface + ArgvSource + ResolveForDial/Invalidate, connector, site conversions incl. transaction.go, gated ONE-shot 1045-invalidate in withRetry/withRetryTx, DSN sentinel redaction, timeout envs | none (independent of #4823) |
| **B** | gasworks | §5: flags + hoistPositional, destination gate + cache-key gateway dim (before cache read), trust-gateway (locked store), resilience (per-attempt timeout + budget + jitter + serve-last-good floor), expires_in | none |
| **C1** | bd (OSS) | §2+§4 core: trust/attachment stores, `hosted.GuardDestination` (unconditional, in ApplyGatewayCredential), terminal `hosted.Resolve` at all three sites, second-user detection, canonicalization | A (ArgvSource/creds.Source); B shipped for e2e |
| **C2** | bd (OSS) | `bd attach` command + verification connect + factored adoption helpers | C1; #4823 merged (adoption helpers) |
| **D** | bd (OSS) | §6 doctor exhaustive sweep + diagnostic contract + hosted.Resolve-error rendering | #4823 (field-report follow-up); C1/C2 for hosted layers |

**Split rationale (red-team):** v1's single WP-C hostaged the entire
destination-trust core to #4823's idle review. Only the adoption-helper
factor-out (C2) needs #4823; the trust stores, unconditional
`GuardDestination`, and terminal resolver (C1) touch none of #4823's diff and
can land first. **The security-critical enforcement (GuardDestination) is in
C1, which does NOT wait on #4823.**

Ship order: A ∥ B → C1 → C2 → D. Each lands as a real OSS PR
(contributors.md + template, `status/needs-review-auto`, no commercial
language — "an authenticating SQL gateway" phrasing, never product names) for
bd; B follows gasworks repo conventions. Every PR gets a Fable red-team pass
before commit (model policy).

## 8. Exit tests (SPEC S2 + grounding-derived)

1. **Long-lived client:** gc-controller-class process >1h continuous ops, zero
   1045s (per-dial re-mint across ≥40 token rotations) — run against
   `prj_848513b16e7b5c43` (the live S1 test project on cherry).
2. **Writes after rotation:** a writer idling >90s between writes succeeds
   (proves the transaction.go conversion; this is the case main fails today).
3. **Zero exports:** new terminal, attached workspace, `bd list` works with an
   empty environment (metadata + attachment record only).
4. **Exfil test (automated, CI) — ALL THREE shapes the red-team found (v1's
   single-shape test would have passed while the hole was open):**
   (a) `credential_helper` metadata → unlisted record host;
   (b) NO `credential_helper`, committed `.beads/config.yaml dolt.host` or
   metadata `dolt_server_host` = unlisted host, with an ambient
   `BEADS_DOLT_CREDENTIAL_COMMAND` set (the fleet/S1 shape);
   (c) attached workspace + committed `.beads/config.yaml dolt.host` = unlisted
   host (terminal-resolution test — dialed host must be the RECORD host, never
   config.yaml). In all three, a canary helper records invocation and MUST NOT
   fire — the destination gate refuses BEFORE any mint. Plus
   `gasworks getToken --gateway evil.example` refused pre-cache, pre-exchange.
5. **Attach idempotency:** re-run attach → no-op; tuple change → refused
   without confirm; `--accept` on unknown host → hard-fail.
6. **Second-user:** fresh clone with committed metadata → first bd command
   prints exactly the remediation; after `bd attach --accept` everything works.
7. **Cache-destination isolation:** getToken for gateway A then `--gateway B`
   (both trusted) → second call does NOT serve A's cached token.
8. **Doctor truth:** on a healthy hosted workspace, `bd doctor` and the init
   post-check report zero connection failures (regression test for the S1
   finding).
9. **Redaction:** grep test that no bd log/error output contains the token
   (DSN sentinel + never-log-ServerUser invariant).

## 9. Owner flags / open items

- **Machine leg absent from the gasworks CLI** (§0.1): which slice builds
  `GASWORKS_API_KEY` + `/sts/v0/machine` client support? Until it exists,
  `BD_TRUST_WORKSPACE=1` is honored unconditionally (§4.5) — a documented
  weakening of SPEC "honored only when GASWORKS_API_KEY is present" (it can
  still never trust a host outside the allowlist, and each argv-hash re-accept
  is logged loudly). Revisit when the machine leg lands.
- **gasworks repo is PUBLIC**: the S2 gasworks changes ship hosted defaults in
  an OSS repo (already the repo's practice — works.gascity.com is hardcoded
  today). Confirm this remains acceptable vs the "commercial code private"
  directive.
- **#4823 review risk**: only WP-C2 factors and reuses its adoption helpers;
  C1 (the security-critical core) does not depend on it (§7 split).
- **Worktree/container attach cost**: attachment records are per-canonical-path,
  so every git worktree, moved checkout, bind-mount alias, or ephemeral-HOME CI
  rebuild needs its own `bd attach --accept` (one command, but real for this
  org's worktree-heavy fleets). Canonicalization rule (filepath.Abs +
  EvalSymlinks, case-fold on macOS/Windows) is a §2.1 implementation detail;
  document the per-path cost in the attach docs and error text, not as a
  surprise. Fleet/k8s hosted rigs use the ENV path (both vars set by
  `mirrorBeadsDoltEnv`) and never require an attachment record.
- **Forward-compat of attach-written metadata**: an OLD bd binary cloning an
  attached repo sees `dolt_mode=server` + no host + unknown `credential_helper`
  field (ignored) → dials config.yaml/127.0.0.1 as root, may auto-start a local
  dolt, fails confusingly (the second-user remediation exists only in new
  binaries). Document a minimum-bd-version for hosted workspaces; consider a
  metadata `hosted_min_bd_version` marker an old binary can at least name.
- **bd release vehicle** (SPEC open question) still undecided — WP A-D target
  a tagged release; the panel installer story is S5.
