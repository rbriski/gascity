package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// tierBHookStoreName identifies the Tier-B journal leg among the federated hook
// stores. It is a fixed marker, not a role name.
const tierBHookStoreName = "graph-journal"

// claimTierBWork is the engine claim seam claimTierBWorkBead calls. It is a
// package var so a test can inject the error-mapping branches — ErrTierBNotClaimable
// (skip, no event), a raw ErrLeaseFenced and a generic error (both drain as
// claims_errored, never laundered to no_work) — that a real single-writer engine
// reaches only under contention, without racing a live claim.
var claimTierBWork = engine.ClaimTierBWork

// tierBHookStore builds the federated Tier-B hook store for a graph-scoped city,
// or reports present=false for a city with no graph scope (leaving a non-Lumen
// city's hook path byte-identical). Its query reads the fold-owned pool claim
// surface — assigned rows first, then the routed frontier, matching tryHookClaim's
// crash-recovery-before-routed tier order — and its claim translates a worker's
// claim into an engine owned.admitted append. It is appended LAST in the store
// slice so existing bd-store precedence is preserved exactly.
func tierBHookStore(cityPath string, routeTargets, identityCandidates []string, assignee string) (hookStore, bool) {
	if !cityHasGraphScope(cityPath) {
		return hookStore{}, false
	}
	st := hookStore{
		name: tierBHookStoreName,
		dir:  cityPath,
		query: func() (string, error) {
			return tierBHookQuery(cityPath, routeTargets, identityCandidates)
		},
		claim: func(ctx context.Context, _ string, _ []string, beadID, claimant string) (beads.Bead, bool, error) {
			// The claimant is the per-call assignee the federation passes
			// (opts.Assignee); the store-construction assignee is a defensive fallback
			// for the (never-hit in production) empty-arg case.
			if strings.TrimSpace(claimant) == "" {
				claimant = assignee
			}
			return claimTierBWorkBead(ctx, cityPath, beadID, claimant)
		},
	}
	return st, true
}

// tierBHookQuery reads the fold-owned pool claim surface as CLI JSON the standard
// hook claim path (decodeHookClaimBeads) consumes. An opted-but-unopenable journal
// degrades to no candidates with a stderr note (a best-effort federated leg — the
// hard-fail discipline belongs to the L2 controller/demand side), so routed pool
// work simply waits for the journal rather than wedging the hook.
func tierBHookQuery(cityPath string, routeTargets, identityCandidates []string) (string, error) {
	store := cachedCityGraphJournal(cityPath)
	if store == nil {
		fmt.Fprintf(os.Stderr, "gc hook --claim: tier-b journal unavailable for %q (routed pool work will wait)\n", cityPath) //nolint:errcheck
		return "[]", nil
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		fmt.Fprintf(os.Stderr, "gc hook --claim: tier-b claim surface unavailable for %q\n", cityPath) //nolint:errcheck
		return "[]", nil
	}
	ctx := context.Background()
	// Assigned (crash-recovery / already-owned) rows FIRST, then the routed
	// frontier — the same tier order tryHookClaim applies (adopt owned in_progress
	// before claiming fresh routed work).
	assigned, err := surface.TierBAssigned(ctx, beads.TierBAssignedQuery{
		Assignees:   identityCandidates,
		MarkerKey:   engine.DispatchModeMetaKey,
		MarkerValue: engine.DispatchModePool,
	})
	if err != nil {
		return "", fmt.Errorf("tier-b assigned query: %w", err)
	}
	routed, err := surface.TierBRoutedFrontier(ctx, routeTargets, 0)
	if err != nil {
		return "", fmt.Errorf("tier-b routed frontier query: %w", err)
	}
	candidates := make([]beads.Bead, 0, len(assigned)+len(routed))
	candidates = append(candidates, assigned...)
	candidates = append(candidates, routed...)
	out, err := json.Marshal(candidates)
	if err != nil {
		return "", fmt.Errorf("tier-b query marshal: %w", err)
	}
	return string(out), nil
}

