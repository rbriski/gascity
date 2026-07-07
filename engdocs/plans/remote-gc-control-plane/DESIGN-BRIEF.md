# Remote City Control Plane — Design Brief (v2, council-ratified)

**Location:** `engdocs/plans/remote-gc-control-plane/DESIGN-BRIEF.md`
**Status:** **APPROVED — go-with-required-refinements.** Nine decisions locked with the human; a fable council (architect / security / DX / SRE) + a red-team ratified the decided design and produced 9 Slice-0 gates + 14 Slice-1 gates. Build to the checklists in §3.
**Capstone (locked):** `gc --city-url=<direct-hardened-city> --city-name=<n> rig add --git-url <repo> … && gc --city-url=… sling <adopt-pr>` drives one adopt-pr through a pipeline merge against a **DIRECT self-hosted hardened city** — `allow_mutations=true` + a verify key + the client's `grant_command`, **zero crucible / private-repo work**.

> **Provenance.** v1 = fable design over 8 code-explorers + 3 prior-art studies, folded through a 4-persona fable review. Then 9 human decisions (interview, one-at-a-time). Then a final fable council + red-team (`.claude/wf-remote-gc-council.js`) ratified the decided design GO-within-decisions and surfaced 23 required refinements. This v2 bakes the decisions and refinements in. Every "verified" claim is anchored to a file:line an agent or the orchestrator actually read.

---

## 1. The nine locked decisions

| # | Branch | Decision |
|---|---|---|
| 0 | **Auth model** | Two *optional* exec sources per context: `credential_command` → transport **bearer** (consumed only by an edge/proxy; the controller ignores `Authorization`), and `grant_command` → **`X-GC-City-Write` grant** (direct hardened self-host). A city needing neither mutates over `X-GC-Request` alone. One uniform client attaches whatever is configured. |
| 2 | **Grant shape** | `grant_command` is a **per-request signer that RE-VALIDATES** (not a blind oracle). gc computes `ReqDigest` + assembles the claim in-tree; the helper receives method/path/query/bodyHash + city/aud as **separate JSON fields via env only**, re-validates `aud==gc-city-write` + `city==configured`, recomputes the digest, stamps `kid`/`epoch`/`jti`/`exp`, ed25519-signs. Key never enters gc. `internal/citywriteauth` **stays verify-only.** |
| 3 | **Config home** | `~/.gc/contexts.toml`, resolved via the **shared** `supervisor.DefaultHome()`/`GC_HOME` seam (0600, temp+rename). Separate from the path-based `cities.toml` registry. |
| 4 | **Precedence** | explicit flag > explicit env > **local city discovery (cwd)** > sticky `default`. Local beats the sticky default (git-like). Remote+local and remote+remote conflicts are **loud errors**. `--api` (existing) is reconciled as an alias of `--city-url`. |
| 5 | **First target** | **DIRECT self-hosted hardened city — zero crucible.** |
| 6 | **Idempotency** | Client-generated `request_id` on `SlingInput` **and** `RigCreateInput`, all shapes, in Slice 1, backed by a **(city, request_id)+body-digest state machine** (§7.3). |
| 7 | **Rig-add** | **Full server-side provisioning** (`git_url` clone + beads init + pack compose), behind a **pure `internal/rig` extraction** that **retires the parallel `controllerState.CreateRig` path** (no second domain implementation). |
| 8 | **Capstone** | **One** capstone: full `rig add` + `sling` together. |
| 9 | **Provision failure** | Async `202`+`EventCursor`; **atomic rollback** to no-rig on any step failure (dir + Dolt DB + config); `request_id` state machine makes re-run a clean re-clone. |

**Council verdict:** all four seats `ratify-with-refinements`; red-team found no reason to relitigate; **GO within the nine decisions.** Residual risks knowingly accepted: §10.

---

## 2. Verified current state (ground truth)

