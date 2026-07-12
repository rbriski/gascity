package main

import (
	"os"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
)

// TestSplitCityDemand_InfraWispRootedInPoolRig pins the window-10 live
// zero-demand shape: an OPEN, UNASSIGNED gcg-wisp resident in the infra store,
// routed to a rig-scoped pool, carrying gc.root_store_ref pointing at that rig's
// work store (root_store=rig:<rig>). On the live split maintainer-city this exact
// shape (gcg-wisp-32s94 routed_to=beads/gc.run-operator root_store=rig:beads)
// produced ZERO poolDesired, so the pool never minted a fresh operator and the
// beads rig's PR queue stalled. The reconciler's demand read MUST count it so a
// fresh op=start spawns.
//
// It differs from conformanceWarmTickDemand (I9) only by carrying the
// gc.root_store_ref metadata the live wisp carried — the demand-routing /
// cross-store-probe path must not treat a root_store_ref-bearing wisp
// differently. Run over both topologies so a rig-path fix keeps legacy
// byte-identity.
func TestSplitCityDemand_InfraWispRootedInPoolRig(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		rootStoreRef := "rig:" + e.rigName
		e.mintWispWith(t, wispOpts{
			title:    "rooted-in-rig infra wisp (window-10 gcg-wisp-32s94 shape)",
			routedTo: e.qualified,
			metadata: map[string]string{beadmeta.RootStoreRefMetadataKey: rootStoreRef},
		})

		result := buildDesiredStateWithSessionBeads(
			"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), e.rigStores, &sessionBeadSnapshot{}, nil, os.Stderr,
		)
		if got := result.ScaleCheckCounts[e.qualified]; got < 1 {
			t.Fatalf("poolDesired demand for %s = %d, want >= 1 (open unassigned infra wisp routed to the rig pool, root_store=%s, produced no fresh demand — the window-10 livelock)", e.qualified, got, rootStoreRef)
		}
	})
}
