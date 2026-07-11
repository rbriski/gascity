package splittest

import (
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/storeref"
)

func TestNewSplitStoresMintsPrefixDisjointIDs(t *testing.T) {
	work, infra := NewSplitStores(t)

	workBead, err := work.Create(beads.Bead{Title: "work"})
	if err != nil {
		t.Fatalf("work create: %v", err)
	}
	infraBead, err := infra.Create(beads.Bead{Title: "graph step"})
	if err != nil {
		t.Fatalf("infra create: %v", err)
	}

	if !strings.HasPrefix(workBead.ID, memStoreWorkPrefix+"-") {
		t.Fatalf("work store minted %s, want %s- prefix", workBead.ID, memStoreWorkPrefix)
	}
	if !strings.HasPrefix(infraBead.ID, config.InfraScopePrefix+"-") {
		t.Fatalf("infra store minted %s, want %s- prefix", infraBead.ID, config.InfraScopePrefix)
	}

	// The disjointness is the residence invariant's ID half: every
	// infra-store bead carries a reserved class prefix, no work-store bead
	// does. Pin both sides against the canonical classifier.
	if config.IsReservedClassPrefix(memStoreWorkPrefix) {
		t.Fatalf("work prefix %q became a reserved class prefix; the kit pair is no longer split-shaped", memStoreWorkPrefix)
	}
	if !config.IsReservedClassPrefix(config.InfraScopePrefix) {
		t.Fatalf("infra prefix %q is not a reserved class prefix", config.InfraScopePrefix)
	}
}

func TestNewSplitStoresRoutesByIDAcrossThePair(t *testing.T) {
	work, infra := NewSplitStores(t)

	workBead, err := work.Create(beads.Bead{Title: "work"})
	if err != nil {
		t.Fatalf("work create: %v", err)
	}
	infraBead, err := infra.Create(beads.Bead{Title: "graph step"})
	if err != nil {
		t.Fatalf("infra create: %v", err)
	}

	stores := []beads.Store{work, infra}
	if owner := storeref.PrefixOwner(infraBead.ID, stores); owner != infra {
		t.Fatalf("PrefixOwner(%s) did not route to the infra store", infraBead.ID)
	}
	got, err := storeref.Resolve(infraBead.ID, stores)
	if err != nil || got.ID != infraBead.ID {
		t.Fatalf("Resolve(%s) = %+v, %v", infraBead.ID, got, err)
	}
	got, err = storeref.Resolve(workBead.ID, stores)
	if err != nil || got.ID != workBead.ID {
		t.Fatalf("Resolve(%s) = %+v, %v", workBead.ID, got, err)
	}
}

func TestNewSplitStoresInfraHonorsExplicitWispIDs(t *testing.T) {
	_, infra := NewSplitStores(t)

	// Production molecule materialization mints EPHEMERAL wisps with
	// gcg-wisp-* ids; the kit's infra store must round-trip that exact
	// shape, not just anonymous durable rows.
	wisp, err := infra.Create(beads.Bead{ID: "gcg-wisp-dv78", Title: "step wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	if wisp.ID != "gcg-wisp-dv78" {
		t.Fatalf("explicit wisp id not honored: got %s", wisp.ID)
	}
	got, err := infra.Get(wisp.ID)
	if err != nil {
		t.Fatalf("get wisp: %v", err)
	}
	if !got.Ephemeral {
		t.Fatalf("wisp lost its ephemeral tier through the kit store: %+v", got)
	}
	wisps, err := infra.List(beads.ListQuery{Status: "open", TierMode: beads.TierWisps})
	if err != nil {
		t.Fatalf("list wisp tier: %v", err)
	}
	if !containsBeadID(wisps, wisp.ID) {
		t.Fatalf("wisp missing from its tier: %+v", wisps)
	}
}

func TestNewSplitStoresCrossStoreDepsFailLoudBothDirections(t *testing.T) {
	work, infra := NewSplitStores(t)

	workBead, err := work.Create(beads.Bead{Title: "source work bead"})
	if err != nil {
		t.Fatalf("work create: %v", err)
	}
	// The production shape from the convoy-tracks landmine: a graph-side
	// bead referencing a work-store bead (and vice versa) must not become a
	// same-store dep row.
	wisp, err := infra.Create(beads.Bead{ID: "gcg-wisp-x1", Title: "routed wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("infra create: %v", err)
	}

	if err := infra.DepAdd(wisp.ID, workBead.ID, "tracks"); err == nil || !strings.Contains(err.Error(), "no issue found") {
		t.Fatalf("infra→work dep: got %v, want bd-shaped rejection", err)
	}
	if err := work.DepAdd(workBead.ID, wisp.ID, "blocks"); err == nil || !strings.Contains(err.Error(), "no issue found") {
		t.Fatalf("work→infra dep: got %v, want bd-shaped rejection", err)
	}

	// Same-store references keep working on both members of the pair.
	other, err := work.Create(beads.Bead{Title: "same-store blocker"})
	if err != nil {
		t.Fatalf("work create: %v", err)
	}
	if err := work.DepAdd(workBead.ID, other.ID, "blocks"); err != nil {
		t.Fatalf("same-store work dep rejected: %v", err)
	}
	root, err := infra.Create(beads.Bead{Title: "graph root"})
	if err != nil {
		t.Fatalf("infra create: %v", err)
	}
	if err := infra.DepAdd(wisp.ID, root.ID, "blocks"); err != nil {
		t.Fatalf("same-store infra dep rejected: %v", err)
	}
}

func TestNewSplitStoresRejectForeignPrefixRowMinting(t *testing.T) {
	work, infra := NewSplitStores(t)

	// A gcg- row minted into the WORK store is the exact foreign-residence
	// shape behind the orphan-release and demand-gate landmines: the bead
	// LOOKS infra-resident by prefix but lives where only work-store scans
	// find it.
	if _, err := work.Create(beads.Bead{ID: "gcg-wisp-strand", Title: "stranded wisp"}); err == nil {
		t.Fatal("work store accepted an infra-prefixed row")
	}
	if _, err := infra.Create(beads.Bead{ID: "gc-9", Title: "work row in infra store"}); err == nil {
		t.Fatal("infra store accepted a work-prefixed row")
	}
}
