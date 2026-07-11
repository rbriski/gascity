#!/usr/bin/env bash
# check-split-topology-rows.sh
#
# Drift-prevention lint for the split-store conformance suite
# (cmd/gc/split_topology_conformance_test.go, TestSplitTopologyConformance).
#
# The suite's whole value is that every invariant runs on BOTH topologies:
#
#     single-store  — infra == nil, resolveClassStore collapses to the work
#                     store (the legacy, pre-split city; byte-identity check)
#     split         — infra != nil, coordination classes resolve to the infra
#                     store (the two-database city under test)
#
# forEachTopology / forEachTopologyWithRig run a t.Run per topology, so an
# invariant that routes through them is guarded on both. An invariant that
# minted its own single-topology env (newSplitEnv(t, true) inline) would
# silently cover only one row — a regression that breaks the other topology
# would sail through. This guard forbids that shape:
#
#   Rule A: every invariant subtest (t.Run("I...")) must invoke
#           forEachTopology or forEachTopologyWithRig.
#   Rule B: the conformance file must not call newSplitEnv* directly — all
#           env construction goes through the forEachTopology helpers, so no
#           invariant can pin itself to one topology.
#
# Exits non-zero with each violation printed. Passes silently when the suite
# is fully two-topology. Cheap + static: wired into `make check`.

set -euo pipefail

repo_root=$(cd "$(dirname "$0")/.." && pwd)
suite="$repo_root/cmd/gc/split_topology_conformance_test.go"

if [[ ! -f "$suite" ]]; then
    echo "check-split-topology-rows: suite not found: $suite" >&2
    exit 1
fi

violations=0

# Rule A: every invariant subtest routes through a forEachTopology helper.
while IFS= read -r line; do
    lineno=${line%%:*}
    body=${line#*:}
    if [[ "$body" != *forEachTopology* ]]; then
        echo "ROW-GUARD: ${suite##"$repo_root"/}:$lineno invariant subtest does not run both topologies (missing forEachTopology): ${body#	}"
        violations=$((violations + 1))
    fi
done < <(grep -nE 't\.Run\("I[0-9]' "$suite" || true)

# Rule B: no direct env construction in the suite — it must flow through the
# forEachTopology helpers (which is where the two-row fan-out lives).
while IFS= read -r line; do
    lineno=${line%%:*}
    body=${line#*:}
    echo "ROW-GUARD: ${suite##"$repo_root"/}:$lineno direct newSplitEnv bypasses forEachTopology (pins one topology): ${body#	}"
    violations=$((violations + 1))
done < <(grep -nE 'newSplitEnv(WithRig)?\(' "$suite" || true)

if (( violations > 0 )); then
    echo "---"
    echo "Split-topology row violations: $violations"
    echo "Every invariant in TestSplitTopologyConformance must run on both the"
    echo "single-store and split topologies via forEachTopology/forEachTopologyWithRig."
    echo "See scripts/check-split-topology-reverts.sh and engdocs for the soundness model."
    exit 1
fi

exit 0
