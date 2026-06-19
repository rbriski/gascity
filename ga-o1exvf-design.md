# Design: `gc init` Managed-Dolt Pre-flight Readiness Wait (Track C)

**Bead:** ga-o1exvf  
**Designer:** gascity/designer  
**Date:** 2026-06-19  
**Source architecture:** ga-v3i9mi

---

## Diagram

![gc init pre-flight wait sequence diagram](/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-o1exvf-dolt-preflight.png)

Source: `/home/jaword/projects/gc-management/.gc/worktrees/gascity/designer/ga-o1exvf-dolt-preflight.excalidraw`

---

## User flow

### Root cause

`gc init` starts the Dolt server process and immediately attempts a connection. When Dolt hasn't finished binding its TCP port, the connection is refused. The current test helper (`initCityWithManagedDoltRecovery`, `test/integration/helpers_test.go:117`) retries twice with a `gc start` fallback — adding 25-30s when the race triggers.

### Target user experience

`gc init` should be idempotent against the Dolt startup race. From the caller's perspective:

**Before:**
```
$ gc init --file config.toml cityDir
# <may fail, then caller retries, then gc start fallback, then 25s wasted>
exit 0  # eventually
```

**After:**
```
$ gc init --file config.toml cityDir
# waits internally (usually 50-200ms) for Dolt to bind its port
exit 0  # clean, single attempt
```

The caller (`initCityWithManagedDoltRecovery` in the test helper) should simplify to a single `gc init` call.

---

## Component design

### Pre-flight poll inside `gc init` (managed Dolt mode)

The change is localized to `gc init`'s managed-Dolt startup path. No changes to the `gc start` command or other callers.

**Execution sequence:**

1. Start Dolt server process (existing).
2. Read the port from `.beads/dolt-server.port` (written by Dolt on startup) — poll for this file's existence first if it does not yet exist (up to 2s, 50ms poll interval).
3. Open TCP connection to `127.0.0.1:<port>` with a short timeout (100ms).
4. If connection refused: wait (exponential backoff, see schedule below), retry.
5. If connection succeeds: close it immediately (readiness probe only), proceed with city init.
6. If 10s total elapsed without a successful connection: exit 1 with message to stderr.

**Backoff schedule:**

| Attempt | Wait before next try | Cumulative |
|---------|---------------------|------------|
| 1       | 50ms                | 50ms       |
| 2       | 100ms               | 150ms      |
| 3       | 200ms               | 350ms      |
| 4       | 400ms               | 750ms      |
| 5       | 800ms               | 1550ms     |
| 6       | 1600ms              | 3150ms     |
| 7       | 3200ms              | 6350ms     |
| 8       | 3650ms (cap at 10s) | 10000ms    |

Total: 8 retries, max 10s. Typical Dolt startup (locally, unloaded): 50-300ms → resolves on attempt 1 or 2.

**Port discovery:**

Dolt writes its port to `.beads/dolt-server.port` after binding. If `gc init` starts polling before this file exists:
- Poll for the file with 50ms interval, up to 2s total.
- If still missing after 2s: check whether the Dolt process has exited (using `cmd.ProcessState`). If exited: report the process exit code and stderr — do not enter the TCP poll loop.
- If the file appears: read the port and enter the TCP poll loop.

This file-before-port approach avoids hardcoding a port number and handles the case where `gc init` is faster than Dolt's port write.

**Partial-state prevention:**

If the 10s timeout fires:
1. Kill the Dolt process.
2. Remove any partially-written city files (the managed-Dolt init writes city tables after Dolt is confirmed ready, so there should be no partial city state to clean — only the Dolt process and its data directory).
3. Exit 1.

Do NOT leave behind a `.beads/` directory or `city.toml` with partial data. The caller must be able to retry `gc init` after a clean failure.

---

## Accessibility / CLI UX

1. **Progress feedback during wait:** the poll loop is silent by default. If `--verbose` is passed (or if the wait exceeds 1s), print to stderr: `gc init: waiting for Dolt to be ready (attempt N/8)...`. Do not print anything for fast paths (< 1s total).

2. **Exit code contract:** exit 0 = city initialized and ready. Exit 1 = initialization failed (Dolt did not start, timeout, or process died). No new exit codes; existing callers relying on `if gc init ...; then` work unchanged.

