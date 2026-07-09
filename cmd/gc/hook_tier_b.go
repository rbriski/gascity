package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// tierBFenceRetries bounds cooperative re-claim/re-settle attempts after a lease
// fence — a driver (engine.Advance) re-acquire bumped the epoch between the
// claim/settle's epoch read and its append. The fence is a retry signal, not a
// terminal failure: between Advances the stream is unheld, so a bounded re-read +
// re-append lands cooperatively.
const tierBFenceRetries = 2

// tierBFenceBackoff is the pause between fence retries (a package var for tests).
var tierBFenceBackoff = 5 * time.Millisecond

// isTierBFenceRetryable reports whether a claim/settle append error is a cooperative
// multi-writer race the bounded re-resolve+retry should chase rather than surface as
// a hard failure. Two shapes qualify, both raised only under a concurrent driver
// (engine.Advance):
//
//   - ErrLeaseFenced: a driver re-acquire bumped the writer-lease epoch between this
//     append's CurrentLeaseEpoch read and its commit, fencing the cooperative append
//     (which never committed).
//   - ErrRebuildRaced: this append DID commit, but a concurrent driver append landed
//     during the claim/settle's Tier-A projection rebuild (appendAndProject →
//     RebuildTierA's TOCTOU recheck). The event is durable and the projection
//     converges on the next fold; a re-resolve+retry re-appends idempotently under
//     the write-once claim/settle token (byte-identical replay dedupes to success)
//     and re-runs the rebuild.
//
// Treating both identically matches the engine's own retry contract (the
// isRetryableRaceErr the driver⟷claimant race suite asserts, advance_race_test.go):
// a committed-but-rebuild-raced claim/settle must NOT be reported as claims_errored /
// a non-zero close.
func isTierBFenceRetryable(err error) bool {
	return errors.Is(err, graphstore.ErrLeaseFenced) || errors.Is(err, graphstore.ErrRebuildRaced)
}

// tierBHookStoreName identifies the Tier-B journal leg among the federated hook
// stores. It is a fixed marker, not a role name. It is sourced from the single
// canonical constant so config.ValidateRigs can reserve the same value as a
// forbidden rig name (a rig with this name would collide with Tier-B routing).
const tierBHookStoreName = config.ReservedGraphJournalRigName

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

	claimErr := claimTierBWithFenceRetry(ctx, gs, ref, assignee, beadID)
	switch {
	case claimErr == nil:
		return tierBReadClaimed(ctx, gs, beadID)
	case errors.Is(claimErr, engine.ErrTierBAlreadyClaimed), errors.Is(claimErr, engine.ErrTierBClaimConflict), isTierBFenceRetryable(claimErr):
		// A persistent lease fence / rebuild race that outlasted the bounded retry is a
		// cooperative multi-writer race, not a terminal failure (L2, S17 + F2): re-read
		// and surface the winner exactly like a lost CAS, so the candidate drains as a
		// NORMAL rejection, never claims_errored.
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

// claimTierBWithFenceRetry claims a Tier-B row, retrying a bounded number of times
// on a lease fence OR a Tier-A rebuild race (both cooperative multi-writer races, see
// isTierBFenceRetryable): it re-resolves the ref (a driver re-acquire may have
// advanced the head/epoch) and re-claims cooperatively. A persistent race is returned
// unwrapped for the caller's conflict-shaped mapping; a row that settled or vanished
// under us surfaces the race for the same mapping rather than looping.
func claimTierBWithFenceRetry(ctx context.Context, gs *graphstore.Store, ref engine.TierBWorkRef, assignee, beadID string) error {
	err := claimTierBWork(ctx, gs, ref.StreamID, ref.Activation, assignee)
	for attempt := 0; attempt < tierBFenceRetries && isTierBFenceRetryable(err); attempt++ {
		time.Sleep(tierBFenceBackoff)
		r2, ok, rerr := engine.ResolveTierBWorkRef(ctx, gs, beadID)
		if rerr != nil {
			return rerr
		}
		if !ok || r2.DispatchMode != engine.DispatchModePool || r2.Settled || r2.Activation == "" {
			return err // the row moved out from under us — surface the race for mapping
		}
		err = claimTierBWork(ctx, gs, r2.StreamID, r2.Activation, assignee)
	}
	return err
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
