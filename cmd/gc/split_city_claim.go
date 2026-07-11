package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// On a split city the worker's work_query (now composite `gc ready`) surfaces
// graph-class step beads that live in the INFRA store, but the winning hookStore
// still points at the WORK store — so a claim/update `bd` subprocess built from
// its dir/env would run `bd update --claim` against the work store and fail
// ("bead not found"). These helpers route each claim-time mutation to the store
// that OWNS the target bead, by reserved id-prefix (gcg- → infra), mirroring the
// read-side storeForID routing and the existing per-rig crossStoreClaimDir.

// hookClaimTargetsInfra reports whether a claim-time mutation on beadID must be
// routed to the infra store: true only on a split city for a reserved-class id
// namespace ("gcg-...", including bd's wisp-tier "gcg-wisp-..." ids — the shape
// production molecules actually claim). This is the by-prefix ownership
// decision, mirroring the read-side claimableStore.storeForID and
// slingSourceStoreRootForCandidate.
func hookClaimTargetsInfra(cityPath, beadID string) bool {
	return cityHasInfraStore(cityPath) && config.IsReservedClassBeadID(beadID)
}

// hookClaimInfraDirEnv returns the (dir, env) a claim-time bd mutation on beadID
// must use. A reserved-class (gcg-...) id on a split city is owned by the infra
// store, so the mutation targets the infra scope's dir/env (the same
// bdRuntimeEnvForRigWithError projection the infra store's opener and the sling
// write path use). Work-class ids, single-store cities, and any infra-env
// resolution failure fall back to the passed (dir, env) — a wrong-store write
// then fails loud rather than silently hitting the wrong store.
func hookClaimInfraDirEnv(cityPath string, cfg *config.City, beadID, dir string, env []string) (string, []string) {
	if !hookClaimTargetsInfra(cityPath, beadID) {
		return dir, env
	}
	infraDir := infraScopeRoot(cityPath)
	overrides, err := bdRuntimeEnvForRigWithError(cityPath, cfg, infraDir)
	if err != nil {
		return dir, env
	}
	return infraDir, mergeRuntimeEnv(env, overrides)
}

// splitCityHookClaimOps returns the claim ops with the mutation seams wrapped to
// route by-id to the infra store on a split city. Only the routing-sensitive
// seams are set; tryHookClaim's applyDefaults fills the rest (Runner, DrainAck,
// EmitClaimRejected, ResolveWorkBranch, Now) with their production defaults. The
// wrappers delegate to the same *WithBdStore implementations, only swapping the
// (dir, env) for a reserved-class target.
func splitCityHookClaimOps(cityPath string, cfg *config.City) hookClaimOps {
	route := func(beadID, dir string, env []string) (string, []string) {
		return hookClaimInfraDirEnv(cityPath, cfg, beadID, dir, env)
	}
	return hookClaimOps{
		Claim: func(ctx context.Context, dir string, env []string, beadID, assignee string) (beads.Bead, bool, error) {
			d, e := route(beadID, dir, env)
			return hookClaimWithBdStore(ctx, d, e, beadID, assignee)
		},
		StampWorkBranch: func(ctx context.Context, dir string, env []string, beadID, assignee, branch string) error {
			d, e := route(beadID, dir, env)
			return hookStampWorkBranchWithBdStore(ctx, d, e, beadID, assignee, branch)
		},
		// Continuation siblings live in the same store as the workflow root, so
		// route the list read by the ROOT bead's prefix.
		ListContinuation: func(ctx context.Context, dir string, env []string, rootID, group string) ([]beads.Bead, error) {
			d, e := route(rootID, dir, env)
			return hookListContinuationWithBdStore(ctx, d, e, rootID, group)
		},
		AssignContinuation: func(ctx context.Context, dir string, env []string, beadID, assignee string) error {
			d, e := route(beadID, dir, env)
			return hookAssignContinuationWithBdStore(ctx, d, e, beadID, assignee)
		},
		// The session bead is session-class, which on a split city also lives in
		// the infra store — route by the session bead's own id prefix.
		RecordSessionPointers: func(ctx context.Context, dir string, env []string, assignee, sessionBeadID, runID, stepID string) error {
			d, e := route(sessionBeadID, dir, env)
			return hookRecordSessionPointersWithBdStore(ctx, d, e, assignee, sessionBeadID, runID, stepID)
		},
	}
}
