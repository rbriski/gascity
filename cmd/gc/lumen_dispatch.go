package main

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// This file is the composition root for the real-bead do path (REDESIGN §2): the
// two Layer-0 seams engine.Advance is wired with, plus the do attempt-history
// visibility query. A ready pool-mode do's work becomes an ORDINARY fold_owned=0
// work bead in the city work store — spawned, claimed, closed, and recovered by the
// native pool machinery — and the controller observes its close to advance the fold.

// errLumenDispatchRoute reports that a pool-mode do's route resolves to no pool-
// capable agent template. It is a loud typed refusal at dispatch time (REDESIGN §3
// GAP): without it a bead routed to a non-template would be invisible to demand,
// claim, and orphan-release — a silent forever-park.
var errLumenDispatchRoute = errors.New("lumen dispatch: route resolves to no pool-capable agent template")

// lumenDispatchWork returns the engine.Options.DispatchWork seam (REDESIGN §2.3):
// lookup-then-create the ordinary work bead for one ready pool-mode do activation in
// the city work store, returning its store-minted id. The lookup-before-create keyed
// on the (run, activation) metadata pair is the idempotency the CAS-blob-before-append
// discipline needs: a crash between the create and the journal dispatch fact leaves a
// findable bead the next Advance re-adopts, never an orphan and never a duplicate.
func lumenDispatchWork(store beads.Store, cfg *config.City) func(context.Context, engine.WorkDispatch) (string, error) {
	return func(_ context.Context, w engine.WorkDispatch) (string, error) {
		if store == nil {
			return "", fmt.Errorf("lumen dispatch: nil work store")
		}
		// 1. Idempotency: a prior create for this exact (run, activation) wins.
		existing, err := store.List(beads.ListQuery{
			Metadata: map[string]string{
				beadmeta.LumenRunMetadataKey:        w.StreamID,
				beadmeta.LumenActivationMetadataKey: w.Activation,
			},
			IncludeClosed: true,
			Live:          true,
		})
		if err != nil {
			return "", fmt.Errorf("lumen dispatch: looking up existing bead for %s/%s: %w", w.StreamID, w.Activation, err)
		}
		if len(existing) > 0 {
			return existing[0].ID, nil
		}

		// 2. Validate the route resolves to a session-capable pool template, so the
		// bead is claimable/recoverable (mirrors the orphan-release predicate).
		if cfg != nil {
			a := findAgentByTemplate(cfg, w.Route)
			if a == nil || !a.SupportsGenericEphemeralSessions() {
				return "", fmt.Errorf("%w: node %q route %q", errLumenDispatchRoute, w.NodeID, w.Route)
			}
		}

		// 3. Create the ordinary, born-claimable work bead. Readiness gating already
		// happened in the fold (only READY do's dispatch), so it needs no deps.
		created, err := store.Create(beads.Bead{
			Type:        "task",
			Title:       w.NodeID,
			Description: w.Prompt,
			Metadata: map[string]string{
				beadmeta.RoutedToMetadataKey:        w.Route,
				beadmeta.LumenRunMetadataKey:        w.StreamID,
				beadmeta.LumenActivationMetadataKey: w.Activation,
				beadmeta.LumenAttemptMetadataKey:    strconv.Itoa(w.Attempt),
			},
		})
		if err != nil {
			return "", fmt.Errorf("lumen dispatch: creating work bead for %s/%s: %w", w.StreamID, w.Activation, err)
		}
		return created.ID, nil
	}
}

// lumenObserveWork returns the engine.Options.ObserveWork seam (REDESIGN §2.4): read
// the dispatched bead's terminal state through ordinary bead reads. A closed bead is
// terminal; its outcome is the raw gc.outcome mapped through the fail-closed firewall
// (LumenOutcomeForGCOutcome) — a bare/unknown close maps to failed, never laundered
// into success. An open/in_progress bead (including an orphan-released bead re-read
// as open) is still in flight. A missing bead is an error (ambiguous store outage vs
// deletion): the run stays parked with a loud per-tick log, never an auto-fail.
func lumenObserveWork(store beads.Store) func(context.Context, string) (engine.WorkObservation, error) {
	return func(_ context.Context, beadID string) (engine.WorkObservation, error) {
		if store == nil {
			return engine.WorkObservation{}, fmt.Errorf("lumen dispatch: nil work store")
		}
		// LIVE read: the worker closes the real bead in a SEPARATE process, and a
		// cross-process close is invisible to the controller's cached store view until
		// cache reconciliation. Read the backing store live (the reconciler's own
		// discipline) so the observe sees the close within a patrol interval.
		b, err := beads.HandlesFor(store).Live.Get(beadID)
		if err != nil {
			return engine.WorkObservation{}, fmt.Errorf("lumen dispatch: observing bead %q: %w", beadID, err)
		}
		if b.Status == "closed" {
			return engine.WorkObservation{
				Terminal: true,
				Outcome:  engine.LumenOutcomeForGCOutcome(b.Metadata[beadmeta.OutcomeMetadataKey]),
			}, nil
		}
		return engine.WorkObservation{Terminal: false}, nil
	}
}

// lumenAttemptRecord is one attempt bead in a do's attempt history (REDESIGN
// visibility requirement): fresh-bead-per-attempt keeps a failed attempt's bead
// closed-and-queryable alongside the next attempt's fresh bead.
type lumenAttemptRecord struct {
	Attempt int
	BeadID  string
	Status  string
	Outcome string
}

// lumenAttemptHistory enumerates every real work bead a run dispatched for one do
// node, oldest attempt first — the visibility path over fresh-bead-per-attempt. It
// filters by the run metadata (a conjunctive ListQuery.Metadata read) and matches the
// do by the bare node id of each bead's recorded activation, so a fail→retry→pass do
// surfaces BOTH the failed attempt-0 bead and the passed attempt-1 bead with their
// outcomes.
func lumenAttemptHistory(store beads.Store, streamID, nodeID string) ([]lumenAttemptRecord, error) {
	if store == nil {
		return nil, fmt.Errorf("lumen dispatch: nil work store")
	}
	all, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.LumenRunMetadataKey: streamID},
		IncludeClosed: true,
		Live:          true,
	})
	if err != nil {
		return nil, fmt.Errorf("lumen dispatch: attempt history for %s/%s: %w", streamID, nodeID, err)
	}
	var out []lumenAttemptRecord
	for _, b := range all {
		if engine.ActivationNodeID(b.Metadata[beadmeta.LumenActivationMetadataKey]) != nodeID {
			continue
		}
		attempt, _ := strconv.Atoi(b.Metadata[beadmeta.LumenAttemptMetadataKey])
		out = append(out, lumenAttemptRecord{
			Attempt: attempt,
			BeadID:  b.ID,
			Status:  b.Status,
			Outcome: b.Metadata[beadmeta.OutcomeMetadataKey],
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Attempt < out[j].Attempt })
	return out, nil
}
