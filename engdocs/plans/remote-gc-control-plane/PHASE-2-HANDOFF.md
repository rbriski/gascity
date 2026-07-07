# Remote-GC — Slice 1 Phase 2 (Group C) Handoff

**Purpose:** hand a fresh session enough to build **Group C — server-side `rig add --git-url` provisioning** without re-deriving the design. The authoritative contract is **`DESIGN-BRIEF.md` v2** (§1 decisions, §3 gates **G12–G23**, §7 server-side design). This doc is the *operational* state: what's done, what's next, the current code seams (confirmed against HEAD), the build order, and the landmines.

> **FRONTIER (2026-07-07): Slice 1 Phase 1 "writes-first" COMPLETE (pushed `e3005cd6c`).** `gc --context prod sling <target> <bead>` mutates a DIRECT hardened city end to end. **NEXT = Group C:** the other capstone mutation — `gc --context prod rig add --git-url <repo> …` — needs real server-side provisioning (clone + beads init + pack compose), behind a pure `internal/rig` extraction, with async `202` + atomic rollback + a `request_id` idempotency state machine. Then the full capstone one-liner + runbook.

**Worktree:** `/data/projects/gascity/.claude/worktrees/gc-remote` (branch `worktree-gc-remote`, on `origin`). Run everything from the worktree root; do **not** `cd` to the main checkout.

---

## 1. First 15 minutes (before writing code)

1. Read **`DESIGN-BRIEF.md`** §1 (Decisions **6** idempotency, **7** rig-add=full-provisioning-retiring-the-parallel-path, **9** async+atomic-rollback), §3 gates **G12–G23**, **§7.2** (`internal/rig` extraction), **§7.3** (the `request_id` state machine — this is spec-first, G13), **§7.4** (async provisioning), §8 (residual risks), §11 (capstone runbook).
2. Read **`HANDOFF.md`** — the frontier note (top) + §8 steps 1–7 as-built (Phase 1 is done; Group C *reuses* its grant transport).
3. Confirm the tree is green:
   ```bash
   go build ./internal/... ./cmd/... && go vet ./internal/api/ ./internal/rig/ 2>/dev/null
   go test ./internal/api/ -run 'Rig|WriteAuth|Grant|Sling' -count=1
   ```
4. Skim the recalled memory `remote-gc-control-plane-design` for the gist.

Do **not** relitigate the 9 decisions. Build to the checklist. **Spec `request_id` (G13) BEFORE implementing it** (Decision).

---

## 2. What is DONE (Phase 1 — do not rebuild)

Slice 0 (remote reads) + Slice 1 Phase 1 (writes) are pushed. Group C **reuses** all of Phase 1's grant transport — you do **not** re-derive the grant path:

