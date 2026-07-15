package effectinventory

import "testing"

func TestCanonicalRegistryCombinesEveryCatalogPartitionWithoutAliasing(t *testing.T) {
	first, err := CanonicalRegistry()
	if err != nil {
		t.Fatalf("CanonicalRegistry: %v", err)
	}
	if len(first.Boundaries) == 0 || len(first.Registrations) == 0 {
		t.Fatalf("CanonicalRegistry returned boundaries=%d registrations=%d, want both non-zero", len(first.Boundaries), len(first.Registrations))
	}
	boundaryKinds := make(map[string]EffectKind, len(first.Boundaries))
	for _, boundary := range first.Boundaries {
		boundaryKinds[boundary.ID] = boundary.Kind
	}
	registrationKinds := make(map[EffectKind]int)
	for _, registration := range first.Registrations {
		registrationKinds[boundaryKinds[registration.BoundaryID]]++
	}
	for _, kind := range []EffectKind{
		KindStoreMutation,
		KindProviderMutation,
		KindProcessMutation,
		KindEventEmission,
		KindWakeSource,
	} {
		if registrationKinds[kind] == 0 {
			t.Errorf("CanonicalRegistry has no %s registrations", kind)
		}
	}

	first.Boundaries[0].ID = "mutated"
	first.Registrations[0].BoundaryID = "mutated"
	second, err := CanonicalRegistry()
	if err != nil {
		t.Fatalf("CanonicalRegistry after caller mutation: %v", err)
	}
	if second.Boundaries[0].ID == "mutated" || second.Registrations[0].BoundaryID == "mutated" {
		t.Fatal("CanonicalRegistry retained caller mutation")
	}
}
