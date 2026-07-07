# Remote-GC — Implementation Handoff

**Purpose:** hand a fresh session enough to continue implementing remote-city support for the `gc` CLI without re-deriving the design. The **authoritative design + build checklist is `DESIGN-BRIEF.md` v2** (council-ratified) in this directory. This doc is the *operational* state: what's done, what's next, how to work, and the landmines.

> **CURRENT FRONTIER (2026-07-07): Slice 0 done (reads work); starting Slice 1 Phase 1 = "writes-first".** Human decisions locked: **(1) sequence writes-first — server hardening (G10/G11) + the grant client path (G18/G19) + a reference minter, so `gc --context prod sling <existing-bead>` mutates a hardened city end to end, BEFORE the big server-side rig provisioning (G12–G17,G20–G22); (2) build a reference minter `gc-write-mint`.** The concrete Phase-1 build spec is **§8** below. The grant contract to match is `internal/citywriteauth/citywriteauth.go` (`Grant`, `ReqDigest`, `Verify`) — token = `base64url(grantJSON) "." base64url(ed25519 sig over grantJSON)`.

**Date of handoff:** 2026-07-07. **Worktree:** `/data/projects/gascity/.claude/worktrees/gc-remote` (branch `feat/agent-workspace-source`). Run everything from the worktree root; do **not** `cd` to the main checkout.

---

## 1. First 5 minutes (do this before writing code)

1. Read `engdocs/plans/remote-gc-control-plane/DESIGN-BRIEF.md` end to end. It is the contract. §1 = the 9 locked decisions; §3 = the build checklist (Slice-0 gates **G1–G9**, Slice-1 gates **G10–G23**); §8/§10 = security + residual risks.
2. Confirm the one landed unit is green:
   ```bash
   go test ./internal/clientcontext/    # must pass
   go vet ./internal/clientcontext/
   ```
3. Skim §2 of the brief ("Verified current state") — every seam you'll touch is cited there with file:line.
4. Read the recalled memory note `remote-gc-control-plane-design` (auto-loaded) for the one-paragraph gist.

Do **not** relitigate the 9 decisions — they were made by the human one-at-a-time and ratified by a fable council + red-team. Refine *within* them.

---

## 2. What is DONE