- **`apiClient()` (`cmd/gc/apiroute.go`)** is localhost-only. **Three fallback planes reroute to local disk and each must be branched/errored for remote:** (a) `GC_NO_API` → `client=nil` → local file mutation (`apiroute.go:45`); (b) the events path (`resolveEventsScope` / `shouldUseLocalCityEventsFallback` in `cmd/gc/cmd_events.go`) grafts the *local* city name onto an override URL and reads `.gc/events.jsonl` on 404; (c) the standalone/supervisor localhost ladder itself.
- **`api.Client` (`internal/api/client.go`)**: `newClient(baseURL, cityName)` builds `genclient` with one `WithRequestEditorFn` (`X-GC-Request`) + `WithHTTPClient(&http.Client{Timeout: defaultClientTimeout})` — both accept **multiples**, the clean injection point. **`ShouldFallback`/`ShouldFallbackForRead` are package-level free functions over `error`** (`client.go:199-226`), classify via `errors.As` (which **unwraps** — a transport wrapper is invisible), and there are ~27 call sites plus shared helpers (`apiErrorFromResponse`, `checkMutation`). `waitForEvent` (`client.go:301-322`) and `eventsAPIScope.client()` (`cmd_events.go:131`) are **two more wire sites**, each a bare `&http.Client{}` with no auth and no `CheckRedirect`.
- **Write-auth (`internal/api/writeauth.go`, `internal/citywriteauth/`) is fully built + verify-only.** Three gates: (a) CSRF `X-GC-Request` **always on**; (b) read-only **purely bind-driven** (`nonLocal && !allow_mutations`, `controller.go:1348-1349`, `cmd_supervisor.go:1255-1256`); (c) grant gate **purely verify-key-driven, orthogonal to bind** (`supervisor.go:231`). Grant = single-use (`jti`, `MemoryReplayGuard` **process-local → single-replica**), request-bound (`ReqDigest(method, decoded Path, canonical RawQuery, sha256(body))`), per-city (`Expect{City}`), ≤2m TTL. `cityScopedObjectMutation` **excludes `/svc/*`** (`writeauth.go:89`). **Boot gap:** `InstallWriteAuth` errors only when `write_auth_required` is *already* set — a non-loopback + `allow_mutations` + **no-key** city **boots wide open** (§3 Slice-1 gate G10).
- **Sling** `POST /v0/city/{n}/sling` (`huma_handlers_sling.go` → `handler_sling.go` → intent wrappers → `internal/sling` `DoSling`): **core parity** (materialize + hook + auto-convoy + controller poke + telemetry). Diverges: no agent nudge (local default is *also* no-nudge), no batch/convoy-child expansion, no `request_id`. **Source-bead formula launches are lock-guarded** (`withSourceWorkflowLaunchLock`/`withGraphV2SourceWorkflowLock`, `sling_core.go:402,508`) with a `ConflictError`→409 — the adopt-pr shape is retry-safe even before `request_id`.
- **Rig create** `POST /v0/city/{n}/rigs` (`humaHandleRigCreate`) only appends `config.Rig` via `StateMutator.CreateRig` (`state.go`). Local `doRigAdd` (`cmd/gc/cmd_rig.go`) does the real provisioning **and** a *third* path exists (`controllerState.CreateRig`/`initializeRigStoreForCreate`) — **three overlapping rig-add paths** to collapse (§7.2).
- **Registry atomic-write pattern** (`internal/supervisor/registry.go:363-387`): `MkdirAll 0700` → temp → `toml.NewEncoder` → `os.Rename`, file `0600`; `DefaultHome()` (`config.go:206`). The template for `internal/clientcontext/`.
- **`X-GC-Request-Id`** is minted into the response header by `withRequestID` (`middleware.go:329`) and **discarded** by the client.
- **Env-name collision:** `GC_CONTEXT` collides with the existing `GC_CONTEXT_ADVISORY_PCT` knob → the client-context env is **`GC_CITY_CONTEXT`** (and `GC_CITY_TOKEN` → **`GC_CITY_URL_TOKEN`**).

