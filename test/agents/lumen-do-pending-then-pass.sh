#!/usr/bin/env bash
# Scripted Lumen pool agent (pending non-consuming repeat e2e). Same claim→read→close
# loop as lumen-do-flaky.sh, but its CLOSE outcome models a CI-repair check that POLLS:
# the FIRST claim across all sessions closes gc.outcome=pending (the check's CI is still
# running — re-poll WITHOUT burning the repeat budget), every subsequent claim closes
# gc.outcome=pass (the repair passed). This drives a
# `repeat lane: … until lane.outcome == pass || iteration >= 1` loop to POLL once
# (non-consuming) and then seal pass, WITH NO LLM and NO control beads.
#
# Attempts run in SEPARATE pooled sessions (one do per subprocess session), so the
# attempt counter must persist on disk across sessions — a flag file in .gc, which is
# safe because attempts are strictly sequential (the driver mints attempt N+1 only after
# N settles). Env contract (set by the session spawn): GC_CITY, GC_TEMPLATE,
# GC_SESSION_NAME, PATH including gc.
set -euo pipefail
cd "$GC_CITY"
work_seconds="${GC_LUMEN_E2E_WORK_SECONDS:-1}"
flag=".gc/lumen-e2e-pending-count"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    # Attempt counter across sessions (sequential attempts, no concurrent writers).
    count=0
    [ -f "$flag" ] && count="$(cat "$flag")"
    count=$((count + 1))
    printf '%s' "$count" > "$flag"
    # Durable per-attempt proof the prompt was read FROM THE CLAIM JSON.
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}-attempt-${count}.txt"
    sleep "$work_seconds"   # hold the claim briefly so the test can observe the claim
    if [ "$count" -eq 1 ]; then
      gc bd update "$id" --set-metadata gc.outcome=pending --status closed
    else
      gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    fi
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-pending-then-pass.sh: no work claimed within 60s" >&2
exit 1
