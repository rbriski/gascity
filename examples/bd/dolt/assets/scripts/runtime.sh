#!/bin/sh

: "${GC_CITY_PATH:?GC_CITY_PATH must be set}"

CITY_RUNTIME_DIR="${GC_CITY_RUNTIME_DIR:-$GC_CITY_PATH/.gc/runtime}"
PACK_STATE_DIR="${GC_PACK_STATE_DIR:-$CITY_RUNTIME_DIR/packs/dolt}"
LEGACY_GC_DIR="$GC_CITY_PATH/.gc"

if [ -d "$PACK_STATE_DIR" ] || [ ! -d "$LEGACY_GC_DIR/dolt-data" ]; then
  DOLT_STATE_DIR="$PACK_STATE_DIR"
else
  DOLT_STATE_DIR="$LEGACY_GC_DIR"
fi

# Data lives under .beads/dolt (gc-beads-bd canonical path). Honor
# GC_DOLT_DATA_DIR first so shell pack commands target the same managed data
# directory as the Go lifecycle and doctor code.
DOLT_BEADS_DATA_DIR="${GC_DOLT_DATA_DIR:-$GC_CITY_PATH/.beads/dolt}"
if [ -n "${GC_DOLT_DATA_DIR:-}" ]; then
  DOLT_DATA_DIR="$GC_DOLT_DATA_DIR"
elif [ -d "$DOLT_BEADS_DATA_DIR" ]; then
  DOLT_DATA_DIR="$DOLT_BEADS_DATA_DIR"
else
  DOLT_DATA_DIR="$DOLT_STATE_DIR/dolt-data"
fi

DOLT_LOG_FILE="${GC_DOLT_LOG_FILE:-$DOLT_STATE_DIR/dolt.log}"
DOLT_PID_FILE="${GC_DOLT_PID_FILE:-$DOLT_STATE_DIR/dolt.pid}"
if [ -n "${GC_DOLT_STATE_FILE:-}" ]; then
  DOLT_STATE_FILE="$GC_DOLT_STATE_FILE"
else
  DOLT_STATE_FILE="$DOLT_STATE_DIR/dolt-state.json"
fi
DOLT_PROVIDER_STATE_FILE="$DOLT_STATE_DIR/dolt-provider-state.json"

GC_BEADS_BD_SCRIPT="$GC_CITY_PATH/.gc/scripts/gc-beads-bd.sh"

# is_local_dolt_host returns 0 (true) when the argument names the local managed
# Dolt server — loopback, the unspecified address, or an unset/empty host — and
# 1 (false) for a configured external endpoint. The health, status, and logs
# commands share it so they agree on whether GC owns a local managed process or
# is merely pointed at a remote server it cannot inspect on-disk. Mirrors the
# gc-beads-bd `is_remote` classification (gastownhall/gascity su-deol8).
is_local_dolt_host() {
  case "$1" in
    ""|127.0.0.1|0.0.0.0|localhost|::1|"[::1]") return 0 ;;
    *) return 1 ;;
  esac
}

read_runtime_state_flag() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  value=$(sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([^,}[:space:]]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true)
  case "$value" in
    true|false)
      printf '%s\n' "$value"
      ;;
  esac
)

read_runtime_state_number() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\\([0-9][0-9]*\\).*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

read_runtime_state_string() (
  state_file="$1"
  key="$2"
  [ -f "$state_file" ] || return 0
  sed -n "s/.*\"$key\"[[:space:]]*:[[:space:]]*\"\\([^\"]*\\)\".*/\\1/p" "$state_file" 2>/dev/null | head -1 || true
)

canonical_path() (
  path="$1"
  if command -v python3 >/dev/null 2>&1; then
    python3 - "$path" <<'PY'
import os
import sys

print(os.path.realpath(sys.argv[1]))
PY
    return $?
  fi
  if command -v readlink >/dev/null 2>&1; then
    readlink -f "$path" 2>/dev/null && return 0
  fi
  printf '%s\n' "$path"
)

same_path() (
  left="$1"
  right="$2"
  [ "$left" = "$right" ] && return 0
  [ "$(canonical_path "$left")" = "$(canonical_path "$right")" ]
)

pid_is_running() (
  pid="$1"

  case "$pid" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if kill -0 "$pid" 2>/dev/null; then
    return 0
  fi

  if command -v ps >/dev/null 2>&1; then
    ps_pid=$(ps -p "$pid" -o pid= 2>/dev/null | tr -d '[:space:]')
    [ "$ps_pid" = "$pid" ] && return 0
  fi

  return 1
)

managed_runtime_listener_pid() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 0
      ;;
  esac

  if ! command -v lsof >/dev/null 2>&1; then
    return 0
  fi

  lsof -nP -t -iTCP:"$port" -sTCP:LISTEN 2>/dev/null \
    | while IFS= read -r holder_pid; do
        case "$holder_pid" in
          ''|*[!0-9]*)
            continue
            ;;
        esac
        if pid_is_running "$holder_pid"; then
          printf '%s\n' "$holder_pid"
          break
        fi
      done
)

