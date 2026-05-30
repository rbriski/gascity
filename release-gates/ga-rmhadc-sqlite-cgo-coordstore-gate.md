# Release gate: ga-rmhadc

**Bead:** ga-rmhadc - needs-deploy: feat(coordstore): opt-in SQLite-cgo bead store backend (default off)  
**Source review bead:** ga-hx7ap3  
**PR:** #2738 - https://github.com/gastownhall/gascity/pull/2738  
**Branch:** `builder/ga-aec8q.16-sqlite-cutover`  
**Code HEAD before gate:** `af686065dd25431077dcab891aef668d3518d298`  
**Base:** `origin/main` at `fa150384f`  
**Verdict:** **PASS**

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | `ga-hx7ap3` is closed with reviewer PASS for PR #2738; deploy bead records reviewed + PASSED evidence and the builder rebase verification. |
| 2 | Acceptance criteria met | PASS | SQLite-cgo bead store backend is build-tagged and default-off, backend selection is explicit, existing Dolt behavior remains unchanged without opt-in, and parity/durability/retention coverage is present. |
| 3 | Tests pass | PASS | See "Test runs" below. |
| 4 | No high-severity review findings open | PASS | Review notes list LOW/INFO findings only; unresolved HIGH count is 0. |
| 5 | Final branch is clean | PASS | `git status --short --branch` is clean before writing this gate file. |
| 6 | Branch diverges cleanly from main | PASS | `git merge-tree --write-tree origin/main HEAD` exits 0 and writes tree `5ccb75462df030a4068aa3fe65b6fdf90be3e0f8`. |
| 7 | Single feature theme | PASS | The branch is large, but the scope is one feature theme: a default-off coordination-store backend with supporting import/parity/health/config/docs surfaces. Reviewer called the bundling cohesive with bisectability risk only informational. |

## Test Runs

```
$ GOTOOLCHAIN=auto go test ./internal/beads -run 'TestFileStore|TestBdStore|TestHQStore|TestSQLite|Test.*Tier' -count=1
ok  	github.com/gastownhall/gascity/internal/beads	0.687s

$ GOTOOLCHAIN=auto go test ./internal/mail/... -count=1
ok  	github.com/gastownhall/gascity/internal/mail	0.003s
ok  	github.com/gastownhall/gascity/internal/mail/beadmail	0.008s
ok  	github.com/gastownhall/gascity/internal/mail/exec	21.938s
?   	github.com/gastownhall/gascity/internal/mail/mailtest	[no test files]

$ GC_FAST_UNIT=0 GOTOOLCHAIN=auto go test ./cmd/gc -run TestCmdMailInbox -count=1
ok  	github.com/gastownhall/gascity/cmd/gc	46.642s

$ GOTOOLCHAIN=auto go test -tags sqlite_cgo ./internal/beads ./cmd/gc -count=1
ok  	github.com/gastownhall/gascity/internal/beads	4.238s
ok  	github.com/gastownhall/gascity/cmd/gc	296.847s

$ GOTOOLCHAIN=auto go vet ./...
(clean)

$ GOTOOLCHAIN=auto make test-fast-parallel
All fast jobs passed
```

## Review Notes

- PR #2738 already exists; this gate updates that PR branch rather than opening a duplicate.
- GitHub reported `mergeable=MERGEABLE`, `mergeStateStatus=UNSTABLE` before the gate commit. CI will rerun after this gate commit lands.
- The backend remains default-off. Runtime behavior is unchanged unless an instance explicitly opts in.
