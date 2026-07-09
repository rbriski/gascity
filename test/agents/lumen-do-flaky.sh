#!/usr/bin/env bash
# Scripted Lumen pool agent (L5 retry e2e). Same claim→read→close loop as
# lumen-do.sh, but its CLOSE outcome depends on the attempt number: the FIRST
# claim across all sessions closes gc.outcome=fail, every subsequent claim closes
# gc.outcome=pass. This drives a `repeat lane: … until lane.outcome == pass || …`
# loop to re-attempt once and then seal pass, WITH NO LLM and NO control beads.
#
# Attempts run in SEPARATE pooled sessions (one do per subprocess session), so the
# attempt counter must persist on disk across sessions — a flag file in .gc, which
# is safe because attempts are strictly sequential (the driver mints attempt N+1
# only after N settles). Env contract (set by the session spawn): GC_CITY,
# GC_TEMPLATE, GC_SESSION_NAME, PATH including gc.
set -euo pipefail
cd "$GC_CITY"
work_seconds="${GC_LUMEN_E2E_WORK_SECONDS:-1}"
flag=".gc/lumen-e2e-attempt-count"
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
      gc bd update "$id" --set-metadata gc.outcome=fail --status closed
    else
      gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    fi
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-flaky.sh: no work claimed within 60s" >&2
exit 1