---

## 3. Build checklists (the authoritative "what to build")

### Slice 0 — read-only remote (9 gates, all in-repo, no grant)

- **G1. Compiler-enforced no-fallback.** Change `ShouldFallback`/`ShouldFallbackForRead`/`FallbackReason` to take a **nil-safe `*Client`** parameter; return `false` for any error from a remote client (`isRemote`), regardless of type (`connError`, `serverError`, `readOnlyError`=403, `cacheNotLiveError`, `clientInitError`). The signature change forces all ~27 call sites + `apiErrorFromResponse`/`checkMutation` through the remote decision at compile time. Suppress `FallbackReason` route-log lines for remote clients.
- **G2. Branch remote target BEFORE `GC_NO_API`** (`apiroute.go:45`); `GC_NO_API` + a resolved remote target = **loud error**, never a fallback.
- **G3. Events plane non-fallbackable for remote.** `shouldUseLocalCityEventsFallback` returns `false` for remote scopes; `resolveEventsScope` must **not** soft-fill `cityPath`/`cityName` from cwd for a remote target. Reconcile `--api` into the precedence chain as an alias of `--city-url` (error if both; remote city name never from cwd). Test: remote scope + 404 ⇒ no `.gc/events.jsonl` read.
- **G4. One shared target resolver replaces the `resolveCity()` preamble** across the ~10 read commands (they resolve-city-then-client and `return 1` *before* the `apiClient` injection point, so the step-0 branch alone cannot carry precedence). Pin the tier-conflict table: `--city` + `--city-url`/`--context` = error; `GC_CITY(_PATH/_ROOT)` + `GC_CITY_URL`/`GC_CITY_CONTEXT` = error.
- **G5. `internal/clientcontext/`** (contexts.toml load/save via the shared `DefaultHome()`/`GC_HOME` seam, 0600, temp+rename) + **`gc context add/list/use/current/remove`**. `add` validates city names for control chars (they break the grant digest preimage later). `current` is a pure dry-run of the full resolver (prints winning tier + what was shadowed).
- **G6. `internal/api/client_remote.go`** — `NewRemoteCityScopedClient(baseURL, cityName, opts…)` sets `isRemote`, threads an **`Authorization` bearer editor** + **one TLS/redirect policy** into **two client shapes**: REST (`Timeout:120s` + dial/TLS sub-budgets) and **STREAM (`Timeout:0` + per-frame-reset context-cancel idle watchdog ~45s; never `http.Client.Timeout`)**. `CheckRedirect` **strips `Authorization` + every `X-GC-*` and refuses cross-host + refuses https→http downgrade.** Thread the stream shape + a **mutex-guarded token source** + `CheckRedirect` into **`waitForEvent` (`client.go:322`) AND `eventsAPIScope.client()` (`cmd_events.go:131`)** — both bare `http.Client` today (a construction-time token capture makes per-reconnect 401 re-mint a no-op).
- **G7. Remote stream reconnect.** One **shared stream-status classifier** honoring `Retry-After` on **429 AND 503**, `404/421/403` permanent, wired into **both** `streamCityEventsOnce` and `streamSupervisorEventsOnce` (both return `reconnect=false` for any non-200 today). On 401, re-invoke `credential_command` **per attempt** with an anti-spin guard against a revoked credential.
- **G8. `credential_command` exec contract** (`internal/clientauth/`, versioned `gascity.dev/client-auth/v1`): env `GC_EXEC_INFO` = `{spec:{server_url(resolved+validated https), city, interactive}}`; stdout `{token, expiration_timestamp REQUIRED}`; in-memory cache to expiry, checked before every REST request **and every SSE (re)connect**; 401 re-invoke per-attempt. `GC_CITY_URL_TOKEN` honored **only** with an ad-hoc `--city-url`/`GC_CITY_URL` target; conflict error against a context credential technique. Pin canonical env names by test.
- **G9. Server `X-GC-Request-Id` sink** (Slice-0 deliverable): `withLogging` reads the header into the `api:` log line; add a `RequestId` field to `SupervisorRequestPayload` (additive typed event + regen). Client captures `X-GC-Request-Id` from every non-2xx into the error string + the failure target-echo line.

