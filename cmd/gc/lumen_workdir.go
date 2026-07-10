package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// lumenWorkBeadRunsInConfiguredDir reports whether a pool trigger request's work bead
// is a Lumen pool-mode do bead — one carrying gc.lumen_run. Such a bead runs in the
// agent's CONFIGURED work dir, never a per-bead <beadID>-<nodeID> trigger workspace
// (HIGH-1): the real-bead do path (REDESIGN) dispatches a plain city-store work bead
// with no isolation, so nothing materializes a per-bead dir. Stamping one either runs
// the do in an empty checkout that cannot see the repo, or wedges the pooled session
// (a tmux pane hangs on `new-session -c <absent-dir>`).
//
// This restores the intent of the deleted Tier-B work-dir guard, re-keyed from the
// old tierBHookStoreName store-ref onto the gc.lumen_run marker the real bead carries.
// The lookup is a cached point-Get against the city work store (where the real bead
// lives, already read for demand this reconcile pass); a miss or a non-Lumen bead
// returns false, so an ordinary trigger bead's work-dir resolution is byte-identical.
func lumenWorkBeadRunsInConfiguredDir(bp *agentBuildParams, request SessionRequest) bool {
	if bp == nil || bp.beadStore == nil {
		return false
	}
	id := strings.TrimSpace(request.WorkBeadID)
	if id == "" {
		return false
	}
	b, err := bp.beadStore.Get(id)
	if err != nil {
		return false
	}
	return strings.TrimSpace(b.Metadata[beadmeta.LumenRunMetadataKey]) != ""
}
