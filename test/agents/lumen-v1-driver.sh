#!/usr/bin/env bash
# Scripted Lumen v1 DRIVER-LOOP agent (agent-driven stepper e2e). Proves the v1
# self-drive shape with no LLM: it claims the ONE run-bead, reads the run stream id
# off the claim JSON description (RUN=<streamID>, the EMIT= convention), then drives
# the run turn by turn IN THIS SESSION via the two thin stepper verbs:
#   step   : gc lumen step   --run <run> --json      -> {"done":..,"node":..,"prompt":..}
#   work   : extract EMIT=<token> from the printed prompt (the do's output)
#   settle : gc lumen settle --run <run> --node <id> --outcome pass --output <token> --json
# It loops until step reports done, then closes the run-bead with the aggregated
# gc.outcome. Raw `bd` NEVER appears here; the journal writes go through `gc lumen`.
# ZERO role names. Env: GC_CITY, GC_SESSION_NAME, PATH including gc, jq.
set -euo pipefail
cd "$GC_CITY"
# GC_LUMEN_V1_STEP_SLEEP holds a deterministic pause (seconds) AFTER each settle so the
# kill/adopt leg can SIGKILL a driver session between two turns; 0 (default) = no pause.
step_sleep="${GC_LUMEN_V1_STEP_SLEEP:-0}"
deadline=$((SECONDS + 120))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" != "work" ]; then
    sleep 0.2
    continue
  fi
  bead_id="$(printf '%s' "$claim" | jq -r '.bead_id')"
  desc="$(printf '%s' "$claim" | jq -r '.description // empty')"
  run=""
  if [[ "$desc" =~ RUN=([^[:space:]]+) ]]; then
    run="${BASH_REMATCH[1]}"
  fi
  if [ -z "$run" ]; then
    echo "lumen-v1-driver.sh: driver bead carries no RUN=<streamID>" >&2
    exit 1
  fi

  # Drive the run turn by turn until step reports done.
  step="$(gc lumen step --run "$run" --json)"
  while [ "$(printf '%s' "$step" | jq -r '.done')" != "true" ]; do
    node="$(printf '%s' "$step" | jq -r '.node')"
    prompt="$(printf '%s' "$step" | jq -r '.prompt // empty')"
    # "Work": the do's output is the EMIT=<token> in its prompt (value-plumbing proof).
    out=""
    if [[ "$prompt" =~ EMIT=([^[:space:]]+) ]]; then
      out="${BASH_REMATCH[1]}"
    fi
    # Durable proof the prompt was read from the stepper, keyed by node id.
    printf '%s\n' "$prompt" > ".gc/lumen-v1-prompt-${node}.txt"
    step="$(gc lumen settle --run "$run" --node "$node" --outcome pass --output "$out" --json)"
    # Deterministic between-turn pause for the kill/adopt leg (0 by default).
    if [ "$step_sleep" != "0" ]; then
      sleep "$step_sleep"
    fi
  done

  agg="$(printf '%s' "$step" | jq -r '.outcome // "pass"')"
  gc_outcome="pass"
  if [ "$agg" != "pass" ]; then
    gc_outcome="fail"
  fi
  gc bd update "$bead_id" --set-metadata "gc.outcome=${gc_outcome}" --status closed
  exit 0
done
echo "lumen-v1-driver.sh: no run-bead claimed within 120s" >&2
exit 1
