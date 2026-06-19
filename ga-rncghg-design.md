# Design: Nudge-on-Work-Ready Dispatch + `gc hook --wait` (Tracks A+B)

**Bead:** ga-rncghg  
**Designer:** gascity/designer  
**Date:** 2026-06-19  
**Source architecture:** ga-v3i9mi

---

## Diagram

![Dispatch flow sequence diagram](/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-rncghg-dispatch-flow.png)

Source: `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-rncghg-dispatch-flow.excalidraw`

---

## User flow

The core experience this design must deliver is: **a worker that is idle discovers new work within ~100ms of it becoming ready**, instead of the current ~2.7s poll gap.

### Before fix (polling)

```
Controller closes step N
  ↓ (100ms patrol)
step N+1 transitions to open
  ↓ (worker is mid-sleep or mid-gc-hook call)
worker's next gc hook fires (up to 2.7s later)
  ↓ (2.5s binary startup)
gc hook finds step N+1
  ↓ (0.4s execute)
step N+1 closes
```

Each step: ~5.5s (2.7s miss + 2.5s hook + 0.4s work).  
16 steps: ~88s dispatch overhead.

### After fix (Track A + B)

```
Controller closes step N
  ↓ (100ms patrol)
step N+1 transitions to open
  ↓ dispatchReadyWorkNudges() queries delta
Controller enqueues nudge + pings wake socket (~10ms)
  ↓ (<10ms unix socket signal)
Worker unblocks from gc hook --wait 5s
  ↓ (2.5s gc hook startup — unavoidable in this scope)
gc hook finds step N+1
  ↓ (0.4s execute)
step N+1 closes
```

Each step: ~3.2s (10ms signal + 2.5s hook + 0.4s work + 0.3s close).  
16 steps: ~52s — a 40% reduction.

---

## Component choices

### Track A — `dispatchReadyWorkNudges()` in `cmd/gc/city_runtime.go`

**Placement:** call on every patrol tick, immediately after the existing `dispatchReadyWaitNudges()` call (line ~2329). These two functions are parallel by design — same shape, different bead type.

**Query:** open work beads (`gc.kind=work`) with `assignee` set to a live session name.

**Delta tracking (critical design choice):** maintain an in-memory set `lastReadyWorkBeads map[string]struct{}` inside the patrol goroutine. On each tick:
1. Query current ready set.
2. Diff against `lastReadyWorkBeads`.
3. For each newly-ready bead (in current but not previous): enqueue nudge + ping socket.
4. Update `lastReadyWorkBeads = currentReadySet`.

**Why in-memory, not persisted:** persisting adds a write to every patrol tick and creates a new failure mode (stale state on restart). On city restart, the set is empty; every open assigned bead appears newly-ready → at most one extra nudge per step. The patrol fallback recovers within 100ms.

**Nudge message:** `"New work ready: <bead-id>"` — short, specific, actionable. The worker can optionally parse the bead-id and skip the work-query step, though the current design doesn't require it.

**Supervisor mode vs legacy mode:** In supervisor mode, the nudge is delivered via the unix socket within ~100ms. In legacy (per-session poller) mode, delivery is within ~2s (unchanged). The `--wait` flag (Track B) targets supervisor mode.

### Track B — `--wait <duration>` flag in `cmd/gc/cmd_hook.go`

**Flag:** `--wait` takes a duration string (e.g. `5s`, `30s`). Default: no flag = existing behavior unchanged. Backward compatible.

**Execution order (critical):**
1. Parse `--wait` flag.
2. **Run work query first** (before blocking). If work found: return exit 0 immediately.
3. If no work: open `nudgequeue.WakeSocketPath(cityPath)` for read.
4. Block until: byte received on socket OR timeout expires.
5. On wake/timeout: run work query once more.
6. Return exit 0 (work found) or 1 (still empty).

**Why check-before-block:** a bead may have become ready between the last `gc hook` call and the new `--wait` call. Without the pre-check, the worker blocks until the *next* step finishes — adding a full ~3s gap. With pre-check, no gap.

**Spurious wakes:** the wake socket is city-wide (broadcast). A worker that wakes and finds no work simply re-enters `gc hook --wait`. Cost: one extra 2.5s hook call per spurious wake. At 2 workers with 16 steps: at most 32 spurious wakes total. Acceptable.

**Test stub change** (`test/agents/graph-dispatch.sh`):
```bash
# Before:
if ready=$(fetch_ready_queue); then ...
else
    sleep 0.2
    continue
fi

# After:
if ready=$(gc hook --wait 5s); then ...
```

