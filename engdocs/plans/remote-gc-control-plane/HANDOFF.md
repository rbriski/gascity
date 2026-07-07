# Remote-GC — Implementation Handoff

**Purpose:** hand a fresh session enough to continue implementing remote-city support for the `gc` CLI without re-deriving the design. The **authoritative design + build checklist is `DESIGN-BRIEF.md` v2** (council-ratified) in this directory. This doc is the *operational* state: what's done, what's next, how to work, and the landmines.

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

### Phase 3 — compiler-enforced no-fallback (gates G1, G3)
- EDIT `internal/api/client.go`: give `ShouldFallback`/`ShouldFallbackForRead`/`FallbackReason` a **nil-safe `*Client` parameter**; return `false` for any error from a remote client (`isRemote`), regardless of type (`connError`, `serverError`, `readOnlyError`=403, `cacheNotLiveError`, `clientInitError`). Update **all ~27 call sites** + `apiErrorFromResponse`/`checkMutation` (the compiler will find them). Malformed remote URL fails at target resolution, never constructs a client. Empty-`CityPath` store-open becomes a hard precondition error.
- EDIT `cmd/gc/cmd_events.go`: **G3** — `shouldUseLocalCityEventsFallback` returns `false` for remote scopes; `resolveEventsScope` must not soft-fill `cityPath`/`cityName` from cwd for a remote target. **Test: remote scope + 404 ⇒ no `.gc/events.jsonl` read.**

### Phase 4 — remote stream reconnect + credential exec + request-id sink (gates G7, G8, G9)
- EDIT `cmd/gc/cmd_events.go`: **G7** — one **shared stream-status classifier** honoring `Retry-After` on **429 AND 503**, `404/421/403` permanent, wired into **both** `streamCityEventsOnce` and `streamSupervisorEventsOnce` (both return `reconnect=false` for any non-200 today). On 401 re-invoke `credential_command` **per attempt** with an anti-spin guard.
- NEW `internal/clientauth/`: **G8** — the versioned exec contract (`gascity.dev/client-auth/v1`): env `GC_EXEC_INFO`, stdout `{token, expiration_timestamp REQUIRED}`, in-memory cache to expiry checked before every request and every SSE (re)connect, per-attempt 401 re-invoke. `GC_CITY_URL_TOKEN` honored only with an ad-hoc target; conflict-errors against a context credential technique.
- EDIT `internal/api/middleware.go` + the `SupervisorRequestPayload` type: **G9** — server sink for `X-GC-Request-Id` into the `api:` log line + a `RequestId` payload field (additive typed event → regen). Client captures `X-GC-Request-Id` from non-2xx into error strings + the failure target-echo line.

**Ordering safety note:** do **not** enable any remote *read command* until **G1 (no-fallback)** is in place — otherwise a remote read error silently falls back to a local store (the exact hazard the design exists to prevent). Build the plumbing (Phases 1–3), then flip on the read set.

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