managed_runtime_tcp_reachable() (
  port="$1"

  case "$port" in
    ''|*[!0-9]*)
      return 1
      ;;
  esac

  if command -v nc >/dev/null 2>&1; then
    nc -z 127.0.0.1 "$port" >/dev/null 2>&1
    return $?
  fi

  if command -v python3 >/dev/null 2>&1; then
    python3 - "$port" <<'PY' >/dev/null 2>&1
import socket
import sys

sock = socket.socket()
sock.settimeout(0.25)
try:
    sock.connect(("127.0.0.1", int(sys.argv[1])))
except OSError:
    raise SystemExit(1)
finally:
    sock.close()
PY
    return $?
  fi

  return 1
)

managed_runtime_port() (
  state_file="$1"
  expected_data_dir="$2"

  [ -f "$state_file" ] || return 0

  running=$(read_runtime_state_flag "$state_file" running)
  pid=$(read_runtime_state_number "$state_file" pid)
  port=$(read_runtime_state_number "$state_file" port)
  data_dir=$(read_runtime_state_string "$state_file" data_dir)

  [ "$running" = "true" ] || return 0
  [ -n "$pid" ] || return 0
  [ -n "$port" ] || return 0
  if ! same_path "$data_dir" "$expected_data_dir"; then
    printf 'dolt runtime: managed state data_dir=%s does not match expected data_dir=%s\n' \
      "$data_dir" "$expected_data_dir" >&2
    return 0
  fi
  pid_is_running "$pid" || return 0

  holder_pid=$(managed_runtime_listener_pid "$port" || true)
  if [ -n "$holder_pid" ]; then
    [ "$holder_pid" = "$pid" ] || return 0
    printf '%s\n' "$port"
    return 0
  fi

  if ! managed_runtime_tcp_reachable "$port"; then
    return 0
  fi

  printf '%s\n' "$port"
)

# Resolve GC_DOLT_PORT. The shared helper prefers validated live managed
# runtime state over stale inherited env, then falls back to GC_DOLT_PORT as an
# operator seed, and exits 78 if neither yields a port.
. "${GC_PACK_DIR:-${PACK_DIR:-${GC_SYSTEM_PACKS_DIR:-$GC_CITY_PATH/.gc/system/packs}/dolt}}/assets/scripts/port_resolve.sh"
GC_DOLT_PORT=$(resolve_dolt_port_or_die "$DOLT_STATE_FILE" "$DOLT_PROVIDER_STATE_FILE" "$DOLT_DATA_DIR" "$GC_CITY_PATH") || exit $?

# Resolve a bounded-execution helper. Prefer gtimeout (coreutils on
# macOS), fall back to timeout (coreutils on Linux), then to running
# the command directly if neither is installed. Running unbounded is
# still better than letting a wedged dolt client hang the caller, but
# patrol callers need a hard upper bound wherever possible.
if command -v gtimeout >/dev/null 2>&1; then
  TIMEOUT_BIN="gtimeout"
elif command -v timeout >/dev/null 2>&1; then
  TIMEOUT_BIN="timeout"
else
  TIMEOUT_BIN=""
fi

_run_bounded_warned_no_timeout=""

# Wall-clock bound (seconds) for `gc rig list --json` rig discovery, shared
# by the compact and health commands and tunable via
# GC_DOLT_RIG_LIST_TIMEOUT_SECS. The bound must absorb a slow-but-healthy gc
# on a busy host (~16s observed): discovery callers degrade to a city-only
# filesystem scan on timeout, which silently drops external rig databases
# (gascity#2740).
GC_DOLT_RIG_LIST_TIMEOUT_SECS="${GC_DOLT_RIG_LIST_TIMEOUT_SECS:-30}"

# run_bounded SECS CMD...  — Run CMD with a wall-clock timeout. Exits
# 124 on timeout (coreutils convention). Uses --kill-after=2 so an
# uncooperative child that ignores SIGTERM (e.g. a dolt client stuck
# in kernel socket wait) is escalated to SIGKILL rather than leaking
# zombies — which is the failure mode the bounded helper exists to
# prevent. If no bounded execution mechanism is available, fail closed rather
# than running a potentially wedged Dolt client unbounded.
run_bounded() {
  _t="$1"; shift
  if [ -n "$TIMEOUT_BIN" ]; then
    "$TIMEOUT_BIN" --kill-after=2 "$_t" "$@"
  elif command -v python3 >/dev/null 2>&1; then
    python3 - "$_t" "$@" <<'PY'
import subprocess
import sys

limit = float(sys.argv[1])
cmd = sys.argv[2:]
try:
    proc = subprocess.run(cmd, capture_output=True, text=True, timeout=limit)
except subprocess.TimeoutExpired as exc:
    sys.stdout.write(exc.stdout or "")
    sys.stderr.write(exc.stderr or "")
    sys.exit(124)
sys.stdout.write(proc.stdout)
sys.stderr.write(proc.stderr)
sys.exit(proc.returncode)
PY
  else
    printf 'dolt runtime: timeout/gtimeout/python3 not found; cannot run bounded command\n' >&2
    return 124
  fi
}

