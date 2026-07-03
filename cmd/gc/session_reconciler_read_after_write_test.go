package main

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// Read-after-write harness (front-door migration Step 6d).
//
// The 6d cutover replaces the reconciler's raw-bead snapshot refresh
// (refreshSessionInfo re-projecting *beadByID[id]) with write-returns-Info
// (infoByID[id] = infoByID[id].ApplyPatch(batch) / markClosed), and then drops
// the session.Metadata[k]=v lockstep and the raw working set. The byte-identical
// write oracle (a recording fake store) is BLIND to same-tick stale reads
// (RECONCILER-FRONT-DOOR-SPEC §2 governing principle): a converted write that
// fails to refresh the infoByID snapshot is invisible until a LATER same-tick
// read consumes the stale value and flips a decision. So every lockstep drop
// needs a multi-session / read-after-write same-tick test — these.
//
// The harness exploits a determinism guarantee to place a write before a read in
// one tick: topoOrder returns a single-template working set in slice order
// (session_reconcile.go:1289 — empty deps returns `sessions` unchanged, and
// same-template sessions keep input order otherwise). So when every seeded
// session shares one template, a session earlier in the []beads.Bead slice is
// visited (and its mutation refreshed onto the snapshot) before a later
// session whose decision reads that mutation off the snapshot. Each test asserts
// an OBSERVABLE outcome (a recycle / restart_requested / running state) that
// flips iff the earlier write reached the later read through the snapshot, so it
// fails loudly if a 6d conversion leaves the snapshot stale.

// TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose guards the
// cross-session min-floor read: the progress-stall recycler exempts a stalled
// pool worker when its pool is at its configured floor, and it measures the pool
// via openPoolSessionCountForTemplate (session_reconciler.go ~2090), which reads
// !Info.Closed off the infoByID snapshot. A pool worker CLOSED earlier in the
// same tick must drop that open count so a stalled worker visited later is
// exempt.
//
// Scenario: floor 1, max 2. A stale failed-create companion (no live runtime, no
// assigned work) is first in the slice, so the reconciler closes it and refreshes
// its snapshot Info BEFORE the stalled worker's min-floor decision runs. With the
// companion closed the pool is at floor (open == 1 == min), so the stalled worker
// must NOT be recycled. If the close's snapshot refresh regresses (the 6d hazard),
// the count stays at 2 > floor and the stalled worker is wrongly recycled — the
// assertions below catch that.
//
// This is the mid-tick-close integration test Step 4D deferred as "impractical —
// topoOrder hides processing order"; single-template ordering makes it
// deterministic.
func TestReconcileSessionBeads_MinFloorCountReflectsMidTickClose(t *testing.T) {
	env, session, sessionName := newProgressStallTestEnv(t)
	env.cfg.Agents[0].MinActiveSessions = restartRequestTestIntPtr(1)
	env.cfg.Agents[0].MaxActiveSessions = restartRequestTestIntPtr(2)

	// A second worker, open at tick start (lifting open == 2 > floor 1), but a
	// stale failed-create with no live runtime and no assigned work, so the
	// reconciler closes it this tick. Placed FIRST so its close lands on the
	// snapshot before the stalled worker's min-floor read.
	closing := env.createSessionBead("worker-closing-companion")
	env.setSessionMetadata(&closing, map[string]string{"state": string(sessionpkg.StateFailedCreate)})

	env.reconcileAtPath(t.TempDir(), []beads.Bead{closing, session})

	// Precondition: the companion actually closed this tick. If it did not, the
	// count never dropped and the rest of the scenario proves nothing.
	gotClosing, err := env.store.Get(closing.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", closing.ID, err)
	}
	if gotClosing.Status != "closed" {
		t.Fatalf("companion status = %q, want closed — a failed-create worker with no live runtime must close mid-tick for this scenario to exercise the read-after-write", gotClosing.Status)
	}

	// The read-after-write assertion: after the same-tick close, open == 1 == floor,
	// so the stalled worker is a min-floor idle worker and must be left running.
	if !env.sp.IsRunning(sessionName) {
		t.Fatalf("session %q was recycled; after the same-tick companion close the pool is at floor (open == 1 == min), so the stalled worker must be min-floor exempt — the min-floor count did not reflect the same-tick close (stale snapshot)", sessionName)
	}
	got, err := env.store.Get(session.ID)
	if err != nil {
		t.Fatalf("store.Get(%s): %v", session.ID, err)
	}
	if got.Metadata["restart_requested"] != "" {
		t.Fatalf("restart_requested = %q, want empty — the stalled worker must be min-floor exempt after the same-tick close", got.Metadata["restart_requested"])
	}
	if strings.Contains(env.stderr.String(), "progress-stalled") {
		t.Fatalf("stderr = %q, want no progress-stalled diagnostic for the exempt floor worker", env.stderr.String())
	}
}
