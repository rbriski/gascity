#!/usr/bin/env bash
# Scripted Lumen pool agent (L4 concurrency e2e). Same claim→read→close contract as
# lumen-do.sh, but instead of a fixed work sleep it BARRIERS on the count of
# concurrent claimants so the do's are provably in flight at once (all admits precede
# all settles in the sealed journal) and their closes fire near-simultaneously — the
# §0.2 concurrent-close stress. The barrier DEGRADES (proceeds after its own deadline)
# so its synchronization phase cannot deadlock a straggler. An optional, separately
# bounded release-file handshake lets a test observe the claimed sessions before they
# settle. Claim markers are test-evidence files, not process state. Raw `bd` NEVER
# appears (L1 corr. #5).
# Env: GC_CITY, GC_SESSION_NAME, PATH incl gc; GC_LUMEN_E2E_BARRIER (expected count);
# GC_LUMEN_E2E_CLAIM_TIMEOUT_SECONDS (optional, capped at 300s);
# GC_LUMEN_E2E_COHORT (optional marker namespace); GC_LUMEN_E2E_RELEASE_FILE
# (optional); GC_LUMEN_E2E_RELEASE_TIMEOUT_SECONDS (optional, capped at 120s).
set -euo pipefail

# Give this worker a per-city process-table identity. The integration test uses
# pgrep against this argv[0] to prove the subprocesses have actually exited;
# lifecycle beads may legitimately remain draining while the reaper catches up.
if [ -n "${GC_LUMEN_E2E_NONCE:-}" ] && [ "${GC_LUMEN_E2E_PROCESS_TAGGED:-}" != "1" ]; then
  export GC_LUMEN_E2E_PROCESS_TAGGED=1
  exec -a "$GC_LUMEN_E2E_NONCE" bash "$0" "$@"
fi

cd "$GC_CITY"

bounded_seconds() {
  local value="$1"
  local fallback="$2"
  local maximum="$3"
  case "$value" in
    ''|*[!0-9]*) value="$fallback" ;;
  esac
  if [ "${#value}" -gt 6 ]; then
    value="$maximum"
  fi
  [ "$value" -ge 1 ] || value=1
  [ "$value" -le "$maximum" ] || value="$maximum"
  printf '%s\n' "$value"
}

barrier="${GC_LUMEN_E2E_BARRIER:-1}"
claim_timeout="$(bounded_seconds "${GC_LUMEN_E2E_CLAIM_TIMEOUT_SECONDS:-}" 60 300)"
release_timeout="$(bounded_seconds "${GC_LUMEN_E2E_RELEASE_TIMEOUT_SECONDS:-}" 60 120)"
cohort="${GC_LUMEN_E2E_COHORT:-}"
case "$cohort" in
  *[!A-Za-z0-9_-]*)
    echo "lumen-do-barrier.sh: invalid cohort: $cohort" >&2
    exit 2
    ;;
esac
marker_stem="lumen-e2e-claimed-"
[ -z "$cohort" ] || marker_stem="${marker_stem}${cohort}-"

deadline=$((SECONDS + claim_timeout))
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
    : > ".gc/${marker_stem}${id}"
    bdeadline=$((SECONDS + 30))
    while [ "$SECONDS" -lt "$bdeadline" ]; do
      n=0
      for marker in ".gc/${marker_stem}"*; do
        [ -e "$marker" ] || continue
        n=$((n + 1))
      done
      [ "$n" -ge "$barrier" ] && break
      sleep 0.1
    done
    release_file="${GC_LUMEN_E2E_RELEASE_FILE:-}"
    if [ -n "$release_file" ]; then
      rdeadline=$((SECONDS + release_timeout))
      while [ ! -e "$release_file" ] && [ "$SECONDS" -lt "$rdeadline" ]; do
        sleep 0.1
      done
      if [ ! -e "$release_file" ]; then
        echo "lumen-do-barrier.sh: release file $release_file did not appear within ${release_timeout}s" >&2
        exit 1
      fi
    fi
    gc bd update "$id" --set-metadata gc.outcome=pass --status closed
    exit 0                  # one do per session; the subprocess session ends here
  fi
  sleep 0.2
done
echo "lumen-do-barrier.sh: no work claimed within ${claim_timeout}s" >&2
exit 1
