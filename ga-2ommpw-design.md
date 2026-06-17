# Design: Compact-Duration Alert for mol-dog-compactor

**Bead**: ga-2ommpw.1  
**Designer**: gascity/designer  
**Blocks**: ga-2ommpw.2 (build), which blocks ga-ox5oz8.2 (2h interval change)  
**Wireframe**: `ga-2ommpw-compact-alert.excalidraw` / `.png`

---

## Problem

`gc dolt compact` runs as an exec order with no duration visibility. When the flatten phase holds the Dolt DB write lock for > 5 min, agent/convoy writes stall. The operator approval for the 2h compactor interval (ga-ox5oz8) was explicitly conditioned on an alert for this case. No alert currently exists.

---

## Signal Location

**File**: `examples/bd/dolt/commands/compact/run.sh`  
**Hook point**: `main()` — the per-DB dispatch loop, wrapping both `flatten_database` and `bare_gc_database` calls.

This is the right place because:
- It captures total per-DB time (pre-flight + flatten/GC + verify), which is what the operator cares about
- `run.sh` already uses `date +%s` timing in `run_full_gc()` (line ~1397); the same idiom works here
- No new scripts or helpers needed — reuses existing `_notify.sh` / `dolt_escalate()`

The compact command does **not** route through agents or formulas; it runs in-process as an exec order. The alert must fire from the script, not from an external observer.

---

## Threshold

**Env var**: `GC_DOLT_COMPACT_WARN_SECS`  
**Default**: `300` (5 minutes)  
**Floor**: `1` (validated at startup, same pattern as other compact env vars)

The 5-min default is derived from operator approval context (ga-ox5oz8). Operators on high-write cities may tune this down.

---

## Behavior

| Condition | Outcome |
|-----------|---------|
| `elapsed_s < WARN_SECS` | Silent — no escalation |
| `elapsed_s >= WARN_SECS` | `dolt_escalate()` fires [MEDIUM] alert |
| escalation script fails | Warning logged to stderr; compact result is **unaffected** |

`>= WARN_SECS` (not `>`): a run that exactly hits the threshold is considered slow.

No advisory dedup (`advisory_state.sh`) needed — a compact duration event is discrete (one event per DB per run), not a persistent steady-state condition. Each DB that exceeds the threshold gets its own alert.

---

## Operator-Visible Surface

**Mechanism**: `dolt_escalate()` from `_notify.sh`  
Route: `GC_ESCALATE_SCRIPT` search path (gastown → maintenance → bd → core packs).  
Same delivery channel as `mol-dog-doctor.sh`'s [MEDIUM] health advisory.

**Subject**:
```
Dolt compact duration alert [MEDIUM] — db=<name>
```

**Body**:
```
compact for db=<name> took <elapsed>s (warn_secs=<threshold>). A slow compact
may hold the DB write lock and stall agent/convoy writes. city=<GC_CITY_PATH>
```

**Severity**: `[MEDIUM]` — the operator is informed and should investigate, but the compact itself completed. Use `[CRITICAL]` only if the compact is still running (not applicable here since we alert post-completion).

**Required context fields** (must appear in the body):
| Field | Source | Description |
|-------|--------|-------------|
| `db` | loop variable | Name of the affected database |
| `elapsed` | `$(date +%s) - db_start` | Actual compact duration in seconds |
| `warn_secs` | `$compact_warn_secs` | Configured threshold |
| `city` | `$GC_CITY_PATH` | City root for disambiguation |

---

## Builder Contract

### 1. Source `_notify.sh` in `run.sh`

After the existing sourced scripts (lines ~169-172), add:
```sh
. "$PACK_DIR/assets/scripts/_notify.sh"
```

### 2. Parse `GC_DOLT_COMPACT_WARN_SECS`

In the env-setup/validation block (near line ~218 where other compact vars are parsed), add:
```sh
compact_warn_secs="${GC_DOLT_COMPACT_WARN_SECS:-300}"
case "$compact_warn_secs" in
  ''|*[!0-9]*|0)
    printf 'compact: invalid GC_DOLT_COMPACT_WARN_SECS=%s (must be a positive integer)\n' \
      "$compact_warn_secs" >&2
    exit 2
    ;;
esac
```