- **Grant transport is built and tested.** `internal/citywriteauth` (verify-only; `ReqDigest`, `ReqDigestFromBodyHash`, `AudienceCityWrite`), `internal/clientgrant` (the `GC_GRANT_INFO` exec contract), `cmd/gc-write-mint` (reference ed25519 minter). The **G18 grant editor** in `internal/api/client_remote.go` (`remoteGrantEditor` + `RemoteOptions.Grant`) already mints a request-bound `X-GC-City-Write` on **every mutating genclient request** — so a new `Client.RigCreate` gets the grant **for free**, no extra wiring.
- **Write routing is built.** `cmd/gc/remote_client.go`: `resolveWriteTarget()` + `buildRemoteWriteClient()` (wires the grant from `ctx.GrantCommand`). `cmd/gc/sling_remote.go::cmdSlingRemote` is the **pattern to mirror** for a `cmd/gc/rig_remote.go::cmdRigAddRemote`.
- **Server hardening (G10/G11) is built.** Fail-closed boot (`writeAuthBootGate`, ack knob `write_auth_allow_unverified` / `GC_CITY_WRITE_ALLOW_UNVERIFIED=1`) and `/svc` refusal. The write-auth middleware already gates `POST /v0/city/{n}/rigs` (it's a `cityScopedObjectMutation`), so once a grant rides a remote `rig add`, the server verifies it with **zero new gate code**.
- `Client.Sling` (`internal/api/client.go`) is the **model** for a new `Client.RigCreate` (but rig-create is **async 202 → SSE**, so model the *wait* on `SubmitSession` at `client.go:~1231`, not `Sling`).

---

## 3. Group C — the gates (grounded in current code)

Full **server-side provisioning** so a remote `rig add --git-url` clones, inits beads, composes packs, and registers the rig server-side. Decision 7 requires this be done **once**, retiring the parallel path.

**Confirmed current seams (HEAD):**
- Local provisioning (the real one, to extract): `cmd/gc/cmd_rig.go` — `cmdRigAdd:168` → `doRigAdd:210` → `doRigAddWithResult:215`; `resolveRigAddPath:188` (dot-relative branch stays in `cmd/gc`); existing rollback scaffolding `snapshotRigAddTopologyFiles:877` + `writeRigAddRollbackError:932`.
- Server handler (config-append only today): `internal/api/huma_handlers_rigs.go:67 humaHandleRigCreate` (input `internal/api/huma_types_rigs.go:24 RigCreateInput`, output `RigCreatedOutput`).
- **The third path to RETIRE (Decision 7 / landmine):** `cmd/gc/api_state.go:1493 controllerState.CreateRig` → `:1520 initializeRigStoreForCreate`. Three overlapping rig-add writers today; leaving two = the object-model-at-center violation.
- Git Layer 0 (G15 hooks here): `internal/git/git.go:337 SanitizedEnv`.
- `request_id` precedent to mirror: it's already used for session async ops (`internal/api/event_payloads.go`, `huma_handlers_sessions_command.go`, `huma_types_sessions.go`); `emitAsyncResult` / `EmitRequestFailed` / `recoverAsRequestFailed` exist — reuse them (G20).
- genclient already has `CreateRig`/`CreateRigWithBody` (`internal/api/genclient/client_gen.go:~15160`); no hand-written `Client.RigCreate` yet.

**Gate summary (see `DESIGN-BRIEF.md` §3 for the full text):**

| Gate | What | Anchor |
|---|---|---|
| **G13** | `request_id` state machine, **spec-first** (§7.3): key `(city,request_id)`+body-digest as a bead **label/metadata** (NOT a new bead type — DoltLite `invalid issue type` trap). States: `in_flight`→replay 202+EventCursor; `succeeded`→return ids; `failed+rolled_back`→**purge** so a same-id retry re-clones. Responses 202-new/202-inflight/200-existing/409-mismatch. | brief §7.3 |
| **G12** | Extract **`internal/rig`** (pure mechanical, `sling.SlingDeps` model: inject `fsys.FS`, step/warning callback, `ReloadConfig`, store-init funcs, pack installer). **RETIRE `controllerState.CreateRig`/`initializeRigStoreForCreate`**; collapse two config writers to one comment-preserving append. Both `cmdRigAdd` and `humaHandleRigCreate` delegate. Byte-identical `city.toml`/`packs.lock`/`routes.jsonl` local-vs-API tests + import-guard test. | `cmd_rig.go`, `api_state.go` |
| **G15** | Harden the server git clone in `internal/git` (Layer 0, via `SanitizedEnv`): allowlist `https`(+`ssh`); reject `file://`/`ext::`/bare paths; `protocol.ext.allow=never`, `protocol.file.allow=never`, `GIT_TERMINAL_PROMPT=0`, `GIT_ASKPASS=/bin/false`, `core.hooksPath=/dev/null`, `--no-recurse-submodules` unless opted in; SSRF-filter loopback/link-local/private/metadata IPs **with post-redirect re-check**; never persist embedded `git_url` creds into `city.toml`. | `git/git.go:337` |
| **G14** | Async **`202`** when `git_url` present (keep sync **`201`** for bare config-append). **Atomic rollback in the SERVER orchestration layer around the shared core, never inside `internal/rig`.** Created-vs-preexisting manifest; stage clone in a temp dir under a server-owned root, rename on full success; rollback drops the created rig DB under managed Dolt; sweep orphaned partial dirs at boot. | new server orch |
| **G16** | Per-rig-**name** in-flight lock (mirror `withSourceWorkflowLaunchLock`): concurrent different-`request_id` POSTs for the same rig get 409 + typed body (in-flight `request_id` + `EventCursor`). Run the `request_id` dedupe under this lock; it also serializes `registerCityDoltConfig`/`clearCityDoltConfig`. | brief §7.4 |
| **G17** | Config reload through `mutateAndPoke`/`pokeCh` (**StateMutator**), NOT the CLI `reloadControllerConfig` (a controller dialing its own socket self-deadlocks). Provisioning goroutine stays off the state/config lock. Emit the **SUCCESS event only after** config is visible via `s.state.Config()` **and** `state.BeadStore(rigName)!=nil` (else `gc sling` right after 404s → the one-liner goes flaky). | brief §7.4 |
| **G20** | Typed async events: `RequestOperationRigCreate` + `RequestResultRigCreate`/`RequestFailed` carrying `request_id` (+ rig name, prefix/branch) and `rig.provision.progress {step: clone\|beads-init\|packs\|config}`. Terminal failure MUST be `events.RequestFailed`. Register payloads (`TestEveryKnownEventTypeHasRegisteredPayload` fails the build otherwise). | `event_payloads.go` |
| **G21** | Rig-create **wait** (client): its own **heartbeat-anchored, watchdog-bounded** deadline (NOT the 4-min `sessionMessageTimeout` — a WAN clone exceeds it). Pull a **minimal reconnect** into Slice 1 for this wait only (`Seq` on `sseEnvelope`, resume `after_seq` from the 202 `EventCursor`, per-attempt 401 re-mint). On unrecoverable failure print `request_id` + a resume recipe. | model on `SubmitSession` |
| **G22** | Model **both** response shapes in Huma/OpenAPI (`201` config-only, `202`+`EventCursor`); regen `openapi.json` + `genclient` + dashboard TS **in the same commit** (`go run ./cmd/genspec` + `go generate ./internal/api/genclient` + `make dashboard-check`). Add `request_id` to `SlingInput` **and** `RigCreateInput`. Byte-identical local output. Client-side 1 MiB body pre-check. | spec discipline |
| **G23** | Capstone runbook (§11) + the full one-liner: `gc --context prod rig add --git-url … && gc --context prod sling <target> <adopt-pr>` against a direct hardened city, observed via `gc --context prod events --follow`. | brief §11 |

**Client half (mirrors Phase 1's sling routing, reuses the grant path):** a `Client.RigCreate` (async: POST → 202 `{request_id, EventCursor}` → wait via reconnecting SSE, G21) + a `cmd/gc/rig_remote.go::cmdRigAddRemote` branch in `cmdRigAdd` (mirror `cmdSlingRemote`: resolve `resolveWriteTarget`, refuse local-only modes, forward `--git-url`/name/prefix/branch, render). The grant rides automatically (G18).

---

## 4. Build order (each phase ≤5 files, TDD, `go test`+`go vet` green before the next)

1. **C1 — G13 spec.** Write the `request_id` state-machine spec into `DESIGN-BRIEF.md` §7.3 / a spec file **before** code. Pin the four responses + the bead-label storage shape. (Decision: spec-first.)
2. **C2 — G12 `internal/rig` extraction.** Pure mechanical: move `doRigAddWithResult`'s provisioning into `internal/rig` (deps injected), **retire `controllerState.CreateRig`/`initializeRigStoreForCreate`**, collapse the config writers. Both callers delegate. Byte-identical artifact tests + import-guard test (mirror the worker-boundary guard). **This is the upstream-candidate; keep the diff clean.**
3. **C3 — G15 git hardening.** `internal/git` Layer 0: URL allowlist + SSRF filter (+ post-redirect re-check) + hardened clone env. Table-test `ext::`/`file://`/metadata-IP rejection.
4. **C4 — G14/G16/G17 server orchestration.** Async `202`, temp-dir-stage-then-rename, atomic rollback (dir + Dolt DB + config) in the server layer, per-rig-name lock, StateMutator reload with success-after-visible. Boot-sweep orphaned partials.
5. **C5 — G20 typed events.** Register the rig-create operation/result/failed + progress payloads; reuse `emitAsyncResult`/`EmitRequestFailed`.
6. **C6 — G22 wire shapes + regen.** Both response shapes in Huma; `request_id` on `SlingInput` + `RigCreateInput`; regen spec + genclient + dashboard TS in one commit; `TestOpenAPISpecInSync` + `make dashboard-check` green.
7. **C7 — G21 client + `cmd/gc` rig-add remote routing.** `Client.RigCreate` (async wait, heartbeat + minimal reconnect) + `cmd/gc/rig_remote.go` (mirror `cmdSlingRemote`).
8. **C8 — G23 capstone.** The full one-liner E2E against a real hardened city (or an `httptest` harness) + the runbook.

---

## 5. Landmines (do not step on these)

1. **`request_id` is a `(city,request_id)`+body-digest state machine (G13)** or Decisions 6 & 9 contradict (phantom rig id after rollback / a retry that can never re-clone). Store as a bead **label/metadata**, NOT a new bead type (DoltLite `invalid issue type` trap).
2. **`internal/rig` extraction MUST retire `controllerState.CreateRig`** (`cmd/gc/api_state.go:1493`). There are **three** rig-add paths today; leaving two is the exact violation Decision 7 exists to prevent.
3. **`git_url` clone is an RCE/SSRF primitive** (G15): `ext::` RCE, `file://` exfil, metadata-IP SSRF, hooks/submodules. Harden in `internal/git` Layer 0, and **re-check the IP after every redirect**.
4. **Rollback lives in the SERVER orchestration layer, never inside `internal/rig`** (G14) — `internal/rig` returns typed errors/step events; the server decides created-vs-preexisting and unwinds.
5. **Config reload via StateMutator (`mutateAndPoke`), NOT the CLI reloader** (G17) — a controller dialing its own control socket mid-request self-deadlocks. Emit SUCCESS **only after** `s.state.Config()` shows the rig **and** its bead store is registered, or `gc sling` immediately after 404s and the one-liner goes flaky.
6. **Rig-add wait needs its OWN heartbeat-anchored deadline** (G21), not the 4-min `sessionMessageTimeout` — a WAN clone routinely exceeds it.
7. **Keep local output byte-identical** (G22): `internal/rig` returns typed errors/step events; `cmd/gc` renders the exact current strings. Warn-and-continue stays warnings → streamed warning events.

---

## 6. Conventions & gotchas (learned in Phase 1 — save yourself the round-trips)

- **The pre-push hook runs the full `test-local-parallel fast` suite (~5 min)** and gates the push. Budget for it. `internal/eventfeed::TestMuxSource_YieldsAndPicksUpNewCity` is a **load-induced flake** (passes in isolation) — retry the push, don't touch its timeout.
- **Every new test directory needs an untagged `testenv_import_test.go`** (`go run scripts/add-testenv-import.go`) or `TestRequiresDedicatedTestenvImportFile` reds the push.
- **Adding any config field regenerates the config schema** → run `go run ./cmd/genschema` (or docsync reds). Adding/altering wire types → `go run ./cmd/genspec` + `go generate ./internal/api/genclient` + `make dashboard-check`, all in the same commit (`TestOpenAPISpecInSync`).
- **TDD**, test next to code, `t.TempDir()` for fs. Integration tests (real infra, or that build a binary) use `//go:build integration`.
- **Build cache:** NEVER `go clean -cache`. Cold build → `GOCACHE=$(mktemp -d) go build ./cmd/gc/`. `go clean -testcache` is fine.
- **Git:** gascity Dolt is LOCAL-ONLY — never `bd dolt push/pull/remote`. Use `git push` only. Never bare `git stash` (shared stack). Commit/push only when asked.
- **Adversarial-review each server phase** with a Workflow (crypto/exec-env for G15, idempotency/rollback for G13/G14, spec-conformance) — it caught 4 real defects across Phase 1.

---

## 7. Pointers

- Contract: `DESIGN-BRIEF.md` (v2). Operational state: `HANDOFF.md`. This doc: Group C pickup.
- Phase-1 grant transport to reuse: `internal/api/client_remote.go` (`remoteGrantEditor`, `RemoteOptions.Grant`), `cmd/gc/remote_client.go` (`resolveWriteTarget`, `buildRemoteWriteClient`), `cmd/gc/sling_remote.go` (`cmdSlingRemote` — the routing pattern to mirror), `internal/api/client.go` (`Client.Sling` shape; `SubmitSession` for the async-wait shape).
- Rig-add seams: `cmd/gc/cmd_rig.go` (`doRigAddWithResult:215`), `internal/api/huma_handlers_rigs.go:67`, `internal/api/huma_types_rigs.go:24`, `cmd/gc/api_state.go:1493` (retire), `internal/git/git.go:337`.
- Async/event precedent: `internal/api/event_payloads.go`, `huma_handlers_sessions_command.go`, `emitAsyncResult`/`EmitRequestFailed`/`recoverAsRequestFailed`.
