#!/bin/bash
# graph-store-sqlite-worker.sh — minimal no-LLM worker for the deployed
# graph_store=sqlite convergence e2e (TestGraphStoreSQLiteDeployedCityConverges).
#
# It drives a graph.v2 molecule's actionable work step to terminal so the
# controller's control-dispatcher can finalize the workflow root — all with the
# molecule's graph beads resident in the embedded SQLite store. It reaches that
# store through the gc bd-shim (gc invoked as `bd`, installed first on PATH),
# which routes graph-class bead ops through the in-process coordrouter Router.
#
# It performs ONLY Router-routable mutations on the work step:
#   bd update <id> --set-metadata gc.outcome=pass --status closed
# (both flags route to the owning SQLite backend). It NEVER uses
#   bd update <id> --claim   (unrouted: passes through to the work-only bd, a
#                             phantom-claim desync under graph_store=sqlite), nor
#   gc hook --claim          (opens a raw work-only store — never sees SQLite), nor
#   bd mol / gate / query    (refused by the shim in the split phase).
# When it must take ownership of an unassigned routed step it self-assigns via
# the ROUTABLE `--assignee` update path, not `--claim`.
#
# Required env (set by gc start): GC_CITY, GC_AGENT, GC_SESSION_NAME/ID.
set -uo pipefail
cd "${GC_CITY:-.}" 2>/dev/null || true
SESSION="${GC_SESSION_NAME:-${GC_SESSION_ID:-${GC_AGENT:-worker}}}"
TRACE="${GC_CITY:-.}/graph-store-worker-trace.log"

note() { printf '%s\n' "$*" >>"$TRACE" 2>/dev/null || true; }

# Record which `bd` resolved so a failed run shows whether the shim was installed
# (resolved -> the gc binary) or the work-only filebdshim shadowed it.
note "start session=$SESSION bd=$(command -v bd 2>/dev/null) resolved=$(readlink -f "$(command -v bd 2>/dev/null)" 2>/dev/null)"

# first_actionable_id prints the id of the first ready bead that is NOT a
# control/latch bead the controller must finalize itself. $1 is a `bd ready
# --json` array; $2 ("assigned"|"routed") selects the ownership predicate.
first_actionable_id() {
	local json="$1" mode="$2"
	local guard='def ctl: . == "workflow" or . == "workflow-finalize" or . == "scope" or . == "scope-check" or . == "gate";'
	local pred
	if [ "$mode" = "routed" ]; then
		pred='select((.assignee // "") == "") | select((.metadata["gc.routed_to"] // "") != "")'
	else
		pred='.'
	fi
	printf '%s' "$json" | jq -r "$guard [.[] | $pred | select(((.metadata[\"gc.kind\"] // \"\") | ctl) | not)][0].id // empty" 2>/dev/null || true
}

while true; do
	# 1) Hooked work: a single-session agent gets the work step Assignee=session,
	#    discoverable SQLite-aware via the shim's federated ready post-filter.
	hooked=$(bd ready --assignee="$SESSION" --json 2>/dev/null || echo '[]')
	id=$(first_actionable_id "$hooked" assigned)
	if [ -n "$id" ]; then
		bd update "$id" --set-metadata "gc.outcome=pass" --status closed >/dev/null 2>&1 \
			&& note "completed id=$id" || note "complete-failed id=$id"
		continue
	fi

	# 2) Fallback: an unassigned routed step (pool/metadata-only routing). Take
	#    ownership via the routable --assignee update, then loop to complete it.
	routed=$(bd ready --json 2>/dev/null || echo '[]')
	rid=$(first_actionable_id "$routed" routed)
	if [ -n "$rid" ]; then
		bd update "$rid" --assignee="$SESSION" --status=in_progress >/dev/null 2>&1 \
			&& note "self-assigned id=$rid" || note "self-assign-failed id=$rid"
		continue
	fi

	sleep 0.3
done
