#!/usr/bin/env bash
# Scripted Lumen pool agent (value-plumbing e2e). Proves the do-output convention with
# no LLM: it claims a do, records the prompt it received (from the claim JSON), extracts
# an EMIT=<token> when present, and closes the bead stamping that token as gc.output_json
# — the dispatcher's step-output field a downstream do's {{ref}} consumes. The "produce"
# do carries EMIT=aval; the "consume" do's prompt is "use <produce output>" and carries
# no EMIT, so it closes with an empty output.
#   claim : gc hook --claim --json
#   read  : .description on the claim JSON (the rendered do prompt)
#   close : gc bd update <id> --set-metadata gc.outcome=pass --set-metadata gc.output_json=<tok> --status closed
# Env: GC_CITY, GC_SESSION_NAME, PATH including gc.
set -euo pipefail
cd "$GC_CITY"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    # Durable proof the prompt was read FROM THE CLAIM JSON (the resolved Description).
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    out=""
    if [[ "$prompt" =~ EMIT=([^[:space:]]+) ]]; then
      out="${BASH_REMATCH[1]}"
    fi
    gc bd update "$id" --set-metadata gc.outcome=pass --set-metadata "gc.output_json=${out}" --status closed
    exit 0  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-chain.sh: no work claimed within 60s" >&2
exit 1