// claimTierBWorkBead translates a worker's claim of a projected Tier-B work bead
// into an engine owned.admitted append, mapping the engine's typed outcomes onto
// the hookClaimFunc contract:
//
//   - success            → re-read the claimed row and return (bead, true, nil);
//   - already-claimed / conflict → re-read and return (winner, false, nil) so the
//     standard bead.claim_rejected fires with the winner;
//   - not-claimable      → (Bead{}, false, nil): next candidate, no event;
//   - anything else (incl. a raw ErrLeaseFenced, whose retryable mapping is L2's)
//     → the error, so the candidate drains as claims_errored, never laundered.
//
// It opens its own *graphstore.Store for the mutation (S13: the engine claim API
// takes a *graphstore.Store, which the beads claim surface deliberately does not
// expose) and re-reads through that same handle so the returned bead reflects the
// just-committed claim.
func claimTierBWorkBead(ctx context.Context, cityPath, beadID, assignee string) (beads.Bead, bool, error) {
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("tier-b claim: loading graph backend: %w", err)
	}
	gs, err := backend.openGraphStore(ctx, cityPath)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("tier-b claim: opening graph store: %w", err)
	}
	defer func() { _ = gs.Close() }()

	ref, ok, err := engine.ResolveTierBWorkRef(ctx, gs, beadID)
	if err != nil {
		return beads.Bead{}, false, fmt.Errorf("tier-b claim: resolving %q: %w", beadID, err)
	}
	if !ok || ref.DispatchMode != engine.DispatchModePool {
		return beads.Bead{}, false, nil
	}

	claimErr := claimTierBWork(ctx, gs, ref.StreamID, ref.Activation, assignee)
	switch {
	case claimErr == nil:
		return tierBReadClaimed(ctx, gs, beadID)
	case errors.Is(claimErr, engine.ErrTierBAlreadyClaimed), errors.Is(claimErr, engine.ErrTierBClaimConflict):
		current, found, rerr := tierBReadClaimed(ctx, gs, beadID)
		if rerr != nil || !found {
			return beads.Bead{}, false, nil
		}
		return current, false, nil
	case errors.Is(claimErr, engine.ErrTierBNotClaimable):
		return beads.Bead{}, false, nil
	default:
		return beads.Bead{}, false, fmt.Errorf("tier-b claim of %q: %w", beadID, claimErr)
	}
}

// appendTierBAssignedWork appends claimed (in_progress) fold-owned pool rows from
// the graph journal to the reconciler's assigned-work slices (S11), so the resume
// tier keeps a mid-do Lumen worker's session alive instead of DRAINING it. It is a
// no-op for a city with no graph scope, and read-only (a fold row is write-closed,
// so it must never be written — see the insertion contract in buildDesiredState).
//
// It appends to all THREE index-aligned slices: releaseOrphanedPoolAssignments
// index-aligns beads with stores, and filterAssignedWorkBeadsForPoolDemand with
// refs, so a partial append would misalign them. The rows carry a distinct store
// ref so nothing writes them back through a work store. The release story for
// orphaned fold rows this makes visible is L2 (blueprint §2.4) — not exercised in
// L1, whose worker sessions are all live.
func appendTierBAssignedWork(cityPath string, beadsOut *[]beads.Bead, stores *[]beads.Store, refs *[]string, stderr io.Writer) {
	if !cityHasGraphScope(cityPath) {
		return
	}
	store := cachedCityGraphJournal(cityPath)
	if store == nil {
		return
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		return
	}
	assigned, err := surface.TierBAssigned(context.Background(), beads.TierBAssignedQuery{
		MarkerKey:   engine.DispatchModeMetaKey,
		MarkerValue: engine.DispatchModePool,
	})
	if err != nil {
		fmt.Fprintf(stderr, "buildDesiredState: tier-b assigned work query: %v\n", err) //nolint:errcheck
		return
	}
	for _, b := range assigned {
		// Only claimed (in_progress) rows drive the resume tier; an open row is
		// unassigned and would never match a session anyway.
		if b.Status != engine.StatusClaimed {
			continue
		}
		*beadsOut = append(*beadsOut, b)
		*stores = append(*stores, store)
		*refs = append(*refs, tierBHookStoreName)
	}
}

// tierBReadClaimed re-reads the projected fold-owned row through the store handle
// that committed the claim, so the returned bead carries gc.routed_to (the claim
// route) and the rendered prompt (Description).
func tierBReadClaimed(ctx context.Context, gs *graphstore.Store, beadID string) (beads.Bead, bool, error) {
	surface, ok := beads.TierBClaimSurfaceStoreFor(beads.NewJournalStore(gs))
	if !ok {
		return beads.Bead{}, false, fmt.Errorf("tier-b claim: claim surface unavailable")
	}
	return surface.FoldOwnedGet(ctx, beadID)
}