The `sleep 0.2` is removed. The blocking wait is inside `gc hook --wait`. No behavior change for the happy path.

---

## Accessibility / CLI UX

This is a backend/infrastructure change with no visual UI component. CLI UX concerns:

1. **`gc hook --wait` flag discoverability:** The flag must appear in `gc hook --help` output with a clear description. Suggested: `--wait duration   Block until work is ready or timeout expires (e.g. 5s, 30s). Runs work query on wake. Default: exit immediately.`

2. **Exit codes preserve existing contract:** exit 0 = work found (and clamed), exit 1 = no work. Adding `--wait` does not change this contract. A caller that checks `$?` behaves identically.

3. **Error messages for socket failure:** if `gc hook --wait` cannot open the wake socket (missing city, bad path), it must print a clear error to stderr and fall back to the regular work query (no blocking). It must not hang indefinitely on socket open.

4. **Timeout feedback:** when `--wait 5s` times out with no work, no output to stdout (same as today's exit 1). A verbose flag (`--verbose`) could print "timed out waiting for work" but is not required for this scope.

5. **Observability:** the `dispatchReadyWorkNudges()` function should emit an event `system.nudge.work_ready_dispatched` with fields `{bead_id, session, delta_count}` for each nudge batch. This makes the dispatch visible in `gc events` and allows operators to verify the nudge path is working without reading logs.

---

## Error handling / edge cases

| Scenario | Behavior | Design note |
|----------|----------|-------------|
| Wake socket does not exist | `gc hook --wait` falls back to immediate query | Socket may not exist if city is in non-supervisor mode |
| Two workers wake simultaneously | Both call `gc hook --wait`, both query, first to `bd update --claim` wins (CAS), other gets empty and re-enters wait | CAS is already atomic — no additional protection needed |
| City restarts mid-dispatch | `lastReadyWorkBeads` resets to empty; next patrol tick emits nudges for all currently-open assigned beads | At most one extra nudge per open step; benign |
| Nudge dropped (socket full or timing race) | Patrol tick (100ms) fires and `dispatchReadyWorkNudges()` picks up the bead on the next iteration | Patrol fallback is the guarantee; socket is best-effort |
| `--wait` flag used without supervisor mode | Block until timeout on socket that has no writers → `gc hook --wait 5s` behaves like `sleep 5s && gc hook` | Degraded but not broken. Docs should note supervisor mode is required for socket-based wakeup |
| `gc hook --wait` called before socket path is known | Must handle missing `cityPath` gracefully — emit stderr warning and proceed without blocking | City detection should reuse existing path-resolution logic |

---

## Guardrails for builder

1. **`dispatchReadyWorkNudges()` is additive:** the patrol tick must not remove or replace existing dispatch logic. Only add the function call after `dispatchReadyWaitNudges()`.

2. **Delta set is in-memory only:** no file writes, no new JSON keys in `nudgequeue.State`.

3. **`gc hook --wait` checks for work before blocking** (see execution order above). The pre-check is load-bearing — do not omit it.

4. **Single wake socket per city:** do not introduce per-session sockets. The city-wide socket is sufficient.

5. **Patrol fallback must remain:** do not add a guard that skips `dispatchReadyWorkNudges()` when the wake socket is confirmed working. The fallback is the reliability guarantee.

6. **Test stub change is part of done-when:** the fix is incomplete without updating `test/agents/graph-dispatch.sh`. Both changes must land together.

7. **Revert #3590/#3593:** the done-when criteria include reverting the timeout bumps. This is builder scope, not a separate design question.

---

## Implementation file targets

| File | Change |
|------|--------|
| `cmd/gc/city_runtime.go` | Add `dispatchReadyWorkNudges()` call on patrol tick (after line ~2329 `dispatchReadyWaitNudges()`) |
| `cmd/gc/cmd_wait.go` | Reference for pattern — `dispatchReadyWaitNudges()` is the template |
| `cmd/gc/cmd_hook.go` | Add `--wait <duration>` flag with check-before-block logic |
| `test/agents/graph-dispatch.sh` | Replace `sleep 0.2` idle branch with `gc hook --wait 5s` |
| `internal/events/` | Register `system.nudge.work_ready_dispatched` event type |

---

## Success criteria

- `TestPersonalWorkFormulaCompileAndRun` completes in ≤60s locally (target: ≤52s)
- Per-step dispatch gap in logs drops from ~2.7s to ~0.1-0.3s (observable via event timestamps)
- Patrol fallback still delivers nudges if socket is unavailable (verify with socket-absent test)
- `gc hook --wait` with no supervisor running exits cleanly after timeout (not hang)
