#!/usr/bin/env bash
# oneshot-e2e-demo.sh — true end-to-end demo of running a formula to completion
# in a TRANSIENT, isolated city, with a real provider in the loop.
#
# It manufactures a throwaway Dolt-backed city that never touches the shared
# machine supervisor (init --no-start), runs the STANDALONE controller against
# it (gc start --controller), spawns a real subprocess worker + control-
# dispatcher, slings the minimal core formula mol-do-work, and drives it until
# the workflow root closes with gc.outcome=pass — then stops and reaps the city.
#
# This is the manual, proven recipe the in-process `gc run` execution slice
# productizes. It uses only the standalone controller, so it is safe to run on a
# host that also runs other cities: it never registers with the supervisor.
#
# Usage: GCBIN=/path/to/gc bash oneshot-e2e-demo.sh
set -uo pipefail

GCBIN="${GCBIN:-gc}"
DEMO_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
WORKER="$DEMO_DIR/self-close-worker.sh"   # minimal, deterministic self-closing worker
CITY="$(mktemp -d)/city"
CTRL_PID=""

cleanup() {
  [ -n "$CTRL_PID" ] && "$GCBIN" stop --city "$CITY" >/dev/null 2>&1
  [ -n "$CTRL_PID" ] && kill "$CTRL_PID" 2>/dev/null
  pkill -f "$CITY/.beads/dolt" 2>/dev/null
  rm -rf "$(dirname "$CITY")"
}
trap cleanup EXIT

fail() { echo "DEMO FAILED: $*" >&2; exit 1; }

CONFIG="$(mktemp)"
cat > "$CONFIG" <<EOF
[workspace]
name = "oneshot-demo"

[session]
provider = "subprocess"

[daemon]
formula_v2 = true
patrol_interval = "100ms"

[[agent]]
name = "worker"
max_active_sessions = 1
start_command = "bash $WORKER"

[[named_session]]
template = "worker"
mode = "always"
EOF

echo "1/5 manufacture isolated Dolt city (no supervisor)…"
"$GCBIN" init --no-start --skip-provider-readiness --file "$CONFIG" "$CITY" >/dev/null 2>&1 \
  || fail "init --no-start"
"$GCBIN" cities 2>/dev/null | grep -q "$CITY" && fail "city was registered with the supervisor (should be isolated)"

echo "2/5 start the STANDALONE controller…"
"$GCBIN" start --controller --city "$CITY" >/dev/null 2>&1 &
CTRL_PID=$!
for _ in $(seq 1 30); do
  [ -S "$CITY/.gc/controller.sock" ] && "$GCBIN" --city "$CITY" bd ready --json >/dev/null 2>&1 && break
  sleep 3
done

echo "3/5 sling mol-do-work (1-member convoy)…"
bead=$("$GCBIN" --city "$CITY" bd create "demo one-shot work item" --json 2>/dev/null | jq -r '.id')
convoy=$("$GCBIN" --city "$CITY" convoy create "demo one-shot" "$bead" --json 2>/dev/null | jq -r '.id')
slout=$("$GCBIN" --city "$CITY" sling worker "$convoy" --on=mol-do-work 2>&1)
root=$(printf '%s' "$slout" | grep -oE 'workflow [a-z]+-[a-z0-9]+' | head -1 | awk '{print $2}')
[ -n "$root" ] || fail "no workflow root in: $slout"
echo "   workflow root: $root"
"$GCBIN" --city "$CITY" convoy dispatch >/dev/null 2>&1 || true   # nudge the control lane

echo "4/5 drive to completion…"
outcome=""
for i in $(seq 1 45); do
  j=$("$GCBIN" --city "$CITY" bd show "$root" --json 2>/dev/null)
  st=$(printf '%s' "$j" | jq -r '.[0].status // empty')
  if [ "$st" = "closed" ]; then outcome=$(printf '%s' "$j" | jq -r '.[0].metadata["gc.outcome"] // empty'); break; fi
  sleep 4
done
[ "$outcome" = "pass" ] || fail "root $root did not reach gc.outcome=pass (status=$st outcome=$outcome)"

echo "5/5 SUCCESS: workflow root $root closed with gc.outcome=pass"
echo "    (city will be stopped and reaped on exit; supervisor untouched)"
