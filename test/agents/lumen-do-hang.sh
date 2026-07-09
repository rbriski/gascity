#!/usr/bin/env bash
# Scripted Lumen pool agent (L4 firewall e2e). Claims ONE do, writes the prompt
# readback proof, then HANGS forever without ever closing. The test SIGKILLs it and
# proves the firewall strands the claim (owned.settled failed "stranded: <assignee>",
# retryable) so the run seals FAILED-stranded instead of wedging.
#
# The hang re-execs into a `sleep` whose argv[0] is the per-city nonce
# ($GC_LUMEN_E2E_NONCE), so the test's `pgrep -f "$nonce"` finds THIS session process
# (a process-table query — the house "query live state" rule, no PID files). exec
# preserves the PID, so the process the test kills IS the one the subprocess provider
# monitors, and its death is what the firewall observes. Raw `bd` NEVER appears.
# Env: GC_CITY, GC_SESSION_NAME, PATH incl gc; GC_LUMEN_E2E_NONCE (kill target token).
set -euo pipefail
cd "$GC_CITY"
deadline=$((SECONDS + 60))
while [ "$SECONDS" -lt "$deadline" ]; do
  claim="$(gc hook --claim --json 2>/dev/null || true)"
  action="$(printf '%s' "$claim" | jq -r '.action // empty' 2>/dev/null || true)"
  if [ "$action" = "work" ]; then
    id="$(printf '%s' "$claim" | jq -r '.bead_id')"
    prompt="$(printf '%s' "$claim" | jq -r '.description // empty')"
    printf '%s\n' "$prompt" > ".gc/lumen-e2e-prompt-${id}.txt"
    # The "side effect": append ONE line per claim-and-execute. A same-name respawn that
    # ADOPTS this claimed row (the firewall-wedge hazard) would re-run the work and append
    # a second line, so a >1 line count is the adoption regression tripwire.
    printf 'exec\n' >> ".gc/lumen-e2e-exec-count-${id}.txt"
    # Never close. Re-exec into a tagged sleep so the test can find + kill this PID.
    exec -a "${GC_LUMEN_E2E_NONCE:-lumen-do-hang}" sleep 2147483647
  fi
  sleep 0.2
done
echo "lumen-do-hang.sh: no work claimed within 60s" >&2
exit 1
