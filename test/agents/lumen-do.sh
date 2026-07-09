#!/usr/bin/env bash
# Scripted Lumen pool agent (L3 e2e). Proves the SESSION mechanism with no LLM:
#   claim : gc hook --claim --json         (the L1 Tier-B federated leg)
#   read  : .description on the claim JSON (the rendered do prompt — NO bd show)
#   close : gc bd update <id> --set-metadata gc.outcome=pass --status closed
# Raw `bd` NEVER appears here: it cannot see .gc/graph/journal.db (L1 corr. #5).
# Env contract (set by the session spawn, S8): GC_CITY, GC_TEMPLATE,
# GC_SESSION_NAME, PATH including gc.
set -euo pipefail
cd "$GC_CITY"
work_seconds="${GC_LUMEN_E2E_WORK_SECONDS:-3}"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    # Trivial "work" + durable proof the prompt was read FROM THE CLAIM JSON
    # (test evidence, not process state — no PID/status semantics):
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    sleep "$work_seconds"   # hold the claim so the test can observe no-mid-do-drain
    gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do.sh: no work claimed within 60s" >&2
exit 1
