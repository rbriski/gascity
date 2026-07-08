package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

const casMetaKey = "gc.control_epoch"

// TestBeadPolicyStoreForwardsConditionalMetadata proves the metadata CAS is
// reachable through the policy wrapper via ConditionalMetadataStoreFor (the
// ConditionalMetadataHandle forward), and that a swap round-trips through it.
func TestBeadPolicyStoreForwardsConditionalMetadata(t *testing.T) {
	ctx := context.Background()
	backing := beads.NewMemStore()
	wrapped := wrapStoreWithBeadPolicies(backing, nil)

	cas, ok := beads.ConditionalMetadataStoreFor(wrapped)
	if !ok {
		t.Fatal("ConditionalMetadataStoreFor(policyStore) = false, want reachable through the policy wrapper")
	}
	bead, err := wrapped.Create(beads.Bead{Title: "work", Metadata: map[string]string{casMetaKey: "1"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	swapped, err := cas.SetMetadataIf(ctx, bead.ID, casMetaKey, "1", "2")
	if err != nil {
		t.Fatalf("SetMetadataIf: %v", err)
	}
	if !swapped {
		t.Fatal("swapped = false, want true through the policy wrapper")
	}
	got, err := wrapped.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata[casMetaKey] != "2" {
		t.Fatalf("value = %q, want 2", got.Metadata[casMetaKey])
	}
}

// TestResidenceRouterRoutesConditionalMetadataByResidence proves the router
// forwards SetMetadataIf to the OWNING leg — legacy-resident beads to the legacy
// leg, journal-resident beads to the journal leg — never blindly to one leg.
func TestResidenceRouterRoutesConditionalMetadataByResidence(t *testing.T) {
	ctx := context.Background()

	t.Run("legacyResidentBead", func(t *testing.T) {
		journal := beads.NewMemStore()
		legacy := beads.NewMemStore()
		router := newResidenceRoutingGraphStore(journal, legacy)

		b, err := legacy.Create(beads.Bead{Title: "legacy", Metadata: map[string]string{casMetaKey: "1"}})
		if err != nil {
			t.Fatalf("Create on legacy leg: %v", err)
		}
		cas, ok := beads.ConditionalMetadataStoreFor(router)
		if !ok {
			t.Fatal("ConditionalMetadataStoreFor(router) = false, want reachable")
		}
		swapped, err := cas.SetMetadataIf(ctx, b.ID, casMetaKey, "1", "2")
		if err != nil {
			t.Fatalf("SetMetadataIf: %v", err)
		}
		if !swapped {
			t.Fatal("swapped = false, want true for a legacy-resident bead")
		}
		got, err := legacy.Get(b.ID)
		if err != nil {
			t.Fatalf("legacy Get: %v", err)
		}
		if got.Metadata[casMetaKey] != "2" {
			t.Fatalf("legacy leg value = %q, want 2 (the CAS routed to the legacy leg)", got.Metadata[casMetaKey])
		}
	})

	t.Run("journalResidentBead", func(t *testing.T) {
		journal := beads.NewMemStore()
		legacy := beads.NewMemStore()
		router := newResidenceRoutingGraphStore(journal, legacy)

		b, err := journal.Create(beads.Bead{Title: "journal", Metadata: map[string]string{casMetaKey: "1"}})
		if err != nil {
			t.Fatalf("Create on journal leg: %v", err)
		}
		cas, _ := beads.ConditionalMetadataStoreFor(router)
		swapped, err := cas.SetMetadataIf(ctx, b.ID, casMetaKey, "1", "9")
		if err != nil {
			t.Fatalf("SetMetadataIf: %v", err)
		}
		if !swapped {
			t.Fatal("swapped = false, want true for a journal-resident bead")
		}
		got, err := journal.Get(b.ID)
		if err != nil {
			t.Fatalf("journal Get: %v", err)
		}
		if got.Metadata[casMetaKey] != "9" {
			t.Fatalf("journal leg value = %q, want 9 (the CAS routed to the journal leg)", got.Metadata[casMetaKey])
		}
	})
}

// noCASLeg is a beads.Store that does not implement ConditionalMetadataStore
// (it embeds the interface, so SetMetadataIf is not promoted). It stands in as a
// residence leg whose CAS capability is absent.
type noCASLeg struct{ beads.Store }

// TestResidenceRouterConditionalMetadataUnsupportedLegIsLoud proves the router
// hard-errors (never silently drops) when the routed leg lacks the capability.
func TestResidenceRouterConditionalMetadataUnsupportedLegIsLoud(t *testing.T) {
	ctx := context.Background()
	journal := beads.NewMemStore()
	legacy := noCASLeg{beads.NewMemStore()}
	router := newResidenceRoutingGraphStore(journal, legacy)

	b, err := legacy.Create(beads.Bead{Title: "legacy no-cas", Metadata: map[string]string{casMetaKey: "1"}})
	if err != nil {
		t.Fatalf("Create on legacy leg: %v", err)
	}
	cas, _ := beads.ConditionalMetadataStoreFor(router)
	swapped, err := cas.SetMetadataIf(ctx, b.ID, casMetaKey, "1", "2")
	if !errors.Is(err, beads.ErrConditionalMetadataUnsupported) {
		t.Fatalf("SetMetadataIf err = %v, want ErrConditionalMetadataUnsupported", err)
	}
	if swapped {
		t.Fatal("swapped = true, want false when the routed leg lacks the CAS capability")
	}
}
