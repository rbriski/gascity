# Release Gate: ga-boh06c - SQLite-cgo coordstore backend

Deploy bead: ga-boh06c
Source review bead: ga-6z82f1
Earlier deploy bead: ga-rmhadc
Branch: builder/ga-aec8q.16-sqlite-cutover
PR: https://github.com/gastownhall/gascity/pull/2738
Reviewed commit: 39116bd6a4bdf24addfec8575a09aff921199dfa
Gate evaluated: 2026-05-31

Note: `docs/PROJECT_MANIFEST.md` is not present in this checkout. This gate
uses the release criteria from the deployer prompt loaded by `gc prime`, with
test scope aligned to `TESTING.md` and the current PR #2738 review notes.

## Summary

This change ships an opt-in SQLite-cgo coordination-store backend for beads.
The existing Dolt-backed path remains the default; the new backend is only
reachable from a build that includes `-tags sqlite_cgo` and an explicit
`[beads] provider = "sqlite"` configuration.

The PR also carries the supporting provider selection, store-health path
delegation, import/shadow CLI commands, parity and durability tests, benchmark
adapters, and documentation needed to evaluate the backend before any live
city opts into it.

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `bd show ga-6z82f1` records `Review Verdict: PASS` from `gascity/reviewer` for PR #2738 at commit `39116bd6a4bdf24addfec8575a09aff921199dfa`. The earlier deploy gate `ga-rmhadc` is also closed with PASS for the same PR lineage. |
| 2 | Acceptance criteria met | PASS | `internal/beads/sqlite_cgo_store.go` implements the Store interface behind `sqlite_cgo && cgo`; `internal/beads/sqlite_cgo_store_stub.go` preserves default-build behavior with a clear missing-tag error; provider selection accepts `sqlite`/`sqlite-cgo` without changing the default; store-health delegates through `StoreHealthPath`; import/shadow CLI surfaces and schema docs are present; tagged and untagged tests cover the new paths. |
| 3 | Tests pass | PASS | `git diff --check origin/main...HEAD` exited 0; `go test ./test/docsync -run TestSchemaFreshness -count=1` passed; focused `internal/beads` suite passed; `go test -tags sqlite_cgo ./internal/beads ./cmd/gc -count=1` passed; `go vet ./...` exited 0; `make test-fast-parallel` completed with `All fast jobs passed`; required GitHub PR checks for #2738 are green at the reviewed head. |
| 4 | No high-severity review findings open | PASS | `ga-6z82f1` review notes record no security issues and only minor non-blocking observations. The earlier `ga-hx7ap3` findings were LOW/INFORMATIONAL and are not HIGH. |
| 5 | Final branch is clean | PASS | `git status --short --branch` was clean at detached PR head before writing this gate file; cleanliness is rechecked after committing this gate before push. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree HEAD origin/main` exited 0 at reviewed head and wrote tree `8491ee09815a4406a387f612da5851977967b3af`. |
| 7 | Single feature theme | PASS | The branch is large, but it has one feature theme: a default-off SQLite-cgo coordination-store backend with the tests, provider wiring, health/import tooling, benchmark adapters, and docs needed to validate that backend. The reviewer called the bundling cohesive and raised only bisectability as informational. |

## Acceptance Evidence

- `SQLiteCGOStore` provides the production Store implementation behind the
  `sqlite_cgo` build tag; the untagged stub fails safely with an explicit tag
  hint.
- `cmd/gc` provider selection keeps existing `bd`, `file`, `exec`, and
  `hqstore` paths unchanged while routing explicit `sqlite`/`sqlite-cgo`
  requests to the new backend.
- Store health reads the backend path through `StoreHealthPath`, so HQStore and
  SQLite-cgo stores report their actual storage locations.
- Coordination-store import and shadow-diff commands are included so operators
  can compare data before any live opt-in.
- Schema freshness and generated config docs are current for the new provider
  value.

## Commands

```text
gh auth status
git worktree add --detach /tmp/gascity-deploy-ga-boh06c origin/builder/ga-aec8q.16-sqlite-cutover
git status --short --branch
git diff --check origin/main...HEAD
git merge-tree --write-tree HEAD origin/main
GOTOOLCHAIN=auto go test ./test/docsync -run TestSchemaFreshness -count=1
GOTOOLCHAIN=auto go test ./internal/beads -run 'TestFileStore|TestBdStore|TestHQStore|TestSQLite|Test.*Tier' -count=1
GOTOOLCHAIN=auto go test -tags sqlite_cgo ./internal/beads ./cmd/gc -count=1
GOTOOLCHAIN=auto go vet ./...
GOTOOLCHAIN=auto make test-fast-parallel
gh pr checks 2738 --required
```

## Test Summary

```text
go test ./test/docsync -run TestSchemaFreshness -count=1
ok  	github.com/gastownhall/gascity/test/docsync	1.620s

go test ./internal/beads -run 'TestFileStore|TestBdStore|TestHQStore|TestSQLite|Test.*Tier' -count=1
ok  	github.com/gastownhall/gascity/internal/beads	1.242s

go test -tags sqlite_cgo ./internal/beads ./cmd/gc -count=1
ok  	github.com/gastownhall/gascity/internal/beads	5.326s
ok  	github.com/gastownhall/gascity/cmd/gc	287.934s

go vet ./...
clean

make test-fast-parallel
All fast jobs passed
```
