#!/usr/bin/env bash
# check-split-topology-reverts.sh
#
# The falsification harness for the split-store soundness claim. For each
# split-store fix commit it inverse-applies ONLY the production hunks
# (excluding *_test.go, so the guarding test stays present) into a throwaway
# worktree, then runs the named test and asserts it goes RED. A fix whose
# named test still PASSES with the production change reverted is a HOLE: the
# invariant does not actually guard the fix, and the soundness claim over that
# landmine is unbacked.
#
# This is expensive (a worktree + per-package build + test per entry), so it is
# ON-DEMAND — `make check-split-topology-reverts`, NOT part of `make check`.
# The cheap always-on guard is scripts/check-split-topology-rows.sh.
#
# Usage:
#   scripts/check-split-topology-reverts.sh                 # full matrix
#   scripts/check-split-topology-reverts.sh --sample N      # first N entries
#   scripts/check-split-topology-reverts.sh --sha <prefix>  # single entry
#
# Each MATRIX row: "<commit>|<package>|<TestName>|<landmine label>"
#   <commit>   fix commit whose production hunks are inverse-applied
#   <package>  go package dir holding the guarding test
#   <TestName> exact top-level test func the revert must red
#   <label>    human description (the landmine the fix closed)

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)

# The soundness ledger: fix commit -> the named test that reds when the
# production change is reverted. Keep this in sync when a new split-store fix
# lands (add its commit + the regression test that must fail without it).
MATRIX=(
    "5b03f0686|internal/dispatch|TestProcessWorkflowFinalizeFailsLoudWhenCrossStoreRefUnresolvable|#3 fail loud when a cross-store source close has no resolver"
    "3bfeaff84|cmd/gc|TestWispAutocloseClosesRootOnlyWispViaInputConvoyAcrossStores|#10 reap root-only wisps via the input convoy's owning store"
    "c224a9792|internal/api|TestBeadReadyFederatesInfraStore|#13 federate the infra store in HTTP bead ready/list"
    "237c2fc9c|internal/sling|TestInstantiateSlingFormulaRoutesMoleculeByClass|#17 route a v1 formula molecule to the work store"
    "09890178b|cmd/gc|TestWaitForSessionCommandable_ReadsInfraStoreSessionOnSplitCity|#18 read session commandability from the infra store"
    "180ad7dd8|cmd/gc|TestBuildDesiredState_WarmTick_TreadmillSessionsStayDesired|spawn/drain treadmill (isCold demand gate)"
    "6b58b621b|internal/config|TestReservedClassBeadIDPrefix|#19 classify reserved-class bead ids by first segment"
    "6b58b621b|cmd/gc|TestClaimableStoreRoutesWispIDsToInfra|#19 wisp-id claim routing (production path)"
    "7ee481bf2|cmd/gc|TestReleaseOrphanedPoolAssignments_OwnsClaimedInfraWispWithoutLiveProbe|wake-filter infra store-ref leg"
)

sample=0
only_sha=""
while [[ $# -gt 0 ]]; do
    case "$1" in
        --sample) sample="${2:-0}"; shift 2 ;;
        --sha) only_sha="${2:-}"; shift 2 ;;
        *) echo "unknown arg: $1" >&2; exit 2 ;;
    esac
done

# Throwaway detached worktree so the live tree is never touched.
wt=$(mktemp -d "${TMPDIR:-/tmp}/split-revert.XXXXXX")
cleanup() {
    git -C "$repo_root" worktree remove --force "$wt" >/dev/null 2>&1 || true
    rm -rf "$wt" 2>/dev/null || true
}
trap cleanup EXIT
git -C "$repo_root" worktree add --detach "$wt" HEAD >/dev/null 2>&1

holes=0
conflicts=0
checked=0
count=0

for row in "${MATRIX[@]}"; do
    IFS='|' read -r sha pkg test label <<<"$row"

    if [[ -n "$only_sha" && "$sha" != "$only_sha"* ]]; then
        continue
    fi
    if (( sample > 0 && count >= sample )); then
        break
    fi
    count=$((count + 1))

    echo "==> ${sha} ${pkg} ${test}"
    echo "    ($label)"

    # Two revert methods, tried in order, taking the union of what each can
    # cleanly isolate:
    #   1. Reverse-apply the production hunks only (excludes *_test.go, so the
    #      guarding test stays present). Narrowest surface -> cleanest for a fix
    #      whose production lines were not touched by later commits.
    #   2. Full 3-way revert of the commit + restore its own test files from
    #      HEAD. Handles later-commit entanglement that the narrow apply cannot.
    # Only when BOTH fail is the fix genuinely un-isolatable (its lines were
    # superseded by later refactors); that is reported as a CONFLICT, not a
    # pass -- the behavior stays guarded by the standing green test, but this
    # historical commit cannot be isolate-reverted.
    reverted=0
    if git -C "$wt" show "$sha" -- '*.go' ':(exclude)*_test.go' \
        | git -C "$wt" apply -R 2>/dev/null; then
        reverted=1
    elif git -C "$wt" revert -n --no-edit "$sha" >/dev/null 2>&1; then
        testfiles=$(git -C "$wt" show --name-only --pretty=format: "$sha" | grep '_test\.go$' || true)
        if [[ -n "$testfiles" ]]; then
            # shellcheck disable=SC2086
            git -C "$wt" checkout HEAD -- $testfiles >/dev/null 2>&1 || true
        fi
        reverted=1
    else
        git -C "$wt" revert --quit >/dev/null 2>&1 || true
    fi
    if (( reverted == 0 )); then
        echo "    CONFLICT: fix does not cleanly revert by narrow-apply or 3-way (entangled with later commits on the same lines); guarded by a standing green test but not isolate-revertible."
        conflicts=$((conflicts + 1))
        git -C "$wt" reset --hard -q HEAD >/dev/null 2>&1 || true
        git -C "$wt" clean -fdq >/dev/null 2>&1 || true
        continue
    fi

    checked=$((checked + 1))
    set +e
    out=$(cd "$wt" && go test "./${pkg}/" -run "^${test}\$" -count=1 2>&1)
    rc=$?
    set -e

    if (( rc == 0 )); then
        echo "    HOLE: ${test} still PASSES with ${sha} reverted — the invariant does not guard this fix."
        holes=$((holes + 1))
    else
        if grep -q "build failed\|cannot find\|undefined:" <<<"$out"; then
            echo "    RED (build): ${test} cannot compile without the fix — guarded (weaker signal; prefer a behavioral red)."
        else
            echo "    RED: ${test} fails with ${sha} reverted — the fix is guarded. ✓"
        fi
    fi

    # Restore the worktree to HEAD for the next entry.
    git -C "$wt" revert --quit >/dev/null 2>&1 || true
    git -C "$wt" reset --hard -q HEAD >/dev/null 2>&1 || true
    git -C "$wt" clean -fdq >/dev/null 2>&1 || true
done

echo "---"
echo "checked=$checked  guarded=$((checked - holes))  holes=$holes  conflicts=$conflicts"
if (( holes > 0 )); then
    echo "SOUNDNESS HOLE: at least one fix is not backed by its named test. Investigate before claiming the arch is sound."
    exit 1
fi
if (( checked == 0 )); then
    echo "no entries checked (all filtered or all conflicts)"
    exit 1
fi
exit 0