3. **Error message quality for timeout:**
   ```
   gc init: managed Dolt server did not become ready within 10s
   gc init: last connection attempt: dial tcp 127.0.0.1:28231: connect: connection refused
   gc init: Dolt process (pid 12345) is still running — consider increasing --dolt-start-timeout
   gc init: partial state removed; retry gc init to try again
   ```
   The error must tell the operator: what happened, the last network error, whether Dolt is still alive, and what to do next.

4. **Error message for process death before ready:**
   ```
   gc init: Dolt server process exited early (exit code 1)
   gc init: stderr: FATAL unable to start server: address already in use
   gc init: city initialization aborted
   ```
   Include Dolt's stderr output. This is the most common real failure case (port conflict).

5. **`--dolt-start-timeout` flag (optional, future):** the 10s cap is hardcoded for this scope. Add a flag in a follow-on bead if operators on slow machines need to override it. Do not add it preemptively.

---

## Test helper simplification

After `gc init` handles the race internally, `initCityWithManagedDoltRecovery` in `test/integration/helpers_test.go:117` can simplify:

**Before:**
```go
func initCityWithManagedDoltRecovery(t *testing.T, ...) {
    t.Helper()
    err := runGCInit(...)
    if err != nil {
        t.Logf("gc init failed (%v), retrying with gc start fallback", err)
        // attempt 2 ...
        // gc start fallback ...
        // wait for managed Dolt city ready ...
    }
    waitForManagedDoltCityReady(t, cityPath, 30*time.Second)
}
```

**After:**
```go
func initCityWithManagedDoltRecovery(t *testing.T, ...) {
    t.Helper()
    require.NoError(t, runGCInit(...))
    // gc init now handles the Dolt startup race internally
}
```

The `waitForManagedDoltCityReady` call can be dropped entirely — `gc init` exits only when Dolt is confirmed ready. The `initCityWithManagedDoltRecovery` function can be renamed `initCity` or inlined at callsites.

---

## Edge cases and error handling

| Scenario | Behavior |
|----------|----------|
| Dolt starts immediately (< 50ms) | First TCP probe succeeds; zero extra wait |
| Dolt starts slowly (1-3s) | Backoff: resolves at attempt 4-5; total ~750ms-1550ms wait |
| Dolt takes >10s | Timeout fires → kill process → remove partial state → exit 1 |
| Dolt process dies before port file written | File-poll detects process exit → report stderr → exit 1 |
| Dolt process dies after port file written but before TCP ready | TCP probe gets ECONNREFUSED → process-liveness check detects exit → report exit code → exit 1 |
| Port file written but wrong port | TCP probe to wrong port → ECONNREFUSED → timeout → exit 1 (this is a Dolt bug, not gc init bug) |
| Two concurrent `gc init` calls on same cityDir | Both start Dolt → port conflict → one Dolt exits → that gc init detects process exit and exits 1 → caller retries or errors | 
| Non-managed Dolt city (no `--managed-dolt`) | Pre-flight poll is skipped entirely — existing path unchanged |

---

## Implementation file targets

| File | Change |
|------|--------|
| `cmd/gc/cmd_init.go` (or similar) | Add `waitForDoltReady(ctx, cityPath, port, 10*time.Second)` call after starting Dolt process |
| New helper: `internal/doltauth/readiness.go` (or new `internal/doltprobe/`) | `WaitForDoltReady(ctx, port string, timeout time.Duration) error` — exponential backoff TCP probe |
| `test/integration/helpers_test.go:117` | Simplify `initCityWithManagedDoltRecovery` to single `gc init` call |

Keep the readiness probe in a dedicated package (not inlined in `cmd/gc/`) so it can be tested independently without invoking the full CLI. The builder should place it in `internal/doltauth/` if it fits there logically, or create a new `internal/doltprobe/` package.

---

## Success criteria

- `gc init` in managed-Dolt mode never returns before Dolt is ready to accept connections
- The race that caused `initCityWithManagedDoltRecovery` retries does not recur (verify with 50× repeated test runs)
- `gc init` exits 1 with a clear error if Dolt does not become ready within 10s
- No partial city state is left behind on failure
- `initCityWithManagedDoltRecovery` simplifies to a single `gc init` call (test helper refactor is part of done-when)
- `TestPersonalWorkFormulaCompileAndRun` loses the ~25s Dolt-init tail (before/after timing comparison in the PR)