- **Design fully ratified.** v1 (fable workflow) → 9-decision human interview → final fable council + red-team = **GO-with-required-refinements**. All refinements are folded into `DESIGN-BRIEF.md` v2.
- **`internal/clientcontext/`** — the `~/.gc/contexts.toml` leaf (gate **G5 storage half**). Types (`Context`, `File`), atomic `0600` load/save, `Lookup`, `EffectiveCity`, `Validate` (rejects control-chars/path-seps in name+city, non-loopback-http, dup names, dangling default). 10 tests, green + vet clean. Kept as a **pure path-parameterized leaf** (no `supervisor` import) so `DefaultPath()`/`GC_HOME` is a single seam added at the `cmd/gc` layer.
- **Slice 0 Phase 1 COMPLETE (gates G5 CLI half, G4, G2-at-resolution) — bead `ga-qq1or2`.** Untracked, tested, adversarially reviewed (2 real bugs found + fixed), exercised end-to-end.
  - NEW `cmd/gc/remote_target.go` — `remoteTarget`, `remoteSelection`, the pure `resolveRemoteSelection` (Decision-4 precedence + same-tier conflict table + credential/token binding), `resolveStickyDefault`, `DefaultPath()` (the one `supervisor.DefaultHome()` seam), `readRemoteSelection`/`resolveRemoteTarget` (impure), `remoteReadsEnabled=false` gate, `errRemoteNotSupportedYet`, `remoteFlagPresent`.
  - NEW `cmd/gc/cmd_context.go` — `gc context add/list/use/current/remove/show` (tabwriter + `-o json`; `current` is a pure dry-run of the resolver).
  - EDIT `cmd/gc/main.go` — persistent flags `--context`/`--city-url`/`--city-name` (+ `run()` reset); `resolvedContext.Remote`; `resolveContext` step-0 remote branch + step-4 sticky default + `remoteContextOrGate` (the single capability-gate choke point); `resolveCommandContext` positional+remote-flag conflict.
  - Tests: `remote_target_test.go` (precedence/conflict table), `cmd_context_test.go`, `remote_gate_test.go` (gate airtightness + the 2 review-fix regressions). All green; `go vet` clean. 336 resolution/routing + 135 direct-caller regression tests still green.
  - **Design refinements from the Phase-1 review (folded in, deviate from the original §3 wording):**
    1. **`apiClient` is NOT flag-guarded (apiroute.go left unedited).** A presence-based `remoteSelectionActive()` guard there wrongly tripped local `--city` commands that merely had a stray `GC_CITY_URL` in the env (bypassing the live local controller → disk divergence). G2's Phase-1 enforcement is instead: the **`resolveContext` gate** (a remote-resolved command errors before it can reach any local fallback) + **`guardNoAPI`** (GC_NO_API + a *resolved* remote target = loud error at resolution). Remote routing becomes **resolution-aware** in Phase 2, keyed on `resolvedContext.Remote`, never on sniffing global flags.
    2. **The contexts file loads only when a *named context* must be resolved** (`--context`/`GC_CITY_CONTEXT`/sticky default) — a malformed `contexts.toml` must never break a purely-local `--city` command.
    3. **`--rig` joins `--city` as a local-tier selector** (shadows a remote env; conflicts with a remote flag).
  - **Deferred to Phase 2 (review note, refuted as a Phase-1 bug):** `GC_CITY_URL_TOKEN` + a sticky-default context credential is not yet a conflict (the token is unused until transport lands); enforce the token-vs-context-credential conflict on the sticky-default tier when Phase 2 wires the bearer.

**Nothing is committed yet** — `engdocs/plans/remote-gc-control-plane/`, `internal/clientcontext/`, and the Phase-1 `cmd/gc/*` files are untracked. Suggested first commit: brief + handoff + `clientcontext` + the Phase-1 `cmd/gc` files together.

---

## 3. What is NEXT — Slice 0 (read-only remote), in dependency order

Slice 0's goal: `gc --context <name> status | beads list | session list/peek | convoy status | mail check | order history | events --follow` against a remote city, **hard-failing (never local-falling-back) on any error**. No grant needed (reads).

**Build order (each phase ≤5 files, TDD, `go test` + `go vet` green before the next):**

### Phase 1 — `gc context` CLI + the shared target resolver (gates G5 CLI half, G4, G2) — ✅ DONE (see §2; the sub-bullets below are the as-built record with the review refinements)
- NEW `cmd/gc/remote_target.go`:
  - `DefaultPath()` → `filepath.Join(supervisor.DefaultHome(), "contexts.toml")` (this is the one place that imports `supervisor` for the home seam; `clientcontext` stays pure).
  - `resolveRemoteTarget()` → `*remoteTarget{BaseURL, CityName, Ctx, ...}` or nil, implementing **precedence** (Decision 4): `--city-url`/`--context` flag > `--api` (alias of `--city-url`) > `GC_CITY_URL`/`GC_CITY_CONTEXT` env > (caller falls through to local discovery) > sticky `default` **only when no local city is discoverable**.
  - **Loud conflict errors** (reuse the `cityRefAmbiguousErr` pattern, `cmd/gc/city_arg_resolve.go:222`): remote+local (`--city-url` + `--city`), remote+remote (`--city-url` + `--context`; `GC_CITY_URL` + `GC_CITY_CONTEXT` to different targets), and `GC_NO_API` + a remote target.
  - Credential-to-target binding: refuse to use a context's `credential_command`/`grant_command` if the resolved target url/city differs from that context's binding.
