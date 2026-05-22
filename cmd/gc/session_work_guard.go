package main

import (
	"fmt"
	"io"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// closeSessionBeadIfUnassigned closes a session bead only when the live store
// confirms no open or in-progress work is assigned to it across the primary
// store AND any attached rig stores. Use this cross-store guard for cleanup
// paths that must not orphan work in any attached store.
//
// Callers must NOT pass a pre-computed work snapshot — this helper queries the
// stores itself so its decision cannot be poisoned by a stale snapshot taken
// earlier in the tick (see the PR that retired the snapshot-based variant).
// Live-query failures fail closed: the bead stays open until assignment can be
// re-verified.
func closeSessionBeadIfUnassigned(
	store beads.Store,
	rigStores map[string]beads.Store,
	cfg *config.City,
	session beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if stderr == nil {
		stderr = io.Discard
	}
	hasAssignedWork, err := sessionHasOpenAssignedWorkForConfig(store, rigStores, session, cfg)
	if err != nil {
		fmt.Fprintf(stderr, "session work guard: checking assigned work for %s: %v\n", session.ID, err) //nolint:errcheck
		return false
	}
	if hasAssignedWork {
		return false
	}
	if isFailedCreateSessionBead(session) {
		return closeFailedCreateBead(store, session.ID, now, stderr)
	}
	return closeBead(store, session.ID, reason, now, stderr)
}

// closeSessionBeadKnownUnassigned is the terminal mutation helper for callers
// that have already proven the runtime session is stopped and no open or
// in-progress work is assigned to the bead. Running sessions must not use this:
// they can claim work after any point-in-time assignment snapshot.
func closeSessionBeadKnownUnassigned(
	store beads.Store,
	sb beads.Bead,
	reason string,
	now time.Time,
	stderr io.Writer,
) bool {
	if store == nil || sb.ID == "" || sb.Status == "closed" {
		return false
	}
	if stderr == nil {
		stderr = io.Discard
	}
	if isFailedCreateSessionBead(sb) {
		return closeFailedCreateBead(store, sb.ID, now, stderr)
	}
	if setMetaBatch(store, sb.ID, sessionpkg.ClosePatch(now, reason), stderr) != nil {
		return false
	}
	if err := store.Close(sb.ID); err != nil {
		fmt.Fprintf(stderr, "session beads: closing %s: %v\n", sb.ID, err) //nolint:errcheck
		return false
	}
	cancelStateAssignedToRetiredSessionBead(store, sb.ID, now, stderr)
	return true
}
