#!/usr/bin/env bash
# self-close-worker.sh — a minimal, LLM-free worker for the one-shot e2e demo.
#
# It polls the city's ready queue for a work step bead routed to it, claims it,
# and closes it with gc.outcome=pass. It never touches control beads (the
# control-dispatcher owns those). This is the deterministic "real provider in
# the loop" worker: a real subprocess that closes beads exactly as an LLM agent
# would, so the workflow drives itself to finalize.
set -uo pipefail

CITY="${GC_CITY:-${GC_CITY_PATH:-$PWD}}"
cd "$CITY" 2>/dev/null || true
export BEADS_DIR="$CITY/.beads"
export BEADS_ACTOR="${GC_SESSION_NAME:-${GC_AGENT:-worker}}"

# Control-bead kinds the control-dispatcher owns — never closed by a worker.
CONTROL='^(workflow|scope|workflow-finalize|scope-check|check|fanout|retry|retry-eval|drain|ralph)$'

for _ in $(seq 1 600); do
  ready=$(bd ready --json --limit=0 2>/dev/null || echo '[]')
  id=$(printf '%s' "$ready" | jq -r --arg ck "$CONTROL" '
        (if type=="array" then . else [] end)
        | map(select((.metadata["gc.kind"] // "") | test($ck) | not))
        | map(select((.metadata["gc.routed_to"] // "") == "worker"))
        | .[0].id // ""' 2>/dev/null || echo "")
  if [ -n "$id" ] && [ "$id" != "null" ]; then
    bd update "$id" --claim >/dev/null 2>&1 || true
    bd update "$id" --set-metadata gc.outcome=pass --status closed >/dev/null 2>&1 || true
  fi
  sleep 0.3
done
