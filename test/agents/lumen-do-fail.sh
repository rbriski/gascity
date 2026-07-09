#!/usr/bin/env bash
# Scripted Lumen pool agent (L4 skip-cascade e2e). The lumen-do.sh claim→read→close
# body, but the close writes gc.outcome=fail — the WORKER verdict that fails a do and
# skip-cascades its dependents. A sibling script, NOT a mutation of lumen-do.sh
# (L3/L5 keep their agent byte-identical). Raw `bd` NEVER appears (L1 corr. #5).
# Env: GC_CITY, GC_SESSION_NAME, PATH incl gc; GC_LUMEN_E2E_WORK_SECONDS.
set -euo pipefail
cd "$GC_CITY"
work_seconds="${GC_LUMEN_E2E_WORK_SECONDS:-1}"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    sleep "$work_seconds"   # hold the claim briefly so the test can observe the admit
    gc bd update "$id" --set-metadata gc.outcome=fail --status closed
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-fail.sh: no work claimed within 60s" >&2
exit 1