### 3. Add env var documentation in header comment

In the `# Environment:` block at the top of `run.sh`, add:
```sh
#   GC_DOLT_COMPACT_WARN_SECS
#     (default: 300) — alert threshold in seconds. A compact that takes
#                     longer than this fires a [MEDIUM] escalation. Set to 0
#                     to disable (though 0 is rejected; set a large value
#                     like 86400 to effectively disable).
```

### 4. Wrap the per-DB dispatch loops in `main()`

In the `flatten_database` loop (lines ~2566–2571):
```sh
while IFS= read -r db; do
    [ -n "$db" ] || continue
    _db_compact_start=$(date +%s)
    if ! flatten_database "$db"; then
        failed_count=$((failed_count + 1))
    fi
    _db_compact_elapsed=$(( $(date +%s) - _db_compact_start ))
    if [ "$_db_compact_elapsed" -ge "$compact_warn_secs" ]; then
        dolt_escalate \
            "Dolt compact duration alert [MEDIUM] — db=${db}" \
            "compact for db=${db} took ${_db_compact_elapsed}s (warn_secs=${compact_warn_secs}). A slow compact may hold the DB write lock and stall agent/convoy writes. city=${GC_CITY_PATH}" \
            || printf 'compact: db=%s duration alert escalation failed (elapsed=%ss)\n' "$db" "$_db_compact_elapsed" >&2
    fi
done < "$_unique_db_tmp"
```

Apply the same timing wrapper to the `bare_gc_database` loop (lines ~2551–2557).

### 5. No new files

The alert reuses `_notify.sh` / `dolt_escalate()`. No new scripts, no new state files, no new orders.

---

## Test Contract

**Test file**: `examples/bd/dolt/compact_duration_alert_test.sh`  
Pattern: shell test, same style as `test/dolt/advisory_dedup_test.sh`.

Required test cases:

| Test | Setup | Expected |
|------|-------|---------|
| Fast compact | `GC_DOLT_COMPACT_WARN_SECS=300`, mock compact exits in 0s | No escalation fired |
| Slow compact | `GC_DOLT_COMPACT_WARN_SECS=1`, mock compact sleeps 2s | Escalation fires with correct db/elapsed/warn fields |
| Threshold boundary | `GC_DOLT_COMPACT_WARN_SECS=2`, mock compact takes exactly 2s | Escalation fires (`>=` semantics) |
| Escalation failure | Mock escalate.sh exits 1 | Compact exits 0, warning on stderr |
| Bare GC path | `GC_DOLT_COMPACT_BARE_GC=1`, `GC_DOLT_COMPACT_WARN_SECS=1`, slow mock | Escalation fires for bare GC too |
| Invalid WARN_SECS | `GC_DOLT_COMPACT_WARN_SECS=0` | compact exits 2 (invalid config) |

---

## Dependencies on Existing Patterns

- `dolt_escalate()` — `examples/bd/dolt/assets/scripts/_notify.sh` (already used in `mol-dog-doctor.sh`)
- `date +%s` timing — already used in `run_full_gc()` (no new timing mechanism)
- `advisory_state.sh` — **not used** (one-shot event, not persistent condition)

---

## Dependency Chain

```
ga-2ommpw.1 (this design)
    → ga-2ommpw.2 (build the alert)
        → ga-ox5oz8.2 (change mol-dog-compactor.toml interval 24h → 2h)
```

The 2h interval MUST NOT land before the duration alert is built and merged.

---

## Out of Scope

- The alert fires **after** compact completes. It does not interrupt or abort a running compact.
- No per-table granularity — the alert is per-DB (total time for that DB's flatten + DOLT_GC).
- No dedup across runs — if every 2h run exceeds 5 min, the operator gets an alert each time (intentional: persistent slowness should be visible).
