package effectinventory

import "testing"

// TestInventoryValidates checks that the canonical P0.1 registry is internally
// consistent and not expired as of the pinned validation date. The registry's
// agreement with the actual execution head (every classified site resolves to a
// discovered site) is proven separately by the cmd/gc inventory test, which
// runs the analyzer; this keeps the package-level check fast and dependency
// free.
func TestInventoryValidates(t *testing.T) {
	if err := ValidateRegistry(Inventory(), validationDate()); err != nil {
		t.Fatalf("ValidateRegistry(Inventory()) = %v", err)
	}
}

// TestInventoryBoundariesAreCanonical guards that the registry classifies
// against the same boundary vocabulary the analyzer discovers with, so a site
// can never reference a boundary the analyzer would not find.
func TestInventoryBoundariesAreCanonical(t *testing.T) {
	known := make(map[string]bool)
	for _, b := range CanonicalBoundaries() {
		known[b.ID] = true
	}
	for _, site := range Inventory().Sites {
		if !known[site.BoundaryID] {
			t.Errorf("site %q references non-canonical boundary %q", site.ID, site.BoundaryID)
		}
	}
}
