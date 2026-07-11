package splittest

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// memStoreWorkPrefix is the id-prefix segment beads.MemStore mints under
// ("gc-<n>", memstore.go Create) — the kit's stand-in for a rig/HQ work
// scope's EffectivePrefix. It is deliberately NOT a reserved class prefix
// (config.IsReservedClassPrefix("gc") is false), so the pair below is
// prefix-disjoint the same way a real split city is: every infra bead
// carries a reserved class prefix, no work bead does.
const memStoreWorkPrefix = "gc"

// NewSplitStores returns a prefix-disjoint strict (work, infra) store pair
// for split-store tests in beads-level consumers (internal/convoy,
// internal/molecule, internal/dispatch, ...). It mirrors the LEAF
// construction of cmd/gc's splitMoleculeStores fixture
// (e12_graph_tail_split_test.go) — work on beads.NewMemStore, infra on
// beads.NewMemStoreHonoringIDs — WITHOUT the cmd/gc policy wrapping, so the
// kit stays importable from internal packages. cmd/gc fixtures wrap these
// strict leaves in wrap{,Infra}StoreWithBeadPolicies themselves.
//
// Store shapes:
//
//   - work mints the default MemStore work prefix ("gc-<n>") and rejects
//     explicit ids outside it — in particular an infra-prefixed create,
//     the foreign-prefix row-minting half of the residence invariant.
//   - infra mints under config.InfraScopePrefix ("gcg-<n>") and honors
//     explicit reserved-prefix ids, so production-shaped wisp ids
//     (gcg-wisp-*) round-trip. It exposes IDPrefix() == "gcg" for
//     storeref prefix routing.
//   - both reject a DepAdd whose endpoint lives in the other store with a
//     bd-shaped "no issue found" error, where plain MemStores would
//     silently accept the cross-store edge.
//
// Wisp tier: the pair is tier-transparent, but it is LEAF-level — there is
// no policy wrapper expanding reads to beads.TierBoth. Fixtures that create
// ephemeral wisps (Bead.Ephemeral) must query them the way tier-aware code
// does: ListQuery.TierMode / ReadyQuery.TierMode set to beads.TierWisps or
// beads.TierBoth. Production molecules materialize as ephemeral wisps, so
// split-store invariants that skip the wisp tier miss the tier the live
// incidents actually happened in.
func NewSplitStores(t *testing.T) (work, infra beads.Store) {
	t.Helper()
	work = newStrict(beads.NewMemStore(), memStoreWorkPrefix, false)
	infra = newStrict(beads.NewMemStoreHonoringIDs(), config.InfraScopePrefix, true)
	return work, infra
}