### Slice 1 — the capstone: direct-self-host writes (14 gates)

- **G10. Fail-closed boot check at BOTH seams** (`controller.go:~1349`, `cmd_supervisor.go:~1256`): non-loopback + `allow_mutations=true` + **no resolvable verify key ⇒ refuse to boot** (or require `write_auth_required`). Extend `InstallWriteAuth` with bind context; add an explicit **ack knob** (so netpol-fronted fleets don't brick on upgrade) + a release-note migration entry; boot-test both sites with/without key/ack.
- **G11. Gate-or-refuse `/svc/*` on a hardened bind** as a **separate mux-layer change**, leaving `cityScopedObjectMutation` + the cross-repo golden vector untouched (`serviceRequestAllowed` currently permits a private-service mutation on only `X-GC-Request`, a direct-published one on nothing).
- **G12. Extract `internal/rig`** as a pure mechanical commit (`sling.SlingDeps` model: inject `fsys.FS`, step/warning callback, `ReloadConfig`, store-init funcs, pack installer) and **RETIRE `controllerState.CreateRig`/`initializeRigStoreForCreate`**, collapsing the two config writers to the surgical comment-preserving append. Both `cmdRigAdd` and `humaHandleRigCreate` delegate. Tests pin byte-identical `city.toml`/`packs.lock`/`routes.jsonl` local-vs-API; add an import-guard test mirroring the worker-boundary guard. `resolveRigAddPath`'s dot-relative branch stays in `cmd/gc` (server path is `cityPath`-relative under a server-owned root).
- **G13. `request_id` state machine** (§7.3) pinned in the spec **before** implementation.
- **G14. Async `202` conditional on `git_url` present** (keep sync `201` for bare config-append). **Atomic rollback in the SERVER orchestration layer around the shared core, never inside `internal/rig`.** Created-vs-preexisting manifest; stage clones in a temp dir under a server-owned root, rename on full success; delete the dir only when this run created it; rollback also drops the created rig DB under managed Dolt; sweep orphaned partial dirs at boot (in-progress marker) or document the loud guard error.
- **G15. Harden the server-side git clone** in `internal/git` (Layer 0, `gitpkg.SanitizedEnv`): allowlist `https`(+`ssh`); reject `file://`, `ext::`, bare local paths; `protocol.ext.allow=never`/`protocol.file.allow=never`; `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `core.hooksPath=/dev/null`, `--no-recurse-submodules` unless opted in; SSRF-filter loopback/link-local/private/metadata IPs with **post-redirect re-check**; never persist embedded `git_url` credentials into `city.toml`.
- **G16. Per-rig-name in-flight provisioning lock** (mirror `withSourceWorkflowLaunchLock`): concurrent different-`request_id` POSTs for the same rig name get 409 with a typed body carrying the in-flight `request_id` + `EventCursor` to re-attach; run the `request_id` dedupe under that lock; it also serializes the process-global `registerCityDoltConfig`/`clearCityDoltConfig`.
- **G17. Config reload through `mutateAndPoke`/`pokeCh` (StateMutator), NOT the CLI `reloadControllerConfig`** (a controller dialing its own control socket mid-request self-deadlocks). Keep the provisioning goroutine off the state/config lock. Emit the rig-create **SUCCESS event only after** config is visible via `s.state.Config()` **and** `state.BeadStore(rigName)!=nil` (else `gc sling` immediately after 404s — the flagship one-liner goes flaky).
- **G18. Grant minting in a genclient `RequestEditorFn` against the FINAL `*http.Request`**: `ReqDigest(method, decoded `URL.Path`, `RawQuery`, body via `GetBody`+reset)`, ordered **after every other body/query editor**. Grant covers **only the POST**; the SSE result stream gets the **credential** editor but **NEVER the grant** editor. **Refuse ALL redirects on grant-bearing requests.** Test a client↔server round-trip digest with a percent-encoded name and a query-bearing mutation.
- **G19. `grant_command` re-validates** (not blind-sign): validate `aud`/`city`, receive method/path/canonicalQuery/bodyHash as separate JSON fields to recompute `req_digest`; pass `GC_GRANT_INFO`/`GC_EXEC_INFO` **via env only (never argv/`sh -c`)**, strip inherited `GC_*_INFO`, JSON-encode every field. A retry mints a **fresh uncached** grant (test: two identical mutations exec the helper twice). `request_id` stays in the body.
- **G20. Typed events for the async op**: `RequestOperationRigCreate` + `RequestResultRigCreate`/`RequestFailed` carrying `request_id` (+ rig name, resolved prefix/branch) and `rig.provision.progress {step: clone|beads-init|packs|config}`. Terminal failure MUST be an `events.RequestFailed` carrying `request_id`. Reuse `emitAsyncResult`/`EmitRequestFailed`/`recoverAsRequestFailed`. (`TestEveryKnownEventTypeHasRegisteredPayload` fails the build otherwise, and `waitForEvent` blocks until SSE close if envelopes lack `request_id`.)
- **G21. Rig-create wait**: its own generous **heartbeat-anchored, watchdog-bounded** deadline (NOT the 4-minute `sessionMessageTimeout` — a WAN clone routinely exceeds it). Pull a **minimal reconnect** into Slice 1 for the rig-add wait only (`Seq` on `sseEnvelope`, resume `after_seq` from the 202 `EventCursor`, per-attempt 401 re-mint); on unrecoverable failure print `request_id` + a resume recipe (terminal event queryable via the non-stream events list).
- **G22. Model both response shapes in Huma/OpenAPI** (`201` config-only, `202`+`EventCursor` provisioning); regen `openapi.json` + `genclient` + dashboard TS **in the same commit**; add `request_id` to `SlingInput` + `RigCreateInput`. Preserve **byte-identical** local output (`internal/rig` returns typed errors/step events; `cmd/gc` renders the exact current strings; warn-and-continue stays warnings mapped to streamed warning events). Pre-check bodies against the 1 MiB cap client-side.
- **G23. Capstone runbook** (§11): SPA gets 401 on every mutation by design; exactly one key source (`GC_CITY_WRITE_PUBKEY` env overrides config; `GC_CITY_WRITE_EPOCH_FLOOR` env-only); supervisor variant needs `[supervisor] allowed_hosts` to include the public hostname or requests die 421; loud boot warning enumerating the fully-unauthenticated read surface + requiring a network/TLS front.

---

## 4. The central fork — Option A (unify on the API)

The CLI has **one** remote transport: the typed HTTP+SSE control plane. Target resolution decides the base URL — local directory ⇒ today's loopback ladder unchanged; remote URL ⇒ `api.NewRemoteCityScopedClient` against the terminus, fail-closed, **no local fallback** (§3 G1–G3). Rejected: a `CityBackend` interface (a second domain surface reimplementing what the API projects — the object-model-at-center violation) and a 14-file rewrite (upstream merge tax). The API already *is* the abstraction: typed, generated, spec-synced, CI-pinned. YAGNI — the "second implementation" is the same generated client at a different URL.

## 5. The auth spectrum & config model

One uniform client attaches whatever the context configures. The three gates are independent (CSRF always; read-only purely bind; grant purely key), so:

| Deployment | Config | Client presents | grant infra |
|---|---|---|---|
| localhost | loopback, no key | `X-GC-Request` (automatic) | none |
| direct trusted | `allow_mutations`, no key | `X-GC-Request` | none |
| **direct hardened (capstone)** | `allow_mutations` + verify key | `X-GC-Request` + **grant** (via `grant_command`) | per-city reference minter |
| crucible-fronted (Slice 3) | verify key = crucible pubkey | **bearer** (edge mints grant) | crucible `CityWriteMinter` |

`~/.gc/contexts.toml` (shared `DefaultHome()`/`GC_HOME`, 0600):

```toml
default = "prod-self-host"   # subordinate to local cwd discovery (Decision 4)

[[context]]
name = "prod-self-host"
url  = "https://box.internal:9443"      # https required for non-loopback + bearer
city = "maintainer-city"                # remote city name; defaults to name
grant_command = "gc-write-mint --key ~/.gc/keys/maintainer.ed25519"  # direct hardened
# credential_command = "eia-helper token --audience gc-city"         # crucible-fronted (Slice 3)
# ca_file / tls_server_name / insecure_skip_verify (dev-only; +bearer requires explicit ack)
# timeout = "120s"                        # REST only; NEVER applied to SSE streams
```

Precedence (Decision 4): `--city-url`/`--context` (flag) > `--api` alias > `GC_CITY_URL`/`GC_CITY_CONTEXT` (env) > local cwd discovery > sticky `default`. Remote+local and remote+remote conflicts are loud `cityRefAmbiguousErr`-style errors. The credential is bound to its context's `url`+`city`; a resolved target that differs refuses to invoke it. `--city-name` is a new flag (never overload `--city`). Every remote invocation echoes `target: <city> @ <url> (context: <n>, cred=exec:<helper>|grant:<helper>|none, route=remote)`; the failure variant adds the captured `X-GC-Request-Id`.

## 6. Command routing & the no-fallback rule
Covered by §3 G1–G4. In short: a nil-safe `*Client` parameter makes non-fallbackability an instance property the compiler enforces across all ~27 call sites; the three out-of-`api.Client` fallback planes (`GC_NO_API`, events `.jsonl`, localhost ladder) are each branched/errored for remote; a shared target resolver runs *before* the `resolveCity()` preamble that today returns 1 first; malformed remote URLs fail at resolution and never construct a client; empty-`CityPath` store-open is a hard precondition error.

## 7. Server-side work (Slice 1)

### 7.1 The direct hardened city, end to end
`grant_command` mints a fresh grant per mutating request (G18/G19). The controller's existing verifier is the final authority — unchanged. `/svc/*` and supervisor-scope mutations stay off the hardened surface (G10/G11).

### 7.2 `internal/rig` extraction (G12)
Pure mechanical commit first: move `doRigAdd`'s provisioning into `internal/rig` (deps injected, `sling.SlingDeps`-style), **retire** the parallel `controllerState.CreateRig` path, collapse two config writers to one comment-preserving append. Both callers delegate; byte-identical artifacts pinned; import-guard test. This is a clean **upstream-candidate**.

### 7.3 `request_id` state machine (G13, spec-first)
Key `(city, request_id)` + stored **body digest**, as a **bead label/metadata** (NOT a new bead type — a new DoltLite type hits the `invalid issue type` trap). Three states: `in_flight` → replay the original `202` + original `EventCursor`; `succeeded` → return existing ids; `failed+rolled_back` → **purge** so a same-id retry re-clones. Responses: `202` new / `202` same-id in-flight / `200`+existing / `409` body-mismatch. `request_id` echoed in success bodies. Cross-invocation dedupe of a *succeeded* provision is by rig **name** with a targeted collision error. This is what keeps Decisions 6 and 9 from contradicting.

> **PINNED (C1).** The full state-machine contract is specified in
> **[`G13-request-id-state-machine.md`](./G13-request-id-state-machine.md)** (adversarially
> reviewed). Headlines: client-generated `request_id` (inverting the session server-mint
> precedent); a `task` bead + `gc.idem.*` metadata durable record fronted by an **in-process
> live index (§3.5)** that defeats the hosted ledger's read-after-write lag (a critical
> double-clone hole a lock alone does not close); the deterministic body digest; **six** response
> codes (adds `400 invalid_request_id` + `409 rig_name_conflict`); `rolled_back`-as-re-executable
> with drop-then-mark rollback (boot **and** runtime); in-flight-replay gated on a live entry so
> orphans re-clone; a **unified Huma output struct** + manual `op.Responses["200"|"202"]` (three
> documented 2xx codes); and an **expanded C4 per-city caveat** (`city.toml` + routes +
> `cityDoltConfigs`, a distinct non-reentrant per-city lock). Build C4/C6/C7 against that file.

### 7.4 Async provisioning + rollback + git hardening
G14/G15/G16/G17/G20/G21 — staged temp-dir clone + rename; server-orchestration-layer atomic rollback (dir + Dolt DB + config); per-rig-name lock; StateMutator config reload with success-after-visible; Layer-0 git hardening; typed async events; heartbeat-anchored wait with minimal reconnect.

## 8. Security & residual risks

**Enforced:** per-city binding end-to-end (bearer + grant + client-refuses-mismatched-target); grant is request-shape-bound by construction; `grant_command` re-validates and runs env-only; fail-closed boot on hardened binds (G10); `/svc/*` gated on hardened binds (G11); TLS required for non-loopback+bearer, `insecure_skip_verify`+bearer refused without explicit ack; `CheckRedirect` strips creds + refuses cross-host, all redirects refused on grant-bearing requests; Layer-0 git clone hardened against ext::/file://SSRF/hooks/submodules.

**Residual risks knowingly accepted (documented in the runbook):**
- **Single-replica only** — `MemoryReplayGuard` is process-local (bounded replay window ≤ 2m TTL + 30s skew); shared `ReplayGuard` + shared `request_id` store deferred to Slice 3. Refuse/boot-warn a second controller against the same write-auth city.
- **Unauthenticated read plane** — write-auth gates only mutations; beads/mail/session peek/transcripts + the 202 stream are exposed to anyone reaching the port. Mitigation is a network/TLS front, not in-band auth.
- **Same-user trust** — anyone running as the gc user can invoke `grant_command`; protection is `0600 contexts.toml` + treating helper-exec access as write access.
- **Single-tenant repo-content trust** — a malicious repo's *content* runs in pipeline agents; only transport abuse is blocked. Revisit for crucible multi-tenant.
- **`GC_CITY_URL`/`GC_CITY_CONTEXT` fail OPEN on an old binary** — automation MUST use explicit flags.
- **1 MiB write-auth body cap** (pre-checked client-side); **epoch-floor inert by default**.

## 9. Phased delivery

- **Slice 0 — read-only remote.** §3 G1–G9. `contexts.toml` + `gc context` + `client_remote.go` (two shapes, `CheckRedirect`, `isRemote`) + compiler-enforced no-fallback + shared resolver + read set (`status`, `citystatus`, `beads list/show`, `wait`, `session list/peek`, `convoy list/status/check`, `mail check/peek/count`, `order history`, `events --follow/--watch`) + remote stream reconnect + `X-GC-Request-Id` sink. No grant. **Ships against any city.**
- **Slice 1 — the capstone (direct self-host).** §3 G10–G23. Full `rig add --git-url` + `sling` → adopt-pr merge against a direct hardened city. `request_id`, `internal/rig` extraction, async provisioning + rollback, git hardening, fail-closed boot + `/svc/*`, grant path. Upstream-candidates: `request_id`, `internal/rig`.
- **Slice 2 — streaming/long-ops.** Shared SSE-follow helper; generalize `waitForEvent` reconnect; `session logs -f` remote; per-context WAN timeouts.
- **Slice 3 — crucible-fronted multi-tenant + UX.** bearer→grant edge (shape allowlist, per-city quota, per-city keys, error taxonomy); `gc login` device flow; shared `ReplayGuard` + shared `request_id` store; `scope` claim on `Grant`; multi-city UX; 0a/0b distinction becomes moot (Slice-0 is read-only, Slice-1 writes go direct-grant).

## 10. Testing plan
Honors `TESTING.md` tiers; no live sockets for routing (existing `apiRouteControllerAliveHook`/`apiRouteSupervisorClientHook` seams). Highlights: precedence table incl. local-beats-default + conflict loud-errors + malformed-URL-fails-at-resolution; **remote-never-touches-disk** across *every* capability-table command in an empty `t.TempDir()` (incl. the events `.jsonl` plane and `resolveAgentForAPI`-class pre-call reads); no-fallback pinned via the `*Client` param over an unreachable endpoint for **every exported method, pinned to method count**; auth+transport parity across **all three** wire sites against `httptest.NewTLSServer` w/ custom CA (success w/ `ca_file`, fail without, cross-host redirect drops creds); `credential_command`/`grant_command` golden transcripts (version match, expiry-required, per-attempt refresh, SSE-reconnect re-mint, ENV-only, fresh-grant-per-retry, re-validation); write-gate integration (test-minted grants; digest round-trip w/ percent-encoded name + query); `request_id` state machine (202-new/202-inflight/200-existing/409-mismatch, rollback purge, name-collision); async provisioning (rollback leaves no dir/DB, partial-crash sweep, git-hardening rejects `ext::`/`file://`/SSRF); fail-closed boot (both seams, with/without key/ack); `internal/rig` byte-identical + import-guard; spec discipline (`TestOpenAPISpecInSync`, `make dashboard-check`, `TestEveryKnownEventTypeHasRegisteredPayload`, tutorial-goldens unaffected).

## 11. Capstone runbook (direct hardened city)
Stand up maintainer-city bound to a network addr with `allow_mutations=true` + a verify key (single source: `write_auth_verify_key` config **or** `GC_CITY_WRITE_PUBKEY` env, not both) behind a network/TLS front. Boot emits a loud warning enumerating the unauthenticated read surface. `gc context add prod --url https://… --city maintainer-city --grant-command "gc-write-mint --key …"`. Then `gc --context prod rig add --git-url <repo> …` (202 → progress events → success) `&& gc --context prod sling <adopt-pr>` → convoy → merge, observed via `gc --context prod events --follow`. Expect: the dashboard SPA 401s on mutations (by design); a supervisor-managed variant needs `[supervisor] allowed_hosts` to include the public hostname (else 421).

## 12. Open questions — resolved by the nine decisions
Q1 (rig-add) → full provisioning (D7). Q2 (grant minting) → direct `grant_command`, edge only for Slice 3 (D0/D2/D5). Q5 (self-hosted remote writes without crucible) → **ANSWERED yes — that is the capstone.** Remaining genuinely-open: shared `ReplayGuard` backend design (Slice 3); whether `scope` claims or per-city keys gate multi-tenant (Slice 3); the ack-knob name for G10.

---

### Change log (v1 → v2)
Baked in the 9 human decisions + 11 council brief-edits. Biggest correction: **Slice 1 is full server-side provisioning against a DIRECT hardened city (no crucible), not register-only/crucible-fronted** — the v1 register-only path and the crucible front door moved to Slice 3. No-fallback became a compiler-enforced `*Client` param (not an instance-field-only flag) + the three leaking fallback planes. Added: fail-closed boot gate, `/svc/*` gate, git-clone hardening, the `request_id` state machine, `internal/rig` extraction-retires-parallel-path, grant-helper re-validation + env-only exec, grant-editor-last + refuse-all-redirects, per-rig lock, StateMutator config reload, typed async events, heartbeat-anchored rig-add wait, `X-GC-Request-Id` sink, `GC_CONTEXT`→`GC_CITY_CONTEXT` + `GC_CITY_TOKEN`→`GC_CITY_URL_TOKEN` renames, and a Residual Risks section + capstone runbook.