- NEW `cmd/gc/cmd_context.go` — model on `cmd/gc/cmd_register.go` (tabwriter + JSONL, `-o json`). Subcommands: `add` (validates via `clientcontext.Context.Validate`), `list` (star the default), `use` (atomic write; warn it's subordinate to local cwd), `current` (**pure dry-run of the full resolver** — prints the winning tier + what was shadowed), `remove`, `show`.
- EDIT `cmd/gc/main.go` — register `--city-url`, `--city-name`, `--context` persistent flags next to `--city`/`--rig` (around `main.go:233`); add the **remote head-branch** to `resolveContext()` (`main.go:463`) returning `resolvedContext{Remote: target}` with **empty `CityPath`** (no fs walk); add the **capability gate** (a table of remote-capable commands; a non-table command under a remote target errors loudly `command "X" does not support a remote city yet`).
- EDIT `cmd/gc/apiroute.go` — **step-0 remote branch in `apiClient()` BEFORE the `GC_NO_API` nil-return** (G2, `apiroute.go:45`); add remote reason codes (`remote-unreachable`, `remote-unauthorized`) that are only ever *reported*, never used to pick a local path.

### Phase 2 — remote client transport (gate G6) — ✅ DONE (bead `ga-rdpbv3`; committed locally)
- DONE `internal/api/client_remote.go` (new): `NewRemoteCityScopedClient(baseURL, cityName, RemoteOptions)` — sets `isRemote`; one TLS/redirect policy → **two `*http.Client` shapes** (REST `Timeout:120s` + dial `15s`/TLS `15s`/response-header `30s` sub-budgets; STREAM `Timeout:0`). `remoteCheckRedirect` **refuses cross-host + refuses https→http downgrade + strips `Authorization` and every `X-GC-*`** on any followed hop + caps at 10. `remoteTLSConfig` (CA-file/SNI/insecure, MinVersion TLS1.2). `remoteAuthEditor` attaches `Authorization: Bearer` from a **live** `TokenSource` (never captured once). `RemoteOptions{Token,CAFile,TLSServerName,InsecureSkipVerify,RESTTimeout}`.
- DONE `internal/api/client.go` (edit): `Client.{isRemote,streamClient,tokenSource,tokenMu}` + `IsRemote()` + `bearerToken()` (mutex-guarded). `waitForEvent` now uses the stream client, attaches a fresh bearer per (re)connect, and runs a **per-frame-reset idle watchdog** (`remoteStreamIdleTimeout=45s`) that cancels a stalled remote stream — **local behavior unchanged** (nil streamClient → bare `http.Client`; nil token source → no auth; no watchdog).
- Tests: `client_remote_test.go` — redirect policy (cross-host/downgrade refusal, cred stripping, 10-cap), TLS (CA verify pass/fail, insecure, garbage-CA), auth editor, `NewRemoteCityScopedClient` basics + **e2e over `httptest.NewTLSServer`** (CA-verified header delivery, fail-without-CA, insecure-succeeds, cross-host-redirect-refused). `go vet` clean; the `Event|SSE|Stream|Sling|RigCreate` suite (110s) still green.
- **DEFERRED (documented deviation):** the `eventsAPIScope.client()` (`cmd_events.go:131`) threading is NOT done here — it needs the resolved remote target/options wired into the events command, which is command-dispatch integration. It lands when the events read path is enabled (with Phase 4 G7/G8). The transport primitives it needs are ready.

  **Bearer vs. capstone note:** the capstone (direct hardened city) uses the **grant** path (`X-GC-City-Write`, Slice 1 G18/G19), NOT the bearer built here. The `TokenSource`/`Authorization` machinery is for the crucible-fronted edge (Slice 3) and ad-hoc `GC_CITY_URL_TOKEN`. Building it now is correct (it's the uniform transport), but Slice-0 reads against a hardened city need no bearer.

### Phase 3 — compiler-enforced no-fallback (gates G1, G3) — ✅ DONE (bead `ga-...`; committed locally)
- DONE `internal/api/client.go`: `ShouldFallback`/`ShouldFallbackForRead`/`FallbackReason` now take a **nil-safe `*Client` first parameter** and return `false` (`FallbackReason` → `"remote"`) for any error from a remote client — the guard is at the TOP, before any `errors.As`, so it is type-independent (landmine #1: `errors.As` unwraps a transport wrapper, so remoteness must come from the client, not the error). **All ~27 non-test call sites updated** (the `if c != nil {…}` idiom passes its `c`; `cmd_extmsg`'s `extMsgReportBindError` was threaded a `c` param). **All ~23 test call sites** pass `nil` (pure error-classification). New `TestRemoteClientNeverFallsBack` pins the property. Correction to the original plan: `apiErrorFromResponse`/`checkMutation` are error **constructors** — they do NOT call the fallback funcs, so the signature change does not touch them; the classification sites (which DO have the client) are the enforcement point. Empty-`CityPath` store-open + malformed-URL guards are naturally covered by the resolveContext gate (remote never reaches a local store-open in Slice 0).
- DONE `cmd/gc/cmd_events.go`: **G3** — `--api` + a remote flag (`--city-url`/`--context`) is now a loud conflict (they share the flag tier; `--api` is the alias). The `--context`/`--city-url` events path (no `--api`) is already refused by the capability gate via `resolveDashboardContext`→`resolveCity`. `shouldUseLocalCityEventsFallback` already returns `false` for an explicit non-local-supervisor `--api` scope, so a remote 404 never reads `.gc/events.jsonl`. Tests (`cmd_events_remote_test.go`): the `--api`+`--context` conflict, the no-`.jsonl`-read property on a remote scope + 404, and the gated remote-context path.
- Verified: `go vet` clean on both packages; ~590 affected `cmd/gc` + `internal/api` tests green (beads/mail/convoy/wait/citystatus/extmsg/agent/session/rig/status/suspend/maintenance + fallback/remote classification + events scope).

### Phase 4 — remote stream reconnect + credential exec + request-id sink (gates G7, G8, G9) — ✅ DONE (bead `ga-esz5xt`; committed locally; adversarial review = 0 findings)
- DONE `cmd/gc/cmd_events.go`: **G7** — `classifyStreamStatus(status, retryAfter) streamRetry{reconnect,delay,reauth}` + `parseRetryAfter` (delta-seconds only, capped at `streamReconnectMax*4`) + `handleStreamNon200` (shared), wired into **both** `streamCityEventsOnce` and `streamSupervisorEventsOnce`. 429/503 → reconnect honoring `Retry-After` (via a ctx-aware `waitForReconnectDelay`); 401 → `reauth` (terminal on the local/unauthenticated stream; the remote path re-invokes the credential); 403/404/421/other → permanent. `--watch` (stopAfterMatch) still never reconnects (matches the connect-failed path). Body closed on every path.
- DONE `internal/clientauth/` (new): **G8** — `CredentialSource` runs the credential command via `sh -c` with the request JSON in **`GC_EXEC_INFO` (env only, never argv)**; `strippedEnv` removes every inherited `GC_*_INFO` so a nested exec can't read a stale request; `Token()` caches until `expiry − 30s skew`; `Refresh()` re-mints (the per-attempt 401 re-invoke); missing/empty token or missing/invalid `expiration_timestamp` is a hard error. Version `gascity.dev/client-auth/v1`. (`GC_CITY_URL_TOKEN` conflict vs a context credential is already enforced at resolution in Phase 1.)
- DONE `internal/api/middleware.go` + `event_payloads.go` + `client_remote.go`: **G9** — `withLogging` sinks the minted `X-GC-Request-Id` into the `api:` log line (`req_id=<id>`) and a new **`SupervisorRequestPayload.RequestID`** field (spec regenerated: `internal/api/openapi.json` + `docs/reference/schema/openapi.{json,txt}`; **no genclient change** — it's an event payload, not an HTTP type); `RequestIDForError(http.Header)` extracts it client-side. Extended `TestSupervisorRequestAuditRecordsBoundedPayload` to pin `RequestID == minted header`.
- Verified: `go vet` clean (3 packages); `go run ./cmd/genspec` clean (only `request_id`); `TestOpenAPISpecInSync`/`TestEveryKnownEventTypeHasRegisteredPayload` green; ~90 internal/api + events cmd/gc regression green; 3-lens adversarial review found **0 confirmed defects**.

---

## 7. Read-set enablement — IN PROGRESS (remote reads now work)

**Landed (`747cf8d39` + migration commit):** the gate is split — `resolveContext` is LOCAL-ONLY (errors on a remote target, so every non-migrated command stays safe), and `resolveContextAllowRemote` returns the remote target for remote-capable readers (the `remoteReadsEnabled` global is gone). `buildRemoteClient(target)` (`cmd/gc/remote_client.go`) constructs `api.NewRemoteCityScopedClient` wiring TLS + the bearer (from `credential_command` via a cached `clientauth.CredentialSource`, or an ad-hoc `GC_CITY_URL_TOKEN`). `resolveReadTarget()` returns a no-fallback remote client OR the local cityPath (so each command keeps its local seam). **Migrated read commands** (each routes remote with no fallback, local path unchanged): `beads list`, `beads show`, `convoy list`, `convoy status`, `mail peek`, `session list`, `session peek`, `wait list`, `wait inspect`. An end-to-end test drives `gc beads list` against an httptest TLS server (reaches `/v0/city/mc/beads` with `X-GC-Request`; the local seam is never called).

**Remaining before Slice-0 ships:**
- **Config-heavy reads** need bespoke handling (they load a local `*config.City`/orders/agents that a remote city lacks): `status`/`rig status` (cmd_status.go), `citystatus`, `mail check` (loads cfg + `citySuspended`), `order history` (loads cfg + orders). Decide per-command whether the API response is self-sufficient or the local-cfg dependency must move server-side. `convoy check` is deliberately local-only (its auto-close mutations must not run off cache-backed reads).
- **Events remote path**: wire `eventsAPIScope` to build/use the remote transport (deferred from G6), and thread `CredentialSource.Refresh()` into the stream-401 `reauth` hook (G7) with an anti-spin cap.
- **Target-echo + request-id**: emit the §5 `target: <city> @ <url> (…)` line on remote invocations, appending `RequestIDForError(resp.Header)` on failure (G9 client half).

Then: **checkpoint + human sign-off before Slice 1** (the write capstone, G10–G23).

_(Original enablement plan retained below for reference.)_
The remaining work is the **command-dispatch wiring** that flips the read set on:

1. **Build a remote client from a resolved `resolvedContext.Remote`** — a new seam (the local `apiClient(cityPath)` ladder never handles remote). Construct `api.NewRemoteCityScopedClient(target.BaseURL, target.CityName, RemoteOptions{…})`, deriving `RemoteOptions.Token` from either `target.Token` (ad-hoc `GC_CITY_URL_TOKEN`, a static source) or `target.Ctx.CredentialCommand` (a `clientauth.CredentialSource{}.Token`), plus `CAFile`/`TLSServerName`/`InsecureSkipVerify`/`Timeout` from the context.
2. **Route the read set through it** — `status | citystatus | beads list/show | wait | session list/peek | convoy list/status/check | mail check/peek/count | order history | events --follow/--watch` call the remote client instead of the local ladder when `ctx.Remote != nil`, and flip `remoteReadsEnabled = true` (or replace it with a per-command capability table). Because of **G1**, any remote error is already non-fallbackable — so this flip is safe.
3. **Thread the credential into the events stream** — `eventsAPIScope` gains the remote transport (deferred from Phase 2/G6) + the stream 401 `reauth` calls `CredentialSource.Refresh()` with an anti-spin cap (G7's reauth hook).
4. **Surface `RequestIDForError`** in the CLI failure **target-echo** line (§5) so a failed remote op prints `target: … (…) request_id=<id>`.

Then: **checkpoint + human sign-off before Slice 1** (the write capstone — G10–G23).

**Ordering safety note (satisfied):** G1 (no-fallback) is in place, so the read set can now be flipped on safely — a remote read error is surfaced, never silently fallen back to a local store.

After Slice 0: **checkpoint** and get sign-off before Slice 1 (the capstone: full `rig add --git-url` + `sling` against a direct hardened city — gates G10–G23, which include the fail-closed boot gate, `/svc/*` gating, the `internal/rig` extraction, the `request_id` state machine, git-clone hardening, and the `grant_command` path).

---

## 4. Landmines the council/red-team surfaced (do not step on these)

1. **`errors.As` unwraps.** A `remoteError` *wrapper* is transparent to `ShouldFallback` — that's why G1 is a `*Client` **parameter**, not a wrapper type or an instance field the free functions can't see.
2. **Three fallback planes leak outside `api.Client`:** `GC_NO_API` nil-return (G2), the events `.gc/events.jsonl` fallback (G3), and the localhost ladder. Each silently reroutes a remote op to **local disk**. Branch/error all three.
3. **The read plane on a hardened city is unauthenticated by design** — reads need no grant; that's why Slice 0 works against any city. Writes (Slice 1) need the grant path. (Read the residual-risk register, brief §8.)
4. **Slice-1 boot gap (red-team blocker):** today `InstallWriteAuth` only errors when `write_auth_required` is *already* set — a non-loopback + `allow_mutations` + **no-key** city boots **wide open**. G10 fixes this at both boot seams with an ack knob.
5. **`git_url` clone is an RCE/SSRF primitive** (Slice 1, G15): `ext::` RCE, `file://` exfil, metadata-IP SSRF, hooks/submodules. Harden in `internal/git` Layer 0.
6. **`request_id` must be a `(city, request_id)+body-digest` state machine** (G13) or Decisions 6 and 9 contradict (phantom rig id after rollback / retry that can never re-clone). Store as a bead **label/metadata**, NOT a new bead type (DoltLite `invalid issue type` trap).
7. **`internal/rig` extraction must RETIRE `controllerState.CreateRig`** (G12) — there are **three** overlapping rig-add paths today; leaving two = the object-model-at-center violation Decision 7 exists to prevent.
8. **Env-name collisions:** the client-context env is **`GC_CITY_CONTEXT`** (not `GC_CONTEXT`, which collides with `GC_CONTEXT_ADVISORY_PCT`) and **`GC_CITY_URL_TOKEN`** (not `GC_CITY_TOKEN`).
9. **`gc rig add` async wait** (Slice 1, G21) needs its **own heartbeat-anchored deadline**, not the 4-minute `sessionMessageTimeout` — a WAN clone exceeds it. Emit the SUCCESS event **only after** config is visible via `s.state.Config()` and the store is registered (G17), or `gc sling` right after 404s and the one-liner goes flaky.

---

## 5. How to work here (conventions)

- **TDD.** Test next to code (`x.go` → `x_test.go`), `t.TempDir()` for fs. Watch it fail, make it pass. Integration tests use `//go:build integration`.
- **Build cache:** **NEVER `go clean -cache`** (corrupts the shared fleet cache). `go clean -testcache` is fine. Cold build → `GOCACHE=$(mktemp -d) go build ./cmd/gc/`.
- **Verify each phase:** `go test ./<pkg>/` + `go vet ./<pkg>/`. For anything touching `internal/api/` schema: `make dashboard-check` + `TestOpenAPISpecInSync`. Spec regen (Slice 1): `go run ./cmd/genspec` + `go generate ./internal/api/genclient` + `make spec-ci`.
- **Invariants (CI-enforced):** object-model-at-center (CLI+API are projections), typed wire (no hand-written JSON; Huma-registered; OpenAPI generated), worker boundary (no new non-test `session.Manager` construction in `cmd/gc`), ZERO hardcoded role names.
- **Upstream mergeability:** prefer new files / small adapters / fork-owned packages. `request_id` and the `internal/rig` extraction are clean **upstream candidates** — shape them for proposal.
- **No commercial/hosted code in OSS.** The crucible edge (Slice 3) is private-repo work; the direct-self-host capstone (Slice 1) is entirely in this repo.
- **bd for task tracking** (not TodoWrite/markdown). Run `bd prime` for the workflow. gascity Dolt is **local-only** — never `bd dolt push/pull/remote`.
- **Git:** commit/push only when asked. Never `tmux kill-server`. Never bare `git stash` (shared stack) — use a WIP commit or `git stash push -u -m "<tag>"`.

---

## 6. Pointers

- Design contract: `engdocs/plans/remote-gc-control-plane/DESIGN-BRIEF.md` (v2).
- Design workflows (for reference/resume, not needed to continue): `.claude/wf-remote-gc-design.js`, `.claude/wf-remote-gc-council.js`.
- Key seams (all cited with line numbers in brief §2): `cmd/gc/apiroute.go` (`apiClient`), `internal/api/client.go` (`newClient`, `ShouldFallback`, `waitForEvent`), `cmd/gc/main.go` (`resolveContext:463`, flags:233), `cmd/gc/cmd_events.go` (`eventsAPIScope.client:131`, `streamCityEventsOnce`), `internal/api/writeauth.go` + `internal/citywriteauth/` (verify-only grant plane), `internal/supervisor/registry.go:363-387` (atomic-write template), `cmd/gc/cmd_register.go` (the `gc context` UX model).

---

## 8. Slice 1 Phase 1 — "writes-first" build spec (NEXT)

**Goal:** `gc --context prod sling <existing-bead>` performs a mutation against a DIRECT hardened city (`allow_mutations=true` + a verify key), with the grant minted client-side by a reference `gc-write-mint` and verified by the already-built `citywriteauth`. NO rig provisioning yet (that's Phase 2 = Group C). Human-locked: writes-first sequencing + build a reference minter.

**The grant contract (match exactly — `internal/citywriteauth`):**
- Token = `base64url(grantJSON) "." base64url(sig)`, `sig = ed25519.Sign(priv, grantJSON)`.
- `Grant{Kid,Aud,City,Epoch,IAT,Exp,JTI,Req}` (all JSON, snake-ish per struct tags: `kid,aud,city,epoch,iat,exp,jti,req`).
- `Req = ReqDigest(method, decodedPath, rawQuery, body)` = `hex(sha256( method "\n" path [ "\n" canonicalQuery ] "\n" hex(sha256(body)) ))`, canonicalQuery = `url.ParseQuery(rawQuery).Encode()` folded in only when non-empty.
- Verify checks: kid→key, ed25519 sig over payload, non-empty city/req/jti, `aud==gc-city-write`, `exp>iat`, `exp-iat<=MaxTTL`, now∈[iat-skew, exp+skew], `epoch>=floor`, `city==pathCity`, `req==ReqDigest(thisRequest)`, single-use jti (MemoryReplayGuard → single-replica).

**Build order (each step TDD, `go test`+`go vet` green before the next):**

1. **`citywriteauth` refactor (tiny, additive):** add `ReqDigestFromBodyHash(method, path, rawQuery, bodyHashHex string) string` and make `ReqDigest(...body []byte)` delegate to it (byte-identical → the golden vector + `golden_test.go` stay green). The minter needs this because it receives the **body hash**, never the body.
2. **`internal/clientgrant/` (new, mirror `internal/clientauth`):** the grant exec contract `gascity.dev/city-write-grant/v1`. `GrantInfo{Version,Aud,City,Method,Path,CanonicalQuery,BodySHA256,ReqDigest}` marshaled to env **`GC_GRANT_INFO`** (env only, never argv; strip inherited `GC_*_INFO`). `GrantSource.Mint(GrantInfo) (token string, err error)` execs `grant_command` via `sh -c`, returns stdout token, validates it splits into `payload.sig` with a 64-byte sig. **NO cache** — a grant is single-use + request-bound, so mint fresh per mutation (a retry mints a fresh grant).
3. **`cmd/gc-write-mint/` (new binary — the reference minter):** reads `GC_GRANT_INFO`, **re-validates** `aud==gc-city-write` + `city==--city`(if pinned), recomputes `req_digest` via `ReqDigestFromBodyHash` and refuses if it ≠ the client's claimed `req_digest`, stamps `kid`(`--kid`)/`epoch`(`--epoch`)/`jti`(random)/`iat`/`exp`(iat+`--ttl`, ≤2m), ed25519-signs with `--key <ed25519 seed/pem>`, prints the token. Never reads argv for the request info (env only). This is a dev/reference tool, kept OUT of the verify-only `citywriteauth` path.
4. **Grant editor (G18)** in `internal/api/client_remote.go`: `RemoteOptions.Grant clientgrant.GrantSource`; a genclient `RequestEditorFn` attached **LAST** (after X-GC-Request + Authorization) that, for a **mutating** method (not GET/HEAD/OPTIONS), computes `ReqDigest(req.Method, req.URL.Path, req.URL.RawQuery, body via GetBody+reset)`, mints the grant, sets `X-GC-City-Write`. Reads get NO grant. Tighten `remoteCheckRedirect` to refuse **all** redirects when the request carries `X-GC-City-Write`.
5. **Write routing:** `resolveWriteTarget()` (sibling of `resolveReadTarget`) builds the remote client with `RemoteOptions.Grant` from `ctx.Ctx.GrantCommand`; `sling` routes its mutation through it (no fallback — G1 already covers remote clients). Non-migrated mutations stay gated via `resolveContext`.
6. **Server hardening:**
   - **G10 fail-closed boot** at BOTH seams (`controller.go:~1349`, `cmd_supervisor.go:~1256`): non-loopback + `allow_mutations` + no resolvable verify key ⇒ refuse to boot, with an explicit ack knob (name TBD — the one genuinely-open Q from brief §12) + a release-note migration entry so netpol-fronted fleets don't brick. Boot-test both sites with/without key/ack.
   - **G11 gate `/svc/*`** on a hardened bind as a separate mux-layer change (leave `cityScopedObjectMutation` + the cross-repo golden vector untouched).
7. **E2E:** an `httptest` server wired to a real `citywriteauth.Verifier` (test key) + a test `GrantSource` (or `gc-write-mint` built into a temp dir) → `sling` mints → server verifies → mutation accepted; plus a digest round-trip test with a percent-encoded city name and a query-bearing mutation (brief §10).

**Landmines:** grant editor MUST be last (after body/query editors) or the digest won't match the wire body; the SSE result stream gets the credential editor but NEVER the grant editor; a retry mints a fresh grant (single-use); `GC_GRANT_INFO` env-only (never argv/`sh -c` interpolation); single-replica replay guard is an accepted residual (boot-warn a 2nd controller).

**Then Phase 2 (Group C):** the big server-side `rig add --git-url` provisioning — `internal/rig` extraction (retire `controllerState.CreateRig`), git-clone hardening (RCE/SSRF), async 202 + atomic rollback, `request_id` state machine, per-rig lock, StateMutator reload, typed events, heartbeat-anchored wait, Huma both-shapes + regen (G12–G17, G20–G22). Then the full capstone one-liner + runbook (G23).
