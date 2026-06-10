# Release gate: ga-gvtotn emergency relay

Status: PASS
Date: 2026-06-10

## Scope

- Deploy bead: ga-gvtotn - needs-deploy: emergency relay + event constants + API schema
- Source review bead: ga-s18qvw - Review: PR #3281 emergency relay (ga-rle1j4.2)
- PR: https://github.com/gastownhall/gascity/pull/3281
- Branch: builder/ga-rle1j4.2-emergency-relay
- Reviewed commit: 6fdc7a84c7cdfe6c7eaf74d28537fb12d984c2f4
- Base checked: origin/main@4d9c7272e7cd80c649dc07526b54f51746996ceb

## Criteria

| # | Criterion | Result | Evidence |
|---|-----------|--------|----------|
| 1 | Review PASS present | PASS | Review bead ga-s18qvw is closed with `REVIEWER VERDICT: PASS`. Notes cite PR #3281 at commit 6fdc7a84c7cdfe6c7eaf74d28537fb12d984c2f4 and all CI checks clean at review time. |
| 2 | Acceptance criteria met | PASS | `internal/emergency` adds `Record`, `NewRecord`, `ValidSeverity`, `WriteSpool`, event recording helpers, and notification dedupe. `cmd/gc/api_state_emergency_relay.go` drains `emergencyCh` into `emergency.RecordSignaled`; `cmd/gc/controller.go` creates the buffered channel and starts the relay. |
| 3 | Tests pass | PASS | `make test-fast-parallel` passed all fast jobs. `go vet ./...` passed. `make dashboard-check` passed codegen, Vite build, TypeScript typecheck, and `go test ./cmd/gc/dashboard/...`. Dashboard preview served `HTTP/1.1 200 OK` on `127.0.0.1:42781`. |
| 4 | No high-severity review findings open | PASS | Review notes list one MEDIUM finding for missing `internal/emergency` unit tests, tracked by follow-up bead ga-guopsu. No HIGH findings are listed. |
| 5 | Final branch is clean | PASS | Worktree was clean before writing this gate. Gate file is committed as the final branch tip; post-commit status must remain clean before push. |
| 6 | Branch diverges cleanly from main | PASS | Branch was 1 ahead / 1 behind `origin/main`; `git merge-tree $(git merge-base HEAD origin/main) origin/main HEAD` found no conflict markers. |
| 7 | Single feature theme | PASS | The commit set is one emergency-event feature: spool record writing, controller relay plumbing, event constants/payload registration, and generated API/dashboard schema for those event types. |

## Acceptance evidence

- Emergency spool behavior lives in `internal/emergency/emergency.go`: records validate severity and message size, default actor to `human`, generate timestamped IDs, write JSON under `.gc/emergency/`, enforce `0o700` directory and `0o600` file permissions, and commit temp files through same-directory hard links.
- Event types are declared in `internal/events/events.go` as `emergency.signaled` and `emergency.acked`, included in `KnownEventTypes`, and registered with `events.RegisterPayload` in `internal/emergency` init.
- `internal/api/event_payloads.go` blank-imports `internal/emergency` so API payload registry coverage sees the emergency event payloads.
- `cmd/gc/api_state.go` owns `emergencyCh`; `cmd/gc/api_state_emergency_relay.go` relays channel records to the event provider as `emergency.signaled`; `cmd/gc/controller.go` wires the channel and relay at controller startup.
- Generated API outputs include `TypedEventStreamEnvelopeEmergencyAcked`, `TypedEventStreamEnvelopeEmergencySignaled`, `TypedTaggedEventStreamEnvelopeEmergencyAcked`, and `TypedTaggedEventStreamEnvelopeEmergencySignaled` in `internal/api/openapi.json`, `docs/schema/openapi.json`, and dashboard generated TypeScript.

## Test log

```text
make test-fast-parallel
All fast jobs passed

go vet ./...
PASS

make dashboard-check
openapi-ts generation: PASS
openapi-typescript schema generation: PASS
vite build: PASS
tsc --noEmit: PASS
go test ./cmd/gc/dashboard/...: PASS

npm run preview -- --host 127.0.0.1 --port 42781
curl -fsSI http://127.0.0.1:42781/
HTTP/1.1 200 OK
```

## Notes

- Reviewer-identified MEDIUM test coverage gap is deferred to ga-guopsu; it is not a gate blocker under the release criteria because there are no unresolved HIGH findings and the feature-specific checks passed.
- Post-gate GitHub checks must be green on the gate commit before the deploy bead is closed and the merge-request is routed.
