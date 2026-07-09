package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// lumenStrandedGraceFloor is the minimum grace window before the firewall settles a
// stranded claimant, regardless of a fast patrol cadence.
const lumenStrandedGraceFloor = 60 * time.Second

// lumenStrandedGrace derives the firewall's grace window from the patrol interval:
// max(2×patrol, 60s). No new config knob — the reconciler's stranded gate itself
// has no independent grace timer (the sleep transition it requires IS the implicit
// grace), so 2×patrol gives the reconciler at least one full patrol to observe and
// stamp its verdict before the firewall acts.
func lumenStrandedGrace(patrol time.Duration) time.Duration {
	if g := 2 * patrol; g > lumenStrandedGraceFloor {
		return g
	}
	return lumenStrandedGraceFloor
}

// patrolInterval returns the city's patrol interval, defaulting safely when no
// config is wired (tests, bare runtimes).
func (cr *CityRuntime) patrolInterval() time.Duration {
	if cr.cfg == nil {
		return 30 * time.Second
	}
	return cr.cfg.Daemon.PatrolIntervalDuration()
}

// lumenClaimOrphanFirewall sweeps claimed-but-unsettled Tier-B work whose claimant
// is dead or stranded and, past a grace window, settles it failed so the run
// advances (skip-cascade) instead of wedging on a write-once claim. It CONSUMES the
// reconciler's liveness verdict — a claimant is dead when NO session bead matches
// its assignee (recycled/deleted), or stranded when the matched session carries the
// reconciler's durable stranded marker — it never re-derives liveness itself
// (keeping judgment out of Go). A zombie's later close loses loudly at the
// write-once settle token.
//
// It runs on the reconciler goroutine (single-threaded with the runs loop, so
// cr.lumen state needs no mutex). A nil snapshot means liveness is unknowable this
// tick (the sessions store is unavailable) — the firewall then does nothing rather
// than falsely kill live work.
func (cr *CityRuntime) lumenClaimOrphanFirewall(ctx context.Context, gs *graphstore.Store, snapshot *sessionBeadSnapshot) {
	if snapshot == nil {
		return // liveness unknowable — never falsely kill
	}
	lr := cr.ensureLumenRuntime()
	surface, ok := beads.TierBClaimSurfaceStoreFor(beads.NewJournalStore(gs))
	if !ok {
		return
	}
	claimed, err := surface.TierBAssigned(ctx, beads.TierBAssignedQuery{
		MarkerKey:   engine.DispatchModeMetaKey,
		MarkerValue: engine.DispatchModePool,
	})
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen firewall: reading claimed work: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	grace := lumenStrandedGrace(cr.patrolInterval())
	now := lr.clk.Now()
	stillClaimed := map[string]bool{} // activations claimed this tick (to prune the grace clock)

	for _, b := range claimed {
		if b.Status != engine.StatusClaimed {
			continue // only claimed-but-unsettled rows can wedge
		}
		ref, found, rerr := engine.ResolveTierBWorkRef(ctx, gs, b.ID)
		if rerr != nil {
			fmt.Fprintf(cr.stderr, "%s: lumen firewall: resolving %q: %v\n", cr.logPrefix, b.ID, rerr) //nolint:errcheck // best-effort stderr
			continue
		}
		if !found || ref.Settled || ref.Activation == "" {
			continue
		}
		stillClaimed[ref.Activation] = true

		if !firewallClaimantDeadOrStranded(snapshot, ref.Assignee) {
			delete(lr.deadSince, ref.Activation) // claimant recovered — reset the clock
			continue
		}
		firstSeen, seen := lr.deadSince[ref.Activation]
		if !seen {
			lr.deadSince[ref.Activation] = now // start the grace clock
			continue
		}
		if now.Sub(firstSeen) < grace {
			continue // grace not yet elapsed (a restart resets the clock — conservative)
		}

		// The DRIVER settles the stranded claim failed as a RETRYABLE infrastructure
		// strand (closer + closerID both "" = the controller-only authoritative override,
		// retryable = true): under a retry-wrapped do the loop re-attempts it as a fresh
		// activation; under repeat the outcome-agnostic cond re-attempts regardless. The
		// write-once token + the closer-identity guard (name AND instance-id axes) mean a
		// zombie's later close loses loud (ErrTierBNotClaimant / claim-conflict).
		if err := engine.SettleTierBWorkAs(ctx, gs, ref.StreamID, ref.Activation, engine.OutcomeFailed, "stranded: "+ref.Assignee, "", "", true); err != nil {
			fmt.Fprintf(cr.stderr, "%s: lumen firewall: settling stranded %q: %v\n", cr.logPrefix, ref.Activation, err) //nolint:errcheck // best-effort stderr
			continue
		}
		delete(lr.deadSince, ref.Activation)
		// Re-Advance the affected stream in the same tick: the skip-cascade routes
		// around the failed do and the run seals failed instead of wedging.
		cr.advanceLumenRun(ctx, gs, engine.OpenRun{RootID: ref.StreamID, StreamID: ref.StreamID})
	}

	// Prune grace-clock entries for activations no longer claimed (settled/gone).
	for act := range lr.deadSince {
		if !stillClaimed[act] {
			delete(lr.deadSince, act)
		}
	}
}

// firewallClaimantDeadOrStranded consumes the reconciler's liveness verdict for a
// claimed row's assignee: dead when NO open session bead matches the assignee
// (recycled/deleted claimant), stranded when the matched session carries the
// reconciler's durable stranded marker (session_reconciler.go stamps it when its
// pool-managed ∧ freeable ∧ not-alive ∧ holds-assigned-work gate fires). It never
// probes runtime liveness itself.
func firewallClaimantDeadOrStranded(snapshot *sessionBeadSnapshot, assignee string) bool {
	assignee = strings.TrimSpace(assignee)
	if assignee == "" {
		return false
	}
	for _, sb := range snapshot.Open() {
		if !sessionBeadMatchesAssignee(sb, assignee) {
			continue
		}
		// Matched a live session bead: stranded iff the reconciler stamped its marker.
		return strings.TrimSpace(sb.Metadata[strandedEventEmittedKey]) != ""
	}
	return true // no session bead matches → the claimant was recycled/deleted
}

// sessionBeadMatchesAssignee reports whether a session bead owns the given assignee
// identity, using the same identity vocabulary the reconciler and pool-desired-state
// use.
func sessionBeadMatchesAssignee(sb beads.Bead, assignee string) bool {
	for _, id := range sessionBeadAssigneeIdentitiesInfo(sessionpkg.InfoFromPersistedBead(sb)) {
		if strings.TrimSpace(id) == assignee {
			return true
		}
	}
	return false
}
