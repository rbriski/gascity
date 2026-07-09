#!/usr/bin/env bash
# Scripted Lumen pool agent (single-kill RECOVERY e2e). Same claim→read loop as
# lumen-do-hang.sh, but it HANGS only on the FIRST attempt and COMPLETES every later
# one — the recovery arm of the firewall-wedge fix:
#
#   attempt :0  → write the exec marker, arm the done flag, then hang under the nonce
#                 (killable) so the test SIGKILLs it once and the firewall strands lane:0
#                 as a RETRYABLE infrastructure strand;
#   attempt :1+ → the retry mints a fresh activation; a FRESH pooled worker claims it,
#                 writes its exec marker, and closes gc.outcome=pass → the run seals pass.
#
# The done flag is armed AFTER the claim but is only observable to the kill AFTER the
# re-exec into the tagged sleep (the nonce process the test's pgrep targets exists only
# then), so a single kill can never race the flag write: the second worker always sees it.
#
# The exec marker (append per claim-and-execute, keyed by bead id) is the "no adopted
# duplicate execution" proof: both attempts claim the bare id "lane", so two legit
# attempts append two lines; an ADOPTED re-execution of lane:0 during the kill window
# would append a third — the wedge-adoption tripwire.
#
# Env: GC_CITY, GC_SESSION_NAME, PATH incl gc; GC_LUMEN_E2E_NONCE (kill target token).
set -euo pipefail
cd "$GC_CITY"
flag=".gc/lumen-e2e-hang-once-done"
deadline=$((SECONDS + 90))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    printf 'exec\n' >> ".gc/lumen-e2e-exec-count-${id}.txt"
    if [ ! -f "$flag" ]; then
      # First attempt: arm the done flag, then hang (killable) so the firewall strands it.
      : > "$flag"
      exec -a "${GC_LUMEN_E2E_NONCE:-lumen-do-hang}" sleep 2147483647
    fi
    # A later attempt (the retry): complete the do so the loop settles pass.
    gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    exit 0
  fi
  sleep 0.2
done
echo "lumen-do-hang-once.sh: no work claimed within 90s" >&2
exit 1
