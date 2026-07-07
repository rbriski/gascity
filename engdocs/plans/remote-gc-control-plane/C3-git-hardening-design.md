# C3 — Server-side git-clone hardening (G15) implementation design

**Parent:** `DESIGN-BRIEF.md` §3 gate **G15** (`DESIGN-BRIEF.md:63`), §7.4
(`:133`), §8 residual risks (`:136`). Sibling of the C2.2 `internal/rig.Provision`
design (where the clone step slots in) and the G13 request-id state machine
(`G13-request-id-state-machine.md` §6, the rollback contract this clone must obey).

**Scope.** `rig add --git-url <repo>` (Slice 1) makes the controller shell out to
`git clone` against an **API-caller-supplied URL**. That URL is an attacker-controlled
string reaching a subprocess on the server — an RCE / SSRF / credential-exfil primitive.
This doc specifies the Layer-0 `internal/git` clone entrypoint, its scheme allowlist,
its hardened env + flags, the SSRF IP fence with the post-redirect problem, credential
non-persistence, the test matrix, and the honestly-scoped residual risks. **No code.**

**Threat model (Slice 1, single-tenant).** Per brief §8 (`:145`), a malicious repo's
*content* running in pipeline agents is an **accepted** risk ("only transport abuse is
blocked"). This gate blocks the **transport-layer** primitives the *URL* opens —
arbitrary-command transports (`ext::`), local-file exfil (`file://`), and SSRF at the
fetch — not what the cloned bytes later do. Content trust is revisited for crucible
multi-tenant.

**Ground truth — what exists today.**

- `internal/git/git.go` has **no `Clone` method**. Confirmed: the package surface is
  worktree ops (`WorktreeRemove:118`, `WorktreeList:131`), status/branch probes, and
  `Fetch` (`git.go:213`) / `PullRebase` (`git.go:240`) — none clone. G15 **adds** one.
- All git invocation is **argv-only**: `runCtx` builds `exec.CommandContext(ctx, "git",
  args...)` (`git.go:420`) — no `sh -c`, no shell string. The Clone entrypoint MUST
  preserve this.
- `SanitizedEnv()` (`git.go:337`) strips the `gitEnvBlacklist` repo-discovery vars;
  `HermeticEnv()` (`git.go:349`) strips more and pins `GIT_CONFIG_NOSYSTEM=1` +
  `GIT_CONFIG_GLOBAL=/dev/null`. The clone builds **on `HermeticEnv`** (strongest base).
- `UntrustedRemoteGitConfigArgs()` (`git.go:385`) is the existing hardening for
  *pack* fetches. **It is too permissive to reuse verbatim for the rig clone**: it sets
  `protocol.file.allow=always` (`git.go:393`) and `protocol.http.allow=always`
  (`git.go:390`) because CLI-local packs legitimately need `file://` and plaintext.
  G15 requires `file` and `ext` **denied**. So G15 needs a **stricter sibling** helper,
  not a reuse. (See §3.)
- The SSRF IP fence already exists, battle-tested, in `internal/api/pack_source_policy.go`:
  `packSourceHostResolver` (`:17`, a stubbable seam), `ensurePublicPackSourceHost`
  (`:108`), `isInternalIP` (`:150`), `parseLooseIPv4` (`:167`, decodes inet_aton
  literals `0x7f000001` / `2130706433` / `0xA9FEA9FE`). G15 **reuses this by extraction**,
  it does not reinvent it (see §4).
- Credential redaction already exists: `gitcred.RedactUserinfo` (`internal/gitcred/redact.go:17`).
  G15 reuses it for §5.

---

## 1. The Clone entrypoint

### 1.1 Signature (Layer 0, `internal/git`)

A package function, not a `*Git` method — the clone runs **before** any worktree
exists, so there is no `workDir` to scope a `*Git` to:

```go
// CloneOptions tunes a hardened clone. The zero value is the safe default:
// https-only, submodules off, redirects refused, no credential injection.
type CloneOptions struct {
    // AllowSSH additionally permits ssh:// and scp-form (git@host:path) URLs.
    // Off by default (brief: "allowlist https (+ssh, gated)").
    AllowSSH bool
    // RecurseSubmodules opts back INTO submodule fetch. Off by default:
    // submodule URLs are a second untrusted-URL surface and .gitmodules can
    // point ext::/file:// (mitigated by protocol.allow=never, but still a
    // second network fan-out). Slice 1 leaves it off.
    RecurseSubmodules bool
    // Depth, when >0, passes --depth for a shallow clone (provisioning wants
    // history-light; caller default is a full clone for adopt-pr fidelity).
    Depth int
    // Branch, when set, passes --branch (single default branch).
    Branch string
    // Cred is optional per-city credential injection (§5); zero = anonymous.
    Cred gitcred.Injection
}

// Clone performs a hardened `git clone <url> <dst>` into an EMPTY dst that the
// caller owns. url is validated against the scheme allowlist (§2); the caller
// MUST have already run the SSRF host fence (§4) — Clone re-asserts the scheme
// guard fail-closed but does not itself resolve DNS. It never runs a shell; all
// hardening rides argv (-c) and env built on HermeticEnv.
func Clone(ctx context.Context, url, dst string, opts CloneOptions) error
```

Rationale for `ctx`: G21 gives rig-create its own heartbeat-anchored, watchdog-bounded
deadline — a WAN clone routinely exceeds `sessionMessageTimeout` (brief `:` G21). The
context is that deadline; `exec.CommandContext` already threads cancellation (`git.go:419`).

### 1.2 Where the clone target dir comes from

The dst is a **staging dir under a server-owned root**, per G14 (`DESIGN-BRIEF.md:133`):
"stage clones in a temp dir under a server-owned root, rename on full success." Clone
never writes directly to the final `req.Path`. The orchestration owns:

1. server-owned root (e.g. `<cityPath>/.gc/provision/<request_id>/`),
2. `Clone(ctx, url, stagingDst, opts)` into it,
3. on full provision success, `os.Rename(stagingDst, req.Path)` (atomic same-filesystem move),
4. on any failure, remove the staging dir — the rollback in §1.4.

### 1.3 Where the step slots into `internal/rig.Provision`

Today `Provision` (`internal/rig/provision.go:25`) has **no clone step** — it stats an
already-present `req.Path` (`provision.go:65`) and `MkdirAll`s it when absent
(`provision.go:210`, C2.2 step 10). The `--git-url` clone lands **before** that:

- **New Provision step ~2.5 (clone), between C2.2 step 2 (stat `req.Path`) and step 10
  (`MkdirAll`).** When `req.GitURL != ""`: the caller has already staged an empty dir;
  Clone populates it; the populated dir then flows into the existing `rigPathExists`
  path (it now exists, so step 10's `MkdirAll` is skipped and git-detect at step 3,
  `provision.go:79`, sees the freshly-cloned `.git`). This reuses the entire existing
  adopt/prefix/beads-init pipeline — the clone just *materializes* the directory the
  rest of Provision already knows how to consume.
- The clone is driven through a **new nil-optional `Deps` func**, mirroring the
  `ProbeBranch func(rigPath string) string` seam (`deps.go:42`):

```go
// CloneGitURL populates an empty staging dir from req.GitURL with the hardened
// clone (C3/G15). nil = the caller does not support --git-url (CLI local path,
// or config-append-only). Fatal-with-rollback when set and it fails.
CloneGitURL func(ctx context.Context, gitURL, stagingDst string, opts git.CloneOptions) error
```

  and a `GitURL string` + `RecurseSubmodules bool` on `ProvisionRequest` (`deps.go:73`).
  The CLI passes `nil` (local `rig add <path>` keeps working byte-identically); the API
  orchestration passes a closure over `git.Clone`. This keeps `internal/rig` from
  importing anything new beyond `internal/git` (already a clean dep per C2.2 step-table
  legend) and keeps the atomic-rollback wrapper in the **server orchestration layer, not
  inside `internal/rig`** — exactly what G14 mandates (`DESIGN-BRIEF.md:133`).

### 1.4 Failure is fatal-with-rollback (coordinate with G14 / G13 §6)

Clone failure is **F (fatal, no partial rig)**. It occurs *before* any config write, so
there is no config/DB to unwind at the `internal/rig` layer — but the **staging dir it
created must be removed**, and that removal is the server orchestration's job (G14). The
ordering is pinned by G13 §6 (`G13-request-id-state-machine.md:322`): *"a record never
reaches `rolled_back` until the partial clone dir / rig DB / any config registration is
fully removed."* So on Clone failure the orchestration:

1. removes the staging dir (partial clone), 2. drops any DB/config it had begun (none
yet at this step), 3. `SetMetadataBatch(state=rolled_back)`, 4. emits terminal
`events.RequestFailed` carrying `request_id` (G20). A same-digest retry then re-clones
into a *fresh* staging dir (G13 §4.2 re-clone path). Because Clone writes only into a
per-`request_id` staging dir, a crashed clone leaves an orphan the G14 boot sweep
reclaims (`G13-request-id-state-machine.md:330`).

---

## 2. URL scheme allowlist

### 2.1 Policy

| Scheme / form | Slice-1 verdict | Why |
|---|---|---|
| `https://` | **ALLOW** | the only default transport |
| `ssh://`, scp-form `git@host:org/repo` | **ALLOW iff `opts.AllowSSH`** | brief "(+ssh, gated)"; auth rides `GIT_SSH_COMMAND` (§5), key never in URL |
| `http://` | **REJECT** | plaintext; credential-in-clear + trivial MITM redirect to internal target. (The pack path allows it at `git.go:390`; the rig path must not.) |
| `git://` | **REJECT** | unauthenticated, unencrypted; no reason for a provisioned rig |
| `file://`, bare local path (`/abs`, `./rel`, `~`) | **REJECT** | reads server-local repos — arbitrary local-filesystem **exfil** into a rig the caller can then pull back via `sling`/beads. Mirror `validateHTTPPackSource`'s `file`/`local` rejection (`pack_source_policy.go:52-55`). |
| `ext::<cmd>` | **REJECT** | `ext::sh -c '…'` runs an **arbitrary command** as the gc user — direct **RCE**. Highest-severity item. |
| anything else (`ftp::`, `fd::`, `<unknown>::`) | **REJECT** | fail-closed default |

### 2.2 Parsing + validation (fail-closed, net/url)

Do **not** hand-scan the string. Classify with the same discipline as
`packSourceHost` (`pack_source_policy.go:66`), which already handles `file://`,
`git@`-scp-form (incl. bracketed IPv6 `git@[::1]:repo`, `:73`), `ssh/https/http`, and
the `github.com/` shorthand. G15's classifier is **stricter** — it returns an explicit
error taxonomy instead of a host string:

1. `ext::` prefix (and any `<word>::` transport-helper form — `strings.Contains(url, "::")`
   before the first `/`) ⇒ `ErrSchemeExt`. Check this **first**, before `url.Parse`,
   because `ext::` is not a parseable URL scheme.
2. `file://` prefix ⇒ `ErrSchemeFile`.
3. `url.Parse` the remainder. `Scheme=="http"` / `"git"` ⇒ `ErrSchemeInsecure`.
   `Scheme=="ssh"` ⇒ allow iff `opts.AllowSSH` else `ErrSchemeSSHNotEnabled`.
   `Scheme=="https"` ⇒ allow.
4. Empty scheme + looks-local (`/`, `./`, `../`, `~`, or a bare `github.com/…` shorthand
   that would resolve locally) ⇒ `ErrBareLocalPath`. **Exception:** the scp-form
   `git@host:path` has no `://` but is a valid ssh remote — detect it exactly as
   `packSourceHost` does (`:70`) and route it through the ssh-gated branch.
5. A `url.Parse` **error** ⇒ `ErrUnparseableURL` (fail closed; never fall through to a
   subprocess with a string git might interpret as a local path or option).

### 2.3 Error taxonomy

Exported sentinels in `internal/git` (`errors.Is`-matchable), each mapping to an API
400 `invalid_git_url` with a caller-safe reason:

```
ErrSchemeExt          // "ext:: transport is not permitted"
ErrSchemeFile         // "file:// sources are not permitted"
ErrBareLocalPath      // "local filesystem paths are not permitted; use an https URL"
ErrSchemeInsecure     // "http:// and git:// are not permitted; use https"
ErrSchemeSSHNotEnabled// "ssh sources are not enabled for this city"
ErrUnparseableURL     // "git URL could not be parsed"
```

All are **wrapped with `%w`** and rendered through the URL **already run through
`gitcred.RedactUserinfo`** (§5) so no error string ever echoes a
`https://user:pass@…` back to the caller or the log.

---

## 3. Hardened clone env & flags

### 3.1 The stricter config-args helper (new, sibling of `UntrustedRemoteGitConfigArgs`)

`UntrustedRemoteGitConfigArgs` (`git.go:385`) cannot be reused — it explicitly allows
`file` and `http`. Add a stricter helper (name e.g. `RigCloneHardeningArgs()`), argv
`-c` overrides prepended before the `clone` subcommand:

```
-c protocol.allow=never          # deny-by-default: any transport not re-allowed below is refused
-c protocol.https.allow=always   # the ONLY unconditionally-allowed transport
-c protocol.ssh.allow=always     # ONLY appended when opts.AllowSSH
-c protocol.ext.allow=never      # explicit belt-and-suspenders (deny-default already covers it)
-c protocol.file.allow=never     # DIVERGES from the pack helper on purpose
-c http.followRedirects=false    # the SSRF redirect kill-switch (§4)
-c core.hooksPath=/dev/null       # no post-checkout / fsmonitor hook executes (matches packman baseHardeningGitArgs, cache.go:285)
-c core.fsmonitor=false           # no fsmonitor process spawn (matches cache.go:284)
-c submodule.recurse=false        # unless opts.RecurseSubmodules
```

`protocol.ext.allow=never` is redundant given `protocol.allow=never` (ext is never
re-allowed) but stated explicitly so a future edit that flips the default can't silently
re-open the RCE transport — and G15 names it as a required flag (`DESIGN-BRIEF.md:63`).

### 3.2 Clone-specific argv flags

- `--no-recurse-submodules` (unless `opts.RecurseSubmodules`) — brief-mandated. Even
  with `protocol.file.allow=never` fencing a malicious `.gitmodules`, submodule fetch is
  a second URL fan-out that the primary SSRF fence never saw; default off.
- `--depth N` only when `opts.Depth>0`.
- `--branch <b>` only when `opts.Branch` set.
- `--` terminator before `<url> <dst>` so a URL that begins with `-` can never be
  parsed as a clone option (option-injection guard; complements the scheme allowlist).

### 3.3 Env (built on `HermeticEnv`, not `SanitizedEnv`)

Base = `HermeticEnv()` (`git.go:349`) — strips repo-discovery + pins
`GIT_CONFIG_NOSYSTEM=1` / `GIT_CONFIG_GLOBAL=/dev/null` so the clone reads **no** system
or user git config (a user-level `url.<x>.insteadOf` or `credential.helper` can't
rewrite the URL or leak a credential). Append:

```
GIT_TERMINAL_PROMPT=0     # never block on an interactive credential prompt (would hang the goroutine)
GIT_ASKPASS=/bin/false    # any askpass fallback exits non-zero instead of prompting
SSH_ASKPASS=/bin/false    # ssh's own askpass path, for the gated ssh branch
GIT_CONFIG_NOSYSTEM=1     # (already set by HermeticEnv; harmless to reassert)
```

These are passed **as env**, not argv, because they are process-environment knobs git
reads directly. `opts.Cred.Env` (§5) is appended last, exactly as
`defaultRunNetworkGit` does (`cache.go:319`). Everything transport-policy is argv `-c`
(§3.1); everything prompt/askpass/config-location is env — the same split
`baseHardeningGitArgs` + `HermeticEnv` already use.

### 3.4 Argv-only invariant (confirm)

The clone is `exec.CommandContext(ctx, "git", allArgs...)` where `allArgs =
RigCloneHardeningArgs() ++ opts.Cred.CfgArgs ++ ["clone", cloneFlags…, "--", url, dst]`.
**No `sh -c`, no shell string, no interpolation into a command line** — identical to
`runCtx` (`git.go:420`) and `defaultRunNetworkGit` (`cache.go:315`). The one place a
shell is involved anywhere in this stack is git's own `!`-helper for credentials
(`gitcred/inject.go:110` `shellQuote`), and that is a *credential-helper* concern G15
does not touch; the clone argv itself is shell-free.

---

## 4. SSRF IP filter with the post-redirect re-check

### 4.1 The pre-fetch host fence (reuse by extraction)

Before Clone, the API orchestration resolves the URL host and rejects internal targets,
**exactly** as `validateHTTPPackSource` already does for POST /packs
(`pack_source_policy.go:49`). The logic — `ensurePublicPackSourceHost` (`:108`),
`isInternalIP` (`:150` covers loopback, RFC1918 + IPv6 ULA via `IsPrivate`, link-local
`169.254.0.0/16` **including `169.254.169.254` metadata**, fe80::/10, unspecified), and
`parseLooseIPv4` (`:167`, decoding the inet_aton literals git's C resolver accepts but
`net.ParseIP` rejects: `0x7f000001`, `2130706433`, `0177.0.0.1`, `0xA9FEA9FE`→metadata)
— is **already correct and adversarially built**. G15 does **not** duplicate it.

**Recommendation: extract the fence into a shared package** (e.g. `internal/ssrf` or a
new `internal/git` file) so both the pack path and the rig-clone path call one
implementation with one stubbable resolver seam (`packSourceHostResolver`,
`pack_source_policy.go:17`). Duplicating it would let the two copies drift — a security
regression waiting to happen. This is the DRY/one-source-of-truth call.

### 4.2 The post-redirect problem — be honest about git

git uses libcurl/its own HTTP client; **there is no Go transport hook** to intercept each
redirect and re-run `isInternalIP` on the post-redirect IP. A `net/http`-style
`CheckRedirect` (like `client_remote.go`'s, brief G6) **does not exist for the git
subprocess**. So the classic "resolve host → verify public → git connects" fence has a
hole: a fenced public host `200`-passes the fence, then `30x`-redirects the clone to
`http://169.254.169.254/…` *inside git*, after the check.

**Options considered:**

1. **Disable redirects entirely — `http.followRedirects=false` (RECOMMENDED, §3.1).**
   git refuses to follow *any* redirect; a `30x` becomes a hard fetch failure. This makes
   the "post-redirect re-check" **vacuously satisfied — there is no post-redirect state to
   re-check.** It is the strongest feasible mechanism and is exactly what the existing
   pack hardening relies on (`git.go:387`, and its doc at `:373` names this as closing the
   "redirect to `169.254.169.254` after the host check passed" class). Cost: a legitimate
   host that 301s the clone URL (e.g. a canonicalizing redirect) fails; acceptable for
   Slice 1 since we require `https` and callers pass canonical repo URLs.
2. **Pre-resolve + pin the IP.** Not feasible for git's own HTTP client — git has no
   `--resolve` (that's curl) and no way to pin a URL to a caller-chosen IP without a
   `url.insteadOf` rewrite that breaks TLS SNI/Host. Rejected.
3. **Manual redirect loop** (`git ls-remote` per hop, re-validating each `Location`)
   before the real clone. This is the *only* way to actually re-check across redirects if
   we ever must *allow* them. It is complex, races DNS (the loop's resolution ≠ the
   clone's), and is **deferred to Slice 3** (crucible multi-tenant, where a shared
   `ReplayGuard` + real edge already exist).

**Decision:** primary control = **redirects fully disabled** (§3.1) + **pre-fetch host
fence** (§4.1). Together they close the redirect-SSRF class. The residual is DNS
rebinding (§7), identical to and no worse than the pack path
(`pack_source_policy.go:43`).

### 4.3 Where the fence runs

At the **API orchestration boundary** before `Deps.CloneGitURL` is invoked — the same
place `validateHTTPPackSource` runs before `packAddImport` (`pack_source_policy.go:48`) —
so a blocked URL returns **400 before any subprocess spawns**. `git.Clone`'s own scheme
guard (§2) is the fail-closed inner layer; the SSRF resolve is the outer layer. Defense
in depth, not a single control.

---

## 5. No credential persistence

### 5.1 The rule

An API caller can embed userinfo: `--git-url https://user:token@github.com/org/repo`.
That token must **never** land in `city.toml`, `routes.jsonl`, a log line, an error
string, or an event payload.

### 5.2 Mechanism

- **Config:** `config.Rig` records the rig by name/path/prefix — it does **not** persist
  a `git_url` today (grep confirms no `GitURL`/`git_url` field in `internal/config`). G15
  keeps it that way: the `--git-url` is a **provisioning-time-only input**, consumed by
  Clone and discarded. Nothing writes it back. If a future field is ever added to record
  provenance, it MUST store `gitcred.RedactUserinfo(url)` (`gitcred/redact.go:17`), never
  the raw string.
- **Logs / errors / events:** every place the URL appears in an error (§2.3), a step
  detail, or a `RequestFailed`/`progress` event (G20) passes it through
  `gitcred.RedactUserinfo` first — the same call `defaultRunNetworkGit` (`cache.go:307`)
  and `defaultHeadCommit` (`importsvc/source.go:230`) already use. `RedactUserinfo`
  rebuilds the URL with `u.User=nil` → `https://***@host/…` (`redact.go:27-32`) and has a
  string-fallback for URLs `url.Parse` rejects (`:39`), so even a malformed
  credential-bearing URL is masked.
- **Preferred path — don't put the secret in the URL at all.** The gated-ssh and
  https-credential cases should ride `opts.Cred` (`gitcred.Injection`,
  `gitcred/inject.go:15`): ssh keys via `GIT_SSH_COMMAND` env (`inject.go:64`), https
  tokens via the gc `git-credential` helper (`inject.go:84`). Then the clone URL carries
  **no userinfo**, the secret lives in env/helper only, and there is nothing to strip.
  Userinfo-in-URL is tolerated-and-redacted, not the blessed path.

---

## 6. Test matrix (`internal/git`, table-tests, `TESTING.md` unit tier)

All under `t.TempDir()`; the SSRF cases use the **stubbable resolver seam**
(`packSourceHostResolver`, `pack_source_policy.go:17`) — no real DNS, no network.

### 6.1 Scheme allowlist (`Clone` / classifier)

| input | want |
|---|---|
| `ext::sh -c 'touch /tmp/pwned'` | `ErrSchemeExt`, **no subprocess spawned** |
| `EXT::…`, `fd::…`, `foo::bar` | `ErrSchemeExt` / fail-closed reject |
| `file:///etc/passwd`, `file://localhost/repo` | `ErrSchemeFile` |
| `/etc/shadow`, `./repo`, `../repo`, `~/repo` | `ErrBareLocalPath` |
| `http://github.com/o/r`, `git://github.com/o/r` | `ErrSchemeInsecure` |
| `ssh://git@github.com/o/r` with `AllowSSH:false` | `ErrSchemeSSHNotEnabled` |
| `ssh://…` / `git@github.com:o/r` with `AllowSSH:true` | allowed (ssh branch) |
| `https://github.com/o/r` | allowed |
| `-oProxyCommand=…` (leading-dash URL) | rejected by parse **and** `--` terminator (assert both) |
| `https://%zz@h/r` (unparseable) | `ErrUnparseableURL`, redacted in message |

### 6.2 SSRF host fence (shared fence, stub resolver)

| host / literal | resolver stub | want |
|---|---|---|
| `169.254.169.254` (metadata, literal) | — | blocked |
| `0xA9FEA9FE`, `2852039166` (inet_aton metadata) | — | blocked (via `parseLooseIPv4`) |
| `127.0.0.1`, `0x7f000001`, `2130706433`, `0177.0.0.1` | — | blocked (loopback) |
| `localhost`, `x.localhost` | — | blocked |
| `[::1]`, `fe80::1`, `fc00::1` | — | blocked (v6 loopback/link-local/ULA) |
| `10.0.0.5`, `192.168.1.1`, `172.16.0.1` | — | blocked (RFC1918) |
| `evil.example.com` | stub → `169.254.169.254` | blocked (resolves-internal) |
| `github.com` | stub → public IP | allowed |
| `unresolvable.invalid` | stub → error | **allowed to proceed** (fence blocks only positively-internal, matching `pack_source_policy.go:135`; git surfaces its own failure) |

### 6.3 Post-redirect / redirect-refusal

- Assert `http.followRedirects=false` is present in the assembled argv (the concrete
  mechanism, since a live `30x` re-check is not feasible against the git subprocess).
- Integration (build-tagged, `test/`): a local `httptest` server that `30x`-redirects to
  `127.0.0.1` — clone **fails** (does not follow), no connection to the internal target.
  Documents that redirect-refusal, not per-hop re-check, is the control.

### 6.4 Credential stripping

- `https://user:tok@github.com/o/r` → every error/step/event string asserts
  `RedactUserinfo` form `https://***@github.com/o/r`, raw `tok` **absent** from all
  outputs (assert `!strings.Contains(out, "tok")`).
- Nothing writes the URL to `city.toml` (parse the written config, assert no `git_url` /
  no `user:` substring).

### 6.5 Hardened-env / argv assertion (golden)

A `Clone` variant with an injected command-capture seam (mirror `defaultRunGit`'s
`runGit` indirection, `cache.go:257`) asserts the exact argv and env:

- argv contains, in order: `protocol.allow=never`, `protocol.https.allow=always`,
  `protocol.ext.allow=never`, `protocol.file.allow=never`, `http.followRedirects=false`,
  `core.hooksPath=/dev/null`, then `clone`, `--no-recurse-submodules`, `--`, url, dst.
- argv does **not** contain `protocol.file.allow=always` / `protocol.http.allow=always`
  (the divergence from `UntrustedRemoteGitConfigArgs` — a regression tripwire).
- env (over `HermeticEnv`) contains `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`,
  `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_CONFIG_NOSYSTEM=1`.
- With `AllowSSH:false`, argv has **no** `protocol.ssh.allow=always`.
- With `RecurseSubmodules:false`, argv has `--no-recurse-submodules` and
  `submodule.recurse=false`.

### 6.6 Rollback wiring (in `internal/rig` / orchestration tests, cross-ref C2.2 + G13)

- Clone-fails → Provision returns fatal, `MkdirAll`/beads-init/config-write **never
  run**, staging dir removed, no `config.Rig` returned (assert against a fake
  `CloneGitURL` that errors).
- Aligns with G13 §6 ordering: `rolled_back` only after the staging dir is gone
  (`G13-request-id-state-machine.md:322`).

---

## 7. Risks

### In scope for Slice 1 (closed by this design)

- **`ext::` RCE** — closed by scheme allowlist (§2) + `protocol.allow=never` /
  `protocol.ext.allow=never` (§3.1). Two independent layers.
- **`file://` / bare-path local exfil** — closed by scheme allowlist +
  `protocol.file.allow=never` (the deliberate divergence from the pack helper).
- **Redirect-based SSRF to metadata/internal** — closed by `http.followRedirects=false`
  (§4.2 option 1) + the pre-fetch host fence (§4.1), including the inet_aton-literal
  bypasses `parseLooseIPv4` already decodes.
- **Hook execution on checkout** — `core.hooksPath=/dev/null` + `core.fsmonitor=false`.
- **Interactive-prompt hang / credential prompt** — `GIT_TERMINAL_PROMPT=0`,
  `GIT_ASKPASS=/bin/false`, `SSH_ASKPASS=/bin/false`.
- **Credential leak into config/logs** — §5, via `RedactUserinfo` + never-persist.
- **System/user git-config injection** (`insteadOf`, ambient `credential.helper`) —
  `HermeticEnv`'s `GIT_CONFIG_NOSYSTEM=1` + `GIT_CONFIG_GLOBAL=/dev/null` (`git.go:358`).

### Things git makes genuinely hard (honest residuals)

- **DNS rebinding TOCTOU (accepted).** git re-resolves the host at fetch time, so a name
  that resolved public during the §4.1 fence can resolve internal at the clone. There is
  no IP-pin for git's HTTP client (§4.2 option 2 rejected). This residual is **identical
  to and no worse than** the existing pack path, already documented at
  `pack_source_policy.go:43` and `git.go:382`. Real fix (pinned-IP fetch) is a Slice-3
  edge concern.
- **Per-hop redirect re-validation is not possible** against the git subprocess — the
  design *avoids* the problem by refusing redirects rather than re-checking them. If a
  future requirement forces allowing redirects, only the manual-`ls-remote`-loop (§4.2
  option 3) can re-check, and it is deferred.
- **Submodule / LFS untrusted-URL fan-out.** Submodules are off by default
  (`--no-recurse-submodules`); if `opts.RecurseSubmodules` is ever set, each submodule
  URL is a fresh untrusted URL the §4.1 fence never saw and `protocol.file.allow=never`
  only partially fences. **git-LFS** runs its own smudge/`git-lfs` fetch outside these
  `protocol.*` knobs — a repo with LFS pointers can trigger a second network fetch; Slice
  1 does not enable LFS and should set `GIT_LFS_SKIP_SMUDGE=1` in the clone env as a
  belt-and-suspenders (add to §3.3 if LFS is present in the environment).

### Explicitly out of scope (brief §8, `DESIGN-BRIEF.md:145`)

- **Repo *content* trust.** A malicious repo's files/build scripts run in pipeline agents
  later; **only transport abuse is blocked here.** Single-tenant accepts this; crucible
  multi-tenant revisits it.
- **Same-user trust** — anyone as the gc user can already invoke git; this gate is about
  the *network caller's* URL, not local users.