# sql_escape_literal VALUE — emit VALUE escaped for embedding inside a
# single-quoted SQL string literal (backslashes doubled, then single
# quotes doubled). Used by dolt_kill_stale_queries to build an exact-match
# WHERE clause from an arbitrary previously-issued query string.
sql_escape_literal() {
  printf '%s' "$1" | sed "s/\\\\/\\\\\\\\/g; s/'/''/g"
}

# dolt_kill_stale_queries HOST PORT USER QUERY — best-effort reclaim of
# server-side work orphaned by a run_bounded timeout. run_bounded's
# --kill-after only terminates the client CLI process; on Dolt 2.1.10 the
# server-side CALL (DOLT_FETCH/DOLT_PUSH/DOLT_GC/...) keeps running to
# completion because nothing tells the server the client gave up, which
# left DOLT_FETCH/DOLT_GC calls accumulating for hours (gascity ga-tyg).
# This looks up the live query by exact text match against
# information_schema.processlist and issues KILL QUERY for each match,
# freeing the server-side connection slot and CPU/IO the orphaned call
# was consuming.
#
# Scanning and killing are each bounded by GC_DOLT_KILL_SCAN_TIMEOUT_SECS
# (default 10s) so a wedged server cannot hang this cleanup step itself.
# Failures here are logged, not fatal — the caller already has its own
# timeout error to report.
dolt_kill_stale_queries() {
  _dksq_host="$1"; _dksq_port="$2"; _dksq_user="$3"; _dksq_query="$4"
  _dksq_scan_timeout="${GC_DOLT_KILL_SCAN_TIMEOUT_SECS:-10}"
  _dksq_escaped=$(sql_escape_literal "$_dksq_query")
  _dksq_ids=$(run_bounded "$_dksq_scan_timeout" \
    dolt --host "$_dksq_host" --port "$_dksq_port" --user "$_dksq_user" --no-tls \
    sql --result-format csv -q "SELECT id FROM information_schema.processlist WHERE info = '$_dksq_escaped'" 2>/dev/null) || {
    printf 'dolt runtime: WARN: could not scan processlist to reclaim timed-out query\n' >&2
    return 1
  }
  printf '%s\n' "$_dksq_ids" | tail -n +2 | while IFS= read -r _dksq_id; do
    _dksq_id=$(printf '%s' "$_dksq_id" | tr -dc '0-9')
    [ -n "$_dksq_id" ] || continue
    printf 'dolt runtime: client timed out; killing orphaned server-side query id=%s\n' "$_dksq_id" >&2
    run_bounded "$_dksq_scan_timeout" \
      dolt --host "$_dksq_host" --port "$_dksq_port" --user "$_dksq_user" --no-tls \
      sql -q "KILL QUERY $_dksq_id" >/dev/null 2>&1 || true
  done
}

# dolt_maintenance_lock_key HOST PORT — emit the sanitized lock-file basename
# (no extension) that `gc dolt compact` and `gc dolt sync` both use to
# serialize maintenance operations against the same managed Dolt server.
# Concurrent compaction (open transactions, graph-rewrite) and sync pushes
# (DOLT_FETCH/DOLT_PUSH) racing on one server is part of the failure mode
# behind the leaked-call incident (gascity ga-tyg): both commands must
# contend on the identical lock file for a given host:port, so this
# normalization has to be shared verbatim rather than reimplemented per
# script. Loopback aliases (empty, localhost, 0.0.0.0, ::, ::1, 127.x.x.x)
# collapse to 127.0.0.1 — over-serializing local endpoints is safer than
# letting two maintenance ops interleave on one local runtime.
dolt_maintenance_lock_key() {
  _dmlk_port="$2"
  _dmlk_host=$(printf '%s' "$1" | tr '[:upper:]' '[:lower:]' | sed 's/^\[\(.*\)\]$/\1/')
  case "$_dmlk_host" in
    ''|localhost|0.0.0.0|::1|::)
      _dmlk_host="127.0.0.1"
      ;;
    127.*.*.*)
      _dmlk_ip=$_dmlk_host
      IFS=.
      # NOTE: `set --` below overwrites the function's own positional
      # params ($1/$2), which is why the port was saved to _dmlk_port
      # before this point rather than read from $2 further down.
      set -- $_dmlk_ip
      unset IFS
      if [ "$#" -eq 4 ] && [ "$1" = "127" ]; then
        _dmlk_valid=1
        for _dmlk_octet in "$2" "$3" "$4"; do
          case "$_dmlk_octet" in
            ''|*[!0-9]*) _dmlk_valid=0 ;;
            *) [ "$_dmlk_octet" -le 255 ] || _dmlk_valid=0 ;;
          esac
        done
        [ "$_dmlk_valid" = 1 ] && _dmlk_host="127.0.0.1"
      fi
      ;;
  esac
  printf '%s-%s' "$_dmlk_host" "$_dmlk_port" | tr -c 'A-Za-z0-9_.-' '-'
}
