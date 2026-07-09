#!/usr/bin/env bash
# Scripted Lumen pool agent (L4 concurrency e2e). Same claim→read→close contract as
# lumen-do.sh, but instead of a fixed work sleep it BARRIERS on the count of
# concurrent claimants so the do's are provably in flight at once (all admits precede
# all settles in the sealed journal) and their closes fire near-simultaneously — the
# §0.2 concurrent-close stress. The barrier DEGRADES (proceeds after its own deadline)
# so a slow spawn never deadlocks a straggler past the firewall grace. Claim markers
# are test-evidence files, not process state. Raw `bd` NEVER appears (L1 corr. #5).
# Env: GC_CITY, GC_SESSION_NAME, PATH incl gc; GC_LUMEN_E2E_BARRIER (expected count).
set -euo pipefail
cd "$GC_CITY"
barrier="${GC_LUMEN_E2E_BARRIER:-1}"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    # Durable proof the prompt was read FROM THE CLAIM JSON (per-bead, test evidence).
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    # Claim marker, then BARRIER: hold the claim until >= $barrier claimants have
    # marked, so the claims overlap; proceed after ~30s regardless (never deadlock).
    : > ".gc/lumen-e2e-claimed-${id}"
    bdeadline=$((SECONDS + 30))
    while [ "$SECONDS" -lt "$bdeadline" ]; do
      n="$(ls .gc/lumen-e2e-claimed-* 2>/dev/null | wc -l | tr -d ' ')"
      [ "$n" -ge "$barrier" ] && break
      sleep 0.1
    done
    gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-barrier.sh: no work claimed within 60s" >&2
exit 1
