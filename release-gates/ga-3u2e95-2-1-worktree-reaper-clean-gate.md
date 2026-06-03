# Release Gate: ga-3u2e95.2.1

Bead: ga-3u2e95.2.1 - Deploy corrected worktree reaper split unit with generated artifacts
Branch: release/ga-3u2e95-2-1-worktree-reaper-clean
Base: origin/main @ fb0df821cb764d65f2b71611781e8c812a70c6c4
Head before gate commit: 5bfeb60b44c079b15c194f878dcd026afb9c38a8

## Source Context

- Architecture decision: ga-4q6sgc.1, Unit A - worktree reaper typed events, config, runtime gate, tests, and event emission.
- Review beads:
  - ga-o0ehhl PASS for typed worktree reaper events at `6a6a4c964`.
  - ga-n0oafq re-review PASS through `8e421922b`.
  - ga-6zlyhy PASS for follow-up commits `9836428c0` and `74a87442b`.
- Prior deploy bead ga-3u2e95.2 failed because generated API/config artifacts were outside the original whitelist; this remediation explicitly allows those generated paths.
- `docs/PROJECT_MANIFEST.md` is not present on this branch; this gate uses the active deployer criteria plus the bead acceptance criteria.

## Gate Criteria

| # | Criterion | Verdict | Evidence |
|---|-----------|---------|----------|
| 1 | Review PASS present | PASS | Closed review beads `ga-o0ehhl`, `ga-n0oafq`, and `ga-6zlyhy` contain PASS verdicts covering the five cherry-picked commits. |
| 2 | Acceptance criteria met | PASS | Fresh branch cut from current `origin/main`; cherry-picked only `6a6a4c964`, `6b7ed6be9`, `8e421922b`, `9836428c0`, and `74a87442b` in that order; diff is limited to allowed reaper, config, OpenAPI, schema, and generated dashboard/client paths. |
| 3 | Tests pass | PASS | `go build ./...`, focused reaper/config/API tests, `make test-fast-parallel`, `go vet ./...`, `make dashboard-check`, and dashboard preview smoke all passed. |
| 4 | No high-severity review findings open | PASS | Reviews report PASS; remaining notes are non-blocking style/test suggestions only. |
| 5 | Final branch is clean | PASS | Before writing this gate file, `git status --short --branch` showed only `## release/ga-3u2e95-2-1-worktree-reaper-clean...origin/main [ahead 5]`. Re-check after committing this file is required before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` succeeded and produced tree `6b2a89809e17401606e6c57548a5ad77fc8200d0`. |
| 7 | Single feature theme | PASS | Commit set is one feature: controller-driven closed-bead worktree auto-reaping with typed events, config enablement, runtime wiring, generated API/schema surfaces, and tests. No coordstore RSS guard, SQLite cleanup, `internal/benchmarks/`, `internal/beads/`, `go.mod`, or `go.sum` changes are present. |

## Acceptance Evidence

Cherry-picks:

| Source SHA | New SHA | Purpose |
|------------|---------|---------|
| 6a6a4c964 | 5c594f5d4 | Typed registered worktree reaper events and generated API/client/schema updates. |
| 6b7ed6be9 | 600271abf | Daemon config field, generated config docs/schema, reaper implementation, runtime call site. |
| 8e421922b | 0a12f0f35 | Reaper helper tests and config accessor tests. |
| 9836428c0 | 8f4db4630 | Runtime gate tests for closed-bead worktree reaping. |
| 74a87442b | 5bfeb60b4 | Event bus emission from `reapClosedBeadWorktrees`. |

Conflict resolution:

```text
internal/config/config.go conflict resolved by keeping main's existing DaemonConfig fields,
including the stage/prune surfaces, and adding AutoReapClosedBeadWorktrees
immediately after AutoRestartOnDrift. AutoReapClosedBeadWorktreesEnabled()
was placed immediately after AutoRestartOnDriftEnabled().
```

Allowed diff paths present:

```text
cmd/gc/bead_worktree_reaper.go
cmd/gc/bead_worktree_reaper_test.go
cmd/gc/city_runtime.go
cmd/gc/city_runtime_bead_worktree_reap_test.go
cmd/gc/dashboard/web/src/generated/index.ts
cmd/gc/dashboard/web/src/generated/schema.d.ts
cmd/gc/dashboard/web/src/generated/types.gen.ts
docs/reference/config.md
docs/schema/city-schema.json
docs/schema/city-schema.txt
docs/schema/openapi.json
docs/schema/openapi.txt
internal/api/genclient/client_gen.go
internal/api/openapi.json
internal/config/config.go
internal/config/config_test.go
internal/events/events.go
internal/events/payloads.go
```

Forbidden paths absent:

```text
internal/benchmarks/
internal/beads/
go.mod
go.sum
```

## Test Evidence

Automated gates:

```text
go build ./...
go test ./cmd/gc/ -run TestAutoReap -count=1
go test ./cmd/gc/ -run "TestCityRuntimeTick_.*ClosedBeadWorktreeReap|TestExtractBeadIDFromWorktreeName|TestIsStrictlyUnderDir" -count=1
go test ./internal/config -run TestDaemonAutoReapClosedBeadWorktrees -count=1
go test ./internal/api -run "TestOpenAPISpecInSync|TestEveryKnownEventTypeHasRegisteredPayload|TestTypedEventEnvelopeUnionsCoverKnownEventTypes" -count=1
make test-fast-parallel
go vet ./...
make dashboard-check
npm run preview -- --host 127.0.0.1 --port 4178
curl -fsS http://127.0.0.1:4178/
curl -fsSI http://127.0.0.1:4178/dashboard.js
```

Results:

```text
go build ./... after conflict resolution: PASS
go build ./... after all cherry-picks: PASS
literal TestAutoReap command: PASS, but matched no tests
focused cmd/gc reaper tests: ok github.com/gastownhall/gascity/cmd/gc 0.301s
focused internal/config auto-reap tests: ok github.com/gastownhall/gascity/internal/config 0.021s
focused internal/api invariant tests: ok github.com/gastownhall/gascity/internal/api 0.135s
make test-fast-parallel: All fast jobs passed
go vet ./...: PASS
make dashboard-check: PASS
dashboard preview: `/` served HTML; `/dashboard.js` returned HTTP 200
```

The dashboard preview process listening on port 4178 was stopped after the smoke.
