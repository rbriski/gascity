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

// NewRigStore returns a strict rig work-store leaf minting under the given rig
// prefix (a rig's config.Rig.EffectivePrefix, e.g. "ra" for "rig-A"), for
// split-store tests that need the third store of a real city: HQ work store +
// infra store + per-rig work stores. It honors explicit rig-prefixed ids (bd
// accepts an in-prefix --id) and rejects foreign-prefix creates and cross-store
// deps exactly like the NewSplitStores pair.
//
// Why this constructor exists: the public wrappers (Strict, StrictWithPrefix)
// never arm leaf-level id minting — that knob is internal, armed only by
// NewSplitStores for the infra leaf — while MemStore leaves mint "gc-<n>"
// regardless of the declared prefix. A strict rig-prefixed leaf was therefore
// impossible to build from outside the package: every store-minted create
// failed the namespace post-check. NewRigStore closes that gap with the same
// leaf-level minting the infra store uses, under the rig's work prefix.
//
// The prefix must be a genuine rig WORK prefix: not empty, not a reserved
// class prefix (a rig store minting infra-shaped ids would break the residence
// invariant the kit models), and not the kit's default work prefix (which
// would alias the NewSplitStores work leaf's id space and defeat by-id prefix
// routing across the trio).
func NewRigStore(t *testing.T, prefix string) beads.Store {
	t.Helper()
	p := normalizePrefix(prefix)
	if p == "" {
		t.Fatalf("splittest.NewRigStore: empty rig prefix %q; pass the rig's EffectivePrefix", prefix)
	}
	if config.IsReservedClassPrefix(p) {
		t.Fatalf("splittest.NewRigStore: rig prefix %q is a reserved class prefix; rig stores hold WORK beads and must mint outside the reserved id space", p)
	}
	if p == memStoreWorkPrefix {
		t.Fatalf("splittest.NewRigStore: rig prefix %q aliases the kit's default work-store prefix; pick a disjoint rig prefix so by-id routing across the store trio stays unambiguous", p)
	}
	return newStrict(beads.NewMemStoreHonoringIDs(), p, true)
}
